// Package poclient is PCP's side of the postoffice control plane: an
// HTTPS client that authenticates the SERVER by the certificate
// fingerprint pinned at pairing (stronger than CA validation — only
// the exact key from the handshake is accepted) and authenticates
// ITSELF by signing every request with the pairing's control key.
//
// PCP always dials out; the gateway never learns PCP's address. The
// package is gateway-side plumbing: it knows nothing of databox or the
// domain layer — callers hand it the Pairing material.
package poclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// Pairing is the material a client needs, extracted from the domain's
// PostOffice record by the caller (mailer) so this package never
// imports the domain layer.
type Pairing struct {
	Endpoint       string // host:port
	TLSFingerprint string // sha256 hex of the gateway's leaf cert, pinned
	ControlPriv    string // base64 ed25519 — signs every request
	POSealPub      string // gateway's X25519 key — payloads TO it seal here
}

// Client talks to one paired gateway.
type Client struct {
	pairing Pairing
	http    *http.Client
}

// New builds a client from the pairing material. Refuses unpaired
// gateways — there is nothing to pin yet.
func New(p Pairing) (*Client, error) {
	if p.Endpoint == "" || p.TLSFingerprint == "" {
		return nil, fmt.Errorf("post office isn't paired")
	}
	want, err := hex.DecodeString(p.TLSFingerprint)
	if err != nil || len(want) != sha256.Size {
		return nil, fmt.Errorf("bad pinned fingerprint")
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			// The pin IS the trust decision: chain and name checks are
			// meaningless against a self-signed cert we exchanged by hand.
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				if len(rawCerts) == 0 {
					return fmt.Errorf("no certificate presented")
				}
				sum := sha256.Sum256(rawCerts[0])
				if !bytes.Equal(sum[:], want) {
					return fmt.Errorf("certificate does not match the pinned fingerprint")
				}
				return nil
			},
		},
		MaxIdleConns:    2,
		IdleConnTimeout: 90 * time.Second,
	}
	return &Client{pairing: p, http: &http.Client{Transport: transport}}, nil
}

// Seal encrypts a payload to this gateway (config pushes, outbound
// submissions).
func (c *Client) Seal(plaintext []byte) ([]byte, error) {
	return wire.Seal(c.pairing.POSealPub, plaintext)
}

// do issues one signed request and decodes the JSON response into out
// (out may be nil).
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	url := "https://" + c.pairing.Endpoint + path
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	// Sign the EXACT request target the server will verify — path AND
	// query — taken from the parsed request so the two sides can't drift
	// on escaping. (The server verifies r.URL.RequestURI().)
	auth, err := wire.SignRequest(c.pairing.ControlPriv, method, req.URL.RequestURI(), body)
	if err != nil {
		return err
	}
	req.Header.Set(wire.AuthHeader, auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		msg := string(raw)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("postoffice answered %d: %s", resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Status polls GET /v1/status (the §11.3 self-report).
func (c *Client) Status(ctx context.Context) (mailproto.StatusResponse, error) {
	var st mailproto.StatusResponse
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &st)
	return st, err
}

// FetchInbound long-polls the gateway's spool; wait=0 returns
// immediately.
func (c *Client) FetchInbound(ctx context.Context, wait time.Duration) (mailproto.InboundResponse, error) {
	path := "/v1/inbound"
	if wait > 0 {
		path += "?wait=" + wait.String()
	}
	var resp mailproto.InboundResponse
	err := c.do(ctx, http.MethodGet, path, nil, &resp)
	return resp, err
}

// AckInbound deletes delivered spool entries.
func (c *Client) AckInbound(ctx context.Context, spoolIDs []string) error {
	raw, err := json.Marshal(mailproto.AckRequest{SpoolIDs: spoolIDs})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/v1/inbound/ack", raw, nil)
}

// PushConfig seals and PUTs the desired state, returning the serial the
// gateway acknowledged.
func (c *Client) PushConfig(ctx context.Context, cp mailproto.ConfigPush) (uint64, error) {
	raw, err := json.Marshal(cp)
	if err != nil {
		return 0, err
	}
	sealed, err := c.Seal(raw)
	if err != nil {
		return 0, err
	}
	var resp mailproto.ConfigResponse
	if err := c.do(ctx, http.MethodPut, "/v1/config", sealed, &resp); err != nil {
		return 0, err
	}
	return resp.AppliedSerial, nil
}

// SubmitOutbound seals and POSTs a batch of outbound messages.
func (c *Client) SubmitOutbound(ctx context.Context, subs []mailproto.OutboundSubmission) ([]string, error) {
	raw, err := json.Marshal(mailproto.OutboundBatch{Messages: subs})
	if err != nil {
		return nil, err
	}
	sealed, err := c.Seal(raw)
	if err != nil {
		return nil, err
	}
	var resp mailproto.OutboundResponse
	if err := c.do(ctx, http.MethodPost, "/v1/outbound", sealed, &resp); err != nil {
		return nil, err
	}
	return resp.Accepted, nil
}

// FetchEvents polls delivery outcomes past cursor.
func (c *Client) FetchEvents(ctx context.Context, cursor uint64) (mailproto.EventsResponse, error) {
	var resp mailproto.EventsResponse
	err := c.do(ctx, http.MethodGet, "/v1/events?cursor="+strconv.FormatUint(cursor, 10), nil, &resp)
	return resp, err
}
