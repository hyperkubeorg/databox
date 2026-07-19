// Package invitespage is the member-facing invite surface (/invites),
// ported from PCD: list your invites with their redemption ledgers,
// mint new ones (who may mint is the signup mode's call — re-checked
// server-side on every request), revoke your own. Admins manage the
// whole site's invites from /admin/invites.
package invitespage

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/invites"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

//go:embed *.tpl
var tplFS embed.FS

type handlers struct {
	k     *kernel.App
	views *template.Template
}

// InviteVM decorates an invite with its derived status and uses.
type InviteVM struct {
	invites.Invite
	Status string
	Uses   []invites.InviteUse
}

// Page is /invites' typed page struct.
type Page struct {
	kernel.Chrome
	Mine       []InviteVM
	CanCreate  bool
	CanPerm    bool
	SignupMode string
}

// Mount registers the member invite routes. Called explicitly from
// cmd/pcp.
func Mount(k *kernel.App) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	return kernel.Mount{App: "invites", Routes: []kernel.Route{
		{Pattern: "GET /invites", Handler: k.Authed(h.page)},
		{Pattern: "POST /invites/create", Handler: k.Authed(h.create)},
		{Pattern: "POST /invites/revoke", Handler: k.Authed(h.revoke)},
	}}
}

func (h *handlers) page(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		h.k.Log.Warn("site config read failed", "err", err)
	}
	mine, err := h.k.Invites.CreatedBy(cctx, user.Username)
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	pg := Page{
		Chrome:     h.k.Chrome(r, "Invites", "settings", sess, user),
		CanCreate:  invites.CanCreate(user, sc.SignupMode),
		CanPerm:    invites.CanCreatePermanent(user),
		SignupMode: sc.SignupMode,
	}
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")
	now := time.Now()
	for _, inv := range mine {
		vm := InviteVM{Invite: inv, Status: inv.Status(now)}
		if uses, err := h.k.Invites.Uses(cctx, inv.Code); err == nil {
			vm.Uses = uses
		}
		pg.Mine = append(pg.Mine, vm)
	}
	ui.Render(w, h.views, "invites", pg)
}

// parseCreateForm reads an invite-create form into Create's arguments.
func parseCreateForm(r *http.Request) (kind, description string, maxUses int, expiresAt time.Time, err error) {
	kind = r.FormValue("kind")
	description = r.FormValue("description")
	maxUses, _ = strconv.Atoi(r.FormValue("max_uses"))
	if d := r.FormValue("expires"); d != "" && kind == invites.KindTime {
		dur, perr := time.ParseDuration(d)
		if perr != nil || dur <= 0 {
			return "", "", 0, time.Time{}, fmt.Errorf("bad expiry")
		}
		expiresAt = time.Now().Add(dur)
	}
	return kind, description, maxUses, expiresAt, nil
}

func (h *handlers) create(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	back := "/invites"
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		h.k.Respond(w, r, back, fmt.Errorf("try again"), nil)
		return
	}
	if !invites.CanCreate(user, sc.SignupMode) {
		h.k.Respond(w, r, back, invites.ErrAccessDenied, nil)
		return
	}
	kind, description, maxUses, expiresAt, err := parseCreateForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	if kind == invites.KindPermanent && !invites.CanCreatePermanent(user) {
		h.k.Respond(w, r, back, invites.ErrAccessDenied, nil)
		return
	}
	inv, err := h.k.Invites.Create(cctx, user, kind, description, maxUses, expiresAt)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "invite.create", inv.Code, fmt.Sprintf("kind=%s desc=%q", inv.Kind, inv.Description))
	h.k.Respond(w, r, back+"?ok=invite+created", nil, map[string]any{"code": inv.Code})
}

func (h *handlers) revoke(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	back := "/invites"
	code := r.FormValue("code")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	inv, found, err := h.k.Invites.Get(cctx, code)
	if err != nil || !found || (inv.CreatedBy != user.Username && !user.IsAdmin) {
		h.k.Respond(w, r, back, invites.ErrNotFound, nil)
		return
	}
	if err := h.k.Invites.Revoke(cctx, code, user.Username); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "invite.revoke", code, "")
	h.k.Respond(w, r, back+"?ok=invite+revoked", nil, nil)
}
