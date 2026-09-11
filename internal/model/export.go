package model

import (
	"encoding/csv"
	"os"
)

// Export writes the rows currently visible in v, in view order, to a new CSV at
// destPath. Cell edits are applied and two columns, Tags and Comment, are
// appended. The source file is read but never modified.
func Export(v *View, s *Session, destPath string) error {
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	w.Comma = rune(v.idx.Delimiter())
	defer w.Flush()

	header := append([]string{}, v.idx.Headers()...)
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
