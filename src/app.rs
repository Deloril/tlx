//! eframe::App implementation: toolbar, left sidebar, right dock, and the
//! virtualized grid with sortable headers, selection, tagging, inline editing
//! and match highlighting. Rows are addressed by view position ->
//! self.rows[pos] (master) -> idx.row(master), the same indirection as the Go
//! GUI.

use eframe::egui;
use egui::text::LayoutJob;
use egui::{Color32, FontId, TextFormat};
use std::collections::BTreeSet;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::Arc;
use std::sync::mpsc::{Receiver, Sender};

use crate::engine::adopt::{detect_annotation_columns, AdoptedColumns};
use crate::engine::casefile::{Case, MasterEntry, TimelineMeta, NOTE_ARTIFACT, NOTE_TIME};
use crate::engine::session::Mode;
use crate::engine::view::Overlay;
use crate::engine::view::{COL_COMMENT, COL_ROW_NUM, COL_TAGS};
use crate::engine::{timecol, ColumnRef, FilterSpec, Index, Session, SortKey, View, COL_ALL};
use crate::prefs::{
    NoteKind, Prefs, SavedView, StoredExportCol, StoredExportLayout, StoredIoc, StoredSource,
};

/// Which per-row annotation action the small editor popup is performing.
#[derive(PartialEq)]
enum Editing {
    None,
    /// Editing the tag text for the selected master row.
    Tag(usize),
    /// Editing the comment text for the selected master row.
    Comment(usize),
    /// Inline-editing a data cell (master row, data column).
    Cell(usize, usize),
}

/// TlxApp keeps `idx` and `session` as owned fields and rebuilds a transient
/// `View` whenever the filter or sort changes (see rebuild_view) — View borrows
/// both, which can't be a field of this same struct.
pub struct TlxApp {
    idx: Option<Arc<Index>>,
    session: Session,
    rows: Vec<usize>,
    filter: FilterSpec,
    sort_keys: Vec<SortKey>,

    filter_text: String,
    applied_filter: String,
    status: String,

    sort_col: Option<ColumnRef>,
    sort_desc: bool,
    row_height: f32,

    // Word-wrap mode (toolbar "Wrap"): cells wrap to multiple lines and each
    // row grows to fit its tallest cell, so wide content stays on screen rather
    // than clipping off the right edge. `wrap_heights` is a per-view-position
    // height cache; `wrap_sig` is a cheap fingerprint of the view it was built
    // for, so the cache is rebuilt when the rows change (filter/sort) rather
    // than every frame. Off by default.
    wrap: bool,
    wrap_heights: Vec<f32>,
    wrap_sig: u64,

    // selection, keyed by master row; anchor is a view position for Shift-range.
    selected: BTreeSet<usize>,
    sel_anchor: Option<usize>,

    // per-column quick filters (header row), keyed by data column index.
    show_filter_row: bool,
    // Columns hidden from the grid via the Columns dropdown. Empty = every
    // column visible (the default). The row-number gutter is never hidden.
    hidden_cols: std::collections::HashSet<ColumnRef>,
    col_filters: Vec<String>,
    // Quick-filter boxes for the virtual Tags and Comment columns (the data
    // col_filters vec is keyed by data-column index and can't hold these).
    col_filter_tags: String,
    col_filter_comment: String,
    // Per-column value picker (header funnel): the applied set of whole-cell
    // values per column, ANDed across columns with OR within. Empty = no
    // picker on that column. `value_popup` is the open editor, if any.
    col_value_filters: std::collections::HashMap<ColumnRef, Vec<String>>,
    value_popup: Option<ValueFilterPopup>,

    // Last substring highlighted in a Details-pane field: (field salt, text).
    // Kept across frames because a right-click collapses the live selection
    // before the context menu can read it.
    details_sel: Option<(usize, String)>,

    // Comment cell being edited inline in the grid: (master row, buffer).
    // Clicking a Comment cell opens an editable box in place rather than the
    // pop-out window.
    inline_comment: Option<(usize, String)>,
    // One-shot: focus the inline Comment box on the first frame after it opens.
    // Focusing every frame would stop it ever losing focus, so a click-away
    // could never commit.
    inline_comment_focus: bool,

    // Artifact candidates already added from the currently-open cell context
    // menu: (cell master, column, added texts). The menu stays open on an Add
    // click and ticks off what's been added so a responder can pick several in
    // one pass. Reset when a new menu opens.
    menu_added: Option<(usize, ColumnRef, BTreeSet<String>)>,

    tagged_only: bool,
    cased: bool,
    tag_checklist: BTreeSet<String>, // ticked tags for the OR filter

    // detailed filter builder (R5): structured column conditions + AND/OR mode
    builder_conds: Vec<CondRow>,
    conds_any: bool,

    // transient editor popup
    editing: Editing,
    edit_buf: String,
    rename_buf: String,

    // right dock / left sidebar visibility and content
    show_right_dock: bool,
    show_left_sidebar: bool,
    timeline_comment: String,

    // floated right-dock panels (R6): each entry renders in its own viewport
    // instead of inline, with a dock-back button and always-on-top toggle.
    floating: BTreeSet<DockPanel>,
    pinned_panels: BTreeSet<DockPanel>,

    // focus request for the filter box (set by `/` and Ctrl+F)
    focus_filter: bool,

    want_scroll_to: Option<usize>, // Ctrl+G go-to-row (view position)
    goto_buf: String,
    show_goto: bool,

    // standalone-mode persistence (notes, timeline comment, IOC lists, views)
    prefs: Prefs,
    path: String,

    // new-note input buffers for the dock
    new_artifact: String,
    new_time: String,
    // inline edit of an existing note: (kind, id) being edited, and its buffer.
    editing_note: Option<(NoteKind, i64)>,
    editing_note_buf: String,
    // one-shot: focus the edit field on the first frame after an edit begins.
    editing_note_focus: bool,
    // A duplicate add was attempted: flash the existing entry a few times
    // instead of adding a second copy. (kind, id, start-time). The start is
    // filled on the first render after the attempt, since note_add has no clock.
    note_flash: Option<(NoteKind, i64, Option<f64>)>,
    // A tag delete from the left pane is awaiting confirmation (the tag name).
    // Deleting a tag strips it from every tagged row and can't be undone, so
    // the sidebar stages it here and a modal confirms before it runs.
    confirm_tag_delete: Option<String>,
    // Open "Export list" window for a notes list: (title, joined text). The text
    // is a scratch buffer the user can edit before copying.
    notes_export: Option<(String, String)>,

    // save-current-view input
    new_view_name: String,

    // IOC manager window state
    show_ioc: bool,
    ioc_lists: Vec<StoredIoc>,
    // Parallel to ioc_lists: the case-DB row id for each list, or None in
    // standalone mode (where lists live in prefs, keyed by name).
    ioc_ids: Vec<Option<i64>>,
    ioc_sel: Option<usize>,
    ioc_name: String,
    ioc_body: String,
    // Keep the IOC window above other windows; on by default like the cell
    // pop-outs and candidate-review window.
    ioc_pinned: bool,

    // Cell pop-out windows (R4): each is its own egui viewport, optionally
    // pinned always-on-top. Keyed by a monotonic id so several can coexist.
    popouts: Vec<CellPopout>,
    next_popout_id: u64,

    // Full artifact-candidate list broken out into its own window when a cell
    // has more than fits in the right-click menu. The responder walks the whole
    // list and adds the relevant ones. None when closed.
    candidate_review: Option<CandidateReview>,

    // theme (R3)
    dark: bool,
    // Alpha (0-255) of the tag highlight wash, shared by every tag. Persisted.
    tag_opacity: u8,

    // case management (R7). When `case` is Some the app works inside a case:
    // annotations persist to the case DB (not the sidecar), notes/IOC/timeline-
    // comment come from the DB, and the left sidebar lists the case timelines.
    case: Option<Case>,
    cur_timeline: Option<TimelineMeta>,
    master_mode: bool,
    master_entries: Vec<MasterEntry>,
    case_timelines: Vec<TimelineMeta>,
    rename_cols_open: bool,
    rename_cols_buf: Vec<String>,
    // Per-data-column display names (renamed columns). Empty = use raw header.
    // Shown in the grid header and the details panel; stored in the case DB.
    col_names: Vec<String>,
    // Whether the virtual annotation columns (#/Tags/Comment) show. Off in the
    // master view, which carries its own fixed columns.
    annot_cols: bool,
    // Source data columns adopted as the session's tags/comment (an imported
    // tlx export or another tool's output). Hidden in the grid and omitted on
    // export so the appended Tags/Comment columns don't duplicate them.
    adopted: AdoptedColumns,

    // Background filter/IOC job (keeps the big-file scan off the UI thread).
    // `job` is Some while a scan runs; the UI shows a progress bar + Cancel and
    // polls `job.rx` each frame. Small files skip this and run synchronously.
    job: Option<ScanJob>,

    // "To Timeline" export builder. Some while the builder dialog is open.
    timeline_export: Option<TimelineExport>,
}

/// A running background scan (filter or IOC). The worker thread owns an
/// Arc<Index> and a snapshot overlay; it reports progress through the shared
/// atomics and hands the result back over the channel. `cancel` set true makes
/// every worker stop at its next checkpoint.
struct ScanJob {
    kind: JobKind,
    rx: Receiver<JobResult>,
    progress: Arc<AtomicUsize>,
    cancel: Arc<AtomicBool>,
    total: usize,
    started: std::time::Instant,
}

#[derive(Clone, Copy, PartialEq)]
enum JobKind {
    Filter,
    FilterClear,
    Ioc,
    IocAll,
    Export,
}

/// What a worker sends back when its scan finishes (or is cancelled).
enum JobResult {
    /// Filter finished: the matched rows, already sorted.
    Filter(Result<Vec<usize>, String>),
    /// IOC finished: for each list scanned, its "ioc:<name>" tag and the
    /// master rows that hit, each paired with the ## comment (if any) of the
    /// indicator it matched. One entry for a single list, several for run-all.
    Ioc(Vec<(String, Vec<(usize, Option<String>)>)>),
    /// Export finished: (rows written, destination path), or an error message.
    Export(Result<(usize, String), String>),
    Cancelled,
}

/// What to write in a background export. The worker only reads rows and writes
/// them, never re-scans; which rows it sees is decided by the ExportScope the
/// caller pairs with the spec.
enum ExportSpec {
    /// Plain "Current view" export: source columns (minus `omit`) plus the
    /// appended Tags/Comment columns, with an optional timeline-comment prelude.
    Current {
        omit: std::collections::HashSet<usize>,
        comment: String,
    },
    /// "To Timeline" export: a custom column layout.
    Timeline {
        cols: Vec<crate::engine::export::TimelineCol>,
        comment: String,
        tagged_only: bool,
    },
}

/// Which rows an export writes. "Current view" exports exactly what is on
/// screen (filter + sort already applied); "To Timeline" exports the whole
/// timeline regardless of the active filter, so the export is a complete record
/// rather than a snapshot of the current search.
enum ExportScope {
    /// Export the already-computed current view: filter + sort applied.
    CurrentView(Vec<usize>),
    /// Export every row in the source, ignoring the filter, in the current
    /// sort order (empty keys = source order). The row set and sort are
    /// resolved inside spawn_export so a sort over a huge file runs off the UI
    /// thread, same as a filter does.
    FullTimeline { sort_keys: Vec<SortKey> },
}

/// The four floatable right-dock panels (R6), matching the Fyne dock. Ordered
/// so BTreeSet iterates them top-to-bottom.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Debug)]
enum DockPanel {
    Filter,
    Details,
    Notes,
    TimelineComment,
}

impl DockPanel {
    fn title(self) -> &'static str {
        match self {
            DockPanel::Filter => "Filter",
            DockPanel::Details => "Details",
            DockPanel::Notes => "Investigator's notes",
            DockPanel::TimelineComment => "Timeline comments",
        }
    }
}

/// One row of the detailed filter builder (R5): a column choice, newline-
/// separated values, and the all/regex/exclude flags. `column` is a ColumnRef,
/// with COL_ALL meaning "any column". Compiles to engine ColumnCond.
#[derive(Clone)]
struct CondRow {
    column: ColumnRef,
    values: String,
    all: bool,
    regexp: bool,
    neg: bool,
}

impl Default for CondRow {
    fn default() -> Self {
        CondRow { column: COL_ALL, values: String::new(), all: false, regexp: false, neg: false }
    }
}

/// Open state for a per-column value picker (the header funnel). `distinct` is
/// the deduplicated set of cell values that were visible in `col` when the
/// picker opened, sorted; `checked` is the working selection the user is
/// editing. `truncated` records that the view had more distinct values than the
/// gather cap, so the UI can say the list is partial. Gathered once on open so
/// a huge view is scanned a single time, not every frame the picker is shown.
struct ValueFilterPopup {
    col: ColumnRef,
    title: String,
    distinct: Vec<String>,
    truncated: bool,
    checked: BTreeSet<String>,
    search: String,
}

/// A floating cell detail window. `editable` mirrors the grid's edit
/// permission: a comment cell, or any cell in World-write mode.
struct CellPopout {
    id: u64,
    master: usize,
    col: ColumnRef,
    title: String,
    text: String,
    editable: bool,
    pinned: bool,
    open: bool,
    dirty: bool,
    // Last non-empty substring highlighted in the box, kept across frames: a
    // right-click collapses the live selection before the context menu reads
    // it, same as the Details pane.
    last_sel: String,
}

/// The full candidate list for one cell, shown in its own window so a long list
/// (more than the menu shows) can be reviewed in full. `added` tracks which
/// candidates have already been sent to Artifacts, so a reviewed one is ticked
/// off rather than added twice.
struct CandidateReview {
    title: String,
    cands: Vec<crate::engine::extract::Candidate>,
    added: Vec<bool>,
    open: bool,
    // Keep the pop-out above other windows; on by default like the cell pop-outs.
    pinned: bool,
}

/// A source timeline column available to the "To Timeline" export builder: a
/// column reference (a data-column index, or COL_TAGS / COL_COMMENT) and the
/// label shown in the palette and used as the "<name>: " prefix when a column
/// is fed by two or more sources.
#[derive(Clone)]
struct SourceCol {
    col: ColumnRef,
    name: String,
}

/// One output column of the "To Timeline" export: a renameable name and the
/// ordered source columns dropped into it.
struct ExportCol {
    name: String,
    sources: Vec<SourceCol>,
}

/// Open-dialog state for the "To Timeline" export builder.
struct TimelineExport {
    cols: Vec<ExportCol>,
    /// Source columns not yet assigned to any output column.
    unassigned: Vec<SourceCol>,
    new_col_name: String,
    /// Only export rows whose Tags or Comment is non-empty.
    tagged_only: bool,
}

/// Where a dragged source column is being dropped.
#[derive(Clone, Copy)]
enum DropDest {
    Unassigned,
    Col(usize),
}

/// Drag-and-drop payload: the ColumnRef of the source being dragged. Any
/// source lives in exactly one bucket (the palette or one output column), so
/// its ColumnRef identifies it uniquely.
#[derive(Clone)]
struct DragCol(ColumnRef);

/// Drag-and-drop payload for reordering output columns: the index of the
/// column being dragged. A distinct type from DragCol so the source-chip and
/// column-reorder drag interactions never pick up each other's payloads.
#[derive(Clone)]
struct DragColIdx(usize);

impl TimelineExport {
    /// Pull the source with the given ColumnRef out of whichever bucket holds
    /// it (palette or any output column).
    fn remove_col(&mut self, col: ColumnRef) -> Option<SourceCol> {
        if let Some(p) = self.unassigned.iter().position(|s| s.col == col) {
            return Some(self.unassigned.remove(p));
        }
        for ec in &mut self.cols {
            if let Some(p) = ec.sources.iter().position(|s| s.col == col) {
                return Some(ec.sources.remove(p));
            }
        }
        None
    }

    /// Move a source to a destination bucket (a no-op if it isn't found or the
    /// target column has gone).
    fn move_to(&mut self, col: ColumnRef, dest: DropDest) {
        let Some(sc) = self.remove_col(col) else { return };
        match dest {
            DropDest::Unassigned => self.unassigned.push(sc),
            DropDest::Col(i) => match self.cols.get_mut(i) {
                Some(ec) => ec.sources.push(sc),
                None => self.unassigned.push(sc),
            },
        }
    }
}

impl Default for TlxApp {
    fn default() -> Self {
        let prefs = Prefs::load();
        let dark = prefs.dark_theme();
        let tag_opacity = prefs.tag_opacity();
        TlxApp {
            idx: None,
            session: Session::new(""),
            rows: Vec::new(),
            filter: FilterSpec::default(),
            sort_keys: Vec::new(),
            filter_text: String::new(),
            applied_filter: String::new(),
            status: "Open a CSV to begin.".to_string(),
            sort_col: None,
            sort_desc: false,
            tag_opacity,
            row_height: 22.0,
            wrap: false,
            wrap_heights: Vec::new(),
            wrap_sig: 0,
            selected: BTreeSet::new(),
            sel_anchor: None,
            show_filter_row: true,
            hidden_cols: std::collections::HashSet::new(),
            col_filters: Vec::new(),
            col_value_filters: std::collections::HashMap::new(),
            value_popup: None,
            col_filter_tags: String::new(),
            col_filter_comment: String::new(),
            details_sel: None,
            inline_comment: None,
            inline_comment_focus: false,
            tagged_only: false,
            cased: false,
            tag_checklist: BTreeSet::new(),
            builder_conds: Vec::new(),
            conds_any: false,
            editing: Editing::None,
            edit_buf: String::new(),
            rename_buf: String::new(),
            show_right_dock: true,
            show_left_sidebar: true,
            timeline_comment: String::new(),
            floating: BTreeSet::new(),
            pinned_panels: BTreeSet::new(),
            focus_filter: false,
            want_scroll_to: None,
            goto_buf: String::new(),
            show_goto: false,
            prefs,
            path: String::new(),
            new_artifact: String::new(),
            new_time: String::new(),
            editing_note: None,
            editing_note_buf: String::new(),
            editing_note_focus: false,
            note_flash: None,
            confirm_tag_delete: None,
            notes_export: None,
            new_view_name: String::new(),
            show_ioc: false,
            ioc_lists: Vec::new(),
            ioc_ids: Vec::new(),
            ioc_sel: None,
            ioc_name: String::new(),
            ioc_body: String::new(),
            ioc_pinned: true,
            popouts: Vec::new(),
            candidate_review: None,
            menu_added: None,
            next_popout_id: 0,
            dark,
            case: None,
            cur_timeline: None,
            master_mode: false,
            master_entries: Vec::new(),
            case_timelines: Vec::new(),
            rename_cols_open: false,
            rename_cols_buf: Vec::new(),
            col_names: Vec::new(),
            annot_cols: true,
            adopted: AdoptedColumns::none(),
            job: None,
            timeline_export: None,
        }
    }
}

impl TlxApp {
    /// Open a standalone CSV: leaves any open case (a standalone file is not
    /// part of a case) and loads its sidecar annotations.
    pub fn open_path(&mut self, path: &str) {
        let t0 = std::time::Instant::now();
        match Index::open(path, None) {
            Ok(idx) => {
                let elapsed = t0.elapsed();
                let status = format!(
                    "Indexed {} rows in {:.0?} ({} columns, delimiter {:?})",
                    idx.row_count(),
                    elapsed,
                    idx.headers().len(),
                    idx.delimiter() as char
                );
                let mut session = Session::new(path);
                // Load an existing sidecar if present so tags/comments return.
                let _ = session.load();
                // Open ready to annotate: a standalone file starts in
                // Investigator so tags/comments work without a mode change.
                session.set_mode(Mode::Investigator);
                // A timeline that already carries Tag/Comment columns and has no
                // stored sidecar seeds its annotations from those columns, which
                // are then hidden and omitted on export. Mirrors Fyne's adopt.
                let adopted = if session.loaded() {
                    AdoptedColumns::none()
                } else {
                    let ac = detect_annotation_columns(idx.headers());
                    if ac.any() {
                        let _ = session.seed_from_columns(&idx, ac);
                    }
                    ac
                };
                // A standalone file leaves any case context behind.
                self.case = None;
                self.cur_timeline = None;
                self.master_mode = false;
                self.master_entries.clear();
                self.col_names.clear();
                self.timeline_comment = self.prefs.timeline_comment(path);
                self.install_index(idx, session, path);
                self.adopted = adopted;
                self.status = status;
            }
            Err(e) => {
                self.status = format!("Failed to open {path}: {e}");
            }
        }
    }

    /// Install a freshly opened index + session as the active view, resetting
    /// filter, sort, selection and per-column state. Shared by the standalone
    /// open path and the case-timeline open path (mirrors Go's reloadWith).
    fn install_index(&mut self, idx: Index, session: Session, path: &str) {
        self.rows = (0..idx.row_count()).collect();
        self.col_filters = vec![String::new(); idx.headers().len()];
        self.col_filter_tags.clear();
        self.col_filter_comment.clear();
        self.col_value_filters.clear();
        self.value_popup = None;
        self.hidden_cols.clear();
        self.session = session;
        self.path = path.to_string();
        self.idx = Some(Arc::new(idx));
        // A new timeline cancels any in-flight scan from the previous one.
        self.cancel_job();
        self.filter = FilterSpec::default();
        self.sort_keys.clear();
        self.sort_col = None;
        self.sort_desc = false;
        self.filter_text.clear();
        self.applied_filter.clear();
        self.builder_conds.clear();
        self.conds_any = false;
        self.tagged_only = false;
        self.tag_checklist.clear();
        self.selected.clear();
        self.sel_anchor = None;
        self.editing = Editing::None;
        self.annot_cols = true;
        self.adopted = AdoptedColumns::none();
        // Load this timeline/case's IOC lists so the sidebar shows them
        // immediately, without waiting for the manager to be opened.
        self.reload_ioc_lists();
    }

    /// Rebuilds `self.rows` from the current filter + sort via a one-shot View.
    fn rebuild_view(&mut self) -> Result<usize, String> {
        let Some(idx) = &self.idx else { return Ok(0) };
        let mut view = View::new(idx, &self.session);
        view.apply(self.filter.clone())?;
        if !self.sort_keys.is_empty() {
            view.sort(self.sort_keys.clone())?;
        }
        let len = view.len();
        self.rows = view.rows().to_vec();
        Ok(len)
    }

    /// Compose the full FilterSpec from the free-text box, per-column boxes,
    /// tagged-only and the tag checklist, then rebuild. Large files run the
    /// scan on a background thread (see spawn_filter) so the UI never blocks;
    /// small ones rebuild synchronously.
    fn apply_filter(&mut self) {
        self.apply_filter_as(JobKind::Filter);
    }

    fn apply_filter_as(&mut self, kind: JobKind) {
        if self.idx.is_none() {
            return;
        }
        let mut col_filters = std::collections::HashMap::new();
        for (c, v) in self.col_filters.iter().enumerate() {
            if !v.is_empty() {
                col_filters.insert(c as ColumnRef, v.clone());
            }
        }
        // Tags and Comment quick-filter boxes map to the virtual columns, so a
        // filter-row box under them narrows on annotations too.
        if !self.col_filter_tags.is_empty() {
            col_filters.insert(COL_TAGS, self.col_filter_tags.clone());
        }
        if !self.col_filter_comment.is_empty() {
            col_filters.insert(COL_COMMENT, self.col_filter_comment.clone());
        }
        // Structured conditions from the filter builder (R5). Values are one
        // per line; empty rows are skipped.
        let conds: Vec<crate::engine::ColumnCond> = self
            .builder_conds
            .iter()
            .filter_map(|r| {
                let values: Vec<String> = r
                    .values
                    .lines()
                    .map(|l| l.trim().to_string())
                    .filter(|l| !l.is_empty())
                    .collect();
                if values.is_empty() {
                    return None;
                }
                Some(crate::engine::ColumnCond {
                    column: r.column,
                    values,
                    regexp: r.regexp,
                    cased: self.cased,
                    all: r.all,
                    neg: r.neg,
                })
            })
            .collect();
        // The free-text box carries the boolean query grammar (Field=value,
        // AND/OR/NOT, /regex/, before/after/between), so it feeds `expr` — the
        // same routing as the Fyne app's applySearch. `query` stays empty.
        self.filter = FilterSpec {
            expr: self.filter_text.trim().to_string(),
            regexp: false,
            cased: self.cased,
            column: COL_ALL,
            tagged_only: self.tagged_only,
            tags: self.tag_checklist.iter().cloned().collect(),
            conds,
            conds_any: self.conds_any,
            col_filters,
            col_value_filters: self
                .col_value_filters
                .iter()
                .filter(|(_, v)| !v.is_empty())
                .map(|(c, v)| (*c, v.clone()))
                .collect(),
            ..Default::default()
        };
        self.applied_filter = self.filter_text.clone();
        self.spawn_filter(kind);
    }

    /// Threshold (rows) above which filter/IOC scans run on a background thread
    /// with a progress bar. Below it the scan is quick enough that a
    /// synchronous rebuild beats the thread setup and the UI never visibly
    /// stalls.
    const ASYNC_ROW_THRESHOLD: usize = 200_000;

