// show_test.go covers the schema-introspection extension (show.go): every
// SHOW form, the DESCRIBE/DESC synonyms, and — critically — that the
// extension's words stayed unreserved so columns named "show"/"tables"
// keep working.
package sql

import (
	"context"
	"strings"
	"testing"
)

func showEngine(t *testing.T) *Engine {
	t.Helper()
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		email TEXT NOT NULL UNIQUE,
		name TEXT DEFAULT 'anon',
		age INTEGER,
		CHECK (age >= 0)
	)`)
	run(t, e, `CREATE TABLE orders (id INTEGER PRIMARY KEY, uid INTEGER, note TEXT)`)
	run(t, e, `CREATE INDEX orders_uid_idx ON orders (uid)`)
	return e
}

func TestShowTables(t *testing.T) {
	e := showEngine(t)
	res := run(t, e, "SHOW TABLES")
	if res.Tag != "SHOW" || len(res.Columns) != 1 || res.Columns[0] != "table" {
		t.Fatalf("shape: %+v", res)
	}
	rowsEqual(t, res, [][]string{{"orders"}, {"users"}})
}

func TestShowDatabases(t *testing.T) {
	e := showEngine(t)
	res := run(t, e, "SHOW DATABASES")
	rowsEqual(t, res, [][]string{{"testdb"}})
}

func TestShowColumnsAndDescribe(t *testing.T) {
	e := showEngine(t)
	want := [][]string{
		{"id", "INTEGER", "NO", "PRI", "NULL", ""},
		{"email", "TEXT", "NO", "UNI", "NULL", ""},
		{"name", "TEXT", "YES", "", "'anon'", ""},
		{"age", "INTEGER", "YES", "", "NULL", ""},
	}
	for _, stmt := range []string{"SHOW COLUMNS FROM users", "DESCRIBE users", "DESC users"} {
		res := run(t, e, stmt)
		if strings.Join(res.Columns, ",") != "column,type,nullable,key,default,extra" {
			t.Fatalf("%s columns: %v", stmt, res.Columns)
		}
		rowsEqual(t, res, want)
	}
}

func TestShowIndexes(t *testing.T) {
	e := showEngine(t)
	res := run(t, e, "SHOW INDEXES FROM orders")
	rowsEqual(t, res, [][]string{
		{"orders", "PRIMARY", "id", "true"},
		{"orders", "orders_uid_idx", "uid", "false"},
	})
	// Unscoped: every table, PRIMARY pseudo-rows included.
	all := run(t, e, "SHOW INDEXES")
	rowsEqual(t, all, [][]string{
		{"orders", "PRIMARY", "id", "true"},
		{"orders", "orders_uid_idx", "uid", "false"},
		{"users", "PRIMARY", "id", "true"},
		{"users", "uniq_email", "email", "true"},
	})
}

func TestShowCreateTable(t *testing.T) {
	e := showEngine(t)
	res := run(t, e, "SHOW CREATE TABLE users")
	if len(res.Rows) != 1 || res.Rows[0][0] == nil || *res.Rows[0][0] != "users" {
		t.Fatalf("rows: %v", textRows(res))
	}
	ddl := *res.Rows[0][1]
	for _, frag := range []string{
		"CREATE TABLE users (",
		"id INTEGER",
		"email TEXT NOT NULL UNIQUE",
		"name TEXT DEFAULT 'anon'",
		"PRIMARY KEY (id)",
		"CHECK (",
	} {
		if !strings.Contains(ddl, frag) {
			t.Fatalf("DDL missing %q:\n%s", frag, ddl)
		}
	}
	// The column-level UNIQUE's implied index must not repeat as DDL.
	if strings.Contains(ddl, "CREATE UNIQUE INDEX uniq_email") {
		t.Fatalf("uniq_email rendered twice:\n%s", ddl)
	}
	// The secondary index renders as its own statement.
	orders := run(t, e, "SHOW CREATE TABLE orders")
	if !strings.Contains(*orders.Rows[0][1], "CREATE INDEX orders_uid_idx ON orders (uid);") {
		t.Fatalf("orders DDL missing index:\n%s", *orders.Rows[0][1])
	}
}

// TestShowRoundTrip feeds SHOW CREATE TABLE output back through the engine.
func TestShowRoundTrip(t *testing.T) {
	e := showEngine(t)
	ddl := *run(t, e, "SHOW CREATE TABLE users").Rows[0][1]
	e2 := NewEngineWithStore(NewMemStore(), "other")
	if _, err := e2.Exec(context.Background(), ddl); err != nil {
		t.Fatalf("round-trip DDL failed: %v\n%s", err, ddl)
	}
	rowsEqual(t, run(t, e2, "SHOW TABLES"), [][]string{{"users"}})
}

// TestShowWordsNotReserved: the extension must not steal identifiers —
// tables and columns named with introspection words keep working.
func TestShowWordsNotReserved(t *testing.T) {
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE show (describe INTEGER PRIMARY KEY, tables TEXT)`)
	run(t, e, `INSERT INTO show (describe, tables) VALUES (1, 'x')`)
	res := run(t, e, `SELECT describe, tables FROM show WHERE describe = 1`)
	rowsEqual(t, res, [][]string{{"1", "x"}})
	rowsEqual(t, run(t, e, "SHOW TABLES"), [][]string{{"show"}})
	rowsEqual(t, run(t, e, "DESCRIBE show"), [][]string{
		{"describe", "INTEGER", "NO", "PRI", "NULL", ""},
		{"tables", "TEXT", "YES", "", "NULL", ""},
	})
}

func TestShowErrors(t *testing.T) {
	e := showEngine(t)
	if _, err := e.Exec(context.Background(), "SHOW COLUMNS FROM nosuch"); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("want no-such-table error, got %v", err)
	}
	if _, err := e.Exec(context.Background(), "SHOW garbage"); err == nil {
		t.Fatal("want parse error for SHOW garbage")
	}
	// "show" mid-statement must still parse as an ordinary identifier, not
	// as introspection (users is empty, so projection never evaluates).
	if _, err := e.Exec(context.Background(), "SELECT show FROM users"); err != nil {
		t.Fatalf("mid-statement 'show' should stay an identifier: %v", err)
	}
}
