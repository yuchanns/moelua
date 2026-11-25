//go:build windows

package vm

import (
	"os/exec"
	"syscall"
)

func execExitInfo(ee *exec.ExitError) (kind string, code int64) {
	return "exit", int64(ee.ExitCode())
}

func processCPUTime() (float64, bool) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, false
	}
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	kt := uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)
	ut := uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime)
	return float64(kt+ut) * 1e-7, true
}
