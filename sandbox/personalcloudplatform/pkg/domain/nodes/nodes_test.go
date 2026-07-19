package nodes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// testStore builds a nodes store over the in-memory fake, plus an "ada"
// account for quota accounting.
func testStore(t *testing.T) (*Store, *users.Store, string) {
	t.Helper()
	db := kvxtest.New(t)
	us := &users.Store{DB: db}
	if _, err := us.CreateUser(context.Background(), "ada", "Ada", "password123"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return &Store{DB: db, Users: us}, us, kvx.NewID() // a fresh drive id
}

func TestFolderCRUDAndOCCNameConflicts(t *testing.T) {
	s, _, drive := testStore(t)
	ctx := context.Background()

	docs, err := s.CreateFolder(ctx, drive, RootID, "Docs", "ada")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Case-insensitive name uniqueness IS the child key.
	if _, err := s.CreateFolder(ctx, drive, RootID, "docs", "ada"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate mkdir = %v, want ErrNameTaken", err)
	}
	// A file colliding with the folder name is refused too.
	if _, err := s.CommitFile(ctx, drive, RootID, "Docs", kvx.NewID(), "text/plain", 3, "ada", true); err == nil {
		t.Fatal("file over folder name must fail")
	}
	// Children list from one prefix, dirs first.
	if _, err := s.CommitFile(ctx, drive, RootID, "a.txt", kvx.NewID(), "text/plain", 3, "ada", true); err != nil {
		t.Fatalf("commit: %v", err)
	}
	kids, err := s.ListFolder(ctx, drive, RootID)
	if err != nil || len(kids) != 2 {
		t.Fatalf("list = %v (%v)", kids, err)
	}
	if !kids[0].IsDir || kids[0].Name != "Docs" || kids[1].Name != "a.txt" {
		t.Fatalf("ordering wrong: %+v", kids)
	}
	// GetByID resolves through the ref; the root is synthetic.
	if n, found, _ := s.GetByID(ctx, drive, docs.ID); !found || n.Name != "Docs" {
		t.Fatalf("GetByID = %+v found=%v", n, found)
	}
	if n, found, _ := s.GetByID(ctx, drive, RootID); !found || !n.IsDir {
		t.Fatal("root must resolve as a folder")
	}
	// Rename keeps the id, frees the old key, refuses collisions.
	if _, err := s.Rename(ctx, drive, docs.ID, "a.TXT", "ada"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("rename onto file = %v, want ErrNameTaken", err)
	}
	ren, err := s.Rename(ctx, drive, docs.ID, "Papers", "ada")
	if err != nil || ren.ID != docs.ID {
		t.Fatalf("rename: %v (%+v)", err, ren)
	}
	if _, found, _ := s.GetChild(ctx, drive, RootID, "Docs"); found {
		t.Fatal("old name still resolves after rename")
	}
	// Case-only rename of the SAME node is allowed (key unchanged).
	if _, err := s.Rename(ctx, drive, docs.ID, "papers", "ada"); err != nil {
		t.Fatalf("case-only rename: %v", err)
	}
}

func TestMoveRejectsCycles(t *testing.T) {
	s, _, drive := testStore(t)
	ctx := context.Background()
	a, _ := s.CreateFolder(ctx, drive, RootID, "A", "ada")
	b, _ := s.CreateFolder(ctx, drive, a.ID, "B", "ada")
	c, _ := s.CreateFolder(ctx, drive, b.ID, "C", "ada")

	if _, err := s.Move(ctx, drive, a.ID, a.ID, "ada"); err == nil {
		t.Fatal("move into itself must fail")
	}
	if _, err := s.Move(ctx, drive, a.ID, c.ID, "ada"); err == nil {
		t.Fatal("move into own subtree must fail")
	}
	// A legal move updates the ref and both parent keys.
	if _, err := s.Move(ctx, drive, c.ID, RootID, "ada"); err != nil {
		t.Fatalf("legal move: %v", err)
	}
	crumbs, err := s.Path(ctx, drive, c.ID)
	if err != nil || len(crumbs) != 2 || crumbs[0].ID != RootID {
		t.Fatalf("path after move = %+v (%v)", crumbs, err)
	}
	// Destination name must be free.
	if _, err := s.CreateFolder(ctx, drive, a.ID, "C", "ada"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := s.Move(ctx, drive, c.ID, a.ID, "ada"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("move onto taken name = %v, want ErrNameTaken", err)
	}
}

func TestVersionsAndRestore(t *testing.T) {
	s, _, drive := testStore(t)
	ctx := context.Background()
	blob1, blob2 := kvx.NewID(), kvx.NewID()
	n1, err := s.CommitFile(ctx, drive, RootID, "Notes.txt", blob1, "text/plain", 10, "ada", true)
	if err != nil {
		t.Fatalf("commit v1: %v", err)
	}
	n2, err := s.CommitFile(ctx, drive, RootID, "Notes.txt", blob2, "text/plain", 20, "ada", true)
	if err != nil {
		t.Fatalf("commit v2: %v", err)
	}
	if n2.ID != n1.ID || n2.Version != 2 || n2.BlobID != blob2 {
		t.Fatalf("overwrite must keep the id and bump the version: %+v", n2)
	}
	vs, err := s.ListVersions(ctx, drive, n1.ID, 0)
	if err != nil || len(vs) != 2 {
		t.Fatalf("versions = %+v (%v)", vs, err)
	}
	// Newest first: head is v2.
	if vs[0].N != 2 || vs[1].N != 1 {
		t.Fatalf("version order wrong: %+v", vs)
	}
	// Restore v1: a NEW version pointing at the old blob, uncharged.
	restored, err := s.RestoreVersion(ctx, drive, n1.ID, vs[1].Rev, "ada")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Version != 3 || restored.BlobID != blob1 || restored.Size != 10 {
		t.Fatalf("restored node = %+v", restored)
	}
	vs, _ = s.ListVersions(ctx, drive, n1.ID, 0)
	if len(vs) != 3 || vs[0].Charged != "n" {
		t.Fatalf("restore must append an UNCHARGED row: %+v", vs)
	}
}

func TestDeleteForeverRefundsQuota(t *testing.T) {
	s, us, drive := testStore(t)
	ctx := context.Background()
	folder, _ := s.CreateFolder(ctx, drive, RootID, "Media", "ada")
	// Two charged uploads inside the folder (blob writes are faked: the
	// version rows carry the charges).
	for i, size := range []int64{100, 250} {
		name := []string{"a.bin", "b.bin"}[i]
		if err := us.ChargeQuota(ctx, "ada", size, 0); err != nil {
			t.Fatalf("charge: %v", err)
		}
		if _, err := s.CommitFile(ctx, drive, folder.ID, name, kvx.NewID(), "application/octet-stream", size, "ada", true); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	u, _, _ := us.Get(ctx, "ada")
	if u.UsedBytes != 350 {
		t.Fatalf("used = %d, want 350", u.UsedBytes)
	}
	var purged []string
	freed, err := s.DeleteForever(ctx, drive, folder.ID, func(id string) { purged = append(purged, id) })
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if freed != 350 {
		t.Fatalf("freed = %d, want 350", freed)
	}
	if len(purged) != 3 { // folder + two files
		t.Fatalf("onPurge saw %d nodes, want 3", len(purged))
	}
	u, _, _ = us.Get(ctx, "ada")
	if u.UsedBytes != 0 {
		t.Fatalf("refund failed: used = %d", u.UsedBytes)
	}
	if _, found, _ := s.GetByID(ctx, drive, folder.ID); found {
		t.Fatal("folder still resolves after delete")
	}
	if kids, _ := s.ListFolder(ctx, drive, RootID); len(kids) != 0 {
		t.Fatalf("root not empty after delete: %+v", kids)
	}
}

func TestListFolderPageCursors(t *testing.T) {
	s, _, drive := testStore(t)
	ctx := context.Background()
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if _, err := s.CreateFolder(ctx, drive, RootID, name, "ada"); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("cursor never terminated")
		}
		ns, next, err := s.ListFolderPage(ctx, drive, RootID, cursor, 2)
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		for _, n := range ns {
			got = append(got, n.Name)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != 5 || got[0] != "a" || got[4] != "e" {
		t.Fatalf("paged listing = %v", got)
	}
}

func TestSearchNamesAndReachable(t *testing.T) {
	s, _, drive := testStore(t)
	ctx := context.Background()
	folder, _ := s.CreateFolder(ctx, drive, RootID, "Reports", "ada")
	n, _ := s.CommitFile(ctx, drive, folder.ID, "FindMeReport.txt", kvx.NewID(), "text/plain", 1, "ada", true)

	ids, err := s.SearchNames(ctx, drive, []string{"find", "report"}, 10)
	if err != nil || len(ids) != 1 || ids[0] != n.ID {
		t.Fatalf("search = %v (%v)", ids, err)
	}
	if ids, _ := s.SearchNames(ctx, drive, []string{"absent"}, 10); len(ids) != 0 {
		t.Fatalf("bogus search hit: %v", ids)
	}
	if ok, _ := s.Reachable(ctx, drive, n.ID); !ok {
		t.Fatal("live node must be reachable")
	}
}

func TestChunkedSessionRecords(t *testing.T) {
	s, _, drive := testStore(t)
	ctx := context.Background()
	id, err := s.InitChunked(ctx, "ada", drive, RootID, "big.iso")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	meta, ok := s.GetTmpMeta(ctx, "ada", id)
	if !ok || meta.Drive != drive || meta.Name != "big.iso" {
		t.Fatalf("meta = %+v ok=%v", meta, ok)
	}
	// Bad names and ids never become sessions/keys.
	if _, err := s.InitChunked(ctx, "ada", drive, RootID, "../evil"); err == nil {
		t.Fatal("traversal name must be rejected")
	}
	if _, ok := s.GetTmpMeta(ctx, "ada", "../../etc"); ok {
		t.Fatal("junk id must miss")
	}
	// The sweep collects old sessions and keeps fresh ones.
	s.SweepTmp(ctx, "ada", time.Hour)
	if _, ok := s.GetTmpMeta(ctx, "ada", id); !ok {
		t.Fatal("fresh session swept")
	}
	s.SweepTmp(ctx, "ada", 0)
	if _, ok := s.GetTmpMeta(ctx, "ada", id); ok {
		t.Fatal("stale session survived the sweep")
	}
}

func TestValidRev(t *testing.T) {
	if !ValidRev(kvx.InvID()) {
		t.Fatal("a freshly minted rev must validate")
	}
	for _, bad := range []string{"", "short", "abcdefghijklmnopqrst-xyz", "12345678901234567890/../x"} {
		if ValidRev(bad) {
			t.Errorf("ValidRev(%q) = true", bad)
		}
	}
}
