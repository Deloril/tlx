package model

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Mode controls what a session permits.
type Mode int

const (
	// ReadOnly forbids every mutation.
	ReadOnly Mode = iota
	// Investigator permits tags and comments only; data columns stay locked.
	Investigator
	// WorldWrite permits editing any field in addition to tags and comments.
	WorldWrite
)

func (m Mode) String() string {
	switch m {
	case ReadOnly:
		return "Read-only"
	case Investigator:
		return "Investigator"
	case WorldWrite:
		return "World-write"
	default:
		return "unknown"
	}
}

var (
	// ErrReadOnly is returned when a mutation is attempted in read-only mode.
	ErrReadOnly = errors.New("read-only mode: change to Investigator or World-write to edit")
	// ErrColumnLocked is returned when a data column is edited in a mode that
	// only allows tag and comment changes.
	ErrColumnLocked = errors.New("investigator mode only permits editing tags and comments")
)

// TagDef is a tag and the colour it paints rows with. Colour is a "#RRGGBB"
// hex string; the GUI parses it. The model stays free of any GUI/colour deps.
type TagDef struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// defaultTagDefs are seeded into every session, in priority order (index 0 wins
// when a row carries more than one). Red for bad, yellow for suspicious, green
// for good.
var defaultTagDefs = []TagDef{
	{Name: "Bad", Color: "#E53935"},
	{Name: "Suspicious", Color: "#FDD835"},
	{Name: "Good", Color: "#43A047"},
}

// defaultTagColor is used for tags applied or loaded without an explicit colour.
const defaultTagColor = "#78909C"

// Session holds everything the user layers on top of the immutable CSV: the
// active mode, per-row tags and comments, and per-cell edits. It implements
// Overlay. State persists to a sidecar JSON file next to the source, so the
// original CSV is never rewritten in place.
type Session struct {
	mu          sync.RWMutex
	sourcePath  string
	sidecarPath string
	mode        Mode

	tags     map[int][]string
	comments map[int]string
	edits    map[int]map[int]string

	// Tag palette: an ordered list of definitions (index 0 = highest priority)
	// plus a name→index lookup. Seeded with the Bad/Suspicious/Good defaults.
	tagDefs  []TagDef
	tagIndex map[string]int

	dirty bool
}

// NewSession returns an empty session for sourcePath in read-only mode.
func NewSession(sourcePath string) *Session {
	s := &Session{
		sourcePath:  sourcePath,
		sidecarPath: sourcePath + ".tlx.json",
		mode:        ReadOnly,
		tags:        map[int][]string{},
		comments:    map[int]string{},
		edits:       map[int]map[int]string{},
		tagIndex:    map[string]int{},
	}
	s.seedDefaults()
	return s
}

// seedDefaults registers the built-in tags if they are not already present.
// Caller must hold the lock, or call before the session is shared.
func (s *Session) seedDefaults() {
	for _, d := range defaultTagDefs {
		s.defineLocked(d.Name, d.Color, false)
	}
}

// SidecarPath is where annotations are stored.
func (s *Session) SidecarPath() string { return s.sidecarPath }

// Mode returns the active mode.
func (s *Session) Mode() Mode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode
}

// SetMode changes the active mode. It never discards existing annotations.
func (s *Session) SetMode(m Mode) {
	s.mu.Lock()
	s.mode = m
	s.mu.Unlock()
}

// Dirty reports whether there are unsaved changes.
func (s *Session) Dirty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirty
}

// MarkSaved clears the dirty flag, e.g. after the annotations have been written
// out to a CSV. It does not itself persist anything.
func (s *Session) MarkSaved() {
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
}

// Overlay implementation.

// Tags returns a copy of the tags on a row.
func (s *Session) Tags(row int) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tags[row]
	if len(t) == 0 {
		return nil
	}
	out := make([]string, len(t))
	copy(out, t)
	return out
}

// Comment returns the comment on a row.
func (s *Session) Comment(row int) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.comments[row]
}

// CellOverride returns an edited value for a data cell, if one exists.
func (s *Session) CellOverride(row, col int) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cols, ok := s.edits[row]; ok {
		v, ok := cols[col]
		return v, ok
	}
	return "", false
}

// Mutations.

