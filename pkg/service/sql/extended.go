// extended.go implements the engine-side machinery of the PostgreSQL
// extended protocol (§13): $N parameter counting and
// substitution, parameter decoding from wire format, and statement
// description (result columns with type OIDs) so Parse/Bind/Describe/
// Execute can answer clients like pgx and psycopg before running anything.
package sql

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Expression walking: every statement's expression fields in one place
// ---------------------------------------------------------------------------

// rewriteStmtExprs applies f to every expression tree in a statement,
// assigning back the (possibly replaced) tree. Both parameter counting and
// parameter substitution ride on this single walker, so no statement shape
// can be covered by one and missed by the other.
func rewriteStmtExprs(st Statement, f func(Expr) (Expr, error)) error {
	apply := func(e *Expr) error {
		if *e == nil {
			return nil
		}
		ne, err := rewriteExpr(*e, f)
		if err != nil {
			return err
		}
		*e = ne
		return nil
	}
	applyItems := func(items []SelectItem) error {
		for i := range items {
			if items[i].Star {
				continue
			}
			if err := apply(&items[i].Expr); err != nil {
				return err
			}
		}
		return nil
	}
	applyCore := func(core *SelectCore) error {
		if err := applyItems(core.Items); err != nil {
			return err
		}
		if err := apply(&core.Where); err != nil {
			return err
		}
		for i := range core.GroupBy {
			if err := apply(&core.GroupBy[i]); err != nil {
				return err
			}
		}
		return nil
	}
	applySelect := func(s *Select) error {
		if s == nil {
			return nil
		}
		if err := applyCore(s.Core); err != nil {
			return err
		}
		for _, arm := range s.Unions {
			if err := applyCore(arm.Core); err != nil {
				return err
			}
		}
		for i := range s.OrderBy {
			if err := apply(&s.OrderBy[i].Expr); err != nil {
				return err
			}
		}
		if err := apply(&s.Limit); err != nil {
			return err
		}
		return apply(&s.Offset)
	}

	switch x := st.(type) {
	case *Select:
		return applySelect(x)
	case *Insert:
		for _, r := range x.Rows {
			for i := range r {
				if err := apply(&r[i]); err != nil {
					return err
				}
			}
		}
		if err := applySelect(x.Select); err != nil {
			return err
		}
		return applyItems(x.Returning)
	case *Update:
		for i := range x.Set {
			if err := apply(&x.Set[i].Value); err != nil {
				return err
			}
		}
		return apply(&x.Where)
	case *Delete:
		return apply(&x.Where)
	case *CreateTable:
		for i := range x.Columns {
			if x.Columns[i].Default != nil {
				if err := apply(&x.Columns[i].Default); err != nil {
					return err
				}
			}
		}
		for i := range x.Checks {
			if err := apply(&x.Checks[i]); err != nil {
				return err
			}
		}
		return nil
	}
	return nil // DDL without expressions, TxStmt
}

// rewriteExpr applies f bottom-up to every node of an expression tree,
// mutating children in place and returning the (possibly new) root.
func rewriteExpr(e Expr, f func(Expr) (Expr, error)) (Expr, error) {
	var err error
	sub := func(c Expr) Expr {
		if err != nil || c == nil {
			return c
		}
		var nc Expr
		nc, err = rewriteExpr(c, f)
		return nc
	}
	switch x := e.(type) {
	case *Unary:
		x.X = sub(x.X)
	case *Binary:
		x.L, x.R = sub(x.L), sub(x.R)
	case *Between:
		x.X, x.Lo, x.Hi = sub(x.X), sub(x.Lo), sub(x.Hi)
	case *InList:
		x.X = sub(x.X)
		for i := range x.List {
			x.List[i] = sub(x.List[i])
		}
	case *IsNull:
		x.X = sub(x.X)
	case *Cast:
		x.X = sub(x.X)
	case *FuncCall:
		for i := range x.Args {
			x.Args[i] = sub(x.Args[i])
		}
	}
	if err != nil {
		return nil, err
	}
	return f(e)
}

// countParams returns the highest $N referenced by any statement.
func countParams(stmts []Statement) (int, error) {
	max := 0
	for _, st := range stmts {
		err := rewriteStmtExprs(st, func(e Expr) (Expr, error) {
			if p, ok := e.(*Param); ok && p.N > max {
				max = p.N
			}
			return e, nil
		})
		if err != nil {
			return 0, err
		}
	}
	return max, nil
}

