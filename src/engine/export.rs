//! Port of internal/model/export.go: write the rows currently visible in a
//! View, in view order, to a new CSV. Cell edits are applied and two
//! columns, Tags and Comment, are appended. The source file is read but
//! never modified.

use std::collections::HashSet;
use std::io::{self, Write};

use super::session::Session;
use super::view::{Overlay, View};

/// Export writes every visible row of v to destPath, with Session s
/// supplying tags/comments/cell overrides. See export_omitting_with_comment
/// for the full set of options.
pub fn export(v: &View, s: &Session, dest_path: &str) -> io::Result<()> {
    export_omitting_with_comment(v, s, dest_path, &HashSet::new(), "")
}

/// ExportOmitting is export with a set of source data columns dropped from
/// the output, keyed by column index. It is used when a timeline's existing
/// tag/comment columns have been adopted as the session's annotations, so
/// the appended Tags/Comment columns do not duplicate the source ones.
pub fn export_omitting(v: &View, s: &Session, dest_path: &str, omit: &HashSet<usize>) -> io::Result<()> {
    export_omitting_with_comment(v, s, dest_path, omit, "")
}

/// ExportOmittingWithComment is export_omitting with an optional
/// timeline-comment prelude. When comment is non-blank a leading row is
/// written before the header: two cells, "Timeline comments:" and the
/// comment text. The header and event rows follow unchanged.
pub fn export_omitting_with_comment(
    v: &View,
    s: &Session,
    dest_path: &str,
    omit: &HashSet<usize>,
    comment: &str,
) -> io::Result<()> {
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
        let master = v.master(pos);
        let rec = v.row(master)?;
        let mut out: Vec<String> = Vec::with_capacity(ncol + 2);
        for c in 0..ncol {
            if omit.contains(&c) {
                continue;
            }
            if let Some(val) = s.cell_override(master, c) {
                out.push(val);
            } else if c < rec.len() {
                out.push(rec[c].clone());
            } else {
                out.push(String::new());
            }
        }
        let tags = s.tags(master);
        out.push(tags.join(", "));
        out.push(s.comment(master));
        w.write_record(&out)?;
    }
    w.flush()
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
        export(&v, &s, dest.to_str().unwrap()).unwrap();
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
        export_omitting_with_comment(&v, &s, dest.to_str().unwrap(), &HashSet::new(), "saw psexec at 03:14").unwrap();
        let got = read(&dest);
        let want = "Timeline comments:,saw psexec at 03:14\nhost,event,Tags,Comment\nalpha,login,,\n";
        assert_eq!(got, want);
        std::fs::remove_file(&dest).ok();

        // A blank comment writes no prelude row.
        let dest2 = std::env::temp_dir().join("tlx-rust-test-export-comment2.csv");
        export_omitting_with_comment(&v, &s, dest2.to_str().unwrap(), &HashSet::new(), "   ").unwrap();
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
        export(&v, &s, dest.to_str().unwrap()).unwrap();
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
        export_omitting(&v, &s, dest.to_str().unwrap(), &omit).unwrap();
        let got = read(&dest);
        // Source Tags/Comment columns are gone; the appended ones carry the
        // seeded values (row 0) and the new tag (row 1).
        let want = "host,event,Tags,Comment\nalpha,login,old-tag,old note\nbravo,logout,new-tag,\n";
        assert_eq!(got, want);
        std::fs::remove_file(&dest).ok();
    }
}
