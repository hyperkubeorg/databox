// icsmail.go — outbound ICS invite mail (spec §7.6): inviting an
// EXTERNAL email address to an event sends a METHOD:REQUEST message
// through the normal outbound path (undo-send hold, rate limits, the
// gateway); edits re-send with a bumped SEQUENCE; removing an external
// (or deleting the event) sends METHOD:CANCEL. Internal invitees keep
// getting notifications (notify.go) — never mail.
//
// The per-event ledger (/pcp/docs/<d>/<n>/icsmail/<event>) records the
// stable UID, the current SEQUENCE, and who was last mailed, so a
// re-save without material changes re-sends nothing and a removed
// invitee is findable for the cancel. It dies with the document.
package calendar

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ics"
)

// icsLedger is the per-event outbound-invite state.
type icsLedger struct {
	UID  string   `json:"uid"`
	Seq  int      `json:"seq"`
	Sent []string `json:"sent,omitempty"` // externals last mailed
	// Fingerprint of the material fields at the last send — a re-save
	// that changes nothing material re-sends nothing.
	Material string `json:"material,omitempty"`
}

func icsLedgerKey(driveID, nodeID, eventID string) string {
	return "/pcp/docs/" + driveID + "/" + nodeID + "/icsmail/" + eventID
}

// externalInvitees extracts an event's external email invitees, sorted.
func externalInvitees(e collab.Event) []string {
	var out []string
	for who := range e.Invites {
		if collab.ExternalInvitee(who) {
			out = append(out, who)
		}
	}
	sort.Strings(out)
	return out
}

// materialFingerprint captures the fields whose change bumps SEQUENCE
// (RFC 5546's "significant change", approximated).
func materialFingerprint(e collab.Event) string {
	return strings.Join([]string{
		e.Title, e.Start.UTC().Format(time.RFC3339), e.End.UTC().Format(time.RFC3339),
		map[bool]string{true: "1", false: "0"}[e.AllDay], e.Location, e.Notes,
	}, "\x00")
}

// OrganizerBox resolves the actor's primary mailbox — the address
// invites go out as (deterministic: lowest address wins, matching the
// mail app's "first mailbox"). ok=false = the account has no mailbox
// and invite mail silently skips.
func (s *Store) OrganizerBox(ctx context.Context, username string) (mail.Mailbox, bool) {
	if s.Mail == nil {
		return mail.Mailbox{}, false
	}
	boxes, err := s.Mail.UserMailboxes(ctx, username)
	if err != nil || len(boxes) == 0 {
		return mail.Mailbox{}, false
	}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].Addr < boxes[j].Addr })
	return boxes[0], true
}

