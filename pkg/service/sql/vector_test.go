// vector_test.go covers the pgvector-style extension (vector.go): lexing
// and precedence of the distance operators, VECTOR(n) DDL, strict literal
// parsing and storage round-trips, distance math against hand-computed
// values, the write-time dimension check, every index/grouping restriction,
// the top-k fast path's exact equivalence with the naive sort on random
// data, and the extended wire protocol with vector parameters.
package sql

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
)

// expectErr runs SQL and requires an error containing want.
func expectErr(t *testing.T, e *Engine, sql, want string) {
	t.Helper()
	_, err := e.Exec(context.Background(), sql)
	if err == nil {
		t.Fatalf("%q: expected error containing %q, got none", sql, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%q: error %q does not contain %q", sql, err, want)
	}
}

// ---------------------------------------------------------------------------
// Lexer / parser
// ---------------------------------------------------------------------------

func TestVectorOperatorLexing(t *testing.T) {
	// Each distance operator must lex as one token, distinct from its
	// prefixes ("<", "<=") and from "<>" (not-equals).
	for _, tc := range []struct{ src, op string }{
		{"SELECT a <-> b", "<->"},
		{"SELECT a <#> b", "<#>"},
		{"SELECT a <=> b", "<=>"},
	} {
		stmts, err := ParseStatements(tc.src)
		if err != nil {
			t.Fatalf("%q: %v", tc.src, err)
		}
		bin, ok := stmts[0].(*Select).Core.Items[0].Expr.(*Binary)
		if !ok || bin.Op != tc.op {
			t.Fatalf("%q: parsed %#v, want Binary %s", tc.src, stmts[0], tc.op)
		}
	}
	// The prefixes still mean what they always meant.
	stmts, err := ParseStatements("SELECT a <= b, a <> b, a < -1")
	if err != nil {
		t.Fatal(err)
	}
	items := stmts[0].(*Select).Core.Items
	if op := items[0].Expr.(*Binary).Op; op != "<=" {
		t.Fatalf("a <= b parsed as %q", op)
	}
	if op := items[1].Expr.(*Binary).Op; op != "!=" {
		t.Fatalf("a <> b parsed as %q", op)
	}
	if op := items[2].Expr.(*Binary).Op; op != "<" {
		t.Fatalf("a < -1 parsed as %q", op)
	}
}

func TestVectorOperatorPrecedence(t *testing.T) {
	// Distance binds tighter than comparison: (a <-> b) < 0.5.
	stmts, err := ParseStatements("SELECT a <-> b < 0.5")
	if err != nil {
		t.Fatal(err)
	}
	outer, ok := stmts[0].(*Select).Core.Items[0].Expr.(*Binary)
	if !ok || outer.Op != "<" {
		t.Fatalf("outer operator = %#v, want <", outer)
	}
	inner, ok := outer.L.(*Binary)
	if !ok || inner.Op != "<->" {
		t.Fatalf("left of < = %#v, want a <-> b", outer.L)
	}
	// And works as a WHERE predicate end to end.
	e := newEngine()
	rowsEqual(t, run(t, e, "SELECT '[0,0]' <-> '[3,4]' < 6, '[0,0]' <-> '[3,4]' < 4"),
		[][]string{{"t", "f"}})
}

func TestVectorDDLParsing(t *testing.T) {
	e := newEngine()
	run(t, e, "CREATE TABLE ok (id INTEGER PRIMARY KEY, emb VECTOR(3))")
	// Bare VECTOR demands a dimension; out-of-range and non-integer
	// dimensions are rejected.
	expectErr(t, e, "CREATE TABLE bad (v VECTOR)", "VECTOR requires a dimension")
	expectErr(t, e, "CREATE TABLE bad (v VECTOR(0))", "vector dimension must be between 1 and 16000")
	expectErr(t, e, "CREATE TABLE bad (v VECTOR(16001))", "vector dimension must be between 1 and 16000")
	expectErr(t, e, "CREATE TABLE bad (v VECTOR(2.5))", "expected an integer vector dimension")
	// The upper bound itself is accepted.
	run(t, e, "CREATE TABLE big (id INTEGER PRIMARY KEY, v VECTOR(16000))")
}

// ---------------------------------------------------------------------------
// Literal parsing, formatting, storage round-trip
// ---------------------------------------------------------------------------

func TestParseVectorTextStrict(t *testing.T) {
	good := map[string][]float32{
		"[1, 2.5, -3]":   {1, 2.5, -3},
		" [1,2,3] ":      {1, 2, 3},
		"[0.001]":        {0.001},
		"[1e2, -1.5E-1]": {100, -0.15},
	}
	for src, want := range good {
		got, err := parseVectorText(src)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%q: %v, want %v", src, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%q: %v, want %v", src, got, want)
			}
		}
	}
	bad := []string{
		"1,2,3", "[1,2,3", "1,2,3]", "[]", "[ ]", "[1,,2]", "[1,2,]",
		"[a,b]", "[NaN]", "[Inf]", "[-Inf]", "[1;2]", "[1 2]", "",
	}
	for _, src := range bad {
		if _, err := parseVectorText(src); err == nil {
			t.Fatalf("%q: expected parse error", src)
		}
	}
}

