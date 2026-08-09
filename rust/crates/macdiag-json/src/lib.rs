//! Read-only macOS diagnostics with a deliberately small, stable JSON shape.
//!
//! The command runner is injected so the parsers and report assembly can be
//! exercised on every host.  The binary itself refuses to run on non-macOS
//! hosts (exit code 2); tests can still use [`collect_report`] with fixtures.

use serde::Serialize;
use std::collections::BTreeMap;
use std::io::{self, Read};
use std::process::{Command, Stdio};
use std::thread;

/// The first version of the public JSON envelope.
pub const SCHEMA_VERSION: u32 = 1;
/// Maximum bytes retained for either stream of a probe.
pub const MAX_OUTPUT_BYTES: usize = 16 * 1024;
/// Marker appended when a probe stream had more data than the bound.
pub const TRUNCATION_MARKER: &str = "\n[output truncated at 16384 bytes]";

/// Output returned by an injected command runner.
///
/// A runner should cap its own reads where possible.  [`collect_report`] also
/// applies a cap, so fixture runners cannot accidentally create an unbounded
/// report.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CommandOutput {
    pub exit_code: Option<i32>,
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
}

impl CommandOutput {
    pub fn new(
        exit_code: Option<i32>,
        stdout: impl Into<Vec<u8>>,
        stderr: impl Into<Vec<u8>>,
    ) -> Self {
        Self {
            exit_code,
            stdout: stdout.into(),
            stderr: stderr.into(),
        }
    }
}

/// A process runner used by report collection.
pub trait CommandRunner {
    fn run(&mut self, program: &str, args: &[&str]) -> io::Result<CommandOutput>;
}

/// The real, non-shell command runner.  It never invokes sudo or mutates the
/// machine; all commands are fixed read-only macOS tools.
#[derive(Debug, Default, Clone, Copy)]
pub struct SystemCommandRunner;

impl CommandRunner for SystemCommandRunner {
    fn run(&mut self, program: &str, args: &[&str]) -> io::Result<CommandOutput> {
        let mut child = Command::new(program)
            .args(args)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()?;

        let stdout = child
            .stdout
            .take()
            .ok_or_else(|| io::Error::other("stdout pipe was not available"))?;
        let stderr = child
            .stderr
            .take()
            .ok_or_else(|| io::Error::other("stderr pipe was not available"))?;

        // Drain both pipes concurrently.  This prevents a noisy command from
        // deadlocking while still retaining only a bounded prefix.
        let stdout_thread = thread::spawn(|| read_bounded(stdout));
        let stderr_thread = thread::spawn(|| read_bounded(stderr));
        let status = child.wait()?;

        let stdout = stdout_thread
            .join()
            .map_err(|_| io::Error::other("stdout reader panicked"))??;
        let stderr = stderr_thread
            .join()
            .map_err(|_| io::Error::other("stderr reader panicked"))??;

        Ok(CommandOutput::new(status.code(), stdout, stderr))
    }
}

