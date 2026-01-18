package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/useful-go/pkg/common"
	"github.com/useful-go/pkg/fs"
	"github.com/useful-go/pkg/text"
	"github.com/useful-go/pkg/ui"
)

// CleanTarget represents a cleanup target
type CleanTarget struct {
	Name        string
	Path        string
	Description string
	NeedsSudo   bool
	Pattern     string // glob 패턴 (예: "*/Cache*")
}

var defaultTargets = []CleanTarget{
	{Name: "Xcode DerivedData", Path: "~/Library/Developer/Xcode/DerivedData", Description: "Xcode 빌드 캐시"},
	{Name: "Xcode Archives", Path: "~/Library/Developer/Xcode/Archives", Description: "Xcode 아카이브"},
	{Name: "Xcode iOS DeviceSupport", Path: "~/Library/Developer/Xcode/iOS DeviceSupport", Description: "iOS 디바이스 지원 파일"},
	{Name: "CocoaPods Cache", Path: "~/Library/Caches/CocoaPods", Description: "CocoaPods 캐시"},
	{Name: "Homebrew Cache", Path: "~/Library/Caches/Homebrew", Description: "Homebrew 다운로드 캐시"},
	{Name: "npm Cache", Path: "~/.npm", Description: "npm 패키지 캐시"},
	{Name: "Yarn Cache", Path: "~/Library/Caches/Yarn", Description: "Yarn 캐시"},
	{Name: "pip Cache", Path: "~/Library/Caches/pip", Description: "Python pip 캐시"},
	{Name: "Go Build Cache", Path: "~/Library/Caches/go-build", Description: "Go 빌드 캐시"},
	{Name: "Gradle Cache", Path: "~/.gradle/caches", Description: "Gradle 빌드 캐시"},
	{Name: "Docker Images", Path: "", Description: "사용하지 않는 Docker 이미지 (docker system prune)", NeedsSudo: false},

	{Name: "System Caches", Path: "/Library/Caches", Description: "시스템 캐시", NeedsSudo: true},
	{Name: "System Logs", Path: "/var/log", Description: "시스템 로그", NeedsSudo: true},
	{Name: "User Caches", Path: "~/Library/Caches", Description: "사용자 앱 캐시"},
	{Name: "User Logs", Path: "~/Library/Logs", Description: "사용자 앱 로그"},

	{Name: "Temp Files", Path: "/tmp", Description: "임시 파일", NeedsSudo: true},
	{Name: "Private Temp", Path: "/private/var/folders", Description: "시스템 임시 폴더", NeedsSudo: true},

	// 패턴 기반 정리
	{Name: "App Support Caches", Path: "~/Library/Application Support", Description: "앱 서포트 내 캐시", Pattern: "*/Cache*"},
	{Name: "Container Caches", Path: "~/Library/Containers", Description: "컨테이너 앱 캐시", Pattern: "*/Data/Library/Caches"},
}

func main() {
	dryRun := flag.Bool("dry-run", false, "실제 삭제 없이 분석만 수행")
	all := flag.Bool("all", false, "sudo 필요한 시스템 경로 포함")
	docker := flag.Bool("docker", false, "Docker 정리 포함")
	flag.Parse()

	common.Header("sysclean - macOS 시스템 데이터 정리")
	fmt.Println()

	if *dryRun {
		common.Info("분석 모드 (실제 삭제하지 않음)")
		fmt.Println()
	}

	var totalSize int64
	var targets []CleanTarget

	for _, target := range defaultTargets {
		if target.NeedsSudo && !*all {
			continue
		}
		if target.Name == "Docker Images" && !*docker {
			continue
		}
		targets = append(targets, target)
	}

	results := analyzeTargets(targets)

	fmt.Println("정리 대상:")
	fmt.Println(text.Separator(70))
	fmt.Printf("%-30s %-15s %s\n", "대상", "크기", "설명")
	fmt.Println(text.Separator(70))

	for _, r := range results {
		if r.Size > 0 {
			fmt.Printf("%-30s %-15s %s\n", r.Target.Name, fs.FormatSize(r.Size), r.Target.Description)
			totalSize += r.Size
		}
	}

	fmt.Println(text.Separator(70))
	fmt.Printf("%-30s %-15s\n", "총계", fs.FormatSize(totalSize))
	fmt.Println()

	if totalSize == 0 {
		common.Success("정리할 데이터가 없습니다")
		return
	}

	if *dryRun {
		common.Info("실제 정리를 수행하려면 --dry-run 옵션을 제거하세요")
		return
	}

	confirm := ui.YesNoConfirmation("위 항목들을 정리하시겠습니까?")
	if !confirm.MustConfirm() {
		return
	}

	fmt.Println()
	cleanTargets(results, *all)
}

