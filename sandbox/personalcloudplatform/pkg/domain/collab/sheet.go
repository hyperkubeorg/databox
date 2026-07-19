// sheet.go — the CSV sheet document: the substrate's original consumer.
// A sheet is a map of cells (row, col) → value; every edit is one op —
// SetCell{Row, Col, Value, HLC} — and a cell's winning value is the op
// with the highest HLC. Compaction folds ops ≤ watermark into a fresh
// snapshot and SAVES BACK plain CSV to the file's blob (uncharged new
// version), so the document downloads as a normal CSV file and keeps
// history.
package collab

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Sheet caps (carried from PCD): values only, no formulas.
const (
	MaxSheetRows = 10000
	MaxSheetCols = 256
)

// Op is one cell write. Clearing a cell is Value == "".
type Op struct {
	Row   int    `json:"r"`
	Col   int    `json:"c"`
	Value string `json:"v"`
	HLC   string `json:"hlc"`
}

// Cell is a folded cell: the winning value and the HLC that won it.
type Cell struct {
	V   string `json:"v"`
	HLC string `json:"hlc"`
}

// Snapshot is the materialized sheet at a known watermark: every op with
// HLC ≤ Watermark is folded in; ops after it replay on load.
type Snapshot struct {
	Watermark string          `json:"watermark"`
	Cells     map[string]Cell `json:"cells"` // key "r,c"
}

// CellKey names a cell in Snapshot.Cells.
func CellKey(row, col int) string { return strconv.Itoa(row) + "," + strconv.Itoa(col) }

// parseCellKey inverts CellKey.
func parseCellKey(key string) (int, int, bool) {
	r, c, ok := strings.Cut(key, ",")
	if !ok {
		return 0, 0, false
	}
	row, err1 := strconv.Atoi(r)
	col, err2 := strconv.Atoi(c)
	return row, col, err1 == nil && err2 == nil
}

// IsSheetFile reports whether a node holds a CSV/TSV sheet the editor
// opens.
func IsSheetFile(n nodes.Node) bool {
	name := strings.ToLower(n.Name)
	return !n.IsDir && (strings.HasSuffix(name, ".csv") || strings.HasSuffix(name, ".tsv"))
}

// AppendOp validates and appends one cell op for actor.
func (s *Store) AppendOp(ctx context.Context, driveID, nodeID string, op Op, actor string) error {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return users.ErrNotFound
	}
	if op.Row < 0 || op.Row >= MaxSheetRows || op.Col < 0 || op.Col >= MaxSheetCols {
		return fmt.Errorf("cell out of bounds (%d,%d)", op.Row, op.Col)
	}
	if len(op.Value) > 4096 {
		return fmt.Errorf("cell values are capped at 4096 characters")
	}
	if !ValidHLC(op.HLC, actor) {
		return fmt.Errorf("bad clock")
	}
	return s.appendOp(ctx, driveID, nodeID, op.HLC, op)
}

// DocState is what an opening editor loads: the folded snapshot plus the
// ops after its watermark (replayed client-side through the same merge).
type DocState struct {
	Snapshot Snapshot `json:"snapshot"`
	Ops      []Op     `json:"ops"`
	OpCount  int      `json:"op_count"`
}

// LoadDocState reads snapshot + tail ops. When NO snapshot exists yet,
// the file's current CSV bytes seed the base cells at the zero
// watermark — an existing spreadsheet opens with its content, and any op
// (HLC > "0…") beats the seed.
func (s *Store) LoadDocState(ctx context.Context, driveID, nodeID string, node nodes.Node) (DocState, error) {
	state := DocState{Snapshot: Snapshot{Cells: map[string]Cell{}}}
	if !s.loadSnapshot(ctx, driveID, nodeID, &state.Snapshot) &&
		node.BlobID != "" && node.Size > 0 && node.Size < 32<<20 {
		var fileRaw bytes.Buffer
		if err := s.DB.GetBlob(ctx, nodes.BlobKey(driveID, node.BlobID), &fileRaw); err == nil {
			seedCSV(&state.Snapshot, fileRaw.Bytes())
		}
	}
	if state.Snapshot.Cells == nil {
		state.Snapshot.Cells = map[string]Cell{}
	}
	err := s.scanOps(ctx, driveID, nodeID, func(value []byte) {
		var op Op
		if json.Unmarshal(value, &op) == nil {
			state.Ops = append(state.Ops, op)
		}
	})
	state.OpCount = len(state.Ops)
	return state, err
}

// seedCSV fills a snapshot's cells from CSV bytes at the zero HLC (any
// real op beats a seed cell).
func seedCSV(snap *Snapshot, raw []byte) {
	r := csv.NewReader(bytes.NewReader(raw))
	r.FieldsPerRecord = -1
	if snap.Cells == nil {
		snap.Cells = map[string]Cell{}
	}
	row := 0
	for row < MaxSheetRows {
		rec, err := r.Read()
		if err != nil {
			break
		}
		for col, v := range rec {
			if col >= MaxSheetCols {
				break
			}
			if v != "" {
				snap.Cells[CellKey(row, col)] = Cell{V: v}
			}
		}
		row++
	}
}

// MergeOp folds one op into a cell set — THE merge rule: highest HLC
// wins (an empty existing HLC is the CSV seed and always loses).
func MergeOp(cells map[string]Cell, op Op) {
	key := CellKey(op.Row, op.Col)
	if cur, ok := cells[key]; ok && cur.HLC >= op.HLC {
		return
	}
	if op.Value == "" {
		// A cleared cell still records its HLC (a tombstone), or an older
		// concurrent write would resurrect it.
		cells[key] = Cell{V: "", HLC: op.HLC}
		return
	}
	cells[key] = Cell{V: op.Value, HLC: op.HLC}
}

// foldDocState is the pure half of the sheet's compaction: fold every
// tail op into the snapshot and advance the watermark.
func foldDocState(state DocState) Snapshot {
	cells := state.Snapshot.Cells
	watermark := state.Snapshot.Watermark
	for _, op := range state.Ops {
		MergeOp(cells, op)
		if op.HLC > watermark {
			watermark = op.HLC
		}
	}
	return Snapshot{Watermark: watermark, Cells: cells}
}

// renderDocCSV materializes a snapshot as plain CSV — the save-back
// bytes and the download shape.
func renderDocCSV(snap Snapshot) []byte {
	maxRow, maxCol := -1, -1
	for key, cell := range snap.Cells {
		if cell.V == "" {
			continue
		}
		r, c, ok := parseCellKey(key)
		if !ok {
			continue
		}
		if r > maxRow {
			maxRow = r
		}
		if c > maxCol {
			maxCol = c
		}
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	for r := 0; r <= maxRow; r++ {
		rec := make([]string, maxCol+1)
		for c := 0; c <= maxCol; c++ {
			rec[c] = snap.Cells[CellKey(r, c)].V
		}
		_ = w.Write(rec)
	}
	w.Flush()
	return buf.Bytes()
}
