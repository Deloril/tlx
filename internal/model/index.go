// Package model holds the CSV engine behind the Timeline explorer: on-disk
// indexing of arbitrarily large delimited files, a filtered/sorted view over
// them, and a session layer for tags, comments and cell edits.
//
// Nothing in this package depends on the GUI, so it builds and tests without a
// display or any C toolchain.
package model

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sync"
)

// candidate delimiters, in the order we prefer them on a tie.
var delimCandidates = []byte{',', '\t', ';', '|'}

// Index is a read-only, random-access view of a delimited file. It holds one
// int64 per record (byte offsets) rather than the record contents, so memory
// stays proportional to the row count, not the file size. A 50M-row file costs
// ~400 MB of offsets and opens without reading rows into memory.
type Index struct {
	path    string
	delim   byte
	headers []string
	bomLen  int64

	// bounds has len == recordCount+1. Record k occupies bytes
	// [bounds[k], bounds[k+1]). Record 0 is the header; data record i is
	// record i+1. The final element is the file size (a terminator).
	bounds []int64

	mu    sync.Mutex // guards f and cache during random reads
	f     *os.File
	cache *rowCache

	// mem holds records for an in-memory index (see NewMemoryIndex). When
	// non-nil the file fields above are unused.
	mem [][]string
}

// NewMemoryIndex builds an index backed by in-memory records rather than a file.
// It is used for synthetic tables such as the master timeline, so the same view,
// filter, sort and export machinery works over rows assembled in memory. Row
// copies are returned so callers may keep them; Close is a no-op.
func NewMemoryIndex(headers []string, records [][]string) *Index {
	return &Index{
		path:    "(memory)",
		delim:   ',',
		headers: headers,
		mem:     records,
	}
}

// Open indexes path with a single sequential pass. The onProgress callback (may
// be nil) is called periodically with bytes scanned and total, so a caller can
// show progress on a multi-GB file.
func Open(path string, onProgress func(done, total int64)) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := st.Size()

	idx := &Index{path: path, f: f, cache: newRowCache(8192)}

	// Detect and skip a UTF-8 BOM.
	head := make([]byte, 3)
	n, _ := io.ReadFull(f, head)
	if n == 3 && head[0] == 0xEF && head[1] == 0xBB && head[2] == 0xBF {
		idx.bomLen = 3
	}
	if _, err := f.Seek(idx.bomLen, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}

	bounds, err := scanBounds(f, idx.bomLen, size, onProgress)
	if err != nil {
		f.Close()
		return nil, err
	}
	if len(bounds) < 2 {
		f.Close()
		return nil, fmt.Errorf("%s: no records found", path)
	}
	idx.bounds = bounds

	// Header record is bounds[0]..bounds[1]. Detect delimiter from it, then
	// parse it into column names.
	headerBytes, err := idx.readSpan(bounds[0], bounds[1])
	if err != nil {
		f.Close()
		return nil, err
	}
	idx.delim = detectDelim(headerBytes)
	hdr, err := parseRecord(headerBytes, idx.delim)
	if err != nil {
		f.Close()
		return nil, err
	}
	idx.headers = hdr
	return idx, nil
}

// scanBounds walks the file once, recording the start offset of every record.
// It is quote-aware: newlines and delimiters inside "..." fields do not end a
// record, and "" is treated as an escaped quote. This is what lets it index
// forensic CSVs whose message fields contain embedded newlines.
func scanBounds(f *os.File, start, size int64, onProgress func(done, total int64)) ([]int64, error) {
	br := bufio.NewReaderSize(f, 1<<20)
	bounds := make([]int64, 0, 1024)
	off := start
	bounds = append(bounds, off) // first record starts here

	var inQuotes, pendingEscape bool
	var sinceReport int64
	for {
		b, err := br.ReadByte()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		off++
		sinceReport++
		if sinceReport >= 1<<22 { // ~4 MB
			if onProgress != nil {
				onProgress(off-start, size-start)
			}
			sinceReport = 0
		}

		if inQuotes {
			if pendingEscape {
				pendingEscape = false
				if b == '"' {
					continue // "" -> literal quote, stay in field
				}
				inQuotes = false // the prior quote closed the field
				// fall through and reprocess b in unquoted state
			} else if b == '"' {
				pendingEscape = true
				continue
			} else {
				continue // ordinary byte inside a quoted field
			}
		}

		// unquoted state
		switch b {
		case '"':
			inQuotes = true
		case '\n':
			bounds = append(bounds, off)
		}
	}

	// Drop a trailing empty record if the file ended with a newline.
	if n := len(bounds); n > 0 && bounds[n-1] == off {
		bounds = bounds[:n-1]
	}
	bounds = append(bounds, size) // terminator
	if onProgress != nil {
		onProgress(size-start, size-start)
	}
	return bounds, nil
}

