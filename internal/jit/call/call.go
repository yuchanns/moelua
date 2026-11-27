//go:build (arm64 || amd64) && !moeluanojit

// Package call provides minimal NOSPLIT Plan 9 trampolines that invoke sealed native
// JIT machine-code entry points while preserving Go's runtime ABI registers.
//
// Uniform internal Go<->JIT calling convention (owned by MoeLua across all OSes):
//   - ARM64: entry receives ctx in X0, returns uint64 in X0; must preserve X18, X28 (g),
//     X29 (FP), X30 (LR), and SP.
//   - AMD64: entry receives ctx in RDI, returns uint64 in RAX; must preserve R14 (g),
//     RBP, RSP, and XMM15 (zero).
package call

// Call invokes JIT-compiled machine code at entry, passing ctx as its sole pointer-sized
// argument and returning the uint64 exit code produced by the compiled code.
//
//go:noescape
func Call(entry uintptr, ctx uintptr) uint64
