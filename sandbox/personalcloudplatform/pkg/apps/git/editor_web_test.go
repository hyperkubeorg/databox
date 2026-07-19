// editor_web_test.go — the in-service editor's web surface (§16): the
// permission matrix (writers only; tag/sha views 404; anonymous is in
// public_web_test's tables), the editor page rendering the vendored Ace
// assets, commit round trips visible in subsequent blob views
// (create/edit/rename/delete), the conflict re-render preserving the
// user's content, and the vendored asset routes themselves (licensed
// files served, literal routes not shadowed by the {ns} wildcard,
// template URL bases in sync with the embed).
package git

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// editorFixture builds ada/hello with the standard pushed history and
// returns the harness, the signed-in owner, and main's head sha.
func editorFixture(t *testing.T) (*wireHarness, *webUser, string) {
	t.Helper()
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	if _, err := h.k.Git.CreateRepo(context.Background(), dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "hello"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.mux)
	t.Cleanup(srv.Close)
	head := pushFixture(t, h, srv.URL, "ada", mintToken(t, h, "ada"), "ada", "hello")
	return h, ada, head
}

func TestEditorPageRenders(t *testing.T) {
	_, ada, head := editorFixture(t)

	w := ada.get(t, "/git/ada/hello/edit/main/README.md")
	wantMarkers(t, w, "editor page",
		"/git/-/assets/vendor/ace/", "ace.js", "ext-searchbox.js", // vendored editor
		`name="content"`, "hello web", // the file, editable without JS too
		`name="base" value="`+head+`"`, // the CAS anchor
		"Commit to main", "Update README.md", `name="path"`, "ace/theme/pcp")

	// New-file page, directory prefilled.
	wantMarkers(t, ada.get(t, "/git/ada/hello/new/main/docs"), "new-file page",
		`value="docs/"`, "Commit to main", "creating on")

	// Binary blobs bounce to the blob view instead of an editor.
	if w := ada.get(t, "/git/ada/hello/edit/main/bin.dat"); w.Code != http.StatusSeeOther ||
		!strings.Contains(w.Header().Get("Location"), "/blob/main/bin.dat") {
		t.Errorf("binary edit = %d %q, want 303 → blob", w.Code, w.Header().Get("Location"))
	}
}

func TestEditorPermissionMatrix(t *testing.T) {
	h, ada, head := editorFixture(t)
	carol := h.signIn(t, "carol") // read grant
	bob := h.signIn(t, "bob")     // stranger
	ctx := context.Background()
	repo, _, _ := h.k.Git.GetRepoByPath(ctx, "ada", "hello")
	if err := h.k.Git.SetGrant(ctx, repo.ID, dgit.UserSubject("carol"), "read"); err != nil {
		t.Fatal(err)
	}

	// GETs: only writers see the editor; below write it's a plain 404.
	for name, u := range map[string]*webUser{"read-grant": carol, "stranger": bob} {
		for _, p := range []string{"/git/ada/hello/edit/main/README.md", "/git/ada/hello/new/main"} {
			if w := u.get(t, p); w.Code != http.StatusNotFound {
				t.Errorf("%s GET %s = %d, want 404", name, p, w.Code)
			}
		}
	}
	// POSTs: same rule, and nothing moves.
	form := url.Values{"base": {head}, "path": {"x.txt"}, "content": {"x"}, "message": {"x"}}
	for name, u := range map[string]*webUser{"read-grant": carol, "stranger": bob} {
		if w := u.post(t, "/git/ada/hello/new/main", form); w.Code != http.StatusNotFound {
			t.Errorf("%s POST new = %d, want 404", name, w.Code)
		}
	}
	if cur, _, _ := h.k.Git.BranchHead(ctx, repo.ID, "main"); cur.String() != head {
		t.Fatal("a forbidden POST moved the branch")
	}

	// Tag and sha refs are not editable — GET and POST alike 404 (§16).
	for _, p := range []string{
		"/git/ada/hello/edit/v1/README.md",
		"/git/ada/hello/edit/" + head + "/README.md",
		"/git/ada/hello/new/v1",
	} {
		if w := ada.get(t, p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (branches only)", p, w.Code)
		}
		if w := ada.post(t, p, form); w.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404 (branches only)", p, w.Code)
		}
	}

	// The blob view's affordances follow the same rule: Edit on a branch
	// for writers, a disabled hint on a tag, nothing below write.
	wantMarkers(t, ada.get(t, "/git/ada/hello/blob/main/README.md"), "writer blob",
		"/git/ada/hello/edit/main/README.md", "Delete", "delDialog")
	if body := ada.get(t, "/git/ada/hello/blob/v1/README.md").Body.String(); strings.Contains(body, "/edit/v1/") ||
		!strings.Contains(body, "Switch to a branch") {
		t.Error("tag blob view must hint, not link, the editor")
	}
	if body := carol.get(t, "/git/ada/hello/blob/main/README.md").Body.String(); strings.Contains(body, "/edit/main/") ||
		strings.Contains(body, "delDialog") {
		t.Error("read-grant blob view leaks editor affordances")
	}
	if body := carol.get(t, "/git/ada/hello").Body.String(); strings.Contains(body, "New file") {
		t.Error("read-grant repo home leaks the New file button")
	}
}

