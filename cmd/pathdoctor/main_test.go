package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseZshPATHDirectFormsQuotesCommentsAndContinuations(t *testing.T) {
	home := t.TempDir()
	input := strings.Join([]string{
		`export PATH="$HOME/bin:$PATH" # keep inherited`,
		`PATH=/one:\`,
		`/two:${PATH}`,
		`PATH+=:/three`,
		`path=("/four space" '/five#hash' $path) # comment`,
		`path+=(/six ${path[@]})`,
	}, "\n")
	items, findings, partial := parseZshPATH(input, "test.zsh", home)
	if partial || len(findings) != 0 {
		t.Fatalf("partial=%v findings=%+v", partial, findings)
	}
	var got []string
	placeholders := 0
	for _, item := range items {
		if item.placeholder {
			placeholders++
		} else {
			got = append(got, item.value)
		}
	}
	want := []string{
		"/four space", "/five#hash", "/one", "/two", filepath.Join(home, "bin"), "/three",
		"/six", "/four space", "/five#hash", "/one", "/two", filepath.Join(home, "bin"), "/three",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") || placeholders != 2 {
		t.Fatalf("items=%+v, want values=%q placeholders=2", items, want)
	}
}

func TestParseScalarHOMEAndPATHPlaceholder(t *testing.T) {
	home := "/safe/home"
	items, findings, partial := parseZshPATH(`PATH="$HOME/bin:${HOME}/sbin:$PATH"`, ".zshrc", home)
	if partial || len(findings) != 0 || len(items) != 3 {
		t.Fatalf("items=%+v findings=%+v partial=%v", items, findings, partial)
	}
	if items[0].value != "/safe/home/bin" || items[1].value != "/safe/home/sbin" || !items[2].placeholder {
		t.Fatalf("items=%+v", items)
	}
}

func TestSequentialScalarPlaceholdersSplicePriorState(t *testing.T) {
	input := "PATH=/a:$PATH\nPATH=$PATH:/b\nPATH=/c:$PATH:/a\n"
	items, findings, partial := parseZshPATH(input, ".zshrc", "/home/test")
	if partial || len(findings) != 0 {
		t.Fatalf("findings=%+v partial=%v", findings, partial)
	}
	want := []string{"/c", "/a", "<inherited>", "/b", "/a"}
	if got := itemValues(items); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("items=%+v got=%q want=%q", items, got, want)
	}
	rep := report{}
	auditItems(&rep, items, fakeExistingOps())
	if !hasKind(rep.Findings, "duplicate") {
		t.Fatalf("expected preserved /a duplicate, findings=%+v", rep.Findings)
	}
}

func TestSequentialArrayPlaceholdersSplicePriorState(t *testing.T) {
	input := "path=(/a $path)\npath=($path /b)\npath+=(/c $path)\n"
	items, findings, partial := parseZshPATH(input, ".zshrc", "/home/test")
	if partial || len(findings) != 0 {
		t.Fatalf("findings=%+v partial=%v", findings, partial)
	}
	want := []string{"/a", "<inherited>", "/b", "/c", "/a", "<inherited>", "/b"}
	if got := itemValues(items); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("items=%+v got=%q want=%q", items, got, want)
	}
}

func TestSingleQuotedHOMERemainsLiteral(t *testing.T) {
	items, findings, partial := parseZshPATH("PATH='$HOME/bin':\"$HOME/sbin\":$PATH\n", ".zshrc", "/safe/home")
	if partial || len(findings) != 0 || len(items) != 3 {
		t.Fatalf("items=%+v findings=%+v partial=%v", items, findings, partial)
	}
	if items[0].value != "$HOME/bin" || items[1].value != "/safe/home/sbin" || !items[2].placeholder {
		t.Fatalf("items=%+v", items)
	}
}

func TestParseMultilineArrayWithComments(t *testing.T) {
	input := "path=(\n  /one # first\n  \"/two space\"\n  $path\n)\n"
	items, findings, partial := parseZshPATH(input, ".zshrc", "/home/test")
	if partial || len(findings) != 0 || len(items) != 3 {
		t.Fatalf("items=%+v findings=%+v partial=%v", items, findings, partial)
	}
	if items[0].value != "/one" || items[1].value != "/two space" || !items[2].placeholder {
		t.Fatalf("items=%+v", items)
	}
}

func TestCommandSubstitutionNeverRunsAndIsRedacted(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, "SENTINEL")
	config := filepath.Join(root, ".zshrc")
	content := "PATH=/safe:$(touch " + sentinel + "):$PATH\nPATH+=:`touch " + sentinel + "`\n"
	writeTestFile(t, config, content, 0o600)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--file", config}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("command substitution executed: %v", err)
	}
	if strings.Contains(stdout.String(), "touch") || !strings.Contains(stdout.String(), "dynamic-unresolved") {
		t.Fatalf("unsafe/unhelpful output: %q", stdout.String())
	}
}

