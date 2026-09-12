# Timeline explorer

A native desktop viewer and annotator for large forensic CSV timelines — the
kind Eric Zimmerman's tools emit (`Alert, Tag, Timestamp, Field, Summary` and
similar). It opens multi-GB files without reading them into memory, and lets you
tag, comment and edit rows under one of three access modes.

It does CSV/TSV and nothing else. No EVTX, no registry hives, no artefact
parsing — point another tool at the raw evidence and feed the CSV here.

It's inspired by Eric Zimmerman's Timeline Explorer and by Timesketch, but built
for a single examiner, not a team. There's no server, no shared database and no
accounts — it's a desktop app that opens a file. Next to Timesketch it's far
lighter: nothing to deploy and nothing to run but the binary.

## Modes

The mode selector in the toolbar switches between:

- **Read-only** — view, filter, sort, search. No changes possible.
- **Investigator** — everything above, plus arbitrary tags and a comment per
  row. Data columns stay locked.
- **World-write** — also edit any cell value.

Tags, comments and edits are stored in a sidecar file next to the source
(`<file>.tlx.json`). The original CSV is never modified. Export writes the
current view to a new CSV with `Tags` and `Comment` columns appended and any
edits applied.

## How it handles large files

Opening a file scans it once to record the byte offset of every record, keeping
one `int64` per row rather than the row contents. A 359 MB, 1.6M-row timeline
indexes in about a second and sits in ~15 MB of heap; rows are read and parsed
on demand as you scroll, with a small LRU cache. The scan is quote-aware, so
`Summary` fields containing commas and embedded newlines are treated as single
records.

Filtering and sorting each make one sequential pass over the file. On a
multi-GB file that pass takes a few seconds; there is no background column
store, so this is the deliberate trade for low memory and instant open.

## Keyboard and mouse

Everything is reachable both ways. Click a column header to sort, click again to
reverse. Click a row to load it into the detail pane on the right.

    /        focus the filter box      Ctrl+F  focus the filter box
    Enter    apply filter              Esc     clear filter
    F3       find next match           Ctrl+G  go to row number
    Ctrl+S   save annotations          Ctrl+E  export current view
    t        tag selected row          c       comment selected row

The filter box takes a query. A bare word matches any column; `Field=value`
matches one column (case-insensitive by default), and terms combine with
`AND`/`OR`/`NOT` and parentheses, e.g.
`Summary=derp AND (tag=bad OR tag=suspicious)`. Wrap a value in `/…/` for a
regular expression. Each column header also has a box that filters just that
column. "Tagged only" limits the view to annotated rows.

## Running

    tlx [-mode ro|investigator|world] <file.csv>

## Building

The engine (`internal/model`) is pure Go. The GUI (`internal/gui`) uses
[Fyne](https://fyne.io), which needs cgo and the platform's GL/windowing
headers.

**macOS** — build on a Mac with the Xcode command line tools (`xcode-select
--install`). Cross-compiling a cgo GUI from Linux needs osxcross and the Apple
SDK, so it is not done from this repo's Linux box.

    make mac              # native binary -> tlx-darwin
    make mac-universal    # arm64 + amd64 fat binary (needs lipo)

For a double-clickable `.app` bundle:

    go install fyne.io/tools/cmd/fyne@latest
    fyne package -os darwin --name "Timeline Explorer" --src ./cmd/tlx

**Linux** — install the dev headers, then `make build`:

    sudo apt-get install -y libgl1-mesa-dev xorg-dev

**Windows** — build on Windows with a MinGW-w64 gcc (e.g. via MSYS2), or
cross-compile with a mingw-w64 toolchain:

    make windows          # -> tlx.exe

## Layout

    cmd/tlx           GUI entry point
    cmd/tlxcheck      headless diagnostic: index/filter/sort timings + memory
    cmd/gentestdata   synthetic timeline generator for load testing
    internal/model    the engine — indexing, view, session, export (pure Go, tested)
    internal/gui      the Fyne front end
