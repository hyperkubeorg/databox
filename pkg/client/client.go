// Package client is databox's official Go client (§25
// "Resolved Decisions"): a thin, dependency-light wrapper over the HTTPS
// API that implements the documented retry convention — exponential
// backoff with jitter on Conflict/ShardSplitting, five attempts by
// default. The CLI, the console REPL, and the SQL/S3 processing layers
// all consume the cluster through this package, so its behavior IS the
// reference client behavior.
//
// TLS trust supports three modes:
//
//   - a CA pool (production, cluster CA or corporate PKI),
//   - an explicit pinned fingerprint set — the console's trust-on-first-use
//     store at ~/.databox/known_certs (§6.3),
//   - insecure=false always; there is no "skip verification" switch.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/certs"
	"github.com/hyperkubeorg/databox/pkg/kv"
)

// Client talks to one databox cluster endpoint.
type Client struct {
	base  string // https://host:port
	http  *http.Client
	token string

	// Retries is the attempt budget for retryable errors (default 5).
	Retries int
}

// Options configures New.
type Options struct {
	// Endpoint is "host:port" (https is implied and required).
	Endpoint string
	// CAPool verifies the server against a CA (optional).
	CAPool *x509.CertPool
	// TrustFingerprints pins acceptable certificate SHA-256 fingerprints
	// (the console trust store). Used when CAPool is nil.
	TrustFingerprints []string
	// OnUnknownCert, when set, is consulted for a certificate that
	// matches neither pool nor pins: return true to trust it for this
	// process (the console's interactive prompt, §6.3).
	OnUnknownCert func(fingerprint string, cert *x509.Certificate) bool
	// Token pre-sets a session token (otherwise call Login).
	Token string
}

// ErrRetryable tags errors the retry loop may re-attempt.
var ErrRetryable = errors.New("retryable")

// ErrTxTooOld tags reads whose pinned read version fell behind the MVCC
// history horizon (§10). It is NOT retryable at the request level —
// re-sending the same read can never succeed — the whole transaction must
// restart with fresh reads (RunTx does this automatically).
var ErrTxTooOld = errors.New("TxTooOld")

// ErrRevisionCompacted tags a watch resume whose from_revision has fallen
// out of the shard's resume buffer (§9.2). Not retryable as-is: re-list
// and re-subscribe from the current state, per the documented contract.
var ErrRevisionCompacted = errors.New("RevisionCompacted")

// New builds a client. Certificate verification is custom but strict: a
// certificate is accepted only via CA chain, an explicit pin, or the
// interactive OnUnknownCert approval.
func New(opts Options) (*Client, error) {
	if opts.Endpoint == "" {
		return nil, fmt.Errorf("endpoint required")
	}
	pins := map[string]bool{}
	for _, f := range opts.TrustFingerprints {
		pins[strings.ToUpper(f)] = true
	}
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Verification happens in VerifyPeerCertificate below; the
		// standard chain check is disabled because self-managed clusters
		// use the embedded CA which the OS does not know. This is pin-or
		// -pool verification, not "no verification".
		InsecureSkipVerify: true, //nolint:gosec — replaced by explicit verification below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("server presented no certificate")
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			// 1) CA pool chain verification when a pool was supplied.
			if opts.CAPool != nil {
				inter := x509.NewCertPool()
				for _, raw := range rawCerts[1:] {
					if c, err := x509.ParseCertificate(raw); err == nil {
						inter.AddCert(c)
					}
				}
				if _, err := leaf.Verify(x509.VerifyOptions{Roots: opts.CAPool, Intermediates: inter}); err == nil {
					return nil
				}
			}
			// 2) Pinned fingerprints (the known_certs store).
			fp := certs.FingerprintDER(rawCerts[0])
			if pins[fp] {
				return nil
			}
			// 3) Interactive trust-on-first-use.
			if opts.OnUnknownCert != nil && opts.OnUnknownCert(fp, leaf) {
				pins[fp] = true
				return nil
			}
			return fmt.Errorf("server certificate %s is not trusted", fp)
		},
	}
	return &Client{
		base:    "https://" + opts.Endpoint,
		token:   opts.Token,
		Retries: 5,
		http: &http.Client{Transport: &http.Transport{
			TLSClientConfig:   tlsCfg,
			ForceAttemptHTTP2: true,
		}},
	}, nil
}

// Token returns the current session token (for persisting sessions).
func (c *Client) Token() string { return c.token }

// apiError is the server's JSON error envelope.
type apiError struct {
	Error string `json:"error"`
}

