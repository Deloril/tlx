package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// This file implements the text filter query language used by the search box.
// A query is a boolean expression over field comparisons, for example:
//
//	Summary=derp AND (tag=bad OR tag=suspicious)
//	Host=/^dc-\d+$/ AND NOT tag=benign
//
// Grammar (precedence NOT > AND > OR; parentheses override):
//
//	or    := and ( "OR" and )*
//	and   := not ( "AND" not )*
//	not   := "NOT" not | atom
//	atom  := "(" or ")" | term
//	term  := field ("=" | "!=") value | value
//	value := bareword | "quoted" | /regex/
//
// field is a column name (case-insensitive), or one of the special names
// tag/tags, comment/comments, row/#, any/*. A bare value with no field matches
// any column. Matching is substring and case-insensitive unless the caller asks
// for case sensitivity; a value wrapped in /…/ is a regular expression.

// rowPred is a compiled predicate over a single row.
type rowPred func(row int, rec []string) bool

type qop int

const (
	opTerm qop = iota
	opAnd
	opOr
	opNot
)

// qtimeop is a timestamp comparison operator on a term.
type qtimeop int

const (
	tNone qtimeop = iota
	tBefore
	tAfter
	tBetween
)

// qnode is a parsed query expression node.
type qnode struct {
	op   qop
	kids []*qnode

	// term fields (op == opTerm)
	field    string
	hasField bool
	neg      bool // the != operator
	value    string
	regex    bool

	// timestamp comparison (timeOp != tNone). timeA is the operand for
	// before/after and the low bound for between; timeB is the high bound.
	timeOp qtimeop
	timeA  string
	timeB  string
}

// ParseQuery parses a filter query string into an expression tree. An empty or
// whitespace-only string yields (nil, nil): no filter.
func ParseQuery(s string) (*qnode, error) {
	toks, err := lexQuery(s)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, nil
	}
	p := &qparser{toks: toks}
	node, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("unexpected %s", p.toks[p.pos].describe())
	}
	return node, nil
}

// --- lexer ---

type tkind int

const (
	tkLParen tkind = iota
	tkRParen
	tkAnd
	tkOr
	tkNot
	tkTerm
)

type qtoken struct {
	kind     tkind
	field    string
	hasField bool
	neg      bool
	value    string
	regex    bool

	timeOp qtimeop
	timeA  string
	timeB  string
}

func (t qtoken) describe() string {
	switch t.kind {
	case tkLParen:
		return "'('"
	case tkRParen:
		return "')'"
	case tkAnd:
		return "'AND'"
	case tkOr:
		return "'OR'"
	case tkNot:
		return "'NOT'"
	default:
		return "term"
	}
}

type atomKind int

const (
	atomNone atomKind = iota
	atomBare
	atomQuoted
	atomRegex
)

func lexQuery(s string) ([]qtoken, error) {
	var toks []qtoken
	i, n := 0, len(s)
	for i < n {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			i++
			continue
		case '(':
			toks = append(toks, qtoken{kind: tkLParen})
			i++
			continue
		case ')':
			toks = append(toks, qtoken{kind: tkRParen})
			i++
			continue
		}

		val, kind, next, err := readAtom(s, i)
		if err != nil {
			return nil, err
		}
		if kind == atomNone {
			return nil, fmt.Errorf("unexpected character %q", s[i])
		}
		i = next

		// A bare word may be a boolean keyword.
		if kind == atomBare {
			switch strings.ToUpper(val) {
			case "AND":
				toks = append(toks, qtoken{kind: tkAnd})
				continue
			case "OR":
				toks = append(toks, qtoken{kind: tkOr})
				continue
			case "NOT":
				toks = append(toks, qtoken{kind: tkNot})
				continue
			}
		}

		// Look for a comparison operator following the first atom.
		j := skipSpaces(s, i)
		if j < n && (s[j] == '=' || (s[j] == '!' && j+1 < n && s[j+1] == '=')) {
			neg := s[j] == '!'
			if neg {
				j += 2
			} else {
				j++
			}
			j = skipSpaces(s, j)
			vval, vkind, vnext, err := readAtom(s, j)
			if err != nil {
				return nil, err
			}
			if vkind == atomNone {
				return nil, fmt.Errorf("expected a value after %q", opStr(neg))
			}
			i = vnext
			toks = append(toks, qtoken{
				kind: tkTerm, field: val, hasField: true,
				neg: neg, value: vval, regex: vkind == atomRegex,
			})
			continue
		}

		// A field followed by a timestamp keyword: FIELD before|after|between …
		if kind == atomBare {
			j := skipSpaces(s, i)
			kw, kwkind, kwnext, _ := readAtom(s, j)
			if kwkind == atomBare {
				switch strings.ToLower(kw) {
				case "before", "after":
					opRaw, next := readTimeOperand(s, kwnext)
					if opRaw == "" {
						return nil, fmt.Errorf("expected a time after %q", kw)
					}
					op := tBefore
					if strings.EqualFold(kw, "after") {
						op = tAfter
					}
					toks = append(toks, qtoken{kind: tkTerm, field: val, hasField: true, timeOp: op, timeA: opRaw})
					i = next
					continue
				case "between":
					aRaw, afterAnd, ok := readBetweenLow(s, kwnext)
					if !ok {
						return nil, errors.New("'between' needs 'and': FIELD between X and Y")
					}
					bRaw, next := readTimeOperand(s, afterAnd)
					if aRaw == "" || bRaw == "" {
						return nil, errors.New("'between' needs two times: FIELD between X and Y")
					}
					toks = append(toks, qtoken{kind: tkTerm, field: val, hasField: true, timeOp: tBetween, timeA: aRaw, timeB: bRaw})
					i = next
					continue
				}
			}
		}

		// Otherwise a bare value term (matches any column).
		toks = append(toks, qtoken{kind: tkTerm, value: val, regex: kind == atomRegex})
	}
	return toks, nil
}

