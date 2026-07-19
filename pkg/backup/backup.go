// Package backup implements the destination side of databox's backup &
// restore system (§17).
//
// A backup destination is anything that can hold a set of named files:
// the Dest interface below. Two production implementations exist —
// S3-compatible object storage (client-side SigV4 signing over plain
// net/http, path-style URLs, custom endpoints supported) and SFTP
// (password auth, host key trusted on first use with the fingerprint
// logged). A third implementation, dirDest, writes to a local directory
// and exists for tests and local development only.
//
// The backup engine itself runs inside the server process
// (pkg/server/backup.go); this package deliberately knows nothing about
// keys, shards, or manifests — it only moves bytes to and from a
// destination.
//
// # Credential handling
//
// This package itself persists nothing: Credentials arrive at Open and
// live in the constructed Dest. Per §17, the SERVER additionally holds
// them AES-256-GCM-encrypted in the system keyspace for the job's
// lifetime (key derived from cluster secret material — see
// pkg/server/backup.go, sealCreds) and purges them on completion or
// cancellation, so any node can resume a job after a coordinator crash
// without the operator re-supplying secrets. Credentials are never
// logged; persisted URLs are redacted.
package backup

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
)

// Dest is a backup destination: a flat namespace of files addressed by
// slash-separated relative paths ("kv-000001.jsonl", "blobs/<sha256>",
// "manifest.json").
type Dest interface {
	// Put stores the full contents of r at path, overwriting any
	// existing file. Implementations must not report success until the
	// data is durably handed to the destination.
	Put(path string, r io.Reader) error
	// Get opens the file at path for reading. A missing file returns an
	// error satisfying errors.Is(err, fs.ErrNotExist) where the
	// implementation can tell.
	Get(path string) (io.ReadCloser, error)
	// List returns the relative paths of every file under prefix
	// (prefix "" lists everything). Order is unspecified.
	List(prefix string) ([]string, error)
}

// Credentials carries the destination secrets supplied at job-issue time.
// See the package comment for how they are held at rest (encrypted, by
// the server — never in cleartext, never logged).
type Credentials struct {
	AccessKey  string // S3 access key ID
	SecretKey  string // S3 secret access key
	S3Endpoint string // S3 endpoint override, e.g. "https://minio.local:9000"; empty = AWS
	Region     string // S3 signing region; empty = "us-east-1"

	SFTPPassword string // SFTP password (password auth only in v1)
}

// Open parses a destination URL and constructs the matching Dest.
//
// Supported URL forms:
//
//	s3://bucket/prefix          (endpoint/keys/region from Credentials)
//	sftp://user@host[:port]/path
//	file:///absolute/path       (local directory — tests and dev only)
//
// The second return value is a redacted form of the URL, safe to persist
// in the job record and to log (any userinfo password is masked).
func Open(rawURL string, creds Credentials, logger *slog.Logger) (Dest, string, error) {
	if logger == nil {
		logger = slog.Default()
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("parse destination url: %w", err)
	}
	switch u.Scheme {
	case "s3":
		if u.Host == "" {
			return nil, "", fmt.Errorf("s3 destination needs a bucket: s3://bucket/prefix")
		}
		if creds.AccessKey == "" || creds.SecretKey == "" {
			return nil, "", fmt.Errorf("s3 destination requires access_key and secret_key")
		}
		d, err := newS3Dest(u.Host, strings.Trim(u.Path, "/"), creds)
		return d, u.Redacted(), err
	case "sftp":
		user := ""
		if u.User != nil {
			user = u.User.Username()
			// Allow sftp://user:pass@host for convenience, but prefer the
			// out-of-band sftp_password field so passwords stay out of URLs.
			if pw, ok := u.User.Password(); ok && creds.SFTPPassword == "" {
				creds.SFTPPassword = pw
			}
		}
		if user == "" {
			return nil, "", fmt.Errorf("sftp destination needs a user: sftp://user@host/path")
		}
		host := u.Host
		if !strings.Contains(host, ":") {
			host += ":22" // default SSH port
		}
		d, err := newSFTPDest(host, user, creds.SFTPPassword, u.Path, logger)
		return d, u.Redacted(), err
	case "file":
		// Local-directory destination: intended for tests and local
		// development; a real backup should leave the machine.
		if u.Path == "" {
			return nil, "", fmt.Errorf("file destination needs a path: file:///dir")
		}
		d, err := newDirDest(u.Path)
		return d, u.Redacted(), err
	default:
		return nil, "", fmt.Errorf("unsupported destination scheme %q (want s3, sftp, or file)", u.Scheme)
	}
}
