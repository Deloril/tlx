//! Port of internal/model/query.go: the boolean filter query language used
//! by the search box.
//!
//! A query is a boolean expression over field comparisons, for example:
//!
//!   Summary=derp AND (tag=bad OR tag=suspicious)
//!   Host=/^dc-\d+$/ AND NOT tag=benign
//!
//! Grammar (precedence NOT > AND > OR; parentheses override):
//!
//!   or    := and ( "OR" and )*
//!   and   := not ( "AND" not )*
//!   not   := "NOT" not | atom
//!   atom  := "(" or ")" | term
//!   term  := field ("=" | "!=") value | value
//!   value := bareword | "quoted" | /regex/
//!
//! field is a column name (case-insensitive), or one of the special names
//! tag/tags, comment/comments, row/#, any/*. A bare value with no field
//! matches any column. Matching is substring and case-insensitive unless the
//! caller asks for case sensitivity; a value wrapped in /.../ is a regular
//! expression.

use super::view::{Matcher, RowPred, View};

/// QTimeOp is a timestamp comparison operator on a term.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum QTimeOp {
    None,
    Before,
    After,
    Between,
}

/// QNode is a parsed query expression node.
pub struct QNode {
    kind: QNodeKind,
}

enum QNodeKind {
    And(Box<QNode>, Box<QNode>),
    Or(Box<QNode>, Box<QNode>),
    Not(Box<QNode>),
    Term {
        field: String,
        has_field: bool,
        neg: bool,
        value: String,
        regex: bool,
        time_op: QTimeOp,
        time_a: String,
        time_b: String,
    },
}

/// ParseQuery parses a filter query string into an expression tree. An empty
/// or whitespace-only string yields Ok(None): no filter.
pub fn parse_query(s: &str) -> Result<Option<QNode>, String> {
    let toks = lex_query(s)?;
    if toks.is_empty() {
        return Ok(None);
    }
    let mut p = QParser { toks, pos: 0 };
    let node = p.parse_or()?;
    if p.pos != p.toks.len() {
        return Err(format!("unexpected {}", p.toks[p.pos].describe()));
    }
    Ok(Some(node))
}

// --- lexer ---

#[derive(Clone, Copy, PartialEq, Eq)]
enum TKind {
    LParen,
    RParen,
    And,
    Or,
    Not,
    Term,
}

#[derive(Clone)]
struct QToken {
    kind: TKind,
    field: String,
    has_field: bool,
    neg: bool,
    value: String,
    regex: bool,
    time_op: QTimeOp,
    time_a: String,
    time_b: String,
}

impl QToken {
    fn simple(kind: TKind) -> Self {
        QToken {
            kind,
            field: String::new(),
            has_field: false,
            neg: false,
            value: String::new(),
            regex: false,
            time_op: QTimeOp::None,
            time_a: String::new(),
            time_b: String::new(),
        }
    }

