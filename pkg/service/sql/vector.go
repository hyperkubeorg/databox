// vector.go implements the pgvector-compatible vector extension of the
// dialect (§13 "Vectors"): the VECTOR(n) value kind
// (fixed-dimension float32), the strict '[1,2.5,-3]' text-literal parser
// and its canonical output form, the three distance operators
//
//	a <-> b   L2 (Euclidean) distance
//	a <#>  b   NEGATIVE inner product (pgvector returns the negation so
//	           that ORDER BY ... <#> ascending means "most similar first")
//	a <=> b   cosine distance (1 - cosine similarity)
//
// the scalar functions vector_dims / l2_distance / inner_product /
// cosine_distance / l2_norm, and the exact-KNN top-k fast path that turns
// "ORDER BY <distance> LIMIT k" into a single scan through a bounded heap
// instead of a full materialize-and-sort.
//
// Semantics decisions, pinned by tests:
//
//   - Cosine distance of a zero-magnitude vector is an ERROR (division by
//     zero has no meaningful similarity; erroring beats pgvector's raw-C
//     NaN, which silently poisons ORDER BY).
//   - Operands coerce like everywhere else in the dialect: a TEXT value in
//     vector position must parse as a strict vector literal; a dimension
//     mismatch between operands is an error, not NULL.
//   - NULL propagates: any NULL operand/argument yields NULL.
//   - Distance math accumulates in float64 (inputs are float32, results
//     are DOUBLE PRECISION), matching pgvector's use of double
//     accumulators.
//
// There are NO vector indexes in v1 — every KNN query is exact. The top-k
// heap changes cost, never results.
package sql

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// maxVectorDim caps declared and literal vector dimensions, the same limit
// pgvector enforces for the vector type.
const maxVectorDim = 16000

// ---------------------------------------------------------------------------
// Literal parsing and canonical formatting
// ---------------------------------------------------------------------------

// parseVectorText parses the pgvector text form "[1, 2.5, -3]" strictly:
// brackets required, elements comma-separated finite numbers, at least one
// element, nothing but whitespace around tokens. NaN and Infinity are
// rejected — they would break every distance metric.
func parseVectorText(s string) ([]float32, error) {
	t := strings.TrimSpace(s)
	if len(t) < 2 || t[0] != '[' || t[len(t)-1] != ']' {
		return nil, fmt.Errorf("malformed vector literal %q: must be like '[1,2,3]'", s)
	}
	body := t[1 : len(t)-1]
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("malformed vector literal %q: vector must have at least 1 dimension", s)
	}
	parts := strings.Split(body, ",")
	if len(parts) > maxVectorDim {
		return nil, fmt.Errorf("vector cannot have more than %d dimensions", maxVectorDim)
	}
	out := make([]float32, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("malformed vector literal %q: empty element", s)
		}
		// Parse at float32 precision — that is the storage width, so what
		// parses is exactly what is stored and later formatted back.
		f, err := strconv.ParseFloat(p, 32)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("malformed vector literal %q: %q is not a finite number", s, p)
		}
		out[i] = float32(f)
	}
	return out, nil
}

