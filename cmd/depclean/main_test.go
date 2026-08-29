package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

var errConfirmationIO = errors.New("injected confirmation I/O failure")

type failingConfirmationReader struct{}

func (failingConfirmationReader) Read([]byte) (int, error) {
	return 0, errConfirmationIO
}

type promptFailWriter struct{}

func (promptFailWriter) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("(y/N):")) {
		return 0, errConfirmationIO
	}
	return len(data), nil
}

func TestScanDependenciesUsesStrictAllowlist(t *testing.T) {
	root := t.TempDir()

	writeTestFile(t, filepath.Join(root, "python", "requirements.txt"), "requests")
	writeTestFile(t, filepath.Join(root, "python", ".env", "pyvenv.cfg"), "home = /tmp")
	writeTestFile(t, filepath.Join(root, "python", "env", "pyvenv.cfg"), "home = /tmp")
	writeTestFile(t, filepath.Join(root, "go", "go.mod"), "module example")
	writeTestFile(t, filepath.Join(root, "go", "vendor", "keep.go"), "package keep")
	writeTestFile(t, filepath.Join(root, "gradle", "build.gradle"), "plugins {}")
	writeTestFile(t, filepath.Join(root, "gradle", "build", "artifact.bin"), "generated")
	writeTestFile(t, filepath.Join(root, "ruby", "Gemfile"), "source 'https://example.invalid'")
	writeTestFile(t, filepath.Join(root, "ruby", ".bundle", "config"), "user config")
	writeTestFile(t, filepath.Join(root, "dotnet", "app.csproj"), "<Project />")
	writeTestFile(t, filepath.Join(root, "dotnet", "packages", "user.bin"), "user data")
	writeTestFile(t, filepath.Join(root, "ios", "Podfile"), "platform :ios")
	writeTestFile(t, filepath.Join(root, "ios", "DerivedData", "user.bin"), "user data")
	for _, barrier := range []string{".env", "env", "vendor", "build", ".bundle", "packages", "DerivedData", "bin"} {
		nested := filepath.Join(root, "barriers", barrier, "nested")
		writeTestFile(t, filepath.Join(nested, "package.json"), "{}")
		writeTestFile(t, filepath.Join(nested, "node_modules", "generated.bin"), "generated")
	}

	found, err := scanDependencies(root, 5, 0, 0)
	if err != nil {
		t.Fatalf("scanDependencies() error = %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("ambiguous directories selected: %+v", found)
	}
}

func TestScanDependenciesFindsValidatedArtifacts(t *testing.T) {
	root := t.TempDir()
	artifacts := []struct {
		project string
		marker  string
		folder  string
		kind    string
	}{
		{"node", "package.json", "node_modules", typeNode},
		{"python-project", "", ".venv", typePythonVenv},
		{"cache", "", "__pycache__", typePyCache},
		{"gradle", "settings.gradle.kts", ".gradle", typeGradle},
		{"rust", "Cargo.toml", "target", typeRust},
		{"maven", "pom.xml", "target", typeMaven},
		{"pods", "Podfile", "Pods", typePods},
		{"dotnet", "app.csproj", "obj", typeDotNet},
	}

	for _, artifact := range artifacts {
		project := filepath.Join(root, artifact.project)
		if artifact.marker != "" {
			writeTestFile(t, filepath.Join(project, artifact.marker), "marker")
		}
		folder := filepath.Join(project, artifact.folder)
		writeTestFile(t, filepath.Join(folder, "generated.bin"), "generated")
		if artifact.kind == typePythonVenv {
			writeTestFile(t, filepath.Join(folder, "pyvenv.cfg"), "home = /tmp")
		}
		ageTree(t, folder, 72*time.Hour)
	}

	found, err := scanDependencies(root, 3, 1, 0)
	if err != nil {
		t.Fatalf("scanDependencies() error = %v", err)
	}
	if len(found) != len(artifacts) {
		t.Fatalf("got %d artifacts, want %d: %+v", len(found), len(artifacts), found)
	}
	gotKinds := make([]string, 0, len(found))
	wantKinds := make([]string, 0, len(artifacts))
	for _, dep := range found {
		gotKinds = append(gotKinds, dep.DepType)
	}
	for _, artifact := range artifacts {
		wantKinds = append(wantKinds, artifact.kind)
	}
	sort.Strings(gotKinds)
	sort.Strings(wantKinds)
	if strings.Join(gotKinds, ",") != strings.Join(wantKinds, ",") {
		t.Fatalf("types = %v, want %v", gotKinds, wantKinds)
	}
}

func TestScanAndInspectionDoNotFollowSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	writeTestFile(t, filepath.Join(root, "project", "package.json"), "{}")
	artifact := filepath.Join(root, "project", "node_modules")
	writeTestFile(t, filepath.Join(artifact, "inside.bin"), "abc")
	writeTestFile(t, filepath.Join(outside, "large.bin"), strings.Repeat("x", 4096))
	if err := os.Symlink(filepath.Join(outside, "large.bin"), filepath.Join(artifact, "outside-link")); err != nil {
		t.Fatal(err)
	}
	ageTree(t, artifact, 72*time.Hour)

	size, latest, err := inspectArtifact(artifact)
	if err != nil {
		t.Fatalf("inspectArtifact() error = %v", err)
	}
	if size != 3 {
		t.Fatalf("size = %d, want 3; symlink target must not count", size)
	}
	if latest.After(time.Now().Add(-48 * time.Hour)) {
		t.Fatalf("latest = %v; symlink metadata/target must not affect age", latest)
	}

	linkedCandidate := filepath.Join(root, "linked", "node_modules")
	writeTestFile(t, filepath.Join(root, "linked", "package.json"), "{}")
	if err := os.Symlink(filepath.Join(outside), linkedCandidate); err != nil {
		t.Fatal(err)
	}
	found, err := scanDependencies(root, 3, 1, 0)
	if err != nil {
		t.Fatalf("scanDependencies() error = %v", err)
	}
	for _, dep := range found {
		if dep.DepPath == linkedCandidate {
			t.Fatalf("symlink candidate selected: %+v", dep)
		}
	}
}

