package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/useful-go/pkg/common"
)

type AuthorStats struct {
	Name      string
	Commits   int
	Additions int
	Deletions int
}

type TimeStats struct {
	Hour    [24]int
	Weekday [7]int
}

func main() {
	days := flag.Int("days", 0, "최근 N일간 통계 (0=전체)")
	author := flag.String("author", "", "특정 작성자 필터")
	top := flag.Int("top", 10, "상위 N명 표시")
	hotspots := flag.Bool("hotspots", false, "자주 변경되는 파일 표시")
	timeStats := flag.Bool("time", false, "시간대별 커밋 통계")
	flag.Parse()

	// git 저장소 확인
	if !isGitRepo() {
		common.Error("Git 저장소가 아닙니다")
		os.Exit(1)
	}

	common.Header("gitstats - Git 커밋 통계")
	fmt.Println()

	// 기본 정보
	printRepoInfo()
	fmt.Println()

	// 기여자별 통계
	printAuthorStats(*days, *author, *top)

	// 핫스팟 (자주 변경되는 파일)
	if *hotspots {
		fmt.Println()
		printHotspots(*days, 10)
	}

	// 시간대별 통계
	if *timeStats {
		fmt.Println()
		printTimeStats(*days)
	}
}

func isGitRepo() bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	err := cmd.Run()
	return err == nil
}

func printRepoInfo() {
	// 브랜치
	branch, _ := exec.Command("git", "branch", "--show-current").Output()

	// 총 커밋 수
	totalCommits, _ := exec.Command("git", "rev-list", "--count", "HEAD").Output()

	// 첫 커밋 날짜
	firstCommit, _ := exec.Command("git", "log", "--reverse", "--format=%cr", "-1").Output()

	// 마지막 커밋 날짜
	lastCommit, _ := exec.Command("git", "log", "--format=%cr", "-1").Output()

	fmt.Printf("📌 브랜치: %s\n", strings.TrimSpace(string(branch)))
	fmt.Printf("📊 총 커밋: %s\n", strings.TrimSpace(string(totalCommits)))
	fmt.Printf("🕐 첫 커밋: %s\n", strings.TrimSpace(string(firstCommit)))
	fmt.Printf("🕐 마지막 커밋: %s\n", strings.TrimSpace(string(lastCommit)))
}

func printAuthorStats(days int, filterAuthor string, top int) {
	args := []string{"log", "--format=%aN", "--shortstat"}

	if days > 0 {
		since := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
		args = append(args, "--since="+since)
	}

	if filterAuthor != "" {
		args = append(args, "--author="+filterAuthor)
	}

	cmd := exec.Command("git", args...)
	output, err := cmd.Output()
	if err != nil {
		common.Error("git log 실행 실패: %v", err)
		return
	}

	stats := parseAuthorStats(string(output))

	// 커밋 수로 정렬
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Commits > stats[j].Commits
	})

	// 상위 N명만
	if len(stats) > top {
		stats = stats[:top]
	}

	title := "기여자 통계"
	if days > 0 {
		title = fmt.Sprintf("기여자 통계 (최근 %d일)", days)
	}
	fmt.Println(title)
	fmt.Println(strings.Repeat("-", 70))
	fmt.Printf("%-25s %8s %12s %12s\n", "작성자", "커밋", "추가(+)", "삭제(-)")
	fmt.Println(strings.Repeat("-", 70))

	var totalCommits, totalAdd, totalDel int
	for _, s := range stats {
		fmt.Printf("%-25s %8d %12d %12d\n", truncate(s.Name, 25), s.Commits, s.Additions, s.Deletions)
		totalCommits += s.Commits
		totalAdd += s.Additions
		totalDel += s.Deletions
	}

	fmt.Println(strings.Repeat("-", 70))
	fmt.Printf("%-25s %8d %12d %12d\n", "합계", totalCommits, totalAdd, totalDel)
}

