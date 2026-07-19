// lastcommit_test.go — the tree-listing attribution walk (§5.2):
// multi-commit fixture with a subdirectory, a rename, and a mode flip;
// the walk-cap fallback (unattributed entries are simply absent); the
// cache round trip; and the path-filtered log (filelog.go) over the
// same history.
package git

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// nfile is one fixture file: content + the executable bit.
type nfile struct {
	content string
	exec    bool
}

// buildNestedTree writes the tree (and subtrees) for files — paths may
// contain "/" — returning the root tree hash.
func buildNestedTree(t *testing.T, sto *RepoStorer, files map[string]nfile) plumbing.Hash {
	t.Helper()
	direct := map[string]nfile{}
	subs := map[string]map[string]nfile{}
	for path, f := range files {
		if i := strings.IndexByte(path, '/'); i >= 0 {
			d := path[:i]
			if subs[d] == nil {
				subs[d] = map[string]nfile{}
			}
			subs[d][path[i+1:]] = f
		} else {
			direct[path] = f
		}
	}
	var entries []object.TreeEntry
	for name, f := range direct {
		h, err := sto.SetEncodedObject(memObj(plumbing.BlobObject, []byte(f.content)))
		if err != nil {
			t.Fatal(err)
		}
		mode := filemode.Regular
		if f.exec {
			mode = filemode.Executable
		}
		entries = append(entries, object.TreeEntry{Name: name, Mode: mode, Hash: h})
	}
	for name, sub := range subs {
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: buildNestedTree(t, sto, sub)})
	}
	// Git's tree order: directories sort as "name/".
	key := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(entries, func(i, j int) bool { return key(entries[i]) < key(entries[j]) })
	obj := sto.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
		t.Fatal(err)
	}
	h, err := sto.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// commitNested lands one commit whose tree is EXACTLY files (declared
// whole per commit — no merging), moving the branch ref.
func commitNested(t *testing.T, s *Store, repo Repo, sto *RepoStorer, branch, msg string,
	parent plumbing.Hash, files map[string]nfile) plumbing.Hash {
	t.Helper()
	sig := object.Signature{Name: "Ada", Email: "a@a", When: time.Now()}
	commit := object.Commit{Author: sig, Committer: sig, Message: msg,
		TreeHash: buildNestedTree(t, sto, files)}
	if !parent.IsZero() {
		commit.ParentHashes = []plumbing.Hash{parent}
	}
	obj := sto.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatal(err)
	}
	h, err := sto.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := sto.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRefUpdates(context.Background(), repo.ID, []RefUpdate{
		{Name: "refs/heads/" + branch, Old: parent, New: h},
	}, 0); err != nil {
		t.Fatal(err)
	}
	return h
}

// lastCommitFixture builds the shared five-commit history:
//
//	c1  adds a.txt, dir/b.txt
//	c2  modifies a.txt
//	c3  adds c.txt
//	c4  renames a.txt → renamed.txt (same blob)
//	c5  chmod +x dir/b.txt (mode-only change)
func lastCommitFixture(t *testing.T, s *Store) (Repo, *RepoStorer, [5]plumbing.Hash) {
	t.Helper()
	seedUser(t, s, "ada")
	repo, sto := seedRepo(t, s, "attrib")
	var c [5]plumbing.Hash
	c[0] = commitNested(t, s, repo, sto, "main", "one\n", plumbing.ZeroHash,
		map[string]nfile{"a.txt": {content: "A1"}, "dir/b.txt": {content: "B1"}})
	c[1] = commitNested(t, s, repo, sto, "main", "two\n", c[0],
		map[string]nfile{"a.txt": {content: "A2"}, "dir/b.txt": {content: "B1"}})
	c[2] = commitNested(t, s, repo, sto, "main", "three\n", c[1],
		map[string]nfile{"a.txt": {content: "A2"}, "dir/b.txt": {content: "B1"}, "c.txt": {content: "C1"}})
	c[3] = commitNested(t, s, repo, sto, "main", "four\n", c[2],
		map[string]nfile{"renamed.txt": {content: "A2"}, "dir/b.txt": {content: "B1"}, "c.txt": {content: "C1"}})
	c[4] = commitNested(t, s, repo, sto, "main", "five\n", c[3],
		map[string]nfile{"renamed.txt": {content: "A2"}, "dir/b.txt": {content: "B1", exec: true}, "c.txt": {content: "C1"}})
	return repo, sto, c
}

