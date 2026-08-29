package common

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestOutputFunctions(t *testing.T) {
	tests := []struct {
		name string
		call func()
		want string
	}{
		{"success", func() { Success("done %d", 2) }, Green + "✓ done 2" + Reset + "\n"},
		{"error", func() { Error("bad") }, Red + "✗ bad" + Reset + "\n"},
		{"warning", func() { Warning("careful") }, Yellow + "⚠ careful" + Reset + "\n"},
		{"info", func() { Info("note") }, Cyan + "ℹ note" + Reset + "\n"},
		{"header", func() { Header("head") }, Bold + "head" + Reset + "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := captureStdout(t, test.call)
			if got != test.want {
				t.Fatalf("output = %q, want %q", got, test.want)
			}
		})
	}
}

func captureStdout(t *testing.T, call func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = original })
	call()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return strings.Clone(string(out))
}
