// addrs.go — email addresses and mailboxes. One namespace per domain
// (/pcp/mail/addrs/<domain>/<local>) holds all three address types, so
// a mailbox, an alias, and a distro list can never collide; a reverse
// index under the owner makes "my addresses" a single-prefix List.
//
// Allowance enforcement is the house OCC pattern: MailboxCount /
// MailAliasCount live ON the user record and are updated in the same
// transaction that claims the address — racing claims for the last
// slot resolve with exactly one winner.
package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Address types (Address.Type).
const (
	AddrMailbox = "mailbox" // a message store the owner reads
	AddrAlias   = "alias"   // delivers into another address's mailbox
	AddrDistro  = "distro"  // expands to a member list at intake
)

// reservedLocals are local parts users can never claim. postmaster and
// abuse are auto-created per domain (domains.go AddDomain);
// mailer-daemon is the bounce sender identity.
var reservedLocals = map[string]bool{
	"postmaster": true, "abuse": true, "mailer-daemon": true,
}

// Address is one deliverable address on a hosted domain.
type Address struct {
	Domain string `json:"domain"`
	Local  string `json:"local"`
	Type   string `json:"type"`
	// Owner is the account behind a mailbox or alias ("" for distros).
	Owner string `json:"owner,omitempty"`
	// BoxID names the mailbox's message store (Type=mailbox).
	BoxID string `json:"box_id,omitempty"`
	// Target is an alias's destination address ("local@domain" on this
	// instance). "" = the owner's first mailbox, resolved at delivery —
	// the state the auto-created postmaster/abuse aliases start in.
	Target string `json:"target,omitempty"`
	// Members are a distro's recipients: internal addresses or external
	// emails. AllowedSenders are EXTERNAL addresses permitted to post to
	// the distro (internal members always may).
	Members        []string  `json:"members,omitempty"`
	AllowedSenders []string  `json:"allowed_senders,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	By             string    `json:"by"`
}

// String is the full address.
func (a Address) String() string { return a.Local + "@" + a.Domain }

// addrRef is the slim reverse-index record under the owner.
type addrRef struct {
	Type string `json:"type"`
}

// Mailbox is one message store (a user with three addresses has three).
type Mailbox struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	// Addr is the mailbox's primary address ("local@domain").
	Addr string `json:"addr"`
	// Signature is appended to composed mail (user-editable).
	Signature string    `json:"signature,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ValidLocalPart gates the user-chosen half of an address: 1–64 chars
// of a-z 0-9 . _ -, no edge dots, no runs of dots. Stored lowercase; it
// becomes a key segment, so this is also the traversal gate.
func ValidLocalPart(local string) error {
	if len(local) < 1 || len(local) > 64 {
		return fmt.Errorf("the address part must be 1–64 characters")
	}
	if local[0] == '.' || local[len(local)-1] == '.' || strings.Contains(local, "..") {
		return fmt.Errorf("dots can't lead, trail, or repeat")
	}
	for _, r := range local {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return fmt.Errorf("addresses may contain only a-z, 0-9, dots, dashes, and underscores")
		}
	}
	return nil
}

// SplitAddr parses "local@domain" (both halves validated) — the gate
// every stored or routed address string passes through.
func SplitAddr(addr string) (local, domain string, ok bool) {
	local, domain, found := strings.Cut(strings.ToLower(strings.TrimSpace(addr)), "@")
	if !found || ValidLocalPart(local) != nil || ValidMailDomain(domain) != nil {
		return "", "", false
	}
	return local, domain, true
}

// validExternalEmail loosely gates addresses we don't host (distro
// members, allowed senders, send targets): shaped like mail, bounded,
// key-safe enough to store. Deliverability is the receiving MTA's
// problem.
func validExternalEmail(addr string) bool {
	if len(addr) > 254 || strings.ContainsAny(addr, " \t\r\n/\x00") {
		return false
	}
	local, domain, found := strings.Cut(addr, "@")
	return found && local != "" && strings.Contains(domain, ".") && ValidMailDomain(strings.ToLower(domain)) == nil
}

