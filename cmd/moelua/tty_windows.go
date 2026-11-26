//go:build windows

package main

import (
	"os"
	"syscall"
)

func isStdinTerminal() bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(os.Stdin.Fd()), &mode) == nil
}
