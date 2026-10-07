//! Port of internal/model/ioc.go: IOC (indicator of compromise) matching. An
//! IOC list is plain text, one indicator per line, matched against every row
//! of a timeline. Rows that hit at least one indicator are surfaced to the
//! caller to tag.
//!
//! Line forms:
//!
//!   evil.exe            plain substring, matched against every data column
//!   /T[0-9]{4}/         a /regex/, matched against every data column
//!   `Summary=psexec AND Timestamp between 2024 and 2025
//!                       a leading backtick makes the rest a full filter
//!                       query (see query.rs), including before/after/
//!                       between on timestamp columns
//!
//! The plain and /regex/ forms skip the Tags and Comment fields, so an
//! indicator never matches a row on its own annotations. A backtick line
//! reaches them only when it names the field, e.g. `tag=beacon or
//! `comment=/psexec/.
//!
//! Lines beginning with a single # are ignored. A line beginning with ## is a
//! comment too, but it labels the block of indicators that follows it: every
//! indicator up to the next blank line inherits the ## text, and when one hits
//! a row the text is merged into the row's comment (prepended if the comment
//! is non-empty and does not already contain it). A blank line ends the block,
//! so indicators after it carry no comment until the next ##. A new ## starts
//! a fresh block. So in
//!
//!   ## Test
//!   1
//!   2
//!   3
//!
//!   4
//!
//! indicators 1, 2 and 3 carry "Test" and 4 carries nothing. Matching is
//! case-insensitive unless the caller asks otherwise.

use super::view::{Matcher, RowPred, View, COL_DATA};

/// IOCError records a line that failed to compile, so the UI can report
/// which indicators were skipped without aborting the whole run.
#[derive(Clone, Debug)]
pub struct IOCError {
    /// 1-based line number within the list.
    pub line: usize,
    /// The offending line, trimmed.
    pub text: String,
    /// Why it failed.
    pub err: String,
}

/// A compiled indicator: its row predicate and the ## comment (if any) from
/// the line directly above it, to stamp onto rows this indicator hits.
struct Indicator<'a> {
    pred: RowPred<'a>,
    comment: Option<String>,
}

/// IOCSet is a compiled IOC list: one indicator per usable line, plus the
/// errors for any lines that did not compile.
pub struct IOCSet<'a> {
    indicators: Vec<Indicator<'a>>,
    pub errors: Vec<IOCError>,
}

impl<'a> IOCSet<'a> {
    /// Count is the number of usable indicators compiled.
    pub fn count(&self) -> usize {
        self.indicators.len()
    }
}

