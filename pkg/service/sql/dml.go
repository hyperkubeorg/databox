// dml.go implements INSERT, UPDATE and DELETE. Each mutating statement runs
// as one optimistic transaction through the client (§10): reads record the
// revisions they observed, writes stage locally, and commit either applies
// everything atomically or aborts with a retryable conflict. A conflict
// re-runs the statement body against fresh reads, exactly as the documented
// retry convention prescribes.
package sql

import (
	"context"
	"fmt"
	"strings"
)

// maxTxRetries bounds the OCC retry loop for a single statement.
const maxTxRetries = 20

// runTx runs fn inside a client transaction, retrying on commit conflicts.
// fn must be idempotent across attempts (it re-reads on each try).
func (e *Engine) runTx(ctx context.Context, fn func(tx kvTx) error) error {
	for attempt := 0; ; attempt++ {
		tx := e.c.NewTx()
		if err := fn(tx); err != nil {
			return err
		}
		err := tx.Commit(ctx)
		if err == nil {
			return nil
		}
		if attempt < maxTxRetries && strings.HasPrefix(strings.TrimPrefix(err.Error(), "retryable: "), "Conflict") {
			continue
		}
		if attempt < maxTxRetries && strings.Contains(err.Error(), "Conflict") {
			continue
		}
		return err
	}
}

// --- INSERT ------------------------------------------------------------------

