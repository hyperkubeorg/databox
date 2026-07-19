// snapstream.go — the transport path for streamed (v2) snapshots.
//
// Regular raft messages ride the per-peer message pump (transport.go). A
// v2 MsgSnap cannot: its payload is the whole state machine, generated
// from a pinned Pebble view at send time. So the transport intercepts it
// and moves the data over a dedicated HTTP request instead:
//
//	POST https://<peer>/internal/raftsnap?gid=<group>
//
//	body: [4B BE header len][marshaled raftpb.Message (the MsgSnap)]
//	      followed by the page stream (format: snapshot.go file header)
//
// The request is authenticated like every internal RPC (mTLS + PSK); the
// server mounts SnapshotHandler on the PSK-gated internal subrouter.
//
// Isolation from consensus traffic: the internal HTTP client speaks
// HTTP/1.1 (no h2 upgrade is configured), so this long-running POST gets
// its own TCP connection — a multi-gigabyte transfer never queues behind,
// or starves, raft heartbeats. Each (peer, group) pair allows one transfer
// in flight; raft's snapshot state machine never wants more.
//
// Sender-side flow, per MsgSnap:
//
//	acquire the pinned view at the snapshot's index (refcounted — a
//	concurrent compaction won't close it mid-transfer) → stream header +
//	pages → on 200 report SnapshotFinish to raft, else SnapshotFailure
//	(raft then retries with the current snapshot).
//
// Receiver-side flow (SnapshotHandler):
//
//	parse header → guard against concurrent/stale transfers → clear and
//	repopulate the staging area (snapshot.go) → verify trailer counts →
//	write the "complete" marker (fsync) → hand the MsgSnap to the local
//	group via Deliver → 200. The group's Ready cycle then installs the
//	staged data (group.go installStreamedSnapshot).
package raft

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"go.etcd.io/raft/v3/raftpb"

	"github.com/hyperkubeorg/databox/pkg/store"
)

// snapKey identifies one in-flight snapshot transfer (sender side).
type snapKey struct {
	peer uint64
	gid  uint64
}

// RegisterGroup lets the transport resolve a gid to its local group — the
// snapshot sender needs the group's storage (pinned view) and raft node
// (ReportSnapshot). Called by StartGroup.
func (t *Transport) RegisterGroup(g *Group) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.groups == nil {
		t.groups = map[uint64]*Group{}
	}
	t.groups[g.GID] = g
}

// UnregisterGroup forgets a stopped group. Called by Group.Stop.
func (t *Transport) UnregisterGroup(gid uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.groups, gid)
}

// group resolves a registered local group.
func (t *Transport) group(gid uint64) (*Group, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	g, ok := t.groups[gid]
	return g, ok
}

