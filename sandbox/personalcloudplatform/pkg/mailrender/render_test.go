package mailrender

import (
	"fmt"
	"strings"
	"testing"
)

// TestSanitizeHTML is the ported PCD sanitizer suite: the whitelist,
// the drop list, the href allowlist, and the click-to-load rewrite are
// documented guarantees (spec §7.3).
func TestSanitizeHTML(t *testing.T) {
	cases := []struct {
		name, in  string
		wants     []string
		forbidden []string
	}{
		{
			name:      "script drops with its content",
			in:        `<p>hi</p><script>alert(1)</script>`,
			wants:     []string{"<p>hi</p>"},
			forbidden: []string{"script", "alert"},
		},
		{
			name:      "style drops with its content",
			in:        `<style>body{background:url(x)}</style><b>ok</b>`,
			wants:     []string{"<b>ok</b>"},
			forbidden: []string{"style", "url("},
		},
		{
			name:      "iframe/object/embed drop",
			in:        `<iframe src="https://evil"></iframe><object data="x"></object><embed src="y"><i>kept</i>`,
			wants:     []string{"<i>kept</i>"},
			forbidden: []string{"iframe", "object", "embed", "evil"},
		},
		{
			name:      "form controls drop",
			in:        `<form action="https://evil"><input value="x"><button>go</button></form><p>t</p>`,
			wants:     []string{"<p>t</p>"},
			forbidden: []string{"form", "input", "button"},
		},
		{
			name:      "unknown tags unwrap, content survives",
			in:        `<article><blink>hello</blink></article>`,
			wants:     []string{"hello"},
			forbidden: []string{"article", "blink"},
		},
		{
			name:      "event handlers and style attributes vanish",
			in:        `<p onclick="alert(1)" style="position:fixed">x</p>`,
			wants:     []string{"<p>x</p>"},
			forbidden: []string{"onclick", "style", "alert"},
		},
		{
			name:      "https href allowed with rel hardening",
			in:        `<a href="https://ok.example/a">go</a>`,
			wants:     []string{`href="https://ok.example/a"`, `rel="noopener noreferrer nofollow"`, `target="_blank"`},
			forbidden: nil,
		},
		{
			name:      "mailto href allowed",
			in:        `<a href="mailto:x@y.z">mail</a>`,
			wants:     []string{`href="mailto:x@y.z"`},
			forbidden: nil,
		},
		{
			name:      "javascript href stripped",
			in:        `<a href="javascript:alert(1)">x</a>`,
			wants:     []string{"<a"},
			forbidden: []string{"javascript", "href"},
		},
		{
			name:      "data URI href stripped",
			in:        `<a href="data:text/html,<script>1</script>">x</a>`,
			wants:     []string{"<a>x</a>"},
			forbidden: []string{"data:"},
		},
		{
			name:      "remote img rewrites to click-to-load",
			in:        `<img src="https://tracker.example/pixel.gif">`,
			wants:     []string{`data-mail-src="https://tracker.example/pixel.gif"`, `alt="[blocked remote image]"`},
			forbidden: []string{` src=`},
		},
		{
			name:      "non-http img src drops entirely",
			in:        `<img src="data:image/svg+xml,<svg onload=alert(1)>">`,
			wants:     []string{"<img"},
			forbidden: []string{"data:", "svg", "onload"},
		},
		{
			name:      "svg and math drop with children",
			in:        `<svg><script>1</script></svg><math><mi>x</mi></math><u>u</u>`,
			wants:     []string{"<u>u</u>"},
			forbidden: []string{"svg", "math", "script"},
		},
		{
			name:      "text escapes",
			in:        `5 < 6 & "quotes"`,
			wants:     []string{"5 &lt; 6 &amp;"},
			forbidden: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := SanitizeHTML(tc.in)
			for _, w := range tc.wants {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q:\n%s", w, out)
				}
			}
			for _, f := range tc.forbidden {
				if strings.Contains(strings.ToLower(out), strings.ToLower(f)) {
					t.Errorf("output leaked %q:\n%s", f, out)
				}
			}
		})
	}
}

