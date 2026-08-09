//! Library implementation for the `repo-snapshot` command.
//!
//! The snapshot intentionally only contains path names, file types, and Git
//! metadata. It never opens a file to read its contents, and symlink targets
//! are not followed while traversing a tree.

use std::cmp::Ordering;
use std::collections::BTreeMap;
use std::ffi::OsStr;
use std::fmt;
use std::fs::{self, DirEntry, FileType};
use std::io::{self, Read};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::thread;

use chrono::{SecondsFormat, Utc};
use serde::Serialize;

/// Current JSON schema version.
pub const SCHEMA_VERSION: u32 = 1;

/// Defaults are deliberately conservative so invoking `repo-snapshot` on a
/// large repository remains bounded.
pub const DEFAULT_MAX_FILES: usize = 1_000;
pub const DEFAULT_MAX_DEPTH: usize = 4;

const MAX_WARNINGS: usize = 64;
const MAX_GIT_CAPTURE_BYTES: usize = 256 * 1024;
const MAX_STATUS_ENTRIES: usize = 1_000;
const MAX_STATUS_BYTES: usize = 64 * 1024;
const GIT_READ_CHUNK_BYTES: usize = 8 * 1024;

/// Options controlling a snapshot collection.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SnapshotOptions {
    /// Maximum number of total entries (directories and file-like entries) to include in `tree`.
    pub max_files: usize,
    /// Maximum child depth to traverse. Direct children have depth 1.
    pub max_depth: usize,
}

impl Default for SnapshotOptions {
    fn default() -> Self {
        Self {
            max_files: DEFAULT_MAX_FILES,
            max_depth: DEFAULT_MAX_DEPTH,
        }
    }
}

/// A complete repository snapshot. This is the stable JSON contract emitted
/// by the CLI.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct Snapshot {
    pub schema_version: u32,
    pub generated_at: String,
    pub path: String,
    pub git: Option<GitSnapshot>,
    pub manifests: Vec<Manifest>,
    pub tree: Vec<TreeEntry>,
    pub warnings: Vec<String>,
}

/// Git metadata collected without mutating the repository.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct GitSnapshot {
    pub root: String,
    pub branch: Option<String>,
    pub head: Option<String>,
    /// Lines from `git status --porcelain=v1 --untracked-files=normal`.
    pub status: Vec<String>,
}

/// A detected project manifest or lock file.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct Manifest {
    /// Path relative to the requested snapshot root, using `/` separators.
    pub path: String,
    /// Stable family name such as `cargo`, `node`, or `python`.
    pub kind: String,
}

/// One path in the bounded, deterministic tree.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct TreeEntry {
    /// Path relative to the requested snapshot root, using `/` separators.
    pub path: String,
    /// `dir`, `file`, `symlink`, or `other`.
    pub kind: String,
    /// Root children have depth 1.
    pub depth: usize,
}

/// Errors that make a snapshot impossible to create. Individual traversal or
/// Git failures are represented as warnings in a successful snapshot.
#[derive(Debug)]
pub enum SnapshotError {
    InvalidPath { path: PathBuf, reason: String },
    Serialize(serde_json::Error),
}

impl fmt::Display for SnapshotError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidPath { path, reason } => {
                write!(f, "invalid snapshot path '{}': {reason}", path.display())
            }
            Self::Serialize(error) => write!(f, "failed to encode snapshot: {error}"),
        }
    }
}

impl std::error::Error for SnapshotError {}

impl From<serde_json::Error> for SnapshotError {
    fn from(error: serde_json::Error) -> Self {
        Self::Serialize(error)
    }
}

/// Collect a bounded, read-only snapshot for `path`.
pub fn snapshot(path: &Path, options: SnapshotOptions) -> Result<Snapshot, SnapshotError> {
    let root = canonical_directory(path)?;
    let mut collector = Collector::new(root.clone(), options);
    collector.collect();
    let git = collect_git(&root, &mut collector.warnings);
    let warnings = collector.warnings.finish();

    Ok(Snapshot {
        schema_version: SCHEMA_VERSION,
        generated_at: Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true),
        path: path_string(&root),
        git,
        manifests: collector.manifests,
        tree: collector.tree,
        warnings,
    })
}