// sendSnapshot streams one v2 snapshot to a peer. Runs in its own
// goroutine; the per-(peer,gid) inflight guard was taken by the caller.
func (t *Transport) sendSnapshot(gid uint64, m raftpb.Message) {
	defer func() {
		t.mu.Lock()
		delete(t.snapInflight, snapKey{m.To, gid})
		t.mu.Unlock()
	}()
	g, ok := t.group(gid)
	if !ok {
		t.logger.Warn("snapshot send for unknown group", "gid", gid)
		return
	}
	report := func(ok bool) { g.ReportSnapshotStatus(m.To, ok) }

	// Pin the view for the duration of the transfer. Fails if a newer
	// compaction superseded this snapshot or a restart lost the view —
	// raft will retry with the current snapshot.
	view, sections, release, err := g.storage.AcquireView(m.Snapshot.Metadata.Index)
	if err != nil {
		t.logger.Warn("snapshot view unavailable", "gid", gid, "peer", m.To, "err", err)
		report(false)
		return
	}
	defer release()

	t.mu.RLock()
	addr := t.peers[m.To]
	t.mu.RUnlock()
	if addr == "" {
		report(false)
		return
	}

	// Stream through a pipe so the request body is produced page by page —
	// memory stays O(page) no matter the shard size.
	pr, pw := io.Pipe()
	go func() {
		hdr, err := m.Marshal()
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		var lenb [4]byte
		binary.BigEndian.PutUint32(lenb[:], uint32(len(hdr)))
		if _, err := pw.Write(lenb[:]); err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := pw.Write(hdr); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(writeSnapshotPages(pw, view, sections))
	}()

	url := fmt.Sprintf("https://%s/internal/raftsnap?gid=%d", addr, gid)
	req, err := http.NewRequest(http.MethodPost, url, pr)
	if err != nil {
		pr.Close()
		report(false)
		return
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(PSKHeader, t.psk)
	resp, err := t.client.Do(req)
	if err != nil {
		t.logger.Warn("snapshot send failed", "gid", gid, "peer", m.To, "err", err)
		t.reportUnreachable(gid, m.To)
		report(false)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.logger.Warn("snapshot rejected by peer", "gid", gid, "peer", m.To, "status", resp.Status)
		report(false)
		return
	}
	t.logger.Info("snapshot streamed to peer", "gid", gid, "peer", m.To,
		"index", m.Snapshot.Metadata.Index)
	report(true)
}

// SnapshotHandler returns the HTTP handler for inbound snapshot streams,
// mounted by the server at /internal/raftsnap (PSK middleware runs first,
// same as /internal/raft). Requires SetStore to have been called.
func (t *Transport) SnapshotHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		gid, err := strconv.ParseUint(r.URL.Query().Get("gid"), 10, 64)
		if err != nil {
			http.Error(w, "bad gid", http.StatusBadRequest)
			return
		}
		if t.store == nil || t.Deliver == nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		if err := t.receiveSnapshot(gid, r.Body); err != nil {
			t.logger.Warn("snapshot receive failed", "gid", gid, "err", err)
			// 409 tells the sender this transfer is stale or already in
			// progress; anything else is a transient server-side failure.
			// Either way raft retries via ReportSnapshot(failure).
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// SetStore hands the transport the node's Pebble store, used to stage
// inbound snapshot streams. Called once by the server during startup,
// right after NewTransport.
func (t *Transport) SetStore(st *store.Store) { t.store = st }

// receiveSnapshot consumes one snapshot stream: header, staging, marker,
// delivery. See the file header for the protocol.
func (t *Transport) receiveSnapshot(gid uint64, body io.Reader) error {
	// Header: the MsgSnap that raft on this node must eventually Step.
	var lenb [4]byte
	if _, err := io.ReadFull(body, lenb[:]); err != nil {
		return fmt.Errorf("read header length: %w", err)
	}
	hlen := binary.BigEndian.Uint32(lenb[:])
	if hlen == 0 || hlen > 16<<20 {
		return fmt.Errorf("implausible header length %d", hlen)
	}
	hdr := make([]byte, hlen)
	if _, err := io.ReadFull(body, hdr); err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	var msg raftpb.Message
	if err := msg.Unmarshal(hdr); err != nil {
		return fmt.Errorf("bad header message: %w", err)
	}
	if msg.Type != raftpb.MsgSnap || msg.Snapshot == nil {
		return fmt.Errorf("header is not a snapshot message")
	}
	man, err := decodeManifest(msg.Snapshot.Data)
	if err != nil {
		return err
	}
	if man.GID != gid || man.Index != msg.Snapshot.Metadata.Index {
		return fmt.Errorf("manifest (gid %d index %d) does not match message (gid %d index %d)",
			man.GID, man.Index, gid, msg.Snapshot.Metadata.Index)
	}

	// One transfer per group at a time, and never while an install of an
	// equal-or-newer staged snapshot is pending — clearing staging under a
	// running install would tear the state machine.
	t.mu.Lock()
	if t.snapReceiving == nil {
		t.snapReceiving = map[uint64]bool{}
	}
	if t.snapReceiving[gid] {
		t.mu.Unlock()
		return fmt.Errorf("another snapshot transfer for group %d is in progress", gid)
	}
	t.snapReceiving[gid] = true
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.snapReceiving, gid)
		t.mu.Unlock()
	}()

	mu := snapLockFor(t.store, gid)
	mu.Lock()
	if m, ok, err := readMarker(t.store, gid); err != nil {
		mu.Unlock()
		return err
	} else if ok {
		switch {
		case m.State == markerInstalling:
			mu.Unlock()
			return fmt.Errorf("group %d is installing a staged snapshot", gid)
		case m.State == markerComplete && man.Index < m.Index:
			mu.Unlock()
			return fmt.Errorf("staged snapshot at index %d is newer than offered %d", m.Index, man.Index)
		}
		// Equal or newer: replace the staged snapshot below.
	}
	if err := clearStaging(t.store, gid); err != nil {
		mu.Unlock()
		return err
	}
	mu.Unlock()

	// Bulk transfer into staging. No lock held: this transfer is the only
	// staging writer (snapReceiving gate above).
	counts, err := stageSnapshotPages(t.store, gid, len(man.Sections), body)
	if err != nil {
		return err
	}

	// Commit point: the synced marker makes the staged snapshot durable
	// and eligible for install (or for resume-after-crash).
	mu.Lock()
	err = writeMarker(t.store, gid, installMarker{
		State:    markerComplete,
		GID:      gid,
		Index:    man.Index,
		Term:     man.Term,
		Conf:     msg.Snapshot.Metadata.ConfState,
		Sections: man.Sections,
		Counts:   counts,
	})
	mu.Unlock()
	if err != nil {
		return err
	}

	// Hand the MsgSnap to the group (lazily created by the server's
	// Deliver if this node has never heard of the gid — the staged data
	// is already durable, so the install that follows will find it).
	t.Deliver(gid, msg)
	return nil
}
