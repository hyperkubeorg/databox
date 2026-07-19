// Package v1api mounts the public JSON API under /api/v1
// (§9). Every handler follows the same shape:
//
//  1. authenticate the bearer token (except login and health),
//  2. authorize the (key, verb) pair against the user's grants (§7.2),
//  3. route the operation through pkg/server's data plane,
//  4. answer JSON — or NDJSON for watch, raw bytes for blob download.
//
// Error mapping: machine-readable error codes from the storage layer
// become HTTP statuses here (Conflict → 409, NotFound → 404,
// Unauthorized → 401/403, ValueTooLarge → 413, ShardSplitting → 503 with
// Retry-After). Clients retry 409/503 with backoff per the documented
// convention.
package v1api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/pkg/kv"
	"github.com/hyperkubeorg/databox/pkg/server"
)

// api bundles the server for handler methods.
type api struct{ s *server.Server }

// Mount attaches the v1 API to the router. Registered with the server via
// server.Mounters in cmd/databox.
func Mount(r *mux.Router, s *server.Server) {
	a := &api{s: s}
	v1 := r.PathPrefix("/api/v1").Subrouter()

	// Authentication.
	v1.HandleFunc("/auth/login", a.login).Methods(http.MethodPost)
	v1.HandleFunc("/auth/logout", a.logout).Methods(http.MethodPost)

	// KV (§9). {key:.*} keeps slashes inside key names.
	v1.HandleFunc("/kv/{key:.*}", a.kvGet).Methods(http.MethodGet)
	v1.HandleFunc("/kv/{key:.*}", a.kvSet).Methods(http.MethodPut)
	v1.HandleFunc("/kv/{key:.*}", a.kvDelete).Methods(http.MethodDelete)
	v1.HandleFunc("/delete-range", a.kvDeleteRange).Methods(http.MethodPost)
	v1.HandleFunc("/list", a.kvList).Methods(http.MethodGet)
	v1.HandleFunc("/watch", a.watch).Methods(http.MethodGet)

	// Transactions (§10).
	v1.HandleFunc("/tx/begin", a.txBegin).Methods(http.MethodPost)
	v1.HandleFunc("/tx/commit", a.txCommit).Methods(http.MethodPost)

	// Locks (§9).
	v1.HandleFunc("/locks/acquire", a.lockAcquire).Methods(http.MethodPost)
	v1.HandleFunc("/locks/release", a.lockRelease).Methods(http.MethodPost)
	v1.HandleFunc("/locks/force-unlock", a.lockForce).Methods(http.MethodPost)
	v1.HandleFunc("/locks/{resource:.*}", a.lockCheck).Methods(http.MethodGet)

	// Blobs (§11): raw request/response bodies, streamed.
	v1.HandleFunc("/blobs/{key:.*}", a.blobPut).Methods(http.MethodPut)
	v1.HandleFunc("/blobs/{key:.*}", a.blobAppend).Methods(http.MethodPatch)
	v1.HandleFunc("/blobs/{key:.*}", a.blobGet).Methods(http.MethodGet)
	v1.HandleFunc("/blobs/{key:.*}", a.blobHead).Methods(http.MethodHead)
	v1.HandleFunc("/blobs/{key:.*}", a.blobDelete).Methods(http.MethodDelete)
	v1.HandleFunc("/blobs-splice", a.blobSplice).Methods(http.MethodPost)

	// Users & grants (§7.3).
	v1.HandleFunc("/users", a.userList).Methods(http.MethodGet)
	v1.HandleFunc("/users", a.userCreate).Methods(http.MethodPost)
	v1.HandleFunc("/users/{name}", a.userDelete).Methods(http.MethodDelete)
	v1.HandleFunc("/users/{name}/password", a.userPasswd).Methods(http.MethodPost)
	v1.HandleFunc("/users/{name}/grants", a.grantAdd).Methods(http.MethodPost)
	v1.HandleFunc("/users/{name}/grants", a.grantRemove).Methods(http.MethodDelete)
	v1.HandleFunc("/users/{name}/access-keys", a.accessKeyCreate).Methods(http.MethodPost)
	v1.HandleFunc("/users/{name}/access-keys", a.accessKeyList).Methods(http.MethodGet)
	v1.HandleFunc("/users/{name}/access-keys/{key}", a.accessKeyDelete).Methods(http.MethodDelete)

	// Cluster management (§16).
	v1.HandleFunc("/cluster/status", a.clusterStatus).Methods(http.MethodGet)
	v1.HandleFunc("/cluster/join-token", a.joinToken).Methods(http.MethodPost)
	v1.HandleFunc("/cluster/decommission", a.decommission).Methods(http.MethodPost)

	// Automation pause/resume (§16.4): admin-only, audited. The route
	// constrains both segments, so unknown targets 404 instead of
	// reaching the handler.
	v1.HandleFunc("/admin/{target:rebalance|split|repair}/{action:pause|resume}", a.adminPause).Methods(http.MethodPost)

	// Manual shard split hint (§15 "manual hint"): admin-only, audited.
	// The numeric constraint keeps this from shadowing the pause route.
	v1.HandleFunc("/admin/shards/{gid:[0-9]+}/split", a.adminShardSplit).Methods(http.MethodPost)

	// Durability policies (§12): admin-only; mutations audited.
	v1.HandleFunc("/policies/{kind}", a.policyList).Methods(http.MethodGet)
	v1.HandleFunc("/policies/{kind}/{path:.*}", a.policyGet).Methods(http.MethodGet)
	v1.HandleFunc("/policies/{kind}/{path:.*}", a.policySet).Methods(http.MethodPut)
	v1.HandleFunc("/policies/{kind}/{path:.*}", a.policyDelete).Methods(http.MethodDelete)

	// System keyspace view — the `.databox/` namespace (§19).
	v1.HandleFunc("/system/list", a.systemList).Methods(http.MethodGet)
	v1.HandleFunc("/system/{key:.*}", a.systemGet).Methods(http.MethodGet)
}

