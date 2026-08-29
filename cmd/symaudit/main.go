package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const (
	defaultMaxLinks = 100_000
	defaultMaxDepth = 64
	symlinkHopLimit = 64
)

const (
	statusOKRelative = "ok-relative"
	statusOKAbsolute = "ok-absolute"
	statusDangling   = "dangling"
	statusLoop       = "loop"
	statusHopLimit   = "hop-limit"
	statusOutside    = "outside-root"
)

var errLinkLimit = errors.New("symlink limit reached")

// linkReport is deliberately metadata-only. symaudit never opens or reads a
// link target; it only inspects directory entries and link metadata.
type linkReport struct {
	Path           string `json:"path"`
	Target         string `json:"target"`
	Status         string `json:"status"`
	Classification string `json:"classification"`
	Hops           int    `json:"hops"`
	Risk           bool   `json:"risk"`
	Reason         string `json:"reason,omitempty"`
}

type auditReport struct {
	Root          string       `json:"root"`
	Links         []linkReport `json:"links"`
	LinkCount     int          `json:"link_count"`
	RiskyCount    int          `json:"risky_count"`
	FindingCount  int          `json:"finding_count"`
	Partial       bool         `json:"partial"`
	PartialReason []string     `json:"partial_reasons,omitempty"`
}

type resolveResult struct {
	status  string
	hops    int
	partial bool
	reason  string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes symaudit without mutating the filesystem.
//
// Exit codes:
//
//	0 no risky findings
//	1 risky findings (absolute targets are risky by default)
//	2 usage or fatal root error
//	3 bounded or otherwise partial audit
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("symaudit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write JSON output")
	maxLinks := flags.Int("max-links", defaultMaxLinks, "maximum links to inspect")
	maxDepth := flags.Int("max-depth", defaultMaxDepth, "maximum directory depth (0 = root only)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: symaudit [--json] [--max-links N] [--max-depth N] [root]")
	}

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *maxLinks < 1 {
		fmt.Fprintf(stderr, "symaudit: --max-links must be at least 1\n")
		flags.Usage()
		return 2
	}
	if *maxDepth < 0 {
		fmt.Fprintf(stderr, "symaudit: --max-depth must be non-negative\n")
		flags.Usage()
		return 2
	}
	if flags.NArg() > 1 {
		fmt.Fprintln(stderr, "symaudit: at most one root path is allowed")
		flags.Usage()
		return 2
	}

	rootArg := "."
	if flags.NArg() == 1 {
		rootArg = flags.Arg(0)
	}
	root, err := canonicalRoot(rootArg)
	if err != nil {
		fmt.Fprintf(stderr, "symaudit: cannot use root %q: %v\n", rootArg, err)
		return 2
	}

	report := audit(root, *maxLinks, *maxDepth)
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(stderr, "symaudit: cannot write JSON report: %v\n", err)
			return 3
		}
	} else {
		writeHuman(stdout, report)
	}

	if report.Partial {
		return 3
	}
	if report.RiskyCount > 0 {
		return 1
	}
	return 0
}

func canonicalRoot(raw string) (string, error) {
	raw = expandHome(raw)
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("root is not a directory")
	}
	return filepath.Clean(canonical), nil
}

func audit(root string, maxLinks, maxDepth int) auditReport {
	report := auditReport{
		Root:  redactPath(root),
		Links: make([]linkReport, 0),
	}
	partialReasons := make(map[string]struct{})
	markPartial := func(reason string) {
		report.Partial = true
		if reason != "" {
			partialReasons[reason] = struct{}{}
		}
	}

	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			markPartial("unreadable-entry")
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			markPartial("relative-path")
			return nil
		}
		depth := pathDepth(rel)
		if depth > maxDepth {
			markPartial("max-depth")
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		// WalkDir does not follow symlink directories. Lstat is still used to
		// make the symlink decision explicit and avoid following a stale entry.
		info, err := os.Lstat(path)
		if err != nil {
			markPartial("unreadable-entry")
			return nil
		}
		if info.Mode()&os.ModeSymlink == 0 {
			if path != root && info.IsDir() && depth >= maxDepth {
				// Do not descend into directories at the bound. This keeps the
				// bound strict and avoids inspecting one extra level.
				markPartial("max-depth")
				return fs.SkipDir
			}
			return nil
		}

		if maxLinks > 0 && len(report.Links) >= maxLinks {
			markPartial("max-links")
			return errLinkLimit
		}
		rawTarget, err := os.Readlink(path)
		if err != nil {
			markPartial("unreadable-link")
			return nil
		}
		resolved := resolveLink(path, rawTarget, root)
		if resolved.partial {
			markPartial(resolved.reason)
		}
		finding := linkReport{
			Path:           redactPath(filepath.Clean(path)),
			Target:         redactTarget(rawTarget),
			Status:         resolved.status,
			Classification: resolved.status,
			Hops:           resolved.hops,
			Risk:           isRisky(resolved.status),
			Reason:         resolved.reason,
		}
		report.Links = append(report.Links, finding)
		if finding.Risk {
			report.RiskyCount++
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errLinkLimit) {
		markPartial("walk-error")
	}

	sort.Slice(report.Links, func(i, j int) bool {
		return report.Links[i].Path < report.Links[j].Path
	})
	report.LinkCount = len(report.Links)
	report.FindingCount = report.RiskyCount
	for reason := range partialReasons {
		report.PartialReason = append(report.PartialReason, reason)
	}
	sort.Strings(report.PartialReason)
	return report
}