    /// Run the current self.filter: synchronously for a small file, or on a
    /// background worker (with progress + cancel) for a large one.
    fn spawn_filter(&mut self, kind: JobKind) {
        if self.idx.is_none() {
            return;
        }
        self.cancel_job();
        let n = self.idx.as_ref().unwrap().row_count();
        if n < Self::ASYNC_ROW_THRESHOLD {
            let t0 = std::time::Instant::now();
            match self.rebuild_view() {
                Ok(rows) => self.status = format!("Filter -> {rows} rows in {:.0?}", t0.elapsed()),
                Err(e) => self.status = format!("Filter error: {e}"),
            }
            return;
        }
        // Background path: a snapshot overlay (Send + Sync) and a shared index.
        // The worker does both the scan and the sort, so neither touches the UI
        // thread — a sort over a multi-GB file is itself a full scan.
        let idx = Arc::clone(self.idx.as_ref().unwrap());
        let snap = self.session.snapshot();
        let spec = self.filter.clone();
        let annot = self.adopted;
        let sort_keys = self.sort_keys.clone();
        let progress = Arc::new(AtomicUsize::new(0));
        let cancel = Arc::new(AtomicBool::new(false));
        let (tx, rx): (Sender<JobResult>, Receiver<JobResult>) = std::sync::mpsc::channel();
        let p2 = Arc::clone(&progress);
        let c2 = Arc::clone(&cancel);
        std::thread::spawn(move || {
            let out = crate::engine::scan::parallel_filter(&idx, &snap, &spec, annot, &p2, &c2);
            let msg = match out {
                Ok(crate::engine::scan::ScanOutcome::Done(rows)) => {
                    if sort_keys.is_empty() || c2.load(Ordering::Relaxed) {
                        JobResult::Filter(Ok(rows))
                    } else {
                        let mut view = View::new(&idx, &snap);
                        view.set_rows(rows);
                        match view.sort(sort_keys) {
                            Ok(()) => JobResult::Filter(Ok(view.rows().to_vec())),
                            Err(e) => JobResult::Filter(Err(e)),
                        }
                    }
                }
                Ok(crate::engine::scan::ScanOutcome::Cancelled) => JobResult::Cancelled,
                Err(e) => JobResult::Filter(Err(e)),
            };
            let _ = tx.send(msg);
        });
        self.job = Some(ScanJob {
            kind,
            rx,
            progress,
            cancel,
            total: n,
            started: std::time::Instant::now(),
        });
        self.status = "Filtering\u{2026}".to_string();
    }

    /// Cancel any in-flight background scan and drop it. The worker sees the
    /// flag and exits; its result (if any) is discarded.
    fn cancel_job(&mut self) {
        if let Some(job) = self.job.take() {
            job.cancel.store(true, Ordering::Relaxed);
        }
    }

    /// Poll a running scan once per frame: install its result when done, else
    /// keep the progress bar live. Returns true while a job is still running
    /// (so the caller can request another repaint).
    fn poll_job(&mut self) -> bool {
        let Some(job) = &self.job else { return false };
        match job.rx.try_recv() {
            Ok(result) => {
                let elapsed = job.started.elapsed();
                self.job = None;
                match result {
                    JobResult::Filter(Ok(rows)) => {
                        // The worker already applied the sort (a sort over a
                        // huge file is itself a scan), so just install the rows.
                        let n = rows.len();
                        self.rows = rows;
                        self.reset_selection_after_filter();
                        self.status = format!("Filter -> {n} rows in {:.0?}", elapsed);
                    }
                    JobResult::Filter(Err(e)) => {
                        self.status = format!("Filter error: {e}");
                    }
                    JobResult::Ioc(lists) => {
                        self.apply_ioc_hits(lists, elapsed);
                    }
                    JobResult::Export(Ok((n, dest))) => {
                        self.status = format!("Exported {n} rows to {dest} in {:.0?}", elapsed);
                    }
                    JobResult::Export(Err(e)) => {
                        self.status = format!("Export failed: {e}");
                    }
                    JobResult::Cancelled => {
                        self.status = "Cancelled.".to_string();
                    }
                }
                false
            }
            Err(std::sync::mpsc::TryRecvError::Empty) => true,
            Err(std::sync::mpsc::TryRecvError::Disconnected) => {
                self.job = None;
                false
            }
        }
    }

    /// After a filter changes the visible set, drop any selected rows that are
    /// no longer visible and clear the range anchor.
    fn reset_selection_after_filter(&mut self) {
        let visible: BTreeSet<usize> = self.rows.iter().copied().collect();
        self.selected.retain(|m| visible.contains(m));
        self.sel_anchor = None;
    }

    fn apply_sort(&mut self, col: ColumnRef) {
        if self.idx.is_none() {
            return;
        }
        if self.sort_col == Some(col) {
            self.sort_desc = !self.sort_desc;
        } else {
            self.sort_col = Some(col);
            self.sort_desc = false;
        }
        self.sort_keys = vec![SortKey { col, desc: self.sort_desc }];
        // Sorting a huge file re-scans it to gather the sort keys, so route it
        // through the same background worker as filtering (which now sorts too)
        // rather than blocking the UI.
        self.spawn_filter(JobKind::Filter);
    }

    fn clear_filters(&mut self) {
        self.filter_text.clear();
        for v in self.col_filters.iter_mut() {
            v.clear();
        }
        self.col_filter_tags.clear();
        self.col_filter_comment.clear();
        self.col_value_filters.clear();
        self.value_popup = None;
        self.tagged_only = false;
        self.tag_checklist.clear();
        self.builder_conds.clear();
        self.conds_any = false;
        self.apply_filter_as(JobKind::FilterClear);
    }

    fn save_session(&mut self) {
        // In master mode nothing is editable, so there is nothing to save.
        if self.master_mode {
            self.status = "Master timeline is read-only.".to_string();
            return;
        }
        // Inside a case, annotations persist to the case DB; standalone files
        // use the sidecar.
        if self.case.is_some() && self.cur_timeline.is_some() {
            self.autosave_case();
            if !self.status.starts_with("Save") {
                self.status = "Saved annotations to case.".to_string();
            }
            return;
        }
        match self.session.save() {
            Ok(()) => self.status = "Saved annotations.".to_string(),
            Err(e) => self.status = format!("Save failed: {e}"),
        }
    }

    /// Export the current view (filtered + sorted) to a new CSV with Tags and
    /// Comment columns appended and any cell edits applied. A non-blank timeline
    /// comment heads the file. Mirrors the Fyne app's export. The source CSV is
    /// never touched.
    fn export_view(&mut self) {
        if self.idx.is_none() {
            self.status = "Open a file first.".to_string();
            return;
        }
        let default_name = {
            let stem = file_stem(&self.path);
            format!("{stem}-export.csv")
        };
        let Some(dest) = rfd::FileDialog::new()
            .add_filter("CSV", &["csv"])
            .set_file_name(default_name)
            .save_file()
        else {
            return;
        };
        let dest = dest.display().to_string();
        let comment = self.timeline_comment.clone();
        // Omit any adopted source Tag/Comment columns so they are not duplicated
        // by the appended Tags/Comment columns.
        let mut omit = std::collections::HashSet::new();
        if self.adopted.tag >= 0 {
            omit.insert(self.adopted.tag as usize);
        }
        if self.adopted.comment >= 0 {
            omit.insert(self.adopted.comment as usize);
        }
        let rows = self.rows.clone();
        self.spawn_export(dest, ExportSpec::Current { omit, comment }, ExportScope::CurrentView(rows));
    }

    /// Write an export to `dest`, off the UI thread for a large file. Mirrors
    /// spawn_filter: a small view exports inline; a large one runs on a worker
    /// with progress + cancel, so the window never freezes on a multi-GB write.
    /// The worker reads the annotation overlay through a Send+Sync snapshot, same
    /// as the filter scan. `scope` decides the rows: the current view as-is, or
    /// the whole timeline re-sorted (the sort runs on the worker for a big file,
    /// since a sort over a multi-GB file is itself a full scan).
    fn spawn_export(&mut self, dest: String, spec: ExportSpec, scope: ExportScope) {
        let Some(idx) = self.idx.clone() else {
            self.status = "Open a file first.".to_string();
            return;
        };
        self.cancel_job();
        let (rows, sort_keys) = match scope {
            ExportScope::CurrentView(rows) => (rows, Vec::new()),
            ExportScope::FullTimeline { sort_keys } => ((0..idx.row_count()).collect(), sort_keys),
        };
        let n = rows.len();
        if n < Self::ASYNC_ROW_THRESHOLD {
            let mut view = View::new(&idx, &self.session);
            view.set_rows(rows);
            if !sort_keys.is_empty() {
                if let Err(e) = view.sort(sort_keys) {
                    self.status = format!("Export failed: {e}");
                    return;
                }
            }
            let progress = AtomicUsize::new(0);
            let cancel = AtomicBool::new(false);
            let t0 = std::time::Instant::now();
            match run_export(&view, &dest, &spec, &progress, &cancel) {
                Ok(crate::engine::export::ExportOutcome::Done(w)) => {
                    self.status = format!("Exported {w} rows to {dest} in {:.0?}", t0.elapsed());
                }
                Ok(crate::engine::export::ExportOutcome::Cancelled) => {
                    self.status = "Export cancelled.".to_string();
                }
                Err(e) => self.status = format!("Export failed: {e}"),
            }
            return;
        }
        let snap = self.session.snapshot();
        let progress = Arc::new(AtomicUsize::new(0));
        let cancel = Arc::new(AtomicBool::new(false));
        let (tx, rx): (Sender<JobResult>, Receiver<JobResult>) = std::sync::mpsc::channel();
        let p2 = Arc::clone(&progress);
        let c2 = Arc::clone(&cancel);
        std::thread::spawn(move || {
            let mut view = View::new(&idx, &snap);
            view.set_rows(rows);
            if !sort_keys.is_empty() {
                if let Err(e) = view.sort(sort_keys) {
                    let _ = tx.send(JobResult::Export(Err(e)));
                    return;
                }
            }
            let msg = match run_export(&view, &dest, &spec, &p2, &c2) {
                Ok(crate::engine::export::ExportOutcome::Done(w)) => JobResult::Export(Ok((w, dest))),
                Ok(crate::engine::export::ExportOutcome::Cancelled) => JobResult::Cancelled,
                Err(e) => JobResult::Export(Err(e.to_string())),
            };
            let _ = tx.send(msg);
        });
        self.job = Some(ScanJob {
            kind: JobKind::Export,
            rx,
            progress,
            cancel,
            total: n,
            started: std::time::Instant::now(),
        });
        self.status = "Exporting\u{2026}".to_string();
    }

    /// The display name of data column `c`: the renamed name if set, else the
    /// raw source header.
    fn col_display_name(&self, c: usize) -> String {
        if let Some(n) = self.col_names.get(c).filter(|n| !n.is_empty()) {
            return n.clone();
        }
        self.idx
            .as_ref()
            .and_then(|i| i.headers().get(c).cloned())
            .unwrap_or_default()
    }

    /// Open the "To Timeline" export builder. If a layout was saved for this
    /// case/file, restore it; otherwise seed the five default output columns
    /// with a best-guess mapping of the current columns.
    fn open_timeline_export_builder(&mut self) {
        if self.idx.is_none() {
            self.status = "Open a file first.".to_string();
            return;
        }
        self.timeline_export = Some(match self.load_export_layout() {
            Some(stored) => self.timeline_export_from_stored(&stored),
            None => self.build_timeline_export(),
        });
    }

    /// Load the saved export layout: from the case DB in case mode, else the
    /// per-file prefs store. None if nothing is saved.
    fn load_export_layout(&self) -> Option<StoredExportLayout> {
        if self.case.is_some() && self.cur_timeline.is_some() {
            return self.case.as_ref().and_then(|c| c.export_layout().ok().flatten());
        }
        if self.path.is_empty() {
            return None;
        }
        self.prefs.export_layout(&self.path)
    }

    /// Persist the current builder layout: to the case DB in case mode, else
    /// the per-file prefs store.
    fn save_export_layout(&mut self, te: &TimelineExport) {
        let stored = StoredExportLayout {
            tagged_only: te.tagged_only,
            columns: te
                .cols
                .iter()
                .map(|c| StoredExportCol {
                    name: c.name.clone(),
                    sources: c.sources.iter().map(stored_source).collect(),
                })
                .collect(),
        };
        if self.case.is_some() && self.cur_timeline.is_some() {
            if let Some(case) = self.case.as_mut() {
                let _ = case.set_export_layout(&stored);
            }
        } else if !self.path.is_empty() {
            self.prefs.set_export_layout(&self.path, stored);
        }
    }

    /// Rebuild a builder from a saved layout, resolving each stored source
    /// against the current columns. Any current source not referenced by the
    /// layout stays in the palette, so columns added since the layout was saved
    /// are still reachable.
    fn timeline_export_from_stored(&self, stored: &StoredExportLayout) -> TimelineExport {
        // Start from the full pool of current sources, then move resolved ones
        // into their output columns.
        let mut base = self.build_source_pool();
        let take_data = |pool: &mut Vec<SourceCol>, name: &str| -> Option<SourceCol> {
            let pos = pool
                .iter()
                .position(|s| s.col >= 0 && s.name.eq_ignore_ascii_case(name))?;
            Some(pool.remove(pos))
        };
        let take_virtual = |pool: &mut Vec<SourceCol>, want: ColumnRef| -> Option<SourceCol> {
            let pos = pool.iter().position(|s| s.col == want)?;
            Some(pool.remove(pos))
        };

        let mut cols = Vec::with_capacity(stored.columns.len());
        for sc in &stored.columns {
            let mut sources = Vec::new();
            for src in &sc.sources {
                let got = match src.kind.as_str() {
                    "tags" => take_virtual(&mut base, COL_TAGS),
                    "comment" => take_virtual(&mut base, COL_COMMENT),
                    _ => take_data(&mut base, &src.name),
                };
                if let Some(s) = got {
                    sources.push(s);
                }
            }
            cols.push(ExportCol { name: sc.name.clone(), sources });
        }
        TimelineExport { cols, unassigned: base, new_col_name: String::new(), tagged_only: stored.tagged_only }
    }

    /// The draggable source pool for the builder: visible data columns (adopted
    /// ones excluded) plus the virtual Tags and Comment.
    fn build_source_pool(&self) -> Vec<SourceCol> {
        let n = self.idx.as_ref().map(|i| i.headers().len()).unwrap_or(0);
        let mut pool: Vec<SourceCol> = Vec::new();
        for c in 0..n as ColumnRef {
            if self.adopted.has(c) {
                continue;
            }
            pool.push(SourceCol { col: c, name: self.col_display_name(c as usize) });
        }
        pool.push(SourceCol { col: COL_TAGS, name: "Tags".to_string() });
        pool.push(SourceCol { col: COL_COMMENT, name: "Comment".to_string() });
        pool
    }

    /// Build the initial builder state: all draggable sources (visible data
    /// columns plus the virtual Tags/Comment) and the five default output
    /// columns, with obvious name matches pre-mapped. Unmatched sources stay in
    /// the palette.
    fn build_timeline_export(&self) -> TimelineExport {
        // Sources are the columns the grid shows: data columns minus any
        // adopted ones (those are folded into the virtual Tags/Comment), then
        // the virtual Tags and Comment themselves.
        let mut pool = self.build_source_pool();

        // Pull the first source whose display name contains any needle
        // (case-insensitive), removing it from the pool.
        fn take_match(pool: &mut Vec<SourceCol>, needles: &[&str]) -> Option<SourceCol> {
            let pos = pool.iter().position(|s| {
                let low = s.name.to_lowercase();
                needles.iter().any(|needle| low.contains(needle))
            })?;
            Some(pool.remove(pos))
        }
        fn take_ref(pool: &mut Vec<SourceCol>, want: ColumnRef) -> Option<SourceCol> {
            let pos = pool.iter().position(|s| s.col == want)?;
            Some(pool.remove(pos))
        }
        fn col(name: &str, src: Option<SourceCol>) -> ExportCol {
            ExportCol { name: name.to_string(), sources: src.into_iter().collect() }
        }

        let timestamp = take_match(&mut pool, &["datetime", "timestamp", "time", "date"]);
        let typ = take_match(&mut pool, &["type", "desc", "category", "field", "artifact"]);
        let event = take_match(&mut pool, &["message", "event", "detail", "descrip", "activity", "summary"]);
        // Comments/Tag default to the session's own annotations.
        let comments = take_ref(&mut pool, COL_COMMENT);
        let tag = take_ref(&mut pool, COL_TAGS);

        let cols = vec![
            col("Timestamp", timestamp),
            col("Type/Field", typ),
            col("Event", event),
            col("Comments", comments),
            col("Tag", tag),
        ];
        TimelineExport { cols, unassigned: pool, new_col_name: String::new(), tagged_only: false }
    }

    /// Draw the "To Timeline" export builder: a palette of unassigned timeline
    /// columns on the left and the output columns on the right, wired with
    /// drag-and-drop. Early-returns when the builder is closed.
    fn timeline_export_window(&mut self, ctx: &egui::Context) {
        let Some(mut te) = self.timeline_export.take() else { return };
        let mut open = true;
        let mut do_export = false;
        let mut cancel = false;
        let mut pending_move: Option<(ColumnRef, DropDest)> = None;
        let mut pending_reorder: Option<(usize, usize)> = None;
        let mut remove_col: Option<usize> = None;

        // Cap the window to the screen so the action row never slides under
        // the main window's bottom edge.
        let screen_h = ctx.input(|i| i.viewport_rect().height());
        let max_h = (screen_h - 80.0).max(240.0);
        egui::Window::new("Export to timeline")
            .collapsible(false)
            .resizable(true)
            .default_size([760.0, 520.0])
            .max_height(max_h)
            .open(&mut open)
            .show(ctx, |ui| {
                // Dark-mode contrast: the default text-field fill is near-black
                // on an already-dark window, so lift it and give widgets a
                // visible border.
                if ui.visuals().dark_mode {
                    let v = ui.visuals_mut();
                    v.extreme_bg_color = Color32::from_gray(42);
                    v.widgets.inactive.bg_stroke = egui::Stroke::new(1.0, Color32::from_gray(90));
                    v.widgets.hovered.bg_stroke = egui::Stroke::new(1.0, Color32::from_gray(130));
                }
                ui.label(
                    "Drag timeline columns onto the export columns on the right. An export \
                     column fed by two or more timeline columns is written as \
                     \"name: value; \" for each source.",
                );
                ui.separator();

                // Reserve room for the controls below the mapping panes (add
                // column, checkbox, two separators, action buttons) so the
                // panes get the rest and the buttons stay on screen.
                let row = ui.spacing().interact_size.y + ui.spacing().item_spacing.y;
                let reserved = 3.0 * row + 28.0;
                let body_h = (ui.available_height() - reserved).max(140.0);

                ui.horizontal_top(|ui| {
                    // Left: palette of unassigned sources (also a drop target,
                    // so you can drag a column back out of an export column).
                    ui.vertical(|ui| {
                        ui.set_width(220.0);
                        ui.strong("Timeline columns");
                        let (_, dropped) = ui.dnd_drop_zone::<DragCol, _>(
                            egui::Frame::group(ui.style()),
                            |ui| {
                                ui.set_min_width(200.0);
                                egui::ScrollArea::vertical()
                                    .id_salt("te-palette")
                                    .max_height(body_h)
                                    .auto_shrink([false, false])
                                    .show(ui, |ui| {
                                        ui.set_min_height(body_h);
                                        for s in &te.unassigned {
                                            let id = egui::Id::new(("te-src", s.col));
                                            ui.dnd_drag_source(id, DragCol(s.col), |ui| {
                                                source_chip(ui, &s.name);
                                            });
                                        }
                                        if te.unassigned.is_empty() {
                                            ui.weak("(all columns assigned)");
                                        }
                                    });
                            },
                        );
                        if let Some(p) = dropped {
                            pending_move = Some((p.0, DropDest::Unassigned));
                        }
                    });
                    ui.separator();
                    // Right: the output columns. Each is a drop target for a
                    // dragged column (reorder by its grip handle) as well as for
                    // source chips.
                    ui.vertical(|ui| {
                        ui.strong("Export columns");
                        ui.weak("Drag the dotted handle on the left of a column to reorder.");
                        egui::ScrollArea::vertical()
                            .id_salt("te-cols")
                            .max_height(body_h)
                            .auto_shrink([false, false])
                            .show(ui, |ui| {
                                for (i, ec) in te.cols.iter_mut().enumerate() {
                                    let (_, col_dropped) = ui.dnd_drop_zone::<DragColIdx, _>(
                                        egui::Frame::new().inner_margin(3.0),
                                        |ui| {
                                            ui.horizontal(|ui| {
                                                // Grip handle: dragging it reorders
                                                // the column.
                                                let grip_id = egui::Id::new(("te-col-grip", i));
                                                ui.dnd_drag_source(grip_id, DragColIdx(i), |ui| {
                                                    grip_handle(ui);
                                                });
                                                ui.add(
                                                    egui::TextEdit::singleline(&mut ec.name)
                                                        .desired_width(150.0),
                                                );
                                                if ui
                                                    .small_button("\u{1F5D1}")
                                                    .on_hover_text("Remove column")
                                                    .clicked()
                                                {
                                                    remove_col = Some(i);
                                                }
                                            });
                                            let (_, dropped) = ui.dnd_drop_zone::<DragCol, _>(
                                                egui::Frame::group(ui.style()),
                                                |ui| {
                                                    ui.set_min_width(410.0);
                                                    ui.set_min_height(30.0);
                                                    ui.horizontal_wrapped(|ui| {
                                                        for s in &ec.sources {
                                                            let id =
                                                                egui::Id::new(("te-chip", i, s.col));
                                                            ui.dnd_drag_source(
                                                                id,
                                                                DragCol(s.col),
                                                                |ui| {
                                                                    source_chip(ui, &s.name);
                                                                },
                                                            );
                                                        }
                                                        if ec.sources.is_empty() {
                                                            ui.weak("drop timeline columns here");
                                                        }
                                                    });
                                                },
                                            );
                                            if let Some(p) = dropped {
                                                pending_move = Some((p.0, DropDest::Col(i)));
                                            }
                                        },
                                    );
                                    if let Some(src) = col_dropped {
                                        pending_reorder = Some((src.0, i));
                                    }
                                    ui.add_space(6.0);
                                }
                            });
                    });
                });

                ui.separator();
                ui.horizontal(|ui| {
                    ui.add(
                        egui::TextEdit::singleline(&mut te.new_col_name)
                            .hint_text("New column name")
                            .desired_width(170.0),
                    );
                    if ui.button("Add column").clicked() {
                        let name = te.new_col_name.trim();
                        let name = if name.is_empty() { "Column".to_string() } else { name.to_string() };
                        te.cols.push(ExportCol { name, sources: Vec::new() });
                        te.new_col_name.clear();
                    }
                });
                ui.checkbox(&mut te.tagged_only, "Export tagged / commented fields only");
                ui.separator();
                ui.horizontal(|ui| {
                    if ui.button("Export\u{2026}").clicked() {
                        do_export = true;
                    }
                    if ui.button("Cancel").clicked() {
                        cancel = true;
                    }
                });
            });

        // Apply a column reorder first, then a source move, then any removal.
        // At most one drag-drop completes per frame, so these don't contend.
        if let Some((from, to)) = pending_reorder {
            if from != to && from < te.cols.len() {
                let col = te.cols.remove(from);
                let dest = to.min(te.cols.len());
                te.cols.insert(dest, col);
            }
        }
        if let Some((col, dest)) = pending_move {
            te.move_to(col, dest);
        }
        if let Some(i) = remove_col {
            if i < te.cols.len() {
                let ec = te.cols.remove(i);
                te.unassigned.extend(ec.sources);
            }
        }

        if do_export {
            // Keep the dialog open if the user cancels the save sheet; close it
            // on a real write (success or failure is reported in the status).
            if self.run_timeline_export(&te) {
                open = false;
            }
        }
        if cancel {
            open = false;
        }
        if open {
            self.timeline_export = Some(te);
        }
    }

    /// Write the current view to a CSV using the builder's column layout.
    /// Returns true if a destination was chosen (the export ran), false if the
    /// save dialog was cancelled.
    fn run_timeline_export(&mut self, te: &TimelineExport) -> bool {
        if self.idx.is_none() {
            self.status = "Open a file first.".to_string();
            return false;
        }
        let default_name = {
            let stem = file_stem(&self.path);
            format!("{stem}-timeline.csv")
        };
        let Some(dest) = rfd::FileDialog::new()
            .add_filter("CSV", &["csv"])
            .set_file_name(default_name)
            .save_file()
        else {
            return false;
        };
        let dest = dest.display().to_string();
        let cols: Vec<crate::engine::export::TimelineCol> = te
            .cols
            .iter()
            .map(|c| crate::engine::export::TimelineCol {
                name: c.name.clone(),
                sources: c.sources.iter().map(|s| (s.col, s.name.clone())).collect(),
            })
            .collect();
        let comment = self.timeline_comment.clone();
        // Persist the layout now, before the (possibly backgrounded) write, so
        // it survives regardless of how the export itself ends.
        self.save_export_layout(te);
        // A timeline export is a complete record, so it covers every row
        // regardless of the active filter, in the current sort order.
        self.spawn_export(
            dest,
            ExportSpec::Timeline { cols, comment, tagged_only: te.tagged_only },
            ExportScope::FullTimeline { sort_keys: self.sort_keys.clone() },
        );
        true
    }

