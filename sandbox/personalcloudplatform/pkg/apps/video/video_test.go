package video

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/shares"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// fixture is the app over kvxtest: ada + a shared drive + a registered,
// scanned video folder holding a 2-episode show and a movie.
type fixture struct {
	h      *handlers
	user   users.User
	sess   users.Session
	drive  drives.Drive
	folder nodes.Node
	movie  media.CatalogEntry
	series media.CatalogEntry
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := kvxtest.New(t)
	us := &users.Store{DB: db}
	ds := &drives.Store{DB: db, Users: us}
	ns := &nodes.Store{DB: db, Users: us}
	ss := &shares.Store{DB: db, Nodes: ns, Drives: ds, Users: us}
	ms := &media.Store{DB: db, Nodes: ns, Drives: ds}
	k := &kernel.App{
		Users: us, Site: &site.Store{DB: db}, Nodes: ns, Drives: ds, Shares: ss, Media: ms,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	ctx := t.Context()
	user, err := us.CreateUser(ctx, "ada", "Ada", "password123")
	if err != nil {
		t.Fatal(err)
	}
	d, err := ds.CreateShared(ctx, "ada", "Media")
	if err != nil {
		t.Fatal(err)
	}
	folder, err := ns.CreateFolder(ctx, d.ID, nodes.RootID, "Video", "ada")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Show S01E01.mp4", "Show S01E02.mp4", "Movie (2023).mp4"} {
		if _, err := ns.CommitFile(ctx, d.ID, folder.ID, name, "AAAAAAAAAAAA", "video/mp4", 4, "ada", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := ms.Register(ctx, d.ID, folder.ID, media.KindVideo, "ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Rescan(ctx, d.ID, folder.ID); err != nil {
		t.Fatal(err)
	}
	movies, _ := ms.ListCatalog(ctx, d.ID, folder.ID, media.CatMovie)
	series, _ := ms.ListCatalog(ctx, d.ID, folder.ID, media.CatSeries)
	if len(movies) != 1 || len(series) != 1 {
		t.Fatalf("fixture catalog: movies=%v series=%v", movies, series)
	}
	return &fixture{
		h: h, user: user, sess: users.Session{Username: "ada", CSRF: "tok"},
		drive: d, folder: folder, movie: movies[0], series: series[0],
	}
}

func TestHomeRendersShelves(t *testing.T) {
	f := newFixture(t)
	w := httptest.NewRecorder()
	f.h.home(w, httptest.NewRequest("GET", "/video", nil), f.sess, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	out := w.Body.String()
	for _, want := range []string{"Show", "Movie", "Video", "/video/f/" + f.drive.ID + "/" + f.folder.ID} {
		if !strings.Contains(out, want) {
			t.Errorf("home missing %q", want)
		}
	}
}

func TestLibAndTitlePages(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("folder", f.folder.ID)
	w := httptest.NewRecorder()
	f.h.folder(w, req, f.sess, f.user)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Movies") {
		t.Fatalf("lib page = %d", w.Code)
	}

	// Series detail: episodes listed in order, Next up = E01.
	req = httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("folder", f.folder.ID)
	req.SetPathValue("kind", media.CatSeries)
	req.SetPathValue("slug", f.series.ID)
	w = httptest.NewRecorder()
	f.h.title(w, req, f.sess, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("title page = %d", w.Code)
	}
	out := w.Body.String()
	for _, want := range []string{"Season 1", "E01", "E02", "Play S01E01", "/drive/app/video?drive="} {
		if !strings.Contains(out, want) {
			t.Errorf("series page missing %q", want)
		}
	}
}

func TestAccessDenialIsNotFound(t *testing.T) {
	f := newFixture(t)
	outsider, err := f.h.k.Users.CreateUser(t.Context(), "erin", "Erin", "password123")
	if err != nil {
		t.Fatal(err)
	}
	sess := users.Session{Username: "erin", CSRF: "tok"}

	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("folder", f.folder.ID)
	w := httptest.NewRecorder()
	f.h.folder(w, req, sess, outsider)
	if w.Code != http.StatusNotFound {
		t.Errorf("outsider lib page = %d, want 404", w.Code)
	}

	// Home renders fine but empty (membership is the subscription).
	w = httptest.NewRecorder()
	f.h.home(w, httptest.NewRequest("GET", "/video", nil), sess, outsider)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), f.folder.ID) {
		t.Errorf("outsider home leaked the folder (status %d)", w.Code)
	}

	// Join → the shelf appears.
	if err := f.h.k.Drives.SetMember(t.Context(), f.drive.ID, "erin", drives.RoleViewer); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	f.h.home(w, httptest.NewRequest("GET", "/video", nil), sess, outsider)
	if !strings.Contains(w.Body.String(), f.folder.ID) {
		t.Errorf("member home missing the shelf")
	}
}

