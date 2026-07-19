package collab

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
)

// hlcAt builds a well-formed HLC for tests.
func hlcAt(millis int64, counter int, actor string) string {
	return fmt.Sprintf("%013d-%06d-%s", millis, counter, actor)
}

func TestValidHLC(t *testing.T) {
	if !ValidHLC(hlcAt(1700000000000, 0, "ada"), "ada") {
		t.Error("well-formed HLC rejected")
	}
	for _, bad := range []string{
		"",
		"1700000000000-000000-bob",    // wrong actor
		"170000000000-000000-ada",     // 12-digit millis
		"1700000000000-00000-ada",     // 5-digit counter
		"170000000000x-000000-ada",    // non-digit
		"1700000000000-000000",        // no actor
		"1700000000000000000-000-ada", // widths off
	} {
		if ValidHLC(bad, "ada") {
			t.Errorf("bad HLC %q accepted", bad)
		}
	}
}

// TestMergeOpConverges is THE correctness property: applying any set of
// ops in ANY order, with duplicates, converges to the same cells.
func TestMergeOpConverges(t *testing.T) {
	ops := []Op{
		{Row: 0, Col: 0, Value: "a", HLC: hlcAt(1000, 0, "ada")},
		{Row: 0, Col: 0, Value: "b", HLC: hlcAt(1000, 0, "bob")}, // same instant — actor tiebreak
		{Row: 0, Col: 0, Value: "c", HLC: hlcAt(2000, 0, "ada")}, // later — wins
		{Row: 1, Col: 2, Value: "x", HLC: hlcAt(1500, 0, "bob")},
		{Row: 1, Col: 2, Value: "", HLC: hlcAt(1600, 0, "ada")}, // tombstone beats x
		{Row: 3, Col: 1, Value: "keep", HLC: hlcAt(900, 3, "bob")},
	}
	apply := func(order []int, dups bool) map[string]Cell {
		cells := map[string]Cell{}
		for _, i := range order {
			MergeOp(cells, ops[i])
			if dups {
				MergeOp(cells, ops[i]) // idempotence
			}
		}
		return cells
	}
	want := apply([]int{0, 1, 2, 3, 4, 5}, false)
	if want[CellKey(0, 0)].V != "c" {
		t.Fatalf("highest HLC should win cell 0,0: got %+v", want[CellKey(0, 0)])
	}
	if want[CellKey(1, 2)].V != "" || want[CellKey(1, 2)].HLC == "" {
		t.Fatalf("clear should tombstone, not drop: got %+v", want[CellKey(1, 2)])
	}
	r := rand.New(rand.NewSource(42))
	for trial := 0; trial < 50; trial++ {
		order := r.Perm(len(ops))
		got := apply(order, trial%2 == 0)
		if len(got) != len(want) {
			t.Fatalf("order %v: %d cells, want %d", order, len(got), len(want))
		}
		for k, w := range want {
			if got[k] != w {
				t.Fatalf("order %v: cell %s = %+v, want %+v", order, k, got[k], w)
			}
		}
	}
	// Actor tiebreak: with only the same-instant pair, bob (later actor
	// string) wins regardless of order.
	a := map[string]Cell{}
	MergeOp(a, ops[0])
	MergeOp(a, ops[1])
	b := map[string]Cell{}
	MergeOp(b, ops[1])
	MergeOp(b, ops[0])
	if a[CellKey(0, 0)] != b[CellKey(0, 0)] || a[CellKey(0, 0)].V != "b" {
		t.Errorf("actor tiebreak not commutative: %+v vs %+v", a[CellKey(0, 0)], b[CellKey(0, 0)])
	}
}