// claimableDomain loads a domain and refuses claims on missing or
// disabled ones.
func (s *Store) claimableDomain(ctx context.Context, domain string) error {
	d, found, err := s.GetDomain(ctx, domain)
	if err != nil {
		return err
	}
	if !found || !d.Enabled {
		return fmt.Errorf("mail domain %s isn't available", domain)
	}
	return nil
}

// CreateMailbox claims local@domain as a new mailbox for user. ONE
// transaction: address uniqueness, the user's allowance (MailboxCount
// vs allowed), the box record, the reverse index — and, on the user's
// FIRST mailbox, the starter label set (spec §7.2). Racing claims
// resolve through OCC. allowed comes from MailboxesFor.
func (s *Store) CreateMailbox(ctx context.Context, username, domain, local string, allowed int) (Mailbox, error) {
	local = strings.ToLower(strings.TrimSpace(local))
	domain = strings.ToLower(strings.TrimSpace(domain))
	if err := ValidLocalPart(local); err != nil {
		return Mailbox{}, err
	}
	if reservedLocals[local] {
		return Mailbox{}, fmt.Errorf("%s is reserved", local)
	}
	if err := s.claimableDomain(ctx, domain); err != nil {
		return Mailbox{}, err
	}
	box := Mailbox{ID: kvx.NewID(), Owner: username, Addr: local + "@" + domain, CreatedAt: time.Now()}
	addr := Address{
		Domain: domain, Local: local, Type: AddrMailbox,
		Owner: username, BoxID: box.ID, CreatedAt: box.CreatedAt, By: username,
	}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		first := false
		if err := s.Users.UpdateInTx(ctx, tx, username, func(u *users.User) error {
			if u.MailboxCount >= allowed {
				return fmt.Errorf("you've used all %d of your email accounts", allowed)
			}
			first = u.MailboxCount == 0
			u.MailboxCount++
			return nil
		}); err != nil {
			return err
		}
		if _, exists, err := tx.Get(ctx, addrsPrefix+domain+"/"+local); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("%s@%s is taken", local, domain)
		}
		araw, _ := json.Marshal(addr)
		tx.Set(addrsPrefix+domain+"/"+local, araw)
		braw, _ := json.Marshal(box)
		tx.Set(boxesPrefix+username+"/"+box.ID, braw)
		rraw, _ := json.Marshal(addrRef{Type: AddrMailbox})
		tx.Set(userAddrsPrefix+username+"/"+domain+"/"+local, rraw)
		if first {
			stageStarterLabels(tx, username)
		}
		return nil
	})
	if err != nil {
		if kvx.IsConflict(err) {
			return Mailbox{}, fmt.Errorf("that address was just claimed — try again")
		}
		return Mailbox{}, err
	}
	return box, nil
}

// CreateAlias claims local@domain as an alias owned by username,
// delivering to target (an internal address string; "" = the owner's
// first mailbox). Same OCC shape as CreateMailbox against the alias cap.
func (s *Store) CreateAlias(ctx context.Context, username, domain, local, target string, maxAliases int) (Address, error) {
	local = strings.ToLower(strings.TrimSpace(local))
	domain = strings.ToLower(strings.TrimSpace(domain))
	if err := ValidLocalPart(local); err != nil {
		return Address{}, err
	}
	if reservedLocals[local] {
		return Address{}, fmt.Errorf("%s is reserved", local)
	}
	if err := s.claimableDomain(ctx, domain); err != nil {
		return Address{}, err
	}
	if target != "" {
		if _, _, ok := SplitAddr(target); !ok {
			return Address{}, fmt.Errorf("bad alias target")
		}
	}
	addr := Address{
		Domain: domain, Local: local, Type: AddrAlias,
		Owner: username, Target: target, CreatedAt: time.Now(), By: username,
	}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if err := s.Users.UpdateInTx(ctx, tx, username, func(u *users.User) error {
			if u.MailAliasCount >= maxAliases {
				return fmt.Errorf("you've used all %d of your aliases", maxAliases)
			}
			u.MailAliasCount++
			return nil
		}); err != nil {
			return err
		}
		if _, exists, err := tx.Get(ctx, addrsPrefix+domain+"/"+local); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("%s@%s is taken", local, domain)
		}
		araw, _ := json.Marshal(addr)
		tx.Set(addrsPrefix+domain+"/"+local, araw)
		rraw, _ := json.Marshal(addrRef{Type: AddrAlias})
		tx.Set(userAddrsPrefix+username+"/"+domain+"/"+local, rraw)
		return nil
	})
	if err != nil {
		if kvx.IsConflict(err) {
			return Address{}, fmt.Errorf("that address was just claimed — try again")
		}
		return Address{}, err
	}
	return addr, nil
}

