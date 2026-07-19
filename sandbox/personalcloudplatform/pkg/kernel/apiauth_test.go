package kernel

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// newAPITestApp wires an App against the kvxtest fake databox with one
// signed-up member and one minted key.
func newAPITestApp(t *testing.T, scopes []string) (*App, string, apikeys.Key) {
	t.Helper()
	db := kvxtest.New(t)
	a := &App{
		Users:   &users.Store{DB: db},
		Site:    &site.Store{DB: db},
		APIKeys: &apikeys.Store{DB: db},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx := context.Background()
	if _, err := a.Users.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, key, err := a.APIKeys.Mint(ctx, "ada", "test key", scopes, time.Time{})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return a, token, key
}

// callAPI runs one request through APIAuthed and returns the recorder
// plus whether the wrapped handler ran (and for whom).
func callAPI(a *App, scope, authz string) (*httptest.ResponseRecorder, string) {
	served := ""
	h := a.APIAuthed(scope, func(w http.ResponseWriter, _ *http.Request, k apikeys.Key, u users.User) {
		served = u.Username
		JSON(w, http.StatusOK, map[string]string{"user": u.Username, "key": k.KeyID})
	})
	r := httptest.NewRequest("GET", "/api/v1/probe", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w, served
}

// envelope decodes the {code, error} error body.
func envelope(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q is not the JSON envelope: %v", w.Body.String(), err)
	}
	return out
}

func TestAPIAuthedValidKey(t *testing.T) {
	a, token, key := newAPITestApp(t, []string{apikeys.ScopeProfileRead})
	w, served := callAPI(a, apikeys.ScopeProfileRead, "Bearer "+token)
	if w.Code != http.StatusOK || served != "ada" {
		t.Fatalf("valid key: status=%d served=%q body=%s", w.Code, served, w.Body)
	}
	// The scheme is case-insensitive; ScopeAny admits any valid key.
	if w, _ := callAPI(a, ScopeAny, "bearer "+token); w.Code != http.StatusOK {
		t.Errorf("lowercase scheme / ScopeAny: status=%d", w.Code)
	}
	_ = key
}

func TestAPIAuthedWrongScope(t *testing.T) {
	a, token, _ := newAPITestApp(t, []string{apikeys.ScopeProfileRead})
	w, served := callAPI(a, apikeys.ScopeDriveWrite, "Bearer "+token)
	if w.Code != http.StatusForbidden || served != "" {
		t.Fatalf("wrong scope: status=%d served=%q", w.Code, served)
	}
	env := envelope(t, w)
	if env["code"] != "forbidden" || env["error"] != "missing scope drive:write" {
		t.Errorf("envelope = %v", env)
	}
}

func TestAPIAuthedBadToken(t *testing.T) {
	a, token, _ := newAPITestApp(t, []string{apikeys.ScopeProfileRead})
	for name, authz := range map[string]string{
		"missing header": "",
		"wrong scheme":   "Basic dXNlcjpwYXNz",
		"junk token":     "Bearer pcp_not_a_real_token",
		"tampered":       "Bearer " + token[:len(token)-2] + "zz",
	} {
		w, served := callAPI(a, apikeys.ScopeProfileRead, authz)
		if w.Code != http.StatusUnauthorized || served != "" {
			t.Errorf("%s: status=%d served=%q", name, w.Code, served)
			continue
		}
		if got := w.Header().Get("WWW-Authenticate"); got == "" {
			t.Errorf("%s: 401 must carry WWW-Authenticate: Bearer", name)
		}
		if env := envelope(t, w); env["code"] != "unauthorized" {
			t.Errorf("%s: envelope = %v", name, env)
		}
	}
}

func TestAPIAuthedExpiredKey(t *testing.T) {
	a, token, key := newAPITestApp(t, []string{apikeys.ScopeProfileRead})
	// Expire the stored record underneath the minted token.
	ctx := context.Background()
	var stored apikeys.Key
	if found, err := kvx.GetJSON(ctx, a.Users.DB, "/pcp/apikeys/"+key.KeyID, &stored); err != nil || !found {
		t.Fatalf("load key: found=%v err=%v", found, err)
	}
	stored.ExpiresAt = time.Now().Add(-time.Hour)
	if err := kvx.SetJSON(ctx, a.Users.DB, "/pcp/apikeys/"+key.KeyID, stored); err != nil {
		t.Fatalf("expire key: %v", err)
	}
	w, served := callAPI(a, apikeys.ScopeProfileRead, "Bearer "+token)
	if w.Code != http.StatusUnauthorized || served != "" {
		t.Fatalf("expired key: status=%d served=%q", w.Code, served)
	}
}

func TestAPIAuthedBannedOwner(t *testing.T) {
	a, token, _ := newAPITestApp(t, []string{apikeys.ScopeProfileRead})
	ctx := context.Background()
	var u users.User
	if found, err := kvx.GetJSON(ctx, a.Users.DB, "/pcp/users/ada", &u); err != nil || !found {
		t.Fatalf("load user: found=%v err=%v", found, err)
	}
	u.Banned = true
	if err := kvx.SetJSON(ctx, a.Users.DB, "/pcp/users/ada", u); err != nil {
		t.Fatalf("ban user: %v", err)
	}
	w, served := callAPI(a, apikeys.ScopeProfileRead, "Bearer "+token)
	if w.Code != http.StatusUnauthorized || served != "" {
		t.Fatalf("banned owner: status=%d served=%q", w.Code, served)
	}
}

func TestAPIAuthedRateLimit(t *testing.T) {
	a, token, _ := newAPITestApp(t, []string{apikeys.ScopeProfileRead})
	a.limAPIKey = newRateLimiterBurst(apiKeyPerMinute, 1) // one-token bucket
	if w, _ := callAPI(a, apikeys.ScopeProfileRead, "Bearer "+token); w.Code != http.StatusOK {
		t.Fatalf("first request: status=%d", w.Code)
	}
	w, served := callAPI(a, apikeys.ScopeProfileRead, "Bearer "+token)
	if w.Code != http.StatusTooManyRequests || served != "" {
		t.Fatalf("second request: status=%d served=%q", w.Code, served)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
	if env := envelope(t, w); env["code"] != "rate_limited" {
		t.Errorf("envelope = %v", env)
	}
}
