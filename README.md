# useful-go

macOS 유틸리티 CLI 모음

Go 유틸리티와 Rust 기반 읽기 전용 진단 도구를 함께 제공합니다.

## Rust 도구

- `repo-snapshot`: Git 상태, manifest, 제한된 파일 트리 수집
- `macdiag-json`: 개인정보를 제외한 macOS 진단 JSON 생성
- `codex-history`: prompt 원문 없는 로컬 Codex 작업 이력 요약

빌드, 설치, 사용법은 [`rust/README.md`](rust/README.md)를 참고하세요.

## 설치

### 전체 빌드 후 사용자 bin으로 설치

```bash
# 빌드 및 ~/bin에 설치
mkdir -p ~/bin && \
go build -o ~/bin/useful ./cmd/useful && \
go build -o ~/bin/portkill ./cmd/portkill && \
go build -o ~/bin/logclean ./cmd/logclean && \
go build -o ~/bin/flatten ./cmd/flatten && \
go build -o ~/bin/sysclean ./cmd/sysclean && \
go build -o ~/bin/gitstats ./cmd/gitstats
```

> `~/bin`이 PATH에 없다면: `echo 'export PATH="$HOME/bin:$PATH"' >> ~/.zshrc && source ~/.zshrc`

### go install 사용

```bash
go install ./cmd/...
```

### 개별 빌드

```bash
go build -o bin/useful ./cmd/useful
go build -o bin/portkill ./cmd/portkill
go build -o bin/logclean ./cmd/logclean
go build -o bin/flatten ./cmd/flatten
go build -o bin/sysclean ./cmd/sysclean
go build -o bin/gitstats ./cmd/gitstats
```

## 명령어

### portkill

포트를 사용하는 프로세스를 종료합니다.

```bash
portkill 8080
```

### logclean

macOS 로그/캐시 파일을 정리합니다.

```bash
logclean --dry-run          # 분석만 수행
logclean --days 30          # 30일 이상 된 파일만
logclean --all              # sudo 필요한 경로 포함
```

### sysclean

macOS 개발 캐시와 오래된 프로젝트 빌드 산출물을 병렬 분석하고 선택 정리합니다. 시스템 전체 경로 대신 재생성 가능한 고정 allowlist만 기본 대상으로 사용합니다.

```bash
sysclean --dry-run                       # 안전 기본 캐시 병렬 분석
sysclean --select                        # 분석 후 번호로 항목 선택
sysclean --all --dry-run                 # 재다운로드 비용 있는 optional 캐시 포함
sysclean --docker --dry-run              # Docker unused data 포함 (volume 제외)
sysclean --only npm-cache,uv-cache       # 지정한 ID만 정리
sysclean --list                          # 지원 ID와 활성화 조건 확인
sysclean --workers 8                     # 병렬 작업자 수 직접 지정 (0: 자동)
sysclean --projects ~/project --dry-run  # 30일 이상 미수정 프로젝트 산출물 탐색
sysclean --projects ~/project --project-days 60 --select
```

프로젝트 탐색은 명시한 루트에서만 동작합니다. `.venv`, `venv`, `node_modules`, `.next`, `.nuxt`, `.turbo`, Rust/Maven `target`, SwiftPM `.build`, Gradle, CocoaPods, .NET 산출물을 marker 파일로 검증합니다. 프로젝트 산출물이 발견되면 실제 삭제 전에 개별 번호 선택을 강제합니다. Git에 추적될 수 있는 `vendor`와 비밀 설정에 흔히 쓰이는 `.env`는 제외합니다.

`Xcode Archives`, `/Library/Caches`, `/var/log`, `/private/var/folders`, 앱 데이터 전체, Docker volume은 자동 정리 대상이 아닙니다.

### gitstats

Git 저장소 커밋 통계를 보여줍니다.

```bash
gitstats                    # 기본 통계
gitstats --days 30          # 최근 30일
gitstats --hotspots         # 자주 변경되는 파일
gitstats --time             # 시간대/요일별 커밋 분포
gitstats --author "홍길동"   # 특정 작성자 필터
gitstats --top 5            # 상위 5명만
```

### flatten

폴더 구조를 평탄화합니다. 숫자는 자동으로 zero-padding되어 정렬 문제를 해결합니다.

```bash
flatten --dry-run /path/to/folder   # 미리보기
flatten --output ./out /path/to/folder  # 출력 폴더 지정
flatten --pad 3 /path/to/folder     # 숫자 3자리 패딩 (001, 002...)
flatten --sep "-" /path/to/folder   # 구분자 변경 (기본: _)
```

**예시:**
```
before/                          after/
├── chapter1/                    ├── chapter1_01.txt
│   ├── 1.txt                    ├── chapter1_02.txt
│   ├── 2.txt                    ├── chapter1_11.txt
│   └── 11.txt                   └── chapter2_01.txt
└── chapter2/
    └── 1.txt
```

## 통합 CLI

```bash
useful portkill 8080
useful logclean --dry-run
useful gitstats --hotspots --time
```
