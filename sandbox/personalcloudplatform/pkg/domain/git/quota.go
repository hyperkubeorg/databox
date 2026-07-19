// quota.go — org quota accounting (§7): ChargeOrgQuota is the OCC
// read-modify-write twin of users.ChargeQuota. Personal repos charge
// the user through the existing path; the effective org limit comes
// from site.QuotaFor over the org's Tier/QuotaOverride.
package git

import "context"

// ChargeOrgQuota adjusts an org's storage usage by delta (positive on
// push, negative on delete/refund). limit > 0 enforces the effective
// quota on positive charges; pass 0 to skip the check (refunds, or an
// unlimited org). Usage floors at 0 — an over-refund never goes
// negative. Semantics mirror users.ChargeQuota exactly.
func (s *Store) ChargeOrgQuota(ctx context.Context, org string, delta, limit int64) error {
	return s.updateOrg(ctx, org, func(o *Org) error {
		next := o.UsedBytes + delta
		if delta > 0 && limit > 0 && next > limit {
			return ErrQuotaExceeded
		}
		if next < 0 {
			next = 0
		}
		o.UsedBytes = next
		return nil
	})
}
