// join_test.go covers the JOIN extension (join.go): inner/left/cross,
// hash and nested-loop paths, qualified references, ambiguity errors, and
// the unreserved-words guarantee.
package sql

import (
	"context"
	"strings"
	"testing"
)

func joinEngine(t *testing.T) *Engine {
	t.Helper()
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
	run(t, e, `CREATE TABLE orders (id INTEGER PRIMARY KEY, uid INTEGER, amount INTEGER)`)
	run(t, e, `INSERT INTO users VALUES (1, 'ann'), (2, 'bob'), (3, 'cid')`)
	run(t, e, `INSERT INTO orders VALUES (10, 1, 50), (11, 1, 70), (12, 2, 30), (13, NULL, 99)`)
	return e
}

func TestInnerJoinHashPath(t *testing.T) {
	e := joinEngine(t)
	res := run(t, e, `SELECT users.name, orders.amount FROM users
		JOIN orders ON users.id = orders.uid
		ORDER BY orders.amount`)
	rowsEqual(t, res, [][]string{{"bob", "30"}, {"ann", "50"}, {"ann", "70"}})
	// INNER JOIN spelling and aliases (AS and bare).
	res = run(t, e, `SELECT u.name, o.amount FROM users AS u
		INNER JOIN orders o ON u.id = o.uid
		WHERE o.amount > 40 ORDER BY o.amount`)
	rowsEqual(t, res, [][]string{{"ann", "50"}, {"ann", "70"}})
}

func TestLeftJoin(t *testing.T) {
	e := joinEngine(t)
	res := run(t, e, `SELECT u.name, o.amount FROM users u
		LEFT JOIN orders o ON u.id = o.uid
		ORDER BY u.name, o.amount`)
	rowsEqual(t, res, [][]string{
		{"ann", "50"}, {"ann", "70"}, {"bob", "30"}, {"cid", "NULL"},
	})
	// LEFT OUTER JOIN spelling; IS NULL finds the unmatched row.
	res = run(t, e, `SELECT u.name FROM users u
		LEFT OUTER JOIN orders o ON u.id = o.uid
		WHERE o.id IS NULL`)
	rowsEqual(t, res, [][]string{{"cid"}})
}

func TestCrossJoinAndNonEquiOn(t *testing.T) {
	e := joinEngine(t)
	res := run(t, e, `SELECT count(*) FROM users CROSS JOIN orders`)
	rowsEqual(t, res, [][]string{{"12"}})
	// Non-equi ON exercises the nested-loop path.
	res = run(t, e, `SELECT u.name, o.amount FROM users u
		JOIN orders o ON u.id < o.uid ORDER BY u.name, o.amount`)
	rowsEqual(t, res, [][]string{{"ann", "30"}})
}

func TestJoinNullKeysNeverMatch(t *testing.T) {
	e := joinEngine(t)
	// orders row 13 has uid NULL: it must not match any user.
	res := run(t, e, `SELECT count(*) FROM orders o JOIN users u ON o.uid = u.id`)
	rowsEqual(t, res, [][]string{{"3"}})
}

func TestJoinStarAndBareColumns(t *testing.T) {
	e := joinEngine(t)
	// * on a join expands to qualified labels, all tables in order.
	res := run(t, e, `SELECT * FROM users u JOIN orders o ON u.id = o.uid ORDER BY o.id LIMIT 1`)
	if strings.Join(res.Columns, ",") != "u.id,u.name,o.id,o.uid,o.amount" {
		t.Fatalf("star columns: %v", res.Columns)
	}
	rowsEqual(t, res, [][]string{{"1", "ann", "10", "1", "50"}})
	// Bare names that exist in exactly one table resolve unqualified.
	res = run(t, e, `SELECT name, amount FROM users u JOIN orders o ON u.id = o.uid ORDER BY amount LIMIT 1`)
	rowsEqual(t, res, [][]string{{"bob", "30"}})
}

func TestJoinThreeTables(t *testing.T) {
	e := joinEngine(t)
	run(t, e, `CREATE TABLE regions (uid INTEGER PRIMARY KEY, region TEXT)`)
	run(t, e, `INSERT INTO regions VALUES (1, 'north'), (2, 'south')`)
	res := run(t, e, `SELECT u.name, o.amount, r.region FROM users u
		JOIN orders o ON u.id = o.uid
		JOIN regions r ON r.uid = u.id
		ORDER BY o.amount`)
	rowsEqual(t, res, [][]string{
		{"bob", "30", "south"}, {"ann", "50", "north"}, {"ann", "70", "north"},
	})
}

func TestJoinAggregates(t *testing.T) {
	e := joinEngine(t)
	res := run(t, e, `SELECT u.name, count(*) AS n, sum(o.amount) AS total
		FROM users u JOIN orders o ON u.id = o.uid
		GROUP BY u.name ORDER BY u.name`)
	rowsEqual(t, res, [][]string{{"ann", "2", "120"}, {"bob", "1", "30"}})
}

