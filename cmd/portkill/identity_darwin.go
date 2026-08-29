//go:build darwin

package main

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	procInfoCallPIDInfo = 2
	procPIDTBSDInfo     = 3
)

// procBSDInfoStart matches the 120-byte prefix and sub-second start fields of
// Darwin's proc_bsdinfo. Keep its size at 136 bytes: SYS_PROC_INFO requires the
// complete PROC_PIDTBSDINFO buffer even though only the final fields are used.
type procBSDInfoStart struct {
	_                 [120]byte
	startSeconds      uint64
	startMicroseconds uint64
}

type darwinIdentityResolver struct{}

func newIdentityResolver() identityResolver { return darwinIdentityResolver{} }

func (darwinIdentityResolver) Resolve(pid int) (string, error) {
	var info procBSDInfoStart
	bufferSize := unsafe.Sizeof(info)
	written, _, errno := syscall.Syscall6(
		syscall.SYS_PROC_INFO,
		procInfoCallPIDInfo,
		uintptr(pid),
		procPIDTBSDInfo,
		0,
		uintptr(unsafe.Pointer(&info)),
		bufferSize,
	)
	runtime.KeepAlive(&info)
	if errno != 0 {
		return "", fmt.Errorf("proc_pidinfo PID %d 실패: %w", pid, errno)
	}
	if written < bufferSize {
		return "", fmt.Errorf("proc_pidinfo PID %d 짧은 응답: %d/%d bytes", pid, written, bufferSize)
	}
	if info.startSeconds == 0 || info.startMicroseconds >= 1_000_000 {
		return "", fmt.Errorf("proc_pidinfo PID %d 유효하지 않은 시작 시각: %d.%06d", pid, info.startSeconds, info.startMicroseconds)
	}
	return fmt.Sprintf("%d.%06d", info.startSeconds, info.startMicroseconds), nil
}