// --- plumbing ---------------------------------------------------------------

// jsonOut writes a JSON response.
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail maps storage-layer errors onto HTTP semantics.
func fail(w http.ResponseWriter, err error) {
	msg := err.Error()
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, server.ErrNotFound) || strings.HasPrefix(msg, "NotFound"):
		status = http.StatusNotFound
	case errors.Is(err, server.ErrUnauthorized) || strings.HasPrefix(msg, "Unauthorized"):
		status = http.StatusForbidden
	case strings.HasPrefix(msg, "Conflict"):
		status = http.StatusConflict
	case strings.Contains(msg, "ValueTooLarge"):
		// Contains, not HasPrefix: request-body-cap errors from readJSON
		// arrive wrapped ("bad request body: ValueTooLarge…") and must
		// still answer 413.
		status = http.StatusRequestEntityTooLarge
	case strings.HasPrefix(msg, "LockHeld"):
		status = http.StatusConflict
	case strings.HasPrefix(msg, "NotHolder"):
		status = http.StatusConflict
	case strings.HasPrefix(msg, "ShardSplitting"), strings.HasPrefix(msg, "ProposalTimeout"),
		strings.HasPrefix(msg, "InsufficientReplicas"):
		// InsufficientReplicas: a blob write could not reach its policy
		// quorum right now (nodes down/joining). Retryable, same as the
		// other transient-capacity conditions.
		status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "1")
	case strings.HasPrefix(msg, "InvalidSplitKey"):
		// A manual split hint named a key outside the shard's range —
		// caller error, not a transient condition.
		status = http.StatusBadRequest
	case strings.HasPrefix(msg, "InvalidRange"):
		// A ranged blob read named an offset/length that isn't one —
		// caller error.
		status = http.StatusBadRequest
	case strings.HasPrefix(msg, "AppendUnsupported"):
		// Appending to a spliced (composite-hash) blob has no midstate to
		// resume — the caller must splice again or rewrite the blob.
		status = http.StatusBadRequest
	case strings.HasPrefix(msg, "RevisionCompacted"):
		status = http.StatusGone
	case errors.Is(err, server.ErrTxTooOld) || strings.HasPrefix(msg, "TxTooOld"):
		// The pinned read version fell behind the MVCC horizon (§10).
		// 410 Gone, like RevisionCompacted: the state the client wants no
		// longer exists. Deliberately NOT 409 — a plain retry of the same
		// request can never succeed; the client must restart the
		// transaction with fresh pins.
		status = http.StatusGone
	}
	jsonOut(w, status, map[string]string{"error": msg})
}

// authn authenticates the request's bearer token.
func (a *api) authn(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	u, err := a.s.Authenticate(tok)
	if err != nil {
		jsonOut(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return auth.User{}, false
	}
	return u, true
}

// userKey extracts and normalizes the {key} path variable: user keys
// always start with "/" (§19), and the system keyspace is not reachable
// through the user KV endpoints.
func userKey(r *http.Request) string { return "/" + mux.Vars(r)["key"] }

// check runs authorization for one key+verb, answering 403 on failure.
func (a *api) check(w http.ResponseWriter, u auth.User, key string, verb auth.Verb) bool {
	if err := a.s.Authorize(u, key, verb); err != nil {
		fail(w, err)
		return false
	}
	return true
}

// jsonBodyFloor is the minimum JSON request-body cap: generous headroom
// for every control-plane body while still bounding what one request can
// make the node buffer (§14 — login is pre-auth, so the cap must hold
// before any credential check).
const jsonBodyFloor = 8 << 20

// maxJSONBytes is the hard cap readJSON enforces. It tracks the configured
// MaxValueBytes so a kvSet body — the largest legitimate JSON request:
// one max-size value, base64-inflated 4/3 plus framing — always fits, and
// never drops below jsonBodyFloor.
func (a *api) maxJSONBytes() int64 {
	n := int64(a.s.Cfg.MaxValueBytes)
	n += n/3 + (1 << 20) // base64 expansion + JSON framing headroom
	if n < jsonBodyFloor {
		n = jsonBodyFloor
	}
	return n
}

// readJSON decodes a JSON request body under the size cap. Every JSON
// endpoint decodes through here; over-limit bodies surface as a
// ValueTooLarge error (→ 413 in fail). Raw-body endpoints — blob put,
// append, and download — stream arbitrarily large payloads by design and
// are deliberately NOT routed through this helper, and the /internal/*
// raft RPCs live in pkg/server, untouched by this cap. The state machine's
// own MaxValueBytes check stays as defense in depth behind this.
func (a *api) readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, a.maxJSONBytes())
	err := json.NewDecoder(r.Body).Decode(v)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return fmt.Errorf("ValueTooLarge: request body exceeds %d bytes", tooBig.Limit)
	}
	return err
}

// writeGate refuses user-data writes while a restore job is populating the
// cluster (§17: writes open only after restore completes and verifies).
// The gate sits at the external API edge — the restore job itself applies
// data through internal server methods and must not be blocked by it.
func (a *api) writeGate(w http.ResponseWriter) bool {
	if a.s.WriteGateActive() {
		w.Header().Set("Retry-After", "5")
		jsonOut(w, http.StatusServiceUnavailable, map[string]string{"error": "RestoreInProgress: cluster is restoring; writes open after verification"})
		return false
	}
	return true
}

// --- auth --------------------------------------------------------------------

// login: POST {username, password} → {token, expires_at}.
func (a *api) login(w http.ResponseWriter, r *http.Request) {
	var body struct{ Username, Password string }
	if err := a.readJSON(w, r, &body); err != nil {
		fail(w, fmt.Errorf("bad request body: %w", err))
		return
	}
	tok, exp, err := a.s.Login(r.Context(), body.Username, body.Password)
	if err != nil {
		// Bad credentials are 401; anything else (leader election in
		// progress, storage trouble) keeps its real status so clients
		// retry instead of misreporting an auth failure.
		if errors.Is(err, server.ErrUnauthorized) {
			jsonOut(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
			return
		}
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"token": tok, "expires_at": exp})
}

