// profiles_test.go — opt-in profiles (§3.2): absence is meaningful,
// CRUD round-trips, shape gates hold.
package git

import (
	"context"
	"strings"
	"testing"
)

func TestProfileLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")

	// No profile until created — enabling Git Services publishes nothing.
	if _, found, err := s.GetProfile(ctx, "ada"); err != nil || found {
		t.Fatalf("fresh user must have no profile: found=%v err=%v", found, err)
	}

	p := Profile{DisplayName: "Ada", Bio: "hi", Public: true,
		DefaultRepoVisibility: VisPublic, NotifyEmail: true}
	if err := s.PutProfile(ctx, "ada", p); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, found, err := s.GetProfile(ctx, "ada")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if !got.Public || got.DisplayName != "Ada" || got.RepoVisibilityDefault() != VisPublic || !got.NotifyEmail {
		t.Fatalf("round-trip lost fields: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("timestamps must be set")
	}

	// Update keeps CreatedAt.
	got.Bio = "updated"
	if err := s.PutProfile(ctx, "ada", got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, _, _ := s.GetProfile(ctx, "ada")
	if !again.CreatedAt.Equal(got.CreatedAt) {
		t.Error("CreatedAt must survive updates")
	}
	if again.Bio != "updated" {
		t.Error("update lost the bio")
	}

	// Zero value reads private (§1: private is the default).
	if (Profile{}).RepoVisibilityDefault() != VisPrivate {
		t.Error("zero-value default visibility must be private")
	}

	// Shape gates.
	if err := s.PutProfile(ctx, "ada", Profile{Bio: strings.Repeat("x", 1001)}); err == nil {
		t.Error("over-long bio must refuse")
	}
	if err := s.PutProfile(ctx, "ada", Profile{DefaultRepoVisibility: "internal"}); err == nil {
		t.Error("bad visibility must refuse")
	}
	if err := s.PutProfile(ctx, "ada", Profile{PinnedRepoIDs: []string{"../../etc"}}); err == nil {
		t.Error("non-id pins must refuse")
	}

	if err := s.DeleteProfile(ctx, "ada"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := s.GetProfile(ctx, "ada"); found {
		t.Fatal("deleted profile must read as never created")
	}
}
