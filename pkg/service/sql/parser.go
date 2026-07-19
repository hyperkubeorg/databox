// parser.go turns tokens into AST nodes. It is a hand-written recursive
// descent parser with Pratt-style operator precedence cloned from chai's
// grammar (REFERENCES/chai/internal/sql/parser). The v1 statement set:
//
//	CREATE TABLE / DROP TABLE / CREATE INDEX / DROP INDEX / ALTER TABLE
//	INSERT (VALUES and SELECT sources, RETURNING)
//	SELECT (WHERE, GROUP BY, ORDER BY, LIMIT/OFFSET, DISTINCT, UNION,
//	        the JOIN extension incl. derived tables and LATERAL — join.go,
//	        and expression subqueries — subquery.go)
//	UPDATE / DELETE
//	BEGIN/COMMIT/ROLLBACK (accepted, no-op — the engine is autocommit)
//	SHOW/DESCRIBE introspection (show.go)
//
// chai's dialect is the cloned baseline; joins, ALTER TABLE and
// introspection are extensions past its boundary, built on unreserved
// words so the conformance corpus is untouched.
package sql

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// parser holds the token stream and a cursor.
type parser struct {
	toks []token
	i    int
}

// ParseStatements parses a script of semicolon-separated statements.
func ParseStatements(src string) ([]Statement, error) {
	lx := &lexer{src: src}
	var toks []token
	for {
		t, err := lx.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
		if t.kind == tkEOF {
			break
		}
	}
	p := &parser{toks: toks}
	var out []Statement
	for {
		// Swallow empty statements (";;", trailing ";").
		for p.acceptOp(";") {
		}
		if p.peek().kind == tkEOF {
			return out, nil
		}
		st, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		out = append(out, st)
		// A statement ends at ";" or EOF; anything else is trailing junk.
		// The wording mirrors chai's scanner ("expected EOF, got ::") which
		// the expr conformance corpus asserts on.
		if !p.acceptOp(";") && p.peek().kind != tkEOF {
			return nil, errAt(p.peek().pos, "expected EOF, got %s", p.peek().s)
		}
	}
}

// --- token helpers ----------------------------------------------------------

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) next() token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

// acceptKw consumes the keyword if it is next and reports whether it did.
func (p *parser) acceptKw(kw string) bool {
	if t := p.peek(); t.kind == tkKeyword && t.s == kw {
		p.next()
		return true
	}
	return false
}

// peekKw reports whether the next token is the given keyword.
func (p *parser) peekKw(kw string) bool {
	t := p.peek()
	return t.kind == tkKeyword && t.s == kw
}

// expectKw consumes the keyword or fails with a syntax error.
func (p *parser) expectKw(kw string) error {
	if !p.acceptKw(kw) {
		return errAt(p.peek().pos, "expected %s, found %q", kw, p.peek().s)
	}
	return nil
}

// acceptOp consumes the operator/punctuation token if it is next.
func (p *parser) acceptOp(op string) bool {
	if t := p.peek(); t.kind == tkOp && t.s == op {
		p.next()
		return true
	}
	return false
}

// expectOp consumes the operator or fails.
func (p *parser) expectOp(op string) error {
	if !p.acceptOp(op) {
		return errAt(p.peek().pos, "expected %q, found %q", op, p.peek().s)
	}
	return nil
}

// ident consumes an identifier (bare or quoted) and returns its name.
func (p *parser) ident() (string, error) {
	t := p.peek()
	if t.kind == tkIdent {
		p.next()
		return t.s, nil
	}
	return "", errAt(t.pos, "expected identifier, found %q", t.s)
}

// identLabel consumes an identifier but returns its label spelling: the
// original case for bare words, because aliases keep the case the user
// typed ("RETURNING b AS B" labels the column "B").
func (p *parser) identLabel() (string, error) {
	t := p.peek()
	if t.kind == tkIdent {
		p.next()
		return labelOf(t), nil
	}
	return "", errAt(t.pos, "expected identifier, found %q", t.s)
}

// --- statements ---------------------------------------------------------------

