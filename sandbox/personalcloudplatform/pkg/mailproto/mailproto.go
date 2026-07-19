// Package mailproto is the mail control-plane payload vocabulary shared
// by pkg/postoffice (the gateway), pkg/poclient (PCP's dialer), and
// pkg/domain/mail (which builds config pushes). Everything here rides
// pkg/wire's sealing/signing; both binaries import this package, so the
// two halves can never drift.
package mailproto

import (
	"fmt"
	"time"
)

// ConfigPush is the declarative desired state PCP PUTs to /v1/config —
// sealed to the gateway's key on the wire. It carries everything the
// gateway needs to run standalone; a push fully replaces the previous
// state, so drift is impossible.
type ConfigPush struct {
	// ManifestSerial versions this push; the gateway persists and
	// reports it, so the admin console can see drift at a glance.
	ManifestSerial uint64 `json:"manifest_serial"`
	// Hostname is the gateway's public name, derived from the admin-set
	// endpoint. The gateway uses it for its HELO banner and Received
	// line, so the admin console is the single source of truth for the
	// gateway's identity — a wrong value captured at `setup` time is
	// corrected here without re-pairing.
	Hostname string `json:"hostname,omitempty"`
	// Recipients are every deliverable address (mailboxes, aliases,
	// distros — flattened). Plaintext ONLY inside the sealed payload;
	// the gateway persists salted hashes.
	Recipients []string `json:"recipients"`
	// Domains carries each served domain's DKIM signing material —
	// held in RAM only on the gateway.
	Domains []DomainConfig `json:"domains"`
	// Spam policy: DNSBL zones checked at connect, and an optional
	// spamd (SpamAssassin) endpoint with score thresholds.
	RBLZones   []string `json:"rbl_zones,omitempty"`
	SpamdAddr  string   `json:"spamd_addr,omitempty"`
	SpamTag    float64  `json:"spam_tag"`
	SpamReject float64  `json:"spam_reject"`
	// Limits. All concrete — PCP resolves defaults before pushing, so
	// the gateway never guesses.
	MaxMsgBytes       int64 `json:"max_msg_bytes"`
	MaxRcpt           int   `json:"max_rcpt"`
	MaxConns          int   `json:"max_conns"`
	MaxConnsPerIP     int   `json:"max_conns_per_ip"`
	PerIPPerMinute    int   `json:"per_ip_per_minute"`
	SpoolCapBytes     int64 `json:"spool_cap_bytes"`
	RecipientSharePct int   `json:"recipient_share_pct"`
}

// DomainConfig is one served domain's signing identity.
type DomainConfig struct {
	Domain       string `json:"domain"`
	DKIMSelector string `json:"dkim_selector"`
	DKIMPrivPEM  string `json:"dkim_priv_pem"`
}

// ConfigResponse acknowledges a push.
type ConfigResponse struct {
	AppliedSerial uint64 `json:"applied_serial"`
}

// InboundEnvelope is one accepted message — SEALED to PCP's key before
// it ever reaches the gateway's disk. Raw already carries the gateway's
// own Received header.
type InboundEnvelope struct {
	From       string    `json:"from"` // SMTP envelope sender ("" = null path / bounces)
	Rcpts      []string  `json:"rcpts"`
	ReceivedAt time.Time `json:"received_at"`
	RemoteIP   string    `json:"remote_ip"`
	SpamScore  float64   `json:"spam_score,omitempty"`
	Raw        []byte    `json:"raw"`
}

// InboundMessage is one spool entry on the wire: an opaque id plus the
// sealed envelope.
type InboundMessage struct {
	SpoolID string `json:"spool_id"`
	Sealed  []byte `json:"sealed"`
}

// InboundResponse answers GET /v1/inbound (long-poll batch, oldest
// first). More=true means the spool holds further messages beyond this
// batch — fetch again immediately after acking.
type InboundResponse struct {
	Messages []InboundMessage `json:"messages"`
	More     bool             `json:"more"`
}

// AckRequest deletes delivered spool entries (POST /v1/inbound/ack).
type AckRequest struct {
	SpoolIDs []string `json:"spool_ids"`
}

