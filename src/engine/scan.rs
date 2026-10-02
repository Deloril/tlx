//! Parallel, cancellable row scan behind the filter and IOC runs.
//!
//! The single-threaded `View::apply` is fine for small files but on a
//! multi-GB timeline the scan dominates (seconds of CPU). This splits the row
//! range into one chunk per worker and scans them concurrently over a shared
//! `Index` (positional reads, no shared cursor), merging the per-chunk hit
//! lists back into ascending master order. A progress counter and a cancel
//! flag let the GUI run it off the UI thread with a progress bar and a Cancel
//! button.
//!
//! Worker count follows `available_parallelism()`, so a 4-core machine fans
//! out over ~4 workers and a 10-core box over ~10. Chunks are contiguous and
//! ascending, so concatenating results in chunk order is already sorted — no
//! merge sort needed.

use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

use super::index::Index;
use super::view::{FilterSpec, Overlay, View};
use super::adopt::AdoptedColumns;

/// How many rows a worker processes between cancel-flag checks and progress
/// bumps. Small enough that Cancel feels instant, large enough that the atomic
/// traffic is negligible.
const CHECK_EVERY: usize = 4096;

/// Outcome of a cancellable scan.
pub enum ScanOutcome {
    /// Completed: the matched master row indices, ascending.
    Done(Vec<usize>),
    /// The cancel flag was set before the scan finished.
    Cancelled,
}

/// chunk_count picks the worker count: the hardware parallelism, capped so a
/// tiny row set does not spawn more threads than it has work for.
fn worker_count(n_rows: usize) -> usize {
    let hw = std::thread::available_parallelism().map(|p| p.get()).unwrap_or(1);
    // At least ~50k rows per worker; no point splitting finer than that.
    let by_rows = n_rows.div_ceil(50_000).max(1);
    hw.min(by_rows).max(1)
}

/// parallel_filter scans every row of `idx` against `spec`, returning the
/// master indices that match (ascending). `ov` supplies the tags/comments/edits
/// the filter reads; it must be shareable across threads (pass a
/// SessionSnapshot, not the live Session). `progress` is bumped as rows are
/// scanned and `cancel`, when set, stops every worker promptly.
///
/// Returns Err only if the filter fails to compile (bad regex / query); an I/O
/// error on one chunk drops that chunk's remaining rows but does not fail the
/// whole scan, matching the single-threaded path's best-effort behaviour.
pub fn parallel_filter(
    idx: &Index,
    ov: &(dyn Overlay + Sync),
    spec: &FilterSpec,
    annot: AdoptedColumns,
    progress: &AtomicUsize,
    cancel: &AtomicBool,
) -> Result<ScanOutcome, String> {
    let n = idx.row_count();
    progress.store(0, Ordering::Relaxed);

    if spec.empty() {
        // No predicate: every row passes. Still honour cancellation and
        // progress so the caller's UI behaves uniformly.
        if cancel.load(Ordering::Relaxed) {
            return Ok(ScanOutcome::Cancelled);
        }
        progress.store(n, Ordering::Relaxed);
        return Ok(ScanOutcome::Done((0..n).collect()));
    }

    // Compile once up front to surface a bad query/regex as an error before
    // spawning any workers. Each worker recompiles its own (the RowPred
    // closures borrow their View), but this validates the spec.
    {
        let probe = make_view(idx, ov, spec, annot);
        probe.compile_filter()?;
    }

    let workers = worker_count(n);
    let chunk = n.div_ceil(workers);
    let mut ranges = Vec::new();
    let mut lo = 0;
    while lo < n {
        let hi = (lo + chunk).min(n);
        ranges.push((lo, hi));
        lo = hi;
    }

    let results: Vec<Result<Vec<usize>, String>> = std::thread::scope(|scope| {
        let handles: Vec<_> = ranges
            .iter()
            .map(|&(lo, hi)| {
                scope.spawn(move || scan_chunk(idx, ov, spec, annot, lo, hi, progress, cancel))
            })
            .collect();
        handles.into_iter().map(|h| h.join().unwrap()).collect()
    });

    if cancel.load(Ordering::Relaxed) {
        return Ok(ScanOutcome::Cancelled);
    }

    // Ranges are contiguous and ascending, so concatenation stays sorted.
    let mut matched = Vec::new();
    for r in results {
        matched.extend(r?);
    }
    Ok(ScanOutcome::Done(matched))
}

