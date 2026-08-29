package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/useful-go/pkg/common"
	"github.com/useful-go/pkg/text"
)

const (
	gitTimeout               = 30 * time.Second
	defaultGitStatsOutputMax = 64 << 20
)

var errNotGitRepo = errors.New("Git 저장소가 아닙니다")
var errGitStatsOutputLimit = errors.New("Git 출력이 64 MiB 제한을 초과했습니다")

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

type repoInfo struct {
	Branch      string
	Commits     int
	FirstCommit string
	LastCommit  string
}

type fileStats struct {
	Name  string
	Count int
}

type options struct {
	days      int
	author    string
	top       int
	hotspots  bool
	timeStats bool
}

type gitRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

type execGitRunner struct {
	dir        string
	maxOutput  int
	executable string
}

func (runner execGitRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	executable := runner.executable
	if executable == "" {
		executable = "git"
	}
	cmd := exec.CommandContext(ctx, executable, hardenedGitStatsArgs(args)...)
	cmd.Dir = runner.dir
	cmd.Env = hardenedGitStatsEnv(os.Environ())
	limit := runner.maxOutput
	if limit <= 0 {
		limit = defaultGitStatsOutputMax
	}
	var out limitedGitStatsBuffer
	out.limit = limit
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if out.truncated {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), errGitStatsOutputLimit)
	}
	if err != nil {
		detail := strings.TrimSpace(out.String())
		if detail != "" {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out.Bytes(), nil
}

type limitedGitStatsBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *limitedGitStatsBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		if remaining > len(data) {
			remaining = len(data)
		}
		_, _ = buffer.buffer.Write(data[:remaining])
	}
	if remaining < len(data) {
		buffer.truncated = true
	}
	return written, nil
}

func (buffer *limitedGitStatsBuffer) Bytes() []byte {
	return buffer.buffer.Bytes()
}

func (buffer *limitedGitStatsBuffer) String() string {
	return buffer.buffer.String()
}

func hardenedGitStatsArgs(args []string) []string {
	hardened := []string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + os.DevNull,
	}
	return append(hardened, args...)
}

func hardenedGitStatsEnv(env []string) []string {
	blocked := map[string]bool{
		"GIT_CONFIG":                       true,
		"GIT_CONFIG_COUNT":                 true,
		"GIT_CONFIG_GLOBAL":                true,
		"GIT_CONFIG_NOSYSTEM":              true,
		"GIT_CONFIG_PARAMETERS":            true,
		"GIT_CONFIG_SYSTEM":                true,
		"GIT_DIR":                          true,
		"GIT_WORK_TREE":                    true,
		"GIT_COMMON_DIR":                   true,
		"GIT_INDEX_FILE":                   true,
		"GIT_OBJECT_DIRECTORY":             true,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_OPTIONAL_LOCKS":               true,
		"LC_ALL":                           true,
	}
	result := make([]string, 0, len(env)+5)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if blocked[key] || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		result = append(result, entry)
	}
	return append(result,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
	)
}

type app struct {
	git gitRunner
	out io.Writer
	now func() time.Time
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	a := app{git: execGitRunner{}, out: os.Stdout, now: time.Now}
	if err := a.run(ctx, os.Args[1:]); err != nil {
		common.Error("%v", err)
		os.Exit(1)
	}
}

func parseOptions(args []string, output io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("gitstats", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintln(output, "Usage: gitstats [options]")
		fs.PrintDefaults()
	}
	fs.IntVar(&opts.days, "days", 0, "최근 N일간 통계 (0=전체)")
	fs.StringVar(&opts.author, "author", "", "특정 작성자 필터")
	fs.IntVar(&opts.top, "top", 10, "상위 N명 표시")
	fs.BoolVar(&opts.hotspots, "hotspots", false, "자주 변경되는 파일 표시")
	fs.BoolVar(&opts.timeStats, "time", false, "시간대별 커밋 통계")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("예상하지 못한 인수: %s", strings.Join(fs.Args(), " "))
	}
	if opts.days < 0 {
		return options{}, errors.New("days는 0 이상이어야 합니다")
	}
	if opts.top < 0 {
		return options{}, errors.New("top은 0 이상이어야 합니다")
	}
	return opts, nil
}

