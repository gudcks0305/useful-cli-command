use std::path::PathBuf;

use clap::Parser;

use codex_history::{DEFAULT_LIMIT, MAX_LIMIT, ScanOptions, generate_report};

#[derive(Debug, Parser)]
#[command(
    name = "codex-history",
    version,
    about = "Summarize local Codex history metadata"
)]
struct Cli {
    /// Codex data directory. Defaults to $CODEX_HOME or $HOME/.codex.
    #[arg(long, value_name = "PATH")]
    root: Option<PathBuf>,

    /// Maximum number of entries to print.
    #[arg(
        long,
        default_value_t = DEFAULT_LIMIT,
        value_name = "N",
        value_parser = parse_limit
    )]
    limit: usize,

    /// Include sessions active during the last N days.
    #[arg(long, value_name = "DAYS")]
    since_days: Option<u64>,

    /// Emit the versioned JSON report.
    #[arg(long)]
    json: bool,

    /// Pretty-print JSON. Implies --json.
    #[arg(long)]
    pretty: bool,
}

fn parse_limit(value: &str) -> Result<usize, String> {
    let limit = value
        .parse::<usize>()
        .map_err(|_| "limit must be an integer".to_owned())?;
    if (1..=MAX_LIMIT).contains(&limit) {
        Ok(limit)
    } else {
        Err(format!("limit must be between 1 and {MAX_LIMIT}"))
    }
}

fn main() {
    let cli = Cli::parse();
    let options = match ScanOptions::from_root_arg(cli.root, cli.limit, cli.since_days) {
        Ok(options) => options,
        Err(error) => {
            eprintln!("codex-history: {error}");
            std::process::exit(1);
        }
    };

    let report = match generate_report(&options) {
        Ok(report) => report,
        Err(error) => {
            eprintln!("codex-history: {error}");
            std::process::exit(1);
        }
    };

    if cli.json || cli.pretty {
        let rendered = if cli.pretty {
            serde_json::to_string_pretty(&report)
        } else {
            serde_json::to_string(&report)
        };
        match rendered {
            Ok(value) => println!("{value}"),
            Err(error) => {
                eprintln!("codex-history: failed to encode report: {error}");
                std::process::exit(1);
            }
        }
    } else {
        print!("{}", report.render_human());
    }
}
