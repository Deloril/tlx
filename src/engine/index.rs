//! Port of internal/model/index.go: a read-only, random-access view of a
//! delimited file. Holds one i64 (well, i64-sized) byte offset per record
//! rather than the record contents, so memory stays proportional to row
//! count, not file size.
//!
//! Faithful to the Go original: single sequential pass to build `bounds`, a
//! quote-aware scanBounds state machine (inQuotes/pending_escape/
//! at_field_start) ported byte-for-byte, BOM skip, delimiter sniffing from the
//! header line, Row(i) via ReadAt of the exact span, and Scan() for a fast
//! sequential full pass used by filter/sort.

use std::fs::File;
use std::io::{self, BufRead, BufReader, Read, Seek, SeekFrom};
use std::sync::Mutex;

#[cfg(unix)]
use std::os::unix::fs::FileExt;

use super::cache::RowCache;

/// Candidate delimiters, in the order we prefer them on a tie (mirrors Go's
/// delimCandidates).
const DELIM_CANDIDATES: [u8; 4] = [b',', b'\t', b';', b'|'];

pub struct Index {
    path: String,
    delim: u8,
    headers: Vec<String>,
    #[allow(dead_code)] // kept for parity with the Go struct; not yet read back anywhere
    bom_len: i64,

    /// bounds has len == record_count+1. Record k occupies bytes
    /// [bounds[k], bounds[k+1]). Record 0 is the header; data record i is
    /// record i+1. The final element is the file size (a terminator).
    bounds: Vec<i64>,

    f: Option<File>,
    /// Row LRU behind a mutex so `row()` can take `&self` and the Index can be
    /// shared across threads (Arc<Index>) for the parallel scan. The lock is
    /// held only around the cheap get/put, never across I/O.
    cache: Mutex<RowCache>,

    /// In-memory backing for synthetic tables (NewMemoryIndex equivalent).
    /// When Some, the file fields above are unused.
    mem: Option<Vec<Vec<String>>>,
}

impl Index {
    /// Port of model.NewMemoryIndex: builds an index backed by in-memory
    /// records rather than a file. Not yet wired into the app (no synthetic
    /// master-timeline feature in this prototype); kept so the shape matches
    /// the Go engine and the GUI layer can grow into it.
    #[allow(dead_code)]
    pub fn new_memory(headers: Vec<String>, records: Vec<Vec<String>>) -> Self {
        Index {
            path: "(memory)".to_string(),
            delim: b',',
            headers,
            bom_len: 0,
            bounds: Vec::new(),
            f: None,
            cache: Mutex::new(RowCache::new(8192)),
            mem: Some(records),
        }
    }

