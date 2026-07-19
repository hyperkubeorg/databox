// storer_test.go — the databox storer (§6.2): tiered object round
// trips, fork-chain read-through, dedup, caps, abort sweeps, and ref
// CAS — all against the kvxtest fake with real OCC semantics.
package git

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage"
)

// memObj builds an in-memory encoded object.
func memObj(t plumbing.ObjectType, content []byte) plumbing.EncodedObject {
	o := &plumbing.MemoryObject{}
	o.SetType(t)
	o.Write(content)
	return o
}

// readAll drains an encoded object's content.
func readAll(t *testing.T, obj plumbing.EncodedObject) []byte {
	t.Helper()
	r, err := obj.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// seedRepo creates ada + a repo and opens its storer.
func seedRepo(t *testing.T, s *Store, name string) (Repo, *RepoStorer) {
	t.Helper()
	ctx := context.Background()
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	sto, err := s.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	return repo, sto
}

func TestStorerRoundTripTiers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, sto := seedRepo(t, s, "tiers")

	small := []byte("hello loose object")
	// Large: incompressible (seeded-random) bytes so the ENCODED size
	// clears the 256 KiB blob threshold.
	large := make([]byte, objBlobThreshold+64<<10)
	rand.New(rand.NewSource(1)).Read(large)
	smallHash, err := sto.SetEncodedObject(memObj(plumbing.BlobObject, small))
	if err != nil {
		t.Fatal(err)
	}
	largeHash, err := sto.SetEncodedObject(memObj(plumbing.BlobObject, large))
	if err != nil {
		t.Fatal(err)
	}
	commit := []byte("tree 0000000000000000000000000000000000000000\nauthor a <a@a> 0 +0000\ncommitter a <a@a> 0 +0000\n\nx\n")
	commitHash, err := sto.SetEncodedObject(memObj(plumbing.CommitObject, commit))
	if err != nil {
		t.Fatal(err)
	}
	if err := sto.Flush(); err != nil {
		t.Fatal(err)
	}

	// Tier placement: small + commit in KV, large in the blob space.
	if _, found, _ := s.DB.Get(ctx, objKey(repo.ID, smallHash.String())); !found {
		t.Error("small object missing from the KV tier")
	}
	if _, _, found, _ := s.DB.StatBlob(ctx, objBlobKey(repo.ID, largeHash.String())); !found {
		t.Error("large object missing from the blob tier")
	}
	if _, found, _ := s.DB.Get(ctx, objKey(repo.ID, largeHash.String())); found {
		t.Error("large object leaked into the KV tier")
	}

	// A FRESH storer (no caches) reads everything back.
	sto2, _ := s.Storer(ctx, repo)
	for _, tc := range []struct {
		hash    plumbing.Hash
		objType plumbing.ObjectType
		content []byte
	}{
		{smallHash, plumbing.BlobObject, small},
		{largeHash, plumbing.BlobObject, large},
		{commitHash, plumbing.CommitObject, commit},
	} {
		obj, err := sto2.EncodedObject(plumbing.AnyObject, tc.hash)
		if err != nil {
			t.Fatalf("read %s: %v", tc.hash, err)
		}
		if obj.Type() != tc.objType || !bytes.Equal(readAll(t, obj), tc.content) {
			t.Fatalf("round trip mismatch for %s", tc.hash)
		}
		// Stat without a full read (§6.2 header).
		if size, err := sto2.EncodedObjectSize(tc.hash); err != nil || size != int64(len(tc.content)) {
			t.Fatalf("size(%s) = %d (err %v), want %d", tc.hash, size, err, len(tc.content))
		}
		if err := sto2.HasEncodedObject(tc.hash); err != nil {
			t.Fatalf("has(%s) = %v", tc.hash, err)
		}
	}
	// Type-filtered lookup misses on the wrong type.
	if _, err := sto2.EncodedObject(plumbing.TreeObject, smallHash); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("wrong-type read = %v, want ErrObjectNotFound", err)
	}
	if err := sto2.HasEncodedObject(plumbing.ComputeHash(plumbing.BlobObject, []byte("ghost"))); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("missing object = %v, want ErrObjectNotFound", err)
	}

	// Iteration sees both tiers; the type filter works.
	iter, err := sto2.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[plumbing.Hash]bool{}
	if err := iter.ForEach(func(o plumbing.EncodedObject) error { seen[o.Hash()] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || !seen[smallHash] || !seen[largeHash] || !seen[commitHash] {
		t.Fatalf("iter saw %v", seen)
	}
	iter, err = sto2.IterEncodedObjects(plumbing.CommitObject)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	iter.ForEach(func(plumbing.EncodedObject) error { count++; return nil })
	if count != 1 {
		t.Fatalf("commit-only iter saw %d objects", count)
	}
}

func TestStorerForkChainFallthrough(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	parent, psto := seedRepo(t, s, "parent")

	content := []byte("shared history")
	h, err := psto.SetEncodedObject(memObj(plumbing.BlobObject, content))
	if err != nil {
		t.Fatal(err)
	}
	if err := psto.Flush(); err != nil {
		t.Fatal(err)
	}
	fork, err := s.ForkRepo(ctx, "ada", parent, "ada", "child")
	if err != nil {
		t.Fatal(err)
	}
	fsto, err := s.Storer(ctx, fork)
	if err != nil {
		t.Fatal(err)
	}
	// Read-through: the fork resolves the parent's object (§6.2)…
	obj, err := fsto.EncodedObject(plumbing.AnyObject, h)
	if err != nil || !bytes.Equal(readAll(t, obj), content) {
		t.Fatalf("fork read-through failed: %v", err)
	}
	// …and dedups against it: re-storing charges nothing.
	if _, err := fsto.SetEncodedObject(memObj(plumbing.BlobObject, content)); err != nil {
		t.Fatal(err)
	}
	if err := fsto.Flush(); err != nil {
		t.Fatal(err)
	}
	if fsto.StoredBytes() != 0 {
		t.Fatalf("dedup stored %d bytes", fsto.StoredBytes())
	}
	// Writes land in the LEAF only: the fork's own object never appears
	// in the parent.
	own, err := fsto.SetEncodedObject(memObj(plumbing.BlobObject, []byte("fork-only")))
	if err != nil {
		t.Fatal(err)
	}
	if err := fsto.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.DB.Get(ctx, objKey(fork.ID, own.String())); !found {
		t.Error("fork write missing from the fork's keyspace")
	}
	if err := psto.HasEncodedObject(own); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Error("fork write leaked into the parent")
	}
}

