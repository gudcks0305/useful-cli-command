package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/useful-go/pkg/text"
	"github.com/useful-go/pkg/ui"
)

const maxPadding = 1024

var numberRegex = regexp.MustCompile(`(\d+)`)

type FileInfo struct {
	SrcPath  string
	RelPath  string
	FileName string
	DirPath  string
	Info     fs.FileInfo
}

type Operation struct {
	SrcPath       string
	RelPath       string
	NewName       string
	SourceInfo    fs.FileInfo
	SourceSize    int64
	SourceModTime time.Time
}

type skippedEntry struct {
	RelPath string
	Reason  string
}

type flattenPlan struct {
	SrcDir         string
	SrcInfo        fs.FileInfo
	DestDir        string
	DestAnchor     string
	DestAnchorInfo fs.FileInfo
	DestComponents []string
	PadWidth       int
	Operations     []Operation
	Skipped        []skippedEntry
}

type destinationLocation struct {
	path       string
	anchor     string
	anchorInfo fs.FileInfo
	components []string
}

type executionHooks struct {
	beforeCopyValidation  func(Operation)
	afterSourceValidation func(Operation)
	afterCopies           func()
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runWithHooks(args, stdin, stdout, stderr, nil)
}

func runWithHooks(args []string, stdin io.Reader, stdout, stderr io.Writer, hooks *executionHooks) int {
	flags := flag.NewFlagSet("flatten", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dryRun := flags.Bool("dry-run", false, "실제 복사 없이 결과만 미리보기")
	output := flags.String("output", "", "출력 폴더 (기본값: <folder>_flattened)")
	separator := flags.String("sep", "_", "폴더명과 파일명 사이 구분자")
	padding := flags.Int("pad", 0, "숫자 패딩 자릿수 (0=자동 계산)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "사용법: flatten [options] <folder>")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "대상 폴더를 정확히 하나 지정해주세요")
		flags.Usage()
		return 2
	}
	if err := validateOptions(*separator, *padding); err != nil {
		fmt.Fprintf(stderr, "잘못된 옵션: %v\n", err)
		return 2
	}

	srcDir := flags.Arg(0)
	destDir := *output
	if destDir == "" {
		destDir = srcDir + "_flattened"
	}

	plan, err := buildPlan(srcDir, destDir, *separator, *padding)
	if err != nil {
		fmt.Fprintf(stderr, "preflight 실패: %v\n", err)
		return 2
	}
	printSkipped(stderr, plan.Skipped)
	if len(plan.Operations) == 0 {
		fmt.Fprintln(stdout, "처리할 일반 파일이 없습니다")
		return 0
	}

	printPlan(stdout, plan)
	if *dryRun {
		fmt.Fprintln(stdout, "\nDry-run 모드: 실제 파일 복사 없음")
		return 0
	}

	srcRoot, err := openVerifiedRoot(plan.SrcDir, plan.SrcInfo)
	if err != nil {
		fmt.Fprintf(stderr, "원본 루트 열기 실패: %v\n", err)
		return 1
	}
	defer srcRoot.Close()
	destAnchorRoot, err := openVerifiedRoot(plan.DestAnchor, plan.DestAnchorInfo)
	if err != nil {
		fmt.Fprintf(stderr, "출력 상위 루트 열기 실패: %v\n", err)
		return 1
	}
	defer destAnchorRoot.Close()

	confirmed, err := ui.ConfirmYesNoE("\n진행하시겠습니까?", stdin, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "확인 I/O 실패: %v\n", err)
		return 1
	}
	if !confirmed {
		if _, err := fmt.Fprintln(stdout, "취소되었습니다."); err != nil {
			fmt.Fprintf(stderr, "취소 출력 실패: %v\n", err)
			return 1
		}
		return 0
	}
	if err := verifyRootPath(plan.SrcDir, plan.SrcInfo, srcRoot); err != nil {
		fmt.Fprintf(stderr, "원본 루트가 확인 후 변경됨: %v\n", err)
		return 1
	}
	if err := verifyRootPath(plan.DestAnchor, plan.DestAnchorInfo, destAnchorRoot); err != nil {
		fmt.Fprintf(stderr, "출력 상위 루트가 확인 후 변경됨: %v\n", err)
		return 1
	}
	destRoot, err := createDestinationRoot(destAnchorRoot, plan.DestComponents)
	if err != nil {
		fmt.Fprintf(stderr, "출력 폴더 생성 실패: %v\n", err)
		return 1
	}
	if destRoot != destAnchorRoot {
		defer destRoot.Close()
	}
	destRootInfo, err := destRoot.Stat(".")
	if err != nil {
		fmt.Fprintf(stderr, "출력 루트 확인 실패: %v\n", err)
		return 1
	}
	if err := verifyRootPath(plan.DestDir, destRootInfo, destRoot); err != nil {
		fmt.Fprintf(stderr, "출력 루트 경로 불일치: %v\n", err)
		return 1
	}

	var success, failed int
	for _, op := range plan.Operations {
		if hooks != nil && hooks.beforeCopyValidation != nil {
			hooks.beforeCopyValidation(op)
		}
		var afterSourceValidation func()
		if hooks != nil && hooks.afterSourceValidation != nil {
			afterSourceValidation = func() { hooks.afterSourceValidation(op) }
		}
		if err := copyFile(srcRoot, destRoot, op, afterSourceValidation); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", op.NewName, err)
			failed++
			continue
		}
		success++
	}
	if hooks != nil && hooks.afterCopies != nil {
		hooks.afterCopies()
	}
	if err := verifyRootPath(plan.DestDir, destRootInfo, destRoot); err != nil {
		fmt.Fprintf(stderr, "출력 루트가 복사 중 변경됨: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "\n완료: %d개 성공, %d개 실패\n", success, failed)
	if failed != 0 {
		return 1
	}
	return 0
}

func validateOptions(separator string, padding int) error {
	if separator == "" {
		return errors.New("--sep는 비어 있을 수 없습니다")
	}
	if strings.ContainsAny(separator, "/\\\x00") {
		return errors.New("--sep에 경로 구분자나 NUL을 사용할 수 없습니다")
	}
	if padding < 0 || padding > maxPadding {
		return fmt.Errorf("--pad는 0부터 %d 사이여야 합니다", maxPadding)
	}
	return nil
}

func buildPlan(srcDir, destDir, separator string, padding int) (flattenPlan, error) {
	var plan flattenPlan

	srcAbs, err := filepath.Abs(srcDir)
	if err != nil {
		return plan, fmt.Errorf("원본 경로 확인: %w", err)
	}
	srcInfo, err := os.Lstat(srcAbs)
	if err != nil {
		return plan, fmt.Errorf("원본 경로 확인: %w", err)
	}
	if srcInfo.Mode()&os.ModeSymlink != 0 {
		return plan, errors.New("원본 루트가 심볼릭 링크입니다")
	}
	if !srcInfo.IsDir() {
		return plan, errors.New("원본 경로가 디렉터리가 아닙니다")
	}
	srcCanonical, err := filepath.EvalSymlinks(srcAbs)
	if err != nil {
		return plan, fmt.Errorf("원본 경로 정규화: %w", err)
	}

	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return plan, fmt.Errorf("출력 경로 확인: %w", err)
	}
	destination, err := canonicalDestination(destAbs)
	if err != nil {
		return plan, err
	}
	inside, err := pathWithin(srcCanonical, destination.path)
	if err != nil {
		return plan, fmt.Errorf("원본/출력 경로 비교: %w", err)
	}
	if inside {
		if samePath(srcCanonical, destination.path) {
			return plan, errors.New("출력 폴더가 원본 폴더와 같습니다")
		}
		return plan, errors.New("출력 폴더가 원본 폴더 내부에 있습니다")
	}

	files, skipped, err := collectFiles(srcCanonical)
	if err != nil {
		return plan, fmt.Errorf("파일 수집: %w", err)
	}
	padWidth := padding
	if padWidth == 0 {
		padWidth = calculatePadding(files)
	}
	operations := planOperations(files, separator, padWidth)
	sortOperations(operations)
	if err := validateOperations(operations, destination.path); err != nil {
		return plan, err
	}

	plan = flattenPlan{
		SrcDir:         srcCanonical,
		SrcInfo:        srcInfo,
		DestDir:        destination.path,
		DestAnchor:     destination.anchor,
		DestAnchorInfo: destination.anchorInfo,
		DestComponents: destination.components,
		PadWidth:       padWidth,
		Operations:     operations,
		Skipped:        skipped,
	}
	return plan, nil
}

