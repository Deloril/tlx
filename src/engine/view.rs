//! Port of internal/model/view.go: an ordered, filtered/sorted subset of an
//! Index's rows. Stores master row indices; the GUI addresses rows by view
//! position via `master()`.
//!
//! Full-shape port: FilterSpec carries Query/Expr/Tags/Conds, matching the Go
//! struct, so query.rs (boolean query language), ioc.rs and highlight.rs can
//! all compile against it. The boolean-query compiler lives in query.rs as
//! further `impl View` methods (compile_expr, compile_time_term) to keep this
//! file from growing unbounded. Like the Go View, this stores the Overlay
//! alongside the Index rather than threading it through every call, which is
//! what lets compile_expr build closures that read tags/comments without an
//! extra lifetime parameter on every method.

use regex::Regex;
use std::collections::HashMap;

use super::adopt::AdoptedColumns;
use super::index::Index;

/// ColumnRef addresses a column in the view. Non-negative values are data
/// columns (indices into a record). Negative values are virtual columns and
/// selectors, mirroring the Go constants.
pub type ColumnRef = i32;

pub const COL_TAGS: ColumnRef = -1;
pub const COL_COMMENT: ColumnRef = -2;
pub const COL_ROW_NUM: ColumnRef = -3;
pub const COL_ALL: ColumnRef = -100;
pub const COL_NONE: ColumnRef = -101;
pub const COL_DATA: ColumnRef = -102;

/// Overlay supplies per-row data that lives outside the CSV: tags, comments
/// and cell edits. Session implements it; filtering and sorting see edited
/// values and virtual columns through this trait, same as the Go interface.
pub trait Overlay {
    fn tags(&self, row: usize) -> Vec<String>;
    fn comment(&self, row: usize) -> String;
    fn cell_override(&self, row: usize, col: usize) -> Option<String>;
}

/// One level of an ordering.
#[derive(Clone, Copy, Debug)]
pub struct SortKey {
    pub col: ColumnRef,
    pub desc: bool,
}

/// One per-column condition: a set of values matched against a single column
/// (or COL_ALL). Values combine by OR unless `all` is set (AND). Conditions
/// combine with each other per `FilterSpec::conds_any`.
#[derive(Clone, Default)]
pub struct ColumnCond {
    pub column: ColumnRef,
    pub values: Vec<String>,
    pub regexp: bool,
    pub cased: bool,
    pub all: bool,
    pub neg: bool,
}

/// FilterSpec describes an absolute filter over the full row set. A row is
/// kept only if it passes every part that is set. Mirrors the Go struct.
#[derive(Clone, Default)]
pub struct FilterSpec {
    pub query: String,
    pub regexp: bool,
    pub cased: bool,
    pub column: ColumnRef,
    pub tagged_only: bool,
    pub tag: String,
    pub tags: Vec<String>,
    /// Boolean query expression (query.rs grammar). Empty means unset.
    pub expr: String,
    pub conds: Vec<ColumnCond>,
    pub conds_any: bool,
    pub col_filters: HashMap<ColumnRef, String>,
}

impl FilterSpec {
    pub fn empty(&self) -> bool {
        self.query.is_empty()
            && self.expr.is_empty()
            && !self.tagged_only
            && self.tag.is_empty()
            && self.tags.is_empty()
            && self.conds.is_empty()
            && !has_col_filter(&self.col_filters)
    }
}

fn has_col_filter(m: &HashMap<ColumnRef, String>) -> bool {
    m.values().any(|v| !v.is_empty())
}

/// One compiled value test: a regexp, a substring needle, or the "non-empty"
/// test used by a lone "*" column filter.
#[derive(Clone)]
pub struct Matcher {
    re: Option<Regex>,
    needle: String, // lowercased when !cased
    cased: bool,
    non_empty: bool,
}

impl Matcher {
    pub fn col_filter(value: &str, cased: bool) -> Result<Matcher, regex::Error> {
        if value == "*" {
            return Ok(Matcher {
                re: None,
                needle: String::new(),
                cased,
                non_empty: true,
            });
        }
        Self::compile(value, false, cased)
    }

