// value.go defines the runtime value system of the SQL engine: the seven
// dialect types (NULL, BOOLEAN, INTEGER/BIGINT, DOUBLE PRECISION, TEXT,
// BYTEA, TIMESTAMP) plus the pgvector-style VECTOR(n) extension type
// (vector.go), how values compare, how arithmetic behaves, how a value
// converts between types, and how a row of values is stored as bytes
// inside the KV store.
//
// The semantics deliberately clone chai's dialect (REFERENCES/chai is the
// behavioral spec — see the sqltests/expr corpus):
//
//   - Comparing a number with a piece of text converts the text to a
//     number and errors if that is impossible ("cannot cast ...").
//   - Comparing a boolean with a number is an error ("cannot compare
//     integer with boolean").
//   - Any comparison or arithmetic with NULL yields NULL (three-valued
//     logic).
//   - Arithmetic between a number and a non-numeric value yields NULL
//     (1 + 'a' is NULL, not an error).
//   - Integer division truncates; dividing by zero is an error; integer
//     overflow is an error.
package sql

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Type enumerates the runtime types a Value can hold. The order of the
// constants also defines the cross-type sort order used by ORDER BY and
// DISTINCT when rows of different types meet (e.g. after a UNION):
// NULL < BOOLEAN < numbers < TEXT < BYTEA < TIMESTAMP.
type Type uint8

// The runtime types. INTEGER and BIGINT share one runtime representation
// (Go int64); the distinction only matters for typeof() and column
// declarations, mirroring chai.
const (
	TypeNull Type = iota
	TypeBool
	TypeInt
	TypeDouble
	TypeText
	TypeBytea
	TypeTimestamp
	// TypeAny marks a column declared without a type (chai allows
	// "CREATE TABLE t(a NOT NULL, ...)"); values stored in such a column
	// keep whatever runtime type they arrived with — no coercion.
	TypeAny
	// TypeVector is the pgvector-style VECTOR(n) extension type: a fixed-
	// dimension array of float32 (vector.go). It sits after TypeAny so the
	// numeric values of the pre-existing types — persisted in catalog
	// records — stay unchanged.
	TypeVector
)

// String names the type the way the dialect spells it (typeof() output).
func (t Type) String() string {
	switch t {
	case TypeNull:
		return "null"
	case TypeBool:
		return "boolean"
	case TypeInt:
		return "integer"
	case TypeDouble:
		return "double precision"
	case TypeText:
		return "text"
	case TypeBytea:
		return "bytea"
	case TypeTimestamp:
		return "timestamp"
	case TypeAny:
		return "any"
	case TypeVector:
		return "vector"
	}
	return "unknown"
}

// Value is one SQL value. Exactly one of the payload fields is meaningful,
// selected by T. Values are small and passed by copy everywhere.
type Value struct {
	T   Type
	B   bool      // TypeBool
	I   int64     // TypeInt
	F   float64   // TypeDouble
	S   string    // TypeText
	Raw []byte    // TypeBytea
	TS  time.Time // TypeTimestamp
	Vec []float32 // TypeVector
}

// Convenience constructors — they keep the executor readable.
func nullV() Value              { return Value{T: TypeNull} }
func boolV(b bool) Value        { return Value{T: TypeBool, B: b} }
func intV(i int64) Value        { return Value{T: TypeInt, I: i} }
func doubleV(f float64) Value   { return Value{T: TypeDouble, F: f} }
func textV(s string) Value      { return Value{T: TypeText, S: s} }
func byteaV(b []byte) Value     { return Value{T: TypeBytea, Raw: b} }
func timeV(t time.Time) Value   { return Value{T: TypeTimestamp, TS: t.UTC()} }
func vectorV(v []float32) Value { return Value{T: TypeVector, Vec: v} }

// IsNull reports whether the value is SQL NULL.
func (v Value) IsNull() bool { return v.T == TypeNull }

// isNumeric groups the two numeric runtime types.
func (v Value) isNumeric() bool { return v.T == TypeInt || v.T == TypeDouble }

// asFloat widens any numeric value to float64 for mixed-type arithmetic.
func (v Value) asFloat() float64 {
	if v.T == TypeInt {
		return float64(v.I)
	}
	return v.F
}

