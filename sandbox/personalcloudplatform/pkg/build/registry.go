// Package build is PCP's side of the Builds runtime (Draft 003 §6.2):
// the buildwire listener that accepts runner sessions (buildwire.go), the
// dispatch/ingest loop that pushes jobs and persists what runners report
// (dispatch.go), and the nightly retention worker (cleanup.go). It is the
// twin of pkg/ferry — a databox-lock singleton loop plus a listener — but
// with the transport reversed: runners DIAL in, so this package holds the
// live sessions in a registry the dispatch loop pushes over.
//
// It is distinct from pkg/domain/build (the records/store), imported here
// as dbuild.
package build

import (
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// liveRunner is one connected runner's session and last-reported state.
type liveRunner struct {
	sess     *yamux.Session
	kind     string
	capacity int
	since    time.Time
}

// Registry holds the live buildwire sessions keyed by runner id. The
// listener populates it as runners connect; the dispatch loop reads it to
// find runners with a live session and spare capacity. Safe for
// concurrent use.
type Registry struct {
	mu      sync.Mutex
	runners map[string]*liveRunner
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{runners: map[string]*liveRunner{}}
}

// put registers (or replaces) a runner's live session. A prior session
// for the same id is closed — one control session per runner.
func (r *Registry) put(id string, lr *liveRunner) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.runners[id]; ok && old.sess != lr.sess {
		_ = old.sess.Close()
	}
	r.runners[id] = lr
}

// remove drops a runner's session iff it is still the registered one (a
// reconnect that already replaced it is left alone).
func (r *Registry) remove(id string, sess *yamux.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lr, ok := r.runners[id]; ok && lr.sess == sess {
		delete(r.runners, id)
	}
}

// session returns a runner's live session, or nil when it is not
// connected (or its session has closed).
func (r *Registry) session(id string) *yamux.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	lr, ok := r.runners[id]
	if !ok || lr.sess.IsClosed() {
		return nil
	}
	return lr.sess
}

// capacity reports a runner's last-reported free-slot count and whether
// it is connected.
func (r *Registry) capacity(id string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lr, ok := r.runners[id]
	if !ok || lr.sess.IsClosed() {
		return 0, false
	}
	return lr.capacity, true
}

// setCapacity updates a runner's cached free-slot count (the hello and
// each dispatch adjust it).
func (r *Registry) setCapacity(id string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lr, ok := r.runners[id]; ok {
		lr.capacity = n
	}
}

// Connected reports the ids of runners with a live session (admin console
// / health visibility).
func (r *Registry) Connected() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.runners))
	for id, lr := range r.runners {
		if !lr.sess.IsClosed() {
			out = append(out, id)
		}
	}
	return out
}
