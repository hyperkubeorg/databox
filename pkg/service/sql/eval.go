// eval.go evaluates expressions against a single row. It implements the
// dialect's three-valued logic and cross-type coercion by delegating the
// actual comparisons/arithmetic to value.go, so the executor and the
// planner share exactly one set of semantics (§13).
package sql

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// row is one evaluated tuple: column name → value. Aggregation and
// projection both operate on this shape.
type row map[string]Value

// evalExpr computes the value of e against r. NULL propagates per the
// dialect's three-valued logic; type mismatches follow value.go's rules.
func evalExpr(e Expr, r row) (Value, error) {
	switch x := e.(type) {
	case *Literal:
		return x.Val, nil

	case *ColumnRef:
		// A nil row means the expression runs without a table (table-less
		// SELECT, INSERT VALUES, DEFAULT) — chai's wording for a column
		// reference there is "no table specified" (expr/arithmetic.sql).
		if r == nil {
			return nullV(), fmt.Errorf("no table specified")
		}
		// Qualified refs (t.col) look up the qualified key that joined or
		// aliased rows carry (join.go); bare refs use the column name.
		key := x.Column
		if x.Table != "" {
			key = x.Table + "." + x.Column
		}
		if v, ok := r[key]; ok {
			return v, nil
		}
		return nullV(), fmt.Errorf("no such column: %s", x.String())

	case *Param:
		// Parameters are substituted by the wire layer before execution;
		// reaching one here means the client bound too few values.
		return nullV(), fmt.Errorf("parameter $%d is not bound", x.N)

	case *Unary:
		return evalUnary(x, r)

	case *Binary:
		return evalBinary(x, r)

	case *Between:
		return evalBetween(x, r)

	case *InList:
		return evalInList(x, r)

	case *IsNull:
		v, err := evalExpr(x.X, r)
		if err != nil {
			return Value{}, err
		}
		isNull := v.IsNull()
		if x.Not {
			return boolV(!isNull), nil
		}
		return boolV(isNull), nil

	case *Cast:
		v, err := evalExpr(x.X, r)
		if err != nil {
			return Value{}, err
		}
		cv, err := CastTo(v, x.To)
		if err != nil {
			return Value{}, err
		}
		// A vector cast with a declared dimension ("x::VECTOR(3)") enforces
		// it, the same check a vector column applies on write.
		if x.To == TypeVector && x.Dim > 0 && !cv.IsNull() && len(cv.Vec) != x.Dim {
			return Value{}, fmt.Errorf("expected %d dimensions, got %d", x.Dim, len(cv.Vec))
		}
		return cv, nil

	case *FuncCall:
		return evalFunc(x, r)
	}
	return Value{}, fmt.Errorf("cannot evaluate %T", e)
}

// evalUnary handles -, + and NOT.
func evalUnary(x *Unary, r row) (Value, error) {
	v, err := evalExpr(x.X, r)
	if err != nil {
		return Value{}, err
	}
	if v.IsNull() {
		return nullV(), nil
	}
	switch x.Op {
	case "+":
		if !v.isNumeric() {
			return nullV(), nil
		}
		return v, nil
	case "-":
		switch v.T {
		case TypeInt:
			return intV(-v.I), nil
		case TypeDouble:
			return doubleV(-v.F), nil
		}
		return nullV(), nil
	case "NOT":
		b, ok := truth(v)
		if !ok {
			return nullV(), nil
		}
		return boolV(!b), nil
	}
	return Value{}, fmt.Errorf("unknown unary operator %q", x.Op)
}

// evalBinary handles arithmetic, comparison, logical, LIKE and ||.
func evalBinary(x *Binary, r row) (Value, error) {
	switch x.Op {
	case "AND", "OR":
		return evalLogical(x, r)
	}
	l, err := evalExpr(x.L, r)
	if err != nil {
		return Value{}, err
	}
	rt, err := evalExpr(x.R, r)
	if err != nil {
		return Value{}, err
	}
	switch x.Op {
	case "+", "-", "*", "/", "%", "&", "|", "^":
		return Arith(x.Op, l, rt)
	case "=", "!=", "<", "<=", ">", ">=":
		return evalCompare(x.Op, l, rt)
	case "<->", "<#>", "<=>":
		// Vector distance operators (vector.go): both operands coerce to
		// vectors, the result is a DOUBLE PRECISION distance.
		return evalVectorDistance(x.Op, l, rt)
	case "||":
		// String concatenation: NULL if either side is NULL, otherwise the
		// textual renderings joined.
		if l.IsNull() || rt.IsNull() {
			return nullV(), nil
		}
		return textV(l.FormatText() + rt.FormatText()), nil
	case "LIKE", "NOT LIKE":
		return evalLike(l, rt, x.Op == "NOT LIKE")
	}
	return Value{}, fmt.Errorf("unknown operator %q", x.Op)
}