// typeofName mirrors chai's typeof(): integers that fit in 32 bits are
// called "integer", larger ones "bigint".
func (v Value) typeofName() string {
	if v.T == TypeInt {
		if v.I >= math.MinInt32 && v.I <= math.MaxInt32 {
			return "integer"
		}
		return "bigint"
	}
	return v.T.String()
}

// pgTimestampFormat is how timestamps are rendered on the wire and in
// results — the format PostgreSQL clients expect for timestamp text.
const pgTimestampFormat = "2006-01-02 15:04:05.999999-07"

// FormatText renders a value as the text a PostgreSQL client receives in
// a DataRow (text format). NULL is handled by the caller (it is a length
// of -1 on the wire, not a string).
func (v Value) FormatText() string {
	switch v.T {
	case TypeBool:
		// PostgreSQL renders booleans as single letters in text format.
		if v.B {
			return "t"
		}
		return "f"
	case TypeInt:
		return strconv.FormatInt(v.I, 10)
	case TypeDouble:
		return formatDouble(v.F)
	case TypeText:
		return v.S
	case TypeBytea:
		// PostgreSQL's "hex" bytea output format: literal backslash-x
		// followed by two lowercase hex digits per byte.
		return `\x` + hex.EncodeToString(v.Raw)
	case TypeTimestamp:
		return v.TS.UTC().Format(pgTimestampFormat)
	case TypeVector:
		// Canonical pgvector text form: "[1,2.5,-3]" (vector.go).
		return formatVector(v.Vec)
	}
	return ""
}

