// group_test.go runs real raft groups against an in-memory network to pin
// the behaviors the HTTP transport provides in production: replication,
// slow-follower catch-up via streamed snapshots (using the actual
// receive/stage/install code path), concurrent Applied() reads (the -race
// gate), and linearizable reads via ReadIndex.
package raft

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.etcd.io/raft/v3/raftpb"

	"github.com/hyperkubeorg/databox/pkg/kv"
	"github.com/hyperkubeorg/databox/pkg/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// memNet is an in-process cluster: every node's Sender routes messages
// straight into the destination group's Step, and streamed snapshots run
// through the real page-stream encoder and the real Transport receive path
// (receiveSnapshot), skipping only the HTTP layer.
type memNet struct {
	mu          sync.Mutex
	nodes       map[uint64]*memNode
	snapStreams atomic.Int64 // completed streamed snapshot transfers
}

// memNode is one cluster member: its own Pebble store, state machine,
// group, and a receive-side Transport used to stage inbound snapshots.
type memNode struct {
	net  *memNet
	id   uint64
	st   *store.Store
	sm   *kv.SM
	g    *Group
	tr   *Transport  // receive path only (SetStore + Deliver)
	down atomic.Bool // true = partitioned: all traffic to it is dropped
}

// Send implements Sender. Mirrors the production transport's contract:
// best-effort delivery, ReportUnreachable on failure, and snapshot status
// reporting after every MsgSnap attempt.
func (n *memNode) Send(gid uint64, msgs []raftpb.Message) {
	for _, m := range msgs {
		n.net.mu.Lock()
		dst := n.net.nodes[m.To]
		n.net.mu.Unlock()
		if dst == nil {
			continue
		}
		if m.Type == raftpb.MsgSnap && IsStreamedSnapshot(m.Snapshot.Data) {
			if dst.down.Load() {
				// Like a failed POST: raft must learn the transfer died.
				n.g.ReportSnapshotStatus(m.To, false)
				n.g.ReportUnreachable(m.To)
				continue
			}
			go n.net.streamSnapshot(n, dst, gid, m)
			continue
		}
		if dst.down.Load() {
			n.g.ReportUnreachable(m.To)
			continue
		}
		dst.g.Step(m)
	}
}

// streamSnapshot performs one snapshot transfer: sender half identical to
// Transport.sendSnapshot (pinned view → header + pages), receiver half the
// real Transport.receiveSnapshot.
func (net *memNet) streamSnapshot(src, dst *memNode, gid uint64, m raftpb.Message) {
	view, sections, release, err := src.g.storage.AcquireView(m.Snapshot.Metadata.Index)
	if err != nil {
		src.g.ReportSnapshotStatus(m.To, false)
		return
	}
	var body bytes.Buffer
	hdr, err := m.Marshal()
	if err == nil {
		var lenb [4]byte
		binary.BigEndian.PutUint32(lenb[:], uint32(len(hdr)))
		body.Write(lenb[:])
		body.Write(hdr)
		err = writeSnapshotPages(&body, view, sections)
	}
	release()
	if err != nil {
		src.g.ReportSnapshotStatus(m.To, false)
		return
	}
	if err := dst.tr.receiveSnapshot(gid, &body); err != nil {
		src.g.ReportSnapshotStatus(m.To, false)
		return
	}
	net.snapStreams.Add(1)
	src.g.ReportSnapshotStatus(m.To, true)
}

