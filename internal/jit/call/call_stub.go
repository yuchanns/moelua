//go:build (!arm64 && !amd64) || moeluanojit

package call

// Call is a compile-time stub for platforms without native JIT support or -tags moeluanojit.
func Call(_, _ uintptr) uint64 {
	panic("jit/call: native JIT unsupported in this build")
}
