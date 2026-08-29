package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	projectfs "github.com/useful-go/pkg/fs"
	projecttext "github.com/useful-go/pkg/text"
	projectui "github.com/useful-go/pkg/ui"
)

const (
	typeNode       = "Node.js"
	typePythonVenv = "Python venv"
	typePyCache    = "Python cache"
	typeGradle     = "Gradle"
	typeMaven      = "Maven"
	typeRust       = "Rust"
	typePods       = "CocoaPods"
	typeDotNet     = ".NET"
)

// FoundDependency is an allowlisted, marker-validated generated artifact.
type FoundDependency struct {
	ProjectPath string
	DepPath     string
	DepType     string
	Size        int64
	LastAccess  time.Time
	DaysSince   int
	identity    os.FileInfo
}

type artifactSnapshot struct {
	size     int64
	latest   time.Time
	identity os.FileInfo
}

type options struct {
	apply    bool
	dryRun   bool
	days     int
	path     string
	depth    int
	minSize  int64
	minSizeS string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, code := parseOptions(args, stderr)
	if code == -1 {
		return 0
	}
	if code != 0 {
		return code
	}

	root, err := canonicalScanRoot(opts.path)
	if err != nil {
		fmt.Fprintf(stderr, "오류: 검색 경로 거부: %v\n", err)
		return 2
	}

	fmt.Fprintln(stdout, "depclean - 오래된 프로젝트 의존성 분석")
	fmt.Fprintf(stdout, "검색 경로: %s\n", root)
	fmt.Fprintf(stdout, "기준: %d일 이상 미수정\n", opts.days)
	if opts.minSize > 0 {
		fmt.Fprintf(stdout, "최소 크기: %s\n", projectfs.FormatSize(opts.minSize))
	}
	if !opts.apply {
		fmt.Fprintln(stdout, "분석 모드 (기본값, 파일을 삭제하지 않음)")
	}
	fmt.Fprintln(stdout)

	found, scanErr := scanDependencies(root, opts.depth, opts.days, opts.minSize)
	if len(found) == 0 {
		fmt.Fprintf(stdout, "%d일 이상 미수정된 안전한 정리 후보가 없습니다.\n", opts.days)
		if scanErr != nil {
			fmt.Fprintf(stderr, "경고: 검색이 일부 완료되지 않음: %v\n", scanErr)
			return 1
		}
		return 0
	}

	printDependencies(stdout, found)
	if scanErr != nil {
		fmt.Fprintf(stderr, "경고: 검색이 일부 완료되지 않음: %v\n", scanErr)
		if opts.apply {
			fmt.Fprintln(stderr, "오류: 불완전한 검색 결과에는 삭제를 적용하지 않음")
			return 1
		}
	}

	if !opts.apply {
		fmt.Fprintln(stdout, "실제 정리: 같은 명령에 --apply 추가 필요 (--dry-run은 분석 별칭)")
		if scanErr != nil {
			return 1
		}
		return 0
	}

	confirmed, err := projectui.ConfirmYesNoE("위 항목들을 삭제하시겠습니까?", stdin, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "오류: 삭제 확인 입력/출력 실패: %v\n", err)
		return 1
	}
	if !confirmed {
		if _, err := fmt.Fprintln(stdout, "취소되었습니다."); err != nil {
			fmt.Fprintf(stderr, "오류: 삭제 취소 출력 실패: %v\n", err)
			return 1
		}
		return 0
	}

	deletedCount, deletedSize := 0, int64(0)
	failed := false
	for _, dep := range found {
		if err := validateRemoval(root, dep); err != nil {
			fmt.Fprintf(stderr, "삭제 거부: %s - %v\n", dep.DepPath, err)
			failed = true
			continue
		}
		if err := removeArtifactNoFollow(root, dep); err != nil {
			fmt.Fprintf(stderr, "삭제 실패: %s - %v\n", dep.DepPath, err)
			failed = true
			continue
		}
		deletedCount++
		deletedSize += dep.Size
		fmt.Fprintf(stdout, "삭제: %s (%s)\n", dep.DepPath, projectfs.FormatSize(dep.Size))
	}

	fmt.Fprintf(stdout, "완료: %d개 삭제, %s 확보\n", deletedCount, projectfs.FormatSize(deletedSize))
	if failed {
		return 1
	}
	return 0
}

