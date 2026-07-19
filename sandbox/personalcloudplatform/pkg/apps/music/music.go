// Package music is the Music app (spec §9): artists, albums, tracks,
// playlists, and a persistent in-app mini-player over the music folders
// registered in the member's drives. Membership IS the subscription
// (the same union as Video), access is re-checked on every browse and
// stream, and the player is a bottom bar that music.js keeps alive
// across in-app navigation (pjax-style page swaps — the audio element
// lives outside the swapped container). Track streaming rides the drive
// app's Range-enabled /drive/file endpoint.
package music

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

//go:embed *.tpl
var tplFS embed.FS

//go:embed assets
var assetFS embed.FS

// handlers carries the parsed template set alongside the kernel.
type handlers struct {
	k     *kernel.App
	views *template.Template
}

// Mount registers the Music app's routes. Called explicitly from
// cmd/pcp.
func Mount(k *kernel.App) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	return kernel.Mount{App: "music", Routes: []kernel.Route{
		{Pattern: "GET /music", Handler: k.Authed(k.FeatureGate("music", h.home))},
		{Pattern: "GET /music/f/{drive}/{folder}", Handler: k.Authed(k.FeatureGate("music", h.folder))},
		{Pattern: "GET /music/album/{drive}/{folder}/{slug}", Handler: k.Authed(k.FeatureGate("music", h.album))},
		{Pattern: "GET /music/artist/{drive}/{folder}/{slug}", Handler: k.Authed(k.FeatureGate("music", h.artist))},
		{Pattern: "GET /music/search", Handler: k.Authed(k.FeatureGate("music", h.search))},
		// Playlists.
		{Pattern: "GET /music/pl/{pl}", Handler: k.Authed(k.FeatureGate("music", h.playlist))},
		{Pattern: "POST /music/playlists/create", Handler: k.Authed(k.FeatureGate("music", h.playlistCreate))},
		{Pattern: "POST /music/playlists/rename", Handler: k.Authed(k.FeatureGate("music", h.playlistRename))},
		{Pattern: "POST /music/playlists/delete", Handler: k.Authed(k.FeatureGate("music", h.playlistDelete))},
		{Pattern: "POST /music/pl/{pl}/add", Handler: k.Authed(k.FeatureGate("music", h.playlistAdd))},
		{Pattern: "POST /music/pl/{pl}/remove", Handler: k.Authed(k.FeatureGate("music", h.playlistRemove))},
		{Pattern: "POST /music/pl/{pl}/move", Handler: k.Authed(k.FeatureGate("music", h.playlistMove))},
		// Per-user state.
		{Pattern: "POST /music/list", Handler: k.Authed(k.FeatureGate("music", h.listToggle))},
		{Pattern: "POST /music/hide", Handler: k.Authed(k.FeatureGate("music", h.hideToggle))},
		// The mini-player's heartbeat (Recently played + launcher card).
		{Pattern: "POST /music/progress", Handler: k.Authed(k.FeatureGate("music", h.progressPost))},
		// Harvested APIC cover art.
		{Pattern: "GET /music/art/{drive}/{folder}/{slug}", Handler: k.Authed(k.FeatureGate("music", h.art))},
		// The app's own JS/CSS.
		{Pattern: "GET /music/assets/", Handler: k.FeatureGateHTTP("music", assetHandler())},
	}}
}

// assetHandler serves the app's embedded JS/CSS (cache like /static/).
func assetHandler() http.Handler {
	fileServer := http.FileServer(http.FS(assetFS))
	return http.StripPrefix("/music/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", ui.AssetCache(r.URL.Path))
		fileServer.ServeHTTP(w, r)
	}))
}

// access resolves the member's role for a node through the ONE resolver
// (shares.Access) — every browse and stream re-checks.
func (h *handlers) access(r *http.Request, user users.User, driveID, nodeID string) error {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	role, _, err := h.k.Shares.Access(cctx, user.Username, driveID, nodeID)
	if err != nil {
		return err
	}
	if !drives.RoleAtLeast(role, drives.RoleViewer) {
		return drives.ErrAccessDenied
	}
	return nil
}

