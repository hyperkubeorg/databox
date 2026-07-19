// cluster_test.go covers the pieces of the cluster package with subtle
// correctness requirements: linearizable counter allocation (NextID) and
// the admin pause flags.
package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/hyperkubeorg/databox/pkg/kv"
)

// fakeFabric is an in-memory metadata group: it applies set/delete and
// tx_apply with the same revision/CAS semantics as the real state machine,
// which is exactly what NextID depends on. Data groups (ProposeToGroup with
// gid != MetaGID) get their own keyspaces plus the kv.SM split-op semantics
// (list_range / freeze_range / split_cleanup / copy_in) the split-execution
// tests exercise.
type fakeFabric struct {
	mu   sync.Mutex
	rev  uint64
	data map[string]kv.Record
	// afterGet, when set, runs after each MetaGet — the hook tests use to
	// interleave a competing writer between read and CAS.
	afterGet func()
	// splitBytes / splitQPS are the configurable split thresholds the
	// trigger tests tune per scenario.
	splitBytes int64
	splitQPS   float64
	// groups holds per-data-group replicated state, mirroring what one
	// kv.SM would hold: latest records plus the active split freeze.
	groups map[uint64]*fakeGroup
	// groupOps logs every data-group op as "<type>@<gid>" in proposal
	// order, so tests can assert protocol ordering (freeze before copy,
	// cleanup after flip).
	groupOps []string
	// confChanges / transfers log membership changes and leadership
	// transfers ("add@gid:node", "learner@gid:node", "remove@gid:node",
	// "gid:from->to") for the placement and balancing tests.
	confChanges []string
	transfers   []string
	// localID is what LocalNodeID reports (defaults to 1).
	localID uint64
}

// fakeGroup is one data group's state in the fake.
type fakeGroup struct {
	data   map[string]kv.Record
	freeze *struct{ start, end string }
}

func newFakeFabric() *fakeFabric {
	return &fakeFabric{data: map[string]kv.Record{}, splitBytes: 16 << 30,
		groups: map[uint64]*fakeGroup{}}
}

// group returns (creating on demand) a data group's state.
func (f *fakeFabric) group(gid uint64) *fakeGroup {
	g, ok := f.groups[gid]
	if !ok {
		g = &fakeGroup{data: map[string]kv.Record{}}
		f.groups[gid] = g
	}
	return g
}

func (f *fakeFabric) IsMetaLeader() bool { return true }

func (f *fakeFabric) MetaGet(key string) (kv.Record, bool, error) {
	f.mu.Lock()
	rec, ok := f.data[key]
	f.mu.Unlock()
	if f.afterGet != nil {
		f.afterGet()
	}
	return rec, ok, nil
}

func (f *fakeFabric) MetaList(prefix string, limit int) ([]kv.ListEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []kv.ListEntry
	for k, rec := range f.data {
		if strings.HasPrefix(k, prefix) && len(out) < limit {
			out = append(out, kv.ListEntry{Key: k, Record: rec})
		}
	}
	return out, nil
}

func (f *fakeFabric) MetaPropose(_ context.Context, op kv.Op) (kv.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch op.Type {
	case "set":
		f.rev++
		f.data[op.Key] = kv.Record{Rev: f.rev, Value: op.Value}
		return kv.Result{Rev: f.rev}, nil
	case "delete":
		f.rev++
		delete(f.data, op.Key)
		return kv.Result{Rev: f.rev}, nil
	case "tx_apply":
		// Validate the read set: missing keys are revision 0.
		for key, seen := range op.Reads {
			cur := uint64(0)
			if rec, ok := f.data[key]; ok {
				cur = rec.Rev
			}
			if cur != seen {
				return kv.Result{Err: kv.ErrConflict, Conflict: key}, nil
			}
		}
		f.rev++
		for _, w := range op.Writes {
			if w.Delete {
				delete(f.data, w.Key)
				continue
			}
			f.data[w.Key] = kv.Record{Rev: f.rev, Value: w.Value}
		}
		return kv.Result{Rev: f.rev}, nil
	}
	return kv.Result{Err: "UnknownOp"}, nil
}

