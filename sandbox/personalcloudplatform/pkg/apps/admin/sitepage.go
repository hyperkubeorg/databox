// sitepage.go — Site: branding (the site name) and the signup mode,
// each mode explained in plain language so the choice is a sentence,
// not a constant.
package admin

import (
	"net/http"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// SignupModeOption is one selectable mode with its explanation.
type SignupModeOption struct {
	Value, Label, Blurb string
}

// SignupModes is the selector, in strictness order.
var SignupModes = []SignupModeOption{
	{site.SignupOpen, "Open", "Anyone who can reach the site may create an account."},
	{site.SignupInvite, "Invite", "New accounts need an invite code — any member can mint one."},
	{site.SignupTrusted, "Trusted invite", "New accounts need an invite code — only members you've granted the “invite” capability (and admins) can mint."},
	{site.SignupAdmin, "Admin invite", "New accounts need an invite code — only admins can mint."},
}

// SitePage is /admin/site's typed page struct.
type SitePage struct {
	shell
	SC    site.Config
	Modes []SignupModeOption
}

func (h *handlers) sitePage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := SitePage{shell: h.shell(r, "Branding & signup", "site", sess, user), Modes: SignupModes}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg.SC, _ = h.k.Site.Get(cctx)
	h.render(w, "admin_site", pg)
}

func (h *handlers) siteSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	name := strings.TrimSpace(r.FormValue("name"))
	mode := r.FormValue("signup_mode")
	// Messenger enablement lives on the Services page now (Draft 004 §10);
	// this page owns only branding + signup.
	h.mutate(w, r, sess, user, "site.config", "mode="+mode, "/admin/site", "saved", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Site.Update(cctx, func(c *site.Config) error {
			c.Name = name
			if mode != "" {
				c.SignupMode = mode
			}
			return nil
		})
	})
}
