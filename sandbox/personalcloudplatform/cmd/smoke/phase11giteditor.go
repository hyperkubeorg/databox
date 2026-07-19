// phase11giteditor.go — the §16 slice of the git smoke: the in-browser
// editor and syntax highlighting against the running pcp. Web-edits an
// existing file through the editor POST and proves the commit with a
// stock `git pull`, creates a new file the same way, and verifies a
// highlighted blob page carries the language hook + fence classes and
// that the vendored assets actually serve.
package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// readFile reads one file as a string ("" on error — assertions catch it).
func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func phase11gitEditor(ada *web, src string) {
	head := func() string {
		return strings.Fields(mustGit(src, "ls-remote", "origin", "refs/heads/main"))[0]
	}

	// The editor page renders with the vendored Ace assets.
	if code, page, err := ada.get("/git/ada/hello/edit/main/README.md"); err == nil && code == 200 &&
		strings.Contains(page, "/git/-/assets/vendor/ace/") && strings.Contains(page, "ace.js") &&
		strings.Contains(page, `name="content"`) {
		pass("editor: edit page renders with the vendored Ace assets")
	} else {
		fail("editor page wrong", "code", code, "err", err)
		return
	}

	// Web-edit README.md (with a Go fence for the highlight check below)
	// → a real commit a stock `git pull` fetches.
	readme := "# hello\n\nedited from the browser\n\n```go\npackage main\n```\n"
	code, body, _ := ada.post("/git/ada/hello/edit/main/README.md", url.Values{
		"base": {head()}, "path": {"README.md"},
		"content": {readme}, "message": {"web edit from the smoke"},
	})
	if code != 200 || jsonMap(body)["ok"] != true {
		fail("editor edit POST", "code", code, "body", body)
		return
	}
	mustGit(src, "pull", "origin", "main")
	if b := readFile(filepath.Join(src, "README.md")); strings.Contains(b, "edited from the browser") {
		pass("editor: web edit landed as a commit — stock git pull sees the content")
	} else {
		fail("pulled README missing the web edit", "content", b)
		return
	}

	// Blame (§5.2): README's first line ("# hello") survives from the
	// repo's first commit while the edited tail belongs to the web-edit
	// commit — the page must attribute lines to BOTH.
	firstSha := strings.Fields(mustGit(src, "rev-list", "--max-parents=0", "HEAD"))[0]
	editSha := head()
	if code, page, _ := ada.get("/git/ada/hello/blame/main/README.md"); code == 200 &&
		strings.Contains(page, firstSha[:8]) && strings.Contains(page, editSha[:8]) &&
		strings.Contains(page, `class="bm"`) {
		pass("blame: page attributes lines to two different commits (grouped gutter)")
	} else {
		fail("blame attribution wrong", "code", code, "first", firstSha[:8], "edit", editSha[:8])
	}

	// History (§5.2): the per-path log lists ONLY commits that touched
	// the path — more.txt has exactly one, and the repo's first commit
	// must not appear.
	if code, page, _ := ada.get("/git/ada/hello/history/main/more.txt"); code == 200 &&
		strings.Contains(page, "too big") && !strings.Contains(page, firstSha[:8]) {
		pass("history: page lists only the commits touching the file")
	} else {
		fail("history filtering wrong", "code", code)
	}

	// Create a new Go file through /new → pull sees it too.
	code, body, _ = ada.post("/git/ada/hello/new/main", url.Values{
		"base": {head()}, "path": {"tools/smoke.go"},
		"content": {"package tools\n\n// born in a browser\n"},
	})
	if code != 200 || jsonMap(body)["ok"] != true {
		fail("editor create POST", "code", code, "body", body)
		return
	}
	mustGit(src, "pull", "origin", "main")
	if b := readFile(filepath.Join(src, "tools", "smoke.go")); strings.Contains(b, "born in a browser") {
		pass("editor: web-created file round-trips through a clone")
	} else {
		fail("pulled tree missing the created file", "content", b)
	}

	// Highlighting: the blob page carries the whitelist language hook +
	// the vendored script include; the rendered README carries the fence
	// class the escape-first renderer re-attached.
	if code, page, _ := ada.get("/git/ada/hello/blob/main/tools/smoke.go"); code == 200 &&
		strings.Contains(page, `data-lang="go"`) &&
		strings.Contains(page, "/git/-/assets/vendor/highlightjs/") {
		pass("highlight: blob page carries data-lang + the vendored highlight.js include")
	} else {
		fail("blob highlight hooks missing", "code", code)
	}
	if code, page, _ := ada.get("/git/ada/hello/blob/main/README.md"); code == 200 &&
		strings.Contains(page, `<pre><code class="language-go">`) {
		pass("highlight: markdown fence carries the whitelisted language class")
	} else {
		fail("fence class missing on the rendered README", "code", code)
	}
	// …and the vendored asset URLs answer 200.
	for _, p := range []string{
		"/git/-/assets/vendor/highlightjs/11.11.1/highlight.min.js",
		"/git/-/assets/vendor/ace/1.44.0/ace.js",
		"/git/-/assets/vendor/ace/1.44.0/LICENSE",
	} {
		if code, body, err := ada.get(p); err != nil || code != 200 || len(body) == 0 {
			fail("vendored asset", "path", p, "code", code, "err", err)
			return
		}
	}
	pass("highlight: vendored asset routes (js + LICENSE) all answer 200")
}
