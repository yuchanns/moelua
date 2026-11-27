//go:build darwin && arm64 && !moeluanojit

#include "textflag.h"

// func flushICache(addr, size uintptr)
// On darwin/arm64 (Apple Silicon), XNU disables EL0 MRS CTR_EL0 (SCTLR_EL1.UCT == 0)
// while permitting EL0 DC CVAU, IC IVAU, DSB ISH, and ISB SY. Stepping by 64 bytes
// covers every Apple Silicon cache-line size (64B and 128B) cleanly with zero CGo.
TEXT ·flushICache(SB), NOSPLIT, $0-16
	MOVD	addr+0(FP), R0
	MOVD	size+8(FP), R1
	CBZ	R1, done
	ADD	R1, R0, R1                  // end = addr + size
	BIC	$63, R0, R6                 // cur = addr &^ 63
dcache_loop:
	CMP	R1, R6
	BHS	dcache_done
	WORD	$0xd50b7b26                 // DC CVAU, X6
	ADD	$64, R6, R6
	B	dcache_loop
dcache_done:
	DSB	$11                         // DSB ISH
	BIC	$63, R0, R6
icache_loop:
	CMP	R1, R6
	BHS	icache_done
	WORD	$0xd50b7526                 // IC IVAU, X6
	ADD	$64, R6, R6
	B	icache_loop
icache_done:
	DSB	$11                         // DSB ISH
	ISB	$15                         // ISB SY
done:
	RET
