// servicespage.go — Services (Draft 004 §6): the one-stop feature console.
// Every launcher app is a feature, off by default; this page is the only
// place a feature is enabled or disabled, it enforces the requirement graph
// (§5) both directions, and it hosts the irreversible per-feature data purge
// (§9). Feature-specific policy stays on each feature's own page — Services
// links out. Reads the registry in pkg/domain/site, so a future feature
// appears here automatically.
package admin

import (
	"fmt"
	"net/http"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// ReqChip is one requirement of a feature and whether it is met.
type ReqChip struct {
	Name    string
	Enabled bool
}

// ServiceRow is one feature's line on the Services list.
type ServiceRow struct {
	ID            string
	Name          string
	Enabled       bool
	Requires      []ReqChip
	Dependents    []string // enabled features that require this one
	CanEnable     bool
	EnableReason  string
	CanDisable    bool
	DisableReason string
	PolicyHref    string // feature's own policy page, if any
	PolicyLabel   string
}

// ServicesPage is /admin/services' typed page struct.
type ServicesPage struct {
	shell
	Rows []ServiceRow
}

// policyLinks maps a feature to its feature-specific policy page (Draft 004
// §10: Services owns enablement, the feature page owns tuning).
var policyLinks = map[string][2]string{
	site.FeatureGit:    {"/admin/site/git", "Git Services policy"},
	site.FeatureMail:   {"/admin/mail/sending", "Mail sending policy"},
	site.FeatureBuilds: {"/admin/build", "Builds settings"},
}

// serviceRow builds one feature's row from the current config.
func serviceRow(sc site.Config, f site.Feature) ServiceRow {
	row := ServiceRow{ID: f.ID, Name: f.Name, Enabled: sc.FeatureEnabled(f.ID)}
	for _, req := range f.Requires {
		row.Requires = append(row.Requires, ReqChip{Name: site.FeatureName(req), Enabled: sc.FeatureEnabled(req)})
	}
	row.Dependents = sc.EnabledDependents(f.ID)
	row.CanEnable, row.EnableReason = sc.CanEnable(f.ID)
	row.CanDisable, row.DisableReason = sc.CanDisable(f.ID)
	if link, ok := policyLinks[f.ID]; ok {
		row.PolicyHref, row.PolicyLabel = link[0], link[1]
	}
	return row
}

func (h *handlers) servicesPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := ServicesPage{shell: h.shell(r, "Services", "services", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, _ := h.k.Site.Get(cctx)
	for _, f := range site.Features() {
		pg.Rows = append(pg.Rows, serviceRow(sc, f))
	}
	h.render(w, "admin_services", pg)
}

// serviceEnable turns a feature on, enforcing requirements (§5.2).
func (h *handlers) serviceEnable(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	id := r.PathValue("id")
	name := site.FeatureName(id)
	h.mutate(w, r, sess, user, "feature.enable", id, "/admin/services", "enabled+"+id, nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Site.EnableFeature(cctx, id)
	})
	_ = name
}

// serviceDisable turns a feature off, refusing while dependents are on (§5.3).
func (h *handlers) serviceDisable(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	id := r.PathValue("id")
	h.mutate(w, r, sess, user, "feature.disable", id, "/admin/services", "disabled+"+id, nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Site.DisableFeature(cctx, id)
	})
}

// ServiceDetailPage is /admin/services/{id}'s typed page struct — the
// requirement explanation, dependents, and the purge danger zone.
type ServiceDetailPage struct {
	shell
	Row ServiceRow
	// PurgeParts describes, in plain language, what this feature's purge
	// will destroy (Draft 004 §9.1) — shown up front in the gauntlet.
	PurgeParts []string
	// Orphans names cross-feature data a purge would leave dangling.
	Orphans []string
}

func (h *handlers) serviceDetail(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	id := r.PathValue("id")
	f, ok := site.FeatureByID(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	pg := ServiceDetailPage{shell: h.shell(r, f.Name+" — Services", "services", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, _ := h.k.Site.Get(cctx)
	pg.Row = serviceRow(sc, f)
	pg.PurgeParts = purgeParts(id)
	pg.Orphans = purgeOrphans(sc, id)
	h.render(w, "admin_service", pg)
}

// servicePurge irreversibly deletes a feature's data (§9). Allowed whether
// the feature is on or off (resolved decision). The ten-click gauntlet is a
// UI affordance; the server independently requires the admin to have typed
// the feature id as a final interlock.
func (h *handlers) servicePurge(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	id := r.PathValue("id")
	f, ok := site.FeatureByID(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	typed := r.FormValue("confirm_name")
	back := "/admin/services/" + id
	if typed != id {
		h.k.Respond(w, r, back, fmt.Errorf("type the feature id %q exactly to confirm the purge", id), nil)
		return
	}
	// Audit records start (with the intended target) and completion totals.
	h.mutate(w, r, sess, user, "feature.purge", id, back, "purged+"+id, nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		recs, bytes, err := purgeFeature(cctx, h.k, id)
		if err != nil {
			return err
		}
		// Second audit line with the concrete footprint removed.
		h.k.Audit(r, user, sess, "feature.purge.done", id, fmt.Sprintf("records=%d bytes=%d", recs, bytes))
		return nil
	})
	_ = f
}
