// Package collab is the collaborative-document substrate (spec §2/§6),
// ported from PCD onto the /pcp/ keyspace: an append-only op-log of
// LWW-register writes, snapshot + lock-gated compaction, and presence.
// Seven document types ride it — the CSV sheet (sheet.go), native
// spreadsheets (grid.go), the document writer (wdoc.go), kanban boards
// (kanban.go), the diagram editor (draw.go), markdown (md.go), and
// calendars (cal.go, phase 5); compaction is one dispatching pass
// (compact.go).
//
// The model, in one paragraph: every edit is one op carrying a hybrid
// logical clock (13-digit wall millis, 6-digit counter, actor) that is
// monotonic per client and globally comparable AS A STRING. A target's
// winning value is the op with the HIGHEST HLC (ties break by the actor
// embedded in the clock). That merge is commutative, associative, and
// idempotent, so applying any set of ops in ANY order, duplicates
// included, converges every replica — no operational transforms, no
// central sequencer. "Live" is optimistic local apply; CORRECTNESS is
// the order-independent merge.
//
// Storage (kvx key table):
//
//	/pcp/docs/<drive>/<node>/ops/<hlc>       one op (append-only; the HLC
//	                                         embeds the actor, so writers
//	                                         never collide — no transaction)
//	/pcp/docs/<drive>/<node>/snapshot        BLOB: folded doc + the HLC
//	                                         watermark it covers
//	/pcp/docs/<drive>/<node>/presence/<user> cursor (lazy TTL, soft-fail)
//
// Boundary: this package imports kvx, the nodes domain (save-backs go
// through nodes.CommitFile with charged=false — editor compaction is
// free by contract), and the databox client. Never kernel/apps.
package collab

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/pkg/kv"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// docsPrefix roots every collaborative-document key.
const docsPrefix = "/pcp/docs/"

// presenceTTL ages out stale cursors (lazy, on read).
const presenceTTL = 30 * time.Second

// Store wraps the databox client with the document substrate. Nodes
// takes the compaction save-backs (uncharged CommitFile) and resolves
// file nodes.
type Store struct {
	DB    *client.Client
	Nodes *nodes.Store
}

// Key builders. Drive and node ids are shape-checked by every caller
// before these run.
func opsPrefix(driveID, nodeID string) string {
	return docsPrefix + driveID + "/" + nodeID + "/ops/"
}
func snapshotKey(driveID, nodeID string) string {
	return docsPrefix + driveID + "/" + nodeID + "/snapshot"
}
func presenceKey(driveID, nodeID, user string) string {
	return docsPrefix + driveID + "/" + nodeID + "/presence/" + user
}

