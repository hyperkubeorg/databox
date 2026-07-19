// messages.go — the message model (Messenger §5). Messages are
// keyed newest-first under a conversation (kvx.InvID) so the databox
// forward-only List returns the latest page first and pages older with the
// server cursor. A msgref row locates a message by its stable id for edit,
// delete, and permalinks. Send bumps the conversation's LastMsgTs in the
// same transaction. Search postings, mention fan-out, and unread badges
// ride on later phases.
package messenger

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// ErrEmptyMessage rejects a blank body.
var ErrEmptyMessage = errors.New("message is empty")

// Message is one posted message. HTML is the rendered, safe cache of Body.
type Message struct {
	ID          string       `json:"id"`   // stable id (permalink, edit/delete, mentions)
	Sort        string       `json:"sort"` // the InvID sort segment that locates the row
	CID         string       `json:"cid"`
	Author      string       `json:"author"`
	Ts          time.Time    `json:"ts"`
	EditedTs    time.Time    `json:"edited_ts,omitempty"`
	Body        string       `json:"body"` // raw markdown
	HTML        string       `json:"html"` // rendered, safe
	Attachments []Attachment `json:"attachments,omitempty"`
	InviteCode  string       `json:"invite_code,omitempty"` // renders a Join embed
	Deleted     bool         `json:"deleted,omitempty"`
}

// MsgRef locates a message by its stable id.
type MsgRef struct {
	CID  string `json:"cid"`
	Sort string `json:"sort"`
}

func msgKey(cid, sort string) string { return msgsPrefix + cid + "/" + sort }
func msgRefKey(msgID string) string  { return msgRefPrefix + msgID }

// SendOpts carries the optional extras on a send (attachments, an invite
// embed) so the channel/DM/group send paths share one signature.
type SendOpts struct {
	Attachments []Attachment
	InviteCode  string
}

// SendToChannel posts a message to a server channel after enforcing
// channel visibility and PermSendMessages. The full users.User is required
// so the permission engine can apply admin override (and gate @here/
// @channel via PermMentionEveryone). It returns the stored message.
func (s *Store) SendToChannel(ctx context.Context, serverID, channelID string, user users.User, body string, opts SendOpts) (Message, error) {
	ch, found, err := s.GetChannel(ctx, serverID, channelID)
	if err != nil || !found {
		return Message{}, ErrNotFound
	}
	set, member, err := s.EffectivePerms(ctx, serverID, user)
	if err != nil {
		return Message{}, err
	}
	if !member && !user.IsAdmin {
		return Message{}, ErrAccessDenied
	}
	if set != PermAll && !set.Has(PermSendMessages) {
		return Message{}, ErrAccessDenied
	}
	if ok, err := s.CanViewChannel(ctx, user, ch); err != nil || !ok {
		return Message{}, ErrAccessDenied
	}
	if len(opts.Attachments) > 0 && set != PermAll && !set.Has(PermAttachFiles) {
		return Message{}, ErrAccessDenied
	}
	// Resolve the channel's recipients (members who can view it).
	rows, err := s.Members(ctx, serverID)
	if err != nil {
		return Message{}, err
	}
	recipients := make([]string, 0, len(rows))
	for _, m := range rows {
		if m.Banned {
			continue
		}
		mu := users.User{Username: m.Username}
		if ok, _ := s.CanViewChannel(ctx, mu, ch); ok {
			recipients = append(recipients, m.Username)
		}
	}
	canEveryone := set == PermAll || set.Has(PermMentionEveryone)
	return s.deliver(ctx, channelID, ConvoChannel, serverID, user, body, recipients, canEveryone, opts)
}

// deliver is the write path shared by channels, DMs, and group DMs: it
// commits the message (with attachments/invite), bumps the conversation,
// and fans out unread badges, mention ledgers, and notifications.
func (s *Store) deliver(ctx context.Context, cid, kind, serverID string, sender users.User, body string, recipients []string, canEveryone bool, opts SendOpts) (Message, error) {
	body = strings.TrimRight(body, " \t\n")
	if strings.TrimSpace(body) == "" && len(opts.Attachments) == 0 && opts.InviteCode == "" {
		return Message{}, ErrEmptyMessage
	}
	if r := []rune(body); len(r) > MaxMessageRunes {
		body = string(r[:MaxMessageRunes])
	}
	author := strings.ToLower(sender.Username)
	now := time.Now().UTC()
	m := Message{
		ID:          kvx.NewID(),
		Sort:        kvx.InvIDAt(now),
		CID:         cid,
		Author:      author,
		Ts:          now,
		Body:        body,
		HTML:        RenderMarkdown(body),
		Attachments: opts.Attachments,
		InviteCode:  opts.InviteCode,
	}
	if err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		setJSONTx(tx, msgKey(cid, m.Sort), m)
		setJSONTx(tx, msgRefKey(m.ID), MsgRef{CID: cid, Sort: m.Sort})
		touchConvoTx(ctx, tx, cid, kind, serverID, now)
		return nil
	}); err != nil {
		return Message{}, err
	}
	s.writeSearch(ctx, m)
	s.fanOut(ctx, m, kind, serverID, recipients, canEveryone, sender)
	return m, nil
}

