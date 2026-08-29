//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunJSONRedactsArgumentsAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "safe.plist")
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>com.example.safe</string>
<key>ProgramArguments</key><array><string>/bin/echo</string><string>secret-token-123</string><string>https://private.example/path</string></array>
<key>EnvironmentVariables</key><dict><key>API_TOKEN</key><string>env-secret-456</string></dict>
<key>Disabled</key><false/>
</dict></plist>`
	if err := os.WriteFile(path, []byte(plist), 0600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--json", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
	}
	output := stdout.String()
	for _, forbidden := range []string{"secret-token-123", "private.example", "API_TOKEN", "env-secret-456", "EnvironmentVariables"} {
		if strings.Contains(output, forbidden) {
			t.Errorf("JSON leaked %q: %s", forbidden, output)
		}
	}
	if !strings.Contains(output, `"executable":"/bin/echo"`) || !strings.Contains(output, `"disabled_declaration":false`) {
		t.Fatalf("JSON missing safe declaration summary: %s", output)
	}
}

func TestAuditDuplicateLabelsAndMissingExecutable(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		writeEmptyPlist(t, dir, "one.plist"),
		writeEmptyPlist(t, dir, "two.plist"),
	}
	fixtures := map[string]map[string]any{
		paths[0]: {"Label": "com.example.duplicate", "Program": "/bin/echo"},
		paths[1]: {"Label": "com.example.duplicate"},
	}
	report := audit(paths, "user", auditConfig{convert: fixtureConverter(t, fixtures), maxFiles: 10})

	if report.Partial {
		t.Fatal("audit unexpectedly partial")
	}
	if !hasCode(report.Entries[0], "duplicate_label") || !hasCode(report.Entries[1], "duplicate_label") {
		t.Fatalf("duplicate findings missing: %+v", report.Entries)
	}
	if !hasCode(report.Entries[1], "missing_executable") {
		t.Fatalf("missing executable finding absent: %+v", report.Entries[1])
	}
}

func TestAuditExecutableFindings(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		writeEmptyPlist(t, dir, "relative.plist"),
		writeEmptyPlist(t, dir, "missing.plist"),
	}
	fixtures := map[string]map[string]any{
		paths[0]: {"Label": "relative", "ProgramArguments": []any{"echo", "do-not-print"}},
		paths[1]: {"Label": "missing", "Program": filepath.Join(dir, "does-not-exist")},
	}
	report := audit(paths, "user", auditConfig{convert: fixtureConverter(t, fixtures), maxFiles: 10})
	byLabel := make(map[string]entryReport)
	for _, entry := range report.Entries {
		byLabel[entry.Label] = entry
	}
	if !hasCode(byLabel["relative"], "relative_executable") {
		t.Fatalf("relative executable finding absent: %+v", byLabel["relative"])
	}
	if !hasCode(byLabel["missing"], "executable_missing") {
		t.Fatalf("missing executable finding absent: %+v", byLabel["missing"])
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("do-not-print")) {
		t.Fatalf("report leaked argument: %s", encoded)
	}
}

func TestAuditLimitIsPartialAndBounded(t *testing.T) {
	dir := t.TempDir()
	fixtures := make(map[string]map[string]any)
	for i, name := range []string{"a.plist", "b.plist", "c.plist"} {
		path := writeEmptyPlist(t, dir, name)
		fixtures[path] = map[string]any{"Label": name, "Program": "/bin/echo", "order": i}
	}
	report := audit([]string{dir}, "user", auditConfig{convert: fixtureConverter(t, fixtures), maxFiles: 2})
	if !report.Partial || report.Scanned != 2 {
		t.Fatalf("limit result = partial %t, scanned %d", report.Partial, report.Scanned)
	}
	count := 0
	for _, entry := range report.Entries {
		if hasCode(entry, "scan_limit_reached") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("scan limit findings = %d, entries = %+v", count, report.Entries)
	}
}

func TestAuditUnparseableIsPartial(t *testing.T) {
	path := writeEmptyPlist(t, t.TempDir(), "bad.plist")
	report := audit([]string{path}, "user", auditConfig{
		convert:  func(context.Context, string) ([]byte, error) { return nil, errors.New("secret parser detail") },
		maxFiles: 10,
	})
	if !report.Partial || !hasCode(report.Entries[0], "plist_unparseable") {
		t.Fatalf("unexpected report: %+v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("secret parser detail")) {
		t.Fatalf("report leaked converter error: %s", encoded)
	}
}

func TestRunUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--scope", "wrong"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run() code = %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: launchaudit") {
		t.Fatalf("usage missing: %q", stderr.String())
	}
}

func TestRunHelpReturnsZeroWithoutAudit(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{arg}, &stdout, &stderr); code != 0 {
			t.Errorf("run(%q) code = %d, stderr = %q", arg, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "usage: launchaudit") {
			t.Errorf("run(%q) usage missing: %q", arg, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("run(%q) unexpectedly audited: %q", arg, stdout.String())
		}
	}
}

func TestSafeLabelPreventsURLLeakWithoutFalseDuplicates(t *testing.T) {
	dir := t.TempDir()
	paths := []string{writeEmptyPlist(t, dir, "a.plist"), writeEmptyPlist(t, dir, "b.plist")}
	fixtures := map[string]map[string]any{
		paths[0]: {"Label": "https://one.example/token", "Program": "/bin/echo"},
		paths[1]: {"Label": "https://two.example/token", "Program": "/bin/echo"},
	}
	report := audit(paths, "user", auditConfig{convert: fixtureConverter(t, fixtures), maxFiles: 10})
	for _, entry := range report.Entries {
		if entry.Label != "[redacted]" || hasCode(entry, "duplicate_label") {
			t.Fatalf("unsafe or false duplicate label: %+v", entry)
		}
	}
}

func TestRootDaemonExecutableTrustFindings(t *testing.T) {
	dir := t.TempDir()
	untrustedDir := filepath.Join(dir, "writable")
	if err := os.Mkdir(untrustedDir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(untrustedDir, 0777); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(untrustedDir, "target")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "declared-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	plistPath := writeEmptyPlist(t, dir, "root.plist")
	fixtures := map[string]map[string]any{
		plistPath: {"Label": "com.example.root", "Program": link},
	}
	report := audit([]string{plistPath}, "local", auditConfig{
		convert: fixtureConverter(t, fixtures), maxFiles: 10,
		rootDaemon: func(string) bool { return true },
	})
	entry := report.Entries[0]
	for _, code := range []string{
		"executable_symlink",
		"executable_world_writable",
		"executable_writable_by_group_or_others",
		"ancestor_world_writable",
		"ancestor_writable_by_group_or_others",
	} {
		if !hasCode(entry, code) {
			t.Errorf("root-scope trust finding %q absent: %+v", code, entry.Findings)
		}
	}
	if os.Getuid() != 0 && (!hasCode(entry, "executable_not_root_owned") || !hasCode(entry, "ancestor_not_root_owned")) {
		t.Errorf("root ownership findings absent: %+v", entry.Findings)
	}
}

func TestUserScopeTrustAvoidsGroupWritableFalsePositiveButFlagsWorldWritable(t *testing.T) {
	dir := t.TempDir()
	programDir := filepath.Join(dir, "program")
	if err := os.Mkdir(programDir, 0770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(programDir, 0770); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(programDir, "target")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0770); err != nil {
		t.Fatal(err)
	}
	plistPath := writeEmptyPlist(t, dir, "user.plist")
	fixtures := map[string]map[string]any{
		plistPath: {"Label": "com.example.user", "Program": target},
	}
	config := auditConfig{
		convert: fixtureConverter(t, fixtures), maxFiles: 10,
		rootDaemon: func(string) bool { return false },
	}
	report := audit([]string{plistPath}, "user", config)
	entry := report.Entries[0]
	for _, code := range []string{
		"executable_writable_by_group_or_others",
		"ancestor_writable_by_group_or_others",
		"executable_not_root_owned",
		"ancestor_not_root_owned",
		"executable_world_writable",
		"ancestor_world_writable",
	} {
		if hasCode(entry, code) {
			t.Errorf("user-scope false positive %q: %+v", code, entry.Findings)
		}
	}

	if err := os.Chmod(programDir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0777); err != nil {
		t.Fatal(err)
	}
	report = audit([]string{plistPath}, "user", config)
	entry = report.Entries[0]
	if !hasCode(entry, "executable_world_writable") || !hasCode(entry, "ancestor_world_writable") {
		t.Fatalf("user scope missed world-writable trust: %+v", entry.Findings)
	}
}

func TestDeclaredSymlinkAncestorIsInspected(t *testing.T) {
	dir := t.TempDir()
	linkDir := filepath.Join(dir, "replaceable")
	if err := os.Mkdir(linkDir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(linkDir, 0777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "declared")
	if err := os.Symlink("/bin/echo", link); err != nil {
		t.Fatal(err)
	}
	plistPath := writeEmptyPlist(t, dir, "symlink.plist")
	fixtures := map[string]map[string]any{
		plistPath: {"Label": "com.example.symlink", "Program": link},
	}
	report := audit([]string{plistPath}, "user", auditConfig{
		convert: fixtureConverter(t, fixtures), maxFiles: 10,
		rootDaemon: func(string) bool { return false },
	})
	entry := report.Entries[0]
	if !hasCode(entry, "executable_symlink") || !hasCode(entry, "ancestor_world_writable") {
		t.Fatalf("declared symlink ancestry not trusted: %+v", entry.Findings)
	}
}

func writeEmptyPlist(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixtureConverter(t *testing.T, fixtures map[string]map[string]any) plistConverter {
	t.Helper()
	return func(_ context.Context, path string) ([]byte, error) {
		fixture, ok := fixtures[path]
		if !ok {
			return nil, errors.New("fixture missing")
		}
		return json.Marshal(fixture)
	}
}

func hasCode(entry entryReport, code string) bool {
	for _, item := range entry.Findings {
		if item.Code == code {
			return true
		}
	}
	return false
}