/// Render a snapshot as compact JSON.
pub fn render_json(value: &Snapshot, pretty: bool) -> Result<String, SnapshotError> {
    if pretty {
        serde_json::to_string_pretty(value).map_err(SnapshotError::from)
    } else {
        serde_json::to_string(value).map_err(SnapshotError::from)
    }
}

/// Render a concise human-readable snapshot. The JSON renderer remains the
/// machine-readable contract; this view is intended for a terminal.
pub fn render_human(value: &Snapshot) -> String {
    let mut output = String::new();
    output.push_str(&format!("Path: {}\n", value.path));

    match &value.git {
        Some(git) => {
            output.push_str(&format!("Git root: {}\n", git.root));
            output.push_str(&format!(
                "Git branch: {}\n",
                git.branch.as_deref().unwrap_or("(detached/unknown)")
            ));
            output.push_str(&format!(
                "Git HEAD: {}\n",
                git.head.as_deref().unwrap_or("(unborn/unknown)")
            ));
            output.push_str("Git status (porcelain):\n");
            if git.status.is_empty() {
                output.push_str("  clean\n");
            } else {
                for line in &git.status {
                    output.push_str("  ");
                    output.push_str(line);
                    output.push('\n');
                }
            }
        }
        None => output.push_str("Git: not a repository\n"),
    }

    output.push_str("Manifests:\n");
    if value.manifests.is_empty() {
        output.push_str("  (none)\n");
    } else {
        for manifest in &value.manifests {
            output.push_str(&format!("  {} ({})\n", manifest.path, manifest.kind));
        }
    }

    output.push_str("Tree:\n");
    if value.tree.is_empty() {
        output.push_str("  (none or depth-limited)\n");
    } else {
        for entry in &value.tree {
            let indent = "  ".repeat(entry.depth);
            output.push_str(&format!("{indent}{} [{}]\n", entry.path, entry.kind));
        }
    }

    if !value.warnings.is_empty() {
        output.push_str("Warnings:\n");
        for warning in &value.warnings {
            output.push_str("  - ");
            output.push_str(warning);
            output.push('\n');
        }
    }

    output
}

fn canonical_directory(path: &Path) -> Result<PathBuf, SnapshotError> {
    let canonical = fs::canonicalize(path).map_err(|error| SnapshotError::InvalidPath {
        path: path.to_path_buf(),
        reason: error.to_string(),
    })?;

    let metadata = fs::metadata(&canonical).map_err(|error| SnapshotError::InvalidPath {
        path: path.to_path_buf(),
        reason: error.to_string(),
    })?;
    if !metadata.is_dir() {
        return Err(SnapshotError::InvalidPath {
            path: path.to_path_buf(),
            reason: "path is not a directory".to_owned(),
        });
    }

    Ok(canonical)
}

#[derive(Debug, Default)]
struct WarningCollector {
    values: Vec<String>,
    dropped: usize,
}

impl WarningCollector {
    fn push(&mut self, message: impl Into<String>) {
        if self.values.len() < MAX_WARNINGS {
            self.values.push(message.into());
        } else {
            self.dropped = self.dropped.saturating_add(1);
        }
    }

    fn finish(mut self) -> Vec<String> {
        if self.dropped > 0 {
            // Keep the returned warning list bounded while retaining an
            // explicit count of evidence that was omitted.
            if self.values.len() == MAX_WARNINGS {
                self.values.pop();
                self.dropped = self.dropped.saturating_add(1);
            }
            self.values
                .push(format!("{} additional warnings omitted", self.dropped));
        }
        self.values
    }
}

struct Collector {
    root: PathBuf,
    options: SnapshotOptions,
    entry_count: usize,
    tree_truncated: bool,
    depth_truncated: bool,
    manifests: Vec<Manifest>,
    tree: Vec<TreeEntry>,
    warnings: WarningCollector,
}

impl Collector {
    fn new(root: PathBuf, options: SnapshotOptions) -> Self {
        Self {
            root,
            options,
            entry_count: 0,
            tree_truncated: false,
            depth_truncated: false,
            manifests: Vec::new(),
            tree: Vec::new(),
            warnings: WarningCollector::default(),
        }
    }

