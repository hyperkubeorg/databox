// collab.go — the collaborative-document HTTP surface (spec §2/§6),
// ported from PCD onto the Drive mount. One generic handler set serves
// every target-op editor through a docKind descriptor; the CSV sheet
// (whose ops are (row, col, value) rather than targets) keeps its own
// two handlers. The live op fan-out rides /drive/events?doc=1
// (events.go).
//
//	GET  /drive/{type}/{drive}/{node}/state   folded doc + tail ops (+?rev=
//	                                          → that version, read-only)
//	POST /drive/{type}/{drive}/{node}/ops     append ops (editor; X-CSRF)
//	POST /drive/{type}/{drive}/{node}/close   editor left → compact + save-back
//	POST /drive/doc/{drive}/{node}/presence   cursor heartbeat (all editors)
//	POST /drive/do/new{sheet,doc,draw,kanban,md}  create → straight into the editor
//	POST /drive/do/importcsv                  CSV file → sibling .sheet
package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// docKind describes one target-op editor's document type to the shared
// state/ops handlers. The calendar (.pccal, calDoc below) registers
// state/close only — its ops live in the Calendar app, which owns the
// invite fan-out.
type docKind struct {
	check func(nodes.Node) bool
	state func(ctx context.Context, s *collab.Store, driveID, nodeID string, node nodes.Node) (map[string]any, error)
	// append is a method expression on *collab.Store (receiver first).
	append   func(s *collab.Store, ctx context.Context, driveID, nodeID string, op collab.TargetOp, actor string) error
	revParse func(raw []byte) (any, error)
	// bodyCap bounds one POSTed op batch.
	bodyCap int64
}

var gridDoc = docKind{
	check: collab.IsGridFile,
	state: func(ctx context.Context, s *collab.Store, driveID, nodeID string, node nodes.Node) (map[string]any, error) {
		st, err := s.LoadGridState(ctx, driveID, nodeID, node)
		if err != nil {
			return nil, err
		}
		return map[string]any{"doc": st.Doc, "ops": st.Ops,
			"maxRows": collab.MaxSheetRows, "maxCols": collab.MaxSheetCols}, nil
	},
	append:   (*collab.Store).AppendGridOp,
	revParse: func(raw []byte) (any, error) { return collab.ParseGridDoc(raw) },
	bodyCap:  2 << 20,
}

var wdocDoc = docKind{
	check: collab.IsWDocFile,
	state: func(ctx context.Context, s *collab.Store, driveID, nodeID string, node nodes.Node) (map[string]any, error) {
		st, err := s.LoadWDocState(ctx, driveID, nodeID, node)
		if err != nil {
			return nil, err
		}
		return map[string]any{"doc": st.Doc, "ops": st.Ops}, nil
	},
	append:   (*collab.Store).AppendWDocOp,
	revParse: func(raw []byte) (any, error) { return collab.ParseWDoc(raw) },
	bodyCap:  4 << 20,
}

var kanbanDoc = docKind{
	check: collab.IsKanbanFile,
	state: func(ctx context.Context, s *collab.Store, driveID, nodeID string, node nodes.Node) (map[string]any, error) {
		st, err := s.LoadKanbanState(ctx, driveID, nodeID, node)
		if err != nil {
			return nil, err
		}
		return map[string]any{"doc": st.Doc, "ops": st.Ops}, nil
	},
	append:   (*collab.Store).AppendKanbanOp,
	revParse: func(raw []byte) (any, error) { return collab.ParseKanban(raw) },
	bodyCap:  4 << 20,
}

var drawDoc = docKind{
	check: collab.IsDrawFile,
	state: func(ctx context.Context, s *collab.Store, driveID, nodeID string, node nodes.Node) (map[string]any, error) {
		st, err := s.LoadDrawState(ctx, driveID, nodeID, node)
		if err != nil {
			return nil, err
		}
		return map[string]any{"doc": st.Doc, "ops": st.Ops}, nil
	},
	append:   (*collab.Store).AppendDrawOp,
	revParse: func(raw []byte) (any, error) { return collab.ParseDraw(raw) },
	bodyCap:  4 << 20,
}

var mdDoc = docKind{
	check: collab.IsMDFile,
	state: func(ctx context.Context, s *collab.Store, driveID, nodeID string, node nodes.Node) (map[string]any, error) {
		st, err := s.LoadMDState(ctx, driveID, nodeID, node)
		if err != nil {
			return nil, err
		}
		return map[string]any{"doc": st.Doc, "ops": st.Ops}, nil
	},
	append: (*collab.Store).AppendMDOp,
	// markdown versions are plain text — seeding IS the parse
	revParse: func(raw []byte) (any, error) { return collab.SeedMD(raw), nil },
	bodyCap:  4 << 20,
}

