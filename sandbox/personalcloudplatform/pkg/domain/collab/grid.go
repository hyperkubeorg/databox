// grid.go — native spreadsheets: the pcp-sheet/1 document format and the
// generalized target-op collaboration on the substrate.
//
// A .sheet file is ONE JSON blob: named sheets (stable random ids, so
// tabs can be renamed/reordered while cells stay bound), cells holding
// the raw INPUT (`i` — literal or "=FORMULA"), the cached COMPUTED value
// (`v`), and a style index (`s`) into a shared style table. Formulas are
// evaluated only in the editor (grid.js); because computed values are
// cached in the document, the server never needs a formula engine —
// exports read `v`.
//
// Targets:
//
//	c:<sheetID>:<r>,<c>   one cell (value: Cell JSON, or null = clear)
//	cw:<sheetID>:<col>    a column width in px
//	sheets                the tab list: [{id,name},…]
//	styles                the style table: [{...},…]
//	active                the last-opened sheet id (every open starts there)
package collab

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// GridFormat and friends pin the document envelope.
const (
	GridFormat      = "pcp-sheet/1"
	GridExt         = ".sheet"
	GridContentType = "application/x-pcp-sheet+json"
	// gridMaxDoc bounds how much document the server will parse or fold.
	gridMaxDoc = 16 << 20
	// gridMaxSheets bounds the tab list.
	gridMaxSheets = 32
	// gridMaxStyles bounds the style table.
	gridMaxStyles = 512
)

// GridCell is one cell of a native spreadsheet.
type GridCell struct {
	I string `json:"i"`           // raw input: literal or "=FORMULA"
	V string `json:"v,omitempty"` // cached computed value (editor-written)
	S int    `json:"s,omitempty"` // style table index (0 = plain)
}

// GridSheet is one tab.
type GridSheet struct {
	ID    string              `json:"id"`
	Name  string              `json:"name"`
	Cells map[string]GridCell `json:"cells"` // key "r,c"
	Cols  map[string]int      `json:"cols,omitempty"`
}

// GridStyle is one entry in the deduplicated style table.
type GridStyle struct {
	B   int    `json:"b,omitempty"`   // bold
	It  int    `json:"i,omitempty"`   // italic
	U   int    `json:"u,omitempty"`   // underline
	A   string `json:"a,omitempty"`   // align: "l" | "c" | "r"
	BG  string `json:"bg,omitempty"`  // background color
	FG  string `json:"fg,omitempty"`  // text color
	Fmt string `json:"fmt,omitempty"` // number format: "0" | "0.00" | "%" | "$"
}

// GridDoc is the whole document.
type GridDoc struct {
	Format string      `json:"format"`
	Sheets []GridSheet `json:"sheets"`
	Styles []GridStyle `json:"styles,omitempty"`
	Active string      `json:"active,omitempty"` // last-opened sheet id
}

// NewGridDoc is an empty document: one blank sheet, the plain style.
func NewGridDoc() GridDoc {
	return GridDoc{
		Format: GridFormat,
		Sheets: []GridSheet{{ID: kvx.NewID()[:8], Name: "Sheet 1", Cells: map[string]GridCell{}}},
		Styles: []GridStyle{{}},
	}
}

// IsGridFile reports whether a node holds a native spreadsheet
// (.sheet, plus .pcgrid for symmetry with the other pc* extensions).
func IsGridFile(n nodes.Node) bool {
	name := strings.ToLower(n.Name)
	return !n.IsDir && (strings.HasSuffix(name, GridExt) || strings.HasSuffix(name, ".pcgrid"))
}

// ParseGridDoc decodes and shape-checks a document. Unknown formats and
// oversized tables are refused — the doc is attacker-writable through
// uploads, so the fold path never trusts it blindly.
func ParseGridDoc(raw []byte) (GridDoc, error) {
	if len(raw) > gridMaxDoc {
		return GridDoc{}, fmt.Errorf("spreadsheet too large")
	}
	var doc GridDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return GridDoc{}, err
	}
	if doc.Format != GridFormat {
		return GridDoc{}, fmt.Errorf("not a %s document", GridFormat)
	}
	if len(doc.Sheets) == 0 || len(doc.Sheets) > gridMaxSheets {
		return GridDoc{}, fmt.Errorf("a spreadsheet has 1–%d sheets", gridMaxSheets)
	}
	if len(doc.Styles) > gridMaxStyles {
		return GridDoc{}, fmt.Errorf("style table too large")
	}
	for i := range doc.Sheets {
		if doc.Sheets[i].Cells == nil {
			doc.Sheets[i].Cells = map[string]GridCell{}
		}
		if !validEntityID(doc.Sheets[i].ID) {
			return GridDoc{}, fmt.Errorf("bad sheet id")
		}
	}
	return doc, nil
}