    fn collect(&mut self) {
        self.visit_directory(&self.root.clone(), 0);
        if self.tree_truncated {
            self.warnings.push(format!(
                "tree truncated after {} total entries (--max-files)",
                self.options.max_files
            ));
        }
        if self.depth_truncated {
            self.warnings.push(format!(
                "tree traversal stopped at depth {} (--max-depth)",
                self.options.max_depth
            ));
        }
        self.manifests.sort_by(|left, right| {
            left.path
                .cmp(&right.path)
                .then_with(|| left.kind.cmp(&right.kind))
        });
    }

    fn visit_directory(&mut self, directory: &Path, depth: usize) {
        if depth >= self.options.max_depth {
            self.depth_truncated = true;
            return;
        }
        if self.entry_count >= self.options.max_files {
            self.tree_truncated = true;
            return;
        }

        let remaining = self.options.max_files - self.entry_count;
        let batch = read_sorted_entries(directory, &self.root, &mut self.warnings, remaining);
        if batch.dropped {
            self.tree_truncated = true;
        }

        for entry in batch.entries {
            if self.entry_count >= self.options.max_files {
                self.tree_truncated = true;
                return;
            }
            let name = entry.file_name();
            let entry_path = entry.path();
            let next_depth = depth.saturating_add(1);
            if next_depth > self.options.max_depth {
                self.depth_truncated = true;
                continue;
            }
            let file_type = match entry.file_type() {
                Ok(file_type) => file_type,
                Err(error) => {
                    self.warn_io("inspect entry", &entry_path, &error);
                    continue;
                }
            };

            if file_type.is_dir() {
                self.push_tree_entry(&entry_path, "dir", next_depth);
                if self.entry_count >= self.options.max_files {
                    self.tree_truncated = true;
                    return;
                }
                if next_depth >= self.options.max_depth {
                    self.depth_truncated = true;
                } else {
                    self.visit_directory(&entry_path, next_depth);
                }
                continue;
            }

            let kind = entry_kind(file_type);
            if let Some(manifest_kind) = manifest_kind(&name) {
                self.manifests.push(Manifest {
                    path: relative_path(&self.root, &entry_path),
                    kind: manifest_kind.to_owned(),
                });
            }
            self.push_tree_entry(&entry_path, kind, next_depth);
            if self.entry_count >= self.options.max_files {
                self.tree_truncated = true;
                return;
            }
        }
    }

    fn push_tree_entry(&mut self, path: &Path, kind: &str, depth: usize) {
        self.entry_count += 1;
        self.tree.push(TreeEntry {
            path: relative_path(&self.root, path),
            kind: kind.to_owned(),
            depth,
        });
    }

    fn warn_io(&mut self, operation: &str, path: &Path, error: &io::Error) {
        self.warnings.push(format!(
            "{operation} '{}': {}",
            relative_path(&self.root, path),
            error
        ));
    }
}

struct EntryBatch {
    entries: Vec<DirEntry>,
    dropped: bool,
}

fn read_sorted_entries(
    directory: &Path,
    root: &Path,
    warnings: &mut WarningCollector,
    limit: usize,
) -> EntryBatch {
    let entries = match fs::read_dir(directory) {
        Ok(entries) => entries,
        Err(error) => {
            warnings.push(format!(
                "read directory '{}': {}",
                relative_path(root, directory),
                error
            ));
            return EntryBatch {
                entries: Vec::new(),
                dropped: false,
            };
        }
    };
    collect_sorted_entries(entries, directory, root, warnings, limit)
}

