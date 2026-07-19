// conformance_test.go runs the chai sqltests corpus in testdata/sqltests
// against the engine over the in-memory store. Two file formats exist,
// both cloned from chai's own harnesses (REFERENCES/chai):
//
//   - sqltests/sql_test.go format for every directory except expr/:
//     "-- setup:" statements, optional "-- suite: name" post-setup blocks
//     (every test runs once per suite on a fresh store), "-- test: name"
//     statements followed by either a "/* result: ... */" block of rows in
//     chai's document syntax or an "-- error: message" line asserting the
//     exact error text.
//
//   - testutil/genexprtests format for expr/: "> expr" followed by the
//     expected expression on the next line, or "! expr" followed by a
//     quoted error fragment. Expressions evaluate with now() pinned to
//     2020-01-01T00:00:00Z, exactly like chai's expression harness.
//
// Assertions are never weakened to pass: result rows compare typed (an
// INTEGER 1 does not match a DOUBLE 1.0, mirroring chai's marshaled-text
// comparison) and "-- error:" requires the exact message. Cases the v1
// dialect deliberately does not cover are recorded in the skip ledger
// below with a reason, and reported in the final tally.
package sql

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Skip ledger
// ---------------------------------------------------------------------------

// skipDirs skips whole suites (top-level corpus directories).
var skipDirs = map[string]string{
	"CREATE_SEQUENCE": "sequences are not implemented in v1",
	"DROP_SEQUENCE":   "sequences are not implemented in v1",
	"SEQUENCES":       "sequences are not implemented in v1",
	"ALTER_TABLE":     "ALTER TABLE is not in the v1 dialect",
	"planning":        "EXPLAIN output is chai-internal; access paths are asserted by planner_test.go",
}

// skipFiles skips single corpus files.
var skipFiles = map[string]string{}

// skipCases skips individual tests, keyed "<file>::<test name>" (optionally
// "<file>::<suite>::<test name>" when only one suite variant must skip).
var skipCases = map[string]string{}

func skipReason(file, suite, test string) (string, bool) {
	if r, ok := skipCases[file+"::"+suite+"::"+test]; ok {
		return r, true
	}
	if r, ok := skipCases[file+"::"+test]; ok {
		return r, true
	}
	return "", false
}

// contentSkipReason skips a case by what its statements use, for features
// that cut across many files. Each rule is a deliberate ledger entry, not
// a way to hide failures: the feature named is genuinely unimplemented.
func contentSkipReason(sqlText string) (string, bool) {
	up := strings.ToUpper(sqlText)
	switch {
	case strings.Contains(sqlText, "__chai_catalog"):
		return "catalog introspection (__chai_catalog with chai-normalized SQL text) not implemented", true
	case strings.Contains(up, "EXPLAIN "):
		return "EXPLAIN output is chai-internal plan text; access paths asserted by planner_test.go", true
	case strings.Contains(up, "NEXTVAL("), strings.Contains(up, "NEXT VALUE FOR"),
		strings.Contains(up, "CREATE SEQUENCE"), strings.Contains(up, "DROP SEQUENCE"):
		return "sequences are not implemented in v1", true
	case strings.Contains(up, "REINDEX"):
		return "REINDEX is not in the v1 dialect", true
	}
	return "", false
}

// confStats tallies executed corpus cases for the final report.
type confStats struct{ pass, fail, skip int }

func (s *confStats) report(t *testing.T, what string) {
	t.Logf("%s corpus: %d passed, %d failed, %d skipped, %d total",
		what, s.pass, s.fail, s.skip, s.pass+s.fail+s.skip)
}

// ---------------------------------------------------------------------------
// Suite-format corpus (everything but expr/)
// ---------------------------------------------------------------------------

func TestConformance(t *testing.T) {
	root := filepath.Join("testdata", "sqltests")
	stats := &confStats{}
	defer stats.report(t, "sqltests")

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if rel == "expr" {
				return fs.SkipDir // different format, run by TestConformanceExpr
			}
			if reason, ok := skipDirs[rel]; ok {
				n := countTests(t, path)
				stats.skip += n
				t.Logf("skip %s (%d cases): %s", rel, n, reason)
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".sql" {
			return nil
		}
		if reason, ok := skipFiles[filepath.ToSlash(rel)]; ok {
			n := countTestsInFile(t, path)
			stats.skip += n
			t.Logf("skip %s (%d cases): %s", rel, n, reason)
			return nil
		}
		runSuiteFile(t, path, filepath.ToSlash(rel), stats)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// countTests counts "-- test:" markers under a directory (times suites),
// so skipped directories still show up honestly in the tally.
func countTests(t *testing.T, dir string) int {
	total := 0
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".sql" {
			total += countTestsInFile(t, path)
		}
		return nil
	})
	return total
}