func TestSourceDotEvalArePartialRedactedAndNeverRun(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, "SOURCE_EVAL_SENTINEL")
	config := filepath.Join(root, ".zshrc")
	content := strings.Join([]string{
		`source "$(touch ` + sentinel + `)"`,
		`. /missing/file`,
		`eval "$(touch ` + sentinel + `)"`,
		`[ -f /maybe ] && source "$(touch ` + sentinel + `)"`,
		`if test -f /maybe; then builtin source /maybe; fi`,
		`command eval "ignored"`,
		`# source /commented`,
		`echo source /not-a-command`,
		`alias harmless='eval ignored'`,
	}, "\n")
	writeTestFile(t, config, content, 0o600)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", "--file", config}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("source/eval content executed: %v", err)
	}
	if strings.Contains(stdout.String(), "touch") || strings.Contains(stdout.String(), sentinel) {
		t.Fatalf("external command RHS leaked: %q", stdout.String())
	}
	var rep report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Partial {
		t.Fatalf("report not partial: %+v", rep)
	}
	count := 0
	for _, finding := range rep.Findings {
		if finding.Kind == "external-mutation-unresolved" {
			count++
			if finding.Component != "[redacted]" {
				t.Fatalf("finding not redacted: %+v", finding)
			}
		}
	}
	if count != 6 {
		t.Fatalf("external mutation count=%d, want 6; findings=%+v", count, rep.Findings)
	}
}

func TestExternalMutationCommandBoundaries(t *testing.T) {
	tests := []struct {
		statement string
		want      bool
	}{
		{statement: `source /file`, want: true},
		{statement: `. /file`, want: true},
		{statement: `eval "$PATH=/other"`, want: true},
		{statement: `[ -s /file ] && source /file`, want: true},
		{statement: `if test -f /file; then command source /file; fi`, want: true},
		{statement: `{ source /file; }`, want: true},
		{statement: `(eval ignored)`, want: true},
		{statement: `echo source /file`, want: false},
		{statement: `alias x='eval ignored'`, want: false},
		{statement: `sourcecode=/file`, want: false},
	}
	for _, test := range tests {
		if got := externalMutationStatement(test.statement); got != test.want {
			t.Errorf("externalMutationStatement(%q)=%v, want %v", test.statement, got, test.want)
		}
	}
}

func TestGroupedSourceAndEvalArePartialAndNeverRun(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, "GROUP_SENTINEL")
	config := filepath.Join(root, ".zshrc")
	writeTestFile(t, config, strings.Join([]string{
		`{ source /missing; }`,
		`{ eval "$(touch ` + sentinel + `)"; }`,
	}, "\n"), 0o600)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", "--file", config}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("grouped eval executed: %v", err)
	}
	if strings.Contains(stdout.String(), "touch") || strings.Contains(stdout.String(), sentinel) {
		t.Fatalf("grouped RHS leaked: %q", stdout.String())
	}
	var rep report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, finding := range rep.Findings {
		if finding.Kind == "external-mutation-unresolved" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("external mutation count=%d, want 2; findings=%+v", count, rep.Findings)
	}
}

func TestTypesetAndExportOptionsAssignPATH(t *testing.T) {
	input := strings.Join([]string{
		`typeset -gx PATH=/one:$PATH`,
		`export -x PATH=$PATH:/two`,
		`typeset -ga path=(/zero $path)`,
	}, "\n")
	items, findings, partial := parseZshPATH(input, ".zshrc", "/home/test")
	if partial || len(findings) != 0 {
		t.Fatalf("findings=%+v partial=%v", findings, partial)
	}
	want := []string{"/zero", "/one", "<inherited>", "/two"}
	if got := itemValues(items); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("items=%+v got=%q want=%q", items, got, want)
	}
}

func TestTypesetDynamicPATHIsRedactedPartial(t *testing.T) {
	items, findings, partial := parseZshPATH(`typeset -gx PATH="$(untrusted):$PATH"`, ".zshrc", "/home/test")
	if !partial || len(items) != 1 || !items[0].placeholder {
		t.Fatalf("items=%+v findings=%+v partial=%v", items, findings, partial)
	}
	if len(findings) != 1 || findings[0].Kind != "dynamic-unresolved" || findings[0].Component != "[redacted]" {
		t.Fatalf("findings=%+v", findings)
	}
}

