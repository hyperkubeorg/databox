// Package launcher is the platform's front door: GET / renders the app
// grid (brand, greeting, five app cards, the admin card for admins, and
// the account footer), and each unbuilt app's route (/drive, /mail,
// /calendar, /video, /music, /admin) renders a shared "coming soon"
// shell — the app chrome plus an empty state — so the app switcher is
// fully navigable before the apps exist. Each shell route is replaced by
// its real app in its build phase.
package launcher

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

//go:embed *.tpl
var tplFS embed.FS

// handlers carries the parsed template set alongside the kernel.
type handlers struct {
	k     *kernel.App
	views *template.Template
}

// Card is one launcher tile.
type Card struct {
	ID     string // app id (icon + switcher highlight)
	Name   string
	Href   string
	Status string // one-line status; static until the apps land
	Badge  int    // count pill on the name (the Admin problems badge)
}

// HomePage is the launcher's typed page struct.
type HomePage struct {
	kernel.Chrome
	Greeting string
	Cards    []Card
}

// ComingSoonPage is the shared shell for apps that haven't landed.
type ComingSoonPage struct {
	kernel.Chrome
	Blurb string
}

// Mount registers the launcher's routes. Called explicitly from
// cmd/pcp. (The last coming-soon shell — /admin — was replaced by the
// real console in phase 8; the shared template survives in case a
// future app needs staging.)
func Mount(k *kernel.App) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	return kernel.Mount{App: "launcher", Routes: []kernel.Route{
		{Pattern: "GET /{$}", Handler: k.Authed(h.home)},
	}}
}

// greeting picks the salutation by the server's local time.
func greeting(now time.Time) string {
	switch h := now.Hour(); {
	case h < 5:
		return "Up late"
	case h < 12:
		return "Good morning"
	case h < 18:
		return "Good afternoon"
	default:
		return "Good evening"
	}
}

// driveStatus is the Drive card's one-liner: storage used, or the
// pitch while the account is still empty.
func driveStatus(user users.User) string {
	if user.UsedBytes <= 0 {
		return "Files, folders, and shared drives"
	}
	return ui.Bytes(user.UsedBytes) + " used"
}

// mailStatus is the Email card's one-liner: the unread-thread count
// across the member's mailboxes (soft-fail — a card line is never
// worth failing the launcher for).
func (h *handlers) mailStatus(r *http.Request, user users.User) string {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	boxes, err := h.k.Mail.UserMailboxes(cctx, user.Username)
	if err != nil || len(boxes) == 0 {
		return "Threaded conversations and labels"
	}
	unread := 0
	for _, b := range boxes {
		n, err := h.k.Mail.UnreadThreads(cctx, user.Username, b.ID, mail.FolderInbox)
		if err != nil {
			continue
		}
		unread += n
	}
	switch unread {
	case 0:
		return "Inbox zero — nothing unread"
	case 1:
		return "1 unread conversation"
	default:
		return fmt.Sprintf("%d unread conversations", unread)
	}
}

