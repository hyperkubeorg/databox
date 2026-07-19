// repos_test.go — repository records (§5.1), forks (§5.3), namespace
// quota dispatch (§6.5/§7), and the atomic ref-update transaction.
package git

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

func TestCreateRepoRules(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	seedUser(t, s, "bob")
	seedUser(t, s, "carol")
	if _, err := s.CreateOrg(ctx, "acme", "ada", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "acme", "bob", OrgRoleMember); err != nil {
		t.Fatal(err)
	}

	// Own namespace: always allowed; duplicate names refuse.
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "dotfiles"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if repo.Visibility != VisPrivate || repo.DefaultBranch != DefaultBranch {
		t.Fatalf("defaults wrong: %+v", repo)
	}
	if _, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "dotfiles"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name = %v, want ErrNameTaken", err)
	}
	// Public requires the site switch (§2).
	if _, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "pub", Visibility: VisPublic}); err == nil {
		t.Fatal("public repo created while AllowPublic=false")
	}
	if _, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "pub", Visibility: VisPublic, AllowPublic: true}); err != nil {
		t.Fatalf("public repo: %v", err)
	}
	// Bad names never reach the store.
	if _, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "Bad Name!"}); err == nil {
		t.Fatal("bad name accepted")
	}

	// Org rules (§5.1): owner always; member only when the setting is on;
	// outsiders and foreign namespaces never.
	if _, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "acme", Name: "tools"}); err != nil {
		t.Fatalf("owner create in org: %v", err)
	}
	if _, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "bob", NS: "acme", Name: "bobs"}); !errors.Is(err, ErrNoCreate) {
		t.Fatalf("member create with setting off = %v, want ErrNoCreate", err)
	}
	if err := s.UpdateOrgSettings(ctx, "acme", "", PermNone, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "bob", NS: "acme", Name: "bobs"}); err != nil {
		t.Fatalf("member create with setting on: %v", err)
	}
	if _, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "carol", NS: "acme", Name: "nope"}); !errors.Is(err, ErrNoCreate) {
		t.Fatalf("outsider create = %v, want ErrNoCreate", err)
	}
	if _, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "bob", NS: "ada", Name: "steal"}); !errors.Is(err, ErrNoCreate) {
		t.Fatalf("foreign user namespace = %v, want ErrNoCreate", err)
	}

	// Lookups: by path and per-ns listing.
	got, found, err := s.GetRepoByPath(ctx, "ada", "dotfiles")
	if err != nil || !found || got.ID != repo.ID {
		t.Fatalf("GetRepoByPath = %+v %v %v", got, found, err)
	}
	repos, err := s.ListReposByNS(ctx, "ada")
	if err != nil || len(repos) != 2 {
		t.Fatalf("ListReposByNS = %d repos, err %v", len(repos), err)
	}
}

