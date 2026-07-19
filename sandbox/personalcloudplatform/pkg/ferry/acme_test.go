package ferry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
)

func testWorker(t *testing.T) *Worker {
	t.Helper()
	db := kvxtest.New(t)
	return New(&dferry.Store{DB: db}, &system.Store{DB: db}, slog.New(slog.DiscardHandler))
}

// TestACMEIssuanceThroughStub drives the worker's REAL x/crypto/acme
// order flow — account registration, HTTP-01 authorization, CSR
// finalize, chain download — against a minimal in-process RFC 8555
// directory whose validation step actually fetches the token from the
// worker's ChallengeHandler (the same handler the kernel mounts; in
// production the probe additionally rides the tunnel, which the live
// smoke covers).
func TestACMEIssuanceThroughStub(t *testing.T) {
	w := testWorker(t)
	ctx := context.Background()

	// The challenge endpoint the CA probes (kernel-mounted in prod).
	chalMux := http.NewServeMux()
	chalMux.HandleFunc("GET /.well-known/acme-challenge/{token}", w.ChallengeHandler())
	chalSrv := httptest.NewServer(chalMux)
	defer chalSrv.Close()

	ca := newACMEStub(t, chalSrv.URL)
	defer ca.srv.Close()

	gw, _, err := w.Ferry.CreateGateway(ctx, "gw", "ada")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Ferry.SetACMEDirectory(ctx, gw.ID, ca.srv.URL+"/dir"); err != nil {
		t.Fatal(err)
	}
	if err := w.Ferry.PutHost(ctx, dferry.Host{
		Hostname: "pcp.example.com", GatewayID: gw.ID, TLSMode: ferryproto.TLSModeACME,
	}); err != nil {
		t.Fatal(err)
	}
	// Add a selfsigned-mode hostname to prove the same sweep mints it.
	if err := w.Ferry.PutHost(ctx, dferry.Host{
		Hostname: "self.example.com", GatewayID: gw.ID, TLSMode: ferryproto.TLSModeSelfSigned,
	}); err != nil {
		t.Fatal(err)
	}

	w.acmeSweep(ctx)

	// The ACME cert landed with the right source and a real chain.
	cert, found, err := w.Ferry.GetCert(ctx, "pcp.example.com")
	if err != nil || !found {
		t.Fatalf("no cert stored: %v", err)
	}
	if cert.Source != ferryproto.TLSModeACME || cert.NotAfter.Before(time.Now().Add(24*time.Hour)) {
		t.Errorf("cert record wrong: source=%s notafter=%v", cert.Source, cert.NotAfter)
	}
	block, _ := pem.Decode([]byte(cert.CertPEM))
	if block == nil {
		t.Fatal("stored cert isn't PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil || leaf.DNSNames[0] != "pcp.example.com" {
		t.Fatalf("leaf wrong: %v %+v", err, leaf.DNSNames)
	}
	if !ca.sawValidation() {
		t.Error("the CA never fetched the HTTP-01 token — challenge handler untested")
	}
	// Challenge tokens are retired after issuance.
	if ka, found, _ := w.Ferry.GetChallenge(ctx, ca.lastToken()); found {
		t.Errorf("challenge token survived issuance: %q", ka)
	}
	// The account persisted (renewals reuse it).
	acct, found, _ := w.Ferry.GetACMEAccount(ctx)
	if !found || acct.DirectoryURL != ca.srv.URL+"/dir" || acct.KeyPEM == "" {
		t.Errorf("account not persisted: %+v", acct)
	}

	// The selfsigned hostname got minted in the same sweep.
	sc, found, _ := w.Ferry.GetCert(ctx, "self.example.com")
	if !found || sc.Source != ferryproto.TLSModeSelfSigned {
		t.Errorf("selfsigned mint missing: %+v", sc)
	}

	// Loop record landed with a success.
	loops, err := w.System.Loops(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := loops["ferryacme"]; !ok || rec.LastSuccess.IsZero() {
		t.Errorf("ferryacme loop record missing: %+v", loops)
	}

	// A second sweep is a no-op (certs current) — the stub sees no new
	// orders.
	before := ca.orderCount()
	w.acmeSweep(ctx)
	if ca.orderCount() != before {
		t.Error("current cert reordered on the next sweep")
	}
}

func TestChallengeHandlerUnknownToken(t *testing.T) {
	w := testWorker(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/acme-challenge/{token}", w.ChallengeHandler())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/.well-known/acme-challenge/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown token → %d, want 404", resp.StatusCode)
	}
}

// --- the RFC 8555 stub --------------------------------------------------------------

// acmeStub is the smallest directory that satisfies x/crypto/acme's
// happy path. No JWS verification — the flow under test is OUR client
// side, not the CA's checks.
type acmeStub struct {
	t       *testing.T
	srv     *httptest.Server
	chalURL string // where HTTP-01 validation probes go

	mu        sync.Mutex
	token     string
	validated bool
	orders    int
	caKey     *ecdsa.PrivateKey
	caCert    *x509.Certificate
	issued    []byte // PEM chain for the finalized order
}

func newACMEStub(t *testing.T, chalBase string) *acmeStub {
	st := &acmeStub{t: t, chalURL: chalBase}
	// A one-cert CA to sign finalized orders.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "stub CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(48 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	st.caKey = key
	st.caCert, _ = x509.ParseCertificate(der)

	mux := http.NewServeMux()
	mux.HandleFunc("/dir", st.directory)
	mux.HandleFunc("/new-nonce", st.nonce)
	mux.HandleFunc("/new-account", st.newAccount)
	mux.HandleFunc("/new-order", st.newOrder)
	mux.HandleFunc("/order/1", st.order)
	mux.HandleFunc("/authz/1", st.authz)
	mux.HandleFunc("/chal/1", st.challenge)
	mux.HandleFunc("/finalize/1", st.finalize)
	mux.HandleFunc("/cert/1", st.cert)
	st.srv = httptest.NewServer(mux)
	return st
}

func (st *acmeStub) sawValidation() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.validated
}

