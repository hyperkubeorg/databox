// alter.go is the ALTER TABLE extension (chai has none; its tests sit in
// the conformance skip ledger). Four operations:
//
//	ALTER TABLE t ADD [COLUMN] name TYPE [NOT NULL] [DEFAULT expr]
//	ALTER TABLE t DROP [COLUMN] name
//	ALTER TABLE t RENAME [COLUMN] old TO new
//	ALTER TABLE t RENAME TO newname
//
// Stored rows always carry every schema column, so ADD backfills existing
// rows (DEFAULT per row, else NULL) and DROP/RENAME rewrite them — the
// same direct-write style CREATE INDEX backfill uses. Like all DDL here,
// a huge table makes these statements heavy; they are metadata-plus-
// rewrite, not lazy migrations. The words ALTER/ADD/COLUMN/RENAME/TO stay
// unreserved (statement-position only).
package sql

import (
	"context"
	"fmt"
	"strings"
)

// AlterTable is one ALTER TABLE statement.
type AlterTable struct {
	Table   string
	Op      string // "add" | "drop" | "rename_column" | "rename_table"
	Col     ColumnDef
	Name    string // drop: column; rename_column: old name
	NewName string // rename_column / rename_table target
}

func (*AlterTable) stmtNode() {}

// --- parsing -----------------------------------------------------------------

// parseAlter parses everything after the ALTER word.
func (p *parser) parseAlter() (Statement, error) {
	p.next() // ALTER
	if err := p.expectKw("TABLE"); err != nil {
		return nil, err
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	switch {
	case p.acceptIdent("add"):
		p.acceptIdent("column")
		col, err := p.parseAlterColumnDef()
		if err != nil {
			return nil, err
		}
		return &AlterTable{Table: name, Op: "add", Col: col}, nil
	case p.acceptKw("DROP"):
		p.acceptIdent("column")
		cn, err := p.ident()
		if err != nil {
			return nil, err
		}
		return &AlterTable{Table: name, Op: "drop", Name: cn}, nil
	case p.acceptIdent("rename"):
		if p.acceptIdent("to") {
			nn, err := p.ident()
			if err != nil {
				return nil, err
			}
			return &AlterTable{Table: name, Op: "rename_table", NewName: nn}, nil
		}
		p.acceptIdent("column")
		old, err := p.ident()
		if err != nil {
			return nil, err
		}
		if !p.acceptIdent("to") {
			return nil, errAt(p.peek().pos, "expected TO, found %q", p.peek().s)
		}
		nn, err := p.ident()
		if err != nil {
			return nil, err
		}
		return &AlterTable{Table: name, Op: "rename_column", Name: old, NewName: nn}, nil
	}
	return nil, errAt(p.peek().pos, "expected ADD, DROP or RENAME after ALTER TABLE %s, found %q", name, p.peek().s)
}

// parseAlterColumnDef parses the column definition ADD COLUMN accepts:
// type plus NOT NULL / DEFAULT. Keys, uniqueness and checks are separate
// statements or CREATE-time constructs, and auto-increment is bound to
// table creation (the counter guards one PK column).
func (p *parser) parseAlterColumnDef() (ColumnDef, error) {
	name, err := p.ident()
	if err != nil {
		return ColumnDef{}, err
	}
	col := ColumnDef{Name: name, Type: TypeAny}
	if p.peek().kind == tkIdent {
		typ, typeName, dim, err := p.parseType()
		if err != nil {
			return ColumnDef{}, err
		}
		if typeName == "SERIAL" {
			return ColumnDef{}, fmt.Errorf("cannot ADD an auto-increment column: recreate the table")
		}
		col.Type, col.TypeName, col.Dim = typ, typeName, dim
	}
	for {
		switch {
		case p.acceptKw("NOT"):
			if err := p.expectKw("NULL"); err != nil {
				return ColumnDef{}, err
			}
			col.NotNull = true
		case p.acceptKw("NULL"):
			// explicit nullability, the default
		case p.acceptKw("DEFAULT"):
			e, err := p.parseExpr(5)
			if err != nil {
				return ColumnDef{}, err
			}
			col.Default = e
		case p.peekKw("PRIMARY"), p.peekKw("UNIQUE"), p.peekKw("CHECK"):
			return ColumnDef{}, fmt.Errorf("ADD COLUMN takes only NOT NULL and DEFAULT; add keys/uniqueness with CREATE INDEX")
		default:
			return col, nil
		}
	}
}

// --- execution ---------------------------------------------------------------

// execAlter dispatches one ALTER TABLE operation.
func (e *Engine) execAlter(ctx context.Context, s *AlterTable) (ExecResult, error) {
	sc, err := e.loadSchema(ctx, s.Table)
	if err != nil {
		return ExecResult{}, err
	}
	switch s.Op {
	case "add":
		err = e.alterAdd(ctx, sc, s.Col)
	case "drop":
		err = e.alterDrop(ctx, sc, s.Name)
	case "rename_column":
		err = e.alterRenameColumn(ctx, sc, s.Name, s.NewName)
	case "rename_table":
		err = e.alterRenameTable(ctx, sc, s.NewName)
	default:
		err = fmt.Errorf("unsupported ALTER op %q", s.Op)
	}
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Tag: "ALTER TABLE"}, nil
}

