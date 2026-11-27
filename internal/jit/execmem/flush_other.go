//go:build (!darwin && !linux) || !arm64 || moeluanojit

package execmem

func flushICache(_, _ uintptr) {}
