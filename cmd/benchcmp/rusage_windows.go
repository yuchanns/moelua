//go:build windows

package main

import (
	"os"
	"syscall"
	"time"
)

func filetimeDuration(ft syscall.Filetime) time.Duration {
	ticks := uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
	return time.Duration(ticks) * 100
}

func getSelfCPU() (time.Duration, bool) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, false
	}
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	return filetimeDuration(user) + filetimeDuration(kernel), true
}

func rusageMaxRSSBytes(_ *os.ProcessState) int64 {
	return 0
}
