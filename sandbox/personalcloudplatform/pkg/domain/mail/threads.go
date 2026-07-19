// threads.go — the conversation model (spec §7.1). A thread's meta
// record is canonical at /pcp/mail/threads/<user>/<box>/<threadID>;
// every listing surface is a maintained index carrying a full copy of
// the meta (one prefix List renders a folder):
//
//	threadidx/<user>/<box>/<folder>/<invTs>-<threadID>   the ONE folder
//	sentidx/<user>/<box>/<invTs>-<threadID>              Sent facet
//	starred/<user>/<box>/<invTs>-<threadID>              Starred index
//	bylabel/<user>/<labelID>/<invTs>-<threadID>          label listings
//
// Every index id derives deterministically from (LastActivity,
// threadID), so a re-file is always "delete the keys the OLD meta
// implies, write the keys the NEW meta implies, one transaction" — no
// orphan rows, provable by full scan (threads_test.go).
package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// ThreadMeta is one conversation's list-row record: everything the
// three panes need without opening a message.
type ThreadMeta struct {
	ThreadID string `json:"thread_id"`
	BoxID    string `json:"box_id"`
	Subject  string `json:"subject"`
	// Participants are the correspondent addresses seen on the thread
	// (bounded; display resolution is the UI's job).
	Participants []string `json:"participants,omitempty"`
	MsgCount     int      `json:"msg_count"`
	UnreadCount  int      `json:"unread_count,omitempty"`
	Starred      bool     `json:"starred,omitempty"`
	// Labels are labelIDs (labels.go).
	Labels []string `json:"labels,omitempty"`
	// Folder is the ONE folder the thread lives in (system name or
	// custom folder id). Sent is a facet — see HasOutbound.
	Folder       string    `json:"folder"`
	LastActivity time.Time `json:"last_activity"`
	// Snippet previews the latest message.
	Snippet     string `json:"snippet,omitempty"`
	AttachCount int    `json:"attach_count,omitempty"`
	// HasOutbound marks threads containing the user's own messages —
	// the Sent facet maintains sentidx rows for them.
	HasOutbound bool `json:"has_outbound,omitempty"`
	// TrashedAt drives trash retention (set on move-to-trash).
	TrashedAt time.Time `json:"trashed_at,omitzero"`
}

// MsgMeta is one message's row inside a thread, ascending by date
// (threads read top-down).
type MsgMeta struct {
	MsgID    string    `json:"msg_id"`
	ThreadID string    `json:"thread_id"`
	From     string    `json:"from"`
	To       []string  `json:"to,omitempty"`
	Cc       []string  `json:"cc,omitempty"`
	Subject  string    `json:"subject"`
	Date     time.Time `json:"date"`
	Size     int64     `json:"size"`
	BlobID   string    `json:"blob_id"`
	Seen     bool      `json:"seen,omitempty"`
	Starred  bool      `json:"starred,omitempty"`
	// Outbound marks the user's own messages (drives the Sent facet).
	Outbound  bool    `json:"outbound,omitempty"`
	SpamScore float64 `json:"spam_score,omitempty"`
	Snippet   string  `json:"snippet,omitempty"`
	HasAttach bool    `json:"has_attach,omitempty"`
	// ViaDistro names the list that expanded to this delivery.
	ViaDistro string `json:"via_distro,omitempty"`
	// Threading headers, kept for reply composition (References chain).
	MessageIDHdr string   `json:"message_id_hdr,omitempty"`
	References   []string `json:"references,omitempty"`
}

// MsgRef locates a message by its stable id.
type MsgRef struct {
	BoxID    string `json:"box_id"`
	ThreadID string `json:"thread_id"`
	// TsKey is the message row's key suffix under the thread.
	TsKey string `json:"ts_key"`
}

// --- keys ---------------------------------------------------------------------------

// invTs is the deterministic newest-first index timestamp.
func invTs(t time.Time) string {
	return fmt.Sprintf("%020d", uint64(math.MaxInt64-t.UnixNano()))
}