impl<'a> View<'a> {
    /// CompileIOCs compiles an IOC list against this view's columns. Field
    /// names in backtick filter lines resolve against the view's headers,
    /// so compile against a view of the timeline being scanned.
    pub fn compile_iocs(&'a self, text: &str, cased: bool) -> IOCSet<'a> {
        let mut set = IOCSet { indicators: Vec::new(), errors: Vec::new() };
        let now = chrono::Utc::now();
        // The ## comment labelling the current block of indicators. A ## line
        // starts a block, a blank line ends it; every indicator in between
        // inherits the comment. A plain-# line is ignored without ending the
        // block.
        let mut block: Option<String> = None;
        for (n, raw) in text.split('\n').enumerate() {
            let line = raw.trim();
            if let Some(rest) = line.strip_prefix("##") {
                let rest = rest.trim();
                block = (!rest.is_empty()).then(|| rest.to_string());
                continue;
            }
            if line.is_empty() {
                block = None;
                continue;
            }
            if line.starts_with('#') {
                continue;
            }
            match self.compile_ioc_line(line, cased, now) {
                Ok(pred) => set.indicators.push(Indicator { pred, comment: block.clone() }),
                Err(e) => set.errors.push(IOCError { line: n + 1, text: line.to_string(), err: e }),
            }
        }
        set
    }

    fn compile_ioc_line(&'a self, line: &str, cased: bool, now: chrono::DateTime<chrono::Utc>) -> Result<RowPred<'a>, String> {
        if let Some(expr) = line.strip_prefix('`') {
            let expr = expr.trim();
            let ast = super::query::parse_query(expr)?;
            let ast = ast.ok_or_else(|| "empty filter".to_string())?;
            return self.compile_expr(&ast, cased, now);
        }
        // A plain string or /regex/ matched against every data column. The
        // Tags and Comment fields are skipped so an indicator does not hit a
        // row on its own annotations; a backtick filter line naming
        // tag=/comment= reaches them.
        let (val, use_regexp) = if line.len() >= 2 && line.starts_with('/') && line.ends_with('/') {
            (&line[1..line.len() - 1], true)
        } else {
            (line, false)
        };
        let m = Matcher::compile(val, use_regexp, cased).map_err(|e| e.to_string())?;
        Ok(Box::new(move |row, rec| self.column_match(row, rec, COL_DATA, &m)))
    }

    /// ScanIOCs runs the compiled indicators over every row of the
    /// underlying index and returns the master indices of rows that hit at
    /// least one, each paired with the ## comment of the first indicator it
    /// hit (None if that indicator carried no comment). It scans the full row
    /// set, not the current filtered view, so a filter in place does not hide
    /// matches.
    pub fn scan_iocs(&self, set: &IOCSet) -> std::io::Result<Vec<(usize, Option<String>)>> {
        let mut hits = Vec::new();
        self.scan_iocs_range(set, 0, self.idx.row_count(), |i, comment| {
            hits.push((i, comment.map(str::to_string)));
            true
        })?;
        Ok(hits)
    }

    /// scan_iocs_range runs the compiled indicators over rows [lo, hi),
    /// calling `on_hit` with each matching master index and the ## comment of
    /// the first indicator that matched it. Returning false from `on_hit`
    /// stops the scan (used for cancellation). Scans its own file range, so
    /// the parallel IOC runner can fan out over chunks.
    pub fn scan_iocs_range<F: FnMut(usize, Option<&str>) -> bool>(
        &self,
        set: &IOCSet,
        lo: usize,
        hi: usize,
        mut on_hit: F,
    ) -> std::io::Result<()> {
        if set.indicators.is_empty() {
            return Ok(());
        }
        self.idx.scan_range(lo, hi, |i, rec| {
            for ind in &set.indicators {
                if (ind.pred)(i, rec) {
                    return on_hit(i, ind.comment.as_deref());
                }
            }
            true
        })
    }
}

#[cfg(test)]
mod tests {
    use super::super::adopt::AdoptedColumns;
    use super::super::index::Index;
    use super::super::view::{Overlay, View};
    use std::collections::HashMap as Map;

    #[derive(Default)]
    struct TestOverlay {
        tags: Map<usize, Vec<String>>,
        comments: Map<usize, String>,
    }

    impl Overlay for TestOverlay {
        fn tags(&self, row: usize) -> Vec<String> {
            self.tags.get(&row).cloned().unwrap_or_default()
        }
        fn comment(&self, row: usize) -> String {
            self.comments.get(&row).cloned().unwrap_or_default()
        }
        fn cell_override(&self, _row: usize, _col: usize) -> Option<String> {
            None
        }
    }

    fn mk_idx(headers: &[&str], records: Vec<Vec<&str>>) -> Index {
        let headers: Vec<String> = headers.iter().map(|s| s.to_string()).collect();
        let records: Vec<Vec<String>> = records
            .into_iter()
            .map(|r| r.into_iter().map(|s| s.to_string()).collect())
            .collect();
        Index::new_memory(headers, records)
    }

    /// Drop the per-hit comments, keeping just the master indices.
    fn rows(hits: Vec<(usize, Option<String>)>) -> Vec<usize> {
        hits.into_iter().map(|(i, _)| i).collect()
    }

    fn ioc_idx() -> Index {
        mk_idx(
            &["Timestamp", "Summary", "Host"],
            vec![
                vec!["2024-01-01 00:00:00", "psexec service install", "dc1"],
                vec!["2024-01-02 00:00:00", "user logon", "ws2"],
                vec!["2024-01-03 00:00:00", "EVIL.exe dropped", "ws3"],
                vec!["2024-01-04 00:00:00", "T1059 detected", "dc1"],
            ],
        )
    }

    #[test]
    fn test_compile_and_scan_iocs() {
        let idx = ioc_idx();
        let ov = TestOverlay::default();
        let v = View::new(&idx, &ov);
        let list = "evil.exe\n/T[0-9]{4}/\n`Summary=psexec AND Host=dc1\n# a comment, ignored\n\nTimestamp before 2024-01-02";
        let set = v.compile_iocs(list, false);
        assert!(set.errors.is_empty(), "unexpected compile errors: {:?}", set.errors);
        assert_eq!(set.count(), 4);
        let hits = v.scan_iocs(&set).unwrap();
        assert_eq!(rows(hits), vec![0, 2, 3]);
    }

    #[test]
    fn test_ioc_errors_collected() {
        let idx = ioc_idx();
        let ov = TestOverlay::default();
        let v = View::new(&idx, &ov);
        let list = "/[/\n`Nope=x\ngood";
        let set = v.compile_iocs(list, false);
        assert_eq!(set.count(), 1);
        assert_eq!(set.errors.len(), 2);
        assert_eq!(set.errors[0].line, 1);
        assert_eq!(set.errors[1].line, 2);
    }

    #[test]
    fn test_ioc_skips_annotation_fields() {
        let idx = mk_idx(
            &["Timestamp", "Summary", "Host"],
            vec![
                vec!["2024-01-01 00:00:00", "clean", "dc1"],
                vec!["2024-01-02 00:00:00", "clean", "ws2"],
                vec!["2024-01-03 00:00:00", "clean", "ws3"],
                vec!["2024-01-04 00:00:00", "psexec", "dc1"],
            ],
        );
        let ov = TestOverlay {
            tags: Map::from([(1, vec!["beacon".to_string()])]),
            comments: Map::from([(2, "psexec seen here".to_string())]),
        };
        let v = View::new(&idx, &ov);
        let hits = v.scan_iocs(&v.compile_iocs("beacon\npsexec\n", false)).unwrap();
        assert_eq!(rows(hits), vec![3]);

        let hits = v.scan_iocs(&v.compile_iocs("`tag=beacon\n", false)).unwrap();
        assert_eq!(rows(hits), vec![1]);
    }

    #[test]
    fn test_ioc_skips_adopted_column() {
        let idx = mk_idx(&["Summary", "Tags"], vec![vec!["clean", "beacon"], vec!["beacon in summary", ""]]);
        let ov = TestOverlay::default();
        let mut v = View::new(&idx, &ov);
        v.set_annotation_columns(AdoptedColumns { tag: 1, comment: -1 });
        let hits = v.scan_iocs(&v.compile_iocs("beacon\n", false)).unwrap();
        assert_eq!(rows(hits), vec![1]);
    }

    #[test]
    fn test_scan_iocs_empty() {
        let idx = ioc_idx();
        let ov = TestOverlay::default();
        let v = View::new(&idx, &ov);
        let hits = v.scan_iocs(&v.compile_iocs("# nothing but a comment\n", false)).unwrap();
        assert!(hits.is_empty());
    }

    #[test]
    fn test_ioc_double_hash_labels_block() {
        let idx = ioc_idx();
        let ov = TestOverlay::default();
        let v = View::new(&idx, &ov);
        // Every indicator in the block under ## inherits the comment; the ##
        // line itself never matches a row.
        let list = "## lateral movement\npsexec\n/T[0-9]{4}/";
        let set = v.compile_iocs(list, false);
        assert!(set.errors.is_empty(), "unexpected errors: {:?}", set.errors);
        assert_eq!(set.count(), 2);
        let hits = v.scan_iocs(&set).unwrap();
        assert_eq!(
            hits,
            vec![
                (0, Some("lateral movement".to_string())),
                (3, Some("lateral movement".to_string())),
            ]
        );
    }

    #[test]
    fn test_ioc_double_hash_block_ends_at_blank() {
        // ## Test / psexec / <blank> / T-regex: the first indicator carries
        // "Test", the one after the blank line carries nothing.
        let idx = ioc_idx();
        let ov = TestOverlay::default();
        let v = View::new(&idx, &ov);
        let list = "## Test\npsexec\n\n/T[0-9]{4}/";
        let hits = v.scan_iocs(&v.compile_iocs(list, false)).unwrap();
        assert_eq!(hits, vec![(0, Some("Test".to_string())), (3, None)]);
    }

    #[test]
    fn test_ioc_double_hash_plain_hash_keeps_block() {
        // A plain-# line inside a block is ignored but does not end it: the
        // indicator after it still inherits the comment.
        let idx = ioc_idx();
        let ov = TestOverlay::default();
        let v = View::new(&idx, &ov);
        let list = "## Test\n# just a note\npsexec";
        let hits = v.scan_iocs(&v.compile_iocs(list, false)).unwrap();
        assert_eq!(hits, vec![(0, Some("Test".to_string()))]);
    }
}
