package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
}
type artifactCandidate struct{ Path, ProjectPath, Type string }
type artifactInspection struct {
	Artifact ProjectArtifact
	Error    error
}

func scanProjectArtifacts(root string, maxDepth, minDays, workers int) ([]ProjectArtifact, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("프로젝트 루트가 디렉터리가 아님: %s", absRoot)
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
		if candidate, ok := matchArtifact(path); ok {
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
	analyzed := parallelMap(candidates, workers, func(candidate artifactCandidate) artifactInspection {
		size, latest, inspectErr := inspectArtifact(candidate.Path)
		return artifactInspection{
			Artifact: ProjectArtifact{Path: candidate.Path, ProjectPath: candidate.ProjectPath, Type: candidate.Type, Size: size, LastModified: latest},
			Error:    inspectErr,
		}
	})
	cutoff := time.Now().AddDate(0, 0, -minDays)
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
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Size == filtered[j].Size {
			return filtered[i].Path < filtered[j].Path
		}
		return filtered[i].Size > filtered[j].Size
	})
	return filtered, walkErrors
}

func matchArtifact(path string) (artifactCandidate, bool) {
	name, projectPath := filepath.Base(path), filepath.Dir(path)
	for _, spec := range artifactSpecs {
		if !contains(spec.Names, name) {
			continue
		}
		if spec.SelfMarker != "" && pathExists(filepath.Join(path, spec.SelfMarker)) || hasIndicator(projectPath, spec.Indicators) {
			return artifactCandidate{Path: path, ProjectPath: projectPath, Type: spec.Type}, true
		}
	}
	return artifactCandidate{}, false
}

func hasIndicator(projectPath string, patterns []string) bool {
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(filepath.Join(projectPath, pattern))
		if len(matches) > 0 {
			return true
		}
	}
	return false
}
func pathExists(path string) bool { _, err := os.Stat(path); return err == nil }
func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func inspectArtifact(path string) (int64, time.Time, error) {
	var size int64
	var latest time.Time
	err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
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
	return size, latest, err
}
