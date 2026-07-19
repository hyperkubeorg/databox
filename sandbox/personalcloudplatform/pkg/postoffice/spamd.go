// spamd.go — optional SpamAssassin scoring over the SPAMC protocol
// (spamd is one-field-easy and never required). A
// configured endpoint is health-checked for the dashboard; scoring
// fails OPEN (score 0) so an unreachable spamd never blocks mail.
package postoffice

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// spamScore asks spamd for a message's score via SPAMC CHECK. Returns 0
// on any error (fail open).
func spamScore(addr string, raw []byte) float64 {
	if addr == "" {
		return 0
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return 0
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	// SPAMC CHECK request: headers, blank line, then the message.
	fmt.Fprintf(conn, "CHECK SPAMC/1.2\r\nContent-length: %d\r\n\r\n", len(raw))
	_, _ = conn.Write(raw)
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}

	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := sc.Text()
		// Spam: True ; 6.1 / 5.0
		if strings.HasPrefix(line, "Spam:") {
			if i := strings.Index(line, ";"); i >= 0 {
				fields := strings.Fields(line[i+1:])
				if len(fields) > 0 {
					if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
						return v
					}
				}
			}
		}
	}
	return 0
}

// spamdHealthy reports whether a configured spamd answers PING, for the
// status dashboard (so a misconfigured endpoint is visible, not silent).
func spamdHealthy(addr string) bool {
	if addr == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(conn, "PING SPAMC/1.2\r\n\r\n")
	sc := bufio.NewScanner(conn)
	if sc.Scan() {
		return strings.Contains(sc.Text(), "PONG") || strings.Contains(sc.Text(), "EX_OK")
	}
	return false
}
