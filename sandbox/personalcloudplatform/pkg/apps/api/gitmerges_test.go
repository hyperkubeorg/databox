// gitmerges_test.go — the /api/v1/git merge-request slice (§9/§12):
// lifecycle (create/list/get with mergeability/comment/state/merge),
// the §9 rules over bearer auth (read opens + comments, write on the
// target merges, author closes own, no access = the 404 envelope), and
// the conflict → 409 shape. Scope enforcement is TestRouteScopeTable's.
package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
)

// apiCommit writes a FLAT file snapshot as one commit through the
// repo's storer (test seeding — the wire path has its own tests).
func apiCommit(t *testing.T, h *gitAPIHarness, repo dgit.Repo, files map[string]string, msg string, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()
	ctx := context.Background()
	sto, err := h.k.Git.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	tree := object.Tree{}
	for _, name := range names {
		blob := sto.NewEncodedObject()
		blob.SetType(plumbing.BlobObject)
		w, err := blob.Writer()
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(files[name]))
		w.Close()
		bh, err := sto.SetEncodedObject(blob)
		if err != nil {
			t.Fatal(err)
		}
		tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: bh})
	}
	treeObj := sto.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		t.Fatal(err)
	}
	th, err := sto.SetEncodedObject(treeObj)
	if err != nil {
		t.Fatal(err)
	}
	sig := object.Signature{Name: "Test", Email: "test@pcp.local", When: time.Now()}
	commit := object.Commit{Author: sig, Committer: sig, Message: msg + "\n", TreeHash: th, ParentHashes: parents}
	obj := sto.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatal(err)
	}
	ch, err := sto.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := sto.Flush(); err != nil {
		t.Fatal(err)
	}
	return ch
}

