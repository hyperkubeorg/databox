package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// newTestStore boots a fake databox with one user (ada), one hosted
// domain (example.test), and one mailbox (ada@example.test).
func newTestStore(t *testing.T) (*Store, Mailbox) {
	t.Helper()
	ctx := context.Background()
	db := kvxtest.New(t)
	us := &users.Store{DB: db}
	s := &Store{DB: db, Users: us, Notify: &notify.Store{DB: db}}
	if _, err := us.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDomain(ctx, "example.test", "ada"); err != nil {
		t.Fatal(err)
	}
	box, err := s.CreateMailbox(ctx, "ada", "example.test", "ada", 5)
	if err != nil {
		t.Fatal(err)
	}
	return s, box
}

// msgClock hands each test message a distinct, increasing Date —
// header dates carry second precision, and message order within a
// thread is date-keyed.
var msgClock = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

func nextMsgDate() time.Time {
	msgClock = msgClock.Add(time.Minute)
	return msgClock
}

// rawMsg builds a minimal RFC 822 message.
func rawMsg(from, to, subject, msgID string, refs []string, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\n", from, to, subject)
	fmt.Fprintf(&b, "Date: %s\r\n", nextMsgDate().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: %s\r\n", msgID)
	if len(refs) > 0 {
		fmt.Fprintf(&b, "References: %s\r\n", strings.Join(refs, " "))
		fmt.Fprintf(&b, "In-Reply-To: %s\r\n", refs[len(refs)-1])
	}
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", body)
	return []byte(b.String())
}

// deliverRaw runs the intake-shaped pipeline: parse → thread id →
// Deliver, with a deterministic delivery id derived from source/key.
func deliverRaw(t *testing.T, s *Store, box Mailbox, folder, source, key string, raw []byte) MsgMeta {
	t.Helper()
	p := ParseMessage(raw)
	meta := s.msgMetaFromParse(p, ThreadID(p.ThreadKey()))
	meta.MsgID = DeliveredMsgID(source, key, box.ID)
	if meta.Date.IsZero() {
		meta.Date = time.Now()
	}
	if err := s.Deliver(context.Background(), Delivery{
		User: box.Owner, BoxID: box.ID, Folder: folder,
		Meta: meta, Raw: raw, SearchText: p.SearchText,
	}); err != nil {
		t.Fatalf("deliver %s/%s: %v", source, key, err)
	}
	return meta
}

// TestThreadedDeliveryAndRollups: a three-message References chain
// lands as ONE thread with ascending messages, the right counts, and
// exactly one inbox index row at the latest activity.
func TestThreadedDeliveryAndRollups(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()

	m1 := rawMsg("bob@remote.io", "ada@example.test", "Trip", "<a1@remote.io>", nil, "first")
	m2 := rawMsg("ada@example.test", "bob@remote.io", "Re: Trip", "<b2@example.test>", []string{"<a1@remote.io>"}, "second")
	m3 := rawMsg("bob@remote.io", "ada@example.test", "Re: Trip", "<c3@remote.io>", []string{"<a1@remote.io>", "<b2@example.test>"}, "third")
	deliverRaw(t, s, box, FolderInbox, "po1", "s1", m1)
	deliverRaw(t, s, box, FolderInbox, "po1", "s2", m2)
	deliverRaw(t, s, box, FolderInbox, "po1", "s3", m3)

	rows, _, err := s.ListThreads(ctx, "ada", box.ID, FolderInbox, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("inbox threads = %d, want 1", len(rows))
	}
	th := rows[0]
	if th.MsgCount != 3 || th.UnreadCount != 3 {
		t.Errorf("counts = %d msgs / %d unread, want 3/3", th.MsgCount, th.UnreadCount)
	}
	if th.Snippet != "third" {
		t.Errorf("snippet = %q, want the latest message's", th.Snippet)
	}
	msgs, err := s.ListThreadMessages(ctx, "ada", box.ID, th.ThreadID)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("thread messages = %d (%v), want 3", len(msgs), err)
	}
	// Ascending: oldest first.
	if msgs[0].Snippet != "first" || msgs[2].Snippet != "third" {
		t.Errorf("messages out of order: %q … %q", msgs[0].Snippet, msgs[2].Snippet)
	}
	// Mark read → rollup zeroes; unread again → restores.
	if err := s.MarkThreadRead(ctx, "ada", box.ID, th.ThreadID, true); err != nil {
		t.Fatal(err)
	}
	th2, _, _ := s.GetThread(ctx, "ada", box.ID, th.ThreadID)
	if th2.UnreadCount != 0 {
		t.Errorf("unread after mark-read = %d", th2.UnreadCount)
	}
	if err := s.MarkThreadRead(ctx, "ada", box.ID, th.ThreadID, false); err != nil {
		t.Fatal(err)
	}
	th3, _, _ := s.GetThread(ctx, "ada", box.ID, th.ThreadID)
	if th3.UnreadCount != 3 {
		t.Errorf("unread after mark-unread = %d, want 3", th3.UnreadCount)
	}
	assertNoOrphanIndexes(t, s, "ada")
}

