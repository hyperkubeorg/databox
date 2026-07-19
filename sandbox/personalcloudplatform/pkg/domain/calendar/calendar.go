// Package calendar is the aggregation layer over calendar FILES (spec
// §8), ported from PCD: a calendar is a .pccal collaborative document
// in a drive (pkg/domain/collab owns the format and the op-log); this
// package resolves a member's whole calendar world — every .pccal
// across their drives, layered with per-user subscription overrides —
// aggregates events in a range across it, applies RSVPs (the invite is
// the authorization), fans out invite/tag/RSVP notifications with the
// per-(event, member) dedup ledger (notify.go), and sends ICS invite
// mail to external invitees through the normal outbound path
// (icsmail.go, spec §7.6).
//
// Keys this package owns (kvx key table):
//
//	/pcp/calsubs/<user>/<driveID>/<nodeID>            → CalSub (visibility override)
//	/pcp/docs/<d>/<n>/notifsent/<event>/<suffix>      → {} (notification dedup;
//	                                                    dies with the doc)
//	/pcp/docs/<d>/<n>/icsmail/<event>                 → icsLedger (SEQUENCE + the
//	                                                    externals last mailed)
//	/pcp/mail/icsrsvp/<user>/<uidHash>                → InboundRSVP (answers to
//	                                                    foreign invites)
//
// A subscription GRANTS NOTHING — aggregation only ever walks the
// member's own drive memberships, and access is re-checked there.
package calendar

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// calSubsPrefix roots the subscription overrides (kvx key table).
const calSubsPrefix = "/pcp/calsubs/"

// MaxRange caps one aggregation query.
const MaxRange = 100 * 24 * time.Hour

// Store wraps the databox client with the calendar aggregation methods.
type Store struct {
	DB     *client.Client
	Users  *users.Store
	Drives *drives.Store
	Nodes  *nodes.Store
	Collab *collab.Store
	Notify *notify.Store
	// Mail sends external ICS invites and is where inbound RSVP replies
	// go out (nil = calendar works, invite mail silently skips).
	Mail *mail.Store
	Log  *slog.Logger
}

// warn logs soft failures without requiring a logger.
func (s *Store) warn(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Warn(msg, args...)
	}
}

// Info is one calendar as the member sees it: the file, its drive, and
// the layered subscription state.
type Info struct {
	DriveID   string `json:"drive"`
	NodeID    string `json:"node"`
	Name      string `json:"name"`
	DriveName string `json:"driveName"`
	Personal  bool   `json:"personal"`
	Color     string `json:"color"`
	Hidden    bool   `json:"hidden"`     // filter state (visible=false)
	Subbed    bool   `json:"subscribed"` // in the filter list at all
	CanEdit   bool   `json:"canEdit"`
}

// DefaultColor picks a stable palette color from the node id.
func DefaultColor(nodeID string) string {
	var h uint32 = 2166136261
	for _, c := range []byte(nodeID) {
		h = (h ^ uint32(c)) * 16777619
	}
	palette := []string{"#5B8CFF", "#3ECF8E", "#F0A66B", "#C687F0", "#F26D78", "#4FC3A1", "#D6B45E", "#79A6FF"}
	return palette[h%uint32(len(palette))]
}

// MemberCalendars resolves the member's whole calendar world: every
// .pccal in their drives, layered with subscription overrides. Personal
// calendars default to LISTED; shared ones follow the auto-subscribe
// preference or a manual subscription.
func (s *Store) MemberCalendars(ctx context.Context, user users.User) ([]Info, error) {
	subs, err := s.CalSubs(ctx, user.Username)
	if err != nil {
		return nil, err
	}
	ds, err := s.Drives.UserDriveInfos(ctx, user.Username)
	if err != nil {
		return nil, err
	}
	autoSub := user.Prefs.CalAutoSub == "on"
	var out []Info
	for _, d := range ds {
		cals, err := s.Nodes.FindBySuffix(ctx, d.ID, collab.CalExt)
		if err != nil {
			s.warn("calendar scan failed", "drive", d.ID, "err", err)
			continue
		}
		personal := d.Type == drives.Personal && d.Owner == user.Username
		for _, n := range cals {
			sub, hasSub := subs[d.ID+"/"+n.ID]
			info := Info{
				DriveID: d.ID, NodeID: n.ID,
				Name:      strings.TrimSuffix(n.Name, collab.CalExt),
				DriveName: d.Name, Personal: personal,
				Color:   DefaultColor(n.ID),
				CanEdit: drives.RoleAtLeast(d.Role, drives.RoleEditor),
			}
			switch {
			case personal, autoSub:
				info.Subbed = true
				info.Hidden = hasSub && sub.Hidden
			default:
				info.Subbed = hasSub
				info.Hidden = !hasSub || sub.Hidden
			}
			out = append(out, info)
		}
	}
	return out, nil
}

// CalEvents is one calendar's slice of an aggregation.
type CalEvents struct {
	Cal    Info           `json:"cal"`
	Events []collab.Event `json:"events"`
}

