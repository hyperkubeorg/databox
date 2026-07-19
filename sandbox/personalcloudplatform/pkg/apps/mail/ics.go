// ics.go — calendar invites in the reading pane (spec §7.6). An
// inbound text/calendar part (mailrender parses it into Body.ICS)
// renders as an invite card between the message body and the
// attachment chips: title, when, where, and — for a METHOD:REQUEST —
// Accept / Maybe / Decline. Answering writes the event into the
// member's primary personal calendar (their own status recorded) and
// emails the METHOD:REPLY back to the organizer through the normal
// send path. The current answer shows on the card on every later read.
package mail

import (
	"net/http"
	"strings"
	"time"

	dcal "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/calendar"
	dmail "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ics"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailrender"
)

// ICSVM is the invite card's view model.
type ICSVM struct {
	UID       string `json:"uid"`
	Method    string `json:"method"`
	Title     string `json:"title"`
	When      string `json:"when"`
	Where     string `json:"where,omitempty"`
	Organizer string `json:"organizer,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	CanRSVP   bool   `json:"canRsvp,omitempty"`
	MyStatus  string `json:"myStatus,omitempty"` // yes | no | maybe
}

// icsWhen renders the card's time line.
func icsWhen(start, end time.Time, allDay bool) string {
	if allDay {
		s := start.Local().Format("Mon, Jan 2, 2006")
		if end.Sub(start) > 24*time.Hour {
			return s + " – " + end.Add(-time.Second).Local().Format("Mon, Jan 2, 2006") + " (all day)"
		}
		return s + " (all day)"
	}
	s := start.Local()
	e := end.Local()
	if s.Year() == e.Year() && s.YearDay() == e.YearDay() {
		return s.Format("Mon, Jan 2, 2006 3:04 PM") + " – " + e.Format("3:04 PM")
	}
	return s.Format("Mon, Jan 2, 2006 3:04 PM") + " – " + e.Format("Mon, Jan 2, 2006 3:04 PM")
}

// icsVM builds the card off a parsed part, folding in the member's
// remembered answer.
func (h *handlers) icsVM(r *http.Request, user users.User, p *mailrender.ICS) *ICSVM {
	if p == nil {
		return nil
	}
	title := p.Summary
	if title == "" {
		title = "(untitled event)"
	}
	// Calendar is optional for Mail: with it off (or absent), the invite
	// card still shows what/when/where, but the RSVP controls disappear —
	// answering would need k.Calendar, and the RSVP route 404s anyway.
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	calOn := h.k.Calendar != nil && h.k.FeatureEnabled(cctx, "calendar")
	vm := &ICSVM{
		UID: p.UID, Method: p.Method, Title: title,
		When: icsWhen(p.Start, p.End, p.AllDay), Where: p.Location,
		Organizer: p.Organizer, Cancelled: p.Cancelled,
		CanRSVP: calOn && p.Method == ics.MethodRequest && !p.Cancelled,
	}
	if calOn {
		if rec, found := h.k.Calendar.InboundRSVPStatus(cctx, user.Username, p.UID); found {
			vm.MyStatus = rec.Status
		}
	}
	return vm
}

// icsRSVP answers an inbound invite: POST /mail/do/icsrsvp
// (msg=<msgID>, status=yes|no|maybe). The event lands in the member's
// primary personal calendar and the METHOD:REPLY mails back to the
// organizer through sendFrom (undo-send hold and rate limits apply,
// like any send).
func (h *handlers) icsRSVP(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		// The route is Calendar-gated, but guard the wiring too: a nil
		// Calendar (app mounted without one) must 404, never panic.
		gctx, gcancel := kernel.Ctx(r)
		off := h.k.Calendar == nil || !h.k.FeatureEnabled(gctx, "calendar")
		gcancel()
		if off {
			return nil, dmail.ErrNotFound
		}
		status := r.FormValue("status")
		raw, _, ok := h.messageRaw(r, user, r.FormValue("msg"))
		if !ok {
			return nil, dmail.ErrNotFound
		}
		body := mailrender.Render(raw)
		if body.ICS == nil || body.ICS.Method != ics.MethodRequest {
			return nil, dmail.ErrNotFound
		}
		inv := ics.Event{
			Method: body.ICS.Method, UID: body.ICS.UID, Summary: body.ICS.Summary,
			Description: "", Location: body.ICS.Location,
			Start: body.ICS.Start, End: body.ICS.End, AllDay: body.ICS.AllDay,
			Organizer: body.ICS.Organizer, Sequence: body.ICS.Sequence,
		}
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, _, _, err := h.k.Calendar.ApplyInboundRSVP(cctx, user, inv, status, h.quotaFor(r, user))
		if err != nil {
			return nil, err
		}
		// The reply ICS to the organizer, through the normal send path.
		if inv.Organizer != "" && !strings.EqualFold(inv.Organizer, box.Addr) {
			sc := h.siteConfig(r)
			att, err := h.k.Mail.StageAttachment(cctx, user.Username, "reply.ics",
				"text/calendar; method=REPLY; charset=utf-8",
				strings.NewReader(string(dcal.BuildReplyICS(inv, box.Addr, status))), sc.Mail.MsgBytes())
			if err != nil {
				h.k.Log.Warn("ics reply staging failed", "err", err)
			} else {
				subject := dcal.ReplySubjectPrefix(status) + ": " + body.ICS.Summary
				if _, err := h.sendFrom(r, user, box, dmail.Draft{
					BoxID: box.ID, From: box.Addr, To: []string{inv.Organizer},
					Subject: subject,
					Text:    user.DisplayName + " has answered “" + body.ICS.Summary + "”: " + status + ".\n",
					Atts:    []dmail.DraftAtt{att},
				}); err != nil {
					h.k.Log.Warn("ics reply send failed", "to", inv.Organizer, "err", err)
				}
			}
		}
		return map[string]any{"status": status}, nil
	})
}
