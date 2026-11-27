//go:build amd64 && !moeluanojit

#include "textflag.h"

// func Call(entry uintptr, ctx uintptr) uint64
// Go's amd64 assembler automatically saves/restores BP around CALL AX.
// Compiled JIT code receives ctx in DI, returns uint64 exit code in AX, and preserves
// R14 (Go g), BP, SP, and X15 (ABIInternal zero register, defensively re-zeroed on return).
TEXT ·Call(SB), NOSPLIT, $0-24
	MOVQ	entry+0(FP), AX
	MOVQ	ctx+8(FP), DI
	CALL	AX
	XORPS	X15, X15
	MOVQ	AX, ret+16(FP)
	RET
