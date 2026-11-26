package main

import (
	"runtime"
	"testing"
)

func TestGetSelfCPU(t *testing.T) {
	cpu0, ok := getSelfCPU()
	if selfCPUSupported {
		if !ok {
			t.Fatalf("getSelfCPU() unavailable on %s", runtime.GOOS)
		}
		if cpu0 < 0 {
			t.Fatalf("getSelfCPU() returned negative duration: %v", cpu0)
		}
		var acc uint64
		for i := range uint64(1_000_000) {
			acc += i * 3
		}
		_ = acc
		cpu1, ok := getSelfCPU()
		if !ok {
			t.Fatalf("second getSelfCPU() failed on %s", runtime.GOOS)
		}
		if cpu1 < cpu0 {
			t.Fatalf("getSelfCPU() regressed: %v -> %v", cpu0, cpu1)
		}
		return
	}
	if ok {
		t.Fatalf("expected getSelfCPU() unavailable on %s", runtime.GOOS)
	}
}
