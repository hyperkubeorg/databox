// threads.go — the JSON state feeds mail.js renders from, and the
// thread mutations (read/star/move/label/purge, folder + label CRUD).
// Every mutation is progressive-enhancement: a plain form POST
// (redirect) and a fetch() call (JSON) share one handler via
// kernel.Respond; fetch callers get the mutated state back for
// in-place re-render.
package mail

import (
	"net/http"
	"strings"

	dmail "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailrender"
)

// listLimit is one list-pane page ("load more" continues the cursor).
const listLimit = 50

// viewSpec is one list-pane view: a folder, a facet, a label, or a
// search.
type viewSpec struct {
	View   string // inbox|archive|spam|trash|starred|sent|drafts|custom-id|label
	Label  string // labelID when View == "label"
	Query  string
	Filter string // all|unread|starred|attach
}

// viewFromQuery reads the list-view params.
func viewFromQuery(r *http.Request) viewSpec {
	q := r.URL.Query()
	v := viewSpec{
		View:   q.Get("folder"),
		Label:  q.Get("label"),
		Query:  strings.TrimSpace(q.Get("q")),
		Filter: q.Get("filter"),
	}
	if v.Label != "" {
		v.View = "label"
	}
	if v.View == "" {
		v.View = dmail.FolderInbox
	}
	return v
}

// listView pages one view's threads into row VMs.
func (h *handlers) listView(r *http.Request, user users.User, box dmail.Mailbox, v viewSpec, cursor string) (rows []RowVM, next string, err error) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	labels, _ := h.k.Mail.ListLabels(cctx, user.Username)
	self := h.selfAddrSet(cctx, user.Username)
	sc := h.siteConfig(r)

	if v.View == "drafts" {
		ds, err := h.k.Mail.ListDrafts(cctx, user.Username, box.ID)
		if err != nil {
			return nil, "", err
		}
		for _, d := range ds {
			subj := d.Subject
			if subj == "" {
				subj = "(no subject)"
			}
			to := "Draft"
			if len(d.To) > 0 {
				to = "To: " + displayName(d.To[0])
			}
			rows = append(rows, RowVM{
				ThreadID: "draft-" + d.ID, IsDraft: true, DraftID: d.ID,
				From: to, Time: rowTime(d.UpdatedAt), Subject: subj,
				Snippet: dmail.Snippet(dmail.HTMLToText(d.HTML) + d.Text),
				Files:   len(d.Atts),
				Avatars: []AvatarVM{avatarFor(box.Addr)},
			})
		}
		return rows, "", nil
	}

	var metas []dmail.ThreadMeta
	switch {
	case v.Query != "":
		metas, err = h.k.Mail.SearchThreads(cctx, user.Username, box.ID, dmail.ParseQuery(v.Query), listLimit)
	case v.View == "starred":
		metas, next, err = h.k.Mail.ListStarred(cctx, user.Username, box.ID, cursor, listLimit)
	case v.View == "sent":
		metas, next, err = h.k.Mail.ListSent(cctx, user.Username, box.ID, cursor, listLimit)
	case v.View == "label":
		metas, next, err = h.k.Mail.ListByLabel(cctx, user.Username, v.Label, cursor, listLimit)
	default:
		metas, next, err = h.k.Mail.ListThreads(cctx, user.Username, box.ID, v.View, cursor, listLimit, sc.Mail.TrashRetentionDays())
	}
	if err != nil {
		return nil, "", err
	}
	sentView := v.View == "sent"
	for _, m := range metas {
		switch v.Filter {
		case "unread":
			if m.UnreadCount == 0 {
				continue
			}
		case "starred":
			if !m.Starred {
				continue
			}
		case "attach":
			if m.AttachCount == 0 {
				continue
			}
		}
		// Label views can carry rows from other mailboxes (labels are
		// per-user); the list pane shows the current mailbox only.
		if m.BoxID != box.ID {
			continue
		}
		rows = append(rows, h.rowVM(m, labels, self, sentView))
	}
	return rows, next, nil
}

