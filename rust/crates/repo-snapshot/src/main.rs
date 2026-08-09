use std::path::PathBuf;
use std::process;

use clap::Parser;
use repo_snapshot::{
    DEFAULT_MAX_DEPTH, DEFAULT_MAX_FILES, SnapshotError, SnapshotOptions, render_human,
    render_json, snapshot,
};

#[derive(Debug, Parser)]
#[command(
    name = "repo-snapshot",
    about = "Create a bounded, read-only snapshot of a repository"
)]
struct Cli {
    /// Repository directory to inspect. Defaults to the current directory.
    #[arg(value_name = "PATH", default_value = ".")]
    path: PathBuf,

    /// Emit compact JSON instead of human-readable output.
    #[arg(long)]
    json: bool,

    /// Emit pretty-printed JSON (implies --json).
    #[arg(long)]
    pretty: bool,

    /// Maximum number of total tree entries (directories and file-like entries).
    #[arg(long, default_value_t = DEFAULT_MAX_FILES, value_name = "N")]
    max_files: usize,

    /// Maximum tree depth; direct children have depth 1.
    #[arg(long, default_value_t = DEFAULT_MAX_DEPTH, value_name = "N")]
    max_depth: usize,
}

fn main() {
    if let Err(error) = run() {
        eprintln!("repo-snapshot: {error}");
        process::exit(1);
    }
}

fn run() -> Result<(), SnapshotError> {
    let cli = Cli::parse();
    let value = snapshot(
        &cli.path,
        SnapshotOptions {
            max_files: cli.max_files,
            max_depth: cli.max_depth,
        },
    )?;

    if cli.pretty || cli.json {
        println!("{}", render_json(&value, cli.pretty)?);
    } else {
        print!("{}", render_human(&value));
    }
    Ok(())
}
