// repos.go — repository records (§5.1) and forks (§5.3): the repo
// record keyed by a stable kvx.NewID (object and blob keys never move),
// the per-ns name index maintained in the same transaction, the
// /pcp/git/forks/<parentID>/<childID> reverse index that enforces the
// fork-block on delete, and the atomic ref-update transaction
// receive-pack rides (§6.2 — git's multi-ref push semantics fall out of
// one OCC RunTx). Namespace quota resolution for pushes (§6.5/§7) lives
// here too: one function dispatches user vs org.
package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// DefaultBranch is the branch new repositories point HEAD at.
const DefaultBranch = "main"

// maxForkDepth caps the forkOf read-through chain (§6.2).
const maxForkDepth = 10

// maxRepoDescription bounds the description field.
const maxRepoDescription = 500

// Errors the app layer translates.
var (
	// ErrHasForks blocks deletion while forks point at the repo (§5.3).
	ErrHasForks = fmt.Errorf("this repository has forks — delete the forks first")
	// ErrNoCreate is a namespace the caller may not create repos in.
	ErrNoCreate = fmt.Errorf("you can't create repositories in that namespace")
)

// Key helpers (kvx key table, §14).
func repoKey(id string) string             { return repoPrefix + id }
func repoNameKey(ns, name string) string   { return repoNamePrefix + ns + "/" + name }
func forkKey(parent, child string) string  { return forksPrefix + parent + "/" + child }
func refKey(repoID, refname string) string { return refsPrefix + repoID + "/" + refname }
func objKey(repoID, sha string) string     { return objPrefix + repoID + "/" + sha }
func objBlobKey(repoID, sha string) string { return objBlobPrefix + repoID + "/" + sha }

// ValidRepoName gates repository names: the platform's shared
// key-segment rule (3–32 of a-z, 0-9, dashes) — names become key
// segments under reponame/<ns>/ and clone-URL path segments.
func ValidRepoName(name string) error {
	return kvx.ValidKeyName(name, "repository name")
}

// validRefName gates a refname before it becomes a key segment under
// refs/<repoID>/: git's own refname rules (via go-git) plus printable
// ASCII only, so the raw refname IS the key suffix — key-charset safe
// and List-order preserving with no escaping. Non-ASCII branch names
// are rejected in v1 (an honest cut; an order-preserving encoding can
// lift it later without moving keys).
func validRefName(name string) error {
	// Only refs/… ever stores: HEAD is derived (storer.go), and a name
	// outside refs/ has no meaning on the wire.
	if !strings.HasPrefix(name, "refs/") {
		return fmt.Errorf("bad ref name %q", name)
	}
	if err := plumbing.ReferenceName(name).Validate(); err != nil {
		return fmt.Errorf("bad ref name %q", name)
	}
	for _, r := range name {
		if r <= 0x20 || r >= 0x7f {
			return fmt.Errorf("ref name %q: only printable ASCII is supported", name)
		}
	}
	return nil
}

// GetRepo loads one repository record by id.
func (s *Store) GetRepo(ctx context.Context, repoID string) (Repo, bool, error) {
	if !kvx.ValidID(repoID) {
		return Repo{}, false, nil
	}
	var r Repo
	found, err := kvx.GetJSON(ctx, s.DB, repoKey(repoID), &r)
	return r, found, err
}

// GetRepoByPath resolves ns/name through the reponame index (§5.1).
func (s *Store) GetRepoByPath(ctx context.Context, ns, name string) (Repo, bool, error) {
	ns, name = strings.ToLower(ns), strings.ToLower(name)
	if kvx.ValidKeyName(ns, "ns") != nil || ValidRepoName(name) != nil {
		return Repo{}, false, nil
	}
	e, found, err := s.DB.Get(ctx, repoNameKey(ns, name))
	if err != nil || !found {
		return Repo{}, false, err
	}
	return s.GetRepo(ctx, string(e.Value))
}

