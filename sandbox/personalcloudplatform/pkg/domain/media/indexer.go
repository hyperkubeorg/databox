// indexer.go — the scanner: walk one registered folder and derive its
// catalog FROM the files (spec §9). Nothing is stored twice — the
// catalog is a cache, rebuilt wholesale on every rescan, so retagging,
// renaming, or editing a sidecar and rescanning is the whole
// maintenance story. Ported from PCD onto the drive-level registry.
//
// Sources, in priority order:
//
//	sidecars — pcd-meta/1 JSON (info.pcmeta per folder, <file>.pcmeta per
//	           file): TYPE overrides (series/parts/movie), titles, years,
//	           descriptions, genres, album/artist pins. Hand-editable; no
//	           third-party service is ever consulted.
//	music    — embedded ID3v2 tags (ranged reads), then the
//	           Artist/Album/NN Title path convention.
//	video    — filename conventions: "Show S01E05 Title.mkv" → series
//	           with seasons; "Something Part 2.mkv" (or Ep/E numbering
//	           without a season) → a PARTS show (one flat run of
//	           episodes); "Movie (2023).mkv" → movie. A folder sidecar
//	           typed series/parts turns its videos into episodes in name
//	           order even without helpful filenames.
//	art      — ONLY designated names, DOMAIN-SPLIT (video: poster.* /
//	           fanart.*; music: cover.* / folder.*), embedded APIC tags,
//	           or a video's own "<filename>.jpg" frame capture — never
//	           arbitrary images, never across domains.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
)

// videoEpRe matches "Show Name S01E05 Optional Title.ext".
var videoEpRe = regexp.MustCompile(`(?i)^(.*?)[. _-]*S(\d{1,2})[. _-]*E(\d{1,3})[. _-]*(.*?)$`)

// videoPartRe matches season-less episode numbering: "Part 2", "Pt.3",
// "Episode 4", "Ep 1", "E01" — a PARTS show.
var videoPartRe = regexp.MustCompile(`(?i)^(.*?)[. _-]*(?:part|pt|episode|ep|e)[. _-]*(\d{1,3})[. _-]*(.*?)$`)

// videoMovieRe matches "Movie Name (2023).ext".
var videoMovieRe = regexp.MustCompile(`^(.*?)[. _]*\((\d{4})\)`)

// trackNoRe strips a leading "NN " track number off a filename.
var trackNoRe = regexp.MustCompile(`^(\d{1,3})[. _-]+(.+)$`)

