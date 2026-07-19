package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dcontacts "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/contacts"
)

// The documented contact response shape (docs/api.md) + the CRUD loop.
func TestContactsShapesAndCRUD(t *testing.T) {
	h, ada := calWorld(t)
	ctx := context.Background()

	// POST /contacts → 201 + the documented shape (personal drive's
	// Contacts folder by default).
	w := httptest.NewRecorder()
	h.contactCreate(w, httptest.NewRequest("POST", "/api/v1/contacts", strings.NewReader(
		`{"name":"Grace Hopper","emails":["grace@remote.example"],"phones":["+1 555 0100"],"org":"Navy"}`)), apikeys.Key{}, ada)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}
	got := jsonBody(t, w)
	for _, k := range []string{"driveId", "nodeId", "name", "emails", "phones", "org", "canEdit"} {
		if _, present := got[k]; !present {
			t.Errorf("contact shape missing %q: %v", k, got)
		}
	}
	if got["name"] != "Grace Hopper" || got["driveId"] != ada.PersonalDrive {
		t.Errorf("contact = %v", got)
	}
	nodeID := got["nodeId"].(string)

	// The Contacts folder was created lazily.
	if folder, found, _ := h.k.Nodes.GetChild(ctx, ada.PersonalDrive, "root", dcontacts.FolderName); !found || !folder.IsDir {
		t.Error("Contacts folder missing")
	}

	// GET /contacts lists it.
	w = httptest.NewRecorder()
	h.contactsList(w, httptest.NewRequest("GET", "/api/v1/contacts", nil), apikeys.Key{}, ada)
	if list := jsonBody(t, w)["contacts"].([]any); len(list) != 1 {
		t.Fatalf("contacts = %v", list)
	}

	// GET one.
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", ada.PersonalDrive)
	req.SetPathValue("node", nodeID)
	w = httptest.NewRecorder()
	h.contactGet(w, req, apikeys.Key{}, ada)
	if w.Code != http.StatusOK || jsonBody(t, w)["name"] != "Grace Hopper" {
		t.Fatalf("get = %d %s", w.Code, w.Body.String())
	}

	// PUT replaces fields.
	req = httptest.NewRequest("PUT", "/x", strings.NewReader(`{"name":"Grace B. Hopper","emails":["grace@remote.example"]}`))
	req.SetPathValue("drive", ada.PersonalDrive)
	req.SetPathValue("node", nodeID)
	w = httptest.NewRecorder()
	h.contactPut(w, req, apikeys.Key{}, ada)
	if w.Code != http.StatusOK || jsonBody(t, w)["name"] != "Grace B. Hopper" {
		t.Fatalf("put = %d %s", w.Code, w.Body.String())
	}
	// Bad cards refuse.
	req = httptest.NewRequest("PUT", "/x", strings.NewReader(`{"name":"","emails":[]}`))
	req.SetPathValue("drive", ada.PersonalDrive)
	req.SetPathValue("node", nodeID)
	w = httptest.NewRecorder()
	h.contactPut(w, req, apikeys.Key{}, ada)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad put = %d", w.Code)
	}

	// DELETE removes the file; the list empties.
	req = httptest.NewRequest("DELETE", "/x", nil)
	req.SetPathValue("drive", ada.PersonalDrive)
	req.SetPathValue("node", nodeID)
	w = httptest.NewRecorder()
	h.contactDelete(w, req, apikeys.Key{}, ada)
	if w.Code != http.StatusOK || jsonBody(t, w)["deleted"] != true {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.contactsList(w, httptest.NewRequest("GET", "/api/v1/contacts", nil), apikeys.Key{}, ada)
	if list := jsonBody(t, w)["contacts"].([]any); len(list) != 0 {
		t.Fatalf("contact survived delete: %v", list)
	}
}

// Another user's key can't reach a private card (not_found, never 403 —
// existence stays private).
func TestContactsCrossUser(t *testing.T) {
	h, ada := calWorld(t)
	ctx := context.Background()
	mallory, _ := h.k.Users.CreateUser(ctx, "mallory", "Mallory", "hunter22pass")

	w := httptest.NewRecorder()
	h.contactCreate(w, httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"Secret Pal","emails":["pal@remote.example"]}`)), apikeys.Key{}, ada)
	nodeID := jsonBody(t, w)["nodeId"].(string)

	req := httptest.NewRequest("GET", "/x", nil)
	req.SetPathValue("drive", ada.PersonalDrive)
	req.SetPathValue("node", nodeID)
	w = httptest.NewRecorder()
	h.contactGet(w, req, apikeys.Key{}, mallory)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-user get = %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.contactsList(w, httptest.NewRequest("GET", "/x", nil), apikeys.Key{}, mallory)
	if list := jsonBody(t, w)["contacts"].([]any); len(list) != 0 {
		t.Fatalf("cross-user list = %v", list)
	}
}
