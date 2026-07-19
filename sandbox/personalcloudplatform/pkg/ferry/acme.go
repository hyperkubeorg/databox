// acme.go — certificate issuance, PCP-side (spec §10.2: the PCP runs
// the ACME client; account and cert private keys never leave PCP's
// databox; the gateway holds pushed copies in RAM only).
//
//   - acme mode: x/crypto/acme against the gateway's directory URL
//     (Let's Encrypt by default). HTTP-01 challenges arrive at the
//     GATEWAY on port 80 and tunnel down to the kernel-mounted handler
//     (ChallengeHandler) like any request — issuance and renewal need
//     nothing on the gateway. Tokens live in databox so any replica can
//     answer.
//   - selfsigned mode: mint a 1-year cert and rotate inside the same
//     renewal window.
//   - custom mode: operator-uploaded PEM; this loop never touches it.
//
// Issued certs land at /pcp/cloudferry/certs/<hostname>; the sync loop
// notices the NotAfter change and pushes.
package ferry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/crypto/acme"

	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
)

// LetsEncryptDirectory is the production CA (a gateway's
// ACMEDirectoryURL overrides it — staging, pebble, tests).
const LetsEncryptDirectory = "https://acme-v02.api.letsencrypt.org/directory"

// issuanceTimeout bounds one order's lifetime.
const issuanceTimeout = 3 * time.Minute

// RunACME loops until ctx dies.
func (w *Worker) RunACME(ctx context.Context) {
	t := time.NewTicker(acmeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		w.acmeSweep(ctx)
	}
}

// acmeSweep issues/renews every hostname that needs it (lock-gated
// singleton like the sync loop).
func (w *Worker) acmeSweep(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if _, err := w.Ferry.DB.LockAcquire(sctx, acmeLock, "exclusive", 11*time.Minute); err != nil {
		return
	}
	defer func() { _ = w.Ferry.DB.LockRelease(context.Background(), acmeLock) }()

	hosts, err := w.Ferry.ListHosts(sctx)
	if err != nil {
		w.Log.Warn("ferryacme: list hosts failed", "err", err)
		w.record(sctx, "ferryacme", err)
		return
	}
	var sweepErr error
	now := time.Now()
	for _, h := range hosts {
		cert, found, err := w.Ferry.GetCert(sctx, h.Hostname)
		if err != nil {
			sweepErr = err
			continue
		}
		if found && cert.Source == h.TLSMode && !cert.NeedsRenewal(now) {
			continue // current
		}
		switch h.TLSMode {
		case ferryproto.TLSModeSelfSigned:
			minted, err := dferry.MintSelfSigned(h.Hostname)
			if err == nil {
				err = w.Ferry.SetCert(sctx, minted)
			}
			if err != nil {
				w.Log.Warn("ferryacme: selfsigned mint failed", "hostname", h.Hostname, "err", err)
				sweepErr = err
				continue
			}
			w.Log.Info("ferryacme: selfsigned cert minted", "hostname", h.Hostname, "not_after", minted.NotAfter)
			w.Kick() // the sync loop pushes it
		case ferryproto.TLSModeACME:
			if err := w.issueACME(sctx, h); err != nil {
				w.Log.Warn("ferryacme: issuance failed", "hostname", h.Hostname, "err", err)
				sweepErr = err
				continue
			}
			w.Kick()
		case ferryproto.TLSModeCustom:
			// Operator-owned; expiry surfaces on the admin page (and as
			// a problem check in phase 8), never auto-replaced.
		}
	}
	w.record(sctx, "ferryacme", sweepErr)
}