// OutboundSubmission is a message PCP hands the gateway to deliver
// (POST /v1/outbound, sealed to the gateway). The gateway DKIM-signs,
// strips PCP-identifying headers, and queues for MX delivery.
type OutboundSubmission struct {
	OutID    string   `json:"out_id"` // idempotency key; echoed in events
	MailFrom string   `json:"mail_from"`
	RcptTo   []string `json:"rcpt_to"`
	Raw      []byte   `json:"raw"`
}

// OutboundBatch is one sealed POST /v1/outbound body.
type OutboundBatch struct {
	Messages []OutboundSubmission `json:"messages"`
}

// OutboundResponse acknowledges acceptance into the gateway's send
// queue (delivery outcomes arrive later via /v1/events).
type OutboundResponse struct {
	Accepted []string `json:"accepted"` // OutIDs queued
}

// Delivery event states.
const (
	EventSent     = "sent"
	EventDeferred = "deferred"
	EventBounced  = "bounced"
)

// DeliveryEvent reports one outbound outcome.
type DeliveryEvent struct {
	Seq    uint64 `json:"seq"`
	OutID  string `json:"out_id"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"` // DSN text on bounce/defer
}

// EventsResponse answers GET /v1/events?cursor=.
type EventsResponse struct {
	Events []DeliveryEvent `json:"events"`
	Cursor uint64          `json:"cursor"`
}

// Counters are the gateway's monotonic per-boot tallies (spec §11.3).
type Counters struct {
	Accepted    uint64 `json:"accepted"`     // messages spooled off SMTP
	RejectedRBL uint64 `json:"rejected_rbl"` // connections refused by DNSBL
	Delivered   uint64 `json:"delivered"`    // outbound sent events
	Deferred    uint64 `json:"deferred"`     // outbound defer reschedules
	Bounced     uint64 `json:"bounced"`      // outbound bounce events
}

// ErrorEntry is one row of the gateway's RAM error ring (§11.3: the
// last 50 operational errors, newest last).
type ErrorEntry struct {
	At   time.Time `json:"at"`
	What string    `json:"what"`
}

// StatusResponse answers GET /v1/status — the §11.3 self-report the
// admin console's worker pages consume: version, uptime, config serial,
// key freshness, queue depths, counters, and recent errors.
type StatusResponse struct {
	Version   string    `json:"version"`
	Now       time.Time `json:"now"`
	StartedAt time.Time `json:"started_at"`
	// ManifestSerial is the applied config push; DKIMInRAM reports key
	// freshness (false after a restart until PCP re-pushes — outbound
	// mail goes unsigned until then).
	ManifestSerial uint64 `json:"manifest_serial"`
	DKIMInRAM      bool   `json:"dkim_in_ram"`
	// Spool depth (count + bytes) and the outbound/event queues.
	SpoolBytes    int64 `json:"spool_bytes"`
	SpoolCount    int   `json:"spool_count"`
	SpoolCapBytes int64 `json:"spool_cap_bytes"`
	OutQueueDepth int   `json:"out_queue_depth"`
	EventDepth    int   `json:"event_depth"`
	// Counters and the error ring.
	Counters   Counters     `json:"counters"`
	LastErrors []ErrorEntry `json:"last_errors,omitempty"`

	Domains         []string  `json:"domains,omitempty"`
	CertNotAfter    time.Time `json:"cert_not_after"`
	SMTPListening   bool      `json:"smtp_listening"`
	SpamdConfigured bool      `json:"spamd_configured"`
	SpamdOK         bool      `json:"spamd_ok"`
	// PublicIPs are the gateway's public-facing addresses (IPv4 + IPv6),
	// so PCP can build SPF and per-IP reverse-DNS checks from the real
	// set mail can leave from.
	PublicIPs []string `json:"public_ips,omitempty"`
}

// Summary is the one-line form persisted on the PostOffice record.
func (s StatusResponse) Summary() string {
	return fmt.Sprintf("v%s · spool %d msg / %d B · outq %d · events %d · manifest #%d",
		s.Version, s.SpoolCount, s.SpoolBytes, s.OutQueueDepth, s.EventDepth, s.ManifestSerial)
}
