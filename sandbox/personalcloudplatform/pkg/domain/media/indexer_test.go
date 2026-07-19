package media

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// fixture builds the full store stack over kvxtest with ada + a shared
// drive.
type fixture struct {
	media  *Store
	users  *users.Store
	drives *drives.Store
	nodes  *nodes.Store
	drive  drives.Drive
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := kvxtest.New(t)
	us := &users.Store{DB: db}
	ds := &drives.Store{DB: db, Users: us}
	ns := &nodes.Store{DB: db, Users: us}
	ms := &Store{DB: db, Nodes: ns, Drives: ds}
	ctx := t.Context()
	if _, err := us.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatalf("user: %v", err)
	}
	d, err := ds.CreateShared(ctx, "ada", "Media")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	return &fixture{media: ms, users: us, drives: ds, nodes: ns, drive: d}
}

// putFile stores content as a file at path (folders auto-created),
// returning the node.
func (f *fixture) putFile(t *testing.T, ctx context.Context, path, contentType string, content []byte) nodes.Node {
	t.Helper()
	parent := nodes.RootID
	segs := strings.Split(path, "/")
	for _, dir := range segs[:len(segs)-1] {
		child, found, err := f.nodes.GetChild(ctx, f.drive.ID, parent, dir)
		if err != nil {
			t.Fatalf("get %s: %v", dir, err)
		}
		if found {
			parent = child.ID
			continue
		}
		n, err := f.nodes.CreateFolder(ctx, f.drive.ID, parent, dir, "ada")
		if err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		parent = n.ID
	}
	blobID := kvx.NewID()
	if err := f.media.DB.PutBlob(ctx, nodes.BlobKey(f.drive.ID, blobID), bytes.NewReader(content), contentType); err != nil {
		t.Fatalf("blob: %v", err)
	}
	n, err := f.nodes.CommitFile(ctx, f.drive.ID, parent, segs[len(segs)-1], blobID, contentType, int64(len(content)), "ada", false)
	if err != nil {
		t.Fatalf("commit %s: %v", path, err)
	}
	return n
}

// mkFolder creates one folder under root and registers it.
func (f *fixture) registerFolder(t *testing.T, ctx context.Context, name, kind string) nodes.Node {
	t.Helper()
	n, err := f.nodes.CreateFolder(ctx, f.drive.ID, nodes.RootID, name, "ada")
	if err != nil {
		t.Fatalf("folder: %v", err)
	}
	if err := f.media.Register(ctx, f.drive.ID, n.ID, kind, "ada"); err != nil {
		t.Fatalf("register: %v", err)
	}
	return n
}

// entryTitles maps a catalog list to its titles.
func entryTitles(entries []CatalogEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Title)
	}
	return out
}

func TestFilenameConventions(t *testing.T) {
	// The episode regex across the naming variants the spec promises.
	for _, tc := range []struct {
		name           string
		show, title    string
		season, ep     int
		matchesEpisode bool
	}{
		{"Show S01E05.mkv", "Show", "Episode 5", 1, 5, true},
		{"Show.Name.S02E10.The.Title.mkv", "Show Name", "The Title", 2, 10, true},
		{"show s1e5.mp4", "show", "Episode 5", 1, 5, true},
		{"Show - S01 E05 - Pilot.mkv", "Show -", "- Pilot", 1, 5, true},
		{"Movie (2023).mkv", "", "", 0, 0, false},
	} {
		base := strings.TrimSuffix(tc.name, ".mkv")
		base = strings.TrimSuffix(base, ".mp4")
		m := videoEpRe.FindStringSubmatch(base)
		if !tc.matchesEpisode {
			if m != nil && cleanTitle(m[1]) != "" {
				t.Errorf("%s: unexpectedly matched as episode: %v", tc.name, m)
			}
			continue
		}
		if m == nil {
			t.Errorf("%s: no episode match", tc.name)
			continue
		}
		if got := cleanTitle(m[1]); !strings.HasPrefix(got, strings.TrimSuffix(tc.show, " -")) {
			t.Errorf("%s: show = %q", tc.name, got)
		}
	}

	// Movie name + year.
	if m := videoMovieRe.FindStringSubmatch("Heat (1995)"); m == nil || cleanTitle(m[1]) != "Heat" || m[2] != "1995" {
		t.Errorf("movie regex = %v", videoMovieRe.FindStringSubmatch("Heat (1995)"))
	}

	// Track-number prefix stripping.
	track := 0
	if got := titleFromFilename("07 Karma Police.mp3", &track); got != "Karma Police" || track != 7 {
		t.Errorf("titleFromFilename = %q track=%d", got, track)
	}
}

