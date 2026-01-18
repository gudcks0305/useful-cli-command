package text

import (
	"fmt"
	"strconv"
	"strings"
)

// Truncate 문자열을 지정된 최대 길이로 자릅니다.
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// TruncatePath 경로를 짧게 줄이고 home 디렉토리를 ~로 표시합니다.
func TruncatePath(path string, maxLen int, homeDir string) string {
	if strings.HasPrefix(path, homeDir) {
		path = "~" + path[len(homeDir):]
	}
	if len(path) <= maxLen {
		return path
	}
	return "..." + path[len(path)-maxLen+3:]
}

// Separator 지정된 길이의 구분선을 생성합니다.
func Separator(length int) string {
	return strings.Repeat("─", length)
}

// SeparatorAuto 길이 자동 계산 구분선
func SeparatorAuto(columns ...int) string {
	length := 50
	if len(columns) > 0 {
		length = columns[0]
	}
	return strings.Repeat("─", length)
}

// Min 두 정수 중 작은 값을 반환합니다.
func Min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ParseSize parses size string to bytes (e.g., "100MB", "1GB").
func ParseSize(s string) int64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "0" || s == "" {
		return 0
	}

	multiplier := int64(1)
	if strings.HasSuffix(s, "GB") {
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	} else if strings.HasSuffix(s, "MB") {
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	} else if strings.HasSuffix(s, "KB") {
		multiplier = 1024
		s = strings.TrimSuffix(s, "KB")
	}

	var value float64
	fmt.Sscanf(strings.TrimSpace(s), "%f", &value)
	return int64(value * float64(multiplier))
}

// SplitByNumbers 문자열을 숫자와 비숫자 부분으로 분리합니다.
func SplitByNumbers(s string) []string {
	var parts []string
	var current strings.Builder
	var inNumber bool

	for _, r := range s {
		isDigit := r >= '0' && r <= '9'
		if current.Len() > 0 && isDigit != inNumber {
			parts = append(parts, current.String())
			current.Reset()
		}
		current.WriteRune(r)
		inNumber = isDigit
	}

	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// NaturalLess 자연 정렬 비교 (숫자 포함 문자열 정렬용)
func NaturalLess(a, b string) bool {
	partsA := SplitByNumbers(a)
	partsB := SplitByNumbers(b)

	for i := 0; i < len(partsA) && i < len(partsB); i++ {
		if partsA[i] != partsB[i] {
			numA, errA := strconv.Atoi(partsA[i])
			numB, errB := strconv.Atoi(partsB[i])

			if errA == nil && errB == nil {
				return numA < numB
			}
			return partsA[i] < partsB[i]
		}
	}
	return len(partsA) < len(partsB)
}
