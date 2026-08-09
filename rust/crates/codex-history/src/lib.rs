//! Read-only summary of the two small Codex history indexes.
//!
//! This crate intentionally never opens the 'sessions/' rollout tree. The
//! rollout files can be very large; the compact index files contain everything
//! needed for this summary.

use std::collections::HashMap;
use std::env;
use std::ffi::OsString;
use std::fmt::{self, Display, Formatter};
use std::fs::{self, File};
use std::io::{self, BufRead, BufReader};
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use chrono::{DateTime, SecondsFormat, TimeZone, Utc};
use serde::{Deserialize, Serialize};
use serde_json::Value;

pub const SCHEMA_VERSION: u32 = 1;
pub const DEFAULT_LIMIT: usize = 50;
pub const MAX_LIMIT: usize = 10_000;
const MAX_WARNINGS: usize = 32;
const MAX_LINE_BYTES: usize = 4 * 1024 * 1024;
const MAX_METADATA_TEXT_CHARS: usize = 256;
const MAX_SESSIONS: usize = 100_000;

/// Command options. root_explicit distinguishes a missing --root from a
/// missing default directory.
#[derive(Debug, Clone)]
pub struct ScanOptions {
    pub root: PathBuf,
    pub root_explicit: bool,
    pub limit: usize,
    pub since_days: Option<u64>,
    pub now: SystemTime,
}

impl ScanOptions {
    pub fn from_root_arg(
        root: Option<PathBuf>,
        limit: usize,
        since_days: Option<u64>,
    ) -> Result<Self, ReportError> {
        match root {
            Some(path) => Ok(Self {
                root: path,
                root_explicit: true,
                limit: limit.min(MAX_LIMIT),
                since_days,
                now: SystemTime::now(),
            }),
            None => Ok(Self {
                root: default_root()?,
                root_explicit: false,
                limit: limit.min(MAX_LIMIT),
                since_days,
                now: SystemTime::now(),
            }),
        }
    }

    #[cfg(test)]
    fn at(root: PathBuf, now: SystemTime) -> Self {
        Self {
            root,
            root_explicit: true,
            limit: DEFAULT_LIMIT,
            since_days: None,
            now,
        }
    }
}

/// Versioned, privacy-safe report emitted by the command.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct Report {
    pub schema_version: u32,
    pub generated_at: String,
    pub source_path: String,
    pub entries: Vec<Entry>,
    pub warnings: Vec<String>,
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct Entry {
    pub session_id: String,
    pub title: String,
    pub updated_at: Option<String>,
    pub prompt_count: u64,
    pub last_activity: Option<String>,
}

