// join.go is the JOIN extension: [INNER] JOIN, LEFT/RIGHT/FULL [OUTER]
// JOIN, CROSS JOIN, comma joins ("FROM a, b"), ON predicates, USING
// column lists, NATURAL joins, parenthesized join groups
// ("FROM (a JOIN b ON …) JOIN c ON …"), derived tables
// ("FROM (SELECT …) AS t"), and LATERAL subqueries. chai's dialect has
// none of this; the extension keeps every word unreserved — JOIN, LEFT,
// RIGHT, FULL, INNER, CROSS, OUTER, NATURAL, USING and LATERAL lex as
// ordinary identifiers and only mean anything in FROM position, so
// columns with those names keep working (the one cost: a bare FROM alias
// cannot be one of those words — use AS).
//
// Execution: the FROM clause is a tree. Base tables scan at the
// statement's snapshot, derived tables evaluate once, LATERAL subqueries
// re-evaluate per row of the sources to their left, and each join node
// combines its sides in memory — a hash join when the join keys are
// known (USING/NATURAL, or an ON clause that is a pure conjunction of
// cross-side equalities), a nested loop otherwise. Rows carry qualified
// keys ("alias.column") plus bare keys for every column name that
// appears in exactly one source; USING/NATURAL columns keep a bare key
// holding the pg-style coalesced value. Referencing any other ambiguous
// bare name is an error.
package sql

import (
	"context"
	"fmt"
)

// joinWords are the FROM-clause words parseTableRef refuses as bare
// aliases so the join grammar stays unambiguous.
var joinWords = map[string]bool{
	"join": true, "left": true, "right": true, "full": true, "inner": true,
	"cross": true, "outer": true, "natural": true, "using": true, "lateral": true,
}

// parseTableRef reads "table [AS alias | alias]"; a bare alias may not be
// a join word.
func (p *parser) parseTableRef() (table, alias string, err error) {
	if table, err = p.ident(); err != nil {
		return "", "", err
	}
	alias, err = p.parseMaybeAlias()
	return table, alias, err
}

// parseMaybeAlias reads an optional alias (AS name, or a bare non-join
// word).
func (p *parser) parseMaybeAlias() (string, error) {
	if p.acceptKw("AS") {
		return p.ident()
	}
	if t := p.peek(); t.kind == tkIdent && !joinWords[t.s] {
		p.next()
		return t.s, nil
	}
	return "", nil
}

// acceptIdent consumes the identifier if it is next.
func (p *parser) acceptIdent(s string) bool {
	if t := p.peek(); t.kind == tkIdent && t.s == s {
		p.next()
		return true
	}
	return false
}

