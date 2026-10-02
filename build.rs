//! Build script: on Windows, compile the app icon into the executable so
//! Explorer, the taskbar and Alt-Tab show it. On every other platform this is
//! a no-op. The GUI subsystem (no console window) is set via the
//! `#![windows_subsystem = "windows"]` attribute in main.rs, not here.

fn main() {
    #[cfg(target_os = "windows")]
    {
        let mut res = winres::WindowsResource::new();
        res.set_icon("packaging/windows/tlx.ico");
        if let Err(e) = res.compile() {
            // Don't fail the whole build if the resource compiler is missing;
            // the exe just ships without an embedded icon.
            println!("cargo:warning=icon embed skipped: {e}");
        }
    }
}
