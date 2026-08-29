package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRelativeValidLink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink("target", link); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", root}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr=%q", code, stderr.String())
	}
	report := decodeReport(t, stdout.Bytes())
	if len(report.Links) != 1 || report.Links[0].Status != statusOKRelative {
		t.Fatalf("links = %#v, want one %s link", report.Links, statusOKRelative)
	}
	if report.Links[0].Target != "target" || report.Links[0].Hops != 1 {
		t.Fatalf("link = %#v, want raw target and one hop", report.Links[0])
	}
}

func TestRunAbsoluteValidLinkIsRisky(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", root}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want absolute-link finding; stderr=%q", code, stderr.String())
	}
	report := decodeReport(t, stdout.Bytes())
	if report.Links[0].Status != statusOKAbsolute || !report.Links[0].Risk {
		t.Fatalf("link = %#v, want risky %s", report.Links[0], statusOKAbsolute)
	}
}

func TestRunDanglingLink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("missing", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", root}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want dangling finding; stderr=%q", code, stderr.String())
	}
	report := decodeReport(t, stdout.Bytes())
	if report.Links[0].Status != statusDangling {
		t.Fatalf("status = %q, want %s", report.Links[0].Status, statusDangling)
	}
}

func TestRunLoopLink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("b", filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", root}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want loop finding; stderr=%q", code, stderr.String())
	}
	report := decodeReport(t, stdout.Bytes())
	if len(report.Links) != 2 || report.Links[0].Status != statusLoop || report.Links[1].Status != statusLoop {
		t.Fatalf("links = %#v, want two loop findings", report.Links)
	}
}

func TestRunHopLimitIsPartial(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	next := "target"
	for i := symlinkHopLimit; i >= 0; i-- {
		name := fmt.Sprintf("link-%02d", i)
		if err := os.Symlink(next, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
		next = name
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", root}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("run() = %d, want hop-limit partial; stderr=%q", code, stderr.String())
	}
	report := decodeReport(t, stdout.Bytes())
	for _, link := range report.Links {
		if filepath.Base(link.Path) == "link-00" {
			if link.Status != statusHopLimit || !link.Risk || link.Reason != "symlink-hop-limit" {
				t.Fatalf("long-chain link = %#v, want risky hop-limit finding", link)
			}
			return
		}
	}
	t.Fatalf("long-chain entry not found in %#v", report.Links)
}

func TestRunEscapeOutsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("../outside", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", root}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want outside-root finding; stderr=%q", code, stderr.String())
	}
	report := decodeReport(t, stdout.Bytes())
	if report.Links[0].Status != statusOutside {
		t.Fatalf("status = %q, want %s", report.Links[0].Status, statusOutside)
	}
}

func TestRunDoesNotTraverseSymlinkDirectory(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(root, "external-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(external, "hidden-dangling")); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", root}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, stderr=%q", code, stderr.String())
	}
	report := decodeReport(t, stdout.Bytes())
	if len(report.Links) != 1 || report.Links[0].Status != statusOutside {
		t.Fatalf("links = %#v, want only symlink directory finding", report.Links)
	}
}

func TestRunLimitsAreExplicitlyPartial(t *testing.T) {
	t.Run("exact-boundary", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "target"), []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("target", filepath.Join(root, "only-link")); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := run([]string{"--json", "--max-links", "1", root}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run() = %d, want complete exact-boundary scan; stderr=%q", code, stderr.String())
		}
		report := decodeReport(t, stdout.Bytes())
		if report.Partial || len(report.Links) != 1 || contains(report.PartialReason, "max-links") {
			t.Fatalf("report = %#v, want complete one-link report", report)
		}
	})

	t.Run("links", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "target"), []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"a", "b"} {
			if err := os.Symlink("target", filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
		}

		var stdout, stderr bytes.Buffer
		code := run([]string{"--json", "--max-links", "1", root}, &stdout, &stderr)
		if code != 3 {
			t.Fatalf("run() = %d, want partial; stderr=%q", code, stderr.String())
		}
		report := decodeReport(t, stdout.Bytes())
		if !report.Partial || len(report.Links) != 1 || !contains(report.PartialReason, "max-links") {
			t.Fatalf("report = %#v, want one link and max-links partial", report)
		}
	})

	t.Run("depth", func(t *testing.T) {
		root := t.TempDir()
		nested := filepath.Join(root, "nested")
		if err := os.Mkdir(nested, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("missing", filepath.Join(nested, "link")); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := run([]string{"--json", "--max-depth", "0", root}, &stdout, &stderr)
		if code != 3 {
			t.Fatalf("run() = %d, want partial; stderr=%q", code, stderr.String())
		}
		report := decodeReport(t, stdout.Bytes())
		if !report.Partial || len(report.Links) != 0 || !contains(report.PartialReason, "max-depth") {
			t.Fatalf("report = %#v, want depth-limited partial", report)
		}
	})
}

func TestHumanOutputQuotesControlCharacters(t *testing.T) {
	root := t.TempDir()
	linkName := "link\nname"
	rawTarget := "missing\tname"
	if err := os.Symlink(rawTarget, filepath.Join(root, linkName)); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{root}, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, want dangling finding; stderr=%q", code, stderr.String())
	}
	output := stdout.String()
	if strings.Contains(output, linkName) || strings.Contains(output, rawTarget) {
		t.Fatalf("human output contains raw control characters: %q", output)
	}
	if !strings.Contains(output, `link\nname`) || !strings.Contains(output, `missing\tname`) {
		t.Fatalf("human output lacks quoted path/target: %q", output)
	}
}

func TestRunJSONIsMachineReadable(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--json", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d, stderr=%q", code, stderr.String())
	}
	if stdout.Len() == 0 || stdout.Bytes()[stdout.Len()-1] != '\n' {
		t.Fatalf("JSON output = %q, want newline-terminated JSON", stdout.String())
	}
	report := decodeReport(t, stdout.Bytes())
	if report.Root == "" || report.Links == nil {
		t.Fatalf("report = %#v, want root and non-nil links", report)
	}
}

func TestRunHelpIsSuccessful(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run([]string{arg}, &stdout, &stderr); code != 0 {
				t.Fatalf("run(%q) = %d, want 0; stderr=%q", arg, code, stderr.String())
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), "usage: symaudit") {
				t.Fatalf("stdout=%q stderr=%q, want usage on stderr", stdout.String(), stderr.String())
			}
		})
	}
}

func decodeReport(t *testing.T, data []byte) auditReport {
	t.Helper()
	var report auditReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("invalid JSON %q: %v", string(data), err)
	}
	return report
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
