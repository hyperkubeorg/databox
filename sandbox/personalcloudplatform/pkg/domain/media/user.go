// user.go — the per-user media surface (spec §9). Membership IS the
// subscription: ForUser folds the member's drive list over each drive's
// registrations — join a shared drive and its registered content
// appears, leave and it's gone. Everything else here stays strictly
// per-user (kvx key table):
//
//	/pcp/media/hidden/<user>/<driveID>/<folderID> → {} (view override)
//	/pcp/media/progress/<user>/<nodeID>           → Progress
//	/pcp/media/watchlist/<user>/<itemKey>         → ListItem
//	/pcp/media/favorites/<user>/<itemKey>         → ListItem
//	/pcp/media/playlists/<user>/<plID>            → Playlist
//
// NONE of these rows grant access: every browse and every stream
// re-checks drive access (shares.Access in the apps), and rows that
// stop resolving simply drop out of view.
package media

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Key prefixes this file owns (kvx key table).
const (
	hiddenPrefix    = "/pcp/media/hidden/"
	progressPrefix  = "/pcp/media/progress/"
	watchlistPrefix = "/pcp/media/watchlist/"
	favoritesPrefix = "/pcp/media/favorites/"
	playlistsPrefix = "/pcp/media/playlists/"
)

// Folder is one registered folder resolved for a member: the
// registration, the folder's display name, the member's drive role, and
// their personal hidden flag.
type Folder struct {
	DriveID   string
	FolderID  string
	Kind      string // KindVideo | KindMusic
	Name      string // the folder node's display name
	DriveName string
	Role      string // the member's role in the drive
	Hidden    bool   // per-user view override
	ScanInfo         // last scan (zero = never scanned)
}

