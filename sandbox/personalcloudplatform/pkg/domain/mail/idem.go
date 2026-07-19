// idem.go — the Idempotency-Key ledger for API sends (spec §12.2):
// /pcp/mail/idem/<user>/<keyHash> → the SendResult a key already
// produced. A retried POST with the same key returns the recorded
// result instead of sending twice. Entries go stale after a day (a key
// reused later is a NEW send — matching Stripe-style semantics) and
// overwrite in place, so the ledger self-bounds per user per key.
package mail

import (
	"context"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// idemTTL is how long a recorded key deduplicates.
const idemTTL = 24 * time.Hour

// idemRecord is one ledger row.
type idemRecord struct {
	Result SendResult `json:"result"`
	At     time.Time  `json:"at"`
}

// idemKey hashes the caller-supplied key into a fixed key segment
// (client keys are arbitrary strings — never storage-key material).
func idemKey(user, key string) string {
	return idemPrefix + user + "/" + shortHash(key)
}

// IdempotentSend answers a previously recorded result for this key,
// when one exists and is fresh.
func (s *Store) IdempotentSend(ctx context.Context, user, key string) (SendResult, bool) {
	if key == "" {
		return SendResult{}, false
	}
	var rec idemRecord
	found, err := kvx.GetJSON(ctx, s.DB, idemKey(user, key), &rec)
	if err != nil || !found || time.Since(rec.At) > idemTTL {
		return SendResult{}, false
	}
	return rec.Result, true
}

// RecordIdempotentSend notes a completed send under its key
// (best-effort — a miss just means a retry could double-send).
func (s *Store) RecordIdempotentSend(ctx context.Context, user, key string, res SendResult) {
	if key == "" {
		return
	}
	_ = kvx.SetJSON(ctx, s.DB, idemKey(user, key), idemRecord{Result: res, At: time.Now().UTC()})
}
