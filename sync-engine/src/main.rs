//! `dubarr-sync` — the Dubarr dubbing-sync CLI.
//!
//! Reads one job JSON object from stdin, prints one result JSON object to
//! stdout. `offset_ms = t_target - t_donor`. Exit codes: 0 success, 2 usage
//! error, 3 analysis failure. `--selftest` runs a built-in synthetic check.

use std::io::Read;

use clap::Parser;
use dubarr_sync::{run_job, selftest, Job, SyncError};

/// Compute the timing offset between a target and a donor audio slice.
#[derive(Debug, Parser)]
#[command(name = "dubarr-sync", version, about)]
struct Cli {
    /// Run the built-in synthetic cross-correlation check and exit.
    #[arg(long)]
    selftest: bool,
}

fn execute(cli: &Cli) -> Result<dubarr_sync::SyncResult, SyncError> {
    if cli.selftest {
        return selftest();
    }
    let mut input = String::new();
    std::io::stdin()
        .read_to_string(&mut input)
        .map_err(|e| SyncError::InvalidJob(format!("cannot read stdin: {e}")))?;
    let job: Job = serde_json::from_str(&input)?;
    run_job(&job)
}

fn main() {
    let cli = Cli::parse();
    match execute(&cli) {
        Ok(result) => match serde_json::to_string(&result) {
            Ok(json) => println!("{json}"),
            Err(e) => {
                eprintln!("dubarr-sync: cannot encode result: {e}");
                std::process::exit(3);
            }
        },
        Err(error) => {
            eprintln!("dubarr-sync: {error}");
            std::process::exit(error.exit_code());
        }
    }
}
