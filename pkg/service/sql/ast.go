// ast.go defines the abstract syntax tree the parser produces and the
// planner/executor consume. Every expression node can print itself back
// as SQL — the dialect labels result columns with the expression text
// (SELECT a % 2 ... yields a column literally named "a % 2"), so String()
// is part of the language surface, not just debugging sugar.
package sql

import (
	"strconv"
	"strings"
)

// Statement is any executable SQL statement.
type Statement interface{ stmtNode() }

// Expr is any SQL expression node.
type Expr interface {
	exprNode()
	String() string
}

// ---------------------------------------------------------------------------
// Statements
// ---------------------------------------------------------------------------

// ColumnDef is one column in CREATE TABLE, with its inline constraints.
type ColumnDef struct {
	Name       string
	Type       Type
	TypeName   string // the declared spelling, e.g. "BIGINT", kept for messages
	Dim        int    // VECTOR(n) dimension; 0 for every other type
	NotNull    bool
	PrimaryKey bool
	Unique     bool
	Auto       bool // SERIAL type or AUTO_INCREMENT attribute
	Default    Expr // nil when no DEFAULT clause
}

// CreateTable is CREATE TABLE with column and table-level constraints.
type CreateTable struct {
	Name        string
	IfNotExists bool
	Columns     []ColumnDef
	PrimaryKey  []string   // table-level PRIMARY KEY(...); empty if inline/none
	Uniques     [][]string // table-level UNIQUE(...) constraint column sets
	// Checks collects every CHECK(...) expression — column-level and
	// table-level — in declaration order. The executor names them
	// <table>_check, <table>_check1, ... exactly like chai.
	Checks []Expr
}

// DropTable is DROP TABLE [IF EXISTS] name.
type DropTable struct {
	Name     string
	IfExists bool
}

// CreateIndex is CREATE [UNIQUE] INDEX [IF NOT EXISTS] [name] ON table(cols).
type CreateIndex struct {
	Name        string // empty means auto-generate
	Table       string
	Columns     []string
	Unique      bool
	IfNotExists bool
}

// DropIndex is DROP INDEX [IF EXISTS] name.
type DropIndex struct {
	Name     string
	IfExists bool
}

// Insert is INSERT INTO with VALUES rows or a SELECT source.
type Insert struct {
	Table     string
	Columns   []string     // explicit column list; empty = positional
	Rows      [][]Expr     // VALUES rows (nil when Select is set)
	Select    *Select      // INSERT ... SELECT source (nil for VALUES)
	Returning []SelectItem // RETURNING projection; nil when absent
	// OnConflict is "" (fail on conflict, the default), "NOTHING"
	// (ON CONFLICT DO NOTHING) or "REPLACE" (ON CONFLICT DO REPLACE).
	OnConflict string
}

// Assignment is one "col = expr" in UPDATE SET.
type Assignment struct {
	Column string
	Value  Expr
}

// Update is UPDATE table SET ... [WHERE ...].
type Update struct {
	Table string
	Set   []Assignment
	Where Expr
}

// Delete is DELETE FROM table [WHERE ...].
type Delete struct {
	Table string
	Where Expr
}

// SelectItem is one projection: an expression with an optional alias, or
// the * wildcard.
type SelectItem struct {
	Star  bool
	Expr  Expr
	Alias string
}

// OrderKey is one ORDER BY term.
type OrderKey struct {
	Expr Expr
	Desc bool
}

// SelectCore is a single SELECT ... FROM ... WHERE ... GROUP BY block —
// the unit that UNION combines.
type SelectCore struct {
	Distinct bool
	Items    []SelectItem
	Table    string // FROM base table when the source is exactly one; else ""
	Alias    string // optional FROM alias ("FROM t AS x"); "" when absent
	// From is the general source tree — set when FROM is anything more
	// than one base table (joins, parenthesized groups, subqueries).
	// Table/Alias and From are mutually exclusive.
	From    TableExpr
	Where   Expr
	GroupBy []Expr
}

// TableExpr is a FROM-clause source: a base table, a derived table
// (subquery), or a join of two sources (join.go).
type TableExpr interface{ tableExpr() }

// TableRef is one base table with an optional alias.
type TableRef struct {
	Table string
	Alias string
}

// SubqueryRef is a derived table: (SELECT ...) AS alias. Lateral marks a
// LATERAL subquery, re-evaluated per row of the sources to its left with
// their columns in scope.
type SubqueryRef struct {
	Select  *Select
	Alias   string
	Lateral bool
}

