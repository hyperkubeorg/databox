// reposettings.go — the repo-admin mutations behind the settings page
// and the repo API (§5.1/§5.2): description, default branch (symbolic
// HEAD — the storer derives HEAD from the record, so flipping it here
// IS the HEAD move), the visibility flip with its §5.3 fork block, and
// branch deletion (which refuses the default branch for the same
// reason). All OCC read-modify-write; the app layer gates on repo-admin
// (or write, for branch deletion) and audits.
package git

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// ErrForksBlockPrivate blocks a public→private flip while forks exist
// (§5.3 — fork reads go through the parent, so going private would leak).
var ErrForksBlockPrivate = fmt.Errorf("this repository has forks that read through it — delete the forks before making it private")

// updateRepo is the shared OCC read-modify-write for repo records.
func (s *Store) updateRepo(ctx context.Context, repoID string, mutate func(tx *client.Tx, r *Repo) error) error {
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var r Repo
		found, err := txGetJSON(ctx, tx, repoKey(repoID), &r)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		if err := mutate(tx, &r); err != nil {
			return err
		}
		txSetJSON(tx, repoKey(repoID), r)
		return nil
	})
}

// SetRepoDescription writes the description (§5.2 settings).
func (s *Store) SetRepoDescription(ctx context.Context, repoID, description string) error {
	description = strings.TrimSpace(description)
	if len(description) > maxRepoDescription {
		return fmt.Errorf("descriptions are capped at %d characters", maxRepoDescription)
	}
	return s.updateRepo(ctx, repoID, func(_ *client.Tx, r *Repo) error {
		r.Description = description
		return nil
	})
}

// SetRepoDefaultBranch moves symbolic HEAD (§6.2): the branch must
// exist, checked inside the transaction so a racing branch delete
// conflicts rather than leaving HEAD dangling.
func (s *Store) SetRepoDefaultBranch(ctx context.Context, repoID, branch string) error {
	refname := "refs/heads/" + branch
	if err := validRefName(refname); err != nil {
		return err
	}
	return s.updateRepo(ctx, repoID, func(tx *client.Tx, r *Repo) error {
		if _, found, err := tx.Get(ctx, refKey(repoID, refname)); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("no branch named %q", branch)
		}
		r.DefaultBranch = branch
		return nil
	})
}

// SetRepoVisibility flips visibility (§5.1): public needs allowPublic;
// public→private is blocked while forks exist (§5.3), the fork check
// riding the transaction so a racing fork conflicts at commit. The app
// layer audits every flip (§13).
func (s *Store) SetRepoVisibility(ctx context.Context, repoID, visibility string, allowPublic bool) error {
	switch visibility {
	case VisPrivate, VisPublic:
	default:
		return fmt.Errorf("bad visibility %q", visibility)
	}
	if visibility == VisPublic && !allowPublic {
		return fmt.Errorf("public repositories are disabled on this site")
	}
	return s.updateRepo(ctx, repoID, func(tx *client.Tx, r *Repo) error {
		if visibility == VisPrivate && r.Visibility == VisPublic {
			if entries, _, err := tx.List(ctx, forksPrefix+repoID+"/", "", 1); err != nil {
				return err
			} else if len(entries) > 0 {
				return ErrForksBlockPrivate
			}
		}
		r.Visibility = visibility
		return nil
	})
}

// CreateBranch creates refs/heads/<branch> pointing at an existing commit
// (the caller resolves `at` from a source ref). It refuses a name that
// already exists; the single-transaction existence check makes a concurrent
// push to the same ref conflict. Creating a branch adds no objects and
// orphans none, so unlike DeleteBranch it schedules no GC.
func (s *Store) CreateBranch(ctx context.Context, repo Repo, branch string, at plumbing.Hash) error {
	refname := "refs/heads/" + branch
	if err := validRefName(refname); err != nil {
		return err
	}
	if at.IsZero() {
		return fmt.Errorf("can't create a branch from an empty revision")
	}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var r Repo
		found, err := txGetJSON(ctx, tx, repoKey(repo.ID), &r)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		if _, found, err := tx.Get(ctx, refKey(repo.ID, refname)); err != nil {
			return err
		} else if found {
			return fmt.Errorf("a branch named %q already exists", branch)
		}
		tx.Set(refKey(repo.ID, refname), []byte(at.String()))
		return nil
	})
	if err != nil {
		return err
	}
	s.NoteRefUpdates(repo.ID, []RefUpdate{{Name: refname, Old: plumbing.ZeroHash, New: at}})
	return nil
}

// DeleteBranch removes one branch ref. The default branch is refused —
// it IS symbolic HEAD (§6.2); move the default first. One transaction:
// the existence read makes a concurrent push to the same ref conflict.
// This is the ONE ref mutation that bypasses ApplyRefUpdates, so it
// feeds NoteRefUpdates itself (§6.5 — a deletion always schedules the
// automatic GC).
func (s *Store) DeleteBranch(ctx context.Context, repo Repo, branch string) error {
	refname := "refs/heads/" + branch
	if err := validRefName(refname); err != nil {
		return err
	}
	var old plumbing.Hash
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var r Repo
		found, err := txGetJSON(ctx, tx, repoKey(repo.ID), &r)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		if branch == r.DefaultBranch {
			return fmt.Errorf("%q is the default branch (the repository's HEAD) — change the default branch first", branch)
		}
		raw, found, err := tx.Get(ctx, refKey(repo.ID, refname))
		if err != nil {
			return err
		} else if !found {
			return fmt.Errorf("no branch named %q", branch)
		}
		old = plumbing.NewHash(strings.TrimSpace(string(raw)))
		tx.Delete(refKey(repo.ID, refname))
		return nil
	})
	if err != nil {
		return err
	}
	s.NoteRefUpdates(repo.ID, []RefUpdate{{Name: refname, Old: old, New: plumbing.ZeroHash}})
	return nil
}
