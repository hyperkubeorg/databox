// Package git is the Git Services domain (PROJECT-DRAFT-002): the
// shared user/org namespace registry (ns.go), opt-in profiles
// (profiles.go), organizations with owner/member roles (orgs.go,
// members.go), teams (teams.go), repository access grants with their
// user-side reverse index (grants.go), the one RoleFor resolution
// function every surface calls (roles.go), and org quota accounting
// (quota.go). Keys live under /pcp/git/ (kvx key table). All §N
// references are to PROJECT-DRAFT-002.
//
// Git object storage, repositories, issues, and merge requests land in
// later build phases (§15); the records here are shaped so they slot in
// (grants reference repos by repoID string).
package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Key prefixes this package owns (kvx key table, §14).
const (
	nsPrefix         = "/pcp/git/ns/"
	profilesPrefix   = "/pcp/git/profiles/"
	orgsPrefix       = "/pcp/git/orgs/"
	orgMembersPrefix = "/pcp/git/orgmembers/"
	userOrgsPrefix   = "/pcp/git/userorgs/"
	teamsPrefix      = "/pcp/git/teams/"
	grantsPrefix     = "/pcp/git/grants/"
	userGrantsPrefix = "/pcp/git/usergrants/"
	teamGrantsPrefix = "/pcp/git/teamgrants/"
	// Phase-2 families (§5.1, §6.2): repository records, the per-ns name
	// index, the fork reverse index, and the git object/ref storage the
	// databox storer (storer.go) owns.
	repoPrefix     = "/pcp/git/repo/"
	repoNamePrefix = "/pcp/git/reponame/"
	forksPrefix    = "/pcp/git/forks/"
	refsPrefix     = "/pcp/git/refs/"
	objPrefix      = "/pcp/git/obj/"
	objBlobPrefix  = "/pcp/git/objblob/"
	// Phase-4 families (§8): the shared issue/MR number sequence, issue
	// records with their state-partitioned activity index, the per-user
	// assigned index (launcher card + dashboard; MRs join in phase 5),
	// comments (shared verbatim by MRs), and repo labels.
	seqPrefix      = "/pcp/git/seq/"
	issuesPrefix   = "/pcp/git/issues/"
	issueIdxPrefix = "/pcp/git/issueidx/"
	assignedPrefix = "/pcp/git/assigned/"
	commentsPrefix = "/pcp/git/comments/"
	labelsPrefix   = "/pcp/git/labels/"
	// Phase-5 families (§9): merge-request records keyed by TARGET repo
	// (numbers ride the shared seq/ counter), their state-partitioned
	// activity index, and the source-side lookup rows receive-pack uses
	// to refresh open MR heads on push.
	mergesPrefix   = "/pcp/git/merges/"
	mergeIdxPrefix = "/pcp/git/mergeidx/"
	mrSrcPrefix    = "/pcp/git/mrsrc/"
	// SSH-transport families (sshkeys.go): per-user public keys, the
	// fingerprint→owner auth index, and the cluster-shared host key.
	sshKeysPrefix = "/pcp/git/sshkeys/"
	sshFpPrefix   = "/pcp/git/sshfp/"
	sshHostKeyKey = "/pcp/git/sshhostkey"
)

// Errors the app layer translates into user-facing messages.
var (
	ErrNotFound      = errors.New("not found")
	ErrNameTaken     = errors.New("that name is already taken")
	ErrLastOwner     = errors.New("an organization always needs at least one owner")
	ErrQuotaExceeded = errors.New("not enough storage space left")
	// ErrSignInRequired is the §10 anonymous write rejection: anonymous
	// visitors NEVER open issues, comment, or create merge requests —
	// even on public repos where RoleFor grants them read (§8's "read
	// may open issues" is overridden for user == "").
	ErrSignInRequired = errors.New("sign in to do that")
)

// Store wraps the databox client with the Git Services access methods.
// Users resolves account existence for the shared namespace (§3.1) and
// membership targets — git sits above users in the domain layering.
type Store struct {
	DB    *client.Client
	Users *users.Store
	// Notify is the platform notification stream (§11) — nil skips
	// issue/MR notifications entirely (unit tests, partial wiring).
	Notify *notify.Store
	// Mail delivers the opt-in email copies of issue/MR events (§11).
	// nil — or Mail disabled in site config — skips them: Git Services
	// never depends on Mail being on.
	Mail *mail.Store
	// Log is the fan-out warn channel (nil falls back to slog.Default).
	Log *slog.Logger
	// testHookPreMergeTx, when set (tests only), runs between MergeMR's
	// object staging and its commit transaction — the CAS-race injection
	// point (§9's "target moved" path).
	testHookPreMergeTx func()
	// repoLocks backs LockRepoPush (gc.go): one in-process mutex per
	// repoID so a push and a GC on this replica never interleave; the
	// databox lock covers other replicas.
	repoLocks sync.Map
	// GCDebounce is the automatic-maintenance trigger→collection delay
	// (gcauto.go); zero means the 30s default. cmd/pcp sets it from
	// PCP_GIT_GC_DEBOUNCE (the smoke shortens it to seconds).
	GCDebounce time.Duration
	// gcMu/gcTimers hold the per-repo debounce timers (gcauto.go): at
	// most one armed timer per repoID, repeats collapse via Reset.
	gcMu     sync.Mutex
	gcTimers map[string]*time.Timer
}

// warn logs a soft failure (lost notifications never fail the action).
func (s *Store) warn(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Warn(msg, args...)
		return
	}
	slog.Warn(msg, args...)
}

// info logs a system event (the automatic-GC completion line, §6.5).
func (s *Store) info(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Info(msg, args...)
		return
	}
	slog.Info(msg, args...)
}

// txGetJSON loads and decodes one record through a caller-owned
// transaction, so the read (present or absent) is OCC-validated.
func txGetJSON(ctx context.Context, tx *client.Tx, key string, v any) (bool, error) {
	raw, found, err := tx.Get(ctx, key)
	if err != nil || !found {
		return false, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, fmt.Errorf("decode %s: %w", key, err)
	}
	return true, nil
}

// txSetJSON encodes and stages one record onto a transaction.
func txSetJSON(tx *client.Tx, key string, v any) {
	raw, _ := json.Marshal(v)
	tx.Set(key, raw)
}

// txScan pages through every key under prefix INSIDE a transaction.
// Bounded collections only (an org's members, a team's grants).
func txScan(ctx context.Context, tx *client.Tx, prefix string, fn func(key string, value []byte) error) error {
	cursor := ""
	for {
		entries, next, err := tx.List(ctx, prefix, cursor, 200)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := fn(e.Key, e.Value); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}
