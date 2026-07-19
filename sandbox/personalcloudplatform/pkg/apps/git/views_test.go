// views_test.go — every git page parses with the base shell and renders
// its fixture data, plus the §3.1 reserved-name/route cross-check.
package git

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// fixtureChrome is a signed-in shell with Git enabled.
func fixtureChrome(title string) kernel.Chrome {
	return kernel.Chrome{
		Title: title, SiteName: "Test Cloud", Theme: "dark",
		CurrentApp: "git", AppName: "Git", GitEnabled: true,
		User:    users.User{Username: "ada", DisplayName: "Ada Morgan"},
		Session: &users.Session{Username: "ada", CSRF: "tok"},
		Apps:    kernel.AppList(site.Config{Git: site.GitConfig{Enabled: true}}, false),
	}
}

func fixtureOrgShell(tab string, owner bool) orgShell {
	return orgShell{
		Chrome: fixtureChrome("acme"),
		Org: dgit.Org{Name: "acme", Description: "the household org",
			DefaultRepoPerm: dgit.PermRead, UsedBytes: 1 << 20, CreatedBy: "ada", CreatedAt: time.Now()},
		IsOwner: owner,
		Tab:     tab,
	}
}

func TestGitPagesRender(t *testing.T) {
	views := ui.MustParse(tplFS)
	now := time.Now()
	cases := []struct {
		page string
		data any
		want []string
	}{
		{"git_home", HomePage{Chrome: fixtureChrome("Git"), HasRepos: true,
			Orgs: []OrgTile{{Name: "acme", Role: "owner"}},
			Groups: []RepoGroup{
				{NS: "ada", Repos: []RepoTile{{NS: "ada", Name: "dotfiles", Visibility: "private", Description: "config files"}}},
				{NS: "acme", IsOrg: true, Repos: []RepoTile{{NS: "acme", Name: "infra", Visibility: "private"}}},
			},
			Shared: []RepoTile{{NS: "acme", Name: "tools", Visibility: "private"}}},
			[]string{"acme", "owner", "/git/ada/dotfiles", "config files", "/git/acme/infra",
				"/git/acme/tools", "/git/orgs/new", "/git/new"}},
		{"git_home", HomePage{Chrome: fixtureChrome("Git")},
			[]string{"Nothing shared with you yet", "not in any organizations"}},
		{"git_settings", SettingsPage{Chrome: fixtureChrome("Git settings"), Host: "cloud.test",
			HasProfile: true,
			Profile:    dgit.Profile{DisplayName: "Ada", Bio: "hi", Public: true, NotifyEmail: true},
			Keys:       []apikeys.Key{{KeyID: "k1", Name: "laptop git", CreatedAt: now}}},
			[]string{"Ada", "laptop git", "Public profile", "git:read", "/settings/apikeys"}},
		{"git_settings", SettingsPage{Chrome: fixtureChrome("Git settings"), Host: "cloud.test",
			NewToken: "pcp_k_secret", NewName: "git credential"},
			[]string{"pcp_k_secret", "never be shown again", "credential.helper store",
				"don't have a git profile yet"}},
		{"git_org_new", OrgNewPage{Chrome: fixtureChrome("New organization")},
			[]string{"/git/orgs/create", "3–32 characters"}},
		{"git_org_members", OrgMembersPage{orgShell: fixtureOrgShell("members", true), Self: "ada",
			Members: []dgit.OrgMemberRow{
				{Username: "ada", OrgMember: dgit.OrgMember{Role: "owner", Since: now}},
				{Username: "bob", OrgMember: dgit.OrgMember{Role: "member", Since: now}},
			}},
			[]string{"@ada", "@bob", "Add member", "Make owner", "members/remove"}},
		{"git_org_teams", OrgTeamsPage{orgShell: fixtureOrgShell("teams", true),
			Teams: []dgit.Team{{ID: "team12345678", Org: "acme", Name: "backend",
				Description: "the backenders", Members: []string{"ada", "bob"}}}},
			[]string{"backend", "@ada", "@bob", "teams/create", "Delete team"}},
		{"git_org_settings", OrgSettingsPage{orgShell: fixtureOrgShell("settings", true)},
			[]string{"default_repo_perm", "Public member list", "Danger zone", "zero repositories"}},
		{"git_issues", IssuesPage{
			repoShell: repoShell{Chrome: fixtureChrome("issues"), RepoPath: "ada/hello",
				Repo: dgit.Repo{OwnerNS: "ada", Name: "hello", Visibility: "private", DefaultBranch: "main"},
				Tab:  "issues", CanWrite: true, OpenIssues: 1},
			State: "open", OpenCount: 1, ClosedCount: 2,
			Labels: []dgit.Label{{ID: "lbl123456789", Name: "bug", Color: "#e8746b"}},
			Rows: []IssueRowVM{{N: 4, Title: "roof leaks", Author: "bob", State: "open",
				Labels:    []dgit.Label{{ID: "lbl123456789", Name: "bug", Color: "#e8746b"}},
				Assignees: []string{"ada"}, Comments: 3, Updated: now, Href: "/git/ada/hello/issues/4"}},
			NextCursor: "cursor-token"},
			[]string{"Open (1)", "Closed (2)", "roof leaks", "#4 by @bob", "bug",
				"3 comments", "New issue", "Manage labels", "labels/create", "Older →"}},
		{"git_repo_edit", EditorPage{
			repoShell: repoShell{Chrome: fixtureChrome("edit"), RepoPath: "ada/hello",
				Repo: dgit.Repo{OwnerNS: "ada", Name: "hello", Visibility: "private", DefaultBranch: "main"},
				Tab:  "code", CanWrite: true, Assets: assetBases()},
			Branch: "main", BaseSHA: strings.Repeat("a", 40),
			OldPath: "docs/guide.md", Path: "docs/guide.md",
			Content: "# guide\n", Message: "Update guide.md",
			Action:     "/git/ada/hello/edit/main/docs/guide.md",
			CancelHref: "/git/ada/hello/blob/main/docs/guide.md"},
			[]string{"ace.js", "ext-searchbox.js", `name="content"`, "Commit to main",
				"ace/theme/pcp", strings.Repeat("a", 40), "Update guide.md", "Cancel"}},
		{"git_repo_tree", TreePage{
			repoShell: repoShell{Chrome: fixtureChrome("tree"), RepoPath: "ada/hello",
				Repo: dgit.Repo{OwnerNS: "ada", Name: "hello", Visibility: "private", DefaultBranch: "main"},
				Tab:  "code"},
			Ref: "main", Branches: []string{"main"},
			HeadSha: strings.Repeat("a", 40), HeadShort: "aaaaaaaa",
			Entries: []EntryVM{
				{Name: "attributed.go", Size: 12, Href: "/git/ada/hello/blob/main/attributed.go",
					LastSha: strings.Repeat("b", 40), LastShort: "bbbbbbbb",
					LastSubject: "make it real", LastWhen: now},
				// No LastSha: the capped-walk fallback renders an em-dash.
				{Name: "orphan.txt", Size: 5, Href: "/git/ada/hello/blob/main/orphan.txt"},
			}},
			[]string{"attributed.go", "make it real", "/git/ada/hello/commit/" + strings.Repeat("b", 40),
				"orphan.txt", `<span class="nolast">—</span>`, "aaaaaaaa",
				"/git/ada/hello/commit/" + strings.Repeat("a", 40)}},
		{"git_repo_history", HistoryPage{
			repoShell: repoShell{Chrome: fixtureChrome("history"), RepoPath: "ada/hello",
				Repo: dgit.Repo{OwnerNS: "ada", Name: "hello", Visibility: "private", DefaultBranch: "main"},
				Tab:  "code"},
			Ref: "main", Path: "docs/guide.md", FileName: "guide.md",
			Crumbs:  crumbs("ada/hello", "blob", "main", "docs/guide.md"),
			HeadSha: strings.Repeat("a", 40), HeadShort: "aaaaaaaa",
			Commits: []CommitVM{{Sha: strings.Repeat("b", 40), ShortSha: "bbbbbbbb",
				Subject: "touch the guide", Author: "ada", When: now}},
			NextAfter: strings.Repeat("c", 40), Capped: true,
			BackHref: "/git/ada/hello/blob/main/docs/guide.md"},
			[]string{"History", "touch the guide", "bbbbbbbb", "aaaaaaaa",
				"/git/ada/hello/commit/" + strings.Repeat("b", 40),
				"?after=" + strings.Repeat("c", 40), "scan stopped early", "Keep looking"}},
		{"git_repo_blame", BlamePage{
			repoShell: repoShell{Chrome: fixtureChrome("blame"), RepoPath: "ada/hello",
				Repo: dgit.Repo{OwnerNS: "ada", Name: "hello", Visibility: "private", DefaultBranch: "main"},
				Tab:  "code"},
			Ref: "main", Path: "main.go", FileName: "main.go",
			Crumbs:  crumbs("ada/hello", "blob", "main", "main.go"),
			HeadSha: strings.Repeat("a", 40), HeadShort: "aaaaaaaa",
			Size: 24, NLines: 3, Lang: "go",
			Groups: []BlameGroup{
				{Sha: strings.Repeat("b", 40), ShortSha: "bbbbbbbb", Subject: "born", Author: "ada", When: now,
					Lines: []BlobLine{{N: 1, Text: "package main"}, {N: 2, Text: ""}}},
				{Sha: strings.Repeat("d", 40), ShortSha: "dddddddd", Subject: "grew", Author: "bob", When: now,
					Lines: []BlobLine{{N: 3, Text: "func main() {}"}}},
			},
			HistoryHref: "/git/ada/hello/history/main/main.go",
			BlobHref:    "/git/ada/hello/blob/main/main.go",
			RawHref:     "/git/ada/hello/raw/main/main.go"},
			// The gicon regression guards ride this case: gitcss must size
			// .ficon globally (the /new page's giant-file-icon bug), the
			// .chip glyphs, and the delete dialog's h3 icon.
			[]string{"bbbbbbbb", "dddddddd", `rowspan="2"`, "package main", "func main() {}",
				`data-lang="go"`, "Normal view", "History",
				".gp .ficon{width:16px;height:16px", ".gp .chip svg{width:13px",
				".eddialog h3 svg{width:16px"}},
		{"git_repo_blame", BlamePage{
			repoShell: repoShell{Chrome: fixtureChrome("blame"), RepoPath: "ada/hello",
				Repo: dgit.Repo{OwnerNS: "ada", Name: "hello", Visibility: "private", DefaultBranch: "main"},
				Tab:  "code"},
			Ref: "main", Path: "big.bin", FileName: "big.bin",
			HeadSha: strings.Repeat("a", 40), HeadShort: "aaaaaaaa",
			Size: 9 << 20, Refused: "This file is too large to blame",
			HistoryHref: "/git/ada/hello/history/main/big.bin",
			BlobHref:    "/git/ada/hello/blob/main/big.bin",
			RawHref:     "/git/ada/hello/raw/main/big.bin"},
			[]string{"too large to blame", "history/main/big.bin"}},
		{"git_issue_new", IssueNewPage{
			repoShell: repoShell{Chrome: fixtureChrome("new issue"), RepoPath: "ada/hello",
				Repo: dgit.Repo{OwnerNS: "ada", Name: "hello", Visibility: "private", DefaultBranch: "main"}, Tab: "issues"}},
			[]string{"issues/create", "Markdown is supported", "Open issue"}},
		{"git_issue", IssuePage{
			repoShell: repoShell{Chrome: fixtureChrome("issue"), RepoPath: "ada/hello",
				Repo: dgit.Repo{OwnerNS: "ada", Name: "hello", Visibility: "private", DefaultBranch: "main"},
				Tab:  "issues", CanWrite: true},
			Issue: dgit.Issue{N: 4, Title: "roof leaks", Body: "the **attic**", Author: "bob", State: "open",
				Assignees: []string{"ada"}, CreatedAt: now, UpdatedAt: now, CommentCount: 1},
			BodyHTML: "<p>the <strong>attic</strong></p>",
			Labels:   []dgit.Label{{ID: "lbl123456789", Name: "bug", Color: "#e8746b"}},
			Comments: []CommentVM{{ID: "c1", Author: "carol", Body: "raw", HTML: "<p>same here</p>",
				CreatedAt: now, CanEdit: true, CanDelete: true}},
			CanClose: true, AllLabels: []dgit.Label{{ID: "lbl123456789", Name: "bug", Color: "#e8746b"}},
			Assignable: []string{"ada", "bob"}, Href: "/git/ada/hello/issues/4"},
			[]string{"roof leaks", "#4", "statepill open", "<strong>attic</strong>", "same here",
				"Close issue", "Apply labels", "Unassign", "comment/delete", "Edit comment", "Add a comment"}},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := views.ExecuteTemplate(&buf, c.page, c.data); err != nil {
			t.Errorf("render %s: %v", c.page, err)
			continue
		}
		out := buf.String()
		for _, want := range c.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s missing %q", c.page, want)
			}
		}
	}
}