func (a app) run(ctx context.Context, args []string) error {
	opts, err := parseOptions(args, a.out)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := a.checkRepo(ctx); err != nil {
		return err
	}

	info, err := a.loadRepoInfo(ctx)
	if err != nil {
		return err
	}
	var authors []AuthorStats
	if info.Commits > 0 {
		authors, err = a.loadAuthorStats(ctx, opts)
		if err != nil {
			return err
		}
	}

	var hotspots []fileStats
	if opts.hotspots && info.Commits > 0 {
		hotspots, err = a.loadHotspots(ctx, opts.days, 10)
		if err != nil {
			return err
		}
	}

	var times TimeStats
	if opts.timeStats && info.Commits > 0 {
		times, err = a.loadTimeStats(ctx, opts.days)
		if err != nil {
			return err
		}
	}

	a.printReport(info, authors, hotspots, times, opts)
	return nil
}

func (a app) checkRepo(ctx context.Context) error {
	out, err := a.git.Run(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return fmt.Errorf("%w: %v", errNotGitRepo, err)
	}
	if strings.TrimSpace(string(out)) != "true" {
		return errNotGitRepo
	}
	return nil
}

func (a app) loadRepoInfo(ctx context.Context) (repoInfo, error) {
	branch, err := a.git.Run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		branch, err = a.git.Run(ctx, "rev-parse", "--short", "HEAD")
		if err != nil {
			return repoInfo{}, fmt.Errorf("브랜치 조회 실패: %w", err)
		}
		branch = []byte("detached@" + strings.TrimSpace(string(branch)))
	}
	status, err := a.git.Run(ctx, "status", "--porcelain=v2", "--branch")
	if err != nil {
		return repoInfo{}, fmt.Errorf("저장소 상태 조회 실패: %w", err)
	}
	if isUnbornStatus(string(status)) {
		return repoInfo{
			Branch:      strings.TrimSpace(string(branch)),
			Commits:     0,
			FirstCommit: "없음",
			LastCommit:  "없음",
		}, nil
	}
	countOutput, err := a.git.Run(ctx, "rev-list", "--count", "HEAD")
	if err != nil {
		return repoInfo{}, fmt.Errorf("커밋 수 조회 실패: %w", err)
	}
	commits, err := strconv.Atoi(strings.TrimSpace(string(countOutput)))
	if err != nil {
		return repoInfo{}, fmt.Errorf("잘못된 커밋 수 %q: %w", strings.TrimSpace(string(countOutput)), err)
	}
	datesOutput, err := a.git.Run(ctx, "log", "--format=%aI", "--reverse", "HEAD")
	if err != nil {
		return repoInfo{}, fmt.Errorf("커밋 날짜 조회 실패: %w", err)
	}
	dates := nonEmptyLines(string(datesOutput))
	if len(dates) == 0 {
		return repoInfo{}, errors.New("커밋 날짜 조회 결과가 비어 있습니다")
	}
	for _, date := range dates {
		if _, err := time.Parse(time.RFC3339, date); err != nil {
			return repoInfo{}, fmt.Errorf("잘못된 커밋 날짜 %q: %w", date, err)
		}
	}
	return repoInfo{
		Branch:      strings.TrimSpace(string(branch)),
		Commits:     commits,
		FirstCommit: dates[0],
		LastCommit:  dates[len(dates)-1],
	}, nil
}

func isUnbornStatus(status string) bool {
	for _, line := range strings.Split(status, "\n") {
		if strings.TrimSpace(line) == "# branch.oid (initial)" {
			return true
		}
	}
	return false
}

