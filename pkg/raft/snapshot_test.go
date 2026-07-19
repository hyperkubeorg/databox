// snapshot_test.go pins the streamed-snapshot format: a state machine with
// thousands of keys, MVCC history, pending intents, and a garbage-collected
// horizon must survive the page-stream → staging → install pipeline with a
// byte-identical keyspace, and an install interrupted at any point must be
// resumable from the durable marker.
package raft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble/v2"

	"github.com/hyperkubeorg/databox/pkg/kv"
	"github.com/hyperkubeorg/databox/pkg/store"
)

const snapTestGID = 7

// buildBusySM fills a state machine with enough traffic to exercise every
// snapshot section: ~3000 writes over 400 keys (overwrites populate MVCC
// history), deletes (tombstone history), a pending 2PC intent, and enough
// entries to advance the GC horizon (cutoff > 0). Returns the last index.
func buildBusySM(t *testing.T, sm *kv.SM, st *store.Store) uint64 {
	t.Helper()
	// Shrink the MVCC knobs so history GC actually runs within the test's
	// entry budget; restore afterwards for other tests in the process.
	oldHist, oldEvery := kv.MVCCHistoryRevisions, kv.MVCCGCEvery
	kv.MVCCHistoryRevisions, kv.MVCCGCEvery = 512, 128
	t.Cleanup(func() { kv.MVCCHistoryRevisions, kv.MVCCGCEvery = oldHist, oldEvery })

	apply := func(index uint64, op kv.Op) kv.Result {
		raw := mustJSON(t, op)
		b := st.DB.NewBatch()
		res := sm.Apply(b, index, raw)
		if err := b.Commit(pebble.NoSync); err != nil {
			t.Fatal(err)
		}
		b.Close()
		r, ok := res.(kv.Result)
		if !ok {
			t.Fatalf("unexpected apply result %T", res)
		}
		return r
	}

	index := uint64(0)
	next := func() uint64 { index++; return index }
	// Overwrite a rotating key set → live records + superseded history.
	for i := 0; i < 3000; i++ {
		key := fmt.Sprintf("/bench/key-%03d", i%400)
		if r := apply(next(), kv.Op{Type: "set", Key: key, Value: []byte(fmt.Sprintf("v%d", i))}); r.Err != "" {
			t.Fatalf("set %d: %+v", i, r)
		}
	}
	// Deletes → tombstone history entries.
	for i := 0; i < 25; i++ {
		apply(next(), kv.Op{Type: "delete", Key: fmt.Sprintf("/bench/key-%03d", i)})
	}
	// A pending 2PC intent → non-empty intents section.
	if r := apply(next(), kv.Op{Type: "tx_prepare", TxID: "tx-snapshot-test",
		Writes: []kv.TxWrite{{Key: "/staged", Value: []byte("intent")}}}); r.Err != "" {
		t.Fatalf("tx_prepare: %+v", r)
	}
	if sm.Cutoff() == 0 {
		t.Fatal("test setup: MVCC GC never ran, cutoff still 0")
	}
	return index
}

