// members_test.go — the §3.3 last-owner invariant and the §3.4
// member-removal team cascade.
package git

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// seedOrg builds acme with ada as owner and the named extra members.
func seedOrg(t *testing.T, s *Store, members ...string) {
	t.Helper()
	ctx := context.Background()
	seedUser(t, s, "ada")
	if _, err := s.CreateOrg(ctx, "acme", "ada", ""); err != nil {
		t.Fatalf("create org: %v", err)
	}
	for _, m := range members {
		seedUser(t, s, m)
		if err := s.AddMember(ctx, "acme", m, OrgRoleMember); err != nil {
			t.Fatalf("add member %s: %v", m, err)
		}
	}
}

func TestLastOwnerInvariant(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedOrg(t, s, "bob")

	// The only owner can neither demote nor leave.
	if err := s.SetMemberRole(ctx, "acme", "ada", OrgRoleMember); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demoting the only owner must be ErrLastOwner, got %v", err)
	}
	if err := s.RemoveMember(ctx, "acme", "ada"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("removing the only owner must be ErrLastOwner, got %v", err)
	}

	// With a second owner both operations pass.
	if err := s.SetMemberRole(ctx, "acme", "bob", OrgRoleOwner); err != nil {
		t.Fatalf("promote bob: %v", err)
	}
	if err := s.SetMemberRole(ctx, "acme", "ada", OrgRoleMember); err != nil {
		t.Fatalf("demote ada with two owners: %v", err)
	}
	// …and now bob is the last owner again.
	if err := s.RemoveMember(ctx, "acme", "bob"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("removing the again-last owner must be ErrLastOwner, got %v", err)
	}
	// Removing the demoted member is fine.
	if err := s.RemoveMember(ctx, "acme", "ada"); err != nil {
		t.Fatalf("remove member ada: %v", err)
	}
	if orgs, _ := s.UserOrgs(ctx, "ada"); len(orgs) != 0 {
		t.Fatal("reverse index row must go with the membership")
	}
}

func TestMemberRemovalCascadesTeams(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedOrg(t, s, "bob", "carol")

	team, err := s.CreateTeam(ctx, "acme", "backend", "")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	for _, m := range []string{"bob", "carol"} {
		if err := s.AddTeamMember(ctx, "acme", team.ID, m); err != nil {
			t.Fatalf("seat %s: %v", m, err)
		}
	}
	// The team holds a grant; bob's reverse index carries it.
	repoID := "repoAAAABBBB"
	if err := s.SetGrant(ctx, repoID, TeamSubject("acme", team.ID), "write"); err != nil {
		t.Fatalf("team grant: %v", err)
	}
	if shared, _ := s.SharedWith(ctx, "bob"); !slices.Contains(shared, repoID) {
		t.Fatal("team grant must fan out to bob before removal")
	}

	// Removing bob from the ORG unseats him from the team and withdraws
	// the fanned-out reverse entry — one commit (§3.4).
	if err := s.RemoveMember(ctx, "acme", "bob"); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	got, _, err := s.GetTeam(ctx, "acme", team.ID)
	if err != nil {
		t.Fatalf("get team: %v", err)
	}
	if slices.Contains(got.Members, "bob") {
		t.Error("bob must be unseated from the team")
	}
	if !slices.Contains(got.Members, "carol") {
		t.Error("carol must keep her seat")
	}
	if shared, _ := s.SharedWith(ctx, "bob"); len(shared) != 0 {
		t.Errorf("bob's reverse entries must be withdrawn, got %v", shared)
	}
	if shared, _ := s.SharedWith(ctx, "carol"); !slices.Contains(shared, repoID) {
		t.Error("carol's reverse entry must survive")
	}
}

func TestAddMemberRefusals(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedOrg(t, s, "bob")
	if err := s.AddMember(ctx, "acme", "bob", OrgRoleMember); err == nil {
		t.Error("re-adding a member must refuse")
	}
	if err := s.AddMember(ctx, "acme", "ghost", OrgRoleMember); err == nil {
		t.Error("adding a nonexistent account must refuse")
	}
	if err := s.AddMember(ctx, "nope", "bob", OrgRoleMember); !errors.Is(err, ErrNotFound) {
		t.Errorf("adding to a nonexistent org must be ErrNotFound, got %v", err)
	}
}
