// Package video is the Video app (spec §9): the streaming lens over the
// video folders registered in the member's drives, in the Slate design
// system. Membership IS the subscription — the home page folds the
// member's drive list over each drive's registrations; joining a shared
// drive makes its registered content appear, leaving removes it — and
// access is re-checked on EVERY browse and stream (shares.Access, the
// one resolver). Playback is the drive app host's Range-seek player;
// this app owns the shelves, detail pages, search, and the per-user
// state: progress heartbeats (Continue watching + cross-device resume),
// watchlist/favorites, hide-folder overrides, watch history.
package video

import (
	"bytes"
	"context"
	"embed"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
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

// Mount registers the Video app's routes. Called explicitly from
// cmd/pcp.
func Mount(k *kernel.App) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	return kernel.Mount{App: "video", Routes: []kernel.Route{
		{Pattern: "GET /video", Handler: k.Authed(k.FeatureGate("video", h.home))},
		{Pattern: "GET /video/f/{drive}/{folder}", Handler: k.Authed(k.FeatureGate("video", h.folder))},
		{Pattern: "GET /video/t/{drive}/{folder}/{kind}/{slug}", Handler: k.Authed(k.FeatureGate("video", h.title))},
		{Pattern: "GET /video/search", Handler: k.Authed(k.FeatureGate("video", h.search))},
		// The player's resume/heartbeat endpoints (apps/video.js in the
		// drive app host calls these; sendBeacon-friendly: csrf rides the
		// form body).
		{Pattern: "GET /video/progress", Handler: k.Authed(k.FeatureGate("video", h.progressGet))},
		{Pattern: "POST /video/progress", Handler: k.Authed(k.FeatureGate("video", h.progressPost))},
		// The player's frame-capture save (editors): a canvas snapshot
		// becomes the show's poster or the episode's own preview art.
		{Pattern: "POST /video/frame", Handler: k.Authed(k.FeatureGate("video", h.frameSave))},
		// Per-user state mutations.
		{Pattern: "POST /video/list", Handler: k.Authed(k.FeatureGate("video", h.listToggle))},
		{Pattern: "POST /video/hide", Handler: k.Authed(k.FeatureGate("video", h.hideToggle))},
		{Pattern: "POST /video/history", Handler: k.Authed(k.FeatureGate("video", h.history))},
		// The app's own CSS.
		{Pattern: "GET /video/assets/", Handler: k.FeatureGateHTTP("video", assetHandler())},
	}}
}

