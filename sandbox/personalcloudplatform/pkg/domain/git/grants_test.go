// grants_test.go — §4.2 reverse-index consistency: team-grant fan-out
// across create/remove, membership changes, and team deletion.
package git

import (
	"context"
	"slices"
	"testing"
)

const repoX = "repoXXXX1111"
const repoY = "repoYYYY2222"

func TestDirectGrantReverseIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "dave")

	if err := s.SetGrant(ctx, repoX, UserSubject("dave"), "read"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if shared, _ := s.SharedWith(ctx, "dave"); !slices.Contains(shared, repoX) {
		t.Fatal("direct grant must appear in usergrants")
	}
	// A role change must not duplicate the reverse entry.
	if err := s.SetGrant(ctx, repoX, UserSubject("dave"), "admin"); err != nil {
		t.Fatalf("role change: %v", err)
	}
	if shared, _ := s.SharedWith(ctx, "dave"); len(shared) != 1 {
		t.Fatalf("role change must not duplicate entries, got %v", shared)
	}
	rows, _ := s.GrantsForRepo(ctx, repoX)
	if len(rows) != 1 || rows[0].Role != "admin" {
		t.Fatalf("grant row wrong: %+v", rows)
	}
	if err := s.RemoveGrant(ctx, repoX, UserSubject("dave")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if shared, _ := s.SharedWith(ctx, "dave"); len(shared) != 0 {
		t.Fatalf("reverse entry must die with the grant, got %v", shared)
	}
}

func TestTeamGrantFanOut(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedOrg(t, s, "bob", "carol", "erin")
	team, err := s.CreateTeam(ctx, "acme", "backend", "")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	for _, m := range []string{"bob", "carol"} {
		if err := s.AddTeamMember(ctx, "acme", team.ID, m); err != nil {
			t.Fatalf("seat %s: %v", m, err)
		}
	}
	subject := TeamSubject("acme", team.ID)

	// Grant creation fans out to every CURRENT member.
	if err := s.SetGrant(ctx, repoX, subject, "write"); err != nil {
		t.Fatalf("team grant: %v", err)
	}
	for _, m := range []string{"bob", "carol"} {
		if shared, _ := s.SharedWith(ctx, m); !slices.Contains(shared, repoX) {
			t.Errorf("%s must see the team grant", m)
		}
	}
	if shared, _ := s.SharedWith(ctx, "erin"); len(shared) != 0 {
		t.Error("erin isn't on the team and must see nothing")
	}

	// A member added later inherits the team's existing grants.
	if err := s.AddTeamMember(ctx, "acme", team.ID, "erin"); err != nil {
		t.Fatalf("seat erin: %v", err)
	}
	if shared, _ := s.SharedWith(ctx, "erin"); !slices.Contains(shared, repoX) {
		t.Error("a later seat must inherit the team grant")
	}

	// Leaving the team withdraws exactly the team-sourced entries…
	if err := s.SetGrant(ctx, repoX, UserSubject("carol"), "read"); err != nil {
		t.Fatalf("direct grant: %v", err)
	}
	if err := s.RemoveTeamMember(ctx, "acme", team.ID, "carol"); err != nil {
		t.Fatalf("unseat carol: %v", err)
	}
	if shared, _ := s.SharedWith(ctx, "carol"); !slices.Contains(shared, repoX) {
		t.Error("carol's DIRECT grant must survive her leaving the team")
	}

	// Removing the team grant unwinds the remaining members' entries.
	if err := s.RemoveGrant(ctx, repoX, subject); err != nil {
		t.Fatalf("remove team grant: %v", err)
	}
	for _, m := range []string{"bob", "erin"} {
		if shared, _ := s.SharedWith(ctx, m); len(shared) != 0 {
			t.Errorf("%s must lose the team grant, got %v", m, shared)
		}
	}
	if shared, _ := s.SharedWith(ctx, "carol"); !slices.Contains(shared, repoX) {
		t.Error("carol's direct grant must still survive")
	}
}

func TestDeleteTeamUnwindsGrants(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedOrg(t, s, "bob")
	team, err := s.CreateTeam(ctx, "acme", "ops", "")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := s.AddTeamMember(ctx, "acme", team.ID, "bob"); err != nil {
		t.Fatalf("seat: %v", err)
	}
	subject := TeamSubject("acme", team.ID)
	for _, repo := range []string{repoX, repoY} {
		if err := s.SetGrant(ctx, repo, subject, "read"); err != nil {
			t.Fatalf("grant %s: %v", repo, err)
		}
	}
	if err := s.DeleteTeam(ctx, "acme", team.ID); err != nil {
		t.Fatalf("delete team: %v", err)
	}
	if shared, _ := s.SharedWith(ctx, "bob"); len(shared) != 0 {
		t.Errorf("team deletion must withdraw its entries, got %v", shared)
	}
	for _, repo := range []string{repoX, repoY} {
		if rows, _ := s.GrantsForRepo(ctx, repo); len(rows) != 0 {
			t.Errorf("team grants on %s must die with the team, got %+v", repo, rows)
		}
	}
	// Team members must be org members; grants demand a real team.
	if err := s.AddTeamMember(ctx, "acme", team.ID, "bob"); err == nil {
		t.Error("seating on a deleted team must refuse")
	}
	if err := s.SetGrant(ctx, repoX, subject, "read"); err == nil {
		t.Error("granting to a deleted team must refuse")
	}
}

func TestTeamMembersMustBeOrgMembers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedOrg(t, s)
	seedUser(t, s, "zoe") // exists, but not in acme
	team, err := s.CreateTeam(ctx, "acme", "ops", "")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := s.AddTeamMember(ctx, "acme", team.ID, "zoe"); err == nil {
		t.Fatal("seating a non-org-member must refuse (§3.4)")
	}
}