    pub fn compile(value: &str, use_regexp: bool, cased: bool) -> Result<Matcher, regex::Error> {
        if use_regexp {
            let pattern = if cased {
                value.to_string()
            } else {
                format!("(?i){value}")
            };
            let re = Regex::new(&pattern)?;
            Ok(Matcher {
                re: Some(re),
                needle: String::new(),
                cased,
                non_empty: false,
            })
        } else if cased {
            Ok(Matcher {
                re: None,
                needle: value.to_string(),
                cased: true,
                non_empty: false,
            })
        } else {
            Ok(Matcher {
                re: None,
                needle: value.to_lowercase(),
                cased: false,
                non_empty: false,
            })
        }
    }

    pub fn matches(&self, s: &str) -> bool {
        if self.non_empty {
            return !s.trim().is_empty();
        }
        if let Some(re) = &self.re {
            return re.is_match(s);
        }
        if self.cased {
            s.contains(self.needle.as_str())
        } else {
            s.to_lowercase().contains(self.needle.as_str())
        }
    }

    /// spans: every non-empty match of the matcher within s, as byte ranges.
    /// Used by highlight.rs.
    pub fn spans(&self, s: &str) -> Vec<(usize, usize)> {
        if let Some(re) = &self.re {
            return re
                .find_iter(s)
                .filter(|m| m.end() > m.start())
                .map(|m| (m.start(), m.end()))
                .collect();
        }
        if self.needle.is_empty() {
            return Vec::new();
        }
        let hay_owned;
        let hay: &str = if self.cased {
            s
        } else {
            hay_owned = s.to_lowercase();
            &hay_owned
        };
        let mut out = Vec::new();
        let mut from = 0usize;
        while from < hay.len() {
            match hay[from..].find(self.needle.as_str()) {
                Some(i) => {
                    let start = from + i;
                    let end = start + self.needle.len();
                    out.push((start, end));
                    from = end;
                }
                None => break,
            }
        }
        out
    }
}

struct CompiledCond {
    column: ColumnRef,
    all: bool,
    neg: bool,
    vals: Vec<Matcher>,
}

/// RowPred is a compiled predicate over a single row; the boolean query
/// compiler in query.rs produces these. It borrows the View (and so the
/// Overlay/Index behind it) for the duration of one apply() call, mirroring
/// how the Go closures close over `v`.
pub type RowPred<'a> = Box<dyn Fn(usize, &[String]) -> bool + 'a>;

pub(super) struct CompiledFilter<'a> {
    pub(super) tagged_only: bool,
    pub(super) tag: String,
    pub(super) tags: Vec<String>,
    pub(super) has_query: bool,
    pub(super) query: Option<Matcher>,
    pub(super) query_col: ColumnRef,
    conds: Vec<CompiledCond>,
    conds_any: bool,
    pub(super) expr: Option<RowPred<'a>>,
    col_filters: Vec<CompiledCond>,
}

/// View is an ordered subset of an Index's rows after filtering and sorting.
pub struct View<'a> {
    pub(super) idx: &'a Index,
    pub(super) ov: &'a dyn Overlay,
    rows: Vec<usize>,
    filter: FilterSpec,
    sort_keys: Vec<SortKey>,
    /// annot names source columns adopted as the session's tags/comments.
    /// COL_DATA matching skips them, mirroring the Go View.annot field.
    pub(super) annot: AdoptedColumns,
}

