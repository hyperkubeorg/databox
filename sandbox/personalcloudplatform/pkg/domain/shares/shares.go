// Package shares owns sharing, both kinds, plus the single access
// resolver every drive surface consults (access.go). Ported from PCD
// onto the /pcp/ keyspace (kvx key table):
//
//	/pcp/shares/<token>                  → Share (public link)
//	/pcp/sharesess/<id>                  → ShareSession (password pass, lazy TTL)
//	/pcp/nodeshares/<drive>/<node>/<tok> → {} (reverse index of a node's links)
//	/pcp/grants/<user>/<drive>/<node>    → Grant ("shared with me")
//	/pcp/nodegrants/<drive>/<node>/<user>→ Grant (reverse index / node ACL)
//
// This package sits at the TOP of the drive domain stack — it imports
// nodes (ancestor walks) and drives (membership, roles) — so it also
// hosts the composed operations that must touch several domains' keys
// (DeleteNode, PurgeDriveSharing).
package shares

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Key prefixes this package owns (kvx key table).
const (
	sharesPrefix     = "/pcp/shares/"
	shareSessPrefix  = "/pcp/sharesess/"
	nodeSharesPrefix = "/pcp/nodeshares/"
	grantsPrefix     = "/pcp/grants/"
	nodeGrantsPrefix = "/pcp/nodegrants/"
)

// Share permissions: what the link's holder may do.
const (
	// PermView renders the file/folder read-only; media streams inline.
	PermView = "view"
	// PermDownload also serves raw bytes / zip.
	PermDownload = "download"
)

// Store wraps the databox client with the sharing access methods, plus
// the domain stores the composed operations consult.
type Store struct {
	DB     *client.Client
	Nodes  *nodes.Store
	Drives *drives.Store
	Users  *users.Store
}