func canonicalDestination(path string) (destinationLocation, error) {
	var location destinationLocation
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return location, errors.New("출력 루트가 심볼릭 링크입니다")
		}
		if !info.IsDir() {
			return location, errors.New("출력 경로가 디렉터리가 아닙니다")
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return location, fmt.Errorf("출력 경로 정규화: %w", err)
		}
		resolvedInfo, err := os.Lstat(resolved)
		if err != nil {
			return location, fmt.Errorf("출력 경로 확인: %w", err)
		}
		return destinationLocation{
			path:       resolved,
			anchor:     resolved,
			anchorInfo: resolvedInfo,
		}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return location, fmt.Errorf("출력 경로 확인: %w", err)
	}

	current := filepath.Clean(path)
	var missing []string
	for {
		info, err = os.Lstat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return location, fmt.Errorf("출력 상위 경로 확인: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return location, errors.New("출력 경로의 기존 상위 폴더를 찾을 수 없습니다")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return location, fmt.Errorf("출력 상위 경로가 디렉터리가 아닙니다: %s", current)
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return location, fmt.Errorf("출력 상위 경로 정규화: %w", err)
	}
	resolvedInfo, err := os.Lstat(resolved)
	if err != nil {
		return location, fmt.Errorf("출력 상위 경로 확인: %w", err)
	}
	if !resolvedInfo.IsDir() {
		return location, fmt.Errorf("출력 상위 경로가 디렉터리가 아닙니다: %s", current)
	}
	anchor := filepath.Clean(resolved)
	components := make([]string, len(missing))
	for i := len(missing) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, missing[i])
		components[len(missing)-1-i] = missing[i]
	}
	return destinationLocation{
		path:       filepath.Clean(resolved),
		anchor:     anchor,
		anchorInfo: resolvedInfo,
		components: components,
	}, nil
}