func TestDirLastCommits(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, sto, c := lastCommitFixture(t, s)

	// Root at head: the rename owns renamed.txt, the add owns c.txt, the
	// mode flip inside dir owns dir (its tree hash changed at c5).
	got, err := s.DirLastCommits(ctx, sto, c[4], "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]plumbing.Hash{"renamed.txt": c[3], "c.txt": c[2], "dir": c[4]}
	if len(got) != len(want) {
		t.Fatalf("root attribution = %+v", got)
	}
	for name, wantSha := range want {
		if got[name].SHA != wantSha.String() {
			t.Errorf("%s attributed to %s, want %s", name, got[name].SHA[:8], wantSha.String()[:8])
		}
	}
	if got["c.txt"].Subject != "three" || got["c.txt"].When.IsZero() {
		t.Errorf("c.txt row = %+v, want subject three + a time", got["c.txt"])
	}

	// Inside dir at head: the mode-only change attributes b.txt to c5.
	got, err = s.DirLastCommits(ctx, sto, c[4], "dir")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["b.txt"].SHA != c[4].String() {
		t.Fatalf("dir attribution = %+v, want b.txt → c5", got)
	}

	// The same listing at an EARLIER commit: a.txt still exists and its
	// last touch is the c2 edit; dir is untouched since c1.
	got, err = s.DirLastCommits(ctx, sto, c[2], "")
	if err != nil {
		t.Fatal(err)
	}
	if got["a.txt"].SHA != c[1].String() || got["dir"].SHA != c[0].String() || got["c.txt"].SHA != c[2].String() {
		t.Fatalf("root@c3 attribution = %+v", got)
	}

	// A directory that doesn't exist: empty map, no error.
	if got, err := s.DirLastCommits(ctx, sto, c[4], "ghost"); err != nil || len(got) != 0 {
		t.Fatalf("ghost dir = %+v %v", got, err)
	}
}

func TestDirLastCommitsCapAndCache(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	repo, sto, c := lastCommitFixture(t, s)

	// Walk cap 1: only what c5 itself touched attributes; the rest is
	// absent — the page's em-dash fallback.
	got, err := dirLastCommits(sto, c[4], "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["dir"].SHA != c[4].String() {
		t.Fatalf("capped walk = %+v, want only dir → c5", got)
	}

	// Cache round trip: the first call writes the family row…
	if _, err := s.DirLastCommits(ctx, sto, c[4], "dir"); err != nil {
		t.Fatal(err)
	}
	key := lastCommitKey(repo.ID, c[4].String(), "dir")
	e, found, err := s.DB.Get(ctx, key)
	if err != nil || !found {
		t.Fatalf("cache row missing at %s (err %v)", key, err)
	}
	// …and the second call READS it: tamper the row, expect the tamper back.
	var cached map[string]LastCommit
	if err := json.Unmarshal(e.Value, &cached); err != nil {
		t.Fatal(err)
	}
	row := cached["b.txt"]
	row.Subject = "tampered sentinel"
	cached["b.txt"] = row
	raw, _ := json.Marshal(cached)
	if _, err := s.DB.Set(ctx, key, raw); err != nil {
		t.Fatal(err)
	}
	got, err = s.DirLastCommits(ctx, sto, c[4], "dir")
	if err != nil {
		t.Fatal(err)
	}
	if got["b.txt"].Subject != "tampered sentinel" {
		t.Fatalf("second call recomputed instead of reading the cache: %+v", got["b.txt"])
	}
}

func TestLogPath(t *testing.T) {
	s := testStore(t)
	_, sto, c := lastCommitFixture(t, s)

	shas := func(log []CommitInfo) []string {
		out := make([]string, len(log))
		for i, ci := range log {
			out[i] = ci.Hash.String()
		}
		return out
	}
	wantShas := func(name string, got []string, want ...plumbing.Hash) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s = %v, want %d commits", name, got, len(want))
		}
		for i := range want {
			if got[i] != want[i].String() {
				t.Errorf("%s[%d] = %s, want %s", name, i, got[i][:8], want[i].String()[:8])
			}
		}
	}

	// a.txt: created c1, edited c2, deleted (renamed away) c4.
	log, next, capped, err := sto.LogPath(c[4], "a.txt", 30, 0)
	if err != nil || capped || !next.IsZero() {
		t.Fatalf("a.txt log err=%v capped=%v next=%v", err, capped, next)
	}
	wantShas("a.txt", shas(log), c[3], c[1], c[0])

	// Nested path + the directory itself.
	log, _, _, err = sto.LogPath(c[4], "dir/b.txt", 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantShas("dir/b.txt", shas(log), c[4], c[0])
	log, _, _, err = sto.LogPath(c[4], "dir", 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantShas("dir", shas(log), c[4], c[0])

	// Scan cap: two commits scanned (c5, c4) find one a.txt match; the
	// cursor continues from c3 and the capped flag says "stopped early".
	log, next, capped, err = sto.LogPath(c[4], "a.txt", 30, 2)
	if err != nil || !capped {
		t.Fatalf("capped scan err=%v capped=%v", err, capped)
	}
	wantShas("a.txt capped", shas(log), c[3])
	if next != c[2] {
		t.Fatalf("capped next = %s, want c3", next.String()[:8])
	}
	// Continuing from the cursor finds the rest.
	log, next, capped, err = sto.LogPath(next, "a.txt", 30, 0)
	if err != nil || capped || !next.IsZero() {
		t.Fatalf("continuation err=%v capped=%v next=%v", err, capped, next)
	}
	wantShas("a.txt continued", shas(log), c[1], c[0])

	// Limit pagination: page size 1 leaves a cursor, not a capped flag.
	log, next, capped, err = sto.LogPath(c[4], "dir/b.txt", 1, 0)
	if err != nil || capped || next.IsZero() {
		t.Fatalf("limit page err=%v capped=%v next=%v", err, capped, next)
	}
	wantShas("dir/b.txt page 1", shas(log), c[4])
}

func TestBlameFile(t *testing.T) {
	s := testStore(t)
	seedUser(t, s, "ada")
	repo, sto := seedRepo(t, s, "blame")
	c1 := commitNested(t, s, repo, sto, "main", "one\n", plumbing.ZeroHash,
		map[string]nfile{"f.txt": {content: "alpha\nbeta\ngamma\n"}})
	c2 := commitNested(t, s, repo, sto, "main", "two\n", c1,
		map[string]nfile{"f.txt": {content: "alpha\nBETA CHANGED\ngamma\n"}})

	lines, err := BlameFile(sto, c2, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("blame lines = %d, want 3", len(lines))
	}
	want := []struct {
		sha  plumbing.Hash
		text string
	}{{c1, "alpha"}, {c2, "BETA CHANGED"}, {c1, "gamma"}}
	for i, w := range want {
		if lines[i].SHA != w.sha.String() || strings.TrimRight(lines[i].Text, "\n") != w.text {
			t.Errorf("line %d = %q @ %s, want %q @ %s", i+1,
				lines[i].Text, lines[i].SHA[:8], w.text, w.sha.String()[:8])
		}
	}
	if lines[0].Author == "" || lines[0].When.IsZero() {
		t.Errorf("line 1 missing author/when: %+v", lines[0])
	}

	// Missing path errors (the page's 404).
	if _, err := BlameFile(sto, c2, "ghost.txt"); err == nil {
		t.Fatal("blame of a missing path must error")
	}
}