func TestVideoCatalogBuild(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	folder := f.registerFolder(t, ctx, "Video", KindVideo)

	f.putFile(t, ctx, "Video/Show S01E01.mp4", "video/mp4", []byte("v1"))
	f.putFile(t, ctx, "Video/Show S01E02 Homecoming.mp4", "video/mp4", []byte("v2"))
	f.putFile(t, ctx, "Video/Movie (2023).mp4", "video/mp4", []byte("v3"))
	f.putFile(t, ctx, "Video/poster.jpg", "image/jpeg", []byte{0xff, 0xd8, 1})

	n, err := f.media.Rescan(ctx, f.drive.ID, folder.ID)
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	// 1 series + 2 episodes + 1 movie.
	if n != 4 {
		t.Fatalf("entries = %d, want 4", n)
	}

	series, err := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatSeries)
	if err != nil || len(series) != 1 || series[0].Title != "Show" {
		t.Fatalf("series = %v (%v)", entryTitles(series), err)
	}
	if series[0].Seasons != 1 || series[0].Items != 2 {
		t.Errorf("series meta = %+v", series[0])
	}
	if series[0].ArtNode == "" {
		t.Errorf("series has no folder poster art")
	}
	eps, err := f.media.ListCatalogUnder(ctx, f.drive.ID, folder.ID, CatEpisode, series[0].ID)
	if err != nil || len(eps) != 2 {
		t.Fatalf("episodes = %v (%v)", eps, err)
	}
	if eps[0].Episode != 1 || eps[1].Episode != 2 || eps[1].Title != "Homecoming" {
		t.Errorf("episode order/titles = %v", entryTitles(eps))
	}
	if eps[0].NodeID == "" {
		t.Errorf("episode carries no playable node")
	}

	movies, err := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatMovie)
	if err != nil || len(movies) != 1 || movies[0].Title != "Movie" || movies[0].Year != 2023 {
		t.Fatalf("movies = %+v (%v)", movies, err)
	}

	// ScanInfo written.
	info, found, err := f.media.GetScanInfo(ctx, f.drive.ID, folder.ID)
	if err != nil || !found || info.Items != 4 || info.ScannedAt.IsZero() {
		t.Fatalf("scan info = %+v found=%v (%v)", info, found, err)
	}
}

