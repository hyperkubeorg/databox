package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/shares"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// driveFixture seeds a kvxtest-backed handler set with ada, her shared
// drive, and a folder + file (blob rows faked — blob paths belong to
// the live smoke).
type driveFixture struct {
	h      *handlers
	user   users.User
	drive  drives.Drive
	folder nodes.Node
	file   nodes.Node
}

func newDriveFixture(t *testing.T) *driveFixture {
	t.Helper()
	h := testHandlers(t)
	// Wire the drive-domain stores over the same fake DB.
	db := h.k.Users.DB
	h.k.Drives = &drives.Store{DB: db, Users: h.k.Users}
	h.k.Nodes = &nodes.Store{DB: db, Users: h.k.Users}
	h.k.Shares = &shares.Store{DB: db, Nodes: h.k.Nodes, Drives: h.k.Drives, Users: h.k.Users}
	ctx := t.Context()
	user, err := h.k.Users.CreateUser(ctx, "ada", "Ada", "password123")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	d, err := h.k.Drives.CreateShared(ctx, "ada", "Team")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	folder, err := h.k.Nodes.CreateFolder(ctx, d.ID, nodes.RootID, "Docs", "ada")
	if err != nil {
		t.Fatalf("folder: %v", err)
	}
	file, err := h.k.Nodes.CommitFile(ctx, d.ID, folder.ID, "a.txt", "AAAAAAAAAAAAAAAA", "text/plain", 42, "ada", true)
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	return &driveFixture{h: h, user: user, drive: d, folder: folder, file: file}
}

// decode fails the test on junk.
func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return got
}

