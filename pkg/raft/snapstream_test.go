// snapstream_test.go exercises the HTTP snapshot endpoint end to end: a
// composed header+pages body POSTed at SnapshotHandler must land in the
// staging area with a "complete" marker and hand the MsgSnap to Deliver —
// and the handler must enforce its concurrency/staleness policies.
package raft

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.etcd.io/raft/v3/raftpb"

	"github.com/hyperkubeorg/databox/pkg/kv"
	"github.com/hyperkubeorg/databox/pkg/store"
)

// buildSnapshotBody composes a full snapshot request body (header + page
// stream) from a source store, exactly as Transport.sendSnapshot streams it.
func buildSnapshotBody(t *testing.T, src *store.Store, sections []SnapshotSection, msg raftpb.Message) []byte {
	t.Helper()
	var body bytes.Buffer
	hdr, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var lenb [4]byte
	binary.BigEndian.PutUint32(lenb[:], uint32(len(hdr)))
	body.Write(lenb[:])
	body.Write(hdr)
	view := src.DB.NewSnapshot()
	defer view.Close()
	if err := writeSnapshotPages(&body, view, sections); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestSnapshotHandlerHTTP(t *testing.T) {
	src := openTestStore(t)
	sm, err := kv.NewSM(snapTestGID, src, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	index := buildBusySM(t, sm, src)
	sections := sm.SnapshotSections()

	manifest, err := encodeManifest(Manifest{
		GID: snapTestGID, Index: index, Term: 3, Sections: sectionNames(sections),
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := raftpb.Message{
		Type: raftpb.MsgSnap, To: 2, From: 1,
		Snapshot: &raftpb.Snapshot{
			Data: manifest,
			Metadata: raftpb.SnapshotMetadata{
				Index: index, Term: 3,
				ConfState: raftpb.ConfState{Voters: []uint64{1, 2}},
			},
		},
	}
	body := buildSnapshotBody(t, src, sections, msg)

	// Receiver: a transport with a store and a Deliver capture.
	dst := openTestStore(t)
	tr := NewTransport(2, nil, "", testLogger())
	tr.SetStore(dst)
	delivered := make(chan raftpb.Message, 1)
	tr.Deliver = func(gid uint64, m raftpb.Message) {
		if gid == snapTestGID {
			delivered <- m
		}
	}
	srv := httptest.NewServer(tr.SnapshotHandler())
	defer srv.Close()

	resp, err := http.Post(fmt.Sprintf("%s/?gid=%d", srv.URL, snapTestGID),
		"application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("handler returned %s", resp.Status)
	}

	// The MsgSnap reached Deliver intact.
	select {
	case m := <-delivered:
		if m.Snapshot.Metadata.Index != index {
			t.Fatalf("delivered snapshot index %d, want %d", m.Snapshot.Metadata.Index, index)
		}
	default:
		t.Fatal("MsgSnap never delivered")
	}
	// Staging is durable and the marker is complete at the right index.
	m, ok, err := readMarker(dst, snapTestGID)
	if err != nil || !ok {
		t.Fatalf("marker missing after receive (err=%v)", err)
	}
	if m.State != markerComplete || m.Index != index {
		t.Fatalf("marker = %+v, want complete@%d", m, index)
	}
	if staged := dumpRange(t, dst, store.RaftSnapStagingPrefix(snapTestGID)); len(staged) == 0 {
		t.Fatal("nothing staged")
	}

	// Policy: a stale (older-index) transfer is refused while a newer one
	// is staged.
	oldManifest, _ := encodeManifest(Manifest{
		GID: snapTestGID, Index: index - 1, Term: 3, Sections: sectionNames(sections),
	})
	oldMsg := msg
	oldMsg.Snapshot = &raftpb.Snapshot{
		Data: oldManifest,
		Metadata: raftpb.SnapshotMetadata{
			Index: index - 1, Term: 3,
			ConfState: raftpb.ConfState{Voters: []uint64{1, 2}},
		},
	}
	oldBody := buildSnapshotBody(t, src, sections, oldMsg)
	resp2, err := http.Post(fmt.Sprintf("%s/?gid=%d", srv.URL, snapTestGID),
		"application/octet-stream", bytes.NewReader(oldBody))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusOK {
		t.Fatal("handler accepted a snapshot older than the staged one")
	}
}
