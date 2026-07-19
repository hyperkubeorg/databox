// keyenc.go is the order-preserving key encoding used for primary keys
// and index entries (§13 storage mapping). It turns one
// or more SQL values into a string that
//
//   - sorts byte-wise in the same order the values sort in SQL, so
//     ORDER BY on an indexed column can become a KV range scan, and
//   - is valid UTF-8 and free of '/', so it can safely ride inside
//     databox key paths, URL segments, and JSON.
//
// The encoding has two stages. Stage 1 produces binary bytes:
//
//	NULL       -> 0x01
//	BOOLEAN    -> 0x02 , then 0x00 (false) or 0x01 (true)
//	INTEGER    -> 0x03 , then 8 bytes big-endian of uint64(v) XOR
//	              0x8000_0000_0000_0000 ("offset binary": flipping the
//	              sign bit makes negative numbers sort before positive)
//	DOUBLE     -> 0x04 , then 8 bytes big-endian of the IEEE-754 bits,
//	              transformed: if the sign bit is set, ALL bits invert
//	              (negative floats reverse order); otherwise only the
//	              sign bit is set. This is the standard trick that makes
//	              float bytes sort numerically.
//	TEXT       -> 0x05 , then the UTF-8 bytes with every 0x00 escaped as
//	              0x00 0xFF, then a terminating 0x00. The terminator
//	              keeps "a" < "ab" and makes multi-column encodings
//	              unambiguous (an FDB-tuple-layer style escape).
//	BYTEA      -> 0x06 , same escaping/termination as TEXT.
//	TIMESTAMP  -> 0x07 , then 8 bytes big-endian offset-binary of the
//	              microseconds since the Unix epoch (int64, like
//	              PostgreSQL's timestamp resolution).
//
// Type tags ascend in the SQL cross-type sort order (NULL first). For a
// composite key the per-value encodings simply concatenate — the
// terminators guarantee prefix-freedom, so ordering still holds.
//
// Stage 2 hex-encodes the binary (lowercase). Hex doubles the size but
// keeps keys printable/UTF-8-safe, and because '0'-'9' < 'a'-'f' in
// ASCII, lowercase hex preserves the byte order of stage 1 exactly.
package sql

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
)

// Stage-1 type tags. Their numeric order IS the cross-type sort order.
const (
	tagNull      = 0x01
	tagBool      = 0x02
	tagInt       = 0x03
	tagDouble    = 0x04
	tagText      = 0x05
	tagBytea     = 0x06
	tagTimestamp = 0x07
)

// encodeKeyValue appends the stage-1 binary encoding of v to dst.
func encodeKeyValue(dst []byte, v Value) ([]byte, error) {
	switch v.T {
	case TypeNull:
		return append(dst, tagNull), nil
	case TypeBool:
		if v.B {
			return append(dst, tagBool, 0x01), nil
		}
		return append(dst, tagBool, 0x00), nil
	case TypeInt:
		dst = append(dst, tagInt)
		return appendOffsetInt64(dst, v.I), nil
	case TypeDouble:
		dst = append(dst, tagDouble)
		// Normalize the two representations that compare equal to values
		// with different bits: -0.0 becomes +0.0 and every NaN becomes the
		// one canonical quiet NaN. Without this, Compare (value.go) would
		// call two keys equal that encode differently, and the planner's
		// index ranges could skip matching rows.
		f := v.F
		if f == 0 {
			f = 0 // drops a negative sign: -0.0 == 0 is true
		}
		if math.IsNaN(f) {
			f = math.NaN() // canonical NaN, sorts above +Inf after transform
		}
		bits := math.Float64bits(f)
		if bits&(1<<63) != 0 {
			bits = ^bits // negative: reverse the order of the magnitude
		} else {
			bits |= 1 << 63 // positive: shift above all negatives
		}
		return binary.BigEndian.AppendUint64(dst, bits), nil
	case TypeText:
		dst = append(dst, tagText)
		return appendEscaped(dst, []byte(v.S)), nil
	case TypeBytea:
		dst = append(dst, tagBytea)
		return appendEscaped(dst, v.Raw), nil
	case TypeTimestamp:
		dst = append(dst, tagTimestamp)
		return appendOffsetInt64(dst, v.TS.UnixMicro()), nil
	}
	return nil, fmt.Errorf("cannot use %s value in a key", v.T)
}

// appendOffsetInt64 writes an int64 in "offset binary": XORing the sign
// bit maps math order onto unsigned byte order.
func appendOffsetInt64(dst []byte, v int64) []byte {
	return binary.BigEndian.AppendUint64(dst, uint64(v)^(1<<63))
}

// appendEscaped writes variable-length bytes: 0x00 becomes 0x00 0xFF and
// a bare 0x00 terminates. Because every type tag is < 0xFF, a shorter
// string always sorts before any longer string it prefixes.
func appendEscaped(dst, data []byte) []byte {
	for _, b := range data {
		if b == 0x00 {
			dst = append(dst, 0x00, 0xFF)
		} else {
			dst = append(dst, b)
		}
	}
	return append(dst, 0x00)
}

// encodeKey runs both stages for a composite value list, returning the
// final hex string used inside KV key paths.
func encodeKey(vals ...Value) (string, error) {
	var bin []byte
	for _, v := range vals {
		var err error
		bin, err = encodeKeyValue(bin, v)
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(bin), nil
}
