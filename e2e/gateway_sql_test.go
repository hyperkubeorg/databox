//go:build e2e

// sqlgateway_test.go — TestSQLGatewayRoundTrip: the §13 SQL layer end to
// end — a real pg wire v3 conversation over TCP against the in-process SQL
// gateway backed by a real cluster. The test speaks the protocol by hand
// (startup, cleartext password auth, simple Query, and the extended
// Parse/Bind/Execute path with $N parameters) — no pg driver dependency.
package e2e

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	sqlsvc "github.com/hyperkubeorg/databox/pkg/service/sql"
)

// startSQLGateway boots the in-process SQL layer against a cluster node and
// waits until it accepts TCP. Cleartext is the test-only transport
// (Options.AllowCleartext); production serves TLS.
func startSQLGateway(t *testing.T, clusterEndpoint string) string {
	t.Helper()
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = sqlsvc.Run(ctx, sqlsvc.Options{
			Listen:         addr,
			Cluster:        clusterEndpoint,
			AllowCleartext: true,
			Logger:         quietLogger(),
		})
	}()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return addr
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("sql gateway on %s never came up", addr)
	return ""
}

// pgConn is a ~100-line pg wire v3 test frontend: framing plus the handful
// of message builders the round trip needs.
type pgConn struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

// dialPG connects, performs startup + cleartext password auth, and waits
// for ReadyForQuery.
func dialPG(t *testing.T, addr, user, password, database string) *pgConn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(120 * time.Second))
	p := &pgConn{t: t, c: c, r: bufio.NewReader(c)}

	// StartupMessage: int32 length, int32 196608, then key/value pairs.
	var body []byte
	for _, kv := range [][2]string{{"user", user}, {"database", database}} {
		body = append(body, kv[0]...)
		body = append(body, 0)
		body = append(body, kv[1]...)
		body = append(body, 0)
	}
	body = append(body, 0)
	msg := binary.BigEndian.AppendUint32(nil, uint32(len(body)+8))
	msg = binary.BigEndian.AppendUint32(msg, 196608)
	if _, err := c.Write(append(msg, body...)); err != nil {
		t.Fatal(err)
	}

	// AuthenticationCleartextPassword ('R' code 3) → PasswordMessage.
	typ, b := p.recv()
	if typ != 'R' || binary.BigEndian.Uint32(b) != 3 {
		t.Fatalf("expected cleartext password request, got %c %v", typ, b)
	}
	p.send('p', append([]byte(password), 0))
	// AuthenticationOk, ParameterStatus*, BackendKeyData, ReadyForQuery.
	if typ, b := p.recv(); typ != 'R' || binary.BigEndian.Uint32(b) != 0 {
		t.Fatalf("authentication failed: %c %s", typ, b)
	}
	p.waitReady()
	return p
}

func (p *pgConn) send(typ byte, body []byte) {
	p.t.Helper()
	hdr := make([]byte, 5)
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(body)+4))
	if _, err := p.c.Write(append(hdr, body...)); err != nil {
		p.t.Fatalf("send %c: %v", typ, err)
	}
}