// formatDouble renders a float with the minimal number of digits that
// round-trips, matching how test expectations are written ("1.5", "2").
func formatDouble(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// ---------------------------------------------------------------------------
// Comparison
// ---------------------------------------------------------------------------

// Compare orders two non-NULL values, returning -1/0/+1. It implements
// the dialect's cross-type rules (see the package comment). Callers must
// handle NULL before calling (three-valued logic lives in the evaluator).
func Compare(a, b Value) (int, error) {
	// Same-kind fast paths first; then the documented coercions.
	switch {
	case a.isNumeric() && b.isNumeric():
		return compareNumeric(a, b), nil
	case a.T == TypeText && b.T == TypeText:
		return strings.Compare(a.S, b.S), nil
	case a.T == TypeBool && b.T == TypeBool:
		return boolCompare(a.B, b.B), nil
	case a.T == TypeBytea && b.T == TypeBytea:
		return bytesCompare(a.Raw, b.Raw), nil
	case a.T == TypeTimestamp && b.T == TypeTimestamp:
		return a.TS.Compare(b.TS), nil

	// number vs text: the text side is cast to the number's type.
	case a.isNumeric() && b.T == TypeText:
		nb, err := castTextToNumeric(b.S, a.T)
		if err != nil {
			return 0, err
		}
		return compareNumeric(a, nb), nil
	case a.T == TypeText && b.isNumeric():
		na, err := castTextToNumeric(a.S, b.T)
		if err != nil {
			return 0, err
		}
		return compareNumeric(na, b), nil

	// boolean vs text: the text side is cast to boolean ('t', 'true'...).
	case a.T == TypeBool && b.T == TypeText:
		bb, err := castTextToBool(b.S)
		if err != nil {
			return 0, err
		}
		return boolCompare(a.B, bb), nil
	case a.T == TypeText && b.T == TypeBool:
		ba, err := castTextToBool(a.S)
		if err != nil {
			return 0, err
		}
		return boolCompare(ba, b.B), nil

	// timestamp vs text: parse the text as a timestamp.
	case a.T == TypeTimestamp && b.T == TypeText:
		tb, err := castTextToTimestamp(b.S)
		if err != nil {
			return 0, err
		}
		return a.TS.Compare(tb), nil
	case a.T == TypeText && b.T == TypeTimestamp:
		ta, err := castTextToTimestamp(a.S)
		if err != nil {
			return 0, err
		}
		return ta.Compare(b.TS), nil

	// bytea vs text: parse the text as a bytea literal ('\xAB...').
	case a.T == TypeBytea && b.T == TypeText:
		rb, err := castTextToBytea(b.S)
		if err != nil {
			return 0, err
		}
		return bytesCompare(a.Raw, rb), nil
	case a.T == TypeText && b.T == TypeBytea:
		ra, err := castTextToBytea(a.S)
		if err != nil {
			return 0, err
		}
		return bytesCompare(ra, b.Raw), nil
	}
	return 0, fmt.Errorf("cannot compare %s with %s", a.T, b.T)
}

// compareNumeric orders two numeric values. Two integers compare exactly;
// any double involved switches to float comparison (like chai/Postgres).
// NaN is ordered above every other number and equal only to itself, so
// comparison stays a total order (IEEE "NaN is unordered" would otherwise
// make NaN compare equal to everything here) and agrees with the key
// encoding in keyenc.go, which the planner's index ranges rely on.
func compareNumeric(a, b Value) int {
	if a.T == TypeInt && b.T == TypeInt {
		switch {
		case a.I < b.I:
			return -1
		case a.I > b.I:
			return 1
		}
		return 0
	}
	af, bf := a.asFloat(), b.asFloat()
	an, bn := math.IsNaN(af), math.IsNaN(bf)
	switch {
	case an && bn:
		return 0
	case an:
		return 1
	case bn:
		return -1
	case af < bf:
		return -1
	case af > bf:
		return 1
	}
	return 0
}

// boolCompare orders booleans with false < true, as everywhere in SQL.
func boolCompare(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

// bytesCompare is bytes.Compare without importing bytes here twice.
func bytesCompare(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// sortCompare provides a total order across ALL types for ORDER BY and
// DISTINCT: NULL sorts first, then values group by type in the Type
// constant order, and only same-group values compare semantically. This
// makes sorting a heterogeneous column (possible after UNION) stable and
// error-free instead of failing mid-sort.
func sortCompare(a, b Value) int {
	ga, gb := sortGroup(a.T), sortGroup(b.T)
	if ga != gb {
		if ga < gb {
			return -1
		}
		return 1
	}
	if a.T == TypeNull {
		return 0
	}
	c, err := Compare(a, b)
	if err != nil {
		return 0 // same group values always compare; defensive only
	}
	return c
}

// sortGroup buckets types so INTEGER and DOUBLE share one numeric bucket.
func sortGroup(t Type) int {
	switch t {
	case TypeNull:
		return 0
	case TypeBool:
		return 1
	case TypeInt, TypeDouble:
		return 2
	case TypeText:
		return 3
	case TypeBytea:
		return 4
	case TypeTimestamp:
		return 5
	case TypeVector:
		// Vectors have no defined SQL order (Compare rejects them); the
		// bucket only keeps sortCompare total so a stray heterogeneous sort
		// cannot panic. Within the bucket vectors compare "equal".
		return 6
	}
	return 7
}

// ---------------------------------------------------------------------------
// Arithmetic
// ---------------------------------------------------------------------------

// Arith evaluates a binary arithmetic/bitwise operator. NULL operands
// yield NULL; a numeric mixed with a non-numeric yields NULL (chai rule);
// division by zero and int64 overflow are hard errors.
func Arith(op string, a, b Value) (Value, error) {
	if a.IsNull() || b.IsNull() {
		return nullV(), nil
	}
	// The dialect only does arithmetic on numbers. Anything else is a
	// silent NULL, per chai's expr/arithmetic.sql (1 + 'a' → NULL).
	if !a.isNumeric() || !b.isNumeric() {
		return nullV(), nil
	}
	// Bitwise operators demand integers on both sides.
	switch op {
	case "&", "|", "^":
		if a.T != TypeInt || b.T != TypeInt {
			return nullV(), nil
		}
		switch op {
		case "&":
			return intV(a.I & b.I), nil
		case "|":
			return intV(a.I | b.I), nil
		default:
			return intV(a.I ^ b.I), nil
		}
	}
	// Pure integer arithmetic stays integer (1/2 == 0) with overflow
	// checks; any double operand promotes the whole operation to float.
	if a.T == TypeInt && b.T == TypeInt {
		return intArith(op, a.I, b.I)
	}
	af, bf := a.asFloat(), b.asFloat()
	switch op {
	case "+":
		return doubleV(af + bf), nil
	case "-":
		return doubleV(af - bf), nil
	case "*":
		return doubleV(af * bf), nil
	case "/":
		if bf == 0 {
			return Value{}, fmt.Errorf("division by zero")
		}
		return doubleV(af / bf), nil
	case "%":
		if bf == 0 {
			return Value{}, fmt.Errorf("division by zero")
		}
		return doubleV(math.Mod(af, bf)), nil
	}
	return Value{}, fmt.Errorf("unknown operator %q", op)
}

// intArith is checked integer arithmetic with chai's width rule: when both
// operands fit in 32 bits they are INTEGERs and the result must also fit
// in 32 bits (1000000000 * 1000000000 errors even though it fits int64);
// otherwise the operation is BIGINT-wide. The error spellings are chai's,
// including the quirk that 64-bit multiply overflow says "integer".
func intArith(op string, a, b int64) (Value, error) {
	int32Wide := a >= math.MinInt32 && a <= math.MaxInt32 &&
		b >= math.MinInt32 && b <= math.MaxInt32
	fits := func(r int64) bool {
		return !int32Wide || (r >= math.MinInt32 && r <= math.MaxInt32)
	}
	switch op {
	case "+":
		r := a + b
		// 64-bit overflow iff the operands share a sign and the result flipped.
		if (a > 0 && b > 0 && r < 0) || (a < 0 && b < 0 && r >= 0) {
			return Value{}, fmt.Errorf("bigint out of range")
		}
		if !fits(r) {
			return Value{}, fmt.Errorf("integer out of range")
		}
		return intV(r), nil
	case "-":
		r := a - b
		if (a >= 0 && b < 0 && r < 0) || (a < 0 && b > 0 && r >= 0) {
			return Value{}, fmt.Errorf("bigint out of range")
		}
		if !fits(r) {
			return Value{}, fmt.Errorf("integer out of range")
		}
		return intV(r), nil
	case "*":
		if a != 0 && b != 0 {
			r := a * b
			if r/b != a || !fits(r) {
				return Value{}, fmt.Errorf("integer out of range")
			}
			return intV(r), nil
		}
		return intV(0), nil
	case "/":
		if b == 0 {
			return Value{}, fmt.Errorf("division by zero")
		}
		return intV(a / b), nil
	case "%":
		if b == 0 {
			return Value{}, fmt.Errorf("division by zero")
		}
		return intV(a % b), nil
	}
	return Value{}, fmt.Errorf("unknown operator %q", op)
}

// ---------------------------------------------------------------------------
// Casts
// ---------------------------------------------------------------------------

// castTextToNumeric parses text as the numeric type of the other operand.
// The error text mirrors chai's ("cannot cast \"a\" as integer: ...").
func castTextToNumeric(s string, target Type) (Value, error) {
	if target == TypeInt {
		i, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			// A decimal-looking string still compares against ints fine
			// as a float ('1.5' vs 1), so fall back to float parsing.
			f, ferr := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if ferr != nil {
				return Value{}, fmt.Errorf("cannot cast %q as integer: %v", s, err)
			}
			return doubleV(f), nil
		}
		return intV(i), nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return Value{}, fmt.Errorf("cannot cast %q as double precision: %v", s, err)
	}
	return doubleV(f), nil
}

// castTextToBool understands the PostgreSQL boolean spellings.
func castTextToBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "t", "true", "y", "yes", "on", "1":
		return true, nil
	case "f", "false", "n", "no", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("cannot cast %q as boolean", s)
}

// timestampFormats are the accepted spellings for timestamp text, tried
// in order. RFC3339 covers machine input; the space-separated forms cover
// what humans and psql type.
var timestampFormats = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999-07",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"2006-01", // chai accepts partial dates: '2023-04' is April 1st
	"2006",    // and '2023' is January 1st (order_by_timestamp.sql)
}

