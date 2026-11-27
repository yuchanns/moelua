//go:build arm64 && !purego && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

// runAsmExec executes the Plan 9 ARM64 direct-threaded bytecode interpreter loop.
//
//go:noescape
func runAsmExec(ctx *asmExecCtx) uint64