// ListReposByNS lists a namespace's repositories, name-ascending (one
// prefix List over the name index; bounded — a household namespace).
// Visibility filtering is the caller's job via RoleFor (§4.3).
func (s *Store) ListReposByNS(ctx context.Context, ns string) ([]Repo, error) {
	ns = strings.ToLower(ns)
	if kvx.ValidKeyName(ns, "ns") != nil {
		return nil, nil
	}
	var out []Repo
	err := kvx.ScanPrefix(ctx, s.DB, repoNamePrefix+ns+"/", func(_ string, v []byte) error {
		r, found, err := s.GetRepo(ctx, string(v))
		if err != nil {
			return err
		}
		if found {
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// CanCreateIn reports whether user may create a repository under ns
// (§5.1): their own namespace always; an org when they're an owner, or
// a member and the org's members-create setting is on.
func (s *Store) CanCreateIn(ctx context.Context, user, ns string) (bool, error) {
	user, ns = strings.ToLower(user), strings.ToLower(ns)
	if user == "" {
		return false, nil
	}
	if user == ns {
		return true, nil
	}
	reg, found, err := s.GetNS(ctx, ns)
	if err != nil || !found || reg.Kind != NSKindOrg {
		return false, err
	}
	m, member, err := s.GetMember(ctx, ns, user)
	if err != nil || !member {
		return false, err
	}
	if m.Role == OrgRoleOwner {
		return true, nil
	}
	org, ok, err := s.GetOrg(ctx, ns)
	if err != nil || !ok {
		return false, err
	}
	return org.MembersCanCreateRepos, nil
}

// CreateRepoInput carries one creation request (§5.1).
type CreateRepoInput struct {
	Creator     string
	NS          string
	Name        string
	Description string
	Visibility  string // "" = private
	// InitReadme makes a real initial commit (README.md) through the
	// storer, with a signature derived from the creator.
	InitReadme bool
	// AllowPublic is site.Config.GitPublicReposAllowed() — the domain
	// takes it as a value so it never reads site config itself.
	AllowPublic bool
}

// CreateRepo creates a repository: permission check (CanCreateIn), name
// validity, and uniqueness via an OCC claim on the reponame index — the
// record and the index commit in one transaction. Public visibility
// requires AllowPublic (§2/§5.1).
func (s *Store) CreateRepo(ctx context.Context, in CreateRepoInput) (Repo, error) {
	in.Creator = strings.ToLower(in.Creator)
	in.NS = strings.ToLower(strings.TrimSpace(in.NS))
	in.Name = strings.ToLower(strings.TrimSpace(in.Name))
	in.Description = strings.TrimSpace(in.Description)
	if err := ValidRepoName(in.Name); err != nil {
		return Repo{}, err
	}
	if len(in.Description) > maxRepoDescription {
		return Repo{}, fmt.Errorf("descriptions are capped at %d characters", maxRepoDescription)
	}
	switch in.Visibility {
	case "":
		in.Visibility = VisPrivate
	case VisPrivate:
	case VisPublic:
		if !in.AllowPublic {
			return Repo{}, fmt.Errorf("public repositories are disabled on this site")
		}
	default:
		return Repo{}, fmt.Errorf("bad visibility %q", in.Visibility)
	}
	if ok, err := s.CanCreateIn(ctx, in.Creator, in.NS); err != nil {
		return Repo{}, err
	} else if !ok {
		return Repo{}, ErrNoCreate
	}
	repo := Repo{
		ID: kvx.NewID(), OwnerNS: in.NS, Name: in.Name, Description: in.Description,
		Visibility: in.Visibility, DefaultBranch: DefaultBranch,
		CreatedAt: time.Now().UTC(),
	}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, taken, err := tx.Get(ctx, repoNameKey(in.NS, in.Name)); err != nil {
			return err
		} else if taken {
			return ErrNameTaken
		}
		txSetJSON(tx, repoKey(repo.ID), repo)
		tx.Set(repoNameKey(in.NS, in.Name), []byte(repo.ID))
		return nil
	})
	if err != nil {
		return Repo{}, err
	}
	if in.InitReadme {
		if err := s.initReadme(ctx, &repo, in.Creator); err != nil {
			// The record exists; the README is best-effort sugar. Surface
			// the error — the caller decides — but don't orphan the repo.
			return repo, fmt.Errorf("repository created, but the initial commit failed: %w", err)
		}
	}
	return repo, nil
}

// ForkRepo forks parent into ns/name (§5.3): a new record with forkOf
// set, private by default, plus a copy of the parent's refs and the
// fork reverse-index row — one transaction. Objects are NOT copied (the
// storer reads through the fork chain); the fork charges nothing.
// Callers gate on RoleFor(user, parent) >= read.
func (s *Store) ForkRepo(ctx context.Context, user string, parent Repo, ns, name string) (Repo, error) {
	user = strings.ToLower(user)
	ns = strings.ToLower(strings.TrimSpace(ns))
	name = strings.ToLower(strings.TrimSpace(name))
	if err := ValidRepoName(name); err != nil {
		return Repo{}, err
	}
	if role, err := s.RoleFor(ctx, user, &parent); err != nil {
		return Repo{}, err
	} else if role < RoleRead {
		return Repo{}, ErrNotFound // unconfirmable (§4.3)
	}
	if ok, err := s.CanCreateIn(ctx, user, ns); err != nil {
		return Repo{}, err
	} else if !ok {
		return Repo{}, ErrNoCreate
	}
	// The fork chain is capped at maxForkDepth for reads (§6.2); refuse
	// to create a fork that could never be read through.
	if depth, err := s.forkDepth(ctx, parent); err != nil {
		return Repo{}, err
	} else if depth+1 >= maxForkDepth {
		return Repo{}, fmt.Errorf("fork chains are capped at %d", maxForkDepth)
	}
	fork := Repo{
		ID: kvx.NewID(), OwnerNS: ns, Name: name,
		Description: parent.Description, Visibility: VisPrivate,
		DefaultBranch: parent.DefaultBranch, ForkOf: parent.ID,
		CreatedAt: time.Now().UTC(),
	}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, taken, err := tx.Get(ctx, repoNameKey(ns, name)); err != nil {
			return err
		} else if taken {
			return ErrNameTaken
		}
		// The parent must still exist (a racing delete conflicts here).
		if _, found, err := tx.Get(ctx, repoKey(parent.ID)); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
		txSetJSON(tx, repoKey(fork.ID), fork)
		tx.Set(repoNameKey(ns, name), []byte(fork.ID))
		tx.Set(forkKey(parent.ID, fork.ID), []byte(fork.ID))
		// Copy the parent's refs — O(refs), the whole point of §5.3.
		srcPrefix := refsPrefix + parent.ID + "/"
		return txScan(ctx, tx, srcPrefix, func(key string, v []byte) error {
			tx.Set(refKey(fork.ID, strings.TrimPrefix(key, srcPrefix)), v)
			return nil
		})
	})
	if err != nil {
		return Repo{}, err
	}
	return fork, nil
}

