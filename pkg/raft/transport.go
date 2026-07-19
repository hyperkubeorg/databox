// transport.go delivers raft messages between nodes.
//
// Databox's internal RPC rides the same HTTPS server as everything else
// (§6.1): a raft message is an HTTP POST of the marshaled
// raftpb.Message to
//
//	POST https://<peer>/internal/raft?gid=<group>
//
// authenticated by mTLS (cluster CA) plus the node PSK in a header. There
// is no protobuf *toolchain* dependency — raftpb ships pre-generated
// marshaling code inside the etcd-raft module.
//
// Delivery is best-effort by design: raft tolerates dropped messages and
// simply retries, so a failed POST is reported to raft (ReportUnreachable)
// and forgotten. Each peer gets a small send queue so one slow peer cannot
// stall the others.
//
// Snapshot messages are the exception: a v2 MsgSnap carries only a small
// manifest, and its bulk data is streamed over a dedicated request instead
// of the message pump — see snapstream.go.
package raft

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"

	"go.etcd.io/raft/v3/raftpb"

	"github.com/hyperkubeorg/databox/pkg/store"
)

// PSKHeader carries the node pre-shared key on every internal RPC request.
const PSKHeader = "X-Databox-PSK"

// Transport fans raft messages out to peers and hands received ones to the
// right local group. One Transport serves every group on the node.
type Transport struct {
	mu    sync.RWMutex
	peers map[uint64]string        // nodeID → advertise address ("host:port")
	queue map[uint64]chan outbound // nodeID → buffered send queue

	client *http.Client // TLS client trusted against the cluster CA
	psk    string       // primary PSK attached to outgoing requests
	self   uint64       // this node's ID, to drop accidental self-sends

	// Deliver routes an inbound message to the local group instance.
	// Set by the server once groups exist.
	Deliver func(gid uint64, msg raftpb.Message)

	// Snapshot streaming state (snapstream.go), all guarded by mu:
	// local groups by gid (snapshot page source + status reporting),
	// per-(peer,gid) outbound transfers, per-gid inbound transfers.
	groups        map[uint64]*Group
	snapInflight  map[snapKey]bool
	snapReceiving map[uint64]bool
	// store is where inbound snapshot streams are staged; set by the
	// server via SetStore during startup.
	store *store.Store

	logger *slog.Logger
}

// outbound pairs a message with its destination group.
type outbound struct {
	gid uint64
	msg raftpb.Message
}

// NewTransport builds a transport. The HTTP client must already be
// configured with the cluster's TLS trust (done in pkg/server).
func NewTransport(self uint64, client *http.Client, psk string, logger *slog.Logger) *Transport {
	return &Transport{
		peers:  map[uint64]string{},
		queue:  map[uint64]chan outbound{},
		client: client,
		psk:    psk,
		self:   self,
		logger: logger,
	}
}

// SetPeer records (or updates) a peer's address and ensures its send loop
// is running. Called whenever cluster topology changes.
func (t *Transport) SetPeer(id uint64, addr string) {
	if id == t.self {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.peers[id] = addr
	if _, ok := t.queue[id]; !ok {
		// A queue of 4096 messages absorbs bursts (heartbeats + appends
		// for many groups); overflow is dropped, which raft handles.
		q := make(chan outbound, 4096)
		t.queue[id] = q
		go t.sendLoop(id, q)
	}
}

// RemovePeer forgets a peer after decommission.
func (t *Transport) RemovePeer(id uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.peers, id)
	if q, ok := t.queue[id]; ok {
		close(q)
		delete(t.queue, id)
	}
}

// Peers returns a snapshot of the known peer map (for status endpoints).
func (t *Transport) Peers() map[uint64]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[uint64]string, len(t.peers))
	for k, v := range t.peers {
		out[k] = v
	}
	return out
}

// Send enqueues messages produced by a group's Ready cycle. Streamed (v2)
// snapshot messages bypass the queue: their bulk data moves over a
// dedicated request so it can never starve heartbeats (snapstream.go).
func (t *Transport) Send(gid uint64, msgs []raftpb.Message) {
	for _, m := range msgs {
		if m.To == t.self || m.To == 0 {
			continue
		}
		if m.Type == raftpb.MsgSnap && m.Snapshot != nil && IsStreamedSnapshot(m.Snapshot.Data) {
			// One transfer per (peer, group); raft never wants more.
			t.mu.Lock()
			if t.snapInflight == nil {
				t.snapInflight = map[snapKey]bool{}
			}
			key := snapKey{m.To, gid}
			if t.snapInflight[key] {
				t.mu.Unlock()
				continue
			}
			t.snapInflight[key] = true
			t.mu.Unlock()
			go t.sendSnapshot(gid, m)
			continue
		}
		t.mu.RLock()
		q, ok := t.queue[m.To]
		t.mu.RUnlock()
		if !ok {
			continue // unknown peer; topology update will fix this
		}
		select {
		case q <- outbound{gid: gid, msg: m}:
		default:
			// Queue full: drop. Raft's retry machinery recovers, and
			// dropping beats blocking the consensus loop.
		}
	}
}

// sendLoop drains one peer's queue, POSTing each message.
func (t *Transport) sendLoop(id uint64, q chan outbound) {
	for ob := range q {
		t.mu.RLock()
		addr := t.peers[id]
		t.mu.RUnlock()
		if addr == "" {
			continue
		}
		if err := t.post(addr, ob.gid, ob.msg); err != nil {
			// Unreachable peers are normal during restarts and network
			// blips; log at debug level and inform raft.
			t.logger.Debug("raft send failed", "peer", id, "addr", addr, "err", err)
			if t.Deliver != nil {
				// Feed an unreachable report back through the local
				// group so raft backs off for this follower.
				t.reportUnreachable(ob.gid, id)
			}
		}
	}
}

// reportUnreachable is wired to Group.ReportUnreachable by the server.
var reportUnreachableHook func(gid, peer uint64)

// SetUnreachableHook installs the callback used to tell a local group that
// a peer could not be reached.
func SetUnreachableHook(f func(gid, peer uint64)) { reportUnreachableHook = f }

func (t *Transport) reportUnreachable(gid, peer uint64) {
	if reportUnreachableHook != nil {
		reportUnreachableHook(gid, peer)
	}
}

// post performs the actual HTTPS request for one message.
func (t *Transport) post(addr string, gid uint64, msg raftpb.Message) error {
	raw, err := msg.Marshal()
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://%s/internal/raft?gid=%d", addr, gid)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(PSKHeader, t.psk)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) // drain for connection reuse
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer returned %s", resp.Status)
	}
	return nil
}

// Handler returns the HTTP handler for inbound raft messages, mounted at
// /internal/raft by the server. PSK verification happens in the server's
// internal-RPC middleware before this handler runs.
func (t *Transport) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		gid, err := strconv.ParseUint(r.URL.Query().Get("gid"), 10, 64)
		if err != nil {
			http.Error(w, "bad gid", http.StatusBadRequest)
			return
		}
		// 512 MiB cap: regular raft messages are ≤1 MiB; only legacy v1
		// snapshots (pre-streaming upgrades) can be large here — v2
		// snapshots move their bulk over /internal/raftsnap instead.
		body, err := io.ReadAll(io.LimitReader(r.Body, 512<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var msg raftpb.Message
		if err := msg.Unmarshal(body); err != nil {
			http.Error(w, "bad message", http.StatusBadRequest)
			return
		}
		if t.Deliver == nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		t.Deliver(gid, msg)
		w.WriteHeader(http.StatusOK)
	}
}