func TestCreateRepoInitReadme(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, err := s.CreateRepo(ctx, CreateRepoInput{
		Creator: "ada", NS: "ada", Name: "hello", Description: "says hi", InitReadme: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if repo.SizeBytes == 0 {
		t.Fatal("initial commit stored no bytes on the record")
	}
	// The default branch points at a real commit whose tree carries the
	// README blob — all readable back through the storer.
	sto, err := s.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := sto.Reference(plumbing.NewBranchReferenceName(DefaultBranch))
	if err != nil {
		t.Fatalf("default branch ref: %v", err)
	}
	commit, err := sto.EncodedObject(plumbing.CommitObject, ref.Hash())
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	if commit.Type() != plumbing.CommitObject {
		t.Fatalf("commit type = %v", commit.Type())
	}
	// The creator's quota was charged for the stored bytes.
	u, _, err := s.Users.Get(ctx, "ada")
	if err != nil || u.UsedBytes != repo.SizeBytes {
		t.Fatalf("quota charge = %d, want %d (err %v)", u.UsedBytes, repo.SizeBytes, err)
	}
	// HEAD resolves symbolically to the default branch.
	head, err := sto.Reference(plumbing.HEAD)
	if err != nil || head.Type() != plumbing.SymbolicReference ||
		head.Target() != plumbing.NewBranchReferenceName(DefaultBranch) {
		t.Fatalf("HEAD = %v (err %v)", head, err)
	}
}

func TestForkAndDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	seedUser(t, s, "bob")
	parent, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "lib", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}

	// A stranger can't fork a private repo — and can't confirm it exists.
	if _, err := s.ForkRepo(ctx, "bob", parent, "bob", "lib"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger fork = %v, want ErrNotFound", err)
	}
	if err := s.SetGrant(ctx, parent.ID, UserSubject("bob"), "read"); err != nil {
		t.Fatal(err)
	}
	fork, err := s.ForkRepo(ctx, "bob", parent, "bob", "lib")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if fork.Visibility != VisPrivate || fork.ForkOf != parent.ID || fork.SizeBytes != 0 {
		t.Fatalf("fork record wrong: %+v", fork)
	}
	// Refs copied (§5.3): the fork's default branch matches the parent's.
	psto, _ := s.Storer(ctx, parent)
	fsto, _ := s.Storer(ctx, fork)
	pref, err := psto.Reference(plumbing.NewBranchReferenceName(DefaultBranch))
	if err != nil {
		t.Fatal(err)
	}
	fref, err := fsto.Reference(plumbing.NewBranchReferenceName(DefaultBranch))
	if err != nil || fref.Hash() != pref.Hash() {
		t.Fatalf("fork ref = %v (err %v), want %v", fref, err, pref.Hash())
	}
	// The fork charged nothing.
	if b, _, _ := s.Users.Get(ctx, "bob"); b.UsedBytes != 0 {
		t.Fatalf("fork charged %d bytes", b.UsedBytes)
	}

	// Fork-block (§5.3): the parent refuses deletion while the fork lives.
	if err := s.DeleteRepo(ctx, parent.ID); !errors.Is(err, ErrHasForks) {
		t.Fatalf("delete forked parent = %v, want ErrHasForks", err)
	}
	if err := s.DeleteRepo(ctx, fork.ID); err != nil {
		t.Fatalf("delete fork: %v", err)
	}
	if err := s.DeleteRepo(ctx, parent.ID); err != nil {
		t.Fatalf("delete parent after fork: %v", err)
	}
	// Deletion refunded the namespace and swept the storage keys.
	if u, _, _ := s.Users.Get(ctx, "ada"); u.UsedBytes != 0 {
		t.Fatalf("delete left %d bytes charged", u.UsedBytes)
	}
	if _, found, _ := s.GetRepoByPath(ctx, "ada", "lib"); found {
		t.Fatal("name index survived deletion")
	}
	if entries, _, err := s.DB.List(ctx, objPrefix+parent.ID+"/", "", 5); err != nil || len(entries) != 0 {
		t.Fatalf("objects survived deletion: %d (err %v)", len(entries), err)
	}
	if entries, _, err := s.DB.List(ctx, refsPrefix+parent.ID+"/", "", 5); err != nil || len(entries) != 0 {
		t.Fatalf("refs survived deletion: %d (err %v)", len(entries), err)
	}
}

func TestApplyRefUpdatesCAS(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "casr"})
	if err != nil {
		t.Fatal(err)
	}
	a := plumbing.ComputeHash(plumbing.BlobObject, []byte("a"))
	b := plumbing.ComputeHash(plumbing.BlobObject, []byte("b"))
	branch := "refs/heads/" + DefaultBranch

	// Create (old = zero) succeeds and lands sizeDelta on the record.
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{{Name: branch, New: a}}, 42); err != nil {
		t.Fatalf("create ref: %v", err)
	}
	if got, _, _ := s.GetRepo(ctx, repo.ID); got.SizeBytes != 42 {
		t.Fatalf("sizeBytes = %d, want 42", got.SizeBytes)
	}
	// Re-create against an existing ref is stale.
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{{Name: branch, New: b}}, 0); !errors.Is(err, ErrStale) {
		t.Fatalf("create over existing = %v, want ErrStale", err)
	}
	// Update with the wrong old value is stale; with the right one wins.
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{{Name: branch, Old: b, New: a}}, 0); !errors.Is(err, ErrStale) {
		t.Fatalf("wrong old = %v, want ErrStale", err)
	}
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{{Name: branch, Old: a, New: b}}, 0); err != nil {
		t.Fatalf("CAS update: %v", err)
	}
	// A stale command anywhere rejects the WHOLE push (atomic, §6.2).
	other := "refs/heads/feature"
	err = s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{
		{Name: other, New: a},
		{Name: branch, Old: a, New: a}, // stale: branch is at b
	}, 0)
	if !errors.Is(err, ErrStale) {
		t.Fatalf("mixed batch = %v, want ErrStale", err)
	}
	if _, found, _ := s.DB.Get(ctx, refKey(repo.ID, other)); found {
		t.Fatal("partial application: sibling ref landed despite the stale command")
	}
	// Delete (new = zero) with the right old value.
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{{Name: branch, Old: b}}, 0); err != nil {
		t.Fatalf("delete ref: %v", err)
	}
	if _, found, _ := s.DB.Get(ctx, refKey(repo.ID, branch)); found {
		t.Fatal("ref survived deletion")
	}
	// Hostile refnames never become keys.
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{{Name: "refs/heads/../../etc", New: a}}, 0); err == nil {
		t.Fatal("dot-walk refname accepted")
	}
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{{Name: "refs/heads/ünïcode", New: a}}, 0); err == nil {
		t.Fatal("non-ASCII refname accepted (v1 restriction)")
	}
}