// castTextToTimestamp parses timestamp text; naked dates/times are UTC.
func castTextToTimestamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, f := range timestampFormats {
		if t, err := time.ParseInLocation(f, s, time.UTC); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot cast %q as timestamp", s)
}

// castTextToBytea converts text to bytea the way chai does: the text is
// decoded as standard base64 (expr/cast.sql: 'YXNkaW5l'::BYTEA is the bytes
// of "asdine"). Hex-form '\x..' strings never reach here — the lexer already
// turns them into bytea literals.
func castTextToBytea(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("cannot cast %q as bytea: %v", s, err)
	}
	return b, nil
}

// CastTo converts a value to a target type — the machinery behind both
// CAST(x AS t) and coercing INSERT/UPDATE values into a column's declared
// type. NULL casts to NULL of any type; casting to "any" (a typeless
// column) keeps the value as-is. The conversion matrix and error wording
// clone chai's (expr/cast.sql): notably bool→text yields 'true'/'false',
// text↔bytea is base64, and unsupported pairs error as
// `cannot cast "<from>" as "<to>"`.
func CastTo(v Value, target Type) (Value, error) {
	if v.IsNull() || v.T == target || target == TypeAny {
		return v, nil
	}
	switch target {
	case TypeInt:
		switch v.T {
		case TypeDouble:
			// Doubles truncate toward zero when stored into an integer
			// column (chai: UPDATE ... SET a = 15.2 stores 15).
			if v.F > math.MaxInt64 || v.F < math.MinInt64 || math.IsNaN(v.F) {
				return Value{}, fmt.Errorf("integer out of range")
			}
			return intV(int64(v.F)), nil
		case TypeText:
			i, err := strconv.ParseInt(strings.TrimSpace(v.S), 10, 64)
			if err != nil {
				// chai also accepts decimal text ('100.5' casts to 100);
				// keep the ParseInt error as the message when both fail,
				// because INSERT/check.sql asserts that exact wording.
				if f, ferr := strconv.ParseFloat(strings.TrimSpace(v.S), 64); ferr == nil {
					return CastTo(doubleV(f), TypeInt)
				}
				return Value{}, fmt.Errorf("cannot cast %q as integer: %v", v.S, err)
			}
			return intV(i), nil
		case TypeBool:
			if v.B {
				return intV(1), nil
			}
			return intV(0), nil
		}
	case TypeDouble:
		switch v.T {
		case TypeInt:
			return doubleV(float64(v.I)), nil
		case TypeText:
			f, err := strconv.ParseFloat(strings.TrimSpace(v.S), 64)
			if err != nil {
				return Value{}, fmt.Errorf("cannot cast %q as double precision: %v", v.S, err)
			}
			return doubleV(f), nil
		}
	case TypeBool:
		switch v.T {
		case TypeText:
			b, err := castTextToBool(v.S)
			if err != nil {
				return Value{}, err
			}
			return boolV(b), nil
		case TypeInt:
			return boolV(v.I != 0), nil
		}
	case TypeText:
		switch v.T {
		case TypeBool:
			// chai spells casted booleans out ('true'), unlike the pg wire
			// text format ('t').
			if v.B {
				return textV("true"), nil
			}
			return textV("false"), nil
		case TypeInt, TypeDouble, TypeTimestamp, TypeVector:
			// Vectors render as their canonical '[1,2.5,-3]' literal, which
			// parses back to the identical vector (float32 round-trip).
			return textV(v.FormatText()), nil
		case TypeBytea:
			// bytea→text is base64, the inverse of text→bytea.
			return textV(base64.StdEncoding.EncodeToString(v.Raw)), nil
		}
	case TypeBytea:
		if v.T == TypeText {
			b, err := castTextToBytea(v.S)
			if err != nil {
				return Value{}, err
			}
			return byteaV(b), nil
		}
	case TypeTimestamp:
		if v.T == TypeText {
			t, err := castTextToTimestamp(v.S)
			if err != nil {
				return Value{}, err
			}
			return timeV(t), nil
		}
	case TypeVector:
		// Only the pgvector text literal form converts to a vector; the
		// strict parser (vector.go) rejects anything else. Dimension checks
		// against a column happen at the write site, which knows the column.
		if v.T == TypeText {
			vec, err := parseVectorText(v.S)
			if err != nil {
				return Value{}, err
			}
			return vectorV(vec), nil
		}
	}
	return Value{}, fmt.Errorf("cannot cast %q as %q", v.T.String(), target.String())
}

