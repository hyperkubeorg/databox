// dms.go — direct messages and group DMs (Messenger §8). A 1:1 DM's
// cid is the deterministic dm_<lo>_<hi>, so either party derives it and the
// conversation is created lazily on first open. A group DM has a random
// g<id>, an explicit roster, and an editable name. Each participant holds a
// membership+list row (/pcp/msg/dms/<user>/<cid> → DMRef) sorted by the
// conversation's LastMsgTs at read time.
package messenger

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// DMRef is one participant's entry in their DM/group list.
type DMRef struct {
	CID     string   `json:"cid"`
	Kind    string   `json:"kind"`            // dm | group
	Other   string   `json:"other,omitempty"` // dm: the other user
	Name    string   `json:"name,omitempty"`  // group: display name
	Members []string `json:"members,omitempty"`
}

func dmRefKey(user, cid string) string { return dmsPrefix + strings.ToLower(user) + "/" + cid }
func groupMemberKey(cid, user string) string {
	return groupMembersPrefix + cid + "/" + strings.ToLower(user)
}

// DMCid is the deterministic conversation id for a 1:1 DM.
func DMCid(a, b string) string {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a > b {
		a, b = b, a
	}
	return "dm_" + a + "_" + b
}

// OpenDM ensures a 1:1 DM conversation exists between two users and returns
// its cid. Idempotent; refuses self-DMs and unknown accounts.
func (s *Store) OpenDM(ctx context.Context, user, other string) (string, error) {
	user, other = strings.ToLower(user), strings.ToLower(other)
	if user == other {
		return "", fmt.Errorf("you can't DM yourself")
	}
	if users.ValidUsername(other) != nil {
		return "", users.ErrNotFound
	}
	if ok, err := s.userExists(ctx, other); err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("no account named %q", other)
	}
	cid := DMCid(user, other)
	now := time.Now().UTC()
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		ensureConvoTx(ctx, tx, cid, ConvoDM, "", now)
		setJSONTx(tx, dmRefKey(user, cid), DMRef{CID: cid, Kind: ConvoDM, Other: other})
		setJSONTx(tx, dmRefKey(other, cid), DMRef{CID: cid, Kind: ConvoDM, Other: user})
		return nil
	})
	return cid, err
}

// CreateGroup makes a group DM with the creator plus the given members and
// returns its cid. Members are validated; the creator is always included.
func (s *Store) CreateGroup(ctx context.Context, creator string, members []string, name string) (string, error) {
	creator = strings.ToLower(creator)
	roster := map[string]bool{creator: true}
	for _, m := range members {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "" || users.ValidUsername(m) != nil {
			continue
		}
		if ok, err := s.userExists(ctx, m); err != nil {
			return "", err
		} else if ok {
			roster[m] = true
		}
	}
	if len(roster) < 2 {
		return "", fmt.Errorf("a group needs at least one other person")
	}
	list := make([]string, 0, len(roster))
	for u := range roster {
		list = append(list, u)
	}
	sort.Strings(list)
	name = strings.TrimSpace(name)
	if name == "" {
		name = groupDefaultName(list)
	}
	cid := "g" + kvx.NewID()
	now := time.Now().UTC()
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		setJSONTx(tx, convoKey(cid), Convo{ID: cid, Kind: ConvoGroup, Name: name, CreatedAt: now, LastMsgTs: now})
		for _, u := range list {
			tx.Set(groupMemberKey(cid, u), []byte("{}"))
			setJSONTx(tx, dmRefKey(u, cid), DMRef{CID: cid, Kind: ConvoGroup, Name: name, Members: list})
		}
		return nil
	})
	return cid, err
}

// LeaveGroup removes a user from a group DM (their membership + list row).
func (s *Store) LeaveGroup(ctx context.Context, cid, user string) error {
	user = strings.ToLower(user)
	if !strings.HasPrefix(cid, "g") {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		tx.Delete(groupMemberKey(cid, user))
		tx.Delete(dmRefKey(user, cid))
		return nil
	})
}

// IsConvoMember reports whether a user participates in a DM/group (or, for a
// channel cid, is a member of its server — resolved by the caller). For
// DMs/groups it checks the participant row.
func (s *Store) IsConvoMember(ctx context.Context, cid, user string) (bool, error) {
	var ref DMRef
	return kvx.GetJSON(ctx, s.DB, dmRefKey(user, cid), &ref)
}