// EventsInRange aggregates events across the member's SUBSCRIBED
// calendars for [from, to) (hidden ones ride along — the view toggles
// locally, exactly like PCD).
func (s *Store) EventsInRange(ctx context.Context, user users.User, from, to time.Time) ([]CalEvents, error) {
	cals, err := s.MemberCalendars(ctx, user)
	if err != nil {
		return nil, err
	}
	out := []CalEvents{}
	for _, c := range cals {
		if !c.Subbed {
			continue
		}
		node, found, err := s.Nodes.GetByID(ctx, c.DriveID, c.NodeID)
		if err != nil || !found {
			continue
		}
		doc, err := s.Collab.LoadCalDoc(ctx, c.DriveID, c.NodeID, node)
		if err != nil {
			s.warn("calendar load failed", "node", c.NodeID, "err", err)
			continue
		}
		if doc.Color != "" {
			c.Color = doc.Color
		}
		out = append(out, CalEvents{Cal: c, Events: collab.EventsBetween(doc, from, to)})
	}
	return out, nil
}

// CreateCalendar makes a new .pccal file in a drive's root. The caller
// has already checked editor access; quota is the member's limit.
func (s *Store) CreateCalendar(ctx context.Context, driveID, name, by string, quota int64) (nodes.Node, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Calendar"
	}
	if !strings.HasSuffix(strings.ToLower(name), collab.CalExt) {
		name += collab.CalExt
	}
	raw, _ := json.Marshal(collab.NewCalDoc(""))
	blobID := kvx.NewID()
	if err := s.DB.PutBlob(ctx, nodes.BlobKey(driveID, blobID), strings.NewReader(string(raw)), collab.CalContentType); err != nil {
		return nodes.Node{}, err
	}
	return s.Nodes.CommitStored(ctx, driveID, nodes.RootID, name, blobID, collab.CalContentType, int64(len(raw)), quota, by)
}

// PrimaryPersonalCalendar resolves the member's default calendar — the
// first .pccal in their personal drive — creating "Calendar.pccal"
// lazily (inbound invite RSVPs land here, spec §7.6).
func (s *Store) PrimaryPersonalCalendar(ctx context.Context, user users.User, quota int64) (string, nodes.Node, error) {
	driveID := user.PersonalDrive
	if driveID == "" {
		return "", nodes.Node{}, users.ErrNotFound
	}
	cals, err := s.Nodes.FindBySuffix(ctx, driveID, collab.CalExt)
	if err != nil {
		return "", nodes.Node{}, err
	}
	if len(cals) > 0 {
		return driveID, cals[0], nil
	}
	n, err := s.CreateCalendar(ctx, driveID, "Calendar", user.Username, quota)
	return driveID, n, err
}

// --- subscriptions ------------------------------------------------------------------

// CalSub is a member's visibility override for one calendar.
type CalSub struct {
	Hidden bool      `json:"hidden,omitempty"`
	At     time.Time `json:"at"`
}

func calSubKey(username, driveID, nodeID string) string {
	return calSubsPrefix + username + "/" + driveID + "/" + nodeID
}

// SetCalSub writes a subscription override; remove=true deletes it
// (back to the default for that calendar's drive).
func (s *Store) SetCalSub(ctx context.Context, username, driveID, nodeID string, hidden, remove bool) error {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil || !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return users.ErrNotFound
	}
	key := calSubKey(username, driveID, nodeID)
	if remove {
		return s.DB.Delete(ctx, key)
	}
	return kvx.SetJSON(ctx, s.DB, key, CalSub{Hidden: hidden, At: time.Now().UTC()})
}

// CalSubs loads every override a member holds, keyed "driveID/nodeID".
func (s *Store) CalSubs(ctx context.Context, username string) (map[string]CalSub, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil, nil
	}
	out := map[string]CalSub{}
	prefix := calSubsPrefix + username + "/"
	err := kvx.ScanPrefix(ctx, s.DB, prefix, func(key string, value []byte) error {
		var sub CalSub
		if json.Unmarshal(value, &sub) == nil {
			out[strings.TrimPrefix(key, prefix)] = sub
		}
		return nil
	})
	return out, err
}

// --- the launcher card --------------------------------------------------------------

// NextToday finds the member's next event today across visible
// calendars: the soonest event still starting (or running) before
// midnight. ok=false = nothing (more) today.
func (s *Store) NextToday(ctx context.Context, user users.User, now time.Time) (collab.Event, bool) {
	dayEnd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	groups, err := s.EventsInRange(ctx, user, now, dayEnd)
	if err != nil {
		return collab.Event{}, false
	}
	var best collab.Event
	found := false
	for _, g := range groups {
		if g.Cal.Hidden {
			continue
		}
		for _, e := range g.Events {
			if !found || e.Start.Before(best.Start) {
				best, found = e, true
			}
		}
	}
	return best, found
}

// SortEvents orders a merged event list by start (aggregation callers).
func SortEvents(events []collab.Event) {
	sort.Slice(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })
}
