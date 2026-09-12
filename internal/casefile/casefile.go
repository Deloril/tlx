// Package casefile stores a set of related timelines and their annotations in a
// single SQLite database, so an investigator can work across several CSVs as one
// case. The source CSVs stay on disk and are never imported wholesale; the
// database holds each timeline's registration, the shared tag palette, per-row
// annotations, and a small snapshot of every tagged row that feeds the master
// timeline view without needing the source file open.
//
// This package depends only on internal/model (for TagDef/SessionSnapshot and
// timestamp parsing) and a pure-Go SQLite driver, so it builds without cgo.
package casefile

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"tlx/internal/model"
)

const schemaVersion = 1

// TimelineMeta is a registered timeline within a case.
type TimelineMeta struct {
	ID         int64
	Name       string
	SourcePath string
	Delimiter  string
	Headers    []string
	// DisplayHeaders are the per-column display names, one per source header.
	// They start equal to Headers and can be renamed so timelines with
	// differently-named columns line up under shared names in the master view.
	DisplayHeaders []string
	TimeCol        int // index of the timestamp column, or -1 if none detected
	AddedAt        string
}

// displayHeadersOrRaw returns t.DisplayHeaders, padded from Headers where a
// display name is missing, so callers always get one name per source column.
func (t TimelineMeta) displayHeadersOrRaw() []string {
	out := make([]string, len(t.Headers))
	for i := range t.Headers {
		if i < len(t.DisplayHeaders) && t.DisplayHeaders[i] != "" {
			out[i] = t.DisplayHeaders[i]
		} else {
			out[i] = t.Headers[i]
		}
	}
	return out
}

// MasterEntry is one tagged row surfaced in the master timeline. Content is
// taken from the stored snapshot, so it is available even if the source CSV is
// offline.
type MasterEntry struct {
	TimelineID int64
	Timeline   string
	Row        int // master row index within its own timeline
	TimeRaw    string
	Time       time.Time
	HasTime    bool
	Tags       []string
	Comment    string
	Summary    string
	// Cells is the snapshot of the row's source cell values (edits applied),
	// and DisplayHeaders names them, so the master view can place each value
	// under its canonical column. Both may be empty for pre-merge snapshots.
	Cells          []string
	DisplayHeaders []string
}

// Case is an open case database.
type Case struct {
	db   *sql.DB
	path string
}