    /// Build and apply "<field> between t-5m and t+5m" for the given data
    /// column and raw cell text (G15 right-click menu).
    fn filter_plus_minus_5min(&mut self, col: usize, cell_text: &str) {
        let Some(idx) = &self.idx else { return };
        let Some(t) = timecol::parse_time(cell_text) else {
            self.status = "Cell is not a timestamp.".to_string();
            return;
        };
        let header = idx.headers().get(col).cloned().unwrap_or_default();
        let lo = (t - chrono::Duration::minutes(5)).format("%Y-%m-%dT%H:%M:%SZ");
        let hi = (t + chrono::Duration::minutes(5)).format("%Y-%m-%dT%H:%M:%SZ");
        let expr = format!("{} between {} and {}", quote_query_field(&header), lo, hi);
        self.filter_text = expr.clone();
        self.filter = FilterSpec { expr, column: COL_ALL, ..Default::default() };
        match self.rebuild_view() {
            Ok(n) => {
                self.applied_filter.clear();
                self.status = format!("\u{00B1}5 min -> {n} rows");
            }
            Err(e) => self.status = format!("Filter error: {e}"),
        }
    }

    /// Persist a timeline comment edit: to the case DB inside a case, else the
    /// standalone prefs store.
    fn save_timeline_comment(&mut self) {
        let text = self.timeline_comment.clone();
        if let (Some(case), Some(tl)) = (self.case.as_mut(), self.cur_timeline.as_ref()) {
            if let Err(e) = case.set_timeline_comment(tl.id, &text) {
                self.status = format!("Save comment: {e}");
            }
        } else if !self.path.is_empty() {
            self.prefs.set_timeline_comment(&self.path, &text);
        }
    }

    /// Click handling on a grid row: plain = select one, Shift = range from
    /// anchor, Cmd/Ctrl = toggle.
    fn on_row_click(&mut self, view_pos: usize, modifiers: egui::Modifiers) {
        let master = self.rows[view_pos];
        if modifiers.shift {
            if let Some(anchor) = self.sel_anchor {
                let (lo, hi) = if anchor <= view_pos { (anchor, view_pos) } else { (view_pos, anchor) };
                self.selected.clear();
                for p in lo..=hi {
                    if p < self.rows.len() {
                        self.selected.insert(self.rows[p]);
                    }
                }
            } else {
                self.selected.insert(master);
                self.sel_anchor = Some(view_pos);
            }
        } else if modifiers.command || modifiers.ctrl {
            if !self.selected.remove(&master) {
                self.selected.insert(master);
            }
            self.sel_anchor = Some(view_pos);
        } else {
            self.selected.clear();
            self.selected.insert(master);
            self.sel_anchor = Some(view_pos);
        }
    }

    fn selected_master(&self) -> Option<usize> {
        self.selected.iter().next().copied()
    }

    /// F3 find-next: scroll to the next view row after the current selection
    /// whose tags, comment or any cell contains the search text (case-
    /// insensitive). Does not change the filter. The needle is the plain-text
    /// value from the search box, so it works alongside the query grammar.
    /// Mirrors Fyne's findNext.
    fn find_next(&mut self) {
        let needle = find_needle(&self.filter_text);
        if needle.is_empty() {
            return;
        }
        let needle = needle.to_lowercase();
        // Resume after the first currently-selected view position.
        let start = self
            .selected_master()
            .and_then(|m| self.rows.iter().position(|&r| r == m))
            .map(|p| p + 1)
            .unwrap_or(0);
        for pos in start..self.rows.len() {
            let master = self.rows[pos];
            if self.row_contains(master, &needle) {
                self.want_scroll_to = Some(pos);
                self.selected.clear();
                self.selected.insert(master);
                self.sel_anchor = Some(pos);
                return;
            }
        }
        self.status = "No further matches.".to_string();
    }

    /// True if a row's tags, comment or any (edited) cell contains the lower-
    /// cased needle. Mirrors Fyne's rowContains.
    fn row_contains(&mut self, master: usize, needle_lower: &str) -> bool {
        if self.session.tags(master).join(" ").to_lowercase().contains(needle_lower) {
            return true;
        }
        if self.session.comment(master).to_lowercase().contains(needle_lower) {
            return true;
        }
        let rec = match self.idx.as_mut().and_then(|i| i.row(master).ok()) {
            Some(r) => r,
            None => return false,
        };
        for (i, cell) in rec.iter().enumerate() {
            let v = self.session.cell_override(master, i).unwrap_or_else(|| cell.clone());
            if v.to_lowercase().contains(needle_lower) {
                return true;
            }
        }
        false
    }

    // --- case management (R7) -------------------------------------------------

    /// Swap in a new active case, dropping any previous one, and refresh the
    /// timeline list. Mirrors Go's setCase.
    fn set_case(&mut self, case: Case) {
        self.case = Some(case);
        self.cur_timeline = None;
        self.master_mode = false;
        self.master_entries.clear();
        self.refresh_case_timelines();
    }

    fn refresh_case_timelines(&mut self) {
        self.case_timelines = match &self.case {
            Some(c) => c.timelines().unwrap_or_default(),
            None => Vec::new(),
        };
    }

    /// New case: pick a save path, create the case folder + DB, open it empty.
    fn new_case(&mut self) {
        let Some(path) = rfd::FileDialog::new()
            .add_filter("tlx case", &["tlxdb"])
            .set_file_name("case.tlxdb")
            .save_file()
        else {
            return;
        };
        let db_path = match case_folder_db(&path.display().to_string()) {
            Ok(p) => p,
            Err(e) => {
                self.status = format!("Create case: {e}");
                return;
            }
        };
        match Case::create(&db_path) {
            Ok(c) => {
                self.set_case(c);
                self.clear_view();
                self.status = format!("Created case {}", case_display_name(&db_path));
            }
            Err(e) => self.status = format!("Create case: {e}"),
        }
    }

    /// Open an existing case DB and show its first timeline (or an empty view).
    fn open_case(&mut self) {
        let Some(path) = rfd::FileDialog::new()
            .add_filter("tlx case", &["tlxdb"])
            .pick_file()
        else {
            return;
        };
        match Case::open(&path.display().to_string()) {
            Ok(c) => {
                self.set_case(c);
                let first = self.case_timelines.first().cloned();
                match first {
                    Some(tl) => self.open_timeline(tl),
                    None => self.clear_view(),
                }
                self.status = format!("Opened case {}", case_display_name(&path.display().to_string()));
            }
            Err(e) => self.status = format!("Open case: {e}"),
        }
    }

    /// Create a case seeded from the currently open standalone timeline,
    /// carrying its annotations across. Mirrors Go's createCaseWithCurrent.
    fn create_case_with_current(&mut self) {
        if self.idx.is_none() || self.case.is_some() || self.master_mode {
            return;
        }
        let src = self.path.clone();
        let snap = self.session.snapshot();
        let Some(path) = rfd::FileDialog::new()
            .add_filter("tlx case", &["tlxdb"])
            .set_file_name("case.tlxdb")
            .save_file()
        else {
            return;
        };
        let db_path = match case_folder_db(&path.display().to_string()) {
            Ok(p) => p,
            Err(e) => {
                self.status = format!("Create case: {e}");
                return;
            }
        };
        let mut case = match Case::create(&db_path) {
            Ok(c) => c,
            Err(e) => {
                self.status = format!("Create case: {e}");
                return;
            }
        };
        let dest = match import_into_case(&db_path, &src) {
            Ok(d) => d,
            Err(e) => {
                self.status = format!("Import timeline: {e}");
                return;
            }
        };
        let idx = match Index::open(&dest, None) {
            Ok(i) => i,
            Err(e) => {
                self.status = format!("Open {dest}: {e}");
                return;
            }
        };
        let name = file_stem(&dest);
        let tl = match case.add_timeline(&name, &idx, &now_rfc3339()) {
            Ok(tl) => tl,
            Err(e) => {
                self.status = format!("Add timeline: {e}");
                return;
            }
        };
        if let Err(e) = case.save_annotations(&tl, &snap, Some(&idx)) {
            self.status = format!("Save annotations: {e}");
            return;
        }
        // Investigator's notes live in the standalone prefs store, keyed by the
        // source path; save_annotations doesn't touch them. Copy them into the
        // case DB for the new timeline so the conversion keeps them, preserving
        // order and the done flag.
        for kind in [NoteKind::Artifact, NoteKind::Time] {
            for n in self.prefs.notes(&src, kind) {
                if let Ok(added) = case.add_note(tl.id, note_kind_str(kind), &n.text) {
                    if n.done {
                        let _ = case.set_note_done(added.id, true);
                    }
                }
            }
        }
        self.set_case(case);
        self.open_timeline_with_index(tl, idx);
    }

    /// Add a timeline to the open case: pick a CSV, copy it into the case
    /// folder, register and open it.
    fn add_timeline_to_case(&mut self) {
        if self.case.is_none() {
            self.status = "Open or create a case first.".to_string();
            return;
        }
        // No format filter: a timeline may be any delimited text file
        // (.csv, .tsv, .txt, .timeline, no extension...), so the open dialog
        // must let any file through rather than grey non-CSV names out.
        let Some(path) = rfd::FileDialog::new().pick_file() else {
            return;
        };
        let db_path = self.case.as_ref().unwrap().path().to_string();
        let dest = match import_into_case(&db_path, &path.display().to_string()) {
            Ok(d) => d,
            Err(e) => {
                self.status = format!("Import timeline: {e}");
                return;
            }
        };
        let idx = match Index::open(&dest, None) {
            Ok(i) => i,
            Err(e) => {
                self.status = format!("Open {dest}: {e}");
                return;
            }
        };
        let name = file_stem(&dest);
        let tl = match self.case.as_mut().unwrap().add_timeline(&name, &idx, &now_rfc3339()) {
            Ok(tl) => tl,
            Err(e) => {
                self.status = format!("Add timeline: {e}");
                return;
            }
        };
        self.refresh_case_timelines();
        self.open_timeline_with_index(tl, idx);
    }

    /// Open a registered timeline by (re)indexing its source file.
    fn open_timeline(&mut self, tl: TimelineMeta) {
        match Index::open(&tl.source_path, None) {
            Ok(idx) => self.open_timeline_with_index(tl, idx),
            Err(e) => self.status = format!("Open {}: {e}", tl.source_path),
        }
    }

    /// Load a timeline's annotations from the case onto an already-opened index
    /// and show it. Mirrors Go's openTimelineWithIndex.
    fn open_timeline_with_index(&mut self, tl: TimelineMeta, idx: Index) {
        // Save the outgoing timeline's annotations first (switch = autosave).
        self.autosave_case();
        // Carry the current mode forward, but never land on Read-only — a
        // freshly opened timeline should be ready to annotate.
        let start_mode = match self.session.mode() {
            Mode::ReadOnly => Mode::Investigator,
            m => m,
        };
        let mut session = Session::new(&tl.source_path);
        if let Some(case) = &self.case {
            match case.load_annotations(tl.id) {
                Ok(snap) => session.load_snapshot(snap),
                Err(e) => self.status = format!("Load annotations: {e}"),
            }
        }
        session.set_mode(start_mode);
        let display = tl.display_headers.clone();
        let comment = self
            .case
            .as_ref()
            .and_then(|c| c.timeline_comment(tl.id).ok())
            .unwrap_or_default();
        let source = tl.source_path.clone();
        self.cur_timeline = Some(tl);
        self.master_mode = false;
        self.master_entries.clear();
        self.install_index(idx, session, &source);
        self.col_names = display;
        self.timeline_comment = comment;
        self.refresh_case_timelines();
    }

    /// Read-only, time-sorted view of every tagged row across the case.
    /// Mirrors Go's showMasterTimeline + masterGrid.
    fn show_master_timeline(&mut self) {
        if self.case.is_none() {
            return;
        }
        self.autosave_case();
        let entries = match self.case.as_ref().unwrap().master() {
            Ok(e) => e,
            Err(e) => {
                self.status = format!("Master timeline: {e}");
                return;
            }
        };
        let (headers, records) = master_grid(&entries);
        let idx = Index::new_memory(headers, records);
        let mut session = Session::new("(master)");
        // The master session is built fresh, so it starts with only the default
        // palette and no row tags. Seed it from the case: first the shared
        // palette (so custom tags like ioc:case list in the sidebar with their
        // real colour), then each row's tags (so the counts are right and rows
        // tint). Seeding needs a writable mode; lock it read-only afterwards.
        session.set_mode(Mode::Investigator);
        if let Some(case) = self.case.as_ref() {
            if let Ok(defs) = case.tag_defs() {
                for d in &defs {
                    let _ = session.define_tag(&d.name, &d.color);
                }
            }
        }
        for (i, e) in entries.iter().enumerate() {
            for t in &e.tags {
                let _ = session.add_tag(i, t);
            }
        }
        session.set_mode(Mode::ReadOnly);
        let n = entries.len();
        self.master_entries = entries;
        self.cur_timeline = None;
        self.install_index(idx, session, "(master)");
        self.master_mode = true;
        self.annot_cols = false; // master carries its own fixed columns
        self.col_names.clear();
        self.status = if n == 0 {
            "Master timeline: no tagged rows yet. Tag rows and save to populate it.".to_string()
        } else {
            format!("Master timeline: {n} tagged rows across the case")
        };
    }

    /// Open the source timeline behind a master-view row, at that row.
    fn open_master_source(&mut self, view_pos: usize) {
        if !self.master_mode || self.case.is_none() {
            return;
        }
        if view_pos >= self.rows.len() {
            return;
        }
        let master = self.rows[view_pos];
        let Some(entry) = self.master_entries.get(master).cloned() else { return };
        let tl = match self.case.as_ref().unwrap().timeline(entry.timeline_id) {
            Ok(tl) => tl,
            Err(e) => {
                self.status = format!("{e}");
                return;
            }
        };
        self.open_timeline(tl);
        if !self.master_mode {
            // Scroll to the originating row (best effort; may be filtered out).
            if let Some(pos) = self.rows.iter().position(|&m| m == entry.row) {
                self.want_scroll_to = Some(pos);
                self.selected.clear();
                self.selected.insert(entry.row);
            }
        }
    }

    /// Save the open timeline's annotations to the case, if any. Called on
    /// timeline switch and on explicit Save. No-op in standalone/master mode.
    fn autosave_case(&mut self) {
        if self.master_mode {
            return;
        }
        let (Some(case), Some(tl)) = (self.case.as_mut(), self.cur_timeline.as_ref()) else {
            return;
        };
        let snap = self.session.snapshot();
        let idx_ref = self.idx.as_deref();
        if let Err(e) = case.save_annotations(tl, &snap, idx_ref) {
            self.status = format!("Save annotations: {e}");
        } else {
            self.session.mark_saved();
        }
    }

    /// Tear down the open view and show the "add a timeline" placeholder. Used
    /// when a case opens with no timeline loaded. Mirrors Go's clearView.
    fn clear_view(&mut self) {
        self.idx = None;
        self.session = Session::new("");
        self.rows.clear();
        self.cur_timeline = None;
        self.master_mode = false;
        self.master_entries.clear();
        self.col_names.clear();
        self.selected.clear();
        self.sel_anchor = None;
        self.editing = Editing::None;
        self.path.clear();
        self.status = "Add a timeline from the Case panel to begin.".to_string();
    }

    /// Open the rename-columns editor for the current case timeline, seeding
    /// the buffer with current display names.
    fn open_rename_columns(&mut self) {
        if self.case.is_none() || self.cur_timeline.is_none() || self.master_mode {
            return;
        }
        let Some(idx) = &self.idx else { return };
        let headers = idx.headers().to_vec();
        self.rename_cols_buf = headers
            .iter()
            .enumerate()
            .map(|(i, h)| {
                self.col_names
                    .get(i)
                    .filter(|n| !n.is_empty())
                    .cloned()
                    .unwrap_or_else(|| h.clone())
            })
            .collect();
        self.rename_cols_open = true;
    }

    /// Left-sidebar Case section (R7): create/open controls with no case; the
    /// master view, add action and every timeline once a case is open. The open
    /// timeline (or master view) is marked. Mirrors Go's refreshCaseSection.
    fn case_section(&mut self, ui: &mut egui::Ui) {
        enum CaseAction {
            New,
            Open,
            CreateWithCurrent,
            Master,
            Add,
            Rename,
            OpenTimeline(TimelineMeta),
        }
        let mut action: Option<CaseAction> = None;

        if self.case.is_none() {
            if ui.button("New case\u{2026}").clicked() {
                action = Some(CaseAction::New);
            }
            if ui.button("Open case\u{2026}").clicked() {
                action = Some(CaseAction::Open);
            }
            if self.idx.is_some() && !self.master_mode {
                if ui.button("Create case with this timeline\u{2026}").clicked() {
                    action = Some(CaseAction::CreateWithCurrent);
                }
            } else {
                ui.weak("Open a case to work across several timelines.");
            }
        } else {
            let name = case_display_name(self.case.as_ref().unwrap().path());
            ui.label(egui::RichText::new(name).italics());
            if ui
                .selectable_label(self.master_mode, "\u{2605} Master timeline")
                .clicked()
            {
                action = Some(CaseAction::Master);
            }
            if ui.button("Add timeline\u{2026}").clicked() {
                action = Some(CaseAction::Add);
            }
            ui.separator();
            if self.case_timelines.is_empty() {
                ui.weak("(no timelines yet)");
            }
            for tl in &self.case_timelines {
                let current = self
                    .cur_timeline
                    .as_ref()
                    .map(|c| c.id == tl.id)
                    .unwrap_or(false)
                    && !self.master_mode;
                let label = if current {
                    format!("\u{2022} {}", tl.name)
                } else {
                    tl.name.clone()
                };
                if ui.selectable_label(current, label).clicked() {
                    action = Some(CaseAction::OpenTimeline(tl.clone()));
                }
            }
            if self.cur_timeline.is_some() && !self.master_mode {
                ui.separator();
                if ui.button("Rename columns\u{2026}").clicked() {
                    action = Some(CaseAction::Rename);
                }
            }
        }

        match action {
            Some(CaseAction::New) => self.new_case(),
            Some(CaseAction::Open) => self.open_case(),
            Some(CaseAction::CreateWithCurrent) => self.create_case_with_current(),
            Some(CaseAction::Master) => self.show_master_timeline(),
            Some(CaseAction::Add) => self.add_timeline_to_case(),
            Some(CaseAction::Rename) => self.open_rename_columns(),
            Some(CaseAction::OpenTimeline(tl)) => self.open_timeline(tl),
            None => {}
        }
    }

    /// Rename-columns editor (R7): one text box per source column, stored as
    /// the timeline's display headers so matching names merge in the master
    /// view. Mirrors Go's renameColumns.
    fn rename_columns_window(&mut self, ctx: &egui::Context) {
        let headers = self.idx.as_ref().map(|i| i.headers().to_vec()).unwrap_or_default();
        if self.rename_cols_buf.len() != headers.len() {
            self.rename_cols_buf = headers.clone();
        }
        let mut open = true;
        let mut save = false;
        let mut cancel = false;
        let title = format!(
            "Rename columns \u{2014} {}",
            self.cur_timeline.as_ref().map(|t| t.name.as_str()).unwrap_or("")
        );
        egui::Window::new(title)
            .collapsible(false)
            .resizable(true)
            .default_size([480.0, 520.0])
            .open(&mut open)
            .show(ctx, |ui| {
                ui.label(
                    "Rename columns so they match across timelines; columns sharing a name \
                     merge into one column in the master view. Blank falls back to the \
                     original name.",
                );
                ui.separator();
                egui::ScrollArea::vertical().max_height(380.0).show(ui, |ui| {
                    for (i, h) in headers.iter().enumerate() {
                        ui.horizontal(|ui| {
                            ui.label(format!("{h} \u{27A1}"));
                            ui.add(
                                egui::TextEdit::singleline(&mut self.rename_cols_buf[i])
                                    .desired_width(220.0),
                            );
                        });
                    }
                });
                ui.separator();
                ui.horizontal(|ui| {
                    if ui.button("Save").clicked() {
                        save = true;
                    }
                    if ui.button("Cancel").clicked() {
                        cancel = true;
                    }
                });
            });

        if cancel {
            open = false;
        }
        if save {
            // Blank entries fall back to the source header.
            let names: Vec<String> = headers
                .iter()
                .enumerate()
                .map(|(i, h)| {
                    let n = self.rename_cols_buf[i].trim();
                    if n.is_empty() { h.clone() } else { n.to_string() }
                })
                .collect();
            if let (Some(case), Some(tl)) = (self.case.as_mut(), self.cur_timeline.as_mut()) {
                match case.set_column_names(tl.id, &names) {
                    Ok(()) => {
                        tl.display_headers = names.clone();
                        self.col_names = names;
                        self.refresh_case_timelines();
                        self.status = "Renamed columns.".to_string();
                    }
                    Err(e) => self.status = format!("Rename columns: {e}"),
                }
            }
            self.rename_cols_open = false;
        } else if !open {
            self.rename_cols_open = false;
        }
    }
}

impl eframe::App for TlxApp {
    fn ui(&mut self, ui: &mut egui::Ui, _frame: &mut eframe::Frame) {
        let ctx = ui.ctx().clone();
        ctx.set_visuals(if self.dark {
            egui::Visuals::dark()
        } else {
            egui::Visuals::light()
        });
        handle_keys(self, &ctx);

        // Drive any background filter/IOC scan: install its result when it
        // lands, else keep repainting so the progress bar animates.
        if self.poll_job() {
            ctx.request_repaint();
        }

        egui::Panel::top("toolbar").show_inside(ui, |ui| {
            ui.horizontal_wrapped(|ui| {
                if ui.button("Open Timeline\u{2026}").clicked() {
                    // No format filter: see add_timeline_to_case. A timeline
                    // can be any delimited text file, so don't grey out
                    // non-CSV names in the open dialog.
                    if let Some(path) = rfd::FileDialog::new().pick_file() {
                        self.open_path(&path.display().to_string());
                    }
                }
                // Case menu (R7): new/open always available; add-timeline and
                // master only once a case is open.
                ui.menu_button("Case", |ui| {
                    if ui.button("New case\u{2026}").clicked() {
                        self.new_case();
                        ui.close();
                    }
                    if ui.button("Open case\u{2026}").clicked() {
                        self.open_case();
                        ui.close();
                    }
                    if self.case.is_some() {
                        ui.separator();
                        if ui.button("\u{2605} Master timeline").clicked() {
                            self.show_master_timeline();
                            ui.close();
                        }
                        if ui.button("Add timeline\u{2026}").clicked() {
                            self.add_timeline_to_case();
                            ui.close();
                        }
                        let can_rename = self.cur_timeline.is_some() && !self.master_mode;
                        if ui.add_enabled(can_rename, egui::Button::new("Rename columns\u{2026}")).clicked() {
                            self.open_rename_columns();
                            ui.close();
                        }
                    } else if self.idx.is_some() && !self.master_mode {
                        ui.separator();
                        if ui.button("Create case with this timeline\u{2026}").clicked() {
                            self.create_case_with_current();
                            ui.close();
                        }
                    }
                });
                ui.separator();

                // Mode selector (G19)
                let mut mode = self.session.mode();
                egui::ComboBox::from_id_salt("mode")
                    .selected_text(format!("{mode}"))
                    .show_ui(ui, |ui| {
                        ui.selectable_value(&mut mode, Mode::ReadOnly, "Read-only");
                        ui.selectable_value(&mut mode, Mode::Investigator, "Investigator");
                        ui.selectable_value(&mut mode, Mode::WorldWrite, "World-write");
                    });
                if mode != self.session.mode() {
                    self.session.set_mode(mode);
                }
                ui.separator();

                ui.label("Filter:");
                let resp = ui.add(
                    egui::TextEdit::singleline(&mut self.filter_text)
                        .id(egui::Id::new("filter_box"))
                        .desired_width(220.0),
                );
                if self.focus_filter {
                    resp.request_focus();
                    self.focus_filter = false;
                }
                if resp.lost_focus() && ctx.input(|i| i.key_pressed(egui::Key::Enter)) {
                    self.apply_filter();
                }
                if ui.button("Apply").clicked() {
                    self.apply_filter();
                }
                if ui.button("Clear all filters").clicked() {
                    self.clear_filters();
                }
                if ui.checkbox(&mut self.tagged_only, "Tagged only").changed() {
                    self.apply_filter();
                }
                if ui.toggle_value(&mut self.show_filter_row, "Filter row").clicked() {}
                ui.toggle_value(&mut self.wrap, "Wrap")
                    .on_hover_text("Word-wrap cells so wide content stays on screen");
                self.columns_menu(ui);
                ui.separator();
                if ui.button("Save").clicked() {
                    self.save_session();
                }
                ui.menu_button("Export\u{2026}", |ui| {
                    if ui.button("Current view").clicked() {
                        self.export_view();
                        ui.close();
                    }
                    if ui.button("To Timeline\u{2026}").clicked() {
                        self.open_timeline_export_builder();
                        ui.close();
                    }
                });
                // Theme toggle (R3). Label shows the mode it switches to,
                // matching the Fyne button.
                let label = if self.dark { "\u{2600} Light" } else { "\u{1F319} Dark" };
                if ui.button(label).clicked() {
                    self.dark = !self.dark;
                    self.prefs.set_dark_theme(self.dark);
                }
                // Right-aligned: show/hide the right side panel. Sits at the far
                // right of the toolbar, above the panel it controls.
                ui.with_layout(egui::Layout::right_to_left(egui::Align::Center), |ui| {
                    ui.toggle_value(&mut self.show_right_dock, "Side panel")
                        .on_hover_text("Show or hide the right side panel");
                });
            });
            // While a background scan runs, show a live progress bar and a
            // Cancel button in place of the plain status line.
            if let Some(job) = &self.job {
                let done = job.progress.load(Ordering::Relaxed);
                let frac = if job.total > 0 {
                    (done as f32 / job.total as f32).clamp(0.0, 1.0)
                } else {
                    0.0
                };
                let verb = match job.kind {
                    JobKind::Filter | JobKind::FilterClear => "Filtering",
                    JobKind::Ioc | JobKind::IocAll => "Scanning IOCs",
                    JobKind::Export => "Exporting",
                };
                ui.horizontal(|ui| {
                    ui.add(
                        egui::ProgressBar::new(frac)
                            .desired_width(240.0)
                            .text(format!("{verb} {}%", (frac * 100.0) as u32)),
                    );
                    if ui.button("Cancel").clicked() {
                        self.cancel_job();
                        self.status = "Cancelled.".to_string();
                    }
                });
            } else {
                ui.label(&self.status);
            }
        });

        // Left sidebar (G11): saved views placeholder, tag checklist, IOC lists.
        if self.show_left_sidebar {
            egui::Panel::left("sidebar").resizable(true).default_size(200.0).show_inside(ui, |ui| {
                egui::CollapsingHeader::new("Case").default_open(true).show(ui, |ui| {
                    self.case_section(ui);
                });
                egui::CollapsingHeader::new("Tags").default_open(true).show(ui, |ui| {
                    self.tags_sidebar(ui);
                });
                egui::CollapsingHeader::new("Saved views").default_open(true).show(ui, |ui| {
                    self.saved_views_section(ui);
                });
                egui::CollapsingHeader::new("IOC lists").default_open(true).show(ui, |ui| {
                    self.ioc_sidebar(ui);
                });
            });
        }

        // Right dock (G10): each panel renders inline unless it has been
        // floated out (R6). The notes panel only exists once a file is open.
        if self.show_right_dock {
            let mut panels = vec![DockPanel::Filter, DockPanel::Details];
            // Notes and the timeline comment attach to an editable timeline:
            // a standalone file or a case timeline, never the master view.
            if !self.path.is_empty() && !self.master_mode {
                panels.push(DockPanel::Notes);
            }
            panels.push(DockPanel::TimelineComment);
            let inline: Vec<DockPanel> =
                panels.into_iter().filter(|p| !self.floating.contains(p)).collect();
            if !inline.is_empty() {
                egui::Panel::right("dock").resizable(true).default_size(320.0).show_inside(ui, |ui| {
                    // Each pane gets its own vertically-resizable sub-panel so
                    // the dividers between them can be dragged; the last pane
                    // fills whatever height is left.
                    let (last, rest) = inline.split_last().unwrap();
                    for &panel in rest {
                        egui::Panel::top(egui::Id::new(("dock-pane", panel)))
                            .resizable(true)
                            .min_size(60.0)
                            .default_size(180.0)
                            .show_inside(ui, |ui| {
                                self.dock_pane(ui, panel);
                            });
                    }
                    egui::CentralPanel::default().show_inside(ui, |ui| {
                        self.dock_pane(ui, *last);
                    });
                });
            }
        }

        // Transient tag/comment/cell editor popup.
        self.editor_window(&ctx);

        if self.show_goto {
            self.goto_window(&ctx);
        }
        if self.show_ioc {
            self.ioc_window(&ctx);
        }
        if self.rename_cols_open {
            self.rename_columns_window(&ctx);
        }
        self.timeline_export_window(&ctx);
        self.value_filter_window(&ctx);
        self.notes_export_window(&ctx);
        self.confirm_tag_delete_window(&ctx);
        self.render_popouts(&ctx);
        self.render_candidate_review(&ctx);
        self.render_floating_panels(&ctx);

        egui::CentralPanel::default().show_inside(ui, |ui| {
            self.grid(ui);
        });

        // Standalone files autosave their sidecar the moment an annotation
        // changes (tags, comments, cell edits), the same way IOC lists persist
        // immediately. Done after the frame's UI so a change made this frame is
        // flushed this frame. Inside a case, annotations go to the case DB via
        // autosave_case on timeline switch, so the sidecar is left alone.
        let standalone = !self.master_mode && !(self.case.is_some() && self.cur_timeline.is_some());
        if standalone && self.session.dirty() {
            if let Err(e) = self.session.save() {
                self.status = format!("Autosave failed: {e}");
            }
        }
    }
}

