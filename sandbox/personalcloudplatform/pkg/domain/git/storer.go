// storer.go — the databox-backed go-git storer (§6.2), per repo:
//
//   - refs at /pcp/git/refs/<repoID>/<refname> (the raw refname is the
//     key suffix — validRefName keeps it key-charset safe and List-order
//     preserving); HEAD is symbolic to the repo record's default branch;
//   - loose objects, zlib-compressed with a one-line "<type> <size>\n"
//     header so type/size stat needs no full read: < objBlobThreshold
//     bytes ENCODED goes to KV at /pcp/git/obj/<repoID>/<sha>, larger
//     goes to a blob at /pcp/git/objblob/<repoID>/<sha> (ranged reads
//     serve the header);
//   - lookup misses walk the forkOf chain READ-ONLY (§5.3 alternates
//     model, depth-capped) — writes always land in the leaf repo;
//   - unpack writes are buffered and flushed in chunked transactions
//     (pure Sets — no read set, so flushes never conflict) sized under
//     databox's 4 MiB value / 8 MiB JSON-body caps; every written key is
//     remembered so an aborted push can be swept and refunded (§6.5).
//
// The storer implements exactly what upload-pack/receive-pack/log/tree
// walks need; shallow/index/config/module methods are honest stubs (git
// data on the wire never touches them).
package git

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	gogitcfg "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	gitstorer "github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// objBlobThreshold routes an ENCODED loose object: below it → KV value,
// at/above → blob (§6.2's 256 KiB split, comfortably under databox's
// 4 MiB KV value cap).
const objBlobThreshold = 256 << 10

// Flush chunking: one flush transaction stays under databox's JSON
// body cap (values ride base64 inside the commit body).
const (
	flushMaxBytes = 2 << 20
	flushMaxKeys  = 200
)

// readCacheMax bounds the encoded-object read/write-back cache that
// keeps packfile delta resolution from re-fetching bases over HTTP.
const readCacheMax = 16 << 20

// DefaultMaxObjectBytes is the per-object cap (§6.4).
const DefaultMaxObjectBytes = 256 << 20

// DefaultMaxPushObjects caps one push's object count (§6.4).
const DefaultMaxPushObjects = 100_000

// ErrObjectTooLarge / ErrTooManyObjects are the §6.4 cap rejections.
var (
	ErrObjectTooLarge  = errors.New("object exceeds the per-object size cap")
	ErrTooManyObjects  = errors.New("push exceeds the object-count cap")
	errUnsupportedGitO = errors.New("not supported by the databox git storer")
)

// RepoStorer is one repository's storage.Storer over databox. It is
// request-scoped (it carries the request context because go-git's
// storer interfaces take none) and NOT safe for use past its request.
type RepoStorer struct {
	st    *Store
	ctx   context.Context
	repo  Repo
	chain []string // leaf repoID first, then forkOf ancestors (§6.2)

	// Caps (§6.4), zero = default. Enforced on writes only.
	MaxObjectBytes int64
	MaxObjects     int
	// OnStored, when set, observes stored-byte deltas as objects land —
	// the incremental quota hook for chunked/gzip pushes (§6.5). An
	// error aborts the unpack.
	OnStored func(delta int64) error

	mu          sync.Mutex
	staged      map[plumbing.Hash][]byte // encoded, not yet flushed
	stagedOrder []plumbing.Hash
	stagedBytes int64
	cache       map[plumbing.Hash][]byte // read/write-back cache (bounded)
	cacheBytes  int64
	writtenKV   []string // flushed KV object keys (abort sweep)
	writtenBlob []string // written blob object keys (abort sweep)
	storedBytes int64
	objCount    int
}

// interface conformance — the compiler is the review gate here.
var _ storage.Storer = (*RepoStorer)(nil)

// Storer opens repo's storage. The fork chain is resolved once, up
// front (§6.2 read-through).
func (s *Store) Storer(ctx context.Context, repo Repo) (*RepoStorer, error) {
	chain, err := s.ForkChain(ctx, repo)
	if err != nil {
		return nil, err
	}
	return &RepoStorer{
		st: s, ctx: ctx, repo: repo, chain: chain,
		MaxObjectBytes: DefaultMaxObjectBytes,
		MaxObjects:     DefaultMaxPushObjects,
		staged:         map[plumbing.Hash][]byte{},
		cache:          map[plumbing.Hash][]byte{},
	}, nil
}