// AddTag applies a tag to a row. Allowed in Investigator and World-write. An
// unknown tag is registered in the palette with the default colour.
func (s *Session) AddTag(row int, tag string) error {
	if tag == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == ReadOnly {
		return ErrReadOnly
	}
	for _, t := range s.tags[row] {
		if t == tag {
			return nil // already present
		}
	}
	s.tags[row] = append(s.tags[row], tag)
	sort.Strings(s.tags[row])
	s.defineLocked(tag, defaultTagColor, false)
	s.dirty = true
	return nil
}

// RemoveTag removes a tag from a row.
func (s *Session) RemoveTag(row int, tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == ReadOnly {
		return ErrReadOnly
	}
	cur := s.tags[row]
	out := cur[:0]
	for _, t := range cur {
		if t != tag {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		delete(s.tags, row)
	} else {
		s.tags[row] = out
	}
	s.dirty = true
	return nil
}

// SetComment sets a row's comment. Allowed in Investigator and World-write.
func (s *Session) SetComment(row int, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == ReadOnly {
		return ErrReadOnly
	}
	if text == "" {
		delete(s.comments, row)
	} else {
		s.comments[row] = text
	}
	s.dirty = true
	return nil
}

// SetCell overrides a data cell's value. World-write only.
func (s *Session) SetCell(row, col int, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.mode {
	case ReadOnly:
		return ErrReadOnly
	case Investigator:
		return ErrColumnLocked
	}
	if s.edits[row] == nil {
		s.edits[row] = map[int]string{}
	}
	s.edits[row][col] = value
	s.dirty = true
	return nil
}

// ClearCell removes a cell edit, reverting to the original value.
func (s *Session) ClearCell(row, col int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.mode {
	case ReadOnly:
		return ErrReadOnly
	case Investigator:
		return ErrColumnLocked
	}
	if cols, ok := s.edits[row]; ok {
		delete(cols, col)
		if len(cols) == 0 {
			delete(s.edits, row)
		}
		s.dirty = true
	}
	return nil
}

// KnownTags returns the palette tag names in priority order, for reuse and
// autocompletion.
func (s *Session) KnownTags() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.tagDefs))
	for i, d := range s.tagDefs {
		out[i] = d.Name
	}
	return out
}

// TagDefs returns a copy of the tag palette in priority order.
func (s *Session) TagDefs() []TagDef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TagDef, len(s.tagDefs))
	copy(out, s.tagDefs)
	return out
}

// TagColor returns the palette colour for a tag, or ("", false) if unknown.
func (s *Session) TagColor(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i, ok := s.tagIndex[name]; ok {
		return s.tagDefs[i].Color, true
	}
	return "", false
}

// DefineTag registers a tag or updates its colour. Recolouring is presentation
// state, so it is allowed in any mode; it does set the dirty flag.
func (s *Session) DefineTag(name, color string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("tag name cannot be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defineLocked(name, color, true)
	s.dirty = true
	return nil
}

// defineLocked adds a tag to the palette, or updates its colour when override
// is true. Caller holds the lock.
func (s *Session) defineLocked(name, color string, override bool) {
	if color == "" {
		color = defaultTagColor
	}
	if i, ok := s.tagIndex[name]; ok {
		if override {
			s.tagDefs[i].Color = color
		}
		return
	}
	s.tagIndex[name] = len(s.tagDefs)
	s.tagDefs = append(s.tagDefs, TagDef{Name: name, Color: color})
}

// RowColor returns the highlight colour for a row: the colour of its
// highest-priority tag (lowest palette index). Returns ("", false) for an
// untagged row.
func (s *Session) RowColor(row int) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	best := -1
	for _, t := range s.tags[row] {
		if i, ok := s.tagIndex[t]; ok {
			if best < 0 || i < best {
				best = i
			}
		}
	}
	if best < 0 {
		return "", false
	}
	return s.tagDefs[best].Color, true
}

// SessionSnapshot is a plain-data copy of every annotation in a session: tags,
// comments, cell edits and the tag palette. It is what an external store (the
// case database) reads and writes, keeping the model free of any DB code.
type SessionSnapshot struct {
	Tags     map[int][]string
	Comments map[int]string
	Edits    map[int]map[int]string
	TagDefs  []TagDef
}

