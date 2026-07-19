// handlers.go implements the page and action handlers wired up in
// frontend.go (§4): the developer KV/blob/watch browsers
// and the admin users/system views. Every handler follows the same shape
// as the JSON API (pkg/routes/v1api) — authenticate the session, authorize
// the (key, verb) against the user's grants (§7.2), run the server data
// plane, and render HTML — so the GUI and the API can never diverge on who
// may do what.
package frontend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/blob"
	"github.com/hyperkubeorg/databox/pkg/kv"
	"github.com/hyperkubeorg/databox/pkg/renderer"
	"github.com/hyperkubeorg/databox/pkg/server"
)

// --- developer KV browser (§4) ----------------------------------------------

// kvRow is one row of the KV listing table.
type kvRow struct {
	Key  string
	Rev  uint64
	Size int64
	Blob bool
}

// kvDir is one "directory" in the non-recursive view: a distinct next
// path segment under the current prefix, browsable by descending into it.
type kvDir struct {
	Segment string // e.g. "somefiles/"
	Prefix  string // full prefix to browse: current prefix + Segment
}

// kvCrumb is one clickable breadcrumb path element.
type kvCrumb struct {
	Label  string
	Prefix string
}

// kvListData feeds kv.tpl.
type kvListData struct {
	Prefix    string
	Recursive bool
	Start     string // user-supplied range start (inclusive)
	Next      string // cursor for the next page ("" = done)
	Crumbs    []kvCrumb
	Dirs      []kvDir
	Rows      []kvRow
	// System marks the metadata (.databox/) view: read-only, admin-only,
	// no watch button (watches cover user shards, not metadata).
	System bool
	// ShowSystemRoot adds the ".databox/" pseudo-directory at the top of
	// the root listing for admins (§19).
	ShowSystemRoot bool
}

// systemPrefix is the virtual path root under which the GUI exposes the
// metadata keyspace inside the KV explorer (§19's `.databox/` namespace).
const systemPrefix = ".databox/"

// pageSize bounds how many items (dirs + keys) one KV browser page shows.
const kvPageSize = 200

// crumbs splits a prefix into clickable path breadcrumbs:
// "/a/b/" → [{/ /}, {a/ /a/}, {b/ /a/b/}]. Metadata prefixes get the
// root crumb plus a ".databox/" crumb, then their own segments.
func crumbs(prefix string) []kvCrumb {
	out := []kvCrumb{{Label: "/", Prefix: "/"}}
	if strings.HasPrefix(prefix, systemPrefix) {
		out = append(out, kvCrumb{Label: systemPrefix, Prefix: systemPrefix})
		rest := strings.TrimPrefix(prefix, systemPrefix)
		at := systemPrefix
		for rest != "" {
			idx := strings.Index(rest, "/")
			if idx < 0 {
				out = append(out, kvCrumb{Label: rest, Prefix: at + rest})
				break
			}
			at = at + rest[:idx+1]
			out = append(out, kvCrumb{Label: rest[:idx+1], Prefix: at})
			rest = rest[idx+1:]
		}
		return out
	}
	rest := strings.TrimPrefix(prefix, "/")
	at := "/"
	for rest != "" {
		idx := strings.Index(rest, "/")
		if idx < 0 {
			// Trailing partial segment (prefix not ending in /): it is
			// a filter, shown but pointing at itself.
			out = append(out, kvCrumb{Label: rest, Prefix: at + rest})
			break
		}
		at = at + rest[:idx+1]
		out = append(out, kvCrumb{Label: rest[:idx+1], Prefix: at})
		rest = rest[idx+1:]
	}
	return out
}

// startCursor converts an inclusive "start from" key into the exclusive
// cursor KVList expects: the largest possible key strictly BELOW start,
// so start itself is included in the scan. Decrementing the final byte
// and padding with 0xff produces exactly that predecessor.
func startCursor(start string) string {
	b := []byte(start)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] > 0 {
			b[i]--
			return string(b[:i+1]) + "\xff\xff\xff\xff"
		}
		// A trailing 0x00 byte: the predecessor is the string without it.
		b = b[:i]
	}
	return "" // start was empty or all zero bytes: begin at the prefix
}