#[derive(Debug)]
pub enum ReportError {
    DefaultRootUnavailable,
    MissingExplicitRoot(PathBuf),
    ExplicitRootNotDirectory(PathBuf),
    ExplicitRootMetadata { path: PathBuf, reason: &'static str },
}

impl Display for ReportError {
    fn fmt(&self, formatter: &mut Formatter<'_>) -> fmt::Result {
        match self {
            Self::DefaultRootUnavailable => write!(
                formatter,
                "cannot resolve default Codex root: set CODEX_HOME or HOME, or pass --root"
            ),
            Self::MissingExplicitRoot(path) => write!(
                formatter,
                "explicit root does not exist: {}",
                display_path(path)
            ),
            Self::ExplicitRootNotDirectory(path) => write!(
                formatter,
                "explicit root is not a directory: {}",
                display_path(path)
            ),
            Self::ExplicitRootMetadata { path, reason } => write!(
                formatter,
                "cannot inspect explicit root {} ({reason})",
                display_path(path)
            ),
        }
    }
}

impl std::error::Error for ReportError {}

/// Resolve the default Codex data directory without touching the filesystem.
pub fn default_root() -> Result<PathBuf, ReportError> {
    resolve_default_root(env::var_os("CODEX_HOME"), env::var_os("HOME"))
}

fn resolve_default_root(
    codex_home: Option<OsString>,
    home: Option<OsString>,
) -> Result<PathBuf, ReportError> {
    if let Some(value) = codex_home
        && !value.is_empty()
    {
        return Ok(PathBuf::from(value));
    }

    if let Some(value) = home
        && !value.is_empty()
    {
        return Ok(PathBuf::from(value).join(".codex"));
    }

    Err(ReportError::DefaultRootUnavailable)
}

/// Scan only the two compact JSONL indexes and build a report.
pub fn generate_report(options: &ScanOptions) -> Result<Report, ReportError> {
    validate_root(options)?;

    let mut warnings = WarningCollector::default();
    let mut sessions: HashMap<String, SessionAccumulator> = HashMap::new();
    let index_path = options.root.join("session_index.jsonl");
    let history_path = options.root.join("history.jsonl");

    scan_session_index(&index_path, &mut sessions, &mut warnings);
    scan_history(&history_path, &mut sessions, &mut warnings);

    let limit = options.limit.min(MAX_LIMIT);
    if options.limit > MAX_LIMIT {
        warnings.push(format!(
            "limit capped at {MAX_LIMIT} entries for bounded output"
        ));
    }

    let cutoff = options.since_days.and_then(|days| {
        options
            .now
            .duration_since(UNIX_EPOCH)
            .ok()
            .and_then(|duration| i64::try_from(duration.as_secs()).ok())
            .and_then(|now| {
                let span = days.saturating_mul(86_400);
                i64::try_from(span)
                    .ok()
                    .map(|span| now.saturating_sub(span))
            })
    });

    let mut missing_activity = 0usize;
    let mut entries: Vec<(Entry, Option<i64>)> = sessions
        .into_iter()
        .map(|(session_id, session)| session.into_entry(session_id))
        .filter_map(|(entry, activity)| match cutoff {
            Some(_) if activity.is_none() => {
                missing_activity = missing_activity.saturating_add(1);
                None
            }
            Some(cutoff) if activity.is_some_and(|value| value < cutoff) => None,
            Some(_) | None => Some((entry, activity)),
        })
        .collect();

    entries.sort_by(|(left, left_activity), (right, right_activity)| {
        right_activity
            .cmp(left_activity)
            .then_with(|| left.session_id.cmp(&right.session_id))
    });
    if entries.len() > limit {
        entries.truncate(limit);
    }

    if options.since_days.is_some() && missing_activity > 0 {
        warnings.push(format!(
            "{missing_activity} entries omitted because activity timestamp was unavailable"
        ));
    }
    if options.since_days.is_some() && cutoff.is_none() {
        warnings.push(
            "since-days could not be evaluated because the clock is before the Unix epoch"
                .to_owned(),
        );
    }
    let report_warnings = warnings.finish();

    Ok(Report {
        schema_version: SCHEMA_VERSION,
        generated_at: system_time_to_rfc3339(options.now),
        source_path: display_path(&options.root),
        entries: entries.into_iter().map(|(entry, _)| entry).collect(),
        warnings: report_warnings,
    })
}

impl Report {
    /// Render a compact human-readable report. Prompt text is deliberately
    /// absent; only metadata from the two index files is displayed.
    pub fn render_human(&self) -> String {
        let mut output = String::new();
        output.push_str("codex-history\n");
        output.push_str(&format!("schema: v{}\n", self.schema_version));
        output.push_str(&format!("source: {}\n", self.source_path));
        output.push_str(&format!("generated: {}\n", self.generated_at));
        output.push_str(&format!("entries: {}\n", self.entries.len()));

        for entry in &self.entries {
            let title = if entry.title.is_empty() {
                "(untitled)"
            } else {
                &entry.title
            };
            let updated = entry.updated_at.as_deref().unwrap_or("-");
            let last = entry.last_activity.as_deref().unwrap_or("-");
            output.push_str(&format!(
                "- {} | {} | updated {} | prompts {} | last {}\n",
                entry.session_id, title, updated, entry.prompt_count, last
            ));
        }

        if !self.warnings.is_empty() {
            output.push_str("warnings:\n");
            for warning in &self.warnings {
                output.push_str(&format!("- {warning}\n"));
            }
        }
        output
    }
}

#[derive(Debug, Default)]
struct WarningCollector {
    warnings: Vec<String>,
    omitted: usize,
    session_cap_warning_emitted: bool,
}

impl WarningCollector {
    fn push(&mut self, message: impl Into<String>) {
        if self.warnings.len() < MAX_WARNINGS {
            self.warnings.push(message.into());
        } else {
            self.omitted = self.omitted.saturating_add(1);
        }
    }

