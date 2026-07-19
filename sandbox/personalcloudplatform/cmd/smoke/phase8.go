// phase8.go — the admin console + observability live smoke (spec §11).
// Against the ALREADY-RUNNING full stack (databox + pcp + paired
// postoffice + paired cloudferry from the earlier phases):
//
//  1. admin home renders healthy after a forced health sweep,
//  2. kill the cloudferry → the health worker raises the plain-language
//     'gateway unreachable' problem → admin notification + launcher
//     badge; restart → auto-resolve into a tombstone,
//  3. restart the postoffice → the DKIM re-push flow heals and the PO
//     detail page shows "in memory ✓" again (plus sample history),
//  4. the view-only Databox page renders LIVE cluster metadata,
//  5. invites: flip the site to invite mode → mint → a second browser
//     signs up with the code (and is refused without one) → the
//     redemption ledger shows the new member,
//  6. impersonation: banner on every page + start/stop audit entries,
//  7. tier create + assign → the member's effective quota reflects it,
//  8. audit CSV export streams the filtered log,
//  9. the mail-domain wizard's DNS check runs live against a domain
//     that can't verify (or shows its degraded notice when the sandbox
//     blocks DNS — asserted either way, and reported which),
//  10. an API key listed on the user detail page is revoked by the
//     admin and 401s immediately.
package main

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// refreshCSRF re-fishes the CSRF meta after a session swap
// (impersonation start/stop mints a fresh session + token).
func (w *web) refreshCSRF() error {
	_, page, err := w.get("/")
	if err != nil {
		return err
	}
	m := regexp.MustCompile(`name="csrf" content="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		return fmt.Errorf("no csrf meta")
	}
	w.csrf = m[1]
	return nil
}

func phase8(ctx context.Context, pcpURL string, db *client.Client, userStore *users.Store,
	keyStore *apikeys.Store, mailDom, poID string, killFerry, restartFerry, killPO, restartPO func()) {
	systemStore := &system.Store{DB: db}
	siteStore := &site.Store{DB: db}

	w := newWeb(pcpURL)
	if err := w.login("ada", "password123"); err != nil {
		fail("phase8: admin login", "err", err)
		return
	}

	// recheck forces a health sweep (the console's own button).
	recheck := func() { _, _, _ = w.post("/admin/system/problems/check", url.Values{}) }
	openProblems := func() map[string]system.Problem {
		open, _ := systemStore.OpenProblems(ctx)
		out := map[string]system.Problem{}
		for _, p := range open {
			out[p.ID] = p
		}
		return out
	}

	// --- 1. healthy home ---------------------------------------------------------
	// Phase 7 hard-killed a pcp replica on purpose; its heartbeat record
	// would legitimately read as a crash. Clear the slate — the live
	// replica re-beats within 30s.
	must(kvx.DeletePrefix(ctx, db, "/pcp/system/replicas/"), "phase8: reset replicas")
	recheck()
	healthy := until(30*time.Second, func() bool {
		recheck()
		return len(openProblems()) == 0
	})
	if healthy {
		code, body, err := w.get("/admin")
		if err == nil && code == 200 && strings.Contains(body, "every check is passing") &&
			strings.Contains(body, "Areas") {
			pass("phase8: admin home shows healthy (problems empty, traffic lights rendered)")
		} else {
			fail("phase8: admin home render", "code", code, "err", err)
		}
	} else {
		fail("phase8: stack not healthy at start", "open", fmt.Sprintf("%+v", openProblems()))
	}

	// --- 2. kill cloudferry → problem + notification + badge → auto-resolve -------
	if killFerry == nil || restartFerry == nil {
		fail("phase8: no ferry controls from phase 7")
		return
	}
	adminNotifsBefore := 0
	{
		rows, _ := (&notifList{db: db}).list(ctx, "ada")
		adminNotifsBefore = len(rows)
	}
	killFerry()
	var cfProblem system.Problem
	raised := until(4*time.Minute, func() bool {
		recheck()
		for id, p := range openProblems() {
			if strings.HasPrefix(id, "cf-unreachable.") {
				cfProblem = p
				return true
			}
		}
		return false
	})
	if raised && cfProblem.Severity == system.SevCritical &&
		strings.Contains(cfProblem.Summary, "offline page") && cfProblem.Action != "" {
		pass("phase8: dead cloudferry → critical problem with plain-language summary + action")
	} else {
		fail("phase8: unreachable problem missing/wrong", "problem", fmt.Sprintf("%+v", cfProblem))
	}
	// The admin was notified through the normal channel...
	rows, _ := (&notifList{db: db}).list(ctx, "ada")
	gotNotif := false
	for _, r := range rows[:max(0, len(rows)-adminNotifsBefore)] {
		if strings.Contains(r, "Problem (critical)") {
			gotNotif = true
		}
	}
	if gotNotif {
		pass("phase8: admin notification arrived for the raised problem")
	} else {
		fail("phase8: no admin notification", "rows", fmt.Sprintf("%v", rows))
	}
	// ...and the launcher Admin card badges the open count.
	if _, body, err := w.get("/"); err == nil && strings.Contains(body, `<span class="badge">`) {
		pass("phase8: launcher Admin card shows the problem badge")
	} else {
		fail("phase8: launcher badge missing")
	}
	// Home lists the problem first.
	if _, body, _ := w.get("/admin"); strings.Contains(body, "offline page") && strings.Contains(body, "critical") {
		pass("phase8: admin home lists the open problem with severity chip")
	} else {
		fail("phase8: admin home problem row missing")
	}

	restartFerry()
	resolved := until(3*time.Minute, func() bool {
		recheck()
		_, stillOpen := openProblems()[cfProblem.ID]
		return !stillOpen
	})
	if resolved {
		all, _ := systemStore.Problems(ctx)
		tomb := false
		for _, p := range all {
			if p.ID == cfProblem.ID && p.Resolved() {
				tomb = true
			}
		}
		if tomb {
			pass("phase8: ferry restart → problem auto-resolved (kept as a tombstone)")
		} else {
			fail("phase8: resolved problem left no tombstone")
		}
	} else {
		fail("phase8: problem never auto-resolved after restart")
	}

	// --- 3. postoffice restart → DKIM re-push visible on the PO detail page -------
	killPO()
	restartPO()
	poHealed := until(2*time.Minute, func() bool {
		_, body, err := w.get("/admin/mail/postoffices/" + poID)
		return err == nil && strings.Contains(body, "in memory ✓")
	})
	if poHealed {
		_, body, _ := w.get("/admin/mail/postoffices/" + poID)
		if strings.Contains(body, "<svg") && strings.Contains(body, "Re-push config") {
			pass("phase8: PO restart → DKIM re-pushed; detail page shows key freshness + sparklines + re-push action")
		} else {
			pass("phase8: PO restart → DKIM re-pushed; detail page shows key freshness (history still filling)")
		}
	} else {
		fail("phase8: PO detail never showed DKIM back in memory")
	}

	// --- 4. the view-only Databox panel renders live cluster metadata --------------
	if _, body, err := w.get("/admin/system/databox"); err == nil &&
		strings.Contains(body, "Raft groups") && strings.Contains(body, "Shards") &&
		strings.Contains(body, "1 node(s)") && strings.Contains(body, "databox cluster") {
		pass("phase8: Databox page renders live cluster data (1 node, groups + leader, CLI commands named)")
	} else {
		_, body, _ := w.get("/admin/system/databox")
		snippet := body
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		fail("phase8: databox panel", "body", snippet)
	}
	// The Workers page shows gateways + loops + replicas.
	if _, body, err := w.get("/admin/system/workers"); err == nil &&
		strings.Contains(body, "mailsync") && strings.Contains(body, "health") &&
		strings.Contains(body, "beating") {
		pass("phase8: Workers page shows loops + replica heartbeats")
	} else {
		fail("phase8: workers page incomplete")
	}

	// --- 5. invites end to end ------------------------------------------------------
	// messenger_enabled rides along: the checkbox stores the inverse
	// (absent = disable), and phase 9 needs Messenger alive.
	if code, body, err := w.post("/admin/site/config", url.Values{
		"name": {"Smoke Cloud"}, "signup_mode": {"invite"}, "messenger_enabled": {"on"},
	}); err != nil || code != 200 || jsonMap(body)["ok"] != true {
		fail("phase8: switch signup mode", "code", code, "body", body)
	}
	// Without a code, signup refuses.
	fresh := newWeb(pcpURL)
	resp, err := fresh.c.PostForm(pcpURL+"/signup", url.Values{
		"username": {"gatecrasher"}, "password": {"password123"},
	})
	if err == nil {
		resp.Body.Close()
	}
	if _, found, _ := userStore.Get(ctx, "gatecrasher"); found {
		fail("phase8: invite mode admitted a signup without a code")
	} else {
		pass("phase8: invite mode refuses signup without a code")
	}
	// Mint through the console.
	code, body, err := w.post("/admin/invites/create", url.Values{
		"kind": {"quantity"}, "max_uses": {"1"}, "description": {"smoke invite"},
	})
	if err != nil || code != 200 || jsonMap(body)["ok"] != true {
		fail("phase8: invite mint", "code", code, "body", body)
		return
	}
	inviteCode, _ := jsonMap(body)["code"].(string)
	if inviteCode == "" {
		fail("phase8: mint returned no code", "body", body)
		return
	}
	// Second browser redeems it.
	resp, err = fresh.c.PostForm(pcpURL+"/signup", url.Values{
		"username": {"smokey"}, "password": {"password123"}, "invite": {inviteCode},
	})
	if err == nil {
		resp.Body.Close()
	}
	newUser, found, _ := userStore.Get(ctx, "smokey")
	if found && newUser.InvitedBy == "ada" && newUser.InviteCode == inviteCode {
		pass("phase8: second-browser signup redeemed the invite atomically (InvitedBy stamped)")
	} else {
		fail("phase8: invited signup failed", "user", fmt.Sprintf("%+v", newUser))
		return
	}
	// The console ledger shows the redemption.
	if _, body, _ := w.get("/admin/invites"); strings.Contains(body, "@smokey") && strings.Contains(body, "smoke invite") {
		pass("phase8: admin invites page shows the redemption ledger row")
	} else {
		fail("phase8: ledger row missing on /admin/invites")
	}

	// --- 6. impersonation: banner + audit ------------------------------------------
	if _, _, err := w.post("/admin/users/impersonate", url.Values{"user": {"smokey"}}); err != nil {
		fail("phase8: impersonate post", "err", err)
	}
	if _, body, err := w.get("/"); err == nil && strings.Contains(body, "viewing as @smokey") {
		pass("phase8: impersonation banner renders on every page")
	} else {
		fail("phase8: impersonation banner missing")
	}
	if err := w.refreshCSRF(); err != nil { // new session, new token
		fail("phase8: csrf after impersonation", "err", err)
	}
	if _, _, err := w.post("/impersonate/stop", url.Values{}); err != nil {
		fail("phase8: impersonate stop", "err", err)
	}
	if err := w.refreshCSRF(); err != nil {
		fail("phase8: csrf after stop", "err", err)
	}
	if _, body, err := w.get("/admin/audit?action=impersonate"); err == nil &&
		strings.Contains(body, "impersonate.start") && strings.Contains(body, "impersonate.stop") &&
		strings.Contains(body, "as @smokey") {
		pass("phase8: impersonation start/stop audited with both identities")
	} else {
		fail("phase8: impersonation audit entries missing")
	}

	// --- 7. tiers affect quota -------------------------------------------------------
	if code, body, _ := w.post("/admin/tiers/set", url.Values{"name": {"tiny"}, "bytes": {"12345"}}); code != 200 || jsonMap(body)["ok"] != true {
		fail("phase8: tier create", "body", body)
	}
	if code, body, _ := w.post("/admin/users/tier", url.Values{"user": {"smokey"}, "tier": {"tiny"}}); code != 200 || jsonMap(body)["ok"] != true {
		fail("phase8: tier assign", "body", body)
	}
	sc, _ := siteStore.Get(ctx)
	smokey, _, _ := userStore.Get(ctx, "smokey")
	if q := site.QuotaFor(sc, smokey.QuotaOverride, smokey.Tier, 10<<30); q == 12345 {
		pass("phase8: tier create + assign → effective quota reflects (12345 bytes)")
	} else {
		fail("phase8: tier quota", "got", fmt.Sprint(q))
	}

	// --- 8. audit CSV export ----------------------------------------------------------
	if _, body, err := w.get("/admin/audit.csv?action=tier."); err == nil &&
		strings.HasPrefix(body, "at,actor,") && strings.Contains(body, "tier.set") {
		pass("phase8: audit CSV export streams the filtered log")
	} else {
		fail("phase8: audit csv export")
	}

	// --- 9. the domain wizard's DNS check against an unverifiable domain ---------------
	if _, body, err := w.get("/admin/mail/domains/" + mailDom + "?check=1"); err == nil {
		switch {
		case strings.Contains(body, "couldn&#39;t run from this server") || strings.Contains(body, "unchecked from here"):
			pass("phase8: DNS wizard ran → resolver blocked here, degraded notice shown (honest, not red)")
		case strings.Contains(body, "missing") || strings.Contains(body, "differs"):
			pass("phase8: DNS wizard ran live → " + mailDom + " shows unverified records plainly")
		default:
			fail("phase8: DNS check rendered neither results nor the degraded notice")
		}
	} else {
		fail("phase8: domain wizard fetch", "err", err)
	}

	// --- 10. API key on user detail + admin revoke → 401 --------------------------------
	token, key, err := keyStore.Mint(ctx, "ada", "smoke-admin-key", []string{"profile:read"}, time.Time{})
	must(err, "phase8: mint key")
	if status, _ := bearer(pcpURL, token, "GET", "/api/v1/profile", "", ""); status != 200 {
		fail("phase8: fresh key should 200", "status", status)
	}
	if _, body, _ := w.get("/admin/users/ada"); strings.Contains(body, "smoke-admin-key") && strings.Contains(body, "profile:read") {
		pass("phase8: user detail lists the API key with scopes (§12.1)")
	} else {
		fail("phase8: key missing from user detail")
	}
	if code, body, _ := w.post("/admin/users/apikey", url.Values{"user": {"ada"}, "key_id": {key.KeyID}}); code != 200 || jsonMap(body)["ok"] != true {
		fail("phase8: admin revoke", "body", body)
	}
	if status, _ := bearer(pcpURL, token, "GET", "/api/v1/profile", "", ""); status == 401 {
		pass("phase8: admin-revoked key 401s immediately")
	} else {
		fail("phase8: revoked key still works")
	}
}

// notifList reads a member's notification texts without importing the
// notify package's paging into the assertions.
type notifList struct{ db *client.Client }

func (n *notifList) list(ctx context.Context, user string) ([]string, error) {
	entries, _, err := n.db.List(ctx, "/pcp/notif/"+user+"/", "", 100)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, string(e.Value))
	}
	return out, nil
}