// kvList renders the KV browser (§4). Two modes:
//
//   - hierarchical (default): entries are grouped by their next path
//     segment — subtrees show as directories, leaf keys as rows. The
//     scan SKIPS AHEAD past each discovered directory (cursor jumps to
//     just after the subtree), so a prefix with a million keys under
//     one child segment costs one page fetch, not a million rows.
//
//   - recursive: the flat paged listing of every key under the prefix.
//
// Pagination is range-based, matching how the storage actually scans:
// "start" begins the listing at a key (inclusive) — e.g. /path/0500000
// to jump into the middle of date- or number-ordered keys — and the
// next-page link continues from the last key shown. List authorization
// applies to the prefix (the coarsest key the caller must own).
func (g *gui) kvList(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	prefix := q.Get("prefix")
	if prefix == "" {
		prefix = "/"
	}
	recursive := q.Get("recursive") == "1"
	start := q.Get("start")
	cursor := q.Get("cursor")
	isAdmin := g.s.AuthorizeAdmin(u) == nil

	// The metadata view: prefixes under ".databox/" browse the system
	// keyspace (§19) — admin-gated, read-only, served by the metadata
	// group instead of the shard map.
	system := strings.HasPrefix(prefix, systemPrefix) || prefix == strings.TrimSuffix(systemPrefix, "/")
	if system {
		if !isAdmin {
			g.failPage(w, r, u, server.ErrUnauthorized)
			return
		}
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
	} else if err := g.s.Authorize(u, prefix, auth.VerbList); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	// The effective scan position: an explicit next-page cursor wins;
	// otherwise a user-supplied inclusive start key is converted to the
	// exclusive cursor form.
	if cursor == "" && start != "" {
		cursor = startCursor(start)
	}
	d := &kvListData{
		Prefix: prefix, Recursive: recursive, Start: start, Crumbs: crumbs(prefix),
		System:         system,
		ShowSystemRoot: isAdmin && prefix == "/" && cursor == "" && start == "",
	}
	// One list function for both keyspaces keeps the scan logic below
	// identical: user keys route through the shard map, system keys read
	// the locally replicated metadata (stripping the virtual prefix).
	list := g.s.KVList
	if system {
		sys := strings.TrimPrefix(prefix, systemPrefix)
		list = func(_, cur string, limit int) ([]kv.ListEntry, string, error) {
			entries, next, err := g.s.SystemListPage(sys, strings.TrimPrefix(cur, systemPrefix), limit)
			// Re-attach the virtual prefix so links stay in .databox/.
			for i := range entries {
				entries[i].Key = systemPrefix + entries[i].Key
			}
			if next != "" {
				next = systemPrefix + next
			}
			return entries, next, err
		}
	}

	if recursive {
		// Flat mode: one straight page.
		entries, next, err := list(prefix, cursor, kvPageSize)
		if err != nil {
			g.failPage(w, r, u, err)
			return
		}
		for _, e := range entries {
			d.Rows = append(d.Rows, kvRow{Key: e.Key, Rev: e.Record.Rev, Size: int64(len(e.Record.Value)), Blob: e.Record.Blob})
		}
		d.Next = next
	} else {
		// Hierarchical mode: group by next path segment with skip-ahead.
		if err := g.kvListGrouped(d, prefix, cursor, list); err != nil {
			g.failPage(w, r, u, err)
			return
		}
	}
	g.render(w, http.StatusOK, "kv.tpl", g.page(r, u, "KV browser", d))
}

// listFunc is the page-fetch shape shared by the user and system views.
type listFunc func(prefix, cursor string, limit int) ([]kv.ListEntry, string, error)

