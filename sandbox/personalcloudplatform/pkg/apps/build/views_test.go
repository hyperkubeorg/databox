// views_test.go — every build page parses with the base shell and renders
// its fixture data (the git app's views_test approach).
package build

import (
	"bytes"
	"strings"
	"testing"
	"time"

	dbuild "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

func fixtureShell(tab string, canWrite, canTrigger bool) buildShell {
	return buildShell{
		Chrome: kernel.Chrome{
			Title: "ada/hello", SiteName: "Test Cloud", Theme: "dark",
			CurrentApp: "git", AppName: "Git", GitEnabled: true, BuildsEnabled: true,
			User:    users.User{Username: "ada", DisplayName: "Ada Morgan"},
			Session: &users.Session{Username: "ada", CSRF: "tok"},
		},
		Repo:       dgit.Repo{OwnerNS: "ada", Name: "hello", Visibility: "private", DefaultBranch: "main"},
		RepoPath:   "ada/hello",
		CanWrite:   canWrite,
		CanTrigger: canTrigger,
		Tab:        tab,
	}
}

func TestBuildPagesRender(t *testing.T) {
	views := ui.MustParse(tplFS)
	now := time.Now()
	cases := []struct {
		page string
		data any
		want []string
	}{
		{"build_list", BuildListPage{
			buildShell: fixtureShell("builds", true, true),
			Builds: []BuildVM{
				{N: 2, State: "running", Trigger: "manual", Ref: "main", Commit: "abcdef12",
					Actor: "ada", CreatedAt: now, Href: "/git/ada/hello/builds/2"},
				{N: 1, State: "success", Trigger: "push", Ref: "main", Commit: "12345678",
					Actor: "bob", RetryOf: 0, CreatedAt: now, Href: "/git/ada/hello/builds/1"},
			}},
			[]string{"Builds", "Run build", "/git/ada/hello/builds/trigger", "Build #2",
				"running", "abcdef12", "by @ada", "/git/ada/hello/builds/1",
				// the full repo nav is present
				"/git/ada/hello/issues", "/git/ada/hello/merges", "/git/ada/hello/releases"}},
		{"build_list", BuildListPage{buildShell: fixtureShell("builds", false, false)},
			[]string{"No builds yet"}},
		{"build_detail", BuildDetailPage{
			buildShell: fixtureShell("builds", true, true),
			Build: BuildVM{N: 3, State: "cancelled", Trigger: "manual", Ref: "main",
				Commit: "deadbeef", Actor: "ada", RetryOf: 1, CreatedAt: now,
				Href: "/git/ada/hello/builds/3"},
			CanCancel: false, CanRetry: true, CanDelete: true,
			Phases: []PhaseVM{
				{Name: "build", State: "success", Image: "busybox", Steps: []dbuild.Step{{Name: "compile"}}},
				{Name: "test", State: "skipped", Requires: "build"},
			}},
			[]string{"Build #3", "cancelled", "retry of #1", "deadbeef",
				"/git/ada/hello/builds/3/retry", "/git/ada/hello/builds/3/delete",
				"Phases", "build", "busybox", "compile", "requires build"}},
		{"release_list", ReleaseListPage{
			buildShell: fixtureShell("releases", true, false),
			Releases: []ReleaseVM{
				{ID: "relAAAABBBB", Tag: "v1.0.0", Name: "First cut", Prerelease: false,
					BuildN: 1, Commit: "abcdef12", Author: "ada", CreatedAt: now,
					Href: "/git/ada/hello/releases/relAAAABBBB"},
			}},
			[]string{"Releases", "v1.0.0", "First cut", "build #1", "by @ada",
				"/git/ada/hello/releases/relAAAABBBB"}},
		{"release_list", ReleaseListPage{buildShell: fixtureShell("releases", false, false)},
			[]string{"No releases yet"}},
		{"release_detail", ReleaseDetailPage{
			buildShell: fixtureShell("releases", true, false),
			Release: ReleaseVM{ID: "relAAAABBBB", Tag: "v1.0.0", Name: "First cut",
				Prerelease: true, BuildN: 1, Commit: "abcdef12", Author: "ada", CreatedAt: now},
			Notes:     "the release notes",
			Artifacts: []string{"binary", "checksums.txt"}},
			[]string{"v1.0.0", "First cut", "pre-release", "the release notes",
				"Artifacts", "binary", "checksums.txt"}},
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
