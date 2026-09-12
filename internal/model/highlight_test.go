package model

import (
	"reflect"
	"testing"
)

func TestHighlighterSpans(t *testing.T) {
	headers := []string{"Host", "Summary"}
	records := [][]string{
		{"ws1", "ran /tmp/evil and /tmp/other"},
	}
	v := queryView(t, headers, records, testOverlay{})
	summary := ColumnRef(1)

	cases := []struct {
		name string
		spec FilterSpec
		ref  ColumnRef
		text string
		want [][2]int
	}{
		{
			name: "regex on named column",
			spec: FilterSpec{Expr: "Summary=/tmp/"},
			ref:  summary,
			text: "ran /tmp/evil and /tmp/other",
			want: [][2]int{{5, 8}, {19, 22}},
		},
		{
			name: "column-scoped term does not highlight other columns",
			spec: FilterSpec{Expr: "Summary=tmp"},
			ref:  ColumnRef(0),
			text: "tmpish host",
			want: nil,
		},
		{
			name: "case-insensitive literal",
			spec: FilterSpec{Expr: "EVIL"},
			ref:  summary,
			text: "ran /tmp/evil and /tmp/other",
			want: [][2]int{{9, 13}},
		},
		{
			name: "negated term is not highlighted",
			spec: FilterSpec{Expr: "NOT tmp"},
			ref:  summary,
			text: "ran /tmp/evil",
			want: nil,
		},
		{
			name: "per-column quick filter",
			spec: FilterSpec{ColFilters: map[ColumnRef]string{summary: "other"}},
			ref:  summary,
			text: "ran /tmp/evil and /tmp/other",
			want: [][2]int{{23, 28}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := v.BuildHighlighter(tc.spec)
			got := h.Spans(tc.ref, tc.text)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Spans = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHighlighterMergesOverlap(t *testing.T) {
	v := queryView(t, []string{"A"}, [][]string{{"aaaa"}}, testOverlay{})
	// Two overlapping literals should merge into one span.
	h := v.BuildHighlighter(FilterSpec{
		Conds: []ColumnCond{{Column: ColAll, Values: []string{"aa", "aaa"}}},
	})
	got := h.Spans(ColumnRef(0), "aaaa")
	want := [][2]int{{0, 4}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged spans = %v, want %v", got, want)
	}
}
