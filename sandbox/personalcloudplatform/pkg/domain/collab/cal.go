// cal.go — the calendar document type (spec §8), joining the substrate
// through the seam the phase-2c editors left: a calendar is a FILE
// (.pccal, pcp-cal/1 JSON) whose events are LWW registers keyed
// `e:<eventID>` in the same op-log, so calendars get live fan-out,
// lock-gated compaction, save-back, and version history for free.
//
// Unlike the opaque writer blocks, event values are FULLY validated at
// append time — they're attacker-shaped and feed server-side
// aggregation, notifications, and outbound invite mail (the calendar
// domain). Invitees are usernames OR external email addresses;
// externals never RSVP here (they answer by ICS mail, spec §7.6).
package collab

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Calendar format constants.
const (
	CalFormat      = "pcp-cal/1"
	CalExt         = ".pccal"
	CalContentType = "application/x-pcp-cal+json"
	calMaxDoc      = 8 << 20
	calMaxEvents   = 20000
	// CalMaxPeople bounds an event's tag + invite lists.
	CalMaxPeople = 64
)

// RSVP statuses (Event.Invites values).
const (
	RSVPInvited = "invited"
	RSVPYes     = "yes"
	RSVPNo      = "no"
	RSVPMaybe   = "maybe"
)

// ValidRSVP accepts an invitee's answer (not "invited" — that's the ask).
func ValidRSVP(s string) bool { return s == RSVPYes || s == RSVPNo || s == RSVPMaybe }

// Event is one calendar entry.
type Event struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	AllDay   bool      `json:"all_day,omitempty"`
	Location string    `json:"location,omitempty"`
	Notes    string    `json:"notes,omitempty"`
	Tags     []string  `json:"tags,omitempty"` // mentioned members
	// Invites maps a member username OR an external email address to an
	// RSVP status. Members answer in-app; externals get ICS mail.
	Invites map[string]string `json:"invites,omitempty"`
	By      string            `json:"by,omitempty"`
	At      time.Time         `json:"at,omitzero"`
}

// ExternalInvitee reports whether an invite key is an outside email
// address rather than a member username.
func ExternalInvitee(key string) bool { return strings.Contains(key, "@") }

// CalDoc is the whole calendar.
type CalDoc struct {
	Format string           `json:"format"`
	Color  string           `json:"color,omitempty"`
	Events map[string]Event `json:"events"`
}

// NewCalDoc is an empty calendar.
func NewCalDoc(color string) CalDoc {
	return CalDoc{Format: CalFormat, Color: color, Events: map[string]Event{}}
}

// IsCalFile reports whether a node holds a calendar.
func IsCalFile(n nodes.Node) bool {
	return !n.IsDir && strings.HasSuffix(strings.ToLower(n.Name), CalExt)
}

// ParseCalDoc decodes and shape-checks a calendar.
func ParseCalDoc(raw []byte) (CalDoc, error) {
	if len(raw) > calMaxDoc {
		return CalDoc{}, fmt.Errorf("calendar too large")
	}
	var doc CalDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return CalDoc{}, err
	}
	if doc.Format != CalFormat {
		return CalDoc{}, fmt.Errorf("not a %s document", CalFormat)
	}
	if doc.Events == nil {
		doc.Events = map[string]Event{}
	}
	if len(doc.Events) > calMaxEvents {
		return CalDoc{}, fmt.Errorf("too many events")
	}
	return doc, nil
}

// validInviteEmail is the minimal shape gate for external invitees (the
// mail domain re-validates before anything sends).
func validInviteEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-3 && len(s) <= 254 &&
		!strings.ContainsAny(s, " \t\r\n<>,;\"") && strings.Contains(s[at+1:], ".")
}

