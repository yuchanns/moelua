//go:build (!arm64 && !amd64) || moeluanojit

package call

import "unsafe"

// Call is a compile-time stub for platforms without native JIT support or -tags moeluanojit.
func Call(_ uintptr, _ unsafe.Pointer) uint64 {
	panic("jit/call: native JIT unsupported in this build")
}
