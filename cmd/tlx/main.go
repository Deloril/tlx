// Command tlx is the Timeline explorer: a native, cross-platform viewer and
// annotator for large forensic CSV timelines.
package main

import (
	"flag"
	"fmt"
	"os"

	"tlx/internal/gui"
	"tlx/internal/model"
)

func main() {
	mode := flag.String("mode", "investigator", "initial mode: ro | investigator | world")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: tlx [-mode ro|investigator|world] [file.csv]")
		fmt.Fprintln(os.Stderr, "with no file, the window opens with an Open button.")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 1 {
		flag.Usage()
		os.Exit(2)
	}

	startMode := model.Investigator
	switch *mode {
	case "ro":
		startMode = model.ReadOnly
	case "world":
		startMode = model.WorldWrite
	}

	a := gui.New()
	a.SetStartMode(startMode)

	// A file argument is optional; without one the user opens from the toolbar.
	if flag.NArg() == 1 {
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
		sess.SetMode(startMode)
		a.OpenInitial(idx, sess)
	}

	a.Run()
}
