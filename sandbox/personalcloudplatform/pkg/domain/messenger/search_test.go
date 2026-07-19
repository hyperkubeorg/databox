package messenger

import (
	"context"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// ParseQuery separates free terms from the operators.
func TestParseQuery(t *testing.T) {
	q := ParseQuery("hello world from:@Bob has:file after:2026-01-01")
	if len(q.Terms) != 2 || q.Terms[0] != "hello" {
		t.Fatalf("terms = %v", q.Terms)
	}
	if q.From != "bob" || !q.HasFile || q.After.IsZero() {
		t.Fatalf("operators wrong: %+v", q)
	}
}

// The inverted index finds a just-posted message by term, scoped to a
// channel, and honors the from: filter.
func TestSearchIndex(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	srv, cid, ada, bob := mkServerWithMembers(t, s, us)

	if _, err := s.SendToChannel(ctx, srv.ID, cid, ada, "the quick brown fox", SendOpts{}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := s.SendToChannel(ctx, srv.ID, cid, bob, "slow green turtle", SendOpts{}); err != nil {
		t.Fatalf("send: %v", err)
	}

	scope := SearchScope{Kind: ScopeChannel, ServerID: srv.ID, CID: cid}
	hits, err := s.Search(ctx, ada, scope, "brown", 20)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].Author != "ada" {
		t.Fatalf("term search = %+v", hits)
	}
	// from: filter narrows to bob's message.
	hits, _ = s.Search(ctx, ada, SearchScope{Kind: ScopeServer, ServerID: srv.ID}, "from:bob turtle", 20)
	if len(hits) != 1 || hits[0].Author != "bob" {
		t.Fatalf("from search = %+v", hits)
	}
	// A term nobody wrote returns nothing.
	if hits, _ := s.Search(ctx, ada, scope, "elephant", 20); len(hits) != 0 {
		t.Fatalf("phantom hits = %+v", hits)
	}
	// Search only consults conversations the viewer may see: a non-member
	// gets nothing from the server scope.
	eve := users.User{Username: "eve"}
	if hits, _ := s.Search(ctx, eve, SearchScope{Kind: ScopeServer, ServerID: srv.ID}, "brown", 20); len(hits) != 0 {
		t.Fatalf("non-member search leaked: %+v", hits)
	}
}
