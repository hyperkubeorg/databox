// roles.go — the repo role ladder (§4.1) and RoleFor, the ONE
// resolution function every surface calls (§4.3): web handlers, the
// API (scope check then RoleFor), and the git wire endpoints. Nothing
// else may inline permission logic — this function carries the
// exhaustive test matrix and is the review gate for any new capability.
package git

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Role is a resolved repo capability level (§4.1). The order is the
// ladder: each level includes everything below it.
type Role int

const (
	RoleNone  Role = iota
	RoleRead       // clone/fetch, browse, open and comment on issues/MRs
	RoleWrite      // push, merge MRs, manage issues/labels/assignees
	RoleAdmin      // repo settings, visibility, grants, delete/transfer
)

// String names a role for grant rows and UI chips.
func (r Role) String() string {
	switch r {
	case RoleRead:
		return "read"
	case RoleWrite:
		return "write"
	case RoleAdmin:
		return "admin"
	}
	return "none"
}

// RoleFromGrant parses a stored/submitted grant role — grants never
// carry "none" (that's an absent grant).
func RoleFromGrant(s string) (Role, error) {
	switch s {
	case "read":
		return RoleRead, nil
	case "write":
		return RoleWrite, nil
	case "admin":
		return RoleAdmin, nil
	}
	return RoleNone, fmt.Errorf("bad role %q", s)
}

// roleFromPerm maps an org's DefaultRepoPerm (none|read|write) to a Role.
func roleFromPerm(p string) Role {
	switch p {
	case PermRead:
		return RoleRead
	case PermWrite:
		return RoleWrite
	}
	return RoleNone
}

// Repo visibility values (§5.1).
const (
	VisPublic  = "public"
	VisPrivate = "private"
)

// Repo is the repository record (§5.1). Phase 2/3 add the storage and
// web surfaces; the struct lives here now so RoleFor compiles against
// the real shape. repoID is a kvx.NewID — stable across rename/transfer.
type Repo struct {
	ID          string `json:"id"`
	OwnerNS     string `json:"owner_ns"` // user or org name (one namespace, §3.1)
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Visibility  string `json:"visibility"` // public | private
	// DefaultBranch is symbolic HEAD (§6.2).
	DefaultBranch string `json:"default_branch"`
	// ForkOf is the parent repoID, or empty (§5.3).
	ForkOf    string    `json:"fork_of,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes int64     `json:"size_bytes"`
}

// RoleFor resolves what user may do with repo (§4.3): the maximum of
// public visibility (read for everyone including anonymous user == ""),
// personal ownership (admin), org ownership (admin) / org default repo
// permission for members, team grants, and direct grants. Callers hide
// no-access repos as 404, never 403 (§4.3).
func (s *Store) RoleFor(ctx context.Context, user string, repo *Repo) (Role, error) {
	user = strings.ToLower(user)
	role := RoleNone
	if repo.Visibility == VisPublic {
		role = RoleRead // §4.3 rule 1 — includes anonymous
	}
	if user == "" {
		return role, nil
	}

	owner := strings.ToLower(repo.OwnerNS)
	ns, found, err := s.GetNS(ctx, owner)
	if err != nil {
		return RoleNone, err
	}
	if found && ns.Kind == NSKindOrg {
		// §4.3 rule 3: org owner → admin; members get the org default.
		m, member, err := s.GetMember(ctx, owner, user)
		if err != nil {
			return RoleNone, err
		}
		if member {
			if m.Role == OrgRoleOwner {
				return RoleAdmin, nil
			}
			org, ok, err := s.GetOrg(ctx, owner)
			if err != nil {
				return RoleNone, err
			}
			if ok {
				role = max(role, roleFromPerm(org.MemberRepoPerm()))
			}
		}
	} else if user == owner {
		// §4.3 rule 2: personal repos — owner → admin.
		return RoleAdmin, nil
	}

	// §4.3 rules 4 + 5: team grants where the user is a member, and
	// direct user grants. Max wins.
	grants, err := s.GrantsForRepo(ctx, repo.ID)
	if err != nil {
		return RoleNone, err
	}
	for _, g := range grants {
		granted, err := RoleFromGrant(g.Role)
		if err != nil || granted <= role {
			continue
		}
		kind, gu, gorg, gteam, ok := parseSubject(g.Subject)
		if !ok {
			continue
		}
		switch kind {
		case "u":
			if gu == user {
				role = granted
			}
		case "t":
			t, found, err := s.GetTeam(ctx, gorg, gteam)
			if err != nil {
				return RoleNone, err
			}
			if found && slices.Contains(t.Members, user) {
				role = granted
			}
		}
	}
	return role, nil
}
