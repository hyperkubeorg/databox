// invites.go — server invite codes (Messenger §8). An invite is a
// short code with an optional expiry and use limit; redeeming it joins the
// server. Codes can be posted into any conversation as an embed (the message
// carries the code; the UI renders a Join card). Minting is gated on
// PermCreateInvite by the caller.
package messenger

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Invite is a redeemable server invite.
type Invite struct {
	Code      string    `json:"code"`
	ServerID  string    `json:"server_id"`
	By        string    `json:"by"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"` // zero = never
	MaxUses   int       `json:"max_uses,omitempty"`   // 0 = unlimited
	Uses      int       `json:"uses"`
}

func inviteKey(code string) string { return invitesPrefix + code }
func serverInviteKey(serverID, code string) string {
	return serverInvitesPrefix + serverID + "/" + code
}

// Expired reports whether an invite is no longer redeemable.
func (i Invite) Expired(now time.Time) bool {
	if !i.ExpiresAt.IsZero() && now.After(i.ExpiresAt) {
		return true
	}
	return i.MaxUses > 0 && i.Uses >= i.MaxUses
}

// CreateInvite mints an invite for a server (caller gates on
// PermCreateInvite). ttl<=0 means no expiry; maxUses<=0 means unlimited.
func (s *Store) CreateInvite(ctx context.Context, serverID, by string, ttl time.Duration, maxUses int) (Invite, error) {
	if !kvx.ValidID(serverID) {
		return Invite{}, ErrNotFound
	}
	if _, found, err := s.GetServer(ctx, serverID); err != nil || !found {
		return Invite{}, ErrNotFound
	}
	inv := Invite{
		Code:      auth.RandomToken(8),
		ServerID:  serverID,
		By:        strings.ToLower(by),
		CreatedAt: time.Now().UTC(),
		MaxUses:   maxUses,
	}
	if ttl > 0 {
		inv.ExpiresAt = inv.CreatedAt.Add(ttl)
	}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		setJSONTx(tx, inviteKey(inv.Code), inv)
		tx.Set(serverInviteKey(serverID, inv.Code), []byte("{}"))
		return nil
	})
	if err != nil {
		return Invite{}, err
	}
	return inv, nil
}

// GetInvite loads an invite by code.
func (s *Store) GetInvite(ctx context.Context, code string) (Invite, bool, error) {
	if !kvx.ValidTokenChars(code) || len(code) < 4 || len(code) > 32 {
		return Invite{}, false, nil
	}
	var inv Invite
	found, err := kvx.GetJSON(ctx, s.DB, inviteKey(code), &inv)
	return inv, found, err
}

// RedeemInvite validates a code and joins the user to its server,
// incrementing the use count in the same transaction (OCC picks a winner on
// the last seat). Returns the joined server.
func (s *Store) RedeemInvite(ctx context.Context, code, user string) (Server, error) {
	user = strings.ToLower(user)
	inv, found, err := s.GetInvite(ctx, code)
	if err != nil || !found {
		return Server{}, fmt.Errorf("that invite is invalid")
	}
	if inv.Expired(time.Now()) {
		return Server{}, fmt.Errorf("that invite has expired")
	}
	srv, found, err := s.GetServer(ctx, inv.ServerID)
	if err != nil || !found {
		return Server{}, fmt.Errorf("that invite is invalid")
	}
	// Already a member? Redeeming is a no-op success (jump straight in).
	if m, member, _ := s.GetMember(ctx, inv.ServerID, user); member && !m.Banned {
		return srv, nil
	}
	if err := s.Join(ctx, inv.ServerID, user); err != nil {
		return Server{}, err
	}
	// Bump the use count (best-effort; the join already succeeded).
	_ = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var cur Invite
		if !getJSONTx(ctx, tx, inviteKey(code), &cur) {
			return nil
		}
		cur.Uses++
		setJSONTx(tx, inviteKey(code), cur)
		return nil
	})
	return srv, nil
}

// RevokeInvite deletes an invite (both key directions; caller gates on
// PermManageServer). Revoking an unknown code is a no-op.
func (s *Store) RevokeInvite(ctx context.Context, serverID, code string) error {
	if !kvx.ValidID(serverID) || !kvx.ValidTokenChars(code) || len(code) < 4 || len(code) > 32 {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var inv Invite
		if getJSONTx(ctx, tx, inviteKey(code), &inv) && inv.ServerID != serverID {
			return ErrNotFound // a code minted for another server
		}
		tx.Delete(inviteKey(code))
		tx.Delete(serverInviteKey(serverID, code))
		return nil
	})
}

// ServerInvites lists a server's active invite codes (caller gates).
func (s *Store) ServerInvites(ctx context.Context, serverID string) ([]Invite, error) {
	var out []Invite
	now := time.Now()
	err := kvx.ScanPrefix(ctx, s.DB, serverInvitesPrefix+serverID+"/", func(key string, _ []byte) error {
		code := key[strings.LastIndex(key, "/")+1:]
		if inv, found, _ := s.GetInvite(ctx, code); found && !inv.Expired(now) {
			out = append(out, inv)
		}
		return nil
	})
	return out, err
}