    fn finish(mut self) -> Vec<String> {
        if self.omitted > 0 {
            let summary = format!("{} additional warnings omitted", self.omitted);
            if self.warnings.len() == MAX_WARNINGS {
                self.warnings.pop();
            }
            self.warnings.push(summary);
        }
        self.warnings
    }

    fn session_cap_reached(&mut self) {
        if !self.session_cap_warning_emitted {
            self.push(format!(
                "session accumulator limit ({MAX_SESSIONS}) reached; new sessions ignored"
            ));
            self.session_cap_warning_emitted = true;
        }
    }
}

#[derive(Debug, Default)]
struct SessionAccumulator {
    title: String,
    updated_at: Option<String>,
    updated_epoch: Option<i64>,
    prompt_count: u64,
    last_activity: Option<ParsedTimestamp>,
}

impl SessionAccumulator {
    fn merge_index(&mut self, title: Option<String>, updated: Option<ParsedTimestamp>) {
        let should_replace = match (self.updated_epoch, updated.as_ref().and_then(|v| v.epoch)) {
            (Some(current), Some(next)) => next >= current,
            (None, Some(_)) => true,
            (Some(_), None) => false,
            (None, None) => true,
        };

        if should_replace {
            if let Some(title) = title
                && !title.is_empty()
            {
                self.title = title;
            }
            if let Some(updated) = updated {
                self.updated_at = Some(updated.raw);
                self.updated_epoch = updated.epoch;
            }
        } else if self.title.is_empty()
            && let Some(title) = title
        {
            self.title = title;
        }
    }

    fn add_history(&mut self, timestamp: Option<ParsedTimestamp>) {
        self.prompt_count = self.prompt_count.saturating_add(1);
        let Some(timestamp) = timestamp else { return };
        let replace = match (&self.last_activity, timestamp.epoch) {
            (None, _) => true,
            (Some(current), Some(next)) => current.epoch.is_none_or(|value| next >= value),
            (Some(_), None) => false,
        };
        if replace {
            self.last_activity = Some(timestamp);
        }
    }

    fn into_entry(self, session_id: String) -> (Entry, Option<i64>) {
        let activity = match (
            self.updated_epoch,
            self.last_activity.as_ref().and_then(|v| v.epoch),
        ) {
            (Some(updated), Some(last)) => Some(updated.max(last)),
            (Some(updated), None) => Some(updated),
            (None, Some(last)) => Some(last),
            (None, None) => None,
        };
        let updated_at = self.updated_at.or_else(|| {
            self.last_activity
                .as_ref()
                .map(|timestamp| timestamp.raw.clone())
        });
        let last_activity = self.last_activity.map(|timestamp| timestamp.raw);
        (
            Entry {
                session_id,
                title: self.title,
                updated_at,
                prompt_count: self.prompt_count,
                last_activity,
            },
            activity,
        )
    }
}

#[derive(Debug, Clone)]
struct ParsedTimestamp {
    raw: String,
    epoch: Option<i64>,
}

#[derive(Debug, Deserialize)]
struct HistoryRecord {
    #[serde(default, alias = "id", alias = "thread_id")]
    session_id: Option<String>,
    #[serde(default)]
    ts: Option<TimestampInput>,
    #[serde(default)]
    timestamp: Option<TimestampInput>,
}

#[derive(Debug, Deserialize)]
#[serde(untagged)]
enum TimestampInput {
    Number(serde_json::Number),
    Text(String),
}

fn validate_root(options: &ScanOptions) -> Result<(), ReportError> {
    if !options.root_explicit {
        return Ok(());
    }
    match fs::metadata(&options.root) {
        Ok(metadata) if metadata.is_dir() => Ok(()),
        Ok(_) => Err(ReportError::ExplicitRootNotDirectory(options.root.clone())),
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            Err(ReportError::MissingExplicitRoot(options.root.clone()))
        }
        Err(error) => Err(ReportError::ExplicitRootMetadata {
            path: options.root.clone(),
            reason: io_error_kind(&error),
        }),
    }
}

