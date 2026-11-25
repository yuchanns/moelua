//go:build !unix

package vm

import "os/exec"

func execExitInfo(ee *exec.ExitError) (kind string, code int64) {
	return "exit", int64(ee.ExitCode())
}
