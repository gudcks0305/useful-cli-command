//go:build !windows

package main

import (
	"os"
	"syscall"
)

func terminationSignal(force bool) (os.Signal, string) {
	if force {
		return syscall.SIGKILL, "SIGKILL"
	}
	return syscall.SIGTERM, "SIGTERM"
}
