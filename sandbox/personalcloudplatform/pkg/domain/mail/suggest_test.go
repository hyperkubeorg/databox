package mail

import (
	"context"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Contacts rank ABOVE recents in the compose typeahead, and a
// contact's "Name <addr>" form dedupes against the same bare recent.
func TestSuggestRecipientsContactsFirst(t *testing.T) {
	db := kvxtest.New(t)
	s := &Store{DB: db, Users: &users.Store{DB: db}}
	ctx := context.Background()

	_ = s.RecordRecent(ctx, "ada", "grace@remote.example")
	_ = s.RecordRecent(ctx, "ada", "other@remote.example")

	// No contacts hook: recents only.
	hits := s.SuggestRecipients(ctx, "ada", "remote", 8)
	if len(hits) != 2 {
		t.Fatalf("recents = %v", hits)
	}

	s.Contacts = func(_ context.Context, username, q string, limit int) []string {
		if username != "ada" {
			t.Errorf("contacts hook got user %q", username)
		}
		return []string{"Grace Hopper <grace@remote.example>", "Vendor Desk <desk@vendor.example>"}
	}
	hits = s.SuggestRecipients(ctx, "ada", "remote", 8)
	if len(hits) < 3 || hits[0] != "Grace Hopper <grace@remote.example>" || hits[1] != "Vendor Desk <desk@vendor.example>" {
		t.Fatalf("contacts must rank first: %v", hits)
	}
	// grace's bare recent deduped away; other@ survives.
	for _, h := range hits {
		if h == "grace@remote.example" {
			t.Fatalf("bare duplicate survived: %v", hits)
		}
	}
	found := false
	for _, h := range hits {
		if h == "other@remote.example" {
			found = true
		}
	}
	if !found {
		t.Fatalf("recent lost: %v", hits)
	}
}
