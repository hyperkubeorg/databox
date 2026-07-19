// markdown.go — the README/markdown renderer for repo pages (§5.2's
// "platform markdown renderer + sanitizer"). Same discipline as the
// messenger chat renderer: safe BY CONSTRUCTION — every run of user
// text is HTML-escaped before any tag wraps it, and only a fixed tag
// set is ever emitted — then the output ALSO passes through the
// platform's mailrender.SanitizeHTML whitelist (belt and suspenders; no
// new sanitizer, per project rule). GitHub-flavored subset: ATX and
// setext headings, fenced code, blockquotes, nested unordered/ordered
// lists, task-list items, pipe tables with column alignment, horizontal
// rules, paragraphs; inline code, bold, italic, strikethrough,
// [text](url) links (http/https/mailto only), and autolinked bare URLs.
// REMOTE images render as links — repo pages never fetch remote content
// — but repo-RELATIVE image and link targets resolve against the repo's
// own raw/blob endpoints (mdContext.RawBase/BlobBase), which already
// neuter active content types (SafeAttachmentCT + nosniff), so a README
// can show its own screenshots without ever leaving the site. Issues
// (§8) and merge requests (§9) reuse this for their bodies/comments.
package git

import (
	"fmt"
	"html"
	"html/template"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailrender"
)

// maxMarkdownBytes caps what the renderer will process; bigger files
// fall back to the plain view.
const maxMarkdownBytes = 512 << 10

var (
	mdBold    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	mdStrike  = regexp.MustCompile(`~~(.+?)~~`)
	mdItalicA = regexp.MustCompile(`\*([^*]+?)\*`)
	mdItalicU = regexp.MustCompile(`\b_([^_]+?)_\b`)
	// mdLink matches [text](target) over already-escaped text; the
	// target charset excludes quotes/spaces so an attribute can't break.
	mdLink = regexp.MustCompile(`(!?)\[([^\]]+)\]\(([^)\s"'<>]+)\)`)
	// mdURL autolinks bare http(s) URLs on escaped text ('<' can't match).
	mdURL     = regexp.MustCompile(`https?://[^\s<]+[^\s<.,)]`)
	mdListNum = regexp.MustCompile(`^\d{1,9}[.)] `)
	// mdIssueRef (§8) matches #123 at a run boundary — never mid-word,
	// so URL fragments and CSS colors don't autolink.
	mdIssueRef = regexp.MustCompile(`(^|[\s([{])#(\d{1,9})\b`)
	// mdMentionRef matches @username the same way (issuenotify.go's
	// mention rule: 3–32 of a-z 0-9 dashes, case-insensitive).
	mdMentionRef = regexp.MustCompile(`(?i)(^|[\s([{])@([a-z0-9][a-z0-9-]{2,31})\b`)
)

// mdContext carries the render-site affordances (§8): RepoPath enables
// #N autolinks to the SAME repo's issue/MR page (cross-repo linking is
// v2), and UserExists gates @mention links to existing accounts — a
// nonexistent name renders as plain text, mirroring the notification
// rule. RawBase/BlobBase ("/git/<ns>/<repo>/raw/<ref>", no trailing
// slash) plus Dir (the markdown file's directory inside the repo)
// resolve repo-relative image/link targets; empty bases leave relative
// targets as plain text. The zero value renders plain markdown.
type mdContext struct {
	RepoPath   string
	UserExists func(username string) bool
	RawBase    string
	BlobBase   string
	Dir        string
}

// renderMarkdown turns raw markdown into safe HTML for repo pages.
func renderMarkdown(raw []byte) template.HTML {
	return renderMarkdownCtx(raw, mdContext{})
}

