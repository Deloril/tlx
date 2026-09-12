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
