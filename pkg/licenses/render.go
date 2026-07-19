// render.go — a small, dependency-free Markdown-to-HTML renderer scoped
// to exactly the constructs LICENSE-REVIEW.md uses: ATX headings, GFM
// pipe tables, ordered/unordered lists, blockquotes, fenced code blocks,
// and inline code / bold / italic / links / bare-URL autolinks. It is not
// a general Markdown engine — keeping it narrow avoids pulling a Markdown
// dependency into an airgapped binary just to display one embedded file.
package licenses

import (
	"html"
	"regexp"
	"strconv"
	"strings"
)

// --- inline ------------------------------------------------------------------

var (
	reCode   = regexp.MustCompile("`([^`]+)`")
	reLink   = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reAuto   = regexp.MustCompile(`https?://[^\s<]+`)
	reBold   = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reItalic = regexp.MustCompile(`\*([^*]+)\*`)
)

// Private-use sentinels wrap placeholders so bold/italic/autolink passes
// never see the protected content they stand in for.
func codeToken(i int) string { return "C" + strconv.Itoa(i) + "" }
func linkToken(i int) string { return "L" + strconv.Itoa(i) + "" }

func anchor(url, text string) string {
	return `<a href="` + url + `" target="_blank" rel="noopener noreferrer">` + text + `</a>`
}

// renderInline turns one line of Markdown-ish text into HTML. The input
// is trusted (our own embedded file) but still escaped for correctness.
func renderInline(s string) string {
	s = html.EscapeString(s)

	// 1. Code spans → placeholders (content already escaped).
	var codes []string
	s = reCode.ReplaceAllStringFunc(s, func(m string) string {
		sub := reCode.FindStringSubmatch(m)
		codes = append(codes, "<code>"+sub[1]+"</code>")
		return codeToken(len(codes) - 1)
	})

	// 2. Markdown links → placeholders (link text may hold code tokens).
	var links []string
	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		sub := reLink.FindStringSubmatch(m)
		links = append(links, anchor(sub[2], sub[1]))
		return linkToken(len(links) - 1)
	})

	// 3. Autolink bare URLs left in the text (e.g. table cells), keeping
	//    any trailing sentence punctuation outside the link.
	s = reAuto.ReplaceAllStringFunc(s, func(u string) string {
		trimmed := strings.TrimRight(u, ".,);:")
		suffix := u[len(trimmed):]
		links = append(links, anchor(trimmed, trimmed))
		return linkToken(len(links)-1) + suffix
	})

	// 4. Emphasis (bold before italic).
	s = reBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = reItalic.ReplaceAllString(s, "<em>$1</em>")

	// 5. Restore placeholders: links first, then the code they may wrap.
	for i := len(links) - 1; i >= 0; i-- {
		s = strings.Replace(s, linkToken(i), links[i], 1)
	}
	for i := len(codes) - 1; i >= 0; i-- {
		s = strings.Replace(s, codeToken(i), codes[i], 1)
	}
	return s
}

// --- block -------------------------------------------------------------------

var (
	reHeading = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reOrdered = regexp.MustCompile(`^\s*\d+\.\s+(.*)$`)
	reTableSep = regexp.MustCompile(`^\s*\|?[\s:|-]+\|?\s*$`)
)

func isBlank(s string) bool  { return strings.TrimSpace(s) == "" }
func isTable(s string) bool  { return strings.HasPrefix(strings.TrimSpace(s), "|") }
func isQuote(s string) bool  { return strings.HasPrefix(strings.TrimSpace(s), ">") }
func isBullet(s string) bool { t := strings.TrimSpace(s); return strings.HasPrefix(t, "- ") }

// renderMarkdown converts the whole document body to HTML.
func renderMarkdown(md string) string {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var b strings.Builder
	i := 0
	for i < len(lines) {
		line := lines[i]

		switch {
		case isBlank(line):
			i++

		case strings.HasPrefix(strings.TrimSpace(line), "```"):
			// Fenced code block: copy verbatim until the closing fence.
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			i++ // consume closing fence (if present)
			b.WriteString("<pre><code>")
			b.WriteString(html.EscapeString(strings.Join(code, "\n")))
			b.WriteString("\n</code></pre>\n")

		case reHeading.MatchString(line):
			m := reHeading.FindStringSubmatch(line)
			lvl := strconv.Itoa(len(m[1]))
			b.WriteString("<h" + lvl + ">" + renderInline(m[2]) + "</h" + lvl + ">\n")
			i++

		case isTable(line):
			i = renderTable(&b, lines, i)

		case isBullet(line):
			b.WriteString("<ul>\n")
			for i < len(lines) && isBullet(lines[i]) {
				item := strings.TrimSpace(lines[i])[2:]
				b.WriteString("<li>" + renderInline(item) + "</li>\n")
				i++
			}
			b.WriteString("</ul>\n")

		case reOrdered.MatchString(line):
			b.WriteString("<ol>\n")
			for i < len(lines) && reOrdered.MatchString(lines[i]) {
				item := reOrdered.FindStringSubmatch(lines[i])[1]
				b.WriteString("<li>" + renderInline(item) + "</li>\n")
				i++
			}
			b.WriteString("</ol>\n")

		case isQuote(line):
			var quoted []string
			for i < len(lines) && isQuote(lines[i]) {
				t := strings.TrimSpace(lines[i])
				t = strings.TrimPrefix(strings.TrimPrefix(t, ">"), " ")
				quoted = append(quoted, renderInline(t))
				i++
			}
			b.WriteString("<blockquote><p>" + strings.Join(quoted, "<br>\n") + "</p></blockquote>\n")

		default:
			// Paragraph: gather until a blank line or a block starter.
			// Intra-paragraph newlines become <br> so the metadata block
			// and version note keep their intended line breaks.
			var para []string
			for i < len(lines) && !isBlank(lines[i]) &&
				!isTable(lines[i]) && !isBullet(lines[i]) &&
				!reOrdered.MatchString(lines[i]) && !isQuote(lines[i]) &&
				!reHeading.MatchString(lines[i]) &&
				!strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				para = append(para, renderInline(lines[i]))
				i++
			}
			b.WriteString("<p>" + strings.Join(para, "<br>\n") + "</p>\n")
		}
	}
	return b.String()
}