    fn describe(&self) -> &'static str {
        match self.kind {
            TKind::LParen => "'('",
            TKind::RParen => "')'",
            TKind::And => "'AND'",
            TKind::Or => "'OR'",
            TKind::Not => "'NOT'",
            TKind::Term => "term",
        }
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
pub(super) enum AtomKind {
    None,
    Bare,
    Quoted,
    Regex,
}

fn lex_query(s: &str) -> Result<Vec<QToken>, String> {
    let mut toks = Vec::new();
    let n = s.len();
    let mut i = 0usize;
    while i < n {
        let c = s.as_bytes()[i];
        match c {
            b' ' | b'\t' | b'\n' | b'\r' => {
                i += 1;
                continue;
            }
            b'(' => {
                toks.push(QToken::simple(TKind::LParen));
                i += 1;
                continue;
            }
            b')' => {
                toks.push(QToken::simple(TKind::RParen));
                i += 1;
                continue;
            }
            _ => {}
        }

        let (val, kind, next) = read_atom(s, i)?;
        if kind == AtomKind::None {
            return Err(format!("unexpected character {:?}", &s[i..i + 1]));
        }
        i = next;

        // A bare word may be a boolean keyword.
        if kind == AtomKind::Bare {
            match val.to_uppercase().as_str() {
                "AND" => {
                    toks.push(QToken::simple(TKind::And));
                    continue;
                }
                "OR" => {
                    toks.push(QToken::simple(TKind::Or));
                    continue;
                }
                "NOT" => {
                    toks.push(QToken::simple(TKind::Not));
                    continue;
                }
                _ => {}
            }
        }

        // Look for a comparison operator following the first atom.
        let j = skip_spaces(s, i);
        let bytes = s.as_bytes();
        if j < n && (bytes[j] == b'=' || (bytes[j] == b'!' && j + 1 < n && bytes[j + 1] == b'=')) {
            let neg = bytes[j] == b'!';
            let mut j = j;
            if neg {
                j += 2;
            } else {
                j += 1;
            }
            j = skip_spaces(s, j);
            let (vval, vkind, vnext) = read_atom(s, j)?;
            if vkind == AtomKind::None {
                return Err(format!("expected a value after {:?}", op_str(neg)));
            }
            i = vnext;
            toks.push(QToken {
                kind: TKind::Term,
                field: val,
                has_field: true,
                neg,
                value: vval,
                regex: vkind == AtomKind::Regex,
                time_op: QTimeOp::None,
                time_a: String::new(),
                time_b: String::new(),
            });
            continue;
        }

        // A field followed by a timestamp keyword: FIELD before|after|between ...
        if kind == AtomKind::Bare || kind == AtomKind::Quoted {
            let j = skip_spaces(s, i);
            if let Ok((kw, kwkind, kwnext)) = read_atom(s, j) {
                if kwkind == AtomKind::Bare {
                    match kw.to_lowercase().as_str() {
                        "before" | "after" => {
                            let (op_raw, next) = read_time_operand(s, kwnext);
                            if op_raw.is_empty() {
                                return Err(format!("expected a time after {kw:?}"));
                            }
                            let op = if kw.eq_ignore_ascii_case("after") {
                                QTimeOp::After
                            } else {
                                QTimeOp::Before
                            };
                            toks.push(QToken {
                                kind: TKind::Term,
                                field: val,
                                has_field: true,
                                neg: false,
                                value: String::new(),
                                regex: false,
                                time_op: op,
                                time_a: op_raw,
                                time_b: String::new(),
                            });
                            i = next;
                            continue;
                        }
                        "between" => {
                            let low = read_between_low(s, kwnext);
                            let Some((a_raw, after_and)) = low else {
                                return Err("'between' needs 'and': FIELD between X and Y".to_string());
                            };
                            let (b_raw, next) = read_time_operand(s, after_and);
                            if a_raw.is_empty() || b_raw.is_empty() {
                                return Err("'between' needs two times: FIELD between X and Y".to_string());
                            }
                            toks.push(QToken {
                                kind: TKind::Term,
                                field: val,
                                has_field: true,
                                neg: false,
                                value: String::new(),
                                regex: false,
                                time_op: QTimeOp::Between,
                                time_a: a_raw,
                                time_b: b_raw,
                            });
                            i = next;
                            continue;
                        }
                        _ => {}
                    }
                }
            }
        }

        // Otherwise a bare value term (matches any column).
        toks.push(QToken {
            kind: TKind::Term,
            field: String::new(),
            has_field: false,
            neg: false,
            value: val,
            regex: kind == AtomKind::Regex,
            time_op: QTimeOp::None,
            time_a: String::new(),
            time_b: String::new(),
        });
    }
    Ok(toks)
}

/// readTimeOperand consumes the raw text of a time operand starting at
/// `from`: it runs to the next boolean keyword (AND/OR/NOT), parenthesis, or
/// end of input, so an operand may contain spaces (a "date time" literal) and
/// a " +/- <dur>" suffix. Returns the trimmed operand text and the offset
/// just past it.
fn read_time_operand(s: &str, from: usize) -> (String, usize) {
    let start = skip_spaces(s, from);
    let mut i = start;
    let mut last = start;
    loop {
        let j = skip_spaces(s, i);
        if j >= s.len() || s.as_bytes()[j] == b'(' || s.as_bytes()[j] == b')' {
            break;
        }
        let Ok((atom, k, next)) = read_atom(s, j) else { break };
        if k == AtomKind::None {
            break;
        }
        if k == AtomKind::Bare {
            match atom.to_uppercase().as_str() {
                "AND" | "OR" | "NOT" => {
                    return (s[start..last].trim().to_string(), i);
                }
                _ => {}
            }
        }
        i = next;
        last = next;
    }
    (s[start..last].trim().to_string(), i)
}

/// readBetweenLow consumes the low operand of a between up to the 'and'
/// keyword. Returns the operand text and the offset just past 'and', or None
/// if no 'and' separator is found.
fn read_between_low(s: &str, from: usize) -> Option<(String, usize)> {
    let start = skip_spaces(s, from);
    let mut i = start;
    let mut last = start;
    loop {
        let j = skip_spaces(s, i);
        if j >= s.len() || s.as_bytes()[j] == b'(' || s.as_bytes()[j] == b')' {
            return None;
        }
        let Ok((atom, k, next)) = read_atom(s, j) else { return None };
        if k == AtomKind::None {
            return None;
        }
        if k == AtomKind::Bare && atom.eq_ignore_ascii_case("and") {
            return Some((s[start..last].trim().to_string(), next));
        }
        i = next;
        last = next;
    }
}

fn op_str(neg: bool) -> &'static str {
    if neg { "!=" } else { "=" }
}

