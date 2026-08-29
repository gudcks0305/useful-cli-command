package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type readError struct{}

func (readError) Read([]byte) (int, error) { return 0, errors.New("forced read error") }

func TestConfirmDeletionPreservesReadErrorAndCancel(t *testing.T) {
	if _, err := confirmDeletion(readError{}, io.Discard); err == nil {
		t.Fatal("read error was discarded")
	}
	var output bytes.Buffer
	confirmed, err := confirmDeletion(strings.NewReader("n\n"), &output)
	if err != nil || confirmed {
		t.Fatalf("cancel = %v, %v", confirmed, err)
	}
}

func TestSyscleanConfirmationReadErrorExitsNonzeroWithoutDeletion(t *testing.T) {
	home := t.TempDir()
	cacheFile := filepath.Join(home, ".npm", "_cacache", "keep")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	input, err := os.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSyscleanConfirmationHelper$")
	cmd.Env = append(os.Environ(), "GO_WANT_SYSCLEAN_HELPER=1", "HOME="+home)
	cmd.Stdin = input
	output, err := cmd.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		t.Fatalf("exit = %v, output:\n%s", err, output)
	}
	if !strings.Contains(string(output), "확인 입력 실패") {
		t.Fatalf("missing input error:\n%s", output)
	}
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("cache file changed: %v", err)
	}
}

func TestSyscleanCancellationExitsZeroWithoutDeletion(t *testing.T) {
	home := t.TempDir()
	cacheFile := filepath.Join(home, ".npm", "_cacache", "keep")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSyscleanConfirmationHelper$")
	cmd.Env = append(os.Environ(), "GO_WANT_SYSCLEAN_HELPER=1", "HOME="+home)
	cmd.Stdin = strings.NewReader("n\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cancel exit = %v, output:\n%s", err, output)
	}
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("cache file changed: %v", err)
	}
}

func TestSyscleanConfirmationHelper(t *testing.T) {
	if os.Getenv("GO_WANT_SYSCLEAN_HELPER") != "1" {
		return
	}
	os.Args = []string{"sysclean", "--only=npm-cache", "--workers=1"}
	main()
}

func TestParseNumberSelection(t *testing.T) {
	got, err := parseNumberSelection("3,1-2,2", 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 1, 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if _, err := parseNumberSelection("2-5", 4); err == nil {
		t.Fatal("expected out-of-range error")
	}
}

func TestSelectConfiguredTargets(t *testing.T) {
	targets := []CleanTarget{
		{ID: "safe", Risk: riskSafe},
		{ID: "optional", Risk: riskOptional},
		{ID: "docker", Risk: riskExplicit, Kind: kindDocker},
	}
	got, err := selectConfiguredTargets(targets, options{})
	if err != nil || len(got) != 1 || got[0].ID != "safe" {
		t.Fatalf("default = %#v, %v", got, err)
	}
	got, err = selectConfiguredTargets(targets, options{only: "docker,safe,docker"})
	if err != nil || len(got) != 2 || got[0].ID != "docker" || got[1].ID != "safe" {
		t.Fatalf("only = %#v, %v", got, err)
	}
}

func TestRemoveContentsPreservesRootAndRemovesDotfiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".hidden"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := removeContents(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("remaining entries: %v", entries)
	}
}

func TestRemoveContentsRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := removeContents(link); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestMissingCachePathIsIdempotent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	size, err := getPathSize(missing)
	if err != nil || size != 0 {
		t.Fatalf("getPathSize(missing) = %d, %v", size, err)
	}
	if err := removeContents(missing); err != nil {
		t.Fatalf("removeContents(missing) = %v", err)
	}
}

func TestParseDockerSize(t *testing.T) {
	tests := map[string]int64{"0B": 0, "1.5kB": 1500, "2MB": 2_000_000, "1GiB": 1 << 30}
	for input, want := range tests {
		got, err := parseDockerSize(input)
		if err != nil || got != want {
			t.Fatalf("%s = %d, %v; want %d", input, got, err, want)
		}
	}
}

