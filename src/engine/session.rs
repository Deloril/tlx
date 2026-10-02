//! Port of internal/model/session.go: Session holds everything the user
//! layers on top of the immutable CSV: the active mode, per-row tags and
//! comments, and per-cell edits. It implements Overlay. State persists to a
//! sidecar JSON file next to the source, so the original CSV is never
//! rewritten in place.

use std::collections::HashMap;
use std::io;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};

use super::view::Overlay;

/// Mode controls what a session permits.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Mode {
    /// ReadOnly forbids every mutation.
    ReadOnly,
    /// Investigator permits tags and comments only; data columns stay locked.
    Investigator,
    /// WorldWrite permits editing any field in addition to tags and comments.
    WorldWrite,
}

impl std::fmt::Display for Mode {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let s = match self {
            Mode::ReadOnly => "Read-only",
            Mode::Investigator => "Investigator",
            Mode::WorldWrite => "World-write",
        };
        f.write_str(s)
    }
}

/// Errors returned by Session mutators. Mirrors Go's ErrReadOnly /
/// ErrColumnLocked sentinel errors.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SessionError {
    ReadOnly,
    ColumnLocked,
    Other(String),
}

impl std::fmt::Display for SessionError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            SessionError::ReadOnly => {
                write!(f, "read-only mode: change to Investigator or World-write to edit")
            }
            SessionError::ColumnLocked => {
                write!(f, "investigator mode only permits editing tags and comments")
            }
            SessionError::Other(s) => write!(f, "{s}"),
        }
    }
}

impl std::error::Error for SessionError {}

pub type Result<T> = std::result::Result<T, SessionError>;

/// TagDef is a tag and the colour it paints rows with. Colour is a
/// "#RRGGBB" hex string; the GUI parses it. The model stays free of any
/// GUI/colour deps.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct TagDef {
    pub name: String,
    pub color: String,
}

/// default_tag_defs are seeded into every session, in priority order (index
/// 0 wins when a row carries more than one). Red for bad, yellow for
/// suspicious, green for good.
fn default_tag_defs() -> Vec<TagDef> {
    vec![
        TagDef { name: "Bad".to_string(), color: "#E53935".to_string() },
        TagDef { name: "Suspicious".to_string(), color: "#FDD835".to_string() },
        TagDef { name: "Good".to_string(), color: "#43A047".to_string() },
    ]
}

/// DEFAULT_TAG_COLOR is used for tags applied or loaded without an explicit
/// colour.
const DEFAULT_TAG_COLOR: &str = "#78909C";

/// NO_HIGHLIGHT_COLOR marks a tag that paints no row background. The GUI
/// offers it as a "No highlight" swatch and row_color treats a row whose top
/// tag carries it as unhighlighted.
pub const NO_HIGHLIGHT_COLOR: &str = "none";

/// SessionSnapshot is a plain-data copy of every annotation in a session:
/// tags, comments, cell edits and the tag palette. It is what an external
/// store (the case database) reads and writes.
#[derive(Clone, Default)]
pub struct SessionSnapshot {
    pub tags: HashMap<usize, Vec<String>>,
    pub comments: HashMap<usize, String>,
    pub edits: HashMap<usize, HashMap<usize, String>>,
    pub tag_defs: Vec<TagDef>,
}

pub struct Session {
    source_path: String,
    sidecar_path: PathBuf,
    mode: Mode,

    tags: HashMap<usize, Vec<String>>,
    comments: HashMap<usize, String>,
    edits: HashMap<usize, HashMap<usize, String>>,

    // Tag palette: an ordered list of definitions (index 0 = highest
    // priority) plus a name->index lookup. Seeded with Bad/Suspicious/Good.
    tag_defs: Vec<TagDef>,
    tag_index: HashMap<String, usize>,

    dirty: bool,
    /// loaded: a sidecar file was read (so we should not re-seed from
    /// columns).
    loaded: bool,
}

/// On-disk sidecar shape. Mirrors Go's `sidecar` struct; row/col keys are
/// strings in JSON (object keys must be strings), parsed back to usize.
#[derive(Serialize, Deserialize, Default)]
struct Sidecar {
    source: String,
    #[serde(default, skip_serializing_if = "HashMap::is_empty")]
    tags: HashMap<String, Vec<String>>,
    #[serde(default, skip_serializing_if = "HashMap::is_empty")]
    comments: HashMap<String, String>,
    #[serde(default, skip_serializing_if = "HashMap::is_empty")]
    edits: HashMap<String, HashMap<String, String>>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    tag_defs: Vec<TagDef>,
    /// Legacy pre-colour format, still read for old sidecars.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    known_tags: Vec<String>,
}