// CreateDistro registers a distribution-list address (admin-only at
// the controller). Members must be internal addresses or external
// emails.
func (s *Store) CreateDistro(ctx context.Context, domain, local string, members, allowedSenders []string, by string) (Address, error) {
	local = strings.ToLower(strings.TrimSpace(local))
	domain = strings.ToLower(strings.TrimSpace(domain))
	if err := ValidLocalPart(local); err != nil {
		return Address{}, err
	}
	if reservedLocals[local] {
		return Address{}, fmt.Errorf("%s is reserved", local)
	}
	if err := s.claimableDomain(ctx, domain); err != nil {
		return Address{}, err
	}
	members, err := normalizeDistroList(members, 500)
	if err != nil {
		return Address{}, err
	}
	if len(members) == 0 {
		return Address{}, fmt.Errorf("a distribution list needs at least one member")
	}
	allowedSenders, err = normalizeDistroList(allowedSenders, 200)
	if err != nil {
		return Address{}, err
	}
	addr := Address{
		Domain: domain, Local: local, Type: AddrDistro,
		Members: members, AllowedSenders: allowedSenders,
		CreatedAt: time.Now(), By: by,
	}
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, exists, err := tx.Get(ctx, addrsPrefix+domain+"/"+local); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("%s@%s is taken", local, domain)
		}
		araw, _ := json.Marshal(addr)
		tx.Set(addrsPrefix+domain+"/"+local, araw)
		return nil
	})
	if err != nil {
		if kvx.IsConflict(err) {
			return Address{}, fmt.Errorf("that address was just claimed — try again")
		}
		return Address{}, err
	}
	return addr, nil
}

// UpdateDistro replaces a distro's members and allowed senders.
func (s *Store) UpdateDistro(ctx context.Context, domain, local string, members, allowedSenders []string) error {
	members, err := normalizeDistroList(members, 500)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return fmt.Errorf("a distribution list needs at least one member")
	}
	allowedSenders, err = normalizeDistroList(allowedSenders, 200)
	if err != nil {
		return err
	}
	return s.mutateAddr(ctx, domain, local, AddrDistro, func(a *Address) error {
		a.Members, a.AllowedSenders = members, allowedSenders
		return nil
	})
}

// RetargetAlias points an alias at a new internal address ("" =
// owner's first mailbox). Works on postmaster/abuse too — that's the
// admin's escape hatch.
func (s *Store) RetargetAlias(ctx context.Context, domain, local, target string) error {
	if target != "" {
		if _, _, ok := SplitAddr(target); !ok {
			return fmt.Errorf("bad alias target")
		}
	}
	return s.mutateAddr(ctx, domain, local, AddrAlias, func(a *Address) error {
		a.Target = target
		return nil
	})
}

// SetMailboxSignature updates a mailbox's compose signature.
func (s *Store) SetMailboxSignature(ctx context.Context, owner, boxID, signature string) error {
	if len(signature) > 4096 {
		return fmt.Errorf("signatures are capped at 4 KiB")
	}
	if !kvx.ValidID(boxID) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, boxesPrefix+owner+"/"+boxID)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var b Mailbox
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		b.Signature = signature
		out, _ := json.Marshal(b)
		tx.Set(boxesPrefix+owner+"/"+boxID, out)
		return nil
	})
}