// Snapshot returns a deep copy of all annotation state.
func (s *Session) Snapshot() SessionSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := SessionSnapshot{
		Tags:     make(map[int][]string, len(s.tags)),
		Comments: make(map[int]string, len(s.comments)),
		Edits:    make(map[int]map[int]string, len(s.edits)),
		TagDefs:  make([]TagDef, len(s.tagDefs)),
	}
	for row, t := range s.tags {
		cp := make([]string, len(t))
		copy(cp, t)
		snap.Tags[row] = cp
	}
	for row, c := range s.comments {
		snap.Comments[row] = c
	}
	for row, cols := range s.edits {
		m := make(map[int]string, len(cols))
		for col, v := range cols {
			m[col] = v
		}
		snap.Edits[row] = m
	}
	copy(snap.TagDefs, s.tagDefs)
	return snap
}

// LoadSnapshot replaces all annotation state from a snapshot. It ignores the
// mode (loading is not a user edit) and leaves the session clean. The palette
// defaults remain seeded; snapshot definitions override their colours and set
// priority order.
func (s *Session) LoadSnapshot(snap SessionSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tags = map[int][]string{}
	s.comments = map[int]string{}
	s.edits = map[int]map[int]string{}
	for _, d := range snap.TagDefs {
		s.defineLocked(d.Name, d.Color, true)
	}
	for row, t := range snap.Tags {
		cp := make([]string, len(t))
		copy(cp, t)
		s.tags[row] = cp
		for _, tag := range t {
			s.defineLocked(tag, defaultTagColor, false)
		}
	}
	for row, c := range snap.Comments {
		if c != "" {
			s.comments[row] = c
		}
	}
	for row, cols := range snap.Edits {
		m := map[int]string{}
		for col, v := range cols {
			m[col] = v
		}
		if len(m) > 0 {
			s.edits[row] = m
		}
	}
	s.dirty = false
}

// Persistence.

type sidecar struct {
	Source   string                       `json:"source"`
	Tags     map[string][]string          `json:"tags,omitempty"`
	Comments map[string]string            `json:"comments,omitempty"`
	Edits    map[string]map[string]string `json:"edits,omitempty"`
	TagDefs  []TagDef                     `json:"tag_defs,omitempty"`
	// KnownTags is the legacy pre-colour format, still read for old sidecars.
	KnownTags []string `json:"known_tags,omitempty"`
}

// Load reads the sidecar file if it exists. A missing file is not an error.
func (s *Session) Load() error {
	data, err := os.ReadFile(s.sidecarPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var sc sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Stored palette overrides the seeded default colours and defines priority
	// order; the defaults remain present because they were seeded in NewSession.
	for _, d := range sc.TagDefs {
		s.defineLocked(d.Name, d.Color, true)
	}
	for k, v := range sc.Tags {
		if row, err := strconv.Atoi(k); err == nil {
			s.tags[row] = v
			for _, t := range v {
				s.defineLocked(t, defaultTagColor, false)
			}
		}
	}
	for _, t := range sc.KnownTags { // legacy pre-colour sidecars
		s.defineLocked(t, defaultTagColor, false)
	}
	for k, v := range sc.Comments {
		if row, err := strconv.Atoi(k); err == nil {
			s.comments[row] = v
		}
	}
	for k, cols := range sc.Edits {
		row, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		m := map[int]string{}
		for ck, cv := range cols {
			if col, err := strconv.Atoi(ck); err == nil {
				m[col] = cv
			}
		}
		if len(m) > 0 {
			s.edits[row] = m
		}
	}
	s.dirty = false
	return nil
}

// Save writes the sidecar file atomically (temp file then rename).
func (s *Session) Save() error {
	s.mu.Lock()
	sc := sidecar{Source: s.sourcePath}
	if len(s.tags) > 0 {
		sc.Tags = map[string][]string{}
		for row, t := range s.tags {
			sc.Tags[strconv.Itoa(row)] = t
		}
	}
	if len(s.comments) > 0 {
		sc.Comments = map[string]string{}
		for row, c := range s.comments {
			sc.Comments[strconv.Itoa(row)] = c
		}
	}
	if len(s.edits) > 0 {
		sc.Edits = map[string]map[string]string{}
		for row, cols := range s.edits {
			m := map[string]string{}
			for col, v := range cols {
				m[strconv.Itoa(col)] = v
			}
			sc.Edits[strconv.Itoa(row)] = m
		}
	}
	if len(s.tagDefs) > 0 {
		sc.TagDefs = make([]TagDef, len(s.tagDefs))
		copy(sc.TagDefs, s.tagDefs)
	}
	s.mu.Unlock()

	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.sidecarPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.sidecarPath); err != nil {
		return err
	}
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
	return nil
}