func TestVectorFormatCanonical(t *testing.T) {
	// Canonical output: no spaces, shortest float32 round-trip form.
	if got := formatVector([]float32{1, 2.5, -3}); got != "[1,2.5,-3]" {
		t.Fatalf("formatVector = %q", got)
	}
	// FormatText → parseVectorText is the identity on the payload.
	in := []float32{0.1, -1e-7, 3.14159265, 16000}
	back, err := parseVectorText(formatVector(in))
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		if back[i] != in[i] {
			t.Fatalf("round trip lost element %d: %v -> %v", i, in[i], back[i])
		}
	}
}

func TestVectorStorageRoundTrip(t *testing.T) {
	// Row encode/decode preserves the vector bit-exactly.
	r := row{"v": vectorV([]float32{1, 2.5, -3.75e-5})}
	enc, err := encodeRow(r)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := decodeRow(enc)
	if err != nil {
		t.Fatal(err)
	}
	got := dec["v"]
	if got.T != TypeVector || len(got.Vec) != 3 {
		t.Fatalf("decoded %#v", got)
	}
	for i, f := range []float32{1, 2.5, -3.75e-5} {
		if got.Vec[i] != f {
			t.Fatalf("element %d: %v, want %v", i, got.Vec[i], f)
		}
	}
	// And through the engine: INSERT literal text, SELECT canonical text.
	e := newEngine()
	run(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, emb VECTOR(3))")
	run(t, e, "INSERT INTO t VALUES (1, '[1, 2.5, -3]')")
	rowsEqual(t, run(t, e, "SELECT emb, typeof(emb) FROM t"), [][]string{{"[1,2.5,-3]", "vector"}})
	// A stored vector copies losslessly through INSERT ... SELECT.
	run(t, e, "CREATE TABLE t2 (id INTEGER PRIMARY KEY, emb VECTOR(3))")
	run(t, e, "INSERT INTO t2 SELECT id, emb FROM t")
	rowsEqual(t, run(t, e, "SELECT emb FROM t2 WHERE id = 1"), [][]string{{"[1,2.5,-3]"}})
}

