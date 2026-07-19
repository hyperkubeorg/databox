// respond.go — progressive enhancement's plumbing: every mutation
// accepts BOTH a plain form POST (finish with a redirect) and a fetch()
// call (finish with JSON), picked by the X-Requested-With header the
// shared JS sends.
package kernel

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// WantsJSON reports whether the shared JS made this request (vs. a plain
// form submit that needs a redirect back).
func WantsJSON(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "fetch"
}

// JSON writes one JSON response.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Respond finishes a mutation: JSON {ok:…} for fetch callers, a redirect
// (with ?err= for failures) for plain forms. payload rides only the JSON
// path.
func (a *App) Respond(w http.ResponseWriter, r *http.Request, back string, err error, payload map[string]any) {
	if WantsJSON(r) {
		out := map[string]any{"ok": err == nil}
		status := http.StatusOK
		if err != nil {
			out["error"] = userErr(err)
			status = errStatus(err)
		}
		for k, v := range payload {
			out[k] = v
		}
		JSON(w, status, out)
		return
	}
	if err != nil {
		sep := "?"
		if strings.Contains(back, "?") {
			sep = "&"
		}
		back += sep + "err=" + url.QueryEscape(userErr(err))
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// UserErr and ErrStatus are userErr/errStatus for apps that assemble
// their own JSON envelopes (the upload endpoints' per-file results,
// where Respond's single-error shape doesn't fit).
func UserErr(err error) string { return userErr(err) }
func ErrStatus(err error) int  { return errStatus(err) }

// userErr keeps raw internals out of the UI: known domain errors pass
// through verbatim, anything else short and key-free passes, the rest
// generalizes.
func userErr(err error) string {
	for _, known := range []error{
		users.ErrUsernameTaken, users.ErrBadCredentials, users.ErrNotFound,
		users.ErrQuotaExceeded, nodes.ErrNameTaken, drives.ErrAccessDenied,
	} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	msg := err.Error()
	if len(msg) < 120 && !strings.Contains(msg, "/pcp/") {
		return msg
	}
	return "something went wrong — try again"
}

// errStatus maps a mutation error to an HTTP status for JSON callers.
func errStatus(err error) int {
	switch {
	case errors.Is(err, users.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, users.ErrBadCredentials), errors.Is(err, users.ErrNoSession):
		return http.StatusUnauthorized
	case errors.Is(err, drives.ErrAccessDenied):
		return http.StatusForbidden
	case errors.Is(err, nodes.ErrNameTaken):
		return http.StatusConflict
	case errors.Is(err, users.ErrQuotaExceeded):
		return http.StatusInsufficientStorage
	default:
		return http.StatusBadRequest
	}
}
