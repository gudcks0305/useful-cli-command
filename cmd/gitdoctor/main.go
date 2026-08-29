package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout  = 10 * time.Second
	maxTimeout      = 5 * time.Minute
	maxOutputBytes  = 16 << 20
	maxLockFiles    = 1024
	maxRepositories = 256
)

var errOutputLimit = errors.New("command output exceeded 16 MiB limit")

type commandRunner interface {
	Run(context.Context, string, ...string) ([]byte, []byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, []byte, error) {
	hardenedArgs := []string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + os.DevNull,
	}
	hardenedArgs = append(hardenedArgs, args...)
	cmd := exec.CommandContext(ctx, "git", hardenedArgs...)
	cmd.Dir = dir
	cmd.Env = hardenedGitEnv(os.Environ())
	var stdout, stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if stdout.truncated || stderr.truncated {
		return stdout.Bytes(), stderr.Bytes(), errOutputLimit
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func hardenedGitEnv(env []string) []string {
	blocked := map[string]bool{
		"GIT_CONFIG":                       true,
		"GIT_CONFIG_COUNT":                 true,
		"GIT_CONFIG_GLOBAL":                true,
		"GIT_CONFIG_NOSYSTEM":              true,
		"GIT_CONFIG_PARAMETERS":            true,
		"GIT_CONFIG_SYSTEM":                true,
		"GIT_DIR":                          true,
		"GIT_WORK_TREE":                    true,
		"GIT_COMMON_DIR":                   true,
		"GIT_INDEX_FILE":                   true,
		"GIT_OBJECT_DIRECTORY":             true,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_OPTIONAL_LOCKS":               true,
	}
	result := make([]string, 0, len(env)+4)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if blocked[key] || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		result = append(result, entry)
	}
	return append(result,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_OPTIONAL_LOCKS=0",
	)
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxOutputBytes - b.buffer.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = b.buffer.Write(p[:remaining])
	}
	if remaining < len(p) {
		b.truncated = true
	}
	return n, nil
}

func (b *limitedBuffer) Bytes() []byte {
	return b.buffer.Bytes()
}

type finding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type branchResult struct {
	Name          string `json:"name"`
	Detached      bool   `json:"detached"`
	Upstream      string `json:"upstream,omitempty"`
	UpstreamState string `json:"upstream_state"`
	Ahead         int    `json:"ahead"`
	Behind        int    `json:"behind"`
}

type changeResult struct {
	Staged    int `json:"staged"`
	Unstaged  int `json:"unstaged"`
	Untracked int `json:"untracked"`
	Conflicts int `json:"conflicts"`
}

type worktreeDetail struct {
	Path     string `json:"path"`
	Branch   string `json:"branch,omitempty"`
	Detached bool   `json:"detached"`
	Prunable bool   `json:"prunable"`
	Locked   bool   `json:"locked"`
}

type worktreeResult struct {
	Total     int              `json:"total"`
	Stale     int              `json:"stale"`
	Prunable  int              `json:"prunable"`
	Locked    int              `json:"locked"`
	Worktrees []worktreeDetail `json:"items"`
}

type fsckResult struct {
	Requested bool   `json:"requested"`
	Passed    *bool  `json:"passed,omitempty"`
	Status    string `json:"status"`
}

type result struct {
	Version    int            `json:"version"`
	Path       string         `json:"path"`
	Repository bool           `json:"repository"`
	Clean      bool           `json:"clean"`
	Partial    bool           `json:"partial"`
	Branch     branchResult   `json:"branch"`
	Changes    changeResult   `json:"changes"`
	Worktrees  worktreeResult `json:"worktrees"`
	Locks      []string       `json:"locks"`
	Fsck       fsckResult     `json:"fsck"`
	Findings   []finding      `json:"findings"`
	Errors     []string       `json:"errors"`
}

type fatalResult struct {
	Version int `json:"version"`
	Error   struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithRunner(args, stdout, stderr, execRunner{})
}

