// uuid.go backs the UUID column type: v4 generation for
// gen_random_uuid() and write-time validation/normalization. UUIDs store
// as TEXT in canonical lowercase 8-4-4-4-12 form, so they key-encode,
// index, and compare like any text — which is what lets a UUID column be
// a primary key.
package sql

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// newUUIDv4 returns a random (version 4, variant 1) UUID in canonical
// lowercase form.
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on supported platforms; a broken entropy
		// source is not something SQL execution can recover from.
		panic("crypto/rand: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return formatUUID(b[:])
}

func formatUUID(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// normalizeUUID validates a UUID's text form — 32 hex digits, with or
// without the canonical hyphens — and returns the canonical lowercase
// spelling. Enforced on every write to a UUID column, so stored values
// are always in one comparable form.
func normalizeUUID(s string) (string, error) {
	hexOnly := strings.ReplaceAll(s, "-", "")
	if len(s) == 36 {
		// Hyphenated form must have them in the canonical positions.
		if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
			return "", fmt.Errorf("invalid UUID %q", s)
		}
	} else if len(s) != 32 {
		return "", fmt.Errorf("invalid UUID %q", s)
	}
	raw, err := hex.DecodeString(strings.ToLower(hexOnly))
	if err != nil || len(raw) != 16 {
		return "", fmt.Errorf("invalid UUID %q", s)
	}
	return formatUUID(raw), nil
}

// coerceUUIDColumn applies UUID validation when the column declared the
// UUID type; other columns pass through. Shared by INSERT and UPDATE.
func coerceUUIDColumn(c column, v Value) (Value, error) {
	if c.TypeName != "UUID" || v.IsNull() {
		return v, nil
	}
	norm, err := normalizeUUID(v.S)
	if err != nil {
		return Value{}, fmt.Errorf("column %q: %v", c.Name, err)
	}
	return textV(norm), nil
}
