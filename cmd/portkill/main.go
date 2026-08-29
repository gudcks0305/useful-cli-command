package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/useful-go/internal/ports"
)

const (
	commandTimeout = 3 * time.Second
	psStartLayout  = "Mon Jan _2 15:04:05 2006"
)

var errLsofUnavailable = errors.New("lsof unavailable")

type options struct {
	port  int
	force bool
}

type processOwner struct {
	PID       int
	StartedAt string
}

type ownerFinder interface {
	Find(port int) ([]processOwner, error)
}

type processSignaler interface {
	Signal(pid int, signal os.Signal) error
}

type dependencies struct {
	finder   ownerFinder
	signaler processSignaler
	selfPID  int
}

type lsofRunner interface {
	Run(args ...string) (stdout, stderr []byte, err error)
}

type psRunner interface {
	Run(args ...string) (stdout, stderr []byte, err error)
}

type identityResolver interface {
	Resolve(pid int) (string, error)
}

type execLsofRunner struct{}

func (execLsofRunner) Run(args ...string) ([]byte, []byte, error) {
	stdout, stderr, err := runCommand("lsof", args...)
	if errors.Is(err, exec.ErrNotFound) {
		err = fmt.Errorf("%w: %v", errLsofUnavailable, err)
	}
	return stdout, stderr, err
}

type execPSRunner struct{}

func (execPSRunner) Run(args ...string) ([]byte, []byte, error) {
	return runCommandWithEnv("ps", withCLocale(os.Environ()), args...)
}

func runCommand(name string, args ...string) ([]byte, []byte, error) {
	return runCommandWithEnv(name, nil, args...)
}

func runCommandWithEnv(name string, env []string, args ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = env
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = fmt.Errorf("%s 실행 시간 초과: %w", name, ctxErr)
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func withCLocale(env []string) []string {
	replaced := make([]string, 0, len(env)+2)
	for _, value := range env {
		if strings.HasPrefix(value, "LC_ALL=") || strings.HasPrefix(value, "LANG=") {
			continue
		}
		replaced = append(replaced, value)
	}
	return append(replaced, "LC_ALL=C", "LANG=C")
}

type lsofOwnerFinder struct {
	runner   lsofRunner
	identity identityResolver
}

type psIdentityResolver struct {
	runner psRunner
}

func (r psIdentityResolver) Resolve(pid int) (string, error) {
	stdout, stderr, err := r.runner.Run("-p", strconv.Itoa(pid), "-o", "lstart=")
	if err != nil {
		detail := strings.TrimSpace(string(stderr))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("ps 조회 실패: %s", detail)
	}
	startedAt, err := parsePSStart(stdout)
	if err != nil {
		return "", err
	}
	return startedAt, nil
}

type osProcessSignaler struct{}

func (osProcessSignaler) Signal(pid int, signal os.Signal) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(signal)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runWithDependencies(args, stdin, stdout, stderr, dependencies{
		finder: lsofOwnerFinder{
			runner:   execLsofRunner{},
			identity: newIdentityResolver(),
		},
		signaler: osProcessSignaler{},
		selfPID:  os.Getpid(),
	})
}

