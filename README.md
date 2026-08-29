# useful-go

macOS 개발·파일·네트워크 진단용 CLI 모음입니다. Go 도구는 외부 모듈 없이
표준 라이브러리만 사용합니다. 일부 macOS 기능은 시스템 명령인 `git`, `lsof`,
`plutil`을 읽기 전용 adapter로 호출합니다.

## 안전 기본값

- `bintrace`, `dedupe`, `gitdoctor`, `launchaudit`, `netcheck`, `pathdoctor`, `symaudit`는 읽기 전용입니다.
- `hashmanifest create`만 새 manifest 파일을 만들 수 있으며 기존 파일을 덮어쓰지 않습니다.
- `depclean`, `logclean`은 기본 분석 전용입니다. 삭제하려면 `--apply`와 대화형 확인이 모두 필요합니다.
- `portkill`은 종료 직전 포트 소유 PID를 다시 확인하고 기본 `SIGTERM`을 보냅니다. `SIGKILL`은 `--force`에서만 사용합니다.
- 파일 트리 탐색·manifest·정리 명령은 symlink를 따라가지 않습니다. `bintrace`에 직접 지정한 symlink만 해석 대상 바이너리를 읽어 metadata와 hash를 표시합니다.

## 설치

모든 Go 명령을 `~/bin`에 설치:

```bash
mkdir -p ~/bin
GOBIN="$HOME/bin" go install ./cmd/...
```

`~/bin`이 `PATH`에 없다면:

```bash
export PATH="$HOME/bin:$PATH"
```

통합 launcher도 함께 설치됩니다. 개별 명령과 같은 디렉터리 또는 `PATH`에서
하위 바이너리를 찾습니다.

```bash
useful bintrace useful
useful gitdoctor .
useful lsport --listen
```

Rust 기반 bounded 진단 도구는 별도 설치합니다.

```bash
cargo install --locked --path rust/crates/repo-snapshot
cargo install --locked --path rust/crates/macdiag-json
cargo install --locked --path rust/crates/codex-history
```

자세한 설명은 [`rust/README.md`](rust/README.md)를 참고하세요.

### Agent skill

Codex·Claude가 이 프로젝트의 CLI를 안전하게 선택하고 실행하도록 bundled
`useful-cli` skill을 설치합니다.

```bash
sh scripts/install-skill.sh
```

공용 원본은 `~/.agents/skills/useful-cli`에 복사되고 Claude에는 해당 경로를
가리키는 user skill link가 생성됩니다. Claude link가 필요 없으면
`sh scripts/install-skill.sh --no-claude`를 사용합니다.

## 명령어

| 명령 | 목적 | 기본 동작 |
|---|---|---|
| `bintrace` | PATH 바이너리 출처·SHA-256·Mach-O/Go build 정보 | 읽기 전용 |
| `dedupe` | 내용이 정확히 같은 중복 파일 탐색 | 읽기 전용 |
| `gitdoctor` | branch/upstream/worktree/lock/fsck 진단 | 읽기 전용 |
| `hashmanifest` | versioned SHA-256 JSONL 생성·검증 | create만 새 파일 가능 |
| `launchaudit` | LaunchAgent/Daemon plist 안전 감사 | 읽기 전용 |
| `netcheck` | DNS→TCP→TLS→HTTP 단계별 연결 진단 | 읽기 전용 |
| `pathdoctor` | `.zshrc`와 현재 PATH의 죽은·중복·위험 entry 감사 | 읽기 전용 |
| `symaudit` | dangling/loop/root escape symlink 감사 | 읽기 전용 |
| `lsport` | 포트 목록과 opened/closed 변화 감시 | 읽기 전용 |
| `portkill` | 포트 소유 프로세스 재확인 후 종료 | 확인 필수 |
| `depclean` | marker 검증된 오래된 프로젝트 산출물 분석 | 기본 분석 전용 |
| `logclean` | 오래된 사용자 로그·선택 cache 분석 | 기본 분석 전용 |
| `sysclean` | allowlist 개발 cache와 프로젝트 산출물 정리 | dry-run/선택 지원 |
| `gitstats` | Git 기여자·hotspot·시간대 통계 | 읽기 전용 |
| `flatten` | 폴더 파일을 충돌 없이 한 디렉터리로 복사 | 확인 필수 |

### bintrace

명령을 실행하지 않고 `PATH`의 모든 후보를 비교합니다.

```bash
bintrace useful
bintrace --json /opt/homebrew/bin/go
```

### dedupe

크기 → SHA-256 → byte 비교 순으로 exact duplicate만 보고합니다. hardlink는
절약 가능 용량에서 중복 계산하지 않습니다.

```bash
dedupe ~/Downloads
dedupe --min-size 10MiB --max-files 50000 --workers 8 ~/Documents
dedupe --json .
```

삭제·hardlink 변환 기능은 없습니다.

### gitdoctor

stable Git porcelain 형식을 사용해 현재 저장소를 진단합니다.

```bash
gitdoctor .
gitdoctor --json --timeout 30s .
gitdoctor --fsck .
```

`--fsck`도 읽기 전용이며 `fetch`, `gc`, `prune`, `reset`, `clean`은 실행하지 않습니다.

### hashmanifest

상대 경로와 SHA-256을 versioned JSON Lines로 저장합니다. 공백·Unicode·개행이
포함된 파일명도 안전하게 encode합니다.

```bash
hashmanifest create .
hashmanifest create --output manifest.jsonl .
hashmanifest verify --root . manifest.jsonl
```

`--output` 대상이 이미 존재하면 실패합니다. verify는 absolute path와 `..`
escape를 거부합니다.

### launchaudit

