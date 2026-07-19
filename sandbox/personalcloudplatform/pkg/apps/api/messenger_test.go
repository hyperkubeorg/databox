package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// The documented /api/v1/messenger response shapes are guarantees — this
// test drives the read + send endpoints end to end over kvxtest.
func TestMessengerAPIShapes(t *testing.T) {
	h := testHandlers(t)
	db := h.k.Users.DB
	h.k.Msg = &dmessenger.Store{DB: db, Users: h.k.Users, Notify: &notify.Store{DB: db}}
	ctx := t.Context()

	ada, err := h.k.Users.CreateUser(ctx, "ada", "Ada", "password123")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := h.k.Msg.CreateServer(ctx, "ada", "API Server", dmessenger.VisibilityOpen)
	if err != nil {
		t.Fatal(err)
	}
	chans, _ := h.k.Msg.Channels(ctx, srv.ID)
	channelID := chans[0].ID

	// GET servers → {servers:[{id,name,visibility,owner}]}
	w := httptest.NewRecorder()
	h.msgServers(w, httptest.NewRequest("GET", "/api/v1/messenger/servers", nil), apikeys.Key{}, ada)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"API Server"`) {
		t.Fatalf("servers = %d %s", w.Code, w.Body)
	}

	// GET channels → {channels:[{id,name,...}]}
	w = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/messenger/servers/"+srv.ID+"/channels", nil)
	req.SetPathValue("server", srv.ID)
	h.msgChannels(w, req, apikeys.Key{}, ada)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"general"`) {
		t.Fatalf("channels = %d %s", w.Code, w.Body)
	}

	// POST a message → 201 with the message resource.
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/v1/messenger/channels/"+channelID+"/messages", strings.NewReader(`{"body":"hello **api**"}`))
	req.SetPathValue("cid", channelID)
	h.msgSend(w, req, apikeys.Key{}, ada)
	if w.Code != http.StatusCreated {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	var sent map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &sent); err != nil {
		t.Fatalf("decode send: %v", err)
	}
	if sent["author"] != "ada" || !strings.Contains(sent["html"].(string), "<strong>api</strong>") {
		t.Fatalf("message resource = %v", sent)
	}

	// GET messages → the sent message, newest-first.
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/messenger/channels/"+channelID+"/messages", nil)
	req.SetPathValue("cid", channelID)
	h.msgMessages(w, req, apikeys.Key{}, ada)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"body":"hello **api**"`) {
		t.Fatalf("messages = %d %s", w.Code, w.Body)
	}

	// A non-member is refused the channel listing.
	eve := users.User{Username: "eve"}
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/messenger/servers/"+srv.ID+"/channels", nil)
	req.SetPathValue("server", srv.ID)
	h.msgChannels(w, req, apikeys.Key{}, eve)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-member channels = %d, want 403", w.Code)
	}
}

// PATCH and DELETE /api/v1/messenger/messages/{msg}: the author edits
// and deletes their own; another member can't touch it; a moderator
// (the server owner) can delete but not edit.
func TestMessengerAPIEditDelete(t *testing.T) {
	h := testHandlers(t)
	db := h.k.Users.DB
	h.k.Msg = &dmessenger.Store{DB: db, Users: h.k.Users, Notify: &notify.Store{DB: db}}
	ctx := t.Context()

	ada, err := h.k.Users.CreateUser(ctx, "ada", "Ada", "password123") // owner/mod
	if err != nil {
		t.Fatal(err)
	}
	bob, err := h.k.Users.CreateUser(ctx, "bob", "Bob", "password123")
	if err != nil {
		t.Fatal(err)
	}
	eve, err := h.k.Users.CreateUser(ctx, "eve", "Eve", "password123")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := h.k.Msg.CreateServer(ctx, "ada", "Mod Server", dmessenger.VisibilityOpen)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"bob", "eve"} {
		if err := h.k.Msg.Join(ctx, srv.ID, u); err != nil {
			t.Fatal(err)
		}
	}
	chans, _ := h.k.Msg.Channels(ctx, srv.ID)
	cid := chans[0].ID
	msg, err := h.k.Msg.SendToChannel(ctx, srv.ID, cid, bob, "first draft", dmessenger.SendOpts{})
	if err != nil {
		t.Fatal(err)
	}

	edit := func(who users.User, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", "/x", strings.NewReader(`{"body":`+body+`}`))
		req.SetPathValue("msg", msg.ID)
		w := httptest.NewRecorder()
		h.msgEdit(w, req, apikeys.Key{}, who)
		return w
	}
	del := func(who users.User) *httptest.ResponseRecorder {
		req := httptest.NewRequest("DELETE", "/x", nil)
		req.SetPathValue("msg", msg.ID)
		w := httptest.NewRecorder()
		h.msgDelete(w, req, apikeys.Key{}, who)
		return w
	}

	// Eve (plain member) can neither edit nor delete Bob's message.
	if w := edit(eve, `"hijack"`); w.Code == http.StatusOK {
		t.Fatalf("eve edit = %d %s", w.Code, w.Body)
	}
	if w := del(eve); w.Code == http.StatusOK {
		t.Fatalf("eve delete = %d %s", w.Code, w.Body)
	}
	// Ada (owner, PermManageMessages) may not edit but may delete.
	if w := edit(ada, `"still not yours"`); w.Code == http.StatusOK {
		t.Fatalf("mod edit = %d %s", w.Code, w.Body)
	}
	// Bob edits his own.
	w := edit(bob, `"final version"`)
	if w.Code != http.StatusOK {
		t.Fatalf("author edit = %d %s", w.Code, w.Body)
	}
	var edited map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &edited); err != nil {
		t.Fatal(err)
	}
	if edited["body"] != "final version" || edited["edited"] != true {
		t.Errorf("edited resource = %v", edited)
	}
	// Ada moderator-deletes it.
	if w := del(ada); w.Code != http.StatusOK {
		t.Fatalf("mod delete = %d %s", w.Code, w.Body)
	}
	if m, found, _ := h.k.Msg.GetMessage(ctx, msg.ID); !found || !m.Deleted {
		t.Errorf("message not tombstoned: %+v (found=%v)", m, found)
	}
}

// GET /api/v1/messenger/att/{cid}/{blob}: message attachment URLs point
// at the bearer path, members can fetch the bytes, outsiders get 404.
func TestMessengerAPIAttachment(t *testing.T) {
	h := testHandlers(t)
	db := h.k.Users.DB
	h.k.Msg = &dmessenger.Store{DB: db, Users: h.k.Users, Notify: &notify.Store{DB: db}}
	ctx := t.Context()

	ada, err := h.k.Users.CreateUser(ctx, "ada", "Ada", "password123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.k.Users.CreateUser(ctx, "bob", "Bob", "password123"); err != nil {
		t.Fatal(err)
	}
	cid, err := h.k.Msg.OpenDM(ctx, "ada", "bob")
	if err != nil {
		t.Fatal(err)
	}
	att, err := h.k.Msg.StageAttachment(ctx, cid, "ada", "notes.txt", "text/plain", 5, 1<<20, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := h.k.Msg.SendToConvo(ctx, ada, cid, "see attached", dmessenger.SendOpts{Attachments: []dmessenger.Attachment{att}})
	if err != nil {
		t.Fatal(err)
	}

	// The resource's attachment URL is the bearer-authed API path.
	res := msgResource(msg)
	if len(res.Attachments) != 1 || !strings.HasPrefix(res.Attachments[0].URL, "/api/v1/messenger/att/") {
		t.Fatalf("attachment url = %+v", res.Attachments)
	}

	fetch := func(who users.User) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/x", nil)
		req.SetPathValue("cid", cid)
		req.SetPathValue("blob", att.BlobID)
		w := httptest.NewRecorder()
		h.msgAttachment(w, req, apikeys.Key{}, who)
		return w
	}
	if w := fetch(ada); w.Code != http.StatusOK || w.Body.String() != "hello" {
		t.Fatalf("member fetch = %d %q", w.Code, w.Body.String())
	}
	if w := fetch(users.User{Username: "eve"}); w.Code != http.StatusNotFound {
		t.Fatalf("outsider fetch = %d, want 404", w.Code)
	}
}

// Typing: POST records the caller; GET answers others' typing state and
// excludes the caller's own.
func TestMessengerAPITyping(t *testing.T) {
	h := testHandlers(t)
	db := h.k.Users.DB
	h.k.Msg = &dmessenger.Store{DB: db, Users: h.k.Users, Notify: &notify.Store{DB: db}}
	ctx := t.Context()

	ada, err := h.k.Users.CreateUser(ctx, "ada", "Ada", "password123")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := h.k.Users.CreateUser(ctx, "bob", "Bob", "password123")
	if err != nil {
		t.Fatal(err)
	}
	cid, err := h.k.Msg.OpenDM(ctx, "ada", "bob")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/x", nil)
	req.SetPathValue("cid", cid)
	w := httptest.NewRecorder()
	h.msgTypingPost(w, req, apikeys.Key{}, ada)
	if w.Code != http.StatusOK {
		t.Fatalf("typing post = %d %s", w.Code, w.Body)
	}

	get := func(who users.User) map[string]any {
		req := httptest.NewRequest("GET", "/x", nil)
		req.SetPathValue("cid", cid)
		w := httptest.NewRecorder()
		h.msgTypingGet(w, req, apikeys.Key{}, who)
		if w.Code != http.StatusOK {
			t.Fatalf("typing get = %d %s", w.Code, w.Body)
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if typing := get(bob)["typing"].([]any); len(typing) != 1 || typing[0] != "ada" {
		t.Errorf("bob sees typing = %v", typing)
	}
	if typing := get(ada)["typing"].([]any); len(typing) != 0 {
		t.Errorf("ada sees own typing = %v", typing)
	}
}
