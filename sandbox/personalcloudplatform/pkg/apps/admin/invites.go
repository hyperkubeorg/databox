// invites.go — People → Invites: every invite on the site with its
// redemption ledger, admin minting (descriptions required — a standing
// door needs a written "why"), and revocation of anyone's code.
package admin

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/invites"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// InviteVM decorates an invite with its derived status and uses.
type InviteVM struct {
	invites.Invite
	Status string
	Uses   []invites.InviteUse
}

// InvitesAdminPage lists every invite with its ledger.
type InvitesAdminPage struct {
	shell
	Invites    []InviteVM
	Cursor     string
	SignupMode string
}

func (h *handlers) invitesPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := InvitesAdminPage{shell: h.shell(r, "Invites", "invites", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if sc, err := h.k.Site.Get(cctx); err == nil {
		pg.SignupMode = sc.SignupMode
	}
	list, next, err := h.k.Invites.List(cctx, r.URL.Query().Get("cursor"), 100)
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	pg.Cursor = next
	now := time.Now()
	for _, inv := range list {
		vm := InviteVM{Invite: inv, Status: inv.Status(now)}
		if uses, err := h.k.Invites.Uses(cctx, inv.Code); err == nil {
			vm.Uses = uses
		}
		pg.Invites = append(pg.Invites, vm)
	}
	h.render(w, "admin_invites", pg)
}

func (h *handlers) inviteCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	back := "/admin/invites"
	kind := r.FormValue("kind")
	description := r.FormValue("description")
	maxUses, _ := strconv.Atoi(r.FormValue("max_uses"))
	var expiresAt time.Time
	if d := r.FormValue("expires"); d != "" && kind == invites.KindTime {
		dur, err := time.ParseDuration(d)
		if err != nil || dur <= 0 {
			h.k.Respond(w, r, back, fmt.Errorf("bad expiry"), nil)
			return
		}
		expiresAt = time.Now().Add(dur)
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	inv, err := h.k.Invites.Create(cctx, user, kind, description, maxUses, expiresAt)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "invite.create", inv.Code, fmt.Sprintf("kind=%s desc=%q", inv.Kind, inv.Description))
	h.k.Respond(w, r, back+"?ok=invite+created", nil, map[string]any{"code": inv.Code})
}

func (h *handlers) inviteRevoke(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	code := r.FormValue("code")
	h.mutate(w, r, sess, user, "invite.revoke", code, "/admin/invites", "invite+revoked", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Invites.Revoke(cctx, code, user.Username)
	})
}
