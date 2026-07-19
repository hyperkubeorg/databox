// events.go — the Messenger SSE bridge (Messenger §6). One
// long-lived /messenger/events stream per open page multiplexes several
// databox watches: the user's unread prefix (all badge updates), the
// focused conversation's messages + typing, and presence. Each watch emits
// a typed, bodyless event; the browser refetches the affected JSON (the
// mail/drive convention). The stream doubles as the presence heartbeat.
package messenger

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// events streams live updates to one open page.
func (h *handlers) events(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	if !h.k.SSE.Acquire(user.Username) {
		http.Error(w, "too many live streams", http.StatusTooManyRequests)
		return
	}
	defer h.k.SSE.Release(user.Username)

	rc, err := kernel.StartSSE(w)
	if err != nil {
		return
	}
	ctx := r.Context()
	cid := r.PathValue("cid") // focused conversation (optional path segment)
	stream := fmt.Sprintf("%s-%d", user.Username, time.Now().UnixNano())

	// Presence heartbeat: mark connected now, refresh inside the window,
	// and announce online once so other viewers refetch. Cleared on exit.
	hbCtx, hbCancel := context.WithCancel(context.Background())
	defer hbCancel()
	_ = h.k.Msg.Heartbeat(ctx, user.Username, stream)
	h.announcePresence(user.Username)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = h.k.Msg.Heartbeat(hbCtx, user.Username, stream)
			}
		}
	}()
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = h.k.Msg.ClearHeartbeat(cctx, user.Username, stream)
		cancel()
		h.announcePresence(user.Username)
	}()

	// Fan several watches into one coalescing channel.
	events := make(chan string, 32)
	emit := func(name string) {
		select {
		case events <- name:
		default: // already pending; SSE ticks are idempotent
		}
	}
	go func() { _ = h.k.Msg.WatchUnread(ctx, user.Username, func() error { emit("unread"); return nil }) }()
	go func() { _ = h.k.Msg.WatchPresence(ctx, func() error { emit("presence"); return nil }) }()
	if cid != "" {
		go func() { _ = h.k.Msg.WatchConvo(ctx, cid, func() error { emit("messages"); return nil }) }()
		go func() { _ = h.k.Msg.WatchTyping(ctx, cid, func() error { emit("typing"); return nil }) }()
	}

	// Writer loop with a keepalive so idle proxies don't drop the stream.
	ka := time.NewTicker(25 * time.Second)
	defer ka.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case name := <-events:
			if _, werr := fmt.Fprintf(w, "event: %s\ndata: {}\n\n", name); werr != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		case <-ka.C:
			if _, werr := fmt.Fprint(w, ": ping\n\n"); werr != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
	}
}

// announcePresence bumps the user's presence record so other viewers'
// presence watches fire and refetch the roster (connect/disconnect
// visibility). Best-effort and cheap.
func (h *handlers) announcePresence(user string) {
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := h.k.Msg.GetPresence(cctx, user)
	if err != nil {
		return
	}
	_ = h.k.Msg.SetStatus(cctx, user, p.Chosen, p.StatusMsg)
}
