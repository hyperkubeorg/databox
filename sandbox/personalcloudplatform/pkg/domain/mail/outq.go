// outq.go — the authoritative outbound queue at /pcp/mail/outq/. A row
// is born HELD (the undo-send window), releases to PENDING, flips to
// SUBMITTED when a gateway takes it, and is deleted when a `sent` or
// `bounced` event arrives — a gateway restart that discards its RAM
// queue simply gets the message re-submitted.
package mail

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Outbound states.
const (
	OutHeld      = "held"      // inside the sender's undo-send window
	OutPending   = "pending"   // waiting for the outbound loop
	OutSubmitted = "submitted" // handed to a gateway
)

// OutMsg is one queued outbound message. The raw bytes live in
// BlobOf's mail blob space under BlobID.
type OutMsg struct {
	ID       string    `json:"id"`
	User     string    `json:"user,omitempty"` // sender account ("" = system: DSNs, distro forwards)
	BoxID    string    `json:"box_id,omitempty"`
	MailFrom string    `json:"mail_from"` // SMTP envelope sender ("" = null path)
	RcptTo   []string  `json:"rcpt_to"`
	BlobID   string    `json:"blob_id"`
	BlobOf   string    `json:"blob_of"` // which account's blob space holds BlobID
	State    string    `json:"state"`
	PO       string    `json:"po,omitempty"` // gateway that took it
	Attempts int       `json:"attempts"`
	At       time.Time `json:"at"`
	// HoldUntil is the undo-send release time (held rows only).
	HoldUntil time.Time `json:"hold_until,omitzero"`
	// SentMsgID is the Sent-copy message to flag when events arrive.
	SentMsgID string `json:"sent_msg_id,omitempty"`
	// Compose survives on held rows so CancelOutbound can hand the
	// draft back; cleared at release.
	Compose *ComposeInput `json:"compose,omitempty"`
}

// EnqueueOutbound queues one message.
func (s *Store) EnqueueOutbound(ctx context.Context, om OutMsg) (OutMsg, error) {
	if om.ID == "" {
		om.ID = kvx.NewID()
	}
	if om.State == "" {
		om.State = OutPending
	}
	if om.At.IsZero() {
		om.At = time.Now()
	}
	key := outqPrefix + kvx.InvIDAt(om.At) + "-" + om.ID
	return om, kvx.SetJSON(ctx, s.DB, key, om)
}

// ScanOutbound walks the queue newest-first (the outbound loop and the
// admin view both page it).
func (s *Store) ScanOutbound(ctx context.Context, fn func(key string, om OutMsg) error) error {
	return kvx.ScanPrefix(ctx, s.DB, outqPrefix, func(key string, value []byte) error {
		var om OutMsg
		if json.Unmarshal(value, &om) != nil {
			return nil
		}
		return fn(key, om)
	})
}

// UpdateOutbound rewrites one queue row.
func (s *Store) UpdateOutbound(ctx context.Context, key string, om OutMsg) error {
	return kvx.SetJSON(ctx, s.DB, key, om)
}

// DeleteOutbound removes a finished queue row.
func (s *Store) DeleteOutbound(ctx context.Context, key string) error {
	return s.DB.Delete(ctx, key)
}

// DeleteOutboundBlob removes a finished queue row's raw bytes from the
// sender's blob space (the Sent copy has its own blob under the
// message id).
func (s *Store) DeleteOutboundBlob(ctx context.Context, owner, blobID string) error {
	return s.DB.DeleteBlob(ctx, blobsPrefix+owner+"/"+blobID)
}

// FindOutbound locates a queue row by its OutID (delivery events carry
// the id, not the key).
func (s *Store) FindOutbound(ctx context.Context, outID string) (key string, om OutMsg, found bool) {
	_ = s.ScanOutbound(ctx, func(k string, o OutMsg) error {
		if o.ID == outID {
			key, om, found = k, o, true
		}
		return nil
	})
	return
}
