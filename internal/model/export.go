package model

import (
	"encoding/csv"
	"os"
	"strconv"
	"strings"
)

// Export writes the rows currently visible in v, in view order, to a new CSV at
// destPath. Cell edits are applied and two columns, Tags and Comment, are
// appended. The source file is read but never modified.
func Export(v *View, s *Session, destPath string) error {
	return ExportOmitting(v, s, destPath, nil)
}

// ExportOmitting is Export with a set of source data columns dropped from the
// output, keyed by column index. It is used when a timeline's existing
// tag/comment columns have been adopted as the session's annotations, so the
// appended Tags/Comment columns do not duplicate the source ones.
func ExportOmitting(v *View, s *Session, destPath string, omit map[int]bool) error {
	return ExportOmittingWithComment(v, s, destPath, omit, "")
}

// ExportOmittingWithComment is ExportOmitting with an optional timeline-comment
// prelude. When comment is non-blank a leading row is written before the header:
// two cells, "Timeline comments:" and the comment text. The header and event
// rows follow unchanged.
func ExportOmittingWithComment(v *View, s *Session, destPath string, omit map[int]bool, comment string) error {
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	// Always write comma-delimited CSV, whatever the source delimiter was. A
	// tab- or pipe-delimited source left commas in field values unquoted, so the
	// output (named .csv) split apart in any comma-based reader. csv.Writer quotes
	// any field containing a comma, quote or newline.
	w.Comma = ','
	defer w.Flush()

	if strings.TrimSpace(comment) != "" {
		if err := w.Write([]string{"Timeline comments:", comment}); err != nil {
			return err
		}
	}

	header := make([]string, 0, len(v.idx.Headers())+2)
	for c, h := range v.idx.Headers() {
		if omit[c] {
			continue
		}
		header = append(header, h)
	}
	header = append(header, "Tags", "Comment")
	if err := w.Write(header); err != nil {
		return err
	}

	ncol := len(v.idx.Headers())
	for pos := 0; pos < v.Len(); pos++ {
		master := v.Master(pos)
		rec, err := v.idx.Row(master)
		if err != nil {
			return err
		}
		out := make([]string, 0, ncol+2)
		for c := 0; c < ncol; c++ {
			if omit[c] {
				continue
			}
			if val, ok := s.CellOverride(master, c); ok {
				out = append(out, val)
			} else if c < len(rec) {
				out = append(out, rec[c])
			} else {
				out = append(out, "")
			}
		}
		tags := s.Tags(master)
		joined := ""
		for i, t := range tags {
			if i > 0 {
				joined += ", "
			}
			joined += t
		}
		out = append(out, joined, s.Comment(master))
		if err := w.Write(out); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// AlignedSource is one view column feeding an output column of a custom-aligned
// export. Title is the column's display name, used as the "<title>:" label when
// the output column merges more than one source.
type AlignedSource struct {
	Ref   ColumnRef
	Title string
}

// AlignedColumn is one output column of a custom-aligned export: a name and the
// ordered list of view columns whose values fill it.
type AlignedColumn struct {
	Name    string
	Sources []AlignedSource
}

// ExportAligned writes v to destPath under a user-defined set of output columns.
// Each output column pulls from one or more view columns (data columns by index,
// or the virtual #, Tags and Comment columns). A single source is written raw; a
// column that merges two or more sources writes each as "<title>: <value>;"
// joined by a space, so the origin of every value stays visible. An optional
// timeline-comment prelude is written the same way as the other exporters.
func ExportAligned(v *View, s *Session, destPath, comment string, cols []AlignedColumn) error {
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	w.Comma = ',' // standard CSV output regardless of source delimiter; see ExportOmittingWithComment
	defer w.Flush()

	if strings.TrimSpace(comment) != "" {
		if err := w.Write([]string{"Timeline comments:", comment}); err != nil {
			return err
		}
	}

	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = c.Name
	}
	if err := w.Write(header); err != nil {
		return err
	}

	for pos := 0; pos < v.Len(); pos++ {
		master := v.Master(pos)
		rec, err := v.idx.Row(master)
		if err != nil {
			return err
		}
		out := make([]string, len(cols))
		for i, c := range cols {
			switch len(c.Sources) {
			case 0:
				out[i] = ""
			case 1:
				out[i] = cellByRef(v, s, master, c.Sources[0].Ref, rec)
			default:
				var b strings.Builder
				for j, src := range c.Sources {
					if j > 0 {
						b.WriteByte(' ')
					}
					b.WriteString(src.Title)
					b.WriteString(": ")
					b.WriteString(cellByRef(v, s, master, src.Ref, rec))
					b.WriteByte(';')
				}
				out[i] = b.String()
			}
		}
		if err := w.Write(out); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// cellByRef resolves one cell's display value for a master row, mirroring the
// grid: the virtual #, Tags and Comment columns, then cell overrides, then the
// raw record. rec is the already-fetched source row for master.
func cellByRef(v *View, s *Session, master int, ref ColumnRef, rec []string) string {
	switch ref {
	case ColRowNum:
		return strconv.Itoa(master + 1)
	case ColTags:
		return strings.Join(s.Tags(master), ", ")
	case ColComment:
		return s.Comment(master)
	default:
		c := int(ref)
		if val, ok := s.CellOverride(master, c); ok {
			return val
		}
		if c >= 0 && c < len(rec) {
			return rec[c]
		}
		return ""
	}
}
