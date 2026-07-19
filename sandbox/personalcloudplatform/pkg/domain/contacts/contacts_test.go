package contacts

import (
	"context"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/pkg/client"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

func newWorld(t *testing.T) (context.Context, *users.Store, *drives.Store, *Store) {
	t.Helper()
	db := kvxtest.New(t)
	us := &users.Store{DB: db}
	us.OnSignup = func(tx *client.Tx, u *users.User) {
		id := kvx.NewID()
		drives.StagePersonalDrive(tx, id, u.Username)
		u.PersonalDrive = id
	}
	ds := &drives.Store{DB: db, Users: us}
	ns := &nodes.Store{DB: db, Users: us}
	return context.Background(), us, ds, &Store{DB: db, Nodes: ns, Drives: ds}
}

func TestSanitize(t *testing.T) {
	c, err := Sanitize(Card{
		Name:   "  Grace Hopper  ",
		Emails: []string{" Grace@Remote.Example ", ""},
		Phones: []string{" +1 555 0100 ", " "},
		Org:    " Navy ",
	})
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if c.Name != "Grace Hopper" || c.Emails[0] != "grace@remote.example" || len(c.Emails) != 1 ||
		c.Phones[0] != "+1 555 0100" || len(c.Phones) != 1 || c.Org != "Navy" || c.Format != CardFormat {
		t.Fatalf("sanitized = %+v", c)
	}
	for _, bad := range []Card{
		{Name: ""},
		{Name: "X", Emails: []string{"no-at"}},
		{Name: "X", Emails: []string{"a b@x.example"}},
		{Name: "X", Emails: []string{"a@nodot"}},
	} {
		if _, err := Sanitize(bad); err == nil {
			t.Errorf("bad card accepted: %+v", bad)
		}
	}
}

// Custom fields: types whitelist to text, blank labels default to the
// type, empty values drop, over-long values REFUSE (never truncate a
// secret), and the count caps at maxFields.
func TestSanitizeFields(t *testing.T) {
	c, err := Sanitize(Card{Name: "X", Fields: []Field{
		{Label: " Door code ", Type: "secret", Value: " 4242 "},
		{Label: "", Type: "note", Value: "line one\nline two"},
		{Label: "Site", Type: "hacked", Value: "https://x.example"},
		{Label: "Empty", Type: "text", Value: "   "},
	}})
	if err != nil {
		t.Fatalf("sanitize fields: %v", err)
	}
	if len(c.Fields) != 3 {
		t.Fatalf("fields = %+v", c.Fields)
	}
	if c.Fields[0].Label != "Door code" || c.Fields[0].Value != "4242" ||
		c.Fields[1].Label != "note" || c.Fields[1].Value != "line one\nline two" ||
		c.Fields[2].Type != "text" {
		t.Fatalf("fields = %+v", c.Fields)
	}
	if _, err := Sanitize(Card{Name: "X", Fields: []Field{
		{Label: "big", Type: "text", Value: strings.Repeat("a", maxFieldValue+1)},
	}}); err == nil {
		t.Error("over-long text field accepted")
	}
	if _, err := Sanitize(Card{Name: "X", Fields: []Field{
		{Label: "big", Type: "secret", Value: strings.Repeat("a", maxFieldSecret+1)},
	}}); err == nil {
		t.Error("over-long secret accepted")
	}
	many := Card{Name: "X"}
	for i := 0; i < maxFields+10; i++ {
		many.Fields = append(many.Fields, Field{Label: "f", Type: "text", Value: "v"})
	}
	if c, err := Sanitize(many); err != nil || len(c.Fields) != maxFields {
		t.Fatalf("field cap: err=%v n=%d", err, len(c.Fields))
	}
}

// Cards aggregate across personal AND shared drives; personal cards
// land in the lazily-created Contacts folder.
func TestCreateAggregateMatch(t *testing.T) {
	ctx, us, ds, cs := newWorld(t)
	ada, err := us.CreateUser(ctx, "ada", "Ada", "hunter22pass")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}

	// Personal card → Contacts folder created lazily.
	driveID, node, err := cs.CreateCard(ctx, ada, "", Card{Name: "Grace Hopper", Emails: []string{"grace@remote.example"}}, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if driveID != ada.PersonalDrive {
		t.Fatalf("card landed in %s", driveID)
	}
	folder, found, err := cs.Nodes.GetChild(ctx, driveID, nodes.RootID, FolderName)
	if err != nil || !found || !folder.IsDir {
		t.Fatalf("Contacts folder missing: %v %v", found, err)
	}
	// A second card reuses it.
	if _, _, err := cs.CreateCard(ctx, ada, "", Card{Name: "Annie Easley", Emails: []string{"annie@remote.example"}}, 0); err != nil {
		t.Fatalf("second create: %v", err)
	}
	kids, _ := cs.Nodes.ListFolder(ctx, driveID, folder.ID)
	if len(kids) != 2 {
		t.Fatalf("Contacts folder children = %d", len(kids))
	}

	// A shared drive's card shows up too, marked by the drive.
	shared, _ := ds.CreateShared(ctx, "ada", "Ops")
	if _, _, err := cs.CreateCard(ctx, ada, shared.ID, Card{Name: "Vendor Desk", Emails: []string{"desk@vendor.example"}}, 0); err != nil {
		t.Fatalf("shared create: %v", err)
	}

	entries, err := cs.Aggregate(ctx, "ada")
	if err != nil || len(entries) != 3 {
		t.Fatalf("aggregate = %d entries, err %v", len(entries), err)
	}

	// Typeahead: name and address matching, "Name <email>" form.
	hits := cs.Match(ctx, "ada", "grace", 8)
	if len(hits) != 1 || hits[0] != "Grace Hopper <grace@remote.example>" {
		t.Fatalf("match by name = %v", hits)
	}
	if hits := cs.Match(ctx, "ada", "vendor.example", 8); len(hits) != 1 || hits[0] != "Vendor Desk <desk@vendor.example>" {
		t.Fatalf("match by address = %v", hits)
	}
	if hits := cs.Match(ctx, "ada", "zzz", 8); len(hits) != 0 {
		t.Fatalf("no-match = %v", hits)
	}

	// Save a new version; the aggregation reads the fresh blob.
	if _, err := cs.SaveCard(ctx, driveID, node.ID, "ada", Card{Name: "Grace B. Hopper", Emails: []string{"grace@remote.example"}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	entries, _ = cs.Aggregate(ctx, "ada")
	found = false
	for _, en := range entries {
		if en.Card.Name == "Grace B. Hopper" {
			found = true
		}
	}
	if !found {
		t.Fatalf("edited card not visible: %+v", entries)
	}
}

func TestSafeCardFileName(t *testing.T) {
	if got := safeCardFileName(`a/b\c:d*e?f"g<h>i|j`); got != "a-b-c-d-e-f-g-h-i-j" {
		t.Fatalf("filename = %q", got)
	}
	if got := safeCardFileName("  .. "); got != "Contact" {
		t.Fatalf("degenerate filename = %q", got)
	}
}