// groupedScan is the hierarchical (delimiter) scan shared by the KV and
// blob browsers. It fetches pages and, on discovering a child directory,
// jumps the cursor past the whole subtree ("<prefix><segment>\xff…") — so
// listing one level costs proportional to the number of CHILDREN, never
// the number of keys underneath them. Leaf entries go through emit, which
// reports whether it kept the row (the blob browser drops non-blob keys);
// kept rows count against the page budget.
func groupedScan(prefix, cursor string, list listFunc, pageSize int, emit func(kv.ListEntry) bool) (dirs []kvDir, next string, err error) {
	const maxFetches = 50 // hard bound on backend round trips per page view
	kept := 0
	for fetch := 0; fetch < maxFetches; fetch++ {
		entries, pageNext, err := list(prefix, cursor, 1000)
		if err != nil {
			return nil, "", err
		}
		skipTo := "" // set when a directory swallows the rest of this page
		for _, e := range entries {
			if len(dirs)+kept >= pageSize {
				// Page full: resume from the last classified item.
				return dirs, e.Key, nil
			}
			rest := strings.TrimPrefix(e.Key, prefix)
			if idx := strings.Index(rest, "/"); idx >= 0 {
				// A deeper key: its first segment is a directory.
				seg := rest[:idx+1]
				dirs = append(dirs, kvDir{Segment: seg, Prefix: prefix + seg})
				// Skip everything else inside this subtree.
				skipTo = prefix + seg + "\xff\xff\xff\xff"
				break
			}
			if emit(e) {
				kept++
			}
		}
		switch {
		case skipTo != "":
			cursor = skipTo
		case pageNext == "":
			return dirs, "", nil // scanned to the end of the prefix
		default:
			cursor = pageNext
		}
	}
	// Fetch budget exhausted (pathologically wide level): let the user
	// continue from where the scan stopped.
	return dirs, cursor, nil
}

// kvListGrouped adapts groupedScan for the KV browser's data shape.
func (g *gui) kvListGrouped(d *kvListData, prefix, cursor string, list listFunc) error {
	dirs, next, err := groupedScan(prefix, cursor, list, kvPageSize, func(e kv.ListEntry) bool {
		d.Rows = append(d.Rows, kvRow{Key: e.Key, Rev: e.Record.Rev, Size: int64(len(e.Record.Value)), Blob: e.Record.Blob})
		return true
	})
	if err != nil {
		return err
	}
	d.Dirs, d.Next = dirs, next
	return nil
}

// kvViewData feeds kv_view.tpl.
type kvViewData struct {
	Key       string
	Parent    string
	Found     bool
	Rev       uint64
	Blob      bool
	Printable bool
	Text      string
	Hex       string
	Size      int64
	CanWrite  bool
	CanDelete bool
	// System marks a metadata (.databox/) key: read-only view.
	System bool
	// Placement is the admin inspection panel: where this key physically
	// lives — shard, raft group, member nodes, current leader. Nil for
	// non-admin viewers and for system keys (which live in the metadata
	// group by definition).
	Placement *server.Placement
	// BlobDetail is the admin storage-layout panel for blob keys: the
	// durability mode and every chunk/EC-shard with the nodes holding it.
	BlobDetail *blobDetail
}

// blobDetail describes a blob's physical layout for the inspection panel.
type blobDetail struct {
	Mode         string // "replica" | "ec"
	Size         int64
	ContentType  string
	SHA256       string
	DataShards   int // EC geometry (0 for replica mode)
	ParityShards int
	TotalShards  int // DataShards + ParityShards, precomputed for the template
	Chunks       []chunkRow
}

// chunkRow is one stored piece: a replica-mode chunk or one EC shard.
type chunkRow struct {
	Label  string // "chunk 3" or "stripe 2 · parity 1"
	Size   int64
	Hash   string // shortened content hash
	Nodes  string // display names of the holders
	Parity bool
}

