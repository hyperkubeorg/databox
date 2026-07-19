// replicas.go — PCP observes itself (spec §11.3): every app replica
// heartbeats /pcp/system/replicas/<id> so the admin Workers page can
// show who is actually serving. Lazy TTL: readers mark records stale
// past the heartbeat window and the heartbeat loop prunes records dead
// for over a day (a scaled-down deploy shouldn't haunt the console).
package system

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// replicasPrefix is this file's key family (kvx key table).
const replicasPrefix = "/pcp/system/replicas/"

// Heartbeat cadence and liveness bounds.
const (
	HeartbeatEvery = 30 * time.Second
	// ReplicaStaleAfter is when a replica reads as gone (4 missed beats).
	ReplicaStaleAfter = 2 * time.Minute
	// replicaPruneAfter is when a dead record is deleted outright.
	replicaPruneAfter = 24 * time.Hour
)

// Replica is one running pcp process.
type Replica struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	SeenAt    time.Time `json:"seen_at"`
}

// Stale reports whether the replica has missed enough heartbeats to
// read as gone.
func (r Replica) Stale(now time.Time) bool {
	return now.Sub(r.SeenAt) > ReplicaStaleAfter
}

// RunReplicaHeartbeat writes this process's replica record every
// HeartbeatEvery until ctx ends, pruning long-dead siblings as it goes.
// Call it once from main; the record dies with the lazy TTL when the
// process stops.
func (s *Store) RunReplicaHeartbeat(ctx context.Context) {
	host, _ := os.Hostname()
	rep := Replica{ID: kvx.NewID(), Host: host, PID: os.Getpid(), StartedAt: time.Now().UTC()}
	beat := func() {
		bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		rep.SeenAt = time.Now().UTC()
		_ = kvx.SetJSON(bctx, s.DB, replicasPrefix+rep.ID, rep)
		// Prune siblings dead for over a day (best-effort, cheap: the
		// replica set is small by nature).
		reps, err := s.Replicas(bctx)
		if err != nil {
			return
		}
		for _, r := range reps {
			if time.Since(r.SeenAt) > replicaPruneAfter && kvx.ValidID(r.ID) {
				_ = s.DB.Delete(bctx, replicasPrefix+r.ID)
			}
		}
	}
	beat()
	t := time.NewTicker(HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Best-effort goodbye so the console doesn't show a stale row
			// for the whole TTL after a clean shutdown.
			dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.DB.Delete(dctx, replicasPrefix+rep.ID)
			cancel()
			return
		case <-t.C:
			beat()
		}
	}
}

// Replicas lists every known replica record (stale ones included — the
// caller renders staleness; the health worker raises on it).
func (s *Store) Replicas(ctx context.Context) ([]Replica, error) {
	var out []Replica
	err := kvx.ScanPrefix(ctx, s.DB, replicasPrefix, func(_ string, value []byte) error {
		var r Replica
		if json.Unmarshal(value, &r) == nil {
			out = append(out, r)
		}
		return nil
	})
	return out, err
}