// execInsert handles both VALUES and INSERT ... SELECT sources, plus an
// optional RETURNING projection and chai's ON CONFLICT clause.
func (e *Engine) execInsert(ctx context.Context, s *Insert) (ExecResult, error) {
	sc, err := e.loadSchema(ctx, s.Table)
	if err != nil {
		return ExecResult{}, err
	}
	// Resolve the target column list: explicit, or every column positionally.
	// Unknown targets fail with chai's wording.
	targetCols := s.Columns
	if len(targetCols) == 0 {
		targetCols = orderColumns(sc)
	} else {
		for _, cn := range targetCols {
			if _, ok := sc.col(cn); !ok {
				return ExecResult{}, fmt.Errorf("table has no column %s", cn)
			}
		}
	}
	// Expression-position subqueries in VALUES materialize first
	// (subquery.go).
	for i := range s.Rows {
		for j := range s.Rows[i] {
			ex, err := e.resolveExpr(ctx, s.Rows[i][j])
			if err != nil {
				return ExecResult{}, err
			}
			s.Rows[i][j] = ex
		}
	}
	// Materialize the source rows as column→value maps.
	var sourceRows []row
	if s.Select != nil {
		// Reading the target table while inserting into it would observe
		// its own writes; chai forbids it and so do we.
		if selectReadsTable(s.Select, s.Table) {
			return ExecResult{}, fmt.Errorf("cannot insert into table %q from a query reading it", s.Table)
		}
		_, vals, err := e.evalSelect(ctx, s.Select, nil)
		if err != nil {
			return ExecResult{}, err
		}
		for _, vr := range vals {
			r := row{}
			for i, cn := range targetCols {
				if i < len(vr) {
					r[cn] = vr[i]
				}
			}
			sourceRows = append(sourceRows, r)
		}
	} else {
		for _, exprRow := range s.Rows {
			// An explicit column list pins the arity; the positional form
			// allows fewer values than columns (the rest default), like chai.
			if len(exprRow) > len(targetCols) {
				return ExecResult{}, fmt.Errorf("INSERT has more values than target columns")
			}
			if len(s.Columns) > 0 && len(exprRow) != len(targetCols) {
				return ExecResult{}, fmt.Errorf("INSERT has %d values but %d target columns", len(exprRow), len(targetCols))
			}
			r := row{}
			for i, ex := range exprRow {
				// nil row: a column reference in VALUES is "no table
				// specified", exactly as in a table-less SELECT.
				v, err := evalExpr(ex, nil)
				if err != nil {
					return ExecResult{}, err
				}
				r[targetCols[i]] = v
			}
			sourceRows = append(sourceRows, r)
		}
	}

	var returning [][]Value
	count := 0
	err = e.runTx(ctx, func(tx kvTx) error {
		count = 0
		returning = nil
		// The rowid counter lives in the catalog; read it transactionally
		// so concurrent inserts into a rowid table can't collide.
		nextRowID := sc.NextRowID
		catalogDirty := false
		// stagedUnique tracks unique-index keys claimed by earlier rows of
		// THIS statement — staged writes are invisible to snapshot reads,
		// so "INSERT ... VALUES (1,1), (2,1)" must find its own duplicate
		// here (INSERT/unique.sql "same value, same statement").
		stagedUnique := map[string]string{}
		for _, src := range sourceRows {
			full, err := e.completeRow(sc, src)
			if err != nil {
				return err
			}
			// Auto-increment PK: a NULL/omitted value draws the next
			// counter value; an explicit value ratchets the counter past
			// itself so later auto values never collide with it.
			if ac := sc.autoCol(); ac != "" {
				if v := full[ac]; v.IsNull() {
					nextRowID++
					catalogDirty = true
					full[ac] = intV(nextRowID)
				} else if v.T == TypeInt && v.I > nextRowID {
					nextRowID = v.I
					catalogDirty = true
				}
			}
			if err := e.enforceChecks(sc, full); err != nil {
				return err
			}
			pkhex, err := e.pkHexFor(sc, full, nextRowID+1)
			if err != nil {
				return err
			}
			if !sc.hasPK() {
				nextRowID++
				catalogDirty = true
			}
			// Primary-key uniqueness: a staged read of the row key both
			// detects an existing row and enrolls it in conflict checking.
			pkTaken := false
			if _, exists, err := tx.Get(ctx, rowKey(e.db, sc.Name, pkhex)); err != nil {
				return err
			} else if exists {
				pkTaken = true
			}
			ukeys, err := uniqueKeysFor(sc, full)
			if err != nil {
				return err
			}
			dupErr := error(nil)
			var conflictPKs []string
			switch {
			case pkTaken:
				dupErr = fmt.Errorf("PRIMARY KEY constraint error: [%s]", strings.Join(sc.PK, ", "))
				conflictPKs = append(conflictPKs, pkhex)
			default:
				// First against rows staged by this very statement, then
				// against the committed snapshot.
				for _, uk := range ukeys {
					if prev, ok := stagedUnique[uk.id]; ok && prev != pkhex {
						dupErr = uniqueErr(uk.idx)
						conflictPKs = append(conflictPKs, prev)
						break
					}
				}
				if dupErr == nil {
					uerr, dups, err := e.uniqueConflicts(ctx, sc, full, pkhex)
					if err != nil {
						return err
					}
					dupErr, conflictPKs = uerr, dups
				}
			}
			if dupErr != nil {
				switch s.OnConflict {
				case "NOTHING":
					continue // silently drop the conflicting row
				case "REPLACE":
					// Remove every row this one collides with, then insert.
					for _, oldPK := range conflictPKs {
						if err := e.stageRowDelete(ctx, sc, oldPK, tx); err != nil {
							return err
						}
						for k, v := range stagedUnique {
							if v == oldPK {
								delete(stagedUnique, k)
							}
						}
					}
				default:
					return dupErr
				}
			}
			enc, err := encodeRow(full)
			if err != nil {
				return err
			}
			tx.Set(rowKey(e.db, sc.Name, pkhex), enc)
			if err := e.stageIndexWrites(sc, full, pkhex, tx); err != nil {
				return err
			}
			for _, uk := range ukeys {
				stagedUnique[uk.id] = pkhex
			}
			count++
			if s.Returning != nil {
				vals, err := projectValues(s.Returning, full, sc)
				if err != nil {
					return err
				}
				returning = append(returning, vals)
			}
		}
		if catalogDirty {
			sc.NextRowID = nextRowID
			tx.Set(catalogKey(e.db, sc.Name), sc.encode())
		}
		return nil
	})
	if err != nil {
		return ExecResult{}, err
	}
	res := ExecResult{Tag: fmt.Sprintf("INSERT 0 %d", count)}
	if s.Returning != nil {
		res.Columns = returningColumns(s.Returning, sc)
		res.typed = returning
		for _, vals := range returning {
			res.Rows = append(res.Rows, textRowOf(vals))
		}
	}
	return res, nil
}