// assetHandler serves the app's embedded CSS (cache like /static/).
func assetHandler() http.Handler {
	fileServer := http.FileServer(http.FS(assetFS))
	return http.StripPrefix("/video/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// videoFolders returns the member's visible video folders (hidden
// filtered out) plus the full set for the manage strip.
func (h *handlers) videoFolders(r *http.Request, user users.User) (visible, all []media.Folder) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	folders, err := h.k.Media.ForUser(cctx, user.Username)
	if err != nil {
		h.k.Log.Warn("media folder union failed", "user", user.Username, "err", err)
	}
	for _, f := range folders {
		if f.Kind != media.KindVideo {
			continue
		}
		all = append(all, f)
		if !f.Hidden {
			visible = append(visible, f)
		}
	}
	return visible, all
}

// EntryVM decorates a catalog entry with its URLs and watch state.
type EntryVM struct {
	media.CatalogEntry
	FolderID string
	ArtURL   string
	PlayURL  string
	OpenURL  string // shelf click-through (detail page, or PlayURL)
	Resume   int    // percent watched, for the progress bar (0 = none)
	Watched  bool
}

// entryVM builds the view model for one catalog entry. back (a
// same-origin relative path, or "") rides the player link so the app
// host's Back button returns to the page the play was launched from
// (a series/movie detail page) instead of the containing drive folder.
func entryVM(folder media.Folder, e media.CatalogEntry, back string) EntryVM {
	vm := EntryVM{CatalogEntry: e, FolderID: folder.FolderID}
	if e.ArtNode != "" {
		vm.ArtURL = "/drive/thumb/" + folder.DriveID + "/" + e.ArtNode
	}
	base := "/video/t/" + folder.DriveID + "/" + folder.FolderID + "/"
	backQ := ""
	if back != "" {
		backQ = "&back=" + url.QueryEscape(back)
	}
	switch e.Kind {
	case media.CatSeries:
		vm.OpenURL = base + media.CatSeries + "/" + e.ID
	case media.CatMovie:
		vm.PlayURL = "/drive/app/video?drive=" + e.DriveID + "&node=" + e.NodeID + backQ
		vm.OpenURL = base + media.CatMovie + "/" + e.ID
	case media.CatEpisode:
		vm.PlayURL = "/drive/app/video?drive=" + e.DriveID + "&node=" + e.NodeID + backQ
		vm.OpenURL = vm.PlayURL
	}
	return vm
}

// Shelf is one registered folder's strip on the home page.
type Shelf struct {
	Folder  media.Folder
	Entries []EntryVM
}

// HomePage is /video's typed page struct.
type HomePage struct {
	kernel.Chrome
	Continue  []EntryVM
	Watchlist []EntryVM
	Favorites []EntryVM
	Shelves   []Shelf
	Folders   []media.Folder // ALL video folders incl. hidden (manage strip)
	HasAny    bool
}

// home renders /video: continue watching, watchlist, favorites, and one
// recently-added shelf per visible registered folder.
func (h *handlers) home(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	visible, all := h.videoFolders(r, user)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()

	pg := HomePage{
		Chrome:  h.k.Chrome(r, "Video", "video", sess, user),
		Folders: all,
		HasAny:  len(all) > 0,
	}
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")

	// Continue watching: recent unfinished progress → files, filtered to
	// nodes a currently visible registered folder still covers (an
	// unregistered folder's rows go dark WITHOUT being deleted —
	// re-registering brings the positions back), then DRESSED from the
	// catalog (episode art falls back to the show's poster). Rows whose
	// access is gone drop out silently.
	if prog, err := h.k.Media.RecentCoveredProgress(cctx, user.Username, media.ProgVideo, 12, true); err == nil && len(prog) > 0 {
		type catHit struct {
			folder media.Folder
			e      media.CatalogEntry
		}
		nodeCat := map[string]catHit{}
		for _, f := range visible {
			for _, kind := range []string{media.CatMovie, media.CatEpisode} {
				entries, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, kind)
				if err != nil {
					continue
				}
				for _, e := range entries {
					key := f.DriveID + "/" + e.NodeID
					if _, ok := nodeCat[key]; !ok {
						nodeCat[key] = catHit{folder: f, e: e}
					}
				}
			}
		}
		for _, p := range prog {
			if h.access(r, user, p.DriveID, p.NodeID) != nil {
				continue
			}
			if _, found, err := h.k.Nodes.GetByID(cctx, p.DriveID, p.NodeID); err != nil || !found {
				continue // deleted media never renders a ghost tile
			}
			var vm EntryVM
			if hit, ok := nodeCat[p.DriveID+"/"+p.NodeID]; ok {
				e := hit.e
				if e.Kind == media.CatEpisode && e.ArtNode == "" {
					if slug, _, found := strings.Cut(e.ID, "/"); found {
						if sh, ok, _ := h.k.Media.GetCatalogEntry(cctx, hit.folder.DriveID, hit.folder.FolderID, media.CatSeries, slug); ok {
							e.ArtNode = sh.ArtNode
						}
					}
				}
				vm = entryVM(hit.folder, e, "/video")
			} else {
				vm = EntryVM{CatalogEntry: media.CatalogEntry{
					Kind: media.CatMovie, Title: p.Title, DriveID: p.DriveID, NodeID: p.NodeID,
				}}
				vm.PlayURL = "/drive/app/video?drive=" + p.DriveID + "&node=" + p.NodeID + "&back=" + url.QueryEscape("/video")
				vm.OpenURL = vm.PlayURL
			}
			if p.Dur > 0 {
				vm.Resume = int(p.Pos * 100 / p.Dur)
			}
			pg.Continue = append(pg.Continue, vm)
		}
	}

	// Watchlist + favorites: saved references resolved LIVE — vanished
	// entries (or lost memberships) drop out silently.
	folderSet := map[string]media.Folder{}
	for _, f := range visible {
		folderSet[f.DriveID+"/"+f.FolderID] = f
	}
	resolve := func(list string) []EntryVM {
		items, err := h.k.Media.ListItems(cctx, user.Username, list)
		if err != nil {
			return nil
		}
		var out []EntryVM
		for _, it := range items {
			f, ok := folderSet[it.DriveID+"/"+it.FolderID]
			if !ok || (it.Kind != media.CatMovie && it.Kind != media.CatSeries) {
				continue
			}
			e, found, err := h.k.Media.GetCatalogEntry(cctx, it.DriveID, it.FolderID, it.Kind, it.Slug)
			if err != nil || !found {
				continue
			}
			out = append(out, entryVM(f, e, ""))
			if len(out) >= 24 {
				break
			}
		}
		return out
	}
	pg.Watchlist = resolve(media.ListWatch)
	pg.Favorites = resolve(media.ListFavs)

	for _, f := range visible {
		entries, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, media.CatMovie)
		if err != nil {
			continue
		}
		if series, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, media.CatSeries); err == nil {
			entries = append(entries, series...)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].AddedAt.After(entries[j].AddedAt) })
		if len(entries) > 12 {
			entries = entries[:12]
		}
		shelf := Shelf{Folder: f}
		for _, e := range entries {
			shelf.Entries = append(shelf.Entries, entryVM(f, e, ""))
		}
		pg.Shelves = append(pg.Shelves, shelf)
	}
	ui.Render(w, h.views, "video_home", pg)
}