func (st *acmeStub) lastToken() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.token
}

func (st *acmeStub) orderCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.orders
}

// jsonReply stamps the headers every ACME response needs.
func (st *acmeStub) jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce-%d", time.Now().UnixNano()))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// jwsPayload extracts a JWS body's payload (unverified — see type doc).
func jwsPayload(r *http.Request) []byte {
	var env struct {
		Payload string `json:"payload"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &env)
	out, _ := base64.RawURLEncoding.DecodeString(env.Payload)
	return out
}

func (st *acmeStub) directory(w http.ResponseWriter, r *http.Request) {
	st.jsonReply(w, http.StatusOK, map[string]string{
		"newNonce":   st.srv.URL + "/new-nonce",
		"newAccount": st.srv.URL + "/new-account",
		"newOrder":   st.srv.URL + "/new-order",
	})
}

func (st *acmeStub) nonce(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce-%d", time.Now().UnixNano()))
	w.WriteHeader(http.StatusOK)
}

func (st *acmeStub) newAccount(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Location", st.srv.URL+"/acct/1")
	st.jsonReply(w, http.StatusCreated, map[string]string{"status": "valid"})
}

func (st *acmeStub) newOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Identifiers []struct{ Type, Value string } `json:"identifiers"`
	}
	_ = json.Unmarshal(jwsPayload(r), &req)
	if len(req.Identifiers) != 1 || req.Identifiers[0].Value != "pcp.example.com" {
		st.jsonReply(w, http.StatusBadRequest, map[string]string{"type": "urn:ietf:params:acme:error:rejectedIdentifier"})
		return
	}
	st.mu.Lock()
	st.orders++
	st.mu.Unlock()
	w.Header().Set("Location", st.srv.URL+"/order/1")
	st.jsonReply(w, http.StatusCreated, st.orderJSON())
}

func (st *acmeStub) orderJSON() map[string]any {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := map[string]any{
		"status":         "pending",
		"identifiers":    []map[string]string{{"type": "dns", "value": "pcp.example.com"}},
		"authorizations": []string{st.srv.URL + "/authz/1"},
		"finalize":       st.srv.URL + "/finalize/1",
	}
	if st.issued != nil {
		out["status"] = "valid"
		out["certificate"] = st.srv.URL + "/cert/1"
	} else if st.validated {
		out["status"] = "ready"
	}
	return out
}

func (st *acmeStub) order(w http.ResponseWriter, r *http.Request) {
	st.jsonReply(w, http.StatusOK, st.orderJSON())
}

func (st *acmeStub) authz(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	status := "pending"
	if st.validated {
		status = "valid"
	}
	st.mu.Unlock()
	st.jsonReply(w, http.StatusOK, map[string]any{
		"status":     status,
		"identifier": map[string]string{"type": "dns", "value": "pcp.example.com"},
		"challenges": []map[string]string{{
			"type": "http-01", "url": st.srv.URL + "/chal/1",
			"token": "stub-token-1", "status": status,
		}},
	})
}

// challenge is the accept endpoint: the stub validates SYNCHRONOUSLY by
// fetching the token from the worker's challenge handler, exactly as a
// CA would (minus the tunnel hop).
func (st *acmeStub) challenge(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	st.token = "stub-token-1"
	st.mu.Unlock()
	resp, err := http.Get(st.chalURL + "/.well-known/acme-challenge/stub-token-1")
	ok := false
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// keyAuth = token "." base64url(JWK thumbprint) — check shape.
		parts := strings.SplitN(strings.TrimSpace(string(body)), ".", 2)
		ok = resp.StatusCode == http.StatusOK && len(parts) == 2 &&
			parts[0] == "stub-token-1" && len(parts[1]) > 20
	}
	if !ok {
		st.jsonReply(w, http.StatusOK, map[string]string{"type": "http-01", "token": "stub-token-1", "status": "invalid"})
		return
	}
	st.mu.Lock()
	st.validated = true
	st.mu.Unlock()
	st.jsonReply(w, http.StatusOK, map[string]string{"type": "http-01", "token": "stub-token-1", "status": "valid"})
}

func (st *acmeStub) finalize(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CSR string `json:"csr"`
	}
	_ = json.Unmarshal(jwsPayload(r), &req)
	der, err := base64.RawURLEncoding.DecodeString(req.CSR)
	if err != nil {
		st.jsonReply(w, http.StatusBadRequest, map[string]string{"type": "urn:ietf:params:acme:error:badCSR"})
		return
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil || len(csr.DNSNames) != 1 || csr.DNSNames[0] != "pcp.example.com" {
		st.jsonReply(w, http.StatusBadRequest, map[string]string{"type": "urn:ietf:params:acme:error:badCSR"})
		return
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      csr.Subject,
		DNSNames:     csr.DNSNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, tmpl, st.caCert, csr.PublicKey, st.caKey)
	if err != nil {
		st.t.Errorf("stub CA sign: %v", err)
		st.jsonReply(w, http.StatusInternalServerError, map[string]string{})
		return
	}
	chain := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: st.caCert.Raw})...)
	st.mu.Lock()
	st.issued = chain
	st.mu.Unlock()
	st.jsonReply(w, http.StatusOK, st.orderJSON())
}

func (st *acmeStub) cert(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	chain := st.issued
	st.mu.Unlock()
	w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce-%d", time.Now().UnixNano()))
	w.Header().Set("Content-Type", "application/pem-certificate-chain")
	_, _ = w.Write(chain)
}
