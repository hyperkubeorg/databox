// media.go — the Media API v1 endpoints (spec §12.2, scopes media:read
// and media:write). Reads cover catalogs, detail entries, cover art, and
// the caller's per-user state (progress, watchlist/favorites, playlists).
// Playable bytes stream through the existing GET /api/v1/drive/download
// (drive:read) with Range — the catalog entries carry the driveId/nodeId
// to feed it. media:write covers the per-user playback state a native
// player needs: progress heartbeats, watched marks, watchlist/favorites,
// and playlists — all rows that grant nothing (access is re-checked on
// every read and stream, matching the domain's rule).
package api

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// mediaRoutes are the Media endpoints Mount registers.
func (h *handlers) mediaRoutes(k *kernel.App) []kernel.Route {
	return []kernel.Route{
		{Pattern: "GET /api/v1/media/folders", Handler: k.APIAuthed(apikeys.ScopeMediaRead, h.mediaFolders)},
		{Pattern: "GET /api/v1/media/catalog/{drive}/{folder}", Handler: k.APIAuthed(apikeys.ScopeMediaRead, h.mediaCatalog)},
		{Pattern: "GET /api/v1/media/entry/{drive}/{folder}/{kind}/{slug}", Handler: k.APIAuthed(apikeys.ScopeMediaRead, h.mediaEntry)},
		{Pattern: "GET /api/v1/media/progress", Handler: k.APIAuthed(apikeys.ScopeMediaRead, h.mediaProgress)},
		{Pattern: "PUT /api/v1/media/progress", Handler: k.APIAuthed(apikeys.ScopeMediaWrite, h.mediaProgressPut)},
		{Pattern: "POST /api/v1/media/watched", Handler: k.APIAuthed(apikeys.ScopeMediaWrite, h.mediaWatched)},
		{Pattern: "GET /api/v1/media/lists/{list}", Handler: k.APIAuthed(apikeys.ScopeMediaRead, h.mediaListGet)},
		{Pattern: "PUT /api/v1/media/lists/{list}", Handler: k.APIAuthed(apikeys.ScopeMediaWrite, h.mediaListPut)},
		{Pattern: "GET /api/v1/media/playlists", Handler: k.APIAuthed(apikeys.ScopeMediaRead, h.mediaPlaylists)},
		{Pattern: "POST /api/v1/media/playlists", Handler: k.APIAuthed(apikeys.ScopeMediaWrite, h.mediaPlaylistCreate)},
		{Pattern: "GET /api/v1/media/playlists/{id}", Handler: k.APIAuthed(apikeys.ScopeMediaRead, h.mediaPlaylistGet)},
		{Pattern: "PATCH /api/v1/media/playlists/{id}", Handler: k.APIAuthed(apikeys.ScopeMediaWrite, h.mediaPlaylistPatch)},
		{Pattern: "DELETE /api/v1/media/playlists/{id}", Handler: k.APIAuthed(apikeys.ScopeMediaWrite, h.mediaPlaylistDelete)},
		{Pattern: "GET /api/v1/media/art/{drive}/{folder}/{slug}", Handler: k.APIAuthed(apikeys.ScopeMediaRead, h.mediaArt)},
	}
}

// --- resource shapes ----------------------------------------------------------

// mediaFolderResponse is one registered folder in GET /media/folders.
type mediaFolderResponse struct {
	DriveID   string    `json:"driveId"`
	FolderID  string    `json:"folderId"`
	Kind      string    `json:"kind"` // video | music
	Name      string    `json:"name"`
	DriveName string    `json:"driveName"`
	Hidden    bool      `json:"hidden"`
	Items     int       `json:"items,omitempty"`
	ScannedAt time.Time `json:"scannedAt,omitzero"`
}

// mediaEntryResponse is one catalog entry.
type mediaEntryResponse struct {
	Kind        string    `json:"kind"`
	Slug        string    `json:"slug"`
	Title       string    `json:"title"`
	Artist      string    `json:"artist,omitempty"`
	Year        int       `json:"year,omitempty"`
	Track       int       `json:"track,omitempty"`
	Season      int       `json:"season,omitempty"`
	Episode     int       `json:"episode,omitempty"`
	Seasons     int       `json:"seasons,omitempty"`
	Parts       bool      `json:"parts,omitempty"`
	Items       int       `json:"items,omitempty"`
	Description string    `json:"description,omitempty"`
	Genres      []string  `json:"genres,omitempty"`
	Refs        []string  `json:"refs,omitempty"`
	DriveID     string    `json:"driveId,omitempty"` // stream via /drive/download
	NodeID      string    `json:"nodeId,omitempty"`
	ArtNode     string    `json:"artNode,omitempty"` // download w/ drive:read
	ArtURL      string    `json:"artUrl,omitempty"`  // GET /media/art (media:read)
	AddedAt     time.Time `json:"addedAt,omitzero"`
}

