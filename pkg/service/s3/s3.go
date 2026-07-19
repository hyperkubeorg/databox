// Package s3 is the S3-compatible gateway layer (§14): a
// stateless server translating the core S3 HTTP API onto databox blobs
// under /s3/<bucket>/<object>, authenticated with AWS SigV4 against databox
// access keys (§7.1).
//
// # Identity and grants
//
// The gateway connects to the cluster as a configured operator identity
// (default: root) which it uses only to resolve access keys and read user
// grant records from the `.databox/` system view (§19). Each request is
// authenticated by verifying its SigV4 signature against the access key's
// secret, and then authorized by evaluating the owning user's grants with
// the very same resolver the storage core uses (pkg/auth) — so an S3 caller
// sees exactly the buckets/objects their grants allow, identical to the KV
// and SQL paths.
//
// # Mapping (§14)
//
//	bucket b, object o        →  key /s3/<b>/<o>
//	bucket marker             →  key /s3/<b>   (empty value; lets ListBuckets work)
//	objects are databox blobs; buckets are prefixes; grants apply as everywhere.
package s3

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// Options configures the S3 gateway.
type Options struct {
	Listen        string // S3 listen address, default :9000
	Cluster       string // databox cluster endpoint (host:port)
	CAFingerprint string // pinned cluster cert fingerprint (optional)
	TLSCertFile   string // this layer's own TLS cert (optional)
	TLSKeyFile    string
	// AllowCleartext permits serving plain HTTP when no TLS cert is
	// configured. The default is to refuse: SigV4 secrets, session
	// payloads and presigned URLs are all replayable if sniffed (§6.1).
	AllowCleartext bool
	Region         string // SigV4 region label to accept (default "us-east-1")
	// RootPrefix is the KV prefix buckets/objects map under (§14).
	// Default "/s3/"; always normalized to have leading and trailing "/".
	RootPrefix string
	// MultipartTTL is how long an unfinished multipart upload may live
	// before the lazy sweep deletes its parts (default 7 days).
	MultipartTTL time.Duration
	// ClockSkew is the maximum tolerated difference between x-amz-date and
	// the gateway clock (default ±15 minutes, AWS's own window).
	ClockSkew time.Duration
	// Operator credentials the gateway uses to resolve keys/grants. Default
	// is root with no password (a fresh cluster); production sets these.
	OperatorUser     string
	OperatorPassword string
	Logger           *slog.Logger
}

// gateway is the running S3 service.
type gateway struct {
	opts   Options
	log    *slog.Logger
	admin  *client.Client // operator connection for key/grant resolution + IO
	tlsCfg *tls.Config
	root   string // normalized RootPrefix, e.g. "/s3/"
}

// normalizeRoot forces a root prefix into "/name/.../" shape so key
// construction can blindly concatenate.
func normalizeRoot(p string) string {
	if p == "" {
		p = "/s3/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// Run starts the gateway and blocks until ctx is cancelled.
func Run(ctx context.Context, opts Options) error {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Listen == "" {
		opts.Listen = ":9000"
	}
	if opts.Region == "" {
		opts.Region = "us-east-1"
	}
	if opts.OperatorUser == "" {
		opts.OperatorUser = "root"
	}
	g := &gateway{opts: opts, log: opts.Logger, root: normalizeRoot(opts.RootPrefix)}

	admin, err := g.newClient()
	if err != nil {
		return fmt.Errorf("connect to cluster: %w", err)
	}
	if err := admin.Login(ctx, opts.OperatorUser, opts.OperatorPassword); err != nil {
		return fmt.Errorf("operator login: %w", err)
	}
	g.admin = admin

	if opts.TLSCertFile != "" && opts.TLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(opts.TLSCertFile, opts.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("load S3 TLS cert: %w", err)
		}
		// User-facing APIs are TLS 1.3+ only (§6.1).
		g.tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	} else if !opts.AllowCleartext {
		// Secure by default: never serve SigV4-authenticated traffic over
		// plaintext unless the operator explicitly opted in (§6.1).
		return fmt.Errorf("no TLS certificate configured and cleartext not allowed; set TLSCertFile/TLSKeyFile or AllowCleartext")
	}

	srv := &http.Server{Addr: opts.Listen, Handler: g.router(), TLSConfig: g.tlsCfg}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5e9)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	g.log.Info("s3 gateway listening", "addr", opts.Listen, "cluster", opts.Cluster, "tls", g.tlsCfg != nil)

	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", opts.Listen, err)
	}
	if g.tlsCfg != nil {
		err = srv.ServeTLS(ln, "", "")
	} else {
		err = srv.Serve(ln)
	}
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// newClient dials the cluster with the gateway's TLS trust settings, the
// same pin-or-trust-on-first-use policy the SQL layer uses (§6.3).
func (g *gateway) newClient() (*client.Client, error) {
	opts := client.Options{Endpoint: g.opts.Cluster}
	if g.opts.CAFingerprint != "" {
		opts.TrustFingerprints = []string{g.opts.CAFingerprint}
	} else {
		opts.OnUnknownCert = func(fp string, _ *x509.Certificate) bool {
			g.log.Warn("trusting cluster certificate on first use", "fingerprint", fp)
			return true
		}
	}
	return client.New(opts)
}
