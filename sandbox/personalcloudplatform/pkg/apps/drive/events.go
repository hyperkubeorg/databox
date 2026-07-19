// events.go — the SSE bridge: one long-lived /drive/events stream per
// open page, fed by databox Watch. Two modes:
//
//	?drive=&folder=       live folder updates — the browser refetches the
//	                      listing on each tick (another member added/
//	                      renamed/deleted something in the open folder)
//	?doc=1&drive=&node=   collaborative-document ops — each op-log append
//	                      is forwarded as an "op" event to open editors
//
// Stream accounting rides the kernel Hub's per-user cap; the handshake
// clears the server's deadlines for THIS connection only
// (kernel.StartSSE).
package drive

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// events bridges one databox Watch to the browser.
func (h *handlers) events(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	driveID := r.URL.Query().Get("drive")
	docMode := r.URL.Query().Get("doc") == "1"
	target := r.URL.Query().Get("folder")
	if docMode {
		target = r.URL.Query().Get("node")
	}
	if _, err := h.access(r, user, driveID, target, drives.RoleViewer); err != nil {
		http.NotFound(w, r)
		return
	}
	if !h.k.SSE.Acquire(user.Username) {
		http.Error(w, "too many live streams", http.StatusTooManyRequests)
		return
	}
	defer h.k.SSE.Release(user.Username)

	rc, err := kernel.StartSSE(w)
	if err != nil {
		return
	}
	// The request's own context ends the watch when the tab closes.
	wctx := r.Context()
	if docMode {
		err = h.k.Collab.WatchOps(wctx, driveID, target, func(opID string, value []byte) error {
			payload, _ := json.Marshal(map[string]any{"id": opID, "op": json.RawMessage(value)})
			if _, werr := fmt.Fprintf(w, "event: op\ndata: %s\n\n", payload); werr != nil {
				return werr
			}
			return rc.Flush()
		})
	} else {
		err = h.k.Nodes.WatchFolder(wctx, driveID, target, func() error {
			if _, werr := fmt.Fprint(w, "event: refresh\ndata: {}\n\n"); werr != nil {
				return werr
			}
			return rc.Flush()
		})
	}
	if err != nil && wctx.Err() == nil {
		h.k.Log.Warn("event stream ended", "user", user.Username, "mode", map[bool]string{true: "doc", false: "folder"}[docMode], "err", err)
	}
}