func toMediaEntry(driveID, folderID string, e media.CatalogEntry) mediaEntryResponse {
	out := mediaEntryResponse{
		Kind: e.Kind, Slug: e.ID, Title: e.Title, Artist: e.Artist,
		Year: e.Year, Track: e.Track, Season: e.Season, Episode: e.Episode,
		Seasons: e.Seasons, Parts: e.Parts, Items: e.Items,
		Description: e.Description, Genres: e.Genres, Refs: e.Refs,
		DriveID: e.DriveID, NodeID: e.NodeID, ArtNode: e.ArtNode,
		AddedAt: e.AddedAt,
	}
	if e.ArtBlob != "" {
		out.ArtURL = "/api/v1/media/art/" + driveID + "/" + folderID + "/" + e.ArtBlob
	}
	return out
}

// --- endpoints ----------------------------------------------------------

// mediaFolders answers the caller's registered-folder union (membership
// IS the subscription; the caller's per-user hidden flags ride along).
func (h *handlers) mediaFolders(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	folders, err := h.k.Media.ForUser(cctx, user.Username)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "folder union failed")
		return
	}
	out := []mediaFolderResponse{}
	for _, f := range folders {
		out = append(out, mediaFolderResponse{
			DriveID: f.DriveID, FolderID: f.FolderID, Kind: f.Kind,
			Name: f.Name, DriveName: f.DriveName, Hidden: f.Hidden,
			Items: f.Items, ScannedAt: f.ScannedAt,
		})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"folders": out})
}

