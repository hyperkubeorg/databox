// alter_test.go covers the ALTER TABLE extension (alter.go): add/drop/
// rename column, rename table, and every dependency guard.
package sql

import (
	"context"
	"strings"
	"testing"
)

func alterEngine(t *testing.T) *Engine {
	t.Helper()
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE people (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	run(t, e, `INSERT INTO people VALUES (1, 'ann'), (2, 'bob')`)
	return e
}

func TestAlterAddColumn(t *testing.T) {
	e := alterEngine(t)
	run(t, e, `ALTER TABLE people ADD COLUMN age INTEGER`)
	// Existing rows backfilled with NULL; the column is selectable at once.
	rowsEqual(t, run(t, e, `SELECT name, age FROM people ORDER BY id`),
		[][]string{{"ann", "NULL"}, {"bob", "NULL"}})
	// With DEFAULT: existing rows backfill with the default.
	run(t, e, `ALTER TABLE people ADD COLUMN city TEXT DEFAULT 'unknown'`)
	rowsEqual(t, run(t, e, `SELECT city FROM people WHERE id = 1`), [][]string{{"unknown"}})
	// New inserts keep applying it.
	run(t, e, `INSERT INTO people (id, name) VALUES (3, 'cid')`)
	rowsEqual(t, run(t, e, `SELECT city FROM people WHERE id = 3`), [][]string{{"unknown"}})
	// NOT NULL + DEFAULT works on a populated table.
	run(t, e, `ALTER TABLE people ADD flag BOOLEAN NOT NULL DEFAULT false`)
	rowsEqual(t, run(t, e, `SELECT count(*) FROM people WHERE flag = false`), [][]string{{"3"}})
	res := run(t, e, `DESCRIBE people`)
	if len(res.Rows) != 5 {
		t.Fatalf("columns after adds: %v", textRows(res))
	}
}

func TestAlterAddErrors(t *testing.T) {
	e := alterEngine(t)
	for stmt, frag := range map[string]string{
		`ALTER TABLE people ADD COLUMN name TEXT`:        "already exists",
		`ALTER TABLE people ADD COLUMN nn TEXT NOT NULL`: "without DEFAULT",
		`ALTER TABLE people ADD COLUMN u TEXT UNIQUE`:    "only NOT NULL and DEFAULT",
		`ALTER TABLE people ADD COLUMN s SERIAL`:         "auto-increment",
		`ALTER TABLE nosuch ADD COLUMN x TEXT`:           "no such table",
	} {
		if _, err := e.Exec(context.Background(), stmt); err == nil || !strings.Contains(err.Error(), frag) {
			t.Fatalf("%s: want %q, got %v", stmt, frag, err)
		}
	}
	// NOT NULL without DEFAULT is fine on an empty table.
	run(t, e, `CREATE TABLE empty (id INTEGER PRIMARY KEY)`)
	run(t, e, `ALTER TABLE empty ADD COLUMN req TEXT NOT NULL`)
}

func TestAlterDropColumn(t *testing.T) {
	e := alterEngine(t)
	run(t, e, `ALTER TABLE people ADD COLUMN age INTEGER DEFAULT 30`)
	run(t, e, `ALTER TABLE people DROP COLUMN age`)
	res := run(t, e, `DESCRIBE people`)
	if len(res.Rows) != 2 {
		t.Fatalf("columns after drop: %v", textRows(res))
	}
	// The dropped column is gone from rows too: referencing it now errors.
	if _, err := e.Exec(context.Background(), `SELECT age FROM people`); err == nil {
		t.Fatal("dropped column still readable")
	}
	// Guards: PK, index, and CHECK references refuse the drop.
	run(t, e, `CREATE TABLE guarded (id INTEGER PRIMARY KEY, v INTEGER, w INTEGER, CHECK (w >= 0))`)
	run(t, e, `CREATE INDEX guarded_v ON guarded (v)`)
	for stmt, frag := range map[string]string{
		`ALTER TABLE guarded DROP COLUMN id`: "primary key",
		`ALTER TABLE guarded DROP COLUMN v`:  "used by index",
		`ALTER TABLE guarded DROP COLUMN w`:  "check constraint",
	} {
		if _, err := e.Exec(context.Background(), stmt); err == nil || !strings.Contains(err.Error(), frag) {
			t.Fatalf("%s: want %q, got %v", stmt, frag, err)
		}
	}
}

func TestAlterRenameColumn(t *testing.T) {
	e := alterEngine(t)
	run(t, e, `ALTER TABLE people RENAME COLUMN name TO full_name`)
	rowsEqual(t, run(t, e, `SELECT full_name FROM people ORDER BY id`),
		[][]string{{"ann"}, {"bob"}})
	if _, err := e.Exec(context.Background(), `SELECT name FROM people`); err == nil {
		t.Fatal("old column name still readable")
	}
	// Index definitions and CHECK expressions follow the rename.
	run(t, e, `CREATE TABLE m (id INTEGER PRIMARY KEY, v INTEGER, CHECK (v >= 0))`)
	run(t, e, `CREATE INDEX m_v ON m (v)`)
	run(t, e, `INSERT INTO m VALUES (1, 5)`)
	run(t, e, `ALTER TABLE m RENAME COLUMN v TO val`)
	idx := run(t, e, `SHOW INDEXES FROM m`)
	rowsEqual(t, idx, [][]string{{"m", "PRIMARY", "id", "true"}, {"m", "m_v", "val", "false"}})
	ddl := *run(t, e, `SHOW CREATE TABLE m`).Rows[0][1]
	if !strings.Contains(ddl, "CHECK (val >= 0)") {
		t.Fatalf("check not renamed:\n%s", ddl)
	}
	// The renamed CHECK still enforces.
	if _, err := e.Exec(context.Background(), `INSERT INTO m VALUES (2, -1)`); err == nil {
		t.Fatal("check lost after rename")
	}
	// The renamed index still narrows scans correctly (value intact).
	rowsEqual(t, run(t, e, `SELECT val FROM m WHERE val = 5`), [][]string{{"5"}})
}

func TestAlterRenameTable(t *testing.T) {
	e := alterEngine(t)
	run(t, e, `CREATE INDEX people_name ON people (name)`)
	run(t, e, `ALTER TABLE people RENAME TO humans`)
	rowsEqual(t, run(t, e, `SELECT name FROM humans ORDER BY id`),
		[][]string{{"ann"}, {"bob"}})
	rowsEqual(t, run(t, e, `SHOW TABLES`), [][]string{{"humans"}})
	if _, err := e.Exec(context.Background(), `SELECT * FROM people`); err == nil {
		t.Fatal("old table name still readable")
	}
	// Indexes moved with the table and still serve lookups.
	idx := run(t, e, `SHOW INDEXES FROM humans`)
	rowsEqual(t, idx, [][]string{{"humans", "PRIMARY", "id", "true"}, {"humans", "people_name", "name", "false"}})
	rowsEqual(t, run(t, e, `SELECT id FROM humans WHERE name = 'bob'`), [][]string{{"2"}})
	// Writes keep working under the new name.
	run(t, e, `INSERT INTO humans VALUES (3, 'cid')`)
	rowsEqual(t, run(t, e, `SELECT count(*) FROM humans`), [][]string{{"3"}})
	// Rename onto an existing table refuses.
	run(t, e, `CREATE TABLE blocker (id INTEGER PRIMARY KEY)`)
	if _, err := e.Exec(context.Background(), `ALTER TABLE humans RENAME TO blocker`); err == nil {
		t.Fatal("rename onto existing table allowed")
	}
}

// TestAlterWordsNotReserved: ALTER's grammar words stay ordinary
// identifiers everywhere else.
func TestAlterWordsNotReserved(t *testing.T) {
	e := NewEngineWithStore(NewMemStore(), "testdb")
	run(t, e, `CREATE TABLE alter (add INTEGER PRIMARY KEY, rename TEXT, to TEXT)`)
	run(t, e, `INSERT INTO alter VALUES (1, 'r', 't')`)
	rowsEqual(t, run(t, e, `SELECT add, rename, to FROM alter`), [][]string{{"1", "r", "t"}})
	run(t, e, `ALTER TABLE alter ADD COLUMN column TEXT DEFAULT 'c'`)
	rowsEqual(t, run(t, e, `SELECT column FROM alter`), [][]string{{"c"}})
}