func (p *parser) parseStatement() (Statement, error) {
	t := p.peek()
	// Introspection words are NOT reserved (show.go): they lex as
	// identifiers and only mean anything at statement-start position.
	if t.kind == tkIdent {
		switch t.s {
		case "show":
			return p.parseShow()
		case "describe":
			p.next()
			return p.parseDescribe()
		case "alter":
			return p.parseAlter()
		}
	}
	if t.kind != tkKeyword {
		return nil, errAt(t.pos, "expected a statement, found %q", t.s)
	}
	switch t.s {
	case "DESC": // DESC <table> — the DESCRIBE shorthand; ORDER BY's DESC never starts a statement
		p.next()
		return p.parseDescribe()
	case "SELECT":
		return p.parseSelect()
	case "INSERT":
		return p.parseInsert()
	case "UPDATE":
		return p.parseUpdate()
	case "DELETE":
		return p.parseDelete()
	case "CREATE":
		return p.parseCreate()
	case "DROP":
		return p.parseDrop()
	case "BEGIN", "COMMIT", "ROLLBACK":
		p.next()
		// Optional TRANSACTION noise word ("BEGIN TRANSACTION").
		p.acceptKw("TRANSACTION")
		return &TxStmt{Keyword: t.s}, nil
	}
	return nil, errAt(t.pos, "unsupported statement %q", t.s)
}

// parseCreate dispatches CREATE TABLE vs CREATE [UNIQUE] INDEX.
func (p *parser) parseCreate() (Statement, error) {
	p.next() // CREATE
	switch {
	case p.acceptKw("TABLE"):
		return p.parseCreateTable()
	case p.acceptKw("UNIQUE"):
		if err := p.expectKw("INDEX"); err != nil {
			return nil, err
		}
		return p.parseCreateIndex(true)
	case p.acceptKw("INDEX"):
		return p.parseCreateIndex(false)
	}
	return nil, errAt(p.peek().pos, "expected TABLE or INDEX after CREATE")
}

