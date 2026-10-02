# Feature parity: tlx-rust vs the Go/Fyne original

This is the honest state of the Rust/egui port against the Go/Fyne `tlx`.
"Done" means the feature is wired into the GUI and works, not just that the
engine supports it. Where there's a gap, it says so.

The engine (indexing, view, session, query grammar, IOC scan, casefile,
export, adopt) is a near line-for-line port of `internal/model` and
`internal/casefile`, with the Go `_test.go` suites ported too — 73 tests, all
passing. The gaps below are all in the GUI layer, not the engine.

## Done

Opening and viewing
- Open a CSV/TSV; offset-based index, rows read on demand with an LRU cache.
  50k rows index in ~9 ms; a 359 MB/1.6M-row file is a second and ~15 MB.
- Virtualised grid (egui_extras TableBuilder): row-number gutter, virtual Tags
  and Comment columns, then the data columns.
- Click a header to sort, click again to reverse.
- Row selection: click, Shift-range, Cmd/Ctrl-toggle.
- Match highlighting in cells for the applied free-text filter.

Filtering
- Toolbar free-text box feeds the boolean query grammar (`Field=value`,
  `AND`/`OR`/`NOT`, parentheses, `/regex/`, `before`/`after`/`between`,
  `now`/`time`, duration shifts). Enter or Apply runs it.
- Per-column filter row (toggle in the toolbar), one box under each data
  column header; Enter applies.
- "Tagged only" toggle.
- Tag checklist in the left sidebar (OR across ticked tags).
- Detailed filter builder in the right dock: structured column conditions with
  AND/OR mode, all-values / regex / exclude flags per condition, plus the
  query box and case-sensitive / tagged-only toggles.
- Right-click a timestamp cell for "Filter ±5 min around this time".
- Clear filters (button or Esc).

Annotation (Investigator / World-write modes)
- Tag editor: tick known tags on/off for the whole selection, recolour from
  the preset palette, create / delete / rename, "No highlight". Read-only mode
  is bumped to Investigator for the duration of a change, as in the original.
- Comment editor per row.
- Cell edit under World-write.
- Tag row-tint wash painted behind each row from the top tag's colour.
- Annotations autosave to the sidecar (`<file>.tlx.json`) in standalone mode,
  or to the case DB inside a case.

Cell pop-out (the double-click-to-float feature)
- Double-click any cell to open it in its own OS window, pinned always-on-top
  by default, with a pin toggle. Comment cells (and any cell under World-write)
  are editable there; the edit commits on close.

Right dock and left sidebar
- Dock panels: Filter, Details, Investigator's notes, Timeline comments.
- Each dock panel floats out into its own window ("Pop out") with a pin-on-top
  toggle, and docks back on "Dock" or window close.
- Left sidebar: Case, Tags, Saved views, IOC lists.

Investigator's notes
- Artifacts and Times lists, with done-checkbox (strike-through), filter-to-
  entry, and delete. Add from the box, or from a cell's right-click menu.
- Backed by the case DB inside a case, else the prefs store.

Timeline comments
- Free-text per-timeline field, saved as you type; heads the file on export.
- Case DB inside a case, else prefs.

IOC lists
- Manager window: list / edit / create / delete, Run / Run all. Hits tag
  `ioc:<name>` in fixed purple.
- Inside a case the lists belong to the case DB (unique names, per-list rows);
  standalone they're per-file in prefs.

Case management
- New case / Open case (toolbar Case menu and the left-sidebar Case section).
- Create case from the open standalone timeline, carrying its annotations.
- Add timeline (copies the CSV into the case folder, registers, opens it).
- Timeline list in the sidebar; the open one is marked; click to switch.
  Switching autosaves the outgoing timeline's annotations.
- Master timeline: read-only, time-sorted view of every tagged row across the
  case, with the fixed Time/Timeline/Tags/Comment columns plus the merged union
  of renamed data columns. Double-click a master row to jump to its source.
- Rename columns, stored as display headers so matching names merge in the
  master view.

Export
- Export the current view (filtered + sorted) to a new CSV with Tags and
  Comment appended and edits applied; a non-blank timeline comment heads the
  file. Toolbar button and Ctrl+E.

Adopt existing columns
- A timeline that already carries a Tag/Comment column (and has no sidecar)
  seeds its annotations from those columns on open; the source columns are then
  hidden in the grid and omitted on export, so there's one set of annotation
  columns, not two.

Theme
- Light / dark toggle in the toolbar, persisted. This matches the original,
  which is a manual dark/light toggle — there is no "system" mode in either.

Keyboard
- `/` and Ctrl+F focus the filter; Enter applies; Esc clears.
- F3 find-next (plain-substring search across tags/comment/cells, skipping the
  filter).
- Ctrl+G go to row; Ctrl+S save; Ctrl+E export; Ctrl+B toggle dock; Ctrl+L
  toggle sidebar; `t` tag, `c` comment on the selection.

## Not yet wired (GUI gaps)

These are present in the Go app but not in the Rust GUI. The engine supports
what they'd need; they're UI work.

- Column picker / per-column visibility (hide/show data columns).
- Ctrl+Shift+F as the filter-row shortcut (the toolbar toggle works; the
  key binding isn't bound).
- The filter row's Tags-column box is a plain text box, not the tag drop-down
  the original shows there.
- Copying a note into the case's default IOC list (the in-case button on each
  note).
- Right-click a selection inside the Details panel to add just that text to
  Artifacts/Times (you can add whole cells from the grid menu; the sub-string-
  from-Details path isn't there).
- `-mode` command-line flag (the app starts read-only; change mode in the
  toolbar). The Go default is investigator.

## Notes on behaviour differences

- egui has no per-row background API, so the tag tint is painted manually
  behind each cell. Visually it matches the Fyne wash (alpha 0x40).
- Dock float and cell pop-out use egui immediate viewports (real OS windows),
  so always-on-top is a real window-level request, same as Fyne.