func TestAuditConcreteProblems(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode and symlink behavior")
	}
	root := t.TempDir()
	good := filepath.Join(root, "good")
	world := filepath.Join(root, "world")
	file := filepath.Join(root, "file")
	broken := filepath.Join(root, "broken")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(world, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(world, 0o777); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, file, "x", 0o644)
	if err := os.Symlink(filepath.Join(root, "absent-target"), broken); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(good, alias); err != nil {
		t.Fatal(err)
	}

	items := []pathItem{
		{value: filepath.Join(root, "missing"), source: "cfg", line: 1},
		{value: broken, source: "cfg", line: 2},
		{value: file, source: "cfg", line: 3},
		{value: good, source: "cfg", line: 4},
		{value: alias, source: "cfg", line: 5},
		{value: "relative/bin", source: "cfg", line: 6},
		{value: "", source: "cfg", line: 7},
		{value: world, source: "cfg", line: 8},
	}
	rep := report{}
	auditItems(&rep, items, defaultOps)
	want := []string{"broken-symlink", "duplicate", "empty-component", "missing", "not-directory", "relative", "world-writable"}
	for _, kind := range want {
		if !hasKind(rep.Findings, kind) {
			t.Errorf("missing kind %q in %+v", kind, rep.Findings)
		}
	}
}

func TestOversizedFileIsExplicitPartial(t *testing.T) {
	config := filepath.Join(t.TempDir(), ".zshrc")
	writeTestFile(t, config, strings.Repeat("x", maxFileSize+1), 0o600)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--file", config, "--json"}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var rep report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Partial || !hasKind(rep.Findings, "oversized-file") {
		t.Fatalf("report=%+v", rep)
	}
}

func TestJSONDeterministicAndHumanQuotesControlCharacters(t *testing.T) {
	root := t.TempDir()
	controlPath := filepath.Join(root, "bad\tname")
	config := filepath.Join(root, ".zshrc")
	writeTestFile(t, config, "PATH='"+controlPath+"'\n", 0o600)

	var first, second, stderr bytes.Buffer
	if code := run([]string{"--json", "--file", config}, &first, &stderr); code != 1 {
		t.Fatalf("code=%d output=%q stderr=%q", code, first.String(), stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"--json", "--file", config}, &second, &stderr); code != 1 {
		t.Fatalf("code=%d output=%q stderr=%q", code, second.String(), stderr.String())
	}
	if first.String() != second.String() || strings.Contains(first.String(), "bad\tname") {
		t.Fatalf("JSON non-deterministic or raw control: %q vs %q", first.String(), second.String())
	}
	var human bytes.Buffer
	if code := run([]string{"--file", config}, &human, &stderr); code != 1 {
		t.Fatalf("human code=%d output=%q", code, human.String())
	}
	if strings.Contains(human.String(), "bad\tname") || !strings.Contains(human.String(), `\t`) {
		t.Fatalf("human control not quoted: %q", human.String())
	}
}

func TestCurrentPATHAudited(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, ".zshrc")
	good := filepath.Join(root, "good")
	if err := os.Mkdir(good, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, config, "PATH=$PATH\n", 0o600)
	ops := defaultOps
	ops.getenv = func(key string) string {
		if key == "PATH" {
			return good + string(os.PathListSeparator) + string(os.PathListSeparator) + good
		}
		return ""
	}
	ops.userHomeDir = func() (string, error) { return root, nil }
	var stdout, stderr bytes.Buffer
	code := runWithOps([]string{"--file", config, "--current", "--json"}, &stdout, &stderr, ops)
	if code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var rep report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !hasKind(rep.Findings, "empty-component") || !hasKind(rep.Findings, "duplicate") {
		t.Fatalf("report=%+v", rep)
	}
}

func TestUsageAndCleanExitCodes(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, ".zshrc")
	dir := filepath.Join(root, "bin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, config, "PATH="+dir+"\n", 0o600)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--file", config}, &stdout, &stderr); code != 0 {
		t.Fatalf("clean code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if code := run([]string{"--bogus"}, &stdout, &stderr); code != 2 {
		t.Fatalf("usage code=%d", code)
	}
}

func hasKind(findings []finding, kind string) bool {
	for _, finding := range findings {
		if finding.Kind == kind {
			return true
		}
	}
	return false
}

func itemValues(items []pathItem) []string {
	values := make([]string, 0, len(items))
	for _, item := range items {
		if item.placeholder {
			values = append(values, "<inherited>")
		} else {
			values = append(values, item.value)
		}
	}
	return values
}

func fakeExistingOps() fileOps {
	ops := defaultOps
	dirInfo, err := os.Stat(os.TempDir())
	if err != nil {
		panic(err)
	}
	ops.lstat = func(string) (os.FileInfo, error) { return dirInfo, nil }
	ops.stat = func(string) (os.FileInfo, error) { return dirInfo, nil }
	ops.evalSymlinks = func(path string) (string, error) { return filepath.Clean(path), nil }
	return ops
}

func writeTestFile(t *testing.T, name, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
