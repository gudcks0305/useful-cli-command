package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultMaxPlists = 512
	convertTimeout   = 5 * time.Second
	maxAncestorDepth = 64
)

type finding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type entryReport struct {
	Source              string    `json:"source"`
	Label               string    `json:"label,omitempty"`
	Executable          string    `json:"executable,omitempty"`
	DisabledDeclaration *bool     `json:"disabled_declaration,omitempty"`
	Findings            []finding `json:"findings"`
	rawLabel            string
}

type auditReport struct {
	Scope        string        `json:"scope"`
	Scanned      int           `json:"scanned"`
	FindingCount int           `json:"finding_count"`
	Partial      bool          `json:"partial"`
	Entries      []entryReport `json:"entries"`
}

type plistConverter func(context.Context, string) ([]byte, error)

type auditConfig struct {
	convert       plistConverter
	maxFiles      int
	ignoreMissing bool
	rootDaemon    func(string) bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if !platformSupported() {
		fmt.Fprintln(stderr, "launchaudit: unsupported platform; macOS is required")
		return 2
	}

	flags := flag.NewFlagSet("launchaudit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write JSON output")
	scope := flags.String("scope", "user", "audit scope: user, local, or all")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: launchaudit [--json] [--scope user|local|all] [path ...]")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *scope != "user" && *scope != "local" && *scope != "all" {
		fmt.Fprintf(stderr, "launchaudit: invalid scope %q\n", *scope)
		flags.Usage()
		return 2
	}

	paths := flags.Args()
	usingDefaultPaths := len(paths) == 0
	if len(paths) == 0 {
		var err error
		paths, err = scopePaths(*scope)
		if err != nil {
			fmt.Fprintln(stderr, "launchaudit: cannot determine audit paths")
			return 3
		}
	}

	report := audit(paths, *scope, auditConfig{
		convert: convertWithPlutil, maxFiles: defaultMaxPlists, ignoreMissing: usingDefaultPaths,
	})
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintln(stderr, "launchaudit: cannot write JSON report")
			return 3
		}
	} else {
		writeHuman(stdout, report)
	}

	if report.Partial {
		return 3
	}
	if report.FindingCount > 0 {
		return 1
	}
	return 0
}

func audit(paths []string, scope string, cfg auditConfig) auditReport {
	if cfg.convert == nil {
		cfg.convert = convertWithPlutil
	}
	if cfg.maxFiles <= 0 {
		cfg.maxFiles = defaultMaxPlists
	}
	if cfg.rootDaemon == nil {
		cfg.rootDaemon = isRootDaemonPlist
	}

	report := auditReport{Scope: scope, Entries: make([]entryReport, 0)}
	files, discoveryEntries, partial := discoverPlists(paths, cfg.maxFiles, cfg.ignoreMissing)
	report.Entries = append(report.Entries, discoveryEntries...)
	report.Partial = partial

	labels := make(map[string][]int)
	for _, path := range files {
		entry, entryPartial := auditPlist(path, cfg.convert, cfg.rootDaemon(path))
		idx := len(report.Entries)
		report.Entries = append(report.Entries, entry)
		report.Scanned++
		if entryPartial {
			report.Partial = true
		}
		if entry.rawLabel != "" {
			labels[entry.rawLabel] = append(labels[entry.rawLabel], idx)
		}
	}

	for _, indexes := range labels {
		if len(indexes) < 2 {
			continue
		}
		for _, idx := range indexes {
			report.Entries[idx].Findings = append(report.Entries[idx].Findings, finding{
				Code: "duplicate_label", Message: "label is declared by more than one plist",
			})
		}
	}

	sort.Slice(report.Entries, func(i, j int) bool { return report.Entries[i].Source < report.Entries[j].Source })
	for i := range report.Entries {
		sort.Slice(report.Entries[i].Findings, func(a, b int) bool {
			return report.Entries[i].Findings[a].Code < report.Entries[i].Findings[b].Code
		})
		report.FindingCount += len(report.Entries[i].Findings)
	}
	return report
}

