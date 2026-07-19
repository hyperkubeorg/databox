// Package build holds PCP's side of the Builds CI/CD subsystem (Draft
// 003): the paired build-runner records (the ferry.Gateway twin — §3.1,
// §6.1), per-repo build/phase/step records with their state-partitioned
// indexes (§3.2), releases (§3.5), the compute allowlist keyspace (§4.4),
// and secrets sealed to the assigned runner (§3.6, §5.3). It also owns
// the `.pcp-builder.yaml` pipeline parser and DAG validation (§5).
//
// Key custody mirrors ferry.Gateway / mail.PostOffice: the PCP control
// (ed25519) and seal (X25519) PRIVATE keys live on the Runner record in
// databox — the app's trust root. Because PCP holds only the runner's
// seal PUBLIC key, it can seal secrets to a runner but never open them
// (§5.3); that is the whole point of the envelope.
//
// Every family lives under /pcp/build/ (Draft 003 §14), registered in the
// kvx canonical key table before any code writes it.
package build

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Key families (kvx key table, Draft 003 §14). All under /pcp/build/.
const (
	runnerPrefix      = "/pcp/build/runners/"     // runners/<id>            → Runner
	runnerByPrefix    = "/pcp/build/runnersby/"   // runnersby/<scope>/<id>  → id (per-scope index)
	accessPrefix      = "/pcp/build/access/"      // access/<subject>        → compute allowlist entry
	profilePrefix     = "/pcp/build/profiles/"    // profiles/<id>           → execution profile
	profileBindPrefix = "/pcp/build/profilebind/" // profilebind/<scope>     → profile binding
	seqPrefix         = "/pcp/build/seq/"         // seq/<repoID>            → per-repo build counter (OCC)
	buildsPrefix      = "/pcp/build/builds/"      // builds/<repoID>/<n>     → Build
	buildIdxPrefix    = "/pcp/build/buildidx/"    // buildidx/<repoID>/<class>/<invTs>-<n> → Build copy
	phasesPrefix      = "/pcp/build/phases/"      // phases/<repoID>/<n>/<phase> → Phase (inline steps)
	logsPrefix        = "/pcp/build/logs/"        // logs/<repoID>/<n>/<phase>   → log KV chunks
	logBlobPrefix     = "/pcp/build/logblob/"     // logblob/<repoID>/<n>/<phase>/<seq> → log overflow BLOB
	artifactsPrefix   = "/pcp/build/artifacts/"   // artifacts/<repoID>/<n>/<name> → artifact metadata
	artBlobPrefix     = "/pcp/build/artblob/"     // artblob/<repoID>/<n>/<name>   → artifact BLOB
	releasesPrefix    = "/pcp/build/releases/"    // releases/<repoID>/<releaseID> → Release
	releaseIdxPrefix  = "/pcp/build/releaseidx/"  // releaseidx/<repoID>/<invTs>-<releaseID> → Release copy
	relTagPrefix      = "/pcp/build/reltag/"      // reltag/<repoID>/<tag>   → releaseID (uniqueness claim)
	relBlobPrefix     = "/pcp/build/relblob/"     // relblob/<repoID>/<releaseID>/<name> → durable release BLOB
	secretsPrefix     = "/pcp/build/secrets/"     // secrets/<scope>/<name>  → sealed Secret
)

// ErrNotFound is the package's missing-record error.
var ErrNotFound = fmt.Errorf("not found")

// txRetryAttempts bounds OCC retries on the shared per-repo build
// sequence (racing build creates conflict by design — one retry claims
// the next number).
const txRetryAttempts = 16

// Store wraps the databox client with the Builds records. Deps are kept
// minimal — the DB is the trust root, the logger is for the workers that
// land in later phases.
type Store struct {
	DB  *client.Client
	Log *slog.Logger
}

// runTxRetry retries RunTx on OCC conflicts (bounded): the shared
// per-repo build counter makes racing creates conflict by design, and a
// retry simply claims the next number.
func (s *Store) runTxRetry(ctx context.Context, fn func(tx *client.Tx) error) error {
	var err error
	for i := 0; i < txRetryAttempts; i++ {
		if err = s.DB.RunTx(ctx, fn); !kvx.IsConflict(err) {
			return err
		}
	}
	return err
}