// do performs one JSON request with the retry convention applied.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var lastErr error
	backoff := 100 * time.Millisecond
	attempts := c.Retries
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter, capped at 3s — the
			// documented retry recommendation clients should copy.
			jitter := time.Duration(rand.Int63n(int64(backoff) / 2))
			select {
			case <-time.After(backoff + jitter):
			case <-ctx.Done():
				// The deadline died during backoff — a bare ctx error
				// hides what actually failed. Carry the last attempt's
				// error: it is the diagnostic.
				if lastErr != nil {
					return fmt.Errorf("%v (last attempt: %w)", ctx.Err(), lastErr)
				}
				return ctx.Err()
			}
			if backoff < 3*time.Second {
				backoff *= 2
			}
		}
		err := c.once(ctx, method, path, in, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, ErrRetryable) {
			return err
		}
		// A retryable failure may be THIS backend's problem. Behind a
		// load-balanced VIP (a Kubernetes Service), keep-alive pins
		// every retry to the same TCP connection — the same possibly
		// unhealthy node. Dropping idle connections makes the next
		// attempt re-dial, letting the balancer rotate backends.
		if t, ok := c.http.Transport.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
	}
	return lastErr
}

// once performs a single request/response cycle.
func (c *Client) once(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRetryable, err) // network errors retry
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		if out != nil {
			return json.Unmarshal(raw, out)
		}
		return nil
	}
	var ae apiError
	_ = json.Unmarshal(raw, &ae)
	msg := ae.Error
	if msg == "" {
		msg = resp.Status
	}
	// 409 (Conflict/LockHeld) and 503 (ShardSplitting, leadership churn)
	// are the retryable statuses per the documented convention.
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusServiceUnavailable {
		return fmt.Errorf("%w: %s", ErrRetryable, msg)
	}
	// TxTooOld (410): typed so transaction runners can distinguish
	// "restart the transaction" from a genuine failure.
	if strings.HasPrefix(msg, "TxTooOld") {
		return fmt.Errorf("%w: %s", ErrTxTooOld, strings.TrimPrefix(msg, "TxTooOld: "))
	}
	return errors.New(msg)
}

// --- auth ---------------------------------------------------------------------

// Login authenticates and stores the session token on the client.
func (c *Client) Login(ctx context.Context, username, password string) error {
	var out struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"username": username, "password": password}, &out); err != nil {
		return err
	}
	c.token = out.Token
	return nil
}

// --- kv -----------------------------------------------------------------------

// KVEntry is one key-value row as the API returns it.
type KVEntry struct {
	Key   string `json:"key"`
	Value []byte `json:"value"`
	Rev   uint64 `json:"rev"`
	Blob  bool   `json:"blob,omitempty"`
}

// Get fetches one key (linearizable). found=false means no such key.
func (c *Client) Get(ctx context.Context, key string) (KVEntry, bool, error) {
	var out KVEntry
	err := c.do(ctx, http.MethodGet, "/api/v1/kv"+escapeKey(key), nil, &out)
	if err != nil {
		if strings.HasPrefix(err.Error(), "NotFound") {
			return KVEntry{}, false, nil
		}
		return KVEntry{}, false, err
	}
	return out, true, nil
}

// Set writes one key, returning its new revision.
func (c *Client) Set(ctx context.Context, key string, value []byte) (uint64, error) {
	var out struct {
		Rev uint64 `json:"rev"`
	}
	err := c.do(ctx, http.MethodPut, "/api/v1/kv"+escapeKey(key), map[string][]byte{"value": value}, &out)
	return out.Rev, err
}

// Delete removes one key.
func (c *Client) Delete(ctx context.Context, key string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/kv"+escapeKey(key), nil, nil)
}

// DeleteRange removes [start, end).
func (c *Client) DeleteRange(ctx context.Context, start, end string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/delete-range", map[string]string{"start": start, "end": end}, nil)
}

// List pages keys under prefix. Pass the returned cursor to continue.
func (c *Client) List(ctx context.Context, prefix, cursor string, limit int) ([]KVEntry, string, error) {
	var out struct {
		Entries    []KVEntry `json:"entries"`
		NextCursor string    `json:"next_cursor"`
	}
	q := url.Values{"prefix": {prefix}, "cursor": {cursor}}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/list?"+q.Encode(), nil, &out)
	return out.Entries, out.NextCursor, err
}

