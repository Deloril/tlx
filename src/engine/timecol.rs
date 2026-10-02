//! Port of internal/model/timecol.go: timestamp parsing for the query
//! language's before/after/between operators and for auto-detecting a
//! timeline's time column.
//!
//! Go's `time.Parse` matches a fixed reference-time layout against the
//! input; chrono has no direct equivalent, so each Go layout below is
//! re-expressed as a chrono strftime-style format string (or, for the RFC3339
//! variants, chrono's built-in RFC3339 parser).

use chrono::{DateTime, Datelike, Duration, FixedOffset, NaiveDate, NaiveDateTime, TimeZone, Timelike, Utc};

/// Our working timestamp type. The Go original uses time.Time, which carries
/// a location; forensic timelines are overwhelmingly either UTC or carry an
/// explicit offset, so we normalise everything to UTC-with-fixed-offset via
/// chrono's DateTime<Utc> and treat a timestamp with no offset as UTC. This
/// matches ParseTime's practical behaviour closely enough for before/after/
/// between comparisons (which only compare relative order).
pub type Instant = DateTime<Utc>;

/// Non-RFC3339 layouts, as chrono strftime format strings, tried in order.
/// Mirrors Go's timeLayouts (minus the two RFC3339 variants, handled
/// separately since chrono parses those with a dedicated parser that also
/// accepts a numeric offset).
const LAYOUTS: &[&str] = &[
    "%Y-%m-%dT%H:%M:%S",
    "%Y-%m-%d %H:%M:%S%.f %z %Z",
    "%Y-%m-%d %H:%M:%S%.f%z",
    "%Y-%m-%d %H:%M:%S%.f",
    "%Y-%m-%d %H:%M:%S",
    "%Y-%m-%d %H:%M",
    "%Y-%m-%d",
    "%m/%d/%Y %H:%M:%S",
    "%m/%d/%Y %I:%M:%S %p",
    "%d/%m/%Y %H:%M:%S",
    "%m/%d/%Y %H:%M",
    "%d-%b-%Y %H:%M:%S",
    "%b %d, %Y %H:%M:%S",
    "%b %e %H:%M:%S",
    "%a %b %d %H:%M:%S %Y",
    "%Y/%m/%d %H:%M:%S",
    "%a %b %e %H:%M:%S %Y",     // ANSIC
    "%a %b %e %H:%M:%S %Z %Y",  // UnixDate
];

/// ParseTime tries to interpret s as a timestamp using the known layouts. It
/// returns the parsed instant and true on success. Surrounding whitespace is
/// tolerated.
pub fn parse_time(s: &str) -> Option<Instant> {
    let s = s.trim();
    if s.is_empty() {
        return None;
    }
    // RFC3339 / RFC3339Nano, with an explicit offset.
    if let Ok(dt) = DateTime::parse_from_rfc3339(s) {
        return Some(dt.with_timezone(&Utc));
    }
    for layout in LAYOUTS {
        if let Ok(dt) = DateTime::parse_from_str(s, layout) {
            return Some(dt.with_timezone(&Utc));
        }
        if let Ok(ndt) = NaiveDateTime::parse_from_str(s, layout) {
            return Some(Utc.from_utc_datetime(&ndt));
        }
        if let Ok(nd) = NaiveDate::parse_from_str(s, layout) {
            return Some(Utc.from_utc_datetime(&nd.and_hms_opt(0, 0, 0).unwrap()));
        }
    }
    None
}

/// timeInterval is the half-open [start,end) span a time literal denotes. A
/// literal naming a whole period (a year, a month, a day, a minute) spans
/// that period; a fully specified timestamp, or one with an explicit +/-
/// duration, is second-granular.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct TimeInterval {
    pub start: Instant,
    pub end: Instant,
}

struct PartialLayout {
    layout: &'static str,
    unit: Unit,
}

#[derive(Clone, Copy)]
enum Unit {
    Year,
    Month,
    Day,
    Minute,
}

