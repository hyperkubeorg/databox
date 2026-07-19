package build

import (
	"context"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	return &Store{DB: kvxtest.New(t)}
}

// completionFor fakes the `pcp-runner setup` half against a pending
// runner record, reporting the given executor kind.
func completionFor(t *testing.T, r Runner, kind string) string {
	t.Helper()
	_, runnerPub, err := wire.NewSignPair()
	if err != nil {
		t.Fatal(err)
	}
	_, runnerSealPub, err := wire.NewSealPair()
	if err != nil {
		t.Fatal(err)
	}
	return buildproto.EncodeCompletionBlob(buildproto.CompletionBlob{
		RunnerPub: runnerPub, RunnerSealPub: runnerSealPub,
		TLSFP:        strings.Repeat("ab", 32),
		Kind:         kind,
		PairingToken: r.PairingToken,
	})
}

func TestNewPendingRunnerMintsKeysAndBlob(t *testing.T) {
	r, blob, err := NewPendingRunner("cluster-1", "system", "ada")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != RunnerPending || r.PairingToken == "" {
		t.Fatalf("not pending: %+v", r)
	}
	if !wire.ValidKeyB64(r.PCPControlPub) || !wire.ValidKeyB64(r.PCPSealPub) {
		t.Error("PCP public keys didn't mint")
	}
	if r.PCPControlPriv == "" || r.PCPSealPriv == "" {
		t.Error("PCP private keys missing (the trust root)")
	}
	if r.MaxConcurrent != DefaultMaxConcurrent || r.Scope != ScopeSystem {
		t.Errorf("defaults wrong: %+v", r)
	}
	// The setup blob is a decodable PCPBR1 code carrying the public halves.
	if !strings.HasPrefix(blob, "PCPBR1.") {
		t.Fatalf("bad blob prefix: %q", blob)
	}
	sb, err := buildproto.DecodeSetupBlob(blob)
	if err != nil {
		t.Fatalf("decode setup blob: %v", err)
	}
	if sb.PCPControl != r.PCPControlPub || sb.PCPSeal != r.PCPSealPub || sb.PairingToken != r.PairingToken {
		t.Errorf("blob doesn't carry the pending identity: %+v", sb)
	}
}

func TestNewPendingRunnerScopeValidation(t *testing.T) {
	for _, ok := range []string{"system", "org:acme", "repo:abcd1234abcd"} {
		if _, _, err := NewPendingRunner("r", ok, "ada"); err != nil {
			t.Errorf("scope %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"weird", "org:", "org:a/b", "repo:"} {
		if _, _, err := NewPendingRunner("r", bad, "ada"); err == nil {
			t.Errorf("scope %q accepted", bad)
		}
	}
}

func TestRunnerPairingLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	r, blob, err := s.CreateRunner(ctx, "cluster-1", "system", "ada")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(blob, "PCPBR1.") || r.Status != RunnerPending {
		t.Fatalf("create: blob=%q status=%q", blob, r.Status)
	}
	// The reverse index lists it under its scope.
	byScope, err := s.ListRunnersByScope(ctx, "system")
	if err != nil || len(byScope) != 1 || byScope[0].ID != r.ID {
		t.Fatalf("scope index: %v %+v", err, byScope)
	}

	// A wrong token is refused; a bad kind is refused; the right one activates.
	bad := completionFor(t, Runner{PairingToken: "wrong"}, buildproto.KindK8s)
	if _, err := s.CompletePairing(ctx, r.ID, bad); err == nil {
		t.Error("wrong pairing token accepted")
	}
	badKind := completionFor(t, r, "quantum")
	if _, err := s.CompletePairing(ctx, r.ID, badKind); err == nil {
		t.Error("unknown executor kind accepted")
	}

	paired, err := s.CompletePairing(ctx, r.ID, completionFor(t, r, buildproto.KindBareMetal))
	if err != nil {
		t.Fatal(err)
	}
	if paired.Status != RunnerActive || paired.PairingToken != "" ||
		paired.Kind != buildproto.KindBareMetal || !wire.ValidKeyB64(paired.RunnerSealPub) {
		t.Fatalf("pairing didn't settle: %+v", paired)
	}
	// The token is burned — a replayed completion is refused.
	if _, err := s.CompletePairing(ctx, r.ID, completionFor(t, r, buildproto.KindK8s)); err == nil {
		t.Error("burned token replayed successfully")
	}

	// Throttle bounds, disable/enable, and re-pair reset the identity.
	if err := s.SetMaxConcurrent(ctx, r.ID, 9999); err == nil {
		t.Error("out-of-bounds concurrency accepted")
	}
	if err := s.SetMaxConcurrent(ctx, r.ID, 8); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunnerStatus(ctx, r.ID, true); err != nil {
		t.Fatal(err)
	}
	repaired, err := s.RepairRunner(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Status != RunnerPending || repaired.PairingToken == "" ||
		repaired.RunnerPub != "" || repaired.Kind != "" {
		t.Fatalf("re-pair didn't reset identity: %+v", repaired)
	}

	list, err := s.ListRunners(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v n=%d", err, len(list))
	}

	// Delete clears the record and its reverse index.
	if err := s.DeleteRunner(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if byScope, _ := s.ListRunnersByScope(ctx, "system"); len(byScope) != 0 {
		t.Errorf("scope index survived delete: %+v", byScope)
	}
}