// The documented GET /api/v1/drive/drives shape (docs/api.md).
func TestDriveListShape(t *testing.T) {
	f := newDriveFixture(t)
	w := httptest.NewRecorder()
	f.h.driveList(w, httptest.NewRequest("GET", "/api/v1/drive/drives", nil), apikeys.Key{}, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	got := decode(t, w)
	ds := got["drives"].([]any)
	if len(ds) != 1 {
		t.Fatalf("drives = %v", ds)
	}
	row := ds[0].(map[string]any)
	for _, k := range []string{"id", "name", "type", "role", "owner", "createdAt"} {
		if _, present := row[k]; !present {
			t.Errorf("drive row missing %q: %v", k, row)
		}
	}
	if row["type"] != "shared" || row["role"] != "owner" {
		t.Errorf("drive row = %v", row)
	}
}

// The documented node resource shape, via stat and folder listing.
func TestDriveStatAndListShape(t *testing.T) {
	f := newDriveFixture(t)
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("node", f.file.ID)
	w := httptest.NewRecorder()
	f.h.driveStat(w, req, apikeys.Key{}, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("stat status = %d: %s", w.Code, w.Body.String())
	}
	got := decode(t, w)
	want := map[string]any{
		"id": f.file.ID, "name": "a.txt", "dir": false,
		"size": float64(42), "contentType": "text/plain", "rev": float64(1),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("stat[%q] = %v, want %v", k, got[k], v)
		}
	}
	for _, k := range []string{"createdAt", "modifiedAt", "modifiedBy"} {
		if _, present := got[k]; !present {
			t.Errorf("stat missing %q", k)
		}
	}

	// Folder listing pages with a cursor.
	req = httptest.NewRequest("GET", "/x?limit=1", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("node", nodes.RootID)
	w = httptest.NewRecorder()
	f.h.driveFolder(w, req, apikeys.Key{}, f.user)
	got = decode(t, w)
	if _, present := got["nodes"]; !present {
		t.Fatalf("listing missing nodes: %v", got)
	}
	if _, present := got["nextCursor"]; !present {
		t.Fatalf("listing missing nextCursor: %v", got)
	}
}

// Mutations return the resource with its new revision; conflicts and
// denials use the documented envelope codes.
func TestDriveMutations(t *testing.T) {
	f := newDriveFixture(t)
	post := func(handler func(http.ResponseWriter, *http.Request, apikeys.Key, users.User), body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest("POST", "/x", strings.NewReader(body)), apikeys.Key{}, f.user)
		return w
	}
	// mkdir.
	w := post(f.h.driveMkdir, `{"driveId":"`+f.drive.ID+`","parentId":"root","name":"New"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("mkdir = %d: %s", w.Code, w.Body.String())
	}
	made := decode(t, w)
	if made["dir"] != true || made["name"] != "New" {
		t.Fatalf("mkdir body = %v", made)
	}
	// mkdir collision → 409 conflict envelope.
	w = post(f.h.driveMkdir, `{"driveId":"`+f.drive.ID+`","parentId":"root","name":"new"}`)
	if w.Code != http.StatusConflict || decode(t, w)["code"] != "conflict" {
		t.Fatalf("dup mkdir = %d %s", w.Code, w.Body.String())
	}
	// rename returns the resource.
	w = post(f.h.driveRename, `{"driveId":"`+f.drive.ID+`","nodeId":"`+f.file.ID+`","name":"b.txt"}`)
	if w.Code != http.StatusOK || decode(t, w)["name"] != "b.txt" {
		t.Fatalf("rename = %d %s", w.Code, w.Body.String())
	}
	// move into the new folder.
	newID := made["id"].(string)
	w = post(f.h.driveMove, `{"driveId":"`+f.drive.ID+`","nodeId":"`+f.file.ID+`","parentId":"`+newID+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("move = %d %s", w.Code, w.Body.String())
	}
	// A stranger reads not_found, never forbidden — a key can't map the
	// keyspace by probing ids.
	stranger, err := f.h.k.Users.CreateUser(t.Context(), "eve", "Eve", "password123")
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	f.h.driveMkdir(w, httptest.NewRequest("POST", "/x", strings.NewReader(`{"driveId":"`+f.drive.ID+`","parentId":"root","name":"X"}`)), apikeys.Key{}, stranger)
	if w.Code != http.StatusNotFound || decode(t, w)["code"] != "not_found" {
		t.Fatalf("stranger mkdir = %d %s, want 404 not_found", w.Code, w.Body.String())
	}
}

// Versions list + restore round-trip (KV only; download is live-smoke).
func TestDriveVersionsAndRestoreShape(t *testing.T) {
	f := newDriveFixture(t)
	// Second content version.
	if _, err := f.h.k.Nodes.CommitFile(t.Context(), f.drive.ID, f.folder.ID, "a.txt", "BBBBBBBBBBBBBBBB", "text/plain", 50, "ada", true); err != nil {
		t.Fatalf("commit v2: %v", err)
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("node", f.file.ID)
	w := httptest.NewRecorder()
	f.h.driveVersions(w, req, apikeys.Key{}, f.user)
	got := decode(t, w)
	vs := got["versions"].([]any)
	if len(vs) != 2 {
		t.Fatalf("versions = %v", vs)
	}
	head := vs[0].(map[string]any)
	for _, k := range []string{"rev", "n", "size", "by", "at"} {
		if _, present := head[k]; !present {
			t.Errorf("version row missing %q: %v", k, head)
		}
	}
	if head["n"] != float64(2) {
		t.Errorf("versions not newest-first: %v", vs)
	}
	// Restore the oldest — a NEW revision of the old content.
	rev := vs[1].(map[string]any)["rev"].(string)
	w = httptest.NewRecorder()
	f.h.driveRestore(w, httptest.NewRequest("POST", "/x", strings.NewReader(`{"driveId":"`+f.drive.ID+`","nodeId":"`+f.file.ID+`","rev":"`+rev+`"}`)), apikeys.Key{}, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", w.Code, w.Body.String())
	}
	if got := decode(t, w); got["rev"] != float64(3) || got["size"] != float64(42) {
		t.Fatalf("restore body = %v", got)
	}
}

// Share links: create → list → revoke, with the documented shape.
func TestDriveShareLinkShape(t *testing.T) {
	f := newDriveFixture(t)
	w := httptest.NewRecorder()
	body := `{"driveId":"` + f.drive.ID + `","nodeId":"` + f.file.ID + `","perms":"download","password":"pw","expiresIn":"168h"}`
	f.h.driveShareCreate(w, httptest.NewRequest("POST", "/x", strings.NewReader(body)), apikeys.Key{}, f.user)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	sh := decode(t, w)
	for _, k := range []string{"token", "url", "driveId", "nodeId", "perms", "password", "expiresAt", "by", "createdAt"} {
		if _, present := sh[k]; !present {
			t.Errorf("share missing %q: %v", k, sh)
		}
	}
	if sh["password"] != true || sh["perms"] != "download" || !strings.HasPrefix(sh["url"].(string), "/s/") {
		t.Errorf("share = %v", sh)
	}
	// List sees it.
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("node", f.file.ID)
	w = httptest.NewRecorder()
	f.h.driveShares(w, req, apikeys.Key{}, f.user)
	if rows := decode(t, w)["shares"].([]any); len(rows) != 1 {
		t.Fatalf("shares = %v", rows)
	}
	// Revoke removes it.
	req = httptest.NewRequest("DELETE", "/x", nil)
	req.SetPathValue("token", sh["token"].(string))
	w = httptest.NewRecorder()
	f.h.driveShareRevoke(w, req, apikeys.Key{}, f.user)
	if w.Code != http.StatusOK || decode(t, w)["revoked"] != true {
		t.Fatalf("revoke = %d %s", w.Code, w.Body.String())
	}
}

// DELETE /nodes is permanent and reports the freed bytes.
func TestDriveDeleteShape(t *testing.T) {
	f := newDriveFixture(t)
	req := httptest.NewRequest("DELETE", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("node", f.folder.ID)
	w := httptest.NewRecorder()
	f.h.driveDelete(w, req, apikeys.Key{}, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
	got := decode(t, w)
	if got["deleted"] != true || got["freedBytes"] != float64(42) {
		t.Fatalf("delete body = %v", got)
	}
}
