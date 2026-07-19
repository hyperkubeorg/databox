// git_test.go — shared fixtures: a fake databox (kvxtest), a store, and
// account seeding.
package git

import (
	"context"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// testStore builds a Store over a fake single-node databox.
func testStore(t *testing.T) *Store {
	t.Helper()
	db := kvxtest.New(t)
	return &Store{DB: db, Users: &users.Store{DB: db}}
}

// seedUser signs an account up through the REAL signup path (so the
// ReserveName hook wiring is exercised where a test installs it).
func seedUser(t *testing.T, s *Store, name string) {
	t.Helper()
	if _, err := s.Users.CreateUser(context.Background(), name, name, "password-1"); err != nil {
		t.Fatalf("seed user %s: %v", name, err)
	}
}
