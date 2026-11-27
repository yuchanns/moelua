//go:build (darwin || linux) && arm64 && !moeluanojit

package execmem

// flushICache cleans D-cache lines to PoU and invalidates I-cache lines across [addr, addr+size).
func flushICache(addr, size uintptr)
