package fs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FormatSize human-readable 파일 크기 반환
func FormatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// ExpandPath ~를 home 디렉토리로 변환
func ExpandPath(path string) string {
	expanded, err := ExpandPathE(path)
	if err != nil {
		return path
	}
	return expanded
}

// ExpandPathE expands a home-relative path and reports home lookup errors.
func ExpandPathE(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

// GetDirSize 디렉토리 전체 크기 계산. 오류 시 기존 호환성을 위해 0을 반환합니다.
// 새 코드는 GetDirSizeE를 사용해 오류를 확인해야 합니다.
func GetDirSize(path string) int64 {
	size, _ := GetDirSizeE(path)
	return size
}

// GetDirSizeE calculates a directory size and reports traversal errors.
// A symlink passed as the root is rejected instead of being treated as a file.
func GetDirSizeE(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("root path is a symlink: %s", path)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("root path is not a directory: %s", path)
	}

	var size int64
	err = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})

	return size, err
}

// IsDirExists 디렉토리 존재 여부 확인
func IsDirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// WalkWithDepth 제한된 깊이로 디렉토리 순회
func WalkWithDepth(root string, maxDepth int, fn func(path string, depth int) error) error {
	if maxDepth < 0 {
		return fmt.Errorf("max depth must be non-negative: %d", maxDepth)
	}
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		depth := 0
		if relPath != "." {
			depth = strings.Count(relPath, string(filepath.Separator)) + 1
		}

		if depth > maxDepth {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if err := fn(path, depth); err != nil {
			return err
		}
		if info.IsDir() && depth == maxDepth {
			return filepath.SkipDir
		}
		return nil
	})
}