// completeRow fills defaults for unspecified columns, coerces every value
// to its column's declared type, and enforces NOT NULL.
func (e *Engine) completeRow(sc *tableSchema, src row) (row, error) {
	out := row{}
	for _, c := range sc.Columns {
		v, ok := src[c.Name]
		if !ok {
			// Apply DEFAULT when present, else NULL.
			if c.Default != "" {
				dv, err := evalDefault(c.Default)
				if err != nil {
					return nil, err
				}
				v = dv
			} else {
				v = nullV()
			}
		}
		if !v.IsNull() {
			// The cast error surfaces bare — chai's INSERT corpus asserts
			// the exact strconv wording with no column prefix.
			cv, err := CastTo(v, c.Type)
			if err != nil {
				return nil, err
			}
			v = cv
			// A vector column's declared dimension is enforced on every
			// write (vector.go).
			if c.Type == TypeVector {
				if err := checkVectorDim(c.Name, c.Dim, v); err != nil {
					return nil, err
				}
			}
			// A UUID column's text form is validated and canonicalized on
			// every write (uuid.go).
			if v, err = coerceUUIDColumn(c, v); err != nil {
				return nil, err
			}
		}
		// An auto-increment column may arrive NULL: the insert loop assigns
		// it from the counter before the row is stored.
		if v.IsNull() && c.NotNull && !c.Auto {
			return nil, fmt.Errorf("NOT NULL constraint error: [%s]", c.Name)
		}
		out[c.Name] = v
	}
	// Reject columns that are not part of the table.
	for name := range src {
		if _, ok := sc.col(name); !ok {
			return nil, fmt.Errorf("table has no column %s", name)
		}
	}
	return out, nil
}

// evalDefault parses and evaluates a stored DEFAULT expression.
func evalDefault(text string) (Value, error) {
	stmts, err := ParseStatements("SELECT " + text)
	if err != nil {
		return Value{}, fmt.Errorf("bad default %q: %v", text, err)
	}
	sel, ok := stmts[0].(*Select)
	if !ok || len(sel.Core.Items) != 1 {
		return Value{}, fmt.Errorf("bad default %q", text)
	}
	return evalExpr(sel.Core.Items[0].Expr, row{})
}

// uniqueConflicts checks every unique index on the table for a candidate
// row, using an index-prefix scan (snapshot read) to find colliding
// entries. A NULL in any indexed column exempts the row — NULLs never
// collide, matching chai (INSERT/unique.sql). It returns the constraint
// error and the primary keys of the rows collided with (for ON CONFLICT
// DO REPLACE); both are nil when the row is clean.
func (e *Engine) uniqueConflicts(ctx context.Context, sc *tableSchema, r row, selfPK string) (error, []string, error) {
	for _, idx := range sc.Indexes {
		if !idx.Unique || rowHasNullIn(idx.Columns, r) {
			continue
		}
		keyhex, err := indexKeyHex(idx, r)
		if err != nil {
			return nil, nil, err
		}
		prefix := oneIndexPrefix(e.db, sc.Name, idx.Name) + keyhex + "/"
		entries, _, err := e.c.List(ctx, prefix, "", 2)
		if err != nil {
			return nil, nil, err
		}
		var dups []string
		for _, ent := range entries {
			if pk := strings.TrimPrefix(ent.Key, prefix); pk != selfPK {
				dups = append(dups, pk)
			}
		}
		if len(dups) > 0 {
			return uniqueErr(idx), dups, nil
		}
	}
	return nil, nil, nil
}

// uniqueErr formats a unique violation exactly as chai does.
func uniqueErr(idx indexDef) error {
	return fmt.Errorf("UNIQUE constraint error: [%s]", strings.Join(idx.Columns, ", "))
}

// uniqueKey identifies one unique-index slot a row occupies.
type uniqueKey struct {
	idx indexDef
	id  string // "<index name>/<encoded key>"
}

// uniqueKeysFor lists the unique-index slots a row would claim (skipping
// NULL-containing keys, which never conflict).
func uniqueKeysFor(sc *tableSchema, r row) ([]uniqueKey, error) {
	var out []uniqueKey
	for _, idx := range sc.Indexes {
		if !idx.Unique || rowHasNullIn(idx.Columns, r) {
			continue
		}
		keyhex, err := indexKeyHex(idx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, uniqueKey{idx: idx, id: idx.Name + "/" + keyhex})
	}
	return out, nil
}

// rowHasNullIn reports whether any of the named columns is NULL in r.
func rowHasNullIn(cols []string, r row) bool {
	for _, cn := range cols {
		if r[cn].IsNull() {
			return true
		}
	}
	return false
}

// stageRowDelete stages removal of one row (by pkhex) and its index
// entries inside a transaction — the ON CONFLICT DO REPLACE eviction.
func (e *Engine) stageRowDelete(ctx context.Context, sc *tableSchema, pkhex string, tx kvTx) error {
	raw, found, err := tx.Get(ctx, rowKey(e.db, sc.Name, pkhex))
	if err != nil || !found {
		return err
	}
	old, err := decodeRow(raw)
	if err != nil {
		return err
	}
	if err := e.stageIndexDeletes(sc, old, pkhex, tx); err != nil {
		return err
	}
	tx.Delete(rowKey(e.db, sc.Name, pkhex))
	return nil
}