// JoinExpr joins two sources.
type JoinExpr struct {
	Type    string // "INNER", "LEFT", "RIGHT", "FULL", "CROSS"
	L, R    TableExpr
	On      Expr     // ON predicate; nil for CROSS/USING/NATURAL
	Using   []string // USING (a, b ...); nil otherwise
	Natural bool     // NATURAL join: USING resolved from common columns
}

func (*TableRef) tableExpr()    {}
func (*SubqueryRef) tableExpr() {}
func (*JoinExpr) tableExpr()    {}

// UnionArm is one "UNION [ALL] <core>" continuation.
type UnionArm struct {
	All  bool
	Core *SelectCore
}

// Select is a full query: the first core, any union arms, and the
// trailing ORDER BY / LIMIT / OFFSET that apply to the combined result.
type Select struct {
	Core    *SelectCore
	Unions  []UnionArm
	OrderBy []OrderKey
	Limit   Expr
	Offset  Expr
}

// TxStmt is BEGIN/COMMIT/ROLLBACK. The engine is autocommit-only (every
// statement is one distributed transaction), so these are accepted as
// no-ops purely for driver compatibility.
type TxStmt struct {
	Keyword string // "BEGIN", "COMMIT" or "ROLLBACK"
}

func (*CreateTable) stmtNode() {}
func (*DropTable) stmtNode()   {}
func (*CreateIndex) stmtNode() {}
func (*DropIndex) stmtNode()   {}
func (*Insert) stmtNode()      {}
func (*Update) stmtNode()      {}
func (*Delete) stmtNode()      {}
func (*Select) stmtNode()      {}
func (*TxStmt) stmtNode()      {}

// ---------------------------------------------------------------------------
// Expressions
// ---------------------------------------------------------------------------

// Literal is a constant value.
type Literal struct {
	Val Value
	// Src preserves the source spelling for column labels, so SELECT 1.0
	// labels its column "1.0" and SELECT 'a' labels it "'a'".
	Src string
}

// ColumnRef references a column, optionally table-qualified.
type ColumnRef struct {
	Table  string // optional qualifier
	Column string
}

// Unary is -x, +x or NOT x.
type Unary struct {
	Op string // "-", "+", "NOT"
	X  Expr
}

// Binary is any infix operator: arithmetic, comparison, AND/OR, LIKE, ||,
// and the vector distance operators.
type Binary struct {
	Op   string // "+","-","*","/","%","&","|","^","=","!=","<","<=",">",">=","AND","OR","LIKE","NOT LIKE","||","<->","<#>","<=>"
	L, R Expr
}

// Between is x [NOT] BETWEEN lo AND hi.
type Between struct {
	X, Lo, Hi Expr
	Not       bool
}

// InList is x [NOT] IN (e1, e2, ...).
type InList struct {
	X    Expr
	List []Expr
	// Sub is "x IN (SELECT ...)": resolved to a literal List before
	// evaluation (subquery.go).
	Sub *Select
	Not bool
}

// Subquery is a subquery in expression position: scalar "(SELECT ...)"
// (one column; one row or NULL) or "EXISTS (SELECT ...)". Resolved to a
// literal before evaluation (subquery.go); correlated forms are not
// supported in expressions — LATERAL is the tool for that.
type Subquery struct {
	Select *Select
	Exists bool
}

func (*Subquery) exprNode() {}
func (e *Subquery) String() string {
	if e.Exists {
		return "EXISTS (SELECT ...)"
	}
	return "(SELECT ...)"
}

// IsNull is x IS [NOT] NULL.
type IsNull struct {
	X   Expr
	Not bool
}

// FuncCall is name(args...) — scalar or aggregate. Star marks COUNT(*).
type FuncCall struct {
	Name string // lowercased
	Args []Expr
	Star bool
}

// Cast is CAST(x AS type) or the short form x::type. Dim carries the
// declared dimension of a vector target ("x::VECTOR(3)") so the cast can
// enforce it; 0 for every other target type.
type Cast struct {
	X        Expr
	To       Type
	TypeName string
	Dim      int
}

// Param is a $N positional parameter inside a prepared statement. It only
// appears in ASTs produced for the extended wire protocol; bindParams
// replaces every Param with a literal before execution.
type Param struct {
	N int // 1-based position
}

