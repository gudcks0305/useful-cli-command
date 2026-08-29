// Package rootfs provides traversal-resistant operations on an already opened
// directory tree. Callers retain ownership of the root handle and of policy
// decisions such as approval, missing roots, and removal of the root itself.
package rootfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Metadata describes regular-file bytes and the latest non-symlink mtime in a
// rooted tree. Symlinks and their targets do not contribute to either value.
type Metadata struct {
	Size   int64
	Latest time.Time
}

// Inspect reads a rooted tree without following symlinks. Every directory is
// opened as its own Root and checked against both the initial and current
// Lstat results before traversal.
func Inspect(root *os.Root) (Metadata, error) {
	if root == nil {
		return Metadata{}, errors.New("inspect nil root")
	}
	info, err := root.Lstat(".")
	if err != nil {
		return Metadata{}, fmt.Errorf("inspect root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Metadata{}, errors.New("inspect root is not a directory")
	}

	metadata := Metadata{Latest: info.ModTime()}
	entries, err := readDir(root)
	if err != nil {
		return Metadata{}, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if err := validEntryName(name); err != nil {
			return Metadata{}, err
		}
		entryInfo, err := root.Lstat(name)
		if err != nil {
			return Metadata{}, fmt.Errorf("inspect %q: %w", name, err)
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if entryInfo.ModTime().After(metadata.Latest) {
			metadata.Latest = entryInfo.ModTime()
		}
		if entryInfo.Mode().IsRegular() {
			metadata.Size += entryInfo.Size()
			continue
		}
		if !entryInfo.IsDir() {
			continue
		}

		child, err := openVerifiedDir(root, name, entryInfo)
		if err != nil {
			return Metadata{}, fmt.Errorf("inspect %q: %w", name, err)
		}
		childMetadata, inspectErr := Inspect(child)
		closeErr := child.Close()
		if err := errors.Join(inspectErr, closeErr); err != nil {
			return Metadata{}, fmt.Errorf("inspect %q: %w", name, err)
		}
		if err := verifyDir(root, name, entryInfo); err != nil {
			return Metadata{}, fmt.Errorf("inspect %q: %w", name, err)
		}
		metadata.Size += childMetadata.Size
		if childMetadata.Latest.After(metadata.Latest) {
			metadata.Latest = childMetadata.Latest
		}
	}
	return metadata, nil
}

// RemoveContents removes every entry beneath root but leaves root itself in
// place. It never descends through symlinks and rechecks entry identity before
// each removal.
func RemoveContents(root *os.Root) error {
	if root == nil {
		return errors.New("remove contents of nil root")
	}
	info, err := root.Lstat(".")
	if err != nil {
		return fmt.Errorf("remove contents root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("remove contents root is not a directory")
	}

	entries, err := readDir(root)
	if err != nil {
		return err
	}
	var joined error
	for _, entry := range entries {
		name := entry.Name()
		if err := validEntryName(name); err != nil {
			joined = errors.Join(joined, err)
			continue
		}
		if err := removeEntry(root, name); err != nil {
			joined = errors.Join(joined, fmt.Errorf("remove %q: %w", name, err))
		}
	}
	return joined
}

func readDir(root *os.Root) ([]os.DirEntry, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open directory: %w", err)
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("read directory: %w", err)
	}
	return entries, nil
}

func validEntryName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("invalid directory entry %q", name)
	}
	return nil
}

func openVerifiedDir(parent *os.Root, name string, expected os.FileInfo) (*os.Root, error) {
	if expected == nil || expected.Mode()&os.ModeSymlink != 0 || !expected.IsDir() {
		return nil, errors.New("expected entry is not a directory")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open directory root: %w", err)
	}
	opened, openedErr := child.Lstat(".")
	current, currentErr := parent.Lstat(name)
	if err := errors.Join(openedErr, currentErr); err != nil {
		return nil, errors.Join(fmt.Errorf("verify directory identity: %w", err), child.Close())
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.IsDir() ||
		!os.SameFile(expected, opened) || !os.SameFile(opened, current) {
		return nil, errors.Join(errors.New("directory identity changed"), child.Close())
	}
	return child, nil
}

func verifyDir(parent *os.Root, name string, expected os.FileInfo) error {
	current, err := parent.Lstat(name)
	if err != nil {
		return fmt.Errorf("final lstat: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(expected, current) {
		return errors.New("directory identity changed")
	}
	return nil
}

func removeEntry(parent *os.Root, name string) error {
	info, err := parent.Lstat(name)
	if err != nil {
		return fmt.Errorf("lstat: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return removeNonDirectory(parent, name, info)
	}

	child, err := openVerifiedDir(parent, name, info)
	if err != nil {
		return err
	}
	removeErr := RemoveContents(child)
	closeErr := child.Close()
	if err := errors.Join(removeErr, closeErr); err != nil {
		return err
	}
	if err := verifyDir(parent, name, info); err != nil {
		return fmt.Errorf("before remove: %w", err)
	}
	if err := parent.Remove(name); err != nil {
		return fmt.Errorf("remove directory: %w", err)
	}
	return nil
}

func removeNonDirectory(parent *os.Root, name string, expected os.FileInfo) error {
	current, err := parent.Lstat(name)
	if err != nil {
		return fmt.Errorf("final lstat: %w", err)
	}
	if current.Mode() != expected.Mode() || !os.SameFile(expected, current) {
		return errors.New("entry identity changed before remove")
	}
	if err := parent.Remove(name); err != nil {
		return fmt.Errorf("remove entry: %w", err)
	}
	return nil
}
