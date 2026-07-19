// provision.go — the ONE mailbox-provisioning path both consoles share.
// Admin assignment (/admin/mail/addresses) and member self-service (the
// /mail chooser and mail settings) both land here: the CreateMailbox
// transaction against the resolved allowance, then the welcome set
// through the real delivery pipeline. The allowance re-checks inside
// the transaction, so racing claims for the last slot resolve with
// exactly one winner.
package mail

import (
	"context"
	"fmt"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// ProvisionMailbox claims local@domain for u against their resolved
// allowance (MailboxesFor), then delivers the welcome messages.
func (s *Store) ProvisionMailbox(ctx context.Context, sc site.Config, u users.User, domain, local string) (Mailbox, error) {
	box, err := s.CreateMailbox(ctx, u.Username, domain, local, MailboxesFor(sc, u))
	if err != nil {
		return Mailbox{}, err
	}
	s.DeliverWelcomes(ctx, sc, u, box)
	return box, nil
}

// CreateOwnMailbox is the member-facing claim: the mail feature must be
// on, and the allowance (per-user override, else the site default) not
// spent — with plain-language messages for "none granted" and "all
// used". Everything else (local-part rules, domain enabled, address
// uniqueness) is the same CreateMailbox transaction the admin uses.
func (s *Store) CreateOwnMailbox(ctx context.Context, sc site.Config, u users.User, local, domain string) (Mailbox, error) {
	if !sc.Mail.Enabled {
		return Mailbox{}, fmt.Errorf("email is turned off on this platform")
	}
	allowed := MailboxesFor(sc, u)
	switch {
	case allowed <= 0:
		return Mailbox{}, fmt.Errorf("your administrator hasn't granted you any email addresses yet")
	case u.MailboxCount >= allowed:
		return Mailbox{}, fmt.Errorf("you've used all %d of your email addresses — ask your admin for more", allowed)
	}
	return s.ProvisionMailbox(ctx, sc, u, domain, local)
}

// AddressAvailability answers the create form's live check: is
// local@domain claimable right now, and if not, why (a user-facing
// reason). It mirrors CreateMailbox's gates without claiming anything —
// the transaction stays the source of truth.
func (s *Store) AddressAvailability(ctx context.Context, domain, local string) (bool, string, error) {
	local = strings.ToLower(strings.TrimSpace(local))
	domain = strings.ToLower(strings.TrimSpace(domain))
	if err := ValidLocalPart(local); err != nil {
		return false, err.Error(), nil
	}
	if reservedLocals[local] {
		return false, local + " is reserved", nil
	}
	d, found, err := s.GetDomain(ctx, domain)
	if err != nil {
		return false, "", err
	}
	if !found || !d.Enabled {
		return false, "mail domain " + domain + " isn't available", nil
	}
	if _, exists, err := s.GetAddress(ctx, domain, local); err != nil {
		return false, "", err
	} else if exists {
		return false, local + "@" + domain + " is taken", nil
	}
	return true, "", nil
}
