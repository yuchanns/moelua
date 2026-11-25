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
