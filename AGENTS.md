# PROJECT KNOWLEDGE BASE

**Generated:** 2026-01-18
**Commit:** daee152
**Branch:** main

## OVERVIEW

Go CLI 유틸리티 모음. macOS 시스템 정리, Git 통계, 파일 관리 도구. Zero dependencies (stdlib only).

## STRUCTURE

```
useful-go/
├── cmd/           # CLI 엔트리포인트 (각 도구별 main.go)
│   ├── useful/    # 통합 런처 (다른 도구들의 dispatcher)
│   ├── depclean/  # 프로젝트 의존성 정리 (node_modules, venv 등)
│   ├── portkill/  # 포트 점유 프로세스 종료
│   ├── logclean/  # 로그/캐시 파일 정리
│   ├── sysclean/  # 시스템 캐시 정리
│   ├── gitstats/  # Git 커밋 통계
│   └── flatten/   # 폴더 평탄화
└── pkg/           # 공용 라이브러리
    ├── common/    # 콘솔 출력 (색상, 로그 레벨)
    ├── fs/        # 파일시스템 유틸 (FormatSize, ExpandPath, GetDirSize)
    ├── text/      # 문자열 처리 (Truncate, ParseSize, NaturalLess)
    └── ui/        # 사용자 입력 (YesNoConfirmation)
```

## WHERE TO LOOK

| Task | Location | Notes |
|------|----------|-------|
| 새 CLI 도구 추가 | `cmd/<name>/main.go` | `pkg/` 패키지 재사용 |
| 출력 포맷팅 | `pkg/common/output.go` | Success/Error/Warning/Info/Fatal |
| 파일 크기 처리 | `pkg/fs/files.go` | FormatSize, GetDirSize |
| 사용자 확인 | `pkg/ui/confirm.go` | YesNoConfirmation |
| 문자열 자르기 | `pkg/text/text.go` | Truncate, TruncatePath |

## CODE MAP

| Symbol | Type | Location | Role |
|--------|------|----------|------|
| `commands` | Variable | cmd/useful/main.go:12 | 사용 가능한 CLI 명령어 목록 |
| `Success/Error/Warning/Info` | Function | pkg/common/output.go | 색상 출력 함수 |
| `Fatal` | Function | pkg/common/output.go:40 | 에러 출력 후 os.Exit(1) |
| `FormatSize` | Function | pkg/fs/files.go | bytes → human readable |
| `GetDirSize` | Function | pkg/fs/files.go | 디렉토리 총 크기 계산 |
| `YesNoConfirmation` | Function | pkg/ui/confirm.go | Y/N 프롬프트 생성 |
| `NaturalLess` | Function | pkg/text/text.go | 자연 정렬 비교 (file2 < file10) |

## CONVENTIONS

- **Go 1.24.0** (go.mod)
- **Zero Dependencies** - stdlib만 사용, 외부 라이브러리 금지
- **표준 Go 레이아웃** - cmd/ + pkg/ 구조
- **통합 CLI 패턴** - `useful <command>` 또는 개별 바이너리로 실행 가능

## ANTI-PATTERNS (THIS PROJECT)

- **루트 바이너리 커밋 금지** - `gitstats`, `sysclean` 등 루트에 빌드된 파일 Git 추적 중 (정리 필요)
- **dist/ 커밋 금지** - 크로스 컴파일 결과물 Git에 포함됨
- **외부 의존성 추가 금지** - stdlib only 원칙 유지

## COMMANDS

```bash
# 빌드 (전체)
go build ./...

# 개별 도구 빌드
go build -o ~/bin/portkill ./cmd/portkill

# 전체 설치
go install ./cmd/...

# 사용 예시
useful portkill 3000
useful depclean --days 30 --path ~/Projects
useful logclean --dry-run
useful gitstats --since="1 month ago"
```

## NOTES

- `depclean` 명령어가 README에 문서화되지 않음
- `.gitignore` 업데이트 필요: `dist/`, 루트 바이너리들 추가
- Makefile/CI 없음 - 빌드 자동화 고려
