package model

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ColumnRef addresses a column in the view. Non-negative values are data
// columns (indices into a record). Negative values are virtual columns and
// selectors.
type ColumnRef int

const (
	ColTags    ColumnRef = -1   // the virtual Tags column
	ColComment ColumnRef = -2   // the virtual Comment column
	ColRowNum  ColumnRef = -3   // the original CSV row number (display only)
	ColAll     ColumnRef = -100 // match against every column
	ColNone    ColumnRef = -101 // no column (disables text matching)
)

// Overlay supplies the per-row data that lives outside the CSV: tags, comments
// and cell edits. Session implements it. Filtering and sorting see edited
// values and virtual columns through this.
type Overlay interface {
	Tags(row int) []string
	Comment(row int) string
	CellOverride(row, col int) (string, bool)
}

// SortKey is one level of an ordering.
type SortKey struct {
	Col  ColumnRef
	Desc bool
}

// ColumnCond is one per-column condition: a set of values matched against a
// single column (or ColAll). Values combine by OR unless All is set, in which
// case the row must match every value. Conditions themselves are combined by
// FilterSpec.CondsAny.
type ColumnCond struct {
	Column ColumnRef // ColAll or a specific data/virtual column
	Values []string  // one or more values to match
	Regexp bool      // treat each value as a regular expression
	Cased  bool      // case-sensitive matching
	All    bool      // require every value (AND); default is any (OR)
	Neg    bool      // invert: keep rows this condition would otherwise reject
}

// FilterSpec describes an absolute filter over the full row set. The free-text
// Query (from the search box), the tag filters and the per-column conditions
// all narrow the result: a row is kept only if it passes every part that is set.
type FilterSpec struct {
	Query      string    // text to match; empty means match all
	Regexp     bool      // treat Query as a regular expression
	Cased      bool      // case-sensitive matching (applies to Query and Expr)
	Column     ColumnRef // ColAll, ColNone, a data column, or a virtual column
	TaggedOnly bool      // keep only rows that carry at least one tag
	Tag        string    // if set, keep only rows carrying this exact tag
	Tags       []string  // if non-empty, keep rows carrying any of these exact tags (OR)

	// Expr is a boolean query expression (see query.go): field comparisons
	// joined by AND/OR/NOT with parentheses. Empty means no expression filter.
	Expr string

	Conds    []ColumnCond // per-column conditions
	CondsAny bool         // true = OR across Conds; false (default) = AND

	// ColFilters are the per-column quick filters from the boxes under each
	// header: a substring each column must contain. They always narrow (AND),
	// independent of CondsAny, and honour Cased. Empty values are ignored.
	ColFilters map[ColumnRef]string
}

// Empty reports whether the spec would keep every row.
func (f FilterSpec) Empty() bool {
	return f.Query == "" && f.Expr == "" && !f.TaggedOnly && f.Tag == "" &&
		len(f.Tags) == 0 && len(f.Conds) == 0 && !hasColFilter(f.ColFilters)
}

func hasColFilter(m map[ColumnRef]string) bool {
	for _, v := range m {
		if v != "" {
			return true
		}
	}
	return false
}

// View is an ordered subset of an Index's rows after filtering and sorting. It
// stores master row indices; the GUI addresses rows by view position.
type View struct {
	idx      *Index
	ov       Overlay
	rows     []int
	filter   FilterSpec
	sortKeys []SortKey
}

// NewView returns an unfiltered, unsorted view of every row.
func NewView(idx *Index, ov Overlay) *View {
	v := &View{idx: idx, ov: ov}
	v.rows = make([]int, idx.RowCount())
	for i := range v.rows {
		v.rows[i] = i
	}
	return v
}

// Len is the number of rows currently visible.
func (v *View) Len() int { return len(v.rows) }

// Master maps a view position to a master row index.
func (v *View) Master(viewRow int) int { return v.rows[viewRow] }

// Filter returns the current filter spec.
func (v *View) Filter() FilterSpec { return v.filter }

// SortKeys returns the current ordering.
func (v *View) SortKeys() []SortKey { return v.sortKeys }

// Reset clears filtering and sorting.
func (v *View) Reset() {
	v.filter = FilterSpec{}
	v.sortKeys = nil
	v.rows = v.rows[:0]
	for i := 0; i < v.idx.RowCount(); i++ {
		v.rows = append(v.rows, i)
	}
}

// Apply sets the filter and re-derives the visible rows from the full set, then
// re-applies the current sort. A nil-Empty filter selects everything.
func (v *View) Apply(spec FilterSpec) error {
	v.filter = spec
	if err := v.refilter(); err != nil {
		return err
	}
	return v.resort()
}