func TestVectorDimensionCheckOnWrite(t *testing.T) {
	e := newEngine()
	run(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, emb VECTOR(3))")
	expectErr(t, e, "INSERT INTO t VALUES (1, '[1,2,3,4]')", `column "emb": expected 3 dimensions, got 4`)
	expectErr(t, e, "INSERT INTO t VALUES (1, '[1]')", `column "emb": expected 3 dimensions, got 1`)
	run(t, e, "INSERT INTO t VALUES (1, '[1,2,3]')")
	expectErr(t, e, "UPDATE t SET emb = '[1,2]' WHERE id = 1", `column "emb": expected 3 dimensions, got 2`)
	// NULL is allowed regardless of dimension (no NOT NULL here).
	run(t, e, "INSERT INTO t VALUES (2, NULL)")
	// Malformed literal fails as a cast, not silently.
	expectErr(t, e, "INSERT INTO t VALUES (3, 'nope')", "malformed vector literal")
	// The ::VECTOR(n) cast enforces its declared dimension too.
	expectErr(t, e, "SELECT '[1,2]'::VECTOR(3)", "expected 3 dimensions, got 2")
	rowsEqual(t, run(t, e, "SELECT '[1,2]'::VECTOR(2)"), [][]string{{"[1,2]"}})
}

// ---------------------------------------------------------------------------
// Distance math
// ---------------------------------------------------------------------------

func TestVectorDistanceMath(t *testing.T) {
	e := newEngine()
	// One row so column operands are exercised alongside literals.
	run(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, a VECTOR(3), b VECTOR(3))")
	run(t, e, "INSERT INTO t VALUES (1, '[1,2,3]', '[4,5,6]')")

	// Hand-computed: a=[1,2,3], b=[4,5,6]
	//   L2        = sqrt(3^2*3)        = sqrt(27)
	//   dot       = 4+10+18            = 32   (operator <#> returns -32)
	//   |a|,|b|   = sqrt(14), sqrt(77)
	//   cosine    = 1 - 32/sqrt(14*77) = 1 - 32/sqrt(1078)
	l2 := formatDouble(math.Sqrt(27))
	cos := formatDouble(1 - 32/math.Sqrt(1078))
	rowsEqual(t, run(t, e, "SELECT a <-> b, a <#> b, a <=> b FROM t"),
		[][]string{{l2, "-32", cos}})
	// Functions: positive inner_product, same distances, norm and dims.
	rowsEqual(t, run(t, e,
		"SELECT l2_distance(a, b), inner_product(a, b), cosine_distance(a, b), l2_norm(a), vector_dims(a) FROM t"),
		[][]string{{l2, "32", cos, formatDouble(math.Sqrt(14)), "3"}})
	// Text literals coerce on both sides; identical vectors are distance 0.
	rowsEqual(t, run(t, e, "SELECT '[3,4]' <-> '[0,0]', '[1,2]' <=> '[1,2]', '[1,2]' <#> '[1,2]'"),
		[][]string{{"5", "0", "-5"}})
	// NULL propagates through operators and functions.
	rowsEqual(t, run(t, e, "SELECT NULL <-> '[1,2]', l2_norm(NULL), vector_dims(NULL)"),
		[][]string{{"NULL", "NULL", "NULL"}})

	// Cosine distance of a zero-magnitude vector is an error (documented
	// decision, see vector.go).
	expectErr(t, e, "SELECT '[0,0]' <=> '[1,2]'", "cosine distance is not defined for zero-magnitude vectors")
	expectErr(t, e, "SELECT cosine_distance('[1,2]', '[0,0]')", "cosine distance is not defined for zero-magnitude vectors")
	// Zero vectors are fine for the other metrics.
	rowsEqual(t, run(t, e, "SELECT '[0,0]' <-> '[3,4]', '[0,0]' <#> '[3,4]'"), [][]string{{"5", "-0"}})

	// Dimension mismatch between operands is an eval error.
	expectErr(t, e, "SELECT '[1,2]' <-> '[1,2,3]'", "vector dimension mismatch: 2 vs 3")
	expectErr(t, e, "SELECT l2_distance('[1]', '[1,2]')", "vector dimension mismatch: 1 vs 2")
	// Non-vector operands are errors, not silent NULLs.
	expectErr(t, e, "SELECT 1 <-> '[1,2]'", "expected a vector, got integer")
	expectErr(t, e, "SELECT vector_dims(42)", "expected a vector, got integer")
}

// ---------------------------------------------------------------------------
// Restrictions: no keys, no indexes, no grouping/dedup
// ---------------------------------------------------------------------------