func countTestsInFile(t *testing.T, path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tests, suites := 0, 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "-- test:") {
			tests++
		}
		if strings.HasPrefix(line, "-- suite:") {
			suites++
		}
	}
	if suites == 0 {
		suites = 1
	}
	return tests * suites
}

// suiteTest is one "-- test:" case: statements plus either an expected
// result block or an expected error.
type suiteTest struct {
	name    string
	expr    string
	result  string
	errText string // exact expected message; "" with fails=true means "any error"
	fails   bool
	line    int
}

type suiteBlock struct {
	name      string
	postSetup string
	tests     []*suiteTest
}

type suiteFile struct {
	setup  string
	suites []suiteBlock
}

// parseSuiteFile is a line-for-line port of chai's sqltests parser
// (REFERENCES/chai/sqltests/sql_test.go) so the corpus is interpreted
// identically: tests belong to every suite, "--" lines inside tests are
// comments, result blocks keep their text verbatim.
func parseSuiteFile(t *testing.T, path string) *suiteFile {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sf := &suiteFile{}
	var cur *suiteTest
	var readingResult, readingSetup, readingSuite, readingComment bool
	suiteIndex := -1

	for lineNo, rawLine := range strings.Split(string(data), "\n") {
		line := rawLine
		if !readingResult {
			line = strings.TrimSpace(line)
		}
		switch {
		case strings.TrimSpace(line) == "":
		case readingComment && strings.TrimSpace(line) == "*/":
			readingComment = false
		case readingComment:
		case strings.HasPrefix(line, "-- setup:"):
			readingSetup = true
		case strings.HasPrefix(line, "-- suite:"):
			readingSuite = true
			suiteIndex++
			sf.suites = append(sf.suites, suiteBlock{name: strings.TrimPrefix(line, "-- suite: ")})
		case strings.HasPrefix(line, "-- test:"):
			readingSetup, readingSuite = false, false
			cur = &suiteTest{name: strings.TrimPrefix(line, "-- test: "), line: lineNo + 1}
			if suiteIndex == -1 {
				suiteIndex++
				sf.suites = append(sf.suites, suiteBlock{name: "default"})
			}
			for i := range sf.suites {
				sf.suites[i].tests = append(sf.suites[i].tests, cur)
			}
		case strings.HasPrefix(line, "/* result:"), strings.HasPrefix(line, "/*result:"):
			readingResult = true
		case strings.HasPrefix(line, "-- error:"):
			if cur == nil {
				t.Fatalf("%s:%d: -- error: without a test", path, lineNo+1)
			}
			cur.fails = true
			cur.errText = strings.TrimSpace(strings.TrimPrefix(line, "-- error:"))
			cur = nil
		case strings.HasPrefix(line, "/*"):
			readingComment = true
		case strings.HasPrefix(line, "--"):
		default:
			trimmed := strings.TrimSpace(line)
			switch {
			case readingSuite:
				sf.suites[suiteIndex].postSetup += line + "\n"
			case readingSetup:
				sf.setup += line + "\n"
			case readingResult && trimmed == "*/":
				readingResult = false
				cur = nil
			case readingResult:
				cur.result += line + "\n"
			case cur != nil:
				cur.expr += line + "\n"
			}
		}
	}
	return sf
}

// runSuiteFile executes every (suite × test) combination on fresh stores.
func runSuiteFile(t *testing.T, path, rel string, stats *confStats) {
	sf := parseSuiteFile(t, path)
	t.Run(rel, func(t *testing.T) {
		for _, suite := range sf.suites {
			t.Run(suite.name, func(t *testing.T) {
				for _, tc := range suite.tests {
					if reason, ok := skipReason(rel, suite.name, tc.name); ok {
						stats.skip++
						t.Logf("skip %s::%s: %s", rel, tc.name, reason)
						continue
					}
					if reason, ok := contentSkipReason(tc.expr); ok {
						stats.skip++
						t.Logf("skip %s::%s: %s", rel, tc.name, reason)
						continue
					}
					ok := t.Run(tc.name, func(t *testing.T) {
						runSuiteCase(t, path, sf.setup, suite.postSetup, tc)
					})
					if ok {
						stats.pass++
					} else {
						stats.fail++
					}
				}
			})
		}
	})
}

