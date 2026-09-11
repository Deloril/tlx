// Command gentestdata writes a synthetic forensic-timeline CSV for exercising
// the engine on large inputs. Usage: gentestdata <rows> <path>.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
)

var (
	sources = []string{"EventLogs", "MFT", "Prefetch", "Registry", "Amcache", "SRUM", "ShellBags"}
	hosts   = []string{"WKSTN-01", "WKSTN-02", "DC-01", "SRV-FILE", "SRV-SQL", "LAPTOP-CFO"}
	users   = []string{"jsmith", "admin", "svc_backup", "rlopez", "SYSTEM", "attacker"}
	events  = []string{
		"User logon", "Process created: powershell.exe -enc <redacted>",
		"File created", "Registry value set", "Scheduled task registered",
		"Service installed", "Network share mapped", "Account created",
	}
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: gentestdata <rows> <path>")
		os.Exit(2)
	}
	n, err := strconv.Atoi(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad row count:", err)
		os.Exit(2)
	}
	f, err := os.Create(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	defer w.Flush()

	w.WriteString("Timestamp,SourceType,HostName,UserName,EventId,Message\n")
	base := int64(1704067200) // 2024-01-01T00:00:00Z
	for i := 0; i < n; i++ {
		ts := base + int64(i)
		src := sources[i%len(sources)]
		host := hosts[(i/3)%len(hosts)]
		user := users[(i/7)%len(users)]
		eid := 4000 + (i % 800)
		msg := events[i%len(events)]
		// Every 500th row gets a quoted field with a comma and a newline, so
		// the indexer's quote handling is exercised at scale.
		if i%500 == 0 {
			msg = fmt.Sprintf("%q", "multi-line detail, with comma\nand a second line for row "+strconv.Itoa(i))
		}
		fmt.Fprintf(w, "%s,%s,%s,%s,%d,%s\n", isoTime(ts), src, host, user, eid, msg)
	}
}

// isoTime formats a unix second as a UTC RFC3339-ish stamp without importing
// time (keeps output deterministic and dependency-free).
func isoTime(sec int64) string {
	days := sec / 86400
	rem := sec % 86400
	h := rem / 3600
	m := (rem % 3600) / 60
	s := rem % 60
	y, mo, d := civilFromDays(days)
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02dZ", y, mo, d, h, m, s)
}

// civilFromDays converts days since the unix epoch to a calendar date using
// Howard Hinnant's algorithm.
func civilFromDays(z int64) (int64, int64, int64) {
	z += 719468
	era := z / 146097
	if z < 0 {
		era = (z - 146096) / 146097
	}
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}