// mediaAccess gates one registered folder: access through the ONE
// resolver, denial indistinguishable from absence.
func (h *handlers) mediaAccess(r *http.Request, user users.User, driveID, folderID string) (media.Registration, error) {
	if err := h.access(r, user, driveID, folderID, drives.RoleViewer); err != nil {
		return media.Registration{}, err
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	reg, found, err := h.k.Media.Get(cctx, driveID, folderID)
	if err != nil {
		return media.Registration{}, err
	}
	if !found || len(reg.Kinds) == 0 {
		return media.Registration{}, users.ErrNotFound
	}
	return reg, nil
}

// mediaCatalog lists one registered folder's catalog. ?kind= picks one
// catalog kind; default = the top-level kinds of every registered
// registration kind (a folder registered as both video AND music
// defaults to movies+series+albums+artists).
func (h *handlers) mediaCatalog(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, folderID := r.PathValue("drive"), r.PathValue("folder")
	reg, err := h.mediaAccess(r, user, driveID, folderID)
	if err != nil {
		apiErr(w, err)
		return
	}
	var kinds []string
	for _, regKind := range reg.Kinds {
		kinds = append(kinds, media.TopKinds(regKind)...)
	}
	if k := r.URL.Query().Get("kind"); k != "" {
		switch k {
		case media.CatAlbum, media.CatArtist, media.CatTrack, media.CatMovie, media.CatSeries, media.CatEpisode:
			kinds = []string{k}
		default:
			kernel.APIError(w, http.StatusBadRequest, "bad_request", "unknown catalog kind")
			return
		}
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	out := []mediaEntryResponse{}
	for _, kind := range kinds {
		entries, err := h.k.Media.ListCatalog(cctx, driveID, folderID, kind)
		if err != nil {
			kernel.APIError(w, http.StatusInternalServerError, "internal", "catalog read failed")
			return
		}
		for _, e := range entries {
			out = append(out, toMediaEntry(driveID, folderID, e))
		}
	}
	// "kinds" is the registration set; "kind" (the first registered
	// kind) survives for pre-set clients.
	kernel.JSON(w, http.StatusOK, map[string]any{"kind": reg.Kinds[0], "kinds": reg.Kinds, "entries": out})
}

// mediaEntry answers one top-level entry plus its children (album →
// tracks, series → episodes, artist → its albums, movie → none).
func (h *handlers) mediaEntry(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, folderID := r.PathValue("drive"), r.PathValue("folder")
	kind, slug := r.PathValue("kind"), r.PathValue("slug")
	if _, err := h.mediaAccess(r, user, driveID, folderID); err != nil {
		apiErr(w, err)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	entry, found, err := h.k.Media.GetCatalogEntry(cctx, driveID, folderID, kind, slug)
	if err != nil || !found {
		apiErr(w, users.ErrNotFound)
		return
	}
	children := []mediaEntryResponse{}
	switch kind {
	case media.CatAlbum:
		if tracks, err := h.k.Media.ListCatalogUnder(cctx, driveID, folderID, media.CatTrack, slug); err == nil {
			for _, t := range tracks {
				children = append(children, toMediaEntry(driveID, folderID, t))
			}
		}
	case media.CatSeries:
		if eps, err := h.k.Media.ListCatalogUnder(cctx, driveID, folderID, media.CatEpisode, slug); err == nil {
			for _, e := range eps {
				children = append(children, toMediaEntry(driveID, folderID, e))
			}
		}
	case media.CatArtist:
		for _, ref := range entry.Refs {
			if album, found, err := h.k.Media.GetCatalogEntry(cctx, driveID, folderID, media.CatAlbum, ref); err == nil && found {
				children = append(children, toMediaEntry(driveID, folderID, album))
			}
		}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{
		"entry": toMediaEntry(driveID, folderID, entry), "children": children,
	})
}

// mediaProgressResponse is one row of GET /media/progress.
type mediaProgressResponse struct {
	DriveID string    `json:"driveId"`
	NodeID  string    `json:"nodeId"`
	Kind    string    `json:"kind"` // video | music
	Title   string    `json:"title,omitempty"`
	Pos     float64   `json:"pos"`
	Dur     float64   `json:"dur,omitempty"`
	Watched bool      `json:"watched"`
	At      time.Time `json:"at"`
}

// mediaProgress lists the caller's playback state, newest first
// (?kind=video|music filters; access re-checked per row so a lost
// membership leaks nothing).
func (h *handlers) mediaProgress(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	kinds := []string{media.ProgVideo, media.ProgMusic}
	if k := r.URL.Query().Get("kind"); k == media.ProgVideo || k == media.ProgMusic {
		kinds = []string{k}
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	out := []mediaProgressResponse{}
	for _, kind := range kinds {
		rows, err := h.k.Media.RecentProgress(cctx, user.Username, kind, 100, false)
		if err != nil {
			kernel.APIError(w, http.StatusInternalServerError, "internal", "progress read failed")
			return
		}
		for _, p := range rows {
			if h.access(r, user, p.DriveID, p.NodeID, drives.RoleViewer) != nil {
				continue
			}
			out = append(out, mediaProgressResponse{
				DriveID: p.DriveID, NodeID: p.NodeID, Kind: p.Kind, Title: p.Title,
				Pos: p.Pos, Dur: p.Dur, Watched: p.Watched(), At: p.At,
			})
		}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"progress": out})
}

// mediaProgressPut records one playback heartbeat (a native player's
// cross-device resume). Access to the played file is checked up front —
// same stance as the web player's heartbeat endpoint.
func (h *handlers) mediaProgressPut(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var body struct {
		DriveID string  `json:"driveId"`
		NodeID  string  `json:"nodeId"`
		Kind    string  `json:"kind"` // video | music (default video)
		Title   string  `json:"title"`
		Pos     float64 `json:"pos"`
		Dur     float64 `json:"dur"`
	}
	if decodeJSON(w, r, &body) != nil {
		return
	}
	if err := h.access(r, user, body.DriveID, body.NodeID, drives.RoleViewer); err != nil {
		apiErr(w, err)
		return
	}
	kind := media.ProgVideo
	if body.Kind == media.ProgMusic {
		kind = media.ProgMusic
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Media.SetProgress(cctx, user.Username, media.Progress{
		DriveID: body.DriveID, NodeID: body.NodeID, Kind: kind,
		Title: strings.TrimSpace(body.Title), Pos: body.Pos, Dur: body.Dur,
	}); err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "progress write failed")
		return
	}
	p, _, err := h.k.Media.GetProgress(cctx, user.Username, body.NodeID)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "progress read failed")
		return
	}
	kernel.JSON(w, http.StatusOK, mediaProgressResponse{
		DriveID: p.DriveID, NodeID: p.NodeID, Kind: p.Kind, Title: p.Title,
		Pos: p.Pos, Dur: p.Dur, Watched: p.Watched(), At: p.At,
	})
}