// logout: POST revokes the presented bearer token server-side (§7.1
// "revocable") — the session stops working on every node, not just this
// one, once the deletion replicates.
func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if err := a.s.TokenRevoke(r.Context(), tok); err != nil {
		fail(w, err)
		return
	}
	a.s.Audit(r.Context(), u.Name, "logout", "session token revoked")
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

// --- kv -----------------------------------------------------------------------

// systemViewPrefix is the §19 virtual namespace: system keys exposed
// read-only through the ordinary Get/List API with admin credentials.
const systemViewPrefix = ".databox/"

// shardHeaders emits the §20 debug headers: which raft groups served the
// request and how long each took. Purely informational, never blocking.
func shardHeaders(w http.ResponseWriter, calls []server.ShardCall) {
	if len(calls) == 0 {
		return
	}
	gids := make([]string, 0, len(calls))
	lats := make([]string, 0, len(calls))
	for _, c := range calls {
		if c.GID == 0 {
			continue // request failed before reaching a shard
		}
		gids = append(gids, strconv.FormatUint(c.GID, 10))
		lats = append(lats, fmt.Sprintf("%d=%.1fms", c.GID, float64(c.Elapsed.Microseconds())/1000))
	}
	if len(gids) == 0 {
		return
	}
	w.Header().Set("X-Databox-Shards", strings.Join(gids, ","))
	w.Header().Set("X-Databox-Shard-Latency", strings.Join(lats, ";"))
}

// kvGet: linearizable read → {key, value, rev, blob}.
//
// MVCC query parameters (§10):
//
//	?at_rev=N   read the key as of shard revision N (410 TxTooOld when N
//	            is older than the shard's MVCC horizon)
//	?pins=g:r,… per-shard pinned read versions; the pin for the key's
//	            shard (if any) is used as its at_rev
//	?tx=1       transaction read mode: always answers 200 with a `found`
//	            flag plus `gid` and `shard_rev`, so the client can pin the
//	            shard's read version even when the key does not exist
func (a *api) kvGet(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	// `.databox/` keys route to the read-only system view (§19): same
	// data as /api/v1/system, reachable through the normal KV GET with
	// admin authorization.
	if raw := mux.Vars(r)["key"]; strings.HasPrefix(raw, systemViewPrefix) {
		if err := a.s.AuthorizeAdmin(u); err != nil {
			fail(w, err)
			return
		}
		sysKey := strings.TrimPrefix(raw, systemViewPrefix)
		rec, found, err := a.s.SystemGet(sysKey)
		if err != nil {
			fail(w, err)
			return
		}
		if !found {
			fail(w, server.ErrNotFound)
			return
		}
		jsonOut(w, http.StatusOK, map[string]any{"key": systemViewPrefix + sysKey, "value": rec.Value, "rev": rec.Rev})
		return
	}
	key := userKey(r)
	if !a.check(w, u, key, auth.VerbRead) {
		return
	}
	q := r.URL.Query()
	atRev, _ := strconv.ParseUint(q.Get("at_rev"), 10, 64)
	txMode := q.Get("tx") == "1"
	if atRev == 0 && !txMode && !q.Has("pins") {
		// Plain read: the pre-MVCC path and response shape, untouched
		// (plus the §20 debug headers).
		rec, found, call, err := a.s.KVGetTraced(r.Context(), key)
		shardHeaders(w, []server.ShardCall{call})
		if err != nil {
			fail(w, err)
			return
		}
		if !found {
			fail(w, server.ErrNotFound)
			return
		}
		jsonOut(w, http.StatusOK, map[string]any{"key": key, "value": rec.Value, "rev": rec.Rev, "blob": rec.Blob})
		return
	}
	rec, found, gid, shardRev, err := a.s.KVGetVersioned(r.Context(), key, parsePins(q.Get("pins")), atRev)
	if err != nil {
		fail(w, err)
		return
	}
	if txMode {
		// Transaction reads treat "not found" as a successful snapshot
		// observation (the tx validates it at commit as revision 0), so
		// it answers 200 — and always carries the pin information.
		out := map[string]any{"key": key, "found": found, "gid": gid, "shard_rev": shardRev}
		if found {
			out["value"], out["rev"], out["blob"] = rec.Value, rec.Rev, rec.Blob
		}
		jsonOut(w, http.StatusOK, out)
		return
	}
	if !found {
		fail(w, server.ErrNotFound)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{
		"key": key, "value": rec.Value, "rev": rec.Rev, "blob": rec.Blob,
		"gid": gid, "shard_rev": shardRev,
	})
}

// parsePins decodes the ?pins= parameter: comma-separated gid:rev pairs,
// e.g. "3:1042,7:88". Malformed pairs are ignored (a missing pin just
// means the read executes at latest, which is always safe).
func parsePins(s string) map[uint64]uint64 {
	if s == "" {
		return nil
	}
	pins := map[uint64]uint64{}
	for _, pair := range strings.Split(s, ",") {
		gidStr, revStr, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}
		gid, err1 := strconv.ParseUint(gidStr, 10, 64)
		rev, err2 := strconv.ParseUint(revStr, 10, 64)
		if err1 == nil && err2 == nil && gid > 0 {
			pins[gid] = rev
		}
	}
	return pins
}

// kvSet: PUT {value: base64} → {rev}.
func (a *api) kvSet(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	if strings.HasPrefix(mux.Vars(r)["key"], systemViewPrefix) {
		fail(w, fmt.Errorf("Unauthorized: the .databox/ system view is read-only (§19)"))
		return
	}
	key := userKey(r)
	if !a.check(w, u, key, auth.VerbWrite) || !a.writeGate(w) {
		return
	}
	var body struct {
		Value []byte `json:"value"`
	}
	if err := a.readJSON(w, r, &body); err != nil {
		fail(w, fmt.Errorf("bad request body: %w", err))
		return
	}
	rev, err := a.s.KVSet(r.Context(), key, body.Value, false)
	if err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"rev": rev})
}

