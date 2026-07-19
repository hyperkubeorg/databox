// Package contacts owns contact cards as FILES (spec §8), ported from
// PCD: a card is a .pccard JSON file in a drive and the Contacts app is
// an aggregating VIEW over every card the member can reach. Cards live
// in drives, so they share, back up, and version like any file — and a
// shared drive's cards are a shared address book for free.
//
// Integration: Match feeds the mail compose typeahead through the
// SuggestRecipients seam (contacts rank ABOVE recents), and the
// calendar's invite typeahead reads it for external addresses. No keys
// of its own — cards are ordinary node/blob rows.
package contacts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hyperkubeorg/databox/pkg/client"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Contact-card file format.
const (
	CardFormat      = "pcp-card/1"
	CardExt         = ".pccard"
	CardContentType = "application/x-pcp-card+json"
	cardMaxBlob     = 64 << 10
	// FolderName is where new cards land in the personal drive (created
	// lazily on first save).
	FolderName = "Contacts"
)

// Card is one card's contents (the blob body).
type Card struct {
	Format string `json:"format"`
	Name   string `json:"name"`
	// Emails / Phones are the addressable fields; Emails feed the mail
	// compose typeahead and the app's "Email" action.
	Emails []string `json:"emails,omitempty"`
	Phones []string `json:"phones,omitempty"`
	Org    string   `json:"org,omitempty"`
	Title  string   `json:"title,omitempty"`
	Notes  string   `json:"notes,omitempty"`
	// Fields are the member's own extras — any number, any label
	// (vault-style: door codes, backup emails, account numbers).
	Fields []Field `json:"fields,omitempty"`
}