// Sort sets the ordering and reorders the visible rows.
func (v *View) Sort(keys []SortKey) error {
	v.sortKeys = keys
	return v.resort()
}

// matcher is one compiled value test: a regexp, a substring needle, or the
// "non-empty" test used by a lone "*" column filter.
type matcher struct {
	re       *regexp.Regexp
	needle   string // lowercased when !cased
	cased    bool
	nonEmpty bool // match any non-blank cell (the "*" column filter)
}

// colFilterMatcher compiles a per-column quick-filter value. A lone "*" is a
// special token meaning "this column is non-empty", not a literal asterisk.
func colFilterMatcher(value string, cased bool) (matcher, error) {
	if value == "*" {
		return matcher{nonEmpty: true}, nil
	}
	return compileMatcher(value, false, cased)
}

func compileMatcher(value string, useRegexp, cased bool) (matcher, error) {
	if useRegexp {
		flags := ""
		if !cased {
			flags = "(?i)"
		}
		re, err := regexp.Compile(flags + value)
		if err != nil {
			return matcher{}, err
		}
		return matcher{re: re}, nil
	}
	if cased {
		return matcher{needle: value, cased: true}, nil
	}
	return matcher{needle: strings.ToLower(value)}, nil
}

func (m matcher) match(s string) bool {
	if m.nonEmpty {
		return strings.TrimSpace(s) != ""
	}
	if m.re != nil {
		return m.re.MatchString(s)
	}
	if m.cased {
		return strings.Contains(s, m.needle)
	}
	return strings.Contains(strings.ToLower(s), m.needle)
}

// compiledCond is a ColumnCond with its values compiled once for the scan.
type compiledCond struct {
	column ColumnRef
	all    bool
	neg    bool // invert the condition's result
	vals   []matcher
}

// compiledFilter is the whole spec reduced to what keep needs per row.
type compiledFilter struct {
	taggedOnly bool
	tag        string
	tags       []string // keep rows carrying any of these exact tags (OR)
	hasQuery   bool
	query      matcher
	queryCol   ColumnRef
	conds      []compiledCond
	condsAny   bool
	expr       rowPred        // compiled boolean query, nil when unset
	colFilters []compiledCond // per-column quick filters, always ANDed
}

func (v *View) refilter() error {
	spec := v.filter
	if spec.Empty() {
		v.rows = v.rows[:0]
		for i := 0; i < v.idx.RowCount(); i++ {
			v.rows = append(v.rows, i)
		}
		return nil
	}

	cf := compiledFilter{
		taggedOnly: spec.TaggedOnly,
		tag:        spec.Tag,
		queryCol:   spec.Column,
		condsAny:   spec.CondsAny,
	}
	for _, t := range spec.Tags {
		if t != "" {
			cf.tags = append(cf.tags, t)
		}
	}
	if spec.Query != "" {
		m, err := compileMatcher(spec.Query, spec.Regexp, spec.Cased)
		if err != nil {
			return err
		}
		cf.query = m
		cf.hasQuery = true
	}
	if spec.Expr != "" {
		ast, err := ParseQuery(spec.Expr)
		if err != nil {
			return err
		}
		if ast != nil {
			pred, err := v.compileExpr(ast, spec.Cased, time.Now())
			if err != nil {
				return err
			}
			cf.expr = pred
		}
	}
	for _, c := range spec.Conds {
		cc := compiledCond{column: c.Column, all: c.All, neg: c.Neg}
		for _, val := range c.Values {
			if val == "" {
				continue
			}
			m, err := compileMatcher(val, c.Regexp, c.Cased)
			if err != nil {
				return err
			}
			cc.vals = append(cc.vals, m)
		}
		if len(cc.vals) > 0 {
			cf.conds = append(cf.conds, cc)
		}
	}
	for ref, val := range spec.ColFilters {
		if val == "" {
			continue
		}
		m, err := colFilterMatcher(val, spec.Cased)
		if err != nil {
			return err
		}
		cf.colFilters = append(cf.colFilters, compiledCond{column: ref, vals: []matcher{m}})
	}

	matched := v.rows[:0]
	err := v.idx.Scan(func(i int, rec []string) bool {
		if v.keep(i, rec, cf) {
			matched = append(matched, i)
		}
		return true
	})
	if err != nil {
		return err
	}
	v.rows = matched
	return nil
}