// renderMarkdownCtx is renderMarkdown with #N/@mention autolinks (§8) —
// issue bodies and comments render through this. Fenced code blocks and
// inline code spans never reach mdFormatRun, so autolinks can't apply
// inside code (the renderer distinguishes them by construction).
func renderMarkdownCtx(raw []byte, mc mdContext) template.HTML {
	if len(raw) > maxMarkdownBytes {
		raw = raw[:maxMarkdownBytes]
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	var b strings.Builder
	// fenceLangs records each fence's whitelisted language, in emission
	// order, for the post-sanitize class injection (§16): the sanitizer
	// strips attributes, so the classes are re-attached AFTER it from
	// this list — whitelist values only, never fence-string input.
	var fenceLangs []string
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "```"):
			fenceLangs = append(fenceLangs, fenceLang(trimmed[3:]))
			var code []string
			i++
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			i++ // closing fence (or EOF)
			b.WriteString("<pre><code>")
			b.WriteString(html.EscapeString(strings.Join(code, "\n")))
			b.WriteString("</code></pre>")

		case mdHeadingLevel(trimmed) > 0:
			level := mdHeadingLevel(trimmed)
			rest := strings.TrimSpace(trimmed[level:])
			tag := "h" + strconv.Itoa(level)
			b.WriteString("<" + tag + ">")
			b.WriteString(mdInline(rest, mc))
			b.WriteString("</" + tag + ">")
			i++

		case trimmed == "---" || trimmed == "***" || trimmed == "___":
			b.WriteString("<hr>")
			i++

		case strings.HasPrefix(line, ">"):
			var quote []string
			for i < len(lines) && strings.HasPrefix(lines[i], ">") {
				quote = append(quote, strings.TrimPrefix(strings.TrimPrefix(lines[i], ">"), " "))
				i++
			}
			b.WriteString("<blockquote>")
			b.WriteString(mdInline(strings.Join(quote, "\n"), mc))
			b.WriteString("</blockquote>")

		case mdIsListLine(line):
			i = mdRenderList(&b, lines, i, mc)

		case mdIsTableStart(lines, i):
			i = mdRenderTable(&b, lines, i, mc)

		case trimmed != "" && i+1 < len(lines) && mdSetextLevel(strings.TrimSpace(lines[i+1])) > 0:
			// Setext heading: a text line underlined with === (h1) or
			// --- (h2). Reached only for plain-text lines — every other
			// block shape matched an earlier case.
			level := mdSetextLevel(strings.TrimSpace(lines[i+1]))
			tag := "h" + strconv.Itoa(level)
			b.WriteString("<" + tag + ">")
			b.WriteString(mdInline(trimmed, mc))
			b.WriteString("</" + tag + ">")
			i += 2

		case trimmed == "":
			i++

		default:
			var para []string
			for i < len(lines) {
				t := strings.TrimSpace(lines[i])
				if t == "" || strings.HasPrefix(t, "```") || mdHeadingLevel(t) > 0 ||
					strings.HasPrefix(lines[i], ">") || mdIsListLine(lines[i]) {
					break
				}
				para = append(para, lines[i])
				i++
			}
			b.WriteString("<p>")
			b.WriteString(mdInline(strings.Join(para, "\n"), mc))
			b.WriteString("</p>")
		}
	}
	// The platform sanitizer as the second layer (mailrender's whitelist
	// rebuild) — reuse, never a new sanitizer. The App variant admits
	// site-relative hrefs, which is what #N/@mention autolinks emit.
	return template.HTML(injectFenceClasses(mailrender.SanitizeAppHTML(b.String()), fenceLangs))
}

// injectFenceClasses re-attaches language classes to fenced code blocks
// AFTER sanitization. Safe by construction: the sanitizer rebuilds the
// HTML, so `<pre><code>` occurs exactly where this renderer emitted a
// fence (escaped user text can never contain a raw tag), occurrence
// order matches emission order, and every class value comes from the
// langByFence whitelist — never from the fence string itself.
func injectFenceClasses(sanitized string, langs []string) string {
	const marker = "<pre><code>"
	if len(langs) == 0 || !strings.Contains(sanitized, marker) {
		return sanitized
	}
	var b strings.Builder
	rest := sanitized
	for _, lang := range langs {
		i := strings.Index(rest, marker)
		if i < 0 {
			break // sanitizer truncation — remaining fences were cut
		}
		b.WriteString(rest[:i])
		if lang == "" {
			b.WriteString(marker)
		} else {
			b.WriteString(`<pre><code class="language-` + lang + `">`)
		}
		rest = rest[i+len(marker):]
	}
	b.WriteString(rest)
	return b.String()
}

func mdIsBullet(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "+ ")
}