// Share is one public link.
type Share struct {
	Token     string    `json:"token"`
	DriveID   string    `json:"drive_id"`
	NodeID    string    `json:"node_id"`
	Perms     string    `json:"perms"` // PermView | PermDownload
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// PwHash gates the link behind a password (argon2id, like logins).
	PwHash string    `json:"pw_hash,omitempty"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

// Expired reports whether the link's optional expiry has passed.
func (sh Share) Expired(now time.Time) bool {
	return !sh.ExpiresAt.IsZero() && now.After(sh.ExpiresAt)
}

func shareKey(token string) string { return sharesPrefix + token }
func nodeShareKey(driveID, nodeID, token string) string {
	return nodeSharesPrefix + driveID + "/" + nodeID + "/" + token
}

// CreateShare mints a public link for a node. The caller gates on the
// creating user's access (editor+). password "" means open link.
func (s *Store) CreateShare(ctx context.Context, driveID, nodeID, perms, password string, expiresAt time.Time, by string) (Share, error) {
	if !kvx.ValidID(driveID) || !nodes.ValidNodeID(nodeID) || nodeID == nodes.RootID {
		return Share{}, users.ErrNotFound
	}
	if perms != PermView && perms != PermDownload {
		return Share{}, users.ErrNotFound
	}
	sh := Share{
		Token: auth.RandomToken(18), DriveID: driveID, NodeID: nodeID,
		Perms: perms, ExpiresAt: expiresAt, By: strings.ToLower(by), At: time.Now().UTC(),
	}
	if password != "" {
		hash, err := auth.HashPassword(password)
		if err != nil {
			return Share{}, err
		}
		sh.PwHash = hash
	}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, _ := json.Marshal(sh)
		tx.Set(shareKey(sh.Token), raw)
		tx.Set(nodeShareKey(driveID, nodeID, sh.Token), []byte("{}"))
		return nil
	})
	if err != nil {
		return Share{}, err
	}
	return sh, nil
}

// GetShare resolves a link token. A token that isn't token-shaped is a
// plain miss — attacker-typed URLs never become keys.
func (s *Store) GetShare(ctx context.Context, token string) (Share, bool, error) {
	if len(token) < 16 || len(token) > 64 || !kvx.ValidTokenChars(token) {
		return Share{}, false, nil
	}
	var sh Share
	found, err := kvx.GetJSON(ctx, s.DB, shareKey(token), &sh)
	return sh, found, err
}

// CheckSharePassword verifies a password-protected link's password.
func CheckSharePassword(sh Share, password string) bool {
	if sh.PwHash == "" {
		return true
	}
	return auth.VerifyPassword(password, sh.PwHash)
}

// NodeShares lists a node's live links, newest first (the "manage
// sharing" panel).
func (s *Store) NodeShares(ctx context.Context, driveID, nodeID string) ([]Share, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return nil, nil
	}
	var out []Share
	err := kvx.ScanPrefix(ctx, s.DB, nodeSharesPrefix+driveID+"/"+nodeID+"/", func(key string, _ []byte) error {
		token := key[strings.LastIndex(key, "/")+1:]
		sh, found, err := s.GetShare(ctx, token)
		if err != nil {
			return err
		}
		if found {
			out = append(out, sh)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

// RevokeShare deletes a link and its index row. Idempotent.
func (s *Store) RevokeShare(ctx context.Context, token string) error {
	sh, found, err := s.GetShare(ctx, token)
	if err != nil || !found {
		return err
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		tx.Delete(shareKey(token))
		tx.Delete(nodeShareKey(sh.DriveID, sh.NodeID, token))
		return nil
	})
}

// ShareSession records a successful password entry for a protected link
// — the browser holds its random id in a cookie, so the password is
// typed once, not per request. Lazy TTL like login sessions.
type ShareSession struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CreateShareSession mints a one-hour pass for a protected link.
func (s *Store) CreateShareSession(ctx context.Context, token string) (string, error) {
	id := auth.RandomToken(18)
	err := kvx.SetJSON(ctx, s.DB, shareSessPrefix+id, ShareSession{Token: token, ExpiresAt: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		return "", err
	}
	return id, nil
}

// CheckShareSession reports whether a pass id unlocks the given link.
func (s *Store) CheckShareSession(ctx context.Context, id, token string) bool {
	if len(id) < 16 || len(id) > 64 || !kvx.ValidTokenChars(id) {
		return false
	}
	var ss ShareSession
	found, err := kvx.GetJSON(ctx, s.DB, shareSessPrefix+id, &ss)
	if err != nil || !found || ss.Token != token {
		return false
	}
	if time.Now().After(ss.ExpiresAt) {
		_ = s.DB.Delete(ctx, shareSessPrefix+id)
		return false
	}
	return true
}

// RemoveNodeSharing revokes every link and grant on one node (the purge
// sweep DeleteNode runs per purged node).
func (s *Store) RemoveNodeSharing(ctx context.Context, driveID, nodeID string) error {
	shares, err := s.NodeShares(ctx, driveID, nodeID)
	if err != nil {
		return err
	}
	for _, sh := range shares {
		if err := s.RevokeShare(ctx, sh.Token); err != nil {
			return err
		}
	}
	if err := kvx.DeletePrefix(ctx, s.DB, nodeSharesPrefix+driveID+"/"+nodeID+"/"); err != nil {
		return err
	}
	grants, err := s.NodeGrants(ctx, driveID, nodeID)
	if err != nil {
		return err
	}
	for _, g := range grants {
		if err := s.RemoveGrant(ctx, driveID, nodeID, g.Username); err != nil {
			return err
		}
	}
	return nil
}

// DeleteNode is THE permanent-delete entry point (UI and API): it runs
// nodes.DeleteForever with this package's sharing sweep hooked to every
// purged node, so links and grants die with their subjects. Returns the
// charged bytes freed (already refunded by the nodes domain).
func (s *Store) DeleteNode(ctx context.Context, driveID, nodeID string) (int64, error) {
	return s.Nodes.DeleteForever(ctx, driveID, nodeID, func(purged string) {
		// Best-effort: an orphaned link resolves to a vanished node and
		// 404s; grants lazily drop in ListSharedWithMe.
		_ = s.RemoveNodeSharing(ctx, driveID, purged)
	})
}

// PurgeDriveSharing sweeps every share and grant row referencing a dying
// drive — part of the drive-deletion composition.
func (s *Store) PurgeDriveSharing(ctx context.Context, driveID string) error {
	if !kvx.ValidID(driveID) {
		return users.ErrNotFound
	}
	// Link tokens live outside the drive prefix; enumerate via the
	// reverse index before sweeping it.
	err := kvx.ScanPrefix(ctx, s.DB, nodeSharesPrefix+driveID+"/", func(key string, _ []byte) error {
		return s.DB.Delete(ctx, shareKey(key[strings.LastIndex(key, "/")+1:]))
	})
	if err != nil {
		return err
	}
	if err := kvx.DeletePrefix(ctx, s.DB, nodeSharesPrefix+driveID+"/"); err != nil {
		return err
	}
	// Grants: user-side rows carry the drive in their MIDDLE segment, so
	// enumerate the drive-side index and delete both directions.
	err = kvx.ScanPrefix(ctx, s.DB, nodeGrantsPrefix+driveID+"/", func(key string, _ []byte) error {
		rest := strings.TrimPrefix(key, nodeGrantsPrefix+driveID+"/")
		nodeID, username, ok := strings.Cut(rest, "/")
		if !ok {
			return nil
		}
		return s.DB.Delete(ctx, grantKey(username, driveID, nodeID))
	})
	if err != nil {
		return err
	}
	return kvx.DeletePrefix(ctx, s.DB, nodeGrantsPrefix+driveID+"/")
}
