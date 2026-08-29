package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLimitedBufferDoesNotExposeReaderFrom(t *testing.T) {
	if _, ok := any(&limitedBuffer{}).(io.ReaderFrom); ok {
		t.Fatal("limitedBuffer exposes io.ReaderFrom and can bypass Write limits")
	}
}

type fakeResponse struct {
	stdout string
	stderr string
	err    error
}

type fakeRunner struct {
	responses map[string]fakeResponse
	calls     []string
}

func (f *fakeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	key := strings.Join(args, "\x00")
	f.calls = append(f.calls, key)
	response, ok := f.responses[key]
	if !ok {
		return nil, nil, errors.New("unexpected command: " + strings.Join(args, " "))
	}
	return []byte(response.stdout), []byte(response.stderr), response.err
}

func commandKey(args ...string) string { return strings.Join(args, "\x00") }

func TestParseStatus(t *testing.T) {
	data := strings.Join([]string{
		"# branch.oid abc",
		"# branch.head main",
		"# branch.upstream origin/main",
		"# branch.ab +2 -3",
		"1 M. N... 100644 100644 100644 abc abc staged.txt",
		"1 .M N... 100644 100644 100644 abc abc unstaged.txt",
		"2 MM N... 100644 100644 100644 abc abc R100 renamed.txt",
		"old-name.txt",
		"u UU N... 100644 100644 100644 100644 abc abc abc conflict.txt",
		"? untracked.txt",
		"",
	}, "\x00")
	report := result{}
	parseStatus([]byte(data), &report)

	if got, want := report.Branch, (branchResult{Name: "main", Upstream: "origin/main", Ahead: 2, Behind: 3}); got != want {
		t.Fatalf("branch = %#v, want %#v", got, want)
	}
	if got, want := report.Changes, (changeResult{Staged: 3, Unstaged: 3, Untracked: 1, Conflicts: 1}); got != want {
		t.Fatalf("changes = %#v, want %#v", got, want)
	}
}