// TestMembersReadOnlyForNonOwners: a plain member's members page carries
// no management forms.
func TestMembersReadOnlyForNonOwners(t *testing.T) {
	views := ui.MustParse(tplFS)
	var buf bytes.Buffer
	pg := OrgMembersPage{orgShell: fixtureOrgShell("members", false), Self: "bob",
		Members: []dgit.OrgMemberRow{{Username: "ada", OrgMember: dgit.OrgMember{Role: "owner"}}}}
	if err := views.ExecuteTemplate(&buf, "git_org_members", pg); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, forbidden := range []string{"members/add", "members/remove", "members/role"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("member view must not offer %q", forbidden)
		}
	}
	// …and no Settings tab either.
	if strings.Contains(out, "/git/orgs/acme/settings") {
		t.Error("member view must not link the settings tab")
	}
}

// TestReservedNamesCoverAppRoutes cross-checks the domain's reserved
// list against the kernel's canonical app list and its own route
// prefixes (§3.1) — a new app whose mount prefix isn't reserved would
// let a namespace shadow a route.
func TestReservedNamesCoverAppRoutes(t *testing.T) {
	// Every app the switcher can ever offer (all gates open).
	var allOn site.Config
	for _, f := range site.Features() {
		f.Set(&allOn, true)
	}
	for _, app := range kernel.AppList(allOn, true) {
		prefix := strings.TrimPrefix(app.Href, "/")
		if !dgit.IsReservedName(prefix) {
			t.Errorf("app route prefix %q must be a reserved name", prefix)
		}
	}
	// Kernel-owned routes and platform surfaces outside AppList.
	for _, prefix := range []string{
		"login", "logout", "signup", "static", "healthz", "launcher",
		"settings", "notifications", "invites", "impersonate", "api", "s",
		// git's own literal segments (§5.2: literals win over {ns});
		// "-" anchors the vendored asset route /git/-/assets (§16).
		"git", "orgs", "new", "-",
	} {
		if !dgit.IsReservedName(prefix) {
			t.Errorf("route prefix %q must be a reserved name", prefix)
		}
	}
}
