// search.go — term-AND search over thread meta + cached message search
// text (spec §7.3), with the prefix operators the list pane's search
// box speaks: from: to: label: in: has:file. Scans one mailbox's
// threads (demo scale, like PCD); terms that thread meta can't satisfy
// fall through to the per-message search-text cache.
package mail

import (
	"context"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Query is a parsed search.
type Query struct {
	Terms   []string // free terms, AND semantics, lowercased
	From    string   // from: sender match
	To      string   // to: recipient match
	Label   string   // label: name (resolved against the user's labels)
	In      string   // in: folder (system name, custom folder NAME, or "starred"/"sent")
	HasFile bool     // has:file
}

// ParseQuery splits a search string into operators and free terms.
func ParseQuery(q string) Query {
	var out Query
	for _, f := range strings.Fields(strings.ToLower(q)) {
		switch {
		case strings.HasPrefix(f, "from:"):
			out.From = strings.TrimPrefix(f, "from:")
		case strings.HasPrefix(f, "to:"):
			out.To = strings.TrimPrefix(f, "to:")
		case strings.HasPrefix(f, "label:"):
			out.Label = strings.TrimPrefix(f, "label:")
		case strings.HasPrefix(f, "in:"):
			out.In = strings.TrimPrefix(f, "in:")
		case f == "has:file" || f == "has:attachment":
			out.HasFile = true
		default:
			out.Terms = append(out.Terms, f)
		}
	}
	return out
}

// SearchThreads runs a query over one mailbox, newest-activity first,
// capped at limit.
func (s *Store) SearchThreads(ctx context.Context, user, boxID string, q Query, limit int) ([]ThreadMeta, error) {
	if !kvx.ValidID(boxID) {
		return nil, ErrNotFound
	}
	if limit <= 0 {
		limit = 50
	}
	// label: resolves by name to the label id once.
	labelID := ""
	if q.Label != "" {
		labels, err := s.ListLabels(ctx, user)
		if err != nil {
			return nil, err
		}
		for _, l := range labels {
			if strings.EqualFold(l.Name, q.Label) {
				labelID = l.ID
				break
			}
		}
		if labelID == "" {
			return nil, nil // unknown label matches nothing
		}
	}
	// in: resolves custom folder names to ids.
	folder := q.In
	switch folder {
	case "", FolderInbox, FolderArchive, FolderSpam, FolderTrash, "starred", "sent":
	default:
		folders, err := s.ListFolders(ctx, user, boxID)
		if err != nil {
			return nil, err
		}
		resolved := ""
		for _, f := range folders {
			if strings.EqualFold(f.Name, folder) {
				resolved = f.ID
				break
			}
		}
		if resolved == "" {
			return nil, nil
		}
		folder = resolved
	}

	var out []ThreadMeta
	err := kvx.ScanPrefix(ctx, s.DB, threadsPrefix+user+"/"+boxID+"/", func(_ string, value []byte) error {
		if len(out) >= limit {
			return nil
		}
		var m ThreadMeta
		if jsonUnmarshal(value, &m) != nil {
			return nil
		}
		if !s.threadMatches(ctx, user, boxID, m, q, folder, labelID) {
			return nil
		}
		out = append(out, m)
		return nil
	})
	// Newest activity first (the canonical scan is threadID-ordered).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].LastActivity.After(out[j-1].LastActivity); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, err
}

// threadMatches applies every operator and term to one thread.
func (s *Store) threadMatches(ctx context.Context, user, boxID string, m ThreadMeta, q Query, folder, labelID string) bool {
	switch folder {
	case "":
	case "starred":
		if !m.Starred {
			return false
		}
	case "sent":
		if !m.HasOutbound {
			return false
		}
	default:
		if m.Folder != folder {
			return false
		}
	}
	if labelID != "" {
		found := false
		for _, l := range m.Labels {
			if l == labelID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if q.HasFile && m.AttachCount == 0 {
		return false
	}
	var msgs []MsgMeta // lazy: loaded only when an operator/term needs them
	loadMsgs := func() []MsgMeta {
		if msgs == nil {
			msgs, _ = s.ListThreadMessages(ctx, user, boxID, m.ThreadID)
			if msgs == nil {
				msgs = []MsgMeta{}
			}
		}
		return msgs
	}
	if q.From != "" && !anyMsg(loadMsgs(), func(mm MsgMeta) bool {
		return strings.Contains(strings.ToLower(mm.From), q.From)
	}) {
		return false
	}
	if q.To != "" && !anyMsg(loadMsgs(), func(mm MsgMeta) bool {
		hay := strings.ToLower(strings.Join(mm.To, " ") + " " + strings.Join(mm.Cc, " "))
		return strings.Contains(hay, q.To)
	}) {
		return false
	}
	if len(q.Terms) == 0 {
		return true
	}
	hay := strings.ToLower(m.Subject + " " + m.Snippet + " " + strings.Join(m.Participants, " "))
	body := ""
	for _, term := range q.Terms {
		if strings.Contains(hay, term) {
			continue
		}
		if body == "" {
			var b strings.Builder
			for _, mm := range loadMsgs() {
				text, _ := s.SearchText(ctx, user, mm.BlobID)
				b.WriteString(strings.ToLower(text))
				b.WriteByte('\n')
			}
			body = b.String()
			if body == "" {
				body = "\x00" // sentinel: loaded, empty
			}
		}
		if !strings.Contains(body, term) {
			return false
		}
	}
	return true
}

func anyMsg(msgs []MsgMeta, fn func(MsgMeta) bool) bool {
	for _, m := range msgs {
		if fn(m) {
			return true
		}
	}
	return false
}