// msgTsKey is the deterministic ASCENDING message key suffix (same
// date + id → same key: the idempotency requirement).
func msgTsKey(date time.Time, msgID string) string {
	suffix := msgID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return fmt.Sprintf("%020d-%s", uint64(date.UnixNano()), suffix)
}

func threadKey(user string, m ThreadMeta) string {
	return threadsPrefix + user + "/" + m.BoxID + "/" + m.ThreadID
}

func msgKey(user, boxID, threadID, tsKey string) string {
	return msgsPrefix + user + "/" + boxID + "/" + threadID + "/" + tsKey
}

// indexKeys derives EVERY index row a meta snapshot implies. Old meta
// in, delete those; new meta in, write those — the whole re-file
// discipline in one function.
func indexKeys(user string, m ThreadMeta) []string {
	id := invTs(m.LastActivity) + "-" + m.ThreadID
	keys := []string{threadIdxPrefix + user + "/" + m.BoxID + "/" + m.Folder + "/" + id}
	if m.HasOutbound {
		keys = append(keys, sentIdxPrefix+user+"/"+m.BoxID+"/"+id)
	}
	if m.Starred {
		keys = append(keys, starredPrefix+user+"/"+m.BoxID+"/"+id)
	}
	for _, l := range m.Labels {
		keys = append(keys, byLabelPrefix+user+"/"+l+"/"+id)
	}
	return keys
}

// stageThreadUpdate deletes the old meta's index rows and writes the
// new meta everywhere (canonical + indexes) on the caller's
// transaction. old == nil means the thread is new.
func stageThreadUpdate(tx *client.Tx, user string, old *ThreadMeta, next ThreadMeta) {
	if old != nil {
		for _, k := range indexKeys(user, *old) {
			tx.Delete(k)
		}
	}
	raw, _ := json.Marshal(next)
	tx.Set(threadKey(user, next), raw)
	for _, k := range indexKeys(user, next) {
		tx.Set(k, raw)
	}
}

// stageThreadDelete removes the canonical record and every index row.
func stageThreadDelete(tx *client.Tx, user string, m ThreadMeta) {
	tx.Delete(threadKey(user, m))
	for _, k := range indexKeys(user, m) {
		tx.Delete(k)
	}
}

// mutateThread is the shared read-modify-write for one thread: loads
// the meta in a transaction, applies mutate, re-files every index.
func (s *Store) mutateThread(ctx context.Context, user, boxID, threadID string, mutate func(*ThreadMeta) error) error {
	if !kvx.ValidID(boxID) || !ValidThreadID(threadID) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		key := threadsPrefix + user + "/" + boxID + "/" + threadID
		raw, found, err := tx.Get(ctx, key)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var m ThreadMeta
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		old := m.snapshot()
		if err := mutate(&m); err != nil {
			return err
		}
		stageThreadUpdate(tx, user, &old, m)
		return nil
	})
}

// snapshot deep-copies the meta's slices, so an index-key derivation
// over the OLD state stays correct after mutate reorders or rewrites
// the shared backing arrays (append/sort/delete all may).
func (m ThreadMeta) snapshot() ThreadMeta {
	c := m
	c.Labels = append([]string(nil), m.Labels...)
	c.Participants = append([]string(nil), m.Participants...)
	return c
}

// --- reads --------------------------------------------------------------------------

// GetThread loads one thread's meta.
func (s *Store) GetThread(ctx context.Context, user, boxID, threadID string) (ThreadMeta, bool, error) {
	if !kvx.ValidID(boxID) || !ValidThreadID(threadID) {
		return ThreadMeta{}, false, nil
	}
	var m ThreadMeta
	found, err := kvx.GetJSON(ctx, s.DB, threadsPrefix+user+"/"+boxID+"/"+threadID, &m)
	return m, found, err
}

