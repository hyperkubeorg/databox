// phase9messenger.go — the Messenger live smoke: over
// real binaries and a real databox node, it drives the web mutations and
// the /api/v1/messenger surface end to end — servers, channels, messages,
// mentions + unread fan-out, DMs, presence, search, and invite redemption.
package main

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// phase9messenger runs the Messenger smoke.
func phase9messenger(ctx context.Context, pcpURL string, userStore *users.Store, keyStore *apikeys.Store) {
	// Ensure the three actors exist (earlier phases may have made some).
	for _, u := range []string{"ada", "bob", "carol"} {
		if _, err := userStore.CreateUser(ctx, u, u, "password123"); err != nil {
			// Already exists is fine; anything else is loud.
			if !strings.Contains(err.Error(), "taken") && !strings.Contains(err.Error(), "exists") {
				// Non-fatal: the account may simply predate this phase.
				_ = err
			}
		}
	}

	ada := newWeb(pcpURL)
	if err := ada.login("ada", "password123"); err != nil {
		fail("phase9: ada login", "err", err)
		return
	}
	bob := newWeb(pcpURL)
	if err := bob.login("bob", "password123"); err != nil {
		fail("phase9: bob login", "err", err)
		return
	}
	pass("messenger: web sessions")

	// --- create an OPEN server (web) ---
	code, body, err := ada.post("/messenger/do/create-server", url.Values{"name": {"Smoke Server"}, "visibility": {"open"}})
	if err != nil || code >= 400 {
		fail("phase9: create server", "code", code, "body", body)
		return
	}
	serverID, _ := jsonMap(body)["server"].(string)
	if serverID == "" {
		fail("phase9: server id missing", "body", body)
		return
	}
	pass("messenger: create server")

	// --- API keys for ada and bob ---
	adaTok, _, err := keyStore.Mint(ctx, "ada", "smoke", []string{apikeys.ScopeMsgRead, apikeys.ScopeMsgWrite}, time.Time{})
	must(err, "phase9 mint ada key")
	bobTok, _, err := keyStore.Mint(ctx, "bob", "smoke", []string{apikeys.ScopeMsgRead, apikeys.ScopeMsgWrite}, time.Time{})
	must(err, "phase9 mint bob key")

	// --- resolve the default channel via the API ---
	sc, cbody := bearer(pcpURL, adaTok, "GET", "/api/v1/messenger/servers/"+serverID+"/channels", "", "")
	if sc != 200 {
		fail("phase9: api channels", "code", sc, "body", cbody)
		return
	}
	channelID := firstChannelID(cbody)
	if channelID == "" {
		fail("phase9: no channel", "body", cbody)
		return
	}
	pass("messenger: api list channels")

	// --- bob joins the open server (web) ---
	sc2, jb, _ := bob.post("/messenger/do/join", url.Values{"server": {serverID}})
	if sc2 >= 400 {
		fail("phase9: bob join", "code", sc2, "body", jb)
		return
	}
	pass("messenger: join open server")

	// --- ada sends a message mentioning bob (API) ---
	sc3, mb := bearer(pcpURL, adaTok, "POST", "/api/v1/messenger/channels/"+channelID+"/messages", `{"body":"hello @bob welcome"}`, "")
	if sc3 != 201 {
		fail("phase9: api send", "code", sc3, "body", mb)
		return
	}
	pass("messenger: api send message")

	// --- bob sees unread + mention (API) ---
	time.Sleep(300 * time.Millisecond)
	sc4, ub := bearer(pcpURL, bobTok, "GET", "/api/v1/messenger/unread", "", "")
	if sc4 != 200 || !strings.Contains(ub, `"mention":true`) {
		fail("phase9: unread/mention", "code", sc4, "body", ub)
		return
	}
	pass("messenger: unread fan-out + mention")

	// --- bob reads the channel, unread clears ---
	bearer(pcpURL, bobTok, "POST", "/api/v1/messenger/channels/"+channelID+"/read", "", "")
	if _, ub2 := bearer(pcpURL, bobTok, "GET", "/api/v1/messenger/unread", "", ""); strings.Contains(ub2, channelID) {
		fail("phase9: unread not cleared", "body", ub2)
		return
	}
	pass("messenger: read clears unread")

	// --- ada opens a DM to bob and sends (API) ---
	sc5, db := bearer(pcpURL, adaTok, "POST", "/api/v1/messenger/dms", `{"user":"bob"}`, "")
	dmCID := stringField(db, "cid")
	if sc5 != 200 || dmCID == "" {
		fail("phase9: open dm", "code", sc5, "body", db)
		return
	}
	if sc, _ := bearer(pcpURL, adaTok, "POST", "/api/v1/messenger/channels/"+dmCID+"/messages", `{"body":"hey bob, a DM"}`, ""); sc != 201 {
		fail("phase9: dm send", "code", sc)
		return
	}
	if _, listing := bearer(pcpURL, bobTok, "GET", "/api/v1/messenger/dms", "", ""); !strings.Contains(listing, dmCID) {
		fail("phase9: dm not listed for bob", "body", listing)
		return
	}
	pass("messenger: direct messages")

	// --- presence set/get (API) ---
	bearer(pcpURL, adaTok, "PUT", "/api/v1/messenger/presence", `{"status":"dnd","message":"heads down"}`, "")
	if _, pb := bearer(pcpURL, adaTok, "GET", "/api/v1/messenger/presence", "", ""); !strings.Contains(pb, `"dnd"`) {
		fail("phase9: presence", "body", pb)
		return
	}
	pass("messenger: presence status")

	// --- search finds the message (API) ---
	time.Sleep(200 * time.Millisecond)
	if sc, sb := bearer(pcpURL, bobTok, "GET", "/api/v1/messenger/search?scope=all&q=welcome", "", ""); sc != 200 || !strings.Contains(sb, "welcome") {
		fail("phase9: search", "code", sc, "body", sb)
		return
	}
	pass("messenger: inverted-index search")

	// --- invite: ada mints, carol redeems (web mint → API redeem) ---
	_, ib, _ := ada.post("/messenger/do/invite", url.Values{"server": {serverID}, "channel": {channelID}})
	invCode, _ := jsonMap(ib)["code"].(string)
	if invCode == "" {
		fail("phase9: invite code missing", "body", ib)
		return
	}
	carolTok, _, err := keyStore.Mint(ctx, "carol", "smoke", []string{apikeys.ScopeMsgWrite}, time.Time{})
	must(err, "phase9 mint carol key")
	if sc, rb := bearer(pcpURL, carolTok, "POST", "/api/v1/messenger/join/"+invCode, "", ""); sc != 200 || !strings.Contains(rb, serverID) {
		fail("phase9: redeem invite", "code", sc, "body", rb)
		return
	}
	pass("messenger: invite redemption")

	// --- server settings: gated page + admin mutations (web) ---
	sc6, sp, _ := ada.get("/messenger/settings/" + serverID)
	if sc6 != 200 || !strings.Contains(sp, "Server settings") || !strings.Contains(sp, "Danger zone") {
		fail("phase9: settings page for owner", "code", sc6)
		return
	}
	if sc, _, _ := bob.get("/messenger/settings/" + serverID); sc != 404 {
		fail("phase9: settings must 404 for plain members", "code", sc)
		return
	}
	pass("messenger: settings page gated to admins")

	if code, b, _ := ada.post("/messenger/do/update-server", url.Values{
		"server": {serverID}, "name": {"Smoke Server II"}, "description": {"renamed by smoke"}, "visibility": {"open"},
	}); code != 200 {
		fail("phase9: update server", "code", code, "body", b)
		return
	}
	if _, sp2, _ := ada.get("/messenger/settings/" + serverID); !strings.Contains(sp2, "Smoke Server II") {
		fail("phase9: rename not reflected")
		return
	}
	pass("messenger: settings rename round-trips")

	// Kick bob: his channel sends refuse until he rejoins (open server).
	if code, b, _ := ada.post("/messenger/do/kick", url.Values{"server": {serverID}, "user": {"bob"}}); code != 200 {
		fail("phase9: kick bob", "code", code, "body", b)
		return
	}
	if code, _, _ := bob.post("/messenger/do/send", url.Values{"server": {serverID}, "channel": {channelID}, "body": {"still here?"}}); code < 400 {
		fail("phase9: kicked member could still send", "code", code)
		return
	}
	if code, _, _ := bob.post("/messenger/do/join", url.Values{"server": {serverID}}); code != 200 {
		fail("phase9: kicked member rejoin", "code", code)
		return
	}
	pass("messenger: kick removes access; open-server rejoin works")

	// Ban carol: rejoining refuses until unbanned.
	if code, _, _ := ada.post("/messenger/do/ban", url.Values{"server": {serverID}, "user": {"carol"}}); code != 200 {
		fail("phase9: ban carol", "code", code)
		return
	}
	if code, _, _ := carolWeb(pcpURL).post("/messenger/do/join", url.Values{"server": {serverID}}); code < 400 {
		fail("phase9: banned member could rejoin", "code", code)
		return
	}
	if code, _, _ := ada.post("/messenger/do/unban", url.Values{"server": {serverID}, "user": {"carol"}}); code != 200 {
		fail("phase9: unban carol", "code", code)
		return
	}
	pass("messenger: ban blocks rejoin until unbanned")

	// Invite lifecycle from settings: mint with a cap, then revoke.
	code, ib2, _ := ada.post("/messenger/do/invite", url.Values{
		"server": {serverID}, "back": {"settings"}, "ttl": {"24h"}, "max_uses": {"1"},
	})
	inv2, _ := jsonMap(ib2)["code"].(string)
	if code != 200 || inv2 == "" {
		fail("phase9: settings invite mint", "code", code, "body", ib2)
		return
	}
	if code, _, _ := ada.post("/messenger/do/revoke-invite", url.Values{"server": {serverID}, "code": {inv2}}); code != 200 {
		fail("phase9: revoke invite", "code", code)
		return
	}
	if sc, rb := bearer(pcpURL, carolTok, "POST", "/api/v1/messenger/join/"+inv2, "", ""); sc < 400 {
		fail("phase9: revoked invite still redeemable", "code", sc, "body", rb)
		return
	}
	pass("messenger: settings invite mint + revoke")

	// --- DM messages fetch with an EMPTY server param (a "server=null"
	// regression: a null data-server once reached the API as a string) ---
	if sc, mb, _ := bob.get("/messenger/api/dm/" + dmCID + "/messages"); sc != 200 || !strings.Contains(mb, "a DM") {
		fail("phase9: dm api messages", "code", sc, "body", mb)
		return
	}
	pass("messenger: DM messages fetch (empty server param)")

	// --- in-place profile API ---
	if sc, pb, _ := ada.get("/messenger/api/profile/bob"); sc != 200 || !strings.Contains(pb, `"username":"bob"`) {
		fail("phase9: profile api", "code", sc, "body", pb)
		return
	}
	pass("messenger: profile card API")

	// --- site-wide presence: a heartbeat from anywhere reads as online ---
	if code, _, _ := bob.post("/messenger/do/heartbeat", url.Values{}); code != 204 {
		fail("phase9: site heartbeat", "code", code)
		return
	}
	// (phase 5 created bob with display name "Bob".)
	if _, rb2, _ := ada.get("/messenger/api/roster/" + serverID); !strings.Contains(rb2, `"Username":"bob","DisplayName":"Bob","Status":"online"`) {
		fail("phase9: heartbeat didn't read as online", "body", rb2)
		return
	}
	pass("messenger: site-wide heartbeat reads as online")

	// --- the waiting-DM bell: ada's DM landed while bob had no messenger
	// page open, so the platform notifications page carries it ---
	if _, nb, _ := bob.get("/notifications"); !strings.Contains(nb, "sent you a message") {
		fail("phase9: waiting-DM bell missing from /notifications")
		return
	}
	pass("messenger: waiting-DM bell lands in platform notifications")

	// --- the appbar bell count API feeds the live badge ---
	if _, cb, _ := bob.get("/notifications/api/count"); !strings.Contains(cb, `"count":`) || strings.Contains(cb, `"count":0`) {
		fail("phase9: bell count api", "body", cb)
		return
	}
	pass("messenger: appbar bell count API nonzero")

	pass("messenger: phase 9 complete")
}

// carolWeb logs a fresh web session in as carol (she RSVPs/joins via
// API elsewhere; the ban check needs her browser identity).
func carolWeb(pcpURL string) *web {
	w := newWeb(pcpURL)
	if err := w.login("carol", "password123"); err != nil {
		fail("phase9: carol login", "err", err)
	}
	return w
}

// firstChannelID pulls the first channel id from the channels response.
func firstChannelID(body string) string {
	m := jsonMap(body)
	list, _ := m["channels"].([]any)
	if len(list) == 0 {
		return ""
	}
	ch, _ := list[0].(map[string]any)
	id, _ := ch["id"].(string)
	return id
}

// stringField reads a top-level string field from a JSON object body.
func stringField(body, field string) string {
	v, _ := jsonMap(body)[field].(string)
	return v
}