impl<'a> View<'a> {
    /// NewView: an unfiltered, unsorted view of every row.
    pub fn new(idx: &'a Index, ov: &'a dyn Overlay) -> View<'a> {
        View {
            idx,
            ov,
            rows: (0..idx.row_count()).collect(),
            filter: FilterSpec::default(),
            sort_keys: Vec::new(),
            annot: AdoptedColumns::none(),
        }
    }

    pub fn set_annotation_columns(&mut self, ac: AdoptedColumns) {
        self.annot = ac;
    }

    /// set_filter_spec stores a spec without re-deriving rows. The parallel
    /// filter uses it to compile a CompiledFilter per worker without each
    /// worker running the (single-threaded) scan inside apply().
    pub(super) fn set_filter_spec(&mut self, spec: FilterSpec) {
        self.filter = spec;
    }

    /// set_rows replaces the visible row set directly. Used by the parallel
    /// filter to install the merged match list before an optional sort.
    pub fn set_rows(&mut self, rows: Vec<usize>) {
        self.rows = rows;
    }

    /// keep_compiled exposes the per-row predicate to the parallel scanner.
    pub(super) fn keep_compiled(&self, row: usize, rec: &[String], cf: &CompiledFilter) -> bool {
        self.keep(row, rec, cf)
    }

    pub fn len(&self) -> usize {
        self.rows.len()
    }

    /// headers: the underlying index's column names.
    pub fn headers(&self) -> &[String] {
        self.idx.headers()
    }

    pub fn is_empty(&self) -> bool {
        self.rows.is_empty()
    }

    /// Master maps a view position to a master row index.
    pub fn master(&self, view_row: usize) -> usize {
        self.rows[view_row]
    }

    /// rows: the current view ordering as master-row indices. Lets callers
    /// (e.g. the GUI, which can't hold a View across frames because it would
    /// self-reference the Index/Overlay it borrows) snapshot the computed
    /// order out of a transient View.
    pub fn rows(&self) -> &[usize] {
        &self.rows
    }

    /// row reads a master row's raw record straight from the index
    /// (uncached). Used by export.rs, which only needs each row once.
    pub fn row(&self, master_row: usize) -> std::io::Result<Vec<String>> {
        self.idx.row_uncached(master_row)
    }

    pub fn filter(&self) -> &FilterSpec {
        &self.filter
    }

    pub fn sort_keys(&self) -> &[SortKey] {
        &self.sort_keys
    }

    pub fn reset(&mut self) {
        self.filter = FilterSpec::default();
        self.sort_keys.clear();
        self.rows = (0..self.idx.row_count()).collect();
    }

    /// Apply sets the filter and re-derives the visible rows from the full
    /// set, then re-applies the current sort.
    pub fn apply(&mut self, spec: FilterSpec) -> Result<(), String> {
        self.filter = spec;
        self.refilter()?;
        self.resort()
    }

    /// Sort sets the ordering and reorders the visible rows.
    pub fn sort(&mut self, keys: Vec<SortKey>) -> Result<(), String> {
        self.sort_keys = keys;
        self.resort()
    }

    pub(super) fn compile_filter(&self) -> Result<CompiledFilter<'_>, String> {
        let spec = &self.filter;
        let mut cf = CompiledFilter {
            tagged_only: spec.tagged_only,
            tag: spec.tag.clone(),
            tags: spec.tags.iter().filter(|t| !t.is_empty()).cloned().collect(),
            has_query: false,
            query: None,
            query_col: spec.column,
            conds: Vec::new(),
            conds_any: spec.conds_any,
            expr: None,
            col_filters: Vec::new(),
        };
        if !spec.query.is_empty() {
            let m = Matcher::compile(&spec.query, spec.regexp, spec.cased).map_err(|e| e.to_string())?;
            cf.query = Some(m);
            cf.has_query = true;
        }
        if !spec.expr.is_empty() {
            let ast = super::query::parse_query(&spec.expr).map_err(|e| e.to_string())?;
            if let Some(ast) = ast {
                let now = chrono::Utc::now();
                let pred = self.compile_expr(&ast, spec.cased, now)?;
                cf.expr = Some(pred);
            }
        }
        for c in &spec.conds {
            let mut vals = Vec::new();
            for val in &c.values {
                if val.is_empty() {
                    continue;
                }
                vals.push(Matcher::compile(val, c.regexp, c.cased).map_err(|e| e.to_string())?);
            }
            if !vals.is_empty() {
                cf.conds.push(CompiledCond {
                    column: c.column,
                    all: c.all,
                    neg: c.neg,
                    vals,
                });
            }
        }
        for (col_ref, val) in &spec.col_filters {
            if val.is_empty() {
                continue;
            }
            let m = Matcher::col_filter(val, spec.cased).map_err(|e| e.to_string())?;
            cf.col_filters.push(CompiledCond {
                column: *col_ref,
                all: false,
                neg: false,
                vals: vec![m],
            });
        }
        Ok(cf)
    }

    fn refilter(&mut self) -> Result<(), String> {
        if self.filter.empty() {
            self.rows = (0..self.idx.row_count()).collect();
            return Ok(());
        }
        // Scoped so `cf` (which borrows self via its RowPred closures) is
        // dropped before we assign self.rows below; NLL otherwise treats the
        // drop glue of the boxed trait objects inside CompiledFilter as a
        // potential use of the borrow, conflicting with the mutable access.
        let matched = {
            let cf = self.compile_filter()?;
            let mut matched = Vec::with_capacity(self.rows.len());
            self.idx
                .scan(|i, rec| {
                    if self.keep(i, rec, &cf) {
                        matched.push(i);
                    }
                    true
                })
                .map_err(|e| e.to_string())?;
            matched
        };
        self.rows = matched;
        Ok(())
    }

    fn keep(&self, row: usize, rec: &[String], cf: &CompiledFilter) -> bool {
        if cf.tagged_only && self.ov.tags(row).is_empty() {
            return false;
        }
        if !cf.tag.is_empty() && !has_tag(&self.ov.tags(row), &cf.tag) {
            return false;
        }
        if !cf.tags.is_empty() && !has_any_tag(&self.ov.tags(row), &cf.tags) {
            return false;
        }
        if cf.has_query && cf.query_col != COL_NONE {
            if let Some(m) = &cf.query {
                if !self.column_match(row, rec, cf.query_col, m) {
                    return false;
                }
            }
        }
        if !cf.conds.is_empty() && !self.conds_match(row, rec, cf) {
            return false;
        }
        if let Some(expr) = &cf.expr {
            if !expr(row, rec) {
                return false;
            }
        }
        for c in &cf.col_filters {
            if !self.column_match(row, rec, c.column, &c.vals[0]) {
                return false;
            }
        }
        true
    }

    fn conds_match(&self, row: usize, rec: &[String], cf: &CompiledFilter) -> bool {
        for c in &cf.conds {
            let ok = self.cond_match(row, rec, c);
            if cf.conds_any && ok {
                return true;
            }
            if !cf.conds_any && !ok {
                return false;
            }
        }
        !cf.conds_any
    }

    fn cond_match(&self, row: usize, rec: &[String], c: &CompiledCond) -> bool {
        let hit = self.cond_hit(row, rec, c);
        if c.neg { !hit } else { hit }
    }

    fn cond_hit(&self, row: usize, rec: &[String], c: &CompiledCond) -> bool {
        for m in &c.vals {
            let hit = self.column_match(row, rec, c.column, m);
            if c.all && !hit {
                return false;
            }
            if !c.all && hit {
                return true;
            }
        }
        c.all
    }

    /// columnMatch: COL_ALL matches every column including the virtual
    /// Tags/Comment; COL_DATA matches data columns only, skipping adopted
    /// annotation columns; a specific ColumnRef matches just that cell.
    pub(super) fn column_match(&self, row: usize, rec: &[String], refc: ColumnRef, m: &Matcher) -> bool {
        match refc {
            COL_ALL => {
                if m.matches(&self.cell(row, rec, COL_TAGS)) || m.matches(&self.cell(row, rec, COL_COMMENT)) {
                    return true;
                }
                self.data_match(row, rec, m, false)
            }
            COL_DATA => self.data_match(row, rec, m, true),
            _ => m.matches(&self.cell(row, rec, refc)),
        }
    }

    fn data_match(&self, row: usize, rec: &[String], m: &Matcher, skip_annot: bool) -> bool {
        for c in 0..rec.len() {
            if skip_annot && self.annot.has(c as i32) {
                continue;
            }
            if m.matches(&self.cell(row, rec, c as ColumnRef)) {
                return true;
            }
        }
        false
    }

    fn resort(&mut self) -> Result<(), String> {
        if self.sort_keys.is_empty() {
            return Ok(());
        }
        let mut pos: HashMap<usize, usize> = HashMap::with_capacity(self.rows.len());
        for (p, &m) in self.rows.iter().enumerate() {
            pos.insert(m, p);
        }
        let sort_keys = self.sort_keys.clone();
        let mut keyvals: Vec<Vec<String>> = vec![Vec::new(); self.rows.len()];
        self.idx
            .scan(|i, rec| {
                if let Some(&p) = pos.get(&i) {
                    let ks: Vec<String> = sort_keys.iter().map(|sk| self.cell(i, rec, sk.col)).collect();
                    keyvals[p] = ks;
                }
                true
            })
            .map_err(|e| e.to_string())?;

        let mut order: Vec<usize> = (0..self.rows.len()).collect();
        order.sort_by(|&a, &b| {
            let ka = &keyvals[a];
            let kb = &keyvals[b];
            for (k, sk) in sort_keys.iter().enumerate() {
                let c = compare_cell(&ka[k], &kb[k]);
                if c == 0 {
                    continue;
                }
                let ord = if sk.desc { c > 0 } else { c < 0 };
                return if ord { std::cmp::Ordering::Less } else { std::cmp::Ordering::Greater };
            }
            std::cmp::Ordering::Equal
        });
        let reordered: Vec<usize> = order.iter().map(|&o| self.rows[o]).collect();
        self.rows = reordered;
        Ok(())
    }

    pub(super) fn cell(&self, master_row: usize, rec: &[String], refc: ColumnRef) -> String {
        match refc {
            COL_ROW_NUM => (master_row + 1).to_string(),
            COL_TAGS => self.ov.tags(master_row).join(", "),
            COL_COMMENT => self.ov.comment(master_row),
            c if c < 0 => String::new(),
            c => {
                let c = c as usize;
                if c >= rec.len() {
                    return String::new();
                }
                if let Some(v) = self.ov.cell_override(master_row, c) {
                    return v;
                }
                rec[c].clone()
            }
        }
    }

    /// resolveField maps a query field name to a column reference. A term
    /// with no field matches any column. Special names cover the virtual
    /// columns; anything else must match a data column header
    /// (case-insensitively). Used by query.rs and ioc.rs.
    pub(super) fn resolve_field(&self, field: &str, has_field: bool) -> Result<ColumnRef, String> {
        if !has_field {
            return Ok(COL_ALL);
        }
        match field.to_lowercase().as_str() {
            "tag" | "tags" => return Ok(COL_TAGS),
            "comment" | "comments" => return Ok(COL_COMMENT),
            "row" | "#" | "line" => return Ok(COL_ROW_NUM),
            "any" | "*" => return Ok(COL_ALL),
            _ => {}
        }
        for (i, h) in self.idx.headers().iter().enumerate() {
            if h.eq_ignore_ascii_case(field) {
                return Ok(i as ColumnRef);
            }
        }
        Err(format!("unknown field {field:?}"))
    }
}