// Watch streams events under prefix to fn until ctx ends or the stream
// breaks. fromRev resumes a single-shard watch (§9.2).
//
// A resume whose from_revision has been compacted fails with
// ErrRevisionCompacted — either up front (HTTP 410, the server pre-flights
// resumability) or mid-stream if compaction raced the subscription. Any
// server-reported mid-stream error line ends the watch with that error;
// it is never delivered to fn as an event.
func (c *Client) Watch(ctx context.Context, prefix string, fromRev uint64, fn func(kv.Event) error) error {
	q := url.Values{"prefix": {prefix}}
	if fromRev > 0 {
		q.Set("from_revision", fmt.Sprint(fromRev))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/watch?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var ae apiError
		_ = json.Unmarshal(raw, &ae)
		if resp.StatusCode == http.StatusGone && strings.HasPrefix(ae.Error, "RevisionCompacted") {
			return fmt.Errorf("%w: re-list and re-subscribe from current state", ErrRevisionCompacted)
		}
		return fmt.Errorf("watch failed: %s: %s", resp.Status, string(raw))
	}
	dec := json.NewDecoder(resp.Body)
	for {
		// A stream line is either an event or a terminal server error —
		// decode both shapes and never hand an error line to fn.
		var line struct {
			kv.Event
			Error string `json:"error"`
		}
		if err := dec.Decode(&line); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if line.Error != "" {
			if strings.HasPrefix(line.Error, "RevisionCompacted") {
				return fmt.Errorf("%w: re-list and re-subscribe from current state", ErrRevisionCompacted)
			}
			return fmt.Errorf("watch ended by server: %s", line.Error)
		}
		if err := fn(line.Event); err != nil {
			return err
		}
	}
}

// --- transactions -----------------------------------------------------------------

// Tx accumulates a read set and write set client-side (§10). Use like:
//
//	tx := c.NewTx()
//	v, _ := tx.Get(ctx, "/a")            // records the revision read
//	tx.Set("/b", append(v, '!'))
//	err := tx.Commit(ctx)                // Conflict → retry the whole func
//
// Reads are SNAPSHOT reads per shard: the first read against a shard pins
// that shard's revision (the server reports which revision the read ran
// at), and every later read against the same shard executes at the pinned
// revision — concurrent writers cannot change what this transaction sees.
// A pin that ages past the server's MVCC history horizon makes reads fail
// with ErrTxTooOld; restart the transaction (RunTx automates this).
type Tx struct {
	c      *Client
	reads  map[string]uint64
	writes []kv.TxWrite
	// cache provides read-your-writes inside the transaction (§10).
	cache map[string]*[]byte
	// pins holds the per-shard read version: shard group ID → the shard
	// revision this transaction reads at. Filled lazily on first contact
	// with each shard and sent as ?pins= on every subsequent read.
	pins map[uint64]uint64
}

// NewTx starts a client-side transaction.
func (c *Client) NewTx() *Tx {
	return &Tx{c: c, reads: map[string]uint64{}, cache: map[string]*[]byte{}, pins: map[uint64]uint64{}}
}

// ReadVersions exposes the per-shard read versions pinned so far (for
// diagnostics; keys are shard group IDs).
func (t *Tx) ReadVersions() map[uint64]uint64 {
	out := make(map[uint64]uint64, len(t.pins))
	for gid, rev := range t.pins {
		out[gid] = rev
	}
	return out
}

// pinsParam encodes the pin map for the ?pins= query parameter.
func (t *Tx) pinsParam() string {
	var b strings.Builder
	for gid, rev := range t.pins {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d:%d", gid, rev)
	}
	return b.String()
}

// pin records a shard's read version the first time the transaction
// touches it. Later reads keep the original pin — that is what makes the
// per-shard view a stable snapshot.
func (t *Tx) pin(gid, shardRev uint64) {
	if _, ok := t.pins[gid]; !ok && gid > 0 {
		t.pins[gid] = shardRev
	}
}

// Get reads through the transaction: staged writes are visible to the
// transaction itself, and every base read records the revision it saw.
// Base reads execute at the shard's pinned read version (see Tx doc).
func (t *Tx) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if v, ok := t.cache[key]; ok {
		if v == nil {
			return nil, false, nil // staged delete
		}
		return *v, true, nil
	}
	// tx=1 asks the server for the transactional response shape: 200 with
	// a found flag plus gid/shard_rev, so even a miss pins the shard.
	q := url.Values{"tx": {"1"}}
	if len(t.pins) > 0 {
		q.Set("pins", t.pinsParam())
	}
	var out struct {
		Found    bool   `json:"found"`
		Value    []byte `json:"value"`
		Rev      uint64 `json:"rev"`
		GID      uint64 `json:"gid"`
		ShardRev uint64 `json:"shard_rev"`
	}
	if err := t.c.do(ctx, http.MethodGet, "/api/v1/kv"+escapeKey(key)+"?"+q.Encode(), nil, &out); err != nil {
		return nil, false, err // ErrTxTooOld surfaces here, typed
	}
	t.pin(out.GID, out.ShardRev)
	if out.Found {
		// Record the VERSION's revision (not the pin): commit validation
		// compares it against the key's latest revision, so a writer that
		// changed the key after our snapshot still conflicts correctly.
		t.reads[key] = out.Rev
		return out.Value, true, nil
	}
	t.reads[key] = 0 // "did not exist" is also a read to validate
	return nil, false, nil
}