// mutateAddr is the shared read-modify-write for one address record.
func (s *Store) mutateAddr(ctx context.Context, domain, local, wantType string, mutate func(*Address) error) error {
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, addrsPrefix+domain+"/"+local)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var a Address
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
		if a.Type != wantType {
			return fmt.Errorf("%s@%s is a %s, not a %s", local, domain, a.Type, wantType)
		}
		if err := mutate(&a); err != nil {
			return err
		}
		out, _ := json.Marshal(a)
		tx.Set(addrsPrefix+domain+"/"+local, out)
		return nil
	})
}

// DeleteAddress removes an alias or distro (mailboxes keep their
// message store — deleting one is a bigger operation deferred to the
// admin console phase). The reserved postmaster/abuse aliases refuse
// deletion; retarget them instead.
func (s *Store) DeleteAddress(ctx context.Context, domain, local string) error {
	if local == "postmaster" || local == "abuse" {
		return fmt.Errorf("%s@%s is required by RFC 2142 — retarget it instead", local, domain)
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, addrsPrefix+domain+"/"+local)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var a Address
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
		if a.Type == AddrMailbox {
			return fmt.Errorf("delete the email account, not just its address")
		}
		tx.Delete(addrsPrefix + domain + "/" + local)
		if a.Owner != "" {
			tx.Delete(userAddrsPrefix + a.Owner + "/" + domain + "/" + local)
			if a.Type == AddrAlias {
				_ = s.Users.UpdateInTx(ctx, tx, a.Owner, func(u *users.User) error {
					if u.MailAliasCount > 0 {
						u.MailAliasCount--
					}
					return nil
				})
			}
		}
		return nil
	})
}

// DeleteMailbox deletes an EMAIL ACCOUNT (phase 8, admin console): the
// address record, the reverse index, the mailbox record, and the whole
// message store — every thread purged (blobs deleted, quota refunded),
// then the drafts/folders/index prefixes swept. The owner's
// MailboxCount frees a slot in the same transaction as the address
// delete, so a racing claim can't over- or under-count.
//
// Consequences (the admin page states them): mail addressed here starts
// bouncing at the next manifest push; postmaster/abuse aliases that
// implicitly delivered to this account fall through to the owner's next
// mailbox, or bounce when none remains.
func (s *Store) DeleteMailbox(ctx context.Context, domain, local string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	local = strings.ToLower(strings.TrimSpace(local))
	a, found, err := s.GetAddress(ctx, domain, local)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	if a.Type != AddrMailbox {
		return fmt.Errorf("%s@%s is a %s — remove it from its own page", local, domain, a.Type)
	}
	owner, boxID := a.Owner, a.BoxID
	// Purge every thread (message rows, refs, blobs, search text,
	// indexes, quota refunds) — PurgeThread is the one tested path for
	// that, so reuse it thread by thread.
	var threadIDs []string
	if err := kvx.ScanPrefix(ctx, s.DB, threadsPrefix+owner+"/"+boxID+"/", func(key string, _ []byte) error {
		threadIDs = append(threadIDs, key[strings.LastIndex(key, "/")+1:])
		return nil
	}); err != nil {
		return err
	}
	for _, id := range threadIDs {
		if err := s.PurgeThread(ctx, owner, boxID, id); err != nil && err != ErrNotFound {
			return err
		}
	}
	// Sweep what's left of the box-scoped families (drafts keep
	// attachment blobs under blobs/<owner>/att-*; a deleted account's
	// stragglers are lazily GC'd with the draft retention).
	for _, prefix := range []string{
		threadIdxPrefix + owner + "/" + boxID + "/",
		sentIdxPrefix + owner + "/" + boxID + "/",
		starredPrefix + owner + "/" + boxID + "/",
		msgsPrefix + owner + "/" + boxID + "/",
		draftsPrefix + owner + "/" + boxID + "/",
		foldersPrefix + owner + "/" + boxID + "/",
	} {
		if err := kvx.DeletePrefix(ctx, s.DB, prefix); err != nil {
			return err
		}
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, found, err := tx.Get(ctx, addrsPrefix+domain+"/"+local); err != nil {
			return err
		} else if !found {
			return nil // raced with another delete — done is done
		}
		tx.Delete(addrsPrefix + domain + "/" + local)
		tx.Delete(userAddrsPrefix + owner + "/" + domain + "/" + local)
		tx.Delete(boxesPrefix + owner + "/" + boxID)
		return s.Users.UpdateInTx(ctx, tx, owner, func(u *users.User) error {
			if u.MailboxCount > 0 {
				u.MailboxCount--
			}
			return nil
		})
	})
}

