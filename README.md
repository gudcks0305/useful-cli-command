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

## 통합 CLI

```bash
useful portkill 8080
useful logclean --dry-run
```
