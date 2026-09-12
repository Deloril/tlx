package model

import "testing"

// testOverlay is a minimal Overlay for exercising the query evaluator.
type testOverlay struct {
	tags     map[int][]string
	comments map[int]string
}

func (o testOverlay) Tags(row int) []string {
	if o.tags == nil {
		return nil
	}
	return o.tags[row]
}
func (o testOverlay) Comment(row int) string {
	if o.comments == nil {
		return ""
	}
	return o.comments[row]
}
func (o testOverlay) CellOverride(row, col int) (string, bool) { return "", false }

// queryView builds a view over an in-memory index with the given overlay.
func queryView(t *testing.T, headers []string, records [][]string, ov Overlay) *View {
	t.Helper()
	idx := NewMemoryIndex(headers, records)
	return NewView(idx, ov)
}

// visible returns the master row indices kept by a query.
func visible(t *testing.T, v *View, spec FilterSpec) []int {
	t.Helper()
	if err := v.Apply(spec); err != nil {
		t.Fatalf("apply %+v: %v", spec, err)
	}
	out := make([]int, v.Len())
	for i := range out {
		out[i] = v.Master(i)
	}
	return out
}

func eq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestQueryFieldEquals(t *testing.T) {
	v := queryView(t,
		[]string{"Summary", "Host"},
		[][]string{
			{"derp happened", "ws1"},
			{"all quiet", "ws2"},
			{"more DERP", "ws3"},
		}, testOverlay{})

	got := visible(t, v, FilterSpec{Expr: "Summary=derp"})
	if !eq(got, []int{0, 2}) { // case-insensitive by default
		t.Errorf("Summary=derp -> %v, want [0 2]", got)
	}
}

func TestQueryCaseSensitive(t *testing.T) {
	v := queryView(t,
		[]string{"Summary"},
		[][]string{{"derp"}, {"DERP"}}, testOverlay{})

	got := visible(t, v, FilterSpec{Expr: "Summary=derp", Cased: true})
	if !eq(got, []int{0}) {
		t.Errorf("cased Summary=derp -> %v, want [0]", got)
	}
}

func TestQueryAndOrParens(t *testing.T) {
	v := queryView(t,
		[]string{"Summary"},
		[][]string{
			{"derp"},      // 0: tag bad
			{"derp"},      // 1: tag suspicious
			{"derp"},      // 2: tag benign
			{"something"}, // 3: tag bad
		}, testOverlay{tags: map[int][]string{
			0: {"bad"}, 1: {"suspicious"}, 2: {"benign"}, 3: {"bad"},
		}})

	got := visible(t, v, FilterSpec{Expr: "Summary=derp AND (tag=bad OR tag=suspicious)"})
	if !eq(got, []int{0, 1}) {
		t.Errorf("got %v, want [0 1]", got)
	}
}

func TestQueryRegexValue(t *testing.T) {
	v := queryView(t,
		[]string{"Host"},
		[][]string{
			{"dc-01"},
			{"ws-99"},
			{"dc-7"},
		}, testOverlay{})

	got := visible(t, v, FilterSpec{Expr: `Host=/^dc-\d+$/`})
	if !eq(got, []int{0, 2}) {
		t.Errorf("regex host -> %v, want [0 2]", got)
	}
}

func TestQueryBareTermAnyColumn(t *testing.T) {
	v := queryView(t,
		[]string{"A", "B"},
		[][]string{
			{"foo", "bar"},
			{"baz", "qux"},
		}, testOverlay{})

	got := visible(t, v, FilterSpec{Expr: "bar"})
	if !eq(got, []int{0}) {
		t.Errorf("bare term -> %v, want [0]", got)
	}
	got = visible(t, v, FilterSpec{Expr: "/^q/"})
	if !eq(got, []int{1}) {
		t.Errorf("bare regex -> %v, want [1]", got)
	}
}

