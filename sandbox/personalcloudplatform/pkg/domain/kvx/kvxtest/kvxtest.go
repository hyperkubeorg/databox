// Package kvxtest is a test-only, in-memory fake of the databox HTTP API
// behind an httptest TLS server, so domain and kernel unit tests can
// exercise REAL store code — Get/Set/Delete/List, OCC transactions,
// whole-object blob put/get/delete (phase 3, mail), and ranged blob
// reads (phase 6, the ID3 indexer) — without a running cluster. It
// implements exactly the wire shapes pkg/client sends and nothing more;
// spliced blob reads and watch paths belong to live smokes against a
// real node.
package kvxtest

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/pkg/kv"
)

// New starts a fake single-node databox and returns a client connected
// to it. The server stops with the test.
func New(t *testing.T) *client.Client {
	t.Helper()
	srv := httptest.NewTLSServer(&fake{data: map[string]entry{}})
	t.Cleanup(srv.Close)
	c, err := client.New(client.Options{
		Endpoint: strings.TrimPrefix(srv.URL, "https://"),
		// httptest's cert is self-signed and per-process; trusting it
		// blindly is exactly right here.
		OnUnknownCert: func(string, *x509.Certificate) bool { return true },
	})
	if err != nil {
		t.Fatalf("kvxtest client: %v", err)
	}
	return c
}

// entry is one stored key.
type entry struct {
	value []byte
	rev   uint64
}

// fake is the in-memory store + HTTP surface. One mutex serializes
// everything — commits are atomic by construction.
type fake struct {
	mu    sync.Mutex
	data  map[string]entry
	blobs map[string][]byte
	rev   uint64
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/kv/"):
		f.kv(w, r, strings.TrimPrefix(r.URL.Path, "/api/v1/kv"))
	case strings.HasPrefix(r.URL.Path, "/api/v1/blobs/"):
		f.blob(w, r, strings.TrimPrefix(r.URL.Path, "/api/v1/blobs"))
	case r.URL.Path == "/api/v1/list":
		f.list(w, r)
	case r.URL.Path == "/api/v1/delete-range":
		f.deleteRange(w, r)
	case r.URL.Path == "/api/v1/tx/commit":
		f.commit(w, r)
	case r.URL.Path == "/api/v1/locks/acquire":
		// Single-process tests never contend — always grant (the media
		// scanner's per-folder lock rides this).
		f.rev++
		writeJSON(w, map[string]uint64{"fencing": f.rev})
	case r.URL.Path == "/api/v1/locks/release":
		writeJSON(w, map[string]any{})
	default:
		jsonError(w, http.StatusNotFound, "NotFound: no such endpoint")
	}
}

// blob serves whole-object put/get/delete (what the mail pipeline
// needs; ranged reads stay live-smoke territory).
func (f *fake) blob(w http.ResponseWriter, r *http.Request, key string) {
	if f.blobs == nil {
		f.blobs = map[string][]byte{}
	}
	switch r.Method {
	case http.MethodPut:
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "BadRequest: "+err.Error())
			return
		}
		f.blobs[key] = raw
		writeJSON(w, map[string]any{})
	case http.MethodGet:
		raw, found := f.blobs[key]
		if !found {
			jsonError(w, http.StatusNotFound, "NotFound: "+key)
			return
		}
		// Ranged reads (?offset=&length=) serve the ID3 indexer's tag
		// windows — the same query shape client.GetBlobRange sends.
		if off := r.URL.Query().Get("offset"); off != "" {
			offset, _ := strconv.ParseInt(off, 10, 64)
			if offset < 0 || offset > int64(len(raw)) {
				offset = int64(len(raw))
			}
			raw = raw[offset:]
			if l := r.URL.Query().Get("length"); l != "" {
				length, _ := strconv.ParseInt(l, 10, 64)
				if length >= 0 && length < int64(len(raw)) {
					raw = raw[:length]
				}
			}
		}
		_, _ = w.Write(raw)
	case http.MethodHead:
		raw, found := f.blobs[key]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(f.blobs, key)
		writeJSON(w, map[string]any{})
	default:
		jsonError(w, http.StatusMethodNotAllowed, "BadRequest: method")
	}
}

