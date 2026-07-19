// chrome.go — Chrome is the shell data every page embeds (it replaces
// PCD's PageData): user, session, site config, theme, current app for
// the switcher, quota, and the impersonation banner state. App pages
// define TYPED page structs that embed Chrome — `Data any` does not
// return.
package kernel

import (
	"net/http"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Chrome is the shared shell every template's page struct embeds.
type Chrome struct {
	Title string
	Error string // red banner
	Flash string // green banner

	// SiteName is the admin-configurable brand; Theme renders body.light
	// ("dark"|"light").
	SiteName string
	Theme    string

	// CurrentApp identifies the app for the switcher highlight
	// (launcher, drive, mail, calendar, video, music, settings, admin);
	// AppName is its display name in the app bar.
	CurrentApp string
	AppName    string

	User    users.User
	Session *users.Session // nil on pre-auth pages: no app bar
	Admin   bool

	// Anon marks an anonymous PublicOK page (Git Draft 002 §10): base.tpl
	// renders the slim sign-in bar instead of the app bar. Pre-auth pages
	// (login/signup) leave it false and render no bar at all.
	Anon bool

	// Impersonator shows the "viewing as" banner (phase 8 mints these
	// sessions; the chrome renders them from day one).
	Impersonator string

	// Per-feature master switches, mirrored from the site registry (Draft
	// 004 §4) so templates can gate cross-feature affordances (§7): a page
	// never renders a link into a disabled feature. All default off.
	DriveEnabled     bool
	MailEnabled      bool
	CalendarEnabled  bool
	ContactsEnabled  bool
	VideoEnabled     bool
	MusicEnabled     bool
	MsgEnabled       bool
	GitEnabled       bool
	SmartHomeEnabled bool
	// BuildsEnabled gates the repo Builds/Releases tabs (Draft 003 §1): a
	// Services-managed feature, not a launcher app.
	BuildsEnabled bool

	// UnreadNotifs is the launcher/notification badge count, fed by the
	// notify domain (mail delivery is its first producer).
	UnreadNotifs int

	// QuotaBytes is the member's effective quota (0 = unlimited);
	// QuotaPct/QuotaHot drive the storage meters.
	QuotaBytes int64
	QuotaPct   int
	QuotaHot   bool

	// Apps is the viewer's canonical app list (AppList) — the ONE list
	// the app switcher, the launcher grid, and any future surface
	// iterate, so the sets can never drift apart.
	Apps []AppLink
}

// AppLink is one entry in the platform's canonical app list.
type AppLink struct {
	ID   string
	Name string
	Href string
}

// AppList is the ordered platform app list for one viewer, built from the
// site feature registry (Draft 004 §8.2): a feature's card appears iff it is
// enabled, and the admin console appears for admins. Launcher itself is
// excluded — surfaces that want it (the switcher) place it explicitly. This
// is the ONE list the launcher grid and the switcher iterate, so they cannot
// drift, and adding a future feature to the registry lights it up here for
// free.
func AppList(sc site.Config, admin bool) []AppLink {
	var apps []AppLink
	for _, f := range site.Features() {
		if !f.Launcher || !sc.FeatureEnabled(f.ID) {
			continue
		}
		apps = append(apps, AppLink{ID: f.ID, Name: f.Name, Href: "/" + f.ID})
	}
	if admin {
		apps = append(apps, AppLink{ID: "admin", Name: "Admin", Href: "/admin"})
	}
	return apps
}

// appNames maps app ids to their app-bar display names.
var appNames = map[string]string{
	"launcher":  "Launcher",
	"drive":     "Drive",
	"mail":      "Email",
	"calendar":  "Calendar",
	"contacts":  "Contacts",
	"video":     "Video",
	"music":     "Music",
	"messenger": "Messenger",
	"git":       "Git",
	"smarthome": "Smart Home",
	"settings":  "Settings",
	"admin":     "Admin",
}

// Chrome builds the shell for a signed-in page. All lookups soft-fail —
// a chrome widget is never worth failing the page for.
func (a *App) Chrome(r *http.Request, title, app string, sess users.Session, user users.User) Chrome {
	ch := Chrome{
		Title:        title,
		CurrentApp:   app,
		AppName:      appNames[app],
		User:         user,
		Session:      &sess,
		Admin:        user.IsAdmin,
		Theme:        user.Prefs.Theme,
		Impersonator: sess.Impersonator,
	}
	if ch.AppName == "" {
		ch.AppName = title
	}
	cctx, cancel := ctx(r)
	defer cancel()
	sc, err := a.Site.Get(cctx)
	if err != nil {
		a.Log.Warn("site config read failed", "err", err)
	}
	ch.SiteName = sc.Name
	ch.DriveEnabled = sc.FeatureEnabled(site.FeatureDrive)
	ch.MailEnabled = sc.FeatureEnabled(site.FeatureMail)
	ch.CalendarEnabled = sc.FeatureEnabled(site.FeatureCalendar)
	ch.ContactsEnabled = sc.FeatureEnabled(site.FeatureContacts)
	ch.VideoEnabled = sc.FeatureEnabled(site.FeatureVideo)
	ch.MusicEnabled = sc.FeatureEnabled(site.FeatureMusic)
	ch.MsgEnabled = sc.FeatureEnabled(site.FeatureMessenger)
	ch.GitEnabled = sc.FeatureEnabled(site.FeatureGit)
	ch.SmartHomeEnabled = sc.FeatureEnabled(site.FeatureSmartHome)
	ch.BuildsEnabled = sc.FeatureEnabled(site.FeatureBuilds)
	ch.Apps = AppList(sc, user.IsAdmin)
	ch.QuotaBytes = site.QuotaFor(sc, user.QuotaOverride, user.Tier, a.DefaultQuota)
	if ch.QuotaBytes > 0 {
		ch.QuotaPct = int(min(100, user.UsedBytes*100/ch.QuotaBytes))
		ch.QuotaHot = ch.QuotaPct >= 90
	}
	if a.Notifs != nil {
		if n, err := a.Notifs.Unread(cctx, user.Username); err == nil {
			ch.UnreadNotifs = n
		}
	}
	return ch
}

// AuthChrome is the Chrome for the pre-auth pages (login/signup): no
// session, theme from the cookie (dark-first).
func (a *App) AuthChrome(r *http.Request, title string) Chrome {
	cctx, cancel := ctx(r)
	defer cancel()
	sc, err := a.Site.Get(cctx)
	if err != nil {
		a.Log.Warn("site config read failed", "err", err)
	}
	theme := cookieTheme(r)
	if theme == "" {
		theme = "dark"
	}
	return Chrome{Title: title, SiteName: sc.Name, Theme: theme}
}
