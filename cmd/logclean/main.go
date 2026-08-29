package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	usefulfs "github.com/useful-go/pkg/fs"
)

type CleanTarget struct {
	Path        string
	Description string
}

type candidate struct {
	path string
	info os.FileInfo
}

type CleanResult struct {
	Target        CleanTarget
	Root          string
	RootInfo      os.FileInfo
	Candidates    []candidate
	FilesCount    int
	TotalSize     int64
	DeletedSize   int64
	Missing       bool
	AnalysisError error
	DeleteErrors  []error
}

type cleanerOps struct {
	lstat        func(string) (os.FileInfo, error)
	evalSymlinks func(string) (string, error)
	walkDir      func(string, iofs.WalkDirFunc) error
	openRoot     func(string) (rootHandle, error)
	now          func() time.Time
}

type rootHandle interface {
	Lstat(string) (os.FileInfo, error)
	Stat(string) (os.FileInfo, error)
	Remove(string) error
	Close() error
}

var defaultCleanerOps = cleanerOps{
	lstat:        os.Lstat,
	evalSymlinks: filepath.EvalSymlinks,
	walkDir:      filepath.WalkDir,
	openRoot: func(path string) (rootHandle, error) {
		return os.OpenRoot(path)
	},
	now: time.Now,
}

type options struct {
	apply   bool
	dryRun  bool
	days    int
	caches  bool
	trash   bool
	allUser bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runWithOps(args, stdin, stdout, stderr, defaultCleanerOps)
}

func runWithOps(args []string, stdin io.Reader, stdout, stderr io.Writer, ops cleanerOps) int {
	flags := flag.NewFlagSet("logclean", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var opts options
	flags.BoolVar(&opts.apply, "apply", false, "분석 결과를 확인한 뒤 파일 삭제")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "호환 옵션: 삭제하지 않고 분석만 수행")
	flags.IntVar(&opts.days, "days", 7, "N일 이상 된 일반 파일만 대상")
	flags.BoolVar(&opts.caches, "caches", false, "사용자 Library/Caches를 대상에 추가")
	flags.BoolVar(&opts.trash, "trash", false, "사용자 휴지통을 대상에 추가")
	flags.BoolVar(&opts.allUser, "all", false, "호환 옵션: --caches와 --trash 활성화 (시스템 경로 제외)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "logclean: 예상하지 않은 인수: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	if opts.days < 0 {
		fmt.Fprintln(stderr, "logclean: --days는 0 이상이어야 합니다")
		return 2
	}
	if opts.apply && opts.dryRun {
		fmt.Fprintln(stderr, "logclean: --apply와 --dry-run은 함께 사용할 수 없습니다")
		return 2
	}
	if opts.allUser {
		opts.caches = true
		opts.trash = true
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "logclean: 홈 디렉터리 확인 실패: %v\n", err)
		return 1
	}
	targets := selectedTargets(home, opts)
	cutoff := ops.now().AddDate(0, 0, -opts.days)
	results := make([]CleanResult, 0, len(targets))
	hadError := false
	for _, target := range targets {
		result := analyzeTarget(target, cutoff, ops)
		if result.AnalysisError != nil {
			hadError = true
		}
		results = append(results, result)
	}
	printSummary(stdout, stderr, results)

	if !opts.apply {
		fmt.Fprintln(stdout, "분석 전용 모드입니다. 삭제하려면 --apply를 명시하세요.")
		if hadError {
			return 1
		}
		return 0
	}

	if countCandidates(results) == 0 {
		if hadError {
			fmt.Fprintln(stderr, "삭제 가능한 파일이 없고 분석 오류가 발생했습니다.")
			return 1
		}
		fmt.Fprintln(stdout, "삭제 대상 없음")
		return 0
	}

	confirmed, err := confirm(stdin, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "확인 입력 실패: %v\n", err)
		return 1
	}
	if !confirmed {
		fmt.Fprintln(stdout, "취소됨")
		if hadError {
			return 1
		}
		return 0
	}

	var totalDeleted int64
	for i := range results {
		if results[i].AnalysisError != nil || len(results[i].Candidates) == 0 {
			continue
		}
		cleanTarget(&results[i], cutoff, ops)
		totalDeleted += results[i].DeletedSize
		for _, deleteErr := range results[i].DeleteErrors {
			hadError = true
			fmt.Fprintf(stderr, "%s: 삭제 실패: %v\n", results[i].Target.Description, deleteErr)
		}
		if results[i].DeletedSize > 0 {
			fmt.Fprintf(stdout, "%s: %s 삭제됨\n", results[i].Target.Description, usefulfs.FormatSize(results[i].DeletedSize))
		}
	}

	if hadError {
		fmt.Fprintf(stderr, "부분 완료: %s 삭제, 하나 이상의 오류 발생\n", usefulfs.FormatSize(totalDeleted))
		return 1
	}
	fmt.Fprintf(stdout, "정리 완료: 총 %s 삭제\n", usefulfs.FormatSize(totalDeleted))
	return 0
}

