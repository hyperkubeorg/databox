package drive

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/shares"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// fixtureChrome is a signed-in shell for render tests.
func fixtureChrome() kernel.Chrome {
	return kernel.Chrome{
		Title: "Drive", SiteName: "Test Cloud", Theme: "dark",
		CurrentApp: "drive", AppName: "Drive",
		// A fully-enabled instance: Drive plus the media features, so the
		// "Open in Video/Music" links and media-registration controls
		// render (Draft 004 §7.5 gates them on Video/Music being enabled).
		DriveEnabled: true, VideoEnabled: true, MusicEnabled: true,
		User:    users.User{Username: "ada", DisplayName: "Ada Morgan", UsedBytes: 1 << 20},
		Session: &users.Session{Username: "ada", CSRF: "tok"},
	}
}

func fixtureSidebar() Sidebar {
	return Sidebar{
		Drives: []drives.Info{
			{Drive: drives.Drive{ID: "driveAAAAAAA", Name: "My Drive", Type: drives.Personal}, Role: "owner"},
			{Drive: drives.Drive{ID: "driveBBBBBBB", Name: "Team", Type: drives.Shared}, Role: "editor"},
		},
		ActiveDrive: "driveAAAAAAA",
	}
}

func render(t *testing.T, page string, data any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := ui.MustParse(tplFS).ExecuteTemplate(&buf, page, data); err != nil {
		t.Fatalf("render %s: %v", page, err)
	}
	return buf.String()
}

func wantAll(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !bytes.Contains([]byte(out), []byte(w)) {
			t.Errorf("output missing %q", w)
		}
	}
}

func TestBrowserRenders(t *testing.T) {
	folder := nodes.Node{ID: "folderXXXXXX", Name: "Photos", IsDir: true, ModifiedAt: time.Now()}
	file := nodes.Node{ID: "fileYYYYYYYY", Name: "cat.jpg", Size: 1234, ContentType: "image/jpeg", ModifiedAt: time.Now()}
	pg := BrowsePage{
		Chrome:  fixtureChrome(),
		Sidebar: fixtureSidebar(),
		Drive:   drives.Drive{ID: "driveAAAAAAA", Name: "My Drive", Type: drives.Personal},
		Role:    "owner", CanEdit: true, IsOwner: true,
		Folder:    nodes.Node{ID: nodes.RootID, IsDir: true},
		Crumbs:    []nodes.Crumb{{ID: nodes.RootID}},
		EventsURL: "/drive/events?drive=driveAAAAAAA&folder=root",
		Children: []NodeVM{
			nodeVM("driveAAAAAAA", folder, map[string][]string{"folderXXXXXX": {"music", "video"}}),
			nodeVM("driveAAAAAAA", file, nil),
		},
	}
	out := render(t, "browser", pg)
	wantAll(t, out,
		`id="browser"`, `data-drive="driveAAAAAAA"`, `data-events=`, // SSE wiring
		`data-open="/drive/d/driveAAAAAAA/folderXXXXXX"`,                        // folder opens in the browser
		`data-open="/drive/app/image?drive=driveAAAAAAA&amp;node=fileYYYYYYYY"`, // image opens its viewer
		`/drive/thumb/driveAAAAAAA/fileYYYYYYYY`,                                // image thumbnail
		`♪ music`, `▶ video`,                                                    // BOTH registered-folder badges
		`data-reg="music video"`,          // context-menu toggle source
		`data-media-kinds="video music"`,  // both media features enabled → both offered
		`id="btn-upload"`, `id="uploads"`, // upload chrome (editor)
		`id="view-toggle"`, `id="cmenu"`, // grid/rows + context menu shells
		`action="/drive/do/mkdir"`,       // no-JS fallback
		`Shared with me`, `1.0 MiB used`, // sidebar rail + meter
		`/drive/assets/browser.js`, // desktop UX layer
	)
	// A personal drive shows no "Drive settings" (members) link.
	if bytes.Contains([]byte(out), []byte("/drive/manage/driveAAAAAAA")) {
		t.Error("personal drive offers member settings")
	}
}

