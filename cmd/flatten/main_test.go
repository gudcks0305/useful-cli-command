package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunRejectsPlannedNameCollisionBeforeWrite(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dest := filepath.Join(root, "output")
	writeFile(t, filepath.Join(src, "a_b.txt"), "top", 0o644)
	writeFile(t, filepath.Join(src, "a", "b.txt"), "nested", 0o644)

	code, _, stderr := runFlatten([]string{"--output", dest, src}, "y\n")
	if code != 2 {
		t.Fatalf("code = %d, want 2; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "출력 이름 충돌") {
		t.Fatalf("stderr = %q, want collision error", stderr)
	}
	assertNotExist(t, dest)
}

func TestRunRejectsExistingDestinationWithoutOverwrite(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dest := filepath.Join(root, "output")
	writeFile(t, filepath.Join(src, "plain.txt"), "new", 0o644)
	writeFile(t, filepath.Join(dest, "plain.txt"), "keep", 0o600)

	code, _, stderr := runFlatten([]string{"--output", dest, src}, "y\n")
	if code != 2 {
		t.Fatalf("code = %d, want 2; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "이미 존재") {
		t.Fatalf("stderr = %q, want existing destination error", stderr)
	}
	data, err := os.ReadFile(filepath.Join(dest, "plain.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" {
		t.Fatalf("existing destination changed to %q", data)
	}
}

func TestRunRejectsEqualAndNestedOutput(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "plain.txt"), "data", 0o644)

	tests := []struct {
		name   string
		output string
		want   string
	}{
		{name: "equal", output: src, want: "같습니다"},
		{name: "nested", output: filepath.Join(src, "nested", "output"), want: "내부"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, _, stderr := runFlatten([]string{"--dry-run", "--output", test.output, src}, "")
			if code != 2 {
				t.Fatalf("code = %d, want 2; stderr=%s", code, stderr)
			}
			if !strings.Contains(stderr, test.want) {
				t.Fatalf("stderr = %q, want %q", stderr, test.want)
			}
			if test.name == "nested" {
				assertNotExist(t, test.output)
			}
		})
	}
}

func TestRunRejectsOutputViaSymlinkIntoSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires privileges on Windows")
	}
	root := t.TempDir()
	src := filepath.Join(root, "source")
	writeFile(t, filepath.Join(src, "plain.txt"), "data", 0o644)
	link := filepath.Join(root, "source-link")
	if err := os.Symlink(src, link); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(link, "nested-output")

	code, _, stderr := runFlatten([]string{"--dry-run", "--output", output, src}, "")
	if code != 2 || !strings.Contains(stderr, "내부") {
		t.Fatalf("code=%d stderr=%q, want nested output rejection", code, stderr)
	}
	assertNotExist(t, filepath.Join(src, "nested-output"))
}

func TestRunRejectsSymlinkAndNonDirectorySource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires privileges on Windows")
	}
	root := t.TempDir()
	realSource := filepath.Join(root, "real")
	writeFile(t, filepath.Join(realSource, "plain.txt"), "data", 0o644)
	symlinkSource := filepath.Join(root, "link")
	if err := os.Symlink(realSource, symlinkSource); err != nil {
		t.Fatal(err)
	}
	nonDirectory := filepath.Join(root, "file")
	writeFile(t, nonDirectory, "data", 0o644)

	for _, test := range []struct {
		name string
		src  string
		want string
	}{
		{name: "symlink", src: symlinkSource, want: "심볼릭 링크"},
		{name: "file", src: nonDirectory, want: "디렉터리가 아닙니다"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, _, stderr := runFlatten([]string{"--dry-run", test.src}, "")
			if code != 2 || !strings.Contains(stderr, test.want) {
				t.Fatalf("code=%d stderr=%q, want %q", code, stderr, test.want)
			}
		})
	}
}

func TestRunSkipsSymlinkFilesAndDoesNotFollowThem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires privileges on Windows")
	}
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dest := filepath.Join(root, "output")
	outside := filepath.Join(root, "secret.txt")
	writeFile(t, outside, "secret", 0o600)
	writeFile(t, filepath.Join(src, "plain.txt"), "safe", 0o644)
	if err := os.Symlink(outside, filepath.Join(src, "linked.txt")); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runFlatten([]string{"--output", dest, src}, "y\n")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "건너뜀 (symlink): linked.txt") {
		t.Fatalf("stderr = %q, want symlink report", stderr)
	}
	assertNotExist(t, filepath.Join(dest, "linked.txt"))
	data, err := os.ReadFile(filepath.Join(dest, "plain.txt"))
	if err != nil || string(data) != "safe" {
		t.Fatalf("copied plain file = %q, %v", data, err)
	}
}

