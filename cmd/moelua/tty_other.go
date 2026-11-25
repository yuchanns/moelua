//go:build !darwin && !freebsd && !netbsd && !openbsd && !linux && !windows

package main

func isStdinTerminal() bool {
	return false
}
