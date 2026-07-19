package smarthome

import (
	"strings"
	"testing"
)

// TestRoleLadder pins the owner > operator > viewer order and the role
// vocabulary (Draft 005 §3.2) — the matrix every permission check
// resolves through.
func TestRoleLadder(t *testing.T) {
	cases := []struct {
		role, min string
		want      bool
	}{
		{RoleOwner, RoleOwner, true},
		{RoleOwner, RoleOperator, true},
		{RoleOwner, RoleViewer, true},
		{RoleOperator, RoleOwner, false},
		{RoleOperator, RoleOperator, true},
		{RoleOperator, RoleViewer, true},
		{RoleViewer, RoleOwner, false},
		{RoleViewer, RoleOperator, false},
		{RoleViewer, RoleViewer, true},
		{"", RoleViewer, false},
		{"admin", RoleViewer, false}, // there is no admin tier (§3.2)
	}
	for _, c := range cases {
		if got := RoleAtLeast(c.role, c.min); got != c.want {
			t.Errorf("RoleAtLeast(%q, %q) = %v, want %v", c.role, c.min, got, c.want)
		}
	}
}

// TestValidRoles pins which names are roles at all, and which may be
// GRANTED (owner is established at create, never granted).
func TestValidRoles(t *testing.T) {
	for _, role := range []string{RoleOwner, RoleOperator, RoleViewer} {
		if !ValidRole(role) {
			t.Errorf("ValidRole(%q) = false", role)
		}
	}
	for _, role := range []string{"", "admin", "editor", "Owner"} {
		if ValidRole(role) {
			t.Errorf("ValidRole(%q) = true", role)
		}
	}
	if !ValidMemberRole(RoleOperator) || !ValidMemberRole(RoleViewer) {
		t.Error("operator and viewer must be grantable")
	}
	if ValidMemberRole(RoleOwner) {
		t.Error("owner must not be grantable")
	}
}

// TestValidAccessSubject pins the allowlist subject shape: u:<name>,
// single segment, nothing else (§3.1 — no org/team/repo vocabulary).
func TestValidAccessSubject(t *testing.T) {
	for _, s := range []string{"u:sam", "u:a", "u:some-user"} {
		if !ValidAccessSubject(s) {
			t.Errorf("ValidAccessSubject(%q) = false", s)
		}
	}
	for _, s := range []string{"", "u:", "sam", "o:org", "t:org/team", "r:repo", "u:a/b", "u:a\x00b"} {
		if ValidAccessSubject(s) {
			t.Errorf("ValidAccessSubject(%q) = true", s)
		}
	}
}

// TestRetentionDefault pins the one-number retention default (§6.3).
func TestRetentionDefault(t *testing.T) {
	if (Space{}).Retention() != DefaultRetentionDays {
		t.Errorf("zero-value space retention = %d, want %d", (Space{}).Retention(), DefaultRetentionDays)
	}
	if (Space{RetentionDays: 30}).Retention() != 30 {
		t.Error("configured retention must win")
	}
}

// TestParseAgentToken pins the agent-token shape (§4.1): anything not
// shaped like CompletePairing's output must be rejected before it can
// reach the store.
func TestParseAgentToken(t *testing.T) {
	id, secret, ok := ParseAgentToken("pcpcam_AAAABBBBCCCCDDDD_" + strings.Repeat("s", 43))
	if !ok || id != "AAAABBBBCCCCDDDD" || len(secret) != 43 {
		t.Fatalf("well-formed token refused (ok=%v id=%q)", ok, id)
	}
	for _, bad := range []string{
		"", "pcp_AAAABBBBCCCCDDDD_" + strings.Repeat("s", 43), // user API key prefix
		"pcpcam_short_" + strings.Repeat("s", 43),
		"pcpcam_AAAABBBBCCCCDDDD_tiny",
		"pcpcam_AAAABBBBCCCCDDDD_" + strings.Repeat("/", 43),
	} {
		if _, _, ok := ParseAgentToken(bad); ok {
			t.Errorf("ParseAgentToken(%q) accepted", bad)
		}
	}
}