fn collect_sorted_entries<I>(
    entries: I,
    directory: &Path,
    root: &Path,
    warnings: &mut WarningCollector,
    limit: usize,
) -> EntryBatch
where
    I: IntoIterator<Item = io::Result<DirEntry>>,
{
    let mut selected = BTreeMap::new();
    let mut dropped = false;
    for entry in entries {
        match entry {
            Ok(entry) => {
                let name = entry.file_name();
                if is_excluded_name(&name) {
                    continue;
                }
                if limit == 0 {
                    dropped = true;
                    continue;
                }
                if selected.len() < limit {
                    selected.insert(name, entry);
                    continue;
                }

                let largest = selected
                    .keys()
                    .next_back()
                    .cloned()
                    .expect("selected is non-empty when at limit");
                if compare_os_str(&name, &largest) == Ordering::Less {
                    selected.remove(&largest);
                    selected.insert(name, entry);
                }
                dropped = true;
            }
            Err(error) => warnings.push(format!(
                "read directory entry '{}': {}",
                relative_path(root, directory),
                error
            )),
        }
    }
    let mut entries: Vec<_> = selected.into_values().collect();
    entries.sort_by(|left, right| compare_os_str(&left.file_name(), &right.file_name()));
    EntryBatch { entries, dropped }
}

fn compare_os_str(left: &OsStr, right: &OsStr) -> Ordering {
    left.to_string_lossy().cmp(&right.to_string_lossy())
}

fn is_excluded_name(name: &OsStr) -> bool {
    matches!(name.to_str(), Some(".git" | "target" | "node_modules"))
}

fn entry_kind(file_type: FileType) -> &'static str {
    if file_type.is_symlink() {
        "symlink"
    } else if file_type.is_file() {
        "file"
    } else {
        "other"
    }
}

fn relative_path(root: &Path, path: &Path) -> String {
    path.strip_prefix(root)
        .unwrap_or(path)
        .components()
        .map(|component| component.as_os_str().to_string_lossy().into_owned())
        .collect::<Vec<_>>()
        .join("/")
}

fn path_string(path: &Path) -> String {
    path.to_string_lossy().into_owned()
}

fn manifest_kind(name: &OsStr) -> Option<&'static str> {
    match name.to_str()? {
        "Cargo.toml" => Some("cargo"),
        "Cargo.lock" => Some("cargo-lock"),
        "package.json" => Some("node"),
        "package-lock.json" | "npm-shrinkwrap.json" => Some("npm-lock"),
        "pnpm-lock.yaml" => Some("pnpm-lock"),
        "yarn.lock" => Some("yarn-lock"),
        "go.mod" => Some("go"),
        "go.sum" => Some("go-lock"),
        "pyproject.toml" | "requirements.txt" | "Pipfile" | "setup.py" | "setup.cfg" => {
            Some("python")
        }
        "Pipfile.lock" | "poetry.lock" | "uv.lock" => Some("python-lock"),
        "Gemfile" => Some("ruby"),
        "Gemfile.lock" => Some("ruby-lock"),
        "composer.json" => Some("php"),
        "composer.lock" => Some("php-lock"),
        "pom.xml" => Some("maven"),
        "build.gradle" | "build.gradle.kts" | "settings.gradle" | "settings.gradle.kts" => {
            Some("gradle")
        }
        "mix.exs" => Some("elixir"),
        "Package.swift" => Some("swift"),
        "Makefile" => Some("make"),
        "Dockerfile" => Some("docker"),
        _ => None,
    }
}

#[derive(Debug)]
struct GitCommandError {
    command: String,
    detail: String,
    not_found: bool,
    stdout_truncated: bool,
    stderr_truncated: bool,
}

impl fmt::Display for GitCommandError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let truncation = if self.stdout_truncated || self.stderr_truncated {
            " [output truncated]"
        } else {
            ""
        };
        if self.detail.is_empty() {
            write!(f, "{} failed{truncation}", self.command)
        } else {
            write!(f, "{} failed: {}{truncation}", self.command, self.detail)
        }
    }
}

