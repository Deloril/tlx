package casefile

import (
	"os"
	"path/filepath"
	"testing"

	"tlx/internal/model"
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

func TestDeleteTagCaseWide(t *testing.T) {
	c := newCase(t)
	idxA := memIndex([]string{"Timestamp", "Msg"}, [][]string{
		{"2026-09-01T08:12:03Z", "logon"},  // row 0: beacon only
		{"2026-09-01T09:00:00Z", "second"}, // row 1: beacon + Bad
	})
	idxB := memIndex([]string{"When", "Msg"}, [][]string{
		{"2026-09-01T10:00:00Z", "outbound"}, // row 0: beacon
	})
	tlA, _ := c.AddTimeline("A", idxA, "2026-09-11T00:00:00Z")
	tlB, _ := c.AddTimeline("B", idxB, "2026-09-11T00:00:00Z")
	if err := c.SaveAnnotations(tlA, model.SessionSnapshot{
		Tags:    map[int][]string{0: {"beacon"}, 1: {"beacon", "Bad"}},
		TagDefs: []model.TagDef{{Name: "beacon", Color: "#1E88E5"}, {Name: "Bad", Color: "#E53935"}},
	}, idxA); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveAnnotations(tlB, model.SessionSnapshot{
		Tags: map[int][]string{0: {"beacon"}},
	}, idxB); err != nil {
		t.Fatal(err)
	}

	if err := c.DeleteTag("beacon"); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}

	// Gone from both timelines' rows; Bad stays on A row 1.
	gotA, _ := c.LoadAnnotations(tlA.ID)
	if len(gotA.Tags[0]) != 0 {
		t.Errorf("A row 0 tags = %v, want []", gotA.Tags[0])
	}
	if len(gotA.Tags[1]) != 1 || gotA.Tags[1][0] != "Bad" {
		t.Errorf("A row 1 tags = %v, want [Bad]", gotA.Tags[1])
	}
	gotB, _ := c.LoadAnnotations(tlB.ID)
	if len(gotB.Tags[0]) != 0 {
		t.Errorf("B row 0 tags = %v, want []", gotB.Tags[0])
	}
	// Gone from the palette.
	defs, _ := c.TagDefs()
	if _, ok := colorOf(defs, "beacon"); ok {
		t.Errorf("beacon still in palette: %+v", defs)
	}
	// Master: only A row 1 (still tagged Bad) survives; the beacon-only rows drop.
	master, err := c.Master()
	if err != nil {
		t.Fatal(err)
	}
	if len(master) != 1 || master[0].TimelineID != tlA.ID || master[0].Row != 1 {
		t.Fatalf("master = %+v, want only A row 1", master)
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

func TestColumnRenameMergesMaster(t *testing.T) {
	c := newCase(t)
	// Two timelines with the same data but differently-named columns.
	idxA := memIndex([]string{"Timestamp", "Computer", "Message"}, [][]string{
		{"2026-09-01T08:12:03Z", "ws1", "logon"},
	})
	idxB := memIndex([]string{"When", "Host", "Event"}, [][]string{
		{"2026-09-01T09:00:00Z", "dc1", "kerberoast"},
	})
	tlA, _ := c.AddTimeline("wks", idxA, "2026-09-11T00:00:00Z")
	tlB, _ := c.AddTimeline("dc", idxB, "2026-09-11T00:00:00Z")

	// Rename B's columns to line up with A's names.
	if err := c.SetColumnNames(tlB.ID, []string{"Timestamp", "Computer", "Message"}); err != nil {
		t.Fatal(err)
	}

	if err := c.SaveAnnotations(tlA, model.SessionSnapshot{Tags: map[int][]string{0: {"Bad"}}}, idxA); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveAnnotations(tlB, model.SessionSnapshot{Tags: map[int][]string{0: {"Bad"}}}, idxB); err != nil {
		t.Fatal(err)
	}

	m, err := c.Master()
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 {
		t.Fatalf("master len = %d, want 2", len(m))
	}
	// Both entries carry their cell snapshot; B reports A's canonical names.
	byName := func(e MasterEntry, name string) string {
		for i, h := range e.DisplayHeaders {
			if h == name && i < len(e.Cells) {
				return e.Cells[i]
			}
		}
		return ""
	}
	// Entry order is by time: A (08:12) then B (09:00).
	if got := byName(m[0], "Computer"); got != "ws1" {
		t.Errorf("m[0] Computer = %q, want ws1", got)
	}
	if got := byName(m[1], "Computer"); got != "dc1" {
		t.Errorf("m[1] Computer = %q (renamed Host), want dc1", got)
	}
	if got := byName(m[1], "Message"); got != "kerberoast" {
		t.Errorf("m[1] Message = %q (renamed Event), want kerberoast", got)
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

func TestIOCLists(t *testing.T) {
	c := newCase(t)

	// A fresh case already has its default list, named after the case file.
	defaultLists, err := c.IOCLists()
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultLists) != 1 || defaultLists[0].Name != "case" {
		t.Fatalf("default lists = %+v, want one named %q", defaultLists, "case")
	}
	if body, err := c.IOCListBody(defaultLists[0].ID); err != nil || body != "" {
		t.Fatalf("default body = %q, %v, want empty", body, err)
	}

	a, err := c.CreateIOCList("Alpha")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.CreateIOCList("beta")
	if err != nil {
		t.Fatal(err)
	}

	lists, err := c.IOCLists()
	if err != nil {
		t.Fatal(err)
	}
	// Ordered by name: Alpha, beta, case.
	if len(lists) != 3 || lists[0].Name != "Alpha" || lists[1].Name != "beta" || lists[2].Name != "case" {
		t.Fatalf("lists = %+v", lists)
	}

	if err := c.SetIOCListBody(a.ID, "1.2.3.4\nevil.example.com"); err != nil {
		t.Fatal(err)
	}
	body, err := c.IOCListBody(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if body != "1.2.3.4\nevil.example.com" {
		t.Errorf("body = %q", body)
	}
	if got, err := c.IOCListBody(b.ID); err != nil || got != "" {
		t.Errorf("beta body = %q, %v, want empty", got, err)
	}

	// Duplicate names, exact and case-insensitive, are rejected.
	if _, err := c.CreateIOCList("Alpha"); err == nil {
		t.Error("CreateIOCList duplicate: want error")
	}
	if _, err := c.CreateIOCList("ALPHA"); err == nil {
		t.Error("CreateIOCList case-insensitive duplicate: want error")
	}
	if _, err := c.CreateIOCList("   "); err == nil {
		t.Error("CreateIOCList blank name: want error")
	}

	// Rename works, and is subject to the same rules.
	if err := c.RenameIOCList(b.ID, "Gamma"); err != nil {
		t.Fatal(err)
	}
	if err := c.RenameIOCList(a.ID, "gamma"); err == nil {
		t.Error("RenameIOCList to case-insensitive duplicate: want error")
	}
	// Renaming a list to its own current name (any case) must not self-collide.
	if err := c.RenameIOCList(b.ID, "GAMMA"); err != nil {
		t.Errorf("rename to own name (different case): %v", err)
	}

	if err := c.DeleteIOCList(a.ID); err != nil {
		t.Fatal(err)
	}
	lists, err = c.IOCLists()
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 2 || lists[0].Name != "GAMMA" || lists[1].Name != "case" {
		t.Fatalf("lists after delete = %+v", lists)
	}
}

// TestIOCListMigratesLegacyValue exercises the one-time migration of the old
// single meta['ioc_list'] value into the new per-case default list, named
// after the case file rather than "IOCs".
func TestIOCListMigratesLegacyValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.tlxdb")
	c, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Force the pre-migration state: drop the seeded default list and its
	// flag, and write the legacy meta value migrate() should pick up.
	if _, err := c.db.Exec(`DELETE FROM ioc_lists`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`DELETE FROM meta WHERE key='ioc_lists_seeded'`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`INSERT INTO meta(key,value) VALUES('ioc_list','1.1.1.1')`); err != nil {
		t.Fatal(err)
	}

	if err := c.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	lists, err := c.IOCLists()
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 1 || lists[0].Name != "legacy" {
		t.Fatalf("lists = %+v, want one named %q", lists, "legacy")
	}
	body, err := c.IOCListBody(lists[0].ID)
	if err != nil || body != "1.1.1.1" {
		t.Fatalf("body = %q, %v, want %q", body, err, "1.1.1.1")
	}
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM meta WHERE key='ioc_list'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("legacy meta row still present")
	}

	// Running migrate() again must not resurrect or duplicate anything, even
	// after the user deletes the seeded list.
	if err := c.DeleteIOCList(lists[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := c.migrate(); err != nil {
		t.Fatal(err)
	}
	lists, err = c.IOCLists()
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 0 {
		t.Fatalf("lists after re-migrate = %+v, want none (deletion must stick)", lists)
	}
	c.Close()
}

func TestNotes(t *testing.T) {
	c := newCase(t)
	idx := memIndex([]string{"Timestamp", "Msg"}, [][]string{{"2026-09-01T08:12:03Z", "x"}})
	tl, err := c.AddTimeline("t1", idx, "2026-09-11T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.AddTimeline("t2", idx, "2026-09-11T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}

	a1, err := c.AddNote(tl.ID, NoteArtifact, "first artifact")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := c.AddNote(tl.ID, NoteArtifact, "second artifact")
	if err != nil {
		t.Fatal(err)
	}
	tm, err := c.AddNote(tl.ID, NoteTime, "2026-09-01T08:00:00Z")
	if err != nil {
		t.Fatal(err)
	}

	// Isolation: a note on the other timeline must not show up here.
	if _, err := c.AddNote(other.ID, NoteArtifact, "belongs to t2"); err != nil {
		t.Fatal(err)
	}

	artifacts, err := c.Notes(tl.ID, NoteArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 2 || artifacts[0].ID != a1.ID || artifacts[1].ID != a2.ID {
		t.Fatalf("artifacts = %+v, want [%d %d] in order", artifacts, a1.ID, a2.ID)
	}
	if artifacts[0].Text != "first artifact" || artifacts[0].Done {
		t.Errorf("artifacts[0] = %+v", artifacts[0])
	}

	times, err := c.Notes(tl.ID, NoteTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(times) != 1 || times[0].ID != tm.ID || times[0].Text != "2026-09-01T08:00:00Z" {
		t.Fatalf("times = %+v", times)
	}

	otherArtifacts, err := c.Notes(other.ID, NoteArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherArtifacts) != 1 || otherArtifacts[0].Text != "belongs to t2" {
		t.Fatalf("other timeline artifacts = %+v, want isolated single note", otherArtifacts)
	}

	// Toggle done and confirm it persists across a fresh read.
	if err := c.SetNoteDone(a1.ID, true); err != nil {
		t.Fatal(err)
	}
	artifacts, err = c.Notes(tl.ID, NoteArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if !artifacts[0].Done {
		t.Errorf("artifacts[0].Done = false after SetNoteDone(true)")
	}
	if err := c.SetNoteDone(a1.ID, false); err != nil {
		t.Fatal(err)
	}
	artifacts, err = c.Notes(tl.ID, NoteArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if artifacts[0].Done {
		t.Errorf("artifacts[0].Done = true after SetNoteDone(false)")
	}

	// Delete removes just the one note.
	if err := c.DeleteNote(a2.ID); err != nil {
		t.Fatal(err)
	}
	artifacts, err = c.Notes(tl.ID, NoteArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].ID != a1.ID {
		t.Fatalf("artifacts after delete = %+v, want only %d", artifacts, a1.ID)
	}

	// Blank text and invalid kind are errors.
	if _, err := c.AddNote(tl.ID, NoteArtifact, "   "); err == nil {
		t.Error("AddNote blank text: want error")
	}
	if _, err := c.AddNote(tl.ID, "bogus", "text"); err == nil {
		t.Error("AddNote invalid kind: want error")
	}
}

func TestReorderNotes(t *testing.T) {
	c := newCase(t)
	idx := memIndex([]string{"Timestamp", "Msg"}, [][]string{{"2026-09-01T08:12:03Z", "x"}})
	tl, err := c.AddTimeline("t1", idx, "2026-09-11T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, txt := range []string{"one", "two", "three"} {
		n, err := c.AddNote(tl.ID, NoteArtifact, txt)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n.ID)
	}
	// Insertion order first.
	got, err := c.Notes(tl.ID, NoteArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != ids[0] || got[2].ID != ids[2] {
		t.Fatalf("initial order = %+v, want insertion order", got)
	}
	// Reverse and confirm it sticks on a fresh read.
	if err := c.ReorderNotes([]int64{ids[2], ids[1], ids[0]}); err != nil {
		t.Fatal(err)
	}
	got, err = c.Notes(tl.ID, NoteArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != ids[2] || got[1].ID != ids[1] || got[2].ID != ids[0] {
		t.Fatalf("reordered = %+v, want reversed", got)
	}
	// A note added after a reorder lands at the end (largest position).
	n4, err := c.AddNote(tl.ID, NoteArtifact, "four")
	if err != nil {
		t.Fatal(err)
	}
	got, err = c.Notes(tl.ID, NoteArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if got[len(got)-1].ID != n4.ID {
		t.Fatalf("new note not last: order = %+v", got)
	}
}

func TestTimelineComment(t *testing.T) {
	c := newCase(t)
	idx := memIndex([]string{"Timestamp", "Msg"}, [][]string{{"2026-09-01T08:12:03Z", "x"}})
	tl, err := c.AddTimeline("t1", idx, "2026-09-11T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.TimelineComment(tl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("fresh comment = %q, want empty", got)
	}

	if err := c.SetTimelineComment(tl.ID, "working theory: initial access via phishing"); err != nil {
		t.Fatal(err)
	}
	got, err = c.TimelineComment(tl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != "working theory: initial access via phishing" {
		t.Errorf("comment = %q", got)
	}

	if err := c.SetTimelineComment(tl.ID, "revised theory: lateral movement via RDP"); err != nil {
		t.Fatal(err)
	}
	got, err = c.TimelineComment(tl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != "revised theory: lateral movement via RDP" {
		t.Errorf("comment after overwrite = %q", got)
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
