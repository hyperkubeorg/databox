// exec.go is the SQL executor: it turns parsed statements into KV reads and
// writes against a databox cluster through the official Go client
// (§13). Every statement runs inside one distributed
// transaction at snapshot isolation; DDL and DML stage their changes in a
// client-side transaction (§10) and commit atomically, so a failed INSERT
// leaves no half-written row or dangling index entry.
//
// Table and index scans become ranged List calls with page-at-a-time
// prefetch (scanBatch keys per round trip), so row-at-a-time execution
// never degrades into a round trip per row.
package sql

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// scanBatch is the read-ahead page size for table/index scans (§13).
const scanBatch = 1000

// Engine executes SQL statements for one connected session. It holds the
// authenticated cluster store (so grants apply to every underlying KV
// operation) and the session's current database name.
type Engine struct {
	c  kvStore
	db string
	// noPlanner forces every statement onto the full-scan path; the planner
	// equivalence tests use it as the trusted baseline.
	noPlanner bool
	// noTopK disables the vector top-k heap (vector.go), forcing ORDER
	// BY-distance-LIMIT queries through the ordinary sort; the top-k
	// equivalence tests use it as the trusted baseline.
	noTopK bool
	// lastPlan records the access path the most recent planned scan chose,
	// so tests can assert the path without re-implementing plan selection.
	lastPlan *scanPlan
}

// NewEngine builds an executor over an authenticated cluster client and
// database name. The client is wrapped in the kvStore adapter so the engine
// core has no direct dependency on the HTTP client.
func NewEngine(c *client.Client, db string) *Engine {
	return NewEngineWithStore(NewClientStore(c), db)
}

// NewEngineWithStore builds an executor over any kvStore (the in-memory
// store backs the dialect-conformance tests).
func NewEngineWithStore(store kvStore, db string) *Engine {
	if db == "" {
		db = "databox"
	}
	return &Engine{c: store, db: db}
}

// ExecResult is the outcome of one statement: a command tag plus, for
// row-returning statements, the column names and text-form rows (a nil
// entry is SQL NULL, distinct from an empty string).
type ExecResult struct {
	Tag     string
	Columns []string
	Rows    [][]*string
	// typed carries the same rows as Values, before text rendering. The
	// conformance runner compares against it so INTEGER 1 and DOUBLE 1.0
	// stay distinguishable the way chai's own assertions keep them.
	typed [][]Value
}