// folderVMs assembles the sidebar rail with live counts: Inbox counts
// unread threads, everything else totals (bounded — the census cap is
// the badge's ceiling).
func (h *handlers) folderVMs(r *http.Request, user users.User, box dmail.Mailbox) []FolderVM {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	count := func(list func() ([]dmail.ThreadMeta, string, error)) int {
		rows, _, err := list()
		if err != nil {
			return 0
		}
		return len(rows)
	}
	u := user.Username
	unread, _ := h.k.Mail.UnreadThreads(cctx, u, box.ID, dmail.FolderInbox)
	drafts, _ := h.k.Mail.ListDrafts(cctx, u, box.ID)
	out := []FolderVM{
		{ID: "inbox", Name: "Inbox", Icon: "inbox", Count: unread},
		{ID: "starred", Name: "Starred", Icon: "star", Count: count(func() ([]dmail.ThreadMeta, string, error) {
			return h.k.Mail.ListStarred(cctx, u, box.ID, "", 999)
		})},
		{ID: "sent", Name: "Sent", Icon: "sent", Count: count(func() ([]dmail.ThreadMeta, string, error) {
			return h.k.Mail.ListSent(cctx, u, box.ID, "", 999)
		})},
		{ID: "drafts", Name: "Drafts", Icon: "drafts", Count: len(drafts)},
		{ID: "archive", Name: "Archive", Icon: "archive", Count: count(func() ([]dmail.ThreadMeta, string, error) {
			return h.k.Mail.ListThreads(cctx, u, box.ID, dmail.FolderArchive, "", 999, 0)
		})},
		{ID: "spam", Name: "Spam", Icon: "spam", Count: count(func() ([]dmail.ThreadMeta, string, error) {
			return h.k.Mail.ListThreads(cctx, u, box.ID, dmail.FolderSpam, "", 999, 0)
		})},
		{ID: "trash", Name: "Trash", Icon: "trash", Count: count(func() ([]dmail.ThreadMeta, string, error) {
			return h.k.Mail.ListThreads(cctx, u, box.ID, dmail.FolderTrash, "", 999, 0)
		})},
	}
	custom, _ := h.k.Mail.ListFolders(cctx, u, box.ID)
	for _, f := range custom {
		out = append(out, FolderVM{ID: f.ID, Name: f.Name, Icon: "folder", Custom: true, Count: count(func() ([]dmail.ThreadMeta, string, error) {
			return h.k.Mail.ListThreads(cctx, u, box.ID, f.ID, "", 999, 0)
		})})
	}
	return out
}

// threadVM loads one conversation for the reading pane: meta, messages
// oldest-first, sanitized bodies, attachment chips. All messages but
// the last collapse (mockup behavior).
func (h *handlers) threadVM(r *http.Request, user users.User, box dmail.Mailbox, threadID string) (ThreadVM, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	meta, found, err := h.k.Mail.GetThread(cctx, user.Username, box.ID, threadID)
	if err != nil || !found {
		return ThreadVM{}, false
	}
	msgs, err := h.k.Mail.ListThreadMessages(cctx, user.Username, box.ID, threadID)
	if err != nil {
		return ThreadVM{}, false
	}
	labels, _ := h.k.Mail.ListLabels(cctx, user.Username)
	self := h.selfAddrSet(cctx, user.Username)
	driveOn := h.k.FeatureEnabled(cctx, "drive")
	vm := ThreadVM{
		ThreadID: meta.ThreadID, Subject: meta.Subject, Starred: meta.Starred,
		Unread: meta.UnreadCount > 0, Folder: meta.Folder,
		Labels: labelVMs(meta.Labels, labels), MsgCount: meta.MsgCount,
		Participants: max(len(meta.Participants), 1),
	}
	if vm.Subject == "" {
		vm.Subject = "(no subject)"
	}
	for i, m := range msgs {
		name := displayName(m.From)
		first, _, _ := strings.Cut(name, " ")
		mv := MsgVM{
			MsgID: m.MsgID, FromName: name, FirstName: first, FromAddr: bareAddr(m.From),
			You: m.Outbound || self[bareAddr(m.From)], Time: msgTime(m.Date),
			Starred: m.Starred, Snippet: m.Snippet, Avatar: avatarFor(m.From),
			Collapsed: i != len(msgs)-1, MessageID: m.MessageIDHdr,
			Refs:         strings.Join(m.References, " "),
			RawURL:       "/mail/raw/" + m.MsgID,
			DriveEnabled: driveOn,
		}
		mv.To = toLine(m, self)
		raw, err := h.k.Mail.MessageBlob(cctx, user.Username, m.BlobID)
		if err == nil {
			body := mailrender.Render(raw)
			mv.Text = body.Text
			mv.HTML = safeHTML(body.HTML)
			mv.ICS = h.icsVM(r, user, body.ICS)
			for _, a := range body.Atts {
				kind, color := attStyle(a.Name, a.CT)
				mv.Atts = append(mv.Atts, AttVM{
					N: a.N, Name: a.Name, Size: sizeLabel(int64(a.Size)),
					Kind: kind, Color: color,
					URL: "/mail/att/" + m.MsgID + "/" + itoa(a.N),
				})
			}
		} else {
			mv.Text = "(message unavailable — try again)"
		}
		vm.Msgs = append(vm.Msgs, mv)
	}
	return vm, true
}

