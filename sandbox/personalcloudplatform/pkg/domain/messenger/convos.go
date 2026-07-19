// convos.go — the conversation abstraction that unifies message storage
// across the three kinds (Messenger §3). A server channel's
// conversation id (cid) IS its channelID; a DM's cid is dm_<lo>_<hi>; a
// group DM's is a random g<id>. The Convo record carries LastMsgTs, which
// the conversation lists and unread checks read.
package messenger

import (
	"context"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Conversation kinds.
const (
	ConvoChannel = "channel" // a server channel (cid == channelID)
	ConvoDM      = "dm"      // a 1:1 direct message (cid == dm_<lo>_<hi>)
	ConvoGroup   = "group"   // a group DM (cid == g<id>)
)

// Convo is one conversation's metadata. For channels it mirrors the
// channel; for DMs/groups it is the canonical record.
type Convo struct {
	ID        string    `json:"id"` // the cid
	Kind      string    `json:"kind"`
	ServerID  string    `json:"server_id,omitempty"` // channels only
	Name      string    `json:"name,omitempty"`      // group DMs
	Icon      string    `json:"icon,omitempty"`
	LastMsgTs time.Time `json:"last_msg_ts"`
	CreatedAt time.Time `json:"created_at"`
}

func convoKey(cid string) string { return convosPrefix + cid }

// GetConvo loads a conversation record (found=false before its first
// message for channels, which are lazily materialized).
func (s *Store) GetConvo(ctx context.Context, cid string) (Convo, bool, error) {
	var c Convo
	found, err := kvx.GetJSON(ctx, s.DB, convoKey(cid), &c)
	return c, found, err
}

// ensureConvoTx materializes a conversation record on an open transaction
// if absent, and returns whether it had to create it. Channels create
// lazily on first message; DMs/groups create explicitly (dms.go).
func ensureConvoTx(ctx context.Context, tx *client.Tx, cid, kind, serverID string, now time.Time) {
	var c Convo
	if getJSONTx(ctx, tx, convoKey(cid), &c) {
		return
	}
	setJSONTx(tx, convoKey(cid), Convo{ID: cid, Kind: kind, ServerID: serverID, CreatedAt: now, LastMsgTs: now})
}

// touchConvoTx bumps LastMsgTs (the one write per message that drives
// conversation ordering and derived unread), creating the record if it
// predates lazy materialization.
func touchConvoTx(ctx context.Context, tx *client.Tx, cid, kind, serverID string, now time.Time) {
	var c Convo
	if !getJSONTx(ctx, tx, convoKey(cid), &c) {
		c = Convo{ID: cid, Kind: kind, ServerID: serverID, CreatedAt: now}
	}
	c.LastMsgTs = now
	setJSONTx(tx, convoKey(cid), c)
}
