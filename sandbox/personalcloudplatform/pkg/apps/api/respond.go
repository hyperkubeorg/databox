// respond.go — the response conventions every /api/v1 endpoint shares
// (spec §12.2), so later phases add endpoints without re-deciding them:
//
//   - JSON bodies both ways; kernel.JSON writes, decodeJSON reads.
//   - Errors are always the {code, error} envelope (kernel.APIError).
//   - Cursor pagination mirrors databox List: ?cursor= (opaque, from the
//     previous response's nextCursor) and ?limit= via pageParams.
//   - Times are RFC 3339 UTC — encoding/json's time.Time default; no
//     custom formats anywhere.
//   - Mutations return the resource with its new revision;
//     Idempotency-Key is honored on send and upload (their phases).
//   - No CORS headers, deliberately: native clients don't need them and
//     it keeps cross-origin browser abuse off the table.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// Pagination bounds.
const (
	defaultLimit = 50
	maxLimit     = 200
)

// maxBody caps a JSON request body; uploads get their own paths with
// their own caps (phase 2).
const maxBody = 1 << 20

// pageParams reads the ?cursor=&limit= pair. The cursor is opaque to
// clients and passed to the domain layer verbatim — domains validate
// anything that becomes a storage key.
func pageParams(r *http.Request) (cursor string, limit int) {
	cursor = r.URL.Query().Get("cursor")
	limit = defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = min(n, maxLimit)
		}
	}
	return cursor, limit
}

// decodeJSON reads one bounded JSON request body into v, answering the
// envelope itself on failure (the caller just returns on error).
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		kernel.APIError(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return err
	}
	return nil
}
