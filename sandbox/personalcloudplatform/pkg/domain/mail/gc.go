// gc.go — the lazy orphaned-blob sweep. Message blobs and their search
// text are deleted best-effort AFTER the purge transaction (threads.go)
// and a mailbox delete walks thousands of them (addrs.go) — a crash
// mid-purge can strand blob + searchtext rows whose msgref is gone.
// This sweep reclaims them.
//
// Orphan-ness is decidable because delivery makes BlobID == MsgID
// (deliver.go): a message blob without /pcp/mail/msgref/<user>/<id> is
// referenced by nothing. Deliver writes the blob BEFORE the msgref
// transaction, so a candidate is never deleted on first sight — it is
// remembered under /pcp/mail/gcpending/ and reclaimed only when still
// orphaned after a grace period (two sweeps see it). Draft attachments
// (att-*, die with their draft), outbound copies (out-*, cleared by the
// outbound loop), and system mail (+sys, uncharged DSN staging) have
// their own lifecycles and are skipped.
//
// The sweep is throttled: one bounded page of blob + searchtext keys
// per pass, continuing from a persisted cursor, so a large install
// amortizes the scan instead of paying it at once.
package mail

import (
	"context"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// GC keyspace (kvx key table).
const (
	gcPendingPrefix = "/pcp/mail/gcpending/"
	gcCursorKey     = "/pcp/mail/gccursor"
)

// SweepOrphanBlobs runs one bounded GC pass: resolve every pending
// candidate (reclaim if still orphaned past grace, forget if its msgref
// reappeared), then scan the next pageLimit blob/searchtext keys from
// the persisted cursor for new candidates. Returns how many orphans
// were reclaimed. Safe to run concurrently with delivery — the grace
// period outlives Deliver's blob-before-msgref window by orders of
// magnitude.
func (s *Store) SweepOrphanBlobs(ctx context.Context, pageLimit int, grace time.Duration) (int, error) {
	if pageLimit <= 0 {
		pageLimit = 500
	}
	removed, err := s.resolvePending(ctx, grace)
	if err != nil {
		return removed, err
	}
	if err := s.discoverOrphans(ctx, pageLimit); err != nil {
		return removed, err
	}
	return removed, nil
}

// resolvePending walks every gcpending row: a candidate whose msgref
// came back is a false alarm (delivery won); one still orphaned past
// its grace stamp is reclaimed (blob + searchtext + the row itself —
// all idempotent, so a candidate whose blob already died just clears).
func (s *Store) resolvePending(ctx context.Context, grace time.Duration) (int, error) {
	removed := 0
	err := kvx.ScanPrefix(ctx, s.DB, gcPendingPrefix, func(key string, value []byte) error {
		rest := strings.TrimPrefix(key, gcPendingPrefix)
		user, blobID, ok := strings.Cut(rest, "/")
		if !ok {
			return s.DB.Delete(ctx, key) // malformed row — drop it
		}
		if _, live, err := s.DB.Get(ctx, msgRefPrefix+user+"/"+blobID); err != nil {
			return err
		} else if live {
			return s.DB.Delete(ctx, key) // delivery completed — not an orphan
		}
		seen, err := time.Parse(time.RFC3339Nano, string(value))
		if err != nil {
			return s.DB.Delete(ctx, key) // unreadable stamp — re-discover next pass
		}
		if time.Since(seen) < grace {
			return nil // too fresh; next sweep decides
		}
		_ = s.DB.DeleteBlob(ctx, blobsPrefix+user+"/"+blobID)
		_ = s.DB.Delete(ctx, searchPrefix+user+"/"+blobID)
		removed++
		return s.DB.Delete(ctx, key)
	})
	return removed, err
}

// discoverOrphans scans one page of blob keys and one of searchtext
// keys (searchtext can outlive its blob when a purge died between the
// two deletes) from the persisted cursor, staging msgref-less message
// ids as pending candidates. The cursor wraps to the start when a
// prefix is exhausted, so the whole space is revisited over time.
func (s *Store) discoverOrphans(ctx context.Context, pageLimit int) error {
	var cur struct {
		Blobs  string `json:"blobs"`
		Search string `json:"search"`
	}
	if _, err := kvx.GetJSON(ctx, s.DB, gcCursorKey, &cur); err != nil {
		return err
	}
	var err error
	if cur.Blobs, err = s.discoverPage(ctx, blobsPrefix, cur.Blobs, pageLimit); err != nil {
		return err
	}
	if cur.Search, err = s.discoverPage(ctx, searchPrefix, cur.Search, pageLimit); err != nil {
		return err
	}
	return kvx.SetJSON(ctx, s.DB, gcCursorKey, cur)
}

// discoverPage stages one prefix's next page of candidates, returning
// the next cursor ("" = wrapped).
func (s *Store) discoverPage(ctx context.Context, prefix, cursor string, pageLimit int) (string, error) {
	if cursor != "" {
		cursor = prefix + cursor
	}
	entries, next, err := s.DB.List(ctx, prefix, cursor, pageLimit)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, e := range entries {
		rest := strings.TrimPrefix(e.Key, prefix)
		user, blobID, ok := strings.Cut(rest, "/")
		if !ok || user == SystemMailAccount ||
			strings.HasPrefix(blobID, "att-") || strings.HasPrefix(blobID, "out-") {
			continue
		}
		if _, live, err := s.DB.Get(ctx, msgRefPrefix+user+"/"+blobID); err != nil {
			return "", err
		} else if live {
			continue
		}
		pKey := gcPendingPrefix + user + "/" + blobID
		if _, staged, err := s.DB.Get(ctx, pKey); err != nil {
			return "", err
		} else if staged {
			continue // the grace clock is already running
		}
		if _, err := s.DB.Set(ctx, pKey, []byte(now)); err != nil {
			return "", err
		}
	}
	return strings.TrimPrefix(next, prefix), nil
}