func TestVectorRestrictions(t *testing.T) {
	const idxErr = "vector columns cannot be indexed (ANN indexes are future work)"
	e := newEngine()
	// PRIMARY KEY — inline and table-level.
	expectErr(t, e, "CREATE TABLE b1 (v VECTOR(3) PRIMARY KEY)", idxErr)
	expectErr(t, e, "CREATE TABLE b2 (v VECTOR(3), PRIMARY KEY (v))", idxErr)
	// UNIQUE — inline and table-level (unique constraints are indexes).
	expectErr(t, e, "CREATE TABLE b3 (v VECTOR(3) UNIQUE)", idxErr)
	expectErr(t, e, "CREATE TABLE b4 (v VECTOR(3), UNIQUE (v))", idxErr)
	// CREATE INDEX, plain and composite.
	run(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER, v VECTOR(2))")
	expectErr(t, e, "CREATE INDEX ON t (v)", idxErr)
	expectErr(t, e, "CREATE UNIQUE INDEX ON t (a, v)", idxErr)

	run(t, e, "INSERT INTO t VALUES (1, 1, '[1,2]'), (2, 1, '[1,2]')")
	// GROUP BY / DISTINCT / UNION dedup all reject vector values.
	expectErr(t, e, "SELECT count(*) FROM t GROUP BY v", "vector values cannot be used in GROUP BY")
	expectErr(t, e, "SELECT DISTINCT v FROM t", "vector values cannot be used in DISTINCT or UNION")
	expectErr(t, e, "SELECT v FROM t UNION SELECT v FROM t", "vector values cannot be used in DISTINCT or UNION")
	// UNION ALL never compares rows, so vectors pass through it.
	if got := run(t, e, "SELECT v FROM t UNION ALL SELECT v FROM t"); len(got.Rows) != 4 {
		t.Fatalf("UNION ALL rows = %d, want 4", len(got.Rows))
	}
	// Equality/ordering comparisons on vectors are undefined.
	expectErr(t, e, "SELECT v = v FROM t", "cannot compare vector with vector")
	expectErr(t, e, "SELECT * FROM t WHERE v = '[1,2]'", "cannot compare vector with text")

	// The planner never builds a key range from a vector predicate: the
	// statement fails on the residual filter's comparison error, and the
	// chosen plan is the full scan (nothing pushed into keyenc).
	if e.lastPlan == nil {
		t.Fatal("no plan recorded for vector-compare WHERE")
	}
	if !e.lastPlan.isFullScan() {
		t.Fatalf("plan for vector-compare WHERE = %v, want full scan", e.lastPlan.describe())
	}
}

// ---------------------------------------------------------------------------
// Top-k fast path: exact equivalence with the naive sort
// ---------------------------------------------------------------------------

// topKEngines returns two engines over one shared store: one with the
// top-k heap active, one forced through the ordinary sort path.
func topKEngines(t *testing.T) (fast, naive *Engine) {
	t.Helper()
	st := NewMemStore()
	fast = NewEngineWithStore(st, "test")
	naive = NewEngineWithStore(st, "test")
	naive.noTopK = true
	return fast, naive
}