func parseAuthorStats(output string) []AuthorStats {
	statsMap := make(map[string]*AuthorStats)

	lines := strings.Split(output, "\n")
	var currentAuthor string

	addDelRegex := regexp.MustCompile(`(\d+) insertion|(\d+) deletion`)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// 작성자 이름 (숫자로 시작하지 않음)
		if !strings.Contains(line, "file") && !strings.Contains(line, "insertion") && !strings.Contains(line, "deletion") {
			currentAuthor = line
			if _, exists := statsMap[currentAuthor]; !exists {
				statsMap[currentAuthor] = &AuthorStats{Name: currentAuthor}
			}
			statsMap[currentAuthor].Commits++
		} else if currentAuthor != "" {
			// 통계 라인
			matches := addDelRegex.FindAllStringSubmatch(line, -1)
			for _, match := range matches {
				if match[1] != "" {
					add, _ := strconv.Atoi(match[1])
					statsMap[currentAuthor].Additions += add
				}
				if match[2] != "" {
					del, _ := strconv.Atoi(match[2])
					statsMap[currentAuthor].Deletions += del
				}
			}
		}
	}

	var result []AuthorStats
	for _, s := range statsMap {
		result = append(result, *s)
	}
	return result
}

func printHotspots(days int, top int) {
	args := []string{"log", "--format=", "--name-only"}

	if days > 0 {
		since := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
		args = append(args, "--since="+since)
	}

	cmd := exec.Command("git", args...)
	output, err := cmd.Output()
	if err != nil {
		return
	}

	fileCount := make(map[string]int)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		file := strings.TrimSpace(scanner.Text())
		if file != "" {
			fileCount[file]++
		}
	}

	// 정렬
	type fileStats struct {
		Name  string
		Count int
	}
	var files []fileStats
	for name, count := range fileCount {
		files = append(files, fileStats{name, count})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Count > files[j].Count
	})

	if len(files) > top {
		files = files[:top]
	}

	title := "🔥 핫스팟 (자주 변경되는 파일)"
	if days > 0 {
		title = fmt.Sprintf("🔥 핫스팟 - 최근 %d일", days)
	}
	fmt.Println(title)
	fmt.Println(strings.Repeat("-", 50))

	for i, f := range files {
		bar := strings.Repeat("█", min(f.Count, 20))
		fmt.Printf("%2d. %-30s %3d %s\n", i+1, truncate(f.Name, 30), f.Count, bar)
	}
}

func printTimeStats(days int) {
	// ISO 8601 형식으로 커밋 시간 가져오기
	args := []string{"log", "--format=%aI"}
	if days > 0 {
		since := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
		args = append(args, "--since="+since)
	}

	cmd := exec.Command("git", args...)
	output, _ := cmd.Output()

	hours := [24]int{}
	weekdays := [7]int{}

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// ISO 8601: 2024-01-15T22:30:45+09:00
		t, err := time.Parse(time.RFC3339, line)
		if err != nil {
			continue
		}

		hours[t.Hour()]++
		weekdays[int(t.Weekday())]++
	}

	fmt.Println("⏰ 시간대별 커밋")
	fmt.Println(strings.Repeat("-", 50))

	maxHour := 1
	for _, c := range hours {
		if c > maxHour {
			maxHour = c
		}
	}

	for h := 0; h < 24; h++ {
		barLen := (hours[h] * 30) / maxHour
		bar := strings.Repeat("█", barLen)
		fmt.Printf("%02d시 %3d %s\n", h, hours[h], bar)
	}

	fmt.Println()
	fmt.Println("📅 요일별 커밋")
	fmt.Println(strings.Repeat("-", 50))

	dayNames := []string{"일", "월", "화", "수", "목", "금", "토"}
	maxDay := 1
	for _, c := range weekdays {
		if c > maxDay {
			maxDay = c
		}
	}

	for d := 0; d < 7; d++ {
		barLen := (weekdays[d] * 30) / maxDay
		bar := strings.Repeat("█", barLen)
		fmt.Printf("%s요일 %3d %s\n", dayNames[d], weekdays[d], bar)
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
