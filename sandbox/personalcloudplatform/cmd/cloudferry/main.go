// cloudferry is Personal Cloud Platform's web gateway: a single static
// binary the operator runs on any public cloud instance to make a PCP
// reachable from the internet without the PCP network accepting a
// single inbound connection. Build it, upload it, then:
//
//	cloudferry setup --data-dir /var/lib/cloudferry
//	    pair with a PCP instance (paste the admin console's setup code,
//	    paste the printed completion code back). One cloudferry, one
//	    PCP — re-pairing requires wiping the data dir.
//
//	cloudferry run --data-dir /var/lib/cloudferry
//	    serve: the public HTTP/HTTPS edge, the tunnel port PCP's stream
//	    pool dials, and the HTTPS control plane PCP configures it over
//
// There is deliberately almost nothing to configure here: listen
// addresses and the data dir. Hostnames, TLS modes, certificates,
// limits, the offline page — all pushed from the PCP admin console
// over the paired channel. Authority flows PCP → cloudferry.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/cloudferry"
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
		dataDir := fs.String("data-dir", "/var/lib/cloudferry", "state directory (identity, TLS, cached config)")
		_ = fs.Parse(args)
		if err := cloudferry.RunSetup(*dataDir, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "setup failed:", err)
			os.Exit(1)
		}
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		dataDir := fs.String("data-dir", "/var/lib/cloudferry", "state directory (identity, TLS, cached config)")
		httpListen := fs.String("http", ":80", "public HTTP listen address")
		httpsListen := fs.String("https", ":443", "public HTTPS listen address")
		tunnelListen := fs.String("tunnel", ":7443", "tunnel listen address (PCP's stream pool dials this)")
		controlListen := fs.String("control", ":7444", "control-plane listen address (PCP dials this)")
		_ = fs.Parse(args)
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		st, err := cloudferry.Load(*dataDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		srv, err := cloudferry.NewServer(st, log)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		// The gateway's own ports are off-limits to TCP relays.
		srv.SetReservedPorts(portOf(*httpListen), portOf(*httpsListen), portOf(*tunnelListen), portOf(*controlListen))
		die := func(what string, err error) {
			log.Error(what+" died", "err", err)
			os.Exit(1)
		}
		go func() { die("public http", srv.ListenAndServePublicHTTP(*httpListen)) }()
		go func() { die("public https", srv.ListenAndServePublicHTTPS(*httpsListen)) }()
		go func() { die("tunnel", srv.ListenAndServeTunnel(*tunnelListen)) }()
		die("control plane", srv.ListenAndServeControl(*controlListen))
	case "version":
		fmt.Println("cloudferry", cloudferry.Version)
	default:
		usage()
		os.Exit(2)
	}
}

// portOf extracts the port from a listen address ("" on failure — an
// unparsable flag will fail at bind with a clearer error anyway).
func portOf(addr string) uint16 {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(n)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  cloudferry setup [--data-dir DIR]
  cloudferry run   [--data-dir DIR] [--http :80] [--https :443] [--tunnel :7443] [--control :7444]
  cloudferry version`)
}
