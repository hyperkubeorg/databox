package settings

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

func TestSettingsRenders(t *testing.T) {
	views := ui.MustParse(tplFS)
	pg := Page{Chrome: kernel.Chrome{
		Title:      "Settings",
		SiteName:   "Test Cloud",
		Theme:      "light",
		CurrentApp: "settings",
		AppName:    "Settings",
		User: users.User{Username: "ada", DisplayName: "Ada Morgan",
			Prefs: users.Prefs{Theme: "light"}, UsedBytes: 1 << 30},
		Session:    &users.Session{Username: "ada", CSRF: "tok"},
		QuotaBytes: 10 << 30,
		QuotaPct:   10,
	}}
	var buf bytes.Buffer
	if err := views.ExecuteTemplate(&buf, "settings", pg); err != nil {
		t.Fatalf("render settings: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"@ada", "Ada Morgan",
		`action="/settings/profile"`, `action="/settings/password"`, `action="/settings/theme"`,
		`value="tok"`,              // CSRF rides every form
		"1.0 GiB",                  // storage line
		`class="light"`,            // theme pre-render
		`href="/settings/apikeys"`, // the API keys page link
	} {
		if !strings.Contains(out, want) {
			t.Errorf("settings missing %q", want)
		}
	}
}

func TestAPIKeysPageRenders(t *testing.T) {
	views := ui.MustParse(tplFS)
	chrome := kernel.Chrome{
		Title: "API keys", SiteName: "Test Cloud", Theme: "dark",
		CurrentApp: "settings", AppName: "Settings",
		User:    users.User{Username: "ada", DisplayName: "Ada"},
		Session: &users.Session{Username: "ada", CSRF: "tok"},
	}
	pg := KeysPage{
		Chrome: chrome,
		Scopes: apikeys.Scopes,
		Keys: []apikeys.Key{{
			KeyID: "k1234567890ab", Name: "phone mail",
			Scopes:    []string{"mail:read", "mail:send"},
			CreatedAt: time.Now().Add(-48 * time.Hour),
			ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
		}},
	}
	var buf bytes.Buffer
	if err := views.ExecuteTemplate(&buf, "apikeys", pg); err != nil {
		t.Fatalf("render apikeys: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"phone mail", "pcp_k1234567890ab_…",
		`<span class="chip">mail:read</span>`, `<span class="chip">mail:send</span>`,
		"2d ago", "never</span>", // created reltime; LastUsed zero renders "never"
		`action="/settings/apikeys/create"`, `action="/settings/apikeys/revoke"`,
		`value="tok"`,
		"Send mail as you", // every scope checkbox carries its description
	} {
		if !strings.Contains(out, want) {
			t.Errorf("apikeys page missing %q", want)
		}
	}
	if strings.Contains(out, "never be shown again") {
		t.Error("the reveal panel must not render without NewToken")
	}

	// The create response: the token renders exactly once, with the
	// one-time notice.
	pg.NewToken, pg.NewName = "pcp_k1234567890ab_secretsecretsecret", "phone mail"
	buf.Reset()
	if err := views.ExecuteTemplate(&buf, "apikeys", pg); err != nil {
		t.Fatalf("render apikeys with reveal: %v", err)
	}
	out = buf.String()
	for _, want := range []string{
		"pcp_k1234567890ab_secretsecretsecret", "never be shown again", `id="copyToken"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reveal panel missing %q", want)
		}
	}
}
