// deleterange_test.go — GUARANTEE: delete-range authorizes the ENTIRE
// range [start, end), not just its start key. rangeCoveredByGrants is the
// deciding function; these tests pin its semantics against the §7.2 grant
// model so a scoped token can never sweep keys outside its grant.
package v1api

import (
	"testing"

	"github.com/hyperkubeorg/databox/pkg/auth"
)

func TestPrefixUpperBound(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/pcp/", "/pcp0"},
		{"abc", "abd"},
		{"a\xff", "b"},
		{"\xff\xff", ""}, // no finite bound
		{"", ""},
	}
	for _, c := range cases {
		if got := prefixUpperBound(c.in); got != c.want {
			t.Errorf("prefixUpperBound(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRangeCoveredByGrants(t *testing.T) {
	allow := func(prefix string, verbs ...auth.Verb) auth.Grant {
		return auth.Grant{Prefix: prefix, Effect: "allow", Verbs: verbs}
	}
	deny := func(prefix string, verbs ...auth.Verb) auth.Grant {
		return auth.Grant{Prefix: prefix, Effect: "deny", Verbs: verbs}
	}
	del := auth.VerbDelete

	scoped := []auth.Grant{allow("/pcp/", auth.VerbRead, auth.VerbWrite, del, auth.VerbList)}

	cases := []struct {
		name       string
		grants     []auth.Grant
		start, end string
		want       bool
	}{
		// The reported attack: a /pcp/-scoped token with an
		// unbounded end would wipe everything sorting after start.
		{"unbounded end rejected", scoped, "/pcp/", "", false},
		// End escaping the granted subtree covers foreign keys.
		{"end outside grant rejected", scoped, "/pcp/", "/zzz", false},
		{"end just past subtree rejected", scoped, "/pcp/", "/pcp0x", false},
		// The bounded whole-prefix form (what the PCP app sends).
		{"whole granted prefix ok", scoped, "/pcp/", "/pcp0", true},
		// A sub-range strictly inside the grant.
		{"sub-range ok", scoped, "/pcp/a", "/pcp/b", true},
		{"sub-prefix bound ok", scoped, "/pcp/sub/", "/pcp/sub0", true},
		// Start outside the grant has no covering allow.
		{"start outside grant rejected", scoped, "/other/", "/other0", false},
		// The verb must be granted, not just any verb on the prefix.
		{"verb not granted rejected",
			[]auth.Grant{allow("/pcp/", auth.VerbWrite)},
			"/pcp/a", "/pcp/b", false},
		// A deny inside the range refuses the whole range…
		{"deny inside range rejected",
			[]auth.Grant{allow("/c/", del), deny("/c/x/", del)},
			"/c/a", "/c0", false},
		// …but not a range that never touches the denied subtree.
		{"deny outside range ok",
			[]auth.Grant{allow("/c/", del), deny("/c/x/", del)},
			"/c/a", "/c/b", true},
		// A longer allow under the deny re-authorizes ranges wholly inside it.
		{"nested allow under deny ok",
			[]auth.Grant{allow("/c/", del), deny("/c/x/", del), allow("/c/x/y/", del)},
			"/c/x/y/1", "/c/x/y/9", true},
		// …but a range spilling past the nested allow hits the deny.
		{"range past nested allow rejected",
			[]auth.Grant{allow("/c/", del), deny("/c/x/", del), allow("/c/x/y/", del)},
			"/c/x/y/1", "/c/x/z", false},
		// Equal-length deny ties lose to deny (§7.2 rule 4).
		{"deny tie rejected",
			[]auth.Grant{allow("/c/", del), deny("/c/", del)},
			"/c/a", "/c/b", false},
		// A deny on an unrelated verb never blocks deletion ranges.
		{"deny other verb ignored",
			[]auth.Grant{allow("/c/", del), deny("/c/x/", auth.VerbRead)},
			"/c/a", "/c0", true},
		{"no grants rejected", nil, "/a", "/b", false},
	}
	for _, c := range cases {
		if got := rangeCoveredByGrants(c.grants, c.start, c.end, del); got != c.want {
			t.Errorf("%s: rangeCoveredByGrants(%q, %q) = %v, want %v", c.name, c.start, c.end, got, c.want)
		}
	}
}
