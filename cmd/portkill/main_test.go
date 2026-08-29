package main

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

type finderResult struct {
	owners []processOwner
	err    error
}

type fakeFinder struct {
	results []finderResult
	calls   []int
}

func (f *fakeFinder) Find(port int) ([]processOwner, error) {
	f.calls = append(f.calls, port)
	if len(f.results) == 0 {
		return nil, errors.New("unexpected finder call")
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result.owners, result.err
}

func testOwners(pids ...int) []processOwner {
	owners := make([]processOwner, 0, len(pids))
	for _, pid := range pids {
		owners = append(owners, processOwner{PID: pid, StartedAt: "Sat Aug 29 18:34:46 2026"})
	}
	return owners
}

type signalCall struct {
	pid    int
	signal os.Signal
}

type fakeSignaler struct {
	calls  []signalCall
	errors map[int]error
}

func (s *fakeSignaler) Signal(pid int, signal os.Signal) error {
	s.calls = append(s.calls, signalCall{pid: pid, signal: signal})
	return s.errors[pid]
}

func runTest(t *testing.T, args []string, input string, finder *fakeFinder, signaler *fakeSignaler, selfPID int) (int, string, string) {
	t.Helper()
	var stdout strings.Builder
	var stderr strings.Builder
	code := runWithDependencies(args, strings.NewReader(input), &stdout, &stderr, dependencies{
		finder:   finder,
		signaler: signaler,
		selfPID:  selfPID,
	})
	return code, stdout.String(), stderr.String()
}

func TestParseArgsRejectsInvalidPortsAndArguments(t *testing.T) {
	t.Parallel()
	tests := [][]string{
		nil,
		{"0"},
		{"65536"},
		{"abc"},
		{"--force"},
		{"3000", "--force"},
		{"--unknown", "3000"},
		{"3000", "extra"},
	}
	for _, args := range tests {
		args := args
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			t.Parallel()
			if _, err := parseArgs(args); err == nil {
				t.Fatalf("parseArgs(%v) succeeded", args)
			}
		})
	}

	for _, args := range [][]string{{"1"}, {"65535"}, {"--force", "3000"}} {
		if _, err := parseArgs(args); err != nil {
			t.Fatalf("parseArgs(%v): %v", args, err)
		}
	}
}

func TestRunHelpUsesStdoutAndReturnsSuccess(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			finder := &fakeFinder{}
			signaler := &fakeSignaler{}
			code, stdout, stderr := runTest(t, []string{arg}, "", finder, signaler, 999)
			if code != 0 || stderr != "" {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
			if !strings.Contains(stdout, "사용법: portkill [--force] <port>") {
				t.Fatalf("stdout=%q", stdout)
			}
			if len(finder.calls) != 0 || len(signaler.calls) != 0 {
				t.Fatalf("finder=%v signals=%+v", finder.calls, signaler.calls)
			}
		})
	}
}

