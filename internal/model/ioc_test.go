package model

import "testing"

func iocView(t *testing.T) *View {
	t.Helper()
	return queryView(t,
		[]string{"Timestamp", "Summary", "Host"},
		[][]string{
			{"2024-01-01 00:00:00", "psexec service install", "dc1"},
			{"2024-01-02 00:00:00", "user logon", "ws2"},
			{"2024-01-03 00:00:00", "EVIL.exe dropped", "ws3"},
			{"2024-01-04 00:00:00", "T1059 detected", "dc1"},
		}, testOverlay{})
}

func TestCompileAndScanIOCs(t *testing.T) {
	v := iocView(t)
	list := `evil.exe
/T[0-9]{4}/
` + "`" + `Summary=psexec AND Host=dc1
# a comment, ignored

Timestamp before 2024-01-02`

	set := v.CompileIOCs(list, false)
	if len(set.Errors) != 0 {
		t.Fatalf("unexpected compile errors: %+v", set.Errors)
	}
	if set.Count() != 4 { // plain, regex, backtick filter, time filter
		t.Fatalf("Count() = %d, want 4", set.Count())
	}
	hits, err := v.ScanIOCs(set)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// row0: psexec filter + time-before; row2: evil.exe (case-insensitive);
	// row3: T1059 regex. Scan returns master indices in ascending order.
	if !eq(hits, []int{0, 2, 3}) {
		t.Errorf("hits = %v, want [0 2 3]", hits)
	}
}

func TestIOCErrorsCollected(t *testing.T) {
	v := iocView(t)
	list := `/[/
` + "`" + `Nope=x
good`
	set := v.CompileIOCs(list, false)
	if set.Count() != 1 { // only "good" compiles
		t.Fatalf("Count() = %d, want 1", set.Count())
	}
	if len(set.Errors) != 2 {
		t.Fatalf("Errors = %+v, want 2", set.Errors)
	}
	// Line numbers are 1-based over the raw text.
	if set.Errors[0].Line != 1 || set.Errors[1].Line != 2 {
		t.Errorf("error lines = %d,%d, want 1,2", set.Errors[0].Line, set.Errors[1].Line)
	}
}

// A plain or /regex/ indicator must not match a row on its own tags or comment,
// only on data columns. A backtick line naming tag=/comment= still reaches them.
func TestIOCSkipsAnnotationFields(t *testing.T) {
	ov := testOverlay{
		tags:     map[int][]string{1: {"beacon"}},
		comments: map[int]string{2: "psexec seen here"},
	}
	v := queryView(t,
		[]string{"Timestamp", "Summary", "Host"},
		[][]string{
			{"2024-01-01 00:00:00", "clean", "dc1"},  // row0: nothing
			{"2024-01-02 00:00:00", "clean", "ws2"},  // row1: tag beacon
			{"2024-01-03 00:00:00", "clean", "ws3"},  // row2: comment mentions psexec
			{"2024-01-04 00:00:00", "psexec", "dc1"}, // row3: psexec in a data column
		}, ov)

	// Plain strings that would only hit the tag or comment must find nothing;
	// the same word in a data column (row3) still hits.
	hits, err := v.ScanIOCs(v.CompileIOCs("beacon\npsexec\n", false))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !eq(hits, []int{3}) {
		t.Errorf("plain hits = %v, want [3] (tag/comment skipped)", hits)
	}

	// A backtick line naming the field reaches the annotations again.
	hits, err = v.ScanIOCs(v.CompileIOCs("`tag=beacon\n", false))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !eq(hits, []int{1}) {
		t.Errorf("`tag=beacon hits = %v, want [1]", hits)
	}
}

// When a source column is adopted as the session's tags, a plain indicator must
// skip that column too, not just the virtual Tags field.
func TestIOCSkipsAdoptedColumn(t *testing.T) {
	v := queryView(t,
		[]string{"Summary", "Tags"},
		[][]string{
			{"clean", "beacon"},       // row0: adopted Tags column says beacon
			{"beacon in summary", ""}, // row1: the word is in a real data column
		}, testOverlay{})
	v.SetAnnotationColumns(AdoptedColumns{Tag: 1, Comment: -1})

	hits, err := v.ScanIOCs(v.CompileIOCs("beacon\n", false))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !eq(hits, []int{1}) {
		t.Errorf("hits = %v, want [1] (adopted Tags column skipped)", hits)
	}
}

func TestScanIOCsEmpty(t *testing.T) {
	v := iocView(t)
	hits, err := v.ScanIOCs(v.CompileIOCs("# nothing but a comment\n", false))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %v, want none", hits)
	}
}