// alterAdd appends a column and backfills every existing row (DEFAULT per
// row, else NULL — rows always store every schema column).
func (e *Engine) alterAdd(ctx context.Context, sc *tableSchema, cd ColumnDef) error {
	if _, dup := sc.col(cd.Name); dup {
		return fmt.Errorf("column %q already exists", cd.Name)
	}
	col := column{
		Name: cd.Name, Type: cd.Type, TypeName: cd.TypeName, Dim: cd.Dim,
		NotNull: cd.NotNull, Default: defaultText(cd.Default),
	}
	// A CREATE-time-style sanity check on the default.
	if cd.Default != nil && exprHasSubquery(cd.Default) {
		return fmt.Errorf("subqueries are not allowed in DEFAULT")
	}
	if cd.Default != nil {
		dv, err := evalExpr(cd.Default, nil)
		if err != nil {
			return fmt.Errorf("invalid default for column %q: %v", cd.Name, err)
		}
		if !dv.IsNull() && cd.Type != TypeAny {
			if _, err := CastTo(dv, cd.Type); err != nil {
				return fmt.Errorf("invalid default for column %q: %v", cd.Name, err)
			}
		}
	}
	rows, err := e.scan(ctx, sc)
	if err != nil {
		return err
	}
	if col.NotNull && col.Default == "" && len(rows) > 0 {
		return fmt.Errorf("cannot add NOT NULL column %q without DEFAULT to a non-empty table", cd.Name)
	}
	for _, sr := range rows {
		v := nullV()
		if col.Default != "" {
			// Per row: volatile defaults (now, gen_random_uuid) get a
			// fresh value for each backfilled row, matching INSERT.
			if v, err = evalDefault(col.Default); err != nil {
				return err
			}
			if !v.IsNull() {
				if v, err = CastTo(v, col.Type); err != nil {
					return err
				}
				if v, err = coerceUUIDColumn(col, v); err != nil {
					return err
				}
			}
		}
		sr.data[col.Name] = v
		enc, err := encodeRow(sr.data)
		if err != nil {
			return err
		}
		if err := e.c.Set(ctx, rowKey(e.db, sc.Name, sr.pkhex), enc); err != nil {
			return err
		}
	}
	sc.Columns = append(sc.Columns, col)
	return e.c.Set(ctx, catalogKey(e.db, sc.Name), sc.encode())
}

// alterDrop removes a column after checking nothing depends on it, then
// scrubs it from every row.
func (e *Engine) alterDrop(ctx context.Context, sc *tableSchema, name string) error {
	idx := -1
	for i, c := range sc.Columns {
		if c.Name == name {
			idx = i
		}
	}
	if idx < 0 {
		return fmt.Errorf("no such column: %s", name)
	}
	for _, pk := range sc.PK {
		if pk == name {
			return fmt.Errorf("cannot drop primary key column %q", name)
		}
	}
	for _, ix := range sc.Indexes {
		for _, cn := range ix.Columns {
			if cn == name {
				return fmt.Errorf("column %q is used by index %s; DROP INDEX %s first", name, ix.Name, ix.Name)
			}
		}
	}
	for _, chk := range sc.Checks {
		ex, err := parseExprText(chk.Expr)
		if err != nil {
			continue
		}
		for _, cn := range exprColumns(ex) {
			if cn == name {
				return fmt.Errorf("column %q is used by check constraint %s", name, chk.Name)
			}
		}
	}
	rows, err := e.scan(ctx, sc)
	if err != nil {
		return err
	}
	for _, sr := range rows {
		if _, ok := sr.data[name]; !ok {
			continue
		}
		delete(sr.data, name)
		enc, err := encodeRow(sr.data)
		if err != nil {
			return err
		}
		if err := e.c.Set(ctx, rowKey(e.db, sc.Name, sr.pkhex), enc); err != nil {
			return err
		}
	}
	sc.Columns = append(sc.Columns[:idx], sc.Columns[idx+1:]...)
	return e.c.Set(ctx, catalogKey(e.db, sc.Name), sc.encode())
}