// kv serves single-key get/put/delete, including the transactional get
// shape (?tx=1: 200 with a found flag plus gid/shard_rev). The real
// server's `.databox/` system view addresses keys WITHOUT the leading
// slash (the mux var strips it) while List prefixes carry the bare
// ".databox/" form — mirror that so clusterview fixtures behave.
func (f *fake) kv(w http.ResponseWriter, r *http.Request, key string) {
	if strings.HasPrefix(key, "/.databox/") {
		key = key[1:]
	}
	switch r.Method {
	case http.MethodGet:
		e, found := f.data[key]
		if r.URL.Query().Get("tx") == "1" {
			writeJSON(w, map[string]any{
				"found": found, "value": e.value, "rev": e.rev,
				"gid": 1, "shard_rev": f.rev,
			})
			return
		}
		if !found {
			jsonError(w, http.StatusNotFound, "NotFound: "+key)
			return
		}
		writeJSON(w, client.KVEntry{Key: key, Value: e.value, Rev: e.rev})
	case http.MethodPut:
		var in struct {
			Value []byte `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			jsonError(w, http.StatusBadRequest, "BadRequest: "+err.Error())
			return
		}
		f.rev++
		f.data[key] = entry{value: in.Value, rev: f.rev}
		writeJSON(w, map[string]uint64{"rev": f.rev})
	case http.MethodDelete:
		delete(f.data, key)
		writeJSON(w, map[string]any{})
	default:
		jsonError(w, http.StatusMethodNotAllowed, "BadRequest: method")
	}
}

// list serves prefix scans; the presence of ?pins= selects the versioned
// shape that returns shard_revs for transactional pinning. Blob keys
// appear too (Blob: true), as on the real server — a blob is visible
// ⇔ its manifest committed into the KV space (the mail GC sweep
// enumerates blobs this way).
func (f *fake) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix, cursor := q.Get("prefix"), q.Get("cursor")
	limit, _ := strconv.Atoi(q.Get("limit"))
	keys := make([]string, 0, len(f.data))
	for k := range f.data {
		if strings.HasPrefix(k, prefix) && (cursor == "" || k >= cursor) {
			keys = append(keys, k)
		}
	}
	blobKeys := map[string]bool{}
	for k := range f.blobs {
		if _, shadowed := f.data[k]; shadowed {
			continue
		}
		if strings.HasPrefix(k, prefix) && (cursor == "" || k >= cursor) {
			keys = append(keys, k)
			blobKeys[k] = true
		}
	}
	sort.Strings(keys)
	next := ""
	if limit > 0 && len(keys) > limit {
		next = keys[limit]
		keys = keys[:limit]
	}
	entries := make([]client.KVEntry, 0, len(keys))
	for _, k := range keys {
		if blobKeys[k] {
			entries = append(entries, client.KVEntry{Key: k, Blob: true})
			continue
		}
		e := f.data[k]
		entries = append(entries, client.KVEntry{Key: k, Value: e.value, Rev: e.rev})
	}
	out := map[string]any{"entries": entries, "next_cursor": next}
	if q.Has("pins") {
		out["shard_revs"] = map[uint64]uint64{1: f.rev}
	}
	writeJSON(w, out)
}

// deleteRange removes every key in [start, end) ("" end = unbounded) —
// kvx.DeletePrefix and the retention sweeps ride this.
func (f *fake) deleteRange(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "BadRequest: "+err.Error())
		return
	}
	for k := range f.data {
		if k >= in.Start && (in.End == "" || k < in.End) {
			delete(f.data, k)
		}
	}
	writeJSON(w, map[string]any{})
}

// commit validates the transaction's read set against current revisions
// (0 = "did not exist") and applies its writes atomically — real OCC
// semantics, minus sharding.
func (f *fake) commit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reads  map[string]uint64 `json:"reads"`
		Writes []kv.TxWrite      `json:"writes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "BadRequest: "+err.Error())
		return
	}
	for key, want := range in.Reads {
		if got := f.data[key].rev; got != want {
			jsonError(w, http.StatusConflict, fmt.Sprintf("Conflict: %s rev %d != %d", key, got, want))
			return
		}
	}
	for _, wr := range in.Writes {
		if wr.Delete {
			delete(f.data, wr.Key)
			continue
		}
		f.rev++
		f.data[wr.Key] = entry{value: wr.Value, rev: f.rev}
	}
	writeJSON(w, map[string]any{})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