// ValidEvent is the shape gate every event write passes.
func ValidEvent(e Event) error {
	if !validEntityID(e.ID) {
		return fmt.Errorf("bad event id")
	}
	if strings.TrimSpace(e.Title) == "" || len(e.Title) > 200 {
		return fmt.Errorf("events need a title (200 chars max)")
	}
	if e.Start.IsZero() || e.End.IsZero() || e.End.Before(e.Start) {
		return fmt.Errorf("events need a start and an end, in order")
	}
	if len(e.Location) > 300 || len(e.Notes) > 4000 {
		return fmt.Errorf("location/notes too long")
	}
	if len(e.Tags)+len(e.Invites) > CalMaxPeople {
		return fmt.Errorf("at most %d people per event", CalMaxPeople)
	}
	for _, u := range e.Tags {
		if users.ValidUsername(u) != nil {
			return fmt.Errorf("bad tag %q", u)
		}
	}
	for who, status := range e.Invites {
		if ExternalInvitee(who) {
			if !validInviteEmail(who) {
				return fmt.Errorf("bad invitee %q", who)
			}
		} else if users.ValidUsername(who) != nil {
			return fmt.Errorf("bad invitee %q", who)
		}
		switch status {
		case RSVPInvited, RSVPYes, RSVPNo, RSVPMaybe:
		default:
			return fmt.Errorf("bad rsvp %q", status)
		}
	}
	return nil
}

// calEventTargetRe pins the event target grammar.
var calEventTargetRe = regexp.MustCompile(`^e:([A-Za-z0-9_-]{4,16})$`)