// kvDelete: DELETE → {rev}.
func (a *api) kvDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	if strings.HasPrefix(mux.Vars(r)["key"], systemViewPrefix) {
		fail(w, fmt.Errorf("Unauthorized: the .databox/ system view is read-only (§19)"))
		return
	}
	key := userKey(r)
	if !a.check(w, u, key, auth.VerbDelete) || !a.writeGate(w) {
		return
	}
	rev, err := a.s.KVDelete(r.Context(), key)
	if err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"rev": rev})
}

// kvDeleteRange: POST {start, end}. The caller must be authorized for the
// ENTIRE range [start, end), not just its start key: `start` passes the
// ordinary per-key check, and — for everyone but root — `end` must be
// bounded and the whole range must fall inside one allow grant with no
// intervening deny (rangeCoveredByGrants). Root keeps unbounded ranges
// (it bypasses Authorize everywhere else too).
func (a *api) kvDeleteRange(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	var body struct{ Start, End string }
	if err := a.readJSON(w, r, &body); err != nil || body.Start == "" {
		fail(w, fmt.Errorf("body must be {start, end}"))
		return
	}
	if !a.check(w, u, body.Start, auth.VerbDelete) {
		return
	}
	if u.Name != auth.RootUser {
		if body.End == "" {
			// An unbounded range sweeps everything sorting after start —
			// never safe for a scoped caller, whatever their grants.
			fail(w, fmt.Errorf("Unauthorized: delete-range requires a bounded end within your granted prefix"))
			return
		}
		if !rangeCoveredByGrants(u.Grants, body.Start, body.End, auth.VerbDelete) {
			fail(w, fmt.Errorf("Unauthorized: user %q lacks %q over the whole range [%q, %q)", u.Name, auth.VerbDelete, body.Start, body.End))
			return
		}
	}
	if !a.writeGate(w) {
		return
	}
	if err := a.s.KVDeleteRange(r.Context(), body.Start, body.End); err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

// prefixUpperBound returns the smallest key sorting after every key that
// has the given prefix — the prefix with its last non-0xff byte
// incremented (trailing 0xff bytes dropped). "" means no finite bound
// exists (an all-0xff prefix).
func prefixUpperBound(p string) string {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return ""
}

// grantMentions reports whether a grant covers the verb (mirrors the
// unexported auth.Grant.mentions).
func grantMentions(g auth.Grant, v auth.Verb) bool {
	for _, gv := range g.Verbs {
		if gv == v {
			return true
		}
	}
	return false
}

// rangeCoveredByGrants decides whether EVERY key in [start, end) is
// authorized for the verb under the §7.2 grant model. Prefix grants make
// this exactly decidable, no key enumeration needed:
//
//  1. There must be an allow grant whose prefix subtree contains the whole
//     range: start carries the prefix and end does not sort past the
//     prefix's upper bound — then any key in [start, end) carries the
//     prefix too. The longest such grant is the deciding one.
//  2. No deny grant at least as specific as that allow may intersect the
//     range: longest-prefix-wins (deny on length ties) would refuse the
//     keys under it, so the range is not wholly authorized. Denies less
//     specific than the covering allow lose to it for every key in the
//     range and are ignored — matching exactly what auth.Allowed answers
//     per key.
func rangeCoveredByGrants(grants []auth.Grant, start, end string, verb auth.Verb) bool {
	if end == "" {
		return false
	}
	covering := -1
	for i, g := range grants {
		if g.Effect != "allow" || !grantMentions(g, verb) || !strings.HasPrefix(start, g.Prefix) {
			continue
		}
		if up := prefixUpperBound(g.Prefix); up != "" && end > up {
			continue // range escapes this grant's subtree
		}
		if covering == -1 || len(g.Prefix) > len(grants[covering].Prefix) {
			covering = i
		}
	}
	if covering == -1 {
		return false
	}
	allowLen := len(grants[covering].Prefix)
	for _, g := range grants {
		if g.Effect != "deny" || !grantMentions(g, verb) || len(g.Prefix) < allowLen {
			continue
		}
		// Does the deny's subtree [prefix, upperBound) intersect [start, end)?
		if g.Prefix >= end {
			continue
		}
		if up := prefixUpperBound(g.Prefix); up != "" && up <= start {
			continue
		}
		return false
	}
	return true
}

// kvList: GET ?prefix&cursor&limit → {entries, next_cursor}.
//
// MVCC query parameters (§9/§10):
//
//	?at_rev=N   scan every touched shard as of revision N (meaningful when
//	            the prefix lives in a single shard — revisions are
//	            per-shard counters)
//	?pins=g:r,… per-shard pinned read versions; pinned shards scan at
//	            their pin, unpinned shards at latest. The response's
//	            shard_revs map reports the revision each shard scanned
//	            at, so transactions can pin lazily. Presence of the pins
//	            parameter (even empty) selects the versioned path.
func (a *api) kvList(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	prefix := q.Get("prefix")
	if prefix == "" {
		prefix = "/"
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	atRev, _ := strconv.ParseUint(q.Get("at_rev"), 10, 64)

	type row struct {
		Key   string `json:"key"`
		Value []byte `json:"value"`
		Rev   uint64 `json:"rev"`
		Blob  bool   `json:"blob,omitempty"`
	}
	toRows := func(entries []kv.ListEntry) []row {
		rows := make([]row, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, row{Key: e.Key, Value: e.Record.Value, Rev: e.Record.Rev, Blob: e.Record.Blob})
		}
		return rows
	}

	// `.databox/` prefixes route to the read-only system view (§19) —
	// admin credentials, metadata keyspace, same cursor semantics.
	if strings.HasPrefix(prefix, systemViewPrefix) {
		if err := a.s.AuthorizeAdmin(u); err != nil {
			fail(w, err)
			return
		}
		if limit <= 0 || limit > 10000 {
			limit = 1000
		}
		sysPrefix := strings.TrimPrefix(prefix, systemViewPrefix)
		cursor := strings.TrimPrefix(q.Get("cursor"), systemViewPrefix)
		entries, next, err := a.s.SystemListPage(sysPrefix, cursor, limit)
		if err != nil {
			fail(w, err)
			return
		}
		rows := make([]row, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, row{Key: systemViewPrefix + e.Key, Value: e.Record.Value, Rev: e.Record.Rev})
		}
		if next != "" {
			next = systemViewPrefix + next
		}
		jsonOut(w, http.StatusOK, map[string]any{"entries": rows, "next_cursor": next})
		return
	}

	if !a.check(w, u, prefix, auth.VerbList) {
		return
	}
	if atRev > 0 || q.Has("pins") {
		entries, next, shardRevs, err := a.s.KVListVersioned(r.Context(), prefix, q.Get("cursor"), limit, parsePins(q.Get("pins")), atRev)
		if err != nil {
			fail(w, err)
			return
		}
		// Debug headers (§20): the versioned path reports which shards it
		// scanned via shard_revs; per-shard timing is not split out here.
		gids := make([]server.ShardCall, 0, len(shardRevs))
		for gid := range shardRevs {
			gids = append(gids, server.ShardCall{GID: gid})
		}
		shardHeadersGIDsOnly(w, gids)
		jsonOut(w, http.StatusOK, map[string]any{"entries": toRows(entries), "next_cursor": next, "shard_revs": shardRevs})
		return
	}
	entries, next, calls, err := a.s.KVListTraced(prefix, q.Get("cursor"), limit)
	shardHeaders(w, calls)
	if err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"entries": toRows(entries), "next_cursor": next})
}

