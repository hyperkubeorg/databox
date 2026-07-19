// grants.go implements the prefix-based allow/deny authorization model
// (§7.2).
//
// A grant is a rule attached to a user:
//
//	{prefix, effect: allow|deny, verbs: [list, read, write, ...]}
//
// Resolution is "most specific prefix wins":
//
//  1. Collect the user's grants whose prefix is a prefix of the target key.
//  2. Sort them longest-prefix-first.
//  3. The first grant that mentions the requested verb decides the outcome.
//  4. If two grants tie on length and disagree, deny wins.
//  5. If nothing mentions the verb: default deny.
//
// The same tree governs every access path — HTTP API, GUI, SQL tables
// (/sql/<db>/<table>/), and S3 buckets (/s3/<bucket>/) — one authorization
// model for everything.
package auth

import (
	"sort"
	"strings"
)

// Verb is a permission kind that a grant can allow or deny.
type Verb string

// The complete verb set. "admin" gates cluster-management APIs (topology,
// policies, users, force-unlock, backups) rather than a key range.
const (
	VerbList   Verb = "list"
	VerbRead   Verb = "read"
	VerbWrite  Verb = "write"
	VerbDelete Verb = "delete"
	VerbWatch  Verb = "watch"
	VerbLock   Verb = "lock"
	VerbAdmin  Verb = "admin"
)

// AllVerbs enumerates every valid verb, used for input validation.
var AllVerbs = []Verb{VerbList, VerbRead, VerbWrite, VerbDelete, VerbWatch, VerbLock, VerbAdmin}

// ValidVerb reports whether s names a known verb.
func ValidVerb(s string) bool {
	for _, v := range AllVerbs {
		if string(v) == s {
			return true
		}
	}
	return false
}

// Grant is one authorization rule. Effect is "allow" or "deny".
type Grant struct {
	Prefix string `json:"prefix"`
	Effect string `json:"effect"`
	Verbs  []Verb `json:"verbs"`
}

// mentions reports whether the grant covers the given verb.
func (g Grant) mentions(v Verb) bool {
	for _, gv := range g.Verbs {
		if gv == v {
			return true
		}
	}
	return false
}

// Allowed evaluates the grant set for one (key, verb) request.
//
// The walk is exactly the specification's algorithm: filter to matching
// prefixes, order longest first, and let the first grant that mentions the
// verb decide — with the deny-beats-allow tiebreak for equal lengths.
func Allowed(grants []Grant, key string, verb Verb) bool {
	// Gather grants whose prefix actually covers the key.
	matching := make([]Grant, 0, len(grants))
	for _, g := range grants {
		if strings.HasPrefix(key, g.Prefix) {
			matching = append(matching, g)
		}
	}
	// Longest prefix first; among equal lengths put deny before allow so
	// the tiebreak falls out of the ordering naturally.
	sort.SliceStable(matching, func(i, j int) bool {
		li, lj := len(matching[i].Prefix), len(matching[j].Prefix)
		if li != lj {
			return li > lj
		}
		return matching[i].Effect == "deny" && matching[j].Effect == "allow"
	})
	// First grant that speaks about this verb wins.
	for _, g := range matching {
		if g.mentions(verb) {
			return g.Effect == "allow"
		}
	}
	// Nothing mentioned the verb: default deny (§7.2 rule 4).
	return false
}
