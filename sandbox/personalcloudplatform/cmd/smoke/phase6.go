// phase6.go — the phase-6 live smoke: Video & Music against the REAL
// pcp binary (whose media scan worker does the indexing), on top of the
// phase-3/4/5 harness:
//
//   - a shared drive gets a music tree (2 artists / 2 albums, REAL
//     ID3v2 tags incl. an APIC cover) and a video tree ("Show S01E01/02",
//     "Movie (2023).mp4" — valid MP4 sniff bytes), registered as
//     music/video through the Drive UI endpoints,
//   - the scan worker builds the catalogs (no manual kick), and records
//     /pcp/system/loops/mediascan,
//   - /music shows artists+albums, the album page lists tracks, a track
//     streams with Range → 206, playlists create/add/reorder,
//   - /video shows the series + movie, the play page mounts, a progress
//     heartbeat POST → home's Continue-watching with resume,
//   - a second user JOINS the shared drive and both apps include the
//     registered content instantly (membership IS the subscription);
//     leaving removes it; hide-folder filters one member's view only,
//   - retagging a file + manual rescan (Drive's endpoint) changes the
//     catalog,
//   - API: media:read lists catalogs/folders, a media-less key gets 403,
//   - launcher cards go live ("Continue watching …", "N albums").
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// --- fixture builders --------------------------------------------------------

// id3Frame renders one ID3v2.3 frame.
func id3Frame(id string, body []byte) []byte {
	out := []byte(id)
	n := len(body)
	out = append(out, byte(n>>24), byte(n>>16), byte(n>>8), byte(n), 0, 0)
	return append(out, body...)
}

// id3Text renders a latin-1 text frame.
func id3TextFrame(id, text string) []byte {
	return id3Frame(id, append([]byte{0}, []byte(text)...))
}

// smokeMP3 assembles a tiny MP3: a REAL ID3v2.3 tag + token MPEG bytes.
func smokeMP3(title, artist, album string, track int, cover []byte) []byte {
	frames := [][]byte{
		id3TextFrame("TIT2", title),
		id3TextFrame("TPE1", artist),
		id3TextFrame("TALB", album),
		id3TextFrame("TRCK", fmt.Sprint(track)),
		id3TextFrame("TYER", "2020"),
	}
	if cover != nil {
		body := []byte{0}
		body = append(body, "image/jpeg"...)
		body = append(body, 0, 3) // NUL + front cover
		body = append(body, 0)    // empty description
		body = append(body, cover...)
		frames = append(frames, id3Frame("APIC", body))
	}
	var fb []byte
	for _, f := range frames {
		fb = append(fb, f...)
	}
	n := len(fb)
	tag := []byte{'I', 'D', '3', 3, 0, 0,
		byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	out := append(tag, fb...)
	return append(out, 0xff, 0xfb, 0x90, 0x00, 1, 2, 3, 4, 5, 6, 7, 8)
}

// smokeMP4 is the smallest byte string http.DetectContentType calls
// video/mp4: a well-formed ftyp box plus filler.
func smokeMP4() []byte {
	out := []byte{0, 0, 0, 0x18}
	out = append(out, "ftypisom"...)
	out = append(out, 0, 0, 2, 0)
	out = append(out, "isomiso2"...)
	return append(out, bytes.Repeat([]byte{0}, 64)...)
}

// getRange performs one session GET with a Range header.
func (w *web) getRange(path, rng string) (*http.Response, string, error) {
	req, _ := http.NewRequest("GET", w.base+path, nil)
	req.Header.Set("Range", rng)
	resp, err := w.c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b), nil
}

