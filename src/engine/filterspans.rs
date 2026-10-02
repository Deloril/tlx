//! Port of internal/model/filterspans.go: classifies a filter query into
//! coloured spans for the filter bar's syntax highlighting. Follows the same
//! grammar as query.rs's parser, but leniently: it never fails, so a
//! half-typed query still colours sensibly.

use super::query::{read_atom_raw, skip_spaces, AtomKind};

/// FilterSpanKind is the role a run of the query plays, for colouring.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum FilterSpanKind {
    /// Whitespace, parentheses or anything unclassified.
    Plain,
    /// A column name on the left of = / != or a time keyword.
    Field,
    /// An operator or keyword: = != AND OR NOT before after between.
    Cond,
    /// A value: a bareword, "quoted" string or /regex/.
    Param,
}

/// FilterSpan is a byte range [start,end) of the query and the role it plays.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct FilterSpan {
    pub start: usize,
    pub end: usize,
    pub kind: FilterSpanKind,
}

/// FilterSpans tiles s with spans (no gaps, so concatenating the spans' text
/// reproduces s) tagged by role, for the filter bar's colouring. A word
/// directly before = / != or a time keyword is a field; the comparison
/// operators and the boolean/time keywords are conditions; every other value
/// is a parameter; whitespace and parentheses are plain.
pub fn filter_spans(s: &str) -> Vec<FilterSpan> {
    let mut spans: Vec<FilterSpan> = Vec::new();
    let mut prev = 0usize;

    let flush_plain = |spans: &mut Vec<FilterSpan>, prev: usize, upto: usize| {
        if upto > prev {
            spans.push(FilterSpan { start: prev, end: upto, kind: FilterSpanKind::Plain });
        }
    };

    let n = s.len();
    let b = s.as_bytes();
    let mut i = 0usize;
    while i < n {
        match b[i] {
            b' ' | b'\t' | b'\n' | b'\r' | b'(' | b')' => {
                i += 1;
                continue;
            }
            _ => {}
        }

        // A comparison operator standing on its own.
        if b[i] == b'=' || (b[i] == b'!' && i + 1 < n && b[i + 1] == b'=') {
            let end = if b[i] == b'!' { i + 2 } else { i + 1 };
            flush_plain(&mut spans, prev, i);
            spans.push(FilterSpan { start: i, end, kind: FilterSpanKind::Cond });
            prev = end;
            i = end;
            continue;
        }

        let (val, kind, next, ok) = read_atom_raw(s, i);
        if kind == AtomKind::None {
            i += 1; // nothing readable here; advance so we always make progress
            continue;
        }
        let start = i;
        if !ok {
            // Unterminated "quote or /regex/: colour the rest as a value.
            flush_plain(&mut spans, prev, start);
            spans.push(FilterSpan { start, end: next, kind: FilterSpanKind::Param });
            prev = next;
            i = next;
            continue;
        }
        i = next;

        if kind == AtomKind::Bare {
            match val.to_uppercase().as_str() {
                "AND" | "OR" | "NOT" => {
                    flush_plain(&mut spans, prev, start);
                    spans.push(FilterSpan { start, end: i, kind: FilterSpanKind::Cond });
                    prev = i;
                    continue;
                }
                _ => {}
            }
            match val.to_lowercase().as_str() {
                "before" | "after" | "between" => {
                    flush_plain(&mut spans, prev, start);
                    spans.push(FilterSpan { start, end: i, kind: FilterSpanKind::Cond });
                    prev = i;
                    continue;
                }
                _ => {}
            }
        }

        // A word (bare or "quoted") immediately followed by = / != is a field.
        if kind == AtomKind::Bare || kind == AtomKind::Quoted {
            let j = skip_spaces(s, i);
            if j < n && (b[j] == b'=' || (b[j] == b'!' && j + 1 < n && b[j + 1] == b'=')) {
                flush_plain(&mut spans, prev, start);
                spans.push(FilterSpan { start, end: i, kind: FilterSpanKind::Field });
                prev = i;
                continue;
            }
            // A word followed by a time keyword is also a field.
            let (kw, kwkind, _, kwok) = read_atom_raw(s, j);
            if kwok && kwkind == AtomKind::Bare {
                match kw.to_lowercase().as_str() {
                    "before" | "after" | "between" => {
                        flush_plain(&mut spans, prev, start);
                        spans.push(FilterSpan { start, end: i, kind: FilterSpanKind::Field });
                        prev = i;
                        continue;
                    }
                    _ => {}
                }
            }
        }

        // Anything else is a value.
        flush_plain(&mut spans, prev, start);
        spans.push(FilterSpan { start, end: i, kind: FilterSpanKind::Param });
        prev = i;
    }
    flush_plain(&mut spans, prev, n);
    spans
}