// forkDepth walks parent's forkOf chain and returns its length.
func (s *Store) forkDepth(ctx context.Context, r Repo) (int, error) {
	depth := 0
	for cur := r; cur.ForkOf != "" && depth < maxForkDepth; depth++ {
		next, found, err := s.GetRepo(ctx, cur.ForkOf)
		if err != nil {
			return 0, err
		}
		if !found {
			break
		}
		cur = next
	}
	return depth, nil
}

// ForkChain resolves the repoID read path for the storer (§6.2): the
// repo itself, then each forkOf ancestor, depth-capped.
func (s *Store) ForkChain(ctx context.Context, r Repo) ([]string, error) {
	chain := []string{r.ID}
	cur := r
	for cur.ForkOf != "" && len(chain) < maxForkDepth {
		next, found, err := s.GetRepo(ctx, cur.ForkOf)
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		chain = append(chain, next.ID)
		cur = next
	}
	return chain, nil
}

// DeleteRepo removes a repository (§5.1): blocked while any fork points
// at it, then one transaction deletes the record, the name index, and
// the fork reverse-index row, making it unreachable — refs, objects,
// and object blobs are swept afterwards (unreachable-first, so a
// crashed sweep never leaves a reachable half-repo), and sizeBytes is
// refunded to the owning namespace. The app layer gates on repo-admin
// and audits.
func (s *Store) DeleteRepo(ctx context.Context, repoID string) error {
	repo, found, err := s.GetRepo(ctx, repoID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	// Outbound-MR block (§9, the fork-block philosophy): a repo that is
	// the SOURCE of an open merge request in ANOTHER repo can't delete —
	// the MR's diff and merge read this repo's objects. The message
	// names the blocking MRs; same-repo MRs die with the repo.
	if outbound, err := s.openOutboundMRs(ctx, repoID); err != nil {
		return err
	} else if len(outbound) > 0 {
		return fmt.Errorf("%w: %s", ErrHasOpenMRs, strings.Join(outbound, ", "))
	}
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, found, err := tx.Get(ctx, repoKey(repoID)); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
		// §5.3 fork-block: one child under forks/<id>/ blocks. The read
		// rides the transaction, so a racing fork conflicts at commit.
		if entries, _, err := tx.List(ctx, forksPrefix+repoID+"/", "", 1); err != nil {
			return err
		} else if len(entries) > 0 {
			return ErrHasForks
		}
		// A racing MR create conflicts here: its mrsrc row is re-checked
		// on the transaction so the block can't be raced around.
		if entries, _, err := tx.List(ctx, mrSrcPrefix+repoID+"/", "", 1); err != nil {
			return err
		} else if len(entries) > 0 {
			var ref mrSrcRef
			if json.Unmarshal(entries[0].Value, &ref) == nil && ref.TargetRepoID != repoID {
				return ErrHasOpenMRs
			}
		}
		tx.Delete(repoKey(repoID))
		tx.Delete(repoNameKey(repo.OwnerNS, repo.Name))
		if repo.ForkOf != "" {
			tx.Delete(forkKey(repo.ForkOf, repoID))
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Sweep the storage families. Object blobs need per-key DeleteBlob
	// (their data lives in blob manifests); refs and KV objects go in
	// range deletes.
	if err := kvx.DeletePrefix(ctx, s.DB, refsPrefix+repoID+"/"); err != nil {
		return err
	}
	if err := kvx.DeletePrefix(ctx, s.DB, objPrefix+repoID+"/"); err != nil {
		return err
	}
	err = kvx.ScanPrefix(ctx, s.DB, objBlobPrefix+repoID+"/", func(key string, _ []byte) error {
		return s.DB.DeleteBlob(ctx, key)
	})
	if err != nil {
		return err
	}
	// Issues, comments, labels, the number sequence, and the assigned
	// rows (§5.1 "removes … issues/MRs") — phase-4 families.
	if err := s.deleteIssueData(ctx, repoID); err != nil {
		return err
	}
	// Merge requests targeting this repo (records, index, assigned rows,
	// and their source-side mrsrc rows) — phase-5 families.
	if err := s.deleteMergeData(ctx, repoID); err != nil {
		return err
	}
	if repo.SizeBytes > 0 {
		if err := s.ChargeNSQuota(ctx, repo.OwnerNS, -repo.SizeBytes, 0); err != nil {
			return err
		}
	}
	return nil
}

// --- quota (§6.5, §7) --------------------------------------------------------

// NSQuotaLimit resolves the owning namespace's effective quota: a
// user's through the platform fields, an org's through its own
// Tier/QuotaOverride — both via the same site.QuotaFor precedence.
func (s *Store) NSQuotaLimit(ctx context.Context, sc site.Config, ns string, bootstrap int64) (int64, error) {
	ns = strings.ToLower(ns)
	reg, found, err := s.GetNS(ctx, ns)
	if err != nil {
		return 0, err
	}
	if found && reg.Kind == NSKindOrg {
		org, ok, err := s.GetOrg(ctx, ns)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, ErrNotFound
		}
		return site.QuotaFor(sc, org.QuotaOverride, org.Tier, bootstrap), nil
	}
	u, ok, err := s.Users.Get(ctx, ns)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrNotFound
	}
	return site.QuotaFor(sc, u.QuotaOverride, u.Tier, bootstrap), nil
}

