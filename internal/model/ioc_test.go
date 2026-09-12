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
