//go:build unix

package main

import (
	"os"
	"runtime"
	"syscall"
	"time"
)

func getSelfCPU() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Sec+ru.Stime.Sec)*time.Second +
		time.Duration(ru.Utime.Usec+ru.Stime.Usec)*time.Microsecond
}

func rusageMaxRSSBytes(ps *os.ProcessState) int64 {
	if ps == nil {
		return 0
	}
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss) // macOS reports bytes
	}
	return int64(ru.Maxrss) * 1024 // Linux / BSD report KiB
}
