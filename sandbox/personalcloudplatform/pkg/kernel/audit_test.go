package kernel

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// newAuditTestApp wires an App for the audit paths.
func newAuditTestApp(t *testing.T) *App {
	t.Helper()
	db := kvxtest.New(t)
	return &App{
		Users: &users.Store{DB: db},
		Site:  &site.Store{DB: db},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// plantAudit writes one entry at a chosen age.
func plantAudit(t *testing.T, a *App, at time.Time, actor, action string) {
	t.Helper()
	e := AuditEntry{At: at, Actor: actor, Action: action}
	err := a.Users.DB.RunTx(context.Background(), func(tx *client.Tx) error {
		raw := mustJSON(t, e)
		tx.Set(auditPrefix+kvx.InvIDAt(at), raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, e AuditEntry) []byte {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ListAudit pages newest-first and filters by actor/action.
func TestListAuditFilters(t *testing.T) {
	ctx := context.Background()
	a := newAuditTestApp(t)
	now := time.Now()
	plantAudit(t, a, now.Add(-3*time.Hour), "root", "user.ban")
	plantAudit(t, a, now.Add(-2*time.Hour), "root", "tier.set")
	plantAudit(t, a, now.Add(-1*time.Hour), "ada", "invite.create")

	rows, err := a.ListAudit(ctx, "", "", "", 10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("unfiltered = %d rows (err %v)", len(rows), err)
	}
	if rows[0].Action != "invite.create" || rows[2].Action != "user.ban" {
		t.Fatalf("order wrong: %+v", rows)
	}
	rows, _ = a.ListAudit(ctx, "root", "", "", 10)
	if len(rows) != 2 {
		t.Fatalf("actor filter = %d rows", len(rows))
	}
	rows, _ = a.ListAudit(ctx, "", "user.", "", 10)
	if len(rows) != 1 || rows[0].Action != "user.ban" {
		t.Fatalf("action filter = %+v", rows)
	}
	// Paging: after the first row's id, only older rows.
	first, _ := a.ListAudit(ctx, "", "", "", 1)
	rest, _ := a.ListAudit(ctx, "", "", first[0].ID, 10)
	if len(rest) != 2 || rest[0].Action != "tier.set" {
		t.Fatalf("paging after %s = %+v", first[0].ID, rest)
	}
}

// Retention: the tunable age window prunes by one DeleteRange; the
// count cap trims the tail.
func TestAuditRetention(t *testing.T) {
	ctx := context.Background()
	a := newAuditTestApp(t)
	now := time.Now()
	plantAudit(t, a, now.Add(-100*24*time.Hour), "old", "ancient.deed")
	plantAudit(t, a, now.Add(-2*time.Hour), "root", "user.ban")
	plantAudit(t, a, now.Add(-1*time.Hour), "root", "tier.set")

	// Default window (90 days): only the ancient entry dies.
	if err := a.PruneAudit(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	rows, _ := a.ListAudit(ctx, "", "", "", 10)
	if len(rows) != 2 {
		t.Fatalf("default prune kept %d rows", len(rows))
	}

	// Tightened settings: 1-day window + cap of 1 entry.
	if err := a.Site.Update(ctx, func(c *site.Config) error {
		c.AuditDays, c.AuditEntries = 1, 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.PruneAudit(ctx); err != nil {
		t.Fatalf("prune with cap: %v", err)
	}
	rows, _ = a.ListAudit(ctx, "", "", "", 10)
	if len(rows) != 1 || rows[0].Action != "tier.set" {
		t.Fatalf("cap prune = %+v, want only the newest row", rows)
	}
}
