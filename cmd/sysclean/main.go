package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/useful-go/pkg/common"
	"github.com/useful-go/pkg/fs"
	"github.com/useful-go/pkg/text"
	"github.com/useful-go/pkg/ui"
)

type options struct {
	dryRun, includeAll, docker, selectMode, yes, list bool
	only, projects                                    string
	workers, projectAge, depth                        int
}

func main() {
	opts := parseFlags()
	if opts.workers < 0 || opts.projectAge < 0 || opts.depth < 1 {
		common.Fatal("--workers/--project-days는 0 이상, --depth는 1 이상이어야 합니다")
	}
	if opts.list {
		printTargetList(defaultTargets)
		return
	}

	common.Header("sysclean - macOS 개발 데이터 정리")
	fmt.Println()
	if opts.dryRun {
		common.Info("분석 모드 (삭제 없음)")
	}
	targets, err := selectConfiguredTargets(defaultTargets, opts)
	if err != nil {
		common.Fatal("대상 선택 실패: %v", err)
	}
	if opts.projects != "" {
		root := fs.ExpandPath(opts.projects)
		common.Info("프로젝트 탐색: %s (%d일 이상 미수정)", root, opts.projectAge)
		artifacts, scanErr := scanProjectArtifacts(root, opts.depth, opts.projectAge, opts.workers)
		if scanErr != nil {
			common.Warning("프로젝트 탐색 일부 실패: %v", scanErr)
		}
		targets = append(targets, artifactTargets(artifacts)...)
	}

	workers := normalizedWorkers(opts.workers, len(targets))
	common.Info("병렬 작업자: %d", workers)
	fmt.Println()
	results := analyzeTargets(targets, workers)
	printAnalysis(results)
	available := nonEmptyResults(results)
	if len(available) == 0 {
		common.Success("정리할 데이터가 없습니다")
		return
	}
	if opts.dryRun {
		common.Info("삭제하려면 --dry-run을 제거하세요")
		return
	}

	if opts.selectMode || containsProjectArtifact(available) {
		available, err = promptSelection(available)
		if err != nil {
			common.Fatal("선택 입력 실패: %v", err)
		}
		if len(available) == 0 {
			common.Info("선택된 대상이 없습니다")
			return
		}
		fmt.Println()
		printAnalysis(available)
	}
	if !opts.yes {
		confirm := ui.YesNoConfirmation("선택 항목을 삭제하시겠습니까?")
		if !confirm.MustConfirm() {
			return
		}
	}

	fmt.Println()
	cleaned := cleanTargets(available, workers)
	var failed int
	var reclaimed int64
	for _, result := range cleaned {
		if result.Error != nil {
			failed++
			common.Error("%s 실패: %v", result.Target.Name, result.Error)
		} else {
			reclaimed += result.Size
			common.Success("%s 완료 (%s)", result.Target.Name, fs.FormatSize(result.Size))
		}
	}
	fmt.Println()
	common.Success("완료: %s 정리, 실패 %d개", fs.FormatSize(reclaimed), failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func parseFlags() options {
	var o options
	flag.BoolVar(&o.dryRun, "dry-run", false, "실제 삭제 없이 분석만 수행")
	flag.BoolVar(&o.includeAll, "all", false, "주의가 필요한 선택 대상 포함")
	flag.BoolVar(&o.docker, "docker", false, "Docker unused data 포함 (volume 제외)")
	flag.BoolVar(&o.selectMode, "select", false, "분석 후 번호로 삭제 대상 선택")
	flag.BoolVar(&o.yes, "yes", false, "최종 확인 생략")
	flag.BoolVar(&o.list, "list", false, "지원 대상 ID 목록 출력")
	flag.StringVar(&o.only, "only", "", "쉼표로 지정한 대상 ID만 분석/정리")
	flag.IntVar(&o.workers, "workers", 0, "병렬 작업자 수 (0: 자동)")
	flag.StringVar(&o.projects, "projects", "", "오래된 프로젝트 산출물을 찾을 루트")
	flag.IntVar(&o.projectAge, "project-days", 30, "프로젝트 산출물 최소 미수정 일수")
	flag.IntVar(&o.depth, "depth", 6, "프로젝트 탐색 최대 깊이")
	flag.Parse()
	return o
}

func selectConfiguredTargets(targets []CleanTarget, o options) ([]CleanTarget, error) {
	if strings.TrimSpace(o.only) != "" {
		byID := make(map[string]CleanTarget, len(targets))
		for _, target := range targets {
			byID[target.ID] = target
		}
		seen := map[string]bool{}
		var selected []CleanTarget
		for _, raw := range strings.Split(o.only, ",") {
			id := strings.TrimSpace(raw)
			target, ok := byID[id]
			if !ok {
				return nil, fmt.Errorf("알 수 없는 ID %q (--list로 확인)", id)
			}
			if !seen[id] {
				selected = append(selected, target)
				seen[id] = true
			}
		}
		return selected, nil
	}
	var selected []CleanTarget
	for _, target := range targets {
		if target.Risk == riskSafe || target.Risk == riskOptional && o.includeAll || target.Kind == kindDocker && o.docker {
			selected = append(selected, target)
		}
	}
	return selected, nil
}

func printTargetList(targets []CleanTarget) {
	fmt.Printf("%-24s %-10s %s\n", "ID", "활성화", "설명")
	fmt.Println(text.Separator(80))
	for _, target := range targets {
		mode := "--only"
		if target.Risk == riskSafe {
			mode = "기본"
		} else if target.Risk == riskOptional {
			mode = "--all"
		} else if target.Kind == kindDocker {
			mode = "--docker"
		}
		fmt.Printf("%-24s %-10s %s\n", target.ID, mode, target.Description)
	}
}

func printAnalysis(results []AnalysisResult) {
	fmt.Println("정리 대상:")
	fmt.Println(text.Separator(98))
	fmt.Printf("%-3s %-26s %-12s %-9s %s\n", "#", "대상", "크기", "등급", "경로/설명")
	fmt.Println(text.Separator(98))
	var total int64
	shown := 0
	for _, result := range results {
		if result.Error != nil {
			common.Warning("%s 분석 실패: %v", result.Target.Name, result.Error)
			continue
		}
		if result.Size == 0 {
			continue
		}
		shown++
		detail := result.Target.Description
		if len(result.Target.Paths) == 1 {
			detail = fs.ExpandPath(result.Target.Paths[0])
		}
		fmt.Printf("%-3d %-26s %-12s %-9s %s\n", shown, result.Target.Name, fs.FormatSize(result.Size), result.Target.Risk, text.Truncate(detail, 43))
		total += result.Size
	}
	fmt.Println(text.Separator(98))
	fmt.Printf("%-30s %-12s\n\n", "총계", fs.FormatSize(total))
}

func nonEmptyResults(results []AnalysisResult) []AnalysisResult {
	filtered := make([]AnalysisResult, 0, len(results))
	for _, result := range results {
		if result.Size > 0 && result.Error == nil {
			filtered = append(filtered, result)
		}
	}
	return filtered
}

func containsProjectArtifact(results []AnalysisResult) bool {
	for _, result := range results {
		if result.Target.Kind == kindArtifact {
			return true
		}
	}
	return false
}

func promptSelection(results []AnalysisResult) ([]AnalysisResult, error) {
	fmt.Print("삭제할 번호 (예: 1,3-5 / all / none): ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	indices, err := parseNumberSelection(strings.TrimSpace(line), len(results))
	if err != nil {
		return nil, err
	}
	selected := make([]AnalysisResult, 0, len(indices))
	for _, index := range indices {
		selected = append(selected, results[index])
	}
	return selected, nil
}

func parseNumberSelection(input string, count int) ([]int, error) {
	input = strings.TrimSpace(strings.ToLower(input))
	if input == "" || input == "none" {
		return nil, nil
	}
	if input == "all" {
		indices := make([]int, count)
		for i := range indices {
			indices[i] = i
		}
		return indices, nil
	}
	chosen := map[int]bool{}
	for _, part := range strings.Split(input, ",") {
		bounds := strings.SplitN(strings.TrimSpace(part), "-", 2)
		start, err := strconv.Atoi(bounds[0])
		if err != nil {
			return nil, fmt.Errorf("잘못된 번호 %q", part)
		}
		end := start
		if len(bounds) == 2 {
			end, err = strconv.Atoi(bounds[1])
			if err != nil || end < start {
				return nil, fmt.Errorf("잘못된 범위 %q", part)
			}
		}
		if start < 1 || end > count {
			return nil, fmt.Errorf("범위 밖 번호 %q (1-%d)", part, count)
		}
		for n := start; n <= end; n++ {
			chosen[n-1] = true
		}
	}
	indices := make([]int, 0, len(chosen))
	for index := range chosen {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	return indices, nil
}

func normalizedWorkers(requested, jobs int) int {
	if jobs < 1 {
		return 1
	}
	workers := requested
	if workers == 0 {
		workers = runtime.GOMAXPROCS(0) * 4
	}
	if workers > jobs {
		workers = jobs
	}
	if workers < 1 {
		workers = 1
	}
	return workers
}