// toLine renders the expanded card's "to …" summary.
func toLine(m dmail.MsgMeta, self map[string]bool) string {
	var names []string
	for _, t := range append(append([]string{}, m.To...), m.Cc...) {
		if self[bareAddr(t)] {
			names = append(names, "you")
		} else {
			first, _, _ := strings.Cut(displayName(t), " ")
			names = append(names, first)
		}
	}
	if len(names) == 0 {
		return "you"
	}
	extra := ""
	if len(names) > 2 {
		extra = " +" + itoa(len(names)-2)
		names = names[:2]
	}
	return strings.Join(names, ", ") + extra
}

// --- JSON feeds -----------------------------------------------------------------------

// apiState answers the sidebar state: mailboxes, folders, labels, and
// the member's undo-send window.
func (h *handlers) apiState(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	boxes := h.boxes(r, user)
	box, ok := pickBox(boxes, r.URL.Query().Get("box"))
	if !ok {
		kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "boxes": []any{}})
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	labels, _ := h.k.Mail.ListLabels(cctx, user.Username)
	lvms := []LabelVM{}
	for _, l := range labels {
		lvms = append(lvms, labelVM(l))
	}
	type boxVM struct {
		ID   string `json:"id"`
		Addr string `json:"addr"`
	}
	bvms := []boxVM{}
	for _, b := range boxes {
		bvms = append(bvms, boxVM{ID: b.ID, Addr: b.Addr})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{
		"ok": true, "boxes": bvms, "box": box.ID, "addr": box.Addr,
		"folders": h.folderVMs(r, user, box), "labels": lvms,
		"undoMs": dmail.UndoWindow(user.Prefs).Milliseconds(),
	})
}

// apiThreads answers one list-pane page.
func (h *handlers) apiThreads(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	boxes := h.boxes(r, user)
	box, ok := pickBox(boxes, r.URL.Query().Get("box"))
	if !ok {
		kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "rows": []any{}})
		return
	}
	v := viewFromQuery(r)
	rows, next, err := h.listView(r, user, box, v, r.URL.Query().Get("cursor"))
	if err != nil {
		kernel.JSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": kernel.UserErr(err)})
		return
	}
	if rows == nil {
		rows = []RowVM{}
	}
	unread := 0
	for _, row := range rows {
		if row.Unread {
			unread++
		}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "rows": rows, "nextCursor": next, "unread": unread})
}

// apiThread answers one conversation.
func (h *handlers) apiThread(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	boxes := h.boxes(r, user)
	box, ok := pickBox(boxes, r.PathValue("box"))
	if !ok || box.ID != r.PathValue("box") {
		http.NotFound(w, r)
		return
	}
	vm, found := h.threadVM(r, user, box, r.PathValue("thread"))
	if !found {
		http.NotFound(w, r)
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "thread": vm})
}