// normalizeDistroList lowercases, dedups, and validates a member or
// sender list.
func normalizeDistroList(in []string, cap int) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, m := range in {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "" || seen[m] {
			continue
		}
		if !validExternalEmail(m) {
			return nil, fmt.Errorf("bad address %q", m)
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) > cap {
		return nil, fmt.Errorf("at most %d entries", cap)
	}
	return out, nil
}

// GetAddress loads one address record.
func (s *Store) GetAddress(ctx context.Context, domain, local string) (Address, bool, error) {
	if ValidMailDomain(domain) != nil || ValidLocalPart(local) != nil {
		return Address{}, false, nil
	}
	var a Address
	found, err := kvx.GetJSON(ctx, s.DB, addrsPrefix+domain+"/"+local, &a)
	return a, found, err
}

// UserAddresses lists every address a user owns (mailboxes + aliases),
// resolved to full records. Small per user by construction.
func (s *Store) UserAddresses(ctx context.Context, username string) ([]Address, error) {
	var out []Address
	err := kvx.ScanPrefix(ctx, s.DB, userAddrsPrefix+username+"/", func(key string, _ []byte) error {
		rest := strings.TrimPrefix(key, userAddrsPrefix+username+"/")
		domain, local, found := strings.Cut(rest, "/")
		if !found {
			return nil
		}
		a, ok, err := s.GetAddress(ctx, domain, local)
		if err != nil {
			return err
		}
		if ok {
			out = append(out, a)
		}
		return nil
	})
	return out, err
}

// UserMailboxes lists a user's message stores.
func (s *Store) UserMailboxes(ctx context.Context, username string) ([]Mailbox, error) {
	var out []Mailbox
	err := kvx.ScanPrefix(ctx, s.DB, boxesPrefix+username+"/", func(_ string, value []byte) error {
		var b Mailbox
		if json.Unmarshal(value, &b) == nil {
			out = append(out, b)
		}
		return nil
	})
	return out, err
}

// GetMailbox loads one message store.
func (s *Store) GetMailbox(ctx context.Context, owner, boxID string) (Mailbox, bool, error) {
	var b Mailbox
	if !kvx.ValidID(boxID) {
		return b, false, nil
	}
	found, err := kvx.GetJSON(ctx, s.DB, boxesPrefix+owner+"/"+boxID, &b)
	return b, found, err
}

// ListDomainAddresses pages one domain's addresses (admin views).
func (s *Store) ListDomainAddresses(ctx context.Context, domain, cursor string, limit int) ([]Address, string, error) {
	if limit <= 0 {
		limit = 100
	}
	c := ""
	if cursor != "" {
		c = addrsPrefix + domain + "/" + cursor
	}
	entries, next, err := s.DB.List(ctx, addrsPrefix+domain+"/", c, limit)
	if err != nil {
		return nil, "", err
	}
	var out []Address
	for _, e := range entries {
		var a Address
		if json.Unmarshal(e.Value, &a) == nil {
			out = append(out, a)
		}
	}
	nextLocal := ""
	if next != "" {
		nextLocal = strings.TrimPrefix(next, addrsPrefix+domain+"/")
	}
	return out, nextLocal, nil
}

