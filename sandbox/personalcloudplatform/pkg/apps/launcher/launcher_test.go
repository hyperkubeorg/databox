package launcher

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// fixtureChrome is a signed-in shell for render tests. Apps comes from
// kernel.AppList exactly like the real Chrome builder.
func fixtureChrome(title, app string, admin bool) kernel.Chrome {
	return kernel.Chrome{
		Title:      title,
		SiteName:   "Test Cloud",
		Theme:      "dark",
		CurrentApp: app,
		AppName:    "Launcher",
		User:       users.User{Username: "ada", DisplayName: "Ada Morgan", IsAdmin: admin},
		Session:    &users.Session{Username: "ada", CSRF: "tok"},
		Admin:      admin,
		Apps:       kernel.AppList(site.Config{}, admin),
	}
}

func TestLauncherRenders(t *testing.T) {
	views := ui.MustParse(tplFS)
	pg := HomePage{
		Chrome:   fixtureChrome("Launcher", "launcher", true),
		Greeting: "Good evening",
		Cards: []Card{
			{ID: "drive", Name: "Drive", Href: "/drive", Status: "Files"},
			{ID: "mail", Name: "Email", Href: "/mail", Status: "Webmail"},
		},
	}
	var buf bytes.Buffer
	if err := views.ExecuteTemplate(&buf, "launcher", pg); err != nil {
		t.Fatalf("render launcher: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Good evening, Ada Morgan",      // greeting
		`href="/drive"`, `href="/mail"`, // cards
		`href="/admin"`,    // admin card (admin fixture)
		"@ada",             // account footer
		`id="appMenu"`,     // app switcher popover
		`meta name="csrf"`, // fetch mutations get their token
	} {
		if !strings.Contains(out, want) {
			t.Errorf("launcher missing %q", want)
		}
	}

	// Non-admins must not see the Admin card.
	pg.Chrome = fixtureChrome("Launcher", "launcher", false)
	buf.Reset()
	if err := views.ExecuteTemplate(&buf, "launcher", pg); err != nil {
		t.Fatalf("render launcher (member): %v", err)
	}
	if strings.Contains(buf.String(), `href="/admin"`) {
		t.Error("launcher shows the Admin card to a non-admin")
	}
}

// TestMailSwitcherGating: with the mail feature off, the app switcher must
// not offer Email. Rendered without a mail card so any /mail link can only
// come from the switcher.
func TestMailSwitcherGating(t *testing.T) {
	views := ui.MustParse(tplFS)
	render := func(mailOn bool) string {
		ch := fixtureChrome("Launcher", "launcher", false)
		ch.MailEnabled = mailOn
		ch.Apps = kernel.AppList(site.Config{Mail: site.MailConfig{Enabled: mailOn}}, false)
		var buf bytes.Buffer
		if err := views.ExecuteTemplate(&buf, "launcher", HomePage{
			Chrome: ch, Greeting: "Hi",
			Cards: []Card{{ID: "drive", Name: "Drive", Href: "/drive", Status: "Files"}},
		}); err != nil {
			t.Fatalf("render: %v", err)
		}
		return buf.String()
	}
	if strings.Contains(render(false), `href="/mail"`) {
		t.Error("app switcher offers Email when the mail feature is off")
	}
	if !strings.Contains(render(true), `href="/mail"`) {
		t.Error("app switcher hides Email when the mail feature is on")
	}
}

func TestComingSoonRenders(t *testing.T) {
	views := ui.MustParse(tplFS)
	pg := ComingSoonPage{Chrome: fixtureChrome("Coming soon", "video", false), Blurb: "Soon."}
	pg.AppName = "Video"
	var buf bytes.Buffer
	if err := views.ExecuteTemplate(&buf, "coming_soon", pg); err != nil {
		t.Fatalf("render coming_soon: %v", err)
	}
	for _, want := range []string{"Video isn't here yet", "Soon.", "Back to the Launcher"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("coming_soon missing %q", want)
		}
	}
}

func TestGreetingBuckets(t *testing.T) {
	cases := map[int]string{3: "Up late", 9: "Good morning", 14: "Good afternoon", 21: "Good evening"}
	for hour, want := range cases {
		at := time.Date(2026, 7, 4, hour, 0, 0, 0, time.Local)
		if got := greeting(at); got != want {
			t.Errorf("greeting(%02d:00) = %q, want %q", hour, got, want)
		}
	}
}
