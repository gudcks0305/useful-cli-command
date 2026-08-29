package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/useful-go/internal/ports"
)

type optionalInt struct {
	value int
	set   bool
}

func (v *optionalInt) String() string { return strconv.Itoa(v.value) }
func (v *optionalInt) Set(raw string) error {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return err
	}
	v.value, v.set = value, true
	return nil
}

type app struct {
	snapshot ports.SnapshotFunc
	tty      bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	info, err := os.Stdout.Stat()
	tty := err == nil && info.Mode()&os.ModeCharDevice != 0
	snapshotter := ports.NewSnapshotter()
	code := app{snapshot: snapshotter.Snapshot, tty: tty}.run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
}

func (a app) run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("lsport", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tcp := flags.Bool("tcp", false, "TCP 포트만 표시")
	udp := flags.Bool("udp", false, "UDP 포트만 표시")
	listen := flags.Bool("listen", false, "LISTEN 상태만 표시")
	watch := flags.Bool("watch", false, "포트 변화를 감시")
	interval := flags.Duration("interval", 2*time.Second, "감시 간격")
	help := flags.Bool("help", false, "도움말")
	flags.BoolVar(help, "h", false, "도움말")
	var portValue, countValue optionalInt
	flags.Var(&portValue, "port", "특정 포트만 표시")
	flags.Var(&countValue, "count", "watch 스냅샷 횟수")
	flags.Usage = func() { printUsage(stderr) }

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *help {
		printUsage(stdout)
		return 0
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "오류: 예상하지 않은 인자: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	if *tcp && *udp {
		fmt.Fprintln(stderr, "오류: --tcp와 --udp를 함께 사용할 수 없습니다")
		return 2
	}
	if portValue.set && (portValue.value < 1 || portValue.value > 65535) {
		fmt.Fprintln(stderr, "오류: --port는 1..65535 범위여야 합니다")
		return 2
	}
	if *interval <= 0 {
		fmt.Fprintln(stderr, "오류: --interval은 양수여야 합니다")
		return 2
	}
	if countValue.set && countValue.value <= 0 {
		fmt.Fprintln(stderr, "오류: --count는 양수여야 합니다")
		return 2
	}
	if !*watch && countValue.set {
		fmt.Fprintln(stderr, "오류: --count는 --watch와 함께 사용해야 합니다")
		return 2
	}
	if *watch && !a.tty && !countValue.set {
		fmt.Fprintln(stderr, "오류: 비-TTY 환경의 --watch는 양수 --count가 필요합니다")
		return 2
	}
	if a.snapshot == nil {
		fmt.Fprintln(stderr, "오류: 포트 조회기가 설정되지 않았습니다")
		return 1
	}

	filter := ports.Filter{TCP: *tcp, UDP: *udp, Listen: *listen, Port: portValue.value}
	snapshot := func(ctx context.Context) ([]ports.Port, error) {
		current, err := a.snapshot(ctx)
		if err != nil {
			return nil, err
		}
		return ports.ApplyFilter(current, filter), nil
	}
	if !*watch {
		current, err := snapshot(ctx)
		if err != nil {
			return printRuntimeError(stderr, err)
		}
		if len(current) == 0 {
			fmt.Fprintln(stdout, "사용 중인 포트가 없습니다")
			return 0
		}
		printTable(stdout, current)
		return 0
	}

	err := ports.Watch(ctx, *interval, countValue.value, snapshot, func(change ports.Change) error {
		if change.Initial {
			fmt.Fprintf(stdout, "초기 스냅샷: %d개 포트\n", len(change.Current))
			printTable(stdout, change.Current)
			return nil
		}
		for _, port := range change.Opened {
			printChange(stdout, "OPENED", port)
		}
		for _, port := range change.Closed {
			printChange(stdout, "CLOSED", port)
		}
		return nil
	})
	if err == nil || errors.Is(err, context.Canceled) {
		return 0
	}
	return printRuntimeError(stderr, err)
}

func printRuntimeError(stderr io.Writer, err error) int {
	switch {
	case errors.Is(err, ports.ErrToolNotFound):
		fmt.Fprintln(stderr, "오류: lsof를 찾을 수 없습니다")
	case errors.Is(err, ports.ErrPermission):
		fmt.Fprintln(stderr, "오류: lsof 실행 권한이 없습니다")
	default:
		fmt.Fprintf(stderr, "오류: 포트 조회 실패: %v\n", err)
	}
	return 1
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "ls-port - 사용 중인 포트 목록 조회/감시")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "사용법: lsport [options]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "옵션:")
	fmt.Fprintln(out, "  --tcp          TCP 포트만 표시")
	fmt.Fprintln(out, "  --udp          UDP 포트만 표시")
	fmt.Fprintln(out, "  --listen       LISTEN 상태만 표시")
	fmt.Fprintln(out, "  --port N       특정 포트만 표시 (1..65535)")
	fmt.Fprintln(out, "  --watch        포트 opened/closed 변화 감시")
	fmt.Fprintln(out, "  --interval D   감시 간격 (기본 2s)")
	fmt.Fprintln(out, "  --count N      N개 스냅샷 후 종료")
	fmt.Fprintln(out, "  -h, --help     도움말")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "비-TTY watch는 양수 --count가 필요합니다.")
}

func printTable(out io.Writer, snapshot []ports.Port) {
	fmt.Fprintf(out, "%-7s %-6s %-8s %-20s %-12s %-12s %s\n", "PORT", "PROTO", "PID", "COMMAND", "USER", "STATE", "NAME")
	for _, port := range snapshot {
		fmt.Fprintf(out, "%-7d %-6s %-8d %-20s %-12s %-12s %s\n", port.Port, port.Protocol, port.PID, truncate(port.Command, 20), truncate(port.User, 12), port.State, port.Name)
	}
	fmt.Fprintf(out, "총 %d개 포트 사용 중\n", len(snapshot))
}

func printChange(out io.Writer, label string, port ports.Port) {
	fmt.Fprintf(out, "%-6s %5d/%-3s pid=%d command=%q user=%q state=%q name=%q\n", label, port.Port, port.Protocol, port.PID, port.Command, port.User, port.State, port.Name)
}

func truncate(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max-1]) + "…"
}
