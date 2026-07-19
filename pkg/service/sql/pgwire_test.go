// pgwire_test.go exercises the extended protocol at the message level: a
// test frontend drives Parse/Bind/Describe/Execute/Sync over an in-memory
// pipe against a conn whose engine runs on the in-memory store — the same
// message flow pgx and psycopg produce for parameterized CRUD.
package sql

import (
	"bufio"
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"net"
	"strings"
	"testing"
	"time"
)

// testFrontend is a minimal pg client for tests: it frames frontend
// messages and decodes backend ones.
type testFrontend struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

// startConn wires a conn (with a fresh engine) to a pipe and runs its loop.
func startConn(t *testing.T) *testFrontend {
	t.Helper()
	client, server := net.Pipe()
	c := newConn(bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server)), server)
	c.eng = NewEngineWithStore(NewMemStore(), "test")
	go func() {
		c.loop()
		server.Close()
	}()
	t.Cleanup(func() { client.Close() })
	client.SetDeadline(time.Now().Add(10 * time.Second))
	return &testFrontend{t: t, c: client, r: bufio.NewReader(client)}
}

// send frames one frontend message.
func (f *testFrontend) send(typ byte, body []byte) {
	f.t.Helper()
	hdr := make([]byte, 5)
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(body)+4))
	if _, err := f.c.Write(append(hdr, body...)); err != nil {
		f.t.Fatalf("send %c: %v", typ, err)
	}
}

// recv reads one backend message.
func (f *testFrontend) recv() (byte, []byte) {
	f.t.Helper()
	typ, body, err := readMessage(f.r)
	if err != nil {
		f.t.Fatalf("recv: %v", err)
	}
	return typ, body
}

// expect asserts the next backend message type and returns its body.
func (f *testFrontend) expect(want byte) []byte {
	f.t.Helper()
	typ, body := f.recv()
	if typ != want {
		if typ == 'E' {
			f.t.Fatalf("expected %c, got ErrorResponse: %s", want, errText(body))
		}
		f.t.Fatalf("expected message %c, got %c", want, typ)
	}
	return body
}

// errText extracts the human message from an ErrorResponse body.
func errText(body []byte) string {
	r := &msgReader{b: body}
	for {
		code := r.byte()
		if code == 0 || r.err != nil {
			return "<unparsed>"
		}
		s := r.cstr()
		if code == 'M' {
			return s
		}
	}
}

// --- frontend message builders -------------------------------------------

func buildParse(name, query string, oids []int32) []byte {
	var b []byte
	b = append(b, name...)
	b = append(b, 0)
	b = append(b, query...)
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, uint16(len(oids)))
	for _, o := range oids {
		b = binary.BigEndian.AppendUint32(b, uint32(o))
	}
	return b
}

// bindParam is one Bind parameter: raw bytes (nil = NULL) plus its format.
type bindParam struct {
	data   []byte
	binary bool
}

func textParam(s string) bindParam { return bindParam{data: []byte(s)} }

func buildBind(portal, stmt string, params []bindParam, resultFormats []int16) []byte {
	var b []byte
	b = append(b, portal...)
	b = append(b, 0)
	b = append(b, stmt...)
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, uint16(len(params)))
	for _, p := range params {
		f := uint16(0)
		if p.binary {
			f = 1
		}
		b = binary.BigEndian.AppendUint16(b, f)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(params)))
	for _, p := range params {
		if p.data == nil {
			b = binary.BigEndian.AppendUint32(b, 0xFFFFFFFF) // -1: NULL
			continue
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(p.data)))
		b = append(b, p.data...)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(resultFormats)))
	for _, f := range resultFormats {
		b = binary.BigEndian.AppendUint16(b, uint16(f))
	}
	return b
}

func buildDescribe(kind byte, name string) []byte {
	return append(append([]byte{kind}, name...), 0)
}

func buildExecute(portal string) []byte {
	b := append([]byte(portal), 0)
	return binary.BigEndian.AppendUint32(b, 0) // no row limit
}

// prepareBindExecute runs the standard cycle for one statement and returns
// the DataRows (as raw bodies) and the CommandComplete tag.
func (f *testFrontend) prepareBindExecute(query string, params []bindParam) ([][]byte, string) {
	f.t.Helper()
	f.send('P', buildParse("", query, nil))
	f.send('B', buildBind("", "", params, nil))
	f.send('E', buildExecute(""))
	f.send('S', nil)
	f.expect('1') // ParseComplete
	f.expect('2') // BindComplete
	var rows [][]byte
	var tag string
	for {
		typ, body := f.recv()
		switch typ {
		case 'D':
			rows = append(rows, body)
		case 'C':
			tag = string(trimNul(body))
		case 'Z':
			return rows, tag
		case 'E':
			f.t.Fatalf("ErrorResponse: %s", errText(body))
		default:
			f.t.Fatalf("unexpected message %c", typ)
		}
	}
}

