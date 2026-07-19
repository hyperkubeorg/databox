// admin_ops.go implements the remaining admin-gated operations views
// (§4 Web Portal, admin audience):
//
//	/policies   durability policy management (§12): list, set, delete the
//	            stored replication/EC rules, plus a resolver that shows
//	            which rule wins for a sample path
//	/locks      lock inspection (§9): every active lock with holder, mode,
//	            fencing token and TTL, and the audited force-unlock button
//	/audit      the audit trail: newest-first list of audited operations,
//	            filterable by actor and action — strictly read-only
//
// All three follow the /users pattern exactly: requireAdmin on every
// handler, CSRF on every POST, mutations redirect back to their page with
// a one-shot ?notice=. Lock and audit state live in the metadata keyspace
// ("locks/", "audit/") and are read through the same SystemList/
// SystemListPage surface as the .databox/ explorer.
package frontend

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/blob"
	"github.com/hyperkubeorg/databox/pkg/server"
)

// --- policy management (§12) ---------------------------------------------------

// policyRow is one stored rule in the policies tables.
type policyRow struct {
	Path string // the governed key subtree, e.g. /logs
	JSON string // the rule value, pretty enough as-is (they are tiny)
}

// policySample is the "which rule wins" resolver result for a typed path.
type policySample struct {
	Key       string      // the sample key the admin asked about
	Policy    blob.Policy // the fully resolved effective policy
	ReplRule  string      // winning replication rule path ("" = built-in default)
	ECRule    string      // winning EC rule path ("" = built-in default)
	ECEnabled bool        // convenience for the template (Policy.ECEnabled)
}

// policiesData feeds policies.tpl.
type policiesData struct {
	Replication []policyRow
	EC          []policyRow
	// Defaults describes the built-in fallback (§12): rs-4-2 EC for large
	// blobs, N-replica for small ones — shown so admins see the whole rule
	// set, not just their overrides.
	Defaults blob.Policy
	// Sample is the resolver panel result; nil until a path is typed.
	Sample *policySample
	Notice string
}

// policyRows sorts a PolicyList result into stable table rows.
func policyRows(rules map[string][]byte) []policyRow {
	paths := make([]string, 0, len(rules))
	for p := range rules {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	rows := make([]policyRow, 0, len(paths))
	for _, p := range paths {
		rows = append(rows, policyRow{Path: p, JSON: string(rules[p])})
	}
	return rows
}

// policiesPage lists both rule families and, when ?sample= names a path,
// resolves the effective policy for it exactly the way blob writes do.
func (g *gui) policiesPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireAdmin(w, r)
	if !ok {
		return
	}
	repl, err := g.s.PolicyList(server.PolicyKindReplication)
	if err != nil {
		g.failPage(w, r, u, err)
		return
	}
	ec, err := g.s.PolicyList(server.PolicyKindEC)
	if err != nil {
		g.failPage(w, r, u, err)
		return
	}
	d := &policiesData{
		Replication: policyRows(repl),
		EC:          policyRows(ec),
		// DefaultPolicy(0) is the engine's built-in fallback: 2 replicas
		// for small blobs, rs-4-2 EC — the same defaults pkg/server wires
		// into the blob engine.
		Defaults: blob.DefaultPolicy(0),
		Notice:   r.URL.Query().Get("notice"),
	}
	// Resolver: run the SAME resolution blob writes use (most specific
	// path wins per family, defaults fill the rest) and also report which
	// stored rule matched, so the admin sees why.
	if sample := r.URL.Query().Get("sample"); sample != "" {
		s := &policySample{Key: sample, Policy: blob.ResolvePolicy(sample, repl, ec, d.Defaults)}
		if p, ok := blob.BestMatch(sample, ruleKeyList(repl)); ok {
			s.ReplRule = p
		}
		if p, ok := blob.BestMatch(sample, ruleKeyList(ec)); ok {
			s.ECRule = p
		}
		s.ECEnabled = s.Policy.ECEnabled
		d.Sample = s
	}
	g.render(w, http.StatusOK, "policies.tpl", g.page(r, u, "Policies", d))
}

// ruleKeyList extracts the paths from a PolicyList map for BestMatch.
func ruleKeyList(rules map[string][]byte) []string {
	out := make([]string, 0, len(rules))
	for p := range rules {
		out = append(out, p)
	}
	return out
}

