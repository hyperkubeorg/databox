// authsvc.go is the server side of the identity system (§7): login,
// token validation, authorization checks, and user/grant/key
// management. Storage is the metadata group; hashing rules live in
// pkg/auth.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/cluster"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/kv"
)

// ErrUnauthorized is returned for bad credentials or missing permissions.
var ErrUnauthorized = errors.New("Unauthorized")

// Login verifies credentials and mints a session token (§7.1). The rule
// for passwordless accounts: an empty stored hash accepts only an empty
// password — which is exactly the fresh-root situation, closed the moment
// a password is set.
func (s *Server) Login(ctx context.Context, username, password string) (token string, expires time.Time, err error) {
	u, ok, err := s.getUser(username)
	if err != nil {
		return "", time.Time{}, err
	}
	if !ok {
		return "", time.Time{}, ErrUnauthorized
	}
	if u.PasswordHash == "" {
		if password != "" {
			return "", time.Time{}, ErrUnauthorized
		}
	} else if !auth.VerifyPassword(password, u.PasswordHash) {
		return "", time.Time{}, ErrUnauthorized
	}
	token = auth.RandomToken(32)
	expires = time.Now().UTC().Add(s.Cfg.TokenTTL)
	rec := auth.Token{User: username, ExpiresAt: expires}
	res, perr := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "set", Key: auth.KeyPrefixTokens + token, Value: rec.Encode()})
	if err := firstErr(perr, res); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// Authenticate resolves a bearer token to its user, enforcing expiry.
// Token reads are local (metadata replicates everywhere), so this is on
// the hot path of every request without a network hop.
func (s *Server) Authenticate(token string) (auth.User, error) {
	if token == "" {
		return auth.User{}, ErrUnauthorized
	}
	rec, ok, err := (*fabric)(s).MetaGet(auth.KeyPrefixTokens + token)
	if err != nil {
		return auth.User{}, err
	}
	if !ok {
		return auth.User{}, ErrUnauthorized
	}
	t, err := auth.DecodeToken(rec.Value)
	if err != nil || t.Expired(time.Now()) {
		return auth.User{}, ErrUnauthorized
	}
	u, ok, err := s.getUser(t.User)
	if err != nil || !ok {
		return auth.User{}, ErrUnauthorized
	}
	return u, nil
}

// TokenRevoke deletes a session token server-side (§7.1 "revocable"):
// the token record is removed from the metadata keyspace, so every node
// rejects it as soon as the deletion replicates. Revoking an unknown or
// already-expired token is a no-op success — logout is idempotent.
func (s *Server) TokenRevoke(ctx context.Context, token string) error {
	if token == "" {
		return ErrUnauthorized
	}
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "delete", Key: auth.KeyPrefixTokens + token})
	return firstErr(err, res)
}

// Authorize checks one (key, verb) against a user's grants. Root bypasses
// everything (§7.1); everyone else goes through the §7.2 resolution.
func (s *Server) Authorize(u auth.User, key string, verb auth.Verb) error {
	if u.Name == auth.RootUser {
		return nil
	}
	if auth.Allowed(u.Grants, key, verb) {
		return nil
	}
	return fmt.Errorf("%w: user %q lacks %q on %q", ErrUnauthorized, u.Name, verb, key)
}

