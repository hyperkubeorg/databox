// query.go executes SELECT: table/aggregate scans, projection, DISTINCT,
// UNION [ALL], ORDER BY and LIMIT/OFFSET (§13). Scans use
// the prefetching reader in exec.go; everything above the scan is in-memory
// over the rows the scan (or the join fold, join.go) returns, the shape
// chai's own executor takes.
package sql

import (
	"context"
	"fmt"
	"sort"
)

// resultRow is one row flowing through the query pipeline: out holds the
// positional projected values (in the column order), env is the environment
// ORDER BY and DISTINCT evaluate against (source columns plus output labels).
type resultRow struct {
	out []Value
	env row
}

// execSelect runs a full query and formats it as text rows for the wire.
func (e *Engine) execSelect(ctx context.Context, s *Select) (ExecResult, error) {
	cols, rows, err := e.evalSelect(ctx, s, nil)
	if err != nil {
		return ExecResult{}, err
	}
	out := ExecResult{Tag: fmt.Sprintf("SELECT %d", len(rows)), Columns: cols, typed: rows}
	for _, r := range rows {
		textRow := make([]*string, len(r))
		for i, v := range r {
			textRow[i] = valueText(v)
		}
		out.Rows = append(out.Rows, textRow)
	}
	return out, nil
}

// evalSelect produces the final column list and value rows for a query,
// after UNION combination, ORDER BY, and LIMIT/OFFSET. outer is the
// enclosing scope for LATERAL subquery bodies (nil at top level).
func (e *Engine) evalSelect(ctx context.Context, s *Select, outer row) ([]string, [][]Value, error) {
	// Expression-position subqueries materialize once per execution
	// (subquery.go) before anything below sees them.
	if err := e.resolveSelectExprs(ctx, s); err != nil {
		return nil, nil, err
	}
	// Vector KNN fast path: "ORDER BY <distance> LIMIT k" streams the scan
	// through a bounded heap instead of sorting everything (vector.go). An
	// executor shortcut only — results are identical to the sort below.
	// Joined, qualified-reference, or lateral-scoped queries take the
	// general path: the heap scan evaluates against bare single-table rows.
	if s.Core.From == nil && outer == nil && !coreUsesQualifiedRefs(s.Core, s.OrderBy) {
		if n, offset, ok := e.topKShape(s); ok {
			return e.execTopK(ctx, s, n, offset)
		}
	}
	// A single core may satisfy ORDER BY straight from its scan order;
	// union arms concatenate, so their combined result never can.
	pushOrder := s.OrderBy
	if len(s.Unions) > 0 {
		pushOrder = nil
	}
	cols, rrows, sorted, err := e.execCore(ctx, s.Core, pushOrder, outer)
	if err != nil {
		return nil, nil, err
	}
	// UNION arms: combine by position; UNION (without ALL) deduplicates the
	// combined result.
	dedup := false
	for _, arm := range s.Unions {
		_, armRows, _, err := e.execCore(ctx, arm.Core, nil, outer)
		if err != nil {
			return nil, nil, err
		}
		rrows = append(rrows, armRows...)
		if !arm.All {
			dedup = true
		}
	}
	if s.Core.Distinct {
		dedup = true
	}
	if dedup {
		// Dedup returns rows in encoded-value order (chai's DISTINCT and
		// UNION are ordered), which invalidates any scan-provided order.
		rrows, err = distinctRows(rrows)
		if err != nil {
			return nil, nil, err
		}
		sorted = false
	}
	// ORDER BY over the combined result, unless the scan already delivered
	// this exact order.
	if len(s.OrderBy) > 0 && !sorted {
		if err := e.orderResult(rrows, s.OrderBy, cols); err != nil {
			return nil, nil, err
		}
	}
	// OFFSET then LIMIT.
	offset, limit, err := e.limitOffset(s)
	if err != nil {
		return nil, nil, err
	}
	if offset > 0 {
		if offset >= len(rrows) {
			rrows = nil
		} else {
			rrows = rrows[offset:]
		}
	}
	if limit >= 0 && limit < len(rrows) {
		rrows = rrows[:limit]
	}
	out := make([][]Value, len(rrows))
	for i, r := range rrows {
		out[i] = r.out
	}
	return cols, out, nil
}

