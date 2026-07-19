// pgwire.go implements the PostgreSQL wire protocol v3 front-end for the SQL
// layer (§13): any psql or libpq/pg driver connects, TLS is
// offered, and password authentication is verified against databox users by
// logging in to the cluster (§7.1). The dialect carried over the wire is
// chai's, not full pg — the same "pg transport, own dialect" model QuestDB
// and immudb use.
//
// Both the simple query protocol ('Q') and the extended protocol are
// supported: Parse/Bind/Describe/Execute/Close/Sync with real $N parameters
// (text format for every type, binary for the fixed-width ones), multiple
// named prepared statements and portals, and typed RowDescriptions — enough
// for pgx and psycopg parameterized CRUD.
package sql

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// Protocol constants from the PostgreSQL frontend/backend protocol.
const (
	sslRequestCode  = 80877103 // magic in place of a protocol version
	protocolVersion = 196608   // 3.0
)

// preparedStmt is one Parse result: the SQL, its parsed form (kept for
// Describe before Bind), and its parameter shape.
type preparedStmt struct {
	sql       string
	stmts     []Statement
	nParams   int
	paramOIDs []int32 // client-declared OIDs, 0 = unspecified (text)
}

// portal is one Bind result: statements with parameters substituted, the
// predicted result shape, and the requested result formats.
type portal struct {
	stmts         []Statement
	desc          stmtDescription
	resultFormats []int16
}

// conn wraps one client connection with buffered IO and its executor.
type conn struct {
	rw       *bufio.ReadWriter
	raw      net.Conn
	eng      *Engine
	prepared map[string]*preparedStmt
	portals  map[string]*portal
	// failed marks an extended-protocol error: per the protocol, further
	// extended messages are skipped until the client sends Sync.
	failed bool
}

// serveConn runs the whole lifecycle for one connection: TLS negotiation,
// startup, authentication, then the query loop.
func (l *layer) serveConn(raw net.Conn) {
	defer raw.Close()
	br := bufio.NewReader(raw)
	bw := bufio.NewWriter(raw)

	// The first message may be an SSLRequest (no type byte, just length +
	// magic). Peek the length+code to decide.
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return
	}
	secured := false
	code := int(binary.BigEndian.Uint32(hdr[4:8]))
	if code == sslRequestCode {
		if l.tlsCfg != nil {
			// Accept TLS: reply 'S', then upgrade the socket.
			if _, err := raw.Write([]byte{'S'}); err != nil {
				return
			}
			tconn := tls.Server(raw, l.tlsCfg)
			if err := tconn.Handshake(); err != nil {
				return
			}
			raw = tconn
			br = bufio.NewReader(raw)
			bw = bufio.NewWriter(raw)
			secured = true
		} else {
			// No TLS available: decline; cleartext continues only if allowed.
			if _, err := raw.Write([]byte{'N'}); err != nil {
				return
			}
		}
		// Read the real startup header now.
		if _, err := io.ReadFull(br, hdr); err != nil {
			return
		}
	}

	// Passwords travel in this stream, so cleartext is refused unless the
	// operator explicitly opted in (Options.AllowCleartext).
	if !secured && !l.opts.AllowCleartext {
		writeError(bw, "server requires TLS (cleartext connections are disabled)")
		bw.Flush()
		return
	}

	// hdr now holds the startup message length+version. Read the remainder.
	length := int(binary.BigEndian.Uint32(hdr[0:4]))
	if length < 8 || length > 1<<20 {
		return
	}
	body := make([]byte, length-8)
	if _, err := io.ReadFull(br, body); err != nil {
		return
	}
	if int(binary.BigEndian.Uint32(hdr[4:8])) != protocolVersion {
		writeError(bw, "unsupported protocol version")
		bw.Flush()
		return
	}
	params := parseStartup(body)
	c := newConn(bufio.NewReadWriter(br, bw), raw)

	// Authenticate: request a cleartext password (safe because the transport
	// is TLS unless cleartext was explicitly allowed), then log in to the
	// cluster.
	cl, err := l.authenticate(c, params["user"], params["database"])
	if err != nil {
		writeError(bw, "authentication failed: "+err.Error())
		bw.Flush()
		return
	}
	db := params["database"]
	if db == "" {
		db = params["user"]
	}
	c.eng = NewEngine(cl, db)

	// Ready: advertise a couple of parameters and enter the query loop.
	writeAuthOk(bw)
	writeParam(bw, "server_version", "15.0 (databox)")
	writeParam(bw, "client_encoding", "UTF8")
	writeBackendKeyData(bw)
	writeReadyForQuery(bw)
	bw.Flush()

	c.loop()
}