// AllAddresses sweeps every hosted address on enabled domains — the
// manifest builder and admin audit views. Bounded by the instance's
// own address count.
func (s *Store) AllAddresses(ctx context.Context) ([]Address, error) {
	domains, err := s.ListDomains(ctx)
	if err != nil {
		return nil, err
	}
	var out []Address
	for _, d := range domains {
		if !d.Enabled {
			continue
		}
		if err := kvx.ScanPrefix(ctx, s.DB, addrsPrefix+d.Domain+"/", func(_ string, value []byte) error {
			var a Address
			if json.Unmarshal(value, &a) == nil {
				out = append(out, a)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// --- delivery resolution -----------------------------------------------------------

// DeliveryTarget is one resolved destination for an inbound recipient.
type DeliveryTarget struct {
	Owner string
	BoxID string
	// ViaDistro names the expanding list ("" for direct/alias delivery).
	ViaDistro string
}

// maxResolveDepth caps alias chains (aliases may point at aliases; a
// loop dies here instead of recursing forever).
const maxResolveDepth = 4

// ResolveDeliveries expands one RCPT address into concrete mailboxes
// plus any EXTERNAL forwards a distro carries. sender is the envelope
// MAIL FROM: a distro only expands when the sender is an internal
// member or on the distro's external allowlist ("" sender = internal
// short-circuit, always trusted). Unknown addresses resolve to nothing
// — the manifest already gated them at the gateway.
func (s *Store) ResolveDeliveries(ctx context.Context, rcpt, sender string) (targets []DeliveryTarget, externals []string, err error) {
	return s.resolveAddr(ctx, rcpt, sender, "", maxResolveDepth, map[string]bool{})
}

func (s *Store) resolveAddr(ctx context.Context, rcpt, sender, viaDistro string, depth int, seen map[string]bool) ([]DeliveryTarget, []string, error) {
	local, domain, ok := SplitAddr(rcpt)
	if !ok || depth <= 0 || seen[local+"@"+domain] {
		return nil, nil, nil
	}
	seen[local+"@"+domain] = true
	addr, found, err := s.GetAddress(ctx, domain, local)
	if err != nil || !found {
		return nil, nil, err
	}
	switch addr.Type {
	case AddrMailbox:
		return []DeliveryTarget{{Owner: addr.Owner, BoxID: addr.BoxID, ViaDistro: viaDistro}}, nil, nil
	case AddrAlias:
		target := addr.Target
		if target == "" {
			// Owner's first mailbox (the postmaster/abuse default).
			boxes, err := s.UserMailboxes(ctx, addr.Owner)
			if err != nil || len(boxes) == 0 {
				return nil, nil, err
			}
			return []DeliveryTarget{{Owner: addr.Owner, BoxID: boxes[0].ID, ViaDistro: viaDistro}}, nil, nil
		}
		return s.resolveAddr(ctx, target, sender, viaDistro, depth-1, seen)
	case AddrDistro:
		// Sender authorization: a distro only expands for internal
		// members or allowlisted external senders. An empty sender is the
		// internal short-circuit (the composer is already authenticated).
		if !distroAllowsSender(addr, sender) {
			return nil, nil, nil
		}
		var all []DeliveryTarget
		var ext []string
		for _, m := range addr.Members {
			mLocal, mDomain, ok := SplitAddr(m)
			if !ok {
				continue
			}
			if _, hosted, err := s.GetDomain(ctx, mDomain); err != nil {
				return nil, nil, err
			} else if hosted {
				ts, es, err := s.resolveAddr(ctx, mLocal+"@"+mDomain, sender, addr.String(), depth-1, seen)
				if err != nil {
					return nil, nil, err
				}
				all = append(all, ts...)
				ext = append(ext, es...)
			} else if !seen[m] {
				seen[m] = true
				ext = append(ext, m)
			}
		}
		return all, ext, nil
	}
	return nil, nil, nil
}

// distroAllowsSender enforces a distribution list's posting policy: an
// empty sender (internal short-circuit) always passes; otherwise the
// sender must be a member or on the external allowlist.
func distroAllowsSender(addr Address, sender string) bool {
	if sender == "" {
		return true
	}
	sender = strings.ToLower(strings.TrimSpace(sender))
	for _, m := range addr.Members {
		if strings.ToLower(m) == sender {
			return true
		}
	}
	for _, as := range addr.AllowedSenders {
		if strings.ToLower(as) == sender {
			return true
		}
	}
	return false
}