// userFolder authorizes one registered video folder for the member:
// access re-checked, registration verified.
func (h *handlers) userFolder(r *http.Request, user users.User, driveID, folderID string) (media.Folder, bool) {
	if err := h.access(r, user, driveID, folderID); err != nil {
		return media.Folder{}, false
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	reg, found, err := h.k.Media.Get(cctx, driveID, folderID)
	if err != nil || !found || !reg.Has(media.KindVideo) {
		return media.Folder{}, false
	}
	f := media.Folder{DriveID: driveID, FolderID: folderID, Kind: media.KindVideo}
	if n, found, _ := h.k.Nodes.GetByID(cctx, driveID, folderID); found {
		f.Name = n.Name
	}
	if info, ok, _ := h.k.Media.GetScanInfo(cctx, driveID, folderID); ok {
		f.ScanInfo = info
	}
	return f, true
}

// FolderPage is one registered folder's library page.
type FolderPage struct {
	kernel.Chrome
	Folder media.Folder
	Movies []EntryVM
	Series []EntryVM
	Hidden bool
	Back   string
}

// folder renders one registered folder: movies grid + series grid.
func (h *handlers) folder(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	f, ok := h.userFolder(r, user, r.PathValue("drive"), r.PathValue("folder"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg := FolderPage{
		Chrome: h.k.Chrome(r, f.Name, "video", sess, user),
		Folder: f,
		Back:   "/video/f/" + f.DriveID + "/" + f.FolderID,
	}
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")
	vms := func(kind string) []EntryVM {
		entries, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, kind)
		if err != nil {
			return nil
		}
		sort.Slice(entries, func(i, j int) bool {
			return strings.ToLower(entries[i].Title) < strings.ToLower(entries[j].Title)
		})
		out := make([]EntryVM, 0, len(entries))
		for _, e := range entries {
			out = append(out, entryVM(f, e, ""))
		}
		return out
	}
	pg.Movies = vms(media.CatMovie)
	pg.Series = vms(media.CatSeries)
	if hidden, err := h.k.Media.ForUser(cctx, user.Username); err == nil {
		for _, hf := range hidden {
			if hf.DriveID == f.DriveID && hf.FolderID == f.FolderID {
				pg.Hidden = hf.Hidden
			}
		}
	}
	ui.Render(w, h.views, "video_lib", pg)
}

// EpisodeVM is one episode row on a detail page: playable + watch state.
type EpisodeVM struct {
	EntryVM
	Percent int // partial progress (0 when none/finished)
}

// SeasonVM groups a detail page's episodes.
type SeasonVM struct {
	Season   int // 0 = a parts show (rendered without season chrome)
	Episodes []EpisodeVM
}

// TitlePage is the movie/series detail page.
type TitlePage struct {
	kernel.Chrome
	Folder  media.Folder
	Kind    string // movies | series
	Entry   EntryVM
	Seasons []SeasonVM
	// Movie-only watch state.
	Watched bool
	Percent int
	// NextUp is the first unwatched episode (series).
	NextUp *EpisodeVM
	// List/toggle state.
	OnWatchlist bool
	OnFavorites bool
	FolderURL   string // "Show in Drive"
	AllNodes    []string
	Back        string
}

// title renders the unified movie/series detail page.
func (h *handlers) title(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	f, ok := h.userFolder(r, user, r.PathValue("drive"), r.PathValue("folder"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	kind, slug := r.PathValue("kind"), r.PathValue("slug")
	if kind != media.CatMovie && kind != media.CatSeries {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	entry, found, err := h.k.Media.GetCatalogEntry(cctx, f.DriveID, f.FolderID, kind, slug)
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	back := "/video/t/" + f.DriveID + "/" + f.FolderID + "/" + kind + "/" + slug
	pg := TitlePage{
		Chrome: h.k.Chrome(r, entry.Title, "video", sess, user),
		Folder: f, Kind: kind, Entry: entryVM(f, entry, back),
		Back: back,
	}
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")
	item := media.ListItem{DriveID: f.DriveID, FolderID: f.FolderID, Kind: kind, Slug: slug, Title: entry.Title}
	pg.OnWatchlist = h.k.Media.OnList(cctx, user.Username, media.ListWatch, item)
	pg.OnFavorites = h.k.Media.OnList(cctx, user.Username, media.ListFavs, item)

	folderOf := func(nodeID string) string {
		crumbs, err := h.k.Nodes.Path(cctx, f.DriveID, nodeID)
		if err != nil || len(crumbs) < 2 {
			return "/drive/d/" + f.DriveID + "/" + f.FolderID
		}
		return "/drive/d/" + f.DriveID + "/" + crumbs[len(crumbs)-2].ID
	}
	if kind == media.CatMovie {
		pg.FolderURL = folderOf(entry.NodeID)
		pg.AllNodes = []string{entry.NodeID}
		if prog, err := h.k.Media.ProgressMap(cctx, user.Username, pg.AllNodes); err == nil {
			if p, ok := prog[entry.NodeID]; ok {
				pg.Watched = p.Watched()
				if !pg.Watched && p.Dur > 0 {
					pg.Percent = int(p.Pos * 100 / p.Dur)
				}
			}
		}
	} else {
		eps, err := h.k.Media.ListCatalogUnder(cctx, f.DriveID, f.FolderID, media.CatEpisode, slug)
		if err != nil {
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}
		for _, e := range eps {
			pg.AllNodes = append(pg.AllNodes, e.NodeID)
		}
		prog, err := h.k.Media.ProgressMap(cctx, user.Username, pg.AllNodes)
		if err != nil {
			h.k.Log.Warn("progress map failed", "err", err)
		}
		bySeason := map[int]*SeasonVM{}
		var order []int
		for _, e := range eps {
			vm := EpisodeVM{EntryVM: entryVM(f, e, back)}
			if p, ok := prog[e.NodeID]; ok {
				vm.Watched = p.Watched()
				if !vm.Watched && p.Dur > 0 {
					vm.Percent = int(p.Pos * 100 / p.Dur)
				}
			}
			sv, ok := bySeason[e.Season]
			if !ok {
				sv = &SeasonVM{Season: e.Season}
				bySeason[e.Season] = sv
				order = append(order, e.Season)
			}
			sv.Episodes = append(sv.Episodes, vm)
			if pg.NextUp == nil && !vm.Watched {
				next := vm
				pg.NextUp = &next
			}
		}
		sort.Ints(order)
		for _, s := range order {
			pg.Seasons = append(pg.Seasons, *bySeason[s])
		}
		if len(eps) > 0 {
			pg.FolderURL = folderOf(eps[0].NodeID)
		}
	}
	ui.Render(w, h.views, "video_title", pg)
}

// SearchPage is /video/search's typed page struct.
type SearchPage struct {
	kernel.Chrome
	Query string
	Hits  []EntryVM
}

// search matches titles/descriptions across the member's visible video
// catalogs.
func (h *handlers) search(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	visible, _ := h.videoFolders(r, user)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg := SearchPage{
		Chrome: h.k.Chrome(r, "Search video", "video", sess, user),
		Query:  r.URL.Query().Get("q"),
	}
	if q != "" {
		for _, f := range visible {
			for _, kind := range []string{media.CatMovie, media.CatSeries} {
				entries, err := h.k.Media.ListCatalog(cctx, f.DriveID, f.FolderID, kind)
				if err != nil {
					continue
				}
				for _, e := range entries {
					if strings.Contains(strings.ToLower(e.Title), q) ||
						strings.Contains(strings.ToLower(e.Description), q) {
						pg.Hits = append(pg.Hits, entryVM(f, e, ""))
						if len(pg.Hits) >= 200 {
							break
						}
					}
				}
			}
		}
	}
	ui.Render(w, h.views, "video_search", pg)
}

// progressGet answers the player's resume lookup: {pos, dur}.
func (h *handlers) progressGet(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	nodeID := r.URL.Query().Get("node")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	p, found, err := h.k.Media.GetProgress(cctx, user.Username, nodeID)
	if err != nil || !found {
		kernel.JSON(w, http.StatusOK, map[string]any{"pos": 0})
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"pos": p.Pos, "dur": p.Dur})
}

// progressPost records a heartbeat. Access is re-checked — a heartbeat
// for a file the member can't read writes nothing.
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
	kind := media.ProgVideo
	if r.FormValue("kind") == media.ProgMusic {
		kind = media.ProgMusic
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	_ = h.k.Media.SetProgress(cctx, user.Username, media.Progress{
		DriveID: driveID, NodeID: nodeID, Kind: kind,
		Title: strings.TrimSpace(r.FormValue("title")), Pos: pos, Dur: dur,
	})
	w.WriteHeader(http.StatusNoContent)
}

// seasonDirRe recognizes "Season 2"-style folders — a poster saved from
// an episode inside one belongs one level UP, at the show root.
var seasonDirRe = regexp.MustCompile(`(?i)^season[ ._-]*\d+$`)

// quotaFor resolves the member's effective storage quota.
func (h *handlers) quotaFor(r *http.Request, user users.User) int64 {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		h.k.Log.Warn("site config read failed", "err", err)
	}
	return site.QuotaFor(sc, user.QuotaOverride, user.Tier, h.k.DefaultQuota)
}

// frameSave stores a frame the player captured: kind "poster" lands as
// poster.jpg in the SHOW folder (the file's parent — or one level above
// a "Season N" dir), kind "thumb" as "<video filename>.jpg" beside the
// file — both designated art names the indexer already reads, so the
// artwork comes from the show itself, never a third-party service.
// Editor+ (it writes a file); overwriting an existing poster by name is
// fine — StoreFile versions it. Covering registered folders rescan in
// the background so the art takes effect.
func (h *handlers) frameSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	fail := func(status int, msg string) {
		kernel.JSON(w, status, map[string]any{"ok": false, "error": msg})
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		fail(http.StatusBadRequest, "bad form")
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		fail(http.StatusForbidden, "bad csrf token")
		return
	}
	driveID, nodeID, kind := r.FormValue("drive"), r.FormValue("node"), r.FormValue("kind")
	if kind != "poster" && kind != "thumb" {
		fail(http.StatusBadRequest, "bad kind")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	role, _, err := h.k.Shares.Access(cctx, user.Username, driveID, nodeID)
	if err != nil || !drives.RoleAtLeast(role, drives.RoleEditor) {
		fail(http.StatusForbidden, "you need edit access to save artwork")
		return
	}
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || node.IsDir {
		fail(http.StatusNotFound, "no such file")
		return
	}
	file, hdr, err := r.FormFile("image")
	if err != nil || hdr.Size <= 0 || hdr.Size > 8<<20 {
		fail(http.StatusBadRequest, "bad image")
		return
	}
	defer file.Close()
	img, err := io.ReadAll(io.LimitReader(file, 8<<20))
	ct := http.DetectContentType(img)
	if err != nil || !strings.HasPrefix(ct, "image/") || ct == "image/svg+xml" {
		fail(http.StatusBadRequest, "not an image")
		return
	}
	ext := ".jpg"
	if ct == "image/png" {
		ext = ".png"
	}
	crumbs, err := h.k.Nodes.Path(cctx, driveID, nodeID)
	if err != nil || len(crumbs) < 2 {
		fail(http.StatusNotFound, "no such file")
		return
	}
	dirID := crumbs[len(crumbs)-2].ID
	name := node.Name + ext // thumb: the indexer's "<video filename>.jpg" convention
	if kind == "poster" {
		name = "poster" + ext
		if len(crumbs) >= 3 && seasonDirRe.MatchString(crumbs[len(crumbs)-2].Name) {
			dirID = crumbs[len(crumbs)-3].ID // show-level, above "Season N"
		}
	}
	if _, err := h.k.Nodes.StoreFile(cctx, driveID, dirID, name, bytes.NewReader(img), h.quotaFor(r, user), user.Username); err != nil {
		fail(kernel.ErrStatus(err), kernel.UserErr(err))
		return
	}
	h.rescanCovering(driveID, crumbs)
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "saved": name})
}

// rescanCovering kicks a background rescan of every REGISTERED folder
// covering the node (walking its crumbs upward), so a freshly saved
// poster/preview takes effect without waiting for the sweep. No
// covering registration = nothing to do, silently.
func (h *handlers) rescanCovering(driveID string, crumbs []nodes.Crumb) {
	ancestors := make([]string, 0, len(crumbs))
	for i := len(crumbs) - 2; i >= 0; i-- {
		ancestors = append(ancestors, crumbs[i].ID)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		for _, folderID := range ancestors {
			if _, found, err := h.k.Media.Get(ctx, driveID, folderID); err != nil || !found {
				continue
			}
			if _, err := h.k.Media.Rescan(ctx, driveID, folderID); err != nil {
				h.k.Log.Warn("rescan after frame save failed", "drive", driveID, "folder", folderID, "err", err)
			}
		}
	}()
}

// listToggle adds/removes a watchlist or favorites item.
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

// history manages watch history: mark watched/unwatched, clear a node
// set, clear everything.
func (h *handlers) history(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	back := backTo(r)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	var err error
	switch r.FormValue("op") {
	case "mark":
		err = h.k.Media.MarkWatched(cctx, user.Username, r.FormValue("drive"), r.FormValue("node"), true)
	case "unmark":
		err = h.k.Media.MarkWatched(cctx, user.Username, r.FormValue("drive"), r.FormValue("node"), false)
	case "clear-nodes":
		_ = r.ParseForm()
		err = h.k.Media.ClearProgress(cctx, user.Username, r.Form["node"])
	case "clear-all":
		err = h.k.Media.ClearProgress(cctx, user.Username, nil)
	default:
		err = users.ErrNotFound
	}
	h.k.Respond(w, r, back, err, nil)
}

// backTo builds the redirect target for a mutation (its hidden "back"
// field, or the app home).
func backTo(r *http.Request) string {
	if b := r.FormValue("back"); strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") {
		return b
	}
	return "/video"
}