// dataRowFields splits a DataRow body into per-column values (nil = NULL).
func dataRowFields(t *testing.T, body []byte) [][]byte {
	t.Helper()
	r := &msgReader{b: body}
	n := int(r.int16())
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		ln := r.int32()
		if ln < 0 {
			out = append(out, nil)
			continue
		}
		out = append(out, r.bytes(int(ln)))
	}
	if r.err != nil {
		t.Fatalf("bad DataRow: %v", r.err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestExtendedParameterizedCRUD(t *testing.T) {
	f := startConn(t)

	_, tag := f.prepareBindExecute("CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, score DOUBLE PRECISION)", nil)
	if tag != "CREATE TABLE" {
		t.Fatalf("tag = %q", tag)
	}

	// Parameterized INSERT with text-format parameters.
	_, tag = f.prepareBindExecute("INSERT INTO t VALUES ($1, $2, $3)",
		[]bindParam{textParam("1"), textParam("alice"), textParam("9.5")})
	if tag != "INSERT 0 1" {
		t.Fatalf("tag = %q", tag)
	}
	_, tag = f.prepareBindExecute("INSERT INTO t VALUES ($1, $2, $3)",
		[]bindParam{textParam("2"), textParam("bob"), textParam("7.25")})
	if tag != "INSERT 0 1" {
		t.Fatalf("tag = %q", tag)
	}

	// Parameterized SELECT.
	rows, tag := f.prepareBindExecute("SELECT id, name FROM t WHERE id = $1", []bindParam{textParam("2")})
	if tag != "SELECT 1" || len(rows) != 1 {
		t.Fatalf("tag=%q rows=%d", tag, len(rows))
	}
	fields := dataRowFields(t, rows[0])
	if string(fields[0]) != "2" || string(fields[1]) != "bob" {
		t.Fatalf("row = %q %q", fields[0], fields[1])
	}

	// Parameterized UPDATE and DELETE with correct tags.
	_, tag = f.prepareBindExecute("UPDATE t SET score = $1 WHERE name = $2",
		[]bindParam{textParam("10"), textParam("alice")})
	if tag != "UPDATE 1" {
		t.Fatalf("tag = %q", tag)
	}
	_, tag = f.prepareBindExecute("DELETE FROM t WHERE id = $1", []bindParam{textParam("1")})
	if tag != "DELETE 1" {
		t.Fatalf("tag = %q", tag)
	}

	// NULL parameter.
	_, tag = f.prepareBindExecute("INSERT INTO t VALUES ($1, $2, $3)",
		[]bindParam{textParam("3"), {data: nil}, textParam("1")})
	if tag != "INSERT 0 1" {
		t.Fatalf("tag = %q", tag)
	}
	rows, _ = f.prepareBindExecute("SELECT name FROM t WHERE id = $1", []bindParam{textParam("3")})
	if fields := dataRowFields(t, rows[0]); fields[0] != nil {
		t.Fatalf("expected NULL name, got %q", fields[0])
	}
}

func TestExtendedBinaryParams(t *testing.T) {
	f := startConn(t)
	f.prepareBindExecute("CREATE TABLE b (id INTEGER PRIMARY KEY, x DOUBLE PRECISION, ok BOOLEAN)", nil)

	// Declare OIDs so binary parameters decode: int8, float8, bool.
	f.send('P', buildParse("ins", "INSERT INTO b VALUES ($1, $2, $3)", []int32{oidInt8, oidFloat8, oidBool}))
	var id [8]byte
	binary.BigEndian.PutUint64(id[:], 42)
	var x [8]byte
	binary.BigEndian.PutUint64(x[:], math.Float64bits(2.5))
	f.send('B', buildBind("", "ins", []bindParam{
		{data: id[:], binary: true},
		{data: x[:], binary: true},
		{data: []byte{1}, binary: true},
	}, nil))
	f.send('E', buildExecute(""))
	f.send('S', nil)
	f.expect('1')
	f.expect('2')
	body := f.expect('C')
	if got := string(trimNul(body)); got != "INSERT 0 1" {
		t.Fatalf("tag = %q", got)
	}
	f.expect('Z')

	rows, _ := f.prepareBindExecute("SELECT id, x, ok FROM b", nil)
	fields := dataRowFields(t, rows[0])
	if string(fields[0]) != "42" || string(fields[1]) != "2.5" || string(fields[2]) != "t" {
		t.Fatalf("row = %q %q %q", fields[0], fields[1], fields[2])
	}
}

func TestExtendedDescribeAndNamedStatements(t *testing.T) {
	f := startConn(t)
	f.prepareBindExecute("CREATE TABLE d (id INTEGER PRIMARY KEY, name TEXT)", nil)
	f.prepareBindExecute("INSERT INTO d VALUES (1, 'x'), (2, 'y')", nil)

	// Two named statements coexist.
	f.send('P', buildParse("sel", "SELECT id, name FROM d WHERE id = $1", nil))
	f.send('P', buildParse("cnt", "SELECT count(*) FROM d", nil))
	f.send('D', buildDescribe('S', "sel"))
	f.send('S', nil)
	f.expect('1')
	f.expect('1')

	// ParameterDescription: one parameter, defaulted to text.
	pd := f.expect('t')
	r := &msgReader{b: pd}
	if n := r.int16(); n != 1 {
		t.Fatalf("param count = %d", n)
	}
	if oid := r.int32(); oid != oidText {
		t.Fatalf("param oid = %d", oid)
	}

	// RowDescription: id is int8, name is text.
	rd := f.expect('T')
	r = &msgReader{b: rd}
	if n := r.int16(); n != 2 {
		t.Fatalf("column count = %d", n)
	}
	checkCol := func(wantName string, wantOID int32) {
		t.Helper()
		name := r.cstr()
		r.int32() // table oid
		r.int16() // attnum
		oid := r.int32()
		r.int16() // size
		r.int32() // typmod
		r.int16() // format
		if name != wantName || oid != wantOID {
			t.Fatalf("column %q oid %d, want %q oid %d", name, oid, wantName, wantOID)
		}
	}
	checkCol("id", oidInt8)
	checkCol("name", oidText)
	f.expect('Z')

	// Bind two portals over the named statements and execute both.
	f.send('B', buildBind("p1", "sel", []bindParam{textParam("2")}, nil))
	f.send('B', buildBind("p2", "cnt", nil, nil))
	f.send('E', buildExecute("p1"))
	f.send('E', buildExecute("p2"))
	f.send('S', nil)
	f.expect('2')
	f.expect('2')
	row := f.expect('D')
	if fields := dataRowFields(t, row); string(fields[0]) != "2" || string(fields[1]) != "y" {
		t.Fatalf("p1 row = %q %q", fields[0], fields[1])
	}
	f.expect('C')
	row = f.expect('D')
	if fields := dataRowFields(t, row); string(fields[0]) != "2" {
		t.Fatalf("count = %q", fields[0])
	}
	f.expect('C')
	f.expect('Z')

	// Close the statement; binding it afterwards is an error.
	f.send('C', buildDescribe('S', "sel"))
	f.send('S', nil)
	f.expect('3')
	f.expect('Z')
	f.send('B', buildBind("", "sel", []bindParam{textParam("1")}, nil))
	f.send('S', nil)
	if typ, body := f.recv(); typ != 'E' {
		t.Fatalf("expected ErrorResponse, got %c (%s)", typ, body)
	}
	f.expect('Z')
}

func TestExtendedBinaryResults(t *testing.T) {
	f := startConn(t)
	f.prepareBindExecute("CREATE TABLE r (id INTEGER PRIMARY KEY)", nil)
	f.prepareBindExecute("INSERT INTO r VALUES (7)", nil)

	// Request binary result format for the single int8 column.
	f.send('P', buildParse("", "SELECT id FROM r", nil))
	f.send('B', buildBind("", "", nil, []int16{1}))
	f.send('E', buildExecute(""))
	f.send('S', nil)
	f.expect('1')
	f.expect('2')
	row := f.expect('D')
	fields := dataRowFields(t, row)
	if len(fields[0]) != 8 || binary.BigEndian.Uint64(fields[0]) != 7 {
		t.Fatalf("binary int8 = %v", fields[0])
	}
	f.expect('C')
	f.expect('Z')
}

func TestExtendedErrorRecovery(t *testing.T) {
	f := startConn(t)

	// A failed Parse must not poison the connection past the next Sync.
	f.send('P', buildParse("", "SELEC nonsense", nil))
	f.send('B', buildBind("", "", nil, nil)) // skipped: failed state
	f.send('S', nil)
	if typ, _ := f.recv(); typ != 'E' {
		t.Fatalf("expected ErrorResponse, got %c", typ)
	}
	f.expect('Z')

	// The connection still works.
	_, tag := f.prepareBindExecute("SELECT 1", nil)
	if tag != "SELECT 1" {
		t.Fatalf("tag = %q", tag)
	}

	// Binding with the wrong parameter count is an error.
	f.send('P', buildParse("", "SELECT $1", nil))
	f.send('B', buildBind("", "", nil, nil))
	f.send('S', nil)
	f.expect('1')
	if typ, _ := f.recv(); typ != 'E' {
		t.Fatalf("expected ErrorResponse for missing params")
	}
	f.expect('Z')
}

func TestCleartextRefusedWithoutOptIn(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	l := &layer{opts: Options{}, log: discardLogger()}
	done := make(chan struct{})
	go func() {
		l.serveConn(server)
		close(done)
	}()
	client.SetDeadline(time.Now().Add(5 * time.Second))

	// Plain startup, no SSLRequest: must be refused before authentication.
	body := []byte("user\x00u\x00database\x00d\x00\x00")
	msg := make([]byte, 8)
	binary.BigEndian.PutUint32(msg[0:4], uint32(len(body)+8))
	binary.BigEndian.PutUint32(msg[4:8], protocolVersion)
	if _, err := client.Write(append(msg, body...)); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(client)
	typ, errBody, err := readMessage(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != 'E' {
		t.Fatalf("expected ErrorResponse, got %c", typ)
	}
	if msg := errText(errBody); !strings.Contains(msg, "TLS") {
		t.Fatalf("error should mention TLS, got %q", msg)
	}
	<-done
}

// discardLogger silences the layer's log output in tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
