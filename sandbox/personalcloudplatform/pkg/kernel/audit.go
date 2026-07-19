// audit.go — the persisted audit log. The PCP upgrade over PCD: a
// privileged-action mutation and its audit entry commit in ONE databox
// transaction via AppendAudit(tx, entry) — no more soft-fail
// fire-and-forget. Retention pruning is a worker (RunAuditRetention),
// not a per-append piggyback.
//
// Entries land at /pcp/audit/<invTs>-<rand>: the inverted-timestamp id
// makes a plain prefix List read newest-first. Entries are never
// editable through the app.
package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// auditPrefix locates the log (kvx key table).
const auditPrefix = "/pcp/audit/"

// Audit retention defaults (admin-tunable via site.Config.AuditDays /
// AuditEntries; zero falls back to these).
const (
	auditRetentionDays  = 90
	auditRetentionSweep = time.Hour
)

// AuditEntry is one recorded privileged action.
type AuditEntry struct {
	At    time.Time `json:"at"`
	Actor string    `json:"actor"`
	// ActorIsAdmin snapshots the bit at action time (a later demotion
	// doesn't rewrite history).
	ActorIsAdmin bool   `json:"actor_is_admin,omitempty"`
	Action       string `json:"action"`           // e.g. "user.ban", "impersonate.start"
	Target       string `json:"target,omitempty"` // who/what it acted on
	Detail       string `json:"detail,omitempty"` // human-readable specifics
	IP           string `json:"ip,omitempty"`
	// Impersonating names the member the actor was viewing as when the
	// action ran — an impersonating admin can never act invisibly.
	Impersonating string `json:"impersonating,omitempty"`
}

// AppendAudit writes one entry INTO the caller's transaction, so the
// privileged mutation and its audit record commit or fail together.
func AppendAudit(tx *client.Tx, e AuditEntry) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	e.Actor = strings.ToLower(e.Actor)
	e.Target = strings.ToLower(e.Target)
	raw, _ := json.Marshal(e)
	tx.Set(auditPrefix+kvx.InvID(), raw)
}

// Audit records a privileged action that has no surrounding transaction:
// structured log plus a one-key commit. Attribution follows PCD's rule —
// an impersonating admin's actions are attributed to the ADMIN, with the
// impersonated member recorded alongside.
func (a *App) Audit(r *http.Request, actor users.User, sess users.Session, action, target, detail string) {
	entry := AuditEntry{
		Actor: actor.Username, ActorIsAdmin: actor.IsAdmin,
		Action: action, Target: target, Detail: detail,
		IP: a.ClientIP(r),
	}
	if sess.Impersonator != "" {
		entry.Actor, entry.ActorIsAdmin = sess.Impersonator, true
		entry.Impersonating = actor.Username
	}
	a.Log.Info("audit", "actor", entry.Actor, "action", action, "target", target, "detail", detail, "impersonating", entry.Impersonating)
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := a.Users.DB.RunTx(cctx, func(tx *client.Tx) error {
		AppendAudit(tx, entry)
		return nil
	})
	if err != nil {
		a.Log.Warn("audit persist failed", "action", action, "err", err)
	}
}

// RunAuditRetention is the pruning worker: on a fixed cadence it reads
// the admin-tunable retention settings (site.Config) and prunes — age
// first with one DeleteRange (inverted ids make "older than the cutoff"
// exactly "every key after the cutoff's prefix"), then the count cap by
// walking to the cap and range-deleting the tail. Records its pass at
// /pcp/system/loops/auditretention. Blocks until ctx ends.
func (a *App) RunAuditRetention(ctx context.Context) {
	t := time.NewTicker(auditRetentionSweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err := a.PruneAudit(cctx)
		if err != nil {
			a.Log.Warn("audit retention sweep failed", "err", err)
		}
		if a.System != nil {
			a.System.RecordLoop(cctx, "auditretention", err)
		}
		cancel()
	}
}

// PruneAudit runs one retention pass with the CURRENT settings (the
// worker's body, callable directly by tests and the console's save
// handler so a shrunk window applies immediately).
func (a *App) PruneAudit(ctx context.Context) error {
	sc, err := a.Site.Get(ctx)
	if err != nil {
		return err
	}
	days := sc.AuditDays
	if days <= 0 {
		days = auditRetentionDays
	}
	cutoff := auditPrefix + kvx.InvCursor(time.Now().Add(-time.Duration(days)*24*time.Hour))
	if err := a.Users.DB.DeleteRange(ctx, cutoff, kvx.PrefixEnd(auditPrefix)); err != nil {
		return err
	}
	if sc.AuditEntries > 0 {
		seen, cursor := 0, ""
		for {
			entries, next, err := a.Users.DB.List(ctx, auditPrefix, cursor, 500)
			if err != nil {
				return err
			}
			for _, e := range entries {
				seen++
				if seen > sc.AuditEntries {
					return a.Users.DB.DeleteRange(ctx, e.Key, kvx.PrefixEnd(auditPrefix))
				}
			}
			if next == "" {
				return nil
			}
			cursor = next
		}
	}
	return nil
}

// AuditRow is an entry plus its id (paging cursors echo it back).
type AuditRow struct {
	ID string
	AuditEntry
}

// ListAudit pages the log newest-first. actor/action filter when
// non-empty (substring match on action so "user." finds the family).
// afterID pages: pass the last row's ID to continue.
func (a *App) ListAudit(ctx context.Context, actor, action, afterID string, limit int) ([]AuditRow, error) {
	if limit <= 0 {
		limit = 50
	}
	actor = strings.ToLower(strings.TrimSpace(actor))
	action = strings.ToLower(strings.TrimSpace(action))
	var out []AuditRow
	cursor := ""
	if afterID != "" && kvx.ValidTokenChars(strings.ReplaceAll(afterID, ".", "")) {
		// Resume strictly after the echoed id (List resumes AT the
		// cursor, so append a byte that sorts after nothing real).
		cursor = auditPrefix + afterID + "\x00"
	}
	for len(out) < limit {
		entries, next, err := a.Users.DB.List(ctx, auditPrefix, cursor, 200)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			var entry AuditEntry
			if json.Unmarshal(e.Value, &entry) != nil {
				continue
			}
			if actor != "" && entry.Actor != actor {
				continue
			}
			if action != "" && !strings.Contains(entry.Action, action) {
				continue
			}
			out = append(out, AuditRow{ID: strings.TrimPrefix(e.Key, auditPrefix), AuditEntry: entry})
			if len(out) == limit {
				return out, nil
			}
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
	return out, nil
}
