// Package contacts is the Contacts app (spec §8): an aggregating
// address book over every .pccard contact-card FILE the member can
// reach across their own and shared drives — the same "files are the
// source of truth, the app is a view" model as calendars. The list
// renders server-side (readable without JS); contacts.js adds search,
// inline create/edit, and delete. New cards land in the personal
// drive's Contacts folder (created lazily); shared-drive cards are a
// shared address book. The mail composer's typeahead reads the same
// aggregation through the SuggestRecipients seam.
package contacts

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"

	dcontacts "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/contacts"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

//go:embed *.tpl
var tplFS embed.FS

//go:embed assets
var assetFS embed.FS

// handlers carries the parsed template set alongside the kernel.
type handlers struct {
	k     *kernel.App
	views *template.Template
}

// Mount registers the Contacts app's routes. Called explicitly from
// cmd/pcp.
func Mount(k *kernel.App) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	return kernel.Mount{App: "contacts", Routes: []kernel.Route{
		{Pattern: "GET /contacts", Handler: k.Authed(k.FeatureGate("contacts", h.page))},
		{Pattern: "GET /contacts/api/list", Handler: k.Authed(k.FeatureGate("contacts", h.apiList))},
		{Pattern: "POST /contacts/do/new", Handler: k.Authed(k.FeatureGate("contacts", h.doNew))},
		{Pattern: "POST /contacts/save", Handler: k.Authed(k.FeatureGate("contacts", h.save))},
		{Pattern: "POST /contacts/delete", Handler: k.Authed(k.FeatureGate("contacts", h.delete))},
		{Pattern: "GET /contacts/assets/", Handler: k.FeatureGateHTTP("contacts", assetHandler())},
	}}
}

// assetHandler serves the app's embedded JS/CSS (cache like /static/).
func assetHandler() http.Handler {
	fileServer := http.FileServer(http.FS(assetFS))
	return http.StripPrefix("/contacts/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", ui.AssetCache(r.URL.Path))
		fileServer.ServeHTTP(w, r)
	}))
}

// DriveVM is one writable save-target in the rail select.
type DriveVM struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Page is /contacts' typed page struct (the SSR list; contacts.js
// re-renders from /contacts/api/list).
type Page struct {
	kernel.Chrome
	Cards      []dcontacts.Entry
	Drives     []DriveVM
	FocusDrive string
	FocusNode  string
}

// page renders the Contacts app.
func (h *handlers) page(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	cards, ds := h.aggregate(r, user)
	pg := Page{
		Chrome: h.k.Chrome(r, "Contacts", "contacts", sess, user),
		Cards:  cards, Drives: ds,
		FocusDrive: r.URL.Query().Get("drive"),
		FocusNode:  r.URL.Query().Get("node"),
	}
	ui.Render(w, h.views, "contacts", pg)
}

// aggregate loads the member's cards + writable drives (soft-fail).
func (h *handlers) aggregate(r *http.Request, user users.User) ([]dcontacts.Entry, []DriveVM) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	cards, err := h.k.Contacts.Aggregate(cctx, user.Username)
	if err != nil {
		h.k.Log.Warn("contacts aggregation failed", "user", user.Username, "err", err)
	}
	var ds []DriveVM
	if infos, err := h.k.Drives.UserDriveInfos(cctx, user.Username); err == nil {
		for _, d := range infos {
			if drives.RoleAtLeast(d.Role, drives.RoleEditor) {
				ds = append(ds, DriveVM{ID: d.ID, Name: d.Name})
			}
		}
	}
	return cards, ds
}

// apiList answers the aggregated address book.
func (h *handlers) apiList(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	cards, ds := h.aggregate(r, user)
	if cards == nil {
		cards = []dcontacts.Entry{}
	}
	if ds == nil {
		ds = []DriveVM{}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "cards": cards, "drives": ds})
}

// cardFromForm reads a Card out of the request form. Custom fields ride
// as a JSON array in "fields" (Sanitize validates every entry; a body
// that doesn't parse means no fields — the app always sends valid JSON).
func cardFromForm(r *http.Request) dcontacts.Card {
	var fields []dcontacts.Field
	if raw := r.FormValue("fields"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &fields)
	}
	return dcontacts.Card{
		Name:   r.FormValue("name"),
		Emails: splitMulti(r.FormValue("emails")),
		Phones: splitMulti(r.FormValue("phones")),
		Org:    r.FormValue("org"),
		Title:  r.FormValue("title"),
		Notes:  r.FormValue("notes"),
		Fields: fields,
	}
}

// splitMulti splits a multi-value field on newlines, commas, or semis.
func splitMulti(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' || r == ';' }) {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// quotaFor resolves the member's effective storage quota.
func (h *handlers) quotaFor(r *http.Request, user users.User) int64 {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		h.k.Log.Warn("site config read failed", "err", err)
	}
	return site.QuotaFor(sc, user.QuotaOverride, user.Tier, h.k.DefaultQuota)
}

// doNew creates a card file — the personal drive's Contacts folder by
// default, a picked writable drive's root otherwise.
func (h *handlers) doNew(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID := r.FormValue("drive")
	back := "/contacts"
	if driveID == "" {
		driveID = user.PersonalDrive
	}
	if err := h.access(r, user, driveID, nodes.RootID, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	driveID, node, err := h.k.Contacts.CreateCard(cctx, user, driveID, cardFromForm(r), h.quotaFor(r, user))
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Respond(w, r, back+"?ok=saved", nil, map[string]any{"drive": driveID, "node": node.ID})
}

// save overwrites an existing card (editor rights on the file).
func (h *handlers) save(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID := r.FormValue("drive"), r.FormValue("node")
	back := "/contacts"
	if err := h.access(r, user, driveID, nodeID, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if _, err := h.k.Contacts.SaveCard(cctx, driveID, nodeID, user.Username, cardFromForm(r)); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Respond(w, r, back+"?ok=saved", nil, nil)
}

// delete removes a card file for good (editor rights; cards are small
// and the file lives on in drive versions until the node goes).
func (h *handlers) delete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID := r.FormValue("drive"), r.FormValue("node")
	back := "/contacts"
	if err := h.access(r, user, driveID, nodeID, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || !dcontacts.IsCardFile(node) {
		http.NotFound(w, r)
		return
	}
	if _, err := h.k.Shares.DeleteNode(cctx, driveID, nodeID); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Respond(w, r, back+"?ok=deleted", nil, nil)
}

// access resolves the member's role for a node (the drive app's twin).
func (h *handlers) access(r *http.Request, user users.User, driveID, nodeID, minRole string) error {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	role, _, err := h.k.Shares.Access(cctx, user.Username, driveID, nodeID)
	if err != nil {
		return err
	}
	if !drives.RoleAtLeast(role, minRole) {
		return drives.ErrAccessDenied
	}
	return nil
}