fn session_for<'a>(
    sessions: &'a mut HashMap<String, SessionAccumulator>,
    session_id: String,
    warnings: &mut WarningCollector,
) -> Option<&'a mut SessionAccumulator> {
    if sessions.contains_key(&session_id) {
        return sessions.get_mut(&session_id);
    }
    if sessions.len() >= MAX_SESSIONS {
        warnings.session_cap_reached();
        return None;
    }
    Some(sessions.entry(session_id).or_default())
}

fn scan_session_index(
    path: &Path,
    sessions: &mut HashMap<String, SessionAccumulator>,
    warnings: &mut WarningCollector,
) {
    let Some(mut reader) = open_source(path, "session_index.jsonl", warnings) else {
        return;
    };
    let mut line_number: usize = 0;
    loop {
        let line = match read_bounded_line(&mut reader) {
            Ok(Some(BoundedLine::Text(line))) => line,
            Ok(Some(BoundedLine::Oversized)) => {
                line_number += 1;
                warnings.push(format!(
                    "session_index.jsonl line {line_number}: line exceeds size limit"
                ));
                continue;
            }
            Ok(Some(BoundedLine::InvalidUtf8)) => {
                line_number += 1;
                warnings.push(format!(
                    "session_index.jsonl line {line_number}: invalid UTF-8"
                ));
                continue;
            }
            Ok(None) => break,
            Err(error) => {
                warnings.push(format!(
                    "session_index.jsonl line {}: read error ({})",
                    line_number.saturating_add(1),
                    io_error_kind(&error)
                ));
                break;
            }
        };
        line_number += 1;
        if line.trim().is_empty() {
            continue;
        }
        let value: Value = match serde_json::from_str(&line) {
            Ok(value) => value,
            Err(_) => {
                warnings.push(format!(
                    "session_index.jsonl line {line_number}: malformed JSON"
                ));
                continue;
            }
        };
        let Some(object) = value.as_object() else {
            warnings.push(format!(
                "session_index.jsonl line {line_number}: expected an object"
            ));
            continue;
        };
        let Some(session_id) = first_string(object, &["id", "session_id", "thread_id"]) else {
            warnings.push(format!(
                "session_index.jsonl line {line_number}: missing session id"
            ));
            continue;
        };
        if session_id.len() > 512 {
            warnings.push(format!(
                "session_index.jsonl line {line_number}: session id exceeds size limit"
            ));
            continue;
        }
        if session_id.chars().any(char::is_control) {
            warnings.push(format!(
                "session_index.jsonl line {line_number}: session id contains control characters"
            ));
            continue;
        }
        let title = first_string(object, &["thread_name", "title", "name"])
            .map(|title| sanitize_title(&title));
        let updated = object
            .get("updated_at")
            .or_else(|| object.get("last_activity"))
            .and_then(parse_timestamp);
        if let Some(session) = session_for(sessions, session_id, warnings) {
            session.merge_index(title, updated);
        }
    }
}