// backToPolicies redirects a completed action home with a one-shot notice.
func backToPolicies(w http.ResponseWriter, r *http.Request, notice string) {
	http.Redirect(w, r, "/policies?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

// policySet stores one rule. The form is structured per kind — replicas
// for replication, geometry + enabled for EC — and the JSON is built here
// server-side, so admins never hand-type JSON. PolicySet validates the
// rule (kind, path shape, value ranges) before committing it.
func (g *gui) policySet(w http.ResponseWriter, r *http.Request) {
	u, ok := g.adminForm(w, r)
	if !ok {
		return
	}
	kind := r.FormValue("kind")
	path := r.FormValue("path")
	var value []byte
	switch kind {
	case server.PolicyKindReplication:
		n, err := strconv.Atoi(r.FormValue("replicas"))
		if err != nil {
			g.errorPage(w, r, u, http.StatusBadRequest, "replicas must be a number")
			return
		}
		value, _ = json.Marshal(blob.ReplicationRule{Replicas: n})
	case server.PolicyKindEC:
		// enabled=off with geometry ignored is a valid "replicate this
		// subtree" rule; ParseECRule (inside PolicySet) enforces the rest.
		rule := blob.ECRule{Enabled: r.FormValue("enabled") == "1"}
		if rule.Enabled {
			var err error
			if rule.Data, err = strconv.Atoi(r.FormValue("data")); err != nil {
				g.errorPage(w, r, u, http.StatusBadRequest, "data shards must be a number")
				return
			}
			if rule.Parity, err = strconv.Atoi(r.FormValue("parity")); err != nil {
				g.errorPage(w, r, u, http.StatusBadRequest, "parity shards must be a number")
				return
			}
		}
		value, _ = json.Marshal(rule)
	default:
		g.errorPage(w, r, u, http.StatusBadRequest, "unknown policy kind")
		return
	}
	if err := g.s.PolicySet(r.Context(), kind, path, value); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	backToPolicies(w, r, "Policy rule for "+path+" saved.")
}

// policyDelete removes one rule; keys under its path fall back to the
// next-most-specific rule or the built-in defaults.
func (g *gui) policyDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := g.adminForm(w, r)
	if !ok {
		return
	}
	if err := g.s.PolicyDelete(r.Context(), r.FormValue("kind"), r.FormValue("path")); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	backToPolicies(w, r, "Policy rule removed.")
}

// --- lock inspection (§9) --------------------------------------------------------

// lockRow is one (resource, holder) pair — a shared lock with N holders
// renders as N rows so every holder's expiry is visible.
type lockRow struct {
	Resource string
	Holder   string
	Mode     string // exclusive | shared
	Fencing  uint64
	Expires  time.Time // zero = held without TTL
	// Expired marks a holder whose TTL already elapsed but whose record
	// has not been pruned yet (pruning happens on the next lock proposal).
	Expired bool
}

// locksData feeds locks.tpl.
type locksData struct {
	Rows   []lockRow
	Notice string
}

// lockValue mirrors the state machine's stored lock shape (pkg/kv
// lockState): mode, holder→expiry-unix-ms map, and the fencing counter.
type lockValue struct {
	Mode    string           `json:"mode"`
	Holders map[string]int64 `json:"holders"`
	Fencing uint64           `json:"fencing"`
}

// locksPageSize bounds one lock listing (locks are transient coordination
// state — a cluster with thousands of simultaneous locks has bigger
// problems than pagination).
const locksPageSize = 1000

// locksPage lists every active lock from the metadata keyspace. Released
// locks delete their record, so everything under locks/ is live state.
func (g *gui) locksPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireAdmin(w, r)
	if !ok {
		return
	}
	entries, err := g.s.SystemList("locks/", locksPageSize)
	if err != nil {
		g.failPage(w, r, u, err)
		return
	}
	d := &locksData{Notice: r.URL.Query().Get("notice")}
	now := time.Now()
	for _, e := range entries {
		var ls lockValue
		if json.Unmarshal(e.Record.Value, &ls) != nil {
			continue // never let one corrupt record blank the whole page
		}
		resource := e.Key[len("locks/"):]
		// One row per holder, holders sorted for a stable page.
		holders := make([]string, 0, len(ls.Holders))
		for h := range ls.Holders {
			holders = append(holders, h)
		}
		sort.Strings(holders)
		for _, h := range holders {
			row := lockRow{Resource: resource, Holder: h, Mode: ls.Mode, Fencing: ls.Fencing}
			if exp := ls.Holders[h]; exp > 0 {
				row.Expires = time.UnixMilli(exp).UTC()
				row.Expired = row.Expires.Before(now)
			}
			d.Rows = append(d.Rows, row)
		}
	}
	g.render(w, http.StatusOK, "locks.tpl", g.page(r, u, "Locks", d))
}