// readTimeOperand consumes the raw text of a time operand starting at from: it
// runs to the next boolean keyword (AND/OR/NOT), parenthesis or end of input, so
// an operand may contain spaces (a "date time" literal) and a " ± <dur>" suffix.
// It returns the trimmed operand text and the offset just past it.
func readTimeOperand(s string, from int) (string, int) {
	start := skipSpaces(s, from)
	i, last := start, start
	for {
		j := skipSpaces(s, i)
		if j >= len(s) || s[j] == '(' || s[j] == ')' {
			break
		}
		atom, k, next, err := readAtom(s, j)
		if err != nil || k == atomNone {
			break
		}
		if k == atomBare {
			switch strings.ToUpper(atom) {
			case "AND", "OR", "NOT":
				return strings.TrimSpace(s[start:last]), i
			}
		}
		i, last = next, next
	}
	return strings.TrimSpace(s[start:last]), i
}

// readBetweenLow consumes the low operand of a between up to the 'and' keyword.
// It returns the operand text, the offset just past 'and', and ok=false if no
// 'and' separator is found.
func readBetweenLow(s string, from int) (string, int, bool) {
	start := skipSpaces(s, from)
	i, last := start, start
	for {
		j := skipSpaces(s, i)
		if j >= len(s) || s[j] == '(' || s[j] == ')' {
			return "", i, false
		}
		atom, k, next, err := readAtom(s, j)
		if err != nil || k == atomNone {
			return "", i, false
		}
		if k == atomBare && strings.EqualFold(atom, "and") {
			return strings.TrimSpace(s[start:last]), next, true
		}
		i, last = next, next
	}
}

func opStr(neg bool) string {
	if neg {
		return "!="
	}
	return "="
}

func skipSpaces(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

// readAtom reads one value/identifier atom starting at i: a quoted string, a
// /regex/, or a bareword. It returns the unwrapped text and how it was written.
func readAtom(s string, i int) (string, atomKind, int, error) {
	n := len(s)
	if i >= n {
		return "", atomNone, i, nil
	}
	switch s[i] {
	case '"':
		i++
		var b strings.Builder
		for i < n {
			if s[i] == '\\' && i+1 < n {
				b.WriteByte(s[i+1])
				i += 2
				continue
			}
			if s[i] == '"' {
				return b.String(), atomQuoted, i + 1, nil
			}
			b.WriteByte(s[i])
			i++
		}
		return "", atomQuoted, i, errors.New("unterminated quoted string")
	case '/':
		i++
		var b strings.Builder
		for i < n {
			if s[i] == '\\' && i+1 < n && s[i+1] == '/' {
				b.WriteByte('/')
				i += 2
				continue
			}
			if s[i] == '/' {
				return b.String(), atomRegex, i + 1, nil
			}
			b.WriteByte(s[i])
			i++
		}
		return "", atomRegex, i, errors.New("unterminated /regex/")
	default:
		start := i
		for i < n {
			c := s[i]
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' ||
				c == '(' || c == ')' || c == '=' || c == '"' {
				break
			}
			if c == '!' && i+1 < n && s[i+1] == '=' {
				break
			}
			i++
		}
		if i == start {
			return "", atomNone, i, nil
		}
		return s[start:i], atomBare, i, nil
	}
}

// --- parser ---

type qparser struct {
	toks []qtoken
	pos  int
}

func (p *qparser) peek() (qtoken, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return qtoken{}, false
}

func (p *qparser) parseOr() (*qnode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind != tkOr {
			return left, nil
		}
		p.pos++
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &qnode{op: opOr, kids: []*qnode{left, right}}
	}
}

func (p *qparser) parseAnd() (*qnode, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind != tkAnd {
			return left, nil
		}
		p.pos++
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &qnode{op: opAnd, kids: []*qnode{left, right}}
	}
}

