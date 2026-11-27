//go:build linux && arm64 && !moeluanojit

#include "textflag.h"

// func flushICache(addr, size uintptr)
// Performs architectural ARMv8 instruction-cache maintenance for [addr, addr+size):
//   1. Reads CTR_EL0 for minimum D-cache and I-cache line strides (and IDC/DIC hints).
//   2. Cleans D-cache lines via DC CVAU unless CTR_EL0.IDC == 1, followed by DSB ISH.
//   3. Invalidates I-cache lines via IC IVAU unless CTR_EL0.DIC == 1, followed by DSB ISH.
//   4. Executes ISB SY to flush the instruction fetch pipeline.
TEXT ·flushICache(SB), NOSPLIT, $0-16
	MOVD	addr+0(FP), R0
	MOVD	size+8(FP), R1
	CBZ	R1, done
	ADD	R1, R0, R1                  // R1 = end = addr + size
	MRS	CTR_EL0, R2

	// Skip D-cache clean if CTR_EL0.IDC (bit 28) is set.
	TBNZ	$28, R2, icache_stage
	UBFX	$16, R2, $4, R3             // DminLine = CTR_EL0[19:16]
	MOVD	$4, R4
	LSL	R3, R4, R4                  // dline = 4 << DminLine
	SUB	$1, R4, R5
	BIC	R5, R0, R6                  // cur = addr &^ (dline - 1)
dcache_loop:
	CMP	R1, R6
	BHS	dcache_done
	WORD	$0xd50b7b26                 // DC CVAU, X6
	ADD	R4, R6, R6
	B	dcache_loop
dcache_done:
	DSB	$11                         // DSB ISH

icache_stage:
	// Skip I-cache invalidation if CTR_EL0.DIC (bit 29) is set.
	TBNZ	$29, R2, isb_only
	AND	$0xf, R2, R3                // IminLine = CTR_EL0[3:0]
	MOVD	$4, R4
	LSL	R3, R4, R4                  // iline = 4 << IminLine
	SUB	$1, R4, R5
	BIC	R5, R0, R6                  // cur = addr &^ (iline - 1)
icache_loop:
	CMP	R1, R6
	BHS	icache_done
	WORD	$0xd50b7526                 // IC IVAU, X6
	ADD	R4, R6, R6
	B	icache_loop
icache_done:
	DSB	$11                         // DSB ISH

isb_only:
	ISB	$15                         // ISB SY
done:
	RET