impl Session {
    /// NewSession returns an empty session for source_path in read-only
    /// mode, with the default tag palette seeded.
    pub fn new(source_path: &str) -> Self {
        let mut s = Session {
            source_path: source_path.to_string(),
            sidecar_path: PathBuf::from(format!("{source_path}.tlx.json")),
            mode: Mode::ReadOnly,
            tags: HashMap::new(),
            comments: HashMap::new(),
            edits: HashMap::new(),
            tag_defs: Vec::new(),
            tag_index: HashMap::new(),
            dirty: false,
            loaded: false,
        };
        s.seed_defaults();
        s
    }

    fn seed_defaults(&mut self) {
        for d in default_tag_defs() {
            self.define(&d.name, &d.color, false);
        }
    }

    pub fn sidecar_path(&self) -> &std::path::Path {
        &self.sidecar_path
    }

    pub fn mode(&self) -> Mode {
        self.mode
    }

    pub fn set_mode(&mut self, m: Mode) {
        self.mode = m;
    }

    pub fn dirty(&self) -> bool {
        self.dirty
    }

    /// loaded reports whether a sidecar file was read for this session.
    /// Used to decide whether to seed annotations from a timeline's
    /// existing tag/comment columns: only when there are no stored
    /// annotations to load.
    pub fn loaded(&self) -> bool {
        self.loaded
    }

    pub fn mark_saved(&mut self) {
        self.dirty = false;
    }

    // --- Overlay implementation ---

    /// comment setter used by adopt.rs's seeding path (bypasses the mode
    /// check, since importing is not a user edit).
    pub(super) fn set_comment_raw(&mut self, row: usize, text: String) {
        self.comments.insert(row, text);
    }

    pub(super) fn seed_tag(&mut self, row: usize, tag: &str) {
        let list = self.tags.entry(row).or_default();
        if list.iter().any(|t| t == tag) {
            return;
        }
        list.push(tag.to_string());
        list.sort();
        self.define(tag, DEFAULT_TAG_COLOR, false);
    }

    // --- Mutations ---

    /// AddTag applies a tag to a row. Allowed in Investigator and
    /// World-write. An unknown tag is registered in the palette with the
    /// default colour.
    pub fn add_tag(&mut self, row: usize, tag: &str) -> Result<()> {
        if tag.is_empty() {
            return Ok(());
        }
        if self.mode == Mode::ReadOnly {
            return Err(SessionError::ReadOnly);
        }
        let list = self.tags.entry(row).or_default();
        if list.iter().any(|t| t == tag) {
            return Ok(());
        }
        list.push(tag.to_string());
        list.sort();
        self.define(tag, DEFAULT_TAG_COLOR, false);
        self.dirty = true;
        Ok(())
    }

    /// RemoveTag removes a tag from a row.
    pub fn remove_tag(&mut self, row: usize, tag: &str) -> Result<()> {
        if self.mode == Mode::ReadOnly {
            return Err(SessionError::ReadOnly);
        }
        if let Some(list) = self.tags.get_mut(&row) {
            list.retain(|t| t != tag);
            if list.is_empty() {
                self.tags.remove(&row);
            }
        }
        self.dirty = true;
        Ok(())
    }

    /// DeleteTag removes a tag from the palette and strips it from every row
    /// that carries it.
    pub fn delete_tag(&mut self, name: &str) -> Result<()> {
        let name = name.trim();
        if name.is_empty() {
            return Ok(());
        }
        if self.mode == Mode::ReadOnly {
            return Err(SessionError::ReadOnly);
        }
        if !self.tag_index.contains_key(name) {
            return Ok(());
        }
        self.tag_defs.retain(|d| d.name != name);
        self.tag_index = self
            .tag_defs
            .iter()
            .enumerate()
            .map(|(i, d)| (d.name.clone(), i))
            .collect();
        let rows: Vec<usize> = self.tags.keys().cloned().collect();
        for row in rows {
            if let Some(list) = self.tags.get_mut(&row) {
                list.retain(|t| t != name);
                if list.is_empty() {
                    self.tags.remove(&row);
                }
            }
        }
        self.dirty = true;
        Ok(())
    }