func TestRunDryRunAndCancelDoNotWrite(t *testing.T) {
	for _, test := range []struct {
		name  string
		args  func(src, dest string) []string
		input string
		want  string
	}{
		{
			name:  "dry-run",
			args:  func(src, dest string) []string { return []string{"--dry-run", "--output", dest, src} },
			input: "y\n",
			want:  "Dry-run",
		},
		{
			name:  "cancel",
			args:  func(src, dest string) []string { return []string{"--output", dest, src} },
			input: "n\n",
			want:  "취소되었습니다",
		},
		{
			name:  "eof rejects",
			args:  func(src, dest string) []string { return []string{"--output", dest, src} },
			input: "",
			want:  "취소되었습니다",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "source")
			dest := filepath.Join(root, "output")
			writeFile(t, filepath.Join(src, "plain.txt"), "data", 0o644)
			code, stdout, stderr := runFlatten(test.args(src, dest), test.input)
			if code != 0 {
				t.Fatalf("code = %d, want 0; stderr=%s", code, stderr)
			}
			if !strings.Contains(stdout, test.want) {
				t.Fatalf("stdout = %q, want %q", stdout, test.want)
			}
			assertNotExist(t, dest)
		})
	}
}

func TestRunConfirmationIOErrorsFailBeforeOutputCreation(t *testing.T) {
	for _, test := range []struct {
		name   string
		input  io.Reader
		output func() io.Writer
		want   error
	}{
		{
			name:   "read error",
			input:  errorReader{err: errConfirmationRead},
			output: func() io.Writer { return &bytes.Buffer{} },
			want:   errConfirmationRead,
		},
		{
			name:   "write error",
			input:  strings.NewReader("yes\n"),
			output: func() io.Writer { return &promptErrorWriter{} },
			want:   io.ErrClosedPipe,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "source")
			dest := filepath.Join(root, "output")
			writeFile(t, filepath.Join(src, "plain.txt"), "data", 0o644)
			var stderr bytes.Buffer
			code := run([]string{"--output", dest, src}, test.input, test.output(), &stderr)
			if code == 0 {
				t.Fatalf("code = 0, want nonzero; stderr=%s", stderr.String())
			}
			if !strings.Contains(stderr.String(), test.want.Error()) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), test.want)
			}
			assertNotExist(t, dest)
		})
	}
}

func TestRunSuccessfulCopyPreservesContentAndMode(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dest := filepath.Join(root, "output")
	sourcePath := filepath.Join(src, "album", "photo2.txt")
	writeFile(t, sourcePath, "photo bytes", 0o640)
	if err := os.Chmod(sourcePath, 0o640); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runFlatten([]string{"--output", dest, src}, "yes\n")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout=%s stderr=%s", code, stdout, stderr)
	}
	destinationPath := filepath.Join(dest, "album_photo02.txt")
	data, err := os.ReadFile(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "photo bytes" {
		t.Fatalf("content = %q", data)
	}
	info, err := os.Stat(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
}

func TestRunRejectsInvalidFlags(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "plain.txt"), "data", 0o644)
	tests := []struct {
		name string
		args []string
	}{
		{name: "negative pad", args: []string{"--pad", "-1", src}},
		{name: "excessive pad", args: []string{"--pad", "1025", src}},
		{name: "empty separator", args: []string{"--sep", "", src}},
		{name: "path separator", args: []string{"--sep", "/", src}},
		{name: "invalid integer", args: []string{"--pad", "nope", src}},
		{name: "extra positional", args: []string{src, src}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, _, _ := runFlatten(test.args, "")
			if code != 2 {
				t.Fatalf("code = %d, want 2", code)
			}
		})
	}
}

func TestCopyFileNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dest := filepath.Join(root, "destination")
	writeFile(t, src, "new", 0o644)
	writeFile(t, dest, "keep", 0o600)
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rootHandle.Close()
	sourceInfo, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	op := Operation{
		RelPath:       "source",
		NewName:       "destination",
		SourceInfo:    sourceInfo,
		SourceSize:    sourceInfo.Size(),
		SourceModTime: sourceInfo.ModTime(),
	}

	if err := copyFile(rootHandle, rootHandle, op, nil); err == nil {
		t.Fatal("copyFile succeeded over existing destination")
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" {
		t.Fatalf("existing destination changed to %q", data)
	}
}

func TestRunRejectsDestinationSwapAfterPreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires privileges on Windows")
	}

	for _, test := range []struct {
		name           string
		existingOutput bool
	}{
		{name: "parent swapped", existingOutput: false},
		{name: "output root swapped", existingOutput: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "source")
			writeFile(t, filepath.Join(src, "new.txt"), "new", 0o644)
			outside := filepath.Join(root, "outside")
			writeFile(t, filepath.Join(outside, "sentinel.txt"), "keep", 0o600)

			parent := filepath.Join(root, "destination-parent")
			if err := os.Mkdir(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(parent, "output")
			swapTarget := parent
			movedTarget := filepath.Join(root, "moved-parent")
			if test.existingOutput {
				if err := os.Mkdir(dest, 0o755); err != nil {
					t.Fatal(err)
				}
				swapTarget = dest
				movedTarget = filepath.Join(root, "moved-output")
			}

			input := &actionReader{
				reader: strings.NewReader("y\n"),
				action: func() {
					if err := os.Rename(swapTarget, movedTarget); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, swapTarget); err != nil {
						t.Fatal(err)
					}
				},
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"--output", dest, src}, input, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "확인 후 변경됨") {
				t.Fatalf("stderr = %q, want path-swap rejection", stderr.String())
			}
			assertNotExist(t, filepath.Join(outside, "new.txt"))
			sentinel, err := os.ReadFile(filepath.Join(outside, "sentinel.txt"))
			if err != nil || string(sentinel) != "keep" {
				t.Fatalf("outside sentinel = %q, %v", sentinel, err)
			}
			if !test.existingOutput {
				assertNotExist(t, filepath.Join(movedTarget, "output"))
			}
		})
	}
}