// Exec parses and executes a SQL string, returning one ExecResult per
// statement. Parsing the whole string first means a syntax error aborts
// before any statement runs.
func (e *Engine) Exec(ctx context.Context, sql string) ([]ExecResult, error) {
	stmts, err := ParseStatements(sql)
	if err != nil {
		return nil, err
	}
	out := make([]ExecResult, 0, len(stmts))
	for _, st := range stmts {
		res, err := e.execOne(ctx, st)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// execOne dispatches a single statement.
func (e *Engine) execOne(ctx context.Context, st Statement) (ExecResult, error) {
	switch s := st.(type) {
	case *CreateTable:
		return e.execCreateTable(ctx, s)
	case *DropTable:
		return e.execDropTable(ctx, s)
	case *CreateIndex:
		return e.execCreateIndex(ctx, s)
	case *DropIndex:
		return e.execDropIndex(ctx, s)
	case *Insert:
		return e.execInsert(ctx, s)
	case *Update:
		return e.execUpdate(ctx, s)
	case *Delete:
		return e.execDelete(ctx, s)
	case *Select:
		return e.execSelect(ctx, s)
	case *TxStmt:
		// Autocommit engine: BEGIN/COMMIT/ROLLBACK are accepted no-ops for
		// driver compatibility (§13 / ast.go TxStmt).
		return ExecResult{Tag: s.Keyword}, nil
	case *Show:
		return e.execShow(ctx, s)
	case *AlterTable:
		return e.execAlter(ctx, s)
	}
	return ExecResult{}, fmt.Errorf("unsupported statement %T", st)
}

// --- catalog access ----------------------------------------------------------

// loadSchema fetches a table's catalog record.
func (e *Engine) loadSchema(ctx context.Context, table string) (*tableSchema, error) {
	ent, found, err := e.c.Get(ctx, catalogKey(e.db, table))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("no such table: %s", table)
	}
	return decodeSchema(ent)
}

// --- DDL ---------------------------------------------------------------------

// execCreateTable writes the catalog record for a new table, honoring
// IF NOT EXISTS and inline/table-level PRIMARY KEY and UNIQUE constraints.
func (e *Engine) execCreateTable(ctx context.Context, s *CreateTable) (ExecResult, error) {
	if _, found, err := e.c.Get(ctx, catalogKey(e.db, s.Name)); err != nil {
		return ExecResult{}, err
	} else if found {
		if s.IfNotExists {
			return ExecResult{Tag: "CREATE TABLE"}, nil
		}
		return ExecResult{}, fmt.Errorf("table %s already exists", s.Name)
	}
	sc := &tableSchema{Name: s.Name}
	var pk []string
	uniqueSets := map[string]bool{} // column sets already covered by a UNIQUE
	for _, cd := range s.Columns {
		if _, dup := sc.col(cd.Name); dup {
			return ExecResult{}, fmt.Errorf("column %q already defined", cd.Name)
		}
		sc.Columns = append(sc.Columns, column{
			Name: cd.Name, Type: cd.Type, TypeName: cd.TypeName, Dim: cd.Dim,
			NotNull: cd.NotNull || cd.PrimaryKey, Unique: cd.Unique,
			Auto:    cd.Auto,
			Default: defaultText(cd.Default),
		})
		if cd.Auto && cd.Type != TypeInt {
			return ExecResult{}, fmt.Errorf("column %q: AUTO_INCREMENT requires an INTEGER column", cd.Name)
		}
		// Vectors never enter the order-preserving key encoding: no PK, no
		// unique index (which is an index under the hood).
		if cd.Type == TypeVector && (cd.PrimaryKey || cd.Unique) {
			return ExecResult{}, errVectorIndex()
		}
		if cd.PrimaryKey {
			pk = append(pk, cd.Name)
		}
		if cd.Unique && !cd.PrimaryKey {
			uniqueSets[cd.Name] = true
			sc.Indexes = append(sc.Indexes, indexDef{
				Name: "uniq_" + cd.Name, Columns: []string{cd.Name}, Unique: true,
			})
		}
		// A DEFAULT must be evaluable without a row and compatible with the
		// column's type, checked now so a bad default fails the CREATE, not
		// some later INSERT (CREATE_TABLE/default.sql).
		if cd.Default != nil && exprHasSubquery(cd.Default) {
			return ExecResult{}, fmt.Errorf("subqueries are not allowed in DEFAULT")
		}
		if cd.Default != nil {
			dv, err := evalExpr(cd.Default, nil)
			if err != nil {
				return ExecResult{}, fmt.Errorf("invalid default for column %q: %v", cd.Name, err)
			}
			if !dv.IsNull() && cd.Type != TypeAny {
				if _, err := CastTo(dv, cd.Type); err != nil {
					return ExecResult{}, fmt.Errorf("invalid default for column %q: %v", cd.Name, err)
				}
			}
		}
	}
	// One primary key per table: rejecting a second inline PRIMARY KEY or an
	// inline one combined with a table-level constraint (chai's wording).
	if len(pk) > 1 || (len(pk) > 0 && len(s.PrimaryKey) > 0) {
		return ExecResult{}, fmt.Errorf("multiple primary keys for table %q are not allowed", s.Name)
	}
	if len(s.PrimaryKey) > 0 {
		pk = s.PrimaryKey
	}
	sc.PK = pk
	// Table-level UNIQUE(...) constraints become unique indexes. Columns
	// must exist (and must not be vectors — a unique index would key-encode
	// them); a duplicate constraint over the same set is an error.
	for i, u := range s.Uniques {
		for _, cn := range u {
			col, ok := sc.col(cn)
			if !ok {
				return ExecResult{}, fmt.Errorf("no such column %q in UNIQUE constraint", cn)
			}
			if col.Type == TypeVector {
				return ExecResult{}, errVectorIndex()
			}
		}
		set := strings.Join(u, "\x00")
		if uniqueSets[set] {
			return ExecResult{}, fmt.Errorf("duplicate UNIQUE constraint on (%s)", strings.Join(u, ", "))
		}
		uniqueSets[set] = true
		sc.Indexes = append(sc.Indexes, indexDef{
			Name: fmt.Sprintf("uniq_%d_%s", i, strings.Join(u, "_")), Columns: u, Unique: true,
		})
	}
	// CHECK constraints: named <table>_check, <table>_check1, ... exactly
	// like chai, stored as SQL text and re-parsed at enforcement time. Any
	// referenced column must exist.
	for i, chk := range s.Checks {
		if exprHasSubquery(chk) {
			return ExecResult{}, fmt.Errorf("subqueries are not allowed in CHECK constraints")
		}
		for _, cn := range exprColumns(chk) {
			if _, ok := sc.col(cn); !ok {
				return ExecResult{}, fmt.Errorf("no such column %q in CHECK constraint", cn)
			}
		}
		name := s.Name + "_check"
		if i > 0 {
			name = fmt.Sprintf("%s_check%d", s.Name, i)
		}
		sc.Checks = append(sc.Checks, checkDef{Name: name, Expr: chk.String()})
	}
	// An auto-increment column must BE the single-column primary key: its
	// value is the row's storage key, and the counter only guards one
	// column per table.
	for _, c := range sc.Columns {
		if c.Auto && (len(pk) != 1 || pk[0] != c.Name) {
			return ExecResult{}, fmt.Errorf("column %q: AUTO_INCREMENT column must be the primary key", c.Name)
		}
	}
	// Validate that PK columns exist — and that none is a vector: the
	// primary key IS the row's storage key, and vectors have no
	// order-preserving encoding (keyenc.go).
	for _, name := range pk {
		col, ok := sc.col(name)
		if !ok {
			return ExecResult{}, fmt.Errorf("primary key column %q does not exist", name)
		}
		if col.Type == TypeVector {
			return ExecResult{}, errVectorIndex()
		}
	}
	if err := e.c.Set(ctx, catalogKey(e.db, s.Name), sc.encode()); err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Tag: "CREATE TABLE"}, nil
}

