// auth_test.go verifies the two security-critical behaviors of pkg/auth:
// the 512-bit password hashing contract (§7.1) and the grant resolution
// algorithm — including the exact worked example from §7.2.
package auth

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestHashPassword512Bits proves the derived key honors the project's
// hard requirement: no password hash shorter than 512 bits.
func TestHashPassword512Bits(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		t.Fatalf("unexpected hash format: %s", encoded)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 64 {
		t.Fatalf("derived key is %d bytes; the project mandates 64 (512 bits)", len(key))
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		t.Fatal(err)
	}
	if len(salt) != 32 {
		t.Fatalf("salt is %d bytes, want 32", len(salt))
	}
}

// TestVerifyPassword covers accept/reject and salt uniqueness.
func TestVerifyPassword(t *testing.T) {
	h1, _ := HashPassword("secret")
	h2, _ := HashPassword("secret")
	if h1 == h2 {
		t.Fatal("two hashes of the same password are identical — salt is not random")
	}
	if !VerifyPassword("secret", h1) {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword("wrong", h1) {
		t.Fatal("wrong password accepted")
	}
	if VerifyPassword("secret", "$argon2id$garbage") {
		t.Fatal("malformed hash accepted")
	}
}

// TestGrantsSpecExample replays the worked example from §7.2
// verbatim: deny * on /, allow list,read,write on /home/sam.
func TestGrantsSpecExample(t *testing.T) {
	grants := []Grant{
		{Prefix: "/", Effect: "deny", Verbs: AllVerbs},
		{Prefix: "/home/sam", Effect: "allow", Verbs: []Verb{VerbList, VerbRead, VerbWrite}},
	}
	cases := []struct {
		key  string
		verb Verb
		want bool
	}{
		// "GET /home/sam/notes/today → allowed (longest match applies recursively)"
		{"/home/sam/notes/today", VerbRead, true},
		// "LIST /home/other → denied (only / matches; it denies)"
		{"/home/other", VerbList, false},
		// "DELETE /home/sam/notes → denied (grant doesn't mention delete;
		//  fall through to / which denies)"
		{"/home/sam/notes", VerbDelete, false},
		{"/home/sam", VerbWrite, true},
		{"/home/sam", VerbAdmin, false},
	}
	for _, c := range cases {
		if got := Allowed(grants, c.key, c.verb); got != c.want {
			t.Errorf("Allowed(%q, %s) = %v, want %v", c.key, c.verb, got, c.want)
		}
	}
}

// TestGrantsDenyWinsTie verifies the equal-length tiebreak: deny beats
// allow at the same prefix (§7.2 rule 5).
func TestGrantsDenyWinsTie(t *testing.T) {
	grants := []Grant{
		{Prefix: "/data", Effect: "allow", Verbs: []Verb{VerbRead}},
		{Prefix: "/data", Effect: "deny", Verbs: []Verb{VerbRead}},
	}
	if Allowed(grants, "/data/x", VerbRead) {
		t.Fatal("deny should win a same-length tie")
	}
}

// TestGrantsDefaultDeny: with no grants at all, everything is denied.
func TestGrantsDefaultDeny(t *testing.T) {
	if Allowed(nil, "/anything", VerbRead) {
		t.Fatal("default must be deny")
	}
}

// TestGrantsFallThrough: the longest matching grant that does not mention
// the verb is skipped in favor of a shorter one that does.
func TestGrantsFallThrough(t *testing.T) {
	grants := []Grant{
		{Prefix: "/a", Effect: "allow", Verbs: []Verb{VerbRead, VerbWrite}},
		{Prefix: "/a/b", Effect: "deny", Verbs: []Verb{VerbDelete}},
	}
	// /a/b/c + read: /a/b doesn't mention read → falls to /a which allows.
	if !Allowed(grants, "/a/b/c", VerbRead) {
		t.Fatal("fall-through to shorter prefix failed")
	}
	// /a/b/c + delete: /a/b denies delete.
	if Allowed(grants, "/a/b/c", VerbDelete) {
		t.Fatal("longest-match deny ignored")
	}
}
