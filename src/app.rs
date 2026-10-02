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
use crate::prefs::{NoteKind, Prefs, SavedView, StoredIoc};

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

    // selection, keyed by master row; anchor is a view position for Shift-range.
    selected: BTreeSet<usize>,
    sel_anchor: Option<usize>,

    // per-column quick filters (header row), keyed by data column index.
    show_filter_row: bool,
    col_filters: Vec<String>,
    // Quick-filter boxes for the virtual Tags and Comment columns (the data
    // col_filters vec is keyed by data-column index and can't hold these).
    col_filter_tags: String,
    col_filter_comment: String,

    // Last substring highlighted in a Details-pane field: (field salt, text).
    // Kept across frames because a right-click collapses the live selection
    // before the context menu can read it.
    details_sel: Option<(usize, String)>,

    // Comment cell being edited inline in the grid: (master row, buffer).
    // Clicking a Comment cell opens an editable box in place rather than the
    // pop-out window.
    inline_comment: Option<(usize, String)>,

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
}

/// What a worker sends back when its scan finishes (or is cancelled).
enum JobResult {
    /// Filter finished: the matched rows, already sorted.
    Filter(Result<Vec<usize>, String>),
    /// IOC finished: for each list scanned, its "ioc:<name>" tag and the
    /// master rows that hit. One entry for a single list, several for run-all.
    Ioc(Vec<(String, Vec<usize>)>),
    Cancelled,
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
}