func TestParseDockerDFExcludesVolumes(t *testing.T) {
	output := []byte("{\"Type\":\"Images\",\"Reclaimable\":\"1.5GB (50%)\"}\n" +
		"{\"Type\":\"Local Volumes\",\"Reclaimable\":\"9GB (90%)\"}\n" +
		"{\"Type\":\"Build Cache\",\"Reclaimable\":\"500MB\"}\n")
	got, err := parseDockerDF(output)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2_000_000_000 {
		t.Fatalf("got %d", got)
	}
}

func TestGetDockerSizeMissingToolReturnsUnavailable(t *testing.T) {
	lookupErr := &exec.Error{Name: "docker", Err: exec.ErrNotFound}
	runCalled := false
	_, err := getDockerSizeWith(
		func(string) (string, error) { return "", lookupErr },
		func(context.Context, string) ([]byte, error) {
			runCalled = true
			return nil, nil
		},
	)
	var unavailable *dockerUnavailableError
	if !errors.As(err, &unavailable) || !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("missing docker error = %T %v", err, err)
	}
	if runCalled {
		t.Fatal("docker command ran after lookup failure")
	}
}

func TestSyscleanDryRunReturnsNonzeroOnMixedAnalysisError(t *testing.T) {
	testSyscleanMixedAnalysisError(t, true)
}

func TestSyscleanApplyFailsClosedOnMixedAnalysisError(t *testing.T) {
	testSyscleanMixedAnalysisError(t, false)
}

func TestSyscleanAnalysisErrorHelper(t *testing.T) {
	if os.Getenv("GO_WANT_SYSCLEAN_ANALYSIS_ERROR_HELPER") != "1" {
		return
	}
	args := []string{"sysclean", "--only=docker,npm-cache", "--workers=1", "--yes"}
	if os.Getenv("SYSCLEAN_HELPER_DRY_RUN") == "1" {
		args = append(args, "--dry-run")
	}
	os.Args = args
	main()
}

func testSyscleanMixedAnalysisError(t *testing.T, dryRun bool) {
	t.Helper()
	home := t.TempDir()
	cacheFile := filepath.Join(home, ".npm", "_cacache", "keep")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSyscleanAnalysisErrorHelper$")
	cmd.Env = append(
		os.Environ(),
		"GO_WANT_SYSCLEAN_ANALYSIS_ERROR_HELPER=1",
		"HOME="+home,
		"PATH=",
	)
	if dryRun {
		cmd.Env = append(cmd.Env, "SYSCLEAN_HELPER_DRY_RUN=1")
	}
	output, err := cmd.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		t.Fatalf("exit = %v, output:\n%s", err, output)
	}
	text := string(output)
	for _, wanted := range []string{"Docker Unused Data 분석 실패", "npm Cache", "안전을 위해 삭제하지 않습니다"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("missing %q:\n%s", wanted, text)
		}
	}
	if strings.Contains(text, "정리할 데이터가 없습니다") {
		t.Fatalf("analysis error reported as empty success:\n%s", text)
	}
	if got, err := os.ReadFile(cacheFile); err != nil || string(got) != "keep" {
		t.Fatalf("cache changed after partial analysis: %q, %v", got, err)
	}
}

func TestInspectArtifactReturnsError(t *testing.T) {
	_, _, err := inspectArtifact(filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("expected missing artifact error")
	}
}

func TestNonEmptyResultsExcludesAnalysisErrors(t *testing.T) {
	results := []AnalysisResult{
		{Target: CleanTarget{ID: "first"}, Size: 10},
		{Target: CleanTarget{ID: "broken"}, Size: 20, Error: os.ErrPermission},
		{Target: CleanTarget{ID: "third"}, Size: 30},
	}
	got := nonEmptyResults(results)
	if len(got) != 2 || got[0].Target.ID != "first" || got[1].Target.ID != "third" {
		t.Fatalf("selectable results = %#v", got)
	}
}