// newConn builds the per-connection state (the engine is attached after
// authentication; tests attach one directly).
func newConn(rw *bufio.ReadWriter, raw net.Conn) *conn {
	return &conn{
		rw:       rw,
		raw:      raw,
		prepared: map[string]*preparedStmt{},
		portals:  map[string]*portal{},
	}
}

// authenticate performs the cleartext-password exchange and returns a logged
// -in cluster client on success.
func (l *layer) authenticate(c *conn, user, database string) (*client.Client, error) {
	// AuthenticationCleartextPassword.
	writeAuthCleartext(c.rw.Writer)
	c.rw.Writer.Flush()
	typ, msg, err := readMessage(c.rw.Reader)
	if err != nil {
		return nil, err
	}
	if typ != 'p' {
		return nil, fmt.Errorf("expected password message, got %q", typ)
	}
	password := string(trimNul(msg))
	cl, err := l.newClient()
	if err != nil {
		return nil, err
	}
	if err := cl.Login(ctxBackground(), user, password); err != nil {
		return nil, err
	}
	return cl, nil
}

// loop reads and dispatches protocol messages until the client disconnects.
func (c *conn) loop() {
	for {
		typ, msg, err := readMessage(c.rw.Reader)
		if err != nil {
			return
		}
		// After an extended-protocol error everything but Sync (and
		// Terminate) is discarded, per the protocol.
		if c.failed && typ != 'S' && typ != 'X' && typ != 'Q' {
			continue
		}
		switch typ {
		case 'Q': // simple query
			c.failed = false
			c.simpleQuery(string(trimNul(msg)))
			c.rw.Writer.Flush()
		// Extended-protocol responses stay buffered until Sync or Flush,
		// as the protocol prescribes — clients pipeline P/B/D/E without
		// reading, so flushing early can deadlock on unbuffered links.
		case 'P': // Parse
			c.handleParse(msg)
		case 'B': // Bind
			c.handleBind(msg)
		case 'D': // Describe
			c.handleDescribe(msg)
		case 'E': // Execute
			c.handleExecute(msg)
		case 'C': // Close (a statement or portal, not the connection)
			c.handleClose(msg)
		case 'S': // Sync
			c.failed = false
			writeReadyForQuery(c.rw.Writer)
			c.rw.Writer.Flush()
		case 'H': // Flush
			c.rw.Writer.Flush()
		case 'X': // Terminate
			return
		default:
			// Unknown message: keep the connection alive but ignore it.
		}
	}
}

// extError reports an extended-protocol failure and arms the skip-until-
// Sync state.
func (c *conn) extError(format string, args ...any) {
	writeError(c.rw.Writer, fmt.Sprintf(format, args...))
	c.failed = true
}

// ---------------------------------------------------------------------------
// Extended protocol handlers
// ---------------------------------------------------------------------------

// handleParse creates (or replaces) a named prepared statement.
func (c *conn) handleParse(msg []byte) {
	r := &msgReader{b: msg}
	name := r.cstr()
	query := r.cstr()
	nOIDs := int(r.int16())
	oids := make([]int32, 0, nOIDs)
	for i := 0; i < nOIDs; i++ {
		oids = append(oids, r.int32())
	}
	if r.err != nil {
		c.extError("malformed Parse message")
		return
	}
	stmts, err := ParseStatements(query)
	if err != nil {
		c.extError("%v", err)
		return
	}
	if len(stmts) > 1 {
		c.extError("cannot prepare multiple statements")
		return
	}
	n, err := countParams(stmts)
	if err != nil {
		c.extError("%v", err)
		return
	}
	// Pad the declared OIDs out to the number of $N references; 0 means
	// "unspecified", which binds as text.
	for len(oids) < n {
		oids = append(oids, 0)
	}
	c.prepared[name] = &preparedStmt{sql: query, stmts: stmts, nParams: n, paramOIDs: oids}
	writeByteMsg(c.rw.Writer, '1') // ParseComplete
}

