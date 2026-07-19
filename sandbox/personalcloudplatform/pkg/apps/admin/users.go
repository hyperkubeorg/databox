// users.go — People: the user directory, the per-user detail page
// (profile, drives, sessions with revoke, connected-from IPs,
// capabilities, tier, quota override, email allowance, API keys with
// revoke per §12.1, ban/unban with optional IP fanout, promote/demote,
// impersonate, delete), and the impersonation stop. PCD parity in the
// §11.1 IA: the list and the detail are separate pages; every mutation
// audits.
package admin

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// UsersPage lists accounts.
type UsersPage struct {
	shell
	Users  []users.User
	Cursor string
	Query  string
}

func (h *handlers) usersPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := UsersPage{shell: h.shell(r, "Users", "users", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	list, next, err := h.k.Users.List(cctx, r.URL.Query().Get("cursor"), 100)
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	if q != "" {
		kept := list[:0]
		for _, u := range list {
			if strings.Contains(u.Username, q) || strings.Contains(strings.ToLower(u.DisplayName), q) {
				kept = append(kept, u)
			}
		}
		list = kept
	}
	pg.Users, pg.Cursor, pg.Query = list, next, q
	h.render(w, "admin_users", pg)
}

// UserDetailPage is one account's full admin view.
type UserDetailPage struct {
	shell
	U         users.User
	Quota     int64 // effective (0 = unlimited)
	Mailboxes int   // effective email-account allowance
	TOTPOn    bool  // 2FA state (the secret itself never reaches the page)
	Drives    []drives.Info
	Sessions  []users.UserSession
	IPs       []users.UserIP
	Keys      []apikeys.Key
	Tiers     []site.Tier
	Caps      []string // whole vocabulary, for checkboxes
	Addresses []mail.Address
}

func (h *handlers) userDetail(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	target := r.PathValue("user")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	u, found, err := h.k.Users.Get(cctx, target)
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	u.PasswordHash = ""
	totpOn := u.TOTPEnabled()
	u.TOTPSecret, u.TOTPPending, u.TOTPRecovery = "", "", nil
	pg := UserDetailPage{shell: h.shell(r, "@"+u.Username, "users", sess, user), U: u, TOTPOn: totpOn, Caps: users.KnownCaps}
	sc, _ := h.k.Site.Get(cctx)
	pg.Quota = site.QuotaFor(sc, u.QuotaOverride, u.Tier, h.k.DefaultQuota)
	pg.Mailboxes = mail.MailboxesFor(sc, u)
	pg.Tiers = sc.Tiers
	if ds, err := h.k.Drives.UserDriveInfos(cctx, u.Username); err == nil {
		pg.Drives = ds
	}
	if ss, err := h.k.Users.UserSessions(cctx, u.Username); err == nil {
		pg.Sessions = ss
	}
	if ips, err := h.k.Users.UserIPs(cctx, u.Username); err == nil {
		pg.IPs = ips
	}
	if keys, err := h.k.APIKeys.ListForUser(cctx, u.Username); err == nil {
		pg.Keys = keys
	}
	if addrs, err := h.k.Mail.UserAddresses(cctx, u.Username); err == nil {
		pg.Addresses = addrs
	}
	h.render(w, "admin_user_detail", pg)
}

// userTarget reads and validates the target account for a mutation,
// refusing self-footguns where asked.
func (h *handlers) userTarget(r *http.Request, actor users.User, allowSelf bool) (users.User, string, error) {
	target := strings.ToLower(r.FormValue("user"))
	back := "/admin/users/" + target
	if !allowSelf && target == actor.Username {
		return users.User{}, "/admin/users", fmt.Errorf("not on your own account")
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	u, found, err := h.k.Users.Get(cctx, target)
	if err != nil {
		return users.User{}, back, err
	}
	if !found {
		return users.User{}, "/admin/users", users.ErrNotFound
	}
	return u, back, nil
}

func (h *handlers) userBan(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	u, back, err := h.userTarget(r, user, false)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	on := r.FormValue("on") == "1"
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Users.SetBanned(cctx, u.Username, on); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	action, detail := "user.unban", ""
	if on {
		action = "user.ban"
		if _, err := h.k.Users.DeleteUserSessions(cctx, u.Username); err != nil {
			h.k.Log.Warn("session revoke on ban failed", "user", u.Username, "err", err)
		}
		if r.FormValue("ips") == "1" {
			n, err := h.k.Users.BanUserIPs(cctx, u.Username, user.Username)
			if err != nil {
				h.k.Log.Warn("ip ban fanout failed", "user", u.Username, "err", err)
			}
			detail = fmt.Sprintf("%d addresses banned too", n)
		}
	} else if r.FormValue("ips") == "1" {
		n, err := h.k.Users.UnbanUserIPs(cctx, u.Username)
		if err != nil {
			h.k.Log.Warn("ip unban failed", "user", u.Username, "err", err)
		}
		detail = fmt.Sprintf("%d addresses unbanned", n)
	}
	h.k.Audit(r, user, sess, action, u.Username, detail)
	h.k.Respond(w, r, back+"?ok=done", nil, nil)
}

func (h *handlers) userAdmin(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	u, back, err := h.userTarget(r, user, false)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	on := r.FormValue("on") == "1"
	action := "user.demote"
	if on {
		action = "user.promote"
	}
	h.mutate(w, r, sess, user, action, u.Username, back, "done", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Users.SetAdmin(cctx, u.Username, on)
	})
}

func (h *handlers) userCaps(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	u, back, err := h.userTarget(r, user, true)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	_ = r.ParseForm()
	caps := r.Form["cap"]
	h.mutate(w, r, sess, user, "user.caps", u.Username+" "+strings.Join(caps, ","), back, "capabilities+saved", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Users.SetCaps(cctx, u.Username, caps)
	})
}

