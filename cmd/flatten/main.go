package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/useful-go/pkg/common"
)

var numberRegex = regexp.MustCompile(`(\d+)`)

func main() {
	dryRun := flag.Bool("dry-run", false, "실제 이동 없이 결과만 미리보기")
	output := flag.String("output", "", "출력 폴더 (미지정시 현재 폴더에 덮어쓰기)")
	separator := flag.String("sep", "_", "폴더명과 파일명 사이 구분자")
	padding := flag.Int("pad", 0, "숫자 패딩 자릿수 (0=자동 계산)")
	flag.Parse()

	if flag.NArg() < 1 {
		common.Error("대상 폴더를 지정해주세요")
		fmt.Println("사용법: flatten [options] <folder>")
		fmt.Println("옵션:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	srcDir := flag.Arg(0)
	if _, err := os.Stat(srcDir); os.IsNotExist(err) {
		common.Fatal("폴더가 존재하지 않습니다: %s", srcDir)
	}

	destDir := *output
	if destDir == "" {
		destDir = srcDir + "_flattened"
	}

	files, err := collectFiles(srcDir)
	if err != nil {
		common.Fatal("파일 수집 실패: %v", err)
	}

	if len(files) == 0 {
		common.Warning("처리할 파일이 없습니다")
		return
	}

	padWidth := *padding
	if padWidth == 0 {
		padWidth = calculatePadding(files)
	}

	operations := planOperations(files, srcDir, destDir, *separator, padWidth)

	common.Header("📁 Flatten 작업 계획")
	fmt.Printf("원본: %s\n", srcDir)
	fmt.Printf("대상: %s\n", destDir)
	fmt.Printf("파일 수: %d\n", len(operations))
	fmt.Printf("숫자 패딩: %d자리\n", padWidth)
	fmt.Println()

	sort.Slice(operations, func(i, j int) bool {
		return naturalLess(operations[i].NewName, operations[j].NewName)
	})

	for _, op := range operations {
		fmt.Printf("  %s → %s\n", op.RelPath, op.NewName)
	}

	if *dryRun {
		fmt.Println()
		common.Info("Dry-run 모드: 실제 파일 이동 없음")
		return
	}

	fmt.Print("\n진행하시겠습니까? (y/N): ")
	var answer string
	fmt.Scanln(&answer)
	if strings.ToLower(answer) != "y" {
		common.Info("취소되었습니다")
		return
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		common.Fatal("출력 폴더 생성 실패: %v", err)
	}

	var success, failed int
	for _, op := range operations {
		destPath := filepath.Join(destDir, op.NewName)
		if err := copyFile(op.SrcPath, destPath); err != nil {
			common.Error("%s: %v", op.NewName, err)
			failed++
		} else {
			success++
		}
	}

	fmt.Println()
	common.Success("완료: %d개 성공, %d개 실패", success, failed)
}

type FileInfo struct {
	SrcPath  string
	RelPath  string
	FileName string
	DirPath  string
}

type Operation struct {
	SrcPath string
	RelPath string
	NewName string
}

func collectFiles(root string) ([]FileInfo, error) {
	var files []FileInfo

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}

		relPath, _ := filepath.Rel(root, path)
		dirPath := filepath.Dir(relPath)

		files = append(files, FileInfo{
			SrcPath:  path,
			RelPath:  relPath,
			FileName: info.Name(),
			DirPath:  dirPath,
		})
		return nil
	})

	return files, err
}

func calculatePadding(files []FileInfo) int {
	maxNum := 0
	for _, f := range files {
		nameWithoutExt := strings.TrimSuffix(f.FileName, filepath.Ext(f.FileName))
		matches := numberRegex.FindAllString(nameWithoutExt, -1)
		for _, m := range matches {
			if n, err := strconv.Atoi(m); err == nil && n > maxNum {
				maxNum = n
			}
		}
	}

	if maxNum == 0 {
		return 2
	}
	padWidth := len(strconv.Itoa(maxNum))
	if padWidth < 2 {
		return 2
	}
	return padWidth
}

func planOperations(files []FileInfo, srcDir, destDir, sep string, padWidth int) []Operation {
	var ops []Operation

	for _, f := range files {
		var newName string

		if f.DirPath == "." {
			newName = padNumbers(f.FileName, padWidth)
		} else {
			dirPart := strings.ReplaceAll(f.DirPath, string(os.PathSeparator), sep)
			paddedFile := padNumbers(f.FileName, padWidth)
			newName = dirPart + sep + paddedFile
		}

		ops = append(ops, Operation{
			SrcPath: f.SrcPath,
			RelPath: f.RelPath,
			NewName: newName,
		})
	}

	return ops
}

func padNumbers(s string, width int) string {
	if width == 0 {
		return s
	}

	ext := filepath.Ext(s)
	nameWithoutExt := strings.TrimSuffix(s, ext)

	padded := numberRegex.ReplaceAllStringFunc(nameWithoutExt, func(match string) string {
		n, _ := strconv.Atoi(match)
		return fmt.Sprintf("%0*d", width, n)
	})

	return padded + ext
}

func naturalLess(a, b string) bool {
	partsA := splitByNumbers(a)
	partsB := splitByNumbers(b)

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

func splitByNumbers(s string) []string {
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

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	return os.WriteFile(dst, data, info.Mode())
}
