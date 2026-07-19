// spool.go — the sealed store-and-forward spool . One
// file per accepted message, ciphertext before first write, deleted on
// PCP's ack. The on-disk shape is exactly the docs promise: opaque
// sortable ids, sizes, timestamps — nothing else. Per-recipient share
// accounting lives in RAM only (persisting it would leak who gets
// mail), so shares reset on restart; the global byte cap never does.
package postoffice

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// spoolEntry is the RAM index row.
type spoolEntry struct {
	id   string
	size int64
	// rcptHashes lets the per-recipient share release on ack. RAM only.
	rcptHashes []string
}

// Spool is the sealed message store.
type Spool struct {
	dir string

	mu      sync.Mutex
	entries map[string]*spoolEntry
	order   []string // sorted ids (they embed the arrival time)
	bytes   int64
	perRcpt map[string]int64 // rcptHash → spooled bytes (RAM only)
	waiters []chan struct{}
}

// openSpool scans the directory and rebuilds the index (ids and sizes
// only — everything else is sealed).
func openSpool(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sp := &Spool{dir: dir, entries: map[string]*spoolEntry{}, perRcpt: map[string]int64{}}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".sealed") {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(f.Name(), ".sealed")
		sp.entries[id] = &spoolEntry{id: id, size: info.Size()}
		sp.bytes += info.Size()
	}
	for id := range sp.entries {
		sp.order = append(sp.order, id)
	}
	sort.Strings(sp.order)
	return sp, nil
}

// newSpoolID mints a time-sortable opaque id (oldest sorts first, so a
// plain string sort is the drain order).
func newSpoolID(at time.Time) string {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return fmt.Sprintf("%019d-%s", at.UnixNano(), hex.EncodeToString(suffix))
}

// Usage reports current fill.
func (sp *Spool) Usage() (bytes int64, count int) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.bytes, len(sp.entries)
}

// HasRoom answers the MAIL-time gate: does size fit under cap, and —
// when rcptHashes are known — under every recipient's share ?
func (sp *Spool) HasRoom(size, cap int64, sharePct int, rcptHashes []string) bool {
	if cap <= 0 {
		return true
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.bytes+size > cap {
		return false
	}
	if sharePct > 0 && sharePct < 100 {
		share := cap * int64(sharePct) / 100
		for _, h := range rcptHashes {
			if sp.perRcpt[h]+size > share {
				return false
			}
		}
	}
	return true
}

// Put writes one sealed message atomically and wakes waiters.
func (sp *Spool) Put(sealed []byte, rcptHashes []string) (string, error) {
	id := newSpoolID(time.Now())
	tmp := filepath.Join(sp.dir, id+".tmp")
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return "", err
	}
	final := filepath.Join(sp.dir, id+".sealed")
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	sp.mu.Lock()
	sp.entries[id] = &spoolEntry{id: id, size: int64(len(sealed)), rcptHashes: rcptHashes}
	sp.order = append(sp.order, id) // Put ids are minted now — already newest
	sp.bytes += int64(len(sealed))
	for _, h := range rcptHashes {
		sp.perRcpt[h] += int64(len(sealed))
	}
	waiters := sp.waiters
	sp.waiters = nil
	sp.mu.Unlock()
	for _, w := range waiters {
		close(w)
	}
	return id, nil
}

// Batch returns up to maxCount/maxBytes of the OLDEST entries with
// their sealed contents, plus whether more remain.
func (sp *Spool) Batch(maxCount int, maxBytes int64) (ids []string, blobs [][]byte, more bool, err error) {
	sp.mu.Lock()
	var picked []string
	var total int64
	for _, id := range sp.order {
		e, ok := sp.entries[id]
		if !ok {
			continue
		}
		if len(picked) >= maxCount || (total > 0 && total+e.size > maxBytes) {
			more = true
			break
		}
		picked = append(picked, id)
		total += e.size
	}
	sp.mu.Unlock()
	for _, id := range picked {
		raw, rerr := os.ReadFile(filepath.Join(sp.dir, id+".sealed"))
		if rerr != nil {
			continue // acked concurrently — the caller just sees a smaller batch
		}
		ids = append(ids, id)
		blobs = append(blobs, raw)
	}
	return ids, blobs, more, nil
}

// Ack deletes delivered entries and releases their share accounting.
func (sp *Spool) Ack(ids []string) {
	sp.mu.Lock()
	kept := sp.order[:0]
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	for _, id := range sp.order {
		e, ok := sp.entries[id]
		if !ok {
			continue
		}
		if !drop[id] {
			kept = append(kept, id)
			continue
		}
		sp.bytes -= e.size
		for _, h := range e.rcptHashes {
			if sp.perRcpt[h] -= e.size; sp.perRcpt[h] <= 0 {
				delete(sp.perRcpt, h)
			}
		}
		delete(sp.entries, id)
	}
	sp.order = kept
	sp.mu.Unlock()
	for _, id := range ids {
		_ = os.Remove(filepath.Join(sp.dir, id+".sealed"))
	}
}

// Wait blocks until a Put lands or the timeout passes (the long-poll
// primitive). Returns immediately when entries already exist.
func (sp *Spool) Wait(timeout time.Duration) {
	sp.mu.Lock()
	if len(sp.entries) > 0 {
		sp.mu.Unlock()
		return
	}
	w := make(chan struct{})
	sp.waiters = append(sp.waiters, w)
	sp.mu.Unlock()
	select {
	case <-w:
	case <-time.After(timeout):
	}
}
