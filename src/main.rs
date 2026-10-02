// On Windows, link as a GUI app so launching it doesn't open a console window
// behind the main window. No effect on other platforms, and only in release so
// `cargo run` on Windows still shows debug output.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

//! tlx: a Rust/egui forensic CSV timeline viewer (the 2.0 port of the original
//! Go+Fyne tool).
//!
//! Two modes:
//!   tlx [file.csv]                   launch the GUI, optionally pre-opening a file
//!   tlx --bench <file.csv> [-q query] [-sort col]
//!                                     headless engine check, mirrors cmd/tlxcheck:
//!                                     prints index time, row count, filter time,
//!                                     sort time and a few sample rows. No GUI.

mod app;
mod engine;
mod prefs;

use engine::{ColumnRef, FilterSpec, Index, Session, SortKey, View, COL_ALL};

fn main() {
    let args: Vec<String> = std::env::args().collect();

    if args.len() > 1 && args[1] == "--bench" {
        run_bench(&args[2..]);
        return;
    }

    let open_file = args.get(1).cloned();

    let mut viewport = eframe::egui::ViewportBuilder::default();
    if let Some(icon) = load_icon() {
        viewport = viewport.with_icon(icon);
    }
    let native_options = eframe::NativeOptions {
        viewport,
        ..Default::default()
    };
    let result = eframe::run_native(
        "tlx-rust",
        native_options,
        Box::new(move |_cc| {
            let mut tlx_app = app::TlxApp::default();
            if let Some(path) = &open_file {
                tlx_app.open_path(path);
            }
            Ok(Box::new(tlx_app))
        }),
    );
    if let Err(e) = result {
        eprintln!("GUI error: {e}");
        std::process::exit(1);
    }
}

/// load_icon decodes the embedded app icon (the same PNG the Go/Fyne build
/// used) into the RGBA form eframe wants for the window, dock and taskbar.
/// Returns None if decoding fails, so a bad asset just drops the icon rather
/// than killing startup.
fn load_icon() -> Option<eframe::egui::IconData> {
    static ICON_PNG: &[u8] = include_bytes!("../assets/icon.png");
    let img = image::load_from_memory(ICON_PNG).ok()?.into_rgba8();
    let (width, height) = img.dimensions();
    Some(eframe::egui::IconData {
        rgba: img.into_raw(),
        width,
        height,
    })
}

