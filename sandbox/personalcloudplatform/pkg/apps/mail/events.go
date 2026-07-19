// events.go — the mail SSE bridge (spec §7.5): one long-lived
// /mail/events?box= stream per open page, fed by databox Watch over the
// mailbox's thread indexes. Every thread mutation (new mail, moves,
// flags, label changes) re-files an index row, so one watch covers the
// whole list pane; the browser refetches state + listing on each tick,
// keeping the wire format trivial and the client honest (the drive
// convention).
package mail

import (
	"fmt"
	"net/http"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// events bridges one databox Watch to the browser.
func (h *handlers) events(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	boxes := h.boxes(r, user)
	box, ok := pickBox(boxes, r.URL.Query().Get("box"))
	if !ok {
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
	err = h.k.Mail.WatchThreads(wctx, user.Username, box.ID, func() error {
		if _, werr := fmt.Fprint(w, "event: refresh\ndata: {}\n\n"); werr != nil {
			return werr
		}
		return rc.Flush()
	})
	if err != nil && wctx.Err() == nil {
		h.k.Log.Warn("mail event stream ended", "user", user.Username, "err", err)
	}
}