// buildBlobDetail decodes a manifest into the inspection panel's rows,
// resolving node IDs to names once via the directory.
func buildBlobDetail(m *blob.Manifest, names map[uint64]string) *blobDetail {
	d := &blobDetail{
		Mode: m.Mode, Size: m.Size, ContentType: m.ContentType, SHA256: m.SHA256,
		DataShards: m.DataShards, ParityShards: m.ParityShards,
		TotalShards: m.DataShards + m.ParityShards,
	}
	nodeLabel := func(ids []uint64) string {
		parts := make([]string, 0, len(ids))
		for _, id := range ids {
			if n, ok := names[id]; ok && n != "" {
				parts = append(parts, fmt.Sprintf("%s (#%d)", n, id))
			} else {
				parts = append(parts, fmt.Sprintf("node #%d", id))
			}
		}
		return strings.Join(parts, ", ")
	}
	short := func(h string) string {
		if len(h) > 12 {
			return h[:12] + "…"
		}
		return h
	}
	for i, ref := range m.Chunks {
		d.Chunks = append(d.Chunks, chunkRow{
			Label: fmt.Sprintf("chunk %d", i+1), Size: ref.Size,
			Hash: short(ref.Hash), Nodes: nodeLabel(ref.Nodes),
		})
	}
	for si, stripe := range m.Stripes {
		for k, ref := range stripe.Shards {
			label := fmt.Sprintf("stripe %d · data %d", si+1, k+1)
			parity := k >= m.DataShards
			if parity {
				label = fmt.Sprintf("stripe %d · parity %d", si+1, k-m.DataShards+1)
			}
			d.Chunks = append(d.Chunks, chunkRow{
				Label: label, Size: ref.Size, Hash: short(ref.Hash),
				Nodes: nodeLabel(ref.Nodes), Parity: parity,
			})
		}
	}
	return d
}

// parentPrefix returns the directory-like prefix for a key, so the "back
// to listing" link lands where the user came from.
func parentPrefix(key string) string {
	if i := strings.LastIndex(strings.TrimSuffix(key, "/"), "/"); i >= 0 {
		return key[:i+1]
	}
	return "/"
}

// kvView inspects one key: its value (text or hex preview) plus edit and
// delete controls, each shown only when the caller holds the verb.
func (g *gui) kvView(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		key = "/"
	}
	isAdmin := g.s.AuthorizeAdmin(u) == nil

	// Metadata keys (.databox/…) read from the system view: admin-gated
	// and strictly read-only — the GUI never mutates cluster internals.
	if strings.HasPrefix(key, systemPrefix) {
		if !isAdmin {
			g.failPage(w, r, u, server.ErrUnauthorized)
			return
		}
		rec, found, err := g.s.SystemGet(strings.TrimPrefix(key, systemPrefix))
		if err != nil {
			g.failPage(w, r, u, err)
			return
		}
		d := &kvViewData{
			Key: key, Parent: parentPrefix(key), Found: found,
			Rev: rec.Rev, Size: int64(len(rec.Value)), System: true,
		}
		if found {
			if renderer.Printable(rec.Value) {
				d.Printable, d.Text = true, string(rec.Value)
			} else {
				d.Hex = renderer.HexPreview(rec.Value, 4096)
			}
		}
		g.render(w, http.StatusOK, "kv_view.tpl", g.page(r, u, key, d))
		return
	}

	if err := g.s.Authorize(u, key, auth.VerbRead); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	rec, found, err := g.s.KVGet(r.Context(), key)
	if err != nil {
		g.failPage(w, r, u, err)
		return
	}
	d := &kvViewData{
		Key:       key,
		Parent:    parentPrefix(key),
		Found:     found,
		Rev:       rec.Rev,
		Blob:      rec.Blob,
		Size:      int64(len(rec.Value)),
		CanWrite:  g.s.Authorize(u, key, auth.VerbWrite) == nil,
		CanDelete: g.s.Authorize(u, key, auth.VerbDelete) == nil,
	}
	// Admin inspection: where the key physically lives (shard → raft
	// group → member nodes + leader). Best effort — a placement lookup
	// failure must not break the value view.
	if isAdmin {
		if p, err := g.s.KeyPlacement(key); err == nil {
			d.Placement = p
		}
		// For blobs, also decode the manifest into the storage-layout
		// panel: durability mode, every chunk/EC shard, and the nodes
		// holding each piece.
		if found && rec.Blob {
			if m, err := blob.Decode(rec.Value); err == nil {
				d.BlobDetail = buildBlobDetail(m, g.s.NodeDirectory())
			}
		}
	}
	if found && !rec.Blob {
		if renderer.Printable(rec.Value) {
			d.Printable = true
			d.Text = string(rec.Value)
		} else {
			d.Hex = renderer.HexPreview(rec.Value, 4096)
		}
	}
	g.render(w, http.StatusOK, "kv_view.tpl", g.page(r, u, "Key "+key, d))
}