// AfterEventSave runs the whole people fan-out for one saved (or
// deleted: cur == nil) event: internal notifications plus external ICS
// mail, diffed against prev. Callers pass the event state from BEFORE
// the op (nil = didn't exist). Everything here is soft-fail — a lost
// notification or invite mail never fails the save that triggered it.
func (s *Store) AfterEventSave(ctx context.Context, sc site.Config, actor users.User, driveID, nodeID string, node nodes.Node, prev, cur *collab.Event) {
	if cur != nil {
		s.NotifyEventPeople(ctx, actor.Username, driveID, nodeID, node, *cur)
	}
	// External diff.
	var prevExt, curExt []string
	if prev != nil {
		prevExt = externalInvitees(*prev)
	}
	if cur != nil {
		curExt = externalInvitees(*cur)
	}
	if len(prevExt) == 0 && len(curExt) == 0 {
		return
	}
	box, ok := s.OrganizerBox(ctx, actor.Username)
	if !ok {
		s.warn("external invitees but no mailbox — invite mail skipped", "user", actor.Username)
		return
	}
	eventID := ""
	if cur != nil {
		eventID = cur.ID
	} else {
		eventID = prev.ID
	}
	var led icsLedger
	_, _ = kvx.GetJSON(ctx, s.DB, icsLedgerKey(driveID, nodeID, eventID), &led)
	if led.UID == "" {
		_, domain, _ := mail.SplitAddr(box.Addr)
		if domain == "" {
			domain = "localhost"
		}
		led.UID = eventID + "-" + nodeID + "@" + domain
	}

	curSet := map[string]bool{}
	for _, e := range curExt {
		curSet[e] = true
	}
	var removed []string
	for _, e := range led.Sent {
		if !curSet[e] {
			removed = append(removed, e)
		}
	}
	sentSet := map[string]bool{}
	for _, e := range led.Sent {
		sentSet[e] = true
	}

	// CANCEL to externals no longer invited (or the whole event gone).
	if len(removed) > 0 && prev != nil {
		cancel := icsEventFor(*prev, led, box.Addr, ics.MethodCancel, removed)
		s.sendICS(ctx, sc, actor, box, removed, "Cancelled: "+prev.Title, cancelBody(*prev), cancel)
	}
	if cur == nil || len(curExt) == 0 {
		led.Sent = nil
		_ = kvx.SetJSON(ctx, s.DB, icsLedgerKey(driveID, nodeID, eventID), led)
		return
	}

	// REQUEST: a material change re-sends to everyone with SEQUENCE+1;
	// otherwise only never-mailed invitees get the current revision.
	fp := materialFingerprint(*cur)
	targets := curExt
	subject := "Invitation: " + cur.Title
	if led.Material != "" && led.Material != fp {
		led.Seq++
		subject = "Updated invitation: " + cur.Title
	} else if led.Material == fp {
		targets = nil
		for _, e := range curExt {
			if !sentSet[e] {
				targets = append(targets, e)
			}
		}
	}
	if len(targets) > 0 {
		req := icsEventFor(*cur, led, box.Addr, ics.MethodRequest, curExt)
		s.sendICS(ctx, sc, actor, box, targets, subject, inviteBody(*cur, actor), req)
	}
	led.Sent, led.Material = curExt, fp
	if err := kvx.SetJSON(ctx, s.DB, icsLedgerKey(driveID, nodeID, eventID), led); err != nil {
		s.warn("ics ledger write failed", "event", eventID, "err", err)
	}
}

// icsEventFor builds the wire event for one method.
func icsEventFor(e collab.Event, led icsLedger, organizer, method string, rcpts []string) ics.Event {
	out := ics.Event{
		Method: method, UID: led.UID, Sequence: led.Seq,
		Summary: e.Title, Description: e.Notes, Location: e.Location,
		Start: e.Start, End: e.End, AllDay: e.AllDay,
		Organizer: organizer,
	}
	for _, r := range rcpts {
		out.Attendees = append(out.Attendees, ics.Attendee{Email: r})
	}
	return out
}

// inviteBody is the human half of an invite message.
func inviteBody(e collab.Event, actor users.User) string {
	name := actor.DisplayName
	if name == "" {
		name = actor.Username
	}
	var b strings.Builder
	b.WriteString(name + " invited you to “" + e.Title + "”.\n\n")
	b.WriteString("When: " + eventWhen(e) + "\n")
	if e.Location != "" {
		b.WriteString("Where: " + e.Location + "\n")
	}
	if e.Notes != "" {
		b.WriteString("\n" + e.Notes + "\n")
	}
	b.WriteString("\nThe attached invitation works with any calendar app.\n")
	return b.String()
}

// cancelBody is the human half of a cancellation.
func cancelBody(e collab.Event) string {
	return "“" + e.Title + "” (" + eventWhen(e) + ") has been cancelled or you were removed from it.\n"
}

// sendICS queues one text/calendar message through the normal outbound
// path: the ICS rides as a text/calendar attachment part, staged like
// any other attachment. Soft-fail.
func (s *Store) sendICS(ctx context.Context, sc site.Config, actor users.User, box mail.Mailbox, to []string, subject, body string, ev ics.Event) {
	raw := ics.Encode(ev)
	att, err := s.Mail.StageAttachment(ctx, actor.Username, "invite.ics",
		"text/calendar; method="+ev.Method+"; charset=utf-8", strings.NewReader(string(raw)), sc.Mail.MsgBytes())
	if err != nil {
		s.warn("ics staging failed", "err", err)
		return
	}
	_, err = s.Mail.SendMessage(ctx, sc, actor, box, mail.ComposeInput{
		From: box.Addr, To: to, Subject: subject, Text: body,
		Atts: []mail.DraftAtt{att},
	})
	if err != nil {
		s.warn("ics invite mail failed", "to", strings.Join(to, ","), "err", err)
	}
}
