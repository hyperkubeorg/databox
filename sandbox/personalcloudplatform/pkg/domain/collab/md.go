// md.go — the collaborative markdown editor's model. The FILE IS PLAIN
// MARKDOWN: unlike .pcdoc/.sheet there is no JSON document format —
// compaction writes ordinary .md bytes back as the file version, and
// any .md that arrived by upload opens straight into the editor.
//
// Collaboration needs stable identity that plain text can't carry, so
// line ids live only in the SNAPSHOT/OP space (never in the file):
//
//	ln:<lineID>  one line's text (JSON string, null = delete)
//	lines        the order: JSON array of line ids
//
// Per-line LWW on the target-op substrate: two people editing different
// lines merge cleanly; the same line resolves by HLC.
//
// Seeding: a bare file (no snapshot yet) splits into lines with
// DETERMINISTIC ids ("s" + padded index) — two editors that cold-open
// the same bytes derive identical ids and converge without
// coordination. Editor-inserted lines get random ids.
//
// A markdown file can also change OUTSIDE the editor (a new version
// uploaded over it). The snapshot records the file blob it derives
// from; on load, a mismatch discards the stale snapshot and pending
// ops (their seeded ids would collide with the fresh seed) and reseeds
// from the current bytes.
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

// Markdown format constants.
const (
	MDFormat      = "pcp-md/1"
	MDContentType = "text/markdown; charset=utf-8"
	mdMaxDoc      = 8 << 20
	mdMaxLines    = 20000
	mdMaxLine     = 16 << 10
)

// MDLine is one line of the document.
type MDLine struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// MDDoc is the folded in-memory document (snapshot form only — the
// file form is plain text).
type MDDoc struct {
	Format string   `json:"format"`
	Lines  []MDLine `json:"lines"`
}

// IsMDFile reports whether a node holds a markdown file.
func IsMDFile(n nodes.Node) bool {
	name := strings.ToLower(n.Name)
	return !n.IsDir && (strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".markdown"))
}

// ValidMDLineText gates one line: no newlines (a line IS the unit), no
// NUL, bounded length.
func ValidMDLineText(s string) error {
	if len(s) > mdMaxLine {
		return fmt.Errorf("line too long (16 KiB cap)")
	}
	if strings.ContainsAny(s, "\n\r\x00") {
		return fmt.Errorf("line contains control bytes")
	}
	return nil
}

// SeedMD splits raw markdown bytes into a document with deterministic
// line ids. CRLF and lone CR normalize to LF first, so the ids two
// cold-opening editors derive agree byte-for-byte.
func SeedMD(raw []byte) MDDoc {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	parts := strings.Split(text, "\n")
	if len(parts) > mdMaxLines {
		parts = parts[:mdMaxLines]
	}
	doc := MDDoc{Format: MDFormat, Lines: make([]MDLine, 0, len(parts))}
	for i, p := range parts {
		if len(p) > mdMaxLine {
			p = p[:mdMaxLine]
		}
		doc.Lines = append(doc.Lines, MDLine{ID: fmt.Sprintf("s%07d", i), Text: p})
	}
	return doc
}

// MDText is the file form: lines joined by LF. A trailing empty line
// round-trips as a conventional trailing newline.
func MDText(doc MDDoc) string {
	parts := make([]string, len(doc.Lines))
	for i, l := range doc.Lines {
		parts[i] = l.Text
	}
	return strings.Join(parts, "\n")
}

// MDDemo seeds a NEW markdown file: a working tour of everything the
// editor renders, ready to be deleted and typed over.
const MDDemo = `# Markdown demo

Every marker stays visible while you type — the styling wraps AROUND
the syntax. Delete all of this and write; it merges live with anyone
else in the file.

## Headings

Each depth gets its own neon block (the Neon toolbar button toggles
the style):

### Third level
#### Fourth level
##### Fifth level
###### Sixth level

## Emphasis

**bold**, *italic*, ~~strikethrough~~, and ` + "`inline code`" + `.
Select text and press Ctrl+B / Ctrl+I / Ctrl+E to wrap it.

## Links

The link text opens web URLs in a new window; the URL part stays
plain editable text: [databox on GitHub](https://github.com/hyperkubeorg/databox)

## Lists and quotes

- bullets
- more bullets
  - indented with two spaces (Tab inserts them)

1. numbered
2. lists too

> Blockquotes look like this.

## Code

Fenced blocks are syntax highlighted and get a Copy button in the
corner. Name a language after the backticks:

` + "```go" + `
// databox-style Go
func main() {
	fmt.Println("hello from the drive") // 42
}
` + "```" + `

` + "```js" + `
const answer = 6 * 7; /* the usual */
console.log("answer:", answer);
` + "```" + `

---

Toolbar: Wrap toggles word wrapping (the number is the wrap width in
characters), Nums toggles line numbers, and Ctrl+S saves right now.
`

// mdLineTargetRe pins the line target grammar (seeded + random ids).
var mdLineTargetRe = regexp.MustCompile(`^ln:([A-Za-z0-9_-]{4,16})$`)

