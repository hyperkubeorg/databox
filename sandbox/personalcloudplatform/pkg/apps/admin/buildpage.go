// buildpage.go — the Builds admin console (Draft 003 §6.1/§7/§11): one
// task page for the whole subsystem's control plane. The Builds master
// switch itself is owned by the Services page (Draft 004 §6) — this page
// only reflects its state and links there. Everything else lives here:
// the terminal-build retention window (§10.2), the compute allowlist
// (§4.4), and the runner pairing wizard + per-runner throttle (§6.1,
// §7.1), cloned from the Web access gateway surface.
package admin

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// buildAccessModes is the access-mode selector's options.
var buildAccessModes = []string{site.BuildAccessAllowlist, site.BuildAccessEveryone}

// BuildRunnerRow is one runner on the Builds home list.
type BuildRunnerRow struct {
	build.Runner
	Answering bool
}

// BuildPage is /admin/build: the master-switch state, retention window,
// the compute allowlist editor, and the runners list.
type BuildPage struct {
	shell
	// Enabled reflects the Services-owned master switch (read-only here).
	Enabled bool
	// RetentionDays is the stored value (0 = the DefaultRetention default).
	RetentionDays    int
	DefaultRetention int
	AccessMode       string
	AccessModes      []string
	Access           []build.AccessEntry
	Runners          []BuildRunnerRow
	// DefaultMax labels the per-runner throttle placeholder.
	DefaultMax int
}

func (h *handlers) buildHome(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := BuildPage{
		shell:            h.shell(r, "Builds", "build", sess, user),
		DefaultRetention: site.DefaultBuildRetentionDays,
		AccessModes:      buildAccessModes,
		DefaultMax:       build.DefaultMaxConcurrent,
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, _ := h.k.Site.Get(cctx)
	pg.Enabled = sc.BuildEnabled()
	pg.RetentionDays = sc.Build.RetentionDays
	pg.AccessMode = sc.BuildAccessMode()
	if entries, err := h.k.Build.ListAccess(cctx); err != nil {
		pg.Error = "couldn't load the compute allowlist"
	} else {
		pg.Access = entries
	}
	runners, err := h.k.Build.ListRunners(cctx)
	if err != nil {
		pg.Error = "couldn't load runners"
	}
	for _, rn := range runners {
		pg.Runners = append(pg.Runners, BuildRunnerRow{
			Runner:    rn,
			Answering: rn.Status == build.RunnerActive && time.Since(rn.LastSeen) < 2*time.Minute,
		})
	}
	h.render(w, "admin_build", pg)
}

// buildRetention stores the terminal-build retention window (§10.2).
func (h *handlers) buildRetention(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	days, err := formInt(r, "retention_days")
	if err != nil || days < 0 || days > 3650 {
		h.k.Respond(w, r, "/admin/build", fmt.Errorf("retention must be a whole number of days 0–3650 (0 = default %d)", site.DefaultBuildRetentionDays), nil)
		return
	}
	h.mutate(w, r, sess, user, "build.retention", fmt.Sprintf("%d", days), "/admin/build", "retention+saved", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Site.Update(cctx, func(c *site.Config) error {
			c.Build.RetentionDays = days
			return nil
		})
	})
}

// buildAccessMode flips the compute access mode (§4.4).
func (h *handlers) buildAccessMode(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	mode := r.FormValue("mode")
	if !site.ValidBuildAccessMode(mode) {
		h.k.Respond(w, r, "/admin/build", fmt.Errorf("bad access mode %q", mode), nil)
		return
	}
	h.mutate(w, r, sess, user, "build.access.mode", mode, "/admin/build", "access+mode+saved", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Site.Update(cctx, func(c *site.Config) error {
			c.Build.AccessMode = mode
			return nil
		})
	})
}

// buildAccessAdd grants one subject compute access (§4.4).
func (h *handlers) buildAccessAdd(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	subject := strings.TrimSpace(r.FormValue("subject"))
	h.mutate(w, r, sess, user, "build.access.add", subject, "/admin/build", "subject+added", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Build.AddAccess(cctx, subject, user.Username)
	})
}