func (p *qparser) parseNot() (*qnode, error) {
	t, ok := p.peek()
	if ok && t.kind == tkNot {
		p.pos++
		child, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &qnode{op: opNot, kids: []*qnode{child}}, nil
	}
	return p.parseAtom()
}

// --- compilation to a row predicate ---

// compileExpr turns a parsed query tree into a predicate over the view's rows,
// resolving field names against the current columns and compiling each value's
// matcher. cased applies to every value in the expression; now is the reference
// time for the now/time keyword and relative arithmetic in time comparisons.
func (v *View) compileExpr(n *qnode, cased bool, now time.Time) (rowPred, error) {
	switch n.op {
	case opAnd:
		left, err := v.compileExpr(n.kids[0], cased, now)
		if err != nil {
			return nil, err
		}
		right, err := v.compileExpr(n.kids[1], cased, now)
		if err != nil {
			return nil, err
		}
		return func(row int, rec []string) bool {
			return left(row, rec) && right(row, rec)
		}, nil
	case opOr:
		left, err := v.compileExpr(n.kids[0], cased, now)
		if err != nil {
			return nil, err
		}
		right, err := v.compileExpr(n.kids[1], cased, now)
		if err != nil {
			return nil, err
		}
		return func(row int, rec []string) bool {
			return left(row, rec) || right(row, rec)
		}, nil
	case opNot:
		child, err := v.compileExpr(n.kids[0], cased, now)
		if err != nil {
			return nil, err
		}
		return func(row int, rec []string) bool {
			return !child(row, rec)
		}, nil
	default: // opTerm
		if n.timeOp != tNone {
			return v.compileTimeTerm(n, now)
		}
		ref, err := v.resolveField(n.field, n.hasField)
		if err != nil {
			return nil, err
		}
		m, err := compileMatcher(n.value, n.regex, cased)
		if err != nil {
			return nil, err
		}
		neg := n.neg
		return func(row int, rec []string) bool {
			hit := v.columnMatch(row, rec, ref, m)
			if neg {
				return !hit
			}
			return hit
		}, nil
	}
}

// compileTimeTerm builds a predicate for a before/after/between comparison. The
// field must name a data column; a row matches only if its cell parses as a
// timestamp and falls in range. Rows with an unparseable cell never match.
func (v *View) compileTimeTerm(n *qnode, now time.Time) (rowPred, error) {
	ref, err := v.resolveField(n.field, n.hasField)
	if err != nil {
		return nil, err
	}
	if ref < 0 {
		return nil, fmt.Errorf("time comparison needs a data column, not %q", n.field)
	}
	a, ok := parseTimeOperand(n.timeA, now)
	if !ok {
		return nil, fmt.Errorf("cannot parse time %q", n.timeA)
	}
	if n.timeOp == tBetween {
		b, ok := parseTimeOperand(n.timeB, now)
		if !ok {
			return nil, fmt.Errorf("cannot parse time %q", n.timeB)
		}
		return func(row int, rec []string) bool {
			t, ok := ParseTime(v.cell(row, rec, ref))
			return ok && !t.Before(a.start) && t.Before(b.end)
		}, nil
	}
	op := n.timeOp
	return func(row int, rec []string) bool {
		t, ok := ParseTime(v.cell(row, rec, ref))
		if !ok {
			return false
		}
		if op == tBefore {
			return t.Before(a.start)
		}
		return !t.Before(a.end) // tAfter: at or past the end of the named period
	}, nil
}

// resolveField maps a query field name to a column reference. A term with no
// field matches any column. Special names cover the virtual columns; anything
// else must match a data column header (case-insensitively).
func (v *View) resolveField(field string, hasField bool) (ColumnRef, error) {
	if !hasField {
		return ColAll, nil
	}
	switch strings.ToLower(field) {
	case "tag", "tags":
		return ColTags, nil
	case "comment", "comments":
		return ColComment, nil
	case "row", "#", "line":
		return ColRowNum, nil
	case "any", "*":
		return ColAll, nil
	}
	headers := v.idx.Headers()
	for i, h := range headers {
		if strings.EqualFold(h, field) {
			return ColumnRef(i), nil
		}
	}
	return ColNone, fmt.Errorf("unknown field %q", field)
}

func (p *qparser) parseAtom() (*qnode, error) {
	t, ok := p.peek()
	if !ok {
		return nil, errors.New("unexpected end of query")
	}
	switch t.kind {
	case tkLParen:
		p.pos++
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		c, ok := p.peek()
		if !ok || c.kind != tkRParen {
			return nil, errors.New("missing ')'")
		}
		p.pos++
		return inner, nil
	case tkTerm:
		p.pos++
		return &qnode{
			op: opTerm, field: t.field, hasField: t.hasField,
			neg: t.neg, value: t.value, regex: t.regex,
			timeOp: t.timeOp, timeA: t.timeA, timeB: t.timeB,
		}, nil
	default:
		return nil, fmt.Errorf("unexpected %s", t.describe())
	}
}