func parseOptions(args []string, stderr io.Writer) (options, int) {
	var opts options
	flags := flag.NewFlagSet("depclean", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.BoolVar(&opts.apply, "apply", false, "후보를 확인한 뒤 실제 삭제")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "분석만 수행 (기본 동작의 호환 별칭)")
	flags.IntVar(&opts.days, "days", 30, "마지막 수정 이후 경과 일수")
	flags.StringVar(&opts.path, "path", ".", "검색할 디렉터리")
	flags.IntVar(&opts.depth, "depth", 5, "검색 깊이 제한")
	flags.StringVar(&opts.minSizeS, "min-size", "0", "최소 크기 필터 (예: 100MB, 1GB)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return options{}, -1
		}
		return options{}, 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "오류: 예상하지 않은 인자: %s\n", strings.Join(flags.Args(), " "))
		return options{}, 2
	}
	if opts.apply && opts.dryRun {
		fmt.Fprintln(stderr, "오류: --apply와 --dry-run을 함께 사용할 수 없음")
		return options{}, 2
	}
	if opts.days < 0 {
		fmt.Fprintln(stderr, "오류: --days는 0 이상이어야 함")
		return options{}, 2
	}
	if opts.depth < 0 {
		fmt.Fprintln(stderr, "오류: --depth는 0 이상이어야 함")
		return options{}, 2
	}
	if strings.TrimSpace(opts.path) == "" {
		fmt.Fprintln(stderr, "오류: --path는 비어 있을 수 없음")
		return options{}, 2
	}
	minSize, err := projecttext.ParseSizeE(opts.minSizeS)
	if err != nil {
		fmt.Fprintf(stderr, "오류: --min-size: %v\n", err)
		return options{}, 2
	}
	opts.minSize = minSize
	return opts, 0
}

func canonicalScanRoot(raw string) (string, error) {
	expanded, err := expandPath(raw)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("symlink 검색 루트는 허용되지 않음: %s", abs)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("디렉터리가 아님: %s", abs)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	resolved = filepath.Clean(resolved)
	if resolved == string(filepath.Separator) {
		return "", errors.New("파일시스템 루트는 검색할 수 없음")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("홈 디렉터리 확인 실패: %w", err)
	}
	homeResolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", fmt.Errorf("홈 디렉터리 확인 실패: %w", err)
	}
	homeInfo, err := os.Stat(homeResolved)
	if err != nil {
		return "", fmt.Errorf("홈 디렉터리 확인 실패: %w", err)
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if os.SameFile(resolvedInfo, homeInfo) {
		return "", errors.New("홈 디렉터리 자체는 검색할 수 없음")
	}
	return resolved, nil
}

func expandPath(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"+string(filepath.Separator))), nil
}

func scanDependencies(root string, maxDepth, days int, minSize int64) ([]FoundDependency, error) {
	var found []FoundDependency
	var scanErrors error
	now := time.Now()
	cutoff := now.AddDate(0, 0, -days)

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			scanErrors = errors.Join(scanErrors, walkErr)
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			scanErrors = errors.Join(scanErrors, fmt.Errorf("%s: %w", path, relErr))
			return filepath.SkipDir
		}
		depth := pathDepth(rel)
		if depth > maxDepth {
			return filepath.SkipDir
		}

		depType, matched, matchErr := identifyArtifact(path)
		if matchErr != nil {
			scanErrors = errors.Join(scanErrors, fmt.Errorf("%s: %w", path, matchErr))
		}
		if matched {
			snapshot, inspectErr := captureArtifactSnapshot(path)
			if inspectErr != nil {
				scanErrors = errors.Join(scanErrors, fmt.Errorf("%s: %w", path, inspectErr))
				return filepath.SkipDir
			}
			if !snapshot.latest.After(cutoff) && snapshot.size >= minSize {
				daysSince := int(now.Sub(snapshot.latest).Hours() / 24)
				if daysSince < 0 {
					daysSince = 0
				}
				found = append(found, FoundDependency{
					ProjectPath: filepath.Dir(path),
					DepPath:     path,
					DepType:     depType,
					Size:        snapshot.size,
					LastAccess:  snapshot.latest,
					DaysSince:   daysSince,
					identity:    snapshot.identity,
				})
			}
			return filepath.SkipDir
		}

		name := entry.Name()
		if shouldSkipSubtree(name) || strings.HasPrefix(name, ".") {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		scanErrors = errors.Join(scanErrors, err)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].DepPath < found[j].DepPath })
	return found, scanErrors
}

func shouldSkipSubtree(name string) bool {
	switch name {
	case ".git", ".svn", ".hg",
		".env", "env", "vendor", "build", ".bundle", "packages", "DerivedData", "bin",
		"node_modules", "venv", ".venv", "__pycache__", ".gradle", "target", "Pods", "obj":
		return true
	default:
		return false
	}
}

func pathDepth(rel string) int {
	if rel == "." || rel == "" {
		return 0
	}
	return strings.Count(filepath.Clean(rel), string(filepath.Separator)) + 1
}

