package collab

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
)

func calEvent(id, title string, start, end time.Time) Event {
	return Event{ID: id, Title: title, Start: start, End: end}
}

func TestValidEvent(t *testing.T) {
	s := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	e := time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC)
	if err := ValidEvent(calEvent("ev01", "Standup", s, e)); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
	ok := calEvent("ev01", "Standup", s, e)
	ok.Invites = map[string]string{"bob": RSVPInvited, "x@remote.example": RSVPInvited}
	ok.Tags = []string{"carol"}
	if err := ValidEvent(ok); err != nil {
		t.Fatalf("event with internal+external invites rejected: %v", err)
	}
	bad := []Event{
		calEvent("x", "short id", s, e),
		calEvent("ev01", "", s, e),
		calEvent("ev01", "backwards", e, s),
		func() Event { v := ok; v.Invites = map[string]string{"bob": "sure"}; return v }(),
		func() Event { v := ok; v.Invites = map[string]string{"has space@x.example": RSVPInvited}; return v }(),
		func() Event { v := ok; v.Invites = map[string]string{"no-dot@example": RSVPInvited}; return v }(),
		func() Event { v := ok; v.Tags = []string{"BAD USER"}; return v }(),
	}
	for i, b := range bad {
		if ValidEvent(b) == nil {
			t.Errorf("bad event %d accepted: %+v", i, b)
		}
	}
}

// FoldCalOp: highest HLC wins, tombstones delete, foreign shapes drop.
func TestCalFold(t *testing.T) {
	s := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	doc := NewCalDoc("")
	op := func(e Event, hlc string) TargetOp {
		raw, _ := json.Marshal(e)
		return TargetOp{T: "e:" + e.ID, V: raw, HLC: hlc}
	}
	first := calEvent("ev01", "First", s, s.Add(time.Hour))
	second := calEvent("ev01", "Second", s, s.Add(2*time.Hour))
	FoldCalOp(&doc, op(first, hlcAt(1000, 0, "ada")))
	FoldCalOp(&doc, op(second, hlcAt(2000, 0, "ada")))
	if doc.Events["ev01"].Title != "Second" {
		t.Fatalf("fold kept %q", doc.Events["ev01"].Title)
	}
	// A structurally invalid value must not clobber the good one.
	FoldCalOp(&doc, TargetOp{T: "e:ev01", V: json.RawMessage(`{"id":"ev01"}`), HLC: hlcAt(3000, 0, "ada")})
	if doc.Events["ev01"].Title != "Second" {
		t.Fatal("invalid event value applied")
	}
	// Tombstone.
	FoldCalOp(&doc, TargetOp{T: "e:ev01", V: json.RawMessage("null"), HLC: hlcAt(4000, 0, "ada")})
	if _, exists := doc.Events["ev01"]; exists {
		t.Fatal("tombstone did not delete")
	}
	// Meta color.
	FoldCalOp(&doc, TargetOp{T: "meta", V: json.RawMessage(`{"color":"#3ECF8E"}`), HLC: hlcAt(5000, 0, "ada")})
	if doc.Color != "#3ECF8E" {
		t.Fatalf("color = %q", doc.Color)
	}
	FoldCalOp(&doc, TargetOp{T: "meta", V: json.RawMessage(`{"color":"url(x)"}`), HLC: hlcAt(6000, 0, "ada")})
	if doc.Color != "#3ECF8E" {
		t.Fatal("invalid color applied")
	}
}

func TestEventsBetween(t *testing.T) {
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	doc := NewCalDoc("")
	doc.Events["ev01"] = calEvent("ev01", "in", base.Add(10*time.Hour), base.Add(11*time.Hour))
	doc.Events["ev02"] = calEvent("ev02", "before", base.Add(-2*time.Hour), base.Add(-time.Hour))
	doc.Events["ev03"] = calEvent("ev03", "spans", base.Add(-time.Hour), base.Add(time.Hour))
	doc.Events["ev04"] = calEvent("ev04", "after", base.Add(25*time.Hour), base.Add(26*time.Hour))
	got := EventsBetween(doc, base, base.Add(24*time.Hour))
	if len(got) != 2 || got[0].ID != "ev03" || got[1].ID != "ev01" {
		t.Fatalf("range query = %+v", got)
	}
}

