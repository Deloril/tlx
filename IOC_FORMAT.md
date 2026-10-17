# tlx IOC list format

An IOC list is plain UTF-8 text, one entry per line. tlx compiles the list,
scans every row of the open timeline, and for each row that matches at least
one indicator it (a) tags the row `ioc:<list name>` and (b) optionally stamps a
comment onto the row (see "Comment blocks"). Your job is to produce the body of
one such list.

## Line types

Each non-empty line is one of four things:

1. **Plain substring** — `evil.exe`
   Case-insensitive substring match against every data column of the row.
   `evil.exe` matches a cell containing `C:\temp\EVIL.exe`.

2. **Regex** — `/T[0-9]{4}/`
   A line wrapped in forward slashes is a regular expression, matched against
   every data column. Rust regex syntax (same as RE2: no backreferences, no
   lookaround). Case-insensitive by default.

3. **Filter query** — `` `Summary=psexec AND Host=/^dc-\d+$/ ``
   A line starting with a backtick (`` ` ``) is a full boolean filter query
   over named fields. This is the only form that can target a specific column,
   combine conditions, or match on timestamps. Grammar below.

4. **Comment** — `# note` or `## label`
   A line starting with `#` is never matched against rows. A single `#` is an
   ignored note. A double `##` is a label for the block of indicators beneath
   it (see "Comment blocks").

Blank lines separate blocks (see "Comment blocks"); outside that role they are
ignored.

### What plain and regex lines do NOT see

Plain and `/regex/` lines match data columns only. They deliberately skip the
row's own Tags and Comment fields, so an indicator never matches a row on its
own annotations. To match on a tag or comment, use a backtick query that names
the field: `` `tag=beacon `` or `` `comment=/psexec/ ``.

## Filter query grammar (backtick lines)

Everything after the leading backtick is a query. Precedence is
`NOT` > `AND` > `OR`; parentheses override.

    or    := and ( "OR" and )*
    and   := not ( "AND" not )*
    not   := "NOT" not | atom
    atom  := "(" or ")" | term
    term  := field ("=" | "!=") value | value
    value := bareword | "quoted string" | /regex/

- **Operators** `AND`, `OR`, `NOT` are case-insensitive. You can write them in
  caps for readability.
- **field** is a column header (case-insensitive match against the timeline's
  headers), or one of the special names:
  - `tag` / `tags` — the row's tags
  - `comment` / `comments` — the row's comment
  - `row` / `#` / `line` — the 1-based row number
  - `any` / `*` — any column (same as a bare value)
- **= vs !=** — `field=value` matches rows where the field contains value;
  `field!=value` matches rows where it does not.
- **value** forms:
  - bareword: `psexec` (substring, case-insensitive)
  - quoted: `"lateral movement"` (use when the value has spaces or operator
    keywords in it)
  - regex: `/^dc-\d+$/` (anchored/complex matching)
- A **bare value with no field** (e.g. `` `psexec ``) matches any column —
  identical to a plain `psexec` line except that a backtick query also reaches
  the Tags and Comment fields.

### Timestamp conditions

A field followed by `before`, `after`, or `between` compares that column as a
timestamp:

    `Timestamp after 2024-01-01
    `Timestamp before 2024-06-30 12:00:00
    `Timestamp between 2024-01-01 and 2024-02-01

- Operands may be any common timestamp format (`2024-01-01`,
  `2024-01-01 09:15:00`, `2024-01-01T09:15:00`, `01/02/2024 09:15:00`,
  RFC3339 with offset, etc.) or the keyword `now` (alias `time`).
- Relative arithmetic is allowed with spaces around the sign and a duration:
  `now - 7d`, `2024-01-01 + 12h`. Duration units: `s` seconds, `m` minutes,
  `h` hours, `d` days, `w` weeks; combinable like `1d12h`.
- `between X and Y` is inclusive of the range; the literal word `and` is
  required.

Timestamp conditions compose with the rest of a query:

    `Summary=logon AND Timestamp between now - 24h and now

## Comment blocks (`##`)

A `##` line labels every indicator below it, up to the next blank line. When
any of those indicators matches a row, the label text is merged into that row's
comment: set if the comment is blank, left alone if it already contains the
text, otherwise prepended as `label; existing comment`.

A blank line ends the block. A `##` with no text, or a new `##`, starts a fresh
block. A single-`#` line inside a block is ignored but does NOT end the block.

Example:

    ## Lateral movement via PsExec
    `Summary=psexec
    /PSEXESVC/

    ## Suspicious scripting
    /T1059/
    powershell -enc

    # the line below has no label above it, so it tags only, no comment
    evil.exe

Rows hitting the first two indicators get the comment "Lateral movement via
PsExec"; rows hitting the next two get "Suspicious scripting"; rows hitting
`evil.exe` are tagged but get no comment.

## Matching semantics (summary)

- Matching is case-insensitive.
- Plain and regex lines scan data columns only (not Tags/Comment).
- Backtick queries can target any column plus the virtual Tags/Comment/row
  fields.
- A row matches the list if it matches ANY single line (lines are OR-ed
  together for tagging purposes).
- Blank lines and `#` lines never match.

## Authoring guidance

- Prefer a specific `` `Field=value `` query over a bare substring when you
  know which column the indicator lives in — it avoids false hits in unrelated
  columns.
- Use `/regex/` for structured indicators (hashes, IPs, MITRE technique IDs,
  path patterns); use a plain substring for literal filenames or tool names.
- Quote values containing spaces or the words and/or/not: `` `Summary="not
  before" ``.
- Group related indicators under a `##` label so analysts see WHY a row was
  flagged, not just that it was.
- One list = one theme. Name the list for the behaviour it detects; that name
  becomes the `ioc:<name>` tag.

## A complete example list

    ## Credential dumping
    /lsass/
    `Summary=comsvcs.dll AND Summary=MiniDump
    procdump

    ## Lateral movement
    `Summary=psexec
    /PSEXESVC/
    wmic /node:

    ## Recent suspicious PowerShell (last 24h)
    `Summary=/powershell.*-enc/ AND Timestamp between now - 24h and now