// lockForceUnlock is the audited admin override (§9): it deletes the lock
// record outright, bumping past every holder. The server side writes the
// audit entry with the actor and the typed reason.
func (g *gui) lockForceUnlock(w http.ResponseWriter, r *http.Request) {
	u, ok := g.adminForm(w, r)
	if !ok {
		return
	}
	resource := r.FormValue("resource")
	if err := g.s.LockForceUnlock(r.Context(), resource, u.Name, r.FormValue("reason")); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	http.Redirect(w, r, "/locks?notice="+url.QueryEscape("Lock "+resource+" force-unlocked (audited)."), http.StatusSeeOther)
}

// --- audit trail (§7.3) -----------------------------------------------------------

// auditRow is one rendered audit entry.
type auditRow struct {
	Time   time.Time
	Actor  string
	Action string
	Detail string
}

// auditData feeds audit.tpl.
type auditData struct {
	Actor  string // current actor filter (prefix)
	Action string // current action filter (prefix)
	Rows   []auditRow
	// Truncated is set when the scan hit its budget before reaching the
	// oldest entries — the shown rows are still the newest matches.
	Truncated bool
}

// Audit scan bounds. Entries are stored under audit/<unixnano>-<rand>, so
// a forward scan visits oldest→newest; the page keeps a rolling tail of
// the newest matches and reverses it for display.
const (
	auditKeep       = 200 // newest matching entries shown
	auditFetchPages = 50  // × 1000-entry pages = scan budget per view
)

// auditPage lists recent audit entries newest-first with prefix filters
// on actor and action. Read-only by design — the trail is evidence.
func (g *gui) auditPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireAdmin(w, r)
	if !ok {
		return
	}
	d := &auditData{
		Actor:  r.URL.Query().Get("actor"),
		Action: r.URL.Query().Get("action"),
	}
	// Rolling tail: page forward through the (chronological) keyspace and
	// keep only the last auditKeep matches. Metadata reads are local, so
	// the scan is cheap; the fetch budget still bounds pathological
	// trails, and Truncated tells the admin when it bit.
	var tail []auditRow
	cursor := ""
	for fetch := 0; ; fetch++ {
		if fetch == auditFetchPages {
			d.Truncated = true
			break
		}
		entries, next, err := g.s.SystemListPage("audit/", cursor, 1000)
		if err != nil {
			g.failPage(w, r, u, err)
			return
		}
		for _, e := range entries {
			var a auth.AuditEntry
			if json.Unmarshal(e.Record.Value, &a) != nil {
				continue
			}
			if !hasPrefixFold(a.Actor, d.Actor) || !hasPrefixFold(a.Action, d.Action) {
				continue
			}
			tail = append(tail, auditRow{Time: a.Time, Actor: a.Actor, Action: a.Action, Detail: a.Detail})
			// Trim lazily at 2× so appends stay amortized O(1).
			if len(tail) > 2*auditKeep {
				tail = append(tail[:0], tail[len(tail)-auditKeep:]...)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(tail) > auditKeep {
		tail = tail[len(tail)-auditKeep:]
	}
	// Newest first.
	for i := len(tail) - 1; i >= 0; i-- {
		d.Rows = append(d.Rows, tail[i])
	}
	g.render(w, http.StatusOK, "audit.tpl", g.page(r, u, "Audit trail", d))
}

// hasPrefixFold is a case-insensitive prefix filter; an empty filter
// matches everything.
func hasPrefixFold(s, prefix string) bool {
	return prefix == "" ||
		(len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix))
}
