package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/useful-go/pkg/common"
)

var cleanTargets = []CleanTarget{
	{Path: "~/Library/Logs", Description: "시스템 로그"},
	{Path: "~/Library/Caches", Description: "앱 캐시"},
	{Path: "/private/var/log", Description: "시스템 var 로그", NeedsSudo: true},
	{Path: "~/.Trash", Description: "휴지통"},
	{Path: "~/Library/Application Support/CrashReporter", Description: "크래시 리포트"},
	{Path: "/Library/Logs", Description: "라이브러리 로그", NeedsSudo: true},
}

type CleanTarget struct {
	Path        string
	Description string
	NeedsSudo   bool
}

type CleanResult struct {
	Target      CleanTarget
	FilesCount  int
	TotalSize   int64
	DeletedSize int64
	Error       error
}

func main() {
	dryRun := flag.Bool("dry-run", false, "삭제하지 않고 정리 대상만 표시")
	days := flag.Int("days", 7, "N일 이상 된 파일만 정리")
	all := flag.Bool("all", false, "모든 대상 정리 (sudo 필요한 항목 포함)")
	flag.Parse()

	common.Header("🧹 macOS 로그/캐시 클리너")
	fmt.Println()

	if *dryRun {
		common.Info("Dry-run 모드: 실제 삭제 없이 분석만 수행합니다")
		fmt.Println()
	}

	var results []CleanResult
	cutoffTime := time.Now().AddDate(0, 0, -*days)

	for _, target := range cleanTargets {
		if target.NeedsSudo && !*all {
			continue
		}

		result := analyzeTarget(target, cutoffTime)
		results = append(results, result)
	}

	printSummary(results)

	if *dryRun {
		common.Info("실제 삭제를 원하면 --dry-run 플래그 없이 실행하세요")
		return
	}

	fmt.Print("\n정리를 진행하시겠습니까? (y/N): ")
	var answer string
	fmt.Scanln(&answer)

	if strings.ToLower(answer) != "y" {
		common.Info("취소되었습니다")
		return
	}

	var totalDeleted int64
	for _, result := range results {
		if result.Error != nil || result.FilesCount == 0 {
			continue
		}
		deleted := cleanTarget(result.Target, cutoffTime)
		totalDeleted += deleted
	}

	common.Success("총 %s 정리 완료", formatSize(totalDeleted))
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

func analyzeTarget(target CleanTarget, cutoff time.Time) CleanResult {
	result := CleanResult{Target: target}
	path := expandPath(target.Path)

	info, err := os.Stat(path)
	if err != nil {
		result.Error = err
		return result
	}

	if !info.IsDir() {
		result.Error = fmt.Errorf("디렉토리가 아님")
		return result
	}

	filepath.Walk(path, func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			result.FilesCount++
			result.TotalSize += info.Size()
		}
		return nil
	})

	return result
}

func cleanTarget(target CleanTarget, cutoff time.Time) int64 {
	var deleted int64
	path := expandPath(target.Path)

	filepath.Walk(path, func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filePath); err == nil {
				deleted += info.Size()
			}
		}
		return nil
	})

	if deleted > 0 {
		common.Success("%s: %s 삭제됨", target.Description, formatSize(deleted))
	}
	return deleted
}

func printSummary(results []CleanResult) {
	common.Header("분석 결과:")
	fmt.Println()

	var totalFiles int
	var totalSize int64

	for _, r := range results {
		if r.Error != nil {
			common.Warning("%-20s: 접근 불가 (%v)", r.Target.Description, r.Error)
			continue
		}
		if r.FilesCount == 0 {
			fmt.Printf("  %-20s: 정리 대상 없음\n", r.Target.Description)
			continue
		}
		fmt.Printf("  %-20s: %d개 파일, %s\n", r.Target.Description, r.FilesCount, formatSize(r.TotalSize))
		totalFiles += r.FilesCount
		totalSize += r.TotalSize
	}

	fmt.Println()
	common.Info("총 %d개 파일, %s 정리 가능", totalFiles, formatSize(totalSize))
}

func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
