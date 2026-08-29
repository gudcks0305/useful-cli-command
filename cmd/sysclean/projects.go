package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/useful-go/internal/rootfs"
)

type artifactSpec struct {
	Type              string
	Names, Indicators []string
	SelfMarker        string
}

var artifactSpecs = []artifactSpec{
	{Type: "Node.js node_modules", Names: []string{"node_modules"}, Indicators: []string{"package.json"}},
	{Type: "Next.js build", Names: []string{".next"}, Indicators: []string{"next.config.*", "package.json"}},
	{Type: "Nuxt build", Names: []string{".nuxt"}, Indicators: []string{"nuxt.config.*", "package.json"}},
	{Type: "Turborepo cache", Names: []string{".turbo"}, Indicators: []string{"turbo.json", "package.json"}},
	{Type: "Python venv", Names: []string{".venv", "venv"}, Indicators: []string{"pyproject.toml", "requirements*.txt", "setup.py"}, SelfMarker: "pyvenv.cfg"},
	{Type: "Python bytecode", Names: []string{"__pycache__"}, Indicators: []string{"pyproject.toml", "requirements*.txt", "setup.py", "*.py"}},
	{Type: "Rust target", Names: []string{"target"}, Indicators: []string{"Cargo.toml"}},
	{Type: "Maven target", Names: []string{"target"}, Indicators: []string{"pom.xml"}},
	{Type: "SwiftPM build", Names: []string{".build"}, Indicators: []string{"Package.swift"}},
	{Type: "Gradle cache", Names: []string{".gradle"}, Indicators: []string{"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts"}},
	{Type: "Gradle build", Names: []string{"build"}, Indicators: []string{"build.gradle", "build.gradle.kts"}},
	{Type: "CocoaPods", Names: []string{"Pods"}, Indicators: []string{"Podfile"}},
	{Type: ".NET build", Names: []string{"bin", "obj"}, Indicators: []string{"*.csproj", "*.fsproj", "*.sln"}},
}

type ProjectArtifact struct {
	Path, ProjectPath, Type string
	Size                    int64
	LastModified            time.Time
	snapshot                *artifactSnapshot
}
type artifactCandidate struct {
	Path, RelPath, ProjectPath string
	Spec                       artifactSpec
}
type artifactInspection struct {
	Artifact ProjectArtifact
	Error    error
}

type artifactSnapshot struct {
	scanRoot     *artifactScanRoot
	parentRel    string
	path         string
	name         string
	typeName     string
	info         os.FileInfo
	markerName   string
	markerInfo   os.FileInfo
	lastModified time.Time
	cutoff       time.Time
}

type artifactScanRoot struct {
	root *os.Root
	once sync.Once
}

func (scan *artifactScanRoot) close() {
	if scan != nil {
		scan.once.Do(func() { scan.root.Close() })
	}
}

func scanProjectArtifacts(root string, maxDepth, minDays, workers int) ([]ProjectArtifact, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(absRoot)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("프로젝트 루트 symlink 거부: %s", absRoot)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("프로젝트 루트가 디렉터리가 아님: %s", absRoot)
	}
	secureRoot, err := os.OpenRoot(absRoot)
	if err != nil {
		return nil, err
	}
	scanRoot := &artifactScanRoot{root: secureRoot}
	keepRoot := false
	defer func() {
		if !keepRoot {
			scanRoot.close()
		}
	}()
	openedInfo, err := secureRoot.Lstat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("프로젝트 루트가 탐색 중 변경됨: %s", absRoot)
	}
	var candidates []artifactCandidate
	var walkErrors error
	err = filepath.WalkDir(absRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			walkErrors = errors.Join(walkErrors, walkErr)
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() || path == absRoot {
			return nil
		}
		rel, err := filepath.Rel(absRoot, path)
		if err != nil {
			return nil
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		if depth > maxDepth {
			return filepath.SkipDir
		}
		candidate, matched, matchErr := matchArtifact(path)
		if matchErr != nil {
			walkErrors = errors.Join(walkErrors, fmt.Errorf("%s: %w", path, matchErr))
			return filepath.SkipDir
		}
		if matched {
			candidate.RelPath = rel
			candidates = append(candidates, candidate)
			return filepath.SkipDir
		}
		name := entry.Name()
		if name == ".git" || name == ".svn" || name == ".hg" || strings.HasPrefix(name, ".") {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		walkErrors = errors.Join(walkErrors, err)
	}
	cutoff := time.Now().AddDate(0, 0, -minDays)
	analyzed := parallelMap(candidates, workers, func(candidate artifactCandidate) artifactInspection {
		return inspectArtifactCandidate(scanRoot, candidate, cutoff)
	})
	filtered := make([]ProjectArtifact, 0, len(analyzed))
	for _, inspection := range analyzed {
		if inspection.Error != nil {
			walkErrors = errors.Join(walkErrors, fmt.Errorf("%s: %w", inspection.Artifact.Path, inspection.Error))
			continue
		}
		artifact := inspection.Artifact
		if artifact.Size > 0 && !artifact.LastModified.After(cutoff) {
			filtered = append(filtered, artifact)
		}
	}
	keepRoot = len(filtered) > 0
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Size == filtered[j].Size {
			return filtered[i].Path < filtered[j].Path
		}
		return filtered[i].Size > filtered[j].Size
	})
	if walkErrors != nil {
		keepRoot = false
		return nil, walkErrors
	}
	return filtered, walkErrors
}