// apiSuggest answers compose recipient typeahead.
func (h *handlers) apiSuggest(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	out := h.k.Mail.SuggestRecipients(cctx, user.Username, r.URL.Query().Get("q"), 8)
	if out == nil {
		out = []string{}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "hits": out})
}

// --- mutations -----------------------------------------------------------------------

// mutate wraps the shared boilerplate: CSRF, mailbox ownership, the
// operation, kernel.Respond.
func (h *handlers) mutate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User, op func(box dmail.Mailbox) (map[string]any, error)) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	boxes := h.boxes(r, user)
	box, ok := pickBox(boxes, r.FormValue("box"))
	if !ok || (r.FormValue("box") != "" && box.ID != r.FormValue("box")) {
		http.NotFound(w, r)
		return
	}
	payload, err := op(box)
	back := r.FormValue("back")
	if back == "" || !strings.HasPrefix(back, "/mail") {
		back = "/mail?box=" + box.ID
	}
	h.k.Respond(w, r, back, err, payload)
}

func (h *handlers) doRead(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return nil, h.k.Mail.MarkThreadRead(cctx, user.Username, box.ID, r.FormValue("thread"), r.FormValue("read") != "0")
	})
}

func (h *handlers) doStar(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		on := r.FormValue("on") == "1"
		return map[string]any{"starred": on}, h.k.Mail.SetThreadStarred(cctx, user.Username, box.ID, r.FormValue("thread"), on)
	})
}

func (h *handlers) doStarMsg(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	on := r.FormValue("on") == "1"
	err := h.k.Mail.SetMessageStarred(cctx, user.Username, r.FormValue("msg"), on)
	h.k.Respond(w, r, "/mail", err, map[string]any{"starred": on})
}

func (h *handlers) doMove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		to := r.FormValue("to")
		// The undo payload names the folder the thread came FROM.
		meta, found, err := h.k.Mail.GetThread(cctx, user.Username, box.ID, r.FormValue("thread"))
		if err != nil || !found {
			return nil, dmail.ErrNotFound
		}
		if err := h.k.Mail.MoveThread(cctx, user.Username, box.ID, r.FormValue("thread"), to); err != nil {
			return nil, err
		}
		return map[string]any{"from": meta.Folder, "to": to}, nil
	})
}

func (h *handlers) doLabel(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		on := r.FormValue("on") == "1"
		return map[string]any{"on": on}, h.k.Mail.SetThreadLabel(cctx, user.Username, box.ID, r.FormValue("thread"), r.FormValue("label"), on)
	})
}

func (h *handlers) doPurge(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return nil, h.k.Mail.PurgeThread(cctx, user.Username, box.ID, r.FormValue("thread"))
	})
}

func (h *handlers) doEmptyTrash(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return nil, h.k.Mail.EmptyTrash(cctx, user.Username, box.ID)
	})
}

func (h *handlers) doFolderCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		f, err := h.k.Mail.CreateFolder(cctx, user.Username, box.ID, r.FormValue("name"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"folder": f}, nil
	})
}

func (h *handlers) doFolderDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return nil, h.k.Mail.DeleteFolder(cctx, user.Username, box.ID, r.FormValue("folder"))
	})
}

func (h *handlers) doLabelCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(_ dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		l, err := h.k.Mail.CreateLabel(cctx, user.Username, r.FormValue("name"), r.FormValue("color"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"label": l}, nil
	})
}

func (h *handlers) doLabelUpdate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(_ dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		order := 0
		if n, err := atoi(r.FormValue("order")); err == nil {
			order = n
		}
		return nil, h.k.Mail.UpdateLabel(cctx, user.Username, dmail.Label{
			ID: r.FormValue("label"), Name: r.FormValue("name"),
			Color: r.FormValue("color"), Order: order,
		})
	})
}

func (h *handlers) doLabelDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(_ dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return nil, h.k.Mail.DeleteLabel(cctx, user.Username, r.FormValue("label"))
	})
}