func (a app) loadAuthorStats(ctx context.Context, opts options) ([]AuthorStats, error) {
	args := []string{"log", "--format=%x1e%aN%x00", "--numstat", "-z"}
	args = appendFilters(args, opts.days, opts.author, a.now())
	out, err := a.git.Run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("기여자 통계 조회 실패: %w", err)
	}
	stats, err := parseAuthorStats(out)
	if err != nil {
		return nil, fmt.Errorf("기여자 통계 해석 실패: %w", err)
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Commits != stats[j].Commits {
			return stats[i].Commits > stats[j].Commits
		}
		return stats[i].Name < stats[j].Name
	})
	if len(stats) > opts.top {
		stats = stats[:opts.top]
	}
	return stats, nil
}

func appendFilters(args []string, days int, author string, now time.Time) []string {
	if days > 0 {
		since := now.Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
		args = append(args, "--since="+since)
	}
	if author != "" {
		args = append(args, "--author="+author)
	}
	return args
}

func parseAuthorStats(output []byte) ([]AuthorStats, error) {
	statsMap := make(map[string]*AuthorStats)
	for _, record := range strings.Split(string(output), "\x1e") {
		if record == "" {
			continue
		}
		nameEnd := strings.IndexByte(record, 0)
		if nameEnd < 0 {
			return nil, errors.New("작성자 레코드 구분자 누락")
		}
		name := record[:nameEnd]
		if name == "" {
			return nil, errors.New("작성자 이름이 비어 있습니다")
		}
		stats := statsMap[name]
		if stats == nil {
			stats = &AuthorStats{Name: name}
			statsMap[name] = stats
		}
		stats.Commits++

		for _, entry := range strings.Split(record[nameEnd+1:], "\x00") {
			entry = strings.TrimPrefix(entry, "\n")
			if entry == "" {
				continue
			}
			fields := strings.SplitN(entry, "\t", 3)
			if len(fields) < 3 {
				continue
			}
			if fields[0] != "-" {
				additions, err := strconv.Atoi(fields[0])
				if err != nil {
					return nil, fmt.Errorf("잘못된 추가 줄 수 %q: %w", fields[0], err)
				}
				stats.Additions += additions
			}
			if fields[1] != "-" {
				deletions, err := strconv.Atoi(fields[1])
				if err != nil {
					return nil, fmt.Errorf("잘못된 삭제 줄 수 %q: %w", fields[1], err)
				}
				stats.Deletions += deletions
			}
		}
	}

	result := make([]AuthorStats, 0, len(statsMap))
	for _, stats := range statsMap {
		result = append(result, *stats)
	}
	return result, nil
}

func (a app) loadHotspots(ctx context.Context, days, top int) ([]fileStats, error) {
	args := []string{"log", "--format=", "--name-only", "-z"}
	args = appendFilters(args, days, "", a.now())
	out, err := a.git.Run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("핫스팟 조회 실패: %w", err)
	}
	files := parseHotspots(out)
	sort.Slice(files, func(i, j int) bool {
		if files[i].Count != files[j].Count {
			return files[i].Count > files[j].Count
		}
		return files[i].Name < files[j].Name
	})
	if len(files) > top {
		files = files[:top]
	}
	return files, nil
}

func parseHotspots(output []byte) []fileStats {
	counts := make(map[string]int)
	for _, raw := range strings.Split(string(output), "\x00") {
		if raw != "" {
			counts[raw]++
		}
	}
	files := make([]fileStats, 0, len(counts))
	for name, count := range counts {
		files = append(files, fileStats{Name: name, Count: count})
	}
	return files
}

func (a app) loadTimeStats(ctx context.Context, days int) (TimeStats, error) {
	args := []string{"log", "--format=%aI"}
	args = appendFilters(args, days, "", a.now())
	out, err := a.git.Run(ctx, args...)
	if err != nil {
		return TimeStats{}, fmt.Errorf("시간대별 통계 조회 실패: %w", err)
	}
	return parseTimeStats(string(out))
}

func parseTimeStats(output string) (TimeStats, error) {
	var stats TimeStats
	for _, line := range nonEmptyLines(output) {
		commitTime, err := time.Parse(time.RFC3339, line)
		if err != nil {
			return TimeStats{}, fmt.Errorf("잘못된 커밋 날짜 %q: %w", line, err)
		}
		stats.Hour[commitTime.Hour()]++
		stats.Weekday[int(commitTime.Weekday())]++
	}
	return stats, nil
}

