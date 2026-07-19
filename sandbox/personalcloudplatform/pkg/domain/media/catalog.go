// catalog.go — the derived catalog: a REBUILDABLE CACHE keyed under the
// registration it describes (kvx key table):
//
//	/pcp/media/catalog/<driveID>/<folderID>/<kind>/<slug> → CatalogEntry
//	/pcp/media/catalog/<driveID>/<folderID>/meta          → ScanInfo
//	/pcp/media/art/<driveID>/<folderID>/<slug>            → BLOB (APIC art)
//
// A rescan re-derives every record from the files (indexer.go),
// DeleteRanges the old prefix, and writes the new — retagging a file
// and rescanning is the whole maintenance story. Unregistering drops
// the catalog; the files are untouched.
package media

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Key prefixes this file owns (kvx key table).
const (
	catalogPrefix = "/pcp/media/catalog/"
	artPrefix     = "/pcp/media/art/"
)

// Catalog kinds (the key segment after the folder id). "meta" is
// reserved for ScanInfo and is never a kind.
const (
	CatAlbum   = "albums"
	CatTrack   = "tracks"
	CatArtist  = "artists"
	CatMovie   = "movies"
	CatSeries  = "series"
	CatEpisode = "episodes"
)

// TopKinds lists a registration kind's top-level catalog kinds (the
// browse pages' shelves; tracks/episodes list under their parent slug).
func TopKinds(regKind string) []string {
	if regKind == KindMusic {
		return []string{CatAlbum, CatArtist}
	}
	return []string{CatMovie, CatSeries}
}

// CatKinds lists EVERY catalog kind a registration kind owns — the
// kind-scoped unregister sweep's domain split (unregistering music must
// leave video's catalog intact, and vice versa).
func CatKinds(regKind string) []string {
	if regKind == KindMusic {
		return []string{CatAlbum, CatTrack, CatArtist}
	}
	return []string{CatMovie, CatSeries, CatEpisode}
}

// CatalogEntry is one denormalized catalog record — shaped for shelves
// and detail pages, not joins.
type CatalogEntry struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"` // the slug this entry is keyed by
	Title   string `json:"title"`
	Artist  string `json:"artist,omitempty"` // music: artist; episodes: series title
	Year    int    `json:"year,omitempty"`
	Track   int    `json:"track,omitempty"`   // tracks: disc order
	Season  int    `json:"season,omitempty"`  // episodes (0 = a parts show)
	Episode int    `json:"episode,omitempty"` // episodes
	// Rich metadata (sidecars, mediameta.go).
	Description string   `json:"description,omitempty"`
	Genres      []string `json:"genres,omitempty"`
	Seasons     int      `json:"seasons,omitempty"` // series: highest season number
	Parts       bool     `json:"parts,omitempty"`   // series: a flat parts show
	// Refs are related catalog slugs (artists → their album slugs).
	Refs []string `json:"refs,omitempty"`
	// Where the playable bytes live (empty for albums/artists/series).
	DriveID string `json:"drive_id,omitempty"`
	NodeID  string `json:"node_id,omitempty"`
	// Art: either a node in the drive (designated folder image, served
	// via /drive/thumb) or a harvested APIC blob under the folder's art
	// space (ArtBlob is the slug segment).
	ArtNode string    `json:"art_node,omitempty"`
	ArtBlob string    `json:"art_blob,omitempty"`
	Items   int       `json:"items,omitempty"` // albums: tracks; series: episodes
	AddedAt time.Time `json:"added_at,omitzero"`
}

// ScanInfo is one registration's scan bookkeeping, stored at the
// catalog's reserved "meta" row (it dies and is reborn with the
// catalog, which is exactly its lifetime).
type ScanInfo struct {
	ScannedAt time.Time `json:"scanned_at"`
	Items     int       `json:"items"`
}

// Key builders. Ids are shape-checked by callers; kind is the fixed
// vocabulary; slugs come from slugify (separator-free by construction).
func catalogKey(driveID, folderID, kind, slug string) string {
	return catalogPrefix + driveID + "/" + folderID + "/" + kind + "/" + slug
}
func catalogMetaKey(driveID, folderID string) string {
	return catalogPrefix + driveID + "/" + folderID + "/meta"
}
func artKey(driveID, folderID, slug string) string {
	return artPrefix + driveID + "/" + folderID + "/" + slug
}

// ArtKey exposes the harvested-art blob key to the serving handlers.
func ArtKey(driveID, folderID, slug string) string { return artKey(driveID, folderID, slug) }

// ValidSlug gates catalog slugs arriving in URLs. Slugify's alphabet is
// a-z0-9- plus the "/" separating a child slug from its parent
// (tracks/episodes) and the SxxEyyy episode segment.
func ValidSlug(slug string) bool {
	if slug == "" || len(slug) > 160 || slug == "meta" {
		return false
	}
	for _, r := range slug {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && (r < 'A' || r > 'Z') && r != '-' && r != '/' {
			return false
		}
	}
	return !strings.Contains(slug, "//")
}

