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
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

// GetDirSize 디렉토리 전체 크기 계산
func GetDirSize(path string) int64 {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return 0
	}

	var size int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})

	return size
}

// IsDirExists 디렉토리 존재 여부 확인
func IsDirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// WalkWithDepth 제한된 깊이로 디렉토리 순회
func WalkWithDepth(root string, maxDepth int, fn func(path string, depth int) error) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		relPath, _ := filepath.Rel(root, path)
		depth := strings.Count(relPath, string(filepath.Separator))

		if depth > maxDepth && info.IsDir() {
			return filepath.SkipDir
		}

		return fn(path, depth)
	})
}
