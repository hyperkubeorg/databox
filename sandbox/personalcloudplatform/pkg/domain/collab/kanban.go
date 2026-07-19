// kanban.go — the kanban board's model: the pcp-kanban/1 format on the
// target-op substrate.
//
// A board is ROWS (swimlanes) of COLUMNS of CARDS, plus an INBOX of
// unsorted items. Every entity is its own merge target — concurrent
// edits to different cards both land; the same card is LWW:
//
//	row:<id>   one swimlane's JSON (null = delete)
//	col:<id>   one column's JSON (null = delete)
//	card:<id>  one card's JSON (null = delete)
//	todo:<id>  one inbox item's JSON (null = delete)
//
// Ordering is FRACTIONAL: rows, columns, and cards carry a float
// position and render sorted by (pos, id) — a drag is one op on the
// moved entity, never a list rewrite, so two people rearranging the
// same column converge without clobbering each other. A card's column
// membership rides on the card itself (`col`), so moving a card and
// editing its text concurrently merge per-card, LWW.
package collab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Kanban format constants.
const (
	KanbanFormat      = "pcp-kanban/1"
	KanbanExt         = ".pckan"
	KanbanContentType = "application/x-pcp-kanban+json"
	kanbanMaxDoc      = 8 << 20
	kanbanMaxRows     = 64
	kanbanMaxCols     = 512
	kanbanMaxCards    = 10000
	kanbanMaxItems    = 2000
	kanbanMaxTitle    = 512
	kanbanMaxText     = 4 << 10
	kanbanMaxDesc     = 16 << 10
	kanbanMaxTags     = 32
	kanbanMaxTag      = 64
	kanbanMaxLinks    = 32
	kanbanMaxLinkName = 200
	kanbanMaxURL      = 2048
	kanbanMaxEntity   = 32 << 10 // one entity's op value
	kanbanMaxPos      = 1e12
)

// KanbanRow is one swimlane.
type KanbanRow struct {
	ID    string  `json:"id"`
	Title string  `json:"title"`
	Pos   float64 `json:"pos"`
}

// KanbanCol is one column; Row binds it to a swimlane.
type KanbanCol struct {
	ID    string  `json:"id"`
	Row   string  `json:"row"`
	Title string  `json:"title"`
	Pos   float64 `json:"pos"`
}

// KanbanLink is one external link on a card.
type KanbanLink struct {
	Name string `json:"n,omitempty"`
	URL  string `json:"u"`
}

// KanbanCard is one card; Col binds it to a column.
type KanbanCard struct {
	ID    string       `json:"id"`
	Col   string       `json:"col"`
	Pos   float64      `json:"pos"`
	Text  string       `json:"text"`
	Desc  string       `json:"desc,omitempty"`
	Tags  []string     `json:"tags,omitempty"`
	Links []KanbanLink `json:"links,omitempty"`
}

// KanbanItem is one unsorted inbox item.
type KanbanItem struct {
	ID   string  `json:"id"`
	Pos  float64 `json:"pos"`
	Text string  `json:"text"`
	Done bool    `json:"done,omitempty"`
}

// KanbanDoc is the whole board.
type KanbanDoc struct {
	Format string       `json:"format"`
	Rows   []KanbanRow  `json:"rows"`
	Cols   []KanbanCol  `json:"cols"`
	Cards  []KanbanCard `json:"cards"`
	Items  []KanbanItem `json:"items,omitempty"`
}

// NewKanban is a starter board: one swimlane, the classic three columns.
func NewKanban() KanbanDoc {
	row := kvx.NewID()[:8]
	doc := KanbanDoc{
		Format: KanbanFormat,
		Rows:   []KanbanRow{{ID: row, Title: "Board", Pos: 1}},
		Cards:  []KanbanCard{},
	}
	for i, title := range []string{"To do", "Doing", "Done"} {
		doc.Cols = append(doc.Cols, KanbanCol{ID: kvx.NewID()[:8], Row: row, Title: title, Pos: float64(i + 1)})
	}
	return doc
}

// IsKanbanFile reports whether a node holds a kanban board.
func IsKanbanFile(n nodes.Node) bool {
	return !n.IsDir && strings.HasSuffix(strings.ToLower(n.Name), KanbanExt)
}