pub(super) fn skip_spaces(s: &str, i: usize) -> usize {
    let b = s.as_bytes();
    let mut i = i;
    while i < b.len() && matches!(b[i], b' ' | b'\t' | b'\n' | b'\r') {
        i += 1;
    }
    i
}

/// readAtom reads one value/identifier atom starting at i: a quoted string,
/// a /regex/, or a bareword. Returns the unwrapped text and how it was
/// written.
fn read_atom(s: &str, i: usize) -> Result<(String, AtomKind, usize), String> {
    let (val, kind, next, ok) = read_atom_raw(s, i);
    if ok {
        Ok((val, kind, next))
    } else if kind == AtomKind::Quoted {
        Err("unterminated quoted string".to_string())
    } else {
        Err("unterminated /regex/".to_string())
    }
}

/// readAtomRaw is read_atom without the error wrapping, mirroring Go's
/// readAtom signature exactly (value, kind, next-offset, ok). Used by
/// filterspans.rs, which classifies leniently and needs the partial atom
/// (and its end offset) even when a quote or regex is left unterminated.
pub(super) fn read_atom_raw(s: &str, i: usize) -> (String, AtomKind, usize, bool) {
    let b = s.as_bytes();
    let n = b.len();
    if i >= n {
        return (String::new(), AtomKind::None, i, true);
    }
    match b[i] {
        b'"' => {
            let mut i = i + 1;
            let mut out = String::new();
            while i < n {
                // Only \" and \\ are escapes. Any other backslash is literal:
                // forensic data is full of Windows paths and escaped \t / \n
                // sequences, so "e:\t%COMSPEC%" must stay backslash-t, not
                // collapse to "e:t%COMSPEC%".
                if b[i] == b'\\' && i + 1 < n && (b[i + 1] == b'"' || b[i + 1] == b'\\') {
                    out.push(b[i + 1] as char);
                    i += 2;
                    continue;
                }
                if b[i] == b'"' {
                    return (out, AtomKind::Quoted, i + 1, true);
                }
                out.push(b[i] as char);
                i += 1;
            }
            (String::new(), AtomKind::Quoted, i, false)
        }
        b'/' => {
            let mut i = i + 1;
            let mut out = String::new();
            while i < n {
                if b[i] == b'\\' && i + 1 < n && b[i + 1] == b'/' {
                    out.push('/');
                    i += 2;
                    continue;
                }
                if b[i] == b'/' {
                    return (out, AtomKind::Regex, i + 1, true);
                }
                out.push(b[i] as char);
                i += 1;
            }
            (String::new(), AtomKind::Regex, i, false)
        }
        _ => {
            let start = i;
            let mut i = i;
            while i < n {
                let c = b[i];
                if matches!(c, b' ' | b'\t' | b'\n' | b'\r' | b'(' | b')' | b'=' | b'"') {
                    break;
                }
                if c == b'!' && i + 1 < n && b[i + 1] == b'=' {
                    break;
                }
                i += 1;
            }
            if i == start {
                return (String::new(), AtomKind::None, i, true);
            }
            (s[start..i].to_string(), AtomKind::Bare, i, true)
        }
    }
}