func postForm(t *testing.T, h func(http.ResponseWriter, *http.Request, users.Session, users.User),
	sess users.Session, user users.User, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/x", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "fetch")
	req.Header.Set("X-CSRF", sess.CSRF)
	w := httptest.NewRecorder()
	h(w, req, sess, user)
	return w
}

func TestProgressRoundTrip(t *testing.T) {
	f := newFixture(t)
	w := postForm(t, f.h.progressPost, f.sess, f.user, url.Values{
		"drive": {f.drive.ID}, "node": {f.movie.NodeID},
		"pos": {"120.5"}, "dur": {"3600"}, "title": {"Movie"}, "kind": {"video"},
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("heartbeat = %d: %s", w.Code, w.Body.String())
	}
	req := httptest.NewRequest("GET", "/video/progress?node="+f.movie.NodeID, nil)
	w = httptest.NewRecorder()
	f.h.progressGet(w, req, f.sess, f.user)
	if !strings.Contains(w.Body.String(), "120.5") {
		t.Errorf("resume lookup = %s", w.Body.String())
	}
	// Home now shows Continue watching with a resume bar.
	w = httptest.NewRecorder()
	f.h.home(w, httptest.NewRequest("GET", "/video", nil), f.sess, f.user)
	if !strings.Contains(w.Body.String(), "Continue watching") || !strings.Contains(w.Body.String(), "mresume") {
		t.Errorf("home missing continue-watching after a heartbeat")
	}

	// Bad CSRF is a 403.
	req = httptest.NewRequest("POST", "/x", strings.NewReader("drive="+f.drive.ID))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	f.h.progressPost(w, req, f.sess, f.user)
	if w.Code != http.StatusForbidden {
		t.Errorf("csrf-less heartbeat = %d", w.Code)
	}

	// A heartbeat for someone else's drive writes nothing.
	outsider, _ := f.h.k.Users.CreateUser(t.Context(), "erin", "Erin", "password123")
	osess := users.Session{Username: "erin", CSRF: "tok"}
	w = postForm(t, f.h.progressPost, osess, outsider, url.Values{
		"drive": {f.drive.ID}, "node": {f.movie.NodeID}, "pos": {"5"}, "dur": {"10"},
	})
	if w.Code != http.StatusNotFound {
		t.Errorf("outsider heartbeat = %d, want 404", w.Code)
	}
}

// TestContinueWatchingFollowsRegistration: unregistering a folder hides
// its Continue-watching entries (progress rows survive), re-registering
// brings them back with the position intact.
func TestContinueWatchingFollowsRegistration(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	w := postForm(t, f.h.progressPost, f.sess, f.user, url.Values{
		"drive": {f.drive.ID}, "node": {f.movie.NodeID},
		"pos": {"120"}, "dur": {"3600"}, "title": {"Movie"}, "kind": {"video"},
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("heartbeat = %d", w.Code)
	}
	home := func() string {
		hw := httptest.NewRecorder()
		f.h.home(hw, httptest.NewRequest("GET", "/video", nil), f.sess, f.user)
		return hw.Body.String()
	}
	if !strings.Contains(home(), "Continue watching") {
		t.Fatal("no continue-watching before unregister")
	}

	if err := f.h.k.Media.Unregister(ctx, f.drive.ID, f.folder.ID, media.KindVideo); err != nil {
		t.Fatal(err)
	}
	if out := home(); strings.Contains(out, "Continue watching") {
		t.Error("unregistered folder still on continue-watching")
	}
	// The row survives in the store (rebuildable-cache philosophy).
	if p, found, _ := f.h.k.Media.GetProgress(ctx, "ada", f.movie.NodeID); !found || p.Pos != 120 {
		t.Fatalf("progress row deleted: %+v found=%v", p, found)
	}

	// Re-register + rescan → the shelf returns with the resume bar.
	if err := f.h.k.Media.Register(ctx, f.drive.ID, f.folder.ID, media.KindVideo, "ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.k.Media.Rescan(ctx, f.drive.ID, f.folder.ID); err != nil {
		t.Fatal(err)
	}
	if out := home(); !strings.Contains(out, "Continue watching") || !strings.Contains(out, "mresume") {
		t.Error("continue-watching did not return after re-register")
	}
}

// postFrame drives the frame-capture endpoint with a multipart body.
func postFrame(t *testing.T, f *fixture, sess users.Session, user users.User,
	fields map[string]string, img []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if img != nil {
		fw, err := mw.CreateFormFile("image", "frame.jpg")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write(img)
	}
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/video/frame", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	f.h.frameSave(w, req, sess, user)
	return w
}

var jpegBytes = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0, 1, 2, 3}

func TestFrameSave(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ns := f.h.k.Nodes
	// Show/Season 1/episode — the poster must land at the SHOW level.
	show, err := ns.CreateFolder(ctx, f.drive.ID, f.folder.ID, "Show", "ada")
	if err != nil {
		t.Fatal(err)
	}
	season, err := ns.CreateFolder(ctx, f.drive.ID, show.ID, "Season 1", "ada")
	if err != nil {
		t.Fatal(err)
	}
	ep, err := ns.CommitFile(ctx, f.drive.ID, season.ID, "Show S01E03.mp4", "BBBBBBBBBBBB", "video/mp4", 4, "ada", false)
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]string{"csrf": f.sess.CSRF, "drive": f.drive.ID, "node": ep.ID}
	with := func(kind string) map[string]string {
		m := map[string]string{"kind": kind}
		for k, v := range base {
			m[k] = v
		}
		return m
	}

	// Kind validation.
	if w := postFrame(t, f, f.sess, f.user, map[string]string{"csrf": f.sess.CSRF, "drive": f.drive.ID, "node": ep.ID, "kind": "banner"}, jpegBytes); w.Code != http.StatusBadRequest {
		t.Errorf("bad kind = %d, want 400", w.Code)
	}
	// Bad CSRF.
	if w := postFrame(t, f, f.sess, f.user, map[string]string{"csrf": "nope", "drive": f.drive.ID, "node": ep.ID, "kind": "poster"}, jpegBytes); w.Code != http.StatusForbidden {
		t.Errorf("bad csrf = %d, want 403", w.Code)
	}
	// Editors only: a viewer-role member gets 403.
	viewer, err := f.h.k.Users.CreateUser(ctx, "erin", "Erin", "password123")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.h.k.Drives.SetMember(ctx, f.drive.ID, "erin", drives.RoleViewer); err != nil {
		t.Fatal(err)
	}
	vsess := users.Session{Username: "erin", CSRF: "tok"}
	vf := map[string]string{"csrf": vsess.CSRF, "drive": f.drive.ID, "node": ep.ID, "kind": "poster"}
	if w := postFrame(t, f, vsess, viewer, vf, jpegBytes); w.Code != http.StatusForbidden {
		t.Errorf("viewer frame save = %d, want 403", w.Code)
	}
	// Non-images (and SVG) never become art.
	if w := postFrame(t, f, f.sess, f.user, with("poster"), []byte("just some text, not pixels")); w.Code != http.StatusBadRequest {
		t.Errorf("text frame = %d, want 400", w.Code)
	}
	if w := postFrame(t, f, f.sess, f.user, with("poster"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)); w.Code != http.StatusBadRequest {
		t.Errorf("svg frame = %d, want 400", w.Code)
	}

	// Poster: lands as poster.jpg in the SHOW folder, above "Season 1".
	w := postFrame(t, f, f.sess, f.user, with("poster"), jpegBytes)
	if w.Code != http.StatusOK {
		t.Fatalf("poster save = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		OK    bool   `json:"ok"`
		Saved string `json:"saved"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.OK || out.Saved != "poster.jpg" {
		t.Fatalf("poster response = %s", w.Body.String())
	}
	if _, found, _ := ns.GetChild(ctx, f.drive.ID, show.ID, "poster.jpg"); !found {
		t.Error("poster.jpg missing from the show folder")
	}
	if _, found, _ := ns.GetChild(ctx, f.drive.ID, season.ID, "poster.jpg"); found {
		t.Error("poster.jpg landed inside the season folder")
	}

	// Preview: "<video filename>.jpg" BESIDE the episode.
	w = postFrame(t, f, f.sess, f.user, with("thumb"), jpegBytes)
	if w.Code != http.StatusOK {
		t.Fatalf("thumb save = %d: %s", w.Code, w.Body.String())
	}
	if _, found, _ := ns.GetChild(ctx, f.drive.ID, season.ID, "Show S01E03.mp4.jpg"); !found {
		t.Error("preview art missing beside the episode")
	}

	// The covering registered folder rescans in the background: the
	// series entry picks the captured poster up as its art.
	deadline := time.Now().Add(5 * time.Second)
	for {
		series, _ := f.h.k.Media.ListCatalog(ctx, f.drive.ID, f.folder.ID, media.CatSeries)
		done := false
		for _, s := range series {
			if s.Title == "Show" && s.ArtNode != "" {
				done = true
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background rescan never picked up the poster: %+v", series)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWatchlistToggleAndHide(t *testing.T) {
	f := newFixture(t)
	w := postForm(t, f.h.listToggle, f.sess, f.user, url.Values{
		"drive": {f.drive.ID}, "folder": {f.folder.ID},
		"kind": {media.CatMovie}, "slug": {f.movie.ID}, "title": {"Movie"},
		"list": {media.ListWatch}, "on": {"1"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("toggle = %d: %s", w.Code, w.Body.String())
	}
	hw := httptest.NewRecorder()
	f.h.home(hw, httptest.NewRequest("GET", "/video", nil), f.sess, f.user)
	if !strings.Contains(hw.Body.String(), "My list") {
		t.Errorf("home missing the watchlist shelf")
	}

	// Hide the folder → its shelf disappears from home.
	w = postForm(t, f.h.hideToggle, f.sess, f.user, url.Values{
		"drive": {f.drive.ID}, "folder": {f.folder.ID}, "on": {"1"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("hide = %d", w.Code)
	}
	hw = httptest.NewRecorder()
	f.h.home(hw, httptest.NewRequest("GET", "/video", nil), f.sess, f.user)
	if strings.Contains(hw.Body.String(), "browse all") {
		t.Errorf("hidden folder still shelved")
	}
	if !strings.Contains(hw.Body.String(), "hidden") {
		t.Errorf("manage strip missing the hidden chip")
	}
}

func TestSearch(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest("GET", "/video/search?q=movie", nil)
	w := httptest.NewRecorder()
	f.h.search(w, req, f.sess, f.user)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Movie") {
		t.Errorf("search = %d", w.Code)
	}
	req = httptest.NewRequest("GET", "/video/search?q=zzzznothing", nil)
	w = httptest.NewRecorder()
	f.h.search(w, req, f.sess, f.user)
	if !strings.Contains(w.Body.String(), "No matches") {
		t.Errorf("search empty state missing")
	}
}
