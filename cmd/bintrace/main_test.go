package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverCommandCandidatesRetainsDuplicatesAndMarksActive(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	for _, directory := range []string{first, second} {
		path := filepath.Join(directory, "tool")
		if err := os.WriteFile(path, []byte("candidate"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	candidates := discoverCommandCandidates("tool", strings.Join([]string{first, first, second}, string(os.PathListSeparator)))
	if len(candidates) != 3 {
		t.Fatalf("candidate count = %d, want 3: %#v", len(candidates), candidates)
	}
	if !candidates[0].Active || candidates[1].Active || candidates[2].Active {
		t.Fatalf("active flags = %#v", candidates)
	}
	wantPaths := []string{filepath.Join(first, "tool"), filepath.Join(first, "tool"), filepath.Join(second, "tool")}
	for i, want := range wantPaths {
		if candidates[i].CandidatePath != want {
			t.Errorf("candidate[%d] path = %q, want %q", i, candidates[i].CandidatePath, want)
		}
	}
}

func TestInspectCandidateSymlinkAndBrokenSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	link := filepath.Join(directory, "link")
	broken := filepath.Join(directory, "broken")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(directory, "missing"), broken); err != nil {
		t.Fatal(err)
	}

	linked := inspectCandidate(link, true)
	if linked.Status != statusOK || linked.SymlinkResolvedPath == "" || linked.SHA256 == "" {
		t.Fatalf("linked candidate = %#v", linked)
	}
	brokenReport := inspectCandidate(broken, true)
	if brokenReport.Status != statusBrokenSymlink {
		t.Fatalf("broken status = %q, want %q (%#v)", brokenReport.Status, statusBrokenSymlink, brokenReport)
	}
}

func TestInspectCandidateReportsNonExecutableRegularPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "go.mod")
	if err := os.WriteFile(path, []byte("module example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	report := inspectCandidate(path, true)
	if report.Status != statusNotExecutable || report.Executable {
		t.Fatalf("non-executable report = %#v", report)
	}
	if report.SHA256 == "" || report.Size == 0 || report.Mode == "" || report.MTime == "" {
		t.Fatalf("non-executable metadata missing: %#v", report)
	}
}

func TestInspectCandidateReportsExecutableDirectPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}

	report := inspectCandidate(path, true)
	if report.Status != statusOK || !report.Executable {
		t.Fatalf("executable report = %#v", report)
	}
}

func TestHashFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload")
	payload := []byte("bintrace hash input\n")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(payload)
	want := hex.EncodeToString(wantDigest[:])
	got, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}
}

func TestInspectFileSnapshotRejectsSameMetadataReplacement(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "tool")
	if err := os.WriteFile(target, []byte("same"), 0o755); err != nil {
		t.Fatal(err)
	}
	expected, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement")
	if err := os.WriteFile(replacement, []byte("size"), expected.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, expected.ModTime(), expected.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, target); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := inspectFileSnapshot(target, expected); !errors.Is(err, errCandidateChanged) {
		t.Fatalf("inspectFileSnapshot error = %v, want errCandidateChanged", err)
	}
}

func TestRunJSONIsValid(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "json-tool")
	if err := os.WriteFile(target, []byte("json payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--json", "json-tool"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run code = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
	}
	var report TraceReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v (%q)", err, stdout.String())
	}
	if report.RequestedPath != "json-tool" || report.Status != statusOK || len(report.Candidates) != 1 {
		t.Fatalf("report = %#v", report)
	}
	if report.Candidates[0].SHA256 == "" || report.Candidates[0].CandidatePath != target {
		t.Fatalf("candidate = %#v", report.Candidates[0])
	}
}

func TestRunHelpWritesUsageToStdout(t *testing.T) {
	for _, argument := range []string{"-h", "--help"} {
		t.Run(argument, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run([]string{argument}, &stdout, &stderr); code != 0 {
				t.Fatalf("run code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
			if stdout.String() != "usage: bintrace [--json] <command-or-path>\n" {
				t.Fatalf("stdout = %q", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestRunParseErrorUsesStderrAndExitTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("run code = %d, want 2", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "command or path is required") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestRunDoesNotExecuteTarget(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "not-run")
	marker := filepath.Join(directory, "executed")
	content := []byte("#!/bin/sh\ntouch " + marker + "\n")
	if err := os.WriteFile(target, content, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{target}, &stdout, &stderr); code != 0 {
		t.Fatalf("run code = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("target was executed; marker stat error = %v", err)
	}
}
