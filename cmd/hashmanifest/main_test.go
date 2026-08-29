package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateDeterministicAndWeirdPaths(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "z.txt", "z")
	writeTestFile(t, root, "a space.txt", "space")
	writeTestFile(t, root, "line\nbreak-한글.txt", "weird")

	var first, second bytes.Buffer
	if code := run([]string{"create", root}, &first, &bytes.Buffer{}); code != 0 {
		t.Fatalf("first create code = %d", code)
	}
	if code := run([]string{"create", root}, &second, &bytes.Buffer{}); code != 0 {
		t.Fatalf("second create code = %d", code)
	}
	if first.String() != second.String() {
		t.Fatal("manifest output is not deterministic")
	}

	records := parseJSONL(t, first.Bytes())
	if len(records) != 4 {
		t.Fatalf("record count = %d, want 4", len(records))
	}
	gotPaths := []string{records[1].Path, records[2].Path, records[3].Path}
	wantPaths := []string{"a space.txt", "line\nbreak-한글.txt", "z.txt"}
	if strings.Join(gotPaths, "|") != strings.Join(wantPaths, "|") {
		t.Fatalf("paths = %#v, want %#v", gotPaths, wantPaths)
	}
	if bytes.Count(first.Bytes(), []byte("\n")) != len(records) {
		t.Fatal("embedded path newline was not JSON escaped")
	}
}

func TestSubcommandHelpReturnsZero(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "create short", args: []string{"create", "-h"}, want: "usage: hashmanifest create"},
		{name: "create long", args: []string{"create", "--help"}, want: "usage: hashmanifest create"},
		{name: "verify short", args: []string{"verify", "-h"}, want: "usage: hashmanifest verify"},
		{name: "verify long", args: []string{"verify", "--help"}, want: "usage: hashmanifest verify"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(test.args, &stdout, &stderr); code != 0 {
				t.Fatalf("code = %d, want 0; stderr=%q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), test.want)
			}
		})
	}
}

func TestVerifyRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	manifest := strings.Join([]string{
		`{"type":"header","version":1,"algorithm":"sha256"}`,
		`{"type":"file","path":"../escape","size":0,"sha256":"` + strings.Repeat("0", 64) + `","status":"ok"}`,
		"",
	}, "\n")
	var stderr bytes.Buffer
	if code := verifyManifest(root, strings.NewReader(manifest), &bytes.Buffer{}, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2; stderr=%s", code, stderr.String())
	}
}

func TestVerifyMismatchAndMissing(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "mismatch", "new")
	digest := sha256.Sum256([]byte("old"))
	manifest := strings.Join([]string{
		`{"type":"header","version":1,"algorithm":"sha256"}`,
		`{"type":"file","path":"mismatch","size":3,"sha256":"` + hex.EncodeToString(digest[:]) + `","status":"ok"}`,
		`{"type":"file","path":"missing","size":1,"sha256":"` + strings.Repeat("0", 64) + `","status":"ok"}`,
		"",
	}, "\n")
	var stdout bytes.Buffer
	if code := verifyManifest(root, strings.NewReader(manifest), &stdout, &bytes.Buffer{}); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "MISMATCH \"mismatch\"") || !strings.Contains(stdout.String(), "MISSING \"missing\"") {
		t.Fatalf("output = %q", stdout.String())
	}
}

func TestCreateOutputCollisionAndSelfExclusion(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "data", "content")
	writeTestFile(t, root, "empty", "")
	output := filepath.Join(root, "manifest.jsonl")
	if code := run([]string{"create", "--output", output, root}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("create code = %d", code)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	records := parseJSONL(t, data)
	if len(records) != 3 || records[1].Path != "data" || records[2].Path != "empty" {
		t.Fatalf("records = %#v", records)
	}
	if records[2].Size == nil || *records[2].Size != 0 || !bytes.Contains(data, []byte(`"size":0`)) {
		t.Fatalf("zero-byte size not explicitly encoded: %s", data)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	if code := run([]string{"create", "--output", output, root}, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
		t.Fatalf("collision code = %d, want 2", code)
	}
	afterCollision, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, afterCollision) {
		t.Fatal("collision changed existing output")
	}
}

