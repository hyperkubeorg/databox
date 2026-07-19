// subquery_test.go covers expression subqueries (scalar, EXISTS, IN),
// derived tables, parenthesized join groups, and LATERAL (subquery.go,
// join.go).
package sql

import (
	"context"
	"strings"
	"testing"
)

func subqEngine(t *testing.T) *Engine {
	t.Helper()
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
	run(t, e, `CREATE TABLE orders (id INTEGER PRIMARY KEY, uid INTEGER, amount INTEGER)`)
	run(t, e, `INSERT INTO users VALUES (1, 'ann'), (2, 'bob'), (3, 'cid')`)
	run(t, e, `INSERT INTO orders VALUES (10, 1, 50), (11, 1, 70), (12, 2, 30), (13, NULL, 99)`)
	return e
}

func TestScalarSubquery(t *testing.T) {
	e := subqEngine(t)
	res := run(t, e, `SELECT name FROM users WHERE id = (SELECT max(uid) FROM orders)`)
	rowsEqual(t, res, [][]string{{"bob"}})
	// In the projection, and against an aggregate.
	res = run(t, e, `SELECT amount - (SELECT avg(amount) FROM orders) AS delta
		FROM orders WHERE id = 12`)
	rowsEqual(t, res, [][]string{{"-32.25"}})
	// Zero rows → NULL.
	res = run(t, e, `SELECT (SELECT amount FROM orders WHERE id = 999) IS NULL AS missing`)
	rowsEqual(t, res, [][]string{{"t"}})
	// LIMIT and ORDER BY inside the subquery.
	res = run(t, e, `SELECT name FROM users
		WHERE id = (SELECT uid FROM orders WHERE uid IS NOT NULL ORDER BY amount DESC LIMIT 1)`)
	rowsEqual(t, res, [][]string{{"ann"}})
}

func TestInAndExistsSubquery(t *testing.T) {
	e := subqEngine(t)
	res := run(t, e, `SELECT name FROM users WHERE id IN (SELECT uid FROM orders) ORDER BY name`)
	rowsEqual(t, res, [][]string{{"ann"}, {"bob"}})
	res = run(t, e, `SELECT name FROM users WHERE id NOT IN (SELECT uid FROM orders WHERE uid IS NOT NULL) ORDER BY name`)
	rowsEqual(t, res, [][]string{{"cid"}})
	// EXISTS is a plain boolean; NOT EXISTS negates it.
	res = run(t, e, `SELECT exists (SELECT 1 FROM orders WHERE amount > 90) AS big`)
	rowsEqual(t, res, [][]string{{"t"}})
	res = run(t, e, `SELECT name FROM users WHERE NOT EXISTS (SELECT 1 FROM orders WHERE amount > 1000)`)
	if len(res.Rows) != 3 {
		t.Fatalf("NOT EXISTS: %v", textRows(res))
	}
	// IN over an empty result set is simply false.
	res = run(t, e, `SELECT count(*) FROM users WHERE id IN (SELECT uid FROM orders WHERE amount > 1000)`)
	rowsEqual(t, res, [][]string{{"0"}})
}

func TestSubqueryInDML(t *testing.T) {
	e := subqEngine(t)
	run(t, e, `UPDATE orders SET amount = (SELECT max(amount) FROM orders) WHERE id = 12`)
	rowsEqual(t, run(t, e, `SELECT amount FROM orders WHERE id = 12`), [][]string{{"99"}})
	// avg is now (50+70+99+99)/4 = 79.5: the 50 and 70 rows fall.
	run(t, e, `DELETE FROM orders WHERE amount < (SELECT avg(amount) FROM orders)`)
	rowsEqual(t, run(t, e, `SELECT count(*) FROM orders`), [][]string{{"2"}})
	run(t, e, `INSERT INTO orders VALUES (14, (SELECT min(id) FROM users), 5)`)
	rowsEqual(t, run(t, e, `SELECT uid FROM orders WHERE id = 14`), [][]string{{"1"}})
}

func TestSubqueryErrors(t *testing.T) {
	e := subqEngine(t)
	for stmt, frag := range map[string]string{
		`SELECT (SELECT id, name FROM users) AS x`:                                              "one column",
		`SELECT (SELECT id FROM users) AS x`:                                                    "more than one row",
		`SELECT name FROM users u WHERE 1 = (SELECT count(*) FROM orders o WHERE o.uid = u.id)`: "LATERAL",
		`CREATE TABLE bad (x INTEGER DEFAULT (SELECT 1))`:                                       "not allowed in DEFAULT",
		`CREATE TABLE bad (x INTEGER, CHECK (x > (SELECT 0)))`:                                  "not allowed in CHECK",
	} {
		if _, err := e.Exec(context.Background(), stmt); err == nil || !strings.Contains(err.Error(), frag) {
			t.Fatalf("%s:\nwant %q, got %v", stmt, frag, err)
		}
	}
}

