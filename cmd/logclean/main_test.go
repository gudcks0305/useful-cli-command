package main

import (
	"bytes"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunDefaultIsAnalysisOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldFile := makeAgedFile(t, filepath.Join(home, "Library", "Logs", "old.log"), 10*24*time.Hour)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--days", "7"}, strings.NewReader("y\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("default run deleted file: %v", err)
	}
	if !strings.Contains(stdout.String(), "--apply") {
		t.Fatalf("stdout missing apply guidance: %s", stdout.String())
	}
}

func TestRunApplyConfirmationDeletesOnlyOldRegularFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldFile := makeAgedFile(t, filepath.Join(home, "Library", "Logs", "old.log"), 10*24*time.Hour)
	newFile := makeAgedFile(t, filepath.Join(home, "Library", "Logs", "new.log"), time.Hour)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--apply", "--days", "7"}, strings.NewReader("yes\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(oldFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old file still exists or unexpected error: %v", err)
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Fatalf("new file deleted: %v", err)
	}
	if !strings.Contains(stdout.String(), "정리 완료") {
		t.Fatalf("stdout missing success: %s", stdout.String())
	}
}

func TestRunApplyRequiresPositiveConfirmation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldFile := makeAgedFile(t, filepath.Join(home, "Library", "Logs", "old.log"), 10*24*time.Hour)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--apply"}, strings.NewReader("n\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("declined apply deleted file: %v", err)
	}
}

func TestRunRejectsSymlinkRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	external := t.TempDir()
	oldFile := makeAgedFile(t, filepath.Join(external, "old.log"), 10*24*time.Hour)
	if err := os.MkdirAll(filepath.Join(home, "Library"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(home, "Library", "Logs")); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--apply"}, strings.NewReader("y\n"), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run() succeeded, stderr=%s", stderr.String())
	}
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("file behind symlink changed: %v", err)
	}
	if !strings.Contains(stderr.String(), "symlink root 거부") {
		t.Fatalf("stderr missing symlink rejection: %s", stderr.String())
	}
}

func TestRunRejectsNegativeDays(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--days", "-1"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "0 이상") {
		t.Fatalf("stderr missing validation: %s", stderr.String())
	}
}

func TestBroadTargetsRequireOptIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cacheFile := makeAgedFile(t, filepath.Join(home, "Library", "Caches", "app", "cache.bin"), 10*24*time.Hour)
	trashFile := makeAgedFile(t, filepath.Join(home, ".Trash", "old.txt"), 10*24*time.Hour)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--apply"}, strings.NewReader("y\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("default run() = %d, stderr=%s", code, stderr.String())
	}
	for _, path := range []string{cacheFile, trashFile} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("default apply touched broad target %s: %v", path, err)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--apply", "--caches"}, strings.NewReader("y\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("cache opt-in run() = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(cacheFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opted-in cache file remains: %v", err)
	}
	if _, err := os.Stat(trashFile); err != nil {
		t.Fatalf("trash deleted without opt-in: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--apply", "--trash"}, strings.NewReader("y\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("trash opt-in run() = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(trashFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opted-in trash file remains: %v", err)
	}
}

func TestRunSkipsSymlinkEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logs := filepath.Join(home, "Library", "Logs")
	makeAgedFile(t, filepath.Join(logs, "regular.log"), 10*24*time.Hour)
	external := makeAgedFile(t, filepath.Join(t.TempDir(), "external.log"), 10*24*time.Hour)
	if err := os.Symlink(external, filepath.Join(logs, "linked.log")); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--apply"}, strings.NewReader("y\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatalf("symlink target changed: %v", err)
	}
}

func TestRunRejectsFileChangedAfterAnalysis(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldFile := makeAgedFile(t, filepath.Join(home, "Library", "Logs", "old.log"), 10*24*time.Hour)
	input := readerFunc(func(p []byte) (int, error) {
		if err := os.WriteFile(oldFile, []byte("changed after analysis"), 0o600); err != nil {
			t.Fatal(err)
		}
		return strings.NewReader("y\n").Read(p)
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--apply"}, input, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run() succeeded, stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "분석 후 변경됨") {
		t.Fatalf("stderr missing changed-file rejection: %s", stderr.String())
	}
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("changed file deleted: %v", err)
	}
}

