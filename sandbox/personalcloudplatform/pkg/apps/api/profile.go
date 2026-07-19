// profile.go — the two phase-2a endpoints: the owner profile and key
// self-inspection.
package api

import (
	"net/http"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// profileResponse is GET /api/v1/profile's body (docs/api.md).
type profileResponse struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Admin       bool   `json:"admin"`
	// QuotaBytes is the effective quota; 0 = unlimited.
	QuotaBytes int64 `json:"quotaBytes"`
	// UsedBytes rides along because it already sits on the account
	// record — no extra scan.
	UsedBytes int64 `json:"usedBytes"`
}

// profile answers the calling key's owner profile (profile:read).
func (h *handlers) profile(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		// Quota resolution soft-fails to the bootstrap default — the
		// profile isn't worth a 500 over a config read.
		h.k.Log.Warn("site config read failed", "err", err)
	}
	kernel.JSON(w, http.StatusOK, profileResponse{
		Username:    user.Username,
		DisplayName: user.DisplayName,
		Admin:       user.IsAdmin,
		QuotaBytes:  site.QuotaFor(sc, user.QuotaOverride, user.Tier, h.k.DefaultQuota),
		UsedBytes:   user.UsedBytes,
	})
}

// keyResponse is GET /api/v1/scopes' body: the calling key's own
// metadata, so a client can see what it holds instead of discovering it
// through 403s.
type keyResponse struct {
	KeyID  string   `json:"keyId"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	// ExpiresAt is absent for keys that never expire.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// scopes answers for ANY valid key (kernel.ScopeAny).
func (h *handlers) scopes(w http.ResponseWriter, _ *http.Request, key apikeys.Key, _ users.User) {
	out := keyResponse{KeyID: key.KeyID, Name: key.Name, Scopes: key.Scopes}
	if !key.ExpiresAt.IsZero() {
		out.ExpiresAt = &key.ExpiresAt
	}
	kernel.JSON(w, http.StatusOK, out)
}
