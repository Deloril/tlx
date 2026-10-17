//! Port of internal/model/export.go: write the rows currently visible in a
//! View, in view order, to a new CSV. Cell edits are applied and two
//! columns, Tags and Comment, are appended. The source file is read but
//! never modified.

use std::collections::HashSet;
use std::io::{self, Write};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

use super::view::{ColumnRef, View, COL_COMMENT, COL_TAGS};

/// How a tracked export ended: completed with a row count, or stopped early
/// because its cancel flag was set.
pub enum ExportOutcome {
    Done(usize),
    Cancelled,
}

/// How often the tracked export loops check the cancel flag and publish
/// progress. Per-row would thrash the atomics for no gain on a multi-GB file.
const CHECK_EVERY: usize = 4096;

/// Export writes every visible row of v to dest_path. Tags/comments/cell edits
/// come from the View's overlay. See export_omitting_with_comment for options.
pub fn export(v: &View, dest_path: &str) -> io::Result<()> {
    export_omitting_with_comment(v, dest_path, &HashSet::new(), "")
}

/// ExportOmitting is export with a set of source data columns dropped from
/// the output, keyed by column index. It is used when a timeline's existing
/// tag/comment columns have been adopted as the session's annotations, so
/// the appended Tags/Comment columns do not duplicate the source ones.
pub fn export_omitting(v: &View, dest_path: &str, omit: &HashSet<usize>) -> io::Result<()> {
    export_omitting_with_comment(v, dest_path, omit, "")
}

/// ExportOmittingWithComment is export_omitting with an optional
/// timeline-comment prelude. When comment is non-blank a leading row is
/// written before the header: two cells, "Timeline comments:" and the
/// comment text. The header and event rows follow unchanged.
pub fn export_omitting_with_comment(
    v: &View,
    dest_path: &str,
    omit: &HashSet<usize>,
    comment: &str,
) -> io::Result<()> {
    let progress = AtomicUsize::new(0);
    let cancel = AtomicBool::new(false);
    export_current_tracked(v, dest_path, omit, comment, &progress, &cancel).map(|_| ())
}

/// export_current_tracked is export_omitting_with_comment with a progress
/// counter and a cancel flag, so the GUI can run it on a worker thread for a
/// large file. `progress` is advanced to the view position reached; when
/// `cancel` is set it stops, removes the partial file and returns Cancelled.
/// Reads every cell through the View's overlay, so it works with either a live
/// Session or a Send+Sync snapshot of one.
pub fn export_current_tracked(
    v: &View,
    dest_path: &str,
    omit: &HashSet<usize>,
    comment: &str,
    progress: &AtomicUsize,
    cancel: &AtomicBool,
) -> io::Result<ExportOutcome> {
    let f = std::fs::File::create(dest_path)?;
    let mut w = CsvWriter::new(f);

    if !comment.trim().is_empty() {
        w.write_record(&["Timeline comments:".to_string(), comment.to_string()])?;
    }

    let headers = v.headers();
    let mut header: Vec<String> = Vec::with_capacity(headers.len() + 2);
    for (c, h) in headers.iter().enumerate() {
        if omit.contains(&c) {
            continue;
        }
        header.push(h.clone());
    }
    header.push("Tags".to_string());
    header.push("Comment".to_string());
    w.write_record(&header)?;

    let ncol = headers.len();
    for pos in 0..v.len() {
        if pos % CHECK_EVERY == 0 {
            if cancel.load(Ordering::Relaxed) {
                drop(w);
                let _ = std::fs::remove_file(dest_path);
                return Ok(ExportOutcome::Cancelled);
            }
            progress.store(pos, Ordering::Relaxed);
        }
        let master = v.master(pos);
        let rec = v.row(master)?;
        let mut out: Vec<String> = Vec::with_capacity(ncol + 2);
        for c in 0..ncol {
            if omit.contains(&c) {
                continue;
            }
            out.push(v.cell(master, &rec, c as ColumnRef));
        }
        out.push(v.cell(master, &rec, COL_TAGS));
        out.push(v.cell(master, &rec, COL_COMMENT));
        w.write_record(&out)?;
    }
    progress.store(v.len(), Ordering::Relaxed);
    w.flush()?;
    Ok(ExportOutcome::Done(v.len()))
}

/// One output column of a "To Timeline" export: a name and the source
/// columns feeding it, in drop order. A column fed by a single source carries
/// that cell's value verbatim; a column fed by two or more sources
/// concatenates "<name>: <data>; " for each source, so several timeline fields
/// collapse into one readable cell.
pub struct TimelineCol {
    pub name: String,
    /// (column ref, label). The label is the "<name>: " prefix used only when
    /// a column has two or more sources.
    pub sources: Vec<(ColumnRef, String)>,
}

