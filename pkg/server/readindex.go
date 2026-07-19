// readindex.go — ReadIndex-based linearizable reads (§23).
//
// Historically every KV read was proposed through the shard's raft log: a
// "get" rides an append + quorum round + apply, which makes even a local
// read cost a log write and puts the < 1 ms p99 local-read target out of
// reach. This file replaces that for LOCALLY HOSTED shards with raft's
// ReadIndex protocol:
//
//	barrier: idx ← group.LinearizableRead(ctx)   (one network round, no log write)
//	read:    answer straight from the local state machine (Pebble)
//
// # Why the barrier makes the local read linearizable
//
// ReadIndex asks the group's leader for its current commit index C and the
// leader confirms — via a heartbeat round with a quorum — that it was still
// leader at some instant AFTER the read began. Any write acknowledged to
// any client before this read started was committed before that instant,
// so its log index is ≤ C. LinearizableRead then blocks until this node's
// APPLIED index reaches C (this is also what makes reads safe on a group
// the node only just joined: if the local replica is still catching up,
// the barrier simply waits until apply reaches the read index). Therefore,
// once the barrier returns, the local Pebble state contains every write
// acknowledged before the read's start — reading it yields a state at
// least as fresh as the read's invocation point, which is exactly the
// linearizability obligation. Writes concurrent with the read may or may
// not be visible; linearizability permits either.
//
// Remote shards are unchanged: they still forward through the existing
// /internal/propose RPC, whose handler proposes through the log (the
// handler lives in http.go and serving remote reads via the remote node's
// own barrier needs a new internal endpoint there — see the integration
// note on LinearizableReadMode). Local reads are the common case for a
// smart client, and they are the §23 win.
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.etcd.io/raft/v3"

	"github.com/hyperkubeorg/databox/pkg/kv"
)

// LinearizableReadMode selects how linearizable KV reads execute on shards
// this node hosts:
//
//	"readindex" (default) — ReadIndex barrier + direct state-machine read.
//	"proposal"            — legacy behavior: every read is a raft proposal.
//
// The escape hatch exists so an operator can restore the old read path
// without a rebuild if the ReadIndex path misbehaves. Checked at read time,
// so it can be flipped by tests (and, once wired, by config) at will.
//
// INTEGRATION NOTE (pkg/config, not owned by this change): add a config
// field `linearizable_reads: readindex|proposal` and assign it to this
// variable in Server.Run alongside the MVCC knobs. Unlike those, this is a
// node-local serving choice, not replicated state — nodes may disagree
// without harming correctness (both modes are linearizable).
var LinearizableReadMode = "readindex"

// useReadIndex reports whether the ReadIndex read path is enabled.
func useReadIndex() bool { return LinearizableReadMode != "proposal" }

// readBarrierTimeout bounds one ReadIndex round. Same rationale as the
// proposal timeout in fabric.go: a request handed to a dying leader simply
// vanishes, so waiting longer than an election cycle is pointless — fail
// fast with the retryable error and let the client back off.
const readBarrierTimeout = 5 * time.Second

// readBarrier runs the ReadIndex barrier on a locally hosted group. After
// it returns nil, reading h.sm directly is linearizable (see the file
// header for the full argument).
//
// Error mapping mirrors ProposeToGroup: a barrier that cannot complete
// because the group has no stable leader (election in progress, leader
// transfer mid-read — etcd-raft drops the request or answers
// ErrProposalDropped) becomes the retryable ProposalTimeout error, which
// the API layer maps to 503 + Retry-After and clients retry.
func (s *Server) readBarrier(ctx context.Context, gid uint64, h *groupHandle) error {
	bctx, cancel := context.WithTimeout(ctx, readBarrierTimeout)
	defer cancel()
	_, err := h.group.LinearizableRead(bctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, raft.ErrProposalDropped) || (bctx.Err() != nil && ctx.Err() == nil) {
		return fmt.Errorf("ProposalTimeout: group %d has no stable leader yet, retry", gid)
	}
	return err
}

// localLinearizableGet is the §23 fast path for KVGet on a local shard:
// ReadIndex barrier, then a snapshot-consistent read of the state machine.
// Result semantics match the proposed "get" op exactly: (record, found),
// with storage failures surfacing as the same "Internal" error code the
// state machine would have returned.
func (s *Server) localLinearizableGet(ctx context.Context, gid uint64, h *groupHandle, key string) (kv.Record, bool, error) {
	if err := s.readBarrier(ctx, gid, h); err != nil {
		return kv.Record{}, false, err
	}
	rec, found, _, err := h.sm.GetLocal(key)
	if err != nil {
		return kv.Record{}, false, errors.New("Internal")
	}
	return rec, found, nil
}