// --- loose-object encoding ----------------------------------------------------

// encodeObject renders the at-rest form: "<type> <size>\n" + zlib(content).
func encodeObject(t plumbing.ObjectType, content []byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s %d\n", t.String(), len(content))
	zw := zlib.NewWriter(&buf)
	zw.Write(content)
	zw.Close()
	return buf.Bytes()
}

// parseObjHeader reads the "<type> <size>\n" line off an encoded object
// (or its first bytes).
func parseObjHeader(encoded []byte) (t plumbing.ObjectType, size int64, bodyAt int, err error) {
	nl := bytes.IndexByte(encoded, '\n')
	if nl < 0 || nl > 32 {
		return plumbing.InvalidObject, 0, 0, fmt.Errorf("bad object header")
	}
	typeStr, sizeStr, ok := strings.Cut(string(encoded[:nl]), " ")
	if !ok {
		return plumbing.InvalidObject, 0, 0, fmt.Errorf("bad object header")
	}
	t, err = plumbing.ParseObjectType(typeStr)
	if err != nil {
		return plumbing.InvalidObject, 0, 0, err
	}
	size, err = strconv.ParseInt(sizeStr, 10, 64)
	return t, size, nl + 1, err
}

// decodeObject inflates an encoded object's content.
func decodeObject(encoded []byte) (plumbing.ObjectType, []byte, error) {
	t, size, at, err := parseObjHeader(encoded)
	if err != nil {
		return plumbing.InvalidObject, nil, err
	}
	zr, err := zlib.NewReader(bytes.NewReader(encoded[at:]))
	if err != nil {
		return plumbing.InvalidObject, nil, err
	}
	defer zr.Close()
	content, err := io.ReadAll(zr)
	if err != nil {
		return plumbing.InvalidObject, nil, err
	}
	if int64(len(content)) != size {
		return plumbing.InvalidObject, nil, fmt.Errorf("object size mismatch")
	}
	return t, content, nil
}

// --- EncodedObjectStorer --------------------------------------------------------

// NewEncodedObject returns a fresh in-memory object for the packfile
// parser to fill.
func (r *RepoStorer) NewEncodedObject() plumbing.EncodedObject { return &plumbing.MemoryObject{} }

// SetEncodedObject stores one loose object in the LEAF repo (§6.2). An
// object already reachable through the fork chain is deduplicated —
// that is what makes forks charge only for what they add (§5.3). Small
// encodings buffer until Flush; large ones stream to a blob key now.
func (r *RepoStorer) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	if r.MaxObjectBytes > 0 && obj.Size() > r.MaxObjectBytes {
		return plumbing.ZeroHash, ErrObjectTooLarge
	}
	rd, err := obj.Reader()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	content, err := io.ReadAll(rd)
	rd.Close()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	h := plumbing.ComputeHash(obj.Type(), content)
	if err := r.HasEncodedObject(h); err == nil {
		return h, nil // already reachable — never stored twice
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.objCount++
	if r.MaxObjects > 0 && r.objCount > r.MaxObjects {
		return plumbing.ZeroHash, ErrTooManyObjects
	}
	encoded := encodeObject(obj.Type(), content)
	if len(encoded) >= objBlobThreshold {
		key := objBlobKey(r.repo.ID, h.String())
		if err := r.st.DB.PutBlob(r.ctx, key, bytes.NewReader(encoded), "application/x-pcp-git-object"); err != nil {
			return plumbing.ZeroHash, err
		}
		r.writtenBlob = append(r.writtenBlob, key)
		if err := r.accountLocked(int64(len(encoded))); err != nil {
			return plumbing.ZeroHash, err
		}
		r.cachePut(h, encoded)
		return h, nil
	}
	if _, dup := r.staged[h]; !dup {
		r.staged[h] = encoded
		r.stagedOrder = append(r.stagedOrder, h)
		r.stagedBytes += int64(len(encoded))
		if err := r.accountLocked(int64(len(encoded))); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	if r.stagedBytes >= flushMaxBytes || len(r.stagedOrder) >= flushMaxKeys {
		if err := r.flushLocked(); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	return h, nil
}

// accountLocked tallies stored bytes and runs the incremental hook.
func (r *RepoStorer) accountLocked(delta int64) error {
	r.storedBytes += delta
	if r.OnStored != nil {
		return r.OnStored(delta)
	}
	return nil
}

// cachePut keeps a bounded write-back cache so packfile delta bases
// resolve without re-fetching (evicts wholesale when over budget —
// simple and good enough for one request).
func (r *RepoStorer) cachePut(h plumbing.Hash, encoded []byte) {
	if int64(len(encoded)) > readCacheMax/4 {
		return
	}
	if r.cacheBytes+int64(len(encoded)) > readCacheMax {
		r.cache = map[plumbing.Hash][]byte{}
		r.cacheBytes = 0
	}
	r.cache[h] = encoded
	r.cacheBytes += int64(len(encoded))
}

// Flush commits buffered object writes in chunked transactions. Call it
// after a successful unpack, before ref updates.
func (r *RepoStorer) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushLocked()
}

func (r *RepoStorer) flushLocked() error {
	for len(r.stagedOrder) > 0 {
		n, bytesInTx := 0, 0
		tx := r.st.DB.NewTx()
		var keys []string
		for _, h := range r.stagedOrder {
			enc := r.staged[h]
			if n > 0 && (n >= flushMaxKeys || bytesInTx+len(enc) > flushMaxBytes) {
				break
			}
			key := objKey(r.repo.ID, h.String())
			tx.Set(key, enc)
			keys = append(keys, key)
			n++
			bytesInTx += len(enc)
		}
		if err := tx.Commit(r.ctx); err != nil {
			return err
		}
		r.writtenKV = append(r.writtenKV, keys...)
		for _, h := range r.stagedOrder[:n] {
			r.cachePut(h, r.staged[h])
			r.stagedBytes -= int64(len(r.staged[h]))
			delete(r.staged, h)
		}
		r.stagedOrder = r.stagedOrder[n:]
	}
	return nil
}

// StoredBytes reports what this storer wrote so far (encoded bytes) —
// the quota reconcile input (§6.5).
func (r *RepoStorer) StoredBytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.storedBytes
}

