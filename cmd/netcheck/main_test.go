package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunHTTPJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("method = %s", request.Method)
		}
		fmt.Fprint(writer, "private response body")
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", server.URL + "/health?token=private"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
	}
	var result checkResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Target != server.URL+"/health" {
		t.Fatalf("target = %q", result.Target)
	}
	if len(result.Phases) != 3 || result.Phases[0].Name != "dns" || result.Phases[1].Name != "tcp" || result.Phases[2].Name != "http" {
		t.Fatalf("phases = %#v", result.Phases)
	}
	for _, phase := range result.Phases {
		if phase.Status != "ok" {
			t.Fatalf("phase = %#v", phase)
		}
	}
	if result.HTTP == nil || result.HTTP.StatusCode != http.StatusOK {
		t.Fatalf("http = %#v", result.HTTP)
	}
	if strings.Contains(stdout.String(), "private response body") || strings.Contains(stdout.String(), "token=private") {
		t.Fatalf("private content leaked: %s", stdout.String())
	}
}

func TestRunDoesNotFollowRedirect(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path == "/start" {
			http.Redirect(writer, request, "/destination", http.StatusFound)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", server.URL + "/start"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d; redirect was followed", requests.Load())
	}
	var result checkResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.HTTP == nil || result.HTTP.StatusCode != http.StatusFound {
		t.Fatalf("http = %#v", result.HTTP)
	}
	phase := result.Phases[len(result.Phases)-1]
	if phase.Status != "failed" || phase.Error != "redirect not followed" {
		t.Fatalf("http phase = %#v", phase)
	}
}

func TestStrictTLSRejectsUntrustedCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", server.URL}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
	}
	var result checkResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	tlsPhase := result.Phases[2]
	if tlsPhase.Name != "tls" || tlsPhase.Status != "failed" {
		t.Fatalf("tls phase = %#v", tlsPhase)
	}
	if result.Certificate != nil {
		t.Fatalf("certificate reported after failed verification: %#v", result.Certificate)
	}
}

func TestTrustedTLSReportsCertificateMetadata(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	spec, err := parseTarget(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, timedOut := checkTarget(ctx, spec, time.Second, pool)
	if timedOut {
		t.Fatal("unexpected timeout")
	}
	for _, phase := range result.Phases {
		if phase.Status != "ok" {
			t.Fatalf("phase = %#v", phase)
		}
	}
	if result.Certificate == nil || result.Certificate.SANCount == 0 || result.Certificate.ExpiresAt == "" {
		t.Fatalf("certificate = %#v", result.Certificate)
	}
}

func TestRunHostAndPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			close(accepted)
			conn.Close()
		}
	}()

	var stdout, stderr bytes.Buffer
	code := run([]string{listener.Addr().String()}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("listener did not accept connection")
	}
	if !strings.Contains(stdout.String(), "dns: ok") || !strings.Contains(stdout.String(), "tcp: ok") {
		t.Fatalf("output = %q", stdout.String())
	}
}

func TestTLSHandshakeTimeoutReturnsThree(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx := newTriggeredDeadlineContext()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			defer conn.Close()
			var firstClientHelloByte [1]byte
			if _, readErr := conn.Read(firstClientHelloByte[:]); readErr == nil {
				ctx.expire()
			}
		}
	}()

	target := "https://" + listener.Addr().String()
	spec, err := parseTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	result, timedOut := checkTarget(ctx, spec, time.Second, nil)
	if code := resultExitCode(result, timedOut); code != 3 {
		t.Fatalf("code = %d, timedOut = %v, phases = %#v", code, timedOut, result.Phases)
	}
	if result.Phases[2].Name != "tls" || result.Phases[2].Status != "failed" {
		t.Fatalf("phases = %#v", result.Phases)
	}
}

type triggeredDeadlineContext struct {
	done chan struct{}
	once sync.Once
}

func newTriggeredDeadlineContext() *triggeredDeadlineContext {
	return &triggeredDeadlineContext{done: make(chan struct{})}
}

func (ctx *triggeredDeadlineContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (ctx *triggeredDeadlineContext) Done() <-chan struct{} {
	return ctx.done
}

func (ctx *triggeredDeadlineContext) Err() error {
	select {
	case <-ctx.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (ctx *triggeredDeadlineContext) Value(any) any {
	return nil
}

func (ctx *triggeredDeadlineContext) expire() {
	ctx.once.Do(func() { close(ctx.done) })
}

func TestRunUsageErrors(t *testing.T) {
	tests := [][]string{
		nil,
		{"one", "two"},
		{"--timeout=0", "localhost"},
		{"ftp://example.com"},
		{"https://"},
		{"https://user:secret@example.com"},
		{"192.0.2.0/24"},
		{"example.com:0"},
		{"example.com:abc"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 2 {
				t.Fatalf("run(%q) = %d, stderr = %q", args, code, stderr.String())
			}
		})
	}
}

func TestRunHelpReturnsZero(t *testing.T) {
	for _, argument := range []string{"-h", "--help"} {
		t.Run(argument, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run([]string{argument}, &stdout, &stderr); code != 0 {
				t.Fatalf("run(%q) = %d, stderr = %q", argument, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "usage: netcheck") {
				t.Fatalf("missing usage output: %q", stderr.String())
			}
		})
	}
}

func TestParseTargetModes(t *testing.T) {
	tests := []struct {
		input    string
		wantPort string
		wantTLS  bool
		wantHTTP bool
	}{
		{"localhost", "", false, false},
		{"localhost:80", "80", false, false},
		{"localhost:443", "443", true, false},
		{"http://localhost/path", "80", false, true},
		{"https://localhost/path", "443", true, true},
		{"[::1]:443", "443", true, false},
		{"::1", "", false, false},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			spec, err := parseTarget(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if spec.port != test.wantPort || spec.useTLS != test.wantTLS || spec.useHTTP != test.wantHTTP {
				t.Fatalf("spec = %#v", spec)
			}
		})
	}
}