func pathWithin(base, target string) (bool, error) {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false, err
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))), nil
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func collectFiles(root string) ([]FileInfo, []skippedEntry, error) {
	var files []FileInfo
	var skipped []skippedEntry
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			skipped = append(skipped, skippedEntry{RelPath: relPath, Reason: "symlink"})
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			skipped = append(skipped, skippedEntry{RelPath: relPath, Reason: "non-regular"})
			return nil
		}
		files = append(files, FileInfo{
			SrcPath:  path,
			RelPath:  relPath,
			FileName: entry.Name(),
			DirPath:  filepath.Dir(relPath),
			Info:     info,
		})
		return nil
	})
	return files, skipped, err
}

func calculatePadding(files []FileInfo) int {
	maxDigits := 0
	for _, file := range files {
		nameWithoutExt := strings.TrimSuffix(file.FileName, filepath.Ext(file.FileName))
		for _, match := range numberRegex.FindAllString(nameWithoutExt, -1) {
			digits := strings.TrimLeft(match, "0")
			if digits == "" {
				digits = "0"
			}
			if len(digits) > maxDigits {
				maxDigits = len(digits)
			}
		}
	}
	if maxDigits < 2 {
		return 2
	}
	return maxDigits
}

func planOperations(files []FileInfo, separator string, padWidth int) []Operation {
	operations := make([]Operation, 0, len(files))
	for _, file := range files {
		newName := padNumbers(file.FileName, padWidth)
		if file.DirPath != "." {
			dirPart := strings.ReplaceAll(file.DirPath, string(os.PathSeparator), separator)
			newName = dirPart + separator + newName
		}
		operations = append(operations, Operation{
			SrcPath:       file.SrcPath,
			RelPath:       file.RelPath,
			NewName:       newName,
			SourceInfo:    file.Info,
			SourceSize:    file.Info.Size(),
			SourceModTime: file.Info.ModTime(),
		})
	}
	return operations
}

func sortOperations(operations []Operation) {
	sort.Slice(operations, func(i, j int) bool {
		a, b := operations[i], operations[j]
		if a.NewName == b.NewName {
			return a.RelPath < b.RelPath
		}
		if naturalLess(a.NewName, b.NewName) {
			return true
		}
		if naturalLess(b.NewName, a.NewName) {
			return false
		}
		return a.NewName < b.NewName
	})
}