// SendDM posts to a 1:1 DM (opening it if needed).
func (s *Store) SendDM(ctx context.Context, sender users.User, other, body string, opts SendOpts) (Message, error) {
	cid, err := s.OpenDM(ctx, sender.Username, other)
	if err != nil {
		return Message{}, err
	}
	return s.deliver(ctx, cid, ConvoDM, "", sender, body, []string{strings.ToLower(sender.Username), strings.ToLower(other)}, false, opts)
}

// SendGroup posts to a group DM the sender belongs to.
func (s *Store) SendGroup(ctx context.Context, sender users.User, cid, body string, opts SendOpts) (Message, error) {
	if ok, err := s.IsConvoMember(ctx, cid, sender.Username); err != nil || !ok {
		return Message{}, ErrAccessDenied
	}
	roster, err := s.GroupMembers(ctx, cid)
	if err != nil {
		return Message{}, err
	}
	return s.deliver(ctx, cid, ConvoGroup, "", sender, body, roster, false, opts)
}

// SendToConvo posts to an existing DM or group DM identified by cid,
// enforcing participation. (Channel sends go through SendToChannel.)
func (s *Store) SendToConvo(ctx context.Context, sender users.User, cid, body string, opts SendOpts) (Message, error) {
	if ok, err := s.IsConvoMember(ctx, cid, sender.Username); err != nil || !ok {
		return Message{}, ErrAccessDenied
	}
	switch {
	case strings.HasPrefix(cid, "g"):
		return s.SendGroup(ctx, sender, cid, body, opts)
	case strings.HasPrefix(cid, "dm_"):
		other := otherDM(cid, sender.Username)
		if other == "" {
			return Message{}, ErrNotFound
		}
		return s.deliver(ctx, cid, ConvoDM, "", sender,
			body, []string{strings.ToLower(sender.Username), other}, false, opts)
	}
	return Message{}, ErrNotFound
}

// otherDM returns the other participant of a 1:1 DM cid (dm_<lo>_<hi>).
// Usernames contain no underscore (kvx.ValidKeyName), so the split is
// unambiguous.
func otherDM(cid, user string) string {
	rest := strings.TrimPrefix(cid, "dm_")
	parts := strings.SplitN(rest, "_", 2)
	if len(parts) != 2 {
		return ""
	}
	user = strings.ToLower(user)
	if parts[0] == user {
		return parts[1]
	}
	if parts[1] == user {
		return parts[0]
	}
	return ""
}

// GroupMembers lists a group DM's roster.
func (s *Store) GroupMembers(ctx context.Context, cid string) ([]string, error) {
	var out []string
	err := kvx.ScanPrefix(ctx, s.DB, groupMembersPrefix+cid+"/", func(key string, _ []byte) error {
		out = append(out, key[strings.LastIndex(key, "/")+1:])
		return nil
	})
	return out, err
}

// ConvoEntry is a DM/group in a user's list, resolved with last activity.
type ConvoEntry struct {
	DMRef
	LastMsgTs time.Time `json:"last_msg_ts"`
	Unread    bool      `json:"unread"`
	Mention   bool      `json:"mention"`
}

// UserConvos lists a user's DMs and group DMs, most-recent activity first,
// annotated with unread/mention state.
func (s *Store) UserConvos(ctx context.Context, user string) ([]ConvoEntry, error) {
	user = strings.ToLower(user)
	unread, _ := s.UnreadForConvos(ctx, user)
	var out []ConvoEntry
	err := kvx.ScanPrefix(ctx, s.DB, dmsPrefix+user+"/", func(_ string, v []byte) error {
		var ref DMRef
		if json.Unmarshal(v, &ref) != nil {
			return nil
		}
		e := ConvoEntry{DMRef: ref}
		if c, found, _ := s.GetConvo(ctx, ref.CID); found {
			e.LastMsgTs = c.LastMsgTs
		}
		if u, ok := unread[ref.CID]; ok {
			e.Unread = u.Count > 0
			e.Mention = u.Mention
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastMsgTs.After(out[j].LastMsgTs) })
	return out, nil
}

// groupDefaultName builds a name from the roster when none is given.
func groupDefaultName(members []string) string {
	if len(members) <= 3 {
		return strings.Join(members, ", ")
	}
	return fmt.Sprintf("%s, %s and %d more", members[0], members[1], len(members)-2)
}
