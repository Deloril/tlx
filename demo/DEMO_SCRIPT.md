# tlx demo script

A presenter walkthrough for a showoff video. Two synthetic host timelines, one
intrusion. You start from a single reported filename and pivot artifact by
artifact until the whole story falls out across both machines.

The flow has five acts:

1. Open one timeline and follow a pivot chain on the workstation.
2. Use the ±5 minute window to pull the rest of the activity out of the noise.
3. Spot the jump to a second host, roll the two timelines into a case.
4. Turn the workstation pivots into an IOC list and fire it at the server.
5. Tag what the IOC can't see, then read the master timeline end to end.

Timings are deliberately absent. Move at whatever pace reads well on camera.

---

## Setup (before you record)

Generate the data once:

    python3 -I demo/generate_timelines.py

That writes `WKSTN-07_filesystem.csv` (~11.9k rows) and
`SRV-APP01_filesystem.csv` (~9k rows) into this folder. Both are mostly real,
benign Windows filesystem/registry/task noise with a hand-built intrusion
spliced in. Nothing in them executes anything.

Launch with the workstation open:

    target/release/tlx demo/WKSTN-07_filesystem.csv

Have `demo/winsvchost_loader.ioc.txt` open in a text editor off to the side, or
just rebuild the list live in act 4 from the Artifacts panel — your call.

The whole on-screen premise: a user at WKSTN-07 reported that an emailed
"invoice" opened a black window and vanished. You have the host's filesystem
timeline. That one filename, `invoice_scan.exe`, is your only lead.

---

## Act 1 — pivot on the workstation

### 1.1 Why you can't just scroll

Say up front: ~12,000 rows, ~2,300 of them on the day in question alone. Scroll
the grid for a second so the audience feels the volume, then stop.

> "This is one machine's filesystem timeline. Eyeballing it is hopeless. I have
> exactly one lead — a filename — so I start there and let each artifact point
> at the next."

### 1.2 Start from the filename

Click the filter box (or press `/`) and type:

    invoice_scan

Enter. Four rows: the file created in `C:\Users\jmorgan\Downloads`, modified,
accessed, and a PE compile timestamp. Click the created row to load it into the
Details panel on the right.

Point at two things:
- the MD5 — `4d5e2b1a9c8f3e7d6a0b1c2d3e4f5a6b`
- the PE compile time — `2015-03-09`, the day *before* it arrived. A freshly
  built binary dressed up as an invoice.

### 1.3 Grab the hash as an artifact, pivot on it

