package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runJSON(t *testing.T, args ...string) (int, report, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args = append([]string{"--json"}, args...)
	code := run(args, &stdout, &stderr)
	var result report
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode JSON: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	return code, result, stderr.String()
}

func TestExactDuplicatesAndNonDuplicate(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "same content")
	writeTestFile(t, root, "nested/b.txt", "same content")
	writeTestFile(t, root, "other.txt", "different!!!")

	code, result, stderr := runJSON(t, root)
	if code != 1 {
		t.Fatalf("code=%d, want 1; stderr=%s", code, stderr)
	}
	if len(result.DuplicateGroups) != 1 {
		t.Fatalf("groups=%#v", result.DuplicateGroups)
	}
	group := result.DuplicateGroups[0]
	if len(group.Files) != 2 || group.ReclaimableBytes != int64(len("same content")) {
		t.Fatalf("group=%#v", group)
	}
}

func TestSameSizeDifferentHash(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a", "abcd")
	writeTestFile(t, root, "b", "wxyz")

	code, result, _ := runJSON(t, root)
	if code != 0 || len(result.DuplicateGroups) != 0 {
		t.Fatalf("code=%d result=%#v", code, result)
	}
	if result.HashedFiles != 2 {
		t.Fatalf("hashed=%d, want 2", result.HashedFiles)
	}
}

func TestHardlinkHasNoReclaimableBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hardlink permissions vary on Windows")
	}
	root := t.TempDir()
	original := writeTestFile(t, root, "original", "hardlinked")
	if err := os.Link(original, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}

	code, result, _ := runJSON(t, root)
	if code != 1 || len(result.DuplicateGroups) != 1 {
		t.Fatalf("code=%d groups=%#v", code, result.DuplicateGroups)
	}
	if result.DuplicateGroups[0].ReclaimableBytes != 0 {
		t.Fatalf("reclaimable=%d", result.DuplicateGroups[0].ReclaimableBytes)
	}
}

func TestSymlinkIgnored(t *testing.T) {
	root := t.TempDir()
	target := writeTestFile(t, root, "target", "content")
	if err := os.Symlink(target, filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	code, result, _ := runJSON(t, root)
	if code != 0 || result.SkippedSymlinks != 1 || len(result.DuplicateGroups) != 0 {
		t.Fatalf("code=%d result=%#v", code, result)
	}
}

func TestMaxFilesProducesPartialResult(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a", "same")
	writeTestFile(t, root, "b", "same")

	code, result, _ := runJSON(t, "--max-files", "1", root)
	if code != 3 || !result.Partial || result.ScannedFiles != 1 {
		t.Fatalf("code=%d result=%#v", code, result)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error, "maximum file count") {
		t.Fatalf("errors=%#v", result.Errors)
	}
}

func TestMaxDepthProducesPartialResult(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "nested/a", "same")

	code, result, _ := runJSON(t, "--max-depth", "0", root)
	if code != 3 || !result.Partial || result.ScannedFiles != 0 {
		t.Fatalf("code=%d result=%#v", code, result)
	}
}

func TestJSONIsDeterministic(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "z", "same")
	writeTestFile(t, root, "a", "same")

	var first, second, stderr bytes.Buffer
	if code := run([]string{"--json", root}, &first, &stderr); code != 1 {
		t.Fatalf("first code=%d stderr=%s", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"--json", root}, &second, &stderr); code != 1 {
		t.Fatalf("second code=%d stderr=%s", code, stderr.String())
	}
	if first.String() != second.String() {
		t.Fatalf("JSON differs:\n%s\n%s", first.String(), second.String())
	}
}

func TestRejectsFileAndSymlinkRoots(t *testing.T) {
	root := t.TempDir()
	file := writeTestFile(t, root, "file", "data")
	for _, candidate := range []string{file} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{candidate}, &stdout, &stderr); code != 2 {
			t.Fatalf("root=%s code=%d", candidate, code)
		}
	}
	link := filepath.Join(root, "root-link")
	if err := os.Symlink(root, link); err == nil {
		var stdout, stderr bytes.Buffer
		if code := run([]string{link}, &stdout, &stderr); code != 2 {
			t.Fatalf("symlink root code=%d", code)
		}
	}
}

func TestHelpReturnsSuccessAndParseErrorsRemainUsageErrors(t *testing.T) {
	for _, helpFlag := range []string{"-h", "--help"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{helpFlag}, &stdout, &stderr); code != 0 {
			t.Fatalf("run(%q) code=%d, want 0", helpFlag, code)
		}
		if !strings.Contains(stderr.String(), "Usage: dedupe") {
			t.Fatalf("run(%q) missing usage: %q", helpFlag, stderr.String())
		}
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--workers", "not-a-number"}, &stdout, &stderr); code != 2 {
		t.Fatalf("parse error code=%d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid value") {
		t.Fatalf("missing parse error: %q", stderr.String())
	}
}

func TestMinSizeUsesCanonicalParser(t *testing.T) {
	var stderr bytes.Buffer
	opts, err := parseOptions([]string{"--min-size", ".5B"}, &stderr)
	if err != nil || opts.minSize != 0 {
		t.Fatalf("floor parse: minSize=%d err=%v stderr=%q", opts.minSize, err, stderr.String())
	}

	stderr.Reset()
	if _, err := parseOptions([]string{"--min-size", "-1MB"}, &stderr); err == nil {
		t.Fatal("negative size accepted")
	}
	if !strings.Contains(stderr.String(), `invalid --min-size "-1MB"`) {
		t.Fatalf("missing min-size error: %q", stderr.String())
	}
}

func TestHashFileRejectsChangedMetadata(t *testing.T) {
	root := t.TempDir()
	path := writeTestFile(t, root, "file", "content")
	oldTime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	record := fileRecord{path: path, size: info.Size(), modTime: info.ModTime().UnixNano(), info: info}
	newTime := oldTime.Add(time.Second)
	if err := os.Chtimes(path, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hashFile(record); err == nil || !strings.Contains(err.Error(), "changed before hashing") {
		t.Fatalf("hashFile error=%v", err)
	}
}
