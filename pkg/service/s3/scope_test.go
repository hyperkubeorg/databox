// scope_test.go verifies API-key scope enforcement in the gateway: a
// scoped key is capped to its prefixes for every owner — including root,
// whose grants otherwise bypass everything.
package s3

import (
	"testing"

	"github.com/hyperkubeorg/databox/pkg/auth"
)

func TestScopedKeyCapsRoot(t *testing.T) {
	c := &caller{user: "root", isRoot: true,
		key: auth.AccessKey{Scopes: []string{"/s3/public/"}}}
	if !c.authorize("/s3/public/img.png", auth.VerbRead) {
		t.Fatal("in-scope request denied")
	}
	if c.authorize("/s3/private/secret", auth.VerbRead) {
		t.Fatal("scoped root key escaped its scope")
	}
}

func TestScopedKeyIntersectsGrants(t *testing.T) {
	grants := []auth.Grant{{Prefix: "/s3/", Effect: "allow",
		Verbs: []auth.Verb{auth.VerbRead, auth.VerbWrite, auth.VerbList}}}
	c := &caller{user: "app", grants: grants,
		key: auth.AccessKey{Scopes: []string{"/s3/uploads/"}}}
	// Inside scope AND grants: allowed.
	if !c.authorize("/s3/uploads/a.bin", auth.VerbWrite) {
		t.Fatal("in-scope granted write denied")
	}
	// Inside grants but outside scope: the key caps it.
	if c.authorize("/s3/other/a.bin", auth.VerbWrite) {
		t.Fatal("scope did not cap a granted key")
	}
	// Unscoped key falls back to grants alone.
	open := &caller{user: "app", grants: grants, key: auth.AccessKey{}}
	if !open.authorize("/s3/other/a.bin", auth.VerbWrite) {
		t.Fatal("unscoped key wrongly restricted")
	}
}