func TestCreatePublishRaceDoesNotOverwriteCompetitor(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "data", "content")
	output := filepath.Join(root, "manifest.jsonl")
	competitor := []byte("competitor wins\n")

	originalPublish := publishOutput
	publishOutput = func(tempPath, outputPath string) error {
		if err := os.WriteFile(outputPath, competitor, 0o600); err != nil {
			return err
		}
		return publishNoReplace(tempPath, outputPath)
	}
	t.Cleanup(func() { publishOutput = originalPublish })

	var stderr bytes.Buffer
	if code := run([]string{"create", "--output", output, root}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2; stderr=%q", code, stderr.String())
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, competitor) {
		t.Fatalf("competing output overwritten: got %q, want %q", got, competitor)
	}
	temps, err := filepath.Glob(filepath.Join(root, ".hashmanifest-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary output not cleaned up: %#v", temps)
	}
}

func TestCreateMaxFilesPartial(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a", "a")
	writeTestFile(t, root, "b", "b")
	var stdout bytes.Buffer
	if code := run([]string{"create", "--max-files", "1", root}, &stdout, &bytes.Buffer{}); code != 3 {
		t.Fatalf("code = %d, want 3", code)
	}
	records := parseJSONL(t, stdout.Bytes())
	if len(records) != 3 || records[2].Type != "issue" || records[2].Status != "limit" {
		t.Fatalf("records = %#v", records)
	}
}

func TestCreatedManifestVerifies(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "ok", "content")
	var manifest, output bytes.Buffer
	if code := run([]string{"create", root}, &manifest, &bytes.Buffer{}); code != 0 {
		t.Fatalf("create code = %d", code)
	}
	if code := verifyManifest(root, bytes.NewReader(manifest.Bytes()), &output, &bytes.Buffer{}); code != 0 {
		t.Fatalf("verify code = %d, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "MATCH \"ok\"") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestVerifyDoesNotFollowSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, outside, "secret", "secret")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	digest := sha256.Sum256([]byte("secret"))
	manifest := strings.Join([]string{
		`{"type":"header","version":1,"algorithm":"sha256"}`,
		`{"type":"file","path":"link","size":6,"sha256":"` + hex.EncodeToString(digest[:]) + `","status":"ok"}`,
		"",
	}, "\n")
	var stdout bytes.Buffer
	if code := verifyManifest(root, strings.NewReader(manifest), &stdout, &bytes.Buffer{}); code != 3 {
		t.Fatalf("code = %d, want 3", code)
	}
	if !strings.Contains(stdout.String(), "UNREADABLE \"link\"") {
		t.Fatalf("output = %q", stdout.String())
	}
}

func TestVerifyRejectsInsideRootDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "real/file", "inside")
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	digest := sha256.Sum256([]byte("inside"))
	manifest := strings.Join([]string{
		`{"type":"header","version":1,"algorithm":"sha256"}`,
		`{"type":"file","path":"alias/file","size":6,"sha256":"` + hex.EncodeToString(digest[:]) + `","status":"ok"}`,
		"",
	}, "\n")
	var stdout bytes.Buffer
	if code := verifyManifest(root, strings.NewReader(manifest), &stdout, &bytes.Buffer{}); code != 3 {
		t.Fatalf("code = %d, want 3", code)
	}
	if !strings.Contains(stdout.String(), `UNREADABLE "alias/file"`) {
		t.Fatalf("output = %q", stdout.String())
	}
}

func TestHashAndRestatDetectsSameMetadataReplacement(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	writeTestFile(t, root, "target", "same")
	expected, err := os.Lstat(target)
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

	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rootHandle.Close() })
	digest, changed, err := hashAndRestat(rootHandle, "target", expected)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || digest != "" {
		t.Fatalf("hashAndRestat = (%q, %t), want empty digest and changed", digest, changed)
	}
}

func TestHashNoSymlinkPathDetectsParentReplacement(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "dir/file", "same")
	target := filepath.Join(root, "dir", "file")
	expected, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rootHandle.Close() })

	digest, changed, err := hashNoSymlinkPathWithHook(rootHandle, filepath.Join("dir", "file"), expected, func() {
		if err := os.Rename(filepath.Join(root, "dir"), filepath.Join(root, "dir-old")); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, root, "dir/file", "same")
		if err := os.Chtimes(filepath.Join(root, "dir", "file"), expected.ModTime(), expected.ModTime()); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || digest == "" {
		t.Fatalf("hashNoSymlinkPathWithHook = (%q, %t), want digest and changed", digest, changed)
	}
}

func writeTestFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func parseJSONL(t *testing.T, data []byte) []manifestRecord {
	t.Helper()
	var records []manifestRecord
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var record manifestRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("invalid JSONL line %q: %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}