// mdIsListLine matches any list item — bullet or ordered, any indent.
func mdIsListLine(line string) bool {
	return mdIsBullet(line) || mdListNum.MatchString(strings.TrimSpace(line))
}

// mdListDepthCap bounds nesting — deeper indents clamp to the cap
// instead of opening runaway levels.
const mdListDepthCap = 8

// mdRenderList emits one list block: consecutive list lines, nested by
// indentation (two or more extra spaces = one level deeper, the GFM
// convention), with ul/ol switching per level and task-list items
// ("[ ] " / "[x] ") rendered as checkbox glyphs. Returns the next line
// index.
func mdRenderList(b *strings.Builder, lines []string, i int, mc mdContext) int {
	type frame struct {
		indent int
		tag    string
	}
	var stack []frame
	closeOne := func() {
		b.WriteString("</" + stack[len(stack)-1].tag + ">")
		stack = stack[:len(stack)-1]
	}
	for i < len(lines) && mdIsListLine(lines[i]) {
		line := lines[i]
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		trimmed := strings.TrimSpace(line)
		tag, item := "ul", ""
		if mdIsBullet(line) {
			item = trimmed[2:]
		} else {
			tag = "ol"
			item = trimmed[strings.Index(trimmed, " ")+1:]
		}
		// Unwind levels this item sits above, then open one when it sits
		// two-plus spaces deeper than the current level (capped).
		for len(stack) > 1 && indent <= stack[len(stack)-1].indent-2 {
			closeOne()
		}
		switch {
		case len(stack) == 0:
			stack = append(stack, frame{indent, tag})
			b.WriteString("<" + tag + ">")
		case indent >= stack[len(stack)-1].indent+2 && len(stack) < mdListDepthCap:
			stack = append(stack, frame{indent, tag})
			b.WriteString("<" + tag + ">")
		case tag != stack[len(stack)-1].tag:
			// Same level, other list kind: close and reopen.
			b.WriteString("</" + stack[len(stack)-1].tag + ">")
			stack[len(stack)-1] = frame{stack[len(stack)-1].indent, tag}
			b.WriteString("<" + tag + ">")
		}
		b.WriteString("<li>")
		b.WriteString(mdListItem(item, mc))
		b.WriteString("</li>")
		i++
	}
	for len(stack) > 0 {
		closeOne()
	}
	return i
}

// mdListItem renders one item's text, turning a leading task marker
// into a checkbox glyph — plain text glyphs, so nothing interactive
// (or sanitizer-fragile) is ever emitted.
func mdListItem(item string, mc mdContext) string {
	if rest, ok := strings.CutPrefix(item, "[ ] "); ok {
		return "☐ " + mdInline(rest, mc)
	}
	if rest, ok := strings.CutPrefix(item, "[x] "); ok {
		return "☑ " + mdInline(rest, mc)
	}
	if rest, ok := strings.CutPrefix(item, "[X] "); ok {
		return "☑ " + mdInline(rest, mc)
	}
	return mdInline(item, mc)
}

// mdSetextLevel reports a setext underline's heading level: a line of
// only = signs is 1, only hyphens is 2, anything else 0. ("---" as its
// own block is still a horizontal rule — the hr case matches first;
// this only fires under a text line.)
func mdSetextLevel(trimmed string) int {
	if trimmed == "" {
		return 0
	}
	eq, dash := true, true
	for _, r := range trimmed {
		eq = eq && r == '='
		dash = dash && r == '-'
	}
	switch {
	case eq:
		return 1
	case dash:
		return 2
	}
	return 0
}

// --- tables (GFM pipe tables, §5.2) -----------------------------------------

// mdTableSepCell matches one delimiter-row cell (:---, ---:, :---:).
var mdTableSepCell = regexp.MustCompile(`^:?-+:?$`)

// mdSplitRow splits one table row into cells: optional outer pipes,
// backslash-escaped pipes stay literal.
func mdSplitRow(line string) []string {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	t = strings.ReplaceAll(t, `\|`, "\x00")
	parts := strings.Split(t, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(strings.ReplaceAll(parts[i], "\x00", "|"))
	}
	return parts
}

