// relay.go — the generic TCP port-relay vocabulary. A relay maps one
// public edge port on the gateway to one target port on the PCP host:
// the gateway accepts raw TCP, opens ONE tunnel stream per connection,
// and the PCP-side dialer splices it to 127.0.0.1:<target>. The concrete
// use case is SSH (edge 22 → a local sshd/git daemon on 4222), but
// nothing here knows about SSH — it is bytes in, bytes out.
//
// Stream discrimination: the gateway opens every tunnel stream, so it
// alone decides a stream's kind. HTTP streams are UNCHANGED on the wire
// (zero added bytes); a relay stream starts with RelayMagic (0x00 — no
// HTTP request can begin with NUL) followed by one JSON header line,
// then raw bidirectional bytes. The PCP dialer peeks the first byte of
// each accepted stream to route it.
package ferryproto

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"
)

// MaxTCPRelays bounds the config list — a gateway is an edge for one
// PCP, not a port-forwarding farm.
const MaxTCPRelays = 16

// RelayMagic is the first byte of a relay stream. HTTP exchanges start
// with a method token, never NUL, so the byte alone discriminates.
const RelayMagic byte = 0x00

// relayHeaderMax bounds the JSON header line after the magic byte.
const relayHeaderMax = 128

// TCPRelay is one configured port relay (rides ConfigPush).
type TCPRelay struct {
	EdgePort   uint16 `json:"edge_port"`   // public port the gateway listens on
	TargetPort uint16 `json:"target_port"` // 127.0.0.1:<port> on the PCP host
	Label      string `json:"label,omitempty"`
}

// ValidateTCPRelays gates a relay list at every boundary that stores
// one: ports in 1–65535 (uint16 kills >65535 at decode; zero is checked
// here), no duplicate edge ports, at most MaxTCPRelays, short labels.
// Collisions with the gateway's OWN listener ports are checked where
// those ports are known (PCP validates against 80/443/control/tunnel;
// the gateway re-checks against its actual bind flags at reconcile).
func ValidateTCPRelays(relays []TCPRelay) error {
	if len(relays) > MaxTCPRelays {
		return fmt.Errorf("at most %d TCP relays", MaxTCPRelays)
	}
	seen := map[uint16]bool{}
	for _, r := range relays {
		if r.EdgePort == 0 || r.TargetPort == 0 {
			return fmt.Errorf("relay ports must be 1–65535")
		}
		if seen[r.EdgePort] {
			return fmt.Errorf("edge port %d is configured twice", r.EdgePort)
		}
		seen[r.EdgePort] = true
		if len(r.Label) > 60 {
			return fmt.Errorf("relay labels are 60 chars max")
		}
	}
	return nil
}

// TCPRelayStatus is one relay's row in the gateway self-report.
type TCPRelayStatus struct {
	EdgePort    uint16 `json:"edge_port"`
	TargetPort  uint16 `json:"target_port"`
	Label       string `json:"label,omitempty"`
	ActiveConns int    `json:"active_conns"`
	Bytes       uint64 `json:"bytes"` // total relayed, both directions
	// Error is the listener's standing problem ("" = listening) — e.g.
	// EADDRINUSE, or a privileged port the process may not bind.
	Error string `json:"error,omitempty"`
}

// RelayHeader is the JSON line a relay stream opens with (after the
// magic byte).
type RelayHeader struct {
	V    int    `json:"v"`
	Port uint16 `json:"port"` // requested target port — the PCP side allowlists it
}

// WriteRelayHeader opens a relay stream: magic + header + newline in
// one write, so the payload bytes that follow are never coalesced into
// the header line.
func WriteRelayHeader(w io.Writer, targetPort uint16) error {
	raw, err := json.Marshal(RelayHeader{V: 1, Port: targetPort})
	if err != nil {
		return err
	}
	frame := make([]byte, 0, len(raw)+2)
	frame = append(frame, RelayMagic)
	frame = append(frame, raw...)
	frame = append(frame, '\n')
	_, err = w.Write(frame)
	return err
}

// ReadRelayHeader parses the header line AFTER the magic byte was
// consumed. It reads byte-by-byte: an over-reading buffer would swallow
// the relay payload that follows the newline.
func ReadRelayHeader(r io.Reader) (uint16, error) {
	line := make([]byte, 0, 64)
	one := make([]byte, 1)
	for {
		if _, err := r.Read(one); err != nil {
			return 0, fmt.Errorf("relay header: %w", err)
		}
		if one[0] == '\n' {
			break
		}
		if line = append(line, one[0]); len(line) > relayHeaderMax {
			return 0, fmt.Errorf("relay header too long")
		}
	}
	var h RelayHeader
	if err := json.Unmarshal(line, &h); err != nil || h.V != 1 || h.Port == 0 {
		return 0, fmt.Errorf("bad relay header")
	}
	return h.Port, nil
}

// closeWriter is the half-close seam: TCP conns have CloseWrite; a
// yamux stream's Close IS a write half-close (FIN out, reads continue).
type closeWriter interface{ CloseWrite() error }

// halfClose signals "no more bytes from me" without killing reads.
func halfClose(c net.Conn) {
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// Splice relays bytes both ways between a and b until BOTH directions
// finish, with half-close propagation (EOF from one side becomes
// CloseWrite on the other, so protocols that shut down one direction
// first — SSH, git — drain cleanly) and a shared idle timeout: idle>0
// and no byte in either direction for that long tears the pair down.
// Callers must pass idle>0 — the watchdog is also what unsticks a
// half-closed pair whose live direction dies silently. Returns bytes
// moved a→b and b→a. Both conns are closed on return.
func Splice(a, b net.Conn, idle time.Duration) (aToB, bToA int64) {
	defer a.Close()
	defer b.Close()
	var last atomicTime
	last.set(time.Now())
	if idle > 0 {
		watchdogDone := make(chan struct{})
		defer close(watchdogDone)
		go func() { // idle watchdog: kill both sides, the copies unblock
			t := time.NewTicker(idle / 4)
			defer t.Stop()
			for {
				select {
				case <-watchdogDone:
					return
				case <-t.C:
					if time.Since(last.get()) > idle {
						_ = a.Close()
						_ = b.Close()
						return
					}
				}
			}
		}()
	}
	done := make(chan struct{}, 2)
	copyDir := func(dst, src net.Conn, n *int64) {
		buf := make([]byte, 32<<10)
		for {
			r, rerr := src.Read(buf)
			if r > 0 {
				last.set(time.Now())
				*n += int64(r)
				if _, werr := dst.Write(buf[:r]); werr != nil {
					break
				}
			}
			if rerr != nil {
				break
			}
		}
		halfClose(dst) // propagate EOF (or error) as a write shutdown
		done <- struct{}{}
	}
	go copyDir(b, a, &aToB)
	copyDir(a, b, &bToA)
	// Wait for BOTH directions: one side finishing (half-close) must not
	// abort the other mid-transfer. The done channel already holds the
	// inline direction's token; this receive is the goroutine's.
	<-done
	<-done
	return aToB, bToA
}

// atomicTime is a lock-free last-activity clock for the idle watchdog.
type atomicTime struct{ ns atomic.Int64 }

func (t *atomicTime) set(v time.Time) { t.ns.Store(v.UnixNano()) }
func (t *atomicTime) get() time.Time  { return time.Unix(0, t.ns.Load()) }
