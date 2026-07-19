package mail

import "testing"

// TestThreadIDDeterminism is the spec §7.1 contract: the same message
// always computes the same thread id, and a References chain lands
// every reply in the root's conversation.
func TestThreadIDDeterminism(t *testing.T) {
	root := ThreadKey{
		MessageID: "<a1@example.test>", Subject: "Trip plans",
		Correspondents: []string{"ada@example.test", "bob@remote.io"},
	}
	rootID := ThreadID(root)
	if rootID == "" || !ValidThreadID(rootID) {
		t.Fatalf("bad thread id %q", rootID)
	}
	if ThreadID(root) != rootID {
		t.Error("thread id is not deterministic")
	}

	// A direct reply: References carries the root.
	reply := ThreadKey{
		MessageID: "<b2@remote.io>", InReplyTo: "<a1@example.test>",
		References: []string{"<a1@example.test>"},
		Subject:    "Re: Trip plans",
	}
	if ThreadID(reply) != rootID {
		t.Error("reply with References did not join the root thread")
	}

	// A reply-to-the-reply: References lists the whole chain oldest
	// first — the root stays the anchor.
	deep := ThreadKey{
		MessageID:  "<c3@example.test>",
		InReplyTo:  "<b2@remote.io>",
		References: []string{"<a1@example.test>", "<b2@remote.io>"},
		Subject:    "RE: Re: Trip plans",
	}
	if ThreadID(deep) != rootID {
		t.Error("deep reply did not join the root thread")
	}

	// In-Reply-To only (no References): joins the parent's id — which
	// for a first-level reply IS the root.
	irt := ThreadKey{MessageID: "<d4@x.io>", InReplyTo: "<a1@example.test>", Subject: "Re: Trip plans"}
	if ThreadID(irt) != rootID {
		t.Error("In-Reply-To-only reply did not join the root thread")
	}

	// A different root is a different thread.
	other := ThreadKey{MessageID: "<z9@example.test>", Subject: "Trip plans"}
	if ThreadID(other) == rootID {
		t.Error("distinct roots collided")
	}
}

// TestThreadIDSubjectFallback covers headerless mail: normalized
// subject + sorted correspondent set.
func TestThreadIDSubjectFallback(t *testing.T) {
	a := ThreadKey{Subject: "Quarterly report", Correspondents: []string{"ada@example.test", "bob@remote.io"}}
	b := ThreadKey{Subject: "Re: Quarterly report", Correspondents: []string{"bob@remote.io", "ada@example.test"}}
	c := ThreadKey{Subject: "FWD: Re: quarterly   REPORT", Correspondents: []string{"Ada@Example.Test", "bob@remote.io"}}
	if ThreadID(a) != ThreadID(b) || ThreadID(b) != ThreadID(c) {
		t.Error("subject fallback is order/prefix/case sensitive")
	}
	// Different correspondents = different conversation.
	d := ThreadKey{Subject: "Quarterly report", Correspondents: []string{"eve@else.where"}}
	if ThreadID(d) == ThreadID(a) {
		t.Error("different correspondent sets collided")
	}
	// The fallback never collides with a Message-ID-rooted thread.
	e := ThreadKey{MessageID: "<quarterly report@x>", Subject: "Quarterly report",
		Correspondents: []string{"ada@example.test", "bob@remote.io"}}
	if ThreadID(e) == ThreadID(a) {
		t.Error("header and fallback derivations collided")
	}
}

func TestNormalizeSubject(t *testing.T) {
	cases := map[string]string{
		"Re: Hello":            "hello",
		"RE[2]: Re: Hello":     "hello",
		"Fwd: FW: fw: Hello":   "hello",
		"  Hello   world  ":    "hello world",
		"Renovation plans":     "renovation plans", // "Re" prefix must not eat words
		"Fwd:Re:Double header": "double header",
	}
	for in, want := range cases {
		if got := NormalizeSubject(in); got != want {
			t.Errorf("NormalizeSubject(%q) = %q, want %q", in, got, want)
		}
	}
}