func TestRunInvalidHelpCombinationUsesStderrAndFails(t *testing.T) {
	finder := &fakeFinder{}
	signaler := &fakeSignaler{}
	code, stdout, stderr := runTest(t, []string{"--help", "3000"}, "", finder, signaler, 999)
	if code == 0 || stdout != "" {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
	if !strings.Contains(stderr, "오류:") || !strings.Contains(stderr, "사용법:") {
		t.Fatalf("stderr=%q", stderr)
	}
}

func TestRunCancelDoesNotSignal(t *testing.T) {
	finder := &fakeFinder{results: []finderResult{{owners: testOwners(20, 10)}}}
	signaler := &fakeSignaler{}
	code, stdout, stderr := runTest(t, []string{"3000"}, "n\n", finder, signaler, 999)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if len(signaler.calls) != 0 {
		t.Fatalf("unexpected signals: %+v", signaler.calls)
	}
	if !strings.Contains(stdout, "PID 20") || !strings.Contains(stdout, "취소") {
		t.Fatalf("missing snapshot/cancel output: %q", stdout)
	}
}

func TestRunNoOwnersIsSuccessfulAndDoesNotPrompt(t *testing.T) {
	finder := &fakeFinder{results: []finderResult{{}}}
	signaler := &fakeSignaler{}
	code, stdout, stderr := runTest(t, []string{"3000"}, "", finder, signaler, 999)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if len(signaler.calls) != 0 || !strings.Contains(stdout, "바인딩된 프로세스가 없습니다") {
		t.Fatalf("signals=%+v stdout=%q", signaler.calls, stdout)
	}
}

func TestRunRevalidatesAndSkipsDisappearedOrReplacementPID(t *testing.T) {
	finder := &fakeFinder{results: []finderResult{
		{owners: testOwners(10, 20)},
		{owners: testOwners(20, 30)},
		{owners: testOwners(20, 30)},
	}}
	signaler := &fakeSignaler{}
	code, stdout, stderr := runTest(t, []string{"3000"}, "yes\n", finder, signaler, 999)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	want := []signalCall{{pid: 20, signal: syscall.SIGTERM}}
	if !reflect.DeepEqual(signaler.calls, want) {
		t.Fatalf("signals=%+v want=%+v", signaler.calls, want)
	}
	if !strings.Contains(stdout, "PID 10: 더 이상") || !strings.Contains(stdout, "PID 30: 확인 후") {
		t.Fatalf("missing race messages: %q", stdout)
	}
}

func TestRunRefusesReusedPIDStillOwningSamePort(t *testing.T) {
	oldOwner := processOwner{PID: 10, StartedAt: "1787999686.000001"}
	newOwner := processOwner{PID: 10, StartedAt: "1787999686.000002"}
	finder := &fakeFinder{results: []finderResult{
		{owners: []processOwner{oldOwner}},
		{owners: []processOwner{newOwner}},
	}}
	signaler := &fakeSignaler{}
	code, stdout, stderr := runTest(t, []string{"3000"}, "y\n", finder, signaler, 999)
	if code != 1 {
		t.Fatalf("code=%d want=1", code)
	}
	if len(signaler.calls) != 0 {
		t.Fatalf("reused PID received signal: %+v", signaler.calls)
	}
	if !strings.Contains(stdout, oldOwner.StartedAt) || !strings.Contains(stderr, "PID 재사용 가능성") {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestRunRequeriesBeforeEachIndividualSignal(t *testing.T) {
	finder := &fakeFinder{results: []finderResult{
		{owners: testOwners(10, 20)},
		{owners: testOwners(10, 20)},
		{owners: testOwners(20)},
	}}
	signaler := &fakeSignaler{}
	code, _, stderr := runTest(t, []string{"3000"}, "y\n", finder, signaler, 999)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if len(finder.calls) != 3 {
		t.Fatalf("finder calls=%v, want initial + one per signal", finder.calls)
	}
	want := []signalCall{{pid: 10, signal: syscall.SIGTERM}, {pid: 20, signal: syscall.SIGTERM}}
	if !reflect.DeepEqual(signaler.calls, want) {
		t.Fatalf("signals=%+v want=%+v", signaler.calls, want)
	}
}

func TestRunFailsClosedWhenIdentityIsUnavailable(t *testing.T) {
	ownerWithoutIdentity := processOwner{PID: 10}
	finder := &fakeFinder{results: []finderResult{
		{owners: []processOwner{ownerWithoutIdentity}},
		{owners: []processOwner{ownerWithoutIdentity}},
	}}
	signaler := &fakeSignaler{}
	code, _, stderr := runTest(t, []string{"3000"}, "y\n", finder, signaler, 999)
	if code != 1 || !strings.Contains(stderr, "identity를 확인할 수 없어") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if len(signaler.calls) != 0 {
		t.Fatalf("identity-less owner received signal: %+v", signaler.calls)
	}
}

func TestRunUsesSIGTERMUnlessForceIsExplicit(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want os.Signal
	}{
		{name: "default", args: []string{"3000"}, want: syscall.SIGTERM},
		{name: "force", args: []string{"--force", "3000"}, want: syscall.SIGKILL},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			finder := &fakeFinder{results: []finderResult{{owners: testOwners(10)}, {owners: testOwners(10)}}}
			signaler := &fakeSignaler{}
			code, _, stderr := runTest(t, test.args, "y\n", finder, signaler, 999)
			if code != 0 || stderr != "" {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
			want := []signalCall{{pid: 10, signal: test.want}}
			if !reflect.DeepEqual(signaler.calls, want) {
				t.Fatalf("signals=%+v want=%+v", signaler.calls, want)
			}
		})
	}
}

func TestRunRefusesSelfAndReportsPartialSignalFailure(t *testing.T) {
	finder := &fakeFinder{results: []finderResult{
		{owners: testOwners(10, 20, 30)},
		{owners: testOwners(10, 20, 30)},
		{owners: testOwners(10, 20, 30)},
		{owners: testOwners(10, 20, 30)},
	}}
	signaler := &fakeSignaler{errors: map[int]error{30: errors.New("permission denied")}}
	code, _, stderr := runTest(t, []string{"3000"}, "y\n", finder, signaler, 20)
	if code != 1 {
		t.Fatalf("code=%d want=1", code)
	}
	want := []signalCall{{pid: 10, signal: syscall.SIGTERM}, {pid: 30, signal: syscall.SIGTERM}}
	if !reflect.DeepEqual(signaler.calls, want) {
		t.Fatalf("signals=%+v want=%+v", signaler.calls, want)
	}
	if !strings.Contains(stderr, "자신의 PID") || !strings.Contains(stderr, "permission denied") {
		t.Fatalf("missing failure output: %q", stderr)
	}
}

func TestRunFinderFailuresAreNonzeroAndNeverSignal(t *testing.T) {
	tests := []struct {
		name    string
		results []finderResult
		input   string
		want    string
	}{
		{name: "initial unavailable", results: []finderResult{{err: errLsofUnavailable}}, want: "lsof를 찾을 수 없습니다"},
		{name: "initial partial", results: []finderResult{{owners: testOwners(10), err: errors.New("UDP query failed")}}, want: "UDP query failed"},
		{name: "revalidation", results: []finderResult{{owners: testOwners(10)}, {err: errors.New("recheck failed")}}, input: "y\n", want: "재확인 실패"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			finder := &fakeFinder{results: test.results}
			signaler := &fakeSignaler{}
			code, stdout, stderr := runTest(t, []string{"3000"}, test.input, finder, signaler, 999)
			if code != 1 || !strings.Contains(stderr, test.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if len(signaler.calls) != 0 {
				t.Fatalf("unexpected signals: %+v", signaler.calls)
			}
		})
	}
}

type runnerResult struct {
	stdout []byte
	stderr []byte
	err    error
}

type fakeRunner struct {
	results []runnerResult
	calls   [][]string
}

type fakeIdentityResolver struct {
	started map[int]string
	errors  map[int]error
	calls   []int
}

func (r *fakeIdentityResolver) Resolve(pid int) (string, error) {
	r.calls = append(r.calls, pid)
	if err := r.errors[pid]; err != nil {
		return "", err
	}
	if startedAt := r.started[pid]; startedAt != "" {
		return startedAt, nil
	}
	return "Sat Aug 29 18:34:46 2026", nil
}

func (r *fakeRunner) Run(args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	result := r.results[0]
	r.results = r.results[1:]
	return result.stdout, result.stderr, result.err
}

type fakeExitError struct{ code int }

func (e fakeExitError) Error() string { return fmt.Sprintf("exit %d", e.code) }
func (e fakeExitError) ExitCode() int { return e.code }

func lsofFields(values ...string) []byte {
	var output []byte
	for _, value := range values {
		output = append(output, value...)
		output = append(output, 0)
	}
	return output
}

func TestExecLsofRunnerReportsUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, _, err := (execLsofRunner{}).Run("-nP")
	if !errors.Is(err, errLsofUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestWithCLocaleReplacesExistingLocaleWithoutDuplicates(t *testing.T) {
	env := withCLocale([]string{"PATH=/usr/bin", "LANG=ko_KR.UTF-8", "LC_ALL=ko_KR.UTF-8", "LANG=en_US.UTF-8"})
	counts := map[string]int{}
	for _, value := range env {
		if strings.HasPrefix(value, "LANG=") || strings.HasPrefix(value, "LC_ALL=") {
			counts[value]++
		}
	}
	if counts["LANG=C"] != 1 || counts["LC_ALL=C"] != 1 || len(counts) != 2 {
		t.Fatalf("locale env=%v", env)
	}
}

func TestParsePSStartAndRealResolver(t *testing.T) {
	valid := map[string]string{
		"  Sat Aug 29 18:34:46 2026    \n": "Sat Aug 29 18:34:46 2026",
		"Sun Aug  9 01:02:03 2026\n":       "Sun Aug  9 01:02:03 2026",
	}
	for output, want := range valid {
		startedAt, err := parsePSStart([]byte(output))
		if err != nil || startedAt != want {
			t.Fatalf("output=%q startedAt=%q want=%q err=%v", output, startedAt, want, err)
		}
	}
	for _, invalid := range []string{"", "STARTED", "Sat Aug 29 18:34:46 2026\nextra"} {
		if _, err := parsePSStart([]byte(invalid)); err == nil {
			t.Fatalf("parsePSStart(%q) succeeded", invalid)
		}
	}

	t.Setenv("LC_ALL", "ko_KR.UTF-8")
	t.Setenv("LANG", "ko_KR.UTF-8")
	realStart, err := (psIdentityResolver{runner: execPSRunner{}}).Resolve(os.Getpid())
	if err != nil || realStart == "" {
		t.Fatalf("real ps identity=%q err=%v", realStart, err)
	}
}

func TestLsofFinderUsesPreciseQueriesAndDeduplicates(t *testing.T) {
	runner := &fakeRunner{results: []runnerResult{
		{stdout: lsofFields(
			"p20", "cserver", "Lalice", "f9", "PTCP", "n*:3000", "TST=LISTEN",
			"p10", "cserver2", "Lbob", "f10", "PTCP", "n127.0.0.1:3000", "TST=LISTEN",
			"p40", "cclient", "Lcarol", "f11", "PTCP", "n*:3000", "TST=ESTABLISHED",
		)},
		{stdout: lsofFields(
			"p20", "cserver", "Lalice", "f8", "PUDP", "n[::]:3000",
			"p30", "cclient", "Lcarol", "f7", "PUDP", "n127.0.0.1:40000->127.0.0.1:3000",
		)},
	}}
	identity := &fakeIdentityResolver{}
	owners, err := (lsofOwnerFinder{runner: runner, identity: identity}).Find(3000)
	if err != nil {
		t.Fatal(err)
	}
	if want := testOwners(10, 20); !reflect.DeepEqual(owners, want) {
		t.Fatalf("owners=%v want=%v", owners, want)
	}
	if want := []int{10, 20}; !reflect.DeepEqual(identity.calls, want) {
		t.Fatalf("identity calls=%v want=%v", identity.calls, want)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls=%v", runner.calls)
	}
	tcpArgs := strings.Join(runner.calls[0], " ")
	udpArgs := strings.Join(runner.calls[1], " ")
	if !strings.Contains(tcpArgs, "-nP") || !strings.Contains(tcpArgs, "-iTCP:3000") || !strings.Contains(tcpArgs, "-sTCP:LISTEN") || !strings.Contains(tcpArgs, "-FpcLuPnT0") {
		t.Fatalf("imprecise TCP args: %q", tcpArgs)
	}
	if !strings.Contains(udpArgs, "-nP") || !strings.Contains(udpArgs, "-iUDP:3000") || !strings.Contains(udpArgs, "-FpcLuPnT0") {
		t.Fatalf("imprecise UDP args: %q", udpArgs)
	}
}

func TestLsofFinderDistinguishesNoMatchUnavailableAndPartialError(t *testing.T) {
	t.Run("no match", func(t *testing.T) {
		runner := &fakeRunner{results: []runnerResult{
			{err: fakeExitError{code: 1}},
			{err: fakeExitError{code: 1}},
		}}
		owners, err := (lsofOwnerFinder{runner: runner, identity: &fakeIdentityResolver{}}).Find(3000)
		if err != nil || len(owners) != 0 {
			t.Fatalf("owners=%v err=%v", owners, err)
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		runner := &fakeRunner{results: []runnerResult{{err: errLsofUnavailable}}}
		_, err := (lsofOwnerFinder{runner: runner, identity: &fakeIdentityResolver{}}).Find(3000)
		if !errors.Is(err, errLsofUnavailable) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("partial error", func(t *testing.T) {
		runner := &fakeRunner{results: []runnerResult{
			{stdout: lsofFields("p10", "cserver", "f9", "PTCP", "n*:3000", "TST=LISTEN")},
			{stderr: []byte("permission denied"), err: fakeExitError{code: 1}},
		}}
		owners, err := (lsofOwnerFinder{runner: runner, identity: &fakeIdentityResolver{}}).Find(3000)
		if want := testOwners(10); !reflect.DeepEqual(owners, want) {
			t.Fatalf("owners=%v want=%v", owners, want)
		}
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("identity unavailable", func(t *testing.T) {
		runner := &fakeRunner{results: []runnerResult{
			{stdout: lsofFields("p10", "cserver", "f9", "PTCP", "n*:3000", "TST=LISTEN")},
			{err: fakeExitError{code: 1}},
		}}
		identity := &fakeIdentityResolver{errors: map[int]error{10: errors.New("process disappeared")}}
		owners, err := (lsofOwnerFinder{runner: runner, identity: identity}).Find(3000)
		if len(owners) != 1 || owners[0].PID != 10 || owners[0].StartedAt != "" {
			t.Fatalf("owners=%v", owners)
		}
		if err == nil || !strings.Contains(err.Error(), "identity") {
			t.Fatalf("err=%v", err)
		}
	})
}