// TestSheetFoldAndCSV covers the compaction fold + CSV materialization
// pure halves: seed loses to ops, watermark advances, CSV shape right.
func TestSheetFoldAndCSV(t *testing.T) {
	snap := Snapshot{Cells: map[string]Cell{}}
	seedCSV(&snap, []byte("name,qty\nwidget,2\n"))
	if snap.Cells[CellKey(0, 0)].V != "name" || snap.Cells[CellKey(1, 1)].V != "2" {
		t.Fatalf("seed wrong: %+v", snap.Cells)
	}
	state := DocState{Snapshot: snap, Ops: []Op{
		{Row: 1, Col: 1, Value: "3", HLC: hlcAt(1000, 0, "ada")}, // beats seed
		{Row: 2, Col: 0, Value: "gadget", HLC: hlcAt(1001, 0, "bob")},
	}}
	folded := foldDocState(state)
	if folded.Watermark != hlcAt(1001, 0, "bob") {
		t.Errorf("watermark = %q", folded.Watermark)
	}
	csv := string(renderDocCSV(folded))
	want := "name,qty\nwidget,3\ngadget,\n"
	if csv != want {
		t.Errorf("csv = %q, want %q", csv, want)
	}
}

func targetOp(t *testing.T, target string, v any, hlc string) TargetOp {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return TargetOp{T: target, V: raw, HLC: hlc}
}

// TestGridFold covers cells, widths, tab renames, and the watermark
// through the pure fold — compaction correctness minus locks/blobs.
func TestGridFold(t *testing.T) {
	doc := NewGridDoc()
	sheetID := doc.Sheets[0].ID
	state := GridState{Doc: doc, Ops: []TargetOp{
		targetOp(t, "c:"+sheetID+":0,0", GridCell{I: "=1+1", V: "2"}, hlcAt(1000, 0, "ada")),
		targetOp(t, "c:"+sheetID+":0,0", GridCell{I: "5", V: "5"}, hlcAt(2000, 0, "bob")),
		targetOp(t, "cw:"+sheetID+":3", 240, hlcAt(2001, 0, "ada")),
		targetOp(t, "sheets", []map[string]string{{"id": sheetID, "name": "Renamed"}}, hlcAt(2002, 0, "ada")),
		targetOp(t, "active", sheetID, hlcAt(2003, 0, "bob")),
	}}
	folded, wm := foldGridState(state)
	if wm != hlcAt(2003, 0, "bob") {
		t.Errorf("watermark = %q", wm)
	}
	sh := folded.Sheets[0]
	if sh.Name != "Renamed" || sh.Cells["0,0"].V != "5" || sh.Cols["3"] != 240 || folded.Active != sheetID {
		t.Errorf("fold wrong: %+v", folded)
	}
	// Idempotence: re-folding the same ops changes nothing.
	again, _ := foldGridState(GridState{Doc: folded, Ops: state.Ops})
	rawA, _ := json.Marshal(folded)
	rawB, _ := json.Marshal(again)
	if string(rawA) != string(rawB) {
		t.Errorf("fold not idempotent:\n%s\n%s", rawA, rawB)
	}
	// A clear op deletes the cell.
	cleared, _ := foldGridState(GridState{Doc: folded, Ops: []TargetOp{
		{T: "c:" + sheetID + ":0,0", V: json.RawMessage("null"), HLC: hlcAt(3000, 0, "ada")},
	}})
	if _, ok := cleared.Sheets[0].Cells["0,0"]; ok {
		t.Error("null op should clear the cell")
	}
}

func TestWDocFold(t *testing.T) {
	doc := NewWDoc()
	first := doc.Blocks[0].ID
	state := WDocState{Doc: doc, Ops: []TargetOp{
		targetOp(t, "bl:"+first, "<p>hello</p>", hlcAt(1000, 0, "ada")),
		targetOp(t, "bl:blk2aaaa", "<p>second</p>", hlcAt(1001, 0, "bob")),
		targetOp(t, "blocks", []string{"blk2aaaa", first}, hlcAt(1002, 0, "bob")),
		targetOp(t, "header", "<em>hdr</em>", hlcAt(1003, 0, "ada")),
		targetOp(t, "page", WDocPage{Size: "a4"}, hlcAt(1004, 0, "ada")),
	}}
	folded, wm := foldWDocState(state)
	if wm != hlcAt(1004, 0, "ada") {
		t.Errorf("watermark = %q", wm)
	}
	if len(folded.Blocks) != 2 || folded.Blocks[0].ID != "blk2aaaa" || folded.Blocks[1].HTML != "<p>hello</p>" {
		t.Errorf("blocks wrong: %+v", folded.Blocks)
	}
	if folded.Header != "<em>hdr</em>" || folded.Page.Size != "a4" {
		t.Errorf("header/page wrong: %+v", folded)
	}
	if got := WDocText(folded); got != "second\nhello\n" {
		t.Errorf("WDocText = %q", got)
	}
}