// bindParams replaces every $N with its bound value as a literal. The
// statements must be a fresh parse (the walker mutates them in place).
func bindParams(stmts []Statement, vals []Value) error {
	for _, st := range stmts {
		err := rewriteStmtExprs(st, func(e Expr) (Expr, error) {
			p, ok := e.(*Param)
			if !ok {
				return e, nil
			}
			if p.N < 1 || p.N > len(vals) {
				return nil, fmt.Errorf("parameter $%d is not bound", p.N)
			}
			v := vals[p.N-1]
			return &Literal{Val: v, Src: v.FormatText()}, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// PostgreSQL type OIDs and parameter decoding
// ---------------------------------------------------------------------------

// The pg type OIDs the layer speaks. Result columns whose type cannot be
// inferred stay text — every value has a text rendering, so text is always
// safe, while a wrong binary claim would corrupt the stream.
const (
	oidBool        = 16
	oidBytea       = 17
	oidInt8        = 20
	oidInt2        = 21
	oidInt4        = 23
	oidText        = 25
	oidFloat4      = 700
	oidFloat8      = 701
	oidVarchar     = 1043
	oidTimestamp   = 1114
	oidTimestamptz = 1184
	oidNumeric     = 1700
)

// oidForType maps an engine type to the OID advertised in RowDescription.
func oidForType(t Type) int32 {
	switch t {
	case TypeBool:
		return oidBool
	case TypeBytea:
		return oidBytea
	case TypeInt:
		return oidInt8
	case TypeDouble:
		return oidFloat8
	case TypeTimestamp:
		return oidTimestamptz
	}
	// TypeVector deliberately falls through to text: vectors travel the
	// wire in their canonical '[1,2.5,-3]' literal form (OID 25), the same
	// representation pgvector clients use for text-format columns.
	return oidText
}

// decodeParam converts one Bind parameter to an engine Value. A nil buffer
// is NULL. Text format works for every type the engine can coerce; binary
// covers the fixed-width types common drivers actually send.
func decodeParam(buf []byte, oid int32, binaryFmt bool) (Value, error) {
	if buf == nil {
		return nullV(), nil
	}
	if !binaryFmt {
		return decodeTextParam(string(buf), oid)
	}
	switch oid {
	case oidInt2:
		if len(buf) != 2 {
			return Value{}, fmt.Errorf("bad int2 parameter length %d", len(buf))
		}
		return intV(int64(int16(binary.BigEndian.Uint16(buf)))), nil
	case oidInt4:
		if len(buf) != 4 {
			return Value{}, fmt.Errorf("bad int4 parameter length %d", len(buf))
		}
		return intV(int64(int32(binary.BigEndian.Uint32(buf)))), nil
	case oidInt8:
		if len(buf) != 8 {
			return Value{}, fmt.Errorf("bad int8 parameter length %d", len(buf))
		}
		return intV(int64(binary.BigEndian.Uint64(buf))), nil
	case oidFloat4:
		if len(buf) != 4 {
			return Value{}, fmt.Errorf("bad float4 parameter length %d", len(buf))
		}
		return doubleV(float64(math.Float32frombits(binary.BigEndian.Uint32(buf)))), nil
	case oidFloat8:
		if len(buf) != 8 {
			return Value{}, fmt.Errorf("bad float8 parameter length %d", len(buf))
		}
		return doubleV(math.Float64frombits(binary.BigEndian.Uint64(buf))), nil
	case oidBool:
		if len(buf) != 1 {
			return Value{}, fmt.Errorf("bad bool parameter length %d", len(buf))
		}
		return boolV(buf[0] != 0), nil
	case oidBytea:
		return byteaV(append([]byte(nil), buf...)), nil
	case oidText, oidVarchar, 0:
		return textV(string(buf)), nil
	}
	return Value{}, fmt.Errorf("binary format not supported for parameter type OID %d", oid)
}

// decodeTextParam parses the text form of a parameter according to its
// declared OID; unknown OIDs stay text and rely on the dialect's own
// coercion at comparison/insert time.
func decodeTextParam(s string, oid int32) (Value, error) {
	switch oid {
	case oidInt2, oidInt4, oidInt8:
		i, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return Value{}, fmt.Errorf("bad integer parameter %q", s)
		}
		return intV(i), nil
	case oidFloat4, oidFloat8, oidNumeric:
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return Value{}, fmt.Errorf("bad float parameter %q", s)
		}
		return doubleV(f), nil
	case oidBool:
		b, err := castTextToBool(s)
		if err != nil {
			return Value{}, err
		}
		return boolV(b), nil
	case oidBytea:
		// pg's hex output format "\x...."; anything else stays raw bytes.
		if strings.HasPrefix(s, `\x`) {
			raw, err := hex.DecodeString(s[2:])
			if err != nil {
				return Value{}, fmt.Errorf("bad bytea parameter: %v", err)
			}
			return byteaV(raw), nil
		}
		return byteaV([]byte(s)), nil
	case oidTimestamp, oidTimestamptz:
		t, err := castTextToTimestamp(s)
		if err != nil {
			return Value{}, err
		}
		return timeV(t), nil
	}
	return textV(s), nil
}

// ---------------------------------------------------------------------------
// Statement description (Describe before Execute)
// ---------------------------------------------------------------------------

// stmtDescription is what Describe needs: result column names and OIDs.
// rowReturning is false for statements that only produce a command tag.
type stmtDescription struct {
	cols         []string
	oids         []int32
	rowReturning bool
}

// describeStatement predicts a statement's result shape without executing
// it. Column types are advertised only when statically known (a plain
// column reference, a cast, a literal, count()); everything else is text,
// which every value can serialize to.
func (e *Engine) describeStatement(ctx context.Context, st Statement) (stmtDescription, error) {
	switch s := st.(type) {
	case *Select:
		sc, err := e.schemaFor(ctx, s.Core.Table)
		if err != nil {
			return stmtDescription{}, err
		}
		var cols []string
		if s.Core.grouped() {
			for _, it := range s.Core.Items {
				if !it.Star {
					cols = append(cols, labelFor(it))
				}
			}
		} else {
			cols = e.projectionColumns(s.Core, sc)
		}
		return stmtDescription{cols: cols, oids: e.inferItemOIDs(s.Core.Items, sc), rowReturning: true}, nil
	case *Insert:
		if s.Returning == nil {
			return stmtDescription{}, nil
		}
		sc, err := e.schemaFor(ctx, s.Table)
		if err != nil {
			return stmtDescription{}, err
		}
		cols := returningColumns(s.Returning, sc)
		return stmtDescription{cols: cols, oids: e.inferItemOIDs(s.Returning, sc), rowReturning: true}, nil
	}
	return stmtDescription{}, nil
}

// schemaFor loads a table schema, tolerating the table-less case.
func (e *Engine) schemaFor(ctx context.Context, table string) (*tableSchema, error) {
	if table == "" {
		return nil, nil
	}
	return e.loadSchema(ctx, table)
}

// inferItemOIDs computes one OID per output column of a projection,
// expanding * into the table's columns.
func (e *Engine) inferItemOIDs(items []SelectItem, sc *tableSchema) []int32 {
	var oids []int32
	for _, it := range items {
		if it.Star {
			if sc != nil {
				for _, c := range sc.Columns {
					oids = append(oids, oidForType(c.Type))
				}
			}
			continue
		}
		oids = append(oids, inferExprOID(it.Expr, sc))
	}
	return oids
}

// inferExprOID guesses the wire type of one projected expression. Only
// shapes with a statically certain type get a non-text OID.
func inferExprOID(e Expr, sc *tableSchema) int32 {
	switch x := e.(type) {
	case *Literal:
		return oidForType(x.Val.T)
	case *ColumnRef:
		if sc != nil {
			if col, ok := sc.col(x.Column); ok {
				return oidForType(col.Type)
			}
		}
	case *Cast:
		return oidForType(x.To)
	case *IsNull:
		return oidBool
	case *Binary:
		switch x.Op {
		case "=", "!=", "<", "<=", ">", ">=", "AND", "OR", "LIKE", "NOT LIKE":
			return oidBool
		case "<->", "<#>", "<=>":
			// Vector distances are always DOUBLE PRECISION.
			return oidFloat8
		}
	case *Between, *InList:
		return oidBool
	case *FuncCall:
		switch x.Name {
		case "count", "length", "vector_dims":
			return oidInt8
		case "avg", "l2_distance", "inner_product", "cosine_distance", "l2_norm":
			return oidFloat8
		case "typeof", "lower", "upper", "trim", "ltrim", "rtrim":
			return oidText
		case "now":
			return oidTimestamptz
		}
	}
	return oidText
}