// runSuiteCase runs one case: setup, post-setup, then the test statements,
// asserting either the expected rows or the expected error.
func runSuiteCase(t *testing.T, path, setup, postSetup string, tc *suiteTest) {
	ctx := context.Background()
	e := NewEngineWithStore(NewMemStore(), "test")
	if setup != "" {
		if _, err := e.Exec(ctx, setup); err != nil {
			t.Fatalf("%s:%d setup: %v", path, tc.line, err)
		}
	}
	if postSetup != "" {
		if _, err := e.Exec(ctx, postSetup); err != nil {
			t.Fatalf("%s:%d post-setup: %v", path, tc.line, err)
		}
	}
	results, err := e.Exec(ctx, tc.expr)
	if tc.fails {
		if err == nil {
			t.Fatalf("%s:%d expected error, got none\n%s", path, tc.line, tc.expr)
		}
		if tc.errText != "" && err.Error() != tc.errText {
			t.Fatalf("%s:%d error mismatch\n got: %s\nwant: %s", path, tc.line, err.Error(), tc.errText)
		}
		return
	}
	if err != nil {
		t.Fatalf("%s:%d unexpected error: %v\n%s", path, tc.line, err, tc.expr)
	}
	// chai's query.Run returns the LAST statement's result.
	last := results[len(results)-1]
	want, err := parseResultDocs(tc.result)
	if err != nil {
		t.Fatalf("%s:%d bad expected result: %v\n%s", path, tc.line, err, tc.result)
	}
	compareResult(t, path, tc.line, want, last)
}

// resultDoc is one expected row: column names in order plus their values.
type resultDoc struct {
	cols []string
	vals []Value
}

// parseResultDocs parses a stream of chai result documents:
//
//	{ id: 1, "a": 2.5, s: 'x', n: null, }
//
// Keys are bare identifiers or quoted strings; values are any constant SQL
// expression, handed to the engine's own parser so literals mean exactly
// what they mean in queries.
func parseResultDocs(src string) ([]resultDoc, error) {
	p := &docParser{s: src}
	var docs []resultDoc
	for {
		p.ws()
		if p.eof() {
			return docs, nil
		}
		doc, err := p.doc()
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
}

type docParser struct {
	s string
	i int
}

func (p *docParser) eof() bool { return p.i >= len(p.s) }

func (p *docParser) ws() {
	for !p.eof() {
		c := p.s[p.i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.i++
			continue
		}
		return
	}
}

func (p *docParser) doc() (resultDoc, error) {
	var doc resultDoc
	if p.eof() || p.s[p.i] != '{' {
		return doc, fmt.Errorf("expected '{' at offset %d", p.i)
	}
	p.i++
	for {
		p.ws()
		if p.eof() {
			return doc, fmt.Errorf("unterminated document")
		}
		if p.s[p.i] == '}' {
			p.i++
			return doc, nil
		}
		key, err := p.key()
		if err != nil {
			return doc, err
		}
		p.ws()
		if p.eof() || p.s[p.i] != ':' {
			return doc, fmt.Errorf("expected ':' after key %q", key)
		}
		p.i++
		text, err := p.valueText()
		if err != nil {
			return doc, err
		}
		val, err := evalDocExpr(text)
		if err != nil {
			return doc, fmt.Errorf("value for %q: %v", key, err)
		}
		doc.cols = append(doc.cols, key)
		doc.vals = append(doc.vals, val)
	}
}

// key reads a bare, single-quoted, double-quoted or backquoted column name.
// Backslash escapes inside quotes are honored ({"'\"A\"'": ...}).
func (p *docParser) key() (string, error) {
	c := p.s[p.i]
	if c == '"' || c == '\'' || c == '`' {
		q := c
		p.i++
		var b strings.Builder
		for !p.eof() && p.s[p.i] != q {
			if p.s[p.i] == '\\' && p.i+1 < len(p.s) {
				p.i++
			}
			b.WriteByte(p.s[p.i])
			p.i++
		}
		if p.eof() {
			return "", fmt.Errorf("unterminated quoted key")
		}
		p.i++
		return b.String(), nil
	}
	start := p.i
	for !p.eof() {
		c := p.s[p.i]
		if c == ':' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			break
		}
		p.i++
	}
	if start == p.i {
		return "", fmt.Errorf("empty key at offset %d", start)
	}
	return p.s[start:p.i], nil
}