// ValidCalColor accepts "#rgb"/"#rrggbb".
func ValidCalColor(c string) bool {
	if (len(c) != 4 && len(c) != 7) || c[0] != '#' {
		return false
	}
	for _, r := range c[1:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// AppendCalOp validates and appends one calendar op, returning the
// event (isEvent=true) when the op saved one — the caller fans out
// notifications and invite mail off it.
func (s *Store) AppendCalOp(ctx context.Context, driveID, nodeID string, op TargetOp, actor string) (Event, bool, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return Event{}, false, users.ErrNotFound
	}
	if !ValidHLC(op.HLC, actor) {
		return Event{}, false, fmt.Errorf("bad clock")
	}
	switch {
	case calEventTargetRe.MatchString(op.T):
		if string(op.V) == "null" {
			return Event{}, false, s.appendOp(ctx, driveID, nodeID, op.HLC, op)
		}
		var e Event
		if err := json.Unmarshal(op.V, &e); err != nil {
			return Event{}, false, fmt.Errorf("bad event")
		}
		if "e:"+e.ID != op.T {
			return Event{}, false, fmt.Errorf("event id mismatch")
		}
		if err := ValidEvent(e); err != nil {
			return Event{}, false, err
		}
		return e, true, s.appendOp(ctx, driveID, nodeID, op.HLC, op)
	case op.T == "meta":
		if len(op.V) > 4096 {
			return Event{}, false, fmt.Errorf("bad meta")
		}
		return Event{}, false, s.appendOp(ctx, driveID, nodeID, op.HLC, op)
	}
	return Event{}, false, fmt.Errorf("bad op target")
}

// FoldCalOp applies one op to a calendar.
func FoldCalOp(doc *CalDoc, op TargetOp) {
	if m := calEventTargetRe.FindStringSubmatch(op.T); m != nil {
		if string(op.V) == "null" || len(op.V) == 0 {
			delete(doc.Events, m[1])
			return
		}
		var e Event
		if json.Unmarshal(op.V, &e) != nil || ValidEvent(e) != nil || e.ID != m[1] {
			return
		}
		if len(doc.Events) >= calMaxEvents {
			if _, exists := doc.Events[e.ID]; !exists {
				return
			}
		}
		doc.Events[e.ID] = e
		return
	}
	if op.T == "meta" {
		var meta struct {
			Color string `json:"color"`
		}
		if json.Unmarshal(op.V, &meta) == nil && ValidCalColor(meta.Color) {
			doc.Color = meta.Color
		}
	}
}

// calSnapshot is the between-compactions snapshot blob.
type calSnapshot struct {
	Watermark string `json:"watermark"`
	Doc       CalDoc `json:"doc"`
}

// CalState is what an opening editor loads.
type CalState struct {
	Doc       CalDoc
	Watermark string
	Ops       []TargetOp
}

// LoadCalState reads snapshot + tail ops, seeding from the file bytes.
func (s *Store) LoadCalState(ctx context.Context, driveID, nodeID string, node nodes.Node) (CalState, error) {
	state := CalState{}
	var snap calSnapshot
	if s.loadSnapshot(ctx, driveID, nodeID, &snap) && snap.Doc.Format == CalFormat {
		state.Doc, state.Watermark = snap.Doc, snap.Watermark
	}
	if state.Doc.Format == "" {
		if node.BlobID != "" && node.Size > 0 && node.Size < calMaxDoc {
			var fileRaw strings.Builder
			if err := s.DB.GetBlob(ctx, nodes.BlobKey(driveID, node.BlobID), &fileRaw); err == nil {
				if doc, err := ParseCalDoc([]byte(fileRaw.String())); err == nil {
					state.Doc = doc
				}
			}
		}
		if state.Doc.Format == "" {
			state.Doc = NewCalDoc("")
		}
	}
	var err error
	state.Ops, err = s.scanTargetOps(ctx, driveID, nodeID)
	return state, err
}

// LoadCalDoc is the folded current state (aggregation + RSVP path).
func (s *Store) LoadCalDoc(ctx context.Context, driveID, nodeID string, node nodes.Node) (CalDoc, error) {
	state, err := s.LoadCalState(ctx, driveID, nodeID, node)
	if err != nil {
		return CalDoc{}, err
	}
	doc, _ := foldCalState(state)
	return doc, nil
}

// foldCalState is the pure half of the calendar compaction.
func foldCalState(state CalState) (CalDoc, string) {
	watermark := state.Watermark
	for _, op := range state.Ops {
		FoldCalOp(&state.Doc, op)
		if op.HLC > watermark {
			watermark = op.HLC
		}
	}
	return state.Doc, watermark
}

// EventsBetween filters a calendar to events overlapping [from, to),
// start-sorted.
func EventsBetween(doc CalDoc, from, to time.Time) []Event {
	var out []Event
	for _, e := range doc.Events {
		if e.Start.Before(to) && e.End.After(from) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// serverHLCCounter disambiguates server-minted clocks within one
// millisecond (two quick server ops must never share an op key).
var serverHLCCounter atomic.Int64

// ServerHLC mints an HLC for server-side ops on a user's behalf (RSVP,
// the API, inbound invite mail).
func ServerHLC(actor string) string {
	return fmt.Sprintf("%013d-%06d-%s", time.Now().UnixMilli(), serverHLCCounter.Add(1)%1000000, actor)
}

// SaveCalEvent appends a SERVER-minted op writing one whole event —
// the API and inbound-RSVP paths (browsers mint their own clocks).
func (s *Store) SaveCalEvent(ctx context.Context, driveID, nodeID string, e Event, actor string) error {
	raw, _ := json.Marshal(e)
	_, _, err := s.AppendCalOp(ctx, driveID, nodeID,
		TargetOp{T: "e:" + e.ID, V: raw, HLC: ServerHLC(actor)}, actor)
	return err
}

// DeleteCalEvent appends a server-minted tombstone op.
func (s *Store) DeleteCalEvent(ctx context.Context, driveID, nodeID, eventID, actor string) error {
	if !validEntityID(eventID) {
		return users.ErrNotFound
	}
	_, _, err := s.AppendCalOp(ctx, driveID, nodeID,
		TargetOp{T: "e:" + eventID, V: json.RawMessage("null"), HLC: ServerHLC(actor)}, actor)
	return err
}

// ApplyRSVP updates exactly one invitee's own answer via a server-minted
// op. The invitee needs NO drive role — being on the invite list IS the
// authorization. Returns the updated event.
func (s *Store) ApplyRSVP(ctx context.Context, driveID, nodeID string, node nodes.Node, eventID, username, status string) (Event, error) {
	username = strings.ToLower(username)
	if !ValidRSVP(status) {
		return Event{}, fmt.Errorf("bad rsvp")
	}
	doc, err := s.LoadCalDoc(ctx, driveID, nodeID, node)
	if err != nil {
		return Event{}, err
	}
	e, ok := doc.Events[eventID]
	if !ok {
		return Event{}, users.ErrNotFound
	}
	if _, invited := e.Invites[username]; !invited {
		return Event{}, users.ErrNotFound // strangers learn nothing
	}
	e.Invites[username] = status
	if err := s.SaveCalEvent(ctx, driveID, nodeID, e, username); err != nil {
		return Event{}, err
	}
	return e, nil
}

// ValidEventID exports the entity-id gate for handlers that key on it.
func ValidEventID(id string) bool { return validEntityID(id) }
