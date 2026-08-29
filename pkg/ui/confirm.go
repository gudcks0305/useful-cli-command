package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// ConfirmConfirmation 은 사용자에게 확인 메시지를 표시하고 응답을 반환합니다.
type Confirmation struct {
	Message  string
	Default  bool   // 기본값 (Enter를 누른 경우)
	Accepted string // 긍정 응답 문자열
	Rejected string // 부정 응답 문자열
}

// DefaultConfirmation 은 기본 확인 프롬프트를 생성합니다.
func DefaultConfirmation(message string) *Confirmation {
	return &Confirmation{
		Message:  message,
		Default:  false,
		Accepted: "y",
		Rejected: "n",
	}
}

// YesNoConfirmation 은 y/n 확인 프롬프트를 생성합니다.
func YesNoConfirmation(message string) *Confirmation {
	return &Confirmation{
		Message:  message,
		Default:  false,
		Accepted: "y",
		Rejected: "n",
	}
}

// ConfirmYesNo prompts once for a yes/no answer. It is the compatibility form
// of ConfirmYesNoE; I/O errors reject the prompt.
func ConfirmYesNo(message string, input io.Reader, output io.Writer) bool {
	confirmed, _ := ConfirmYesNoE(message, input, output)
	return confirmed
}

// ConfirmYesNoE prompts once and accepts "y" or "yes" case-insensitively.
// "n", "no", empty input, unknown input, and EOF without an answer reject
// normally with a nil error. Non-EOF read errors and write errors are returned.
func ConfirmYesNoE(message string, input io.Reader, output io.Writer) (bool, error) {
	return YesNoConfirmation(message).PromptFromE(input, output)
}

// Prompt 확인 메시지를 표시하고 사용자 응답을 반환합니다.
func (c *Confirmation) Prompt() bool {
	confirmed, _ := c.PromptE()
	return confirmed
}

// PromptE prompts using standard streams and preserves I/O errors.
func (c *Confirmation) PromptE() (bool, error) {
	return c.PromptFromE(os.Stdin, os.Stdout)
}

// PromptFrom is the compatibility form of PromptFromE. I/O errors reject the prompt.
func (c *Confirmation) PromptFrom(input io.Reader, output io.Writer) bool {
	confirmed, _ := c.PromptFromE(input, output)
	return confirmed
}

// PromptFromE reads and writes through supplied streams and preserves I/O errors.
// EOF is accepted when it follows an answer; an empty EOF applies Default.
func (c *Confirmation) PromptFromE(input io.Reader, output io.Writer) (bool, error) {
	reader := bufio.NewReader(input)

	if c.Default {
		if _, err := fmt.Fprintf(output, "%s (Y/n): ", c.Message); err != nil {
			return false, err
		}
	} else {
		if _, err := fmt.Fprintf(output, "%s (y/N): ", c.Message); err != nil {
			return false, err
		}
	}

	answer, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	answer = strings.TrimSpace(strings.ToLower(answer))

	if answer == "" {
		return c.Default, nil
	}

	accepted := strings.ToLower(strings.TrimSpace(c.Accepted))
	if accepted == "y" || accepted == "yes" {
		return answer == "y" || answer == "yes", nil
	}
	return answer == accepted, nil
}

// MustConfirm prompts once, prints cancellation, and rejects on I/O errors.
func (c *Confirmation) MustConfirm() bool {
	confirmed, _ := c.MustConfirmE()
	return confirmed
}

// MustConfirmE prompts once using standard streams and preserves I/O errors.
func (c *Confirmation) MustConfirmE() (bool, error) {
	return c.MustConfirmFromE(os.Stdin, os.Stdout)
}

// MustConfirmFromE prompts once and writes a cancellation message for a normal rejection.
func (c *Confirmation) MustConfirmFromE(input io.Reader, output io.Writer) (bool, error) {
	confirmed, err := c.PromptFromE(input, output)
	if err != nil || confirmed {
		return confirmed, err
	}
	if _, err := fmt.Fprintln(output, "취소되었습니다."); err != nil {
		return false, err
	}
	return false, nil
}