// parseFromClause parses one FROM item plus its join continuations,
// producing the source tree.
func (p *parser) parseFromClause() (TableExpr, error) {
	left, err := p.parseTablePrimary()
	if err != nil {
		return nil, err
	}
	for {
		if p.acceptOp(",") {
			// Old-style comma join: a CROSS join; WHERE supplies the
			// predicate, as it always has.
			right, err := p.parseTablePrimary()
			if err != nil {
				return nil, err
			}
			left = &JoinExpr{Type: "CROSS", L: left, R: right}
			continue
		}
		natural := p.acceptIdent("natural")
		jt, ok, err := p.parseJoinIntro(natural)
		if err != nil {
			return nil, err
		}
		if !ok {
			return left, nil
		}
		right, err := p.parseTablePrimary()
		if err != nil {
			return nil, err
		}
		j := &JoinExpr{Type: jt, L: left, R: right, Natural: natural}
		if jt != "CROSS" && !natural {
			switch {
			case p.acceptKw("ON"):
				if j.On, err = p.parseExpr(1); err != nil {
					return nil, err
				}
			case p.acceptIdent("using"):
				if err := p.expectOp("("); err != nil {
					return nil, err
				}
				for {
					cn, err := p.ident()
					if err != nil {
						return nil, err
					}
					j.Using = append(j.Using, cn)
					if p.acceptOp(",") {
						continue
					}
					break
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
			default:
				return nil, errAt(p.peek().pos, "expected ON or USING after JOIN")
			}
		}
		left = j
	}
}

// parseTablePrimary parses one FROM item: a base table, "(SELECT …) AS
// alias", a parenthesized join group, or LATERAL (SELECT …) AS alias.
func (p *parser) parseTablePrimary() (TableExpr, error) {
	// LATERAL is only a marker when a subquery follows — a table named
	// "lateral" keeps working.
	lateral := false
	if t := p.peek(); t.kind == tkIdent && t.s == "lateral" && p.peekAheadIsOp(1, "(") && p.peekAheadKw(2, "SELECT") {
		p.next()
		lateral = true
	}
	if p.acceptOp("(") {
		if p.peekKw("SELECT") {
			sub, err := p.parseSelect()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			alias, err := p.parseMaybeAlias()
			if err != nil {
				return nil, err
			}
			return &SubqueryRef{Select: sub, Alias: alias, Lateral: lateral}, nil
		}
		// A parenthesized join group.
		te, err := p.parseFromClause()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		if t := p.peek(); t.kind == tkIdent && !joinWords[t.s] {
			return nil, errAt(t.pos, "aliasing a parenthesized join is not supported")
		}
		return te, nil
	}
	tbl, alias, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	return &TableRef{Table: tbl, Alias: alias}, nil
}

// peekAheadIsOp reports whether the token n places ahead is the operator.
func (p *parser) peekAheadIsOp(n int, op string) bool {
	if p.i+n >= len(p.toks) {
		return false
	}
	t := p.toks[p.i+n]
	return t.kind == tkOp && t.s == op
}

// parseJoinIntro consumes a join introducer ([INNER|LEFT|RIGHT|FULL
// [OUTER]|CROSS] JOIN) and returns its type. After NATURAL an introducer
// is mandatory and CROSS is invalid.
func (p *parser) parseJoinIntro(natural bool) (string, bool, error) {
	t := p.peek()
	if t.kind != tkIdent {
		if natural {
			return "", false, errAt(t.pos, "expected a JOIN after NATURAL, found %q", t.s)
		}
		return "", false, nil
	}
	need := func(kind string) (string, bool, error) {
		if !p.acceptIdent("join") {
			return "", false, errAt(p.peek().pos, "expected JOIN after %s, found %q", kind, p.peek().s)
		}
		return kind, true, nil
	}
	switch t.s {
	case "join":
		p.next()
		return "INNER", true, nil
	case "inner":
		p.next()
		return need("INNER")
	case "left", "right", "full":
		p.next()
		p.acceptIdent("outer")
		switch t.s {
		case "left":
			return need("LEFT")
		case "right":
			return need("RIGHT")
		}
		return need("FULL")
	case "cross":
		if natural {
			return "", false, errAt(t.pos, "NATURAL CROSS JOIN is not a thing")
		}
		p.next()
		return need("CROSS")
	}
	if natural {
		return "", false, errAt(t.pos, "expected a JOIN after NATURAL, found %q", t.s)
	}
	return "", false, nil
}

// --- execution ---------------------------------------------------------------

// joinTable is one FROM-clause leaf with its scan (or derived rows)
// materialized. A LATERAL leaf has rows == nil; its rows come per left
// row during the fold.
type joinTable struct {
	qual    string // alias, or the table name
	sc      *tableSchema
	rows    []row
	lateral *SubqueryRef
}

// joinSource is the result of evaluating a subtree: its leaves, the
// USING columns coalesced inside it, and its joined rows.
type joinSource struct {
	tables    []joinTable
	usingCols []string
	rows      []row
}

// resolvedJoin is a join node's statically resolved USING keys.
type resolvedJoin struct {
	usingCols []string
	leftKeys  []string
	rightKeys []string
}

// execJoinRows materializes the joined row set for a core with a source
// tree, returning a synthetic schema whose column names are the *
// expansion (USING columns once, bare; everything else qualified).
func (e *Engine) execJoinRows(ctx context.Context, core *SelectCore, extraExprs []Expr, outer row) (*tableSchema, []row, error) {
	if lateralLeaf(core.From) != nil {
		return nil, nil, fmt.Errorf("LATERAL must follow the FROM items it references")
	}
	// Phase 1: materialize every leaf (scan base tables, evaluate derived
	// tables; LATERAL leaves contribute their static schema only).
	leaves, err := e.loadLeaves(ctx, core.From, outer)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, t := range leaves {
		if seen[t.qual] {
			return nil, nil, fmt.Errorf("duplicate table name/alias %q in FROM (alias one of them)", t.qual)
		}
		seen[t.qual] = true
	}
	owners := map[string]int{}
	for _, t := range leaves {
		for _, c := range t.sc.Columns {
			owners[c.Name]++
		}
	}
	// Statically resolve each join node's USING/NATURAL keys.
	resolved := map[*JoinExpr]resolvedJoin{}
	rootInfo, err := resolveJoinNode(core.From, leaves, owners, resolved)
	if err != nil {
		return nil, nil, err
	}
	usingSeen := map[string]bool{}
	for _, cn := range rootInfo.usingCols {
		usingSeen[cn] = true
	}
	// Validate every column reference up front: ambiguous bare names and
	// unknown qualifiers fail here with a clear message instead of a
	// confusing per-row miss. References the enclosing query provides
	// (LATERAL bodies, outer row) resolve at runtime instead.
	quals := map[string]*tableSchema{}
	for _, t := range leaves {
		quals[t.qual] = t.sc
	}
	var exprs []Expr
	for _, it := range core.Items {
		if !it.Star {
			exprs = append(exprs, it.Expr)
		}
	}
	exprs = append(exprs, core.GroupBy...)
	exprs = append(exprs, extraExprs...)
	if core.Where != nil {
		exprs = append(exprs, core.Where)
	}
	collectOnExprs(core.From, &exprs)
	for _, ex := range exprs {
		var refErr error
		walkExpr(ex, func(n Expr) {
			cr, ok := n.(*ColumnRef)
			if !ok || refErr != nil {
				return
			}
			if cr.Table == "" {
				if owners[cr.Column] > 1 && !usingSeen[cr.Column] {
					refErr = fmt.Errorf("ambiguous column %q: qualify it (e.g. %s.%s)", cr.Column, leaves[0].qual, cr.Column)
				}
				return
			}
			sc, ok := quals[cr.Table]
			if !ok {
				if outer != nil {
					return // may belong to the enclosing query
				}
				refErr = fmt.Errorf("unknown table %q in reference %s", cr.Table, cr.String())
				return
			}
			if _, ok := sc.col(cr.Column); !ok {
				refErr = fmt.Errorf("no such column: %s", cr.String())
			}
		})
		if refErr != nil {
			return nil, nil, refErr
		}
	}
	// Phase 2: evaluate the tree.
	leafByQual := map[string]joinTable{}
	for _, t := range leaves {
		leafByQual[t.qual] = t
	}
	src, err := e.evalTableNode(ctx, core.From, &joinEnv{
		leaves: leafByQual, owners: owners, resolved: resolved, outer: outer,
	})
	if err != nil {
		return nil, nil, err
	}
	// Synthetic schema for * expansion: USING columns once (bare, pg
	// style), then each leaf's remaining columns, qualified, in order.
	syn := &tableSchema{Name: "join"}
	for _, cn := range rootInfo.usingCols {
		for _, t := range leaves {
			if c, ok := t.sc.col(cn); ok {
				c.Name = cn
				syn.Columns = append(syn.Columns, c)
				break
			}
		}
	}
	for _, t := range leaves {
		for _, c := range t.sc.Columns {
			if usingSeen[c.Name] {
				continue
			}
			qc := c
			qc.Name = t.qual + "." + c.Name
			syn.Columns = append(syn.Columns, qc)
		}
	}
	return syn, src.rows, nil
}

// loadLeaves materializes the tree's leaves in order.
func (e *Engine) loadLeaves(ctx context.Context, te TableExpr, outer row) ([]joinTable, error) {
	switch t := te.(type) {
	case *TableRef:
		qual := t.Alias
		if qual == "" {
			qual = t.Table
		}
		sc, err := e.loadSchema(ctx, t.Table)
		if err != nil {
			return nil, err
		}
		stored, err := e.scan(ctx, sc)
		if err != nil {
			return nil, err
		}
		rows := make([]row, len(stored))
		for i, sr := range stored {
			rows[i] = sr.data
		}
		return []joinTable{{qual: qual, sc: sc, rows: rows}}, nil
	case *SubqueryRef:
		if t.Alias == "" {
			return nil, fmt.Errorf("a subquery in FROM needs an alias: (SELECT ...) AS name")
		}
		if t.Lateral {
			sc, err := lateralSchema(t)
			if err != nil {
				return nil, err
			}
			return []joinTable{{qual: t.Alias, sc: sc, lateral: t}}, nil
		}
		cols, vals, err := e.evalSelect(ctx, t.Select, outer)
		if err != nil {
			return nil, err
		}
		sc, err := derivedSchema(t.Alias, cols)
		if err != nil {
			return nil, err
		}
		rows := make([]row, len(vals))
		for i, vr := range vals {
			r := row{}
			for j, cn := range cols {
				if j < len(vr) {
					r[cn] = vr[j]
				}
			}
			rows[i] = r
		}
		return []joinTable{{qual: t.Alias, sc: sc, rows: rows}}, nil
	case *JoinExpr:
		l, err := e.loadLeaves(ctx, t.L, outer)
		if err != nil {
			return nil, err
		}
		r, err := e.loadLeaves(ctx, t.R, outer)
		if err != nil {
			return nil, err
		}
		if lat := lateralLeaf(t.R); lat != nil && (t.Type == "RIGHT" || t.Type == "FULL") {
			return nil, fmt.Errorf("LATERAL cannot be the right side of a %s JOIN", t.Type)
		}
		if lat := lateralLeaf(t.L); lat != nil {
			return nil, fmt.Errorf("LATERAL must follow the FROM items it references")
		}
		return append(l, r...), nil
	}
	return nil, fmt.Errorf("unsupported FROM item %T", te)
}

// lateralLeaf returns the lateral SubqueryRef when the subtree is exactly
// a lateral leaf.
func lateralLeaf(te TableExpr) *SubqueryRef {
	if sq, ok := te.(*SubqueryRef); ok && sq.Lateral {
		return sq
	}
	return nil
}

// derivedSchema builds the synthetic schema of a derived table from its
// output labels.
func derivedSchema(alias string, cols []string) (*tableSchema, error) {
	sc := &tableSchema{Name: alias}
	dup := map[string]bool{}
	for _, cn := range cols {
		if dup[cn] {
			return nil, fmt.Errorf("duplicate column %q in subquery %s: alias it", cn, alias)
		}
		dup[cn] = true
		sc.Columns = append(sc.Columns, column{Name: cn, Type: TypeAny, TypeName: ""})
	}
	return sc, nil
}

// lateralSchema derives a LATERAL subquery's columns statically from its
// projection labels ("*" would need the rows we don't have yet).
func lateralSchema(sq *SubqueryRef) (*tableSchema, error) {
	var cols []string
	for _, it := range sq.Select.Core.Items {
		if it.Star {
			return nil, fmt.Errorf("SELECT * is not supported in a LATERAL subquery — list the columns")
		}
		cols = append(cols, labelFor(it))
	}
	return derivedSchema(sq.Alias, cols)
}

// collectOnExprs gathers every ON predicate in the tree.
func collectOnExprs(te TableExpr, out *[]Expr) {
	if j, ok := te.(*JoinExpr); ok {
		if j.On != nil {
			*out = append(*out, j.On)
		}
		collectOnExprs(j.L, out)
		collectOnExprs(j.R, out)
	}
}

// nodeInfo is a subtree's static shape: its leaves and coalesced columns.
type nodeInfo struct {
	tables    []joinTable
	usingCols []string
}

// resolveJoinNode statically resolves every join node's USING/NATURAL
// key sets, top-down over the tree.
func resolveJoinNode(te TableExpr, leaves []joinTable, owners map[string]int, resolved map[*JoinExpr]resolvedJoin) (nodeInfo, error) {
	find := func(qual string) joinTable {
		for _, t := range leaves {
			if t.qual == qual {
				return t
			}
		}
		return joinTable{}
	}
	var walk func(TableExpr) (nodeInfo, error)
	walk = func(te TableExpr) (nodeInfo, error) {
		switch t := te.(type) {
		case *TableRef:
			qual := t.Alias
			if qual == "" {
				qual = t.Table
			}
			return nodeInfo{tables: []joinTable{find(qual)}}, nil
		case *SubqueryRef:
			return nodeInfo{tables: []joinTable{find(t.Alias)}}, nil
		case *JoinExpr:
			li, err := walk(t.L)
			if err != nil {
				return nodeInfo{}, err
			}
			ri, err := walk(t.R)
			if err != nil {
				return nodeInfo{}, err
			}
			cols := t.Using
			if t.Natural {
				rightNames := map[string]bool{}
				for _, rt := range ri.tables {
					for _, c := range rt.sc.Columns {
						rightNames[c.Name] = true
					}
				}
				for _, cn := range ri.usingCols {
					rightNames[cn] = true
				}
				colSeen := map[string]bool{}
				for _, cn := range li.usingCols {
					if rightNames[cn] && !colSeen[cn] {
						colSeen[cn] = true
						cols = append(cols, cn)
					}
				}
				for _, lt := range li.tables {
					for _, c := range lt.sc.Columns {
						if rightNames[c.Name] && !colSeen[c.Name] {
							colSeen[c.Name] = true
							cols = append(cols, c.Name)
						}
					}
				}
			}
			rj := resolvedJoin{usingCols: cols}
			for _, cn := range cols {
				lk, err := sideKey(cn, li, "left")
				if err != nil {
					return nodeInfo{}, err
				}
				rk, err := sideKey(cn, ri, "right")
				if err != nil {
					return nodeInfo{}, err
				}
				rj.leftKeys = append(rj.leftKeys, lk)
				rj.rightKeys = append(rj.rightKeys, rk)
			}
			resolved[t] = rj
			out := nodeInfo{
				tables:    append(append([]joinTable{}, li.tables...), ri.tables...),
				usingCols: append(append([]string{}, li.usingCols...), ri.usingCols...),
			}
			for _, cn := range cols {
				dup := false
				for _, prev := range out.usingCols {
					if prev == cn {
						dup = true
					}
				}
				if !dup {
					out.usingCols = append(out.usingCols, cn)
				}
			}
			return out, nil
		}
		return nodeInfo{}, fmt.Errorf("unsupported FROM item %T", te)
	}
	return walk(te)
}

// sideKey resolves the row key one side of a USING join reads a column
// from: the coalesced bare key when an inner join already merged it, else
// the single owning table's qualified key.
func sideKey(cn string, side nodeInfo, which string) (string, error) {
	for _, uc := range side.usingCols {
		if uc == cn {
			return cn, nil
		}
	}
	var ownersHere []string
	for _, t := range side.tables {
		if _, ok := t.sc.col(cn); ok {
			ownersHere = append(ownersHere, t.qual)
		}
	}
	switch len(ownersHere) {
	case 0:
		return "", fmt.Errorf("USING column %q is not on the %s side of the join", cn, which)
	case 1:
		return ownersHere[0] + "." + cn, nil
	}
	return "", fmt.Errorf("USING column %q is ambiguous on the %s side (%s, %s)", cn, which, ownersHere[0], ownersHere[1])
}

// joinEnv carries the shared context of one tree evaluation.
type joinEnv struct {
	leaves   map[string]joinTable
	owners   map[string]int
	resolved map[*JoinExpr]resolvedJoin
	outer    row
}

// evalTableNode evaluates a subtree to its joined row set.
func (e *Engine) evalTableNode(ctx context.Context, te TableExpr, env *joinEnv) (joinSource, error) {
	switch t := te.(type) {
	case *TableRef:
		qual := t.Alias
		if qual == "" {
			qual = t.Table
		}
		return leafSource(env.leaves[qual], env.owners), nil
	case *SubqueryRef:
		return leafSource(env.leaves[t.Alias], env.owners), nil
	case *JoinExpr:
		left, err := e.evalTableNode(ctx, t.L, env)
		if err != nil {
			return joinSource{}, err
		}
		if lat := lateralLeaf(t.R); lat != nil {
			return e.lateralFold(ctx, left, env.leaves[lat.Alias], t, env)
		}
		right, err := e.evalTableNode(ctx, t.R, env)
		if err != nil {
			return joinSource{}, err
		}
		return e.joinFold(left, right, t, env)
	}
	return joinSource{}, fmt.Errorf("unsupported FROM item %T", te)
}

// leafSource lifts one materialized leaf into a joinSource.
func leafSource(t joinTable, owners map[string]int) joinSource {
	rows := make([]row, len(t.rows))
	for i, r := range t.rows {
		rows[i] = qualifyJoinRow(r, t, owners)
	}
	return joinSource{tables: []joinTable{t}, rows: rows}
}

// qualifyJoinRow builds the joined-row view of one table row: qualified
// keys always, bare keys only for unambiguous column names.
func qualifyJoinRow(r row, t joinTable, owners map[string]int) row {
	out := row{}
	for _, c := range t.sc.Columns {
		v := r[c.Name]
		out[t.qual+"."+c.Name] = v
		if owners[c.Name] == 1 {
			out[c.Name] = v
		}
	}
	return out
}

// nullExtensionSource returns a source's keys with every value NULL — one
// side of an outer join's no-match row.
func nullExtensionSource(s joinSource, owners map[string]int) row {
	out := row{}
	for _, t := range s.tables {
		for _, c := range t.sc.Columns {
			out[t.qual+"."+c.Name] = nullV()
			if owners[c.Name] == 1 {
				out[c.Name] = nullV()
			}
		}
	}
	for _, cn := range s.usingCols {
		out[cn] = nullV()
	}
	return out
}

// mergedSource combines two sides' static shape plus this node's new
// USING columns.
func mergedSource(l, r joinSource, using []string, rows []row) joinSource {
	out := joinSource{
		tables:    append(append([]joinTable{}, l.tables...), r.tables...),
		usingCols: append(append([]string{}, l.usingCols...), r.usingCols...),
		rows:      rows,
	}
	for _, cn := range using {
		dup := false
		for _, prev := range out.usingCols {
			if prev == cn {
				dup = true
			}
		}
		if !dup {
			out.usingCols = append(out.usingCols, cn)
		}
	}
	return out
}

// qualSet is the set of qualifiers a source answers to.
func qualSet(s joinSource) map[string]bool {
	out := map[string]bool{}
	for _, t := range s.tables {
		out[t.qual] = true
	}
	return out
}

// joinFold joins two evaluated sources at one join node.
func (e *Engine) joinFold(left, right joinSource, j *JoinExpr, env *joinEnv) (joinSource, error) {
	rj := env.resolved[j]
	emitLeft := j.Type == "LEFT" || j.Type == "FULL"
	emitRight := j.Type == "RIGHT" || j.Type == "FULL"
	nullLeft := nullExtensionSource(left, env.owners)
	nullRight := nullExtensionSource(right, env.owners)

	merge := func(l, r row) row {
		m := make(row, len(l)+len(r)+len(rj.usingCols))
		for k, v := range l {
			m[k] = v
		}
		for k, v := range r {
			m[k] = v
		}
		// The coalesced bare value for USING columns: left when present,
		// right on a left-null row — i.e. first non-NULL.
		for i, cn := range rj.usingCols {
			v := m[rj.leftKeys[i]]
			if v.IsNull() {
				v = m[rj.rightKeys[i]]
			}
			m[cn] = v
		}
		return m
	}

	// Hash path: USING/NATURAL keys, or an ON clause that is a pure
	// conjunction of cross-side equalities.
	leftKeys, rightKeys := rj.leftKeys, rj.rightKeys
	if len(leftKeys) == 0 && j.Type != "CROSS" {
		leftKeys, rightKeys, _ = equiPairsSides(j.On, qualSet(left), qualSet(right))
	}
	if len(leftKeys) > 0 {
		table := map[string][]int{}
		for i, rr := range right.rows {
			key, null, err := joinKey(rr, rightKeys)
			if err != nil {
				return joinSource{}, err
			}
			if null {
				continue // NULL never equals anything
			}
			table[key] = append(table[key], i)
		}
		rightMatched := make([]bool, len(right.rows))
		var out []row
		for _, lr := range left.rows {
			key, null, err := joinKey(lr, leftKeys)
			if err != nil {
				return joinSource{}, err
			}
			var matches []int
			if !null {
				matches = table[key]
			}
			for _, ri := range matches {
				rightMatched[ri] = true
				out = append(out, merge(lr, right.rows[ri]))
			}
			if len(matches) == 0 && emitLeft {
				out = append(out, merge(lr, nullRight))
			}
		}
		if emitRight {
			for i, rr := range right.rows {
				if !rightMatched[i] {
					out = append(out, merge(nullLeft, rr))
				}
			}
		}
		return mergedSource(left, right, rj.usingCols, out), nil
	}

	// Nested loop: CROSS (including NATURAL with no common columns, where
	// every pair matches) or a general ON predicate.
	rightMatched := make([]bool, len(right.rows))
	var out []row
	for _, lr := range left.rows {
		matched := false
		for ri, rr := range right.rows {
			m := merge(lr, rr)
			if j.On != nil {
				ok, err := predicateTrue(j.On, m)
				if err != nil {
					return joinSource{}, err
				}
				if !ok {
					continue
				}
			}
			matched = true
			rightMatched[ri] = true
			out = append(out, m)
		}
		if !matched && emitLeft {
			out = append(out, merge(lr, nullRight))
		}
	}
	if emitRight {
		for i, rr := range right.rows {
			if !rightMatched[i] {
				out = append(out, merge(nullLeft, rr))
			}
		}
	}
	return mergedSource(left, right, rj.usingCols, out), nil
}

// lateralFold evaluates a LATERAL subquery once per left row, with that
// row's columns (and the enclosing query's outer row) in scope.
func (e *Engine) lateralFold(ctx context.Context, left joinSource, leaf joinTable, j *JoinExpr, env *joinEnv) (joinSource, error) {
	if len(j.Using) > 0 || j.Natural {
		return joinSource{}, fmt.Errorf("LATERAL joins take ON (or CROSS), not USING/NATURAL")
	}
	sq := leaf.lateral
	rightSrc := joinSource{tables: []joinTable{leaf}}
	nullRight := nullExtensionSource(rightSrc, env.owners)
	emitLeft := j.Type == "LEFT"
	var out []row
	for _, lr := range left.rows {
		// The lateral scope: enclosing outer row, overlaid by this row.
		scope := make(row, len(env.outer)+len(lr))
		for k, v := range env.outer {
			scope[k] = v
		}
		for k, v := range lr {
			scope[k] = v
		}
		cols, vals, err := e.evalSelect(ctx, sq.Select, scope)
		if err != nil {
			return joinSource{}, err
		}
		matched := false
		for _, vr := range vals {
			r := row{}
			for i, cn := range cols {
				if i < len(vr) {
					r[cn] = vr[i]
				}
			}
			m := merge2(lr, qualifyJoinRow(r, leaf, env.owners))
			if j.On != nil {
				ok, err := predicateTrue(j.On, m)
				if err != nil {
					return joinSource{}, err
				}
				if !ok {
					continue
				}
			}
			matched = true
			out = append(out, m)
		}
		if !matched && emitLeft {
			out = append(out, merge2(lr, nullRight))
		}
	}
	return mergedSource(left, rightSrc, nil, out), nil
}

// merge2 overlays r onto a copy of l.
func merge2(l, r row) row {
	m := make(row, len(l)+len(r))
	for k, v := range l {
		m[k] = v
	}
	for k, v := range r {
		m[k] = v
	}
	return m
}

// equiPairsSides recognizes "a.x = b.y [AND ...]" ON clauses where each
// equality references the two sides of the join, one each. It returns
// the per-side lookup keys (qualified) in matching order.
func equiPairsSides(on Expr, leftQuals, rightQuals map[string]bool) (leftKeys, rightKeys []string, ok bool) {
	var walk func(Expr) bool
	walk = func(ex Expr) bool {
		b, isBin := ex.(*Binary)
		if !isBin {
			return false
		}
		if b.Op == "AND" {
			return walk(b.L) && walk(b.R)
		}
		if b.Op != "=" {
			return false
		}
		l, lok := b.L.(*ColumnRef)
		r, rok := b.R.(*ColumnRef)
		if !lok || !rok || l.Table == "" || r.Table == "" {
			return false
		}
		switch {
		case leftQuals[l.Table] && rightQuals[r.Table]:
			leftKeys = append(leftKeys, l.Table+"."+l.Column)
			rightKeys = append(rightKeys, r.Table+"."+r.Column)
		case rightQuals[l.Table] && leftQuals[r.Table]:
			leftKeys = append(leftKeys, r.Table+"."+r.Column)
			rightKeys = append(rightKeys, l.Table+"."+l.Column)
		default:
			return false
		}
		return true
	}
	if on == nil || !walk(on) {
		return nil, nil, false
	}
	return leftKeys, rightKeys, true
}

// joinKey builds the hash key for one row over the given lookup keys.
// null reports that any component was NULL (such rows never match).
func joinKey(r row, keys []string) (key string, null bool, err error) {
	vals := make([]Value, len(keys))
	for i, k := range keys {
		v := r[k]
		if v.IsNull() {
			return "", true, nil
		}
		vals[i] = v
	}
	s, err := encodeKey(vals...)
	if err != nil {
		return "", false, err
	}
	return s, false, nil
}

// --- shared expression helpers ----------------------------------------------

// walkExpr visits every node of an expression tree.
func walkExpr(e Expr, fn func(Expr)) {
	if e == nil {
		return
	}
	fn(e)
	switch x := e.(type) {
	case *Unary:
		walkExpr(x.X, fn)
	case *Binary:
		walkExpr(x.L, fn)
		walkExpr(x.R, fn)
	case *Between:
		walkExpr(x.X, fn)
		walkExpr(x.Lo, fn)
		walkExpr(x.Hi, fn)
	case *InList:
		walkExpr(x.X, fn)
		for _, item := range x.List {
			walkExpr(item, fn)
		}
	case *IsNull:
		walkExpr(x.X, fn)
	case *Cast:
		walkExpr(x.X, fn)
	case *FuncCall:
		for _, a := range x.Args {
			walkExpr(a, fn)
		}
	}
}

// hasQualifiedRef reports whether any expression references a qualified
// column — the signal that single-table rows need qualified keys added.
func hasQualifiedRef(exprs ...Expr) bool {
	found := false
	for _, e := range exprs {
		walkExpr(e, func(n Expr) {
			if cr, ok := n.(*ColumnRef); ok && cr.Table != "" {
				found = true
			}
		})
	}
	return found
}

// coreQual is the qualifier a core's rows answer to: the alias, or the
// table name.
func coreQual(core *SelectCore) string {
	if core.Alias != "" {
		return core.Alias
	}
	return core.Table
}

// coreUsesQualifiedRefs reports whether any expression of a single-table
// core (or its ORDER BY) uses a qualified column reference.
func coreUsesQualifiedRefs(core *SelectCore, order []OrderKey) bool {
	var exprs []Expr
	for _, it := range core.Items {
		if !it.Star {
			exprs = append(exprs, it.Expr)
		}
	}
	exprs = append(exprs, core.GroupBy...)
	if core.Where != nil {
		exprs = append(exprs, core.Where)
	}
	for _, k := range order {
		exprs = append(exprs, k.Expr)
	}
	return hasQualifiedRef(exprs...)
}

// withQualKeys returns a copy of a single-table row that also carries
// qualified keys under the given qualifier.
func withQualKeys(r row, qual string, sc *tableSchema) row {
	out := make(row, len(r)*2)
	for k, v := range r {
		out[k] = v
	}
	for _, c := range sc.Columns {
		out[qual+"."+c.Name] = r[c.Name]
	}
	return out
}