// --- parser ---

struct QParser {
    toks: Vec<QToken>,
    pos: usize,
}

impl QParser {
    fn peek(&self) -> Option<&QToken> {
        self.toks.get(self.pos)
    }

    fn parse_or(&mut self) -> Result<QNode, String> {
        let mut left = self.parse_and()?;
        loop {
            match self.peek() {
                Some(t) if t.kind == TKind::Or => {
                    self.pos += 1;
                    let right = self.parse_and()?;
                    left = QNode { kind: QNodeKind::Or(Box::new(left), Box::new(right)) };
                }
                _ => return Ok(left),
            }
        }
    }

    fn parse_and(&mut self) -> Result<QNode, String> {
        let mut left = self.parse_not()?;
        loop {
            match self.peek() {
                Some(t) if t.kind == TKind::And => {
                    self.pos += 1;
                    let right = self.parse_not()?;
                    left = QNode { kind: QNodeKind::And(Box::new(left), Box::new(right)) };
                }
                _ => return Ok(left),
            }
        }
    }

    fn parse_not(&mut self) -> Result<QNode, String> {
        if let Some(t) = self.peek() {
            if t.kind == TKind::Not {
                self.pos += 1;
                let child = self.parse_not()?;
                return Ok(QNode { kind: QNodeKind::Not(Box::new(child)) });
            }
        }
        self.parse_atom()
    }

    fn parse_atom(&mut self) -> Result<QNode, String> {
        let t = self.peek().ok_or_else(|| "unexpected end of query".to_string())?;
        match t.kind {
            TKind::LParen => {
                self.pos += 1;
                let inner = self.parse_or()?;
                match self.peek() {
                    Some(c) if c.kind == TKind::RParen => {
                        self.pos += 1;
                        Ok(inner)
                    }
                    _ => Err("missing ')'".to_string()),
                }
            }
            TKind::Term => {
                let t = t.clone();
                self.pos += 1;
                Ok(QNode {
                    kind: QNodeKind::Term {
                        field: t.field,
                        has_field: t.has_field,
                        neg: t.neg,
                        value: t.value,
                        regex: t.regex,
                        time_op: t.time_op,
                        time_a: t.time_a,
                        time_b: t.time_b,
                    },
                })
            }
            _ => Err(format!("unexpected {}", t.describe())),
        }
    }
}

/// QBinOp distinguishes the two binary node kinds for QNode::binary().
#[derive(Clone, Copy, PartialEq, Eq)]
pub(super) enum QBinOp {
    And,
    Or,
}

impl QNode {
    /// binary: Some((op, left, right)) if this node is an And/Or node.
    pub(super) fn binary(&self) -> Option<(QBinOp, &QNode, &QNode)> {
        match &self.kind {
            QNodeKind::And(l, r) => Some((QBinOp::And, l, r)),
            QNodeKind::Or(l, r) => Some((QBinOp::Or, l, r)),
            _ => None,
        }
    }

    /// not_child: Some(child) if this node is a Not node.
    pub(super) fn not_child(&self) -> Option<&QNode> {
        match &self.kind {
            QNodeKind::Not(c) => Some(c),
            _ => None,
        }
    }

