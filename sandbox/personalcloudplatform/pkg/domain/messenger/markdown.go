// markdown.go — the safe chat-markdown renderer (Messenger §5).
// Discord-flavored subset: fenced + inline code, bold, italic,
// strikethrough, spoilers, blockquotes, unordered lists, and autolinked
// http(s) URLs. Safe BY CONSTRUCTION: every run of user text is
// HTML-escaped first, and the renderer only ever inserts a fixed, known
// set of tags — no untrusted byte can become markup. (This is the same
// discipline pkg/mailrender enforces for email, applied to a controlled
// grammar so no separate sanitizer pass is needed.)
package messenger

import (
	"html"
	"regexp"
	"strings"
)

// MaxMessageRunes bounds a single message body (defense against a runaway
// paste; the site's byte cap still applies to attachments).
const MaxMessageRunes = 8000

var (
	reBold    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reStrike  = regexp.MustCompile(`~~(.+?)~~`)
	reSpoiler = regexp.MustCompile(`\|\|(.+?)\|\|`)
	reItalicA = regexp.MustCompile(`\*(.+?)\*`)
	reItalicU = regexp.MustCompile(`_(.+?)_`)
	// Autolink bare http(s) URLs. Operates on already-escaped text, so the
	// match can't contain '<'; a trailing ')' or '.' is left out of the URL.
	reURL = regexp.MustCompile(`https?://[^\s<]+[^\s<.,)]`)
)

// RenderMarkdown turns a raw message body into safe HTML. The output is
// trusted (only known tags over escaped text) and is stored as the HTML
// cache on the message.
func RenderMarkdown(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(raw, "\n")

	var b strings.Builder
	i := 0
	for i < len(lines) {
		line := lines[i]

		// Fenced code block: ``` … ```
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			var code []string
			i++
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			i++ // consume the closing fence (or EOF)
			b.WriteString("<pre><code>")
			b.WriteString(html.EscapeString(strings.Join(code, "\n")))
			b.WriteString("</code></pre>")
			continue
		}

		// Blockquote: consecutive "> " lines.
		if strings.HasPrefix(line, ">") {
			var quote []string
			for i < len(lines) && strings.HasPrefix(lines[i], ">") {
				quote = append(quote, strings.TrimPrefix(strings.TrimPrefix(lines[i], ">"), " "))
				i++
			}
			b.WriteString("<blockquote>")
			b.WriteString(inline(strings.Join(quote, "\n")))
			b.WriteString("</blockquote>")
			continue
		}

		// Unordered list: consecutive "- " / "* " lines.
		if isListItem(line) {
			b.WriteString("<ul>")
			for i < len(lines) && isListItem(lines[i]) {
				item := strings.TrimSpace(lines[i])[2:]
				b.WriteString("<li>")
				b.WriteString(inline(item))
				b.WriteString("</li>")
				i++
			}
			b.WriteString("</ul>")
			continue
		}

		// Blank line: paragraph break.
		if strings.TrimSpace(line) == "" {
			i++
			continue
		}

		// Paragraph: gather until a blank line or a block starter.
		var para []string
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" &&
			!strings.HasPrefix(strings.TrimSpace(lines[i]), "```") &&
			!strings.HasPrefix(lines[i], ">") && !isListItem(lines[i]) {
			para = append(para, lines[i])
			i++
		}
		b.WriteString("<p>")
		b.WriteString(inline(strings.Join(para, "\n")))
		b.WriteString("</p>")
	}
	return b.String()
}

func isListItem(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ")
}

// inline renders inline formatting over a text run. Inline code spans are
// extracted FIRST (their contents are escaped but never formatted), then
// the remaining text is escaped and marked up. Newlines become <br>.
func inline(text string) string {
	var b strings.Builder
	for {
		start := strings.IndexByte(text, '`')
		if start < 0 {
			b.WriteString(formatRun(text))
			break
		}
		end := strings.IndexByte(text[start+1:], '`')
		if end < 0 {
			b.WriteString(formatRun(text))
			break
		}
		end = start + 1 + end
		b.WriteString(formatRun(text[:start]))
		b.WriteString("<code>")
		b.WriteString(html.EscapeString(text[start+1 : end]))
		b.WriteString("</code>")
		text = text[end+1:]
	}
	return b.String()
}

// formatRun escapes a plain run and applies the inline tag grammar. Order:
// escape, then bold/strike/spoiler/italic (bold before italic so ** wins
// over *), then autolink, then newline → <br>.
func formatRun(run string) string {
	if run == "" {
		return ""
	}
	out := html.EscapeString(run)
	out = reBold.ReplaceAllString(out, "<strong>$1</strong>")
	out = reStrike.ReplaceAllString(out, "<del>$1</del>")
	out = reSpoiler.ReplaceAllString(out, `<span class="msg-spoiler">$1</span>`)
	out = reItalicA.ReplaceAllString(out, "<em>$1</em>")
	out = reItalicU.ReplaceAllString(out, "<em>$1</em>")
	// @mentions render as highlighted chips (the handle is already escaped,
	// so this only wraps known-safe text). Notification targeting happens
	// separately at send time (mentions.go).
	out = reMention.ReplaceAllString(out, `<span class="msg-mention">@$1</span>`)
	out = reURL.ReplaceAllStringFunc(out, func(u string) string {
		return `<a href="` + u + `" rel="noopener noreferrer nofollow" target="_blank">` + u + `</a>`
	})
	out = strings.ReplaceAll(out, "\n", "<br>")
	return out
}