// exprColumns collects every column name an expression references, for
// DDL-time validation of CHECK constraints.
func exprColumns(e Expr) []string {
	var out []string
	var walk func(Expr)
	walk = func(e Expr) {
		switch x := e.(type) {
		case *ColumnRef:
			out = append(out, x.Column)
		case *Unary:
			walk(x.X)
		case *Binary:
			walk(x.L)
			walk(x.R)
		case *Between:
			walk(x.X)
			walk(x.Lo)
			walk(x.Hi)
		case *InList:
			walk(x.X)
			for _, item := range x.List {
				walk(item)
			}
		case *IsNull:
			walk(x.X)
		case *Cast:
			walk(x.X)
		case *FuncCall:
			for _, a := range x.Args {
				walk(a)
			}
		}
	}
	walk(e)
	return out
}

// defaultText renders a DEFAULT expression to storable SQL text.
func defaultText(e Expr) string {
	if e == nil {
		return ""
	}
	return e.String()
}

// execDropTable removes a table's catalog record, all its rows, and all its
// index entries in one transaction (DeleteRange backs the bulk removal).
func (e *Engine) execDropTable(ctx context.Context, s *DropTable) (ExecResult, error) {
	if _, found, err := e.c.Get(ctx, catalogKey(e.db, s.Name)); err != nil {
		return ExecResult{}, err
	} else if !found {
		if s.IfExists {
			return ExecResult{Tag: "DROP TABLE"}, nil
		}
		return ExecResult{}, fmt.Errorf("no such table: %s", s.Name)
	}
	// Rows and index entries are contiguous ranges; DeleteRange clears each.
	if err := e.c.DeleteRange(ctx, tablePrefix(e.db, s.Name), prefixEnd(tablePrefix(e.db, s.Name))); err != nil {
		return ExecResult{}, err
	}
	if err := e.c.DeleteRange(ctx, indexPrefix(e.db, s.Name), prefixEnd(indexPrefix(e.db, s.Name))); err != nil {
		return ExecResult{}, err
	}
	if err := e.c.Delete(ctx, catalogKey(e.db, s.Name)); err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Tag: "DROP TABLE"}, nil
}

