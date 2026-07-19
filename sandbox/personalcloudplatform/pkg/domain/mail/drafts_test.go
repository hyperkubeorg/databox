package mail

import (
	"context"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// TestDraftRoundTrip covers save → get → update → list → delete with
// quota deltas riding the whole way.
func TestDraftRoundTrip(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()

	d, err := s.SaveDraft(ctx, "ada", Draft{
		BoxID: box.ID, From: box.Addr, To: []string{"friend@remote.example"},
		Subject: "hello", HTML: "<p>hi there</p>",
	}, 0)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if d.ID == "" || d.Size == 0 {
		t.Fatalf("draft not filled: %+v", d)
	}
	got, found, err := s.GetDraft(ctx, "ada", box.ID, d.ID)
	if err != nil || !found || got.Subject != "hello" || got.HTML != "<p>hi there</p>" {
		t.Fatalf("get: %v %v %+v", err, found, got)
	}
	// Update in place: same id, new content, size re-charges by delta.
	got.Subject = "hello again"
	upd, err := s.SaveDraft(ctx, "ada", got, 0)
	if err != nil || upd.ID != d.ID {
		t.Fatalf("update: %v (%s vs %s)", err, upd.ID, d.ID)
	}
	list, err := s.ListDrafts(ctx, "ada", box.ID)
	if err != nil || len(list) != 1 || list[0].Subject != "hello again" {
		t.Fatalf("list: %v %+v", err, list)
	}
	if err := s.DeleteDraft(ctx, "ada", box.ID, d.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if u, _, _ := s.Users.Get(ctx, "ada"); u.UsedBytes != 0 {
		t.Errorf("quota not refunded: %d", u.UsedBytes)
	}
}

// TestDraftAttachments covers staging, appending, size accounting,
// removal, and the send-time ConsumeDraft (blobs survive for undo).
func TestDraftAttachments(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()

	d, err := s.SaveDraft(ctx, "ada", Draft{BoxID: box.ID, Subject: "att"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	att, err := s.StageAttachment(ctx, "ada", "doc.pdf", "application/pdf", strings.NewReader("%PDF-1.4 payload"), 1<<20)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if att.BlobID == "" || att.Size != int64(len("%PDF-1.4 payload")) {
		t.Fatalf("staged att wrong: %+v", att)
	}
	d, err = s.AppendDraftAtt(ctx, "ada", box.ID, d.ID, att, 0)
	if err != nil || len(d.Atts) != 1 {
		t.Fatalf("append: %v %+v", err, d.Atts)
	}
	if d.Size != int64(len("att"))+att.Size {
		t.Errorf("size = %d, want text+att", d.Size)
	}
	// The staged blob reads back through the mail blob space.
	if raw, err := s.MessageBlob(ctx, "ada", att.BlobID); err != nil || string(raw) != "%PDF-1.4 payload" {
		t.Fatalf("blob readback: %v %q", err, raw)
	}
	// Remove refunds and deletes the blob.
	d, err = s.RemoveDraftAtt(ctx, "ada", box.ID, d.ID, att.ID, 0)
	if err != nil || len(d.Atts) != 0 {
		t.Fatalf("remove: %v %+v", err, d.Atts)
	}
	if _, err := s.MessageBlob(ctx, "ada", att.BlobID); err == nil {
		t.Error("removed attachment blob survived")
	}
	// Re-attach, then ConsumeDraft: the row goes, the blob STAYS (the
	// held outbound references it until release).
	att2, _ := s.StageAttachment(ctx, "ada", "b.txt", "text/plain", strings.NewReader("bytes"), 1<<20)
	if _, err := s.AppendDraftAtt(ctx, "ada", box.ID, d.ID, att2, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeDraft(ctx, "ada", box.ID, d.ID); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, found, _ := s.GetDraft(ctx, "ada", box.ID, d.ID); found {
		t.Error("consumed draft row survived")
	}
	if raw, err := s.MessageBlob(ctx, "ada", att2.BlobID); err != nil || string(raw) != "bytes" {
		t.Errorf("consumed draft's blob must survive for undo: %v", err)
	}
	if u, _, _ := s.Users.Get(ctx, "ada"); u.UsedBytes != 0 {
		t.Errorf("consume must refund the draft charge: %d", u.UsedBytes)
	}
}

// TestStageAttachmentCap refuses oversize uploads.
func TestStageAttachmentCap(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.StageAttachment(context.Background(), "ada", "big.bin", "", strings.NewReader(strings.Repeat("x", 100)), 50); err == nil {
		t.Fatal("oversize attachment accepted")
	}
}

// TestComposeMessageAtts covers the multipart/mixed render: nested
// alternative body, base64 attachment part, threading headers intact.
func TestComposeMessageAtts(t *testing.T) {
	raw := string(ComposeMessageAtts(ComposeInput{
		From: "ada@example.test", To: []string{"b@remote.example"},
		Subject: "with files", HTML: "<p>see attached</p>",
		InReplyTo:  "<parent@remote.example>",
		References: []string{"<root@remote.example>", "<parent@remote.example>"},
	}, []AttData{{Name: "doc.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4\n")}}))

	for _, want := range []string{
		"Content-Type: multipart/mixed",
		"Content-Type: multipart/alternative",
		"Content-Type: text/html",
		"Content-Type: application/pdf",
		`Content-Disposition: attachment; filename="doc.pdf"`,
		"Content-Transfer-Encoding: base64",
		"JVBERi0xLjQK", // base64("%PDF-1.4\n")
		"In-Reply-To: <parent@remote.example>",
		"References: <root@remote.example> <parent@remote.example>",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("message missing %q", want)
		}
	}
	// The parse pipeline sees it as an attachment-carrying message.
	p := ParseMessage([]byte(raw))
	if !p.HasAttach {
		t.Error("parse missed the attachment")
	}
	if p.Subject != "with files" {
		t.Errorf("subject = %q", p.Subject)
	}
}

// TestSendWithAttachments sends through the real store path: staged
// blobs fold into the raw message, cancel returns the att metadata.
func TestSendWithAttachments(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	sc := site.Config{}
	sc.Mail.Enabled = true
	ada, _, _ := s.Users.Get(ctx, "ada")
	ada.Prefs.UndoSendSecs = 30

	att, err := s.StageAttachment(ctx, "ada", "notes.txt", "text/plain", strings.NewReader("attached text"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.SendMessage(ctx, sc, ada, box, ComposeInput{
		From: box.Addr, To: []string{"friend@remote.example"},
		Subject: "files", Text: "body", Atts: []DraftAtt{att},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	_, om, found := s.FindOutbound(ctx, res.OutID)
	if !found || om.State != OutHeld {
		t.Fatalf("outbound not held: %v %+v", found, om)
	}
	raw, err := s.MessageBlob(ctx, om.BlobOf, om.BlobID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "multipart/mixed") || !strings.Contains(string(raw), `filename="notes.txt"`) {
		t.Error("queued message lacks the attachment part")
	}
	in, err := s.CancelOutbound(ctx, "ada", res.OutID)
	if err != nil || len(in.Atts) != 1 || in.Atts[0].Name != "notes.txt" || in.Atts[0].BlobID == "" {
		t.Fatalf("cancel must return attachment metadata: %v %+v", err, in.Atts)
	}
	// The staged blob still exists — the restored draft can send again.
	if _, err := s.MessageBlob(ctx, "ada", in.Atts[0].BlobID); err != nil {
		t.Errorf("staged blob gone after cancel: %v", err)
	}
}
