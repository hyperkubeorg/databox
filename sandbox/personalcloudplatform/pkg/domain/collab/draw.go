// draw.go — the diagram editor's model: the pcp-draw/1 element format on
// the target-op substrate.
//
// A diagram is a Z-ORDERED list of ELEMENTS (shapes, connectors,
// freehand strokes, text). Elements are the merge granularity:
// concurrent edits to different elements both land; the same element is
// LWW. Targets:
//
//	el:<elementID>  one element's JSON (null = delete)
//	order           the z-order id list (JSON array of strings)
//	bg              the canvas background style (JSON string)
//
// Connectors (line/arrow) may BIND endpoints to shapes (SID/EID) and
// carry mid waypoints — the editor re-routes them when bound shapes
// move.
package collab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Draw format constants.
const (
	DrawFormat      = "pcp-draw/1"
	DrawExt         = ".pcdraw"
	DrawContentType = "application/x-pcp-draw+json"
	drawMaxDoc      = 8 << 20
	drawMaxEls      = 5000
	drawMaxEl       = 32 << 10
	drawMaxPts      = 2048
	drawMaxText     = 4 << 10
)

// DrawEl is one canvas element. Field names are short on purpose —
// diagrams hold thousands of these.
type DrawEl struct {
	ID     string       `json:"id"`
	Type   string       `json:"t"` // rect | ellipse | diamond | line | arrow | draw | text
	X      float64      `json:"x"`
	Y      float64      `json:"y"`
	W      float64      `json:"w,omitempty"`
	H      float64      `json:"h,omitempty"`
	Points [][2]float64 `json:"pts,omitempty"` // freehand points, relative to x/y
	Stroke string       `json:"stroke,omitempty"`
	Fill   string       `json:"fill,omitempty"`
	SW     float64      `json:"sw,omitempty"` // stroke width
	Text   string       `json:"text,omitempty"`
	FS     float64      `json:"fs,omitempty"`  // font size
	SID    string       `json:"sid,omitempty"` // start endpoint bound to this element
	EID    string       `json:"eid,omitempty"` // end endpoint bound to this element
	SA     string       `json:"sa,omitempty"`  // start anchor point on SID (n|s|e|w|corners)
	EA     string       `json:"ea,omitempty"`  // end anchor point on EID
	TS     string       `json:"ts,omitempty"`  // start terminator: none|arrow|x ("" = type default)
	TE     string       `json:"te,omitempty"`  // end terminator: none|arrow|x
	WP     [][2]float64 `json:"wp,omitempty"`  // connector waypoints, absolute
}

// drawTypes gates Element.Type.
var drawTypes = map[string]bool{
	"rect": true, "ellipse": true, "diamond": true,
	"line": true, "arrow": true, "draw": true, "text": true,
}

// drawColorRe allows CSS hex colors and a small set of keywords the
// editor uses ("none", "transparent").
var drawColorRe = regexp.MustCompile(`^(#[0-9a-fA-F]{3,8}|none|transparent)?$`)

// drawAnchorRe gates connector anchor names (empty = dynamic edge).
var drawAnchorRe = regexp.MustCompile(`^(n|s|e|w|nw|ne|sw|se)?$`)

// drawTermRe gates line terminators (empty = the type's default).
var drawTermRe = regexp.MustCompile(`^(none|arrow|x)?$`)

// drawBGs gates the canvas background style.
var drawBGs = map[string]bool{"": true, "dots": true, "grid": true, "lines": true, "none": true}