func mustJSON(t *testing.T, op kv.Op) []byte {
	t.Helper()
	raw, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// dumpRange collects every key/value in [prefix, upper) as a flat map —
// the byte-identical comparison basis.
func dumpRange(t *testing.T, st *store.Store, prefix []byte) map[string]string {
	t.Helper()
	iter, err := st.DB.NewIter(&pebble.IterOptions{
		LowerBound: prefix, UpperBound: store.PrefixUpperBound(prefix),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer iter.Close()
	out := map[string]string{}
	for iter.First(); iter.Valid(); iter.Next() {
		out[string(iter.Key())] = string(iter.Value())
	}
	return out
}

// smPrefix covers the entire m/<gid>/ namespace: latest keys, intents,
// revision counter, MVCC history, and cutoff all live under it.
func smPrefix(gid uint64) []byte {
	p := store.SMPrefix(gid)
	return p[:len(p)-2] // strip the trailing "k/" → "m/<gid>/"
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// streamToStaging runs the sender half against a pinned view and the
// receiver half into st2's staging area, returning the marker it wrote —
// exactly what transport + receiveSnapshot do, minus HTTP.
func streamToStaging(t *testing.T, src *store.Store, sections []SnapshotSection, dst *store.Store, index uint64) installMarker {
	t.Helper()
	view := src.DB.NewSnapshot()
	defer view.Close()
	var buf bytes.Buffer
	if err := writeSnapshotPages(&buf, view, sections); err != nil {
		t.Fatalf("writeSnapshotPages: %v", err)
	}
	if err := clearStaging(dst, snapTestGID); err != nil {
		t.Fatal(err)
	}
	counts, err := stageSnapshotPages(dst, snapTestGID, len(sections), &buf)
	if err != nil {
		t.Fatalf("stageSnapshotPages: %v", err)
	}
	m := installMarker{
		State: markerComplete, GID: snapTestGID, Index: index, Term: 3,
		Sections: sectionNames(sections), Counts: counts,
	}
	if err := writeMarker(dst, snapTestGID, m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestStreamedSnapshotRoundTrip: page stream → staging → install must
// reproduce the source state machine byte for byte, including MVCC history
// and the GC cutoff, and must fully replace divergent pre-existing state.
func TestStreamedSnapshotRoundTrip(t *testing.T) {
	src := openTestStore(t)
	sm, err := kv.NewSM(snapTestGID, src, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	index := buildBusySM(t, sm, src)

	// The receiver starts with divergent state that the install must wipe.
	dst := openTestStore(t)
	sm2, err := kv.NewSM(snapTestGID, dst, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	b := dst.DB.NewBatch()
	sm2.Apply(b, 1, mustJSON(t, kv.Op{Type: "set", Key: "/stale", Value: []byte("old")}))
	if err := b.Commit(pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	b.Close()

	sections := sm.SnapshotSections()
	m := streamToStaging(t, src, sections, dst, index)

	// Install: complete → installing → finish, as the group loop does.
	m.State = markerInstalling
	if err := writeMarker(dst, snapTestGID, m); err != nil {
		t.Fatal(err)
	}
	meta, manifest, err := finishSnapshotInstall(dst, snapTestGID, sm2.SnapshotSections())
	if err != nil {
		t.Fatalf("finishSnapshotInstall: %v", err)
	}
	if meta.Index != index || meta.Term != 3 {
		t.Fatalf("installed meta = %+v, want index %d term 3", meta, index)
	}
	if !IsStreamedSnapshot(manifest) {
		t.Fatal("install did not persist a v2 manifest")
	}
	if err := sm2.RefreshAfterRestore(); err != nil {
		t.Fatal(err)
	}

	// Byte-identical state machine keyspace (latest, intents, history,
	// revision, cutoff — the whole m/<gid>/ namespace).
	want := dumpRange(t, src, smPrefix(snapTestGID))
	got := dumpRange(t, dst, smPrefix(snapTestGID))
	if len(want) != len(got) {
		t.Fatalf("restored keyspace has %d keys, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("key %q: restored %q, want %q", k, got[k], v)
		}
	}
	if _, stale := got[string(store.SMKey(snapTestGID, "/stale"))]; stale {
		t.Fatal("divergent pre-existing key survived the install")
	}
	// In-memory caches reloaded.
	if sm2.Rev() != sm.Rev() || sm2.Cutoff() != sm.Cutoff() {
		t.Fatalf("caches: rev %d/%d cutoff %d/%d", sm2.Rev(), sm.Rev(), sm2.Cutoff(), sm.Cutoff())
	}
	// Raft-side artifacts: applied index, no marker, empty staging.
	if applied, _ := dst.GetU64(store.RaftAppliedKey(snapTestGID)); applied != index {
		t.Fatalf("applied = %d, want %d", applied, index)
	}
	if _, ok, _ := readMarker(dst, snapTestGID); ok {
		t.Fatal("marker survived the install")
	}
	if staged := dumpRange(t, dst, store.RaftSnapStagingPrefix(snapTestGID)); len(staged) != 0 {
		t.Fatalf("%d staged keys survived the install", len(staged))
	}
}

// TestSnapshotInstallCrashResume: a crash after the "installing" marker —
// with the live keyspace already wiped — must be recoverable purely from
// staging + marker, producing the same final state.
func TestSnapshotInstallCrashResume(t *testing.T) {
	src := openTestStore(t)
	sm, err := kv.NewSM(snapTestGID, src, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	index := buildBusySM(t, sm, src)
	sections := sm.SnapshotSections()

	dst := openTestStore(t)
	m := streamToStaging(t, src, sections, dst, index)
	m.State = markerInstalling
	if err := writeMarker(dst, snapTestGID, m); err != nil {
		t.Fatal(err)
	}
	// Simulate the crash point: live sections wiped, copy not yet done.
	wipe := dst.DB.NewBatch()
	for _, sec := range sections {
		if err := wipe.DeleteRange(sec.Prefix, store.PrefixUpperBound(sec.Prefix), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := wipe.Commit(pebble.Sync); err != nil {
		t.Fatal(err)
	}
	wipe.Close()

	// Restart path: StartGroup calls RecoverPendingInstall first.
	resumed, err := RecoverPendingInstall(dst, snapTestGID, sections)
	if err != nil {
		t.Fatalf("RecoverPendingInstall: %v", err)
	}
	if !resumed {
		t.Fatal("recovery did not resume the interrupted install")
	}
	want := dumpRange(t, src, smPrefix(snapTestGID))
	got := dumpRange(t, dst, smPrefix(snapTestGID))
	if len(want) != len(got) {
		t.Fatalf("recovered keyspace has %d keys, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("key %q: recovered %q, want %q", k, got[k], v)
		}
	}
	if _, ok, _ := readMarker(dst, snapTestGID); ok {
		t.Fatal("marker survived recovery")
	}
}

// TestStageRejectsTruncatedStream: a stream cut mid-transfer must fail
// staging (trailer counts never verify), leaving no complete marker.
func TestStageRejectsTruncatedStream(t *testing.T) {
	src := openTestStore(t)
	sm, err := kv.NewSM(snapTestGID, src, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	buildBusySM(t, sm, src)
	sections := sm.SnapshotSections()
	view := src.DB.NewSnapshot()
	defer view.Close()
	var buf bytes.Buffer
	if err := writeSnapshotPages(&buf, view, sections); err != nil {
		t.Fatal(err)
	}
	dst := openTestStore(t)
	cut := buf.Bytes()[:buf.Len()/2]
	if _, err := stageSnapshotPages(dst, snapTestGID, len(sections), bytes.NewReader(cut)); err == nil {
		t.Fatal("truncated stream staged without error")
	}
}