func runWithRunner(args []string, stdout, stderr io.Writer, runner commandRunner) int {
	flags := flag.NewFlagSet("gitdoctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write deterministic JSON output")
	checkFsck := flags.Bool("fsck", false, "run bounded, read-only git fsck")
	timeout := flags.Duration("timeout", defaultTimeout, "overall timeout (for example 30s or 2m)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 1 {
		fmt.Fprintln(stderr, "gitdoctor: expected at most one path")
		return 2
	}
	if *timeout <= 0 || *timeout > maxTimeout {
		fmt.Fprintf(stderr, "gitdoctor: --timeout must be greater than 0 and at most %s\n", maxTimeout)
		return 2
	}

	path := "."
	if flags.NArg() == 1 {
		path = flags.Arg(0)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		writeFatal(stdout, stderr, *jsonOutput, "invalid_path", "cannot resolve path")
		return 2
	}
	info, err := os.Stat(absPath)
	if err != nil || !info.IsDir() {
		writeFatal(stdout, stderr, *jsonOutput, "invalid_path", "path must be an existing directory")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if _, _, err := runner.Run(ctx, absPath, "--version"); err != nil {
		code := "git_unavailable"
		message := "git executable is unavailable"
		exitCode := 2
		if ctx.Err() != nil {
			code, message = "timeout", "git version check timed out"
			exitCode = 3
		} else if errors.Is(err, errOutputLimit) {
			code, message = "output_limit", "git version output exceeded safety limit"
			exitCode = 3
		}
		writeFatal(stdout, stderr, *jsonOutput, code, message)
		return exitCode
	}
	inside, _, err := runner.Run(ctx, absPath, "rev-parse", "--is-inside-work-tree")
	if err != nil && ctx.Err() != nil {
		writeFatal(stdout, stderr, *jsonOutput, "timeout", "repository check timed out")
		return 3
	}
	if errors.Is(err, errOutputLimit) {
		writeFatal(stdout, stderr, *jsonOutput, "output_limit", "repository check output exceeded safety limit")
		return 3
	}
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		writeFatal(stdout, stderr, *jsonOutput, "not_repository", "path is not inside a Git worktree")
		return 2
	}

	report := diagnose(ctx, absPath, *checkFsck, runner)
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintln(stderr, "gitdoctor: cannot write JSON output")
			return 2
		}
	} else {
		writeHuman(stdout, report)
	}
	if report.Partial {
		return 3
	}
	if len(report.Findings) > 0 {
		return 1
	}
	return 0
}

func diagnose(ctx context.Context, path string, checkFsck bool, runner commandRunner) result {
	report := result{
		Version:    1,
		Path:       path,
		Repository: true,
		Locks:      []string{},
		Findings:   []finding{},
		Errors:     []string{},
		Worktrees:  worktreeResult{Worktrees: []worktreeDetail{}},
		Fsck:       fsckResult{Requested: checkFsck, Status: "skipped"},
	}

	if root, _, err := runner.Run(ctx, path, "rev-parse", "--show-toplevel"); err == nil {
		report.Path = strings.TrimSpace(string(root))
	} else {
		addPartial(&report, ctx, "repo_root_unavailable")
	}

	unsafeFilters, configErr := inspectExternalFilters(ctx, path, runner)
	if configErr != nil {
		addPartial(&report, ctx, "filter_config_scan_incomplete")
	} else if len(unsafeFilters) > 0 {
		addPartial(&report, ctx, "unsafe_filter_config")
		for _, key := range unsafeFilters {
			report.Findings = append(report.Findings, finding{
				"unsafe_filter_config",
				"status skipped because external filter is configured: " + key,
			})
		}
	} else {
		status, _, err := runner.Run(ctx, path, "status", "--porcelain=v2", "--branch", "-z")
		if err != nil {
			addPartial(&report, ctx, "status_unavailable")
		} else {
			parseStatus(status, &report)
			classifyUpstream(ctx, path, runner, &report)
			addStatusFindings(&report)
		}
	}

	worktrees, _, err := runner.Run(ctx, path, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		addPartial(&report, ctx, "worktree_list_unavailable")
	} else {
		report.Worktrees = parseWorktrees(worktrees)
		addWorktreeFindings(&report)
	}

	commonDir, _, err := runner.Run(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		addPartial(&report, ctx, "common_dir_unavailable")
	} else {
		locks, scanErr := findLocks(strings.TrimSpace(string(commonDir)))
		report.Locks = locks
		if scanErr != nil {
			addPartial(&report, ctx, "lock_scan_incomplete")
		}
		for _, lock := range locks {
			report.Findings = append(report.Findings, finding{"lock_file", "leftover lock file: " + lock})
		}
	}

	if checkFsck {
		report.Fsck.Status = "passed"
		passed := true
		report.Fsck.Passed = &passed
		_, _, err := runner.Run(ctx, path, "fsck", "--no-progress", "--no-dangling")
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, errOutputLimit) {
				report.Fsck.Status = "incomplete"
				report.Fsck.Passed = nil
				addPartial(&report, ctx, "fsck_incomplete")
			} else {
				passed = false
				report.Fsck.Passed = &passed
				report.Fsck.Status = "failed"
				report.Findings = append(report.Findings, finding{"fsck_failed", "git fsck reported repository integrity problems"})
			}
		}
	}

	sort.Slice(report.Findings, func(i, j int) bool {
		if report.Findings[i].Code == report.Findings[j].Code {
			return report.Findings[i].Message < report.Findings[j].Message
		}
		return report.Findings[i].Code < report.Findings[j].Code
	})
	sort.Strings(report.Errors)
	report.Clean = !report.Partial && len(report.Findings) == 0
	return report
}

