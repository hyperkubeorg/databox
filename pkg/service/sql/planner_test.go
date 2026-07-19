// planner_test.go verifies the query planner two ways at once: every case
// asserts the access path that was chosen (point/range/index/full scan)
// AND that the planned execution returns exactly what a forced full scan
// returns. The second engine with noPlanner=true is the trusted baseline —
// same store contents, no pushdown of any kind.
package sql

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// plannerPair returns two engines over independent stores that received
// the same setup statements: one planning, one full-scan baseline.
func plannerPair(t *testing.T, setup ...string) (planned, baseline *Engine) {
	t.Helper()
	planned = NewEngineWithStore(NewMemStore(), "test")
	baseline = NewEngineWithStore(NewMemStore(), "test")
	baseline.noPlanner = true
	for _, s := range setup {
		for _, e := range []*Engine{planned, baseline} {
			if _, err := e.Exec(context.Background(), s); err != nil {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
	}
	return planned, baseline
}

// textOf flattens an ExecResult to comparable string rows ("NULL" for nil).
func textOf(r ExecResult) []string {
	out := make([]string, len(r.Rows))
	for i, row := range r.Rows {
		parts := make([]string, len(row))
		for j, v := range row {
			if v == nil {
				parts[j] = "NULL"
			} else {
				parts[j] = *v
			}
		}
		out[i] = strings.Join(parts, "|")
	}
	return out
}

// checkEquivalent runs sql on both engines and requires identical results.
// ordered=false compares as multisets (a plan may legally change row order
// when no ORDER BY constrains it).
func checkEquivalent(t *testing.T, planned, baseline *Engine, sql string, ordered bool) ExecResult {
	t.Helper()
	pres, perr := planned.Exec(context.Background(), sql)
	bres, berr := baseline.Exec(context.Background(), sql)
	if (perr == nil) != (berr == nil) {
		t.Fatalf("%q: planned err=%v baseline err=%v", sql, perr, berr)
	}
	if perr != nil {
		if perr.Error() != berr.Error() {
			t.Fatalf("%q: error text diverged: planned %q baseline %q", sql, perr, berr)
		}
		return ExecResult{}
	}
	if len(pres) != len(bres) {
		t.Fatalf("%q: result count %d vs %d", sql, len(pres), len(bres))
	}
	for i := range pres {
		pt, bt := textOf(pres[i]), textOf(bres[i])
		if !ordered {
			sort.Strings(pt)
			sort.Strings(bt)
		}
		if strings.Join(pt, "\n") != strings.Join(bt, "\n") {
			t.Fatalf("%q: rows diverged\nplanned:\n%s\nbaseline:\n%s",
				sql, strings.Join(pt, "\n"), strings.Join(bt, "\n"))
		}
		if pres[i].Tag != bres[i].Tag {
			t.Fatalf("%q: tag %q vs %q", sql, pres[i].Tag, bres[i].Tag)
		}
	}
	return pres[len(pres)-1]
}

// wantPath asserts structural properties of the last chosen plan.
func wantPath(t *testing.T, e *Engine, wantIndex string, wantFull bool, wantRanges int) {
	t.Helper()
	p := e.lastPlan
	if p == nil {
		t.Fatalf("no plan was recorded")
	}
	if p.isFullScan() != wantFull {
		t.Fatalf("full scan = %v, want %v (plan %s)", p.isFullScan(), wantFull, p.describe())
	}
	gotIndex := ""
	if p.index != nil {
		gotIndex = p.index.Name
	}
	if gotIndex != wantIndex {
		t.Fatalf("index = %q, want %q (plan %s)", gotIndex, wantIndex, p.describe())
	}
	if !wantFull && len(p.ranges) != wantRanges {
		t.Fatalf("ranges = %d, want %d (plan %s)", len(p.ranges), wantRanges, p.describe())
	}
}

const plannerTable = `CREATE TABLE t (
	id INTEGER PRIMARY KEY,
	a INTEGER,
	b INTEGER,
	s TEXT,
	d DOUBLE PRECISION
)`

func plannerSetup() []string {
	rows := make([]string, 0, 20)
	for i := 1; i <= 20; i++ {
		rows = append(rows, fmt.Sprintf("(%d, %d, %d, 's%02d', %d.5)", i, i%5, i%3, i, i))
	}
	return []string{
		plannerTable,
		"CREATE INDEX idx_a ON t(a)",
		"CREATE INDEX idx_ab2 ON t(b, a)",
		"INSERT INTO t VALUES " + strings.Join(rows, ", "),
	}
}

func TestPlannerPKPaths(t *testing.T) {
	p, b := plannerPair(t, plannerSetup()...)

	// Point lookup on the primary key: one range, no index.
	checkEquivalent(t, p, b, "SELECT * FROM t WHERE id = 7", false)
	wantPath(t, p, "", false, 1)

	// PK ranges, inclusive and exclusive.
	for _, q := range []string{
		"SELECT id FROM t WHERE id > 15",
		"SELECT id FROM t WHERE id >= 15",
		"SELECT id FROM t WHERE id < 4",
		"SELECT id FROM t WHERE id <= 4",
		"SELECT id FROM t WHERE id BETWEEN 5 AND 9",
		"SELECT id FROM t WHERE id > 3 AND id < 8",
	} {
		checkEquivalent(t, p, b, q, false)
		wantPath(t, p, "", false, 1)
	}

	// IN on the PK fans out into one point range per distinct value.
	checkEquivalent(t, p, b, "SELECT id FROM t WHERE id IN (3, 9, 9, 14)", false)
	wantPath(t, p, "", false, 3)

	// No usable predicate: full scan.
	checkEquivalent(t, p, b, "SELECT id FROM t WHERE a + b > 2", false)
	wantPath(t, p, "", true, 0)
}

func TestPlannerSecondaryIndexPaths(t *testing.T) {
	p, b := plannerPair(t, plannerSetup()...)

	// Equality on an indexed column drives the index.
	checkEquivalent(t, p, b, "SELECT * FROM t WHERE a = 3", false)
	wantPath(t, p, "idx_a", false, 1)

	// Range over the index.
	checkEquivalent(t, p, b, "SELECT id FROM t WHERE a >= 2 AND a < 4", false)
	wantPath(t, p, "idx_a", false, 1)

	// IN over the index.
	checkEquivalent(t, p, b, "SELECT id FROM t WHERE a IN (1, 4)", false)
	wantPath(t, p, "idx_a", false, 2)

	// Composite index: equality on the leading column alone...
	checkEquivalent(t, p, b, "SELECT id FROM t WHERE b = 1", false)
	wantPath(t, p, "idx_ab2", false, 1)

	// ...and equality prefix plus range on the next column. Two pinned
	// columns beat the single-column index.
	checkEquivalent(t, p, b, "SELECT id FROM t WHERE b = 2 AND a > 1", false)
	wantPath(t, p, "idx_ab2", false, 1)

	// Equality beats an open range on another index (chai picks the same).
	checkEquivalent(t, p, b, "SELECT id FROM t WHERE a > 1 AND b = 2", false)
	wantPath(t, p, "idx_ab2", false, 1)

	// The residual filter still applies: extra predicates narrow further.
	checkEquivalent(t, p, b, "SELECT id FROM t WHERE a = 3 AND s > 's10'", false)
	wantPath(t, p, "idx_a", false, 1)
}

func TestPlannerCompositePK(t *testing.T) {
	p, b := plannerPair(t,
		"CREATE TABLE c (x INTEGER, y INTEGER, v TEXT, PRIMARY KEY (x, y))",
		"INSERT INTO c VALUES (1,1,'a'),(1,2,'b'),(2,1,'c'),(2,2,'d'),(3,5,'e')",
	)
	// Prefix equality on x becomes one PK range.
	checkEquivalent(t, p, b, "SELECT * FROM c WHERE x = 1", false)
	wantPath(t, p, "", false, 1)

	// Full-key equality is a point range.
	checkEquivalent(t, p, b, "SELECT v FROM c WHERE x = 2 AND y = 2", false)
	wantPath(t, p, "", false, 1)

	// Prefix equality plus range on the second column.
	checkEquivalent(t, p, b, "SELECT v FROM c WHERE x = 1 AND y >= 2", false)
	wantPath(t, p, "", false, 1)

	// A predicate only on the second column cannot use the PK order.
	checkEquivalent(t, p, b, "SELECT v FROM c WHERE y = 1", false)
	wantPath(t, p, "", true, 0)
}

func TestPlannerOrderPushdown(t *testing.T) {
	p, b := plannerPair(t, plannerSetup()...)

	// ORDER BY on the PK of a full table read: served by scan order.
	res := checkEquivalent(t, p, b, "SELECT id FROM t ORDER BY id", true)
	if len(res.Rows) != 20 {
		t.Fatalf("expected 20 rows, got %d", len(res.Rows))
	}
	// ORDER BY on the index driving the WHERE.
	checkEquivalent(t, p, b, "SELECT id, a FROM t WHERE a >= 2 ORDER BY a", true)
	wantPath(t, p, "idx_a", false, 1)

	// Equality-pinned prefix may be skipped: idx_ab2(b, a), b fixed.
	checkEquivalent(t, p, b, "SELECT id FROM t WHERE b = 1 ORDER BY a", true)
	wantPath(t, p, "idx_ab2", false, 1)

	// DESC is not pushed down but must still come back correct.
	checkEquivalent(t, p, b, "SELECT id FROM t WHERE a = 2 ORDER BY id DESC", true)

	// LIMIT/OFFSET ride on the pushed-down order.
	checkEquivalent(t, p, b, "SELECT id FROM t ORDER BY id LIMIT 5 OFFSET 3", true)

	// Alias shadowing: the output column "a" is really b, so the sort must
	// NOT be skipped even though the scan is ordered by the table's a.
	checkEquivalent(t, p, b, "SELECT b AS a FROM t WHERE a >= 0 ORDER BY a", true)

	// ORDER BY an expression is never pushed down.
	checkEquivalent(t, p, b, "SELECT id FROM t WHERE a = 1 ORDER BY id % 4, id", true)
}

func TestPlannerSafetyFallbacks(t *testing.T) {
	p, b := plannerPair(t,
		"CREATE TABLE s (id INTEGER PRIMARY KEY, txt TEXT, d DOUBLE PRECISION)",
		"CREATE INDEX idx_txt ON s(txt)",
		"CREATE INDEX idx_d ON s(d)",
		"INSERT INTO s VALUES (1, '5', 1.5), (2, '05', -0.0), (3, ' 5', 0.0), (4, 'x', 'NaN'), (5, '6', 9007199254740993.0)",
	)

	// A number compared against a TEXT column matches '5', '05', ' 5' by
	// the dialect's coercion; a key range on the text '5' would miss two of
	// them, so the planner must fall back to a full scan.
	checkEquivalent(t, p, b, "SELECT id FROM s WHERE txt = 5", false)
	wantPath(t, p, "", true, 0)

	// Text against a text column is exact and plans fine.
	checkEquivalent(t, p, b, "SELECT id FROM s WHERE txt = '5'", false)
	wantPath(t, p, "idx_txt", false, 1)

	// -0.0 and 0.0 compare equal and must land in the same key range.
	checkEquivalent(t, p, b, "SELECT id FROM s WHERE d = 0.0", false)
	wantPath(t, p, "idx_d", false, 1)
	checkEquivalent(t, p, b, "SELECT id FROM s WHERE d = -0.0", false)
	checkEquivalent(t, p, b, "SELECT id FROM s WHERE d >= 0.0", false)

	// NaN behaves as a value equal only to itself, on both paths.
	checkEquivalent(t, p, b, "SELECT id FROM s WHERE d = 'NaN'", false)
	checkEquivalent(t, p, b, "SELECT id FROM s WHERE d > 5", false)

	// now() must never become a plan bound (the residual filter would
	// re-evaluate it at a later instant).
	checkEquivalent(t, p, b, "SELECT id FROM s WHERE id > 0 AND txt = CAST(now() AS TEXT)", false)
	wantPath(t, p, "", false, 1) // the id > 0 half still plans

	// A lossy constant (1.5 against an INTEGER key) is not usable.
	checkEquivalent(t, p, b, "SELECT id FROM s WHERE id = 1.5", false)
	wantPath(t, p, "", true, 0)

	// An integral double IS usable against an INTEGER key...
	checkEquivalent(t, p, b, "SELECT id FROM s WHERE id = 2.0", false)
	wantPath(t, p, "", false, 1)

	// ...but not above 2^53 where float64 collapses neighbors.
	checkEquivalent(t, p, b, "SELECT id FROM s WHERE id = 9007199254740993.0", false)
	wantPath(t, p, "", true, 0)

	// Contradictory equalities over-read nothing and return nothing.
	checkEquivalent(t, p, b, "SELECT id FROM s WHERE id = 1 AND id = 2", false)
}

func TestPlannerDML(t *testing.T) {
	p, b := plannerPair(t, plannerSetup()...)

	// UPDATE through the secondary index.
	checkEquivalent(t, p, b, "UPDATE t SET s = 'upd' WHERE a = 2", false)
	wantPath(t, p, "idx_a", false, 1)
	checkEquivalent(t, p, b, "SELECT id, s FROM t ORDER BY id", true)

	// UPDATE through a PK range.
	checkEquivalent(t, p, b, "UPDATE t SET b = b + 10 WHERE id BETWEEN 3 AND 6", false)
	checkEquivalent(t, p, b, "SELECT id, b FROM t ORDER BY id", true)

	// DELETE through the PK and through an index, then verify the leftover
	// table and that index scans still agree with full scans.
	checkEquivalent(t, p, b, "DELETE FROM t WHERE id IN (1, 20)", false)
	wantPath(t, p, "", false, 2)
	checkEquivalent(t, p, b, "DELETE FROM t WHERE a = 4", false)
	wantPath(t, p, "idx_a", false, 1)
	checkEquivalent(t, p, b, "SELECT * FROM t ORDER BY id", true)
	checkEquivalent(t, p, b, "SELECT id FROM t WHERE a = 3", false)
}

// TestPlannerPaging exercises the ranged reader across multiple prefetch
// pages (scanBatch keys per List call).
func TestPlannerPaging(t *testing.T) {
	rows := make([]string, 0, 2500)
	for i := 1; i <= 2500; i++ {
		rows = append(rows, fmt.Sprintf("(%d, %d)", i, i%7))
	}
	p, b := plannerPair(t,
		"CREATE TABLE big (id INTEGER PRIMARY KEY, g INTEGER)",
		"CREATE INDEX idx_g ON big(g)",
		"INSERT INTO big VALUES "+strings.Join(rows, ", "),
	)
	// A PK range wider than one page.
	res := checkEquivalent(t, p, b, "SELECT id FROM big WHERE id > 100 AND id <= 2400 ORDER BY id", true)
	wantPath(t, p, "", false, 1)
	if len(res.Rows) != 2300 {
		t.Fatalf("expected 2300 rows, got %d", len(res.Rows))
	}
	// An index range wider than one page (g=0 matches ~357 rows; widen with
	// a range over g to cross the page boundary).
	checkEquivalent(t, p, b, "SELECT count(*) FROM big WHERE g >= 1 AND g <= 5", false)
	wantPath(t, p, "idx_g", false, 1)
}
