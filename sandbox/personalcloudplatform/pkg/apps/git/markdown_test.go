// markdown_test.go — the §8 autolink extensions: #N issue references
// and @mentions (existing users only), applied post-escape, never
// inside code (fenced or inline — the renderer carves code out before
// formatting), never double-wrapping explicit links, and immune to
// crafted-body injection.
package git

import (
	"strings"
	"testing"
)

// testCtx is a repo context where only "ada" and "bob" exist.
func testCtx() mdContext {
	return mdContext{
		RepoPath:   "ada/hello",
		UserExists: func(name string) bool { return name == "ada" || name == "bob" },
	}
}

func render(t *testing.T, src string) string {
	t.Helper()
	return string(renderMarkdownCtx([]byte(src), testCtx()))
}

func TestIssueRefAutolink(t *testing.T) {
	out := render(t, "see #12 and (#345) please")
	for _, want := range []string{
		`<a href="/git/ada/hello/issues/12">#12</a>`,
		`<a href="/git/ada/hello/issues/345">#345</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	// Mid-word: no link (URL fragments, css colors).
	if out := render(t, "color #fff and x#12"); strings.Contains(out, "/issues/") {
		t.Errorf("mid-word ref linked: %q", out)
	}
	// Start of line: a paragraph + autolink, NOT a heading (the
	// space-after-# rule).
	out = render(t, "#12 broke everything")
	if !strings.Contains(out, `<a href="/git/ada/hello/issues/12">#12</a>`) || strings.Contains(out, "<h1>") {
		t.Errorf("line-leading ref = %q", out)
	}
	// Real headings still work.
	if out := render(t, "# title"); !strings.Contains(out, "<h1>title</h1>") {
		t.Errorf("heading broke: %q", out)
	}
	// Without a repo context, no ref links (plain READMEs unchanged).
	if out := string(renderMarkdownCtx([]byte("see #12"), mdContext{})); strings.Contains(out, "<a") {
		t.Errorf("context-free render linked: %q", out)
	}
}

func TestIssueRefNotInCode(t *testing.T) {
	// Fenced code.
	out := render(t, "```\nsee #12 and @ada\n```")
	if strings.Contains(out, "<a") {
		t.Errorf("autolink inside fenced code: %q", out)
	}
	// Inline code.
	out = render(t, "run `fix #12 for @ada` now")
	if strings.Contains(out, "/issues/12") || strings.Contains(out, `href="/git/ada"`) {
		t.Errorf("autolink inside inline code: %q", out)
	}
	// …while the same tokens outside code still link.
	out = render(t, "`#12` but really #12")
	if !strings.Contains(out, `<a href="/git/ada/hello/issues/12">#12</a>`) {
		t.Errorf("outside-code ref lost: %q", out)
	}
}

func TestMentionAutolink(t *testing.T) {
	out := render(t, "ping @ada and @ghost-user")
	if !strings.Contains(out, `<a href="/git/ada">@ada</a>`) {
		t.Errorf("existing-user mention not linked: %q", out)
	}
	// Nonexistent users render as plain text (the §11 twin rule).
	if strings.Contains(out, `href="/git/ghost-user"`) {
		t.Errorf("nonexistent user linked: %q", out)
	}
	// Case preserved in the label, lowered in the target.
	out = render(t, "hi @Bob")
	if !strings.Contains(out, `<a href="/git/bob">@Bob</a>`) {
		t.Errorf("case handling wrong: %q", out)
	}
	// Email addresses don't mention (mid-word guard).
	if out := render(t, "mail ada@bob.example"); strings.Contains(out, `href="/git/bob"`) {
		t.Errorf("email fragment linked: %q", out)
	}
}

