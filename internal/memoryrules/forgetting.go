package memoryrules

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

// Evaluate runs expr against f (hand-written tokenizer + recursive-descent parser,
// no dependency). Precedence: `||`, `&&`, `!`, comparison, primary. `==`/`!=` accept
// strings; other comparisons require numeric on both sides.
func Evaluate(expr string, f Fields) (bool, error) {
	if strings.TrimSpace(expr) == "" {
		return false, fmt.Errorf("when must not be empty")
	}
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

var twoCharOps = []struct {
	sym  string
	kind tokKind
}{
	{"&&", tokAnd},
	{"||", tokOr},
	{"==", tokEq},
	{"!=", tokNe},
	{"<=", tokLe},
	{">=", tokGe},
}

// scanTwoChar: the two-character operator at i, if one starts there.
func scanTwoChar(expr string, i int) (tokKind, string, bool) {
	if i+1 >= len(expr) {
		return 0, "", false
	}
	for _, op := range twoCharOps {
		if expr[i:i+2] == op.sym {
			return op.kind, op.sym, true
		}
	}
	return 0, "", false
}

// singleCharKind: the kind of a one-character operator (! < >).
func singleCharKind(c byte) tokKind {
	switch c {
	case '!':
		return tokNot
	case '<':
		return tokLt
	case '>':
		return tokGt
	}
	return 0
}

// scanString: the "..." literal starting at i; returns the token and the index just past it.
func scanString(expr string, i int) (token, int, error) {
	start := i
	i++
	var sb strings.Builder
	for i < len(expr) && expr[i] != '"' {
		sb.WriteByte(expr[i])
		i++
	}
	if i >= len(expr) {
		return token{}, 0, fmt.Errorf("unterminated string literal at position %d", start)
	}
	return token{tokString, sb.String(), start}, i + 1, nil
}

// literalStarts: whether a string, integer (optional leading -), or identifier literal starts at i.
func literalStarts(expr string, i int) bool {
	c := expr[i]
	return c == '"' || isIdentStart(c) || (c >= '0' && c <= '9') || (c == '-' && i+1 < len(expr) && expr[i+1] >= '0' && expr[i+1] <= '9')
}

// scanLiteral: the literal starting at i (string, integer, or identifier); returns the token and the index just past it.
func scanLiteral(expr string, i int) (token, int, error) {
	if expr[i] == '"' {
		return scanString(expr, i)
	}
	start := i
	if c := expr[i]; c >= '0' && c <= '9' || c == '-' {
		i++
		for i < len(expr) && expr[i] >= '0' && expr[i] <= '9' {
			i++
		}
		return token{tokInt, expr[start:i], start}, i, nil
	}
	for i < len(expr) && isIdentPart(expr[i]) {
		i++
	}
	return token{tokIdent, expr[start:i], start}, i, nil
}

func tokenize(expr string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(expr) {
		c := expr[i]
		switch c {
		case ' ', '\t', '\n', '\r':
			i++
		case '(':
			toks = append(toks, token{tokLParen, "(", i})
			i++
		case ')':
			toks = append(toks, token{tokRParen, ")", i})
			i++
		case '"':
			t, next, err := scanString(expr, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, t)
			i = next
		default:
			if kind, sym, ok := scanTwoChar(expr, i); ok {
				toks = append(toks, token{kind, sym, i})
				i += 2
				break
			}
			if c == '!' || c == '<' || c == '>' {
				toks = append(toks, token{singleCharKind(c), string(c), i})
				i++
				break
			}
			if literalStarts(expr, i) {
				t, next, err := scanLiteral(expr, i)
				if err != nil {
					return nil, err
				}
				toks = append(toks, t)
				i = next
				break
			}
			return nil, fmt.Errorf("unexpected character %q at position %d", c, i)
		}
	}
	toks = append(toks, token{tokEOF, "", len(expr)})
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

// parseBinary folds a left-associative run of op over operand results: both
// sides must be boolean at each step, and combine replaces the left value.
func (p *exprParser) parseBinary(opKind tokKind, op string, operand func() (value, error), combine func(l, r value) value) (value, error) {
	v, err := operand()
	if err != nil {
		return value{}, err
	}
	for p.cur().kind == opKind {
		if v.kind != valBool {
			return value{}, fmt.Errorf("`%s` at position %d requires boolean operands", op, p.cur().pos)
		}
		p.pos++
		rhs, err := operand()
		if err != nil {
			return value{}, err
		}
		if rhs.kind != valBool {
			return value{}, fmt.Errorf("`%s` requires boolean operands", op)
		}
		v = combine(v, rhs)
	}
	return v, nil
}

func (p *exprParser) parseOr() (value, error) {
	return p.parseBinary(tokOr, "||", p.parseAnd, func(l, r value) value { return value{kind: valBool, b: l.b || r.b} })
}

func (p *exprParser) parseAnd() (value, error) {
	return p.parseBinary(tokAnd, "&&", p.parseUnary, func(l, r value) value { return value{kind: valBool, b: l.b && r.b} })
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
