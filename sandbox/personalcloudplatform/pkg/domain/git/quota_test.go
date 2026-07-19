// quota_test.go — ChargeOrgQuota mirrors users.ChargeQuota exactly (§7).
package git

import (
	"context"
	"errors"
	"testing"
)

func TestChargeOrgQuota(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	if _, err := s.CreateOrg(ctx, "acme", "ada", ""); err != nil {
		t.Fatalf("org: %v", err)
	}
	used := func() int64 {
		o, _, err := s.GetOrg(ctx, "acme")
		if err != nil {
			t.Fatalf("get org: %v", err)
		}
		return o.UsedBytes
	}

	// Within the limit charges accumulate.
	if err := s.ChargeOrgQuota(ctx, "acme", 600, 1000); err != nil {
		t.Fatalf("first charge: %v", err)
	}
	// Over the limit refuses and leaves usage untouched.
	if err := s.ChargeOrgQuota(ctx, "acme", 500, 1000); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over-limit charge must be ErrQuotaExceeded, got %v", err)
	}
	if got := used(); got != 600 {
		t.Fatalf("refused charge must not change usage, got %d", got)
	}
	// Exactly to the limit is allowed.
	if err := s.ChargeOrgQuota(ctx, "acme", 400, 1000); err != nil {
		t.Fatalf("to-the-limit charge: %v", err)
	}
	// limit 0 skips the check (refunds / unlimited orgs).
	if err := s.ChargeOrgQuota(ctx, "acme", 5000, 0); err != nil {
		t.Fatalf("limit-0 charge: %v", err)
	}
	// Refunds floor at 0 — an over-refund never goes negative.
	if err := s.ChargeOrgQuota(ctx, "acme", -999999, 0); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if got := used(); got != 0 {
		t.Fatalf("usage must floor at 0, got %d", got)
	}
	// A missing org is a plain ErrNotFound.
	if err := s.ChargeOrgQuota(ctx, "ghost", 1, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing org must be ErrNotFound, got %v", err)
	}
}
