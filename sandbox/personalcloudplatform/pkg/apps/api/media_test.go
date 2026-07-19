package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// mediaFixture extends the drive fixture with a registered video folder
// and a small catalog.
type mediaFixture struct {
	*driveFixture
	folder nodes.Node
}

func newMediaFixture(t *testing.T) *mediaFixture {
	t.Helper()
	f := newDriveFixture(t)
	db := f.h.k.Users.DB
	f.h.k.Media = &media.Store{DB: db, Nodes: f.h.k.Nodes, Drives: f.h.k.Drives}
	ctx := t.Context()
	folder, err := f.h.k.Nodes.CreateFolder(ctx, f.drive.ID, nodes.RootID, "Video", "ada")
	if err != nil {
		t.Fatalf("folder: %v", err)
	}
	if err := f.h.k.Media.Register(ctx, f.drive.ID, folder.ID, media.KindVideo, "ada"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := f.h.k.Nodes.CommitFile(ctx, f.drive.ID, folder.ID, "Movie (2023).mp4", "BBBBBBBBBBBB", "video/mp4", 9, "ada", false); err != nil {
		t.Fatalf("file: %v", err)
	}
	if _, err := f.h.k.Media.Rescan(ctx, f.drive.ID, folder.ID); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	return &mediaFixture{driveFixture: f, folder: folder}
}

// The documented GET /api/v1/media/folders shape (docs/api.md).
func TestMediaFoldersShape(t *testing.T) {
	f := newMediaFixture(t)
	w := httptest.NewRecorder()
	f.h.mediaFolders(w, httptest.NewRequest("GET", "/api/v1/media/folders", nil), apikeys.Key{}, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := decode(t, w)
	rows := got["folders"].([]any)
	if len(rows) != 1 {
		t.Fatalf("folders = %v", rows)
	}
	row := rows[0].(map[string]any)
	for _, k := range []string{"driveId", "folderId", "kind", "name", "driveName", "hidden", "items", "scannedAt"} {
		if _, present := row[k]; !present {
			t.Errorf("folder row missing %q: %v", k, row)
		}
	}
	if row["kind"] != "video" || row["name"] != "Video" || row["items"] != float64(1) {
		t.Errorf("folder row = %v", row)
	}
}

// The documented GET /api/v1/media/catalog shape.
func TestMediaCatalogShape(t *testing.T) {
	f := newMediaFixture(t)
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("folder", f.folder.ID)
	w := httptest.NewRecorder()
	f.h.mediaCatalog(w, req, apikeys.Key{}, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := decode(t, w)
	if got["kind"] != "video" {
		t.Errorf("kind = %v", got["kind"])
	}
	entries := got["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	e := entries[0].(map[string]any)
	for _, k := range []string{"kind", "slug", "title", "driveId", "nodeId"} {
		if _, present := e[k]; !present {
			t.Errorf("entry missing %q: %v", k, e)
		}
	}
	if e["kind"] != "movies" || e["title"] != "Movie" || e["year"] != float64(2023) {
		t.Errorf("entry = %v", e)
	}

	// A junk ?kind= is a 400, not a scan.
	req = httptest.NewRequest("GET", "/x?kind=junk", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("folder", f.folder.ID)
	w = httptest.NewRecorder()
	f.h.mediaCatalog(w, req, apikeys.Key{}, f.user)
	if w.Code != http.StatusBadRequest {
		t.Errorf("junk kind status = %d", w.Code)
	}
}

// A folder registered as video AND music: /media/folders lists one
// entry per kind, and the catalog default folds both top-level sets.
func TestMediaDualKindShapes(t *testing.T) {
	f := newMediaFixture(t)
	ctx := t.Context()
	if err := f.h.k.Media.Register(ctx, f.drive.ID, f.folder.ID, media.KindMusic, "ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.k.Nodes.CommitFile(ctx, f.drive.ID, f.folder.ID, "01 Song.mp3", "CCCCCCCCCCCC", "audio/mpeg", 9, "ada", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.k.Media.Rescan(ctx, f.drive.ID, f.folder.ID); err != nil {
		t.Fatal(err)
	}

	// Folders: one entry per (folder, kind).
	w := httptest.NewRecorder()
	f.h.mediaFolders(w, httptest.NewRequest("GET", "/x", nil), apikeys.Key{}, f.user)
	rows := decode(t, w)["folders"].([]any)
	kinds := map[any]bool{}
	for _, r := range rows {
		row := r.(map[string]any)
		if row["folderId"] != f.folder.ID {
			t.Errorf("unexpected folder row: %v", row)
		}
		kinds[row["kind"]] = true
	}
	if len(rows) != 2 || !kinds["video"] || !kinds["music"] {
		t.Fatalf("dual-kind folders = %v", rows)
	}

	// Catalog: default = both registrations' top-level kinds; the
	// response carries the full kinds set (+ legacy "kind").
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("folder", f.folder.ID)
	w = httptest.NewRecorder()
	f.h.mediaCatalog(w, req, apikeys.Key{}, f.user)
	got := decode(t, w)
	if got["kind"] != "video" || len(got["kinds"].([]any)) != 2 {
		t.Errorf("catalog kinds = %v / %v", got["kind"], got["kinds"])
	}
	seen := map[any]bool{}
	for _, e := range got["entries"].([]any) {
		seen[e.(map[string]any)["kind"]] = true
	}
	if !seen["movies"] || !seen["albums"] {
		t.Errorf("catalog default missed a kind's entries: %v", seen)
	}
}

// The documented GET /api/v1/media/entry shape (movie: no children).
func TestMediaEntryShape(t *testing.T) {
	f := newMediaFixture(t)
	// Find the movie slug via the catalog.
	entries, err := f.h.k.Media.ListCatalog(t.Context(), f.drive.ID, f.folder.ID, media.CatMovie)
	if err != nil || len(entries) != 1 {
		t.Fatalf("catalog: %v (%v)", entries, err)
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("folder", f.folder.ID)
	req.SetPathValue("kind", media.CatMovie)
	req.SetPathValue("slug", entries[0].ID)
	w := httptest.NewRecorder()
	f.h.mediaEntry(w, req, apikeys.Key{}, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := decode(t, w)
	entry := got["entry"].(map[string]any)
	if entry["title"] != "Movie" || entry["slug"] != entries[0].ID {
		t.Errorf("entry = %v", entry)
	}
	if children := got["children"].([]any); len(children) != 0 {
		t.Errorf("movie children = %v", children)
	}
}

// Access is a membership question: a non-member's key sees nothing —
// not_found, never forbidden (no keyspace mapping).
func TestMediaAccessDenied(t *testing.T) {
	f := newMediaFixture(t)
	outsider, err := f.h.k.Users.CreateUser(t.Context(), "erin", "Erin", "password123")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("folder", f.folder.ID)
	w := httptest.NewRecorder()
	f.h.mediaCatalog(w, req, apikeys.Key{}, outsider)
	if w.Code != http.StatusNotFound {
		t.Errorf("outsider catalog status = %d, want 404", w.Code)
	}
	// And the union is empty.
	w = httptest.NewRecorder()
	f.h.mediaFolders(w, httptest.NewRequest("GET", "/x", nil), apikeys.Key{}, outsider)
	if got := decode(t, w); len(got["folders"].([]any)) != 0 {
		t.Errorf("outsider folders = %v", got["folders"])
	}
}

// The documented GET /api/v1/media/progress shape.
func TestMediaProgressShape(t *testing.T) {
	f := newMediaFixture(t)
	ctx := t.Context()
	entries, _ := f.h.k.Media.ListCatalog(ctx, f.drive.ID, f.folder.ID, media.CatMovie)
	if err := f.h.k.Media.SetProgress(ctx, "ada", media.Progress{
		DriveID: f.drive.ID, NodeID: entries[0].NodeID, Kind: media.ProgVideo,
		Title: "Movie", Pos: 42, Dur: 100,
	}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	f.h.mediaProgress(w, httptest.NewRequest("GET", "/x", nil), apikeys.Key{}, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	rows := decode(t, w)["progress"].([]any)
	if len(rows) != 1 {
		t.Fatalf("progress = %v", rows)
	}
	row := rows[0].(map[string]any)
	for _, k := range []string{"driveId", "nodeId", "kind", "title", "pos", "watched", "at"} {
		if _, present := row[k]; !present {
			t.Errorf("progress row missing %q: %v", k, row)
		}
	}
	if row["pos"] != float64(42) || row["watched"] != false || row["kind"] != "video" {
		t.Errorf("progress row = %v", row)
	}

	// Another user's key sees nothing.
	other := users.User{Username: "erin"}
	w = httptest.NewRecorder()
	f.h.mediaProgress(w, httptest.NewRequest("GET", "/x", nil), apikeys.Key{}, other)
	if rows := decode(t, w)["progress"].([]any); len(rows) != 0 {
		t.Errorf("cross-user progress = %v", rows)
	}
}

// The documented PUT /api/v1/media/progress behavior: a heartbeat lands,
// answers the stored row, and an outsider's write is a 404.
func TestMediaProgressPut(t *testing.T) {
	f := newMediaFixture(t)
	entries, _ := f.h.k.Media.ListCatalog(t.Context(), f.drive.ID, f.folder.ID, media.CatMovie)
	nodeID := entries[0].NodeID
	body := fmt.Sprintf(`{"driveId":%q,"nodeId":%q,"kind":"video","title":"Movie","pos":42,"dur":100}`, f.drive.ID, nodeID)
	w := httptest.NewRecorder()
	f.h.mediaProgressPut(w, httptest.NewRequest("PUT", "/x", strings.NewReader(body)), apikeys.Key{}, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := decode(t, w)
	if got["pos"] != float64(42) || got["watched"] != false || got["kind"] != "video" {
		t.Errorf("progress = %v", got)
	}
	if p, found, _ := f.h.k.Media.GetProgress(t.Context(), "ada", nodeID); !found || p.Pos != 42 {
		t.Errorf("stored progress = %+v (found=%v)", p, found)
	}

	// An outsider can't write progress against a drive they can't see.
	outsider, err := f.h.k.Users.CreateUser(t.Context(), "erin", "Erin", "password123")
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	f.h.mediaProgressPut(w, httptest.NewRequest("PUT", "/x", strings.NewReader(body)), apikeys.Key{}, outsider)
	if w.Code != http.StatusNotFound {
		t.Errorf("outsider progress put status = %d, want 404", w.Code)
	}
}

// POST /api/v1/media/watched flips the bit; off clears the position.
func TestMediaWatched(t *testing.T) {
	f := newMediaFixture(t)
	entries, _ := f.h.k.Media.ListCatalog(t.Context(), f.drive.ID, f.folder.ID, media.CatMovie)
	nodeID := entries[0].NodeID
	mark := func(on bool) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"driveId":%q,"nodeId":%q,"watched":%v}`, f.drive.ID, nodeID, on)
		w := httptest.NewRecorder()
		f.h.mediaWatched(w, httptest.NewRequest("POST", "/x", strings.NewReader(body)), apikeys.Key{}, f.user)
		return w
	}
	if w := mark(true); w.Code != http.StatusOK {
		t.Fatalf("watched status = %d: %s", w.Code, w.Body.String())
	}
	if p, found, _ := f.h.k.Media.GetProgress(t.Context(), "ada", nodeID); !found || !p.Watched() {
		t.Errorf("watched not stored: %+v (found=%v)", p, found)
	}
	if w := mark(false); w.Code != http.StatusOK {
		t.Fatalf("unwatch status = %d", w.Code)
	}
	if _, found, _ := f.h.k.Media.GetProgress(t.Context(), "ada", nodeID); found {
		t.Error("unwatch left a progress row behind")
	}
}

// The watchlist/favorites surface: add, list, remove; junk list names
// are a 404.
func TestMediaLists(t *testing.T) {
	f := newMediaFixture(t)
	entries, _ := f.h.k.Media.ListCatalog(t.Context(), f.drive.ID, f.folder.ID, media.CatMovie)
	slug := entries[0].ID
	put := func(list string, on bool) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"driveId":%q,"folderId":%q,"kind":"movies","slug":%q,"title":"Movie","on":%v}`,
			f.drive.ID, f.folder.ID, slug, on)
		req := httptest.NewRequest("PUT", "/x", strings.NewReader(body))
		req.SetPathValue("list", list)
		w := httptest.NewRecorder()
		f.h.mediaListPut(w, req, apikeys.Key{}, f.user)
		return w
	}
	if w := put("watchlist", true); w.Code != http.StatusOK {
		t.Fatalf("list put status = %d: %s", w.Code, w.Body.String())
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("list", "watchlist")
	w := httptest.NewRecorder()
	f.h.mediaListGet(w, req, apikeys.Key{}, f.user)
	items := decode(t, w)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["slug"] != slug {
		t.Fatalf("watchlist = %v", items)
	}
	if w := put("watchlist", false); w.Code != http.StatusOK {
		t.Fatalf("list remove status = %d", w.Code)
	}
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("list", "watchlist")
	f.h.mediaListGet(w, req, apikeys.Key{}, f.user)
	if items := decode(t, w)["items"].([]any); len(items) != 0 {
		t.Errorf("watchlist after remove = %v", items)
	}
	// Junk list name → 404.
	req = httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("list", "junk")
	w = httptest.NewRecorder()
	f.h.mediaListGet(w, req, apikeys.Key{}, f.user)
	if w.Code != http.StatusNotFound {
		t.Errorf("junk list status = %d", w.Code)
	}
}

// The playlist lifecycle: create, list, get, patch (rename + replace
// tracks), delete — and cross-user isolation.
func TestMediaPlaylists(t *testing.T) {
	f := newMediaFixture(t)
	w := httptest.NewRecorder()
	f.h.mediaPlaylistCreate(w, httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"Road trip"}`)), apikeys.Key{}, f.user)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}
	pl := decode(t, w)
	plID := pl["id"].(string)
	if pl["name"] != "Road trip" || len(pl["tracks"].([]any)) != 0 {
		t.Errorf("created playlist = %v", pl)
	}

	// Patch: rename and set tracks in one call.
	entries, _ := f.h.k.Media.ListCatalog(t.Context(), f.drive.ID, f.folder.ID, media.CatMovie)
	body := fmt.Sprintf(`{"name":"Long drive","tracks":[{"driveId":%q,"nodeId":%q,"title":"Movie"}]}`,
		f.drive.ID, entries[0].NodeID)
	req := httptest.NewRequest("PATCH", "/x", strings.NewReader(body))
	req.SetPathValue("id", plID)
	w = httptest.NewRecorder()
	f.h.mediaPlaylistPatch(w, req, apikeys.Key{}, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("patch status = %d: %s", w.Code, w.Body.String())
	}
	got := decode(t, w)
	if got["name"] != "Long drive" || len(got["tracks"].([]any)) != 1 {
		t.Errorf("patched playlist = %v", got)
	}

	// Another user's key can't see or delete it.
	other := users.User{Username: "erin"}
	req = httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("id", plID)
	w = httptest.NewRecorder()
	f.h.mediaPlaylistGet(w, req, apikeys.Key{}, other)
	if w.Code != http.StatusNotFound {
		t.Errorf("cross-user playlist get status = %d", w.Code)
	}
	req = httptest.NewRequest("DELETE", "/x", nil)
	req.SetPathValue("id", plID)
	w = httptest.NewRecorder()
	f.h.mediaPlaylistDelete(w, req, apikeys.Key{}, other)
	if w.Code != http.StatusNotFound {
		t.Errorf("cross-user playlist delete status = %d", w.Code)
	}

	// The owner deletes it.
	req = httptest.NewRequest("DELETE", "/x", nil)
	req.SetPathValue("id", plID)
	w = httptest.NewRecorder()
	f.h.mediaPlaylistDelete(w, req, apikeys.Key{}, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d", w.Code)
	}
	w = httptest.NewRecorder()
	f.h.mediaPlaylists(w, httptest.NewRequest("GET", "/x", nil), apikeys.Key{}, f.user)
	if pls := decode(t, w)["playlists"].([]any); len(pls) != 0 {
		t.Errorf("playlists after delete = %v", pls)
	}
}