/// compareCell orders two cell values: numeric when both parse as numbers,
/// otherwise case-insensitive string order. Empty strings sort last. Returns
/// -1/0/1 like the Go version (kept as i32 rather than Ordering so the
/// sign-based desc/asc flip in resort reads the same as the original).
fn compare_cell(a: &str, b: &str) -> i32 {
    if a == b {
        return 0;
    }
    if a.is_empty() {
        return 1;
    }
    if b.is_empty() {
        return -1;
    }
    let fa = a.trim().parse::<f64>();
    let fb = b.trim().parse::<f64>();
    if let (Ok(fa), Ok(fb)) = (fa, fb) {
        return if fa < fb {
            -1
        } else if fa > fb {
            1
        } else {
            0
        };
    }
    let la = a.to_lowercase();
    let lb = b.to_lowercase();
    match la.cmp(&lb) {
        std::cmp::Ordering::Less => -1,
        std::cmp::Ordering::Equal => 0,
        std::cmp::Ordering::Greater => 1,
    }
}

fn has_tag(tags: &[String], t: &str) -> bool {
    tags.iter().any(|x| x == t)
}

fn has_any_tag(tags: &[String], wanted: &[String]) -> bool {
    wanted.iter().any(|w| has_tag(tags, w))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::engine::index::open_temp;
    use crate::engine::session::Session;

    #[test]
    fn test_filter_and_sort() {
        let (idx, _p) = open_temp("host,event\nalpha,login\nbravo,logout\nalpha,logout\ncharlie,login\n");
        let s = Session::new("x");
        let mut v = View::new(&idx, &s);

        v.apply(FilterSpec { query: "alpha".to_string(), column: COL_ALL, ..Default::default() }).unwrap();
        assert_eq!(v.len(), 2, "filter alpha -> want 2 rows");

        // Sort the full set by host ascending.
        v.reset();
        v.sort(vec![SortKey { col: 0, desc: false }]).unwrap();
        let hosts: Vec<String> = (0..v.len())
            .map(|i| v.row(v.master(i)).unwrap()[0].clone())
            .collect();
        assert_eq!(hosts.join(","), "alpha,alpha,bravo,charlie");

        // Descending.
        v.sort(vec![SortKey { col: 0, desc: true }]).unwrap();
        let r0 = v.row(v.master(0)).unwrap();
        assert_eq!(r0[0], "charlie");
    }

    #[test]
    fn test_numeric_sort() {
        let (idx, _p) = open_temp("n\n10\n2\n1\n100\n");
        let s = Session::new("x");
        let mut v = View::new(&idx, &s);
        v.sort(vec![SortKey { col: 0, desc: false }]).unwrap();
        let got: Vec<String> = (0..v.len())
            .map(|i| v.row(v.master(i)).unwrap()[0].clone())
            .collect();
        assert_eq!(got.join(","), "1,2,10,100", "numeric sort should not be lexical");
    }

    #[test]
    fn test_regex_filter() {
        let (idx, _p) = open_temp("msg\nfailed login\nsuccess\nfailed logout\n");
        let s = Session::new("x");
        let mut v = View::new(&idx, &s);
        v.apply(FilterSpec { query: "^failed".to_string(), regexp: true, column: 0, ..Default::default() }).unwrap();
        assert_eq!(v.len(), 2);
    }

    #[test]
    fn test_column_cond_filter() {
        let (idx, _p) = open_temp("host,event\nalpha,login\nbravo,logout\nalpha,logout\ncharlie,login\n");
        let s = Session::new("x");
        let mut v = View::new(&idx, &s);

        // host contains alpha OR bravo (any within one column).
        v.apply(FilterSpec {
            conds: vec![ColumnCond { column: 0, values: vec!["alpha".into(), "bravo".into()], ..Default::default() }],
            ..Default::default()
        })
        .unwrap();
        assert_eq!(v.len(), 3, "host in {{alpha,bravo}}");

        // AND across two columns: host=alpha AND event=logout.
        v.apply(FilterSpec {
            conds: vec![
                ColumnCond { column: 0, values: vec!["alpha".into()], ..Default::default() },
                ColumnCond { column: 1, values: vec!["logout".into()], ..Default::default() },
            ],
            ..Default::default()
        })
        .unwrap();
        assert_eq!(v.len(), 1, "alpha AND logout");

        // OR across two columns: host=charlie OR event=logout.
        v.apply(FilterSpec {
            conds_any: true,
            conds: vec![
                ColumnCond { column: 0, values: vec!["charlie".into()], ..Default::default() },
                ColumnCond { column: 1, values: vec!["logout".into()], ..Default::default() },
            ],
            ..Default::default()
        })
        .unwrap();
        assert_eq!(v.len(), 3, "charlie OR logout"); // bravo/logout, alpha/logout, charlie/login
    }

    #[test]
    fn test_column_cond_neg() {
        let (idx, _p) = open_temp("host,event\nalpha,login\nbravo,logout\nalpha,cleared\ncharlie,signin\n");
        let s = Session::new("x");
        let mut v = View::new(&idx, &s);

        // event does NOT contain "log" — drops the login and logout rows.
        v.apply(FilterSpec {
            conds: vec![ColumnCond { column: 1, values: vec!["log".into()], neg: true, ..Default::default() }],
            ..Default::default()
        })
        .unwrap();
        assert_eq!(v.len(), 2, "event NOT containing log");

        // Excluding via regex works too: event does not match /log/.
        v.apply(FilterSpec {
            conds: vec![ColumnCond { column: 1, values: vec!["log".into()], regexp: true, neg: true, ..Default::default() }],
            ..Default::default()
        })
        .unwrap();
        assert_eq!(v.len(), 2, "event NOT matching /log/");

        // A negated condition combines with a positive one under AND: host=alpha
        // AND event does not contain "login" -> only the alpha/cleared row
        // (master 2).
        v.apply(FilterSpec {
            conds: vec![
                ColumnCond { column: 0, values: vec!["alpha".into()], ..Default::default() },
                ColumnCond { column: 1, values: vec!["login".into()], neg: true, ..Default::default() },
            ],
            ..Default::default()
        })
        .unwrap();
        assert_eq!(v.len(), 1);
        assert_eq!(v.master(0), 2);
    }

    // Covers the mechanism the header right-click "empty" / "not empty" menu
    // items rely on: a regexp `\S` condition matches non-empty cells, and
    // negating it keeps the empty (and whitespace-only) ones.
    #[test]
    fn test_column_emptiness() {
        let (idx, _p) = open_temp("host,note\nalpha,hit\nbravo,\ncharlie,   \ndelta,ok\n");
        let s = Session::new("x");
        let mut v = View::new(&idx, &s);

        // Not empty: note has a non-space char -> alpha and delta only.
        v.apply(FilterSpec {
            conds: vec![ColumnCond { column: 1, values: vec![r"\S".into()], regexp: true, ..Default::default() }],
            ..Default::default()
        })
        .unwrap();
        assert_eq!(v.len(), 2);
        assert_eq!(v.master(0), 0);
        assert_eq!(v.master(1), 3);

        // Empty: negation keeps the blank and whitespace-only rows -> bravo,
        // charlie.
        v.apply(FilterSpec {
            conds: vec![ColumnCond { column: 1, values: vec![r"\S".into()], regexp: true, neg: true, ..Default::default() }],
            ..Default::default()
        })
        .unwrap();
        assert_eq!(v.len(), 2);
        assert_eq!(v.master(0), 1);
        assert_eq!(v.master(1), 2);
    }

    #[test]
    fn test_column_cond_all_values() {
        let (idx, _p) = open_temp("msg\nfailed login attempt\nfailed logout\nsuccess login\n");
        let s = Session::new("x");
        let mut v = View::new(&idx, &s);
        // A single column must contain BOTH words.
        v.apply(FilterSpec {
            conds: vec![ColumnCond { column: 0, values: vec!["failed".into(), "login".into()], all: true, ..Default::default() }],
            ..Default::default()
        })
        .unwrap();
        assert_eq!(v.len(), 1, "failed AND login");
    }

    #[test]
    fn test_filter_tagged_only() {
        let (idx, _p) = open_temp("a\n1\n2\n3\n");
        let mut s = Session::new("x");
        s.set_mode(crate::engine::session::Mode::Investigator);
        s.add_tag(1, "keep").unwrap();
        let mut v = View::new(&idx, &s);
        v.apply(FilterSpec { tagged_only: true, ..Default::default() }).unwrap();
        assert_eq!(v.len(), 1);
        assert_eq!(v.master(0), 1);
    }

    #[test]
    fn test_filter_tags() {
        let (idx, _p) = open_temp("a\n1\n2\n3\n4\n");
        let mut s = Session::new("x");
        s.set_mode(crate::engine::session::Mode::Investigator);
        s.add_tag(0, "bad").unwrap();
        s.add_tag(1, "suspicious").unwrap();
        s.add_tag(2, "good").unwrap();
        let mut v = View::new(&idx, &s);
        v.apply(FilterSpec { tags: vec!["bad".into(), "suspicious".into()], ..Default::default() }).unwrap();
        assert_eq!(v.len(), 2);
        assert_eq!(v.master(0), 0);
        assert_eq!(v.master(1), 1);

        // An empty Tags slice must not filter anything.
        v.apply(FilterSpec { tags: vec![], ..Default::default() }).unwrap();
        assert_eq!(v.len(), 4);
    }
}
