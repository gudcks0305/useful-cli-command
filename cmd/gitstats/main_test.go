package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type dirGitRunner string

func (dir dirGitRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	gitArgs := append([]string{"-C", string(dir)}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, out)
	}
	return out, nil
}

type failingRunner struct {
	calls int
}

func (runner *failingRunner) Run(context.Context, ...string) ([]byte, error) {
	runner.calls++
	if runner.calls == 1 {
		return []byte("true\n"), nil
	}
	return nil, errors.New("injected failure")
}

func TestParseOptionsRejectsNegativeValues(t *testing.T) {
	for _, args := range [][]string{{"--days=-1"}, {"--top=-1"}} {
		if _, err := parseOptions(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("parseOptions(%v) succeeded", args)
		}
	}
}

func TestHelpAndInvalidArgs(t *testing.T) {
	for _, help := range []string{"-h", "--help"} {
		var output bytes.Buffer
		runner := &failingRunner{}
		a := app{git: runner, out: &output, now: time.Now}
		if err := a.run(context.Background(), []string{help}); err != nil {
			t.Fatalf("run(%q): %v", help, err)
		}
		if runner.calls != 0 || !strings.Contains(output.String(), "Usage: gitstats [options]") {
			t.Fatalf("help output/calls = %q, %d", output.String(), runner.calls)
		}
	}

	if err := (app{git: &failingRunner{}, out: &bytes.Buffer{}, now: time.Now}).run(context.Background(), []string{"--unknown"}); err == nil {
		t.Fatal("invalid flag succeeded")
	}
}

func TestParseAuthorStatsMachineFormat(t *testing.T) {
	input := []byte("\x1e홍 길동\x00\x00\n2\t1\todd name\x00\x1eAlice\x00\x00\n-\t-\tbinary\x00")
	got, err := parseAuthorStats(input)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]AuthorStats{
		"홍 길동":  {Name: "홍 길동", Commits: 1, Additions: 2, Deletions: 1},
		"Alice": {Name: "Alice", Commits: 1},
	}
	for _, stats := range got {
		if !reflect.DeepEqual(stats, want[stats.Name]) {
			t.Fatalf("stats[%q] = %#v", stats.Name, stats)
		}
	}
}

func TestRunReportsCommandFailure(t *testing.T) {
	runner := &failingRunner{}
	a := app{git: runner, out: &bytes.Buffer{}, now: time.Now}
	err := a.run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("run error = %v", err)
	}
}

func TestRunInNonRepository(t *testing.T) {
	a := app{git: dirGitRunner(t.TempDir()), out: &bytes.Buffer{}, now: time.Now}
	err := a.run(context.Background(), nil)
	if !errors.Is(err, errNotGitRepo) {
		t.Fatalf("run error = %v, want errNotGitRepo", err)
	}
}

func TestHardenedGitStatsInvocation(t *testing.T) {
	args := hardenedGitStatsArgs([]string{"status", "--porcelain=v2"})
	wantPrefix := []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull}
	if !reflect.DeepEqual(args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("args prefix = %#v, want %#v", args, wantPrefix)
	}

	env := hardenedGitStatsEnv([]string{
		"PATH=/bin",
		"LC_ALL=ko_KR.UTF-8",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.fsmonitor",
		"GIT_CONFIG_VALUE_0=/tmp/unsafe",
		"GIT_OPTIONAL_LOCKS=1",
		"GIT_DIR=/tmp/other",
	})
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{
		"ko_KR.UTF-8",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=",
		"GIT_CONFIG_VALUE_0=",
		"GIT_OPTIONAL_LOCKS=1",
		"GIT_DIR=/tmp/other",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("hardened env retained %q: %s", forbidden, joined)
		}
	}
	for _, required := range []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("hardened env missing %q: %s", required, joined)
		}
	}
}

func TestExecGitRunnerDisablesRepositoryFsmonitorHook(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, nil, "init", "-q")
	sentinel := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hook := filepath.Join(t.TempDir(), "fsmonitor.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n: > \""+sentinel+"\"\nprintf '\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, repo, nil, "config", "core.fsmonitor", hook)

	runner := execGitRunner{dir: repo}
	if _, err := runner.Run(context.Background(), "status", "--porcelain=v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fsmonitor hook executed or sentinel check failed: %v", err)
	}
}

func TestExecGitRunnerBoundsCombinedOutput(t *testing.T) {
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf '0123456789abcdef'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := execGitRunner{maxOutput: 8, executable: fakeGit}
	if _, err := runner.Run(context.Background(), "status"); !errors.Is(err, errGitStatsOutputLimit) {
		t.Fatalf("Run error = %v, want errGitStatsOutputLimit", err)
	}
}

func TestRunInUnbornRepository(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, nil, "init", "-q", "--initial-branch=unborn")

	var output bytes.Buffer
	a := app{git: dirGitRunner(repo), out: &output, now: time.Now}
	if err := a.run(context.Background(), []string{"--hotspots", "--time"}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, expected := range []string{
		"브랜치: unborn",
		"총 커밋: 0",
		"첫 커밋: 없음",
		"마지막 커밋: 없음",
		"합계",
		"핫스팟",
		"시간대별 커밋",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("output missing %q:\n%s", expected, text)
		}
	}
}

func TestTempRepositoryStatsAndDeterministicOrder(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, nil, "init", "-q")
	git(t, repo, nil, "config", "user.name", "Fallback")
	git(t, repo, nil, "config", "user.email", "fallback@example.com")

	writeFile(t, filepath.Join(repo, "odd\npath.txt"), "one\ntwo\n")
	git(t, repo, nil, "add", ".")
	commitEnv := []string{
		"GIT_AUTHOR_NAME=홍 길동",
		"GIT_AUTHOR_EMAIL=hong@example.com",
		"GIT_COMMITTER_NAME=홍 길동",
		"GIT_COMMITTER_EMAIL=hong@example.com",
		"GIT_AUTHOR_DATE=2024-01-01T03:04:05+09:00",
		"GIT_COMMITTER_DATE=2024-01-01T03:04:05+09:00",
	}
	git(t, repo, commitEnv, "commit", "-qm", "first")

	writeFile(t, filepath.Join(repo, "alpha.txt"), "alpha\n")
	git(t, repo, nil, "add", ".")
	commitEnv = []string{
		"GIT_AUTHOR_NAME=Alice",
		"GIT_AUTHOR_EMAIL=alice@example.com",
		"GIT_COMMITTER_NAME=Alice",
		"GIT_COMMITTER_EMAIL=alice@example.com",
		"GIT_AUTHOR_DATE=2024-01-02T15:00:00+09:00",
		"GIT_COMMITTER_DATE=2024-01-02T15:00:00+09:00",
	}
	git(t, repo, commitEnv, "commit", "-qm", "second")

	var output bytes.Buffer
	a := app{
		git: dirGitRunner(repo),
		out: &output,
		now: func() time.Time { return time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC) },
	}
	if err := a.run(context.Background(), []string{"--hotspots", "--time"}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, expected := range []string{
		"총 커밋: 2",
		"첫 커밋: 2024-01-01T03:04:05+09:00",
		"마지막 커밋: 2024-01-02T15:00:00+09:00",
		"Alice",
		"홍 길동",
		`"odd\npath.txt"`,
		"03시   1",
		"15시   1",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("output missing %q:\n%s", expected, text)
		}
	}
	if strings.Index(text, "Alice") > strings.Index(text, "홍 길동") {
		t.Errorf("equal-count authors not sorted by name:\n%s", text)
	}
}

func git(t *testing.T, repo string, extraEnv []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(append(os.Environ(), "LC_ALL=C"), extraEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