// ValidDrawEl is the shape gate the editor ops and the parser share.
func ValidDrawEl(e DrawEl) error {
	if !validEntityID(e.ID) {
		return fmt.Errorf("bad element id")
	}
	if !drawTypes[e.Type] {
		return fmt.Errorf("bad element type %q", e.Type)
	}
	if len(e.Points) > drawMaxPts || len(e.WP) > 64 {
		return fmt.Errorf("too many points")
	}
	if len(e.Text) > drawMaxText {
		return fmt.Errorf("text too long")
	}
	if !drawColorRe.MatchString(e.Stroke) || !drawColorRe.MatchString(e.Fill) {
		return fmt.Errorf("bad color")
	}
	if e.SW < 0 || e.SW > 40 || e.FS < 0 || e.FS > 200 {
		return fmt.Errorf("bad stroke width or font size")
	}
	if (e.SID != "" && !validEntityID(e.SID)) || (e.EID != "" && !validEntityID(e.EID)) {
		return fmt.Errorf("bad binding")
	}
	if !drawAnchorRe.MatchString(e.SA) || !drawAnchorRe.MatchString(e.EA) {
		return fmt.Errorf("bad anchor")
	}
	if !drawTermRe.MatchString(e.TS) || !drawTermRe.MatchString(e.TE) {
		return fmt.Errorf("bad terminator")
	}
	// Coordinates just need to be finite-ish; NaN/Inf don't survive JSON.
	for _, v := range []float64{e.X, e.Y, e.W, e.H} {
		if v < -1e7 || v > 1e7 {
			return fmt.Errorf("coordinate out of range")
		}
	}
	return nil
}

// DrawDoc is the whole diagram; Els order is z-order (first = back).
type DrawDoc struct {
	Format string   `json:"format"`
	Els    []DrawEl `json:"els"`
	BG     string   `json:"bg,omitempty"` // canvas background: dots|grid|lines|none
}

// NewDraw is an empty canvas.
func NewDraw() DrawDoc { return DrawDoc{Format: DrawFormat, Els: []DrawEl{}} }

// IsDrawFile reports whether a node holds a diagram.
func IsDrawFile(n nodes.Node) bool {
	return !n.IsDir && strings.HasSuffix(strings.ToLower(n.Name), DrawExt)
}

// ParseDraw decodes and shape-checks a diagram.
func ParseDraw(raw []byte) (DrawDoc, error) {
	if len(raw) > drawMaxDoc {
		return DrawDoc{}, fmt.Errorf("diagram too large")
	}
	var doc DrawDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return DrawDoc{}, err
	}
	if doc.Format != DrawFormat {
		return DrawDoc{}, fmt.Errorf("not a %s diagram", DrawFormat)
	}
	if len(doc.Els) > drawMaxEls {
		return DrawDoc{}, fmt.Errorf("too many elements")
	}
	if !drawBGs[doc.BG] {
		return DrawDoc{}, fmt.Errorf("bad background")
	}
	for _, e := range doc.Els {
		if err := ValidDrawEl(e); err != nil {
			return DrawDoc{}, err
		}
	}
	return doc, nil
}

// drawElTargetRe pins the element target grammar.
var drawElTargetRe = regexp.MustCompile(`^el:([A-Za-z0-9_-]{4,16})$`)

// AppendDrawOp validates and appends one diagram op.
func (s *Store) AppendDrawOp(ctx context.Context, driveID, nodeID string, op TargetOp, actor string) error {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return users.ErrNotFound
	}
	if !ValidHLC(op.HLC, actor) {
		return fmt.Errorf("bad clock")
	}
	switch {
	case drawElTargetRe.MatchString(op.T):
		if len(op.V) > drawMaxEl {
			return fmt.Errorf("element too large")
		}
		if string(op.V) != "null" && len(op.V) > 0 {
			var el DrawEl
			if err := json.Unmarshal(op.V, &el); err != nil {
				return fmt.Errorf("bad element")
			}
			if "el:"+el.ID != op.T {
				return fmt.Errorf("element id mismatch")
			}
			if err := ValidDrawEl(el); err != nil {
				return err
			}
		}
	case op.T == "order":
		if len(op.V) > 256<<10 {
			return fmt.Errorf("order list too large")
		}
	case op.T == "bg":
		var bg string
		if json.Unmarshal(op.V, &bg) != nil || !drawBGs[bg] {
			return fmt.Errorf("bad background")
		}
	default:
		return fmt.Errorf("bad op target")
	}
	return s.appendOp(ctx, driveID, nodeID, op.HLC, op)
}

