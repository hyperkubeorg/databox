// page.go — the SSR app page. GET /mail renders the full three-pane
// state for ?box=&folder=&label=&q=&filter=&thread= — rows and the open
// thread are real server-rendered markup (readable, linkable, no-JS
// navigable), then mail.js re-renders both panes from the same JSON
// feeds for the live interaction model.
package mail

import (
	"net/http"

	dmail "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// Page is /mail's typed page struct.
type Page struct {
	kernel.Chrome

	Boxes []dmail.Mailbox
	Box   dmail.Mailbox

	Folders []FolderVM
	Labels  []LabelVM

	// List state.
	View       viewSpec
	Title      string
	Rows       []RowVM
	NextCursor string
	Unread     int // unread rows in the current listing (the pill)

	// Reading state ("" = empty state).
	Thread *ThreadVM

	// UndoMs is the member's undo-send window for the send toast.
	UndoMs int64

	// CanCreate adds the sidebar's "+ New address" option (allowance
	// slot free) linking to the chooser.
	CanCreate bool
}

// viewTitle names the list pane.
func viewTitle(v viewSpec, folders []FolderVM, labels []LabelVM) string {
	if v.Query != "" {
		return "Search"
	}
	if v.View == "label" {
		for _, l := range labels {
			if l.ID == v.Label {
				return l.Name
			}
		}
		return "Label"
	}
	for _, f := range folders {
		if f.ID == v.View {
			return f.Name
		}
	}
	return "Inbox"
}

// page renders /mail — or the account chooser when nothing is picked:
// zero mailboxes (create or the "nothing granted" notice), several
// mailboxes with no ?box=, or the explicit ?box=new ("new" can never
// be a real id — kvx ids are ≥8 chars). Exactly one mailbox goes
// straight in, as ever.
func (h *handlers) page(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	boxes := h.boxes(r, user)
	want := r.URL.Query().Get("box")
	if want == "new" || len(boxes) == 0 || (want == "" && len(boxes) > 1) {
		h.accountsPage(w, r, sess, user, boxes)
		return
	}
	box, _ := pickBox(boxes, want)
	v := viewFromQuery(r)
	pg := Page{
		Chrome: h.k.Chrome(r, "Email", "mail", sess, user),
		Boxes:  boxes, Box: box,
		Folders: h.folderVMs(r, user, box),
		View:    v,
		UndoMs:  dmail.UndoWindow(user.Prefs).Milliseconds(),
	}
	_, remaining := allowanceFor(h.siteConfig(r), user)
	pg.CanCreate = remaining > 0
	cctx, cancel := kernel.Ctx(r)
	labels, _ := h.k.Mail.ListLabels(cctx, user.Username)
	cancel()
	for _, l := range labels {
		pg.Labels = append(pg.Labels, labelVM(l))
	}
	pg.Title = viewTitle(v, pg.Folders, pg.Labels)

	rows, next, err := h.listView(r, user, box, v, r.URL.Query().Get("cursor"))
	if err != nil {
		h.k.Log.Warn("thread list failed", "user", user.Username, "err", err)
	}
	pg.Rows, pg.NextCursor = rows, next
	for _, row := range rows {
		if row.Unread {
			pg.Unread++
		}
	}
	if tid := r.URL.Query().Get("thread"); tid != "" {
		if vm, found := h.threadVM(r, user, box, tid); found {
			pg.Thread = &vm
		}
	}
	ui.Render(w, h.views, "mail", pg)
}
