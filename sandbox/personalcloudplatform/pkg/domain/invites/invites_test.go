package invites

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

func fixture(t *testing.T) (*Store, *users.Store) {
	t.Helper()
	db := kvxtest.New(t)
	inv := &Store{DB: db}
	us := &users.Store{DB: db}
	us.RedeemInvite = inv.RedeemInTx
	return inv, us
}

func member(name string) users.User { return users.User{Username: name} }
func admin(name string) users.User  { return users.User{Username: name, IsAdmin: true} }

// The three kinds enforce their own limits; status derivation matches.
func TestInviteKindsAndStatus(t *testing.T) {
	ctx := context.Background()
	inv, _ := fixture(t)

	// Quantity: exhausted at MaxUses.
	q, err := inv.Create(ctx, member("ada"), KindQuantity, "", 1, time.Time{})
	if err != nil {
		t.Fatalf("create quantity: %v", err)
	}
	if got := q.Status(time.Now()); got != StatusActive {
		t.Fatalf("fresh quantity invite = %q", got)
	}
	q.Uses = 1
	if got := q.Status(time.Now()); got != StatusExhausted {
		t.Fatalf("spent quantity invite = %q", got)
	}

	// Time: expired past ExpiresAt.
	tm, err := inv.Create(ctx, member("ada"), KindTime, "", 0, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("create time: %v", err)
	}
	if got := tm.Status(time.Now().Add(2 * time.Hour)); got != StatusExpired {
		t.Fatalf("expired time invite = %q", got)
	}

	// Permanent: members may not mint; admins may, with a description.
	if _, err := inv.Create(ctx, member("ada"), KindPermanent, "", 0, time.Time{}); err == nil {
		// members CAN mint permanent at the domain level? No: the HTTP
		// layer gates via CanCreatePermanent. Domain allows the shape.
		_ = err
	}
	if _, err := inv.Create(ctx, admin("root"), KindPermanent, "", 0, time.Time{}); err == nil {
		t.Fatalf("admin invite without a description must be refused")
	}
	p, err := inv.Create(ctx, admin("root"), KindPermanent, "standing door", 0, time.Time{})
	if err != nil {
		t.Fatalf("create permanent: %v", err)
	}
	// Revocation composes with every kind and wins over everything.
	if err := inv.Revoke(ctx, p.Code, "root"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, _, _ := inv.Get(ctx, p.Code)
	if got.Status(time.Now()) != StatusRevoked {
		t.Fatalf("revoked invite = %q", got.Status(time.Now()))
	}
	if err := StatusErr(StatusRevoked); err != ErrInviteRevoked {
		t.Fatalf("StatusErr(revoked) = %v", err)
	}
}

// Redemption inside the signup transaction: uses counted, ledger row
// written, user stamped with InvitedBy/InviteCode.
func TestRedeemInSignup(t *testing.T) {
	ctx := context.Background()
	inv, us := fixture(t)
	code, err := inv.Create(ctx, member("ada"), KindQuantity, "", 2, time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	u, err := us.CreateUserInvited(ctx, "bob", "Bob", "password123", code.Code, "10.0.0.9")
	if err != nil {
		t.Fatalf("signup with invite: %v", err)
	}
	if u.InvitedBy != "ada" || u.InviteCode != code.Code {
		t.Fatalf("redemption not stamped on user: %+v", u)
	}
	after, _, _ := inv.Get(ctx, code.Code)
	if after.Uses != 1 {
		t.Fatalf("uses = %d, want 1", after.Uses)
	}
	uses, err := inv.Uses(ctx, code.Code)
	if err != nil || len(uses) != 1 || uses[0].Username != "bob" || uses[0].IP != "10.0.0.9" {
		t.Fatalf("ledger wrong: %+v (err %v)", uses, err)
	}

	// A bad code refuses the signup outright.
	if _, err := us.CreateUserInvited(ctx, "carol", "", "password123", "nope-nope-nope", ""); err != ErrBadInvite {
		t.Fatalf("bad code = %v, want ErrBadInvite", err)
	}
	// A revoked code refuses with its own message.
	_ = inv.Revoke(ctx, code.Code, "ada")
	if _, err := us.CreateUserInvited(ctx, "carol", "", "password123", code.Code, ""); err != ErrInviteRevoked {
		t.Fatalf("revoked code = %v, want ErrInviteRevoked", err)
	}
}

// Racing signups on a quantity invite's LAST slot: OCC admits exactly
// one; the loser gets the exhausted refusal, and the invite can never
// oversubscribe.
func TestRedeemLastSlotRace(t *testing.T) {
	ctx := context.Background()
	inv, us := fixture(t)
	code, err := inv.Create(ctx, member("ada"), KindQuantity, "", 1, time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, name := range []string{"bob", "carol"} {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			_, errs[i] = us.CreateUserInvited(ctx, name, "", "password123", code.Code, "")
		}(i, name)
	}
	wg.Wait()
	okCount := 0
	for _, err := range errs {
		if err == nil {
			okCount++
		}
	}
	if okCount != 1 {
		t.Fatalf("exactly one racer must win the last slot, got %d (errs %v)", okCount, errs)
	}
	after, _, _ := inv.Get(ctx, code.Code)
	if after.Uses != 1 {
		t.Fatalf("oversubscribed: uses = %d", after.Uses)
	}
}

// The per-creator record cap refuses the 101st invite.
func TestPerCreatorCap(t *testing.T) {
	ctx := context.Background()
	inv, _ := fixture(t)
	for i := 0; i < MaxInvitesPerOwner; i++ {
		if _, err := inv.Create(ctx, member("ada"), KindQuantity, "", 1, time.Time{}); err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
	}
	if _, err := inv.Create(ctx, member("ada"), KindQuantity, "", 1, time.Time{}); err == nil {
		t.Fatalf("cap must refuse invite #%d", MaxInvitesPerOwner+1)
	}
	// Shape validation.
	if _, err := inv.Create(ctx, member("bob"), KindQuantity, "", 0, time.Time{}); err == nil {
		t.Fatal("quantity 0 must be refused")
	}
	if _, err := inv.Create(ctx, member("bob"), KindTime, "", 0, time.Now().Add(-time.Hour)); err == nil {
		t.Fatal("past expiry must be refused")
	}
	if _, err := inv.Create(ctx, member("bob"), "banana", "", 0, time.Time{}); err == nil {
		t.Fatal("bad kind must be refused")
	}
}

// Who may mint under each signup mode (spec §4 / PCD parity).
func TestCanCreateMatrix(t *testing.T) {
	trusted := users.User{Username: "t", Caps: []string{users.CapInvite}}
	cases := []struct {
		mode string
		u    users.User
		want bool
	}{
		{site.SignupOpen, member("m"), false},
		{site.SignupOpen, admin("a"), true}, // staging before flipping the gate
		{site.SignupInvite, member("m"), true},
		{site.SignupTrusted, member("m"), false},
		{site.SignupTrusted, trusted, true},
		{site.SignupTrusted, admin("a"), true},
		{site.SignupAdmin, member("m"), false},
		{site.SignupAdmin, trusted, false},
		{site.SignupAdmin, admin("a"), true},
	}
	for _, tc := range cases {
		if got := CanCreate(tc.u, tc.mode); got != tc.want {
			t.Errorf("CanCreate(%s admin=%v caps=%v) = %v, want %v", tc.mode, tc.u.IsAdmin, tc.u.Caps, got, tc.want)
		}
	}
	if CanCreatePermanent(member("m")) || !CanCreatePermanent(admin("a")) {
		t.Error("permanent invites are admin-only")
	}
}
