//! Pull candidate artifact strings out of a cell for the right-click "add to
//! Artifacts/Times" menu.
//!
//! The goal is one click: a right-click lists the whole cell, every indicator
//! we can recognise in it (IPs, hashes, domains, paths, GUIDs, emails), and
//! then the leftover whitespace tokens as a fallback. Most forensic artifacts
//! are a single token, so the recognised ones usually sit right under the
//! cursor and the user never opens a sub-window.
//!
//! Order is detected-IOCs first (most specific), then plain tokens, then the
//! whole cell last. Duplicates are dropped keeping first occurrence, so a token
//! that already appeared as a detected IOC is not repeated.

use std::sync::OnceLock;

use regex::Regex;

/// A candidate string to add, with a short label naming why it was surfaced
/// ("IPv4", "SHA-256", "token", "whole cell"). The label drives nothing but the
/// menu text.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Candidate {
    pub kind: &'static str,
    pub text: String,
}

struct Detector {
    kind: &'static str,
    re: Regex,
}

fn detectors() -> &'static [Detector] {
    static D: OnceLock<Vec<Detector>> = OnceLock::new();
    D.get_or_init(|| {
        // Order matters: more specific patterns first so e.g. a SHA-256 isn't
        // also reported as a shorter hex run. Each pattern matches the whole
        // indicator, anchored on word-ish boundaries by the surrounding scan.
        let defs: &[(&str, &str)] = &[
            // IPv4 with optional :port.
            ("IPv4", r"\b(?:\d{1,3}\.){3}\d{1,3}(?::\d{1,5})?\b"),
            // IPv6 (compressed or full), kept simple: hex groups with ::.
            ("IPv6", r"\b(?:[0-9A-Fa-f]{1,4}:){2,7}[0-9A-Fa-f]{0,4}\b"),
            ("SHA-256", r"\b[0-9A-Fa-f]{64}\b"),
            ("SHA-1", r"\b[0-9A-Fa-f]{40}\b"),
            ("MD5", r"\b[0-9A-Fa-f]{32}\b"),
            ("GUID", r"\b[0-9A-Fa-f]{8}-(?:[0-9A-Fa-f]{4}-){3}[0-9A-Fa-f]{12}\b"),
            ("email", r"\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b"),
            // Windows path: drive-letter or UNC, through the last segment.
            ("path", r"(?:[A-Za-z]:\\|\\\\)[^\s,;|]+"),
            // Unix path: at least two segments so a lone "/" or date isn't a hit.
            ("path", r"/[^\s,;|/]+(?:/[^\s,;|]+)+"),
            // Domain / host: labels then a TLD. Comes after email/path so those
            // win first.
            ("domain", r"\b(?:[A-Za-z0-9](?:[A-Za-z0-9\-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,}\b"),
        ];
        defs.iter()
            .map(|(kind, pat)| Detector { kind, re: Regex::new(pat).expect("static regex") })
            .collect()
    })
}

/// candidates returns the ranked list of addable strings for a cell: detected
/// indicators first, then whitespace tokens, then the whole trimmed cell. The
/// list is de-duplicated (first occurrence wins) and never contains blanks.
pub fn candidates(cell: &str) -> Vec<Candidate> {
    let cell = cell.trim();
    let mut out: Vec<Candidate> = Vec::new();
    let mut seen: std::collections::HashSet<String> = std::collections::HashSet::new();
    let mut push = |kind: &'static str, text: &str| {
        let text = text.trim_matches(|c: char| c.is_whitespace() || matches!(c, ',' | ';' | '|' | '"' | '\''));
        if text.is_empty() {
            return;
        }
        if seen.insert(text.to_string()) {
            out.push(Candidate { kind, text: text.to_string() });
        }
    };

    if cell.is_empty() {
        return out;
    }

    // Detected indicators, most specific first.
    for d in detectors() {
        for m in d.re.find_iter(cell) {
            push(d.kind, m.as_str());
        }
    }

    // Plain whitespace tokens as a fallback for anything the detectors missed.
    // Skipped when the cell is a single token (the whole-cell entry covers it).
    let tokens: Vec<&str> = cell.split_whitespace().collect();
    if tokens.len() > 1 {
        for t in tokens {
            push("token", t);
        }
    }

    // Whole cell last, so a one-token or free-text cell is still one click.
    push("whole cell", cell);

    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn texts(cell: &str) -> Vec<String> {
        candidates(cell).into_iter().map(|c| c.text).collect()
    }

    #[test]
    fn empty_cell_has_no_candidates() {
        assert!(candidates("").is_empty());
        assert!(candidates("   ").is_empty());
    }

    #[test]
    fn single_token_is_one_candidate() {
        // No separate "token" entry when the whole cell is one token.
        assert_eq!(texts("evil.exe"), vec!["evil.exe"]);
    }

    #[test]
    fn detects_ipv4_first() {
        let got = texts("connection from 192.168.1.44 established");
        assert_eq!(got[0], "192.168.1.44");
        // Whole cell is present as a fallback.
        assert!(got.contains(&"connection from 192.168.1.44 established".to_string()));
    }

    #[test]
    fn detects_hashes_by_length() {
        let md5 = "d41d8cd98f00b204e9800998ecf8427e";
        let sha1 = "da39a3ee5e6b4b0d3255bfef95601890afd80709";
        assert!(candidates(md5).iter().any(|c| c.kind == "MD5"));
        assert!(candidates(sha1).iter().any(|c| c.kind == "SHA-1"));
    }

    #[test]
    fn detects_windows_path() {
        let got = candidates(r"ran C:\Users\bob\evil.exe now");
        assert!(got.iter().any(|c| c.kind == "path" && c.text == r"C:\Users\bob\evil.exe"));
    }

    #[test]
    fn detects_email_and_domain() {
        let got = candidates("mail from bob@evil.com via relay");
        assert!(got.iter().any(|c| c.kind == "email" && c.text == "bob@evil.com"));
    }

    #[test]
    fn dedupes_repeats() {
        let got = texts("evil.com evil.com");
        assert_eq!(got.iter().filter(|t| *t == "evil.com").count(), 1);
    }

    #[test]
    fn guid_detected() {
        let g = "550e8400-e29b-41d4-a716-446655440000";
        assert!(candidates(g).iter().any(|c| c.kind == "GUID"));
    }
}
