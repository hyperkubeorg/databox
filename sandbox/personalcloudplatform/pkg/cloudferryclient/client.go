// Package cloudferryclient is PCP's side of the cloudferry planes: an
// HTTPS control client that authenticates the SERVER by the certificate
// fingerprint pinned at pairing and ITSELF by signing every request
// with the pairing's control key (client.go), plus the tunnel dialer
// that keeps the outbound stream pool alive (dialer.go).
//
// PCP always dials out; the gateway never learns PCP's address. The
// package is gateway-side plumbing: it knows nothing of databox or the
// domain layer — callers hand it the Pairing material. Mirror of
// pkg/poclient for mail.
package cloudferryclient

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
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// Pairing is the material a client needs, extracted from the domain's
// Gateway record by the caller (pkg/ferry) so this package never
// imports the domain layer.
type Pairing struct {
	ControlEndpoint string // host:port for /v1/*
	TunnelEndpoint  string // host:port the stream pool dials
	TLSFingerprint  string // sha256 hex of the gateway's leaf cert, pinned (both planes)
	ControlPriv     string // base64 ed25519 — signs every request + tunnel hello
	FerrySealPub    string // gateway's X25519 key — payloads TO it seal here
}

// Client talks to one paired gateway's control plane.
type Client struct {
	pairing Pairing
	http    *http.Client
}

// New builds a client from the pairing material. Refuses unpaired
// gateways — there is nothing to pin yet.
func New(p Pairing) (*Client, error) {
	if p.ControlEndpoint == "" || p.TLSFingerprint == "" {
		return nil, fmt.Errorf("cloudferry isn't paired")
	}
	tlsCfg, err := pinnedTLS(p.TLSFingerprint)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		TLSClientConfig: tlsCfg,
		MaxIdleConns:    2,
		IdleConnTimeout: 90 * time.Second,
	}
	return &Client{pairing: p, http: &http.Client{Transport: transport}}, nil
}

// pinnedTLS builds the fingerprint-pinned client TLS config shared by
// the control client and the tunnel dialer.
func pinnedTLS(fingerprint string) (*tls.Config, error) {
	want, err := hex.DecodeString(fingerprint)
	if err != nil || len(want) != sha256.Size {
		return nil, fmt.Errorf("bad pinned fingerprint")
	}
	return &tls.Config{
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
	}, nil
}

// Seal encrypts a payload to this gateway (config and cert pushes).
func (c *Client) Seal(plaintext []byte) ([]byte, error) {
	return wire.Seal(c.pairing.FerrySealPub, plaintext)
}

// do issues one signed request and decodes the JSON response into out
// (out may be nil).
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	url := "https://" + c.pairing.ControlEndpoint + path
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	// Sign the EXACT request target the server will verify — path AND
	// query — taken from the parsed request so the two sides can't
	// drift on escaping.
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		msg := string(raw)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("cloudferry answered %d: %s", resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Status polls GET /v1/status (the §11.3 self-report).
func (c *Client) Status(ctx context.Context) (ferryproto.StatusResponse, error) {
	var st ferryproto.StatusResponse
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &st)
	return st, err
}

// PushConfig seals and PUTs the desired state, returning the serial the
// gateway acknowledged.
func (c *Client) PushConfig(ctx context.Context, cp ferryproto.ConfigPush) (uint64, error) {
	raw, err := json.Marshal(cp)
	if err != nil {
		return 0, err
	}
	sealed, err := c.Seal(raw)
	if err != nil {
		return 0, err
	}
	var resp ferryproto.ConfigResponse
	if err := c.do(ctx, http.MethodPut, "/v1/config", sealed, &resp); err != nil {
		return 0, err
	}
	return resp.AppliedSerial, nil
}

// PushCert seals and PUTs one hostname's serving certificate (held
// RAM-only on the gateway).
func (c *Client) PushCert(ctx context.Context, cp ferryproto.CertPush) (ferryproto.CertResponse, error) {
	raw, err := json.Marshal(cp)
	if err != nil {
		return ferryproto.CertResponse{}, err
	}
	sealed, err := c.Seal(raw)
	if err != nil {
		return ferryproto.CertResponse{}, err
	}
	var resp ferryproto.CertResponse
	err = c.do(ctx, http.MethodPut, "/v1/certs", sealed, &resp)
	return resp, err
}