// execCreateIndex adds an index to a table's catalog and backfills entries
// for existing rows.
func (e *Engine) execCreateIndex(ctx context.Context, s *CreateIndex) (ExecResult, error) {
	sc, err := e.loadSchema(ctx, s.Table)
	if err != nil {
		return ExecResult{}, err
	}
	name := s.Name
	if name == "" {
		// Auto-generated names follow chai: <table>_<cols>_idx, then a
		// numeric suffix until the name is free (test_a_idx, test_a_idx1...).
		base := s.Table + "_" + strings.Join(s.Columns, "_") + "_idx"
		name = base
		for n := 1; indexNameTaken(sc, name); n++ {
			name = fmt.Sprintf("%s%d", base, n)
		}
	}
	for _, idx := range sc.Indexes {
		if idx.Name == name {
			if s.IfNotExists {
				return ExecResult{Tag: "CREATE INDEX"}, nil
			}
			return ExecResult{}, fmt.Errorf("index %s already exists", name)
		}
	}
	for _, cn := range s.Columns {
		col, ok := sc.col(cn)
		if !ok {
			return ExecResult{}, fmt.Errorf("index column %q does not exist", cn)
		}
		// Index entries are order-preserving key encodings (keyenc.go);
		// vectors have none, and exact KNN needs no index anyway.
		if col.Type == TypeVector {
			return ExecResult{}, errVectorIndex()
		}
	}
	idx := indexDef{Name: name, Columns: s.Columns, Unique: s.Unique}
	// Backfill: scan existing rows and write an index entry for each.
	rows, err := e.scan(ctx, sc)
	if err != nil {
		return ExecResult{}, err
	}
	seen := map[string]string{}
	for _, sr := range rows {
		keyhex, err := indexKeyHex(idx, sr.data)
		if err != nil {
			return ExecResult{}, err
		}
		// NULL keys never collide, same as at insert time.
		if s.Unique && !rowHasNullIn(idx.Columns, sr.data) {
			if _, dup := seen[keyhex]; dup {
				return ExecResult{}, uniqueErr(idx)
			}
			seen[keyhex] = sr.pkhex
		}
		if err := e.c.Set(ctx, indexEntryKey(e.db, s.Table, name, keyhex, sr.pkhex), []byte(sr.pkhex)); err != nil {
			return ExecResult{}, err
		}
	}
	sc.Indexes = append(sc.Indexes, idx)
	if err := e.c.Set(ctx, catalogKey(e.db, s.Table), sc.encode()); err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Tag: "CREATE INDEX"}, nil
}

// execDropIndex removes an index by name across all tables (index names are
// unique per table; we locate the owning table by scanning the catalog).
func (e *Engine) execDropIndex(ctx context.Context, s *DropIndex) (ExecResult, error) {
	tables, err := e.listTables(ctx)
	if err != nil {
		return ExecResult{}, err
	}
	for _, tn := range tables {
		sc, err := e.loadSchema(ctx, tn)
		if err != nil {
			return ExecResult{}, err
		}
		for i, idx := range sc.Indexes {
			if idx.Name == s.Name {
				if err := e.c.DeleteRange(ctx, oneIndexPrefix(e.db, tn, s.Name), prefixEnd(oneIndexPrefix(e.db, tn, s.Name))); err != nil {
					return ExecResult{}, err
				}
				sc.Indexes = append(sc.Indexes[:i], sc.Indexes[i+1:]...)
				if err := e.c.Set(ctx, catalogKey(e.db, tn), sc.encode()); err != nil {
					return ExecResult{}, err
				}
				return ExecResult{Tag: "DROP INDEX"}, nil
			}
		}
	}
	if s.IfExists {
		return ExecResult{Tag: "DROP INDEX"}, nil
	}
	return ExecResult{}, fmt.Errorf("no such index: %s", s.Name)
}

