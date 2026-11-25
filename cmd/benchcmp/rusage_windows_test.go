//go:build windows

package main

import (
	"syscall"
	"testing"
	"time"
)

func TestFiletimeDuration(t *testing.T) {
	if got := filetimeDuration(syscall.Filetime{HighDateTime: 0, LowDateTime: 10_000}); got != time.Millisecond {
		t.Fatalf("got %v, want %v", got, time.Millisecond)
	}
	wantHi := time.Duration(uint64(1)<<32) * 100
	if got := filetimeDuration(syscall.Filetime{HighDateTime: 1, LowDateTime: 0}); got != wantHi {
		t.Fatalf("got %v, want %v", got, wantHi)
	}
}
