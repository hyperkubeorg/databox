package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

func testHandlers(t *testing.T) *handlers {
	t.Helper()
	db := kvxtest.New(t)
	return &handlers{k: &kernel.App{
		Users:        &users.Store{DB: db},
		Site:         &site.Store{DB: db},
		APIKeys:      &apikeys.Store{DB: db},
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		DefaultQuota: 10 << 30,
	}}
}

// The documented /api/v1/profile response shape (docs/api.md) is a
// guarantee — this test is its gate.
func TestProfileShape(t *testing.T) {
	h := testHandlers(t)
	user := users.User{Username: "ada", DisplayName: "Ada Morgan", IsAdmin: true, UsedBytes: 1 << 30}
	w := httptest.NewRecorder()
	h.profile(w, httptest.NewRequest("GET", "/api/v1/profile", nil), apikeys.Key{}, user)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{
		"username": "ada", "displayName": "Ada Morgan", "admin": true,
		"quotaBytes": float64(10 << 30), "usedBytes": float64(1 << 30),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("profile[%q] = %v, want %v", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("profile has %d fields, want %d: %v", len(got), len(want), got)
	}
}

// The documented /api/v1/scopes response shape (docs/api.md).
func TestScopesShape(t *testing.T) {
	h := testHandlers(t)
	key := apikeys.Key{KeyID: "k123", Name: "phone", Scopes: []string{"profile:read", "drive:read"}}

	w := httptest.NewRecorder()
	h.scopes(w, httptest.NewRequest("GET", "/api/v1/scopes", nil), key, users.User{})
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["keyId"] != "k123" || got["name"] != "phone" {
		t.Errorf("scopes body = %v", got)
	}
	if _, present := got["expiresAt"]; present {
		t.Error("expiresAt must be absent for never-expiring keys")
	}

	key.ExpiresAt = time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	w = httptest.NewRecorder()
	h.scopes(w, httptest.NewRequest("GET", "/api/v1/scopes", nil), key, users.User{})
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["expiresAt"] != "2027-01-02T03:04:05Z" {
		t.Errorf("expiresAt = %v, want RFC 3339", got["expiresAt"])
	}
}

// Unmatched paths inside /api/v1/ answer the JSON envelope, never HTML.
func TestNotFoundEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	notFound(w, httptest.NewRequest("GET", "/api/v1/no-such-thing", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var env map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env["code"] != "not_found" || env["error"] == "" {
		t.Errorf("envelope = %v", env)
	}
}

func TestPageParams(t *testing.T) {
	cases := []struct {
		url    string
		cursor string
		limit  int
	}{
		{"/x", "", defaultLimit},
		{"/x?cursor=abc&limit=10", "abc", 10},
		{"/x?limit=100000", "", maxLimit},
		{"/x?limit=junk", "", defaultLimit},
		{"/x?limit=-3", "", defaultLimit},
	}
	for _, tc := range cases {
		cursor, limit := pageParams(httptest.NewRequest("GET", tc.url, nil))
		if cursor != tc.cursor || limit != tc.limit {
			t.Errorf("pageParams(%q) = (%q, %d), want (%q, %d)", tc.url, cursor, limit, tc.cursor, tc.limit)
		}
	}
}

// decodeJSON answers the envelope itself on junk and enforces the body cap.
func TestDecodeJSON(t *testing.T) {
	var v struct {
		Name string `json:"name"`
	}
	r := httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"ok"}`))
	if err := decodeJSON(httptest.NewRecorder(), r, &v); err != nil || v.Name != "ok" {
		t.Fatalf("decode valid: %v (%+v)", err, v)
	}
	w := httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":`))
	if err := decodeJSON(w, r, &v); err == nil {
		t.Fatal("junk body must error")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("junk body status = %d", w.Code)
	}
}
