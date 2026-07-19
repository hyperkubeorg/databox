// compact.go — the lock-gated compaction pass every doc type shares.
//
// Past a threshold of appended op batches (the app layer counts) or on
// editor close, whoever holds the databox LOCK "pcp/compact/<drive>/
// <node>" folds the op log into a fresh snapshot, SAVES BACK the
// materialized file content to the node as an UNCHARGED new version
// (nodes.CommitFile charged=false — this is what makes the document
// download as a real CSV/JSON/markdown file and keeps history), then
// prunes exactly the folded ops. Losing the lock race is success —
// someone else is doing the work. Idempotent and safe to lose: the op
// log is the source of truth until pruned.
//
// The fold itself is pure per type (fold*State in each file); this file
// only owns the lock, the ordering, and the type dispatch — so the fold
// logic unit-tests without locks or blobs.
package collab

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// CompactEvery is the append-batch cadence for opportunistic compaction
// (the app layer folds every Nth batch in the background; close always
// folds).
const CompactEvery = 32

// folded is one type's compaction output: the snapshot to store (built
// AFTER the save-back so md can record the new blob id), the file bytes
// to save back, and the watermark the prune covers.
type folded struct {
	snapshot  func(blobID string) []byte
	file      []byte
	fileCT    string
	watermark string
	opCount   int
}

// Compact runs one compaction pass for whatever document type the node
// holds (dispatch by file name, exactly like the append endpoints). A
// node that is no doc type is a no-op.
func (s *Store) Compact(ctx context.Context, driveID, nodeID, by string) error {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return users.ErrNotFound
	}
	resource := "pcp/compact/" + driveID + "/" + nodeID
	if _, err := s.DB.LockAcquire(ctx, resource, "exclusive", 2*time.Minute); err != nil {
		return nil // a racing compactor holds it — their fold covers us
	}
	defer func() { _ = s.DB.LockRelease(ctx, resource) }()

	node, found, err := s.Nodes.GetByID(ctx, driveID, nodeID)
	if err != nil || !found || node.IsDir {
		return err
	}
	f, err := s.foldFor(ctx, driveID, nodeID, node)
	if err != nil || f.opCount == 0 {
		return err
	}

	// Save-back first, then snapshot, then prune: a crash between steps
	// leaves the unpruned log to re-fold idempotently on the next pass.
	blobID := kvx.NewID()
	if err := s.DB.PutBlob(ctx, nodes.BlobKey(driveID, blobID), bytes.NewReader(f.file), f.fileCT); err != nil {
		return err
	}
	ref, found, err := s.Nodes.GetRef(ctx, driveID, nodeID)
	if err != nil || !found {
		return users.ErrNotFound
	}
	if _, err := s.Nodes.CommitFile(ctx, driveID, ref.ParentID, ref.Name, blobID, f.fileCT, int64(len(f.file)), by, false); err != nil {
		return err
	}
	if err := s.DB.PutBlob(ctx, snapshotKey(driveID, nodeID), bytes.NewReader(f.snapshot(blobID)), "application/json"); err != nil {
		return err
	}
	// Prune exactly what the snapshot folded: [start, watermark+"\x00").
	prefix := opsPrefix(driveID, nodeID)
	return s.DB.DeleteRange(ctx, prefix, prefix+f.watermark+"\x00")
}

// foldFor loads and folds the node's document by type.
func (s *Store) foldFor(ctx context.Context, driveID, nodeID string, node nodes.Node) (folded, error) {
	switch {
	case IsGridFile(node):
		state, err := s.LoadGridState(ctx, driveID, nodeID, node)
		if err != nil {
			return folded{}, err
		}
		doc, wm := foldGridState(state)
		raw, _ := json.Marshal(doc)
		return folded{
			snapshot: func(string) []byte { raw, _ := json.Marshal(gridSnapshot{Watermark: wm, Doc: doc}); return raw },
			file:     raw, fileCT: GridContentType, watermark: wm, opCount: len(state.Ops),
		}, nil
	case IsWDocFile(node):
		state, err := s.LoadWDocState(ctx, driveID, nodeID, node)
		if err != nil {
			return folded{}, err
		}
		doc, wm := foldWDocState(state)
		raw, _ := json.Marshal(doc)
		return folded{
			snapshot: func(string) []byte { raw, _ := json.Marshal(wdocSnapshot{Watermark: wm, Doc: doc}); return raw },
			file:     raw, fileCT: WDocContentType, watermark: wm, opCount: len(state.Ops),
		}, nil
	case IsKanbanFile(node):
		state, err := s.LoadKanbanState(ctx, driveID, nodeID, node)
		if err != nil {
			return folded{}, err
		}
		doc, wm := foldKanbanState(state)
		raw, _ := json.Marshal(doc)
		return folded{
			snapshot: func(string) []byte { raw, _ := json.Marshal(kanbanSnapshot{Watermark: wm, Doc: doc}); return raw },
			file:     raw, fileCT: KanbanContentType, watermark: wm, opCount: len(state.Ops),
		}, nil
	case IsDrawFile(node):
		state, err := s.LoadDrawState(ctx, driveID, nodeID, node)
		if err != nil {
			return folded{}, err
		}
		doc, wm := foldDrawState(state)
		raw, _ := json.Marshal(doc)
		return folded{
			snapshot: func(string) []byte { raw, _ := json.Marshal(drawSnapshot{Watermark: wm, Doc: doc}); return raw },
			file:     raw, fileCT: DrawContentType, watermark: wm, opCount: len(state.Ops),
		}, nil
	case IsMDFile(node):
		state, err := s.LoadMDState(ctx, driveID, nodeID, node)
		if err != nil {
			return folded{}, err
		}
		doc, wm := foldMDState(state)
		return folded{
			// The md snapshot must record the blob the save-back just
			// committed, or the next load reads it as an out-of-band
			// replacement and reseeds.
			snapshot: func(blobID string) []byte {
				raw, _ := json.Marshal(mdSnapshot{Watermark: wm, Blob: blobID, Doc: doc})
				return raw
			},
			file: []byte(MDText(doc)), fileCT: MDContentType, watermark: wm, opCount: len(state.Ops),
		}, nil
	case IsCalFile(node):
		state, err := s.LoadCalState(ctx, driveID, nodeID, node)
		if err != nil {
			return folded{}, err
		}
		doc, wm := foldCalState(state)
		raw, _ := json.Marshal(doc)
		return folded{
			snapshot: func(string) []byte { raw, _ := json.Marshal(calSnapshot{Watermark: wm, Doc: doc}); return raw },
			file:     raw, fileCT: CalContentType, watermark: wm, opCount: len(state.Ops),
		}, nil
	case IsSheetFile(node):
		state, err := s.LoadDocState(ctx, driveID, nodeID, node)
		if err != nil {
			return folded{}, err
		}
		snap := foldDocState(state)
		return folded{
			snapshot: func(string) []byte { raw, _ := json.Marshal(snap); return raw },
			file:     renderDocCSV(snap), fileCT: "text/csv", watermark: snap.Watermark, opCount: len(state.Ops),
		}, nil
	}
	return folded{}, nil
}
