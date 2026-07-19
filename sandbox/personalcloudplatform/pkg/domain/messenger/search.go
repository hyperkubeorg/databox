// search.go — the maintained inverted index (Messenger §9). Each
// message write emits one posting per unique term (value = author, for
// from: filtering) plus an author-index row; queries (M5) are index reads,
// never scans. This file carries the write side, called from deliver; the
// query side and facets layer on top in the search phase.
package messenger

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// maxTermsPerMsg caps unique index terms per message (a runaway paste
// shouldn't write thousands of postings). The cap is logged when hit.
const maxTermsPerMsg = 200

// minTermLen skips 1-character noise.
const minTermLen = 2

func searchKey(cid, term, sort string) string {
	return searchPrefix + cid + "/" + term + "/" + sort
}
func authorIdxKey(cid, author, sort string) string {
	return authorIdxPrefix + cid + "/" + strings.ToLower(author) + "/" + sort
}

// tokenize lowercases and splits a body into distinct, index-safe terms
// (letters/digits runs), skipping stopwords and 1-char noise.
func tokenize(body string) []string {
	fields := strings.FieldsFunc(strings.ToLower(body), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < minTermLen || stopwords[f] || !kvx.ValidTokenChars(f) {
			continue
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// writeSearch emits a message's term postings and author-index row. The
// posting value carries the author so a term+from: query needs no join.
// Best-effort: a missing posting only hides a message from search.
func (s *Store) writeSearch(ctx context.Context, m Message) {
	if m.Body == "" {
		// Still index by author so from: queries find attachment/invite posts.
		_ = kvx.SetJSON(ctx, s.DB, authorIdxKey(m.CID, m.Author, m.Sort), struct{}{})
		return
	}
	terms := tokenize(m.Body)
	if len(terms) > maxTermsPerMsg {
		if s.Log != nil {
			s.Log.Warn("messenger search terms capped", "cid", m.CID, "msg", m.ID, "terms", len(terms), "cap", maxTermsPerMsg)
		}
		terms = terms[:maxTermsPerMsg]
	}
	for _, t := range terms {
		_ = kvx.SetJSON(ctx, s.DB, searchKey(m.CID, t, m.Sort), map[string]string{"a": m.Author})
	}
	_ = kvx.SetJSON(ctx, s.DB, authorIdxKey(m.CID, m.Author, m.Sort), struct{}{})
}

// clearSearch removes a message's postings (on delete/edit re-index). It
// scans the message's own terms — cheap and exact.
func (s *Store) clearSearch(ctx context.Context, cid string, m Message) {
	for _, t := range tokenize(m.Body) {
		_ = s.DB.Delete(ctx, searchKey(cid, t, m.Sort))
	}
}

// --- query side -------------------------------------------------------------

// Search scopes.
const (
	ScopeChannel = "channel" // one channel (needs CID)
	ScopeServer  = "server"  // all viewable channels in a server (needs ServerID)
	ScopeAll     = "all"     // every conversation the viewer is in
	ScopeDMs     = "dms"     // the viewer's DMs and group DMs
)

// SearchScope bounds a search to a set of conversations.
type SearchScope struct {
	Kind     string
	ServerID string
	CID      string
}

// Query is a parsed search: free terms plus the from:/has:/before:/after:
// operators.
type Query struct {
	Terms   []string
	From    string
	HasFile bool
	Before  time.Time
	After   time.Time
}

// Empty reports whether a query has nothing to match on.
func (q Query) Empty() bool {
	return len(q.Terms) == 0 && q.From == "" && !q.HasFile && q.Before.IsZero() && q.After.IsZero()
}

// ParseQuery reads a raw query string into terms + operators.
func ParseQuery(raw string) Query {
	var q Query
	for _, tok := range strings.Fields(raw) {
		low := strings.ToLower(tok)
		switch {
		case strings.HasPrefix(low, "from:"):
			q.From = strings.ToLower(strings.TrimPrefix(tok, "from:"))
			q.From = strings.TrimPrefix(q.From, "@")
		case low == "has:file" || low == "has:attachment":
			q.HasFile = true
		case strings.HasPrefix(low, "before:"):
			if d, err := time.Parse("2006-01-02", strings.TrimPrefix(low, "before:")); err == nil {
				q.Before = d
			}
		case strings.HasPrefix(low, "after:"):
			if d, err := time.Parse("2006-01-02", strings.TrimPrefix(low, "after:")); err == nil {
				q.After = d
			}
		case strings.HasPrefix(low, "in:"), strings.HasPrefix(low, "server:"):
			// Scope operators are honored by the caller's scope selection.
		default:
			for _, t := range tokenize(tok) {
				q.Terms = append(q.Terms, t)
			}
		}
	}
	return q
}

// SearchHit is one matched message with light context.
type SearchHit struct {
	Message
	ServerID string `json:"server_id,omitempty"`
	Where    string `json:"where"` // "#channel" or a DM/group name
}

// Search runs a parsed query over a scope, returning matches newest-first.
// Only conversations the viewer may see are consulted (privacy). It is an
// index read (term postings / author index), never a full scan.
func (s *Store) Search(ctx context.Context, viewer users.User, scope SearchScope, raw string, limit int) ([]SearchHit, error) {
	q := ParseQuery(raw)
	if q.Empty() || (len(q.Terms) == 0 && q.From == "") {
		return nil, nil // need at least a term or from:
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	cids, where, servers, err := s.scopeConvos(ctx, viewer, scope)
	if err != nil {
		return nil, err
	}
	var hits []SearchHit
	for _, cid := range cids {
		sorts := s.candidateSorts(ctx, cid, q)
		for _, sortID := range sorts {
			var m Message
			if found, _ := kvx.GetJSON(ctx, s.DB, msgKey(cid, sortID), &m); !found || m.Deleted {
				continue
			}
			if !matches(m, q) {
				continue
			}
			hits = append(hits, SearchHit{Message: m, ServerID: servers[cid], Where: where[cid]})
			if len(hits) >= limit*4 {
				break
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Ts.After(hits[j].Ts) })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// candidateSorts returns candidate message sort-keys for a conversation:
// postings of the first term, else the author index, newest-first.
func (s *Store) candidateSorts(ctx context.Context, cid string, q Query) []string {
	var prefix string
	if len(q.Terms) > 0 {
		prefix = searchKey(cid, q.Terms[0], "")
	} else {
		prefix = authorIdxKey(cid, q.From, "")
	}
	var out []string
	entries, _, err := s.DB.List(ctx, prefix, "", 400)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		out = append(out, strings.TrimPrefix(e.Key, prefix))
	}
	return out
}

// matches applies the full query to a loaded message (the postings gate on
// the first term; remaining terms/operators are verified here).
func matches(m Message, q Query) bool {
	if q.From != "" && !strings.EqualFold(m.Author, q.From) {
		return false
	}
	if q.HasFile && len(m.Attachments) == 0 {
		return false
	}
	if !q.After.IsZero() && m.Ts.Before(q.After) {
		return false
	}
	if !q.Before.IsZero() && m.Ts.After(q.Before) {
		return false
	}
	if len(q.Terms) > 0 {
		body := strings.ToLower(m.Body)
		for _, t := range q.Terms {
			if !strings.Contains(body, t) {
				return false
			}
		}
	}
	return true
}

// scopeConvos resolves a scope to the conversation ids the viewer may
// search, plus per-cid display context (where) and server id.
func (s *Store) scopeConvos(ctx context.Context, viewer users.User, scope SearchScope) (cids []string, where, servers map[string]string, err error) {
	where, servers = map[string]string{}, map[string]string{}
	addChannel := func(serverID string, ch Channel) {
		if ok, _ := s.CanViewChannel(ctx, viewer, ch); ok {
			cids = append(cids, ch.ID)
			where[ch.ID] = "#" + ch.Name
			servers[ch.ID] = serverID
		}
	}
	addServer := func(serverID string) error {
		chs, e := s.Channels(ctx, serverID)
		if e != nil {
			return e
		}
		for _, ch := range chs {
			addChannel(serverID, ch)
		}
		return nil
	}
	addDMs := func() error {
		convos, e := s.UserConvos(ctx, viewer.Username)
		if e != nil {
			return e
		}
		for _, c := range convos {
			cids = append(cids, c.CID)
			if c.Kind == ConvoGroup {
				where[c.CID] = c.Name
			} else {
				where[c.CID] = "@" + c.Other
			}
		}
		return nil
	}

	switch scope.Kind {
	case ScopeChannel:
		ch, found, e := s.GetChannel(ctx, scope.ServerID, scope.CID)
		if e != nil || !found {
			return nil, where, servers, e
		}
		addChannel(scope.ServerID, ch)
	case ScopeServer:
		err = addServer(scope.ServerID)
	case ScopeDMs:
		err = addDMs()
	default: // ScopeAll
		ms, e := s.UserServers(ctx, viewer.Username)
		if e != nil {
			return nil, where, servers, e
		}
		for _, m := range ms {
			if e := addServer(m.ServerID); e != nil {
				return nil, where, servers, e
			}
		}
		err = addDMs()
	}
	return cids, where, servers, err
}

// stopwords are the common terms not worth indexing.
var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "to": true,
	"of": true, "in": true, "on": true, "at": true, "for": true, "it": true,
	"this": true, "that": true, "with": true, "as": true, "by": true, "i": true,
	"you": true, "he": true, "she": true, "we": true, "they": true,
}
