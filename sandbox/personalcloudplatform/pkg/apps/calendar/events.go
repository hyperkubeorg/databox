// events.go — the calendar's live-update stream: ONE SSE connection
// fanning in the member's visible calendars. The client passes the
// calendars it renders (?cals=drive/node,...); each is access-checked,
// then a databox Watch on its op-log prefix forwards a "refresh" event
// naming the calendar — the view refetches the range. One connection
// spends one slot of the kernel Hub's per-user cap no matter how many
// calendars ride it.
package calendar

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// maxWatchedCals bounds one stream's watch fan-out.
const maxWatchedCals = 16

// events bridges the visible calendars' op-logs to the browser.
func (h *handlers) events(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	var targets [][2]string // (drive, node), access-checked
	for _, raw := range strings.Split(r.URL.Query().Get("cals"), ",") {
		driveID, nodeID, ok := strings.Cut(strings.TrimSpace(raw), "/")
		if !ok {
			continue
		}
		if err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
			continue // silently skip what the member can't see
		}
		targets = append(targets, [2]string{driveID, nodeID})
		if len(targets) >= maxWatchedCals {
			break
		}
	}
	if len(targets) == 0 {
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
	wctx := r.Context()
	var mu sync.Mutex // one writer at a time across the watch goroutines
	send := func(driveID, nodeID string) error {
		mu.Lock()
		defer mu.Unlock()
		if _, werr := fmt.Fprintf(w, "event: refresh\ndata: {\"cal\":%q}\n\n", driveID+"/"+nodeID); werr != nil {
			return werr
		}
		return rc.Flush()
	}
	var wg sync.WaitGroup
	for _, t := range targets {
		driveID, nodeID := t[0], t[1]
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := h.k.Collab.WatchOps(wctx, driveID, nodeID, func(string, []byte) error {
				return send(driveID, nodeID)
			})
			if err != nil && wctx.Err() == nil {
				h.k.Log.Warn("calendar event stream ended", "user", user.Username, "cal", nodeID, "err", err)
			}
		}()
	}
	wg.Wait()
}