impl TlxApp {
    /// The "Columns" toolbar dropdown: a checkbox per column (Tags/Comment then
    /// the data columns, matching the grid, minus the always-on # gutter).
    /// Unchecking hides a column from the grid; "Show all" clears every hide.
    /// All columns start checked.
    fn columns_menu(&mut self, ui: &mut egui::Ui) {
        let Some(idx) = self.idx.clone() else { return };
        let n_data = idx.headers().len();
        let mut cols: Vec<ColumnRef> = Vec::new();
        if self.annot_cols {
            cols.push(COL_TAGS);
            cols.push(COL_COMMENT);
        }
        cols.extend((0..n_data as ColumnRef).filter(|&c| !self.adopted.has(c)));

        ui.menu_button("Columns", |ui| {
            ui.set_min_width(160.0);
            if ui.button("Show all").clicked() {
                self.hidden_cols.clear();
            }
            ui.separator();
            egui::ScrollArea::vertical().max_height(420.0).show(ui, |ui| {
                for c in cols {
                    let name = match c {
                        COL_TAGS => "Tags".to_string(),
                        COL_COMMENT => "Comment".to_string(),
                        c => self.col_display_name(c as usize),
                    };
                    let mut visible = !self.hidden_cols.contains(&c);
                    if ui.checkbox(&mut visible, name).changed() {
                        if visible {
                            self.hidden_cols.remove(&c);
                        } else {
                            self.hidden_cols.insert(c);
                        }
                    }
                }
            });
        });
    }

    fn grid(&mut self, ui: &mut egui::Ui) {
        // Split-borrow disjoint fields so idx (needs &mut for its row cache)
        // and session (tags/comments/tints) can both be read in the row loop.
        let TlxApp {
            idx,
            session,
            rows,
            selected,
            col_filters,
            col_filter_tags,
            col_filter_comment,
            col_value_filters,
            hidden_cols,
            applied_filter,
            sort_col,
            sort_desc,
            show_filter_row,
            row_height,
            tag_opacity,
            wrap,
            wrap_heights,
            wrap_sig,
            want_scroll_to,
            path,
            annot_cols,
            col_names,
            master_mode,
            adopted,
            inline_comment,
            inline_comment_focus,
            menu_added,
            ..
        } = self;
        let Some(idx) = idx.as_ref() else {
            ui.vertical_centered(|ui| {
                ui.add_space(40.0);
                ui.label("Add a timeline from the Case panel to begin.");
            });
            return;
        };
        let headers = idx.headers().to_vec();
        let n_data = headers.len();
        let n_rows = rows.len();
        let highlight = applied_filter.clone();
        let sort_col = *sort_col;
        let sort_desc = *sort_desc;
        let show_filter_row = *show_filter_row;
        let tag_opacity = *tag_opacity;
        let wrap = *wrap;
        let annot_cols = *annot_cols;
        let master_mode = *master_mode;
        let adopted = *adopted;
        let col_names = col_names.clone();
        let notes_on = !path.is_empty() && !master_mode;
        // Configured tags for the Tags filter-row dropdown. The box offers only
        // defined tags rather than free text, so you can't filter on a tag that
        // was never set up.
        let known_tags = session.known_tags();

        // Column order mirrors the Fyne app's buildColumns: a row-number
        // gutter, the Tags and Comment virtual columns, then the data columns.
        // The master view carries its own Time/Timeline/Tags/Comment columns,
        // so the virtual annotation columns are suppressed there.
        let mut columns: Vec<ColumnRef> = if annot_cols {
            vec![COL_ROW_NUM, COL_TAGS, COL_COMMENT]
        } else {
            vec![COL_ROW_NUM]
        };
        // Skip any data column adopted as the session's tags/comment: its
        // content already shows in the virtual Tags/Comment columns.
        columns.extend((0..n_data as ColumnRef).filter(|&c| !adopted.has(c)));
        // Drop columns the user hid via the Columns dropdown. The # gutter is
        // never hidden, so it is exempt.
        columns.retain(|c| *c == COL_ROW_NUM || !hidden_cols.contains(c));
        let col_title = |refc: ColumnRef| -> String {
            match refc {
                COL_ROW_NUM => "#".to_string(),
                COL_TAGS => "\u{270F} Tags".to_string(),
                COL_COMMENT => "\u{270F} Comment".to_string(),
                c => col_names
                    .get(c as usize)
                    .filter(|n| !n.is_empty())
                    .cloned()
                    .unwrap_or_else(|| headers.get(c as usize).cloned().unwrap_or_default()),
            }
        };

        use egui_extras::{Column, TableBuilder};

        // In wrap mode each row must be tall enough for its tallest wrapped
        // cell. Estimating that from the cell text needs a pass over every row,
        // so it's cached and only rebuilt when the view (rows or columns)
        // changes - gated by a cheap fingerprint rather than redone each frame.
        // The estimate rounds up and runs slightly generous: overshooting adds
        // a little blank space, undershooting would clip, which wrap exists to
        // avoid. Widths track the Column::initial values below; resizing a
        // column narrower than its initial can clip until the view refreshes.
        if wrap {
            use std::hash::{Hash, Hasher};
            let mut hsh = std::collections::hash_map::DefaultHasher::new();
            n_rows.hash(&mut hsh);
            columns.hash(&mut hsh);
            rows.first().hash(&mut hsh);
            rows.last().hash(&mut hsh);
            rows.get(n_rows / 2).hash(&mut hsh);
            let sig = hsh.finish();
            if *wrap_sig != sig || wrap_heights.len() != n_rows {
                let col_px = |refc: ColumnRef| -> f32 {
                    match refc {
                        COL_ROW_NUM => 64.0,
                        COL_TAGS => 150.0,
                        COL_COMMENT => 220.0,
                        _ => 240.0,
                    }
                };
                wrap_heights.clear();
                wrap_heights.reserve(n_rows);
                for &master in rows.iter() {
                    let rec = idx.row(master).unwrap_or_default();
                    let mut max_lines = 1usize;
                    for &refc in &columns {
                        if refc == COL_ROW_NUM {
                            continue;
                        }
                        let text = match refc {
                            COL_TAGS => session.tags(master).join(", "),
                            COL_COMMENT => session.comment(master),
                            c => session
                                .cell_override(master, c as usize)
                                .unwrap_or_else(|| rec.get(c as usize).cloned().unwrap_or_default()),
                        };
                        if text.is_empty() {
                            continue;
                        }
                        let usable = (col_px(refc) - 16.0).max(20.0);
                        let mut lines = 0usize;
                        for seg in text.split('\n') {
                            let chars = seg.chars().count().max(1) as f32;
                            lines += ((chars * 8.0) / usable).ceil() as usize;
                        }
                        max_lines = max_lines.max(lines.max(1));
                    }
                    let height = (max_lines.min(200) as f32) * 16.0 + 6.0;
                    wrap_heights.push(height.max(*row_height));
                }
                *wrap_sig = sig;
            }
        }

        let header_h = if show_filter_row { 48.0 } else { 24.0 };
        let mut clicked_col: Option<ColumnRef> = None;
        let mut col_filter_changed = false;
        // Header right-click menu actions, applied after the table borrow ends.
        let mut header_sort: Option<(ColumnRef, bool)> = None; // (col, desc)
        let mut header_empty: Option<(ColumnRef, bool)> = None; // (col, want_empty)
        let mut header_clear: Option<ColumnRef> = None;
        // A header funnel was clicked: open the per-column value picker for this
        // column. Gathered + opened after the table borrow ends (needs &mut self).
        let mut open_value_filter: Option<ColumnRef> = None;
        let mut row_clicks: Vec<(usize, egui::Modifiers)> = Vec::new();
        let mut dbl_click_cell: Option<(usize, ColumnRef)> = None;
        // Pending cell-menu / annotation actions, applied after the borrow ends.
        let mut filter_5min: Option<(usize, String)> = None; // (data col, cell text)
        let mut add_artifact: Option<String> = None;
        // Artifacts added from the sticky cell menu this frame (the menu stays
        // open, so several can arrive at once). (cell master, column, text).
        let mut sticky_add: Vec<(usize, ColumnRef, String)> = Vec::new();
        let mut add_time: Option<String> = None;
        let mut open_tag: Option<usize> = None;
        let mut open_comment: Option<usize> = None;
        // Comment cell clicked: start inline editing on this master row.
        let mut open_inline_comment: Option<usize> = None;
        // The inline comment edit box lost focus or took Enter: commit it.
        let mut commit_inline_comment = false;
        // Right-clicking a row that isn't part of the current selection makes it
        // the selection (view position), so a context action targets what the
        // user pointed at rather than a stale selection elsewhere.
        let mut ctx_select: Option<usize> = None;
        let sel_count = selected.len();
        // Tag toggled from the inline context menu: (tag name, add?). Applied to
        // the current selection (or clicked row) after the table borrow ends.
        let mut tag_toggle: Option<(String, bool)> = None;
        // Open the cell pop-out to pick a substring (real text selection works
        // there, unlike a right-clicked Label).
        let mut open_popout: Option<(usize, ColumnRef)> = None;
        // Open the full candidate-review window: (cell text, all candidates).
        let mut open_review: Option<(String, Vec<crate::engine::extract::Candidate>)> = None;

        // egui_extras' Table scrolls only vertically (it hard-codes the
        // horizontal axis off). Wrap it in a horizontal ScrollArea so a wide
        // timeline can pan side to side; the table keeps its own vertical
        // scroll (and row virtualisation, which matters for multi-million-row
        // files). auto_shrink is off so the area fills the panel. The table's
        // vertical scrollbar rides the right edge of the column block rather
        // than the viewport, which is the price of horizontal panning here.
        egui::ScrollArea::horizontal().auto_shrink([false, false]).show(ui, |ui| {
        let mut builder = TableBuilder::new(ui)
            .striped(true)
            .resizable(true)
            .sense(egui::Sense::click())
            .cell_layout(egui::Layout::left_to_right(egui::Align::Center));
        for &refc in &columns {
            let col = match refc {
                COL_ROW_NUM => Column::initial(64.0).at_least(40.0).clip(true),
                COL_TAGS => Column::initial(150.0).at_least(60.0).clip(true),
                COL_COMMENT => Column::initial(220.0).at_least(60.0).clip(true),
                // A fixed, resizable initial rather than Column::auto(): a
                // single very wide cell (plaso 'message' can be thousands of
                // chars) makes an auto column measure an enormous width, which
                // both swamps the viewport and drives egui_extras' column
                // sizing to a NaN width that panics ("Negative width ... NaN").
                _ => Column::initial(240.0).at_least(60.0).clip(true),
            };
            builder = builder.column(col);
        }
        if let Some(p) = want_scroll_to.take() {
            builder = builder.scroll_to_row(p, Some(egui::Align::Center));
        }
        builder
            .header(header_h, |mut header| {
                for &refc in &columns {
                    header.col(|ui| {
                        ui.vertical(|ui| {
                            let marker = if sort_col == Some(refc) {
                                if sort_desc { " \u{2B07}" } else { " \u{2B06}" }
                            } else {
                                ""
                            };
                            let btn = ui.button(format!("{}{marker}", col_title(refc)));
                            if btn.clicked() {
                                clicked_col = Some(refc);
                            }
                            // Right-click a header for sort and quick emptiness
                            // filters (data and annotation columns; not #).
                            if refc != COL_ROW_NUM {
                                btn.context_menu(|ui| {
                                    if ui.button("Sort ascending").clicked() {
                                        header_sort = Some((refc, false));
                                        ui.close();
                                    }
                                    if ui.button("Sort descending").clicked() {
                                        header_sort = Some((refc, true));
                                        ui.close();
                                    }
                                    ui.separator();
                                    if ui.button("Filter: not empty").clicked() {
                                        header_empty = Some((refc, false));
                                        ui.close();
                                    }
                                    if ui.button("Filter: empty").clicked() {
                                        header_empty = Some((refc, true));
                                        ui.close();
                                    }
                                    if ui.button("Clear this column's filter").clicked() {
                                        header_clear = Some(refc);
                                        ui.close();
                                    }
                                });
                            }
                            // Per-column quick filter: data columns plus the
                            // virtual Tags and Comment columns. Tags is a
                            // dropdown of configured tags; the rest are free text.
                            if show_filter_row {
                                if refc == COL_TAGS {
                                    let current = if col_filter_tags.is_empty() {
                                        "(any)".to_string()
                                    } else {
                                        col_filter_tags.clone()
                                    };
                                    egui::ComboBox::from_id_salt("tag-filter")
                                        .selected_text(current)
                                        .width(f32::INFINITY)
                                        .show_ui(ui, |ui| {
                                            if ui
                                                .selectable_label(col_filter_tags.is_empty(), "(any)")
                                                .clicked()
                                            {
                                                col_filter_tags.clear();
                                                col_filter_changed = true;
                                            }
                                            for t in &known_tags {
                                                if ui
                                                    .selectable_label(col_filter_tags == t, t)
                                                    .clicked()
                                                {
                                                    *col_filter_tags = t.clone();
                                                    col_filter_changed = true;
                                                }
                                            }
                                        });
                                } else {
                                    let box_ref: Option<&mut String> = match refc {
                                        COL_COMMENT => Some(col_filter_comment),
                                        c if c >= 0 => Some(&mut col_filters[c as usize]),
                                        _ => None,
                                    };
                                    if let Some(buf) = box_ref {
                                        ui.horizontal(|ui| {
                                            // Funnel opens the per-column value
                                            // picker. Filled when a selection is
                                            // active on this column, hollow
                                            // otherwise. Painted, not a glyph -
                                            // see funnel_icon.
                                            let active = col_value_filters
                                                .get(&refc)
                                                .is_some_and(|v| !v.is_empty());
                                            if funnel_icon(ui, active)
                                                .on_hover_text("Filter by value\u{2026}")
                                                .clicked()
                                            {
                                                open_value_filter = Some(refc);
                                            }
                                            let box_resp = ui.add(
                                                egui::TextEdit::singleline(buf)
                                                    .hint_text("filter\u{2026}")
                                                    .desired_width(f32::INFINITY),
                                            );
                                            if box_resp.lost_focus()
                                                && ui.input(|i| i.key_pressed(egui::Key::Enter))
                                            {
                                                col_filter_changed = true;
                                            }
                                        });
                                    }
                                }
                            }
                        });
                    });
                }
            })
            .body(|body| {
                // One heterogeneous-rows path for both modes: wrap feeds the
                // per-row heights computed above, plain mode feeds a uniform
                // height. This keeps a single row-render closure.
                let heights: Box<dyn Iterator<Item = f32>> = if wrap {
                    Box::new(wrap_heights.iter().copied())
                } else {
                    Box::new(std::iter::repeat(*row_height).take(n_rows))
                };
                body.heterogeneous_rows(heights, |mut row| {
                    let view_pos = row.index();
                    let master = rows[view_pos];
                    row.set_selected(selected.contains(&master));
                    let rec = idx.row(master).unwrap_or_default();
                    // Top tag's colour tints the whole row at the global tag
                    // opacity, matching the Fyne rowTintColor wash.
                    let tint = session.row_color(master).and_then(|hex| parse_hex_tint(&hex, tag_opacity));
                    for &refc in &columns {
                        let mut dbl = false;
                        // Is this the Comment cell currently being edited inline?
                        let editing_inline = refc == COL_COMMENT
                            && inline_comment.as_ref().is_some_and(|(m, _)| *m == master);
                        // A click-sensing overlay over the whole cell, captured
                        // out of the closure. It sits on top of the label so a
                        // right-click lands even when the pointer is over text -
                        // a sensing Label would otherwise swallow it.
                        let mut hit: Option<egui::Response> = None;
                        let (_cell_rect, cell_resp0) = row.col(|ui| {
                            if let Some(tint) = tint {
                                ui.painter().rect_filled(ui.clip_rect(), 0.0, tint);
                            }
                            if editing_inline {
                                // Editable box in place of the label; committed
                                // after the table loop (session is borrowed here).
                                if let Some((_, buf)) = inline_comment.as_mut() {
                                    let r = ui.add(
                                        egui::TextEdit::singleline(buf)
                                            .desired_width(f32::INFINITY),
                                    );
                                    let just_focused = *inline_comment_focus;
                                    if *inline_comment_focus {
                                        r.request_focus();
                                        *inline_comment_focus = false;
                                    }
                                    // A press anywhere outside the box commits the
                                    // edit. lost_focus() alone is unreliable here:
                                    // in a virtualised egui_extras table the
                                    // TextEdit's auto id can shift between frames,
                                    // so it may never hold keyboard focus to lose
                                    // (Enter still works - that key check is
                                    // global). Skip the frame we grab focus, when
                                    // the opening click may still read as pressed.
                                    let press_outside = !just_focused
                                        && ui.ctx().input(|i| {
                                            i.pointer.any_pressed()
                                                && i.pointer
                                                    .interact_pos()
                                                    .is_some_and(|p| !r.rect.contains(p))
                                        });
                                    if r.lost_focus()
                                        || ui.input(|i| i.key_pressed(egui::Key::Enter))
                                        || press_outside
                                    {
                                        commit_inline_comment = true;
                                    }
                                }
                                return;
                            }
                            let owned;
                            let text: &str = match refc {
                                COL_ROW_NUM => {
                                    owned = (master + 1).to_string();
                                    &owned
                                }
                                COL_TAGS => {
                                    owned = session.tags(master).join(", ");
                                    &owned
                                }
                                COL_COMMENT => {
                                    owned = session.comment(master);
                                    &owned
                                }
                                c => {
                                    let c = c as usize;
                                    if let Some(v) = session.cell_override(master, c) {
                                        owned = v;
                                        &owned
                                    } else {
                                        rec.get(c).map(|s| s.as_str()).unwrap_or("")
                                    }
                                }
                            };
                            // The Label does not sense clicks. All interaction
                            // hangs off an explicit sensing region over the whole
                            // cell rect (added below), so a right-click lands even
                            // when the pointer is over text - a sensing Label
                            // would otherwise swallow it.
                            // Wrap mode lets a cell flow onto several lines (the
                            // row grew to fit); otherwise clip to one line and
                            // offer the full text on hover.
                            let mk = |label: egui::Label| if wrap { label.wrap() } else { label.truncate() };
                            let resp = if highlight.is_empty() {
                                ui.add(mk(egui::Label::new(text)))
                            } else {
                                ui.add(mk(egui::Label::new(highlighted_job(text, &highlight, ui.style()))))
                            };
                            if !wrap && text.len() > 40 {
                                resp.on_hover_text(text);
                            }
                            // Sensing overlay over this cell's full rect, drawn
                            // last so it wins hit-testing over the label's text.
                            hit = Some(ui.interact(
                                ui.max_rect(),
                                ui.id().with(("cell-hit", master, refc)),
                                egui::Sense::click(),
                            ));
                        });
                        // While editing inline, skip the overlay/click handling so
                        // the TextEdit keeps focus and the caret works normally.
                        if editing_inline {
                            continue;
                        }
                        let cell_resp = match hit {
                            Some(h) => cell_resp0.union(h),
                            None => cell_resp0,
                        };
                        // Clicking the Comment cell opens an inline editable
                        // box; the Tags cell and data cells select the row (the
                        // overlay consumes the click, so row-level selection
                        // won't fire for it). Tags are edited from the cell's
                        // right-click menu or the Tags sidebar, not a left
                        // click. Double-click pops out.
                        if cell_resp.clicked() {
                            match refc {
                                COL_COMMENT => {
                                    open_inline_comment = Some(master);
                                }
                                _ => row_clicks.push((view_pos, ui_modifiers(&cell_resp))),
                            }
                        }
                        if cell_resp.double_clicked() {
                            dbl = true;
                        }
                        // The menu hangs off the whole cell response, not the
                        // inner Label, so it still opens on an empty Tags/Comment
                        // cell (a zero-width Label has no hit area).
                        let cell_text: String = match refc {
                            COL_ROW_NUM => (master + 1).to_string(),
                            COL_TAGS => session.tags(master).join(", "),
                            COL_COMMENT => session.comment(master),
                            c => session
                                .cell_override(master, c as usize)
                                .unwrap_or_else(|| rec.get(c as usize).cloned().unwrap_or_default()),
                        };
                        if cell_resp.secondary_clicked() && !selected.contains(&master) {
                            ctx_select = Some(view_pos);
                        }
                        let is_time = refc >= 0 && timecol::parse_time(&cell_text).is_some();
                        // CloseOnClickOutside (not the default CloseOnClick) so an
                        // Add-artifact click leaves the menu open for the next
                        // pick; actions that should dismiss call ui.close().
                        egui::Popup::context_menu(&cell_resp)
                            .close_behavior(egui::PopupCloseBehavior::CloseOnClickOutside)
                            .show(|ui| {
                            let targets = if selected.contains(&master) {
                                sel_count.max(1)
                            } else {
                                1
                            };
                            let suffix = if targets > 1 {
                                format!(" ({targets} rows)")
                            } else {
                                String::new()
                            };
                            // Notes first - this is the action reached for most,
                            // and keeping it at the top means it never falls off
                            // the bottom of a tall menu. We split the cell into
                            // candidates - detected IOCs (IP, hash, path, domain,
                            // email, GUID) first, then whitespace tokens, then the
                            // whole cell - so the string the user wants is usually
                            // one click, no sub-window. "Pick substring..." is the
                            // escape hatch for an odd multi-word span.
                            if (refc >= 0 || refc == COL_COMMENT) && notes_on {
                                let cands = crate::engine::extract::candidates(&cell_text);
                                if !cands.is_empty() {
                                    // Which candidates have been added from this
                                    // open menu (ticked off, like the review
                                    // window). Reset when the menu targets a new
                                    // cell.
                                    let fresh = menu_added
                                        .as_ref()
                                        .map(|(m, c, _)| *m != master || *c != refc)
                                        .unwrap_or(true);
                                    if fresh {
                                        *menu_added = Some((master, refc, BTreeSet::new()));
                                    }
                                    let added_set = &mut menu_added.as_mut().unwrap().2;
                                    // Cap the inline list so a token-heavy cell
                                    // doesn't produce a menu taller than the
                                    // screen; the rest go to a review window.
                                    const MENU_MAX: usize = 20;
                                    ui.label("Add to Artifacts:");
                                    for c in cands.iter().take(MENU_MAX) {
                                        if added_set.contains(&c.text) {
                                            ui.horizontal(|ui| {
                                                ui.weak("\u{2714}");
                                                ui.weak(&c.text);
                                            });
                                            continue;
                                        }
                                        let label = if c.kind == "whole cell" || c.kind == "token" {
                                            c.text.clone()
                                        } else {
                                            format!("{}  ({})", c.text, c.kind)
                                        };
                                        // No ui.close(): the menu stays open so
                                        // several candidates can be added in one
                                        // pass. The clicked item ticks off.
                                        if ui.button(label).clicked() {
                                            sticky_add.push((master, refc, c.text.clone()));
                                            added_set.insert(c.text.clone());
                                        }
                                    }
                                    if cands.len() > MENU_MAX
                                        && ui
                                            .button(format!(
                                                "\u{2026} {} more \u{2014} review all\u{2026}",
                                                cands.len() - MENU_MAX
                                            ))
                                            .on_hover_text("Open the full candidate list in a window")
                                            .clicked()
                                    {
                                        open_review = Some((cell_text.clone(), cands.clone()));
                                        ui.close();
                                    }
                                    if is_time && ui.button("Add cell to Times").clicked() {
                                        add_time = Some(cell_text.trim().to_string());
                                        ui.close();
                                    }
                                    if ui
                                        .button("Pick substring\u{2026}")
                                        .on_hover_text("Open the cell to select an arbitrary span")
                                        .clicked()
                                    {
                                        open_popout = Some((master, refc));
                                        ui.close();
                                    }
                                    ui.separator();
                                }
                            }
                            if is_time && ui.button("Filter \u{00B1}5 min around this time").clicked() {
                                filter_5min = Some((refc as usize, cell_text.clone()));
                                ui.close();
                            }
                            // Defined tags inline: tick to add, untick to remove.
                            // Shows the clicked row's state; applies to the whole
                            // target set. A long palette goes in a scroll area so
                            // it can't push the rest of the menu off-screen.
                            if !master_mode {
                                if is_time {
                                    ui.separator();
                                }
                                ui.label(if targets > 1 {
                                    format!("Tags ({targets} rows)")
                                } else {
                                    "Tags".to_string()
                                });
                                let row_tags = session.tags(master);
                                egui::ScrollArea::vertical()
                                    .max_height(220.0)
                                    .show(ui, |ui| {
                                        for t in &known_tags {
                                            let mut on = row_tags.iter().any(|x| x == t);
                                            if ui.checkbox(&mut on, t).clicked() {
                                                tag_toggle = Some((t.clone(), on));
                                                ui.close();
                                            }
                                        }
                                    });
                                if ui.button("More tags / new\u{2026}").clicked() {
                                    open_tag = Some(master);
                                    ui.close();
                                }
                                ui.separator();
                                if ui.button(format!("\u{1F4AC} Comment{suffix}\u{2026}")).clicked() {
                                    open_comment = Some(master);
                                    ui.close();
                                }
                            }
                        });
                        if dbl {
                            dbl_click_cell = Some((master, refc));
                        }
                    }
                    let resp = row.response();
                    if resp.clicked() {
                        row_clicks.push((view_pos, ui_modifiers(&resp)));
                    }
                });
            });
        });

        for (pos, mods) in row_clicks {
            self.on_row_click(pos, mods);
        }
        // A right-click outside the selection makes that row the selection, so
        // the tag/comment action below targets it.
        if let Some(pos) = ctx_select {
            self.on_row_click(pos, egui::Modifiers::default());
        }
        if let Some(master) = open_tag {
            self.editing = Editing::Tag(master);
            self.edit_buf.clear();
        }
        if let Some(master) = open_comment {
            self.editing = Editing::Comment(master);
            // Prefill with the clicked row's comment only when editing a single
            // row; for a bulk comment start blank so one message applies to all.
            self.edit_buf = if self.selected.len() > 1 {
                String::new()
            } else {
                self.session.comment(master)
            };
        }
        // Commit the inline comment edit (Enter, or a press outside the box).
        // Write only when the text actually changed. Bump ReadOnly->Investigator
        // for the write, then restore, same as elsewhere. Runs before the open
        // handler below so clicking straight from one Comment cell to another
        // commits the first edit instead of discarding it.
        if commit_inline_comment {
            if let Some((master, text)) = self.inline_comment.take() {
                if self.session.comment(master) != text {
                    let prev = self.session.mode();
                    if prev == Mode::ReadOnly {
                        self.session.set_mode(Mode::Investigator);
                    }
                    let _ = self.session.set_comment(master, &text);
                    if prev == Mode::ReadOnly {
                        self.session.set_mode(prev);
                    }
                }
            }
        }
        // Start inline comment editing: load the current comment into the buffer
        // the Comment cell renders as a TextEdit next frame.
        if let Some(master) = open_inline_comment {
            let cur = self.session.comment(master);
            self.inline_comment = Some((master, cur));
            self.inline_comment_focus = true;
        }
        // Inline tag toggle from the context menu: add/remove across the
        // selection (or the single clicked row). Bump ReadOnly->Investigator so
        // the write lands, then restore the mode, same as edit_palette.
        if let Some((tag, add)) = tag_toggle {
            let targets: Vec<usize> = if self.selected.is_empty() {
                Vec::new()
            } else {
                self.selected.iter().copied().collect()
            };
            let prev = self.session.mode();
            if prev == Mode::ReadOnly {
                self.session.set_mode(Mode::Investigator);
            }
            for &m in &targets {
                let _ = if add {
                    self.session.add_tag(m, &tag)
                } else {
                    self.session.remove_tag(m, &tag)
                };
            }
            if prev == Mode::ReadOnly {
                self.session.set_mode(prev);
            }
            self.status = format!(
                "{} tag '{}' on {} row(s).",
                if add { "Added" } else { "Removed" },
                tag,
                targets.len()
            );
            self.apply_filter();
        }
        if let Some((master, refc)) = open_popout {
            self.open_cell_popout(master, refc);
        }
        if let Some((cell_text, cands)) = open_review {
            let n = cands.len();
            let snippet: String = cell_text.chars().take(48).collect();
            self.candidate_review = Some(CandidateReview {
                title: format!("Artifact candidates ({n}) \u{2014} {snippet}"),
                added: vec![false; n],
                cands,
                open: true,
                pinned: true,
            });
        }
        if let Some((col, text)) = filter_5min {
            self.filter_plus_minus_5min(col, &text);
        }
        if let Some(text) = add_artifact {
            if self.note_add(NoteKind::Artifact, &text) {
                self.status = "Added to Artifacts.".to_string();
            }
        }
        // Artifacts added from the sticky cell menu (menu stays open, so this
        // can carry several in one frame).
        if !sticky_add.is_empty() {
            let n = sticky_add
                .iter()
                .filter(|(_, _, text)| self.note_add(NoteKind::Artifact, text))
                .count();
            if n > 0 {
                self.status = format!("Added {n} to Artifacts.");
            }
        }
        if let Some(text) = add_time {
            if self.note_add(NoteKind::Time, &text) {
                self.status = "Added to Times.".to_string();
            }
        }
        if let Some((master, refc)) = dbl_click_cell {
            if self.master_mode {
                // In the master view, double-click jumps to the source row.
                if let Some(pos) = self.rows.iter().position(|&m| m == master) {
                    self.open_master_source(pos);
                }
            } else {
                self.open_cell_popout(master, refc);
            }
        }
        if let Some(c) = clicked_col {
            self.apply_sort(c);
        }
        if let Some((col, desc)) = header_sort {
            self.apply_sort_dir(col, desc);
        }
        if let Some((col, want_empty)) = header_empty {
            self.apply_emptiness_filter(col, want_empty);
        }
        if let Some(col) = header_clear {
            self.clear_column_filter(col);
        }
        if let Some(col) = open_value_filter {
            self.open_value_picker(col);
        }
        if col_filter_changed {
            self.apply_filter();
        }
    }

