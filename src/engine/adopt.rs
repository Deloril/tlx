//! Port of internal/model/adopt.go: detecting timelines that already carry
//! annotation columns (a re-imported tlx export, or output from another
//! tool). Rather than adding tlx's own virtual columns beside them, we adopt
//! the existing ones: their values seed the session's tags and comments, and
//! the GUI shows the virtual Tags/Comment columns in their place.

use super::index::Index;
use super::session::Session;
use super::view::ColumnRef;

/// AdoptedColumns names the data columns an imported timeline already uses
/// for tags and comments, by source index. -1 means none was found.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct AdoptedColumns {
    pub tag: i32,
    pub comment: i32,
}

impl AdoptedColumns {
    pub fn none() -> Self {
        AdoptedColumns { tag: -1, comment: -1 }
    }

    /// Any reports whether either an existing tag or comment column was
    /// found.
    pub fn any(&self) -> bool {
        self.tag >= 0 || self.comment >= 0
    }

    /// Has reports whether col is one of the adopted columns.
    pub fn has(&self, col: ColumnRef) -> bool {
        col >= 0 && (col == self.tag || col == self.comment)
    }
}

/// annotationHeader reports whether a header names an annotation column: a
/// tag column (tag/tags) or a comment column (comment/comments/note/notes).
fn annotation_header(h: &str) -> (bool, bool) {
    match h.trim().to_lowercase().as_str() {
        "tag" | "tags" => (true, false),
        "comment" | "comments" | "note" | "notes" => (false, true),
        _ => (false, false),
    }
}

/// IsAnnotationHeader reports whether a header names a tag or comment
/// column. The master view uses it to drop such columns from a timeline's
/// data columns, since their content is already carried by the fixed Tags
/// and Comment columns.
pub fn is_annotation_header(h: &str) -> bool {
    let (is_tag, is_comment) = annotation_header(h);
    is_tag || is_comment
}

/// DetectAnnotationColumns looks for existing tag and comment columns by
/// header name (case-insensitive). The first match of each kind wins.
/// Indices are -1 when absent.
pub fn detect_annotation_columns(headers: &[String]) -> AdoptedColumns {
    let mut ac = AdoptedColumns::none();
    for (i, h) in headers.iter().enumerate() {
        let (is_tag, is_comment) = annotation_header(h);
        if is_tag && ac.tag < 0 {
            ac.tag = i as i32;
        }
        if is_comment && ac.comment < 0 {
            ac.comment = i as i32;
        }
    }
    ac
}

/// splitTags breaks a tag cell into individual tags on commas and
/// semicolons, trimming whitespace and dropping empties. A lone "-" is
/// dropped too: plaso/log2timeline writes "-" as its empty-value marker, so
/// an untagged row carries "-" rather than a blank cell, and we must not seed
/// that as a real tag.
pub fn split_tags(cell: &str) -> Vec<String> {
    cell.split(|c: char| c == ',' || c == ';')
        .map(|f| f.trim())
        .filter(|f| !f.is_empty() && *f != "-")
        .map(|f| f.to_string())
        .collect()
}

impl Session {
    /// SeedFromColumns fills the session's tags and comments from the given
    /// source columns of idx, one pass over the file. Tag cells are split on
    /// commas and semicolons. It bypasses the mode check (an import is not a
    /// user edit) and leaves the session clean. Columns set to -1 are
    /// skipped; nothing happens when neither is set.
    pub fn seed_from_columns(&mut self, idx: &Index, ac: AdoptedColumns) -> std::io::Result<()> {
        if !ac.any() {
            return Ok(());
        }
        idx.scan(|i, rec| {
            if ac.tag >= 0 && (ac.tag as usize) < rec.len() {
                for t in split_tags(&rec[ac.tag as usize]) {
                    self.seed_tag(i, &t);
                }
            }
            if ac.comment >= 0 && (ac.comment as usize) < rec.len() {
                let c = rec[ac.comment as usize].trim();
                if !c.is_empty() {
                    self.set_comment_raw(i, c.to_string());
                }
            }
            true
        })?;
        self.mark_saved();
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::engine::index::Index;
    use crate::engine::view::Overlay;

    #[test]
    fn test_detect_annotation_columns() {
        let cases: Vec<(Vec<&str>, AdoptedColumns)> = vec![
            (vec!["Time", "Summary", "Host"], AdoptedColumns { tag: -1, comment: -1 }),
            (vec!["Time", "Tags", "Comment"], AdoptedColumns { tag: 1, comment: 2 }),
            (vec!["tag", "note", "Summary"], AdoptedColumns { tag: 0, comment: 1 }),
            (vec!["Time", "Notes"], AdoptedColumns { tag: -1, comment: 1 }),
            // First of each kind wins.
            (vec!["Tag", "Tags", "Comment", "Note"], AdoptedColumns { tag: 0, comment: 2 }),
        ];
        for (headers, want) in cases {
            let headers: Vec<String> = headers.iter().map(|s| s.to_string()).collect();
            let got = detect_annotation_columns(&headers);
            assert_eq!(got, want, "detect_annotation_columns({headers:?})");
        }
    }

    #[test]
    fn test_split_tags() {
        let cases: Vec<(&str, Vec<&str>)> = vec![
            ("", vec![]),
            ("bad", vec!["bad"]),
            ("bad, lateral-movement", vec!["bad", "lateral-movement"]),
            ("a; b ;c", vec!["a", "b", "c"]),
            (" , ,, ", vec![]),
            // plaso's empty-value marker is not a tag.
            ("-", vec![]),
            (" - ", vec![]),
            ("bad, -", vec!["bad"]),
        ];
        for (input, want) in cases {
            let got = split_tags(input);
            let want: Vec<String> = want.into_iter().map(|s| s.to_string()).collect();
            assert_eq!(got, want, "split_tags({input:?})");
        }
    }

    #[test]
    fn test_seed_from_columns() {
        let idx = Index::new_memory(
            vec!["Time".into(), "Tags".into(), "Comment".into(), "Host".into()],
            vec![
                vec!["t0".into(), "bad, lateral-movement".into(), "looks bad".into(), "dc1".into()],
                vec!["t1".into(), "".into(), "".into(), "ws2".into()],
                vec!["t2".into(), "good".into(), "benign logon".into(), "ws3".into()],
            ],
        );
        let mut s = super::super::session::Session::new("(mem)");
        let ac = detect_annotation_columns(idx.headers());
        s.seed_from_columns(&idx, ac).unwrap();

        assert_eq!(s.tags(0), vec!["bad".to_string(), "lateral-movement".to_string()]);
        assert_eq!(s.comment(0), "looks bad");
        assert!(s.tags(1).is_empty(), "row1 tags should be none");
        assert_eq!(s.tags(2), vec!["good".to_string()]);
        assert_eq!(s.comment(2), "benign logon");
        // Seeding is not a user edit.
        assert!(!s.dirty(), "session should be clean after seeding");
        // The seeded tags are registered in the palette.
        assert!(s.tag_color("lateral-movement").is_some(), "seeded tag not registered in palette");
    }

    #[test]
    fn test_seed_from_columns_none_is_noop() {
        let idx = Index::new_memory(vec!["Time".into(), "Host".into()], vec![vec!["t0".into(), "dc1".into()]]);
        let mut s = super::super::session::Session::new("(mem)");
        s.seed_from_columns(&idx, detect_annotation_columns(idx.headers())).unwrap();
        assert!(s.tags(0).is_empty());
        assert_eq!(s.comment(0), "");
    }
}