// enforceChecks evaluates every CHECK constraint against a candidate row.
// NULL passes (unknown is not a violation); any non-truthy result fails
// with chai's wording.
func (e *Engine) enforceChecks(sc *tableSchema, r row) error {
	for _, chk := range sc.Checks {
		ex, err := parseExprText(chk.Expr)
		if err != nil {
			return fmt.Errorf("corrupt check constraint %q: %v", chk.Name, err)
		}
		v, err := evalExpr(ex, r)
		if err != nil {
			return err
		}
		if v.IsNull() {
			continue
		}
		if b, ok := truth(v); !ok || !b {
			return fmt.Errorf("row violates check constraint %q", chk.Name)
		}
	}
	return nil
}

// parseExprText parses a stored SQL expression (DEFAULT/CHECK text).
func parseExprText(text string) (Expr, error) {
	stmts, err := ParseStatements("SELECT " + text)
	if err != nil {
		return nil, err
	}
	sel, ok := stmts[0].(*Select)
	if !ok || len(sel.Core.Items) != 1 || sel.Core.Items[0].Star {
		return nil, fmt.Errorf("not an expression: %q", text)
	}
	return sel.Core.Items[0].Expr, nil
}

// selectReadsTable reports whether any core of a SELECT reads the table.
func selectReadsTable(s *Select, table string) bool {
	var treeReads func(te TableExpr) bool
	treeReads = func(te TableExpr) bool {
		switch t := te.(type) {
		case *TableRef:
			return t.Table == table
		case *SubqueryRef:
			return selectReadsTable(t.Select, table)
		case *JoinExpr:
			return treeReads(t.L) || treeReads(t.R)
		}
		return false
	}
	coreReads := func(c *SelectCore) bool {
		if c.Table == table {
			return true
		}
		return c.From != nil && treeReads(c.From)
	}
	if coreReads(s.Core) {
		return true
	}
	for _, arm := range s.Unions {
		if coreReads(arm.Core) {
			return true
		}
	}
	return false
}

// stageIndexWrites stages index entries for a row inside a transaction.
func (e *Engine) stageIndexWrites(sc *tableSchema, r row, pkhex string, tx kvTx) error {
	for _, idx := range sc.Indexes {
		keyhex, err := indexKeyHex(idx, r)
		if err != nil {
			return err
		}
		tx.Set(indexEntryKey(e.db, sc.Name, idx.Name, keyhex, pkhex), []byte(pkhex))
	}
	return nil
}

// stageIndexDeletes stages removal of a row's index entries.
func (e *Engine) stageIndexDeletes(sc *tableSchema, r row, pkhex string, tx kvTx) error {
	for _, idx := range sc.Indexes {
		keyhex, err := indexKeyHex(idx, r)
		if err != nil {
			return err
		}
		tx.Delete(indexEntryKey(e.db, sc.Name, idx.Name, keyhex, pkhex))
	}
	return nil
}

// --- UPDATE ------------------------------------------------------------------