func (p *parser) parseCreateTable() (Statement, error) {
	st := &CreateTable{}
	if p.acceptKw("IF") {
		if err := p.expectKw("NOT"); err != nil {
			return nil, err
		}
		if err := p.expectKw("EXISTS"); err != nil {
			return nil, err
		}
		st.IfNotExists = true
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	st.Name = name
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	for {
		if err := p.parseTableItem(st); err != nil {
			return nil, err
		}
		if p.acceptOp(",") {
			continue
		}
		break
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return st, nil
}

// parseTableItem parses one column definition or table-level constraint.
func (p *parser) parseTableItem(st *CreateTable) error {
	// Table-level constraints first: [CONSTRAINT name] PRIMARY KEY(...) /
	// UNIQUE(...) / CHECK(...). Constraint names parse but are not stored;
	// the executor names constraints the way chai does.
	if p.acceptKw("CONSTRAINT") {
		if _, err := p.ident(); err != nil { // constraint name, unused
			return err
		}
	}
	switch {
	case p.acceptKw("PRIMARY"):
		if err := p.expectKw("KEY"); err != nil {
			return err
		}
		cols, err := p.parseIdentList()
		if err != nil {
			return err
		}
		if len(st.PrimaryKey) > 0 {
			return fmt.Errorf("multiple PRIMARY KEY constraints")
		}
		st.PrimaryKey = cols
		return nil
	case p.acceptKw("UNIQUE"):
		cols, err := p.parseIdentList()
		if err != nil {
			return err
		}
		st.Uniques = append(st.Uniques, cols)
		return nil
	case p.acceptKw("CHECK"):
		e, err := p.parseParenExpr()
		if err != nil {
			return err
		}
		st.Checks = append(st.Checks, e)
		return nil
	}

	// Otherwise: a column definition. The type is optional (chai allows
	// "a NOT NULL"); a typeless column stores any value uncoerced.
	name, err := p.ident()
	if err != nil {
		return err
	}
	col := ColumnDef{Name: name, Type: TypeAny}
	if p.peek().kind == tkIdent {
		typ, typeName, dim, err := p.parseType()
		if err != nil {
			return err
		}
		col.Type, col.TypeName, col.Dim = typ, typeName, dim
		if typeName == "SERIAL" {
			// SERIAL is INTEGER + auto-increment; the stored type name is
			// the storage type, matching what DESCRIBE reports.
			col.TypeName, col.Auto = "INTEGER", true
		}
	}
	// Inline constraints repeat in any order; repeating the same one is an
	// error (CREATE_TABLE corpus: "NOT NULL NOT NULL" and "PRIMARY KEY
	// PRIMARY KEY" must fail).
	for {
		switch {
		case p.peek().kind == tkIdent && (p.peek().s == "auto_increment" || p.peek().s == "autoincrement"):
			// Unreserved attribute words (MySQL/SQLite spellings) — only
			// meaningful in this position, so columns named auto_increment
			// keep working.
			p.next()
			if col.Auto {
				return fmt.Errorf("column %q: AUTO_INCREMENT specified twice", name)
			}
			col.Auto = true
		case p.acceptKw("NOT"):
			if err := p.expectKw("NULL"); err != nil {
				return err
			}
			if col.NotNull {
				return fmt.Errorf("column %q: NOT NULL specified twice", name)
			}
			col.NotNull = true
		case p.acceptKw("NULL"):
			// explicit nullability, the default — nothing to record
		case p.acceptKw("PRIMARY"):
			if err := p.expectKw("KEY"); err != nil {
				return err
			}
			if col.PrimaryKey {
				return fmt.Errorf("column %q: PRIMARY KEY specified twice", name)
			}
			col.PrimaryKey = true
			// Optional key direction; the engine stores keys ascending and
			// sorts explicitly, so the direction is accepted and ignored.
			if !p.acceptKw("DESC") {
				p.acceptKw("ASC")
			}
		case p.acceptKw("UNIQUE"):
			col.Unique = true
		case p.acceptKw("DEFAULT"):
			// The bound expression excludes AND/OR and comparisons (chai
			// forbids them in DEFAULT); parsing stops before such tokens and
			// the leftover token then fails the statement as trailing junk.
			e, err := p.parseExpr(5)
			if err != nil {
				return err
			}
			col.Default = e
		case p.acceptKw("CHECK"):
			e, err := p.parseParenExpr()
			if err != nil {
				return err
			}
			st.Checks = append(st.Checks, e)
		default:
			st.Columns = append(st.Columns, col)
			return nil
		}
	}
}

// parseParenExpr parses a mandatory parenthesized expression — the shape
// CHECK constraints require ("CHECK a > 10" without parentheses is a
// syntax error in the dialect).
func (p *parser) parseParenExpr() (Expr, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	e, err := p.parseExpr(1)
	if err != nil {
		return nil, err
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return e, nil
}

// parseType reads a column type. All the dialect's aliases collapse onto
// the six storage types (chai: TINYINT..INT are INTEGER, INT8 is BIGINT);
// VECTOR(n) is the pgvector extension type. The dim return is only nonzero
// for vectors (the declared dimension).
func (p *parser) parseType() (Type, string, int, error) {
	t := p.peek()
	if t.kind != tkIdent {
		return 0, "", 0, errAt(t.pos, "expected a type name, found %q", t.s)
	}
	p.next()
	switch t.s {
	case "int", "integer", "tinyint", "smallint", "mediumint", "int2", "int4":
		return TypeInt, "INTEGER", 0, nil
	case "bigint", "int8":
		return TypeInt, "BIGINT", 0, nil
	case "double", "float8":
		// DOUBLE PRECISION is two words; the second is optional here.
		if n := p.peek(); n.kind == tkIdent && n.s == "precision" {
			p.next()
		}
		return TypeDouble, "DOUBLE PRECISION", 0, nil
	case "real":
		return TypeDouble, "DOUBLE PRECISION", 0, nil
	case "text", "clob":
		return TypeText, "TEXT", 0, nil
	case "varchar", "character", "char":
		// An optional length "(n)" parses and is ignored — the store has
		// no fixed-width strings (chai does the same).
		if p.acceptOp("(") {
			if n := p.peek(); n.kind != tkNumber {
				return 0, "", 0, errAt(n.pos, "expected length, found %q", n.s)
			}
			p.next()
			if err := p.expectOp(")"); err != nil {
				return 0, "", 0, err
			}
		}
		return TypeText, "TEXT", 0, nil
	case "bool", "boolean":
		return TypeBool, "BOOLEAN", 0, nil
	case "serial", "serial2", "serial4", "serial8", "smallserial", "bigserial":
		// pg's auto-increment pseudo-types: INTEGER storage; the column
		// parser turns the SERIAL marker into the Auto flag.
		return TypeInt, "SERIAL", 0, nil
	case "uuid":
		// UUID stores as canonical-form TEXT (validated on write, uuid.go),
		// so it key-encodes and indexes like text — usable as PRIMARY KEY.
		return TypeText, "UUID", 0, nil
	case "bytea", "blob", "bytes":
		return TypeBytea, "BYTEA", 0, nil
	case "timestamp", "timestamptz", "datetime":
		return TypeTimestamp, "TIMESTAMP", 0, nil
	case "vector":
		// VECTOR always requires an explicit dimension — bare VECTOR would
		// leave the write-time dimension check meaningless.
		dim, err := p.parseVectorDim(t.pos)
		if err != nil {
			return 0, "", 0, err
		}
		return TypeVector, fmt.Sprintf("VECTOR(%d)", dim), dim, nil
	}
	return 0, "", 0, errAt(t.pos, "unknown type %q", t.s)
}

// parseVectorDim reads the mandatory "(n)" after VECTOR and validates the
// pgvector bounds 1..16000.
func (p *parser) parseVectorDim(pos int) (int, error) {
	if !p.acceptOp("(") {
		return 0, errAt(pos, "VECTOR requires a dimension, e.g. VECTOR(3)")
	}
	t := p.peek()
	if t.kind != tkNumber || strings.ContainsAny(t.s, ".eE") {
		return 0, errAt(t.pos, "expected an integer vector dimension, found %q", t.s)
	}
	p.next()
	dim, err := strconv.Atoi(t.s)
	if err != nil || dim < 1 || dim > maxVectorDim {
		return 0, errAt(t.pos, "vector dimension must be between 1 and %d", maxVectorDim)
	}
	if err := p.expectOp(")"); err != nil {
		return 0, err
	}
	return dim, nil
}

// parseIdentList reads "(a, b DESC, c ASC)" — a parenthesized column list
// with optional per-column sort directions, used by PRIMARY KEY(...),
// UNIQUE(...) and CREATE INDEX. Directions are accepted and ignored: the
// engine stores every key ascending and sorts explicitly, so a DESC key
// changes nothing observable through SQL. Duplicate names are an error
// (CREATE_TABLE corpus: "PRIMARY KEY(a, a)" fails).
func (p *parser) parseIdentList() ([]string, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for {
		id, err := p.ident()
		if err != nil {
			return nil, err
		}
		if seen[id] {
			return nil, fmt.Errorf("column %q appears twice in constraint", id)
		}
		seen[id] = true
		out = append(out, id)
		if !p.acceptKw("DESC") {
			p.acceptKw("ASC")
		}
		if p.acceptOp(",") {
			continue
		}
		break
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *parser) parseCreateIndex(unique bool) (Statement, error) {
	st := &CreateIndex{Unique: unique}
	if p.acceptKw("IF") {
		if err := p.expectKw("NOT"); err != nil {
			return nil, err
		}
		if err := p.expectKw("EXISTS"); err != nil {
			return nil, err
		}
		st.IfNotExists = true
	}
	// The index name is optional: "CREATE INDEX ON test(a)" auto-names.
	if !p.peekKw("ON") {
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		st.Name = name
	} else if st.IfNotExists {
		// chai rejects IF NOT EXISTS on an anonymous index — the generated
		// name is fresh each time, so the clause could never match.
		return nil, fmt.Errorf("IF NOT EXISTS requires an explicit index name")
	}
	if err := p.expectKw("ON"); err != nil {
		return nil, err
	}
	tbl, err := p.ident()
	if err != nil {
		return nil, err
	}
	st.Table = tbl
	cols, err := p.parseIdentList()
	if err != nil {
		return nil, err
	}
	st.Columns = cols
	return st, nil
}

func (p *parser) parseDrop() (Statement, error) {
	p.next() // DROP
	switch {
	case p.acceptKw("TABLE"):
		st := &DropTable{}
		if p.acceptKw("IF") {
			if err := p.expectKw("EXISTS"); err != nil {
				return nil, err
			}
			st.IfExists = true
		}
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		st.Name = name
		return st, nil
	case p.acceptKw("INDEX"):
		st := &DropIndex{}
		if p.acceptKw("IF") {
			if err := p.expectKw("EXISTS"); err != nil {
				return nil, err
			}
			st.IfExists = true
		}
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		st.Name = name
		return st, nil
	}
	return nil, errAt(p.peek().pos, "expected TABLE or INDEX after DROP")
}

func (p *parser) parseInsert() (Statement, error) {
	p.next() // INSERT
	if err := p.expectKw("INTO"); err != nil {
		return nil, err
	}
	st := &Insert{}
	tbl, err := p.ident()
	if err != nil {
		return nil, err
	}
	st.Table = tbl
	// Optional explicit column list. Naming a column twice is an error
	// (INSERT corpus: "INSERT INTO t(a, a) VALUES ..." fails).
	if p.acceptOp("(") {
		seen := map[string]bool{}
		for {
			id, err := p.ident()
			if err != nil {
				return nil, err
			}
			if seen[id] {
				return nil, fmt.Errorf("column %q specified twice", id)
			}
			seen[id] = true
			st.Columns = append(st.Columns, id)
			if p.acceptOp(",") {
				continue
			}
			break
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
	}
	switch {
	case p.acceptKw("VALUES"):
		for {
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			var row []Expr
			for {
				e, err := p.parseExpr(1)
				if err != nil {
					return nil, err
				}
				row = append(row, e)
				if p.acceptOp(",") {
					// A trailing comma before ")" is tolerated, matching
					// chai (the SELECT/STRINGS corpus setup relies on it).
					if t := p.peek(); t.kind == tkOp && t.s == ")" {
						break
					}
					continue
				}
				break
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			st.Rows = append(st.Rows, row)
			if p.acceptOp(",") {
				continue
			}
			break
		}
	case p.peekKw("SELECT"):
		sel, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		st.Select = sel
	default:
		return nil, errAt(p.peek().pos, "expected VALUES or SELECT")
	}
	// ON CONFLICT DO NOTHING | DO REPLACE (chai's conflict clause).
	if p.acceptKw("ON") {
		if err := p.expectKw("CONFLICT"); err != nil {
			return nil, err
		}
		if err := p.expectKw("DO"); err != nil {
			return nil, err
		}
		switch {
		case p.acceptKw("NOTHING"):
			st.OnConflict = "NOTHING"
		case p.acceptKw("REPLACE"):
			st.OnConflict = "REPLACE"
		default:
			return nil, errAt(p.peek().pos, "expected NOTHING or REPLACE after DO")
		}
	}
	if p.acceptKw("RETURNING") {
		items, err := p.parseSelectItems()
		if err != nil {
			return nil, err
		}
		st.Returning = items
	}
	return st, nil
}

func (p *parser) parseUpdate() (Statement, error) {
	p.next() // UPDATE
	st := &Update{}
	tbl, err := p.ident()
	if err != nil {
		return nil, err
	}
	st.Table = tbl
	if err := p.expectKw("SET"); err != nil {
		return nil, err
	}
	for {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		e, err := p.parseExpr(1)
		if err != nil {
			return nil, err
		}
		st.Set = append(st.Set, Assignment{Column: col, Value: e})
		if p.acceptOp(",") {
			continue
		}
		break
	}
	if p.acceptKw("WHERE") {
		e, err := p.parseExpr(1)
		if err != nil {
			return nil, err
		}
		st.Where = e
	}
	return st, nil
}

func (p *parser) parseDelete() (Statement, error) {
	p.next() // DELETE
	if err := p.expectKw("FROM"); err != nil {
		return nil, err
	}
	st := &Delete{}
	tbl, err := p.ident()
	if err != nil {
		return nil, err
	}
	st.Table = tbl
	if p.acceptKw("WHERE") {
		e, err := p.parseExpr(1)
		if err != nil {
			return nil, err
		}
		st.Where = e
	}
	return st, nil
}

// parseSelect parses a full query: cores joined by UNION, then the
// trailing ORDER BY / LIMIT / OFFSET that apply to the whole result.
func (p *parser) parseSelect() (*Select, error) {
	core, err := p.parseSelectCore()
	if err != nil {
		return nil, err
	}
	st := &Select{Core: core}
	for p.acceptKw("UNION") {
		all := p.acceptKw("ALL")
		arm, err := p.parseSelectCore()
		if err != nil {
			return nil, err
		}
		st.Unions = append(st.Unions, UnionArm{All: all, Core: arm})
	}
	if p.acceptKw("ORDER") {
		if err := p.expectKw("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr(1)
			if err != nil {
				return nil, err
			}
			k := OrderKey{Expr: e}
			if p.acceptKw("DESC") {
				k.Desc = true
			} else {
				p.acceptKw("ASC")
			}
			st.OrderBy = append(st.OrderBy, k)
			if p.acceptOp(",") {
				continue
			}
			break
		}
	}
	if p.acceptKw("LIMIT") {
		e, err := p.parseExpr(1)
		if err != nil {
			return nil, err
		}
		st.Limit = e
	}
	if p.acceptKw("OFFSET") {
		e, err := p.parseExpr(1)
		if err != nil {
			return nil, err
		}
		st.Offset = e
	}
	return st, nil
}

func (p *parser) parseSelectCore() (*SelectCore, error) {
	if err := p.expectKw("SELECT"); err != nil {
		return nil, err
	}
	core := &SelectCore{}
	if p.acceptKw("DISTINCT") {
		core.Distinct = true
	} else {
		p.acceptKw("ALL")
	}
	items, err := p.parseSelectItems()
	if err != nil {
		return nil, err
	}
	core.Items = items
	if p.acceptKw("FROM") {
		from, err := p.parseFromClause()
		if err != nil {
			return nil, err
		}
		// One bare table keeps the legacy fields (the planner and every
		// single-table path key off core.Table); anything more complex
		// rides the source tree.
		if tr, ok := from.(*TableRef); ok {
			core.Table, core.Alias = tr.Table, tr.Alias
		} else {
			core.From = from
		}
	}
	if p.acceptKw("WHERE") {
		e, err := p.parseExpr(1)
		if err != nil {
			return nil, err
		}
		core.Where = e
	}
	if p.acceptKw("GROUP") {
		if err := p.expectKw("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr(1)
			if err != nil {
				return nil, err
			}
			core.GroupBy = append(core.GroupBy, e)
			if p.acceptOp(",") {
				continue
			}
			break
		}
	}
	return core, nil
}

// parseSelectItems reads a projection list (also used by RETURNING).
func (p *parser) parseSelectItems() ([]SelectItem, error) {
	var items []SelectItem
	for {
		if p.acceptOp("*") {
			items = append(items, SelectItem{Star: true})
		} else {
			e, err := p.parseExpr(1)
			if err != nil {
				return nil, err
			}
			it := SelectItem{Expr: e}
			if p.acceptKw("AS") {
				a, err := p.identLabel()
				if err != nil {
					return nil, err
				}
				it.Alias = a
			} else if t := p.peek(); t.kind == tkIdent {
				// bare alias: "SELECT a b" names column a as b
				p.next()
				it.Alias = labelOf(t)
			}
			items = append(items, it)
		}
		if p.acceptOp(",") {
			continue
		}
		return items, nil
	}
}

// --- expressions --------------------------------------------------------------

// infixPrec returns the binding power of the upcoming infix operator, or
// 0 when the next token does not continue an expression. The table is
// chai's scanner.Precedence() verbatim.
func (p *parser) infixPrec() int {
	t := p.peek()
	if t.kind == tkKeyword {
		switch t.s {
		case "OR":
			return 1
		case "AND":
			return 2
		case "NOT":
			// Infix NOT only exists as NOT LIKE / NOT IN / NOT BETWEEN.
			if n := p.toks[p.i+1]; n.kind == tkKeyword &&
				(n.s == "LIKE" || n.s == "IN" || n.s == "BETWEEN") {
				return 4
			}
			return 0
		case "IS", "IN", "LIKE", "BETWEEN":
			return 4
		}
		return 0
	}
	if t.kind != tkOp {
		return 0
	}
	switch t.s {
	case "=", "!=":
		return 4
	case "<", "<=", ">", ">=":
		return 5
	case "<->", "<#>", "<=>":
		// Vector distance operators bind just above comparison, so
		// "a <-> b < 0.5" parses as "(a <-> b) < 0.5" — the distance is a
		// double that then participates in the comparison.
		return 6
	case "&", "|", "^":
		return 6
	case "+", "-":
		return 7
	case "*", "/", "%":
		return 8
	case "||":
		return 9
	}
	return 0
}

// parseExpr is the Pratt loop: parse a prefix operand, then greedily
// bind infix operators whose precedence is at least minPrec.
func (p *parser) parseExpr(minPrec int) (Expr, error) {
	lhs, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		prec := p.infixPrec()
		if prec == 0 || prec < minPrec {
			return lhs, nil
		}
		t := p.peek()

		// Keyword operators with special right-hand shapes.
		if t.kind == tkKeyword {
			switch t.s {
			case "IS":
				p.next()
				not := p.acceptKw("NOT")
				if err := p.expectKw("NULL"); err != nil {
					return nil, err
				}
				lhs = &IsNull{X: lhs, Not: not}
				continue
			case "NOT":
				p.next() // the LIKE/IN/BETWEEN keyword follows
				switch {
				case p.acceptKw("LIKE"):
					rhs, err := p.parseExpr(prec + 1)
					if err != nil {
						return nil, err
					}
					lhs = &Binary{Op: "NOT LIKE", L: lhs, R: rhs}
				case p.acceptKw("IN"):
					list, sub, err := p.parseInRHS()
					if err != nil {
						return nil, err
					}
					lhs = &InList{X: lhs, List: list, Sub: sub, Not: true}
				case p.acceptKw("BETWEEN"):
					b, err := p.parseBetweenTail(lhs, true)
					if err != nil {
						return nil, err
					}
					lhs = b
				default:
					return nil, errAt(p.peek().pos, "expected LIKE, IN or BETWEEN after NOT")
				}
				continue
			case "LIKE":
				p.next()
				rhs, err := p.parseExpr(prec + 1)
				if err != nil {
					return nil, err
				}
				lhs = &Binary{Op: "LIKE", L: lhs, R: rhs}
				continue
			case "IN":
				p.next()
				list, sub, err := p.parseInRHS()
				if err != nil {
					return nil, err
				}
				lhs = &InList{X: lhs, List: list, Sub: sub}
				continue
			case "BETWEEN":
				p.next()
				b, err := p.parseBetweenTail(lhs, false)
				if err != nil {
					return nil, err
				}
				lhs = b
				continue
			case "AND", "OR":
				p.next()
				rhs, err := p.parseExpr(prec + 1)
				if err != nil {
					return nil, err
				}
				lhs = &Binary{Op: t.s, L: lhs, R: rhs}
				continue
			}
		}

		// Plain left-associative binary operator.
		p.next()
		rhs, err := p.parseExpr(prec + 1)
		if err != nil {
			return nil, err
		}
		lhs = &Binary{Op: t.s, L: lhs, R: rhs}
	}
}

// parseBetweenTail parses "lo AND hi" after BETWEEN. The bounds parse one
// level above comparison so the AND belongs to BETWEEN, not boolean AND.
func (p *parser) parseBetweenTail(x Expr, not bool) (Expr, error) {
	lo, err := p.parseExpr(5)
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("AND"); err != nil {
		return nil, err
	}
	hi, err := p.parseExpr(5)
	if err != nil {
		return nil, err
	}
	return &Between{X: x, Lo: lo, Hi: hi, Not: not}, nil
}

// parseInRHS reads IN's right side: "(SELECT ...)" or "(e1, e2, ...)".
func (p *parser) parseInRHS() ([]Expr, *Select, error) {
	if p.peekIsOp("(") && p.peekAheadKw(1, "SELECT") {
		p.next() // (
		sub, err := p.parseSelect()
		if err != nil {
			return nil, nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, nil, err
		}
		return nil, sub, nil
	}
	list, err := p.parseExprList()
	return list, nil, err
}

// peekIsOp reports whether the next token is the given operator.
func (p *parser) peekIsOp(op string) bool {
	t := p.peek()
	return t.kind == tkOp && t.s == op
}

// peekAheadKw reports whether the token n places ahead is the keyword.
func (p *parser) peekAheadKw(n int, kw string) bool {
	if p.i+n >= len(p.toks) {
		return false
	}
	t := p.toks[p.i+n]
	return t.kind == tkKeyword && t.s == kw
}

// parseExprList reads "(e1, e2, ...)" for IN.
func (p *parser) parseExprList() ([]Expr, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var out []Expr
	for {
		e, err := p.parseExpr(1)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		if p.acceptOp(",") {
			continue
		}
		break
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return out, nil
}

// parseUnary parses prefix operators and primary expressions.
func (p *parser) parseUnary() (Expr, error) {
	t := p.peek()
	switch {
	case t.kind == tkOp && (t.s == "-" || t.s == "+"):
		p.next()
		// "-9223372036854775808" must parse as MinInt64: the bare digits
		// overflow int64 on their own, so the sign folds in BEFORE the
		// literal converts (expr/comparison.sql exercises MinInt64).
		if nt := p.peek(); t.s == "-" && nt.kind == tkNumber && !strings.ContainsAny(nt.s, ".eE") {
			if i, err := strconv.ParseInt("-"+nt.s, 10, 64); err == nil {
				p.next()
				return p.parseCastSuffix(&Literal{Val: intV(i), Src: "-" + nt.s})
			}
		}
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		// Fold the sign into numeric literals so "-1" is a literal (and
		// its column label reads "-1", matching the dialect).
		if lit, ok := x.(*Literal); ok && lit.Val.isNumeric() {
			if t.s == "-" {
				v := lit.Val
				if v.T == TypeInt {
					v.I = -v.I
				} else {
					v.F = -v.F
				}
				return p.parseCastSuffix(&Literal{Val: v, Src: "-" + lit.Src})
			}
			return p.parseCastSuffix(lit)
		}
		if t.s == "+" {
			return x, nil
		}
		return &Unary{Op: "-", X: x}, nil
	case t.kind == tkKeyword && t.s == "NOT":
		p.next()
		// NOT binds looser than comparisons (precedence 3 in chai), so
		// "NOT a = 1" negates the whole comparison.
		x, err := p.parseExpr(4)
		if err != nil {
			return nil, err
		}
		return &Unary{Op: "NOT", X: x}, nil
	}
	return p.parsePostfix()
}

// parsePostfix parses a primary expression followed by any number of
// short-form casts: 1::TEXT, x::DOUBLE PRECISION::TEXT. "::" binds tighter
// than every infix operator, matching chai.
func (p *parser) parsePostfix() (Expr, error) {
	e, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	return p.parseCastSuffix(e)
}

// parseCastSuffix wraps e in Cast nodes for any trailing "::type" casts.
// Sign-folded literals need it too ("-1::INTEGER" is valid, cast.sql).
func (p *parser) parseCastSuffix(e Expr) (Expr, error) {
	for p.acceptOp("::") {
		typ, typeName, dim, err := p.parseType()
		if err != nil {
			return nil, err
		}
		e = &Cast{X: e, To: typ, TypeName: typeName, Dim: dim}
	}
	return e, nil
}

// parsePrimary parses literals, column references, function calls, CAST
// and parenthesized expressions.
func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.kind {
	case tkNumber:
		p.next()
		return numberLiteral(t)
	case tkString:
		p.next()
		src := "'" + strings.ReplaceAll(t.s, "'", "''") + "'"
		// A string spelled '\x...' is a BYTEA literal in the dialect
		// (expr/literal.sql: typeof('\xFF') is 'bytea'), with hex content.
		if strings.HasPrefix(t.s, `\x`) || strings.HasPrefix(t.s, `\X`) {
			raw, err := decodeHexLiteral(t.s[2:])
			if err != nil {
				return nil, errAt(t.pos, "%v", err)
			}
			return &Literal{Val: byteaV(raw), Src: src}, nil
		}
		// Preserve the quoted spelling for column labels ("'a'").
		return &Literal{Val: textV(t.s), Src: src}, nil
	case tkParam:
		p.next()
		n, err := strconv.Atoi(t.s)
		if err != nil || n < 1 {
			return nil, errAt(t.pos, "bad positional parameter $%s", t.s)
		}
		return &Param{N: n}, nil
	case tkKeyword:
		switch t.s {
		case "NULL":
			p.next()
			return &Literal{Val: nullV(), Src: "NULL"}, nil
		case "TRUE":
			p.next()
			return &Literal{Val: boolV(true), Src: "true"}, nil
		case "FALSE":
			p.next()
			return &Literal{Val: boolV(false), Src: "false"}, nil
		case "CAST":
			p.next()
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			x, err := p.parseExpr(1)
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("AS"); err != nil {
				return nil, err
			}
			typ, typeName, dim, err := p.parseType()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return &Cast{X: x, To: typ, TypeName: typeName, Dim: dim}, nil
		}
		if t.s == "EXISTS" && p.peekAheadIsOp(1, "(") && p.peekAheadKw(2, "SELECT") {
			// EXISTS (SELECT ...) — EXISTS is already a keyword (IF EXISTS),
			// so it is handled here rather than with the identifiers.
			p.next() // EXISTS
			p.next() // (
			sub, err := p.parseSelect()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return &Subquery{Select: sub, Exists: true}, nil
		}
		return nil, errAt(t.pos, "unexpected keyword %q in expression", t.s)
	case tkIdent:
		p.next()
		// A call: name(...). Otherwise a column ref, maybe qualified.
		if p.acceptOp("(") {
			return p.parseCallTail(t.s)
		}
		// Qualified reference "table.column": ident '.' ident.
		if pt := p.peek(); pt.kind == tkOp && pt.s == "." {
			p.next()
			col, err := p.ident()
			if err != nil {
				return nil, err
			}
			return &ColumnRef{Table: t.s, Column: col}, nil
		}
		return &ColumnRef{Column: t.s}, nil
	case tkOp:
		if t.s == "(" {
			p.next()
			// A scalar subquery: "(SELECT ...)" in expression position.
			if p.peekKw("SELECT") {
				sub, err := p.parseSelect()
				if err != nil {
					return nil, err
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				return &Subquery{Select: sub}, nil
			}
			e, err := p.parseExpr(1)
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return e, nil
		}
	}
	return nil, errAt(t.pos, "unexpected %q in expression", t.s)
}

// parseCallTail parses the argument list after "name(".
func (p *parser) parseCallTail(name string) (Expr, error) {
	fc := &FuncCall{Name: strings.ToLower(name)}
	if p.acceptOp("*") {
		fc.Star = true
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return fc, nil
	}
	if p.acceptOp(")") {
		return fc, nil
	}
	for {
		e, err := p.parseExpr(1)
		if err != nil {
			return nil, err
		}
		fc.Args = append(fc.Args, e)
		if p.acceptOp(",") {
			continue
		}
		break
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return fc, nil
}

// decodeHexLiteral decodes the hex payload of a '\x...' bytea literal,
// with chai's exact error wording ("invalid hexadecimal digit: h").
func decodeHexLiteral(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		if ib, ok := err.(hex.InvalidByteError); ok {
			return nil, fmt.Errorf("invalid hexadecimal digit: %c", byte(ib))
		}
		return nil, fmt.Errorf("invalid hexadecimal data: %v", err)
	}
	return b, nil
}

// numberLiteral converts a numeric token: plain digit runs become
// integers (erroring past int64 like chai's overflow tests expect),
// anything with a dot or exponent becomes a double.
func numberLiteral(t token) (Expr, error) {
	if !strings.ContainsAny(t.s, ".eE") {
		i, err := strconv.ParseInt(t.s, 10, 64)
		if err != nil {
			return nil, errAt(t.pos, "integer out of range: %s", t.s)
		}
		return &Literal{Val: intV(i), Src: t.s}, nil
	}
	f, err := strconv.ParseFloat(t.s, 64)
	if err != nil {
		return nil, errAt(t.pos, "bad number %q", t.s)
	}
	return &Literal{Val: doubleV(f), Src: t.s}, nil
}
