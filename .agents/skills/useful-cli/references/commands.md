# Useful CLI command reference

This is a routing and safety reference, not a substitute for current `--help` or source. The repository may be newer than installed binaries.

## Read-only diagnostics

| Command | Use it for | Useful forms | Important boundary |
|---|---|---|---|
| `bintrace` | Resolve every PATH candidate and inspect executable provenance, SHA-256, Mach-O, and Go build metadata | `bintrace useful`, `bintrace --json /path/to/bin` | Does not execute the target. A directly named non-executable file is reported as a finding. |
| `dedupe` | Find byte-identical duplicate files | `dedupe --min-size 10MiB ~/Downloads`, `dedupe --json .` | Discovery only; never deletes or hardlinks. File/depth limits can produce partial exit `3`. |
| `gitdoctor` | Diagnose branch, upstream, worktrees, locks, status, and optional integrity | `gitdoctor .`, `gitdoctor --json --fsck .` | Read-only. Does not fetch, gc, prune, reset, or clean. `--fsck` is bounded. |
| `launchaudit` | Audit macOS launchd plist safety | `launchaudit`, `launchaudit --scope local`, `launchaudit --json FILE` | macOS only. Redacts full arguments/environment and does not change launchd state. A plist `Disabled` value is not proof of loaded state. |
| `netcheck` | Isolate DNS, TCP, TLS, and HTTP failures | `netcheck https://example.com`, `netcheck --timeout 3s host:443` | One explicit target; no CIDR/port scan, proxy-env use, TLS bypass, or redirect following. |
| `pathdoctor` | Audit `.zshrc`/profile PATH entries and current PATH | `pathdoctor`, `pathdoctor --current`, `pathdoctor --file ~/.zprofile --json` | Static parser: does not source the file or execute `eval`, substitutions, or globs. Dynamic constructs make the result partial. |
| `symaudit` | Find dangling links, loops, absolute links, and root escapes | `symaudit .`, `symaudit --max-depth 8 --json PATH` | Never follows a symlink directory during traversal. Absolute links remain review findings even when resolved. Audit only. |
| `lsport` | List TCP/UDP owners and watch opened/closed ports | `lsport --listen`, `lsport --port 8080`, `lsport --watch --count 10` | Read-only. Give agent/non-TTY watches a positive `--count`; it includes the initial snapshot. |
| `gitstats` | Summarize contributors, hotspots, and commit times | `gitstats --days 30`, `gitstats --hotspots --time` | Reads Git history only. Author and top-N filters are available. |

## Creating or changing files

### `hashmanifest`

- Create deterministic SHA-256 JSONL: `hashmanifest create .`.
- Write a new file: `hashmanifest create --output manifest.jsonl .`.
- Verify: `hashmanifest verify --root . manifest.jsonl`.
- Existing output is never overwritten. Verify rejects absolute paths, `..` escapes, duplicate paths, unknown record fields, and symlink traversal.

### `flatten`

- Preview first: `flatten --dry-run /path/to/folder`.
- Copy to an explicit destination: `flatten --output ./out /path/to/folder`.
- It copies regular files; it does not move the source. It preflights collisions, refuses overwrite, skips symlinks/non-regular files, and prompts before copying.

## Deletion and process control

### `depclean`

- Analyze by default: `depclean --path ~/project --days 30`.
- Delete only with both `--apply` and confirmation: `depclean --path ~/project --days 30 --min-size 100MB --apply`.
- Candidates require project markers. Supported artifacts include `node_modules`, virtualenvs, `__pycache__`, selected Gradle/Maven/Rust targets, Pods, and `.NET obj`.
- It refuses filesystem root, the home directory itself, and a symlink root. It does not target `.env`, `vendor`, generic `build`/`bin`, or DerivedData.

### `logclean`

- Analyze user logs by default: `logclean --days 30`.
- Add user caches or Trash explicitly: `--caches`, `--trash`, or compatibility `--all`.
- Delete only with `--apply` plus confirmation.
- System `/private/var/log` and `/Library/Logs` are outside deletion scope.

### `sysclean`

- Always start with `sysclean --dry-run` or a narrowed form such as `sysclean --only npm-cache,uv-cache --dry-run`.
- Discover target IDs with `sysclean --list`.
- `--all` adds optional targets; `--docker` adds Docker unused data but not volumes; `--projects ROOT` scans old project artifacts. `--only ID,...` replaces the default selection with the named IDs.
- Without `--dry-run`, the command can delete after analysis and confirmation. Prefer `--select` for review. `--yes` bypasses final confirmation and needs explicit authorization.
- Docker volumes, Xcode Archives, system-wide logs/caches, `.env`, and `vendor` are not automatic cleanup targets.

### `portkill`

1. Inspect first: `lsport --port 8080`.
2. Request graceful termination: `portkill 8080`.
3. Use `portkill --force 8080` only when SIGKILL is explicitly required.

The command accepts TCP LISTEN and UDP local-bound owners, prompts before signaling, and rechecks ownership/identity immediately before each signal. The final identity-check-to-signal OS race cannot be made fully atomic.

## Rust diagnostics

These tools are read-only and live under `rust/crates/`; they are not dispatched by `useful`.

| Command | Use it for | Useful forms | Important boundary |
|---|---|---|---|
| `repo-snapshot` | Collect bounded Git status, manifest, and file-tree context | `repo-snapshot .`, `repo-snapshot . --json`, `repo-snapshot . --pretty --max-files 500 --max-depth 5` | Does not read file contents. Bounds are deliberate context limits. |
| `macdiag-json` | Collect privacy-reduced macOS hardware, battery, memory, storage, and probe JSON | `macdiag-json --pretty`, `macdiag-json --section battery` | Does not collect serial, UUID, hostname, or username by default. Non-macOS exits `2`. |
| `codex-history` | Summarize local Codex session/history metadata | `codex-history --limit 20 --since-days 7`, `codex-history --json` | Does not print prompt text or scan rollout files by default. Uses `$CODEX_HOME`, then `$HOME/.codex`; otherwise require `--root`. |

## Dispatcher and installation

- `useful --help` lists registered commands.
- The dispatcher resolves an executable sibling first and then searches `PATH`.
- Install every Go command together from the repository only when requested:

  ```bash
  mkdir -p "$HOME/bin"
  GOBIN="$HOME/bin" GOTOOLCHAIN=go1.24.0 go install ./cmd/...
  ```

- Install Rust commands individually only when requested: `cargo install --locked --path rust/crates/<name>`.

- If `$HOME/bin` is not in `PATH`, prefer a session-local `PATH` update unless the user explicitly asks to edit shell startup files. Use `pathdoctor` before changing `.zshrc` when existing PATH health is uncertain.
