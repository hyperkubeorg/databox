// kvx.go — the shared storage helpers (see doc.go for the key table).
package kvx

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/client"
)

// NewID mints a random, key-safe identifier (drives, nodes, blobs,
// uploads, labels, …). RandomToken's alphabet (A-Za-z0-9_-) contains no
// separator, so an id can never traverse out of its key position.
func NewID() string { return auth.RandomToken(12) }

// InvID mints a key-safe id that sorts NEWEST FIRST: the timestamp is
// inverted (math.MaxInt64 - nanos) and zero-padded to fixed width, so a
// plain ascending prefix List returns entries newest-first with no
// ORDER BY anywhere. The random suffix breaks same-nanosecond ties.
func InvID() string { return InvIDAt(time.Now()) }

// InvIDAt is InvID with an injectable clock (thread indexes re-file at a
// known activity time; tests pin ordering).
func InvIDAt(t time.Time) string {
	return fmt.Sprintf("%020d-%s", math.MaxInt64-t.UnixNano(), auth.RandomToken(3))
}

// InvCursor is the id prefix every entry OLDER than t sorts after —
// retention turns "delete older than the cutoff" into one DeleteRange
// starting here.
func InvCursor(t time.Time) string {
	return fmt.Sprintf("%020d", math.MaxInt64-t.UnixNano())
}

// TSKey formats a time as a fixed-width, zero-padded unix-milliseconds
// key segment that sorts CHRONOLOGICALLY (oldest first) — the smart-home
// segment/thumbnail families range-scan by time window, the opposite
// ordering of InvID (Draft 005 §6.2). Deterministic — no random suffix:
// segment ingest is idempotent on (camera, start), so a re-send must
// land on the same key. 13 digits covers past year 2200.
func TSKey(t time.Time) string { return TSKeyMs(t.UnixMilli()) }

// TSKeyMs is TSKey over a raw millisecond stamp (ingest payloads carry
// ms on the wire). Negative stamps clamp to zero so a bad payload can
// never shorten the key and escape its fixed-width ordering.
func TSKeyMs(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%013d", ms)
}

// ValidKeyName is the shared rule for user-chosen key segments
// (usernames, mailbox local parts): 3–32 chars of a-z, 0-9, dashes.
// Anything else must never become part of a storage key.
func ValidKeyName(name, what string) error {
	if len(name) < 3 || len(name) > 32 {
		return fmt.Errorf("%s must be 3–32 characters", what)
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return fmt.Errorf("%s may contain only a-z, 0-9, and dashes", what)
		}
	}
	return nil
}

// ValidID accepts only what NewID (or auth.RandomToken generally) could
// have produced. Ids arrive in URLs — attacker-controlled — and become
// key segments, so anything else must never reach the store.
func ValidID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	return ValidTokenChars(id)
}

// ValidTokenChars reports whether s is entirely RandomToken alphabet
// (URL-safe base64: A-Za-z0-9_-).
func ValidTokenChars(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// ValidName gates display names typed by users that become key segments
// under a folder-style prefix. The ONLY structural rules are: no
// separator, not a dot-walk, printable, bounded. Everything else —
// spaces, unicode, punctuation — is legitimate and allowed.
func ValidName(name string) error {
	if name == "" {
		return fmt.Errorf("name can't be empty")
	}
	if len(name) > 255 {
		return fmt.Errorf("name is capped at 255 bytes")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("that name is reserved")
	}
	if strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("names can't contain slashes")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name can't be only spaces")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("names can't contain control characters")
		}
	}
	return nil
}

// --- OCC helpers ------------------------------------------------------------

// IsConflict recognizes a databox OCC commit conflict from the client's
// error text (the client wraps the HTTP 409 into an error string).
// Racing writers retry on it; one wins.
func IsConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Conflict")
}

// PrefixEnd returns the exclusive upper bound for a prefix range — the
// prefix with its last byte incremented — matching DeleteRange's
// [start, end) contract.
func PrefixEnd(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return "" // unbounded
}

// --- JSON plumbing ----------------------------------------------------------

// GetJSON loads and decodes one record.
func GetJSON(ctx context.Context, db *client.Client, key string, v any) (bool, error) {
	e, found, err := db.Get(ctx, key)
	if err != nil || !found {
		return false, err
	}
	if err := json.Unmarshal(e.Value, v); err != nil {
		return false, fmt.Errorf("decode %s: %w", key, err)
	}
	return true, nil
}

// SetJSON encodes and stores one record.
func SetJSON(ctx context.Context, db *client.Client, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = db.Set(ctx, key, raw)
	return err
}

// ScanPrefix pages through EVERY key under prefix, calling fn per entry.
// Use only for collections that stay small (users, one folder, a member
// list); unbounded data must page explicitly.
func ScanPrefix(ctx context.Context, db *client.Client, prefix string, fn func(key string, value []byte) error) error {
	cursor := ""
	for {
		entries, next, err := db.List(ctx, prefix, cursor, 500)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := fn(e.Key, e.Value); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// DeletePrefix removes every key under prefix in one DeleteRange.
func DeletePrefix(ctx context.Context, db *client.Client, prefix string) error {
	return db.DeleteRange(ctx, prefix, PrefixEnd(prefix))
}
