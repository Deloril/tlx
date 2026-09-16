package model

import "testing"

// kindString names a span kind for readable failures.
func kindString(k FilterSpanKind) string {
	switch k {
	case SpanField:
		return "field"
	case SpanCond:
		return "cond"
	case SpanParam:
		return "param"
	default:
		return "plain"
	}
}

// spanText returns "text:kind" for each non-plain span, so tests can assert the
// interesting classifications without spelling out every whitespace gap.
func nonPlain(s string) []string {
	var out []string
	for _, sp := range FilterSpans(s) {
		if sp.Kind == SpanPlain {
			continue
		}
		out = append(out, s[sp.Start:sp.End]+":"+kindString(sp.Kind))
	}
	return out
}

func TestFilterSpansTiling(t *testing.T) {
	// The spans must cover the whole string in order with no gaps or overlaps.
	for _, s := range []string{
		"", "   ", "Summary=svchost AND (tag=bad OR tag=sus)",
		`Host=/^dc-\d+$/ AND NOT tag=benign`,
		`Time between 2021-01-01 and 2021-02-01`,
		`"Process Name"=cmd.exe`,
		`unterminated "quote`,
		`bad /regex`,
	} {
		spans := FilterSpans(s)
		pos := 0
		var rebuilt string
		for _, sp := range spans {
			if sp.Start != pos {
				t.Fatalf("%q: gap/overlap at %d, span starts %d", s, pos, sp.Start)
			}
			if sp.End < sp.Start || sp.End > len(s) {
				t.Fatalf("%q: span %v out of range", s, sp)
			}
			rebuilt += s[sp.Start:sp.End]
			pos = sp.End
		}
		if pos != len(s) {
			t.Fatalf("%q: spans end at %d, want %d", s, pos, len(s))
		}
		if rebuilt != s {
			t.Fatalf("%q: rebuilt %q", s, rebuilt)
		}
	}
}

func TestFilterSpansClassification(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Summary=svchost", []string{"Summary:field", "=:cond", "svchost:param"}},
		{"tag=bad OR tag=sus", []string{
			"tag:field", "=:cond", "bad:param",
			"OR:cond",
			"tag:field", "=:cond", "sus:param",
		}},
		{"NOT tag=x", []string{"NOT:cond", "tag:field", "=:cond", "x:param"}},
		{"host!=dc1", []string{"host:field", "!=:cond", "dc1:param"}},
		{"Time between a and b", []string{
			"Time:field", "between:cond", "a:param", "and:cond", "b:param",
		}},
		{"Time before 2021-01-01", []string{"Time:field", "before:cond", "2021-01-01:param"}},
		{`"Process Name"=cmd`, []string{`"Process Name":field`, "=:cond", "cmd:param"}},
		{"psexec", []string{"psexec:param"}}, // a bare value matches any column
		{"/regex/", []string{"/regex/:param"}},
	}
	for _, c := range cases {
		got := nonPlain(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("%q: got %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%q: span %d = %q, want %q (got %v)", c.in, i, got[i], c.want[i], got)
			}
		}
	}
}