    /// Open the per-column value picker for `col`: gather the distinct cell
    /// values currently visible in that column (capped), seed the working
    /// selection from any filter already applied to the column, and show the
    /// editor window. Toggling it on the same column closes it.
    fn open_value_picker(&mut self, col: ColumnRef) {
        if self.value_popup.as_ref().is_some_and(|p| p.col == col) {
            self.value_popup = None;
            return;
        }
        let Some(idx) = self.idx.clone() else { return };
        // Cap the distinct set so a view with millions of unique cells can't
        // build an unusable list (or thrash memory). Once the cap is hit the
        // scan stops and the UI flags the list as partial.
        const CAP: usize = 2000;
        let mut set: BTreeSet<String> = BTreeSet::new();
        let mut truncated = false;
        for &master in &self.rows {
            let val = match col {
                COL_COMMENT => self.session.comment(master),
                c if c >= 0 => idx
                    .row(master)
                    .ok()
                    .and_then(|r| r.get(c as usize).cloned())
                    .unwrap_or_default(),
                _ => String::new(),
            };
            if !set.contains(&val) {
                if set.len() >= CAP {
                    truncated = true;
                    break;
                }
                set.insert(val);
            }
        }
        let checked: BTreeSet<String> = self
            .col_value_filters
            .get(&col)
            .map(|v| v.iter().cloned().collect())
            .unwrap_or_default();
        let title = match col {
            COL_COMMENT => "Comment".to_string(),
            c => self.col_display_name(c as usize),
        };
        self.value_popup = Some(ValueFilterPopup {
            col,
            title,
            distinct: set.into_iter().collect(),
            truncated,
            checked,
            search: String::new(),
        });
    }

    /// Render the per-column value picker window, if open. Ticked values are the
    /// working selection; Apply commits them as the column's value filter and
    /// re-runs the filter, Clear drops the column's filter entirely. Closing the
    /// window (X) discards the in-progress selection.
    fn value_filter_window(&mut self, ctx: &egui::Context) {
        if self.value_popup.is_none() {
            return;
        }
        let mut open = true;
        let mut apply = false;
        let mut do_clear = false;
        let popup = self.value_popup.as_mut().unwrap();
        egui::Window::new(format!("Filter: {}", popup.title))
            .id(egui::Id::new("value-filter-window"))
            .collapsible(false)
            .resizable(true)
            .default_width(300.0)
            .open(&mut open)
            .show(ctx, |ui| {
                ui.add(
                    egui::TextEdit::singleline(&mut popup.search)
                        .hint_text("search values\u{2026}")
                        .desired_width(f32::INFINITY),
                );
                ui.horizontal(|ui| {
                    if ui.button("All").clicked() {
                        for v in &popup.distinct {
                            popup.checked.insert(v.clone());
                        }
                    }
                    if ui.button("None").clicked() {
                        popup.checked.clear();
                    }
                    ui.label(format!("{} selected", popup.checked.len()));
                });
                if popup.truncated {
                    ui.weak(format!(
                        "First {} values; narrow the view to see more.",
                        popup.distinct.len()
                    ));
                }
                ui.separator();
                let needle = popup.search.to_lowercase();
                egui::ScrollArea::vertical().max_height(340.0).auto_shrink([false, true]).show(ui, |ui| {
                    for v in &popup.distinct {
                        if !needle.is_empty() && !v.to_lowercase().contains(&needle) {
                            continue;
                        }
                        let label = if v.is_empty() { "(blank)".to_string() } else { v.clone() };
                        let mut on = popup.checked.contains(v);
                        if ui.checkbox(&mut on, label).changed() {
                            if on {
                                popup.checked.insert(v.clone());
                            } else {
                                popup.checked.remove(v);
                            }
                        }
                    }
                });
                ui.separator();
                ui.horizontal(|ui| {
                    if ui.button("Apply").clicked() {
                        apply = true;
                    }
                    if ui.button("Clear filter").clicked() {
                        do_clear = true;
                    }
                });
            });

        if !open {
            self.value_popup = None;
            return;
        }
        if apply {
            let popup = self.value_popup.take().unwrap();
            let col = popup.col;
            let vals: Vec<String> = popup.checked.into_iter().collect();
            if vals.is_empty() {
                self.col_value_filters.remove(&col);
            } else {
                self.col_value_filters.insert(col, vals);
            }
            self.apply_filter();
        } else if do_clear {
            let col = self.value_popup.take().unwrap().col;
            self.col_value_filters.remove(&col);
            self.apply_filter();
        }
    }

    /// Sort by one column in an explicit direction (header right-click), as
    /// opposed to apply_sort's click-to-toggle.
    fn apply_sort_dir(&mut self, col: ColumnRef, desc: bool) {
        if self.idx.is_none() {
            return;
        }
        self.sort_col = Some(col);
        self.sort_desc = desc;
        self.sort_keys = vec![SortKey { col, desc }];
        self.spawn_filter(JobKind::Filter);
    }

    /// Add an empty / non-empty condition on one column from the header menu.
    /// Emptiness is a regexp `\S` test (any non-whitespace char): matching it
    /// keeps non-empty cells, negating it keeps empty ones. Replaces any
    /// existing builder condition on the same column so repeated menu picks
    /// don't stack.
    fn apply_emptiness_filter(&mut self, col: ColumnRef, want_empty: bool) {
        if self.idx.is_none() {
            return;
        }
        self.builder_conds.retain(|r| r.column != col);
        self.builder_conds.push(CondRow {
            column: col,
            values: r"\S".to_string(),
            all: false,
            regexp: true,
            neg: want_empty,
        });
        self.apply_filter();
    }

    /// Clear every filter touching one column: its quick-filter box plus any
    /// builder condition scoped to it. Leaves the free-text query and other
    /// columns alone.
    fn clear_column_filter(&mut self, col: ColumnRef) {
        match col {
            COL_TAGS => self.col_filter_tags.clear(),
            COL_COMMENT => self.col_filter_comment.clear(),
            c if c >= 0 && (c as usize) < self.col_filters.len() => {
                self.col_filters[c as usize].clear();
            }
            _ => {}
        }
        self.col_value_filters.remove(&col);
        if self.value_popup.as_ref().is_some_and(|p| p.col == col) {
            self.value_popup = None;
        }
        self.builder_conds.retain(|r| r.column != col);
        self.apply_filter();
    }

    /// Open a floating detail window for one cell (R4). Editable when it is a
    /// comment, or any cell under World-write — same rule as the grid.
    fn open_cell_popout(&mut self, master: usize, col: ColumnRef) {
        let headers = self.idx.as_ref().map(|i| i.headers().to_vec()).unwrap_or_default();
        let (title_col, text) = match col {
            COL_TAGS => ("Tags".to_string(), self.session.tags(master).join(", ")),
            COL_COMMENT => ("Comment".to_string(), self.session.comment(master)),
            COL_ROW_NUM => ("#".to_string(), (master + 1).to_string()),
            c if c >= 0 => {
                let name = headers.get(c as usize).cloned().unwrap_or_default();
                let val = self
                    .session
                    .cell_override(master, c as usize)
                    .or_else(|| self.idx.as_mut().and_then(|i| i.row(master).ok()).and_then(|r| r.get(c as usize).cloned()))
                    .unwrap_or_default();
                (name, val)
            }
            _ => return,
        };
        let editable = col == COL_COMMENT
            || (col >= 0 && self.session.mode() == Mode::WorldWrite);
        let id = self.next_popout_id;
        self.next_popout_id += 1;
        self.popouts.push(CellPopout {
            id,
            master,
            col,
            title: format!("{title_col} \u{2014} row {}", master + 1),
            text,
            editable,
            pinned: true, // pop-outs open pinned on top, matching the Fyne default
            open: true,
            dirty: false,
            last_sel: String::new(),
        });
    }

    /// Render every open cell pop-out as its own OS window (egui deferred
    /// viewport). A pinned window asks the backend for an always-on-top level.
    /// Edits commit to the session when the window closes.
    fn render_popouts(&mut self, ctx: &egui::Context) {
        if self.popouts.is_empty() {
            return;
        }
        // Collect edits out of the viewport closures, then apply.
        let mut commits: Vec<(usize, ColumnRef, String)> = Vec::new();
        // Note additions requested from a pop-out's "Add selection" buttons.
        let mut note_adds: Vec<(NoteKind, String)> = Vec::new();
        let notes_on = !self.path.is_empty() && !self.master_mode;
        for p in &mut self.popouts {
            let vp_id = egui::ViewportId::from_hash_of(("cell-popout", p.id));
            let mut builder = egui::ViewportBuilder::default()
                .with_title(&p.title)
                .with_inner_size([520.0, 360.0]);
            if p.pinned {
                builder = builder.with_always_on_top();
            }
            // Immediate viewports take an FnMut, so the closure can mutate the
            // pop-out's fields directly through these borrows.
            let title = p.title.clone();
            let editable = p.editable;
            let (master, col) = (p.master, p.col);
            let text_ref = &mut p.text;
            let pinned_ref = &mut p.pinned;
            let open_ref = &mut p.open;
            let dirty_ref = &mut p.dirty;
            let last_sel_ref = &mut p.last_sel;
            ctx.show_viewport_immediate(vp_id, builder, |ui, _class| {
                let vctx = ui.ctx().clone();
                ui.horizontal(|ui| {
                    ui.strong(&title);
                    if ui.toggle_value(pinned_ref, "\u{1F4CC} Pin").changed() {
                        let level = if *pinned_ref {
                            egui::WindowLevel::AlwaysOnTop
                        } else {
                            egui::WindowLevel::Normal
                        };
                        vctx.send_viewport_cmd(egui::ViewportCommand::WindowLevel(level));
                    }
                });
                ui.separator();
                // "Add selection" works off a live TextEdit even for a
                // read-only cell: the box lets the user highlight the exact
                // substring they want, and we read its selection back from the
                // TextEditOutput. A plain selectable Label can't give us that —
                // egui doesn't expose per-widget selection text.
                egui::ScrollArea::both().show(ui, |ui| {
                    let out = egui::TextEdit::multiline(text_ref)
                        .desired_width(f32::INFINITY)
                        .desired_rows(14)
                        .interactive(editable || notes_on)
                        .show(ui);
                    if out.response.changed() && editable {
                        *dirty_ref = true;
                    }
                    // Keep the last non-empty selection: a right-click collapses
                    // the live one before the context menu can read it.
                    if let Some(range) = out.cursor_range {
                        let r = range.as_sorted_char_range();
                        let (start, end) = (r.start.0, r.end.0);
                        if start < end {
                            *last_sel_ref =
                                text_ref.chars().skip(start).take(end - start).collect();
                        }
                    }
                    // Right-click the text to add the highlighted substring (or
                    // the whole cell) to Artifacts/Times - the same menu as the
                    // Details pane.
                    if notes_on {
                        let whole = text_ref.trim().to_string();
                        let sel = last_sel_ref.trim().to_string();
                        let payload = if sel.is_empty() { whole.clone() } else { sel.clone() };
                        out.response.context_menu(|ui| {
                            let enabled = !payload.is_empty();
                            let a = if sel.is_empty() {
                                "Add cell to Artifacts".to_string()
                            } else {
                                format!("Add \"{sel}\" to Artifacts")
                            };
                            if ui.add_enabled(enabled, egui::Button::new(a)).clicked() {
                                note_adds.push((NoteKind::Artifact, payload.clone()));
                                ui.close();
                            }
                            let t = if sel.is_empty() {
                                "Add cell to Times".to_string()
                            } else {
                                format!("Add \"{sel}\" to Times")
                            };
                            if ui.add_enabled(enabled, egui::Button::new(t)).clicked() {
                                note_adds.push((NoteKind::Time, payload.clone()));
                                ui.close();
                            }
                        });
                    }
                });
                if notes_on {
                    ui.separator();
                    ui.horizontal(|ui| {
                        // Buttons mirror the right-click menu for discoverability:
                        // add the highlighted substring if there is one, else the
                        // whole cell.
                        let sel = last_sel_ref.trim().to_string();
                        let payload =
                            if sel.is_empty() { text_ref.trim().to_string() } else { sel.clone() };
                        let label = if sel.is_empty() {
                            "Add cell to Artifacts"
                        } else {
                            "Add selection to Artifacts"
                        };
                        if ui.add_enabled(!payload.is_empty(), egui::Button::new(label)).clicked() {
                            note_adds.push((NoteKind::Artifact, payload.clone()));
                        }
                        let tlabel = if sel.is_empty() {
                            "Add cell to Times"
                        } else {
                            "Add selection to Times"
                        };
                        if ui.add_enabled(!payload.is_empty(), egui::Button::new(tlabel)).clicked() {
                            note_adds.push((NoteKind::Time, payload));
                        }
                    });
                }
                if vctx.input(|i| i.viewport().close_requested()) {
                    *open_ref = false;
                }
            });
            if !*open_ref && *dirty_ref && editable {
                commits.push((master, col, text_ref.clone()));
            }
        }
        // Apply edits from any window that just closed.
        for (master, col, text) in commits {
            let res = match col {
                COL_COMMENT => self.session.set_comment(master, &text),
                c if c >= 0 => self.session.set_cell(master, c as usize, &text),
                _ => Ok(()),
            };
            if let Err(e) = res {
                self.status = format!("{e}");
            }
        }
        for (kind, text) in note_adds {
            if self.note_add(kind, &text) {
                self.status = match kind {
                    NoteKind::Artifact => "Added to Artifacts.".to_string(),
                    NoteKind::Time => "Added to Times.".to_string(),
                };
            }
        }
        if self.popouts.iter().any(|p| !p.open) {
            self.popouts.retain(|p| p.open);
            self.apply_filter(); // refresh grid so edits/tints show
        }
    }

    /// Render the full artifact-candidate review window, when open. Lists every
    /// candidate the cell produced with an Add button each; added ones are
    /// ticked off so a long list can be worked through without double-adding.
    fn render_candidate_review(&mut self, ctx: &egui::Context) {
        let Some(review) = self.candidate_review.as_mut() else { return };
        let vp_id = egui::ViewportId::from_hash_of("candidate-review");
        let mut builder = egui::ViewportBuilder::default()
            .with_title(&review.title)
            .with_inner_size([480.0, 560.0]);
        if review.pinned {
            builder = builder.with_always_on_top();
        }
        // Candidates to add this frame (index into review.cands).
        let mut to_add: Vec<usize> = Vec::new();
        let mut add_all = false;
        let title = review.title.clone();
        let cands = &review.cands;
        let added = &mut review.added;
        let open_ref = &mut review.open;
        let pinned_ref = &mut review.pinned;
        ctx.show_viewport_immediate(vp_id, builder, |ui, _class| {
            let vctx = ui.ctx().clone();
            ui.horizontal(|ui| {
                ui.strong(&title);
                ui.with_layout(egui::Layout::right_to_left(egui::Align::Center), |ui| {
                    if ui.toggle_value(pinned_ref, "\u{1F4CC} Pin").changed() {
                        let level = if *pinned_ref {
                            egui::WindowLevel::AlwaysOnTop
                        } else {
                            egui::WindowLevel::Normal
                        };
                        vctx.send_viewport_cmd(egui::ViewportCommand::WindowLevel(level));
                    }
                    let remaining = added.iter().filter(|a| !**a).count();
                    if ui
                        .add_enabled(remaining > 0, egui::Button::new(format!("Add all remaining ({remaining})")))
                        .clicked()
                    {
                        add_all = true;
                    }
                });
            });
            ui.separator();
            egui::ScrollArea::vertical().show(ui, |ui| {
                for (i, c) in cands.iter().enumerate() {
                    ui.horizontal(|ui| {
                        if added[i] {
                            ui.weak("\u{2714}");
                            ui.weak(&c.text);
                        } else {
                            if ui.button("Add").clicked() {
                                to_add.push(i);
                            }
                            ui.label(&c.text);
                            if c.kind != "whole cell" && c.kind != "token" {
                                ui.weak(format!("({})", c.kind));
                            }
                        }
                    });
                }
            });
            if vctx.input(|i| i.viewport().close_requested()) {
                *open_ref = false;
            }
        });

        // Apply additions (collected to avoid borrowing self inside the closure).
        let picks: Vec<(usize, String)> = if add_all {
            review
                .cands
                .iter()
                .enumerate()
                .filter(|(i, _)| !review.added[*i])
                .map(|(i, c)| (i, c.text.clone()))
                .collect()
        } else {
            to_add.iter().map(|&i| (i, review.cands[i].text.clone())).collect()
        };
        let closing = !review.open;
        let mut added = 0usize;
        for (i, text) in &picks {
            if self.note_add(NoteKind::Artifact, text) {
                added += 1;
            }
            if let Some(r) = self.candidate_review.as_mut() {
                r.added[*i] = true;
            }
        }
        if added > 0 {
            self.status = format!("Added {added} to Artifacts.");
        }
        if closing {
            self.candidate_review = None;
        }
    }

    /// Render one inline dock pane: a title row with a pop-out button, then the
    /// scrollable body. Wrapped in its own resizable sub-panel by the caller so
    /// each pane's height can be dragged independently.
    fn dock_pane(&mut self, ui: &mut egui::Ui, panel: DockPanel) {
        ui.horizontal(|ui| {
            ui.strong(panel.title());
            ui.with_layout(egui::Layout::right_to_left(egui::Align::Center), |ui| {
                if ui.small_button("\u{2197} Pop out").clicked() {
                    self.floating.insert(panel);
                    self.pinned_panels.insert(panel);
                }
            });
        });
        ui.separator();
        egui::ScrollArea::vertical()
            .id_salt(("dock-pane-scroll", panel))
            .auto_shrink([false, false])
            .show(ui, |ui| {
                self.panel_body(ui, panel);
            });
    }