// formatVector renders the canonical pgvector output form: no spaces, each
// element the shortest decimal that round-trips its float32 ("[1,2.5,-3]").
func formatVector(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// asVector coerces an operand/argument into a vector: vectors pass through,
// text must be a strict vector literal (this is how '$1' text parameters
// and quoted literals reach vector positions). Anything else is an error —
// unlike arithmetic there is no silent-NULL rule here, because a typo'd
// operand would otherwise vanish instead of failing.
func asVector(v Value) ([]float32, error) {
	switch v.T {
	case TypeVector:
		return v.Vec, nil
	case TypeText:
		return parseVectorText(v.S)
	}
	return nil, fmt.Errorf("expected a vector, got %s", v.T)
}

// checkSameDims errors unless the two vectors share a dimension, with the
// mismatch spelled out for the user.
func checkSameDims(a, b []float32) error {
	if len(a) != len(b) {
		return fmt.Errorf("vector dimension mismatch: %d vs %d", len(a), len(b))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Distance math (float64 accumulation over float32 elements)
// ---------------------------------------------------------------------------

// vecL2Distance is sqrt(sum((a_i-b_i)^2)) — Euclidean distance.
func vecL2Distance(a, b []float32) float64 {
	sum := 0.0
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return math.Sqrt(sum)
}

// vecInnerProduct is sum(a_i*b_i) — the POSITIVE inner product (the <#>
// operator negates it, per pgvector).
func vecInnerProduct(a, b []float32) float64 {
	sum := 0.0
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

// vecL2Norm is sqrt(sum(a_i^2)) — the Euclidean magnitude.
func vecL2Norm(a []float32) float64 {
	sum := 0.0
	for _, f := range a {
		sum += float64(f) * float64(f)
	}
	return math.Sqrt(sum)
}

// vecCosineDistance is 1 - (a·b)/(|a||b|). A zero-magnitude operand has no
// direction, so cosine similarity is undefined: error (see the package
// comment for why this beats returning NaN). The denominator is computed
// as sqrt(|a|²·|b|²) — one square root of the product, exactly as pgvector
// does — so identical vectors divide by their exact squared norm and
// report a distance of exactly 0.
func vecCosineDistance(a, b []float32) (float64, error) {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0, fmt.Errorf("cosine distance is not defined for zero-magnitude vectors")
	}
	sim := dot / math.Sqrt(na*nb)
	// Clamp float rounding so the result stays within [0, 2] (similarity
	// within [-1, 1]), like pgvector.
	if sim > 1 {
		sim = 1
	} else if sim < -1 {
		sim = -1
	}
	return 1 - sim, nil
}

// ---------------------------------------------------------------------------
// Operator and function evaluation (called from eval.go)
// ---------------------------------------------------------------------------

// isDistanceOp reports whether op is one of the three vector distance
// operators. Shared by the evaluator, the parser tests and the top-k
// detector.
func isDistanceOp(op string) bool {
	return op == "<->" || op == "<#>" || op == "<=>"
}

// evalVectorDistance evaluates a distance operator over two coerced
// operands. NULL propagates; a non-vector operand or dimension mismatch is
// an error.
func evalVectorDistance(op string, l, r Value) (Value, error) {
	if l.IsNull() || r.IsNull() {
		return nullV(), nil
	}
	a, err := asVector(l)
	if err != nil {
		return Value{}, err
	}
	b, err := asVector(r)
	if err != nil {
		return Value{}, err
	}
	if err := checkSameDims(a, b); err != nil {
		return Value{}, err
	}
	switch op {
	case "<->":
		return doubleV(vecL2Distance(a, b)), nil
	case "<#>":
		// pgvector: <#> is the NEGATIVE inner product, so ascending ORDER BY
		// ranks the most similar vectors first.
		return doubleV(-vecInnerProduct(a, b)), nil
	case "<=>":
		d, err := vecCosineDistance(a, b)
		if err != nil {
			return Value{}, err
		}
		return doubleV(d), nil
	}
	return Value{}, fmt.Errorf("unknown vector operator %q", op)
}

// isVectorFuncName recognizes the five vector scalar functions.
func isVectorFuncName(name string) bool {
	switch name {
	case "vector_dims", "l2_distance", "inner_product", "cosine_distance", "l2_norm":
		return true
	}
	return false
}

// evalVectorFunc evaluates one vector scalar function over already-
// evaluated arguments. NULL arguments yield NULL; wrong arity, non-vector
// arguments and dimension mismatches are errors.
func evalVectorFunc(name string, args []Value) (Value, error) {
	// Unary: vector_dims(v) and l2_norm(v).
	if name == "vector_dims" || name == "l2_norm" {
		if len(args) != 1 {
			return Value{}, fmt.Errorf("%s expects 1 argument", name)
		}
		if args[0].IsNull() {
			return nullV(), nil
		}
		v, err := asVector(args[0])
		if err != nil {
			return Value{}, err
		}
		if name == "vector_dims" {
			return intV(int64(len(v))), nil
		}
		return doubleV(vecL2Norm(v)), nil
	}
	// Binary: the three distances. inner_product is POSITIVE here (only the
	// <#> operator negates), matching pgvector's function/operator split.
	if len(args) != 2 {
		return Value{}, fmt.Errorf("%s expects 2 arguments", name)
	}
	if args[0].IsNull() || args[1].IsNull() {
		return nullV(), nil
	}
	a, err := asVector(args[0])
	if err != nil {
		return Value{}, err
	}
	b, err := asVector(args[1])
	if err != nil {
		return Value{}, err
	}
	if err := checkSameDims(a, b); err != nil {
		return Value{}, err
	}
	switch name {
	case "l2_distance":
		return doubleV(vecL2Distance(a, b)), nil
	case "inner_product":
		return doubleV(vecInnerProduct(a, b)), nil
	case "cosine_distance":
		d, err := vecCosineDistance(a, b)
		if err != nil {
			return Value{}, err
		}
		return doubleV(d), nil
	}
	return Value{}, fmt.Errorf("unknown vector function %s", name)
}

// checkVectorDim enforces a column's declared dimension on write, with the
// user-facing wording the spec pins ("expected 3 dimensions, got 4").
func checkVectorDim(colName string, dim int, v Value) error {
	if v.T != TypeVector || v.IsNull() {
		return nil
	}
	if len(v.Vec) != dim {
		return fmt.Errorf("column %q: expected %d dimensions, got %d", colName, dim, len(v.Vec))
	}
	return nil
}

// errVectorIndex is the single error for every path that would put a
// vector into the order-preserving key encoding (PRIMARY KEY, UNIQUE,
// CREATE INDEX): vectors have no meaningful byte order, and approximate
// nearest-neighbor structures are explicitly future work.
func errVectorIndex() error {
	return fmt.Errorf("vector columns cannot be indexed (ANN indexes are future work)")
}

// ---------------------------------------------------------------------------
// Top-k fast path: ORDER BY <distance> LIMIT k as a bounded-heap scan
// ---------------------------------------------------------------------------

// topKMax bounds how large a LIMIT+OFFSET the heap path accepts; beyond it
// the ordinary sort is no worse and the heap's memory bound stops meaning
// anything.
const topKMax = 10000

// topKItem is one surviving candidate in the heap: the projected row, its
// distance, and its scan sequence number (the tiebreak that makes the heap
// reproduce the stable sort's tie order exactly).
type topKItem struct {
	rr   resultRow
	dist Value
	seq  int
}

// topKWorse orders candidates the way the naive path does: by distance
// under the total sortCompare order (NULL first, like ORDER BY), ties
// broken by scan order — precisely the key of a stable ascending sort.
// "worse" means a sorts after b.
func topKWorse(a, b topKItem) bool {
	c := sortCompare(a.dist, b.dist)
	if c != 0 {
		return c > 0
	}
	return a.seq > b.seq
}

// topKShape matches a query against the fast-path shape:
//
//	SELECT ... FROM t [WHERE ...] ORDER BY <a <-> b> [ASC] LIMIT k [OFFSET m]
//
// with no UNION, DISTINCT or grouping, and k+m within topKMax. It returns
// the heap capacity (limit+offset) and offset. Everything the shape
// excludes falls back to the ordinary sort path — same results, more work.
func (e *Engine) topKShape(s *Select) (n, offset int, ok bool) {
	if e.noTopK ||
		len(s.Unions) > 0 || s.Core.Distinct || s.Core.grouped() ||
		s.Core.Table == "" || len(s.OrderBy) != 1 || s.OrderBy[0].Desc ||
		s.Limit == nil {
		return 0, 0, false
	}
	bin, isBin := s.OrderBy[0].Expr.(*Binary)
	if !isBin || !isDistanceOp(bin.Op) {
		return 0, 0, false
	}
	off, limit, err := e.limitOffset(s)
	if err != nil || limit < 0 || limit+off > topKMax {
		return 0, 0, false // bad/huge LIMIT: let the normal path handle it
	}
	return limit + off, off, true
}

// execTopK runs the fast path: one planned scan, WHERE filter, projection,
// then a size-bounded max-heap keyed by (distance, scan order). The heap
// root is always the worst kept candidate, so a full table streams through
// O(n log k) with O(k) memory instead of a full sort. Results are
// byte-identical to the naive path (vector_test.go asserts equivalence on
// random data).
func (e *Engine) execTopK(ctx context.Context, s *Select, n, offset int) ([]string, [][]Value, error) {
	core := s.Core
	sc, err := e.loadSchema(ctx, core.Table)
	if err != nil {
		return nil, nil, err
	}
	stored, plan, err := e.scanFor(ctx, sc, core.Where)
	if err != nil {
		return nil, nil, err
	}
	// Record the fast path on the plan so tests/observability can pin it.
	if plan != nil {
		plan.TopK = true
	}
	if err := checkStarHasTable(core, sc); err != nil {
		return nil, nil, err
	}
	cols := e.projectionColumns(core, sc)
	orderExpr := s.OrderBy[0].Expr

	heap := make([]topKItem, 0, min(n, len(stored)))
	// pending holds rows until a second candidate appears: with 0 or 1
	// matching rows a sort never evaluates its keys, so the fast path must
	// not either (a bad distance expression stays un-evaluated, exactly as
	// in the naive path).
	var pending *topKItem
	seq := 0
	admit := func(it topKItem) {
		if n == 0 {
			return // LIMIT 0: nothing is ever kept
		}
		if len(heap) < n {
			heap = append(heap, it)
			topKSiftUp(heap)
			return
		}
		if topKWorse(heap[0], it) {
			heap[0] = it
			topKSiftDown(heap)
		}
	}
	for _, sr := range stored {
		okRow, err := predicateTrue(core.Where, sr.data)
		if err != nil {
			return nil, nil, err
		}
		if !okRow {
			continue
		}
		vals, err := e.projectRow(core, sc, sr.data)
		if err != nil {
			return nil, nil, err
		}
		it := topKItem{rr: resultRow{out: vals, env: mergeEnv(sr.data, cols, vals)}, seq: seq}
		seq++
		if pending == nil && seq == 1 {
			pending = &it
			continue
		}
		// Second row seen: distances start mattering. Flush the held first
		// row, then process normally.
		if pending != nil {
			pending.dist, err = evalExpr(orderExpr, pending.rr.env)
			if err != nil {
				return nil, nil, err
			}
			admit(*pending)
			pending = nil
		}
		it.dist, err = evalExpr(orderExpr, it.rr.env)
		if err != nil {
			return nil, nil, err
		}
		admit(it)
	}
	if pending != nil {
		// Exactly one matching row: order is vacuous, skip the distance
		// evaluation (naive-path parity) and apply OFFSET/LIMIT directly.
		if offset > 0 || n == 0 {
			return cols, nil, nil
		}
		return cols, [][]Value{pending.rr.out}, nil
	}
	// The heap holds the n best in heap order; sort them into result order
	// (ascending distance, ties by scan order) and drop the OFFSET prefix.
	sort.Slice(heap, func(i, j int) bool { return topKWorse(heap[j], heap[i]) })
	if offset >= len(heap) {
		return cols, nil, nil
	}
	out := make([][]Value, 0, len(heap)-offset)
	for _, it := range heap[offset:] {
		out = append(out, it.rr.out)
	}
	return cols, out, nil
}

// topKSiftUp restores the max-heap property after appending to the tail.
func topKSiftUp(h []topKItem) {
	i := len(h) - 1
	for i > 0 {
		parent := (i - 1) / 2
		if !topKWorse(h[i], h[parent]) {
			return
		}
		h[i], h[parent] = h[parent], h[i]
		i = parent
	}
}

// topKSiftDown restores the max-heap property after replacing the root.
func topKSiftDown(h []topKItem) {
	i := 0
	for {
		worst := i
		if l := 2*i + 1; l < len(h) && topKWorse(h[l], h[worst]) {
			worst = l
		}
		if r := 2*i + 2; r < len(h) && topKWorse(h[r], h[worst]) {
			worst = r
		}
		if worst == i {
			return
		}
		h[i], h[worst] = h[worst], h[i]
		i = worst
	}
}
