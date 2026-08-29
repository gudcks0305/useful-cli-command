package fs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGetDirSizeE(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	size, err := GetDirSizeE(root)
	if err != nil || size != 4 {
		t.Fatalf("GetDirSizeE = %d, %v", size, err)
	}
	if _, err := GetDirSizeE(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing root did not report error")
	}
}

func TestExpandPathE(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ExpandPathE("~/folder")
	if err != nil || got != filepath.Join(home, "folder") {
		t.Fatalf("ExpandPathE = %q, %v", got, err)
	}
}

func TestGetDirSizeERejectsSymlinkRoot(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := GetDirSizeE(link); err == nil {
		t.Fatal("symlink root accepted")
	}
}

func TestWalkWithDepth(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "one", "two"), 0o700); err != nil {
		t.Fatal(err)
	}
	beyondLimit := filepath.Join(root, "one", "two", "hidden.txt")
	if err := os.WriteFile(beyondLimit, []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	var depths []int
	var visited []string
	if err := WalkWithDepth(root, 2, func(path string, depth int) error {
		depths = append(depths, depth)
		visited = append(visited, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []int{0, 1, 2}; !reflect.DeepEqual(depths, want) {
		t.Fatalf("depths = %v, want %v", depths, want)
	}
	for _, path := range visited {
		if path == beyondLimit {
			t.Fatalf("file beyond max depth was visited: %s", path)
		}
	}
}

func TestWalkWithDepthRejectsNegativeLimit(t *testing.T) {
	called := false
	err := WalkWithDepth(t.TempDir(), -1, func(string, int) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("negative max depth succeeded")
	}
	if called {
		t.Fatal("callback called for invalid max depth")
	}
}