fn make_view<'a>(
    idx: &'a Index,
    ov: &'a (dyn Overlay + Sync),
    spec: &FilterSpec,
    annot: AdoptedColumns,
) -> View<'a> {
    let mut view = View::new(idx, ov);
    view.set_annotation_columns(annot);
    view.set_filter_spec(spec.clone());
    view
}

#[allow(clippy::too_many_arguments)]
fn scan_chunk(
    idx: &Index,
    ov: &(dyn Overlay + Sync),
    spec: &FilterSpec,
    annot: AdoptedColumns,
    lo: usize,
    hi: usize,
    progress: &AtomicUsize,
    cancel: &AtomicBool,
) -> Result<Vec<usize>, String> {
    let view = make_view(idx, ov, spec, annot);
    let cf = view.compile_filter()?;
    let mut hits = Vec::new();
    let mut since_check = 0usize;
    let mut stopped = false;
    let res = idx.scan_range(lo, hi, |i, rec| {
        if view.keep_compiled(i, rec, &cf) {
            hits.push(i);
        }
        since_check += 1;
        if since_check >= CHECK_EVERY {
            progress.fetch_add(since_check, Ordering::Relaxed);
            since_check = 0;
            if cancel.load(Ordering::Relaxed) {
                stopped = true;
                return false;
            }
        }
        true
    });
    if since_check > 0 {
        progress.fetch_add(since_check, Ordering::Relaxed);
    }
    // An I/O error mid-chunk returns what we matched so far (best effort), as
    // the serial path would on a short read; a bad-spec error can't happen
    // here since compile_filter already succeeded on the probe.
    let _ = res;
    let _ = stopped;
    Ok(hits)
}