func identifyArtifact(path string) (string, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", false, nil
	}

	name := filepath.Base(path)
	projectPath := filepath.Dir(path)
	switch name {
	case "node_modules":
		ok, err := regularFile(filepath.Join(projectPath, "package.json"))
		return typeNode, ok, err
	case "venv", ".venv":
		ok, err := regularFile(filepath.Join(path, "pyvenv.cfg"))
		return typePythonVenv, ok, err
	case "__pycache__":
		return typePyCache, true, nil
	case ".gradle":
		ok, err := hasAnyRegularFile(projectPath, []string{
			"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts",
		})
		return typeGradle, ok, err
	case "target":
		cargo, cargoErr := regularFile(filepath.Join(projectPath, "Cargo.toml"))
		if cargo {
			return typeRust, true, cargoErr
		}
		pom, pomErr := regularFile(filepath.Join(projectPath, "pom.xml"))
		if pom {
			return typeMaven, true, errors.Join(cargoErr, pomErr)
		}
		return "", false, errors.Join(cargoErr, pomErr)
	case "Pods":
		ok, err := regularFile(filepath.Join(projectPath, "Podfile"))
		return typePods, ok, err
	case "obj":
		ok, err := hasRegularCSProj(projectPath)
		return typeDotNet, ok, err
	default:
		// Explicit allowlist: .env, env, vendor, build, bin, packages,
		// .bundle, DerivedData, and every other generic name are excluded.
		return "", false, nil
	}
}

func regularFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

func hasAnyRegularFile(dir string, names []string) (bool, error) {
	var joined error
	found := false
	for _, name := range names {
		ok, err := regularFile(filepath.Join(dir, name))
		joined = errors.Join(joined, err)
		found = found || ok
	}
	return found, joined
}

func hasRegularCSProj(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	var joined error
	for _, entry := range entries {
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".csproj") {
			continue
		}
		ok, statErr := regularFile(filepath.Join(dir, entry.Name()))
		joined = errors.Join(joined, statErr)
		if ok {
			return true, joined
		}
	}
	return false, joined
}

func inspectArtifact(path string) (int64, time.Time, error) {
	snapshot, err := captureArtifactSnapshot(path)
	return snapshot.size, snapshot.latest, err
}

func captureArtifactSnapshot(path string) (artifactSnapshot, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return artifactSnapshot{}, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return artifactSnapshot{}, errors.New("artifact가 symlink이거나 디렉터리가 아님")
	}

	var size int64
	var latest time.Time
	err = filepath.WalkDir(path, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		if info.Mode().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	if err != nil {
		return artifactSnapshot{}, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return artifactSnapshot{}, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.IsDir() || !os.SameFile(before, after) {
		return artifactSnapshot{}, errors.New("검사 중 artifact identity가 변경됨")
	}
	return artifactSnapshot{size: size, latest: latest, identity: after}, nil
}

func validateRemoval(root string, dep FoundDependency) error {
	target, err := filepath.Abs(dep.DepPath)
	if err != nil {
		return err
	}
	target = filepath.Clean(target)
	if !strictlyBelow(root, target) {
		return errors.New("검색 루트 밖이거나 루트 자체인 경로")
	}

	info, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("삭제 직전 Lstat 실패: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("symlink 대상")
	}
	if !info.IsDir() {
		return errors.New("디렉터리가 아님")
	}
	if dep.identity == nil {
		return errors.New("분석 시점 artifact identity가 없음")
	}
	if !os.SameFile(dep.identity, info) {
		return errors.New("분석 후 artifact 디렉터리가 교체됨")
	}

	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("삭제 직전 경로 확인 실패: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return err
	}
	resolved = filepath.Clean(resolved)
	if resolved != target {
		return errors.New("경로 구성요소에 symlink가 생김")
	}
	if !strictlyBelow(root, resolved) {
		return errors.New("해석된 대상이 검색 루트 밖이거나 루트 자체임")
	}

	currentType, ok, err := identifyArtifact(target)
	if err != nil {
		return fmt.Errorf("artifact 재검증 실패: %w", err)
	}
	if !ok || currentType != dep.DepType {
		return errors.New("artifact marker 재검증 실패")
	}

	current, err := captureArtifactSnapshot(target)
	if err != nil {
		return fmt.Errorf("artifact 삭제 직전 재검사 실패: %w", err)
	}
	if !os.SameFile(dep.identity, current.identity) {
		return errors.New("삭제 직전 artifact 디렉터리가 교체됨")
	}
	if current.size != dep.Size || !current.latest.Equal(dep.LastAccess) {
		return fmt.Errorf(
			"분석 후 artifact 내용이 변경됨 (크기 %d -> %d, 최신 수정 %s -> %s)",
			dep.Size, current.size,
			dep.LastAccess.Format(time.RFC3339Nano), current.latest.Format(time.RFC3339Nano),
		)
	}
	return nil
}

func strictlyBelow(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || rel == "" || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func printDependencies(out io.Writer, found []FoundDependency) {
	var totalSize int64
	fmt.Fprintln(out, "발견된 오래된 의존성:")
	for _, dep := range found {
		fmt.Fprintf(out, "- %s | %s | %s | %d일\n", dep.DepPath, dep.DepType, projectfs.FormatSize(dep.Size), dep.DaysSince)
		totalSize += dep.Size
	}
	fmt.Fprintf(out, "총 %d개, %s\n\n", len(found), projectfs.FormatSize(totalSize))
}