func TestRunDefaultsToAnalysisOnly(t *testing.T) {
	root, artifact := makeOldNodeArtifact(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--path", root, "--days", "1"}, strings.NewReader("y\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Lstat(artifact); err != nil {
		t.Fatalf("default run deleted artifact: %v", err)
	}
	if !strings.Contains(stdout.String(), "분석 모드") || !strings.Contains(stdout.String(), "--apply") {
		t.Fatalf("analysis guidance missing: %s", stdout.String())
	}
}

func TestRunApplyRequiresConfirmation(t *testing.T) {
	root, artifact := makeOldNodeArtifact(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--path", root, "--days", "1", "--apply"}, strings.NewReader("n\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("declined run() = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Lstat(artifact); err != nil {
		t.Fatalf("declined confirmation deleted artifact: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--path", root, "--days", "1", "--apply"}, strings.NewReader("y\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("confirmed run() = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Lstat(artifact); !os.IsNotExist(err) {
		t.Fatalf("confirmed artifact still exists or wrong error: %v", err)
	}
}

func TestRunApplyEOFDoesNotDelete(t *testing.T) {
	root, artifact := makeOldNodeArtifact(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--path", root, "--days", "1", "--apply"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Lstat(artifact); err != nil {
		t.Fatalf("EOF confirmation deleted artifact: %v", err)
	}
}

func TestRunApplyAcceptsYes(t *testing.T) {
	root, artifact := makeOldNodeArtifact(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--path", root, "--days", "1", "--apply"}, strings.NewReader("YES\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Lstat(artifact); !os.IsNotExist(err) {
		t.Fatalf("YES confirmation did not delete artifact: %v", err)
	}
}

func TestRunApplyConfirmationReadErrorDoesNotDelete(t *testing.T) {
	root, artifact := makeOldNodeArtifact(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--path", root, "--days", "1", "--apply"}, failingConfirmationReader{}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "삭제 확인 입력/출력 실패") || !strings.Contains(stderr.String(), errConfirmationIO.Error()) {
		t.Fatalf("confirmation read error context missing: %s", stderr.String())
	}
	if _, err := os.Lstat(artifact); err != nil {
		t.Fatalf("confirmation read error deleted artifact: %v", err)
	}
}

func TestRunApplyConfirmationWriteErrorDoesNotDelete(t *testing.T) {
	root, artifact := makeOldNodeArtifact(t)
	var stderr bytes.Buffer
	code := run([]string{"--path", root, "--days", "1", "--apply"}, strings.NewReader("y\n"), promptFailWriter{}, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "삭제 확인 입력/출력 실패") || !strings.Contains(stderr.String(), errConfirmationIO.Error()) {
		t.Fatalf("confirmation write error context missing: %s", stderr.String())
	}
	if _, err := os.Lstat(artifact); err != nil {
		t.Fatalf("confirmation write error deleted artifact: %v", err)
	}
}

func TestValidateRemovalRejectsEscapeSymlinkAndChangedMarker(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	writeTestFile(t, filepath.Join(root, "keep"), "keep")
	writeTestFile(t, filepath.Join(outside, "package.json"), "{}")
	outsideArtifact := filepath.Join(outside, "node_modules")
	writeTestFile(t, filepath.Join(outsideArtifact, "generated.bin"), "generated")

	outsideDep := FoundDependency{DepPath: outsideArtifact, DepType: typeNode}
	if err := validateRemoval(root, outsideDep); err == nil {
		t.Fatal("validateRemoval accepted root escape")
	}

	linked := filepath.Join(root, "node_modules")
	if err := os.Symlink(outsideArtifact, linked); err != nil {
		t.Fatal(err)
	}
	linkedDep := FoundDependency{DepPath: linked, DepType: typeNode}
	if err := validateRemoval(root, linkedDep); err == nil {
		t.Fatal("validateRemoval accepted symlink target")
	}

	project := filepath.Join(root, "project")
	marker := filepath.Join(project, "package.json")
	writeTestFile(t, marker, "{}")
	artifact := filepath.Join(project, "node_modules")
	writeTestFile(t, filepath.Join(artifact, "generated.bin"), "generated")
	ageTree(t, artifact, 72*time.Hour)
	dep := scanSingleDependency(t, root)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := validateRemoval(root, dep); err == nil {
		t.Fatal("validateRemoval accepted artifact after marker removal")
	}
}

func TestValidateRemovalRejectsReplacedValidArtifact(t *testing.T) {
	root, artifact := makeOldNodeArtifact(t)
	dep := scanSingleDependency(t, root)

	// Keep old inode allocated so replacement must have a different identity.
	if err := os.Rename(artifact, artifact+".old"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(artifact, "generated.bin"), "generated")
	ageTree(t, artifact, 72*time.Hour)

	if err := validateRemoval(root, dep); err == nil {
		t.Fatal("validateRemoval accepted a different valid directory at the same path")
	}
}

func TestValidateRemovalRejectsFreshFileAfterScan(t *testing.T) {
	root, artifact := makeOldNodeArtifact(t)
	dep := scanSingleDependency(t, root)
	writeTestFile(t, filepath.Join(artifact, "fresh.bin"), "new activity")

	if err := validateRemoval(root, dep); err == nil {
		t.Fatal("validateRemoval accepted artifact modified after analysis")
	}
	if _, err := os.Lstat(filepath.Join(artifact, "fresh.bin")); err != nil {
		t.Fatalf("validation removed or changed fresh file: %v", err)
	}
}

func TestRemoveArtifactNoFollowRejectsIntermediateSymlinkSwap(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	project := filepath.Join(root, "project")
	writeTestFile(t, filepath.Join(project, "package.json"), "{}")
	artifact := filepath.Join(project, "node_modules")
	writeTestFile(t, filepath.Join(artifact, "generated.bin"), "generated")
	ageTree(t, artifact, 72*time.Hour)
	canonicalRoot := canonicalTestRoot(t, root)
	dep := scanSingleDependency(t, canonicalRoot)
	if err := validateRemoval(canonicalRoot, dep); err != nil {
		t.Fatalf("precondition validation failed: %v", err)
	}

	outside := filepath.Join(base, "outside")
	outsideArtifact := filepath.Join(outside, "node_modules")
	valuable := filepath.Join(outsideArtifact, "valuable.txt")
	writeTestFile(t, valuable, "must survive")
	if err := os.Rename(project, project+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, project); err != nil {
		t.Fatal(err)
	}

	if err := removeArtifactNoFollow(canonicalRoot, dep); err == nil {
		t.Fatal("secure removal followed swapped intermediate symlink")
	}
	if contents, err := os.ReadFile(valuable); err != nil || string(contents) != "must survive" {
		t.Fatalf("outside file changed: contents=%q err=%v", contents, err)
	}
	if _, err := os.Lstat(filepath.Join(project+"-old", "node_modules")); err != nil {
		t.Fatalf("original approved artifact unexpectedly changed: %v", err)
	}
}

func TestRemoveArtifactNoFollowDoesNotFollowNestedSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	project := filepath.Join(root, "project")
	writeTestFile(t, filepath.Join(project, "package.json"), "{}")
	artifact := filepath.Join(project, "node_modules")
	writeTestFile(t, filepath.Join(artifact, "generated.bin"), "generated")
	outside := filepath.Join(base, "outside")
	valuable := filepath.Join(outside, "valuable.txt")
	writeTestFile(t, valuable, "must survive")
	if err := os.Symlink(outside, filepath.Join(artifact, "outside-link")); err != nil {
		t.Fatal(err)
	}
	ageTree(t, artifact, 72*time.Hour)
	canonicalRoot := canonicalTestRoot(t, root)
	dep := scanSingleDependency(t, canonicalRoot)
	if err := validateRemoval(canonicalRoot, dep); err != nil {
		t.Fatalf("precondition validation failed: %v", err)
	}
	if err := removeArtifactNoFollow(canonicalRoot, dep); err != nil {
		t.Fatalf("secure removal failed: %v", err)
	}
	if _, err := os.Lstat(artifact); !os.IsNotExist(err) {
		t.Fatalf("artifact remains or wrong error: %v", err)
	}
	if contents, err := os.ReadFile(valuable); err != nil || string(contents) != "must survive" {
		t.Fatalf("symlink target changed: contents=%q err=%v", contents, err)
	}
}

func TestCanonicalScanRootRejectsDangerousRoots(t *testing.T) {
	if _, err := canonicalScanRoot(string(filepath.Separator)); err == nil {
		t.Fatal("filesystem root accepted")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalScanRoot(home); err == nil {
		t.Fatal("home root accepted")
	}

	realRoot := t.TempDir()
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalScanRoot(link); err == nil {
		t.Fatal("symlink root accepted")
	}
}

func TestRunRejectsInvalidFlagsAndRoots(t *testing.T) {
	root := t.TempDir()
	tests := [][]string{
		{"--path", root, "--days", "-1"},
		{"--path", root, "--depth", "-1"},
		{"--path", root, "--min-size", "-1GB"},
		{"--path", root, "--min-size", "garbage"},
		{"--path", root, "--apply", "--dry-run"},
		{"--path", ""},
		{"--path", string(filepath.Separator)},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := run(args, strings.NewReader("y\n"), &stdout, &stderr); code != 2 {
			t.Errorf("run(%v) = %d, want 2; stderr = %s", args, code, stderr.String())
		}
	}
}

func TestParseOptionsUsesCanonicalMinSizeSemantics(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"1.5KB", 1536},
		{".5KiB", 512},
		{"1.9B", 1},
		{"", 0},
	}
	for _, test := range tests {
		var stderr bytes.Buffer
		opts, code := parseOptions([]string{"--min-size", test.input}, &stderr)
		if code != 0 {
			t.Fatalf("parseOptions(--min-size %q) = %d, stderr = %s", test.input, code, stderr.String())
		}
		if opts.minSize != test.want {
			t.Errorf("--min-size %q = %d, want %d", test.input, opts.minSize, test.want)
		}
	}

	var stderr bytes.Buffer
	if _, code := parseOptions([]string{"--min-size", "garbage"}, &stderr); code != 2 {
		t.Fatalf("invalid --min-size code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "오류: --min-size:") || !strings.Contains(stderr.String(), "invalid size") {
		t.Fatalf("min-size error context missing: %s", stderr.String())
	}
}

func TestDryRunAliasNeverDeletes(t *testing.T) {
	root, artifact := makeOldNodeArtifact(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--path", root, "--days", "1", "--dry-run"}, strings.NewReader("y\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Lstat(artifact); err != nil {
		t.Fatalf("--dry-run deleted artifact: %v", err)
	}
}

func TestScanDependenciesSurfacesWalkError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	found, err := scanDependencies(missing, 3, 0, 0)
	if err == nil {
		t.Fatal("scanDependencies swallowed root walk error")
	}
	if len(found) != 0 {
		t.Fatalf("found = %+v, want none", found)
	}
}

func makeOldNodeArtifact(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	writeTestFile(t, filepath.Join(project, "package.json"), "{}")
	artifact := filepath.Join(project, "node_modules")
	writeTestFile(t, filepath.Join(artifact, "generated.bin"), "generated")
	ageTree(t, artifact, 72*time.Hour)
	return root, artifact
}

func scanSingleDependency(t *testing.T, root string) FoundDependency {
	t.Helper()
	found, err := scanDependencies(root, 3, 1, 0)
	if err != nil {
		t.Fatalf("scanDependencies() error = %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("scanDependencies() found %d, want 1: %+v", len(found), found)
	}
	return found[0]
}

func canonicalTestRoot(t *testing.T, root string) string {
	t.Helper()
	canonical, err := canonicalScanRoot(root)
	if err != nil {
		t.Fatalf("canonicalScanRoot() error = %v", err)
	}
	return canonical
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ageTree(t *testing.T, root string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return os.Chtimes(path, old, old)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Updating child timestamps can modify parent directory metadata on some
	// filesystems. Set root last so age tests stay deterministic.
	if err := os.Chtimes(root, old, old); err != nil {
		t.Fatal(err)
	}
}