// calDoc is the calendar's read surface on the app host: /state feeds
// the agenda module read-only; edits happen in the Calendar app (its
// ops endpoint owns the notification/ICS fan-out, so the generic ops
// handler is deliberately NOT registered for this kind).
var calDoc = docKind{
	check: collab.IsCalFile,
	state: func(ctx context.Context, s *collab.Store, driveID, nodeID string, node nodes.Node) (map[string]any, error) {
		st, err := s.LoadCalState(ctx, driveID, nodeID, node)
		if err != nil {
			return nil, err
		}
		doc := st.Doc
		for _, op := range st.Ops {
			collab.FoldCalOp(&doc, op)
		}
		return map[string]any{"doc": doc, "ops": []collab.TargetOp{}}, nil
	},
	revParse: func(raw []byte) (any, error) { return collab.ParseCalDoc(raw) },
	bodyCap:  1 << 20,
}

// docNode resolves and authorizes the document's file node.
func (h *handlers) docNode(r *http.Request, user users.User, minRole string) (string, string, nodes.Node, bool) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	if _, err := h.access(r, user, driveID, nodeID, minRole); err != nil {
		return "", "", nodes.Node{}, false
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || node.IsDir {
		return "", "", nodes.Node{}, false
	}
	return driveID, nodeID, node, true
}

// serveRevState answers a /state request for one revision: the parsed
// doc straight from its immutable blob, an empty op tail, no presence.
// The app host mounts the editor read-only over it with a restore
// banner, so "what am I restoring?" is answered by looking at it.
func (h *handlers) serveRevState(w http.ResponseWriter, r *http.Request, driveID, nodeID, rev string, parse func([]byte) (any, error)) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	v, found, err := h.k.Nodes.GetVersion(cctx, driveID, nodeID, rev)
	if err != nil || !found || v.Size > 16<<20 {
		kernel.JSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown version"})
		return
	}
	var buf bytes.Buffer
	if err := h.k.Nodes.DB.GetBlob(cctx, nodes.BlobKey(driveID, v.BlobID), &buf); err != nil {
		kernel.JSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown version"})
		return
	}
	doc, err := parse(buf.Bytes())
	if err != nil {
		kernel.JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "that version does not parse as this document type"})
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "doc": doc, "ops": []collab.TargetOp{}, "rev": rev})
}

// docState loads what an opening editor needs (?rev= previews an old
// version read-only).
func (h *handlers) docState(k docKind) func(http.ResponseWriter, *http.Request, users.Session, users.User) {
	return func(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
		driveID, nodeID, node, ok := h.docNode(r, user, drives.RoleViewer)
		if !ok || !k.check(node) {
			http.NotFound(w, r)
			return
		}
		if rev := r.URL.Query().Get("rev"); rev != "" {
			h.serveRevState(w, r, driveID, nodeID, rev, k.revParse)
			return
		}
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		out, err := k.state(cctx, h.k.Collab, driveID, nodeID, node)
		if err != nil {
			kernel.JSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "state load failed"})
			return
		}
		presence, _ := h.k.Collab.ListPresence(cctx, driveID, nodeID)
		out["ok"], out["presence"] = true, presence
		kernel.JSON(w, http.StatusOK, out)
	}
}

// docOpCounter spreads out compaction checks (every 32nd append batch,
// plus every editor close).
var docOpCounter int64