func TestStorerCapsAndAbort(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, sto := seedRepo(t, s, "caps")

	sto.MaxObjectBytes = 8
	if _, err := sto.SetEncodedObject(memObj(plumbing.BlobObject, []byte("way past eight bytes"))); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("oversize object = %v, want ErrObjectTooLarge", err)
	}
	sto.MaxObjectBytes = DefaultMaxObjectBytes
	sto.MaxObjects = 2
	if _, err := sto.SetEncodedObject(memObj(plumbing.BlobObject, []byte("one"))); err != nil {
		t.Fatal(err)
	}
	if _, err := sto.SetEncodedObject(memObj(plumbing.BlobObject, []byte("two"))); err != nil {
		t.Fatal(err)
	}
	if _, err := sto.SetEncodedObject(memObj(plumbing.BlobObject, []byte("three"))); !errors.Is(err, ErrTooManyObjects) {
		t.Fatalf("over-count = %v, want ErrTooManyObjects", err)
	}

	// The incremental hook observes stored bytes; its error aborts.
	sto2, _ := s.Storer(ctx, repo)
	var accrued int64
	sto2.OnStored = func(d int64) error { accrued += d; return nil }
	big := make([]byte, objBlobThreshold+1024)
	rand.New(rand.NewSource(2)).Read(big)
	if _, err := sto2.SetEncodedObject(memObj(plumbing.BlobObject, big)); err != nil {
		t.Fatal(err)
	}
	if _, err := sto2.SetEncodedObject(memObj(plumbing.BlobObject, []byte("small too"))); err != nil {
		t.Fatal(err)
	}
	if err := sto2.Flush(); err != nil {
		t.Fatal(err)
	}
	if accrued != sto2.StoredBytes() || accrued == 0 {
		t.Fatalf("OnStored accrued %d, StoredBytes %d", accrued, sto2.StoredBytes())
	}
	// Abort sweeps every written key — KV and blob tiers both.
	if err := sto2.Abort(); err != nil {
		t.Fatal(err)
	}
	if entries, _, _ := s.DB.List(ctx, objPrefix+repo.ID+"/", "", 10); len(entries) > 2 {
		t.Fatalf("abort left %d KV objects (want the 2 pre-abort ones)", len(entries))
	}
	bigHash := plumbing.ComputeHash(plumbing.BlobObject, big)
	if _, _, found, _ := s.DB.StatBlob(ctx, objBlobKey(repo.ID, bigHash.String())); found {
		t.Error("abort left the blob-tier object")
	}
}

func TestStorerRefCASConflict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	_, sto := seedRepo(t, s, "refs")
	_ = ctx

	name := plumbing.NewBranchReferenceName("main")
	a := plumbing.ComputeHash(plumbing.BlobObject, []byte("a"))
	b := plumbing.ComputeHash(plumbing.BlobObject, []byte("b"))
	if err := sto.SetReference(plumbing.NewHashReference(name, a)); err != nil {
		t.Fatal(err)
	}
	// CAS against a stale old value refuses.
	stale := plumbing.NewHashReference(name, b)
	if err := sto.CheckAndSetReference(plumbing.NewHashReference(name, b), stale); !errors.Is(err, storage.ErrReferenceHasChanged) {
		t.Fatalf("stale CAS = %v, want ErrReferenceHasChanged", err)
	}
	// CAS against the real value wins.
	if err := sto.CheckAndSetReference(plumbing.NewHashReference(name, b), plumbing.NewHashReference(name, a)); err != nil {
		t.Fatal(err)
	}
	got, err := sto.Reference(name)
	if err != nil || got.Hash() != b {
		t.Fatalf("ref = %v (err %v), want %v", got, err, b)
	}
	// Iteration lists HEAD (symbolic) + the branch.
	iter, err := sto.IterReferences()
	if err != nil {
		t.Fatal(err)
	}
	names := map[plumbing.ReferenceName]bool{}
	iter.ForEach(func(r *plumbing.Reference) error { names[r.Name()] = true; return nil })
	if !names[plumbing.HEAD] || !names[name] || len(names) != 2 {
		t.Fatalf("iter refs = %v", names)
	}
}
