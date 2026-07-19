package mail

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSweepOrphanBlobs covers the two-phase GC: a delivered message
// whose msgref survives is never touched; strand the blob + searchtext
// (a crashed purge / deleted mailbox) and the first sweep only STAGES
// the candidate, the second — past grace — reclaims both rows. Draft
// attachments, outbound copies, and system mail are exempt families.
func TestSweepOrphanBlobs(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()

	deliver := func(subject, msgID string) MsgMeta {
		raw := rawMsg("bob@remote.example", "ada@example.test", subject, msgID, nil, "body "+subject)
		parsed := ParseMessage(raw)
		meta := s.msgMetaFromParse(parsed, ThreadID(parsed.ThreadKey()))
		meta.MsgID = DeliveredMsgID("test", msgID, box.ID)
		if err := s.Deliver(ctx, Delivery{
			User: "ada", BoxID: box.ID, Folder: FolderInbox,
			Meta: meta, Raw: raw, SearchText: parsed.SearchText,
		}); err != nil {
			t.Fatal(err)
		}
		meta.BlobID = meta.MsgID // what Deliver assigns (deliver.go)
		return meta
	}
	live := deliver("stays", "<live@remote.example>")
	dead := deliver("stranded", "<dead@remote.example>")

	// Strand the second message the way a crashed purge does: message
	// row + msgref die, the blob and searchtext linger.
	if err := s.DB.Delete(ctx, msgRefPrefix+"ada/"+dead.MsgID); err != nil {
		t.Fatal(err)
	}
	// Exempt families: a draft attachment, an outbound copy, system mail.
	for _, k := range []string{
		blobsPrefix + "ada/att-abc123XYZabc",
		blobsPrefix + "ada/out-def456UVWdef",
		blobsPrefix + SystemMailAccount + "/deadbeef00000000",
	} {
		if err := s.DB.PutBlob(ctx, k, strings.NewReader("x"), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
	}

	// Sweep 1: the orphan is only STAGED (grace clock starts) — nothing
	// deleted yet, so a delivery racing its msgref write is safe.
	if n, err := s.SweepOrphanBlobs(ctx, 100, time.Hour); err != nil || n != 0 {
		t.Fatalf("first sweep: removed %d err %v", n, err)
	}
	if _, err := s.MessageBlob(ctx, "ada", dead.BlobID); err != nil {
		t.Fatal("first sweep deleted inside the grace window")
	}

	// Sweep 2 with zero grace: the candidate reclaims — blob and
	// searchtext both gone; the live message and exempt families stay.
	n, err := s.SweepOrphanBlobs(ctx, 100, 0)
	if err != nil || n != 1 {
		t.Fatalf("second sweep: removed %d err %v", n, err)
	}
	if _, err := s.MessageBlob(ctx, "ada", dead.BlobID); err == nil {
		t.Error("orphaned blob survived the sweep")
	}
	if txt, _ := s.SearchText(ctx, "ada", dead.BlobID); txt != "" {
		t.Error("orphaned searchtext survived the sweep")
	}
	if _, err := s.MessageBlob(ctx, "ada", live.BlobID); err != nil {
		t.Error("live message blob was reclaimed")
	}
	if txt, _ := s.SearchText(ctx, "ada", live.BlobID); txt == "" {
		t.Error("live searchtext was reclaimed")
	}
	for _, k := range []string{"ada/att-abc123XYZabc", "ada/out-def456UVWdef", SystemMailAccount + "/deadbeef00000000"} {
		user, id, _ := strings.Cut(k, "/")
		if _, err := s.MessageBlob(ctx, user, id); err != nil {
			t.Errorf("exempt blob %s was reclaimed", k)
		}
	}
	// The pending ledger is clean (no stale candidates left behind).
	if err := scanCount(ctx, s, gcPendingPrefix, 0); err != nil {
		t.Error(err)
	}

	// A candidate whose msgref REAPPEARS before grace is forgotten, not
	// reclaimed (the Deliver race in miniature).
	if err := s.DB.Delete(ctx, msgRefPrefix+"ada/"+live.MsgID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SweepOrphanBlobs(ctx, 100, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Set(ctx, msgRefPrefix+"ada/"+live.MsgID, []byte(`{"box_id":"x","thread_id":"y","ts_key":"z"}`)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SweepOrphanBlobs(ctx, 100, 0); err != nil || n != 0 {
		t.Fatalf("revived candidate was reclaimed: removed %d err %v", n, err)
	}
	if _, err := s.MessageBlob(ctx, "ada", live.BlobID); err != nil {
		t.Error("revived message's blob was reclaimed")
	}
}

// TestSweepOrphanBlobsPaging proves the persisted cursor amortizes the
// scan: with pageLimit 1 the discovery still reaches every orphan
// across successive sweeps.
func TestSweepOrphanBlobsPaging(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()

	var blobIDs []string
	for _, m := range []string{"<p1@r.example>", "<p2@r.example>", "<p3@r.example>"} {
		raw := rawMsg("bob@remote.example", "ada@example.test", "page "+m, m, nil, "body")
		parsed := ParseMessage(raw)
		meta := s.msgMetaFromParse(parsed, ThreadID(parsed.ThreadKey()))
		meta.MsgID = DeliveredMsgID("test", m, box.ID)
		if err := s.Deliver(ctx, Delivery{
			User: "ada", BoxID: box.ID, Folder: FolderInbox,
			Meta: meta, Raw: raw, SearchText: parsed.SearchText,
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.DB.Delete(ctx, msgRefPrefix+"ada/"+meta.MsgID); err != nil {
			t.Fatal(err)
		}
		blobIDs = append(blobIDs, meta.MsgID) // BlobID == MsgID
	}
	// Page-1 sweeps: each pass stages one more candidate and reclaims
	// everything staged before it; all three must be gone eventually.
	total := 0
	for i := 0; i < 12 && total < 3; i++ {
		n, err := s.SweepOrphanBlobs(ctx, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if total != 3 {
		t.Fatalf("reclaimed %d of 3 across paged sweeps", total)
	}
	for _, id := range blobIDs {
		if _, err := s.MessageBlob(ctx, "ada", id); err == nil {
			t.Errorf("blob %s survived", id)
		}
	}
}

// scanCount asserts a prefix holds exactly want rows.
func scanCount(ctx context.Context, s *Store, prefix string, want int) error {
	entries, _, err := s.DB.List(ctx, prefix, "", 1000)
	if err != nil {
		return err
	}
	if len(entries) != want {
		return fmt.Errorf("%s: %d rows, want %d", prefix, len(entries), want)
	}
	return nil
}
