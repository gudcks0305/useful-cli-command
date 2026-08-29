//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

func platformSupported() bool { return true }

func scopePaths(scope string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	paths := []string{filepath.Join(home, "Library", "LaunchAgents")}
	if scope == "local" || scope == "all" {
		paths = append(paths, "/Library/LaunchAgents", "/Library/LaunchDaemons")
	}
	if scope == "all" {
		paths = append(paths, "/System/Library/LaunchAgents", "/System/Library/LaunchDaemons")
	}
	return paths, nil
}

func fileOwnerUID(info os.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

func expectedOwner(path string) (uint32, bool) {
	clean := filepath.Clean(path)
	if clean == "/Library/LaunchAgents" || clean == "/Library/LaunchDaemons" ||
		filepath.Dir(clean) == "/Library/LaunchAgents" || filepath.Dir(clean) == "/Library/LaunchDaemons" ||
		filepath.Dir(clean) == "/System/Library/LaunchAgents" || filepath.Dir(clean) == "/System/Library/LaunchDaemons" {
		return 0, true
	}
	if filepath.Base(filepath.Dir(clean)) == "LaunchAgents" && filepath.Base(filepath.Dir(filepath.Dir(clean))) == "Library" {
		return uint32(os.Getuid()), true
	}
	return 0, false
}

func isRootDaemonPlist(path string) bool {
	dir := filepath.Clean(filepath.Dir(path))
	return dir == "/Library/LaunchDaemons" || dir == "/System/Library/LaunchDaemons"
}
