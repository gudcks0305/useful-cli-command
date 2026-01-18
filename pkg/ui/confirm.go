package ui

import (
	"bufio"
	"fmt"
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

// Prompt 확인 메시지를 표시하고 사용자 응답을 반환합니다.
func (c *Confirmation) Prompt() bool {
	reader := bufio.NewReader(os.Stdin)

	if c.Default {
		fmt.Printf("%s (Y/n): ", c.Message)
	} else {
		fmt.Printf("%s (y/N): ", c.Message)
	}

	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))

	if answer == "" {
		return c.Default
	}

	return answer == c.Accepted
}

// MustConfirm 확인 메시지를 표시하고 응답이 긍정적일 때까지 반복합니다.
func (c *Confirmation) MustConfirm() bool {
	for {
		if c.Prompt() {
			return true
		}
		fmt.Println("취소되었습니다.")
		return false
	}
}