/// parallel_ioc scans every row against a compiled IOC list, returning the
/// master indices that hit at least one indicator (ascending). Same threading
/// and cancellation model as parallel_filter; the predicates are recompiled
/// per worker from `body` so each borrows its own View.
pub fn parallel_ioc(
    idx: &Index,
    ov: &(dyn Overlay + Sync),
    body: &str,
    cased: bool,
    annot: AdoptedColumns,
    progress: &AtomicUsize,
    cancel: &AtomicBool,
) -> ScanOutcome {
    let n = idx.row_count();
    progress.store(0, Ordering::Relaxed);
    if n == 0 {
        return ScanOutcome::Done(Vec::new());
    }

    let workers = worker_count(n);
    let chunk = n.div_ceil(workers);
    let mut ranges = Vec::new();
    let mut lo = 0;
    while lo < n {
        let hi = (lo + chunk).min(n);
        ranges.push((lo, hi));
        lo = hi;
    }

    let results: Vec<Vec<usize>> = std::thread::scope(|scope| {
        let handles: Vec<_> = ranges
            .iter()
            .map(|&(lo, hi)| {
                scope.spawn(move || {
                    let mut view = View::new(idx, ov);
                    view.set_annotation_columns(annot);
                    let set = view.compile_iocs(body, cased);
                    if set.count() == 0 {
                        return Vec::new();
                    }
                    let mut hits = Vec::new();
                    let mut since_check = 0usize;
                    let _ = view.scan_iocs_range(&set, lo, hi, |i| {
                        hits.push(i);
                        since_check += 1;
                        if since_check >= CHECK_EVERY {
                            progress.fetch_add(since_check, Ordering::Relaxed);
                            since_check = 0;
                            if cancel.load(Ordering::Relaxed) {
                                return false;
                            }
                        }
                        true
                    });
                    progress.fetch_add(since_check, Ordering::Relaxed);
                    hits
                })
            })
            .collect();
        handles.into_iter().map(|h| h.join().unwrap()).collect()
    });

    if cancel.load(Ordering::Relaxed) {
        return ScanOutcome::Cancelled;
    }
    let mut matched = Vec::new();
    for r in results {
        matched.extend(r);
    }
    ScanOutcome::Done(matched)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::engine::index::Index;
    use crate::engine::view::{Overlay, View, COL_ALL};
    use std::sync::atomic::{AtomicBool, AtomicUsize};

    struct NoOverlay;
    impl Overlay for NoOverlay {
        fn tags(&self, _row: usize) -> Vec<String> {
            Vec::new()
        }
        fn comment(&self, _row: usize) -> String {
            String::new()
        }
        fn cell_override(&self, _row: usize, _col: usize) -> Option<String> {
            None
        }
    }

    // A memory index of n rows, column 0 = row number, column 1 = "even"/"odd".
    fn big_idx(n: usize) -> Index {
        let records: Vec<Vec<String>> = (0..n)
            .map(|i| vec![i.to_string(), if i % 2 == 0 { "even".into() } else { "odd".into() }])
            .collect();
        Index::new_memory(vec!["n".into(), "parity".into()], records)
    }

    // Serial reference: filter via View::apply over the same spec.
    fn serial(idx: &Index, spec: &FilterSpec) -> Vec<usize> {
        let ov = NoOverlay;
        let mut v = View::new(idx, &ov);
        v.apply(spec.clone()).unwrap();
        v.rows().to_vec()
    }

    fn spec_parity() -> FilterSpec {
        FilterSpec { expr: "parity=even".to_string(), column: COL_ALL, ..Default::default() }
    }

    #[test]
    fn parallel_matches_serial_many_workers() {
        // 300k rows forces several workers (>50k/worker) and exercises the
        // chunk-boundary concatenation.
        let idx = big_idx(300_000);
        let ov = NoOverlay;
        let spec = spec_parity();
        let progress = AtomicUsize::new(0);
        let cancel = AtomicBool::new(false);
        let out = parallel_filter(
            &idx,
            &ov,
            &spec,
            AdoptedColumns::none(),
            &progress,
            &cancel,
        )
        .unwrap();
        match out {
            ScanOutcome::Done(rows) => {
                assert_eq!(rows, serial(&idx, &spec));
                // Rows stay ascending across chunk joins.
                assert!(rows.windows(2).all(|w| w[0] < w[1]));
                assert_eq!(progress.load(Ordering::Relaxed), 300_000);
            }
            ScanOutcome::Cancelled => panic!("unexpected cancel"),
        }
    }

    #[test]
    fn parallel_empty_spec_returns_all() {
        let idx = big_idx(120_000);
        let ov = NoOverlay;
        let spec = FilterSpec::default();
        let progress = AtomicUsize::new(0);
        let cancel = AtomicBool::new(false);
        let out =
            parallel_filter(&idx, &ov, &spec, AdoptedColumns::none(), &progress, &cancel)
                .unwrap();
        match out {
            ScanOutcome::Done(rows) => assert_eq!(rows, (0..120_000).collect::<Vec<_>>()),
            ScanOutcome::Cancelled => panic!("unexpected cancel"),
        }
    }

    #[test]
    fn parallel_bad_regex_errors() {
        let idx = big_idx(60_000);
        let ov = NoOverlay;
        let spec = FilterSpec {
            query: "[".to_string(),
            regexp: true,
            column: 0,
            ..Default::default()
        };
        let progress = AtomicUsize::new(0);
        let cancel = AtomicBool::new(false);
        let r = parallel_filter(&idx, &ov, &spec, AdoptedColumns::none(), &progress, &cancel);
        assert!(r.is_err());
    }

    #[test]
    fn parallel_ioc_matches_serial() {
        let idx = big_idx(250_000);
        let ov = NoOverlay;
        let progress = AtomicUsize::new(0);
        let cancel = AtomicBool::new(false);
        // Indicator "odd" hits every odd-parity row's data column.
        let out = parallel_ioc(
            &idx,
            &ov,
            "odd\n",
            false,
            AdoptedColumns::none(),
            &progress,
            &cancel,
        );
        let got = match out {
            ScanOutcome::Done(rows) => rows,
            ScanOutcome::Cancelled => panic!("unexpected cancel"),
        };
        let want: Vec<usize> = (0..250_000).filter(|i| i % 2 == 1).collect();
        assert_eq!(got, want);
    }

    #[test]
    fn parallel_cancel_stops_early() {
        let idx = big_idx(400_000);
        let ov = NoOverlay;
        let spec = spec_parity();
        let progress = AtomicUsize::new(0);
        let cancel = AtomicBool::new(true); // pre-cancelled
        let out =
            parallel_filter(&idx, &ov, &spec, AdoptedColumns::none(), &progress, &cancel)
                .unwrap();
        assert!(matches!(out, ScanOutcome::Cancelled));
    }

    #[test]
    fn worker_count_scales_but_caps() {
        // Tiny row sets never over-split; large ones cap at hardware threads.
        assert_eq!(worker_count(0), 1);
        assert_eq!(worker_count(1), 1);
        assert_eq!(worker_count(49_999), 1);
        let hw = std::thread::available_parallelism().map(|p| p.get()).unwrap_or(1);
        assert!(worker_count(10_000_000) <= hw);
    }
}