// AuthorizeAdmin gates cluster-management operations: root, or any grant
// of the "admin" verb anywhere.
func (s *Server) AuthorizeAdmin(u auth.User) error {
	if u.Name == auth.RootUser {
		return nil
	}
	for _, g := range u.Grants {
		if g.Effect == "allow" {
			for _, v := range g.Verbs {
				if v == auth.VerbAdmin {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("%w: admin access required", ErrUnauthorized)
}

// getUser loads a user record from local metadata.
func (s *Server) getUser(name string) (auth.User, bool, error) {
	rec, ok, err := (*fabric)(s).MetaGet(auth.KeyPrefixUsers + name)
	if err != nil || !ok {
		return auth.User{}, false, err
	}
	u, err := auth.DecodeUser(rec.Value)
	if err != nil {
		return auth.User{}, false, err
	}
	return u, true, nil
}

// putUser writes a user record through raft and audits the mutation.
func (s *Server) putUser(ctx context.Context, actor string, u auth.User, action string) error {
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "set", Key: auth.KeyPrefixUsers + u.Name, Value: u.Encode()})
	if err := firstErr(err, res); err != nil {
		return err
	}
	s.audit(ctx, actor, action, "user="+u.Name)
	return nil
}

// UserCreate creates a user with an optional initial password.
func (s *Server) UserCreate(ctx context.Context, actor, name, password string) error {
	if name == "" {
		return fmt.Errorf("user name required")
	}
	if _, exists, _ := s.getUser(name); exists {
		return fmt.Errorf("user %q already exists", name)
	}
	u := auth.User{Name: name, CreatedAt: time.Now().UTC()}
	if password != "" {
		hash, err := auth.HashPassword(password)
		if err != nil {
			return err
		}
		u.PasswordHash = hash
	}
	return s.putUser(ctx, actor, u, "user-create")
}

// UserSetPassword replaces a user's password hash (argon2id-512, §7.1).
func (s *Server) UserSetPassword(ctx context.Context, actor, name, password string) error {
	u, ok, err := s.getUser(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("user %q not found", name)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	u.PasswordHash = hash
	return s.putUser(ctx, actor, u, "user-passwd")
}

// UserDelete removes a user (root is indestructible).
func (s *Server) UserDelete(ctx context.Context, actor, name string) error {
	if name == auth.RootUser {
		return fmt.Errorf("the root user cannot be deleted")
	}
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "delete", Key: auth.KeyPrefixUsers + name})
	if err := firstErr(err, res); err != nil {
		return err
	}
	s.audit(ctx, actor, "user-delete", "user="+name)
	return nil
}

// UserGet returns one user with the password hash redacted — the
// GUI/API-facing read (internal callers use getUser).
func (s *Server) UserGet(name string) (auth.User, bool, error) {
	u, ok, err := s.getUser(name)
	if err != nil || !ok {
		return auth.User{}, false, err
	}
	u.PasswordHash = ""
	return u, true, nil
}

// UserListPage pages through users, optionally filtered by a name prefix
// (the GUI's search box). Built for directories with thousands of users:
// one metadata page per call, cursor = resume strictly after that name.
func (s *Server) UserListPage(nameFilter, cursor string, limit int) (users []auth.User, next string, err error) {
	h := s.meta()
	if h == nil {
		return nil, "", fmt.Errorf("metadata group not started")
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	cur := ""
	if cursor != "" {
		cur = auth.KeyPrefixUsers + cursor
	}
	entries, err := h.sm.List(auth.KeyPrefixUsers+nameFilter, cur, limit)
	if err != nil {
		return nil, "", err
	}
	for _, e := range entries {
		u, err := auth.DecodeUser(e.Record.Value)
		if err != nil {
			continue
		}
		u.PasswordHash = "" // redact
		users = append(users, u)
	}
	if len(entries) == limit {
		next = users[len(users)-1].Name
	}
	return users, next, nil
}

// UserList returns all users with password hashes redacted — hashes never
// leave the storage layer.
func (s *Server) UserList() ([]auth.User, error) {
	entries, err := (*fabric)(s).MetaList(auth.KeyPrefixUsers, 10000)
	if err != nil {
		return nil, err
	}
	out := make([]auth.User, 0, len(entries))
	for _, e := range entries {
		u, err := auth.DecodeUser(e.Record.Value)
		if err != nil {
			continue
		}
		u.PasswordHash = "" // redact
		out = append(out, u)
	}
	return out, nil
}

// GrantAdd attaches a grant rule to a user (§7.2).
func (s *Server) GrantAdd(ctx context.Context, actor, user string, g auth.Grant) error {
	if g.Effect != "allow" && g.Effect != "deny" {
		return fmt.Errorf(`grant effect must be "allow" or "deny"`)
	}
	for _, v := range g.Verbs {
		if !auth.ValidVerb(string(v)) {
			return fmt.Errorf("unknown verb %q", v)
		}
	}
	u, ok, err := s.getUser(user)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("user %q not found", user)
	}
	u.Grants = append(u.Grants, g)
	return s.putUser(ctx, actor, u, "grant-add")
}

// GrantRemove removes grants matching (prefix, effect) from a user.
func (s *Server) GrantRemove(ctx context.Context, actor, user, prefix, effect string) error {
	u, ok, err := s.getUser(user)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("user %q not found", user)
	}
	kept := u.Grants[:0]
	removed := 0
	for _, g := range u.Grants {
		if g.Prefix == prefix && (effect == "" || g.Effect == effect) {
			removed++
			continue
		}
		kept = append(kept, g)
	}
	if removed == 0 {
		return fmt.Errorf("no grant with prefix %q on user %q", prefix, user)
	}
	u.Grants = kept
	return s.putUser(ctx, actor, u, "grant-remove")
}