func validateOperations(operations []Operation, destDir string) error {
	seen := make(map[string]string, len(operations))
	for _, op := range operations {
		destPath := filepath.Join(destDir, op.NewName)
		if previous, exists := seen[destPath]; exists {
			return fmt.Errorf("출력 이름 충돌: %q와 %q -> %q", previous, op.RelPath, op.NewName)
		}
		seen[destPath] = op.RelPath
		if samePath(op.SrcPath, destPath) {
			return fmt.Errorf("원본과 출력 파일이 같습니다: %s", op.RelPath)
		}
		destInfo, err := os.Lstat(destPath)
		if err == nil {
			srcInfo, srcErr := os.Lstat(op.SrcPath)
			if srcErr == nil && os.SameFile(srcInfo, destInfo) {
				return fmt.Errorf("원본과 출력 파일이 같습니다: %s", op.RelPath)
			}
			return fmt.Errorf("출력 대상이 이미 존재합니다: %s", destPath)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("출력 대상 확인 실패 %s: %w", destPath, err)
		}
	}
	return nil
}

func padNumbers(value string, width int) string {
	if width == 0 {
		return value
	}
	ext := filepath.Ext(value)
	nameWithoutExt := strings.TrimSuffix(value, ext)
	padded := numberRegex.ReplaceAllStringFunc(nameWithoutExt, func(match string) string {
		digits := strings.TrimLeft(match, "0")
		if digits == "" {
			digits = "0"
		}
		if len(digits) >= width {
			return digits
		}
		return strings.Repeat("0", width-len(digits)) + digits
	})
	return padded + ext
}

func naturalLess(a, b string) bool {
	return text.NaturalLess(a, b)
}

func printSkipped(w io.Writer, skipped []skippedEntry) {
	for _, entry := range skipped {
		fmt.Fprintf(w, "건너뜀 (%s): %s\n", entry.Reason, entry.RelPath)
	}
}

func printPlan(w io.Writer, plan flattenPlan) {
	fmt.Fprintln(w, "📁 Flatten 작업 계획")
	fmt.Fprintf(w, "원본: %s\n", plan.SrcDir)
	fmt.Fprintf(w, "대상: %s\n", plan.DestDir)
	fmt.Fprintf(w, "파일 수: %d\n", len(plan.Operations))
	fmt.Fprintf(w, "숫자 패딩: %d자리\n\n", plan.PadWidth)
	for _, op := range plan.Operations {
		fmt.Fprintf(w, "  %s → %s\n", op.RelPath, op.NewName)
	}
}

func openVerifiedRoot(path string, expected fs.FileInfo) (*os.Root, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	if err := verifyRootPath(path, expected, root); err != nil {
		closeErr := root.Close()
		return nil, errors.Join(err, closeErr)
	}
	return root, nil
}

