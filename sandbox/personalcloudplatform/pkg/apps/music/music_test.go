package music

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/shares"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// fixture: ada + a shared drive + a registered, scanned music folder
// with one two-track album (path conventions — untagged files).
type fixture struct {
	h      *handlers
	user   users.User
	sess   users.Session
	drive  drives.Drive
	folder nodes.Node
	album  media.CatalogEntry
	tracks []media.CatalogEntry
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
	folder, err := ns.CreateFolder(ctx, d.ID, nodes.RootID, "Music", "ada")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := ns.CreateFolder(ctx, d.ID, folder.ID, "Artist One", "ada")
	if err != nil {
		t.Fatal(err)
	}
	albumDir, err := ns.CreateFolder(ctx, d.ID, artist.ID, "Album Alpha", "ada")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"01 Song A.mp3", "02 Song B.mp3"} {
		blobID := kvx.NewID()
		if err := db.PutBlob(ctx, nodes.BlobKey(d.ID, blobID), bytes.NewReader([]byte{0xff, 0xfb, 1, 2}), "audio/mpeg"); err != nil {
			t.Fatal(err)
		}
		if _, err := ns.CommitFile(ctx, d.ID, albumDir.ID, name, blobID, "audio/mpeg", 4, "ada", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := ms.Register(ctx, d.ID, folder.ID, media.KindMusic, "ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Rescan(ctx, d.ID, folder.ID); err != nil {
		t.Fatal(err)
	}
	albums, _ := ms.ListCatalog(ctx, d.ID, folder.ID, media.CatAlbum)
	if len(albums) != 1 {
		t.Fatalf("fixture albums = %v", albums)
	}
	tracks, _ := ms.ListCatalogUnder(ctx, d.ID, folder.ID, media.CatTrack, albums[0].ID)
	if len(tracks) != 2 {
		t.Fatalf("fixture tracks = %v", tracks)
	}
	return &fixture{
		h: h, user: user, sess: users.Session{Username: "ada", CSRF: "tok"},
		drive: d, folder: folder, album: albums[0], tracks: tracks,
	}
}

func postForm(t *testing.T, h func(http.ResponseWriter, *http.Request, users.Session, users.User),
	sess users.Session, user users.User, path map[string]string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/x", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "fetch")
	req.Header.Set("X-CSRF", sess.CSRF)
	for k, v := range path {
		req.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	h(w, req, sess, user)
	return w
}

func TestHomeAndFolderRender(t *testing.T) {
	f := newFixture(t)
	w := httptest.NewRecorder()
	f.h.home(w, httptest.NewRequest("GET", "/music", nil), f.sess, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("home = %d", w.Code)
	}
	out := w.Body.String()
	for _, want := range []string{"Album Alpha", "Artist One", `id="music-root"`, "music.js"} {
		if !strings.Contains(out, want) {
			t.Errorf("home missing %q", want)
		}
	}

	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("folder", f.folder.ID)
	w = httptest.NewRecorder()
	f.h.folder(w, req, f.sess, f.user)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Album Alpha") {
		t.Fatalf("folder page = %d", w.Code)
	}
}

func TestAlbumPageListsPlayableTracks(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("folder", f.folder.ID)
	req.SetPathValue("slug", f.album.ID)
	w := httptest.NewRecorder()
	f.h.album(w, req, f.sess, f.user)
	if w.Code != http.StatusOK {
		t.Fatalf("album = %d", w.Code)
	}
	out := w.Body.String()
	for _, want := range []string{
		"Song A", "Song B", "Play album",
		"/drive/file/" + f.drive.ID + "/" + f.tracks[0].NodeID + "?inline=1", // Range stream source
		`data-queue`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("album page missing %q", want)
		}
	}

	// A stale slug lands on the folder page, not a 404.
	req = httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("folder", f.folder.ID)
	req.SetPathValue("slug", "gone-0000")
	w = httptest.NewRecorder()
	f.h.album(w, req, f.sess, f.user)
	if w.Code != http.StatusSeeOther {
		t.Errorf("stale album slug = %d, want 303", w.Code)
	}
}

func TestArtistPage(t *testing.T) {
	f := newFixture(t)
	artists, _ := f.h.k.Media.ListCatalog(t.Context(), f.drive.ID, f.folder.ID, media.CatArtist)
	if len(artists) != 1 {
		t.Fatalf("artists = %v", artists)
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("folder", f.folder.ID)
	req.SetPathValue("slug", artists[0].ID)
	w := httptest.NewRecorder()
	f.h.artist(w, req, f.sess, f.user)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Album Alpha") {
		t.Fatalf("artist page = %d", w.Code)
	}
}