// execUpdate applies SET assignments to every row matching WHERE. Rows are
// selected by a snapshot scan; the transaction re-reads each affected key so
// commit detects a concurrent writer.
func (e *Engine) execUpdate(ctx context.Context, s *Update) (ExecResult, error) {
	sc, err := e.loadSchema(ctx, s.Table)
	if err != nil {
		return ExecResult{}, err
	}
	// Expression-position subqueries materialize first (subquery.go).
	if s.Where != nil {
		if s.Where, err = e.resolveExpr(ctx, s.Where); err != nil {
			return ExecResult{}, err
		}
	}
	for i := range s.Set {
		if s.Set[i].Value, err = e.resolveExpr(ctx, s.Set[i].Value); err != nil {
			return ExecResult{}, err
		}
	}
	// Planned scan: only the key ranges the WHERE clause can match are
	// read; the predicate below stays authoritative for every row.
	rows, _, err := e.scanFor(ctx, sc, s.Where)
	if err != nil {
		return ExecResult{}, err
	}
	qualifyPred := hasQualifiedRef(s.Where)
	count := 0
	err = e.runTx(ctx, func(tx kvTx) error {
		count = 0
		for _, sr := range rows {
			pred := sr.data
			if qualifyPred {
				pred = withQualKeys(sr.data, sc.Name, sc)
			}
			ok, err := predicateTrue(s.Where, pred)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			// Record the current revision of this row for conflict checking.
			cur, exists, err := tx.Get(ctx, rowKey(e.db, sc.Name, sr.pkhex))
			if err != nil {
				return err
			}
			if !exists {
				continue // deleted by a concurrent tx since the scan
			}
			curRow, err := decodeRow(cur)
			if err != nil {
				return err
			}
			newRow, err := e.applyAssignments(sc, curRow, s.Set)
			if err != nil {
				return err
			}
			if err := e.enforceChecks(sc, newRow); err != nil {
				return err
			}
			newPK, err := e.pkHexFor(sc, newRow, 0)
			if err != nil {
				return err
			}
			// A primary-key change must not land on another existing row
			// (UPDATE/pk.sql "set primary key / conflict").
			if newPK != sr.pkhex && sc.hasPK() {
				if _, exists, err := tx.Get(ctx, rowKey(e.db, sc.Name, newPK)); err != nil {
					return err
				} else if exists {
					return fmt.Errorf("PRIMARY KEY constraint error: [%s]", strings.Join(sc.PK, ", "))
				}
			}
			if uerr, _, err := e.uniqueConflicts(ctx, sc, newRow, newPK); err != nil {
				return err
			} else if uerr != nil {
				return uerr
			}
			enc, err := encodeRow(newRow)
			if err != nil {
				return err
			}
			// Maintain indexes and handle a primary-key change (rare).
			if err := e.stageIndexDeletes(sc, curRow, sr.pkhex, tx); err != nil {
				return err
			}
			if newPK != sr.pkhex {
				tx.Delete(rowKey(e.db, sc.Name, sr.pkhex))
			}
			tx.Set(rowKey(e.db, sc.Name, newPK), enc)
			if err := e.stageIndexWrites(sc, newRow, newPK, tx); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Tag: fmt.Sprintf("UPDATE %d", count)}, nil
}

// applyAssignments computes the updated row: evaluate each SET expression
// against the current row, then coerce to the column type.
func (e *Engine) applyAssignments(sc *tableSchema, cur row, set []Assignment) (row, error) {
	out := row{}
	for k, v := range cur {
		out[k] = v
	}
	for _, a := range set {
		col, ok := sc.col(a.Column)
		if !ok {
			return nil, fmt.Errorf("no such column: %s", a.Column)
		}
		v, err := evalExpr(a.Value, cur)
		if err != nil {
			return nil, err
		}
		if !v.IsNull() {
			v, err = CastTo(v, col.Type)
			if err != nil {
				return nil, err
			}
			// Same write-time dimension check as INSERT (vector.go).
			if col.Type == TypeVector {
				if err := checkVectorDim(col.Name, col.Dim, v); err != nil {
					return nil, err
				}
			}
			// Same UUID canonicalization as INSERT (uuid.go).
			if v, err = coerceUUIDColumn(col, v); err != nil {
				return nil, err
			}
		}
		if v.IsNull() && col.NotNull {
			return nil, fmt.Errorf("NOT NULL constraint error: [%s]", a.Column)
		}
		out[a.Column] = v
	}
	return out, nil
}

// --- DELETE ------------------------------------------------------------------

// execDelete removes every row matching WHERE, clearing index entries too.
func (e *Engine) execDelete(ctx context.Context, s *Delete) (ExecResult, error) {
	sc, err := e.loadSchema(ctx, s.Table)
	if err != nil {
		return ExecResult{}, err
	}
	// Expression-position subqueries materialize first (subquery.go).
	if s.Where != nil {
		if s.Where, err = e.resolveExpr(ctx, s.Where); err != nil {
			return ExecResult{}, err
		}
	}
	// Planned scan, same contract as UPDATE: the plan narrows what is
	// read, the WHERE predicate still decides what is deleted.
	rows, _, err := e.scanFor(ctx, sc, s.Where)
	if err != nil {
		return ExecResult{}, err
	}
	qualifyPred := hasQualifiedRef(s.Where)
	count := 0
	err = e.runTx(ctx, func(tx kvTx) error {
		count = 0
		for _, sr := range rows {
			pred := sr.data
			if qualifyPred {
				pred = withQualKeys(sr.data, sc.Name, sc)
			}
			ok, err := predicateTrue(s.Where, pred)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			cur, exists, err := tx.Get(ctx, rowKey(e.db, sc.Name, sr.pkhex))
			if err != nil {
				return err
			}
			if !exists {
				continue
			}
			curRow, err := decodeRow(cur)
			if err != nil {
				return err
			}
			if err := e.stageIndexDeletes(sc, curRow, sr.pkhex, tx); err != nil {
				return err
			}
			tx.Delete(rowKey(e.db, sc.Name, sr.pkhex))
			count++
		}
		return nil
	})
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Tag: fmt.Sprintf("DELETE %d", count)}, nil
}
