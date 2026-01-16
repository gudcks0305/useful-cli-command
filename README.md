# useful-go

macOS 유틸리티 CLI 모음

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

macOS 시스템 캐시/임시 파일을 정리합니다. 패턴 기반으로 앱 캐시를 포괄적으로 감지합니다.

```bash
sysclean --dry-run          # 분석만 수행
sysclean --all              # sudo 필요한 시스템 경로 포함
sysclean --docker           # Docker 정리 포함
```

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
