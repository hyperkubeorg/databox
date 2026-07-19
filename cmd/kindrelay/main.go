// Command kindrelay backs the Makefile's OPTIONAL relay-* targets: a
// dumb TCP relay from a friendly localhost port to a NodePort the kind
// cluster already publishes on the host (kind.yaml extraPortMappings).
// The default dev tooling is plain `make port-forward-*` (kubectl);
// this is the streaming-safe alternative.
//
// It exists because kubectl port-forward multiplexes every connection
// over one SPDY stream through the API server, and sustained transfers
// (video streaming, large blobs) can wedge that tunnel until the
// process is restarted — "error creating error stream … Timeout
// occurred" on every connection after the first stall. A raw TCP relay
// to the published NodePort has no tunnel to wedge and runs at wire
// speed; TLS traffic (databox, pg) passes through untouched.
//
// Usage: kindrelay <listen-addr> <target-addr>
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <listen-addr> <target-addr>\n", os.Args[0])
		os.Exit(2)
	}
	listen, target := os.Args[1], os.Args[2]

	// Probe before listening: a dead target means the kind cluster is
	// down or predates the kind.yaml mapping for this port — fail loud
	// now, not on the first browser request.
	probe, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s is not reachable: %v\n\n", target, err)
		fmt.Fprintln(os.Stderr, "Is the kind cluster up? kind host-port mappings only apply at cluster")
		fmt.Fprintln(os.Stderr, "creation — if kind.yaml gained this port after the cluster was made,")
		fmt.Fprintln(os.Stderr, "run `make kind-down && make kind-up` once.")
		os.Exit(1)
	}
	probe.Close()

	ls, err := listeners(listen)
	if err != nil {
		log.Fatalf("listen %s: %v", listen, err)
	}
	for _, l := range ls {
		log.Printf("relaying %s -> %s", l.Addr(), target)
		go func(l net.Listener) {
			for {
				conn, err := l.Accept()
				if err != nil {
					log.Fatalf("accept: %v", err)
				}
				go relay(conn.(*net.TCPConn), target)
			}
		}(l)
	}
	select {} // serve forever; accept loops above own the lifecycle
}

// listeners binds the listen address. "localhost" binds BOTH loopbacks
// (127.0.0.1 and ::1) like kubectl port-forward does — browsers often
// resolve localhost to ::1 first, and a v4-only listener looks dead to
// them. Anything else binds exactly what was asked.
func listeners(addr string) ([]net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "localhost" {
		l, lerr := net.Listen("tcp", addr)
		if lerr != nil {
			return nil, lerr
		}
		return []net.Listener{l}, nil
	}
	var ls []net.Listener
	l4, err4 := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err4 == nil {
		ls = append(ls, l4)
	}
	if l6, err6 := net.Listen("tcp", net.JoinHostPort("::1", port)); err6 == nil {
		ls = append(ls, l6)
	}
	if len(ls) == 0 {
		return nil, err4
	}
	return ls, nil
}

// relay pumps bytes both ways and half-closes each direction as its
// source ends, so protocols that shut down one side first (pg, TLS
// close_notify) terminate cleanly instead of hanging.
func relay(c *net.TCPConn, target string) {
	defer c.Close()
	t, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		log.Printf("dial %s: %v", target, err)
		return
	}
	tc := t.(*net.TCPConn)
	defer tc.Close()
	done := make(chan struct{}, 2)
	var fromBackend int64
	pump := func(dst, src *net.TCPConn, n *int64) {
		copied, _ := io.Copy(dst, src) // a torn stream just ends the pair
		if n != nil {
			*n = copied
		}
		dst.CloseWrite()
		done <- struct{}{}
	}
	go pump(tc, c, nil)
	go pump(c, tc, &fromBackend)
	<-done
	<-done
	// The empty-response signature: the backend accepted, sent NOTHING,
	// and closed. That means the published port itself is dead (e.g. a
	// runtime proxy socket whose inner leg fails) — say so instead of
	// leaving the browser error as the only clue.
	if fromBackend == 0 {
		log.Printf("%s accepted a connection but sent no data — the published port may not actually serve; try `curl -v http://%s/` directly", target, target)
	}
}