func discoverPlists(paths []string, limit int, ignoreMissing bool) ([]string, []entryReport, bool) {
	files := make([]string, 0)
	entries := make([]entryReport, 0)
	seen := make(map[string]struct{})
	partial := false
	limitHit := false

	add := func(path string) bool {
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			return true
		}
		seen[path] = struct{}{}
		if len(files) >= limit {
			if !limitHit {
				entries = append(entries, entryReport{Source: path, Findings: []finding{{
					Code: "scan_limit_reached", Message: "plist scan limit reached; remaining entries were not inspected",
				}}})
				limitHit = true
			}
			partial = true
			return false
		}
		files = append(files, path)
		return true
	}

	for _, rawPath := range paths {
		path := expandHome(rawPath)
		info, err := os.Lstat(path)
		if err != nil {
			if ignoreMissing && errors.Is(err, os.ErrNotExist) {
				continue
			}
			entries = append(entries, entryReport{Source: filepath.Clean(path), Findings: []finding{{
				Code: "path_unreadable", Message: "audit path could not be inspected",
			}}})
			partial = true
			continue
		}

		if info.IsDir() {
			dirEntries, err := os.ReadDir(path)
			if err != nil {
				entries = append(entries, entryReport{Source: filepath.Clean(path), Findings: []finding{{
					Code: "directory_unreadable", Message: "audit directory could not be read",
				}}})
				partial = true
				continue
			}
			sort.Slice(dirEntries, func(i, j int) bool { return dirEntries[i].Name() < dirEntries[j].Name() })
			for _, dirEntry := range dirEntries {
				if !strings.EqualFold(filepath.Ext(dirEntry.Name()), ".plist") || !dirEntry.Type().IsRegular() {
					continue
				}
				if !add(filepath.Join(path, dirEntry.Name())) {
					break
				}
			}
			continue
		}

		if info.Mode().IsRegular() && strings.EqualFold(filepath.Ext(path), ".plist") {
			add(path)
			continue
		}
		entries = append(entries, entryReport{Source: filepath.Clean(path), Findings: []finding{{
			Code: "not_direct_plist", Message: "path is not a direct regular .plist file or directory",
		}}})
	}
	sort.Strings(files)
	return files, entries, partial
}

func auditPlist(path string, convert plistConverter, rootDaemon bool) (entryReport, bool) {
	entry := entryReport{Source: filepath.Clean(path), Findings: make([]finding, 0)}
	ctx, cancel := context.WithTimeout(context.Background(), convertTimeout)
	defer cancel()
	data, err := convert(ctx, path)
	if err != nil {
		entry.Findings = append(entry.Findings, finding{Code: "plist_unparseable", Message: "plist could not be converted and parsed"})
		return entry, true
	}

	var plist map[string]any
	if err := json.Unmarshal(data, &plist); err != nil || plist == nil {
		entry.Findings = append(entry.Findings, finding{Code: "plist_unparseable", Message: "plist conversion did not produce a JSON dictionary"})
		return entry, true
	}

	label, ok := nonEmptyString(plist["Label"])
	if !ok {
		entry.Findings = append(entry.Findings, finding{Code: "missing_label", Message: "required Label is missing or invalid"})
	} else {
		entry.Label = safeLabel(label)
		entry.rawLabel = label
	}

	if disabled, ok := plist["Disabled"].(bool); ok {
		entry.DisabledDeclaration = &disabled
	} else if _, exists := plist["Disabled"]; exists {
		entry.Findings = append(entry.Findings, finding{Code: "invalid_disabled", Message: "Disabled declaration is not a boolean"})
	}

	executable, found := declaredExecutable(plist)
	if !found {
		entry.Findings = append(entry.Findings, finding{Code: "missing_executable", Message: "Program or ProgramArguments[0] is missing or invalid"})
	} else {
		entry.Executable = safeExecutable(executable)
		if !filepath.IsAbs(executable) {
			entry.Findings = append(entry.Findings, finding{Code: "relative_executable", Message: "relative executable path is ambiguous and was not resolved"})
		} else {
			entry.Findings = append(entry.Findings, auditExecutable(executable, rootDaemon)...)
		}
	}

	if info, err := os.Lstat(path); err != nil {
		entry.Findings = append(entry.Findings, finding{Code: "plist_metadata_unreadable", Message: "plist ownership and permissions could not be inspected"})
		return entry, true
	} else {
		if info.Mode().Perm()&0022 != 0 {
			entry.Findings = append(entry.Findings, finding{Code: "plist_writable_by_others", Message: "plist is writable by group or others"})
		}
		if expected, check := expectedOwner(path); check {
			if actual, ok := fileOwnerUID(info); ok && actual != expected {
				entry.Findings = append(entry.Findings, finding{Code: "unexpected_owner", Message: "plist owner does not match its launchd scope"})
			}
		}
	}
	return entry, false
}

