package drives

import (
	"context"
	"testing"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

func testStores(t *testing.T) (*Store, *users.Store) {
	t.Helper()
	db := kvxtest.New(t)
	us := &users.Store{DB: db}
	return &Store{DB: db, Users: us}, us
}

func mkUser(t *testing.T, us *users.Store, name string) users.User {
	t.Helper()
	u, err := us.CreateUser(context.Background(), name, name, "password123")
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return u
}

func TestRoleOrdering(t *testing.T) {
	if !RoleAtLeast(RoleOwner, RoleEditor) || !RoleAtLeast(RoleEditor, RoleViewer) {
		t.Fatal("role ranks broken")
	}
	if RoleAtLeast(RoleViewer, RoleEditor) || RoleAtLeast("", RoleViewer) {
		t.Fatal("weak roles must not pass")
	}
	if StrongerRole(RoleViewer, RoleEditor) != RoleEditor {
		t.Fatal("StrongerRole wrong")
	}
	if ValidRole("admin") {
		t.Fatal("unknown role accepted")
	}
}

// The signup hook births the personal drive in the SAME transaction as
// the account (cmd/pcp wires this exact closure).
func TestPersonalDriveAtSignup(t *testing.T) {
	s, us := testStores(t)
	ctx := context.Background()
	us.OnSignup = func(tx *client.Tx, u *users.User) {
		id := kvx.NewID()
		StagePersonalDrive(tx, id, u.Username)
		u.PersonalDrive = id
	}
	u := mkUser(t, us, "ada")
	if u.PersonalDrive == "" {
		t.Fatal("signup didn't set PersonalDrive")
	}
	d, found, err := s.Get(ctx, u.PersonalDrive)
	if err != nil || !found || d.Type != Personal || d.Owner != "ada" {
		t.Fatalf("personal drive = %+v found=%v (%v)", d, found, err)
	}
	m, found, _ := s.GetMember(ctx, d.ID, "ada")
	if !found || m.Role != RoleOwner {
		t.Fatalf("owner membership = %+v found=%v", m, found)
	}
	// The reverse index serves the sidebar with one List.
	infos, err := s.UserDriveInfos(ctx, "ada")
	if err != nil || len(infos) != 1 || infos[0].Role != RoleOwner {
		t.Fatalf("infos = %+v (%v)", infos, err)
	}
	// The stored account carries the id.
	got, _, _ := us.Get(ctx, "ada")
	if got.PersonalDrive != u.PersonalDrive {
		t.Fatal("PersonalDrive not persisted")
	}
}

// ClaimPersonalDrive backfills accounts that predate the hook, exactly
// once.
func TestClaimPersonalDrive(t *testing.T) {
	s, us := testStores(t)
	ctx := context.Background()
	mkUser(t, us, "old-timer")
	mint := func(tx *client.Tx) string {
		id := kvx.NewID()
		StagePersonalDrive(tx, id, "old-timer")
		return id
	}
	first, err := us.ClaimPersonalDrive(ctx, "old-timer", mint)
	if err != nil || first == "" {
		t.Fatalf("claim: %v (%q)", err, first)
	}
	second, err := us.ClaimPersonalDrive(ctx, "old-timer", mint)
	if err != nil || second != first {
		t.Fatalf("second claim = %q (%v), want %q", second, err, first)
	}
	if _, found, _ := s.Get(ctx, first); !found {
		t.Fatal("claimed drive missing")
	}
}

func TestSharedDriveMembership(t *testing.T) {
	s, us := testStores(t)
	ctx := context.Background()
	mkUser(t, us, "ada")
	mkUser(t, us, "bob")

	d, err := s.CreateShared(ctx, "ada", "Team Space")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Unknown members never gain rows.
	if err := s.SetMember(ctx, d.ID, "ghost", RoleEditor); err == nil {
		t.Fatal("unknown member accepted")
	}
	if err := s.SetMember(ctx, d.ID, "bob", RoleEditor); err != nil {
		t.Fatalf("add member: %v", err)
	}
	// Both directions land: drive-side row and bob's sidebar.
	if m, found, _ := s.GetMember(ctx, d.ID, "bob"); !found || m.Role != RoleEditor {
		t.Fatalf("member = %+v found=%v", m, found)
	}
	if infos, _ := s.UserDriveInfos(ctx, "bob"); len(infos) != 1 || infos[0].ID != d.ID {
		t.Fatalf("bob's drives = %+v", infos)
	}
	// Role change is an overwrite; the member list sorts owners first.
	_ = s.SetMember(ctx, d.ID, "bob", RoleViewer)
	rows, _ := s.Members(ctx, d.ID)
	if len(rows) != 2 || rows[0].Username != "ada" || rows[1].Role != RoleViewer {
		t.Fatalf("member rows = %+v", rows)
	}
	// The owner can't be removed; a member can.
	if err := s.RemoveMember(ctx, d.ID, "ada"); err == nil {
		t.Fatal("owner removal accepted")
	}
	if err := s.RemoveMember(ctx, d.ID, "bob"); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if infos, _ := s.UserDriveInfos(ctx, "bob"); len(infos) != 0 {
		t.Fatalf("bob still sees the drive: %+v", infos)
	}
	// Rename applies to shared drives only.
	if err := s.Rename(ctx, d.ID, "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// Delete removes memberships (both directions) and the record.
	if err := s.Delete(ctx, d.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := s.Get(ctx, d.ID); found {
		t.Fatal("drive survives delete")
	}
	if infos, _ := s.UserDriveInfos(ctx, "ada"); len(infos) != 0 {
		t.Fatalf("ada still sees the deleted drive: %+v", infos)
	}
}

func TestPersonalDriveRestrictions(t *testing.T) {
	s, us := testStores(t)
	ctx := context.Background()
	mkUser(t, us, "ada")
	mkUser(t, us, "bob")
	var driveID string
	_, err := us.ClaimPersonalDrive(ctx, "ada", func(tx *client.Tx) string {
		driveID = kvx.NewID()
		StagePersonalDrive(tx, driveID, "ada")
		return driveID
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.Rename(ctx, driveID, "Sneaky"); err == nil {
		t.Fatal("personal drives must not rename")
	}
	if err := s.SetMember(ctx, driveID, "bob", RoleEditor); err == nil {
		t.Fatal("personal drives must not gain members")
	}
}
