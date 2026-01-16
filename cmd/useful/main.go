package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/useful-go/pkg/common"
)

var commands = map[string]Command{
	"portkill": {
		Description: "포트를 사용하는 프로세스 종료",
		Usage:       "useful portkill <port>",
	},
	"logclean": {
		Description: "macOS 로그/캐시 파일 정리",
		Usage:       "useful logclean [--dry-run] [--days N] [--all]",
	},
	"flatten": {
		Description: "폴더 구조 평탄화 (숫자 자동 패딩)",
		Usage:       "useful flatten [--dry-run] [--output DIR] [--pad N] <folder>",
	},
	"sysclean": {
		Description: "macOS 시스템 데이터 정리",
		Usage:       "useful sysclean [--dry-run] [--all] [--docker]",
	},
	"gitstats": {
		Description: "Git 커밋 통계",
		Usage:       "useful gitstats [--days N] [--hotspots] [--time]",
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

	cmdPath := filepath.Join(filepath.Dir(execPath), subCmd)
	if _, err := os.Stat(cmdPath); os.IsNotExist(err) {
		cmdPath, err = exec.LookPath(subCmd)
		if err != nil {
			common.Fatal("명령어를 찾을 수 없습니다: %s", subCmd)
		}
	}

	cmd := exec.Command(cmdPath, os.Args[2:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
}

func printHelp() {
	common.Header("useful - macOS 유틸리티 CLI 모음")
	fmt.Println()
	fmt.Println("사용법: useful <command> [options]")
	fmt.Println()
	fmt.Println("명령어:")
	for name, cmd := range commands {
		fmt.Printf("  %-12s %s\n", name, cmd.Description)
		fmt.Printf("               %s\n", cmd.Usage)
	}
	fmt.Println()
	fmt.Println("도움말: useful <command> --help")
}
