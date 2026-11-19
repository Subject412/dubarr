//! `dubarr-sync` — the Dubarr dubbing-sync CLI.
//!
//! M1 scaffold: the binary only reports its identity. Slice loading (hound),
//! cross-correlation (rustfft) and the JSON contract arrive in a later task.
//!
//! Contract reminder: `offset_ms = t_target - t_donor`.

use clap::Parser;

/// Compute the timing offset between a target and a donor audio slice.
#[derive(Debug, Parser)]
#[command(name = "dubarr-sync", version, about)]
struct Cli {}

fn main() {
    let _cli = Cli::parse();
    println!("{} {}", env!("CARGO_PKG_NAME"), env!("CARGO_PKG_VERSION"));
}
