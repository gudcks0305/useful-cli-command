package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/useful-go/pkg/common"
)

func main() {
	if len(os.Args) < 2 {
		common.Error("포트 번호를 입력해주세요")
		fmt.Println("사용법: portkill <port>")
		os.Exit(1)
	}

	port := os.Args[1]
	if _, err := strconv.Atoi(port); err != nil {
		common.Fatal("유효하지 않은 포트 번호: %s", port)
	}

	pids := findProcessByPort(port)
	if len(pids) == 0 {
		common.Warning("포트 %s를 사용하는 프로세스가 없습니다", port)
		return
	}

	common.Info("포트 %s를 사용하는 프로세스:", port)
	for _, pid := range pids {
		showProcessInfo(pid)
	}

	fmt.Print("\n이 프로세스를 종료하시겠습니까? (y/N): ")
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))

	if answer != "y" && answer != "yes" {
		common.Info("취소되었습니다")
		return
	}

	for _, pid := range pids {
		killProcess(pid)
	}
}

func findProcessByPort(port string) []string {
	cmd := exec.Command("lsof", "-i", ":"+port, "-t")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var pids []string
	seen := make(map[string]bool)

	for _, line := range lines {
		pid := strings.TrimSpace(line)
		if pid != "" && !seen[pid] {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}
	return pids
}

func showProcessInfo(pid string) {
	cmd := exec.Command("ps", "-p", pid, "-o", "pid,comm,user,%cpu,%mem")
	output, err := cmd.Output()
	if err != nil {
		common.Warning("PID %s 정보 조회 실패", pid)
		return
	}
	fmt.Println(string(output))
}

func killProcess(pid string) {
	cmd := exec.Command("kill", "-9", pid)
	if err := cmd.Run(); err != nil {
		common.Error("PID %s 종료 실패: %v", pid, err)
		return
	}
	common.Success("PID %s 종료 완료", pid)
}