// shardHeadersGIDsOnly emits X-Databox-Shards without latency figures —
// used where per-shard timing is not individually measured.
func shardHeadersGIDsOnly(w http.ResponseWriter, calls []server.ShardCall) {
	gids := make([]string, 0, len(calls))
	for _, c := range calls {
		if c.GID != 0 {
			gids = append(gids, strconv.FormatUint(c.GID, 10))
		}
	}
	if len(gids) > 0 {
		sort.Strings(gids)
		w.Header().Set("X-Databox-Shards", strings.Join(gids, ","))
	}
}

// watch: GET ?prefix&from_revision → NDJSON event stream (§9.2).
func (a *api) watch(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	prefix := q.Get("prefix")
	if prefix == "" {
		prefix = "/"
	}
	if !a.check(w, u, prefix, auth.VerbWatch) {
		return
	}
	var from uint64
	if v := q.Get("from_revision"); v != "" {
		from, _ = strconv.ParseUint(v, 10, 64)
	}
	// Preflight BEFORE committing to a 200 + stream (§9.2): a compacted
	// from_revision answers a proper 410 RevisionCompacted here, not an
	// in-body error line the client has to fish out of the stream.
	gids, err := a.s.WatchPreflight(r.Context(), prefix, from)
	if err != nil {
		fail(w, err)
		return
	}
	// Debug header (§20): the raft groups this watch spans.
	shardHeadersGIDsOnly(w, toShardCalls(gids))
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	err = a.s.Watch(r.Context(), prefix, from, func(ev kv.Event) error {
		if err := enc.Encode(ev); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	})
	if err != nil && r.Context().Err() == nil {
		// Stream already started; append a terminal error line. The Go
		// client surfaces these as errors from Watch (see pkg/client).
		_ = enc.Encode(map[string]string{"error": err.Error()})
	}
}

// toShardCalls adapts a bare GID list to the header helper's input.
func toShardCalls(gids []uint64) []server.ShardCall {
	out := make([]server.ShardCall, 0, len(gids))
	for _, g := range gids {
		out = append(out, server.ShardCall{GID: g})
	}
	return out
}

// --- transactions ---------------------------------------------------------------

// txBegin: POST → {txid, read_versions}. See §10 — the protocol is
// stateless server-side. Read versions are captured LAZILY, not here:
// revisions are per-shard, and pinning every shard up front would either
// serialize tx/begin through every group or hand out versions for shards
// the transaction never touches. Instead read_versions starts empty and
// the client fills it as it reads — each read response carries the shard
// revision it executed at (gid + shard_rev), the client pins the first one
// per shard, and later reads to that shard are sent with ?pins= so they
// execute at the pinned revision (snapshot semantics per shard).
func (a *api) txBegin(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authn(w, r); !ok {
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{
		"txid":          a.s.TxBegin(),
		"read_versions": map[uint64]uint64{}, // filled lazily by the client, see above
	})
}

// txCommit: POST {reads: {key: rev}, writes: [{key, value?, delete?}]}
// → {rev} or 409 Conflict. Reads need read permission, writes write/delete.
func (a *api) txCommit(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	var body struct {
		Reads  map[string]uint64 `json:"reads"`
		Writes []kv.TxWrite      `json:"writes"`
	}
	if err := a.readJSON(w, r, &body); err != nil {
		fail(w, fmt.Errorf("bad request body: %w", err))
		return
	}
	if len(body.Writes) > 0 && !a.writeGate(w) {
		return
	}
	for key := range body.Reads {
		if !a.check(w, u, key, auth.VerbRead) {
			return
		}
	}
	for _, wr := range body.Writes {
		verb := auth.VerbWrite
		if wr.Delete {
			verb = auth.VerbDelete
		}
		if !a.check(w, u, wr.Key, verb) {
			return
		}
	}
	ctx := r.Context()
	rev, err := a.s.TxCommit(ctx, body.Reads, body.Writes)
	if err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"rev": rev})
}

// --- locks -----------------------------------------------------------------------

