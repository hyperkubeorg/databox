// builds.go — wiring for the Builds runtime layer (Draft 003 §6.2): the
// buildwire listener, the dispatch/ingest loop, and the cleanup worker
// are constructed here with the cross-domain resolvers (spec bytes from
// git, sealed secrets from the build store) that keep pkg/build free of
// those imports. main.go starts the returned loops.
package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-git/go-git/v5/plumbing"

	buildruntime "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/build"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
	dbuild "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// maxSpecBytes caps the `.pcp-builder.yaml` size read from a commit.
const maxSpecBytes = 1 << 20

// buildRuntime bundles the runtime loops so main.go starts them together.
type buildRuntime struct {
	Listener   *buildruntime.Listener
	Dispatcher *buildruntime.Dispatcher
	Cleaner    *buildruntime.Cleaner
}

// newBuildRuntime constructs the Builds runtime layer. buildwireAddr ""
// disables the listener (dispatch still runs, but no runner can connect).
func newBuildRuntime(buildwireAddr string, bs *dbuild.Store, gs *dgit.Store, st *site.Store, log *slog.Logger) (*buildRuntime, error) {
	reg := buildruntime.NewRegistry()
	var listener *buildruntime.Listener
	if buildwireAddr != "" {
		l, err := buildruntime.NewListener(buildwireAddr, bs, st, reg, log)
		if err != nil {
			return nil, err
		}
		listener = l
	}
	disp := buildruntime.NewDispatcher(bs, st, reg, log)
	disp.Spec = specResolver(gs)
	disp.Secrets = secretsResolver(bs, gs)
	// Profile resolution (§7.2) and quota refund (§10.1) are wired once the
	// profile store lands; nil here means image defaults and no refund.
	cleaner := &buildruntime.Cleaner{Build: bs, Site: st, Log: log}
	return &buildRuntime{Listener: listener, Dispatcher: disp, Cleaner: cleaner}, nil
}

// specResolver reads a build's `.pcp-builder.yaml` from its triggering
// commit (§5.1).
func specResolver(gs *dgit.Store) func(context.Context, dbuild.Build) ([]byte, error) {
	return func(ctx context.Context, b dbuild.Build) ([]byte, error) {
		repo, found, err := gs.GetRepo(ctx, b.RepoID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("repo %s not found", b.RepoID)
		}
		sto, err := gs.Storer(ctx, repo)
		if err != nil {
			return nil, err
		}
		var commit plumbing.Hash
		if b.Trigger.Commit != "" {
			commit = plumbing.NewHash(b.Trigger.Commit)
		} else {
			h, ok, err := sto.ResolveRef(repo.DefaultBranch)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("cannot resolve %s", repo.DefaultBranch)
			}
			commit = h
		}
		fi, found, err := sto.FileAt(commit, ".pcp-builder.yaml", maxSpecBytes)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("no .pcp-builder.yaml in the triggering commit")
		}
		if fi.TooLarge {
			return nil, fmt.Errorf(".pcp-builder.yaml exceeds %d bytes", maxSpecBytes)
		}
		return fi.Content, nil
	}
}

// secretsResolver gathers the sealed secrets for a build's runner scope
// (repo shadows org, §5.3), failing fast if any is sealed to a former
// runner (a runner change invalidates its secrets — re-enter).
func secretsResolver(bs *dbuild.Store, gs *dgit.Store) func(context.Context, dbuild.Build, dbuild.Runner) ([]buildproto.SealedSecret, error) {
	return func(ctx context.Context, b dbuild.Build, runner dbuild.Runner) ([]buildproto.SealedSecret, error) {
		scopes := []string{dbuild.SecretScopeRepoPrefix + b.RepoID}
		if repo, found, err := gs.GetRepo(ctx, b.RepoID); err == nil && found && repo.OwnerNS != "" {
			scopes = append(scopes, dbuild.SecretScopeOrgPrefix+repo.OwnerNS)
		}
		want := dbuild.SealFingerprint(runner.RunnerSealPub)
		seen := map[string]bool{}
		var out []buildproto.SealedSecret
		for _, scope := range scopes {
			names, err := bs.ListSecretNames(ctx, scope)
			if err != nil {
				return nil, err
			}
			for _, name := range names {
				if seen[name] {
					continue // repo scope shadows org
				}
				sec, ok, err := bs.GetSecret(ctx, scope, name)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
				if sec.SealFingerprint != want {
					return nil, fmt.Errorf("secret %q is sealed to a former runner — re-enter it in Builds settings", name)
				}
				seen[name] = true
				out = append(out, buildproto.SealedSecret{Name: name, Sealed: sec.Sealed})
			}
		}
		return out, nil
	}
}