// AppendCalOp on the fake store: validation gates + load/fold + RSVP.
func TestCalAppendLoadRSVP(t *testing.T) {
	db := kvxtest.New(t)
	store := &Store{DB: db, Nodes: &nodes.Store{DB: db}}
	ctx := context.Background()
	drive, node := "drv000000001", "nod000000001"
	fileNode := nodes.Node{ID: node, Name: "Team.pccal"}

	s := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	e := calEvent("ev01", "Standup", s, s.Add(time.Hour))
	e.By = "ada"
	e.Invites = map[string]string{"bob": RSVPInvited}
	raw, _ := json.Marshal(e)

	// Bad clock, bad target, id mismatch all refuse.
	if _, _, err := store.AppendCalOp(ctx, drive, node, TargetOp{T: "e:ev01", V: raw, HLC: "junk"}, "ada"); err == nil {
		t.Fatal("bad clock accepted")
	}
	if _, _, err := store.AppendCalOp(ctx, drive, node, TargetOp{T: "cell:0:0", V: raw, HLC: hlcAt(1000, 0, "ada")}, "ada"); err == nil {
		t.Fatal("foreign target accepted")
	}
	if _, _, err := store.AppendCalOp(ctx, drive, node, TargetOp{T: "e:evXX", V: raw, HLC: hlcAt(1000, 0, "ada")}, "ada"); err == nil {
		t.Fatal("id mismatch accepted")
	}
	ev, isEvent, err := store.AppendCalOp(ctx, drive, node, TargetOp{T: "e:ev01", V: raw, HLC: hlcAt(1000, 0, "ada")}, "ada")
	if err != nil || !isEvent || ev.Title != "Standup" {
		t.Fatalf("append: %v %v %+v", err, isEvent, ev)
	}

	doc, err := store.LoadCalDoc(ctx, drive, node, fileNode)
	if err != nil || len(doc.Events) != 1 {
		t.Fatalf("load: %v %+v", err, doc)
	}

	// RSVP: the invite is the entire authorization.
	got, err := store.ApplyRSVP(ctx, drive, node, fileNode, "ev01", "bob", RSVPYes)
	if err != nil || got.Invites["bob"] != RSVPYes {
		t.Fatalf("invitee rsvp: %v %+v", err, got)
	}
	if _, err := store.ApplyRSVP(ctx, drive, node, fileNode, "ev01", "mallory", RSVPYes); err == nil {
		t.Fatal("stranger rsvp accepted") // must 404 upstream
	}
	if _, err := store.ApplyRSVP(ctx, drive, node, fileNode, "ev01", "bob", "sure"); err == nil {
		t.Fatal("bad status accepted")
	}
	// The answer folded in.
	doc, _ = store.LoadCalDoc(ctx, drive, node, fileNode)
	if doc.Events["ev01"].Invites["bob"] != RSVPYes {
		t.Fatalf("rsvp not folded: %+v", doc.Events["ev01"])
	}
}

func TestCalParseRejectsForeign(t *testing.T) {
	if _, err := ParseCalDoc([]byte(`{"format":"pcp-md/1"}`)); err == nil {
		t.Fatal("foreign format accepted")
	}
	if _, err := ParseCalDoc([]byte(`{"format":"pcp-cal/1","events":{}}`)); err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}
}

func TestIsCalFile(t *testing.T) {
	if !IsCalFile(nodes.Node{Name: "Team.PCCAL"}) {
		t.Error("case-insensitive extension failed")
	}
	if IsCalFile(nodes.Node{Name: "Team.pccal", IsDir: true}) {
		t.Error("directory accepted")
	}
	if IsCalFile(nodes.Node{Name: "notes.md"}) {
		t.Error("markdown accepted")
	}
}