// lockAcquire: POST {resource, mode, ttl_ms} → {fencing}. The holder
// identity is the authenticated user plus an optional client-supplied
// suffix (so one user can hold distinct handles).
func (a *api) lockAcquire(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	var body struct {
		Resource string `json:"resource"`
		Mode     string `json:"mode"`
		TTLms    int64  `json:"ttl_ms"`
		Handle   string `json:"handle"`
	}
	if err := a.readJSON(w, r, &body); err != nil || body.Resource == "" {
		fail(w, fmt.Errorf("body must include resource"))
		return
	}
	if !a.check(w, u, body.Resource, auth.VerbLock) {
		return
	}
	holder := u.Name
	if body.Handle != "" {
		holder += "/" + body.Handle
	}
	fencing, err := a.s.LockAcquire(r.Context(), body.Resource, holder, body.Mode, time.Duration(body.TTLms)*time.Millisecond)
	if err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"fencing": fencing, "holder": holder})
}

// lockRelease: POST {resource, handle?}.
func (a *api) lockRelease(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	var body struct{ Resource, Handle string }
	if err := a.readJSON(w, r, &body); err != nil || body.Resource == "" {
		fail(w, fmt.Errorf("body must include resource"))
		return
	}
	if !a.check(w, u, body.Resource, auth.VerbLock) {
		return
	}
	holder := u.Name
	if body.Handle != "" {
		holder += "/" + body.Handle
	}
	if err := a.s.LockRelease(r.Context(), body.Resource, holder); err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

// lockForce: POST {resource, reason} — admin only, audited (§9).
func (a *api) lockForce(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	if err := a.s.AuthorizeAdmin(u); err != nil {
		fail(w, err)
		return
	}
	var body struct{ Resource, Reason string }
	if err := a.readJSON(w, r, &body); err != nil || body.Resource == "" {
		fail(w, fmt.Errorf("body must include resource"))
		return
	}
	if err := a.s.LockForceUnlock(r.Context(), body.Resource, u.Name, body.Reason); err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

// lockCheck: GET /locks/{resource} → current state + fencing token.
func (a *api) lockCheck(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	resource := mux.Vars(r)["resource"]
	if !a.check(w, u, resource, auth.VerbLock) {
		return
	}
	rec, found, err := a.s.LockCheck(resource)
	if err != nil {
		fail(w, err)
		return
	}
	if !found {
		jsonOut(w, http.StatusOK, map[string]any{"locked": false})
		return
	}
	var state json.RawMessage = rec.Value
	jsonOut(w, http.StatusOK, map[string]any{"locked": true, "state": state})
}

// --- blobs -------------------------------------------------------------------------

// blobPut: PUT with the raw blob as the request body → manifest summary.
func (a *api) blobPut(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	key := userKey(r)
	if !a.check(w, u, key, auth.VerbWrite) || !a.writeGate(w) {
		return
	}
	m, rev, err := a.s.PutBlob(r.Context(), key, r.Body, r.Header.Get("Content-Type"))
	if err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"rev": rev, "size": m.Size, "sha256": m.SHA256, "mode": m.Mode})
}

// blobAppend: PATCH with the raw bytes to append as the request body →
// updated manifest summary. Conflict (409) means a concurrent append won;
// retry the whole call.
func (a *api) blobAppend(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	key := userKey(r)
	if !a.check(w, u, key, auth.VerbWrite) || !a.writeGate(w) {
		return
	}
	m, rev, err := a.s.AppendBlob(r.Context(), key, r.Body)
	if err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"rev": rev, "size": m.Size, "sha256": m.SHA256, "mode": m.Mode})
}

// blobGet: GET → raw blob bytes, streamed with hash verification.
// Optional ?offset= and ?length= select a byte window: chunks outside it
// are never read (the manifest's per-chunk sizes make the seek pure
// arithmetic), which is what lets databox consumers serve HTTP Range —
// video seeking — cheaply. length omitted or -1 means "to the end".
func (a *api) blobGet(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	key := userKey(r)
	if !a.check(w, u, key, auth.VerbRead) {
		return
	}
	m, err := a.s.StatBlob(r.Context(), key)
	if err != nil {
		fail(w, err)
		return
	}
	offset, length := int64(0), int64(-1)
	if v := r.URL.Query().Get("offset"); v != "" {
		if offset, err = strconv.ParseInt(v, 10, 64); err != nil || offset < 0 {
			fail(w, fmt.Errorf("InvalidRange: bad offset %q", v))
			return
		}
	}
	if v := r.URL.Query().Get("length"); v != "" {
		if length, err = strconv.ParseInt(v, 10, 64); err != nil {
			fail(w, fmt.Errorf("InvalidRange: bad length %q", v))
			return
		}
	}
	if offset > m.Size {
		fail(w, fmt.Errorf("InvalidRange: offset %d beyond blob size %d", offset, m.Size))
		return
	}
	window := m.Size - offset
	if length >= 0 && length < window {
		window = length
	}
	ct := m.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(window, 10))
	w.Header().Set("X-Databox-SHA256", m.SHA256)
	if offset == 0 && length < 0 {
		if _, err := a.s.GetBlob(r.Context(), key, w); err != nil {
			// Headers are gone; the truncated body plus closed connection
			// signals the failure. Log-worthy but nothing else to do.
			return
		}
		return
	}
	if _, err := a.s.GetBlobRange(r.Context(), key, w, offset, window); err != nil {
		return
	}
}