fn read_bounded<R: Read>(mut reader: R) -> io::Result<Vec<u8>> {
    // Leave room for the marker.  The loop still drains all input so the child
    // can exit cleanly even when a command is unexpectedly verbose.
    let prefix_limit = MAX_OUTPUT_BYTES.saturating_sub(TRUNCATION_MARKER.len());
    let mut retained = Vec::with_capacity(prefix_limit.min(8 * 1024));
    let mut truncated = false;
    let mut buffer = [0_u8; 8192];

    loop {
        let count = reader.read(&mut buffer)?;
        if count == 0 {
            break;
        }

        if retained.len() < prefix_limit {
            let keep = (prefix_limit - retained.len()).min(count);
            retained.extend_from_slice(&buffer[..keep]);
            if keep < count {
                truncated = true;
            }
        } else {
            truncated = true;
        }
    }

    if truncated {
        retained.extend_from_slice(TRUNCATION_MARKER.as_bytes());
    }
    Ok(retained)
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ProbeStatus {
    Ok,
    Failed,
    SpawnError,
}

#[derive(Debug, Clone, Serialize)]
pub struct ProbeResult {
    pub command: String,
    pub status: ProbeStatus,
    pub exit_code: Option<i32>,
    pub stdout: String,
    pub stderr: String,
    pub truncated: bool,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct PlatformInfo {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub version: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub build: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub kernel: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub architecture: Option<String>,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct HardwareInfo {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub model: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub machine: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub model_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub chip: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cpu_count: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub memory_bytes: Option<u64>,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct BatteryInfo {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub power_source: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub percentage: Option<u8>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub state: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub time_remaining_minutes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub is_present: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub is_charging: Option<bool>,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct MemoryInfo {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub page_size_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub total_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub free_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub active_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub inactive_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub wired_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub compressed_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub swapins: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub swapouts: Option<u64>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub pages: BTreeMap<String, u64>,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct StorageInfo {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mount_point: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub filesystem: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub total_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub used_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub available_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub capacity_percent: Option<u8>,
}

#[derive(Debug, Clone, Serialize)]
pub struct DiagnosticReport {
    pub schema_version: u32,
    pub generated_at: String,
    pub platform: PlatformInfo,
    pub hardware: HardwareInfo,
    pub battery: BatteryInfo,
    pub memory: MemoryInfo,
    pub storage: StorageInfo,
    pub probes: BTreeMap<String, ProbeResult>,
    pub warnings: Vec<String>,
}

#[derive(Debug)]
struct CapturedProbe {
    result: ProbeResult,
    stdout: String,
}

/// Collect all diagnostics with an injected runner.
pub fn collect_report<R: CommandRunner + ?Sized>(runner: &mut R) -> DiagnosticReport {
    let mut warnings = Vec::new();
    let mut probes = BTreeMap::new();

    let sw_vers = capture_probe(runner, "sw_vers", "sw_vers", &[], &mut warnings);
    let uname = capture_probe(runner, "uname", "uname", &["-srm"], &mut warnings);
    let sysctl = capture_probe(
        runner,
        "sysctl",
        "sysctl",
        &["-n", "hw.model", "hw.machine", "hw.ncpu", "hw.memsize"],
        &mut warnings,
    );
    let pmset = capture_probe(
        runner,
        "pmset_batt",
        "pmset",
        &["-g", "batt"],
        &mut warnings,
    );
    let vm_stat = capture_probe(runner, "vm_stat", "vm_stat", &[], &mut warnings);
    let df = capture_probe(runner, "df_root", "df", &["-Pk", "/"], &mut warnings);
    let profiler = capture_probe(
        runner,
        "system_profiler_hardware",
        "system_profiler",
        &["SPHardwareDataType", "-detailLevel", "mini"],
        &mut warnings,
    );

    for (name, captured) in [
        ("sw_vers", &sw_vers),
        ("uname", &uname),
        ("sysctl", &sysctl),
        ("pmset_batt", &pmset),
        ("vm_stat", &vm_stat),
        ("df_root", &df),
        ("system_profiler_hardware", &profiler),
    ] {
        probes.insert(name.to_owned(), captured.result.clone());
    }

    let mut platform = parse_sw_vers(&sw_vers.stdout);
    apply_uname(&mut platform, &uname.stdout);

    let mut hardware = parse_sysctl_hardware(&sysctl.stdout);
    merge_hardware(
        &mut hardware,
        parse_system_profiler_hardware(&profiler.stdout),
    );

    let mut memory = parse_vm_stat(&vm_stat.stdout);
    memory.total_bytes = hardware.memory_bytes;

    DiagnosticReport {
        schema_version: SCHEMA_VERSION,
        generated_at: chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Secs, true),
        platform,
        hardware,
        battery: parse_pmset_batt(&pmset.stdout),
        memory,
        storage: parse_df_pk(&df.stdout),
        probes,
        warnings,
    }
}

fn capture_probe<R: CommandRunner + ?Sized>(
    runner: &mut R,
    name: &str,
    program: &str,
    args: &[&str],
    warnings: &mut Vec<String>,
) -> CapturedProbe {
    let command = format_command(program, args);
    match runner.run(program, args) {
        Ok(output) => {
            let (stdout, stdout_truncated) = redact_and_bound(&output.stdout);
            let (stderr, stderr_truncated) = redact_and_bound(&output.stderr);
            let truncated = stdout_truncated || stderr_truncated;
            if stdout_truncated {
                warnings.push(format!(
                    "probe `{name}` stdout was truncated at {MAX_OUTPUT_BYTES} bytes"
                ));
            }
            if stderr_truncated {
                warnings.push(format!(
                    "probe `{name}` stderr was truncated at {MAX_OUTPUT_BYTES} bytes"
                ));
            }

            let status = if output.exit_code == Some(0) {
                ProbeStatus::Ok
            } else {
                warnings.push(format!(
                    "probe `{name}` failed with exit code {}",
                    output
                        .exit_code
                        .map_or_else(|| "unknown".to_owned(), |code| code.to_string())
                ));
                ProbeStatus::Failed
            };

            CapturedProbe {
                result: ProbeResult {
                    command,
                    status,
                    exit_code: output.exit_code,
                    stdout: stdout.clone(),
                    stderr,
                    truncated,
                },
                stdout,
            }
        }
        Err(error) => {
            let error_text = redact_and_bound(error.to_string().as_bytes()).0;
            warnings.push(format!("probe `{name}` could not run: {error_text}"));
            CapturedProbe {
                result: ProbeResult {
                    command,
                    status: ProbeStatus::SpawnError,
                    exit_code: None,
                    stdout: String::new(),
                    stderr: error_text,
                    truncated: false,
                },
                stdout: String::new(),
            }
        }
    }
}

fn format_command(program: &str, args: &[&str]) -> String {
    let mut command = program.to_owned();
    for arg in args {
        command.push(' ');
        if arg.chars().all(|character| {
            character.is_ascii_alphanumeric() || matches!(character, '-' | '_' | '/' | '.')
        }) {
            command.push_str(arg);
        } else {
            command.push('\'');
            command.push_str(&arg.replace('\'', "'\\''"));
            command.push('\'');
        }
    }
    command
}

fn redact_and_bound(bytes: &[u8]) -> (String, bool) {
    let input = String::from_utf8_lossy(bytes);
    let mut redacted = String::with_capacity(input.len().min(MAX_OUTPUT_BYTES));

    for (index, line) in input.lines().enumerate() {
        if index > 0 {
            redacted.push('\n');
        }
        let lower = line.to_ascii_lowercase();
        if [
            "serial number",
            "serialnumber",
            "hardware uuid",
            "platform uuid",
            "product uuid",
            "uuid",
            "hostname",
            "host name",
            "user name",
            "username",
            "home directory",
            "login",
            "owner",
        ]
        .iter()
        .any(|marker| lower.contains(marker))
        {
            redacted.push_str("[redacted]");
        } else {
            redacted.push_str(&redact_id_tokens(&redact_user_path(line)));
        }
    }
    if input.ends_with('\n') {
        redacted.push('\n');
    }

    let (bounded, truncated) = truncate_text(&redacted);
    (bounded, truncated || redacted.contains(TRUNCATION_MARKER))
}

fn redact_user_path(line: &str) -> String {
    let mut result = String::with_capacity(line.len());
    let mut rest = line;
    while let Some(position) = rest.find("/Users/") {
        result.push_str(&rest[..position + "/Users/".len()]);
        let after_prefix = &rest[position + "/Users/".len()..];
        let end = after_prefix
            .find(|character: char| character == '/' || character.is_whitespace())
            .unwrap_or(after_prefix.len());
        result.push_str("[redacted]");
        rest = &after_prefix[end..];
    }
    result.push_str(rest);
    result
}

fn redact_id_tokens(line: &str) -> String {
    let bytes = line.as_bytes();
    let mut result = String::with_capacity(line.len());
    let mut index = 0;

    while index < bytes.len() {
        if is_standalone_id_start(bytes, index) {
            let value_start = index + 3;
            let mut end = value_start;
            while end < bytes.len() && !is_id_value_delimiter(bytes[end]) {
                end += 1;
            }
            if end > value_start {
                result.push_str("[redacted]");
                index = end;
                continue;
            }
        }

        let Some(character) = line[index..].chars().next() else {
            break;
        };
        result.push(character);
        index += character.len_utf8();
    }

    result
}

fn is_standalone_id_start(bytes: &[u8], index: usize) -> bool {
    if index + 3 > bytes.len() || &bytes[index..index + 3] != b"id=" {
        return false;
    }
    index == 0 || !matches!(bytes[index - 1], b'a'..=b'z' | b'A'..=b'Z' | b'0'..=b'9' | b'_' | b'-')
}

fn is_id_value_delimiter(byte: u8) -> bool {
    byte.is_ascii_whitespace() || matches!(byte, b')' | b']' | b'}' | b',' | b';' | b':')
}

fn truncate_text(text: &str) -> (String, bool) {
    if text.len() <= MAX_OUTPUT_BYTES {
        return (text.to_owned(), false);
    }
    let prefix_limit = MAX_OUTPUT_BYTES.saturating_sub(TRUNCATION_MARKER.len());
    let mut end = prefix_limit.min(text.len());
    while end > 0 && !text.is_char_boundary(end) {
        end -= 1;
    }
    let mut bounded = text[..end].to_owned();
    bounded.push_str(TRUNCATION_MARKER);
    (bounded, true)
}

/// Parse `sw_vers`'s `Key: Value` lines.
pub fn parse_sw_vers(input: &str) -> PlatformInfo {
    let mut platform = PlatformInfo::default();
    for line in input.lines() {
        let Some((key, value)) = line.split_once(':') else {
            continue;
        };
        let value = nonempty(value);
        match key.trim().to_ascii_lowercase().as_str() {
            "productname" => platform.name = value,
            "productversion" => platform.version = value,
            "buildversion" => platform.build = value,
            _ => {}
        }
    }
    platform
}

fn apply_uname(platform: &mut PlatformInfo, input: &str) {
    let fields: Vec<&str> = input.split_whitespace().collect();
    if fields.len() >= 2 && platform.kernel.is_none() {
        platform.kernel = Some(fields[1].to_owned());
    }
    if fields.len() >= 3 && platform.architecture.is_none() {
        platform.architecture = Some(fields[2].to_owned());
    }
}

/// Parse the fixed order emitted by `sysctl -n hw.model hw.machine hw.ncpu hw.memsize`.
/// Also accepts labelled `key: value` fixtures for easier unit testing.
pub fn parse_sysctl_hardware(input: &str) -> HardwareInfo {
    let mut hardware = HardwareInfo::default();
    let mut positional = Vec::new();
    for line in input.lines().map(str::trim).filter(|line| !line.is_empty()) {
        if let Some((key, value)) = line.split_once(':') {
            assign_sysctl_value(&mut hardware, key.trim(), value.trim());
        } else {
            positional.push(line);
        }
    }

    if hardware.model.is_none() {
        hardware.model = positional.first().and_then(|value| nonempty(value));
    }
    if hardware.machine.is_none() {
        hardware.machine = positional.get(1).and_then(|value| nonempty(value));
    }
    if hardware.cpu_count.is_none() {
        hardware.cpu_count = positional.get(2).and_then(|value| parse_u64(value));
    }
    if hardware.memory_bytes.is_none() {
        hardware.memory_bytes = positional.get(3).and_then(|value| parse_u64(value));
    }
    hardware
}

fn assign_sysctl_value(hardware: &mut HardwareInfo, key: &str, value: &str) {
    let key = key.to_ascii_lowercase();
    match key.trim_start_matches("hw.") {
        "model" => hardware.model = nonempty(value),
        "machine" => hardware.machine = nonempty(value),
        "ncpu" => hardware.cpu_count = parse_u64(value),
        "memsize" => hardware.memory_bytes = parse_u64(value),
        _ => {}
    }
}

/// Parse safe fields from `system_profiler SPHardwareDataType`.
pub fn parse_system_profiler_hardware(input: &str) -> HardwareInfo {
    let mut hardware = HardwareInfo::default();
    for line in input.lines() {
        let Some((key, value)) = line.split_once(':') else {
            continue;
        };
        let key = key.trim().to_ascii_lowercase();
        let value = value.trim();
        if key == "chip" || key == "chip name" {
            hardware.chip = nonempty(value);
        } else if key == "model name" {
            hardware.model_name = nonempty(value);
        } else if key == "model identifier" && hardware.model.is_none() {
            hardware.model = nonempty(value);
        } else if key == "total number of cores" {
            hardware.cpu_count = value
                .split_whitespace()
                .find_map(parse_u64)
                .or(hardware.cpu_count);
        } else if key == "memory" && hardware.memory_bytes.is_none() {
            hardware.memory_bytes = parse_memory_size(value);
        }
    }
    hardware
}

fn merge_hardware(base: &mut HardwareInfo, overlay: HardwareInfo) {
    if base.model.is_none() {
        base.model = overlay.model;
    }
    if base.machine.is_none() {
        base.machine = overlay.machine;
    }
    if base.model_name.is_none() {
        base.model_name = overlay.model_name;
    }
    if base.chip.is_none() {
        base.chip = overlay.chip;
    }
    if base.cpu_count.is_none() {
        base.cpu_count = overlay.cpu_count;
    }
    if base.memory_bytes.is_none() {
        base.memory_bytes = overlay.memory_bytes;
    }
}

/// Parse `pmset -g batt` without exposing battery identifiers.
pub fn parse_pmset_batt(input: &str) -> BatteryInfo {
    let mut battery = BatteryInfo::default();
    for line in input.lines() {
        let trimmed = line.trim();
        if let Some(source) = trimmed.strip_prefix("Now drawing from '")
            && let Some(end) = source.find('\'')
        {
            battery.power_source = nonempty(&source[..end]);
        }
        let Some(percent_end) = trimmed.find('%') else {
            continue;
        };
        let percent_start = trimmed[..percent_end]
            .rfind(|character: char| !character.is_ascii_digit())
            .map_or(0, |index| index + 1);
        battery.percentage = trimmed[percent_start..percent_end].parse::<u8>().ok();

        let tail = trimmed[percent_end + 1..].trim_start_matches(';');
        let fields: Vec<&str> = tail.split(';').map(str::trim).collect();
        // The first field is often `AC attached`; scan all fields for the
        // actual battery state.  If a fixture exposes more than one known
        // state, the later field is the most specific/current one.
        let mut state = None;
        for field in fields {
            let Some(field) = nonempty(field) else {
                continue;
            };
            let Some((recognized_state, is_charging)) = parse_battery_state(&field) else {
                continue;
            };
            state = Some((recognized_state.to_owned(), is_charging));
        }
        if let Some((state, is_charging)) = state {
            battery.state = Some(state);
            battery.is_charging = Some(is_charging);
        }
        if let Some(present) = find_value(trimmed, "present:") {
            battery.is_present = parse_bool(present);
        }
        if let Some(remaining) = trimmed.split_once("remaining").map(|(before, _)| before) {
            let candidate = remaining.split_whitespace().next_back().unwrap_or_default();
            battery.time_remaining_minutes = parse_hhmm_minutes(candidate);
        }
    }
    battery
}

fn parse_battery_state(value: &str) -> Option<(&'static str, bool)> {
    let value = value.trim().to_ascii_lowercase();
    [
        ("not charging", false),
        ("finishing charge", true),
        ("charging", true),
        ("finishing", true),
        ("charged", false),
        ("discharging", false),
    ]
    .into_iter()
    .find(|(state, _)| {
        value == *state
            || value
                .strip_prefix(state)
                .is_some_and(|suffix| suffix.chars().next().is_some_and(char::is_whitespace))
    })
}

/// Parse page counts from `vm_stat`, converting the useful counters to bytes.
pub fn parse_vm_stat(input: &str) -> MemoryInfo {
    let mut memory = MemoryInfo::default();
    for line in input.lines() {
        let lower = line.to_ascii_lowercase();
        if let Some(page_size) = lower
            .split("page size of")
            .nth(1)
            .and_then(|part| part.split_whitespace().next())
            .and_then(parse_u64)
        {
            memory.page_size_bytes = Some(page_size);
        }

        let Some((label, raw_value)) = line.split_once(':') else {
            continue;
        };
        let Some(value) = parse_u64(raw_value) else {
            continue;
        };
        let normalized = label
            .trim()
            .trim_start_matches("Pages ")
            .to_ascii_lowercase()
            .replace(' ', "_");
        if label.trim().to_ascii_lowercase().starts_with("pages ") {
            memory.pages.insert(normalized.clone(), value);
            let bytes = memory
                .page_size_bytes
                .and_then(|size| value.checked_mul(size));
            match normalized.as_str() {
                "free" => memory.free_bytes = bytes,
                "active" => memory.active_bytes = bytes,
                "inactive" => memory.inactive_bytes = bytes,
                "wired_down" => memory.wired_bytes = bytes,
                "occupied_by_compressor" | "compressed" => memory.compressed_bytes = bytes,
                _ => {}
            }
        } else if normalized == "swapins" {
            memory.swapins = Some(value);
        } else if normalized == "swapouts" {
            memory.swapouts = Some(value);
        }
    }

    // vm_stat normally puts the page-size header before counters.  Recompute
    // bytes if a fixture put the header after its counters.
    if let Some(size) = memory.page_size_bytes {
        memory.free_bytes = page_bytes(&memory.pages, "free", size);
        memory.active_bytes = page_bytes(&memory.pages, "active", size);
        memory.inactive_bytes = page_bytes(&memory.pages, "inactive", size);
        memory.wired_bytes = page_bytes(&memory.pages, "wired_down", size);
        memory.compressed_bytes = memory
            .pages
            .get("occupied_by_compressor")
            .or_else(|| memory.pages.get("compressed"))
            .and_then(|pages| pages.checked_mul(size));
    }
    memory
}

fn page_bytes(pages: &BTreeMap<String, u64>, key: &str, page_size: u64) -> Option<u64> {
    pages
        .get(key)
        .and_then(|pages| pages.checked_mul(page_size))
}

/// Parse `df -Pk /`; values are converted from 1024-byte blocks to bytes.
pub fn parse_df_pk(input: &str) -> StorageInfo {
    for line in input.lines() {
        let fields: Vec<&str> = line.split_whitespace().collect();
        if fields.len() < 6 || fields[0].eq_ignore_ascii_case("filesystem") {
            continue;
        }
        let Some(total_blocks) = parse_u64(fields[1]) else {
            continue;
        };
        let used_blocks = parse_u64(fields[2]);
        let available_blocks = parse_u64(fields[3]);
        let capacity_percent = fields[4].trim_end_matches('%').parse::<u8>().ok();
        let mount_point = fields[5..].join(" ");
        return StorageInfo {
            mount_point: nonempty(&mount_point),
            filesystem: nonempty(fields[0]),
            total_bytes: total_blocks.checked_mul(1024),
            used_bytes: used_blocks.and_then(|value| value.checked_mul(1024)),
            available_bytes: available_blocks.and_then(|value| value.checked_mul(1024)),
            capacity_percent,
        };
    }
    StorageInfo::default()
}

fn parse_memory_size(value: &str) -> Option<u64> {
    let mut fields = value.split_whitespace();
    let number = fields.next()?.parse::<f64>().ok()?;
    let unit = fields.next().unwrap_or("B").to_ascii_lowercase();
    let multiplier = match unit.as_str() {
        "b" | "bytes" => 1_f64,
        "kb" | "kib" => 1024_f64,
        "mb" | "mib" => 1024_f64.powi(2),
        "gb" | "gib" => 1024_f64.powi(3),
        "tb" | "tib" => 1024_f64.powi(4),
        _ => return None,
    };
    (number * multiplier).round().to_u64_checked()
}

trait ToU64Checked {
    fn to_u64_checked(self) -> Option<u64>;
}

impl ToU64Checked for f64 {
    fn to_u64_checked(self) -> Option<u64> {
        if self.is_finite() && self >= 0.0 && self <= u64::MAX as f64 {
            Some(self as u64)
        } else {
            None
        }
    }
}

fn parse_u64(value: &str) -> Option<u64> {
    value
        .trim()
        .trim_end_matches('.')
        .replace(',', "")
        .parse::<u64>()
        .ok()
}

fn parse_hhmm_minutes(value: &str) -> Option<u64> {
    let (hours, minutes) = value.split_once(':')?;
    parse_u64(hours)?
        .checked_mul(60)?
        .checked_add(parse_u64(minutes)?)
}

fn parse_bool(value: &str) -> Option<bool> {
    match value.trim().to_ascii_lowercase().as_str() {
        "true" | "yes" | "1" => Some(true),
        "false" | "no" | "0" => Some(false),
        _ => None,
    }
}

fn find_value<'a>(input: &'a str, key: &str) -> Option<&'a str> {
    input
        .to_ascii_lowercase()
        .find(&key.to_ascii_lowercase())
        .map(|index| &input[index + key.len()..])
}

fn nonempty(value: &str) -> Option<String> {
    let value = value.trim();
    (!value.is_empty()).then(|| value.to_owned())
}

/// Return one section as a JSON value, preserving the same field names as the
/// full report.  `all` is accepted as an explicit synonym for no filtering.
/// Unknown sections return `Ok(None)`; serialization failures remain visible as
/// `Err` instead of being silently treated as unknown sections.
pub fn section_value(
    report: &DiagnosticReport,
    section: &str,
) -> Result<Option<serde_json::Value>, serde_json::Error> {
    let value = match section.to_ascii_lowercase().as_str() {
        "all" => serde_json::to_value(report),
        "platform" => serde_json::to_value(&report.platform),
        "hardware" => serde_json::to_value(&report.hardware),
        "battery" => serde_json::to_value(&report.battery),
        "memory" => serde_json::to_value(&report.memory),
        "storage" => serde_json::to_value(&report.storage),
        "probes" => serde_json::to_value(&report.probes),
        "warnings" => serde_json::to_value(&report.warnings),
        _ => return Ok(None),
    }?;
    Ok(Some(value))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_pmset_fixture() {
        let value = parse_pmset_batt(
            "Now drawing from 'AC Power'\n -InternalBattery-0 (id=123)\t95%; charging; 1:23 remaining; present: true\n",
        );
        assert_eq!(value.power_source.as_deref(), Some("AC Power"));
        assert_eq!(value.percentage, Some(95));
        assert_eq!(value.state.as_deref(), Some("charging"));
        assert_eq!(value.time_remaining_minutes, Some(83));
        assert_eq!(value.is_present, Some(true));
        assert_eq!(value.is_charging, Some(true));
    }

    #[test]
    fn parses_pmset_not_charging_after_power_source_field() {
        let value = parse_pmset_batt(
            "Now drawing from 'AC Power'\n -InternalBattery-0 (id=123)\t80%; AC attached; not charging present: true\n",
        );
        assert_eq!(value.state.as_deref(), Some("not charging"));
        assert_eq!(value.is_charging, Some(false));
    }

    #[test]
    fn parses_vm_stat_fixture() {
        let value = parse_vm_stat(
            "Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages free: 10.\nPages active: 20.\nPages inactive: 30.\nPages wired down: 40.\nPages occupied by compressor: 5.\nSwapins: 7.\nSwapouts: 8.\n",
        );
        assert_eq!(value.page_size_bytes, Some(4096));
        assert_eq!(value.free_bytes, Some(40_960));
        assert_eq!(value.active_bytes, Some(81_920));
        assert_eq!(value.wired_bytes, Some(163_840));
        assert_eq!(value.compressed_bytes, Some(20_480));
        assert_eq!(value.swapins, Some(7));
    }

    #[test]
    fn parses_df_fixture() {
        let value = parse_df_pk(
            "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/disk3s1 100000 25000 75000 25% /\n",
        );
        assert_eq!(value.mount_point.as_deref(), Some("/"));
        assert_eq!(value.total_bytes, Some(102_400_000));
        assert_eq!(value.used_bytes, Some(25_600_000));
        assert_eq!(value.available_bytes, Some(76_800_000));
        assert_eq!(value.capacity_percent, Some(25));
    }

    #[test]
    fn redacts_private_values_and_bounds_output() {
        let fixture =
            b"Serial Number (system): secret\nUser Name: someone\npath=/Users/someone/Documents\n";
        let (value, truncated) = redact_and_bound(fixture);
        assert!(!value.contains("secret"));
        assert!(!value.contains("someone"));
        assert!(!truncated);

        let huge = vec![b'x'; MAX_OUTPUT_BYTES + 100];
        let (value, truncated) = redact_and_bound(&huge);
        assert!(truncated);
        assert!(value.len() <= MAX_OUTPUT_BYTES);
        assert!(value.contains(TRUNCATION_MARKER));

        let (value, truncated) = redact_and_bound(TRUNCATION_MARKER.as_bytes());
        assert!(truncated);
        assert!(value.contains(TRUNCATION_MARKER));
    }

    #[derive(Default)]
    struct FakeRunner {
        outputs: BTreeMap<String, io::Result<CommandOutput>>,
    }

    impl CommandRunner for FakeRunner {
        fn run(&mut self, program: &str, _args: &[&str]) -> io::Result<CommandOutput> {
            self.outputs
                .remove(program)
                .unwrap_or_else(|| Err(io::Error::new(io::ErrorKind::NotFound, "fixture missing")))
        }
    }

    #[test]
    fn redacts_battery_id_in_probe_output_without_losing_battery_data() {
        let mut runner = FakeRunner::default();
        runner.outputs.insert(
            "pmset".to_owned(),
            Ok(CommandOutput::new(
                Some(0),
                "Now drawing from 'AC Power'\n -InternalBattery-0 (id=23396451)\t80%; AC attached; not charging present: true\n",
                "",
            )),
        );

        let report = collect_report(&mut runner);
        let probe = report
            .probes
            .get("pmset_batt")
            .expect("pmset probe is present");
        assert!(!probe.stdout.contains("23396451"));
        assert!(probe.stdout.contains("[redacted]"));
        assert_eq!(report.battery.percentage, Some(80));
        assert_eq!(report.battery.state.as_deref(), Some("not charging"));
        assert_eq!(report.battery.is_charging, Some(false));
    }

    #[test]
    fn failures_become_warnings_and_json_remains_valid() {
        let mut runner = FakeRunner::default();
        runner
            .outputs
            .insert("sw_vers".to_owned(), Err(io::Error::other("not found")));
        let report = collect_report(&mut runner);
        assert!(!report.warnings.is_empty());
        let json = serde_json::to_value(&report).expect("report serializes");
        for key in [
            "schema_version",
            "generated_at",
            "platform",
            "hardware",
            "battery",
            "memory",
            "storage",
            "probes",
            "warnings",
        ] {
            assert!(json.get(key).is_some(), "missing {key}");
        }
    }

    #[test]
    fn section_value_distinguishes_known_all_and_unknown() {
        let report = collect_report(&mut FakeRunner::default());
        assert!(
            section_value(&report, "battery")
                .expect("battery serializes")
                .is_some()
        );
        assert!(
            section_value(&report, "all")
                .expect("full report serializes")
                .is_some()
        );
        assert!(
            section_value(&report, "not-a-section")
                .expect("unknown section has no serialization error")
                .is_none()
        );
    }
}
