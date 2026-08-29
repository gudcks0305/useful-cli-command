//go:build windows

package main

import "os"

func terminationSignal(force bool) (os.Signal, string) {
	if force {
		return os.Kill, "Kill"
	}
	// Windows os.Process.Signal rejects Interrupt instead of silently turning a
	// graceful request into a force kill. --force remains required for os.Kill.
	return os.Interrupt, "Interrupt"
}
