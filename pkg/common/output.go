package common

import (
	"fmt"
	"os"
)

// ANSI color codes
const (
	Reset  = "\033[0m"
	Red    = "\033[31m"
	Green  = "\033[32m"
	Yellow = "\033[33m"
	Blue   = "\033[34m"
	Cyan   = "\033[36m"
	Bold   = "\033[1m"
)

// Success prints a success message in green
func Success(format string, args ...interface{}) {
	fmt.Printf(Green+"✓ "+format+Reset+"\n", args...)
}

// Error prints an error message in red
func Error(format string, args ...interface{}) {
	fmt.Printf(Red+"✗ "+format+Reset+"\n", args...)
}

// Warning prints a warning message in yellow
func Warning(format string, args ...interface{}) {
	fmt.Printf(Yellow+"⚠ "+format+Reset+"\n", args...)
}

// Info prints an info message in cyan
func Info(format string, args ...interface{}) {
	fmt.Printf(Cyan+"ℹ "+format+Reset+"\n", args...)
}

// Fatal prints an error message and exits
func Fatal(format string, args ...interface{}) {
	Error(format, args...)
	os.Exit(1)
}

// Header prints a bold header
func Header(format string, args ...interface{}) {
	fmt.Printf(Bold+format+Reset+"\n", args...)
}