const PARTIAL_LAYOUTS: &[PartialLayout] = &[
    PartialLayout { layout: "%Y", unit: Unit::Year },
    PartialLayout { layout: "%Y-%m", unit: Unit::Month },
    PartialLayout { layout: "%Y-%m-%d", unit: Unit::Day },
    PartialLayout { layout: "%Y/%m/%d", unit: Unit::Day },
    PartialLayout { layout: "%Y-%m-%d %H:%M", unit: Unit::Minute },
    PartialLayout { layout: "%Y-%m-%dT%H:%M", unit: Unit::Minute },
    PartialLayout { layout: "%Y/%m/%d %H:%M", unit: Unit::Minute },
];

/// parseTimeBound parses a timestamp that may name a whole period, returning
/// the [start,end) span it covers. A bare year/month/day/minute spans that
/// unit; anything fuller is second-granular. None if it cannot be parsed.
pub fn parse_time_bound(s: &str) -> Option<TimeInterval> {
    let s = unquote(s.trim());
    if s.is_empty() {
        return None;
    }
    for pl in PARTIAL_LAYOUTS {
        // A bare "%Y" etc. must consume the whole string: chrono's
        // parse_from_str already requires an exact match for these formats
        // (no trailing data), same as Go's time.Parse.
        if let Some(t) = try_partial(&s, pl.layout) {
            return Some(span_for(t, pl.unit));
        }
    }
    parse_time(&s).map(|t| TimeInterval { start: t, end: t + Duration::seconds(1) })
}

fn try_partial(s: &str, layout: &str) -> Option<Instant> {
    // Year-only and year-month layouts need a date to anchor day 1; try the
    // natural chrono parsers that correspond to each layout's granularity.
    match layout {
        "%Y" => {
            let y: i32 = s.parse().ok()?;
            Some(Utc.with_ymd_and_hms(y, 1, 1, 0, 0, 0).single()?)
        }
        "%Y-%m" => {
            let parts: Vec<&str> = s.split('-').collect();
            if parts.len() != 2 {
                return None;
            }
            let y: i32 = parts[0].parse().ok()?;
            let m: u32 = parts[1].parse().ok()?;
            Some(Utc.with_ymd_and_hms(y, m, 1, 0, 0, 0).single()?)
        }
        _ => {
            // Try NaiveDateTime first: for a layout that includes time fields
            // (e.g. "%Y-%m-%d %H:%M"), NaiveDate::parse_from_str happily
            // matches too (chrono lets a date-only parse consume extra
            // trailing format specifiers it doesn't need) and silently drops
            // the time-of-day, which produced wrong results for minute-
            // granular bounds like "2020-06-15 13:45". Date-only layouts
            // (e.g. "%Y-%m-%d") simply fail the NaiveDateTime parse and fall
            // through to the NaiveDate attempt below.
            if let Ok(ndt) = NaiveDateTime::parse_from_str(s, layout) {
                return Some(Utc.from_utc_datetime(&ndt));
            }
            if let Ok(nd) = NaiveDate::parse_from_str(s, layout) {
                return Some(Utc.from_utc_datetime(&nd.and_hms_opt(0, 0, 0).unwrap()));
            }
            None
        }
    }
}

/// spanFor returns the [start,end) interval for a time truncated to unit.
fn span_for(t: Instant, unit: Unit) -> TimeInterval {
    let y = t.year();
    let m = t.month();
    let d = t.day();
    match unit {
        Unit::Year => {
            let start = Utc.with_ymd_and_hms(y, 1, 1, 0, 0, 0).single().unwrap();
            let end = Utc.with_ymd_and_hms(y + 1, 1, 1, 0, 0, 0).single().unwrap();
            TimeInterval { start, end }
        }
        Unit::Month => {
            let start = Utc.with_ymd_and_hms(y, m, 1, 0, 0, 0).single().unwrap();
            let (ny, nm) = if m == 12 { (y + 1, 1) } else { (y, m + 1) };
            let end = Utc.with_ymd_and_hms(ny, nm, 1, 0, 0, 0).single().unwrap();
            TimeInterval { start, end }
        }
        Unit::Day => {
            let start = Utc.with_ymd_and_hms(y, m, d, 0, 0, 0).single().unwrap();
            let end = start + Duration::days(1);
            TimeInterval { start, end }
        }
        Unit::Minute => {
            let start = Utc
                .with_ymd_and_hms(y, m, d, t.hour(), t.minute(), 0)
                .single()
                .unwrap();
            let end = start + Duration::minutes(1);
            TimeInterval { start, end }
        }
    }
}

