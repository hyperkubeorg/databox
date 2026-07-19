// admin.go — the account mutations only the admin console performs
// (ban, capabilities, tier, quota override, account deletion), plus the
// impersonation session mints. Ported from PCD feature-for-feature; the
// HTTP layer gates on admin rights and audits.
package users

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// SetBanned flips the ban bit. Banned accounts are refused at login and
// per-request (kernel.Authed); the caller also revokes live sessions.
func (s *Store) SetBanned(ctx context.Context, username string, banned bool) error {
	return s.update(ctx, username, func(u *User) error { u.Banned = banned; return nil })
}

// SetCaps replaces a member's capability set. Unknown capabilities are
// refused so a typo can't mint a phantom grant.
func (s *Store) SetCaps(ctx context.Context, username string, caps []string) error {
	for _, c := range caps {
		if !slices.Contains(KnownCaps, c) {
			return fmt.Errorf("unknown capability %q", c)
		}
	}
	slices.Sort(caps)
	caps = slices.Compact(caps)
	return s.update(ctx, username, func(u *User) error { u.Caps = caps; return nil })
}

// SetTier assigns the member to a named quota tier ("" = site default).
// The caller (admin console) validates the tier exists in site.Config.
func (s *Store) SetTier(ctx context.Context, username, tier string) error {
	return s.update(ctx, username, func(u *User) error { u.Tier = tier; return nil })
}

// SetQuotaOverride sets the per-user quota override: bytes > 0, 0 to
// clear (fall back to tier/default), site.QuotaUnlimited for no limit.
func (s *Store) SetQuotaOverride(ctx context.Context, username string, bytes int64) error {
	if bytes < site.QuotaUnlimited {
		return fmt.Errorf("bad quota override %d", bytes)
	}
	return s.update(ctx, username, func(u *User) error { u.QuotaOverride = bytes; return nil })
}

// DeleteUser removes THIS package's rows for a dying account: the user
// record, every live session, and the connected-from IPs. The personal
// drive, sharing rows, media state, and mail addresses are other
// domains' keys — the admin console composes their purges around this
// call (each package deletes only what it owns).
//
// Deliberately left behind (documented, acceptable at this scale):
// content uploaded INTO shared drives, grants/shares they created on
// other drives, invites they minted and the redemption ledger, audit
// entries naming them, and their mailbox message stores once the
// addresses are gone (unreachable, reclaimed only by hand).
func (s *Store) DeleteUser(ctx context.Context, username string) error {
	username = strings.ToLower(username)
	if ValidUsername(username) != nil {
		return ErrNotFound
	}
	if _, found, err := s.Get(ctx, username); err != nil {
		return err
	} else if !found {
		return ErrNotFound
	}
	if _, err := s.DeleteUserSessions(ctx, username); err != nil {
		return err
	}
	if err := kvx.DeletePrefix(ctx, s.DB, userIPsPrefix+username+"/"); err != nil {
		return err
	}
	return s.DB.Delete(ctx, usersPrefix+username)
}