// execCore executes one SELECT ... FROM ... WHERE ... [GROUP BY] block,
// returning the output columns and projected rows (unlimited). order is
// the query's ORDER BY when this core is the only source; the returned
// sorted flag reports that the scan already delivered that order and the
// in-memory sort may be skipped.
func (e *Engine) execCore(ctx context.Context, core *SelectCore, order []OrderKey, outer row) ([]string, []resultRow, bool, error) {
	// Gather source rows: a real table scan, or a single empty row for a
	// table-less "SELECT 1".
	var src []row
	var sc *tableSchema
	sorted := false
	if core.From != nil {
		// Source-tree path (join.go): joins, derived tables, LATERAL.
		// sc becomes the synthetic qualified-column schema for *.
		var orderExprs []Expr
		for _, k := range order {
			orderExprs = append(orderExprs, k.Expr)
		}
		var err error
		sc, src, err = e.execJoinRows(ctx, core, orderExprs, outer)
		if err != nil {
			return nil, nil, false, err
		}
	} else if core.Table != "" {
		var err error
		sc, err = e.loadSchema(ctx, core.Table)
		if err != nil {
			return nil, nil, false, err
		}
		stored, plan, err := e.scanFor(ctx, sc, core.Where)
		if err != nil {
			return nil, nil, false, err
		}
		for _, sr := range stored {
			src = append(src, sr.data)
		}
		// Qualified references over a single table ("FROM t x WHERE
		// x.a = 1") need qualified keys in the rows; skip the copy for
		// the common unqualified case.
		if qual := coreQual(core); coreUsesQualifiedRefs(core, order) {
			for i := range src {
				src[i] = withQualKeys(src[i], qual, sc)
			}
		}
		// Order pushdown: the scan reads keys ascending, so when the ORDER
		// BY keys line up with the scan's key columns the rows are already
		// sorted. Grouping re-buckets rows and loses that order.
		if plan != nil && !core.grouped() && plan.satisfiesOrder(order) &&
			orderKeysBindToSource(order, core) {
			sorted = true
		}
	} else if outer != nil {
		// A table-less SELECT inside a LATERAL body evaluates against the
		// outer scope ("SELECT o.amount * 2 AS twice").
		src = []row{outer}
	} else {
		// One nil row: expressions evaluate once, and a column reference
		// errors with "no table specified" like chai's table-less SELECT.
		src = []row{nil}
	}
	// The enclosing scope backs any name the local sources don't bind:
	// overlay each source row onto the outer row (local wins).
	if outer != nil && (core.From != nil || core.Table != "") {
		for i, r := range src {
			m := make(row, len(outer)+len(r))
			for k, v := range outer {
				m[k] = v
			}
			for k, v := range r {
				m[k] = v
			}
			src[i] = m
		}
	}
	// WHERE filter.
	filtered := make([]row, 0, len(src))
	for _, r := range src {
		ok, err := predicateTrue(core.Where, r)
		if err != nil {
			return nil, nil, false, err
		}
		if ok {
			filtered = append(filtered, r)
		}
	}
	// Aggregate/grouped path when any projection is an aggregate or GROUP BY
	// is present.
	if core.grouped() {
		cols, rows, err := e.execGrouped(core, sc, filtered)
		return cols, rows, false, err
	}
	// Plain projection. "SELECT *" without FROM has nothing to expand.
	if err := checkStarHasTable(core, sc); err != nil {
		return nil, nil, false, err
	}
	cols := e.projectionColumns(core, sc)
	rows := make([]resultRow, 0, len(filtered))
	for _, r := range filtered {
		vals, err := e.projectRow(core, sc, r)
		if err != nil {
			return nil, nil, false, err
		}
		env := mergeEnv(r, cols, vals)
		rows = append(rows, resultRow{out: vals, env: env})
	}
	return cols, rows, sorted, nil
}

// orderKeysBindToSource guards order pushdown against label shadowing:
// orderResult resolves an ORDER BY name through the output columns first,
// so "SELECT b AS a ... ORDER BY a" sorts on b even though the scan is
// ordered by the table's a. Pushdown is only safe when every output column
// sharing an ORDER BY key's name is that same source column (a plain
// reference or a * expansion).
func orderKeysBindToSource(keys []OrderKey, core *SelectCore) bool {
	for _, k := range keys {
		cr, ok := k.Expr.(*ColumnRef)
		if !ok {
			return false
		}
		for _, it := range core.Items {
			if it.Star {
				continue // * expands to the source columns themselves
			}
			if labelFor(it) != cr.Column {
				continue
			}
			ref, ok := it.Expr.(*ColumnRef)
			if !ok || ref.Column != cr.Column {
				return false
			}
		}
	}
	return true
}