// indexNameTaken reports whether a table already has an index by name.
func indexNameTaken(sc *tableSchema, name string) bool {
	for _, idx := range sc.Indexes {
		if idx.Name == name {
			return true
		}
	}
	return false
}

// listTables enumerates the table names in the current database.
func (e *Engine) listTables(ctx context.Context) ([]string, error) {
	var names []string
	cursor := ""
	for {
		entries, next, err := e.c.List(ctx, catalogPrefix(e.db), cursor, scanBatch)
		if err != nil {
			return nil, err
		}
		for _, ent := range entries {
			names = append(names, tableFromCatalogKey(ent.Key, e.db))
		}
		if next == "" || len(entries) == 0 {
			break
		}
		cursor = next
	}
	return names, nil
}

// --- scanning ----------------------------------------------------------------

// storedRow is one row read from the table: its decoded columns plus the
// hex primary-key that names its storage key (needed by UPDATE/DELETE and
// index maintenance).
type storedRow struct {
	data  row
	pkhex string
}

// scan reads every row of a table, paging with read-ahead prefetch.
func (e *Engine) scan(ctx context.Context, sc *tableSchema) ([]storedRow, error) {
	prefix := tablePrefix(e.db, sc.Name)
	var out []storedRow
	cursor := ""
	for {
		entries, next, err := e.c.List(ctx, prefix, cursor, scanBatch)
		if err != nil {
			return nil, err
		}
		for _, ent := range entries {
			cols, err := decodeRow(ent.Value)
			if err != nil {
				return nil, err
			}
			out = append(out, storedRow{data: cols, pkhex: strings.TrimPrefix(ent.Key, prefix)})
		}
		if next == "" || len(entries) == 0 {
			break
		}
		cursor = next
	}
	return out, nil
}

// --- helpers -----------------------------------------------------------------

// prefixEnd returns the exclusive upper bound for a prefix range: the
// prefix with its last byte incremented, matching DeleteRange semantics.
func prefixEnd(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return "" // unbounded (all keys ≥ prefix)
}

// indexKeyHex builds the encoded index key for a row's index columns.
func indexKeyHex(idx indexDef, r row) (string, error) {
	vals := make([]Value, len(idx.Columns))
	for i, cn := range idx.Columns {
		vals[i] = r[cn]
	}
	return encodeKey(vals...)
}

// pkHexFor computes the storage key hex for a row given the schema's PK,
// or generates the next rowid when the table has no declared primary key.
func (e *Engine) pkHexFor(sc *tableSchema, r row, rowid int64) (string, error) {
	if !sc.hasPK() {
		return encodeKey(intV(rowid))
	}
	vals := make([]Value, len(sc.PK))
	for i, cn := range sc.PK {
		v := r[cn]
		if v.IsNull() {
			return "", fmt.Errorf("primary key column %q cannot be NULL", cn)
		}
		vals[i] = v
	}
	return encodeKey(vals...)
}

// orderColumns returns the schema's column names in declaration order, used
// as the default projection for SELECT *.
func orderColumns(sc *tableSchema) []string {
	names := make([]string, len(sc.Columns))
	for i, c := range sc.Columns {
		names[i] = c.Name
	}
	return names
}

// stableSortRows sorts rows by the ORDER BY keys using the total order in
// value.go (sortCompare), so heterogeneous columns never fail mid-sort.
func stableSortRows(rows []row, keys []OrderKey) error {
	var sortErr error
	sort.SliceStable(rows, func(i, j int) bool {
		for _, k := range keys {
			av, err := evalExpr(k.Expr, rows[i])
			if err != nil {
				sortErr = err
				return false
			}
			bv, err := evalExpr(k.Expr, rows[j])
			if err != nil {
				sortErr = err
				return false
			}
			c := sortCompare(av, bv)
			if c == 0 {
				continue
			}
			if k.Desc {
				return c > 0
			}
			return c < 0
		}
		return false
	})
	return sortErr
}
