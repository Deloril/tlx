package model

import (
	"strings"
	"time"
)

// timeLayouts are the timestamp formats tried, in order, when parsing a cell.
// Forensic timelines come from many tools, so the list is deliberately broad.
var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"01/02/2006 15:04:05",
	"01/02/2006 3:04:05 PM",
	"02/01/2006 15:04:05",
	"1/2/2006 3:04:05 PM",
	"1/2/2006 15:04",
	"02-Jan-2006 15:04:05",
	"Jan 2, 2006 15:04:05",
	"Jan _2 15:04:05",
	"Mon Jan 2 15:04:05 2006",
	"2006/01/02 15:04:05",
	time.ANSIC,
	time.UnixDate,
}

// ParseTime tries to interpret s as a timestamp using the known layouts. It
// returns the parsed time and true on success. Surrounding whitespace and a
// trailing "UTC"/"Z" quirk are tolerated.
func ParseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// timeInterval is the half-open [start,end) span a time literal denotes. A
// literal naming a whole period (a year, a month, a day, a minute) spans that
// period; a fully specified timestamp, or one with an explicit ± duration, is
// second-granular. The before/after/between operators compare against it.
type timeInterval struct {
	start, end time.Time
}

// partialLayout pairs a timestamp layout with the unit it leaves unspecified, so
// a bare year/month/day/minute expands to the whole period rather than an instant.
type partialLayout struct {
	layout string
	unit   string // "year","month","day","minute"
}

var partialLayouts = []partialLayout{
	{"2006", "year"},
	{"2006-01", "month"},
	{"2006-01-02", "day"},
	{"2006/01/02", "day"},
	{"2006-01-02 15:04", "minute"},
	{"2006-01-02T15:04", "minute"},
	{"2006/01/02 15:04", "minute"},
}

// parseTimeBound parses a timestamp that may name a whole period, returning the
// [start,end) span it covers. A bare year/month/day/minute spans that unit;
// anything fuller is second-granular. ok is false if it cannot be parsed.
func parseTimeBound(s string) (timeInterval, bool) {
	s = unquote(strings.TrimSpace(s))
	if s == "" {
		return timeInterval{}, false
	}
	for _, pl := range partialLayouts {
		if t, err := time.Parse(pl.layout, s); err == nil {
			return spanFor(t, pl.unit), true
		}
	}
	if t, ok := ParseTime(s); ok {
		return timeInterval{start: t, end: t.Add(time.Second)}, true
	}
	return timeInterval{}, false
}

// spanFor returns the [start,end) interval for a time truncated to unit.
func spanFor(t time.Time, unit string) timeInterval {
	y, mo, d := t.Date()
	loc := t.Location()
	switch unit {
	case "year":
		start := time.Date(y, 1, 1, 0, 0, 0, 0, loc)
		return timeInterval{start, start.AddDate(1, 0, 0)}
	case "month":
		start := time.Date(y, mo, 1, 0, 0, 0, 0, loc)
		return timeInterval{start, start.AddDate(0, 1, 0)}
	case "day":
		start := time.Date(y, mo, d, 0, 0, 0, 0, loc)
		return timeInterval{start, start.AddDate(0, 0, 1)}
	case "minute":
		start := time.Date(y, mo, d, t.Hour(), t.Minute(), 0, 0, loc)
		return timeInterval{start, start.Add(time.Minute)}
	default:
		return timeInterval{t, t.Add(time.Second)}
	}
}