func (v *View) keep(row int, rec []string, cf compiledFilter) bool {
	if cf.taggedOnly && len(v.ov.Tags(row)) == 0 {
		return false
	}
	if cf.tag != "" && !hasTag(v.ov.Tags(row), cf.tag) {
		return false
	}
	if len(cf.tags) > 0 && !hasAnyTag(v.ov.Tags(row), cf.tags) {
		return false
	}
	if cf.hasQuery && cf.queryCol != ColNone {
		if !v.columnMatch(row, rec, cf.queryCol, cf.query) {
			return false
		}
	}
	if len(cf.conds) > 0 && !v.condsMatch(row, rec, cf) {
		return false
	}
	if cf.expr != nil && !cf.expr(row, rec) {
		return false
	}
	for _, c := range cf.colFilters {
		if !v.columnMatch(row, rec, c.column, c.vals[0]) {
			return false
		}
	}
	return true
}

// condsMatch evaluates the per-column condition group with the configured
// across-condition combinator.
func (v *View) condsMatch(row int, rec []string, cf compiledFilter) bool {
	for _, c := range cf.conds {
		ok := v.condMatch(row, rec, c)
		if cf.condsAny && ok {
			return true
		}
		if !cf.condsAny && !ok {
			return false
		}
	}
	// AND: reaching here means all passed. OR: none passed.
	return !cf.condsAny
}

func (v *View) condMatch(row int, rec []string, c compiledCond) bool {
	hit := v.condHit(row, rec, c)
	if c.neg {
		return !hit
	}
	return hit
}

// condHit is the un-negated test: the values combine by OR, or by AND when
// c.all is set.
func (v *View) condHit(row int, rec []string, c compiledCond) bool {
	for _, m := range c.vals {
		hit := v.columnMatch(row, rec, c.column, m)
		if c.all && !hit {
			return false
		}
		if !c.all && hit {
			return true
		}
	}
	return c.all
}

// columnMatch reports whether the matcher hits the given column, or any column
// when ref is ColAll.
func (v *View) columnMatch(row int, rec []string, ref ColumnRef, m matcher) bool {
	if ref != ColAll {
		return m.match(v.cell(row, rec, ref))
	}
	if m.match(v.cell(row, rec, ColTags)) || m.match(v.cell(row, rec, ColComment)) {
		return true
	}
	for c := 0; c < len(rec); c++ {
		if m.match(v.cell(row, rec, ColumnRef(c))) {
			return true
		}
	}
	return false
}

func (v *View) resort() error {
	if len(v.sortKeys) == 0 {
		return nil
	}
	// Collect sort keys for the visible rows in one sequential pass.
	pos := make(map[int]int, len(v.rows))
	for p, m := range v.rows {
		pos[m] = p
	}
	keyvals := make([][]string, len(v.rows))
	err := v.idx.Scan(func(i int, rec []string) bool {
		if p, ok := pos[i]; ok {
			ks := make([]string, len(v.sortKeys))
			for k, sk := range v.sortKeys {
				ks[k] = v.cell(i, rec, sk.Col)
			}
			keyvals[p] = ks
		}
		return true
	})
	if err != nil {
		return err
	}
	order := make([]int, len(v.rows))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ka, kb := keyvals[order[a]], keyvals[order[b]]
		for k, sk := range v.sortKeys {
			c := compareCell(ka[k], kb[k])
			if c == 0 {
				continue
			}
			if sk.Desc {
				return c > 0
			}
			return c < 0
		}
		return false
	})
	reordered := make([]int, len(v.rows))
	for i, o := range order {
		reordered[i] = v.rows[o]
	}
	v.rows = reordered
	return nil
}

func (v *View) cell(masterRow int, rec []string, ref ColumnRef) string {
	switch ref {
	case ColRowNum:
		return strconv.Itoa(masterRow + 1)
	case ColTags:
		return strings.Join(v.ov.Tags(masterRow), ", ")
	case ColComment:
		return v.ov.Comment(masterRow)
	default:
		c := int(ref)
		if c < 0 || c >= len(rec) {
			return ""
		}
		if val, ok := v.ov.CellOverride(masterRow, c); ok {
			return val
		}
		return rec[c]
	}
}

// compareCell orders two cell values: numeric when both parse as numbers,
// otherwise case-insensitive string order. Empty strings sort last.
func compareCell(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1
	}
	if b == "" {
		return -1
	}
	fa, ea := strconv.ParseFloat(strings.TrimSpace(a), 64)
	fb, eb := strconv.ParseFloat(strings.TrimSpace(b), 64)
	if ea == nil && eb == nil {
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		default:
			return 0
		}
	}
	la, lb := strings.ToLower(a), strings.ToLower(b)
	return strings.Compare(la, lb)
}

func hasTag(tags []string, t string) bool {
	for _, x := range tags {
		if x == t {
			return true
		}
	}
	return false
}

// hasAnyTag reports whether tags contains any of the wanted tags (OR).
func hasAnyTag(tags, wanted []string) bool {
	for _, w := range wanted {
		if hasTag(tags, w) {
			return true
		}
	}
	return false
}
