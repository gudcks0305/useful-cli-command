package ports

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strconv"
	"testing"
	"time"
)

type runnerResult struct {
	stdout, stderr []byte
	err            error
}

func (r runnerResult) Run(context.Context, string, ...string) ([]byte, []byte, error) {
	return r.stdout, r.stderr, r.err
}

func fields(values ...string) []byte {
	var result []byte
	for _, value := range values {
		result = append(result, value...)
		result = append(result, 0)
	}
	return result
}

func TestParseNULFieldsSpacesIPv6AndState(t *testing.T) {
	fixture := fields("p42", "cCommand With Spaces", "Lalice smith", "u501", "\nf10", "PTCP", "n[::1]:8080->[2001:db8::1]:443", "TST=ESTABLISHED", "TQR=0", "\n", "f11", "PUDP", "n*:5353", "\n")
	got, err := Parse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	want := []Port{
		{Port: 5353, Protocol: "UDP", PID: 42, Command: "Command With Spaces", User: "alice smith", Name: "*:5353"},
		{Port: 8080, Protocol: "TCP", PID: 42, Command: "Command With Spaces", User: "alice smith", Name: "[::1]:8080->[2001:db8::1]:443", State: "ESTABLISHED"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse() = %#v, want %#v", got, want)
	}
}

func TestParseSkipsWildcardServiceAndFallsBackToUID(t *testing.T) {
	fixture := fields("p7", "cdaemon", "u501", "\nf3", "PUDP", "n*:*", "\nf4", "PTCP", "n127.0.0.1:3000", "TST=LISTEN")
	got, err := Parse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Port != 3000 || got[0].User != "501" {
		t.Fatalf("unexpected ports: %#v", got)
	}
}

func TestParseValidWildcardOnlyIsEmpty(t *testing.T) {
	got, err := Parse(fields("p7", "cdaemon", "u501", "\nf3", "PUDP", "n*:*"))
	if err != nil || len(got) != 0 {
		t.Fatalf("ports=%#v err=%v", got, err)
	}
}

func TestParseRejectsInvalidPIDRange(t *testing.T) {
	for _, pid := range []string{"0", "-1", "2147483648", "not-a-pid"} {
		fixture := fields("p"+pid, "f3", "PTCP", "n*:3000", "TST=LISTEN")
		if _, err := Parse(fixture); !errors.Is(err, ErrMalformedOutput) {
			t.Fatalf("PID %q err=%v, want ErrMalformedOutput", pid, err)
		}
	}
}

func TestApplyFilterUsesLocalSideOfConnectedSocket(t *testing.T) {
	fixture := fields(
		"p7", "cclient", "f3", "PUDP", "n127.0.0.1:40000->127.0.0.1:3000",
		"f4", "PUDP", "n127.0.0.1:3000->127.0.0.1:40000",
	)
	snapshot, err := Parse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	got := ApplyFilter(snapshot, Filter{UDP: true, Port: 3000})
	if len(got) != 1 || got[0].Name != "127.0.0.1:3000->127.0.0.1:40000" {
		t.Fatalf("filtered=%#v", got)
	}
}

func TestSnapshotDistinguishesEmptyMissingAndPermission(t *testing.T) {
	exitError := func(command string) error {
		err := exec.Command("/bin/sh", "-c", command).Run()
		if err == nil {
			t.Fatalf("command %q unexpectedly succeeded", command)
		}
		return err
	}
	tests := []struct {
		name      string
		runner    Runner
		want      error
		wantEmpty bool
	}{
		{name: "exit 1 is empty", runner: runnerResult{err: exitError("exit 1")}, wantEmpty: true},
		{name: "exit 2 is failure", runner: runnerResult{err: exitError("exit 2")}},
		{name: "generic error is failure", runner: runnerResult{err: errors.New("runner failed")}},
		{name: "signal is failure", runner: runnerResult{err: exitError("kill -TERM $$")}},
		{name: "missing", runner: runnerResult{err: &exec.Error{Name: "lsof", Err: exec.ErrNotFound}}, want: ErrToolNotFound},
		{name: "permission", runner: runnerResult{stderr: []byte("Operation not permitted"), err: errors.New("exit status 1")}, want: ErrPermission},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (Snapshotter{Runner: tt.runner}).Snapshot(context.Background())
			if tt.wantEmpty {
				if err != nil || len(got) != 0 {
					t.Fatalf("ports=%#v err=%v", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ports=%#v, want failure", got)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("err=%v want %v", err, tt.want)
			}
		})
	}
}

func TestApplyFilter(t *testing.T) {
	snapshot := []Port{
		{Port: 80, Protocol: "TCP", PID: 1, State: "LISTEN", Name: "*:80"},
		{Port: 53, Protocol: "UDP", PID: 2, Name: "*:53"},
		{Port: 443, Protocol: "TCP", PID: 3, State: "ESTABLISHED", Name: "*:443"},
	}
	got := ApplyFilter(snapshot, Filter{TCP: true, Listen: true, Port: 80})
	if len(got) != 1 || got[0].Port != 80 {
		t.Fatalf("unexpected filter result: %#v", got)
	}
}

func TestDiffOrderingAndStateNotIdentity(t *testing.T) {
	previous := []Port{
		{Port: 9000, Protocol: "TCP", PID: 2, Name: "*:9000", State: "LISTEN"},
		{Port: 7000, Protocol: "UDP", PID: 4, Name: "*:7000"},
	}
	current := []Port{
		{Port: 9000, Protocol: "TCP", PID: 2, Name: "*:9000", State: "ESTABLISHED"},
		{Port: 3000, Protocol: "TCP", PID: 9, Name: "*:3000"},
		{Port: 1000, Protocol: "TCP", PID: 8, Name: "*:1000"},
	}
	opened, closed := Diff(previous, current)
	if got := []int{opened[0].Port, opened[1].Port}; !reflect.DeepEqual(got, []int{1000, 3000}) {
		t.Fatalf("opened ordering = %v", got)
	}
	if len(closed) != 1 || closed[0].Port != 7000 {
		t.Fatalf("closed = %#v", closed)
	}
}

func TestWatchCount(t *testing.T) {
	calls := 0
	var changes []Change
	err := Watch(context.Background(), time.Nanosecond, 3, func(context.Context) ([]Port, error) {
		calls++
		return []Port{{Port: calls, Protocol: "TCP", PID: 1, Name: "*:" + strconv.Itoa(calls)}}, nil
	}, func(change Change) error { changes = append(changes, change); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(changes) != 3 || !changes[0].Initial || changes[1].Initial {
		t.Fatalf("calls=%d changes=%#v", calls, changes)
	}
}

func TestWatchCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Watch(ctx, time.Hour, 0, func(context.Context) ([]Port, error) { calls++; return nil, nil }, func(Change) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