// handleBind materializes a portal: decode parameters, substitute them
// into a fresh parse of the statement, and predict the result shape.
func (c *conn) handleBind(msg []byte) {
	r := &msgReader{b: msg}
	portalName := r.cstr()
	stmtName := r.cstr()
	nFmt := int(r.int16())
	fmts := make([]int16, 0, nFmt)
	for i := 0; i < nFmt; i++ {
		fmts = append(fmts, r.int16())
	}
	nVals := int(r.int16())
	vals := make([][]byte, 0, nVals)
	for i := 0; i < nVals; i++ {
		ln := r.int32()
		if ln < 0 {
			vals = append(vals, nil) // NULL
			continue
		}
		vals = append(vals, r.bytes(int(ln)))
	}
	nResFmt := int(r.int16())
	resFmts := make([]int16, 0, nResFmt)
	for i := 0; i < nResFmt; i++ {
		resFmts = append(resFmts, r.int16())
	}
	if r.err != nil {
		c.extError("malformed Bind message")
		return
	}
	ps, ok := c.prepared[stmtName]
	if !ok {
		c.extError("prepared statement %q does not exist", stmtName)
		return
	}
	if nVals != ps.nParams {
		c.extError("bind supplies %d parameters, but statement requires %d", nVals, ps.nParams)
		return
	}
	// Decode parameters into engine values.
	bound := make([]Value, nVals)
	for i, raw := range vals {
		fmtCode := int16(0)
		if len(fmts) == 1 {
			fmtCode = fmts[0]
		} else if i < len(fmts) {
			fmtCode = fmts[i]
		}
		oid := int32(0)
		if i < len(ps.paramOIDs) {
			oid = ps.paramOIDs[i]
		}
		v, err := decodeParam(raw, oid, fmtCode == 1)
		if err != nil {
			c.extError("parameter $%d: %v", i+1, err)
			return
		}
		bound[i] = v
	}
	// Substitute into a fresh parse — bindParams mutates the AST, and the
	// prepared statement must stay reusable for the next Bind.
	stmts, err := ParseStatements(ps.sql)
	if err != nil {
		c.extError("%v", err)
		return
	}
	if err := bindParams(stmts, bound); err != nil {
		c.extError("%v", err)
		return
	}
	p := &portal{stmts: stmts, resultFormats: resFmts}
	if len(stmts) == 1 {
		desc, err := c.eng.describeStatement(ctxBackground(), stmts[0])
		if err != nil {
			c.extError("%v", err)
			return
		}
		p.desc = desc
	}
	c.portals[portalName] = p
	writeByteMsg(c.rw.Writer, '2') // BindComplete
}

// handleDescribe answers with the shape of a statement ('S') or portal
// ('P'): ParameterDescription (statements only) plus RowDescription or
// NoData.
func (c *conn) handleDescribe(msg []byte) {
	r := &msgReader{b: msg}
	kind := r.byte()
	name := r.cstr()
	if r.err != nil {
		c.extError("malformed Describe message")
		return
	}
	switch kind {
	case 'S':
		ps, ok := c.prepared[name]
		if !ok {
			c.extError("prepared statement %q does not exist", name)
			return
		}
		writeParameterDescription(c.rw.Writer, ps.paramOIDs)
		if len(ps.stmts) != 1 {
			writeByteMsg(c.rw.Writer, 'n') // NoData
			return
		}
		desc, err := c.eng.describeStatement(ctxBackground(), ps.stmts[0])
		if err != nil {
			c.extError("%v", err)
			return
		}
		writeDescription(c.rw.Writer, desc, nil)
	case 'P':
		p, ok := c.portals[name]
		if !ok {
			c.extError("portal %q does not exist", name)
			return
		}
		writeDescription(c.rw.Writer, p.desc, p.resultFormats)
	default:
		c.extError("bad Describe kind %q", kind)
	}
}

// writeDescription emits RowDescription or NoData for a statement shape.
func writeDescription(w *bufio.Writer, desc stmtDescription, formats []int16) {
	if !desc.rowReturning {
		writeByteMsg(w, 'n') // NoData
		return
	}
	writeRowDescription(w, desc.cols, desc.oids, formats)
}

// handleExecute runs a portal and streams its rows. RowDescription is NOT
// sent here — the extended protocol reserves that for Describe.
func (c *conn) handleExecute(msg []byte) {
	r := &msgReader{b: msg}
	name := r.cstr()
	_ = r.int32() // max rows: portal suspension is not supported; send all
	p, ok := c.portals[name]
	if !ok {
		c.extError("portal %q does not exist", name)
		return
	}
	if len(p.stmts) == 0 {
		writeByteMsg(c.rw.Writer, 'I') // EmptyQueryResponse
		return
	}
	res, err := c.eng.execOne(ctxBackground(), p.stmts[0])
	if err != nil {
		c.extError("%v", err)
		return
	}
	if res.Columns != nil {
		for i, textRow := range res.Rows {
			var typed []Value
			if i < len(res.typed) {
				typed = res.typed[i]
			}
			if err := writeDataRowFormatted(c.rw.Writer, textRow, typed, p.resultFormats); err != nil {
				c.extError("%v", err)
				return
			}
		}
	}
	writeCommandComplete(c.rw.Writer, res.Tag)
}

