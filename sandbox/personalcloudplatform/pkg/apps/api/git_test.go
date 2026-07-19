// git_test.go — the /api/v1/git slice (Draft 002 §12): repo CRUD +
// settings + grants, orgs/members/teams, profile, the §2 gate, and the
// §4.3 rule (private-no-access answers the 404 envelope, never 403).
// Scope enforcement itself is covered by TestRouteScopeTable.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// gitAPIHarness is a full router with the git domain wired.
type gitAPIHarness struct {
	k   *kernel.App
	mux http.Handler
}

func newGitAPIHarness(t *testing.T) *gitAPIHarness {
	t.Helper()
	db := kvxtest.New(t)
	userStore := &users.Store{DB: db, SessionTTL: time.Hour}
	k := &kernel.App{
		Users:        userStore,
		Site:         &site.Store{DB: db},
		APIKeys:      &apikeys.Store{DB: db},
		Git:          &dgit.Store{DB: db, Users: userStore},
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		DefaultQuota: 10 << 30,
	}
	mux, err := k.Router(Mount(k, nil))
	if err != nil {
		t.Fatal(err)
	}
	return &gitAPIHarness{k: k, mux: mux}
}

func (h *gitAPIHarness) user(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := h.k.Users.CreateUser(ctx, name, name, "password123"); err != nil {
		t.Fatal(err)
	}
	token, _, err := h.k.APIKeys.Mint(ctx, name, "t",
		[]string{apikeys.ScopeGitRead, apikeys.ScopeGitWrite}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// call runs one bearer request and decodes the JSON body.
func (h *gitAPIHarness) call(t *testing.T, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rdr)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestGitAPIGate(t *testing.T) {
	h := newGitAPIHarness(t)
	token := h.user(t, "ada")
	// Disabled: JSON 404 envelope, indistinguishable from unbuilt (§2).
	code, body := h.call(t, token, "GET", "/api/v1/git/repos", "")
	if code != http.StatusNotFound || body["code"] != "not_found" {
		t.Fatalf("gate-off = %d %v, want the 404 envelope", code, body)
	}
}

func TestGitAPIRepos(t *testing.T) {
	h := newGitAPIHarness(t)
	ctx := context.Background()
	ada := h.user(t, "ada")
	bob := h.user(t, "bob")
	if err := h.k.Site.Update(ctx, func(c *site.Config) error { c.Git.Enabled = true; return nil }); err != nil {
		t.Fatal(err)
	}

	// Create.
	code, res := h.call(t, ada, "POST", "/api/v1/git/repos",
		`{"name":"tools","description":"cli things","visibility":"private","initReadme":true}`)
	if code != http.StatusCreated || res["fullName"] != "ada/tools" || res["defaultBranch"] != "main" {
		t.Fatalf("create = %d %v", code, res)
	}

	// Get: owner sees role admin; stranger gets the 404 envelope (§4.3).
	code, res = h.call(t, ada, "GET", "/api/v1/git/repos/ada/tools", "")
	if code != http.StatusOK || res["role"] != "admin" {
		t.Fatalf("owner get = %d %v", code, res)
	}
	code, res = h.call(t, bob, "GET", "/api/v1/git/repos/ada/tools", "")
	if code != http.StatusNotFound || res["code"] != "not_found" {
		t.Fatalf("stranger get = %d %v, want 404 envelope", code, res)
	}

	// List: owner sees it under repos.
	code, res = h.call(t, ada, "GET", "/api/v1/git/repos", "")
	if code != http.StatusOK || !strings.Contains(recode(t, res), "ada/tools") {
		t.Fatalf("list = %d %v", code, res)
	}

	// Patch: description + visibility (public allowed by default).
	code, res = h.call(t, ada, "PATCH", "/api/v1/git/repos/ada/tools",
		`{"description":"updated","visibility":"public"}`)
	if code != http.StatusOK || res["description"] != "updated" || res["visibility"] != "public" {
		t.Fatalf("patch = %d %v", code, res)
	}
	// A public repo reads for everyone.
	if code, _ := h.call(t, bob, "GET", "/api/v1/git/repos/ada/tools", ""); code != http.StatusOK {
		t.Fatalf("public get = %d", code)
	}

	// Grants CRUD.
	code, res = h.call(t, ada, "PUT", "/api/v1/git/repos/ada/tools/grants", `{"username":"bob","role":"write"}`)
	if code != http.StatusOK || res["subject"] != "u:bob" {
		t.Fatalf("grant put = %d %v", code, res)
	}
	code, res = h.call(t, ada, "GET", "/api/v1/git/repos/ada/tools/grants", "")
	if code != http.StatusOK || !strings.Contains(recode(t, res), `"u:bob"`) {
		t.Fatalf("grants list = %d %v", code, res)
	}
	// Grant listing is admin-gated: bob (write) gets the 404 envelope.
	if code, _ := h.call(t, bob, "GET", "/api/v1/git/repos/ada/tools/grants", ""); code != http.StatusNotFound {
		t.Fatalf("non-admin grants list = %d, want 404", code)
	}
	if code, _ := h.call(t, ada, "DELETE", "/api/v1/git/repos/ada/tools/grants/u:bob", ""); code != http.StatusOK {
		t.Fatalf("grant delete = %d", code)
	}

	// Fork (bob may read the public repo), then delete: the parent is
	// fork-blocked, the fork deletes, then the parent goes.
	code, res = h.call(t, bob, "POST", "/api/v1/git/repos/ada/tools/fork", `{"name":"tools"}`)
	if code != http.StatusCreated || res["fullName"] != "bob/tools" {
		t.Fatalf("fork = %d %v", code, res)
	}
	if code, res = h.call(t, ada, "DELETE", "/api/v1/git/repos/ada/tools", ""); code == http.StatusOK {
		t.Fatalf("fork-blocked delete = %d %v, want failure", code, res)
	}
	if code, _ = h.call(t, bob, "DELETE", "/api/v1/git/repos/bob/tools", ""); code != http.StatusOK {
		t.Fatalf("fork delete = %d", code)
	}
	if code, _ = h.call(t, ada, "DELETE", "/api/v1/git/repos/ada/tools", ""); code != http.StatusOK {
		t.Fatalf("parent delete = %d", code)
	}
}

func TestGitAPIOrgsAndProfile(t *testing.T) {
	h := newGitAPIHarness(t)
	ctx := context.Background()
	ada := h.user(t, "ada")
	bob := h.user(t, "bob")
	carol := h.user(t, "carol")
	if err := h.k.Site.Update(ctx, func(c *site.Config) error { c.Git.Enabled = true; return nil }); err != nil {
		t.Fatal(err)
	}

	// Org create + get + settings.
	code, res := h.call(t, ada, "POST", "/api/v1/git/orgs", `{"name":"acme","description":"the org"}`)
	if code != http.StatusCreated || res["name"] != "acme" || res["role"] != "owner" {
		t.Fatalf("org create = %d %v", code, res)
	}
	// Non-members can't confirm the org exists.
	if code, _ := h.call(t, bob, "GET", "/api/v1/git/orgs/acme", ""); code != http.StatusNotFound {
		t.Fatalf("non-member org get = %d, want 404", code)
	}
	code, res = h.call(t, ada, "PATCH", "/api/v1/git/orgs/acme", `{"defaultRepoPerm":"read","membersCanCreateRepos":true}`)
	if code != http.StatusOK || res["defaultRepoPerm"] != "read" || res["membersCanCreateRepos"] != true {
		t.Fatalf("org patch = %d %v", code, res)
	}

	// Members: add, list, role change is owner-gated.
	if code, _ := h.call(t, ada, "POST", "/api/v1/git/orgs/acme/members", `{"username":"bob","role":"member"}`); code != http.StatusOK {
		t.Fatalf("member add = %d", code)
	}
	code, res = h.call(t, bob, "GET", "/api/v1/git/orgs/acme/members", "")
	if code != http.StatusOK || !strings.Contains(recode(t, res), `"bob"`) {
		t.Fatalf("members list = %d %v", code, res)
	}
	if code, _ := h.call(t, bob, "POST", "/api/v1/git/orgs/acme/members", `{"username":"carol","role":"member"}`); code != http.StatusNotFound {
		t.Fatalf("member add by non-owner = %d, want 404", code)
	}
	_ = carol

	// Teams: create, add member, team grant fans out to repo access.
	code, res = h.call(t, ada, "POST", "/api/v1/git/orgs/acme/teams", `{"name":"backend"}`)
	if code != http.StatusCreated {
		t.Fatalf("team create = %d %v", code, res)
	}
	teamID, _ := res["id"].(string)
	if code, _ := h.call(t, ada, "POST", "/api/v1/git/orgs/acme/teams/"+teamID+"/members", `{"username":"bob"}`); code != http.StatusOK {
		t.Fatalf("team member add = %d", code)
	}
	code, res = h.call(t, ada, "POST", "/api/v1/git/repos", `{"ns":"acme","name":"infra"}`)
	if code != http.StatusCreated {
		t.Fatalf("org repo create = %d %v", code, res)
	}
	if code, _ := h.call(t, ada, "PUT", "/api/v1/git/repos/acme/infra/grants", `{"team":"`+teamID+`","role":"write"}`); code != http.StatusOK {
		t.Fatalf("team grant = %d", code)
	}
	code, res = h.call(t, bob, "GET", "/api/v1/git/repos/acme/infra", "")
	if code != http.StatusOK || res["role"] != "write" {
		t.Fatalf("team-granted get = %d %v, want write role", code, res)
	}
	// Member removal is transactional across teams: bob loses access.
	if code, _ := h.call(t, ada, "DELETE", "/api/v1/git/orgs/acme/members/bob", ""); code != http.StatusOK {
		t.Fatalf("member remove = %d", code)
	}
	// bob keeps org default-read? No — he's out of the org entirely and
	// out of the team; the repo is private → 404.
	if code, _ := h.call(t, bob, "GET", "/api/v1/git/repos/acme/infra", ""); code != http.StatusNotFound {
		t.Fatalf("post-removal get = %d, want 404", code)
	}

	// Profile: absent → exists:false; put → round-trips.
	code, res = h.call(t, ada, "GET", "/api/v1/git/profile", "")
	if code != http.StatusOK || res["exists"] != false {
		t.Fatalf("profile get = %d %v", code, res)
	}
	code, res = h.call(t, ada, "PUT", "/api/v1/git/profile",
		`{"displayName":"Ada M","bio":"hi","public":true,"defaultRepoVisibility":"public"}`)
	if code != http.StatusOK || res["displayName"] != "Ada M" || res["defaultRepoVisibility"] != "public" {
		t.Fatalf("profile put = %d %v", code, res)
	}
	code, res = h.call(t, ada, "GET", "/api/v1/git/profile", "")
	if code != http.StatusOK || res["exists"] != true || res["public"] != true {
		t.Fatalf("profile reget = %d %v", code, res)
	}
}

// recode flattens a decoded body back to JSON for contains checks.
func recode(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestGitRepoContentsAPI — the editor's API twin (§16/§12): create,
// upsert-update, rename, delete, the 409 conflict, and the §4.3 rule
// (read-role callers 404).
func TestGitRepoContentsAPI(t *testing.T) {
	h := newGitAPIHarness(t)
	ctx := context.Background()
	if err := h.k.Site.Update(ctx, func(c *site.Config) error { c.Git.Enabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	ada := h.user(t, "ada")
	bob := h.user(t, "bob")
	repo, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "hello", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	head := func() string {
		hash, _, _ := h.k.Git.BranchHead(ctx, repo.ID, "main")
		return hash.String()
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	fileAt := func(path string) (string, bool) {
		sto, err := h.k.Git.Storer(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		hash, _, _ := h.k.Git.BranchHead(ctx, repo.ID, "main")
		f, found, err := sto.FileAt(hash, path, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return string(f.Content), found
	}

	// Create.
	base := head()
	code, res := h.call(t, ada, "POST", "/api/v1/git/repos/ada/hello/contents",
		`{"branch":"main","path":"src/app.py","content":"`+b64("print('hi')\n")+`","baseSha":"`+base+`","message":"add app"}`)
	if code != http.StatusOK || res["commit"] == "" || res["path"] != "src/app.py" {
		t.Fatalf("create = %d %v", code, res)
	}
	if got, ok := fileAt("src/app.py"); !ok || got != "print('hi')\n" {
		t.Fatalf("created file = %q ok=%v", got, ok)
	}

	// Upsert: the same path now updates (no explicit fromPath needed).
	code, _ = h.call(t, ada, "POST", "/api/v1/git/repos/ada/hello/contents",
		`{"branch":"main","path":"src/app.py","content":"`+b64("print('v2')\n")+`","baseSha":"`+head()+`"}`)
	if code != http.StatusOK {
		t.Fatalf("update = %d", code)
	}
	if got, _ := fileAt("src/app.py"); got != "print('v2')\n" {
		t.Fatalf("updated file = %q", got)
	}

	// Rename via fromPath.
	code, _ = h.call(t, ada, "POST", "/api/v1/git/repos/ada/hello/contents",
		`{"branch":"main","fromPath":"src/app.py","path":"src/main.py","content":"`+b64("print('v2')\n")+`","baseSha":"`+head()+`"}`)
	if code != http.StatusOK {
		t.Fatalf("rename = %d", code)
	}
	if _, ok := fileAt("src/app.py"); ok {
		t.Error("rename left the old path")
	}
	if _, ok := fileAt("src/main.py"); !ok {
		t.Error("rename lost the new path")
	}

	// CAS conflict: a stale baseSha over a path that changed → 409.
	stale := head()
	if code, _ = h.call(t, ada, "POST", "/api/v1/git/repos/ada/hello/contents",
		`{"branch":"main","path":"src/main.py","content":"`+b64("theirs\n")+`","baseSha":"`+stale+`"}`); code != http.StatusOK {
		t.Fatalf("setup edit = %d", code)
	}
	code, res = h.call(t, ada, "POST", "/api/v1/git/repos/ada/hello/contents",
		`{"branch":"main","path":"src/main.py","content":"`+b64("ours\n")+`","baseSha":"`+stale+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("stale save = %d %v, want 409", code, res)
	}
	if got, _ := fileAt("src/main.py"); got != "theirs\n" {
		t.Errorf("conflicted save changed content: %q", got)
	}

	// Delete.
	code, res = h.call(t, ada, "POST", "/api/v1/git/repos/ada/hello/contents",
		`{"branch":"main","path":"src/main.py","delete":true,"baseSha":"`+head()+`"}`)
	if code != http.StatusOK || res["deleted"] != true {
		t.Fatalf("delete = %d %v", code, res)
	}
	if _, ok := fileAt("src/main.py"); ok {
		t.Error("deleted file still present")
	}

	// A read-role caller (public repo would grant read; this one is
	// private and ungranted) answers the unconfirmable 404 (§4.3).
	if code, _ := h.call(t, bob, "POST", "/api/v1/git/repos/ada/hello/contents",
		`{"branch":"main","path":"x.txt","content":"`+b64("x")+`"}`); code != http.StatusNotFound {
		t.Fatalf("stranger contents = %d, want 404", code)
	}
	if err := h.k.Git.SetGrant(ctx, repo.ID, dgit.UserSubject("bob"), "read"); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.call(t, bob, "POST", "/api/v1/git/repos/ada/hello/contents",
		`{"branch":"main","path":"x.txt","content":"`+b64("x")+`"}`); code != http.StatusNotFound {
		t.Fatalf("read-grant contents = %d, want 404 (write required)", code)
	}

	// Junk content is a 400, not a commit.
	if code, _ := h.call(t, ada, "POST", "/api/v1/git/repos/ada/hello/contents",
		`{"branch":"main","path":"x.txt","content":"not-base64!!!"}`); code != http.StatusBadRequest {
		t.Fatalf("bad base64 = %d, want 400", code)
	}
}
