// blob_test.go proves the engine's durability mechanics: write quorum
// (placement shortfalls fail the upload, nothing commits), EC shard
// reconstruction after loss, at-rest scrub detection of corrupt chunks,
// policy resolution (most-specific path wins over defaults), and the
// repair rate limiter's pacing.
package blob

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"testing"
	"time"
)

// TestWriteQuorumReplicaFailure: a replica-mode write must fail with a
// retryable QuorumError when it cannot place min(replicas, nodes)
// distinct-node copies — never succeed with fewer copies than promised.
func TestWriteQuorumReplicaFailure(t *testing.T) {
	e := testEngine(t, 3)
	p := e.Peers.(*memPeers)
	p.failPush[2] = true
	p.failPush[3] = true // only the local node can store → 1 of 2 copies

	_, err := e.Write("/q", bytes.NewReader([]byte("small blob")), "")
	if err == nil {
		t.Fatal("expected quorum failure, write succeeded")
	}
	var qe *QuorumError
	if !errors.As(err, &qe) {
		t.Fatalf("expected QuorumError, got %T: %v", err, err)
	}
	if qe.Got != 1 || qe.Want != 2 {
		t.Fatalf("QuorumError got/want = %d/%d, expected 1/2", qe.Got, qe.Want)
	}
	// The error must carry the retryable code the API layer maps on.
	if got := err.Error(); len(got) < len("InsufficientReplicas") || got[:len("InsufficientReplicas")] != "InsufficientReplicas" {
		t.Fatalf("error does not carry InsufficientReplicas code: %q", got)
	}

	// One healthy peer is enough for the 2-copy quorum.
	p.failPush[2] = false
	if _, err := e.Write("/q", bytes.NewReader([]byte("small blob")), ""); err != nil {
		t.Fatalf("write with reachable quorum failed: %v", err)
	}
}

// TestWriteQuorumECDistinct: with enough nodes for all-distinct shard
// placement, a node refusing writes must fail the upload rather than let
// shards silently co-locate; with a small cluster the same failure is
// tolerated by spreading over the remaining nodes.
func TestWriteQuorumECDistinct(t *testing.T) {
	data := make([]byte, 1024*5) // 5 chunks → EC mode, 6 shards per stripe
	rand.Read(data)

	// 6 nodes: rs-4-2 demands 6 distinct holders; one refusing node makes
	// that impossible, so the write must fail (retryable).
	e := testEngine(t, 6)
	e.Peers.(*memPeers).failPush[4] = true
	_, err := e.Write("/ec", bytes.NewReader(data), "")
	var qe *QuorumError
	if !errors.As(err, &qe) {
		t.Fatalf("expected QuorumError with a refusing node in a 6-node cluster, got %v", err)
	}

	// 3 nodes: fewer nodes than shards, so distinctness is best-effort —
	// a refusing node just shifts shards onto the survivors.
	e = testEngine(t, 3)
	e.Peers.(*memPeers).failPush[2] = true
	m, err := e.Write("/ec", bytes.NewReader(data), "")
	if err != nil {
		t.Fatalf("small-cluster write should tolerate one refusing node: %v", err)
	}
	roundTrip(t, e, m, data)
	for _, st := range m.Stripes {
		for _, ref := range st.Shards {
			if len(ref.Nodes) != 1 || ref.Nodes[0] == 2 {
				t.Fatalf("shard recorded on refusing node: %+v", ref)
			}
		}
	}
}