// listIndex pages one index prefix newest-first (the key order).
func (s *Store) listIndex(ctx context.Context, prefix, cursor string, limit int) ([]ThreadMeta, string, error) {
	if limit <= 0 {
		limit = 50
	}
	c := ""
	if cursor != "" {
		c = prefix + cursor
	}
	entries, next, err := s.DB.List(ctx, prefix, c, limit)
	if err != nil {
		return nil, "", err
	}
	var out []ThreadMeta
	for _, e := range entries {
		var m ThreadMeta
		if json.Unmarshal(e.Value, &m) == nil {
			out = append(out, m)
		}
	}
	nextID := ""
	if next != "" {
		nextID = strings.TrimPrefix(next, prefix)
	}
	return out, nextID, nil
}

// ListThreads pages one folder, newest-activity first. Listing Trash
// lazily prunes threads past the retention window first (best-effort).
func (s *Store) ListThreads(ctx context.Context, user, boxID, folder, cursor string, limit int, trashDays int) ([]ThreadMeta, string, error) {
	if !ValidFolder(folder) || !kvx.ValidID(boxID) {
		return nil, "", ErrNotFound
	}
	if folder == FolderTrash && trashDays > 0 {
		s.pruneTrash(ctx, user, boxID, trashDays)
	}
	return s.listIndex(ctx, threadIdxPrefix+user+"/"+boxID+"/"+folder+"/", cursor, limit)
}

// ListSent pages the Sent facet (threads containing your messages).
func (s *Store) ListSent(ctx context.Context, user, boxID, cursor string, limit int) ([]ThreadMeta, string, error) {
	if !kvx.ValidID(boxID) {
		return nil, "", ErrNotFound
	}
	return s.listIndex(ctx, sentIdxPrefix+user+"/"+boxID+"/", cursor, limit)
}

// ListStarred pages the Starred index.
func (s *Store) ListStarred(ctx context.Context, user, boxID, cursor string, limit int) ([]ThreadMeta, string, error) {
	if !kvx.ValidID(boxID) {
		return nil, "", ErrNotFound
	}
	return s.listIndex(ctx, starredPrefix+user+"/"+boxID+"/", cursor, limit)
}

// ListByLabel pages one label's threads.
func (s *Store) ListByLabel(ctx context.Context, user, labelID, cursor string, limit int) ([]ThreadMeta, string, error) {
	if !kvx.ValidID(labelID) {
		return nil, "", ErrNotFound
	}
	return s.listIndex(ctx, byLabelPrefix+user+"/"+labelID+"/", cursor, limit)
}

// ListThreadMessages returns a thread's messages oldest-first (the key
// order — threads read top-down).
func (s *Store) ListThreadMessages(ctx context.Context, user, boxID, threadID string) ([]MsgMeta, error) {
	if !kvx.ValidID(boxID) || !ValidThreadID(threadID) {
		return nil, ErrNotFound
	}
	var out []MsgMeta
	err := kvx.ScanPrefix(ctx, s.DB, msgsPrefix+user+"/"+boxID+"/"+threadID+"/", func(_ string, value []byte) error {
		var m MsgMeta
		if json.Unmarshal(value, &m) == nil {
			out = append(out, m)
		}
		return nil
	})
	return out, err
}

// GetMessage loads one message's meta by stable id.
func (s *Store) GetMessage(ctx context.Context, user, msgID string) (MsgMeta, MsgRef, bool, error) {
	var ref MsgRef
	if !kvx.ValidID(msgID) {
		return MsgMeta{}, ref, false, nil
	}
	found, err := kvx.GetJSON(ctx, s.DB, msgRefPrefix+user+"/"+msgID, &ref)
	if err != nil || !found {
		return MsgMeta{}, ref, false, err
	}
	var meta MsgMeta
	found, err = kvx.GetJSON(ctx, s.DB, msgKey(user, ref.BoxID, ref.ThreadID, ref.TsKey), &meta)
	return meta, ref, found, err
}

// UnreadThreads counts a folder's unread threads, bounded (the sidebar
// badge shows 999 as its ceiling rather than paying for a census).
func (s *Store) UnreadThreads(ctx context.Context, user, boxID, folder string) (int, error) {
	rows, _, err := s.ListThreads(ctx, user, boxID, folder, "", 999, 0)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range rows {
		if m.UnreadCount > 0 {
			n++
		}
	}
	return n, nil
}

