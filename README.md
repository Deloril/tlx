# Timeline explorer

## Read this first: what this is for

This is a teaching tool. It was built to help people learn the core moves of
timeline analysis — following a pivot, recognising an indicator, tagging what
matters, and building a timeline of events — on real-shaped data, in a desktop
app that behaves like the commercial ones. That is the whole reason it exists.

It has no accreditation, certification, or validation for live incident
response, and it never will. Nothing here has been tested to any forensic
soundness standard, there is no chain-of-custody guarantee, and the results are
not fit to present as evidence. Treat every output as a learning exercise, not
a finding.

That is not "don't use it". Use it to practise, to teach, to run a workshop, to
get a feel for the workflow before you sit in front of accredited tools. Just
don't mistake it for one of them, and don't put it on the critical path of a
real case.

> **This code is entirely AI-generated.** Every line was written by an AI
> assistant. None of it has been hand-written, and none of it has been read,
> reviewed or audited by a person. Read the code yourself before you trust it
> with anything, and don't assume it's correct or safe because it compiles and
> runs.

A native desktop viewer and annotator for large forensic CSV timelines — the
kind Eric Zimmerman's tools emit (`Alert, Tag, Timestamp, Field, Summary` and
similar). It opens multi-GB files without reading them into memory, and lets you
tag, comment and edit rows under one of three access modes.

It does CSV/TSV and nothing else. No EVTX, no registry hives, no artefact
parsing — point another tool at the raw evidence and feed the CSV here.

It's inspired by Eric Zimmerman's Timeline Explorer and by Timesketch, but built
for one person learning the workflow, not a team working a case. There's no
server, no shared database and no accounts — it's a desktop app that opens a
file. The point is to make the moves of timeline analysis cheap to practise:
open a file, filter it, spot an indicator, tag it, pivot on it, write it up.

## Modes

The mode selector in the toolbar switches between:

- **Read-only** — view, filter, sort, search. No changes possible.
- **Investigator** — everything above, plus arbitrary tags and a comment per
  row. Data columns stay locked.
- **World-write** — also edit any cell value.

Tags, comments and edits are stored in a sidecar file next to the source
(`<file>.tlx.json`); inside a case they go to the case database instead. They
autosave shortly after each change, so nothing needs saving by hand. The
original CSV is never modified. Export writes the current view to a new CSV with
`Tags` and `Comment` columns appended and any edits applied; if the timeline has
a comment (see below) it heads the file as a `Timeline comments:` row before the
header.

## How it handles large files

Opening a file scans it once to record the byte offset of every record, keeping
one 8-byte offset per row rather than the row contents, so memory stays
proportional to row count, not file size. A multi-GB, multi-million-row timeline
indexes in a second or two; rows are read and parsed on demand as you scroll,
with a small LRU cache. The scan is quote-aware, so `Summary` fields containing
commas and embedded newlines are treated as single records.

Filtering reads the file in parallel — one worker per core — with a progress
bar and a Cancel button, so a multi-GB pass finishes in a fraction of the time a
single thread would take. Sorting makes one pass. There is no background column
store; this is the deliberate trade for low memory and instant open.

## Keyboard and mouse

Everything is reachable both ways. Click a column header to sort, click again to
reverse. Click a row to load it into the Details panel of the right dock.

    /        focus the filter box      Ctrl+F  focus the filter box
    Enter    apply filter              Esc     clear filter
    F3       find next match           Ctrl+G  go to row number
    Ctrl+S   save annotations          Ctrl+E  export current view
    Ctrl+B   toggle right dock         Ctrl+L  toggle left sidebar
    t        tag selected row          c       comment selected row

The filter box takes a query. A bare word matches any column; `Field=value`
matches one column (case-insensitive by default), and terms combine with
`AND`/`OR`/`NOT` and parentheses, e.g.
`Summary=derp AND (tag=bad OR tag=suspicious)`. Wrap a value in `/…/` for a
regular expression. The toolbar's Filter row toggle shows a box under each
column header that filters just that column; press Enter to apply, and a lone
`*` keeps rows where the column is non-empty. Under the Tags column the box is a
tag drop-down rather than a text box. The filter row is on by default; toggle it
off if you want shorter grid rows. The Clear filters button (or Esc) drops
everything at once. "Tagged only" limits the view to annotated rows. Whatever
the filter matches is highlighted in the grid and in the hover tooltip.

The Filter panel's Tags button opens the same checklist of every tag; tick one
or more to keep rows carrying any of them, without typing `tag=`.

Right-click a cell holding a timestamp for "Filter ±5 min around this time",
which narrows that column to a 5-minute window either side of the value. The
same menu adds a timestamp to the notes' Times list, or any other cell to the
Artifacts list (see below).

The right-click menu doesn't just offer the whole cell. It pulls out the
indicators it can recognise inside the cell — IPs, hashes, domains, paths,
GUIDs, emails — and lists each as a one-click add to Artifacts, with the raw
whitespace tokens and the whole cell underneath. Adding one keeps the menu open
so you can tick off several, and if a cell holds more than twenty candidates a
"review all" option pops the full list into its own window. This is where the
indicator-spotting practice lives: the tool surfaces what looks interesting, you
decide what's worth keeping.

