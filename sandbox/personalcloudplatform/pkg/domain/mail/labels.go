// labels.go — user-defined colored labels, orthogonal to folders
// (spec §7.2). The starter set (Work/Personal/Finance/Travel) is
// staged into the transaction that creates the user's FIRST mailbox;
// label CRUD lives in mail settings; thread label set/unset re-files
// the bylabel index through the standard thread update.
package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Label is one user label.
type Label struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"` // #RRGGBB
	Order int    `json:"order"`
}

// maxLabels bounds one user's label set.
const maxLabels = 100

// starterLabels are created at mailbox provisioning (spec §7.2 — the
// mockup's sidebar set).
var starterLabels = []Label{
	{Name: "Work", Color: "#67C99A", Order: 0},
	{Name: "Personal", Color: "#6FB6E8", Order: 1},
	{Name: "Finance", Color: "#C08AF0", Order: 2},
	{Name: "Travel", Color: "#E8746B", Order: 3},
}

// stageStarterLabels writes the starter set on the caller's
// transaction (CreateMailbox, first mailbox only).
func stageStarterLabels(tx *client.Tx, user string) {
	for _, l := range starterLabels {
		l.ID = kvx.NewID()
		raw, _ := json.Marshal(l)
		tx.Set(labelsPrefix+user+"/"+l.ID, raw)
	}
}

// validLabel gates name and color.
func validLabel(l Label) error {
	l.Name = strings.TrimSpace(l.Name)
	if l.Name == "" || len(l.Name) > 40 {
		return fmt.Errorf("label names are 1–40 characters")
	}
	if len(l.Color) != 7 || l.Color[0] != '#' || !isHex(l.Color[1:]) {
		return fmt.Errorf("label colors look like #RRGGBB")
	}
	return nil
}

// CreateLabel adds a label.
func (s *Store) CreateLabel(ctx context.Context, user string, name, color string) (Label, error) {
	l := Label{ID: kvx.NewID(), Name: strings.TrimSpace(name), Color: color}
	if err := validLabel(l); err != nil {
		return Label{}, err
	}
	existing, err := s.ListLabels(ctx, user)
	if err != nil {
		return Label{}, err
	}
	if len(existing) >= maxLabels {
		return Label{}, fmt.Errorf("at most %d labels", maxLabels)
	}
	for _, e := range existing {
		if strings.EqualFold(e.Name, l.Name) {
			return Label{}, fmt.Errorf("a label named %q already exists", l.Name)
		}
	}
	l.Order = len(existing)
	return l, kvx.SetJSON(ctx, s.DB, labelsPrefix+user+"/"+l.ID, l)
}

// UpdateLabel renames/recolors/reorders a label.
func (s *Store) UpdateLabel(ctx context.Context, user string, l Label) error {
	if !kvx.ValidID(l.ID) {
		return ErrNotFound
	}
	if err := validLabel(l); err != nil {
		return err
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, found, err := tx.Get(ctx, labelsPrefix+user+"/"+l.ID); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
		raw, _ := json.Marshal(l)
		tx.Set(labelsPrefix+user+"/"+l.ID, raw)
		return nil
	})
}

// DeleteLabel removes a label: every labeled thread sheds it (index
// row + meta strip), then the label record goes.
func (s *Store) DeleteLabel(ctx context.Context, user, labelID string) error {
	if !kvx.ValidID(labelID) {
		return ErrNotFound
	}
	// Walk the label's index; each row names a thread to strip.
	for {
		rows, _, err := s.ListByLabel(ctx, user, labelID, "", 100)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		for _, m := range rows {
			if err := s.SetThreadLabel(ctx, user, m.BoxID, m.ThreadID, labelID, false); err != nil && err != ErrNotFound {
				return err
			}
		}
	}
	return s.DB.Delete(ctx, labelsPrefix+user+"/"+labelID)
}

// ListLabels returns a user's labels, order-sorted.
func (s *Store) ListLabels(ctx context.Context, user string) ([]Label, error) {
	var out []Label
	err := kvx.ScanPrefix(ctx, s.DB, labelsPrefix+user+"/", func(_ string, value []byte) error {
		var l Label
		if json.Unmarshal(value, &l) == nil {
			out = append(out, l)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, err
}

// SetThreadLabel adds or removes one label on a thread (the bylabel
// index re-files in the same transaction as the meta).
func (s *Store) SetThreadLabel(ctx context.Context, user, boxID, threadID, labelID string, on bool) error {
	if !kvx.ValidID(labelID) {
		return ErrNotFound
	}
	if on {
		// The label must exist before a thread can wear it.
		if _, found, err := s.DB.Get(ctx, labelsPrefix+user+"/"+labelID); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
	}
	return s.mutateThread(ctx, user, boxID, threadID, func(m *ThreadMeta) error {
		has := slices.Contains(m.Labels, labelID)
		switch {
		case on && !has:
			m.Labels = append(m.Labels, labelID)
			sort.Strings(m.Labels)
		case !on && has:
			m.Labels = slices.DeleteFunc(m.Labels, func(id string) bool { return id == labelID })
		}
		return nil
	})
}
