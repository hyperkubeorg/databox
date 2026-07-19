// export.go — exports as CONVERSIONS. Every GET /drive/export endpoint
// streams a download; its POST twin lands the same bytes as a sibling
// file in the drive instead. Saving an export is how a member converts
// a document (.pcdoc → .html, .sheet → .xlsx, diagram → .svg) without
// ever round-tripping through their own machine. Markdown has no export
// twins — the file IS plain .md; Download in the host header serves the
// real thing.
//
// Re-saving the same export overwrites by name, which versions the
// previous copy through the normal CommitFile path.
package drive

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// exportSaveLimit caps a saved export body (client-generated PNG/SVG).
const exportSaveLimit = 32 << 20

// saveExportSibling writes raw as a file next to the source node and
// answers JSON. The caller has already authorized RoleEditor. Quota is
// charged — a saved export is a new file the member asked for.
func (h *handlers) saveExportSibling(w http.ResponseWriter, r *http.Request, user users.User, driveID, nodeID, name, contentType string, raw []byte) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	parent := nodes.RootID
	if crumbs, err := h.k.Nodes.Path(cctx, driveID, nodeID); err == nil && len(crumbs) >= 2 {
		parent = crumbs[len(crumbs)-2].ID
	}
	node, err := h.storeDoc(r, user, driveID, parent, name, raw, contentType)
	if err != nil {
		kernel.JSON(w, kernel.ErrStatus(err), map[string]any{"ok": false, "error": kernel.UserErr(err)})
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "id": node.ID, "name": node.Name})
}

// exportCSRF gates the POST export twins: fetch()-style X-CSRF header or
// a form token both pass.
func exportCSRF(r *http.Request, sess users.Session) bool {
	return r.Header.Get("X-CSRF") == sess.CSRF || kernel.CheckCSRF(r, sess)
}

// --- grid exports ------------------------------------------------------------------

// loadGridFor is the shared front half of every grid export.
func (h *handlers) loadGridFor(w http.ResponseWriter, r *http.Request, user users.User, minRole string) (string, string, string, collab.GridDoc, bool) {
	driveID, nodeID, node, ok := h.docNode(r, user, minRole)
	if !ok || !collab.IsGridFile(node) {
		http.NotFound(w, r)
		return "", "", "", collab.GridDoc{}, false
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	doc, err := h.k.Collab.LoadGridDoc(cctx, driveID, nodeID, node)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return "", "", "", collab.GridDoc{}, false
	}
	return driveID, nodeID, strings.TrimSuffix(node.Name, collab.GridExt), doc, true
}

// pickSheet resolves the ?sheet= query (default: the first sheet).
func pickSheet(r *http.Request, doc collab.GridDoc) (collab.GridSheet, bool) {
	if want := r.URL.Query().Get("sheet"); want != "" {
		for _, sh := range doc.Sheets {
			if sh.ID == want {
				return sh, true
			}
		}
		return collab.GridSheet{}, false
	}
	return doc.Sheets[0], true
}

// exportCSV streams one sheet's computed values as CSV. ?sheet= names a
// sheet id (default: the first).
func (h *handlers) exportCSV(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	_, _, base, doc, ok := h.loadGridFor(w, r, user, drives.RoleViewer)
	if !ok {
		return
	}
	sheet, ok := pickSheet(r, doc)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+pathEscape(base+" - "+sheet.Name+".csv"))
	_, _ = w.Write(collab.SheetCSV(sheet))
}

// exportCSVSave lands one sheet's CSV as a sibling file in the drive.
func (h *handlers) exportCSVSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !exportCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID, base, doc, ok := h.loadGridFor(w, r, user, drives.RoleEditor)
	if !ok {
		return
	}
	sheet, ok := pickSheet(r, doc)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.saveExportSibling(w, r, user, driveID, nodeID, base+" - "+sheet.Name+".csv", "text/csv", collab.SheetCSV(sheet))
}

// exportXLSX streams the whole workbook as .xlsx (values + tab names).
func (h *handlers) exportXLSX(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	_, nodeID, base, doc, ok := h.loadGridFor(w, r, user, drives.RoleViewer)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+pathEscape(base+".xlsx"))
	if err := collab.WriteXLSX(w, doc); err != nil {
		h.k.Log.Warn("xlsx export failed", "node", nodeID, "err", err)
	}
}

// exportXLSXSave lands the workbook's XLSX as a sibling file.
func (h *handlers) exportXLSXSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !exportCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID, base, doc, ok := h.loadGridFor(w, r, user, drives.RoleEditor)
	if !ok {
		return
	}
	var buf bytes.Buffer
	if err := collab.WriteXLSX(&buf, doc); err != nil {
		kernel.JSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "xlsx build failed"})
		return
	}
	h.saveExportSibling(w, r, user, driveID, nodeID, base+".xlsx",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", buf.Bytes())
}

// --- writer exports ----------------------------------------------------------------

// loadWDocFor is the shared front half of every writer export.
func (h *handlers) loadWDocFor(w http.ResponseWriter, r *http.Request, user users.User, minRole string) (string, string, string, collab.WDocDoc, bool) {
	driveID, nodeID, node, ok := h.docNode(r, user, minRole)
	if !ok || !collab.IsWDocFile(node) {
		http.NotFound(w, r)
		return "", "", "", collab.WDocDoc{}, false
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	doc, err := h.k.Collab.LoadWDoc(cctx, driveID, nodeID, node)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return "", "", "", collab.WDocDoc{}, false
	}
	return driveID, nodeID, strings.TrimSuffix(node.Name, collab.WDocExt), doc, true
}