func (p *pgConn) recv() (byte, []byte) {
	p.t.Helper()
	typ, err := p.r.ReadByte()
	if err != nil {
		p.t.Fatalf("recv: %v", err)
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(p.r, lenBuf[:]); err != nil {
		p.t.Fatalf("recv len: %v", err)
	}
	n := int(binary.BigEndian.Uint32(lenBuf[:])) - 4
	body := make([]byte, n)
	if _, err := io.ReadFull(p.r, body); err != nil {
		p.t.Fatalf("recv body: %v", err)
	}
	return typ, body
}

// waitReady consumes messages until ReadyForQuery, failing on ErrorResponse.
func (p *pgConn) waitReady() {
	p.t.Helper()
	for {
		typ, body := p.recv()
		switch typ {
		case 'Z':
			return
		case 'E':
			p.t.Fatalf("server error: %s", pgErrText(body))
		}
	}
}

// pgErrText pulls the human message out of an ErrorResponse body.
func pgErrText(body []byte) string {
	for len(body) > 0 && body[0] != 0 {
		code := body[0]
		body = body[1:]
		end := 0
		for end < len(body) && body[end] != 0 {
			end++
		}
		if code == 'M' {
			return string(body[:end])
		}
		body = body[end+1:]
	}
	return "<unparsed>"
}

// query runs one simple-protocol statement and returns the text rows.
func (p *pgConn) query(sql string) [][]string {
	p.t.Helper()
	p.send('Q', append([]byte(sql), 0))
	var rows [][]string
	for {
		typ, body := p.recv()
		switch typ {
		case 'T': // RowDescription — column shapes, not needed for asserts
		case 'D': // DataRow
			n := int(binary.BigEndian.Uint16(body))
			body = body[2:]
			row := make([]string, 0, n)
			for i := 0; i < n; i++ {
				ln := int(int32(binary.BigEndian.Uint32(body)))
				body = body[4:]
				if ln < 0 {
					row = append(row, "<null>")
					continue
				}
				row = append(row, string(body[:ln]))
				body = body[ln:]
			}
			rows = append(rows, row)
		case 'C': // CommandComplete
		case 'E':
			p.t.Fatalf("query %q: %s", sql, pgErrText(body))
		case 'Z':
			return rows
		}
	}
}

// execExtended runs one parameterized statement through the extended
// protocol: Parse → Bind (text params) → Execute → Sync.
func (p *pgConn) execExtended(sql string, params ...string) {
	p.t.Helper()
	// Parse (unnamed statement, no declared OIDs).
	var parse []byte
	parse = append(parse, 0) // unnamed
	parse = append(parse, sql...)
	parse = append(parse, 0)
	parse = binary.BigEndian.AppendUint16(parse, 0)
	p.send('P', parse)
	// Bind (unnamed portal ← unnamed statement, all-text params/results).
	var bind []byte
	bind = append(bind, 0, 0)                     // portal, statement
	bind = binary.BigEndian.AppendUint16(bind, 0) // param format codes: default text
	bind = binary.BigEndian.AppendUint16(bind, uint16(len(params)))
	for _, v := range params {
		bind = binary.BigEndian.AppendUint32(bind, uint32(len(v)))
		bind = append(bind, v...)
	}
	bind = binary.BigEndian.AppendUint16(bind, 0) // result formats: default
	p.send('B', bind)
	// Execute (unnamed portal, no row cap) + Sync.
	p.send('E', append([]byte{0}, 0, 0, 0, 0))
	p.send('S', nil)
	// Expect ParseComplete, BindComplete, CommandComplete, ReadyForQuery.
	for _, want := range []byte{'1', '2', 'C'} {
		typ, body := p.recv()
		if typ == 'E' {
			p.t.Fatalf("extended %q: %s", sql, pgErrText(body))
		}
		if typ != want {
			p.t.Fatalf("extended %q: expected %c, got %c", sql, want, typ)
		}
	}
	p.waitReady()
}

// TestSQLGatewayRoundTrip — GUARANTEE: the §13 SQL layer speaks pg wire v3
// (startup, cleartext auth against databox users, simple AND extended
// protocol) and executes DDL/DML/queries against the cluster: CREATE
// TABLE, parameterized INSERT via $N binds, SELECT returning the rows.
func TestSQLGatewayRoundTrip(t *testing.T) {
	nodes := startCluster(t, 1)
	addr := startSQLGateway(t, nodes[0].endpoint())

	pg := dialPG(t, addr, "root", "", "e2edb")

	if rows := pg.query("CREATE TABLE people (id INTEGER PRIMARY KEY, name TEXT)"); len(rows) != 0 {
		t.Fatalf("CREATE TABLE returned rows: %v", rows)
	}
	// Parameterized inserts through the extended protocol ($1, $2 binds).
	pg.execExtended("INSERT INTO people (id, name) VALUES ($1, $2)", "1", "ada")
	pg.execExtended("INSERT INTO people (id, name) VALUES ($1, $2)", "2", "grace")

	rows := pg.query("SELECT id, name FROM people ORDER BY id")
	if len(rows) != 2 {
		t.Fatalf("SELECT returned %d rows, want 2: %v", len(rows), rows)
	}
	if rows[0][0] != "1" || rows[0][1] != "ada" || rows[1][0] != "2" || rows[1][1] != "grace" {
		t.Fatalf("SELECT rows wrong: %v", rows)
	}
	// A WHERE over the same data, still through the real cluster.
	rows = pg.query("SELECT name FROM people WHERE id = 2")
	if len(rows) != 1 || rows[0][0] != "grace" {
		t.Fatalf("filtered SELECT wrong: %v", rows)
	}
}