func TestVectorTopKMatchesNaive(t *testing.T) {
	fast, naive := topKEngines(t)
	run(t, fast, "CREATE TABLE v (id INTEGER PRIMARY KEY, cat INTEGER, emb VECTOR(4))")

	// A few hundred random vectors, seeded for reproducibility. Elements
	// stay in (0.1, 1.1) so no zero vectors trip cosine; every 10th row
	// repeats the previous embedding so ties exercise the heap's
	// stable-order tiebreak.
	rng := rand.New(rand.NewSource(42))
	vec := func() string {
		return fmt.Sprintf("[%g,%g,%g,%g]",
			rng.Float64()+0.1, rng.Float64()+0.1, rng.Float64()+0.1, rng.Float64()+0.1)
	}
	last := vec()
	var ins strings.Builder
	ins.WriteString("INSERT INTO v VALUES ")
	for i := 0; i < 300; i++ {
		if i%10 != 9 {
			last = vec()
		}
		if i > 0 {
			ins.WriteString(",")
		}
		fmt.Fprintf(&ins, "(%d, %d, '%s')", i, i%5, last)
	}
	run(t, fast, ins.String())

	query := vec()
	queries := []string{
		fmt.Sprintf("SELECT id FROM v ORDER BY emb <-> '%s' LIMIT 10", query),
		fmt.Sprintf("SELECT id FROM v ORDER BY emb <-> '%s' ASC LIMIT 25 OFFSET 5", query),
		fmt.Sprintf("SELECT id, emb <#> '%s' FROM v ORDER BY emb <#> '%s' LIMIT 7", query, query),
		fmt.Sprintf("SELECT id FROM v ORDER BY emb <=> '%s' LIMIT 400", query), // k > table size
		fmt.Sprintf("SELECT id FROM v WHERE cat = 3 ORDER BY emb <-> '%s' LIMIT 10", query),
		fmt.Sprintf("SELECT id FROM v ORDER BY emb <-> '%s' LIMIT 0", query),
		fmt.Sprintf("SELECT id FROM v WHERE id = 42 ORDER BY emb <-> '%s' LIMIT 3", query), // 1 row
		fmt.Sprintf("SELECT id FROM v ORDER BY emb <-> '%s' LIMIT 3 OFFSET 299", query),
		fmt.Sprintf("SELECT id FROM v ORDER BY emb <-> '%s' LIMIT 3 OFFSET 400", query), // offset past end
	}
	for _, q := range queries {
		fres, ferr := fast.Exec(context.Background(), q)
		nres, nerr := naive.Exec(context.Background(), q)
		if ferr != nil || nerr != nil {
			t.Fatalf("%q: fast err=%v naive err=%v", q, ferr, nerr)
		}
		ft, nt := textRows(fres[0]), textRows(nres[0])
		if fmt.Sprint(ft) != fmt.Sprint(nt) {
			t.Fatalf("%q: fast path diverged\nfast:  %v\nnaive: %v", q, ft, nt)
		}
		// The fast engine must actually have taken the heap path...
		if fast.lastPlan == nil || !fast.lastPlan.TopK {
			t.Fatalf("%q: expected TopK plan, got %v", q, fast.lastPlan)
		}
		// ...and the baseline must not have.
		if naive.lastPlan != nil && naive.lastPlan.TopK {
			t.Fatalf("%q: baseline unexpectedly used TopK", q)
		}
	}

	// Shapes the fast path must decline (DESC, huge k, grouping, no LIMIT):
	// still correct, no TopK mark.
	for _, q := range []string{
		fmt.Sprintf("SELECT id FROM v ORDER BY emb <-> '%s' DESC LIMIT 5", query),
		fmt.Sprintf("SELECT id FROM v ORDER BY emb <-> '%s' LIMIT 20000", query),
		fmt.Sprintf("SELECT id FROM v ORDER BY emb <-> '%s'", query),
		fmt.Sprintf("SELECT cat, count(*) FROM v GROUP BY cat ORDER BY cat LIMIT 3"),
	} {
		fres, ferr := fast.Exec(context.Background(), q)
		nres, nerr := naive.Exec(context.Background(), q)
		if ferr != nil || nerr != nil {
			t.Fatalf("%q: fast err=%v naive err=%v", q, ferr, nerr)
		}
		if fmt.Sprint(textRows(fres[0])) != fmt.Sprint(textRows(nres[0])) {
			t.Fatalf("%q: results diverged", q)
		}
		if fast.lastPlan != nil && fast.lastPlan.TopK {
			t.Fatalf("%q: TopK must not apply to this shape", q)
		}
	}

	// Error parity: a dimension mismatch in the ORDER BY errors the same
	// way on both paths.
	q := "SELECT id FROM v ORDER BY emb <-> '[1,2]' LIMIT 5"
	_, ferr := fast.Exec(context.Background(), q)
	_, nerr := naive.Exec(context.Background(), q)
	if ferr == nil || nerr == nil || ferr.Error() != nerr.Error() {
		t.Fatalf("error parity: fast=%v naive=%v", ferr, nerr)
	}
}

