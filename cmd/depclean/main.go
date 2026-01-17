package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/useful-go/pkg/common"
)

// DependencyType represents a type of dependency folder
type DependencyType struct {
	Name        string   // 표시 이름
	Folders     []string // 찾을 폴더 이름들
	Description string   // 설명
	Indicator   string   // 프로젝트 판별 파일 (예: package.json)
}

var dependencyTypes = []DependencyType{
	{
		Name:        "Node.js",
		Folders:     []string{"node_modules"},
		Description: "npm/yarn/pnpm 패키지",
		Indicator:   "package.json",
	},
	{
		Name:        "Python venv",
		Folders:     []string{"venv", ".venv", "env", ".env", "__pycache__"},
		Description: "Python 가상환경 및 캐시",
		Indicator:   "requirements.txt",
	},
	{
		Name:        "Go",
		Folders:     []string{"vendor"},
		Description: "Go vendor 모듈",
		Indicator:   "go.mod",
	},
	{
		Name:        "Gradle",
		Folders:     []string{".gradle", "build"},
		Description: "Gradle 캐시 및 빌드",
		Indicator:   "build.gradle",
	},
	{
		Name:        "Maven",
		Folders:     []string{"target"},
		Description: "Maven 빌드 결과물",
		Indicator:   "pom.xml",
	},
	{
		Name:        "Rust",
		Folders:     []string{"target"},
		Description: "Cargo 빌드 결과물",
		Indicator:   "Cargo.toml",
	},
	{
		Name:        "Ruby",
		Folders:     []string{"vendor/bundle", ".bundle"},
		Description: "Bundler 패키지",
		Indicator:   "Gemfile",
	},
	{
		Name:        "PHP",
		Folders:     []string{"vendor"},
		Description: "Composer 패키지",
		Indicator:   "composer.json",
	},
	{
		Name:        ".NET",
		Folders:     []string{"bin", "obj", "packages"},
		Description: ".NET 빌드 및 패키지",
		Indicator:   "*.csproj",
	},
	{
		Name:        "iOS/macOS",
		Folders:     []string{"Pods", "DerivedData"},
		Description: "CocoaPods 및 Xcode 빌드",
		Indicator:   "Podfile",
	},
}

type FoundDependency struct {
	ProjectPath string
	DepPath     string
	DepType     string
	Size        int64
	LastAccess  time.Time
	DaysSince   int
}

func main() {
	dryRun := flag.Bool("dry-run", false, "실제 삭제 없이 분석만 수행")
	days := flag.Int("days", 30, "마지막 접근 이후 경과 일수 (기본: 30일)")
	scanPath := flag.String("path", ".", "검색할 디렉토리 (기본: 현재 디렉토리)")
	maxDepth := flag.Int("depth", 5, "검색 깊이 제한 (기본: 5)")
	minSize := flag.String("min-size", "0", "최소 크기 필터 (예: 100MB, 1GB)")
	flag.Parse()

	// 경로 처리
	searchPath := expandPath(*scanPath)
	if !filepath.IsAbs(searchPath) {
		cwd, _ := os.Getwd()
		searchPath = filepath.Join(cwd, searchPath)
	}

	minSizeBytes := parseSize(*minSize)

	common.Header("depclean - 오래된 프로젝트 의존성 정리")
	fmt.Println()
	common.Info("검색 경로: %s", searchPath)
	common.Info("기준: %d일 이상 미접근", *days)
	if minSizeBytes > 0 {
		common.Info("최소 크기: %s", formatSize(minSizeBytes))
	}
	fmt.Println()

	if *dryRun {
		common.Info("분석 모드 (실제 삭제하지 않음)")
		fmt.Println()
	}

	// 의존성 검색
	found := scanDependencies(searchPath, *maxDepth, *days, minSizeBytes)

	if len(found) == 0 {
		common.Success("%d일 이상 미접근 의존성이 없습니다", *days)
		return
	}

	// 결과 출력
	var totalSize int64
	fmt.Println("발견된 오래된 의존성:")
	fmt.Println(strings.Repeat("-", 90))
	fmt.Printf("%-40s %-12s %-10s %s\n", "프로젝트", "타입", "크기", "미접근")
	fmt.Println(strings.Repeat("-", 90))

	for _, dep := range found {
		projectName := truncatePath(dep.ProjectPath, 38)
		fmt.Printf("%-40s %-12s %-10s %d일\n",
			projectName, dep.DepType, formatSize(dep.Size), dep.DaysSince)
		totalSize += dep.Size
	}

	fmt.Println(strings.Repeat("-", 90))
	fmt.Printf("%-40s %-12s %-10s\n", fmt.Sprintf("총 %d개", len(found)), "", formatSize(totalSize))
	fmt.Println()

	if *dryRun {
		common.Info("실제 정리를 수행하려면 --dry-run 옵션을 제거하세요")
		return
	}

	// 확인
	fmt.Print("위 항목들을 삭제하시겠습니까? (y/N): ")
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))

	if answer != "y" && answer != "yes" {
		common.Info("취소되었습니다")
		return
	}

	fmt.Println()

	// 삭제 수행
	var deletedCount int
	var deletedSize int64
	for _, dep := range found {
		err := os.RemoveAll(dep.DepPath)
		if err != nil {
			common.Error("삭제 실패: %s - %v", dep.DepPath, err)
		} else {
			deletedCount++
			deletedSize += dep.Size
			common.Success("삭제: %s (%s)", truncatePath(dep.DepPath, 50), formatSize(dep.Size))
		}
	}

	fmt.Println()
	common.Success("완료: %d개 삭제, %s 확보", deletedCount, formatSize(deletedSize))
}

