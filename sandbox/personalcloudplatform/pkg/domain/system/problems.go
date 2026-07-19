// problems.go — the §11.2 problems model: failing health checks
// materialize as records at /pcp/system/problems/<id> carrying a
// severity, a plain-language summary, a recommended action, and a link
// to the admin page that fixes it. Problems auto-resolve when their
// check passes (kept 24h as a "recently resolved" tombstone, then
// pruned), so the admin home always answers "what's wrong RIGHT NOW,
// and what just healed".
package system

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// problemsPrefix is this file's key family (kvx key table).
const problemsPrefix = "/pcp/system/problems/"

// Problem severities, mildest first.
const (
	SevInfo     = "info"
	SevWarn     = "warn"
	SevCritical = "critical"
)

// TombstoneTTL is how long a resolved problem stays visible.
const TombstoneTTL = 24 * time.Hour

// Problem is one open (or recently resolved) health finding.
type Problem struct {
	ID       string `json:"id"`
	Severity string `json:"severity"` // info | warn | critical
	// Area buckets the problem for the admin home's traffic lights
	// (people | storage | mail | webaccess | system | site).
	Area string `json:"area,omitempty"`
	// Summary says what's wrong; Action says what to do about it — both
	// full sentences, never a bare number or an unexplained red dot.
	Summary string `json:"summary"`
	Action  string `json:"action,omitempty"`
	// Source links the admin page where the problem is visible/fixable.
	Source     string    `json:"source,omitempty"`
	Since      time.Time `json:"since"`
	UpdatedAt  time.Time `json:"updated_at"`
	ResolvedAt time.Time `json:"resolved_at,omitzero"`
}

// Resolved reports whether the problem is a tombstone.
func (p Problem) Resolved() bool { return !p.ResolvedAt.IsZero() }

// sevRank orders severities for sorting and escalation checks.
func sevRank(s string) int {
	switch s {
	case SevCritical:
		return 2
	case SevWarn:
		return 1
	}
	return 0
}

// SevRank is sevRank for callers that sort problems themselves.
func SevRank(s string) int { return sevRank(s) }

// validProblemID gates ids: health checks build them from key-safe
// parts, but the id also arrives in admin URLs.
func validProblemID(id string) bool {
	return len(id) >= 3 && len(id) <= 128 && kvx.ValidTokenChars(strings.ReplaceAll(id, ".", ""))
}

// ProblemID builds a key-safe id from parts (dots join them; each part
// is squashed to the token alphabet).
func ProblemID(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, p := range parts {
		var b strings.Builder
		for _, r := range p {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
				b.WriteRune(r)
			default:
				b.WriteByte('-')
			}
		}
		if b.Len() > 0 {
			clean = append(clean, b.String())
		}
	}
	return strings.Join(clean, ".")
}

// Raise opens (or refreshes) a problem. An already-open problem keeps
// its Since — "how long has this been wrong" must survive re-raising —
// while severity/summary/action track the newest evaluation. Returns
// true when this call OPENED the problem (it was absent or resolved),
// or escalated its severity — the health worker notifies on that edge.
func (s *Store) Raise(ctx context.Context, p Problem) (bool, error) {
	if !validProblemID(p.ID) {
		return false, nil
	}
	now := time.Now().UTC()
	key := problemsPrefix + p.ID
	var existing Problem
	found, err := kvx.GetJSON(ctx, s.DB, key, &existing)
	if err != nil {
		return false, err
	}
	opened := !found || existing.Resolved()
	escalated := found && !existing.Resolved() && sevRank(p.Severity) > sevRank(existing.Severity)
	if !opened {
		p.Since = existing.Since
	} else if p.Since.IsZero() {
		p.Since = now
	}
	p.ResolvedAt = time.Time{}
	p.UpdatedAt = now
	if err := kvx.SetJSON(ctx, s.DB, key, p); err != nil {
		return false, err
	}
	return opened || escalated, nil
}

// Resolve closes a problem, keeping it as a tombstone for TombstoneTTL.
// Resolving an absent or already-resolved problem is a no-op.
func (s *Store) Resolve(ctx context.Context, id string) error {
	if !validProblemID(id) {
		return nil
	}
	key := problemsPrefix + id
	var p Problem
	found, err := kvx.GetJSON(ctx, s.DB, key, &p)
	if err != nil || !found || p.Resolved() {
		return err
	}
	p.ResolvedAt = time.Now().UTC()
	p.UpdatedAt = p.ResolvedAt
	return kvx.SetJSON(ctx, s.DB, key, p)
}

// Problems returns every record — open problems first (critical → warn
// → info, oldest first within a severity), tombstones after. The set is
// small by nature: it's what's currently wrong, not a log.
func (s *Store) Problems(ctx context.Context) ([]Problem, error) {
	var out []Problem
	err := kvx.ScanPrefix(ctx, s.DB, problemsPrefix, func(_ string, value []byte) error {
		var p Problem
		if json.Unmarshal(value, &p) == nil {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Resolved() != b.Resolved() {
			return !a.Resolved()
		}
		if ra, rb := sevRank(a.Severity), sevRank(b.Severity); ra != rb {
			return ra > rb
		}
		return a.Since.Before(b.Since)
	})
	return out, nil
}

// OpenProblems filters Problems to the unresolved set.
func (s *Store) OpenProblems(ctx context.Context) ([]Problem, error) {
	all, err := s.Problems(ctx)
	if err != nil {
		return nil, err
	}
	open := all[:0]
	for _, p := range all {
		if !p.Resolved() {
			open = append(open, p)
		}
	}
	return open, nil
}

// OpenProblemCount counts unresolved warn+critical problems — the
// launcher Admin card's badge (info stays off the badge by design).
func (s *Store) OpenProblemCount(ctx context.Context) (int, error) {
	open, err := s.OpenProblems(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range open {
		if sevRank(p.Severity) >= sevRank(SevWarn) {
			n++
		}
	}
	return n, nil
}

// notifiedPrefix is the admin-notification dedup ledger: one record per
// problem id, so a flapping check can't page every admin every minute.
const notifiedPrefix = "/pcp/system/notified/"

// notifyDedupWindow is how long one problem stays silenced after a
// notification went out.
const notifyDedupWindow = 24 * time.Hour

// ShouldNotify claims the notification slot for a problem: true at most
// once per dedup window per problem id. Best-effort (a race between two
// health replicas can double-notify once — the databox lock around the
// health sweep makes even that unlikely).
func (s *Store) ShouldNotify(ctx context.Context, problemID string) bool {
	if !validProblemID(problemID) {
		return false
	}
	key := notifiedPrefix + problemID
	var rec struct {
		At time.Time `json:"at"`
	}
	found, err := kvx.GetJSON(ctx, s.DB, key, &rec)
	if err == nil && found && time.Since(rec.At) < notifyDedupWindow {
		return false
	}
	rec.At = time.Now().UTC()
	return kvx.SetJSON(ctx, s.DB, key, rec) == nil
}

// PruneResolved deletes tombstones older than TombstoneTTL.
func (s *Store) PruneResolved(ctx context.Context) error {
	all, err := s.Problems(ctx)
	if err != nil {
		return err
	}
	for _, p := range all {
		if p.Resolved() && time.Since(p.ResolvedAt) > TombstoneTTL && validProblemID(p.ID) {
			if err := s.DB.Delete(ctx, problemsPrefix+p.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