func TestParseWorktrees(t *testing.T) {
	data := "worktree /z\x00HEAD abc\x00detached\x00locked reason\x00\x00" +
		"worktree /a\x00HEAD def\x00branch refs/heads/main\x00prunable missing\x00\x00"
	got := parseWorktrees([]byte(data))
	want := worktreeResult{
		Total: 2, Stale: 1, Prunable: 1, Locked: 1,
		Worktrees: []worktreeDetail{
			{Path: "/a", Branch: "main", Prunable: true},
			{Path: "/z", Detached: true, Locked: true},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("worktrees = %#v, want %#v", got, want)
	}
}

func TestClassifyGoneUpstream(t *testing.T) {
	runner := &fakeRunner{responses: map[string]fakeResponse{
		commandKey("rev-parse", "--verify", "--quiet", "@{upstream}"): {err: errors.New("exit status 1")},
	}}
	report := result{Branch: branchResult{Name: "main", Upstream: "origin/main"}}
	classifyUpstream(context.Background(), "/repo", runner, &report)
	addStatusFindings(&report)
	if report.Branch.UpstreamState != "gone" {
		t.Fatalf("upstream state = %q, want gone", report.Branch.UpstreamState)
	}
	if len(report.Findings) != 1 || report.Findings[0].Code != "upstream_gone" {
		t.Fatalf("findings = %#v", report.Findings)
	}
}

func TestFindLocks(t *testing.T) {
	commonDir := t.TempDir()
	for _, name := range []string{"index.lock", filepath.Join("refs", "heads", "main.lock")} {
		path := filepath.Join(commonDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	locks, err := findLocks(commonDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"index.lock", "refs/heads/main.lock"}
	if !reflect.DeepEqual(locks, want) {
		t.Fatalf("locks = %v, want %v", locks, want)
	}
}

func TestRunWithFakeRunnerJSON(t *testing.T) {
	commonDir := t.TempDir()
	inputDir := t.TempDir()
	runner := &fakeRunner{responses: map[string]fakeResponse{
		commandKey("--version"):                                               {stdout: "git version test\n"},
		commandKey("rev-parse", "--is-inside-work-tree"):                      {stdout: "true\n"},
		commandKey("rev-parse", "--show-toplevel"):                            {stdout: "/repo\n"},
		commandKey("config", "--name-only", "--list", "-z"):                   {stdout: "core.bare\x00"},
		commandKey("ls-files", "--stage", "-z"):                               {},
		commandKey("status", "--porcelain=v2", "--branch", "-z"):              {stdout: "# branch.oid abc\x00# branch.head main\x00# branch.ab +1 -0\x00? new.txt\x00"},
		commandKey("config", "--get", "branch.main.remote"):                   {err: errors.New("exit status 1")},
		commandKey("worktree", "list", "--porcelain", "-z"):                   {stdout: "worktree /repo\x00HEAD abc\x00branch refs/heads/main\x00\x00"},
		commandKey("rev-parse", "--path-format=absolute", "--git-common-dir"): {stdout: commonDir + "\n"},
	}}
	var stdout, stderr bytes.Buffer
	exitCode := runWithRunner([]string{"--json", inputDir}, &stdout, &stderr, runner)
	if exitCode != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", exitCode, stderr.String())
	}
	var report result
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if report.Path != "/repo" || report.Branch.Ahead != 1 || report.Changes.Untracked != 1 {
		t.Fatalf("unexpected report: %#v", report)
	}
	gotCodes := []string{}
	for _, item := range report.Findings {
		gotCodes = append(gotCodes, item.Code)
	}
	wantCodes := []string{"ahead", "untracked_files", "upstream_missing"}
	if !reflect.DeepEqual(gotCodes, wantCodes) {
		t.Fatalf("finding codes = %v, want %v", gotCodes, wantCodes)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "fsck") {
			t.Fatal("fsck ran without --fsck")
		}
	}
}

func TestRunFsckOnlyWhenRequested(t *testing.T) {
	commonDir := t.TempDir()
	inputDir := t.TempDir()
	runner := &fakeRunner{responses: map[string]fakeResponse{
		commandKey("--version"):                                               {stdout: "git version test\n"},
		commandKey("rev-parse", "--is-inside-work-tree"):                      {stdout: "true\n"},
		commandKey("rev-parse", "--show-toplevel"):                            {stdout: "/repo\n"},
		commandKey("config", "--name-only", "--list", "-z"):                   {stdout: "core.bare\x00"},
		commandKey("ls-files", "--stage", "-z"):                               {},
		commandKey("status", "--porcelain=v2", "--branch", "-z"):              {stdout: "# branch.oid abc\x00# branch.head (detached)\x00"},
		commandKey("worktree", "list", "--porcelain", "-z"):                   {stdout: "worktree /repo\x00HEAD abc\x00detached\x00\x00"},
		commandKey("rev-parse", "--path-format=absolute", "--git-common-dir"): {stdout: commonDir + "\n"},
		commandKey("fsck", "--no-progress", "--no-dangling"):                  {},
	}}
	var stdout, stderr bytes.Buffer
	if exitCode := runWithRunner([]string{"--json", "--fsck", inputDir}, &stdout, &stderr, runner); exitCode != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	if !contains(runner.calls, commandKey("fsck", "--no-progress", "--no-dangling")) {
		t.Fatal("fsck command was not called")
	}
}

func TestIntegrationDirtyRepository(t *testing.T) {
	repo := t.TempDir()
	runGitTest(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := run([]string{"--json", repo}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	var report result
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if report.Changes.Staged != 1 || report.Changes.Untracked != 1 {
		t.Fatalf("changes = %#v", report.Changes)
	}
	if report.Repository != true || report.Partial {
		t.Fatalf("repository=%v partial=%v errors=%v", report.Repository, report.Partial, report.Errors)
	}
}

func TestIntegrationNonRepository(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := run([]string{"--json", t.TempDir()}, &stdout, &stderr)
	if exitCode != 2 {
		t.Fatalf("exit = %d, want 2", exitCode)
	}
	var report fatalResult
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if report.Error.Code != "not_repository" {
		t.Fatalf("error code = %q", report.Error.Code)
	}
}

func TestFsmonitorHookIsNotExecuted(t *testing.T) {
	repo := initializedCommittedRepo(t)
	sentinel := filepath.Join(repo, "fsmonitor-sentinel")
	hook := filepath.Join(repo, "fsmonitor-hook")
	script := "#!/bin/sh\n: > " + sentinel + "\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "config", "core.fsmonitor", hook)

	// Prove malicious fixture executes without gitdoctor hardening.
	runGitTest(t, repo, "status", "--porcelain=v2", "--branch", "-z")
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("fsmonitor fixture did not execute: %v", err)
	}
	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := run([]string{"--json", repo}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("exit = %d, want findings exit 1; stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gitdoctor executed core.fsmonitor hook; stat error=%v", err)
	}
}

func TestGlobalFsmonitorConfigIsIgnored(t *testing.T) {
	repo := initializedCommittedRepo(t)
	sentinel := filepath.Join(repo, "global-fsmonitor-sentinel")
	hook := filepath.Join(repo, "global-fsmonitor-hook")
	script := "#!/bin/sh\n: > " + sentinel + "\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	runGitTest(t, repo, "config", "--file", globalConfig, "core.fsmonitor", hook)
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

	// Prove inherited global configuration is active for an ordinary Git call.
	runGitTest(t, repo, "status", "--porcelain=v2", "--branch", "-z")
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("global fsmonitor fixture did not execute: %v", err)
	}
	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if exitCode := run([]string{"--json", repo}, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("exit = %d, want findings exit 1; stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gitdoctor executed global core.fsmonitor hook; stat error=%v", err)
	}
}

func TestExternalCleanFilterRefusesStatusWithoutExecution(t *testing.T) {
	repo := initializedCommittedRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("tracked.txt filter=probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", ".gitattributes")
	runGitTest(t, repo, "commit", "-qm", "attributes")

	tracked := filepath.Join(repo, "tracked.txt")
	info, err := os.Stat(tracked)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(repo, "filter-sentinel")
	filter := filepath.Join(repo, "clean-filter")
	script := "#!/bin/sh\n: > " + sentinel + "\ncat\n"
	if err := os.WriteFile(filter, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "config", "filter.probe.clean", filter)
	if err := os.WriteFile(tracked, []byte("bbbb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tracked, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	// Same-size/racy tracked content makes plain git status invoke clean filter.
	runGitTest(t, repo, "status", "--porcelain=v2", "--branch", "-z")
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("clean filter fixture did not execute: %v", err)
	}
	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := run([]string{"--json", repo}, &stdout, &stderr)
	if exitCode != 3 {
		t.Fatalf("exit = %d, want partial exit 3; stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gitdoctor executed clean filter; stat error=%v", err)
	}
	var report result
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if !contains(report.Errors, "unsafe_filter_config") {
		t.Fatalf("errors = %v, want unsafe_filter_config", report.Errors)
	}
}

func TestExternalFilterKeyDetection(t *testing.T) {
	data := []byte("filter.safe.smudge\x00Filter.Danger.Clean\x00filter.stream.process\x00filter.stream.required\x00")
	want := []string{"filter.danger.clean", "filter.stream.process"}
	if got := externalFilterKeys(data); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestSubmoduleCleanFilterRefusesParentStatus(t *testing.T) {
	subSource := initializedCommittedRepo(t)
	super := initializedCommittedRepo(t)
	runGitTest(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subSource, "sub")
	runGitTest(t, super, "commit", "-qm", "add submodule")

	sub := filepath.Join(super, "sub")
	runGitTest(t, sub, "config", "user.name", "gitdoctor test")
	runGitTest(t, sub, "config", "user.email", "gitdoctor@example.invalid")
	if err := os.WriteFile(filepath.Join(sub, ".gitattributes"), []byte("tracked.txt filter=probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, sub, "add", ".gitattributes")
	runGitTest(t, sub, "commit", "-qm", "attributes")
	runGitTest(t, super, "add", "sub")
	runGitTest(t, super, "commit", "-qm", "update submodule")

	tracked := filepath.Join(sub, "tracked.txt")
	info, err := os.Stat(tracked)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(sub, "sub-filter-sentinel")
	filter := filepath.Join(sub, "clean-filter")
	script := "#!/bin/sh\n: > " + sentinel + "\ncat\n"
	if err := os.WriteFile(filter, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, sub, "config", "filter.probe.clean", filter)
	if err := os.WriteFile(tracked, []byte("bbbb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tracked, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := run([]string{"--json", super}, &stdout, &stderr)
	if exitCode != 3 {
		t.Fatalf("exit = %d, want partial exit 3; stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gitdoctor executed submodule clean filter; stat error=%v", err)
	}
	var report result
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !contains(report.Errors, "unsafe_filter_config") {
		t.Fatalf("errors = %v, want unsafe_filter_config", report.Errors)
	}
}

func TestGitlinkPaths(t *testing.T) {
	data := []byte("100644 abc 0\tfile\x00160000 def 0\tz-sub\x00160000 ghi 2\ta-sub\x00160000 jkl 3\ta-sub\x00")
	want := []string{"a-sub", "z-sub"}
	if got := gitlinkPaths(data); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

func TestTimeoutValidation(t *testing.T) {
	for _, value := range []string{"0s", "6m"} {
		var stdout, stderr bytes.Buffer
		if exitCode := run([]string{"--timeout", value}, &stdout, &stderr); exitCode != 2 {
			t.Fatalf("timeout %s exit = %d, want 2", value, exitCode)
		}
	}
}

func TestHelpAndParseErrorExitCodes(t *testing.T) {
	for _, helpFlag := range []string{"-h", "--help"} {
		var stdout, stderr bytes.Buffer
		if exitCode := run([]string{helpFlag}, &stdout, &stderr); exitCode != 0 {
			t.Fatalf("%s exit = %d, want 0", helpFlag, exitCode)
		}
		if !strings.Contains(stderr.String(), "Usage of gitdoctor:") {
			t.Fatalf("%s did not print usage: %q", helpFlag, stderr.String())
		}
	}

	var stdout, stderr bytes.Buffer
	if exitCode := run([]string{"--unknown"}, &stdout, &stderr); exitCode != 2 {
		t.Fatalf("parse error exit = %d, want 2", exitCode)
	}
}

func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func initializedCommittedRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGitTest(t, repo, "init", "-q")
	runGitTest(t, repo, "config", "user.name", "gitdoctor test")
	runGitTest(t, repo, "config", "user.email", "gitdoctor@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("aaaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "tracked.txt")
	runGitTest(t, repo, "commit", "-qm", "initial")
	return repo
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
