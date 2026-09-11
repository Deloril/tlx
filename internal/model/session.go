package model

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strconv"
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

// Session holds everything the user layers on top of the immutable CSV: the
// active mode, per-row tags and comments, and per-cell edits. It implements
// Overlay. State persists to a sidecar JSON file next to the source, so the
// original CSV is never rewritten in place.
type Session struct {
	mu          sync.RWMutex
	sourcePath  string
	sidecarPath string
	mode        Mode

	tags      map[int][]string
	comments  map[int]string
	edits     map[int]map[int]string
	knownTags map[string]struct{}
	dirty     bool
}

// NewSession returns an empty session for sourcePath in read-only mode.
func NewSession(sourcePath string) *Session {
	return &Session{
		sourcePath:  sourcePath,
		sidecarPath: sourcePath + ".tlx.json",
		mode:        ReadOnly,
		tags:        map[int][]string{},
		comments:    map[int]string{},
		edits:       map[int]map[int]string{},
		knownTags:   map[string]struct{}{},
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

// AddTag applies a tag to a row. Allowed in Investigator and World-write.
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
	s.knownTags[tag] = struct{}{}
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

// KnownTags returns the sorted set of tags used so far, for autocompletion.
func (s *Session) KnownTags() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.knownTags))
	for t := range s.knownTags {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Persistence.

type sidecar struct {
	Source    string                       `json:"source"`
	Tags      map[string][]string          `json:"tags,omitempty"`
	Comments  map[string]string            `json:"comments,omitempty"`
	Edits     map[string]map[string]string `json:"edits,omitempty"`
	KnownTags []string                     `json:"known_tags,omitempty"`
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
	for k, v := range sc.Tags {
		if row, err := strconv.Atoi(k); err == nil {
			s.tags[row] = v
			for _, t := range v {
				s.knownTags[t] = struct{}{}
			}
		}
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
	for _, t := range sc.KnownTags {
		s.knownTags[t] = struct{}{}
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
	sc.KnownTags = make([]string, 0, len(s.knownTags))
	for t := range s.knownTags {
		sc.KnownTags = append(sc.KnownTags, t)
	}
	sort.Strings(sc.KnownTags)
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