fn collect_git(root: &Path, warnings: &mut WarningCollector) -> Option<GitSnapshot> {
    let root_output = match run_git(root, &["rev-parse", "--show-toplevel"]) {
        Ok(output) => {
            warn_git_capture("git rev-parse --show-toplevel", &output, warnings, true);
            output
        }
        Err(error) if !error.not_found && is_not_git_repository(&error.detail) => {
            warnings.push(format!("git repository not detected: {error}"));
            return None;
        }
        Err(error) => {
            warnings.push(format!("git metadata unavailable: {error}"));
            return None;
        }
    };

    let git_root = first_line(&root_output.stdout).unwrap_or_else(|| path_string(root));

    let branch = match run_git(root, &["symbolic-ref", "--quiet", "--short", "HEAD"]) {
        Ok(output) => {
            warn_git_capture(
                "git symbolic-ref --quiet --short HEAD",
                &output,
                warnings,
                true,
            );
            first_line(&output.stdout)
        }
        Err(error) if is_expected_branch_failure(&error.detail) => None,
        Err(error) => {
            warnings.push(format!("git branch unavailable: {error}"));
            None
        }
    };

    let head = match run_git(root, &["rev-parse", "--verify", "HEAD"]) {
        Ok(output) => {
            warn_git_capture("git rev-parse --verify HEAD", &output, warnings, true);
            first_line(&output.stdout)
        }
        Err(error) if is_expected_unborn_head(&error.detail) => None,
        Err(error) => {
            warnings.push(format!("git HEAD unavailable: {error}"));
            None
        }
    };

    let status = match run_git(
        root,
        &["status", "--porcelain=v1", "--untracked-files=normal"],
    ) {
        Ok(output) => {
            // Status has a separate line/byte cap so a dirty repository
            // cannot make the JSON payload unbounded.
            if output.stderr_truncated {
                warnings.push("git status stderr exceeded the bounded capture prefix".to_owned());
            }
            let parsed = parse_status(&output.stdout);
            if parsed.truncated || output.stdout_truncated {
                warnings.push(format!(
                    "git status truncated at {} entries / {} bytes",
                    MAX_STATUS_ENTRIES, MAX_STATUS_BYTES
                ));
            }
            parsed.lines
        }
        Err(error) => {
            warnings.push(format!("git status unavailable: {error}"));
            Vec::new()
        }
    };

    Some(GitSnapshot {
        root: git_root,
        branch,
        head,
        status,
    })
}

struct GitOutput {
    stdout: String,
    stdout_truncated: bool,
    stderr_truncated: bool,
}

fn run_git(root: &Path, args: &[&str]) -> Result<GitOutput, GitCommandError> {
    let command = format!("git {}", args.join(" "));
    let mut child = Command::new("git")
        .args(args)
        .current_dir(root)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|error| GitCommandError {
            command: command.clone(),
            detail: error.to_string(),
            not_found: error.kind() == io::ErrorKind::NotFound,
            stdout_truncated: false,
            stderr_truncated: false,
        })?;

    let stdout = child.stdout.take().ok_or_else(|| GitCommandError {
        command: command.clone(),
        detail: "failed to capture stdout".to_owned(),
        not_found: false,
        stdout_truncated: false,
        stderr_truncated: false,
    })?;
    let stderr = child.stderr.take().ok_or_else(|| GitCommandError {
        command: command.clone(),
        detail: "failed to capture stderr".to_owned(),
        not_found: false,
        stdout_truncated: false,
        stderr_truncated: false,
    })?;

    let stdout_reader = thread::spawn(move || capture_prefix(stdout));
    let stderr_reader = thread::spawn(move || capture_prefix(stderr));
    let status = child.wait();
    let stdout = join_capture(stdout_reader).map_err(|error| GitCommandError {
        command: command.clone(),
        detail: format!("failed to read stdout: {error}"),
        not_found: false,
        stdout_truncated: false,
        stderr_truncated: false,
    })?;
    let stderr = join_capture(stderr_reader).map_err(|error| GitCommandError {
        command: command.clone(),
        detail: format!("failed to read stderr: {error}"),
        not_found: false,
        stdout_truncated: stdout.truncated,
        stderr_truncated: false,
    })?;
    let status = status.map_err(|error| GitCommandError {
        command: command.clone(),
        detail: error.to_string(),
        not_found: false,
        stdout_truncated: stdout.truncated,
        stderr_truncated: stderr.truncated,
    })?;

    let stdout_text = String::from_utf8_lossy(&stdout.bytes).into_owned();
    if status.success() {
        return Ok(GitOutput {
            stdout: stdout_text,
            stdout_truncated: stdout.truncated,
            stderr_truncated: stderr.truncated,
        });
    }

    Err(GitCommandError {
        command,
        detail: truncate_detail(String::from_utf8_lossy(&stderr.bytes).trim()),
        not_found: false,
        stdout_truncated: stdout.truncated,
        stderr_truncated: stderr.truncated,
    })
}

