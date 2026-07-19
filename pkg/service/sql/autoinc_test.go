// autoinc_test.go covers auto-increment primary keys (SERIAL /
// AUTO_INCREMENT) and the UUID column type with gen_random_uuid().
package sql

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

func TestAutoIncrementBasics(t *testing.T) {
	e := NewEngineWithStore(NewMemStore(), "testdb")
	// Both spellings must parse; SERIAL is the pg form.
	run(t, e, `CREATE TABLE a (id SERIAL PRIMARY KEY, v TEXT)`)
	run(t, e, `CREATE TABLE b (id INTEGER AUTO_INCREMENT PRIMARY KEY, v TEXT)`)

	res := run(t, e, `INSERT INTO a (v) VALUES ('x'), ('y') RETURNING id, v`)
	rowsEqual(t, res, [][]string{{"1", "x"}, {"2", "y"}})
	// Explicit NULL also draws from the counter (MySQL semantics).
	res = run(t, e, `INSERT INTO a (id, v) VALUES (NULL, 'z') RETURNING id`)
	rowsEqual(t, res, [][]string{{"3"}})
	// DESCRIBE reports the flag; the type stays INTEGER.
	res = run(t, e, `DESCRIBE a`)
	rowsEqual(t, res, [][]string{
		{"id", "INTEGER", "NO", "PRI", "NULL", "auto_increment"},
		{"v", "TEXT", "YES", "", "NULL", ""},
	})
}

func TestAutoIncrementExplicitValueRatchets(t *testing.T) {
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE t (id SERIAL PRIMARY KEY, v TEXT)`)
	run(t, e, `INSERT INTO t (v) VALUES ('a')`)             // id 1
	run(t, e, `INSERT INTO t (id, v) VALUES (100, 'jump')`) // explicit
	res := run(t, e, `INSERT INTO t (v) VALUES ('b') RETURNING id`)
	rowsEqual(t, res, [][]string{{"101"}}) // counter ratcheted past 100
	// A low explicit value must not rewind the counter.
	run(t, e, `INSERT INTO t (id, v) VALUES (50, 'mid')`)
	res = run(t, e, `INSERT INTO t (v) VALUES ('c') RETURNING id`)
	rowsEqual(t, res, [][]string{{"102"}})
}

func TestAutoIncrementRoundTrip(t *testing.T) {
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE t (id SERIAL PRIMARY KEY, v TEXT)`)
	ddl := *run(t, e, `SHOW CREATE TABLE t`).Rows[0][1]
	if !strings.Contains(ddl, "id INTEGER AUTO_INCREMENT") {
		t.Fatalf("DDL missing AUTO_INCREMENT:\n%s", ddl)
	}
	e2 := NewEngineWithStore(NewMemStore(), "other")
	if _, err := e2.Exec(context.Background(), ddl); err != nil {
		t.Fatalf("round-trip: %v\n%s", err, ddl)
	}
	rowsEqual(t, run(t, e2, `INSERT INTO t (v) VALUES ('x') RETURNING id`), [][]string{{"1"}})
}

