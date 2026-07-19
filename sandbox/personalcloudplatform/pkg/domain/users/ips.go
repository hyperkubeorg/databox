// ips.go — the network-address side of moderation, ported from PCD:
// which IPs each member has connected from, and IP bans.
//
// Keys (kvx key table):
//
//	/pcp/userips/<username>/<ip> → UserIP (first/last seen + login count)
//	/pcp/ipbans/<ip>             → IPBan — refused at login AND signup,
//	                               so a banned member can't just register
//	                               a new account from the same address
//
// IPs come from RemoteAddr (or X-Forwarded-For behind a trusted proxy —
// client-controlled otherwise!), so EVERY method validates through
// net.ParseIP before an address becomes part of a key; the canonical
// .String() form is stored, so "::1" and "0:0:0:0:0:0:0:1" land on one
// record.
package users

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Key prefixes this file owns (kvx key table).
const (
	userIPsPrefix = "/pcp/userips/"
	ipBansPrefix  = "/pcp/ipbans/"
)

// keyIP validates and canonicalizes an address for use as a key segment
// ("" = not an IP — never store it).
func keyIP(ip string) string {
	p := net.ParseIP(strings.TrimSpace(ip))
	if p == nil {
		return ""
	}
	return p.String()
}

// ErrProtectedIP refuses banning an address that would lock out the
// site itself. The message is admin-facing.
var ErrProtectedIP = errors.New("that address is localhost — banning it would lock out everyone connecting through it")

// ProtectedIP reports whether an address may NEVER be banned: loopback
// and unspecified. Behind a local reverse proxy without
// TRUST_PROXY_HEADERS every visitor LOOKS like 127.0.0.1 — one "ban the
// user and all their IPs" click would ban the whole site, admins
// included. Belt and braces: BanIP refuses these, BanUserIPs skips
// them, and IPBanned ignores even a hand-planted record for one.
func ProtectedIP(ip string) bool {
	p := net.ParseIP(strings.TrimSpace(ip))
	return p != nil && (p.IsLoopback() || p.IsUnspecified())
}

// UserIP is one address a member has connected from.
type UserIP struct {
	IP        string    `json:"ip"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Logins    int       `json:"logins"` // sign-ins (and the signup) from this address
}

// RecordUserIP notes that username connected from ip just now; isLogin
// marks an actual sign-in (or signup). Plain read-modify-write, no
// transaction: two racing touches can lose a LastSeen tick — worthless
// to defend against. A non-IP is silently dropped, never a key.
func (s *Store) RecordUserIP(ctx context.Context, username, ip string, isLogin bool) error {
	username = strings.ToLower(username)
	if ValidUsername(username) != nil {
		return nil
	}
	addr := keyIP(ip)
	if addr == "" {
		return nil
	}
	key := userIPsPrefix + username + "/" + addr
	now := time.Now().UTC()
	rec := UserIP{IP: addr, FirstSeen: now}
	_, _ = kvx.GetJSON(ctx, s.DB, key, &rec)
	if rec.FirstSeen.IsZero() {
		rec.FirstSeen = now
	}
	rec.IP = addr
	rec.LastSeen = now
	if isLogin {
		rec.Logins++
	}
	return kvx.SetJSON(ctx, s.DB, key, rec)
}

// UserIPs lists every address a member has connected from, most
// recently seen first (admin user detail).
func (s *Store) UserIPs(ctx context.Context, username string) ([]UserIP, error) {
	username = strings.ToLower(username)
	if ValidUsername(username) != nil {
		return nil, nil
	}
	var out []UserIP
	err := kvx.ScanPrefix(ctx, s.DB, userIPsPrefix+username+"/", func(_ string, value []byte) error {
		var rec UserIP
		if json.Unmarshal(value, &rec) == nil {
			out = append(out, rec)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out, nil
}

// IPBan is one banned address. User names the member whose ban fanned
// this out ("" for a hand-placed single-IP ban) — unbanning that member
// lifts exactly these, never a ban another member earned the address.
type IPBan struct {
	IP   string    `json:"ip"`
	User string    `json:"user,omitempty"`
	By   string    `json:"by"`
	At   time.Time `json:"at"`
}

// BanIP bans one address. sourceUser attributes the ban to a member's
// user+IPs ban; pass "" for a standalone ban. Protected addresses are
// refused outright.
func (s *Store) BanIP(ctx context.Context, ip, sourceUser, by string) error {
	addr := keyIP(ip)
	if addr == "" {
		return ErrNotFound
	}
	if ProtectedIP(addr) {
		return ErrProtectedIP
	}
	return kvx.SetJSON(ctx, s.DB, ipBansPrefix+addr, IPBan{
		IP: addr, User: strings.ToLower(sourceUser), By: strings.ToLower(by), At: time.Now().UTC(),
	})
}

// UnbanIP lifts one address ban (idempotent).
func (s *Store) UnbanIP(ctx context.Context, ip string) error {
	addr := keyIP(ip)
	if addr == "" {
		return nil
	}
	return s.DB.Delete(ctx, ipBansPrefix+addr)
}

// GetIPBan loads one address's ban, if any.
func (s *Store) GetIPBan(ctx context.Context, ip string) (IPBan, bool, error) {
	addr := keyIP(ip)
	if addr == "" {
		return IPBan{}, false, nil
	}
	var ban IPBan
	found, err := kvx.GetJSON(ctx, s.DB, ipBansPrefix+addr, &ban)
	return ban, found, err
}

// IPBanned reports whether an address is banned. Non-IPs are never
// banned, and PROTECTED addresses read as never banned even if a record
// was somehow planted — localhost locking itself out is the one failure
// this feature must never produce.
func (s *Store) IPBanned(ctx context.Context, ip string) (bool, error) {
	if ProtectedIP(ip) {
		return false, nil
	}
	_, found, err := s.GetIPBan(ctx, ip)
	return found, err
}

// BanUserIPs bans every address the member has ever connected from,
// attributing each ban to them. Protected addresses are silently
// skipped, never an error. Returns how many were banned.
func (s *Store) BanUserIPs(ctx context.Context, username, by string) (int, error) {
	ips, err := s.UserIPs(ctx, username)
	if err != nil {
		return 0, err
	}
	banned := 0
	for _, rec := range ips {
		if ProtectedIP(rec.IP) {
			continue
		}
		if err := s.BanIP(ctx, rec.IP, username, by); err != nil {
			return banned, err
		}
		banned++
	}
	return banned, nil
}

// UnbanUserIPs lifts the IP bans attributed to username — the exact set
// a user+IPs ban fanned out, and nothing another member's ban earned a
// shared address. Returns how many were lifted.
func (s *Store) UnbanUserIPs(ctx context.Context, username string) (int, error) {
	username = strings.ToLower(username)
	ips, err := s.UserIPs(ctx, username)
	if err != nil {
		return 0, err
	}
	lifted := 0
	for _, rec := range ips {
		ban, found, err := s.GetIPBan(ctx, rec.IP)
		if err != nil {
			return lifted, err
		}
		if !found || ban.User != username {
			continue
		}
		if err := s.UnbanIP(ctx, rec.IP); err != nil {
			return lifted, err
		}
		lifted++
	}
	return lifted, nil
}

// ScanIPBans walks every banned address (admin console — the set stays
// small at this app's scale).
func (s *Store) ScanIPBans(ctx context.Context, fn func(IPBan)) error {
	return kvx.ScanPrefix(ctx, s.DB, ipBansPrefix, func(_ string, value []byte) error {
		var b IPBan
		if json.Unmarshal(value, &b) == nil {
			fn(b)
		}
		return nil
	})
}