// alterRenameColumn renames a column everywhere it appears: schema, PK
// list, index definitions, CHECK expressions, and every stored row.
func (e *Engine) alterRenameColumn(ctx context.Context, sc *tableSchema, old, nn string) error {
	if _, ok := sc.col(old); !ok {
		return fmt.Errorf("no such column: %s", old)
	}
	if _, dup := sc.col(nn); dup {
		return fmt.Errorf("column %q already exists", nn)
	}
	for i := range sc.Columns {
		if sc.Columns[i].Name == old {
			sc.Columns[i].Name = nn
		}
	}
	for i := range sc.PK {
		if sc.PK[i] == old {
			sc.PK[i] = nn
		}
	}
	for i := range sc.Indexes {
		for j := range sc.Indexes[i].Columns {
			if sc.Indexes[i].Columns[j] == old {
				sc.Indexes[i].Columns[j] = nn
			}
		}
	}
	for i, chk := range sc.Checks {
		ex, err := parseExprText(chk.Expr)
		if err != nil {
			continue
		}
		walkExpr(ex, func(n Expr) {
			if cr, ok := n.(*ColumnRef); ok && cr.Column == old {
				cr.Column = nn
			}
		})
		sc.Checks[i].Expr = ex.String()
	}
	rows, err := e.scan(ctx, sc)
	if err != nil {
		return err
	}
	for _, sr := range rows {
		v, ok := sr.data[old]
		if !ok {
			continue
		}
		delete(sr.data, old)
		sr.data[nn] = v
		enc, err := encodeRow(sr.data)
		if err != nil {
			return err
		}
		if err := e.c.Set(ctx, rowKey(e.db, sc.Name, sr.pkhex), enc); err != nil {
			return err
		}
	}
	return e.c.Set(ctx, catalogKey(e.db, sc.Name), sc.encode())
}

// alterRenameTable moves the catalog record, every row, and every index
// entry under the new name, then removes the old ranges.
func (e *Engine) alterRenameTable(ctx context.Context, sc *tableSchema, nn string) error {
	if _, found, err := e.c.Get(ctx, catalogKey(e.db, nn)); err != nil {
		return err
	} else if found {
		return fmt.Errorf("table %s already exists", nn)
	}
	old := sc.Name
	rows, err := e.scan(ctx, sc)
	if err != nil {
		return err
	}
	for _, sr := range rows {
		enc, err := encodeRow(sr.data)
		if err != nil {
			return err
		}
		if err := e.c.Set(ctx, rowKey(e.db, nn, sr.pkhex), enc); err != nil {
			return err
		}
	}
	// Index entries keep their value (the pkhex); only the key prefix
	// carries the table name.
	oldIdx := indexPrefix(e.db, old)
	newIdx := indexPrefix(e.db, nn)
	cursor := ""
	for {
		entries, next, err := e.c.List(ctx, oldIdx, cursor, scanBatch)
		if err != nil {
			return err
		}
		for _, ent := range entries {
			if err := e.c.Set(ctx, newIdx+strings.TrimPrefix(ent.Key, oldIdx), ent.Value); err != nil {
				return err
			}
		}
		if next == "" || len(entries) == 0 {
			break
		}
		cursor = next
	}
	sc.Name = nn
	if err := e.c.Set(ctx, catalogKey(e.db, nn), sc.encode()); err != nil {
		return err
	}
	if err := e.c.DeleteRange(ctx, tablePrefix(e.db, old), prefixEnd(tablePrefix(e.db, old))); err != nil {
		return err
	}
	if err := e.c.DeleteRange(ctx, oldIdx, prefixEnd(oldIdx)); err != nil {
		return err
	}
	return e.c.Delete(ctx, catalogKey(e.db, old))
}
