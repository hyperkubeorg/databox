package mail

import (
	"context"
	"testing"
)

func TestParseQuery(t *testing.T) {
	q := ParseQuery("from:bob to:ada label:Work in:inbox has:file quarterly report")
	if q.From != "bob" || q.To != "ada" || q.Label != "work" || q.In != "inbox" || !q.HasFile {
		t.Errorf("operators wrong: %+v", q)
	}
	if len(q.Terms) != 2 || q.Terms[0] != "quarterly" || q.Terms[1] != "report" {
		t.Errorf("terms wrong: %v", q.Terms)
	}
}

// TestSearchOperators drives every operator over a small corpus.
func TestSearchOperators(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()

	// #1: from bob, has attachment wording in body, inbox.
	m1raw := []byte("From: Bob <bob@remote.io>\r\nTo: ada@example.test\r\n" +
		"Subject: Quarterly report\r\nMessage-ID: <q1@remote.io>\r\n" +
		"Date: Mon, 06 Jul 2026 10:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=xx\r\n\r\n" +
		"--xx\r\nContent-Type: text/plain\r\n\r\nnumbers inside the spreadsheet\r\n" +
		"--xx\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=q.pdf\r\n\r\nPDF\r\n--xx--\r\n")
	t1 := deliverRaw(t, s, box, FolderInbox, "po1", "q1", m1raw)

	// #2: from carol, no attachment, archived.
	m2raw := rawMsg("carol@else.where", "ada@example.test", "Lunch plans", "<q2@else.where>", nil, "tacos on friday")
	t2 := deliverRaw(t, s, box, FolderInbox, "po1", "q2", m2raw)
	if err := s.MoveThread(ctx, "ada", box.ID, t2.ThreadID, FolderArchive); err != nil {
		t.Fatal(err)
	}
	// Label #2 with Work.
	labels, _ := s.ListLabels(ctx, "ada")
	if err := s.SetThreadLabel(ctx, "ada", box.ID, t2.ThreadID, labels[0].ID, true); err != nil {
		t.Fatal(err)
	}

	run := func(q string) []ThreadMeta {
		t.Helper()
		out, err := s.SearchThreads(ctx, "ada", box.ID, ParseQuery(q), 10)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		return out
	}
	if got := run("from:bob"); len(got) != 1 || got[0].ThreadID != t1.ThreadID {
		t.Errorf("from: matched %d", len(got))
	}
	if got := run("to:ada quarterly"); len(got) != 1 {
		t.Errorf("to:+term matched %d", len(got))
	}
	if got := run("label:work"); len(got) != 1 || got[0].ThreadID != t2.ThreadID {
		t.Errorf("label: matched %d", len(got))
	}
	if got := run("in:archive"); len(got) != 1 || got[0].ThreadID != t2.ThreadID {
		t.Errorf("in: matched %d", len(got))
	}
	if got := run("has:file"); len(got) != 1 || got[0].ThreadID != t1.ThreadID {
		t.Errorf("has:file matched %d", len(got))
	}
	// Body term falls through to search text.
	if got := run("spreadsheet"); len(got) != 1 || got[0].ThreadID != t1.ThreadID {
		t.Errorf("body term matched %d", len(got))
	}
	// AND semantics: both terms must hit.
	if got := run("tacos spreadsheet"); len(got) != 0 {
		t.Errorf("AND semantics broken: %d", len(got))
	}
	// Unknown label matches nothing.
	if got := run("label:nosuch"); len(got) != 0 {
		t.Errorf("unknown label matched %d", len(got))
	}
	// Operators combine.
	if got := run("from:carol in:archive tacos"); len(got) != 1 {
		t.Errorf("combined operators matched %d", len(got))
	}
	if got := run("from:carol in:inbox"); len(got) != 0 {
		t.Errorf("folder mismatch matched %d", len(got))
	}
}