// cleanTitle turns dot/underscore filename debris into a display title.
func cleanTitle(s string) string {
	s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// Designated art names are DOMAIN-SPLIT so one folder can feed both a
// video and a music registration without cross-contamination (a
// captured show poster must never become an album's cover). Best first.
var videoArtNames = []string{"poster.jpg", "poster.png", "fanart.jpg", "fanart.png"}
var musicArtNames = []string{"cover.jpg", "cover.png", "folder.jpg", "folder.png"}

// mediaFile is one walked file plus its location.
type mediaFile struct {
	node   nodes.Node
	rel    string // full relative path from the registered folder
	relDir string // its folder ("" = the registered folder itself)
}

// showBuild accumulates one show during the scan.
type showBuild struct {
	title  string
	meta   MediaMeta
	eps    []CatalogEntry
	art    string // folder-art node id
	newest time.Time
}

// Rescan rebuilds one registered folder's catalog from its files.
// Returns the entry count. A databox lock makes each (drive, folder)
// scan a cluster-wide singleton; losing the lock means someone else is
// already doing the work (not an error). The old catalog is dropped
// only after the walk succeeds, so a failed scan keeps the previous
// shelves.
func (s *Store) Rescan(ctx context.Context, driveID, folderID string) (int, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(folderID) {
		return 0, nil
	}
	resource := "pcp/mediascan/" + driveID + "/" + folderID
	if _, err := s.DB.LockAcquire(ctx, resource, "exclusive", 5*time.Minute); err != nil {
		info, _, _ := s.GetScanInfo(ctx, driveID, folderID)
		return info.Items, nil
	}
	defer func() { _ = s.DB.LockRelease(context.Background(), resource) }()

	reg, found, err := s.Get(ctx, driveID, folderID)
	if err != nil {
		return 0, err
	}
	if !found {
		// The registration is gone — a surviving catalog is a ghost.
		return 0, s.dropCatalog(ctx, driveID, folderID)
	}
	// Files are the truth: a registered folder that was DELETED loses
	// its registration (and catalog) instead of serving ghosts.
	if _, found, err := s.Nodes.GetByID(ctx, driveID, folderID); err != nil {
		return 0, err
	} else if !found {
		return 0, s.Unregister(ctx, driveID, folderID, "")
	}

	var audio, video []mediaFile
	sidecars := []mediaFile{}
	videoArt := map[string]string{} // folder relDir → designated poster nodeID
	musicArt := map[string]string{} // folder relDir → designated cover nodeID
	images := map[string]string{}   // every other image, lowercased rel → nodeID
	err = s.Nodes.WalkSubtree(ctx, driveID, folderID, func(rel string, n nodes.Node) error {
		if n.IsDir {
			return nil
		}
		dir := path.Dir(rel)
		if dir == "." {
			dir = ""
		}
		f := mediaFile{node: n, rel: rel, relDir: dir}
		name := strings.ToLower(n.Name)
		if IsMediaMetaFile(name) {
			sidecars = append(sidecars, f)
			return nil
		}
		for _, p := range videoArtNames {
			if name == p {
				if _, ok := videoArt[dir]; !ok {
					videoArt[dir] = n.ID
				}
				return nil
			}
		}
		for _, p := range musicArtNames {
			if name == p {
				if _, ok := musicArt[dir]; !ok {
					musicArt[dir] = n.ID
				}
				return nil
			}
		}
		switch kindOf(n) {
		case "aud":
			audio = append(audio, f)
		case "vid":
			video = append(video, f)
		case "img":
			// An arbitrary image is NEVER art by itself (a random photo
			// must not become a show's poster) — it only counts when a
			// video claims it by name ("<video filename>.jpg", the
			// frame-capture convention). Folder art comes exclusively
			// from the designated names above.
			images[strings.ToLower(rel)] = n.ID
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	// Read the sidecars (small, capped) into folder- and file-level maps.
	folderMeta := map[string]MediaMeta{}
	fileMeta := map[string]MediaMeta{}
	for _, f := range sidecars {
		if f.node.Size <= 0 || f.node.Size > mediaMetaMax {
			continue
		}
		var buf bytes.Buffer
		if err := s.DB.GetBlob(ctx, nodes.BlobKey(driveID, f.node.BlobID), &buf); err != nil {
			continue
		}
		meta, ok := ParseMediaMeta(buf.Bytes())
		if !ok {
			continue
		}
		if strings.EqualFold(f.node.Name, MediaMetaFolderName) {
			folderMeta[f.relDir] = meta
		} else {
			fileMeta[strings.TrimSuffix(strings.ToLower(f.rel), MediaMetaExt)] = meta
		}
	}
	// metaForDir resolves the nearest folder sidecar, walking UP so a
	// show-root info.pcmeta covers its season subfolders.
	metaForDir := func(dir string) (MediaMeta, string, bool) {
		for d := dir; ; d = path.Dir(d) {
			if d == "." {
				d = ""
			}
			if m, ok := folderMeta[d]; ok {
				return m, d, true
			}
			if d == "" {
				return MediaMeta{}, "", false
			}
		}
	}
	artForDir := func(dir string) string {
		for d := dir; ; d = path.Dir(d) {
			if d == "." {
				d = ""
			}
			if id, ok := videoArt[d]; ok {
				return id
			}
			if d == "" {
				return ""
			}
		}
	}

	// Per-file art: an image named "<video filename>.jpg" supplies that
	// episode's/movie's own preview.
	fileArt := map[string]string{}
	for _, f := range video {
		low := strings.ToLower(f.rel)
		for _, ext := range []string{".jpg", ".jpeg", ".png"} {
			if id, ok := images[low+ext]; ok {
				fileArt[low] = id
				break
			}
		}
	}

	entries := map[string]CatalogEntry{}
	putEntry := func(kind, id string, e CatalogEntry) {
		e.Kind, e.ID = kind, id
		entries[kind+"/"+id] = e
	}

	// EVERY kind in the registration set builds its catalog from the one
	// walk — a mixed folder registered for both feeds both apps. The
	// whole-prefix swap below also prunes a kind that was unregistered
	// between scans.
	if reg.Has(KindMusic) {
		s.indexMusic(ctx, driveID, folderID, audio, fileMeta, folderMeta, musicArt, putEntry)
	}
	if reg.Has(KindVideo) {
		indexVideo(driveID, video, fileMeta, fileArt, metaForDir, artForDir, putEntry)
	}

	// Swap the catalog: remember the old art set, drop the old prefix,
	// write the new set, then delete art blobs nothing references.
	oldArt := map[string]bool{}
	_ = kvx.ScanPrefix(ctx, s.DB, catalogPrefix+driveID+"/"+folderID+"/", func(_ string, value []byte) error {
		var e CatalogEntry
		if json.Unmarshal(value, &e) == nil && e.ArtBlob != "" {
			oldArt[e.ArtBlob] = true
		}
		return nil
	})
	if err := kvx.DeletePrefix(ctx, s.DB, catalogPrefix+driveID+"/"+folderID+"/"); err != nil {
		return 0, err
	}
	for key, e := range entries {
		kind, id, _ := strings.Cut(key, "/")
		if err := kvx.SetJSON(ctx, s.DB, catalogKey(driveID, folderID, kind, id), e); err != nil {
			return 0, err
		}
		delete(oldArt, e.ArtBlob)
	}
	for slug := range oldArt {
		_ = s.DB.DeleteBlob(ctx, artKey(driveID, folderID, slug))
	}
	if err := kvx.SetJSON(ctx, s.DB, catalogMetaKey(driveID, folderID),
		ScanInfo{ScannedAt: time.Now().UTC(), Items: len(entries)}); err != nil {
		return len(entries), err
	}
	return len(entries), nil
}

// indexVideo classifies video files: sidecar type wins, then SxxEyy →
// series, season-less part numbering → parts show, folder typed
// series/parts → name-ordered episodes, else movie.
func indexVideo(driveID string, video []mediaFile, fileMeta map[string]MediaMeta,
	fileArt map[string]string,
	metaForDir func(string) (MediaMeta, string, bool), artForDir func(string) string,
	putEntry func(kind, id string, e CatalogEntry)) {

	// A movie's poster: the folder's designated art, else its own
	// captured frame. Episodes use their captured frame directly.
	movieArt := func(f mediaFile) string {
		if a := artForDir(f.relDir); a != "" {
			return a
		}
		return fileArt[strings.ToLower(f.rel)]
	}

	shows := map[string]*showBuild{}
	getShow := func(title string, meta MediaMeta, artDir string) *showBuild {
		if meta.Title != "" {
			title = meta.Title
		}
		key := strings.ToLower(title)
		sh, ok := shows[key]
		if !ok {
			sh = &showBuild{title: title, meta: meta}
			shows[key] = sh
		}
		if sh.meta.Description == "" && meta.Description != "" {
			sh.meta = meta
		}
		if sh.art == "" {
			sh.art = artForDir(artDir)
		}
		return sh
	}
	folderName := func(dir string) string {
		if dir == "" {
			return ""
		}
		return path.Base(dir)
	}

	for _, f := range video {
		base := strings.TrimSuffix(f.node.Name, path.Ext(f.node.Name))
		fm := fileMeta[strings.ToLower(f.rel)]
		dm, metaDir, hasDM := metaForDir(f.relDir)

		// A movie by declaration (file sidecar, or a movie-typed folder).
		if fm.Type == MediaTypeMovie || (hasDM && dm.Type == MediaTypeMovie) {
			emitMovie(driveID, f, base, mergeMeta(fm, dm), movieArt(f), putEntry)
			continue
		}
		// Series with seasons: SxxEyy.
		if m := videoEpRe.FindStringSubmatch(base); m != nil && (m[1] != "" || folderName(f.relDir) != "") {
			showTitle := cleanTitle(m[1])
			if showTitle == "" {
				// "S01E05.mkv" inside "Show/Season 1/": the folder names it.
				showTitle = cleanTitle(strings.TrimSuffix(folderName(f.relDir), path.Ext(folderName(f.relDir))))
			}
			season, _ := strconv.Atoi(m[2])
			ep, _ := strconv.Atoi(m[3])
			title := cleanTitle(m[4])
			if title == "" {
				title = fmt.Sprintf("Episode %d", ep)
			}
			sh := getShow(showTitle, dmIfCovers(dm, hasDM), f.relDir)
			sh.addEp(CatalogEntry{Title: title, Season: season, Episode: ep,
				DriveID: driveID, NodeID: f.node.ID, AddedAt: f.node.ModifiedAt,
				ArtNode: fileArt[strings.ToLower(f.rel)]})
			continue
		}
		// Parts show: season-less numbering ("Part 2", "Ep 4", "E01").
		if m := videoPartRe.FindStringSubmatch(base); m != nil && m[2] != "" && (hasDM && dm.Type != MediaTypeMovie || cleanTitle(m[1]) != "" || f.relDir != "") {
			showTitle := cleanTitle(m[1])
			if showTitle == "" {
				showTitle = cleanTitle(folderName(f.relDir))
			}
			if showTitle == "" {
				emitMovie(driveID, f, base, mergeMeta(fm, dm), movieArt(f), putEntry)
				continue
			}
			ep, _ := strconv.Atoi(m[2])
			title := cleanTitle(m[3])
			if title == "" {
				title = fmt.Sprintf("Part %d", ep)
			}
			sh := getShow(showTitle, dmIfCovers(dm, hasDM), f.relDir)
			sh.addEp(CatalogEntry{Title: title, Season: 0, Episode: ep,
				DriveID: driveID, NodeID: f.node.ID, AddedAt: f.node.ModifiedAt,
				ArtNode: fileArt[strings.ToLower(f.rel)]})
			continue
		}
		// Folder declared a show: its videos are episodes in name order.
		if hasDM && (dm.Type == MediaTypeSeries || dm.Type == MediaTypeParts) {
			showTitle := dm.Title
			if showTitle == "" {
				showTitle = cleanTitle(folderName(metaDir))
			}
			if showTitle == "" {
				showTitle = "Untitled show"
			}
			sh := getShow(showTitle, dm, f.relDir)
			sh.addEp(CatalogEntry{Title: cleanTitle(base), Season: 0, Episode: 0, // numbered after sorting
				DriveID: driveID, NodeID: f.node.ID, AddedAt: f.node.ModifiedAt,
				ArtNode: fileArt[strings.ToLower(f.rel)]})
			continue
		}
		emitMovie(driveID, f, base, mergeMeta(fm, dm), movieArt(f), putEntry)
	}

	for _, sh := range shows {
		sh.finish()
		slug := slugify(sh.title)
		maxSeason := 0
		for i, e := range sh.eps {
			if e.Season > maxSeason {
				maxSeason = e.Season
			}
			e.Artist = sh.title
			putEntry(CatEpisode, fmt.Sprintf("%s/S%02dE%03d", slug, e.Season, e.Episode), e)
			sh.eps[i] = e
		}
		putEntry(CatSeries, slug, CatalogEntry{
			Title: sh.title, Year: sh.meta.Year,
			Description: sh.meta.Description, Genres: sh.meta.Genres,
			Seasons: maxSeason, Parts: maxSeason == 0,
			Items: len(sh.eps), AddedAt: sh.newest,
			DriveID: driveID, ArtNode: sh.art,
		})
	}
}

// addEp collects one episode.
func (sh *showBuild) addEp(e CatalogEntry) {
	sh.eps = append(sh.eps, e)
	if e.AddedAt.After(sh.newest) {
		sh.newest = e.AddedAt
	}
}

// finish orders episodes and numbers the unnumbered (folder-declared
// shows: name order becomes part order).
func (sh *showBuild) finish() {
	sort.Slice(sh.eps, func(i, j int) bool {
		a, b := sh.eps[i], sh.eps[j]
		if a.Season != b.Season {
			return a.Season < b.Season
		}
		if a.Episode != b.Episode {
			return a.Episode < b.Episode
		}
		return strings.ToLower(a.Title) < strings.ToLower(b.Title)
	})
	next := 1
	for i := range sh.eps {
		if sh.eps[i].Episode == 0 {
			sh.eps[i].Episode = next
		}
		next = sh.eps[i].Episode + 1
	}
}

// dmIfCovers passes the folder meta through only when it exists —
// keeping zero-value semantics honest at call sites.
func dmIfCovers(dm MediaMeta, has bool) MediaMeta {
	if !has {
		return MediaMeta{}
	}
	return dm
}

// mergeMeta overlays file-level metadata over folder-level.
func mergeMeta(fm, dm MediaMeta) MediaMeta {
	out := dm
	if fm.Title != "" {
		out.Title = fm.Title
	}
	if fm.Year != 0 {
		out.Year = fm.Year
	}
	if fm.Description != "" {
		out.Description = fm.Description
	}
	if len(fm.Genres) > 0 {
		out.Genres = fm.Genres
	}
	if fm.Type != "" {
		out.Type = fm.Type
	}
	return out
}

// emitMovie writes one movie entry, meta beating filename parsing.
func emitMovie(driveID string, f mediaFile, base string, meta MediaMeta, art string,
	putEntry func(kind, id string, e CatalogEntry)) {
	title, year := cleanTitle(base), 0
	if m := videoMovieRe.FindStringSubmatch(base); m != nil {
		title = cleanTitle(m[1])
		year, _ = strconv.Atoi(m[2])
	}
	if meta.Title != "" {
		title = meta.Title
	}
	if meta.Year != 0 {
		year = meta.Year
	}
	putEntry(CatMovie, slugify(title+strconv.Itoa(year)), CatalogEntry{
		Title: title, Year: year,
		Description: meta.Description, Genres: meta.Genres,
		DriveID: driveID, NodeID: f.node.ID, AddedAt: f.node.ModifiedAt,
		ArtNode: art,
	})
}

// indexMusic is the audio side: ID3 tags via ranged reads, path
// conventions as fallback, and sidecars on top — a track's .pcmeta
// relabels the file, a folder's info.pcmeta pins the album's
// title/artist/year. Art: an explicit folder image (cover.jpg …) beats
// embedded APIC art, so uploading a cover fixes bad embedded artwork.
func (s *Store) indexMusic(ctx context.Context, driveID, folderID string, audio []mediaFile,
	fileMeta map[string]MediaMeta, folderMeta map[string]MediaMeta,
	folderArt map[string]string, putEntry func(kind, id string, e CatalogEntry)) {
	// Albums group by (folder, album name) — NOT by track artist, or a
	// single "feat." credit would split an album in two. The album's
	// display artist is the credit every track shares, the shared PRIMARY
	// artist, or "Various Artists".
	type albumKey struct{ dir, album string }
	albums := map[albumKey][]CatalogEntry{}
	albumArt := map[albumKey]CatalogEntry{}
	for _, f := range audio {
		info, tagged := ID3Info{}, false
		if strings.HasSuffix(strings.ToLower(f.node.Name), ".mp3") {
			info, tagged = s.ReadID3(ctx, nodes.BlobKey(driveID, f.node.BlobID))
		}
		if !tagged || info.Title == "" {
			info.Title = titleFromFilename(f.node.Name, &info.Track)
		}
		if info.Artist == "" || info.Album == "" {
			segs := strings.Split(f.relDir, "/")
			if info.Album == "" && len(segs) >= 1 && segs[len(segs)-1] != "" {
				info.Album = segs[len(segs)-1]
			}
			if info.Artist == "" && len(segs) >= 2 {
				info.Artist = segs[len(segs)-2]
			}
		}
		// Sidecar overrides: folder-level album details first, then the
		// track's own sidecar (more specific wins).
		if dm, ok := folderMeta[f.relDir]; ok {
			if dm.Album != "" {
				info.Album = dm.Album
			}
			if dm.Artist != "" && info.Artist == "" {
				info.Artist = dm.Artist // uncredited tracks inherit the album artist
			}
			if dm.Year != 0 {
				info.Year = dm.Year
			}
		}
		if fm, ok := fileMeta[strings.ToLower(f.rel)]; ok {
			if fm.Title != "" {
				info.Title = fm.Title
			}
			if fm.Artist != "" {
				info.Artist = fm.Artist
			}
			if fm.Album != "" {
				info.Album = fm.Album
			}
			if fm.TrackNo > 0 {
				info.Track = fm.TrackNo
			}
		}
		if info.Album == "" {
			info.Album = "Unsorted"
		}
		if info.Artist == "" {
			info.Artist = "Unknown Artist"
		}
		k := albumKey{dir: f.relDir, album: info.Album}
		track := CatalogEntry{
			Title: info.Title, Artist: info.Artist, Year: info.Year, Track: info.Track,
			DriveID: driveID, NodeID: f.node.ID, AddedAt: f.node.ModifiedAt,
		}
		albums[k] = append(albums[k], track)
		// Explicit folder art (cover.jpg — user-managed) wins over
		// embedded APIC art, so uploading a cover FIXES bad artwork.
		if _, have := albumArt[k]; !have {
			if artNode, ok := folderArt[f.relDir]; ok {
				albumArt[k] = CatalogEntry{ArtNode: artNode}
			}
		}
		if _, have := albumArt[k]; !have && info.Cover != nil && len(info.Cover) < 2<<20 {
			slug := slugify(info.Artist + "/" + info.Album)
			if err := s.DB.PutBlob(ctx, artKey(driveID, folderID, slug), bytes.NewReader(info.Cover), info.CoverMIME); err == nil {
				albumArt[k] = CatalogEntry{ArtBlob: slug}
			}
		}
	}
	// Artists: every credited name (multi-artist credits split) maps to
	// the album slugs their songs appear on — the discography pages'
	// data. Display keeps the full credit string on tracks/albums.
	artistAlbums := map[string]map[string]bool{}
	albumArtist := func(tracks []CatalogEntry) string {
		if len(tracks) == 0 {
			return "Unknown Artist"
		}
		same, primary := true, SplitArtists(tracks[0].Artist)
		samePrimary := len(primary) > 0
		for _, t := range tracks[1:] {
			if t.Artist != tracks[0].Artist {
				same = false
			}
			p := SplitArtists(t.Artist)
			if len(p) == 0 || len(primary) == 0 || !strings.EqualFold(p[0], primary[0]) {
				samePrimary = false
			}
		}
		switch {
		case same:
			return tracks[0].Artist
		case samePrimary:
			return primary[0]
		}
		return "Various Artists"
	}
	for k, tracks := range albums {
		artist := albumArtist(tracks)
		if dm, ok := folderMeta[k.dir]; ok && dm.Artist != "" {
			artist = dm.Artist // the folder sidecar pins the album artist
		}
		slug := slugify(artist + "/" + k.album)
		sort.Slice(tracks, func(i, j int) bool {
			if tracks[i].Track != tracks[j].Track {
				return tracks[i].Track < tracks[j].Track
			}
			return tracks[i].Title < tracks[j].Title
		})
		newest := time.Time{}
		year := 0
		for i, t := range tracks {
			putEntry(CatTrack, slug+"/"+fmt.Sprintf("%03d-%s", i+1, slugify(t.Title)), t)
			if t.AddedAt.After(newest) {
				newest = t.AddedAt
			}
			if t.Year > 0 {
				year = t.Year
			}
		}
		album := CatalogEntry{
			Title: k.album, Artist: artist, Year: year, Items: len(tracks), AddedAt: newest,
			DriveID: driveID,
		}
		album.ArtBlob, album.ArtNode = albumArt[k].ArtBlob, albumArt[k].ArtNode
		putEntry(CatAlbum, slug, album)
		for _, t := range tracks {
			for _, name := range SplitArtists(t.Artist) {
				key := strings.ToLower(name)
				if artistAlbums[key] == nil {
					artistAlbums[key] = map[string]bool{}
				}
				artistAlbums[key][slug] = true
			}
		}
	}
	displayName := map[string]string{}
	for _, tracks := range albums {
		for _, t := range tracks {
			for _, name := range SplitArtists(t.Artist) {
				displayName[strings.ToLower(name)] = name
			}
		}
	}
	for key, albumSet := range artistAlbums {
		refs := make([]string, 0, len(albumSet))
		for slug := range albumSet {
			refs = append(refs, slug)
		}
		sort.Strings(refs)
		if len(refs) > 200 {
			refs = refs[:200]
		}
		putEntry(CatArtist, slugify(displayName[key]), CatalogEntry{
			Title: displayName[key], Items: len(refs), Refs: refs,
			DriveID: driveID,
		})
	}
}

// artistSepRe splits a multi-artist credit on the conventional
// separators — ";", "/", "feat.", "ft." — while leaving names like
// "Simon & Garfunkel" whole ("&" and "," are too ambiguous to split).
var artistSepRe = regexp.MustCompile(`(?i)\s*(?:;|/|\bfeat\.?\s|\bft\.?\s)\s*`)

// SplitArtists explodes one credit string into individual artist names.
func SplitArtists(credit string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range artistSepRe.Split(credit, -1) {
		name := strings.TrimSpace(part)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		out = append(out, name)
	}
	return out
}

// kindOf buckets a node by media family without importing the drive
// app's FileKind (domain stays UI-free).
func kindOf(n nodes.Node) string {
	ct := strings.ToLower(n.ContentType)
	switch {
	case strings.HasPrefix(ct, "audio/"):
		return "aud"
	case strings.HasPrefix(ct, "video/"):
		return "vid"
	case strings.HasPrefix(ct, "image/"):
		return "img"
	}
	switch strings.ToLower(path.Ext(n.Name)) {
	case ".mp3", ".flac", ".ogg", ".m4a", ".wav", ".opus", ".aac":
		return "aud"
	case ".mp4", ".mkv", ".webm", ".mov", ".avi", ".m4v":
		return "vid"
	case ".jpg", ".jpeg", ".png", ".webp":
		return "img"
	}
	return ""
}

// titleFromFilename strips the extension and a leading "NN " track
// number ("07 Karma Police.mp3" → "Karma Police", track 7).
func titleFromFilename(name string, track *int) string {
	base := strings.TrimSuffix(name, path.Ext(name))
	if m := trackNoRe.FindStringSubmatch(base); m != nil {
		if track != nil && *track == 0 {
			*track, _ = strconv.Atoi(m[1])
		}
		base = m[2]
	}
	return cleanTitle(base)
}