// ChargeNSQuota adjusts a namespace's storage usage by delta: the user
// path through users.ChargeQuota, the org path through ChargeOrgQuota —
// identical semantics (limit > 0 enforces on positive charges; refunds
// pass 0 and floor at zero).
func (s *Store) ChargeNSQuota(ctx context.Context, ns string, delta, limit int64) error {
	ns = strings.ToLower(ns)
	reg, found, err := s.GetNS(ctx, ns)
	if err != nil {
		return err
	}
	if found && reg.Kind == NSKindOrg {
		return s.ChargeOrgQuota(ctx, ns, delta, limit)
	}
	if err := s.Users.ChargeQuota(ctx, ns, delta, limit); err != nil {
		if errors.Is(err, users.ErrQuotaExceeded) {
			return ErrQuotaExceeded // one error value for both namespace kinds
		}
		return err
	}
	return nil
}

// --- atomic ref updates (§6.2/§6.3) -------------------------------------------

// RefUpdate is one receive-pack command: create (Old zero), update, or
// delete (New zero).
type RefUpdate struct {
	Name string
	Old  plumbing.Hash
	New  plumbing.Hash
}

// ErrStale is a compare-and-swap miss on a ref update — the client's
// view of the ref is behind (git's "fetch first").
var ErrStale = fmt.Errorf("failed to update ref: not the current value (fetch first)")

