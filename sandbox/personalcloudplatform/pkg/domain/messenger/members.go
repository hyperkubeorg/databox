// members.go — server membership (join/leave/kick/ban), member listing,
// and the permission engine. Membership rows are written both directions
// (members/, usermembers/) in one transaction, matching drives.
package messenger

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// MemberRow is one entry in a server's member list.
type MemberRow struct {
	Username string `json:"username"`
	Member
}

// Membership is a user-side row resolved to its server id.
type Membership struct {
	ServerID string `json:"server_id"`
	Member
}

// Members lists a server's membership (name-sorted; presence-sorting is a
// view concern layered on top by the app).
func (s *Store) Members(ctx context.Context, serverID string) ([]MemberRow, error) {
	if !kvx.ValidID(serverID) {
		return nil, nil
	}
	var out []MemberRow
	err := kvx.ScanPrefix(ctx, s.DB, membersPrefix+serverID+"/", func(key string, v []byte) error {
		var m Member
		if json.Unmarshal(v, &m) != nil {
			return nil
		}
		out = append(out, MemberRow{Username: key[strings.LastIndex(key, "/")+1:], Member: m})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

// GetMember loads one account's membership of a server.
func (s *Store) GetMember(ctx context.Context, serverID, username string) (Member, bool, error) {
	username = strings.ToLower(username)
	if !kvx.ValidID(serverID) || users.ValidUsername(username) != nil {
		return Member{}, false, nil
	}
	var m Member
	found, err := kvx.GetJSON(ctx, s.DB, memberKey(serverID, username), &m)
	return m, found, err
}

// IsMember is the cheap membership check.
func (s *Store) IsMember(ctx context.Context, serverID, username string) (bool, error) {
	_, found, err := s.GetMember(ctx, serverID, username)
	return found, err
}

// UserServers lists every server a user belongs to — one prefix List over
// the reverse index.
func (s *Store) UserServers(ctx context.Context, username string) ([]Membership, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil, nil
	}
	var out []Membership
	err := kvx.ScanPrefix(ctx, s.DB, userMembersPrefix+username+"/", func(key string, v []byte) error {
		var m Member
		if json.Unmarshal(v, &m) != nil {
			return nil
		}
		out = append(out, Membership{ServerID: key[strings.LastIndex(key, "/")+1:], Member: m})
		return nil
	})
	return out, err
}

// ServerInfo is a server plus the asking user's membership — what the
// servers rail renders.
type ServerInfo struct {
	Server
	Member
}

// UserServerInfos resolves UserServers to full server records, name-sorted.
// Rows whose server record has vanished are skipped.
func (s *Store) UserServerInfos(ctx context.Context, username string) ([]ServerInfo, error) {
	memberships, err := s.UserServers(ctx, username)
	if err != nil {
		return nil, err
	}
	out := make([]ServerInfo, 0, len(memberships))
	for _, m := range memberships {
		srv, found, err := s.GetServer(ctx, m.ServerID)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, ServerInfo{Server: srv, Member: m.Member})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// Join adds username to a server as an @everyone member, both index
// directions in one OCC transaction. Open servers admit anyone; invite-only
// servers are joined through RedeemInvite (which calls this after checking
// the code). A banned member can't rejoin. Idempotent: re-joining keeps the
// existing membership.
func (s *Store) Join(ctx context.Context, serverID, username string) error {
	username = strings.ToLower(username)
	if !kvx.ValidID(serverID) || users.ValidUsername(username) != nil {
		return ErrNotFound
	}
	if ok, err := s.userExists(ctx, username); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("no account named %q", username)
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var srv Server
		if !getJSONTx(ctx, tx, serverKey(serverID), &srv) {
			return ErrNotFound
		}
		// Existing membership wins (idempotent), unless banned.
		var existing Member
		if getJSONTx(ctx, tx, memberKey(serverID, username), &existing) {
			if existing.Banned {
				return ErrAccessDenied
			}
			return nil
		}
		everyone, err := s.everyoneRoleID(ctx, serverID)
		if err != nil {
			return err
		}
		m := Member{RoleIDs: []string{everyone}, JoinedAt: time.Now().UTC()}
		setJSONTx(tx, memberKey(serverID, username), m)
		setJSONTx(tx, userMemberKey(username, serverID), m)
		return nil
	})
}

// Leave drops a member from a server (both directions). The owner can't
// leave — they delete or transfer the server instead.
func (s *Store) Leave(ctx context.Context, serverID, username string) error {
	username = strings.ToLower(username)
	if !kvx.ValidID(serverID) || users.ValidUsername(username) != nil {
		return ErrNotFound
	}
	srv, found, err := s.GetServer(ctx, serverID)
	if err != nil || !found {
		return err
	}
	if srv.Owner == username {
		return fmt.Errorf("the owner can't leave — transfer or delete the server")
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		tx.Delete(memberKey(serverID, username))
		tx.Delete(userMemberKey(username, serverID))
		return nil
	})
}

// Kick removes a member; Ban removes and marks them so they can't rejoin.
// Both are caller-gated on PermKickMembers / PermBanMembers and refuse the
// owner.
func (s *Store) Kick(ctx context.Context, serverID, username string) error {
	return s.removeMember(ctx, serverID, username, false)
}

// Ban removes a member and leaves a tombstone membership (Banned=true) on
// the server side so a rejoin is refused; the reverse index is cleared so
// the server vanishes from their rail.
func (s *Store) Ban(ctx context.Context, serverID, username string) error {
	return s.removeMember(ctx, serverID, username, true)
}

