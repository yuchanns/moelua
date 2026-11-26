//go:build !unix && !windows

package vm

import "os/exec"

func configureCancelableCmd(_ *exec.Cmd) {}

func execExitInfo(ee *exec.ExitError) (kind string, code int64) {
	return "exit", int64(ee.ExitCode())
}

func processCPUTime() (float64, bool) {
	return 0, false
}
