package memory

import (
	"fmt"
	"strconv"
	"strings"
)

// Rule is one forgetting rule (epic #1255 P3): When is a boolean expression
// over Fields, Then is "invalidate" or "keep". Rules are evaluated in order,
// first match wins; no match keeps the memory.
type Rule struct {
	When string
	Then string
}

const (
	ThenInvalidate = "invalidate"
	ThenKeep       = "keep"
)

// Fields is one memory's forgetting-relevant snapshot, computed by the
// caller from a point (age/days-since fields use "never" = age_days per the
// epic decision, so a memory never upvoted/recalled ages out on age alone).
type Fields struct {
	Upvotes         int
	Downvotes       int
	Score           int
	AgeDays         int64
	DaysSinceUpvote int64
	DaysSinceRecall int64
	Recalls         int
	Tier            string
	Scope           string
}

// DefaultRules are applied when config carries no memory.forgetting.rules
// (epic #1255 decision): unverified memories age out after 90 days without
// an upvote, any memory whose net score drops to -2 or below is invalidated,
// and a verified memory is otherwise kept.
func DefaultRules() []Rule {
	return []Rule{
		{When: `tier == "unverified" && days_since_upvote > 90`, Then: ThenInvalidate},
		{When: `score <= -2`, Then: ThenInvalidate},
		{When: `tier == "verified"`, Then: ThenKeep},
	}
}

// ValidateRules parses every rule's expression and rejects an unknown Then,
// so a config error is caught at load time (rule index + token position)
// rather than crashing or silently no-op'ing a bad rule inside the nightly sweep.
func ValidateRules(rules []Rule) error {
	for i, r := range rules {
		if r.Then != ThenInvalidate && r.Then != ThenKeep {
			return fmt.Errorf("forgetting rule %d: then must be %q or %q, got %q", i, ThenInvalidate, ThenKeep, r.Then)
		}
		if _, err := Evaluate(r.When, Fields{}); err != nil {
			return fmt.Errorf("forgetting rule %d: %w", i, err)
		}
	}
	return nil
}

// Evaluate parses and runs expr against f in one pass - hand-written
// tokenizer + recursive-descent parser, no external dependency, no
// reflection. Grammar (lowest to highest precedence): `||`, `&&`, unary `!`,
// comparison (`== != < <= > >=`), primary (field ident, int/quoted-string
// literal, parenthesized expr). Comparison operators other than == and !=
// require both sides numeric; == and != also work on strings.
func Evaluate(expr string, f Fields) (bool, error) {
	toks, err := tokenize(expr)
	if err != nil {
		return false, err
	}
	p := &exprParser{toks: toks, fields: f}
	v, err := p.parseOr()
	if err != nil {
		return false, err
	}
	if p.pos != len(p.toks)-1 { // everything but EOF must be consumed
		return false, fmt.Errorf("unexpected token %q at position %d", p.toks[p.pos].text, p.toks[p.pos].pos)
	}
	if v.kind != valBool {
		return false, fmt.Errorf("expression does not evaluate to a boolean")
	}
	return v.b, nil
}

// --- tokenizer ---

type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokInt
	tokString
	tokAnd
	tokOr
	tokNot
	tokEq
	tokNe
	tokLt
	tokLe
	tokGt
	tokGe
	tokLParen
	tokRParen
)

type token struct {
	kind tokKind
	text string
	pos  int
}