// AppendMDOp validates and appends one markdown op.
func (s *Store) AppendMDOp(ctx context.Context, driveID, nodeID string, op TargetOp, actor string) error {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return users.ErrNotFound
	}
	if !ValidHLC(op.HLC, actor) {
		return fmt.Errorf("bad clock")
	}
	switch {
	case mdLineTargetRe.MatchString(op.T):
		if len(op.V) > mdMaxLine+1024 {
			return fmt.Errorf("line too long")
		}
		if string(op.V) != "null" && len(op.V) > 0 {
			var text string
			if json.Unmarshal(op.V, &text) != nil {
				return fmt.Errorf("bad line value")
			}
			if err := ValidMDLineText(text); err != nil {
				return err
			}
		}
	case op.T == "lines":
		if len(op.V) > 512<<10 {
			return fmt.Errorf("order list too large")
		}
	default:
		return fmt.Errorf("bad op target")
	}
	return s.appendOp(ctx, driveID, nodeID, op.HLC, op)
}

// FoldMDOp applies one op to a document. Line content that arrives
// before its order op appends at the end rather than vanish.
func FoldMDOp(doc *MDDoc, op TargetOp) {
	if m := mdLineTargetRe.FindStringSubmatch(op.T); m != nil {
		id := m[1]
		idx := -1
		for i := range doc.Lines {
			if doc.Lines[i].ID == id {
				idx = i
				break
			}
		}
		if string(op.V) == "null" || len(op.V) == 0 {
			if idx >= 0 {
				doc.Lines = append(doc.Lines[:idx], doc.Lines[idx+1:]...)
			}
			return
		}
		var text string
		if json.Unmarshal(op.V, &text) != nil || ValidMDLineText(text) != nil {
			return
		}
		if idx >= 0 {
			doc.Lines[idx].Text = text
		} else if len(doc.Lines) < mdMaxLines {
			doc.Lines = append(doc.Lines, MDLine{ID: id, Text: text})
		}
		return
	}
	if op.T == "lines" {
		var order []string
		if json.Unmarshal(op.V, &order) != nil || len(order) == 0 || len(order) > mdMaxLines {
			return
		}
		known := map[string]MDLine{}
		for _, l := range doc.Lines {
			known[l.ID] = l
		}
		next := make([]MDLine, 0, len(order))
		seen := map[string]bool{}
		for _, id := range order {
			if l, ok := known[id]; ok && !seen[id] {
				next = append(next, l)
				seen[id] = true
			}
		}
		for _, l := range doc.Lines { // content ahead of its order op stays
			if !seen[l.ID] {
				next = append(next, l)
			}
		}
		doc.Lines = next
	}
}

// MDState is what an opening editor loads.
type MDState struct {
	Doc       MDDoc
	Watermark string
	Ops       []TargetOp
}

// mdSnapshot is the between-compactions snapshot blob. Blob records the
// file version the fold derives from — a mismatch means the file was
// replaced outside the editor and the snapshot is stale.
type mdSnapshot struct {
	Watermark string `json:"watermark"`
	Blob      string `json:"blob"`
	Doc       MDDoc  `json:"doc"`
}

// LoadMDState reads snapshot + tail ops, seeding (and reseeding after
// an out-of-band file replacement) from the file bytes.
func (s *Store) LoadMDState(ctx context.Context, driveID, nodeID string, node nodes.Node) (MDState, error) {
	state := MDState{}
	stale := false
	var snap mdSnapshot
	if s.loadSnapshot(ctx, driveID, nodeID, &snap) && snap.Doc.Format == MDFormat {
		if snap.Blob == node.BlobID {
			state.Doc, state.Watermark = snap.Doc, snap.Watermark
		} else {
			stale = true // the file moved on without us — reseed below
		}
	}
	if state.Doc.Format == "" {
		if node.BlobID != "" && node.Size >= 0 && node.Size < mdMaxDoc {
			var fileRaw bytes.Buffer
			if err := s.DB.GetBlob(ctx, nodes.BlobKey(driveID, node.BlobID), &fileRaw); err == nil {
				state.Doc = SeedMD(fileRaw.Bytes())
			}
		}
		if state.Doc.Format == "" {
			state.Doc = SeedMD(nil)
		}
		if stale {
			// Stale ops carry ids from the OLD seed and would misapply
			// onto the new one — drop snapshot and log together.
			// Best-effort: a race just re-runs this on the next load.
			_ = s.DB.DeleteBlob(ctx, snapshotKey(driveID, nodeID))
			prefix := opsPrefix(driveID, nodeID)
			_ = s.DB.DeleteRange(ctx, prefix, kvx.PrefixEnd(prefix))
			return state, nil
		}
	}
	var err error
	state.Ops, err = s.scanTargetOps(ctx, driveID, nodeID)
	return state, err
}

// foldMDState is the pure half of the markdown compaction.
func foldMDState(state MDState) (MDDoc, string) {
	watermark := state.Watermark
	for _, op := range state.Ops {
		FoldMDOp(&state.Doc, op)
		if op.HLC > watermark {
			watermark = op.HLC
		}
	}
	return state.Doc, watermark
}
