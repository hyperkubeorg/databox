// reconcile_meta_test.go covers the metadata voter rule (1/3/5 voters,
// NO other membership — non-members mirror) and the leadership balancer —
// the placement behaviors with quorum consequences.
package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/kv"
)

func testController(f *fakeFabric) *Controller {
	return NewController(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func seedJSON(t *testing.T, f *fakeFabric, key string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.MetaPropose(context.Background(), kv.Op{Type: "set", Key: key, Value: raw}); err != nil {
		t.Fatal(err)
	}
}

func seedActiveNodes(t *testing.T, f *fakeFabric, ids ...uint64) {
	t.Helper()
	for _, id := range ids {
		seedJSON(t, f, KeyNodes+nodeKey(id), Node{
			ID: id, Name: fmt.Sprintf("n%d", id), Addr: fmt.Sprintf("n%d:8443", id),
			State: "active", Live: true, LastSeen: time.Now().UTC(),
		})
	}
}

func readGroup(t *testing.T, f *fakeFabric, gid uint64) GroupInfo {
	t.Helper()
	rec, ok, err := f.MetaGet(KeyGroups + fmt.Sprintf("%d", gid))
	if err != nil || !ok {
		t.Fatalf("group %d record missing (err=%v)", gid, err)
	}
	var g GroupInfo
	if err := json.Unmarshal(rec.Value, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func sortedU64(in []uint64) []uint64 {
	out := append([]uint64(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The voter target follows the 1/3/5 rule: 1 below 3 nodes, 3 from 3–7
// nodes, 5 only once the fleet reaches 8.
func TestMetaVoterTargetRule(t *testing.T) {
	want := map[int]int{0: 0, 1: 1, 2: 1, 3: 3, 4: 3, 5: 3, 6: 3, 7: 3, 8: 5, 9: 5, 100: 5, 5000: 5}
	for n, w := range want {
		if got := MetaVoterTarget(n); got != w {
			t.Fatalf("MetaVoterTarget(%d) = %d, want %d", n, got, w)
		}
	}
}

// A 5-node fleet seats exactly 3 voters (lowest ordinals) — never 5, and
// no other kind of membership exists.
func TestMetaPlacementFiveNodesThreeVoters(t *testing.T) {
	f := newFakeFabric()
	c := testController(f)
	seedActiveNodes(t, f, 1, 2, 3, 4, 5)
	seedJSON(t, f, KeyGroups+"1", GroupInfo{GID: 1, Members: []uint64{1}, Kind: "meta"})
	for i := 0; i < 10; i++ {
		if err := c.reconcilePlacement(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	g := readGroup(t, f, 1)
	if !equalU64(sortedU64(g.Members), []uint64{1, 2, 3}) {
		t.Fatalf("voters = %v, want [1 2 3] (3 voters at 5 nodes)", sortedU64(g.Members))
	}
	for _, cc := range f.confChanges {
		if strings.HasPrefix(cc, "learner@") {
			t.Fatalf("no learner conf changes may exist: %v", f.confChanges)
		}
	}
}

// Reaching 8 nodes finally unlocks the 5-voter target.
func TestMetaPlacementEightNodesFiveVoters(t *testing.T) {
	f := newFakeFabric()
	c := testController(f)
	seedActiveNodes(t, f, 1, 2, 3, 4, 5, 6, 7, 8)
	seedJSON(t, f, KeyGroups+"1", GroupInfo{GID: 1, Members: []uint64{1, 2, 3}, Kind: "meta"})
	for i := 0; i < 10; i++ {
		if err := c.reconcilePlacement(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	g := readGroup(t, f, 1)
	if !equalU64(sortedU64(g.Members), []uint64{1, 2, 3, 4, 5}) {
		t.Fatalf("voters = %v, want [1 2 3 4 5] at 8 nodes", sortedU64(g.Members))
	}
}

// Over-target membership (fleet shrank, or upgrade from the wider eras)
// is conf-changed OUT — dead voters first, highest ordinal next, never
// the leader (this controller's node).
func TestMetaOverTargetUnseatsDeadFirstNeverLeader(t *testing.T) {
	f := newFakeFabric()
	c := testController(f)
	seedActiveNodes(t, f, 1, 2, 3, 4, 5, 7)
	// Node 6 is a dead voter (verdict dead).
	seedJSON(t, f, KeyNodes+nodeKey(6), Node{ID: 6, State: "active", Live: false, LastSeen: time.Now().UTC().Add(-time.Minute)})
	// 7 voters, but only 6 eligible nodes → target 3.
	seedJSON(t, f, KeyGroups+"1", GroupInfo{GID: 1, Members: []uint64{1, 2, 3, 4, 5, 6, 7}, Kind: "meta"})
	if err := c.reconcilePlacement(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := readGroup(t, f, 1)
	for _, m := range g.Members {
		if m == 6 {
			t.Fatalf("dead voter 6 should be unseated first; members = %v", g.Members)
		}
	}
	for i := 0; i < 6; i++ {
		if err := c.reconcilePlacement(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	g = readGroup(t, f, 1)
	if !equalU64(sortedU64(g.Members), []uint64{1, 2, 3}) {
		t.Fatalf("voters = %v, want [1 2 3] after shrink to target", sortedU64(g.Members))
	}
	joined := strings.Join(f.confChanges, " ")
	if strings.Contains(joined, "remove@1:1") {
		t.Fatalf("the metadata leader (node 1) must never unseat itself: %v", f.confChanges)
	}
}

// A freed seat (decommission) is refilled with the lowest-ordinal
// eligible non-member — which mirrors until seated.
func TestMetaSeatRefillAfterDrain(t *testing.T) {
	f := newFakeFabric()
	c := testController(f)
	seedActiveNodes(t, f, 1, 2, 4, 5)
	seedJSON(t, f, KeyGroups+"1", GroupInfo{GID: 1, Members: []uint64{1, 2}, Kind: "meta"})
	if err := c.reconcilePlacement(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := readGroup(t, f, 1)
	if !equalU64(sortedU64(g.Members), []uint64{1, 2, 4}) {
		t.Fatalf("voters = %v, want [1 2 4] (lowest eligible ordinal seated)", sortedU64(g.Members))
	}
}

func seedStats(t *testing.T, f *fakeFabric, gid, leader uint64) {
	t.Helper()
	seedJSON(t, f, KeyStats+fmt.Sprintf("%d", gid), GroupStats{
		GID: gid, Leader: leader, Reported: time.Now().UTC(),
	})
}

// The balancer moves one leadership from the busiest node to a
// least-loaded fellow voter, prefers data groups over the metadata group,
// and then goes quiet for the cooldown window.
func TestLeadershipRebalanceMovesFromBusiest(t *testing.T) {
	f := newFakeFabric()
	c := testController(f)
	seedActiveNodes(t, f, 1, 2, 3)
	seedJSON(t, f, KeyGroups+"1", GroupInfo{GID: 1, Members: []uint64{1, 2, 3}, Kind: "meta"})
	seedJSON(t, f, KeyGroups+"2", GroupInfo{GID: 2, Members: []uint64{1, 2, 3}, Kind: "data"})
	seedJSON(t, f, KeyGroups+"3", GroupInfo{GID: 3, Members: []uint64{1, 2, 3}, Kind: "data"})
	seedStats(t, f, 2, 1)
	seedStats(t, f, 3, 1) // node 1 leads meta (LocalNodeID) + both data groups
	if err := c.reconcileLeadership(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.transfers) != 1 {
		t.Fatalf("transfers = %v, want exactly one", f.transfers)
	}
	if strings.HasPrefix(f.transfers[0], "1:") {
		t.Fatalf("transfer %s moved the metadata group; data groups must be preferred", f.transfers[0])
	}
	if !strings.Contains(f.transfers[0], ":1->") {
		t.Fatalf("transfer %s should move a leadership off node 1", f.transfers[0])
	}
	// Cooldown: an immediate second pass must not transfer again.
	if err := c.reconcileLeadership(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.transfers) != 1 {
		t.Fatalf("cooldown violated: transfers = %v", f.transfers)
	}
}

// The balancer stays silent on: small skew (hysteresis), admin pause, a
// pending decommission, and any group with an unhealthy member.
func TestLeadershipRebalanceStaysQuiet(t *testing.T) {
	base := func() (*fakeFabric, *Controller) {
		f := newFakeFabric()
		c := testController(f)
		seedActiveNodes(t, f, 1, 2)
		seedJSON(t, f, KeyGroups+"1", GroupInfo{GID: 1, Members: []uint64{1, 2}, Kind: "meta"})
		seedJSON(t, f, KeyGroups+"2", GroupInfo{GID: 2, Members: []uint64{1, 2}, Kind: "data"})
		return f, c
	}

	// Skew of 1 (meta on node 1, data on node 2) is noise, not imbalance.
	f, c := base()
	seedStats(t, f, 2, 2)
	if err := c.reconcileLeadership(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.transfers) != 0 {
		t.Fatalf("hysteresis violated: %v", f.transfers)
	}

	// Paused rebalance suspends balancing entirely.
	f, c = base()
	seedStats(t, f, 2, 1)
	raw, _ := json.Marshal(PauseFlag{Paused: true, Actor: "test"})
	if _, err := f.MetaPropose(context.Background(), kv.Op{Type: "set", Key: KeyAdminPause + "rebalance", Value: raw}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileLeadership(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.transfers) != 0 {
		t.Fatalf("paused rebalance must not transfer: %v", f.transfers)
	}

	// A pending decommission owns leadership placement.
	f, c = base()
	seedStats(t, f, 2, 1)
	seedJSON(t, f, KeyDecomm+nodeKey(2), uint64(2))
	if err := c.reconcileLeadership(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.transfers) != 0 {
		t.Fatalf("decommission in progress must suspend balancing: %v", f.transfers)
	}

	// An unhealthy group member means repair comes first.
	f, c = base()
	seedStats(t, f, 2, 1)
	seedJSON(t, f, KeyNodes+nodeKey(2), Node{ID: 2, State: "active", Live: false, LastSeen: time.Now().UTC().Add(-time.Minute)})
	if err := c.reconcileLeadership(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.transfers) != 0 {
		t.Fatalf("degraded group must suspend balancing: %v", f.transfers)
	}
}

// A node holding well over its fair share of leaderships raises a
// warning-tier alert; balanced fleets raise none.
func TestLeaderSkewAlert(t *testing.T) {
	f := newFakeFabric()
	c := testController(f)
	seedActiveNodes(t, f, 1, 2)
	seedJSON(t, f, KeyGroups+"1", GroupInfo{GID: 1, Members: []uint64{1, 2}, Kind: "meta"})
	for gid := uint64(2); gid <= 4; gid++ {
		seedJSON(t, f, KeyGroups+fmt.Sprintf("%d", gid), GroupInfo{GID: gid, Members: []uint64{1, 2}, Kind: "data"})
		seedStats(t, f, gid, 1) // node 1 leads everything
	}
	if err := c.reconcileAlerts(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := f.MetaGet(KeyAlerts + "node-1-leadership")
	if !ok {
		t.Fatal("expected node-1-leadership skew alert")
	}
	var a Alert
	if json.Unmarshal(rec.Value, &a) != nil || a.Severity != "warning" {
		t.Fatalf("skew alert severity = %q, want warning", a.Severity)
	}
	// Rebalance the leads evenly: the alert must clear.
	seedStats(t, f, 2, 2)
	seedStats(t, f, 3, 2)
	if err := c.reconcileAlerts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.MetaGet(KeyAlerts + "node-1-leadership"); ok {
		t.Fatal("skew alert should clear once leaderships even out")
	}
}
