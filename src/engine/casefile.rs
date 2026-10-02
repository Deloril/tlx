//! Port of internal/casefile/casefile.go: a case is a set of related
//! timelines and their annotations stored in a single SQLite database, so an
//! investigator can work across several CSVs as one case. The source CSVs
//! stay on disk and are never imported wholesale; the database holds each
//! timeline's registration, the shared tag palette, per-row annotations, and
//! a small snapshot of every tagged row that feeds the master timeline view
//! without needing the source file open.
//!
//! Uses rusqlite with the bundled SQLite (no system libsqlite3 dependency),
//! matching the Go original's choice of a driver that needs no cgo/system
//! library.

use std::collections::HashMap;

use rusqlite::{params, Connection, OptionalExtension};

use super::index::Index;
use super::session::{SessionSnapshot, TagDef};
use super::timecol::parse_time;

const SCHEMA_VERSION: i64 = 1;

/// Errors returned by Case operations. Wraps rusqlite's error plus the
/// domain-specific validation errors the Go original returns as plain
/// fmt.Errorf (duplicate/blank IOC list name, bad note kind/text).
#[derive(Debug)]
pub enum CasefileError {
    Sqlite(rusqlite::Error),
    Other(String),
}

impl std::fmt::Display for CasefileError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            CasefileError::Sqlite(e) => write!(f, "{e}"),
            CasefileError::Other(s) => write!(f, "{s}"),
        }
    }
}

impl std::error::Error for CasefileError {}

impl From<rusqlite::Error> for CasefileError {
    fn from(e: rusqlite::Error) -> Self {
        CasefileError::Sqlite(e)
    }
}

pub type Result<T> = std::result::Result<T, CasefileError>;

fn other<S: Into<String>>(s: S) -> CasefileError {
    CasefileError::Other(s.into())
}

/// TimelineMeta is a registered timeline within a case.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TimelineMeta {
    pub id: i64,
    pub name: String,
    pub source_path: String,
    pub delimiter: String,
    pub headers: Vec<String>,
    /// display_headers are the per-column display names, one per source
    /// header. They start equal to headers and can be renamed so timelines
    /// with differently-named columns line up under shared names in the
    /// master view.
    pub display_headers: Vec<String>,
    /// time_col: index of the timestamp column, or -1 if none detected.
    pub time_col: i64,
    pub added_at: String,
}

impl TimelineMeta {
    /// display_headers_or_raw returns display_headers, padded from headers
    /// where a display name is missing, so callers always get one name per
    /// source column.
    fn display_headers_or_raw(&self) -> Vec<String> {
        self.headers
            .iter()
            .enumerate()
            .map(|(i, h)| {
                self.display_headers
                    .get(i)
                    .filter(|d| !d.is_empty())
                    .cloned()
                    .unwrap_or_else(|| h.clone())
            })
            .collect()
    }
}

/// MasterEntry is one tagged row surfaced in the master timeline. Content is
/// taken from the stored snapshot, so it is available even if the source CSV
/// is offline.
#[derive(Clone, Debug, Default)]
pub struct MasterEntry {
    pub timeline_id: i64,
    pub timeline: String,
    /// row: master row index within its own timeline.
    pub row: usize,
    pub time_raw: String,
    pub time: Option<chrono::DateTime<chrono::Utc>>,
    pub has_time: bool,
    pub tags: Vec<String>,
    pub comment: String,
    pub summary: String,
    /// cells: snapshot of the row's source cell values (edits applied), and
    /// display_headers names them, so the master view can place each value
    /// under its canonical column. Both may be empty for pre-merge
    /// snapshots.
    pub cells: Vec<String>,
    pub display_headers: Vec<String>,
}

/// IOCListMeta identifies a named IOC list within a case.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct IOCListMeta {
    pub id: i64,
    pub name: String,
}

/// Note kinds.
pub const NOTE_ARTIFACT: &str = "artifact";
pub const NOTE_TIME: &str = "time";

/// Note is one investigator note attached to a timeline. kind is
/// NOTE_ARTIFACT or NOTE_TIME.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Note {
    pub id: i64,
    pub kind: String,
    pub text: String,
    pub done: bool,
}

/// Case is an open case database.
pub struct Case {
    conn: Connection,
    path: String,
}

impl Case {
    /// create makes a new case database at path and initialises its schema
    /// and the default tag palette. It fails if the file cannot be created.
    pub fn create(path: &str) -> Result<Case> {
        Self::open(path)
    }

    /// open opens an existing case database, initialising the schema if the
    /// file is new or empty (so open doubles as create for a fresh path).
    pub fn open(path: &str) -> Result<Case> {
        let conn = open_db(path)?;
        let mut c = Case { conn, path: path.to_string() };
        c.init_schema()?;
        Ok(c)
    }

    /// path is the database file path.
    pub fn path(&self) -> &str {
        &self.path
    }