func auditExecutable(executable string, rootDaemon bool) []finding {
	findings := make([]finding, 0)
	declaredInfo, err := os.Lstat(executable)
	if err != nil {
		return append(findings, finding{Code: "executable_missing", Message: "declared executable does not exist or cannot be inspected"})
	}
	if declaredInfo.Mode()&os.ModeSymlink != 0 {
		findings = append(findings, finding{Code: "executable_symlink", Message: "declared executable is a symbolic link; resolved target was inspected"})
	}

	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return append(findings, finding{Code: "executable_target_unreadable", Message: "executable symlink target could not be resolved and inspected"})
	}
	resolved = filepath.Clean(resolved)
	targetInfo, err := os.Lstat(resolved)
	if err != nil {
		return append(findings, finding{Code: "executable_target_unreadable", Message: "resolved executable target could not be inspected"})
	}
	if !targetInfo.Mode().IsRegular() {
		findings = append(findings, finding{Code: "executable_not_regular", Message: "resolved executable is not a regular file"})
	}
	if targetInfo.Mode().Perm()&0111 == 0 {
		findings = append(findings, finding{Code: "executable_not_executable", Message: "resolved executable has no executable permission bit"})
	}
	if targetInfo.Mode().Perm()&0002 != 0 {
		findings = append(findings, finding{Code: "executable_world_writable", Message: "resolved executable is writable by others"})
	}
	if rootDaemon {
		if targetInfo.Mode().Perm()&0022 != 0 {
			findings = appendOnce(findings, finding{Code: "executable_writable_by_group_or_others", Message: "root-scope executable is writable by group or others"})
		}
		if uid, ok := fileOwnerUID(targetInfo); !ok || uid != 0 {
			findings = append(findings, finding{Code: "executable_not_root_owned", Message: "root-scope executable is not owned by root"})
		}
	}

	findings = auditAncestorChain(filepath.Dir(executable), rootDaemon, findings)
	if filepath.Dir(resolved) != filepath.Dir(executable) {
		findings = auditAncestorChain(filepath.Dir(resolved), rootDaemon, findings)
	}
	return findings
}

func auditAncestorChain(ancestor string, rootDaemon bool, findings []finding) []finding {
	for depth := 0; ; depth++ {
		if depth >= maxAncestorDepth {
			findings = append(findings, finding{Code: "ancestor_walk_limit", Message: "executable ancestor inspection reached its safety limit"})
			break
		}
		info, err := os.Lstat(ancestor)
		if err != nil {
			findings = append(findings, finding{Code: "ancestor_unreadable", Message: "an executable ancestor could not be inspected"})
			break
		}
		isSymlink := info.Mode()&os.ModeSymlink != 0
		// Symlink permission bits are not access controls on Darwin. Its parent
		// and the fully resolved target chain carry the replaceability checks.
		if !isSymlink && info.Mode().Perm()&0002 != 0 {
			findings = appendOnce(findings, finding{Code: "ancestor_world_writable", Message: "an executable ancestor is writable by others"})
		}
		if rootDaemon {
			if !isSymlink && info.Mode().Perm()&0022 != 0 {
				findings = appendOnce(findings, finding{Code: "ancestor_writable_by_group_or_others", Message: "a root-scope executable ancestor is writable by group or others"})
			}
			if uid, ok := fileOwnerUID(info); !ok || uid != 0 {
				findings = appendOnce(findings, finding{Code: "ancestor_not_root_owned", Message: "a root-scope executable ancestor is not owned by root"})
			}
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			break
		}
		ancestor = parent
	}
	return findings
}

func appendOnce(findings []finding, item finding) []finding {
	for _, existing := range findings {
		if existing.Code == item.Code {
			return findings
		}
	}
	return append(findings, item)
}

func declaredExecutable(plist map[string]any) (string, bool) {
	if program, ok := nonEmptyString(plist["Program"]); ok {
		return program, true
	}
	args, ok := plist["ProgramArguments"].([]any)
	if !ok || len(args) == 0 {
		return "", false
	}
	return nonEmptyString(args[0])
}

func nonEmptyString(value any) (string, bool) {
	text, ok := value.(string)
	text = strings.TrimSpace(text)
	return text, ok && text != ""
}

func convertWithPlutil(ctx context.Context, path string) ([]byte, error) {
	return exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "json", "-o", "-", "--", path).Output()
}

func writeHuman(w io.Writer, report auditReport) {
	for _, entry := range report.Entries {
		status := "ok"
		if len(entry.Findings) > 0 {
			status = "finding"
		}
		fmt.Fprintf(w, "%s: %s", status, entry.Source)
		if entry.Label != "" {
			fmt.Fprintf(w, " label=%s", entry.Label)
		}
		if entry.Executable != "" {
			fmt.Fprintf(w, " executable=%s", entry.Executable)
		}
		if entry.DisabledDeclaration != nil {
			fmt.Fprintf(w, " disabled-declaration=%t", *entry.DisabledDeclaration)
		}
		fmt.Fprintln(w)
		for _, item := range entry.Findings {
			fmt.Fprintf(w, "  - %s: %s\n", item.Code, item.Message)
		}
	}
	fmt.Fprintf(w, "summary: scanned=%d findings=%d partial=%t\n", report.Scanned, report.FindingCount, report.Partial)
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func safeLabel(label string) string {
	if strings.Contains(label, "://") || len(label) > 200 {
		return "[redacted]"
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, label)
}

func safeExecutable(executable string) string {
	if strings.Contains(executable, "://") || len(executable) > 4096 {
		return "[redacted]"
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, executable)
}