/// export_timeline writes the visible rows of v to dest_path using a custom
/// column layout rather than the source columns + appended Tags/Comment. Each
/// output column pulls from zero or more source columns (data columns or the
/// virtual Tags/Comment). A non-blank timeline comment heads the file, same as
/// the plain export. When `tagged_only` is set, a row is written only if its
/// Tags or Comment is non-empty. The source CSV is never touched.
pub fn export_timeline(
    v: &View,
    dest_path: &str,
    cols: &[TimelineCol],
    comment: &str,
    tagged_only: bool,
) -> io::Result<()> {
    let progress = AtomicUsize::new(0);
    let cancel = AtomicBool::new(false);
    export_timeline_tracked(v, dest_path, cols, comment, tagged_only, &progress, &cancel).map(|_| ())
}

/// export_timeline_tracked is export_timeline with a progress counter and a
/// cancel flag, mirroring export_current_tracked, so the GUI can run a
/// "To Timeline" export off the UI thread. `progress` tracks rows scanned (not
/// written: tagged_only may skip some); the returned count is rows written.
pub fn export_timeline_tracked(
    v: &View,
    dest_path: &str,
    cols: &[TimelineCol],
    comment: &str,
    tagged_only: bool,
    progress: &AtomicUsize,
    cancel: &AtomicBool,
) -> io::Result<ExportOutcome> {
    let f = std::fs::File::create(dest_path)?;
    let mut w = CsvWriter::new(f);

    if !comment.trim().is_empty() {
        w.write_record(&["Timeline comments:".to_string(), comment.to_string()])?;
    }

    let header: Vec<String> = cols.iter().map(|c| c.name.clone()).collect();
    w.write_record(&header)?;

    // Tags and comments come from the overlay, not the row data, so the
    // tagged_only test can be made before touching the disk. That matters: a
    // tagged-only export of a multi-GB file otherwise reads every row only to
    // drop almost all of them. Reading the record (the slow part) is deferred
    // until a row is known to be written.
    const NO_REC: &[String] = &[];
    let mut written = 0usize;
    for pos in 0..v.len() {
        if pos % CHECK_EVERY == 0 {
            if cancel.load(Ordering::Relaxed) {
                drop(w);
                let _ = std::fs::remove_file(dest_path);
                return Ok(ExportOutcome::Cancelled);
            }
            progress.store(pos, Ordering::Relaxed);
        }
        let master = v.master(pos);
        if tagged_only
            && v.cell(master, NO_REC, COL_TAGS).trim().is_empty()
            && v.cell(master, NO_REC, COL_COMMENT).trim().is_empty()
        {
            continue;
        }
        let rec = v.row(master)?;
        let out: Vec<String> = cols.iter().map(|c| render_timeline_cell(v, master, &rec, c)).collect();
        w.write_record(&out)?;
        written += 1;
    }
    progress.store(v.len(), Ordering::Relaxed);
    w.flush()?;
    Ok(ExportOutcome::Done(written))
}

/// render_timeline_cell builds one output cell from a column's sources. Zero
/// sources yields an empty cell; one source yields the raw value; two or more
/// yield "<name>: <data>; " repeated, so the origin of each fragment survives
/// the merge.
fn render_timeline_cell(v: &View, master: usize, rec: &[String], c: &TimelineCol) -> String {
    match c.sources.as_slice() {
        [] => String::new(),
        [(refc, _name)] => v.cell(master, rec, *refc),
        many => {
            let mut s = String::new();
            for (refc, name) in many {
                s.push_str(name);
                s.push_str(": ");
                s.push_str(&v.cell(master, rec, *refc));
                s.push_str("; ");
            }
            s
        }
    }
}

/// A minimal CSV writer: always comma-delimited (whatever the source
/// delimiter was) and RFC4180 quoting (a field containing a comma, quote or
/// newline is wrapped in quotes with internal quotes doubled). Mirrors Go's
/// encoding/csv.Writer with Comma=','.
struct CsvWriter<W: Write> {
    w: W,
}

impl<W: Write> CsvWriter<W> {
    fn new(w: W) -> Self {
        CsvWriter { w }
    }

    fn write_record(&mut self, fields: &[String]) -> io::Result<()> {
        for (i, field) in fields.iter().enumerate() {
            if i > 0 {
                self.w.write_all(b",")?;
            }
            self.write_field(field)?;
        }
        // Go's encoding/csv.Writer defaults to UseCRLF=false, i.e. plain "\n"
        // line endings; match that so exported files are byte-for-byte
        // identical to the Go app's output.
        self.w.write_all(b"\n")
    }