func TestMusicCatalogBuildAndRetag(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	folder := f.registerFolder(t, ctx, "Music", KindMusic)

	cover := []byte{0xff, 0xd8, 0xff, 0xe0, 9, 9}
	tagged := mp3Fixture(id3Tag(3,
		id3TextFrame("TIT2", "Song A", false),
		id3TextFrame("TPE1", "Artist One", false),
		id3TextFrame("TALB", "Album Alpha", false),
		id3TextFrame("TRCK", "1", false),
		id3TextFrame("TYER", "2001", false),
		apicFrame(cover),
	))
	f.putFile(t, ctx, "Music/Artist One/Album Alpha/whatever.mp3", "audio/mpeg", tagged)
	// Untagged: filename convention + folder path fill in.
	f.putFile(t, ctx, "Music/Artist One/Album Alpha/02 Song B.mp3", "audio/mpeg", mp3Fixture(nil))

	if _, err := f.media.Rescan(ctx, f.drive.ID, folder.ID); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	albums, err := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatAlbum)
	if err != nil || len(albums) != 1 {
		t.Fatalf("albums = %v (%v)", entryTitles(albums), err)
	}
	al := albums[0]
	if al.Title != "Album Alpha" || al.Artist != "Artist One" || al.Year != 2001 || al.Items != 2 {
		t.Errorf("album = %+v", al)
	}
	if al.ArtBlob == "" {
		t.Errorf("APIC cover not harvested")
	} else {
		var buf bytes.Buffer
		if err := f.media.DB.GetBlob(ctx, ArtKey(f.drive.ID, folder.ID, al.ArtBlob), &buf); err != nil || !bytes.Equal(buf.Bytes(), cover) {
			t.Errorf("art blob mismatch: %v", err)
		}
	}
	tracks, err := f.media.ListCatalogUnder(ctx, f.drive.ID, folder.ID, CatTrack, al.ID)
	if err != nil || len(tracks) != 2 {
		t.Fatalf("tracks = %v (%v)", entryTitles(tracks), err)
	}
	if tracks[0].Title != "Song A" || tracks[1].Title != "Song B" || tracks[1].Track != 2 {
		t.Errorf("track order = %v", entryTitles(tracks))
	}
	artists, err := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatArtist)
	if err != nil || len(artists) != 1 || artists[0].Title != "Artist One" {
		t.Fatalf("artists = %v (%v)", entryTitles(artists), err)
	}
	if len(artists[0].Refs) != 1 || artists[0].Refs[0] != al.ID {
		t.Errorf("artist refs = %v", artists[0].Refs)
	}

	// RETAG: overwrite the tagged file with a new album name; a rescan
	// rebuilds the catalog from the new truth (rebuildable-cache
	// principle), and the orphaned art blob is swept.
	retagged := mp3Fixture(id3Tag(3,
		id3TextFrame("TIT2", "Song A", false),
		id3TextFrame("TPE1", "Artist One", false),
		id3TextFrame("TALB", "Album Beta", false),
	))
	f.putFile(t, ctx, "Music/Artist One/Album Alpha/whatever.mp3", "audio/mpeg", retagged)
	if _, err := f.media.Rescan(ctx, f.drive.ID, folder.ID); err != nil {
		t.Fatalf("rescan 2: %v", err)
	}
	albums, _ = f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatAlbum)
	names := entryTitles(albums)
	if len(albums) != 2 || !strings.Contains(strings.Join(names, ","), "Album Beta") {
		t.Fatalf("retagged albums = %v", names)
	}
	if _, _, found, _ := f.media.DB.StatBlob(ctx, ArtKey(f.drive.ID, folder.ID, al.ArtBlob)); found {
		t.Errorf("orphaned art blob survived the swap")
	}
}

func TestSidecarOverrides(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	folder := f.registerFolder(t, ctx, "Video", KindVideo)
	// A folder sidecar typed "movie" pins an episode-looking file.
	f.putFile(t, ctx, "Video/Oddball/info.pcmeta", "application/json",
		EncodeMediaMeta(MediaMeta{Type: MediaTypeMovie, Title: "Oddball Feature", Year: 1999}))
	f.putFile(t, ctx, "Video/Oddball/Oddball S01E01.mp4", "video/mp4", []byte("x"))
	if _, err := f.media.Rescan(ctx, f.drive.ID, folder.ID); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	movies, _ := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatMovie)
	if len(movies) != 1 || movies[0].Title != "Oddball Feature" || movies[0].Year != 1999 {
		t.Fatalf("sidecar-pinned movie = %+v", movies)
	}
	if series, _ := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatSeries); len(series) != 0 {
		t.Errorf("sidecar-pinned movie also produced a series: %v", entryTitles(series))
	}
}

func TestUnregisterDropsCatalog(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	folder := f.registerFolder(t, ctx, "Video", KindVideo)
	f.putFile(t, ctx, "Video/Movie (2020).mp4", "video/mp4", []byte("x"))
	if _, err := f.media.Rescan(ctx, f.drive.ID, folder.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.media.Unregister(ctx, f.drive.ID, folder.ID, ""); err != nil {
		t.Fatal(err)
	}
	if movies, _ := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatMovie); len(movies) != 0 {
		t.Errorf("catalog survived unregister: %v", entryTitles(movies))
	}
	// The files themselves are untouched.
	if _, found, _ := f.nodes.GetChild(ctx, f.drive.ID, folder.ID, "Movie (2020).mp4"); !found {
		t.Errorf("unregister deleted the file")
	}
}

