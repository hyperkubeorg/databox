// deliver.go — the delivery pipeline (spec §7.1): parse → threadID →
// one transaction that writes the message row, upserts the thread
// meta, and re-files every index — plus the blob, search text, quota
// charge, and notification around it.
//
// Idempotent on MsgID: intake retries, replica races, and duplicate
// spool entries re-derive the same deterministic id, hit the msgref
// existence check, and write nothing.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
)

// maxParticipants bounds a thread's stored correspondent set.
const maxParticipants = 24

// Delivery is one message landing in one mailbox.
type Delivery struct {
	User   string
	BoxID  string
	Folder string // inbox or spam for intake; ignored for existing threads (see folder rules)
	Meta   MsgMeta
	Raw    []byte
	// SearchText caches alongside the blob ("" skips).
	SearchText string
	// Quota is the owner's effective storage quota (0 = unlimited).
	Quota int64
	// Notify fires the new-mail notification (skipped for sent copies).
	Notify bool
}

// errAlreadyDelivered short-circuits the transaction on a replay.
var errAlreadyDelivered = errNotError("already delivered")

type errNotError string

func (e errNotError) Error() string { return string(e) }

// Deliver writes one message into a mailbox's threads. The thread id
// must already be set on Meta.ThreadID (compute it with ThreadID over
// the parse — send and intake both do). Replays return nil having
// written nothing.
func (s *Store) Deliver(ctx context.Context, d Delivery) error {
	if d.Meta.MsgID == "" || d.Meta.ThreadID == "" || !ValidFolder(d.Folder) || !kvx.ValidID(d.BoxID) {
		return errNotError("bad delivery")
	}
	refKey := msgRefPrefix + d.User + "/" + d.Meta.MsgID
	if _, exists, err := s.DB.Get(ctx, refKey); err != nil {
		return err
	} else if exists {
		return nil // already delivered
	}
	d.Meta.BlobID = d.Meta.MsgID
	d.Meta.Size = int64(len(d.Raw))
	if d.Meta.Date.IsZero() {
		d.Meta.Date = time.Now()
	}
	if err := s.Users.ChargeQuota(ctx, d.User, d.Meta.Size, d.Quota); err != nil {
		return err
	}
	if err := s.DB.PutBlob(ctx, blobsPrefix+d.User+"/"+d.Meta.BlobID, bytes.NewReader(d.Raw), "message/rfc822"); err != nil {
		s.creditQuota(ctx, d.User, d.Meta.Size)
		return err
	}
	s.putSearchText(ctx, d.User, d.Meta.BlobID, d.SearchText)

	tsKey := msgTsKey(d.Meta.Date, d.Meta.MsgID)
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, exists, err := tx.Get(ctx, refKey); err != nil {
			return err
		} else if exists {
			return errAlreadyDelivered
		}
		// Load (or birth) the thread meta and fold the message in.
		tKey := threadsPrefix + d.User + "/" + d.BoxID + "/" + d.Meta.ThreadID
		var old *ThreadMeta
		next := ThreadMeta{ThreadID: d.Meta.ThreadID, BoxID: d.BoxID, Folder: d.Folder}
		if raw, found, err := tx.Get(ctx, tKey); err != nil {
			return err
		} else if found {
			var m ThreadMeta
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			prev := m.snapshot()
			old, next = &prev, m
		}
		foldMessage(&next, d)
		mraw, _ := json.Marshal(d.Meta)
		tx.Set(msgKey(d.User, d.BoxID, d.Meta.ThreadID, tsKey), mraw)
		rraw, _ := json.Marshal(MsgRef{BoxID: d.BoxID, ThreadID: d.Meta.ThreadID, TsKey: tsKey})
		tx.Set(refKey, rraw)
		stageThreadUpdate(tx, d.User, old, next)
		return nil
	})
	if err != nil {
		// A racing delivery won: its blob is byte-identical (same id ⇒
		// same content), so only our quota charge needs undoing.
		s.creditQuota(ctx, d.User, d.Meta.Size)
		if err == errAlreadyDelivered || kvx.IsConflict(err) {
			return nil
		}
		return err
	}
	if d.Notify && s.Notify != nil {
		nctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = s.Notify.Notify(nctx, d.User, notify.Notification{
			Kind: notify.KindMail, At: time.Now(), From: d.Meta.From,
			Text: "New mail: " + d.Meta.Subject,
			URL:  "/mail?box=" + d.BoxID + "&thread=" + d.Meta.ThreadID,
		})
	}
	return nil
}

// foldMessage merges one message into a thread meta (new or existing).
// Folder rules for existing threads: an inbound inbox delivery pulls an
// archived or trashed thread BACK to the inbox (Gmail semantics — new
// activity resurfaces the conversation); a spam-tagged delivery never
// moves an established thread; otherwise the thread keeps its folder.
func foldMessage(m *ThreadMeta, d Delivery) {
	existing := m.MsgCount > 0
	msg := d.Meta
	if existing && d.Folder == FolderInbox && (m.Folder == FolderArchive || m.Folder == FolderTrash) {
		m.Folder = FolderInbox
		m.TrashedAt = time.Time{}
	}
	m.MsgCount++
	if !msg.Seen && !msg.Outbound {
		m.UnreadCount++
	}
	if msg.Outbound {
		m.HasOutbound = true
	}
	if msg.HasAttach {
		m.AttachCount++
	}
	if m.Subject == "" {
		m.Subject = msg.Subject
	}
	if !msg.Date.Before(m.LastActivity) {
		m.LastActivity = msg.Date
		m.Snippet = msg.Snippet
		if msg.Subject != "" && !existing {
			m.Subject = msg.Subject
		}
	}
	m.Participants = mergeParticipants(m.Participants, msg.From, msg.To, msg.Cc)
}

// mergeParticipants folds new correspondents into the bounded set.
func mergeParticipants(have []string, from string, to, cc []string) []string {
	seen := map[string]bool{}
	for _, p := range have {
		seen[p] = true
	}
	add := func(v string) {
		if v == "" || seen[v] || len(have) >= maxParticipants {
			return
		}
		seen[v] = true
		have = append(have, v)
	}
	add(from)
	for _, v := range to {
		add(v)
	}
	for _, v := range cc {
		add(v)
	}
	return have
}

// MessageBlob reads one raw RFC 822 message (the phase-4 render seam:
// the reading pane parses these bytes; attachment downloads stream
// them).
func (s *Store) MessageBlob(ctx context.Context, user, blobID string) ([]byte, error) {
	var buf bytes.Buffer
	if err := s.DB.GetBlob(ctx, blobsPrefix+user+"/"+blobID, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