func (h *handlers) userTier(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	u, back, err := h.userTarget(r, user, true)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	tier := r.FormValue("tier")
	h.mutate(w, r, sess, user, "user.tier", u.Username+" "+tier, back, "tier+set", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		if tier != "" {
			sc, _ := h.k.Site.Get(cctx)
			if _, ok := sc.TierBytes(tier); !ok {
				return fmt.Errorf("unknown tier %q", tier)
			}
		}
		return h.k.Users.SetTier(cctx, u.Username, tier)
	})
}

// parseBytesField reads a byte-count field: "" = 0 (unset), "unlimited"
// = QuotaUnlimited, otherwise integer bytes.
func parseBytesField(v string) (int64, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	switch v {
	case "":
		return 0, nil
	case "unlimited", "-1":
		return site.QuotaUnlimited, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("byte counts are plain integers (or \"unlimited\")")
	}
	return n, nil
}

func (h *handlers) userQuota(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	u, back, err := h.userTarget(r, user, true)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "user.quota", u.Username+" "+r.FormValue("bytes"), back, "quota+set", nil, func() error {
		bytes, err := parseBytesField(r.FormValue("bytes"))
		if err != nil {
			return err
		}
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Users.SetQuotaOverride(cctx, u.Username, bytes)
	})
}

// userMailboxes sets the per-user email-account allowance ("" = site
// default, "none" = explicitly zero, integer = that many).
func (h *handlers) userMailboxes(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	u, back, err := h.userTarget(r, user, true)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	v := strings.TrimSpace(strings.ToLower(r.FormValue("count")))
	h.mutate(w, r, sess, user, "user.mailboxes", u.Username+" "+v, back, "email+allowance+set", nil, func() error {
		override := 0
		switch v {
		case "":
		case "none", "0":
			override = mail.MailboxesNone
		default:
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || n > 100 {
				return fmt.Errorf("mailbox counts are 0–100 (blank = site default)")
			}
			override = n
		}
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Users.SetMailboxOverride(cctx, u.Username, override)
	})
}

func (h *handlers) userSessions(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	u, back, err := h.userTarget(r, user, true)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "user.sessions.revoke", u.Username, back, "signed+out+everywhere", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, err := h.k.Users.DeleteUserSessions(cctx, u.Username)
		return err
	})
}