func TestJoinErrors(t *testing.T) {
	e := joinEngine(t)
	for stmt, frag := range map[string]string{
		// "id" exists in both tables.
		`SELECT id FROM users u JOIN orders o ON u.id = o.uid`:     "ambiguous column",
		`SELECT x.name FROM users u JOIN orders o ON u.id = o.uid`: "unknown table",
		`SELECT u.nope FROM users u JOIN orders o ON u.id = o.uid`: "no such column",
		`SELECT 1 FROM users JOIN users ON users.id = users.id`:    "duplicate table",
		`SELECT 1 FROM users u JOIN nosuch n ON u.id = n.id`:       "no such table",
	} {
		if _, err := e.Exec(context.Background(), stmt); err == nil || !strings.Contains(err.Error(), frag) {
			t.Fatalf("%s:\nwant %q, got %v", stmt, frag, err)
		}
	}
}

func TestSingleTableQualifiedRefs(t *testing.T) {
	e := joinEngine(t)
	res := run(t, e, `SELECT u.name FROM users u WHERE u.id = 2`)
	rowsEqual(t, res, [][]string{{"bob"}})
	res = run(t, e, `SELECT users.name FROM users WHERE users.id = 1 ORDER BY users.name`)
	rowsEqual(t, res, [][]string{{"ann"}})
	// Qualified refs in UPDATE/DELETE WHERE clauses.
	run(t, e, `UPDATE orders SET amount = 31 WHERE orders.id = 12`)
	rowsEqual(t, run(t, e, `SELECT amount FROM orders WHERE id = 12`), [][]string{{"31"}})
	run(t, e, `DELETE FROM orders WHERE orders.id = 13`)
	rowsEqual(t, run(t, e, `SELECT count(*) FROM orders`), [][]string{{"3"}})
}

func TestJoinWordsNotReserved(t *testing.T) {
	e := NewEngineWithStore(NewMemStore(), "testdb")
	// Tables and columns named with join words keep working.
	run(t, e, `CREATE TABLE join (id INTEGER PRIMARY KEY, left TEXT, outer TEXT)`)
	run(t, e, `INSERT INTO join VALUES (1, 'l', 'o')`)
	rowsEqual(t, run(t, e, `SELECT left, outer FROM join WHERE id = 1`), [][]string{{"l", "o"}})
	rowsEqual(t, run(t, e, `SELECT join.left FROM join`), [][]string{{"l"}})
}

func TestInsertFromJoinSelect(t *testing.T) {
	e := joinEngine(t)
	run(t, e, `CREATE TABLE report (name TEXT, amount INTEGER)`)
	run(t, e, `INSERT INTO report SELECT u.name, o.amount FROM users u JOIN orders o ON u.id = o.uid`)
	rowsEqual(t, run(t, e, `SELECT count(*) FROM report`), [][]string{{"3"}})
	// The self-read guard sees join tables too.
	if _, err := e.Exec(context.Background(),
		`INSERT INTO report SELECT u.name, r.amount FROM users u JOIN report r ON u.name = r.name`); err == nil {
		t.Fatal("want self-read error through a join")
	}
}

func TestRightJoin(t *testing.T) {
	e := joinEngine(t)
	// RIGHT JOIN mirrors LEFT: the NULL-uid order survives, userless.
	res := run(t, e, `SELECT u.name, o.amount FROM users u
		RIGHT JOIN orders o ON u.id = o.uid
		ORDER BY o.amount`)
	rowsEqual(t, res, [][]string{
		{"bob", "30"}, {"ann", "50"}, {"ann", "70"}, {"NULL", "99"},
	})
	res = run(t, e, `SELECT o.amount FROM users u
		RIGHT OUTER JOIN orders o ON u.id = o.uid
		WHERE u.id IS NULL`)
	rowsEqual(t, res, [][]string{{"99"}})
}

func TestFullJoin(t *testing.T) {
	e := joinEngine(t)
	// FULL keeps both sides' unmatched rows: cid (no orders) and the
	// NULL-uid order.
	res := run(t, e, `SELECT u.name, o.amount FROM users u
		FULL JOIN orders o ON u.id = o.uid
		ORDER BY u.name, o.amount`)
	rowsEqual(t, res, [][]string{
		{"NULL", "99"},
		{"ann", "50"}, {"ann", "70"}, {"bob", "30"}, {"cid", "NULL"},
	})
	// Non-equi FULL exercises the nested-loop both-sides tracking:
	// one match (ann/70) + 2 unmatched users + 3 unmatched orders.
	res = run(t, e, `SELECT count(*) FROM users u FULL OUTER JOIN orders o ON u.id = o.uid AND o.amount > 60`)
	rowsEqual(t, res, [][]string{{"6"}})
}

