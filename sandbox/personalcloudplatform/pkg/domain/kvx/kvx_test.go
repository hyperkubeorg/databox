package kvx

import (
	"strings"
	"testing"
	"time"
)

// Newer entries must sort strictly BEFORE older ones — that inversion is
// the whole point of InvID (newest-first prefix Lists).
func TestInvIDNewestFirst(t *testing.T) {
	t0 := time.Now()
	older := InvIDAt(t0)
	newer := InvIDAt(t0.Add(time.Second))
	if !(newer < older) {
		t.Fatalf("newer id %q must sort before older id %q", newer, older)
	}
}

func TestInvIDShape(t *testing.T) {
	id := InvID()
	parts := strings.SplitN(id, "-", 2)
	if len(parts) != 2 || len(parts[0]) != 20 {
		t.Fatalf("id %q must be a 20-digit inverted timestamp plus suffix", id)
	}
	for _, r := range parts[0] {
		if r < '0' || r > '9' {
			t.Fatalf("timestamp part of %q must be digits", id)
		}
	}
}

// Every entry OLDER than the cursor's time must sort AFTER the cursor,
// so retention's single DeleteRange starting at the cursor is exact.
func TestInvCursorBoundsOlderEntries(t *testing.T) {
	cut := time.Now()
	cursor := InvCursor(cut)
	older := InvIDAt(cut.Add(-time.Hour))
	newer := InvIDAt(cut.Add(time.Hour))
	if !(older > cursor) {
		t.Errorf("older entry %q must sort after cursor %q", older, cursor)
	}
	if !(newer < cursor) {
		t.Errorf("newer entry %q must sort before cursor %q", newer, cursor)
	}
}

func TestNewIDIsValid(t *testing.T) {
	id := NewID()
	if !ValidID(id) {
		t.Fatalf("NewID produced %q which ValidID rejects", id)
	}
}

func TestValidID(t *testing.T) {
	for _, bad := range []string{"", "short", "has/slash-longer", "has.dot-longer", strings.Repeat("a", 65)} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true, want false", bad)
		}
	}
	if !ValidID("abc_DEF-123x") {
		t.Errorf("ValidID rejected a token-shaped id")
	}
}

func TestValidKeyName(t *testing.T) {
	for _, ok := range []string{"sam", "a-b-c", "user123", strings.Repeat("z", 32)} {
		if err := ValidKeyName(ok, "username"); err != nil {
			t.Errorf("ValidKeyName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "ab", "UPPER", "with space", "dot.dot", "sl/ash", strings.Repeat("z", 33)} {
		if err := ValidKeyName(bad, "username"); err == nil {
			t.Errorf("ValidKeyName(%q) = nil, want error", bad)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"Photo.JPG", "budget 2026 (final)", "résumé", "a"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "a\x00b", "   ", "ctl\x01", strings.Repeat("a", 256)} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) = nil, want error", bad)
		}
	}
}

func TestPrefixEnd(t *testing.T) {
	cases := map[string]string{
		"/pcp/audit/": "/pcp/audit0",
		"abc":         "abd",
		"a\xff":       "b",
		"\xff":        "",
	}
	for in, want := range cases {
		if got := PrefixEnd(in); got != want {
			t.Errorf("PrefixEnd(%q) = %q, want %q", in, got, want)
		}
	}
}

// TSKey must sort CHRONOLOGICALLY (oldest first — the opposite of
// InvID), stay fixed-width, and be deterministic so an idempotent
// segment re-send lands on the same key (Draft 005 §6.2).
func TestTSKeyChronological(t *testing.T) {
	t0 := time.Now()
	older := TSKey(t0)
	newer := TSKey(t0.Add(time.Second))
	if !(older < newer) {
		t.Fatalf("older key %q must sort before newer key %q", older, newer)
	}
	if len(older) != 13 || len(newer) != 13 {
		t.Fatalf("TSKey must be fixed 13-digit width, got %q / %q", older, newer)
	}
	if TSKey(t0) != TSKey(t0) {
		t.Fatal("TSKey must be deterministic")
	}
	if TSKeyMs(-5) != TSKeyMs(0) {
		t.Fatal("negative stamps must clamp to zero, never shorten the key")
	}
}
