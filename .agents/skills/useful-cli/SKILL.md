---
name: useful-cli
description: Use and troubleshoot this repository's `useful` Go CLI suite and Rust diagnostics, including pathdoctor, sysclean, depclean, lsport, repo-snapshot, macdiag-json, and codex-history. Use when the user mentions this project, asks which bundled CLI fits a task, wants a safe command run, or names one of its commands; do not use for unrelated generic shell utilities or generic Go CLI design.
---

# Useful CLI

Operate the checkout that contains this project skill. For a globally installed copy, use the current checkout or locate one by its `go.mod` module and Git remote; never assume a fixed home-directory path.

## Establish the runtime

1. Determine whether the user wants to use an installed binary, inspect the source tree, or change the project.
2. For installed behavior, enumerate collisions with `type -a useful` or `type -a <command>`, resolve the executable, then read its `--help` before constructing flags.
3. For source behavior, inspect the current repository and run `GOTOOLCHAIN=go1.24.0 go run ./cmd/<command> --help` when needed.
4. Do not assume the installed binaries match the dirty source tree. State which one supplied the evidence. Use `bintrace` when available, otherwise `go version -m`, when provenance matters.
5. Prefer `useful <command> ...` when the dispatcher and sibling binaries are installed together. If dispatch fails, resolve the individual binary rather than silently using different source code.

Do not install or rebuild binaries merely to answer a usage question. Build or install only when the user asks or when it is an authorized implementation step.

## Choose and run safely

Read [references/commands.md](references/commands.md) when selecting a command, forming flags, or checking a command-specific safety boundary.

- Begin cleanup, copy, and process-control work with the relevant read-only view or `--dry-run`.
- Treat `sysclean` without `--dry-run`, `depclean --apply`, `logclean --apply`, `flatten` without `--dry-run`, and `portkill` as mutating operations.
- Show the concrete targets before mutation. Preserve the CLI's confirmation prompt. Use `--yes` or `portkill --force` only when the user's request clearly authorizes that stronger action.
- Never turn discovery from `dedupe`, `pathdoctor`, `symaudit`, or another audit command into deletion or automatic repair unless separately requested.
- Preserve user exclusions and unrelated files. Do not broaden a named path, cleanup class, port, or repository scope.

## Interpret results

For the result-signal commands `bintrace`, `dedupe`, `gitdoctor`, `hashmanifest`, `launchaudit`, `netcheck`, `pathdoctor`, and `symaudit`, interpret exit status as:

- `0`: complete with no finding.
- `1`: finding, mismatch, duplicate, or check failure; inspect the report before calling it an execution error.
- `2`: usage error, unsupported environment, or fatal input.
- `3`: partial result caused by a timeout, bound, permission problem, or incomplete analysis.

Do not apply that mapping to `lsport`, `gitstats`, the Rust diagnostics, or mutating commands. They may use `1` for an operational, scan, or serialization failure. Prefer stderr, command output, and current source/help over a blanket interpretation.

When reporting, include the command form, whether it was read-only/dry-run or mutating, the resolved target, exit status, and any partial-result boundary. Keep raw paths and exact finding names intact.

## Project work

When modifying this repository:

- Keep Go code compatible with Go 1.24 and standard-library-only.
- Add a standalone entry point under `cmd/<name>` and register it in `cmd/useful/main.go` when adding a command.
- Preserve the launcher rule: executable sibling first, then `PATH`.
- Validate proportionally with `gofmt`, focused tests, `go test -race`, `go vet`, and `go build`; use exact Go 1.24 for compatibility checks when available.
- Do not overwrite the user's existing dirty-worktree changes or commit generated binaries and `dist/` artifacts.