// Unban clears a ban tombstone.
func (s *Store) Unban(ctx context.Context, serverID, username string) error {
	username = strings.ToLower(username)
	if !kvx.ValidID(serverID) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var m Member
		if !getJSONTx(ctx, tx, memberKey(serverID, username), &m) {
			return nil
		}
		if m.Banned {
			tx.Delete(memberKey(serverID, username))
		}
		return nil
	})
}

func (s *Store) removeMember(ctx context.Context, serverID, username string, ban bool) error {
	username = strings.ToLower(username)
	if !kvx.ValidID(serverID) || users.ValidUsername(username) != nil {
		return ErrNotFound
	}
	srv, found, err := s.GetServer(ctx, serverID)
	if err != nil || !found {
		return err
	}
	if srv.Owner == username {
		return fmt.Errorf("the owner can't be removed")
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		tx.Delete(userMemberKey(username, serverID))
		if ban {
			setJSONTx(tx, memberKey(serverID, username), Member{Banned: true, JoinedAt: time.Now().UTC()})
		} else {
			tx.Delete(memberKey(serverID, username))
		}
		return nil
	})
}

// SetMemberRoles replaces a member's role set (caller gates on
// PermManageRoles). Unknown role ids are dropped; @everyone is always
// retained.
func (s *Store) SetMemberRoles(ctx context.Context, serverID, username string, roleIDs []string) error {
	roles, err := s.Roles(ctx, serverID)
	if err != nil {
		return err
	}
	valid := map[string]bool{}
	everyone := ""
	for _, r := range roles {
		valid[r.ID] = true
		if r.Everyone {
			everyone = r.ID
		}
	}
	keep := make([]string, 0, len(roleIDs)+1)
	if everyone != "" {
		keep = append(keep, everyone)
	}
	for _, id := range roleIDs {
		if valid[id] && id != everyone && !containsStr(keep, id) {
			keep = append(keep, id)
		}
	}
	return s.setMemberRoles(ctx, serverID, username, keep)
}

// setMemberRoles writes a member's role set to both index directions.
func (s *Store) setMemberRoles(ctx context.Context, serverID, username string, roleIDs []string) error {
	username = strings.ToLower(username)
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var m Member
		if !getJSONTx(ctx, tx, memberKey(serverID, username), &m) {
			return ErrNotFound
		}
		m.RoleIDs = roleIDs
		setJSONTx(tx, memberKey(serverID, username), m)
		setJSONTx(tx, userMemberKey(username, serverID), m)
		return nil
	})
}

// --- permission engine -----------------------------------------------------

// EffectivePerms resolves a user's permission set within a server: PermAll
// for the owner and for any PCP admin (admin override, §11), otherwise the
// union of their roles' Perms. A non-member gets 0. found reports whether
// the user is a member (admins may act without being members).
func (s *Store) EffectivePerms(ctx context.Context, serverID string, user users.User) (Perm, bool, error) {
	srv, found, err := s.GetServer(ctx, serverID)
	if err != nil || !found {
		return 0, false, err
	}
	uname := strings.ToLower(user.Username)
	if srv.Owner == uname || user.IsAdmin {
		return PermAll, srv.Owner == uname, nil
	}
	m, member, err := s.GetMember(ctx, serverID, uname)
	if err != nil || !member || m.Banned {
		return 0, false, err
	}
	roles, err := s.Roles(ctx, serverID)
	if err != nil {
		return 0, member, err
	}
	byID := make(map[string]Role, len(roles))
	for _, r := range roles {
		byID[r.ID] = r
	}
	var set Perm
	for _, id := range m.RoleIDs {
		set |= byID[id].Perms
	}
	return set, true, nil
}

// Can is the boolean gate: does user hold perm p in this server?
func (s *Store) Can(ctx context.Context, serverID string, user users.User, p Perm) (bool, error) {
	set, _, err := s.EffectivePerms(ctx, serverID, user)
	if err != nil {
		return false, err
	}
	return set.Has(p), nil
}

// CanViewChannel reports whether user may see a channel: public channels
// need only membership/view; private channels additionally require one of
// the channel's AllowRoles (owner and admins bypass via PermAll).
func (s *Store) CanViewChannel(ctx context.Context, user users.User, ch Channel) (bool, error) {
	set, member, err := s.EffectivePerms(ctx, ch.ServerID, user)
	if err != nil {
		return false, err
	}
	if set == PermAll { // owner or admin
		return true, nil
	}
	if !member || !set.Has(PermViewChannel) {
		return false, nil
	}
	if !ch.Private {
		return true, nil
	}
	m, _, err := s.GetMember(ctx, ch.ServerID, strings.ToLower(user.Username))
	if err != nil {
		return false, err
	}
	for _, want := range ch.AllowRoles {
		if containsStr(m.RoleIDs, want) {
			return true, nil
		}
	}
	return false, nil
}

// everyoneRoleID returns a server's @everyone role id.
func (s *Store) everyoneRoleID(ctx context.Context, serverID string) (string, error) {
	roles, err := s.Roles(ctx, serverID)
	if err != nil {
		return "", err
	}
	for _, r := range roles {
		if r.Everyone {
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("server %s has no @everyone role", serverID)
}

// userExists reports whether an account exists (guards phantom members).
func (s *Store) userExists(ctx context.Context, username string) (bool, error) {
	if s.Users == nil {
		return true, nil
	}
	_, found, err := s.Users.Get(ctx, username)
	return found, err
}
