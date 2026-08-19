package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseNumberSelection(t *testing.T) {
	got, err := parseNumberSelection("3,1-2,2", 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 1, 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if _, err := parseNumberSelection("2-5", 4); err == nil {
		t.Fatal("expected out-of-range error")
	}
}

func TestSelectConfiguredTargets(t *testing.T) {
	targets := []CleanTarget{
		{ID: "safe", Risk: riskSafe},
		{ID: "optional", Risk: riskOptional},
		{ID: "docker", Risk: riskExplicit, Kind: kindDocker},
	}
	got, err := selectConfiguredTargets(targets, options{})
	if err != nil || len(got) != 1 || got[0].ID != "safe" {
		t.Fatalf("default = %#v, %v", got, err)
	}
	got, err = selectConfiguredTargets(targets, options{only: "docker,safe,docker"})
	if err != nil || len(got) != 2 || got[0].ID != "docker" || got[1].ID != "safe" {
		t.Fatalf("only = %#v, %v", got, err)
	}
}

func TestRemoveContentsPreservesRootAndRemovesDotfiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".hidden"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := removeContents(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("remaining entries: %v", entries)
	}
}

func TestRemoveContentsRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := removeContents(link); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestParseDockerSize(t *testing.T) {
	tests := map[string]int64{"0B": 0, "1.5kB": 1500, "2MB": 2_000_000, "1GiB": 1 << 30}
	for input, want := range tests {
		got, err := parseDockerSize(input)
		if err != nil || got != want {
			t.Fatalf("%s = %d, %v; want %d", input, got, err, want)
		}
	}
}

func TestParseDockerDFExcludesVolumes(t *testing.T) {
	output := []byte("{\"Type\":\"Images\",\"Reclaimable\":\"1.5GB (50%)\"}\n" +
		"{\"Type\":\"Local Volumes\",\"Reclaimable\":\"9GB (90%)\"}\n" +
		"{\"Type\":\"Build Cache\",\"Reclaimable\":\"500MB\"}\n")
	got, err := parseDockerDF(output)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2_000_000_000 {
		t.Fatalf("got %d", got)
	}
}

func TestInspectArtifactReturnsError(t *testing.T) {
	_, _, err := inspectArtifact(filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("expected missing artifact error")
	}
}

func TestNonEmptyResultsExcludesAnalysisErrors(t *testing.T) {
	results := []AnalysisResult{
		{Target: CleanTarget{ID: "first"}, Size: 10},
		{Target: CleanTarget{ID: "broken"}, Size: 20, Error: os.ErrPermission},
		{Target: CleanTarget{ID: "third"}, Size: 30},
	}
	got := nonEmptyResults(results)
	if len(got) != 2 || got[0].Target.ID != "first" || got[1].Target.ID != "third" {
		t.Fatalf("selectable results = %#v", got)
	}
}

func TestScanProjectArtifactsFindsOldVenv(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	venv := filepath.Join(project, ".venv")
	if err := os.MkdirAll(venv, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(venv, "pyvenv.cfg")
	if err := os.WriteFile(marker, []byte("home = test"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(venv, old, old); err != nil {
		t.Fatal(err)
	}
	artifacts, err := scanProjectArtifacts(root, 4, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Path != venv {
		t.Fatalf("artifacts = %#v", artifacts)
	}
}

func TestParallelMapPreservesOrderAndLimit(t *testing.T) {
	items := []int{5, 4, 3, 2, 1}
	var active atomic.Int32
	var peak atomic.Int32
	got := parallelMap(items, 2, func(value int) int {
		current := active.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(time.Duration(value) * time.Millisecond)
		active.Add(-1)
		return value * 2
	})
	if !reflect.DeepEqual(got, []int{10, 8, 6, 4, 2}) {
		t.Fatalf("order changed: %v", got)
	}
	if peak.Load() > 2 {
		t.Fatalf("peak workers = %d", peak.Load())
	}
}
