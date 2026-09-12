package model

import (
	"sort"
	"strings"
)

// This file handles timelines that already carry annotation columns. Some CSVs
// (a re-imported tlx export, or output from another tool) ship a Tags column
// and/or a Comment/Note column. Rather than adding tlx's own virtual columns
// beside them, we adopt the existing ones: their values seed the session's tags
// and comments, and the GUI shows the virtual Tags/Comment columns in their
// place. So there is one set of annotation columns, not two.

// AdoptedColumns names the data columns an imported timeline already uses for
// tags and comments, by source index. -1 means none was found.
type AdoptedColumns struct {
	Tag     int
	Comment int
}

// Any reports whether either an existing tag or comment column was found.
func (a AdoptedColumns) Any() bool { return a.Tag >= 0 || a.Comment >= 0 }

// Has reports whether col is one of the adopted columns.
func (a AdoptedColumns) Has(col int) bool {
	return col >= 0 && (col == a.Tag || col == a.Comment)
}

// annotationHeader reports whether a header names an annotation column: a tag
// column (tag/tags) or a comment column (comment/comments/note/notes).
func annotationHeader(h string) (isTag, isComment bool) {
	switch strings.ToLower(strings.TrimSpace(h)) {
	case "tag", "tags":
		return true, false
	case "comment", "comments", "note", "notes":
		return false, true
	}
	return false, false
}

// IsAnnotationHeader reports whether a header names a tag or comment column.
// The master view uses it to drop such columns from a timeline's data columns,
// since their content is already carried by the fixed Tags and Comment columns.
func IsAnnotationHeader(h string) bool {
	isTag, isComment := annotationHeader(h)
	return isTag || isComment
}

// DetectAnnotationColumns looks for existing tag and comment columns by header
// name (case-insensitive). The first match of each kind wins. Indices are -1
// when absent.
func DetectAnnotationColumns(headers []string) AdoptedColumns {
	ac := AdoptedColumns{Tag: -1, Comment: -1}
	for i, h := range headers {
		isTag, isComment := annotationHeader(h)
		if isTag && ac.Tag < 0 {
			ac.Tag = i
		}
		if isComment && ac.Comment < 0 {
			ac.Comment = i
		}
	}
	return ac
}

// splitTags breaks a tag cell into individual tags on commas and semicolons,
// trimming whitespace and dropping empties.
func splitTags(cell string) []string {
	fields := strings.FieldsFunc(cell, func(r rune) bool {
		return r == ',' || r == ';'
	})
	out := fields[:0]
	for _, f := range fields {
		if t := strings.TrimSpace(f); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// SeedFromColumns fills the session's tags and comments from the given source
// columns of idx, one pass over the file. Tag cells are split on commas and
// semicolons. It bypasses the mode check (an import is not a user edit) and
// leaves the session clean. Columns set to -1 are skipped; nothing happens when
// neither is set.
func (s *Session) SeedFromColumns(idx *Index, ac AdoptedColumns) error {
	if !ac.Any() {
		return nil
	}
	err := idx.Scan(func(i int, rec []string) bool {
		s.mu.Lock()
		if ac.Tag >= 0 && ac.Tag < len(rec) {
			for _, t := range splitTags(rec[ac.Tag]) {
				s.seedTagLocked(i, t)
			}
		}
		if ac.Comment >= 0 && ac.Comment < len(rec) {
			if c := strings.TrimSpace(rec[ac.Comment]); c != "" {
				s.comments[i] = c
			}
		}
		s.mu.Unlock()
		return true
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
	return nil
}

// seedTagLocked adds a tag to a row during seeding, registering it in the
// palette with the default colour. Caller holds the lock. Duplicates on a row
// are ignored.
func (s *Session) seedTagLocked(row int, tag string) {
	for _, t := range s.tags[row] {
		if t == tag {
			return
		}
	}
	s.tags[row] = append(s.tags[row], tag)
	sort.Strings(s.tags[row])
	s.defineLocked(tag, defaultTagColor, false)
}