func TestQueryNotAndNotEquals(t *testing.T) {
	v := queryView(t,
		[]string{"Summary"},
		[][]string{{"derp"}, {"quiet"}, {"derp again"}}, testOverlay{})

	got := visible(t, v, FilterSpec{Expr: "NOT Summary=derp"})
	if !eq(got, []int{1}) {
		t.Errorf("NOT -> %v, want [1]", got)
	}
	got = visible(t, v, FilterSpec{Expr: "Summary!=derp"})
	if !eq(got, []int{1}) {
		t.Errorf("!= -> %v, want [1]", got)
	}
}

func TestQueryQuotedValueWithSpaces(t *testing.T) {
	v := queryView(t,
		[]string{"Summary"},
		[][]string{{"a b c"}, {"abc"}}, testOverlay{})

	got := visible(t, v, FilterSpec{Expr: `Summary="a b"`})
	if !eq(got, []int{0}) {
		t.Errorf("quoted -> %v, want [0]", got)
	}
}

func TestQueryTagAndComment(t *testing.T) {
	v := queryView(t,
		[]string{"Summary"},
		[][]string{{"x"}, {"y"}, {"z"}},
		testOverlay{
			tags:     map[int][]string{0: {"bad"}, 1: {"good"}},
			comments: map[int]string{2: "needs review"},
		})

	got := visible(t, v, FilterSpec{Expr: "tag=bad OR comment=review"})
	if !eq(got, []int{0, 2}) {
		t.Errorf("tag/comment -> %v, want [0 2]", got)
	}
}

func TestQueryPrecedence(t *testing.T) {
	// AND binds tighter than OR: A OR B AND C == A OR (B AND C).
	v := queryView(t,
		[]string{"S"},
		[][]string{
			{"a"},   // 0
			{"b c"}, // 1
			{"b"},   // 2
			{"c"},   // 3
		}, testOverlay{})

	got := visible(t, v, FilterSpec{Expr: "S=a OR S=b AND S=c"})
	if !eq(got, []int{0, 1}) {
		t.Errorf("precedence -> %v, want [0 1]", got)
	}
}

func TestColFilters(t *testing.T) {
	v := queryView(t,
		[]string{"Host", "Msg"},
		[][]string{
			{"ws1", "logon"},
			{"ws2", "logon"},
			{"ws1", "logoff"},
		}, testOverlay{})

	// Two column boxes AND together regardless of anything else.
	got := visible(t, v, FilterSpec{ColFilters: map[ColumnRef]string{
		0: "ws1", 1: "logon",
	}})
	if !eq(got, []int{0}) {
		t.Errorf("col filters -> %v, want [0]", got)
	}

	// Composes with the query expression (also AND).
	got = visible(t, v, FilterSpec{
		Expr:       "Msg=logon",
		ColFilters: map[ColumnRef]string{0: "ws2"},
	})
	if !eq(got, []int{1}) {
		t.Errorf("expr + col filter -> %v, want [1]", got)
	}

	// Case sensitivity flows through.
	got = visible(t, v, FilterSpec{Cased: true, ColFilters: map[ColumnRef]string{0: "WS1"}})
	if len(got) != 0 {
		t.Errorf("cased col filter -> %v, want []", got)
	}
}

func TestQueryErrors(t *testing.T) {
	v := queryView(t, []string{"S"}, [][]string{{"x"}}, testOverlay{})
	cases := []string{
		"S=x AND",         // trailing operator
		"(S=x",            // unbalanced paren
		"S=x)",            // extra paren
		"Nope=x",          // unknown field
		"S=/[/",           // bad regex
		`S="unterminated`, // unterminated quote
		"S=x S=y",         // two terms, no operator
	}
	for _, q := range cases {
		if err := v.Apply(FilterSpec{Expr: q}); err == nil {
			t.Errorf("query %q: expected error, got none (kept %d rows)", q, v.Len())
		}
	}
}