    /// RenameTag changes a tag's name across the palette and every row that
    /// carries it, keeping the tag's colour and priority.
    pub fn rename_tag(&mut self, old_name: &str, new_name: &str) -> Result<()> {
        let old_name = old_name.trim();
        let new_name = new_name.trim();
        if new_name.is_empty() {
            return Err(SessionError::Other("tag name cannot be empty".to_string()));
        }
        if old_name == new_name {
            return Ok(());
        }
        if self.mode == Mode::ReadOnly {
            return Err(SessionError::ReadOnly);
        }
        let i = *self
            .tag_index
            .get(old_name)
            .ok_or_else(|| SessionError::Other(format!("unknown tag {old_name:?}")))?;
        if self.tag_index.contains_key(new_name) {
            return Err(SessionError::Other(format!(
                "a tag named {new_name:?} already exists"
            )));
        }
        self.tag_defs[i].name = new_name.to_string();
        self.tag_index.remove(old_name);
        self.tag_index.insert(new_name.to_string(), i);
        for list in self.tags.values_mut() {
            let mut changed = false;
            for t in list.iter_mut() {
                if t == old_name {
                    *t = new_name.to_string();
                    changed = true;
                }
            }
            if changed {
                list.sort();
            }
        }
        self.dirty = true;
        Ok(())
    }

    /// SetComment sets a row's comment. Allowed in Investigator and
    /// World-write.
    pub fn set_comment(&mut self, row: usize, text: &str) -> Result<()> {
        if self.mode == Mode::ReadOnly {
            return Err(SessionError::ReadOnly);
        }
        if text.is_empty() {
            self.comments.remove(&row);
        } else {
            self.comments.insert(row, text.to_string());
        }
        self.dirty = true;
        Ok(())
    }

    /// SetCell overrides a data cell's value. World-write only.
    pub fn set_cell(&mut self, row: usize, col: usize, value: &str) -> Result<()> {
        match self.mode {
            Mode::ReadOnly => return Err(SessionError::ReadOnly),
            Mode::Investigator => return Err(SessionError::ColumnLocked),
            Mode::WorldWrite => {}
        }
        self.edits.entry(row).or_default().insert(col, value.to_string());
        self.dirty = true;
        Ok(())
    }

    /// ClearCell removes a cell edit, reverting to the original value.
    pub fn clear_cell(&mut self, row: usize, col: usize) -> Result<()> {
        match self.mode {
            Mode::ReadOnly => return Err(SessionError::ReadOnly),
            Mode::Investigator => return Err(SessionError::ColumnLocked),
            Mode::WorldWrite => {}
        }
        if let Some(cols) = self.edits.get_mut(&row) {
            cols.remove(&col);
            if cols.is_empty() {
                self.edits.remove(&row);
            }
            self.dirty = true;
        }
        Ok(())
    }

    /// KnownTags returns the palette tag names in priority order.
    pub fn known_tags(&self) -> Vec<String> {
        self.tag_defs.iter().map(|d| d.name.clone()).collect()
    }

    /// TagDefs returns a copy of the tag palette in priority order.
    pub fn tag_defs(&self) -> Vec<TagDef> {
        self.tag_defs.clone()
    }

    /// TagColor returns the palette colour for a tag, or None if unknown.
    pub fn tag_color(&self, name: &str) -> Option<String> {
        self.tag_index.get(name).map(|&i| self.tag_defs[i].color.clone())
    }

    /// DefineTag registers a tag or updates its colour. Recolouring is
    /// presentation state, so it is allowed in any mode; it does set the
    /// dirty flag.
    pub fn define_tag(&mut self, name: &str, color: &str) -> Result<()> {
        let name = name.trim();
        if name.is_empty() {
            return Err(SessionError::Other("tag name cannot be empty".to_string()));
        }
        self.define(name, color, true);
        self.dirty = true;
        Ok(())
    }

    /// define adds a tag to the palette, or updates its colour when
    /// `override_color` is true.
    fn define(&mut self, name: &str, color: &str, override_color: bool) {
        let color = if color.is_empty() { DEFAULT_TAG_COLOR } else { color };
        if let Some(&i) = self.tag_index.get(name) {
            if override_color {
                self.tag_defs[i].color = color.to_string();
            }
            return;
        }
        self.tag_index.insert(name.to_string(), self.tag_defs.len());
        self.tag_defs.push(TagDef { name: name.to_string(), color: color.to_string() });
    }