// TestDualKindFolder: ONE mixed folder registered as video AND music
// builds both catalogs from one rescan, and unregistering one kind
// prunes only that kind's catalog (+ its harvested art), leaving the
// other intact.
func TestDualKindFolder(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	folder := f.registerFolder(t, ctx, "Mixed", KindVideo)
	if err := f.media.Register(ctx, f.drive.ID, folder.ID, KindMusic, "ada"); err != nil {
		t.Fatalf("second kind: %v", err)
	}

	cover := []byte{0xff, 0xd8, 0xff, 0xe0, 7, 7}
	f.putFile(t, ctx, "Mixed/Movie (2021).mp4", "video/mp4", []byte("v"))
	f.putFile(t, ctx, "Mixed/Show S01E01.mp4", "video/mp4", []byte("v"))
	f.putFile(t, ctx, "Mixed/Artist/Album/01 Song.mp3", "audio/mpeg", mp3Fixture(id3Tag(3,
		id3TextFrame("TIT2", "Song", false),
		id3TextFrame("TPE1", "Artist", false),
		id3TextFrame("TALB", "Album", false),
		apicFrame(cover),
	)))

	n, err := f.media.Rescan(ctx, f.drive.ID, folder.ID)
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	// movie + series + episode + album + track + artist.
	if n != 6 {
		t.Fatalf("entries = %d, want 6", n)
	}
	movies, _ := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatMovie)
	albums, _ := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatAlbum)
	if len(movies) != 1 || len(albums) != 1 {
		t.Fatalf("dual catalogs: movies=%v albums=%v", entryTitles(movies), entryTitles(albums))
	}
	artBlob := albums[0].ArtBlob
	if artBlob == "" {
		t.Fatal("music APIC art not harvested from the dual-kind folder")
	}

	// Unregister MUSIC: its catalog + art vanish, video's survives, and
	// the registration keeps the video kind.
	if err := f.media.Unregister(ctx, f.drive.ID, folder.ID, KindMusic); err != nil {
		t.Fatalf("unregister music: %v", err)
	}
	for _, kind := range []string{CatAlbum, CatTrack, CatArtist} {
		if rows, _ := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, kind); len(rows) != 0 {
			t.Errorf("%s catalog survived music unregister: %v", kind, entryTitles(rows))
		}
	}
	if _, _, found, _ := f.media.DB.StatBlob(ctx, ArtKey(f.drive.ID, folder.ID, artBlob)); found {
		t.Error("music art blob survived music unregister")
	}
	if movies, _ := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatMovie); len(movies) != 1 {
		t.Error("video catalog swept by music unregister")
	}
	if series, _ := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatSeries); len(series) != 1 {
		t.Error("series swept by music unregister")
	}
	reg, found, _ := f.media.Get(ctx, f.drive.ID, folder.ID)
	if !found || !reg.Has(KindVideo) || reg.Has(KindMusic) {
		t.Fatalf("registration after music unregister = %+v found=%v", reg, found)
	}

	// A rescan of the surviving registration rebuilds only video.
	if n, err := f.media.Rescan(ctx, f.drive.ID, folder.ID); err != nil || n != 3 {
		t.Fatalf("video-only rescan = %d (%v), want 3", n, err)
	}
	if albums, _ := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatAlbum); len(albums) != 0 {
		t.Errorf("music catalog reappeared without a registration: %v", entryTitles(albums))
	}

	// Unregister VIDEO (the last kind): row + catalog fully gone.
	if err := f.media.Unregister(ctx, f.drive.ID, folder.ID, KindVideo); err != nil {
		t.Fatalf("unregister video: %v", err)
	}
	if _, found, _ := f.media.Get(ctx, f.drive.ID, folder.ID); found {
		t.Error("registration row survived its last kind")
	}
	if movies, _ := f.media.ListCatalog(ctx, f.drive.ID, folder.ID, CatMovie); len(movies) != 0 {
		t.Error("video catalog survived the last unregister")
	}
}

func TestRescanOfDeletedFolderUnregisters(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	folder := f.registerFolder(t, ctx, "Video", KindVideo)
	// Delete the folder node out from under the registration.
	if err := f.drives.DB.Delete(ctx, "/pcp/noderef/"+f.drive.ID+"/"+folder.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.media.Rescan(ctx, f.drive.ID, folder.ID); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if _, found, _ := f.media.Get(ctx, f.drive.ID, folder.ID); found {
		t.Errorf("registration survived its folder's deletion")
	}
}
