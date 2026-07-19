// Package media owns the drive-level registered-folder REGISTRY (spec
// §9): a drive editor/owner marks a folder as *Video content* and/or
// *Music content* (the kinds are a SET — one mixed folder can feed both
// apps), and that registration is a property of the DRIVE — join the
// drive, its registered content appears in Video/Music automatically.
//
//	/pcp/media/registry/<driveID>/<folderID> → Registration {kinds, by, at}
//
// Phase 6 adds the rest of the substrate: the rebuildable catalogs the
// indexer derives from the files themselves (catalog.go, indexer.go,
// id3.go, mediameta.go), the per-user surface — union across the
// member's drives, hidden overrides, progress, watchlist/favorites,
// playlists (user.go) — and the scan worker (scan.go). Access is NEVER
// granted by a registration: browse and stream re-check drive access.
package media

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// registryPrefix locates the registrations (kvx key table).
const registryPrefix = "/pcp/media/registry/"

// Registration kinds.
const (
	KindVideo = "video"
	KindMusic = "music"
)

// ValidKind accepts the two registration kinds.
func ValidKind(kind string) bool { return kind == KindVideo || kind == KindMusic }

// Registration marks one folder as media content for its whole drive.
// Kinds is an ordered, deduped SET (KindVideo/KindMusic) — a mixed
// folder registered for both feeds both apps from one row.
type Registration struct {
	Kinds []string  `json:"kinds"`
	By    string    `json:"by"`
	At    time.Time `json:"at"`
}