Timestamp columns compare with `before`, `after` and `between`, e.g.
`Timestamp between 2020 and 2021` or `Timestamp after now - 7d`. A bare year,
month or day covers the whole period, so `after 2020` means 2021 onward. Shift a
time with `+`/`-` and a duration (`s`, `m`, `h`, `d`, `w`, combining as `1d12h`);
`now` and `time` are the current time.

## Existing tag/comment columns

If an imported timeline already carries a tags column (`Tag`/`Tags`) or a
comment column (`Comment`/`Comments`/`Note`/`Notes`), tlx adopts it instead of
adding its own: the values seed the session's tags and comments, so they are
coloured, searchable with `tag=`, feed the master view, and are written back on
save. There is one set of annotation columns, not two. Tag cells split on commas
and semicolons.

## IOC lists

A timeline can carry several named IOC lists, each a set of indicators one per
line matched against the open timeline; hits are tagged `ioc:<list name>`, so
the grid shows which list matched a row. The IOC lists section of the left
sidebar lists them — click a name to edit its indicators, the play button to
run it, the trash to delete it, New list to add one. Every case starts with a
default list named after the case. A standalone timeline keeps its lists per
file; inside a case they belong to the case and run against each new timeline as
it is imported (Run all runs every list at once). Editing a list gives Save,
which stores the indicators without touching any tags, and Save & run, which
also scans the open timeline. A line is a plain string (matched against every
data column), a `/regex/`, or — with a leading backtick — a filter query using
the syntax above, e.g.
`` `Summary=psexec AND Timestamp between 2024 and 2025 ``. The plain and regex
forms skip the Tags and Comment fields, so an indicator never matches a row on
its own annotations; a backtick line reaches them only when it names the field
(`` `tag=beacon ``, `` `comment=/psexec/ ``). Blank lines and lines starting
with `#` are ignored; matching is case-insensitive.

## Panels: notes and comments

The right side of the window is a dock of collapsible panels — Filter, Details,
Investigator's notes and Timeline comments. Tap a title to collapse or expand a
panel; the float button pops one out into its own window, and "Dock to right"
(or closing that window) returns it. The left sidebar holds Saved views, Case
and IOC lists, each collapsible the same way.

**Investigator's notes** are two per-timeline lists, Artifacts and Times, filled
by right-clicking cells in the grid, by typing straight into the box under each
list, or from the Details panel. Each entry has a checkbox that strikes it
through when you're done, a button to filter the view to rows containing it, a
delete button, and — inside a case — a button to copy it into the case's default
IOC list. The lists belong to one timeline and are not shared with the others.

Field values in the Details panel are selectable: highlight part of a long
Summary (or the whole field) and right-click to add just that text to the
Artifacts or Times list.

**Timeline comments** is a free-text field per timeline for running notes
towards a write-up, saved as you type. When it is not blank it heads an export
as the first CSV row.

## Running

    tlx [file.csv]

Pass a CSV to open it on launch, or start with none and use the toolbar's
"Open CSV…" button. It opens in investigator mode, so tags and comments work
straight away; switch to Read-only or World-write from the mode drop-down in the
toolbar.

A headless diagnostic is built in for load testing:

    tlx --bench <file.csv> [-q <query>] [-sort <col>]

It reports index time, row count, memory, and serial vs parallel filter timings.

## Building

tlx is a Rust/egui app. Version 2.0 is a port of the original Go+Fyne tool to
Rust; the engine and GUI are a single crate with no cgo. You need a stable Rust
toolchain ([rustup](https://rustup.rs)).

    cargo build --release        # -> target/release/tlx
    cargo test                   # engine unit tests

**Linux** also needs the GL/windowing dev headers:

    sudo apt-get install -y libgl1-mesa-dev xorg-dev libwayland-dev libxkbcommon-dev

**Windows** embeds `packaging/windows/tlx.ico` into the exe at build time (see
`build.rs`) and links as a GUI app, so no console window opens.

Release bundles for Linux (AppImage), macOS (universal `.app`) and Windows
(icon-embedded `.exe`) are built by CI on every push; pushing a `v*` tag also
publishes them as a GitHub release.

## Layout

    src/main.rs       entry point: GUI launch and the --bench diagnostic
    src/app.rs        the egui front end
    src/engine        the engine — indexing, view, session, export (tested)
    src/prefs.rs      window/UI preferences
    build.rs          embeds the Windows icon into the exe
    packaging         icon generator and per-OS bundle assets (.icns, .ico)
    assets/icon.png   1024px app icon, embedded in the binary and shown on the window

`packaging/icon/icon-master.png` is the full-resolution source art, kept for
the README and other high-res uses. `assets/icon.png` is the 1024px version
embedded in the binary. After changing the art, downscale the master to 1024px
into `assets/icon.png`, then run `python3 packaging/icon/gen_icon.py` (needs
Pillow) to rebuild the macOS `.icns` and Windows `.ico` from it.

## License

MIT — see [LICENSE](LICENSE). Use it for anything, commercial or not; the only
condition is that you keep the copyright and licence notice. No warranty (see
the AI-generated disclaimer above).