// ValidHLC gates client-minted clocks: "<13 digit millis>-<6 digit
// counter>-<actor>", actor = the WRITING user (the handler enforces the
// suffix), fixed widths so string order IS clock order.
func ValidHLC(hlc, actor string) bool {
	parts := strings.SplitN(hlc, "-", 3)
	if len(parts) != 3 || len(parts[0]) != 13 || len(parts[1]) != 6 {
		return false
	}
	for _, r := range parts[0] + parts[1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return parts[2] == actor
}

// TargetOp is the generalized substrate op: an opaque JSON value bound
// to a string TARGET, highest HLC per target wins. Each doc type pins
// its own target grammar at append time.
type TargetOp struct {
	T   string          `json:"t"`
	V   json.RawMessage `json:"v"`
	HLC string          `json:"hlc"`
}

// validEntityID accepts the short random ids editors mint for sheets,
// blocks, cards, elements, and lines ("s0000001" seeds included).
func validEntityID(id string) bool {
	return len(id) >= 4 && len(id) <= 16 && kvx.ValidTokenChars(id)
}

// appendOp stores one validated op. No transaction: the HLC embeds the
// actor, so keys are unique per writer and blind appends never collide.
func (s *Store) appendOp(ctx context.Context, driveID, nodeID, hlc string, op any) error {
	return kvx.SetJSON(ctx, s.DB, opsPrefix(driveID, nodeID)+hlc, op)
}

// scanOps replays the op log in key order — which IS ascending HLC
// order, so plain last-write application is highest-HLC-wins.
func (s *Store) scanOps(ctx context.Context, driveID, nodeID string, fn func(value []byte)) error {
	return kvx.ScanPrefix(ctx, s.DB, opsPrefix(driveID, nodeID), func(_ string, value []byte) error {
		fn(value)
		return nil
	})
}

// scanTargetOps is scanOps decoded into TargetOps (every type but the
// CSV sheet).
func (s *Store) scanTargetOps(ctx context.Context, driveID, nodeID string) ([]TargetOp, error) {
	var ops []TargetOp
	err := s.scanOps(ctx, driveID, nodeID, func(value []byte) {
		var op TargetOp
		if json.Unmarshal(value, &op) == nil && op.T != "" {
			ops = append(ops, op)
		}
	})
	return ops, err
}

// loadSnapshot reads the snapshot blob into v (false = none usable —
// absent, unreadable, or undecodable; the file bytes seed instead).
func (s *Store) loadSnapshot(ctx context.Context, driveID, nodeID string, v any) bool {
	var raw strings.Builder
	if err := s.DB.GetBlob(ctx, snapshotKey(driveID, nodeID), &raw); err != nil || raw.Len() == 0 {
		return false
	}
	return json.Unmarshal([]byte(raw.String()), v) == nil
}

// DeleteDoc removes a document's entire op/snapshot/presence space —
// the nodes delete path calls it so a deleted file leaves no doc keys.
func (s *Store) DeleteDoc(ctx context.Context, driveID, nodeID string) error {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return users.ErrNotFound
	}
	_ = s.DB.DeleteBlob(ctx, snapshotKey(driveID, nodeID))
	return kvx.DeletePrefix(ctx, s.DB, docsPrefix+driveID+"/"+nodeID+"/")
}

// WatchOps streams a document's op-log appends: fn receives each op's
// key suffix (the <hlc> id, actor embedded) and raw value. The collab
// SSE bridge forwards both so editors merge without a re-read. Blocks
// until ctx ends or the stream breaks.
func (s *Store) WatchOps(ctx context.Context, driveID, nodeID string, fn func(opID string, value []byte) error) error {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return users.ErrNotFound
	}
	prefix := opsPrefix(driveID, nodeID)
	return s.DB.Watch(ctx, prefix, 0, func(ev kv.Event) error {
		if ev.Type != "put" {
			return nil
		}
		return fn(strings.TrimPrefix(ev.Key, prefix), ev.Value)
	})
}

// --- presence ---------------------------------------------------------------

// Presence is one editor's live cursor.
type Presence struct {
	User string    `json:"user"`
	Row  int       `json:"r"`
	Col  int       `json:"c"`
	At   time.Time `json:"at"`
}

// SetPresence records a cursor heartbeat (plain write, soft-fail
// territory — the caller ignores errors).
func (s *Store) SetPresence(ctx context.Context, driveID, nodeID, user string, row, col int) error {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) || users.ValidUsername(user) != nil {
		return nil
	}
	return kvx.SetJSON(ctx, s.DB, presenceKey(driveID, nodeID, user),
		Presence{User: user, Row: row, Col: col, At: time.Now().UTC()})
}

// ListPresence returns live cursors (stale ones lazily deleted).
func (s *Store) ListPresence(ctx context.Context, driveID, nodeID string) ([]Presence, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return nil, nil
	}
	prefix := docsPrefix + driveID + "/" + nodeID + "/presence/"
	var out []Presence
	now := time.Now()
	err := kvx.ScanPrefix(ctx, s.DB, prefix, func(key string, value []byte) error {
		var p Presence
		if json.Unmarshal(value, &p) != nil {
			return nil
		}
		if now.Sub(p.At) > presenceTTL {
			_ = s.DB.Delete(ctx, key)
			return nil
		}
		out = append(out, p)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out, err
}