// grouped reports whether the core needs aggregate execution.
func (c *SelectCore) grouped() bool {
	if len(c.GroupBy) > 0 {
		return true
	}
	for _, it := range c.Items {
		if !it.Star && exprHasAggregate(it.Expr) {
			return true
		}
	}
	return false
}

// projectionColumns computes the output column labels for a non-grouped
// projection, expanding * to the table's declared columns.
func (e *Engine) projectionColumns(core *SelectCore, sc *tableSchema) []string {
	var cols []string
	for _, it := range core.Items {
		if it.Star {
			if sc != nil {
				cols = append(cols, orderColumns(sc)...)
			}
			continue
		}
		cols = append(cols, labelFor(it))
	}
	return cols
}

// checkStarHasTable rejects a * projection with no FROM table.
func checkStarHasTable(core *SelectCore, sc *tableSchema) error {
	if sc != nil {
		return nil
	}
	for _, it := range core.Items {
		if it.Star {
			return fmt.Errorf("no table specified")
		}
	}
	return nil
}

// projectRow evaluates a non-grouped projection for one source row.
func (e *Engine) projectRow(core *SelectCore, sc *tableSchema, r row) ([]Value, error) {
	var vals []Value
	for _, it := range core.Items {
		if it.Star {
			if sc != nil {
				for _, cn := range orderColumns(sc) {
					vals = append(vals, r[cn])
				}
			}
			continue
		}
		v, err := evalExpr(it.Expr, r)
		if err != nil {
			return nil, err
		}
		vals = append(vals, v)
	}
	return vals, nil
}

// mergeEnv builds the ORDER BY / DISTINCT environment: the source columns
// plus the output labels (labels win on collision).
func mergeEnv(src row, cols []string, vals []Value) row {
	env := row{}
	for k, v := range src {
		env[k] = v
	}
	for i, c := range cols {
		if i < len(vals) {
			env[c] = vals[i]
		}
	}
	return env
}

// limitOffset evaluates the LIMIT and OFFSET expressions to integers.
// limit == -1 means unbounded.
func (e *Engine) limitOffset(s *Select) (offset, limit int, err error) {
	limit = -1
	if s.Limit != nil {
		v, err := evalExpr(s.Limit, row{})
		if err != nil {
			return 0, 0, err
		}
		n, err := asInt(v)
		if err != nil {
			return 0, 0, fmt.Errorf("LIMIT: %v", err)
		}
		if n < 0 {
			n = 0
		}
		limit = n
	}
	if s.Offset != nil {
		v, err := evalExpr(s.Offset, row{})
		if err != nil {
			return 0, 0, err
		}
		n, err := asInt(v)
		if err != nil {
			return 0, 0, fmt.Errorf("OFFSET: %v", err)
		}
		if n > 0 {
			offset = n
		}
	}
	return offset, limit, nil
}

// asInt coerces a value to a Go int for LIMIT/OFFSET.
func asInt(v Value) (int, error) {
	switch v.T {
	case TypeInt:
		return int(v.I), nil
	case TypeDouble:
		return int(v.F), nil
	}
	return 0, fmt.Errorf("expected an integer, got %s", v.T)
}

// orderResult sorts the result rows by the ORDER BY keys. A key that names
// an output column (or a 1-based position) sorts on that column; otherwise
// the key expression is evaluated against each row's environment.
func (e *Engine) orderResult(rows []resultRow, keys []OrderKey, cols []string) error {
	// Precompute a comparison function shared across the sort.
	colIndex := map[string]int{}
	for i, c := range cols {
		colIndex[c] = i
	}
	valueOf := func(r resultRow, k OrderKey) (Value, error) {
		// ORDER BY <integer>: 1-based output position.
		if lit, ok := k.Expr.(*Literal); ok && lit.Val.T == TypeInt {
			idx := int(lit.Val.I) - 1
			if idx >= 0 && idx < len(r.out) {
				return r.out[idx], nil
			}
		}
		// ORDER BY <output column name>.
		if cr, ok := k.Expr.(*ColumnRef); ok {
			if idx, ok := colIndex[cr.Column]; ok {
				return r.out[idx], nil
			}
		}
		return evalExpr(k.Expr, r.env)
	}
	// When every key is DESC the result is the exact reverse of the
	// ascending sort — including tie order. chai serves DESC by scanning
	// backwards, so ties come back in reversed scan order, and the corpus
	// asserts that (SELECT/order_by_desc_index.sql); a stable descending
	// sort would keep ties in forward order instead.
	allDesc := true
	for _, k := range keys {
		if !k.Desc {
			allDesc = false
			break
		}
	}
	var sortErr error
	sortStable(rows, func(a, b resultRow) bool {
		for _, k := range keys {
			av, err := valueOf(a, k)
			if err != nil {
				sortErr = err
				return false
			}
			bv, err := valueOf(b, k)
			if err != nil {
				sortErr = err
				return false
			}
			c := sortCompare(av, bv)
			if c == 0 {
				continue
			}
			if k.Desc && !allDesc {
				return c > 0
			}
			return c < 0
		}
		return false
	})
	if allDesc {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	return sortErr
}

// distinctRows removes duplicate output rows and returns the survivors in
// encoded-value order — chai's DISTINCT/UNION iterate a sorted dedup tree,
// and the corpus asserts that order (SELECT/distinct.sql, union.sql).
// Vectors are rejected: they have no key encoding, so no dedup order
// (UNION ALL, which never compares rows, still carries them fine).
func distinctRows(rows []resultRow) ([]resultRow, error) {
	type keyed struct {
		key string
		r   resultRow
	}
	seen := map[string]bool{}
	out := make([]keyed, 0, len(rows))
	for _, r := range rows {
		for _, v := range r.out {
			if v.T == TypeVector {
				return nil, fmt.Errorf("vector values cannot be used in DISTINCT or UNION")
			}
		}
		key := rowKeyString(r.out)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, keyed{key: key, r: r})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].key < out[j].key })
	res := make([]resultRow, len(out))
	for i, k := range out {
		res[i] = k.r
	}
	return res, nil
}