func TestKanbanFold(t *testing.T) {
	doc := NewKanban()
	col := doc.Cols[0].ID
	state := KanbanState{Doc: doc, Ops: []TargetOp{
		targetOp(t, "card:cardaaaa", KanbanCard{ID: "cardaaaa", Col: col, Pos: 2, Text: "later"}, hlcAt(1000, 0, "ada")),
		targetOp(t, "card:cardbbbb", KanbanCard{ID: "cardbbbb", Col: col, Pos: 1, Text: "first"}, hlcAt(1001, 0, "bob")),
		targetOp(t, "card:cardaaaa", KanbanCard{ID: "cardaaaa", Col: col, Pos: 3, Text: "edited"}, hlcAt(1002, 0, "bob")),
		targetOp(t, "todo:itemaaaa", KanbanItem{ID: "itemaaaa", Pos: 1, Text: "inbox"}, hlcAt(1003, 0, "ada")),
	}}
	folded, _ := foldKanbanState(state)
	if len(folded.Cards) != 2 || folded.Cards[0].ID != "cardbbbb" || folded.Cards[1].Text != "edited" {
		t.Errorf("cards wrong: %+v", folded.Cards)
	}
	if len(folded.Items) != 1 || folded.Items[0].Text != "inbox" {
		t.Errorf("items wrong: %+v", folded.Items)
	}
	// Delete op removes the card.
	gone, _ := foldKanbanState(KanbanState{Doc: folded, Ops: []TargetOp{
		{T: "card:cardbbbb", V: json.RawMessage("null"), HLC: hlcAt(2000, 0, "ada")},
	}})
	if len(gone.Cards) != 1 || gone.Cards[0].ID != "cardaaaa" {
		t.Errorf("delete fold wrong: %+v", gone.Cards)
	}
}

func TestDrawFold(t *testing.T) {
	state := DrawState{Doc: NewDraw(), Ops: []TargetOp{
		targetOp(t, "el:rectaaaa", DrawEl{ID: "rectaaaa", Type: "rect", X: 10, Y: 10, W: 100, H: 50}, hlcAt(1000, 0, "ada")),
		targetOp(t, "el:textbbbb", DrawEl{ID: "textbbbb", Type: "text", X: 5, Y: 5, Text: "hi", FS: 16}, hlcAt(1001, 0, "bob")),
		targetOp(t, "order", []string{"textbbbb", "rectaaaa"}, hlcAt(1002, 0, "ada")),
		targetOp(t, "bg", "grid", hlcAt(1003, 0, "ada")),
	}}
	folded, _ := foldDrawState(state)
	if len(folded.Els) != 2 || folded.Els[0].ID != "textbbbb" || folded.BG != "grid" {
		t.Errorf("draw fold wrong: %+v", folded)
	}
	// Invalid element ops fold to nothing.
	bad, _ := foldDrawState(DrawState{Doc: folded, Ops: []TargetOp{
		targetOp(t, "el:rectaaaa", DrawEl{ID: "rectaaaa", Type: "nope"}, hlcAt(2000, 0, "ada")),
	}})
	if bad.Els[1].Type != "rect" {
		t.Errorf("invalid op should not fold: %+v", bad.Els[1])
	}
}