// docOps appends a batch of target ops for the signed-in editor.
func (h *handlers) docOps(k docKind) func(http.ResponseWriter, *http.Request, users.Session, users.User) {
	return func(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
		if r.Header.Get("X-CSRF") != sess.CSRF {
			http.Error(w, "bad csrf token", http.StatusForbidden)
			return
		}
		driveID, nodeID, node, ok := h.docNode(r, user, drives.RoleEditor)
		if !ok || !k.check(node) {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Ops []collab.TargetOp `json:"ops"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, k.bodyCap)).Decode(&body); err != nil || len(body.Ops) == 0 || len(body.Ops) > 256 {
			kernel.JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad op batch"})
			return
		}
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		for _, op := range body.Ops {
			if err := k.append(h.k.Collab, cctx, driveID, nodeID, op, user.Username); err != nil {
				kernel.JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": kernel.UserErr(err)})
				return
			}
		}
		kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
		// Opportunistic compaction: every Nth batch, fold in the
		// background (the databox lock keeps concurrent folders to one).
		if atomic.AddInt64(&docOpCounter, 1)%collab.CompactEvery == 0 {
			go h.compactDoc(driveID, nodeID, user.Username)
		}
	}
}

// docClose is the editor's parting beacon: compact so the file blob
// reflects the session's edits — the save-back that makes the document
// downloadable as real CSV/JSON/markdown — without waiting for the op
// threshold.
func (h *handlers) docClose(check func(nodes.Node) bool) func(http.ResponseWriter, *http.Request, users.Session, users.User) {
	return func(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
		driveID, nodeID, node, ok := h.docNode(r, user, drives.RoleEditor)
		if !ok || !check(node) {
			http.NotFound(w, r)
			return
		}
		go h.compactDoc(driveID, nodeID, user.Username)
		w.WriteHeader(http.StatusNoContent)
	}
}

// compactDoc runs one compaction pass with its own context (the request
// that triggered it has moved on).
func (h *handlers) compactDoc(driveID, nodeID, by string) {
	cctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := h.k.Collab.Compact(cctx, driveID, nodeID, by); err != nil {
		h.k.Log.Warn("doc compaction failed", "drive", driveID, "node", nodeID, "err", err)
	}
}

// docPresence records a cursor heartbeat (always 204 — soft-fail).
// Shared by every editor type.
func (h *handlers) docPresence(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	driveID, nodeID, _, ok := h.docNode(r, user, drives.RoleViewer)
	if !ok {
		http.NotFound(w, r)
		return
	}
	row, _ := strconv.Atoi(r.FormValue("row"))
	col, _ := strconv.Atoi(r.FormValue("col"))
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	_ = h.k.Collab.SetPresence(cctx, driveID, nodeID, user.Username, row, col)
	w.WriteHeader(http.StatusNoContent)
}

// --- the CSV sheet (row/col ops, not targets) --------------------------------------

// sheetNode resolves and authorizes a .csv/.tsv file.
func (h *handlers) sheetNode(r *http.Request, user users.User, minRole string) (string, string, nodes.Node, bool) {
	driveID, nodeID, node, ok := h.docNode(r, user, minRole)
	if !ok || !collab.IsSheetFile(node) {
		return "", "", nodes.Node{}, false
	}
	return driveID, nodeID, node, true
}

// sheetState loads what an opening CSV editor needs.
func (h *handlers) sheetState(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	driveID, nodeID, node, ok := h.sheetNode(r, user, drives.RoleViewer)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	state, err := h.k.Collab.LoadDocState(cctx, driveID, nodeID, node)
	if err != nil {
		kernel.JSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "state load failed"})
		return
	}
	presence, _ := h.k.Collab.ListPresence(cctx, driveID, nodeID)
	kernel.JSON(w, http.StatusOK, map[string]any{
		"ok": true, "snapshot": state.Snapshot, "ops": state.Ops, "presence": presence,
		"maxRows": collab.MaxSheetRows, "maxCols": collab.MaxSheetCols,
	})
}

// sheetOps appends a batch of cell ops.
func (h *handlers) sheetOps(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if r.Header.Get("X-CSRF") != sess.CSRF {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID, _, ok := h.sheetNode(r, user, drives.RoleEditor)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Ops []collab.Op `json:"ops"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || len(body.Ops) == 0 || len(body.Ops) > 256 {
		kernel.JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad op batch"})
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	for _, op := range body.Ops {
		if err := h.k.Collab.AppendOp(cctx, driveID, nodeID, op, user.Username); err != nil {
			kernel.JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": kernel.UserErr(err)})
			return
		}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
	if atomic.AddInt64(&docOpCounter, 1)%collab.CompactEvery == 0 {
		go h.compactDoc(driveID, nodeID, user.Username)
	}
}

// --- creation ----------------------------------------------------------------------

// newSpec describes one "New document" action: the default name, the
// enforced extension set (first is appended when missing), the app that
// opens the result, and the starter content.
type newSpec struct {
	defName string
	exts    []string
	app     string
	content func() ([]byte, string)
}

var (
	newSheetSpec = newSpec{"Untitled spreadsheet", []string{collab.GridExt, ".pcgrid"}, "grid",
		func() ([]byte, string) {
			raw, _ := json.Marshal(collab.NewGridDoc())
			return raw, collab.GridContentType
		}}
	newDocSpec = newSpec{"Untitled document", []string{collab.WDocExt}, "writer",
		func() ([]byte, string) { raw, _ := json.Marshal(collab.NewWDoc()); return raw, collab.WDocContentType }}
	newDrawSpec = newSpec{"Untitled diagram", []string{collab.DrawExt}, "draw",
		func() ([]byte, string) { raw, _ := json.Marshal(collab.NewDraw()); return raw, collab.DrawContentType }}
	newKanbanSpec = newSpec{"Untitled board", []string{collab.KanbanExt}, "kanban",
		func() ([]byte, string) {
			raw, _ := json.Marshal(collab.NewKanban())
			return raw, collab.KanbanContentType
		}}
	// New markdown files seed with the feature-tour demo — a live example
	// beats an empty page, and deleting it is one Ctrl+A away.
	newMDSpec = newSpec{"Untitled", []string{".md", ".markdown"}, "md",
		func() ([]byte, string) { return []byte(collab.MDDemo), collab.MDContentType }}
)

// doNew creates a starter document in a folder (editor+, quota charged
// like any upload) and sends the member straight into its editor.
func (h *handlers) doNew(spec newSpec) func(http.ResponseWriter, *http.Request, users.Session, users.User) {
	return func(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
		if !kernel.CheckCSRF(r, sess) {
			http.Error(w, "bad csrf token", http.StatusForbidden)
			return
		}
		driveID, parentID := r.FormValue("drive"), r.FormValue("parent")
		back := backTo(r, driveID)
		if _, err := h.access(r, user, driveID, parentID, drives.RoleEditor); err != nil {
			h.k.Respond(w, r, back, err, nil)
			return
		}
		name := strings.TrimSpace(r.FormValue("name"))
		if name == "" {
			name = spec.defName
		}
		hasExt := false
		for _, ext := range spec.exts {
			if strings.HasSuffix(strings.ToLower(name), ext) {
				hasExt = true
				break
			}
		}
		if !hasExt {
			name += spec.exts[0]
		}
		raw, contentType := spec.content()
		node, err := h.storeDoc(r, user, driveID, parentID, name, raw, contentType)
		if err != nil {
			h.k.Respond(w, r, back, err, nil)
			return
		}
		openURL := "/drive/app/" + spec.app + "?drive=" + driveID + "&node=" + node.ID
		h.k.Respond(w, r, openURL, nil, map[string]any{"id": node.ID, "open": openURL})
	}
}

// storeDoc writes a document blob and commits its file node, charging
// quota — creations are uploads; only compaction save-backs are free.
func (h *handlers) storeDoc(r *http.Request, user users.User, driveID, parentID, name string, raw []byte, contentType string) (nodes.Node, error) {
	blobID := kvx.NewID()
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Nodes.DB.PutBlob(cctx, nodes.BlobKey(driveID, blobID), bytes.NewReader(raw), contentType); err != nil {
		return nodes.Node{}, err
	}
	return h.k.Nodes.CommitStored(cctx, driveID, parentID, name, blobID, contentType, int64(len(raw)), h.uploadQuota(r, user), user.Username)
}

// doImportCSV converts a CSV file into a sibling .sheet document
// (literals only — formulas and styles are the editor's job).
func (h *handlers) doImportCSV(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID := r.FormValue("drive"), r.FormValue("node")
	back := backTo(r, driveID)
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || node.IsDir {
		h.k.Respond(w, r, back, users.ErrNotFound, nil)
		return
	}
	if node.Size > 8<<20 {
		h.k.Respond(w, r, back, errCSVTooLarge, nil)
		return
	}
	var raw bytes.Buffer
	if err := h.k.Nodes.DB.GetBlob(cctx, nodes.BlobKey(driveID, node.BlobID), &raw); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	base := strings.TrimSuffix(node.Name, ".csv")
	base = strings.TrimSuffix(base, ".tsv")
	doc, err := collab.GridFromCSV(raw.Bytes(), base)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	parent := nodes.RootID
	if crumbs, err := h.k.Nodes.Path(cctx, driveID, nodeID); err == nil && len(crumbs) >= 2 {
		parent = crumbs[len(crumbs)-2].ID
	}
	docRaw, _ := json.Marshal(doc)
	created, err := h.storeDoc(r, user, driveID, parent, base+collab.GridExt, docRaw, collab.GridContentType)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	openURL := "/drive/app/grid?drive=" + driveID + "&node=" + created.ID
	h.k.Respond(w, r, openURL, nil, map[string]any{"id": created.ID, "open": openURL})
}

// errCSVTooLarge keeps the conversion cap message in one place.
var errCSVTooLarge = csvTooLargeError{}

type csvTooLargeError struct{}

func (csvTooLargeError) Error() string { return "that CSV is too large to convert (8 MiB cap)" }
