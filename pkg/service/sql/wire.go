// wire.go holds the low-level PostgreSQL v3 message codec used by pgwire.go.
// Every backend message is a one-byte type, a 4-byte big-endian length that
// includes those four bytes, then the body. These helpers keep pgwire.go
// readable by hiding the framing.
package sql

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// readMessage reads one typed protocol message, returning its type byte and
// body (without the length prefix).
func readMessage(r *bufio.Reader) (byte, []byte, error) {
	typ, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return 0, nil, err
	}
	length := int(binary.BigEndian.Uint32(lenBuf[:]))
	if length < 4 {
		return typ, nil, nil
	}
	body := make([]byte, length-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}

// msg accumulates a backend message body and flushes it with the correct
// framing when done.
type msg struct {
	typ  byte
	body []byte
}

func newMsg(typ byte) *msg { return &msg{typ: typ} }

func (m *msg) int32(v int32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(v))
	m.body = append(m.body, b[:]...)
}

func (m *msg) int16(v int16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], uint16(v))
	m.body = append(m.body, b[:]...)
}

// str appends a NUL-terminated string.
func (m *msg) str(s string) {
	m.body = append(m.body, []byte(s)...)
	m.body = append(m.body, 0)
}

// bytesField appends a length-prefixed value column (used by DataRow); a
// nil pointer encodes SQL NULL as length -1.
func (m *msg) bytesField(p *string) {
	if p == nil {
		m.int32(-1)
		return
	}
	m.int32(int32(len(*p)))
	m.body = append(m.body, []byte(*p)...)
}

// write frames and writes the message.
func (m *msg) write(w *bufio.Writer) {
	var hdr [5]byte
	hdr[0] = m.typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(m.body)+4))
	w.Write(hdr[:])
	w.Write(m.body)
}

// --- specific backend messages ----------------------------------------------

// writeByteMsg writes a body-less message (ParseComplete, NoData, etc.).
func writeByteMsg(w *bufio.Writer, typ byte) {
	newMsg(typ).write(w)
}

// writeAuthCleartext sends AuthenticationCleartextPassword (R, code 3).
func writeAuthCleartext(w *bufio.Writer) {
	m := newMsg('R')
	m.int32(3)
	m.write(w)
}

// writeAuthOk sends AuthenticationOk (R, code 0).
func writeAuthOk(w *bufio.Writer) {
	m := newMsg('R')
	m.int32(0)
	m.write(w)
}

// writeParam sends a ParameterStatus (S) key/value.
func writeParam(w *bufio.Writer, k, v string) {
	m := newMsg('S')
	m.str(k)
	m.str(v)
	m.write(w)
}

// writeBackendKeyData sends a BackendKeyData (K). The values are cancellation
// keys we do not act on (query cancellation is not supported in v1), so any
// stable pair is fine.
func writeBackendKeyData(w *bufio.Writer) {
	m := newMsg('K')
	m.int32(1) // process id
	m.int32(1) // secret key
	m.write(w)
}

// writeReadyForQuery sends ReadyForQuery (Z) with idle transaction status.
func writeReadyForQuery(w *bufio.Writer) {
	m := newMsg('Z')
	m.body = append(m.body, 'I')
	m.write(w)
}

// writeRowDescription sends a RowDescription (T). oids supplies the type
// OID per column (missing entries fall back to text); formats carries the
// portal's requested result format codes (nil/empty means all text).
func writeRowDescription(w *bufio.Writer, cols []string, oids []int32, formats []int16) {
	m := newMsg('T')
	m.int16(int16(len(cols)))
	for i, c := range cols {
		oid := int32(oidText)
		if i < len(oids) && oids[i] != 0 {
			oid = oids[i]
		}
		m.str(c)
		m.int32(0)   // table OID (unknown)
		m.int16(0)   // column attribute number
		m.int32(oid) // type OID
		m.int16(-1)  // type size: variable
		m.int32(-1)  // type modifier
		m.int16(formatFor(formats, i))
	}
	m.write(w)
}

// formatFor resolves the pg format-code rule: no codes = all text, one
// code = applies to every column, otherwise per column.
func formatFor(formats []int16, i int) int16 {
	switch {
	case len(formats) == 0:
		return 0
	case len(formats) == 1:
		return formats[0]
	case i < len(formats):
		return formats[i]
	}
	return 0
}

// writeDataRow sends a DataRow (D) in text format. Each field is a
// *string; nil is NULL.
func writeDataRow(w *bufio.Writer, vals []*string) {
	m := newMsg('D')
	m.int16(int16(len(vals)))
	for _, v := range vals {
		m.bytesField(v)
	}
	m.write(w)
}

// writeDataRowFormatted sends a DataRow honoring the portal's requested
// result formats: binary for the fixed-width types when asked, text
// otherwise. typed may be nil when only text is available (then any binary
// request fails rather than corrupting the stream).
func writeDataRowFormatted(w *bufio.Writer, texts []*string, typed []Value, formats []int16) error {
	m := newMsg('D')
	m.int16(int16(len(texts)))
	for i, tv := range texts {
		if formatFor(formats, i) == 0 {
			m.bytesField(tv)
			continue
		}
		if tv == nil {
			m.int32(-1) // NULL is format-independent
			continue
		}
		if typed == nil || i >= len(typed) {
			return fmt.Errorf("binary result format requested for column %d with no typed value", i)
		}
		raw, err := encodeBinaryValue(typed[i])
		if err != nil {
			return err
		}
		m.int32(int32(len(raw)))
		m.body = append(m.body, raw...)
	}
	m.write(w)
	return nil
}

// encodeBinaryValue renders a value in pg binary format for the types the
// layer advertises binary-capably: int8, float8, bool, bytea, text.
func encodeBinaryValue(v Value) ([]byte, error) {
	switch v.T {
	case TypeInt:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(v.I))
		return b[:], nil
	case TypeDouble:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], math.Float64bits(v.F))
		return b[:], nil
	case TypeBool:
		if v.B {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case TypeBytea:
		return v.Raw, nil
	case TypeText:
		return []byte(v.S), nil
	}
	return nil, fmt.Errorf("binary result format not supported for %s", v.T)
}

// writeParameterDescription sends a ParameterDescription (t) for Describe
// of a prepared statement; unspecified OIDs are reported as text.
func writeParameterDescription(w *bufio.Writer, oids []int32) {
	m := newMsg('t')
	m.int16(int16(len(oids)))
	for _, oid := range oids {
		if oid == 0 {
			oid = oidText
		}
		m.int32(oid)
	}
	m.write(w)
}

// writeCommandComplete sends a CommandComplete (C) with the command tag.
func writeCommandComplete(w *bufio.Writer, tag string) {
	m := newMsg('C')
	m.str(tag)
	m.write(w)
}

// writeError sends a minimal ErrorResponse (E): severity, SQLSTATE, message.
// SQLSTATE 42000 is "syntax error or access rule violation" — a reasonable
// generic class for dialect/execution errors.
func writeError(w *bufio.Writer, message string) {
	m := newMsg('E')
	m.body = append(m.body, 'S')
	m.body = append(m.body, []byte("ERROR")...)
	m.body = append(m.body, 0)
	m.body = append(m.body, 'C')
	m.body = append(m.body, []byte("42000")...)
	m.body = append(m.body, 0)
	m.body = append(m.body, 'M')
	m.body = append(m.body, []byte(message)...)
	m.body = append(m.body, 0)
	m.body = append(m.body, 0) // terminator
	m.write(w)
}
