// roles_test.go — the exhaustive RoleFor matrix (§4.3). This test is
// the review gate: any new capability must land here first.
package git

import (
	"context"
	"testing"
)

// TestRoleForMatrix builds one world and asserts every (viewer, repo)
// cell:
//
//	users: ada (personal repos), bob (org owner), carol (org member),
//	       dave (outside collaborator), erin (team member, org member),
//	       zoe (unrelated), "" (anonymous)
//	orgs:  acme — owner bob; members carol, erin; team backend{erin}
//	repos: adapub/adapriv (personal, ada)
//	       orgpub/orgpriv (acme, default perm varies per case)
//	grants: dave u:read on adapriv; team backend write on orgpriv;
//	        carol u:admin on orgpriv (direct beats org default; max wins)
func TestRoleForMatrix(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, u := range []string{"ada", "bob", "carol", "dave", "erin", "zoe"} {
		seedUser(t, s, u)
	}
	if _, err := s.CreateOrg(ctx, "acme", "bob", ""); err != nil {
		t.Fatalf("org: %v", err)
	}
	for _, m := range []string{"carol", "erin"} {
		if err := s.AddMember(ctx, "acme", m, OrgRoleMember); err != nil {
			t.Fatalf("member %s: %v", m, err)
		}
	}
	team, err := s.CreateTeam(ctx, "acme", "backend", "")
	if err != nil {
		t.Fatalf("team: %v", err)
	}
	if err := s.AddTeamMember(ctx, "acme", team.ID, "erin"); err != nil {
		t.Fatalf("seat erin: %v", err)
	}

	repo := func(id, owner, vis string) *Repo {
		return &Repo{ID: id, OwnerNS: owner, Name: id, Visibility: vis, DefaultBranch: "main"}
	}
	adapub := repo("adapub111111", "ada", VisPublic)
	adapriv := repo("adapriv11111", "ada", VisPrivate)
	orgpub := repo("orgpub111111", "acme", VisPublic)
	orgpriv := repo("orgpriv11111", "acme", VisPrivate)

	if err := s.SetGrant(ctx, adapriv.ID, UserSubject("dave"), "read"); err != nil {
		t.Fatalf("grant dave: %v", err)
	}
	if err := s.SetGrant(ctx, orgpriv.ID, TeamSubject("acme", team.ID), "write"); err != nil {
		t.Fatalf("grant team: %v", err)
	}
	if err := s.SetGrant(ctx, orgpriv.ID, UserSubject("carol"), "admin"); err != nil {
		t.Fatalf("grant carol: %v", err)
	}

	setPerm := func(perm string) {
		if err := s.UpdateOrgSettings(ctx, "acme", "", perm, false, false); err != nil {
			t.Fatalf("set org perm %q: %v", perm, err)
		}
	}

	type cell struct {
		name string
		user string
		repo *Repo
		perm string // acme DefaultRepoPerm for this cell
		want Role
	}
	cases := []cell{
		// Rule 1: public → read for everyone, including anonymous.
		{"anon/public-personal", "", adapub, PermNone, RoleRead},
		{"anon/private-personal", "", adapriv, PermNone, RoleNone},
		{"anon/public-org", "", orgpub, PermNone, RoleRead},
		{"anon/private-org", "", orgpriv, PermNone, RoleNone},
		{"unrelated/public", "zoe", adapub, PermNone, RoleRead},
		{"unrelated/private", "zoe", adapriv, PermNone, RoleNone},
		{"unrelated/private-org", "zoe", orgpriv, PermNone, RoleNone},

		// Rule 2: personal owner → admin (public or private).
		{"owner/personal-private", "ada", adapriv, PermNone, RoleAdmin},
		{"owner/personal-public", "ada", adapub, PermNone, RoleAdmin},

		// Rule 3: org owner → admin; member gets the org default.
		{"orgowner/private", "bob", orgpriv, PermNone, RoleAdmin},
		{"orgowner/public", "bob", orgpub, PermNone, RoleAdmin},
		{"member/default-none", "carol", orgpub, PermNone, RoleRead}, // public still reads
		{"member/default-read", "erin", orgpub, PermRead, RoleRead},
		{"member/default-write", "erin", orgpub, PermWrite, RoleWrite},
		// Org membership grants nothing on someone ELSE's personal repo.
		{"member/foreign-personal", "carol", adapriv, PermNone, RoleNone},

		// Rule 4: team grants where the user is a member.
		{"team/write-on-private", "erin", orgpriv, PermNone, RoleWrite},
		{"team/nonmember-sees-nothing", "zoe", orgpriv, PermNone, RoleNone},

		// Rule 5: direct grants — outside collaborators need no org.
		{"direct/read-private", "dave", adapriv, PermNone, RoleRead},
		{"direct/none-elsewhere", "dave", orgpriv, PermNone, RoleNone},

		// Max wins: carol's direct admin beats the org default AND the
		// default-write she'd get as a member.
		{"max/direct-admin-beats-default", "carol", orgpriv, PermWrite, RoleAdmin},
		// Max wins: erin holds default-write and team-write on a public
		// repo — write, not read.
		{"max/write-beats-public-read", "erin", orgpriv, PermWrite, RoleWrite},
	}
	for _, c := range cases {
		setPerm(c.perm)
		got, err := s.RoleFor(ctx, c.user, c.repo)
		if err != nil {
			t.Errorf("%s: RoleFor error %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: RoleFor(%q) = %v, want %v", c.name, c.user, got, c.want)
		}
	}
}

// TestRoleLadder pins the ordering the whole §4 model leans on.
func TestRoleLadder(t *testing.T) {
	if !(RoleNone < RoleRead && RoleRead < RoleWrite && RoleWrite < RoleAdmin) {
		t.Fatal("role ladder ordering broken")
	}
	for in, want := range map[string]Role{"read": RoleRead, "write": RoleWrite, "admin": RoleAdmin} {
		got, err := RoleFromGrant(in)
		if err != nil || got != want {
			t.Errorf("RoleFromGrant(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := RoleFromGrant("none"); err == nil {
		t.Error("grants never carry \"none\"")
	}
}
