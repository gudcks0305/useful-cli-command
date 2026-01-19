package main

import (
	"flag"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/useful-go/pkg/common"
)

type PortInfo struct {
	Port     int
	Protocol string
	PID      string
	Command  string
	User     string
	State    string
	CPU      string
	Mem      string
}

func main() {
	tcpOnly := flag.Bool("tcp", false, "TCP 포트만 표시")
	udpOnly := flag.Bool("udp", false, "UDP 포트만 표시")
	listen := flag.Bool("listen", false, "LISTEN 상태만 표시")
	portFilter := flag.Int("port", 0, "특정 포트만 표시")
	help := flag.Bool("help", false, "도움말")
	flag.BoolVar(help, "h", false, "도움말")

	flag.Parse()

	if *help {
		printUsage()
		return
	}

	ports := getPortList(*tcpOnly, *udpOnly, *listen, *portFilter)
	if len(ports) == 0 {
		common.Warning("사용 중인 포트가 없습니다")
		return
	}

	printPortTable(ports)
}

func printUsage() {
	common.Header("ls-port - 사용 중인 포트 목록 조회")
	fmt.Println()
	fmt.Println("사용법: lsport [options]")
	fmt.Println()
	fmt.Println("옵션:")
	fmt.Println("  --tcp          TCP 포트만 표시")
	fmt.Println("  --udp          UDP 포트만 표시")
	fmt.Println("  --listen       LISTEN 상태만 표시")
	fmt.Println("  --port N       특정 포트만 표시")
	fmt.Println("  -h, --help     도움말")
	fmt.Println()
	fmt.Println("예시:")
	fmt.Println("  lsport              # 모든 포트 표시")
	fmt.Println("  lsport --tcp        # TCP만 표시")
	fmt.Println("  lsport --listen     # 리스닝 포트만 표시")
	fmt.Println("  lsport --port 3000  # 3000번 포트만 표시")
}

func getPortList(tcpOnly, udpOnly, listenOnly bool, portFilter int) []PortInfo {
	// lsof -i -P -n: 네트워크 연결 정보, 포트 숫자로 표시, DNS 해석 안함
	cmd := exec.Command("lsof", "-i", "-P", "-n")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var ports []PortInfo
	seen := make(map[string]bool)
	lines := strings.Split(string(output), "\n")

	// 포트 추출 정규식
	portRegex := regexp.MustCompile(`:(\d+)(?:\s|$|->)`)

	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue // 헤더 스킵
		}

		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}

		command := fields[0]
		pid := fields[1]
		user := fields[2]
		protocol := strings.ToUpper(fields[7]) // TCP, UDP
		name := fields[8]                       // 연결 정보

		// 프로토콜 필터
		if tcpOnly && !strings.HasPrefix(protocol, "TCP") {
			continue
		}
		if udpOnly && !strings.HasPrefix(protocol, "UDP") {
			continue
		}

		// 상태 확인
		state := ""
		if len(fields) >= 10 {
			state = fields[9]
			// 괄호 제거: (LISTEN) -> LISTEN
			state = strings.Trim(state, "()")
		}

		if listenOnly && state != "LISTEN" {
			continue
		}

		// 로컬 포트 추출
		matches := portRegex.FindStringSubmatch(name)
		if len(matches) < 2 {
			continue
		}

		port, err := strconv.Atoi(matches[1])
		if err != nil {
			continue
		}

		// 포트 필터
		if portFilter > 0 && port != portFilter {
			continue
		}

		// 중복 제거 (같은 포트/프로토콜/프로세스)
		key := fmt.Sprintf("%d-%s-%s", port, protocol, pid)
		if seen[key] {
			continue
		}
		seen[key] = true

		// CPU, 메모리 사용량 조회
		cpu, mem := getProcessStats(pid)

		ports = append(ports, PortInfo{
			Port:     port,
			Protocol: protocol,
			PID:      pid,
			Command:  truncate(command, 20),
			User:     user,
			State:    state,
			CPU:      cpu,
			Mem:      mem,
		})
	}

	// 포트 번호로 정렬
	sort.Slice(ports, func(i, j int) bool {
		return ports[i].Port < ports[j].Port
	})

	return ports
}

func printPortTable(ports []PortInfo) {
	common.Header("사용 중인 포트 목록")
	fmt.Println()

	// 헤더
	fmt.Printf("%s%-7s %-6s %-8s %-18s %-7s %-7s %-12s %-10s%s\n",
		common.Bold, "PORT", "PROTO", "PID", "COMMAND", "CPU%", "MEM%", "USER", "STATE", common.Reset)
	fmt.Println(strings.Repeat("─", 85))

	for _, p := range ports {
		stateColor := getStateColor(p.State)
		cpuColor := getCPUColor(p.CPU)
		memColor := getMemColor(p.Mem)
		fmt.Printf("%-7d %-6s %-8s %-18s %s%-7s%s %s%-7s%s %-12s %s%-10s%s\n",
			p.Port, p.Protocol, p.PID, truncate(p.Command, 18),
			cpuColor, p.CPU, common.Reset,
			memColor, p.Mem, common.Reset,
			p.User, stateColor, p.State, common.Reset)
	}

	fmt.Println()
	common.Info("총 %d개 포트 사용 중", len(ports))
}

func getCPUColor(cpu string) string {
	val, err := strconv.ParseFloat(cpu, 64)
	if err != nil {
		return ""
	}
	if val >= 50 {
		return common.Red
	} else if val >= 20 {
		return common.Yellow
	}
	return ""
}

func getMemColor(mem string) string {
	val, err := strconv.ParseFloat(mem, 64)
	if err != nil {
		return ""
	}
	if val >= 10 {
		return common.Red
	} else if val >= 5 {
		return common.Yellow
	}
	return ""
}

func getStateColor(state string) string {
	switch state {
	case "LISTEN":
		return common.Green
	case "ESTABLISHED":
		return common.Cyan
	case "CLOSE_WAIT", "TIME_WAIT":
		return common.Yellow
	default:
		return ""
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// getProcessStats returns CPU%, MEM% for a given PID
func getProcessStats(pid string) (cpu, mem string) {
	cmd := exec.Command("ps", "-p", pid, "-o", "%cpu,%mem")
	output, err := cmd.Output()
	if err != nil {
		return "-", "-"
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return "-", "-"
	}

	fields := strings.Fields(lines[1])
	if len(fields) >= 2 {
		return fields[0], fields[1]
	}
	return "-", "-"
}
