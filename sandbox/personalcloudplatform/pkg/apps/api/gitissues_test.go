// gitissues_test.go — the /api/v1/git issue slice (§8/§12): lifecycle
// (create/list/get/comment/label/assign/state), the §8 rules over
// bearer auth (read opens+comments, write triages, author closes own,
// no access = the 404 envelope), and filters. Scope enforcement is
// TestRouteScopeTable's job.
package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// issueAPIFixture: ada owns priv; dave write, carol read, bob nothing.
func issueAPIFixture(t *testing.T) (*gitAPIHarness, map[string]string, dgit.Repo) {
	t.Helper()
	h := newGitAPIHarness(t)
	ctx := context.Background()
	if err := h.k.Site.Update(ctx, func(c *site.Config) error { c.Git.Enabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{}
	for _, u := range []string{"ada", "bob", "carol", "dave"} {
		tokens[u] = h.user(t, u)
	}
	repo, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "priv"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.k.Git.SetGrant(ctx, repo.ID, dgit.UserSubject("dave"), "write"); err != nil {
		t.Fatal(err)
	}
	if err := h.k.Git.SetGrant(ctx, repo.ID, dgit.UserSubject("carol"), "read"); err != nil {
		t.Fatal(err)
	}
	return h, tokens, repo
}

func TestIssueAPILifecycle(t *testing.T) {
	h, tok, _ := issueAPIFixture(t)
	base := "/api/v1/git/repos/ada/priv"

	// Create with triage fields (owner).
	code, body := h.call(t, tok["ada"], "POST", base+"/issues", `{"title":"roof leaks","body":"see attic","assignees":["dave"]}`)
	if code != http.StatusCreated || body["number"] != float64(1) || body["state"] != "open" {
		t.Fatalf("create = %d %v", code, body)
	}
	// Labels: create then apply.
	code, lbl := h.call(t, tok["ada"], "POST", base+"/labels", `{"name":"bug","color":"#e8746b"}`)
	if code != http.StatusCreated {
		t.Fatalf("label create = %d %v", code, lbl)
	}
	labelID, _ := lbl["id"].(string)
	code, body = h.call(t, tok["ada"], "PUT", base+"/issues/1/labels", `{"labelIds":["`+labelID+`"]}`)
	if code != http.StatusOK || len(body["labelIds"].([]any)) != 1 {
		t.Fatalf("labels put = %d %v", code, body)
	}
	// Comment (read-role carol may).
	code, body = h.call(t, tok["carol"], "POST", base+"/issues/1/comments", `{"body":"same here"}`)
	if code != http.StatusCreated || body["author"] != "carol" {
		t.Fatalf("comment = %d %v", code, body)
	}
	commentID, _ := body["id"].(string)
	// Edit own; someone else's edit 404s.
	if code, _ := h.call(t, tok["carol"], "PATCH", base+"/issues/1/comments/"+commentID, `{"body":"same here!"}`); code != http.StatusOK {
		t.Fatalf("own edit = %d", code)
	}
	if code, _ := h.call(t, tok["dave"], "PATCH", base+"/issues/1/comments/"+commentID, `{"body":"hijack"}`); code != http.StatusNotFound {
		t.Fatalf("foreign edit = %d, want 404", code)
	}
	// Get: issue + comments.
	code, body = h.call(t, tok["carol"], "GET", base+"/issues/1", "")
	issue, _ := body["issue"].(map[string]any)
	comments, _ := body["comments"].([]any)
	if code != http.StatusOK || issue["comments"] != float64(1) || len(comments) != 1 {
		t.Fatalf("get = %d %v", code, body)
	}
	// Assign / unassign (write role).
	if code, _ := h.call(t, tok["dave"], "POST", base+"/issues/1/assignees", `{"username":"carol"}`); code != http.StatusOK {
		t.Fatalf("assign = %d", code)
	}
	if n, _ := h.k.Git.AssignedOpenCount(context.Background(), "carol"); n != 1 {
		t.Fatalf("carol assigned = %d", n)
	}
	if code, _ := h.call(t, tok["dave"], "DELETE", base+"/issues/1/assignees/carol", ""); code != http.StatusOK {
		t.Fatalf("unassign = %d", code)
	}
	// State: close.
	code, body = h.call(t, tok["ada"], "POST", base+"/issues/1/state", `{"state":"closed"}`)
	if code != http.StatusOK || body["state"] != "closed" {
		t.Fatalf("close = %d %v", code, body)
	}
	// List filters: open empty, closed has it, label filter matches.
	code, body = h.call(t, tok["ada"], "GET", base+"/issues", "")
	if code != http.StatusOK || len(body["issues"].([]any)) != 0 {
		t.Fatalf("open list = %d %v", code, body)
	}
	code, body = h.call(t, tok["ada"], "GET", base+"/issues?state=closed&label="+labelID, "")
	if code != http.StatusOK || len(body["issues"].([]any)) != 1 {
		t.Fatalf("closed+label list = %d %v", code, body)
	}
	// Delete the comment (repo-write may delete any).
	if code, _ := h.call(t, tok["dave"], "DELETE", base+"/issues/1/comments/"+commentID, ""); code != http.StatusOK {
		t.Fatalf("write delete comment = %d", code)
	}
	// Label delete.
	if code, _ := h.call(t, tok["ada"], "DELETE", base+"/labels/"+labelID, ""); code != http.StatusOK {
		t.Fatalf("label delete = %d", code)
	}
}

func TestIssueAPIRules(t *testing.T) {
	h, tok, _ := issueAPIFixture(t)
	base := "/api/v1/git/repos/ada/priv"

	// No access: every route answers the 404 envelope (§4.3).
	for _, c := range []struct{ method, path, body string }{
		{"GET", base + "/issues", ""},
		{"POST", base + "/issues", `{"title":"x"}`},
		{"GET", base + "/issues/1", ""},
		{"POST", base + "/issues/1/comments", `{"body":"x"}`},
		{"GET", base + "/labels", ""},
	} {
		if code, _ := h.call(t, tok["bob"], c.method, c.path, c.body); code != http.StatusNotFound {
			t.Errorf("stranger %s %s = %d, want 404", c.method, c.path, code)
		}
	}

	// Read may create — but triage fields need write.
	code, _ := h.call(t, tok["carol"], "POST", base+"/issues", `{"title":"carol's","assignees":["carol"]}`)
	if code != http.StatusForbidden {
		t.Errorf("read-role create with assignees = %d, want 403", code)
	}
	code, body := h.call(t, tok["carol"], "POST", base+"/issues", `{"title":"carol's"}`)
	if code != http.StatusCreated {
		t.Fatalf("read-role create = %d %v", code, body)
	}
	n := int(body["number"].(float64))

	// Read can't triage.
	if code, _ := h.call(t, tok["carol"], "PUT", base+fmt.Sprintf("/issues/%d/labels", n), `{"labelIds":[]}`); code != http.StatusNotFound {
		t.Errorf("read-role labels = %d, want 404", code)
	}
	if code, _ := h.call(t, tok["carol"], "POST", base+fmt.Sprintf("/issues/%d/assignees", n), `{"username":"carol"}`); code != http.StatusNotFound {
		t.Errorf("read-role assign = %d, want 404", code)
	}
	if code, _ := h.call(t, tok["carol"], "POST", base+"/labels", `{"name":"x","color":"#ffffff"}`); code != http.StatusNotFound {
		t.Errorf("read-role label create = %d, want 404", code)
	}

	// Author closes own at read role; another read-role user can't.
	if code, _ := h.call(t, tok["carol"], "POST", base+fmt.Sprintf("/issues/%d/state", n), `{"state":"closed"}`); code != http.StatusOK {
		t.Errorf("author close own = %d", code)
	}
	code, body = h.call(t, tok["dave"], "POST", base+"/issues", `{"title":"dave's"}`)
	if code != http.StatusCreated {
		t.Fatal("dave create failed")
	}
	n2 := int(body["number"].(float64))
	if code, _ := h.call(t, tok["carol"], "POST", base+fmt.Sprintf("/issues/%d/state", n2), `{"state":"closed"}`); code != http.StatusNotFound {
		t.Errorf("read-role close of another's = %d, want 404", code)
	}
	// Write closes anything.
	if code, _ := h.call(t, tok["dave"], "POST", base+fmt.Sprintf("/issues/%d/state", n2), `{"state":"closed"}`); code != http.StatusOK {
		t.Errorf("write close = %d", code)
	}

	// Gate off: unbuilt (§2).
	if err := h.k.Site.Update(context.Background(), func(c *site.Config) error { c.Git.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if code, body := h.call(t, tok["ada"], "GET", base+"/issues", ""); code != http.StatusNotFound || !strings.Contains(fmt.Sprint(body), "not_found") {
		t.Errorf("gate-off = %d %v", code, body)
	}
}