// AccessKeyCreate mints a Databox API key for a user (§7.1) — the
// credential gateways authenticate with. scopes optionally narrows the
// key to the given prefixes (empty = the user's full grant extent). The
// secret is returned exactly once, here.
func (s *Server) AccessKeyCreate(ctx context.Context, actor, user string, scopes []string) (auth.AccessKey, error) {
	if _, ok, err := s.getUser(user); err != nil || !ok {
		return auth.AccessKey{}, fmt.Errorf("user %q not found", user)
	}
	// Normalize: drop empties, require prefixes to name user keyspace.
	var clean []string
	for _, p := range scopes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			return auth.AccessKey{}, fmt.Errorf("scope %q must start with / (user keyspace)", p)
		}
		clean = append(clean, p)
	}
	key := auth.AccessKey{
		KeyID:     "DBX" + auth.RandomToken(12),
		Secret:    auth.RandomToken(30),
		User:      user,
		CreatedAt: time.Now().UTC(),
		Scopes:    clean,
	}
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "set", Key: auth.KeyPrefixAccessKeys + key.KeyID, Value: key.Encode()})
	if err := firstErr(err, res); err != nil {
		return auth.AccessKey{}, err
	}
	s.audit(ctx, actor, "access-key-create", "user="+user+" key="+key.KeyID)
	return key, nil
}

// AccessKeyList returns a user's access keys with the secrets REDACTED —
// secrets are shown exactly once, at mint time, and never again (§7.1).
func (s *Server) AccessKeyList(user string) ([]auth.AccessKey, error) {
	entries, err := (*fabric)(s).MetaList(auth.KeyPrefixAccessKeys, 10000)
	if err != nil {
		return nil, err
	}
	var out []auth.AccessKey
	for _, e := range entries {
		k, err := auth.DecodeAccessKey(e.Record.Value)
		if err != nil || k.User != user {
			continue
		}
		k.Secret = "" // redact: list responses never carry secrets
		out = append(out, k)
	}
	return out, nil
}

// AccessKeyDelete revokes one access key. The owner check lives here so
// every surface (GUI, API) enforces it identically: a user may revoke
// only their own keys; admins pass any owner.
func (s *Server) AccessKeyDelete(ctx context.Context, actor, owner, keyID string) error {
	k, found, err := s.AccessKeyLookup(keyID)
	if err != nil {
		return err
	}
	if !found || k.User != owner {
		return fmt.Errorf("access key %q not found for user %q", keyID, owner)
	}
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "delete", Key: auth.KeyPrefixAccessKeys + keyID})
	if err := firstErr(err, res); err != nil {
		return err
	}
	s.audit(ctx, actor, "access-key-delete", "user="+owner+" key="+keyID)
	return nil
}

// AccessKeyLookup resolves a key ID for SigV4 verification (pkg/service/s3).
func (s *Server) AccessKeyLookup(keyID string) (auth.AccessKey, bool, error) {
	rec, ok, err := (*fabric)(s).MetaGet(auth.KeyPrefixAccessKeys + keyID)
	if err != nil || !ok {
		return auth.AccessKey{}, false, err
	}
	k, err := auth.DecodeAccessKey(rec.Value)
	if err != nil {
		return auth.AccessKey{}, false, err
	}
	return k, true, nil
}

// SystemGet / SystemList expose the read-only `.databox/` view (§19):
// admin-gated raw access to metadata keys.
func (s *Server) SystemGet(key string) (kv.Record, bool, error) {
	return (*fabric)(s).MetaGet(key)
}

// SystemList scans metadata keys for the `.databox/` view.
func (s *Server) SystemList(prefix string, limit int) ([]kv.ListEntry, error) {
	return (*fabric)(s).MetaList(prefix, limit)
}

