// planner.go chooses the access path for SELECT/UPDATE/DELETE: a point
// lookup or ranged scan over the primary key, a ranged scan over a
// secondary index, or the full table scan as the always-correct fallback
// (§13: "lexer/parser → AST → planner → executor").
//
// The planner only ever narrows *where the executor reads*; it never
// relaxes filtering. Every statement re-applies its full WHERE clause to
// the rows a plan returns, so a plan may safely over-read (an inclusive
// bound where the predicate is exclusive) but must never under-read.
// When any doubt exists — non-constant comparisons, typeless columns,
// coercions that change a value — the planner falls back to scanning
// everything.
package sql

import (
	"context"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Plan representation
// ---------------------------------------------------------------------------

// keyRange is one contiguous slice of a table's or index's key space, in
// the hex key encoding (keyenc.go). start is inclusive, end exclusive;
// an empty string means unbounded on that side.
type keyRange struct {
	startHex string
	endHex   string
}

// scanPlan is the chosen access path for one table read.
type scanPlan struct {
	// index is the secondary index to drive the scan, or nil to scan the
	// table's own key range (the primary key).
	index *indexDef
	// ranges are the key slices to read. Empty means "everything" — the
	// full-scan fallback. Ranges are sorted and disjoint, so concatenating
	// their rows preserves ascending key order.
	ranges []keyRange
	// keyCols are the columns the scan yields rows in ascending order by
	// (the PK columns or the index columns). Empty when the scan order is
	// meaningless (implicit-rowid tables, typeless key columns).
	keyCols []string
	// fixedPrefix counts how many leading keyCols are pinned to a single
	// equality value by the plan; ORDER BY may skip over those.
	fixedPrefix int
	// TopK marks that the executor served this scan through the vector
	// top-k heap (ORDER BY <distance> LIMIT k, vector.go) instead of a
	// full sort. Set after planning, by the executor — it is an execution
	// strategy, not an access path.
	TopK bool
}

// describe renders the plan for tests and debugging, in the spirit of
// chai's EXPLAIN output: table.Scan(...), index.Scan("name", ...).
func (p *scanPlan) describe() string {
	target := "table.Scan"
	if p.index != nil {
		target = "index.Scan(" + p.index.Name + ")"
	}
	if len(p.ranges) == 0 {
		return target
	}
	parts := make([]string, len(p.ranges))
	for i, r := range p.ranges {
		parts[i] = "[" + r.startHex + "," + r.endHex + ")"
	}
	return target + " " + strings.Join(parts, " ")
}

// isFullScan reports whether the plan degenerated to reading everything.
func (p *scanPlan) isFullScan() bool { return len(p.ranges) == 0 }

// ---------------------------------------------------------------------------
// Predicate analysis
// ---------------------------------------------------------------------------

// colConstraint is one usable predicate on a single column, with constant
// bounds already coerced to the column's type.
type colConstraint struct {
	eq *Value  // col = v
	in []Value // col IN (v1, v2, ...)
	lo *Value  // lower bound (inclusive-widened)
	hi *Value  // upper bound (inclusive-widened)
}

// splitAnd flattens the top-level AND tree of a predicate into conjuncts.
func splitAnd(e Expr, out []Expr) []Expr {
	if b, ok := e.(*Binary); ok && b.Op == "AND" {
		return splitAnd(b.R, splitAnd(b.L, out))
	}
	return append(out, e)
}

// constValue evaluates an expression that must not reference columns or
// parameters. ok=false means "not a constant" and the conjunct is ignored
// by the planner (the residual filter still enforces it).
func constValue(e Expr) (Value, bool) {
	// now() is re-evaluated per row by the residual filter, so a bound
	// computed from it here would disagree with the filter and could
	// exclude rows the filter accepts. Only deterministic expressions
	// qualify as planning constants.
	if !deterministic(e) {
		return Value{}, false
	}
	// Evaluating against a nil row makes any column reference error out,
	// which is exactly the signal we want.
	v, err := evalExpr(e, nil)
	if err != nil {
		return Value{}, false
	}
	return v, true
}

// deterministic reports whether an expression always evaluates to the same
// value — the precondition for using it as a plan bound. Only now() is
// non-deterministic in the v1 dialect, but unknown functions are treated
// as volatile too, out of caution.
func deterministic(e Expr) bool {
	switch x := e.(type) {
	case nil:
		return true
	case *Literal, *ColumnRef, *Param:
		return true
	case *Unary:
		return deterministic(x.X)
	case *Binary:
		return deterministic(x.L) && deterministic(x.R)
	case *Between:
		return deterministic(x.X) && deterministic(x.Lo) && deterministic(x.Hi)
	case *InList:
		if !deterministic(x.X) {
			return false
		}
		for _, item := range x.List {
			if !deterministic(item) {
				return false
			}
		}
		return true
	case *IsNull:
		return deterministic(x.X)
	case *Cast:
		return deterministic(x.X)
	case *FuncCall:
		switch x.Name {
		case "lower", "upper", "trim", "ltrim", "rtrim", "length", "typeof",
			"abs", "coalesce",
			"vector_dims", "l2_distance", "inner_product", "cosine_distance", "l2_norm":
			for _, a := range x.Args {
				if !deterministic(a) {
					return false
				}
			}
			return true
		}
		return false // now() and anything unknown
	}
	return false
}

// coerceForKey converts a constant to the column's declared type for key
// encoding. The plan compares *encoded* stored values against the encoded
// constant, while the residual filter compares stored values against the
// *original* constant with the dialect's coercion rules — so the coercion
// is only safe when, for every possible stored value w of the column's
// type, Compare(w, v) and Compare(w, coerced) agree. That fails in two
// ways this function guards against:
//
//   - Coercions into a type whose cross-type equality is many-to-one.
//     A TEXT column compared to the number 5 matches '5', '05', ' 5' ...
//     (text casts to number), but only the exact text '5' shares the key
//     encoding. The same applies to TEXT vs boolean ('t','true','yes'...).
//     So a coercion is allowed only when comparing against the original
//     constant already goes through the column's own type: numbers and
//     numeric text for numeric columns, text for timestamp/bytea/bool
//     columns (those casts are deterministic single-valued), and only the
//     identical type for text columns.
//
//   - Integer columns hit by a value that passed through float64. Above
//     2^53 many integers collapse onto one float, so the residual filter
//     (which compares via float) accepts integers the single-point key
//     range misses. Such values are only accepted well inside the exact
//     range.
//
// It also rejects lossy coercions outright (e.g. '1.5' to integer 1) by
// requiring the coerced value to still compare equal to the original.
func coerceForKey(v Value, col column) (Value, bool) {
	if col.Type == TypeAny || v.IsNull() {
		return Value{}, false
	}
	// Vector columns never reach the key encoding: they cannot be PK or
	// indexed (exec.go rejects both), so no plan range can use them.
	if col.Type == TypeVector || v.T == TypeVector {
		return Value{}, false
	}
	// Which source types may be pushed into this column's key space.
	switch col.Type {
	case TypeInt, TypeDouble:
		if !v.isNumeric() && v.T != TypeText {
			return Value{}, false
		}
	case TypeText:
		if v.T != TypeText {
			return Value{}, false
		}
	case TypeBool, TypeTimestamp, TypeBytea:
		if v.T != col.Type && v.T != TypeText {
			return Value{}, false
		}
	default:
		if v.T != col.Type {
			return Value{}, false
		}
	}
	cv, err := CastTo(v, col.Type)
	if err != nil {
		return Value{}, false
	}
	c, err := Compare(cv, v)
	if err != nil || c != 0 {
		return Value{}, false
	}
	// The float64 collapse guard for integer keys: only trust conversions
	// that never rounded. Text that parses as a plain integer is exact;
	// everything else that reached an int through a float must be small
	// enough that float64 represents it (and its neighbors) exactly.
	if col.Type == TypeInt && v.T != TypeInt {
		exactText := false
		if v.T == TypeText {
			_, perr := strconv.ParseInt(strings.TrimSpace(v.S), 10, 64)
			exactText = perr == nil
		}
		const maxExactFloatInt = int64(1) << 53
		if !exactText && (cv.I >= maxExactFloatInt || cv.I <= -maxExactFloatInt) {
			return Value{}, false
		}
	}
	return cv, true
}

// analyzeWhere extracts per-column constraints from the WHERE clause's
// top-level conjuncts. Only "col <op> constant" shapes qualify.
func analyzeWhere(sc *tableSchema, where Expr) map[string]*colConstraint {
	cons := map[string]*colConstraint{}
	if where == nil {
		return cons
	}
	get := func(name string) *colConstraint {
		c := cons[name]
		if c == nil {
			c = &colConstraint{}
			cons[name] = c
		}
		return c
	}
	for _, conj := range splitAnd(where, nil) {
		switch x := conj.(type) {
		case *Binary:
			op := x.Op
			colRef, valEx := x.L, x.R
			if _, ok := colRef.(*ColumnRef); !ok {
				// Maybe the column is on the right: flip the comparison.
				colRef, valEx = x.R, x.L
				switch op {
				case "<":
					op = ">"
				case "<=":
					op = ">="
				case ">":
					op = "<"
				case ">=":
					op = "<="
				}
			}
			cr, ok := colRef.(*ColumnRef)
			if !ok {
				continue
			}
			col, ok := sc.col(cr.Column)
			if !ok {
				continue
			}
			v, ok := constValue(valEx)
			if !ok {
				continue
			}
			cv, ok := coerceForKey(v, col)
			if !ok {
				continue
			}
			c := get(cr.Column)
			switch op {
			case "=":
				c.eq = &cv
			case ">", ">=":
				// Widened to inclusive; the residual filter restores strictness.
				if c.lo == nil || mustCompare(cv, *c.lo) > 0 {
					c.lo = &cv
				}
			case "<", "<=":
				if c.hi == nil || mustCompare(cv, *c.hi) < 0 {
					c.hi = &cv
				}
			}
		case *Between:
			if x.Not {
				continue
			}
			cr, ok := x.X.(*ColumnRef)
			if !ok {
				continue
			}
			col, ok := sc.col(cr.Column)
			if !ok {
				continue
			}
			lo, ok1 := constValue(x.Lo)
			hi, ok2 := constValue(x.Hi)
			if !ok1 || !ok2 {
				continue
			}
			clo, ok1 := coerceForKey(lo, col)
			chi, ok2 := coerceForKey(hi, col)
			if !ok1 || !ok2 {
				continue
			}
			c := get(cr.Column)
			if c.lo == nil || mustCompare(clo, *c.lo) > 0 {
				c.lo = &clo
			}
			if c.hi == nil || mustCompare(chi, *c.hi) < 0 {
				c.hi = &chi
			}
		case *InList:
			if x.Not {
				continue
			}
			cr, ok := x.X.(*ColumnRef)
			if !ok {
				continue
			}
			col, ok := sc.col(cr.Column)
			if !ok {
				continue
			}
			vals := make([]Value, 0, len(x.List))
			usable := true
			for _, item := range x.List {
				v, ok := constValue(item)
				if !ok {
					usable = false
					break
				}
				if v.IsNull() {
					continue // NULL in the list never matches by equality
				}
				cv, ok := coerceForKey(v, col)
				if !ok {
					usable = false
					break
				}
				vals = append(vals, cv)
			}
			if !usable || len(vals) == 0 {
				continue
			}
			c := get(cr.Column)
			if c.in == nil {
				c.in = vals
			}
		}
	}
	return cons
}

// mustCompare compares two same-typed coerced values; coerceForKey already
// guaranteed comparability, so an error cannot occur in practice.
func mustCompare(a, b Value) int {
	c, err := Compare(a, b)
	if err != nil {
		return 0
	}
	return c
}

// ---------------------------------------------------------------------------
// Plan selection
// ---------------------------------------------------------------------------

// candidateScore rates how much of a key's leading columns a constraint
// set can consume: eq counts fully-pinned columns, extra is 1 when a
// range or IN also applies to the next column.
func candidateScore(cols []string, cons map[string]*colConstraint) (eq int, extra int) {
	for _, cn := range cols {
		c := cons[cn]
		if c != nil && c.eq != nil {
			eq++
			continue
		}
		if c != nil && (c.in != nil || c.lo != nil || c.hi != nil) {
			extra = 1
		}
		break
	}
	return eq, extra
}

// typedKey reports whether every key column has a declared type — the
// precondition for both range pushdown and order pushdown (typeless
// columns store mixed types whose encoded order differs from SQL order).
func typedKey(sc *tableSchema, cols []string) bool {
	for _, cn := range cols {
		col, ok := sc.col(cn)
		if !ok || col.Type == TypeAny {
			return false
		}
	}
	return true
}

// planScan picks the best access path for a WHERE clause against a table.
// It always returns a usable plan; the zero-information case is the full
// table scan.
func (e *Engine) planScan(sc *tableSchema, where Expr) *scanPlan {
	cons := analyzeWhere(sc, where)

	// The fallback: scan the whole table. Its natural order is the PK.
	best := &scanPlan{}
	if sc.hasPK() && typedKey(sc, sc.PK) {
		best.keyCols = sc.PK
	}
	bestEq, bestExtra := -1, 0

	consider := func(idx *indexDef, cols []string) {
		if !typedKey(sc, cols) {
			return
		}
		eq, extra := candidateScore(cols, cons)
		if eq+extra == 0 {
			return
		}
		// Prefer more pinned columns; on a tie prefer the PK (idx==nil),
		// which avoids the per-row fetch an index scan needs. The earlier
		// candidate wins ties because the PK is considered first.
		if eq*2+extra <= bestEq*2+bestExtra {
			return
		}
		ranges, fixed, ok := buildRanges(cols, cons)
		if !ok {
			return
		}
		best = &scanPlan{index: idx, ranges: ranges, keyCols: cols, fixedPrefix: fixed}
		bestEq, bestExtra = eq, extra
	}

	if sc.hasPK() {
		consider(nil, sc.PK)
	}
	for i := range sc.Indexes {
		idx := &sc.Indexes[i]
		consider(idx, idx.Columns)
	}
	return best
}

// buildRanges turns the consumed constraints on a key's leading columns
// into concrete key ranges. fixed is the number of leading columns pinned
// to one equality value.
func buildRanges(cols []string, cons map[string]*colConstraint) ([]keyRange, int, bool) {
	var eqVals []Value
	i := 0
	for ; i < len(cols); i++ {
		c := cons[cols[i]]
		if c == nil || c.eq == nil {
			break
		}
		eqVals = append(eqVals, *c.eq)
	}
	prefixHex, err := encodeKey(eqVals...)
	if err != nil {
		return nil, 0, false
	}

	// After the equality prefix, at most one more column can contribute:
	// an IN fans out into one point range per value; lo/hi bound one range.
	var c *colConstraint
	if i < len(cols) {
		c = cons[cols[i]]
	}
	switch {
	case c != nil && c.in != nil:
		// Sort IN values by their encoded form so the concatenated ranges
		// stay in ascending key order.
		encs := make([]string, 0, len(c.in))
		for _, v := range c.in {
			h, err := encodeKey(v)
			if err != nil {
				return nil, 0, false
			}
			encs = append(encs, h)
		}
		sort.Strings(encs)
		var ranges []keyRange
		for j, h := range encs {
			if j > 0 && h == encs[j-1] {
				continue // duplicate IN values collapse into one range
			}
			ranges = append(ranges, keyRange{
				startHex: prefixHex + h,
				endHex:   prefixEnd(prefixHex + h),
			})
		}
		return ranges, len(eqVals), true
	case c != nil && (c.lo != nil || c.hi != nil):
		r := keyRange{startHex: prefixHex}
		if c.lo != nil {
			h, err := encodeKey(*c.lo)
			if err != nil {
				return nil, 0, false
			}
			r.startHex = prefixHex + h
		}
		if c.hi != nil {
			h, err := encodeKey(*c.hi)
			if err != nil {
				return nil, 0, false
			}
			// Inclusive upper bound: everything with this value-encoding
			// prefix stays in range (composite keys continue after it).
			r.endHex = prefixEnd(prefixHex + h)
		} else {
			r.endHex = prefixEnd(prefixHex)
		}
		return []keyRange{r}, len(eqVals), true
	case len(eqVals) > 0:
		return []keyRange{{startHex: prefixHex, endHex: prefixEnd(prefixHex)}}, len(eqVals), true
	}
	return nil, 0, false
}

// ---------------------------------------------------------------------------
// Plan execution
// ---------------------------------------------------------------------------

// scanFor is the single entry point statements use to read a table for a
// WHERE clause: it plans, records the plan for tests/observability, and
// runs it. With the planner disabled (the test baseline) it returns a nil
// plan and reads everything, which also switches off order pushdown.
func (e *Engine) scanFor(ctx context.Context, sc *tableSchema, where Expr) ([]storedRow, *scanPlan, error) {
	if e.noPlanner {
		rows, err := e.scan(ctx, sc)
		return rows, nil, err
	}
	plan := e.planScan(sc, where)
	e.lastPlan = plan
	rows, err := e.runPlan(ctx, sc, plan)
	return rows, plan, err
}

// runPlan reads the rows a plan selects, in ascending key order. The
// caller still applies its full WHERE predicate to every returned row.
func (e *Engine) runPlan(ctx context.Context, sc *tableSchema, plan *scanPlan) ([]storedRow, error) {
	if plan == nil || plan.isFullScan() {
		return e.scan(ctx, sc)
	}
	if plan.index == nil {
		return e.scanTableRanges(ctx, sc, plan.ranges)
	}
	return e.scanIndexRanges(ctx, sc, plan.index, plan.ranges)
}

// scanTableRanges reads row ranges directly from the table's key space.
func (e *Engine) scanTableRanges(ctx context.Context, sc *tableSchema, ranges []keyRange) ([]storedRow, error) {
	prefix := tablePrefix(e.db, sc.Name)
	var out []storedRow
	for _, r := range ranges {
		err := e.listRange(ctx, prefix, r, func(key string, value []byte) error {
			cols, err := decodeRow(value)
			if err != nil {
				return err
			}
			out = append(out, storedRow{data: cols, pkhex: strings.TrimPrefix(key, prefix)})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanIndexRanges reads index entries in range, then fetches each row by
// its primary key. Index entry keys are <keyhex>/<pkhex> and their value
// is the pkhex, so no decoding beyond a prefix strip is needed.
func (e *Engine) scanIndexRanges(ctx context.Context, sc *tableSchema, idx *indexDef, ranges []keyRange) ([]storedRow, error) {
	prefix := oneIndexPrefix(e.db, sc.Name, idx.Name)
	var out []storedRow
	for _, r := range ranges {
		err := e.listRange(ctx, prefix, r, func(key string, value []byte) error {
			pkhex := string(value)
			raw, found, err := e.c.Get(ctx, rowKey(e.db, sc.Name, pkhex))
			if err != nil {
				return err
			}
			if !found {
				return nil // index entry racing a delete; the row is gone
			}
			cols, err := decodeRow(raw)
			if err != nil {
				return err
			}
			out = append(out, storedRow{data: cols, pkhex: pkhex})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// listRange pages through prefix+[start,end) with the standard prefetch
// batch size, invoking fn for each entry in key order.
//
// The store's List API is (prefix, cursor) with an exclusive cursor, so an
// inclusive start needs a cursor just below start. Decrementing the last
// byte and appending 0xFF produces such a string, and because every key
// under the prefix is hex text (plus '/'), no real key can fall between
// that cursor and start.
func (e *Engine) listRange(ctx context.Context, prefix string, r keyRange, fn func(key string, value []byte) error) error {
	cursor := ""
	if r.startHex != "" {
		cursor = decrementKey(prefix + r.startHex)
	}
	end := ""
	if r.endHex != "" {
		end = prefix + r.endHex
	}
	for {
		entries, next, err := e.c.List(ctx, prefix, cursor, scanBatch)
		if err != nil {
			return err
		}
		for _, ent := range entries {
			if end != "" && ent.Key >= end {
				return nil
			}
			if err := fn(ent.Key, ent.Value); err != nil {
				return err
			}
		}
		if next == "" || len(entries) == 0 {
			return nil
		}
		cursor = next
	}
}

// decrementKey returns the largest cursor value that still admits key
// itself: last byte minus one, padded with 0xFF.
func decrementKey(key string) string {
	b := []byte(key)
	b[len(b)-1]--
	return string(b) + "\xff"
}

// ---------------------------------------------------------------------------
// Order pushdown
// ---------------------------------------------------------------------------

// satisfiesOrder reports whether rows produced by the plan are already in
// the order the ORDER BY keys request, making the in-memory sort
// unnecessary. Only ascending keys over plain column references qualify —
// the scan reads keys ascending and the key encoding sorts like SQL.
func (p *scanPlan) satisfiesOrder(keys []OrderKey) bool {
	if len(keys) == 0 || len(p.keyCols) == 0 {
		return false
	}
	// The ORDER BY columns must line up with the scan's key columns,
	// optionally skipping key columns pinned by equality (their value is
	// constant across the result, so they cannot affect order).
	for skip := 0; skip <= p.fixedPrefix; skip++ {
		if orderMatchesCols(keys, p.keyCols[skip:]) {
			return true
		}
	}
	return false
}

// orderMatchesCols checks that keys is an ascending column-name prefix of cols.
func orderMatchesCols(keys []OrderKey, cols []string) bool {
	if len(keys) > len(cols) {
		return false
	}
	for i, k := range keys {
		if k.Desc {
			return false
		}
		cr, ok := k.Expr.(*ColumnRef)
		if !ok || cr.Column != cols[i] {
			return false
		}
	}
	return true
}