// kanbanPosOK gates fractional positions (JSON can't carry NaN/Inf, but
// hand-posted magnitudes still get bounded).
func kanbanPosOK(p float64) bool { return p >= -kanbanMaxPos && p <= kanbanMaxPos }

// ValidKanbanRow is the swimlane shape gate the ops and parser share.
func ValidKanbanRow(r KanbanRow) error {
	if !validEntityID(r.ID) {
		return fmt.Errorf("bad row id")
	}
	if len(r.Title) > kanbanMaxTitle {
		return fmt.Errorf("row title too long")
	}
	if !kanbanPosOK(r.Pos) {
		return fmt.Errorf("row position out of range")
	}
	return nil
}

// ValidKanbanCol is the column shape gate.
func ValidKanbanCol(c KanbanCol) error {
	if !validEntityID(c.ID) || !validEntityID(c.Row) {
		return fmt.Errorf("bad column id")
	}
	if len(c.Title) > kanbanMaxTitle {
		return fmt.Errorf("column title too long")
	}
	if !kanbanPosOK(c.Pos) {
		return fmt.Errorf("column position out of range")
	}
	return nil
}

// kanbanURLOK allows only http(s) — card links render as anchors, so
// javascript: and friends stay out at the model.
func kanbanURLOK(u string) bool {
	if len(u) == 0 || len(u) > kanbanMaxURL {
		return false
	}
	low := strings.ToLower(u)
	return strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://")
}

// ValidKanbanCard is the card shape gate.
func ValidKanbanCard(c KanbanCard) error {
	if !validEntityID(c.ID) || !validEntityID(c.Col) {
		return fmt.Errorf("bad card id")
	}
	if !kanbanPosOK(c.Pos) {
		return fmt.Errorf("card position out of range")
	}
	if len(c.Text) > kanbanMaxText {
		return fmt.Errorf("card text too long")
	}
	if len(c.Desc) > kanbanMaxDesc {
		return fmt.Errorf("card description too long")
	}
	if len(c.Tags) > kanbanMaxTags {
		return fmt.Errorf("too many tags")
	}
	for _, t := range c.Tags {
		if t == "" || len(t) > kanbanMaxTag || strings.ContainsAny(t, " \t\n\r,#") {
			return fmt.Errorf("bad tag")
		}
	}
	if len(c.Links) > kanbanMaxLinks {
		return fmt.Errorf("too many links")
	}
	for _, l := range c.Links {
		if len(l.Name) > kanbanMaxLinkName || !kanbanURLOK(l.URL) {
			return fmt.Errorf("bad link")
		}
	}
	return nil
}

// ValidKanbanItem is the inbox-item shape gate.
func ValidKanbanItem(i KanbanItem) error {
	if !validEntityID(i.ID) {
		return fmt.Errorf("bad item id")
	}
	if len(i.Text) > kanbanMaxText {
		return fmt.Errorf("item text too long")
	}
	if !kanbanPosOK(i.Pos) {
		return fmt.Errorf("item position out of range")
	}
	return nil
}

// ParseKanban decodes and shape-checks a board.
func ParseKanban(raw []byte) (KanbanDoc, error) {
	if len(raw) > kanbanMaxDoc {
		return KanbanDoc{}, fmt.Errorf("board too large")
	}
	var doc KanbanDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return KanbanDoc{}, err
	}
	if doc.Format != KanbanFormat {
		return KanbanDoc{}, fmt.Errorf("not a %s board", KanbanFormat)
	}
	if len(doc.Rows) > kanbanMaxRows || len(doc.Cols) > kanbanMaxCols ||
		len(doc.Cards) > kanbanMaxCards || len(doc.Items) > kanbanMaxItems {
		return KanbanDoc{}, fmt.Errorf("board has too many entities")
	}
	for _, r := range doc.Rows {
		if err := ValidKanbanRow(r); err != nil {
			return KanbanDoc{}, err
		}
	}
	for _, c := range doc.Cols {
		if err := ValidKanbanCol(c); err != nil {
			return KanbanDoc{}, err
		}
	}
	for _, c := range doc.Cards {
		if err := ValidKanbanCard(c); err != nil {
			return KanbanDoc{}, err
		}
	}
	for _, i := range doc.Items {
		if err := ValidKanbanItem(i); err != nil {
			return KanbanDoc{}, err
		}
	}
	if doc.Cards == nil {
		doc.Cards = []KanbanCard{}
	}
	return doc, nil
}