// List scans a prefix at the transaction's snapshot: pinned shards return
// their state as of the pin, newly touched shards are pinned by the scan.
// Every returned key joins the read set (validated at commit); note that
// keys *absent* from the result are NOT validated — phantom inserts into
// the scanned range do not conflict. Staged writes are not merged into
// the result (List reads the base snapshot only).
func (t *Tx) List(ctx context.Context, prefix, cursor string, limit int) ([]KVEntry, string, error) {
	q := url.Values{"prefix": {prefix}, "cursor": {cursor}}
	// Always send pins (even empty) — its presence selects the versioned
	// scan and gets shard_revs back for lazy pinning.
	q.Set("pins", t.pinsParam())
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	var out struct {
		Entries    []KVEntry         `json:"entries"`
		NextCursor string            `json:"next_cursor"`
		ShardRevs  map[uint64]uint64 `json:"shard_revs"`
	}
	if err := t.c.do(ctx, http.MethodGet, "/api/v1/list?"+q.Encode(), nil, &out); err != nil {
		return nil, "", err
	}
	for gid, rev := range out.ShardRevs {
		t.pin(gid, rev)
	}
	for _, e := range out.Entries {
		if _, staged := t.cache[e.Key]; !staged {
			t.reads[e.Key] = e.Rev
		}
	}
	return out.Entries, out.NextCursor, nil
}

// Set stages a write.
func (t *Tx) Set(key string, value []byte) {
	v := append([]byte(nil), value...)
	t.cache[key] = &v
	t.writes = append(t.writes, kv.TxWrite{Key: key, Value: v})
}

// Delete stages a deletion.
func (t *Tx) Delete(key string) {
	t.cache[key] = nil
	t.writes = append(t.writes, kv.TxWrite{Key: key, Delete: true})
}

// Commit submits the transaction. A Conflict error means another writer
// won; re-run the transaction body and commit again.
func (t *Tx) Commit(ctx context.Context) error {
	if len(t.writes) == 0 {
		return nil // read-only transactions validate trivially
	}
	// Commit must NOT ride the generic retry loop: a Conflict result is
	// a real answer (re-run the transaction), not a transient fault.
	body := map[string]any{"reads": t.reads, "writes": t.writes}
	saved := t.c.Retries
	t.c.Retries = 1
	err := t.c.do(ctx, http.MethodPost, "/api/v1/tx/commit", body, nil)
	t.c.Retries = saved
	return err
}

// IsConflict reports whether err is an OCC commit conflict — the "re-run
// the transaction body" answer, as opposed to a transport fault.
func IsConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Conflict")
}

// RunTx runs fn inside a transaction and commits, restarting the whole
// transaction (fresh Tx, fresh read versions) with backoff when the
// commit conflicts or a read answers ErrTxTooOld — the §10 retry
// convention. Any other error from fn or commit returns immediately.
// The attempt budget is the client's Retries setting.
func (c *Client) RunTx(ctx context.Context, fn func(tx *Tx) error) error {
	attempts := c.Retries
	if attempts < 1 {
		attempts = 1
	}
	backoff := 100 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			jitter := time.Duration(rand.Int63n(int64(backoff) / 2))
			select {
			case <-time.After(backoff + jitter):
			case <-ctx.Done():
				// The deadline died during backoff — a bare ctx error
				// hides what actually failed. Carry the last attempt's
				// error: it is the diagnostic.
				if lastErr != nil {
					return fmt.Errorf("%v (last attempt: %w)", ctx.Err(), lastErr)
				}
				return ctx.Err()
			}
			if backoff < 3*time.Second {
				backoff *= 2
			}
		}
		tx := c.NewTx()
		err := fn(tx)
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err == nil {
			return nil
		}
		// Restartable: someone else won (Conflict) or our snapshot aged
		// out (TxTooOld). Everything else is a real failure.
		if IsConflict(err) || errors.Is(err, ErrTxTooOld) {
			lastErr = err
			continue
		}
		return err
	}
	return lastErr
}