// --- flags --------------------------------------------------------------------------

// MarkThreadRead flips every message's Seen flag and zeroes (or
// restores) the thread's unread rollup.
func (s *Store) MarkThreadRead(ctx context.Context, user, boxID, threadID string, read bool) error {
	msgs, err := s.ListThreadMessages(ctx, user, boxID, threadID)
	if err != nil {
		return err
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		unread := 0
		for _, m := range msgs {
			if m.Seen != read {
				m.Seen = read
				raw, _ := json.Marshal(m)
				tx.Set(msgKey(user, boxID, threadID, msgTsKey(m.Date, m.MsgID)), raw)
			}
			if !read && !m.Outbound {
				unread++
			}
		}
		key := threadsPrefix + user + "/" + boxID + "/" + threadID
		raw, found, err := tx.Get(ctx, key)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var m ThreadMeta
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		old := m.snapshot()
		m.UnreadCount = unread
		stageThreadUpdate(tx, user, &old, m)
		return nil
	})
}

// SetThreadStarred stars/unstars a thread (maintains the Starred
// index).
func (s *Store) SetThreadStarred(ctx context.Context, user, boxID, threadID string, on bool) error {
	return s.mutateThread(ctx, user, boxID, threadID, func(m *ThreadMeta) error {
		m.Starred = on
		return nil
	})
}

// SetMessageStarred stars one message (per-message star, spec §7.1 —
// independent of the thread star).
func (s *Store) SetMessageStarred(ctx context.Context, user, msgID string, on bool) error {
	return s.MutateMessage(ctx, user, msgID, func(m *MsgMeta) { m.Starred = on })
}

// MutateMessage rewrites one message's meta in place (flags, the
// bounce marker). The thread meta is untouched — callers that change
// rollup-relevant fields use the thread-level operations instead.
func (s *Store) MutateMessage(ctx context.Context, user, msgID string, mutate func(*MsgMeta)) error {
	_, ref, found, err := s.GetMessage(ctx, user, msgID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	key := msgKey(user, ref.BoxID, ref.ThreadID, ref.TsKey)
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, key)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var m MsgMeta
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		mutate(&m)
		out, _ := json.Marshal(m)
		tx.Set(key, out)
		return nil
	})
}

// --- moves, purge, retention ---------------------------------------------------------

// MoveThread relocates a thread to another folder in one transaction
// (all indexes re-file; messages and blobs never move). Moving to
// Trash stamps TrashedAt for retention; moving out clears it.
func (s *Store) MoveThread(ctx context.Context, user, boxID, threadID, toFolder string) error {
	if !ValidFolder(toFolder) {
		return ErrNotFound
	}
	return s.mutateThread(ctx, user, boxID, threadID, func(m *ThreadMeta) error {
		if m.Folder == toFolder {
			return nil
		}
		m.Folder = toFolder
		if toFolder == FolderTrash {
			m.TrashedAt = time.Now().UTC()
		} else {
			m.TrashedAt = time.Time{}
		}
		return nil
	})
}

// PurgeThread permanently deletes a conversation: message rows, refs,
// blobs, search text, every index, the meta — and refunds quota.
func (s *Store) PurgeThread(ctx context.Context, user, boxID, threadID string) error {
	meta, found, err := s.GetThread(ctx, user, boxID, threadID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	msgs, err := s.ListThreadMessages(ctx, user, boxID, threadID)
	if err != nil {
		return err
	}
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		for _, m := range msgs {
			tx.Delete(msgKey(user, boxID, threadID, msgTsKey(m.Date, m.MsgID)))
			tx.Delete(msgRefPrefix + user + "/" + m.MsgID)
		}
		stageThreadDelete(tx, user, meta)
		return nil
	})
	if err != nil {
		return err
	}
	var refund int64
	for _, m := range msgs {
		_ = s.DB.DeleteBlob(ctx, blobsPrefix+user+"/"+m.BlobID)
		_ = s.DB.Delete(ctx, searchPrefix+user+"/"+m.BlobID)
		refund += m.Size
	}
	if refund > 0 {
		s.creditQuota(ctx, user, refund)
	}
	return nil
}