// valueText captures the raw expression text up to the next ',' or '}' at
// nesting depth zero, respecting quotes and parentheses.
func (p *docParser) valueText() (string, error) {
	start := p.i
	depth := 0
	for !p.eof() {
		c := p.s[p.i]
		switch c {
		case '\'', '"':
			q := c
			p.i++
			for !p.eof() && p.s[p.i] != q {
				p.i++
			}
			if p.eof() {
				return "", fmt.Errorf("unterminated string in value")
			}
		case '(', '[', '{':
			depth++
		case ')', ']':
			depth--
		case '}':
			if depth == 0 {
				return strings.TrimSpace(p.s[start:p.i]), nil
			}
			depth--
		case ',':
			if depth == 0 {
				txt := strings.TrimSpace(p.s[start:p.i])
				p.i++
				return txt, nil
			}
		}
		p.i++
	}
	return "", fmt.Errorf("unterminated value")
}

// evalDocExpr parses and evaluates a constant expression from a result doc
// through the engine's own front end.
func evalDocExpr(text string) (Value, error) {
	stmts, err := ParseStatements("SELECT " + text)
	if err != nil {
		return Value{}, err
	}
	sel, ok := stmts[0].(*Select)
	if !ok || len(sel.Core.Items) != 1 || sel.Core.Items[0].Star {
		return Value{}, fmt.Errorf("not a single expression: %q", text)
	}
	// nil row: a column reference is "no table specified", as in chai's
	// expression harness.
	return evalExpr(sel.Core.Items[0].Expr, nil)
}

// compareResult checks columns (names, order) and typed values row by row.
// The type must match exactly — chai compares marshaled text, where 1 and
// 1.0 differ — except that expected text compares against timestamp/bytea
// results through the dialect's own deterministic conversion, mirroring
// how chai result docs spell timestamps as strings.
func compareResult(t *testing.T, path string, line int, want []resultDoc, got ExecResult) {
	t.Helper()
	if len(got.typed) != len(want) {
		t.Fatalf("%s:%d row count: got %d want %d\ngot rows: %s",
			path, line, len(got.typed), len(want), dumpRows(got))
	}
	for i, doc := range want {
		if len(doc.cols) != len(got.Columns) {
			t.Fatalf("%s:%d row %d: %d columns, want %d (%v vs %v)",
				path, line, i, len(got.Columns), len(doc.cols), got.Columns, doc.cols)
		}
		for j := range doc.cols {
			if doc.cols[j] != got.Columns[j] {
				t.Fatalf("%s:%d row %d col %d: name %q, want %q",
					path, line, i, j, got.Columns[j], doc.cols[j])
			}
			if !valuesConform(doc.vals[j], got.typed[i][j]) {
				t.Fatalf("%s:%d row %d col %q: got %s(%s), want %s(%s)",
					path, line, i, doc.cols[j],
					got.typed[i][j].T, got.typed[i][j].FormatText(),
					doc.vals[j].T, doc.vals[j].FormatText())
			}
		}
	}
}

// valuesConform is the strict result-row equality: same type, same value.
// A text expectation may match a timestamp or bytea result through the
// dialect's deterministic text conversion (chai docs write timestamps as
// strings); numbers never cross the int/double line.
func valuesConform(want, got Value) bool {
	if want.IsNull() || got.IsNull() {
		return want.IsNull() && got.IsNull()
	}
	if want.T == got.T {
		c, err := Compare(want, got)
		return err == nil && c == 0
	}
	if want.T == TypeText && (got.T == TypeTimestamp || got.T == TypeBytea) {
		c, err := Compare(want, got)
		return err == nil && c == 0
	}
	return false
}

