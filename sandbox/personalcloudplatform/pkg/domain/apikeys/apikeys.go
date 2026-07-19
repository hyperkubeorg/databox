// Package apikeys owns the bearer keys the /api/v1 surface accepts
// (spec §12.1). A key's secret is shown exactly once at mint time;
// storage holds only its SHA-256 digest at /pcp/apikeys/<keyID>, with a
// reverse index /pcp/userkeys/<user>/<keyID> for per-user listing —
// both rows written and deleted in one transaction (kvx key table).
//
// Token format: pcp_<keyID>_<base64url secret>. The keyID rides in the
// token so verification is one Get, no scan; the fixed-width keyID makes
// the parse unambiguous even though base64url contains '_'.
package apikeys

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Key prefixes this package owns (kvx key table).
const (
	keysPrefix     = "/pcp/apikeys/"
	userKeysPrefix = "/pcp/userkeys/"
)

// Token mechanics.
const (
	tokenPrefix = "pcp_"
	// keyIDLen is len(kvx.NewID()): 12 random bytes as base64url. The
	// parse is fixed-width because the id itself may contain '_'.
	keyIDLen = 16
	// secretBytes of crypto/rand entropy per secret (43 chars base64url).
	secretBytes = 32
)

// Limits.
const (
	// MaxKeysPerUser caps how many keys one account may hold.
	MaxKeysPerUser = 25
	// maxNameLen caps a key's display name.
	maxNameLen = 60
	// lastUsedThrottle bounds how often a verify refreshes LastUsed.
	lastUsedThrottle = 5 * time.Minute
)

// ErrNotFound is a revoke/list miss (also: not the caller's key).
var ErrNotFound = errors.New("api key not found")

// The canonical scope names (spec §12.1). Routes gate on these; a key is
// always additionally capped by its owner's own access.
const (
	ScopeProfileRead   = "profile:read"
	ScopeDriveRead     = "drive:read"
	ScopeDriveWrite    = "drive:write"
	ScopeMailRead      = "mail:read"
	ScopeMailWrite     = "mail:write"
	ScopeMailSend      = "mail:send"
	ScopeCalendarRead  = "calendar:read"
	ScopeCalendarWrite = "calendar:write"
	ScopeContactsRead  = "contacts:read"
	ScopeContactsWrite = "contacts:write"
	ScopeMediaRead     = "media:read"
	ScopeMediaWrite    = "media:write"
	ScopeMsgRead       = "messenger:read"
	ScopeMsgWrite      = "messenger:write"
	// Git Services scopes (Draft 002 §6.3/§12): shared by the /api/v1
	// endpoints and the git wire protocol — one credential story.
	ScopeGitRead  = "git:read"
	ScopeGitWrite = "git:write"
	// Builds (CI/CD) scopes (Draft 003 §12): the repo Builds/Releases
	// surface's API twin, gated additionally by the git repo role.
	ScopeBuildRead  = "build:read"
	ScopeBuildWrite = "build:write"
	// Smart Home scopes (Draft 005 §11): the phone app's surface, gated
	// additionally by the caller's space role. Agent ingest is NOT here
	// — agents carry their own token class.
	ScopeSmartHomeRead  = "smarthome:read"
	ScopeSmartHomeWrite = "smarthome:write"
)

// Scope pairs a scope name with the plain-language description the
// Settings page shows beside its checkbox.
type Scope struct{ Name, Desc string }

// Scopes is the canonical scope list, in display order. ValidScopes
// normalizes every requested set against it.
var Scopes = []Scope{
	{ScopeProfileRead, "Read your profile — username, display name, storage usage"},
	{ScopeDriveRead, "Read your files and folders, and download file contents"},
	{ScopeDriveWrite, "Create, upload, rename, move, and delete files"},
	{ScopeMailRead, "Read your mail — folders, threads, and messages"},
	{ScopeMailWrite, "Change mail state — flags, labels, moves, and drafts"},
	{ScopeMailSend, "Send mail as you"},
	{ScopeCalendarRead, "Read your calendars and events"},
	{ScopeCalendarWrite, "Create and change calendar events, and RSVP"},
	{ScopeContactsRead, "Read your contacts"},
	{ScopeContactsWrite, "Create and change contacts"},
	{ScopeMediaRead, "Read media catalogs, thumbnails, and stream video/music"},
	{ScopeMediaWrite, "Record playback progress and manage your watchlist, favorites, and playlists"},
	{ScopeMsgRead, "Read your servers, channels, DMs, and messages"},
	{ScopeMsgWrite, "Send messages, manage membership, and set your status"},
	{ScopeGitRead, "Clone and fetch git repositories, and read repos, issues, and merge requests"},
	{ScopeGitWrite, "Push to git repositories, and change repos, issues, and merge requests"},
	{ScopeBuildRead, "Read builds and releases for git repositories"},
	{ScopeBuildWrite, "Trigger, cancel, retry, and delete builds"},
	{ScopeSmartHomeRead, "Watch your cameras — spaces, live view, recordings, events, and clips"},
	{ScopeSmartHomeWrite, "Acknowledge events and save or delete clips in your spaces"},
}