// fanOut delivers unread badges, mention ledgers, and notifications to a
// message's recipients (author excluded). Mentions bump the badge with the
// mention flag and raise a notification (suppressed while the recipient is
// DND).
func (s *Store) fanOut(ctx context.Context, m Message, kind, serverID string, recipients []string, canEveryone bool, sender users.User) {
	author := strings.ToLower(sender.Username)
	fromName := sender.DisplayName
	if fromName == "" {
		fromName = sender.Username
	}
	_ = s.ClearUnread(ctx, author, m.CID)

	if len(recipients) > UnreadFanoutCap {
		if s.Log != nil {
			s.Log.Warn("messenger fan-out skipped (over cap; badges derived)",
				"cid", m.CID, "recipients", len(recipients), "cap", UnreadFanoutCap)
		}
		return
	}
	names, here, channelAll := parseMentions(m.Body)
	channelAll = channelAll && canEveryone
	here = here && canEveryone
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[strings.ToLower(n)] = true
	}
	for _, u := range recipients {
		lu := strings.ToLower(u)
		if lu == author {
			continue
		}
		mention := channelAll || nameSet[lu]
		if !mention && here {
			if connected, _ := s.IsConnected(ctx, u); connected {
				mention = true
			}
		}
		// A DM/group message to someone without a messenger page open
		// raises the bell (visible from every PCP app) — but at most once
		// per conversation until they read it, so an active back-and-forth
		// doesn't stack a row per message. The dedupe keys on whether we've
		// ALREADY notified them since their last read (a "notified" marker),
		// NOT on the unread Count: a prior bump that raised no bell (e.g. it
		// landed while they had messenger open) must still let the next
		// message ring. Checked BEFORE we mark notified.
		dmWaiting := false
		if !mention && kind != ConvoChannel && s.Notify != nil {
			if in, _ := s.InMessenger(ctx, u); !in {
				dmWaiting = !s.wasNotified(ctx, u, m.CID)
			}
		}
		_ = s.BumpUnread(ctx, u, m.CID, kind, serverID, mention)
		if mention {
			s.recordMention(ctx, u, m)
			s.notifyMention(ctx, u, fromName, m, kind, serverID)
		} else if dmWaiting {
			if s.notifyDMWaiting(ctx, u, fromName, m, kind, serverID) {
				s.markNotified(ctx, u, m.CID)
			}
		}
	}
}

// Messages returns a page of a conversation's messages in ASCENDING
// (oldest-first) display order, plus an opaque cursor for the NEXT-OLDER
// page (empty when the beginning is reached). Pass an empty cursor for the
// latest page. limit caps the page (0 = 50).
func (s *Store) Messages(ctx context.Context, cid, cursor string, limit int) ([]Message, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	prefix := msgsPrefix + cid + "/"
	entries, next, err := s.DB.List(ctx, prefix, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	// Store order is newest-first (InvID); reverse to ascending for display.
	out := make([]Message, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		var m Message
		if json.Unmarshal(entries[i].Value, &m) == nil {
			out = append(out, m)
		}
	}
	return out, next, nil
}

// GetMessage loads one message by its stable id (via the msgref).
func (s *Store) GetMessage(ctx context.Context, msgID string) (Message, bool, error) {
	if !kvx.ValidID(msgID) {
		return Message{}, false, nil
	}
	var ref MsgRef
	found, err := kvx.GetJSON(ctx, s.DB, msgRefKey(msgID), &ref)
	if err != nil || !found {
		return Message{}, false, err
	}
	var m Message
	found, err = kvx.GetJSON(ctx, s.DB, msgKey(ref.CID, ref.Sort), &m)
	return m, found, err
}

// EditMessage rewrites a message's body (author only; moderators use
// DeleteMessage). The stable id and position are preserved; EditedTs is
// stamped.
func (s *Store) EditMessage(ctx context.Context, msgID, author, body string) (Message, error) {
	author = strings.ToLower(author)
	body = strings.TrimRight(body, " \t\n")
	if strings.TrimSpace(body) == "" {
		return Message{}, ErrEmptyMessage
	}
	if r := []rune(body); len(r) > MaxMessageRunes {
		body = string(r[:MaxMessageRunes])
	}
	var ref MsgRef
	found, err := kvx.GetJSON(ctx, s.DB, msgRefKey(msgID), &ref)
	if err != nil || !found {
		return Message{}, ErrNotFound
	}
	var out Message
	var oldBody string
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var m Message
		if !getJSONTx(ctx, tx, msgKey(ref.CID, ref.Sort), &m) {
			return ErrNotFound
		}
		if m.Deleted {
			return ErrNotFound
		}
		if !strings.EqualFold(m.Author, author) {
			return ErrAccessDenied
		}
		oldBody = m.Body
		m.Body = body
		m.HTML = RenderMarkdown(body)
		m.EditedTs = time.Now().UTC()
		setJSONTx(tx, msgKey(ref.CID, ref.Sort), m)
		out = m
		return nil
	})
	if err == nil {
		s.clearSearch(ctx, ref.CID, Message{Body: oldBody, Sort: ref.Sort})
		s.writeSearch(ctx, out)
	}
	return out, err
}

// DeleteMessage tombstones a message so scrollback stays contiguous. canMod
// lets a moderator (PermManageMessages) delete another member's message;
// otherwise only the author may.
func (s *Store) DeleteMessage(ctx context.Context, msgID, actor string, canMod bool) error {
	actor = strings.ToLower(actor)
	var ref MsgRef
	found, err := kvx.GetJSON(ctx, s.DB, msgRefKey(msgID), &ref)
	if err != nil || !found {
		return ErrNotFound
	}
	var oldBody string
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var m Message
		if !getJSONTx(ctx, tx, msgKey(ref.CID, ref.Sort), &m) {
			return ErrNotFound
		}
		if !canMod && !strings.EqualFold(m.Author, actor) {
			return ErrAccessDenied
		}
		oldBody = m.Body
		m.Deleted = true
		m.Body = ""
		m.HTML = ""
		m.Attachments = nil
		m.InviteCode = ""
		setJSONTx(tx, msgKey(ref.CID, ref.Sort), m)
		return nil
	})
	if err == nil {
		s.clearSearch(ctx, ref.CID, Message{Body: oldBody, Sort: ref.Sort})
	}
	return err
}
