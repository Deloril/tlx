//! Standalone-mode persistence, this port's stand-in for the Fyne app's
//! preferences store (and the Wails port's prefs.go): the state the original
//! keeps per source file outside a case — Investigator's notes, the timeline
//! comment, standalone IOC lists — plus the app-level saved-views list. One
//! JSON file under the OS config dir, loaded whole and rewritten whole on each
//! change (the same trade-off Fyne's own preferences API makes; fine at this
//! size). Case-mode equivalents live in the SQLite case DB (casefile.rs), not
//! here.

use std::collections::HashMap;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};

#[derive(Serialize, Deserialize, Default)]
pub struct PrefsData {
    #[serde(default)]
    pub saved_views: Vec<SavedView>,
    /// Keyed by source file path.
    #[serde(default)]
    pub notes: HashMap<String, NoteSet>,
    #[serde(default)]
    pub timeline_comments: HashMap<String, String>,
    #[serde(default)]
    pub ioc_lists: HashMap<String, Vec<StoredIoc>>,
    /// Dark theme on/off. Defaults to dark (true) to match the Fyne app's
    /// startup variant; see dark_theme().
    #[serde(default = "default_true")]
    pub dark_theme: bool,
}

fn default_true() -> bool {
    true
}

#[derive(Serialize, Deserialize, Default, Clone)]
pub struct NoteSet {
    #[serde(default)]
    pub artifacts: Vec<StoredNote>,
    #[serde(default)]
    pub times: Vec<StoredNote>,
    #[serde(default)]
    pub next_id: i64,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct StoredNote {
    pub id: i64,
    pub text: String,
    #[serde(default)]
    pub done: bool,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct StoredIoc {
    pub name: String,
    #[serde(default)]
    pub body: String,
}

/// SavedView is an application-level saved filter+sort. The sort column is
/// stored by title (not ref) so a view still loosely applies to a timeline
/// with a different column order — unresolved titles fall back to "any
/// column", matching the original.
#[derive(Serialize, Deserialize, Clone, Default)]
pub struct SavedView {
    pub name: String,
    #[serde(default)]
    pub query: String,
    #[serde(default)]
    pub cased: bool,
    #[serde(default)]
    pub tagged_only: bool,
    #[serde(default)]
    pub sort_column: String,
    #[serde(default)]
    pub sort_desc: bool,
    #[serde(default)]
    pub col_filters: HashMap<String, String>, // keyed by column title
    #[serde(default)]
    pub tags: Vec<String>,
}

pub struct Prefs {
    path: PathBuf,
    data: PrefsData,
}

fn prefs_path() -> PathBuf {
    // $XDG_CONFIG_HOME or ~/.config on Linux; ~/Library/Application Support on
    // macOS; %AppData% on Windows. Fall back to the temp dir.
    let base = config_dir().unwrap_or_else(std::env::temp_dir);
    let dir = base.join("tlx-rust");
    let _ = std::fs::create_dir_all(&dir);
    dir.join("prefs.json")
}

#[cfg(target_os = "macos")]
fn config_dir() -> Option<PathBuf> {
    std::env::var_os("HOME").map(|h| PathBuf::from(h).join("Library/Application Support"))
}

#[cfg(target_os = "windows")]
fn config_dir() -> Option<PathBuf> {
    std::env::var_os("APPDATA").map(PathBuf::from)
}

#[cfg(all(unix, not(target_os = "macos")))]
fn config_dir() -> Option<PathBuf> {
    std::env::var_os("XDG_CONFIG_HOME")
        .map(PathBuf::from)
        .or_else(|| std::env::var_os("HOME").map(|h| PathBuf::from(h).join(".config")))
}

impl Prefs {
    /// Load reads the prefs file, returning an empty store if it does not exist
    /// yet or fails to parse. Prefs are convenience state, never fatal.
    pub fn load() -> Self {
        let path = prefs_path();
        let data = std::fs::read(&path)
            .ok()
            .and_then(|b| serde_json::from_slice(&b).ok())
            .unwrap_or_default();
        Prefs { path, data }
    }

    fn save(&self) {
        if let Ok(b) = serde_json::to_vec_pretty(&self.data) {
            let _ = std::fs::write(&self.path, b);
        }
    }

    // --- notes ---------------------------------------------------------------

    pub fn notes(&self, path: &str, kind: NoteKind) -> Vec<StoredNote> {
        match self.data.notes.get(path) {
            Some(ns) => kind.list(ns).clone(),
            None => Vec::new(),
        }
    }

    pub fn add_note(&mut self, path: &str, kind: NoteKind, text: &str) -> StoredNote {
        let ns = self.data.notes.entry(path.to_string()).or_default();
        ns.next_id += 1;
        let note = StoredNote { id: ns.next_id, text: text.trim().to_string(), done: false };
        match kind {
            NoteKind::Artifact => ns.artifacts.push(note.clone()),
            NoteKind::Time => ns.times.push(note.clone()),
        }
        self.save();
        note
    }

    pub fn set_note_done(&mut self, path: &str, kind: NoteKind, id: i64, done: bool) {
        if let Some(ns) = self.data.notes.get_mut(path) {
            let list = match kind {
                NoteKind::Artifact => &mut ns.artifacts,
                NoteKind::Time => &mut ns.times,
            };
            if let Some(n) = list.iter_mut().find(|n| n.id == id) {
                n.done = done;
                self.save();
            }
        }
    }

    pub fn delete_note(&mut self, path: &str, kind: NoteKind, id: i64) {
        if let Some(ns) = self.data.notes.get_mut(path) {
            let list = match kind {
                NoteKind::Artifact => &mut ns.artifacts,
                NoteKind::Time => &mut ns.times,
            };
            list.retain(|n| n.id != id);
            self.save();
        }
    }

    // --- timeline comment ----------------------------------------------------

    pub fn timeline_comment(&self, path: &str) -> String {
        self.data.timeline_comments.get(path).cloned().unwrap_or_default()
    }

    pub fn set_timeline_comment(&mut self, path: &str, text: &str) {
        self.data.timeline_comments.insert(path.to_string(), text.to_string());
        self.save();
    }

    // --- IOC lists -----------------------------------------------------------

    /// ioc_lists returns this file's lists, seeding one named after the file if
    /// none exist yet (matching the standalone auto-seed in the Wails port).
    pub fn ioc_lists(&mut self, path: &str) -> Vec<StoredIoc> {
        if self.data.ioc_lists.get(path).map(|l| l.is_empty()).unwrap_or(true) {
            let name = std::path::Path::new(path)
                .file_stem()
                .and_then(|s| s.to_str())
                .unwrap_or("iocs")
                .to_string();
            self.data
                .ioc_lists
                .insert(path.to_string(), vec![StoredIoc { name, body: String::new() }]);
            self.save();
        }
        self.data.ioc_lists.get(path).cloned().unwrap_or_default()
    }

    pub fn set_ioc_lists(&mut self, path: &str, lists: Vec<StoredIoc>) {
        self.data.ioc_lists.insert(path.to_string(), lists);
        self.save();
    }

    // --- theme ---------------------------------------------------------------

    pub fn dark_theme(&self) -> bool {
        self.data.dark_theme
    }

    pub fn set_dark_theme(&mut self, dark: bool) {
        self.data.dark_theme = dark;
        self.save();
    }

    // --- saved views ---------------------------------------------------------

    pub fn saved_views(&self) -> Vec<SavedView> {
        self.data.saved_views.clone()
    }

    pub fn put_saved_view(&mut self, v: SavedView) {
        if let Some(existing) = self.data.saved_views.iter_mut().find(|e| e.name == v.name) {
            *existing = v;
        } else {
            self.data.saved_views.push(v);
        }
        self.save();
    }

    pub fn delete_saved_view(&mut self, name: &str) {
        self.data.saved_views.retain(|v| v.name != name);
        self.save();
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum NoteKind {
    Artifact,
    Time,
}

impl NoteKind {
    fn list<'a>(&self, ns: &'a NoteSet) -> &'a Vec<StoredNote> {
        match self {
            NoteKind::Artifact => &ns.artifacts,
            NoteKind::Time => &ns.times,
        }
    }
}
