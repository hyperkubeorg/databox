// wdoc.go — the document writer's model: the pcp-doc/1 block format on
// the target-op substrate.
//
// A document is an ordered list of BLOCKS (paragraph-level HTML
// fragments with stable random ids). Blocks are the merge granularity:
// concurrent edits to different paragraphs both land; the same
// paragraph is LWW. Targets:
//
//	bl:<blockID>   one block's HTML (JSON string; null = delete)
//	blocks         the id order (JSON array of strings)
//	header|footer  single fragments rendered at the page edges
//	page           the page setup (size/orientation/margins)
//
// The server treats block HTML as OPAQUE bytes under size caps — the
// whitelist sanitizer lives in the editor, the only render path (raw
// file downloads are attachments; the HTML export ships with a
// no-scripts CSP).
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

// WDoc format constants.
const (
	WDocFormat      = "pcp-doc/1"
	WDocExt         = ".pcdoc"
	WDocContentType = "application/x-pcp-doc+json"
	wdocMaxDoc      = 8 << 20
	wdocMaxBlocks   = 5000
	wdocMaxBlock    = 64 << 10
)

// WDocBlock is one paragraph-level fragment.
type WDocBlock struct {
	ID   string `json:"id"`
	HTML string `json:"html"`
}

// WDocPage is the document's page setup. Margins in centimeters; zero
// values read as the defaults.
type WDocPage struct {
	Size   string  `json:"size,omitempty"`   // "letter" | "a4" | "legal"
	Orient string  `json:"orient,omitempty"` // "portrait" | "landscape"
	MT     float64 `json:"mt,omitempty"`
	MR     float64 `json:"mr,omitempty"`
	MB     float64 `json:"mb,omitempty"`
	ML     float64 `json:"ml,omitempty"`
}

// ValidWDocPage gates a page-setup op.
func ValidWDocPage(p WDocPage) bool {
	switch p.Size {
	case "", "letter", "a4", "legal":
	default:
		return false
	}
	switch p.Orient {
	case "", "portrait", "landscape":
	default:
		return false
	}
	for _, m := range []float64{p.MT, p.MR, p.MB, p.ML} {
		if m < 0 || m > 10 {
			return false
		}
	}
	return true
}

// WDocDoc is the whole document.
type WDocDoc struct {
	Format string      `json:"format"`
	Blocks []WDocBlock `json:"blocks"`
	Page   WDocPage    `json:"page,omitzero"`
	// Header and Footer are single sanitized fragments rendered at the
	// page's top and bottom (same whitelist as blocks).
	Header string `json:"header,omitempty"`
	Footer string `json:"footer,omitempty"`
}

// NewWDoc is an empty document: one empty paragraph.
func NewWDoc() WDocDoc {
	return WDocDoc{Format: WDocFormat, Blocks: []WDocBlock{{ID: kvx.NewID()[:8], HTML: "<p></p>"}}}
}

// IsWDocFile reports whether a node holds a writer document.
func IsWDocFile(n nodes.Node) bool {
	return !n.IsDir && strings.HasSuffix(strings.ToLower(n.Name), WDocExt)
}

// ParseWDoc decodes and shape-checks a document.
func ParseWDoc(raw []byte) (WDocDoc, error) {
	if len(raw) > wdocMaxDoc {
		return WDocDoc{}, fmt.Errorf("document too large")
	}
	var doc WDocDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return WDocDoc{}, err
	}
	if doc.Format != WDocFormat {
		return WDocDoc{}, fmt.Errorf("not a %s document", WDocFormat)
	}
	if len(doc.Blocks) == 0 || len(doc.Blocks) > wdocMaxBlocks {
		return WDocDoc{}, fmt.Errorf("a document has 1–%d blocks", wdocMaxBlocks)
	}
	for _, b := range doc.Blocks {
		if !validEntityID(b.ID) {
			return WDocDoc{}, fmt.Errorf("bad block id")
		}
		if len(b.HTML) > wdocMaxBlock {
			return WDocDoc{}, fmt.Errorf("block too large")
		}
	}
	return doc, nil
}

// wdocBlockTargetRe pins the block target grammar (same id alphabet as
// every other entity id).
var wdocBlockTargetRe = regexp.MustCompile(`^bl:([A-Za-z0-9_-]{4,16})$`)

// AppendWDocOp validates and appends one writer op.
func (s *Store) AppendWDocOp(ctx context.Context, driveID, nodeID string, op TargetOp, actor string) error {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return users.ErrNotFound
	}
	if !ValidHLC(op.HLC, actor) {
		return fmt.Errorf("bad clock")
	}
	switch {
	case wdocBlockTargetRe.MatchString(op.T):
		if len(op.V) > wdocMaxBlock {
			return fmt.Errorf("block too large")
		}
	case op.T == "blocks":
		if len(op.V) > 256<<10 {
			return fmt.Errorf("order list too large")
		}
	case op.T == "header" || op.T == "footer":
		if len(op.V) > wdocMaxBlock {
			return fmt.Errorf("fragment too large")
		}
	case op.T == "page":
		var p WDocPage
		if err := json.Unmarshal(op.V, &p); err != nil || !ValidWDocPage(p) {
			return fmt.Errorf("bad page setup")
		}
	default:
		return fmt.Errorf("bad op target")
	}
	return s.appendOp(ctx, driveID, nodeID, op.HLC, op)
}

// WDocState is what an opening editor loads.
type WDocState struct {
	Doc       WDocDoc
	Watermark string
	Ops       []TargetOp
}

// wdocSnapshot is the between-compactions snapshot blob.
type wdocSnapshot struct {
	Watermark string  `json:"watermark"`
	Doc       WDocDoc `json:"doc"`
}