#[cfg(test)]
mod tests {
    use super::*;

    fn kind_string(k: FilterSpanKind) -> &'static str {
        match k {
            FilterSpanKind::Field => "field",
            FilterSpanKind::Cond => "cond",
            FilterSpanKind::Param => "param",
            FilterSpanKind::Plain => "plain",
        }
    }

    /// non_plain returns "text:kind" for each non-plain span, so tests can
    /// assert the interesting classifications without spelling out every
    /// whitespace gap.
    fn non_plain(s: &str) -> Vec<String> {
        filter_spans(s)
            .into_iter()
            .filter(|sp| sp.kind != FilterSpanKind::Plain)
            .map(|sp| format!("{}:{}", &s[sp.start..sp.end], kind_string(sp.kind)))
            .collect()
    }

    #[test]
    fn test_filter_spans_tiling() {
        for s in [
            "",
            "   ",
            "Summary=svchost AND (tag=bad OR tag=sus)",
            r"Host=/^dc-\d+$/ AND NOT tag=benign",
            "Time between 2021-01-01 and 2021-02-01",
            r#""Process Name"=cmd.exe"#,
            r#"unterminated "quote"#,
            "bad /regex",
        ] {
            let spans = filter_spans(s);
            let mut pos = 0usize;
            let mut rebuilt = String::new();
            for sp in &spans {
                assert_eq!(sp.start, pos, "{s:?}: gap/overlap at {pos}, span starts {}", sp.start);
                assert!(sp.end >= sp.start && sp.end <= s.len(), "{s:?}: span {sp:?} out of range");
                rebuilt.push_str(&s[sp.start..sp.end]);
                pos = sp.end;
            }
            assert_eq!(pos, s.len(), "{s:?}: spans end at {pos}, want {}", s.len());
            assert_eq!(rebuilt, s, "{s:?}: rebuilt {rebuilt:?}");
        }
    }

    #[test]
    fn test_filter_spans_classification() {
        let cases: Vec<(&str, Vec<&str>)> = vec![
            ("Summary=svchost", vec!["Summary:field", "=:cond", "svchost:param"]),
            (
                "tag=bad OR tag=sus",
                vec!["tag:field", "=:cond", "bad:param", "OR:cond", "tag:field", "=:cond", "sus:param"],
            ),
            ("NOT tag=x", vec!["NOT:cond", "tag:field", "=:cond", "x:param"]),
            ("host!=dc1", vec!["host:field", "!=:cond", "dc1:param"]),
            (
                "Time between a and b",
                vec!["Time:field", "between:cond", "a:param", "and:cond", "b:param"],
            ),
            ("Time before 2021-01-01", vec!["Time:field", "before:cond", "2021-01-01:param"]),
            (r#""Process Name"=cmd"#, vec![r#""Process Name":field"#, "=:cond", "cmd:param"]),
            ("psexec", vec!["psexec:param"]), // a bare value matches any column
            ("/regex/", vec!["/regex/:param"]),
        ];
        for (input, want) in cases {
            let got = non_plain(input);
            assert_eq!(got, want, "{input:?}");
        }
    }
}
