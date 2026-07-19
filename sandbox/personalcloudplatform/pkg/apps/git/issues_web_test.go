// issues_web_test.go — the §8 web surface end to end through the real
// router: the issue lifecycle (create → list → view → comment → label →
// assign → close), the rendered #N autolink, the dashboard "Assigned to
// you" section, notification bells, and the permission matrix (owner /
// write / read / none / private-404) across pages AND mutations.
package git

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
)

func TestIssueLifecycleWeb(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	bob := h.signIn(t, "bob")
	ctx := context.Background()
	repo, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.k.Git.SetGrant(ctx, repo.ID, dgit.UserSubject("bob"), "read"); err != nil {
		t.Fatal(err)
	}

	// Empty list + the Issues tab in the shell.
	wantMarkers(t, ada.get(t, "/git/ada/hello/issues"), "empty issues",
		"No open issues", "New issue", "Open (0)", "Closed (0)")
	wantMarkers(t, ada.get(t, "/git/ada/hello"), "repo home tab", "/git/ada/hello/issues")

	// Create #1 and #2; #2's body references #1 and mentions bob.
	if w := ada.post(t, "/git/ada/hello/issues/create", url.Values{"title": {"roof leaks"}, "body": {"water everywhere"}}); w.Code != http.StatusSeeOther || !strings.HasSuffix(w.Header().Get("Location"), "/issues/1") {
		t.Fatalf("create #1 = %d loc %q", w.Code, w.Header().Get("Location"))
	}
	if w := ada.post(t, "/git/ada/hello/issues/create", url.Values{"title": {"follow-up"}, "body": {"see #1 — @bob can you look?"}}); !strings.HasSuffix(w.Header().Get("Location"), "/issues/2") {
		t.Fatalf("create #2 loc %q", w.Header().Get("Location"))
	}
	// The mention raised bob's bell.
	if rows, _ := h.k.Notifs.List(ctx, "bob", 10); len(rows) != 1 || !strings.Contains(rows[0].Text, "mentioned you") {
		t.Errorf("bob bells after mention = %+v", rows)
	}

	// View #2: autolink + mention link render; the shell badge counts 2.
	wantMarkers(t, ada.get(t, "/git/ada/hello/issues/2"), "issue view",
		`<a href="/git/ada/hello/issues/1">#1</a>`, `<a href="/git/bob">@bob</a>`,
		"follow-up", "open", `class="gitcount">2<`)

	// bob (read role) comments; ada gets the bell.
	if w := bob.post(t, "/git/ada/hello/issues/1/comment", url.Values{"body": {"same in my room"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("read-role comment = %d", w.Code)
	}
	wantMarkers(t, ada.get(t, "/git/ada/hello/issues/1"), "comment thread", "same in my room", "@bob")
	if rows, _ := h.k.Notifs.List(ctx, "ada", 10); len(rows) == 0 || !strings.Contains(rows[0].Text, "commented on") {
		t.Errorf("ada bells after comment = %+v", rows)
	}

	// bob edits his own comment inline; deletes it later is covered in
	// the permission matrix — here the happy path.
	comments, _ := h.k.Git.ListComments(ctx, repo.ID, 1)
	if len(comments) != 1 {
		t.Fatalf("comments = %d", len(comments))
	}
	if w := bob.post(t, "/git/ada/hello/issues/1/comment/edit", url.Values{"id": {comments[0].ID}, "body": {"same in my room (edited)"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("own edit = %d", w.Code)
	}
	wantMarkers(t, bob.get(t, "/git/ada/hello/issues/1"), "edited comment", "(edited)", "edited")

	// Labels: create, apply to #1, filter the list.
	if w := ada.post(t, "/git/ada/hello/labels/create", url.Values{"name": {"bug"}, "color": {"#e8746b"}}); !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatalf("label create loc %q", w.Header().Get("Location"))
	}
	labels, _ := h.k.Git.ListLabels(ctx, repo.ID)
	if len(labels) != 1 {
		t.Fatalf("labels = %+v", labels)
	}
	if w := ada.post(t, "/git/ada/hello/issues/1/labels", url.Values{"label": {labels[0].ID}}); w.Code != http.StatusSeeOther {
		t.Fatalf("apply label = %d", w.Code)
	}
	filtered := ada.get(t, "/git/ada/hello/issues?label="+labels[0].ID)
	if body := filtered.Body.String(); !strings.Contains(body, "roof leaks") || strings.Contains(body, "follow-up") {
		t.Errorf("label filter wrong: has-roof=%v has-followup=%v",
			strings.Contains(body, "roof leaks"), strings.Contains(body, "follow-up"))
	}

	// Assign bob: bell + dashboard "Assigned to you" + open count.
	if w := ada.post(t, "/git/ada/hello/issues/1/assign", url.Values{"username": {"bob"}, "on": {"1"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("assign = %d", w.Code)
	}
	if n, _ := h.k.Git.AssignedOpenCount(ctx, "bob"); n != 1 {
		t.Fatalf("bob assigned count = %d", n)
	}
	wantMarkers(t, bob.get(t, "/git"), "dashboard assigned", "Assigned to you", "roof leaks", "/git/ada/hello/issues/1")

	// Close #1 (owner): the assigned count drops, list moves.
	if w := ada.post(t, "/git/ada/hello/issues/1/state", url.Values{"state": {"closed"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("close = %d", w.Code)
	}
	if n, _ := h.k.Git.AssignedOpenCount(ctx, "bob"); n != 0 {
		t.Errorf("assigned count after close = %d", n)
	}
	wantMarkers(t, ada.get(t, "/git/ada/hello/issues?state=closed"), "closed list", "roof leaks", "Closed (1)")
	if body := ada.get(t, "/git/ada/hello/issues").Body.String(); strings.Contains(body, "roof leaks") {
		t.Error("closed issue still on the open tab")
	}
}

func TestIssuePermissionMatrixWeb(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")     // owner (admin)
	dave := h.signIn(t, "dave")   // write grant
	carol := h.signIn(t, "carol") // read grant
	bob := h.signIn(t, "bob")     // no access
	ctx := context.Background()
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
	// carol (read) opens an issue — §8's public-reporting rule.
	if w := carol.post(t, "/git/ada/priv/issues/create", url.Values{"title": {"carol's issue"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("read-role create = %d", w.Code)
	}

	// Pages: reader roles 200; stranger 404 (never 403, §4.3).
	for _, p := range []string{"/git/ada/priv/issues", "/git/ada/priv/issues/new", "/git/ada/priv/issues/1"} {
		for name, u := range map[string]*webUser{"owner": ada, "write": dave, "read": carol} {
			if w := u.get(t, p); w.Code != http.StatusOK {
				t.Errorf("%s %s = %d, want 200", name, p, w.Code)
			}
		}
		if w := bob.get(t, p); w.Code != http.StatusNotFound {
			t.Errorf("stranger %s = %d, want 404", p, w.Code)
		}
	}

	// Triage (labels/assign/label CRUD): write yes, read 404.
	if w := ada.post(t, "/git/ada/priv/labels/create", url.Values{"name": {"bug"}, "color": {"#e8746b"}}); !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatalf("owner label create loc %q", w.Header().Get("Location"))
	}
	if w := carol.post(t, "/git/ada/priv/labels/create", url.Values{"name": {"nope"}, "color": {"#ffffff"}}); w.Code != http.StatusNotFound {
		t.Errorf("read-role label create = %d, want 404", w.Code)
	}
	if w := carol.post(t, "/git/ada/priv/issues/1/assign", url.Values{"username": {"carol"}, "on": {"1"}}); w.Code != http.StatusNotFound {
		t.Errorf("read-role assign = %d, want 404", w.Code)
	}
	if w := dave.post(t, "/git/ada/priv/issues/1/assign", url.Values{"username": {"carol"}, "on": {"1"}}); w.Code != http.StatusSeeOther {
		t.Errorf("write-role assign = %d", w.Code)
	}
	if w := bob.post(t, "/git/ada/priv/issues/1/comment", url.Values{"body": {"hi"}}); w.Code != http.StatusNotFound {
		t.Errorf("stranger comment = %d, want 404", w.Code)
	}

	// Close/reopen: the author (read role) may on their OWN issue; a
	// read-role non-author 404s; write closes anything.
	if w := carol.post(t, "/git/ada/priv/issues/1/state", url.Values{"state": {"closed"}}); w.Code != http.StatusSeeOther {
		t.Errorf("author close own = %d", w.Code)
	}
	if w := carol.post(t, "/git/ada/priv/issues/1/state", url.Values{"state": {"open"}}); w.Code != http.StatusSeeOther {
		t.Errorf("author reopen own = %d", w.Code)
	}
	if w := dave.post(t, "/git/ada/priv/issues/create", url.Values{"title": {"dave's"}}); w.Code != http.StatusSeeOther {
		t.Fatal("dave create failed")
	}
	if w := carol.post(t, "/git/ada/priv/issues/2/state", url.Values{"state": {"closed"}}); w.Code != http.StatusNotFound {
		t.Errorf("read-role close of another's issue = %d, want 404", w.Code)
	}
	if w := dave.post(t, "/git/ada/priv/issues/1/state", url.Values{"state": {"closed"}}); w.Code != http.StatusSeeOther {
		t.Errorf("write-role close any = %d", w.Code)
	}

	// Comments: edit own only; repo-write deletes any.
	if w := carol.post(t, "/git/ada/priv/issues/2/comment", url.Values{"body": {"carol's words"}}); w.Code != http.StatusSeeOther {
		t.Fatal("carol comment failed")
	}
	comments, _ := h.k.Git.ListComments(ctx, repo.ID, 2)
	if len(comments) != 1 {
		t.Fatalf("comments = %d", len(comments))
	}
	id := comments[0].ID
	if w := dave.post(t, "/git/ada/priv/issues/2/comment/edit", url.Values{"id": {id}, "body": {"hijack"}}); w.Code != http.StatusNotFound {
		t.Errorf("edit another's comment = %d, want 404", w.Code)
	}
	if w := carol.post(t, "/git/ada/priv/issues/2/comment/delete", url.Values{"id": {id}}); w.Code != http.StatusSeeOther {
		t.Errorf("delete own comment = %d", w.Code)
	}
	if w := carol.post(t, "/git/ada/priv/issues/2/comment", url.Values{"body": {"again"}}); w.Code != http.StatusSeeOther {
		t.Fatal("carol recomment failed")
	}
	comments, _ = h.k.Git.ListComments(ctx, repo.ID, 2)
	if w := dave.post(t, "/git/ada/priv/issues/2/comment/delete", url.Values{"id": {comments[0].ID}}); w.Code != http.StatusSeeOther {
		t.Errorf("repo-write delete any = %d", w.Code)
	}
}
