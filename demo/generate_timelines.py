#!/usr/bin/env python3
"""Build two synthetic forensic timelines for the tlx demo.

Run with an isolated interpreter so nothing is imported from the CWD:

    python3 -I demo/generate_timelines.py

It reads real, benign noise rows from the reference client1.csv, re-times a
large share of them into the demo window, and splices in a hand-built
two-host intrusion. Output (next to this script):

    WKSTN-07_filesystem.csv   host 1 - workstation, initial access
    SRV-APP01_filesystem.csv  host 2 - server, lateral target

Everything here is fabricated teaching data. The "malware" is inert strings.
"""

import csv
import os
import random
from datetime import datetime, timedelta

SEED = 1337
random.seed(SEED)

HERE = os.path.dirname(os.path.abspath(__file__))
REF = os.path.expanduser(
    "~/Library/CloudStorage/OneDrive-Personal/Timelines/client1.csv"
)
SCAN_ROWS = 350_000          # how much of the reference file to draw noise from
HOST1_NOISE = 11_900
HOST2_NOISE = 8_950

# ---------------------------------------------------------------------------
# The intrusion's indicators. Each was checked to be absent from the reference
# noise so a filter on any of them returns only the planted rows.
# ---------------------------------------------------------------------------
HASH_A = "4d5e2b1a9c8f3e7d6a0b1c2d3e4f5a6b"   # the loader, under all its names
HASH_B = "9f1e8d7c6b5a4039281716a5b4c3d2e1"   # config.dat
HASH_PS = "3b8f7a2c1d9e04657b2a1f8c6d4e9a05"  # p.ps1 collection script
HASH_7Z = "e2c4a6b8d0f1234567890abcdef01234"  # bundled archiver (legit tool)
PE_COMPILE = "2015-03-09 02:11:07Z"           # shared PE compile time, both hosts

STAGE = r"C:\ProgramData\WinSvcHost"
RUN_KEY = r"Microsoft\Windows\CurrentVersion\Run\WinSvcHost"
TASK = "WinSvcHostUpdate"

# Tokens that must never appear in a noise row, so indicators stay clean.
FORBIDDEN = [
    HASH_A, HASH_B, HASH_PS, "winsvchost", "svch0st", "spoolsvc",
    "invoice_scan", "data.7z", "db_backup.7z", "winsvchostupdate",
    "srv-app01", "wkstn-07", "jmorgan", "svc_sql", "crm_prod",
]


def filesum(path, md5, user):
    return f"Path: {path}   MD5: {md5}   User: {user}"


def regsum(path, typ, value, user):
    if value is None:
        return f"Path: {path}   Type: {typ}   User: {user}"
    return f"Path: {path}   Type: {typ}   Value: {value}   User: {user}"


def tasksum(name, status, creator):
    return f"Name: {name}   Status: {status}   MD5: [N/A]   Creator: {creator}"


def load_noise_pool():
    """Read (Field, Summary) pairs from the reference file, skipping any row
    that carries one of our indicators or has an empty summary."""
    pool = []
    with open(REF, newline="", encoding="utf-8", errors="replace") as f:
        r = csv.reader(f)
        next(r, None)  # header
        for i, row in enumerate(r):
            if i >= SCAN_ROWS:
                break
            if len(row) < 5:
                continue
            ts, field, summary = row[2], row[3], row[4]
            if not summary or not field:
                continue
            blob = (field + " " + summary).lower()
            if any(tok in blob for tok in FORBIDDEN):
                continue
            pool.append((ts, field, summary))
    if len(pool) < HOST1_NOISE + 1000:
        raise SystemExit(
            f"noise pool too small ({len(pool)}); check REF path: {REF}"
        )
    return pool


def retime(orig):
    """Give a noise row a demo-window timestamp. A fifth keep an authentic old
    MFT/PE time, most spread across Jan-Jun 2015, a fifth land on the incident
    day so the attack never sits alone in a time sort."""
    r = random.random()
    if r < 0.22:
        if orig and len(orig) >= 4 and orig[:4].isdigit():
            return orig
        return "2009-07-14 02:34:08Z"
    if r < 0.80:
        start = datetime(2015, 1, 1)
        span = int((datetime(2015, 6, 30) - start).total_seconds())
        return (start + timedelta(seconds=random.randint(0, span))).strftime(
            "%Y-%m-%d %H:%M:%SZ"
        )
    base = datetime(2015, 3, 10, 8, 0, 0)
    return (base + timedelta(seconds=random.randint(0, 10 * 3600))).strftime(
        "%Y-%m-%d %H:%M:%SZ"
    )


def noise_rows(pool, n):
    picks = random.sample(pool, n)
    return [(retime(ts), field, summary) for (ts, field, summary) in picks]


