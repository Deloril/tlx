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
	if kt := s2.KnownTags(); strings.Join(kt, ",") != "beacon,c2" {
		t.Fatalf("known tags = %v", kt)
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
