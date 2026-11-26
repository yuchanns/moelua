//go:build unix

package main

import (
	"os"
	"runtime"
	"syscall"
	"time"
)

const selfCPUSupported = true

func getSelfCPU() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	return time.Duration(int64(ru.Utime.Sec)+int64(ru.Stime.Sec))*time.Second +
		time.Duration(int64(ru.Utime.Usec)+int64(ru.Stime.Usec))*time.Microsecond, true
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
