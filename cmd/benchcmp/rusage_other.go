//go:build !unix && !windows

package main

import (
	"os"
	"time"
)

func getSelfCPU() (time.Duration, bool) {
	return 0, false
}

func rusageMaxRSSBytes(_ *os.ProcessState) int64 {
	return 0
}