# ---------------------------------------------------------------------------
# Host 1 - WKSTN-07. Phishing lure -> renamed copies -> staging dir -> later
# archive + persistence + task -> lateral copy to SRV-APP01.
# ---------------------------------------------------------------------------
def host1_intrusion():
    u = r"WKSTN-07\jmorgan"
    sysu = r"NT AUTHORITY\SYSTEM"
    rows = []
    a = rows.append

    dl = r"C:\Users\jmorgan\Downloads\invoice_scan.exe"
    appd = r"C:\Users\jmorgan\AppData\Roaming\Microsoft\Windows\svch0st.exe"

    # Initial access: the lure is written and run.
    a(("2015-03-10 14:18:22Z", "File/Created", filesum(dl, HASH_A, u)))
    a(("2015-03-10 14:18:22Z", "File/Modified", filesum(dl, HASH_A, u)))
    a((PE_COMPILE, "File/PEInfo/PETimestamp", filesum(dl, HASH_A, u)))
    a(("2015-03-10 14:19:03Z", "File/Accessed", filesum(dl, HASH_A, u)))

    # Renamed copy #1 in the user profile (same hash, new name, new location).
    a(("2015-03-10 14:19:10Z", "File/Created", filesum(appd, HASH_A, u)))
    a(("2015-03-10 14:19:10Z", "File/Modified", filesum(appd, HASH_A, u)))

    # The staging directory and the files dropped into it, all ~14:19.
    a(("2015-03-10 14:19:12Z", "File/Created", filesum(STAGE, "[N/A]", sysu)))
    for name, h in [
        ("spoolsvc.exe", HASH_A),   # renamed copy #2 - the pivot target
        ("config.dat", HASH_B),
        ("p.ps1", HASH_PS),
        ("readme.txt", "[N/A]"),
    ]:
        p = STAGE + "\\" + name
        a(("2015-03-10 14:19:13Z", "File/Created", filesum(p, h, sysu)))
        a(("2015-03-10 14:19:13Z", "File/Modified", filesum(p, h, sysu)))

    # ~28 minutes later: the archiver and its output appear in the same dir -
    # the two files whose timestamp differs from the rest of the folder.
    sevenz = STAGE + r"\7za.exe"
    arc = STAGE + r"\data.7z"
    a(("2015-03-10 14:46:05Z", "File/Created", filesum(sevenz, HASH_7Z, sysu)))
    a(("2015-03-10 14:46:05Z", "File/Modified", filesum(sevenz, HASH_7Z, sysu)))
    a(("2015-03-10 14:47:33Z", "File/Created", filesum(arc, "[N/A]", sysu)))

    # Everything else inside the +-5 min window around 14:47:33.
    a(("2015-03-10 14:45:12Z", "Task/Trigger/Begin",
       tasksum(TASK, "SCHED_S_TASK_READY", sysu)))
    a(("2015-03-10 14:45:12Z", "Task/NextRunTime",
       tasksum(TASK, "SCHED_S_TASK_READY", sysu)))
    a(("2015-03-10 14:45:40Z", "Registry/Modified",
       regsum(RUN_KEY, "REG_SZ", STAGE + r"\spoolsvc.exe", u)))
    a(("2015-03-10 14:46:40Z", "File/Accessed",
       filesum(STAGE + r"\p.ps1", HASH_PS, sysu)))
    a(("2015-03-10 14:48:05Z", "File/Modified",
       filesum(STAGE + r"\config.dat", HASH_B, sysu)))

    # Lateral movement: the loader is copied to the server, and the mapped
    # drive is recorded. This is the thread that leads to host 2.
    lat = r"\\SRV-APP01\C$\Windows\Temp\spoolsvc.exe"
    a(("2015-03-10 14:49:20Z", "File/Accessed", filesum(lat, HASH_A, u)))
    a(("2015-03-10 14:49:22Z", "Registry/Modified",
       regsum(r"Software\Microsoft\Windows\CurrentVersion\Explorer\Map Network Drive MRU",
              "REG_SZ", r"\\SRV-APP01\C$", u)))
    return rows