// gridCellTargetRe pins the cell/width target grammar.
var (
	gridCellTargetRe = regexp.MustCompile(`^c:([A-Za-z0-9_-]{4,16}):(\d{1,5}),(\d{1,3})$`)
	gridColTargetRe  = regexp.MustCompile(`^cw:([A-Za-z0-9_-]{4,16}):(\d{1,3})$`)
)

// AppendGridOp validates and appends one grid op. Append-only, no
// transaction — the HLC embeds the actor, keys never collide.
func (s *Store) AppendGridOp(ctx context.Context, driveID, nodeID string, op TargetOp, actor string) error {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return users.ErrNotFound
	}
	if !ValidHLC(op.HLC, actor) {
		return fmt.Errorf("bad clock")
	}
	switch {
	case gridCellTargetRe.MatchString(op.T):
		if m := gridCellTargetRe.FindStringSubmatch(op.T); m != nil {
			r, _ := strconv.Atoi(m[2])
			c, _ := strconv.Atoi(m[3])
			if r >= MaxSheetRows || c >= MaxSheetCols {
				return fmt.Errorf("cell out of bounds")
			}
		}
		if len(op.V) > 8192 {
			return fmt.Errorf("cell values are capped at 8 KiB")
		}
	case gridColTargetRe.MatchString(op.T):
		if len(op.V) > 16 {
			return fmt.Errorf("bad width")
		}
	case op.T == "sheets", op.T == "styles":
		if len(op.V) > 64<<10 {
			return fmt.Errorf("table too large")
		}
	case op.T == "active":
		var id string
		if json.Unmarshal(op.V, &id) != nil || !validEntityID(id) {
			return fmt.Errorf("bad active sheet")
		}
	default:
		return fmt.Errorf("bad op target")
	}
	return s.appendOp(ctx, driveID, nodeID, op.HLC, op)
}

// GridState is what an opening editor loads: the folded document plus
// tail ops to replay through the same merge.
type GridState struct {
	Doc       GridDoc
	Watermark string
	Ops       []TargetOp
}

// gridSnapshot is what the snapshot blob stores between compactions.
type gridSnapshot struct {
	Watermark string  `json:"watermark"`
	Doc       GridDoc `json:"doc"`
}

// LoadGridState reads snapshot + tail ops. With no snapshot yet, the
// file's own bytes are the base (a fresh file IS a valid doc); an
// unreadable file starts empty rather than erroring — the ops are the
// truth from then on.
func (s *Store) LoadGridState(ctx context.Context, driveID, nodeID string, node nodes.Node) (GridState, error) {
	state := GridState{}
	var snap gridSnapshot
	if s.loadSnapshot(ctx, driveID, nodeID, &snap) && snap.Doc.Format == GridFormat {
		state.Doc, state.Watermark = snap.Doc, snap.Watermark
	}
	if state.Doc.Format == "" {
		if node.BlobID != "" && node.Size > 0 && node.Size < gridMaxDoc {
			var fileRaw bytes.Buffer
			if err := s.DB.GetBlob(ctx, nodes.BlobKey(driveID, node.BlobID), &fileRaw); err == nil {
				if doc, err := ParseGridDoc(fileRaw.Bytes()); err == nil {
					state.Doc = doc
				}
			}
		}
		if state.Doc.Format == "" {
			state.Doc = NewGridDoc()
		}
	}
	var err error
	state.Ops, err = s.scanTargetOps(ctx, driveID, nodeID)
	return state, err
}

