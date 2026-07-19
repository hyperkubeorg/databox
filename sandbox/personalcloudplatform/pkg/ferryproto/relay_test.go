package ferryproto

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func TestValidateTCPRelays(t *testing.T) {
	ok := []TCPRelay{
		{EdgePort: 22, TargetPort: 4222, Label: "ssh for git testing"},
		{EdgePort: 2222, TargetPort: 22},
	}
	if err := ValidateTCPRelays(ok); err != nil {
		t.Fatalf("valid list refused: %v", err)
	}
	if err := ValidateTCPRelays(nil); err != nil {
		t.Fatalf("empty list refused: %v", err)
	}
	cases := []struct {
		name   string
		relays []TCPRelay
	}{
		{"zero edge port", []TCPRelay{{EdgePort: 0, TargetPort: 4222}}},
		{"zero target port", []TCPRelay{{EdgePort: 22, TargetPort: 0}}},
		{"duplicate edge ports", []TCPRelay{{EdgePort: 22, TargetPort: 4222}, {EdgePort: 22, TargetPort: 5222}}},
		{"long label", []TCPRelay{{EdgePort: 22, TargetPort: 4222, Label: strings.Repeat("x", 61)}}},
	}
	for _, c := range cases {
		if err := ValidateTCPRelays(c.relays); err == nil {
			t.Errorf("%s accepted", c.name)
		}
	}
	// Over the cap.
	many := make([]TCPRelay, MaxTCPRelays+1)
	for i := range many {
		many[i] = TCPRelay{EdgePort: uint16(10000 + i), TargetPort: 4222}
	}
	if err := ValidateTCPRelays(many); err == nil {
		t.Error("oversized relay list accepted")
	}
	if err := ValidateTCPRelays(many[:MaxTCPRelays]); err != nil {
		t.Errorf("list at the cap refused: %v", err)
	}
	// A >65535 port dies at JSON decode (uint16), not silently truncates.
	var r TCPRelay
	if err := json.Unmarshal([]byte(`{"edge_port":70000,"target_port":4222}`), &r); err == nil {
		t.Error("out-of-range port decoded")
	}
}

func TestConfigPushCarriesRelays(t *testing.T) {
	cp := ConfigPush{
		Serial:    3,
		TCPRelays: []TCPRelay{{EdgePort: 22, TargetPort: 4222, Label: "ssh"}},
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	var got ConfigPush
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.TCPRelays) != 1 || got.TCPRelays[0].EdgePort != 22 ||
		got.TCPRelays[0].TargetPort != 4222 || got.TCPRelays[0].Label != "ssh" {
		t.Fatalf("relay round-trip: %+v", got.TCPRelays)
	}
	// Status rows round-trip too (the admin page renders them).
	st := StatusResponse{TCPRelays: []TCPRelayStatus{
		{EdgePort: 22, TargetPort: 4222, ActiveConns: 2, Bytes: 999, Error: "bind: address already in use"},
	}}
	raw, _ = json.Marshal(st)
	var gotSt StatusResponse
	if err := json.Unmarshal(raw, &gotSt); err != nil {
		t.Fatal(err)
	}
	if len(gotSt.TCPRelays) != 1 || gotSt.TCPRelays[0].Bytes != 999 || gotSt.TCPRelays[0].Error == "" {
		t.Fatalf("status round-trip: %+v", gotSt.TCPRelays)
	}
}

// TestRelayHeaderFraming proves the header reader consumes EXACTLY the
// magic+header line — the first payload byte after the newline must
// survive for the splice.
func TestRelayHeaderFraming(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		if err := WriteRelayHeader(a, 4222); err != nil {
			t.Error(err)
		}
		_, _ = a.Write([]byte("SSH-2.0-payload"))
	}()
	one := make([]byte, 1)
	if _, err := b.Read(one); err != nil || one[0] != RelayMagic {
		t.Fatalf("magic byte: %v %q", err, one)
	}
	port, err := ReadRelayHeader(b)
	if err != nil || port != 4222 {
		t.Fatalf("header: port=%d err=%v", port, err)
	}
	buf := make([]byte, 32)
	n, err := b.Read(buf)
	if err != nil || !strings.HasPrefix(string(buf[:n]), "SSH-2.0") {
		t.Fatalf("payload after header: %q err=%v", buf[:n], err)
	}

	// Junk headers are refused: bad JSON, wrong version, zero port, too long.
	for _, junk := range []string{"{oops\n", `{"v":2,"port":1}` + "\n", `{"v":1,"port":0}` + "\n", strings.Repeat("x", 200) + "\n"} {
		if _, err := ReadRelayHeader(strings.NewReader(junk)); err == nil {
			t.Errorf("junk header %q accepted", junk[:min(12, len(junk))])
		}
	}
}