    /// term: (field, has_field, neg, value, regex) for a plain value/regex
    /// term. Returns None for non-term nodes and for time-comparison terms
    /// (before/after/between), which have nothing to highlight.
    pub(super) fn term(&self) -> Option<(&str, bool, bool, &str, bool)> {
        match &self.kind {
            QNodeKind::Term { field, has_field, neg, value, regex, time_op, .. } => {
                if *time_op != QTimeOp::None {
                    return None;
                }
                Some((field.as_str(), *has_field, *neg, value.as_str(), *regex))
            }
            _ => None,
        }
    }
}

// --- compilation to a row predicate ---

impl<'a> View<'a> {
    /// compileExpr turns a parsed query tree into a predicate over the
    /// view's rows, resolving field names against the current columns and
    /// compiling each value's matcher. `cased` applies to every value in the
    /// expression; `now` is the reference time for the now/time keyword and
    /// relative arithmetic in time comparisons.
    pub(super) fn compile_expr(
        &'a self,
        n: &QNode,
        cased: bool,
        now: chrono::DateTime<chrono::Utc>,
    ) -> Result<RowPred<'a>, String> {
        match &n.kind {
            QNodeKind::And(l, r) => {
                let left = self.compile_expr(l, cased, now)?;
                let right = self.compile_expr(r, cased, now)?;
                Ok(Box::new(move |row, rec| left(row, rec) && right(row, rec)))
            }
            QNodeKind::Or(l, r) => {
                let left = self.compile_expr(l, cased, now)?;
                let right = self.compile_expr(r, cased, now)?;
                Ok(Box::new(move |row, rec| left(row, rec) || right(row, rec)))
            }
            QNodeKind::Not(c) => {
                let child = self.compile_expr(c, cased, now)?;
                Ok(Box::new(move |row, rec| !child(row, rec)))
            }
            QNodeKind::Term {
                field,
                has_field,
                neg,
                value,
                regex,
                time_op,
                time_a,
                time_b,
            } => {
                if *time_op != QTimeOp::None {
                    return self.compile_time_term(field, *has_field, *time_op, time_a, time_b, now);
                }
                let refc = self.resolve_field(field, *has_field)?;
                let m = Matcher::compile(value, *regex, cased).map_err(|e| e.to_string())?;
                let neg = *neg;
                Ok(Box::new(move |row, rec| {
                    let hit = self.column_match(row, rec, refc, &m);
                    if neg { !hit } else { hit }
                }))
            }
        }
    }

    /// compileTimeTerm builds a predicate for a before/after/between
    /// comparison. The field must name a data column; a row matches only if
    /// its cell parses as a timestamp and falls in range. Rows with an
    /// unparseable cell never match.
    fn compile_time_term(
        &'a self,
        field: &str,
        has_field: bool,
        time_op: QTimeOp,
        time_a: &str,
        time_b: &str,
        now: chrono::DateTime<chrono::Utc>,
    ) -> Result<RowPred<'a>, String> {
        let refc = self.resolve_field(field, has_field)?;
        if refc < 0 {
            return Err(format!("time comparison needs a data column, not {field:?}"));
        }
        let a = super::timecol::parse_time_operand(time_a, now)
            .ok_or_else(|| format!("cannot parse time {time_a:?}"))?;
        if time_op == QTimeOp::Between {
            let b = super::timecol::parse_time_operand(time_b, now)
                .ok_or_else(|| format!("cannot parse time {time_b:?}"))?;
            return Ok(Box::new(move |row, rec| {
                let cell = self.cell(row, rec, refc);
                match super::timecol::parse_time(&cell) {
                    Some(t) => t >= a.start && t < b.end,
                    None => false,
                }
            }));
        }
        Ok(Box::new(move |row, rec| {
            let cell = self.cell(row, rec, refc);
            match super::timecol::parse_time(&cell) {
                Some(t) => {
                    if time_op == QTimeOp::Before {
                        t < a.start
                    } else {
                        t >= a.end // after: at or past the end of the named period
                    }
                }
                None => false,
            }
        }))
    }
}

