// tx.go is the distributed transaction coordinator (§10).
//
// The client-visible model is FoundationDB-style optimistic concurrency:
// the client reads freely (each Get response carries the key's revision),
// accumulates a read set {key → revision} and a write set, and submits
// both in one commit call. No locks are held while the client thinks.
//
// Commit protocol, chosen by how many shards the transaction touches:
//
//	1 shard  → a single "tx_apply" raft command validates the read set and
//	           applies the writes atomically. No coordination needed.
//
//	N shards → two-phase commit with the metadata group as the durable
//	           transaction-status oracle:
//	             a. write   txs/<id> = {state: pending}     (metadata)
//	             b. send    tx_prepare to every shard group — validates
//	                        reads, stages writes as invisible intents
//	             c. decide  txs/<id> = {state: committed}   (metadata)
//	                        — THIS is the atomic commit point
//	             d. send    tx_commit to every group (intents → writes)
//	             e. delete  txs/<id>
//
// If the coordinator dies between (c) and (d), the janitor loop finds the
// committed record and finishes step (d); if it dies before (c), the
// janitor aborts. Either way intents never leak — all-or-nothing holds.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/pkg/kv"
)

// txRecordPrefix is where in-flight cross-shard transactions are recorded
// in the metadata keyspace.
const txRecordPrefix = "txs/"

// txRecord is the durable transaction-status record.
type txRecord struct {
	ID      string    `json:"id"`
	State   string    `json:"state"` // pending | committed | aborted
	Groups  []uint64  `json:"groups"`
	Started time.Time `json:"started"`
}

// TxCommit validates and applies a transaction. reads maps key → the
// revision the client observed (0 for "key did not exist"); writes are the
// staged mutations. Returns the commit revision of the first group (for
// single-group transactions this is *the* commit revision).
func (s *Server) TxCommit(ctx context.Context, reads map[string]uint64, writes []kv.TxWrite) (uint64, error) {
	f := (*fabric)(s)
	// Partition reads and writes by owning shard group.
	type part struct {
		reads  map[string]uint64
		writes []kv.TxWrite
	}
	parts := map[uint64]*part{}
	getPart := func(key string) (*part, error) {
		sh, err := s.shardForWrite(key)
		if err != nil {
			return nil, err
		}
		p, ok := parts[sh.GID]
		if !ok {
			p = &part{reads: map[string]uint64{}}
			parts[sh.GID] = p
		}
		return p, nil
	}
	for key, rev := range reads {
		p, err := getPart(key)
		if err != nil {
			return 0, err
		}
		p.reads[key] = rev
	}
	for _, w := range writes {
		if len(w.Value) > s.Cfg.MaxValueBytes {
			return 0, ErrValueTooLarge
		}
		p, err := getPart(w.Key)
		if err != nil {
			return 0, err
		}
		p.writes = append(p.writes, w)
	}
	if len(parts) == 0 {
		return 0, fmt.Errorf("empty transaction")
	}

	// Fast path: everything lives in one group → atomic by construction.
	if len(parts) == 1 {
		for gid, p := range parts {
			res, err := f.ProposeToGroup(ctx, gid, kv.Op{Type: "tx_apply", Reads: p.reads, Writes: p.writes})
			return res.Rev, firstErr(err, res)
		}
	}

	// Slow path: two-phase commit across groups.
	txid := auth.RandomToken(12)
	gids := make([]uint64, 0, len(parts))
	for gid := range parts {
		gids = append(gids, gid)
	}
	rec := txRecord{ID: txid, State: "pending", Groups: gids, Started: time.Now().UTC()}
	if err := s.putTxRecord(ctx, rec); err != nil {
		return 0, err
	}
	// Phase 1: prepare everywhere. Any failure → abort everywhere.
	for gid, p := range parts {
		res, err := f.ProposeToGroup(ctx, gid, kv.Op{Type: "tx_prepare", TxID: txid, Reads: p.reads, Writes: p.writes})
		if err := firstErr(err, res); err != nil {
			s.abortTx(ctx, rec)
			return 0, fmt.Errorf("Conflict: prepare on group %d: %w", gid, err)
		}
	}
	// Decision point: flipping the record to committed IS the commit.
	rec.State = "committed"
	if err := s.putTxRecord(ctx, rec); err != nil {
		// Unknown outcome for the client; the janitor resolves the
		// pending record (abort, since state never reached committed).
		return 0, fmt.Errorf("transaction outcome unknown, will be resolved: %w", err)
	}
	// Phase 2: materialize intents. Failures here are repaired by the
	// janitor — the decision is already durable.
	var commitRev uint64
	for gid := range parts {
		res, err := f.ProposeToGroup(ctx, gid, kv.Op{Type: "tx_commit", TxID: txid})
		if err := firstErr(err, res); err != nil {
			s.Logger.Warn("tx_commit delivery failed; janitor will finish", "txid", txid, "gid", gid, "err", err)
			continue
		}
		if commitRev == 0 {
			commitRev = res.Rev
		}
	}
	// Cleanup (best effort; janitor also cleans completed records).
	_, _ = f.MetaPropose(ctx, kv.Op{Type: "delete", Key: txRecordPrefix + txid})
	return commitRev, nil
}

