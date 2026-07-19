// watch.go implements the change-notification hub for one raft group
// (§9.2).
//
// Every mutation the state machine applies publishes an Event here. The
// hub does two jobs:
//
//  1. Fan events out to live subscribers, filtered by key prefix.
//  2. Keep a bounded ring buffer of recent events so a client whose
//     stream dropped can resume with `from_revision` without missing
//     anything — as long as the requested revision is still in the
//     buffer. Older resume points get ErrCompacted and the client is
//     expected to re-list and re-subscribe (the documented contract).
//
// Ordering guarantee: events are published in apply order, so a single
// subscriber sees this group's (= this shard's) changes in order. There is
// deliberately no cross-shard ordering promise.
package kv

import (
	"errors"
	"strings"
	"sync"
)

// Event is one change notification.
type Event struct {
	Rev   uint64 `json:"rev"`
	Type  string `json:"type"` // "put" | "delete"
	Key   string `json:"key"`
	Value []byte `json:"value,omitempty"`
	Blob  bool   `json:"blob,omitempty"`
}

// ErrCompacted is returned when a resume revision has already fallen out
// of the ring buffer. Clients re-list and re-subscribe from now.
var ErrCompacted = errors.New("RevisionCompacted")

// Hub is the per-group event fan-out.
type Hub struct {
	mu   sync.Mutex
	subs map[int]*subscriber
	next int

	// ring holds the most recent events for resume. head is the index
	// of the next write slot; the buffer wraps once full.
	ring  []Event
	head  int
	count int
}

// subscriber is one live watch stream.
type subscriber struct {
	prefix string
	ch     chan Event
}

// NewHub creates a hub with the given resume-buffer capacity.
func NewHub(bufferSize int) *Hub {
	if bufferSize <= 0 {
		bufferSize = 4096
	}
	return &Hub{subs: map[int]*subscriber{}, ring: make([]Event, bufferSize)}
}

// publish records the event and delivers it to matching subscribers.
// Called from the state machine's apply path; it must never block, so
// slow subscribers get events dropped from their private channel and will
// detect the gap via revision numbers and resume.
func (h *Hub) publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring[h.head] = ev
	h.head = (h.head + 1) % len(h.ring)
	if h.count < len(h.ring) {
		h.count++
	}
	for _, sub := range h.subs {
		if strings.HasPrefix(ev.Key, sub.prefix) {
			select {
			case sub.ch <- ev:
			default:
				// Subscriber is not keeping up. Dropping here is safe:
				// the client sees a revision gap and resumes, which
				// replays from the ring buffer.
			}
		}
	}
}

// Subscribe registers a stream for events under prefix, optionally
// replaying history from fromRev (0 = only new events). The returned
// cancel function must be called when the stream ends.
func (h *Hub) Subscribe(prefix string, fromRev uint64) (<-chan Event, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Buffered generously: the API layer drains this into the HTTP
	// response; 1024 in-flight events absorbs bursts.
	ch := make(chan Event, 1024)
	if fromRev > 0 {
		// Replay from the ring. The oldest buffered revision bounds how
		// far back we can resume.
		oldest := h.oldestRevLocked()
		if oldest == 0 || fromRev < oldest {
			return nil, nil, ErrCompacted
		}
		h.replayLocked(prefix, fromRev, ch)
	}
	id := h.next
	h.next++
	h.subs[id] = &subscriber{prefix: prefix, ch: ch}
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, id)
	}
	return ch, cancel, nil
}

// oldestRevLocked returns the smallest revision still in the ring
// (0 when the ring is empty).
func (h *Hub) oldestRevLocked() uint64 {
	if h.count == 0 {
		return 0
	}
	start := (h.head - h.count + len(h.ring)) % len(h.ring)
	return h.ring[start].Rev
}

// replayLocked pushes buffered events with Rev >= fromRev matching prefix
// into ch (bounded by the channel's capacity; overflow means the client
// resumes again, which converges).
func (h *Hub) replayLocked(prefix string, fromRev uint64, ch chan Event) {
	start := (h.head - h.count + len(h.ring)) % len(h.ring)
	for i := 0; i < h.count; i++ {
		ev := h.ring[(start+i)%len(h.ring)]
		if ev.Rev >= fromRev && strings.HasPrefix(ev.Key, prefix) {
			select {
			case ch <- ev:
			default:
				return
			}
		}
	}
}