    /// Details pane: every field of the selected row. Each value is a
    /// read-only text box so its selection is retrievable (egui won't hand back
    /// a Label's highlighted substring), and right-clicking a value adds the
    /// highlighted text — or the whole field if nothing's selected — to the
    /// Artifacts or Times list.
    fn details_body(&mut self, ui: &mut egui::Ui) {
        let Some(master) = self.selected_master() else {
            ui.weak("Select a row.");
            return;
        };
        let Some(idx) = self.idx.as_ref() else { return };
        let headers = idx.headers().to_vec();
        let Ok(rec) = idx.row(master) else { return };
        let notes_on = !self.path.is_empty() && !self.master_mode;

        // Data-column fields.
        let fields: Vec<(String, String)> = headers
            .iter()
            .enumerate()
            .map(|(c, name)| (name.clone(), rec.get(c).cloned().unwrap_or_default()))
            .collect();

        let mut note_add: Option<(NoteKind, String)> = None;
        // Last highlighted substring, kept across frames so a right-click
        // (which collapses the live selection) still has it to add.
        let mut sel_store = self.details_sel.take();
        for (i, (name, val)) in fields.iter().enumerate() {
            Self::details_field_row(ui, i, name, val, notes_on, &mut note_add, &mut sel_store);
        }

        // Tags and Comment: editable from here regardless of mode (Investigator
        // and World-write both allow annotations; a Read-only session is bumped
        // for the edit by the editor windows). The Edit buttons open the same
        // bulk-aware editors as the grid's right-click menu.
        ui.separator();
        let tags = self.session.tags(master).join(", ");
        let comment = self.session.comment(master);
        let read_only = self.session.mode() == Mode::ReadOnly;
        let mut open_tag_master: Option<usize> = None;
        let mut open_comment_master: Option<usize> = None;
        if !self.master_mode {
            ui.horizontal(|ui| {
                ui.strong("Tags:");
                if ui.add_enabled(!read_only, egui::Button::new("\u{270F} Edit")).clicked() {
                    open_tag_master = Some(master);
                }
            });
            Self::details_field_row(ui, usize::MAX - 1, "", &tags, notes_on, &mut note_add, &mut sel_store);
            ui.horizontal(|ui| {
                ui.strong("Comment:");
                if ui.add_enabled(!read_only, egui::Button::new("\u{270F} Edit")).clicked() {
                    open_comment_master = Some(master);
                }
            });
            Self::details_field_row(ui, usize::MAX, "", &comment, notes_on, &mut note_add, &mut sel_store);
            if read_only {
                ui.weak("Switch to Investigator or World-write to edit tags/comments.");
            }
        } else {
            ui.strong(format!("Tags: {tags}"));
            ui.label(format!("Comment: {comment}"));
        }

        if let Some(m) = open_tag_master {
            self.editing = Editing::Tag(m);
            self.edit_buf.clear();
        }
        if let Some(m) = open_comment_master {
            self.editing = Editing::Comment(m);
            self.edit_buf = if self.selected.len() > 1 {
                String::new()
            } else {
                self.session.comment(m)
            };
        }
        // Keep the captured selection for the next frame (the right-click
        // menu reads it after the caret has collapsed).
        self.details_sel = sel_store;
        if let Some((kind, text)) = note_add {
            if self.note_add(kind, &text) {
                self.status = match kind {
                    NoteKind::Artifact => "Added to Artifacts.".to_string(),
                    NoteKind::Time => "Added to Times.".to_string(),
                };
            }
        }
    }

    /// One Details-pane field: a read-only box (so its selection is
    /// retrievable) with a right-click menu to add the highlighted text, or the
    /// whole value, to Artifacts/Times. `name` empty renders value-only.
    fn details_field_row(
        ui: &mut egui::Ui,
        salt: usize,
        name: &str,
        val: &str,
        notes_on: bool,
        note_add: &mut Option<(NoteKind, String)>,
        sel_store: &mut Option<(usize, String)>,
    ) {
        ui.horizontal_wrapped(|ui| {
            if !name.is_empty() {
                ui.strong(format!("{name}:"));
            }
            // Bind to a &str: egui's TextBuffer impl for &str is immutable
            // (typing is a no-op) but still selectable, so the value can't be
            // edited yet a substring can be highlighted to add.
            let mut text: &str = val;
            let out = egui::TextEdit::singleline(&mut text)
                .id_salt(("details-field", salt))
                .desired_width(f32::INFINITY)
                .show(ui);
            // Read the live selection and remember the last non-empty one for
            // this field. A right-click moves the caret and collapses the
            // selection on the same frame the context menu opens, so reading
            // cursor_range inside the menu gives nothing - we must capture it
            // here, every frame, and fall back to the stored value.
            if let Some(r) = out.cursor_range {
                let r = r.as_sorted_char_range();
                if r.end.0 > r.start.0 {
                    let s: String =
                        val.chars().skip(r.start.0).take(r.end.0 - r.start.0).collect();
                    *sel_store = Some((salt, s));
                }
            }
            if notes_on {
                out.response.context_menu(|ui| {
                    // The highlighted substring for this field, if any, else the
                    // whole value.
                    let sel = match sel_store {
                        Some((s, txt)) if *s == salt && !txt.trim().is_empty() => txt.trim().to_string(),
                        _ => val.trim().to_string(),
                    };
                    let enabled = !sel.is_empty();
                    let whole = sel == val.trim();
                    let art_label =
                        if whole { "Add field to Artifacts".to_string() } else { format!("Add \"{sel}\" to Artifacts") };
                    let time_label =
                        if whole { "Add field to Times".to_string() } else { format!("Add \"{sel}\" to Times") };
                    if ui.add_enabled(enabled, egui::Button::new(art_label)).clicked() {
                        *note_add = Some((NoteKind::Artifact, sel.clone()));
                        ui.close();
                    }
                    if ui.add_enabled(enabled, egui::Button::new(time_label)).clicked() {
                        *note_add = Some((NoteKind::Time, sel));
                        ui.close();
                    }
                });
            }
        });
    }

    /// Render one dock panel's content. Shared by the inline dock and the
    /// floating-window path (R6) so a panel behaves identically either way.
    fn panel_body(&mut self, ui: &mut egui::Ui, panel: DockPanel) {
        match panel {
            DockPanel::Filter => self.filter_builder_section(ui),
            DockPanel::Details => self.details_body(ui),
            DockPanel::Notes => self.notes_section(ui),
            DockPanel::TimelineComment => {
                let resp = ui.add(
                    egui::TextEdit::multiline(&mut self.timeline_comment)
                        .desired_rows(4)
                        .desired_width(f32::INFINITY),
                );
                if resp.changed() {
                    self.save_timeline_comment();
                }
            }
        }
    }

    /// Draw every floated dock panel in its own OS window (R6), each with a
    /// dock-back button and an always-on-top toggle. Mirrors internal/gui/dock.go.
    fn render_floating_panels(&mut self, ctx: &egui::Context) {
        if self.floating.is_empty() {
            return;
        }
        let panels: Vec<DockPanel> = self.floating.iter().copied().collect();
        for panel in panels {
            let vp_id = egui::ViewportId::from_hash_of(("dock-float", panel.title()));
            let mut pinned = self.pinned_panels.contains(&panel);
            let mut builder = egui::ViewportBuilder::default()
                .with_title(panel.title())
                .with_inner_size([360.0, 420.0]);
            if pinned {
                builder = builder.with_always_on_top();
            }
            let mut dock_back = false;
            let mut closed = false;
            ctx.show_viewport_immediate(vp_id, builder, |ui, _class| {
                let vctx = ui.ctx().clone();
                ui.horizontal(|ui| {
                    if ui.button("\u{21A9} Dock").clicked() {
                        dock_back = true;
                    }
                    if ui.toggle_value(&mut pinned, "\u{1F4CC} Pin").changed() {
                        let level = if pinned {
                            egui::WindowLevel::AlwaysOnTop
                        } else {
                            egui::WindowLevel::Normal
                        };
                        vctx.send_viewport_cmd(egui::ViewportCommand::WindowLevel(level));
                    }
                });
                ui.separator();
                egui::ScrollArea::vertical().show(ui, |ui| {
                    self.panel_body(ui, panel);
                });
                if vctx.input(|i| i.viewport().close_requested()) {
                    closed = true;
                }
            });
            if pinned {
                self.pinned_panels.insert(panel);
            } else {
                self.pinned_panels.remove(&panel);
            }
            // Closing the window re-docks the panel, matching the Fyne
            // SetCloseIntercept behaviour.
            if dock_back || closed {
                self.floating.remove(&panel);
            }
        }
    }

    /// Detailed filter builder (R5): structured column conditions with an
    /// AND/OR mode, plus the free-text query box and the case-sensitive /
    /// tagged-only toggles. Mirrors internal/gui/filterwin.go. Feeds
    /// FilterSpec.conds, which the engine already compiles and applies.
    fn filter_builder_section(&mut self, ui: &mut egui::Ui) {
        let headers = self.idx.as_ref().map(|i| i.headers().to_vec()).unwrap_or_default();

        ui.label("Column conditions");
        ui.horizontal(|ui| {
            ui.radio_value(&mut self.conds_any, false, "Match all (AND)");
            ui.radio_value(&mut self.conds_any, true, "Match any (OR)");
        });

        let mut delete: Option<usize> = None;
        for (i, cond) in self.builder_conds.iter_mut().enumerate() {
            ui.group(|ui| {
                ui.horizontal(|ui| {
                    ui.label("Column:");
                    let sel = if cond.column == COL_ALL {
                        "Any column".to_string()
                    } else {
                        headers.get(cond.column as usize).cloned().unwrap_or_else(|| "?".to_string())
                    };
                    egui::ComboBox::from_id_salt(("cond_col", i))
                        .selected_text(sel)
                        .show_ui(ui, |ui| {
                            ui.selectable_value(&mut cond.column, COL_ALL, "Any column");
                            for (c, h) in headers.iter().enumerate() {
                                ui.selectable_value(&mut cond.column, c as ColumnRef, h);
                            }
                        });
                    if ui.small_button("\u{00D7}").on_hover_text("Remove condition").clicked() {
                        delete = Some(i);
                    }
                });
                ui.add(
                    egui::TextEdit::multiline(&mut cond.values)
                        .hint_text("one value per line")
                        .desired_rows(2)
                        .desired_width(f32::INFINITY),
                );
                ui.horizontal(|ui| {
                    ui.checkbox(&mut cond.all, "all values");
                    ui.checkbox(&mut cond.regexp, "regex");
                    ui.checkbox(&mut cond.neg, "exclude");
                });
            });
        }
        if let Some(i) = delete {
            self.builder_conds.remove(i);
        }
        if ui.button("+ Add condition").clicked() {
            self.builder_conds.push(CondRow::default());
        }

        ui.separator();
        ui.label("Query");
        ui.add(
            egui::TextEdit::multiline(&mut self.filter_text)
                .hint_text("text, or Field=value AND (tag=bad OR tag=suspicious). /regex/ for regex.")
                .desired_rows(3)
                .desired_width(f32::INFINITY),
        );
        let mut dirty = false;
        if ui.checkbox(&mut self.cased, "Case sensitive").changed() {
            dirty = true;
        }
        if ui.checkbox(&mut self.tagged_only, "Tagged only").changed() {
            dirty = true;
        }
        ui.horizontal(|ui| {
            if ui.button("Apply").clicked() {
                dirty = true;
            }
            if ui.button("Clear all").clicked() {
                self.clear_filters();
                return;
            }
        });
        if dirty {
            self.apply_filter();
        }
    }

    /// Saved views sidebar (G16): list stored views, apply/delete, and capture
    /// the current filter+sort+column layout as a new one. Views are stored
    /// app-wide and matched to a timeline by column title.
    fn saved_views_section(&mut self, ui: &mut egui::Ui) {
        enum ViewAction {
            Apply(SavedView),
            Delete(String),
        }
        let mut action: Option<ViewAction> = None;

        for v in self.prefs.saved_views() {
            ui.horizontal(|ui| {
                if ui.add(egui::Label::new(&v.name).sense(egui::Sense::click())).on_hover_text("Apply").clicked() {
                    action = Some(ViewAction::Apply(v.clone()));
                }
                if ui.small_button("\u{00D7}").on_hover_text("Delete").clicked() {
                    action = Some(ViewAction::Delete(v.name.clone()));
                }
            });
        }
        let mut submit = false;
        ui.horizontal(|ui| {
            let resp = ui.add(
                egui::TextEdit::singleline(&mut self.new_view_name).hint_text("Save current as\u{2026}"),
            );
            if (resp.lost_focus() && ui.input(|i| i.key_pressed(egui::Key::Enter))) || ui.button("+").clicked() {
                submit = true;
            }
        });
        if submit {
            let name = self.new_view_name.trim().to_string();
            if !name.is_empty() {
                let v = self.capture_view(name);
                self.prefs.put_saved_view(v);
                self.new_view_name.clear();
            }
        }

        match action {
            Some(ViewAction::Apply(v)) => self.apply_saved_view(&v),
            Some(ViewAction::Delete(name)) => self.prefs.delete_saved_view(&name),
            None => {}
        }
    }

    /// Snapshot the active filter+sort into a SavedView, storing the sort
    /// column by title so it survives a column-order change.
    fn capture_view(&self, name: String) -> SavedView {
        let headers = self.idx.as_ref().map(|i| i.headers().to_vec()).unwrap_or_default();
        let sort_column = self
            .sort_col
            .and_then(|c| if c >= 0 { headers.get(c as usize).cloned() } else { None })
            .unwrap_or_default();
        let col_filters = self
            .col_filters
            .iter()
            .enumerate()
            .filter(|(_, v)| !v.is_empty())
            .filter_map(|(c, v)| headers.get(c).map(|h| (h.clone(), v.clone())))
            .collect();
        SavedView {
            name,
            query: self.filter_text.clone(),
            cased: self.cased,
            tagged_only: self.tagged_only,
            sort_column,
            sort_desc: self.sort_desc,
            col_filters,
            tags: self.tag_checklist.iter().cloned().collect(),
        }
    }

    /// Apply a saved view: resolve its column titles against the current
    /// headers (unresolved titles are dropped, as the original does), set the
    /// filter inputs and rebuild.
    fn apply_saved_view(&mut self, v: &SavedView) {
        let headers = self.idx.as_ref().map(|i| i.headers().to_vec()).unwrap_or_default();
        let title_to_col = |title: &str| headers.iter().position(|h| h == title);

        self.filter_text = v.query.clone();
        self.cased = v.cased;
        self.tagged_only = v.tagged_only;
        self.tag_checklist = v.tags.iter().cloned().collect();
        for b in self.col_filters.iter_mut() {
            b.clear();
        }
        for (title, val) in &v.col_filters {
            if let Some(c) = title_to_col(title) {
                if c < self.col_filters.len() {
                    self.col_filters[c] = val.clone();
                }
            }
        }
        if !v.col_filters.is_empty() {
            self.show_filter_row = true;
        }
        // Restore sort.
        if let Some(c) = title_to_col(&v.sort_column) {
            self.sort_col = Some(c as ColumnRef);
            self.sort_desc = v.sort_desc;
            self.sort_keys = vec![SortKey { col: c as ColumnRef, desc: v.sort_desc }];
        } else {
            self.sort_col = None;
            self.sort_keys.clear();
        }
        self.apply_filter();
    }

    /// Left-sidebar IOC lists section: each list with a run button, an edit
    /// button that opens the manager focused on it, and a delete button, plus
    /// New and Run all at the bottom. Actions are collected first so the
    /// per-list buttons can borrow `self.ioc_lists` immutably in the loop.
    fn ioc_sidebar(&mut self, ui: &mut egui::Ui) {
        if self.path.is_empty() && self.case.is_none() {
            ui.weak("Open a timeline or case first.");
            return;
        }

        enum IocAction {
            Run(usize),
            Edit(usize),
            Delete(usize),
            New,
            RunAll,
        }
        let mut action: Option<IocAction> = None;

        if self.ioc_lists.is_empty() {
            ui.weak("No IOC lists yet.");
        }
        for (i, l) in self.ioc_lists.iter().enumerate() {
            ui.horizontal(|ui| {
                if ui.small_button("\u{25B6}").on_hover_text("Run this list").clicked() {
                    action = Some(IocAction::Run(i));
                }
                if ui.small_button("\u{270F}").on_hover_text("Edit in the IOC manager").clicked() {
                    action = Some(IocAction::Edit(i));
                }
                if ui.small_button("\u{1F5D1}").on_hover_text("Delete this list").clicked() {
                    action = Some(IocAction::Delete(i));
                }
                ui.label(&l.name);
            });
        }
        ui.separator();
        ui.horizontal(|ui| {
            if ui.button("New").clicked() {
                action = Some(IocAction::New);
            }
            if !self.ioc_lists.is_empty() && ui.button("Run all").clicked() {
                action = Some(IocAction::RunAll);
            }
        });

        match action {
            Some(IocAction::Run(i)) => {
                if let Some(l) = self.ioc_lists.get(i) {
                    let lists = vec![(l.name.clone(), l.body.clone())];
                    self.spawn_ioc(lists, JobKind::Ioc);
                }
            }
            Some(IocAction::Edit(i)) => self.open_ioc_manager_at(i),
            Some(IocAction::Delete(i)) => {
                self.ioc_sel = Some(i);
                self.ioc_delete_selected();
            }
            Some(IocAction::New) => {
                // open_ioc_manager seeds ioc_sel/name/body, then create a fresh
                // list in the manager.
                self.open_ioc_manager();
                self.ioc_create_list();
            }
            Some(IocAction::RunAll) => self.run_all_iocs(),
            None => {}
        }
    }

    fn open_ioc_manager(&mut self) {
        if self.path.is_empty() && self.case.is_none() {
            self.status = "Open a file or case first.".to_string();
            return;
        }
        self.reload_ioc_lists();
        self.ioc_sel = if self.ioc_lists.is_empty() { None } else { Some(0) };
        if let Some(l) = self.ioc_lists.first() {
            self.ioc_name = l.name.clone();
            self.ioc_body = l.body.clone();
        }
        self.show_ioc = true;
    }

    /// Open the IOC manager focused on list `i` (from the sidebar's Edit
    /// button).
    fn open_ioc_manager_at(&mut self, i: usize) {
        self.open_ioc_manager();
        if i < self.ioc_lists.len() {
            self.ioc_sel = Some(i);
            self.ioc_name = self.ioc_lists[i].name.clone();
            self.ioc_body = self.ioc_lists[i].body.clone();
        }
    }

    /// Load the IOC lists from wherever they live: the case DB inside a case,
    /// else the standalone prefs store. Fills ioc_lists and the parallel
    /// ioc_ids (Some(id) in a case, None standalone).
    fn reload_ioc_lists(&mut self) {
        if let Some(case) = &self.case {
            let metas = case.ioc_lists().unwrap_or_default();
            self.ioc_ids = metas.iter().map(|m| Some(m.id)).collect();
            self.ioc_lists = metas
                .iter()
                .map(|m| StoredIoc {
                    name: m.name.clone(),
                    body: case.ioc_list_body(m.id).unwrap_or_default(),
                })
                .collect();
        } else {
            self.ioc_lists = self.prefs.ioc_lists(&self.path);
            self.ioc_ids = vec![None; self.ioc_lists.len()];
        }
    }

    /// IOC manager window (G17): list/edit/create/delete standalone IOC lists,
    /// run one or all. Running compiles the list and tags every hit
    /// "ioc:<name>", mirroring the engine's ioc scan.
    fn ioc_window(&mut self, ctx: &egui::Context) {
        let mut open = true;
        let mut select: Option<usize> = None;
        let mut create = false;
        let mut delete = false;
        let mut save = false;
        let mut run = false;
        let mut run_all = false;

        // A real OS window (viewport), not an in-app egui::Window, so it can be
        // dragged outside the main window and onto another monitor. Same pattern
        // as the cell pop-out and candidate-review windows.
        let vp_id = egui::ViewportId::from_hash_of("ioc-manager");
        let mut builder = egui::ViewportBuilder::default()
            .with_title("IOC lists")
            .with_inner_size([560.0, 360.0]);
        if self.ioc_pinned {
            builder = builder.with_always_on_top();
        }
        // Copied out so the closure can still borrow the rest of `self`; written
        // back after the viewport renders.
        let mut pinned = self.ioc_pinned;
        ctx.show_viewport_immediate(vp_id, builder, |ui, _class| {
            let vctx = ui.ctx().clone();
            ui.with_layout(egui::Layout::right_to_left(egui::Align::Center), |ui| {
                if ui.toggle_value(&mut pinned, "\u{1F4CC} Pin").changed() {
                    let level = if pinned {
                        egui::WindowLevel::AlwaysOnTop
                    } else {
                        egui::WindowLevel::Normal
                    };
                    vctx.send_viewport_cmd(egui::ViewportCommand::WindowLevel(level));
                }
            });
            ui.horizontal_top(|ui| {
                ui.vertical(|ui| {
                    ui.set_width(170.0);
                    egui::ScrollArea::vertical().max_height(260.0).show(ui, |ui| {
                        for (i, l) in self.ioc_lists.iter().enumerate() {
                            if ui
                                .selectable_label(self.ioc_sel == Some(i), &l.name)
                                .clicked()
                            {
                                select = Some(i);
                            }
                        }
                    });
                    ui.horizontal(|ui| {
                        if ui.button("New").clicked() {
                            create = true;
                        }
                        if ui.button("Run all").clicked() {
                            run_all = true;
                        }
                    });
                });
                ui.separator();
                ui.vertical(|ui| {
                    if self.ioc_sel.is_some() {
                        ui.add(egui::TextEdit::singleline(&mut self.ioc_name).hint_text("List name").desired_width(f32::INFINITY));
                        // Lift the text box off the window background: its default
                        // fill (extreme_bg_color) is near-black in dark mode and
                        // sits on an equally dark window, so the box is invisible.
                        // A lighter fill plus a visible border gives it an edge.
                        let dark = ui.visuals().dark_mode;
                        let box_fill = if dark {
                            egui::Color32::from_gray(32)
                        } else {
                            egui::Color32::from_gray(250)
                        };
                        let box_stroke = egui::Stroke::new(1.0, ui.visuals().widgets.noninteractive.fg_stroke.color);
                        // Fill the window: give the box all the remaining height
                        // bar a slice reserved for the button row below, so it
                        // grows and shrinks as the window is resized.
                        let reserved = ui.spacing().interact_size.y + ui.spacing().item_spacing.y * 2.0;
                        let box_height = (ui.available_height() - reserved).max(80.0);
                        egui::Frame::new().fill(box_fill).stroke(box_stroke).inner_margin(2.0).show(ui, |ui| {
                            ui.visuals_mut().extreme_bg_color = box_fill;
                            egui::ScrollArea::vertical().show(ui, |ui| {
                                ui.add_sized(
                                    [ui.available_width(), box_height],
                                    egui::TextEdit::multiline(&mut self.ioc_body)
                                        .hint_text("One indicator per line: plain substring, /regex/, or `Field=value query`. A ## line labels the block below it (until a blank line); its text is stamped onto rows those indicators hit")
                                        .desired_width(f32::INFINITY)
                                        .font(egui::TextStyle::Monospace),
                                );
                            });
                        });
                        ui.horizontal(|ui| {
                            if ui.button("Delete").clicked() {
                                delete = true;
                            }
                            if ui.button("Save").clicked() {
                                save = true;
                            }
                            if ui.button("Save & run").clicked() {
                                save = true;
                                run = true;
                            }
                        });
                    } else {
                        ui.weak("Select or create a list.");
                    }
                });
            });
            if vctx.input(|i| i.viewport().close_requested()) {
                open = false;
            }
        });
        self.ioc_pinned = pinned;

        if let Some(i) = select {
            // Persist the current edit before switching away.
            self.ioc_commit_edit();
            self.ioc_sel = Some(i);
            if let Some(l) = self.ioc_lists.get(i) {
                self.ioc_name = l.name.clone();
                self.ioc_body = l.body.clone();
            }
        }
        if create {
            self.ioc_commit_edit();
            self.ioc_create_list();
        }
        if delete {
            self.ioc_delete_selected();
        }
        if save {
            self.ioc_commit_edit();
        }
        if run {
            self.run_ioc_selected();
        }
        if run_all {
            self.ioc_commit_edit();
            self.run_all_iocs();
        }
        if !open {
            self.ioc_commit_edit();
            self.show_ioc = false;
        }
    }

    /// Create a new empty IOC list and select it. In a case the name must be
    /// unique; a clash keeps the "new list" default and surfaces the error.
    fn ioc_create_list(&mut self) {
        if let Some(case) = self.case.as_mut() {
            // Find a free default name ("new list", "new list 2", ...).
            let mut name = "new list".to_string();
            let mut n = 2;
            while self.ioc_lists.iter().any(|l| l.name.eq_ignore_ascii_case(&name)) {
                name = format!("new list {n}");
                n += 1;
            }
            match case.create_ioc_list(&name) {
                Ok(_) => {
                    self.reload_ioc_lists();
                    self.ioc_sel = self.ioc_lists.iter().position(|l| l.name == name);
                }
                Err(e) => {
                    self.status = format!("Create IOC list: {e}");
                    return;
                }
            }
        } else {
            self.ioc_lists.push(StoredIoc { name: "new list".to_string(), body: String::new() });
            self.ioc_ids.push(None);
            self.ioc_sel = Some(self.ioc_lists.len() - 1);
            self.prefs.set_ioc_lists(&self.path, self.ioc_lists.clone());
        }
        if let Some(l) = self.ioc_sel.and_then(|i| self.ioc_lists.get(i)) {
            self.ioc_name = l.name.clone();
            self.ioc_body = l.body.clone();
        }
    }