// evalLogical implements AND/OR with SQL three-valued logic: AND is false
// if either side is false (even with a NULL present); OR is true if either
// side is true.
func evalLogical(x *Binary, r row) (Value, error) {
	lv, err := evalExpr(x.L, r)
	if err != nil {
		return Value{}, err
	}
	lb, lok := truth(lv)
	// Short-circuit on the determining value.
	if x.Op == "AND" && lok && !lb {
		return boolV(false), nil
	}
	if x.Op == "OR" && lok && lb {
		return boolV(true), nil
	}
	rv, err := evalExpr(x.R, r)
	if err != nil {
		return Value{}, err
	}
	rb, rok := truth(rv)
	switch x.Op {
	case "AND":
		if rok && !rb {
			return boolV(false), nil
		}
		if lok && rok {
			return boolV(lb && rb), nil
		}
		return nullV(), nil
	default: // OR
		if rok && rb {
			return boolV(true), nil
		}
		if lok && rok {
			return boolV(lb || rb), nil
		}
		return nullV(), nil
	}
}

// evalCompare evaluates a comparison operator into a boolean or NULL.
func evalCompare(op string, l, r Value) (Value, error) {
	if l.IsNull() || r.IsNull() {
		return nullV(), nil
	}
	c, err := Compare(l, r)
	if err != nil {
		return Value{}, err
	}
	switch op {
	case "=":
		return boolV(c == 0), nil
	case "!=":
		return boolV(c != 0), nil
	case "<":
		return boolV(c < 0), nil
	case "<=":
		return boolV(c <= 0), nil
	case ">":
		return boolV(c > 0), nil
	case ">=":
		return boolV(c >= 0), nil
	}
	return Value{}, fmt.Errorf("unknown comparison %q", op)
}

// evalBetween is lo <= x <= hi with NULL propagation.
func evalBetween(x *Between, r row) (Value, error) {
	v, err := evalExpr(x.X, r)
	if err != nil {
		return Value{}, err
	}
	lo, err := evalExpr(x.Lo, r)
	if err != nil {
		return Value{}, err
	}
	hi, err := evalExpr(x.Hi, r)
	if err != nil {
		return Value{}, err
	}
	if v.IsNull() || lo.IsNull() || hi.IsNull() {
		return nullV(), nil
	}
	geLo, err := evalCompare(">=", v, lo)
	if err != nil {
		return Value{}, err
	}
	leHi, err := evalCompare("<=", v, hi)
	if err != nil {
		return Value{}, err
	}
	b1, _ := truth(geLo)
	b2, _ := truth(leHi)
	res := b1 && b2
	if x.Not {
		res = !res
	}
	return boolV(res), nil
}

// evalInList is x IN (list...) with NULL semantics: a match yields true; no
// match yields false unless a NULL was present, in which case NULL.
func evalInList(x *InList, r row) (Value, error) {
	v, err := evalExpr(x.X, r)
	if err != nil {
		return Value{}, err
	}
	if v.IsNull() {
		return nullV(), nil
	}
	sawNull := false
	for _, item := range x.List {
		iv, err := evalExpr(item, r)
		if err != nil {
			return Value{}, err
		}
		if iv.IsNull() {
			sawNull = true
			continue
		}
		c, err := Compare(v, iv)
		if err != nil {
			return Value{}, err
		}
		if c == 0 {
			return boolV(!x.Not), nil
		}
	}
	if sawNull {
		return nullV(), nil
	}
	return boolV(x.Not), nil
}

// evalLike implements SQL LIKE with % (any run) and _ (single char).
func evalLike(l, r Value, negate bool) (Value, error) {
	if l.IsNull() || r.IsNull() {
		return nullV(), nil
	}
	s, pat := l.FormatText(), r.FormatText()
	m := likeMatch(pat, s)
	if negate {
		m = !m
	}
	return boolV(m), nil
}

// likeMatch is a straightforward backtracking matcher for LIKE patterns.
func likeMatch(pat, s string) bool {
	// Dynamic programming over runes keeps it correct for UTF-8 input.
	pr, sr := []rune(pat), []rune(s)
	np, ns := len(pr), len(sr)
	dp := make([][]bool, np+1)
	for i := range dp {
		dp[i] = make([]bool, ns+1)
	}
	dp[0][0] = true
	for i := 1; i <= np; i++ {
		if pr[i-1] == '%' {
			dp[i][0] = dp[i-1][0]
		}
	}
	for i := 1; i <= np; i++ {
		for j := 1; j <= ns; j++ {
			switch pr[i-1] {
			case '%':
				dp[i][j] = dp[i-1][j] || dp[i][j-1]
			case '_':
				dp[i][j] = dp[i-1][j-1]
			default:
				dp[i][j] = dp[i-1][j-1] && pr[i-1] == sr[j-1]
			}
		}
	}
	return dp[np][ns]
}