// kanbanTargetRe pins the entity target grammar.
var kanbanTargetRe = regexp.MustCompile(`^(row|col|card|todo):([A-Za-z0-9_-]{4,16})$`)

// AppendKanbanOp validates and appends one board op.
func (s *Store) AppendKanbanOp(ctx context.Context, driveID, nodeID string, op TargetOp, actor string) error {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return users.ErrNotFound
	}
	if !ValidHLC(op.HLC, actor) {
		return fmt.Errorf("bad clock")
	}
	m := kanbanTargetRe.FindStringSubmatch(op.T)
	if m == nil {
		return fmt.Errorf("bad op target")
	}
	if len(op.V) > kanbanMaxEntity {
		return fmt.Errorf("entity too large")
	}
	if string(op.V) == "null" || len(op.V) == 0 {
		return s.appendOp(ctx, driveID, nodeID, op.HLC, op)
	}
	var err error
	switch m[1] {
	case "row":
		var r KanbanRow
		if json.Unmarshal(op.V, &r) != nil || r.ID != m[2] {
			return fmt.Errorf("bad row")
		}
		err = ValidKanbanRow(r)
	case "col":
		var c KanbanCol
		if json.Unmarshal(op.V, &c) != nil || c.ID != m[2] {
			return fmt.Errorf("bad column")
		}
		err = ValidKanbanCol(c)
	case "card":
		var c KanbanCard
		if json.Unmarshal(op.V, &c) != nil || c.ID != m[2] {
			return fmt.Errorf("bad card")
		}
		err = ValidKanbanCard(c)
	case "todo":
		var i KanbanItem
		if json.Unmarshal(op.V, &i) != nil || i.ID != m[2] {
			return fmt.Errorf("bad item")
		}
		err = ValidKanbanItem(i)
	}
	if err != nil {
		return err
	}
	return s.appendOp(ctx, driveID, nodeID, op.HLC, op)
}

// KanbanState is what an opening editor loads.
type KanbanState struct {
	Doc       KanbanDoc
	Watermark string
	Ops       []TargetOp
}

// kanbanSnapshot is the between-compactions snapshot blob.
type kanbanSnapshot struct {
	Watermark string    `json:"watermark"`
	Doc       KanbanDoc `json:"doc"`
}

// LoadKanbanState reads snapshot + tail ops (file bytes seed the base
// when no snapshot exists yet).
func (s *Store) LoadKanbanState(ctx context.Context, driveID, nodeID string, node nodes.Node) (KanbanState, error) {
	state := KanbanState{}
	var snap kanbanSnapshot
	if s.loadSnapshot(ctx, driveID, nodeID, &snap) && snap.Doc.Format == KanbanFormat {
		state.Doc, state.Watermark = snap.Doc, snap.Watermark
	}
	if state.Doc.Format == "" {
		if node.BlobID != "" && node.Size > 0 && node.Size < kanbanMaxDoc {
			var fileRaw bytes.Buffer
			if err := s.DB.GetBlob(ctx, nodes.BlobKey(driveID, node.BlobID), &fileRaw); err == nil {
				if doc, err := ParseKanban(fileRaw.Bytes()); err == nil {
					state.Doc = doc
				}
			}
		}
		if state.Doc.Format == "" {
			state.Doc = NewKanban()
		}
	}
	var err error
	state.Ops, err = s.scanTargetOps(ctx, driveID, nodeID)
	return state, err
}

// kanbanIndex finds an entity's slice index by id (-1 = absent).
func kanbanIndex[T any](list []T, id string, idOf func(T) string) int {
	for i := range list {
		if idOf(list[i]) == id {
			return i
		}
	}
	return -1
}