fn scan_history(
    path: &Path,
    sessions: &mut HashMap<String, SessionAccumulator>,
    warnings: &mut WarningCollector,
) {
    let Some(mut reader) = open_source(path, "history.jsonl", warnings) else {
        return;
    };
    let mut line_number: usize = 0;
    loop {
        let line = match read_bounded_line(&mut reader) {
            Ok(Some(BoundedLine::Text(line))) => line,
            Ok(Some(BoundedLine::Oversized)) => {
                line_number += 1;
                warnings.push(format!(
                    "history.jsonl line {line_number}: line exceeds size limit"
                ));
                continue;
            }
            Ok(Some(BoundedLine::InvalidUtf8)) => {
                line_number += 1;
                warnings.push(format!("history.jsonl line {line_number}: invalid UTF-8"));
                continue;
            }
            Ok(None) => break,
            Err(error) => {
                warnings.push(format!(
                    "history.jsonl line {}: read error ({})",
                    line_number.saturating_add(1),
                    io_error_kind(&error)
                ));
                break;
            }
        };
        line_number += 1;
        if line.trim().is_empty() {
            continue;
        }
        let record: HistoryRecord = match serde_json::from_str(&line) {
            Ok(record) => record,
            Err(_) => {
                warnings.push(format!("history.jsonl line {line_number}: malformed JSON"));
                continue;
            }
        };
        let Some(session_id) = record.session_id else {
            warnings.push(format!(
                "history.jsonl line {line_number}: missing session id"
            ));
            continue;
        };
        if session_id.len() > 512 {
            warnings.push(format!(
                "history.jsonl line {line_number}: session id exceeds size limit"
            ));
            continue;
        }
        if session_id.chars().any(char::is_control) {
            warnings.push(format!(
                "history.jsonl line {line_number}: session id contains control characters"
            ));
            continue;
        }
        // Unknown fields, including text, are skipped by HistoryRecord and
        // never copied into the report.
        let timestamp = record
            .ts
            .as_ref()
            .or(record.timestamp.as_ref())
            .and_then(parse_timestamp_input);
        if let Some(session) = session_for(sessions, session_id, warnings) {
            session.add_history(timestamp);
        }
    }
}

fn open_source(
    path: &Path,
    label: &str,
    warnings: &mut WarningCollector,
) -> Option<BufReader<File>> {
    match File::open(path) {
        Ok(file) => Some(BufReader::new(file)),
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            warnings.push(format!("{label} not found"));
            None
        }
        Err(error) => {
            warnings.push(format!(
                "{label} could not be opened ({})",
                io_error_kind(&error)
            ));
            None
        }
    }
}

#[derive(Debug)]
enum BoundedLine {
    Text(String),
    Oversized,
    InvalidUtf8,
}

fn read_bounded_line<R: BufRead>(reader: &mut R) -> io::Result<Option<BoundedLine>> {
    let mut line = Vec::with_capacity(8 * 1024);
    let mut oversized = false;
    let mut consumed_any = false;

    loop {
        let chunk = reader.fill_buf()?;
        if chunk.is_empty() {
            break;
        }
        consumed_any = true;
        if let Some(newline) = chunk.iter().position(|byte| *byte == b'\n') {
            if !oversized {
                let remaining = MAX_LINE_BYTES.saturating_sub(line.len());
                if newline > remaining {
                    oversized = true;
                } else {
                    line.extend_from_slice(&chunk[..newline]);
                }
            }
            reader.consume(newline + 1);
            break;
        }

        if !oversized {
            let remaining = MAX_LINE_BYTES.saturating_sub(line.len());
            let take = chunk.len().min(remaining);
            line.extend_from_slice(&chunk[..take]);
            if chunk.len() > remaining {
                oversized = true;
            }
        }
        let consumed = chunk.len();
        reader.consume(consumed);
    }

    if !consumed_any {
        return Ok(None);
    }
    if oversized {
        return Ok(Some(BoundedLine::Oversized));
    }
    match String::from_utf8(line) {
        Ok(line) => Ok(Some(BoundedLine::Text(line))),
        Err(_) => Ok(Some(BoundedLine::InvalidUtf8)),
    }
}

fn first_string(object: &serde_json::Map<String, Value>, keys: &[&str]) -> Option<String> {
    keys.iter().find_map(|key| {
        object
            .get(*key)
            .and_then(Value::as_str)
            .map(ToOwned::to_owned)
    })
}

fn parse_timestamp_input(value: &TimestampInput) -> Option<ParsedTimestamp> {
    match value {
        TimestampInput::Number(number) => number_to_timestamp(number.as_f64()?),
        TimestampInput::Text(string) => parse_timestamp_string(string),
    }
}

