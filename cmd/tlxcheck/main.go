// Command tlxcheck is a headless diagnostic for the engine: it opens a CSV,
// reports index time and memory, then times a filter and a sort. It shares no
// code with the GUI, so it runs anywhere Go runs.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"tlx/internal/model"
)

func main() {
	query := flag.String("q", "", "substring to filter across all columns")
	sortCol := flag.Int("sort", -1, "data column index to sort by (-1 = none)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: tlxcheck [-q substr] [-sort col] <file.csv>")
		os.Exit(2)
	}
	path := flag.Arg(0)

	t0 := time.Now()
	idx, err := model.Open(path, func(done, total int64) {
		fmt.Fprintf(os.Stderr, "\rindexing %3.0f%%", 100*float64(done)/float64(total))
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer idx.Close()

	fmt.Printf("indexed %d rows in %s\n", idx.RowCount(), time.Since(t0).Round(time.Millisecond))
	fmt.Printf("delimiter %q, columns: %v\n", string(idx.Delimiter()), idx.Headers())
	printMem()

	s := model.NewSession(path)
	v := model.NewView(idx, s)

	if *query != "" {
		t := time.Now()
		if err := v.Apply(model.FilterSpec{Query: *query, Column: model.ColAll}); err != nil {
			fmt.Fprintln(os.Stderr, "filter:", err)
			os.Exit(1)
		}
		fmt.Printf("filter %q -> %d rows in %s\n", *query, v.Len(), time.Since(t).Round(time.Millisecond))
	}
	if *sortCol >= 0 {
		t := time.Now()
		if err := v.Sort([]model.SortKey{{Col: model.ColumnRef(*sortCol)}}); err != nil {
			fmt.Fprintln(os.Stderr, "sort:", err)
			os.Exit(1)
		}
		fmt.Printf("sort by col %d in %s\n", *sortCol, time.Since(t).Round(time.Millisecond))
	}

	// Spot-check a few rows across the (possibly filtered/sorted) view.
	n := v.Len()
	for _, p := range []int{0, n / 2, n - 1} {
		if p < 0 || p >= n {
			continue
		}
		r, err := idx.Row(v.Master(p))
		if err != nil {
			fmt.Fprintln(os.Stderr, "row:", err)
			continue
		}
		fmt.Printf("row %d: %v\n", p, truncate(r))
	}
	printMem()
}

func truncate(rec []string) []string {
	out := make([]string, len(rec))
	for i, s := range rec {
		if len(s) > 40 {
			s = s[:40] + "…"
		}
		out[i] = s
	}
	return out
}

func printMem() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("heap in use: %d MB\n", m.HeapAlloc>>20)
}
