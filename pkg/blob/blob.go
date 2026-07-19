// Package blob implements databox's blob storage engine
// (§11, §12): the data plane for values too large for the
// KV path.
//
// # Separation from raft
//
// Chunk bytes NEVER travel through raft. Raft replicates only the blob's
// *manifest* (a small JSON chunk map stored at the blob's key with the
// Blob flag). Chunk data moves node-to-node over the internal HTTPS RPC
// with its own connection pool, so a 10 GB upload cannot delay KV
// consensus by a microsecond.
//
// # On-disk layout
//
// Chunks are content-addressed files:
//
//	<data-dir>/chunks/<first two hex of sha256>/<full sha256 hex>
//
// Content addressing gives three properties the design leans on hard:
// chunks are immutable, identical data deduplicates itself, and any copy
// can be verified by rehashing (used on every read and by the repair loop).
//
// # Durability policies (§12)
//
//   - Small blobs (≤ 1 chunk): plain replication, default 2 copies.
//   - Large blobs: Reed-Solomon rs-4-2 erasure coding — each stripe of 4
//     data chunks gains 2 parity chunks; any 4 of the 6 reconstruct the
//     stripe. Falls back to replication when the cluster has too few
//     nodes to spread an EC stripe usefully.
//   - Per-key overrides come from stored policy rules (policy.go): the
//     most specific path rule wins over the built-in defaults above.
//
// # Placement quorum (§11 write path)
//
// A write succeeds only when its placement targets are actually achieved:
// replica mode needs min(replicas, active nodes) distinct-node copies; EC
// mode needs every shard stored, on all-distinct nodes whenever the
// cluster is large enough. Falling short fails the upload with a
// retryable QuorumError — the manifest never commits, so a "2-replica"
// blob can never quietly exist with one copy.
//
// # Visibility guarantee (documented in docs/consistency.md)
//
// The manifest is written to the KV store only after every chunk reports
// durable on its target nodes. A blob is visible ⇔ its manifest committed.
// Failed uploads leave orphan chunks, never partial blobs; orphans are
// garbage-collected by the repair loop.
package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/klauspost/reedsolomon"
)

// Manifest is the KV-stored description of one blob. It is small (a few
// hundred bytes per GB of blob) and travels through raft like any value.
type Manifest struct {
	Size   int64  `json:"size"`             // total blob length in bytes
	SHA256 string `json:"sha256"`           // whole-blob hash, or composite (see Composite)
	Mode   string `json:"mode"`             // "replica" | "ec"
	Chunks []Ref  `json:"chunks,omitempty"` // replica mode: ordered data chunks
	// EC mode: stripes of DataShards+ParityShards chunk refs each.
	DataShards   int      `json:"data_shards,omitempty"`
	ParityShards int      `json:"parity_shards,omitempty"`
	Stripes      []Stripe `json:"stripes,omitempty"`
	ContentType  string   `json:"content_type,omitempty"` // for HTTP/S3 serving
	// HashState is the marshaled SHA-256 midstate after hashing the whole
	// blob. Append resumes the running hash from here instead of
	// re-reading gigabytes just to keep SHA256 truthful.
	HashState []byte `json:"hash_state,omitempty"`
	// Composite marks SHA256 as a COMPOSITE hash rather than a plain
	// whole-blob digest. Splice (splice.go) produces composite manifests:
	// recomputing a true whole-blob hash would require re-reading every
	// byte, defeating the point of a metadata-only splice, so — exactly
	// like S3's multipart ETags — the hash is recorded per spliced source.
	//
	// Field format, byte level:
	//
	//   composite=false (or absent): sha256 is 64 lowercase hex chars —
	//     the SHA-256 of the complete blob content. Every manifest written
	//     before splice existed decodes with composite=false and keeps its
	//     exact prior meaning (full backward compatibility).
	//
	//   composite=true: sha256 is "<h1>,<h2>,...,<hn>" — the comma-joined
	//     64-hex SHA-256 digests of each spliced source's content, in
	//     splice order. Splicing a composite source inlines its component
	//     list (never nests), so components always cover the whole blob
	//     left to right. hash_state is always empty when composite=true.
	//
	// Content integrity does not weaken under a composite hash: every
	// chunk is content-addressed and verified on every read; the composite
	// components additionally pin each source's whole content. Only the
	// single-digest convenience is traded away, and Append is refused on
	// composite manifests (append.go) because there is no whole-blob
	// midstate to resume.
	Composite bool `json:"composite,omitempty"`
}