// TestFenceLanguageClasses — ```lang fences carry language classes into
// the sanitized HTML (§16), whitelist-matched only: the class value is
// always a langByFence VALUE, never the fence string, so a hostile info
// string can inject nothing.
func TestFenceLanguageClasses(t *testing.T) {
	// A known language, case-insensitive, with trailing info words.
	for _, src := range []string{
		"```go\nfmt.Println(1)\n```",
		"```Go\nfmt.Println(1)\n```",
		"```go linenums\nfmt.Println(1)\n```",
	} {
		out := render(t, src)
		if !strings.Contains(out, `<pre><code class="language-go">`) {
			t.Errorf("fence %q missing language class: %q", src, out)
		}
	}
	// Aliases map onto the vendored grammar names.
	if out := render(t, "```c++\nint x;\n```"); !strings.Contains(out, `class="language-cpp"`) {
		t.Errorf("alias fence: %q", out)
	}
	// Unknown languages stay a plain block.
	if out := render(t, "```klingon\nqapla\n```"); strings.Contains(out, "language-") {
		t.Errorf("unknown fence got a class: %q", out)
	}
	// Multiple fences keep their order (known, unknown, known).
	out := render(t, "```go\na\n```\ntext\n```nope\nb\n```\nmore\n```python\nc\n```")
	if !strings.Contains(out, `class="language-go"`) || !strings.Contains(out, `class="language-python"`) {
		t.Errorf("multi-fence classes lost: %q", out)
	}
	if strings.Count(out, "language-") != 2 {
		t.Errorf("unknown fence classed anyway: %q", out)
	}
	goAt, pyAt := strings.Index(out, "language-go"), strings.Index(out, "language-python")
	if goAt < 0 || pyAt < 0 || goAt > pyAt {
		t.Errorf("fence class order wrong: %q", out)
	}
	// Inline code never gets a class.
	if out := render(t, "run `go build` now"); strings.Contains(out, "language-") {
		t.Errorf("inline code classed: %q", out)
	}
	// Hostile fence info strings: nothing from the input reaches the
	// attribute — no class at all, and no attribute injection.
	for _, src := range []string{
		"```go\" onmouseover=\"alert(1)\ncode\n```",
		"```<script>alert(1)</script>\ncode\n```",
		"```language-x\"><img src=x onerror=alert(1)>\ncode\n```",
		"``` go\"onload=x\ncode\n```",
	} {
		out := render(t, src)
		if strings.Contains(out, "language-") {
			t.Errorf("hostile fence %q earned a class: %q", src, out)
		}
		if strings.Contains(out, "onmouseover=\"") || strings.Contains(out, "<script") ||
			strings.Contains(out, "onerror=") || strings.Contains(out, "onload=") {
			t.Errorf("hostile fence injected: %q → %q", src, out)
		}
	}
	// A body that merely CONTAINS the literal text "<pre><code>" must not
	// consume a class slot — it arrives escaped, so injection by marker
	// collision is impossible.
	out = render(t, "look: `<pre><code>` and then\n\n```go\nreal\n```")
	if !strings.Contains(out, `class="language-go"`) {
		t.Errorf("escaped marker text stole the class: %q", out)
	}
}

func TestAutolinkInjectionSafety(t *testing.T) {
	// Crafted bodies: everything is escaped BEFORE the ref pass, and the
	// ref anchors interpolate only validated repo paths + digits /
	// username charset.
	cases := []string{
		`<script>alert(1)</script> #12`,
		`#12" onmouseover="alert(1)`,
		`@ada" onclick="x`,
		`[click](javascript:alert(1)) #12`,
		`<a href="/evil">#12</a>`,
	}
	for _, src := range cases {
		out := render(t, src)
		// Dangerous constructs must never appear in ATTRIBUTE position —
		// a raw quote after the name is what an injected attribute needs;
		// escaped text (&#34;) is fine.
		if strings.Contains(out, "<script") || strings.Contains(out, `href="javascript:`) ||
			strings.Contains(out, `onmouseover="`) || strings.Contains(out, `onclick="`) ||
			strings.Contains(out, `href="/evil"`) {
			t.Errorf("injection survived: %q → %q", src, out)
		}
	}
	// Explicit links never double-wrap; refs inside anchor text stay.
	out := render(t, "[see #12](https://example.test/x) and https://example.test/#5")
	if strings.Contains(out, `<a href="/git/ada/hello/issues/12"`) {
		t.Errorf("ref rewritten inside an explicit link: %q", out)
	}
	if strings.Contains(out, `/issues/5`) {
		t.Errorf("URL fragment linked as issue: %q", out)
	}
}

// --- the GFM extensions: tables, task lists, nesting, setext, relative
// targets — and their safety edges. ---------------------------------------

// relCtx is testCtx plus the repo-relative bases a README render gets.
func relCtx(dir string) mdContext {
	mc := testCtx()
	mc.RawBase = "/git/ada/hello/raw/main"
	mc.BlobBase = "/git/ada/hello/blob/main"
	mc.Dir = dir
	return mc
}

func renderRel(t *testing.T, dir, src string) string {
	t.Helper()
	return string(renderMarkdownCtx([]byte(src), relCtx(dir)))
}