fn sanitize_title(title: &str) -> String {
    sanitize_text(title, 200)
}

fn parse_timestamp(value: &Value) -> Option<ParsedTimestamp> {
    match value {
        Value::Number(number) => number_to_timestamp(number.as_f64()?),
        Value::String(string) => parse_timestamp_string(string),
        _ => None,
    }
}

fn parse_timestamp_string(string: &str) -> Option<ParsedTimestamp> {
    let trimmed = string.trim();
    if trimmed.is_empty() {
        return None;
    }
    if let Ok(number) = trimmed.parse::<f64>() {
        return number_to_timestamp(number);
    }
    Some(ParsedTimestamp {
        raw: sanitize_scalar(trimmed),
        epoch: parse_rfc3339_epoch(trimmed),
    })
}

fn number_to_timestamp(number: f64) -> Option<ParsedTimestamp> {
    if !number.is_finite() {
        return None;
    }
    let absolute = number.abs();
    let (epoch, raw) = if absolute < 100_000_000_000.0 {
        let epoch = number.round() as i64;
        (epoch, system_epoch_to_rfc3339(epoch))
    } else if absolute < 100_000_000_000_000.0 {
        let epoch = (number / 1_000.0).round() as i64;
        (epoch, system_epoch_to_rfc3339(epoch))
    } else {
        let epoch = (number / 1_000_000_000.0).round() as i64;
        (epoch, system_epoch_to_rfc3339(epoch))
    };
    Some(ParsedTimestamp {
        raw,
        epoch: Some(epoch),
    })
}

fn sanitize_scalar(value: &str) -> String {
    sanitize_text(value, MAX_METADATA_TEXT_CHARS)
}

fn sanitize_text(value: &str, max_chars: usize) -> String {
    let mut result = String::with_capacity(value.len().min(max_chars));
    for character in value.chars() {
        result.push(if character.is_control() {
            ' '
        } else {
            character
        });
        if result.chars().count() >= max_chars {
            break;
        }
    }
    result.trim().to_owned()
}

fn io_error_kind(error: &io::Error) -> &'static str {
    match error.kind() {
        io::ErrorKind::PermissionDenied => "permission denied",
        io::ErrorKind::InvalidData => "invalid data",
        io::ErrorKind::UnexpectedEof => "unexpected EOF",
        _ => "I/O error",
    }
}

fn display_path(path: &Path) -> String {
    let rendered = path.to_string_lossy();
    if let Some(home) = env::var_os("HOME") {
        let home = PathBuf::from(home);
        let home_text = home.to_string_lossy();
        if rendered == home_text {
            return "~".to_owned();
        }
        let prefix = format!("{home_text}/");
        if rendered.starts_with(&prefix) {
            return format!("~/{}", &rendered[prefix.len()..]);
        }
    }
    rendered.into_owned()
}

fn system_time_to_rfc3339(time: SystemTime) -> String {
    DateTime::<Utc>::from(time).to_rfc3339_opts(SecondsFormat::Secs, true)
}

fn system_epoch_to_rfc3339(epoch: i64) -> String {
    Utc.timestamp_opt(epoch, 0)
        .single()
        .map(|timestamp| timestamp.to_rfc3339_opts(SecondsFormat::Secs, true))
        .unwrap_or_else(|| epoch.to_string())
}