// issueACME runs one HTTP-01 order for h against its gateway's
// directory.
func (w *Worker) issueACME(ctx context.Context, h dferry.Host) error {
	ictx, cancel := context.WithTimeout(ctx, issuanceTimeout)
	defer cancel()

	gw, found, err := w.Ferry.GetGateway(ictx, h.GatewayID)
	if err != nil || !found {
		return fmt.Errorf("gateway for %s: %v", h.Hostname, err)
	}
	dir := gw.ACMEDirectoryURL
	if dir == "" {
		dir = LetsEncryptDirectory
	}
	cl, err := w.acmeClient(ictx, dir)
	if err != nil {
		return err
	}

	order, err := cl.AuthorizeOrder(ictx, acme.DomainIDs(h.Hostname))
	if err != nil {
		return fmt.Errorf("new order: %w", err)
	}
	var tokens []string
	defer func() {
		for _, tok := range tokens {
			w.Ferry.DeleteChallenge(context.Background(), tok)
		}
	}()
	for _, zurl := range order.AuthzURLs {
		z, err := cl.GetAuthorization(ictx, zurl)
		if err != nil {
			return fmt.Errorf("authorization: %w", err)
		}
		if z.Status == acme.StatusValid {
			continue
		}
		var chal *acme.Challenge
		for _, c := range z.Challenges {
			if c.Type == "http-01" {
				chal = c
				break
			}
		}
		if chal == nil {
			return fmt.Errorf("CA offered no http-01 challenge for %s", h.Hostname)
		}
		keyAuth, err := cl.HTTP01ChallengeResponse(chal.Token)
		if err != nil {
			return err
		}
		// Publish the token BEFORE accepting: the CA's validation
		// request arrives through the tunnel at any replica.
		if err := w.Ferry.SetChallenge(ictx, chal.Token, keyAuth); err != nil {
			return fmt.Errorf("publish challenge: %w", err)
		}
		tokens = append(tokens, chal.Token)
		if _, err := cl.Accept(ictx, chal); err != nil {
			return fmt.Errorf("accept challenge: %w", err)
		}
		if _, err := cl.WaitAuthorization(ictx, z.URI); err != nil {
			return fmt.Errorf("authorization never validated: %w", err)
		}
	}

	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: h.Hostname},
		DNSNames: []string{h.Hostname},
	}, certKey)
	if err != nil {
		return err
	}
	ders, _, err := cl.CreateOrderCert(ictx, order.FinalizeURL, csr, true)
	if err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	var chain []byte
	for _, der := range ders {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalECPrivateKey(certKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := w.Ferry.SetCert(ictx, dferry.HostCert{
		Hostname: h.Hostname, CertPEM: string(chain), KeyPEM: string(keyPEM),
		Source: ferryproto.TLSModeACME,
	}); err != nil {
		return err
	}
	w.Log.Info("ferryacme: certificate issued", "hostname", h.Hostname, "directory", dir)
	return nil
}

// acmeClient loads (or registers) the CA account for dir and returns a
// ready client. One account key per directory: switching directories
// registers fresh.
func (w *Worker) acmeClient(ctx context.Context, dir string) (*acme.Client, error) {
	acct, found, err := w.Ferry.GetACMEAccount(ctx)
	if err != nil {
		return nil, err
	}
	if !found || acct.DirectoryURL != dir || acct.KeyPEM == "" {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return nil, err
		}
		acct = dferry.ACMEAccount{
			DirectoryURL: dir,
			KeyPEM:       string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})),
		}
	}
	block, _ := pem.Decode([]byte(acct.KeyPEM))
	if block == nil {
		return nil, fmt.Errorf("stored ACME account key doesn't parse")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	cl := &acme.Client{Key: key, DirectoryURL: dir, UserAgent: "pcp-cloudferry/" + "0.1"}
	if acct.AccountURL == "" {
		a, err := cl.Register(ctx, &acme.Account{}, acme.AcceptTOS)
		switch {
		case err == nil:
			acct.AccountURL = a.URI
		case errors.Is(err, acme.ErrAccountAlreadyExists):
			// key already registered; the client resolves the URL lazily
		default:
			return nil, fmt.Errorf("register account: %w", err)
		}
		if err := w.Ferry.SetACMEAccount(ctx, acct); err != nil {
			return nil, err
		}
	}
	return cl, nil
}

// ChallengeHandler serves /.well-known/acme-challenge/{token} — mounted
// UNAUTHENTICATED on the kernel router; the CA's probe tunnels through
// the gateway to any replica.
func (w *Worker) ChallengeHandler() http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		keyAuth, found, err := w.Ferry.GetChallenge(ctx, token)
		if err != nil || !found {
			http.NotFound(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(rw, keyAuth)
	}
}