// blobHead: HEAD → size/hash headers only.
func (a *api) blobHead(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	key := userKey(r)
	if !a.check(w, u, key, auth.VerbRead) {
		return
	}
	m, err := a.s.StatBlob(r.Context(), key)
	if err != nil {
		fail(w, err)
		return
	}
	if m.ContentType != "" {
		w.Header().Set("Content-Type", m.ContentType)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
	w.Header().Set("X-Databox-SHA256", m.SHA256)
	w.WriteHeader(http.StatusOK)
}

// blobDelete: DELETE → removes the manifest (chunks GC later).
func (a *api) blobDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	key := userKey(r)
	if !a.check(w, u, key, auth.VerbDelete) || !a.writeGate(w) {
		return
	}
	if err := a.s.DeleteBlob(r.Context(), key); err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

// blobSplice: POST {"destination","sources","content_type"} → commits at
// destination one blob whose content is the ordered concatenation of the
// source blobs, by chunk-map splice (§25) — no data bytes move. Sources
// are left in place (delete them separately if temporary; shared chunks
// survive). Conflict (409) means a concurrent write won; retry the call.
func (a *api) blobSplice(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	var body struct {
		Destination string   `json:"destination"`
		Sources     []string `json:"sources"`
		ContentType string   `json:"content_type"`
	}
	if err := a.readJSON(w, r, &body); err != nil || body.Destination == "" || len(body.Sources) == 0 {
		jsonOut(w, http.StatusBadRequest, map[string]any{"error": "destination and at least one source required"})
		return
	}
	// Write on the destination, read on every source — same verbs a
	// download-then-upload of the same content would need.
	if !a.check(w, u, body.Destination, auth.VerbWrite) || !a.writeGate(w) {
		return
	}
	for _, src := range body.Sources {
		if !a.check(w, u, src, auth.VerbRead) {
			return
		}
	}
	m, rev, err := a.s.SpliceBlobs(r.Context(), body.Destination, body.Sources, body.ContentType)
	if err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"rev": rev, "size": m.Size, "sha256": m.SHA256, "composite": m.Composite, "mode": m.Mode})
}

// --- users & grants (admin-gated, §7.3) ---------------------------------------------

func (a *api) adminOnly(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	u, ok := a.authn(w, r)
	if !ok {
		return auth.User{}, false
	}
	if err := a.s.AuthorizeAdmin(u); err != nil {
		fail(w, err)
		return auth.User{}, false
	}
	return u, true
}

func (a *api) userList(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.adminOnly(w, r); !ok {
		return
	}
	users, err := a.s.UserList()
	if err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"users": users})
}

func (a *api) userCreate(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminOnly(w, r)
	if !ok {
		return
	}
	var body struct{ Name, Password string }
	if err := a.readJSON(w, r, &body); err != nil {
		fail(w, err)
		return
	}
	if err := a.s.UserCreate(r.Context(), u.Name, body.Name, body.Password); err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *api) userDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminOnly(w, r)
	if !ok {
		return
	}
	if err := a.s.UserDelete(r.Context(), u.Name, mux.Vars(r)["name"]); err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

// userPasswd: users may change their own password; admins anyone's.
func (a *api) userPasswd(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	target := mux.Vars(r)["name"]
	if u.Name != target {
		if err := a.s.AuthorizeAdmin(u); err != nil {
			fail(w, err)
			return
		}
	}
	var body struct{ Password string }
	if err := a.readJSON(w, r, &body); err != nil || body.Password == "" {
		fail(w, fmt.Errorf("body must include password"))
		return
	}
	if err := a.s.UserSetPassword(r.Context(), u.Name, target, body.Password); err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *api) grantAdd(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminOnly(w, r)
	if !ok {
		return
	}
	var g auth.Grant
	if err := a.readJSON(w, r, &g); err != nil {
		fail(w, err)
		return
	}
	if err := a.s.GrantAdd(r.Context(), u.Name, mux.Vars(r)["name"], g); err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *api) grantRemove(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminOnly(w, r)
	if !ok {
		return
	}
	var body struct{ Prefix, Effect string }
	if err := a.readJSON(w, r, &body); err != nil {
		fail(w, err)
		return
	}
	if err := a.s.GrantRemove(r.Context(), u.Name, mux.Vars(r)["name"], body.Prefix, body.Effect); err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *api) accessKeyCreate(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	target := mux.Vars(r)["name"]
	if u.Name != target {
		if err := a.s.AuthorizeAdmin(u); err != nil {
			fail(w, err)
			return
		}
	}
	// Optional scope prefixes narrow the key below the user's grants.
	var body struct {
		Scopes []string `json:"scopes"`
	}
	_ = a.readJSON(w, r, &body) // empty body = unscoped
	key, err := a.s.AccessKeyCreate(r.Context(), u.Name, target, body.Scopes)
	if err != nil {
		fail(w, err)
		return
	}
	// The one and only time the secret is shown (§7.1).
	jsonOut(w, http.StatusOK, key)
}

// accessKeyList shows a user's keys (secrets redacted); self or admin.
func (a *api) accessKeyList(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	target := mux.Vars(r)["name"]
	if u.Name != target {
		if err := a.s.AuthorizeAdmin(u); err != nil {
			fail(w, err)
			return
		}
	}
	keys, err := a.s.AccessKeyList(target)
	if err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"access_keys": keys})
}

// accessKeyDelete revokes one of a user's keys; self or admin.
func (a *api) accessKeyDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := a.authn(w, r)
	if !ok {
		return
	}
	target := mux.Vars(r)["name"]
	if u.Name != target {
		if err := a.s.AuthorizeAdmin(u); err != nil {
			fail(w, err)
			return
		}
	}
	if err := a.s.AccessKeyDelete(r.Context(), u.Name, target, mux.Vars(r)["key"]); err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

// --- cluster management -----------------------------------------------------------

func (a *api) clusterStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.adminOnly(w, r); !ok {
		return
	}
	report, err := a.s.Status()
	if err != nil {
		fail(w, err)
		return
	}
	// Pending manual split hints ride alongside the report (§15): queued
	// operator intent belongs in status. Best-effort — a hint-read hiccup
	// must not take down the whole status answer.
	hints, err := a.s.SplitHintsPending()
	if err != nil {
		hints = nil
	}
	jsonOut(w, http.StatusOK, struct {
		*server.StatusReport
		SplitHints []cluster.SplitHint `json:"split_hints,omitempty"`
	}{report, hints})
}

func (a *api) joinToken(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.adminOnly(w, r); !ok {
		return
	}
	var body struct {
		TTL string `json:"ttl"`
	}
	_ = a.readJSON(w, r, &body)
	ttl, _ := time.ParseDuration(body.TTL)
	tok, err := a.s.MintJoinToken(r.Context(), ttl)
	if err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]string{"token": tok})
}