// ForUser returns the union of registered folders across every drive
// the member belongs to — the Video/Music apps' one subscription
// source. A folder registered for BOTH kinds yields one row per kind
// (the per-kind view the apps expect). Hidden folders ride along
// flagged (the apps filter, the settings surface lists). Folders whose
// node vanished are skipped.
func (s *Store) ForUser(ctx context.Context, username string) ([]Folder, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil, nil
	}
	memberships, err := s.Drives.UserDrives(ctx, username)
	if err != nil {
		return nil, err
	}
	hidden, err := s.hiddenSet(ctx, username)
	if err != nil {
		return nil, err
	}
	var out []Folder
	for _, m := range memberships {
		regs, err := s.ListRegistered(ctx, m.DriveID)
		if err != nil {
			return nil, err
		}
		if len(regs) == 0 {
			continue
		}
		drive, foundDrive, err := s.Drives.Get(ctx, m.DriveID)
		if err != nil {
			return nil, err
		}
		for _, reg := range regs {
			n, found, err := s.Nodes.GetByID(ctx, m.DriveID, reg.FolderID)
			if err != nil {
				return nil, err
			}
			if !found || !n.IsDir {
				continue
			}
			for _, kind := range reg.Kinds {
				f := Folder{
					DriveID: m.DriveID, FolderID: reg.FolderID, Kind: kind,
					Name: n.Name, Role: m.Role,
					Hidden: hidden[m.DriveID+"/"+reg.FolderID],
				}
				if foundDrive {
					f.DriveName = drive.Name
				}
				if info, ok, _ := s.GetScanInfo(ctx, m.DriveID, reg.FolderID); ok {
					f.ScanInfo = info
				}
				out = append(out, f)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// --- hidden overrides -------------------------------------------------------

func hiddenKey(username, driveID, folderID string) string {
	return hiddenPrefix + username + "/" + driveID + "/" + folderID
}

// SetHidden hides (on) or unhides a registered folder from THIS
// member's view. Grants nothing, removes nothing for anyone else.
func (s *Store) SetHidden(ctx context.Context, username, driveID, folderID string, on bool) error {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil || !kvx.ValidID(driveID) || !kvx.ValidID(folderID) {
		return users.ErrNotFound
	}
	if !on {
		return s.DB.Delete(ctx, hiddenKey(username, driveID, folderID))
	}
	return kvx.SetJSON(ctx, s.DB, hiddenKey(username, driveID, folderID), struct{}{})
}

// hiddenSet loads the member's hidden overrides as "drive/folder" keys.
func (s *Store) hiddenSet(ctx context.Context, username string) (map[string]bool, error) {
	out := map[string]bool{}
	err := kvx.ScanPrefix(ctx, s.DB, hiddenPrefix+username+"/", func(key string, _ []byte) error {
		out[key[len(hiddenPrefix)+len(username)+1:]] = true
		return nil
	})
	return out, err
}

// --- playback progress --------------------------------------------------------

// Progress kinds: which app's shelf a record feeds.
const (
	ProgVideo = "video"
	ProgMusic = "music"
)

// Progress is one member's playback state in one file. Title/Kind are
// denormalized from the player so shelves and the launcher card render
// without a catalog join; access is still re-checked at render.
type Progress struct {
	DriveID string  `json:"drive_id"`
	NodeID  string  `json:"node_id"`
	Kind    string  `json:"kind"` // ProgVideo | ProgMusic
	Title   string  `json:"title,omitempty"`
	Pos     float64 `json:"pos"` // seconds
	Dur     float64 `json:"dur"` // seconds (0 = unknown)
	// Done marks an explicit "watched"; ≥95% playback counts as watched
	// without it.
	Done bool      `json:"done,omitempty"`
	At   time.Time `json:"at"`
}

// Watched reports whether the record counts as finished.
func (p Progress) Watched() bool { return p.Done || (p.Dur > 0 && p.Pos >= p.Dur*0.95) }

func progressKey(username, nodeID string) string { return progressPrefix + username + "/" + nodeID }

// SetProgress records a playback heartbeat. Plain write — racing
// heartbeats just keep the latest, which is the semantic anyway. A
// manual Done mark survives later heartbeats.
func (s *Store) SetProgress(ctx context.Context, username string, p Progress) error {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil || !kvx.ValidID(p.NodeID) || !kvx.ValidID(p.DriveID) {
		return nil
	}
	if p.Kind != ProgMusic {
		p.Kind = ProgVideo
	}
	if len(p.Title) > 255 {
		p.Title = p.Title[:255]
	}
	var prev Progress
	if found, _ := kvx.GetJSON(ctx, s.DB, progressKey(username, p.NodeID), &prev); found && prev.Done {
		p.Done = true
	}
	p.At = time.Now().UTC()
	return kvx.SetJSON(ctx, s.DB, progressKey(username, p.NodeID), p)
}

// GetProgress loads one file's saved position.
func (s *Store) GetProgress(ctx context.Context, username, nodeID string) (Progress, bool, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil || !kvx.ValidID(nodeID) {
		return Progress{}, false, nil
	}
	var p Progress
	found, err := kvx.GetJSON(ctx, s.DB, progressKey(username, nodeID), &p)
	return p, found, err
}

// MarkWatched flips one file's watched bit by hand. on=false rewinds it
// to unwatched (clearing position too — "watch it fresh").
func (s *Store) MarkWatched(ctx context.Context, username, driveID, nodeID string, on bool) error {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil || !kvx.ValidID(nodeID) || !kvx.ValidID(driveID) {
		return nil
	}
	if !on {
		return s.DB.Delete(ctx, progressKey(username, nodeID))
	}
	return kvx.SetJSON(ctx, s.DB, progressKey(username, nodeID), Progress{
		DriveID: driveID, NodeID: nodeID, Kind: ProgVideo, Done: true, At: time.Now().UTC(),
	})
}

// ClearProgress removes playback history at any grain: one node set (a
// show's episodes), or — nodeIDs nil — the member's ENTIRE history.
func (s *Store) ClearProgress(ctx context.Context, username string, nodeIDs []string) error {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil
	}
	if nodeIDs == nil {
		return kvx.DeletePrefix(ctx, s.DB, progressPrefix+username+"/")
	}
	for _, id := range nodeIDs {
		if !kvx.ValidID(id) {
			continue
		}
		if err := s.DB.Delete(ctx, progressKey(username, id)); err != nil {
			return err
		}
	}
	return nil
}

// ProgressMap bulk-loads progress for a node set (detail pages: one
// show's episodes).
func (s *Store) ProgressMap(ctx context.Context, username string, nodeIDs []string) (map[string]Progress, error) {
	username = strings.ToLower(username)
	out := map[string]Progress{}
	if users.ValidUsername(username) != nil {
		return out, nil
	}
	for _, id := range nodeIDs {
		if !kvx.ValidID(id) {
			continue
		}
		var p Progress
		if found, err := kvx.GetJSON(ctx, s.DB, progressKey(username, id), &p); err != nil {
			return out, err
		} else if found {
			out[id] = p
		}
	}
	return out, nil
}

// RecentProgress lists a member's most recent playback of one kind,
// newest first, capped. unfinishedOnly filters to in-flight video (the
// Continue-watching shelf); music's Recently-played wants everything.
func (s *Store) RecentProgress(ctx context.Context, username, kind string, limit int, unfinishedOnly bool) ([]Progress, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil, nil
	}
	var all []Progress
	err := kvx.ScanPrefix(ctx, s.DB, progressPrefix+username+"/", func(_ string, value []byte) error {
		var p Progress
		if json.Unmarshal(value, &p) != nil || p.Kind != kind {
			return nil
		}
		if unfinishedOnly && (p.Watched() || p.Pos <= 10) {
			return nil
		}
		all = append(all, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].At.After(all[j].At) })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// playableCatKinds lists the catalog kinds that carry playable nodes
// for one progress kind — the covered-progress join's scan set.
func playableCatKinds(kind string) []string {
	if kind == ProgMusic {
		return []string{CatTrack}
	}
	return []string{CatMovie, CatEpisode}
}

// RecentCoveredProgress is RecentProgress filtered to rows whose node
// is still COVERED by one of the member's visible registered folders of
// that kind (catalog lookup by node id). Progress rows are deliberately
// NOT deleted when a folder is unregistered — the catalog is a
// rebuildable cache, and re-registering must bring positions back — so
// every Continue-watching surface (Video home, Music's Recently played,
// the launcher cards) filters HERE at render instead.
func (s *Store) RecentCoveredProgress(ctx context.Context, username, kind string, limit int, unfinishedOnly bool) ([]Progress, error) {
	rows, err := s.RecentProgress(ctx, username, kind, 0, unfinishedOnly)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	folders, err := s.ForUser(ctx, username)
	if err != nil {
		return nil, err
	}
	covered := map[string]bool{}
	for _, f := range folders {
		if f.Kind != kind || f.Hidden {
			continue
		}
		for _, catKind := range playableCatKinds(kind) {
			entries, err := s.ListCatalog(ctx, f.DriveID, f.FolderID, catKind)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				covered[f.DriveID+"/"+e.NodeID] = true
			}
		}
	}
	var out []Progress
	for _, p := range rows {
		if !covered[p.DriveID+"/"+p.NodeID] {
			continue
		}
		out = append(out, p)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// --- watchlist & favorites ------------------------------------------------------

// Per-user list names.
const (
	ListWatch = "watchlist"
	ListFavs  = "favorites"
)

// ValidList accepts the two list names.
func ValidList(l string) bool { return l == ListWatch || l == ListFavs }

// ListItem is one saved catalog reference. It resolves LIVE at render:
// an item whose catalog entry (or drive membership) vanished simply
// drops out.
type ListItem struct {
	DriveID  string    `json:"drive"`
	FolderID string    `json:"folder"`
	Kind     string    `json:"kind"` // movies | series | albums | artists
	Slug     string    `json:"slug"`
	Title    string    `json:"title"` // display fallback while resolving
	At       time.Time `json:"at"`
}

// listKey: <prefix><user>/<drive>:<folder>:<kind>:<slug> — ids and the
// kind vocabulary can't contain the separator; slugs swap "/" for "~"
// (not in the slug alphabet).
func listKey(username, list string, it ListItem) string {
	prefix := watchlistPrefix
	if list == ListFavs {
		prefix = favoritesPrefix
	}
	slug := strings.ReplaceAll(it.Slug, "/", "~")
	return prefix + username + "/" + it.DriveID + ":" + it.FolderID + ":" + it.Kind + ":" + slug
}

// validListItem is the shared shape gate.
func validListItem(it ListItem) bool {
	if !kvx.ValidID(it.DriveID) || !kvx.ValidID(it.FolderID) || !ValidSlug(it.Slug) {
		return false
	}
	switch it.Kind {
	case CatMovie, CatSeries, CatAlbum, CatArtist:
		return true
	}
	return false
}

// SetListItem adds (on=true) or removes a watchlist/favorites item.
func (s *Store) SetListItem(ctx context.Context, username, list string, it ListItem, on bool) error {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil || !ValidList(list) || !validListItem(it) {
		return users.ErrNotFound
	}
	key := listKey(username, list, it)
	if !on {
		return s.DB.Delete(ctx, key)
	}
	it.At = time.Now().UTC()
	if len(it.Title) > 255 {
		it.Title = it.Title[:255]
	}
	return kvx.SetJSON(ctx, s.DB, key, it)
}

// ListItems reads one list, newest first.
func (s *Store) ListItems(ctx context.Context, username, list string) ([]ListItem, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil || !ValidList(list) {
		return nil, nil
	}
	prefix := watchlistPrefix
	if list == ListFavs {
		prefix = favoritesPrefix
	}
	var out []ListItem
	err := kvx.ScanPrefix(ctx, s.DB, prefix+username+"/", func(_ string, value []byte) error {
		var it ListItem
		if json.Unmarshal(value, &it) == nil {
			out = append(out, it)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

// OnList reports membership (the detail page's toggle state).
func (s *Store) OnList(ctx context.Context, username, list string, it ListItem) bool {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil || !ValidList(list) || !validListItem(it) {
		return false
	}
	_, found, _ := s.DB.Get(ctx, listKey(username, list, it))
	return found
}

// --- playlists ----------------------------------------------------------------

// PlaylistTrack references a playable file directly (drive + node), so
// playlists span folders and survive catalog rescans. Playback
// re-checks access — a track the member can no longer read won't play.
type PlaylistTrack struct {
	DriveID string `json:"drive_id"`
	NodeID  string `json:"node_id"`
	Title   string `json:"title"`
	Artist  string `json:"artist,omitempty"`
}

// Playlist is one member's ordered track list.
type Playlist struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Tracks []PlaylistTrack `json:"tracks"`
	At     time.Time       `json:"at"`
}

// playlistMaxTracks bounds one playlist.
const playlistMaxTracks = 500

func playlistKey(username, plID string) string { return playlistsPrefix + username + "/" + plID }

// CreatePlaylist makes an empty named playlist.
func (s *Store) CreatePlaylist(ctx context.Context, username, name string) (Playlist, error) {
	username = strings.ToLower(username)
	name = strings.TrimSpace(name)
	if users.ValidUsername(username) != nil || name == "" || len(name) > 100 {
		return Playlist{}, fmt.Errorf("playlists need a name (100 chars max)")
	}
	pl := Playlist{ID: kvx.NewID(), Name: name, Tracks: []PlaylistTrack{}, At: time.Now().UTC()}
	return pl, kvx.SetJSON(ctx, s.DB, playlistKey(username, pl.ID), pl)
}

// GetPlaylist loads one of the member's playlists.
func (s *Store) GetPlaylist(ctx context.Context, username, plID string) (Playlist, bool, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil || !kvx.ValidID(plID) {
		return Playlist{}, false, nil
	}
	var pl Playlist
	found, err := kvx.GetJSON(ctx, s.DB, playlistKey(username, plID), &pl)
	return pl, found, err
}

// Playlists lists the member's playlists, name-sorted.
func (s *Store) Playlists(ctx context.Context, username string) ([]Playlist, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil, nil
	}
	var out []Playlist
	err := kvx.ScanPrefix(ctx, s.DB, playlistsPrefix+username+"/", func(_ string, value []byte) error {
		var pl Playlist
		if json.Unmarshal(value, &pl) == nil {
			out = append(out, pl)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// UpdatePlaylist mutates one playlist read-modify-write (rename, add,
// remove, move — the mutate func gets the loaded record).
func (s *Store) UpdatePlaylist(ctx context.Context, username, plID string, mutate func(*Playlist) error) error {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil || !kvx.ValidID(plID) {
		return users.ErrNotFound
	}
	pl, found, err := s.GetPlaylist(ctx, username, plID)
	if err != nil {
		return err
	}
	if !found {
		return users.ErrNotFound
	}
	if err := mutate(&pl); err != nil {
		return err
	}
	if len(pl.Tracks) > playlistMaxTracks {
		return fmt.Errorf("playlists are capped at %d tracks", playlistMaxTracks)
	}
	if strings.TrimSpace(pl.Name) == "" || len(pl.Name) > 100 {
		return fmt.Errorf("playlists need a name (100 chars max)")
	}
	return kvx.SetJSON(ctx, s.DB, playlistKey(username, plID), pl)
}

// DeletePlaylist removes one playlist.
func (s *Store) DeletePlaylist(ctx context.Context, username, plID string) error {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil || !kvx.ValidID(plID) {
		return nil
	}
	return s.DB.Delete(ctx, playlistKey(username, plID))
}