impl Default for TlxApp {
    fn default() -> Self {
        let prefs = Prefs::load();
        let dark = prefs.dark_theme();
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
            row_height: 22.0,
            selected: BTreeSet::new(),
            sel_anchor: None,
            show_filter_row: true,
            col_filters: Vec::new(),
            col_filter_tags: String::new(),
            col_filter_comment: String::new(),
            details_sel: None,
            inline_comment: None,
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
            new_view_name: String::new(),
            show_ioc: false,
            ioc_lists: Vec::new(),
            ioc_ids: Vec::new(),
            ioc_sel: None,
            ioc_name: String::new(),
            ioc_body: String::new(),
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
        let idx = self.idx.as_ref().unwrap();
        let mut view = View::new(idx, &self.session);
        if let Err(e) = view.apply(self.filter.clone()) {
            self.status = format!("Export filter: {e}");
            return;
        }
        if !self.sort_keys.is_empty() {
            if let Err(e) = view.sort(self.sort_keys.clone()) {
                self.status = format!("Export sort: {e}");
                return;
            }
        }
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
        match crate::engine::export::export_omitting_with_comment(
            &view,
            &self.session,
            &dest,
            &omit,
            &comment,
        ) {
            Ok(()) => self.status = format!("Exported {} rows to {dest}", view.len()),
            Err(e) => self.status = format!("Export failed: {e}"),
        }
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
        let Some(path) = rfd::FileDialog::new()
            .add_filter("CSV/TSV", &["csv", "tsv", "txt"])
            .pick_file()
        else {
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
                    format!("\u{25CF} {}", tl.name)
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
                            ui.label(format!("{h} \u{2192}"));
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
                if ui.button("Open CSV\u{2026}").clicked() {
                    if let Some(path) = rfd::FileDialog::new()
                        .add_filter("CSV/TSV", &["csv", "tsv", "txt"])
                        .pick_file()
                    {
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
                if ui.button("Clear").clicked() {
                    self.clear_filters();
                }
                if ui.checkbox(&mut self.tagged_only, "Tagged only").changed() {
                    self.apply_filter();
                }
                if ui.toggle_value(&mut self.show_filter_row, "Filter row").clicked() {}
                ui.separator();
                if ui.button("Save").clicked() {
                    self.save_session();
                }
                if ui.button("Export\u{2026}").clicked() {
                    self.export_view();
                }
                // Theme toggle (R3). Label shows the mode it switches to,
                // matching the Fyne button.
                let label = if self.dark { "\u{2600} Light" } else { "\u{1F319} Dark" };
                if ui.button(label).clicked() {
                    self.dark = !self.dark;
                    self.prefs.set_dark_theme(self.dark);
                }
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
                egui::CollapsingHeader::new("IOC lists").show(ui, |ui| {
                    if ui.button("Manage IOC lists\u{2026}").clicked() {
                        self.open_ioc_manager();
                    }
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
        self.render_popouts(&ctx);
        self.render_candidate_review(&ctx);
        self.render_floating_panels(&ctx);

        egui::CentralPanel::default().show_inside(ui, |ui| {
            self.grid(ui);
        });
    }
}

impl TlxApp {
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
            applied_filter,
            sort_col,
            sort_desc,
            show_filter_row,
            row_height,
            want_scroll_to,
            path,
            annot_cols,
            col_names,
            master_mode,
            adopted,
            inline_comment,
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
        let col_title = |refc: ColumnRef| -> String {
            match refc {
                COL_ROW_NUM => "#".to_string(),
                COL_TAGS => "\u{270E} Tags".to_string(),
                COL_COMMENT => "\u{270E} Comment".to_string(),
                c => col_names
                    .get(c as usize)
                    .filter(|n| !n.is_empty())
                    .cloned()
                    .unwrap_or_else(|| headers.get(c as usize).cloned().unwrap_or_default()),
            }
        };

        use egui_extras::{Column, TableBuilder};

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
                _ => Column::auto().at_least(60.0).clip(true),
            };
            builder = builder.column(col);
        }
        if let Some(p) = want_scroll_to.take() {
            builder = builder.scroll_to_row(p, Some(egui::Align::Center));
        }

        let header_h = if show_filter_row { 48.0 } else { 24.0 };
        let mut clicked_col: Option<ColumnRef> = None;
        let mut col_filter_changed = false;
        // Header right-click menu actions, applied after the table borrow ends.
        let mut header_sort: Option<(ColumnRef, bool)> = None; // (col, desc)
        let mut header_empty: Option<(ColumnRef, bool)> = None; // (col, want_empty)
        let mut header_clear: Option<ColumnRef> = None;
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

        builder
            .header(header_h, |mut header| {
                for &refc in &columns {
                    header.col(|ui| {
                        ui.vertical(|ui| {
                            let marker = if sort_col == Some(refc) {
                                if sort_desc { " \u{25BC}" } else { " \u{25B2}" }
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
                                    }
                                }
                            }
                        });
                    });
                }
            })
            .body(|body| {
                body.rows(*row_height, n_rows, |mut row| {
                    let view_pos = row.index();
                    let master = rows[view_pos];
                    row.set_selected(selected.contains(&master));
                    let rec = idx.row(master).unwrap_or_default();
                    // Top tag's colour tints the whole row (semi-transparent),
                    // matching the Fyne rowTintColor wash.
                    let tint = session.row_color(master).and_then(|hex| parse_hex_tint(&hex));
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
                                    r.request_focus();
                                    if r.lost_focus()
                                        || ui.input(|i| i.key_pressed(egui::Key::Enter))
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
                            let resp = if highlight.is_empty() {
                                ui.add(egui::Label::new(text).truncate())
                            } else {
                                ui.add(
                                    egui::Label::new(highlighted_job(text, &highlight, ui.style()))
                                        .truncate(),
                                )
                            };
                            if text.len() > 40 {
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
                        // Clicking the Tags cell opens its editor; the Comment
                        // cell opens an inline editable box; a data cell selects
                        // the row (the overlay consumes the click, so row-level
                        // selection won't fire for it). Double-click pops out.
                        if cell_resp.clicked() {
                            match refc {
                                COL_TAGS => open_tag = Some(master),
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
                                                ui.weak("\u{2713}");
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
        // Start inline comment editing: load the current comment into the buffer
        // the Comment cell renders as a TextEdit next frame.
        if let Some(master) = open_inline_comment {
            let cur = self.session.comment(master);
            self.inline_comment = Some((master, cur));
        }
        // Commit the inline comment edit (Enter or focus lost). Bump
        // ReadOnly->Investigator for the write, then restore, same as elsewhere.
        if commit_inline_comment {
            if let Some((master, text)) = self.inline_comment.take() {
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
            });
        }
        if let Some((col, text)) = filter_5min {
            self.filter_plus_minus_5min(col, &text);
        }
        if let Some(text) = add_artifact {
            self.note_add(NoteKind::Artifact, &text);
            self.status = "Added to Artifacts.".to_string();
        }
        // Artifacts added from the sticky cell menu (menu stays open, so this
        // can carry several in one frame).
        if !sticky_add.is_empty() {
            let n = sticky_add.len();
            for (_, _, text) in &sticky_add {
                self.note_add(NoteKind::Artifact, text);
            }
            self.status = format!("Added {n} to Artifacts.");
        }
        if let Some(text) = add_time {
            self.note_add(NoteKind::Time, &text);
            self.status = "Added to Times.".to_string();
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
        if col_filter_changed {
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
                    if ui.toggle_value(pinned_ref, "\u{1F4CC} Pin on top").changed() {
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
            self.note_add(kind, &text);
            self.status = match kind {
                NoteKind::Artifact => "Added to Artifacts.".to_string(),
                NoteKind::Time => "Added to Times.".to_string(),
            };
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
        let builder = egui::ViewportBuilder::default()
            .with_title(&review.title)
            .with_inner_size([480.0, 560.0]);
        // Candidates to add this frame (index into review.cands).
        let mut to_add: Vec<usize> = Vec::new();
        let mut add_all = false;
        let title = review.title.clone();
        let cands = &review.cands;
        let added = &mut review.added;
        let open_ref = &mut review.open;
        ctx.show_viewport_immediate(vp_id, builder, |ui, _class| {
            let vctx = ui.ctx().clone();
            ui.horizontal(|ui| {
                ui.strong(&title);
                ui.with_layout(egui::Layout::right_to_left(egui::Align::Center), |ui| {
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
                            ui.weak("\u{2713}");
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
        for (i, text) in &picks {
            self.note_add(NoteKind::Artifact, text);
            if let Some(r) = self.candidate_review.as_mut() {
                r.added[*i] = true;
            }
        }
        if !picks.is_empty() {
            self.status = format!("Added {} to Artifacts.", picks.len());
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
                if ui.add_enabled(!read_only, egui::Button::new("\u{270E} Edit")).clicked() {
                    open_tag_master = Some(master);
                }
            });
            Self::details_field_row(ui, usize::MAX - 1, "", &tags, notes_on, &mut note_add, &mut sel_store);
            ui.horizontal(|ui| {
                ui.strong("Comment:");
                if ui.add_enabled(!read_only, egui::Button::new("\u{270E} Edit")).clicked() {
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
            self.note_add(kind, &text);
            self.status = match kind {
                NoteKind::Artifact => "Added to Artifacts.".to_string(),
                NoteKind::Time => "Added to Times.".to_string(),
            };
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
                    if ui.toggle_value(&mut pinned, "\u{1F4CC} Pin on top").changed() {
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

        egui::Window::new("IOC lists")
            .collapsible(false)
            .resizable(true)
            .default_size([560.0, 360.0])
            .open(&mut open)
            .show(ctx, |ui| {
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
                            ui.add(
                                egui::TextEdit::multiline(&mut self.ioc_body)
                                    .hint_text("One indicator per line: plain substring, /regex/, or `Field=value query`")
                                    .desired_rows(12)
                                    .desired_width(f32::INFINITY)
                                    .font(egui::TextStyle::Monospace),
                            );
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
            });

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
                    crate::engine::scan::ScanOutcome::Done(hits) => {
                        results.push((format!("ioc:{name}"), hits))
                    }
                    crate::engine::scan::ScanOutcome::Cancelled => {
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
    fn apply_ioc_hits(&mut self, lists: Vec<(String, Vec<usize>)>, elapsed: std::time::Duration) {
        let prev_mode = self.session.mode();
        if prev_mode == Mode::ReadOnly {
            self.session.set_mode(Mode::Investigator);
        }
        let mut total = 0usize;
        for (tag, hits) in &lists {
            let _ = self.session.define_tag(tag, "#8E24AA");
            for master in hits {
                if self.session.add_tag(*master, tag).is_ok() {
                    total += 1;
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

    fn note_add(&mut self, kind: NoteKind, text: &str) {
        if let (Some(case), Some(tl)) = (self.case.as_mut(), self.cur_timeline.as_ref()) {
            if let Err(e) = case.add_note(tl.id, note_kind_str(kind), text) {
                self.status = format!("Add note: {e}");
            }
        } else if !self.path.is_empty() {
            self.prefs.add_note(&self.path, kind, text);
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

    /// Investigator's notes dock (G13): Artifacts and Times lists with a done
    /// checkbox (strike-through), filter-to-entry, and delete. Backed by the
    /// case DB inside a case, else the standalone prefs store.
    fn notes_section(&mut self, ui: &mut egui::Ui) {
        enum NoteAction {
            Toggle(NoteKind, i64, bool),
            Delete(NoteKind, i64),
            FilterTo(String),
        }
        let mut action: Option<NoteAction> = None;

        for (kind, label, buf_is_artifact) in [
            (NoteKind::Artifact, "Artifacts", true),
            (NoteKind::Time, "Times", false),
        ] {
            ui.strong(label);
            let notes = self.note_list(kind);
            for (id, text, done) in &notes {
                ui.horizontal(|ui| {
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
                    if ui.small_button("\u{2315}").on_hover_text("Filter to this entry").clicked() {
                        action = Some(NoteAction::FilterTo(text.clone()));
                    }
                    if ui.small_button("\u{00D7}").on_hover_text("Delete").clicked() {
                        action = Some(NoteAction::Delete(kind, *id));
                    }
                });
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
            Some(NoteAction::FilterTo(text)) => {
                // Quote the entry as a plain free-text match (wrap in quotes so
                // special characters stay literal) and apply. Escape backslash
                // first, then quote, so a trailing "\" or an embedded '"' can't
                // break out of or prematurely close the quoted atom.
                let escaped = text.replace('\\', "\\\\").replace('"', "\\\"");
                self.filter_text = format!("\"{escaped}\"");
                self.apply_filter();
            }
            None => {}
        }
    }

    /// Left-sidebar Tags section: a per-tag row with a filter checkbox, a
    /// colour swatch that opens a recolour menu, and a delete button. Ticking
    /// tags drives the OR filter; recolour/delete edit the palette directly.
    fn tags_sidebar(&mut self, ui: &mut egui::Ui) {
        let defs = self.session.tag_defs();
        if defs.is_empty() {
            ui.weak("No tags yet.");
            return;
        }
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
                if ui.checkbox(&mut on, &d.name).changed() {
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
            self.tag_checklist.remove(&name);
            self.edit_palette(|s| s.delete_tag(&name));
            filter_changed = true;
        }
        if filter_changed {
            self.apply_filter();
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
                                ui.label("\u{2300}"); // no-highlight sentinel
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

/// Parse a "#RRGGBB" tag colour into a semi-transparent row tint (alpha 0x40),
/// the same wash the Fyne app paints behind a tagged row. Returns None for an
/// unparseable or the "none" sentinel colour.
fn parse_hex_tint(hex: &str) -> Option<Color32> {
    let h = hex.strip_prefix('#')?;
    if h.len() != 6 {
        return None;
    }
    let r = u8::from_str_radix(&h[0..2], 16).ok()?;
    let g = u8::from_str_radix(&h[2..4], 16).ok()?;
    let b = u8::from_str_radix(&h[4..6], 16).ok()?;
    Some(Color32::from_rgba_unmultiplied(r, g, b, 0x40))
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