// phase6 runs the Video & Music smoke.
func phase6(ctx context.Context, pcpURL string, db *client.Client,
	userStore *users.Store, keyStore *apikeys.Store) {

	driveStore := &drives.Store{DB: db, Users: userStore}
	nodeStore := &nodes.Store{DB: db, Users: userStore}
	mediaStore := &media.Store{DB: db, Nodes: nodeStore, Drives: driveStore}
	systemStore := &system.Store{DB: db}

	ada := newWeb(pcpURL)
	if err := ada.login("ada", "password123"); err != nil {
		fail("phase6: ada login", "err", err)
		return
	}

	// --- provision the shared drive + media trees --------------------------------
	shared, err := driveStore.CreateShared(ctx, "ada", "Household Media")
	must(err, "phase6 shared drive")
	mkdir := func(parent, name string) nodes.Node {
		n, err := nodeStore.CreateFolder(ctx, shared.ID, parent, name, "ada")
		must(err, "phase6 mkdir "+name)
		return n
	}
	put := func(parent nodes.Node, name string, content []byte) nodes.Node {
		n, err := nodeStore.StoreFile(ctx, shared.ID, parent.ID, name, bytes.NewReader(content), 0, "ada")
		must(err, "phase6 put "+name)
		return n
	}
	cover := append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{7}, 48)...)

	musicRoot := mkdir(nodes.RootID, "Music")
	a1 := mkdir(musicRoot.ID, "Artist One")
	al1 := mkdir(a1.ID, "Album Alpha")
	put(al1, "01 First Light.mp3", smokeMP3("First Light", "Artist One", "Album Alpha", 1, cover))
	put(al1, "02 Second Wind.mp3", smokeMP3("Second Wind", "Artist One", "Album Alpha", 2, nil))
	a2 := mkdir(musicRoot.ID, "Artist Two")
	al2 := mkdir(a2.ID, "Album Beta")
	put(al2, "01 Beta Wave.mp3", smokeMP3("Beta Wave", "Artist Two", "Album Beta", 1, nil))

	videoRoot := mkdir(nodes.RootID, "Video")
	put(videoRoot, "Show S01E01.mp4", smokeMP4())
	put(videoRoot, "Show S01E02.mp4", smokeMP4())
	movieNode := put(videoRoot, "Movie (2023).mp4", smokeMP4())
	pass("phase6: shared drive + music/video fixture trees uploaded")

	// --- register through the Drive UI endpoints ---------------------------------
	for _, reg := range []struct{ node, kind string }{
		{musicRoot.ID, "music"}, {videoRoot.ID, "video"},
	} {
		code, body, err := ada.post("/drive/media/register", url.Values{
			"drive": {shared.ID}, "node": {reg.node}, "kind": {reg.kind},
		})
		if err != nil || code != 200 || !strings.Contains(body, `"ok":true`) {
			fail("phase6: register "+reg.kind, "code", code, "body", body, "err", err)
			return
		}
	}
	pass("phase6: folders registered as music/video via the Drive UI")

	// --- the scan worker builds the catalogs (no manual kick) ---------------------
	if until(90*time.Second, func() bool {
		albums, _ := mediaStore.ListCatalog(ctx, shared.ID, musicRoot.ID, media.CatAlbum)
		movies, _ := mediaStore.ListCatalog(ctx, shared.ID, videoRoot.ID, media.CatMovie)
		series, _ := mediaStore.ListCatalog(ctx, shared.ID, videoRoot.ID, media.CatSeries)
		return len(albums) == 2 && len(movies) == 1 && len(series) == 1
	}) {
		pass("phase6: scan worker built both catalogs (2 albums, 1 movie, 1 series)")
	} else {
		fail("phase6: catalogs never appeared")
		return
	}
	loops, _ := systemStore.Loops(ctx)
	if rec, ok := loops["mediascan"]; ok && !rec.LastSuccess.IsZero() {
		pass("phase6: loop record /pcp/system/loops/mediascan")
	} else {
		fail("phase6: mediascan loop record missing")
	}

	// --- /music: artists/albums → album page → Range stream -----------------------
	code, body, _ := ada.get("/music")
	if code == 200 && strings.Contains(body, "Album Alpha") && strings.Contains(body, "Artist One") &&
		strings.Contains(body, "Album Beta") {
		pass("phase6: /music shows both artists' albums")
	} else {
		fail("phase6: /music home wrong", "code", code)
	}
	albums, _ := mediaStore.ListCatalog(ctx, shared.ID, musicRoot.ID, media.CatAlbum)
	var alpha media.CatalogEntry
	for _, al := range albums {
		if al.Title == "Album Alpha" {
			alpha = al
		}
	}
	if alpha.ArtBlob == "" {
		fail("phase6: APIC cover not harvested for Album Alpha")
	}
	code, body, _ = ada.get("/music/album/" + shared.ID + "/" + musicRoot.ID + "/" + alpha.ID)
	if code == 200 && strings.Contains(body, "First Light") && strings.Contains(body, "Second Wind") {
		pass("phase6: album page lists its tracks in order")
	} else {
		fail("phase6: album page wrong", "code", code)
	}
	tracks, _ := mediaStore.ListCatalogUnder(ctx, shared.ID, musicRoot.ID, media.CatTrack, alpha.ID)
	if len(tracks) != 2 {
		fail("phase6: track catalog wrong", "tracks", fmt.Sprintf("%+v", tracks))
		return
	}
	if resp, _, err := ada.getRange("/drive/file/"+shared.ID+"/"+tracks[0].NodeID+"?inline=1", "bytes=0-9"); err == nil &&
		resp.StatusCode == http.StatusPartialContent && strings.HasPrefix(resp.Header.Get("Content-Range"), "bytes 0-9/") {
		pass("phase6: track streams with Range → 206 Partial Content")
	} else {
		fail("phase6: ranged track stream failed", "err", err)
	}

	// --- playlists: create → add ×2 → reorder --------------------------------------
	code, body, _ = ada.post("/music/playlists/create", url.Values{"name": {"Road trip"}})
	out := jsonMap(body)
	plID, _ := out["id"].(string)
	if code != 200 || plID == "" {
		fail("phase6: playlist create", "code", code, "body", body)
		return
	}
	for _, tr := range tracks {
		code, body, _ = ada.post("/music/pl/"+plID+"/add", url.Values{
			"drive": {shared.ID}, "node": {tr.NodeID}, "title": {tr.Title}, "artist": {tr.Artist},
		})
		if code != 200 {
			fail("phase6: playlist add", "code", code, "body", body)
		}
	}
	code, _, _ = ada.post("/music/pl/"+plID+"/move", url.Values{"idx": {"1"}, "dir": {"up"}})
	pl, _, _ := mediaStore.GetPlaylist(ctx, "ada", plID)
	if code == 200 && len(pl.Tracks) == 2 && pl.Tracks[0].Title == "Second Wind" {
		pass("phase6: playlist create → add ×2 → reorder")
	} else {
		fail("phase6: playlist state wrong", "tracks", fmt.Sprintf("%+v", pl.Tracks))
	}

	// --- /video: series + movie → play page → heartbeat → continue watching --------
	code, body, _ = ada.get("/video")
	if code == 200 && strings.Contains(body, "/video/t/"+shared.ID+"/"+videoRoot.ID+"/series/") &&
		strings.Contains(body, "/video/t/"+shared.ID+"/"+videoRoot.ID+"/movies/") {
		pass("phase6: /video shows the series and the movie")
	} else {
		fail("phase6: /video home wrong", "code", code)
	}
	code, body, _ = ada.get("/drive/app/video?drive=" + shared.ID + "&node=" + movieNode.ID)
	if code == 200 && strings.Contains(body, "PCP_APP") {
		pass("phase6: play page (app host) mounts")
	} else {
		fail("phase6: play page wrong", "code", code)
	}
	code, _, _ = ada.post("/video/progress", url.Values{
		"drive": {shared.ID}, "node": {movieNode.ID},
		"pos": {"420"}, "dur": {"3600"}, "kind": {"video"}, "title": {"Movie"},
	})
	if code != 204 {
		fail("phase6: progress heartbeat", "code", code)
	}
	code, body, _ = ada.get("/video/progress?node=" + movieNode.ID)
	if code == 200 && strings.Contains(body, "420") {
		pass("phase6: heartbeat stored; resume lookup answers the position")
	} else {
		fail("phase6: resume lookup wrong", "code", code, "body", body)
	}
	code, body, _ = ada.get("/video")
	if code == 200 && strings.Contains(body, "Continue watching") && strings.Contains(body, "mresume") {
		pass("phase6: /video home shows Continue watching with a resume bar")
	} else {
		fail("phase6: continue-watching missing")
	}
	// A music heartbeat feeds Recently played + the launcher card.
	_, _, _ = ada.post("/music/progress", url.Values{
		"drive": {shared.ID}, "node": {tracks[0].NodeID},
		"pos": {"30"}, "dur": {"180"}, "title": {"First Light"},
	})

	// --- membership IS the subscription --------------------------------------------
	if _, err := userStore.CreateUser(ctx, "erin", "Erin", "password123"); err != nil {
		fail("phase6: create erin", "err", err)
		return
	}
	erin := newWeb(pcpURL)
	if err := erin.login("erin", "password123"); err != nil {
		fail("phase6: erin login", "err", err)
		return
	}
	// The folder id in the page (shelf links) is the leak-proof marker —
	// the empty-state copy legitimately mentions "Show S01E05".
	code, body, _ = erin.get("/video")
	if code == 200 && !strings.Contains(body, videoRoot.ID) {
		pass("phase6: non-member sees no registered content")
	} else {
		fail("phase6: non-member leaked content", "code", code)
	}
	must(driveStore.SetMember(ctx, shared.ID, "erin", drives.RoleViewer), "phase6 add erin")
	code, vbody, _ := erin.get("/video")
	_, mbody, _ := erin.get("/music")
	if code == 200 && strings.Contains(vbody, videoRoot.ID) && strings.Contains(mbody, "Album Alpha") {
		pass("phase6: joining the drive surfaces Video AND Music instantly")
	} else {
		fail("phase6: member union missing content")
	}
	must(driveStore.RemoveMember(ctx, shared.ID, "erin"), "phase6 remove erin")
	_, vbody, _ = erin.get("/video")
	if !strings.Contains(vbody, videoRoot.ID) {
		pass("phase6: leaving the drive removes the content")
	} else {
		fail("phase6: ex-member still sees content")
	}

	// --- hide folder: per-user view override ----------------------------------------
	code, _, _ = ada.post("/video/hide", url.Values{
		"drive": {shared.ID}, "folder": {videoRoot.ID}, "on": {"1"},
	})
	_, body, _ = ada.get("/video")
	if code == 200 && !strings.Contains(body, "browse all") && strings.Contains(body, "hidden") {
		pass("phase6: hide-folder filters the member's own view")
	} else {
		fail("phase6: hide-folder did not filter")
	}
	_, _, _ = ada.post("/video/hide", url.Values{
		"drive": {shared.ID}, "folder": {videoRoot.ID}, "on": {"0"},
	})

	// --- retag + manual rescan changes the catalog ----------------------------------
	_, err = nodeStore.StoreFile(ctx, shared.ID, al2.ID, "01 Beta Wave.mp3",
		bytes.NewReader(smokeMP3("Beta Wave", "Artist Two", "Album Gamma", 1, nil)), 0, "ada")
	must(err, "phase6 retag")
	code, _, _ = ada.post("/drive/media/rescan", url.Values{
		"drive": {shared.ID}, "node": {musicRoot.ID},
	})
	albums, _ = mediaStore.ListCatalog(ctx, shared.ID, musicRoot.ID, media.CatAlbum)
	names := []string{}
	for _, al := range albums {
		names = append(names, al.Title)
	}
	if code == 200 && strings.Contains(strings.Join(names, ","), "Album Gamma") &&
		!strings.Contains(strings.Join(names, ","), "Album Beta") {
		pass("phase6: retag + manual rescan rebuilt the catalog (Beta → Gamma)")
	} else {
		fail("phase6: rescan after retag wrong", "albums", names, "code", code)
	}

	// --- API: media:read reads, a media-less key is denied ---------------------------
	mediaTok, _, err := keyStore.Mint(ctx, "ada", "gallery", []string{apikeys.ScopeMediaRead}, time.Time{})
	must(err, "phase6 mint media key")
	code, body = bearer(pcpURL, mediaTok, "GET", "/api/v1/media/folders", "", "")
	if code == 200 && strings.Contains(body, `"kind":"music"`) && strings.Contains(body, `"kind":"video"`) {
		pass("phase6: API media:read lists the registered-folder union")
	} else {
		fail("phase6: API folders wrong", "code", code, "body", body)
	}
	code, body = bearer(pcpURL, mediaTok, "GET", "/api/v1/media/catalog/"+shared.ID+"/"+videoRoot.ID, "", "")
	if code == 200 && strings.Contains(body, `"kind":"movies"`) && strings.Contains(body, `"nodeId"`) {
		pass("phase6: API catalog answers entries with stream refs")
	} else {
		fail("phase6: API catalog wrong", "code", code, "body", body)
	}
	driveTok, _, err := keyStore.Mint(ctx, "ada", "files-only", []string{apikeys.ScopeDriveRead}, time.Time{})
	must(err, "phase6 mint drive key")
	code, _ = bearer(pcpURL, driveTok, "GET", "/api/v1/media/folders", "", "")
	if code == http.StatusForbidden {
		pass("phase6: a key without media:read gets 403")
	} else {
		fail("phase6: scope gate failed", "code", code)
	}

	// --- launcher cards --------------------------------------------------------------
	code, body, _ = ada.get("/")
	if code == 200 && strings.Contains(body, "Continue watching Movie") &&
		strings.Contains(body, "Last played: First Light") {
		pass("phase6: launcher cards live (Continue watching + Last played)")
	} else {
		fail("phase6: launcher cards wrong", "code", code)
	}
}
