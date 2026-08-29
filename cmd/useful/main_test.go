package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestSortedCommandNames(t *testing.T) {
	registry := map[string]Command{
		"zeta":  {},
		"alpha": {},
		"mid":   {},
	}

	got := sortedCommandNames(registry)
	want := []string{"alpha", "mid", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedCommandNames() = %v, want %v", got, want)
	}

	if !sort.StringsAreSorted(got) {
		t.Fatalf("command names are not sorted: %v", got)
	}
}

func TestResolveCommandPathFallsBackFromUnusableSibling(t *testing.T) {
	launcherDir := t.TempDir()
	pathDir := t.TempDir()
	launcher := filepath.Join(launcherDir, "useful")
	pathCommand := filepath.Join(pathDir, "gitstats")
	if err := os.WriteFile(pathCommand, []byte("executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)

	for _, setup := range []func() error{
		func() error { return os.Mkdir(filepath.Join(launcherDir, "gitstats"), 0o755) },
		func() error {
			if err := os.RemoveAll(filepath.Join(launcherDir, "gitstats")); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(launcherDir, "gitstats"), []byte("not executable"), 0o644)
		},
	} {
		if err := setup(); err != nil {
			t.Fatal(err)
		}
		got, err := resolveCommandPath(launcher, "gitstats")
		if err != nil {
			t.Fatal(err)
		}
		if got != pathCommand {
			t.Fatalf("resolveCommandPath() = %q, want %q", got, pathCommand)
		}
	}
}

func TestResolveCommandPathPrefersExecutableSibling(t *testing.T) {
	launcherDir := t.TempDir()
	launcher := filepath.Join(launcherDir, "useful")
	sibling := filepath.Join(launcherDir, "gitstats")
	if err := os.WriteFile(sibling, []byte("executable"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveCommandPath(launcher, "gitstats")
	if err != nil {
		t.Fatal(err)
	}
	if got != sibling {
		t.Fatalf("resolveCommandPath() = %q, want %q", got, sibling)
	}
}

func TestExecuteCommandReturnsLaunchError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := executeCommand(filepath.Join(t.TempDir(), "missing"), nil, bytes.NewReader(nil), &stdout, &stderr)
	if err == nil || code != 1 {
		t.Fatalf("executeCommand() = (%d, %v), want (1, error)", code, err)
	}
}

func TestRegistryContainsDiagnosticCommands(t *testing.T) {
	want := []string{
		"bintrace",
		"dedupe",
		"gitdoctor",
		"hashmanifest",
		"launchaudit",
		"netcheck",
		"pathdoctor",
		"symaudit",
	}
	for _, name := range want {
		if _, ok := commands[name]; !ok {
			t.Errorf("commands missing %q", name)
		}
	}
}