func TestMDSeedFoldRoundtrip(t *testing.T) {
	raw := []byte("# Title\r\n\r\nbody line\n")
	a, b := SeedMD(raw), SeedMD(raw)
	for i := range a.Lines {
		if a.Lines[i] != b.Lines[i] {
			t.Fatal("seed ids not deterministic")
		}
	}
	if MDText(a) != "# Title\n\nbody line\n" {
		t.Errorf("normalize+roundtrip = %q", MDText(a))
	}
	state := MDState{Doc: a, Ops: []TargetOp{
		targetOp(t, "ln:s0000000", "# Better title", hlcAt(1000, 0, "ada")),
		targetOp(t, "ln:newlineaa", "appended", hlcAt(1001, 0, "bob")),
		targetOp(t, "lines", []string{"s0000000", "newlineaa", "s0000001", "s0000002", "s0000003"}, hlcAt(1002, 0, "bob")),
	}}
	folded, wm := foldMDState(state)
	if wm != hlcAt(1002, 0, "bob") {
		t.Errorf("watermark = %q", wm)
	}
	if got := MDText(folded); got != "# Better title\nappended\n\nbody line\n" {
		t.Errorf("folded text = %q", got)
	}
}

// TestAppendAndLoadOnKVX exercises the REAL store paths kvxtest can
// carry: op validation, the append keyspace, and load's op-scan replay
// (no snapshot blob exists, so the doc starts from its default).
func TestAppendAndLoadOnKVX(t *testing.T) {
	db := kvxtest.New(t)
	s := &Store{DB: db, Nodes: &nodes.Store{DB: db}}
	ctx := context.Background()
	driveID, nodeID := "driveAAAAAAA", "nodeBBBBBBBB"

	// Bad clocks and foreign actors never land.
	if err := s.AppendGridOp(ctx, driveID, nodeID, targetOp(t, "active", "sheet1aa", hlcAt(1000, 0, "bob")), "ada"); err == nil {
		t.Error("foreign actor accepted")
	}
	if err := s.AppendGridOp(ctx, driveID, nodeID, targetOp(t, "bogus", "x", hlcAt(1000, 0, "ada")), "ada"); err == nil {
		t.Error("bad target accepted")
	}
	if err := s.AppendOp(ctx, driveID, nodeID, Op{Row: MaxSheetRows, Col: 0, Value: "x", HLC: hlcAt(1000, 0, "ada")}, "ada"); err == nil {
		t.Error("out-of-bounds cell accepted")
	}

	// Two actors' grid ops land and replay in HLC order.
	docNode := nodes.Node{ID: nodeID, Name: "Budget.sheet"}
	ops := []TargetOp{
		targetOp(t, "sheets", []map[string]string{{"id": "sheet1aa", "name": "Data"}}, hlcAt(1000, 0, "ada")),
		targetOp(t, "c:sheet1aa:0,0", GridCell{I: "1", V: "1"}, hlcAt(1001, 0, "bob")),
		targetOp(t, "c:sheet1aa:0,0", GridCell{I: "2", V: "2"}, hlcAt(1002, 0, "ada")),
	}
	for _, op := range ops {
		actor := strings.SplitN(op.HLC, "-", 3)[2]
		if err := s.AppendGridOp(ctx, driveID, nodeID, op, actor); err != nil {
			t.Fatalf("append %s: %v", op.T, err)
		}
	}
	state, err := s.LoadGridState(ctx, driveID, nodeID, docNode)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.Ops) != 3 {
		t.Fatalf("replayed %d ops, want 3", len(state.Ops))
	}
	folded, _ := foldGridState(state)
	if folded.Sheets[0].Name != "Data" || folded.Sheets[0].Cells["0,0"].V != "2" {
		t.Errorf("converged doc wrong: %+v", folded.Sheets[0])
	}

	// The ops live where the key table says they live.
	entries, _, err := db.List(ctx, "/pcp/docs/"+driveID+"/"+nodeID+"/ops/", "", 10)
	if err != nil || len(entries) != 3 {
		t.Fatalf("ops keyspace: %d entries, err %v", len(entries), err)
	}

	// Presence: set, list, and foreign users' rows sort deterministically.
	if err := s.SetPresence(ctx, driveID, nodeID, "bob", 1, 2); err != nil {
		t.Fatalf("presence: %v", err)
	}
	if err := s.SetPresence(ctx, driveID, nodeID, "ada", 0, 0); err != nil {
		t.Fatalf("presence: %v", err)
	}
	ps, err := s.ListPresence(ctx, driveID, nodeID)
	if err != nil || len(ps) != 2 || ps[0].User != "ada" || ps[1].Col != 2 {
		t.Fatalf("presence list: %+v err %v", ps, err)
	}
}