// FoldTargetOp applies one op to a document (the server-side half of
// the merge; the editor mirrors it). Ops arrive HLC-ordered from the
// key scan, so plain last-write application IS highest-HLC-wins.
func FoldTargetOp(doc *GridDoc, op TargetOp) {
	if m := gridCellTargetRe.FindStringSubmatch(op.T); m != nil {
		sheet := gridSheetByID(doc, m[1])
		if sheet == nil {
			return
		}
		key := m[2] + "," + m[3]
		var cell *GridCell
		if err := json.Unmarshal(op.V, &cell); err != nil {
			return
		}
		if cell == nil || (cell.I == "" && cell.S == 0) {
			delete(sheet.Cells, key)
			return
		}
		sheet.Cells[key] = *cell
		return
	}
	if m := gridColTargetRe.FindStringSubmatch(op.T); m != nil {
		sheet := gridSheetByID(doc, m[1])
		if sheet == nil {
			return
		}
		var w int
		if json.Unmarshal(op.V, &w) != nil || w <= 0 || w > 2000 {
			return
		}
		if sheet.Cols == nil {
			sheet.Cols = map[string]int{}
		}
		sheet.Cols[m[2]] = w
		return
	}
	switch op.T {
	case "sheets":
		var tabs []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(op.V, &tabs) != nil || len(tabs) == 0 || len(tabs) > gridMaxSheets {
			return
		}
		old := map[string]GridSheet{}
		for _, sh := range doc.Sheets {
			old[sh.ID] = sh
		}
		next := make([]GridSheet, 0, len(tabs))
		seen := map[string]bool{}
		for _, t := range tabs {
			if !validEntityID(t.ID) || seen[t.ID] || strings.TrimSpace(t.Name) == "" {
				continue
			}
			seen[t.ID] = true
			sh, ok := old[t.ID]
			if !ok {
				sh = GridSheet{ID: t.ID, Cells: map[string]GridCell{}}
			}
			sh.Name = t.Name
			next = append(next, sh)
		}
		if len(next) > 0 {
			doc.Sheets = next
		}
	case "styles":
		var styles []GridStyle
		if json.Unmarshal(op.V, &styles) != nil || len(styles) == 0 || len(styles) > gridMaxStyles {
			return
		}
		doc.Styles = styles
	case "active":
		var id string
		if json.Unmarshal(op.V, &id) == nil && validEntityID(id) {
			doc.Active = id
		}
	}
}

// gridSheetByID finds a tab (nil = vanished — ops on deleted sheets
// fold to nothing).
func gridSheetByID(doc *GridDoc, id string) *GridSheet {
	for i := range doc.Sheets {
		if doc.Sheets[i].ID == id {
			return &doc.Sheets[i]
		}
	}
	return nil
}

// foldGridState is the pure half of the grid's compaction.
func foldGridState(state GridState) (GridDoc, string) {
	watermark := state.Watermark
	for _, op := range state.Ops {
		FoldTargetOp(&state.Doc, op)
		if op.HLC > watermark {
			watermark = op.HLC
		}
	}
	return state.Doc, watermark
}

// LoadGridDoc reads a .sheet file's CURRENT folded state (snapshot base
// + unfolded ops) — what exports and conversions operate on, so a
// just-typed cell exports without waiting for compaction.
func (s *Store) LoadGridDoc(ctx context.Context, driveID, nodeID string, node nodes.Node) (GridDoc, error) {
	state, err := s.LoadGridState(ctx, driveID, nodeID, node)
	if err != nil {
		return GridDoc{}, err
	}
	doc, _ := foldGridState(state)
	return doc, nil
}

// --- conversions -------------------------------------------------------------------

// GridFromCSV builds a single-sheet document from CSV bytes (literals
// only — the import path).
func GridFromCSV(raw []byte, sheetName string) (GridDoc, error) {
	doc := NewGridDoc()
	if strings.TrimSpace(sheetName) != "" {
		doc.Sheets[0].Name = sheetName
	}
	r := csv.NewReader(bytes.NewReader(raw))
	r.FieldsPerRecord = -1
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
				doc.Sheets[0].Cells[CellKey(row, col)] = GridCell{I: v, V: v}
			}
		}
		row++
	}
	return doc, nil
}

// SheetCSV renders one sheet as CSV using cached computed values (raw
// input when no computation is cached — literals are their own value).
func SheetCSV(sheet GridSheet) []byte {
	maxRow, maxCol := -1, -1
	for key := range sheet.Cells {
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
			rec[c] = gridCellValue(sheet.Cells[CellKey(r, c)])
		}
		_ = w.Write(rec)
	}
	w.Flush()
	return buf.Bytes()
}

// gridCellValue picks what a cell "is" for exports: the cached computed
// value, falling back to the raw input for plain literals.
func gridCellValue(c GridCell) string {
	if c.V != "" {
		return c.V
	}
	if strings.HasPrefix(c.I, "=") {
		return "" // formula never evaluated — nothing sane to export
	}
	return c.I
}
