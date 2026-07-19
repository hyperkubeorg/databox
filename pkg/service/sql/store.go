// store.go defines the narrow key-value interface the SQL executor needs
// and two implementations: a thin adapter over the production cluster
// client, and an in-memory store used by the dialect-conformance tests
// (§13). Depending on an interface rather than the concrete
// client lets the whole engine — DDL, DML, SELECT, aggregation — run in a
// unit test without a live cluster, which is what makes porting chai's
// sqltests corpus practical.
package sql

import (
	"context"
	"sort"
	"strings"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// kvPair is one key/value entry returned by a scan.
type kvPair struct {
	Key   string
	Value []byte
}

// kvStore is the cluster surface the executor uses. Its methods mirror the
// databox KV API (§9) at exactly the granularity the SQL layer needs.
type kvStore interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
	DeleteRange(ctx context.Context, start, end string) error
	List(ctx context.Context, prefix, cursor string, limit int) ([]kvPair, string, error)
	NewTx() kvTx
}

// kvTx is one optimistic transaction (§10): staged reads record revisions,
// staged writes apply atomically at commit, and a conflict aborts the whole
// set.
type kvTx interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(key string, value []byte)
	Delete(key string)
	Commit(ctx context.Context) error
}

// --- production adapter over pkg/client --------------------------------------

// clientStore adapts *client.Client to kvStore.
type clientStore struct{ c *client.Client }

// NewClientStore wraps a cluster client as a kvStore for the executor.
func NewClientStore(c *client.Client) kvStore { return &clientStore{c: c} }

func (s *clientStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	e, found, err := s.c.Get(ctx, key)
	return e.Value, found, err
}

func (s *clientStore) Set(ctx context.Context, key string, value []byte) error {
	_, err := s.c.Set(ctx, key, value)
	return err
}

func (s *clientStore) Delete(ctx context.Context, key string) error { return s.c.Delete(ctx, key) }

func (s *clientStore) DeleteRange(ctx context.Context, start, end string) error {
	return s.c.DeleteRange(ctx, start, end)
}

func (s *clientStore) List(ctx context.Context, prefix, cursor string, limit int) ([]kvPair, string, error) {
	entries, next, err := s.c.List(ctx, prefix, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	out := make([]kvPair, len(entries))
	for i, e := range entries {
		out[i] = kvPair{Key: e.Key, Value: e.Value}
	}
	return out, next, nil
}

func (s *clientStore) NewTx() kvTx { return &clientTx{tx: s.c.NewTx()} }

// clientTx adapts *client.Tx to kvTx.
type clientTx struct{ tx *client.Tx }

func (t *clientTx) Get(ctx context.Context, key string) ([]byte, bool, error) {
	return t.tx.Get(ctx, key)
}
func (t *clientTx) Set(key string, value []byte) { t.tx.Set(key, value) }
func (t *clientTx) Delete(key string)            { t.tx.Delete(key) }
func (t *clientTx) Commit(ctx context.Context) error {
	return t.tx.Commit(ctx)
}

// --- in-memory store (tests) -------------------------------------------------

// memStore is a simple ordered map used by conformance tests. It provides
// the same semantics the executor relies on (ordered range scans, atomic
// multi-key commit) without a network or raft.
type memStore struct {
	m map[string][]byte
}

// NewMemStore builds an empty in-memory kvStore.
func NewMemStore() kvStore { return &memStore{m: map[string][]byte{}} }

func (s *memStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	v, ok := s.m[key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), v...), true, nil
}

func (s *memStore) Set(_ context.Context, key string, value []byte) error {
	s.m[key] = append([]byte(nil), value...)
	return nil
}

func (s *memStore) Delete(_ context.Context, key string) error {
	delete(s.m, key)
	return nil
}

func (s *memStore) DeleteRange(_ context.Context, start, end string) error {
	for k := range s.m {
		if k >= start && (end == "" || k < end) {
			delete(s.m, k)
		}
	}
	return nil
}

func (s *memStore) List(_ context.Context, prefix, cursor string, limit int) ([]kvPair, string, error) {
	var keys []string
	for k := range s.m {
		if strings.HasPrefix(k, prefix) && (cursor == "" || k > cursor) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]kvPair, len(keys))
	for i, k := range keys {
		out[i] = kvPair{Key: k, Value: append([]byte(nil), s.m[k]...)}
	}
	next := ""
	if limit > 0 && len(keys) == limit {
		next = keys[len(keys)-1]
	}
	return out, next, nil
}

func (s *memStore) NewTx() kvTx {
	return &memTx{s: s, sets: map[string][]byte{}, dels: map[string]bool{}}
}

// memTx buffers writes and applies them on Commit. The in-memory store is
// single-threaded in tests, so there are no conflicts to detect.
type memTx struct {
	s    *memStore
	sets map[string][]byte
	dels map[string]bool
}

func (t *memTx) Get(_ context.Context, key string) ([]byte, bool, error) {
	if t.dels[key] {
		return nil, false, nil
	}
	if v, ok := t.sets[key]; ok {
		return append([]byte(nil), v...), true, nil
	}
	v, ok := t.s.m[key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), v...), true, nil
}

func (t *memTx) Set(key string, value []byte) {
	delete(t.dels, key)
	t.sets[key] = append([]byte(nil), value...)
}

func (t *memTx) Delete(key string) {
	delete(t.sets, key)
	t.dels[key] = true
}

func (t *memTx) Commit(_ context.Context) error {
	for k := range t.dels {
		delete(t.s.m, k)
	}
	for k, v := range t.sets {
		t.s.m[k] = v
	}
	return nil
}