func detectDelim(header []byte) byte {
	best := byte(',')
	bestCount := -1
	for _, d := range delimCandidates {
		if c := countOutsideQuotes(header, d); c > bestCount {
			best, bestCount = d, c
		}
	}
	return best
}

func countOutsideQuotes(b []byte, delim byte) int {
	var inQuotes, pendingEscape bool
	count := 0
	for _, c := range b {
		if inQuotes {
			if pendingEscape {
				pendingEscape = false
				if c == '"' {
					continue
				}
				inQuotes = false
			} else if c == '"' {
				pendingEscape = true
				continue
			} else {
				continue
			}
		}
		if c == '"' {
			inQuotes = true
		} else if c == delim {
			count++
		}
	}
	return count
}

func parseRecord(raw []byte, delim byte) ([]string, error) {
	r := csv.NewReader(bytes.NewReader(raw))
	r.Comma = rune(delim)
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	r.ReuseRecord = false
	rec, err := r.Read()
	if err != nil && err != io.EOF {
		return nil, err
	}
	return rec, nil
}

// Headers returns the column names from the first record.
func (idx *Index) Headers() []string { return idx.headers }

// Delimiter is the byte used to separate fields.
func (idx *Index) Delimiter() byte { return idx.delim }

// Path is the source file path.
func (idx *Index) Path() string { return idx.path }

// RowCount is the number of data records (header excluded).
func (idx *Index) RowCount() int {
	if idx.mem != nil {
		return len(idx.mem)
	}
	return len(idx.bounds) - 2
}

// Row returns data record i (0-based, header excluded). The returned slice is a
// fresh copy safe for the caller to keep. Results are cached.
func (idx *Index) Row(i int) ([]string, error) {
	if i < 0 || i >= idx.RowCount() {
		return nil, fmt.Errorf("row %d out of range [0,%d)", i, idx.RowCount())
	}
	if idx.mem != nil {
		rec := idx.mem[i]
		out := make([]string, len(rec))
		copy(out, rec)
		return out, nil
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if rec, ok := idx.cache.get(i); ok {
		return rec, nil
	}
	raw, err := idx.readSpanLocked(idx.bounds[i+1], idx.bounds[i+2])
	if err != nil {
		return nil, err
	}
	rec, err := parseRecord(trimEOL(raw), idx.delim)
	if err != nil {
		return nil, err
	}
	idx.cache.put(i, rec)
	return rec, nil
}

func trimEOL(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))
	b = bytes.TrimSuffix(b, []byte("\r"))
	return b
}

func (idx *Index) readSpan(start, end int64) ([]byte, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.readSpanLocked(start, end)
}

func (idx *Index) readSpanLocked(start, end int64) ([]byte, error) {
	if end < start {
		return nil, fmt.Errorf("bad span [%d,%d)", start, end)
	}
	buf := make([]byte, end-start)
	if _, err := idx.f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

// Scan streams every data record sequentially from the start of the file. This
// is far faster than calling Row in a loop (no per-row seek) and is what filter
// and sort-key extraction use for a full pass. fn returns false to stop early.
func (idx *Index) Scan(fn func(i int, rec []string) bool) error {
	if idx.mem != nil {
		for i, rec := range idx.mem {
			if !fn(i, rec) {
				return nil
			}
		}
		return nil
	}
	f, err := os.Open(idx.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(idx.bomLen, io.SeekStart); err != nil {
		return err
	}
	r := csv.NewReader(f)
	r.Comma = rune(idx.delim)
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	r.ReuseRecord = true

	// Skip the header.
	if _, err := r.Read(); err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	i := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if !fn(i, rec) {
			return nil
		}
		i++
	}
}

// Close releases the underlying file handle.
func (idx *Index) Close() error {
	if idx.mem != nil {
		return nil
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.f != nil {
		err := idx.f.Close()
		idx.f = nil
		return err
	}
	return nil
}