// ValidScope reports whether name is on the canonical list.
func ValidScope(name string) bool {
	for _, s := range Scopes {
		if s.Name == name {
			return true
		}
	}
	return false
}

// ValidScopes validates a requested scope set, deduplicates it, and
// returns it in canonical order. At least one scope is required — a key
// that can do nothing is a mistake, not a feature.
func ValidScopes(requested []string) ([]string, error) {
	want := map[string]bool{}
	for _, s := range requested {
		if !ValidScope(s) {
			return nil, fmt.Errorf("unknown scope %q", s)
		}
		want[s] = true
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("pick at least one scope")
	}
	out := make([]string, 0, len(want))
	for _, s := range Scopes {
		if want[s.Name] {
			out = append(out, s.Name)
		}
	}
	return out, nil
}

// Key is one stored API key — everything but the secret.
type Key struct {
	KeyID  string   `json:"key_id"`
	Owner  string   `json:"owner"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	// Digest is the hex SHA-256 of the secret — the secret itself is
	// never stored anywhere.
	Digest    string    `json:"digest"`
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt zero means the key never expires.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// LastUsed refreshes on verify, throttled (lastUsedThrottle).
	LastUsed time.Time `json:"last_used,omitzero"`
}

// HasScope reports whether the key grants a scope.
func (k Key) HasScope(scope string) bool { return slices.Contains(k.Scopes, scope) }

// Expired reports whether the key's optional expiry has passed.
func (k Key) Expired(now time.Time) bool {
	return !k.ExpiresAt.IsZero() && now.After(k.ExpiresAt)
}

// VerifySecret compares a presented secret against the stored digest.
// Hashing first makes the comparison fixed-length and constant-time, so
// timing reveals nothing about either value.
func (k Key) VerifySecret(secret string) bool {
	sum := sha256.Sum256([]byte(secret))
	digest, err := hex.DecodeString(k.Digest)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(sum[:], digest) == 1
}

// ParseToken splits a presented bearer token into key id and secret.
// Tokens are attacker-controlled and the id becomes a storage key
// segment, so anything not shaped like Mint's output is rejected here —
// it can't exist and must never reach the store.
func ParseToken(token string) (keyID, secret string, ok bool) {
	rest, found := strings.CutPrefix(token, tokenPrefix)
	if !found || len(rest) < keyIDLen+2 || rest[keyIDLen] != '_' {
		return "", "", false
	}
	keyID, secret = rest[:keyIDLen], rest[keyIDLen+1:]
	if !kvx.ValidID(keyID) || len(secret) < 40 || len(secret) > 64 || !kvx.ValidTokenChars(secret) {
		return "", "", false
	}
	return keyID, secret, true
}

// newKey assembles a record and its one-time token from pre-minted
// randomness — the pure core Mint wraps, kept separate so the token
// format, validation, and digesting are testable without a cluster.
func newKey(owner, name string, scopes []string, expiresAt time.Time, keyID, secret string, now time.Time) (Key, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Key{}, "", fmt.Errorf("the key needs a name")
	}
	if len(name) > maxNameLen {
		return Key{}, "", fmt.Errorf("key names are capped at %d characters", maxNameLen)
	}
	normalized, err := ValidScopes(scopes)
	if err != nil {
		return Key{}, "", err
	}
	if !expiresAt.IsZero() && !expiresAt.After(now) {
		return Key{}, "", fmt.Errorf("expiry must be in the future")
	}
	sum := sha256.Sum256([]byte(secret))
	k := Key{
		KeyID: keyID, Owner: owner, Name: name, Scopes: normalized,
		Digest: hex.EncodeToString(sum[:]), CreatedAt: now.UTC(),
	}
	if !expiresAt.IsZero() {
		k.ExpiresAt = expiresAt.UTC()
	}
	return k, tokenPrefix + keyID + "_" + secret, nil
}

// Store wraps the databox client with the API-key access methods.
type Store struct {
	DB *client.Client
}

// Mint creates a key and returns the full token — the ONLY time it ever
// exists outside the caller's hands — plus the stored record. The key
// row and its reverse-index row commit in one transaction.
func (s *Store) Mint(ctx context.Context, owner, name string, scopes []string, expiresAt time.Time) (string, Key, error) {
	owner = strings.ToLower(owner)
	k, token, err := newKey(owner, name, scopes, expiresAt, kvx.NewID(), auth.RandomToken(secretBytes), time.Now())
	if err != nil {
		return "", Key{}, err
	}
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		// The per-user cap counts the reverse index inside the
		// transaction. tx.List doesn't validate ABSENT keys (phantom
		// inserts don't conflict), so two racing mints can land one over
		// the cap — it's a guard rail against runaway automation, not an
		// invariant.
		entries, _, err := tx.List(ctx, userKeysPrefix+owner+"/", "", MaxKeysPerUser+1)
		if err != nil {
			return err
		}
		if len(entries) >= MaxKeysPerUser {
			return fmt.Errorf("at most %d API keys per account — revoke one first", MaxKeysPerUser)
		}
		raw, _ := json.Marshal(k)
		tx.Set(keysPrefix+k.KeyID, raw)
		tx.Set(userKeysPrefix+owner+"/"+k.KeyID, []byte(k.KeyID))
		return nil
	})
	if err != nil {
		return "", Key{}, err
	}
	return token, k, nil
}

// Verify resolves a presented bearer token: parse, one Get,
// constant-time digest compare, expiry check. ok=false covers every
// rejection identically; err is reserved for storage failures.
func (s *Store) Verify(ctx context.Context, token string) (Key, bool, error) {
	keyID, secret, ok := ParseToken(token)
	if !ok {
		return Key{}, false, nil
	}
	var k Key
	found, err := kvx.GetJSON(ctx, s.DB, keysPrefix+keyID, &k)
	if err != nil || !found {
		return Key{}, false, err
	}
	if !k.VerifySecret(secret) || k.Expired(time.Now()) {
		return Key{}, false, nil
	}
	s.touch(ctx, k)
	return k, true, nil
}

// touch refreshes LastUsed, throttled to once per lastUsedThrottle and
// best-effort — a lost update costs only display freshness. The write is
// a read-modify-write transaction for one reason: a plain Set racing
// Revoke could resurrect a just-deleted key.
func (s *Store) touch(ctx context.Context, k Key) {
	now := time.Now().UTC()
	if now.Sub(k.LastUsed) < lastUsedThrottle {
		return
	}
	_ = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, keysPrefix+k.KeyID)
		if err != nil || !found {
			return err
		}
		var cur Key
		if err := json.Unmarshal(raw, &cur); err != nil {
			return err
		}
		cur.LastUsed = now
		out, _ := json.Marshal(cur)
		tx.Set(keysPrefix+k.KeyID, out)
		return nil
	})
}

// Revoke deletes a key and its reverse-index row in one transaction,
// effective on the next verify. A key that isn't the caller's is a plain
// miss — revoke can't probe other accounts' key ids.
func (s *Store) Revoke(ctx context.Context, owner, keyID string) error {
	owner = strings.ToLower(owner)
	if !kvx.ValidID(keyID) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, keysPrefix+keyID)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var k Key
		if err := json.Unmarshal(raw, &k); err != nil {
			return err
		}
		if k.Owner != owner {
			return ErrNotFound
		}
		tx.Delete(keysPrefix + keyID)
		tx.Delete(userKeysPrefix + owner + "/" + keyID)
		return nil
	})
}

// ListForUser loads every key the member owns, newest first. Digests are
// stripped — they never travel past this package.
func (s *Store) ListForUser(ctx context.Context, owner string) ([]Key, error) {
	owner = strings.ToLower(owner)
	prefix := userKeysPrefix + owner + "/"
	var out []Key
	err := kvx.ScanPrefix(ctx, s.DB, prefix, func(key string, _ []byte) error {
		var k Key
		found, err := kvx.GetJSON(ctx, s.DB, keysPrefix+strings.TrimPrefix(key, prefix), &k)
		if err != nil || !found {
			return err // an index row racing a revoke is a silent skip
		}
		k.Digest = ""
		out = append(out, k)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Key) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}
