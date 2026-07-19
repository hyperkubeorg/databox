// lexer.go tokenizes SQL text for the parser. The token vocabulary is
// the chai dialect's (REFERENCES/chai/internal/sql/scanner is the spec):
// unquoted identifiers fold to lowercase, 'single quotes' are string
// literals (with ” as the escape for a quote), "double quotes" are
// identifiers, both comment styles work, and != / <> / || / <= / >= are
// multi-character operators.
package sql

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// tokKind classifies a token.
type tokKind int

// Token kinds. Keywords are their own kind with the keyword uppercased
// in tok.s, which keeps the parser's keyword checks readable.
const (
	tkEOF     tokKind = iota
	tkIdent           // identifier (lowercased if it was unquoted)
	tkKeyword         // reserved word, uppercased in s
	tkNumber          // numeric literal (s holds the raw spelling)
	tkString          // 'string literal' (s holds the decoded value)
	tkOp              // operator or punctuation (s holds the spelling)
	tkParam           // $N positional parameter (s holds the digits)
)

// token is one lexeme with its position for error messages.
type token struct {
	kind tokKind
	s    string
	pos  int // byte offset in the input, for error reporting
	// orig preserves an unquoted identifier's source spelling. Name lookups
	// are case-insensitive (s is lowercased) but aliases label result
	// columns with the case the user typed ("SELECT 1 AS A" → column "A").
	orig string
}

// labelOf is the spelling an identifier token contributes to a result
// column label: the original case when it was a bare word, s otherwise
// (quoted identifiers already keep their case in s).
func labelOf(t token) string {
	if t.orig != "" {
		return t.orig
	}
	return t.s
}

// keywords is every reserved word the parser cares about. Anything else
// scans as an identifier (so column names like "value" keep working).
var keywords = map[string]bool{}

func init() {
	for _, w := range []string{
		"SELECT", "FROM", "WHERE", "GROUP", "BY", "ORDER", "LIMIT",
		"OFFSET", "ASC", "DESC", "DISTINCT", "ALL", "UNION", "AS",
		"INSERT", "INTO", "VALUES", "RETURNING", "UPDATE", "SET",
		"DELETE", "CREATE", "DROP", "TABLE", "INDEX", "UNIQUE",
		"PRIMARY", "KEY", "NOT", "NULL", "DEFAULT", "CHECK", "ON",
		"IF", "EXISTS", "AND", "OR", "IN", "LIKE", "BETWEEN", "IS",
		"TRUE", "FALSE", "CAST", "BEGIN", "COMMIT", "ROLLBACK",
		"TRANSACTION", "CONSTRAINT", "CONFLICT", "DO", "NOTHING",
		"REPLACE",
	} {
		keywords[w] = true
	}
}

// lexer walks the input producing tokens one at a time.
type lexer struct {
	src string
	i   int
}

// errAt formats a parse/lex error with its byte position.
func errAt(pos int, format string, args ...any) error {
	return fmt.Errorf("at offset %d: %s", pos, fmt.Sprintf(format, args...))
}

// next returns the next token, skipping whitespace and comments.
func (lx *lexer) next() (token, error) {
	for {
		lx.skipSpace()
		if lx.i >= len(lx.src) {
			return token{kind: tkEOF, pos: lx.i}, nil
		}
		// "--" line comments and "/* */" block comments vanish here so
		// the parser never sees them.
		if strings.HasPrefix(lx.src[lx.i:], "--") {
			nl := strings.IndexByte(lx.src[lx.i:], '\n')
			if nl < 0 {
				lx.i = len(lx.src)
			} else {
				lx.i += nl + 1
			}
			continue
		}
		if strings.HasPrefix(lx.src[lx.i:], "/*") {
			end := strings.Index(lx.src[lx.i+2:], "*/")
			if end < 0 {
				return token{}, errAt(lx.i, "unterminated block comment")
			}
			lx.i += 2 + end + 2
			continue
		}
		break
	}
	pos := lx.i
	c := lx.src[lx.i]

	switch {
	case c == '\'':
		return lx.scanString(pos)
	case c == '"':
		return lx.scanQuotedIdent(pos)
	case c == '$':
		return lx.scanParam(pos)
	case c >= '0' && c <= '9':
		return lx.scanNumber(pos)
	case c == '.' && lx.i+1 < len(lx.src) && isDigit(lx.src[lx.i+1]):
		return lx.scanNumber(pos)
	case isIdentStart(rune(c)) || c >= utf8.RuneSelf:
		return lx.scanIdent(pos)
	}

	// Multi-character operators first, longest match wins — so the vector
	// distance operators are listed before their prefixes: "<->" before
	// "<>"-would-never-match-it but crucially "<=>" before "<=", and both
	// before bare "<". "::" is the short-form cast (1::TEXT), cloned from
	// chai's grammar; "<->"/"<#>"/"<=>" are the pgvector distance operators.
	for _, op := range []string{"<->", "<=>", "<#>", "!=", "<>", "<=", ">=", "||", "::"} {
		if strings.HasPrefix(lx.src[lx.i:], op) {
			lx.i += len(op)
			if op == "<>" {
				op = "!=" // normalize both not-equals spellings
			}
			return token{kind: tkOp, s: op, pos: pos}, nil
		}
	}
	switch c {
	case '+', '-', '*', '/', '%', '&', '|', '^', '=', '<', '>',
		'(', ')', ',', ';', '.':
		// A bare '.' (not starting a number — that case matched above) is
		// the qualified-name separator: table.column (join.go).
		lx.i++
		return token{kind: tkOp, s: string(c), pos: pos}, nil
	}
	return token{}, errAt(pos, "unexpected character %q", string(c))
}

