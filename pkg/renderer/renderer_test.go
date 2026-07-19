// renderer_test.go proves the embedded template set is sound: every .tpl
// file parses against the base layout and renders without error. This is
// the "templates cannot break in front of a user" guarantee — a template
// syntax error fails this test (and MustNew at boot) instead of a request.
package renderer

import (
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/pkg/templates"
)

// TestAllTemplatesParse checks that New() succeeds and that every .tpl in
// pkg/templates (minus the base layout itself) ends up renderable.
func TestAllTemplatesParse(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	files, err := fs.Glob(templates.FS, "*.tpl")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	want := map[string]bool{}
	for _, f := range files {
		if f != "base.tpl" {
			want[f] = true
		}
	}
	got := map[string]bool{}
	for _, n := range r.Names() {
		got[n] = true
	}
	for f := range want {
		if !got[f] {
			t.Errorf("template %s embedded but not parsed as a page", f)
		}
	}
	if len(got) != len(want) {
		t.Errorf("parsed pages %v != embedded pages %v", r.Names(), files)
	}
}

// TestRequiredPagesPresent pins the page set: every §4 view must ship a
// template. A renamed or dropped .tpl fails here even though the glob
// tests above would happily pass with fewer pages.
func TestRequiredPagesPresent(t *testing.T) {
	r := MustNew()
	got := map[string]bool{}
	for _, n := range r.Names() {
		got[n] = true
	}
	required := []string{
		"login.tpl", "dashboard.tpl", "error.tpl",
		"kv.tpl", "kv_view.tpl", "blobs.tpl", "watch.tpl", "query.tpl",
		"users.tpl", "user_detail.tpl", "account.tpl",
		"policies.tpl", "locks.tpl", "audit.tpl",
	}
	for _, name := range required {
		if !got[name] {
			t.Errorf("required page template %s missing", name)
		}
	}
}

// TestRenderEveryPageWithEmptyData executes each page with a Page whose
// Data is nil. All page templates guard their payload with {{with .Data}},
// so this must succeed — it catches runtime template errors (bad field
// names, broken pipelines) that parsing alone cannot.
func TestRenderEveryPageWithEmptyData(t *testing.T) {
	r := MustNew()
	for _, name := range r.Names() {
		rec := httptest.NewRecorder()
		p := &Page{Title: "t", User: "root", Admin: true, CSRF: "csrf-token", Path: "/"}
		if err := r.Render(rec, 200, name, p); err != nil {
			t.Errorf("Render(%s) = %v", name, err)
			continue
		}
		body := rec.Body.String()
		if !strings.Contains(body, "<!doctype html>") {
			t.Errorf("Render(%s): missing doctype", name)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Render(%s): Content-Type = %q", name, ct)
		}
	}
}

// TestRenderUnknownPage verifies the renderer fails loudly (500 + error)
// for a page name that does not exist, rather than writing nothing.
func TestRenderUnknownPage(t *testing.T) {
	r := MustNew()
	rec := httptest.NewRecorder()
	if err := r.Render(rec, 200, "nope.tpl", &Page{}); err == nil {
		t.Fatal("expected error for unknown page")
	}
	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestErrorBanner confirms the shared error banner path in base.tpl.
func TestErrorBanner(t *testing.T) {
	r := MustNew()
	rec := httptest.NewRecorder()
	err := r.Render(rec, 403, "error.tpl", &Page{User: "dev", Error: "permission denied on /secret"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "permission denied on /secret") {
		t.Fatal("error banner text missing from output")
	}
}

// TestHelpers exercises the exported formatting helpers the frontend uses.
func TestHelpers(t *testing.T) {
	if !Printable([]byte("hello\nworld\t!")) {
		t.Error("text should be printable")
	}
	if Printable([]byte{0x00, 0x01, 0xff}) {
		t.Error("binary should not be printable")
	}
	if Printable([]byte{0xc3, 0x28}) { // invalid UTF-8
		t.Error("invalid UTF-8 should not be printable")
	}
	if s := PrettyJSON([]byte(`{"a":1}`)); !strings.Contains(s, "\n  \"a\": 1") {
		t.Errorf("PrettyJSON = %q", s)
	}
	if s := PrettyJSON([]byte("not json")); s != "not json" {
		t.Errorf("PrettyJSON passthrough = %q", s)
	}
	if s := humanBytes(0); s != "0 B" {
		t.Errorf("humanBytes(0) = %q", s)
	}
	if s := humanBytes(1536); s != "1.5 KiB" {
		t.Errorf("humanBytes(1536) = %q", s)
	}
	if s := HexPreview([]byte("abc"), 2); !strings.Contains(s, "61 62") || strings.Contains(s, "63") {
		t.Errorf("HexPreview truncation broken: %q", s)
	}
}