func (*Literal) exprNode()   {}
func (*ColumnRef) exprNode() {}
func (*Unary) exprNode()     {}
func (*Binary) exprNode()    {}
func (*Between) exprNode()   {}
func (*InList) exprNode()    {}
func (*IsNull) exprNode()    {}
func (*FuncCall) exprNode()  {}
func (*Cast) exprNode()      {}
func (*Param) exprNode()     {}

// String renders the literal exactly as it was typed when possible.
func (e *Literal) String() string {
	if e.Src != "" {
		return e.Src
	}
	switch e.Val.T {
	case TypeNull:
		return "NULL"
	case TypeText:
		return "'" + strings.ReplaceAll(e.Val.S, "'", "''") + "'"
	default:
		return e.Val.FormatText()
	}
}

func (e *ColumnRef) String() string {
	if e.Table != "" {
		return e.Table + "." + e.Column
	}
	return e.Column
}

func (e *Unary) String() string {
	if e.Op == "NOT" {
		return "NOT " + e.X.String()
	}
	return e.Op + e.X.String()
}

func (e *Binary) String() string {
	return e.L.String() + " " + e.Op + " " + e.R.String()
}

func (e *Between) String() string {
	not := ""
	if e.Not {
		not = "NOT "
	}
	return e.X.String() + " " + not + "BETWEEN " + e.Lo.String() + " AND " + e.Hi.String()
}

func (e *InList) String() string {
	parts := make([]string, len(e.List))
	for i, x := range e.List {
		parts[i] = x.String()
	}
	not := ""
	if e.Not {
		not = "NOT "
	}
	return e.X.String() + " " + not + "IN (" + strings.Join(parts, ", ") + ")"
}

func (e *IsNull) String() string {
	if e.Not {
		return e.X.String() + " IS NOT NULL"
	}
	return e.X.String() + " IS NULL"
}

// String renders function calls with the name uppercased — chai labels the
// result column of SELECT lower(a) as "LOWER(a)" (SELECT/STRINGS corpus).
func (e *FuncCall) String() string {
	name := strings.ToUpper(e.Name)
	if e.Star {
		return name + "(*)"
	}
	parts := make([]string, len(e.Args))
	for i, a := range e.Args {
		parts[i] = a.String()
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

// String renders CAST with the type name lowercased — chai labels
// SELECT CAST(b as TEXT) as "CAST(b AS text)" (SELECT/STRINGS corpus).
func (e *Cast) String() string {
	return "CAST(" + e.X.String() + " AS " + strings.ToLower(e.TypeName) + ")"
}

func (e *Param) String() string { return "$" + strconv.Itoa(e.N) }

// labelFor picks the result-column name for a projection: the alias when
// given, the bare column name for plain references, otherwise the
// expression's SQL text.
func labelFor(it SelectItem) string {
	if it.Alias != "" {
		return it.Alias
	}
	if c, ok := it.Expr.(*ColumnRef); ok {
		return c.Column
	}
	return it.Expr.String()
}

// exprHasAggregate walks an expression tree looking for aggregate calls;
// the planner uses it to decide between plain and grouped execution.
func exprHasAggregate(e Expr) bool {
	switch x := e.(type) {
	case *FuncCall:
		if isAggregateName(x.Name) {
			return true
		}
		for _, a := range x.Args {
			if exprHasAggregate(a) {
				return true
			}
		}
	case *Unary:
		return exprHasAggregate(x.X)
	case *Binary:
		return exprHasAggregate(x.L) || exprHasAggregate(x.R)
	case *Between:
		return exprHasAggregate(x.X) || exprHasAggregate(x.Lo) || exprHasAggregate(x.Hi)
	case *InList:
		if exprHasAggregate(x.X) {
			return true
		}
		for _, a := range x.List {
			if exprHasAggregate(a) {
				return true
			}
		}
	case *IsNull:
		return exprHasAggregate(x.X)
	case *Cast:
		return exprHasAggregate(x.X)
	}
	return false
}

// isAggregateName recognizes the five v1 aggregate functions.
func isAggregateName(name string) bool {
	switch name {
	case "count", "sum", "avg", "min", "max":
		return true
	}
	return false
}

// quoteIdent renders an identifier safely back into SQL (used by
// parameter substitution helpers and messages).
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// litInt is a helper to build integer literal expressions.
func litInt(i int64) *Literal {
	return &Literal{Val: intV(i), Src: strconv.FormatInt(i, 10)}
}
