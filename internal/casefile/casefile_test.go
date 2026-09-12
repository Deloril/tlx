package casefile

import (
	"os"
	"path/filepath"
	"testing"

	"timeline-engine/internal/model"
)

func memIndex(headers []string, records [][]string) *model.Index {
	return model.NewMemoryIndex(headers, records)
}

func newCase(t *testing.T) *Case {
	t.Helper()
	path := filepath.Join(t.TempDir(), "case.tlxdb")
	c, err := Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestDefaultPaletteSeeded(t *testing.T) {
	c := newCase(t)
	defs, err := c.TagDefs()
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 3 || defs[0].Name != "Bad" || defs[2].Name != "Good" {
		t.Fatalf("palette = %+v, want Bad/Suspicious/Good", defs)
	}
	if defs[0].Color != "#E53935" {
		t.Errorf("Bad colour = %s", defs[0].Color)
	}
}

func TestAddTimelineDetectsTimeColumn(t *testing.T) {
	c := newCase(t)
	idx := memIndex(
		[]string{"Timestamp", "Host", "Message"},
		[][]string{
			{"2026-09-01T08:12:03Z", "ws1", "logon"},
			{"2026-09-01T08:14:55Z", "fw", "outbound"},
		},
	)
	tl, err := c.AddTimeline("workstation", idx, "2026-09-11T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if tl.TimeCol != 0 {
		t.Errorf("TimeCol = %d, want 0", tl.TimeCol)
	}
	tls, err := c.Timelines()
	if err != nil {
		t.Fatal(err)
	}
	if len(tls) != 1 || tls[0].Name != "workstation" || len(tls[0].Headers) != 3 {
		t.Fatalf("timelines = %+v", tls)
	}
}

func TestAnnotationRoundTrip(t *testing.T) {
	c := newCase(t)
	idx := memIndex(
		[]string{"Timestamp", "Message"},
		[][]string{
			{"2026-09-01T08:12:03Z", "logon"},
			{"2026-09-01T09:00:00Z", "beacon"},
		},
	)
	tl, err := c.AddTimeline("t1", idx, "2026-09-11T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}

	snap := model.SessionSnapshot{
		Tags:     map[int][]string{0: {"Bad"}, 1: {"Suspicious"}},
		Comments: map[int]string{0: "initial access"},
		Edits:    map[int]map[int]string{1: {1: "beacon (edited)"}},
		TagDefs:  []model.TagDef{{Name: "Bad", Color: "#E53935"}, {Name: "beacon", Color: "#1E88E5"}},
	}
	if err := c.SaveAnnotations(tl, snap, idx); err != nil {
		t.Fatal(err)
	}

	got, err := c.LoadAnnotations(tl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tags[0]) != 1 || got.Tags[0][0] != "Bad" {
		t.Errorf("tags[0] = %v", got.Tags[0])
	}
	if got.Comments[0] != "initial access" {
		t.Errorf("comment[0] = %q", got.Comments[0])
	}
	if got.Edits[1][1] != "beacon (edited)" {
		t.Errorf("edit = %q", got.Edits[1][1])
	}
	// The new "beacon" def must have persisted into the case palette.
	if _, ok := colorOf(got.TagDefs, "beacon"); !ok {
		t.Errorf("beacon def not persisted: %+v", got.TagDefs)
	}
}

func TestMasterOrdering(t *testing.T) {
	c := newCase(t)
	idxA := memIndex([]string{"Timestamp", "Msg"}, [][]string{
		{"2026-09-01T09:02:10Z", "ticket forged"},  // row 0
		{"2026-09-01T08:12:03Z", "initial access"}, // row 1
	})
	idxB := memIndex([]string{"When", "Msg"}, [][]string{
		{"2026-09-01T08:14:55Z", "outbound c2"}, // row 0
		{"not-a-time", "no timestamp here"},     // row 1
	})
	tlA, _ := c.AddTimeline("dc", idxA, "2026-09-11T00:00:00Z")
	tlB, _ := c.AddTimeline("fw", idxB, "2026-09-11T00:00:00Z")

	// Tag every row so all appear in the master view.
	if err := c.SaveAnnotations(tlA, model.SessionSnapshot{
		Tags: map[int][]string{0: {"Bad"}, 1: {"Bad"}},
	}, idxA); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveAnnotations(tlB, model.SessionSnapshot{
		Tags: map[int][]string{0: {"Suspicious"}, 1: {"Good"}},
	}, idxB); err != nil {
		t.Fatal(err)
	}

	m, err := c.Master()
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 4 {
		t.Fatalf("master len = %d, want 4", len(m))
	}
	// Timed rows come first, ascending: 08:12 (dc), 08:14 (fw), 09:02 (dc).
	wantSummary := []string{"initial access", "outbound c2", "ticket forged"}
	for i, w := range wantSummary {
		if !m[i].HasTime {
			t.Errorf("entry %d should have time", i)
		}
		if got := m[i].Summary; got == "" || got[len(got)-len(w):] != w {
			t.Errorf("entry %d summary = %q, want …%q", i, got, w)
		}
	}
	// The untimed row is last.
	if m[3].HasTime {
		t.Errorf("last entry should be untimed, got %+v", m[3])
	}
	if len(m[3].Tags) != 1 || m[3].Tags[0] != "Good" {
		t.Errorf("last entry tags = %v", m[3].Tags)
	}
}

func TestRemoveTimeline(t *testing.T) {
	c := newCase(t)
	idx := memIndex([]string{"Timestamp", "Msg"}, [][]string{{"2026-09-01T08:12:03Z", "x"}})
	tl, _ := c.AddTimeline("t", idx, "2026-09-11T00:00:00Z")
	c.SaveAnnotations(tl, model.SessionSnapshot{Tags: map[int][]string{0: {"Bad"}}}, idx)

	if err := c.RemoveTimeline(tl.ID); err != nil {
		t.Fatal(err)
	}
	tls, _ := c.Timelines()
	if len(tls) != 0 {
		t.Errorf("timelines after remove = %d", len(tls))
	}
	m, _ := c.Master()
	if len(m) != 0 {
		t.Errorf("master after remove = %d", len(m))
	}
}

// TestFileBackedTimelineEndToEnd exercises the real on-disk path: a CSV opened
// via model.Open, registered, tagged, saved, then surfaced in the master view
// with a summary drawn from the source row.
func TestFileBackedTimelineEndToEnd(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "auth.csv")
	const csv = "Timestamp,Host,Message\n" +
		"2026-09-01 08:12:03,ws1,successful logon\n" +
		"2026-09-01 09:00:00,ws1,service installed\n"
	if err := os.WriteFile(csvPath, []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := model.Open(csvPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	c, err := Create(filepath.Join(dir, "case.tlxdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	tl, err := c.AddTimeline("auth", idx, "2026-09-11T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if tl.TimeCol != 0 {
		t.Fatalf("TimeCol = %d, want 0 (Timestamp)", tl.TimeCol)
	}

	snap := model.SessionSnapshot{Tags: map[int][]string{1: {"Suspicious"}}}
	if err := c.SaveAnnotations(tl, snap, idx); err != nil {
		t.Fatal(err)
	}

	m, err := c.Master()
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("master len = %d, want 1", len(m))
	}
	e := m[0]
	if !e.HasTime {
		t.Error("entry should have a parsed time")
	}
	if e.Row != 1 {
		t.Errorf("row = %d, want 1", e.Row)
	}
	if want := "service installed"; e.Summary == "" || e.Summary[len(e.Summary)-len(want):] != want {
		t.Errorf("summary = %q, want …%q", e.Summary, want)
	}
}

func colorOf(defs []model.TagDef, name string) (string, bool) {
	for _, d := range defs {
		if d.Name == name {
			return d.Color, true
		}
	}
	return "", false
}
