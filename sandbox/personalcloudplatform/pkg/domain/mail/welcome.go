// welcome.go — admin-composed onboarding messages, delivered into
// every newly created mailbox through the REAL delivery pipeline
// (never special-cased, so a fresh mailbox immediately materializes
// verifiable thread/message records) — plus the system DSN builders
// (quota bounces, delivery-failure bounces) and the system blob space.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
)

// Welcome scopes.
const (
	WelcomeAll    = "all"    // delivered to every new mailbox
	WelcomeDomain = "domain" // delivered to new mailboxes on one domain
)

// maxWelcomes bounds the admin's welcome list.
const maxWelcomes = 50

// Welcome is one admin-composed onboarding message.
type Welcome struct {
	ID     string `json:"id"`
	Scope  string `json:"scope"`            // WelcomeAll | WelcomeDomain
	Domain string `json:"domain,omitempty"` // scope=domain
	// From is the internal address the message arrives from; "" =
	// postmaster@<the new mailbox's domain>.
	From    string `json:"from,omitempty"`
	Subject string `json:"subject"`
	// Body is plain text; {{username}} {{display_name}} {{address}}
	// {{domain}} {{site_name}} substitute at delivery time.
	Body      string    `json:"body"`
	Enabled   bool      `json:"enabled"`
	Order     int       `json:"order"`
	CreatedAt time.Time `json:"created_at"`
	By        string    `json:"by"`
}

// SetWelcome creates (blank ID) or updates a welcome message.
func (s *Store) SetWelcome(ctx context.Context, w Welcome) (Welcome, error) {
	if w.Scope != WelcomeAll && w.Scope != WelcomeDomain {
		return Welcome{}, fmt.Errorf("bad welcome scope")
	}
	if w.Scope == WelcomeDomain {
		if err := ValidMailDomain(w.Domain); err != nil {
			return Welcome{}, err
		}
	} else {
		w.Domain = ""
	}
	w.Subject = strings.TrimSpace(w.Subject)
	if w.Subject == "" || len(w.Subject) > 200 {
		return Welcome{}, fmt.Errorf("welcome messages need a subject (≤200 chars)")
	}
	if len(w.Body) > 64<<10 {
		return Welcome{}, fmt.Errorf("welcome body is capped at 64 KiB")
	}
	if w.From != "" {
		if _, _, ok := SplitAddr(w.From); !ok {
			return Welcome{}, fmt.Errorf("bad from address")
		}
	}
	if w.ID == "" {
		existing, err := s.ListWelcomes(ctx)
		if err != nil {
			return Welcome{}, err
		}
		if len(existing) >= maxWelcomes {
			return Welcome{}, fmt.Errorf("at most %d welcome messages", maxWelcomes)
		}
		w.ID = kvx.NewID()
		w.CreatedAt = time.Now()
	} else if !kvx.ValidID(w.ID) {
		return Welcome{}, ErrNotFound
	}
	return w, kvx.SetJSON(ctx, s.DB, welcomePrefix+w.ID, w)
}

// DeleteWelcome removes a welcome message.
func (s *Store) DeleteWelcome(ctx context.Context, id string) error {
	if !kvx.ValidID(id) {
		return ErrNotFound
	}
	return s.DB.Delete(ctx, welcomePrefix+id)
}

