// exec_test.go is the dialect-conformance harness (§13,
// §22): it runs SQL statements through the full engine against the in-memory
// store and checks results, the same way chai's sqltests corpus is ported.
// These cases mirror representative sqltests behaviors — DDL, DML, the type
// system, expressions, ordering, DISTINCT, UNION, and aggregation.
package sql

import (
	"context"
	"testing"
)

// run executes SQL and returns the last statement's result rows as text.
func run(t *testing.T, e *Engine, sql string) ExecResult {
	t.Helper()
	res, err := e.Exec(context.Background(), sql)
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	return res[len(res)-1]
}

// rowsEqual compares an ExecResult's rows to want (nil pointer → "NULL").
func rowsEqual(t *testing.T, got ExecResult, want [][]string) {
	t.Helper()
	if len(got.Rows) != len(want) {
		t.Fatalf("row count: got %d want %d (%v)", len(got.Rows), len(want), textRows(got))
	}
	for i := range want {
		if len(got.Rows[i]) != len(want[i]) {
			t.Fatalf("row %d width: got %d want %d", i, len(got.Rows[i]), len(want[i]))
		}
		for j := range want[i] {
			g := "NULL"
			if got.Rows[i][j] != nil {
				g = *got.Rows[i][j]
			}
			if g != want[i][j] {
				t.Fatalf("row %d col %d: got %q want %q", i, j, g, want[i][j])
			}
		}
	}
}

func textRows(r ExecResult) [][]string {
	out := make([][]string, len(r.Rows))
	for i, row := range r.Rows {
		out[i] = make([]string, len(row))
		for j, v := range row {
			if v == nil {
				out[i][j] = "NULL"
			} else {
				out[i][j] = *v
			}
		}
	}
	return out
}

func newEngine() *Engine { return NewEngineWithStore(NewMemStore(), "test") }

func TestSelectConstants(t *testing.T) {
	e := newEngine()
	rowsEqual(t, run(t, e, "SELECT 1 + 2 * 3"), [][]string{{"7"}})
	rowsEqual(t, run(t, e, "SELECT 10 / 3"), [][]string{{"3"}})     // integer division
	rowsEqual(t, run(t, e, "SELECT 10.0 / 4"), [][]string{{"2.5"}}) // float division
	rowsEqual(t, run(t, e, "SELECT 'a' || 'b'"), [][]string{{"ab"}})
	rowsEqual(t, run(t, e, "SELECT 1 + NULL"), [][]string{{"NULL"}})
	rowsEqual(t, run(t, e, "SELECT typeof(1), typeof(1.5), typeof('x')"),
		[][]string{{"integer", "double precision", "text"}})
}

func TestCreateInsertSelect(t *testing.T) {
	e := newEngine()
	run(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)")
	run(t, e, "INSERT INTO users (id, name, age) VALUES (1, 'alice', 30), (2, 'bob', 25), (3, 'carol', 35)")
	got := run(t, e, "SELECT id, name FROM users ORDER BY id")
	rowsEqual(t, got, [][]string{{"1", "alice"}, {"2", "bob"}, {"3", "carol"}})

	got = run(t, e, "SELECT name FROM users WHERE age > 28 ORDER BY age DESC")
	rowsEqual(t, got, [][]string{{"carol"}, {"alice"}})
}

func TestUpdateDelete(t *testing.T) {
	e := newEngine()
	run(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)")
	run(t, e, "INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)")
	run(t, e, "UPDATE t SET v = v + 5 WHERE id = 2")
	rowsEqual(t, run(t, e, "SELECT v FROM t WHERE id = 2"), [][]string{{"25"}})
	run(t, e, "DELETE FROM t WHERE id = 1")
	rowsEqual(t, run(t, e, "SELECT id FROM t ORDER BY id"), [][]string{{"2"}, {"3"}})
}

func TestAggregatesAndGroupBy(t *testing.T) {
	e := newEngine()
	run(t, e, "CREATE TABLE sales (id INTEGER PRIMARY KEY, region TEXT, amount INTEGER)")
	run(t, e, "INSERT INTO sales VALUES (1,'east',100),(2,'east',200),(3,'west',50),(4,'west',150)")
	got := run(t, e, "SELECT region, sum(amount), count(*) FROM sales GROUP BY region ORDER BY region")
	rowsEqual(t, got, [][]string{{"east", "300", "2"}, {"west", "200", "2"}})
	rowsEqual(t, run(t, e, "SELECT count(*) FROM sales"), [][]string{{"4"}})
	rowsEqual(t, run(t, e, "SELECT avg(amount) FROM sales WHERE region = 'east'"), [][]string{{"150"}})
}

func TestDistinctAndUnion(t *testing.T) {
	e := newEngine()
	run(t, e, "CREATE TABLE a (id INTEGER PRIMARY KEY, v INTEGER)")
	run(t, e, "INSERT INTO a VALUES (1,1),(2,1),(3,2)")
	rowsEqual(t, run(t, e, "SELECT DISTINCT v FROM a ORDER BY v"), [][]string{{"1"}, {"2"}})
	got := run(t, e, "SELECT v FROM a WHERE v = 1 UNION SELECT 9 ORDER BY v")
	rowsEqual(t, got, [][]string{{"1"}, {"9"}})
	got = run(t, e, "SELECT v FROM a UNION ALL SELECT v FROM a")
	if len(got.Rows) != 6 {
		t.Fatalf("UNION ALL should keep duplicates: got %d rows", len(got.Rows))
	}
}

func TestUniqueConstraint(t *testing.T) {
	e := newEngine()
	run(t, e, "CREATE TABLE u (id INTEGER PRIMARY KEY, email TEXT UNIQUE)")
	run(t, e, "INSERT INTO u VALUES (1, 'a@x.com')")
	_, err := e.Exec(context.Background(), "INSERT INTO u VALUES (2, 'a@x.com')")
	if err == nil || err.Error() != "UNIQUE constraint error: [email]" {
		t.Fatalf("expected unique violation, got %v", err)
	}
}

func TestNotNullAndDefault(t *testing.T) {
	e := newEngine()
	run(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, n TEXT NOT NULL, flag INTEGER DEFAULT 1)")
	_, err := e.Exec(context.Background(), "INSERT INTO t (id, n) VALUES (1, NULL)")
	if err == nil || err.Error() != "NOT NULL constraint error: [n]" {
		t.Fatalf("expected not-null violation, got %v", err)
	}
	run(t, e, "INSERT INTO t (id, n) VALUES (1, 'x')")
	rowsEqual(t, run(t, e, "SELECT flag FROM t WHERE id = 1"), [][]string{{"1"}})
}

func TestReturning(t *testing.T) {
	e := newEngine()
	run(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)")
	got := run(t, e, "INSERT INTO t VALUES (1, 42) RETURNING id, v")
	rowsEqual(t, got, [][]string{{"1", "42"}})
}

func TestImplicitRowID(t *testing.T) {
	e := newEngine()
	run(t, e, "CREATE TABLE log (msg TEXT)")
	run(t, e, "INSERT INTO log (msg) VALUES ('one'), ('two')")
	rowsEqual(t, run(t, e, "SELECT count(*) FROM log"), [][]string{{"2"}})
}