func (a *api) decommission(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminOnly(w, r)
	if !ok {
		return
	}
	var body struct {
		NodeID uint64 `json:"node_id"`
		Force  bool   `json:"force"`
	}
	if err := a.readJSON(w, r, &body); err != nil || body.NodeID == 0 {
		fail(w, fmt.Errorf("body must include node_id"))
		return
	}
	if err := a.s.Decommission(r.Context(), body.NodeID, u.Name, body.Force); err != nil {
		fail(w, err)
		return
	}
	// The guided-removal message (§16.3): tell the operator exactly what
	// to check before touching another node.
	jsonOut(w, http.StatusOK, map[string]any{
		"ok": true,
		"guidance": "Decommission started. Before removing another node, run `databox cluster status` " +
			"and wait until safe_to_proceed is true and this node no longer appears in any group.",
	})
}

// --- automation pause/resume (§16.4) --------------------------------------------------

// adminPause: POST /admin/{rebalance|split|repair}/{pause|resume}.
// Admin-only; the server audits the flag change.
func (a *api) adminPause(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminOnly(w, r)
	if !ok {
		return
	}
	target := mux.Vars(r)["target"]
	paused := mux.Vars(r)["action"] == "pause"
	if err := a.s.AdminPause(r.Context(), u.Name, target, paused); err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"ok": true, "target": target, "paused": paused})
}

// adminShardSplit: POST /admin/shards/{gid}/split, optional body
// {"at": "<key>"} for an explicit split key (default: the range's median).
// Admin-only, audited server-side. Records a hint the reconciler consumes
// on its next tick — the split itself is asynchronous, like every other
// controller action; progress is visible in cluster status.
func (a *api) adminShardSplit(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminOnly(w, r)
	if !ok {
		return
	}
	gid, err := strconv.ParseUint(mux.Vars(r)["gid"], 10, 64)
	if err != nil || gid == 0 {
		fail(w, fmt.Errorf("InvalidSplitKey: gid must be a positive integer"))
		return
	}
	var body struct {
		At string `json:"at"`
	}
	// The body is optional (no body = median split); a present-but-broken
	// body is a caller error worth rejecting rather than silently ignoring.
	if err := a.readJSON(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
		fail(w, fmt.Errorf("bad request body: %w", err))
		return
	}
	if err := a.s.SplitHintRequest(r.Context(), u.Name, gid, body.At); err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{
		"ok": true, "gid": gid, "at": body.At,
		"note": "split hint recorded; the reconciler acts on its next tick (unless splitting is paused) — watch `databox cluster status`",
	})
}

// --- durability policies (§12) --------------------------------------------------------

// policyRef pulls and normalizes the {kind}/{path} pair from the route.
// Stored policy paths always start with "/" (they name user-keyspace
// subtrees); the URL form omits the leading slash.
func policyRef(r *http.Request) (kind, path string) {
	v := mux.Vars(r)
	return v["kind"], "/" + v["path"]
}

// policyList: GET /policies/{kind} → {policies: {path: rule}}.
func (a *api) policyList(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.adminOnly(w, r); !ok {
		return
	}
	rules, err := a.s.PolicyList(mux.Vars(r)["kind"])
	if err != nil {
		fail(w, err)
		return
	}
	out := make(map[string]json.RawMessage, len(rules))
	for path, raw := range rules {
		out[path] = json.RawMessage(raw)
	}
	jsonOut(w, http.StatusOK, map[string]any{"policies": out})
}

// policyGet: GET /policies/{kind}/{path} → the stored rule JSON.
func (a *api) policyGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.adminOnly(w, r); !ok {
		return
	}
	kind, path := policyRef(r)
	raw, found, err := a.s.PolicyGet(kind, path)
	if err != nil {
		fail(w, err)
		return
	}
	if !found {
		fail(w, server.ErrNotFound)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"kind": kind, "path": path, "rule": json.RawMessage(raw)})
}

// policySet: PUT /policies/{kind}/{path} with the rule JSON as the body
// ({"replicas":N} or {"data":D,"parity":P,"enabled":B}). Validated before
// commit; audited.
func (a *api) policySet(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminOnly(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		fail(w, err)
		return
	}
	kind, path := policyRef(r)
	if err := a.s.PolicySet(r.Context(), kind, path, body); err != nil {
		fail(w, err)
		return
	}
	a.s.Audit(r.Context(), u.Name, "policy-set", fmt.Sprintf("kind=%s path=%s", kind, path))
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

// policyDelete: DELETE /policies/{kind}/{path}; audited.
func (a *api) policyDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminOnly(w, r)
	if !ok {
		return
	}
	kind, path := policyRef(r)
	if err := a.s.PolicyDelete(r.Context(), kind, path); err != nil {
		fail(w, err)
		return
	}
	a.s.Audit(r.Context(), u.Name, "policy-delete", fmt.Sprintf("kind=%s path=%s", kind, path))
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

// --- system view (`.databox/`, §19) --------------------------------------------------

func (a *api) systemGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.adminOnly(w, r); !ok {
		return
	}
	key := strings.TrimPrefix(mux.Vars(r)["key"], ".databox/")
	rec, found, err := a.s.SystemGet(key)
	if err != nil {
		fail(w, err)
		return
	}
	if !found {
		fail(w, server.ErrNotFound)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"key": key, "value": rec.Value, "rev": rec.Rev})
}

func (a *api) systemList(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.adminOnly(w, r); !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 1000
	}
	prefix := strings.TrimPrefix(q.Get("prefix"), ".databox/")
	entries, err := a.s.SystemList(prefix, limit)
	if err != nil {
		fail(w, err)
		return
	}
	type row struct {
		Key   string `json:"key"`
		Value []byte `json:"value"`
		Rev   uint64 `json:"rev"`
	}
	rows := make([]row, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, row{Key: e.Key, Value: e.Record.Value, Rev: e.Record.Rev})
	}
	jsonOut(w, http.StatusOK, map[string]any{"entries": rows})
}
