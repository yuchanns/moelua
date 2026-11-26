//go:build windows

package vm

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func configureCancelableCmd(cmd *exec.Cmd) {
	// Terminate the entire Windows process tree (cmd.exe + descendants holding inherited
	// stdin/stdout pipes open) on context cancellation before falling back to Process.Kill.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		tkCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		tk := exec.CommandContext(tkCtx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		tk.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := tk.Run(); err == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 100 * time.Millisecond
}

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