func verifyRootPath(path string, expected fs.FileInfo, root *os.Root) error {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() || !os.SameFile(expected, before) {
		return errors.New("경로가 다른 디렉터리 또는 심볼릭 링크로 변경되었습니다")
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.IsDir() ||
		!os.SameFile(expected, rootInfo) || !os.SameFile(expected, after) {
		return errors.New("열린 루트와 현재 경로가 다릅니다")
	}
	return nil
}

func createDestinationRoot(anchor *os.Root, components []string) (*os.Root, error) {
	if len(components) == 0 {
		return anchor, nil
	}

	current := anchor
	currentOwned := false
	for _, component := range components {
		if component == "" || component == "." || component == ".." ||
			strings.ContainsAny(component, "/\\\x00") {
			if currentOwned {
				_ = current.Close()
			}
			return nil, fmt.Errorf("안전하지 않은 출력 경로 구성요소: %q", component)
		}
		if _, err := current.Lstat(component); err == nil {
			if currentOwned {
				_ = current.Close()
			}
			return nil, fmt.Errorf("출력 경로가 preflight 후 나타났습니다: %s", component)
		} else if !errors.Is(err, os.ErrNotExist) {
			if currentOwned {
				_ = current.Close()
			}
			return nil, err
		}
		if err := current.Mkdir(component, 0o755); err != nil {
			if currentOwned {
				_ = current.Close()
			}
			return nil, err
		}
		child, err := openVerifiedChildRoot(current, component)
		if err != nil {
			if currentOwned {
				_ = current.Close()
			}
			return nil, err
		}
		if currentOwned {
			if err := current.Close(); err != nil {
				_ = child.Close()
				return nil, err
			}
		}
		current = child
		currentOwned = true
	}
	return current, nil
}

func openVerifiedChildRoot(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, errors.New("출력 경로 구성요소가 디렉터리가 아니거나 심볼릭 링크입니다")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	childInfo, err := child.Stat(".")
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	after, err := parent.Lstat(name)
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.IsDir() ||
		!os.SameFile(before, childInfo) || !os.SameFile(before, after) {
		_ = child.Close()
		return nil, errors.New("출력 경로 구성요소가 여는 동안 변경되었습니다")
	}
	return child, nil
}

func copyFile(srcRoot, dstRoot *os.Root, op Operation, afterSourceValidation func()) (resultErr error) {
	srcFile, err := srcRoot.Open(op.RelPath)
	if err != nil {
		return err
	}
	defer func() {
		if srcFile != nil {
			resultErr = errors.Join(resultErr, closeWithContext(srcFile, "원본 파일 닫기"))
		}
	}()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}
	if !srcInfo.Mode().IsRegular() {
		return errors.New("원본이 일반 파일이 아닙니다")
	}
	currentInfo, err := srcRoot.Lstat(op.RelPath)
	if err != nil {
		return err
	}
	if currentInfo.Mode()&os.ModeSymlink != 0 ||
		!sourceMatchesSnapshot(op, srcInfo) || !sourceMatchesSnapshot(op, currentInfo) {
		return errors.New("원본이 preflight 후 변경되었거나 심볼릭 링크입니다")
	}

	dstFile, err := dstRoot.OpenFile(op.NewName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, srcInfo.Mode().Perm())
	if err != nil {
		return err
	}
	createdInfo, statErr := dstFile.Stat()
	created := true
	defer func() {
		if dstFile != nil {
			resultErr = errors.Join(resultErr, closeWithContext(dstFile, "출력 파일 닫기"))
		}
		if resultErr != nil && created {
			resultErr = errors.Join(resultErr, removeCreatedFile(dstRoot, op.NewName, createdInfo))
		}
	}()
	if statErr != nil {
		return statErr
	}

	if afterSourceValidation != nil {
		afterSourceValidation()
	}
	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}
	afterCopyInfo, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("복사 후 원본 확인: %w", err)
	}
	afterCopyPathInfo, err := srcRoot.Lstat(op.RelPath)
	if err != nil {
		return fmt.Errorf("복사 후 원본 경로 확인: %w", err)
	}
	if afterCopyPathInfo.Mode()&os.ModeSymlink != 0 ||
		!sourceMatchesSnapshot(op, afterCopyInfo) || !sourceMatchesSnapshot(op, afterCopyPathInfo) {
		return errors.New("원본이 복사 중 변경되었습니다")
	}
	if err := dstFile.Chmod(srcInfo.Mode().Perm()); err != nil {
		return err
	}
	if err := dstFile.Sync(); err != nil {
		return err
	}
	if err := dstFile.Close(); err != nil {
		dstFile = nil
		return fmt.Errorf("출력 파일 닫기: %w", err)
	}
	dstFile = nil
	if err := srcFile.Close(); err != nil {
		srcFile = nil
		return fmt.Errorf("원본 파일 닫기: %w", err)
	}
	srcFile = nil
	currentDestInfo, err := dstRoot.Lstat(op.NewName)
	if err != nil {
		return fmt.Errorf("출력 파일 최종 확인: %w", err)
	}
	if currentDestInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(createdInfo, currentDestInfo) {
		return errors.New("출력 파일이 복사 중 다른 항목으로 변경되었습니다")
	}
	created = false
	return nil
}

func sourceMatchesSnapshot(op Operation, current fs.FileInfo) bool {
	return op.SourceInfo != nil && current != nil &&
		os.SameFile(op.SourceInfo, current) &&
		op.SourceSize == current.Size() &&
		op.SourceModTime.Equal(current.ModTime())
}

func removeCreatedFile(root *os.Root, name string, created fs.FileInfo) error {
	if created == nil {
		return errors.New("부분 출력 identity를 확인할 수 없어 제거하지 않았습니다")
	}
	current, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("부분 출력 확인: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !os.SameFile(created, current) {
		return errors.New("부분 출력 경로가 변경되어 제거하지 않았습니다")
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("부분 출력 제거: %w", err)
	}
	return nil
}

func closeWithContext(file *os.File, context string) error {
	if err := file.Close(); err != nil {
		return fmt.Errorf("%s: %w", context, err)
	}
	return nil
}
