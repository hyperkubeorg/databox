// smarthomepage.go — the Smart Home admin console (Draft 005 §12): the
// creation access mode + allowlist (§3.1) and the instance caps. The
// master switch itself is owned by the Services page (Draft 004 §6) —
// this page only reflects its state and links there.
package admin

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/smarthome"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// smarthomeAccessModes is the access-mode selector's options.
var smarthomeAccessModes = []string{site.SmartHomeAccessAllowlist, site.SmartHomeAccessEveryone}

// SmartHomePage is /admin/smarthome: the master-switch state, the
// creation allowlist editor, and the instance caps.
type SmartHomePage struct {
	shell
	// Enabled reflects the Services-owned master switch (read-only here).
	Enabled     bool
	AccessMode  string
	AccessModes []string
	Access      []smarthome.AccessEntry
	// Stored cap values (0 = default) and their defaults for labels.
	C        site.SmartHomeConfig
	Defaults site.SmartHomeConfig
}

func (h *handlers) smarthomeHome(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := SmartHomePage{
		shell:       h.shell(r, "Smart Home", "smarthome", sess, user),
		AccessModes: smarthomeAccessModes,
		Defaults: site.SmartHomeConfig{
			MaxSpacesPerUser:   site.DefaultSmartHomeMaxSpaces,
			MaxCamerasPerSpace: site.DefaultSmartHomeMaxCameras,
			MaxAgentsPerSpace:  site.DefaultSmartHomeMaxAgents,
			MaxMembersPerSpace: site.DefaultSmartHomeMaxMembers,
			MaxRetentionDays:   site.DefaultSmartHomeMaxRetention,
		},
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, _ := h.k.Site.Get(cctx)
	pg.Enabled = sc.SmartHomeEnabled()
	pg.AccessMode = sc.SmartHomeAccessMode()
	pg.C = sc.SmartHome
	if entries, err := h.k.SmartHome.ListAccess(cctx); err != nil {
		pg.Error = "couldn't load the creation allowlist"
	} else {
		pg.Access = entries
	}
	h.render(w, "admin_smarthome", pg)
}

// smarthomeAccessMode flips the creation access mode (§3.1).
func (h *handlers) smarthomeAccessMode(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	mode := r.FormValue("mode")
	if !site.ValidSmartHomeAccessMode(mode) {
		h.k.Respond(w, r, "/admin/smarthome", fmt.Errorf("bad access mode %q", mode), nil)
		return
	}
	h.mutate(w, r, sess, user, "smarthome.access.mode", mode, "/admin/smarthome", "access+mode+saved", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Site.Update(cctx, func(c *site.Config) error {
			c.SmartHome.AccessMode = mode
			return nil
		})
	})
}

// smarthomeAccessAdd grants one user creation access (§3.1). Bare
// usernames are accepted and normalized to the u: subject form.
func (h *handlers) smarthomeAccessAdd(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	subject := strings.ToLower(strings.TrimSpace(r.FormValue("subject")))
	if subject != "" && !strings.HasPrefix(subject, smarthome.AccessUserPrefix) {
		subject = smarthome.AccessUserPrefix + subject
	}
	h.mutate(w, r, sess, user, "smarthome.access.add", subject, "/admin/smarthome", "user+added", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		name := strings.TrimPrefix(subject, smarthome.AccessUserPrefix)
		if _, found, err := h.k.Users.Get(cctx, name); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("no account named %q", name)
		}
		return h.k.SmartHome.AddAccess(cctx, subject, user.Username)
	})
}

// smarthomeAccessRemove revokes one user's creation access.
func (h *handlers) smarthomeAccessRemove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	subject := strings.TrimSpace(r.FormValue("subject"))
	h.mutate(w, r, sess, user, "smarthome.access.remove", subject, "/admin/smarthome", "user+removed", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.SmartHome.RemoveAccess(cctx, subject)
	})
}

// smarthomeCaps stores the instance caps (§12) in one form.
func (h *handlers) smarthomeCaps(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	fields := []struct {
		form string
		dst  func(*site.SmartHomeConfig, int)
	}{
		{"max_spaces", func(c *site.SmartHomeConfig, v int) { c.MaxSpacesPerUser = v }},
		{"max_cameras", func(c *site.SmartHomeConfig, v int) { c.MaxCamerasPerSpace = v }},
		{"max_agents", func(c *site.SmartHomeConfig, v int) { c.MaxAgentsPerSpace = v }},
		{"max_members", func(c *site.SmartHomeConfig, v int) { c.MaxMembersPerSpace = v }},
		{"max_retention", func(c *site.SmartHomeConfig, v int) { c.MaxRetentionDays = v }},
	}
	vals := make([]int, len(fields))
	for i, f := range fields {
		v, err := formInt(r, f.form)
		if err != nil || v < 0 {
			h.k.Respond(w, r, "/admin/smarthome", fmt.Errorf("caps must be whole numbers (0 = default)"), nil)
			return
		}
		vals[i] = v
	}
	h.mutate(w, r, sess, user, "smarthome.caps", "", "/admin/smarthome", "caps+saved", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Site.Update(cctx, func(c *site.Config) error {
			for i, f := range fields {
				f.dst(&c.SmartHome, vals[i])
			}
			return nil
		})
	})
}