#[cfg(test)]
mod tests {
    use super::super::index::Index;
    use super::super::view::{ColumnCond, ColumnRef, FilterSpec, Overlay, View};
    use std::collections::HashMap as Map;

    #[derive(Default)]
    struct TestOverlay {
        tags: Map<usize, Vec<String>>,
        comments: Map<usize, String>,
    }

    impl Overlay for TestOverlay {
        fn tags(&self, row: usize) -> Vec<String> {
            self.tags.get(&row).cloned().unwrap_or_default()
        }
        fn comment(&self, row: usize) -> String {
            self.comments.get(&row).cloned().unwrap_or_default()
        }
        fn cell_override(&self, _row: usize, _col: usize) -> Option<String> {
            None
        }
    }

    fn query_view(headers: &[&str], records: Vec<Vec<&str>>) -> Index {
        let headers: Vec<String> = headers.iter().map(|s| s.to_string()).collect();
        let records: Vec<Vec<String>> = records
            .into_iter()
            .map(|r| r.into_iter().map(|s| s.to_string()).collect())
            .collect();
        Index::new_memory(headers, records)
    }

    fn visible(idx: &Index, ov: &dyn Overlay, spec: FilterSpec) -> Result<Vec<usize>, String> {
        let mut v = View::new(idx, ov);
        v.apply(spec)?;
        Ok((0..v.len()).map(|i| v.master(i)).collect())
    }

    #[test]
    fn test_query_field_equals() {
        let idx = query_view(
            &["Summary", "Host"],
            vec![vec!["derp happened", "ws1"], vec!["all quiet", "ws2"], vec!["more DERP", "ws3"]],
        );
        let ov = TestOverlay::default();
        let got = visible(&idx, &ov, FilterSpec { expr: "Summary=derp".to_string(), ..Default::default() }).unwrap();
        assert_eq!(got, vec![0, 2]);
    }

    #[test]
    fn test_query_case_sensitive() {
        let idx = query_view(&["Summary"], vec![vec!["derp"], vec!["DERP"]]);
        let ov = TestOverlay::default();
        let got = visible(
            &idx,
            &ov,
            FilterSpec { expr: "Summary=derp".to_string(), cased: true, ..Default::default() },
        )
        .unwrap();
        assert_eq!(got, vec![0]);
    }

    #[test]
    fn test_query_and_or_parens() {
        let idx = query_view(&["Summary"], vec![vec!["derp"], vec!["derp"], vec!["derp"], vec!["something"]]);
        let ov = TestOverlay {
            tags: Map::from([
                (0, vec!["bad".to_string()]),
                (1, vec!["suspicious".to_string()]),
                (2, vec!["benign".to_string()]),
                (3, vec!["bad".to_string()]),
            ]),
            comments: Map::new(),
        };
        let got = visible(
            &idx,
            &ov,
            FilterSpec {
                expr: "Summary=derp AND (tag=bad OR tag=suspicious)".to_string(),
                ..Default::default()
            },
        )
        .unwrap();
        assert_eq!(got, vec![0, 1]);
    }

    #[test]
    fn test_query_regex_value() {
        let idx = query_view(&["Host"], vec![vec!["dc-01"], vec!["ws-99"], vec!["dc-7"]]);
        let ov = TestOverlay::default();
        let got = visible(
            &idx,
            &ov,
            FilterSpec { expr: r"Host=/^dc-\d+$/".to_string(), ..Default::default() },
        )
        .unwrap();
        assert_eq!(got, vec![0, 2]);
    }

    #[test]
    fn test_query_bare_term_any_column() {
        let idx = query_view(&["A", "B"], vec![vec!["foo", "bar"], vec!["baz", "qux"]]);
        let ov = TestOverlay::default();
        let got = visible(&idx, &ov, FilterSpec { expr: "bar".to_string(), ..Default::default() }).unwrap();
        assert_eq!(got, vec![0]);
        let got = visible(&idx, &ov, FilterSpec { expr: "/^q/".to_string(), ..Default::default() }).unwrap();
        assert_eq!(got, vec![1]);
    }

