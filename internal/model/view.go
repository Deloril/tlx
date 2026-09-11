package model

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ColumnRef addresses a column in the view. Non-negative values are data
// columns (indices into a record). Negative values are virtual columns and
// selectors.
type ColumnRef int

const (
	ColTags    ColumnRef = -1   // the virtual Tags column
	ColComment ColumnRef = -2   // the virtual Comment column
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

// FilterSpec describes an absolute filter over the full row set.
type FilterSpec struct {
	Query      string    // text to match; empty means match all
	Regexp     bool      // treat Query as a regular expression
	Cased      bool      // case-sensitive matching
	Column     ColumnRef // ColAll, ColNone, a data column, or a virtual column
	TaggedOnly bool      // keep only rows that carry at least one tag
	Tag        string    // if set, keep only rows carrying this exact tag
}

// Empty reports whether the spec would keep every row.
func (f FilterSpec) Empty() bool {
	return f.Query == "" && !f.TaggedOnly && f.Tag == ""
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

func (v *View) refilter() error {
	spec := v.filter
	if spec.Empty() {
		v.rows = v.rows[:0]
		for i := 0; i < v.idx.RowCount(); i++ {
			v.rows = append(v.rows, i)
		}
		return nil
	}

	var re *regexp.Regexp
	needle := spec.Query
	if spec.Query != "" {
		if spec.Regexp {
			flags := ""
			if !spec.Cased {
				flags = "(?i)"
			}
			r, err := regexp.Compile(flags + spec.Query)
			if err != nil {
				return err
			}
			re = r
		} else if !spec.Cased {
			needle = strings.ToLower(spec.Query)
		}
	}

	matched := v.rows[:0]
	err := v.idx.Scan(func(i int, rec []string) bool {
		if v.keep(i, rec, spec, re, needle) {
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

func (v *View) keep(row int, rec []string, spec FilterSpec, re *regexp.Regexp, needle string) bool {
	if spec.TaggedOnly && len(v.ov.Tags(row)) == 0 {
		return false
	}
	if spec.Tag != "" && !hasTag(v.ov.Tags(row), spec.Tag) {
		return false
	}
	if spec.Query == "" {
		return true
	}
	switch spec.Column {
	case ColNone:
		return true
	case ColAll:
		if v.textMatch(v.cell(row, rec, ColTags), re, needle, spec) {
			return true
		}
		if v.textMatch(v.cell(row, rec, ColComment), re, needle, spec) {
			return true
		}
		for c := 0; c < len(rec); c++ {
			if v.textMatch(v.cell(row, rec, ColumnRef(c)), re, needle, spec) {
				return true
			}
		}
		return false
	default:
		return v.textMatch(v.cell(row, rec, spec.Column), re, needle, spec)
	}
}

func (v *View) textMatch(s string, re *regexp.Regexp, needle string, spec FilterSpec) bool {
	if re != nil {
		return re.MatchString(s)
	}
	if spec.Cased {
		return strings.Contains(s, needle)
	}
	return strings.Contains(strings.ToLower(s), needle)
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
