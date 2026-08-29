package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/useful-go/internal/ports"
)

func testApp(tty bool, snapshots ...[]ports.Port) app {
	index := 0
	return app{tty: tty, snapshot: func(context.Context) ([]ports.Port, error) {
		if index >= len(snapshots) {
			return snapshots[len(snapshots)-1], nil
		}
		result := snapshots[index]
		index++
		return result, nil
	}}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	tests := [][]string{
		{"--port", "0"}, {"--port", "65536"}, {"--interval", "0s"},
		{"--count", "0", "--watch"}, {"--count", "1"}, {"--tcp", "--udp"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := testApp(true, nil).run(context.Background(), args, &stdout, &stderr); code != 2 {
				t.Fatalf("run(%v) code=%d stderr=%q", args, code, stderr.String())
			}
		})
	}
}

func TestRunRequiresCountForNonTTYWatch(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := testApp(false, nil).run(context.Background(), []string{"--watch"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "비-TTY") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestRunWatchPrintsDeterministicChangesAndCountStops(t *testing.T) {
	first := []ports.Port{{Port: 9000, Protocol: "TCP", PID: 1, Command: "old", User: "me", State: "LISTEN", Name: "*:9000"}}
	second := []ports.Port{
		{Port: 3000, Protocol: "TCP", PID: 2, Command: "new", User: "me", State: "LISTEN", Name: "*:3000"},
		{Port: 1000, Protocol: "UDP", PID: 3, Command: "dns", User: "me", Name: "*:1000"},
	}
	var stdout, stderr bytes.Buffer
	code := testApp(false, first, second).run(context.Background(), []string{"--watch", "--count", "2", "--interval", time.Nanosecond.String()}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	output := stdout.String()
	udp := strings.Index(output, "OPENED  1000/UDP")
	tcp := strings.Index(output, "OPENED  3000/TCP")
	closed := strings.Index(output, "CLOSED  9000/TCP")
	if udp < 0 || tcp < udp || closed < tcp {
		t.Fatalf("unexpected output ordering:\n%s", output)
	}
}

func TestRunCanceledWatchReturnsSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := app{tty: true, snapshot: func(context.Context) ([]ports.Port, error) { cancel(); return nil, nil }}
	var stdout, stderr bytes.Buffer
	if code := a.run(ctx, []string{"--watch", "--interval", "1h"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}