// TestSafeAttachmentCT is the active-content override.
func TestSafeAttachmentCT(t *testing.T) {
	cases := map[string]string{
		"":                        "application/octet-stream",
		"text/html":               "application/octet-stream",
		"text/html; charset=utf8": "application/octet-stream",
		"image/svg+xml":           "application/octet-stream",
		"application/xhtml+xml":   "application/octet-stream",
		"text/xml":                "application/octet-stream",
		"text/javascript":         "application/octet-stream",
		"application/pdf":         "application/pdf",
		"image/png":               "image/png",
		"text/plain":              "text/plain",
	}
	for in, want := range cases {
		if got := SafeAttachmentCT(in); got != want {
			t.Errorf("SafeAttachmentCT(%q) = %q, want %q", in, got, want)
		}
	}
}

// mimeFixture is a multipart/mixed message: alternative text+html body
// (with a script and a remote img in the HTML) plus a PDF attachment.
func mimeFixture() []byte {
	var b strings.Builder
	b.WriteString("From: Bob <bob@remote.example>\r\n")
	b.WriteString("To: ada@example.test\r\n")
	b.WriteString("Subject: fixture\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=OUTER\r\n\r\n")
	b.WriteString("--OUTER\r\nContent-Type: multipart/alternative; boundary=INNER\r\n\r\n")
	b.WriteString("--INNER\r\nContent-Type: text/plain\r\n\r\nplain body\r\n")
	b.WriteString("--INNER\r\nContent-Type: text/html\r\n\r\n")
	b.WriteString(`<p>rich <strong>body</strong></p><script>alert(1)</script><img src="https://tracker.example/p.gif">` + "\r\n")
	b.WriteString("--INNER--\r\n")
	b.WriteString("--OUTER\r\nContent-Type: application/pdf; name=\"doc.pdf\"\r\n")
	b.WriteString("Content-Disposition: attachment; filename=\"doc.pdf\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	b.WriteString("JVBERi0xLjQK\r\n") // "%PDF-1.4\n"
	b.WriteString("--OUTER--\r\n")
	return []byte(b.String())
}

// TestRender covers the MIME walk: text + sanitized html + attachment
// metadata off one multipart message.
func TestRender(t *testing.T) {
	out := Render(mimeFixture())
	if !strings.Contains(out.Text, "plain body") {
		t.Errorf("text body missing: %q", out.Text)
	}
	if !strings.Contains(out.HTML, "<strong>body</strong>") {
		t.Errorf("html body missing: %q", out.HTML)
	}
	if strings.Contains(out.HTML, "script") || strings.Contains(out.HTML, "alert") {
		t.Errorf("script survived render: %q", out.HTML)
	}
	if !strings.Contains(out.HTML, `data-mail-src="https://tracker.example/p.gif"`) {
		t.Errorf("remote img not rewritten: %q", out.HTML)
	}
	if len(out.Atts) != 1 || out.Atts[0].Name != "doc.pdf" || out.Atts[0].CT != "application/pdf" {
		t.Fatalf("attachment meta wrong: %+v", out.Atts)
	}
	if out.Atts[0].Size != len("%PDF-1.4\n") {
		t.Errorf("attachment size = %d", out.Atts[0].Size)
	}
}

// TestAttachmentPart decodes attachment #0 and refuses junk indexes.
func TestAttachmentPart(t *testing.T) {
	raw := mimeFixture()
	att, ok := AttachmentPart(raw, "0")
	if !ok || att.Name != "doc.pdf" || string(att.Data) != "%PDF-1.4\n" {
		t.Fatalf("attachment part: ok=%v %+v", ok, att)
	}
	for _, junk := range []string{"1", "-1", "x", "9999"} {
		if _, ok := AttachmentPart(raw, junk); ok {
			t.Errorf("AttachmentPart(%q) matched", junk)
		}
	}
}

// TestRenderUnparseable degrades to the raw body, never errors.
func TestRenderUnparseable(t *testing.T) {
	out := Render([]byte("total junk\r\n\r\nbody here"))
	if out.Text == "" {
		t.Error("unparseable message rendered empty")
	}
}

// TestRenderCapsHostileSize keeps a giant html body bounded.
func TestRenderCapsHostileSize(t *testing.T) {
	huge := fmt.Sprintf("From: a@b.c\r\nMIME-Version: 1.0\r\nContent-Type: text/html\r\n\r\n<p>%s</p>",
		strings.Repeat("A", 3<<20))
	out := Render([]byte(huge))
	if len(out.HTML) > maxRenderHTML {
		t.Errorf("html body exceeds cap: %d", len(out.HTML))
	}
}
