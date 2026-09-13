package model

import (
	"sort"
	"strings"
)

// Highlighter locates the substrings a filter matches so the UI can highlight
// them in the grid. It holds the positive (non-negated) matchers of a spec: the
// boolean query's terms, the per-column conditions and the per-column quick
// filters. Negated query terms (NOT / !=) contribute nothing to highlight.
type Highlighter struct {
	terms []hlTerm
}

type hlTerm struct {
	ref ColumnRef // ColAll or a specific column
	m   matcher
}

// Empty reports whether there is nothing to highlight. Safe on a nil receiver.
func (h *Highlighter) Empty() bool { return h == nil || len(h.terms) == 0 }

// BuildHighlighter compiles the positive matchers of a spec. Parts that fail to
// compile are skipped; the same spec is validated separately by Apply, so an
// error there is surfaced to the user through the normal path.
func (v *View) BuildHighlighter(spec FilterSpec) *Highlighter {
	h := &Highlighter{}
	if spec.Expr != "" {
		if ast, err := ParseQuery(spec.Expr); err == nil && ast != nil {
			v.collectTerms(ast, false, spec.Cased, h)
		}
	}
	if spec.Query != "" {
		if m, err := compileMatcher(spec.Query, spec.Regexp, spec.Cased); err == nil {
			h.terms = append(h.terms, hlTerm{ref: spec.Column, m: m})
		}
	}
	for _, c := range spec.Conds {
		if c.Neg { // an excluded condition matches rows that DON'T contain the value
			continue
		}
		for _, val := range c.Values {
			if val == "" {
				continue
			}
			if m, err := compileMatcher(val, c.Regexp, c.Cased); err == nil {
				h.terms = append(h.terms, hlTerm{ref: c.Column, m: m})
			}
		}
	}
	for ref, val := range spec.ColFilters {
		if val == "" {
			continue
		}
		if m, err := colFilterMatcher(val, spec.Cased); err == nil {
			h.terms = append(h.terms, hlTerm{ref: ref, m: m})
		}
	}
	return h
}

// collectTerms walks the query tree adding positive terms. neg tracks whether an
// odd number of NOTs currently applies; a term is negated (and skipped) when
// that parity differs from the term's own != flag.
func (v *View) collectTerms(n *qnode, neg, cased bool, h *Highlighter) {
	switch n.op {
	case opAnd, opOr:
		v.collectTerms(n.kids[0], neg, cased, h)
		v.collectTerms(n.kids[1], neg, cased, h)
	case opNot:
		v.collectTerms(n.kids[0], !neg, cased, h)
	default: // opTerm
		if neg != n.neg { // effectively negated
			return
		}
		ref, err := v.resolveField(n.field, n.hasField)
		if err != nil || ref == ColNone {
			ref = ColAll
		}
		if m, err := compileMatcher(n.value, n.regex, cased); err == nil {
			h.terms = append(h.terms, hlTerm{ref: ref, m: m})
		}
	}
}

// Spans returns the byte ranges in text that the highlighter matches for the
// given column, merged so overlapping matches don't produce nested ranges. Terms
// scoped to another column are ignored; ColAll terms apply to every column.
func (h *Highlighter) Spans(ref ColumnRef, text string) [][2]int {
	if h.Empty() || text == "" {
		return nil
	}
	var spans [][2]int
	for _, t := range h.terms {
		if t.ref != ColAll && t.ref != ref {
			continue
		}
		spans = append(spans, t.m.spans(text)...)
	}
	return mergeSpans(spans)
}

// spans returns every non-empty match of the matcher within s, as byte ranges.
func (m matcher) spans(s string) [][2]int {
	if m.re != nil {
		locs := m.re.FindAllStringIndex(s, -1)
		out := make([][2]int, 0, len(locs))
		for _, l := range locs {
			if l[1] > l[0] {
				out = append(out, [2]int{l[0], l[1]})
			}
		}
		return out
	}
	if m.needle == "" {
		return nil
	}
	// m.needle is already lowercased when !cased (see compileMatcher). Lowercasing
	// the haystack keeps byte offsets aligned for ASCII, which covers the forensic
	// field values this matches against.
	hay := s
	if !m.cased {
		hay = strings.ToLower(s)
	}
	var out [][2]int
	for from := 0; from < len(hay); {
		i := strings.Index(hay[from:], m.needle)
		if i < 0 {
			break
		}
		start := from + i
		end := start + len(m.needle)
		out = append(out, [2]int{start, end})
		from = end
	}
	return out
}

// mergeSpans sorts and coalesces overlapping or touching ranges.
func mergeSpans(spans [][2]int) [][2]int {
	if len(spans) < 2 {
		return spans
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i][0] != spans[j][0] {
			return spans[i][0] < spans[j][0]
		}
		return spans[i][1] < spans[j][1]
	})
	out := spans[:1]
	for _, s := range spans[1:] {
		last := &out[len(out)-1]
		if s[0] <= last[1] {
			if s[1] > last[1] {
				last[1] = s[1]
			}
			continue
		}
		out = append(out, s)
	}
	return out
}