// handleClose drops a named statement or portal.
func (c *conn) handleClose(msg []byte) {
	r := &msgReader{b: msg}
	kind := r.byte()
	name := r.cstr()
	switch kind {
	case 'S':
		delete(c.prepared, name)
	case 'P':
		delete(c.portals, name)
	}
	writeByteMsg(c.rw.Writer, '3') // CloseComplete
}

// simpleQuery executes a query string and writes results. It emits one
// RowDescription/DataRow set per row-returning statement, a CommandComplete
// per statement, and finally ReadyForQuery (as the simple protocol requires).
func (c *conn) simpleQuery(sql string) {
	w := c.rw.Writer
	if strings.TrimSpace(sql) == "" {
		writeByteMsg(w, 'I') // EmptyQueryResponse
		writeReadyForQuery(w)
		return
	}
	results, err := c.eng.Exec(ctxBackground(), sql)
	if err != nil {
		writeError(w, err.Error())
		writeReadyForQuery(w)
		return
	}
	for _, res := range results {
		if res.Columns != nil {
			// The simple protocol has no Describe: infer OIDs from the
			// first row's runtime types (text is always a safe fallback).
			writeRowDescription(w, res.Columns, runtimeOIDs(res), nil)
			for _, r := range res.Rows {
				writeDataRow(w, r)
			}
		}
		writeCommandComplete(w, res.Tag)
	}
	writeReadyForQuery(w)
}

// runtimeOIDs derives column OIDs from the first result row's values;
// columns with no rows (or NULL) stay text.
func runtimeOIDs(res ExecResult) []int32 {
	oids := make([]int32, len(res.Columns))
	for i := range oids {
		oids[i] = oidText
	}
	if len(res.typed) > 0 {
		for i, v := range res.typed[0] {
			if i < len(oids) && !v.IsNull() {
				oids[i] = oidForType(v.T)
			}
		}
	}
	return oids
}

// ---------------------------------------------------------------------------
// Frontend message parsing helpers
// ---------------------------------------------------------------------------

// msgReader is a cursor over one frontend message body; any overrun sets
// err and subsequent reads return zero values.
type msgReader struct {
	b   []byte
	i   int
	err error
}

func (r *msgReader) fail() {
	if r.err == nil {
		r.err = fmt.Errorf("truncated message")
	}
}

func (r *msgReader) byte() byte {
	if r.i >= len(r.b) {
		r.fail()
		return 0
	}
	c := r.b[r.i]
	r.i++
	return c
}

func (r *msgReader) cstr() string {
	for j := r.i; j < len(r.b); j++ {
		if r.b[j] == 0 {
			s := string(r.b[r.i:j])
			r.i = j + 1
			return s
		}
	}
	r.fail()
	return ""
}

func (r *msgReader) int16() int16 {
	if r.i+2 > len(r.b) {
		r.fail()
		return 0
	}
	v := int16(binary.BigEndian.Uint16(r.b[r.i:]))
	r.i += 2
	return v
}

func (r *msgReader) int32() int32 {
	if r.i+4 > len(r.b) {
		r.fail()
		return 0
	}
	v := int32(binary.BigEndian.Uint32(r.b[r.i:]))
	r.i += 4
	return v
}

func (r *msgReader) bytes(n int) []byte {
	if n < 0 || r.i+n > len(r.b) {
		r.fail()
		return nil
	}
	v := r.b[r.i : r.i+n]
	r.i += n
	return v
}

// --- startup parsing ---------------------------------------------------------

// parseStartup splits the NUL-separated key/value pairs of a startup body.
func parseStartup(body []byte) map[string]string {
	out := map[string]string{}
	parts := strings.Split(string(body), "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] == "" {
			break
		}
		out[parts[i]] = parts[i+1]
	}
	return out
}

// trimNul drops a single trailing NUL, as most text messages carry one.
func trimNul(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == 0 {
		return b[:len(b)-1]
	}
	return b
}