    /// Port of model.Open: indexes `path` with a single sequential pass.
    /// `on_progress` (optional) is called periodically with bytes scanned and
    /// total.
    pub fn open(
        path: &str,
        on_progress: Option<&mut dyn FnMut(i64, i64)>,
    ) -> io::Result<Index> {
        let mut f = File::open(path)?;
        let size = f.metadata()?.len() as i64;

        let mut bom_len: i64 = 0;
        // Detect and skip a UTF-8 BOM.
        {
            let mut head = [0u8; 3];
            let n = read_full_best_effort(&mut f, &mut head)?;
            if n == 3 && head[0] == 0xEF && head[1] == 0xBB && head[2] == 0xBF {
                bom_len = 3;
            }
        }
        f.seek(SeekFrom::Start(bom_len as u64))?;

        // Detect the delimiter from the header line before scanning record
        // bounds: scan_bounds needs it to tell a field-opening quote from a
        // literal quote in the middle of an unquoted field.
        let header_line = read_first_line(&mut f, bom_len)?;
        let delim = detect_delim(&header_line);
        f.seek(SeekFrom::Start(bom_len as u64))?;

        let bounds = scan_bounds(&mut f, bom_len, size, delim, on_progress)?;
        if bounds.len() < 2 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("{path}: no records found"),
            ));
        }

        let mut idx = Index {
            path: path.to_string(),
            delim,
            headers: Vec::new(),
            bom_len,
            bounds,
            f: Some(f),
            cache: Mutex::new(RowCache::new(8192)),
            mem: None,
        };

        // Header record is bounds[0]..bounds[1]; parse it into column names.
        // trim_eol mirrors Row()/Scan(): our hand-rolled parse_record (unlike
        // Go's encoding/csv) does not stop at a bare newline, only at field
        // boundaries, so a trailing "\n" must be stripped explicitly or it
        // ends up appended to the last header/field value.
        let header_bytes = idx.read_span(idx.bounds[0], idx.bounds[1])?;
        idx.headers = parse_record(trim_eol(&header_bytes), delim)?;
        Ok(idx)
    }

    pub fn headers(&self) -> &[String] {
        &self.headers
    }

    pub fn delimiter(&self) -> u8 {
        self.delim
    }

    #[allow(dead_code)] // mirrors Go's Index.Path(); not yet surfaced in the UI
    pub fn path(&self) -> &str {
        &self.path
    }

    /// RowCount: number of data records (header excluded).
    pub fn row_count(&self) -> usize {
        if let Some(mem) = &self.mem {
            mem.len()
        } else {
            self.bounds.len().saturating_sub(2)
        }
    }

    /// Row returns data record i (0-based, header excluded). Results are
    /// cached in an LRU. Takes `&self` (the cache is behind a mutex) so the
    /// Index can be shared across threads for the parallel scan.
    pub fn row(&self, i: usize) -> io::Result<Vec<String>> {
        if i >= self.row_count() {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("row {i} out of range [0,{})", self.row_count()),
            ));
        }
        if let Some(mem) = &self.mem {
            return Ok(mem[i].clone());
        }
        if let Some(rec) = self.cache.lock().unwrap().get(i) {
            return Ok(rec);
        }
        let raw = self.read_span(self.bounds[i + 1], self.bounds[i + 2])?;
        let rec = parse_record(trim_eol(&raw), self.delim)?;
        self.cache.lock().unwrap().put(i, rec.clone());
        Ok(rec)
    }

    /// row_uncached reads data record i without going through the LRU cache,
    /// using only `&self` (opens a fresh file handle per call). Used by
    /// callers that only hold an immutable Index reference, such as View
    /// (export.rs reads rows this way). Row() is preferred when `&mut self`
    /// is available, since it benefits from the cache.
    pub fn row_uncached(&self, i: usize) -> io::Result<Vec<String>> {
        if i >= self.row_count() {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("row {i} out of range [0,{})", self.row_count()),
            ));
        }
        if let Some(mem) = &self.mem {
            return Ok(mem[i].clone());
        }
        let start = self.bounds[i + 1];
        let end = self.bounds[i + 2];
        let len = (end - start) as usize;
        let mut buf = vec![0u8; len];
        let mut f = File::open(&self.path)?;
        read_at_compat(&mut f, start as u64, &mut buf)?;
        let rec = parse_record(trim_eol(&buf), self.delim)?;
        Ok(rec)
    }

    fn read_span(&self, start: i64, end: i64) -> io::Result<Vec<u8>> {
        if end < start {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("bad span [{start},{end})"),
            ));
        }
        let len = (end - start) as usize;
        let mut buf = vec![0u8; len];
        let f = self.f.as_ref().expect("read_span called on memory index");
        read_at_compat(f, start as u64, &mut buf)?;
        Ok(buf)
    }

    /// Scan streams every data record sequentially from the start of the
    /// file, calling `fn_` with (index, record). Far faster than calling
    /// Row() in a loop since there's no per-row seek. Return false from
    /// `fn_` to stop early.
    pub fn scan<F: FnMut(usize, &[String]) -> bool>(&self, fn_: F) -> io::Result<()> {
        self.scan_range(0, self.row_count(), fn_)
    }

    /// scan_range streams data records [lo, hi) sequentially from its own file
    /// handle, calling `fn_` with (absolute index, record). Each call opens a
    /// private handle and reads with a positional cursor, so several ranges can
    /// scan one file concurrently without a shared seek position — this is what
    /// the parallel filter fans out over. Return false from `fn_` to stop early.
    pub fn scan_range<F: FnMut(usize, &[String]) -> bool>(
        &self,
        lo: usize,
        hi: usize,
        mut fn_: F,
    ) -> io::Result<()> {
        let n = self.row_count();
        let hi = hi.min(n);
        if lo >= hi {
            return Ok(());
        }
        if let Some(mem) = &self.mem {
            for i in lo..hi {
                if !fn_(i, &mem[i]) {
                    return Ok(());
                }
            }
            return Ok(());
        }
        let mut f = File::open(&self.path)?;
        f.seek(SeekFrom::Start(self.bounds[lo + 1] as u64))?;
        let mut br = BufReader::with_capacity(1 << 20, f);
        let mut buf: Vec<u8> = Vec::new();
        for i in lo..hi {
            let span = (self.bounds[i + 2] - self.bounds[i + 1]) as usize;
            if buf.len() < span {
                buf.resize(span, 0);
            }
            br.read_exact(&mut buf[..span])?;
            let rec = parse_record(trim_eol(&buf[..span]), self.delim)?;
            if !fn_(i, &rec) {
                return Ok(());
            }
        }
        Ok(())
    }
}