func nonEmptyLines(output string) []string {
	var result []string
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}

func (a app) printReport(info repoInfo, authors []AuthorStats, hotspots []fileStats, times TimeStats, opts options) {
	fmt.Fprintln(a.out, "gitstats - Git 커밋 통계")
	fmt.Fprintln(a.out)
	fmt.Fprintf(a.out, "📌 브랜치: %s\n", info.Branch)
	fmt.Fprintf(a.out, "📊 총 커밋: %d\n", info.Commits)
	fmt.Fprintf(a.out, "🕐 첫 커밋: %s\n", info.FirstCommit)
	fmt.Fprintf(a.out, "🕐 마지막 커밋: %s\n", info.LastCommit)
	fmt.Fprintln(a.out)
	a.printAuthorStats(authors, opts.days)
	if opts.hotspots {
		fmt.Fprintln(a.out)
		a.printHotspots(hotspots, opts.days)
	}
	if opts.timeStats {
		fmt.Fprintln(a.out)
		a.printTimeStats(times)
	}
}

func (a app) printAuthorStats(stats []AuthorStats, days int) {
	title := "기여자 통계"
	if days > 0 {
		title = fmt.Sprintf("기여자 통계 (최근 %d일)", days)
	}
	fmt.Fprintln(a.out, title)
	fmt.Fprintln(a.out, text.Separator(70))
	fmt.Fprintf(a.out, "%-25s %8s %12s %12s\n", "작성자", "커밋", "추가(+)", "삭제(-)")
	fmt.Fprintln(a.out, text.Separator(70))

	var totalCommits, totalAdd, totalDel int
	for _, stats := range stats {
		fmt.Fprintf(a.out, "%-25s %8d %12d %12d\n", text.Truncate(displayLabel(stats.Name), 25), stats.Commits, stats.Additions, stats.Deletions)
		totalCommits += stats.Commits
		totalAdd += stats.Additions
		totalDel += stats.Deletions
	}
	fmt.Fprintln(a.out, text.Separator(70))
	fmt.Fprintf(a.out, "%-25s %8d %12d %12d\n", "합계", totalCommits, totalAdd, totalDel)
}

func (a app) printHotspots(files []fileStats, days int) {
	title := "🔥 핫스팟 (자주 변경되는 파일)"
	if days > 0 {
		title = fmt.Sprintf("🔥 핫스팟 - 최근 %d일", days)
	}
	fmt.Fprintln(a.out, title)
	fmt.Fprintln(a.out, text.Separator(50))
	for i, file := range files {
		bar := strings.Repeat("█", text.Min(file.Count, 20))
		fmt.Fprintf(a.out, "%2d. %-30s %3d %s\n", i+1, text.Truncate(displayLabel(file.Name), 30), file.Count, bar)
	}
}

func displayLabel(value string) string {
	if strings.ContainsAny(value, "\r\n\t") {
		return strconv.QuoteToGraphic(value)
	}
	return value
}

func (a app) printTimeStats(stats TimeStats) {
	fmt.Fprintln(a.out, "⏰ 시간대별 커밋")
	fmt.Fprintln(a.out, text.Separator(50))
	maxHour := maxCount(stats.Hour[:])
	for hour, count := range stats.Hour {
		bar := strings.Repeat("█", (count*30)/maxHour)
		fmt.Fprintf(a.out, "%02d시 %3d %s\n", hour, count, bar)
	}

	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "📅 요일별 커밋")
	fmt.Fprintln(a.out, text.Separator(50))
	dayNames := []string{"일", "월", "화", "수", "목", "금", "토"}
	maxDay := maxCount(stats.Weekday[:])
	for day, count := range stats.Weekday {
		bar := strings.Repeat("█", (count*30)/maxDay)
		fmt.Fprintf(a.out, "%s요일 %3d %s\n", dayNames[day], count, bar)
	}
}

func maxCount(counts []int) int {
	maximum := 1
	for _, count := range counts {
		if count > maximum {
			maximum = count
		}
	}
	return maximum
}
