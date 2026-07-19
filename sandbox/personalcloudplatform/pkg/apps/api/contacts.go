// contacts.go — the Contacts API v1 endpoints (spec §12.2, scopes
// contacts:read / contacts:write). Cards are drive FILES, so a contact
// is addressed by its (drive, node) pair. Response shapes are
// documented in docs/api.md and gated by shape tests.
package api

import (
	"net/http"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dcontacts "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/contacts"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// contactsRoutes are the Contacts endpoints Mount registers.
func (h *handlers) contactsRoutes(k *kernel.App) []kernel.Route {
	return []kernel.Route{
		{Pattern: "GET /api/v1/contacts", Handler: k.APIAuthed(apikeys.ScopeContactsRead, h.contactsList)},
		{Pattern: "GET /api/v1/contacts/{drive}/{node}", Handler: k.APIAuthed(apikeys.ScopeContactsRead, h.contactGet)},
		{Pattern: "POST /api/v1/contacts", Handler: k.APIAuthed(apikeys.ScopeContactsWrite, h.contactCreate)},
		{Pattern: "PUT /api/v1/contacts/{drive}/{node}", Handler: k.APIAuthed(apikeys.ScopeContactsWrite, h.contactPut)},
		{Pattern: "DELETE /api/v1/contacts/{drive}/{node}", Handler: k.APIAuthed(apikeys.ScopeContactsWrite, h.contactDelete)},
	}
}

// contactResponse is one contact resource.
type contactResponse struct {
	DriveID string            `json:"driveId"`
	NodeID  string            `json:"nodeId"`
	Name    string            `json:"name"`
	Emails  []string          `json:"emails,omitempty"`
	Phones  []string          `json:"phones,omitempty"`
	Org     string            `json:"org,omitempty"`
	Title   string            `json:"title,omitempty"`
	Notes   string            `json:"notes,omitempty"`
	Fields  []dcontacts.Field `json:"fields,omitempty"`
	CanEdit bool              `json:"canEdit,omitempty"`
}

func toContactResponse(driveID, nodeID string, c dcontacts.Card, canEdit bool) contactResponse {
	return contactResponse{
		DriveID: driveID, NodeID: nodeID, Name: c.Name,
		Emails: c.Emails, Phones: c.Phones, Org: c.Org, Title: c.Title, Notes: c.Notes,
		Fields: c.Fields, CanEdit: canEdit,
	}
}

// contactsList answers the aggregated address book (GET /contacts).
func (h *handlers) contactsList(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	entries, err := h.k.Contacts.Aggregate(cctx, user.Username)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "contacts aggregation failed")
		return
	}
	out := []contactResponse{}
	for _, en := range entries {
		out = append(out, toContactResponse(en.DriveID, en.NodeID, en.Card, en.CanEdit))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"contacts": out})
}

// contactNode resolves + authorizes one .pccard file for the key owner.
func (h *handlers) contactNode(r *http.Request, user users.User, minRole string) (string, string, nodes.Node, bool) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	role, _, err := h.k.Shares.Access(cctx, user.Username, driveID, nodeID)
	if err != nil || !drives.RoleAtLeast(role, minRole) {
		return "", "", nodes.Node{}, false
	}
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || !dcontacts.IsCardFile(node) {
		return "", "", nodes.Node{}, false
	}
	return driveID, nodeID, node, true
}

// contactGet answers one card (GET /contacts/{drive}/{node}).
func (h *handlers) contactGet(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, nodeID, node, ok := h.contactNode(r, user, drives.RoleViewer)
	if !ok {
		kernel.APIError(w, http.StatusNotFound, "not_found", "no such contact")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	card, err := h.k.Contacts.LoadCard(cctx, driveID, node.BlobID)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "card load failed")
		return
	}
	role, _, _ := h.k.Shares.Access(cctx, user.Username, driveID, nodeID)
	kernel.JSON(w, http.StatusOK, toContactResponse(driveID, nodeID, card, drives.RoleAtLeast(role, drives.RoleEditor)))
}

// cardBody is the POST/PUT request shape.
type cardBody struct {
	DriveID string            `json:"driveId"`
	Name    string            `json:"name"`
	Emails  []string          `json:"emails"`
	Phones  []string          `json:"phones"`
	Org     string            `json:"org"`
	Title   string            `json:"title"`
	Notes   string            `json:"notes"`
	Fields  []dcontacts.Field `json:"fields"`
}

func (b cardBody) card() dcontacts.Card {
	return dcontacts.Card{Name: b.Name, Emails: b.Emails, Phones: b.Phones, Org: b.Org, Title: b.Title, Notes: b.Notes, Fields: b.Fields}
}

// contactCreate makes a card (POST /contacts). Omitting driveId targets
// the personal drive's Contacts folder (created lazily).
func (h *handlers) contactCreate(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var body cardBody
	if decodeJSON(w, r, &body) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	driveID := body.DriveID
	if driveID == "" {
		driveID = user.PersonalDrive
	}
	role, _, err := h.k.Shares.Access(cctx, user.Username, driveID, nodes.RootID)
	if err != nil || !drives.RoleAtLeast(role, drives.RoleEditor) {
		kernel.APIError(w, http.StatusNotFound, "not_found", "no such drive")
		return
	}
	sc, scErr := h.k.Site.Get(cctx)
	if scErr != nil {
		sc = site.Config{}
	}
	quota := site.QuotaFor(sc, user.QuotaOverride, user.Tier, h.k.DefaultQuota)
	driveID, node, err := h.k.Contacts.CreateCard(cctx, user, body.DriveID, body.card(), quota)
	if err != nil {
		kernel.APIError(w, kernel.ErrStatus(err), "bad_request", kernel.UserErr(err))
		return
	}
	card, _ := h.k.Contacts.LoadCard(cctx, driveID, node.BlobID)
	kernel.JSON(w, http.StatusCreated, toContactResponse(driveID, node.ID, card, true))
}

// contactPut overwrites a card's fields (PUT /contacts/{drive}/{node}).
func (h *handlers) contactPut(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var body cardBody
	if decodeJSON(w, r, &body) != nil {
		return
	}
	driveID, nodeID, _, ok := h.contactNode(r, user, drives.RoleEditor)
	if !ok {
		kernel.APIError(w, http.StatusNotFound, "not_found", "no such contact")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	card, err := h.k.Contacts.SaveCard(cctx, driveID, nodeID, user.Username, body.card())
	if err != nil {
		kernel.APIError(w, http.StatusBadRequest, "bad_request", kernel.UserErr(err))
		return
	}
	kernel.JSON(w, http.StatusOK, toContactResponse(driveID, nodeID, card, true))
}

// contactDelete removes a card file (DELETE /contacts/{drive}/{node}).
func (h *handlers) contactDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, nodeID, _, ok := h.contactNode(r, user, drives.RoleEditor)
	if !ok {
		kernel.APIError(w, http.StatusNotFound, "not_found", "no such contact")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if _, err := h.k.Shares.DeleteNode(cctx, driveID, nodeID); err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "delete failed")
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"deleted": true})
}
