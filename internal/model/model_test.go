package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func openT(t *testing.T, content string) *Index {
	t.Helper()
	idx, err := Open(writeTemp(t, "t.csv", content), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

func TestIndexBasic(t *testing.T) {
	idx := openT(t, "a,b,c\n1,2,3\n4,5,6\n")
	if got := idx.RowCount(); got != 2 {
		t.Fatalf("RowCount = %d, want 2", got)
	}
	if h := idx.Headers(); strings.Join(h, "|") != "a|b|c" {
		t.Fatalf("headers = %v", h)
	}
	r, err := idx.Row(1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r, "|") != "4|5|6" {
		t.Fatalf("row 1 = %v", r)
	}
}

func TestIndexNoTrailingNewline(t *testing.T) {
	idx := openT(t, "a,b\n1,2\n3,4")
	if idx.RowCount() != 2 {
		t.Fatalf("RowCount = %d, want 2", idx.RowCount())
	}
	r, _ := idx.Row(1)
	if strings.Join(r, "|") != "3|4" {
		t.Fatalf("last row = %v", r)
	}
}

func TestIndexQuotedNewline(t *testing.T) {
	// A quoted field spans two physical lines; it must stay one record.
	idx := openT(t, "id,msg\n1,\"line one\nline two\"\n2,ok\n")
	if idx.RowCount() != 2 {
		t.Fatalf("RowCount = %d, want 2", idx.RowCount())
	}
	r, _ := idx.Row(0)
	if r[1] != "line one\nline two" {
		t.Fatalf("row0 msg = %q", r[1])
	}
	r, _ = idx.Row(1)
	if r[0] != "2" {
		t.Fatalf("row1 = %v", r)
	}
}

func TestIndexEscapedQuotes(t *testing.T) {
	idx := openT(t, "id,msg\n1,\"he said \"\"hi\"\" today\"\n")
	r, _ := idx.Row(0)
	if r[1] != `he said "hi" today` {
		t.Fatalf("msg = %q", r[1])
	}
}

func TestIndexCRLFAndBOM(t *testing.T) {
	idx := openT(t, "\xEF\xBB\xBFa,b\r\n1,2\r\n3,4\r\n")
	if idx.RowCount() != 2 {
		t.Fatalf("RowCount = %d, want 2", idx.RowCount())
	}
	if idx.Headers()[0] != "a" {
		t.Fatalf("BOM not stripped: header %q", idx.Headers()[0])
	}
	r, _ := idx.Row(0)
	if r[1] != "2" {
		t.Fatalf("row0 = %v (CRLF not trimmed?)", r)
	}
}

func TestDelimiterTSV(t *testing.T) {
	idx := openT(t, "a\tb\tc\n1\t2\t3\n")
	if idx.Delimiter() != '\t' {
		t.Fatalf("delimiter = %q, want tab", idx.Delimiter())
	}
	r, _ := idx.Row(0)
	if strings.Join(r, "|") != "1|2|3" {
		t.Fatalf("row = %v", r)
	}
}

func TestScanOrder(t *testing.T) {
	idx := openT(t, "n\n0\n1\n2\n3\n")
	var seen []string
	idx.Scan(func(i int, rec []string) bool {
		seen = append(seen, rec[0])
		return true
	})
	if strings.Join(seen, ",") != "0,1,2,3" {
		t.Fatalf("scan = %v", seen)
	}
}

func TestFilterAndSort(t *testing.T) {
	idx := openT(t, "host,event\nalpha,login\nbravo,logout\nalpha,logout\ncharlie,login\n")
	s := NewSession(idx.Path())
	v := NewView(idx, s)

	if err := v.Apply(FilterSpec{Query: "alpha", Column: ColAll}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 2 {
		t.Fatalf("filter alpha -> %d rows, want 2", v.Len())
	}

	// Sort the full set by host ascending.
	v.Reset()
	if err := v.Sort([]SortKey{{Col: 0}}); err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for i := 0; i < v.Len(); i++ {
		r, _ := idx.Row(v.Master(i))
		hosts = append(hosts, r[0])
	}
	if strings.Join(hosts, ",") != "alpha,alpha,bravo,charlie" {
		t.Fatalf("sorted hosts = %v", hosts)
	}

	// Descending.
	v.Sort([]SortKey{{Col: 0, Desc: true}})
	r0, _ := idx.Row(v.Master(0))
	if r0[0] != "charlie" {
		t.Fatalf("desc first = %v", r0)
	}
}

func TestNumericSort(t *testing.T) {
	idx := openT(t, "n\n10\n2\n1\n100\n")
	v := NewView(idx, NewSession(idx.Path()))
	v.Sort([]SortKey{{Col: 0}})
	var got []string
	for i := 0; i < v.Len(); i++ {
		r, _ := idx.Row(v.Master(i))
		got = append(got, r[0])
	}
	if strings.Join(got, ",") != "1,2,10,100" {
		t.Fatalf("numeric sort = %v (should not be lexical)", got)
	}
}

func TestRegexFilter(t *testing.T) {
	idx := openT(t, "msg\nfailed login\nsuccess\nfailed logout\n")
	v := NewView(idx, NewSession(idx.Path()))
	if err := v.Apply(FilterSpec{Query: `^failed`, Regexp: true, Column: 0}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 2 {
		t.Fatalf("regex filter -> %d, want 2", v.Len())
	}
}

func TestModesGuardMutations(t *testing.T) {
	idx := openT(t, "a,b\n1,2\n")
	s := NewSession(idx.Path())

	// Read-only: everything blocked.
	if err := s.AddTag(0, "x"); err != ErrReadOnly {
		t.Fatalf("AddTag in RO = %v, want ErrReadOnly", err)
	}
	if err := s.SetComment(0, "hi"); err != ErrReadOnly {
		t.Fatalf("SetComment in RO = %v", err)
	}
	if err := s.SetCell(0, 0, "z"); err != ErrReadOnly {
		t.Fatalf("SetCell in RO = %v", err)
	}

	// Investigator: tags and comments only.
	s.SetMode(Investigator)
	if err := s.AddTag(0, "malware"); err != nil {
		t.Fatalf("AddTag in Investigator: %v", err)
	}
	if err := s.SetComment(0, "suspicious"); err != nil {
		t.Fatalf("SetComment in Investigator: %v", err)
	}
	if err := s.SetCell(0, 0, "z"); err != ErrColumnLocked {
		t.Fatalf("SetCell in Investigator = %v, want ErrColumnLocked", err)
	}

	// World-write: cells too.
	s.SetMode(WorldWrite)
	if err := s.SetCell(0, 0, "EDITED"); err != nil {
		t.Fatalf("SetCell in WorldWrite: %v", err)
	}
	if v, ok := s.CellOverride(0, 0); !ok || v != "EDITED" {
		t.Fatalf("override = %q,%v", v, ok)
	}
	if got := s.Tags(0); len(got) != 1 || got[0] != "malware" {
		t.Fatalf("tags = %v", got)
	}
}

func TestSessionPersistence(t *testing.T) {
	idx := openT(t, "a,b\n1,2\n3,4\n")
	s := NewSession(idx.Path())
	s.SetMode(WorldWrite)
	s.AddTag(0, "beacon")
	s.AddTag(0, "c2")
	s.SetComment(1, "note here")
	s.SetCell(1, 0, "99")
	if !s.Dirty() {
		t.Fatal("expected dirty")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if s.Dirty() {
		t.Fatal("still dirty after save")
	}

	// Reload into a fresh session.
	s2 := NewSession(idx.Path())
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s2.Tags(0); strings.Join(got, ",") != "beacon,c2" {
		t.Fatalf("reloaded tags = %v", got)
	}
	if s2.Comment(1) != "note here" {
		t.Fatalf("reloaded comment = %q", s2.Comment(1))
	}
	if v, ok := s2.CellOverride(1, 0); !ok || v != "99" {
		t.Fatalf("reloaded override = %q,%v", v, ok)
	}
	// KnownTags now leads with the seeded defaults, then user tags in the order
	// they were first applied.
	if kt := s2.KnownTags(); strings.Join(kt, ",") != "Bad,Suspicious,Good,beacon,c2" {
		t.Fatalf("known tags = %v", kt)
	}
}

func TestDefaultTagColors(t *testing.T) {
	s := NewSession("x.csv")
	for _, d := range defaultTagDefs {
		got, ok := s.TagColor(d.Name)
		if !ok || got != d.Color {
			t.Fatalf("default tag %q colour = %q,%v want %q", d.Name, got, ok, d.Color)
		}
	}
	// Bad outranks Good on a row carrying both.
	s.SetMode(Investigator)
	s.AddTag(0, "Good")
	s.AddTag(0, "Bad")
	c, ok := s.RowColor(0)
	if !ok || c != "#E53935" {
		t.Fatalf("row colour = %q,%v, want Bad red", c, ok)
	}
}

func TestCustomTagColorPersists(t *testing.T) {
	idx := openT(t, "a\n1\n2\n")
	s := NewSession(idx.Path())
	s.SetMode(Investigator)
	if err := s.DefineTag("beacon", "#123456"); err != nil {
		t.Fatal(err)
	}
	s.AddTag(0, "beacon")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s2 := NewSession(idx.Path())
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if c, ok := s2.TagColor("beacon"); !ok || c != "#123456" {
		t.Fatalf("reloaded beacon colour = %q,%v", c, ok)
	}
	// Defaults survive a reload too.
	if c, _ := s2.TagColor("Bad"); c != "#E53935" {
		t.Fatalf("reloaded Bad colour = %q", c)
	}
}

func TestDeleteTag(t *testing.T) {
	s := NewSession("x.csv")
	s.SetMode(Investigator)
	s.AddTag(0, "beacon")
	s.AddTag(0, "Bad")
	s.AddTag(1, "beacon")

	if err := s.DeleteTag("beacon"); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}
	// Gone from the palette lookup.
	if _, ok := s.TagColor("beacon"); ok {
		t.Error("beacon still in palette after delete")
	}
	// Gone from every row; the other tag stays.
	if tags := s.Tags(0); len(tags) != 1 || tags[0] != "Bad" {
		t.Errorf("row 0 tags = %v, want [Bad]", tags)
	}
	if tags := s.Tags(1); len(tags) != 0 {
		t.Errorf("row 1 tags = %v, want []", tags)
	}
	// Read-only sessions must refuse it, like other annotation edits.
	s.SetMode(ReadOnly)
	if err := s.DeleteTag("Bad"); err != ErrReadOnly {
		t.Errorf("DeleteTag in RO = %v, want ErrReadOnly", err)
	}
}

func TestColumnCondFilter(t *testing.T) {
	idx := openT(t, "host,event\nalpha,login\nbravo,logout\nalpha,logout\ncharlie,login\n")
	v := NewView(idx, NewSession(idx.Path()))

	// host contains alpha OR bravo (any within one column).
	if err := v.Apply(FilterSpec{Conds: []ColumnCond{
		{Column: 0, Values: []string{"alpha", "bravo"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 3 {
		t.Fatalf("host in {alpha,bravo} -> %d rows, want 3", v.Len())
	}

	// AND across two columns: host=alpha AND event=logout.
	if err := v.Apply(FilterSpec{Conds: []ColumnCond{
		{Column: 0, Values: []string{"alpha"}},
		{Column: 1, Values: []string{"logout"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 1 {
		t.Fatalf("alpha AND logout -> %d rows, want 1", v.Len())
	}

	// OR across two columns: host=charlie OR event=logout.
	if err := v.Apply(FilterSpec{CondsAny: true, Conds: []ColumnCond{
		{Column: 0, Values: []string{"charlie"}},
		{Column: 1, Values: []string{"logout"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 3 { // bravo/logout, alpha/logout, charlie/login
		t.Fatalf("charlie OR logout -> %d rows, want 3", v.Len())
	}
}

func TestColumnCondNeg(t *testing.T) {
	idx := openT(t, "host,event\nalpha,login\nbravo,logout\nalpha,cleared\ncharlie,signin\n")
	v := NewView(idx, NewSession(idx.Path()))

	// event does NOT contain "log" — drops the login and logout rows.
	if err := v.Apply(FilterSpec{Conds: []ColumnCond{
		{Column: 1, Values: []string{"log"}, Neg: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 2 {
		t.Fatalf("event NOT containing log -> %d rows, want 2", v.Len())
	}

	// Excluding via regex works too: event does not match /log/.
	if err := v.Apply(FilterSpec{Conds: []ColumnCond{
		{Column: 1, Values: []string{"log"}, Regexp: true, Neg: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 2 {
		t.Fatalf("event NOT matching /log/ -> %d rows, want 2", v.Len())
	}

	// A negated condition combines with a positive one under AND: host=alpha AND
	// event does not contain "login" -> only the alpha/cleared row (master 2).
	if err := v.Apply(FilterSpec{Conds: []ColumnCond{
		{Column: 0, Values: []string{"alpha"}},
		{Column: 1, Values: []string{"login"}, Neg: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 1 || v.Master(0) != 2 {
		t.Fatalf("alpha AND NOT login -> len=%d master0=%d, want 1 row (master 2)", v.Len(), v.Master(0))
	}
}

// TestColumnEmptiness covers the mechanism the header right-click "empty" /
// "not empty" menu items rely on: a regexp `\S` condition matches non-empty
// cells, and negating it keeps the empty (and whitespace-only) ones.
func TestColumnEmptiness(t *testing.T) {
	idx := openT(t, "host,note\nalpha,hit\nbravo,\ncharlie,   \ndelta,ok\n")
	v := NewView(idx, NewSession(idx.Path()))

	// Not empty: note has a non-space char -> alpha and delta only.
	if err := v.Apply(FilterSpec{Conds: []ColumnCond{
		{Column: 1, Values: []string{`\S`}, Regexp: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 2 || v.Master(0) != 0 || v.Master(1) != 3 {
		t.Fatalf("note not empty -> len=%d masters=%d,%d, want 2 (0,3)", v.Len(), v.Master(0), v.Master(1))
	}

	// Empty: negation keeps the blank and whitespace-only rows -> bravo, charlie.
	if err := v.Apply(FilterSpec{Conds: []ColumnCond{
		{Column: 1, Values: []string{`\S`}, Regexp: true, Neg: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 2 || v.Master(0) != 1 || v.Master(1) != 2 {
		t.Fatalf("note empty -> len=%d masters=%d,%d, want 2 (1,2)", v.Len(), v.Master(0), v.Master(1))
	}
}

func TestColumnCondAllValues(t *testing.T) {
	idx := openT(t, "msg\nfailed login attempt\nfailed logout\nsuccess login\n")
	v := NewView(idx, NewSession(idx.Path()))
	// A single column must contain BOTH words.
	if err := v.Apply(FilterSpec{Conds: []ColumnCond{
		{Column: 0, Values: []string{"failed", "login"}, All: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 1 {
		t.Fatalf("failed AND login -> %d rows, want 1", v.Len())
	}
}

func TestFilterTaggedOnly(t *testing.T) {
	idx := openT(t, "a\n1\n2\n3\n")
	s := NewSession(idx.Path())
	s.SetMode(Investigator)
	s.AddTag(1, "keep")
	v := NewView(idx, s)
	if err := v.Apply(FilterSpec{TaggedOnly: true}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 1 || v.Master(0) != 1 {
		t.Fatalf("tagged-only view len=%d master0=%d", v.Len(), v.Master(0))
	}
}

func TestFilterTags(t *testing.T) {
	idx := openT(t, "a\n1\n2\n3\n4\n")
	s := NewSession(idx.Path())
	s.SetMode(Investigator)
	s.AddTag(0, "bad")
	s.AddTag(1, "suspicious")
	s.AddTag(2, "good")
	v := NewView(idx, s)
	if err := v.Apply(FilterSpec{Tags: []string{"bad", "suspicious"}}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 2 || v.Master(0) != 0 || v.Master(1) != 1 {
		t.Fatalf("tags filter len=%d masters=%v", v.Len(), []int{v.Master(0), v.Master(1)})
	}
	// An empty Tags slice must not filter anything.
	if err := v.Apply(FilterSpec{Tags: []string{}}); err != nil {
		t.Fatal(err)
	}
	if v.Len() != 4 {
		t.Fatalf("empty tags filter len=%d, want 4", v.Len())
	}
}

func TestExport(t *testing.T) {
	idx := openT(t, "host,event\nalpha,login\nbravo,logout\n")
	s := NewSession(idx.Path())
	s.SetMode(WorldWrite)
	s.AddTag(0, "flag")
	s.SetComment(0, "look here")
	s.SetCell(1, 1, "LOGOUT")
	v := NewView(idx, s)

	dest := filepath.Join(t.TempDir(), "out.csv")
	if err := Export(v, s, dest); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dest)
	got := string(data)
	want := "host,event,Tags,Comment\nalpha,login,flag,look here\nbravo,LOGOUT,,\n"
	if got != want {
		t.Fatalf("export =\n%q\nwant\n%q", got, want)
	}
}

// A non-blank timeline comment heads the export with a two-cell row before the
// header; a blank comment adds no row.
func TestExportWithComment(t *testing.T) {
	idx := openT(t, "host,event\nalpha,login\n")
	s := NewSession(idx.Path())
	v := NewView(idx, s)

	dest := filepath.Join(t.TempDir(), "out.csv")
	if err := ExportOmittingWithComment(v, s, dest, nil, "saw psexec at 03:14"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dest)
	got := string(data)
	want := "Timeline comments:,saw psexec at 03:14\nhost,event,Tags,Comment\nalpha,login,,\n"
	if got != want {
		t.Fatalf("export =\n%q\nwant\n%q", got, want)
	}

	// A blank comment writes no prelude row.
	dest2 := filepath.Join(t.TempDir(), "out2.csv")
	if err := ExportOmittingWithComment(v, s, dest2, nil, "   "); err != nil {
		t.Fatal(err)
	}
	data2, _ := os.ReadFile(dest2)
	if got := string(data2); got != "host,event,Tags,Comment\nalpha,login,,\n" {
		t.Fatalf("blank-comment export =\n%q", got)
	}
}

// TestExportAligned covers the custom-align export: output columns pull from one
// or more view columns; a single source writes raw, a merge writes "title: val;"
// per source. Virtual Tags/Comment columns resolve like the grid.
func TestExportAligned(t *testing.T) {
	idx := openT(t, "host,user,event\nalpha,root,login\nbravo,guest,logout\n")
	s := NewSession(idx.Path())
	s.SetMode(Investigator)
	s.AddTag(0, "bad")
	s.SetComment(0, "look")
	v := NewView(idx, s)

	cols := []AlignedColumn{
		{Name: "Who", Sources: []AlignedSource{{Ref: 0, Title: "host"}, {Ref: 1, Title: "user"}}},
		{Name: "What", Sources: []AlignedSource{{Ref: 2, Title: "event"}}},
		{Name: "Notes", Sources: []AlignedSource{{Ref: ColTags, Title: "Tags"}, {Ref: ColComment, Title: "Comment"}}},
	}
	dest := filepath.Join(t.TempDir(), "aligned.csv")
	if err := ExportAligned(v, s, dest, "", cols); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	want := "Who,What,Notes\n" +
		"host: alpha; user: root;,login,Tags: bad; Comment: look;\n" +
		"host: bravo; user: guest;,logout,Tags: ; Comment: ;\n"
	if string(got) != want {
		t.Fatalf("aligned export =\n%q\nwant\n%q", got, want)
	}
}

// When a source CSV already has Tags/Comment columns that were adopted as the
// session's annotations, ExportOmitting drops them so the appended Tags/Comment
// columns do not duplicate the source ones.
func TestExportOmitting(t *testing.T) {
	idx := openT(t, "host,Tags,Comment,event\nalpha,old-tag,old note,login\nbravo,,,logout\n")
	s := NewSession(idx.Path())
	ac := DetectAnnotationColumns(idx.Headers()) // Tag=1, Comment=2
	if err := s.SeedFromColumns(idx, ac); err != nil {
		t.Fatal(err)
	}
	s.SetMode(Investigator)
	s.AddTag(1, "new-tag")
	v := NewView(idx, s)

	dest := filepath.Join(t.TempDir(), "out.csv")
	omit := map[int]bool{ac.Tag: true, ac.Comment: true}
	if err := ExportOmitting(v, s, dest, omit); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dest)
	got := string(data)
	// Source Tags/Comment columns are gone; the appended ones carry the seeded
	// values (row 0) and the new tag (row 1).
	want := "host,event,Tags,Comment\nalpha,login,old-tag,old note\nbravo,logout,new-tag,\n"
	if got != want {
		t.Fatalf("export =\n%q\nwant\n%q", got, want)
	}
}
