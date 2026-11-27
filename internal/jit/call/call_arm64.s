//go:build arm64 && !moeluanojit

#include "textflag.h"

// func Call(entry uintptr, ctx uintptr) uint64
// Go's arm64 assembler automatically saves/restores R29 (FP) and R30 (LR) around CALL (R9).
// Compiled JIT code receives ctx in R0, returns uint64 exit code in R0, and preserves
// R18 (platform), R28 (Go g), R29 (FP), R30 (LR), and RSP.
TEXT ·Call(SB), NOSPLIT, $0-24
	MOVD	entry+0(FP), R9
	MOVD	ctx+8(FP), R0
	CALL	(R9)
	MOVD	R0, ret+16(FP)
	RET
