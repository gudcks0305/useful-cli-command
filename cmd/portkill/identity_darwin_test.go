//go:build darwin

package main

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestDarwinIdentityResolverReturnsStableSubsecondStart(t *testing.T) {
	resolver := darwinIdentityResolver{}
	first, err := resolver.Resolve(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolver.Resolve(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("identity changed: %q -> %q", first, second)
	}
	parts := strings.Split(first, ".")
	if len(parts) != 2 || len(parts[1]) != 6 {
		t.Fatalf("identity lacks microseconds: %q", first)
	}
	if _, err := strconv.ParseUint(parts[0], 10, 64); err != nil {
		t.Fatalf("seconds %q: %v", parts[0], err)
	}
	if _, err := strconv.ParseUint(parts[1], 10, 64); err != nil {
		t.Fatalf("microseconds %q: %v", parts[1], err)
	}
}