// TestIdempotentRedelivery: replaying the same spool entry (same
// deterministic id) writes nothing new.
func TestIdempotentRedelivery(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	raw := rawMsg("bob@remote.io", "ada@example.test", "Dup", "<dup@remote.io>", nil, "hello")
	deliverRaw(t, s, box, FolderInbox, "po1", "spool-9", raw)
	u1, _, _ := s.Users.Get(ctx, "ada")
	deliverRaw(t, s, box, FolderInbox, "po1", "spool-9", raw) // replay
	u2, _, _ := s.Users.Get(ctx, "ada")

	rows, _, _ := s.ListThreads(ctx, "ada", box.ID, FolderInbox, "", 10, 0)
	if len(rows) != 1 || rows[0].MsgCount != 1 {
		t.Fatalf("redelivery duplicated: threads=%d msgs=%d", len(rows), rows[0].MsgCount)
	}
	if u1.UsedBytes != u2.UsedBytes {
		t.Errorf("redelivery re-charged quota: %d → %d", u1.UsedBytes, u2.UsedBytes)
	}
	assertNoOrphanIndexes(t, s, "ada")
}

// TestIndexRefileAtomicity: after a storm of deliveries, moves, stars,
// and labels, a full scan finds exactly the index rows the canonical
// metas imply — no orphans, no misses.
func TestIndexRefileAtomicity(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	labels, _ := s.ListLabels(ctx, "ada")
	if len(labels) == 0 {
		t.Fatal("starter labels missing")
	}

	// Ten conversations, some replied to (re-files), then a churn of
	// moves/stars/labels.
	var threadIDs []string
	for i := 0; i < 10; i++ {
		root := fmt.Sprintf("<r%d@remote.io>", i)
		raw := rawMsg("bob@remote.io", "ada@example.test", fmt.Sprintf("Topic %d", i), root, nil, "body")
		m := deliverRaw(t, s, box, FolderInbox, "po1", fmt.Sprintf("s%d", i), raw)
		threadIDs = append(threadIDs, m.ThreadID)
		if i%2 == 0 {
			reply := rawMsg("carol@else.where", "ada@example.test", fmt.Sprintf("Re: Topic %d", i),
				fmt.Sprintf("<re%d@else.where>", i), []string{root}, "reply")
			deliverRaw(t, s, box, FolderInbox, "po1", fmt.Sprintf("s%d-re", i), reply)
		}
	}
	moves := []string{FolderArchive, FolderTrash, FolderSpam, FolderInbox}
	for i, id := range threadIDs {
		if err := s.MoveThread(ctx, "ada", box.ID, id, moves[i%len(moves)]); err != nil {
			t.Fatalf("move %d: %v", i, err)
		}
		if i%3 == 0 {
			if err := s.SetThreadStarred(ctx, "ada", box.ID, id, true); err != nil {
				t.Fatal(err)
			}
		}
		if i%2 == 1 {
			if err := s.SetThreadLabel(ctx, "ada", box.ID, id, labels[i%len(labels)].ID, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A couple of label removals and unstars re-file again.
	_ = s.SetThreadStarred(ctx, "ada", box.ID, threadIDs[0], false)
	_ = s.SetThreadLabel(ctx, "ada", box.ID, threadIDs[1], labels[1].ID, false)

	assertNoOrphanIndexes(t, s, "ada")
}

// assertNoOrphanIndexes fully scans every index family and asserts a
// perfect bijection with what the canonical thread metas imply.
func assertNoOrphanIndexes(t *testing.T, s *Store, user string) {
	t.Helper()
	ctx := context.Background()
	want := map[string]bool{}
	err := kvx.ScanPrefix(ctx, s.DB, threadsPrefix+user+"/", func(_ string, value []byte) error {
		var m ThreadMeta
		if err := json.Unmarshal(value, &m); err != nil {
			return err
		}
		for _, k := range indexKeys(user, m) {
			want[k] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, prefix := range []string{
		threadIdxPrefix + user + "/", sentIdxPrefix + user + "/",
		starredPrefix + user + "/", byLabelPrefix + user + "/",
	} {
		if err := kvx.ScanPrefix(ctx, s.DB, prefix, func(key string, _ []byte) error {
			got[key] = true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("ORPHAN index row: %s", k)
		}
	}
	for k := range want {
		if !got[k] {
			t.Errorf("MISSING index row: %s", k)
		}
	}
}

// TestSentFacetAndFolderExclusivity: an outbound message makes the
// thread visible in Sent WITHOUT leaving its one folder.
func TestSentFacetAndFolderExclusivity(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()

	inbound := rawMsg("bob@remote.io", "ada@example.test", "Facet", "<f1@remote.io>", nil, "hi ada")
	deliverRaw(t, s, box, FolderInbox, "po1", "f1", inbound)

	// Ada's reply (outbound) joins the same thread.
	reply := rawMsg("ada@example.test", "bob@remote.io", "Re: Facet", "<f2@example.test>", []string{"<f1@remote.io>"}, "hi bob")
	p := ParseMessage(reply)
	meta := s.msgMetaFromParse(p, ThreadID(p.ThreadKey()))
	meta.MsgID = DeliveredMsgID("sent", "out-1", box.ID)
	meta.Seen, meta.Outbound = true, true
	if err := s.Deliver(ctx, Delivery{
		User: "ada", BoxID: box.ID, Folder: FolderArchive,
		Meta: meta, Raw: reply, SearchText: p.SearchText,
	}); err != nil {
		t.Fatal(err)
	}

	inboxRows, _, _ := s.ListThreads(ctx, "ada", box.ID, FolderInbox, "", 10, 0)
	sentRows, _, _ := s.ListSent(ctx, "ada", box.ID, "", 10)
	if len(inboxRows) != 1 {
		t.Fatalf("inbox rows = %d, want 1 (thread keeps its one folder)", len(inboxRows))
	}
	if len(sentRows) != 1 || sentRows[0].ThreadID != inboxRows[0].ThreadID {
		t.Fatalf("sent facet rows = %d, want the same thread", len(sentRows))
	}
	if inboxRows[0].UnreadCount != 1 {
		t.Errorf("outbound message counted as unread: %d", inboxRows[0].UnreadCount)
	}
	if !inboxRows[0].HasOutbound || inboxRows[0].MsgCount != 2 {
		t.Errorf("facet meta wrong: %+v", inboxRows[0])
	}
	assertNoOrphanIndexes(t, s, "ada")
}

// TestMoveArchiveResurface: archived threads return to the inbox when
// new mail arrives; spam-tagged mail never moves an established thread.
func TestMoveArchiveResurface(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	raw := rawMsg("bob@remote.io", "ada@example.test", "Wake", "<w1@remote.io>", nil, "one")
	m := deliverRaw(t, s, box, FolderInbox, "po1", "w1", raw)
	if err := s.MoveThread(ctx, "ada", box.ID, m.ThreadID, FolderArchive); err != nil {
		t.Fatal(err)
	}
	reply := rawMsg("bob@remote.io", "ada@example.test", "Re: Wake", "<w2@remote.io>", []string{"<w1@remote.io>"}, "two")
	deliverRaw(t, s, box, FolderInbox, "po1", "w2", reply)
	th, _, _ := s.GetThread(ctx, "ada", box.ID, m.ThreadID)
	if th.Folder != FolderInbox {
		t.Errorf("archived thread did not resurface: folder=%s", th.Folder)
	}
	// A spam delivery into the same thread leaves it in the inbox.
	spam := rawMsg("bob@remote.io", "ada@example.test", "Re: Wake", "<w3@remote.io>", []string{"<w1@remote.io>"}, "three")
	deliverRaw(t, s, box, FolderSpam, "po1", "w3", spam)
	th2, _, _ := s.GetThread(ctx, "ada", box.ID, m.ThreadID)
	if th2.Folder != FolderInbox {
		t.Errorf("spam delivery moved an established thread to %s", th2.Folder)
	}
	assertNoOrphanIndexes(t, s, "ada")
}

// TestPurgeRefundsQuota: purging from trash deletes everything and
// refunds the bytes.
func TestPurgeRefundsQuota(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	raw := rawMsg("bob@remote.io", "ada@example.test", "Purge", "<p1@remote.io>", nil, "delete me")
	m := deliverRaw(t, s, box, FolderInbox, "po1", "p1", raw)
	u1, _, _ := s.Users.Get(ctx, "ada")
	if u1.UsedBytes == 0 {
		t.Fatal("delivery did not charge quota")
	}
	if err := s.MoveThread(ctx, "ada", box.ID, m.ThreadID, FolderTrash); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeThread(ctx, "ada", box.ID, m.ThreadID); err != nil {
		t.Fatal(err)
	}
	u2, _, _ := s.Users.Get(ctx, "ada")
	if u2.UsedBytes != 0 {
		t.Errorf("purge did not refund: used=%d", u2.UsedBytes)
	}
	if _, _, found, _ := s.GetMessage(ctx, "ada", m.MsgID); found {
		t.Error("msgref survived purge")
	}
	if text, _ := s.SearchText(ctx, "ada", m.MsgID); text != "" {
		t.Error("search text survived purge")
	}
	// Every index family is empty.
	assertNoOrphanIndexes(t, s, "ada")
	rows, _, _ := s.ListThreads(ctx, "ada", box.ID, FolderTrash, "", 10, 0)
	if len(rows) != 0 {
		t.Error("trash still lists the purged thread")
	}
}

// TestTrashRetentionLazyPrune: listing trash purges threads past the
// retention window.
func TestTrashRetentionLazyPrune(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	raw := rawMsg("bob@remote.io", "ada@example.test", "Old", "<o1@remote.io>", nil, "stale")
	m := deliverRaw(t, s, box, FolderInbox, "po1", "o1", raw)
	if err := s.MoveThread(ctx, "ada", box.ID, m.ThreadID, FolderTrash); err != nil {
		t.Fatal(err)
	}
	// Backdate TrashedAt past the window.
	if err := s.mutateThread(ctx, "ada", box.ID, m.ThreadID, func(tm *ThreadMeta) error {
		tm.TrashedAt = time.Now().AddDate(0, 0, -45)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.ListThreads(ctx, "ada", box.ID, FolderTrash, "", 10, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("lazy prune left %d threads in trash", len(rows))
	}
	u, _, _ := s.Users.Get(ctx, "ada")
	if u.UsedBytes != 0 {
		t.Errorf("prune did not refund: used=%d", u.UsedBytes)
	}
}

// TestLabelsCRUDAndIndex: starter set exists; set/unset maintains the
// bylabel index; deleting a label strips threads.
func TestLabelsCRUDAndIndex(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	labels, err := s.ListLabels(ctx, "ada")
	if err != nil || len(labels) != 4 {
		t.Fatalf("starter labels = %d (%v), want 4", len(labels), err)
	}
	if labels[0].Name != "Work" || labels[0].Color != "#67C99A" {
		t.Errorf("starter label wrong: %+v", labels[0])
	}
	raw := rawMsg("bob@remote.io", "ada@example.test", "Labeled", "<l1@remote.io>", nil, "x")
	m := deliverRaw(t, s, box, FolderInbox, "po1", "l1", raw)
	work := labels[0]
	if err := s.SetThreadLabel(ctx, "ada", box.ID, m.ThreadID, work.ID, true); err != nil {
		t.Fatal(err)
	}
	rows, _, _ := s.ListByLabel(ctx, "ada", work.ID, "", 10)
	if len(rows) != 1 || rows[0].ThreadID != m.ThreadID {
		t.Fatalf("bylabel rows = %d", len(rows))
	}
	// New label + assign + delete → stripped everywhere.
	l, err := s.CreateLabel(ctx, "ada", "Receipts", "#AABBCC")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetThreadLabel(ctx, "ada", box.ID, m.ThreadID, l.ID, true)
	if err := s.DeleteLabel(ctx, "ada", l.ID); err != nil {
		t.Fatal(err)
	}
	th, _, _ := s.GetThread(ctx, "ada", box.ID, m.ThreadID)
	for _, id := range th.Labels {
		if id == l.ID {
			t.Error("deleted label still on thread meta")
		}
	}
	assertNoOrphanIndexes(t, s, "ada")
}

// TestPerMessageStar: a message star flips independently of the thread
// star.
func TestPerMessageStar(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	raw := rawMsg("bob@remote.io", "ada@example.test", "Star", "<st1@remote.io>", nil, "x")
	m := deliverRaw(t, s, box, FolderInbox, "po1", "st1", raw)
	if err := s.SetMessageStarred(ctx, "ada", m.MsgID, true); err != nil {
		t.Fatal(err)
	}
	meta, _, found, _ := s.GetMessage(ctx, "ada", m.MsgID)
	if !found || !meta.Starred {
		t.Error("message star lost")
	}
	th, _, _ := s.GetThread(ctx, "ada", box.ID, m.ThreadID)
	if th.Starred {
		t.Error("message star leaked to the thread")
	}
	rows, _, _ := s.ListStarred(ctx, "ada", box.ID, "", 10)
	if len(rows) != 0 {
		t.Error("starred index has a row without a thread star")
	}
}

// TestCustomFolders: create, move in, delete → threads land in
// Archive.
func TestCustomFolders(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	f, err := s.CreateFolder(ctx, "ada", box.ID, "Receipts")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFolder(ctx, "ada", box.ID, "inbox"); err == nil {
		t.Error("system folder name accepted")
	}
	raw := rawMsg("bob@remote.io", "ada@example.test", "Receipt", "<rc1@remote.io>", nil, "x")
	m := deliverRaw(t, s, box, FolderInbox, "po1", "rc1", raw)
	if err := s.MoveThread(ctx, "ada", box.ID, m.ThreadID, f.ID); err != nil {
		t.Fatal(err)
	}
	rows, _, _ := s.ListThreads(ctx, "ada", box.ID, f.ID, "", 10, 0)
	if len(rows) != 1 {
		t.Fatalf("custom folder rows = %d", len(rows))
	}
	if err := s.DeleteFolder(ctx, "ada", box.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	th, _, _ := s.GetThread(ctx, "ada", box.ID, m.ThreadID)
	if th.Folder != FolderArchive {
		t.Errorf("folder delete left thread in %s", th.Folder)
	}
	assertNoOrphanIndexes(t, s, "ada")
}
