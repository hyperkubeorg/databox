// postoffice is Personal Cloud Platform's mail gateway: a single
// static binary the operator runs on a cloud host with mail-capable
// networking. Build it, upload it, then:
//
//	postoffice setup --data-dir /var/lib/postoffice
//	    pair with a PCP instance (paste the admin console's setup code,
//	    paste the printed completion code back)
//
//	postoffice run --data-dir /var/lib/postoffice
//	    serve: the HTTPS control plane PCP dials, and (once configured
//	    by PCP's config push) the public SMTP listener
//
// There is deliberately almost nothing to configure here: listen
// addresses and the data dir. Domains, recipients, DKIM keys, limits,
// spam policy — all of it is pushed from the PCP admin console over the
// paired channel. Authority flows PCP → postoffice.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/postoffice"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "setup":
		fs := flag.NewFlagSet("setup", flag.ExitOnError)
		dataDir := fs.String("data-dir", "/var/lib/postoffice", "state directory (identity, TLS, spool)")
		_ = fs.Parse(args)
		if err := postoffice.RunSetup(*dataDir, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "setup failed:", err)
			os.Exit(1)
		}
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		dataDir := fs.String("data-dir", "/var/lib/postoffice", "state directory (identity, TLS, spool)")
		httpsListen := fs.String("https-listen", ":8443", "control-plane listen address (PCP dials this)")
		smtpListen := fs.String("smtp-listen", ":25", "SMTP listen address (reserved until a config push enables mail)")
		_ = fs.Parse(args)
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		st, err := postoffice.Load(*dataDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		srv, err := postoffice.NewServer(st, log)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		go func() {
			if err := srv.RunSMTP(*smtpListen); err != nil {
				log.Error("smtp listener died", "err", err)
				os.Exit(1)
			}
		}()
		go srv.RunOutbound(context.Background())
		if err := srv.ListenAndServe(*httpsListen); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "version":
		fmt.Println("postoffice", postoffice.Version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  postoffice setup [--data-dir DIR]
  postoffice run   [--data-dir DIR] [--https-listen :8443] [--smtp-listen :25]
  postoffice version`)
}