// ---------------------------------------------------------------------------
// Extended wire protocol: $N vector parameters, text OIDs
// ---------------------------------------------------------------------------

func TestExtendedVectorParams(t *testing.T) {
	f := startConn(t)
	f.prepareBindExecute("CREATE TABLE items (id INTEGER PRIMARY KEY, embedding VECTOR(3))", nil)

	// Text-format $N parameters carrying '[...]' literals INSERT cleanly.
	for i, v := range []string{"[0,0,0]", "[1,1,1]", "[10,10,10]"} {
		_, tag := f.prepareBindExecute("INSERT INTO items VALUES ($1, $2)",
			[]bindParam{textParam(fmt.Sprint(i + 1)), textParam(v)})
		if tag != "INSERT 0 1" {
			t.Fatalf("insert %d tag = %q", i, tag)
		}
	}
	// A bad vector parameter surfaces as a clean error.
	f.send('P', buildParse("", "INSERT INTO items VALUES ($1, $2)", nil))
	f.send('B', buildBind("", "", []bindParam{textParam("9"), textParam("[1,2]")}, nil))
	f.send('E', buildExecute(""))
	f.send('S', nil)
	f.expect('1')
	f.expect('2')
	if typ, body := f.recv(); typ != 'E' || !strings.Contains(errText(body), "expected 3 dimensions, got 2") {
		t.Fatalf("expected dimension error, got %c %s", typ, body)
	}
	f.expect('Z')

	// KNN through a $1 vector parameter: nearest two to [2,2,2] are
	// [1,1,1] then [0,0,0].
	rows, tag := f.prepareBindExecute(
		"SELECT id FROM items ORDER BY embedding <-> $1 LIMIT 2",
		[]bindParam{textParam("[2,2,2]")})
	if tag != "SELECT 2" || len(rows) != 2 {
		t.Fatalf("knn tag=%q rows=%d", tag, len(rows))
	}
	if id0 := string(dataRowFields(t, rows[0])[0]); id0 != "2" {
		t.Fatalf("nearest = %s, want 2", id0)
	}
	if id1 := string(dataRowFields(t, rows[1])[0]); id1 != "1" {
		t.Fatalf("second nearest = %s, want 1", id1)
	}

	// Describe: a vector column advertises OID 25 (text) and a distance
	// expression advertises float8.
	f.send('P', buildParse("d", "SELECT embedding, embedding <-> $1 AS dist FROM items", nil))
	f.send('D', buildDescribe('S', "d"))
	f.send('S', nil)
	f.expect('1')
	f.expect('t') // ParameterDescription
	rd := f.expect('T')
	r := &msgReader{b: rd}
	if n := r.int16(); n != 2 {
		t.Fatalf("column count = %d", n)
	}
	readCol := func() (string, int32) {
		name := r.cstr()
		r.int32()
		r.int16()
		oid := r.int32()
		r.int16()
		r.int32()
		r.int16()
		return name, oid
	}
	if name, oid := readCol(); name != "embedding" || oid != oidText {
		t.Fatalf("embedding described as %q oid %d, want text(25)", name, oid)
	}
	if name, oid := readCol(); name != "dist" || oid != oidFloat8 {
		t.Fatalf("dist described as %q oid %d, want float8", name, oid)
	}
	f.expect('Z')

	// And the value comes back in canonical text form.
	rows, _ = f.prepareBindExecute("SELECT embedding FROM items WHERE id = $1",
		[]bindParam{textParam("3")})
	if got := string(dataRowFields(t, rows[0])[0]); got != "[10,10,10]" {
		t.Fatalf("embedding text = %q", got)
	}
}
