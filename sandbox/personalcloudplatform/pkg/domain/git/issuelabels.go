// issuelabels.go — repo-level labels (§8):
// /pcp/git/labels/<repoID>/<labelID> {name, color}, repo-write managed,
// mirroring the mail-label conventions (colored chips, #RRGGBB, name
// uniqueness, a bounded set). Deleting a label removes it from issues
// LAZILY: issue records keep the dangling id and readers filter against
// the live label set — the deliberate choice, because eager removal
// would rewrite an unbounded number of issue records + index copies in
// one action, while the lazy filter is one map lookup at render time
// and the dangling ids are invisible everywhere.
package git

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Label is one repo label.
type Label struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"` // #RRGGBB
}

// maxRepoLabels bounds one repo's label set (the mail cap).
const maxRepoLabels = 100

func labelKey(repoID, labelID string) string { return labelsPrefix + repoID + "/" + labelID }

// validLabel gates name and color (the mail-label rules).
func validLabel(l Label) error {
	if l.Name == "" || len(l.Name) > 40 {
		return fmt.Errorf("label names are 1–40 characters")
	}
	if len(l.Color) != 7 || l.Color[0] != '#' {
		return fmt.Errorf("label colors look like #RRGGBB")
	}
	for _, r := range l.Color[1:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return fmt.Errorf("label colors look like #RRGGBB")
		}
	}
	return nil
}

// ListLabels returns a repo's labels, name-sorted.
func (s *Store) ListLabels(ctx context.Context, repoID string) ([]Label, error) {
	if !kvx.ValidID(repoID) {
		return nil, nil
	}
	var out []Label
	err := kvx.ScanPrefix(ctx, s.DB, labelsPrefix+repoID+"/", func(_ string, v []byte) error {
		var l Label
		if json.Unmarshal(v, &l) == nil {
			out = append(out, l)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, err
}

// LabelMap resolves ids → labels for render-time filtering (the lazy
// deletion semantics above: a dangling id simply isn't in the map).
func (s *Store) LabelMap(ctx context.Context, repoID string) (map[string]Label, error) {
	labels, err := s.ListLabels(ctx, repoID)
	if err != nil {
		return nil, err
	}
	m := make(map[string]Label, len(labels))
	for _, l := range labels {
		m[l.ID] = l
	}
	return m, nil
}

// CreateLabel adds a label (repo-write, enforced by the app layer).
func (s *Store) CreateLabel(ctx context.Context, repoID, name, color string) (Label, error) {
	if !kvx.ValidID(repoID) {
		return Label{}, ErrNotFound
	}
	l := Label{ID: kvx.NewID(), Name: strings.TrimSpace(name), Color: strings.ToLower(color)}
	if err := validLabel(l); err != nil {
		return Label{}, err
	}
	existing, err := s.ListLabels(ctx, repoID)
	if err != nil {
		return Label{}, err
	}
	if len(existing) >= maxRepoLabels {
		return Label{}, fmt.Errorf("at most %d labels", maxRepoLabels)
	}
	for _, e := range existing {
		if strings.EqualFold(e.Name, l.Name) {
			return Label{}, fmt.Errorf("a label named %q already exists", l.Name)
		}
	}
	return l, kvx.SetJSON(ctx, s.DB, labelKey(repoID, l.ID), l)
}

// UpdateLabel renames/recolors one label.
func (s *Store) UpdateLabel(ctx context.Context, repoID string, l Label) error {
	if !kvx.ValidID(repoID) || !kvx.ValidID(l.ID) {
		return ErrNotFound
	}
	l.Name = strings.TrimSpace(l.Name)
	l.Color = strings.ToLower(l.Color)
	if err := validLabel(l); err != nil {
		return err
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, found, err := tx.Get(ctx, labelKey(repoID, l.ID)); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
		txSetJSON(tx, labelKey(repoID, l.ID), l)
		return nil
	})
}

// DeleteLabel removes the label record only — issues shed the dangling
// id lazily at read time (the package-comment decision).
func (s *Store) DeleteLabel(ctx context.Context, repoID, labelID string) error {
	if !kvx.ValidID(repoID) || !kvx.ValidID(labelID) {
		return ErrNotFound
	}
	return s.DB.Delete(ctx, labelKey(repoID, labelID))
}
