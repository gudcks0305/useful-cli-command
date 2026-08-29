//go:build !darwin

package main

import "os"

func platformSupported() bool { return false }

func scopePaths(string) ([]string, error) { return nil, os.ErrInvalid }

func fileOwnerUID(os.FileInfo) (uint32, bool) { return 0, false }

func expectedOwner(string) (uint32, bool) { return 0, false }

func isRootDaemonPlist(string) bool { return false }