    /// RowColor returns the highlight colour for a row: the colour of its
    /// highest-priority tag (lowest palette index). Returns None for an
    /// untagged row.
    pub fn row_color(&self, row: usize) -> Option<String> {
        let mut best: Option<usize> = None;
        if let Some(list) = self.tags.get(&row) {
            for t in list {
                if let Some(&i) = self.tag_index.get(t) {
                    if best.is_none_or(|b| i < b) {
                        best = Some(i);
                    }
                }
            }
        }
        let best = best?;
        let c = &self.tag_defs[best].color;
        if c != NO_HIGHLIGHT_COLOR {
            Some(c.clone())
        } else {
            None
        }
    }

    /// Snapshot returns a deep copy of all annotation state.
    pub fn snapshot(&self) -> SessionSnapshot {
        SessionSnapshot {
            tags: self.tags.clone(),
            comments: self.comments.clone(),
            edits: self.edits.clone(),
            tag_defs: self.tag_defs.clone(),
        }
    }

    /// LoadSnapshot replaces all annotation state from a snapshot. It
    /// ignores the mode (loading is not a user edit) and leaves the session
    /// clean. The palette defaults remain seeded; snapshot definitions
    /// override their colours and set priority order.
    pub fn load_snapshot(&mut self, snap: SessionSnapshot) {
        self.tags.clear();
        self.comments.clear();
        self.edits.clear();
        for d in &snap.tag_defs {
            self.define(&d.name, &d.color, true);
        }
        for (row, t) in snap.tags {
            for tag in &t {
                self.define(tag, DEFAULT_TAG_COLOR, false);
            }
            self.tags.insert(row, t);
        }
        for (row, c) in snap.comments {
            if !c.is_empty() {
                self.comments.insert(row, c);
            }
        }
        for (row, cols) in snap.edits {
            if !cols.is_empty() {
                self.edits.insert(row, cols);
            }
        }
        self.dirty = false;
    }

    // --- Persistence ---

    /// Load reads the sidecar file if it exists. A missing file is not an
    /// error.
    pub fn load(&mut self) -> io::Result<()> {
        let data = match std::fs::read_to_string(&self.sidecar_path) {
            Ok(d) => d,
            Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
            Err(e) => return Err(e),
        };
        let sc: Sidecar = serde_json::from_str(&data)
            .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
        self.loaded = true;
        for d in &sc.tag_defs {
            self.define(&d.name, &d.color, true);
        }
        for (k, v) in sc.tags {
            if let Ok(row) = k.parse::<usize>() {
                // Drop plaso's "-" null marker if an earlier version seeded it
                // as a real tag; it was never a tag the user set.
                let v: Vec<String> = v.into_iter().filter(|t| t != "-").collect();
                if v.is_empty() {
                    continue;
                }
                for t in &v {
                    self.define(t, DEFAULT_TAG_COLOR, false);
                }
                self.tags.insert(row, v);
            }
        }
        for t in &sc.known_tags {
            self.define(t, DEFAULT_TAG_COLOR, false);
        }
        for (k, v) in sc.comments {
            if let Ok(row) = k.parse::<usize>() {
                self.comments.insert(row, v);
            }
        }
        for (k, cols) in sc.edits {
            let Ok(row) = k.parse::<usize>() else { continue };
            let mut m = HashMap::new();
            for (ck, cv) in cols {
                if let Ok(col) = ck.parse::<usize>() {
                    m.insert(col, cv);
                }
            }
            if !m.is_empty() {
                self.edits.insert(row, m);
            }
        }
        self.dirty = false;
        Ok(())
    }

