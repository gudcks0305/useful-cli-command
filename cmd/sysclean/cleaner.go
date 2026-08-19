package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	usefulfs "github.com/useful-go/pkg/fs"
)

type riskLevel string
type targetKind string

const (
	riskSafe     riskLevel  = "safe"
	riskOptional riskLevel  = "optional"
	riskExplicit riskLevel  = "explicit"
	kindCache    targetKind = "cache"
	kindArtifact targetKind = "artifact"
	kindDocker   targetKind = "docker"
)

type CleanTarget struct {
	ID, Name, Description string
	Paths                 []string
	Risk                  riskLevel
	Kind                  targetKind
	RemoveRoot            bool
	PrecomputedSize       int64
}

var defaultTargets = []CleanTarget{
	{ID: "xcode-derived-data", Name: "Xcode DerivedData", Description: "Xcode 재생성 가능 빌드 데이터", Paths: []string{"~/Library/Developer/Xcode/DerivedData"}, Risk: riskSafe, Kind: kindCache},
	{ID: "xcode-doc-cache", Name: "Xcode Documentation", Description: "Xcode 문서 캐시", Paths: []string{"~/Library/Developer/Xcode/DocumentationCache"}, Risk: riskSafe, Kind: kindCache},
	{ID: "swiftpm-cache", Name: "SwiftPM Cache", Description: "Swift Package Manager 캐시", Paths: []string{"~/Library/Caches/org.swift.swiftpm"}, Risk: riskSafe, Kind: kindCache},
	{ID: "cocoapods-cache", Name: "CocoaPods Cache", Description: "CocoaPods 다운로드 캐시", Paths: []string{"~/Library/Caches/CocoaPods"}, Risk: riskSafe, Kind: kindCache},
	{ID: "homebrew-cache", Name: "Homebrew Cache", Description: "Homebrew 다운로드 캐시", Paths: []string{"~/Library/Caches/Homebrew"}, Risk: riskSafe, Kind: kindCache},
	{ID: "npm-cache", Name: "npm Cache", Description: "npm content-addressable cache만", Paths: []string{"~/.npm/_cacache"}, Risk: riskSafe, Kind: kindCache},
	{ID: "yarn-cache", Name: "Yarn Cache", Description: "Yarn 다운로드 캐시", Paths: []string{"~/Library/Caches/Yarn", "~/.yarn/cache"}, Risk: riskSafe, Kind: kindCache},
	{ID: "pnpm-store", Name: "pnpm Store", Description: "pnpm package store", Paths: []string{"~/Library/pnpm/store"}, Risk: riskSafe, Kind: kindCache},
	{ID: "bun-cache", Name: "Bun Cache", Description: "Bun package cache", Paths: []string{"~/.bun/install/cache"}, Risk: riskSafe, Kind: kindCache},
	{ID: "pip-cache", Name: "pip Cache", Description: "pip wheel/download cache", Paths: []string{"~/Library/Caches/pip"}, Risk: riskSafe, Kind: kindCache},
	{ID: "uv-cache", Name: "uv Cache", Description: "uv package cache", Paths: []string{"~/.cache/uv", "~/Library/Caches/uv"}, Risk: riskSafe, Kind: kindCache},
	{ID: "go-build-cache", Name: "Go Build Cache", Description: "Go build cache", Paths: []string{"~/Library/Caches/go-build"}, Risk: riskSafe, Kind: kindCache},
	{ID: "gradle-cache", Name: "Gradle Cache", Description: "Gradle dependency/build cache", Paths: []string{"~/.gradle/caches"}, Risk: riskSafe, Kind: kindCache},
	{ID: "composer-cache", Name: "Composer Cache", Description: "Composer package cache", Paths: []string{"~/Library/Caches/composer"}, Risk: riskSafe, Kind: kindCache},
	{ID: "xcode-device-support", Name: "Xcode DeviceSupport", Description: "필요 시 다시 생성되는 디버깅 심볼", Paths: []string{"~/Library/Developer/Xcode/iOS DeviceSupport"}, Risk: riskOptional, Kind: kindCache},
	{ID: "maven-cache", Name: "Maven Repository", Description: "재다운로드 필요한 로컬 Maven 저장소", Paths: []string{"~/.m2/repository"}, Risk: riskOptional, Kind: kindCache},
	{ID: "cargo-cache", Name: "Cargo Registry Cache", Description: "재다운로드 필요한 Cargo registry cache", Paths: []string{"~/.cargo/registry/cache", "~/.cargo/git/db"}, Risk: riskOptional, Kind: kindCache},
	{ID: "playwright-browsers", Name: "Playwright Browsers", Description: "재다운로드 필요한 Playwright 브라우저", Paths: []string{"~/Library/Caches/ms-playwright"}, Risk: riskOptional, Kind: kindCache},
	{ID: "chrome-cache", Name: "Chrome Cache", Description: "Chrome 종료 후 권장; History/Cookies 제외", Paths: []string{"~/Library/Caches/Google/Chrome"}, Risk: riskOptional, Kind: kindCache},
	{ID: "jetbrains-cache", Name: "JetBrains Cache", Description: "JetBrains IDE 재생성 가능 캐시", Paths: []string{"~/Library/Caches/JetBrains"}, Risk: riskOptional, Kind: kindCache},
	{ID: "trash", Name: "Trash", Description: "복구 불가능하게 휴지통 비움", Paths: []string{"~/.Trash"}, Risk: riskExplicit, Kind: kindCache},
	{ID: "docker", Name: "Docker Unused Data", Description: "중지 container, unused network, dangling image, build cache; volume 제외", Risk: riskExplicit, Kind: kindDocker},
}