// renderTable consumes a GFM pipe table starting at lines[start] and
// returns the index just past it.
func renderTable(b *strings.Builder, lines []string, start int) int {
	cells := func(row string) []string {
		row = strings.TrimSpace(row)
		row = strings.TrimPrefix(row, "|")
		row = strings.TrimSuffix(row, "|")
		parts := strings.Split(row, "|")
		for j := range parts {
			parts[j] = strings.TrimSpace(parts[j])
		}
		return parts
	}

	b.WriteString(`<div class="tablewrap"><table>`)
	i := start
	header := cells(lines[i])
	i++
	// Skip the |---|---| separator row if present.
	if i < len(lines) && reTableSep.MatchString(lines[i]) {
		i++
	}
	b.WriteString("<thead><tr>")
	for _, c := range header {
		b.WriteString("<th>" + renderInline(c) + "</th>")
	}
	b.WriteString("</tr></thead><tbody>")
	for i < len(lines) && isTable(lines[i]) {
		b.WriteString("<tr>")
		for _, c := range cells(lines[i]) {
			b.WriteString("<td>" + renderInline(c) + "</td>")
		}
		b.WriteString("</tr>")
		i++
	}
	b.WriteString("</tbody></table></div>\n")
	return i
}

// --- page --------------------------------------------------------------------

// renderPage wraps the rendered body in a self-contained, theme-aware
// HTML document with no external assets, so it renders identically in an
// airgapped browser.
func renderPage(md string) []byte {
	body := renderMarkdown(md)
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Third-Party License Report</title>
<style>` + pageCSS + `</style>
</head>
<body>
<main>
`)
	b.WriteString(body)
	b.WriteString(`</main>
</body>
</html>
`)
	return []byte(b.String())
}

const pageCSS = `
:root{color-scheme:light dark;
  --bg:#ffffff;--fg:#1a1a1a;--muted:#5b6470;--border:#e2e5e9;
  --thead:#f5f6f8;--code-bg:#f0f1f3;--link:#0b62d6;--quote:#f7f8fa;}
@media (prefers-color-scheme:dark){:root{
  --bg:#14171a;--fg:#e6e8eb;--muted:#9aa4af;--border:#2a2f36;
  --thead:#1c2126;--code-bg:#20262c;--link:#6ea8ff;--quote:#1a1e22;}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);
  font:16px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;}
main{max-width:960px;margin:0 auto;padding:2.5rem 1.25rem 4rem;}
h1{font-size:1.9rem;margin:0 0 1rem;line-height:1.25}
h2{font-size:1.4rem;margin:2.2rem 0 .8rem;padding-top:.6rem;border-top:1px solid var(--border)}
h3{font-size:1.12rem;margin:1.6rem 0 .6rem}
p{margin:.7rem 0}
a{color:var(--link);text-decoration:none;overflow-wrap:anywhere}
a:hover{text-decoration:underline}
code{background:var(--code-bg);padding:.12em .35em;border-radius:4px;
  font:0.86em/1.4 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;overflow-wrap:anywhere}
pre{background:var(--code-bg);padding:1rem;border-radius:8px;overflow-x:auto}
pre code{background:none;padding:0}
ul,ol{margin:.6rem 0;padding-left:1.4rem}
li{margin:.3rem 0}
blockquote{margin:1rem 0;padding:.6rem 1rem;background:var(--quote);
  border-left:3px solid var(--border);border-radius:4px;color:var(--muted)}
blockquote p{margin:0}
.tablewrap{overflow-x:auto;margin:1rem 0}
table{border-collapse:collapse;width:100%;font-size:.92rem}
th,td{border:1px solid var(--border);padding:.45rem .6rem;text-align:left;vertical-align:top}
thead th{background:var(--thead)}
tbody tr:nth-child(even){background:color-mix(in srgb,var(--thead) 45%,transparent)}
`