// ApplyRefUpdates applies a push's ref commands in ONE OCC transaction
// (§6.2): every command's old value is compare-and-swapped against the
// stored ref, so git's atomic multi-ref push semantics fall out of
// RunTx. A command whose precondition fails rejects the WHOLE push —
// per-command partial application would break the one-transaction
// guarantee. sizeDelta (stored bytes this push added) lands on the repo
// record in the same transaction. A successful batch feeds
// NoteRefUpdates (§6.5): EVERY ref mutation except the branches-page
// delete flows through here — wire receive-pack, web-editor commits,
// the initial README — so this one hook sees them all and schedules the
// automatic GC when the batch could have orphaned objects.
func (s *Store) ApplyRefUpdates(ctx context.Context, repoID string, updates []RefUpdate, sizeDelta int64) error {
	for _, u := range updates {
		if err := validRefName(u.Name); err != nil {
			return err
		}
	}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var repo Repo
		found, err := txGetJSON(ctx, tx, repoKey(repoID), &repo)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		for _, u := range updates {
			raw, exists, err := tx.Get(ctx, refKey(repoID, u.Name))
			if err != nil {
				return err
			}
			cur := plumbing.ZeroHash
			if exists {
				cur = plumbing.NewHash(strings.TrimSpace(string(raw)))
			}
			if cur != u.Old {
				return fmt.Errorf("%w: %s", ErrStale, u.Name)
			}
			if u.New.IsZero() {
				if exists {
					tx.Delete(refKey(repoID, u.Name))
				}
				continue
			}
			tx.Set(refKey(repoID, u.Name), []byte(u.New.String()))
		}
		if sizeDelta != 0 {
			repo.SizeBytes += sizeDelta
			if repo.SizeBytes < 0 {
				repo.SizeBytes = 0
			}
			txSetJSON(tx, repoKey(repoID), repo)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.NoteRefUpdates(repoID, updates)
	return nil
}

// AddRepoSize adjusts a repo record's sizeBytes (delete refunds, GC).
func (s *Store) AddRepoSize(ctx context.Context, repoID string, delta int64) error {
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var repo Repo
		found, err := txGetJSON(ctx, tx, repoKey(repoID), &repo)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		repo.SizeBytes += delta
		if repo.SizeBytes < 0 {
			repo.SizeBytes = 0
		}
		txSetJSON(tx, repoKey(repoID), repo)
		return nil
	})
}

// Forks lists the repoIDs forked from parent (bounded at household
// scale; the web fork list and leak-rule filtering ride this).
func (s *Store) Forks(ctx context.Context, parentID string) ([]string, error) {
	if !kvx.ValidID(parentID) {
		return nil, nil
	}
	var out []string
	err := kvx.ScanPrefix(ctx, s.DB, forksPrefix+parentID+"/", func(_ string, v []byte) error {
		out = append(out, string(v))
		return nil
	})
	return out, err
}