fn parse_rfc3339_epoch(value: &str) -> Option<i64> {
    DateTime::parse_from_rfc3339(value)
        .ok()
        .map(|timestamp| timestamp.timestamp())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::ffi::OsString;
    use std::fs;

    #[test]
    fn timestamp_round_trip_and_offsets() {
        assert_eq!(system_epoch_to_rfc3339(0), "1970-01-01T00:00:00Z");
        assert_eq!(
            system_epoch_to_rfc3339(1_754_645_264),
            "2025-08-08T09:27:44Z"
        );
        assert_eq!(parse_rfc3339_epoch("1970-01-01T09:00:00+09:00"), Some(0));
    }

    #[test]
    fn scan_does_not_emit_prompt_text() {
        let directory = tempfile::tempdir().expect("tempdir");
        fs::write(
            directory.path().join("session_index.jsonl"),
            "{\"id\":\"abc\",\"thread_name\":\"Safe\",\"updated_at\":\"2026-01-02T00:00:00Z\"}\n",
        )
        .expect("index");
        fs::write(
            directory.path().join("history.jsonl"),
            "{\"session_id\":\"abc\",\"ts\":1767312000,\"text\":\"TOP_SECRET_DO_NOT_PRINT\"}\n",
        )
        .expect("history");
        let report = generate_report(&ScanOptions::at(
            directory.path().to_path_buf(),
            UNIX_EPOCH + std::time::Duration::from_secs(1_767_312_000),
        ))
        .expect("report");
        let serialized = serde_json::to_string(&report).expect("json");
        assert!(!serialized.contains("TOP_SECRET_DO_NOT_PRINT"));
        assert_eq!(report.entries[0].prompt_count, 1);
    }

    #[test]
    fn default_root_requires_an_environment_path() {
        assert!(matches!(
            resolve_default_root(None, None),
            Err(ReportError::DefaultRootUnavailable)
        ));
        assert!(matches!(
            resolve_default_root(Some(OsString::new()), Some(OsString::new())),
            Err(ReportError::DefaultRootUnavailable)
        ));
        assert_eq!(
            resolve_default_root(None, Some(OsString::from("/tmp/home"))).expect("home"),
            PathBuf::from("/tmp/home/.codex")
        );
    }

    #[test]
    fn oversized_history_line_is_drained_without_secret_leak() {
        let directory = tempfile::tempdir().expect("tempdir");
        fs::write(
            directory.path().join("session_index.jsonl"),
            "{\"id\":\"ok\",\"thread_name\":\"Safe\",\"updated_at\":\"2026-01-02T00:00:00Z\"}\n",
        )
        .expect("index");
        let oversized_text = "OVERSIZED_SECRET_DO_NOT_PRINT".repeat(MAX_LINE_BYTES / 20);
        let history = format!(
            "{{\"session_id\":\"bad\",\"text\":\"{oversized_text}\"}}\n{{\"session_id\":\"ok\",\"ts\":1767312000,\"text\":\"valid\"}}\n"
        );
        fs::write(directory.path().join("history.jsonl"), history).expect("history");

        let report = generate_report(&ScanOptions::at(
            directory.path().to_path_buf(),
            UNIX_EPOCH + std::time::Duration::from_secs(1_767_312_000),
        ))
        .expect("report");
        assert_eq!(report.entries.len(), 1);
        assert_eq!(report.entries[0].session_id, "ok");
        assert!(
            report
                .warnings
                .iter()
                .any(|warning| warning.contains("line exceeds size limit"))
        );
        assert!(
            !serde_json::to_string(&report)
                .expect("json")
                .contains("OVERSIZED_SECRET_DO_NOT_PRINT")
        );
    }

    #[test]
    fn title_controls_cannot_inject_human_output() {
        let directory = tempfile::tempdir().expect("tempdir");
        fs::write(
            directory.path().join("session_index.jsonl"),
            "{\"id\":\"abc\",\"thread_name\":\"first\\nsecond\\tthird\\u0000\",\"updated_at\":\"2026-01-02T00:00:00Z\"}\n",
        )
        .expect("index");
        let report = generate_report(&ScanOptions::at(
            directory.path().to_path_buf(),
            UNIX_EPOCH + std::time::Duration::from_secs(1_767_312_000),
        ))
        .expect("report");
        assert_eq!(report.entries[0].title, "first second third");
        assert!(!report.render_human().contains("first\nsecond"));
    }

    #[test]
    fn since_days_excludes_sessions_without_activity() {
        let directory = tempfile::tempdir().expect("tempdir");
        fs::write(
            directory.path().join("session_index.jsonl"),
            concat!(
                "{\"id\":\"unknown\",\"thread_name\":\"Unknown\"}\n",
                "{\"id\":\"recent\",\"thread_name\":\"Recent\",\"updated_at\":\"2026-01-02T00:00:00Z\"}\n"
            ),
        )
        .expect("index");
        fs::write(
            directory.path().join("history.jsonl"),
            "{\"session_id\":\"recent\",\"ts\":1767312000,\"text\":\"valid\"}\n",
        )
        .expect("history");
        let mut options = ScanOptions::at(
            directory.path().to_path_buf(),
            UNIX_EPOCH + std::time::Duration::from_secs(1_767_312_000),
        );
        options.since_days = Some(1);
        let report = generate_report(&options).expect("report");
        assert_eq!(
            report
                .entries
                .iter()
                .map(|entry| entry.session_id.as_str())
                .collect::<Vec<_>>(),
            vec!["recent"]
        );
        assert!(
            report
                .warnings
                .iter()
                .any(|warning| warning.contains("activity timestamp was unavailable"))
        );
    }

    #[test]
    fn explicit_root_metadata_errors_are_distinct() {
        let directory = tempfile::tempdir().expect("tempdir");
        let file = directory.path().join("not-a-directory");
        fs::write(&file, "x").expect("file");
        let error = generate_report(&ScanOptions::at(
            file,
            UNIX_EPOCH + std::time::Duration::from_secs(1_767_312_000),
        ))
        .expect_err("not a directory");
        assert!(matches!(error, ReportError::ExplicitRootNotDirectory(_)));

        let missing = directory.path().join("missing");
        let error = generate_report(&ScanOptions::at(
            missing,
            UNIX_EPOCH + std::time::Duration::from_secs(1_767_312_000),
        ))
        .expect_err("missing");
        assert!(matches!(error, ReportError::MissingExplicitRoot(_)));
    }

    #[test]
    fn control_character_session_ids_are_rejected() {
        let directory = tempfile::tempdir().expect("tempdir");
        fs::write(
            directory.path().join("session_index.jsonl"),
            "{\"id\":\"bad\\nid\",\"thread_name\":\"not emitted\",\"updated_at\":\"2026-01-02T00:00:00Z\"}\n",
        )
        .expect("index");
        fs::write(
            directory.path().join("history.jsonl"),
            "{\"session_id\":\"bad\\nid\",\"ts\":1767312000,\"text\":\"SECRET\"}\n",
        )
        .expect("history");
        let report = generate_report(&ScanOptions::at(
            directory.path().to_path_buf(),
            UNIX_EPOCH + std::time::Duration::from_secs(1_767_312_000),
        ))
        .expect("report");
        assert!(report.entries.is_empty());
        assert_eq!(
            report
                .warnings
                .iter()
                .filter(|warning| warning.contains("session id contains control characters"))
                .count(),
            2
        );
        assert!(!report.render_human().contains("bad\nid"));
    }

    #[test]
    fn session_accumulator_cap_preserves_existing_ids() {
        let mut sessions = HashMap::with_capacity(MAX_SESSIONS);
        for index in 0..MAX_SESSIONS {
            sessions.insert(index.to_string(), SessionAccumulator::default());
        }
        let mut warnings = WarningCollector::default();
        assert!(session_for(&mut sessions, "0".to_owned(), &mut warnings).is_some());
        assert!(session_for(&mut sessions, "new-session".to_owned(), &mut warnings).is_none());
        assert_eq!(sessions.len(), MAX_SESSIONS);
        let warnings = warnings.finish();
        assert_eq!(
            warnings
                .iter()
                .filter(|warning| warning.contains("session accumulator limit"))
                .count(),
            1
        );
    }

    #[test]
    fn library_limit_is_normalized() {
        let directory = tempfile::tempdir().expect("tempdir");
        let options =
            ScanOptions::from_root_arg(Some(directory.path().to_path_buf()), usize::MAX, None)
                .expect("options");
        assert_eq!(options.limit, MAX_LIMIT);
    }
}