// LoadWDocState reads snapshot + tail ops (file bytes seed the base when
// no snapshot exists yet).
func (s *Store) LoadWDocState(ctx context.Context, driveID, nodeID string, node nodes.Node) (WDocState, error) {
	state := WDocState{}
	var snap wdocSnapshot
	if s.loadSnapshot(ctx, driveID, nodeID, &snap) && snap.Doc.Format == WDocFormat {
		state.Doc, state.Watermark = snap.Doc, snap.Watermark
	}
	if state.Doc.Format == "" {
		if node.BlobID != "" && node.Size > 0 && node.Size < wdocMaxDoc {
			var fileRaw bytes.Buffer
			if err := s.DB.GetBlob(ctx, nodes.BlobKey(driveID, node.BlobID), &fileRaw); err == nil {
				if doc, err := ParseWDoc(fileRaw.Bytes()); err == nil {
					state.Doc = doc
				}
			}
		}
		if state.Doc.Format == "" {
			state.Doc = NewWDoc()
		}
	}
	var err error
	state.Ops, err = s.scanTargetOps(ctx, driveID, nodeID)
	return state, err
}

// FoldWDocOp applies one op to a document. Content ops for blocks the
// order list doesn't (yet) know APPEND rather than vanish — the order op
// may simply not have arrived; a later "blocks" op places them.
func FoldWDocOp(doc *WDocDoc, op TargetOp) {
	if m := wdocBlockTargetRe.FindStringSubmatch(op.T); m != nil {
		id := m[1]
		var html *string
		if err := json.Unmarshal(op.V, &html); err != nil {
			return
		}
		idx := -1
		for i := range doc.Blocks {
			if doc.Blocks[i].ID == id {
				idx = i
				break
			}
		}
		if html == nil || *html == "" {
			if idx >= 0 && len(doc.Blocks) > 1 {
				doc.Blocks = append(doc.Blocks[:idx], doc.Blocks[idx+1:]...)
			}
			return
		}
		if len(*html) > wdocMaxBlock {
			return
		}
		if idx >= 0 {
			doc.Blocks[idx].HTML = *html
		} else if len(doc.Blocks) < wdocMaxBlocks {
			doc.Blocks = append(doc.Blocks, WDocBlock{ID: id, HTML: *html})
		}
		return
	}
	if op.T == "header" || op.T == "footer" {
		var html string
		if json.Unmarshal(op.V, &html) != nil || len(html) > wdocMaxBlock {
			return
		}
		if op.T == "header" {
			doc.Header = html
		} else {
			doc.Footer = html
		}
		return
	}
	if op.T == "page" {
		var p WDocPage
		if json.Unmarshal(op.V, &p) == nil && ValidWDocPage(p) {
			doc.Page = p
		}
		return
	}
	if op.T == "blocks" {
		var order []string
		if json.Unmarshal(op.V, &order) != nil || len(order) == 0 || len(order) > wdocMaxBlocks {
			return
		}
		known := map[string]WDocBlock{}
		for _, b := range doc.Blocks {
			known[b.ID] = b
		}
		next := make([]WDocBlock, 0, len(order))
		seen := map[string]bool{}
		for _, id := range order {
			if b, ok := known[id]; ok && !seen[id] {
				next = append(next, b)
				seen[id] = true
			}
		}
		// Content that arrived ahead of its order op survives at the end.
		for _, b := range doc.Blocks {
			if !seen[b.ID] {
				next = append(next, b)
			}
		}
		if len(next) > 0 {
			doc.Blocks = next
		}
	}
}

// foldWDocState is the pure half of the writer's compaction.
func foldWDocState(state WDocState) (WDocDoc, string) {
	watermark := state.Watermark
	for _, op := range state.Ops {
		FoldWDocOp(&state.Doc, op)
		if op.HLC > watermark {
			watermark = op.HLC
		}
	}
	return state.Doc, watermark
}

// LoadWDoc reads a .pcdoc file's CURRENT folded state (snapshot base +
// unfolded ops) — the export path.
func (s *Store) LoadWDoc(ctx context.Context, driveID, nodeID string, node nodes.Node) (WDocDoc, error) {
	state, err := s.LoadWDocState(ctx, driveID, nodeID, node)
	if err != nil {
		return WDocDoc{}, err
	}
	doc, _ := foldWDocState(state)
	return doc, nil
}

// WDocText derives the plain-text form: tags stripped, block breaks
// preserved, the few common entities decoded. Good enough for a .txt
// export; the HTML form is the faithful one.
func WDocText(doc WDocDoc) string {
	var b strings.Builder
	for _, blk := range doc.Blocks {
		b.WriteString(stripTags(blk.HTML))
		b.WriteString("\n")
	}
	return b.String()
}

var (
	tagRe   = regexp.MustCompile(`<[^>]*>`)
	brRe    = regexp.MustCompile(`(?i)<br\s*/?>`)
	endLIRe = regexp.MustCompile(`(?i)</li>`)
)

// stripTags flattens one block's HTML to text. <br> and </li> become
// line breaks first so lists and manual breaks survive.
func stripTags(html string) string {
	html = brRe.ReplaceAllString(html, "\n")
	html = endLIRe.ReplaceAllString(html, "\n")
	html = tagRe.ReplaceAllString(html, "")
	r := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&nbsp;", " ")
	return strings.TrimRight(r.Replace(html), "\n")
}

// WDocPageWidthCM is the page's physical width in centimeters (the
// editor and the HTML export share it).
func WDocPageWidthCM(p WDocPage) float64 {
	w, h := 21.59, 27.94 // letter
	switch p.Size {
	case "a4":
		w, h = 21.0, 29.7
	case "legal":
		w, h = 21.59, 35.56
	}
	if p.Orient == "landscape" {
		return h
	}
	return w
}