#[derive(Debug)]
struct CapturedPipe {
    bytes: Vec<u8>,
    truncated: bool,
}

fn capture_prefix<R: Read>(mut reader: R) -> io::Result<CapturedPipe> {
    let mut bytes = Vec::with_capacity(MAX_GIT_CAPTURE_BYTES.min(GIT_READ_CHUNK_BYTES));
    let mut buffer = [0_u8; GIT_READ_CHUNK_BYTES];
    let mut truncated = false;

    loop {
        let read = reader.read(&mut buffer)?;
        if read == 0 {
            break;
        }

        let remaining = MAX_GIT_CAPTURE_BYTES.saturating_sub(bytes.len());
        let keep = remaining.min(read);
        bytes.extend_from_slice(&buffer[..keep]);
        if keep < read {
            truncated = true;
        }
    }

    Ok(CapturedPipe { bytes, truncated })
}

fn join_capture(handle: thread::JoinHandle<io::Result<CapturedPipe>>) -> io::Result<CapturedPipe> {
    handle
        .join()
        .map_err(|_| io::Error::other("Git output reader panicked"))?
}

#[derive(Debug)]
struct ParsedStatus {
    lines: Vec<String>,
    truncated: bool,
}

fn parse_status(value: &str) -> ParsedStatus {
    let mut lines = Vec::new();
    let mut bytes = 0_usize;
    let mut truncated = false;

    for line in value.lines().map(|line| line.trim_end_matches('\r')) {
        if line.is_empty() {
            continue;
        }
        let line_bytes = line.len().saturating_add(1);
        if lines.len() >= MAX_STATUS_ENTRIES || bytes.saturating_add(line_bytes) > MAX_STATUS_BYTES
        {
            truncated = true;
            break;
        }
        bytes = bytes.saturating_add(line_bytes);
        lines.push(line.to_owned());
    }

    ParsedStatus { lines, truncated }
}

fn warn_git_capture(
    command: &str,
    output: &GitOutput,
    warnings: &mut WarningCollector,
    include_stdout: bool,
) {
    if include_stdout && output.stdout_truncated {
        warnings.push(format!(
            "{command} stdout exceeded the bounded capture prefix"
        ));
    }
    if output.stderr_truncated {
        warnings.push(format!(
            "{command} stderr exceeded the bounded capture prefix"
        ));
    }
}

fn first_line(value: &str) -> Option<String> {
    value
        .lines()
        .map(str::trim)
        .find(|line| !line.is_empty())
        .map(ToOwned::to_owned)
}

fn truncate_detail(value: &str) -> String {
    const MAX_DETAIL_CHARS: usize = 240;
    let mut chars = value.chars();
    let detail: String = chars.by_ref().take(MAX_DETAIL_CHARS).collect();
    if chars.next().is_some() {
        format!("{detail}…")
    } else {
        detail
    }
}

fn is_not_git_repository(detail: &str) -> bool {
    let lower = detail.to_ascii_lowercase();
    lower.contains("not a git repository") || lower.contains("not a git repo")
}

fn is_expected_branch_failure(detail: &str) -> bool {
    let lower = detail.to_ascii_lowercase();
    lower.contains("not a symbolic ref")
        || lower.contains("not a valid branch")
        || lower.contains("no such ref")
}

