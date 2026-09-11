// Command tlx is the Timeline explorer: a native, cross-platform viewer and
// annotator for large forensic CSV timelines.
package main

import (
	"flag"
	"fmt"
	"os"

	"timeline-engine/internal/gui"
	"timeline-engine/internal/model"
)

func main() {
	mode := flag.String("mode", "ro", "initial mode: ro | investigator | world")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: tlx [-mode ro|investigator|world] <file.csv>")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	path := flag.Arg(0)

	idx, err := model.Open(path, func(done, total int64) {
		if total > 0 {
			fmt.Fprintf(os.Stderr, "\rindexing %3.0f%%", 100*float64(done)/float64(total))
		}
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}

	sess := model.NewSession(path)
	if err := sess.Load(); err != nil {
		fmt.Fprintln(os.Stderr, "load annotations:", err)
	}
	switch *mode {
	case "investigator":
		sess.SetMode(model.Investigator)
	case "world":
		sess.SetMode(model.WorldWrite)
	default:
		sess.SetMode(model.ReadOnly)
	}

	gui.New(idx, sess).Run()
}
