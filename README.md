# useful-go

macOS 유틸리티 CLI 모음

## 설치

```bash
go install ./cmd/...
```

또는 개별 빌드:

```bash
go build -o bin/useful ./cmd/useful
go build -o bin/portkill ./cmd/portkill
go build -o bin/logclean ./cmd/logclean
go build -o bin/flatten ./cmd/flatten
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
```
