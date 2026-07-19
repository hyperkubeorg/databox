// drives.go — shared drives: create, membership management, rename,
// delete. Personal drives are born at signup and managed by nobody.
package drive

import (
	"errors"
	"net/http"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// DriveNewPage is the create form's typed page struct.
type DriveNewPage struct {
	kernel.Chrome
	Sidebar
}

func (h *handlers) driveNewForm(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := DriveNewPage{
		Chrome:  h.k.Chrome(r, "New shared drive", "drive", sess, user),
		Sidebar: h.sidebar(r, user, ""),
	}
	pg.Error = r.URL.Query().Get("err")
	ui.Render(w, h.views, "drive_new", pg)
}

func (h *handlers) driveCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	d, err := h.k.Drives.CreateShared(cctx, user.Username, r.FormValue("name"))
	if err != nil {
		h.k.Respond(w, r, "/drive/drives/new", err, nil)
		return
	}
	h.k.Respond(w, r, "/drive/d/"+d.ID+"/"+nodes.RootID, nil, map[string]any{"id": d.ID})
}

// driveOwner loads a drive and requires the acting user to own it (or
// be an admin).
func (h *handlers) driveOwner(r *http.Request, user users.User, driveID string) (drives.Drive, error) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	d, found, err := h.k.Drives.Get(cctx, driveID)
	if err != nil {
		return drives.Drive{}, err
	}
	if !found {
		return drives.Drive{}, users.ErrNotFound
	}
	m, isMember, err := h.k.Drives.GetMember(cctx, driveID, user.Username)
	if err != nil {
		return drives.Drive{}, err
	}
	if user.IsAdmin || (isMember && m.Role == drives.RoleOwner) {
		return d, nil
	}
	return drives.Drive{}, drives.ErrAccessDenied
}

// DriveSettingsPage is the drive settings page's typed page struct.
type DriveSettingsPage struct {
	kernel.Chrome
	Sidebar
	Drive   drives.Drive
	Members []drives.MemberRow
	IsOwner bool
	MyRole  string
}

func (h *handlers) driveSettings(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	driveID := r.PathValue("drive")
	role, err := h.access(r, user, driveID, nodes.RootID, drives.RoleViewer)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	d, found, err := h.k.Drives.Get(cctx, driveID)
	if err != nil || !found || d.Type != drives.Shared {
		http.NotFound(w, r)
		return
	}
	members, err := h.k.Drives.Members(cctx, driveID)
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	pg := DriveSettingsPage{
		Chrome:  h.k.Chrome(r, d.Name+" — drive settings", "drive", sess, user),
		Sidebar: h.sidebar(r, user, driveID),
		Drive:   d, Members: members,
		IsOwner: role == drives.RoleOwner || user.IsAdmin,
		MyRole:  role,
	}
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")
	ui.Render(w, h.views, "drive_settings", pg)
}

func (h *handlers) driveMemberSet(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID := r.PathValue("drive")
	back := "/drive/manage/" + driveID
	if _, err := h.driveOwner(r, user, driveID); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Drives.SetMember(cctx, driveID, r.FormValue("user"), r.FormValue("role"))
	h.k.Respond(w, r, back, err, nil)
}

// driveMemberRemove drops a member. Owners remove anyone (but the
// owner); a member may remove THEMSELF (leave the drive).
func (h *handlers) driveMemberRemove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID := r.PathValue("drive")
	target := r.FormValue("user")
	back := "/drive/manage/" + driveID
	if target != user.Username {
		if _, err := h.driveOwner(r, user, driveID); err != nil {
			h.k.Respond(w, r, back, err, nil)
			return
		}
	} else {
		back = "/drive" // leaving: the settings page is gone for them
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	h.k.Respond(w, r, back, h.k.Drives.RemoveMember(cctx, driveID, target), nil)
}

func (h *handlers) driveRename(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID := r.PathValue("drive")
	back := "/drive/manage/" + driveID
	if _, err := h.driveOwner(r, user, driveID); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	h.k.Respond(w, r, back, h.k.Drives.Rename(cctx, driveID, r.FormValue("name")), nil)
}

// errConfirmName refuses a drive deletion whose confirm field doesn't
// echo the drive's name.
var errConfirmName = errors.New("type the drive's exact name to confirm deletion")

// driveDelete destroys a shared drive and everything in it (owner only;
// the confirm field must echo the drive name — a deliberate speed bump).
// Composition: refund the tree's charges, purge node data, purge
// sharing, purge media registrations, then the drive record — each
// domain deletes only its own keys.
func (h *handlers) driveDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID := r.PathValue("drive")
	back := "/drive/manage/" + driveID
	d, err := h.driveOwner(r, user, driveID)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	if d.Type != drives.Shared {
		h.k.Respond(w, r, back, users.ErrNotFound, nil)
		return
	}
	if r.FormValue("confirm") != d.Name {
		h.k.Respond(w, r, back, errConfirmName, nil)
		return
	}
	cctx, cancel := longCtx(r)
	defer cancel()
	// Refunds first — the version rows die in the purge.
	if refunds, err := h.k.Nodes.DriveUsage(cctx, driveID); err == nil {
		for username, bytes := range refunds {
			_ = h.k.Users.ChargeQuota(cctx, username, -bytes, 0)
		}
	}
	if err := h.k.Nodes.PurgeDriveData(cctx, driveID); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	if err := h.k.Shares.PurgeDriveSharing(cctx, driveID); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	_ = h.k.Media.PurgeDrive(cctx, driveID)
	if err := h.k.Drives.Delete(cctx, driveID); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "drive.delete", d.Name, "shared drive "+driveID+" deleted")
	h.k.Respond(w, r, "/drive", nil, nil)
}
