// aggregate.go implements grouped execution: GROUP BY partitioning and the
// five aggregate functions the dialect ships — COUNT, SUM, AVG, MIN, MAX
// (§13). A projection may mix aggregates with grouping
// columns and scalar arithmetic (e.g. SELECT k, sum(v)*2 ... GROUP BY k);
// each aggregate subexpression is computed over the group, substituted back
// as a literal, and the surrounding scalar expression is then evaluated
// once per group.
package sql

import (
	"fmt"
	"sort"
)

// execGrouped runs the aggregate/GROUP BY path and returns output columns
// and one result row per group.
func (e *Engine) execGrouped(core *SelectCore, sc *tableSchema, rows []row) ([]string, []resultRow, error) {
	// Aggregates need a table to aggregate over; chai rejects a table-less
	// "SELECT MAX(3)" (SELECT/projection_no_table.sql).
	if sc == nil {
		return nil, nil, fmt.Errorf("no table specified")
	}
	cols := make([]string, 0, len(core.Items))
	for _, it := range core.Items {
		if it.Star {
			return nil, nil, fmt.Errorf("cannot use * with aggregation")
		}
		cols = append(cols, labelFor(it))
	}

	// Partition rows into groups keyed by the GROUP BY expressions. With no
	// GROUP BY, all rows form a single group (and a table with zero rows
	// still yields one aggregate row — COUNT(*) → 0).
	type group struct {
		key  string
		rep  row
		rows []row
	}
	var groups []*group
	index := map[string]*group{}
	addTo := func(key string, r row) {
		g, ok := index[key]
		if !ok {
			g = &group{key: key, rep: r}
			index[key] = g
			groups = append(groups, g)
		}
		g.rows = append(g.rows, r)
	}
	if len(core.GroupBy) == 0 {
		g := &group{key: "", rep: row{}}
		g.rows = rows
		groups = append(groups, g)
	} else {
		for _, r := range rows {
			vals := make([]Value, len(core.GroupBy))
			for i, ge := range core.GroupBy {
				v, err := evalExpr(ge, r)
				if err != nil {
					return nil, nil, err
				}
				// Group keys rely on the order-preserving key encoding,
				// which vectors do not have (vector.go).
				if v.T == TypeVector {
					return nil, nil, fmt.Errorf("vector values cannot be used in GROUP BY")
				}
				vals[i] = v
			}
			key := rowKeyString(vals)
			addTo(key, r)
		}
	}

	// Groups come back in group-key order (the key encoding sorts like the
	// values do), matching chai's output for GROUP BY without ORDER BY.
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].key < groups[j].key })

	out := make([]resultRow, 0, len(groups))
	for _, g := range groups {
		vals := make([]Value, len(core.Items))
		for i, it := range core.Items {
			// Replace aggregates in the item with their computed values,
			// then evaluate the resulting scalar expression on the group's
			// representative row (valid for GROUP BY columns and constants).
			sub, err := substituteAggregates(it.Expr, g.rows)
			if err != nil {
				return nil, nil, err
			}
			v, err := evalExpr(sub, g.rep)
			if err != nil {
				return nil, nil, err
			}
			vals[i] = v
		}
		env := row{}
		for i, c := range cols {
			env[c] = vals[i]
		}
		out = append(out, resultRow{out: vals, env: env})
	}
	return cols, out, nil
}

// substituteAggregates returns a copy of e with every aggregate call
// replaced by a literal of its computed value over rows. Non-aggregate
// nodes are returned unchanged (they evaluate against the representative
// row later).
func substituteAggregates(e Expr, rows []row) (Expr, error) {
	switch x := e.(type) {
	case *FuncCall:
		if isAggregateName(x.Name) {
			v, err := computeAggregate(x, rows)
			if err != nil {
				return nil, err
			}
			return &Literal{Val: v, Src: v.FormatText()}, nil
		}
		nc := &FuncCall{Name: x.Name, Star: x.Star}
		for _, a := range x.Args {
			na, err := substituteAggregates(a, rows)
			if err != nil {
				return nil, err
			}
			nc.Args = append(nc.Args, na)
		}
		return nc, nil
	case *Unary:
		nx, err := substituteAggregates(x.X, rows)
		if err != nil {
			return nil, err
		}
		return &Unary{Op: x.Op, X: nx}, nil
	case *Binary:
		l, err := substituteAggregates(x.L, rows)
		if err != nil {
			return nil, err
		}
		r, err := substituteAggregates(x.R, rows)
		if err != nil {
			return nil, err
		}
		return &Binary{Op: x.Op, L: l, R: r}, nil
	case *Cast:
		nx, err := substituteAggregates(x.X, rows)
		if err != nil {
			return nil, err
		}
		return &Cast{X: nx, To: x.To, TypeName: x.TypeName, Dim: x.Dim}, nil
	}
	return e, nil
}

// computeAggregate evaluates one aggregate function over a group's rows.
func computeAggregate(fc *FuncCall, rows []row) (Value, error) {
	switch fc.Name {
	case "count":
		if fc.Star {
			return intV(int64(len(rows))), nil
		}
		if len(fc.Args) != 1 {
			return Value{}, fmt.Errorf("count expects one argument")
		}
		n := int64(0)
		for _, r := range rows {
			v, err := evalExpr(fc.Args[0], r)
			if err != nil {
				return Value{}, err
			}
			if !v.IsNull() {
				n++
			}
		}
		return intV(n), nil
	case "sum", "avg":
		if len(fc.Args) != 1 {
			return Value{}, fmt.Errorf("%s expects one argument", fc.Name)
		}
		var sumI int64
		var sumF float64
		allInt := true
		count := 0
		for _, r := range rows {
			v, err := evalExpr(fc.Args[0], r)
			if err != nil {
				return Value{}, err
			}
			if v.IsNull() {
				continue
			}
			count++
			switch v.T {
			case TypeInt:
				sumI += v.I
				sumF += float64(v.I)
			case TypeDouble:
				allInt = false
				sumF += v.F
			default:
				return Value{}, fmt.Errorf("%s requires numeric input, got %s", fc.Name, v.T)
			}
		}
		if count == 0 {
			return nullV(), nil // SUM/AVG over no rows is NULL
		}
		if fc.Name == "avg" {
			return doubleV(sumF / float64(count)), nil
		}
		if allInt {
			return intV(sumI), nil
		}
		return doubleV(sumF), nil
	case "min", "max":
		if len(fc.Args) != 1 {
			return Value{}, fmt.Errorf("%s expects one argument", fc.Name)
		}
		var best Value
		have := false
		for _, r := range rows {
			v, err := evalExpr(fc.Args[0], r)
			if err != nil {
				return Value{}, err
			}
			if v.IsNull() {
				continue
			}
			if !have {
				best, have = v, true
				continue
			}
			c := sortCompare(v, best)
			if (fc.Name == "min" && c < 0) || (fc.Name == "max" && c > 0) {
				best = v
			}
		}
		if !have {
			return nullV(), nil
		}
		return best, nil
	}
	return Value{}, fmt.Errorf("unknown aggregate %s", fc.Name)
}