// kvSet writes a key from the edit form and returns to its view.
func (g *gui) kvSet(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		g.errorPage(w, r, u, http.StatusBadRequest, "bad form")
		return
	}
	if !g.checkCSRF(w, r, u) {
		return
	}
	key := r.FormValue("key")
	if err := g.s.Authorize(u, key, auth.VerbWrite); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	if _, err := g.s.KVSet(r.Context(), key, []byte(r.FormValue("value")), false); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	http.Redirect(w, r, "/kv/view?key="+urlQuery(key), http.StatusSeeOther)
}

// kvDelete removes a key and returns to the parent listing.
func (g *gui) kvDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		g.errorPage(w, r, u, http.StatusBadRequest, "bad form")
		return
	}
	if !g.checkCSRF(w, r, u) {
		return
	}
	key := r.FormValue("key")
	if err := g.s.Authorize(u, key, auth.VerbDelete); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	if _, err := g.s.KVDelete(r.Context(), key); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	http.Redirect(w, r, "/kv?prefix="+urlQuery(parentPrefix(key)), http.StatusSeeOther)
}

// --- blob browser (§4, §11) --------------------------------------------------

// blobRow is one row of the blob listing table.
type blobRow struct {
	Key         string
	Size        int64
	ContentType string
	SHA256      string
	Rev         uint64
	Mode        string // "replica" | "ec" — durability at a glance
}

// blobsData feeds blobs.tpl — same navigation shape as the KV browser.
type blobsData struct {
	Prefix    string
	Recursive bool
	Start     string
	Next      string
	Crumbs    []kvCrumb
	Dirs      []kvDir
	Rows      []blobRow
}

// blobList is the blob browser: the same hierarchical navigation as the
// KV explorer (directories via delimiter skip-ahead, recursive toggle,
// range-start pagination), with leaf rows filtered to blob-backed keys
// and sized from their manifests. Nothing is ever listed exhaustively —
// every mode is a bounded page over a cursor.
func (g *gui) blobList(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	prefix := q.Get("prefix")
	if prefix == "" {
		prefix = "/"
	}
	recursive := q.Get("recursive") == "1"
	start := q.Get("start")
	cursor := q.Get("cursor")
	if err := g.s.Authorize(u, prefix, auth.VerbList); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	if cursor == "" && start != "" {
		cursor = startCursor(start)
	}
	d := &blobsData{Prefix: prefix, Recursive: recursive, Start: start, Crumbs: crumbs(prefix)}
	// emit keeps only blob-backed keys, sizing rows from the manifest.
	emit := func(e kv.ListEntry) bool {
		if !e.Record.Blob {
			return false
		}
		row := blobRow{Key: e.Key, Rev: e.Record.Rev}
		if m, err := blob.Decode(e.Record.Value); err == nil {
			row.Size = m.Size
			row.ContentType = m.ContentType
			row.SHA256 = m.SHA256
			row.Mode = m.Mode
		}
		d.Rows = append(d.Rows, row)
		return true
	}
	if recursive {
		// Flat mode: one bounded page; the cursor advances through the
		// underlying scan even when a page contains few blobs, so
		// paging always makes progress.
		entries, next, err := g.s.KVList(prefix, cursor, kvPageSize)
		if err != nil {
			g.failPage(w, r, u, err)
			return
		}
		for _, e := range entries {
			emit(e)
		}
		d.Next = next
	} else {
		dirs, next, err := groupedScan(prefix, cursor, g.s.KVList, kvPageSize, emit)
		if err != nil {
			g.failPage(w, r, u, err)
			return
		}
		d.Dirs, d.Next = dirs, next
	}
	g.render(w, http.StatusOK, "blobs.tpl", g.page(r, u, "Blob browser", d))
}

