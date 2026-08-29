package rootfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInspectNestedTree(t *testing.T) {
	path := t.TempDir()
	nested := filepath.Join(path, "one", "two")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "root.bin"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	latest := time.Now().Add(-time.Hour).Truncate(time.Second)
	leaf := filepath.Join(nested, "leaf.bin")
	if err := os.WriteFile(leaf, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(leaf, latest, latest); err != nil {
		t.Fatal(err)
	}

	root := openTestRoot(t, path)
	metadata, err := Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Size != 8 {
		t.Fatalf("size = %d, want 8", metadata.Size)
	}
	if metadata.Latest.Before(latest) {
		t.Fatalf("latest = %v, want >= %v", metadata.Latest, latest)
	}
	if err := RemoveContents(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("remaining nested-tree entries: %v", entries)
	}
}

func TestInspectAndRemoveContentsNeverFollowSymlink(t *testing.T) {
	path := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "inside"), []byte("in"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(path, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(24 * time.Hour)
	if err := os.Chtimes(outside, future, future); err != nil {
		t.Fatal(err)
	}

	root := openTestRoot(t, path)
	metadata, err := Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Size != 2 {
		t.Fatalf("size = %d, want 2", metadata.Size)
	}
	if !metadata.Latest.Before(future) {
		t.Fatalf("latest = %v; symlink target mtime was followed", metadata.Latest)
	}
	if err := RemoveContents(root); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "outside" {
		t.Fatalf("outside sentinel = %q, %v", got, err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("remaining entries: %v", entries)
	}
}

func TestOpenVerifiedDirRejectsDirectorySwap(t *testing.T) {
	path := t.TempDir()
	victim := filepath.Join(path, "victim")
	if err := os.Mkdir(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	root := openTestRoot(t, path)
	original, err := root.Lstat("victim")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(victim, victim+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	if child, err := openVerifiedDir(root, "victim", original); err == nil {
		child.Close()
		t.Fatal("swapped directory identity accepted")
	}
}

func TestClosedRootErrors(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(root); err == nil {
		t.Fatal("Inspect accepted closed root")
	}
	if err := RemoveContents(root); err == nil {
		t.Fatal("RemoveContents accepted closed root")
	}
	if _, err := Inspect(nil); err == nil {
		t.Fatal("Inspect accepted nil root")
	}
	if err := RemoveContents(nil); err == nil {
		t.Fatal("RemoveContents accepted nil root")
	}
}

func TestRemoveContentsReportsEntryError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission test is not meaningful as root")
	}
	path := t.TempDir()
	locked := filepath.Join(path, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "keep"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	root := openTestRoot(t, path)
	err := RemoveContents(root)
	if err == nil {
		t.Fatal("permission error was discarded")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Logf("platform error is not os.ErrPermission: %v", err)
	}
}

func openTestRoot(t *testing.T, path string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("close root: %v", err)
		}
	})
	return root
}