Right-click the MD5 value in the grid. tlx pulls the indicators out of the cell
for you — the menu offers the hash as a one-click `Add to Artifacts`. Add it.
(Call out that this is the indicator-spotting surface: the tool proposes, you
decide what's worth keeping.)

Now clear the filter (Esc) and filter on the hash itself:

    4d5e2b1a9c8f3e7d6a0b1c2d3e4f5a6b

Nine rows, but look at the paths — the same bytes under three different names in
three different places:

- `C:\Users\jmorgan\Downloads\invoice_scan.exe` — where it landed
- `C:\Users\jmorgan\AppData\Roaming\Microsoft\Windows\svch0st.exe` — a renamed
  copy hiding in the profile (that's a zero, not an "o")
- `C:\ProgramData\WinSvcHost\spoolsvc.exe` — a second copy in a service-looking
  folder
- `\\SRV-APP01\C$\Windows\Temp\spoolsvc.exe` — park this one. We'll come back.

> "Same file, three names, three locations. Renaming to blend in is the whole
> trick. The hash doesn't care what you call it."

### 1.4 Search the staging location

The `C:\ProgramData\WinSvcHost` folder is the interesting one — programs don't
normally live there. Filter on the path:

    WinSvcHost

Click the Timestamp column header to sort. Read the folder:

- the folder and four files — `spoolsvc.exe`, `config.dat`, `p.ps1`,
  `readme.txt` — all created at `14:19:1x`. Dropped together.
- then two stragglers: `7za.exe` at `14:46:05` and `data.7z` at `14:47:33`.

> "Everything in this folder was dropped in the same second — except two files
> that show up half an hour later. An archiver, and an archive. That gap is the
> signal. Something came back to this folder at 14:47 and packaged things up."

---

## Act 2 — the ±5 minute window

### 2.1 Pivot on the odd timestamp

Right-click the `2015-03-10 14:47:33Z` timestamp on the `data.7z` row and choose
**Filter ±5 min around this time**. The Timestamp column narrows to
`14:42:33`–`14:52:33`.

About 53 rows land in the window — most benign. Read them; don't eyeball. The
attack's second stage is all here:

- `14:45:12` — a scheduled task `WinSvcHostUpdate` is created
- `14:45:40` — a `Run` key `...\CurrentVersion\Run\WinSvcHost` pointing at
  `spoolsvc.exe` — persistence, twice over
- `14:46:05` — `7za.exe` written
- `14:46:40` — the `p.ps1` collection script is accessed
- `14:47:33` — `data.7z` created (our pivot)
- `14:48:05` — `config.dat` modified
- `14:49:20` — `\\SRV-APP01\C$\Windows\Temp\spoolsvc.exe` accessed
- `14:49:22` — a mapped-drive record for `\\SRV-APP01\C$`

> "One right-click turned a single suspicious file into the full local picture:
> persistence by Run key and scheduled task, a staging archive, and a
> collection script. And right at the end — a copy of the loader written to
> another machine's C$ share."

### 2.2 Tag the local story

Select the rows that matter (click, Shift-click / Ctrl-click to multi-select)
and press `t` to tag. Use something like `loader`, `persistence`, `staging`.
Press `c` on the lateral-copy row and comment: `copied to SRV-APP01 - 14:49`.

Tick **Tagged only** in the filter panel to show the view collapse to just your
annotated rows. That is the workstation story in a dozen lines.

---

## Act 3 — two hosts, one case

### 3.1 The jump

Point back at the `\\SRV-APP01` rows.

> "The workstation isn't the end of it. The loader was copied to a server,
> SRV-APP01, at 14:49. I've got that host's timeline too. Time to stop working
> files and start working a case."

### 3.2 Create the case from the workstation

In the toolbar: **Create case with this timeline…**. Save it as
`demo/winsvchost-case.tlxdb`. The workstation comes into the case with the tags
and comments you just made intact — check the Case panel in the left sidebar.

### 3.3 Add the server

Case panel → **Add timeline…** → pick `demo/SRV-APP01_filesystem.csv`. Now the
case holds both hosts. Switch between them from the Case panel. The server is
~9,000 rows of its own noise — same problem as before, so don't hand-hunt it.

---

## Act 4 — build an IOC, fire it at the server

### 4.1 Harvest the workstation's indicators

Go back to the workstation in the case. Open **Investigator's notes** →
**Artifacts**. If you added the hash earlier it's already there; add the rest by
right-clicking cells, or just use the ready-made list. Hit **Export list** to
pop the artifacts out as copyable text.

### 4.2 Make the list

Left sidebar → **IOC lists** → **New list**. Name it `winsvchost-loader`. Paste
the body (from Export list, or straight from `demo/winsvchost_loader.ioc.txt`):

    ## Loader binary (hash + known aliases)
    /4d5e2b1a9c8f3e7d6a0b1c2d3e4f5a6b/
    svch0st.exe
    spoolsvc.exe
    invoice_scan.exe

    ## Staging directory and collection archives
    \WinSvcHost\
    data.7z
    db_backup.7z

    ## Persistence: Run key and scheduled task
    `Summary=Run\WinSvcHost
    WinSvcHostUpdate

Call out the three line types as you paste: a `/regex/` for the hash, plain
substrings for the filenames and the staging path, and a backtick query
targeting the Run key. The `##` labels become the comment on every row they
match, so an analyst sees *why* a row was flagged.

### 4.3 Run it against both hosts

**Save & run** scans the open timeline. Then switch to the server and run the
list there too (or **Run all** from the IOC section to sweep every timeline in
the case at once). Filter the server on tag:

    tag=ioc:winsvchost-loader

The list lights up the server's copy of the intrusion with no manual hunting:
the loader in `C:\Windows\Temp` and in `C:\ProgramData\WinSvcHost`, the Run key,
the `WinSvcHostUpdate` task, and `db_backup.7z` in the staging folder.

> "The indicators I pulled off the workstation just found the same toolkit on
> the server. That's the payoff of pivoting into reusable indicators instead of
> one-off searches."

---

## Act 5 — what the IOC can't see, and the master timeline

### 5.1 Follow the archives back to what they hold

The IOC tagged two files in the server's staging folder that weren't on the
workstation: a temp file, `~db7z8f2.tmp`, and `db_backup.7z`. Filter
`WinSvcHost` and sort by time — the temp file is created at `15:19`, the
archive is fully MACE'd at `15:39`, and the temp file is deleted a minute after
that. An archive called `db_backup`, built through a temp file, staged by this
toolkit. A backup of what? You know the move: pivot on the timestamps.

Pivot one — collection. Right-click the `15:19:05` timestamp on the
`~db7z8f2.tmp` created row → **Filter ±5 min around this time** (~41 rows). The
window holds the whole collection step:

- `15:17:40` — `Service/Stopped` on `MSSQLSERVER`
- `15:18:10` — `D:\MSSQL\DATA\CRM_prod.mdf` accessed by `SRV-APP01\svc_sql`
- `15:18:52` — `CRM_prod_log.ldf` accessed
- `15:19:05` — `~db7z8f2.tmp` created — compression starts

> "SQL keeps the data files locked while it's running, so they stopped the
> service to get at them, read the database and its log, and spun up a temp
> archive. The temp file is created the moment the service goes down — that's
> the collection, in order."

Pivot two — packaging and cleanup. Right-click the `15:39:48` timestamp on the
`db_backup.7z` row → **Filter ±5 min** (~40 rows):

- `15:39:48` — `db_backup.7z` created, modified, accessed and changed (a fresh,
  fully MACE'd output file)
- `15:40:15` — `~db7z8f2.tmp` deleted
- `15:41:02` — `Service/Started` on `MSSQLSERVER`

> "Twenty minutes of compression later the finished archive lands, the temp
> file is wiped, and SQL is restarted to close the outage before anyone
> notices. Start to finish, that's the data theft — and the two database reads
> at its centre carry no shared hash and sit nowhere near the staging folder,
> so the IOC never flagged them. You only get here by reading the sequence. The
> list does the mechanical matching; the judgement is still yours."

Tag the two `CRM_prod` reads `exfil`, and the `Service/Stopped` /
`Service/Started` pair `defense-evasion`. Comment the stop row
`SQL stopped to unlock DB files; restarted 15:41 after db_backup.7z staged`.

### 5.2 Read the whole thing as one story

Toolbar → **★ Master timeline**. Every tagged row across both hosts, merged and
read-only, with a **Timeline** column showing which host each came from. Click
the **Time** header to sort, and narrate the intrusion top to bottom:

- `14:18` WKSTN-07 — `invoice_scan.exe` written to Downloads, run
- `14:19` WKSTN-07 — renamed to `svch0st.exe` and `spoolsvc.exe`, staging folder
  created
- `14:45` WKSTN-07 — Run key + `WinSvcHostUpdate` task for persistence
- `14:47` WKSTN-07 — `data.7z` staged
- `14:49` WKSTN-07 → SRV-APP01 — loader copied to the server's C$ share
- `14:51` SRV-APP01 — same loader, same persistence installed on the server
- `15:17` SRV-APP01 — SQL service stopped to unlock the database files
- `15:18` SRV-APP01 — `CRM_prod.mdf` / `.ldf` read; temp archive started
- `15:39` SRV-APP01 — `db_backup.7z` finished, temp file deleted, SQL restarted
- `02:00` next day SRV-APP01 — the scheduled task fires again

Click any master row to jump back into that host's full timeline at that exact
event.

> "One reported filename, followed artifact by artifact, became a two-host
> intrusion with initial access, persistence, lateral movement and data theft —
> on a date-sorted page you could hand to anyone."

---

## Closing beats to mention if you have time

- Nothing was ever written to the source CSVs — tags, comments and edits live in
  the case database (or a sidecar for standalone files) and autosave.
- `Ctrl+E` exports the current view to a new CSV with `Tags` and `Comment`
  columns appended — the write-up artifact.
- The whole investigation used filter, one artifact pivot, one ±5 minute
  window, one IOC list and manual tagging. That's the core loop the tool is
  built to make cheap.