// blobDownload streams a blob to the browser as an attachment.
func (g *gui) blobDownload(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	key := r.URL.Query().Get("key")
	if err := g.s.Authorize(u, key, auth.VerbRead); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	m, err := g.s.StatBlob(r.Context(), key)
	if err != nil {
		g.failPage(w, r, u, err)
		return
	}
	ct := m.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
	w.Header().Set("Content-Disposition", "attachment; filename=\""+path.Base(key)+"\"")
	// Headers are set; from here a stream error only truncates the body.
	_, _ = g.s.GetBlob(r.Context(), key, w)
}

// blobUpload streams a multipart file into the blob store. The form places
// csrf and key before the file part so both are known before the (possibly
// huge) file body is streamed to the engine.
func (g *gui) blobUpload(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		g.errorPage(w, r, u, http.StatusBadRequest, "expected a multipart upload")
		return
	}
	var csrf, key string
	for {
		part, err := mr.NextPart()
		if err != nil {
			g.errorPage(w, r, u, http.StatusBadRequest, "malformed upload (no file part)")
			return
		}
		switch part.FormName() {
		case "csrf":
			buf := make([]byte, 512)
			n, _ := part.Read(buf)
			csrf = strings.TrimSpace(string(buf[:n]))
		case "key":
			buf := make([]byte, 4096)
			n, _ := part.Read(buf)
			key = strings.TrimSpace(string(buf[:n]))
		case "file":
			// Validate everything we can before touching the stream.
			if !g.checkCSRFValue(w, r, u, csrf) {
				return
			}
			if err := g.s.Authorize(u, key, auth.VerbWrite); err != nil {
				g.failPage(w, r, u, err)
				return
			}
			ct := part.Header.Get("Content-Type")
			if _, _, err := g.s.PutBlob(r.Context(), key, part, ct); err != nil {
				g.failPage(w, r, u, err)
				return
			}
			http.Redirect(w, r, "/blobs?prefix="+urlQuery(parentPrefix(key)), http.StatusSeeOther)
			return
		}
	}
}

// blobDelete removes a blob manifest and returns to the listing.
func (g *gui) blobDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		g.errorPage(w, r, u, http.StatusBadRequest, "bad form")
		return
	}
	if !g.checkCSRF(w, r, u) {
		return
	}
	key := r.FormValue("key")
	if err := g.s.Authorize(u, key, auth.VerbDelete); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	if err := g.s.DeleteBlob(r.Context(), key); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	http.Redirect(w, r, "/blobs?prefix="+urlQuery(parentPrefix(key)), http.StatusSeeOther)
}

// --- watch console (§4, §9.2) ------------------------------------------------

// watchData feeds watch.tpl.
type watchData struct {
	Prefix string
	// AutoStart begins streaming on page load (deep links from the KV
	// explorer's "watch this prefix" buttons).
	AutoStart bool
}

// watchPage renders the live watch console shell; the streaming happens in
// watchStream, called by the page's inline fetch().
func (g *gui) watchPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	prefix := r.URL.Query().Get("prefix")
	if prefix == "" {
		prefix = "/"
	}
	// go=1 auto-starts the stream — the KV explorer's "watch this
	// prefix/key" links land here already running.
	g.render(w, http.StatusOK, "watch.tpl", g.page(r, u, "Watch console",
		&watchData{Prefix: prefix, AutoStart: r.URL.Query().Get("go") == "1"}))
}

// watchStream is the cookie-authenticated NDJSON endpoint the console page
// streams from. It mirrors /api/v1/watch but authenticates via the session
// cookie (the HttpOnly token can never reach the page's JavaScript, §4).
func (g *gui) watchStream(w http.ResponseWriter, r *http.Request) {
	u, ok := g.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	prefix := r.URL.Query().Get("prefix")
	if prefix == "" {
		prefix = "/"
	}
	if err := g.s.Authorize(u, prefix, auth.VerbWatch); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	err := g.s.Watch(r.Context(), prefix, 0, func(ev kv.Event) error {
		if err := enc.Encode(ev); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	})
	if err != nil && r.Context().Err() == nil {
		// The stream already started; append a terminal error line so the
		// console shows why it ended.
		_ = enc.Encode(map[string]string{"error": err.Error()})
	}
}

// urlQuery escapes a value for inclusion in a query string.
func urlQuery(s string) string { return url.QueryEscape(s) }