// TestSpliceBothWaysAndHalfClose is the byte-splice contract: bytes
// flow both directions, an EOF on one side becomes a write shutdown on
// the other (the far reader drains and finishes), and both counters
// report what moved.
func TestSpliceBothWaysAndHalfClose(t *testing.T) {
	// Real TCP sockets — net.Pipe has no CloseWrite.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dial := func() (client, server net.Conn) {
		t.Helper()
		type acc struct {
			c   net.Conn
			err error
		}
		ch := make(chan acc, 1)
		go func() { c, err := ln.Accept(); ch <- acc{c, err} }()
		client, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		a := <-ch
		if a.err != nil {
			t.Fatal(a.err)
		}
		return client, a.c
	}

	// Pair 1 <-> splice <-> pair 2: client1 talks to client2 through it.
	c1, s1 := dial()
	c2, s2 := dial()
	spliced := make(chan [2]int64, 1)
	go func() {
		aToB, bToA := Splice(s1, c2, time.Minute)
		spliced <- [2]int64{aToB, bToA}
	}()

	// c1 → c2 direction (server-first banner works too — no ordering).
	if _, err := c1.Write([]byte("hello from edge")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := s2.Read(buf)
	if err != nil || string(buf[:n]) != "hello from edge" {
		t.Fatalf("edge→target: %q %v", buf[:n], err)
	}
	// c2 ← s2 direction.
	if _, err := s2.Write([]byte("echo back")); err != nil {
		t.Fatal(err)
	}
	n, err = c1.Read(buf)
	if err != nil || string(buf[:n]) != "echo back" {
		t.Fatalf("target→edge: %q %v", buf[:n], err)
	}

	// Half-close: c1 shuts its write side; s2 must see EOF while the
	// reverse path still works.
	_ = c1.(*net.TCPConn).CloseWrite()
	if n, err := s2.Read(buf); err == nil {
		t.Fatalf("target didn't see EOF after edge half-close (got %d bytes)", n)
	}
	if _, err := s2.Write([]byte("last words")); err != nil {
		t.Fatalf("write after peer half-close: %v", err)
	}
	n, _ = c1.Read(buf)
	if string(buf[:n]) != "last words" {
		t.Fatalf("post-half-close delivery: %q", buf[:n])
	}
	_ = s2.Close()

	select {
	case counts := <-spliced:
		if counts[0] != int64(len("hello from edge")) || counts[1] != int64(len("echo back")+len("last words")) {
			t.Fatalf("byte counts: %v", counts)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("splice never finished after both sides closed")
	}
}

// TestSpliceSurvivesFirstDirectionClose is the regression test for the
// premature-return bug: when the target→edge direction EOFs FIRST, the
// edge→target direction must keep flowing until its own EOF — Splice
// may not tear the pair down after just one side finishes.
func TestSpliceSurvivesFirstDirectionClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dial := func() (client, server net.Conn) {
		t.Helper()
		ch := make(chan net.Conn, 1)
		go func() { c, _ := ln.Accept(); ch <- c }()
		client, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return client, <-ch
	}
	c1, s1 := dial()
	c2, s2 := dial()
	defer c1.Close()
	defer s2.Close()
	spliced := make(chan struct{})
	go func() { Splice(s1, c2, time.Minute); close(spliced) }()

	// The INLINE direction (b→a, i.e. s2→…→c1) finishes first.
	_ = s2.(*net.TCPConn).CloseWrite()
	buf := make([]byte, 64)
	if _, err := c1.Read(buf); err == nil {
		t.Fatal("edge never saw the propagated EOF")
	}
	// …and the other direction still works afterwards.
	if _, err := c1.Write([]byte("still flowing")); err != nil {
		t.Fatal(err)
	}
	_ = s2.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := s2.Read(buf)
	if err != nil || string(buf[:n]) != "still flowing" {
		t.Fatalf("live direction died with the closed one: %q %v", buf[:n], err)
	}
	select {
	case <-spliced:
		t.Fatal("splice returned while one direction was still open")
	default:
	}
	_ = c1.(*net.TCPConn).CloseWrite()
	select {
	case <-spliced:
	case <-time.After(5 * time.Second):
		t.Fatal("splice never finished after both EOFs")
	}
}

// TestSpliceIdleTimeout proves a silent pair is torn down.
func TestSpliceIdleTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 2)
	go func() {
		for range 2 {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			ch <- c
		}
	}()
	c1, _ := net.Dial("tcp", ln.Addr().String())
	c2, _ := net.Dial("tcp", ln.Addr().String())
	s1, s2 := <-ch, <-ch
	defer c1.Close()
	defer c2.Close()
	done := make(chan struct{})
	go func() { Splice(s1, s2, 200*time.Millisecond); close(done) }()
	select {
	case <-done: // idle watchdog fired
	case <-time.After(5 * time.Second):
		t.Fatal("idle splice never torn down")
	}
}