// skipSpace advances past whitespace.
func (lx *lexer) skipSpace() {
	for lx.i < len(lx.src) {
		switch lx.src[lx.i] {
		case ' ', '\t', '\r', '\n', '\v', '\f':
			lx.i++
		default:
			return
		}
	}
}

// scanString reads a 'literal', where ” encodes a single quote.
func (lx *lexer) scanString(pos int) (token, error) {
	lx.i++ // opening quote
	var b strings.Builder
	for lx.i < len(lx.src) {
		c := lx.src[lx.i]
		if c == '\'' {
			// '' inside a string is one literal quote.
			if lx.i+1 < len(lx.src) && lx.src[lx.i+1] == '\'' {
				b.WriteByte('\'')
				lx.i += 2
				continue
			}
			lx.i++
			return token{kind: tkString, s: b.String(), pos: pos}, nil
		}
		b.WriteByte(c)
		lx.i++
	}
	return token{}, errAt(pos, "unterminated string literal")
}

// scanQuotedIdent reads a "quoted identifier"; "" escapes a quote.
// Quoted identifiers keep their exact case and may contain spaces.
func (lx *lexer) scanQuotedIdent(pos int) (token, error) {
	lx.i++
	var b strings.Builder
	for lx.i < len(lx.src) {
		c := lx.src[lx.i]
		if c == '"' {
			if lx.i+1 < len(lx.src) && lx.src[lx.i+1] == '"' {
				b.WriteByte('"')
				lx.i += 2
				continue
			}
			lx.i++
			return token{kind: tkIdent, s: b.String(), pos: pos}, nil
		}
		b.WriteByte(c)
		lx.i++
	}
	return token{}, errAt(pos, "unterminated quoted identifier")
}

// scanParam reads a $N positional parameter reference.
func (lx *lexer) scanParam(pos int) (token, error) {
	lx.i++
	start := lx.i
	for lx.i < len(lx.src) && isDigit(lx.src[lx.i]) {
		lx.i++
	}
	if lx.i == start {
		return token{}, errAt(pos, "expected digits after $")
	}
	return token{kind: tkParam, s: lx.src[start:lx.i], pos: pos}, nil
}

// scanNumber reads integer or float spellings including exponents
// (1, 1.5, .5, 1e9, 1.2E-3).
func (lx *lexer) scanNumber(pos int) (token, error) {
	start := lx.i
	seenDot, seenExp := false, false
	for lx.i < len(lx.src) {
		c := lx.src[lx.i]
		switch {
		case isDigit(c):
			lx.i++
		case c == '.' && !seenDot && !seenExp:
			seenDot = true
			lx.i++
		case (c == 'e' || c == 'E') && !seenExp && lx.i > start:
			seenExp = true
			lx.i++
			// optional sign immediately after the exponent marker
			if lx.i < len(lx.src) && (lx.src[lx.i] == '+' || lx.src[lx.i] == '-') {
				lx.i++
			}
		default:
			return token{kind: tkNumber, s: lx.src[start:lx.i], pos: pos}, nil
		}
	}
	return token{kind: tkNumber, s: lx.src[start:lx.i], pos: pos}, nil
}

// scanIdent reads a bare identifier or keyword. Unquoted identifiers
// fold to lowercase — the dialect is case-insensitive (MISC/casing.sql).
func (lx *lexer) scanIdent(pos int) (token, error) {
	start := lx.i
	for lx.i < len(lx.src) {
		r, size := utf8.DecodeRuneInString(lx.src[lx.i:])
		if !isIdentPart(r) {
			break
		}
		lx.i += size
	}
	word := lx.src[start:lx.i]
	upper := strings.ToUpper(word)
	if keywords[upper] {
		return token{kind: tkKeyword, s: upper, pos: pos}, nil
	}
	return token{kind: tkIdent, s: strings.ToLower(word), pos: pos, orig: word}, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isIdentPart(r rune) bool {
	return r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