// rowKeyString builds a comparable identity string for a value tuple, used
// by DISTINCT/UNION dedup. It uses the order-preserving key encoding so
// equal values collapse regardless of representation.
func rowKeyString(vals []Value) string {
	s, err := encodeKey(vals...)
	if err != nil {
		// Values that cannot be key-encoded (shouldn't happen for result
		// tuples) fall back to their text form.
		var b []byte
		for _, v := range vals {
			b = append(b, []byte(v.T.String()+":"+v.FormatText()+"\x00")...)
		}
		return string(b)
	}
	return s
}

// --- projection helpers for RETURNING ----------------------------------------

// returningColumns computes the labels for a RETURNING clause.
func returningColumns(items []SelectItem, sc *tableSchema) []string {
	var cols []string
	for _, it := range items {
		if it.Star {
			cols = append(cols, orderColumns(sc)...)
			continue
		}
		cols = append(cols, labelFor(it))
	}
	return cols
}

// projectValues evaluates a RETURNING projection for one row.
func projectValues(items []SelectItem, r row, sc *tableSchema) ([]Value, error) {
	var out []Value
	for _, it := range items {
		if it.Star {
			for _, cn := range orderColumns(sc) {
				out = append(out, r[cn])
			}
			continue
		}
		v, err := evalExpr(it.Expr, r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// textRowOf renders one value row as wire text (nil pointer for NULL).
func textRowOf(vals []Value) []*string {
	out := make([]*string, len(vals))
	for i, v := range vals {
		out[i] = valueText(v)
	}
	return out
}

// valueText renders a value for the wire: nil pointer for NULL, otherwise
// the dialect's text representation.
func valueText(v Value) *string {
	if v.IsNull() {
		return nil
	}
	s := v.FormatText()
	return &s
}

// sortStable is a tiny insertion-free stable sort wrapper so query.go does
// not import sort twice with different signatures; it defers to the
// standard library.
func sortStable(rows []resultRow, less func(a, b resultRow) bool) {
	stableSort(rows, less)
}

// stableSort is a stable merge sort over resultRows (kept local so the less
// function can close over shared error state).
func stableSort(rows []resultRow, less func(a, b resultRow) bool) {
	n := len(rows)
	if n < 2 {
		return
	}
	buf := make([]resultRow, n)
	for width := 1; width < n; width *= 2 {
		for i := 0; i < n; i += 2 * width {
			mid := min(i+width, n)
			end := min(i+2*width, n)
			merge(rows, buf, i, mid, end, less)
		}
		copy(rows, buf[:n])
	}
}

// merge is the stable merge step used by stableSort.
func merge(rows, buf []resultRow, lo, mid, hi int, less func(a, b resultRow) bool) {
	i, j, k := lo, mid, lo
	for i < mid && j < hi {
		if less(rows[j], rows[i]) {
			buf[k] = rows[j]
			j++
		} else {
			buf[k] = rows[i]
			i++
		}
		k++
	}
	for i < mid {
		buf[k] = rows[i]
		i, k = i+1, k+1
	}
	for j < hi {
		buf[k] = rows[j]
		j, k = j+1, k+1
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