    /// Save writes the sidecar file atomically (temp file then rename).
    pub fn save(&mut self) -> io::Result<()> {
        let mut sc = Sidecar {
            source: self.source_path.clone(),
            ..Default::default()
        };
        if !self.tags.is_empty() {
            sc.tags = self.tags.iter().map(|(k, v)| (k.to_string(), v.clone())).collect();
        }
        if !self.comments.is_empty() {
            sc.comments = self.comments.iter().map(|(k, v)| (k.to_string(), v.clone())).collect();
        }
        if !self.edits.is_empty() {
            sc.edits = self
                .edits
                .iter()
                .map(|(row, cols)| {
                    (
                        row.to_string(),
                        cols.iter().map(|(c, v)| (c.to_string(), v.clone())).collect(),
                    )
                })
                .collect();
        }
        if !self.tag_defs.is_empty() {
            sc.tag_defs = self.tag_defs.clone();
        }

        let data = serde_json::to_string_pretty(&sc)
            .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
        let tmp = self.sidecar_path.with_extension(
            self.sidecar_path
                .extension()
                .map(|e| format!("{}.tmp", e.to_string_lossy()))
                .unwrap_or_else(|| "tmp".to_string()),
        );
        std::fs::write(&tmp, data)?;
        std::fs::rename(&tmp, &self.sidecar_path)?;
        self.dirty = false;
        Ok(())
    }
}

impl Default for Session {
    fn default() -> Self {
        Self::new("")
    }
}

impl Overlay for Session {
    fn tags(&self, row: usize) -> Vec<String> {
        self.tags.get(&row).cloned().unwrap_or_default()
    }

    fn comment(&self, row: usize) -> String {
        self.comments.get(&row).cloned().unwrap_or_default()
    }

    fn cell_override(&self, row: usize, col: usize) -> Option<String> {
        self.edits.get(&row).and_then(|m| m.get(&col)).cloned()
    }
}

/// SessionSnapshot is a plain-data copy of the annotations, so it is
/// Send + Sync and can be shared with the background filter threads without
/// borrowing the live Session (which the UI thread may mutate). Filtering only
/// reads tags/comments/edits, so the snapshot is all the scan needs.
impl Overlay for SessionSnapshot {
    fn tags(&self, row: usize) -> Vec<String> {
        self.tags.get(&row).cloned().unwrap_or_default()
    }

    fn comment(&self, row: usize) -> String {
        self.comments.get(&row).cloned().unwrap_or_default()
    }