    #[test]
    fn test_query_not_and_not_equals() {
        let idx = query_view(&["Summary"], vec![vec!["derp"], vec!["quiet"], vec!["derp again"]]);
        let ov = TestOverlay::default();
        let got = visible(&idx, &ov, FilterSpec { expr: "NOT Summary=derp".to_string(), ..Default::default() }).unwrap();
        assert_eq!(got, vec![1]);
        let got = visible(&idx, &ov, FilterSpec { expr: "Summary!=derp".to_string(), ..Default::default() }).unwrap();
        assert_eq!(got, vec![1]);
    }

    #[test]
    fn test_query_quoted_value_with_spaces() {
        let idx = query_view(&["Summary"], vec![vec!["a b c"], vec!["abc"]]);
        let ov = TestOverlay::default();
        let got = visible(
            &idx,
            &ov,
            FilterSpec { expr: r#"Summary="a b""#.to_string(), ..Default::default() },
        )
        .unwrap();
        assert_eq!(got, vec![0]);
    }

    #[test]
    fn test_query_tag_and_comment() {
        let idx = query_view(&["Summary"], vec![vec!["x"], vec!["y"], vec!["z"]]);
        let ov = TestOverlay {
            tags: Map::from([(0, vec!["bad".to_string()]), (1, vec!["good".to_string()])]),
            comments: Map::from([(2, "needs review".to_string())]),
        };
        let got = visible(
            &idx,
            &ov,
            FilterSpec { expr: "tag=bad OR comment=review".to_string(), ..Default::default() },
        )
        .unwrap();
        assert_eq!(got, vec![0, 2]);
    }

    #[test]
    fn test_query_precedence() {
        let idx = query_view(&["S"], vec![vec!["a"], vec!["b c"], vec!["b"], vec!["c"]]);
        let ov = TestOverlay::default();
        let got = visible(
            &idx,
            &ov,
            FilterSpec { expr: "S=a OR S=b AND S=c".to_string(), ..Default::default() },
        )
        .unwrap();
        assert_eq!(got, vec![0, 1]);
    }

    #[test]
    fn test_col_filters() {
        let idx = query_view(
            &["Host", "Msg"],
            vec![vec!["ws1", "logon"], vec!["ws2", "logon"], vec!["ws1", "logoff"]],
        );
        let ov = TestOverlay::default();
        let mut cf: Map<ColumnRef, String> = Map::new();
        cf.insert(0, "ws1".to_string());
        cf.insert(1, "logon".to_string());
        let got = visible(&idx, &ov, FilterSpec { col_filters: cf, ..Default::default() }).unwrap();
        assert_eq!(got, vec![0]);

        let mut cf2: Map<ColumnRef, String> = Map::new();
        cf2.insert(0, "ws2".to_string());
        let got = visible(
            &idx,
            &ov,
            FilterSpec { expr: "Msg=logon".to_string(), col_filters: cf2, ..Default::default() },
        )
        .unwrap();
        assert_eq!(got, vec![1]);

        let mut cf3: Map<ColumnRef, String> = Map::new();
        cf3.insert(0, "WS1".to_string());
        let got = visible(&idx, &ov, FilterSpec { cased: true, col_filters: cf3, ..Default::default() }).unwrap();
        assert_eq!(got, Vec::<usize>::new());
    }

    #[test]
    fn test_col_filter_non_empty() {
        let idx = query_view(
            &["Host", "Note"],
            vec![vec!["ws1", "hello"], vec!["ws2", ""], vec!["ws3", "  "], vec!["ws4", "world"]],
        );
        let ov = TestOverlay::default();
        let mut cf: Map<ColumnRef, String> = Map::new();
        cf.insert(1, "*".to_string());
        let got = visible(&idx, &ov, FilterSpec { col_filters: cf, ..Default::default() }).unwrap();
        assert_eq!(got, vec![0, 3]);
    }

    #[test]
    fn test_query_quoted_backslash_is_literal() {
        // plaso writes \t and \n as two-character escapes inside the message,
        // and Windows paths carry backslashes. A quoted value must keep the
        // backslash: "e:\t%COMSPEC%" matches the literal bytes e:\t%COMSPEC%,
        // not e:t%COMSPEC%.
        let idx = query_view(
            &["Msg"],
            vec![vec![r"service file name:\te:\t%COMSPEC% /c"], vec!["e:t%COMSPEC%"]],
        );
        let ov = TestOverlay::default();
        let got = visible(
            &idx,
            &ov,
            FilterSpec { expr: r#""e:\t%COMSPEC%""#.to_string(), ..Default::default() },
        )
        .unwrap();
        assert_eq!(got, vec![0]);

        // \" and \\ still escape: a value with an embedded quote and backslash.
        let idx = query_view(&["Msg"], vec![vec![r#"a"b\c"#], vec!["nope"]]);
        let got = visible(
            &idx,
            &ov,
            FilterSpec { expr: r#""a\"b\\c""#.to_string(), ..Default::default() },
        )
        .unwrap();
        assert_eq!(got, vec![0]);
    }

    #[test]
    fn test_query_time_quoted_field() {
        let idx = query_view(
            &["Event Time", "Summary"],
            vec![vec!["2020-06-01 10:00:00", "mid"], vec!["2021-06-01 10:00:00", "new"]],
        );
        let ov = TestOverlay::default();
        let got = visible(
            &idx,
            &ov,
            FilterSpec {
                expr: r#""Event Time" between 2020 and 2020"#.to_string(),
                ..Default::default()
            },
        )
        .unwrap();
        assert_eq!(got, vec![0]);
    }

    fn time_idx() -> Index {
        query_view(
            &["Timestamp", "Summary"],
            vec![
                vec!["2019-06-01 10:00:00", "old"],
                vec!["2020-06-01 10:00:00", "mid"],
                vec!["2021-06-01 10:00:00", "new"],
                vec!["notatime", "bad"],
            ],
        )
    }

    #[test]
    fn test_query_time_operators() {
        let cases: Vec<(&str, Vec<usize>)> = vec![
            ("Timestamp before 2020", vec![0]),
            ("Timestamp after 2020", vec![2]),
            ("Timestamp between 2020 and 2020", vec![1]),
            ("Timestamp between 2019 and 2021", vec![0, 1, 2]),
            ("Timestamp before 2020-06-01 10:00:00 + 1d", vec![0, 1]),
            ("Timestamp after 2019 AND Summary=new", vec![2]),
            ("Timestamp between 2019 and 2021 AND Summary=mid", vec![1]),
        ];
        for (q, want) in cases {
            let idx = time_idx();
            let ov = TestOverlay::default();
            let got = visible(&idx, &ov, FilterSpec { expr: q.to_string(), ..Default::default() }).unwrap();
            assert_eq!(got, want, "query {q:?}");
        }
    }

    #[test]
    fn test_query_time_errors() {
        let cases = [
            "Timestamp before notaday",
            "Timestamp between 2020",
            "Timestamp between 2020 and",
            "tag before 2020",
        ];
        for q in cases {
            let idx = time_idx();
            let ov = TestOverlay::default();
            let mut v = View::new(&idx, &ov);
            let err = v.apply(FilterSpec { expr: q.to_string(), ..Default::default() });
            assert!(err.is_err(), "query {q:?}: expected error, got none (kept {} rows)", v.len());
        }
    }

    #[test]
    fn test_query_errors() {
        let cases = ["S=x AND", "(S=x", "S=x)", "Nope=x", "S=/[/", r#"S="unterminated"#, "S=x S=y"];
        for q in cases {
            let idx = query_view(&["S"], vec![vec!["x"]]);
            let ov = TestOverlay::default();
            let mut v = View::new(&idx, &ov);
            let err = v.apply(FilterSpec { expr: q.to_string(), ..Default::default() });
            assert!(err.is_err(), "query {q:?}: expected error, got none (kept {} rows)", v.len());
        }
    }
}