/// parseTimeOperand parses an operand of a before/after/between comparison: a
/// timestamp literal (or the keyword "now"/"time" meaning the current time),
/// optionally followed by " + <dur>" or " - <dur>" to shift it. The
/// arithmetic operator must be space-separated so it is not confused with a
/// date's hyphens. now supplies the value of the now/time keyword and the
/// base for relative arithmetic. A shifted operand collapses to a
/// one-second instant.
pub fn parse_time_operand(s: &str, now: Instant) -> Option<TimeInterval> {
    let s = s.trim();
    // Find the last space-delimited +/- (the arithmetic operator). A date's
    // own hyphens have no surrounding spaces, so they are never matched here.
    let mut sign = 0i32;
    let mut idx: isize = -1;
    if let Some(i) = s.rfind(" + ") {
        sign = 1;
        idx = i as isize;
    }
    if let Some(i) = s.rfind(" - ") {
        if i as isize > idx {
            sign = -1;
            idx = i as isize;
        }
    }
    if idx >= 0 {
        let idx = idx as usize;
        let lit = s[..idx].trim();
        let dur_str = s[idx + 3..].trim();
        let dur = parse_duration(dur_str)?;
        let mut base = parse_instant(lit, now)?;
        base = if sign < 0 { base - dur } else { base + dur };
        return Some(TimeInterval { start: base, end: base + Duration::seconds(1) });
    }
    if is_now_keyword(s) {
        return Some(TimeInterval { start: now, end: now + Duration::seconds(1) });
    }
    parse_time_bound(s)
}

/// parseInstant resolves a literal (or now/time keyword) to a single instant.
fn parse_instant(s: &str, now: Instant) -> Option<Instant> {
    if is_now_keyword(s) {
        return Some(now);
    }
    parse_time_bound(s).map(|iv| iv.start)
}

fn is_now_keyword(s: &str) -> bool {
    let s = s.trim().to_lowercase();
    s == "now" || s == "time"
}

fn unquote(s: &str) -> String {
    if s.len() >= 2 && s.starts_with('"') && s.ends_with('"') {
        s[1..s.len() - 1].to_string()
    } else {
        s.to_string()
    }
}

/// parseDuration parses durations like "90s", "5m", "2h", "7d", "1w" and
/// combinations such as "1d12h". Units: s (second), m (minute), h (hour),
/// d (day = 24h), w (week = 7d). None on malformed input.
pub fn parse_duration(s: &str) -> Option<Duration> {
    let s = s.trim();
    if s.is_empty() {
        return None;
    }
    let bytes = s.as_bytes();
    let mut total = Duration::zero();
    let mut i = 0usize;
    let n = bytes.len();
    while i < n {
        let start = i;
        while i < n && bytes[i].is_ascii_digit() {
            i += 1;
        }
        if i == start || i >= n {
            return None; // need digits then a unit letter
        }
        let num: i64 = s[start..i].parse().ok()?;
        let unit = match bytes[i] {
            b's' => Duration::seconds(1),
            b'm' => Duration::minutes(1),
            b'h' => Duration::hours(1),
            b'd' => Duration::hours(24),
            b'w' => Duration::hours(24 * 7),
            _ => return None,
        };
        total = total + unit * (num as i32);
        i += 1;
    }
    Some(total)
}

