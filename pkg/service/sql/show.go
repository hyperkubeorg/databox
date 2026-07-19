// show.go is the schema-introspection extension: SHOW TABLES / DATABASES /
// COLUMNS / INDEXES / CREATE TABLE, plus DESCRIBE as a synonym for SHOW
// COLUMNS. These are dialect extensions (chai has none — its
// __chai_catalog is explicitly out of scope, §13), so none of their words
// are reserved: "show", "tables", … lex as ordinary identifiers and only
// gain meaning at statement-start position. A column named "show" keeps
// working everywhere else, and the conformance corpus is untouched.
//
// Everything reads the ordinary catalog keys (catalog.go), so grants apply:
// you can only inspect schemas you could read anyway.
package sql

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Show is one introspection statement. Kind is "tables", "databases",
// "columns", "indexes" or "create"; Table is set for the table-scoped
// kinds (optional for "indexes": empty = every table).
type Show struct {
	Kind  string
	Table string
}

func (*Show) stmtNode() {}

// --- parsing -----------------------------------------------------------------

// parseShow parses everything after the SHOW word.
func (p *parser) parseShow() (Statement, error) {
	p.next() // SHOW
	t := p.peek()
	switch {
	case t.kind == tkIdent && t.s == "tables":
		p.next()
		return &Show{Kind: "tables"}, nil
	case t.kind == tkIdent && t.s == "databases":
		p.next()
		return &Show{Kind: "databases"}, nil
	case t.kind == tkIdent && t.s == "columns":
		p.next()
		if err := p.expectKw("FROM"); err != nil {
			return nil, err
		}
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		return &Show{Kind: "columns", Table: name}, nil
	case t.kind == tkIdent && (t.s == "indexes" || t.s == "index"):
		p.next()
		s := &Show{Kind: "indexes"}
		if p.acceptKw("FROM") {
			name, err := p.ident()
			if err != nil {
				return nil, err
			}
			s.Table = name
		}
		return s, nil
	case t.kind == tkKeyword && t.s == "CREATE":
		p.next()
		if err := p.expectKw("TABLE"); err != nil {
			return nil, err
		}
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		return &Show{Kind: "create", Table: name}, nil
	}
	return nil, errAt(t.pos, "expected TABLES, DATABASES, COLUMNS FROM, INDEXES or CREATE TABLE after SHOW, found %q", t.s)
}

// parseDescribe parses the table name after DESCRIBE/DESC.
func (p *parser) parseDescribe() (Statement, error) {
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	return &Show{Kind: "columns", Table: name}, nil
}

// --- execution ---------------------------------------------------------------

// execShow dispatches one introspection statement.
func (e *Engine) execShow(ctx context.Context, s *Show) (ExecResult, error) {
	switch s.Kind {
	case "tables":
		return e.showTables(ctx)
	case "databases":
		return e.showDatabases(ctx)
	case "columns":
		return e.showColumns(ctx, s.Table)
	case "indexes":
		return e.showIndexes(ctx, s.Table)
	case "create":
		return e.showCreate(ctx, s.Table)
	}
	return ExecResult{}, fmt.Errorf("unsupported SHOW kind %q", s.Kind)
}

func sptr(s string) *string { return &s }

func (e *Engine) showTables(ctx context.Context) (ExecResult, error) {
	names, err := e.listTables(ctx) // catalog key order = sorted
	if err != nil {
		return ExecResult{}, err
	}
	res := ExecResult{Tag: "SHOW", Columns: []string{"table"}}
	for _, n := range names {
		res.Rows = append(res.Rows, []*string{sptr(n)})
	}
	return res, nil
}