// parseTimeOperand parses an operand of a before/after/between comparison: a
// timestamp literal (or the keyword "now"/"time" meaning the current time),
// optionally followed by " + <dur>" or " - <dur>" to shift it. The arithmetic
// operator must be space-separated so it is not confused with a date's hyphens.
// now supplies the value of the "now"/"time" keyword and the base for relative
// arithmetic. A shifted operand collapses to a one-second instant.
func parseTimeOperand(s string, now time.Time) (timeInterval, bool) {
	s = strings.TrimSpace(s)
	// Find the last space-delimited +/- (the arithmetic operator). A date's own
	// hyphens have no surrounding spaces, so they are never matched here.
	sign, idx := 0, -1
	if i := strings.LastIndex(s, " + "); i > idx {
		sign, idx = 1, i
	}
	if i := strings.LastIndex(s, " - "); i > idx {
		sign, idx = -1, i
	}
	if idx >= 0 {
		lit := strings.TrimSpace(s[:idx])
		dur, ok := parseDuration(strings.TrimSpace(s[idx+3:]))
		if !ok {
			return timeInterval{}, false
		}
		base, ok := parseInstant(lit, now)
		if !ok {
			return timeInterval{}, false
		}
		if sign < 0 {
			base = base.Add(-dur)
		} else {
			base = base.Add(dur)
		}
		return timeInterval{start: base, end: base.Add(time.Second)}, true
	}
	if isNowKeyword(s) {
		return timeInterval{start: now, end: now.Add(time.Second)}, true
	}
	return parseTimeBound(s)
}

// parseInstant resolves a literal (or now/time keyword) to a single instant.
func parseInstant(s string, now time.Time) (time.Time, bool) {
	if isNowKeyword(s) {
		return now, true
	}
	iv, ok := parseTimeBound(s)
	return iv.start, ok
}

func isNowKeyword(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "now" || s == "time"
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// parseDuration parses durations like "90s", "5m", "2h", "7d", "1w" and
// combinations such as "1d12h". Units: s (second), m (minute), h (hour),
// d (day = 24h), w (week = 7d). ok is false on malformed input.
func parseDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	var total time.Duration
	i, n := 0, len(s)
	for i < n {
		j := i
		for j < n && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i || j >= n { // need digits then a unit letter
			return 0, false
		}
		num := int64(0)
		for k := i; k < j; k++ {
			num = num*10 + int64(s[k]-'0')
		}
		var unit time.Duration
		switch s[j] {
		case 's':
			unit = time.Second
		case 'm':
			unit = time.Minute
		case 'h':
			unit = time.Hour
		case 'd':
			unit = 24 * time.Hour
		case 'w':
			unit = 7 * 24 * time.Hour
		default:
			return 0, false
		}
		total += time.Duration(num) * unit
		i = j + 1
	}
	return total, true
}

// DetectTimeColumn samples up to sample rows and returns the index of the column
// whose values parse as timestamps most often (needing a clear majority), or -1
// if none qualifies. Header names containing common time words break ties and
// lower the bar slightly, so a "Timestamp" column wins over an incidental match.
func DetectTimeColumn(idx *Index) int {
	const sample = 200
	ncol := len(idx.Headers())
	if ncol == 0 {
		return -1
	}
	hits := make([]int, ncol)
	seen := make([]int, ncol)
	n := 0
	idx.Scan(func(i int, rec []string) bool {
		for c := 0; c < ncol && c < len(rec); c++ {
			if strings.TrimSpace(rec[c]) == "" {
				continue
			}
			seen[c]++
			if _, ok := ParseTime(rec[c]); ok {
				hits[c]++
			}
		}
		n++
		return n < sample
	})

	best, bestScore := -1, 0.0
	for c := 0; c < ncol; c++ {
		if seen[c] == 0 {
			continue
		}
		rate := float64(hits[c]) / float64(seen[c])
		threshold := 0.6
		if looksLikeTimeHeader(idx.Headers()[c]) {
			threshold = 0.3 // trust the header name, tolerate messier data
			rate += 0.15    // and give it an edge on ties
		}
		if rate >= threshold && rate > bestScore {
			best, bestScore = c, rate
		}
	}
	return best
}

func looksLikeTimeHeader(h string) bool {
	h = strings.ToLower(h)
	for _, w := range []string{"time", "date", "timestamp", "created", "modified", "accessed", "when"} {
		if strings.Contains(h, w) {
			return true
		}
	}
	return false
}