// mdIsTableStart reports whether a header row + delimiter row begin at
// line i: both contain pipes, every delimiter cell is dashes-with-
// optional-colons, and the cell counts agree — so prose that merely
// contains a pipe never becomes a table.
func mdIsTableStart(lines []string, i int) bool {
	if i+1 >= len(lines) || !strings.Contains(lines[i], "|") || !strings.Contains(lines[i+1], "|") {
		return false
	}
	sep := mdSplitRow(lines[i+1])
	if len(sep) == 0 || len(sep) != len(mdSplitRow(lines[i])) {
		return false
	}
	for _, c := range sep {
		if !mdTableSepCell.MatchString(c) {
			return false
		}
	}
	return true
}

// mdRenderTable emits one pipe table: the header row, the alignment
// from the delimiter row (as align attributes the sanitizer admits from
// its fixed value set), then every following row that contains a pipe.
// Short rows pad, long rows truncate — the column count is the header's.
func mdRenderTable(b *strings.Builder, lines []string, i int, mc mdContext) int {
	head := mdSplitRow(lines[i])
	aligns := make([]string, len(head))
	for j, c := range mdSplitRow(lines[i+1]) {
		l, r := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		switch {
		case l && r:
			aligns[j] = "center"
		case r:
			aligns[j] = "right"
		}
	}
	cell := func(tag, text string, col int) {
		b.WriteString("<" + tag)
		if aligns[col] != "" {
			b.WriteString(` align="` + aligns[col] + `"`)
		}
		b.WriteString(">")
		b.WriteString(mdInline(text, mc))
		b.WriteString("</" + tag + ">")
	}
	b.WriteString("<table><thead><tr>")
	for j, h := range head {
		cell("th", h, j)
	}
	b.WriteString("</tr></thead><tbody>")
	i += 2
	for i < len(lines) && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != "" {
		row := mdSplitRow(lines[i])
		b.WriteString("<tr>")
		for j := range head {
			text := ""
			if j < len(row) {
				text = row[j]
			}
			cell("td", text, j)
		}
		b.WriteString("</tr>")
		i++
	}
	b.WriteString("</tbody></table>")
	return i
}

// mdHeadingLevel reports the heading level of a trimmed line, or 0.
// A heading needs a SPACE after its #s ("# title"), so an issue
// reference starting a line ("#123 broke it") stays a paragraph and
// autolinks instead of becoming an h1.
func mdHeadingLevel(trimmed string) int {
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' && level < 6 {
		level++
	}
	if level == 0 || level >= len(trimmed) || trimmed[level] != ' ' {
		return 0
	}
	return level
}

// mdInline renders inline formatting over one text run: code spans are
// carved out FIRST (escaped, never formatted, never autolinked), the
// rest is escaped and marked up.
func mdInline(text string, mc mdContext) string {
	var b strings.Builder
	for {
		start := strings.IndexByte(text, '`')
		if start < 0 {
			b.WriteString(mdFormatRun(text, mc))
			return b.String()
		}
		end := strings.IndexByte(text[start+1:], '`')
		if end < 0 {
			b.WriteString(mdFormatRun(text, mc))
			return b.String()
		}
		b.WriteString(mdFormatRun(text[:start], mc))
		b.WriteString("<code>")
		b.WriteString(html.EscapeString(text[start+1 : start+1+end]))
		b.WriteString("</code>")
		text = text[start+end+2:]
	}
}

// mdFormatRun escapes one run and applies the fixed tag set.
func mdFormatRun(text string, mc mdContext) string {
	s := html.EscapeString(text)
	// Links first — the target must not be chewed up by emphasis.
	s = mdLink.ReplaceAllStringFunc(s, func(m string) string {
		parts := mdLink.FindStringSubmatch(m)
		bang, label, target := parts[1], parts[2], parts[3]
		// Repo-relative image targets resolve to the repo's OWN raw
		// endpoint (SafeAttachmentCT + nosniff there neuter anything
		// active) — a README shows its own screenshots. Remote images
		// still render as links: repo pages never fetch remote content
		// (same privacy stance as mail).
		if bang == "!" {
			if src, ok := mdResolveRel(mc.RawBase, mc.Dir, target); ok {
				return `<img src="` + src + `" alt="` + label + `">`
			}
		}
		if mdSafeTarget(target) {
			return `<a href="` + target + `" rel="noopener noreferrer nofollow">` + label + `</a>`
		}
		// Repo-relative link targets land on the blob view.
		if href, ok := mdResolveRel(mc.BlobBase, mc.Dir, target); ok {
			return `<a href="` + href + `">` + label + `</a>`
		}
		return m
	})
	s = mdAutolink(s, mc)
	s = mdBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = mdStrike.ReplaceAllString(s, "<s>$1</s>")
	s = mdItalicA.ReplaceAllString(s, "<em>$1</em>")
	s = mdItalicU.ReplaceAllString(s, "<em>$1</em>")
	return strings.ReplaceAll(s, "\n", "<br>")
}

