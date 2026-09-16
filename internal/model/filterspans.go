package model

import "strings"

// This file classifies a filter query into coloured spans for the filter bar's
// syntax highlighting. It follows the same grammar as ParseQuery (see query.go)
// but leniently: it never fails, so a half-typed query still colours sensibly.

// FilterSpanKind is the role a run of the query plays, for colouring.
type FilterSpanKind int

const (
	// SpanPlain is whitespace, parentheses or anything unclassified.
	SpanPlain FilterSpanKind = iota
	// SpanField is a column name on the left of = / != or a time keyword.
	SpanField
	// SpanCond is an operator or keyword: = != AND OR NOT before after between.
	SpanCond
	// SpanParam is a value: a bareword, "quoted" string or /regex/.
	SpanParam
)

// FilterSpan is a byte range [Start,End) of the query and the role it plays.
type FilterSpan struct {
	Start, End int
	Kind       FilterSpanKind
}

// FilterSpans tiles s with spans (no gaps, so concatenating the spans' text
// reproduces s) tagged by role, for the filter bar's colouring. A word directly
// before = / != or a time keyword is a field; the comparison operators and the
// boolean/time keywords are conditions; every other value is a parameter;
// whitespace and parentheses are plain.
func FilterSpans(s string) []FilterSpan {
	var spans []FilterSpan
	prev := 0
	flushPlain := func(upto int) {
		if upto > prev {
			spans = append(spans, FilterSpan{prev, upto, SpanPlain})
		}
	}
	emit := func(start, end int, k FilterSpanKind) {
		flushPlain(start)
		spans = append(spans, FilterSpan{start, end, k})
		prev = end
	}

	n := len(s)
	i := 0
	for i < n {
		switch s[i] {
		case ' ', '\t', '\n', '\r', '(', ')':
			i++ // stays part of the plain gap
			continue
		}

		// A comparison operator standing on its own.
		if s[i] == '=' || (s[i] == '!' && i+1 < n && s[i+1] == '=') {
			end := i + 1
			if s[i] == '!' {
				end = i + 2
			}
			emit(i, end, SpanCond)
			i = end
			continue
		}

		val, kind, next, err := readAtom(s, i)
		if kind == atomNone {
			i++ // nothing readable here; advance so we always make progress
			continue
		}
		start := i
		if err != nil {
			// Unterminated "quote or /regex/: colour the rest as a value.
			emit(start, next, SpanParam)
			i = next
			continue
		}
		i = next

		if kind == atomBare {
			switch strings.ToUpper(val) {
			case "AND", "OR", "NOT":
				emit(start, i, SpanCond)
				continue
			}
			switch strings.ToLower(val) {
			case "before", "after", "between":
				emit(start, i, SpanCond)
				continue
			}
		}

		// A word (bare or "quoted") immediately followed by = / != is a field.
		if kind == atomBare || kind == atomQuoted {
			j := skipSpaces(s, i)
			if j < n && (s[j] == '=' || (s[j] == '!' && j+1 < n && s[j+1] == '=')) {
				emit(start, i, SpanField)
				continue
			}
			// A word followed by a time keyword is also a field.
			if kw, kwkind, _, _ := readAtom(s, j); kwkind == atomBare {
				switch strings.ToLower(kw) {
				case "before", "after", "between":
					emit(start, i, SpanField)
					continue
				}
			}
		}

		// Anything else is a value.
		emit(start, i, SpanParam)
	}
	flushPlain(n)
	return spans
}