func TestPlaylistLifecycle(t *testing.T) {
	f := newFixture(t)
	// Create.
	w := postForm(t, f.h.playlistCreate, f.sess, f.user, nil, url.Values{"name": {"Road trip"}})
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	pls, _ := f.h.k.Media.Playlists(t.Context(), "ada")
	if len(pls) != 1 {
		t.Fatalf("playlists = %v", pls)
	}
	plID := pls[0].ID

	// Add both tracks.
	for _, tr := range f.tracks {
		w = postForm(t, f.h.playlistAdd, f.sess, f.user, map[string]string{"pl": plID}, url.Values{
			"drive": {f.drive.ID}, "node": {tr.NodeID}, "title": {tr.Title}, "artist": {tr.Artist},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("add = %d: %s", w.Code, w.Body.String())
		}
	}
	// Reorder: move index 1 up.
	w = postForm(t, f.h.playlistMove, f.sess, f.user, map[string]string{"pl": plID}, url.Values{
		"idx": {"1"}, "dir": {"up"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("move = %d", w.Code)
	}
	pl, _, _ := f.h.k.Media.GetPlaylist(t.Context(), "ada", plID)
	if len(pl.Tracks) != 2 || pl.Tracks[0].Title != "Song B" {
		t.Fatalf("reorder = %+v", pl.Tracks)
	}

	// Page renders the queue.
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("pl", plID)
	w = httptest.NewRecorder()
	f.h.playlist(w, req, f.sess, f.user)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Song B") {
		t.Fatalf("playlist page = %d", w.Code)
	}

	// Remove + rename + delete.
	w = postForm(t, f.h.playlistRemove, f.sess, f.user, map[string]string{"pl": plID}, url.Values{"idx": {"0"}})
	if w.Code != http.StatusOK {
		t.Fatalf("remove = %d", w.Code)
	}
	w = postForm(t, f.h.playlistRename, f.sess, f.user, nil, url.Values{"pl": {plID}, "name": {"Long drive"}})
	if w.Code != http.StatusOK {
		t.Fatalf("rename = %d", w.Code)
	}
	w = postForm(t, f.h.playlistDelete, f.sess, f.user, nil, url.Values{"pl": {plID}})
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	if pls, _ := f.h.k.Media.Playlists(t.Context(), "ada"); len(pls) != 0 {
		t.Fatalf("playlist survived delete: %v", pls)
	}

	// Adding a track from a drive the member can't read is refused.
	outsider, _ := f.h.k.Users.CreateUser(t.Context(), "erin", "Erin", "password123")
	osess := users.Session{Username: "erin", CSRF: "tok"}
	opl, _ := f.h.k.Media.CreatePlaylist(t.Context(), "erin", "Mine")
	w = postForm(t, f.h.playlistAdd, osess, outsider, map[string]string{"pl": opl.ID}, url.Values{
		"drive": {f.drive.ID}, "node": {f.tracks[0].NodeID}, "title": {"X"},
	})
	if w.Code == http.StatusOK {
		t.Errorf("outsider playlist add succeeded")
	}
}

func TestMusicAccessDenial(t *testing.T) {
	f := newFixture(t)
	outsider, _ := f.h.k.Users.CreateUser(t.Context(), "erin", "Erin", "password123")
	osess := users.Session{Username: "erin", CSRF: "tok"}
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", f.drive.ID)
	req.SetPathValue("folder", f.folder.ID)
	req.SetPathValue("slug", f.album.ID)
	w := httptest.NewRecorder()
	f.h.album(w, req, osess, outsider)
	if w.Code != http.StatusNotFound {
		t.Errorf("outsider album = %d, want 404", w.Code)
	}

	// Membership flips it on instantly.
	if err := f.h.k.Drives.SetMember(t.Context(), f.drive.ID, "erin", drives.RoleViewer); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	f.h.album(w, req, osess, outsider)
	if w.Code != http.StatusOK {
		t.Errorf("member album = %d", w.Code)
	}
}

func TestSearchAndProgress(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest("GET", "/music/search?q=alpha", nil)
	w := httptest.NewRecorder()
	f.h.search(w, req, f.sess, f.user)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Album Alpha") {
		t.Errorf("search = %d", w.Code)
	}

	// A mini-player heartbeat lands in Recently played.
	w = postForm(t, f.h.progressPost, f.sess, f.user, nil, url.Values{
		"drive": {f.drive.ID}, "node": {f.tracks[0].NodeID},
		"pos": {"31"}, "dur": {"180"}, "title": {"Song A"},
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("heartbeat = %d", w.Code)
	}
	hw := httptest.NewRecorder()
	f.h.home(hw, httptest.NewRequest("GET", "/music", nil), f.sess, f.user)
	if !strings.Contains(hw.Body.String(), "Recently played") {
		t.Errorf("home missing Recently played after a heartbeat")
	}
}