// userTOTPReset clears a member's two-factor state — the "lost their
// phone AND their recovery codes" lever. No password proof here (the
// admin doesn't have it); the mutation is audited instead.
func (h *handlers) userTOTPReset(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	u, back, err := h.userTarget(r, user, true)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "user.totp.reset", u.Username, back, "two-factor+auth+reset", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Users.ResetTOTP(cctx, u.Username)
	})
}

// userAPIKeyRevoke kills one of the member's API keys (spec §12.1:
// admin oversight). Effective immediately — the next bearer request
// 401s.
func (h *handlers) userAPIKeyRevoke(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	u, back, err := h.userTarget(r, user, true)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	keyID := r.FormValue("key_id")
	h.mutate(w, r, sess, user, "user.apikey.revoke", u.Username+" "+keyID, back, "key+revoked", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.APIKeys.Revoke(cctx, u.Username, keyID)
	})
}

// userDelete removes the account and composes the cross-domain purges:
// mail addresses (so mail stops arriving), the personal drive's data,
// then the users domain's own rows.
func (h *handlers) userDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	u, back, err := h.userTarget(r, user, false)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	if r.FormValue("confirm") != u.Username {
		h.k.Respond(w, r, back, fmt.Errorf("type the username to confirm deletion"), nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	// Mail addresses first — deleting them stops new mail; the message
	// stores go with the address for mailboxes.
	if addrs, err := h.k.Mail.UserAddresses(cctx, u.Username); err == nil {
		for _, ad := range addrs {
			if err := h.k.Mail.DeleteAddress(cctx, ad.Domain, ad.Local); err != nil {
				h.k.Log.Warn("address purge on delete failed", "addr", ad.String(), "err", err)
			}
		}
	}
	// Personal drive: each domain purges what it owns.
	if u.PersonalDrive != "" {
		if err := h.k.Nodes.PurgeDriveData(cctx, u.PersonalDrive); err != nil {
			h.k.Respond(w, r, back, err, nil)
			return
		}
		if err := h.k.Shares.PurgeDriveSharing(cctx, u.PersonalDrive); err != nil {
			h.k.Respond(w, r, back, err, nil)
			return
		}
		_ = h.k.Media.PurgeDrive(cctx, u.PersonalDrive)
		if err := h.k.Drives.Delete(cctx, u.PersonalDrive); err != nil {
			h.k.Respond(w, r, back, err, nil)
			return
		}
	}
	// API keys die with the account.
	if keys, err := h.k.APIKeys.ListForUser(cctx, u.Username); err == nil {
		for _, key := range keys {
			_ = h.k.APIKeys.Revoke(cctx, u.Username, key.KeyID)
		}
	}
	if err := h.k.Users.DeleteUser(cctx, u.Username); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "user.delete", u.Username, "")
	h.k.Respond(w, r, "/admin/users?ok=account+deleted", nil, nil)
}

// impersonate mints a session AS the target (fully audited — start,
// stop, and everything auditable in between carries Impersonating).
func (h *handlers) impersonate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	u, back, err := h.userTarget(r, user, false)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	newSess, token, err := h.k.Users.ImpersonateSession(cctx, user.Username, u.Username, h.k.ClientIP(r), r.UserAgent())
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	// The admin's own session dies — one browser, one identity at a time.
	h.k.DropSession(r)
	h.k.Audit(r, user, sess, "impersonate.start", u.Username, "")
	h.k.SetSessionCookie(w, r, token, newSess.ExpiresAt)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// impersonateStop ends an impersonation: back to the admin's own
// account. Reachable by the impersonated session (NOT AdminOnly — the
// session's user is the member).
func (h *handlers) impersonateStop(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	newSess, token, err := h.k.Users.EndImpersonation(cctx, sess, h.k.ClientIP(r), r.UserAgent())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.k.DropSession(r)
	h.k.Audit(r, user, sess, "impersonate.stop", user.Username, "")
	h.k.SetSessionCookie(w, r, token, newSess.ExpiresAt)
	http.Redirect(w, r, "/admin/users/"+user.Username, http.StatusSeeOther)
}