    fn cell_override(&self, row: usize, col: usize) -> Option<String> {
        self.edits.get(&row).and_then(|m| m.get(&col)).cloned()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::engine::index::open_temp;

    #[test]
    fn test_modes_guard_mutations() {
        let (idx, _p) = open_temp("a,b\n1,2\n");
        let mut s = Session::new(idx.path());

        // Read-only: everything blocked.
        assert_eq!(s.add_tag(0, "x"), Err(SessionError::ReadOnly));
        assert_eq!(s.set_comment(0, "hi"), Err(SessionError::ReadOnly));
        assert_eq!(s.set_cell(0, 0, "z"), Err(SessionError::ReadOnly));

        // Investigator: tags and comments only.
        s.set_mode(Mode::Investigator);
        s.add_tag(0, "malware").unwrap();
        s.set_comment(0, "suspicious").unwrap();
        assert_eq!(s.set_cell(0, 0, "z"), Err(SessionError::ColumnLocked));

        // World-write: cells too.
        s.set_mode(Mode::WorldWrite);
        s.set_cell(0, 0, "EDITED").unwrap();
        assert_eq!(s.cell_override(0, 0), Some("EDITED".to_string()));
        assert_eq!(s.tags(0), vec!["malware".to_string()]);
    }

    #[test]
    fn test_session_persistence() {
        let (idx, _p) = open_temp("a,b\n1,2\n3,4\n");
        let mut s = Session::new(idx.path());
        s.set_mode(Mode::WorldWrite);
        s.add_tag(0, "beacon").unwrap();
        s.add_tag(0, "c2").unwrap();
        s.set_comment(1, "note here").unwrap();
        s.set_cell(1, 0, "99").unwrap();
        assert!(s.dirty(), "expected dirty");
        s.save().unwrap();
        assert!(!s.dirty(), "still dirty after save");

        // Reload into a fresh session.
        let mut s2 = Session::new(idx.path());
        s2.load().unwrap();
        assert_eq!(s2.tags(0).join(","), "beacon,c2");
        assert_eq!(s2.comment(1), "note here");
        assert_eq!(s2.cell_override(1, 0), Some("99".to_string()));
        // known_tags leads with the seeded defaults, then user tags in the
        // order they were first applied.
        assert_eq!(s2.known_tags().join(","), "Bad,Suspicious,Good,beacon,c2");

        std::fs::remove_file(s.sidecar_path()).ok();
    }

    #[test]
    fn test_default_tag_colors() {
        let s = Session::new("x.csv");
        for d in default_tag_defs() {
            assert_eq!(s.tag_color(&d.name), Some(d.color), "default colour for {}", d.name);
        }
        // Bad outranks Good on a row carrying both.
        let mut s = s;
        s.set_mode(Mode::Investigator);
        s.add_tag(0, "Good").unwrap();
        s.add_tag(0, "Bad").unwrap();
        assert_eq!(s.row_color(0), Some("#E53935".to_string()), "row colour should be Bad red");
    }

    #[test]
    fn test_custom_tag_color_persists() {
        let (idx, _p) = open_temp("a\n1\n2\n");
        let mut s = Session::new(idx.path());
        s.set_mode(Mode::Investigator);
        s.define_tag("beacon", "#123456").unwrap();
        s.add_tag(0, "beacon").unwrap();
        s.save().unwrap();

        let mut s2 = Session::new(idx.path());
        s2.load().unwrap();
        assert_eq!(s2.tag_color("beacon"), Some("#123456".to_string()));
        // Defaults survive a reload too.
        assert_eq!(s2.tag_color("Bad"), Some("#E53935".to_string()));

        std::fs::remove_file(s.sidecar_path()).ok();
    }

    #[test]
    fn test_delete_tag() {
        let mut s = Session::new("x.csv");
        s.set_mode(Mode::Investigator);
        s.add_tag(0, "beacon").unwrap();
        s.add_tag(0, "Bad").unwrap();
        s.add_tag(1, "beacon").unwrap();

        s.delete_tag("beacon").unwrap();
        // Gone from the palette lookup.
        assert_eq!(s.tag_color("beacon"), None, "beacon still in palette after delete");
        // Gone from every row; the other tag stays.
        assert_eq!(s.tags(0), vec!["Bad".to_string()]);
        assert!(s.tags(1).is_empty());

        // Read-only sessions must refuse it, like other annotation edits.
        s.set_mode(Mode::ReadOnly);
        assert_eq!(s.delete_tag("Bad"), Err(SessionError::ReadOnly));
    }

    #[test]
    fn test_no_highlight_color() {
        let mut s = Session::new("x.csv");
        s.set_mode(Mode::Investigator);
        s.define_tag("note", NO_HIGHLIGHT_COLOR).unwrap();

        // A row whose only tag is no-highlight paints no background.
        s.add_tag(0, "note").unwrap();
        assert_eq!(s.row_color(0), None, "no-highlight row should have no colour");
        // tag_color still reports the sentinel so the editor can preselect it.
        assert_eq!(s.tag_color("note"), Some(NO_HIGHLIGHT_COLOR.to_string()));

        // A higher-priority coloured tag still wins over a no-highlight one.
        s.add_tag(1, "note").unwrap();
        s.add_tag(1, "Bad").unwrap();
        assert_eq!(s.row_color(1), Some("#E53935".to_string()), "row with Bad + note should be Bad red");
    }

    #[test]
    fn test_rename_tag() {
        let mut s = Session::new("x.csv");
        s.set_mode(Mode::Investigator);
        s.define_tag("beacon", "#123456").unwrap();
        s.add_tag(0, "beacon").unwrap();
        s.add_tag(0, "Bad").unwrap();
        s.add_tag(1, "beacon").unwrap();

        s.rename_tag("beacon", "c2").unwrap();
        // Old name gone, new name carries the old colour.
        assert_eq!(s.tag_color("beacon"), None, "old name still in palette after rename");
        assert_eq!(s.tag_color("c2"), Some("#123456".to_string()));
        // Rows swapped the name, staying sorted, and other tags are
        // untouched.
        assert_eq!(s.tags(0).join(","), "Bad,c2");
        assert_eq!(s.tags(1), vec!["c2".to_string()]);

        // Renaming onto an existing tag is rejected, not merged.
        assert!(s.rename_tag("c2", "Bad").is_err());
        // Unknown source tag errors.
        assert!(s.rename_tag("nope", "x").is_err());
        // Read-only refuses it.
        s.set_mode(Mode::ReadOnly);
        assert_eq!(s.rename_tag("c2", "c3"), Err(SessionError::ReadOnly));
    }
}
