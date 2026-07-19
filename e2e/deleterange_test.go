//go:build e2e

// deleterange_test.go — TestDeleteRangeScopedAuthorization: delete-range
// must be authorized over the ENTIRE range, not just its start key (§7.2,
// §14). A token scoped to one prefix could otherwise send
// {"start": "<its prefix>", "end": ""} and wipe every key sorting after
// it — cross-tenant deletion through a single under-checked field.
package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// TestDeleteRangeScopedAuthorization — GUARANTEE: a /pcp/-scoped
// delete token cannot delete-range with an unbounded end or an end outside
// its prefix (403, nothing deleted), CAN delete ranges inside its prefix,
// and root keeps unbounded ranges.
func TestDeleteRangeScopedAuthorization(t *testing.T) {
	nodes := startCluster(t, 1)
	root := rootClient(t, nodes[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// app: allow list/read/write/delete under /pcp/ only — the
	// exact shape of the PCP example's service account.
	if err := root.Raw(ctx, http.MethodPost, "/api/v1/users",
		map[string]string{"name": "app", "password": "pw123"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := root.Raw(ctx, http.MethodPost, "/api/v1/users/app/grants", map[string]any{
		"prefix": "/pcp/", "effect": "allow",
		"verbs": []string{"list", "read", "write", "delete"},
	}, nil); err != nil {
		t.Fatal(err)
	}

	// Seed keys inside and outside the grant.
	for _, k := range []string{"/pcp/a", "/pcp/b", "/other/x", "/zzz/y"} {
		if _, err := root.Set(ctx, k, []byte("v")); err != nil {
			t.Fatal(err)
		}
	}

	app, err := client.New(client.Options{Endpoint: nodes[0].endpoint(), OnUnknownCert: acceptAll})
	if err != nil {
		t.Fatal(err)
	}
	app.Retries = 1
	if err := app.Login(ctx, "app", "pw123"); err != nil {
		t.Fatal(err)
	}

	mustExist := func(key string) {
		t.Helper()
		if _, found, err := root.Get(ctx, key); err != nil || !found {
			t.Fatalf("key %q should exist (found=%v err=%v)", key, found, err)
		}
	}
	mustBeGone := func(key string) {
		t.Helper()
		if _, found, err := root.Get(ctx, key); err != nil || found {
			t.Fatalf("key %q should be gone (found=%v err=%v)", key, found, err)
		}
	}

	// 1. Unbounded end: refused, nothing deleted.
	if err := app.DeleteRange(ctx, "/pcp/", ""); err == nil ||
		!strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("scoped delete-range with end=\"\": want Unauthorized, got %v", err)
	}
	// 2. End outside the granted prefix: refused, nothing deleted.
	if err := app.DeleteRange(ctx, "/pcp/", "/zzz"); err == nil ||
		!strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("scoped delete-range with foreign end: want Unauthorized, got %v", err)
	}
	for _, k := range []string{"/pcp/a", "/pcp/b", "/other/x", "/zzz/y"} {
		mustExist(k)
	}

	// 3. The bounded whole-prefix form (start, prefixEnd(start)) — what the
	// PCP app sends — works within the grant.
	if err := app.DeleteRange(ctx, "/pcp/", "/pcp0"); err != nil {
		t.Fatalf("scoped delete-range inside grant refused: %v", err)
	}
	mustBeGone("/pcp/a")
	mustBeGone("/pcp/b")
	mustExist("/other/x")
	mustExist("/zzz/y")

	// 4. Root keeps unbounded ranges: everything after "/" goes.
	if err := root.DeleteRange(ctx, "/", ""); err != nil {
		t.Fatalf("root unbounded delete-range refused: %v", err)
	}
	mustBeGone("/other/x")
	mustBeGone("/zzz/y")
}