# ---------------------------------------------------------------------------
# Host 2 - SRV-APP01. Inbound copy -> persistence + task -> SQL data access ->
# archive staged -> next-day task fire.
# ---------------------------------------------------------------------------
def host2_intrusion():
    u = r"SRV-APP01\svc_sql"
    sysu = r"NT AUTHORITY\SYSTEM"
    rows = []
    a = rows.append

    temp = r"C:\Windows\Temp\spoolsvc.exe"
    a(("2015-03-10 14:49:19Z", "File/Created", filesum(temp, HASH_A, u)))
    a(("2015-03-10 14:49:19Z", "File/Modified", filesum(temp, HASH_A, u)))
    a((PE_COMPILE, "File/PEInfo/PETimestamp", filesum(temp, HASH_A, u)))

    # Persistence installed on the server.
    a(("2015-03-10 14:51:30Z", "Registry/Modified",
       regsum(RUN_KEY, "REG_SZ", STAGE + r"\spoolsvc.exe", sysu)))
    a(("2015-03-10 14:51:45Z", "File/Created", filesum(STAGE, "[N/A]", sysu)))
    for name, h in [("spoolsvc.exe", HASH_A), ("config.dat", HASH_B)]:
        p = STAGE + "\\" + name
        a(("2015-03-10 14:51:46Z", "File/Created", filesum(p, h, sysu)))
        a(("2015-03-10 14:51:46Z", "File/Modified", filesum(p, h, sysu)))
    a(("2015-03-10 14:52:10Z", "Task/Trigger/Begin",
       tasksum(TASK, "SCHED_S_TASK_READY", sysu)))
    a(("2015-03-10 14:52:10Z", "Task/NextRunTime",
       tasksum(TASK, "SCHED_S_TASK_READY", sysu)))

    # Data theft. SQL holds the .mdf/.ldf open, so the attacker stops the
    # service to release the locks, copies the files, and kicks off an archive
    # into a temp working file. None of this carries a planted hash and only
    # the staged files sit in the IOC'd folder - the DB reads and the service
    # control events have to be found by pivoting and tagged by hand.
    def svc(status):
        return (f"Name: MSSQLSERVER   Status: {status}   "
                f"Source: Service Control Manager   User: {sysu}")

    a(("2015-03-10 15:17:40Z", "Service/Stopped", svc("STOPPED")))
    a(("2015-03-10 15:18:10Z", "File/Accessed",
       filesum(r"D:\MSSQL\DATA\CRM_prod.mdf", "[N/A]", u)))
    a(("2015-03-10 15:18:52Z", "File/Accessed",
       filesum(r"D:\MSSQL\DATA\CRM_prod_log.ldf", "[N/A]", u)))

    # Archiver temp file, created as compression starts (~= the stop time).
    tmp = STAGE + r"\~db7z8f2.tmp"
    a(("2015-03-10 15:19:05Z", "File/Created", filesum(tmp, "[N/A]", sysu)))
    a(("2015-03-10 15:19:05Z", "File/Modified", filesum(tmp, "[N/A]", sysu)))

    # ~20 min later: compression finishes, the output archive is fully MACEd,
    # the temp file is removed, and SQL is restarted to close the outage.
    arc = STAGE + r"\db_backup.7z"
    for fld in ("File/Created", "File/Modified", "File/Accessed", "File/Changed"):
        a(("2015-03-10 15:39:48Z", fld, filesum(arc, "[N/A]", sysu)))
    a(("2015-03-10 15:40:15Z", "File/Deleted", filesum(tmp, "[N/A]", sysu)))
    a(("2015-03-10 15:41:02Z", "Service/Started", svc("RUNNING")))

    # Next-day beacon: the scheduled task fires.
    a(("2015-03-11 02:00:11Z", "Task/NextRunTime",
       tasksum(TASK, "SCHED_S_TASK_READY", sysu)))
    a(("2015-03-11 02:00:12Z", "File/Accessed",
       filesum(STAGE + r"\spoolsvc.exe", HASH_A, sysu)))
    return rows


def write_csv(path, rows):
    rows = sorted(rows, key=lambda r: r[0])
    with open(path, "w", newline="", encoding="utf-8") as f:
        w = csv.writer(f, quoting=csv.QUOTE_ALL, lineterminator="\r\n")
        w.writerow(["Alert", "Tag", "Timestamp", "Field", "Summary"])
        for ts, field, summary in rows:
            w.writerow(["", "", ts, field, summary])
    return len(rows)


def main():
    print(f"reading noise from {REF}")
    pool = load_noise_pool()
    print(f"noise pool: {len(pool):,} usable rows")

    h1 = noise_rows(pool, HOST1_NOISE) + host1_intrusion()
    h2 = noise_rows(pool, HOST2_NOISE) + host2_intrusion()

    p1 = os.path.join(HERE, "WKSTN-07_filesystem.csv")
    p2 = os.path.join(HERE, "SRV-APP01_filesystem.csv")
    n1 = write_csv(p1, h1)
    n2 = write_csv(p2, h2)
    print(f"wrote {p1}  ({n1:,} rows)")
    print(f"wrote {p2}  ({n2:,} rows)")


if __name__ == "__main__":
    main()
