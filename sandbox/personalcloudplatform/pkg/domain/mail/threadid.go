// threadid.go — conversation identity (spec §7.1). Deterministic by
// construction: the same message computes the same threadID on every
// replica and every redelivery, so idempotent intake can never fork a
// conversation.
//
//   - With threading headers: the root Message-ID is References[0]
//     (References lists ancestors oldest-first, RFC 5322 §3.6.4); a
//     message carrying only In-Reply-To uses that parent id. threadID =
//     hash(root id).
//   - Without headers: threadID = hash(normalized subject — Re:/Fwd:
//     prefixes stripped — + the sorted correspondent set), so a
//     headerless reply still meets its conversation.
package mail

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// Domain-separation contexts for the two id derivations.
const (
	threadIDCtx     = "pcp-thread-v1"
	threadSubjCtx   = "pcp-thread-subj-v1"
	threadIDHexLen  = 16 // 64 bits — plenty for one user's mail
	maxThreadRefLen = 998
)

// ThreadKey is the header material thread identity derives from.
type ThreadKey struct {
	MessageID  string   // this message's Message-ID header (may be "")
	InReplyTo  string   // In-Reply-To header (may be "")
	References []string // References header ids, oldest first
	Subject    string
	// Correspondents feed the fallback: every address on From/To/Cc.
	Correspondents []string
}

// ThreadID computes the conversation id for a message.
func ThreadID(k ThreadKey) string {
	if root := rootMessageID(k); root != "" {
		return hashThreadID(threadIDCtx, root)
	}
	return hashThreadID(threadSubjCtx,
		NormalizeSubject(k.Subject)+"\x00"+strings.Join(normalizeCorrespondents(k.Correspondents), ","))
}

// rootMessageID walks References/In-Reply-To to the conversation root.
func rootMessageID(k ThreadKey) string {
	for _, ref := range k.References {
		if id := cleanMsgID(ref); id != "" {
			return id // References is oldest-first: the first entry is the root
		}
	}
	if id := cleanMsgID(k.InReplyTo); id != "" {
		return id
	}
	// A fresh message with a Message-ID roots its own thread ONLY when
	// it also has a subject-less identity problem — otherwise replies
	// that arrive without References but with matching subjects would
	// fork. Root on the Message-ID: replies carry it in References[0].
	return cleanMsgID(k.MessageID)
}

// cleanMsgID normalizes one message-id token: angle brackets stripped,
// whitespace trimmed, lowercased (ids are case-insensitively unique in
// practice), bounded.
func cleanMsgID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" || len(id) > maxThreadRefLen || !strings.Contains(id, "@") {
		return ""
	}
	return id
}

// rePrefix matches reply/forward subject prefixes, with optional
// bracketed counters ("Re:", "RE[2]:", "Fwd:", "FW:").
var rePrefix = regexp.MustCompile(`^(?i:(re|fwd?|fw)(\[\d+\])?:\s*)`)

// NormalizeSubject strips Re:/Fwd: prefixes (repeatedly), collapses
// whitespace, and lowercases — the fallback identity's subject half.
func NormalizeSubject(s string) string {
	s = strings.TrimSpace(s)
	for {
		next := rePrefix.ReplaceAllString(s, "")
		if next == s {
			break
		}
		s = strings.TrimSpace(next)
	}
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// normalizeCorrespondents lowercases, dedups, and sorts the address set.
func normalizeCorrespondents(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range in {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func hashThreadID(ctx, material string) string {
	sum := sha256.Sum256([]byte(ctx + "\x00" + material))
	return hex.EncodeToString(sum[:])[:threadIDHexLen]
}

// ValidThreadID gates thread ids arriving in URLs.
func ValidThreadID(id string) bool {
	return len(id) == threadIDHexLen && isHex(id)
}