// Ref points at one stored chunk and the nodes that hold copies.
type Ref struct {
	Hash  string   `json:"hash"`
	Size  int64    `json:"size"`
	Nodes []uint64 `json:"nodes"`
}

// Stripe is one erasure-coding unit: DataShards data chunks followed by
// ParityShards parity chunks. DataLen is the real byte length of the
// stripe before shard padding, so reads can trim.
type Stripe struct {
	Shards  []Ref `json:"shards"`
	DataLen int64 `json:"data_len"`
}

// ChunkStore manages the node-local chunk directory.
type ChunkStore struct {
	dir string
	mu  sync.Mutex // serializes directory creation only; files are immutable
}

// NewChunkStore prepares the chunk directory under dataDir.
func NewChunkStore(dataDir string) (*ChunkStore, error) {
	dir := filepath.Join(dataDir, "chunks")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create chunk dir: %w", err)
	}
	return &ChunkStore{dir: dir}, nil
}

// path maps a chunk hash to its file location.
func (cs *ChunkStore) path(hash string) string {
	if len(hash) < 2 {
		return filepath.Join(cs.dir, "xx", hash)
	}
	return filepath.Join(cs.dir, hash[:2], hash)
}

// Put stores chunk data (verifying it matches the claimed hash) and
// fsyncs before returning — a chunk acknowledged is a chunk on disk.
func (cs *ChunkStore) Put(hash string, data []byte) error {
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hash {
		return fmt.Errorf("chunk data does not match hash %s", hash)
	}
	p := cs.path(hash)
	if _, err := os.Stat(p); err == nil {
		return nil // content-addressed: already have it
	}
	cs.mu.Lock()
	err := os.MkdirAll(filepath.Dir(p), 0o750)
	cs.mu.Unlock()
	if err != nil {
		return err
	}
	// Write to a temp file then rename, so a crash never leaves a
	// half-written chunk under its final (trusted) name.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Get reads a chunk and verifies its content hash before returning it.
func (cs *ChunkStore) Get(hash string) ([]byte, error) {
	data, err := os.ReadFile(cs.path(hash))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hash {
		// Bit rot or tampering: report loudly. The read path falls back
		// to another holder, and the repair loop's scrub pass deletes the
		// corrupt copy so re-replication restores a good one here.
		return nil, fmt.Errorf("chunk %s failed hash verification (corrupt on disk)", hash)
	}
	return data, nil
}

// Has reports whether the chunk exists locally (without verifying).
func (cs *ChunkStore) Has(hash string) bool {
	_, err := os.Stat(cs.path(hash))
	return err == nil
}

// Delete removes a chunk file (used by GC after a blob's refs drop).
func (cs *ChunkStore) Delete(hash string) error {
	err := os.Remove(cs.path(hash))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ChunkInfo describes one locally stored chunk for the repair/GC loop.
type ChunkInfo struct {
	Hash    string
	Size    int64
	ModTime int64 // unix seconds; GC only touches chunks old enough to
	// be certain no in-flight upload still references them
}

// List enumerates every chunk on local disk. Used by the GC pass to find
// orphans (chunks no manifest references) and by capacity metrics.
func (cs *ChunkStore) List() ([]ChunkInfo, error) {
	var out []ChunkInfo
	entries, err := os.ReadDir(cs.dir)
	if err != nil {
		return nil, err
	}
	for _, sub := range entries {
		if !sub.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(cs.dir, sub.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			info, err := f.Info()
			if err != nil || f.IsDir() {
				continue
			}
			// Skip temp files from in-progress writes.
			if len(f.Name()) < 8 || f.Name()[0] == '.' {
				continue
			}
			out = append(out, ChunkInfo{Hash: f.Name(), Size: info.Size(), ModTime: info.ModTime().Unix()})
		}
	}
	return out, nil
}

// Peers abstracts "other nodes" for the engine: how to push, fetch, and
// probe chunks remotely, and which nodes are available. Implemented by
// pkg/server over the internal RPC.
type Peers interface {
	// ActiveNodes lists node IDs currently usable for placement,
	// excluding draining/removed nodes. Includes the local node.
	ActiveNodes() []uint64
	// Self is the local node ID.
	Self() uint64
	// PushChunk stores a chunk on a remote node.
	PushChunk(node uint64, hash string, data []byte) error
	// FetchChunk retrieves a chunk from a remote node.
	FetchChunk(node uint64, hash string) ([]byte, error)
	// HasChunk probes a remote node for a chunk.
	HasChunk(node uint64, hash string) bool
}

// Engine is the blob data plane for one node.
type Engine struct {
	Store     *ChunkStore
	Peers     Peers
	ChunkSize int // stripe/data chunk size, default 8 MiB (§11)
	// Replicas is the built-in default copy count for replica-mode blobs
	// (§12: default 2). Stored policy rules override it per key.
	Replicas int
	// Policies resolves per-key durability overrides (§12). Nil means
	// built-in defaults only.
	Policies PolicySource
	// Limiter paces background repair/scrub IO (§11: "rate-limited to
	// protect foreground traffic"). Foreground reads and writes never
	// consult it. Nil means unlimited.
	Limiter *RateLimiter

	// scrubCursor tracks the incremental at-rest scrub's position so each
	// ScrubNext batch resumes where the last one stopped (scrub.go).
	scrubMu     sync.Mutex
	scrubCursor string
}

// QuorumError reports a chunk placement that could not reach its required
// copy count or node spread. The message carries the InsufficientReplicas
// code so the API layer can surface it as retryable: the upload failed
// cleanly (no manifest committed, orphan chunks are GC'd) and a retry may
// succeed once cluster membership settles.
type QuorumError struct {
	Hash string // chunk (or shard) that could not be placed
	Got  int    // distinct-node copies achieved
	Want int    // distinct-node copies required
}

func (e *QuorumError) Error() string {
	return fmt.Sprintf("InsufficientReplicas: chunk %s stored on %d of %d required nodes, retry",
		e.Hash, e.Got, e.Want)
}

// Write consumes the stream and stores it under key's durability policy,
// returning the manifest to commit into the KV store. The caller commits
// the manifest; if anything here fails, no manifest exists and the blob
// simply never becomes visible (§11 write path).
func (e *Engine) Write(key string, r io.Reader, contentType string) (*Manifest, error) {
	// Buffer the whole blob through fixed-size chunks, hashing as we go.
	whole := sha256.New()
	var chunks [][]byte
	var total int64
	buf := make([]byte, e.ChunkSize)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			total += int64(n)
			whole.Write(buf[:n])
			chunks = append(chunks, append([]byte(nil), buf[:n]...))
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	m := &Manifest{
		Size:        total,
		SHA256:      hex.EncodeToString(whole.Sum(nil)),
		ContentType: contentType,
	}
	// Persist the hash midstate so future appends can extend the running
	// SHA-256 without re-reading the existing data (crypto/sha256's
	// digest implements encoding.BinaryMarshaler exactly for this).
	if bm, ok := whole.(encoding.BinaryMarshaler); ok {
		if st, err := bm.MarshalBinary(); err == nil {
			m.HashState = st
		}
	}
	// Policy decision (§12): single-chunk blobs replicate; larger blobs
	// erasure-code when the key's policy allows it and the cluster can
	// spread a stripe usefully, otherwise fall back to replication.
	pol := e.PolicyFor(key)
	nodes := e.Peers.ActiveNodes()
	if len(chunks) <= 1 || len(nodes) < 3 || !pol.ECEnabled {
		m.Mode = "replica"
		for _, c := range chunks {
			ref, err := e.storeReplicated(c, pol.Replicas)
			if err != nil {
				return nil, err
			}
			m.Chunks = append(m.Chunks, ref)
		}
		return m, nil
	}
	m.Mode = "ec"
	m.DataShards, m.ParityShards = pol.DataShards, pol.ParityShards
	// Each stripe erasure-codes DataShards consecutive chunks. The last
	// stripe may cover fewer real chunks; shards are padded to equal
	// size as Reed-Solomon requires.
	for i := 0; i < len(chunks); i += m.DataShards {
		end := i + m.DataShards
		if end > len(chunks) {
			end = len(chunks)
		}
		stripe, err := e.storeStripe(chunks[i:end], m.DataShards, m.ParityShards, nodes)
		if err != nil {
			return nil, err
		}
		m.Stripes = append(m.Stripes, stripe)
	}
	return m, nil
}

// storeReplicated saves a chunk locally plus remote copies until the
// effective quorum — min(replicas, active nodes) distinct nodes — is met.
// Falling short fails the write with a retryable QuorumError instead of
// quietly committing fewer copies than the policy promises.
func (e *Engine) storeReplicated(data []byte, replicas int) (Ref, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	ref := Ref{Hash: hash, Size: int64(len(data))}
	nodes := e.Peers.ActiveNodes()
	want := max(min(replicas, len(nodes)), 1)
	// Local copy first — the upload node always keeps one replica so a
	// success response implies at least local durability.
	if err := e.Store.Put(hash, data); err != nil {
		return ref, err
	}
	ref.Nodes = append(ref.Nodes, e.Peers.Self())
	// Additional copies on other active nodes, in node-list order. A
	// failed push moves on to the next candidate; the quorum check below
	// is what decides success.
	for _, n := range nodes {
		if len(ref.Nodes) >= want {
			break
		}
		if n == e.Peers.Self() {
			continue
		}
		if err := e.Peers.PushChunk(n, hash, data); err != nil {
			continue
		}
		ref.Nodes = append(ref.Nodes, n)
	}
	if len(ref.Nodes) < want {
		return ref, &QuorumError{Hash: hash, Got: len(ref.Nodes), Want: want}
	}
	return ref, nil
}

// storeStripe erasure-codes up to dataShards chunks into a full shard set
// and places every shard with maximum node spread:
//
//   - active nodes ≥ shard count: every shard lands on a distinct node —
//     this is the placement the rs-4-2 "survives 2 node failures" promise
//     assumes, so failing to achieve it fails the upload (retryable via
//     QuorumError) rather than silently co-locating shards.
//   - fewer nodes: shards spread as evenly as possible; every shard must
//     still land on some node or the upload fails.
func (e *Engine) storeStripe(data [][]byte, dataShards, parityShards int, nodes []uint64) (Stripe, error) {
	// Real byte length of the stripe (for trimming on read).
	var dataLen int64
	for _, d := range data {
		dataLen += int64(len(d))
	}
	// Pad the shard set: all shards must be the same length, and we may
	// have fewer real chunks than dataShards in the final stripe.
	shardSize := 0
	for _, d := range data {
		if len(d) > shardSize {
			shardSize = len(d)
		}
	}
	shards := make([][]byte, dataShards+parityShards)
	for i := 0; i < dataShards; i++ {
		shards[i] = make([]byte, shardSize)
		if i < len(data) {
			copy(shards[i], data[i])
		}
	}
	for i := dataShards; i < len(shards); i++ {
		shards[i] = make([]byte, shardSize)
	}
	enc, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		return Stripe{}, err
	}
	if err := enc.Encode(shards); err != nil {
		return Stripe{}, fmt.Errorf("reed-solomon encode: %w", err)
	}
	// Distinct placement is mandatory when the cluster can provide it.
	distinct := len(nodes) >= len(shards)
	used := map[uint64]int{} // node → shards of THIS stripe it holds
	stripe := Stripe{DataLen: dataLen}
	for k, shard := range shards {
		sum := sha256.Sum256(shard)
		hash := hex.EncodeToString(sum[:])
		target, ok := e.placeShard(hash, shard, nodes, k, used, distinct)
		if !ok {
			return Stripe{}, &QuorumError{Hash: hash, Got: 0, Want: 1}
		}
		used[target]++
		stripe.Shards = append(stripe.Shards, Ref{Hash: hash, Size: int64(len(shard)), Nodes: []uint64{target}})
	}
	return stripe, nil
}

// placeShard stores one shard on the best available node. Candidates are
// tried in round-robin order starting at the shard's index (even spread),
// preferring nodes that hold no shard of this stripe yet. When distinct
// placement is required (cluster ≥ shard count) a shard that cannot land
// on an unused node fails outright — co-locating instead would quietly
// weaken the stripe's fault tolerance while the manifest still claims
// full rs-N-M durability.
func (e *Engine) placeShard(hash string, shard []byte, nodes []uint64, k int, used map[uint64]int, distinct bool) (uint64, bool) {
	try := func(n uint64) bool {
		if n == e.Peers.Self() {
			return e.Store.Put(hash, shard) == nil
		}
		return e.Peers.PushChunk(n, hash, shard) == nil
	}
	// First pass: nodes not yet holding a shard of this stripe.
	for i := 0; i < len(nodes); i++ {
		n := nodes[(k+i)%len(nodes)]
		if used[n] > 0 {
			continue
		}
		if try(n) {
			return n, true
		}
	}
	if distinct {
		return 0, false
	}
	// Small cluster: shards must double up. Reuse the least-loaded nodes
	// first so the spread stays as even as the cluster allows.
	order := append([]uint64(nil), nodes...)
	sort.Slice(order, func(i, j int) bool { return used[order[i]] < used[order[j]] })
	for _, n := range order {
		if try(n) {
			return n, true
		}
	}
	return 0, false
}

// Read streams the blob described by the manifest to w, verifying every
// chunk hash and reconstructing EC stripes when shards are missing.
func (e *Engine) Read(m *Manifest, w io.Writer) error {
	switch m.Mode {
	case "replica":
		for _, ref := range m.Chunks {
			data, err := e.fetch(ref)
			if err != nil {
				return err
			}
			if _, err := w.Write(data); err != nil {
				return err
			}
		}
		return nil
	case "ec":
		enc, err := reedsolomon.New(m.DataShards, m.ParityShards)
		if err != nil {
			return err
		}
		for _, stripe := range m.Stripes {
			if err := e.readStripe(enc, m.DataShards, stripe, w); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown blob mode %q", m.Mode)
	}
}

// readStripe gathers shards (tolerating up to parity-count losses),
// reconstructs if needed, and writes the stripe's real data bytes.
func (e *Engine) readStripe(enc reedsolomon.Encoder, dataShards int, stripe Stripe, w io.Writer) error {
	shards := make([][]byte, len(stripe.Shards))
	available := 0
	for i, ref := range stripe.Shards {
		data, err := e.fetch(ref)
		if err == nil {
			shards[i] = data
			available++
		}
	}
	if available < dataShards {
		return fmt.Errorf("stripe unreadable: only %d of %d shards available (need %d)",
			available, len(stripe.Shards), dataShards)
	}
	// Reconstruct any missing data shards from parity.
	if err := enc.Reconstruct(shards); err != nil {
		return fmt.Errorf("reed-solomon reconstruct: %w", err)
	}
	// Concatenate data shards and trim padding to the stripe's true length.
	var buf bytes.Buffer
	for i := 0; i < dataShards; i++ {
		buf.Write(shards[i])
	}
	_, err := w.Write(buf.Bytes()[:stripe.DataLen])
	return err
}

// fetch returns a chunk from the nearest holder: local disk first, then
// each recorded node, then (last resort) every active node — placement
// may have drifted since the manifest was written.
func (e *Engine) fetch(ref Ref) ([]byte, error) {
	if e.Store.Has(ref.Hash) {
		if data, err := e.Store.Get(ref.Hash); err == nil {
			return data, nil
		}
		// Local copy corrupt: fall through to remote copies.
	}
	tried := map[uint64]bool{e.Peers.Self(): true}
	for _, n := range ref.Nodes {
		if tried[n] {
			continue
		}
		tried[n] = true
		if data, err := e.Peers.FetchChunk(n, ref.Hash); err == nil {
			return data, nil
		}
	}
	for _, n := range e.Peers.ActiveNodes() {
		if tried[n] {
			continue
		}
		if data, err := e.Peers.FetchChunk(n, ref.Hash); err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("chunk %s unavailable on any node", ref.Hash)
}

// AllRefs enumerates every chunk reference in a manifest — used by the
// repair loop and by delete/GC.
func (m *Manifest) AllRefs() []Ref {
	var out []Ref
	out = append(out, m.Chunks...)
	for _, s := range m.Stripes {
		out = append(out, s.Shards...)
	}
	return out
}

// Decode parses a manifest from its KV record value.
func Decode(value []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(value, &m); err != nil {
		return nil, fmt.Errorf("parse blob manifest: %w", err)
	}
	return &m, nil
}

// Encode renders the manifest for KV storage.
func (m *Manifest) Encode() []byte {
	b, _ := json.Marshal(m)
	return b
}

// ServePeerChunk handles the internal RPC endpoints other nodes use to
// push/fetch/probe chunks on this node:
//
//	PUT  /internal/chunk/<hash>   store chunk body
//	GET  /internal/chunk/<hash>   return chunk body
//	HEAD /internal/chunk/<hash>   200 if present, 404 if not
func (e *Engine) ServePeerChunk(w http.ResponseWriter, r *http.Request, hash string) {
	switch r.Method {
	case http.MethodPut:
		data, err := io.ReadAll(io.LimitReader(r.Body, int64(e.ChunkSize)*2))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := e.Store.Put(hash, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		data, err := e.Store.Get(hash)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data)
	case http.MethodHead:
		if e.Store.Has(hash) {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
