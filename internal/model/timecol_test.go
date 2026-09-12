package model

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"90s", 90 * time.Second, true},
		{"5m", 5 * time.Minute, true},
		{"2h", 2 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"1w", 7 * 24 * time.Hour, true},
		{"1d12h", 36 * time.Hour, true},
		{"", 0, false},
		{"5", 0, false},  // number, no unit
		{"5x", 0, false}, // unknown unit
		{"d", 0, false},  // unit, no number
	}
	for _, c := range cases {
		got, ok := parseDuration(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseDuration(%q) = (%v,%v), want (%v,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseTimeBoundGranularity(t *testing.T) {
	cases := []struct {
		in         string
		start, end string // RFC3339 UTC
	}{
		{"2020", "2020-01-01T00:00:00Z", "2021-01-01T00:00:00Z"},
		{"2020-06", "2020-06-01T00:00:00Z", "2020-07-01T00:00:00Z"},
		{"2020-06-15", "2020-06-15T00:00:00Z", "2020-06-16T00:00:00Z"},
		{"2020-06-15 13:45", "2020-06-15T13:45:00Z", "2020-06-15T13:46:00Z"},
	}
	for _, c := range cases {
		iv, ok := parseTimeBound(c.in)
		if !ok {
			t.Errorf("parseTimeBound(%q) failed", c.in)
			continue
		}
		if iv.start.UTC().Format(time.RFC3339) != c.start || iv.end.UTC().Format(time.RFC3339) != c.end {
			t.Errorf("parseTimeBound(%q) = [%v,%v), want [%v,%v)", c.in,
				iv.start.UTC().Format(time.RFC3339), iv.end.UTC().Format(time.RFC3339), c.start, c.end)
		}
	}
	if _, ok := parseTimeBound("not a time"); ok {
		t.Errorf("parseTimeBound(garbage) should fail")
	}
}

func TestParseTimeOperandArithmetic(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in    string
		start string // expected interval start, RFC3339 UTC
	}{
		{"now", "2024-01-15T12:00:00Z"},
		{"time", "2024-01-15T12:00:00Z"},
		{"time - 7d", "2024-01-08T12:00:00Z"},
		{"now + 2h", "2024-01-15T14:00:00Z"},
		{"2020-01-01 + 1w", "2020-01-08T00:00:00Z"},
		{"2020-06-15 12:00:00 - 90m", "2020-06-15T10:30:00Z"},
	}
	for _, c := range cases {
		iv, ok := parseTimeOperand(c.in, now)
		if !ok {
			t.Errorf("parseTimeOperand(%q) failed", c.in)
			continue
		}
		if iv.start.UTC().Format(time.RFC3339) != c.start {
			t.Errorf("parseTimeOperand(%q) start = %v, want %v", c.in,
				iv.start.UTC().Format(time.RFC3339), c.start)
		}
	}
	if _, ok := parseTimeOperand("2020-01-01 + bogus", now); ok {
		t.Errorf("bad duration should fail")
	}
}