// putTxRecord writes the transaction-status record through raft.
func (s *Server) putTxRecord(ctx context.Context, rec txRecord) error {
	raw, _ := json.Marshal(rec)
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "set", Key: txRecordPrefix + rec.ID, Value: raw})
	return firstErr(err, res)
}

// abortTx rolls back intents on every group and records the abort.
func (s *Server) abortTx(ctx context.Context, rec txRecord) {
	f := (*fabric)(s)
	rec.State = "aborted"
	raw, _ := json.Marshal(rec)
	_, _ = f.MetaPropose(ctx, kv.Op{Type: "set", Key: txRecordPrefix + rec.ID, Value: raw})
	for _, gid := range rec.Groups {
		_, _ = f.ProposeToGroup(ctx, gid, kv.Op{Type: "tx_abort", TxID: rec.ID})
	}
	_, _ = f.MetaPropose(ctx, kv.Op{Type: "delete", Key: txRecordPrefix + rec.ID})
}

// txJanitorLoop resolves transactions whose coordinator died mid-protocol.
// Runs everywhere; acts only on the metadata leader (like the controller).
func (s *Server) txJanitorLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			f := (*fabric)(s)
			if !f.IsMetaLeader() {
				continue
			}
			entries, err := f.MetaList(txRecordPrefix, 1000)
			if err != nil {
				continue
			}
			for _, e := range entries {
				var rec txRecord
				if json.Unmarshal(e.Record.Value, &rec) != nil {
					continue
				}
				// Give live coordinators time: only touch records older
				// than twice the transaction timeout.
				if time.Since(rec.Started) < 2*s.Cfg.TxTimeout+5*time.Second {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				switch rec.State {
				case "committed":
					// Decision was commit: finish phase 2.
					s.Logger.Info("janitor finishing committed tx", "txid", rec.ID)
					for _, gid := range rec.Groups {
						_, _ = f.ProposeToGroup(ctx, gid, kv.Op{Type: "tx_commit", TxID: rec.ID})
					}
					_, _ = f.MetaPropose(ctx, kv.Op{Type: "delete", Key: txRecordPrefix + rec.ID})
				default:
					// pending/aborted: roll back.
					s.Logger.Info("janitor aborting stale tx", "txid", rec.ID, "state", rec.State)
					s.abortTx(ctx, rec)
				}
				cancel()
			}
		case <-s.stopC:
			return
		}
	}
}

// TxBegin allocates a transaction ID. The protocol is stateless server-side
// until commit — reads carry revisions on their own — but the endpoint
// exists so clients have a natural place to start timeout tracking (§10).
func (s *Server) TxBegin() string { return auth.RandomToken(12) }

// Ensure cluster import stays (used by data.go siblings and future files).
var _ = cluster.MetaGID
