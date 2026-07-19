// expr.go — the requiresPhase boolean-expression evaluator (Draft 003
// §5.2). A bare phase name means "that phase succeeded"; the grammar adds
// &&, ||, !, and parentheses. The parser is a tiny recursive descent over
// a tokenizer, evaluated against a predicate that answers "did this phase
// succeed?" — the DAG driver decides skip-vs-run from the result (a false
// expression skips the phase, §5.2).
//
// ParseSpec/ValidateSpec (pkg/domain/build) already reject unknown
// references and cycles at trigger time, so evaluation here trusts the
// names; a lookup for an unknown phase resolves false.
package runner

import (
	"fmt"
	"strings"
)

// evalRequires reports whether a requiresPhase expression is satisfied,
// given a predicate that answers "did phase <name> succeed?". An empty
// expression is a root — always satisfied.
func evalRequires(expr string, succeeded func(name string) bool) (bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return true, nil
	}
	p := &exprParser{tokens: tokenizeExpr(expr), succeeded: succeeded}
	v, err := p.parseOr()
	if err != nil {
		return false, err
	}
	if p.pos != len(p.tokens) {
		return false, fmt.Errorf("unexpected token %q in requiresPhase", p.tokens[p.pos])
	}
	return v, nil
}

// exprParser is a single-pass recursive-descent evaluator.
type exprParser struct {
	tokens    []string
	pos       int
	succeeded func(name string) bool
}

func (p *exprParser) peek() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return ""
}

func (p *exprParser) next() string {
	t := p.peek()
	p.pos++
	return t
}

// parseOr := parseAnd ( "||" parseAnd )*
func (p *exprParser) parseOr() (bool, error) {
	v, err := p.parseAnd()
	if err != nil {
		return false, err
	}
	for p.peek() == "||" {
		p.next()
		rhs, err := p.parseAnd()
		if err != nil {
			return false, err
		}
		v = v || rhs
	}
	return v, nil
}

// parseAnd := parseUnary ( "&&" parseUnary )*
func (p *exprParser) parseAnd() (bool, error) {
	v, err := p.parseUnary()
	if err != nil {
		return false, err
	}
	for p.peek() == "&&" {
		p.next()
		rhs, err := p.parseUnary()
		if err != nil {
			return false, err
		}
		v = v && rhs
	}
	return v, nil
}

// parseUnary := "!" parseUnary | parsePrimary
func (p *exprParser) parseUnary() (bool, error) {
	if p.peek() == "!" {
		p.next()
		v, err := p.parseUnary()
		if err != nil {
			return false, err
		}
		return !v, nil
	}
	return p.parsePrimary()
}

// parsePrimary := "(" parseOr ")" | name
func (p *exprParser) parsePrimary() (bool, error) {
	t := p.peek()
	switch t {
	case "":
		return false, fmt.Errorf("requiresPhase ended unexpectedly")
	case "(":
		p.next()
		v, err := p.parseOr()
		if err != nil {
			return false, err
		}
		if p.next() != ")" {
			return false, fmt.Errorf("missing ) in requiresPhase")
		}
		return v, nil
	case ")", "&&", "||", "!":
		return false, fmt.Errorf("unexpected %q in requiresPhase", t)
	default:
		p.next()
		return p.succeeded(t), nil
	}
}

// tokenizeExpr splits an expression into names and operator tokens
// (&&, ||, !, (, )). Whitespace separates; operators need no spaces.
func tokenizeExpr(expr string) []string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	runes := []rune(expr)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		case c == '(' || c == ')' || c == '!':
			flush()
			tokens = append(tokens, string(c))
		case c == '&' || c == '|':
			flush()
			// Coalesce && and || (a lone & or | still tokenizes as itself,
			// which parsePrimary rejects — a clear error, not a silent pass).
			if i+1 < len(runes) && runes[i+1] == c {
				tokens = append(tokens, string([]rune{c, c}))
				i++
			} else {
				tokens = append(tokens, string(c))
			}
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return tokens
}
