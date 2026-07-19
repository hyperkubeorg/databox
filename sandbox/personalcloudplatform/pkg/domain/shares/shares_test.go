package shares

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// fixture builds the full domain stack over the fake, one shared drive
// owned by ada with bob as viewer and cara outside, and a small tree:
// root/Folder/Sub/File.txt.
type fixture struct {
	ctx    context.Context
	s      *Store
	us     *users.Store
	ds     *drives.Store
	ns     *nodes.Store
	drive  drives.Drive
	folder nodes.Node
	sub    nodes.Node
	file   nodes.Node
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := kvxtest.New(t)
	us := &users.Store{DB: db}
	ds := &drives.Store{DB: db, Users: us}
	ns := &nodes.Store{DB: db, Users: us}
	s := &Store{DB: db, Nodes: ns, Drives: ds, Users: us}
	ctx := context.Background()
	for _, name := range []string{"ada", "bob", "cara"} {
		if _, err := us.CreateUser(ctx, name, name, "password123"); err != nil {
			t.Fatalf("user %s: %v", name, err)
		}
	}
	d, err := ds.CreateShared(ctx, "ada", "Team")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if err := ds.SetMember(ctx, d.ID, "bob", drives.RoleViewer); err != nil {
		t.Fatalf("member: %v", err)
	}
	folder, err := ns.CreateFolder(ctx, d.ID, nodes.RootID, "Folder", "ada")
	if err != nil {
		t.Fatalf("folder: %v", err)
	}
	sub, err := ns.CreateFolder(ctx, d.ID, folder.ID, "Sub", "ada")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	file, err := ns.CommitFile(ctx, d.ID, sub.ID, "File.txt", "AAAAAAAAAAAAAAAA", "text/plain", 42, "ada", true)
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	return &fixture{ctx: ctx, s: s, us: us, ds: ds, ns: ns, drive: d, folder: folder, sub: sub, file: file}
}