func inspectExternalFilters(ctx context.Context, root string, runner commandRunner) ([]string, error) {
	seen := map[string]bool{}
	unsafe := []string{}
	repositories := 0
	var inspect func(string, string) error
	inspect = func(dir, label string) error {
		canonical, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return err
		}
		if seen[canonical] {
			return nil
		}
		seen[canonical] = true
		repositories++
		if repositories > maxRepositories {
			return errors.New("repository scan limit exceeded")
		}

		configNames, _, err := runner.Run(ctx, dir, "config", "--name-only", "--list", "-z")
		if err != nil {
			return err
		}
		for _, key := range externalFilterKeys(configNames) {
			if label == "" {
				unsafe = append(unsafe, key)
			} else {
				unsafe = append(unsafe, "submodule "+strconv.Quote(label)+": "+key)
			}
		}

		index, _, err := runner.Run(ctx, dir, "ls-files", "--stage", "-z")
		if err != nil {
			return err
		}
		for _, subPath := range gitlinkPaths(index) {
			child := filepath.Join(dir, filepath.FromSlash(subPath))
			if _, err := os.Lstat(filepath.Join(child, ".git")); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			inside, _, err := runner.Run(ctx, child, "rev-parse", "--is-inside-work-tree")
			if err != nil || strings.TrimSpace(string(inside)) != "true" {
				if err != nil {
					return err
				}
				return errors.New("submodule is not a worktree")
			}
			childLabel := subPath
			if label != "" {
				childLabel = label + "/" + subPath
			}
			if err := inspect(child, childLabel); err != nil {
				return err
			}
		}
		return nil
	}
	if err := inspect(root, ""); err != nil {
		return nil, err
	}
	sort.Strings(unsafe)
	return unsafe, nil
}