// SystemListPage is the cursored form of SystemList, letting the GUI's
// KV explorer page through the metadata keyspace with the same range
// semantics as user keys (cursor = resume strictly after this key).
func (s *Server) SystemListPage(prefix, cursor string, limit int) ([]kv.ListEntry, string, error) {
	h := s.meta()
	if h == nil {
		return nil, "", fmt.Errorf("metadata group not started")
	}
	entries, err := h.sm.List(prefix, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(entries) == limit {
		next = entries[len(entries)-1].Key
	}
	return entries, next, nil
}

// ImpersonateToken mints a session token FOR another user without their
// password — the admin "act as" facility (§7.3 management surfaces). The
// caller must already be admin-authorized; the impersonation is loudly
// audited with both identities so the trail shows who really acted.
func (s *Server) ImpersonateToken(ctx context.Context, actor, target string) (string, time.Time, error) {
	if _, ok, err := s.getUser(target); err != nil {
		return "", time.Time{}, err
	} else if !ok {
		return "", time.Time{}, fmt.Errorf("user %q not found", target)
	}
	token := auth.RandomToken(32)
	expires := time.Now().UTC().Add(s.Cfg.TokenTTL)
	rec := auth.Token{User: target, ExpiresAt: expires}
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "set", Key: auth.KeyPrefixTokens + token, Value: rec.Encode()})
	if err := firstErr(err, res); err != nil {
		return "", time.Time{}, err
	}
	s.audit(ctx, actor, "impersonate", "target="+target)
	return token, expires, nil
}

// Placement describes where one key physically lives — the admin
// inspection view behind the KV explorer's "details" panel.
type Placement struct {
	Shard   cluster.Shard     `json:"shard"`
	Group   cluster.GroupInfo `json:"group"`
	Leader  uint64            `json:"leader"` // 0 = unknown from this node
	Members []PlacementMember `json:"members"`
}

// PlacementMember is one node hosting the key's raft group.
type PlacementMember struct {
	ID      uint64 `json:"id"`
	Name    string `json:"name"`
	Addr    string `json:"addr"`
	Healthy bool   `json:"healthy"`
	Leader  bool   `json:"leader"`
}

// NodeDirectory maps node IDs to display names — used by the GUI to
// label chunk placement without repeating the lookup per chunk.
func (s *Server) NodeDirectory() map[uint64]string {
	out := map[uint64]string{}
	nodes, err := cluster.Nodes((*fabric)(s))
	if err != nil {
		return out
	}
	for _, n := range nodes {
		out[n.ID] = n.Name
	}
	return out
}

// KeyPlacement resolves the shard, raft group, and member nodes covering
// a user key.
func (s *Server) KeyPlacement(key string) (*Placement, error) {
	f := (*fabric)(s)
	sh, err := cluster.ShardFor(f, key)
	if err != nil {
		return nil, err
	}
	p := &Placement{Shard: sh}
	groups, err := cluster.Groups(f)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.GID == sh.GID {
			p.Group = g
		}
	}
	// Leader is known locally when this node hosts the group; otherwise
	// the stats report (published by the leader itself) names it.
	if h, ok := s.handle(sh.GID); ok {
		p.Leader = h.group.LeaderID()
	} else if rec, ok, _ := f.MetaGet(cluster.KeyStats + fmt.Sprintf("%d", sh.GID)); ok {
		var st cluster.GroupStats
		if json.Unmarshal(rec.Value, &st) == nil {
			p.Leader = st.Leader
		}
	}
	nodes, err := cluster.Nodes(f)
	if err != nil {
		return nil, err
	}
	byID := map[uint64]cluster.Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	for _, m := range p.Group.Members {
		n := byID[m]
		p.Members = append(p.Members, PlacementMember{
			ID: m, Name: n.Name, Addr: n.Addr,
			Healthy: n.State == "active" && n.Live,
			Leader:  m == p.Leader,
		})
	}
	return p, nil
}

// Audit exposes audit logging to route handlers.
func (s *Server) Audit(ctx context.Context, actor, action, detail string) {
	s.audit(ctx, actor, action, detail)
}

// NodeID exposes this node's identity to routes and services.
func (s *Server) NodeID() uint64 { return s.nodeID }

// ClusterID exposes the cluster identity.
func (s *Server) ClusterID() string { return s.clusterID }