// musicFolders returns the member's visible music folders plus the full
// set (hidden included) for the manage strip.
func (h *handlers) musicFolders(r *http.Request, user users.User) (visible, all []media.Folder) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	folders, err := h.k.Media.ForUser(cctx, user.Username)
	if err != nil {
		h.k.Log.Warn("media folder union failed", "user", user.Username, "err", err)
	}
	for _, f := range folders {
		if f.Kind != media.KindMusic {
			continue
		}
		all = append(all, f)
		if !f.Hidden {
			visible = append(visible, f)
		}
	}
	return visible, all
}

// EntryVM decorates a catalog entry with its URLs.
type EntryVM struct {
	media.CatalogEntry
	FolderID string
	ArtURL   string
	FileURL  string // tracks: the Range-streaming source
	OpenURL  string
}

// entryVM builds the view model for one catalog entry. back (a
// same-origin relative path, or "") rides a track's player link so the
// app host's Back button returns to the album/artist page the track was
// opened from instead of the containing drive folder.
func entryVM(folder media.Folder, e media.CatalogEntry, back string) EntryVM {
	vm := EntryVM{CatalogEntry: e, FolderID: folder.FolderID}
	switch {
	case e.ArtBlob != "":
		vm.ArtURL = "/music/art/" + folder.DriveID + "/" + folder.FolderID + "/" + e.ArtBlob
	case e.ArtNode != "":
		vm.ArtURL = "/drive/thumb/" + folder.DriveID + "/" + e.ArtNode
	}
	base := "/music/" + strings.TrimSuffix(e.Kind, "s") + "/" + folder.DriveID + "/" + folder.FolderID + "/"
	switch e.Kind {
	case media.CatAlbum:
		vm.OpenURL = base + e.ID
	case media.CatArtist:
		vm.OpenURL = base + e.ID
	case media.CatTrack:
		vm.FileURL = "/drive/file/" + e.DriveID + "/" + e.NodeID + "?inline=1"
		vm.OpenURL = "/drive/app/music?drive=" + e.DriveID + "&node=" + e.NodeID
		if back != "" {
			vm.OpenURL += "&back=" + url.QueryEscape(back)
		}
	}
	return vm
}

// Shelf is one registered folder's strip on the home page.
type Shelf struct {
	Folder  media.Folder
	Entries []EntryVM
}

// HomePage is /music's typed page struct.
type HomePage struct {
	kernel.Chrome
	Recent    []EntryVM // recently played tracks, dressed from catalogs
	Favorites []EntryVM
	Shelves   []Shelf
	Artists   []EntryVM
	Playlists []media.Playlist
	Folders   []media.Folder
	HasAny    bool
}

// trackIndex maps driveID/nodeID → a dressed track VM (with album art)
// across the member's visible folders — Recently-played's join.
func (h *handlers) trackIndex(r *http.Request, visible []media.Folder) map[string]EntryVM {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	out := map[string]EntryVM{}
	for _, f := range visible {
		albumArt := map[string]string{}
		if albums, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, media.CatAlbum); err == nil {
			for _, al := range albums {
				vm := entryVM(f, al, "")
				albumArt[al.ID] = vm.ArtURL
			}
		}
		tracks, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, media.CatTrack)
		if err != nil {
			continue
		}
		for _, t := range tracks {
			key := f.DriveID + "/" + t.NodeID
			if _, ok := out[key]; ok {
				continue
			}
			vm := entryVM(f, t, "")
			if i := strings.LastIndex(t.ID, "/"); i > 0 {
				if art := albumArt[t.ID[:i]]; art != "" {
					vm.ArtURL = art
				}
				vm.OpenURL = "/music/album/" + f.DriveID + "/" + f.FolderID + "/" + t.ID[:i]
			}
			out[key] = vm
		}
	}
	return out
}

