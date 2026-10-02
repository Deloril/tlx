//! Port of internal/model/highlight.go: locates the substrings a filter
//! matches so the UI can highlight them in the grid. Holds the positive
//! (non-negated) matchers of a spec: the boolean query's terms, the
//! per-column conditions and the per-column quick filters. Negated query
//! terms (NOT / !=) contribute nothing to highlight.

use super::query::QNode;
use super::view::{ColumnRef, FilterSpec, Matcher, View, COL_ALL, COL_NONE};

struct HlTerm {
    /// COL_ALL or a specific column.
    refc: ColumnRef,
    m: Matcher,
}

/// Highlighter locates match spans for a compiled FilterSpec.
pub struct Highlighter {
    terms: Vec<HlTerm>,
}

impl Highlighter {
    /// Empty reports whether there is nothing to highlight.
    pub fn empty(&self) -> bool {
        self.terms.is_empty()
    }

    /// Spans returns the byte ranges in text that the highlighter matches
    /// for the given column, merged so overlapping matches don't produce
    /// nested ranges. Terms scoped to another column are ignored; COL_ALL
    /// terms apply to every column.
    pub fn spans(&self, refc: ColumnRef, text: &str) -> Vec<(usize, usize)> {
        if self.empty() || text.is_empty() {
            return Vec::new();
        }
        let mut spans = Vec::new();
        for t in &self.terms {
            if t.refc != COL_ALL && t.refc != refc {
                continue;
            }
            spans.extend(t.m.spans(text));
        }
        merge_spans(spans)
    }
}

impl<'a> View<'a> {
    /// BuildHighlighter compiles the positive matchers of a spec. Parts that
    /// fail to compile are skipped; the same spec is validated separately
    /// by apply(), so an error there is surfaced to the user through the
    /// normal path.
    pub fn build_highlighter(&self, spec: &FilterSpec) -> Highlighter {
        let mut h = Highlighter { terms: Vec::new() };
        if !spec.expr.is_empty() {
            if let Ok(Some(ast)) = super::query::parse_query(&spec.expr) {
                self.collect_terms(&ast, false, spec.cased, &mut h);
            }
        }
        if !spec.query.is_empty() {
            if let Ok(m) = Matcher::compile(&spec.query, spec.regexp, spec.cased) {
                h.terms.push(HlTerm { refc: spec.column, m });
            }
        }
        for c in &spec.conds {
            if c.neg {
                // An excluded condition matches rows that DON'T contain the
                // value.
                continue;
            }
            for val in &c.values {
                if val.is_empty() {
                    continue;
                }
                if let Ok(m) = Matcher::compile(val, c.regexp, c.cased) {
                    h.terms.push(HlTerm { refc: c.column, m });
                }
            }
        }
        for (refc, val) in &spec.col_filters {
            if val.is_empty() {
                continue;
            }
            if let Ok(m) = Matcher::col_filter(val, spec.cased) {
                h.terms.push(HlTerm { refc: *refc, m });
            }
        }
        h
    }

    /// collectTerms walks the query tree adding positive terms. `neg` tracks
    /// whether an odd number of NOTs currently applies; a term is negated
    /// (and skipped) when that parity differs from the term's own != flag.
    fn collect_terms(&self, n: &QNode, neg: bool, cased: bool, h: &mut Highlighter) {
        match n.binary() {
            Some((super::query::QBinOp::And, l, r)) | Some((super::query::QBinOp::Or, l, r)) => {
                self.collect_terms(l, neg, cased, h);
                self.collect_terms(r, neg, cased, h);
            }
            None => {
                if let Some(child) = n.not_child() {
                    self.collect_terms(child, !neg, cased, h);
                    return;
                }
                if let Some((field, has_field, term_neg, value, regex)) = n.term() {
                    if neg != term_neg {
                        return; // effectively negated
                    }
                    let mut refc = self.resolve_field(field, has_field).unwrap_or(COL_NONE);
                    if refc == COL_NONE {
                        refc = COL_ALL;
                    }
                    if let Ok(m) = Matcher::compile(value, regex, cased) {
                        h.terms.push(HlTerm { refc, m });
                    }
                }
                // Time-comparison terms contribute nothing to highlighting
                // (same as the Go version, which only calls compileMatcher
                // on n.value and skips time terms implicitly since they
                // have no regular value/regex to highlight).
            }
        }
    }
}

/// mergeSpans sorts and coalesces overlapping or touching ranges.
fn merge_spans(mut spans: Vec<(usize, usize)>) -> Vec<(usize, usize)> {
    if spans.len() < 2 {
        return spans;
    }
    spans.sort_by(|a, b| a.0.cmp(&b.0).then(a.1.cmp(&b.1)));
    let mut out: Vec<(usize, usize)> = Vec::with_capacity(spans.len());
    out.push(spans[0]);
    for &(s, e) in &spans[1..] {
        let last = out.last_mut().unwrap();
        if s <= last.1 {
            if e > last.1 {
                last.1 = e;
            }
        } else {
            out.push((s, e));
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::super::index::Index;
    use super::super::view::{ColumnCond, Overlay, View, COL_ALL};
    use std::collections::HashMap;

    #[derive(Default)]
    struct TestOverlay;
    impl Overlay for TestOverlay {
        fn tags(&self, _row: usize) -> Vec<String> {
            Vec::new()
        }
        fn comment(&self, _row: usize) -> String {
            String::new()
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

    #[test]
    fn test_highlighter_spans() {
        let idx = mk_idx(&["Host", "Summary"], vec![vec!["ws1", "ran /tmp/evil and /tmp/other"]]);
        let ov = TestOverlay;
        let v = View::new(&idx, &ov);
        let summary = 1i32;

        // regex on named column
        let h = v.build_highlighter(&super::FilterSpec { expr: "Summary=/tmp/".to_string(), ..Default::default() });
        assert_eq!(h.spans(summary, "ran /tmp/evil and /tmp/other"), vec![(5, 8), (19, 22)]);

        // column-scoped term does not highlight other columns
        let h = v.build_highlighter(&super::FilterSpec { expr: "Summary=tmp".to_string(), ..Default::default() });
        assert_eq!(h.spans(0, "tmpish host"), Vec::<(usize, usize)>::new());

        // case-insensitive literal
        let h = v.build_highlighter(&super::FilterSpec { expr: "EVIL".to_string(), ..Default::default() });
        assert_eq!(h.spans(summary, "ran /tmp/evil and /tmp/other"), vec![(9, 13)]);

        // negated term is not highlighted
        let h = v.build_highlighter(&super::FilterSpec { expr: "NOT tmp".to_string(), ..Default::default() });
        assert_eq!(h.spans(summary, "ran /tmp/evil"), Vec::<(usize, usize)>::new());

        // per-column quick filter
        let mut cf = HashMap::new();
        cf.insert(summary, "other".to_string());
        let h = v.build_highlighter(&super::FilterSpec { col_filters: cf, ..Default::default() });
        assert_eq!(h.spans(summary, "ran /tmp/evil and /tmp/other"), vec![(23, 28)]);
    }

    #[test]
    fn test_highlighter_merges_overlap() {
        let idx = mk_idx(&["A"], vec![vec!["aaaa"]]);
        let ov = TestOverlay;
        let v = View::new(&idx, &ov);
        let h = v.build_highlighter(&super::FilterSpec {
            conds: vec![ColumnCond { column: COL_ALL, values: vec!["aa".to_string(), "aaa".to_string()], ..Default::default() }],
            ..Default::default()
        });
        assert_eq!(h.spans(0, "aaaa"), vec![(0, 4)]);
    }
}
