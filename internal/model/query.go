package model

import (
	"errors"
	"fmt"
	"strings"
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

		// Otherwise a bare value term (matches any column).
		toks = append(toks, qtoken{kind: tkTerm, value: val, regex: kind == atomRegex})
	}
	return toks, nil
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
// matcher. cased applies to every value in the expression.
func (v *View) compileExpr(n *qnode, cased bool) (rowPred, error) {
	switch n.op {
	case opAnd:
		left, err := v.compileExpr(n.kids[0], cased)
		if err != nil {
			return nil, err
		}
		right, err := v.compileExpr(n.kids[1], cased)
		if err != nil {
			return nil, err
		}
		return func(row int, rec []string) bool {
			return left(row, rec) && right(row, rec)
		}, nil
	case opOr:
		left, err := v.compileExpr(n.kids[0], cased)
		if err != nil {
			return nil, err
		}
		right, err := v.compileExpr(n.kids[1], cased)
		if err != nil {
			return nil, err
		}
		return func(row int, rec []string) bool {
			return left(row, rec) || right(row, rec)
		}, nil
	case opNot:
		child, err := v.compileExpr(n.kids[0], cased)
		if err != nil {
			return nil, err
		}
		return func(row int, rec []string) bool {
			return !child(row, rec)
		}, nil
	default: // opTerm
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
		}, nil
	default:
		return nil, fmt.Errorf("unexpected %s", t.describe())
	}
}
