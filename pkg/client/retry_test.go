// retry_test.go — the retry convention's backend rotation: between
// retryable attempts the client must DROP idle connections so, behind a
// load-balanced VIP, the next attempt can land on a different backend
// instead of riding keep-alive back into the same unhealthy node.
package client

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRetryRotatesConnections(t *testing.T) {
	var conns, reqs atomic.Int64
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqs.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"ProposalTimeout: no stable leader yet, retry"}`))
			return
		}
		w.Write([]byte(`{"token":"tok"}`))
	}))
	ts.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			conns.Add(1)
		}
	}
	ts.StartTLS()
	defer ts.Close()

	c, err := New(Options{
		Endpoint:      ts.Listener.Addr().String(),
		OnUnknownCert: func(string, *x509.Certificate) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login(context.Background(), "root", "pw"); err != nil {
		t.Fatalf("login through retries failed: %v", err)
	}
	if got := reqs.Load(); got != 3 {
		t.Fatalf("want 3 attempts (2 retryable + success), got %d", got)
	}
	// One fresh dial per retryable failure — a single reused keep-alive
	// connection means every retry was pinned to the same backend.
	if got := conns.Load(); got < 3 {
		t.Errorf("retries reused a pinned connection: %d connections for %d attempts", got, reqs.Load())
	}
}