    /// Delete the selected IOC list from its backing store.
    fn ioc_delete_selected(&mut self) {
        let Some(i) = self.ioc_sel else { return };
        if i >= self.ioc_lists.len() {
            return;
        }
        if let Some(case) = self.case.as_mut() {
            if let Some(Some(id)) = self.ioc_ids.get(i).copied() {
                if let Err(e) = case.delete_ioc_list(id) {
                    self.status = format!("Delete IOC list: {e}");
                    return;
                }
            }
            self.reload_ioc_lists();
        } else {
            self.ioc_lists.remove(i);
            self.ioc_ids.remove(i);
            self.prefs.set_ioc_lists(&self.path, self.ioc_lists.clone());
        }
        self.ioc_sel = if self.ioc_lists.is_empty() { None } else { Some(0) };
        if let Some(l) = self.ioc_sel.and_then(|i| self.ioc_lists.get(i)) {
            self.ioc_name = l.name.clone();
            self.ioc_body = l.body.clone();
        } else {
            self.ioc_name.clear();
            self.ioc_body.clear();
        }
    }

    /// Flush the name/body edit fields back into the selected list and persist
    /// to the backing store (case DB or prefs).
    fn ioc_commit_edit(&mut self) {
        let Some(i) = self.ioc_sel else { return };
        let (changed, name, body) = match self.ioc_lists.get(i) {
            Some(l) if l.name != self.ioc_name || l.body != self.ioc_body => {
                (true, self.ioc_name.clone(), self.ioc_body.clone())
            }
            _ => (false, String::new(), String::new()),
        };
        if !changed {
            return;
        }
        if let Some(case) = self.case.as_mut() {
            if let Some(Some(id)) = self.ioc_ids.get(i).copied() {
                let old_name = self.ioc_lists[i].name.clone();
                if name != old_name {
                    if let Err(e) = case.rename_ioc_list(id, &name) {
                        self.status = format!("Rename IOC list: {e}");
                        // Revert the edit field so the invalid name does not stick.
                        self.ioc_name = old_name;
                        return;
                    }
                }
                if let Err(e) = case.set_ioc_list_body(id, &body) {
                    self.status = format!("Save IOC list: {e}");
                    return;
                }
            }
            self.ioc_lists[i].name = name;
            self.ioc_lists[i].body = body;
        } else {
            self.ioc_lists[i].name = name;
            self.ioc_lists[i].body = body;
            self.prefs.set_ioc_lists(&self.path, self.ioc_lists.clone());
        }
    }

    fn run_ioc_selected(&mut self) {
        let Some(i) = self.ioc_sel else { return };
        let lists: Vec<(String, String)> = match self.ioc_lists.get(i) {
            Some(l) => vec![(l.name.clone(), l.body.clone())],
            None => return,
        };
        self.spawn_ioc(lists, JobKind::Ioc);
    }

    fn run_all_iocs(&mut self) {
        let lists: Vec<(String, String)> =
            self.ioc_lists.iter().map(|l| (l.name.clone(), l.body.clone())).collect();
        if lists.is_empty() {
            return;
        }
        self.spawn_ioc(lists, JobKind::IocAll);
    }

    /// Scan one or more IOC lists against the whole timeline on a background
    /// worker, returning each list's hits for tagging. Small files scan inline.
    /// Rows are tagged on the UI thread in apply_ioc_hits once the scan lands.
    fn spawn_ioc(&mut self, lists: Vec<(String, String)>, kind: JobKind) {
        if self.idx.is_none() {
            return;
        }
        self.cancel_job();
        let idx = self.idx.as_ref().unwrap();
        let n = idx.row_count();

        if n < Self::ASYNC_ROW_THRESHOLD {
            let t0 = std::time::Instant::now();
            let mut results = Vec::new();
            for (name, body) in &lists {
                let view = View::new(idx.as_ref(), &self.session);
                let set = view.compile_iocs(body, false);
                if set.count() == 0 {
                    continue;
                }
                match view.scan_iocs(&set) {
                    Ok(hits) => results.push((format!("ioc:{name}"), hits)),
                    Err(e) => {
                        self.status = format!("IOC scan error: {e}");
                        return;
                    }
                }
            }
            self.apply_ioc_hits(results, t0.elapsed());
            return;
        }

        // Background path.
        let idx = Arc::clone(idx);
        let snap = self.session.snapshot();
        let annot = self.adopted;
        let progress = Arc::new(AtomicUsize::new(0));
        let cancel = Arc::new(AtomicBool::new(false));
        let (tx, rx): (Sender<JobResult>, Receiver<JobResult>) = std::sync::mpsc::channel();
        let p2 = Arc::clone(&progress);
        let c2 = Arc::clone(&cancel);
        // run-all scans each list over the full file; progress spans all lists.
        let total = n.saturating_mul(lists.len().max(1));
        std::thread::spawn(move || {
            let mut results = Vec::new();
            for (name, body) in &lists {
                if c2.load(Ordering::Relaxed) {
                    let _ = tx.send(JobResult::Cancelled);
                    return;
                }
                match crate::engine::scan::parallel_ioc(&idx, &snap, body, false, annot, &p2, &c2) {
                    crate::engine::scan::IocOutcome::Done(hits) => {
                        results.push((format!("ioc:{name}"), hits))
                    }
                    crate::engine::scan::IocOutcome::Cancelled => {
                        let _ = tx.send(JobResult::Cancelled);
                        return;
                    }
                }
            }
            let _ = tx.send(JobResult::Ioc(results));
        });
        self.job = Some(ScanJob {
            kind,
            rx,
            progress,
            cancel,
            total,
            started: std::time::Instant::now(),
        });
        self.status = "Scanning IOCs\u{2026}".to_string();
    }

    /// Tag the rows each IOC list matched, then refresh the view. A read-only
    /// session is temporarily bumped so the tags can be written, matching the
    /// Go behaviour. Each list's tag "ioc:<name>" is fixed purple.
    fn apply_ioc_hits(
        &mut self,
        lists: Vec<(String, Vec<(usize, Option<String>)>)>,
        elapsed: std::time::Duration,
    ) {
        let prev_mode = self.session.mode();
        if prev_mode == Mode::ReadOnly {
            self.session.set_mode(Mode::Investigator);
        }
        let mut total = 0usize;
        for (tag, hits) in &lists {
            let _ = self.session.define_tag(tag, "#8E24AA");
            for (master, note) in hits {
                if self.session.add_tag(*master, tag).is_ok() {
                    total += 1;
                }
                // A ## comment on the matching indicator is merged into the
                // row's comment: set when blank, left alone when it already
                // contains the text, else prepended.
                if let Some(note) = note {
                    let cur = self.session.comment(*master);
                    let merged = if cur.is_empty() {
                        Some(note.clone())
                    } else if cur.contains(note.as_str()) {
                        None
                    } else {
                        Some(format!("{note}; {cur}"))
                    };
                    if let Some(merged) = merged {
                        let _ = self.session.set_comment(*master, &merged);
                    }
                }
            }
        }
        self.session.set_mode(prev_mode);
        if lists.len() == 1 {
            self.status = format!("{}: {} rows tagged in {:.0?}", lists[0].0, total, elapsed);
        } else {
            self.status =
                format!("Ran {} lists \u{00B7} {} rows tagged in {:.0?}", lists.len(), total, elapsed);
        }
        // Reflect new tags in the current view (e.g. a tagged-only filter).
        self.apply_filter();
    }

    // --- notes storage abstraction (R7) --------------------------------------
    // Notes live in the case DB when a case timeline is open, else in the
    // standalone prefs store. These helpers hide which, so notes_section and
    // the cell menu can stay storage-agnostic. Entries are (id, text, done).

    fn note_list(&self, kind: NoteKind) -> Vec<(i64, String, bool)> {
        if let (Some(case), Some(tl)) = (&self.case, &self.cur_timeline) {
            let k = note_kind_str(kind);
            case.notes(tl.id, k)
                .unwrap_or_default()
                .into_iter()
                .map(|n| (n.id, n.text, n.done))
                .collect()
        } else {
            self.prefs
                .notes(&self.path, kind)
                .into_iter()
                .map(|n| (n.id, n.text, n.done))
                .collect()
        }
    }

    /// Add a note for `kind`, unless an entry with the same text (trimmed,
    /// case-insensitive) already exists - duplicates are refused and the
    /// existing entry is set to flash instead. Returns true when a new note was
    /// actually added.
    fn note_add(&mut self, kind: NoteKind, text: &str) -> bool {
        let trimmed = text.trim();
        if let Some((id, _, _)) = self
            .note_list(kind)
            .into_iter()
            .find(|(_, t, _)| t.trim().eq_ignore_ascii_case(trimmed))
        {
            // Already present: flash the existing row (start resolved at render)
            // and add nothing.
            self.note_flash = Some((kind, id, None));
            return false;
        }
        if let (Some(case), Some(tl)) = (self.case.as_mut(), self.cur_timeline.as_ref()) {
            if let Err(e) = case.add_note(tl.id, note_kind_str(kind), text) {
                self.status = format!("Add note: {e}");
                return false;
            }
            true
        } else if !self.path.is_empty() {
            self.prefs.add_note(&self.path, kind, text);
            true
        } else {
            false
        }
    }

    fn note_set_done(&mut self, kind: NoteKind, id: i64, done: bool) {
        if let Some(case) = self.case.as_mut() {
            if self.cur_timeline.is_some() {
                let _ = case.set_note_done(id, done);
                return;
            }
        }
        self.prefs.set_note_done(&self.path, kind, id, done);
    }

    fn note_delete(&mut self, kind: NoteKind, id: i64) {
        if let Some(case) = self.case.as_mut() {
            if self.cur_timeline.is_some() {
                let _ = case.delete_note(id);
                return;
            }
        }
        self.prefs.delete_note(&self.path, kind, id);
    }

    fn note_set_text(&mut self, kind: NoteKind, id: i64, text: &str) {
        if let Some(case) = self.case.as_mut() {
            if self.cur_timeline.is_some() {
                let _ = case.set_note_text(id, text);
                return;
            }
        }
        self.prefs.set_note_text(&self.path, kind, id, text);
    }

    /// Investigator's notes dock (G13): Artifacts and Times lists with a done
    /// checkbox (strike-through), filter-to-entry, and delete. Backed by the
    /// case DB inside a case, else the standalone prefs store.
    fn notes_section(&mut self, ui: &mut egui::Ui) {
        enum NoteAction {
            Toggle(NoteKind, i64, bool),
            Delete(NoteKind, i64),
            FilterTo(String),
            BeginEdit(NoteKind, i64, String),
            CommitEdit(NoteKind, i64),
            CancelEdit,
            OpenExport(String, String),
            FilterTimeWindow(String),
        }
        let mut action: Option<NoteAction> = None;
        let editing = self.editing_note;

        // Duplicate-add flash: a pending flash gets its start time on the first
        // render after the attempt; three sine humps over FLASH_DUR read as a
        // "couple of flashes" on the existing entry.
        const FLASH_DUR: f64 = 0.9;
        let now = ui.input(|i| i.time);
        if let Some((_, _, start)) = &mut self.note_flash {
            if start.is_none() {
                *start = Some(now);
            }
        }
        let flash = self.note_flash;

        for (kind, label, buf_is_artifact) in [
            (NoteKind::Artifact, "Artifacts", true),
            (NoteKind::Time, "Times", false),
        ] {
            let notes = self.note_list(kind);
            ui.horizontal(|ui| {
                ui.strong(label);
                // Export list: open every entry as one copyable block of text.
                // Disabled when the list is empty.
                ui.add_enabled_ui(!notes.is_empty(), |ui| {
                    if ui.small_button("Export list").on_hover_text("Open all entries as copyable text").clicked() {
                        let joined = notes
                            .iter()
                            .map(|(_, t, _)| t.clone())
                            .collect::<Vec<_>>()
                            .join("\n");
                        action = Some(NoteAction::OpenExport(label.to_string(), joined));
                    }
                });
            });
            for (id, text, done) in &notes {
                let ir = ui.horizontal(|ui| {
                    if editing == Some((kind, *id)) {
                        let resp = ui.add(
                            egui::TextEdit::singleline(&mut self.editing_note_buf).desired_width(180.0),
                        );
                        if self.editing_note_focus {
                            resp.request_focus();
                            self.editing_note_focus = false;
                        }
                        let enter = resp.lost_focus() && ui.input(|i| i.key_pressed(egui::Key::Enter));
                        if ui.small_button("\u{2714}").on_hover_text("Save").clicked() || enter {
                            action = Some(NoteAction::CommitEdit(kind, *id));
                        }
                        if ui.small_button("\u{00D7}").on_hover_text("Cancel").clicked()
                            || ui.input(|i| i.key_pressed(egui::Key::Escape))
                        {
                            action = Some(NoteAction::CancelEdit);
                        }
                        return;
                    }
                    let mut d = *done;
                    if ui.checkbox(&mut d, "").changed() {
                        action = Some(NoteAction::Toggle(kind, *id, d));
                    }
                    let rt = if *done {
                        egui::RichText::new(text).strikethrough().weak()
                    } else {
                        egui::RichText::new(text)
                    };
                    ui.add(egui::Label::new(rt).truncate());
                    if ui.small_button("\u{270F}").on_hover_text("Edit").clicked() {
                        action = Some(NoteAction::BeginEdit(kind, *id, text.clone()));
                    }
                    if ui.small_button("\u{1F50D}").on_hover_text("Filter to this entry").clicked() {
                        action = Some(NoteAction::FilterTo(text.clone()));
                    }
                    // Times entries can filter to a ±5 min window, the same as the
                    // grid's right-click on a timestamp cell.
                    if kind == NoteKind::Time
                        && ui
                            .small_button("\u{00B1}5m")
                            .on_hover_text("Filter \u{00B1}5 min around this time")
                            .clicked()
                    {
                        action = Some(NoteAction::FilterTimeWindow(text.clone()));
                    }
                    if ui.small_button("\u{00D7}").on_hover_text("Delete").clicked() {
                        action = Some(NoteAction::Delete(kind, *id));
                    }
                });
                // Overlay a pulsing wash on the entry a duplicate add just
                // targeted, so the eye is drawn to the copy that already exists.
                if let Some((fk, fid, Some(fs))) = flash {
                    if fk == kind && fid == *id {
                        let elapsed = now - fs;
                        if elapsed < FLASH_DUR {
                            let hump =
                                ((elapsed / FLASH_DUR) * std::f64::consts::PI * 3.0).sin().abs();
                            let a = (hump * 150.0) as u8;
                            ui.painter().rect_filled(
                                ir.response.rect,
                                3.0,
                                egui::Color32::from_rgba_unmultiplied(255, 210, 0, a),
                            );
                            ui.ctx().request_repaint();
                        }
                    }
                }
            }
            let buf = if buf_is_artifact { &mut self.new_artifact } else { &mut self.new_time };
            let mut submit = false;
            ui.horizontal(|ui| {
                let resp = ui.add(
                    egui::TextEdit::singleline(buf).hint_text(format!("Add {}\u{2026}", label.to_lowercase())),
                );
                if (resp.lost_focus() && ui.input(|i| i.key_pressed(egui::Key::Enter))) || ui.button("+").clicked() {
                    submit = true;
                }
            });
            if submit {
                let text = buf.trim().to_string();
                if !text.is_empty() {
                    if buf_is_artifact { self.new_artifact.clear() } else { self.new_time.clear() }
                    self.note_add(kind, &text);
                }
            }
            ui.separator();
        }

        match action {
            Some(NoteAction::Toggle(k, id, done)) => self.note_set_done(k, id, done),
            Some(NoteAction::Delete(k, id)) => self.note_delete(k, id),
            Some(NoteAction::BeginEdit(k, id, text)) => {
                self.editing_note = Some((k, id));
                self.editing_note_buf = text;
                self.editing_note_focus = true;
            }
            Some(NoteAction::CommitEdit(k, id)) => {
                let text = self.editing_note_buf.trim().to_string();
                if !text.is_empty() {
                    self.note_set_text(k, id, &text);
                }
                self.editing_note = None;
                self.editing_note_buf.clear();
            }
            Some(NoteAction::CancelEdit) => {
                self.editing_note = None;
                self.editing_note_buf.clear();
            }
            Some(NoteAction::FilterTo(text)) => {
                // Quote the entry as a plain free-text match (wrap in quotes so
                // special characters stay literal) and apply. Escape backslash
                // first, then quote, so a trailing "\" or an embedded '"' can't
                // break out of or prematurely close the quoted atom.
                let escaped = text.replace('\\', "\\\\").replace('"', "\\\"");
                self.filter_text = format!("\"{escaped}\"");
                self.apply_filter();
            }
            Some(NoteAction::OpenExport(title, text)) => {
                self.notes_export = Some((title, text));
            }
            Some(NoteAction::FilterTimeWindow(text)) => {
                // No column is attached to a Times entry, so target the index's
                // primary time column, then reuse the grid's ±5 min filter.
                match self.idx.as_ref().and_then(|idx| timecol::detect_time_column(idx)) {
                    Some(col) => self.filter_plus_minus_5min(col, &text),
                    None => self.status = "No time column to filter on.".to_string(),
                }
            }
            None => {}
        }

        // Drop the flash once its run is over so it fires just once per attempt.
        if let Some((_, _, Some(fs))) = self.note_flash {
            if now - fs >= FLASH_DUR {
                self.note_flash = None;
            }
        }
    }

    /// Render the "Export list" window for a notes list, if open: every entry as
    /// one block of text in a scrollable multiline box, plus a copy button. The
    /// text is editable so the user can trim before copying; edits are discarded
    /// on close (the underlying list is untouched).
    fn notes_export_window(&mut self, ctx: &egui::Context) {
        let Some((title, text)) = self.notes_export.as_mut() else { return };
        let mut open = true;
        egui::Window::new(format!("Export {title}"))
            .id(egui::Id::new("notes-export-window"))
            .collapsible(false)
            .resizable(true)
            .default_width(440.0)
            .default_height(360.0)
            .open(&mut open)
            .show(ctx, |ui| {
                ui.horizontal(|ui| {
                    if ui.button("Copy to clipboard").clicked() {
                        ui.ctx().copy_text(text.clone());
                    }
                    ui.weak("or select all and copy manually");
                });
                ui.separator();
                egui::ScrollArea::vertical().auto_shrink([false, false]).show(ui, |ui| {
                    ui.add_sized(
                        ui.available_size(),
                        egui::TextEdit::multiline(text).desired_width(f32::INFINITY),
                    );
                });
            });
        if !open {
            self.notes_export = None;
        }
    }

    /// Left-sidebar Tags section: a per-tag row with a filter checkbox, a
    /// colour swatch that opens a recolour menu, and a delete button. Ticking
    /// tags drives the OR filter; recolour/delete edit the palette directly.
    fn tags_sidebar(&mut self, ui: &mut egui::Ui) {
        // Global highlight opacity: one wash strength for every tag, so an
        // over-saturated colour can be toned down without recolouring. Persisted
        // once the drag ends (or immediately on a click/keyboard change) rather
        // than every frame.
        ui.horizontal(|ui| {
            ui.label("Highlight opacity");
            let mut pct = (self.tag_opacity as f32 / 255.0 * 100.0).round() as u8;
            let resp = ui.add(egui::Slider::new(&mut pct, 0..=100).suffix("%"));
            if resp.changed() {
                self.tag_opacity = (pct as f32 / 100.0 * 255.0).round() as u8;
                if !resp.dragged() {
                    self.prefs.set_tag_opacity(self.tag_opacity);
                }
            }
            if resp.drag_stopped() {
                self.prefs.set_tag_opacity(self.tag_opacity);
            }
        });
        ui.separator();

        let defs = self.session.tag_defs();
        if defs.is_empty() {
            ui.weak("No tags yet.");
            return;
        }
        // How many rows carry each tag, over the whole dataset (not the
        // filtered view). Recomputed each frame so it tracks tag edits live.
        let counts = self.session.tag_counts();
        // Collect edits out of the loop so we don't borrow self mutably while
        // iterating a cloned def list.
        let mut toggle: Option<(String, bool)> = None;
        let mut recolor: Option<(String, String)> = None;
        let mut delete: Option<String> = None;
        let mut filter_changed = false;

        for d in &defs {
            ui.horizontal(|ui| {
                // Colour swatch opens a recolour menu.
                let swatch = parse_hex_opaque(&d.color);
                ui.menu_button(
                    egui::RichText::new("\u{25A0}").color(swatch.unwrap_or(Color32::GRAY)),
                    |ui| {
                        for preset in PALETTE_PRESETS {
                            if let Some(c) = parse_hex_opaque(preset) {
                                let (rect, resp) = ui.allocate_exact_size(
                                    egui::vec2(22.0, 18.0),
                                    egui::Sense::click(),
                                );
                                ui.painter().rect_filled(rect, 2.0, c);
                                if resp.clicked() {
                                    recolor = Some((d.name.clone(), preset.to_string()));
                                    ui.close();
                                }
                            }
                        }
                        if ui.button("No highlight").clicked() {
                            recolor = Some((d.name.clone(), "none".to_string()));
                            ui.close();
                        }
                    },
                );
                let mut on = self.tag_checklist.contains(&d.name);
                let label = format!("{} ({})", d.name, counts.get(&d.name).copied().unwrap_or(0));
                if ui.checkbox(&mut on, label).changed() {
                    toggle = Some((d.name.clone(), on));
                    filter_changed = true;
                }
                if ui
                    .small_button("\u{00D7}")
                    .on_hover_text("Delete tag")
                    .clicked()
                {
                    delete = Some(d.name.clone());
                }
            });
        }

        if let Some((name, on)) = toggle {
            if on {
                self.tag_checklist.insert(name);
            } else {
                self.tag_checklist.remove(&name);
            }
        }
        if let Some((name, color)) = recolor {
            self.edit_palette(|s| s.define_tag(&name, &color));
            filter_changed = true;
        }
        if let Some(name) = delete {
            // Don't delete yet: deleting strips the tag from every row and is
            // irreversible, so stage it for the confirmation modal.
            self.confirm_tag_delete = Some(name);
        }
        if filter_changed {
            self.apply_filter();
        }
    }

    /// Modal that confirms a tag deletion staged from the left pane, spelling
    /// out that it strips the tag from every tagged row and can't be undone.
    /// Runs the delete only on an explicit confirm.
    fn confirm_tag_delete_window(&mut self, ctx: &egui::Context) {
        let Some(name) = self.confirm_tag_delete.clone() else {
            return;
        };
        let count = self.session.tag_counts().get(&name).copied().unwrap_or(0);
        let mut proceed = false;
        let resp = egui::Modal::new(egui::Id::new("confirm-tag-delete")).show(ctx, |ui| {
            ui.set_max_width(360.0);
            ui.heading("Delete tag?");
            ui.add_space(6.0);
            ui.label(format!(
                "Deleting \u{201C}{name}\u{201D} removes it from {count} tagged row{}. This can't be undone.",
                if count == 1 { "" } else { "s" }
            ));
            ui.add_space(12.0);
            ui.horizontal(|ui| {
                if ui.button("Cancel").clicked() {
                    proceed = false;
                    ui.close();
                }
                ui.with_layout(egui::Layout::right_to_left(egui::Align::Center), |ui| {
                    let btn = egui::Button::new(
                        egui::RichText::new("Delete tag").color(Color32::WHITE),
                    )
                    .fill(Color32::from_rgb(176, 42, 42));
                    if ui.add(btn).clicked() {
                        proceed = true;
                    }
                });
            });
        });

        if proceed {
            self.tag_checklist.remove(&name);
            self.edit_palette(|s| s.delete_tag(&name));
            self.apply_filter();
            self.confirm_tag_delete = None;
        } else if resp.should_close() {
            // Backdrop click, Escape, or Cancel: leave the tag untouched.
            self.confirm_tag_delete = None;
        }
    }

    /// Run a palette edit (recolour/delete), bumping a read-only session to
    /// Investigator for the duration so the change is permitted, same as the
    /// tag editor window.
    fn edit_palette<F>(&mut self, f: F)
    where
        F: FnOnce(&mut Session) -> crate::engine::session::Result<()>,
    {
        let prev = self.session.mode();
        if prev == Mode::ReadOnly {
            self.session.set_mode(Mode::Investigator);
        }
        let res = f(&mut self.session);
        self.session.set_mode(prev);
        if let Err(e) = res {
            self.status = format!("{e}");
        }
    }

    fn editor_window(&mut self, ctx: &egui::Context) {
        match self.editing {
            Editing::None => {}
            Editing::Tag(m) => self.tag_editor_window(ctx, m),
            Editing::Comment(m) => self.comment_editor_window(ctx, m),
            Editing::Cell(m, c) => self.cell_editor_window(ctx, m, c),
        }
    }