    fn init_schema(&mut self) -> Result<()> {
        const DDL: &str = "
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS timelines (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL,
    source_path TEXT NOT NULL,
    delimiter   TEXT NOT NULL DEFAULT ',',
    headers     TEXT NOT NULL DEFAULT '[]',
    time_col    INTEGER NOT NULL DEFAULT -1,
    added_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tag_defs (
    name     TEXT PRIMARY KEY,
    color    TEXT NOT NULL,
    priority INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tags (
    timeline_id INTEGER NOT NULL,
    row         INTEGER NOT NULL,
    tag         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tags_tl ON tags(timeline_id);
CREATE TABLE IF NOT EXISTS comments (
    timeline_id INTEGER NOT NULL,
    row         INTEGER NOT NULL,
    comment     TEXT NOT NULL,
    PRIMARY KEY (timeline_id, row)
);
CREATE TABLE IF NOT EXISTS edits (
    timeline_id INTEGER NOT NULL,
    row         INTEGER NOT NULL,
    col         INTEGER NOT NULL,
    value       TEXT NOT NULL,
    PRIMARY KEY (timeline_id, row, col)
);
CREATE TABLE IF NOT EXISTS tagged_snapshot (
    timeline_id INTEGER NOT NULL,
    row         INTEGER NOT NULL,
    time_unix   INTEGER,
    time_raw    TEXT NOT NULL,
    summary     TEXT NOT NULL,
    PRIMARY KEY (timeline_id, row)
);";
        self.conn.execute_batch(DDL)?;

        let have: Option<String> = self
            .conn
            .query_row("SELECT value FROM meta WHERE key='schema_version'", [], |r| r.get(0))
            .optional()?;
        if have.is_none() {
            self.conn.execute(
                "INSERT INTO meta(key,value) VALUES('schema_version',?1)",
                params![SCHEMA_VERSION.to_string()],
            )?;
            self.seed_palette()?;
        }
        self.migrate()
    }

    /// migrate applies additive schema changes on top of the base DDL so
    /// older case files keep working. Each step is guarded, so running it
    /// repeatedly is safe.
    pub fn migrate(&mut self) -> Result<()> {
        self.add_column_if_missing("timelines", "display_headers", "TEXT NOT NULL DEFAULT '[]'")?;
        self.add_column_if_missing("tagged_snapshot", "cells", "TEXT NOT NULL DEFAULT '[]'")?;
        self.add_column_if_missing("timelines", "comment", "TEXT NOT NULL DEFAULT ''")?;
        self.conn.execute_batch(
            "CREATE TABLE IF NOT EXISTS ioc_lists (
                id   INTEGER PRIMARY KEY AUTOINCREMENT,
                name TEXT NOT NULL UNIQUE,
                body TEXT NOT NULL DEFAULT ''
            );",
        )?;
        self.seed_default_ioc_list()?;
        self.conn.execute_batch(
            "CREATE TABLE IF NOT EXISTS notes (
                id          INTEGER PRIMARY KEY AUTOINCREMENT,
                timeline_id INTEGER NOT NULL,
                kind        TEXT NOT NULL,
                text        TEXT NOT NULL,
                done        INTEGER NOT NULL DEFAULT 0
            );
            CREATE INDEX IF NOT EXISTS idx_notes_tl ON notes(timeline_id);",
        )?;
        self.add_column_if_missing("notes", "position", "INTEGER NOT NULL DEFAULT 0")?;
        Ok(())
    }

    /// seed_default_ioc_list gives every case a default IOC list named after
    /// the case file, carrying forward any body from the old single
    /// meta['ioc_list'] value. It runs exactly once, guarded by the
    /// meta['ioc_lists_seeded'] flag, so a list the user later deletes is
    /// never resurrected on reopen.
    fn seed_default_ioc_list(&mut self) -> Result<()> {
        let seeded: Option<String> = self
            .conn
            .query_row("SELECT value FROM meta WHERE key='ioc_lists_seeded'", [], |r| r.get(0))
            .optional()?;
        if seeded.is_some() {
            return Ok(());
        }

        let body: String = self
            .conn
            .query_row("SELECT value FROM meta WHERE key='ioc_list'", [], |r| r.get(0))
            .optional()?
            .unwrap_or_default();

        let name = self.case_name();
        let tx = self.conn.transaction()?;
        tx.execute("INSERT OR IGNORE INTO ioc_lists(name,body) VALUES(?1,?2)", params![name, body])?;
        tx.execute("DELETE FROM meta WHERE key='ioc_list'", [])?;
        tx.execute(
            "INSERT INTO meta(key,value) VALUES('ioc_lists_seeded','1')
             ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            [],
        )?;
        tx.commit()?;
        Ok(())
    }

    /// case_name derives a default list/case display name from the database
    /// file path: its base name without extension, or "case" if that is
    /// empty.
    fn case_name(&self) -> String {
        let p = std::path::Path::new(&self.path);
        let stem = p.file_stem().and_then(|s| s.to_str()).unwrap_or("");
        if stem.is_empty() {
            "case".to_string()
        } else {
            stem.to_string()
        }
    }

    /// add_column_if_missing adds a column to a table unless it is already
    /// present.
    fn add_column_if_missing(&mut self, table: &str, column: &str, decl: &str) -> Result<()> {
        let mut stmt = self.conn.prepare(&format!("SELECT name FROM pragma_table_info('{table}')"))?;
        let mut rows = stmt.query([])?;
        let mut found = false;
        while let Some(row) = rows.next()? {
            let name: String = row.get(0)?;
            if name == column {
                found = true;
                break;
            }
        }
        drop(rows);
        drop(stmt);
        if found {
            return Ok(());
        }
        self.conn.execute(&format!("ALTER TABLE {table} ADD COLUMN {column} {decl}"), [])?;
        Ok(())
    }

    /// seed_palette writes the built-in tag defaults if the palette is
    /// empty.
    fn seed_palette(&mut self) -> Result<()> {
        let n: i64 = self.conn.query_row("SELECT COUNT(*) FROM tag_defs", [], |r| r.get(0))?;
        if n > 0 {
            return Ok(());
        }
        // Seeded defaults, in priority order; mirrors model.NewSession("").TagDefs().
        let defs = super::session::Session::new("").tag_defs();
        self.save_tag_defs(&defs)
    }

    /// timelines returns every registered timeline, oldest first.
    pub fn timelines(&self) -> Result<Vec<TimelineMeta>> {
        let mut stmt = self.conn.prepare(
            "SELECT id,name,source_path,delimiter,headers,display_headers,time_col,added_at
             FROM timelines ORDER BY id",
        )?;
        let mut rows = stmt.query([])?;
        let mut out = Vec::new();
        while let Some(row) = rows.next()? {
            out.push(timeline_from_row(row)?);
        }
        Ok(out)
    }

    /// timeline returns a single timeline by id.
    pub fn timeline(&self, id: i64) -> Result<TimelineMeta> {
        let mut stmt = self.conn.prepare(
            "SELECT id,name,source_path,delimiter,headers,display_headers,time_col,added_at
             FROM timelines WHERE id=?1",
        )?;
        let mut rows = stmt.query(params![id])?;
        match rows.next()? {
            Some(row) => timeline_from_row(row),
            None => Err(CasefileError::Sqlite(rusqlite::Error::QueryReturnedNoRows)),
        }
    }

    /// add_timeline registers an already-opened index as a timeline in the
    /// case. The timestamp column is auto-detected. name defaults to the
    /// file's base name when empty (left to the caller, as in the Go
    /// original). added_at is supplied by the caller.
    pub fn add_timeline(&mut self, name: &str, idx: &Index, added_at: &str) -> Result<TimelineMeta> {
        let headers = idx.headers().to_vec();
        let headers_json = serde_json::to_string(&headers).map_err(|e| other(e.to_string()))?;
        let time_col = super::timecol::detect_time_column(idx).map(|c| c as i64).unwrap_or(-1);
        self.conn.execute(
            "INSERT INTO timelines(name,source_path,delimiter,headers,display_headers,time_col,added_at)
             VALUES(?1,?2,?3,?4,?5,?6,?7)",
            params![
                name,
                idx.path(),
                (idx.delimiter() as char).to_string(),
                headers_json,
                headers_json,
                time_col,
                added_at
            ],
        )?;
        let id = self.conn.last_insert_rowid();
        Ok(TimelineMeta {
            id,
            name: name.to_string(),
            source_path: idx.path().to_string(),
            delimiter: (idx.delimiter() as char).to_string(),
            headers: headers.clone(),
            display_headers: headers,
            time_col,
            added_at: added_at.to_string(),
        })
    }

    /// set_column_names stores per-column display names for a timeline.
    /// names is one entry per source column; a blank entry falls back to
    /// the source header.
    pub fn set_column_names(&mut self, id: i64, names: &[String]) -> Result<()> {
        let data = serde_json::to_string(names).map_err(|e| other(e.to_string()))?;
        self.conn.execute("UPDATE timelines SET display_headers=?1 WHERE id=?2", params![data, id])?;
        Ok(())
    }

    /// timeline_comment returns the free-text comment stored for a
    /// timeline, or "".
    pub fn timeline_comment(&self, id: i64) -> Result<String> {
        let text: String = self.conn.query_row("SELECT comment FROM timelines WHERE id=?1", params![id], |r| r.get(0))?;
        Ok(text)
    }

    /// set_timeline_comment stores a timeline's free-text comment.
    pub fn set_timeline_comment(&mut self, id: i64, text: &str) -> Result<()> {
        self.conn.execute("UPDATE timelines SET comment=?1 WHERE id=?2", params![text, id])?;
        Ok(())
    }

    /// remove_timeline deletes a timeline and all of its annotations.
    pub fn remove_timeline(&mut self, id: i64) -> Result<()> {
        let tx = self.conn.transaction()?;
        for stmt in [
            "DELETE FROM tags WHERE timeline_id=?1",
            "DELETE FROM comments WHERE timeline_id=?1",
            "DELETE FROM edits WHERE timeline_id=?1",
            "DELETE FROM tagged_snapshot WHERE timeline_id=?1",
            "DELETE FROM notes WHERE timeline_id=?1",
            "DELETE FROM timelines WHERE id=?1",
        ] {
            tx.execute(stmt, params![id])?;
        }
        tx.commit()?;
        Ok(())
    }

    /// set_time_column overrides the detected timestamp column for a
    /// timeline.
    pub fn set_time_column(&mut self, id: i64, col: i64) -> Result<()> {
        self.conn.execute("UPDATE timelines SET time_col=?1 WHERE id=?2", params![col, id])?;
        Ok(())
    }

    /// tag_defs returns the case's shared tag palette in priority order.
    pub fn tag_defs(&self) -> Result<Vec<TagDef>> {
        let mut stmt = self.conn.prepare("SELECT name,color FROM tag_defs ORDER BY priority")?;
        let mut rows = stmt.query([])?;
        let mut out = Vec::new();
        while let Some(row) = rows.next()? {
            out.push(TagDef { name: row.get(0)?, color: row.get(1)? });
        }
        Ok(out)
    }

    /// save_tag_defs replaces the palette with defs, preserving their order
    /// as priority.
    pub fn save_tag_defs(&mut self, defs: &[TagDef]) -> Result<()> {
        let tx = self.conn.transaction()?;
        tx.execute("DELETE FROM tag_defs", [])?;
        for (i, d) in defs.iter().enumerate() {
            tx.execute(
                "INSERT INTO tag_defs(name,color,priority) VALUES(?1,?2,?3)",
                params![d.name, d.color, i as i64],
            )?;
        }
        tx.commit()?;
        Ok(())
    }

    /// delete_tag removes a tag from the case palette and strips it from
    /// every row of every timeline. Snapshot rows left with no tags drop
    /// out, so they no longer appear in the master view.
    pub fn delete_tag(&mut self, name: &str) -> Result<()> {
        let tx = self.conn.transaction()?;
        tx.execute("DELETE FROM tag_defs WHERE name=?1", params![name])?;
        tx.execute("DELETE FROM tags WHERE tag=?1", params![name])?;
        tx.execute(
            "DELETE FROM tagged_snapshot
             WHERE NOT EXISTS (SELECT 1 FROM tags
                 WHERE tags.timeline_id = tagged_snapshot.timeline_id
                   AND tags.row = tagged_snapshot.row)",
            [],
        )?;
        tx.commit()?;
        Ok(())
    }

    /// load_annotations reads a timeline's annotations into a snapshot. The
    /// case palette is included so a session opened from it paints rows
    /// consistently.
    pub fn load_annotations(&self, timeline_id: i64) -> Result<SessionSnapshot> {
        let mut snap = SessionSnapshot::default();
        snap.tag_defs = self.tag_defs()?;

        {
            let mut stmt = self.conn.prepare("SELECT row,tag FROM tags WHERE timeline_id=?1")?;
            let mut rows = stmt.query(params![timeline_id])?;
            while let Some(row) = rows.next()? {
                let r: i64 = row.get(0)?;
                let tag: String = row.get(1)?;
                snap.tags.entry(r as usize).or_default().push(tag);
            }
        }
        for v in snap.tags.values_mut() {
            v.sort();
        }

        {
            let mut stmt = self.conn.prepare("SELECT row,comment FROM comments WHERE timeline_id=?1")?;
            let mut rows = stmt.query(params![timeline_id])?;
            while let Some(row) = rows.next()? {
                let r: i64 = row.get(0)?;
                let c: String = row.get(1)?;
                snap.comments.insert(r as usize, c);
            }
        }

        {
            let mut stmt = self.conn.prepare("SELECT row,col,value FROM edits WHERE timeline_id=?1")?;
            let mut rows = stmt.query(params![timeline_id])?;
            while let Some(row) = rows.next()? {
                let r: i64 = row.get(0)?;
                let c: i64 = row.get(1)?;
                let v: String = row.get(2)?;
                snap.edits.entry(r as usize).or_default().insert(c as usize, v);
            }
        }

        Ok(snap)
    }

    /// save_annotations writes a timeline's annotations back to the
    /// database, replacing any prior state for that timeline, and rebuilds
    /// its tagged-row snapshot from `rows` (the open index for the
    /// timeline). The case palette is updated from the snapshot so newly
    /// defined tags and recolourings persist.
    pub fn save_annotations(&mut self, tl: &TimelineMeta, snap: &SessionSnapshot, rows: Option<&Index>) -> Result<()> {
        let tx = self.conn.transaction()?;
        for stmt in [
            "DELETE FROM tags WHERE timeline_id=?1",
            "DELETE FROM comments WHERE timeline_id=?1",
            "DELETE FROM edits WHERE timeline_id=?1",
            "DELETE FROM tagged_snapshot WHERE timeline_id=?1",
        ] {
            tx.execute(stmt, params![tl.id])?;
        }

        for (row, tags) in &snap.tags {
            for tag in tags {
                tx.execute(
                    "INSERT INTO tags(timeline_id,row,tag) VALUES(?1,?2,?3)",
                    params![tl.id, *row as i64, tag],
                )?;
            }
        }
        for (row, c) in &snap.comments {
            if c.is_empty() {
                continue;
            }
            tx.execute(
                "INSERT INTO comments(timeline_id,row,comment) VALUES(?1,?2,?3)",
                params![tl.id, *row as i64, c],
            )?;
        }
        for (row, cols) in &snap.edits {
            for (col, v) in cols {
                tx.execute(
                    "INSERT INTO edits(timeline_id,row,col,value) VALUES(?1,?2,?3,?4)",
                    params![tl.id, *row as i64, *col as i64, v],
                )?;
            }
        }

        // Snapshot every tagged row so the master view needs no source file.
        for (row, tags) in &snap.tags {
            if tags.is_empty() {
                continue;
            }
            let mut rec: Vec<String> = Vec::new();
            if let Some(idx) = rows {
                if let Ok(r) = idx.row_uncached(*row) {
                    rec = r;
                }
            }
            if let Some(cols) = snap.edits.get(row) {
                apply_edits(&mut rec, cols);
            }
            let (time_raw, time_unix, has_time) = extract_time(&rec, tl.time_col);
            let summary = summarize(&rec);
            let cells_json = serde_json::to_string(&rec).map_err(|e| other(e.to_string()))?;
            let time_unix_param: Option<i64> = if has_time { Some(time_unix) } else { None };
            tx.execute(
                "INSERT INTO tagged_snapshot(timeline_id,row,time_unix,time_raw,summary,cells)
                 VALUES(?1,?2,?3,?4,?5,?6)",
                params![tl.id, *row as i64, time_unix_param, time_raw, summary, cells_json],
            )?;
        }

        if !snap.tag_defs.is_empty() {
            tx.execute("DELETE FROM tag_defs", [])?;
            for (i, d) in snap.tag_defs.iter().enumerate() {
                tx.execute(
                    "INSERT INTO tag_defs(name,color,priority) VALUES(?1,?2,?3)",
                    params![d.name, d.color, i as i64],
                )?;
            }
        }

        tx.commit()?;
        Ok(())
    }

    /// ioc_lists returns every named IOC list in the case, ordered by name.
    pub fn ioc_lists(&self) -> Result<Vec<IOCListMeta>> {
        let mut stmt = self.conn.prepare("SELECT id,name FROM ioc_lists ORDER BY name")?;
        let mut rows = stmt.query([])?;
        let mut out = Vec::new();
        while let Some(row) = rows.next()? {
            out.push(IOCListMeta { id: row.get(0)?, name: row.get(1)? });
        }
        Ok(out)
    }

    /// ioc_list_body returns the body text (one indicator per line) of the
    /// list with the given id.
    pub fn ioc_list_body(&self, id: i64) -> Result<String> {
        let body: String = self.conn.query_row("SELECT body FROM ioc_lists WHERE id=?1", params![id], |r| r.get(0))?;
        Ok(body)
    }

    /// create_ioc_list creates a new empty named list and returns it. The
    /// name must be non-empty and unique (case-insensitive); a duplicate or
    /// blank name is an error.
    pub fn create_ioc_list(&mut self, name: &str) -> Result<IOCListMeta> {
        let name = self.check_ioc_list_name(name, 0)?;
        self.conn.execute("INSERT INTO ioc_lists(name,body) VALUES(?1,'')", params![name])?;
        let id = self.conn.last_insert_rowid();
        Ok(IOCListMeta { id, name })
    }

    /// set_ioc_list_body replaces the body text of a list.
    pub fn set_ioc_list_body(&mut self, id: i64, body: &str) -> Result<()> {
        self.conn.execute("UPDATE ioc_lists SET body=?1 WHERE id=?2", params![body, id])?;
        Ok(())
    }

    /// rename_ioc_list changes a list's name (same non-empty/unique rule as
    /// create_ioc_list).
    pub fn rename_ioc_list(&mut self, id: i64, name: &str) -> Result<()> {
        let name = self.check_ioc_list_name(name, id)?;
        self.conn.execute("UPDATE ioc_lists SET name=?1 WHERE id=?2", params![name, id])?;
        Ok(())
    }

    /// delete_ioc_list removes a list.
    pub fn delete_ioc_list(&mut self, id: i64) -> Result<()> {
        self.conn.execute("DELETE FROM ioc_lists WHERE id=?1", params![id])?;
        Ok(())
    }

    /// check_ioc_list_name trims name, rejects it if blank, and rejects it
    /// if it collides case-insensitively with another list's name
    /// (exclude_id excludes the row being renamed; pass 0 when creating).
    /// It returns the trimmed name ready to store.
    fn check_ioc_list_name(&self, name: &str, exclude_id: i64) -> Result<String> {
        let name = name.trim();
        if name.is_empty() {
            return Err(other("ioc list name is required"));
        }
        let exists: Option<i64> = self
            .conn
            .query_row(
                "SELECT 1 FROM ioc_lists WHERE name=?1 COLLATE NOCASE AND id<>?2",
                params![name, exclude_id],
                |r| r.get(0),
            )
            .optional()?;
        if exists.is_some() {
            return Err(other(format!("an IOC list named {name:?} already exists")));
        }
        Ok(name.to_string())
    }

    /// notes returns a timeline's notes of the given kind in display order:
    /// by the manual position first, then id (insertion order) as a
    /// tie-break for notes that have never been reordered.
    pub fn notes(&self, timeline_id: i64, kind: &str) -> Result<Vec<Note>> {
        let mut stmt = self.conn.prepare(
            "SELECT id,kind,text,done FROM notes
             WHERE timeline_id=?1 AND kind=?2 ORDER BY position ASC, id ASC",
        )?;
        let mut rows = stmt.query(params![timeline_id, kind])?;
        let mut out = Vec::new();
        while let Some(row) = rows.next()? {
            let done: i64 = row.get(3)?;
            out.push(Note { id: row.get(0)?, kind: row.get(1)?, text: row.get(2)?, done: done != 0 });
        }
        Ok(out)
    }

    /// add_note appends a note to a timeline and returns it (with its new
    /// id). text is trimmed; empty-after-trim is an error. kind must be
    /// NOTE_ARTIFACT or NOTE_TIME, else error.
    pub fn add_note(&mut self, timeline_id: i64, kind: &str, text: &str) -> Result<Note> {
        if kind != NOTE_ARTIFACT && kind != NOTE_TIME {
            return Err(other(format!("invalid note kind {kind:?}")));
        }
        let text = text.trim();
        if text.is_empty() {
            return Err(other("note text is required"));
        }
        // Append at the end of this kind's list: one past the current max position.
        let pos: i64 = self.conn.query_row(
            "SELECT COALESCE(MAX(position),-1)+1 FROM notes WHERE timeline_id=?1 AND kind=?2",
            params![timeline_id, kind],
            |r| r.get(0),
        )?;
        self.conn.execute(
            "INSERT INTO notes(timeline_id,kind,text,done,position) VALUES(?1,?2,?3,0,?4)",
            params![timeline_id, kind, text, pos],
        )?;
        let id = self.conn.last_insert_rowid();
        Ok(Note { id, kind: kind.to_string(), text: text.to_string(), done: false })
    }

    /// set_note_done sets a note's done flag.
    pub fn set_note_done(&mut self, id: i64, done: bool) -> Result<()> {
        self.conn.execute("UPDATE notes SET done=?1 WHERE id=?2", params![done as i64, id])?;
        Ok(())
    }

    /// delete_note removes a note by id.
    pub fn delete_note(&mut self, id: i64) -> Result<()> {
        self.conn.execute("DELETE FROM notes WHERE id=?1", params![id])?;
        Ok(())
    }

    /// reorder_notes writes a new display order for the given note ids,
    /// assigning each its position by index. Ids not in the slice are left
    /// untouched; callers pass the full ordered id list for one
    /// timeline+kind.
    pub fn reorder_notes(&mut self, ids: &[i64]) -> Result<()> {
        let tx = self.conn.transaction()?;
        {
            let mut stmt = tx.prepare("UPDATE notes SET position=?1 WHERE id=?2")?;
            for (i, id) in ids.iter().enumerate() {
                stmt.execute(params![i as i64, id])?;
            }
        }
        tx.commit()?;
        Ok(())
    }

    /// master returns every tagged row across all timelines, sorted
    /// chronologically (rows with a parseable timestamp first, ascending;
    /// rows without follow, grouped by timeline then row).
    pub fn master(&self) -> Result<Vec<MasterEntry>> {
        let tls = self.timelines()?;
        let mut names: HashMap<i64, String> = HashMap::new();
        let mut display: HashMap<i64, Vec<String>> = HashMap::new();
        for t in &tls {
            names.insert(t.id, t.name.clone());
            display.insert(t.id, t.display_headers_or_raw());
        }

        // Bulk-load tags and comments into maps first. With a single DB
        // connection we must never run a query while another cursor is
        // still open, so each of these fully drains and closes before the
        // next.
        let all_tags = self.all_tags()?;
        let all_comments = self.all_comments()?;

        let mut out = Vec::new();
        {
            let mut stmt = self
                .conn
                .prepare("SELECT timeline_id,row,time_unix,time_raw,summary,cells FROM tagged_snapshot")?;
            let mut rows = stmt.query([])?;
            while let Some(row) = rows.next()? {
                let timeline_id: i64 = row.get(0)?;
                let r: i64 = row.get(1)?;
                let time_unix: Option<i64> = row.get(2)?;
                let time_raw: String = row.get(3)?;
                let summary: String = row.get(4)?;
                let cells_json: String = row.get(5)?;
                let cells: Vec<String> = serde_json::from_str(&cells_json).unwrap_or_default();

                let mut e = MasterEntry {
                    timeline_id,
                    timeline: names.get(&timeline_id).cloned().unwrap_or_default(),
                    row: r as usize,
                    time_raw,
                    summary,
                    cells,
                    display_headers: display.get(&timeline_id).cloned().unwrap_or_default(),
                    ..Default::default()
                };
                if let Some(tu) = time_unix {
                    e.time = Some(chrono::DateTime::from_timestamp_nanos(tu));
                    e.has_time = true;
                }
                let key = (timeline_id, r as usize);
                e.tags = all_tags.get(&key).cloned().unwrap_or_default();
                e.tags.sort();
                e.comment = all_comments.get(&key).cloned().unwrap_or_default();
                out.push(e);
            }
        }

        out.sort_by(|a, b| {
            if a.has_time != b.has_time {
                // timed rows first
                return if a.has_time { std::cmp::Ordering::Less } else { std::cmp::Ordering::Greater };
            }
            if a.has_time {
                return a.time.cmp(&b.time);
            }
            if a.timeline != b.timeline {
                return a.timeline.cmp(&b.timeline);
            }
            a.row.cmp(&b.row)
        });
        Ok(out)
    }

    /// all_tags loads every (timeline,row)->tags mapping in one pass.
    fn all_tags(&self) -> Result<HashMap<(i64, usize), Vec<String>>> {
        let mut stmt = self.conn.prepare("SELECT timeline_id,row,tag FROM tags")?;
        let mut rows = stmt.query([])?;
        let mut out: HashMap<(i64, usize), Vec<String>> = HashMap::new();
        while let Some(row) = rows.next()? {
            let tl: i64 = row.get(0)?;
            let r: i64 = row.get(1)?;
            let tag: String = row.get(2)?;
            out.entry((tl, r as usize)).or_default().push(tag);
        }
        Ok(out)
    }

    /// all_comments loads every (timeline,row)->comment mapping in one
    /// pass.
    fn all_comments(&self) -> Result<HashMap<(i64, usize), String>> {
        let mut stmt = self.conn.prepare("SELECT timeline_id,row,comment FROM comments")?;
        let mut rows = stmt.query([])?;
        let mut out: HashMap<(i64, usize), String> = HashMap::new();
        while let Some(row) = rows.next()? {
            let tl: i64 = row.get(0)?;
            let r: i64 = row.get(1)?;
            let c: String = row.get(2)?;
            out.insert((tl, r as usize), c);
        }
        Ok(out)
    }
}

fn timeline_from_row(row: &rusqlite::Row) -> Result<TimelineMeta> {
    let headers_json: String = row.get(4)?;
    let display_json: String = row.get(5)?;
    Ok(TimelineMeta {
        id: row.get(0)?,
        name: row.get(1)?,
        source_path: row.get(2)?,
        delimiter: row.get(3)?,
        headers: serde_json::from_str(&headers_json).unwrap_or_default(),
        display_headers: serde_json::from_str(&display_json).unwrap_or_default(),
        time_col: row.get(6)?,
        added_at: row.get(7)?,
    })
}

fn open_db(path: &str) -> Result<Connection> {
    let conn = Connection::open(path)?;
    // SQLite tolerates one writer; a single connection avoids "database is
    // locked" under our simple, low-concurrency access. rusqlite's
    // Connection is already exclusive to this handle, matching Go's
    // db.SetMaxOpenConns(1).
    conn.execute_batch("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;")?;
    Ok(conn)
}

fn apply_edits(rec: &mut Vec<String>, edits: &HashMap<usize, String>) {
    if edits.is_empty() {
        return;
    }
    for (&col, v) in edits {
        while col >= rec.len() {
            rec.push(String::new());
        }
        rec[col] = v.clone();
    }
}

fn extract_time(rec: &[String], time_col: i64) -> (String, i64, bool) {
    if time_col < 0 || time_col as usize >= rec.len() {
        return (String::new(), 0, false);
    }
    let raw = rec[time_col as usize].clone();
    if let Some(t) = parse_time(&raw) {
        return (raw, t.timestamp_nanos_opt().unwrap_or(0), true);
    }
    (raw, 0, false)
}

/// summarize joins non-empty cells into a compact one-line summary, capped
/// so a wide row does not bloat the database or the master grid.
fn summarize(rec: &[String]) -> String {
    const MAX: usize = 400;
    let parts: Vec<&str> = rec.iter().map(|c| c.trim()).filter(|c| !c.is_empty()).collect();
    let s = parts.join(" | ");
    if s.chars().count() > MAX {
        let truncated: String = s.chars().take(MAX).collect();
        format!("{truncated}…")
    } else {
        s
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::engine::session::SessionSnapshot;

    fn temp_db_path(tag: &str) -> std::path::PathBuf {
        use std::sync::atomic::{AtomicU64, Ordering};
        static COUNTER: AtomicU64 = AtomicU64::new(0);
        let n = COUNTER.fetch_add(1, Ordering::Relaxed);
        std::env::temp_dir().join(format!(
            "tlx-rust-test-casefile-{tag}-{}-{}-{}.tlxdb",
            std::process::id(),
            std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos(),
            n
        ))
    }

    fn new_case(tag: &str) -> (Case, std::path::PathBuf) {
        let path = temp_db_path(tag);
        let c = Case::create(path.to_str().unwrap()).expect("create case");
        (c, path)
    }

    fn color_of(defs: &[TagDef], name: &str) -> Option<String> {
        defs.iter().find(|d| d.name == name).map(|d| d.color.clone())
    }

    #[test]
    fn test_default_palette_seeded() {
        let (c, path) = new_case("palette");
        let defs = c.tag_defs().unwrap();
        assert_eq!(defs.len(), 3);
        assert_eq!(defs[0].name, "Bad");
        assert_eq!(defs[2].name, "Good");
        assert_eq!(defs[0].color, "#E53935");
        std::fs::remove_file(&path).ok();
    }

    #[test]
    fn test_add_timeline_detects_time_column() {
        let (mut c, path) = new_case("addtl");
        let idx = Index::new_memory(
            vec!["Timestamp".into(), "Host".into(), "Message".into()],
            vec![
                vec!["2026-09-01T08:12:03Z".into(), "ws1".into(), "logon".into()],
                vec!["2026-09-01T08:14:55Z".into(), "fw".into(), "outbound".into()],
            ],
        );
        let tl = c.add_timeline("workstation", &idx, "2026-09-11T00:00:00Z").unwrap();
        assert_eq!(tl.time_col, 0);
        let tls = c.timelines().unwrap();
        assert_eq!(tls.len(), 1);
        assert_eq!(tls[0].name, "workstation");
        assert_eq!(tls[0].headers.len(), 3);
        std::fs::remove_file(&path).ok();
    }

    #[test]
    fn test_annotation_round_trip() {
        let (mut c, path) = new_case("roundtrip");
        let idx = Index::new_memory(
            vec!["Timestamp".into(), "Message".into()],
            vec![
                vec!["2026-09-01T08:12:03Z".into(), "logon".into()],
                vec!["2026-09-01T09:00:00Z".into(), "beacon".into()],
            ],
        );
        let tl = c.add_timeline("t1", &idx, "2026-09-11T00:00:00Z").unwrap();

        let mut snap = SessionSnapshot::default();
        snap.tags.insert(0, vec!["Bad".to_string()]);
        snap.tags.insert(1, vec!["Suspicious".to_string()]);
        snap.comments.insert(0, "initial access".to_string());
        let mut e1 = HashMap::new();
        e1.insert(1usize, "beacon (edited)".to_string());
        snap.edits.insert(1, e1);
        snap.tag_defs = vec![
            TagDef { name: "Bad".to_string(), color: "#E53935".to_string() },
            TagDef { name: "beacon".to_string(), color: "#1E88E5".to_string() },
        ];

        c.save_annotations(&tl, &snap, Some(&idx)).unwrap();

        let got = c.load_annotations(tl.id).unwrap();
        assert_eq!(got.tags.get(&0).unwrap(), &vec!["Bad".to_string()]);
        assert_eq!(got.comments.get(&0).unwrap(), "initial access");
        assert_eq!(got.edits.get(&1).unwrap().get(&1).unwrap(), "beacon (edited)");
        assert!(color_of(&got.tag_defs, "beacon").is_some());

        std::fs::remove_file(&path).ok();
    }

    #[test]
    fn test_delete_tag_case_wide() {
        let (mut c, path) = new_case("deltag");
        let idx_a = Index::new_memory(
            vec!["Timestamp".into(), "Msg".into()],
            vec![
                vec!["2026-09-01T08:12:03Z".into(), "logon".into()],
                vec!["2026-09-01T09:00:00Z".into(), "second".into()],
            ],
        );
        let idx_b = Index::new_memory(
            vec!["When".into(), "Msg".into()],
            vec![vec!["2026-09-01T10:00:00Z".into(), "outbound".into()]],
        );
        let tl_a = c.add_timeline("A", &idx_a, "2026-09-11T00:00:00Z").unwrap();
        let tl_b = c.add_timeline("B", &idx_b, "2026-09-11T00:00:00Z").unwrap();

        let mut snap_a = SessionSnapshot::default();
        snap_a.tags.insert(0, vec!["beacon".to_string()]);
        snap_a.tags.insert(1, vec!["Bad".to_string(), "beacon".to_string()]);
        snap_a.tag_defs = vec![
            TagDef { name: "beacon".to_string(), color: "#1E88E5".to_string() },
            TagDef { name: "Bad".to_string(), color: "#E53935".to_string() },
        ];
        c.save_annotations(&tl_a, &snap_a, Some(&idx_a)).unwrap();

        let mut snap_b = SessionSnapshot::default();
        snap_b.tags.insert(0, vec!["beacon".to_string()]);
        c.save_annotations(&tl_b, &snap_b, Some(&idx_b)).unwrap();

        c.delete_tag("beacon").unwrap();

        let got_a = c.load_annotations(tl_a.id).unwrap();
        assert!(got_a.tags.get(&0).map(|v| v.is_empty()).unwrap_or(true));
        assert_eq!(got_a.tags.get(&1).unwrap(), &vec!["Bad".to_string()]);

        let got_b = c.load_annotations(tl_b.id).unwrap();
        assert!(got_b.tags.get(&0).map(|v| v.is_empty()).unwrap_or(true));

        let defs = c.tag_defs().unwrap();
        assert!(color_of(&defs, "beacon").is_none());

        let master = c.master().unwrap();
        assert_eq!(master.len(), 1);
        assert_eq!(master[0].timeline_id, tl_a.id);
        assert_eq!(master[0].row, 1);

        std::fs::remove_file(&path).ok();
    }

    #[test]
    fn test_master_ordering() {
        let (mut c, path) = new_case("masterorder");
        let idx_a = Index::new_memory(
            vec!["Timestamp".into(), "Msg".into()],
            vec![
                vec!["2026-09-01T09:02:10Z".into(), "ticket forged".into()],
                vec!["2026-09-01T08:12:03Z".into(), "initial access".into()],
            ],
        );
        let idx_b = Index::new_memory(
            vec!["When".into(), "Msg".into()],
            vec![
                vec!["2026-09-01T08:14:55Z".into(), "outbound c2".into()],
                vec!["not-a-time".into(), "no timestamp here".into()],
            ],
        );
        let tl_a = c.add_timeline("dc", &idx_a, "2026-09-11T00:00:00Z").unwrap();
        let tl_b = c.add_timeline("fw", &idx_b, "2026-09-11T00:00:00Z").unwrap();

        let mut snap_a = SessionSnapshot::default();
        snap_a.tags.insert(0, vec!["Bad".to_string()]);
        snap_a.tags.insert(1, vec!["Bad".to_string()]);
        c.save_annotations(&tl_a, &snap_a, Some(&idx_a)).unwrap();

        let mut snap_b = SessionSnapshot::default();
        snap_b.tags.insert(0, vec!["Suspicious".to_string()]);
        snap_b.tags.insert(1, vec!["Good".to_string()]);
        c.save_annotations(&tl_b, &snap_b, Some(&idx_b)).unwrap();

        let m = c.master().unwrap();
        assert_eq!(m.len(), 4);
        let want_summary = ["initial access", "outbound c2", "ticket forged"];
        for (i, w) in want_summary.iter().enumerate() {
            assert!(m[i].has_time, "entry {i} should have time");
            assert!(m[i].summary.ends_with(w), "entry {i} summary = {:?}, want ...{w}", m[i].summary);
        }
        assert!(!m[3].has_time, "last entry should be untimed");
        assert_eq!(m[3].tags, vec!["Good".to_string()]);

        std::fs::remove_file(&path).ok();
    }

    #[test]
    fn test_column_rename_merges_master() {
        let (mut c, path) = new_case("colrename");
        let idx_a = Index::new_memory(
            vec!["Timestamp".into(), "Computer".into(), "Message".into()],
            vec![vec!["2026-09-01T08:12:03Z".into(), "ws1".into(), "logon".into()]],
        );
        let idx_b = Index::new_memory(
            vec!["When".into(), "Host".into(), "Event".into()],
            vec![vec!["2026-09-01T09:00:00Z".into(), "dc1".into(), "kerberoast".into()]],
        );
        let tl_a = c.add_timeline("wks", &idx_a, "2026-09-11T00:00:00Z").unwrap();
        let tl_b = c.add_timeline("dc", &idx_b, "2026-09-11T00:00:00Z").unwrap();

        c.set_column_names(
            tl_b.id,
            &["Timestamp".to_string(), "Computer".to_string(), "Message".to_string()],
        )
        .unwrap();

        let mut snap_a = SessionSnapshot::default();
        snap_a.tags.insert(0, vec!["Bad".to_string()]);
        c.save_annotations(&tl_a, &snap_a, Some(&idx_a)).unwrap();
        let mut snap_b = SessionSnapshot::default();
        snap_b.tags.insert(0, vec!["Bad".to_string()]);
        c.save_annotations(&tl_b, &snap_b, Some(&idx_b)).unwrap();

        let m = c.master().unwrap();
        assert_eq!(m.len(), 2);
        let by_name = |e: &MasterEntry, name: &str| -> String {
            for (i, h) in e.display_headers.iter().enumerate() {
                if h == name && i < e.cells.len() {
                    return e.cells[i].clone();
                }
            }
            String::new()
        };
        assert_eq!(by_name(&m[0], "Computer"), "ws1");
        assert_eq!(by_name(&m[1], "Computer"), "dc1");
        assert_eq!(by_name(&m[1], "Message"), "kerberoast");

        std::fs::remove_file(&path).ok();
    }

    #[test]
    fn test_remove_timeline() {
        let (mut c, path) = new_case("removetl");
        let idx = Index::new_memory(
            vec!["Timestamp".into(), "Msg".into()],
            vec![vec!["2026-09-01T08:12:03Z".into(), "x".into()]],
        );
        let tl = c.add_timeline("t", &idx, "2026-09-11T00:00:00Z").unwrap();
        let mut snap = SessionSnapshot::default();
        snap.tags.insert(0, vec!["Bad".to_string()]);
        c.save_annotations(&tl, &snap, Some(&idx)).unwrap();

        c.remove_timeline(tl.id).unwrap();
        assert_eq!(c.timelines().unwrap().len(), 0);
        assert_eq!(c.master().unwrap().len(), 0);

        std::fs::remove_file(&path).ok();
    }

    #[test]
    fn test_file_backed_timeline_end_to_end() {
        let dir = std::env::temp_dir().join(format!(
            "tlx-rust-test-casefile-e2e-{}-{}",
            std::process::id(),
            std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        let csv_path = dir.join("auth.csv");
        let csv = "Timestamp,Host,Message\n\
                   2026-09-01 08:12:03,ws1,successful logon\n\
                   2026-09-01 09:00:00,ws1,service installed\n";
        std::fs::write(&csv_path, csv).unwrap();

        let idx = Index::open(csv_path.to_str().unwrap(), None).unwrap();

        let db_path = dir.join("case.tlxdb");
        let mut c = Case::create(db_path.to_str().unwrap()).unwrap();

        let tl = c.add_timeline("auth", &idx, "2026-09-11T00:00:00Z").unwrap();
        assert_eq!(tl.time_col, 0, "TimeCol should be 0 (Timestamp)");

        let mut snap = SessionSnapshot::default();
        snap.tags.insert(1, vec!["Suspicious".to_string()]);
        c.save_annotations(&tl, &snap, Some(&idx)).unwrap();

        let m = c.master().unwrap();
        assert_eq!(m.len(), 1);
        let e = &m[0];
        assert!(e.has_time, "entry should have a parsed time");
        assert_eq!(e.row, 1);
        assert!(e.summary.ends_with("service installed"), "summary = {:?}", e.summary);

        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_ioc_lists() {
        let (mut c, path) = new_case("ioclists");

        let default_lists = c.ioc_lists().unwrap();
        assert_eq!(default_lists.len(), 1);
        // Default list named after the case db file's stem, which starts
        // with "tlx-rust-test-casefile-ioclists-".
        assert!(default_lists[0].name.starts_with("tlx-rust-test-casefile-ioclists-"));
        assert_eq!(c.ioc_list_body(default_lists[0].id).unwrap(), "");

        let a = c.create_ioc_list("Alpha").unwrap();
        let b = c.create_ioc_list("beta").unwrap();

        let lists = c.ioc_lists().unwrap();
        assert_eq!(lists.len(), 3);
        assert_eq!(lists[0].name, "Alpha");
        assert_eq!(lists[1].name, "beta");

        c.set_ioc_list_body(a.id, "1.2.3.4\nevil.example.com").unwrap();
        assert_eq!(c.ioc_list_body(a.id).unwrap(), "1.2.3.4\nevil.example.com");
        assert_eq!(c.ioc_list_body(b.id).unwrap(), "");

        assert!(c.create_ioc_list("Alpha").is_err());
        assert!(c.create_ioc_list("ALPHA").is_err());
        assert!(c.create_ioc_list("   ").is_err());

        c.rename_ioc_list(b.id, "Gamma").unwrap();
        assert!(c.rename_ioc_list(a.id, "gamma").is_err());
        c.rename_ioc_list(b.id, "GAMMA").unwrap();

        c.delete_ioc_list(a.id).unwrap();
        let lists = c.ioc_lists().unwrap();
        assert_eq!(lists.len(), 2);
        assert_eq!(lists[0].name, "GAMMA");

        std::fs::remove_file(&path).ok();
    }

    #[test]
    fn test_ioc_list_migrates_legacy_value() {
        // Needs a DB file whose stem is literally "legacy" (the migration
        // names the seeded default list after the case file).
        let dir = std::env::temp_dir().join(format!(
            "tlx-rust-test-casefile-legacy-dir-{}-{}",
            std::process::id(),
            std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("legacy.tlxdb");
        {
            let mut c = Case::create(path.to_str().unwrap()).unwrap();
            c.conn.execute("DELETE FROM ioc_lists", []).unwrap();
            c.conn.execute("DELETE FROM meta WHERE key='ioc_lists_seeded'", []).unwrap();
            c.conn
                .execute("INSERT INTO meta(key,value) VALUES('ioc_list','1.1.1.1')", [])
                .unwrap();

            c.migrate().unwrap();

            let lists = c.ioc_lists().unwrap();
            assert_eq!(lists.len(), 1);
            assert_eq!(lists[0].name, "legacy");
            assert_eq!(c.ioc_list_body(lists[0].id).unwrap(), "1.1.1.1");
            let n: i64 = c
                .conn
                .query_row("SELECT COUNT(*) FROM meta WHERE key='ioc_list'", [], |r| r.get(0))
                .unwrap();
            assert_eq!(n, 0);

            c.delete_ioc_list(lists[0].id).unwrap();
            c.migrate().unwrap();
            let lists = c.ioc_lists().unwrap();
            assert_eq!(lists.len(), 0, "deletion must stick across re-migrate");
        }
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_notes() {
        let (mut c, path) = new_case("notes");
        let idx = Index::new_memory(
            vec!["Timestamp".into(), "Msg".into()],
            vec![vec!["2026-09-01T08:12:03Z".into(), "x".into()]],
        );
        let tl = c.add_timeline("t1", &idx, "2026-09-11T00:00:00Z").unwrap();
        let other = c.add_timeline("t2", &idx, "2026-09-11T00:00:00Z").unwrap();

        let a1 = c.add_note(tl.id, NOTE_ARTIFACT, "first artifact").unwrap();
        let a2 = c.add_note(tl.id, NOTE_ARTIFACT, "second artifact").unwrap();
        let tm = c.add_note(tl.id, NOTE_TIME, "2026-09-01T08:00:00Z").unwrap();
        c.add_note(other.id, NOTE_ARTIFACT, "belongs to t2").unwrap();

        let artifacts = c.notes(tl.id, NOTE_ARTIFACT).unwrap();
        assert_eq!(artifacts.len(), 2);
        assert_eq!(artifacts[0].id, a1.id);
        assert_eq!(artifacts[1].id, a2.id);
        assert_eq!(artifacts[0].text, "first artifact");
        assert!(!artifacts[0].done);

        let times = c.notes(tl.id, NOTE_TIME).unwrap();
        assert_eq!(times.len(), 1);
        assert_eq!(times[0].id, tm.id);
        assert_eq!(times[0].text, "2026-09-01T08:00:00Z");

        let other_artifacts = c.notes(other.id, NOTE_ARTIFACT).unwrap();
        assert_eq!(other_artifacts.len(), 1);
        assert_eq!(other_artifacts[0].text, "belongs to t2");

        c.set_note_done(a1.id, true).unwrap();
        let artifacts = c.notes(tl.id, NOTE_ARTIFACT).unwrap();
        assert!(artifacts[0].done);
        c.set_note_done(a1.id, false).unwrap();
        let artifacts = c.notes(tl.id, NOTE_ARTIFACT).unwrap();
        assert!(!artifacts[0].done);

        c.delete_note(a2.id).unwrap();
        let artifacts = c.notes(tl.id, NOTE_ARTIFACT).unwrap();
        assert_eq!(artifacts.len(), 1);
        assert_eq!(artifacts[0].id, a1.id);

        assert!(c.add_note(tl.id, NOTE_ARTIFACT, "   ").is_err());
        assert!(c.add_note(tl.id, "bogus", "text").is_err());

        std::fs::remove_file(&path).ok();
    }

    #[test]
    fn test_reorder_notes() {
        let (mut c, path) = new_case("reorder");
        let idx = Index::new_memory(
            vec!["Timestamp".into(), "Msg".into()],
            vec![vec!["2026-09-01T08:12:03Z".into(), "x".into()]],
        );
        let tl = c.add_timeline("t1", &idx, "2026-09-11T00:00:00Z").unwrap();
        let mut ids = Vec::new();
        for txt in ["one", "two", "three"] {
            ids.push(c.add_note(tl.id, NOTE_ARTIFACT, txt).unwrap().id);
        }
        let got = c.notes(tl.id, NOTE_ARTIFACT).unwrap();
        assert_eq!(got[0].id, ids[0]);
        assert_eq!(got[2].id, ids[2]);

        c.reorder_notes(&[ids[2], ids[1], ids[0]]).unwrap();
        let got = c.notes(tl.id, NOTE_ARTIFACT).unwrap();
        assert_eq!(got[0].id, ids[2]);
        assert_eq!(got[1].id, ids[1]);
        assert_eq!(got[2].id, ids[0]);

        let n4 = c.add_note(tl.id, NOTE_ARTIFACT, "four").unwrap();
        let got = c.notes(tl.id, NOTE_ARTIFACT).unwrap();
        assert_eq!(got.last().unwrap().id, n4.id);

        std::fs::remove_file(&path).ok();
    }

    #[test]
    fn test_timeline_comment() {
        let (mut c, path) = new_case("tlcomment");
        let idx = Index::new_memory(
            vec!["Timestamp".into(), "Msg".into()],
            vec![vec!["2026-09-01T08:12:03Z".into(), "x".into()]],
        );
        let tl = c.add_timeline("t1", &idx, "2026-09-11T00:00:00Z").unwrap();

        assert_eq!(c.timeline_comment(tl.id).unwrap(), "");

        c.set_timeline_comment(tl.id, "working theory: initial access via phishing").unwrap();
        assert_eq!(c.timeline_comment(tl.id).unwrap(), "working theory: initial access via phishing");

        c.set_timeline_comment(tl.id, "revised theory: lateral movement via RDP").unwrap();
        assert_eq!(c.timeline_comment(tl.id).unwrap(), "revised theory: lateral movement via RDP");

        std::fs::remove_file(&path).ok();
    }
}
