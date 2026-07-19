// shares.go — sharing, both kinds:
//
//	POST /drive/share/create /drive/share/revoke   public links (token URLs)
//	POST /drive/share/grant  /drive/share/ungrant  per-user grants
//	GET  /s/{token}                    the public link itself (no auth):
//	                                   a folder share browses read-only,
//	                                   a file share views/downloads
//	GET  /s/{token}/f/{node}           a folder/file inside the share
//	GET  /s/{token}/raw/{node}         bytes (Range-enabled)
//	GET  /s/{token}/zip/{node}         subtree zip (download perm)
//	POST /s/{token}                    password entry → share session
//
// Sub-node addressing is verified by ANCESTRY: every {node} reached
// through a share must sit inside the shared subtree (walk to the shared
// root), so a crafted URL can't reach siblings.
package drive

import (
	"net/http"
	"net/url"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/shares"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// shareCreate mints a public link (editor+ on the node).
func (h *handlers) shareCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID := r.FormValue("drive"), r.FormValue("node")
	back := backTo(r, driveID)
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	var expiresAt time.Time
	if e := r.FormValue("expires"); e != "" {
		d, err := time.ParseDuration(e)
		if err != nil || d <= 0 {
			h.k.Respond(w, r, back, err, nil)
			return
		}
		expiresAt = time.Now().UTC().Add(d)
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sh, err := h.k.Shares.CreateShare(cctx, driveID, nodeID, r.FormValue("perms"), r.FormValue("password"), expiresAt, user.Username)
	h.k.Respond(w, r, back, err, map[string]any{"token": sh.Token, "url": "/s/" + sh.Token})
}

// shareRevoke deletes a link. Allowed for its creator or an editor on
// the node.
func (h *handlers) shareRevoke(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	token := r.FormValue("token")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sh, found, err := h.k.Shares.GetShare(cctx, token)
	back := r.FormValue("back")
	if back == "" || back[0] != '/' {
		back = "/drive"
	}
	if err != nil || !found {
		h.k.Respond(w, r, back, users.ErrNotFound, nil)
		return
	}
	if sh.By != user.Username {
		if _, err := h.access(r, user, sh.DriveID, sh.NodeID, drives.RoleEditor); err != nil {
			h.k.Respond(w, r, back, err, nil)
			return
		}
	}
	h.k.Respond(w, r, back, h.k.Shares.RevokeShare(cctx, token), nil)
}

// shareGrant shares a node with a named user (editor+ on the node).
func (h *handlers) shareGrant(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID := r.FormValue("drive"), r.FormValue("node")
	back := backTo(r, driveID)
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Shares.SetGrant(cctx, driveID, nodeID, r.FormValue("user"), r.FormValue("role"), user.Username)
	h.k.Respond(w, r, back, err, nil)
}

// shareUngrant removes a person share (editor+ on the node).
func (h *handlers) shareUngrant(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID := r.FormValue("drive"), r.FormValue("node")
	back := backTo(r, driveID)
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	h.k.Respond(w, r, back, h.k.Shares.RemoveGrant(cctx, driveID, nodeID, r.FormValue("user")), nil)
}

// --- the public link ---------------------------------------------------------------

// resolveShare loads a live share: expiry checked, password satisfied
// (via the share-session cookie). needPw=true means "render the password
// form".
func (h *handlers) resolveShare(r *http.Request, token string) (shares.Share, bool, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sh, found, err := h.k.Shares.GetShare(cctx, token)
	if err != nil || !found || sh.Expired(time.Now()) {
		return shares.Share{}, false, false
	}
	if sh.PwHash != "" {
		c, err := r.Cookie("pcp_share_" + token)
		if err != nil || !h.k.Shares.CheckShareSession(cctx, c.Value, token) {
			return sh, true, true
		}
	}
	return sh, true, false
}

// shareNode resolves {node} inside a share's subtree, verifying
// ancestry. An empty {node} means the shared node itself.
func (h *handlers) shareNode(r *http.Request, sh shares.Share, nodeID string) (nodes.Node, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if nodeID == "" || nodeID == sh.NodeID {
		n, found, err := h.k.Nodes.GetByID(cctx, sh.DriveID, sh.NodeID)
		return n, err == nil && found
	}
	crumbs, err := h.k.Nodes.Path(cctx, sh.DriveID, nodeID)
	if err != nil {
		return nodes.Node{}, false
	}
	inside := false
	for _, c := range crumbs {
		if c.ID == sh.NodeID {
			inside = true
			break
		}
	}
	if !inside {
		return nodes.Node{}, false
	}
	n, found, err := h.k.Nodes.GetByID(cctx, sh.DriveID, nodeID)
	return n, err == nil && found
}

// SharePage is the public link page's typed struct. It renders WITHOUT
// the app chrome (no session): password gate, read-only folder browsing,
// or a single file's view/download.
type SharePage struct {
	Token    string
	Share    shares.Share
	Node     nodes.Node
	Kind     string
	Children []ShareChildVM
	Crumbs   []nodes.Crumb // within the share only
	NeedPw   bool
	CanDL    bool
	RawURL   string
	SiteName string
	Error    string
}

// ShareChildVM is one row of a shared folder's listing.
type ShareChildVM struct {
	nodes.Node
	Kind    string
	OpenURL string
}

// sharePage renders a share: password gate, folder listing, or file
// view.
func (h *handlers) sharePage(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	sh, found, needPw := h.resolveShare(r, token)
	if !found {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, _ := h.k.Site.Get(cctx)
	pg := SharePage{Token: token, Share: sh, NeedPw: needPw, SiteName: sc.Name,
		CanDL: sh.Perms == shares.PermDownload, Error: r.URL.Query().Get("err")}
	if needPw {
		ui.Render(w, h.views, "share", pg)
		return
	}
	node, ok := h.shareNode(r, sh, r.PathValue("node"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	pg.Node = node
	pg.Kind = FileKind(node.Name, node.ContentType, node.IsDir)
	pg.RawURL = "/s/" + token + "/raw/" + node.ID
	if node.IsDir {
		children, err := h.k.Nodes.ListFolder(cctx, sh.DriveID, node.ID)
		if err != nil {
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}
		for _, c := range children {
			pg.Children = append(pg.Children, ShareChildVM{
				Node: c, Kind: FileKind(c.Name, c.ContentType, c.IsDir),
				OpenURL: "/s/" + token + "/f/" + c.ID,
			})
		}
		// Crumbs from the shared root down to here.
		if crumbs, err := h.k.Nodes.Path(cctx, sh.DriveID, node.ID); err == nil {
			keep := false
			for _, c := range crumbs {
				if c.ID == sh.NodeID {
					keep = true
				}
				if keep {
					pg.Crumbs = append(pg.Crumbs, c)
				}
			}
		}
	}
	ui.Render(w, h.views, "share", pg)
}

// sharePassword checks a typed password and mints the share session.
func (h *handlers) sharePassword(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sh, found, err := h.k.Shares.GetShare(cctx, token)
	if err != nil || !found || sh.Expired(time.Now()) {
		http.NotFound(w, r)
		return
	}
	if !shares.CheckSharePassword(sh, r.FormValue("password")) {
		http.Redirect(w, r, "/s/"+token+"?err="+url.QueryEscape("wrong password"), http.StatusSeeOther)
		return
	}
	id, err := h.k.Shares.CreateShareSession(cctx, token)
	if err != nil {
		http.Error(w, "try again", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "pcp_share_" + token, Value: id, Path: "/s/" + token,
		HttpOnly: true, Secure: h.k.SecureCookies, SameSite: http.SameSiteLaxMode,
		MaxAge: 3600,
	})
	http.Redirect(w, r, "/s/"+token, http.StatusSeeOther)
}

// shareRaw serves bytes through a share (Range-enabled). View-only
// shares stream inline media but never attachment downloads.
func (h *handlers) shareRaw(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	sh, found, needPw := h.resolveShare(r, token)
	if !found || needPw {
		http.NotFound(w, r)
		return
	}
	node, ok := h.shareNode(r, sh, r.PathValue("node"))
	if !ok || node.IsDir {
		http.NotFound(w, r)
		return
	}
	inline := r.URL.Query().Get("inline") == "1"
	if sh.Perms != shares.PermDownload {
		inline = true // view-only: media renders, nothing attaches
	}
	h.serveBlob(w, r, nodes.BlobKey(sh.DriveID, node.BlobID), node.BlobID, node.Name, node.ContentType, node.Size, inline)
}

// shareZip streams a shared folder as a zip (download perm only).
func (h *handlers) shareZip(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	sh, found, needPw := h.resolveShare(r, token)
	if !found || needPw || sh.Perms != shares.PermDownload {
		http.NotFound(w, r)
		return
	}
	node, ok := h.shareNode(r, sh, r.PathValue("node"))
	if !ok || !node.IsDir {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	name := node.Name
	if name == "" {
		name = "shared"
	}
	h.streamZip(w, r, name+".zip", sh.DriveID, func(add func(string, nodes.Node) error) error {
		return h.k.Nodes.WalkSubtree(cctx, sh.DriveID, node.ID, add)
	})
}
