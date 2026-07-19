// metaproxy_test.go proves the peer-address-book builder honors the
// discovery contract: members first, then live nodes, then the rest;
// removed nodes, self, blanks, and duplicates never enter; the book is
// hard-capped. The book is what lets a node survive complete metadata
// member turnover (the ship-of-theseus property), so its ordering and
// exclusions are behavior, not style.
package server

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/hyperkubeorg/databox/pkg/cluster"
)

func TestBuildPeerBookOrderingAndExclusions(t *testing.T) {
	members := []metaMemberInfo{
		{ID: 3, Addr: "m3:7000"},
		{ID: 5, Addr: ""}, // member with unknown addr: skipped, not fatal
		{ID: 7, Addr: "m7:7000"},
	}
	nodes := []cluster.Node{
		{ID: 1, Addr: "self:7000", State: "active", Live: true},   // self — excluded
		{ID: 2, Addr: "dead:7000", State: "active", Live: false},  // not live — after live ones
		{ID: 3, Addr: "m3:7000", State: "active", Live: true},     // duplicate of a member
		{ID: 4, Addr: "n4:7000", State: "active", Live: true},     // live non-member
		{ID: 6, Addr: "gone:7000", State: "removed", Live: false}, // removed — excluded
		{ID: 8, Addr: "n8:7000", State: "draining", Live: true},   // draining still answers
	}
	got := buildPeerBook(members, nodes, 1)
	want := []string{"m3:7000", "m7:7000", "n4:7000", "n8:7000", "dead:7000"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildPeerBook = %v, want %v", got, want)
	}
}

func TestBuildPeerBookCap(t *testing.T) {
	var nodes []cluster.Node
	for i := 0; i < peerBookCap*2; i++ {
		nodes = append(nodes, cluster.Node{
			ID: uint64(i + 10), Addr: fmt.Sprintf("n%d:7000", i), State: "active", Live: true,
		})
	}
	got := buildPeerBook([]metaMemberInfo{{ID: 2, Addr: "m:7000"}}, nodes, 1)
	if len(got) != peerBookCap {
		t.Fatalf("book length = %d, want cap %d", len(got), peerBookCap)
	}
	if got[0] != "m:7000" {
		t.Fatalf("member not first: %v", got[0])
	}
}