func TestRunRejectsSourceSubstitutionAfterConfirmation(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dest := filepath.Join(root, "output")
	sourcePath := filepath.Join(src, "plain.txt")
	writeFile(t, sourcePath, "original", 0o640)

	hooks := &executionHooks{
		beforeCopyValidation: func(Operation) {
			if err := os.Rename(sourcePath, sourcePath+".replaced"); err != nil {
				t.Fatal(err)
			}
			writeFile(t, sourcePath, "substitute", 0o640)
		},
	}
	var stdout, stderr bytes.Buffer
	code := runWithHooks([]string{"--output", dest, src}, strings.NewReader("yes\n"), &stdout, &stderr, hooks)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "preflight 후 변경") {
		t.Fatalf("stderr = %q, want substitution rejection", stderr.String())
	}
	assertNotExist(t, filepath.Join(dest, "plain.txt"))
}

func TestRunRejectsInPlaceSourceMutationAndRemovesDestination(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dest := filepath.Join(root, "output")
	sourcePath := filepath.Join(src, "plain.txt")
	writeFile(t, sourcePath, "original", 0o640)

	hooks := &executionHooks{
		afterSourceValidation: func(op Operation) {
			if err := os.WriteFile(sourcePath, []byte("mutated!"), 0o640); err != nil {
				t.Fatal(err)
			}
			changedTime := op.SourceModTime.Add(2 * time.Second)
			if err := os.Chtimes(sourcePath, changedTime, changedTime); err != nil {
				t.Fatal(err)
			}
		},
	}
	var stdout, stderr bytes.Buffer
	code := runWithHooks([]string{"--output", dest, src}, strings.NewReader("yes\n"), &stdout, &stderr, hooks)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "복사 중 변경") {
		t.Fatalf("stderr = %q, want mutation rejection", stderr.String())
	}
	assertNotExist(t, filepath.Join(dest, "plain.txt"))
}

func TestRunRejectsDestinationRenameAfterCopies(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dest := filepath.Join(root, "output")
	movedDest := filepath.Join(root, "moved-output")
	writeFile(t, filepath.Join(src, "plain.txt"), "data", 0o640)

	hooks := &executionHooks{
		afterCopies: func() {
			if err := os.Rename(dest, movedDest); err != nil {
				t.Fatal(err)
			}
		},
	}
	var stdout, stderr bytes.Buffer
	code := runWithHooks([]string{"--output", dest, src}, strings.NewReader("yes\n"), &stdout, &stderr, hooks)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "완료:") {
		t.Fatalf("stdout reports false success: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "출력 루트가 복사 중 변경됨") {
		t.Fatalf("stderr = %q, want destination rename rejection", stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(movedDest, "plain.txt"))
	if err != nil || string(data) != "data" {
		t.Fatalf("moved output = %q, %v", data, err)
	}
}

func TestDryRunPlanIsDeterministic(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dest := filepath.Join(root, "output")
	writeFile(t, filepath.Join(src, "z10.txt"), "10", 0o644)
	writeFile(t, filepath.Join(src, "z2.txt"), "2", 0o644)
	writeFile(t, filepath.Join(src, "a", "item.txt"), "a", 0o644)

	code1, stdout1, stderr1 := runFlatten([]string{"--dry-run", "--output", dest, src}, "")
	code2, stdout2, stderr2 := runFlatten([]string{"--dry-run", "--output", dest, src}, "")
	if code1 != 0 || code2 != 0 {
		t.Fatalf("codes = %d, %d; stderr=%q / %q", code1, code2, stderr1, stderr2)
	}
	if stdout1 != stdout2 || stderr1 != stderr2 {
		t.Fatal("dry-run plan is not deterministic")
	}
}

func runFlatten(args []string, input string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(input), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists or could not be checked: %v", path, err)
	}
}

type actionReader struct {
	reader *strings.Reader
	action func()
}

var errConfirmationRead = errors.New("confirmation read failed")

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

type promptErrorWriter struct {
	buffer bytes.Buffer
}

func (w *promptErrorWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("진행하시겠습니까")) {
		return 0, io.ErrClosedPipe
	}
	return w.buffer.Write(p)
}

func (r *actionReader) Read(p []byte) (int, error) {
	if r.action != nil {
		action := r.action
		r.action = nil
		action()
	}
	return r.reader.Read(p)
}
