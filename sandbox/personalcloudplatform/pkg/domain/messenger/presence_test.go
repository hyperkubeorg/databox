package messenger

import (
	"context"
	"testing"
)

// EffectiveStatus hides Invisible/Offline and requires a connection.
func TestEffectiveStatus(t *testing.T) {
	cases := []struct {
		chosen              string
		connected, selfView bool
		want                string
	}{
		{StatusOnline, true, false, StatusOnline},
		{StatusOnline, false, false, StatusOffline}, // not connected → offline
		{StatusAway, true, false, StatusAway},
		{StatusDND, true, false, StatusDND},
		{StatusInvisible, true, false, StatusOffline},  // hidden from others
		{StatusInvisible, true, true, StatusInvisible}, // but the user sees it
		{StatusOffline, true, false, StatusOffline},
		{"", true, false, StatusOnline}, // default
		{"", false, true, StatusOnline}, // self default
	}
	for _, c := range cases {
		if got := EffectiveStatus(c.chosen, c.connected, c.selfView); got != c.want {
			t.Errorf("EffectiveStatus(%q, conn=%v, self=%v) = %q, want %q", c.chosen, c.connected, c.selfView, got, c.want)
		}
	}
}

// StatusRank pulls online to the top.
func TestStatusRank(t *testing.T) {
	if !(StatusRank(StatusOnline) < StatusRank(StatusAway) &&
		StatusRank(StatusAway) < StatusRank(StatusOffline)) {
		t.Fatal("status rank ordering wrong")
	}
}

// Setting and reading a chosen status round-trips; the default is Online.
func TestSetGetStatus(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	ada := mkUser(t, us, "ada")

	if p, _ := s.GetPresence(ctx, ada.Username); p.Chosen != StatusOnline {
		t.Fatalf("default status = %q, want online", p.Chosen)
	}
	if err := s.SetStatus(ctx, ada.Username, StatusDND, "heads down"); err != nil {
		t.Fatalf("set: %v", err)
	}
	p, _ := s.GetPresence(ctx, ada.Username)
	if p.Chosen != StatusDND || p.StatusMsg != "heads down" {
		t.Fatalf("status = %+v", p)
	}
	// A bad status coerces to Online.
	_ = s.SetStatus(ctx, ada.Username, "bogus", "")
	if p, _ := s.GetPresence(ctx, ada.Username); p.Chosen != StatusOnline {
		t.Fatalf("bad status not coerced: %q", p.Chosen)
	}
}