// GetScanInfo loads one registration's last-scan record.
func (s *Store) GetScanInfo(ctx context.Context, driveID, folderID string) (ScanInfo, bool, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(folderID) {
		return ScanInfo{}, false, nil
	}
	var info ScanInfo
	found, err := kvx.GetJSON(ctx, s.DB, catalogMetaKey(driveID, folderID), &info)
	return info, found, err
}

// ListCatalog reads one kind's entries for a registered folder (a whole
// shelf — the catalog is bounded by the folder's size).
func (s *Store) ListCatalog(ctx context.Context, driveID, folderID, kind string) ([]CatalogEntry, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(folderID) {
		return nil, nil
	}
	var out []CatalogEntry
	err := kvx.ScanPrefix(ctx, s.DB, catalogPrefix+driveID+"/"+folderID+"/"+kind+"/", func(_ string, value []byte) error {
		var e CatalogEntry
		if json.Unmarshal(value, &e) == nil {
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

// ListCatalogUnder reads entries under kind/<parentSlug>/ (an album's
// tracks, a show's episodes) in key order — already track/episode
// sorted by construction.
func (s *Store) ListCatalogUnder(ctx context.Context, driveID, folderID, kind, parentSlug string) ([]CatalogEntry, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(folderID) || !ValidSlug(parentSlug) {
		return nil, nil
	}
	var out []CatalogEntry
	err := kvx.ScanPrefix(ctx, s.DB, catalogPrefix+driveID+"/"+folderID+"/"+kind+"/"+parentSlug+"/", func(_ string, value []byte) error {
		var e CatalogEntry
		if json.Unmarshal(value, &e) == nil {
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

// GetCatalogEntry loads one top-level entry by kind + slug (detail
// pages, lists).
func (s *Store) GetCatalogEntry(ctx context.Context, driveID, folderID, kind, slug string) (CatalogEntry, bool, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(folderID) || !ValidSlug(slug) {
		return CatalogEntry{}, false, nil
	}
	switch kind {
	case CatAlbum, CatArtist, CatMovie, CatSeries:
	default:
		return CatalogEntry{}, false, nil
	}
	var e CatalogEntry
	found, err := kvx.GetJSON(ctx, s.DB, catalogKey(driveID, folderID, kind, slug), &e)
	return e, found, err
}

// dropCatalog removes one registration's catalog rows and harvested art
// blobs (the unregister-all path; rescans swap in place instead).
func (s *Store) dropCatalog(ctx context.Context, driveID, folderID string) error {
	if err := s.dropArt(ctx, driveID, folderID); err != nil {
		return err
	}
	return kvx.DeletePrefix(ctx, s.DB, catalogPrefix+driveID+"/"+folderID+"/")
}

// dropCatalogKind removes ONE registration kind's catalog rows and the
// art blobs they reference, leaving the other kind's catalog (and the
// scan meta row) intact — the kind-scoped unregister sweep.
func (s *Store) dropCatalogKind(ctx context.Context, driveID, folderID, regKind string) error {
	for _, kind := range CatKinds(regKind) {
		prefix := catalogPrefix + driveID + "/" + folderID + "/" + kind + "/"
		err := kvx.ScanPrefix(ctx, s.DB, prefix, func(_ string, value []byte) error {
			var e CatalogEntry
			if json.Unmarshal(value, &e) == nil && e.ArtBlob != "" {
				_ = s.DB.DeleteBlob(ctx, artKey(driveID, folderID, e.ArtBlob))
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := kvx.DeletePrefix(ctx, s.DB, prefix); err != nil {
			return err
		}
	}
	return nil
}

// dropArt deletes the harvested art blobs the catalog references
// (folderID "" sweeps the whole drive — the PurgeDrive path). Blobs
// have no range-delete, so the catalog rows are the index.
func (s *Store) dropArt(ctx context.Context, driveID, folderID string) error {
	prefix := catalogPrefix + driveID + "/"
	if folderID != "" {
		prefix += folderID + "/"
	}
	return kvx.ScanPrefix(ctx, s.DB, prefix, func(key string, value []byte) error {
		var e CatalogEntry
		if json.Unmarshal(value, &e) != nil || e.ArtBlob == "" {
			return nil
		}
		fid := folderID
		if fid == "" {
			// …/catalog/<drive>/<folder>/<kind>/<slug>: recover the folder.
			rest := strings.TrimPrefix(key, prefix)
			fid, _, _ = strings.Cut(rest, "/")
		}
		_ = s.DB.DeleteBlob(ctx, artKey(driveID, fid, e.ArtBlob))
		return nil
	})
}

// nonSlug strips everything that isn't a safe slug rune.
var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugify derives a stable, key-safe catalog id from a display string.
// A short FNV suffix keeps distinct titles that slug identically apart.
func slugify(s string) string {
	base := nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	base = strings.Trim(base, "-")
	if len(base) > 48 {
		base = base[:48]
	}
	if base == "" {
		base = "untitled"
	}
	var h uint32 = 2166136261
	for _, c := range []byte(s) {
		h = (h ^ uint32(c)) * 16777619
	}
	return fmt.Sprintf("%s-%04x", base, h&0xffff)
}
