# Rust CLI tools

읽기 전용 진단과 작업 컨텍스트 수집을 위한 Rust CLI입니다.

## Build and test

```bash
cd rust
cargo build --workspace
cargo test --workspace
cargo clippy --workspace --all-targets -- -D warnings
```

## Install

```bash
cargo install --path rust/crates/repo-snapshot
cargo install --path rust/crates/macdiag-json
cargo install --path rust/crates/codex-history
```

## Commands

### repo-snapshot

저장소의 Git 상태, manifest, 제한된 파일 트리를 수집합니다. 파일 내용은 읽지 않습니다.

```bash
repo-snapshot .
repo-snapshot . --json
repo-snapshot . --pretty --max-files 500 --max-depth 5
```

### macdiag-json

macOS 하드웨어, 배터리, 메모리, 저장공간 probe를 읽기 전용으로 실행하고 JSON으로 출력합니다. 일련번호, UUID, 호스트명, 사용자명은 기본 수집하지 않습니다.

```bash
macdiag-json
macdiag-json --pretty
macdiag-json --section battery
```

`--section`은 `all`, `platform`, `hardware`, `battery`, `memory`, `storage`, `probes`, `warnings`를 받습니다. macOS 외 플랫폼에서는 종료 코드 `2`를 반환합니다.

### codex-history

로컬 Codex `session_index.jsonl`과 `history.jsonl`에서 세션 메타데이터를 요약합니다. prompt 원문은 출력하지 않으며 rollout 파일을 기본 스캔하지 않습니다.

```bash
codex-history
codex-history --limit 20 --since-days 7
codex-history --json
codex-history --root /path/to/codex-data --pretty
```

기본 경로는 `$CODEX_HOME`, 없으면 `$HOME/.codex`입니다. 둘 다 없으면 현재 디렉터리를 추측하지 않고 명시적 `--root`를 요구합니다.
