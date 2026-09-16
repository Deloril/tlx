package model

import (
	"errors"
	"strings"
	"time"
)

// This file implements IOC (indicator of compromise) matching. An IOC list is
// plain text, one indicator per line, matched against every row of a timeline.
// Rows that hit at least one indicator are surfaced to the caller to tag.
//
// Line forms:
//
//	evil.exe            plain substring, matched against every data column
//	/T[0-9]{4}/         a /regex/, matched against every data column
//	`Summary=psexec AND Timestamp between 2024 and 2025
//	                    a leading backtick makes the rest a full filter query
//	                    (see query.go), including before/after/between on
//	                    timestamp columns
//
// The plain and /regex/ forms skip the Tags and Comment fields, so an indicator
// never matches a row on its own annotations. A backtick line reaches them only
// when it names the field, e.g. `tag=beacon or `comment=/psexec/.
//
// Blank lines and lines beginning with # are ignored. Matching is
// case-insensitive unless the caller asks otherwise.

// IOCError records a line that failed to compile, so the UI can report which
// indicators were skipped without aborting the whole run.
type IOCError struct {
	Line int    // 1-based line number within the list
	Text string // the offending line, trimmed
	Err  string // why it failed
}

// IOCSet is a compiled IOC list: one predicate per usable line, plus the errors
// for any lines that did not compile.
type IOCSet struct {
	preds  []rowPred
	Errors []IOCError
}

// Count is the number of usable indicators compiled.
func (s *IOCSet) Count() int {
	if s == nil {
		return 0
	}
	return len(s.preds)
}

// CompileIOCs compiles an IOC list against this view's columns. Field names in
// backtick filter lines resolve against the view's headers, so compile against a
// view of the timeline being scanned.
func (v *View) CompileIOCs(text string, cased bool) *IOCSet {
	set := &IOCSet{}
	now := time.Now()
	for n, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pred, err := v.compileIOCLine(line, cased, now)
		if err != nil {
			set.Errors = append(set.Errors, IOCError{Line: n + 1, Text: line, Err: err.Error()})
			continue
		}
		set.preds = append(set.preds, pred)
	}
	return set
}

func (v *View) compileIOCLine(line string, cased bool, now time.Time) (rowPred, error) {
	if strings.HasPrefix(line, "`") {
		expr := strings.TrimSpace(line[1:])
		ast, err := ParseQuery(expr)
		if err != nil {
			return nil, err
		}
		if ast == nil {
			return nil, errors.New("empty filter")
		}
		return v.compileExpr(ast, cased, now)
	}
	// A plain string or /regex/ matched against every data column. The Tags and
	// Comment fields are skipped so an indicator does not hit a row on its own
	// annotations; a backtick filter line naming tag=/comment= reaches them.
	val, useRegexp := line, false
	if len(line) >= 2 && line[0] == '/' && line[len(line)-1] == '/' {
		val, useRegexp = line[1:len(line)-1], true
	}
	m, err := compileMatcher(val, useRegexp, cased)
	if err != nil {
		return nil, err
	}
	return func(row int, rec []string) bool {
		return v.columnMatch(row, rec, ColData, m)
	}, nil
}

// ScanIOCs runs the compiled indicators over every row of the underlying index
// and returns the master indices of rows that hit at least one. It scans the
// full row set, not the current filtered view, so a filter in place does not
// hide matches.
func (v *View) ScanIOCs(set *IOCSet) ([]int, error) {
	if set == nil || len(set.preds) == 0 {
		return nil, nil
	}
	var hits []int
	err := v.idx.Scan(func(i int, rec []string) bool {
		for _, p := range set.preds {
			if p(i, rec) {
				hits = append(hits, i)
				break
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return hits, nil
}