// mediaWatched flips one file's watched bit by hand (off clears the
// position too — "watch it fresh", same semantic as the web app).
func (h *handlers) mediaWatched(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var body struct {
		DriveID string `json:"driveId"`
		NodeID  string `json:"nodeId"`
		Watched bool   `json:"watched"`
	}
	if decodeJSON(w, r, &body) != nil {
		return
	}
	if err := h.access(r, user, body.DriveID, body.NodeID, drives.RoleViewer); err != nil {
		apiErr(w, err)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Media.MarkWatched(cctx, user.Username, body.DriveID, body.NodeID, body.Watched); err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "watched write failed")
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"watched": body.Watched})
}

// mediaListItemResponse is one saved watchlist/favorites reference.
type mediaListItemResponse struct {
	DriveID  string    `json:"driveId"`
	FolderID string    `json:"folderId"`
	Kind     string    `json:"kind"` // movies | series | albums | artists
	Slug     string    `json:"slug"`
	Title    string    `json:"title,omitempty"`
	At       time.Time `json:"at"`
}

// mediaListGet reads the caller's watchlist or favorites, newest first.
// Items resolve LIVE at render in the apps; here rows whose folder
// access is gone are filtered the same way.
func (h *handlers) mediaListGet(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	list := r.PathValue("list")
	if !media.ValidList(list) {
		apiErr(w, users.ErrNotFound)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	items, err := h.k.Media.ListItems(cctx, user.Username, list)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "list read failed")
		return
	}
	out := []mediaListItemResponse{}
	for _, it := range items {
		if h.access(r, user, it.DriveID, it.FolderID, drives.RoleViewer) != nil {
			continue
		}
		out = append(out, mediaListItemResponse{
			DriveID: it.DriveID, FolderID: it.FolderID, Kind: it.Kind,
			Slug: it.Slug, Title: it.Title, At: it.At,
		})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"items": out})
}

// mediaListPut adds (on=true) or removes a watchlist/favorites item.
func (h *handlers) mediaListPut(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	list := r.PathValue("list")
	if !media.ValidList(list) {
		apiErr(w, users.ErrNotFound)
		return
	}
	var body struct {
		DriveID  string `json:"driveId"`
		FolderID string `json:"folderId"`
		Kind     string `json:"kind"`
		Slug     string `json:"slug"`
		Title    string `json:"title"`
		On       bool   `json:"on"`
	}
	if decodeJSON(w, r, &body) != nil {
		return
	}
	if err := h.access(r, user, body.DriveID, body.FolderID, drives.RoleViewer); err != nil {
		apiErr(w, err)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Media.SetListItem(cctx, user.Username, list, media.ListItem{
		DriveID: body.DriveID, FolderID: body.FolderID, Kind: body.Kind,
		Slug: body.Slug, Title: strings.TrimSpace(body.Title),
	}, body.On)
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"on": body.On})
}

// --- playlists ----------------------------------------------------------

// playlistTrackResource is one playlist track reference. Tracks grant
// nothing: playback re-checks drive access when the client streams the
// node via /drive/download.
type playlistTrackResource struct {
	DriveID string `json:"driveId"`
	NodeID  string `json:"nodeId"`
	Title   string `json:"title"`
	Artist  string `json:"artist,omitempty"`
}

// playlistResource is one playlist in API shape.
type playlistResource struct {
	ID     string                  `json:"id"`
	Name   string                  `json:"name"`
	Tracks []playlistTrackResource `json:"tracks"`
	At     time.Time               `json:"at"`
}

func toPlaylistResource(pl media.Playlist) playlistResource {
	out := playlistResource{ID: pl.ID, Name: pl.Name, Tracks: []playlistTrackResource{}, At: pl.At}
	for _, t := range pl.Tracks {
		out.Tracks = append(out.Tracks, playlistTrackResource{
			DriveID: t.DriveID, NodeID: t.NodeID, Title: t.Title, Artist: t.Artist,
		})
	}
	return out
}