// home renders /music: recently played, favorites, per-folder album
// shelves, the artist strip, and playlists.
func (h *handlers) home(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	visible, all := h.musicFolders(r, user)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg := HomePage{
		Chrome:  h.k.Chrome(r, "Music", "music", sess, user),
		Folders: all,
		HasAny:  len(all) > 0,
	}
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")

	// Recently played: heartbeat rows filtered to tracks a currently
	// visible registered folder still covers (unregistering hides them
	// without deleting the rows — re-registering brings them back), then
	// resolved live against the catalogs (access re-checked).
	if prog, err := h.k.Media.RecentCoveredProgress(cctx, user.Username, media.ProgMusic, 12, false); err == nil && len(prog) > 0 {
		idx := h.trackIndex(r, visible)
		for _, p := range prog {
			if h.access(r, user, p.DriveID, p.NodeID) != nil {
				continue
			}
			if vm, ok := idx[p.DriveID+"/"+p.NodeID]; ok {
				pg.Recent = append(pg.Recent, vm)
				continue
			}
			if _, found, err := h.k.Nodes.GetByID(cctx, p.DriveID, p.NodeID); err != nil || !found {
				continue
			}
			vm := EntryVM{CatalogEntry: media.CatalogEntry{Kind: media.CatTrack, Title: p.Title, DriveID: p.DriveID, NodeID: p.NodeID}}
			vm.FileURL = "/drive/file/" + p.DriveID + "/" + p.NodeID + "?inline=1"
			vm.OpenURL = "/drive/app/music?drive=" + p.DriveID + "&node=" + p.NodeID + "&back=" + url.QueryEscape("/music")
			pg.Recent = append(pg.Recent, vm)
		}
	}

	// Favorites resolved live (albums/artists).
	folderSet := map[string]media.Folder{}
	for _, f := range visible {
		folderSet[f.DriveID+"/"+f.FolderID] = f
	}
	if items, err := h.k.Media.ListItems(cctx, user.Username, media.ListFavs); err == nil {
		for _, it := range items {
			f, ok := folderSet[it.DriveID+"/"+it.FolderID]
			if !ok || (it.Kind != media.CatAlbum && it.Kind != media.CatArtist) {
				continue
			}
			e, found, err := h.k.Media.GetCatalogEntry(cctx, it.DriveID, it.FolderID, it.Kind, it.Slug)
			if err != nil || !found {
				continue
			}
			pg.Favorites = append(pg.Favorites, entryVM(f, e, ""))
			if len(pg.Favorites) >= 24 {
				break
			}
		}
	}

	for _, f := range visible {
		albums, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, media.CatAlbum)
		if err != nil {
			continue
		}
		sort.Slice(albums, func(i, j int) bool { return albums[i].AddedAt.After(albums[j].AddedAt) })
		if len(albums) > 12 {
			albums = albums[:12]
		}
		shelf := Shelf{Folder: f}
		for _, e := range albums {
			shelf.Entries = append(shelf.Entries, entryVM(f, e, ""))
		}
		pg.Shelves = append(pg.Shelves, shelf)

		if artists, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, media.CatArtist); err == nil {
			for _, a := range artists {
				pg.Artists = append(pg.Artists, entryVM(f, a, ""))
			}
		}
	}
	sort.Slice(pg.Artists, func(i, j int) bool {
		return strings.ToLower(pg.Artists[i].Title) < strings.ToLower(pg.Artists[j].Title)
	})
	if pls, err := h.k.Media.Playlists(cctx, user.Username); err == nil {
		pg.Playlists = pls
	}
	ui.Render(w, h.views, "music_home", pg)
}

