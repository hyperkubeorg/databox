// initcommit.go — the optional "initialize with README" first commit
// (§5.1): a real blob/tree/commit written through the databox storer,
// with a signature derived from the creator, landing on the default
// branch via the same atomic ref-update path pushes use.
package git

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// initReadme writes the initial commit and points the default branch at
// it. The handful of stored bytes are charged to the owning namespace
// (no limit check — a README never tips a quota meaningfully) and land
// on the repo record's sizeBytes through ApplyRefUpdates.
func (s *Store) initReadme(ctx context.Context, repo *Repo, creator string) error {
	sto, err := s.Storer(ctx, *repo)
	if err != nil {
		return err
	}
	// Signature: the shared browser-commit identity (webcommit.go) —
	// display name + the synthetic @pcp.local address (§11).
	sig := s.signatureFor(ctx, creator)

	readme := fmt.Sprintf("# %s\n", repo.Name)
	if repo.Description != "" {
		readme += "\n" + repo.Description + "\n"
	}
	blobHash, err := writeBlob(sto, []byte(readme))
	if err != nil {
		return err
	}

	tree := object.Tree{Entries: []object.TreeEntry{
		{Name: "README.md", Mode: filemode.Regular, Hash: blobHash},
	}}
	treeObj := sto.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		return err
	}
	treeHash, err := sto.SetEncodedObject(treeObj)
	if err != nil {
		return err
	}

	commit := object.Commit{
		Author: sig, Committer: sig,
		Message:  "Initial commit\n",
		TreeHash: treeHash,
	}
	commitObj := sto.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return err
	}
	commitHash, err := sto.SetEncodedObject(commitObj)
	if err != nil {
		return err
	}
	if err := sto.Flush(); err != nil {
		return err
	}
	size := sto.StoredBytes()
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{
		{Name: "refs/heads/" + repo.DefaultBranch, Old: plumbing.ZeroHash, New: commitHash},
	}, size); err != nil {
		return err
	}
	repo.SizeBytes += size
	return s.ChargeNSQuota(ctx, repo.OwnerNS, size, 0)
}