func dumpRows(r ExecResult) string {
	var b strings.Builder
	for _, row := range r.typed {
		b.WriteString("\n  {")
		for j, v := range row {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s: %s(%s)", r.Columns[j], v.T, v.FormatText())
		}
		b.WriteString("}")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// expr/ corpus (genexprtests format)
// ---------------------------------------------------------------------------

// exprStmt is one "> expr" or "! expr" line with its expectation.
type exprStmt struct {
	expr string
	res  string
	fail bool
	line int
}

type exprTest struct {
	name  string
	stmts []exprStmt
}

// parseExprFile ports REFERENCES/chai/internal/testutil/genexprtests.
func parseExprFile(t *testing.T, path string) []exprTest {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var tests []exprTest
	var curStmt *exprStmt
	for lineNo, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
		case strings.HasPrefix(line, "-- test:"):
			tests = append(tests, exprTest{name: strings.TrimPrefix(line, "-- test: ")})
		case strings.HasPrefix(line, "--"):
		case line[0] == '>':
			cur := &tests[len(tests)-1]
			cur.stmts = append(cur.stmts, exprStmt{expr: strings.TrimPrefix(line, "> "), line: lineNo + 1})
			curStmt = &cur.stmts[len(cur.stmts)-1]
		case line[0] == '!':
			cur := &tests[len(tests)-1]
			cur.stmts = append(cur.stmts, exprStmt{expr: strings.TrimPrefix(line, "! "), fail: true, line: lineNo + 1})
			curStmt = &cur.stmts[len(cur.stmts)-1]
		default:
			if curStmt == nil {
				t.Fatalf("%s:%d: expectation before any expression", path, lineNo+1)
			}
			if curStmt.fail {
				if len(line) < 2 || line[0] != '\'' || line[len(line)-1] != '\'' {
					t.Fatalf("%s:%d: error expectation must be quoted: %q", path, lineNo+1, line)
				}
				curStmt.res = line[1 : len(line)-1]
			} else {
				curStmt.res = line
			}
		}
	}
	return tests
}

func TestConformanceExpr(t *testing.T) {
	// chai's expression harness pins the transaction clock.
	savedNow := nowFunc
	nowFunc = func() time.Time { return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) }
	defer func() { nowFunc = savedNow }()

	dir := filepath.Join("testdata", "sqltests", "expr")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	stats := &confStats{}
	defer stats.report(t, "expr")

	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".sql" {
			continue
		}
		rel := "expr/" + ent.Name()
		if reason, ok := skipFiles[rel]; ok {
			n := countTestsInFile(t, filepath.Join(dir, ent.Name()))
			stats.skip += n
			t.Logf("skip %s (%d cases): %s", rel, n, reason)
			continue
		}
		path := filepath.Join(dir, ent.Name())
		tests := parseExprFile(t, path)
		t.Run(ent.Name(), func(t *testing.T) {
			for _, tc := range tests {
				if reason, ok := skipReason(rel, "", tc.name); ok {
					stats.skip++
					t.Logf("skip %s::%s: %s", rel, tc.name, reason)
					continue
				}
				ok := t.Run(tc.name, func(t *testing.T) {
					for _, st := range tc.stmts {
						runExprCase(t, path, st)
					}
				})
				if ok {
					stats.pass++
				} else {
					stats.fail++
				}
			}
		})
	}
}

// runExprCase evaluates one expression with no table in scope and checks
// the expected value (chai compares with EQ, i.e. the dialect's own loose
// equality) or that the error message contains the expected fragment.
func runExprCase(t *testing.T, path string, st exprStmt) {
	t.Helper()
	got, gotErr := evalDocExpr(st.expr)
	if st.fail {
		// A parse error or an eval error both satisfy the expectation, as
		// in chai's harness; the message must contain the fragment.
		if gotErr == nil {
			t.Errorf("%s:%d `%s`: expected error %q, got %s(%s)",
				path, st.line, st.expr, st.res, got.T, got.FormatText())
			return
		}
		if !strings.Contains(gotErr.Error(), st.res) {
			t.Errorf("%s:%d `%s`: error %q does not contain %q", path, st.line, st.expr, gotErr.Error(), st.res)
		}
		return
	}
	if gotErr != nil {
		t.Errorf("%s:%d `%s`: unexpected error: %v", path, st.line, st.expr, gotErr)
		return
	}
	want, err := evalDocExpr(st.res)
	if err != nil {
		t.Errorf("%s:%d bad expectation %q: %v", path, st.line, st.res, err)
		return
	}
	if want.IsNull() || got.IsNull() {
		if want.IsNull() != got.IsNull() {
			t.Errorf("%s:%d `%s`: got %s(%s), want %s(%s)",
				path, st.line, st.expr, got.T, got.FormatText(), want.T, want.FormatText())
		}
		return
	}
	c, cerr := Compare(want, got)
	if cerr != nil || c != 0 {
		t.Errorf("%s:%d `%s`: got %s(%s), want %s(%s)",
			path, st.line, st.expr, got.T, got.FormatText(), want.T, want.FormatText())
	}
}