func selectedTargets(home string, opts options) []CleanTarget {
	targets := []CleanTarget{
		{Path: filepath.Join(home, "Library", "Logs"), Description: "사용자 로그"},
		{Path: filepath.Join(home, "Library", "Application Support", "CrashReporter"), Description: "사용자 크래시 리포트"},
	}
	if opts.caches {
		targets = append(targets, CleanTarget{Path: filepath.Join(home, "Library", "Caches"), Description: "사용자 캐시 (opt-in)"})
	}
	if opts.trash {
		targets = append(targets, CleanTarget{Path: filepath.Join(home, ".Trash"), Description: "사용자 휴지통 (opt-in)"})
	}
	return targets
}

func analyzeTarget(target CleanTarget, cutoff time.Time, ops cleanerOps) CleanResult {
	result := CleanResult{Target: target}
	rootInfo, err := ops.lstat(target.Path)
	if errors.Is(err, os.ErrNotExist) {
		result.Missing = true
		return result
	}
	if err != nil {
		result.AnalysisError = fmt.Errorf("root lstat: %w", err)
		return result
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		result.AnalysisError = fmt.Errorf("symlink root 거부: %s", target.Path)
		return result
	}
	if !rootInfo.IsDir() {
		result.AnalysisError = fmt.Errorf("root가 디렉터리가 아님: %s", target.Path)
		return result
	}
	canonicalRoot, err := ops.evalSymlinks(target.Path)
	if err != nil {
		result.AnalysisError = fmt.Errorf("root canonical path 확인: %w", err)
		return result
	}
	canonicalRoot, err = filepath.Abs(canonicalRoot)
	if err != nil {
		result.AnalysisError = fmt.Errorf("root absolute path 확인: %w", err)
		return result
	}
	result.Root = filepath.Clean(canonicalRoot)
	result.RootInfo = rootInfo

	err = ops.walkDir(result.Root, func(path string, entry iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%s: %w", path, walkErr)
		}
		if !withinRoot(result.Root, path) {
			return fmt.Errorf("root 경계 밖 경로 거부: %s", path)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("%s: info: %w", path, infoErr)
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			return nil
		}
		result.Candidates = append(result.Candidates, candidate{path: filepath.Clean(path), info: info})
		result.FilesCount++
		result.TotalSize += info.Size()
		return nil
	})
	if err != nil {
		result.AnalysisError = fmt.Errorf("walk: %w", err)
	}
	return result
}