func TestAutoIncrementErrors(t *testing.T) {
	e := NewEngineWithStore(NewMemStore(), "testdb")
	for stmt, frag := range map[string]string{
		`CREATE TABLE x (id TEXT AUTO_INCREMENT PRIMARY KEY)`:                      "requires an INTEGER column",
		`CREATE TABLE x (id SERIAL, v TEXT)`:                                       "must be the primary key",
		`CREATE TABLE x (id SERIAL PRIMARY KEY, n INTEGER AUTO_INCREMENT)`:         "must be the primary key",
		`CREATE TABLE x (a INTEGER, b INTEGER AUTO_INCREMENT, PRIMARY KEY (a, b))`: "must be the primary key",
	} {
		if _, err := e.Exec(context.Background(), stmt); err == nil || !strings.Contains(err.Error(), frag) {
			t.Fatalf("%s: want %q, got %v", stmt, frag, err)
		}
	}
	// The attribute words stay unreserved: a column NAMED auto_increment
	// still works.
	run(t, e, `CREATE TABLE ok (id INTEGER PRIMARY KEY, auto_increment TEXT)`)
	run(t, e, `INSERT INTO ok VALUES (1, 'fine')`)
	rowsEqual(t, run(t, e, `SELECT auto_increment FROM ok`), [][]string{{"fine"}})
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestUUIDPrimaryKey(t *testing.T) {
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE items (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), name TEXT)`)
	res := run(t, e, `INSERT INTO items (name) VALUES ('one'), ('two') RETURNING id`)
	if len(res.Rows) != 2 {
		t.Fatalf("rows: %v", textRows(res))
	}
	a, b := *res.Rows[0][0], *res.Rows[1][0]
	if !uuidRe.MatchString(a) || !uuidRe.MatchString(b) {
		t.Fatalf("not canonical v4 UUIDs: %q %q", a, b)
	}
	if a == b {
		t.Fatalf("generated UUIDs collide: %q", a)
	}
	// Point lookup by the generated key.
	rowsEqual(t, run(t, e, `SELECT name FROM items WHERE id = '`+a+`'`), [][]string{{"one"}})
	// DESCRIBE reports the declared type.
	res = run(t, e, `DESCRIBE items`)
	if got := *res.Rows[0][1]; got != "UUID" {
		t.Fatalf("type: %q", got)
	}
}

func TestUUIDNormalizationAndValidation(t *testing.T) {
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE u (id UUID PRIMARY KEY, note TEXT)`)
	// Uppercase and un-hyphenated forms normalize to canonical lowercase.
	run(t, e, `INSERT INTO u VALUES ('123E4567-E89B-42D3-A456-426614174000', 'upper')`)
	run(t, e, `INSERT INTO u VALUES ('00112233445566778899aabbccddeeff', 'bare')`)
	rowsEqual(t, run(t, e, `SELECT id FROM u ORDER BY note`), [][]string{
		{"00112233-4455-6677-8899-aabbccddeeff"},
		{"123e4567-e89b-42d3-a456-426614174000"},
	})
	// The canonical form is one comparable spelling: mixed-case lookups hit.
	rowsEqual(t, run(t, e, `SELECT note FROM u WHERE id = '123E4567E89B42D3A456426614174000'`), [][]string{}) // raw compare: no normalization on query literals
	// Invalid forms are rejected on write, INSERT and UPDATE both.
	for _, stmt := range []string{
		`INSERT INTO u VALUES ('not-a-uuid', 'bad')`,
		`INSERT INTO u VALUES ('123e4567-e89b-42d3-a456-42661417400', 'short')`,
		`UPDATE u SET id = 'zz112233445566778899aabbccddeeff' WHERE note = 'bare'`,
	} {
		if _, err := e.Exec(context.Background(), stmt); err == nil || !strings.Contains(err.Error(), "invalid UUID") {
			t.Fatalf("%s: want invalid UUID error, got %v", stmt, err)
		}
	}
}

func TestUUIDRoundTrip(t *testing.T) {
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE items (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), name TEXT)`)
	ddl := *run(t, e, `SHOW CREATE TABLE items`).Rows[0][1]
	if !strings.Contains(ddl, "id UUID") || !strings.Contains(ddl, "DEFAULT GEN_RANDOM_UUID()") && !strings.Contains(ddl, "DEFAULT gen_random_uuid()") {
		t.Fatalf("DDL:\n%s", ddl)
	}
	e2 := NewEngineWithStore(NewMemStore(), "other")
	if _, err := e2.Exec(context.Background(), ddl); err != nil {
		t.Fatalf("round-trip: %v\n%s", err, ddl)
	}
	res := run(t, e2, `INSERT INTO items (name) VALUES ('x') RETURNING id`)
	if !uuidRe.MatchString(*res.Rows[0][0]) {
		t.Fatalf("round-tripped table lost UUID generation: %v", textRows(res))
	}
}