func gitlinkPaths(data []byte) []string {
	paths := []string{}
	seen := map[string]bool{}
	for _, raw := range bytes.Split(data, []byte{0}) {
		record := string(raw)
		tab := strings.IndexByte(record, '\t')
		if tab < 0 || !strings.HasPrefix(record, "160000 ") {
			continue
		}
		path := record[tab+1:]
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func externalFilterKeys(data []byte) []string {
	keys := []string{}
	seen := map[string]bool{}
	for _, raw := range bytes.Split(data, []byte{0}) {
		key := strings.ToLower(string(raw))
		if !strings.HasPrefix(key, "filter.") ||
			(!strings.HasSuffix(key, ".clean") && !strings.HasSuffix(key, ".process")) || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func parseStatus(data []byte, report *result) {
	for _, raw := range bytes.Split(data, []byte{0}) {
		record := string(raw)
		switch {
		case strings.HasPrefix(record, "# branch.head "):
			report.Branch.Name = strings.TrimPrefix(record, "# branch.head ")
			report.Branch.Detached = report.Branch.Name == "(detached)"
			if report.Branch.Detached {
				report.Branch.Name = ""
			}
		case strings.HasPrefix(record, "# branch.upstream "):
			report.Branch.Upstream = strings.TrimPrefix(record, "# branch.upstream ")
		case strings.HasPrefix(record, "# branch.ab "):
			fields := strings.Fields(strings.TrimPrefix(record, "# branch.ab "))
			if len(fields) == 2 {
				report.Branch.Ahead, _ = strconv.Atoi(strings.TrimPrefix(fields[0], "+"))
				report.Branch.Behind, _ = strconv.Atoi(strings.TrimPrefix(fields[1], "-"))
			}
		case strings.HasPrefix(record, "1 "), strings.HasPrefix(record, "2 "):
			fields := strings.SplitN(record, " ", 3)
			if len(fields) >= 2 {
				countXY(fields[1], &report.Changes)
			}
		case strings.HasPrefix(record, "u "):
			report.Changes.Conflicts++
			report.Changes.Staged++
			report.Changes.Unstaged++
		case strings.HasPrefix(record, "? "):
			report.Changes.Untracked++
		}
	}
}

func countXY(xy string, changes *changeResult) {
	if len(xy) != 2 {
		return
	}
	if xy[0] != '.' {
		changes.Staged++
	}
	if xy[1] != '.' {
		changes.Unstaged++
	}
}

func classifyUpstream(ctx context.Context, path string, runner commandRunner, report *result) {
	if report.Branch.Detached {
		report.Branch.UpstreamState = "not_applicable"
		return
	}
	if report.Branch.Upstream != "" {
		_, _, err := runner.Run(ctx, path, "rev-parse", "--verify", "--quiet", "@{upstream}")
		if err == nil {
			report.Branch.UpstreamState = "set"
			return
		}
		if ctx.Err() != nil || errors.Is(err, errOutputLimit) {
			report.Branch.UpstreamState = "unknown"
			addPartial(report, ctx, "upstream_state_unavailable")
			return
		}
		report.Branch.UpstreamState = "gone"
		return
	}
	if report.Branch.Name == "" {
		report.Branch.UpstreamState = "unknown"
		return
	}
	remote, _, err := runner.Run(ctx, path, "config", "--get", "branch."+report.Branch.Name+".remote")
	if err == nil && strings.TrimSpace(string(remote)) != "" {
		report.Branch.UpstreamState = "gone"
		return
	}
	if ctx.Err() != nil || errors.Is(err, errOutputLimit) {
		report.Branch.UpstreamState = "unknown"
		addPartial(report, ctx, "upstream_state_unavailable")
		return
	}
	report.Branch.UpstreamState = "missing"
}

func addStatusFindings(report *result) {
	switch {
	case report.Branch.Detached:
		report.Findings = append(report.Findings, finding{"detached_head", "HEAD is detached"})
	case report.Branch.UpstreamState == "gone":
		report.Findings = append(report.Findings, finding{"upstream_gone", "configured upstream reference is gone"})
	case report.Branch.UpstreamState == "missing":
		report.Findings = append(report.Findings, finding{"upstream_missing", "branch has no configured upstream"})
	}
	if report.Branch.Ahead > 0 {
		report.Findings = append(report.Findings, finding{"ahead", fmt.Sprintf("branch is ahead by %d commit(s)", report.Branch.Ahead)})
	}
	if report.Branch.Behind > 0 {
		report.Findings = append(report.Findings, finding{"behind", fmt.Sprintf("branch is behind by %d commit(s)", report.Branch.Behind)})
	}
	if report.Changes.Staged > 0 {
		report.Findings = append(report.Findings, finding{"staged_changes", fmt.Sprintf("%d staged path(s)", report.Changes.Staged)})
	}
	if report.Changes.Unstaged > 0 {
		report.Findings = append(report.Findings, finding{"unstaged_changes", fmt.Sprintf("%d unstaged path(s)", report.Changes.Unstaged)})
	}
	if report.Changes.Untracked > 0 {
		report.Findings = append(report.Findings, finding{"untracked_files", fmt.Sprintf("%d untracked path(s)", report.Changes.Untracked)})
	}
	if report.Changes.Conflicts > 0 {
		report.Findings = append(report.Findings, finding{"conflicts", fmt.Sprintf("%d conflicted path(s)", report.Changes.Conflicts)})
	}
}

func parseWorktrees(data []byte) worktreeResult {
	result := worktreeResult{Worktrees: []worktreeDetail{}}
	var current *worktreeDetail
	flush := func() {
		if current == nil {
			return
		}
		result.Worktrees = append(result.Worktrees, *current)
		current = nil
	}
	for _, raw := range bytes.Split(data, []byte{0}) {
		record := string(raw)
		if record == "" {
			flush()
			continue
		}
		key, value, _ := strings.Cut(record, " ")
		if key == "worktree" {
			flush()
			current = &worktreeDetail{Path: value}
			continue
		}
		if current == nil {
			continue
		}
		switch key {
		case "branch":
			current.Branch = strings.TrimPrefix(value, "refs/heads/")
		case "detached":
			current.Detached = true
		case "prunable":
			current.Prunable = true
		case "locked":
			current.Locked = true
		}
	}
	flush()
	sort.Slice(result.Worktrees, func(i, j int) bool { return result.Worktrees[i].Path < result.Worktrees[j].Path })
	result.Total = len(result.Worktrees)
	for _, wt := range result.Worktrees {
		if wt.Prunable {
			result.Prunable++
			result.Stale++
		}
		if wt.Locked {
			result.Locked++
		}
	}
	return result
}

func addWorktreeFindings(report *result) {
	for _, wt := range report.Worktrees.Worktrees {
		if wt.Prunable {
			report.Findings = append(report.Findings, finding{"worktree_prunable", "stale/prunable worktree: " + strconv.Quote(wt.Path)})
		}
		if wt.Locked {
			report.Findings = append(report.Findings, finding{"worktree_locked", "locked worktree: " + strconv.Quote(wt.Path)})
		}
	}
}

func findLocks(commonDir string) ([]string, error) {
	locks := []string{}
	seen := map[string]bool{}
	add := func(path string) {
		rel, err := filepath.Rel(commonDir, path)
		if err == nil && !seen[rel] {
			seen[rel] = true
			locks = append(locks, filepath.ToSlash(rel))
		}
	}
	for _, name := range []string{"HEAD.lock", "config.lock", "index.lock", "packed-refs.lock", "shallow.lock"} {
		path := filepath.Join(commonDir, name)
		if info, err := os.Lstat(path); err == nil && !info.IsDir() {
			add(path)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return locks, err
		}
	}
	err := filepath.WalkDir(filepath.Join(commonDir, "refs"), func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".lock") {
			add(path)
			if len(locks) >= maxLockFiles {
				return errOutputLimit
			}
		}
		return nil
	})
	sort.Strings(locks)
	return locks, err
}

func addPartial(report *result, ctx context.Context, code string) {
	report.Partial = true
	if ctx.Err() != nil {
		code = "timeout"
	}
	for _, existing := range report.Errors {
		if existing == code {
			return
		}
	}
	report.Errors = append(report.Errors, code)
}

func writeHuman(w io.Writer, report result) {
	fmt.Fprintf(w, "gitdoctor: %q\n", report.Path)
	branch := report.Branch.Name
	if report.Branch.Detached {
		branch = "(detached)"
	}
	fmt.Fprintf(w, "branch: %s\n", branch)
	fmt.Fprintf(w, "upstream: %s", report.Branch.UpstreamState)
	if report.Branch.Upstream != "" {
		fmt.Fprintf(w, " (%s)", report.Branch.Upstream)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "ahead/behind: %d/%d\n", report.Branch.Ahead, report.Branch.Behind)
	fmt.Fprintf(w, "changes: staged=%d unstaged=%d untracked=%d conflicts=%d\n",
		report.Changes.Staged, report.Changes.Unstaged, report.Changes.Untracked, report.Changes.Conflicts)
	fmt.Fprintf(w, "worktrees: total=%d stale/prunable=%d locked=%d\n",
		report.Worktrees.Total, report.Worktrees.Prunable, report.Worktrees.Locked)
	fmt.Fprintf(w, "locks: %d\n", len(report.Locks))
	fmt.Fprintf(w, "fsck: %s\n", report.Fsck.Status)
	if len(report.Findings) == 0 {
		fmt.Fprintln(w, "findings: none")
	} else {
		fmt.Fprintln(w, "findings:")
		for _, item := range report.Findings {
			fmt.Fprintf(w, "- %s: %s\n", item.Code, item.Message)
		}
	}
	if report.Partial {
		fmt.Fprintf(w, "status: partial (%s)\n", strings.Join(report.Errors, ", "))
	} else if report.Clean {
		fmt.Fprintln(w, "status: clean")
	} else {
		fmt.Fprintln(w, "status: findings")
	}
}

func writeFatal(stdout, stderr io.Writer, jsonOutput bool, code, message string) {
	if !jsonOutput {
		fmt.Fprintf(stderr, "gitdoctor: %s\n", message)
		return
	}
	result := fatalResult{Version: 1}
	result.Error.Code = code
	result.Error.Message = message
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(result)
}