/// DetectTimeColumn samples up to `sample` rows and returns the index of the
/// column whose values parse as timestamps most often (needing a clear
/// majority), or None if no column qualifies. Header names containing
/// common time words break ties and lower the bar slightly, so a
/// "Timestamp" column wins over an incidental match.
pub fn detect_time_column(idx: &super::index::Index) -> Option<usize> {
    const SAMPLE: usize = 200;
    let ncol = idx.headers().len();
    if ncol == 0 {
        return None;
    }
    let mut hits = vec![0usize; ncol];
    let mut seen = vec![0usize; ncol];
    let mut n = 0usize;
    let _ = idx.scan(|_i, rec| {
        for c in 0..ncol.min(rec.len()) {
            if rec[c].trim().is_empty() {
                continue;
            }
            seen[c] += 1;
            if parse_time(&rec[c]).is_some() {
                hits[c] += 1;
            }
        }
        n += 1;
        n < SAMPLE
    });

    let mut best: Option<usize> = None;
    let mut best_score = 0.0f64;
    for c in 0..ncol {
        if seen[c] == 0 {
            continue;
        }
        let mut rate = hits[c] as f64 / seen[c] as f64;
        let mut threshold = 0.6;
        if looks_like_time_header(&idx.headers()[c]) {
            threshold = 0.3;
            rate += 0.15;
        }
        if rate >= threshold && rate > best_score {
            best = Some(c);
            best_score = rate;
        }
    }
    best
}

fn looks_like_time_header(h: &str) -> bool {
    let h = h.to_lowercase();
    ["time", "date", "timestamp", "created", "modified", "accessed", "when"]
        .iter()
        .any(|w| h.contains(w))
}

#[allow(dead_code)]
pub fn fixed_offset_now() -> DateTime<FixedOffset> {
    Utc::now().with_timezone(&FixedOffset::east_opt(0).unwrap())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parse_duration() {
        let cases: Vec<(&str, Option<Duration>)> = vec![
            ("90s", Some(Duration::seconds(90))),
            ("5m", Some(Duration::minutes(5))),
            ("2h", Some(Duration::hours(2))),
            ("7d", Some(Duration::hours(7 * 24))),
            ("1w", Some(Duration::hours(7 * 24))),
            ("1d12h", Some(Duration::hours(36))),
            ("", None),
            ("5", None),
            ("5x", None),
            ("d", None),
        ];
        for (input, want) in cases {
            let got = parse_duration(input);
            assert_eq!(got, want, "parse_duration({input:?})");
        }
    }

    #[test]
    fn test_parse_time_bound_granularity() {
        let cases = [
            ("2020", "2020-01-01T00:00:00+00:00", "2021-01-01T00:00:00+00:00"),
            ("2020-06", "2020-06-01T00:00:00+00:00", "2020-07-01T00:00:00+00:00"),
            ("2020-06-15", "2020-06-15T00:00:00+00:00", "2020-06-16T00:00:00+00:00"),
            (
                "2020-06-15 13:45",
                "2020-06-15T13:45:00+00:00",
                "2020-06-15T13:46:00+00:00",
            ),
        ];
        for (input, start, end) in cases {
            let iv = parse_time_bound(input).unwrap_or_else(|| panic!("parse_time_bound({input:?}) failed"));
            assert_eq!(iv.start.to_rfc3339(), start, "start for {input:?}");
            assert_eq!(iv.end.to_rfc3339(), end, "end for {input:?}");
        }
        assert!(parse_time_bound("not a time").is_none());
    }

    #[test]
    fn test_parse_time_operand_arithmetic() {
        let now = Utc.with_ymd_and_hms(2024, 1, 15, 12, 0, 0).unwrap();
        let cases = [
            ("now", "2024-01-15T12:00:00+00:00"),
            ("time", "2024-01-15T12:00:00+00:00"),
            ("time - 7d", "2024-01-08T12:00:00+00:00"),
            ("now + 2h", "2024-01-15T14:00:00+00:00"),
            ("2020-01-01 + 1w", "2020-01-08T00:00:00+00:00"),
            ("2020-06-15 12:00:00 - 90m", "2020-06-15T10:30:00+00:00"),
        ];
        for (input, start) in cases {
            let iv = parse_time_operand(input, now).unwrap_or_else(|| panic!("parse_time_operand({input:?}) failed"));
            assert_eq!(iv.start.to_rfc3339(), start, "start for {input:?}");
        }
        assert!(parse_time_operand("2020-01-01 + bogus", now).is_none());
    }
}