// userFolder authorizes one registered music folder for the member.
func (h *handlers) userFolder(r *http.Request, user users.User, driveID, folderID string) (media.Folder, bool) {
	if err := h.access(r, user, driveID, folderID); err != nil {
		return media.Folder{}, false
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	reg, found, err := h.k.Media.Get(cctx, driveID, folderID)
	if err != nil || !found || !reg.Has(media.KindMusic) {
		return media.Folder{}, false
	}
	f := media.Folder{DriveID: driveID, FolderID: folderID, Kind: media.KindMusic}
	if n, found, _ := h.k.Nodes.GetByID(cctx, driveID, folderID); found {
		f.Name = n.Name
	}
	if info, ok, _ := h.k.Media.GetScanInfo(cctx, driveID, folderID); ok {
		f.ScanInfo = info
	}
	return f, true
}

// FolderPage is one registered folder's page: albums + artists.
type FolderPage struct {
	kernel.Chrome
	Folder  media.Folder
	Albums  []EntryVM
	Artists []EntryVM
	Hidden  bool
	Back    string
}

// folder renders one registered folder.
func (h *handlers) folder(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	f, ok := h.userFolder(r, user, r.PathValue("drive"), r.PathValue("folder"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg := FolderPage{
		Chrome: h.k.Chrome(r, f.Name, "music", sess, user),
		Folder: f,
		Back:   "/music/f/" + f.DriveID + "/" + f.FolderID,
	}
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")
	if albums, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, media.CatAlbum); err == nil {
		sort.Slice(albums, func(i, j int) bool {
			return strings.ToLower(albums[i].Title) < strings.ToLower(albums[j].Title)
		})
		for _, e := range albums {
			pg.Albums = append(pg.Albums, entryVM(f, e, ""))
		}
	}
	if artists, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, media.CatArtist); err == nil {
		sort.Slice(artists, func(i, j int) bool {
			return strings.ToLower(artists[i].Title) < strings.ToLower(artists[j].Title)
		})
		for _, e := range artists {
			pg.Artists = append(pg.Artists, entryVM(f, e, ""))
		}
	}
	if folders, err := h.k.Media.ForUser(cctx, user.Username); err == nil {
		for _, hf := range folders {
			if hf.DriveID == f.DriveID && hf.FolderID == f.FolderID {
				pg.Hidden = hf.Hidden
			}
		}
	}
	ui.Render(w, h.views, "music_folder", pg)
}

// AlbumPage is one album's track list + queue player.
type AlbumPage struct {
	kernel.Chrome
	Folder      media.Folder
	Album       EntryVM
	Tracks      []EntryVM
	Playlists   []media.Playlist
	FolderURL   string
	OnFavorites bool
	Back        string
}

// album renders one album.
func (h *handlers) album(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	f, ok := h.userFolder(r, user, r.PathValue("drive"), r.PathValue("folder"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	slug := r.PathValue("slug")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	entry, found, err := h.k.Media.GetCatalogEntry(cctx, f.DriveID, f.FolderID, media.CatAlbum, slug)
	if err != nil || !found {
		// A retag changes the slug — land on the folder, not a dead 404.
		http.Redirect(w, r, "/music/f/"+f.DriveID+"/"+f.FolderID, http.StatusSeeOther)
		return
	}
	pg := AlbumPage{
		Chrome: h.k.Chrome(r, entry.Title, "music", sess, user),
		Folder: f, Album: entryVM(f, entry, ""),
		Back: "/music/album/" + f.DriveID + "/" + f.FolderID + "/" + slug,
	}
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")
	tracks, err := h.k.Media.ListCatalogUnder(cctx, f.DriveID, f.FolderID, media.CatTrack, slug)
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	for _, t := range tracks {
		vm := entryVM(f, t, pg.Back)
		vm.ArtURL = pg.Album.ArtURL
		pg.Tracks = append(pg.Tracks, vm)
	}
	if pls, err := h.k.Media.Playlists(cctx, user.Username); err == nil {
		pg.Playlists = pls
	}
	if len(pg.Tracks) > 0 {
		if crumbs, err := h.k.Nodes.Path(cctx, f.DriveID, pg.Tracks[0].NodeID); err == nil && len(crumbs) >= 2 {
			pg.FolderURL = "/drive/d/" + f.DriveID + "/" + crumbs[len(crumbs)-2].ID
		}
	}
	pg.OnFavorites = h.k.Media.OnList(cctx, user.Username, media.ListFavs,
		media.ListItem{DriveID: f.DriveID, FolderID: f.FolderID, Kind: media.CatAlbum, Slug: slug, Title: entry.Title})
	ui.Render(w, h.views, "music_album", pg)
}

// ArtistPage is one artist's discography.
type ArtistPage struct {
	kernel.Chrome
	Folder media.Folder
	Artist EntryVM
	Albums []EntryVM
	Back   string
}

// artist renders one artist's albums (from the entry's Refs).
func (h *handlers) artist(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	f, ok := h.userFolder(r, user, r.PathValue("drive"), r.PathValue("folder"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	entry, found, err := h.k.Media.GetCatalogEntry(cctx, f.DriveID, f.FolderID, media.CatArtist, r.PathValue("slug"))
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	pg := ArtistPage{
		Chrome: h.k.Chrome(r, entry.Title, "music", sess, user),
		Folder: f, Artist: entryVM(f, entry, ""),
		Back: "/music/f/" + f.DriveID + "/" + f.FolderID,
	}
	for _, slug := range entry.Refs {
		album, found, err := h.k.Media.GetCatalogEntry(cctx, f.DriveID, f.FolderID, media.CatAlbum, slug)
		if err != nil || !found {
			continue
		}
		pg.Albums = append(pg.Albums, entryVM(f, album, ""))
	}
	ui.Render(w, h.views, "music_artist", pg)
}

// PlaylistPage is one playlist + its queue.
type PlaylistPage struct {
	kernel.Chrome
	PL   media.Playlist
	Back string
}

// playlist renders one playlist. Track access is checked at STREAM time
// (/drive/file re-checks); a row the member can't read simply won't
// play.
func (h *handlers) playlist(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pl, found, err := h.k.Media.GetPlaylist(cctx, user.Username, r.PathValue("pl"))
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	pg := PlaylistPage{
		Chrome: h.k.Chrome(r, pl.Name, "music", sess, user),
		PL:     pl,
		Back:   "/music/pl/" + pl.ID,
	}
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")
	ui.Render(w, h.views, "music_playlist", pg)
}

// SearchPage is /music/search's typed page struct.
type SearchPage struct {
	kernel.Chrome
	Query string
	Hits  []EntryVM
}

// search matches album/artist titles across the member's visible music
// catalogs.
func (h *handlers) search(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	visible, _ := h.musicFolders(r, user)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg := SearchPage{
		Chrome: h.k.Chrome(r, "Search music", "music", sess, user),
		Query:  r.URL.Query().Get("q"),
	}
	if q != "" {
		for _, f := range visible {
			for _, kind := range []string{media.CatAlbum, media.CatArtist} {
				entries, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, kind)
				if err != nil {
					continue
				}
				for _, e := range entries {
					if strings.Contains(strings.ToLower(e.Title), q) ||
						strings.Contains(strings.ToLower(e.Artist), q) {
						pg.Hits = append(pg.Hits, entryVM(f, e, ""))
						if len(pg.Hits) >= 200 {
							break
						}
					}
				}
			}
		}
	}
	ui.Render(w, h.views, "music_search", pg)
}

// --- playlists ------------------------------------------------------------

func (h *handlers) playlistCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pl, err := h.k.Media.CreatePlaylist(cctx, user.Username, r.FormValue("name"))
	if err != nil {
		h.k.Respond(w, r, backTo(r), err, nil)
		return
	}
	h.k.Respond(w, r, "/music/pl/"+pl.ID, nil, map[string]any{"id": pl.ID})
}

func (h *handlers) playlistRename(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	plID := r.FormValue("pl")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Media.UpdatePlaylist(cctx, user.Username, plID, func(pl *media.Playlist) error {
		pl.Name = strings.TrimSpace(r.FormValue("name"))
		return nil
	})
	h.k.Respond(w, r, "/music/pl/"+plID, err, nil)
}

func (h *handlers) playlistDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Media.DeletePlaylist(cctx, user.Username, r.FormValue("pl"))
	h.k.Respond(w, r, "/music", err, nil)
}

// playlistAdd appends a track. The member must be able to READ the file
// (a playlist grants nothing at play time either — /drive/file
// re-checks).
func (h *handlers) playlistAdd(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	plID := r.PathValue("pl")
	back := backTo(r)
	driveID, nodeID := r.FormValue("drive"), r.FormValue("node")
	if err := h.access(r, user, driveID, nodeID); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Media.UpdatePlaylist(cctx, user.Username, plID, func(pl *media.Playlist) error {
		pl.Tracks = append(pl.Tracks, media.PlaylistTrack{
			DriveID: driveID, NodeID: nodeID,
			Title:  strings.TrimSpace(r.FormValue("title")),
			Artist: strings.TrimSpace(r.FormValue("artist")),
		})
		return nil
	})
	h.k.Respond(w, r, back, err, nil)
}

func (h *handlers) playlistRemove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	plID := r.PathValue("pl")
	idx, _ := strconv.Atoi(r.FormValue("idx"))
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Media.UpdatePlaylist(cctx, user.Username, plID, func(pl *media.Playlist) error {
		if idx < 0 || idx >= len(pl.Tracks) {
			return users.ErrNotFound
		}
		pl.Tracks = append(pl.Tracks[:idx], pl.Tracks[idx+1:]...)
		return nil
	})
	h.k.Respond(w, r, "/music/pl/"+plID, err, nil)
}

func (h *handlers) playlistMove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	plID := r.PathValue("pl")
	idx, _ := strconv.Atoi(r.FormValue("idx"))
	dir := 1
	if r.FormValue("dir") == "up" {
		dir = -1
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Media.UpdatePlaylist(cctx, user.Username, plID, func(pl *media.Playlist) error {
		j := idx + dir
		if idx < 0 || idx >= len(pl.Tracks) || j < 0 || j >= len(pl.Tracks) {
			return nil // nudging past the edge is a no-op, not an error
		}
		pl.Tracks[idx], pl.Tracks[j] = pl.Tracks[j], pl.Tracks[idx]
		return nil
	})
	h.k.Respond(w, r, "/music/pl/"+plID, err, nil)
}

// --- per-user state ----------------------------------------------------------

// listToggle adds/removes a favorites (or watchlist) item.
func (h *handlers) listToggle(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	back := backTo(r)
	it := media.ListItem{
		DriveID: r.FormValue("drive"), FolderID: r.FormValue("folder"),
		Kind: r.FormValue("kind"), Slug: r.FormValue("slug"), Title: r.FormValue("title"),
	}
	if err := h.access(r, user, it.DriveID, it.FolderID); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Media.SetListItem(cctx, user.Username, r.FormValue("list"), it, r.FormValue("on") == "1")
	h.k.Respond(w, r, back, err, nil)
}

// hideToggle hides/unhides one registered folder from THIS member's
// view.
func (h *handlers) hideToggle(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, folderID := r.FormValue("drive"), r.FormValue("folder")
	back := backTo(r)
	if err := h.access(r, user, driveID, folderID); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Media.SetHidden(cctx, user.Username, driveID, folderID, r.FormValue("on") == "1")
	h.k.Respond(w, r, back, err, nil)
}

// progressPost records a play heartbeat from the mini-player
// (Recently played + the launcher card). Access re-checked.
func (h *handlers) progressPost(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID := r.FormValue("drive"), r.FormValue("node")
	if err := h.access(r, user, driveID, nodeID); err != nil {
		http.NotFound(w, r)
		return
	}
	pos, _ := strconv.ParseFloat(r.FormValue("pos"), 64)
	dur, _ := strconv.ParseFloat(r.FormValue("dur"), 64)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	_ = h.k.Media.SetProgress(cctx, user.Username, media.Progress{
		DriveID: driveID, NodeID: nodeID, Kind: media.ProgMusic,
		Title: strings.TrimSpace(r.FormValue("title")), Pos: pos, Dur: dur,
	})
	w.WriteHeader(http.StatusNoContent)
}

// art serves harvested APIC cover art. Gated on folder access — the
// slug is server-derived, never a raw filename.
func (h *handlers) art(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	driveID, folderID, slug := r.PathValue("drive"), r.PathValue("folder"), r.PathValue("slug")
	if !media.ValidSlug(slug) {
		http.NotFound(w, r)
		return
	}
	if err := h.access(r, user, driveID, folderID); err != nil {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	key := media.ArtKey(driveID, folderID, slug)
	_, ct, found, err := h.k.Media.DB.StatBlob(cctx, key)
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	if !strings.HasPrefix(ct, "image/") || ct == "image/svg+xml" {
		ct = "image/jpeg"
	}
	var buf bytes.Buffer
	if err := h.k.Media.DB.GetBlob(cctx, key, &buf); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(buf.Bytes())
}

// backTo builds the redirect target for a mutation (its hidden "back"
// field, or the app home).
func backTo(r *http.Request) string {
	if b := r.FormValue("back"); strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") {
		return b
	}
	return "/music"
}