// Create makes a new case database at path and initialises its schema and
// the default tag palette. It fails if the file cannot be created.
func Create(path string) (*Case, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	c := &Case{db: db, path: path}
	if err := c.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

// Open opens an existing case database, initialising the schema if the file
// is new or empty (so Open doubles as Create for a fresh path).
func Open(path string) (*Case, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	c := &Case{db: db, path: path}
	if err := c.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite tolerates one writer; a single connection avoids "database is
	// locked" under our simple, low-concurrency access.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Path is the database file path.
func (c *Case) Path() string { return c.path }

// Close releases the database.
func (c *Case) Close() error { return c.db.Close() }

func (c *Case) initSchema() error {
	const ddl = `
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
);`
	if _, err := c.db.Exec(ddl); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	// Record schema version and seed the palette once.
	var have string
	err := c.db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&have)
	if err == sql.ErrNoRows {
		if _, err := c.db.Exec(`INSERT INTO meta(key,value) VALUES('schema_version',?)`,
			fmt.Sprint(schemaVersion)); err != nil {
			return err
		}
		if err := c.seedPalette(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return c.migrate()
}

// migrate applies additive schema changes on top of the base DDL so older case
// files keep working. Each step is guarded, so running it repeatedly is safe.
func (c *Case) migrate() error {
	// Merged master view: per-timeline display names and a per-tagged-row cell
	// snapshot. Both default to empty on old rows; readers fall back gracefully.
	if err := c.addColumnIfMissing("timelines", "display_headers", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing("tagged_snapshot", "cells", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	// Per-timeline free-text comment, a running note toward a final write-up.
	if err := c.addColumnIfMissing("timelines", "comment", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// Multiple named IOC lists, replacing the single meta['ioc_list'] value.
	if _, err := c.db.Exec(`CREATE TABLE IF NOT EXISTS ioc_lists (
	    id   INTEGER PRIMARY KEY AUTOINCREMENT,
	    name TEXT NOT NULL UNIQUE,
	    body TEXT NOT NULL DEFAULT ''
	);`); err != nil {
		return fmt.Errorf("create ioc_lists: %w", err)
	}
	if err := c.seedDefaultIOCList(); err != nil {
		return err
	}
	// Per-timeline investigator notes, not shared between timelines.
	if _, err := c.db.Exec(`CREATE TABLE IF NOT EXISTS notes (
	    id          INTEGER PRIMARY KEY AUTOINCREMENT,
	    timeline_id INTEGER NOT NULL,
	    kind        TEXT NOT NULL,
	    text        TEXT NOT NULL,
	    done        INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_notes_tl ON notes(timeline_id);`); err != nil {
		return fmt.Errorf("create notes: %w", err)
	}
	return nil
}

// seedDefaultIOCList gives every case a default IOC list named after the case
// file, carrying forward any body from the old single meta['ioc_list'] value.
// It runs exactly once, guarded by the meta['ioc_lists_seeded'] flag, so a
// list the user later deletes is never resurrected on reopen.
func (c *Case) seedDefaultIOCList() error {
	var seeded string
	err := c.db.QueryRow(`SELECT value FROM meta WHERE key='ioc_lists_seeded'`).Scan(&seeded)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		return nil
	}

	var body string
	if err := c.db.QueryRow(`SELECT value FROM meta WHERE key='ioc_list'`).Scan(&body); err != nil && err != sql.ErrNoRows {
		return err
	}

	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO ioc_lists(name,body) VALUES(?,?)`,
		c.caseName(), body); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM meta WHERE key='ioc_list'`); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('ioc_lists_seeded','1')
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// caseName derives a default list/case display name from the database file
// path: its base name without extension, or "case" if that is empty.
func (c *Case) caseName() string {
	name := strings.TrimSuffix(filepath.Base(c.path), filepath.Ext(c.path))
	if name == "" {
		return "case"
	}
	return name
}

// addColumnIfMissing adds a column to a table unless it is already present.
func (c *Case) addColumnIfMissing(table, column, decl string) error {
	rows, err := c.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = c.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl))
	return err
}

// seedPalette writes the built-in tag defaults if the palette is empty.
func (c *Case) seedPalette() error {
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM tag_defs`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	defs := model.NewSession("").TagDefs() // seeded defaults, in priority order
	return c.SaveTagDefs(defs)
}

// Timelines returns every registered timeline, oldest first.
func (c *Case) Timelines() ([]TimelineMeta, error) {
	rows, err := c.db.Query(`SELECT id,name,source_path,delimiter,headers,display_headers,time_col,added_at
		FROM timelines ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimelineMeta
	for rows.Next() {
		var t TimelineMeta
		var headersJSON, displayJSON string
		if err := rows.Scan(&t.ID, &t.Name, &t.SourcePath, &t.Delimiter,
			&headersJSON, &displayJSON, &t.TimeCol, &t.AddedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(headersJSON), &t.Headers)
		json.Unmarshal([]byte(displayJSON), &t.DisplayHeaders)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Timeline returns a single timeline by id.
func (c *Case) Timeline(id int64) (TimelineMeta, error) {
	var t TimelineMeta
	var headersJSON, displayJSON string
	err := c.db.QueryRow(`SELECT id,name,source_path,delimiter,headers,display_headers,time_col,added_at
		FROM timelines WHERE id=?`, id).Scan(&t.ID, &t.Name, &t.SourcePath,
		&t.Delimiter, &headersJSON, &displayJSON, &t.TimeCol, &t.AddedAt)
	if err != nil {
		return TimelineMeta{}, err
	}
	json.Unmarshal([]byte(headersJSON), &t.Headers)
	json.Unmarshal([]byte(displayJSON), &t.DisplayHeaders)
	return t, nil
}

// AddTimeline registers an already-opened index as a timeline in the case.
// The timestamp column is auto-detected. name defaults to the file's base name
// when empty. addedAt is supplied by the caller (the model forbids wall-clock
// reads in some contexts); pass time.Now().Format(time.RFC3339) or similar.
func (c *Case) AddTimeline(name string, idx *model.Index, addedAt string) (TimelineMeta, error) {
	headers := idx.Headers()
	headersJSON, _ := json.Marshal(headers)
	timeCol := model.DetectTimeColumn(idx)
	res, err := c.db.Exec(`INSERT INTO timelines(name,source_path,delimiter,headers,display_headers,time_col,added_at)
		VALUES(?,?,?,?,?,?,?)`, name, idx.Path(), string(idx.Delimiter()),
		string(headersJSON), string(headersJSON), timeCol, addedAt)
	if err != nil {
		return TimelineMeta{}, err
	}
	id, _ := res.LastInsertId()
	return TimelineMeta{
		ID: id, Name: name, SourcePath: idx.Path(),
		Delimiter: string(idx.Delimiter()), Headers: headers,
		DisplayHeaders: append([]string(nil), headers...),
		TimeCol:        timeCol, AddedAt: addedAt,
	}, nil
}

// SetColumnNames stores per-column display names for a timeline. names is one
// entry per source column; a blank entry falls back to the source header.
func (c *Case) SetColumnNames(id int64, names []string) error {
	data, err := json.Marshal(names)
	if err != nil {
		return err
	}
	_, err = c.db.Exec(`UPDATE timelines SET display_headers=? WHERE id=?`, string(data), id)
	return err
}

// TimelineComment returns the free-text comment stored for a timeline, or "".
func (c *Case) TimelineComment(id int64) (string, error) {
	var text string
	err := c.db.QueryRow(`SELECT comment FROM timelines WHERE id=?`, id).Scan(&text)
	return text, err
}

// SetTimelineComment stores a timeline's free-text comment.
func (c *Case) SetTimelineComment(id int64, text string) error {
	_, err := c.db.Exec(`UPDATE timelines SET comment=? WHERE id=?`, text, id)
	return err
}

// RemoveTimeline deletes a timeline and all of its annotations.
func (c *Case) RemoveTimeline(id int64) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM tags WHERE timeline_id=?`,
		`DELETE FROM comments WHERE timeline_id=?`,
		`DELETE FROM edits WHERE timeline_id=?`,
		`DELETE FROM tagged_snapshot WHERE timeline_id=?`,
		`DELETE FROM notes WHERE timeline_id=?`,
		`DELETE FROM timelines WHERE id=?`,
	} {
		if _, err := tx.Exec(stmt, id); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// SetTimeColumn overrides the detected timestamp column for a timeline.
func (c *Case) SetTimeColumn(id int64, col int) error {
	_, err := c.db.Exec(`UPDATE timelines SET time_col=? WHERE id=?`, col, id)
	return err
}

// TagDefs returns the case's shared tag palette in priority order.
func (c *Case) TagDefs() ([]model.TagDef, error) {
	rows, err := c.db.Query(`SELECT name,color FROM tag_defs ORDER BY priority`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.TagDef
	for rows.Next() {
		var d model.TagDef
		if err := rows.Scan(&d.Name, &d.Color); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SaveTagDefs replaces the palette with defs, preserving their order as priority.
func (c *Case) SaveTagDefs(defs []model.TagDef) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM tag_defs`); err != nil {
		tx.Rollback()
		return err
	}
	for i, d := range defs {
		if _, err := tx.Exec(`INSERT INTO tag_defs(name,color,priority) VALUES(?,?,?)`,
			d.Name, d.Color, i); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// DeleteTag removes a tag from the case palette and strips it from every row of
// every timeline. Snapshot rows left with no tags drop out, so they no longer
// appear in the master view.
func (c *Case) DeleteTag(name string) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	rollback := func(e error) error { tx.Rollback(); return e }
	if _, err := tx.Exec(`DELETE FROM tag_defs WHERE name=?`, name); err != nil {
		return rollback(err)
	}
	if _, err := tx.Exec(`DELETE FROM tags WHERE tag=?`, name); err != nil {
		return rollback(err)
	}
	if _, err := tx.Exec(`DELETE FROM tagged_snapshot
		WHERE NOT EXISTS (SELECT 1 FROM tags
			WHERE tags.timeline_id = tagged_snapshot.timeline_id
			  AND tags.row = tagged_snapshot.row)`); err != nil {
		return rollback(err)
	}
	return tx.Commit()
}

// LoadAnnotations reads a timeline's annotations into a snapshot. The case
// palette is included so a session opened from it paints rows consistently.
func (c *Case) LoadAnnotations(timelineID int64) (model.SessionSnapshot, error) {
	snap := model.SessionSnapshot{
		Tags:     map[int][]string{},
		Comments: map[int]string{},
		Edits:    map[int]map[int]string{},
	}
	defs, err := c.TagDefs()
	if err != nil {
		return snap, err
	}
	snap.TagDefs = defs

	tagRows, err := c.db.Query(`SELECT row,tag FROM tags WHERE timeline_id=?`, timelineID)
	if err != nil {
		return snap, err
	}
	for tagRows.Next() {
		var row int
		var tag string
		if err := tagRows.Scan(&row, &tag); err != nil {
			tagRows.Close()
			return snap, err
		}
		snap.Tags[row] = append(snap.Tags[row], tag)
	}
	tagRows.Close()
	for row := range snap.Tags {
		sort.Strings(snap.Tags[row])
	}

	cRows, err := c.db.Query(`SELECT row,comment FROM comments WHERE timeline_id=?`, timelineID)
	if err != nil {
		return snap, err
	}
	for cRows.Next() {
		var row int
		var c string
		if err := cRows.Scan(&row, &c); err != nil {
			cRows.Close()
			return snap, err
		}
		snap.Comments[row] = c
	}
	cRows.Close()

	eRows, err := c.db.Query(`SELECT row,col,value FROM edits WHERE timeline_id=?`, timelineID)
	if err != nil {
		return snap, err
	}
	for eRows.Next() {
		var row, col int
		var v string
		if err := eRows.Scan(&row, &col, &v); err != nil {
			eRows.Close()
			return snap, err
		}
		if snap.Edits[row] == nil {
			snap.Edits[row] = map[int]string{}
		}
		snap.Edits[row][col] = v
	}
	eRows.Close()
	return snap, nil
}

// RowReader supplies the source content of a row for snapshotting. *model.Index
// satisfies it.
type RowReader interface {
	Row(i int) ([]string, error)
}

// SaveAnnotations writes a timeline's annotations back to the database, replacing
// any prior state for that timeline, and rebuilds its tagged-row snapshot from
// rows (the open index for the timeline). The case palette is updated from
// the snapshot so newly defined tags and recolourings persist.
func (c *Case) SaveAnnotations(tl TimelineMeta, snap model.SessionSnapshot, rows RowReader) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	rollback := func(e error) error { tx.Rollback(); return e }

	for _, stmt := range []string{
		`DELETE FROM tags WHERE timeline_id=?`,
		`DELETE FROM comments WHERE timeline_id=?`,
		`DELETE FROM edits WHERE timeline_id=?`,
		`DELETE FROM tagged_snapshot WHERE timeline_id=?`,
	} {
		if _, err := tx.Exec(stmt, tl.ID); err != nil {
			return rollback(err)
		}
	}

	for row, tags := range snap.Tags {
		for _, tag := range tags {
			if _, err := tx.Exec(`INSERT INTO tags(timeline_id,row,tag) VALUES(?,?,?)`,
				tl.ID, row, tag); err != nil {
				return rollback(err)
			}
		}
	}
	for row, c := range snap.Comments {
		if c == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO comments(timeline_id,row,comment) VALUES(?,?,?)`,
			tl.ID, row, c); err != nil {
			return rollback(err)
		}
	}
	for row, cols := range snap.Edits {
		for col, v := range cols {
			if _, err := tx.Exec(`INSERT INTO edits(timeline_id,row,col,value) VALUES(?,?,?,?)`,
				tl.ID, row, col, v); err != nil {
				return rollback(err)
			}
		}
	}

	// Snapshot every tagged row so the master view needs no source file.
	for row := range snap.Tags {
		if len(snap.Tags[row]) == 0 {
			continue
		}
		var rec []string
		if rows != nil {
			if r, err := rows.Row(row); err == nil {
				rec = r
			}
		}
		rec = applyEdits(rec, snap.Edits[row])
		timeRaw, timeUnix, hasTime := extractTime(rec, tl.TimeCol)
		summary := summarize(rec)
		cellsJSON, _ := json.Marshal(rec)
		if _, err := tx.Exec(`INSERT INTO tagged_snapshot(timeline_id,row,time_unix,time_raw,summary,cells)
			VALUES(?,?,?,?,?,?)`, tl.ID, row, nullableUnix(timeUnix, hasTime), timeRaw, summary, string(cellsJSON)); err != nil {
			return rollback(err)
		}
	}

	if len(snap.TagDefs) > 0 {
		if _, err := tx.Exec(`DELETE FROM tag_defs`); err != nil {
			return rollback(err)
		}
		for i, d := range snap.TagDefs {
			if _, err := tx.Exec(`INSERT INTO tag_defs(name,color,priority) VALUES(?,?,?)`,
				d.Name, d.Color, i); err != nil {
				return rollback(err)
			}
		}
	}
	return tx.Commit()
}

// IOCListMeta identifies a named IOC list within a case.
type IOCListMeta struct {
	ID   int64
	Name string
}

// IOCLists returns every named IOC list in the case, ordered by name.
func (c *Case) IOCLists() ([]IOCListMeta, error) {
	rows, err := c.db.Query(`SELECT id,name FROM ioc_lists ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IOCListMeta
	for rows.Next() {
		var m IOCListMeta
		if err := rows.Scan(&m.ID, &m.Name); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// IOCListBody returns the body text (one indicator per line) of the list with
// the given id.
func (c *Case) IOCListBody(id int64) (string, error) {
	var body string
	err := c.db.QueryRow(`SELECT body FROM ioc_lists WHERE id=?`, id).Scan(&body)
	return body, err
}

// CreateIOCList creates a new empty named list and returns it. The name must be
// non-empty and unique (case-insensitive); a duplicate or blank name is an error.
func (c *Case) CreateIOCList(name string) (IOCListMeta, error) {
	name, err := c.checkIOCListName(name, 0)
	if err != nil {
		return IOCListMeta{}, err
	}
	res, err := c.db.Exec(`INSERT INTO ioc_lists(name,body) VALUES(?,'')`, name)
	if err != nil {
		return IOCListMeta{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return IOCListMeta{}, err
	}
	return IOCListMeta{ID: id, Name: name}, nil
}

// SetIOCListBody replaces the body text of a list.
func (c *Case) SetIOCListBody(id int64, body string) error {
	_, err := c.db.Exec(`UPDATE ioc_lists SET body=? WHERE id=?`, body, id)
	return err
}

// RenameIOCList changes a list's name (same non-empty/unique rule as CreateIOCList).
func (c *Case) RenameIOCList(id int64, name string) error {
	name, err := c.checkIOCListName(name, id)
	if err != nil {
		return err
	}
	_, err = c.db.Exec(`UPDATE ioc_lists SET name=? WHERE id=?`, name, id)
	return err
}

// DeleteIOCList removes a list.
func (c *Case) DeleteIOCList(id int64) error {
	_, err := c.db.Exec(`DELETE FROM ioc_lists WHERE id=?`, id)
	return err
}

// checkIOCListName trims name, rejects it if blank, and rejects it if it
// collides case-insensitively with another list's name (excludeID excludes
// the row being renamed; pass 0 when creating). It returns the trimmed name
// ready to store.
func (c *Case) checkIOCListName(name string, excludeID int64) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("ioc list name is required")
	}
	var exists int
	err := c.db.QueryRow(`SELECT 1 FROM ioc_lists WHERE name=? COLLATE NOCASE AND id<>?`,
		name, excludeID).Scan(&exists)
	if err == nil {
		return "", fmt.Errorf("an IOC list named %q already exists", name)
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	return name, nil
}

// Note is one investigator note attached to a timeline. Kind is NoteArtifact
// or NoteTime.
type Note struct {
	ID   int64
	Kind string
	Text string
	Done bool
}

// Note kinds.
const (
	NoteArtifact = "artifact"
	NoteTime     = "time"
)

// Notes returns a timeline's notes of the given kind, oldest first (insertion order).
func (c *Case) Notes(timelineID int64, kind string) ([]Note, error) {
	rows, err := c.db.Query(`SELECT id,kind,text,done FROM notes
		WHERE timeline_id=? AND kind=? ORDER BY id ASC`, timelineID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		var done int
		if err := rows.Scan(&n.ID, &n.Kind, &n.Text, &done); err != nil {
			return nil, err
		}
		n.Done = done != 0
		out = append(out, n)
	}
	return out, rows.Err()
}

// AddNote appends a note to a timeline and returns it (with its new ID). Text is
// trimmed; empty-after-trim is an error. kind must be NoteArtifact or NoteTime,
// else error.
func (c *Case) AddNote(timelineID int64, kind, text string) (Note, error) {
	if kind != NoteArtifact && kind != NoteTime {
		return Note{}, fmt.Errorf("invalid note kind %q", kind)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Note{}, fmt.Errorf("note text is required")
	}
	res, err := c.db.Exec(`INSERT INTO notes(timeline_id,kind,text,done) VALUES(?,?,?,0)`,
		timelineID, kind, text)
	if err != nil {
		return Note{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Note{}, err
	}
	return Note{ID: id, Kind: kind, Text: text, Done: false}, nil
}

// SetNoteDone sets a note's done flag.
func (c *Case) SetNoteDone(id int64, done bool) error {
	v := 0
	if done {
		v = 1
	}
	_, err := c.db.Exec(`UPDATE notes SET done=? WHERE id=?`, v, id)
	return err
}

// DeleteNote removes a note by id.
func (c *Case) DeleteNote(id int64) error {
	_, err := c.db.Exec(`DELETE FROM notes WHERE id=?`, id)
	return err
}

// Master returns every tagged row across all timelines, sorted chronologically
// (rows with a parseable timestamp first, ascending; rows without follow,
// grouped by timeline then row).
func (c *Case) Master() ([]MasterEntry, error) {
	names := map[int64]string{}
	display := map[int64][]string{}
	tls, err := c.Timelines()
	if err != nil {
		return nil, err
	}
	for _, t := range tls {
		names[t.ID] = t.Name
		display[t.ID] = t.displayHeadersOrRaw()
	}

	// Bulk-load tags and comments into maps first. With a single DB connection
	// we must never run a query while another cursor is still open, so each of
	// these fully drains and closes before the next.
	allTags, err := c.allTags()
	if err != nil {
		return nil, err
	}
	allComments, err := c.allComments()
	if err != nil {
		return nil, err
	}

	rows, err := c.db.Query(`SELECT timeline_id,row,time_unix,time_raw,summary,cells FROM tagged_snapshot`)
	if err != nil {
		return nil, err
	}
	var out []MasterEntry
	for rows.Next() {
		var e MasterEntry
		var tu sql.NullInt64
		var cellsJSON string
		if err := rows.Scan(&e.TimelineID, &e.Row, &tu, &e.TimeRaw, &e.Summary, &cellsJSON); err != nil {
			rows.Close()
			return nil, err
		}
		e.Timeline = names[e.TimelineID]
		e.DisplayHeaders = display[e.TimelineID]
		json.Unmarshal([]byte(cellsJSON), &e.Cells)
		if tu.Valid {
			e.Time = time.Unix(0, tu.Int64)
			e.HasTime = true
		}
		key := rowKey{e.TimelineID, e.Row}
		e.Tags = allTags[key]
		sort.Strings(e.Tags)
		e.Comment = allComments[key]
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.HasTime != b.HasTime {
			return a.HasTime // timed rows first
		}
		if a.HasTime {
			return a.Time.Before(b.Time)
		}
		if a.Timeline != b.Timeline {
			return a.Timeline < b.Timeline
		}
		return a.Row < b.Row
	})
	return out, nil
}

// rowKey identifies a row within a timeline for the bulk-loaded maps.
type rowKey struct {
	tl  int64
	row int
}

// allTags loads every (timeline,row)->tags mapping in one pass.
func (c *Case) allTags() (map[rowKey][]string, error) {
	rows, err := c.db.Query(`SELECT timeline_id,row,tag FROM tags`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[rowKey][]string{}
	for rows.Next() {
		var k rowKey
		var tag string
		if err := rows.Scan(&k.tl, &k.row, &tag); err != nil {
			return nil, err
		}
		out[k] = append(out[k], tag)
	}
	return out, rows.Err()
}

// allComments loads every (timeline,row)->comment mapping in one pass.
func (c *Case) allComments() (map[rowKey]string, error) {
	rows, err := c.db.Query(`SELECT timeline_id,row,comment FROM comments`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[rowKey]string{}
	for rows.Next() {
		var k rowKey
		var c string
		if err := rows.Scan(&k.tl, &k.row, &c); err != nil {
			return nil, err
		}
		out[k] = c
	}
	return out, rows.Err()
}

// --- helpers ---

func applyEdits(rec []string, edits map[int]string) []string {
	if len(edits) == 0 {
		return rec
	}
	out := append([]string(nil), rec...)
	for col, v := range edits {
		for col >= len(out) {
			out = append(out, "")
		}
		out[col] = v
	}
	return out
}

func extractTime(rec []string, timeCol int) (raw string, unixNano int64, ok bool) {
	if timeCol < 0 || timeCol >= len(rec) {
		return "", 0, false
	}
	raw = rec[timeCol]
	if t, parsed := model.ParseTime(raw); parsed {
		return raw, t.UnixNano(), true
	}
	return raw, 0, false
}

func nullableUnix(v int64, ok bool) interface{} {
	if !ok {
		return nil
	}
	return v
}

// summarize joins non-empty cells into a compact one-line summary, capped so a
// wide row does not bloat the database or the master grid.
func summarize(rec []string) string {
	const max = 400
	parts := make([]string, 0, len(rec))
	for _, c := range rec {
		c = strings.TrimSpace(c)
		if c != "" {
			parts = append(parts, c)
		}
	}
	s := strings.Join(parts, " | ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
