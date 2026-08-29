package ui

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

var errInput = errors.New("input failed")

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errInput }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestPromptFrom(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		confirm Confirmation
		want    bool
		prompt  string
	}{
		{"default yes", "\n", Confirmation{Message: "Go?", Default: true, Accepted: "y"}, true, "Go? (Y/n): "},
		{"default no", "\n", Confirmation{Message: "Go?", Default: false, Accepted: "y"}, false, "Go? (y/N): "},
		{"case insensitive without newline", "YES", Confirmation{Message: "Go?", Accepted: "yes"}, true, "Go? (y/N): "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if got := test.confirm.PromptFrom(strings.NewReader(test.input), &output); got != test.want {
				t.Fatalf("PromptFrom = %v, want %v", got, test.want)
			}
			if output.String() != test.prompt {
				t.Fatalf("prompt = %q, want %q", output.String(), test.prompt)
			}
		})
	}
}

func TestConfirmYesNoE(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"short yes", "y\n", true},
		{"full yes", "YES\n", true},
		{"no", "n\n", false},
		{"empty", "\n", false},
		{"EOF", "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			confirmed, err := ConfirmYesNoE("Go?", strings.NewReader(test.input), &output)
			if err != nil || confirmed != test.want {
				t.Fatalf("ConfirmYesNoE = %v, %v; want %v", confirmed, err, test.want)
			}
			if got := output.String(); got != "Go? (y/N): " {
				t.Fatalf("prompt = %q", got)
			}
		})
	}
}

func TestConfirmYesNoEIOErrors(t *testing.T) {
	if _, err := ConfirmYesNoE("Go?", failingReader{}, io.Discard); !errors.Is(err, errInput) {
		t.Fatalf("read error = %v", err)
	}
	if _, err := ConfirmYesNoE("Go?", strings.NewReader("yes\n"), failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write error = %v", err)
	}
	if ConfirmYesNo("Go?", failingReader{}, io.Discard) {
		t.Fatal("legacy wrapper accepted read error")
	}
}

func TestPromptFromEPreservesIOErrors(t *testing.T) {
	confirm := YesNoConfirmation("Go?")
	if _, err := confirm.PromptFromE(failingReader{}, io.Discard); !errors.Is(err, errInput) {
		t.Fatalf("read error = %v", err)
	}
	if _, err := confirm.PromptFromE(strings.NewReader("y\n"), failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write error = %v", err)
	}
}

func TestMustConfirmFromERejectsOnce(t *testing.T) {
	var output bytes.Buffer
	confirmed, err := YesNoConfirmation("Go?").MustConfirmFromE(strings.NewReader("n\n"), &output)
	if err != nil || confirmed {
		t.Fatalf("MustConfirmFromE = %v, %v", confirmed, err)
	}
	if got := output.String(); got != "Go? (y/N): 취소되었습니다.\n" {
		t.Fatalf("output = %q", got)
	}
}
