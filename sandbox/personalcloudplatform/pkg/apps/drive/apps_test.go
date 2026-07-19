package drive

import (
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// TestFileKindEditorBuckets covers the 2c additions: the app-backed
// editor kinds.
func TestFileKindEditorBuckets(t *testing.T) {
	cases := []struct{ name, want string }{
		{"Budget.sheet", "gsheet"},
		{"Budget.pcgrid", "gsheet"},
		{"Notes.pcdoc", "gdoc"},
		{"Sketch.pcdraw", "gdraw"},
		{"Sprint.pckan", "gkan"},
		{"README.md", "gmd"},
		{"notes.markdown", "gmd"},
		{"data.csv", "sheet"},
	}
	for _, tc := range cases {
		if got := FileKind(tc.name, "", false); got != tc.want {
			t.Errorf("FileKind(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestAppRegistryOpenURLs: every registered kind opens through the app
// host; unregistered kinds keep downloading.
func TestAppRegistryOpenURLs(t *testing.T) {
	n := nodes.Node{ID: "fileYYYYYYYY"}
	cases := map[string]string{
		"img":    "/drive/app/image?drive=dr&node=fileYYYYYYYY",
		"vid":    "/drive/app/video?drive=dr&node=fileYYYYYYYY",
		"aud":    "/drive/app/music?drive=dr&node=fileYYYYYYYY",
		"pdf":    "/drive/app/pdf?drive=dr&node=fileYYYYYYYY",
		"sheet":  "/drive/app/sheet?drive=dr&node=fileYYYYYYYY",
		"gsheet": "/drive/app/grid?drive=dr&node=fileYYYYYYYY",
		"gdoc":   "/drive/app/writer?drive=dr&node=fileYYYYYYYY",
		"gdraw":  "/drive/app/draw?drive=dr&node=fileYYYYYYYY",
		"gkan":   "/drive/app/kanban?drive=dr&node=fileYYYYYYYY",
		"gmd":    "/drive/app/md?drive=dr&node=fileYYYYYYYY",
		"doc":    "/drive/file/dr/fileYYYYYYYY",
		"zip":    "/drive/file/dr/fileYYYYYYYY",
	}
	for kind, want := range cases {
		if got := openURLFor("dr", n, kind); got != want {
			t.Errorf("openURLFor(%q) = %q, want %q", kind, got, want)
		}
	}
	// Every registry app is known and serveable; editors are a subset.
	for kind, app := range appRegistry {
		if !knownApps[app] {
			t.Errorf("registry kind %q names unknown app %q", kind, app)
		}
	}
	for app := range editableApps {
		if !knownApps[app] {
			t.Errorf("editable app %q not known", app)
		}
	}
}

// TestCollabRoutesMounted pins the 2c HTTP surface: host, doc state/ops/
// close per type, presence, exports, and the creation endpoints.
func TestCollabRoutesMounted(t *testing.T) {
	k := &kernel.App{}
	patterns := map[string]bool{}
	for _, rt := range Mount(k).Routes {
		patterns[rt.Pattern] = true
	}
	for _, want := range []string{
		"GET /drive/app/{app}",
		"GET /drive/doc/{drive}/{node}/state",
		"POST /drive/doc/{drive}/{node}/ops",
		"POST /drive/doc/{drive}/{node}/presence",
		"POST /drive/doc/{drive}/{node}/close",
		"GET /drive/grid/{drive}/{node}/state",
		"POST /drive/grid/{drive}/{node}/ops",
		"POST /drive/grid/{drive}/{node}/close",
		"GET /drive/wdoc/{drive}/{node}/state",
		"POST /drive/wdoc/{drive}/{node}/ops",
		"POST /drive/wdoc/{drive}/{node}/close",
		"GET /drive/kanban/{drive}/{node}/state",
		"POST /drive/kanban/{drive}/{node}/ops",
		"POST /drive/kanban/{drive}/{node}/close",
		"GET /drive/draw/{drive}/{node}/state",
		"POST /drive/draw/{drive}/{node}/ops",
		"POST /drive/draw/{drive}/{node}/close",
		"POST /drive/draw/{drive}/{node}/export",
		"GET /drive/md/{drive}/{node}/state",
		"POST /drive/md/{drive}/{node}/ops",
		"POST /drive/md/{drive}/{node}/close",
		"GET /drive/export/{drive}/{node}/csv",
		"POST /drive/export/{drive}/{node}/csv",
		"GET /drive/export/{drive}/{node}/xlsx",
		"POST /drive/export/{drive}/{node}/xlsx",
		"GET /drive/export/{drive}/{node}/html",
		"POST /drive/export/{drive}/{node}/html",
		"GET /drive/export/{drive}/{node}/txt",
		"POST /drive/export/{drive}/{node}/txt",
		"POST /drive/do/newsheet",
		"POST /drive/do/newdoc",
		"POST /drive/do/newdraw",
		"POST /drive/do/newkanban",
		"POST /drive/do/newmd",
		"POST /drive/do/importcsv",
	} {
		if !patterns[want] {
			t.Errorf("route missing: %s", want)
		}
	}
}

func TestAppHostRenders(t *testing.T) {
	file := nodes.Node{ID: "fileYYYYYYYY", Name: "Budget.sheet", Size: 512,
		ContentType: collab.GridContentType, ModifiedAt: time.Now()}
	pg := AppHostPage{
		Chrome:  fixtureChrome(),
		AppID:   "grid",
		Node:    file,
		DriveID: "driveAAAAAAA",
		FileURL: "/drive/file/driveAAAAAAA/fileYYYYYYYY?inline=1",
		CanEdit: true, Editable: true,
		BackURL: "/drive/d/driveAAAAAAA/root",
		Playlist: []NodeVM{
			{Node: nodes.Node{ID: "sibZZZZZZZZZ", Name: "Other.sheet"}, DriveID: "driveAAAAAAA"},
		},
	}
	out := render(t, "app_host", pg)
	wantAll(t, out,
		`window.PCP_APP`,
		`/drive/assets/apps/grid.js`,                   // the module import
		`href="/drive/d/driveAAAAAAA/root"`,            // back to the folder
		`data-rename="1"`,                              // click-to-rename (editor)
		`href="/drive/file/driveAAAAAAA/fileYYYYYYYY"`, // download
		`href="/drive/n/driveAAAAAAA/fileYYYYYYYY"`,    // details
		`"/drive/doc/driveAAAAAAA/fileYYYYYYYY/ops"`,   // collab endpoints
		// & escapes as \u0026 in the JS string context — same URL.
		`/drive/events?doc=1\u0026drive=driveAAAAAAA\u0026node=fileYYYYYYYY`,
		`/drive/assets/apps.css`, // the Slate editor chrome
		`sibZZZZZZZZZ`,           // playlist sibling
	)
	if strings.Contains(out, "revbanner") {
		t.Error("no revision → no banner")
	}

	// Read-only revision preview: banner, restore form, no rename.
	pg.Rev, pg.RevN, pg.RevBy, pg.RevAt = "00000000000000000001-abc", 3, "ada", time.Now()
	pg.CanRestore, pg.CanEdit = true, false
	out = render(t, "app_host", pg)
	wantAll(t, out,
		"revbanner", "Read-only preview", "Restore this version",
		`action="/drive/do/restorever"`,
	)
	if strings.Contains(out, `data-rename="1"`) {
		t.Error("rev preview must not offer rename")
	}

	// A viewer (non-editable app) gets doc: null.
	pg = AppHostPage{Chrome: fixtureChrome(), AppID: "image",
		Node: nodes.Node{ID: "imgAAAAAAAAA", Name: "cat.jpg"}, DriveID: "driveAAAAAAA",
		FileURL: "/drive/file/driveAAAAAAA/imgAAAAAAAAA?inline=1", BackURL: "/drive/d/driveAAAAAAA/root"}
	out = render(t, "app_host", pg)
	wantAll(t, out, "doc: null", "/drive/assets/apps/image.js")
}

// TestWDocExportHTMLShape pins the standalone export: page CSS, blocks
// in order, header/footer — and no script anywhere the renderer writes
// (the CSP header is the second belt; the handler test can't run
// without a live store, so the shape check lives on the pure renderer).
func TestWDocExportHTMLShape(t *testing.T) {
	doc := collab.WDocDoc{
		Format: collab.WDocFormat,
		Blocks: []collab.WDocBlock{
			{ID: "blk1aaaa", HTML: "<h1>Title</h1>"},
			{ID: "blk2bbbb", HTML: "<p>Body &amp; soul</p>"},
		},
		Page:   collab.WDocPage{Size: "a4", Orient: "landscape"},
		Header: "<em>hdr</em>",
		Footer: "<em>ftr</em>",
	}
	out := string(wdocExportHTML("My <Doc>", doc))
	wantAll(t, out,
		"<!doctype html>",
		"<title>My &lt;Doc&gt;</title>", // escaped title
		"@page{size:A4 landscape",
		"<header><em>hdr</em></header>",
		"<h1>Title</h1>",
		"<p>Body &amp; soul</p>",
		"<footer><em>ftr</em></footer>",
	)
	if strings.Index(out, "<h1>Title</h1>") > strings.Index(out, "<p>Body") {
		t.Error("blocks out of order")
	}
}