#[cfg(unix)]
fn read_at_compat(f: &File, offset: u64, buf: &mut [u8]) -> io::Result<()> {
    // Mirror Go's ReadAt: positional read, no shared seek cursor, so it is
    // safe to call concurrently on one shared File from several threads.
    match f.read_at(buf, offset) {
        Ok(_) => Ok(()),
        Err(e) => Err(e),
    }
}

#[cfg(not(unix))]
fn read_at_compat(f: &File, offset: u64, buf: &mut [u8]) -> io::Result<()> {
    // seek_read is the positional-read equivalent on Windows; like pread it
    // does not disturb a shared cursor, so a shared File is safe here too.
    #[cfg(windows)]
    {
        use std::os::windows::fs::FileExt;
        let mut read = 0;
        while read < buf.len() {
            let n = f.seek_read(&mut buf[read..], offset + read as u64)?;
            if n == 0 {
                break;
            }
            read += n;
        }
        Ok(())
    }
    #[cfg(not(windows))]
    {
        // Fallback for other non-unix targets: clone the handle so each read
        // has its own cursor.
        let mut owned = f.try_clone()?;
        owned.seek(SeekFrom::Start(offset))?;
        owned.read_exact(buf)
    }
}

fn read_full_best_effort(f: &mut File, buf: &mut [u8]) -> io::Result<usize> {
    // Go's io.ReadFull: reads until buf is full or EOF; returns n read so far
    // on EOF without erroring the caller (Open ignores the error).
    let mut n = 0;
    while n < buf.len() {
        match f.read(&mut buf[n..]) {
            Ok(0) => break,
            Ok(k) => n += k,
            Err(ref e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        }
    }
    Ok(n)
}

/// readFirstLine: returns the bytes of the first line at offset `start`, up
/// to and including the terminating newline (or EOF).
fn read_first_line(f: &mut File, start: i64) -> io::Result<Vec<u8>> {
    f.seek(SeekFrom::Start(start as u64))?;
    let mut br = BufReader::with_capacity(1 << 16, &mut *f);
    let mut line = Vec::new();
    br.read_until(b'\n', &mut line)?;
    Ok(line)
}

/// scanBounds: walks the file once, recording the start offset of every
/// record. Quote-aware: newlines and delimiters inside "..." fields do not
/// end a record, and "" is an escaped quote. This is a byte-for-byte port of
/// the Go state machine so embedded-newline forensic CSVs index identically.
fn scan_bounds(
    f: &mut File,
    start: i64,
    size: i64,
    delim: u8,
    on_progress: Option<&mut dyn FnMut(i64, i64)>,
) -> io::Result<Vec<i64>> {
    let mut br = BufReader::with_capacity(1 << 20, &mut *f);
    let mut bounds: Vec<i64> = Vec::with_capacity(1024);
    let mut off = start;
    bounds.push(off); // first record starts here

    // at_field_start is true at the beginning of each record and right after
    // a delimiter. A quote only opens a quoted field there; a quote anywhere
    // else is a literal character (matching RFC 4180 / encoding/csv).
    let mut in_quotes = false;
    let mut pending_escape = false;
    let mut at_field_start = true;
    let mut since_report: i64 = 0;

    let mut on_progress = on_progress;

    // Read in chunks for speed, but process byte-by-byte to preserve the
    // exact state machine semantics of the Go version.
    let mut chunk = [0u8; 1 << 16];
    loop {
        let n = br.read(&mut chunk)?;
        if n == 0 {
            break;
        }
        for &b in &chunk[..n] {
            off += 1;
            since_report += 1;
            if since_report >= 1 << 22 {
                // ~4 MB
                if let Some(cb) = on_progress.as_deref_mut() {
                    cb(off - start, size - start);
                }
                since_report = 0;
            }

            if in_quotes {
                if pending_escape {
                    pending_escape = false;
                    if b == b'"' {
                        continue; // "" -> literal quote, stay in field
                    }
                    in_quotes = false; // the prior quote closed the field
                    // fall through and reprocess b in unquoted state
                } else if b == b'"' {
                    pending_escape = true;
                    continue;
                } else {
                    continue; // ordinary byte inside a quoted field
                }
            }

            // unquoted state
            if b == b'"' {
                if at_field_start {
                    in_quotes = true;
                }
                at_field_start = false;
            } else if b == delim {
                at_field_start = true;
            } else if b == b'\n' {
                bounds.push(off);
                at_field_start = true;
            } else {
                at_field_start = false;
            }
        }
    }

    // Drop a trailing empty record if the file ended with a newline.
    if let Some(&last) = bounds.last() {
        if last == off {
            bounds.pop();
        }
    }
    bounds.push(size); // terminator
    if let Some(cb) = on_progress.as_deref_mut() {
        cb(size - start, size - start);
    }
    Ok(bounds)
}

fn detect_delim(header: &[u8]) -> u8 {
    let mut best = b',';
    let mut best_count: i64 = -1;
    for &d in &DELIM_CANDIDATES {
        let c = count_outside_quotes(header, d);
        if c > best_count {
            best = d;
            best_count = c;
        }
    }
    best
}

fn count_outside_quotes(b: &[u8], delim: u8) -> i64 {
    let mut in_quotes = false;
    let mut pending_escape = false;
    let mut count = 0i64;
    for &c in b {
        if in_quotes {
            if pending_escape {
                pending_escape = false;
                if c == b'"' {
                    continue;
                }
                in_quotes = false;
            } else if c == b'"' {
                pending_escape = true;
                continue;
            } else {
                continue;
            }
        }
        if c == b'"' {
            in_quotes = true;
        } else if c == delim {
            count += 1;
        }
    }
    count
}

/// parseRecord: parse exactly one already-bounded CSV/TSV record (one line,
/// quote-balanced, already identified by scan_bounds) into fields.
///
/// This is a direct state-machine port rather than a call into the `csv`
/// crate: constructing a `csv::Reader` per record (one per row, called from
/// the hot `Scan` loop that filter/sort run over all 1.5M rows) was the
/// dominant cost in early profiling — about 100x slower than this. The
/// state machine mirrors scan_bounds/encoding-csv's LazyQuotes semantics: a
/// quote only opens a quoted field right at a field boundary; "" inside a
/// quoted field is an escaped literal quote; a bare quote appearing mid-field
/// is kept as a literal character (LazyQuotes), matching Go's behaviour.
fn parse_record(raw: &[u8], delim: u8) -> io::Result<Vec<String>> {
    let mut fields = Vec::new();
    let mut cur: Vec<u8> = Vec::with_capacity(32);
    let mut in_quotes = false;
    let mut pending_escape = false;
    let mut at_field_start = true;
    let mut i = 0;
    while i < raw.len() {
        let b = raw[i];
        if in_quotes {
            if pending_escape {
                pending_escape = false;
                if b == b'"' {
                    cur.push(b'"');
                    i += 1;
                    continue;
                }
                in_quotes = false;
                // fall through: reprocess b in unquoted state (no advance)
            } else if b == b'"' {
                pending_escape = true;
                i += 1;
                continue;
            } else {
                cur.push(b);
                i += 1;
                continue;
            }
        }
        // unquoted state
        if b == b'"' {
            if at_field_start {
                in_quotes = true;
            } else {
                cur.push(b); // literal quote mid-field (LazyQuotes)
            }
            at_field_start = false;
            i += 1;
        } else if b == delim {
            fields.push(String::from_utf8_lossy(&cur).into_owned());
            cur.clear();
            at_field_start = true;
            i += 1;
        } else {
            cur.push(b);
            at_field_start = false;
            i += 1;
        }
    }
    fields.push(String::from_utf8_lossy(&cur).into_owned());
    Ok(fields)
}

fn trim_eol(b: &[u8]) -> &[u8] {
    let b = if b.ends_with(b"\n") { &b[..b.len() - 1] } else { b };
    if b.ends_with(b"\r") { &b[..b.len() - 1] } else { b }
}

/// Test-only helper mirroring Go's openT/writeTemp: writes `content` to a
/// freshly named file under the OS temp dir and opens an Index over it. The
/// file is left on disk (tests run in a throwaway CI/dev sandbox and the
/// files are tiny); each call uses a unique name so concurrent tests don't
/// collide.
#[cfg(test)]
pub(super) fn open_temp(content: &str) -> (Index, std::path::PathBuf) {
    use std::sync::atomic::{AtomicU64, Ordering};
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let n = COUNTER.fetch_add(1, Ordering::Relaxed);
    let path = std::env::temp_dir().join(format!(
        "tlx-rust-test-{}-{}-{}.csv",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos(),
        n
    ));
    std::fs::write(&path, content).expect("write temp csv");
    let idx = Index::open(path.to_str().unwrap(), None).expect("open temp csv");
    (idx, path)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_index_basic() {
        let (mut idx, _p) = open_temp("a,b,c\n1,2,3\n4,5,6\n");
        assert_eq!(idx.row_count(), 2);
        assert_eq!(idx.headers().join("|"), "a|b|c");
        let r = idx.row(1).unwrap();
        assert_eq!(r.join("|"), "4|5|6");
    }

    #[test]
    fn test_index_no_trailing_newline() {
        let (mut idx, _p) = open_temp("a,b\n1,2\n3,4");
        assert_eq!(idx.row_count(), 2);
        let r = idx.row(1).unwrap();
        assert_eq!(r.join("|"), "3|4");
    }

    #[test]
    fn test_index_quoted_newline() {
        // A quoted field spans two physical lines; it must stay one record.
        let (mut idx, _p) = open_temp("id,msg\n1,\"line one\nline two\"\n2,ok\n");
        assert_eq!(idx.row_count(), 2);
        let r = idx.row(0).unwrap();
        assert_eq!(r[1], "line one\nline two");
        let r = idx.row(1).unwrap();
        assert_eq!(r[0], "2");
    }

    #[test]
    fn test_index_escaped_quotes() {
        let (mut idx, _p) = open_temp("id,msg\n1,\"he said \"\"hi\"\" today\"\n");
        let r = idx.row(0).unwrap();
        assert_eq!(r[1], "he said \"hi\" today");
    }

    #[test]
    fn test_index_crlf_and_bom() {
        let (mut idx, _p) = open_temp("\u{feff}a,b\r\n1,2\r\n3,4\r\n");
        assert_eq!(idx.row_count(), 2);
        assert_eq!(idx.headers()[0], "a", "BOM not stripped");
        let r = idx.row(0).unwrap();
        assert_eq!(r[1], "2", "CRLF not trimmed?");
    }

    #[test]
    fn test_delimiter_tsv() {
        let (mut idx, _p) = open_temp("a\tb\tc\n1\t2\t3\n");
        assert_eq!(idx.delimiter(), b'\t');
        let r = idx.row(0).unwrap();
        assert_eq!(r.join("|"), "1|2|3");
    }

    #[test]
    fn test_scan_order() {
        let (idx, _p) = open_temp("n\n0\n1\n2\n3\n");
        let mut seen = Vec::new();
        idx.scan(|_i, rec| {
            seen.push(rec[0].clone());
            true
        })
        .unwrap();
        assert_eq!(seen.join(","), "0,1,2,3");
    }

    // An unbalanced quote in the middle of an unquoted field (a lone " —
    // common in forensic log dumps: URLs, flag strings) is a literal
    // character, not the start of a quoted field. A naive scanner would flip
    // into quote mode on any ", swallowing the following newlines and
    // collapsing many real rows into one record.
    #[test]
    fn test_scan_bounds_stray_quote_is_literal() {
        let (mut idx, _p) = open_temp("id,msg\n1,ab\"cd\n2,ok\n3,more\n4,last\n");
        assert_eq!(idx.row_count(), 4);
        let r = idx.row(0).unwrap();
        assert_eq!(r[1], "ab\"cd");
        let r = idx.row(3).unwrap();
        assert_eq!(r[0], "4");
        assert_eq!(r[1], "last");

        // Scan must stay in lockstep with Row across the stray quote.
        let mut count = 0usize;
        idx.scan(|i, rec| {
            let want = idx.row_uncached(i).unwrap();
            assert_eq!(rec.join("|"), want.join("|"), "scan row {i}");
            count += 1;
            true
        })
        .unwrap();
        assert_eq!(count, idx.row_count());
    }

    // A clean embedded newline inside a properly quoted field must also scan
    // in lockstep with Row.
    #[test]
    fn test_scan_matches_row_on_embedded_newline() {
        let (idx, _p) = open_temp("id,msg\n1,\"line one\nline two\"\n2,ok\n3,\"a\nb\nc\"\n");
        assert_eq!(idx.row_count(), 3);
        idx.scan(|i, rec| {
            let want = idx.row_uncached(i).unwrap();
            assert_eq!(rec.join("|"), want.join("|"), "scan row {i}");
            true
        })
        .unwrap();
    }
}
