package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/useful-go/pkg/common"
)

var commands = map[string]Command{
	"bintrace": {
		Description: "PATH 바이너리 출처와 무결성 조회",
		Usage:       "useful bintrace [--json] <command-or-path>",
	},
	"dedupe": {
		Description: "정확한 중복 파일 읽기 전용 탐색",
		Usage:       "useful dedupe [--json] [--min-size SIZE] [path]",
	},
	"gitdoctor": {
		Description: "Git 저장소와 worktree 읽기 전용 진단",
		Usage:       "useful gitdoctor [--json] [--fsck] [path]",
	},
	"hashmanifest": {
		Description: "SHA-256 manifest 생성과 검증",
		Usage:       "useful hashmanifest <create|verify> [options]",
	},
	"launchaudit": {
		Description: "macOS launchd plist 읽기 전용 감사",
		Usage:       "useful launchaudit [--json] [--scope SCOPE] [path ...]",
	},
	"lsport": {
		Description: "사용 중인 포트 목록 조회",
		Usage:       "useful lsport [--tcp] [--udp] [--listen] [--port N] [--watch]",
	},
	"netcheck": {
		Description: "DNS/TCP/TLS/HTTP 단계별 연결 진단",
		Usage:       "useful netcheck [--json] [--timeout D] <target>",
	},
	"pathdoctor": {
		Description: "zshrc와 현재 PATH 정적 안전 진단",
		Usage:       "useful pathdoctor [--json] [--file FILE] [--current]",
	},
	"portkill": {
		Description: "포트를 사용하는 프로세스 종료",
		Usage:       "useful portkill [--force] <port>",
	},
	"logclean": {
		Description: "macOS 사용자 로그 분석과 선택 정리",
		Usage:       "useful logclean [--apply] [--days N] [--caches] [--trash]",
	},
	"flatten": {
		Description: "폴더 구조 평탄화 (숫자 자동 패딩)",
		Usage:       "useful flatten [--dry-run] [--output DIR] [--pad N] <folder>",
	},
	"sysclean": {
		Description: "macOS 개발 캐시와 프로젝트 산출물 선택 정리",
		Usage:       "useful sysclean [--dry-run] [--select] [--projects PATH] [--all] [--docker]",
	},
	"gitstats": {
		Description: "Git 커밋 통계",
		Usage:       "useful gitstats [--days N] [--hotspots] [--time]",
	},
	"depclean": {
		Description: "marker 검증된 프로젝트 산출물 분석과 선택 정리",
		Usage:       "useful depclean [--apply] [--days N] [--path DIR] [--min-size SIZE]",
	},
	"symaudit": {
		Description: "symlink 상태와 root escape 읽기 전용 감사",
		Usage:       "useful symaudit [--json] [path]",
	},
}

type Command struct {
	Description string
	Usage       string
}

func main() {
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(0)
	}

	subCmd := os.Args[1]

	if subCmd == "help" || subCmd == "-h" || subCmd == "--help" {
		printHelp()
		os.Exit(0)
	}

	if _, exists := commands[subCmd]; !exists {
		common.Error("알 수 없는 명령어: %s", subCmd)
		printHelp()
		os.Exit(1)
	}

	execPath, err := os.Executable()
	if err != nil {
		common.Fatal("실행 경로 확인 실패: %v", err)
	}

	cmdPath, err := resolveCommandPath(execPath, subCmd)
	if err != nil {
		common.Fatal("명령어를 찾을 수 없습니다: %s", subCmd)
	}

	code, err := executeCommand(cmdPath, os.Args[2:], os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		common.Error("명령 실행 실패: %v", err)
	}
	os.Exit(code)
}

func resolveCommandPath(launcherPath, name string) (string, error) {
	sibling := filepath.Join(filepath.Dir(launcherPath), name)
	if info, err := os.Stat(sibling); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
		return sibling, nil
	}
	return exec.LookPath(name)
}

func executeCommand(path string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.Command(path, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

func printHelp() {
	common.Header("useful - macOS 유틸리티 CLI 모음")
	fmt.Println()
	fmt.Println("사용법: useful <command> [options]")
	fmt.Println()
	fmt.Println("명령어:")
	for _, name := range sortedCommandNames(commands) {
		cmd := commands[name]
		fmt.Printf("  %-12s %s\n", name, cmd.Description)
		fmt.Printf("               %s\n", cmd.Usage)
	}
	fmt.Println()
	fmt.Println("도움말: useful <command> --help")
}

func sortedCommandNames(registry map[string]Command) []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