func TestScanProjectArtifactsFindsOldVenv(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	venv := filepath.Join(project, ".venv")
	if err := os.MkdirAll(venv, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(venv, "pyvenv.cfg")
	if err := os.WriteFile(marker, []byte("home = test"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(venv, old, old); err != nil {
		t.Fatal(err)
	}
	artifacts, err := scanProjectArtifacts(root, 4, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Path != venv {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	defer closeArtifactRoots(artifactTargets(artifacts))
}

func TestScanProjectArtifactsRejectsVenvWithoutRegularSelfMarker(t *testing.T) {
	tests := []struct {
		name          string
		symlinkMarker bool
	}{
		{name: "missing"},
		{name: "symlink", symlinkMarker: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			project := filepath.Join(root, "project")
			venv := filepath.Join(project, ".venv")
			if err := os.MkdirAll(venv, 0o700); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-48 * time.Hour)
			writeOldFile(t, filepath.Join(project, "pyproject.toml"), []byte("[project]"), old)
			writeOldFile(t, filepath.Join(venv, "old-data"), []byte("old"), old)
			if test.symlinkMarker {
				target := filepath.Join(root, "fake-pyvenv.cfg")
				writeOldFile(t, target, []byte("home = fake"), old)
				if err := os.Symlink(target, filepath.Join(venv, "pyvenv.cfg")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chtimes(venv, old, old); err != nil {
				t.Fatal(err)
			}
			parent, err := os.OpenRoot(project)
			if err != nil {
				t.Fatal(err)
			}
			var spec artifactSpec
			for _, candidate := range artifactSpecs {
				if candidate.Type == "Python venv" {
					spec = candidate
					break
				}
			}
			if spec.SelfMarker == "" {
				parent.Close()
				t.Fatal("Python venv self-marker spec missing")
			}
			if _, _, markerErr := findArtifactMarker(parent, ".venv", spec); markerErr == nil {
				parent.Close()
				t.Fatal("secure marker revalidation accepted invalid self marker")
			}
			parent.Close()

			artifacts, err := scanProjectArtifacts(root, 4, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(artifacts) != 0 {
				defer closeArtifactRoots(artifactTargets(artifacts))
				t.Fatalf("invalid venv accepted: %#v", artifacts)
			}
		})
	}
}

func TestArtifactCleanupRejectsReplacementAfterAnalysis(t *testing.T) {
	root := t.TempDir()
	venv := makeOldVenv(t, root, "project")
	artifacts, err := scanProjectArtifacts(root, 4, 1, 1)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("scan = %#v, %v", artifacts, err)
	}
	targets := artifactTargets(artifacts)
	defer closeArtifactRoots(targets)

	approved := venv + "-approved"
	if err := os.Rename(venv, approved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(venv, 0o700); err != nil {
		t.Fatal(err)
	}
	newMarker := filepath.Join(venv, "pyvenv.cfg")
	if err := os.WriteFile(newMarker, []byte("new environment"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(newMarker, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(venv, old, old); err != nil {
		t.Fatal(err)
	}

	cleaned := cleanTargets([]AnalysisResult{{Target: targets[0]}}, 1)
	if cleaned[0].Error == nil {
		t.Fatal("replacement artifact was accepted")
	}
	if got, err := os.ReadFile(newMarker); err != nil || string(got) != "new environment" {
		t.Fatalf("replacement data changed: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(approved, "pyvenv.cfg")); err != nil {
		t.Fatalf("approved artifact unexpectedly changed: %v", err)
	}
}

func TestArtifactCleanupDeletesUnchangedApprovedArtifact(t *testing.T) {
	root := t.TempDir()
	venv := makeOldVenv(t, root, "project")
	artifacts, err := scanProjectArtifacts(root, 4, 1, 1)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("scan = %#v, %v", artifacts, err)
	}
	targets := artifactTargets(artifacts)
	defer closeArtifactRoots(targets)

	cleaned := cleanTargets([]AnalysisResult{{Target: targets[0]}}, 1)
	if cleaned[0].Error != nil {
		t.Fatalf("approved artifact cleanup failed: %v", cleaned[0].Error)
	}
	if _, err := os.Lstat(venv); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("approved artifact still exists: %v", err)
	}
}

func TestArtifactCleanupParentSymlinkSwapCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	artifact := filepath.Join(project, "node_modules")
	if err := os.MkdirAll(artifact, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	writeOldFile(t, filepath.Join(project, "package.json"), []byte("{}"), old)
	writeOldFile(t, filepath.Join(artifact, "approved"), []byte("old"), old)
	if err := os.Chtimes(artifact, old, old); err != nil {
		t.Fatal(err)
	}
	artifacts, err := scanProjectArtifacts(root, 4, 1, 1)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("scan = %#v, %v", artifacts, err)
	}
	targets := artifactTargets(artifacts)
	defer closeArtifactRoots(targets)

	approvedProject := filepath.Join(root, "project-approved")
	if err := os.Rename(project, approvedProject); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideArtifact := filepath.Join(outside, "node_modules")
	if err := os.MkdirAll(outsideArtifact, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outsideArtifact, "sentinel")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, project); err != nil {
		t.Fatal(err)
	}

	cleaned := cleanTargets([]AnalysisResult{{Target: targets[0]}}, 1)
	if cleaned[0].Error == nil {
		t.Fatal("swapped intermediate parent was accepted")
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "outside" {
		t.Fatalf("outside sentinel changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(approvedProject, "node_modules", "approved")); err != nil || string(got) != "old" {
		t.Fatalf("approved data changed after parent swap: %q, %v", got, err)
	}
}

func TestArtifactCleanupRejectsFreshActivity(t *testing.T) {
	root := t.TempDir()
	venv := makeOldVenv(t, root, "project")
	artifacts, err := scanProjectArtifacts(root, 4, 1, 1)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("scan = %#v, %v", artifacts, err)
	}
	targets := artifactTargets(artifacts)
	defer closeArtifactRoots(targets)
	fresh := filepath.Join(venv, "fresh-data")
	if err := os.WriteFile(fresh, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleaned := cleanTargets([]AnalysisResult{{Target: targets[0]}}, 1)
	if cleaned[0].Error == nil {
		t.Fatal("fresh artifact activity was accepted")
	}
	if got, err := os.ReadFile(fresh); err != nil || string(got) != "keep" {
		t.Fatalf("fresh data changed: %q, %v", got, err)
	}
}

func TestArtifactCleanupRejectsMarkerMutation(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	artifact := filepath.Join(project, "node_modules")
	if err := os.MkdirAll(artifact, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	marker := filepath.Join(project, "package.json")
	writeOldFile(t, marker, []byte("{}"), old)
	writeOldFile(t, filepath.Join(artifact, "approved"), []byte("old"), old)
	if err := os.Chtimes(artifact, old, old); err != nil {
		t.Fatal(err)
	}
	artifacts, err := scanProjectArtifacts(root, 4, 1, 1)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("scan = %#v, %v", artifacts, err)
	}
	targets := artifactTargets(artifacts)
	defer closeArtifactRoots(targets)
	if err := os.WriteFile(marker, []byte("{\"changed\":true}"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleaned := cleanTargets([]AnalysisResult{{Target: targets[0]}}, 1)
	if cleaned[0].Error == nil {
		t.Fatal("mutated marker was accepted")
	}
	if got, err := os.ReadFile(filepath.Join(artifact, "approved")); err != nil || string(got) != "old" {
		t.Fatalf("artifact changed after marker mutation: %q, %v", got, err)
	}
}

func TestRemoveContentsUnlinksSymlinkWithoutFollowingIt(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := removeContents(root); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "outside" {
		t.Fatalf("outside sentinel changed: %q, %v", got, err)
	}
}

func makeOldVenv(t *testing.T, root, projectName string) string {
	t.Helper()
	venv := filepath.Join(root, projectName, ".venv")
	if err := os.MkdirAll(venv, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	writeOldFile(t, filepath.Join(venv, "pyvenv.cfg"), []byte("home = test"), old)
	if err := os.Chtimes(venv, old, old); err != nil {
		t.Fatal(err)
	}
	return venv
}

func writeOldFile(t *testing.T, path string, data []byte, old time.Time) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestParallelMapPreservesOrderAndLimit(t *testing.T) {
	items := []int{5, 4, 3, 2, 1}
	var active atomic.Int32
	var peak atomic.Int32
	got := parallelMap(items, 2, func(value int) int {
		current := active.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(time.Duration(value) * time.Millisecond)
		active.Add(-1)
		return value * 2
	})
	if !reflect.DeepEqual(got, []int{10, 8, 6, 4, 2}) {
		t.Fatalf("order changed: %v", got)
	}
	if peak.Load() > 2 {
		t.Fatalf("peak workers = %d", peak.Load())
	}
}