func runWithDependencies(args []string, stdin io.Reader, stdout, stderr io.Writer, deps dependencies) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		printUsage(stdout)
		return 0
	}

	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "오류: %v\n", err)
		printUsage(stderr)
		return 1
	}

	owners, err := deps.finder.Find(opts.port)
	if err != nil {
		printOwners(stdout, opts.port, owners, true)
		printFindError(stderr, err)
		return 1
	}
	if len(owners) == 0 {
		fmt.Fprintf(stdout, "포트 %d에 바인딩된 프로세스가 없습니다.\n", opts.port)
		return 0
	}

	printOwners(stdout, opts.port, owners, false)
	fmt.Fprint(stdout, "종료 신호를 보내시겠습니까? (y/N): ")
	answer, readErr := bufio.NewReader(stdin).ReadString('\n')
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		fmt.Fprintf(stderr, "입력 읽기 실패: %v\n", readErr)
		return 1
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "y" && answer != "yes" {
		fmt.Fprintln(stdout, "취소되었습니다.")
		return 0
	}

	confirmed := make(map[int]processOwner, len(owners))
	for _, owner := range owners {
		confirmed[owner.PID] = owner
	}
	reportedNew := make(map[int]struct{})

	signal, signalName := terminationSignal(opts.force)

	failed := false
	for _, owner := range owners {
		// Ownership/identity validation and PID signaling cannot be one atomic
		// kernel operation. Re-query here, immediately before each signal, to
		// keep the unavoidable non-atomic window as small as possible.
		currentOwners, err := deps.finder.Find(opts.port)
		if err != nil {
			printFindError(stderr, fmt.Errorf("PID %d 신호 전 소유자 재확인 실패: %w", owner.PID, err))
			failed = true
			continue
		}
		var currentOwner processOwner
		stillOwns := false
		for _, candidate := range currentOwners {
			if _, wasConfirmed := confirmed[candidate.PID]; !wasConfirmed {
				if _, reported := reportedNew[candidate.PID]; !reported {
					fmt.Fprintf(stdout, "PID %d: 확인 후 생긴 새 소유자이므로 종료하지 않음.\n", candidate.PID)
					reportedNew[candidate.PID] = struct{}{}
				}
			}
			if candidate.PID == owner.PID {
				currentOwner, stillOwns = candidate, true
			}
		}
		if !stillOwns {
			fmt.Fprintf(stdout, "PID %d: 더 이상 포트 %d 소유자가 아니므로 건너뜀.\n", owner.PID, opts.port)
			continue
		}
		if owner.StartedAt == "" || currentOwner.StartedAt == "" {
			fmt.Fprintf(stderr, "PID %d: 프로세스 시작 identity를 확인할 수 없어 신호를 보내지 않음.\n", owner.PID)
			failed = true
			continue
		}
		if owner.StartedAt != currentOwner.StartedAt {
			fmt.Fprintf(stderr, "PID %d: 프로세스 시작 identity 변경(%s -> %s), PID 재사용 가능성으로 신호를 보내지 않음.\n", owner.PID, owner.StartedAt, currentOwner.StartedAt)
			failed = true
			continue
		}
		if owner.PID == deps.selfPID {
			fmt.Fprintf(stderr, "PID %d: portkill 자신의 PID에는 신호를 보내지 않음.\n", owner.PID)
			failed = true
			continue
		}
		if err := deps.signaler.Signal(owner.PID, signal); err != nil {
			fmt.Fprintf(stderr, "PID %d: %s 전송 실패: %v\n", owner.PID, signalName, err)
			failed = true
			continue
		}
		fmt.Fprintf(stdout, "PID %d: %s 전송 완료.\n", owner.PID, signalName)
	}

	if failed {
		return 1
	}
	return 0
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, "사용법: portkill [--force] <port>")
}

func parseArgs(args []string) (options, error) {
	var opts options
	var portArg string
	switch {
	case len(args) == 1:
		portArg = args[0]
	case len(args) == 2 && args[0] == "--force":
		opts.force = true
		portArg = args[1]
	default:
		return opts, errors.New("인자는 [--force] <port> 형식이어야 합니다")
	}

	port, err := strconv.Atoi(portArg)
	if err != nil {
		return opts, fmt.Errorf("유효하지 않은 포트 %q: 정수여야 합니다", portArg)
	}
	if port < 1 || port > 65535 {
		return opts, fmt.Errorf("유효하지 않은 포트 %d: 1..65535 범위여야 합니다", port)
	}
	opts.port = port
	return opts, nil
}

