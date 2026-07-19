// Package mail owns everything email: hosted domains and their DKIM
// keys, postoffice pairings, addresses (mailboxes/aliases/distros),
// and — rebuilt around CONVERSATIONS for PCP (spec §7.1) — threads,
// messages, labels, folders, drafts, the send path with its undo-send
// hold, and search.
//
// File map: domains.go (mail domains + DKIM), addrs.go (addresses +
// mailboxes + delivery resolution), postoffices.go (gateway pairing +
// config pushes), threadid.go (thread identity), threads.go (thread
// meta, indexes, folders, flags, moves, purge), deliver.go (the
// delivery pipeline), labels.go, drafts.go, send.go (compose + send +
// undo hold), outq.go (outbound queue), search.go, parse.go (MIME →
// meta/search text), welcome.go.
//
// Key families are under /pcp/mail/ (kvx key table). Reverse indexes
// commit in the same transaction as their primary record, so every
// list is one prefix List; message ids are deterministic, so intake
// retries and replica races can never duplicate a message.
package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Key prefixes this package owns (kvx key table).
const (
	domainsPrefix   = "/pcp/mail/domains/"
	dkimPrefix      = "/pcp/mail/dkim/"
	posPrefix       = "/pcp/mail/postoffices/"
	poDomainsPrefix = "/pcp/mail/podomains/"
	domainPOsPrefix = "/pcp/mail/domainpos/"
	addrsPrefix     = "/pcp/mail/addrs/"
	userAddrsPrefix = "/pcp/mail/useraddrs/"
	boxesPrefix     = "/pcp/mail/boxes/"
	threadsPrefix   = "/pcp/mail/threads/"
	threadIdxPrefix = "/pcp/mail/threadidx/"
	sentIdxPrefix   = "/pcp/mail/sentidx/"
	starredPrefix   = "/pcp/mail/starred/"
	msgsPrefix      = "/pcp/mail/msgs/"
	msgRefPrefix    = "/pcp/mail/msgref/"
	labelsPrefix    = "/pcp/mail/labels/"
	byLabelPrefix   = "/pcp/mail/bylabel/"
	draftsPrefix    = "/pcp/mail/drafts/"
	foldersPrefix   = "/pcp/mail/folders/"
	blobsPrefix     = "/pcp/mail/blobs/"
	searchPrefix    = "/pcp/mail/searchtext/"
	outqPrefix      = "/pcp/mail/outq/"
	poCursorsPrefix = "/pcp/mail/pocursors/"
	sendlogPrefix   = "/pcp/mail/sendlog/"
	welcomePrefix   = "/pcp/mail/welcome/"
	recentsPrefix   = "/pcp/mail/recents/"
	idemPrefix      = "/pcp/mail/idem/"
	// serialKey is the monotonic address-manifest version, bumped in a
	// transaction whenever the manifest content changes.
	serialKey = "/pcp/mail/serial"
)

// Errors callers translate into user-facing messages.
var (
	ErrNotFound = errors.New("not found")
)

// Store wraps the databox client with the mail domain's access methods.
// Users composes in for allowance counters and quota charges (staged
// into mail transactions via users.UpdateInTx); Notify, when set, fires
// the new-mail notification from the delivery pipeline.
type Store struct {
	DB     *client.Client
	Users  *users.Store
	Notify *notify.Store
	// DefaultQuota is the bootstrap per-user quota (cmd/pcp's
	// PCP_DEFAULT_QUOTA) — the last rung of site.QuotaFor.
	DefaultQuota int64
	// Contacts is the compose-typeahead address book (phase 5's contacts
	// domain, wired in cmd/pcp — mail stays below contacts in the domain
	// layering). Returns "Name <email>" hits; nil = no address book.
	Contacts func(ctx context.Context, username, q string, limit int) []string
}

// MailboxesNone is the users.User.MailboxOverride value meaning
// "explicitly zero mailboxes" (0 means "unset — site default").
const MailboxesNone = -1

// MailboxesFor resolves how many email accounts a user may hold:
// per-user override beats the site default. 0 = none.
func MailboxesFor(sc site.Config, u users.User) int {
	switch {
	case u.MailboxOverride == MailboxesNone:
		return 0
	case u.MailboxOverride > 0:
		return u.MailboxOverride
	}
	return sc.Mail.MailboxAllowance()
}

// System folders. Custom folders use folder IDs (kvx.NewID) in the same
// key position; the names below are reserved. A thread lives in exactly
// ONE of these (spec §7.1) — Sent is a facet (sentidx), Drafts its own
// model, Starred an index.
const (
	FolderInbox   = "inbox"
	FolderArchive = "archive"
	FolderSpam    = "spam"
	FolderTrash   = "trash"
)

// SystemFolders is the rail's fixed order.
var SystemFolders = []string{FolderInbox, FolderArchive, FolderSpam, FolderTrash}

// systemFolder reports whether name is reserved.
func systemFolder(name string) bool {
	for _, f := range SystemFolders {
		if f == name {
			return true
		}
	}
	// The facet/virtual names are reserved too so a custom folder can
	// never shadow them.
	return name == "sent" || name == "drafts" || name == "starred"
}

// ValidFolder gates a folder key segment: a system name or a folder id.
func ValidFolder(f string) bool {
	switch f {
	case FolderInbox, FolderArchive, FolderSpam, FolderTrash:
		return true
	}
	return kvx.ValidID(f)
}

// DeliveredMsgID derives the deterministic per-mailbox message id for a
// delivery: retries and replica races write the same id, and the msgref
// existence check makes the second write a no-op.
func DeliveredMsgID(source, key, boxID string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + key + "\x00" + boxID))
	return hex.EncodeToString(sum[:12])
}

// hashBytes / shortHash derive deterministic keys from content.
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