func TestNodeDetailsRenders(t *testing.T) {
	file := nodes.Node{ID: "fileYYYYYYYY", Name: "Notes.txt", Size: 42, ContentType: "text/plain", Version: 2, ModifiedAt: time.Now(), ModifiedBy: "ada"}
	pg := NodeDetailsPage{
		Chrome:  fixtureChrome(),
		Sidebar: fixtureSidebar(),
		Drive:   drives.Drive{ID: "driveAAAAAAA", Name: "My Drive"},
		VM:      nodeVM("driveAAAAAAA", file, nil),
		Crumbs:  []nodes.Crumb{{ID: nodes.RootID}, {ID: file.ID, Name: file.Name}},
		CanEdit: true, Owner: true, ParentID: nodes.RootID,
		Versions: []nodes.VersionRow{{Rev: "00000000000000000001-abc", Version: nodes.Version{N: 2, Size: 42, By: "ada", At: time.Now()}}},
		Shares:   []shares.Share{{Token: "tok123456789012345", Perms: "download"}},
		Grants:   []shares.NodeGrantRow{{Username: "bob", Grant: shares.Grant{Role: "viewer"}}},
	}
	out := render(t, "node", pg)
	wantAll(t, out,
		"Notes.txt", "Version history", "Restore",
		"/s/tok123456789012345", "@bob",
		`action="/drive/share/create"`, `action="/drive/share/grant"`,
		`action="/drive/do/delete"`, "Delete forever",
	)
}

func TestFolderDetailsOfferMediaRegistration(t *testing.T) {
	folder := nodes.Node{ID: "folderXXXXXX", Name: "Albums", IsDir: true, ModifiedAt: time.Now(), ModifiedBy: "ada"}
	pg := NodeDetailsPage{
		Chrome: fixtureChrome(), Sidebar: fixtureSidebar(),
		Drive:   drives.Drive{ID: "driveAAAAAAA", Name: "My Drive"},
		VM:      nodeVM("driveAAAAAAA", folder, nil),
		Crumbs:  []nodes.Crumb{{ID: nodes.RootID}, {ID: folder.ID, Name: folder.Name}},
		CanEdit: true, ParentID: nodes.RootID,
	}
	out := render(t, "node", pg)
	wantAll(t, out, "Use as Video content", "Use as Music content", `action="/drive/media/register"`)

	// One kind registered: that kind flips to "Stop using…", the OTHER
	// stays offered — the toggles are independent.
	pg.Registered = []string{"music"}
	out = render(t, "node", pg)
	wantAll(t, out, "Stop using as music content", "Use as Video content",
		`action="/drive/media/unregister"`, `action="/drive/media/register"`, "Rescan now")

	// Both kinds registered: both flip, both badges show.
	pg.Registered = []string{"music", "video"}
	out = render(t, "node", pg)
	wantAll(t, out, "Stop using as music content", "Stop using as video content",
		"Open in Music", "Open in Video", "♪ music", "▶ video")
	if bytes.Contains([]byte(out), []byte("Use as Video content")) {
		t.Error("dual-registered folder still offers registering video")
	}
}

func TestPublicShareRenders(t *testing.T) {
	// Password gate.
	pg := SharePage{Token: "tok123456789012345", SiteName: "Test Cloud", NeedPw: true}
	out := render(t, "share", pg)
	wantAll(t, out, "password-protected", `action="/s/tok123456789012345"`)

	// Folder browse with download.
	pg = SharePage{
		Token: "tok123456789012345", SiteName: "Test Cloud", CanDL: true,
		Node:   nodes.Node{ID: "folderXXXXXX", Name: "Pics", IsDir: true},
		Crumbs: []nodes.Crumb{{ID: "folderXXXXXX", Name: "Pics"}},
		Children: []ShareChildVM{{
			Node: nodes.Node{ID: "fileYYYYYYYY", Name: "cat.jpg", Size: 9, ModifiedAt: time.Now()},
			Kind: "img", OpenURL: "/s/tok123456789012345/f/fileYYYYYYYY",
		}},
	}
	out = render(t, "share", pg)
	wantAll(t, out, "cat.jpg", "/s/tok123456789012345/zip/folderXXXXXX", "/s/tok123456789012345/f/fileYYYYYYYY")
	// The public page never renders the app chrome.
	if bytes.Contains([]byte(out), []byte("appbar")) {
		t.Error("public share page leaks the app chrome")
	}
}

