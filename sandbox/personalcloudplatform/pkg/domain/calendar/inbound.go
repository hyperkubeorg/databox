// inbound.go — answering a FOREIGN invite (spec §7.6): an inbound
// text/calendar METHOD:REQUEST renders as a card in the reading pane;
// Accept/Maybe/Decline writes the event into the member's primary
// personal calendar (status recorded on their own invite entry) and the
// mail app emails the METHOD:REPLY back to the organizer. The answer is
// remembered per (user, UID) at /pcp/mail/icsrsvp/<user>/<uidHash>, so
// the card shows the current state on every later read.
package calendar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ics"
)

// icsRSVPPrefix roots the inbound-invite answers (kvx key table).
const icsRSVPPrefix = "/pcp/mail/icsrsvp/"

// InboundRSVP is one remembered answer to a foreign invite.
type InboundRSVP struct {
	Status  string    `json:"status"` // yes | no | maybe
	At      time.Time `json:"at"`
	DriveID string    `json:"drive"`
	NodeID  string    `json:"node"`
	EventID string    `json:"event"`
}

// uidHash keys an ICS UID into the ledger (UIDs are foreign-shaped).
func uidHash(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(sum[:8])
}

// icsEventID derives the deterministic local event id for a foreign
// UID (fits the substrate's entity-id grammar).
func icsEventID(uid string) string { return "ics" + uidHash(uid)[:12] }

// InboundRSVPStatus reads the member's remembered answer for a UID.
func (s *Store) InboundRSVPStatus(ctx context.Context, username, uid string) (InboundRSVP, bool) {
	var rec InboundRSVP
	if users.ValidUsername(username) != nil || uid == "" {
		return rec, false
	}
	found, err := kvx.GetJSON(ctx, s.DB, icsRSVPPrefix+username+"/"+uidHash(uid), &rec)
	return rec, err == nil && found
}

// ApplyInboundRSVP answers a foreign METHOD:REQUEST: the event lands in
// (or updates within) the member's primary personal calendar with their
// own status recorded, and the answer is remembered. Returns the stored
// event's location. quota bounds the lazy default-calendar create.
func (s *Store) ApplyInboundRSVP(ctx context.Context, user users.User, inv ics.Event, status string, quota int64) (driveID, nodeID string, ev collab.Event, err error) {
	if !collab.ValidRSVP(status) {
		return "", "", collab.Event{}, fmt.Errorf("bad rsvp")
	}
	if inv.Method != ics.MethodRequest {
		return "", "", collab.Event{}, fmt.Errorf("not an invitation")
	}
	driveID, node, err := s.PrimaryPersonalCalendar(ctx, user, quota)
	if err != nil {
		return "", "", collab.Event{}, err
	}
	doc, err := s.Collab.LoadCalDoc(ctx, driveID, node.ID, node)
	if err != nil {
		return "", "", collab.Event{}, err
	}
	id := icsEventID(inv.UID)
	e, exists := doc.Events[id]
	if !exists {
		title := strings.TrimSpace(inv.Summary)
		if title == "" {
			title = "(untitled invitation)"
		}
		if len(title) > 200 {
			title = title[:200]
		}
		end := inv.End
		if !end.After(inv.Start) {
			end = inv.Start.Add(time.Hour)
		}
		notes := inv.Description
		if len(notes) > 4000 {
			notes = notes[:4000]
		}
		loc := inv.Location
		if len(loc) > 300 {
			loc = loc[:300]
		}
		e = collab.Event{
			ID: id, Title: title, Start: inv.Start.UTC(), End: end.UTC(),
			AllDay: inv.AllDay, Location: loc, Notes: notes,
			By: user.Username, At: time.Now().UTC(),
		}
	}
	if e.Invites == nil {
		e.Invites = map[string]string{}
	}
	e.Invites[user.Username] = status
	if err := s.Collab.SaveCalEvent(ctx, driveID, node.ID, e, user.Username); err != nil {
		return "", "", collab.Event{}, err
	}
	rec := InboundRSVP{Status: status, At: time.Now().UTC(), DriveID: driveID, NodeID: node.ID, EventID: id}
	if err := kvx.SetJSON(ctx, s.DB, icsRSVPPrefix+user.Username+"/"+uidHash(inv.UID), rec); err != nil {
		s.warn("inbound rsvp ledger write failed", "user", user.Username, "err", err)
	}
	return driveID, node.ID, e, nil
}

// partStatFor maps an RSVP answer to its iTIP participation status.
func partStatFor(status string) string {
	switch status {
	case collab.RSVPYes:
		return ics.PartAccepted
	case collab.RSVPNo:
		return ics.PartDeclined
	default:
		return ics.PartTentative
	}
}

// ReplySubjectPrefix renders the conventional reply-subject prefix.
func ReplySubjectPrefix(status string) string {
	switch status {
	case collab.RSVPYes:
		return "Accepted"
	case collab.RSVPNo:
		return "Declined"
	default:
		return "Tentative"
	}
}

// BuildReplyICS renders the METHOD:REPLY the organizer gets back.
func BuildReplyICS(inv ics.Event, replierAddr, status string) []byte {
	return ics.Encode(ics.Event{
		Method: ics.MethodReply, UID: inv.UID, Sequence: inv.Sequence,
		Summary: inv.Summary, Start: inv.Start, End: inv.End, AllDay: inv.AllDay,
		Organizer: inv.Organizer,
		Attendees: []ics.Attendee{{Email: replierAddr, PartStat: partStatFor(status)}},
	})
}
