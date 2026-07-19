// ns.go — the ONE namespace users and organizations share (§3.1):
// /pcp/git/ns/<name>. Org creation OCC-claims a name here after
// checking the users store; platform signup checks this registry (and
// the reserved list) inside its own uniqueness transaction via the
// users.Store.ReserveName hook, so an org name can never be shadowed by
// a later signup — even while Git Services is disabled. Names are
// immutable in v1.
package git

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Namespace kinds (NS.Kind).
const (
	NSKindUser = "user"
	NSKindOrg  = "org"
)

// NS is one namespace registry entry.
type NS struct {
	Kind  string    `json:"kind"` // user | org
	Since time.Time `json:"since"`
}

// reservedNames are the names neither an org nor a signup may claim
// (§3.1): git's own literal route segments plus every top-level kernel
// and app route prefix, so a namespace page can never collide with a
// real route. pkg/apps/git's TestReservedNamesCoverAppRoutes
// cross-checks this list against kernel.AppList. Names shorter than
// kvx.ValidKeyName's minimum ("s") are listed anyway — the list is the
// authority, not the length rule.
var reservedNames = map[string]bool{
	// §3.1 core + git app literals. "-" anchors /git/-/… (the vendored
	// asset route, §16) — reserved so the literal segment can never be
	// shadowed by a namespace (it fails ValidKeyName anyway; the list is
	// the authority).
	"git": true, "api": true, "admin": true, "orgs": true, "settings": true, "new": true,
	"-": true,
	// Kernel-owned routes (router.go).
	"login": true, "logout": true, "signup": true, "static": true, "healthz": true,
	// App mounts and platform surfaces (cmd/pcp route registrations).
	"launcher": true, "drive": true, "mail": true, "calendar": true, "contacts": true,
	"video": true, "music": true, "messenger": true, "smarthome": true,
	"notifications": true, "invites": true, "impersonate": true, "s": true,
}

// IsReservedName reports whether a name may never become a namespace.
func IsReservedName(name string) bool { return reservedNames[strings.ToLower(name)] }

// ValidNSName gates any name that will become a /pcp/git/ key segment:
// the shared username rule (3–32 of a-z, 0-9, dashes) plus the reserved
// list.
func ValidNSName(name string) error {
	if err := kvx.ValidKeyName(name, "name"); err != nil {
		return err
	}
	if IsReservedName(name) {
		return fmt.Errorf("%q is a reserved name", name)
	}
	return nil
}

// nsKey locates one registry entry.
func nsKey(name string) string { return nsPrefix + strings.ToLower(name) }

// GetNS loads one namespace entry. A name that isn't key-shaped is a
// plain miss.
func (s *Store) GetNS(ctx context.Context, name string) (NS, bool, error) {
	name = strings.ToLower(name)
	if kvx.ValidKeyName(name, "name") != nil {
		return NS{}, false, nil
	}
	var ns NS
	found, err := kvx.GetJSON(ctx, s.DB, nsKey(name), &ns)
	return ns, found, err
}

// Available reports whether a name could still be claimed: not
// reserved, not a registered namespace, not an existing account. The
// pre-flight check for forms; the authoritative claim is the OCC
// transaction (claimOrgInTx / the signup hook).
func (s *Store) Available(ctx context.Context, name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := ValidNSName(name); err != nil {
		return err
	}
	if _, found, err := s.GetNS(ctx, name); err != nil {
		return err
	} else if found {
		return ErrNameTaken
	}
	if _, found, err := s.Users.Get(ctx, name); err != nil {
		return err
	} else if found {
		return ErrNameTaken
	}
	return nil
}

// CheckUsernameInTx is the signup-side namespace check (§3.1), wired as
// users.Store.ReserveName from cmd/pcp: one extra Get inside signup's
// existing uniqueness transaction — the absent read is OCC-validated at
// commit, so a signup racing an org claim for the same name resolves
// with exactly one winner. Works while Git Services is disabled.
func (s *Store) CheckUsernameInTx(ctx context.Context, tx *client.Tx, username string) error {
	if IsReservedName(username) {
		return fmt.Errorf("that username is reserved")
	}
	if _, found, err := tx.Get(ctx, nsKey(username)); err != nil {
		return err
	} else if found {
		return users.ErrUsernameTaken
	}
	return nil
}

// claimOrgInTx stages an org's namespace claim (§3.1): the name must
// not be an account (read through the transaction, so a racing signup
// conflicts) and not already registered.
func (s *Store) claimOrgInTx(ctx context.Context, tx *client.Tx, name string, now time.Time) error {
	if err := ValidNSName(name); err != nil {
		return err
	}
	if exists, err := s.Users.ExistsInTx(ctx, tx, name); err != nil {
		return err
	} else if exists {
		return ErrNameTaken
	}
	if _, found, err := tx.Get(ctx, nsKey(name)); err != nil {
		return err
	} else if found {
		return ErrNameTaken
	}
	txSetJSON(tx, nsKey(name), NS{Kind: NSKindOrg, Since: now})
	return nil
}