// mediaPlaylists lists the caller's playlists, name-sorted.
func (h *handlers) mediaPlaylists(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pls, err := h.k.Media.Playlists(cctx, user.Username)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "playlist read failed")
		return
	}
	out := []playlistResource{}
	for _, pl := range pls {
		out = append(out, toPlaylistResource(pl))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"playlists": out})
}

// mediaPlaylistCreate makes an empty named playlist.
func (h *handlers) mediaPlaylistCreate(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var body struct {
		Name string `json:"name"`
	}
	if decodeJSON(w, r, &body) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pl, err := h.k.Media.CreatePlaylist(cctx, user.Username, body.Name)
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusCreated, toPlaylistResource(pl))
}

// mediaPlaylistGet loads one playlist.
func (h *handlers) mediaPlaylistGet(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pl, found, err := h.k.Media.GetPlaylist(cctx, user.Username, r.PathValue("id"))
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "playlist read failed")
		return
	}
	if !found {
		apiErr(w, users.ErrNotFound)
		return
	}
	kernel.JSON(w, http.StatusOK, toPlaylistResource(pl))
}

// mediaPlaylistPatch renames and/or replaces a playlist's track list.
// Full-replace tracks keeps the client dead simple (fetch, reorder
// locally, PATCH back) — the domain caps size and playback re-checks
// access, so a bogus reference plays nothing.
func (h *handlers) mediaPlaylistPatch(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var body struct {
		Name   *string                  `json:"name"`
		Tracks *[]playlistTrackResource `json:"tracks"`
	}
	if decodeJSON(w, r, &body) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	plID := r.PathValue("id")
	err := h.k.Media.UpdatePlaylist(cctx, user.Username, plID, func(pl *media.Playlist) error {
		if body.Name != nil {
			pl.Name = strings.TrimSpace(*body.Name)
		}
		if body.Tracks != nil {
			tracks := make([]media.PlaylistTrack, 0, len(*body.Tracks))
			for _, t := range *body.Tracks {
				if !kvx.ValidID(t.DriveID) || !kvx.ValidID(t.NodeID) {
					return fmt.Errorf("bad track reference")
				}
				if len(t.Title) > 255 {
					t.Title = t.Title[:255]
				}
				tracks = append(tracks, media.PlaylistTrack{
					DriveID: t.DriveID, NodeID: t.NodeID, Title: t.Title, Artist: t.Artist,
				})
			}
			pl.Tracks = tracks
		}
		return nil
	})
	if err != nil {
		apiErr(w, err)
		return
	}
	pl, found, err := h.k.Media.GetPlaylist(cctx, user.Username, plID)
	if err != nil || !found {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "playlist read failed")
		return
	}
	kernel.JSON(w, http.StatusOK, toPlaylistResource(pl))
}

// mediaPlaylistDelete removes one playlist.
func (h *handlers) mediaPlaylistDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	plID := r.PathValue("id")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if _, found, err := h.k.Media.GetPlaylist(cctx, user.Username, plID); err != nil || !found {
		apiErr(w, users.ErrNotFound)
		return
	}
	if err := h.k.Media.DeletePlaylist(cctx, user.Username, plID); err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "playlist delete failed")
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// mediaArt serves one harvested APIC cover blob (folder-designated and
// per-file art are ordinary drive files — download them with
// drive:read).
func (h *handlers) mediaArt(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, folderID, slug := r.PathValue("drive"), r.PathValue("folder"), r.PathValue("slug")
	if !media.ValidSlug(slug) {
		apiErr(w, users.ErrNotFound)
		return
	}
	if _, err := h.mediaAccess(r, user, driveID, folderID); err != nil {
		apiErr(w, err)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	key := media.ArtKey(driveID, folderID, slug)
	_, ct, found, err := h.k.Media.DB.StatBlob(cctx, key)
	if err != nil || !found {
		apiErr(w, users.ErrNotFound)
		return
	}
	if !strings.HasPrefix(ct, "image/") || ct == "image/svg+xml" {
		ct = "image/jpeg"
	}
	var buf bytes.Buffer
	if err := h.k.Media.DB.GetBlob(cctx, key, &buf); err != nil {
		apiErr(w, users.ErrNotFound)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(buf.Bytes())
}