func TestTableRendering(t *testing.T) {
	out := render(t, "| Name | Count |\n| :--- | ---: |\n| ada | 3 |\n| **bob** | 5 |")
	for _, want := range []string{
		"<table>", "<thead>", "<th>Name</th>", `<th align="right">Count</th>`,
		"<tbody>", "<td>ada</td>", `<td align="right">3</td>`,
		"<td><strong>bob</strong></td>", "</table>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q: %q", want, out)
		}
	}
	// Center alignment and escaped pipes inside cells.
	out = render(t, "| a | b |\n|:-:|---|\n| c\\|d | e |")
	if !strings.Contains(out, `<th align="center">a</th>`) || !strings.Contains(out, `<td align="center">c|d</td>`) {
		t.Errorf("center/escaped-pipe table wrong: %q", out)
	}
	// Prose with a pipe but no delimiter row stays a paragraph.
	out = render(t, "a | b\nplain text")
	if strings.Contains(out, "<table>") {
		t.Errorf("prose with a pipe became a table: %q", out)
	}
	// Ragged rows pad/truncate to the header's column count.
	out = render(t, "| a | b |\n|---|---|\n| only |\n| x | y | extra |")
	if !strings.Contains(out, "<td>only</td><td></td>") || strings.Contains(out, "extra") {
		t.Errorf("ragged rows not normalized: %q", out)
	}
	// A crafted alignment can't inject — cells are markup, not attrs.
	out = render(t, "| a |\n|---|\n| \" onmouseover=\"x |")
	if strings.Contains(out, `onmouseover="x"`) {
		t.Errorf("table cell injection survived: %q", out)
	}
}

func TestTaskLists(t *testing.T) {
	out := render(t, "- [x] done\n- [ ] todo\n- plain")
	if !strings.Contains(out, "<li>☑ done</li>") || !strings.Contains(out, "<li>☐ todo</li>") ||
		!strings.Contains(out, "<li>plain</li>") {
		t.Errorf("task list wrong: %q", out)
	}
}

func TestNestedLists(t *testing.T) {
	out := render(t, "- top\n  - inner\n    1. numbered\n  - inner2\n- top2\n+ plus")
	for _, want := range []string{
		"<ul><li>top</li><ul><li>inner</li><ol><li>numbered</li></ol><li>inner2</li></ul><li>top2</li><li>plus</li></ul>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("nested list output missing %q: %q", want, out)
		}
	}
}

func TestSetextHeadings(t *testing.T) {
	out := render(t, "Title\n=====\n\nSection\n---\n\nbody")
	if !strings.Contains(out, "<h1>Title</h1>") || !strings.Contains(out, "<h2>Section</h2>") {
		t.Errorf("setext headings wrong: %q", out)
	}
	// A lone --- stays a horizontal rule.
	out = render(t, "para\n\n---\n\nmore")
	if !strings.Contains(out, "<hr") {
		t.Errorf("hr lost: %q", out)
	}
}

func TestRelativeImagesAndLinks(t *testing.T) {
	// Root README: relative image → raw endpoint; relative link → blob.
	out := renderRel(t, "", "![shot](docs/shot.png) and [guide](docs/guide.md)")
	if !strings.Contains(out, `<img src="/git/ada/hello/raw/main/docs/shot.png" alt="shot"`) {
		t.Errorf("relative image not resolved: %q", out)
	}
	if !strings.Contains(out, `<a href="/git/ada/hello/blob/main/docs/guide.md"`) {
		t.Errorf("relative link not resolved: %q", out)
	}
	// A markdown file in a subdirectory resolves against its own dir,
	// and ../ walks up WITHIN the repo.
	out = renderRel(t, "docs/sub", "![a](../pic.png)")
	if !strings.Contains(out, `src="/git/ada/hello/raw/main/docs/pic.png"`) {
		t.Errorf("dir-relative image wrong: %q", out)
	}
	// Traversal clamps at the repo root — never escapes into /git/.
	out = renderRel(t, "", "![a](../../../../etc/passwd)")
	if !strings.Contains(out, `src="/git/ada/hello/raw/main/etc/passwd"`) {
		t.Errorf("traversal not clamped: %q", out)
	}
	// Scheme-shaped targets are never repo paths.
	for _, src := range []string{"![a](javascript:alert(1))", "![a](data:text/html,x)", "[a](vbscript:x)"} {
		out = renderRel(t, "", src)
		if strings.Contains(out, "javascript:") && strings.Contains(out, "href=") ||
			strings.Contains(out, `src="data:`) || strings.Contains(out, `href="vbscript:`) {
			t.Errorf("scheme target survived: %q → %q", src, out)
		}
	}
	// Remote images still render as text/link — never an <img>.
	out = renderRel(t, "", "![a](https://evil.test/pixel.png)")
	if strings.Contains(out, "<img") {
		t.Errorf("remote image embedded: %q", out)
	}
	// Without bases (issue bodies), relative targets stay plain text.
	out = render(t, "![a](docs/shot.png)")
	if strings.Contains(out, "<img") {
		t.Errorf("relative image resolved without a base: %q", out)
	}
}
