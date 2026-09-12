package model

import (
	"reflect"
	"testing"
)

func TestDetectAnnotationColumns(t *testing.T) {
	cases := []struct {
		headers []string
		want    AdoptedColumns
	}{
		{[]string{"Time", "Summary", "Host"}, AdoptedColumns{Tag: -1, Comment: -1}},
		{[]string{"Time", "Tags", "Comment"}, AdoptedColumns{Tag: 1, Comment: 2}},
		{[]string{"tag", "note", "Summary"}, AdoptedColumns{Tag: 0, Comment: 1}},
		{[]string{"Time", "Notes"}, AdoptedColumns{Tag: -1, Comment: 1}},
		// First of each kind wins.
		{[]string{"Tag", "Tags", "Comment", "Note"}, AdoptedColumns{Tag: 0, Comment: 2}},
	}
	for _, c := range cases {
		got := DetectAnnotationColumns(c.headers)
		if got != c.want {
			t.Errorf("DetectAnnotationColumns(%v) = %+v, want %+v", c.headers, got, c.want)
		}
	}
}

func TestSplitTags(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"bad", []string{"bad"}},
		{"bad, lateral-movement", []string{"bad", "lateral-movement"}},
		{"a; b ;c", []string{"a", "b", "c"}},
		{" , ,, ", nil},
	}
	for _, c := range cases {
		got := splitTags(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitTags(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSeedFromColumns(t *testing.T) {
	idx := NewMemoryIndex(
		[]string{"Time", "Tags", "Comment", "Host"},
		[][]string{
			{"t0", "bad, lateral-movement", "looks bad", "dc1"},
			{"t1", "", "", "ws2"},
			{"t2", "good", "benign logon", "ws3"},
		})
	s := NewSession("(mem)")
	ac := DetectAnnotationColumns(idx.Headers())
	if err := s.SeedFromColumns(idx, ac); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if got := s.Tags(0); !reflect.DeepEqual(got, []string{"bad", "lateral-movement"}) {
		t.Errorf("row0 tags = %v", got)
	}
	if got := s.Comment(0); got != "looks bad" {
		t.Errorf("row0 comment = %q", got)
	}
	if got := s.Tags(1); got != nil {
		t.Errorf("row1 tags = %v, want none", got)
	}
	if got := s.Tags(2); !reflect.DeepEqual(got, []string{"good"}) {
		t.Errorf("row2 tags = %v", got)
	}
	if got := s.Comment(2); got != "benign logon" {
		t.Errorf("row2 comment = %q", got)
	}
	// Seeding is not a user edit.
	if s.Dirty() {
		t.Errorf("session should be clean after seeding")
	}
	// The seeded tags are registered in the palette.
	if _, ok := s.TagColor("lateral-movement"); !ok {
		t.Errorf("seeded tag not registered in palette")
	}
}

func TestSeedFromColumnsNoneIsNoop(t *testing.T) {
	idx := NewMemoryIndex([]string{"Time", "Host"}, [][]string{{"t0", "dc1"}})
	s := NewSession("(mem)")
	if err := s.SeedFromColumns(idx, DetectAnnotationColumns(idx.Headers())); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if s.Tags(0) != nil || s.Comment(0) != "" {
		t.Errorf("no annotation columns should seed nothing")
	}
}