func TestRunSurfacesWalkError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	makeAgedFile(t, filepath.Join(home, "Library", "Logs", "old.log"), 10*24*time.Hour)
	ops := defaultCleanerOps
	ops.walkDir = func(string, iofs.WalkDirFunc) error { return errors.New("walk denied") }

	var stdout, stderr bytes.Buffer
	code := runWithOps(nil, strings.NewReader(""), &stdout, &stderr, ops)
	if code == 0 {
		t.Fatalf("runWithOps() succeeded, stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "walk denied") {
		t.Fatalf("stderr missing walk error: %s", stderr.String())
	}
}

func TestRunSurfacesDeleteErrorAndDoesNotClaimSuccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldFile := makeAgedFile(t, filepath.Join(home, "Library", "Logs", "old.log"), 10*24*time.Hour)
	ops := defaultCleanerOps
	ops.openRoot = func(path string) (rootHandle, error) {
		root, err := os.OpenRoot(path)
		if err != nil {
			return nil, err
		}
		return &removeErrorRoot{rootHandle: root}, nil
	}

	var stdout, stderr bytes.Buffer
	code := runWithOps([]string{"--apply"}, strings.NewReader("y\n"), &stdout, &stderr, ops)
	if code == 0 {
		t.Fatalf("runWithOps() succeeded, stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "remove denied") || !strings.Contains(stderr.String(), "부분 완료") {
		t.Fatalf("stderr missing delete error: %s", stderr.String())
	}
	if strings.Contains(stdout.String(), "정리 완료") {
		t.Fatalf("stdout falsely claims success: %s", stdout.String())
	}
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("failed deletion changed file: %v", err)
	}
}

func TestRootRemoveResistsIntermediateSymlinkSwap(t *testing.T) {
	probeParent := t.TempDir()
	probeTarget := t.TempDir()
	probeLink := filepath.Join(probeParent, "link")
	if err := os.Symlink(probeTarget, probeLink); err != nil {
		t.Skipf("platform cannot create symlinks: %v", err)
	}
	if err := os.Remove(probeLink); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	logs := filepath.Join(home, "Library", "Logs")
	innerDir := filepath.Join(logs, "nested")
	parkedDir := filepath.Join(logs, "nested-before-swap")
	innerFile := makeAgedFile(t, filepath.Join(innerDir, "victim.log"), 10*24*time.Hour)
	outsideDir := t.TempDir()
	outsideSentinel := makeAgedFile(t, filepath.Join(outsideDir, "victim.log"), 10*24*time.Hour)

	ops := defaultCleanerOps
	ops.openRoot = func(path string) (rootHandle, error) {
		root, err := os.OpenRoot(path)
		if err != nil {
			return nil, err
		}
		return &swapOnRemoveRoot{
			rootHandle: root,
			before: func() error {
				if err := os.Rename(innerDir, parkedDir); err != nil {
					return err
				}
				return os.Symlink(outsideDir, innerDir)
			},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := runWithOps([]string{"--apply"}, strings.NewReader("y\n"), &stdout, &stderr, ops)
	if code == 0 {
		t.Fatalf("runWithOps() succeeded after symlink swap; stdout=%s", stdout.String())
	}
	if _, err := os.Stat(outsideSentinel); err != nil {
		t.Fatalf("outside sentinel was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parkedDir, filepath.Base(innerFile))); err != nil {
		t.Fatalf("original in-root file unexpectedly removed: %v", err)
	}
}

func TestRunRejectsApplyDryRunConflictWithoutDeletion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldFile := makeAgedFile(t, filepath.Join(home, "Library", "Logs", "old.log"), 10*24*time.Hour)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--apply", "--dry-run"}, strings.NewReader("y\n"), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run() = %d, want 2; stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("conflicting flags deleted file: %v", err)
	}
	if !strings.Contains(stderr.String(), "함께 사용할 수 없습니다") {
		t.Fatalf("stderr missing conflict error: %s", stderr.String())
	}
}

func makeAgedFile(t *testing.T, path string, age time.Duration) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return path
}

type readerFunc func([]byte) (int, error)

func (fn readerFunc) Read(p []byte) (int, error) {
	return fn(p)
}

var _ io.Reader = readerFunc(nil)

type removeErrorRoot struct {
	rootHandle
}

func (r *removeErrorRoot) Remove(string) error {
	return errors.New("remove denied")
}

type swapOnRemoveRoot struct {
	rootHandle
	before  func() error
	swapped bool
}

func (r *swapOnRemoveRoot) Remove(name string) error {
	if !r.swapped {
		r.swapped = true
		if err := r.before(); err != nil {
			return err
		}
	}
	return r.rootHandle.Remove(name)
}