// mdSafeTarget admits link targets that can't run script (escaped
// already, so quotes arrived as &quot; and can't appear here).
func mdSafeTarget(t string) bool {
	l := strings.ToLower(t)
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") ||
		strings.HasPrefix(l, "mailto:") || strings.HasPrefix(l, "#")
}

// mdResolveRel resolves a repo-relative target against base (the repo's
// raw or blob endpoint, "/git/<ns>/<repo>/raw/<ref>") and the markdown
// file's directory. Rooted path.Clean makes escaping the repo tree
// impossible by construction — "../" clamps at the repo root, schemes
// and absolute paths are refused, and the git tree lookup on the other
// end resolves nothing outside the repository anyway.
func mdResolveRel(base, dir, target string) (string, bool) {
	if base == "" || target == "" {
		return "", false
	}
	if strings.HasPrefix(target, "/") || strings.Contains(target, "\\") {
		return "", false
	}
	// A colon before any slash marks a scheme (mailto:, data:,
	// javascript:, vbscript:, …) — never a repo path.
	if c := strings.IndexByte(target, ':'); c >= 0 {
		s := strings.IndexByte(target, '/')
		if s < 0 || c < s {
			return "", false
		}
	}
	pathPart, frag, _ := strings.Cut(target, "#")
	pathPart = strings.TrimPrefix(pathPart, "./")
	if pathPart == "" {
		return "", false
	}
	resolved := path.Clean("/" + dir + "/" + pathPart)
	out := base + resolved
	if frag != "" {
		out += "#" + frag
	}
	return out, true
}

// mdAutolink wraps bare URLs (and, with a context, #N/@mention refs).
// Segments already inside an <a>…</a> (the only anchors that can exist
// — the input was escaped) are skipped, so explicit links never
// double-wrap and refs never rewrite anchor internals.
func mdAutolink(s string, mc mdContext) string {
	var out strings.Builder
	for {
		i := strings.Index(s, "<a ")
		if i < 0 {
			out.WriteString(mdAutolinkPlain(s, mc))
			return out.String()
		}
		out.WriteString(mdAutolinkPlain(s[:i], mc))
		j := strings.Index(s[i:], "</a>")
		if j < 0 {
			out.WriteString(s[i:])
			return out.String()
		}
		out.WriteString(s[i : i+j+4])
		s = s[i+j+4:]
	}
}

// mdAutolinkPlain rewrites one anchor-free, already-escaped segment:
// #N and @mention refs first (their anchors carry no scheme, so the URL
// pass below can't re-match inside them), then bare URLs.
func mdAutolinkPlain(s string, mc mdContext) string {
	if mc.RepoPath != "" {
		// mc.RepoPath is a validated ns/name and $2 is digits — both
		// key-charset safe, so the href needs no further escaping (§8).
		s = mdIssueRef.ReplaceAllString(s,
			`$1<a href="/git/`+mc.RepoPath+`/issues/$2">#$2</a>`)
	}
	if mc.UserExists != nil {
		s = mdMentionRef.ReplaceAllStringFunc(s, func(m string) string {
			parts := mdMentionRef.FindStringSubmatch(m)
			name := strings.ToLower(parts[2])
			if !mc.UserExists(name) {
				return m // nonexistent users render as plain text (§11's twin rule)
			}
			return parts[1] + `<a href="/git/` + name + `">@` + parts[2] + `</a>`
		})
	}
	return mdURL.ReplaceAllStringFunc(s, func(m string) string {
		return fmt.Sprintf(`<a href="%s" rel="noopener noreferrer nofollow">%s</a>`, m, m)
	})
}