func matchArtifact(path string) (artifactCandidate, bool, error) {
	name, projectPath := filepath.Base(path), filepath.Dir(path)
	for _, spec := range artifactSpecs {
		if !contains(spec.Names, name) {
			continue
		}
		if spec.SelfMarker != "" {
			if isRegularFile(filepath.Join(path, spec.SelfMarker)) {
				return artifactCandidate{Path: path, ProjectPath: projectPath, Spec: spec}, true, nil
			}
			continue
		}
		matched, err := hasIndicator(projectPath, spec.Indicators)
		if err != nil {
			return artifactCandidate{}, false, err
		}
		if matched {
			return artifactCandidate{Path: path, ProjectPath: projectPath, Spec: spec}, true, nil
		}
	}
	return artifactCandidate{}, false, nil
}

func hasIndicator(projectPath string, patterns []string) (bool, error) {
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(projectPath, pattern))
		if err != nil {
			return false, err
		}
		if len(matches) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func isRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func inspectArtifact(path string) (int64, time.Time, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return 0, time.Time{}, fmt.Errorf("artifact 디렉터리 아님")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	openedInfo, err := root.Lstat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		return 0, time.Time{}, errors.Join(fmt.Errorf("artifact가 검사 중 변경됨"), root.Close())
	}
	metadata, inspectErr := rootfs.Inspect(root)
	if err := errors.Join(inspectErr, root.Close()); err != nil {
		return 0, time.Time{}, err
	}
	return metadata.Size, metadata.Latest, nil
}

func inspectArtifactCandidate(scanRoot *artifactScanRoot, candidate artifactCandidate, cutoff time.Time) (result artifactInspection) {
	parentRel := filepath.Dir(candidate.RelPath)
	parent, err := scanRoot.root.OpenRoot(parentRel)
	if err != nil {
		return artifactInspection{Artifact: ProjectArtifact{Path: candidate.Path}, Error: err}
	}
	defer func() {
		if err := parent.Close(); err != nil {
			result.Error = errors.Join(result.Error, fmt.Errorf("artifact parent handle 닫기 실패: %w", err))
			if result.Artifact.Path == "" {
				result.Artifact.Path = candidate.Path
			}
		}
	}()

	name := filepath.Base(candidate.RelPath)
	info, err := parent.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("artifact symlink 또는 비-directory 거부")
		}
		return artifactInspection{Artifact: ProjectArtifact{Path: candidate.Path}, Error: err}
	}
	markerName, markerInfo, err := findArtifactMarker(parent, name, candidate.Spec)
	if err != nil {
		return artifactInspection{Artifact: ProjectArtifact{Path: candidate.Path}, Error: err}
	}
	artifactRoot, err := parent.OpenRoot(name)
	if err != nil {
		return artifactInspection{Artifact: ProjectArtifact{Path: candidate.Path}, Error: err}
	}
	openedInfo, statErr := artifactRoot.Lstat(".")
	if statErr != nil || !os.SameFile(info, openedInfo) {
		return artifactInspection{
			Artifact: ProjectArtifact{Path: candidate.Path},
			Error:    errors.Join(fmt.Errorf("artifact가 검사 중 변경됨"), artifactRoot.Close()),
		}
	}
	metadata, inspectErr := rootfs.Inspect(artifactRoot)
	if err := errors.Join(inspectErr, artifactRoot.Close()); err != nil {
		return artifactInspection{Artifact: ProjectArtifact{Path: candidate.Path}, Error: err}
	}

	snapshot := &artifactSnapshot{
		scanRoot:     scanRoot,
		parentRel:    parentRel,
		path:         candidate.Path,
		name:         name,
		typeName:     candidate.Spec.Type,
		info:         info,
		markerName:   markerName,
		markerInfo:   markerInfo,
		lastModified: metadata.Latest,
		cutoff:       cutoff,
	}
	return artifactInspection{Artifact: ProjectArtifact{
		Path:         candidate.Path,
		ProjectPath:  candidate.ProjectPath,
		Type:         candidate.Spec.Type,
		Size:         metadata.Size,
		LastModified: metadata.Latest,
		snapshot:     snapshot,
	}}
}

func findArtifactMarker(parent *os.Root, artifactName string, spec artifactSpec) (string, os.FileInfo, error) {
	if spec.SelfMarker != "" {
		name := filepath.Join(artifactName, spec.SelfMarker)
		info, err := parent.Lstat(name)
		if err != nil {
			return "", nil, fmt.Errorf("%s self marker 없음: %w", spec.Type, err)
		}
		if !info.Mode().IsRegular() {
			return "", nil, fmt.Errorf("%s self marker가 regular file 아님", spec.Type)
		}
		return name, info, nil
	}
	dir, err := parent.Open(".")
	if err != nil {
		return "", nil, err
	}
	entries, readErr := dir.ReadDir(-1)
	if err := errors.Join(readErr, dir.Close()); err != nil {
		return "", nil, err
	}
	for _, pattern := range spec.Indicators {
		for _, entry := range entries {
			matched, matchErr := filepath.Match(pattern, entry.Name())
			if matchErr != nil {
				return "", nil, matchErr
			}
			if !matched {
				continue
			}
			info, statErr := parent.Lstat(entry.Name())
			if statErr == nil && info.Mode().IsRegular() {
				return entry.Name(), info, nil
			}
		}
	}
	return "", nil, fmt.Errorf("%s marker 재검증 실패", spec.Type)
}
