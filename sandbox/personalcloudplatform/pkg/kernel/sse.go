// sse.go — the live-event hub skeleton. Phase 1 ships the per-user
// stream accounting and the SSE handshake helper; the databox
// Watch→event bridges arrive with their features (folder updates in
// phase 2, mail in phase 3/4).
package kernel

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// maxSSEPerUser caps concurrent event streams per member so one browser
// (or script) can't pin unbounded goroutines.
const maxSSEPerUser = 12

// Hub counts live streams per username, enforcing the cap.
type Hub struct {
	mu   sync.Mutex
	open map[string]int
	max  int
}

// NewHub builds a hub allowing maxPerUser concurrent streams per member.
func NewHub(maxPerUser int) *Hub {
	return &Hub{open: map[string]int{}, max: maxPerUser}
}

// Acquire counts a stream in, refusing past the cap. Pair every true
// return with a deferred Release.
func (h *Hub) Acquire(username string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[username] >= h.max {
		return false
	}
	h.open[username]++
	return true
}

// Release counts a stream out.
func (h *Hub) Release(username string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.open[username]--
	if h.open[username] <= 0 {
		delete(h.open, username)
	}
}

// StartSSE performs the SSE handshake: event-stream headers, the
// server's read/write deadlines cleared for this connection (the stream
// is long-lived BY DESIGN; everything else keeps its timeouts), and an
// opening comment flushed so the client's onopen fires. Returns the
// controller the caller flushes after each event.
func StartSSE(w http.ResponseWriter) (*http.ResponseController, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return nil, err
	}
	return rc, rc.Flush()
}