// evalFunc evaluates the scalar functions the dialect ships. Aggregates
// are handled by the executor, not here, and reaching one at this level is
// a planner bug.
func evalFunc(x *FuncCall, r row) (Value, error) {
	if isAggregateName(x.Name) {
		return Value{}, fmt.Errorf("aggregate %s() not allowed here", x.Name)
	}
	args := make([]Value, len(x.Args))
	for i, a := range x.Args {
		v, err := evalExpr(a, r)
		if err != nil {
			return Value{}, err
		}
		args[i] = v
	}
	switch x.Name {
	case "lower":
		// Non-text input yields NULL, not an error (SELECT/STRINGS corpus).
		if len(args) == 1 {
			if args[0].T != TypeText {
				return nullV(), nil
			}
			return textV(strings.ToLower(args[0].S)), nil
		}
	case "upper":
		if len(args) == 1 {
			if args[0].T != TypeText {
				return nullV(), nil
			}
			return textV(strings.ToUpper(args[0].S)), nil
		}
	case "trim", "ltrim", "rtrim":
		return evalTrim(x.Name, args)
	case "length":
		if len(args) == 1 {
			if args[0].IsNull() {
				return nullV(), nil
			}
			return intV(int64(len([]rune(args[0].FormatText())))), nil
		}
	case "typeof":
		if len(args) == 1 {
			return textV(args[0].typeofName()), nil
		}
	case "now":
		// now() is the statement timestamp. It lives behind a variable so
		// the conformance tests can pin the clock the way chai's do.
		if len(args) == 0 {
			return timeV(nowFunc()), nil
		}
	case "gen_random_uuid":
		// pg's v4 UUID generator (uuid.go); the idiomatic UUID primary key
		// is `id UUID PRIMARY KEY DEFAULT gen_random_uuid()`.
		if len(args) == 0 {
			return textV(newUUIDv4()), nil
		}
	case "abs":
		if len(args) == 1 {
			switch args[0].T {
			case TypeInt:
				if args[0].I < 0 {
					return intV(-args[0].I), nil
				}
				return args[0], nil
			case TypeDouble:
				return doubleV(math.Abs(args[0].F)), nil
			case TypeNull:
				return nullV(), nil
			}
		}
	case "coalesce":
		for _, a := range args {
			if !a.IsNull() {
				return a, nil
			}
		}
		return nullV(), nil
	case "vector_dims", "l2_distance", "inner_product", "cosine_distance", "l2_norm":
		// The pgvector scalar functions live in vector.go.
		return evalVectorFunc(x.Name, args)
	}
	return Value{}, fmt.Errorf("unknown function %s/%d", x.Name, len(args))
}

// nowFunc supplies now(); tests override it to a fixed instant.
var nowFunc = time.Now

// evalTrim implements TRIM/LTRIM/RTRIM(text[, cutset]). Following chai,
// any non-text subject or cutset makes the result NULL rather than an
// error (SELECT/STRINGS/trim.sql: TRIM(42) is NULL).
func evalTrim(name string, args []Value) (Value, error) {
	if len(args) != 1 && len(args) != 2 {
		return Value{}, fmt.Errorf("%s expects 1 or 2 arguments", name)
	}
	if args[0].T != TypeText {
		return nullV(), nil
	}
	cutset := " "
	if len(args) == 2 {
		if args[1].T != TypeText {
			return nullV(), nil
		}
		cutset = args[1].S
	}
	switch name {
	case "ltrim":
		return textV(strings.TrimLeft(args[0].S, cutset)), nil
	case "rtrim":
		return textV(strings.TrimRight(args[0].S, cutset)), nil
	}
	return textV(strings.Trim(args[0].S, cutset)), nil
}

// truth extracts a boolean from a value for logical contexts, coercing text
// and numbers the way the dialect does. ok=false means the value is NULL or
// otherwise not truth-valued (NULL in three-valued logic).
func truth(v Value) (b bool, ok bool) {
	switch v.T {
	case TypeBool:
		return v.B, true
	case TypeNull:
		return false, false
	case TypeInt:
		return v.I != 0, true
	case TypeDouble:
		return v.F != 0, true
	case TypeText:
		if bb, err := castTextToBool(v.S); err == nil {
			return bb, true
		}
	}
	return false, false
}

// predicateTrue reports whether a WHERE/HAVING predicate passes: only a
// definite boolean true admits the row (NULL and false both reject).
func predicateTrue(e Expr, r row) (bool, error) {
	if e == nil {
		return true, nil
	}
	v, err := evalExpr(e, r)
	if err != nil {
		return false, err
	}
	b, ok := truth(v)
	return ok && b, nil
}
