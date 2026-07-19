// notify.go — server-side notification fan-out for calendar events,
// with the PCD dedup-ledger semantics: exactly one notification per
// (document, event, suffix). The suffix encodes the recipient (and,
// for RSVP changes, the answer), so a re-saved event never re-notifies
// but a changed answer does. The ledger lives INSIDE the document's
// /pcp/docs/ key space, so a purged document takes it along.
package calendar

import (
	"context"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// notifSentKey is the dedup ledger row for one (event, suffix).
func notifSentKey(driveID, nodeID, eventID, suffix string) string {
	return "/pcp/docs/" + driveID + "/" + nodeID + "/notifsent/" + eventID + "/" + suffix
}

// NotifyOnce sends a notification exactly once per (document, event,
// suffix). Best-effort callers log and move on.
func (s *Store) NotifyOnce(ctx context.Context, driveID, nodeID, eventID, suffix, recipient string, n notify.Notification) error {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) || !collab.ValidEventID(eventID) {
		return nil
	}
	key := notifSentKey(driveID, nodeID, eventID, suffix)
	if _, found, err := s.DB.Get(ctx, key); err != nil || found {
		return err
	}
	if _, err := s.DB.Set(ctx, key, []byte("{}")); err != nil {
		return err
	}
	return s.Notify.Notify(ctx, recipient, n)
}

// eventWhen renders the notification's time fragment.
func eventWhen(e collab.Event) string {
	if e.AllDay {
		return e.Start.Format("Jan 2")
	}
	return e.Start.Local().Format("Jan 2, 3:04 PM")
}

// NotifyEventPeople fans invite/tag notifications out to INTERNAL
// members, once per (event, member) — external email invitees get ICS
// mail instead (icsmail.go). Soft-fail: a lost notification never
// fails the save.
func (s *Store) NotifyEventPeople(ctx context.Context, actor string, driveID, nodeID string, node nodes.Node, e collab.Event) {
	calName := strings.TrimSuffix(node.Name, collab.CalExt)
	url := "/calendar?drive=" + driveID + "&node=" + nodeID + "&event=" + e.ID
	when := eventWhen(e)
	for invitee := range e.Invites {
		if invitee == actor || collab.ExternalInvitee(invitee) {
			continue
		}
		err := s.NotifyOnce(ctx, driveID, nodeID, e.ID, "inv-"+invitee, invitee, notify.Notification{
			Kind: notify.KindInvite, From: actor, URL: url,
			Text: "@" + actor + " invited you: “" + e.Title + "” (" + when + ", " + calName + ")",
		})
		if err != nil {
			s.warn("invite notification failed", "to", invitee, "err", err)
		}
	}
	for _, tagged := range e.Tags {
		if tagged == actor {
			continue
		}
		if _, alsoInvited := e.Invites[tagged]; alsoInvited {
			continue // the invite already says it
		}
		err := s.NotifyOnce(ctx, driveID, nodeID, e.ID, "tag-"+tagged, tagged, notify.Notification{
			Kind: notify.KindTag, From: actor, URL: url,
			Text: "@" + actor + " tagged you on “" + e.Title + "” (" + when + ", " + calName + ")",
		})
		if err != nil {
			s.warn("tag notification failed", "to", tagged, "err", err)
		}
	}
}

// NotifyRSVP tells the event's creator about an answer. Answer changes
// re-notify: the dedup suffix carries the status.
func (s *Store) NotifyRSVP(ctx context.Context, driveID, nodeID string, e collab.Event, by, status string) {
	if e.By == "" || e.By == by || users.ValidUsername(e.By) != nil {
		return
	}
	err := s.NotifyOnce(ctx, driveID, nodeID, e.ID, "rsvp-"+by+"-"+status, e.By, notify.Notification{
		Kind: notify.KindRSVP, From: by, At: time.Now().UTC(),
		URL:  "/calendar?drive=" + driveID + "&node=" + nodeID + "&event=" + e.ID,
		Text: "@" + by + " answered “" + e.Title + "”: " + status,
	})
	if err != nil {
		s.warn("rsvp notification failed", "to", e.By, "err", err)
	}
}