func TestUsingJoin(t *testing.T) {
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE emp (id INTEGER PRIMARY KEY, dept_id INTEGER, name TEXT)`)
	run(t, e, `CREATE TABLE dept (dept_id INTEGER PRIMARY KEY, dept TEXT)`)
	run(t, e, `INSERT INTO emp VALUES (1, 10, 'ann'), (2, 20, 'bob'), (3, 30, 'cid')`)
	run(t, e, `INSERT INTO dept VALUES (10, 'eng'), (20, 'ops')`)
	// The USING column resolves bare, coalesced.
	res := run(t, e, `SELECT dept_id, name, dept FROM emp JOIN dept USING (dept_id) ORDER BY dept_id`)
	rowsEqual(t, res, [][]string{{"10", "ann", "eng"}, {"20", "bob", "ops"}})
	// FULL ... USING: the bare column coalesces from whichever side exists.
	run(t, e, `INSERT INTO dept VALUES (40, 'hr')`)
	res = run(t, e, `SELECT dept_id, name, dept FROM emp FULL JOIN dept USING (dept_id) ORDER BY dept_id`)
	rowsEqual(t, res, [][]string{
		{"10", "ann", "eng"}, {"20", "bob", "ops"}, {"30", "cid", "NULL"}, {"40", "NULL", "hr"},
	})
	// SELECT * shows the USING column once, bare and first.
	star := run(t, e, `SELECT * FROM emp JOIN dept USING (dept_id) ORDER BY dept_id LIMIT 1`)
	if strings.Join(star.Columns, ",") != "dept_id,emp.id,emp.name,dept.dept" {
		t.Fatalf("star columns: %v", star.Columns)
	}
}

func TestNaturalJoin(t *testing.T) {
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT)`)
	run(t, e, `CREATE TABLE b (id INTEGER PRIMARY KEY, w TEXT)`)
	run(t, e, `INSERT INTO a VALUES (1, 'x'), (2, 'y')`)
	run(t, e, `INSERT INTO b VALUES (2, 'B2'), (3, 'B3')`)
	rowsEqual(t, run(t, e, `SELECT id, v, w FROM a NATURAL JOIN b`), [][]string{{"2", "y", "B2"}})
	rowsEqual(t, run(t, e, `SELECT id, v, w FROM a NATURAL LEFT JOIN b ORDER BY id`),
		[][]string{{"1", "x", "NULL"}, {"2", "y", "B2"}})
	rowsEqual(t, run(t, e, `SELECT id FROM a NATURAL FULL JOIN b ORDER BY id`),
		[][]string{{"1"}, {"2"}, {"3"}})
	// No common columns: NATURAL degrades to CROSS (pg semantics).
	run(t, e, `CREATE TABLE c (n INTEGER PRIMARY KEY)`)
	run(t, e, `INSERT INTO c VALUES (7), (8)`)
	rowsEqual(t, run(t, e, `SELECT count(*) FROM a NATURAL JOIN c`), [][]string{{"4"}})
}

func TestCommaJoin(t *testing.T) {
	e := joinEngine(t)
	res := run(t, e, `SELECT u.name, o.amount FROM users u, orders o
		WHERE u.id = o.uid ORDER BY o.amount`)
	rowsEqual(t, res, [][]string{{"bob", "30"}, {"ann", "50"}, {"ann", "70"}})
	rowsEqual(t, run(t, e, `SELECT count(*) FROM users, orders`), [][]string{{"12"}})
}

func TestSelfJoin(t *testing.T) {
	e := joinEngine(t)
	res := run(t, e, `SELECT a.name, b.name FROM users a JOIN users b ON a.id < b.id ORDER BY a.name, b.name`)
	rowsEqual(t, res, [][]string{
		{"ann", "bob"}, {"ann", "cid"}, {"bob", "cid"},
	})
}

func TestUsingErrors(t *testing.T) {
	e := joinEngine(t)
	for stmt, frag := range map[string]string{
		`SELECT 1 FROM users u JOIN orders o USING (name)`:   "not on the right side",
		`SELECT 1 FROM users u JOIN orders o USING (amount)`: "not on the left side",
	} {
		if _, err := e.Exec(context.Background(), stmt); err == nil || !strings.Contains(err.Error(), frag) {
			t.Fatalf("%s: want %q, got %v", stmt, frag, err)
		}
	}
}

func TestNewJoinWordsNotReserved(t *testing.T) {
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE natural (id INTEGER PRIMARY KEY, using TEXT, full TEXT, right TEXT)`)
	run(t, e, `INSERT INTO natural VALUES (1, 'u', 'f', 'r')`)
	rowsEqual(t, run(t, e, `SELECT using, full, right FROM natural WHERE id = 1`),
		[][]string{{"u", "f", "r"}})
}