func resolveLink(linkPath, rawTarget, root string) resolveResult {
	current := filepath.Clean(linkPath)
	absoluteTarget := filepath.IsAbs(rawTarget)
	visited := make(map[string]struct{}, symlinkHopLimit)

	for hop := 0; ; hop++ {
		parent, err := filepath.EvalSymlinks(filepath.Dir(current))
		if err != nil {
			return classifyResolveError(err, hop)
		}
		parent = filepath.Clean(parent)
		if !withinRoot(root, parent) {
			return resolveResult{status: statusOutside, hops: hop, reason: "canonical-parent-outside-root"}
		}

		// Parent is canonical, so aliases through symlink directories share
		// one visited key. This catches loops without traversing directories.
		actual := filepath.Clean(filepath.Join(parent, filepath.Base(current)))
		if !withinRoot(root, actual) {
			return resolveResult{status: statusOutside, hops: hop, reason: "canonical-path-outside-root"}
		}
		if _, seen := visited[actual]; seen {
			return resolveResult{status: statusLoop, hops: hop, reason: "visited-link"}
		}
		visited[actual] = struct{}{}

		info, err := os.Lstat(current)
		if err != nil {
			return classifyResolveError(err, hop)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			// parent canonicalization above proves this final object is under
			// root, even when current used an alias in an intermediate path.
			status := statusOKRelative
			if absoluteTarget {
				status = statusOKAbsolute
			}
			return resolveResult{status: status, hops: hop, reason: ""}
		}
		if hop >= symlinkHopLimit {
			return resolveResult{
				status:  statusHopLimit,
				hops:    hop,
				partial: true,
				reason:  "symlink-hop-limit",
			}
		}

		target, err := os.Readlink(current)
		if err != nil {
			return classifyResolveError(err, hop)
		}
		next := target
		if !filepath.IsAbs(target) {
			next = filepath.Join(parent, target)
		}
		next = filepath.Clean(next)
		if !withinRootWithAliases(root, next) {
			return resolveResult{status: statusOutside, hops: hop + 1, reason: "lexical-target-outside-root"}
		}
		current = next
	}

}

func classifyResolveError(err error, hops int) resolveResult {
	if errors.Is(err, syscall.ELOOP) {
		return resolveResult{status: statusLoop, hops: hops, reason: "filesystem-loop"}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return resolveResult{status: statusDangling, hops: hops, reason: "target-missing"}
	}
	return resolveResult{
		status:  statusDangling,
		hops:    hops,
		partial: true,
		reason:  "target-inspection-error",
	}
}

func withinRoot(root, candidate string) bool {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// withinRootWithAliases handles platform aliases such as macOS /var ->
// /private/var. A lexical check remains the fast path; EvalSymlinks is used
// only when that check rejects a candidate, and only for the candidate path
// (or its parent when the final target is dangling).
func withinRootWithAliases(root, candidate string) bool {
	if withinRoot(root, candidate) {
		return true
	}
	canonical, err := filepath.EvalSymlinks(candidate)
	if err == nil {
		return withinRoot(root, canonical)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(candidate))
	if err != nil {
		return false
	}
	return withinRoot(root, filepath.Join(parent, filepath.Base(candidate)))
}

func pathDepth(rel string) int {
	if rel == "." || rel == "" {
		return 0
	}
	return 1 + strings.Count(rel, string(filepath.Separator))
}

func isRisky(status string) bool {
	return status != statusOKRelative
}

func writeHuman(out io.Writer, report auditReport) {
	fmt.Fprintf(out, "root: %s\n", strconv.Quote(report.Root))
	for _, link := range report.Links {
		marker := "OK"
		if link.Risk {
			marker = "RISK"
		}
		fmt.Fprintf(out, "[%s] %s -> %s (%s)\n", marker, strconv.Quote(link.Path), strconv.Quote(link.Target), link.Status)
	}
	fmt.Fprintf(out, "links: %d  risky: %d\n", report.LinkCount, report.RiskyCount)
	if report.Partial {
		fmt.Fprintf(out, "partial: %s\n", strings.Join(report.PartialReason, ", "))
	}
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"+string(filepath.Separator)))
}

func redactTarget(target string) string {
	if filepath.IsAbs(target) {
		return redactPath(target)
	}
	return target
}

func redactPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !filepath.IsAbs(path) {
		return path
	}
	home, _ = filepath.Abs(home)
	path, _ = filepath.Abs(path)
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	if rel == "." {
		return "$HOME"
	}
	return filepath.Join("$HOME", rel)
}