// Has reports whether the registration covers one kind.
func (reg Registration) Has(kind string) bool {
	for _, k := range reg.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// UnmarshalJSON folds the legacy single-kind row shape ({"kind":"x"},
// pre kinds-as-a-set) into Kinds — live registries keep decoding.
func (reg *Registration) UnmarshalJSON(data []byte) error {
	var raw struct {
		Kinds []string  `json:"kinds"`
		Kind  string    `json:"kind"`
		By    string    `json:"by"`
		At    time.Time `json:"at"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	reg.Kinds, reg.By, reg.At = raw.Kinds, raw.By, raw.At
	if len(reg.Kinds) == 0 && raw.Kind != "" {
		reg.Kinds = []string{raw.Kind}
	}
	return nil
}

// Store wraps the databox client with the media access methods.
type Store struct {
	DB *client.Client
	// Nodes walks registered folders and resolves nodes for the indexer
	// and the per-user surface (nil breaks only those paths — the
	// registry itself needs no tree access).
	Nodes *nodes.Store
	// Drives resolves the member's drive list — membership IS the
	// subscription (spec §9).
	Drives *drives.Store
}

func registryKey(driveID, folderID string) string {
	return registryPrefix + driveID + "/" + folderID
}

// Register ADDS one kind to a folder's registration set (editor+ on the
// drive, gated by the caller). Registering an already-registered kind
// just refreshes by/at — using one folder for both video and music is
// two Register calls, no unregister in between.
func (s *Store) Register(ctx context.Context, driveID, folderID, kind, by string) error {
	if !ValidKind(kind) {
		return users.ErrNotFound
	}
	if !kvx.ValidID(driveID) || !kvx.ValidID(folderID) {
		return users.ErrNotFound
	}
	reg, _, err := s.Get(ctx, driveID, folderID)
	if err != nil {
		return err
	}
	if !reg.Has(kind) {
		reg.Kinds = append(reg.Kinds, kind)
	}
	reg.By, reg.At = strings.ToLower(by), time.Now().UTC()
	return kvx.SetJSON(ctx, s.DB, registryKey(driveID, folderID), reg)
}

// Unregister removes ONE kind from a folder's registration (kind "" =
// every kind), pruning the removed kind's derived catalog + harvested
// art (a cache with no registration is a ghost) and deleting the row
// only when the set empties. Idempotent; the files are untouched.
func (s *Store) Unregister(ctx context.Context, driveID, folderID, kind string) error {
	if !kvx.ValidID(driveID) || !kvx.ValidID(folderID) {
		return nil
	}
	if kind == "" {
		if err := s.dropCatalog(ctx, driveID, folderID); err != nil {
			return err
		}
		return s.DB.Delete(ctx, registryKey(driveID, folderID))
	}
	if !ValidKind(kind) {
		return users.ErrNotFound
	}
	if err := s.dropCatalogKind(ctx, driveID, folderID, kind); err != nil {
		return err
	}
	reg, found, err := s.Get(ctx, driveID, folderID)
	if err != nil || !found {
		return err
	}
	kept := reg.Kinds[:0]
	for _, k := range reg.Kinds {
		if k != kind {
			kept = append(kept, k)
		}
	}
	reg.Kinds = kept
	if len(reg.Kinds) == 0 {
		if err := s.dropCatalog(ctx, driveID, folderID); err != nil {
			return err
		}
		return s.DB.Delete(ctx, registryKey(driveID, folderID))
	}
	return kvx.SetJSON(ctx, s.DB, registryKey(driveID, folderID), reg)
}

// Get loads one folder's registration.
func (s *Store) Get(ctx context.Context, driveID, folderID string) (Registration, bool, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(folderID) {
		return Registration{}, false, nil
	}
	var reg Registration
	found, err := kvx.GetJSON(ctx, s.DB, registryKey(driveID, folderID), &reg)
	return reg, found, err
}

// RegisteredRow is one registration resolved to its folder id.
type RegisteredRow struct {
	FolderID string
	Registration
}

// ListRegistered returns a drive's registered folders, newest first —
// the Drive UI's badge source and phase 6's catalog roots. One prefix
// List.
func (s *Store) ListRegistered(ctx context.Context, driveID string) ([]RegisteredRow, error) {
	if !kvx.ValidID(driveID) {
		return nil, nil
	}
	var out []RegisteredRow
	err := kvx.ScanPrefix(ctx, s.DB, registryPrefix+driveID+"/", func(key string, value []byte) error {
		var reg Registration
		if json.Unmarshal(value, &reg) != nil {
			return nil
		}
		out = append(out, RegisteredRow{FolderID: key[strings.LastIndex(key, "/")+1:], Registration: reg})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

// SiteRegistration is one registered folder with its drive (the health
// worker's "are there registrations at all" question).
type SiteRegistration struct {
	DriveID  string
	FolderID string
	Registration
}

// AllRegistrations walks every registered folder on the site (health
// worker; the set stays small — folders someone deliberately marked,
// not files).
func (s *Store) AllRegistrations(ctx context.Context) ([]SiteRegistration, error) {
	var out []SiteRegistration
	err := kvx.ScanPrefix(ctx, s.DB, registryPrefix, func(key string, value []byte) error {
		rest := strings.TrimPrefix(key, registryPrefix)
		driveID, folderID, ok := strings.Cut(rest, "/")
		if !ok {
			return nil
		}
		var reg Registration
		if json.Unmarshal(value, &reg) != nil {
			return nil
		}
		out = append(out, SiteRegistration{DriveID: driveID, FolderID: folderID, Registration: reg})
		return nil
	})
	return out, err
}

// PurgeDrive removes every registration, catalog, and harvested art
// blob for a dying drive (part of the drive-deletion composition).
// Per-user rows (progress, lists) resolve LIVE and simply stop
// resolving — no sweep needed.
func (s *Store) PurgeDrive(ctx context.Context, driveID string) error {
	if !kvx.ValidID(driveID) {
		return nil
	}
	if err := kvx.DeletePrefix(ctx, s.DB, catalogPrefix+driveID+"/"); err != nil {
		return err
	}
	if err := s.dropArt(ctx, driveID, ""); err != nil {
		return err
	}
	return kvx.DeletePrefix(ctx, s.DB, registryPrefix+driveID+"/")
}
