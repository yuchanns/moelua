//go:build !unix

package main

import (
	"os"
	"time"
)

func getSelfCPU() time.Duration {
	return time.Duration(time.Now().UnixNano())
}

func rusageMaxRSSBytes(_ *os.ProcessState) int64 {
	return 0
}