// The full resolution matrix: membership beats grants; grants walk
// ancestors; strangers are denied.
func TestAccessMatrix(t *testing.T) {
	f := newFixture(t)
	check := func(user, node, wantRole, wantVia string, wantDenied bool) {
		t.Helper()
		role, via, err := f.s.Access(f.ctx, user, f.drive.ID, node)
		if wantDenied {
			if !errors.Is(err, drives.ErrAccessDenied) {
				t.Fatalf("Access(%s) = %q/%q/%v, want denial", user, role, via, err)
			}
			return
		}
		if err != nil || role != wantRole || via != wantVia {
			t.Fatalf("Access(%s) = %q/%q/%v, want %q/%q", user, role, via, err, wantRole, wantVia)
		}
	}
	// Membership covers the whole tree.
	check("ada", nodes.RootID, drives.RoleOwner, ViaMembership, false)
	check("ada", f.file.ID, drives.RoleOwner, ViaMembership, false)
	check("bob", f.file.ID, drives.RoleViewer, ViaMembership, false)
	// A stranger is denied everywhere.
	check("cara", nodes.RootID, "", "", true)
	check("cara", f.file.ID, "", "", true)
	// A grant on an ANCESTOR folder covers the file (editor).
	if err := f.s.SetGrant(f.ctx, f.drive.ID, f.folder.ID, "cara", drives.RoleEditor, "ada"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	check("cara", f.file.ID, drives.RoleEditor, ViaGrant, false)
	check("cara", f.folder.ID, drives.RoleEditor, ViaGrant, false)
	// …but not siblings outside the granted subtree.
	outside, _ := f.ns.CreateFolder(f.ctx, f.drive.ID, nodes.RootID, "Elsewhere", "ada")
	check("cara", outside.ID, "", "", true)
	// The strongest grant on the chain wins.
	if err := f.s.SetGrant(f.ctx, f.drive.ID, f.file.ID, "cara", drives.RoleViewer, "ada"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	check("cara", f.file.ID, drives.RoleEditor, ViaGrant, false)
	// Ungrant closes the door again.
	_ = f.s.RemoveGrant(f.ctx, f.drive.ID, f.folder.ID, "cara")
	_ = f.s.RemoveGrant(f.ctx, f.drive.ID, f.file.ID, "cara")
	check("cara", f.file.ID, "", "", true)
	// Grants never mint owners.
	if err := f.s.SetGrant(f.ctx, f.drive.ID, f.file.ID, "cara", drives.RoleOwner, "ada"); err == nil {
		t.Fatal("owner grant accepted")
	}
}

func TestSharedWithMe(t *testing.T) {
	f := newFixture(t)
	if err := f.s.SetGrant(f.ctx, f.drive.ID, f.folder.ID, "cara", drives.RoleViewer, "ada"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	rows, err := f.s.ListSharedWithMe(f.ctx, "cara")
	if err != nil || len(rows) != 1 || rows[0].Node.ID != f.folder.ID || rows[0].Grant.By != "ada" {
		t.Fatalf("shared-with-me = %+v (%v)", rows, err)
	}
	// The node ACL panel sees the same grant from the other direction.
	acl, _ := f.s.NodeGrants(f.ctx, f.drive.ID, f.folder.ID)
	if len(acl) != 1 || acl[0].Username != "cara" {
		t.Fatalf("node grants = %+v", acl)
	}
}

func TestShareLinksExpiryAndPassword(t *testing.T) {
	f := newFixture(t)
	// Open link.
	sh, err := f.s.CreateShare(f.ctx, f.drive.ID, f.file.ID, PermDownload, "", time.Time{}, "ada")
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	got, found, _ := f.s.GetShare(f.ctx, sh.Token)
	if !found || got.Expired(time.Now()) || !CheckSharePassword(got, "anything") {
		t.Fatalf("open link = %+v found=%v", got, found)
	}
	// Password link: wrong password refused, right one passes, and the
	// share session unlocks without retyping.
	pw, err := f.s.CreateShare(f.ctx, f.drive.ID, f.file.ID, PermView, "hunter22", time.Time{}, "ada")
	if err != nil {
		t.Fatalf("pw share: %v", err)
	}
	if CheckSharePassword(pw, "wrong") {
		t.Fatal("wrong password accepted")
	}
	if !CheckSharePassword(pw, "hunter22") {
		t.Fatal("right password refused")
	}
	sess, err := f.s.CreateShareSession(f.ctx, pw.Token)
	if err != nil || !f.s.CheckShareSession(f.ctx, sess, pw.Token) {
		t.Fatalf("share session: %v", err)
	}
	if f.s.CheckShareSession(f.ctx, sess, sh.Token) {
		t.Fatal("session unlocks the WRONG link")
	}
	// Expired link reads as expired.
	old, err := f.s.CreateShare(f.ctx, f.drive.ID, f.file.ID, PermView, "", time.Now().Add(-time.Hour), "ada")
	if err != nil {
		t.Fatalf("expired share: %v", err)
	}
	if got, _, _ := f.s.GetShare(f.ctx, old.Token); !got.Expired(time.Now()) {
		t.Fatal("expired link reads live")
	}
	// The node's link list sees all three; revoke removes both rows.
	links, _ := f.s.NodeShares(f.ctx, f.drive.ID, f.file.ID)
	if len(links) != 3 {
		t.Fatalf("node shares = %d, want 3", len(links))
	}
	if err := f.s.RevokeShare(f.ctx, sh.Token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, found, _ := f.s.GetShare(f.ctx, sh.Token); found {
		t.Fatal("revoked link still resolves")
	}
	links, _ = f.s.NodeShares(f.ctx, f.drive.ID, f.file.ID)
	if len(links) != 2 {
		t.Fatalf("node shares after revoke = %d, want 2", len(links))
	}
	// Bogus tokens never become keys.
	if _, found, _ := f.s.GetShare(f.ctx, "../../../etc/passwd"); found {
		t.Fatal("junk token resolved")
	}
}

// DeleteNode (the composed entry point) purges the subtree AND its
// sharing rows, refunding quota.
func TestDeleteNodeSweepsSharing(t *testing.T) {
	f := newFixture(t)
	if err := f.us.ChargeQuota(f.ctx, "ada", 42, 0); err != nil {
		t.Fatalf("charge: %v", err)
	}
	sh, _ := f.s.CreateShare(f.ctx, f.drive.ID, f.file.ID, PermDownload, "", time.Time{}, "ada")
	_ = f.s.SetGrant(f.ctx, f.drive.ID, f.sub.ID, "cara", drives.RoleViewer, "ada")

	freed, err := f.s.DeleteNode(f.ctx, f.drive.ID, f.folder.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if freed != 42 {
		t.Fatalf("freed = %d, want 42", freed)
	}
	if _, found, _ := f.s.GetShare(f.ctx, sh.Token); found {
		t.Fatal("share link survived the delete")
	}
	if rows, _ := f.s.ListSharedWithMe(f.ctx, "cara"); len(rows) != 0 {
		t.Fatalf("grant survived the delete: %+v", rows)
	}
	u, _, _ := f.us.Get(f.ctx, "ada")
	if u.UsedBytes != 0 {
		t.Fatalf("quota not refunded: %d", u.UsedBytes)
	}
}

func TestPurgeDriveSharing(t *testing.T) {
	f := newFixture(t)
	sh, _ := f.s.CreateShare(f.ctx, f.drive.ID, f.file.ID, PermDownload, "", time.Time{}, "ada")
	_ = f.s.SetGrant(f.ctx, f.drive.ID, f.folder.ID, "cara", drives.RoleViewer, "ada")
	if err := f.s.PurgeDriveSharing(f.ctx, f.drive.ID); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, found, _ := f.s.GetShare(f.ctx, sh.Token); found {
		t.Fatal("link survived the drive purge")
	}
	if rows, _ := f.s.ListSharedWithMe(f.ctx, "cara"); len(rows) != 0 {
		t.Fatalf("grants survived the drive purge: %+v", rows)
	}
}