// showDatabases lists the distinct <db> segments under /sql/. It sees only
// databases whose keys the session may list.
func (e *Engine) showDatabases(ctx context.Context) (ExecResult, error) {
	seen := map[string]bool{}
	cursor := ""
	for {
		entries, next, err := e.c.List(ctx, sqlRoot, cursor, scanBatch)
		if err != nil {
			return ExecResult{}, err
		}
		for _, ent := range entries {
			rest := strings.TrimPrefix(ent.Key, sqlRoot)
			if db, _, ok := strings.Cut(rest, "/"); ok && db != "" {
				seen[db] = true
			}
		}
		if next == "" || len(entries) == 0 {
			break
		}
		cursor = next
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	res := ExecResult{Tag: "SHOW", Columns: []string{"database"}}
	for _, n := range names {
		res.Rows = append(res.Rows, []*string{sptr(n)})
	}
	return res, nil
}

// columnTypeText renders a column's declared type, with the vector
// dimension when one applies.
func columnTypeText(c column) string {
	if c.Dim > 0 && !strings.Contains(c.TypeName, "(") {
		return fmt.Sprintf("%s(%d)", c.TypeName, c.Dim)
	}
	return c.TypeName
}

func (e *Engine) showColumns(ctx context.Context, table string) (ExecResult, error) {
	sc, err := e.loadSchema(ctx, table)
	if err != nil {
		return ExecResult{}, err
	}
	pk := map[string]bool{}
	for _, n := range sc.PK {
		pk[n] = true
	}
	res := ExecResult{Tag: "SHOW", Columns: []string{"column", "type", "nullable", "key", "default", "extra"}}
	for _, c := range sc.Columns {
		nullable := "YES"
		if c.NotNull {
			nullable = "NO"
		}
		key := ""
		switch {
		case pk[c.Name]:
			key = "PRI"
		case c.Unique:
			key = "UNI"
		}
		var def *string
		if c.Default != "" {
			def = sptr(c.Default)
		}
		extra := ""
		if c.Auto {
			extra = "auto_increment"
		}
		res.Rows = append(res.Rows, []*string{
			sptr(c.Name), sptr(columnTypeText(c)), sptr(nullable), sptr(key), def, sptr(extra),
		})
	}
	return res, nil
}

// showIndexes lists secondary indexes (a PRIMARY pseudo-row represents the
// declared primary key, which is the row storage key, not a real index).
func (e *Engine) showIndexes(ctx context.Context, table string) (ExecResult, error) {
	tables := []string{table}
	if table == "" {
		var err error
		if tables, err = e.listTables(ctx); err != nil {
			return ExecResult{}, err
		}
	}
	res := ExecResult{Tag: "SHOW", Columns: []string{"table", "index", "columns", "unique"}}
	for _, tn := range tables {
		sc, err := e.loadSchema(ctx, tn)
		if err != nil {
			return ExecResult{}, err
		}
		if sc.hasPK() {
			res.Rows = append(res.Rows, []*string{
				sptr(tn), sptr("PRIMARY"), sptr(strings.Join(sc.PK, ", ")), sptr("true"),
			})
		}
		for _, idx := range sc.Indexes {
			res.Rows = append(res.Rows, []*string{
				sptr(tn), sptr(idx.Name), sptr(strings.Join(idx.Columns, ", ")), sptr(fmt.Sprint(idx.Unique)),
			})
		}
	}
	return res, nil
}

// showCreate renders canonical DDL for a table: the CREATE TABLE (columns,
// PRIMARY KEY, CHECKs) followed by one CREATE INDEX per secondary index
// that a column-level UNIQUE didn't already imply. It is a faithful
// canonical form, not the original statement text (which is not stored).
func (e *Engine) showCreate(ctx context.Context, table string) (ExecResult, error) {
	sc, err := e.loadSchema(ctx, table)
	if err != nil {
		return ExecResult{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (", sc.Name)
	// Column-level UNIQUE creates a uniq_<col> index (execCreateTable);
	// those render on the column, not as a separate CREATE INDEX.
	colUnique := map[string]bool{}
	for _, c := range sc.Columns {
		if c.Unique {
			colUnique["uniq_"+c.Name] = true
		}
	}
	for i, c := range sc.Columns {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "\n  %s %s", c.Name, columnTypeText(c))
		if c.Auto {
			b.WriteString(" AUTO_INCREMENT")
		}
		if c.NotNull && !(len(sc.PK) == 1 && sc.PK[0] == c.Name) {
			b.WriteString(" NOT NULL")
		}
		if c.Unique {
			b.WriteString(" UNIQUE")
		}
		if c.Default != "" {
			fmt.Fprintf(&b, " DEFAULT %s", c.Default)
		}
	}
	if sc.hasPK() {
		fmt.Fprintf(&b, ",\n  PRIMARY KEY (%s)", strings.Join(sc.PK, ", "))
	}
	for _, chk := range sc.Checks {
		fmt.Fprintf(&b, ",\n  CONSTRAINT %s CHECK (%s)", chk.Name, chk.Expr)
	}
	b.WriteString("\n);")
	for _, idx := range sc.Indexes {
		if colUnique[idx.Name] {
			continue
		}
		uniq := ""
		if idx.Unique {
			uniq = "UNIQUE "
		}
		fmt.Fprintf(&b, "\nCREATE %sINDEX %s ON %s (%s);", uniq, idx.Name, sc.Name, strings.Join(idx.Columns, ", "))
	}
	return ExecResult{
		Tag:     "SHOW",
		Columns: []string{"table", "create_table"},
		Rows:    [][]*string{{sptr(sc.Name), sptr(b.String())}},
	}, nil
}
