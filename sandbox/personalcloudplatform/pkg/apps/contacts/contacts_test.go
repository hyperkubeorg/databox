package contacts

import (
	"bytes"
	"strings"
	"testing"

	dcontacts "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/contacts"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

func TestContactsPageRenders(t *testing.T) {
	views := ui.MustParse(tplFS)
	pg := Page{
		Chrome: kernel.Chrome{
			Title: "Contacts", SiteName: "Test Cloud", Theme: "dark",
			CurrentApp: "contacts", AppName: "Contacts",
			User:    users.User{Username: "ada", DisplayName: "Ada Morgan", PersonalDrive: "drvp"},
			Session: &users.Session{Username: "ada", CSRF: "tok"},
		},
		Cards: []dcontacts.Entry{{
			DriveID: "drvp", NodeID: "nod1", DriveName: "My Drive", CanEdit: true,
			Card: dcontacts.Card{Name: "Grace <Hopper>", Emails: []string{"grace@remote.example"}},
		}},
		Drives: []DriveVM{{ID: "drvp", Name: "My Drive"}},
	}
	var buf bytes.Buffer
	if err := views.ExecuteTemplate(&buf, "contacts", pg); err != nil {
		t.Fatalf("render contacts: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`data-card="drvp/nod1"`,         // the SSR row
		"Grace &lt;Hopper&gt;",          // escaped name
		"grace@remote.example",          // subtitle
		`id="ct-search"`, `id="ct-new"`, // rail
		`value="drvp" selected`,        // the personal drive preselected
		"/contacts/assets/contacts.js", // live model
	} {
		if !strings.Contains(out, want) {
			t.Errorf("contacts page missing %q", want)
		}
	}

	// The empty state renders without cards.
	pg.Cards = nil
	buf.Reset()
	if err := views.ExecuteTemplate(&buf, "contacts", pg); err != nil {
		t.Fatalf("render empty contacts: %v", err)
	}
	if !strings.Contains(buf.String(), "No contacts yet") {
		t.Error("empty state missing")
	}
}

func TestSplitMulti(t *testing.T) {
	got := splitMulti("a@x.example, b@y.example\nc@z.example; ;")
	if len(got) != 3 || got[0] != "a@x.example" || got[2] != "c@z.example" {
		t.Fatalf("splitMulti = %v", got)
	}
}
