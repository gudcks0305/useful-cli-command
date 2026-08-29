// Package ports collects and compares local Internet socket snapshots.
package ports

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	ErrToolNotFound    = errors.New("lsof not found")
	ErrPermission      = errors.New("permission denied while running lsof")
	ErrMalformedOutput = errors.New("malformed lsof output")
)

const maxPID = 1<<31 - 1

type Port struct {
	Port     int
	Protocol string
	PID      int
	Command  string
	User     string
	Name     string
	State    string
}

type Filter struct {
	TCP, UDP, Listen bool
	Port             int
}

type Runner interface {
	Run(context.Context, string, ...string) (stdout, stderr []byte, err error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type Snapshotter struct{ Runner Runner }

func NewSnapshotter() Snapshotter { return Snapshotter{Runner: execRunner{}} }

// Snapshot returns an empty slice for a successful empty selection, keeping it
// distinct from missing-tool and permission errors.
func (s Snapshotter) Snapshot(ctx context.Context) ([]Port, error) {
	runner := s.Runner
	if runner == nil {
		runner = execRunner{}
	}
	out, stderr, err := runner.Run(ctx, "lsof", "-nP", "-i", "-FpcLuPnT0")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var execErr *exec.Error
		if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%w: %v", ErrToolNotFound, err)
		}
		message := strings.ToLower(string(stderr))
		if strings.Contains(message, "permission denied") || strings.Contains(message, "not permitted") || strings.Contains(message, "operation not permitted") {
			return nil, fmt.Errorf("%w: %s", ErrPermission, strings.TrimSpace(string(stderr)))
		}
		// lsof exits 1 when its selection has no matches and emits no diagnostic.
		// Other failures (including signals and exit 2) must remain visible.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(out) == 0 && len(bytes.TrimSpace(stderr)) == 0 {
			return []Port{}, nil
		}
		return nil, fmt.Errorf("lsof failed: %w%s", err, stderrSuffix(stderr))
	}
	if len(out) == 0 {
		return []Port{}, nil
	}
	return Parse(out)
}

func stderrSuffix(stderr []byte) string {
	message := strings.TrimSpace(string(stderr))
	if message == "" {
		return ""
	}
	return ": " + message
}

// Parse reads NUL-terminated lsof field output (-F...0). Newlines separating
// process and file sets are ignored; spaces inside field values are retained.
func Parse(data []byte) ([]Port, error) {
	type process struct {
		pid                 int
		command, login, uid string
	}
	var proc process
	var file Port
	var haveProc, haveFile, sawField, sawStructure bool
	var parsed []Port
	flushFile := func() {
		defer func() { haveFile = false; file = Port{} }()
		if !haveFile || !haveProc || file.Protocol == "" || file.Name == "" {
			return
		}
		sawStructure = true
		port, ok := localPort(file.Name)
		if !ok {
			return
		}
		file.Port, file.PID, file.Command = port, proc.pid, proc.command
		file.User = proc.login
		if file.User == "" {
			file.User = proc.uid
		}
		file.Protocol = strings.ToUpper(file.Protocol)
		parsed = append(parsed, file)
	}
	for _, raw := range bytes.Split(data, []byte{0}) {
		raw = bytes.Trim(raw, "\r\n")
		if len(raw) == 0 {
			continue
		}
		sawField = true
		id, value := raw[0], string(raw[1:])
		switch id {
		case 'p':
			flushFile()
			pid, err := strconv.Atoi(value)
			if err != nil || pid <= 0 || pid > maxPID {
				haveProc = false
				proc = process{}
				continue
			}
			proc, haveProc = process{pid: pid}, true
		case 'c':
			if haveProc {
				proc.command = value
			}
		case 'L':
			if haveProc {
				proc.login = value
			}
		case 'u':
			if haveProc {
				proc.uid = value
			}
		case 'f':
			flushFile()
			haveFile = true
		case 'P':
			if haveFile {
				file.Protocol = value
			}
		case 'n':
			if haveFile {
				file.Name = value
			}
		case 'T':
			if haveFile && strings.HasPrefix(value, "ST=") {
				file.State = strings.TrimPrefix(value, "ST=")
			}
		}
	}
	flushFile()
	if sawField && !sawStructure {
		return nil, ErrMalformedOutput
	}
	return uniqueSorted(parsed), nil
}

func localPort(name string) (int, bool) {
	local := name
	if before, _, ok := strings.Cut(name, "->"); ok {
		local = before
	}
	colon := strings.LastIndexByte(local, ':')
	if colon < 0 || colon == len(local)-1 {
		return 0, false
	}
	port, err := strconv.Atoi(local[colon+1:])
	return port, err == nil && port >= 1 && port <= 65535
}

func ApplyFilter(snapshot []Port, filter Filter) []Port {
	filtered := make([]Port, 0, len(snapshot))
	for _, port := range snapshot {
		if filter.TCP && port.Protocol != "TCP" {
			continue
		}
		if filter.UDP && port.Protocol != "UDP" {
			continue
		}
		if filter.Listen && port.State != "LISTEN" {
			continue
		}
		if filter.Port != 0 && port.Port != filter.Port {
			continue
		}
		filtered = append(filtered, port)
	}
	return uniqueSorted(filtered)
}

func key(port Port) string {
	return fmt.Sprintf("%05d\x00%s\x00%010d\x00%s", port.Port, port.Protocol, port.PID, port.Name)
}

func uniqueSorted(snapshot []Port) []Port {
	byKey := make(map[string]Port, len(snapshot))
	for _, port := range snapshot {
		byKey[key(port)] = port
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	result := make([]Port, 0, len(keys))
	for _, k := range keys {
		result = append(result, byKey[k])
	}
	return result
}

// Diff excludes state and display metadata from identity, avoiding false
// close/open pairs when one socket merely changes state.
func Diff(previous, current []Port) (opened, closed []Port) {
	previousByKey := make(map[string]Port, len(previous))
	currentByKey := make(map[string]Port, len(current))
	for _, port := range previous {
		previousByKey[key(port)] = port
	}
	for _, port := range current {
		currentByKey[key(port)] = port
	}
	for k, port := range currentByKey {
		if _, ok := previousByKey[k]; !ok {
			opened = append(opened, port)
		}
	}
	for k, port := range previousByKey {
		if _, ok := currentByKey[k]; !ok {
			closed = append(closed, port)
		}
	}
	return uniqueSorted(opened), uniqueSorted(closed)
}

type Change struct {
	Sample         int
	Initial        bool
	Current        []Port
	Opened, Closed []Port
}

type SnapshotFunc func(context.Context) ([]Port, error)

// Watch samples immediately, then after each interval. Count is number of
// snapshots; zero means until context cancellation.
func Watch(ctx context.Context, interval time.Duration, count int, snapshot SnapshotFunc, emit func(Change) error) error {
	if interval <= 0 {
		return errors.New("interval must be positive")
	}
	if count < 0 {
		return errors.New("count must not be negative")
	}
	if snapshot == nil || emit == nil {
		return errors.New("snapshot and emit are required")
	}
	var previous []Port
	for sample := 1; count == 0 || sample <= count; sample++ {
		if sample > 1 {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return ctx.Err()
			case <-timer.C:
			}
		}
		current, err := snapshot(ctx)
		if err != nil {
			return err
		}
		current = uniqueSorted(current)
		change := Change{Sample: sample, Current: current}
		if sample == 1 {
			change.Initial = true
		} else {
			change.Opened, change.Closed = Diff(previous, current)
		}
		if err := emit(change); err != nil {
			return err
		}
		previous = current
	}
	return nil
}