// FoldKanbanOp applies one op to a board. Ops arrive HLC-ordered from
// the key scan, so plain last-write application IS highest-HLC-wins.
func FoldKanbanOp(doc *KanbanDoc, op TargetOp) {
	m := kanbanTargetRe.FindStringSubmatch(op.T)
	if m == nil {
		return
	}
	id := m[2]
	del := string(op.V) == "null" || len(op.V) == 0
	switch m[1] {
	case "row":
		idx := kanbanIndex(doc.Rows, id, func(r KanbanRow) string { return r.ID })
		if del {
			if idx >= 0 {
				doc.Rows = append(doc.Rows[:idx], doc.Rows[idx+1:]...)
			}
			return
		}
		var r KanbanRow
		if json.Unmarshal(op.V, &r) != nil || r.ID != id || ValidKanbanRow(r) != nil {
			return
		}
		if idx >= 0 {
			doc.Rows[idx] = r
		} else if len(doc.Rows) < kanbanMaxRows {
			doc.Rows = append(doc.Rows, r)
		}
	case "col":
		idx := kanbanIndex(doc.Cols, id, func(c KanbanCol) string { return c.ID })
		if del {
			if idx >= 0 {
				doc.Cols = append(doc.Cols[:idx], doc.Cols[idx+1:]...)
			}
			return
		}
		var c KanbanCol
		if json.Unmarshal(op.V, &c) != nil || c.ID != id || ValidKanbanCol(c) != nil {
			return
		}
		if idx >= 0 {
			doc.Cols[idx] = c
		} else if len(doc.Cols) < kanbanMaxCols {
			doc.Cols = append(doc.Cols, c)
		}
	case "card":
		idx := kanbanIndex(doc.Cards, id, func(c KanbanCard) string { return c.ID })
		if del {
			if idx >= 0 {
				doc.Cards = append(doc.Cards[:idx], doc.Cards[idx+1:]...)
			}
			return
		}
		var c KanbanCard
		if json.Unmarshal(op.V, &c) != nil || c.ID != id || ValidKanbanCard(c) != nil {
			return
		}
		if idx >= 0 {
			doc.Cards[idx] = c
		} else if len(doc.Cards) < kanbanMaxCards {
			doc.Cards = append(doc.Cards, c)
		}
	case "todo":
		idx := kanbanIndex(doc.Items, id, func(i KanbanItem) string { return i.ID })
		if del {
			if idx >= 0 {
				doc.Items = append(doc.Items[:idx], doc.Items[idx+1:]...)
			}
			return
		}
		var it KanbanItem
		if json.Unmarshal(op.V, &it) != nil || it.ID != id || ValidKanbanItem(it) != nil {
			return
		}
		if idx >= 0 {
			doc.Items[idx] = it
		} else if len(doc.Items) < kanbanMaxItems {
			doc.Items = append(doc.Items, it)
		}
	}
}

// SortKanban orders every list by (pos, id) — render order, and the
// deterministic on-disk order compaction writes.
func SortKanban(doc *KanbanDoc) {
	sort.SliceStable(doc.Rows, func(i, j int) bool {
		if doc.Rows[i].Pos != doc.Rows[j].Pos {
			return doc.Rows[i].Pos < doc.Rows[j].Pos
		}
		return doc.Rows[i].ID < doc.Rows[j].ID
	})
	sort.SliceStable(doc.Cols, func(i, j int) bool {
		if doc.Cols[i].Pos != doc.Cols[j].Pos {
			return doc.Cols[i].Pos < doc.Cols[j].Pos
		}
		return doc.Cols[i].ID < doc.Cols[j].ID
	})
	sort.SliceStable(doc.Cards, func(i, j int) bool {
		if doc.Cards[i].Pos != doc.Cards[j].Pos {
			return doc.Cards[i].Pos < doc.Cards[j].Pos
		}
		return doc.Cards[i].ID < doc.Cards[j].ID
	})
	sort.SliceStable(doc.Items, func(i, j int) bool {
		if doc.Items[i].Pos != doc.Items[j].Pos {
			return doc.Items[i].Pos < doc.Items[j].Pos
		}
		return doc.Items[i].ID < doc.Items[j].ID
	})
}

// foldKanbanState is the pure half of the board's compaction (sorted —
// the deterministic on-disk order).
func foldKanbanState(state KanbanState) (KanbanDoc, string) {
	watermark := state.Watermark
	for _, op := range state.Ops {
		FoldKanbanOp(&state.Doc, op)
		if op.HLC > watermark {
			watermark = op.HLC
		}
	}
	SortKanban(&state.Doc)
	return state.Doc, watermark
}