    fn write_field(&mut self, field: &str) -> io::Result<()> {
        let needs_quote = field.contains(',') || field.contains('"') || field.contains('\n') || field.contains('\r');
        if !needs_quote {
            return self.w.write_all(field.as_bytes());
        }
        self.w.write_all(b"\"")?;
        for ch in field.chars() {
            if ch == '"' {
                self.w.write_all(b"\"\"")?;
            } else {
                let mut buf = [0u8; 4];
                self.w.write_all(ch.encode_utf8(&mut buf).as_bytes())?;
            }
        }
        self.w.write_all(b"\"")
    }

    fn flush(&mut self) -> io::Result<()> {
        self.w.flush()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::engine::index::open_temp;
    use crate::engine::session::Session;
    use crate::engine::view::View;

    fn read(path: &std::path::Path) -> String {
        std::fs::read_to_string(path).unwrap()
    }

    #[test]
    fn test_export() {
        let (idx, _p) = open_temp("host,event\nalpha,login\nbravo,logout\n");
        let mut s = Session::new(idx.path());
        s.set_mode(crate::engine::session::Mode::WorldWrite);
        s.add_tag(0, "flag").unwrap();
        s.set_comment(0, "look here").unwrap();
        s.set_cell(1, 1, "LOGOUT").unwrap();
        let v = View::new(&idx, &s);

        let dest = std::env::temp_dir().join("tlx-rust-test-export-out.csv");
        export(&v, dest.to_str().unwrap()).unwrap();
        let got = read(&dest);
        let want = "host,event,Tags,Comment\nalpha,login,flag,look here\nbravo,LOGOUT,,\n";
        assert_eq!(got, want);
        std::fs::remove_file(&dest).ok();
    }

    // A non-blank timeline comment heads the export with a two-cell row
    // before the header; a blank comment adds no row.
    #[test]
    fn test_export_with_comment() {
        let (idx, _p) = open_temp("host,event\nalpha,login\n");
        let s = Session::new(idx.path());
        let v = View::new(&idx, &s);

        let dest = std::env::temp_dir().join("tlx-rust-test-export-comment1.csv");
        export_omitting_with_comment(&v, dest.to_str().unwrap(), &HashSet::new(), "saw psexec at 03:14").unwrap();
        let got = read(&dest);
        let want = "Timeline comments:,saw psexec at 03:14\nhost,event,Tags,Comment\nalpha,login,,\n";
        assert_eq!(got, want);
        std::fs::remove_file(&dest).ok();

        // A blank comment writes no prelude row.
        let dest2 = std::env::temp_dir().join("tlx-rust-test-export-comment2.csv");
        export_omitting_with_comment(&v, dest2.to_str().unwrap(), &HashSet::new(), "   ").unwrap();
        let got2 = read(&dest2);
        assert_eq!(got2, "host,event,Tags,Comment\nalpha,login,,\n");
        std::fs::remove_file(&dest2).ok();
    }

    // Export always writes comma-delimited CSV even when the source was
    // tab- or otherwise-delimited, and quotes any value containing a comma
    // so a comma-based reader does not split it across columns.
    #[test]
    fn test_export_forces_comma_delimiter() {
        let (idx, _p) = open_temp("host\tcmd\nalpha\tps -enc AAA, -foo\nbravo\tplain\n");
        assert_eq!(idx.delimiter(), b'\t', "setup: delimiter should be tab");
        let s = Session::new(idx.path());
        let v = View::new(&idx, &s);

        let dest = std::env::temp_dir().join("tlx-rust-test-export-tsv.csv");
        export(&v, dest.to_str().unwrap()).unwrap();
        let got = read(&dest);
        let want = "host,cmd,Tags,Comment\n\
                    alpha,\"ps -enc AAA, -foo\",,\n\
                    bravo,plain,,\n";
        assert_eq!(got, want);
        std::fs::remove_file(&dest).ok();
    }

    // When a source CSV already has Tags/Comment columns that were adopted
    // as the session's annotations, export_omitting drops them so the
    // appended Tags/Comment columns do not duplicate the source ones.
    #[test]
    fn test_export_omitting() {
        let (idx, _p) = open_temp("host,Tags,Comment,event\nalpha,old-tag,old note,login\nbravo,,,logout\n");
        let mut s = Session::new(idx.path());
        let ac = crate::engine::adopt::detect_annotation_columns(idx.headers()); // Tag=1, Comment=2
        s.seed_from_columns(&idx, ac).unwrap();
        s.set_mode(crate::engine::session::Mode::Investigator);
        s.add_tag(1, "new-tag").unwrap();
        let v = View::new(&idx, &s);

        let dest = std::env::temp_dir().join("tlx-rust-test-export-omit.csv");
        let mut omit = HashSet::new();
        omit.insert(ac.tag as usize);
        omit.insert(ac.comment as usize);
        export_omitting(&v, dest.to_str().unwrap(), &omit).unwrap();
        let got = read(&dest);
        // Source Tags/Comment columns are gone; the appended ones carry the
        // seeded values (row 0) and the new tag (row 1).
        let want = "host,event,Tags,Comment\nalpha,login,old-tag,old note\nbravo,logout,new-tag,\n";
        assert_eq!(got, want);
        std::fs::remove_file(&dest).ok();
    }

    // A "To Timeline" tagged_only export driven off a Send+Sync snapshot (the
    // GUI's background path) must still see tags that were seeded from an
    // adopted source column, so seeded rows survive the filter. Mirrors
    // open_path: adopt a lowercase `tag` column, seed, snapshot, export.
    #[test]
    fn test_export_timeline_tagged_only_via_snapshot() {
        let (idx, _p) = open_temp("datetime,message,tag\n2020,login,bad\n2021,logout,\n2022,other,lateral\n");
        let mut s = Session::new(idx.path());
        let ac = crate::engine::adopt::detect_annotation_columns(idx.headers()); // tag=2
        s.seed_from_columns(&idx, ac).unwrap();

        // The GUI hands the worker a snapshot, not the live session.
        let snap = s.snapshot();
        let mut v = View::new(&idx, &snap);
        v.set_rows((0..idx.row_count()).collect());

        let cols = vec![
            TimelineCol { name: "Timestamp".into(), sources: vec![(0, "datetime".into())] },
            TimelineCol { name: "Event".into(), sources: vec![(1, "message".into())] },
            TimelineCol { name: "Tag".into(), sources: vec![(COL_TAGS, "Tags".into())] },
        ];

        let dest = std::env::temp_dir().join("tlx-rust-test-export-timeline-snap.csv");
        export_timeline(&v, dest.to_str().unwrap(), &cols, "", true).unwrap();
        let got = read(&dest);
        // Rows 0 and 2 carry seeded tags; row 1 (blank tag) is dropped.
        let want = "Timestamp,Event,Tag\n2020,login,bad\n2022,other,lateral\n";
        assert_eq!(got, want);
        std::fs::remove_file(&dest).ok();
    }

    // A "To Timeline" export maps source columns onto named output columns.
    // A single-source column carries the raw value; a multi-source column
    // concatenates "<name>: <data>; " per source. Virtual Tags/Comment can be
    // sources too.
    #[test]
    fn test_export_timeline() {
        let (idx, _p) = open_temp("datetime,desc,host\n2020,login,alpha\n2021,logout,bravo\n");
        let mut s = Session::new(idx.path());
        s.set_mode(crate::engine::session::Mode::WorldWrite);
        s.add_tag(0, "flag").unwrap();
        s.set_comment(0, "look here").unwrap();
        let v = View::new(&idx, &s);

        let cols = vec![
            TimelineCol { name: "Timestamp".into(), sources: vec![(0, "datetime".into())] },
            // Two sources -> "<name>: <data>; " for each.
            TimelineCol { name: "Event".into(), sources: vec![(1, "desc".into()), (2, "host".into())] },
            TimelineCol { name: "Comments".into(), sources: vec![(crate::engine::view::COL_COMMENT, "Comment".into())] },
            TimelineCol { name: "Tag".into(), sources: vec![(crate::engine::view::COL_TAGS, "Tags".into())] },
            // Zero sources -> empty cell.
            TimelineCol { name: "Notes".into(), sources: vec![] },
        ];

        let dest = std::env::temp_dir().join("tlx-rust-test-export-timeline.csv");
        export_timeline(&v, dest.to_str().unwrap(), &cols, "", false).unwrap();
        let got = read(&dest);
        let want = "Timestamp,Event,Comments,Tag,Notes\n\
                    2020,desc: login; host: alpha; ,look here,flag,\n\
                    2021,desc: logout; host: bravo; ,,,\n";
        assert_eq!(got, want);
        std::fs::remove_file(&dest).ok();

        // tagged_only drops the second row (no tag or comment); row 0 stays
        // because it has both.
        export_timeline(&v, dest.to_str().unwrap(), &cols, "", true).unwrap();
        let got = read(&dest);
        let want = "Timestamp,Event,Comments,Tag,Notes\n\
                    2020,desc: login; host: alpha; ,look here,flag,\n";
        assert_eq!(got, want);
        std::fs::remove_file(&dest).ok();
    }
}

