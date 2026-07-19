// notifmail.go — plain notification email for other apps (Git Services
// Draft 002 §11 is the first consumer): a system-generated text message
// delivered into the recipient's PRIMARY mailbox through the standard
// delivery pipeline (welcome-mail semantics — quota-charged, threaded,
// searchable), never through the outbound queue (the recipient is a
// hosted account by definition). Callers own the opt-in policy; this
// only knows how to deliver. Best-effort territory: callers log and
// move on.
package mail

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// PrimaryMailbox resolves a user's primary mailbox (deterministic:
// lowest address wins — the calendar's OrganizerBox rule). ok=false
// means the account has no mailbox and notification mail skips.
func (s *Store) PrimaryMailbox(ctx context.Context, username string) (Mailbox, bool) {
	boxes, err := s.UserMailboxes(ctx, strings.ToLower(username))
	if err != nil || len(boxes) == 0 {
		return Mailbox{}, false
	}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].Addr < boxes[j].Addr })
	return boxes[0], true
}

// DeliverNotification composes and delivers one plain-text notification
// message to username's primary mailbox. fromName labels the sender
// ("Git Services"); the address is postmaster@<the recipient's own
// domain> — notification mail never impersonates a member. The bell
// notification is suppressed (the caller already raised the app's own).
func (s *Store) DeliverNotification(ctx context.Context, sc site.Config, username, fromName, subject, body string) error {
	username = strings.ToLower(username)
	box, ok := s.PrimaryMailbox(ctx, username)
	if !ok {
		return nil // no mailbox — nothing to deliver, not an error
	}
	u, found, err := s.Users.Get(ctx, username)
	if err != nil || !found {
		return err
	}
	_, domain, _ := SplitAddr(box.Addr)
	if domain == "" {
		domain = "localhost"
	}
	now := time.Now()
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s <postmaster@%s>\r\n", headerEncode(fromName), domain)
	fmt.Fprintf(&b, "To: %s\r\n", box.Addr)
	fmt.Fprintf(&b, "Subject: %s\r\n", headerEncode(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <notif-%s@%s>\r\n", kvx.NewID(), domain)
	fmt.Fprintf(&b, "Auto-Submitted: auto-generated\r\n")
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	b.WriteString("\r\n")
	raw := b.Bytes()
	parsed := ParseMessage(raw)
	meta := s.msgMetaFromParse(parsed, ThreadID(parsed.ThreadKey()))
	meta.MsgID = DeliveredMsgID("notif", parsed.MessageID, box.ID)
	return s.Deliver(ctx, Delivery{
		User: username, BoxID: box.ID, Folder: FolderInbox,
		Meta: meta, Raw: raw, SearchText: parsed.SearchText,
		Quota: s.quotaFor(sc, u), Notify: false,
	})
}