/// Headless diagnostic, mirroring cmd/tlxcheck/main.go: open, report index
/// time/rows/memory, optionally filter and sort, print timings and a few
/// sample rows.
fn run_bench(args: &[String]) {
    let mut path: Option<String> = None;
    let mut query = String::new();
    let mut sort_col: Option<ColumnRef> = None;

    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "-q" => {
                i += 1;
                if i < args.len() {
                    query = args[i].clone();
                }
            }
            "-sort" => {
                i += 1;
                if i < args.len() {
                    sort_col = args[i].parse::<i32>().ok();
                }
            }
            other => path = Some(other.to_string()),
        }
        i += 1;
    }

    let Some(path) = path else {
        eprintln!("usage: tlx-rust --bench [-q substr] [-sort col] <file.csv>");
        std::process::exit(2);
    };

    let t0 = std::time::Instant::now();
    let idx = match Index::open(
        &path,
        Some(&mut |done, total| {
            eprint!("\rindexing {:3.0}%", 100.0 * done as f64 / total as f64);
        }),
    ) {
        Ok(idx) => idx,
        Err(e) => {
            eprintln!("\nopen: {e}");
            std::process::exit(1);
        }
    };
    eprintln!();
    let index_elapsed = t0.elapsed();

    println!("indexed {} rows in {:.0?}", idx.row_count(), index_elapsed);
    println!(
        "delimiter {:?}, columns: {:?}",
        idx.delimiter() as char,
        idx.headers()
    );
    print_mem();

    let session = Session::new(&path);
    let mut view = View::new(&idx, &session);

    if !query.is_empty() {
        let spec = FilterSpec {
            query: query.clone(),
            regexp: false,
            cased: false,
            column: COL_ALL,
            ..Default::default()
        };

        // Serial path (what the UI thread used to run).
        let t = std::time::Instant::now();
        if let Err(e) = view.apply(spec.clone()) {
            eprintln!("filter: {e}");
            std::process::exit(1);
        }
        let serial_elapsed = t.elapsed();
        println!(
            "filter(serial) {:?} -> {} rows in {:.0?}",
            query,
            view.len(),
            serial_elapsed
        );

        // Parallel path (what the GUI now runs off-thread). It reads
        // annotations through a Send+Sync snapshot, same as the GUI worker.
        let snap = session.snapshot();
        let progress = std::sync::atomic::AtomicUsize::new(0);
        let cancel = std::sync::atomic::AtomicBool::new(false);
        let t = std::time::Instant::now();
        let out = crate::engine::scan::parallel_filter(
            &idx,
            &snap,
            &spec,
            crate::engine::adopt::AdoptedColumns::none(),
            &progress,
            &cancel,
        );
        match out {
            Ok(crate::engine::scan::ScanOutcome::Done(rows)) => {
                let par_elapsed = t.elapsed();
                let speedup = serial_elapsed.as_secs_f64() / par_elapsed.as_secs_f64().max(1e-9);
                println!(
                    "filter(parallel) {:?} -> {} rows in {:.0?} ({:.1}x)",
                    query,
                    rows.len(),
                    par_elapsed,
                    speedup
                );
            }
            Ok(crate::engine::scan::ScanOutcome::Cancelled) => println!("filter(parallel): cancelled"),
            Err(e) => eprintln!("filter(parallel): {e}"),
        }
    }
    if let Some(col) = sort_col {
        let t = std::time::Instant::now();
        if let Err(e) = view.sort(vec![SortKey { col, desc: false }]) {
            eprintln!("sort: {e}");
            std::process::exit(1);
        }
        println!("sort by col {col} in {:.0?}", t.elapsed());
    }

    // Spot-check a few rows across the (possibly filtered/sorted) view.
    let n = view.len();
    for &p in &[0usize, n / 2, n.saturating_sub(1)] {
        if p >= n {
            continue;
        }
        match view.row(view.master(p)) {
            Ok(rec) => println!("row {p}: {:?}", truncate(&rec)),
            Err(e) => eprintln!("row: {e}"),
        }
    }
    print_mem();
}

fn truncate(rec: &[String]) -> Vec<String> {
    rec.iter()
        .map(|s| {
            if s.chars().count() > 40 {
                let t: String = s.chars().take(40).collect();
                format!("{t}\u{2026}")
            } else {
                s.clone()
            }
        })
        .collect()
}

/// Best-effort resident memory report. Rust's std has no runtime.ReadMemStats
/// equivalent, so this reads the OS-reported peak RSS via getrusage on
/// Unix. Values are not directly comparable to Go's HeapAlloc (RSS includes
/// the whole process, not just the Go heap) but give a real process-memory
/// number.
fn print_mem() {
    #[cfg(unix)]
    {
        use std::mem::MaybeUninit;
        unsafe {
            let mut usage = MaybeUninit::<RUsage>::zeroed();
            if getrusage(0, usage.as_mut_ptr()) == 0 {
                let usage = usage.assume_init();
                // macOS reports ru_maxrss in bytes; Linux in KB.
                #[cfg(target_os = "macos")]
                let mb = usage.ru_maxrss / (1024 * 1024);
                #[cfg(not(target_os = "macos"))]
                let mb = usage.ru_maxrss / 1024;
                println!("peak RSS: {mb} MB");
                return;
            }
        }
    }
    println!("peak RSS: (unavailable)");
}

#[cfg(unix)]
#[repr(C)]
struct RUsage {
    ru_utime: [i64; 2],
    ru_stime: [i64; 2],
    ru_maxrss: i64,
    ru_ixrss: i64,
    ru_idrss: i64,
    ru_isrss: i64,
    ru_minflt: i64,
    ru_majflt: i64,
    ru_nswap: i64,
    ru_inblock: i64,
    ru_oublock: i64,
    ru_msgsnd: i64,
    ru_msgrcv: i64,
    ru_nsignals: i64,
    ru_nvcsw: i64,
    ru_nivcsw: i64,
}

#[cfg(unix)]
unsafe extern "C" {
    fn getrusage(who: i32, usage: *mut RUsage) -> i32;
}