// ListWelcomes returns every welcome message, delivery-ordered (Order,
// then age).
func (s *Store) ListWelcomes(ctx context.Context) ([]Welcome, error) {
	var out []Welcome
	err := kvx.ScanPrefix(ctx, s.DB, welcomePrefix, func(_ string, value []byte) error {
		var w Welcome
		if json.Unmarshal(value, &w) == nil {
			out = append(out, w)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, err
}

// WelcomesFor filters to the enabled messages a new mailbox on domain
// receives, in delivery order.
func (s *Store) WelcomesFor(ctx context.Context, domain string) ([]Welcome, error) {
	all, err := s.ListWelcomes(ctx)
	if err != nil {
		return nil, err
	}
	kept := all[:0]
	for _, w := range all {
		if w.Enabled && (w.Scope == WelcomeAll || w.Domain == domain) {
			kept = append(kept, w)
		}
	}
	return kept, nil
}

// BuildWelcomeMessage renders one welcome into deliverable RFC 822
// bytes. Deterministic Message-ID: re-delivery after a failure can't
// double-deliver (the delivery id derives from it too).
func BuildWelcomeMessage(we Welcome, siteName string, user users.User, box Mailbox) []byte {
	_, domain, _ := SplitAddr(box.Addr)
	from := we.From
	if from == "" {
		from = "postmaster@" + domain
	}
	vars := strings.NewReplacer(
		"{{username}}", user.Username,
		"{{display_name}}", user.DisplayName,
		"{{address}}", box.Addr,
		"{{domain}}", domain,
		"{{site_name}}", siteName,
	)
	subject := vars.Replace(we.Subject)
	body := vars.Replace(we.Body)
	now := time.Now()
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s <%s>\r\n", siteName, from)
	fmt.Fprintf(&b, "To: %s\r\n", box.Addr)
	fmt.Fprintf(&b, "Subject: %s\r\n", headerEncode(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <welcome-%s-%s@%s>\r\n", we.ID, box.ID, domain)
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	b.WriteString("\r\n")
	return b.Bytes()
}

// DeliverWelcomes drops every applicable welcome into a fresh mailbox
// through the standard pipeline (best-effort — a failed welcome never
// fails mailbox creation).
func (s *Store) DeliverWelcomes(ctx context.Context, sc site.Config, user users.User, box Mailbox) {
	_, domain, _ := SplitAddr(box.Addr)
	wes, err := s.WelcomesFor(ctx, domain)
	if err != nil {
		return
	}
	for _, we := range wes {
		raw := BuildWelcomeMessage(we, sc.Name, user, box)
		parsed := ParseMessage(raw)
		meta := s.msgMetaFromParse(parsed, ThreadID(parsed.ThreadKey()))
		meta.MsgID = DeliveredMsgID("welcome", we.ID, box.ID)
		_ = s.Deliver(ctx, Delivery{
			User: user.Username, BoxID: box.ID, Folder: FolderInbox,
			Meta: meta, Raw: raw, SearchText: parsed.SearchText,
			Quota: s.quotaFor(sc, user), Notify: true,
		})
	}
}

// --- system mail (DSNs, forwards) ---------------------------------------------------

// SystemMailAccount is the blob-space owner for system-generated mail
// (DSNs, distro forwards). '+' is outside the username alphabet, so no
// real account can ever collide with it.
const SystemMailAccount = "+sys"

// PutSystemBlob stores raw message bytes in the system blob space
// (uncharged — system mail belongs to no user's quota).
func (s *Store) PutSystemBlob(ctx context.Context, blobID string, raw []byte) error {
	return s.DB.PutBlob(ctx, blobsPrefix+SystemMailAccount+"/"+blobID, bytes.NewReader(raw), "message/rfc822")
}

// DeleteSystemBlob cleans up after the outbound loop.
func (s *Store) DeleteSystemBlob(ctx context.Context, blobID string) error {
	return s.DB.DeleteBlob(ctx, blobsPrefix+SystemMailAccount+"/"+blobID)
}

// BuildQuotaDSN composes the over-quota bounce for an inbound
// envelope. The returned mailFrom is the null reverse-path ("") — RFC
// 5321's bounce rule, and the reason a DSN can never bounce back and
// loop.
func BuildQuotaDSN(env mailproto.InboundEnvelope) (raw []byte, mailFrom string) {
	domain := "localhost"
	if len(env.Rcpts) > 0 {
		if _, d, ok := SplitAddr(env.Rcpts[0]); ok {
			domain = d
		}
	}
	var b bytes.Buffer
	now := time.Now().UTC()
	fmt.Fprintf(&b, "From: Mail Delivery System <mailer-daemon@%s>\r\n", domain)
	fmt.Fprintf(&b, "To: %s\r\n", env.From)
	fmt.Fprintf(&b, "Subject: Undeliverable: mailbox is full\r\n")
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Auto-Submitted: auto-replied\r\n")
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	fmt.Fprintf(&b, "Your message to %s could not be delivered:\r\n\r\n", strings.Join(env.Rcpts, ", "))
	fmt.Fprintf(&b, "    the recipient's mailbox is over its storage quota (552 5.2.2)\r\n\r\n")
	fmt.Fprintf(&b, "The message was received %s and has been discarded.\r\n", env.ReceivedAt.UTC().Format(time.RFC1123Z))
	return b.Bytes(), ""
}

// BuildBounceMessage composes the sender-facing DSN for a permanent
// outbound failure.
func BuildBounceMessage(om OutMsg, detail string) []byte {
	domain := "localhost"
	if _, d, ok := SplitAddr(om.MailFrom); ok {
		domain = d
	}
	now := time.Now()
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: Mail Delivery System <mailer-daemon@%s>\r\n", domain)
	fmt.Fprintf(&b, "To: %s\r\n", om.MailFrom)
	fmt.Fprintf(&b, "Subject: Undeliverable: message to %s\r\n", strings.Join(om.RcptTo, ", "))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Auto-Submitted: auto-replied\r\n")
	fmt.Fprintf(&b, "Message-ID: <bounce-%s@%s>\r\n", om.ID, domain)
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	fmt.Fprintf(&b, "Your message could not be delivered to:\r\n\r\n    %s\r\n\r\n", strings.Join(om.RcptTo, "\r\n    "))
	fmt.Fprintf(&b, "Reason: %s\r\n", detail)
	return b.Bytes()
}
