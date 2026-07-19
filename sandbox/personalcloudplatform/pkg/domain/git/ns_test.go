// ns_test.go — the shared namespace (§3.1): org claims, reserved names,
// and the signup-side collision check.
package git

import (
	"context"
	"errors"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

func TestClaimAndAvailability(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")

	// An existing username can't become an org.
	if _, err := s.CreateOrg(ctx, "ada", "ada", ""); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("org over a username must be ErrNameTaken, got %v", err)
	}

	// A fresh name claims: ns registry entry, org record, creator owner.
	org, err := s.CreateOrg(ctx, "Acme", "ada", "the household org")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if org.Name != "acme" {
		t.Errorf("org names must lowercase, got %q", org.Name)
	}
	ns, found, err := s.GetNS(ctx, "acme")
	if err != nil || !found || ns.Kind != NSKindOrg {
		t.Fatalf("ns registry entry missing/wrong: found=%v kind=%q err=%v", found, ns.Kind, err)
	}
	if m, found, _ := s.GetMember(ctx, "acme", "ada"); !found || m.Role != OrgRoleOwner {
		t.Fatalf("creator must be owner, got found=%v role=%q", found, m.Role)
	}

	// Second claim of the same name loses.
	if _, err := s.CreateOrg(ctx, "acme", "ada", ""); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate org claim must be ErrNameTaken, got %v", err)
	}

	// Availability mirrors all three refusals.
	if err := s.Available(ctx, "acme"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("Available(org) = %v", err)
	}
	if err := s.Available(ctx, "ada"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("Available(user) = %v", err)
	}
	if err := s.Available(ctx, "admin"); err == nil {
		t.Error("Available(reserved) must refuse")
	}
	if err := s.Available(ctx, "fresh-name"); err != nil {
		t.Errorf("Available(fresh) = %v", err)
	}
}

func TestReservedNamesRejected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	// §3.1: git, api, admin, orgs, settings, new + every route prefix.
	for _, name := range []string{
		"git", "api", "admin", "orgs", "settings", "new",
		"login", "logout", "signup", "static", "healthz", "launcher",
		"drive", "mail", "calendar", "contacts", "video", "music",
		"messenger", "notifications", "invites", "impersonate",
	} {
		if !IsReservedName(name) {
			t.Errorf("%q must be reserved", name)
		}
		if _, err := s.CreateOrg(ctx, name, "ada", ""); err == nil {
			t.Errorf("org claim of reserved %q must refuse", name)
		}
	}
}

// TestSignupChecksRegistry: with the ReserveName hook wired (as cmd/pcp
// does), signup refuses org names and reserved names — even though Git
// Services being enabled was never consulted (§3.1).
func TestSignupChecksRegistry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	s.Users.ReserveName = s.CheckUsernameInTx

	seedUser(t, s, "ada")
	if _, err := s.CreateOrg(ctx, "acme", "ada", ""); err != nil {
		t.Fatalf("create org: %v", err)
	}

	// An org name can't be shadowed by a later signup.
	if _, err := s.Users.CreateUser(ctx, "acme", "Acme", "password-1"); !errors.Is(err, users.ErrUsernameTaken) {
		t.Fatalf("signup over an org must be ErrUsernameTaken, got %v", err)
	}
	// Reserved names are rejected at signup too.
	if _, err := s.Users.CreateUser(ctx, "admin", "Admin", "password-1"); err == nil {
		t.Fatal("signup of a reserved name must refuse")
	}
	// A fresh name still signs up fine with the hook installed.
	if _, err := s.Users.CreateUser(ctx, "bob", "Bob", "password-1"); err != nil {
		t.Fatalf("fresh signup with hook: %v", err)
	}
}

// TestDeleteOrgReleasesName: deletion releases the namespace so it can
// be claimed again (§3.3).
func TestDeleteOrgReleasesName(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	if _, err := s.CreateOrg(ctx, "acme", "ada", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.DeleteOrg(ctx, "acme"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := s.GetNS(ctx, "acme"); found {
		t.Fatal("ns entry must be released")
	}
	if orgs, _ := s.UserOrgs(ctx, "ada"); len(orgs) != 0 {
		t.Fatalf("reverse membership rows must die with the org, got %v", orgs)
	}
	if _, err := s.CreateOrg(ctx, "acme", "ada", ""); err != nil {
		t.Fatalf("re-claim after delete: %v", err)
	}
}

// TestDeleteOrgBlockedByRepos: the §3.3 zero-repos rule, checked against
// the phase-3 reponame index prefix.
func TestDeleteOrgBlockedByRepos(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	if _, err := s.CreateOrg(ctx, "acme", "ada", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.DB.Set(ctx, repoNamePrefix+"acme/tools", []byte(`"repoid123456"`)); err != nil {
		t.Fatalf("seed repo name row: %v", err)
	}
	if err := s.DeleteOrg(ctx, "acme"); err == nil {
		t.Fatal("delete with repos must refuse")
	}
}
