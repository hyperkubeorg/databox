//go:build e2e

// logout_test.go — TestLogoutRevokesToken: §7.1 "sessions are revocable".
// Logout must invalidate the presented bearer token server-side, not just
// client-side — the very next request with that token is 401.
package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestLogoutRevokesToken — GUARANTEE: POST /api/v1/auth/logout revokes the
// session token; subsequent requests with it are rejected as Unauthorized.
func TestLogoutRevokesToken(t *testing.T) {
	nodes := startCluster(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c := rootClient(t, nodes[0].port)
	c.Retries = 1 // a 401 must surface, not be retried into a timeout

	// The token works before logout.
	if err := c.Raw(ctx, http.MethodGet, "/api/v1/cluster/status", nil, nil); err != nil {
		t.Fatalf("authenticated request before logout: %v", err)
	}

	if err := c.Raw(ctx, http.MethodPost, "/api/v1/auth/logout", nil, nil); err != nil {
		t.Fatalf("logout: %v", err)
	}

	// The same client still holds the revoked token: every request must now
	// answer 401 Unauthorized.
	err := c.Raw(ctx, http.MethodGet, "/api/v1/cluster/status", nil, nil)
	if err == nil {
		t.Fatal("request with a revoked token succeeded")
	}
	if !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("revoked token: want Unauthorized, got %v", err)
	}
	// KV path too — revocation is global, not per-endpoint.
	if _, _, err := c.Get(ctx, "/logout/probe"); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("revoked token on KV path: want Unauthorized, got %v", err)
	}

	// A fresh login works — revocation killed the session, not the user.
	c2 := rootClient(t, nodes[0].port)
	if err := c2.Raw(ctx, http.MethodGet, "/api/v1/cluster/status", nil, nil); err != nil {
		t.Fatalf("fresh session after logout: %v", err)
	}
}