// --- locks --------------------------------------------------------------------------

// LockAcquire takes/refreshes a lock, returning the fencing token (§9).
func (c *Client) LockAcquire(ctx context.Context, resource, mode string, ttl time.Duration) (uint64, error) {
	var out struct {
		Fencing uint64 `json:"fencing"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/locks/acquire",
		map[string]any{"resource": resource, "mode": mode, "ttl_ms": ttl.Milliseconds()}, &out)
	return out.Fencing, err
}

// LockRelease releases a lock held by this user.
func (c *Client) LockRelease(ctx context.Context, resource string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/locks/release", map[string]string{"resource": resource}, nil)
}

// --- blobs ---------------------------------------------------------------------------

// PutBlob streams r into the blob at key.
func (c *Client) PutBlob(ctx context.Context, key string, r io.Reader, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/api/v1/blobs"+escapeKey(key), r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("blob upload failed: %s: %s", resp.Status, string(raw))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// GetBlob streams the blob at key into w.
func (c *Client) GetBlob(ctx context.Context, key string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/blobs"+escapeKey(key), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("blob download failed: %s: %s", resp.Status, string(raw))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// GetBlobRange streams length bytes of the blob at key, starting at
// offset, into w (length < 0 = to the end). The server never touches
// chunks outside the window — this is the primitive HTTP Range serving
// (video/audio seeking) builds on.
func (c *Client) GetBlobRange(ctx context.Context, key string, offset, length int64, w io.Writer) error {
	q := url.Values{"offset": {fmt.Sprint(offset)}}
	if length >= 0 {
		q.Set("length", fmt.Sprint(length))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/blobs"+escapeKey(key)+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("blob range download failed: %s: %s", resp.Status, string(raw))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// StatBlob returns a blob's size and content type without reading data
// (HEAD). found=false when the key holds no blob.
func (c *Client) StatBlob(ctx context.Context, key string) (size int64, contentType string, found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.base+"/api/v1/blobs"+escapeKey(key), nil)
	if err != nil {
		return 0, "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return 0, "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, "", false, fmt.Errorf("blob stat failed: %s", resp.Status)
	}
	size, _ = strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	return size, resp.Header.Get("Content-Type"), true, nil
}

// AppendBlob extends the blob at key with r's contents. A Conflict error
// means a concurrent append won — retry the call (the failed attempt's
// data never became visible).
func (c *Client) AppendBlob(ctx context.Context, key string, r io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.base+"/api/v1/blobs"+escapeKey(key), r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("blob append failed: %s: %s", resp.Status, string(raw))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// DeleteBlob removes the blob at key.
func (c *Client) DeleteBlob(ctx context.Context, key string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/blobs"+escapeKey(key), nil, nil)
}

// SpliceResult summarizes the blob a splice committed.
type SpliceResult struct {
	Rev  uint64 `json:"rev"`  // destination manifest's commit revision
	Size int64  `json:"size"` // total bytes (sum of the sources)
	// SHA256 is the destination's content hash. When Composite is true it
	// is the comma-joined per-source SHA-256 hex digests, in splice order
	// (the manifest's composite hash format); otherwise a plain whole-blob
	// digest.
	SHA256    string `json:"sha256"`
	Composite bool   `json:"composite"`
	Mode      string `json:"mode"` // "replica" | "ec"
}

// SpliceBlob concatenates the blobs at srcs, in order, into a single blob
// at dst — server-side, by splicing the sources' chunk maps into one
// manifest, so no blob data streams through the client (§25). The sources
// are left in place; delete them afterwards if they were temporary (their
// chunks are shared with dst and survive the deletes). Conflicts with
// concurrent writers are retried automatically per the client convention.
//
// Multi-source destinations carry a composite hash and refuse AppendBlob;
// see SpliceResult.SHA256.
func (c *Client) SpliceBlob(ctx context.Context, dst string, srcs []string, contentType string) (SpliceResult, error) {
	var out SpliceResult
	err := c.do(ctx, http.MethodPost, "/api/v1/blobs-splice", map[string]any{
		"destination":  dst,
		"sources":      srcs,
		"content_type": contentType,
	}, &out)
	return out, err
}

// --- admin -----------------------------------------------------------------------------

// Raw performs an arbitrary API call — the console and CLI use this for
// admin endpoints without duplicating every method here.
func (c *Client) Raw(ctx context.Context, method, path string, in, out any) error {
	return c.do(ctx, method, path, in, out)
}

// escapeKey renders a user key ("/a/b c") as a URL path suffix, keeping
// slashes as path separators but escaping everything else.
func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