func tokenize(expr string) ([]token, error) {
	var toks []token
	i := 0
	n := len(expr)
	for i < n {
		c := expr[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			toks = append(toks, token{tokLParen, "(", i})
			i++
		case c == ')':
			toks = append(toks, token{tokRParen, ")", i})
			i++
		case c == '&' && i+1 < n && expr[i+1] == '&':
			toks = append(toks, token{tokAnd, "&&", i})
			i += 2
		case c == '|' && i+1 < n && expr[i+1] == '|':
			toks = append(toks, token{tokOr, "||", i})
			i += 2
		case c == '=' && i+1 < n && expr[i+1] == '=':
			toks = append(toks, token{tokEq, "==", i})
			i += 2
		case c == '!' && i+1 < n && expr[i+1] == '=':
			toks = append(toks, token{tokNe, "!=", i})
			i += 2
		case c == '!':
			toks = append(toks, token{tokNot, "!", i})
			i++
		case c == '<' && i+1 < n && expr[i+1] == '=':
			toks = append(toks, token{tokLe, "<=", i})
			i += 2
		case c == '<':
			toks = append(toks, token{tokLt, "<", i})
			i++
		case c == '>' && i+1 < n && expr[i+1] == '=':
			toks = append(toks, token{tokGe, ">=", i})
			i += 2
		case c == '>':
			toks = append(toks, token{tokGt, ">", i})
			i++
		case c == '"':
			start := i
			i++
			var sb strings.Builder
			closed := false
			for i < n {
				if expr[i] == '"' {
					closed = true
					i++
					break
				}
				sb.WriteByte(expr[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string literal at position %d", start)
			}
			toks = append(toks, token{tokString, sb.String(), start})
		case c >= '0' && c <= '9' || (c == '-' && i+1 < n && expr[i+1] >= '0' && expr[i+1] <= '9'):
			start := i
			i++
			for i < n && expr[i] >= '0' && expr[i] <= '9' {
				i++
			}
			toks = append(toks, token{tokInt, expr[start:i], start})
		case isIdentStart(c):
			start := i
			for i < n && isIdentPart(expr[i]) {
				i++
			}
			toks = append(toks, token{tokIdent, expr[start:i], start})
		default:
			return nil, fmt.Errorf("unexpected character %q at position %d", c, i)
		}
	}
	toks = append(toks, token{tokEOF, "", n})
	return toks, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// --- values ---

type valKind int

const (
	valBool valKind = iota
	valInt
	valStr
)

type value struct {
	kind valKind
	b    bool
	n    int64
	s    string
}

// --- parser ---

type exprParser struct {
	toks   []token
	pos    int
	fields Fields
}

func (p *exprParser) cur() token { return p.toks[p.pos] }

func (p *exprParser) parseOr() (value, error) {
	v, err := p.parseAnd()
	if err != nil {
		return value{}, err
	}
	for p.cur().kind == tokOr {
		if v.kind != valBool {
			return value{}, fmt.Errorf("`||` at position %d requires boolean operands", p.cur().pos)
		}
		p.pos++
		rhs, err := p.parseAnd()
		if err != nil {
			return value{}, err
		}
		if rhs.kind != valBool {
			return value{}, fmt.Errorf("`||` requires boolean operands")
		}
		v = value{kind: valBool, b: v.b || rhs.b}
	}
	return v, nil
}

func (p *exprParser) parseAnd() (value, error) {
	v, err := p.parseUnary()
	if err != nil {
		return value{}, err
	}
	for p.cur().kind == tokAnd {
		if v.kind != valBool {
			return value{}, fmt.Errorf("`&&` at position %d requires boolean operands", p.cur().pos)
		}
		p.pos++
		rhs, err := p.parseUnary()
		if err != nil {
			return value{}, err
		}
		if rhs.kind != valBool {
			return value{}, fmt.Errorf("`&&` requires boolean operands")
		}
		v = value{kind: valBool, b: v.b && rhs.b}
	}
	return v, nil
}

func (p *exprParser) parseUnary() (value, error) {
	if p.cur().kind == tokNot {
		p.pos++
		v, err := p.parseUnary()
		if err != nil {
			return value{}, err
		}
		if v.kind != valBool {
			return value{}, fmt.Errorf("`!` requires a boolean operand")
		}
		return value{kind: valBool, b: !v.b}, nil
	}
	return p.parseComparison()
}

func (p *exprParser) parseComparison() (value, error) {
	lhs, err := p.parsePrimary()
	if err != nil {
		return value{}, err
	}
	op := p.cur()
	switch op.kind {
	case tokEq, tokNe, tokLt, tokLe, tokGt, tokGe:
		p.pos++
	default:
		return lhs, nil
	}
	rhs, err := p.parsePrimary()
	if err != nil {
		return value{}, err
	}
	return compare(op, lhs, rhs)
}

func compare(op token, lhs, rhs value) (value, error) {
	if op.kind != tokEq && op.kind != tokNe {
		if lhs.kind != valInt || rhs.kind != valInt {
			return value{}, fmt.Errorf("operator %q at position %d requires numeric operands", op.text, op.pos)
		}
		var b bool
		switch op.kind {
		case tokLt:
			b = lhs.n < rhs.n
		case tokLe:
			b = lhs.n <= rhs.n
		case tokGt:
			b = lhs.n > rhs.n
		case tokGe:
			b = lhs.n >= rhs.n
		}
		return value{kind: valBool, b: b}, nil
	}
	if lhs.kind != rhs.kind {
		return value{}, fmt.Errorf("operator %q at position %d requires operands of the same type", op.text, op.pos)
	}
	var eq bool
	switch lhs.kind {
	case valInt:
		eq = lhs.n == rhs.n
	case valStr:
		eq = lhs.s == rhs.s
	default:
		return value{}, fmt.Errorf("operator %q at position %d cannot compare booleans", op.text, op.pos)
	}
	if op.kind == tokNe {
		eq = !eq
	}
	return value{kind: valBool, b: eq}, nil
}

func (p *exprParser) parsePrimary() (value, error) {
	t := p.cur()
	switch t.kind {
	case tokLParen:
		p.pos++
		v, err := p.parseOr()
		if err != nil {
			return value{}, err
		}
		if p.cur().kind != tokRParen {
			return value{}, fmt.Errorf("expected closing ')' at position %d", p.cur().pos)
		}
		p.pos++
		return v, nil
	case tokInt:
		p.pos++
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return value{}, fmt.Errorf("invalid integer %q at position %d", t.text, t.pos)
		}
		return value{kind: valInt, n: n}, nil
	case tokString:
		p.pos++
		return value{kind: valStr, s: t.text}, nil
	case tokIdent:
		p.pos++
		return lookupField(t, p.fields)
	default:
		return value{}, fmt.Errorf("unexpected token %q at position %d", t.text, t.pos)
	}
}

func lookupField(t token, f Fields) (value, error) {
	switch t.text {
	case "upvotes":
		return value{kind: valInt, n: int64(f.Upvotes)}, nil
	case "downvotes":
		return value{kind: valInt, n: int64(f.Downvotes)}, nil
	case "score":
		return value{kind: valInt, n: int64(f.Score)}, nil
	case "age_days":
		return value{kind: valInt, n: f.AgeDays}, nil
	case "days_since_upvote":
		return value{kind: valInt, n: f.DaysSinceUpvote}, nil
	case "days_since_recall":
		return value{kind: valInt, n: f.DaysSinceRecall}, nil
	case "recalls":
		return value{kind: valInt, n: int64(f.Recalls)}, nil
	case "tier":
		return value{kind: valStr, s: f.Tier}, nil
	case "scope":
		return value{kind: valStr, s: f.Scope}, nil
	default:
		return value{}, fmt.Errorf("unknown field %q at position %d", t.text, t.pos)
	}
}