// buildAccessRemove revokes one subject's compute access.
func (h *handlers) buildAccessRemove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	subject := strings.TrimSpace(r.FormValue("subject"))
	h.mutate(w, r, sess, user, "build.access.remove", subject, "/admin/build", "subject+removed", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Build.RemoveAccess(cctx, subject)
	})
}

// BuildRunnerPage is one runner's detail / pairing wizard.
type BuildRunnerPage struct {
	shell
	R build.Runner
	// SetupBlob is the pairing code, shown while pending.
	SetupBlob  string
	Answering  bool
	DefaultMax int
}

func (h *handlers) buildRunnerDetail(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rn, found, err := h.k.Build.GetRunner(cctx, r.PathValue("id"))
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	pg := BuildRunnerPage{
		shell:      h.shell(r, rn.Name, "build", sess, user),
		R:          rn,
		DefaultMax: build.DefaultMaxConcurrent,
		Answering:  rn.Status == build.RunnerActive && time.Since(rn.LastSeen) < 2*time.Minute,
	}
	if rn.Status == build.RunnerPending {
		pg.SetupBlob = rn.SetupBlob()
	}
	h.render(w, "admin_build_runner", pg)
}

// buildRunnerCreate mints a pending runner and jumps to its pairing page
// (mirrors waGWCreate — the setup blob is shown on the detail page).
func (h *handlers) buildRunnerCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	scope := strings.TrimSpace(r.FormValue("scope"))
	if scope == "" {
		scope = build.ScopeSystem
	}
	rn, _, err := h.k.Build.CreateRunner(cctx, strings.TrimSpace(r.FormValue("name")), scope, user.Username)
	if err != nil {
		h.k.Respond(w, r, "/admin/build", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "build.runner.create", rn.ID, rn.Name)
	h.k.Respond(w, r, "/admin/build/runners/"+rn.ID+"?ok=runner+created+—+follow+the+pairing+steps", nil, nil)
}

// runnerForm resolves the target runner for a POST (form field "id").
func (h *handlers) runnerForm(r *http.Request) (build.Runner, string, error) {
	id := r.FormValue("id")
	back := "/admin/build/runners/" + id
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rn, found, err := h.k.Build.GetRunner(cctx, id)
	if err != nil {
		return rn, back, err
	}
	if !found {
		return rn, "/admin/build", users.ErrNotFound
	}
	return rn, back, nil
}

func (h *handlers) buildRunnerPair(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	rn, back, err := h.runnerForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "build.runner.pair", rn.ID, back, "paired+—+the+runner+is+ready", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, err := h.k.Build.CompletePairing(cctx, rn.ID, r.FormValue("completion"))
		return err
	})
}

func (h *handlers) buildRunnerRepair(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	rn, back, err := h.runnerForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "build.runner.repair", rn.ID, back, "re-pair+started+—+run+setup+on+the+runner+again", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, err := h.k.Build.RepairRunner(cctx, rn.ID)
		return err
	})
}

func (h *handlers) buildRunnerStatus(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	rn, back, err := h.runnerForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	disable := r.FormValue("action") == "disable"
	flash := "runner+enabled"
	if disable {
		flash = "runner+disabled"
	}
	h.mutate(w, r, sess, user, "build.runner.status", rn.ID, back, flash, nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Build.SetRunnerStatus(cctx, rn.ID, disable)
	})
}

// buildRunnerThrottle sets one runner's concurrency cap (§7.1).
func (h *handlers) buildRunnerThrottle(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	rn, back, err := h.runnerForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	n, err := formInt(r, "max_concurrent")
	if err != nil {
		h.k.Respond(w, r, back, fmt.Errorf("max concurrent must be a whole number"), nil)
		return
	}
	h.mutate(w, r, sess, user, "build.runner.throttle", fmt.Sprintf("%s %d", rn.ID, n), back, "throttle+saved", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Build.SetMaxConcurrent(cctx, rn.ID, n)
	})
}

func (h *handlers) buildRunnerDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	rn, back, err := h.runnerForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "build.runner.delete", rn.ID+" "+rn.Name, "/admin/build", "runner+removed", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Build.DeleteRunner(cctx, rn.ID)
	})
}