func TestDriveSettingsRenders(t *testing.T) {
	pg := DriveSettingsPage{
		Chrome: fixtureChrome(), Sidebar: fixtureSidebar(),
		Drive:   drives.Drive{ID: "driveBBBBBBB", Name: "Team", Type: drives.Shared, Owner: "ada", CreatedAt: time.Now()},
		Members: []drives.MemberRow{{Username: "ada", Member: drives.Member{Role: "owner", At: time.Now()}}, {Username: "bob", Member: drives.Member{Role: "editor", At: time.Now()}}},
		IsOwner: true, MyRole: "owner",
	}
	out := render(t, "drive_settings", pg)
	wantAll(t, out, "@ada", "@bob", "Add member", "Delete drive forever",
		`action="/drive/manage/driveBBBBBBB/member"`, `action="/drive/manage/driveBBBBBBB/delete"`)
}

func TestSharedAndSearchRender(t *testing.T) {
	shpg := SharedPage{Chrome: fixtureChrome(), Sidebar: fixtureSidebar()}
	wantAll(t, render(t, "shared", shpg), "Nothing shared yet")

	sepg := SearchPage{Chrome: fixtureChrome(), Sidebar: fixtureSidebar(), Query: "report"}
	wantAll(t, render(t, "search", sepg), "No matches")
	wantAll(t, render(t, "drive_new", DriveNewPage{Chrome: fixtureChrome(), Sidebar: fixtureSidebar()}), "New shared drive")
}

func TestFileKindBuckets(t *testing.T) {
	cases := []struct {
		name, ct string
		dir      bool
		want     string
	}{
		{"x", "", true, "dir"},
		{"cat.jpg", "image/jpeg", false, "img"},
		{"clip.mp4", "video/mp4", false, "vid"},
		{"song.mp3", "", false, "aud"},
		{"doc.pdf", "application/pdf", false, "pdf"},
		{"data.csv", "", false, "sheet"},
		{"backup.tar.gz", "", false, "zip"},
		{"README", "text/plain", false, "doc"},
	}
	for _, tc := range cases {
		if got := FileKind(tc.name, tc.ct, tc.dir); got != tc.want {
			t.Errorf("FileKind(%q, %q, %v) = %q, want %q", tc.name, tc.ct, tc.dir, got, tc.want)
		}
	}
}

// The openURLFor seam, phase-2c form: registered kinds open their app
// host page; everything else keeps downloading.
func TestOpenURLSeam(t *testing.T) {
	img := nodes.Node{ID: "fileYYYYYYYY"}
	if got := openURLFor("dr", img, "img"); got != "/drive/app/image?drive=dr&node=fileYYYYYYYY" {
		t.Errorf("img open = %q", got)
	}
	if got := openURLFor("dr", img, "doc"); got != "/drive/file/dr/fileYYYYYYYY" {
		t.Errorf("doc open = %q", got)
	}
}

// TestBrowserMediaKindsGated: with Video disabled, the browser passes only
// "music" as an offerable media kind, so the context menu can't offer
// registering a folder as Video content (the reported bug).
func TestBrowserMediaKindsGated(t *testing.T) {
	ch := fixtureChrome()
	ch.VideoEnabled = false // Video turned off in admin
	pg := BrowsePage{
		Chrome:  ch,
		Sidebar: fixtureSidebar(),
		Drive:   drives.Drive{ID: "driveAAAAAAA", Name: "My Drive", Type: drives.Personal},
		Role:    "owner", CanEdit: true, IsOwner: true,
		Folder:    nodes.Node{ID: nodes.RootID, IsDir: true},
		Crumbs:    []nodes.Crumb{{ID: nodes.RootID}},
		EventsURL: "/drive/events?drive=driveAAAAAAA&folder=root",
	}
	out := render(t, "browser", pg)
	if !strings.Contains(out, `data-media-kinds="music"`) {
		t.Errorf("with Video off, expected data-media-kinds=\"music\"; got:\n%.200s", out)
	}
	if strings.Contains(out, `data-media-kinds="video`) || strings.Contains(out, ` video music"`) {
		t.Error("disabled Video must not appear in data-media-kinds")
	}
}
