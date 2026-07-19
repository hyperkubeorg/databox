// subquery.go resolves subqueries in expression position — scalar
// "(SELECT ...)", "EXISTS (SELECT ...)", and "x IN (SELECT ...)" — by
// executing each once per statement and substituting the result as a
// literal. Only NON-correlated subqueries are supported in expressions:
// one that references the enclosing query's columns fails with a hint,
// because per-row re-evaluation has a first-class spelling here —
// [LEFT] JOIN LATERAL (join.go). FROM-position subqueries never pass
// through this file; the source tree executes them natively.
package sql

import (
	"context"
	"fmt"
	"strings"
)

// resolveSelectExprs resolves every expression-position subquery in a
// SELECT's own clauses (not inside FROM subqueries — those resolve when
// they execute).
func (e *Engine) resolveSelectExprs(ctx context.Context, s *Select) error {
	cores := []*SelectCore{s.Core}
	for _, arm := range s.Unions {
		cores = append(cores, arm.Core)
	}
	for _, core := range cores {
		for i := range core.Items {
			if core.Items[i].Star {
				continue
			}
			ex, err := e.resolveExpr(ctx, core.Items[i].Expr)
			if err != nil {
				return err
			}
			core.Items[i].Expr = ex
		}
		if core.Where != nil {
			ex, err := e.resolveExpr(ctx, core.Where)
			if err != nil {
				return err
			}
			core.Where = ex
		}
		for i := range core.GroupBy {
			ex, err := e.resolveExpr(ctx, core.GroupBy[i])
			if err != nil {
				return err
			}
			core.GroupBy[i] = ex
		}
	}
	for i := range s.OrderBy {
		ex, err := e.resolveExpr(ctx, s.OrderBy[i].Expr)
		if err != nil {
			return err
		}
		s.OrderBy[i].Expr = ex
	}
	if s.Limit != nil {
		ex, err := e.resolveExpr(ctx, s.Limit)
		if err != nil {
			return err
		}
		s.Limit = ex
	}
	if s.Offset != nil {
		ex, err := e.resolveExpr(ctx, s.Offset)
		if err != nil {
			return err
		}
		s.Offset = ex
	}
	return nil
}

// resolveExpr returns ex with every Subquery / IN-subquery node replaced
// by its materialized literal form.
func (e *Engine) resolveExpr(ctx context.Context, ex Expr) (Expr, error) {
	switch x := ex.(type) {
	case *Subquery:
		return e.materializeSubquery(ctx, x)
	case *InList:
		nx, err := e.resolveExpr(ctx, x.X)
		if err != nil {
			return nil, err
		}
		out := &InList{X: nx, Not: x.Not}
		if x.Sub != nil {
			cols, vals, err := e.runSubquery(ctx, x.Sub)
			if err != nil {
				return nil, err
			}
			if len(cols) != 1 {
				return nil, fmt.Errorf("IN subquery must return one column, got %d", len(cols))
			}
			for _, vr := range vals {
				out.List = append(out.List, &Literal{Val: vr[0]})
			}
			// IN over an empty set is FALSE, not an empty-syntax error:
			// keep one impossible arm? No — evalInList of an empty list
			// handles it; leave List empty.
			return out, nil
		}
		for _, item := range x.List {
			ni, err := e.resolveExpr(ctx, item)
			if err != nil {
				return nil, err
			}
			out.List = append(out.List, ni)
		}
		return out, nil
	case *Unary:
		nx, err := e.resolveExpr(ctx, x.X)
		if err != nil {
			return nil, err
		}
		return &Unary{Op: x.Op, X: nx}, nil
	case *Binary:
		l, err := e.resolveExpr(ctx, x.L)
		if err != nil {
			return nil, err
		}
		r, err := e.resolveExpr(ctx, x.R)
		if err != nil {
			return nil, err
		}
		return &Binary{Op: x.Op, L: l, R: r}, nil
	case *Between:
		nx, err := e.resolveExpr(ctx, x.X)
		if err != nil {
			return nil, err
		}
		lo, err := e.resolveExpr(ctx, x.Lo)
		if err != nil {
			return nil, err
		}
		hi, err := e.resolveExpr(ctx, x.Hi)
		if err != nil {
			return nil, err
		}
		return &Between{X: nx, Lo: lo, Hi: hi, Not: x.Not}, nil
	case *IsNull:
		nx, err := e.resolveExpr(ctx, x.X)
		if err != nil {
			return nil, err
		}
		return &IsNull{X: nx, Not: x.Not}, nil
	case *Cast:
		nx, err := e.resolveExpr(ctx, x.X)
		if err != nil {
			return nil, err
		}
		return &Cast{X: nx, To: x.To, TypeName: x.TypeName, Dim: x.Dim}, nil
	case *FuncCall:
		out := &FuncCall{Name: x.Name, Star: x.Star}
		for _, a := range x.Args {
			na, err := e.resolveExpr(ctx, a)
			if err != nil {
				return nil, err
			}
			out.Args = append(out.Args, na)
		}
		return out, nil
	}
	return ex, nil
}

// materializeSubquery executes one expression subquery and returns its
// literal value: EXISTS → boolean; scalar → the single value, NULL when
// no row, an error past one row or one column.
func (e *Engine) materializeSubquery(ctx context.Context, sq *Subquery) (Expr, error) {
	cols, vals, err := e.runSubquery(ctx, sq.Select)
	if err != nil {
		return nil, err
	}
	if sq.Exists {
		return &Literal{Val: boolV(len(vals) > 0)}, nil
	}
	if len(cols) != 1 {
		return nil, fmt.Errorf("scalar subquery must return one column, got %d", len(cols))
	}
	switch len(vals) {
	case 0:
		return &Literal{Val: nullV()}, nil
	case 1:
		return &Literal{Val: vals[0][0]}, nil
	}
	return nil, fmt.Errorf("scalar subquery returned more than one row (%d)", len(vals))
}

// runSubquery evaluates an expression subquery, translating unresolved
// column errors into the correlated-subquery hint.
func (e *Engine) runSubquery(ctx context.Context, s *Select) ([]string, [][]Value, error) {
	cols, vals, err := e.evalSelect(ctx, s, nil)
	if err != nil && (strings.Contains(err.Error(), "no such column") ||
		strings.Contains(err.Error(), "unknown table") ||
		strings.Contains(err.Error(), "no table specified")) {
		return nil, nil, fmt.Errorf("%v (correlated subqueries are not supported in expressions — use [LEFT] JOIN LATERAL)", err)
	}
	return cols, vals, err
}

// exprHasSubquery reports whether an expression contains any subquery
// node — DDL validation uses it to keep subqueries out of stored
// DEFAULT/CHECK expressions.
func exprHasSubquery(exprs ...Expr) bool {
	found := false
	for _, ex := range exprs {
		walkExpr(ex, func(n Expr) {
			switch t := n.(type) {
			case *Subquery:
				found = true
			case *InList:
				if t.Sub != nil {
					found = true
				}
			}
		})
	}
	return found
}