// Field is one arbitrary extra fact about a contact. Type picks the
// rendering: text, secret (masked until revealed), note (multiline),
// url (link), or date.
type Field struct {
	Label string `json:"label"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// fieldTypes is the Field.Type whitelist (unknown types store as text).
var fieldTypes = map[string]bool{"text": true, "secret": true, "note": true, "url": true, "date": true}

// Field limits: count per card, label length, and value length by type
// (secrets/notes get room; truncating a secret would corrupt it, so
// over-long values REFUSE rather than clip).
const (
	maxFields      = 50
	maxFieldLabel  = 60
	maxFieldValue  = 500
	maxFieldSecret = 4000
)

// Store wraps the databox client with the card access methods.
type Store struct {
	DB     *client.Client
	Nodes  *nodes.Store
	Drives *drives.Store
}

// IsCardFile reports whether a node holds a contact card.
func IsCardFile(n nodes.Node) bool {
	return !n.IsDir && strings.HasSuffix(strings.ToLower(n.Name), CardExt)
}

// ParseCard decodes a card blob, tolerating an absent format tag.
func ParseCard(raw []byte) (Card, error) {
	if len(raw) > cardMaxBlob {
		return Card{}, fmt.Errorf("card too large")
	}
	var c Card
	if err := json.Unmarshal(raw, &c); err != nil {
		return Card{}, err
	}
	if c.Format == "" {
		c.Format = CardFormat
	}
	return c, nil
}

// validCardEmail is the address gate for card emails (the mail domain
// re-validates before anything sends).
func validCardEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-3 && len(s) <= 254 &&
		!strings.ContainsAny(s, " \t\r\n<>,;\"") && strings.Contains(s[at+1:], ".")
}

// Sanitize trims and validates a card's fields for storage.
func Sanitize(c Card) (Card, error) {
	c.Format = CardFormat
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || len(c.Name) > 200 {
		return Card{}, fmt.Errorf("a contact needs a name (200 chars max)")
	}
	var emails []string
	for _, e := range c.Emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !validCardEmail(e) {
			return Card{}, fmt.Errorf("bad email %q", e)
		}
		emails = append(emails, e)
	}
	c.Emails = emails
	var phones []string
	for _, p := range c.Phones {
		if p = strings.TrimSpace(p); p != "" && len(p) <= 60 {
			phones = append(phones, p)
		}
	}
	c.Phones = phones
	c.Org = strings.TrimSpace(c.Org)
	c.Title = strings.TrimSpace(c.Title)
	if len(c.Org) > 200 {
		c.Org = c.Org[:200]
	}
	if len(c.Title) > 200 {
		c.Title = c.Title[:200]
	}
	if len(c.Notes) > 4000 {
		c.Notes = c.Notes[:4000]
	}
	var fields []Field
	for _, f := range c.Fields {
		f.Label = strings.TrimSpace(f.Label)
		f.Value = strings.TrimSpace(f.Value)
		if f.Value == "" {
			continue
		}
		if !fieldTypes[f.Type] {
			f.Type = "text"
		}
		if f.Label == "" {
			f.Label = f.Type
		}
		if len(f.Label) > maxFieldLabel {
			f.Label = f.Label[:maxFieldLabel]
		}
		max := maxFieldValue
		if f.Type == "secret" || f.Type == "note" {
			max = maxFieldSecret
		}
		if len(f.Value) > max {
			return Card{}, fmt.Errorf("field %q is too long (%d chars max)", f.Label, max)
		}
		fields = append(fields, f)
		if len(fields) >= maxFields {
			break
		}
	}
	c.Fields = fields
	return c, nil
}

// FindCards lists every reachable .pccard file in a drive, name-sorted.
func (s *Store) FindCards(ctx context.Context, driveID string) ([]nodes.Node, error) {
	return s.Nodes.FindBySuffix(ctx, driveID, CardExt)
}

// LoadCard reads and parses one card file's blob.
func (s *Store) LoadCard(ctx context.Context, driveID, blobID string) (Card, error) {
	var buf bytes.Buffer
	if err := s.DB.GetBlob(ctx, nodes.BlobKey(driveID, blobID), &buf); err != nil {
		return Card{}, err
	}
	return ParseCard(buf.Bytes())
}

// Entry is one aggregated card: where it lives plus its contents.
type Entry struct {
	DriveID   string `json:"drive"`
	NodeID    string `json:"node"`
	DriveName string `json:"driveName"`
	CanEdit   bool   `json:"canEdit"`
	Card      Card   `json:"card"`
}

// Aggregate collects every reachable card across the member's drives —
// the mirror of the calendar's MemberCalendars.
func (s *Store) Aggregate(ctx context.Context, username string) ([]Entry, error) {
	ds, err := s.Drives.UserDriveInfos(ctx, username)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, d := range ds {
		cardNodes, err := s.FindCards(ctx, d.ID)
		if err != nil {
			continue
		}
		canEdit := drives.RoleAtLeast(d.Role, drives.RoleEditor)
		for _, n := range cardNodes {
			card, err := s.LoadCard(ctx, d.ID, n.BlobID)
			if err != nil {
				continue
			}
			out = append(out, Entry{DriveID: d.ID, NodeID: n.ID, DriveName: d.Name, CanEdit: canEdit, Card: card})
		}
	}
	return out, nil
}

// Match returns "Name <email>" display strings for cards matching q —
// the address-book half of the compose typeahead (SuggestRecipients
// ranks these ABOVE recents).
func (s *Store) Match(ctx context.Context, username, q string, limit int) []string {
	q = strings.ToLower(strings.TrimSpace(q))
	entries, err := s.Aggregate(ctx, username)
	if err != nil {
		return nil
	}
	var out []string
	for _, en := range entries {
		nameHit := q == "" || strings.Contains(strings.ToLower(en.Card.Name), q)
		for _, e := range en.Card.Emails {
			if nameHit || strings.Contains(e, q) {
				out = append(out, en.Card.Name+" <"+e+">")
				break
			}
		}
		if len(out) >= limit {
			break
		}
	}
	return out
}

// contactsFolder resolves (creating lazily) the personal drive's
// Contacts folder — where new cards land.
func (s *Store) contactsFolder(ctx context.Context, driveID, by string) (string, error) {
	n, found, err := s.Nodes.GetChild(ctx, driveID, nodes.RootID, FolderName)
	if err != nil {
		return "", err
	}
	if found && n.IsDir {
		return n.ID, nil
	}
	if found {
		return nodes.RootID, nil // a FILE named Contacts squats the name — root then
	}
	created, err := s.Nodes.CreateFolder(ctx, driveID, nodes.RootID, FolderName, by)
	if err != nil {
		if kvx.IsConflict(err) { // racing create — re-resolve
			if n, found, err2 := s.Nodes.GetChild(ctx, driveID, nodes.RootID, FolderName); err2 == nil && found && n.IsDir {
				return n.ID, nil
			}
		}
		return "", err
	}
	return created.ID, nil
}

// CreateCard writes a new card file. driveID "" = the personal drive's
// Contacts folder (created lazily); a shared drive gets the card at its
// root. The caller has already checked editor access; quota charges the
// creator like any upload.
func (s *Store) CreateCard(ctx context.Context, user users.User, driveID string, card Card, quota int64) (string, nodes.Node, error) {
	card, err := Sanitize(card)
	if err != nil {
		return "", nodes.Node{}, err
	}
	parentID := nodes.RootID
	if driveID == "" || driveID == user.PersonalDrive {
		driveID = user.PersonalDrive
		if driveID == "" {
			return "", nodes.Node{}, users.ErrNotFound
		}
		if parentID, err = s.contactsFolder(ctx, driveID, user.Username); err != nil {
			return "", nodes.Node{}, err
		}
	}
	raw, _ := json.Marshal(card)
	blobID := kvx.NewID()
	if err := s.DB.PutBlob(ctx, nodes.BlobKey(driveID, blobID), bytes.NewReader(raw), CardContentType); err != nil {
		return "", nodes.Node{}, err
	}
	name := safeCardFileName(card.Name) + CardExt
	n, err := s.Nodes.CommitStored(ctx, driveID, parentID, name, blobID, CardContentType, int64(len(raw)), quota, user.Username)
	return driveID, n, err
}

// SaveCard overwrites an existing card file with edited fields (a new
// version, same file name — the display name lives in the blob),
// mirroring the editor save-back path.
func (s *Store) SaveCard(ctx context.Context, driveID, nodeID, by string, card Card) (Card, error) {
	card, err := Sanitize(card)
	if err != nil {
		return Card{}, err
	}
	ref, found, err := s.Nodes.GetRef(ctx, driveID, nodeID)
	if err != nil {
		return Card{}, err
	}
	if !found {
		return Card{}, users.ErrNotFound
	}
	raw, _ := json.Marshal(card)
	blobID := kvx.NewID()
	if err := s.DB.PutBlob(ctx, nodes.BlobKey(driveID, blobID), bytes.NewReader(raw), CardContentType); err != nil {
		return Card{}, err
	}
	_, err = s.Nodes.CommitFile(ctx, driveID, ref.ParentID, ref.Name, blobID, CardContentType, int64(len(raw)), by, false)
	return card, err
}

// safeCardFileName strips name characters the node layer refuses.
func safeCardFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\x00':
			return '-'
		}
		return r
	}, name)
	name = strings.Trim(strings.TrimSpace(name), ".")
	if name == "" {
		name = "Contact"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}