func TestNSQuota(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	if _, err := s.CreateOrg(ctx, "acme", "ada", ""); err != nil {
		t.Fatal(err)
	}
	sc := site.Config{}

	// User path: bootstrap default applies; charges + refunds dispatch
	// to users.ChargeQuota with ONE quota error for both kinds.
	limit, err := s.NSQuotaLimit(ctx, sc, "ada", 100)
	if err != nil || limit != 100 {
		t.Fatalf("user limit = %d (err %v), want 100", limit, err)
	}
	if err := s.ChargeNSQuota(ctx, "ada", 80, limit); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if err := s.ChargeNSQuota(ctx, "ada", 30, limit); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over-quota user charge = %v, want ErrQuotaExceeded", err)
	}
	if err := s.ChargeNSQuota(ctx, "ada", -80, 0); err != nil {
		t.Fatalf("refund: %v", err)
	}

	// Org path: the org's own override wins (§7).
	if err := s.SetOrgQuotaOverride(ctx, "acme", 50); err != nil {
		t.Fatal(err)
	}
	limit, err = s.NSQuotaLimit(ctx, sc, "acme", 100)
	if err != nil || limit != 50 {
		t.Fatalf("org limit = %d (err %v), want 50", limit, err)
	}
	if err := s.ChargeNSQuota(ctx, "acme", 60, limit); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over-quota org charge = %v, want ErrQuotaExceeded", err)
	}
	if err := s.ChargeNSQuota(ctx, "acme", 40, limit); err != nil {
		t.Fatalf("org charge: %v", err)
	}
	org, _, _ := s.GetOrg(ctx, "acme")
	if org.UsedBytes != 40 {
		t.Fatalf("org used = %d, want 40", org.UsedBytes)
	}
	// Unknown namespaces resolve to a miss, not a zero limit.
	if _, err := s.NSQuotaLimit(ctx, sc, "ghost", 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ghost ns limit err = %v, want ErrNotFound", err)
	}
}

func TestValidRefNameCharset(t *testing.T) {
	good := []string{"refs/heads/main", "refs/tags/v1.0.0", "refs/heads/feat/a-b_c"}
	bad := []string{"", "HEAD/x", "refs/heads/a b", "refs/heads/a..b",
		"refs/heads/a\x01b", "refs/heads/ø", "refs/heads/x.lock", "refs/heads//x"}
	for _, n := range good {
		if err := validRefName(n); err != nil {
			t.Errorf("validRefName(%q) = %v, want nil", n, err)
		}
	}
	for _, n := range bad {
		if err := validRefName(n); err == nil {
			t.Errorf("validRefName(%q) accepted", n)
		}
	}
	// Raw refnames preserve List order under the prefix (the encoding IS
	// the identity for the accepted charset).
	if !(refKey("r", "refs/heads/a") < refKey("r", "refs/heads/b")) ||
		!(refKey("r", "refs/heads/b") < refKey("r", "refs/tags/a")) {
		t.Error("ref keys do not preserve refname order")
	}
}
