package kernel

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// The kernel's auth pages must parse with the base shell and render with
// fixture Chrome data.
func TestAuthPagesRender(t *testing.T) {
	views := ui.MustParse(tplFS)

	login := LoginPage{
		Chrome: Chrome{Title: "sign in", SiteName: "Test Cloud", Theme: "dark"},
		Next:   "/settings",
	}
	var buf bytes.Buffer
	if err := views.ExecuteTemplate(&buf, "login", login); err != nil {
		t.Fatalf("render login: %v", err)
	}
	for _, want := range []string{"Test Cloud", `name="next" value="/settings"`, "Welcome back"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("login page missing %q", want)
		}
	}

	for _, tc := range []struct {
		needInvite bool
		want       string
	}{
		{false, "Create your account"},
		{true, "Invite-only"},
		{true, `name="invite"`},
	} {
		signup := SignupPage{
			Chrome:     Chrome{Title: "sign up", SiteName: "Test Cloud", Theme: "light"},
			NeedInvite: tc.needInvite,
			InviteCode: "abc123def456",
		}
		buf.Reset()
		if err := views.ExecuteTemplate(&buf, "signup", signup); err != nil {
			t.Fatalf("render signup (invite=%v): %v", tc.needInvite, err)
		}
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("signup page (invite=%v) missing %q", tc.needInvite, tc.want)
		}
	}
}