기본값은 현재 사용자의 `~/Library/LaunchAgents`만 검사합니다. plist의
`Disabled` 값은 선언일 뿐 실제 loaded 상태로 해석하지 않습니다.

```bash
launchaudit
launchaudit --scope local
launchaudit --json /path/to/example.plist
```

환경변수와 전체 `ProgramArguments`는 출력하지 않습니다. `local`은
`/Library/LaunchAgents`와 `/Library/LaunchDaemons`, `all`은 System 경로까지
추가합니다.

### netcheck

대상은 반드시 명시해야 합니다. redirect, environment proxy, TLS 검증 우회,
CIDR/port scan은 지원하지 않습니다.

```bash
netcheck https://example.com
netcheck --timeout 3s example.com:443
netcheck --json http://127.0.0.1:8080/health
```

### pathdoctor

`.zshrc`를 source/eval하지 않고 직접 `PATH`/`path` 대입만 정적으로 분석합니다.
죽은 directory, broken symlink, 중복, 상대·빈 entry, directory가 아닌 entry,
world-writable directory를 line 번호와 함께 보고합니다. command substitution,
glob, HOME/PATH 이외 변수는 실행하지 않고 partial로 표시합니다.

```bash
pathdoctor
pathdoctor --current
pathdoctor --json --file ~/.zprofile
```

자동 수정 기능은 없습니다. `source`, `eval`, 복잡한 parameter modifier가 실제
PATH를 바꾸는 경우 결과는 보수적인 부분 분석입니다.

### symaudit

symlink 디렉터리를 따라가지 않고 상태를 분류합니다. absolute link는 정상
대상이어도 review 대상으로 보고합니다.

```bash
symaudit .
symaudit --max-depth 8 --max-links 10000 ~/project
symaudit --json .
```

### lsport

`lsof` machine-field 출력을 사용합니다. watch의 `--count`는 초기 snapshot을
포함합니다. 비-TTY 환경에서는 무한 대기를 막기 위해 `--count`가 필수입니다.

```bash
lsport --listen
lsport --tcp --port 8080
lsport --listen --watch --interval 1s --count 10
```

### portkill

TCP LISTEN과 UDP local-bound 소유자만 대상으로 합니다.

```bash
portkill 8080          # 확인 후 SIGTERM
portkill --force 8080  # 확인 후 SIGKILL
```

처음 확인한 PID가 신호 직전에도 같은 포트를 소유할 때만 처리합니다.
macOS에서는 `proc_pidinfo`의 microsecond 시작 identity도 비교하고 각 PID마다
신호 직전 다시 조회합니다. identity 확인과 signal 자체는 하나의 원자적 OS
연산이 아니므로 마지막 두 동작 사이의 극소 race는 완전히 제거할 수 없습니다.

### depclean

허용 대상은 marker로 검증된 `node_modules`, `venv/.venv`, `__pycache__`,
`.gradle`, Maven/Rust `target`, `Pods`, `.NET obj`입니다. `.env`, `vendor`,
일반 `build`, `bin`, `DerivedData`는 삭제 후보가 아닙니다.

```bash
depclean --path ~/project --days 30
depclean --path ~/project --days 30 --min-size 100MB --apply
```

파일시스템 `/`, 홈 디렉터리 자체, symlink root는 거부합니다.

### logclean

기본 대상은 사용자 로그와 CrashReporter입니다. cache와 휴지통은 별도 opt-in입니다.

```bash
logclean --days 30
logclean --days 30 --caches
logclean --days 30 --caches --apply
logclean --trash --apply
```

`--all`은 호환 옵션으로 사용자 cache와 휴지통만 추가합니다. 시스템
`/private/var/log`, `/Library/Logs`는 더 이상 삭제하지 않습니다.

### sysclean

재생성 가능한 개발 cache allowlist와 명시한 프로젝트 root만 대상으로 합니다.

```bash
sysclean --dry-run
sysclean --select
sysclean --all --dry-run
sysclean --docker --dry-run
sysclean --only npm-cache,uv-cache
sysclean --projects ~/project --project-days 60 --select
```

Docker volume, Xcode Archives, 시스템 전체 cache/log, `.env`, `vendor`는 자동
정리하지 않습니다. `--docker` 사용 시 Docker 실행 파일이 없으면 `0 B`로
숨기지 않고 오류로 종료합니다.

### gitstats

```bash
gitstats --days 30
gitstats --hotspots --time
gitstats --author "홍길동" --top 5
```

### flatten

쓰기 전에 전체 충돌을 검사하며 기존 파일을 덮어쓰지 않습니다. symlink와
non-regular 파일은 건너뜁니다.

```bash
flatten --dry-run /path/to/folder
flatten --output ./out /path/to/folder
flatten --pad 3 --sep "-" /path/to/folder
```

## 종료 코드

새 읽기 전용 진단 명령은 다음 규칙을 사용합니다.

| 코드 | 의미 |
|---:|---|
| `0` | 검사 완료, finding 없음 |
| `1` | duplicate·mismatch·진단 finding 또는 check 실패 |
| `2` | 사용법 오류, 지원하지 않는 환경, fatal 입력 오류 |
| `3` | timeout·한도 도달·권한 문제 등 partial 결과 |

finding을 정상적인 CI 신호로 사용할 때는 exit `1`을 고려해야 합니다.

## 개발

```bash
gofmt -w ./cmd ./internal ./pkg
go test ./...
go test -race ./...
go vet ./...
go build ./...

cargo fmt --manifest-path rust/Cargo.toml --all -- --check
cargo test --manifest-path rust/Cargo.toml --workspace
cargo clippy --manifest-path rust/Cargo.toml --workspace --all-targets -- -D warnings
```