    /// Tag palette editor (R8): tick known tags on/off for the selected rows
    /// (applies to every selected master, not just the clicked one), recolour a
    /// tag from preset swatches, create a new tag, delete or rename. Mirrors the
    /// Fyne editTagsPopup. A read-only session is bumped to Investigator for the
    /// duration of a change, matching the original.
    fn tag_editor_window(&mut self, ctx: &egui::Context, clicked: usize) {
        // Act on the whole selection, falling back to the clicked row.
        let targets: Vec<usize> = if self.selected.is_empty() {
            vec![clicked]
        } else {
            self.selected.iter().copied().collect()
        };

        let mut open = true;
        enum TagAction {
            Toggle(String, bool),
            Recolor(String, String),
            Create(String, String),
            Delete(String),
            Rename(String, String),
        }
        let mut action: Option<TagAction> = None;
        let defs = self.session.tag_defs();
        // "On" when every target already carries the tag.
        let all_have = |name: &str| targets.iter().all(|&m| self.session.tags(m).iter().any(|t| t == name));

        egui::Window::new("Tags")
            .collapsible(false)
            .resizable(true)
            .default_size([340.0, 360.0])
            .open(&mut open)
            .show(ctx, |ui| {
                ui.label(format!(
                    "Editing {} row{}",
                    targets.len(),
                    if targets.len() == 1 { "" } else { "s" }
                ));
                ui.separator();
                egui::ScrollArea::vertical().max_height(220.0).show(ui, |ui| {
                    for d in &defs {
                        ui.horizontal(|ui| {
                            // Swatch + checkbox to apply/remove.
                            if let Some(c) = parse_hex_opaque(&d.color) {
                                let (rect, resp) =
                                    ui.allocate_exact_size(egui::vec2(16.0, 16.0), egui::Sense::click());
                                ui.painter().rect_filled(rect, 2.0, c);
                                let _ = resp;
                            } else {
                                ui.label("\u{25CB}"); // no-highlight sentinel
                            }
                            let mut on = all_have(&d.name);
                            if ui.checkbox(&mut on, &d.name).changed() {
                                action = Some(TagAction::Toggle(d.name.clone(), on));
                            }
                            // Recolour from presets via a small menu.
                            ui.menu_button("\u{1F3A8}", |ui| {
                                for preset in PALETTE_PRESETS {
                                    if let Some(c) = parse_hex_opaque(preset) {
                                        let (rect, resp) = ui.allocate_exact_size(
                                            egui::vec2(22.0, 18.0),
                                            egui::Sense::click(),
                                        );
                                        ui.painter().rect_filled(rect, 2.0, c);
                                        if resp.clicked() {
                                            action = Some(TagAction::Recolor(
                                                d.name.clone(),
                                                preset.to_string(),
                                            ));
                                            ui.close();
                                        }
                                    }
                                }
                                if ui.button("No highlight").clicked() {
                                    action = Some(TagAction::Recolor(d.name.clone(), "none".to_string()));
                                    ui.close();
                                }
                            });
                            if ui.small_button("\u{00D7}").on_hover_text("Delete tag").clicked() {
                                action = Some(TagAction::Delete(d.name.clone()));
                            }
                        });
                    }
                });
                ui.separator();
                // New tag: name + colour.
                ui.horizontal(|ui| {
                    ui.add(
                        egui::TextEdit::singleline(&mut self.edit_buf)
                            .hint_text("New tag name\u{2026}")
                            .desired_width(160.0),
                    );
                    if ui.button("Create").clicked() {
                        let name = self.edit_buf.trim().to_string();
                        if !name.is_empty() {
                            action = Some(TagAction::Create(name, PALETTE_PRESETS[0].to_string()));
                        }
                    }
                });
                // Rename the clicked row's first tag, if any.
                ui.horizontal(|ui| {
                    ui.add(
                        egui::TextEdit::singleline(&mut self.rename_buf)
                            .hint_text("Rename tag to\u{2026}")
                            .desired_width(160.0),
                    );
                    if ui.button("Rename").clicked() {
                        if let Some(old) = self.session.tags(clicked).first().cloned() {
                            let new = self.rename_buf.trim().to_string();
                            if !new.is_empty() {
                                action = Some(TagAction::Rename(old, new));
                            }
                        }
                    }
                });
            });

        if let Some(act) = action {
            let prev = self.session.mode();
            if prev == Mode::ReadOnly {
                self.session.set_mode(Mode::Investigator);
            }
            let res = match act {
                TagAction::Toggle(name, on) => {
                    let mut r = Ok(());
                    for &m in &targets {
                        r = if on {
                            self.session.add_tag(m, &name)
                        } else {
                            self.session.remove_tag(m, &name)
                        };
                        if r.is_err() {
                            break;
                        }
                    }
                    r
                }
                TagAction::Recolor(name, color) => self.session.define_tag(&name, &color),
                TagAction::Create(name, color) => {
                    let r = self.session.define_tag(&name, &color);
                    if r.is_ok() {
                        for &m in &targets {
                            let _ = self.session.add_tag(m, &name);
                        }
                        self.edit_buf.clear();
                    }
                    r
                }
                TagAction::Delete(name) => self.session.delete_tag(&name),
                TagAction::Rename(old, new) => {
                    let r = self.session.rename_tag(&old, &new);
                    if r.is_ok() {
                        self.rename_buf.clear();
                    }
                    r
                }
            };
            self.session.set_mode(prev);
            match res {
                Ok(()) => self.apply_filter(),
                Err(e) => self.status = format!("{e}"),
            }
        }

        if !open {
            self.editing = Editing::None;
            self.edit_buf.clear();
            self.rename_buf.clear();
        }
    }

    fn comment_editor_window(&mut self, ctx: &egui::Context, master: usize) {
        // The comment applies to the whole selection (falling back to the
        // clicked row), so a bulk comment writes once to every selected row.
        let targets: Vec<usize> = if self.selected.is_empty() {
            vec![master]
        } else {
            self.selected.iter().copied().collect()
        };
        let mut open = true;
        let mut commit = false;
        let mut cancel = false;
        let title = if targets.len() > 1 {
            format!("Comment {} rows", targets.len())
        } else {
            "Comment row".to_string()
        };
        egui::Window::new(title)
            .collapsible(false)
            .resizable(true)
            .open(&mut open)
            .show(ctx, |ui| {
                if targets.len() > 1 {
                    ui.weak(format!("Applies to all {} selected rows.", targets.len()));
                }
                let resp = ui.add(
                    egui::TextEdit::multiline(&mut self.edit_buf)
                        .desired_rows(5)
                        .desired_width(360.0),
                );
                resp.request_focus();
                ui.horizontal(|ui| {
                    if ui.button("OK").clicked() {
                        commit = true;
                    }
                    if ui.button("Cancel").clicked() {
                        cancel = true;
                    }
                });
                // Enter commits, Ctrl+Enter inserts a newline (handled by the
                // multiline widget itself).
                if resp.lost_focus()
                    && ui.input(|i| i.key_pressed(egui::Key::Enter) && !i.modifiers.command && !i.modifiers.ctrl)
                {
                    commit = true;
                }
            });
        if !open {
            cancel = true;
        }
        if commit {
            let text = self.edit_buf.clone();
            // Investigator and World-write may comment; only Read-only is
            // blocked. Bump a Read-only session for the write, matching the
            // tag editor, so the right-click path isn't silently refused.
            let prev = self.session.mode();
            if prev == Mode::ReadOnly {
                self.session.set_mode(Mode::Investigator);
            }
            let mut res = Ok(());
            for &m in &targets {
                res = self.session.set_comment(m, &text);
                if res.is_err() {
                    break;
                }
            }
            self.session.set_mode(prev);
            match res {
                Ok(()) => {
                    self.editing = Editing::None;
                    self.edit_buf.clear();
                    self.status = if targets.len() > 1 {
                        format!("Commented {} rows.", targets.len())
                    } else {
                        "Comment saved.".to_string()
                    };
                    self.apply_filter();
                }
                Err(e) => self.status = format!("{e}"),
            }
        } else if cancel {
            self.editing = Editing::None;
            self.edit_buf.clear();
        }
    }

    fn cell_editor_window(&mut self, ctx: &egui::Context, master: usize, col: usize) {
        let mut open = true;
        let mut commit = false;
        let mut cancel = false;
        egui::Window::new("Edit cell")
            .collapsible(false)
            .resizable(false)
            .open(&mut open)
            .show(ctx, |ui| {
                let resp = ui.text_edit_singleline(&mut self.edit_buf);
                resp.request_focus();
                ui.horizontal(|ui| {
                    if ui.button("OK").clicked() {
                        commit = true;
                    }
                    if ui.button("Cancel").clicked() {
                        cancel = true;
                    }
                });
                if resp.lost_focus() && ui.input(|i| i.key_pressed(egui::Key::Enter)) {
                    commit = true;
                }
            });
        if !open {
            cancel = true;
        }
        if commit {
            match self.session.set_cell(master, col, &self.edit_buf.clone()) {
                Ok(()) => {
                    self.editing = Editing::None;
                    self.edit_buf.clear();
                    self.apply_filter();
                }
                Err(e) => self.status = format!("{e}"),
            }
        } else if cancel {
            self.editing = Editing::None;
            self.edit_buf.clear();
        }
    }

    fn goto_window(&mut self, ctx: &egui::Context) {
        let mut open = true;
        let mut go = false;
        egui::Window::new("Go to row")
            .collapsible(false)
            .resizable(false)
            .open(&mut open)
            .show(ctx, |ui| {
                let resp = ui.text_edit_singleline(&mut self.goto_buf);
                resp.request_focus();
                if (resp.lost_focus() && ui.input(|i| i.key_pressed(egui::Key::Enter)))
                    || ui.button("Go").clicked()
                {
                    go = true;
                }
            });
        if go {
            if let Ok(n) = self.goto_buf.trim().parse::<usize>() {
                if n >= 1 && n <= self.rows.len() {
                    self.want_scroll_to = Some(n - 1);
                }
            }
            self.show_goto = false;
            self.goto_buf.clear();
        } else if !open {
            self.show_goto = false;
        }
    }
}

/// Keyboard shortcuts (G18): / and Ctrl+F focus the filter, Esc clears,
/// Ctrl+S save, Ctrl+B toggle dock, Ctrl+L toggle sidebar, Ctrl+G go-to-row,
/// t tag, c comment. Enter-to-apply lives on the filter box itself.
fn handle_keys(app: &mut TlxApp, ctx: &egui::Context) {
    // Don't steal single-letter keys while a text field has focus.
    let typing = ctx.memory(|m| m.focused().is_some());
    ctx.input(|i| {
        let cmd = i.modifiers.command || i.modifiers.ctrl;
        if cmd && i.key_pressed(egui::Key::F) {
            app.focus_filter = true;
        }
        if cmd && i.key_pressed(egui::Key::S) {
            app.save_session();
        }
        if cmd && i.key_pressed(egui::Key::E) {
            app.export_view();
        }
        if cmd && i.key_pressed(egui::Key::B) {
            app.show_right_dock = !app.show_right_dock;
        }
        if cmd && i.key_pressed(egui::Key::L) {
            app.show_left_sidebar = !app.show_left_sidebar;
        }
        if cmd && i.key_pressed(egui::Key::G) {
            app.show_goto = true;
        }
        if i.key_pressed(egui::Key::F3) {
            app.find_next();
        }
        if i.key_pressed(egui::Key::Escape) {
            app.clear_filters();
        }
        if !typing {
            if i.key_pressed(egui::Key::Slash) {
                app.focus_filter = true;
            }
            if i.key_pressed(egui::Key::T) {
                if let Some(m) = app.selected_master() {
                    app.editing = Editing::Tag(m);
                    app.edit_buf.clear();
                }
            }
            if i.key_pressed(egui::Key::C) {
                if let Some(m) = app.selected_master() {
                    app.editing = Editing::Comment(m);
                    app.edit_buf = app.session.comment(m);
                }
            }
        }
    });
}

fn ui_modifiers(resp: &egui::Response) -> egui::Modifiers {
    resp.ctx.input(|i| i.modifiers)
}

/// Extract a plain substring for F3 find-next from the search box: the value
/// side of the last `field=value` term, with /regex/ or "quoted" delimiters
/// unwrapped. Best effort. Mirrors Fyne's findNeedle.
fn find_needle(q: &str) -> String {
    let mut q = q.trim().to_string();
    if let Some(i) = q.rfind('=') {
        q = q[i + 1..].trim().to_string();
    }
    let bytes = q.as_bytes();
    if bytes.len() >= 2 {
        let first = bytes[0];
        let last = bytes[bytes.len() - 1];
        if (first == b'/' && last == b'/') || (first == b'"' && last == b'"') {
            q = q[1..q.len() - 1].to_string();
        }
    }
    q
}

/// Map a prefs NoteKind to the case DB's note-kind string.
fn note_kind_str(kind: NoteKind) -> &'static str {
    match kind {
        NoteKind::Artifact => NOTE_ARTIFACT,
        NoteKind::Time => NOTE_TIME,
    }
}

/// A case's display name: its DB file's base name without extension
/// (".../foo/foo.tlxdb" -> "foo"). Mirrors Go's caseDisplayName.
fn case_display_name(path: &str) -> String {
    file_stem(path)
}

/// Base name without extension, or "case" if empty. Mirrors the Go helpers'
/// filepath.Base + TrimSuffix.
fn file_stem(path: &str) -> String {
    let stem = std::path::Path::new(path)
        .file_stem()
        .and_then(|s| s.to_str())
        .unwrap_or("");
    if stem.is_empty() { "case".to_string() } else { stem.to_string() }
}

/// Run an export over `view` according to `spec`, with progress + cancel.
/// A free function so it can run on either the UI thread (small view) or a
/// worker (large view); the View carries whichever overlay was handed to it.
fn run_export(
    view: &View,
    dest: &str,
    spec: &ExportSpec,
    progress: &AtomicUsize,
    cancel: &AtomicBool,
) -> std::io::Result<crate::engine::export::ExportOutcome> {
    match spec {
        ExportSpec::Current { omit, comment } => {
            crate::engine::export::export_current_tracked(view, dest, omit, comment, progress, cancel)
        }
        ExportSpec::Timeline { cols, comment, tagged_only } => crate::engine::export::export_timeline_tracked(
            view,
            dest,
            cols,
            comment,
            *tagged_only,
            progress,
            cancel,
        ),
    }
}

/// Serialize a builder source column for persistence, tagging virtual columns
/// by kind and data columns by their (name-resolvable) display name.
fn stored_source(s: &SourceCol) -> StoredSource {
    let kind = match s.col {
        COL_TAGS => "tags",
        COL_COMMENT => "comment",
        _ => "data",
    };
    StoredSource { kind: kind.to_string(), name: s.name.clone() }
}

/// Draw a drag handle (two columns of three dots) for an export column. Painted
/// rather than drawn from a font glyph so it renders regardless of which fonts
/// egui bundles. The enclosing dnd_drag_source adds the grab cursor and the
/// drag sense, so this only needs to be visible.
fn grip_handle(ui: &mut egui::Ui) {
    let (rect, _) = ui.allocate_exact_size(egui::vec2(12.0, 20.0), egui::Sense::hover());
    let color = ui.visuals().weak_text_color();
    for row in 0..3 {
        for col in 0..2 {
            let x = rect.left() + 4.0 + col as f32 * 4.0;
            let y = rect.center().y - 4.0 + row as f32 * 4.0;
            ui.painter().circle_filled(egui::pos2(x, y), 1.2, color);
        }
    }
}

/// Draw a clickable funnel (filter) icon and return its Response. Painted
/// rather than drawn from a font glyph so it renders regardless of which fonts
/// egui bundles - the default fonts have no funnel glyph (and U+22A1 and the
/// like show as a blank tofu box). `active` fills the funnel to show the column
/// has a value filter applied; otherwise it is a hollow outline.
fn funnel_icon(ui: &mut egui::Ui, active: bool) -> egui::Response {
    let (rect, resp) = ui.allocate_exact_size(egui::vec2(16.0, 18.0), egui::Sense::click());
    if resp.hovered() {
        ui.painter().rect_filled(rect, 2.0, ui.visuals().widgets.hovered.bg_fill);
    }
    let color = if active {
        ui.visuals().selection.bg_fill
    } else {
        ui.visuals().strong_text_color()
    };
    let r = rect.shrink(3.0);
    let cx = r.center().x;
    let tip_y = r.top() + r.height() * 0.5;
    // The bowl: a wide top edge converging to a point, then a short stem.
    let bowl = vec![
        egui::pos2(r.left(), r.top()),
        egui::pos2(r.right(), r.top()),
        egui::pos2(cx, tip_y),
    ];
    let stroke = egui::Stroke::new(1.4, color);
    if active {
        ui.painter().add(egui::Shape::convex_polygon(bowl, color, egui::Stroke::NONE));
    } else {
        ui.painter().add(egui::Shape::convex_polygon(bowl, egui::Color32::TRANSPARENT, stroke));
    }
    ui.painter().line_segment([egui::pos2(cx, tip_y), egui::pos2(cx, r.bottom())], stroke);
    resp
}

/// Draw a draggable source-column chip (a bordered label) for the export
/// builder. The chip shows a grab cursor via the enclosing dnd_drag_source.
fn source_chip(ui: &mut egui::Ui, name: &str) {
    egui::Frame::group(ui.style())
        .inner_margin(egui::Margin::symmetric(6, 2))
        .show(ui, |ui| {
            ui.label(name);
        });
}

/// Current time as an RFC3339 string for the added_at column. (The engine
/// forbids argless time in workflow scripts, but this is normal app code.)
fn now_rfc3339() -> String {
    chrono::Utc::now().to_rfc3339()
}

/// Turn the path chosen in the save dialog into a path inside a dedicated case
/// folder, creating the folder. Choosing ".../foo.tlxdb" yields
/// ".../foo/foo.tlxdb" so the DB and its copied timelines sit together.
/// Mirrors Go's caseFolderDB.
fn case_folder_db(chosen: &str) -> std::io::Result<String> {
    let p = std::path::Path::new(chosen);
    let mut base = p.file_stem().and_then(|s| s.to_str()).unwrap_or("").to_string();
    if base.is_empty() {
        base = "case".to_string();
    }
    let dir = p.parent().unwrap_or_else(|| std::path::Path::new("."));
    let folder = dir.join(&base);
    std::fs::create_dir_all(&folder)?;
    Ok(folder.join(format!("{base}.tlxdb")).display().to_string())
}

/// Make sure `src` lives in the case's folder, copying it in when it is
/// elsewhere; return the path to register. A file already in the folder is
/// used in place; a name clash with a different file gets a "-N" suffix.
/// Mirrors Go's importIntoCase + uniquePath.
fn import_into_case(db_path: &str, src: &str) -> std::io::Result<String> {
    let dir = std::path::Path::new(db_path)
        .parent()
        .unwrap_or_else(|| std::path::Path::new("."));
    let src_path = std::path::Path::new(src);
    let src_dir = src_path.parent().unwrap_or_else(|| std::path::Path::new("."));
    let abs_dir = std::fs::canonicalize(dir).unwrap_or_else(|_| dir.to_path_buf());
    let abs_src_dir = std::fs::canonicalize(src_dir).unwrap_or_else(|_| src_dir.to_path_buf());
    if abs_dir == abs_src_dir {
        return Ok(src.to_string());
    }
    let file_name = src_path.file_name().unwrap_or_default();
    let dest = unique_path(dir.join(file_name));
    std::fs::copy(src, &dest)?;
    Ok(dest.display().to_string())
}

/// Return `p` if free, else `p` with a "-N" suffix before the extension for the
/// first N that does not exist.
fn unique_path(p: std::path::PathBuf) -> std::path::PathBuf {
    if !p.exists() {
        return p;
    }
    let ext = p.extension().and_then(|s| s.to_str()).unwrap_or("").to_string();
    let stem = p.file_stem().and_then(|s| s.to_str()).unwrap_or("").to_string();
    let dir = p.parent().unwrap_or_else(|| std::path::Path::new("."));
    for i in 1.. {
        let name = if ext.is_empty() {
            format!("{stem}-{i}")
        } else {
            format!("{stem}-{i}.{ext}")
        };
        let cand = dir.join(name);
        if !cand.exists() {
            return cand;
        }
    }
    unreachable!()
}

/// Turn tagged-row entries into the master view's headers and records. Leading
/// columns are fixed (Time, Timeline, Tags, Comment); the rest is the ordered
/// union of every timeline's display headers, so columns renamed to the same
/// name across timelines line up. Entries snapshotted before the merge feature
/// carry no cells and fall back to a Summary column. Mirrors Go's masterGrid.
fn master_grid(entries: &[MasterEntry]) -> (Vec<String>, Vec<Vec<String>>) {
    use crate::engine::adopt::is_annotation_header;
    let fixed = ["Time", "Timeline", "Tags", "Comment"];

    let mut canonical: Vec<String> = Vec::new();
    let mut seen: std::collections::HashMap<String, usize> = std::collections::HashMap::new();
    let mut need_summary = false;
    for e in entries {
        if e.cells.is_empty() {
            need_summary = true;
        }
        for h in &e.display_headers {
            if h.is_empty() || is_annotation_header(h) {
                continue;
            }
            if !seen.contains_key(h) {
                seen.insert(h.clone(), canonical.len());
                canonical.push(h.clone());
            }
        }
    }

    let mut headers: Vec<String> = fixed.iter().map(|s| s.to_string()).collect();
    headers.extend(canonical.iter().cloned());
    if need_summary {
        headers.push("Summary".to_string());
    }

    let mut records = Vec::with_capacity(entries.len());
    for e in entries {
        let tval = if e.has_time {
            e.time
                .map(|t| t.format("%Y-%m-%d %H:%M:%S%.3f").to_string())
                .unwrap_or_else(|| e.time_raw.clone())
        } else {
            e.time_raw.clone()
        };
        let mut rec = vec![tval, e.timeline.clone(), e.tags.join(", "), e.comment.clone()];
        let mut cells = vec![String::new(); canonical.len()];
        for (j, h) in e.display_headers.iter().enumerate() {
            if j >= e.cells.len() {
                break;
            }
            if let Some(&ci) = seen.get(h) {
                cells[ci] = e.cells[j].clone();
            }
        }
        rec.extend(cells);
        if need_summary {
            rec.push(e.summary.clone());
        }
        records.push(rec);
    }
    (headers, records)
}

/// Colours offered when defining or recolouring a tag, matching the Fyne
/// palettePresets.
const PALETTE_PRESETS: &[&str] = &[
    "#E53935", "#FDD835", "#43A047", "#1E88E5", "#8E24AA", "#00897B", "#FB8C00", "#6D4C41",
    "#78909C",
];

/// Parse "#RRGGBB" into an opaque colour, or None for an unparseable or "none"
/// value (the no-highlight sentinel). Used for tag swatches.
fn parse_hex_opaque(hex: &str) -> Option<Color32> {
    let h = hex.strip_prefix('#')?;
    if h.len() != 6 {
        return None;
    }
    let r = u8::from_str_radix(&h[0..2], 16).ok()?;
    let g = u8::from_str_radix(&h[2..4], 16).ok()?;
    let b = u8::from_str_radix(&h[4..6], 16).ok()?;
    Some(Color32::from_rgb(r, g, b))
}

/// Parse a "#RRGGBB" tag colour into a semi-transparent row tint at the given
/// alpha, the wash painted behind a tagged row. `alpha` is the global tag
/// opacity. Returns None for an unparseable or the "none" sentinel colour.
fn parse_hex_tint(hex: &str, alpha: u8) -> Option<Color32> {
    let h = hex.strip_prefix('#')?;
    if h.len() != 6 {
        return None;
    }
    let r = u8::from_str_radix(&h[0..2], 16).ok()?;
    let g = u8::from_str_radix(&h[2..4], 16).ok()?;
    let b = u8::from_str_radix(&h[4..6], 16).ok()?;
    Some(Color32::from_rgba_unmultiplied(r, g, b, alpha))
}

/// Quote a column header as a query-grammar field token (mirrors the Go
/// quoteQueryField), escaping backslashes and quotes so a header with spaces
/// parses as one field.
fn quote_query_field(s: &str) -> String {
    let mut out = String::from("\"");
    for ch in s.chars() {
        if ch == '\\' || ch == '"' {
            out.push('\\');
        }
        out.push(ch);
    }
    out.push('"');
    out
}

/// Build a LayoutJob highlighting every case-insensitive occurrence of `needle`
/// in `text` with a coloured background (G21 match highlight).
fn highlighted_job(text: &str, needle: &str, style: &egui::Style) -> LayoutJob {
    let mut job = LayoutJob::default();
    let font_id = FontId::proportional(13.0);
    let text_color = style.visuals.text_color();
    if needle.is_empty() {
        job.append(text, 0.0, TextFormat::simple(font_id, text_color));
        return job;
    }
    let lower_text = text.to_lowercase();
    let lower_needle = needle.to_lowercase();
    let mut idx = 0usize;
    let mut found_any = false;
    while idx < lower_text.len() {
        if let Some(rel) = lower_text[idx..].find(&lower_needle) {
            let start = idx + rel;
            let end = start + lower_needle.len();
            if start > idx {
                job.append(&text[idx..start], 0.0, TextFormat::simple(font_id.clone(), text_color));
            }
            let mut fmt = TextFormat::simple(font_id.clone(), Color32::BLACK);
            fmt.background = Color32::from_rgb(255, 230, 90);
            job.append(&text[start..end], 0.0, fmt);
            idx = end;
            found_any = true;
        } else {
            break;
        }
    }
    if idx < text.len() {
        job.append(&text[idx..], 0.0, TextFormat::simple(font_id, text_color));
    }
    if !found_any {
        job.sections.clear();
        job.text.clear();
        job.append(text, 0.0, TextFormat::simple(FontId::proportional(13.0), text_color));
    }
    job
}