// EmptyTrash purges every trashed thread in a mailbox.
func (s *Store) EmptyTrash(ctx context.Context, user, boxID string) error {
	for {
		rows, _, err := s.ListThreads(ctx, user, boxID, FolderTrash, "", 100, 0)
		if err != nil || len(rows) == 0 {
			return err
		}
		for _, m := range rows {
			if err := s.PurgeThread(ctx, user, boxID, m.ThreadID); err != nil && err != ErrNotFound {
				return err
			}
		}
	}
}

// pruneTrash is the lazy retention sweep: threads trashed before the
// cutoff purge on the way into a Trash listing. Best-effort — a prune
// failure never fails the listing.
func (s *Store) pruneTrash(ctx context.Context, user, boxID string, trashDays int) {
	cutoff := time.Now().AddDate(0, 0, -trashDays)
	rows, _, err := s.listIndex(ctx, threadIdxPrefix+user+"/"+boxID+"/"+FolderTrash+"/", "", 200)
	if err != nil {
		return
	}
	for _, m := range rows {
		if !m.TrashedAt.IsZero() && m.TrashedAt.Before(cutoff) {
			_ = s.PurgeThread(ctx, user, boxID, m.ThreadID)
		}
	}
}

// creditQuota refunds bytes (best-effort).
func (s *Store) creditQuota(ctx context.Context, user string, size int64) {
	_ = s.Users.ChargeQuota(ctx, user, -size, 0)
}

// --- custom folders -------------------------------------------------------------------

// Folder is one custom folder.
type Folder struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// maxCustomFolders bounds the sidebar (spec §7.1: up to 50 custom).
const maxCustomFolders = 50

// CreateFolder adds a custom folder to a mailbox.
func (s *Store) CreateFolder(ctx context.Context, user, boxID, name string) (Folder, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return Folder{}, fmt.Errorf("folder names are 1–60 characters")
	}
	if systemFolder(strings.ToLower(name)) {
		return Folder{}, fmt.Errorf("that folder already exists")
	}
	if !kvx.ValidID(boxID) {
		return Folder{}, ErrNotFound
	}
	existing, err := s.ListFolders(ctx, user, boxID)
	if err != nil {
		return Folder{}, err
	}
	if len(existing) >= maxCustomFolders {
		return Folder{}, fmt.Errorf("at most %d folders", maxCustomFolders)
	}
	for _, f := range existing {
		if strings.EqualFold(f.Name, name) {
			return Folder{}, fmt.Errorf("a folder named %q already exists", name)
		}
	}
	f := Folder{ID: kvx.NewID(), Name: name}
	return f, kvx.SetJSON(ctx, s.DB, foldersPrefix+user+"/"+boxID+"/"+f.ID, f)
}

// DeleteFolder removes a custom folder, moving its threads to Archive
// first (they must not orphan under a dead folder id).
func (s *Store) DeleteFolder(ctx context.Context, user, boxID, folderID string) error {
	if !kvx.ValidID(folderID) {
		return ErrNotFound
	}
	for {
		rows, _, err := s.ListThreads(ctx, user, boxID, folderID, "", 100, 0)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		for _, m := range rows {
			if err := s.MoveThread(ctx, user, boxID, m.ThreadID, FolderArchive); err != nil {
				return err
			}
		}
	}
	return s.DB.Delete(ctx, foldersPrefix+user+"/"+boxID+"/"+folderID)
}

// ListFolders lists a mailbox's custom folders, name-sorted.
func (s *Store) ListFolders(ctx context.Context, user, boxID string) ([]Folder, error) {
	var out []Folder
	err := kvx.ScanPrefix(ctx, s.DB, foldersPrefix+user+"/"+boxID+"/", func(_ string, value []byte) error {
		var f Folder
		if json.Unmarshal(value, &f) == nil {
			out = append(out, f)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, err
}
