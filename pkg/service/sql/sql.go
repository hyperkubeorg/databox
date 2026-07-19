// Package sql is the SQL processing layer (§13): a
// stateless server speaking the PostgreSQL wire protocol v3 and executing
// the chai SQL dialect against the databox KV store.
//
// # Architecture
//
// The layer holds no data. Each client connection authenticates with a
// databox username/password (verified by logging in to the cluster), and
// every statement then runs as an authenticated client of the cluster —
// so the §7.2 grant model applies to SQL exactly as it does to the raw KV
// API, at table granularity via the /sql/<db>/<table>/ key mapping. Scale
// the layer by running more instances; they share nothing.
//
// # Pieces
//
//	lexer.go / parser.go / ast.go  the chai-dialect front end
//	value.go / keyenc.go           the type system and order-preserving keys
//	catalog.go                     table/index schema on the cluster
//	eval.go / query.go / aggregate.go / dml.go / exec.go   the executor
//	pgwire.go / wire.go            the PostgreSQL v3 protocol server
package sql

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// Options configures the SQL layer.
type Options struct {
	Listen        string // pg wire listen address, default :5432
	Cluster       string // databox cluster endpoint (host:port)
	CAFingerprint string // pinned cluster cert fingerprint (optional)
	TLSCertFile   string // this layer's own TLS cert (optional)
	TLSKeyFile    string
	// AllowCleartext permits connections without TLS. Passwords travel on
	// this stream, so cleartext is refused by default; set this only on a
	// trusted network (and expect cmd/ to expose it as an explicit flag).
	AllowCleartext bool
	Logger         *slog.Logger
}

// layer is the running SQL service: it accepts pg connections and, for each,
// dials the cluster as the authenticating user.
type layer struct {
	opts   Options
	log    *slog.Logger
	tlsCfg *tls.Config // non-nil when serving TLS to pg clients
}

// Run starts the SQL layer and blocks until ctx is cancelled.
func Run(ctx context.Context, opts Options) error {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Listen == "" {
		opts.Listen = ":5432"
	}
	l := &layer{opts: opts, log: opts.Logger}

	// Serve TLS to pg clients when a certificate is configured. Password
	// auth is only safe over TLS, so connections without it are refused
	// unless AllowCleartext explicitly opts in (trusted networks only).
	if opts.TLSCertFile != "" && opts.TLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(opts.TLSCertFile, opts.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("load SQL layer TLS cert: %w", err)
		}
		l.tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	}

	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", opts.Listen, err)
	}
	defer ln.Close()
	l.log.Info("sql layer listening", "addr", opts.Listen, "cluster", opts.Cluster, "tls", l.tlsCfg != nil)

	// Close the listener when the context ends so Accept unblocks.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				l.log.Warn("accept failed", "err", err)
				continue
			}
		}
		go l.serveConn(c)
	}
}

// newClient dials the cluster with the layer's TLS trust settings. A pinned
// CA fingerprint is preferred; absent one, the layer trusts the cluster's
// certificate on first use (the same trust-on-first-use path the console
// uses, §6.3) — never InsecureSkipVerify.
func (l *layer) newClient() (*client.Client, error) {
	opts := client.Options{Endpoint: l.opts.Cluster}
	if l.opts.CAFingerprint != "" {
		opts.TrustFingerprints = []string{l.opts.CAFingerprint}
	} else {
		opts.OnUnknownCert = func(fp string, _ *x509.Certificate) bool {
			l.log.Warn("trusting cluster certificate on first use", "fingerprint", fp)
			return true
		}
	}
	return client.New(opts)
}

// ctxBackground returns a background context for per-statement work. It is a
// function so pgwire.go does not import context directly for this one use.
func ctxBackground() context.Context { return context.Background() }
