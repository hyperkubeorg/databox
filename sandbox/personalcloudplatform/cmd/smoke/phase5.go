// phase5.go — the phase-5 live smoke: Calendar, Contacts, and the ICS
// mail round-trip against the REAL pcp binary + paired postoffice, on
// top of the phase-3/4 harness:
//
//   - shared drive + shared .pccal + personal .pccal; events through the
//     collab ops endpoint (X-CSRF); the /calendar month SSR shows both,
//   - a NON-MEMBER invitee RSVPs (the invite is the entire auth), the
//     creator is notified, the answer aggregates; a stranger gets 404,
//   - calsub hide flips the calendar's filter state,
//   - an EXTERNAL email invitee produces an outbound message whose raw
//     carries a text/calendar METHOD:REQUEST part, released to the
//     gateway,
//   - an inbound METHOD:REQUEST over SMTP renders an invite card in the
//     thread JSON; Accept writes the event into the primary personal
//     calendar AND queues a METHOD:REPLY to the organizer,
//   - a contact card ranks FIRST in /mail/api/suggest,
//   - API keys: calendar:read lists events, write denied on the read
//     key, contacts CRUD on a contacts key,
//   - the launcher card shows today's next event.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dcalendar "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/calendar"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailrender"
)

// postJSON posts a JSON body with the session's CSRF header (the collab
// ops shape).
func (w *web) postJSON(path, body string) (int, string, error) {
	req, err := http.NewRequest("POST", w.base+path, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF", w.csrf)
	req.Header.Set("X-Requested-With", "fetch")
	resp, err := w.c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

// phase5 runs the Calendar/Contacts/ICS smoke.
func phase5(ctx context.Context, pcpURL, poSMTP, mailDom string, db *client.Client,
	mailStore *mail.Store, userStore *users.Store, keyStore *apikeys.Store, box mail.Mailbox) {

	driveStore := &drives.Store{DB: db, Users: userStore}
	nodeStore := &nodes.Store{DB: db, Users: userStore}
	collabStore := &collab.Store{DB: db, Nodes: nodeStore}
	notifyStore := &notify.Store{DB: db}
	calStore := &dcalendar.Store{
		DB: db, Users: userStore, Drives: driveStore, Nodes: nodeStore,
		Collab: collabStore, Notify: notifyStore, Mail: mailStore,
	}

	// --- users + sessions ---------------------------------------------------------
	for _, u := range []string{"bob", "carol", "dave"} {
		if _, err := userStore.CreateUser(ctx, u, strings.ToUpper(u[:1])+u[1:], "password123"); err != nil {
			fail("phase5: create "+u, "err", err)
			return
		}
	}
	ada := newWeb(pcpURL)
	if err := ada.login("ada", "password123"); err != nil {
		fail("phase5: ada login", "err", err)
		return
	}
	// /drive self-heals ada's personal drive (the account predates the
	// signup hook in this harness).
	_, _, _ = ada.get("/drive")
	adaUser, _, _ := userStore.Get(ctx, "ada")
	if adaUser.PersonalDrive == "" {
		fail("phase5: ada has no personal drive after /drive")
		return
	}

	// --- calendars: personal + shared drive ---------------------------------------
	shared, err := driveStore.CreateShared(ctx, "ada", "Ops")
	must(err, "phase5 shared drive")
	must(driveStore.SetMember(ctx, shared.ID, "bob", drives.RoleEditor), "phase5 add bob")

	newCal := func(driveID, name string) (string, string) {
		form := url.Values{"name": {name}}
		if driveID != "" {
			form.Set("drive", driveID)
		}
		_, body, _ := ada.post("/calendar/do/new", form)
		out := jsonMap(body)
		if out["ok"] != true {
			fail("phase5: new calendar "+name, "body", body)
			return "", ""
		}
		return out["drive"].(string), out["node"].(string)
	}
	pDrive, pNode := newCal("", "Home")
	sDrive, sNode := newCal(shared.ID, "Team")
	if pNode == "" || sNode == "" {
		return
	}
	pass("calendar create: personal Home.pccal + shared Team.pccal")

	// --- events through the ops endpoint -------------------------------------------
	now := time.Now()
	hlc := func(user string) string {
		return fmt.Sprintf("%013d-%06d-%s", time.Now().UnixMilli(), 111, user)
	}
	eventJSON := func(id, title string, start time.Time, invites string) string {
		return fmt.Sprintf(`{"id":%q,"title":%q,"start":%q,"end":%q,"invites":{%s},"by":"ada","at":%q}`,
			id, title, start.UTC().Format(time.RFC3339), start.Add(time.Hour).UTC().Format(time.RFC3339),
			invites, now.UTC().Format(time.RFC3339))
	}
	sendOp := func(w *web, driveID, nodeID, op string) (int, string) {
		code, body, err := w.postJSON("/calendar/cal/"+driveID+"/"+nodeID+"/ops", `{"ops":[`+op+`]}`)
		if err != nil {
			return 0, err.Error()
		}
		return code, body
	}
	// Personal: today's next event (the launcher card reads this).
	persStart := now.Add(30 * time.Minute)
	if code, body := sendOp(ada, pDrive, pNode, fmt.Sprintf(`{"t":"e:evtpersonal1","v":%s,"hlc":%q}`,
		eventJSON("evtpersonal1", "Standup", persStart, ""), hlc("ada"))); code != 200 {
		fail("phase5: personal event op", "code", code, "body", body)
		return
	}
	// Shared: carol (NOT a member) is invited.
	sharedStart := now.Add(2 * time.Hour)
	if code, body := sendOp(ada, sDrive, sNode, fmt.Sprintf(`{"t":"e:evtshared001","v":%s,"hlc":%q}`,
		eventJSON("evtshared001", "Launch review", sharedStart, `"carol":"invited"`), hlc("ada"))); code != 200 {
		fail("phase5: shared event op", "code", code, "body", body)
		return
	}
	pass("event ops: X-CSRF batches accepted on both calendars")

	// --- the month SSR shows both -----------------------------------------------------
	_, page, _ := ada.get("/calendar")
	if strings.Contains(page, "Standup") && strings.Contains(page, "Launch review") &&
		strings.Contains(page, `data-open="`+sDrive+"/"+sNode+"/evtshared001") {
		pass("month SSR: both events render server-side with chip targets")
	} else {
		fail("month SSR missing events")
	}

	// --- carol was notified; she RSVPs with ZERO drive access --------------------------
	if until(10*time.Second, func() bool {
		rows, _ := notifyStore.List(ctx, "carol", 10)
		for _, r := range rows {
			if r.Kind == notify.KindInvite && strings.Contains(r.Text, "Launch review") {
				return true
			}
		}
		return false
	}) {
		pass("invite notification reached carol")
	} else {
		fail("carol never notified")
	}
	carol := newWeb(pcpURL)
	if err := carol.login("carol", "password123"); err != nil {
		fail("phase5: carol login", "err", err)
		return
	}
	_, body, _ := carol.post("/calendar/cal/"+sDrive+"/"+sNode+"/rsvp",
		url.Values{"event": {"evtshared001"}, "status": {"yes"}})
	if jsonMap(body)["ok"] == true {
		pass("RSVP: non-member invitee answered (the invite IS the auth)")
	} else {
		fail("carol rsvp refused", "body", body)
	}
	dave := newWeb(pcpURL)
	if err := dave.login("dave", "password123"); err == nil {
		code, _, _ := dave.post("/calendar/cal/"+sDrive+"/"+sNode+"/rsvp",
			url.Values{"event": {"evtshared001"}, "status": {"yes"}})
		if code == 404 {
			pass("RSVP: stranger gets 404 and learns nothing")
		} else {
			fail("stranger rsvp status", "code", code)
		}
	}
	// The answer aggregates + the creator hears about it.
	if until(10*time.Second, func() bool {
		node, found, _ := nodeStore.GetByID(ctx, sDrive, sNode)
		if !found {
			return false
		}
		doc, err := collabStore.LoadCalDoc(ctx, sDrive, sNode, node)
		return err == nil && doc.Events["evtshared001"].Invites["carol"] == "yes"
	}) {
		pass("RSVP state folded into the shared calendar")
	} else {
		fail("rsvp never folded")
	}
	if until(10*time.Second, func() bool {
		rows, _ := notifyStore.List(ctx, "ada", 10)
		for _, r := range rows {
			if r.Kind == notify.KindRSVP && strings.Contains(r.Text, "carol") {
				return true
			}
		}
		return false
	}) {
		pass("RSVP notification reached the creator")
	} else {
		fail("ada never heard carol's answer")
	}

	// --- bob hides the shared calendar via calsub ---------------------------------------
	bob := newWeb(pcpURL)
	if err := bob.login("bob", "password123"); err != nil {
		fail("phase5: bob login", "err", err)
		return
	}
	_, body, _ = bob.post("/calendar/calsub", url.Values{"drive": {sDrive}, "node": {sNode}, "hidden": {"0"}})
	if jsonMap(body)["ok"] != true {
		fail("bob subscribe", "body", body)
	}
	_, body, _ = bob.post("/calendar/calsub", url.Values{"drive": {sDrive}, "node": {sNode}, "hidden": {"1"}})
	if jsonMap(body)["ok"] != true {
		fail("bob hide", "body", body)
	}
	_, body, _ = bob.get("/calendar/api/list")
	hidden := false
	for _, c := range jsonMap(body)["calendars"].([]any) {
		cal := c.(map[string]any)
		if cal["node"] == sNode && cal["hidden"] == true && cal["subscribed"] == true {
			hidden = true
		}
	}
	if hidden {
		pass("calsub: hide override layered over the subscription")
	} else {
		fail("calsub hide not visible", "body", body)
	}

	// --- external invitee → outbound ICS mail --------------------------------------------
	const extInvitee = "friend@pcp-smoke-nonexistent.invalid"
	if code, body := sendOp(ada, sDrive, sNode, fmt.Sprintf(`{"t":"e:evtshared001","v":%s,"hlc":%q}`,
		eventJSON("evtshared001", "Launch review", sharedStart, `"carol":"yes","`+extInvitee+`":"invited"`), hlc("ada"))); code != 200 {
		fail("phase5: external invite op", "code", code, "body", body)
	}
	var inviteRaw []byte
	if found := until(30*time.Second, func() bool {
		_ = mailStore.ScanOutbound(ctx, func(_ string, om mail.OutMsg) error {
			if len(om.RcptTo) == 1 && om.RcptTo[0] == extInvitee && strings.Contains(om.MailFrom, "ada@") {
				if raw, err := mailStore.MessageBlob(ctx, om.BlobOf, om.BlobID); err == nil {
					inviteRaw = raw
				}
			}
			return nil
		})
		return inviteRaw != nil
	}); found {
		// The part is base64 in transit — parse the raw like a client would.
		parsed := mailrender.Render(inviteRaw)
		if strings.Contains(string(inviteRaw), "text/calendar") && parsed.ICS != nil &&
			parsed.ICS.Method == "REQUEST" && parsed.ICS.Summary == "Launch review" {
			pass("outbound ICS: queued raw carries a text/calendar METHOD:REQUEST part")
		} else {
			fail("outbound ICS part malformed", "ics", fmt.Sprintf("%+v", parsed.ICS))
		}
	} else {
		fail("outbound ICS invite not observed")
	}

	// --- inbound METHOD:REQUEST over SMTP → invite card → Accept → REPLY out --------------
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\n" +
		"UID:smoke-inv-1@remote.example\r\nSUMMARY:Vendor sync\r\nLOCATION:Meet room\r\n" +
		"DTSTART:20260801T150000Z\r\nDTEND:20260801T160000Z\r\nSEQUENCE:0\r\n" +
		"ORGANIZER:mailto:boss@remote.example\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:ada@" + mailDom + "\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	var inv strings.Builder
	fmt.Fprintf(&inv, "From: boss@remote.example\r\nTo: ada@%s\r\nSubject: Invitation: Vendor sync\r\n", mailDom)
	fmt.Fprintf(&inv, "Date: %s\r\nMessage-ID: <inv1@remote.example>\r\nMIME-Version: 1.0\r\n", time.Now().Format(time.RFC1123Z))
	inv.WriteString("Content-Type: multipart/alternative; boundary=IB\r\n\r\n--IB\r\nContent-Type: text/plain\r\n\r\nPlease come.\r\n")
	inv.WriteString("--IB\r\nContent-Type: text/calendar; method=REQUEST; charset=utf-8\r\n\r\n" + ics + "--IB--\r\n")
	c, err := smtp.Dial(poSMTP)
	must(err, "phase5 smtp dial")
	must(c.Mail("boss@remote.example"), "phase5 MAIL")
	must(c.Rcpt("ada@"+mailDom), "phase5 RCPT")
	wtr, err := c.Data()
	must(err, "phase5 DATA")
	_, _ = wtr.Write([]byte(inv.String()))
	must(wtr.Close(), "phase5 DATA close")
	_ = c.Quit()

	var invThread, invMsg string
	if until(45*time.Second, func() bool {
		_, body, _ := ada.get("/mail/api/threads?box=" + box.ID + "&folder=inbox")
		for _, r := range jsonMap(body)["rows"].([]any) {
			row := r.(map[string]any)
			if strings.Contains(row["subject"].(string), "Vendor sync") {
				invThread = row["threadId"].(string)
				return true
			}
		}
		return false
	}) {
		pass("inbound invite delivered")
	} else {
		fail("inbound invite never arrived")
		return
	}
	_, body, _ = ada.get("/mail/api/thread/" + box.ID + "/" + invThread)
	thread := jsonMap(body)["thread"].(map[string]any)
	msgs := thread["msgs"].([]any)
	icsVM, _ := msgs[len(msgs)-1].(map[string]any)["ics"].(map[string]any)
	if icsVM != nil && icsVM["method"] == "REQUEST" && icsVM["title"] == "Vendor sync" &&
		icsVM["canRsvp"] == true && icsVM["where"] == "Meet room" {
		invMsg = msgs[len(msgs)-1].(map[string]any)["msgId"].(string)
		pass("invite card in thread JSON: title/when/where + RSVP enabled")
	} else {
		fail("invite card missing from thread JSON", "ics", fmt.Sprintf("%v", icsVM))
		return
	}
	// The SSR reading pane carries the card too.
	_, page, _ = ada.get("/mail?box=" + box.ID + "&thread=" + invThread)
	if strings.Contains(page, `class="invite`) && strings.Contains(page, "Vendor sync") && strings.Contains(page, "data-icsrsvp") {
		pass("invite card renders in the SSR reading pane")
	} else {
		fail("SSR invite card missing")
	}
	// Accept.
	_, body, _ = ada.post("/mail/do/icsrsvp", url.Values{"box": {box.ID}, "msg": {invMsg}, "status": {"yes"}})
	if jsonMap(body)["ok"] == true {
		pass("icsrsvp: Accept accepted")
	} else {
		fail("icsrsvp refused", "body", body)
		return
	}
	// The event landed in ada's primary personal calendar…
	adaUser, _, _ = userStore.Get(ctx, "ada")
	if until(10*time.Second, func() bool {
		groups, err := calStore.EventsInRange(ctx, adaUser,
			time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC))
		if err != nil {
			return false
		}
		for _, g := range groups {
			for _, e := range g.Events {
				if e.Title == "Vendor sync" && e.Invites["ada"] == "yes" {
					return true
				}
			}
		}
		return false
	}) {
		pass("accepted invite written into the personal calendar (status recorded)")
	} else {
		fail("event never landed in the personal calendar")
	}
	// …the card shows the current answer…
	_, body, _ = ada.get("/mail/api/thread/" + box.ID + "/" + invThread)
	msgs = jsonMap(body)["thread"].(map[string]any)["msgs"].([]any)
	icsVM, _ = msgs[len(msgs)-1].(map[string]any)["ics"].(map[string]any)
	if icsVM != nil && icsVM["myStatus"] == "yes" {
		pass("invite card shows the remembered answer")
	} else {
		fail("myStatus missing", "ics", fmt.Sprintf("%v", icsVM))
	}
	// …and the METHOD:REPLY queued to the organizer.
	var replyRaw []byte
	if found := until(20*time.Second, func() bool {
		_ = mailStore.ScanOutbound(ctx, func(_ string, om mail.OutMsg) error {
			if len(om.RcptTo) == 1 && om.RcptTo[0] == "boss@remote.example" {
				if raw, err := mailStore.MessageBlob(ctx, om.BlobOf, om.BlobID); err == nil {
					replyRaw = raw
				}
			}
			return nil
		})
		return replyRaw != nil
	}); found {
		parsed := mailrender.Render(replyRaw)
		if parsed.ICS != nil && parsed.ICS.Method == "REPLY" && parsed.ICS.UID == "smoke-inv-1@remote.example" {
			pass("METHOD:REPLY queued outbound to the organizer")
		} else {
			fail("ics reply malformed", "ics", fmt.Sprintf("%+v", parsed.ICS))
		}
	} else {
		fail("ics reply not observed")
	}

	// --- contacts + typeahead ranking ------------------------------------------------------
	_, body, _ = ada.post("/contacts/do/new", url.Values{
		"name": {"Grace Hopper"}, "emails": {"grace@remote.example"}, "org": {"Navy"},
	})
	if jsonMap(body)["ok"] == true {
		pass("contact card created (personal Contacts folder)")
	} else {
		fail("contact create", "body", body)
	}
	_, body, _ = ada.get("/mail/api/suggest?q=grace")
	hits, _ := jsonMap(body)["hits"].([]any)
	if len(hits) > 0 && strings.Contains(hits[0].(string), "Grace Hopper <grace@remote.example>") {
		pass("compose typeahead ranks the contact first")
	} else {
		fail("suggest ranking", "hits", fmt.Sprintf("%v", hits))
	}

	// --- API keys: calendar read/write split + contacts CRUD --------------------------------
	calReadTok, _, err := keyStore.Mint(ctx, "ada", "cal-read", []string{apikeys.ScopeCalendarRead}, time.Time{})
	must(err, "phase5 mint cal-read")
	from := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	to := now.Add(24 * time.Hour).UTC().Format(time.RFC3339)
	code, body2 := bearer(pcpURL, calReadTok, "GET", "/api/v1/calendar/events?from="+from+"&to="+to, "", "")
	if code == 200 && strings.Contains(body2, "Standup") {
		pass("API calendar:read lists aggregated events")
	} else {
		fail("api events", "code", code, "body", body2)
	}
	code, _ = bearer(pcpURL, calReadTok, "POST", "/api/v1/calendar/events",
		`{"title":"nope","start":"2026-07-08T10:00:00Z","end":"2026-07-08T11:00:00Z"}`, "")
	if code == 403 {
		pass("API write denied on the read-only key (403)")
	} else {
		fail("scope gate", "code", code)
	}
	ctTok, _, err := keyStore.Mint(ctx, "ada", "contacts", []string{apikeys.ScopeContactsRead, apikeys.ScopeContactsWrite}, time.Time{})
	must(err, "phase5 mint contacts")
	code, body2 = bearer(pcpURL, ctTok, "POST", "/api/v1/contacts",
		`{"name":"Annie Easley","emails":["annie@remote.example"]}`, "")
	created := jsonMap(body2)
	if code == 201 && created["name"] == "Annie Easley" {
		pass("API contacts create (201)")
	} else {
		fail("api contact create", "code", code, "body", body2)
	}
	cDrive, _ := created["driveId"].(string)
	cNode, _ := created["nodeId"].(string)
	code, body2 = bearer(pcpURL, ctTok, "PUT", "/api/v1/contacts/"+cDrive+"/"+cNode,
		`{"name":"Annie J. Easley","emails":["annie@remote.example"]}`, "")
	if code == 200 && jsonMap(body2)["name"] == "Annie J. Easley" {
		pass("API contacts update")
	} else {
		fail("api contact put", "code", code, "body", body2)
	}
	code, body2 = bearer(pcpURL, ctTok, "GET", "/api/v1/contacts", "", "")
	if code == 200 && strings.Contains(body2, "Annie J. Easley") && strings.Contains(body2, "Grace Hopper") {
		pass("API contacts list aggregates web + API cards")
	} else {
		fail("api contact list", "code", code)
	}
	code, body2 = bearer(pcpURL, ctTok, "DELETE", "/api/v1/contacts/"+cDrive+"/"+cNode, "", "")
	if code == 200 && jsonMap(body2)["deleted"] == true {
		pass("API contacts delete")
	} else {
		fail("api contact delete", "code", code, "body", body2)
	}

	// --- the launcher card ---------------------------------------------------------------------
	// The Standup event sits at phase-start+30min; near midnight it rolls
	// onto the NEXT day, where NextToday correctly finds nothing — skip
	// rather than flake for runs started 23:30–00:00.
	_, page, _ = ada.get("/")
	if persStart.Local().Day() != time.Now().Local().Day() {
		pass("launcher next-event check skipped — the event rolled past midnight")
	} else if strings.Contains(page, "Standup in") {
		pass("launcher card shows today's next event")
	} else {
		fail("launcher card missing next event")
	}
}
