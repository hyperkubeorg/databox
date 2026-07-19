// dirdest.go implements Dest against a local directory (file:// URLs).
//
// This destination exists for tests and local development: the backup
// integration tests use it to exercise the full engine without an S3 or
// SFTP server, and it is handy for quick manual experiments. A real
// backup should leave the machine — a node backing up to its own disk
// protects against nothing the disk can do to you.
package backup

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// dirDest is one local directory tree.
type dirDest struct{ dir string }

// newDirDest ensures the directory exists and returns the destination.
func newDirDest(dir string) (*dirDest, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create backup dir: %w", err)
	}
	return &dirDest{dir: dir}, nil
}

// full maps a Dest-relative path onto the directory, refusing traversal —
// paths come from our own manifests, but a corrupted manifest must not be
// able to write outside the tree.
func (d *dirDest) full(p string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(p))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", fmt.Errorf("invalid backup path %q", p)
	}
	return filepath.Join(d.dir, clean), nil
}

// Put writes via temp-file-then-rename so "file exists" implies "file
// complete" — the property resume checks depend on.
func (d *dirDest) Put(p string, r io.Reader) error {
	full, err := d.full(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), full)
}

// Get opens a file; missing files return fs.ErrNotExist as usual.
func (d *dirDest) Get(p string) (io.ReadCloser, error) {
	full, err := d.full(p)
	if err != nil {
		return nil, err
	}
	return os.Open(full)
}

// List walks the tree returning slash-separated relative paths.
func (d *dirDest) List(prefix string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(d.dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, err := filepath.Rel(d.dir, path)
		if err != nil {
			return err
		}
		slashed := filepath.ToSlash(rel)
		if strings.HasPrefix(slashed, prefix) {
			out = append(out, slashed)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}
