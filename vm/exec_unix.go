//go:build unix

package vm

import (
	"os/exec"
	"syscall"
)

func execExitInfo(ee *exec.ExitError) (kind string, code int64) {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return "signal", int64(ws.Signal())
	}
	return "exit", int64(ee.ExitCode())
}

func processCPUTime() (float64, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	sec := float64(ru.Utime.Sec) + float64(ru.Utime.Usec)*1e-6 +
		float64(ru.Stime.Sec) + float64(ru.Stime.Usec)*1e-6
	return sec, true
}
