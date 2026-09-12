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
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"timeline-engine/internal/model"
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
	return nil
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