func TestGridCSVConversions(t *testing.T) {
	doc, err := GridFromCSV([]byte("a,b\n1,=2\n"), "Imported")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Sheets[0].Name != "Imported" || doc.Sheets[0].Cells["1,0"].I != "1" {
		t.Errorf("import wrong: %+v", doc.Sheets[0])
	}
	sheet := GridSheet{Cells: map[string]GridCell{
		"0,0": {I: "x"},
		"0,1": {I: "=1+1", V: "2"},
		"1,0": {I: "=broken"}, // formula with no cached value exports empty
	}}
	if got := string(SheetCSV(sheet)); got != "x,2\n,\n" {
		t.Errorf("SheetCSV = %q", got)
	}
}

func TestWriteXLSXShape(t *testing.T) {
	doc := NewGridDoc()
	doc.Sheets[0].Name = "Q1 [draft]"
	doc.Sheets[0].Cells["0,0"] = GridCell{I: "42", V: "42"}
	doc.Sheets[0].Cells["0,1"] = GridCell{I: "hello & bye"}
	var buf strings.Builder
	if err := WriteXLSX(&nopWriter{&buf}, doc); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "PK") {
		t.Error("xlsx is not a zip")
	}
	xml := worksheetXML(doc.Sheets[0])
	if !strings.Contains(xml, `<c r="A1"><v>42</v></c>`) {
		t.Errorf("number cell missing: %s", xml)
	}
	if !strings.Contains(xml, "hello &amp; bye") {
		t.Errorf("inline string not escaped: %s", xml)
	}
	if got := xlsxSheetName("Q1 [draft]", 0); got != "Q1 _draft_" {
		t.Errorf("sheet name sanitize = %q", got)
	}
	if XLSXColName(27) != "AB" {
		t.Errorf("col name = %q", XLSXColName(27))
	}
}

// nopWriter adapts a strings.Builder to io.Writer for the zip.
type nopWriter struct{ b *strings.Builder }

func (w *nopWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

func TestParsersRejectForeignFormats(t *testing.T) {
	if _, err := ParseGridDoc([]byte(`{"format":"pcd-sheet/1","sheets":[{"id":"aaaa1111","name":"x","cells":{}}]}`)); err == nil {
		t.Error("grid parser accepted a foreign format")
	}
	if _, err := ParseWDoc([]byte(`{"format":"nope","blocks":[{"id":"aaaa1111","html":""}]}`)); err == nil {
		t.Error("wdoc parser accepted a foreign format")
	}
	if _, err := ParseKanban([]byte(`{"format":"nope"}`)); err == nil {
		t.Error("kanban parser accepted a foreign format")
	}
	if _, err := ParseDraw([]byte(`{"format":"nope"}`)); err == nil {
		t.Error("draw parser accepted a foreign format")
	}
}

func TestIsDocFiles(t *testing.T) {
	file := func(name string) nodes.Node { return nodes.Node{Name: name} }
	if !IsGridFile(file("a.sheet")) || !IsGridFile(file("a.pcgrid")) || IsGridFile(file("a.csv")) {
		t.Error("IsGridFile wrong")
	}
	if !IsSheetFile(file("a.csv")) || !IsSheetFile(file("a.TSV")) || IsSheetFile(file("a.sheet")) {
		t.Error("IsSheetFile wrong")
	}
	if !IsWDocFile(file("a.pcdoc")) || !IsKanbanFile(file("a.pckan")) ||
		!IsDrawFile(file("a.pcdraw")) || !IsMDFile(file("README.md")) || !IsMDFile(file("a.markdown")) {
		t.Error("doc type checks wrong")
	}
	dir := nodes.Node{Name: "a.md", IsDir: true}
	if IsMDFile(dir) {
		t.Error("a folder is never a doc")
	}
	_ = kvx.ValidID // keep the import honest if assertions above change
}
