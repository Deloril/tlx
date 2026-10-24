# tlx demo data

Synthetic timelines and a presenter script for a tlx showoff video. One
intrusion across two Windows hosts, built so that every step of the demo
exercises a real tlx feature: filter, artifact pivot, the ±5 minute window,
cases, IOC lists and the master timeline.

All of this is fabricated teaching data. The noise is real, benign Windows
filesystem/registry/task records sampled from a reference image; the intrusion
is hand-authored inert strings. Nothing here runs or references live malware.

## Files

| File | What it is |
|------|------------|
| `generate_timelines.py` | Builds the two CSVs from the reference noise file. Deterministic (seeded). |
| `WKSTN-07_filesystem.csv` | Host 1 — the workstation. Initial access and local staging. ~11.9k rows. |
| `SRV-APP01_filesystem.csv` | Host 2 — the server. Lateral target and data theft. ~9k rows. |
| `winsvchost_loader.ioc.txt` | The IOC list you build from host 1 and fire at host 2. |
| `DEMO_SCRIPT.md` | The five-act presenter walkthrough with on-screen actions and lines. |

## Regenerating

    python3 -I demo/generate_timelines.py

The script reads noise from
`~/Library/CloudStorage/OneDrive-Personal/Timelines/client1.csv`. Edit `REF` at
the top if yours lives elsewhere. The seed is fixed, so reruns are identical;
change `SEED` for a fresh shuffle. Any row carrying one of the planted
indicators is filtered out of the noise, so a search on an indicator only ever
returns planted rows.

## Running the demo

    target/release/tlx demo/WKSTN-07_filesystem.csv

Then follow `DEMO_SCRIPT.md`.

---

## Answer key

The spoilers, so you can rehearse and confirm the data is behaving. Don't read
this out — it's the shape of the thing you're about to discover on camera.

### The indicators

| Indicator | Value |
|-----------|-------|
| Loader MD5 (all aliases) | `4d5e2b1a9c8f3e7d6a0b1c2d3e4f5a6b` |
| `config.dat` MD5 | `9f1e8d7c6b5a4039281716a5b4c3d2e1` |
| `p.ps1` MD5 | `3b8f7a2c1d9e04657b2a1f8c6d4e9a05` |
| Loader aliases | `invoice_scan.exe`, `svch0st.exe`, `spoolsvc.exe` |
| Staging directory | `C:\ProgramData\WinSvcHost\` |
| Persistence — Run key | `...\CurrentVersion\Run\WinSvcHost` → `spoolsvc.exe` |
| Persistence — task | `WinSvcHostUpdate` |
| Collection archives | `data.7z` (WKSTN-07), `db_backup.7z` (SRV-APP01) |
| Shared PE compile time | `2015-03-09 02:11:07Z` (same binary on both hosts) |
| Exfil target (no IOC — tag by hand) | `D:\MSSQL\DATA\CRM_prod.mdf`, `CRM_prod_log.ldf` |

### The pivot chain on WKSTN-07

1. `invoice_scan` → 4 rows, one lead → loader MD5.
2. Filter the MD5 → 9 rows, 4 paths: Downloads, AppData `svch0st.exe`,
   `WinSvcHost\spoolsvc.exe`, and `\\SRV-APP01\C$\...` (the lateral tell).
3. Filter `WinSvcHost` → the folder: four files at `14:19:1x`, two stragglers
   (`7za.exe` 14:46, `data.7z` 14:47). The gap is the pivot.
4. Right-click `14:47:33` → **Filter ±5 min** → ~53 rows holding the task, the
   Run key, the archiver, the collection script, and the copy to SRV-APP01.

### Timeline of the intrusion

| Time (2015) | Host | Event |
|-------------|------|-------|
| 03-10 14:18 | WKSTN-07 | `invoice_scan.exe` written to Downloads and run |
| 03-10 14:19 | WKSTN-07 | copied to `svch0st.exe` + `spoolsvc.exe`; `WinSvcHost` folder dropped |
| 03-10 14:45 | WKSTN-07 | `Run\WinSvcHost` key + `WinSvcHostUpdate` task created |
| 03-10 14:46–47 | WKSTN-07 | `7za.exe` and `data.7z` staged |
| 03-10 14:49 | WKSTN-07 → SRV-APP01 | loader copied to `\\SRV-APP01\C$\Windows\Temp` |
| 03-10 14:51 | SRV-APP01 | loader + persistence installed on the server |
| 03-10 15:17 | SRV-APP01 | `MSSQLSERVER` service stopped to release the DB file locks |
| 03-10 15:18–19 | SRV-APP01 | `CRM_prod.mdf` / `.ldf` read by `svc_sql`; `~db7z8f2.tmp` archive started |
| 03-10 15:39 | SRV-APP01 | `db_backup.7z` MACE'd; `~db7z8f2.tmp` deleted; `MSSQLSERVER` restarted (15:41) |
| 03-11 02:00 | SRV-APP01 | `WinSvcHostUpdate` task fires again |

### IOC coverage, on purpose

The list tags 23 of 24 planted rows on WKSTN-07 and 14 of 17 on SRV-APP01. What
it leaves behind is the teaching point:

- the bare `WinSvcHost` folder-creation row (the files inside it are tagged);
- the two `CRM_prod` database reads and the `MSSQLSERVER` stop/start pair — no
  shared hash, outside the staging directory, so the IOC can't reach them. The
  IOC *does* tag the two staged outputs in the folder, `~db7z8f2.tmp` and
  `db_backup.7z`. Those are the leads: a ±5 min pivot on the temp file's
  `15:19:05` creation surfaces the service stop and the DB reads (collection),
  and a ±5 min pivot on `db_backup.7z` at `15:39:48` surfaces the temp-file
  deletion and the service restart (packaging + cleanup). Tag the DB reads
  `exfil` and the service events `defense-evasion` by hand.

### A note on Field types

The server timeline carries two `Service/Stopped` / `Service/Started` rows
alongside the filesystem/registry/task events — service-control events that a
combined host timeline would include. Their summary mirrors the task format:
`Name: … Status: STOPPED|RUNNING Source: Service Control Manager User: …`.
