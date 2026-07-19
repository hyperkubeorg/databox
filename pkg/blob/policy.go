// policy.go is the durability policy engine (§12): the
// rules that decide, per blob key, how many replica copies a small blob
// gets and what erasure-coding geometry a large blob gets.
//
// Rules live in the metadata keyspace (the server stores them under
// `policies/replication/<path>` and `policies/ec/<path>`) as small JSON
// values. Resolution is bottom-up: the most specific (longest) path rule
// governing a key wins; keys no rule governs fall back to the built-in
// defaults — rs-4-2 EC, 2 replicas for small blobs.
//
// This file is deliberately storage-agnostic: it defines the rule shapes,
// their validation, and the pure resolution function. pkg/server adapts
// the metadata keyspace to PolicySource; tests exercise resolution with
// plain maps.
package blob

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Policy is the resolved durability decision for one blob key (§12).
type Policy struct {
	Replicas     int  // replica-mode copy count
	DataShards   int  // EC stripe data shards
	ParityShards int  // EC stripe parity shards
	ECEnabled    bool // false forces replica mode regardless of blob size
}

// DefaultPolicy returns the built-in §12 defaults: rs-4-2 erasure coding
// for large blobs, `replicas` copies (2 when unset) for small ones.
func DefaultPolicy(replicas int) Policy {
	if replicas < 1 {
		replicas = 2
	}
	return Policy{Replicas: replicas, DataShards: 4, ParityShards: 2, ECEnabled: true}
}

// PolicySource resolves stored policy rules for a key, starting from the
// built-in defaults in def. Implemented by pkg/server over the metadata
// keyspace.
type PolicySource interface {
	PolicyFor(key string, def Policy) Policy
}

// PolicyFor resolves the effective policy for a key: stored rules (when a
// source is wired) override the engine's built-in defaults.
func (e *Engine) PolicyFor(key string) Policy {
	def := DefaultPolicy(e.Replicas)
	if e.Policies == nil {
		return def
	}
	return e.Policies.PolicyFor(key, def)
}

// ReplicationRule is the JSON value stored under
// policies/replication/<path>, e.g. {"replicas":3}.
type ReplicationRule struct {
	Replicas int `json:"replicas"`
}

// ECRule is the JSON value stored under policies/ec/<path>, e.g.
// {"data":4,"parity":2,"enabled":true}. enabled=false forces replica mode
// for the subtree (geometry fields are then ignored).
type ECRule struct {
	Data    int  `json:"data"`
	Parity  int  `json:"parity"`
	Enabled bool `json:"enabled"`
}

// ParseReplicationRule decodes and validates a stored replication rule.
func ParseReplicationRule(raw []byte) (ReplicationRule, error) {
	var r ReplicationRule
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("parse replication policy: %w", err)
	}
	if r.Replicas < 1 || r.Replicas > 16 {
		return r, fmt.Errorf("replication policy: replicas must be 1..16 (got %d)", r.Replicas)
	}
	return r, nil
}

// ParseECRule decodes and validates a stored erasure-coding rule.
func ParseECRule(raw []byte) (ECRule, error) {
	var r ECRule
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("parse ec policy: %w", err)
	}
	// Geometry only matters when EC is enabled; a bare {"enabled":false}
	// is a valid "replicate this subtree" rule.
	if r.Enabled {
		if r.Data < 1 || r.Parity < 1 || r.Data+r.Parity > 32 {
			return r, fmt.Errorf("ec policy: need data ≥ 1, parity ≥ 1, data+parity ≤ 32 (got %d+%d)", r.Data, r.Parity)
		}
	}
	return r, nil
}

// ResolvePolicy applies stored rules to the defaults: the most specific
// matching replication rule sets the replica count; the most specific
// matching EC rule sets stripe geometry and enablement. Rules that fail
// validation are ignored (a bad rule must never break writes).
func ResolvePolicy(key string, repl, ec map[string][]byte, def Policy) Policy {
	pol := def
	if path, ok := BestMatch(key, ruleKeys(repl)); ok {
		if r, err := ParseReplicationRule(repl[path]); err == nil {
			pol.Replicas = r.Replicas
		}
	}
	if path, ok := BestMatch(key, ruleKeys(ec)); ok {
		if r, err := ParseECRule(ec[path]); err == nil {
			pol.ECEnabled = r.Enabled
			if r.Enabled {
				pol.DataShards, pol.ParityShards = r.Data, r.Parity
			}
		}
	}
	return pol
}

// ruleKeys extracts the rule paths from a rule map.
func ruleKeys(rules map[string][]byte) []string {
	out := make([]string, 0, len(rules))
	for p := range rules {
		out = append(out, p)
	}
	return out
}

// BestMatch returns the most specific (longest) stored policy path that
// governs key. Paths match at segment boundaries: "/logs" governs "/logs"
// and "/logs/app.log" but not "/logstash".
func BestMatch(key string, paths []string) (string, bool) {
	best, found := "", false
	for _, p := range paths {
		if !pathGoverns(p, key) {
			continue
		}
		if !found || len(p) > len(best) {
			best, found = p, true
		}
	}
	return best, found
}

// pathGoverns reports whether a policy path applies to a key.
func pathGoverns(path, key string) bool {
	if path == "" || !strings.HasPrefix(key, path) {
		return false
	}
	// Exact match, a directory-style path ("/logs/"), or the next byte of
	// the key starting a new segment — anything else is a partial-name
	// collision like "/logs" vs "/logstash".
	return len(key) == len(path) || strings.HasSuffix(path, "/") || key[len(path)] == '/'
}