// calendarStatus is the Calendar card's one-liner: today's next event
// ("Standup in 2h"), or "Nothing today" (soft-fail like mailStatus).
func (h *handlers) calendarStatus(r *http.Request, user users.User) string {
	if h.k.Calendar == nil {
		return "Events and invites"
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	now := time.Now()
	e, ok := h.k.Calendar.NextToday(cctx, user, now)
	if !ok {
		return "Nothing today"
	}
	switch {
	case e.AllDay:
		return e.Title + " — all day"
	case !e.Start.After(now):
		return e.Title + " — now"
	default:
		d := e.Start.Sub(now)
		if d < time.Hour {
			return fmt.Sprintf("%s in %dm", e.Title, int(d.Minutes()))
		}
		return fmt.Sprintf("%s in %dh", e.Title, int(d.Round(time.Hour).Hours()))
	}
}

// mediaFolders resolves the member's registered folders once for both
// media cards (soft-fail: nil).
func (h *handlers) mediaFolders(r *http.Request, user users.User) []media.Folder {
	if h.k.Media == nil {
		return nil
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	folders, err := h.k.Media.ForUser(cctx, user.Username)
	if err != nil {
		h.k.Log.Warn("launcher media union failed", "user", user.Username, "err", err)
	}
	return folders
}

// videoStatus is the Video card's one-liner: the in-flight title
// ("Continue watching X"), else the catalog size ("N titles").
func (h *handlers) videoStatus(r *http.Request, user users.User, folders []media.Folder) string {
	if h.k.Media == nil {
		return "Movies and shows from your drives"
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	// Covered-progress only — a folder unregistered from Video must not
	// keep pitching its titles from the launcher card.
	if recent, err := h.k.Media.RecentCoveredProgress(cctx, user.Username, media.ProgVideo, 1, true); err == nil && len(recent) > 0 && recent[0].Title != "" {
		return "Continue watching " + recent[0].Title
	}
	titles := 0
	for _, f := range folders {
		if f.Kind != media.KindVideo || f.Hidden {
			continue
		}
		for _, kind := range []string{media.CatMovie, media.CatSeries} {
			if entries, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, kind); err == nil {
				titles += len(entries)
			}
		}
	}
	switch titles {
	case 0:
		return "Movies and shows from your drives"
	case 1:
		return "1 title"
	default:
		return fmt.Sprintf("%d titles", titles)
	}
}

// musicStatus is the Music card's one-liner: the last played track,
// else the album count.
func (h *handlers) musicStatus(r *http.Request, user users.User, folders []media.Folder) string {
	if h.k.Media == nil {
		return "Artists, albums, and playlists"
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if recent, err := h.k.Media.RecentCoveredProgress(cctx, user.Username, media.ProgMusic, 1, false); err == nil && len(recent) > 0 && recent[0].Title != "" {
		return "Last played: " + recent[0].Title
	}
	albums := 0
	for _, f := range folders {
		if f.Kind != media.KindMusic || f.Hidden {
			continue
		}
		if entries, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, media.CatAlbum); err == nil {
			albums += len(entries)
		}
	}
	switch albums {
	case 0:
		return "Artists, albums, and playlists"
	case 1:
		return "1 album"
	default:
		return fmt.Sprintf("%d albums", albums)
	}
}

// messengerStatus is the Messenger card's one-liner: how many servers the
// member is in (unread/mention summary lands with the message model in a
// later phase). Soft-fail like the others.
func (h *handlers) messengerStatus(r *http.Request, user users.User) string {
	if h.k.Msg == nil {
		return "Servers, channels, and direct messages"
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	servers, err := h.k.Msg.UserServers(cctx, user.Username)
	if err != nil || len(servers) == 0 {
		return "Servers, channels, and direct messages"
	}
	if len(servers) == 1 {
		return "1 server"
	}
	return fmt.Sprintf("%d servers", len(servers))
}

// gitStatus is the Git card's one-liner (Draft 002 §1): the count of
// open issues + merge requests assigned to the member. Soft-fail.
func (h *handlers) gitStatus(r *http.Request, user users.User) string {
	if h.k.Git == nil {
		return "Repositories, organizations, and code review"
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	n, err := h.k.Git.AssignedOpenCount(cctx, user.Username)
	if err != nil {
		return "Repositories, organizations, and code review"
	}
	switch n {
	case 0:
		return "Nothing assigned to you"
	case 1:
		return "1 open item assigned to you"
	default:
		return fmt.Sprintf("%d open items assigned to you", n)
	}
}

// adminProblems is the Admin card's badge: open warn/critical problems
// (soft-fail — a badge is never worth failing the launcher for).
func (h *handlers) adminProblems(r *http.Request, user users.User) int {
	if !user.IsAdmin || h.k.System == nil {
		return 0
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	n, err := h.k.System.OpenProblemCount(cctx)
	if err != nil {
		return 0
	}
	return n
}

// home renders the launcher.
func (h *handlers) home(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	folders := h.mediaFolders(r, user)
	chrome := h.k.Chrome(r, "Launcher", "launcher", sess, user)
	// The card SET is Chrome.Apps — the same canonical list the app
	// switcher iterates (kernel.AppList), so the two can never drift.
	// The launcher only decorates each entry with its live status line.
	cards := make([]Card, 0, len(chrome.Apps))
	for _, a := range chrome.Apps {
		c := Card{ID: a.ID, Name: a.Name, Href: a.Href}
		switch a.ID {
		case "drive":
			// Storage usage rides on the user record — no extra read.
			c.Status = driveStatus(user)
		case "mail":
			c.Status = h.mailStatus(r, user)
		case "calendar":
			c.Status = h.calendarStatus(r, user)
		case "contacts":
			c.Status = "People and shared address books"
		case "video":
			c.Status = h.videoStatus(r, user, folders)
		case "music":
			c.Status = h.musicStatus(r, user, folders)
		case "messenger":
			c.Status = h.messengerStatus(r, user)
		case "git":
			c.Status = h.gitStatus(r, user)
		case "admin":
			c.Status = "Site administration"
			c.Badge = h.adminProblems(r, user)
		}
		cards = append(cards, c)
	}
	pg := HomePage{
		Chrome:   chrome,
		Greeting: greeting(time.Now()),
		Cards:    cards,
	}
	ui.Render(w, h.views, "launcher", pg)
}

// comingSoon builds the placeholder handler for one unbuilt app.
func (h *handlers) comingSoon(appID, blurb string) func(http.ResponseWriter, *http.Request, users.Session, users.User) {
	return func(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
		pg := ComingSoonPage{
			Chrome: h.k.Chrome(r, "Coming soon", appID, sess, user),
			Blurb:  blurb,
		}
		ui.Render(w, h.views, "coming_soon", pg)
	}
}
