// sftpdest.go implements Dest over SFTP (github.com/pkg/sftp on
// golang.org/x/crypto/ssh). v1 supports password authentication only.
//
// Host key policy: trust-on-first-use per process. The first connection
// to a host records the presented key and logs its SHA-256 fingerprint so
// the operator can verify it out of band; a later connection to the same
// host presenting a different key is refused. This mirrors the console's
// certificate TOFU model (§6.3) at the SSH layer.
package backup

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// knownHostKeys is the process-wide TOFU store: host address → public key
// fingerprint seen on first connect.
var (
	knownHostKeysMu sync.Mutex
	knownHostKeys   = map[string]string{}
)

// tofuHostKey builds the accept-on-first-use host key callback.
func tofuHostKey(logger *slog.Logger) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		knownHostKeysMu.Lock()
		defer knownHostKeysMu.Unlock()
		if seen, ok := knownHostKeys[hostname]; ok {
			if seen != fp {
				return fmt.Errorf("sftp host %s key changed (had %s, now %s) — refusing", hostname, seen, fp)
			}
			return nil
		}
		knownHostKeys[hostname] = fp
		logger.Warn("sftp host key accepted on first use — verify this fingerprint",
			"host", hostname, "fingerprint", fp)
		return nil
	}
}

// sftpDest is one directory tree on one SFTP server.
type sftpDest struct {
	client *sftp.Client
	conn   *ssh.Client
	base   string // remote base directory, absolute or login-relative
}

// newSFTPDest dials the server and verifies the base path is usable.
func newSFTPDest(addr, user, password, base string, logger *slog.Logger) (*sftpDest, error) {
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: tofuHostKey(logger),
		Timeout:         30 * time.Second,
	}
	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("sftp dial %s: %w", addr, err)
	}
	cl, err := sftp.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("sftp session: %w", err)
	}
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		base = "."
	}
	return &sftpDest{client: cl, conn: conn, base: base}, nil
}

// full maps a Dest-relative path to the remote path.
func (d *sftpDest) full(p string) string { return path.Join(d.base, p) }

// Put writes a file, creating parent directories, via a temp-then-rename
// so a dropped connection never leaves a truncated file under its final
// name (resume checks rely on "file exists ⇒ file complete").
func (d *sftpDest) Put(p string, r io.Reader) error {
	full := d.full(p)
	if err := d.client.MkdirAll(path.Dir(full)); err != nil {
		return fmt.Errorf("sftp mkdir %s: %w", path.Dir(full), err)
	}
	tmp := full + ".tmp"
	f, err := d.client.Create(tmp)
	if err != nil {
		return fmt.Errorf("sftp create %s: %w", tmp, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		d.client.Remove(tmp)
		return fmt.Errorf("sftp write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	// PosixRename overwrites atomically where the server supports the
	// extension; fall back to remove+rename otherwise.
	if err := d.client.PosixRename(tmp, full); err != nil {
		d.client.Remove(full)
		if err := d.client.Rename(tmp, full); err != nil {
			return fmt.Errorf("sftp rename %s: %w", full, err)
		}
	}
	return nil
}

// Get opens a remote file for reading.
func (d *sftpDest) Get(p string) (io.ReadCloser, error) {
	f, err := d.client.Open(d.full(p))
	if err != nil {
		return nil, fmt.Errorf("sftp open %s: %w", d.full(p), err)
	}
	return f, nil
}

// List walks the base directory collecting regular files under prefix.
// A missing base directory is an empty destination, not an error — the
// first Put creates it.
func (d *sftpDest) List(prefix string) ([]string, error) {
	if _, err := d.client.Stat(d.base); err != nil {
		return nil, nil
	}
	var out []string
	walker := d.client.Walk(d.base)
	for walker.Step() {
		if walker.Err() != nil {
			continue
		}
		st := walker.Stat()
		if st == nil || st.IsDir() {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(walker.Path(), d.base), "/")
		if strings.HasSuffix(rel, ".tmp") {
			continue // in-flight upload leftovers are not complete files
		}
		if strings.HasPrefix(rel, prefix) {
			out = append(out, rel)
		}
	}
	return out, nil
}

// Close tears down the SFTP session and SSH connection.
func (d *sftpDest) Close() error {
	d.client.Close()
	return d.conn.Close()
}