type AnalysisResult struct {
	Target CleanTarget
	Size   int64
	Error  error
}

func analyzeTargets(targets []CleanTarget) []AnalysisResult {
	var results []AnalysisResult

	for _, target := range targets {
		result := AnalysisResult{Target: target}

		if target.Name == "Docker Images" {
			size := getDockerSize()
			result.Size = size
		} else if target.Pattern != "" {
			// 패턴 기반 분석
			size := getPatternSize(expandPath(target.Path), target.Pattern)
			result.Size = size
		} else {
			path := expandPath(target.Path)
			size := getDirSize(path)
			result.Size = size
		}

		results = append(results, result)
	}

	return results
}

func cleanTargets(results []AnalysisResult, useSudo bool) {
	for _, r := range results {
		if r.Size == 0 {
			continue
		}

		if r.Target.Name == "Docker Images" {
			cleanDocker()
			continue
		}

		path := expandPath(r.Target.Path)

		var err error
		if r.Target.Pattern != "" {
			// 패턴 기반 정리
			err = cleanPattern(path, r.Target.Pattern, r.Target.NeedsSudo && useSudo)
		} else {
			err = cleanPath(path, r.Target.NeedsSudo && useSudo)
		}

		if err != nil {
			common.Error("%s 정리 실패: %v", r.Target.Name, err)
		} else {
			common.Success("%s 정리 완료 (%s)", r.Target.Name, fs.FormatSize(r.Size))
		}
	}
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

func getDirSize(path string) int64 {
	return fs.GetDirSize(path)
}

// getPatternSize 패턴에 매칭되는 디렉토리들의 총 크기 계산
func getPatternSize(basePath, pattern string) int64 {
	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		return 0
	}

	var totalSize int64
	matches, err := filepath.Glob(filepath.Join(basePath, pattern))
	if err != nil {
		return 0
	}

	for _, match := range matches {
		size := fs.GetDirSize(match)
		totalSize += size
	}

	return totalSize
}

func getDockerSize() int64 {
	if _, err := exec.LookPath("docker"); err != nil {
		return 0
	}

	cmd := exec.Command("docker", "system", "df", "--format", "{{.Size}}")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}

	if len(strings.TrimSpace(string(output))) > 0 {
		return 1
	}
	return 0
}

func cleanDocker() {
	cmd := exec.Command("docker", "system", "prune", "-f")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		common.Error("Docker 정리 실패: %v", err)
	} else {
		common.Success("Docker 정리 완료")
	}
}

func cleanPath(path string, useSudo bool) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	var cmd *exec.Cmd
	if useSudo {
		cmd = exec.Command("sudo", "rm", "-rf", path+"/*")
	} else {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			entryPath := filepath.Join(path, entry.Name())
			if err := os.RemoveAll(entryPath); err != nil {
				common.Warning("삭제 실패: %s", entryPath)
			}
		}
		return nil
	}

	return cmd.Run()
}

// cleanPattern 패턴에 매칭되는 디렉토리들 정리
func cleanPattern(basePath, pattern string, useSudo bool) error {
	matches, err := filepath.Glob(filepath.Join(basePath, pattern))
	if err != nil {
		return err
	}

	for _, match := range matches {
		if useSudo {
			cmd := exec.Command("sudo", "rm", "-rf", match)
			if err := cmd.Run(); err != nil {
				common.Warning("삭제 실패: %s", match)
			}
		} else {
			if err := os.RemoveAll(match); err != nil {
				common.Warning("삭제 실패: %s", match)
			}
		}
	}

	return nil
}

func formatSize(bytes int64) string {
	return fs.FormatSize(bytes)
}