fn is_expected_unborn_head(detail: &str) -> bool {
    let lower = detail.to_ascii_lowercase();
    lower.contains("needed a single revision")
        || lower.contains("ambiguous argument 'head'")
        || lower.contains("does not have any commits")
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::io::Cursor;
    use std::process::Command;

    use tempfile::TempDir;

    fn options(max_files: usize, max_depth: usize) -> SnapshotOptions {
        SnapshotOptions {
            max_files,
            max_depth,
        }
    }

    #[test]
    fn non_git_directory_is_valid_and_does_not_read_contents() {
        let directory = TempDir::new().expect("temp dir");
        fs::write(
            directory.path().join("Cargo.toml"),
            "secret = \"do not read\"\n",
        )
        .expect("manifest");
        fs::write(directory.path().join("notes.txt"), "PRIVATE FILE CONTENT\n").expect("file");
        let value = snapshot(directory.path(), options(100, 3)).expect("snapshot");

        assert!(value.git.is_none());
        assert_eq!(value.manifests[0].path, "Cargo.toml");
        assert!(value.tree.iter().any(|entry| entry.path == "notes.txt"));
        let encoded = render_json(&value, false).expect("json");
        assert!(!encoded.contains("PRIVATE FILE CONTENT"));
        assert!(!encoded.contains("do not read"));
    }

    #[test]
    fn traversal_is_sorted_and_bounded() {
        let directory = TempDir::new().expect("temp dir");
        fs::create_dir(directory.path().join("z-dir")).expect("directory");
        fs::write(directory.path().join("b.txt"), "b").expect("file");
        fs::write(directory.path().join("a.txt"), "a").expect("file");
        fs::write(directory.path().join("z-dir").join("nested.txt"), "nested")
            .expect("nested file");
        fs::create_dir(directory.path().join("target")).expect("excluded");
        fs::write(directory.path().join("target").join("hidden.txt"), "hidden")
            .expect("excluded file");

        let value = snapshot(directory.path(), options(2, 1)).expect("snapshot");
        let paths: Vec<_> = value.tree.iter().map(|entry| entry.path.as_str()).collect();
        assert_eq!(paths, vec!["a.txt", "b.txt"]);
        assert!(
            value
                .warnings
                .iter()
                .any(|warning| warning.contains("max-files"))
        );
        assert!(!paths.iter().any(|path| path.starts_with("target/")));

        fs::write(directory.path().join("c.txt"), "c").expect("file");
        let bounded = snapshot(directory.path(), options(2, 1)).expect("bounded snapshot");
        assert!(
            bounded
                .warnings
                .iter()
                .any(|warning| warning.contains("max-files"))
        );
    }

    #[test]
    fn high_fanout_selection_is_deterministic_and_entry_bounded() {
        let directory = TempDir::new().expect("temp dir");
        for index in (0..40).rev() {
            fs::write(
                directory.path().join(format!("file-{index:02}.txt")),
                "metadata only",
            )
            .expect("file");
        }

        let first = snapshot(directory.path(), options(5, 2)).expect("snapshot");
        let second = snapshot(directory.path(), options(5, 2)).expect("snapshot");
        let first_paths: Vec<_> = first.tree.iter().map(|entry| entry.path.as_str()).collect();
        let second_paths: Vec<_> = second
            .tree
            .iter()
            .map(|entry| entry.path.as_str())
            .collect();
        assert_eq!(first_paths, second_paths);
        assert_eq!(
            first_paths,
            vec![
                "file-00.txt",
                "file-01.txt",
                "file-02.txt",
                "file-03.txt",
                "file-04.txt"
            ]
        );
        assert!(first.tree.len() <= 5);
        assert!(
            first
                .warnings
                .iter()
                .any(|warning| warning.contains("max-files"))
        );
    }

    #[test]
    fn entry_cap_stops_deep_traversal_and_manifest_growth() {
        let directory = TempDir::new().expect("temp dir");
        let mut current = directory.path().to_path_buf();
        for depth in 0..8 {
            current = current.join(format!("level-{depth}"));
            fs::create_dir(&current).expect("directory");
            fs::write(current.join("Cargo.toml"), "metadata only").expect("manifest");
        }

        let value = snapshot(directory.path(), options(3, 20)).expect("snapshot");
        assert!(value.tree.len() <= 3);
        assert!(value.manifests.len() <= 3);
        assert!(
            value
                .warnings
                .iter()
                .any(|warning| warning.contains("max-files"))
        );
        assert!(value.tree.iter().all(|entry| entry.depth <= 3));
    }

    #[test]
    fn manifest_detection_is_recursive_and_deterministic() {
        let directory = TempDir::new().expect("temp dir");
        fs::create_dir(directory.path().join("nested")).expect("nested");
        fs::write(directory.path().join("nested").join("package.json"), "{}").expect("manifest");
        fs::write(directory.path().join("go.mod"), "module example\n").expect("manifest");

        let value = snapshot(directory.path(), options(100, 4)).expect("snapshot");
        let paths: Vec<_> = value
            .manifests
            .iter()
            .map(|manifest| manifest.path.as_str())
            .collect();
        assert_eq!(paths, vec!["go.mod", "nested/package.json"]);
    }

    #[test]
    fn git_metadata_is_collected_when_git_is_available() {
        let directory = TempDir::new().expect("temp dir");
        let git_available = Command::new("git").arg("--version").output().is_ok();
        if !git_available {
            return;
        }

        run_git_test_command(directory.path(), &["init", "-q"]);
        run_git_test_command(
            directory.path(),
            &["config", "user.email", "test@example.invalid"],
        );
        run_git_test_command(
            directory.path(),
            &["config", "user.name", "repo-snapshot test"],
        );
        fs::write(directory.path().join("tracked.txt"), "metadata only").expect("file");
        run_git_test_command(directory.path(), &["add", "tracked.txt"]);
        run_git_test_command(directory.path(), &["commit", "-qm", "initial"]);
        fs::write(directory.path().join("changed.txt"), "untracked content").expect("file");

        let value = snapshot(directory.path(), options(100, 3)).expect("snapshot");
        let git = value.git.expect("git metadata");
        assert!(git.head.is_some());
        assert!(git.branch.is_some());
        assert!(git.status.iter().any(|line| line.contains("changed.txt")));
    }

    #[test]
    fn invalid_path_is_fatal() {
        let error = snapshot(
            Path::new("/path/that/does/not/exist"),
            SnapshotOptions::default(),
        )
        .expect_err("invalid path should fail");
        assert!(matches!(error, SnapshotError::InvalidPath { .. }));
    }

    #[test]
    fn bounded_git_capture_drains_large_input() {
        let input = vec![b'x'; MAX_GIT_CAPTURE_BYTES * 2];
        let captured = capture_prefix(Cursor::new(input)).expect("capture");
        assert_eq!(captured.bytes.len(), MAX_GIT_CAPTURE_BYTES);
        assert!(captured.truncated);
    }

    #[test]
    fn status_parser_caps_entries_and_bytes() {
        let value = (0..MAX_STATUS_ENTRIES + 10)
            .map(|index| format!("?? file-{index:04}.txt"))
            .collect::<Vec<_>>()
            .join("\n");
        let parsed = parse_status(&value);
        assert!(parsed.lines.len() <= MAX_STATUS_ENTRIES);
        assert!(
            parsed
                .lines
                .iter()
                .map(|line| line.len() + 1)
                .sum::<usize>()
                <= MAX_STATUS_BYTES
        );
        assert!(parsed.truncated);
    }

    #[test]
    fn warning_collector_caps_and_summarizes() {
        let mut warnings = WarningCollector::default();
        for index in 0..MAX_WARNINGS + 10 {
            warnings.push(format!("warning-{index}"));
        }
        let finished = warnings.finish();
        assert_eq!(finished.len(), MAX_WARNINGS);
        assert!(
            finished
                .last()
                .is_some_and(|warning| warning.contains("additional warnings omitted"))
        );
    }

    #[test]
    fn directory_entry_errors_preserve_readable_siblings() {
        let directory = TempDir::new().expect("temp dir");
        fs::write(directory.path().join("a.txt"), "a").expect("file");
        fs::write(directory.path().join("b.txt"), "b").expect("file");
        let read_dir = fs::read_dir(directory.path()).expect("read dir");
        let entries = read_dir.chain(std::iter::once(Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "injected entry error",
        ))));
        let mut warnings = WarningCollector::default();
        let readable = collect_sorted_entries(
            entries,
            directory.path(),
            directory.path(),
            &mut warnings,
            10,
        );

        let names: Vec<_> = readable
            .entries
            .iter()
            .map(|entry| entry.file_name().to_string_lossy().into_owned())
            .collect();
        assert_eq!(names, vec!["a.txt", "b.txt"]);
        assert!(
            warnings
                .finish()
                .iter()
                .any(|warning| warning.contains("injected entry error"))
        );
    }

    fn run_git_test_command(directory: &Path, args: &[&str]) {
        let output = Command::new("git")
            .args(args)
            .current_dir(directory)
            .output()
            .expect("git command");
        assert!(output.status.success(), "git {:?} failed", args);
    }
}