func (f lsofOwnerFinder) Find(port int) ([]processOwner, error) {
	queries := []struct {
		name   string
		args   []string
		filter ports.Filter
	}{
		{
			name:   "TCP LISTEN",
			args:   []string{"-nP", "-a", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-FpcLuPnT0"},
			filter: ports.Filter{TCP: true, Listen: true, Port: port},
		},
		{
			name:   "UDP",
			args:   []string{"-nP", "-a", fmt.Sprintf("-iUDP:%d", port), "-FpcLuPnT0"},
			filter: ports.Filter{UDP: true, Port: port},
		},
	}

	seenPIDs := make(map[int]struct{})
	var pids []int
	var findErrors []error
	for _, query := range queries {
		stdout, stderr, err := f.runner.Run(query.args...)
		if errors.Is(err, errLsofUnavailable) || errors.Is(err, exec.ErrNotFound) {
			return resolveOwners(pids, f.identity, &findErrors), fmt.Errorf("%w: 실행 파일을 찾을 수 없습니다", errLsofUnavailable)
		}

		if len(bytes.TrimSpace(stdout)) > 0 {
			snapshot, parseErr := ports.Parse(stdout)
			if parseErr != nil {
				findErrors = append(findErrors, fmt.Errorf("%s 출력 파싱 실패: %w", query.name, parseErr))
			} else {
				for _, socket := range ports.ApplyFilter(snapshot, query.filter) {
					if _, seen := seenPIDs[socket.PID]; seen {
						continue
					}
					seenPIDs[socket.PID] = struct{}{}
					pids = append(pids, socket.PID)
				}
			}
		}

		if err == nil {
			continue
		}
		if isLsofNoMatch(err, stdout, stderr) {
			continue
		}
		detail := strings.TrimSpace(string(stderr))
		if detail == "" {
			detail = err.Error()
		}
		findErrors = append(findErrors, fmt.Errorf("%s 조회 실패: %s", query.name, detail))
	}

	owners := resolveOwners(pids, f.identity, &findErrors)
	return owners, errors.Join(findErrors...)
}

func resolveOwners(pids []int, resolver identityResolver, findErrors *[]error) []processOwner {
	owners := make([]processOwner, 0, len(pids))
	for _, pid := range pids {
		owner := processOwner{PID: pid}
		if resolver == nil {
			*findErrors = append(*findErrors, fmt.Errorf("PID %d identity resolver 없음", pid))
			owners = append(owners, owner)
			continue
		}
		startedAt, err := resolver.Resolve(pid)
		if err != nil {
			*findErrors = append(*findErrors, fmt.Errorf("PID %d 시작 identity 조회 실패: %w", pid, err))
		} else {
			owner.StartedAt = startedAt
		}
		owners = append(owners, owner)
	}
	return owners
}

func isLsofNoMatch(err error, stdout, stderr []byte) bool {
	if len(bytes.TrimSpace(stdout)) != 0 || len(bytes.TrimSpace(stderr)) != 0 {
		return false
	}
	var exitErr interface{ ExitCode() int }
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

func parsePSStart(output []byte) (string, error) {
	startedAt := strings.TrimSpace(string(output))
	if startedAt == "" {
		return "", errors.New("프로세스 시작 identity 출력이 비어 있음")
	}
	if strings.Contains(startedAt, "\n") {
		return "", fmt.Errorf("프로세스 시작 identity 출력이 여러 줄임: %q", startedAt)
	}
	parsed, err := time.Parse(psStartLayout, startedAt)
	if err != nil {
		return "", fmt.Errorf("프로세스 시작 identity 파싱 실패 %q: %w", startedAt, err)
	}
	return parsed.Format(psStartLayout), nil
}

func printOwners(stdout io.Writer, port int, owners []processOwner, partial bool) {
	if len(owners) == 0 {
		return
	}
	label := "소유 프로세스 스냅샷"
	if partial {
		label = "부분 소유 프로세스 스냅샷"
	}
	fmt.Fprintf(stdout, "포트 %d %s:\n", port, label)
	for _, owner := range owners {
		startedAt := owner.StartedAt
		if startedAt == "" {
			startedAt = "<identity unavailable>"
		}
		fmt.Fprintf(stdout, "  PID %d  started=%s\n", owner.PID, startedAt)
	}
}

func printFindError(stderr io.Writer, err error) {
	if errors.Is(err, errLsofUnavailable) {
		fmt.Fprintln(stderr, "소유자 조회 실패: lsof를 찾을 수 없습니다.")
		return
	}
	fmt.Fprintf(stderr, "소유자 조회 실패: %v\n", err)
}