// Abort deletes everything this storer wrote (staged, flushed, blobs) —
// the failed-push cleanup (§6.5). The caller refunds the charges.
func (r *RepoStorer) Abort() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.staged, r.stagedOrder, r.stagedBytes = map[plumbing.Hash][]byte{}, nil, 0
	var firstErr error
	for _, key := range r.writtenKV {
		if err := r.st.DB.Delete(r.ctx, key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, key := range r.writtenBlob {
		if err := r.st.DB.DeleteBlob(r.ctx, key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	r.writtenKV, r.writtenBlob = nil, nil
	return firstErr
}

// readEncoded resolves one object hash: staged/cache first, then each
// chain repo's KV tier, then its blob tier (§6.2 read-through).
func (r *RepoStorer) readEncoded(h plumbing.Hash) ([]byte, error) {
	r.mu.Lock()
	if enc, ok := r.staged[h]; ok {
		r.mu.Unlock()
		return enc, nil
	}
	if enc, ok := r.cache[h]; ok {
		r.mu.Unlock()
		return enc, nil
	}
	r.mu.Unlock()
	sha := h.String()
	for _, repoID := range r.chain {
		if e, found, err := r.st.DB.Get(r.ctx, objKey(repoID, sha)); err != nil {
			return nil, err
		} else if found {
			r.mu.Lock()
			r.cachePut(h, e.Value)
			r.mu.Unlock()
			return e.Value, nil
		}
		var buf bytes.Buffer
		key := objBlobKey(repoID, sha)
		if _, _, found, err := r.st.DB.StatBlob(r.ctx, key); err != nil {
			return nil, err
		} else if !found {
			continue
		}
		if err := r.st.DB.GetBlob(r.ctx, key, &buf); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return nil, plumbing.ErrObjectNotFound
}

// statEncoded reads only an object's header: KV values arrive whole
// anyway; blob headers ride a 64-byte ranged read.
func (r *RepoStorer) statEncoded(h plumbing.Hash) (plumbing.ObjectType, int64, error) {
	r.mu.Lock()
	if enc, ok := r.staged[h]; ok {
		r.mu.Unlock()
		t, size, _, err := parseObjHeader(enc)
		return t, size, err
	}
	if enc, ok := r.cache[h]; ok {
		r.mu.Unlock()
		t, size, _, err := parseObjHeader(enc)
		return t, size, err
	}
	r.mu.Unlock()
	sha := h.String()
	for _, repoID := range r.chain {
		if e, found, err := r.st.DB.Get(r.ctx, objKey(repoID, sha)); err != nil {
			return plumbing.InvalidObject, 0, err
		} else if found {
			t, size, _, err := parseObjHeader(e.Value)
			return t, size, err
		}
		key := objBlobKey(repoID, sha)
		if _, _, found, err := r.st.DB.StatBlob(r.ctx, key); err != nil {
			return plumbing.InvalidObject, 0, err
		} else if !found {
			continue
		}
		var head bytes.Buffer
		if err := r.st.DB.GetBlobRange(r.ctx, key, 0, 64, &head); err != nil {
			return plumbing.InvalidObject, 0, err
		}
		t, size, _, err := parseObjHeader(head.Bytes())
		return t, size, err
	}
	return plumbing.InvalidObject, 0, plumbing.ErrObjectNotFound
}

// EncodedObject loads one object, walking the fork chain on a miss.
func (r *RepoStorer) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	encoded, err := r.readEncoded(h)
	if err != nil {
		return nil, err
	}
	actual, content, err := decodeObject(encoded)
	if err != nil {
		return nil, err
	}
	if t != plumbing.AnyObject && actual != t {
		return nil, plumbing.ErrObjectNotFound
	}
	obj := &plumbing.MemoryObject{}
	obj.SetType(actual)
	obj.SetSize(int64(len(content)))
	obj.Write(content)
	return obj, nil
}

// HasEncodedObject answers existence across the fork chain.
func (r *RepoStorer) HasEncodedObject(h plumbing.Hash) error {
	_, _, err := r.statEncoded(h)
	return err
}

// EncodedObjectSize stats an object without a full read (§6.2 — the
// header carries type and size).
func (r *RepoStorer) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	_, size, err := r.statEncoded(h)
	return size, err
}

// IterEncodedObjects walks the LEAF repo's own objects (fork parents'
// objects belong to the parents; GC and stats want leaf-only).
func (r *RepoStorer) IterEncodedObjects(t plumbing.ObjectType) (gitstorer.EncodedObjectIter, error) {
	var hashes []plumbing.Hash
	collect := func(sha string, header []byte) error {
		actual, _, _, err := parseObjHeader(header)
		if err != nil {
			return err
		}
		if t == plumbing.AnyObject || actual == t {
			hashes = append(hashes, plumbing.NewHash(sha))
		}
		return nil
	}
	prefix := objPrefix + r.repo.ID + "/"
	cursor := ""
	for {
		entries, next, err := r.st.DB.List(r.ctx, prefix, cursor, 500)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if err := collect(strings.TrimPrefix(e.Key, prefix), e.Value); err != nil {
				return nil, err
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	blobPrefix := objBlobPrefix + r.repo.ID + "/"
	cursor = ""
	for {
		entries, next, err := r.st.DB.List(r.ctx, blobPrefix, cursor, 500)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			var head bytes.Buffer
			if err := r.st.DB.GetBlobRange(r.ctx, e.Key, 0, 64, &head); err != nil {
				return nil, err
			}
			if err := collect(strings.TrimPrefix(e.Key, blobPrefix), head.Bytes()); err != nil {
				return nil, err
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return gitstorer.NewEncodedObjectLookupIter(r, t, hashes), nil
}

// AddAlternate — databox repos share objects through the forkOf chain,
// not filesystem alternates.
func (r *RepoStorer) AddAlternate(string) error { return errUnsupportedGitO }

// --- ReferenceStorer -------------------------------------------------------------

// Reference resolves one ref. HEAD is symbolic to the repo record's
// default branch (§6.2) — it never lives in the refs keyspace.
func (r *RepoStorer) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	if name == plumbing.HEAD {
		return plumbing.NewSymbolicReference(plumbing.HEAD,
			plumbing.NewBranchReferenceName(r.repo.DefaultBranch)), nil
	}
	if err := validRefName(string(name)); err != nil {
		return nil, plumbing.ErrReferenceNotFound
	}
	e, found, err := r.st.DB.Get(r.ctx, refKey(r.repo.ID, string(name)))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, plumbing.ErrReferenceNotFound
	}
	return plumbing.NewHashReference(name, plumbing.NewHash(strings.TrimSpace(string(e.Value)))), nil
}

// SetReference writes one hash ref. Symbolic refs are derived state
// here (HEAD ← default branch), never stored.
func (r *RepoStorer) SetReference(ref *plumbing.Reference) error {
	if ref.Type() != plumbing.HashReference {
		return nil
	}
	if err := validRefName(string(ref.Name())); err != nil {
		return err
	}
	_, err := r.st.DB.Set(r.ctx, refKey(r.repo.ID, string(ref.Name())), []byte(ref.Hash().String()))
	return err
}

// CheckAndSetReference is the single-ref CAS (go-git callers); pushes
// go through Store.ApplyRefUpdates for multi-ref atomicity.
func (r *RepoStorer) CheckAndSetReference(new, old *plumbing.Reference) error {
	if new == nil {
		return errors.New("nil reference")
	}
	if err := validRefName(string(new.Name())); err != nil {
		return err
	}
	key := refKey(r.repo.ID, string(new.Name()))
	return r.st.DB.RunTx(r.ctx, func(tx *client.Tx) error {
		if old != nil {
			raw, found, err := tx.Get(r.ctx, key)
			if err != nil {
				return err
			}
			if !found || strings.TrimSpace(string(raw)) != old.Hash().String() {
				return storage.ErrReferenceHasChanged
			}
		}
		tx.Set(key, []byte(new.Hash().String()))
		return nil
	})
}

// RemoveReference deletes one ref.
func (r *RepoStorer) RemoveReference(name plumbing.ReferenceName) error {
	if err := validRefName(string(name)); err != nil {
		return nil
	}
	return r.st.DB.Delete(r.ctx, refKey(r.repo.ID, string(name)))
}

// IterReferences lists the repo's refs plus symbolic HEAD.
func (r *RepoStorer) IterReferences() (gitstorer.ReferenceIter, error) {
	refs := []*plumbing.Reference{
		plumbing.NewSymbolicReference(plumbing.HEAD,
			plumbing.NewBranchReferenceName(r.repo.DefaultBranch)),
	}
	prefix := refsPrefix + r.repo.ID + "/"
	cursor := ""
	for {
		entries, next, err := r.st.DB.List(r.ctx, prefix, cursor, 500)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			name := plumbing.ReferenceName(strings.TrimPrefix(e.Key, prefix))
			refs = append(refs, plumbing.NewHashReference(name,
				plumbing.NewHash(strings.TrimSpace(string(e.Value)))))
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return gitstorer.NewReferenceSliceIter(refs), nil
}

// CountLooseRefs counts stored refs.
func (r *RepoStorer) CountLooseRefs() (int, error) {
	n := 0
	prefix := refsPrefix + r.repo.ID + "/"
	cursor := ""
	for {
		entries, next, err := r.st.DB.List(r.ctx, prefix, cursor, 500)
		if err != nil {
			return 0, err
		}
		n += len(entries)
		if next == "" {
			return n, nil
		}
		cursor = next
	}
}

// PackRefs — refs are already individual KV rows; nothing to pack.
func (r *RepoStorer) PackRefs() error { return nil }

// --- honest stubs (unused by the wire protocol) -----------------------------------

// Shallow — shallow clones are not served in v1 (§6.3: upload-pack
// rejects shallow requests before this is consulted).
func (r *RepoStorer) Shallow() ([]plumbing.Hash, error) { return nil, nil }

// SetShallow — never valid server-side here.
func (r *RepoStorer) SetShallow([]plumbing.Hash) error { return errUnsupportedGitO }

// Index / SetIndex — a bare server repo has no worktree index.
func (r *RepoStorer) Index() (*index.Index, error) { return &index.Index{Version: 2}, nil }
func (r *RepoStorer) SetIndex(*index.Index) error  { return errUnsupportedGitO }

// Config / SetConfig — nothing configurable lives in the storer; repo
// metadata is the Repo record.
func (r *RepoStorer) Config() (*gogitcfg.Config, error) { return gogitcfg.NewConfig(), nil }
func (r *RepoStorer) SetConfig(*gogitcfg.Config) error  { return errUnsupportedGitO }

// Module — no submodule storage; an empty in-memory storer satisfies
// go-git's "missing module" contract.
func (r *RepoStorer) Module(string) (storage.Storer, error) { return memory.NewStorage(), nil }