func cleanTarget(result *CleanResult, cutoff time.Time, ops cleanerOps) {
	root, err := ops.openRoot(result.Root)
	if err != nil {
		result.DeleteErrors = append(result.DeleteErrors, fmt.Errorf("root open: %w", err))
		return
	}
	defer func() {
		if err := root.Close(); err != nil {
			result.DeleteErrors = append(result.DeleteErrors, fmt.Errorf("root close: %w", err))
		}
	}()

	rootLstat, err := root.Lstat(".")
	if err != nil {
		result.DeleteErrors = append(result.DeleteErrors, fmt.Errorf("root lstat 재확인: %w", err))
		return
	}
	rootStat, err := root.Stat(".")
	if err != nil {
		result.DeleteErrors = append(result.DeleteErrors, fmt.Errorf("root stat 재확인: %w", err))
		return
	}
	if !rootLstat.IsDir() || !rootStat.IsDir() || !os.SameFile(rootLstat, rootStat) || !os.SameFile(result.RootInfo, rootStat) {
		result.DeleteErrors = append(result.DeleteErrors, errors.New("root가 분석 후 변경됨"))
		return
	}

	for _, item := range result.Candidates {
		size, err := deleteCandidate(root, *result, item, cutoff)
		if err != nil {
			result.DeleteErrors = append(result.DeleteErrors, fmt.Errorf("%s: %w", item.path, err))
			continue
		}
		result.DeletedSize += size
	}
}

func deleteCandidate(root rootHandle, result CleanResult, item candidate, cutoff time.Time) (int64, error) {
	if !withinRoot(result.Root, item.path) {
		return 0, errors.New("root 경계 밖 candidate")
	}
	relativePath, err := filepath.Rel(result.Root, item.path)
	if err != nil {
		return 0, fmt.Errorf("candidate relative path 확인: %w", err)
	}
	if relativePath == "." || filepath.IsAbs(relativePath) || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return 0, errors.New("root 경계 밖 relative candidate")
	}

	currentLstat, err := root.Lstat(relativePath)
	if err != nil {
		return 0, fmt.Errorf("file root lstat 재확인: %w", err)
	}
	if currentLstat.Mode()&os.ModeSymlink != 0 || !currentLstat.Mode().IsRegular() {
		return 0, errors.New("일반 파일이 아니거나 symlink임")
	}
	currentStat, err := root.Stat(relativePath)
	if err != nil {
		return 0, fmt.Errorf("file root stat 재확인: %w", err)
	}
	if !currentStat.Mode().IsRegular() || !os.SameFile(currentLstat, currentStat) {
		return 0, errors.New("lstat/stat 사이 파일 또는 symlink 변경 감지")
	}
	if !os.SameFile(item.info, currentStat) || item.info.Size() != currentStat.Size() || !item.info.ModTime().Equal(currentStat.ModTime()) {
		return 0, errors.New("파일이 분석 후 변경됨")
	}
	if !currentStat.ModTime().Before(cutoff) {
		return 0, errors.New("파일이 더 이상 age 조건을 충족하지 않음")
	}
	if err := root.Remove(relativePath); err != nil {
		return 0, err
	}
	return currentStat.Size(), nil
}

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func countCandidates(results []CleanResult) int {
	var count int
	for _, result := range results {
		if result.AnalysisError == nil {
			count += len(result.Candidates)
		}
	}
	return count
}

func confirm(stdin io.Reader, stdout io.Writer) (bool, error) {
	fmt.Fprint(stdout, "정리를 진행하시겠습니까? [y/N]: ")
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func printSummary(stdout, stderr io.Writer, results []CleanResult) {
	fmt.Fprintln(stdout, "분석 결과:")
	var totalFiles int
	var totalSize int64
	for _, result := range results {
		switch {
		case result.AnalysisError != nil:
			fmt.Fprintf(stderr, "%s: 분석 실패: %v\n", result.Target.Description, result.AnalysisError)
		case result.Missing:
			fmt.Fprintf(stdout, "  %s: 경로 없음\n", result.Target.Description)
		case result.FilesCount == 0:
			fmt.Fprintf(stdout, "  %s: 정리 대상 없음\n", result.Target.Description)
		default:
			fmt.Fprintf(stdout, "  %s: %d개 파일, %s\n", result.Target.Description, result.FilesCount, usefulfs.FormatSize(result.TotalSize))
			totalFiles += result.FilesCount
			totalSize += result.TotalSize
		}
	}
	fmt.Fprintf(stdout, "총 %d개 파일, %s 정리 가능\n", totalFiles, usefulfs.FormatSize(totalSize))
}