func scanDependencies(root string, maxDepth, days int, minSize int64) []FoundDependency {
	var found []FoundDependency
	cutoffTime := time.Now().AddDate(0, 0, -days)

	// 제외할 디렉토리
	skipDirs := map[string]bool{
		".git":         true,
		".svn":         true,
		".hg":          true,
		"node_modules": true, // 하위 검색 방지
		"vendor":       true,
		".gradle":      true,
		"target":       true,
		"build":        true,
		"venv":         true,
		".venv":        true,
	}

	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		// 깊이 체크
		relPath, _ := filepath.Rel(root, path)
		depth := strings.Count(relPath, string(filepath.Separator))
		if depth > maxDepth {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if !info.IsDir() {
			return nil
		}

		dirName := info.Name()

		// 숨김 폴더 및 제외 폴더 스킵
		if dirName != "." && strings.HasPrefix(dirName, ".") && dirName != ".gradle" && dirName != ".venv" && dirName != ".env" && dirName != ".bundle" {
			return filepath.SkipDir
		}

		// 의존성 폴더인지 확인
		for _, depType := range dependencyTypes {
			for _, folder := range depType.Folders {
				if dirName == folder || (strings.Contains(folder, "/") && strings.HasSuffix(path, folder)) {
					// 프로젝트 루트 찾기
					projectPath := filepath.Dir(path)

					// 인디케이터 파일 확인 (선택적)
					if depType.Indicator != "" && !strings.Contains(depType.Indicator, "*") {
						indicatorPath := filepath.Join(projectPath, depType.Indicator)
						if _, err := os.Stat(indicatorPath); os.IsNotExist(err) {
							// 인디케이터가 없으면 해당 타입이 아닐 수 있음
							// 하지만 node_modules 같은 경우는 어쨌든 정리 대상
							if folder != "node_modules" && folder != "__pycache__" {
								continue
							}
						}
					}

					// 마지막 접근 시간 확인
					lastAccess := getLastAccessTime(path)
					if lastAccess.After(cutoffTime) {
						return filepath.SkipDir
					}

					// 크기 계산
					size := getDirSize(path)
					if size < minSize {
						return filepath.SkipDir
					}

					daysSince := int(time.Since(lastAccess).Hours() / 24)

					found = append(found, FoundDependency{
						ProjectPath: projectPath,
						DepPath:     path,
						DepType:     depType.Name,
						Size:        size,
						LastAccess:  lastAccess,
						DaysSince:   daysSince,
					})

					return filepath.SkipDir
				}
			}
		}

		// 제외 디렉토리 체크 (의존성 내부 검색 방지)
		if skipDirs[dirName] {
			return filepath.SkipDir
		}

		return nil
	})

	return found
}

func getLastAccessTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Now()
	}

	// ModTime을 기준으로 사용 (접근 시간은 OS에 따라 다를 수 있음)
	modTime := info.ModTime()

	// 하위 파일들 중 가장 최근 수정 시간 찾기
	filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.ModTime().After(modTime) {
			modTime = info.ModTime()
		}
		return nil
	})

	return modTime
}

func getDirSize(path string) int64 {
	var size int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

func truncatePath(path string, maxLen int) string {
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(path, home) {
		path = "~" + path[len(home):]
	}
	if len(path) <= maxLen {
		return path
	}
	return "..." + path[len(path)-maxLen+3:]
}

func parseSize(s string) int64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "0" || s == "" {
		return 0
	}

	multiplier := int64(1)
	if strings.HasSuffix(s, "GB") {
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	} else if strings.HasSuffix(s, "MB") {
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	} else if strings.HasSuffix(s, "KB") {
		multiplier = 1024
		s = strings.TrimSuffix(s, "KB")
	}

	var value float64
	fmt.Sscanf(strings.TrimSpace(s), "%f", &value)
	return int64(value * float64(multiplier))
}

func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
