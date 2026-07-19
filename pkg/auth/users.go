// users.go defines the stored shapes for users, session tokens, and S3
// access keys, plus the system-keyspace locations they live at.
//
// All identity state lives in the Metadata Raft group (system keys, no "/"
// prefix) alongside the rest of the cluster state — there is no separate
// user database (§7.3):
//
//	users/<name>          → User (JSON)
//	tokens/<token>        → Token (JSON), TTL-checked on every request
//	accesskeys/<keyID>    → AccessKey (JSON), maps S3 key IDs to users
//	audit/<ts>-<n>        → audit trail entries for sensitive operations
//
// The functions here only marshal/unmarshal; reading and writing the
// metadata group is the caller's job (pkg/server wires that up). Keeping
// this package storage-agnostic makes it trivially unit-testable.
package auth

import (
	"encoding/json"
	"strings"
	"time"
)

// System-keyspace prefixes for identity records. These are exported so the
// API layer, backup engine, and `.databox/` virtual view all agree on them.
const (
	KeyPrefixUsers      = "users/"
	KeyPrefixTokens     = "tokens/"
	KeyPrefixAccessKeys = "accesskeys/"
	KeyPrefixAudit      = "audit/"
)

// RootUser is the name of the built-in superuser. Root bypasses all grant
// checks and exists from cluster bootstrap, initially with no password
// unless the operator supplied one (root_password_file / Helm secret).
const RootUser = "root"

// User is the stored record for one identity.
type User struct {
	// Name is the login name; it is also the record's key suffix.
	Name string `json:"name"`

	// PasswordHash is the argon2id-512 encoded hash (see password.go).
	// Empty string means "no password set" — permitted only for root
	// before first configuration, and login with an empty password is
	// only accepted when the stored hash is empty.
	PasswordHash string `json:"password_hash"`

	// Grants is the user's authorization rule set (§7.2). Root's grants
	// are ignored — root always passes.
	Grants []Grant `json:"grants"`

	// CreatedAt records when the user was created, for the GUI and audit.
	CreatedAt time.Time `json:"created_at"`
}

// Token is a server-side session token record. The opaque token string
// itself is the key suffix; possession of the string is the credential.
type Token struct {
	User      string    `json:"user"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Expired reports whether the token is past its lifetime.
func (t Token) Expired(now time.Time) bool { return now.After(t.ExpiresAt) }

// AccessKey is a Databox API key: a machine credential pair bound to a
// user, consumed by gateways (the S3 gateway's SigV4 signing today;
// custom gateways tomorrow). Signature verification requires the server
// to know the secret, so the secret is stored as generated; it is random
// and revocable, never a human-chosen password.
type AccessKey struct {
	KeyID     string    `json:"key_id"`
	Secret    string    `json:"secret"`
	User      string    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	// Scopes optionally narrows the key below the user's grants: when
	// non-empty, the key may only touch keys under these prefixes (the
	// user's grants still apply on top — a key can never exceed its
	// user). Empty means "the full extent of the user's grants".
	Scopes []string `json:"scopes,omitempty"`
}

// InScope reports whether a storage key falls inside the API key's scope.
// Scope applies to EVERY holder of the key, including root's keys — the
// scope is a property of the credential, not the user.
func (k AccessKey) InScope(storageKey string) bool {
	if len(k.Scopes) == 0 {
		return true
	}
	for _, p := range k.Scopes {
		if strings.HasPrefix(storageKey, p) {
			return true
		}
	}
	return false
}

// AuditEntry records a sensitive operation (force-unlock, root recovery,
// user/grant mutations) for the audit trail (§7.3, §9).
type AuditEntry struct {
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`  // who performed the action
	Action string    `json:"action"` // e.g. "force-unlock", "root-password-reset"
	Detail string    `json:"detail"` // free-form context (resource, reason)
}

// Marshal helpers. JSON is the storage encoding for all system records:
// human-readable in the `.databox/` view and stable across versions.

func (u User) Encode() []byte       { b, _ := json.Marshal(u); return b }
func (t Token) Encode() []byte      { b, _ := json.Marshal(t); return b }
func (k AccessKey) Encode() []byte  { b, _ := json.Marshal(k); return b }
func (a AuditEntry) Encode() []byte { b, _ := json.Marshal(a); return b }

// DecodeUser parses a stored user record.
func DecodeUser(b []byte) (User, error) {
	var u User
	err := json.Unmarshal(b, &u)
	return u, err
}

// DecodeToken parses a stored token record.
func DecodeToken(b []byte) (Token, error) {
	var t Token
	err := json.Unmarshal(b, &t)
	return t, err
}

// DecodeAccessKey parses a stored access-key record.
func DecodeAccessKey(b []byte) (AccessKey, error) {
	var k AccessKey
	err := json.Unmarshal(b, &k)
	return k, err
}