func TestEditorCommitRoundTrips(t *testing.T) {
	h, ada, head := editorFixture(t)
	ctx := context.Background()
	repo, _, _ := h.k.Git.GetRepoByPath(ctx, "ada", "hello")

	// Edit README through the form → visible in the next blob view.
	w := ada.post(t, "/git/ada/hello/edit/main/README.md", url.Values{
		"base": {head}, "path": {"README.md"},
		"content": {"# edited in the browser\n"}, "message": {"web edit"},
	})
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "/blob/main/README.md") {
		t.Fatalf("edit POST = %d %q", w.Code, w.Header().Get("Location"))
	}
	wantMarkers(t, ada.get(t, "/git/ada/hello/blob/main/README.md?plain=1"), "blob after edit", "edited in the browser")

	// Create a Go file → the blob view carries the highlight hook
	// (data-lang from the extension whitelist) and the vendored script.
	head2, _, _ := h.k.Git.BranchHead(ctx, repo.ID, "main")
	w = ada.post(t, "/git/ada/hello/new/main", url.Values{
		"base": {head2.String()}, "path": {"cmd/tool/main.go"},
		"content": {"package main\n\nfunc main() {}\n"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create POST = %d (%s)", w.Code, w.Body.String())
	}
	wantMarkers(t, ada.get(t, "/git/ada/hello/blob/main/cmd/tool/main.go"), "highlighted blob",
		`data-lang="go"`, "package main", "/git/-/assets/vendor/highlightjs/", "highlight.min.js")

	// Rename in the same commit: change the path field.
	head3, _, _ := h.k.Git.BranchHead(ctx, repo.ID, "main")
	w = ada.post(t, "/git/ada/hello/edit/main/docs/guide.md", url.Values{
		"base": {head3.String()}, "path": {"docs/handbook.md"},
		"content": {"# handbook\n"}, "message": {"rename the guide"},
	})
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "/blob/main/docs/handbook.md") {
		t.Fatalf("rename POST = %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := ada.get(t, "/git/ada/hello/blob/main/docs/guide.md"); w.Code != http.StatusNotFound {
		t.Error("old path survived the rename")
	}
	wantMarkers(t, ada.get(t, "/git/ada/hello/blob/main/docs/handbook.md"), "renamed blob", "handbook")

	// Delete through the confirm form → back to the tree, file gone,
	// history intact (the commit log grew).
	head4, _, _ := h.k.Git.BranchHead(ctx, repo.ID, "main")
	w = ada.post(t, "/git/ada/hello/edit/main/docs/handbook.md", url.Values{
		"op": {"delete"}, "base": {head4.String()}, "message": {"drop the handbook"},
	})
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "/tree/main/docs") {
		t.Fatalf("delete POST = %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := ada.get(t, "/git/ada/hello/blob/main/docs/handbook.md"); w.Code != http.StatusNotFound {
		t.Error("deleted file still serves")
	}
	wantMarkers(t, ada.get(t, "/git/ada/hello/commits/main"), "history", "drop the handbook", "rename the guide", "web edit")
}

func TestEditorConflictPreservesContent(t *testing.T) {
	h, ada, head := editorFixture(t)
	ctx := context.Background()
	repo, _, _ := h.k.Git.GetRepoByPath(ctx, "ada", "hello")

	// Someone else lands an edit on README while our page is open.
	if _, err := h.k.Git.WebCommit(ctx, site.Config{}, repo, dgit.WebCommitInput{
		Branch: "main", BaseSHA: head,
		OldPath: "README.md", NewPath: "README.md",
		Content: []byte("# theirs\n"), Author: "ada",
	}); err != nil {
		t.Fatal(err)
	}
	// Our save with the STALE base: no redirect — the editor re-renders
	// with our content intact and the friendly message.
	w := ada.post(t, "/git/ada/hello/edit/main/README.md", url.Values{
		"base": {head}, "path": {"README.md"},
		"content": {"MY CAREFULLY TYPED WORK\n"}, "message": {"mine"},
	})
	wantMarkers(t, w, "conflict re-render",
		"MY CAREFULLY TYPED WORK", "branch moved", `name="content"`, "Commit to main")
	if got, _, _ := h.k.Git.BranchHead(ctx, repo.ID, "main"); got.String() == head {
		t.Error("nothing should have reset the branch") // sanity: their commit stands
	}
	// The untouched-path flavor rebases instead: a new file with the
	// same stale base just lands.
	w = ada.post(t, "/git/ada/hello/new/main", url.Values{
		"base": {head}, "path": {"fresh.txt"}, "content": {"rebased\n"},
	})
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "/blob/main/fresh.txt") {
		t.Fatalf("rebase POST = %d %q", w.Code, w.Header().Get("Location"))
	}
	wantMarkers(t, ada.get(t, "/git/ada/hello/blob/main/README.md?plain=1"), "concurrent edit kept", "theirs")
}