// exportHTML streams a standalone HTML file of the document.
func (h *handlers) exportHTML(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	_, _, base, doc, ok := h.loadWDocFor(w, r, user, drives.RoleViewer)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+pathEscape(base+".html"))
	// Belt and braces for anyone opening the export in a browser: the
	// stored fragments are attacker-writable, so scripts are refused
	// even if a hostile payload survived a bypassed client sanitizer.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	_, _ = w.Write(wdocExportHTML(base, doc))
}

// exportHTMLSave lands the HTML export as a sibling file in the drive.
func (h *handlers) exportHTMLSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !exportCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID, base, doc, ok := h.loadWDocFor(w, r, user, drives.RoleEditor)
	if !ok {
		return
	}
	h.saveExportSibling(w, r, user, driveID, nodeID, base+".html", "text/html; charset=utf-8", wdocExportHTML(base, doc))
}

// exportTXT streams the plain-text form.
func (h *handlers) exportTXT(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	_, _, base, doc, ok := h.loadWDocFor(w, r, user, drives.RoleViewer)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+pathEscape(base+".txt"))
	_, _ = w.Write([]byte(collab.WDocText(doc)))
}

// exportTXTSave lands the plain-text export as a sibling file.
func (h *handlers) exportTXTSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !exportCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID, base, doc, ok := h.loadWDocFor(w, r, user, drives.RoleEditor)
	if !ok {
		return
	}
	h.saveExportSibling(w, r, user, driveID, nodeID, base+".txt", "text/plain; charset=utf-8", []byte(collab.WDocText(doc)))
}

// wdocExportHTML renders the standalone-HTML form of a document.
func wdocExportHTML(base string, doc collab.WDocDoc) []byte {
	var w bytes.Buffer
	page := doc.Page
	if !collab.ValidWDocPage(page) {
		page = collab.WDocPage{}
	}
	widthCM, mt, mr, mb, ml := collab.WDocPageWidthCM(page), cmOr(page.MT), cmOr(page.MR), cmOr(page.MB), cmOr(page.ML)
	fmt.Fprintf(&w, "<!doctype html>\n<html><head><meta charset=\"utf-8\">\n<title>%s</title>\n", htmlEscape(base))
	fmt.Fprintf(&w, "<style>body{width:%.1fcm;margin:1cm auto;padding:%.1fcm %.1fcm %.1fcm %.1fcm;box-sizing:border-box;font-family:Georgia,serif;line-height:1.6;color:#222;background:#fff}"+
		"pre{background:#f4f4f4;padding:12px;border-radius:6px;overflow:auto;white-space:pre-wrap}"+
		"blockquote{border-left:3px solid #ccc;margin-left:0;padding-left:16px;color:#555}"+
		"header,footer{color:#777;font-size:.85em}header{border-bottom:1px solid #ddd;margin-bottom:1.5em;padding-bottom:.4em}footer{border-top:1px solid #ddd;margin-top:1.5em;padding-top:.4em}"+
		"hr.pb{border:0;page-break-after:always;break-after:page;margin:0}"+
		"@page{size:%s %s;margin:%.1fcm %.1fcm %.1fcm %.1fcm}</style>\n</head><body>\n",
		widthCM, mt, mr, mb, ml, pageSizeName(page.Size), orientName(page.Orient), mt, mr, mb, ml)
	if doc.Header != "" {
		fmt.Fprintf(&w, "<header>%s</header>\n", doc.Header)
	}
	for _, b := range doc.Blocks {
		fmt.Fprintln(&w, b.HTML)
	}
	if doc.Footer != "" {
		fmt.Fprintf(&w, "<footer>%s</footer>\n", doc.Footer)
	}
	fmt.Fprint(&w, "</body></html>\n")
	return w.Bytes()
}

// cmOr defaults a zero margin to 2.5cm (the page model's default).
func cmOr(v float64) float64 {
	if v == 0 {
		return 2.5
	}
	return v
}

// pageSizeName / orientName map stored values to CSS @page tokens.
func pageSizeName(s string) string {
	switch s {
	case "a4":
		return "A4"
	case "legal":
		return "legal"
	}
	return "letter"
}
func orientName(o string) string {
	if o == "landscape" {
		return "landscape"
	}
	return "portrait"
}

// htmlEscape escapes a small string for the export's <title>.
func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// --- diagram export ----------------------------------------------------------------

// drawExportSave lands a client-rendered export (the SVG scene, or a
// canvas-rasterized PNG) as a sibling file. The diagram renders on the
// client, so the client supplies the bytes; the server only checks the
// format is one of the two it advertised and stores the body verbatim.
// SVG is stored but never served inline (download.go's inlineSafe), so a
// hostile hand-posted payload can't script against the app's origin.
func (h *handlers) drawExportSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !exportCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID, node, ok := h.docNode(r, user, drives.RoleEditor)
	if !ok || !collab.IsDrawFile(node) {
		http.NotFound(w, r)
		return
	}
	ext, contentType := "svg", "image/svg+xml"
	if r.URL.Query().Get("fmt") == "png" {
		ext, contentType = "png", "image/png"
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, exportSaveLimit))
	if err != nil || len(raw) == 0 {
		kernel.JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad export body"})
		return
	}
	base := strings.TrimSuffix(node.Name, collab.DrawExt)
	h.saveExportSibling(w, r, user, driveID, nodeID, base+"."+ext, contentType, raw)
}
