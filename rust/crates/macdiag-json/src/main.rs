#[cfg(target_os = "macos")]
use clap::Parser;
#[cfg(target_os = "macos")]
use macdiag_json::{SystemCommandRunner, collect_report, section_value};

#[cfg(target_os = "macos")]
#[derive(Debug, Parser)]
#[command(name = "macdiag-json", about = "Read-only macOS diagnostics as JSON")]
struct Cli {
    /// Pretty-print the JSON output.
    #[arg(long)]
    pretty: bool,

    /// Emit one section instead of the complete stable envelope.
    #[arg(long, value_name = "SECTION")]
    section: Option<String>,
}

fn main() {
    #[cfg(not(target_os = "macos"))]
    {
        eprintln!("macdiag-json: unsupported platform (requires macOS)");
        std::process::exit(2);
    }

    #[cfg(target_os = "macos")]
    {
        let cli = Cli::parse();
        let mut runner = SystemCommandRunner;
        let report = collect_report(&mut runner);

        let value = match cli.section.as_deref() {
            Some(section) => match section_value(&report, section) {
                Ok(Some(value)) => value,
                Ok(None) => {
                    eprintln!(
                        "macdiag-json: unknown section `{section}` (use all, platform, hardware, battery, memory, storage, probes, or warnings)"
                    );
                    std::process::exit(2);
                }
                Err(error) => {
                    eprintln!("macdiag-json: failed to serialize section `{section}`: {error}");
                    std::process::exit(1);
                }
            },
            None => match serde_json::to_value(&report) {
                Ok(value) => value,
                Err(error) => {
                    eprintln!("macdiag-json: failed to serialize report: {error}");
                    std::process::exit(1);
                }
            },
        };

        let serialized = if cli.pretty {
            serde_json::to_string_pretty(&value)
        } else {
            serde_json::to_string(&value)
        };
        match serialized {
            Ok(json) => println!("{json}"),
            Err(error) => {
                eprintln!("macdiag-json: failed to serialize report: {error}");
                std::process::exit(1);
            }
        }
    }
}