func TestEditorEmptyRepoFirstFile(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	if _, err := h.k.Git.CreateRepo(context.Background(), dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "fresh"}); err != nil {
		t.Fatal(err)
	}
	// The quick-setup block offers the browser path.
	wantMarkers(t, ada.get(t, "/git/ada/fresh"), "empty home", "create a file in the browser", "/git/ada/fresh/new/main")
	wantMarkers(t, ada.get(t, "/git/ada/fresh/new/main"), "empty-repo editor", "Commit to main")
	w := ada.post(t, "/git/ada/fresh/new/main", url.Values{
		"base": {""}, "path": {"README.md"}, "content": {"# first\n"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("first-file POST = %d (%s)", w.Code, w.Body.String())
	}
	wantMarkers(t, ada.get(t, "/git/ada/fresh"), "born repo home", "first", "README.md")
}

func TestGitAssets(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")

	// The template URL bases must point INTO the embed — a vendor bump
	// that forgets one side fails here.
	for _, base := range []string{hljsAssetBase, aceAssetBase} {
		if !strings.HasPrefix(base, assetRoutePrefix) {
			t.Fatalf("asset base %q outside %q", base, assetRoutePrefix)
		}
	}
	files := []string{
		hljsAssetBase + "/highlight.min.js",
		hljsAssetBase + "/dockerfile.min.js",
		hljsAssetBase + "/LICENSE",
		hljsAssetBase + "/VENDOR.md",
		aceAssetBase + "/ace.js",
		aceAssetBase + "/ext-searchbox.js",
		aceAssetBase + "/mode-golang.js",
		aceAssetBase + "/mode-c_cpp.js",
		aceAssetBase + "/LICENSE",
		aceAssetBase + "/VENDOR.md",
	}
	for _, p := range files {
		w := ada.get(t, p)
		if w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Errorf("GET %s = %d (%d bytes), want 200", p, w.Code, w.Body.Len())
			continue
		}
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s Cache-Control = %q, want immutable (version rides the path)", p, cc)
		}
		if strings.HasSuffix(p, ".js") {
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
				t.Errorf("%s Content-Type = %q", p, ct)
			}
		}
	}
	// Both LICENSE files are the real BSD-3-Clause text.
	for _, p := range []string{hljsAssetBase + "/LICENSE", aceAssetBase + "/LICENSE"} {
		if body := ada.get(t, p).Body.String(); !strings.Contains(body, "Redistribution and use in source and binary forms") {
			t.Errorf("%s is not the upstream license text", p)
		}
	}

	// "-" is reserved (§3.1/§16), so the literal route can't be shadowed
	// — and a namespace probe under it stays a 404, not a repo page.
	if !dgit.IsReservedName("-") {
		t.Fatal(`"-" must be a reserved namespace name`)
	}
	if w := ada.get(t, "/git/-"); w.Code != http.StatusNotFound {
		t.Errorf("GET /git/- = %d, want 404", w.Code)
	}
	if w := ada.get(t, "/git/-/assets/vendor/nope.js"); w.Code != http.StatusNotFound {
		t.Errorf("unknown asset = %d, want 404", w.Code)
	}

	// The §2 gate covers assets too: disabled Git Services is unbuilt.
	if err := h.k.Site.Update(context.Background(), func(c *site.Config) error {
		c.Git.Enabled = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := ada.get(t, files[0]); w.Code != http.StatusNotFound {
		t.Errorf("gate-off asset = %d, want 404", w.Code)
	}
}

// TestGitAssetsAnonymous — public pages need the highlight scripts, so
// the asset routes follow the §10 anonymous rules exactly: served while
// public repos are allowed, 404 while they're not.
func TestGitAssetsAnonymous(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	asset := hljsAssetBase + "/highlight.min.js"
	if w := anonGet(h, asset); w.Code != http.StatusOK {
		t.Errorf("anonymous asset with public allowed = %d, want 200", w.Code)
	}
	if err := h.k.Site.Update(context.Background(), func(c *site.Config) error {
		c.Git.PublicReposDisabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := anonGet(h, asset); w.Code != http.StatusNotFound {
		t.Errorf("anonymous asset with public disallowed = %d, want 404", w.Code)
	}
}

// TestFileTemplatePCPBuilder locks the ?template=pcp-builder starter: the
// right filename and a non-trivial body the new-file editor prefills.
func TestFileTemplatePCPBuilder(t *testing.T) {
	name, body, ok := fileTemplate("pcp-builder")
	if !ok || name != ".pcp-builder.yaml" {
		t.Fatalf("pcp-builder template = %q,%v; want .pcp-builder.yaml,true", name, ok)
	}
	for _, want := range []string{"phases:", "pipeline:", "requiresPhase", "artifacts:"} {
		if !strings.Contains(body, want) {
			t.Errorf("template body missing %q", want)
		}
	}
	if _, _, ok := fileTemplate("nope"); ok {
		t.Error("unknown template key should return ok=false")
	}
}