func apiSetBranch(t *testing.T, h *gitAPIHarness, repoID, branch string, old, new plumbing.Hash) {
	t.Helper()
	err := h.k.Git.ApplyRefUpdates(context.Background(), repoID, []dgit.RefUpdate{
		{Name: "refs/heads/" + branch, Old: old, New: new},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
}

// mergeAPIFixture: ada owns priv (main + a feature branch one commit
// ahead); dave write, carol read, bob nothing.
func mergeAPIFixture(t *testing.T) (*gitAPIHarness, map[string]string, dgit.Repo, plumbing.Hash, plumbing.Hash) {
	t.Helper()
	h, tokens, repo := issueAPIFixture(t)
	base := apiCommit(t, h, repo, map[string]string{"README.md": "# hi\n"}, "base")
	feature := apiCommit(t, h, repo, map[string]string{"README.md": "# hi\n", "feature.txt": "yes\n"}, "add feature", base)
	apiSetBranch(t, h, repo.ID, "main", plumbing.ZeroHash, base)
	apiSetBranch(t, h, repo.ID, "feature", plumbing.ZeroHash, feature)
	return h, tokens, repo, base, feature
}

func TestMergeAPILifecycle(t *testing.T) {
	h, tok, repo, _, feature := mergeAPIFixture(t)
	base := "/api/v1/git/repos/ada/priv"

	// carol (read) opens the MR (§9 read opens).
	code, body := h.call(t, tok["carol"], "POST", base+"/merges",
		`{"title":"take it","body":"adds feature.txt","sourceBranch":"feature","targetBranch":"main"}`)
	if code != http.StatusCreated || body["number"] != float64(1) || body["state"] != "open" {
		t.Fatalf("create = %d %v", code, body)
	}
	if body["headSha"] != feature.String() {
		t.Fatalf("headSha = %v, want %s", body["headSha"], feature)
	}
	// List.
	code, body = h.call(t, tok["carol"], "GET", base+"/merges", "")
	if code != http.StatusOK || len(body["merges"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, body)
	}
	// Get: merge + comments + mergeability (FF).
	code, body = h.call(t, tok["carol"], "GET", base+"/merges/1", "")
	if code != http.StatusOK {
		t.Fatalf("get = %d %v", code, body)
	}
	check, _ := body["mergeability"].(map[string]any)
	if check == nil || check["mergeable"] != true || check["fastForward"] != true {
		t.Fatalf("mergeability = %v", check)
	}
	// Comment (read role).
	if code, c := h.call(t, tok["carol"], "POST", base+"/merges/1/comments", `{"body":"please"}`); code != http.StatusCreated || c["author"] != "carol" {
		t.Fatalf("comment = %d %v", code, c)
	}
	// Triage: assign + labels (write role).
	if code, _ := h.call(t, tok["dave"], "POST", base+"/merges/1/assignees", `{"username":"carol"}`); code != http.StatusOK {
		t.Fatalf("assign = %d", code)
	}
	if n, _ := h.k.Git.AssignedOpenCount(context.Background(), "carol"); n != 1 {
		t.Fatalf("carol assigned = %d", n)
	}
	code, lbl := h.call(t, tok["ada"], "POST", base+"/labels", `{"name":"review","color":"#6fb6e8"}`)
	if code != http.StatusCreated {
		t.Fatal("label create failed")
	}
	labelID, _ := lbl["id"].(string)
	if code, body := h.call(t, tok["dave"], "PUT", base+"/merges/1/labels", `{"labelIds":["`+labelID+`"]}`); code != http.StatusOK || len(body["labelIds"].([]any)) != 1 {
		t.Fatalf("labels = %d %v", code, body)
	}
	// Merge (dave, write on target): FF; assigned rows unwind.
	code, body = h.call(t, tok["dave"], "POST", base+"/merges/1/merge", "")
	if code != http.StatusOK || body["state"] != "merged" || body["mergedCommit"] != feature.String() {
		t.Fatalf("merge = %d %v", code, body)
	}
	if head, _, _ := h.k.Git.BranchHead(context.Background(), repo.ID, "main"); head != feature {
		t.Fatalf("main = %s, want %s", head, feature)
	}
	if n, _ := h.k.Git.AssignedOpenCount(context.Background(), "carol"); n != 0 {
		t.Errorf("assigned after merge = %d", n)
	}
	// Merged is terminal.
	if code, _ := h.call(t, tok["ada"], "POST", base+"/merges/1/state", `{"state":"closed"}`); code == http.StatusOK {
		t.Error("merged MR changed state")
	}
	// Unassign route shape (fresh MR).
	if code, _ := h.call(t, tok["carol"], "POST", base+"/merges", `{"title":"again","sourceBranch":"feature","targetBranch":"main"}`); code != http.StatusCreated {
		t.Fatal("second create failed")
	}
	if code, _ := h.call(t, tok["dave"], "POST", base+"/merges/2/assignees", `{"username":"carol"}`); code != http.StatusOK {
		t.Fatal("assign 2 failed")
	}
	if code, _ := h.call(t, tok["dave"], "DELETE", base+"/merges/2/assignees/carol", ""); code != http.StatusOK {
		t.Error("unassign failed")
	}
}

func TestMergeAPIRulesAndConflicts(t *testing.T) {
	h, tok, repo, baseCommit, _ := mergeAPIFixture(t)
	base := "/api/v1/git/repos/ada/priv"

	// No access: the 404 envelope on every route (§4.3).
	for _, c := range []struct{ method, path, body string }{
		{"GET", base + "/merges", ""},
		{"POST", base + "/merges", `{"title":"x","sourceBranch":"feature","targetBranch":"main"}`},
		{"GET", base + "/merges/1", ""},
		{"POST", base + "/merges/1/merge", ""},
	} {
		if code, _ := h.call(t, tok["bob"], c.method, c.path, c.body); code != http.StatusNotFound {
			t.Errorf("stranger %s %s = %d, want 404", c.method, c.path, code)
		}
	}

	// carol (read) opens #1; she can't merge (write on target, §9) and
	// can't triage — but closes and reopens her OWN.
	code, body := h.call(t, tok["carol"], "POST", base+"/merges", `{"title":"carol's","sourceBranch":"feature","targetBranch":"main"}`)
	if code != http.StatusCreated {
		t.Fatalf("read-role create = %d %v", code, body)
	}
	n := int(body["number"].(float64))
	if code, _ := h.call(t, tok["carol"], "POST", base+fmt.Sprintf("/merges/%d/merge", n), ""); code != http.StatusNotFound {
		t.Errorf("read-role merge = %d, want 404", code)
	}
	if code, _ := h.call(t, tok["carol"], "POST", base+fmt.Sprintf("/merges/%d/assignees", n), `{"username":"carol"}`); code != http.StatusNotFound {
		t.Errorf("read-role assign = %d, want 404", code)
	}
	if code, _ := h.call(t, tok["carol"], "POST", base+fmt.Sprintf("/merges/%d/state", n), `{"state":"closed"}`); code != http.StatusOK {
		t.Errorf("author close own = %d", code)
	}
	if code, _ := h.call(t, tok["carol"], "POST", base+fmt.Sprintf("/merges/%d/state", n), `{"state":"open"}`); code != http.StatusOK {
		t.Errorf("author reopen own = %d", code)
	}
	// Triage fields on create need write.
	if code, _ := h.call(t, tok["carol"], "POST", base+"/merges", `{"title":"x","sourceBranch":"feature","targetBranch":"main","assignees":["carol"]}`); code != http.StatusForbidden {
		t.Errorf("read-role create with assignees = %d, want 403", code)
	}

	// Conflict: diverge main so both sides touched README.md → 409 with
	// the file list.
	main2 := apiCommit(t, h, repo, map[string]string{"README.md": "# TARGET\n"}, "target edit", baseCommit)
	apiSetBranch(t, h, repo.ID, "main", baseCommit, main2)
	clash := apiCommit(t, h, repo, map[string]string{"README.md": "# SOURCE\n", "feature.txt": "yes\n"}, "clash", baseCommit)
	apiSetBranch(t, h, repo.ID, "clash", plumbing.ZeroHash, clash)
	code, body = h.call(t, tok["ada"], "POST", base+"/merges", `{"title":"clash","sourceBranch":"clash","targetBranch":"main"}`)
	if code != http.StatusCreated {
		t.Fatalf("clash create = %d %v", code, body)
	}
	cn := int(body["number"].(float64))
	// The GET reports the conflict up front…
	code, body = h.call(t, tok["ada"], "GET", base+fmt.Sprintf("/merges/%d", cn), "")
	check, _ := body["mergeability"].(map[string]any)
	if code != http.StatusOK || check == nil || check["mergeable"] != false {
		t.Fatalf("clash get = %d %v", code, body)
	}
	// …and the merge attempt answers 409 naming the file.
	code, body = h.call(t, tok["ada"], "POST", base+fmt.Sprintf("/merges/%d/merge", cn), "")
	if code != http.StatusConflict {
		t.Fatalf("clash merge = %d %v", code, body)
	}
	files, _ := body["conflicts"].([]any)
	if len(files) != 1 || files[0] != "README.md" {
		t.Fatalf("conflicts = %v", body)
	}

	// Bad source repos are unconfirmable.
	if code, _ := h.call(t, tok["ada"], "POST", base+"/merges", `{"title":"x","sourceRepo":"ghost/ghost","sourceBranch":"main","targetBranch":"main"}`); code != http.StatusNotFound {
		t.Errorf("ghost source = %d, want 404", code)
	}
}