// ---------------------------------------------------------------------------
// Row storage encoding (the VALUE side of the KV pair; the KEY side lives
// in keyenc.go)
// ---------------------------------------------------------------------------

// A stored row is a JSON object mapping column name to a tagged value:
//
//	{"a": {"t":"i","v":42}, "b": {"t":"s","v":"hi"}, "c": {"t":"n"}}
//
// Tags: "n" null, "b" boolean, "i" integer (JSON number, decoded with
// UseNumber so the full int64 range survives), "d" double, "s" text,
// "x" bytea (base64), "t" timestamp (RFC3339Nano), "v" vector (the
// canonical pgvector text form "[1,2.5,-3]", each element the shortest
// string that round-trips its float32 — exact, and as inspectable as the
// rest of the row). JSON keeps the stored rows human-inspectable through
// the ordinary KV API — a debugging gift — at a modest size cost.

// taggedValue is the JSON shape of one stored value.
type taggedValue struct {
	T string          `json:"t"`
	V json.RawMessage `json:"v,omitempty"`
}

// encodeRow serializes a row (column → value) for storage.
func encodeRow(row map[string]Value) ([]byte, error) {
	out := make(map[string]taggedValue, len(row))
	for k, v := range row {
		tv, err := encodeValueJSON(v)
		if err != nil {
			return nil, err
		}
		out[k] = tv
	}
	return json.Marshal(out)
}