// ProposeToGroup routes metadata proposals to MetaPropose and applies data
// -group ops against the group's fake state with kv.SM's semantics — enough
// fidelity for the split reconciler's freeze/copy/flip/cleanup protocol.
func (f *fakeFabric) ProposeToGroup(ctx context.Context, gid uint64, op kv.Op) (kv.Result, error) {
	if gid == MetaGID {
		return f.MetaPropose(ctx, op)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groupOps = append(f.groupOps, fmt.Sprintf("%s@%d", op.Type, gid))
	g := f.group(gid)
	inRange := func(k, start, end string) bool { return k >= start && (end == "" || k < end) }
	switch op.Type {
	case "set":
		if g.freeze != nil && inRange(op.Key, g.freeze.start, g.freeze.end) {
			return kv.Result{Err: kv.ErrShardSplitting}, nil
		}
		f.rev++
		g.data[op.Key] = kv.Record{Rev: f.rev, Value: op.Value}
		return kv.Result{Rev: f.rev}, nil
	case "list_range":
		var keys []string
		for k := range g.data {
			if inRange(k, op.Start, op.End) && (op.Cursor == "" || k > op.Cursor) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		if op.Limit > 0 && len(keys) > op.Limit {
			keys = keys[:op.Limit]
		}
		out := make([]kv.ListEntry, 0, len(keys))
		for _, k := range keys {
			out = append(out, kv.ListEntry{Key: k, Record: g.data[k]})
		}
		return kv.Result{Entries: out}, nil
	case "copy_in":
		for k, rec := range op.Pairs {
			g.data[k] = rec
		}
		return kv.Result{}, nil
	case "freeze_range":
		g.freeze = &struct{ start, end string }{op.Start, op.End}
		return kv.Result{}, nil
	case "split_cleanup":
		if g.freeze != nil && g.freeze.start == op.Start && g.freeze.end == op.End {
			g.freeze = nil
		}
		// End "" = to the end of the keyspace, like freeze_range.
		for k := range g.data {
			if inRange(k, op.Start, op.End) {
				delete(g.data, k)
			}
		}
		return kv.Result{}, nil
	}
	return kv.Result{Err: "UnknownOp"}, nil
}
func (f *fakeFabric) ListGroup(_ uint64, prefix, _ string, limit int) ([]kv.ListEntry, error) {
	return f.MetaList(prefix, limit)
}
func (f *fakeFabric) CreateGroupEverywhere(context.Context, uint64, []uint64) error { return nil }
func (f *fakeFabric) AddMember(_ context.Context, gid, node uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confChanges = append(f.confChanges, fmt.Sprintf("add@%d:%d", gid, node))
	return nil
}
func (f *fakeFabric) RemoveMember(_ context.Context, gid, node uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confChanges = append(f.confChanges, fmt.Sprintf("remove@%d:%d", gid, node))
	return nil
}
func (f *fakeFabric) TransferGroupLeadership(gid, from, to uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transfers = append(f.transfers, fmt.Sprintf("%d:%d->%d", gid, from, to))
}
func (f *fakeFabric) LocalGroupSize(uint64) (uint64, bool) { return 0, false }
func (f *fakeFabric) LocalNodeID() uint64 {
	if f.localID != 0 {
		return f.localID
	}
	return 1
}
func (f *fakeFabric) Replicas() int                                                 { return 3 }
func (f *fakeFabric) SplitThresholdBytes() int64                                    { return f.splitBytes }
func (f *fakeFabric) SplitThresholdQPS() float64                                    { return f.splitQPS }

// Sequential allocations start at `first` and never repeat.
func TestNextIDSequential(t *testing.T) {
	f := newFakeFabric()
	ctx := context.Background()
	for want := uint64(2); want < 7; want++ {
		got, err := NextID(ctx, f, KeyNextNode, 2)
		if err != nil {
			t.Fatalf("NextID: %v", err)
		}
		if got != want {
			t.Fatalf("NextID = %d, want %d", got, want)
		}
	}
}

// A competing allocation between read and CAS must NOT produce a duplicate
// ID — the stale CAS conflicts and NextID retries on the fresh counter.
// This is the meta-leader-failover double-allocation scenario.
func TestNextIDRaceRetries(t *testing.T) {
	f := newFakeFabric()
	ctx := context.Background()
	raced := false
	f.afterGet = func() {
		if raced {
			return
		}
		raced = true
		// Competing allocator sneaks in a full allocation (old read-then-
		// set style) after our read but before our CAS.
		f.mu.Lock()
		next := uint64(2)
		if rec, ok := f.data[KeyNextNode]; ok {
			_ = json.Unmarshal(rec.Value, &next)
		}
		raw, _ := json.Marshal(next + 1)
		f.rev++
		f.data[KeyNextNode] = kv.Record{Rev: f.rev, Value: raw}
		f.mu.Unlock()
	}
	got, err := NextID(ctx, f, KeyNextNode, 2)
	if err != nil {
		t.Fatalf("NextID: %v", err)
	}
	// The interloper took 2; the CAS retry must observe that and take 3.
	if got != 3 {
		t.Fatalf("NextID = %d after race, want 3 (2 was taken concurrently)", got)
	}
}

// Concurrent allocators always receive distinct IDs.
func TestNextIDConcurrentUnique(t *testing.T) {
	f := newFakeFabric()
	ctx := context.Background()
	const n = 32
	ids := make(chan uint64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := NextID(ctx, f, KeyNextGroup, 2)
			if err != nil {
				t.Errorf("NextID: %v", err)
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[uint64]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate ID %d allocated", id)
		}
		seen[id] = true
	}
}

// Paused reflects the stored flag and fails open on absence.
func TestPausedFlag(t *testing.T) {
	f := newFakeFabric()
	if Paused(f, "rebalance") {
		t.Fatal("unset flag must read as not paused")
	}
	raw, _ := json.Marshal(PauseFlag{Paused: true, Actor: "root"})
	if _, err := f.MetaPropose(context.Background(), kv.Op{Type: "set", Key: KeyAdminPause + "rebalance", Value: raw}); err != nil {
		t.Fatal(err)
	}
	if !Paused(f, "rebalance") {
		t.Fatal("set flag must read as paused")
	}
	if Paused(f, "split") {
		t.Fatal("other subsystems unaffected")
	}
}