// DrawState is what an opening editor loads.
type DrawState struct {
	Doc       DrawDoc
	Watermark string
	Ops       []TargetOp
}

// drawSnapshot is the between-compactions snapshot blob.
type drawSnapshot struct {
	Watermark string  `json:"watermark"`
	Doc       DrawDoc `json:"doc"`
}

// LoadDrawState reads snapshot + tail ops (file bytes seed the base when
// no snapshot exists yet).
func (s *Store) LoadDrawState(ctx context.Context, driveID, nodeID string, node nodes.Node) (DrawState, error) {
	state := DrawState{}
	var snap drawSnapshot
	if s.loadSnapshot(ctx, driveID, nodeID, &snap) && snap.Doc.Format == DrawFormat {
		state.Doc, state.Watermark = snap.Doc, snap.Watermark
	}
	if state.Doc.Format == "" {
		if node.BlobID != "" && node.Size > 0 && node.Size < drawMaxDoc {
			var fileRaw bytes.Buffer
			if err := s.DB.GetBlob(ctx, nodes.BlobKey(driveID, node.BlobID), &fileRaw); err == nil {
				if doc, err := ParseDraw(fileRaw.Bytes()); err == nil {
					state.Doc = doc
				}
			}
		}
		if state.Doc.Format == "" {
			state.Doc = NewDraw()
		}
	}
	var err error
	state.Ops, err = s.scanTargetOps(ctx, driveID, nodeID)
	return state, err
}

// FoldDrawOp applies one op to a diagram. Element content that arrives
// before its order op appends (z-top) rather than vanish.
func FoldDrawOp(doc *DrawDoc, op TargetOp) {
	if m := drawElTargetRe.FindStringSubmatch(op.T); m != nil {
		id := m[1]
		idx := -1
		for i := range doc.Els {
			if doc.Els[i].ID == id {
				idx = i
				break
			}
		}
		if string(op.V) == "null" || len(op.V) == 0 {
			if idx >= 0 {
				doc.Els = append(doc.Els[:idx], doc.Els[idx+1:]...)
			}
			return
		}
		var el DrawEl
		if json.Unmarshal(op.V, &el) != nil || el.ID != id || ValidDrawEl(el) != nil {
			return
		}
		if idx >= 0 {
			doc.Els[idx] = el
		} else if len(doc.Els) < drawMaxEls {
			doc.Els = append(doc.Els, el)
		}
		return
	}
	if op.T == "bg" {
		var bg string
		if json.Unmarshal(op.V, &bg) == nil && drawBGs[bg] {
			doc.BG = bg
		}
		return
	}
	if op.T == "order" {
		var order []string
		if json.Unmarshal(op.V, &order) != nil || len(order) == 0 || len(order) > drawMaxEls {
			return
		}
		known := map[string]DrawEl{}
		for _, e := range doc.Els {
			known[e.ID] = e
		}
		next := make([]DrawEl, 0, len(order))
		seen := map[string]bool{}
		for _, id := range order {
			if e, ok := known[id]; ok && !seen[id] {
				next = append(next, e)
				seen[id] = true
			}
		}
		// Elements that arrived ahead of their order op stay on top.
		for _, e := range doc.Els {
			if !seen[e.ID] {
				next = append(next, e)
			}
		}
		doc.Els = next
	}
}

// foldDrawState is the pure half of the diagram's compaction.
func foldDrawState(state DrawState) (DrawDoc, string) {
	watermark := state.Watermark
	for _, op := range state.Ops {
		FoldDrawOp(&state.Doc, op)
		if op.HLC > watermark {
			watermark = op.HLC
		}
	}
	return state.Doc, watermark
}