// newCluster boots n nodes into one raft group and returns them.
func newCluster(t *testing.T, gid uint64, n int, snapCount uint64) *memNet {
	t.Helper()
	net := &memNet{nodes: map[uint64]*memNode{}}
	bootstrap := make([]uint64, n)
	for i := range bootstrap {
		bootstrap[i] = uint64(i + 1)
	}
	for _, id := range bootstrap {
		node := &memNode{net: net, id: id}
		node.st = openTestStore(t)
		sm, err := kv.NewSM(gid, node.st, nil, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		node.sm = sm
		node.tr = NewTransport(id, nil, "", testLogger())
		node.tr.SetStore(node.st)
		g, err := StartGroup(GroupConfig{
			GID: gid, NodeID: id, SM: sm, Store: node.st,
			Transport: node, Logger: testLogger(),
			Bootstrap: bootstrap, SnapCount: snapCount,
			TickInterval: 2 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		node.g = g
		node.tr.Deliver = func(gid uint64, msg raftpb.Message) { g.Step(msg) }
		net.mu.Lock()
		net.nodes[id] = node
		net.mu.Unlock()
		t.Cleanup(g.Stop)
	}
	return net
}

// waitLeader polls until some node leads the group.
func (net *memNet) waitLeader(t *testing.T) *memNode {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		net.mu.Lock()
		for _, n := range net.nodes {
			if n.g.IsLeader() {
				net.mu.Unlock()
				return n
			}
		}
		net.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no leader elected within deadline")
	return nil
}

// propose retries a proposal until it commits (elections drop proposals).
func propose(t *testing.T, g *Group, op kv.Op) kv.Result {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		res, err := g.Propose(ctx, op)
		cancel()
		if err == nil {
			if r, ok := res.(kv.Result); ok {
				return r
			}
			t.Fatalf("unexpected result %T", res)
		}
	}
	t.Fatal("proposal never committed")
	return kv.Result{}
}

// TestSnapshotCatchUp: a partitioned follower whose entries were compacted
// away must catch up through a streamed snapshot and converge to a
// byte-identical state machine — MVCC history and cutoff included.
func TestSnapshotCatchUp(t *testing.T) {
	const gid = 3
	net := newCluster(t, gid, 3, 8 /* compact every 8 entries */)
	leader := net.waitLeader(t)

	// Pick a follower and partition it.
	var follower *memNode
	net.mu.Lock()
	for _, n := range net.nodes {
		if n.id != leader.id {
			follower = n
			break
		}
	}
	net.mu.Unlock()
	follower.down.Store(true)

	// Enough traffic that compaction (snapCount 8) truncates everything
	// the follower would need, with overwrites so MVCC history is real.
	for i := 0; i < 60; i++ {
		propose(t, leader.g, kv.Op{
			Type:  "set",
			Key:   fmt.Sprintf("/k-%02d", i%20),
			Value: []byte(fmt.Sprintf("v%d", i)),
		})
	}

	// Heal the partition; the leader finds the follower's entries
	// compacted and streams a snapshot.
	follower.down.Store(false)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && follower.g.Applied() < leader.g.Applied() {
		time.Sleep(10 * time.Millisecond)
	}
	if follower.g.Applied() < leader.g.Applied() {
		t.Fatalf("follower applied %d, leader %d — never caught up",
			follower.g.Applied(), leader.g.Applied())
	}
	if net.snapStreams.Load() == 0 {
		t.Fatal("follower caught up without a streamed snapshot (test lost its premise)")
	}
	// Byte-identical state machines.
	want := dumpRange(t, leader.st, smPrefix(gid))
	got := dumpRange(t, follower.st, smPrefix(gid))
	if len(want) != len(got) {
		t.Fatalf("follower has %d SM keys, leader %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("key %q: follower %q, leader %q", k, got[k], v)
		}
	}
	if follower.sm.Rev() != leader.sm.Rev() {
		t.Fatalf("rev: follower %d, leader %d", follower.sm.Rev(), leader.sm.Rev())
	}
}

// TestAppliedConcurrentAccess is the -race gate for the Applied() fix:
// API goroutines read the applied index while the run loop advances it.
func TestAppliedConcurrentAccess(t *testing.T) {
	const gid = 4
	net := newCluster(t, gid, 1, 0)
	leader := net.waitLeader(t)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var last uint64
			for {
				select {
				case <-stop:
					return
				default:
				}
				a := leader.g.Applied()
				if a < last {
					t.Error("applied index went backwards")
					return
				}
				last = a
				leader.g.ConfState()
			}
		}()
	}
	for i := 0; i < 100; i++ {
		propose(t, leader.g, kv.Op{Type: "set", Key: fmt.Sprintf("/r-%d", i)})
	}
	close(stop)
	wg.Wait()
}

// TestLinearizableRead: ReadIndex must return an index at or beyond every
// previously committed write, and only after the local apply caught up —
// so a local read after it sees that write.
func TestLinearizableRead(t *testing.T) {
	const gid = 5
	net := newCluster(t, gid, 3, 0)
	leader := net.waitLeader(t)

	res := propose(t, leader.g, kv.Op{Type: "set", Key: "/lin", Value: []byte("v1")})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	idx, err := leader.g.LinearizableRead(ctx)
	if err != nil {
		t.Fatalf("LinearizableRead: %v", err)
	}
	if idx < res.Rev {
		t.Fatalf("read index %d is older than committed write at %d", idx, res.Rev)
	}
	if applied := leader.g.Applied(); applied < idx {
		t.Fatalf("returned before applying: applied %d < read index %d", applied, idx)
	}
	if rec, ok, _ := leader.sm.Get("/lin"); !ok || string(rec.Value) != "v1" {
		t.Fatalf("local read after ReadIndex barrier missed the write: %+v ok=%v", rec, ok)
	}

	// Followers serve ReadIndex too (raft forwards to the leader).
	var follower *memNode
	net.mu.Lock()
	for _, n := range net.nodes {
		if n.id != leader.id {
			follower = n
			break
		}
	}
	net.mu.Unlock()
	fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer fcancel()
	fidx, err := follower.g.LinearizableRead(fctx)
	if err != nil {
		t.Fatalf("follower LinearizableRead: %v", err)
	}
	if fidx < res.Rev {
		t.Fatalf("follower read index %d older than committed write at %d", fidx, res.Rev)
	}
	if rec, ok, _ := follower.sm.Get("/lin"); !ok || string(rec.Value) != "v1" {
		t.Fatalf("follower read after barrier missed the write: %+v ok=%v", rec, ok)
	}
}