// TestECShardSpread: healthy clusters must place every shard of a stripe
// on a distinct node.
func TestECShardSpread(t *testing.T) {
	e := testEngine(t, 6)
	data := make([]byte, 1024*4)
	rand.Read(data)
	m, err := e.Write("/spread", bytes.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	for si, st := range m.Stripes {
		seen := map[uint64]bool{}
		for _, ref := range st.Shards {
			if seen[ref.Nodes[0]] {
				t.Fatalf("stripe %d co-locates shards on node %d", si, ref.Nodes[0])
			}
			seen[ref.Nodes[0]] = true
		}
	}
}

// TestECReconstruction: a shard lost from every holder is rebuilt
// bit-exact from the stripe's survivors.
func TestECReconstruction(t *testing.T) {
	e := testEngine(t, 6)
	p := e.Peers.(*memPeers)
	data := make([]byte, 1024*4) // exactly one full stripe
	rand.Read(data)
	m, err := e.Write("/rebuild", bytes.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "ec" || len(m.Stripes) != 1 {
		t.Fatalf("expected 1 EC stripe, got %+v", m)
	}
	// Destroy shard 2 everywhere it lives.
	lost := m.Stripes[0].Shards[2]
	for _, n := range lost.Nodes {
		if n == p.Self() {
			if err := e.Store.Delete(lost.Hash); err != nil {
				t.Fatal(err)
			}
		}
		delete(p.disks[n], lost.Hash)
	}

	rebuilt, err := e.ReconstructStripeShards(m, 0, []int{2})
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	if len(rebuilt) != 1 || rebuilt[2] == nil {
		t.Fatalf("expected shard 2 back, got %v keys", len(rebuilt))
	}
	// Re-place it and prove the blob reads end-to-end again.
	if err := e.Store.Put(lost.Hash, rebuilt[2]); err != nil {
		t.Fatalf("re-place reconstructed shard: %v", err)
	}
	roundTrip(t, e, m, data)
}

// TestECReconstructionTooManyLost: losing more shards than parity covers
// must fail loudly, not fabricate data.
func TestECReconstructionTooManyLost(t *testing.T) {
	e := testEngine(t, 6)
	p := e.Peers.(*memPeers)
	data := make([]byte, 1024*4)
	rand.Read(data)
	m, err := e.Write("/gone", bytes.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	var missing []int
	for i := 0; i < 3; i++ { // rs-4-2 tolerates 2; kill 3
		ref := m.Stripes[0].Shards[i]
		for _, n := range ref.Nodes {
			if n == p.Self() {
				_ = e.Store.Delete(ref.Hash)
			}
			delete(p.disks[n], ref.Hash)
		}
		missing = append(missing, i)
	}
	if _, err := e.ReconstructStripeShards(m, 0, missing); err == nil {
		t.Fatal("expected reconstruction to fail with 3 of 6 shards lost")
	}
}

// TestScrubDetectsCorruption: an on-disk bit flip is caught by the
// incremental scrub, the corrupt copy is deleted, and the cursor wraps.
func TestScrubDetectsCorruption(t *testing.T) {
	e := testEngine(t, 1)
	m, err := e.Write("/scrub", bytes.NewReader([]byte("scrub me, I dare you")), "")
	if err != nil {
		t.Fatal(err)
	}
	hash := m.Chunks[0].Hash
	// Flip bytes under the chunk's trusted name.
	if err := os.WriteFile(e.Store.path(hash), []byte("rotten bits"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := e.ScrubNext(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Corrupt) != 1 || res.Corrupt[0] != hash {
		t.Fatalf("scrub missed the corrupt chunk: %+v", res)
	}
	if !res.Wrapped {
		t.Fatalf("scrub of a one-chunk disk should complete a full pass: %+v", res)
	}
	if e.Store.Has(hash) {
		t.Fatal("corrupt chunk still on disk after scrub")
	}
	// A clean disk scrubs clean.
	res, err = e.ScrubNext(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Corrupt) != 0 {
		t.Fatalf("second scrub found phantom corruption: %+v", res)
	}
}

// TestScrubIncremental: the cursor advances across bounded batches and
// wraps after covering everything exactly once.
func TestScrubIncremental(t *testing.T) {
	e := testEngine(t, 1)
	for i := 0; i < 5; i++ {
		buf := make([]byte, 100)
		rand.Read(buf)
		if _, err := e.Write("/inc", bytes.NewReader(buf), ""); err != nil {
			t.Fatal(err)
		}
	}
	total, batches := 0, 0
	for {
		res, err := e.ScrubNext(2)
		if err != nil {
			t.Fatal(err)
		}
		total += res.Scanned
		batches++
		if res.Wrapped {
			break
		}
		if batches > 10 {
			t.Fatal("scrub cursor never wrapped")
		}
	}
	if total != 5 {
		t.Fatalf("scrub covered %d chunks across batches, want 5", total)
	}
	if batches < 3 {
		t.Fatalf("scrub was not incremental: %d batches for 5 chunks with batch size 2", batches)
	}
}

// TestPolicyResolution: most-specific path wins, segment-boundary
// matching, and fallback to the built-in defaults.
func TestPolicyResolution(t *testing.T) {
	def := DefaultPolicy(2)
	repl := map[string][]byte{
		"/":         []byte(`{"replicas":3}`),
		"/logs":     []byte(`{"replicas":2}`),
		"/logs/hot": []byte(`{"replicas":5}`),
	}
	ec := map[string][]byte{
		"/media": []byte(`{"data":8,"parity":3,"enabled":true}`),
		"/logs":  []byte(`{"enabled":false}`),
	}
	cases := []struct {
		key  string
		want Policy
	}{
		// Root rule overrides the default replica count; EC defaults hold.
		{"/misc/a", Policy{Replicas: 3, DataShards: 4, ParityShards: 2, ECEnabled: true}},
		// /logs beats /: replicas 2 and EC disabled for the subtree.
		{"/logs/app.log", Policy{Replicas: 2, DataShards: 4, ParityShards: 2, ECEnabled: false}},
		// /logs/hot beats /logs for replicas; EC rule for /logs still governs.
		{"/logs/hot/now", Policy{Replicas: 5, DataShards: 4, ParityShards: 2, ECEnabled: false}},
		// Segment boundary: /logstash is NOT governed by /logs.
		{"/logstash/x", Policy{Replicas: 3, DataShards: 4, ParityShards: 2, ECEnabled: true}},
		// Custom EC geometry.
		{"/media/movie", Policy{Replicas: 3, DataShards: 8, ParityShards: 3, ECEnabled: true}},
	}
	for _, c := range cases {
		if got := ResolvePolicy(c.key, repl, ec, def); got != c.want {
			t.Errorf("ResolvePolicy(%q) = %+v, want %+v", c.key, got, c.want)
		}
	}
	// No rules at all → untouched defaults.
	if got := ResolvePolicy("/x", nil, nil, def); got != def {
		t.Errorf("empty rules changed the default policy: %+v", got)
	}
	// Malformed rules are ignored, never fatal.
	bad := map[string][]byte{"/": []byte(`{"replicas":0}`)}
	if got := ResolvePolicy("/x", bad, nil, def); got != def {
		t.Errorf("invalid rule leaked into resolution: %+v", got)
	}
}

// TestPolicyRuleValidation: stored rule shapes are validated on write.
func TestPolicyRuleValidation(t *testing.T) {
	if _, err := ParseReplicationRule([]byte(`{"replicas":3}`)); err != nil {
		t.Errorf("valid replication rule rejected: %v", err)
	}
	for _, raw := range []string{`{"replicas":0}`, `{"replicas":99}`, `not json`} {
		if _, err := ParseReplicationRule([]byte(raw)); err == nil {
			t.Errorf("bad replication rule %q accepted", raw)
		}
	}
	if _, err := ParseECRule([]byte(`{"data":4,"parity":2,"enabled":true}`)); err != nil {
		t.Errorf("valid ec rule rejected: %v", err)
	}
	if _, err := ParseECRule([]byte(`{"enabled":false}`)); err != nil {
		t.Errorf("ec-off rule rejected: %v", err)
	}
	for _, raw := range []string{`{"data":0,"parity":2,"enabled":true}`, `{"data":4,"parity":0,"enabled":true}`, `{"data":30,"parity":30,"enabled":true}`} {
		if _, err := ParseECRule([]byte(raw)); err == nil {
			t.Errorf("bad ec rule %q accepted", raw)
		}
	}
}

// TestPolicyDrivesWriteMode: an EC-off policy forces replica mode for a
// blob that would otherwise erasure-code, and a replication rule sets the
// copy count.
func TestPolicyDrivesWriteMode(t *testing.T) {
	e := testEngine(t, 6)
	e.Policies = policySourceFunc(func(key string, def Policy) Policy {
		def.ECEnabled = false
		def.Replicas = 3
		return def
	})
	data := make([]byte, 1024*5)
	rand.Read(data)
	m, err := e.Write("/forced", bytes.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "replica" {
		t.Fatalf("EC-off policy ignored: mode=%s", m.Mode)
	}
	for i, ref := range m.Chunks {
		if len(ref.Nodes) != 3 {
			t.Fatalf("chunk %d has %d copies, policy wants 3", i, len(ref.Nodes))
		}
	}
	roundTrip(t, e, m, data)
}

// policySourceFunc adapts a plain function to PolicySource for tests.
type policySourceFunc func(key string, def Policy) Policy

func (f policySourceFunc) PolicyFor(key string, def Policy) Policy { return f(key, def) }

// TestRateLimiter: nil is unlimited; overdrafting the bucket delays the
// next call by the deterministic repayment time.
func TestRateLimiter(t *testing.T) {
	var nilLimiter *RateLimiter
	nilLimiter.Wait(1 << 40) // must not panic or block

	l := NewRateLimiter(1 << 30) // 1 GiB/s, 1 GiB burst
	start := time.Now()
	l.Wait(1 << 30) // drains the full burst instantly
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("burst-sized wait blocked for %v", d)
	}
	start = time.Now()
	l.Wait(1 << 28) // 256 MiB overdraft → ~250ms repayment
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("overdraft repaid too fast: %v", d)
	}
}