func TestDerivedTables(t *testing.T) {
	e := subqEngine(t)
	res := run(t, e, `SELECT big.name FROM (SELECT name, id FROM users WHERE id > 1) AS big ORDER BY big.id`)
	rowsEqual(t, res, [][]string{{"bob"}, {"cid"}})
	// A derived table joins like any other source.
	res = run(t, e, `SELECT u.name, t.total
		FROM users u
		JOIN (SELECT uid, sum(amount) AS total FROM orders WHERE uid IS NOT NULL GROUP BY uid) t
		  ON u.id = t.uid
		ORDER BY t.total DESC`)
	rowsEqual(t, res, [][]string{{"ann", "120"}, {"bob", "30"}})
	// SELECT * inside a derived table works (evaluated up front).
	res = run(t, e, `SELECT count(*) FROM (SELECT * FROM orders) o2`)
	rowsEqual(t, res, [][]string{{"4"}})
	// Alias required.
	if _, err := e.Exec(context.Background(), `SELECT 1 FROM (SELECT id FROM users)`); err == nil ||
		!strings.Contains(err.Error(), "needs an alias") {
		t.Fatalf("want alias error, got %v", err)
	}
}

func TestParenthesizedJoinGroups(t *testing.T) {
	e := subqEngine(t)
	run(t, e, `CREATE TABLE regions (uid INTEGER PRIMARY KEY, region TEXT)`)
	run(t, e, `INSERT INTO regions VALUES (1, 'north'), (2, 'south')`)
	// Right-grouped: users joins the (orders⋈regions) result.
	res := run(t, e, `SELECT u.name, r.region FROM users u
		JOIN (orders o JOIN regions r ON o.uid = r.uid) ON u.id = o.uid
		WHERE o.amount > 40 ORDER BY o.amount`)
	rowsEqual(t, res, [][]string{{"ann", "north"}, {"ann", "north"}})
	// Grouping changes outer-join semantics: LEFT of a grouped inner join.
	res = run(t, e, `SELECT u.name, o.amount FROM users u
		LEFT JOIN (orders o JOIN regions r ON o.uid = r.uid) ON u.id = o.uid
		ORDER BY u.name, o.amount`)
	rowsEqual(t, res, [][]string{
		{"ann", "50"}, {"ann", "70"}, {"bob", "30"}, {"cid", "NULL"},
	})
}

func TestLateralJoin(t *testing.T) {
	e := subqEngine(t)
	// Top order per user — the canonical LATERAL shape.
	res := run(t, e, `SELECT u.name, top.amount
		FROM users u
		JOIN LATERAL (SELECT amount FROM orders WHERE uid = u.id ORDER BY amount DESC LIMIT 1) top ON true
		ORDER BY u.name`)
	rowsEqual(t, res, [][]string{{"ann", "70"}, {"bob", "30"}})
	// LEFT JOIN LATERAL keeps users with no orders.
	res = run(t, e, `SELECT u.name, top.amount
		FROM users u
		LEFT JOIN LATERAL (SELECT amount FROM orders WHERE uid = u.id ORDER BY amount DESC LIMIT 1) top ON true
		ORDER BY u.name`)
	rowsEqual(t, res, [][]string{{"ann", "70"}, {"bob", "30"}, {"cid", "NULL"}})
	// Table-less lateral body computes per left row.
	res = run(t, e, `SELECT u.name, x.twice
		FROM users u
		JOIN LATERAL (SELECT u.id * 2 AS twice) x ON true
		ORDER BY u.name`)
	rowsEqual(t, res, [][]string{{"ann", "2"}, {"bob", "4"}, {"cid", "6"}})
	// Aggregates inside the lateral body (correlated count).
	res = run(t, e, `SELECT u.name, n.cnt
		FROM users u
		LEFT JOIN LATERAL (SELECT count(*) AS cnt FROM orders WHERE uid = u.id) n ON true
		ORDER BY u.name`)
	rowsEqual(t, res, [][]string{{"ann", "2"}, {"bob", "1"}, {"cid", "0"}})
}

func TestLateralErrors(t *testing.T) {
	e := subqEngine(t)
	for stmt, frag := range map[string]string{
		`SELECT 1 FROM users u RIGHT JOIN LATERAL (SELECT u.id AS x) t ON true`: "RIGHT",
		`SELECT 1 FROM LATERAL (SELECT 1 AS x) t`:                               "must follow",
		`SELECT 1 FROM users u JOIN LATERAL (SELECT * FROM orders) t ON true`:   "list the columns",
	} {
		if _, err := e.Exec(context.Background(), stmt); err == nil || !strings.Contains(err.Error(), frag) {
			t.Fatalf("%s:\nwant %q, got %v", stmt, frag, err)
		}
	}
	// A table named "lateral" keeps working.
	run(t, e, `CREATE TABLE lateral (id INTEGER PRIMARY KEY)`)
	run(t, e, `INSERT INTO lateral VALUES (1)`)
	rowsEqual(t, run(t, e, `SELECT id FROM lateral`), [][]string{{"1"}})
}