// encodeValueJSON converts one Value to its tagged JSON form.
func encodeValueJSON(v Value) (taggedValue, error) {
	switch v.T {
	case TypeNull:
		return taggedValue{T: "n"}, nil
	case TypeBool:
		raw, _ := json.Marshal(v.B)
		return taggedValue{T: "b", V: raw}, nil
	case TypeInt:
		return taggedValue{T: "i", V: json.RawMessage(strconv.FormatInt(v.I, 10))}, nil
	case TypeDouble:
		raw, err := json.Marshal(v.F)
		if err != nil {
			// JSON cannot carry NaN/Inf; store them as strings instead.
			raw, _ = json.Marshal(formatDouble(v.F))
		}
		return taggedValue{T: "d", V: raw}, nil
	case TypeText:
		raw, err := json.Marshal(v.S)
		if err != nil {
			return taggedValue{}, err
		}
		return taggedValue{T: "s", V: raw}, nil
	case TypeBytea:
		raw, _ := json.Marshal(base64.StdEncoding.EncodeToString(v.Raw))
		return taggedValue{T: "x", V: raw}, nil
	case TypeTimestamp:
		raw, _ := json.Marshal(v.TS.UTC().Format(time.RFC3339Nano))
		return taggedValue{T: "t", V: raw}, nil
	case TypeVector:
		// The canonical text form is lossless for float32 (shortest
		// round-tripping decimal per element), so it doubles as the storage
		// encoding — see the byte-format comment above.
		raw, _ := json.Marshal(formatVector(v.Vec))
		return taggedValue{T: "v", V: raw}, nil
	}
	return taggedValue{}, fmt.Errorf("cannot store value of type %s", v.T)
}

// decodeRow parses a stored row back into column → Value.
func decodeRow(data []byte) (map[string]Value, error) {
	var raw map[string]taggedValue
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("corrupt row: %v", err)
	}
	out := make(map[string]Value, len(raw))
	for k, tv := range raw {
		v, err := decodeValueJSON(tv)
		if err != nil {
			return nil, fmt.Errorf("corrupt row column %q: %v", k, err)
		}
		out[k] = v
	}
	return out, nil
}

// decodeValueJSON reverses encodeValueJSON.
func decodeValueJSON(tv taggedValue) (Value, error) {
	switch tv.T {
	case "n":
		return nullV(), nil
	case "b":
		var b bool
		if err := json.Unmarshal(tv.V, &b); err != nil {
			return Value{}, err
		}
		return boolV(b), nil
	case "i":
		i, err := strconv.ParseInt(strings.TrimSpace(string(tv.V)), 10, 64)
		if err != nil {
			return Value{}, err
		}
		return intV(i), nil
	case "d":
		var f float64
		if err := json.Unmarshal(tv.V, &f); err != nil {
			// NaN/Inf were stored as strings; parse those.
			var s string
			if err2 := json.Unmarshal(tv.V, &s); err2 != nil {
				return Value{}, err
			}
			pf, err2 := strconv.ParseFloat(s, 64)
			if err2 != nil {
				return Value{}, err2
			}
			return doubleV(pf), nil
		}
		return doubleV(f), nil
	case "s":
		var s string
		if err := json.Unmarshal(tv.V, &s); err != nil {
			return Value{}, err
		}
		return textV(s), nil
	case "x":
		var s string
		if err := json.Unmarshal(tv.V, &s); err != nil {
			return Value{}, err
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return Value{}, err
		}
		return byteaV(b), nil
	case "t":
		var s string
		if err := json.Unmarshal(tv.V, &s); err != nil {
			return Value{}, err
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return Value{}, err
		}
		return timeV(t), nil
	case "v":
		var s string
		if err := json.Unmarshal(tv.V, &s); err != nil {
			return Value{}, err
		}
		vec, err := parseVectorText(s)
		if err != nil {
			return Value{}, err
		}
		return vectorV(vec), nil
	}
	return Value{}, fmt.Errorf("unknown value tag %q", tv.T)
}
