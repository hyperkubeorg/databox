package messenger

import (
	"strings"
	"testing"
)

// The renderer is safe by construction: no raw HTML in the input can reach
// the output as markup.
func TestMarkdownEscapesHTML(t *testing.T) {
	cases := []string{
		`<script>alert(1)</script>`,
		`<img src=x onerror=alert(1)>`,
		`a <b>bold</b> tag`,
		`click <a href="javascript:evil()">here</a>`,
	}
	// Safety property: no raw tag from the input survives. Everything is
	// escaped first, so the only '<' in the output belong to the renderer's
	// own known-safe tags — never a <script>, <img>, <b>, or a
	// javascript:-scheme anchor. (Escaped forms like &lt;img ...&gt; are
	// inert plain text and expected.)
	rawTags := []string{"<script", "<img", "<b>", "<a href=\"javascript"}
	for _, in := range cases {
		out := RenderMarkdown(in)
		for _, bad := range rawTags {
			if strings.Contains(strings.ToLower(out), bad) {
				t.Fatalf("unsafe output for %q contained %q:\n%s", in, bad, out)
			}
		}
	}
}

// Inline formatting produces the expected known-safe tags.
func TestMarkdownFormatting(t *testing.T) {
	checks := map[string]string{
		"**bold**":         "<strong>bold</strong>",
		"*italic*":         "<em>italic</em>",
		"~~gone~~":         "<del>gone</del>",
		"use `code` here":  "<code>code</code>",
		"a ||secret|| bit": `<span class="msg-spoiler">secret</span>`,
	}
	for in, want := range checks {
		if out := RenderMarkdown(in); !strings.Contains(out, want) {
			t.Fatalf("RenderMarkdown(%q) = %q, want it to contain %q", in, out, want)
		}
	}
}

// Formatting inside an inline code span is NOT applied, and the content is
// escaped.
func TestMarkdownCodeSpanLiteral(t *testing.T) {
	out := RenderMarkdown("`**not bold** <x>`")
	if strings.Contains(out, "<strong>") || strings.Contains(out, "<x>") {
		t.Fatalf("code span was formatted or unescaped: %s", out)
	}
	if !strings.Contains(out, "&lt;x&gt;") {
		t.Fatalf("code span not escaped: %s", out)
	}
}

// Fenced code blocks render as a preformatted block with escaped content.
func TestMarkdownFencedCode(t *testing.T) {
	out := RenderMarkdown("```\n<b>x</b>\n```")
	if !strings.Contains(out, "<pre><code>") || !strings.Contains(out, "&lt;b&gt;") {
		t.Fatalf("fenced code wrong: %s", out)
	}
}

// Autolinking wraps a bare URL and marks it noopener/nofollow.
func TestMarkdownAutolink(t *testing.T) {
	out := RenderMarkdown("see https://example.com/x now")
	if !strings.Contains(out, `href="https://example.com/x"`) || !strings.Contains(out, "nofollow") {
		t.Fatalf("autolink wrong: %s", out)
	}
}
