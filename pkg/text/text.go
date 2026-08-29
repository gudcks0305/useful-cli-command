package text

import (
	"fmt"
	"math/big"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var decimalSizePattern = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// Truncate 문자열을 지정된 최대 길이로 자릅니다.
func Truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return strings.Repeat(".", maxLen)
	}
	return string(runes[:maxLen-3]) + "..."
}

// TruncatePath 경로를 짧게 줄이고 home 디렉토리를 ~로 표시합니다.
func TruncatePath(path string, maxLen int, homeDir string) string {
	if homeDir != "" && (path == homeDir || strings.HasPrefix(path, homeDir+string(filepath.Separator))) {
		path = "~" + path[len(homeDir):]
	}
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(path)
	if len(runes) <= maxLen {
		return path
	}
	if maxLen <= 3 {
		return strings.Repeat(".", maxLen)
	}
	return "..." + string(runes[len(runes)-maxLen+3:])
}

// Separator 지정된 길이의 구분선을 생성합니다.
func Separator(length int) string {
	if length <= 0 {
		return ""
	}
	return strings.Repeat("─", length)
}

// SeparatorAuto 길이 자동 계산 구분선
func SeparatorAuto(columns ...int) string {
	length := 50
	if len(columns) > 0 {
		length = columns[0]
	}
	return Separator(length)
}

// Min 두 정수 중 작은 값을 반환합니다.
func Min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ParseSize parses size string to bytes (e.g., "100MB", "1GB").
// It returns zero for invalid input for compatibility; new code should use ParseSizeE.
func ParseSize(s string) int64 {
	value, _ := ParseSizeE(s)
	return value
}

// ParseSizeE parses a non-negative size using binary units. KB and KiB are
// aliases for 1024 bytes (likewise MB/MiB, GB/GiB, and TB/TiB). Fractional
// bytes are rounded down. Units and aliases are case-insensitive.
func ParseSizeE(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	upper := strings.ToUpper(s)
	units := []struct {
		suffix     string
		multiplier int64
	}{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
		{"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"TB", 1 << 40},
		{"B", 1},
	}
	multiplier := int64(1)
	number := s
	for _, unit := range units {
		if strings.HasSuffix(upper, unit.suffix) {
			multiplier = unit.multiplier
			number = strings.TrimSpace(s[:len(s)-len(unit.suffix)])
			break
		}
	}

	if !decimalSizePattern.MatchString(number) {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	value, ok := new(big.Rat).SetString(number)
	if !ok {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if value.Sign() < 0 {
		return 0, fmt.Errorf("size must be a finite non-negative number")
	}
	value.Mul(value, new(big.Rat).SetInt64(multiplier))
	bytes := new(big.Int).Quo(value.Num(), value.Denom())
	if !bytes.IsInt64() {
		return 0, fmt.Errorf("size exceeds int64")
	}
	return bytes.Int64(), nil
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