type AnalysisResult struct {
	Target CleanTarget
	Size   int64
	Error  error
}

func analyzeTargets(targets []CleanTarget, workers int) []AnalysisResult {
	return parallelMap(targets, workers, func(target CleanTarget) AnalysisResult {
		result := AnalysisResult{Target: target}
		if target.PrecomputedSize > 0 {
			result.Size = target.PrecomputedSize
			return result
		}
		if target.Kind == kindDocker {
			result.Size, result.Error = getDockerSize()
			return result
		}
		for _, rawPath := range target.Paths {
			size, err := getPathSize(usefulfs.ExpandPath(rawPath))
			result.Size += size
			if err != nil {
				result.Error = errors.Join(result.Error, err)
			}
		}
		return result
	})
}

func cleanTargets(results []AnalysisResult, workers int) []AnalysisResult {
	return parallelMap(results, workers, func(result AnalysisResult) AnalysisResult {
		result.Error = nil
		if result.Target.Kind == kindDocker {
			result.Error = cleanDocker()
			return result
		}
		for _, rawPath := range result.Target.Paths {
			path := usefulfs.ExpandPath(rawPath)
			var err error
			if result.Target.RemoveRoot {
				err = removeRoot(path)
			} else {
				err = removeContents(path)
			}
			if err != nil {
				result.Error = errors.Join(result.Error, fmt.Errorf("%s: %w", path, err))
			}
		}
		return result
	})
}

func parallelMap[T any, R any](items []T, workers int, fn func(T) R) []R {
	results := make([]R, len(items))
	if len(items) == 0 {
		return results
	}
	workers = normalizedWorkers(workers, len(items))
	jobs := make(chan int)
	done := make(chan struct{}, workers)
	for range workers {
		go func() {
			for index := range jobs {
				results[index] = fn(items[index])
			}
			done <- struct{}{}
		}()
	}
	for index := range items {
		jobs <- index
	}
	close(jobs)
	for range workers {
		<-done
	}
	return results
}

func getPathSize(path string) (int64, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return info.Size(), nil
	}
	var size int64
	err = filepath.WalkDir(path, func(_ string, entry iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func removeContents(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("symlink 디렉터리 거부")
	}
	if !info.IsDir() {
		return fmt.Errorf("디렉터리가 아님")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	var joined error
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(path, entry.Name())); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

func removeRoot(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("symlink 대상 거부")
	}
	return os.RemoveAll(path)
}

type dockerDFRow struct {
	Type        string `json:"Type"`
	Reclaimable string `json:"Reclaimable"`
}

func getDockerSize() (int64, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "system", "df", "--format", "{{json .}}").Output()
	if ctx.Err() != nil {
		return 0, fmt.Errorf("docker 응답 시간 초과")
	}
	if err != nil {
		return 0, err
	}
	return parseDockerDF(output)
}

func parseDockerDF(output []byte) (int64, error) {
	var total int64
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		var row dockerDFRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return 0, err
		}
		if strings.EqualFold(row.Type, "Local Volumes") {
			continue
		}
		fields := strings.Fields(row.Reclaimable)
		if len(fields) == 0 {
			continue
		}
		size, err := parseDockerSize(fields[0])
		if err != nil {
			return 0, err
		}
		total += size
	}
	return total, nil
}

func parseDockerSize(value string) (int64, error) {
	value = strings.TrimSpace(value)
	index := 0
	for index < len(value) && (value[index] >= '0' && value[index] <= '9' || value[index] == '.') {
		index++
	}
	if index == 0 {
		return 0, fmt.Errorf("Docker 크기 해석 실패: %q", value)
	}
	number, err := strconv.ParseFloat(value[:index], 64)
	if err != nil {
		return 0, err
	}
	units := map[string]float64{"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30, "TIB": 1 << 40}
	multiplier, ok := units[strings.ToUpper(strings.TrimSpace(value[index:]))]
	if !ok {
		return 0, fmt.Errorf("Docker 크기 단위 해석 실패: %q", value[index:])
	}
	return int64(number * multiplier), nil
}

func cleanDocker() error {
	cmd := exec.Command("docker", "system", "prune", "-f")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func artifactTargets(artifacts []ProjectArtifact) []CleanTarget {
	targets := make([]CleanTarget, 0, len(artifacts))
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	for i, artifact := range artifacts {
		targets = append(targets, CleanTarget{ID: fmt.Sprintf("project-%d", i+1), Name: artifact.Type, Description: artifact.Path, Paths: []string{artifact.Path}, Risk: riskExplicit, Kind: kindArtifact, RemoveRoot: true})
		targets[len(targets)-1].PrecomputedSize = artifact.Size
	}
	return targets
}
