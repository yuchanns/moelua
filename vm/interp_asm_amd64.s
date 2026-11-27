//go:build amd64 && !purego && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

#include "textflag.h"
#include "asm_offsets.h"

// Pinned registers across the direct-threaded AMD64 interpreter loop:
//   AX, BX, CX, DX, SI, DI: scratch per handler (AX/DX for DIVQ, CX/CL for shifts)
//   R8:  inline 8-byte-stride jump table base (&jtable[0])
//   R9:  s.arena.pagePtrs base pointer
//   R10: bp.consts base pointer
//   R11: current 32-bit bytecode instruction word (zero-extended)
//   R12: current frame R[0] (&stack[base])
//   R13: current instruction pointer (points to NEXT instruction after fetch)
//   R14: UNTOUCHED (Go g)
//   R15: *asmExecCtx
//   BP, SP, X15: UNTOUCHED (Go frame pointer, stack pointer, ABIInternal zero XMM)

#define JENT(lbl) \
	JMP	lbl; \
	PCALIGN	$8

// DISPATCH_NEXT fetches the next 32-bit instruction, advances R13, and jumps via a
// per-opcode indirect branch site so the CPU BTB predicts each opcode transition
// independently without Go switch BTB aliasing.
#define DISPATCH_NEXT \
	MOVL	(R13), R11; \
	ADDQ	$4, R13; \
	MOVL	R11, AX; \
	ANDL	$0x7f, AX; \
	LEAQ	(R8)(AX*8), AX; \
	JMP	AX

// RESOLVE_SMALL resolves a non-null, non-large Arena ObjRef (32-bit zero-extended in REF) into DST.
// REF and DST may alias; TMP must be distinct from REF and DST.
#define RESOLVE_SMALL(REF, DST, TMP) \
	MOVL	REF, TMP; \
	SHRL	$ARENA_WORDBITS, TMP; \
	MOVQ	(R9)(TMP*8), TMP; \
	ANDL	$ARENA_WORDMASK, REF; \
	LEAQ	(TMP)(REF*8), DST

// RESOLVE_FAST resolves any live small-slab or large-span Arena ObjRef in REF into DST.
// REF is preserved or clobbered (32-bit); TMP must be distinct from REF and DST.
#define RESOLVE_FAST(REF, DST, TMP, L_LARGE, L_DONE) \
	TESTL	$0x80000000, REF; \
	JNE	L_LARGE; \
	MOVL	REF, TMP; \
	SHRL	$ARENA_WORDBITS, TMP; \
	MOVQ	(R9)(TMP*8), TMP; \
	ANDL	$ARENA_WORDMASK, REF; \
	LEAQ	(TMP)(REF*8), DST; \
	JMP	L_DONE; \
L_LARGE: \
	ANDL	$0x7fffffff, REF; \
	LEAQ	(REF)(REF*2), TMP; \
	MOVQ	CTX_LARGESPANS(R15), DST; \
	MOVQ	(DST)(TMP*8), DST; \
L_DONE:

// COND_DISPATCH finishes a comparison opcode whose boolean result (0 or 1) is in AL.
// If cond != w.K(), skips the following OP_JMP; otherwise executes its signed jump.
#define COND_DISPATCH(L_SKIP, L_BACK) \
	MOVL	R11, CX; \
	SHRL	$15, CX; \
	ANDL	$1, CX; \
	CMPB	AL, CL; \
	JNE	L_SKIP; \
	MOVL	(R13), AX; \
	SHRL	$7, AX; \
	SUBQ	$OFFSET_SJ, AX; \
	LEAQ	4(R13)(AX*4), R13; \
	TESTQ	AX, AX; \
	JS	L_BACK; \
	DISPATCH_NEXT; \
L_BACK: \
	DECQ	CTX_POLL(R15); \
	JLE	exit_poll; \
	DISPATCH_NEXT; \
L_SKIP: \
	ADDQ	$4, R13; \
	DISPATCH_NEXT

// func runAsmExec(ctx *asmExecCtx) uint64
TEXT ·runAsmExec(SB), NOSPLIT, $0-16
	PCALIGN	$16
	JMP	entry
	PCALIGN	$16

// 128-entry fixed-stride (8B) inline jump table starting at exact offset +16 from ·runAsmExec(SB).
// Entries 85..127 pad to 128 so ANDL $0x7f guarantees bounds safety without a branch.
jtable:
	JENT(op_MOVE)        // 0
	JENT(op_LOADI)       // 1
	JENT(op_LOADF)       // 2
	JENT(op_LOADK)       // 3
	JENT(exit_cold)      // 4  OP_LOADKX
	JENT(op_LOADFALSE)   // 5
	JENT(op_LFALSESKIP)  // 6
	JENT(op_LOADTRUE)    // 7
	JENT(op_LOADNIL)     // 8
	JENT(op_GETUPVAL)    // 9
	JENT(op_SETUPVAL)    // 10
	JENT(op_GETTABUP)    // 11
	JENT(op_GETTABLE)    // 12
	JENT(op_GETI)        // 13
	JENT(op_GETFIELD)    // 14
	JENT(op_SETTABUP)    // 15
	JENT(op_SETTABLE)    // 16
	JENT(op_SETI)        // 17
	JENT(op_SETFIELD)    // 18
	JENT(exit_cold)      // 19 OP_NEWTABLE
	JENT(op_SELF)        // 20
	JENT(op_ADDI)        // 21
	JENT(op_ADDK)        // 22
	JENT(op_SUBK)        // 23
	JENT(op_MULK)        // 24
	JENT(op_MODK)        // 25
	JENT(exit_cold)      // 26 OP_POWK
	JENT(op_DIVK)        // 27
	JENT(op_IDIVK)       // 28
	JENT(op_BANDK)       // 29
	JENT(op_BORK)        // 30
	JENT(op_BXORK)       // 31
	JENT(op_SHLI)        // 32
	JENT(op_SHRI)        // 33
	JENT(op_ADD)         // 34
	JENT(op_SUB)         // 35
	JENT(op_MUL)         // 36
	JENT(op_MOD)         // 37
	JENT(exit_cold)      // 38 OP_POW
	JENT(op_DIV)         // 39
	JENT(op_IDIV)        // 40
	JENT(op_BAND)        // 41
	JENT(op_BOR)         // 42
	JENT(op_BXOR)        // 43
	JENT(op_SHL)         // 44
	JENT(op_SHR)         // 45
	JENT(exit_cold)      // 46 OP_MMBIN
	JENT(exit_cold)      // 47 OP_MMBINI
	JENT(exit_cold)      // 48 OP_MMBINK
	JENT(op_UNM)         // 49
	JENT(op_BNOT)        // 50
	JENT(op_NOT)         // 51
	JENT(op_LEN)         // 52
	JENT(exit_cold)      // 53 OP_CONCAT
	JENT(exit_cold)      // 54 OP_CLOSE
	JENT(exit_cold)      // 55 OP_TBC
	JENT(op_JMP)         // 56
	JENT(op_EQ)          // 57
	JENT(op_LT)          // 58
	JENT(op_LE)          // 59
	JENT(op_EQK)         // 60
	JENT(op_EQI)         // 61
	JENT(op_LTI)         // 62
	JENT(op_LEI)         // 63
	JENT(op_GTI)         // 64
	JENT(op_GEI)         // 65
	JENT(op_TEST)        // 66
	JENT(op_TESTSET)     // 67
	JENT(op_CALL)        // 68
	JENT(op_TAILCALL)    // 69
	JENT(exit_cold)      // 70 OP_RETURN
	JENT(op_RETURN0)     // 71
	JENT(op_RETURN1)     // 72
	JENT(op_FORLOOP)     // 73
	JENT(op_FORPREP)     // 74
	JENT(exit_cold)      // 75 OP_TFORPREP
	JENT(exit_cold)      // 76 OP_TFORCALL
	JENT(op_TFORLOOP)    // 77
	JENT(exit_cold)      // 78 OP_SETLIST
	JENT(exit_cold)      // 79 OP_CLOSURE
	JENT(exit_cold)      // 80 OP_VARARG
	JENT(exit_cold)      // 81 OP_GETVARG
	JENT(exit_cold)      // 82 OP_ERRNNIL
	JENT(exit_cold)      // 83 OP_VARARGPREP
	JENT(exit_cold)      // 84 OP_EXTRAARG
	// Pad 85..127 (43 entries) to 128
	JENT(exit_cold)      // 85
	JENT(exit_cold)      // 86
	JENT(exit_cold)      // 87
	JENT(exit_cold)      // 88
	JENT(exit_cold)      // 89
	JENT(exit_cold)      // 90
	JENT(exit_cold)      // 91
	JENT(exit_cold)      // 92
	JENT(exit_cold)      // 93
	JENT(exit_cold)      // 94
	JENT(exit_cold)      // 95
	JENT(exit_cold)      // 96
	JENT(exit_cold)      // 97
	JENT(exit_cold)      // 98
	JENT(exit_cold)      // 99
	JENT(exit_cold)      // 100
	JENT(exit_cold)      // 101
	JENT(exit_cold)      // 102
	JENT(exit_cold)      // 103
	JENT(exit_cold)      // 104
	JENT(exit_cold)      // 105
	JENT(exit_cold)      // 106
	JENT(exit_cold)      // 107
	JENT(exit_cold)      // 108
	JENT(exit_cold)      // 109
	JENT(exit_cold)      // 110
	JENT(exit_cold)      // 111
	JENT(exit_cold)      // 112
	JENT(exit_cold)      // 113
	JENT(exit_cold)      // 114
	JENT(exit_cold)      // 115
	JENT(exit_cold)      // 116
	JENT(exit_cold)      // 117
	JENT(exit_cold)      // 118
	JENT(exit_cold)      // 119
	JENT(exit_cold)      // 120
	JENT(exit_cold)      // 121
	JENT(exit_cold)      // 122
	JENT(exit_cold)      // 123
	JENT(exit_cold)      // 124
	JENT(exit_cold)      // 125
	JENT(exit_cold)      // 126
	JENT(exit_cold)      // 127

entry:
	MOVQ	ctx+0(FP), R15
	MOVQ	CTX_REGS(R15), R12
	MOVQ	CTX_CONSTS(R15), R10
	MOVQ	CTX_PAGEPTRS(R15), R9
	MOVQ	CTX_INSNS(R15), R13
	MOVQ	CTX_PC(R15), AX
	LEAQ	(R13)(AX*4), R13
	LEAQ	·runAsmExec+16(SB), R8
	DISPATCH_NEXT

// ============================================================================
// Loads & Moves
// ============================================================================

op_MOVE:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX                      // A
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX                      // B
	SHLQ	$4, AX
	SHLQ	$4, BX
	MOVOU	(R12)(BX*1), X0
	MOVOU	X0, (R12)(AX*1)
	DISPATCH_NEXT

op_LOADI:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVL	R11, BX
	SHRL	$15, BX
	SUBQ	$OFFSET_SBX, BX
	MOVQ	BX, VAL_BITS(R12)(AX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(AX*1)
	DISPATCH_NEXT

op_LOADF:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVL	R11, BX
	SHRL	$15, BX
	SUBQ	$OFFSET_SBX, BX
	CVTSQ2SD BX, X0
	MOVSD	X0, VAL_BITS(R12)(AX*1)
	MOVQ	$TAG_FLOAT, VAL_TAG(R12)(AX*1)
	DISPATCH_NEXT

op_LOADK:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVL	R11, BX
	SHRL	$15, BX
	SHLQ	$4, BX
	MOVOU	(R10)(BX*1), X0
	MOVOU	X0, (R12)(AX*1)
	DISPATCH_NEXT

op_LOADFALSE:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVQ	$0, VAL_BITS(R12)(AX*1)
	MOVQ	$TAG_FALSE, VAL_TAG(R12)(AX*1)
	DISPATCH_NEXT

op_LFALSESKIP:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVQ	$0, VAL_BITS(R12)(AX*1)
	MOVQ	$TAG_FALSE, VAL_TAG(R12)(AX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_LOADTRUE:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVQ	$0, VAL_BITS(R12)(AX*1)
	MOVQ	$TAG_TRUE, VAL_TAG(R12)(AX*1)
	DISPATCH_NEXT

op_LOADNIL:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	LEAQ	(R12)(AX*1), DI
	MOVL	R11, CX
	SHRL	$16, CX
	MOVBLZX	CL, CX                      // B (loop B+1 times)
loadnil_loop:
	MOVQ	$0, VAL_BITS(DI)
	MOVQ	$TAG_NIL, VAL_TAG(DI)
	ADDQ	$16, DI
	SUBL	$1, CX
	JGE	loadnil_loop
	DISPATCH_NEXT

// ============================================================================
// Upvalues
// ============================================================================

op_GETUPVAL:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	MOVQ	CTX_CLUPVALS(R15), CX
	MOVL	(CX)(BX*4), SI              // uvRef
	RESOLVE_SMALL(SI, SI, DX)           // SI = *Upvalue
	MOVL	UV_OWNERREF(SI), CX
	TESTL	CX, CX
	JE	getupval_closed
	CMPL	CX, CTX_THSELF(R15)
	JNE	exit_cold
	MOVLQSX	UV_STACKIDX(SI), BX
	SHLQ	$4, BX
	MOVQ	CTX_STACK(R15), DX
	MOVOU	(DX)(BX*1), X0
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVOU	X0, (R12)(AX*1)
	DISPATCH_NEXT
getupval_closed:
	MOVOU	UV_CLOSED(SI), X0
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVOU	X0, (R12)(AX*1)
	DISPATCH_NEXT

op_SETUPVAL:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	MOVQ	CTX_CLUPVALS(R15), CX
	MOVL	(CX)(BX*4), SI              // uvRef
	RESOLVE_SMALL(SI, SI, DX)           // SI = *Upvalue
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVQ	VAL_BITS(R12)(AX*1), BX
	MOVQ	VAL_TAG(R12)(AX*1), CX
	MOVL	UV_OWNERREF(SI), DX
	TESTL	DX, DX
	JE	setupval_closed
	CMPL	DX, CTX_THSELF(R15)
	JNE	exit_cold
	MOVLQSX	UV_STACKIDX(SI), DI
	SHLQ	$4, DI
	MOVQ	CTX_STACK(R15), DX
	MOVQ	BX, VAL_BITS(DX)(DI*1)
	MOVQ	CX, VAL_TAG(DX)(DI*1)
	DISPATCH_NEXT
setupval_closed:
	LEAQ	-TAG_FIRST_GC(CX), DX
	CMPQ	DX, $(TAG_LAST_GC-TAG_FIRST_GC)
	JA	setupval_closed_store
	TESTB	$(1<<MARK_BLACK_BIT), UV_MARK(SI)
	JNE	exit_cold
setupval_closed_store:
	MOVQ	BX, (UV_CLOSED+VAL_BITS)(SI)
	MOVQ	CX, (UV_CLOSED+VAL_TAG)(SI)
	DISPATCH_NEXT

// ============================================================================
// Table Upvalue / Field / Self / Array Ops
// ============================================================================

op_GETTABUP:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	MOVQ	CTX_CLUPVALS(R15), CX
	MOVL	(CX)(BX*4), SI
	RESOLVE_SMALL(SI, SI, DX)           // SI = *Upvalue
	MOVL	UV_OWNERREF(SI), CX
	TESTL	CX, CX
	JE	gettabup_uv_closed
	CMPL	CX, CTX_THSELF(R15)
	JNE	exit_cold
	MOVLQSX	UV_STACKIDX(SI), BX
	SHLQ	$4, BX
	MOVQ	CTX_STACK(R15), DX
	MOVQ	VAL_BITS(DX)(BX*1), SI
	MOVQ	VAL_TAG(DX)(BX*1), CX
	JMP	gettabup_have_tv
gettabup_uv_closed:
	MOVQ	(UV_CLOSED+VAL_BITS)(SI), DX
	MOVQ	(UV_CLOSED+VAL_TAG)(SI), CX
	MOVQ	DX, SI
gettabup_have_tv:
	CMPQ	CX, $TAG_TABLE
	JNE	exit_cold
	RESOLVE_SMALL(SI, SI, DX)           // SI = *Table
	// Load *ICEntry into DI
	LEAQ	-4(R13), AX
	SUBQ	CTX_INSNS(R15), AX
	MOVQ	CTX_ICSLOTS(R15), BX
	MOVLQSX	(BX)(AX*1), CX              // icSlot
	LEAQ	(CX)(CX*2), CX              // icSlot * 3
	SHLQ	$4, CX                      // icSlot * 48
	MOVQ	CTX_IC(R15), DI
	ADDQ	CX, DI                      // DI = *ICEntry
	MOVBLZX	IC_KIND(DI), BX
	CMPB	BL, $IC_ENVSLOT
	JNE	gettabup_check_own
	MOVL	IC_HOLDERREF(DI), CX
	CMPL	CX, TBL_SELF(SI)
	JNE	exit_cold
	MOVL	TBL_HASHREF(SI), CX
	TESTL	CX, CX
	JE	exit_cold
	RESOLVE_SMALL(CX, CX, DX)           // CX = *tableHash
	MOVQ	IC_EPOCH(DI), AX
	CMPQ	AX, THASH_DICTGEN(CX)
	JNE	exit_cold
	MOVL	THASH_NODESREF(CX), BX
	TESTL	BX, BX
	JE	exit_cold
	RESOLVE_FAST(BX, BX, DX, gettabup_nodes_large, gettabup_nodes_ready)
	MOVLQSX	IC_SLOT(DI), CX
	LEAQ	(CX)(CX*4), CX              // slot * 5
	LEAQ	(BX)(CX*8), BX              // &nodes[slot]
	MOVQ	(HNODE_VAL+VAL_BITS)(BX), AX
	MOVQ	(HNODE_VAL+VAL_TAG)(BX), DX
	CMPQ	DX, $TAG_NIL
	JNE	gettabup_store
	CMPB	IC_NOMETA(DI), $0
	JE	exit_cold
gettabup_store:
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVQ	AX, VAL_BITS(R12)(CX*1)
	MOVQ	DX, VAL_TAG(R12)(CX*1)
	DISPATCH_NEXT
gettabup_check_own:
	CMPB	BL, $IC_OWNSLOT
	JNE	exit_cold
	MOVL	IC_SHAPEREF(DI), CX
	CMPL	CX, TBL_SHAPEREF(SI)
	JNE	exit_cold
	MOVLQSX	IC_SLOT(DI), CX
	SHLQ	$4, CX
	MOVL	TBL_SLOTSREF(SI), BX
	TESTL	BX, BX
	JNE	gettabup_own_ext
	LEAQ	TBL_INLINESLOTS(SI), BX
	JMP	gettabup_own_load
gettabup_own_ext:
	RESOLVE_SMALL(BX, BX, DX)
gettabup_own_load:
	MOVQ	VAL_BITS(BX)(CX*1), AX
	MOVQ	VAL_TAG(BX)(CX*1), DX
	CMPQ	DX, $TAG_NIL
	JNE	gettabup_store
	CMPB	IC_NOMETA(DI), $0
	JE	exit_cold
	JMP	gettabup_store

op_SETTABUP:
	MOVL	R11, BX
	SHRL	$7, BX
	MOVBLZX	BL, BX                      // A (upval idx)
	MOVQ	CTX_CLUPVALS(R15), CX
	MOVL	(CX)(BX*4), SI
	RESOLVE_SMALL(SI, SI, DX)           // SI = *Upvalue
	MOVL	UV_OWNERREF(SI), CX
	TESTL	CX, CX
	JE	settabup_uv_closed
	CMPL	CX, CTX_THSELF(R15)
	JNE	exit_cold
	MOVLQSX	UV_STACKIDX(SI), BX
	SHLQ	$4, BX
	MOVQ	CTX_STACK(R15), DX
	MOVQ	VAL_BITS(DX)(BX*1), SI
	MOVQ	VAL_TAG(DX)(BX*1), CX
	JMP	settabup_have_tv
settabup_uv_closed:
	MOVQ	(UV_CLOSED+VAL_BITS)(SI), DX
	MOVQ	(UV_CLOSED+VAL_TAG)(SI), CX
	MOVQ	DX, SI
settabup_have_tv:
	CMPQ	CX, $TAG_TABLE
	JNE	exit_cold
	RESOLVE_SMALL(SI, SI, DX)           // SI = *Table
	TESTB	$(1<<TBL_FLAG_ISPROTO_BIT), TBL_FLAGS(SI)
	JNE	exit_cold
	// Load val = RK(C) into X0 (and tag into CX)
	MOVL	R11, BX
	SHRL	$24, BX
	SHLQ	$4, BX
	TESTL	$(1<<15), R11
	JNE	settabup_val_k
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), CX
	JMP	settabup_have_val
settabup_val_k:
	MOVQ	VAL_BITS(R10)(BX*1), AX
	MOVQ	VAL_TAG(R10)(BX*1), CX
settabup_have_val:
	CMPQ	CX, $TAG_NIL
	JE	exit_cold
	// Load *ICEntry into DI
	LEAQ	-4(R13), BX
	SUBQ	CTX_INSNS(R15), BX
	MOVQ	CTX_ICSLOTS(R15), DX
	MOVLQSX	(DX)(BX*1), BX
	LEAQ	(BX)(BX*2), BX
	SHLQ	$4, BX
	MOVQ	CTX_IC(R15), DI
	ADDQ	BX, DI                      // DI = *ICEntry
	CMPB	IC_KIND(DI), $IC_ENVSLOT
	JNE	exit_cold
	MOVL	IC_HOLDERREF(DI), BX
	CMPL	BX, TBL_SELF(SI)
	JNE	exit_cold
	MOVL	TBL_HASHREF(SI), BX
	TESTL	BX, BX
	JE	exit_cold
	RESOLVE_SMALL(BX, BX, DX)           // BX = *tableHash
	MOVQ	IC_EPOCH(DI), DX
	CMPQ	DX, THASH_DICTGEN(BX)
	JNE	exit_cold
	MOVL	THASH_NODESREF(BX), BX
	TESTL	BX, BX
	JE	exit_cold
	RESOLVE_FAST(BX, BX, DX, settabup_nodes_large, settabup_nodes_ready)
	MOVLQSX	IC_SLOT(DI), DX
	LEAQ	(DX)(DX*4), DX
	LEAQ	(BX)(DX*8), BX              // &nodes[slot]
	CMPQ	(HNODE_VAL+VAL_TAG)(BX), $TAG_NIL
	JE	exit_cold
	// GC barrier check before mutation
	LEAQ	-TAG_FIRST_GC(CX), DX
	CMPQ	DX, $(TAG_LAST_GC-TAG_FIRST_GC)
	JA	settabup_do_store
	TESTB	$(1<<MARK_BLACK_BIT), TBL_MARK(SI)
	JNE	exit_cold
settabup_do_store:
	MOVQ	AX, (HNODE_VAL+VAL_BITS)(BX)
	MOVQ	CX, (HNODE_VAL+VAL_TAG)(BX)
	MOVB	$0, TBL_TMFLAGS(SI)
	DISPATCH_NEXT

op_GETFIELD:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	CMPQ	VAL_TAG(R12)(BX*1), $TAG_TABLE
	JNE	exit_cold
	MOVL	VAL_BITS(R12)(BX*1), SI
	RESOLVE_SMALL(SI, SI, DX)           // SI = *Table
	// Load *ICEntry into DI
	LEAQ	-4(R13), AX
	SUBQ	CTX_INSNS(R15), AX
	MOVQ	CTX_ICSLOTS(R15), BX
	MOVLQSX	(BX)(AX*1), CX
	LEAQ	(CX)(CX*2), CX
	SHLQ	$4, CX
	MOVQ	CTX_IC(R15), DI
	ADDQ	CX, DI                      // DI = *ICEntry
	MOVL	TBL_SHAPEREF(SI), AX
	CMPL	AX, IC_SHAPEREF(DI)
	JNE	getfield_poly
	MOVBLZX	IC_KIND(DI), BX
	CMPB	BL, $IC_OWNSLOT
	JNE	getfield_proto
	MOVLQSX	IC_SLOT(DI), CX
	SHLQ	$4, CX
	MOVL	TBL_SLOTSREF(SI), BX
	TESTL	BX, BX
	JNE	getfield_own_ext
	LEAQ	TBL_INLINESLOTS(SI), BX
	JMP	getfield_own_load
getfield_own_ext:
	RESOLVE_SMALL(BX, BX, DX)
getfield_own_load:
	MOVQ	VAL_BITS(BX)(CX*1), AX
	MOVQ	VAL_TAG(BX)(CX*1), DX
	CMPQ	DX, $TAG_NIL
	JNE	getfield_store
	CMPB	IC_NOMETA(DI), $0
	JNE	getfield_store
	CMPB	IC_PROTOABSENT(DI), $0
	JE	exit_cold
	MOVQ	IC_EPOCH(DI), CX
	CMPQ	CX, CTX_PROTOEPOCH(R15)
	JNE	exit_cold
getfield_store:
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVQ	AX, VAL_BITS(R12)(CX*1)
	MOVQ	DX, VAL_TAG(R12)(CX*1)
	DISPATCH_NEXT
getfield_proto:
	CMPB	BL, $IC_PROTOVAL
	JNE	getfield_poly
	CMPB	IC_NOMETA(DI), $0
	JNE	getfield_proto_hit
	MOVQ	IC_EPOCH(DI), CX
	CMPQ	CX, CTX_PROTOEPOCH(R15)
	JNE	getfield_poly
getfield_proto_hit:
	MOVOU	IC_VAL(DI), X0
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVOU	X0, (R12)(CX*1)
	DISPATCH_NEXT
getfield_poly:
	MOVL	IC_POLYREF(DI), BX
	TESTL	BX, BX
	JE	exit_cold
	MOVBLZX	IC_POLYCOUNT(DI), CX
	TESTL	CX, CX
	JE	exit_cold
	RESOLVE_SMALL(BX, DI, DX)           // DI = *polyEntry
getfield_poly_loop:
	CMPL	AX, POLY_SHAPEREF(DI)
	JE	getfield_poly_match
	ADDQ	$POLY_SIZE, DI
	DECL	CX
	JNE	getfield_poly_loop
	JMP	exit_cold
getfield_poly_match:
	MOVBLZX	POLY_KIND(DI), BX
	CMPB	BL, $IC_OWNSLOT
	JNE	getfield_poly_proto
	MOVLQSX	POLY_SLOT(DI), CX
	SHLQ	$4, CX
	MOVL	TBL_SLOTSREF(SI), BX
	TESTL	BX, BX
	JNE	getfield_poly_ext
	LEAQ	TBL_INLINESLOTS(SI), BX
	JMP	getfield_poly_load
getfield_poly_ext:
	RESOLVE_SMALL(BX, BX, DX)
getfield_poly_load:
	MOVQ	VAL_BITS(BX)(CX*1), AX
	MOVQ	VAL_TAG(BX)(CX*1), DX
	CMPQ	DX, $TAG_NIL
	JNE	getfield_store
	CMPB	POLY_NOMETA(DI), $0
	JNE	getfield_store
	CMPB	POLY_PROTOABSENT(DI), $0
	JE	exit_cold
	MOVQ	POLY_EPOCH(DI), CX
	CMPQ	CX, CTX_PROTOEPOCH(R15)
	JNE	exit_cold
	JMP	getfield_store
getfield_poly_proto:
	CMPB	BL, $IC_PROTOVAL
	JNE	exit_cold
	CMPB	POLY_NOMETA(DI), $0
	JNE	getfield_poly_proto_hit
	MOVQ	POLY_EPOCH(DI), CX
	CMPQ	CX, CTX_PROTOEPOCH(R15)
	JNE	exit_cold
getfield_poly_proto_hit:
	MOVOU	POLY_VAL(DI), X0
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVOU	X0, (R12)(CX*1)
	DISPATCH_NEXT

op_SELF:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX                      // A * 16
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX                      // B * 16
	MOVQ	VAL_BITS(R12)(BX*1), SI
	MOVQ	VAL_TAG(R12)(BX*1), CX
	MOVQ	SI, (VAL_SIZE+VAL_BITS)(R12)(AX*1)
	MOVQ	CX, (VAL_SIZE+VAL_TAG)(R12)(AX*1)
	CMPQ	CX, $TAG_TABLE
	JNE	exit_cold
	RESOLVE_SMALL(SI, SI, DX)           // SI = *Table
	// Load *ICEntry into DI
	LEAQ	-4(R13), BX
	SUBQ	CTX_INSNS(R15), BX
	MOVQ	CTX_ICSLOTS(R15), DX
	MOVLQSX	(DX)(BX*1), CX
	LEAQ	(CX)(CX*2), CX
	SHLQ	$4, CX
	MOVQ	CTX_IC(R15), DI
	ADDQ	CX, DI                      // DI = *ICEntry
	MOVL	TBL_SHAPEREF(SI), DX
	CMPL	DX, IC_SHAPEREF(DI)
	JNE	self_poly
	MOVBLZX	IC_KIND(DI), BX
	CMPB	BL, $IC_PROTOVAL
	JNE	self_own
	CMPB	IC_NOMETA(DI), $0
	JNE	self_proto_hit
	MOVQ	IC_EPOCH(DI), CX
	CMPQ	CX, CTX_PROTOEPOCH(R15)
	JNE	self_poly
self_proto_hit:
	MOVOU	IC_VAL(DI), X0
	MOVOU	X0, (R12)(AX*1)
	DISPATCH_NEXT
self_own:
	CMPB	BL, $IC_OWNSLOT
	JNE	self_poly
	MOVLQSX	IC_SLOT(DI), CX
	SHLQ	$4, CX
	MOVL	TBL_SLOTSREF(SI), BX
	TESTL	BX, BX
	JNE	self_own_ext
	LEAQ	TBL_INLINESLOTS(SI), BX
	JMP	self_own_load
self_own_ext:
	RESOLVE_SMALL(BX, BX, DX)
self_own_load:
	MOVQ	VAL_BITS(BX)(CX*1), SI
	MOVQ	VAL_TAG(BX)(CX*1), DX
	CMPQ	DX, $TAG_NIL
	JNE	self_store
	CMPB	IC_NOMETA(DI), $0
	JNE	self_store
	CMPB	IC_PROTOABSENT(DI), $0
	JE	exit_cold
	MOVQ	IC_EPOCH(DI), CX
	CMPQ	CX, CTX_PROTOEPOCH(R15)
	JNE	exit_cold
self_store:
	MOVQ	SI, VAL_BITS(R12)(AX*1)
	MOVQ	DX, VAL_TAG(R12)(AX*1)
	DISPATCH_NEXT
self_poly:
	MOVL	IC_POLYREF(DI), BX
	TESTL	BX, BX
	JE	exit_cold
	MOVBLZX	IC_POLYCOUNT(DI), CX
	TESTL	CX, CX
	JE	exit_cold
	MOVQ	AX, X1                      // save A * 16
	RESOLVE_SMALL(BX, DI, AX)           // DI = *polyEntry
self_poly_loop:
	CMPL	DX, POLY_SHAPEREF(DI)
	JE	self_poly_match
	ADDQ	$POLY_SIZE, DI
	DECL	CX
	JNE	self_poly_loop
	JMP	exit_cold
self_poly_match:
	MOVQ	X1, AX                      // restore A * 16
	MOVBLZX	POLY_KIND(DI), BX
	CMPB	BL, $IC_PROTOVAL
	JNE	self_poly_own
	CMPB	POLY_NOMETA(DI), $0
	JNE	self_poly_proto_hit
	MOVQ	POLY_EPOCH(DI), CX
	CMPQ	CX, CTX_PROTOEPOCH(R15)
	JNE	exit_cold
self_poly_proto_hit:
	MOVOU	POLY_VAL(DI), X0
	MOVOU	X0, (R12)(AX*1)
	DISPATCH_NEXT
self_poly_own:
	CMPB	BL, $IC_OWNSLOT
	JNE	exit_cold
	MOVLQSX	POLY_SLOT(DI), CX
	SHLQ	$4, CX
	MOVL	TBL_SLOTSREF(SI), BX
	TESTL	BX, BX
	JNE	self_poly_ext
	LEAQ	TBL_INLINESLOTS(SI), BX
	JMP	self_poly_load
self_poly_ext:
	RESOLVE_SMALL(BX, BX, DX)
self_poly_load:
	MOVQ	VAL_BITS(BX)(CX*1), SI
	MOVQ	VAL_TAG(BX)(CX*1), DX
	CMPQ	DX, $TAG_NIL
	JNE	self_store
	CMPB	POLY_NOMETA(DI), $0
	JNE	self_store
	CMPB	POLY_PROTOABSENT(DI), $0
	JE	exit_cold
	MOVQ	POLY_EPOCH(DI), CX
	CMPQ	CX, CTX_PROTOEPOCH(R15)
	JNE	exit_cold
	JMP	self_store

op_SETFIELD:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	CMPQ	VAL_TAG(R12)(AX*1), $TAG_TABLE
	JNE	exit_cold
	MOVL	VAL_BITS(R12)(AX*1), SI
	RESOLVE_SMALL(SI, SI, DX)           // SI = *Table
	TESTB	$(1<<TBL_FLAG_ISPROTO_BIT), TBL_FLAGS(SI)
	JNE	exit_cold
	// Load val = RK(C) into (AX=bits, CX=tag)
	MOVL	R11, BX
	SHRL	$24, BX
	SHLQ	$4, BX
	TESTL	$(1<<15), R11
	JNE	setfield_val_k
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), CX
	JMP	setfield_have_val
setfield_val_k:
	MOVQ	VAL_BITS(R10)(BX*1), AX
	MOVQ	VAL_TAG(R10)(BX*1), CX
setfield_have_val:
	// Load *ICEntry into DI
	LEAQ	-4(R13), BX
	SUBQ	CTX_INSNS(R15), BX
	MOVQ	CTX_ICSLOTS(R15), DX
	MOVLQSX	(DX)(BX*1), BX
	LEAQ	(BX)(BX*2), BX
	SHLQ	$4, BX
	MOVQ	CTX_IC(R15), DI
	ADDQ	BX, DI                      // DI = *ICEntry
	MOVL	TBL_SHAPEREF(SI), DX
	CMPL	DX, IC_SHAPEREF(DI)
	JNE	setfield_poly
	MOVBLZX	IC_KIND(DI), BX
	CMPB	BL, $IC_OWNSLOT
	JNE	setfield_addslot
	MOVLQSX	IC_SLOT(DI), DX
	SHLQ	$4, DX
	MOVL	TBL_SLOTSREF(SI), BX
	TESTL	BX, BX
	JNE	setfield_own_ext
	LEAQ	TBL_INLINESLOTS(SI)(DX*1), BX
	JMP	setfield_own_ptr
setfield_own_ext:
	// Save val.bits in X0 temporarily so we have a scratch reg for RESOLVE_SMALL
	MOVQ	AX, X0
	RESOLVE_SMALL(BX, BX, AX)
	ADDQ	DX, BX
	MOVQ	X0, AX
setfield_own_ptr:
	CMPQ	VAL_TAG(BX), $TAG_NIL
	JNE	setfield_own_barrier
	CMPB	IC_NOMETA(DI), $0
	JNE	setfield_own_barrier
	CMPB	IC_PROTOABSENT(DI), $0
	JE	exit_cold
	MOVQ	IC_EPOCH(DI), DX
	CMPQ	DX, CTX_PROTOEPOCH(R15)
	JNE	exit_cold
setfield_own_barrier:
	LEAQ	-TAG_FIRST_GC(CX), DX
	CMPQ	DX, $(TAG_LAST_GC-TAG_FIRST_GC)
	JA	setfield_own_store
	TESTB	$(1<<MARK_BLACK_BIT), TBL_MARK(SI)
	JNE	exit_cold
setfield_own_store:
	MOVQ	AX, VAL_BITS(BX)
	MOVQ	CX, VAL_TAG(BX)
	DISPATCH_NEXT
setfield_addslot:
	CMPB	BL, $IC_ADDSLOT
	JNE	exit_cold
	CMPQ	CX, $TAG_NIL
	JE	exit_cold
	CMPB	IC_NOMETA(DI), $0
	JNE	setfield_add_epoch_ok
	MOVQ	IC_EPOCH(DI), DX
	CMPQ	DX, CTX_PROTOEPOCH(R15)
	JNE	exit_cold
setfield_add_epoch_ok:
	MOVL	IC_TARGETSHAPE(DI), DX
	TESTL	DX, DX
	JE	exit_cold
	MOVLQSX	IC_SLOT(DI), BX
	MOVBLZX	TBL_SLOTSCAP(SI), DI        // DI = slotsCap (ICEntry no longer needed!)
	CMPQ	BX, DI
	JGE	exit_cold
	TESTB	$(1<<MARK_BLACK_BIT), TBL_MARK(SI)
	JNE	exit_cold
	// Save targetShape in X1, slot in DI, val.bits in X0
	MOVQ	DX, X1
	MOVQ	BX, DI
	SHLQ	$4, BX
	MOVL	TBL_SLOTSREF(SI), DX
	TESTL	DX, DX
	JNE	setfield_add_ext
	LEAQ	TBL_INLINESLOTS(SI)(BX*1), DX
	JMP	setfield_add_store
setfield_add_ext:
	MOVQ	AX, X0
	RESOLVE_SMALL(DX, DX, AX)
	ADDQ	BX, DX
	MOVQ	X0, AX
setfield_add_store:
	MOVQ	AX, VAL_BITS(DX)
	MOVQ	CX, VAL_TAG(DX)
	INCL	DI
	MOVB	DI, TBL_SLOTSLEN(SI)
	MOVQ	X1, DX
	MOVL	DX, TBL_SHAPEREF(SI)
	DISPATCH_NEXT
setfield_poly:
	MOVL	IC_POLYREF(DI), BX
	TESTL	BX, BX
	JE	exit_cold
	MOVBLZX	IC_POLYCOUNT(DI), DX
	TESTL	DX, DX
	JE	exit_cold
	MOVQ	AX, X0                      // save val.bits
	RESOLVE_SMALL(BX, DI, AX)           // DI = *polyEntry
	MOVL	TBL_SHAPEREF(SI), AX
setfield_poly_loop:
	CMPL	AX, POLY_SHAPEREF(DI)
	JE	setfield_poly_match
	ADDQ	$POLY_SIZE, DI
	DECL	DX
	JNE	setfield_poly_loop
	JMP	exit_cold
setfield_poly_match:
	MOVQ	X0, AX                      // restore val.bits
	MOVBLZX	POLY_KIND(DI), BX
	CMPB	BL, $IC_OWNSLOT
	JNE	setfield_poly_add
	MOVLQSX	POLY_SLOT(DI), DX
	SHLQ	$4, DX
	MOVL	TBL_SLOTSREF(SI), BX
	TESTL	BX, BX
	JNE	setfield_poly_own_ext
	LEAQ	TBL_INLINESLOTS(SI)(DX*1), BX
	JMP	setfield_poly_own_ptr
setfield_poly_own_ext:
	MOVQ	AX, X0
	RESOLVE_SMALL(BX, BX, AX)
	ADDQ	DX, BX
	MOVQ	X0, AX
setfield_poly_own_ptr:
	CMPQ	VAL_TAG(BX), $TAG_NIL
	JNE	setfield_own_barrier
	CMPB	POLY_NOMETA(DI), $0
	JNE	setfield_own_barrier
	CMPB	POLY_PROTOABSENT(DI), $0
	JE	exit_cold
	MOVQ	POLY_EPOCH(DI), DX
	CMPQ	DX, CTX_PROTOEPOCH(R15)
	JNE	exit_cold
	JMP	setfield_own_barrier
setfield_poly_add:
	CMPB	BL, $IC_ADDSLOT
	JNE	exit_cold
	CMPQ	CX, $TAG_NIL
	JE	exit_cold
	CMPB	POLY_NOMETA(DI), $0
	JNE	setfield_poly_add_epoch_ok
	MOVQ	POLY_EPOCH(DI), DX
	CMPQ	DX, CTX_PROTOEPOCH(R15)
	JNE	exit_cold
setfield_poly_add_epoch_ok:
	MOVL	POLY_TARGETSHAPE(DI), DX
	TESTL	DX, DX
	JE	exit_cold
	MOVLQSX	POLY_SLOT(DI), BX
	MOVBLZX	TBL_SLOTSCAP(SI), DI
	CMPQ	BX, DI
	JGE	exit_cold
	TESTB	$(1<<MARK_BLACK_BIT), TBL_MARK(SI)
	JNE	exit_cold
	MOVQ	DX, X1
	MOVQ	BX, DI
	SHLQ	$4, BX
	MOVL	TBL_SLOTSREF(SI), DX
	TESTL	DX, DX
	JNE	setfield_add_ext
	LEAQ	TBL_INLINESLOTS(SI)(BX*1), DX
	JMP	setfield_add_store

op_GETI:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	CMPQ	VAL_TAG(R12)(BX*1), $TAG_TABLE
	JNE	exit_cold
	MOVL	VAL_BITS(R12)(BX*1), SI
	RESOLVE_SMALL(SI, SI, DX)           // SI = *Table
	MOVL	R11, CX
	SHRL	$24, CX                     // C (0..255)
	SUBQ	$1, CX                      // idx = uint64(C - 1)
	MOVL	TBL_ARRLEN(SI), AX
	CMPQ	CX, AX
	JAE	exit_cold
	MOVL	TBL_ARRREF(SI), BX
	RESOLVE_FAST(BX, BX, DX, geti_large, geti_arr_ready)
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX                      // A * 16
	MOVBLZX	TBL_ARRKIND(SI), DX
	CMPB	DL, $ARR_INT
	JNE	geti_not_int
	MOVQ	(BX)(CX*8), DX
	MOVQ	DX, VAL_BITS(R12)(AX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(AX*1)
	DISPATCH_NEXT
geti_not_int:
	CMPB	DL, $ARR_FLOAT
	JNE	geti_not_float
	MOVQ	(BX)(CX*8), DX
	MOVQ	DX, VAL_BITS(R12)(AX*1)
	MOVQ	$TAG_FLOAT, VAL_TAG(R12)(AX*1)
	DISPATCH_NEXT
geti_not_float:
	CMPB	DL, $ARR_GENERIC
	JNE	geti_bool
	SHLQ	$4, CX
	MOVQ	VAL_BITS(BX)(CX*1), DI
	MOVQ	VAL_TAG(BX)(CX*1), DX
	CMPQ	DX, $TAG_NIL
	JNE	geti_gen_store
	CMPL	TBL_METAREF(SI), $0
	JNE	exit_cold
geti_gen_store:
	MOVQ	DI, VAL_BITS(R12)(AX*1)
	MOVQ	DX, VAL_TAG(R12)(AX*1)
	DISPATCH_NEXT
geti_bool:
	CMPB	DL, $ARR_BOOL
	JNE	exit_cold
	MOVBLZX	(BX)(CX*1), DX
	ADDQ	$TAG_FALSE, DX
	MOVQ	$0, VAL_BITS(R12)(AX*1)
	MOVQ	DX, VAL_TAG(R12)(AX*1)
	DISPATCH_NEXT

op_GETTABLE:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	CMPQ	VAL_TAG(R12)(BX*1), $TAG_TABLE
	JNE	exit_cold
	CMPQ	VAL_TAG(R12)(CX*1), $TAG_INT
	JNE	exit_cold
	MOVL	VAL_BITS(R12)(BX*1), SI
	MOVQ	VAL_BITS(R12)(CX*1), CX
	RESOLVE_SMALL(SI, SI, DX)           // SI = *Table
	SUBQ	$1, CX                      // idx = uint64(rc.Int() - 1)
	MOVL	TBL_ARRLEN(SI), AX
	CMPQ	CX, AX
	JAE	exit_cold
	MOVL	TBL_ARRREF(SI), BX
	RESOLVE_FAST(BX, BX, DX, gettab_large, gettab_arr_ready)
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVBLZX	TBL_ARRKIND(SI), DX
	CMPB	DL, $ARR_INT
	JNE	gettab_not_int
	MOVQ	(BX)(CX*8), DX
	MOVQ	DX, VAL_BITS(R12)(AX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(AX*1)
	DISPATCH_NEXT
gettab_not_int:
	CMPB	DL, $ARR_FLOAT
	JNE	gettab_not_float
	MOVQ	(BX)(CX*8), DX
	MOVQ	DX, VAL_BITS(R12)(AX*1)
	MOVQ	$TAG_FLOAT, VAL_TAG(R12)(AX*1)
	DISPATCH_NEXT
gettab_not_float:
	CMPB	DL, $ARR_BOOL
	JNE	gettab_generic
	MOVBLZX	(BX)(CX*1), DX
	ADDQ	$TAG_FALSE, DX
	MOVQ	$0, VAL_BITS(R12)(AX*1)
	MOVQ	DX, VAL_TAG(R12)(AX*1)
	DISPATCH_NEXT
gettab_generic:
	CMPB	DL, $ARR_GENERIC
	JNE	exit_cold
	SHLQ	$4, CX
	MOVQ	VAL_BITS(BX)(CX*1), DI
	MOVQ	VAL_TAG(BX)(CX*1), DX
	CMPQ	DX, $TAG_NIL
	JNE	gettab_gen_store
	CMPL	TBL_METAREF(SI), $0
	JNE	exit_cold
gettab_gen_store:
	MOVQ	DI, VAL_BITS(R12)(AX*1)
	MOVQ	DX, VAL_TAG(R12)(AX*1)
	DISPATCH_NEXT

op_SETI:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	CMPQ	VAL_TAG(R12)(AX*1), $TAG_TABLE
	JNE	exit_cold
	MOVL	VAL_BITS(R12)(AX*1), SI
	RESOLVE_SMALL(SI, SI, DX)           // SI = *Table
	TESTB	$(1<<TBL_FLAG_ISPROTO_BIT), TBL_FLAGS(SI)
	JNE	exit_cold
	MOVL	R11, CX
	SHRL	$16, CX
	MOVBLZX	CL, CX                      // B
	SUBQ	$1, CX                      // idx = uint64(B - 1)
	MOVL	TBL_ARRLEN(SI), AX
	CMPQ	CX, AX
	JAE	exit_cold
	// val = RK(C) -> (AX=bits, DI=tag)
	MOVL	R11, BX
	SHRL	$24, BX
	SHLQ	$4, BX
	TESTL	$(1<<15), R11
	JNE	seti_val_k
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), DI
	JMP	seti_have_val
seti_val_k:
	MOVQ	VAL_BITS(R10)(BX*1), AX
	MOVQ	VAL_TAG(R10)(BX*1), DI
seti_have_val:
	MOVL	TBL_ARRREF(SI), BX
	RESOLVE_FAST(BX, BX, DX, seti_large, seti_arr_ready)
	MOVBLZX	TBL_ARRKIND(SI), DX
	CMPB	DL, $ARR_INT
	JNE	seti_not_int
	TESTQ	DI, DI
	JNE	exit_cold
	MOVQ	AX, (BX)(CX*8)
	DISPATCH_NEXT
seti_not_int:
	CMPB	DL, $ARR_FLOAT
	JNE	seti_not_float
	CMPQ	DI, $TAG_FLOAT
	JNE	exit_cold
	MOVQ	AX, (BX)(CX*8)
	DISPATCH_NEXT
seti_not_float:
	CMPB	DL, $ARR_GENERIC
	JNE	seti_bool
	SHLQ	$4, CX
	CMPQ	VAL_TAG(BX)(CX*1), $TAG_NIL
	JNE	seti_gen_barrier
	CMPL	TBL_METAREF(SI), $0
	JNE	exit_cold
seti_gen_barrier:
	LEAQ	-TAG_FIRST_GC(DI), DX
	CMPQ	DX, $(TAG_LAST_GC-TAG_FIRST_GC)
	JA	seti_gen_store
	TESTB	$(1<<MARK_BLACK_BIT), TBL_MARK(SI)
	JNE	exit_cold
seti_gen_store:
	MOVQ	AX, VAL_BITS(BX)(CX*1)
	MOVQ	DI, VAL_TAG(BX)(CX*1)
	DISPATCH_NEXT
seti_bool:
	CMPB	DL, $ARR_BOOL
	JNE	exit_cold
	LEAQ	-TAG_FALSE(DI), DX
	CMPQ	DX, $1
	JA	exit_cold
	MOVB	DL, (BX)(CX*1)
	DISPATCH_NEXT

op_SETTABLE:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	CMPQ	VAL_TAG(R12)(AX*1), $TAG_TABLE
	JNE	exit_cold
	CMPQ	VAL_TAG(R12)(BX*1), $TAG_INT
	JNE	exit_cold
	MOVL	VAL_BITS(R12)(AX*1), SI
	MOVQ	VAL_BITS(R12)(BX*1), CX
	RESOLVE_SMALL(SI, SI, DX)           // SI = *Table
	TESTB	$(1<<TBL_FLAG_ISPROTO_BIT), TBL_FLAGS(SI)
	JNE	exit_cold
	// val = RK(C) -> (AX=bits, DI=tag)
	MOVL	R11, BX
	SHRL	$24, BX
	SHLQ	$4, BX
	TESTL	$(1<<15), R11
	JNE	settab_val_k
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), DI
	JMP	settab_have_val
settab_val_k:
	MOVQ	VAL_BITS(R10)(BX*1), AX
	MOVQ	VAL_TAG(R10)(BX*1), DI
settab_have_val:
	SUBQ	$1, CX                      // idx = uint64(rb.Int() - 1)
	MOVL	TBL_ARRLEN(SI), DX          // n = t.arrLen
	CMPQ	CX, DX
	JAE	settab_maybe_append
	MOVL	TBL_ARRREF(SI), BX
	RESOLVE_FAST(BX, BX, DX, settab_large, settab_arr_ready)
	MOVBLZX	TBL_ARRKIND(SI), DX
	CMPB	DL, $ARR_INT
	JNE	settab_not_int
	TESTQ	DI, DI
	JNE	exit_cold
	MOVQ	AX, (BX)(CX*8)
	DISPATCH_NEXT
settab_not_int:
	CMPB	DL, $ARR_FLOAT
	JNE	settab_not_float
	CMPQ	DI, $TAG_FLOAT
	JNE	exit_cold
	MOVQ	AX, (BX)(CX*8)
	DISPATCH_NEXT
settab_not_float:
	CMPB	DL, $ARR_BOOL
	JNE	settab_generic
	LEAQ	-TAG_FALSE(DI), DX
	CMPQ	DX, $1
	JA	exit_cold
	MOVB	DL, (BX)(CX*1)
	DISPATCH_NEXT
settab_generic:
	CMPB	DL, $ARR_GENERIC
	JNE	exit_cold
	SHLQ	$4, CX
	CMPQ	VAL_TAG(BX)(CX*1), $TAG_NIL
	JNE	settab_gen_barrier
	CMPL	TBL_METAREF(SI), $0
	JNE	exit_cold
settab_gen_barrier:
	LEAQ	-TAG_FIRST_GC(DI), DX
	CMPQ	DX, $(TAG_LAST_GC-TAG_FIRST_GC)
	JA	settab_gen_store
	TESTB	$(1<<MARK_BLACK_BIT), TBL_MARK(SI)
	JNE	exit_cold
settab_gen_store:
	MOVQ	AX, VAL_BITS(BX)(CX*1)
	MOVQ	DI, VAL_TAG(BX)(CX*1)
	DISPATCH_NEXT
settab_maybe_append:
	JNE	exit_cold                   // idx != n
	CMPL	DX, TBL_ARRCAP(SI)
	JAE	exit_cold                   // n >= arrCap
	CMPL	TBL_HASHREF(SI), $0
	JNE	exit_cold
	CMPL	TBL_METAREF(SI), $0
	JNE	exit_cold
	MOVL	TBL_ARRREF(SI), BX
	RESOLVE_FAST(BX, BX, DX, settab_app_large, settab_app_ready)
	MOVBLZX	TBL_ARRKIND(SI), DX
	CMPB	DL, $ARR_INT
	JNE	settab_app_not_int
	TESTQ	DI, DI
	JNE	exit_cold
	LEAL	1(CX), DX
	MOVL	DX, TBL_ARRLEN(SI)
	MOVQ	AX, (BX)(CX*8)
	DISPATCH_NEXT
settab_app_not_int:
	CMPB	DL, $ARR_FLOAT
	JNE	settab_app_not_float
	CMPQ	DI, $TAG_FLOAT
	JNE	exit_cold
	LEAL	1(CX), DX
	MOVL	DX, TBL_ARRLEN(SI)
	MOVQ	AX, (BX)(CX*8)
	DISPATCH_NEXT
settab_app_not_float:
	CMPB	DL, $ARR_BOOL
	JNE	settab_app_generic
	LEAQ	-TAG_FALSE(DI), DX
	CMPQ	DX, $1
	JA	exit_cold
	MOVB	DL, (BX)(CX*1)
	LEAL	1(CX), DX
	MOVL	DX, TBL_ARRLEN(SI)
	DISPATCH_NEXT
settab_app_generic:
	CMPB	DL, $ARR_GENERIC
	JNE	exit_cold
	CMPQ	DI, $TAG_NIL
	JE	exit_cold
	LEAQ	-TAG_FIRST_GC(DI), DX
	CMPQ	DX, $(TAG_LAST_GC-TAG_FIRST_GC)
	JA	settab_app_gen_store
	TESTB	$(1<<MARK_BLACK_BIT), TBL_MARK(SI)
	JNE	exit_cold
settab_app_gen_store:
	LEAL	1(CX), DX
	MOVL	DX, TBL_ARRLEN(SI)
	SHLQ	$4, CX
	MOVQ	AX, VAL_BITS(BX)(CX*1)
	MOVQ	DI, VAL_TAG(BX)(CX*1)
	DISPATCH_NEXT

// ============================================================================
// Arithmetic & Bitwise Ops (skip following OP_MMBIN* on success via ADDQ $4, R13)
// ============================================================================

op_ADDI:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), CX
	MOVL	R11, DX
	SHRL	$24, DX
	SUBQ	$OFFSET_SC, DX
	TESTQ	CX, CX
	JNE	addi_not_int
	ADDQ	DX, AX
	MOVL	R11, BX
	SHRL	$7, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVQ	AX, VAL_BITS(R12)(BX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(BX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT
addi_not_int:
	CMPQ	CX, $TAG_FLOAT
	JNE	exit_cold
	MOVQ	AX, X0
	CVTSQ2SD DX, X1
	ADDSD	X1, X0
	MOVL	R11, BX
	SHRL	$7, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVSD	X0, VAL_BITS(R12)(BX*1)
	MOVQ	$TAG_FLOAT, VAL_TAG(R12)(BX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_ADDK:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	MOVQ	VAL_BITS(R10)(CX*1), DX
	MOVQ	VAL_TAG(R10)(CX*1), DI
	MOVQ	SI, BX
	ORQ	DI, BX
	JNE	addk_not_both_int
	ADDQ	DX, AX
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVQ	AX, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT
addk_not_both_int:
	CMPQ	BX, $1
	JA	exit_cold
	TESTQ	SI, SI
	JE	addk_rb_int
	MOVQ	AX, X0
	JMP	addk_kc_conv
addk_rb_int:
	CVTSQ2SD AX, X0
addk_kc_conv:
	TESTQ	DI, DI
	JE	addk_kc_int
	MOVQ	DX, X1
	JMP	addk_fdo
addk_kc_int:
	CVTSQ2SD DX, X1
addk_fdo:
	ADDSD	X1, X0
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVSD	X0, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_FLOAT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_SUBK:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	MOVQ	VAL_BITS(R10)(CX*1), DX
	MOVQ	VAL_TAG(R10)(CX*1), DI
	MOVQ	SI, BX
	ORQ	DI, BX
	JNE	subk_not_both_int
	SUBQ	DX, AX
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVQ	AX, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT
subk_not_both_int:
	CMPQ	BX, $1
	JA	exit_cold
	TESTQ	SI, SI
	JE	subk_rb_int
	MOVQ	AX, X0
	JMP	subk_kc_conv
subk_rb_int:
	CVTSQ2SD AX, X0
subk_kc_conv:
	TESTQ	DI, DI
	JE	subk_kc_int
	MOVQ	DX, X1
	JMP	subk_fdo
subk_kc_int:
	CVTSQ2SD DX, X1
subk_fdo:
	SUBSD	X1, X0
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVSD	X0, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_FLOAT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_MULK:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	MOVQ	VAL_BITS(R10)(CX*1), DX
	MOVQ	VAL_TAG(R10)(CX*1), DI
	MOVQ	SI, BX
	ORQ	DI, BX
	JNE	mulk_not_both_int
	IMULQ	DX, AX
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVQ	AX, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT
mulk_not_both_int:
	CMPQ	BX, $1
	JA	exit_cold
	TESTQ	SI, SI
	JE	mulk_rb_int
	MOVQ	AX, X0
	JMP	mulk_kc_conv
mulk_rb_int:
	CVTSQ2SD AX, X0
mulk_kc_conv:
	TESTQ	DI, DI
	JE	mulk_kc_int
	MOVQ	DX, X1
	JMP	mulk_fdo
mulk_kc_int:
	CVTSQ2SD DX, X1
mulk_fdo:
	MULSD	X1, X0
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVSD	X0, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_FLOAT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_DIVK:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	MOVQ	VAL_BITS(R10)(CX*1), DX
	MOVQ	VAL_TAG(R10)(CX*1), DI
	MOVQ	SI, BX
	ORQ	DI, BX
	CMPQ	BX, $1
	JA	exit_cold
	TESTQ	SI, SI
	JE	divk_rb_int
	MOVQ	AX, X0
	JMP	divk_kc_conv
divk_rb_int:
	CVTSQ2SD AX, X0
divk_kc_conv:
	TESTQ	DI, DI
	JE	divk_kc_int
	MOVQ	DX, X1
	JMP	divk_fdo
divk_kc_int:
	CVTSQ2SD DX, X1
divk_fdo:
	DIVSD	X1, X0
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVSD	X0, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_FLOAT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_MODK:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	ORQ	VAL_TAG(R10)(CX*1), SI
	JNE	exit_cold
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_BITS(R10)(CX*1), SI
	TESTQ	AX, AX
	JS	exit_cold
	CMPQ	SI, $0
	JLE	exit_cold
	XORL	DX, DX
	DIVQ	SI                          // RDX = remainder
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVQ	DX, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_IDIVK:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	ORQ	VAL_TAG(R10)(CX*1), SI
	JNE	exit_cold
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_BITS(R10)(CX*1), SI
	TESTQ	AX, AX
	JS	exit_cold
	CMPQ	SI, $0
	JLE	exit_cold
	XORL	DX, DX
	DIVQ	SI                          // RAX = quotient
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVQ	AX, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_BANDK:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	CMPQ	VAL_TAG(R12)(BX*1), $TAG_INT
	JNE	exit_cold
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_BITS(R12)(BX*1), AX
	ANDQ	VAL_BITS(R10)(CX*1), AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_BORK:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	CMPQ	VAL_TAG(R12)(BX*1), $TAG_INT
	JNE	exit_cold
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_BITS(R12)(BX*1), AX
	ORQ	VAL_BITS(R10)(CX*1), AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_BXORK:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	CMPQ	VAL_TAG(R12)(BX*1), $TAG_INT
	JNE	exit_cold
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_BITS(R12)(BX*1), AX
	XORQ	VAL_BITS(R10)(CX*1), AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

// OP_SHLI: R[A] = sC << R[B]
op_SHLI:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	CMPQ	VAL_TAG(R12)(BX*1), $TAG_INT
	JNE	exit_cold
	MOVQ	VAL_BITS(R12)(BX*1), CX
	CMPQ	CX, $64
	JAE	exit_cold
	MOVL	R11, AX
	SHRL	$24, AX
	SUBQ	$OFFSET_SC, AX
	SHLQ	CL, AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

// OP_SHRI: R[A] = uint64(R[B]) >> sC
op_SHRI:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	CMPQ	VAL_TAG(R12)(BX*1), $TAG_INT
	JNE	exit_cold
	MOVL	R11, CX
	SHRL	$24, CX
	SUBQ	$OFFSET_SC, CX
	CMPQ	CX, $64
	JAE	exit_cold
	MOVQ	VAL_BITS(R12)(BX*1), AX
	SHRQ	CL, AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_ADD:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	MOVQ	VAL_BITS(R12)(CX*1), DX
	MOVQ	VAL_TAG(R12)(CX*1), DI
	MOVQ	SI, BX
	ORQ	DI, BX
	JNE	add_not_both_int
	ADDQ	DX, AX
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVQ	AX, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT
add_not_both_int:
	CMPQ	BX, $1
	JA	exit_cold
	TESTQ	SI, SI
	JE	add_rb_int
	MOVQ	AX, X0
	JMP	add_rc_conv
add_rb_int:
	CVTSQ2SD AX, X0
add_rc_conv:
	TESTQ	DI, DI
	JE	add_rc_int
	MOVQ	DX, X1
	JMP	add_fdo
add_rc_int:
	CVTSQ2SD DX, X1
add_fdo:
	ADDSD	X1, X0
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVSD	X0, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_FLOAT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_SUB:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	MOVQ	VAL_BITS(R12)(CX*1), DX
	MOVQ	VAL_TAG(R12)(CX*1), DI
	MOVQ	SI, BX
	ORQ	DI, BX
	JNE	sub_not_both_int
	SUBQ	DX, AX
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVQ	AX, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT
sub_not_both_int:
	CMPQ	BX, $1
	JA	exit_cold
	TESTQ	SI, SI
	JE	sub_rb_int
	MOVQ	AX, X0
	JMP	sub_rc_conv
sub_rb_int:
	CVTSQ2SD AX, X0
sub_rc_conv:
	TESTQ	DI, DI
	JE	sub_rc_int
	MOVQ	DX, X1
	JMP	sub_fdo
sub_rc_int:
	CVTSQ2SD DX, X1
sub_fdo:
	SUBSD	X1, X0
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVSD	X0, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_FLOAT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_MUL:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	MOVQ	VAL_BITS(R12)(CX*1), DX
	MOVQ	VAL_TAG(R12)(CX*1), DI
	MOVQ	SI, BX
	ORQ	DI, BX
	JNE	mul_not_both_int
	IMULQ	DX, AX
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVQ	AX, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT
mul_not_both_int:
	CMPQ	BX, $1
	JA	exit_cold
	TESTQ	SI, SI
	JE	mul_rb_int
	MOVQ	AX, X0
	JMP	mul_rc_conv
mul_rb_int:
	CVTSQ2SD AX, X0
mul_rc_conv:
	TESTQ	DI, DI
	JE	mul_rc_int
	MOVQ	DX, X1
	JMP	mul_fdo
mul_rc_int:
	CVTSQ2SD DX, X1
mul_fdo:
	MULSD	X1, X0
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVSD	X0, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_FLOAT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_DIV:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	MOVQ	VAL_BITS(R12)(CX*1), DX
	MOVQ	VAL_TAG(R12)(CX*1), DI
	MOVQ	SI, BX
	ORQ	DI, BX
	CMPQ	BX, $1
	JA	exit_cold
	TESTQ	SI, SI
	JE	div_rb_int
	MOVQ	AX, X0
	JMP	div_rc_conv
div_rb_int:
	CVTSQ2SD AX, X0
div_rc_conv:
	TESTQ	DI, DI
	JE	div_rc_int
	MOVQ	DX, X1
	JMP	div_fdo
div_rc_int:
	CVTSQ2SD DX, X1
div_fdo:
	DIVSD	X1, X0
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVSD	X0, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_FLOAT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_MOD:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	ORQ	VAL_TAG(R12)(CX*1), SI
	JNE	exit_cold
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_BITS(R12)(CX*1), SI
	TESTQ	AX, AX
	JS	exit_cold
	CMPQ	SI, $0
	JLE	exit_cold
	XORL	DX, DX
	DIVQ	SI
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVQ	DX, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_IDIV:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	ORQ	VAL_TAG(R12)(CX*1), SI
	JNE	exit_cold
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_BITS(R12)(CX*1), SI
	TESTQ	AX, AX
	JS	exit_cold
	CMPQ	SI, $0
	JLE	exit_cold
	XORL	DX, DX
	DIVQ	SI
	MOVL	R11, CX
	SHRL	$7, CX
	MOVBLZX	CL, CX
	SHLQ	$4, CX
	MOVQ	AX, VAL_BITS(R12)(CX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(CX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_BAND:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	ORQ	VAL_TAG(R12)(CX*1), SI
	JNE	exit_cold
	MOVQ	VAL_BITS(R12)(BX*1), AX
	ANDQ	VAL_BITS(R12)(CX*1), AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_BOR:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	ORQ	VAL_TAG(R12)(CX*1), SI
	JNE	exit_cold
	MOVQ	VAL_BITS(R12)(BX*1), AX
	ORQ	VAL_BITS(R12)(CX*1), AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_BXOR:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	ORQ	VAL_TAG(R12)(CX*1), SI
	JNE	exit_cold
	MOVQ	VAL_BITS(R12)(BX*1), AX
	XORQ	VAL_BITS(R12)(CX*1), AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_SHL:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	ORQ	VAL_TAG(R12)(CX*1), SI
	JNE	exit_cold
	MOVQ	VAL_BITS(R12)(CX*1), CX
	CMPQ	CX, $64
	JAE	exit_cold
	MOVQ	VAL_BITS(R12)(BX*1), AX
	SHLQ	CL, AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_SHR:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVL	R11, CX
	SHRL	$24, CX
	SHLQ	$4, CX
	MOVQ	VAL_TAG(R12)(BX*1), SI
	ORQ	VAL_TAG(R12)(CX*1), SI
	JNE	exit_cold
	MOVQ	VAL_BITS(R12)(CX*1), CX
	CMPQ	CX, $64
	JAE	exit_cold
	MOVQ	VAL_BITS(R12)(BX*1), AX
	SHRQ	CL, AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	ADDQ	$4, R13
	DISPATCH_NEXT

op_UNM:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVQ	VAL_BITS(R12)(BX*1), AX
	MOVQ	VAL_TAG(R12)(BX*1), CX
	TESTQ	CX, CX
	JNE	unm_not_int
	NEGQ	AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	DISPATCH_NEXT
unm_not_int:
	CMPQ	CX, $TAG_FLOAT
	JNE	exit_cold
	BTCQ	$63, AX                     // flip IEEE 754 sign bit
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_FLOAT, VAL_TAG(R12)(DX*1)
	DISPATCH_NEXT

op_BNOT:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	CMPQ	VAL_TAG(R12)(BX*1), $TAG_INT
	JNE	exit_cold
	MOVQ	VAL_BITS(R12)(BX*1), AX
	NOTQ	AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	DISPATCH_NEXT

op_NOT:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVQ	VAL_TAG(R12)(BX*1), AX
	SUBQ	$TAG_NIL, AX
	CMPQ	AX, $1
	SETLS	AL                          // 1 if falsy (-> true), 0 if truthy (-> false)
	MOVBLZX	AL, AX
	ADDQ	$TAG_FALSE, AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	$0, VAL_BITS(R12)(DX*1)
	MOVQ	AX, VAL_TAG(R12)(DX*1)
	DISPATCH_NEXT

op_LEN:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVQ	VAL_BITS(R12)(BX*1), SI
	MOVQ	VAL_TAG(R12)(BX*1), CX
	CMPQ	CX, $TAG_TABLE
	JNE	len_check_str
	RESOLVE_SMALL(SI, SI, DX)
	CMPL	TBL_METAREF(SI), $0
	JNE	exit_cold
	CMPL	TBL_HASHREF(SI), $0
	JNE	exit_cold
	MOVBLZX	TBL_ARRKIND(SI), AX
	SUBL	$ARR_INT, AX
	CMPL	AX, $(ARR_BOOL-ARR_INT)
	JA	exit_cold
	MOVL	TBL_ARRLEN(SI), AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	DISPATCH_NEXT
len_check_str:
	CMPQ	CX, $TAG_STRING
	JNE	exit_cold
	RESOLVE_FAST(SI, SI, DX, len_str_large, len_str_ready)
	MOVL	STR_LEN(SI), AX
	MOVL	R11, DX
	SHRL	$7, DX
	MOVBLZX	DL, DX
	SHLQ	$4, DX
	MOVQ	AX, VAL_BITS(R12)(DX*1)
	MOVQ	$TAG_INT, VAL_TAG(R12)(DX*1)
	DISPATCH_NEXT

// ============================================================================
// Branches, Comparisons & Loops
// ============================================================================

op_JMP:
	MOVL	R11, AX
	SHRL	$7, AX
	SUBQ	$OFFSET_SJ, AX
	LEAQ	(R13)(AX*4), R13
	TESTQ	AX, AX
	JS	jmp_back
	DISPATCH_NEXT
jmp_back:
	DECQ	CTX_POLL(R15)
	JLE	exit_poll
	DISPATCH_NEXT

op_EQ:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVQ	VAL_BITS(R12)(AX*1), SI
	MOVQ	VAL_TAG(R12)(AX*1), CX
	MOVQ	VAL_BITS(R12)(BX*1), DI
	MOVQ	VAL_TAG(R12)(BX*1), DX
	MOVQ	CX, AX
	ORQ	DX, AX
	JNE	eq_not_both_int
	CMPQ	SI, DI
	SETEQ	AL
	COND_DISPATCH(eq_skip, eq_back)
eq_not_both_int:
	CMPQ	CX, DX
	JNE	eq_diff_tag
	CMPQ	CX, $TAG_FLOAT
	JE	eq_both_float
	CMPQ	SI, DI
	JE	eq_true
	CMPQ	CX, $TAG_TABLE
	JE	eq_diff_tables
	CMPQ	CX, $TAG_USERDATA
	JE	exit_cold
	CMPQ	CX, $TAG_STRING
	JE	eq_diff_str
	CMPQ	CX, $TAG_FROZENSTR
	JE	exit_cold
eq_false:
	XORL	AX, AX
	COND_DISPATCH(eq_skip2, eq_back2)
eq_true:
	MOVL	$1, AX
	COND_DISPATCH(eq_skip3, eq_back3)
eq_both_float:
	MOVQ	SI, X0
	MOVQ	DI, X1
	UCOMISD	X1, X0
	SETEQ	AL
	SETPC	CL
	ANDB	CL, AL
	COND_DISPATCH(eq_skip4, eq_back4)
eq_diff_tables:
	RESOLVE_SMALL(SI, SI, BX)
	CMPL	TBL_METAREF(SI), $0
	JNE	exit_cold
	RESOLVE_SMALL(DI, DI, BX)
	CMPL	TBL_METAREF(DI), $0
	JNE	exit_cold
	JMP	eq_false
eq_diff_str:
	TESTL	$0x80000000, SI
	JNE	exit_cold
	TESTL	$0x80000000, DI
	JNE	exit_cold
	RESOLVE_SMALL(SI, SI, BX)
	CMPB	STR_KIND(SI), $GCKIND_SHORTSTR
	JNE	exit_cold
	RESOLVE_SMALL(DI, DI, BX)
	CMPB	STR_KIND(DI), $GCKIND_SHORTSTR
	JNE	exit_cold
	JMP	eq_false
eq_diff_tag:
	CMPQ	AX, $1
	JBE	exit_cold                   // int vs float -> exact boundary in Go
	SUBQ	$TAG_FROZENSTR, CX
	CMPQ	CX, $1
	JA	eq_false
	SUBQ	$TAG_FROZENSTR, DX
	CMPQ	DX, $1
	JBE	exit_cold                   // frozen str vs dynamic str -> Go
	JMP	eq_false

op_LT:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVQ	VAL_BITS(R12)(AX*1), SI
	MOVQ	VAL_TAG(R12)(AX*1), CX
	MOVQ	VAL_BITS(R12)(BX*1), DI
	MOVQ	VAL_TAG(R12)(BX*1), DX
	MOVQ	CX, AX
	ORQ	DX, AX
	JNE	lt_not_both_int
	CMPQ	SI, DI
	SETLT	AL
	COND_DISPATCH(lt_skip, lt_back)
lt_not_both_int:
	CMPQ	CX, $TAG_FLOAT
	JNE	exit_cold
	CMPQ	DX, $TAG_FLOAT
	JNE	exit_cold
	MOVQ	SI, X0                      // ra
	MOVQ	DI, X1                      // rb
	UCOMISD	X0, X1                      // X1 - X0 = rb - ra; HI (JA) => rb > ra <=> ra < rb (NaN-safe!)
	SETHI	AL
	COND_DISPATCH(lt_skip2, lt_back2)

op_LE:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVQ	VAL_BITS(R12)(AX*1), SI
	MOVQ	VAL_TAG(R12)(AX*1), CX
	MOVQ	VAL_BITS(R12)(BX*1), DI
	MOVQ	VAL_TAG(R12)(BX*1), DX
	MOVQ	CX, AX
	ORQ	DX, AX
	JNE	le_not_both_int
	CMPQ	SI, DI
	SETLE	AL
	COND_DISPATCH(le_skip, le_back)
le_not_both_int:
	CMPQ	CX, $TAG_FLOAT
	JNE	exit_cold
	CMPQ	DX, $TAG_FLOAT
	JNE	exit_cold
	MOVQ	SI, X0
	MOVQ	DI, X1
	UCOMISD	X0, X1                      // rb - ra; CC (JAE) => rb >= ra <=> ra <= rb (NaN-safe!)
	SETCC	AL
	COND_DISPATCH(le_skip2, le_back2)

op_EQK:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVQ	VAL_BITS(R12)(AX*1), SI
	MOVQ	VAL_TAG(R12)(AX*1), CX
	MOVQ	VAL_BITS(R10)(BX*1), DI
	MOVQ	VAL_TAG(R10)(BX*1), DX
	MOVQ	CX, AX
	ORQ	DX, AX
	JNE	eqk_not_both_int
	CMPQ	SI, DI
	SETEQ	AL
	COND_DISPATCH(eqk_skip, eqk_back)
eqk_not_both_int:
	CMPQ	CX, DX
	JNE	eqk_diff_tag
	CMPQ	CX, $TAG_FLOAT
	JE	eqk_both_float
	CMPQ	SI, DI
	JE	eqk_true
	CMPQ	CX, $TAG_STRING
	JE	eqk_diff_str
	CMPQ	CX, $TAG_FROZENSTR
	JE	exit_cold
eqk_false:
	XORL	AX, AX
	COND_DISPATCH(eqk_skip2, eqk_back2)
eqk_true:
	MOVL	$1, AX
	COND_DISPATCH(eqk_skip3, eqk_back3)
eqk_both_float:
	MOVQ	SI, X0
	MOVQ	DI, X1
	UCOMISD	X1, X0
	SETEQ	AL
	SETPC	CL
	ANDB	CL, AL
	COND_DISPATCH(eqk_skip4, eqk_back4)
eqk_diff_str:
	TESTL	$0x80000000, SI
	JNE	exit_cold
	TESTL	$0x80000000, DI
	JNE	exit_cold
	RESOLVE_SMALL(SI, SI, BX)
	CMPB	STR_KIND(SI), $GCKIND_SHORTSTR
	JNE	exit_cold
	RESOLVE_SMALL(DI, DI, BX)
	CMPB	STR_KIND(DI), $GCKIND_SHORTSTR
	JNE	exit_cold
	JMP	eqk_false
eqk_diff_tag:
	CMPQ	AX, $1
	JBE	exit_cold
	SUBQ	$TAG_FROZENSTR, CX
	CMPQ	CX, $1
	JA	eqk_false
	SUBQ	$TAG_FROZENSTR, DX
	CMPQ	DX, $1
	JBE	exit_cold
	JMP	eqk_false

op_EQI:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVQ	VAL_BITS(R12)(AX*1), SI
	MOVQ	VAL_TAG(R12)(AX*1), CX
	MOVL	R11, DI
	SHRL	$16, DI
	MOVBLZX	DI, DI
	SUBQ	$OFFSET_SC, DI
	TESTQ	CX, CX
	JNE	eqi_not_int
	CMPQ	SI, DI
	SETEQ	AL
	COND_DISPATCH(eqi_skip, eqi_back)
eqi_not_int:
	CMPQ	CX, $TAG_FLOAT
	JNE	eqi_false
	MOVQ	SI, X0
	CVTSQ2SD DI, X1
	UCOMISD	X1, X0
	SETEQ	AL
	SETPC	CL
	ANDB	CL, AL
	COND_DISPATCH(eqi_skip2, eqi_back2)
eqi_false:
	XORL	AX, AX
	COND_DISPATCH(eqi_skip3, eqi_back3)

op_LTI:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVQ	VAL_BITS(R12)(AX*1), SI
	MOVQ	VAL_TAG(R12)(AX*1), CX
	MOVL	R11, DI
	SHRL	$16, DI
	MOVBLZX	DI, DI
	SUBQ	$OFFSET_SC, DI
	TESTQ	CX, CX
	JNE	lti_not_int
	CMPQ	SI, DI
	SETLT	AL
	COND_DISPATCH(lti_skip, lti_back)
lti_not_int:
	CMPQ	CX, $TAG_FLOAT
	JNE	exit_cold
	MOVQ	SI, X0
	CVTSQ2SD DI, X1
	UCOMISD	X0, X1                      // im - ra; HI => ra < im (NaN-safe!)
	SETHI	AL
	COND_DISPATCH(lti_skip2, lti_back2)

op_LEI:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVQ	VAL_BITS(R12)(AX*1), SI
	MOVQ	VAL_TAG(R12)(AX*1), CX
	MOVL	R11, DI
	SHRL	$16, DI
	MOVBLZX	DI, DI
	SUBQ	$OFFSET_SC, DI
	TESTQ	CX, CX
	JNE	lei_not_int
	CMPQ	SI, DI
	SETLE	AL
	COND_DISPATCH(lei_skip, lei_back)
lei_not_int:
	CMPQ	CX, $TAG_FLOAT
	JNE	exit_cold
	MOVQ	SI, X0
	CVTSQ2SD DI, X1
	UCOMISD	X0, X1                      // im - ra; CC => ra <= im (NaN-safe!)
	SETCC	AL
	COND_DISPATCH(lei_skip2, lei_back2)

op_GTI:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVQ	VAL_BITS(R12)(AX*1), SI
	MOVQ	VAL_TAG(R12)(AX*1), CX
	MOVL	R11, DI
	SHRL	$16, DI
	MOVBLZX	DI, DI
	SUBQ	$OFFSET_SC, DI
	TESTQ	CX, CX
	JNE	gti_not_int
	CMPQ	SI, DI
	SETGT	AL
	COND_DISPATCH(gti_skip, gti_back)
gti_not_int:
	CMPQ	CX, $TAG_FLOAT
	JNE	exit_cold
	MOVQ	SI, X0
	CVTSQ2SD DI, X1
	UCOMISD	X1, X0                      // ra - im; HI => ra > im (NaN-safe!)
	SETHI	AL
	COND_DISPATCH(gti_skip2, gti_back2)

op_GEI:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVQ	VAL_BITS(R12)(AX*1), SI
	MOVQ	VAL_TAG(R12)(AX*1), CX
	MOVL	R11, DI
	SHRL	$16, DI
	MOVBLZX	DI, DI
	SUBQ	$OFFSET_SC, DI
	TESTQ	CX, CX
	JNE	gei_not_int
	CMPQ	SI, DI
	SETGE	AL
	COND_DISPATCH(gei_skip, gei_back)
gei_not_int:
	CMPQ	CX, $TAG_FLOAT
	JNE	exit_cold
	MOVQ	SI, X0
	CVTSQ2SD DI, X1
	UCOMISD	X1, X0                      // ra - im; CC => ra >= im (NaN-safe!)
	SETCC	AL
	COND_DISPATCH(gei_skip2, gei_back2)

op_TEST:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVQ	VAL_TAG(R12)(AX*1), CX
	SUBQ	$TAG_NIL, CX
	CMPQ	CX, $1
	SETHI	AL                          // 1 if truthy, 0 if falsy
	COND_DISPATCH(test_skip, test_back)

op_TESTSET:
	MOVL	R11, BX
	SHRL	$16, BX
	MOVBLZX	BL, BX
	SHLQ	$4, BX
	MOVQ	VAL_BITS(R12)(BX*1), SI
	MOVQ	VAL_TAG(R12)(BX*1), DX
	LEAQ	-TAG_NIL(DX), CX
	CMPQ	CX, $1
	SETHI	AL
	MOVL	R11, CX
	SHRL	$15, CX
	ANDL	$1, CX
	CMPB	AL, CL
	JNE	testset_skip
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVQ	SI, VAL_BITS(R12)(AX*1)
	MOVQ	DX, VAL_TAG(R12)(AX*1)
	MOVL	(R13), AX
	SHRL	$7, AX
	SUBQ	$OFFSET_SJ, AX
	LEAQ	4(R13)(AX*4), R13
	TESTQ	AX, AX
	JS	testset_back
	DISPATCH_NEXT
testset_back:
	DECQ	CTX_POLL(R15)
	JLE	exit_poll
	DISPATCH_NEXT
testset_skip:
	ADDQ	$4, R13
	DISPATCH_NEXT

op_FORLOOP:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	LEAQ	(R12)(AX*1), SI             // &regs[A]
	MOVQ	(VAL_SIZE+VAL_TAG)(SI), CX  // step.tag
	TESTQ	CX, CX
	JNE	forloop_float
	MOVQ	VAL_TAG(SI), DX
	ORQ	(2*VAL_SIZE+VAL_TAG)(SI), DX
	JNE	forloop_done
	MOVQ	VAL_BITS(SI), BX            // cnt (uint64)
	TESTQ	BX, BX
	JE	forloop_done
	DECQ	BX
	MOVQ	BX, VAL_BITS(SI)
	MOVQ	(VAL_SIZE+VAL_BITS)(SI), DX // step
	ADDQ	DX, (2*VAL_SIZE+VAL_BITS)(SI)
	MOVL	R11, CX
	SHRL	$15, CX
	SHLQ	$2, CX
	SUBQ	CX, R13
	DECQ	CTX_POLL(R15)
	JLE	exit_poll
	DISPATCH_NEXT
forloop_float:
	CMPQ	CX, $TAG_FLOAT
	JNE	forloop_done
	CMPQ	VAL_TAG(SI), $TAG_FLOAT
	JNE	forloop_done
	CMPQ	(2*VAL_SIZE+VAL_TAG)(SI), $TAG_FLOAT
	JNE	forloop_done
	MOVSD	(VAL_SIZE+VAL_BITS)(SI), X0 // step
	MOVSD	VAL_BITS(SI), X1            // lim
	MOVSD	(2*VAL_SIZE+VAL_BITS)(SI), X2 // idx
	ADDSD	X0, X2                      // idx += step
	XORPD	X3, X3                      // 0.0
	UCOMISD	X3, X0                      // step - 0.0
	JHI	forloop_fpos                // step > 0 (NaN-safe!)
	UCOMISD	X0, X3                      // 0.0 - step
	JCC	forloop_fneg                // step <= 0 (rejects NaN!)
	DISPATCH_NEXT
forloop_fpos:
	UCOMISD	X2, X1                      // lim - idx; CC => idx <= lim (NaN-safe!)
	JCC	forloop_ftake
	DISPATCH_NEXT
forloop_fneg:
	UCOMISD	X1, X2                      // idx - lim; CC => idx >= lim (NaN-safe!)
	JCC	forloop_ftake
	DISPATCH_NEXT
forloop_ftake:
	MOVSD	X2, (2*VAL_SIZE+VAL_BITS)(SI)
	MOVL	R11, CX
	SHRL	$15, CX
	SHLQ	$2, CX
	SUBQ	CX, R13
	DECQ	CTX_POLL(R15)
	JLE	exit_poll
	DISPATCH_NEXT
forloop_done:
	DISPATCH_NEXT

op_TFORLOOP:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	CMPQ	(3*VAL_SIZE+VAL_TAG)(R12)(AX*1), $TAG_NIL
	JE	tforloop_done
	MOVL	R11, CX
	SHRL	$15, CX
	SHLQ	$2, CX
	SUBQ	CX, R13
	DECQ	CTX_POLL(R15)
	JLE	exit_poll
tforloop_done:
	DISPATCH_NEXT

op_FORPREP:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	LEAQ	(R12)(AX*1), SI             // &regs[A]
	MOVQ	VAL_TAG(SI), DX
	ORQ	(VAL_SIZE+VAL_TAG)(SI), DX
	ORQ	(2*VAL_SIZE+VAL_TAG)(SI), DX
	JNE	exit_cold
	MOVQ	VAL_BITS(SI), BX            // init
	MOVQ	(VAL_SIZE+VAL_BITS)(SI), AX // lim
	MOVQ	(2*VAL_SIZE+VAL_BITS)(SI), CX // step
	CMPQ	CX, $0
	JLE	forprep_nonpos
	// step > 0
	CMPQ	BX, AX                      // init > lim?
	JG	forprep_skip
	SUBQ	BX, AX                      // uint64(lim) - uint64(init)
	CMPQ	CX, $1
	JE	forprep_store
	XORL	DX, DX
	DIVQ	CX                          // RAX = cnt
forprep_store:
	MOVQ	AX, VAL_BITS(SI)            // regs[A] = cnt
	MOVQ	CX, (VAL_SIZE+VAL_BITS)(SI) // regs[A+1] = step
	MOVQ	BX, (2*VAL_SIZE+VAL_BITS)(SI) // regs[A+2] = init
	DISPATCH_NEXT
forprep_nonpos:
	TESTQ	CX, CX
	JE	exit_cold                   // step == 0 -> error in Go
	CMPQ	BX, AX                      // init < lim?
	JL	forprep_skip
	MOVQ	BX, DX
	SUBQ	AX, DX                      // uint64(init) - uint64(lim)
	MOVQ	DX, AX                      // dividend in RAX
	LEAQ	1(CX), DI                   // step + 1 (overflow-safe for MinInt64)
	NEGQ	DI                          // -(step + 1)
	INCQ	DI                          // uint64(-(step+1)) + 1
	XORL	DX, DX
	DIVQ	DI                          // RAX = cnt
	JMP	forprep_store
forprep_skip:
	MOVL	R11, AX
	SHRL	$15, AX
	LEAQ	4(R13)(AX*4), R13
	DISPATCH_NEXT

// ============================================================================
// Fast Lua-to-Lua Tail Call, Call & Return
// ============================================================================

op_TAILCALL:
	TESTL	$(1<<15), R11
	JNE	exit_cold                   // K == 1 -> closeUpvals in Go
	MOVL	R11, CX
	SHRL	$16, CX
	MOVBLZX	CL, CX                      // B
	TESTL	CX, CX
	JE	exit_cold                   // B == 0 (MULTARG) -> Go
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	LEAQ	(R12)(AX*1), SI             // SI = &srcStart
	CMPQ	VAL_TAG(SI), $TAG_CLOSURE
	JNE	exit_cold
	MOVL	VAL_BITS(SI), BX            // callee.self (ObjRef)
	MOVQ	BX, X0                      // save callee.self
	RESOLVE_SMALL(BX, BX, DX)           // BX = *Closure
	MOVQ	BX, X3                      // save *Closure
	MOVL	CL_PROTOIDX(BX), AX
	MOVQ	CTX_PROTOS(R15), DX
	MOVQ	(DX)(AX*8), DI              // DI = *boundProto
	CMPB	BP_ISVARARG(DI), $0
	JNE	exit_cold
	// Load current callFrame cf = &frames[curCI]
	MOVQ	CTX_CI(R15), AX
	IMULQ	$CF_SIZE, AX
	ADDQ	CTX_FRAMES(R15), AX         // AX = cf
	MOVQ	AX, X2                      // save cf
	MOVQ	CF_FUNCIDX(AX), DX          // funcIdx
	MOVQ	DX, X1                      // save funcIdx
	DECL	CX                          // nargs = B - 1
	MOVBLZX	BP_MAXSTACK(DI), BX         // maxStack
	MOVQ	BX, AX
	CMPQ	CX, BX
	CMOVQGT	CX, AX                      // max(nargs, maxStack)
	LEAQ	(1+STACK_SLACK)(DX)(AX*1), AX // need = funcIdx + 1 + max(nargs, maxStack) + STACK_SLACK
	MOVQ	CTX_TH(R15), BX
	CMPQ	AX, TH_STACKLEN(BX)
	JG	exit_cold                   // stack growth -> Go
	// All exit_cold guards passed! Copy [srcStart .. srcStart+nargs] -> [funcIdx .. funcIdx+nargs].
	MOVQ	DX, AX
	SHLQ	$4, AX
	ADDQ	CTX_STACK(R15), AX          // AX = dst = &stack[funcIdx]
	LEAQ	1(CX), BX                   // count = 1 + nargs (>= 1)
tailcall_copy_loop:
	MOVOU	(SI), X4
	MOVOU	X4, (AX)
	ADDQ	$16, SI
	ADDQ	$16, AX
	DECQ	BX
	JNE	tailcall_copy_loop
	// Fill [newBase+nargs .. newBase+maxStack) with NilValue if nargs < maxStack
	MOVBLZX	BP_MAXSTACK(DI), BX         // maxStack
	CMPQ	CX, BX
	JGE	tailcall_nil_done
	MOVQ	BX, SI
	SUBQ	CX, SI                      // maxStack - nargs (> 0)
tailcall_nil_loop:
	MOVQ	$0, VAL_BITS(AX)
	MOVQ	$TAG_NIL, VAL_TAG(AX)
	ADDQ	$16, AX
	DECQ	SI
	JNE	tailcall_nil_loop
tailcall_nil_done:
	// Update Thread top & highStack
	MOVQ	X1, DX                      // funcIdx
	INCQ	DX                          // newBase = funcIdx + 1
	LEAQ	(DX)(BX*1), AX              // frameEnd = newBase + maxStack
	MOVQ	CTX_TH(R15), SI
	MOVQ	AX, TH_TOP(SI)
	CMPQ	AX, TH_HIGHSTACK(SI)
	JLE	tailcall_hs_ok
	MOVQ	AX, TH_HIGHSTACK(SI)
tailcall_hs_ok:
	// Update callFrame cf in place matching interp_cold.go:3336-3344
	MOVQ	X2, SI                      // cf
	MOVQ	X0, AX                      // callee.self
	MOVL	AX, CF_CLREF(SI)
	MOVL	$0, CF_VARARGTABREF(SI)
	MOVQ	DX, CF_BASE(SI)
	MOVQ	$0, CF_PC(SI)
	MOVQ	$0, CF_NEXTRAARGS(SI)
	MOVB	$1, CF_DIRTY(SI)
	MOVB	$1, CF_ISTAILCALL(SI)
	MOVB	$0, CF_EXTRAARGS(SI)
	MOVQ	$-1, CF_OLDPC(SI)
	// Update ctx & pinned registers for callee
	MOVQ	DX, CTX_BASE(R15)
	SHLQ	$4, DX
	MOVQ	CTX_STACK(R15), R12
	ADDQ	DX, R12                     // R12 = &stack[newBase]
	MOVQ	X3, BX                      // *Closure
	MOVL	CL_UPVALSREF(BX), AX
	TESTL	AX, AX
	JE	tailcall_no_upvals
	RESOLVE_SMALL(AX, AX, CX)
	MOVQ	AX, CTX_CLUPVALS(R15)
	JMP	tailcall_check_bp
tailcall_no_upvals:
	MOVQ	$0, CTX_CLUPVALS(R15)
tailcall_check_bp:
	CMPQ	DI, CTX_CURBP(R15)
	JE	tailcall_same_bp
	MOVQ	DI, CTX_CURBP(R15)
	MOVQ	BP_INSNS(DI), R13
	MOVQ	BP_CONSTS(DI), R10
	MOVQ	BP_ICSLOTS(DI), AX
	MOVQ	BP_IC(DI), CX
	MOVQ	R13, CTX_INSNS(R15)
	MOVQ	R10, CTX_CONSTS(R15)
	MOVQ	AX, CTX_ICSLOTS(R15)
	MOVQ	CX, CTX_IC(R15)
	JMP	tailcall_poll
tailcall_same_bp:
	MOVQ	CTX_INSNS(R15), R13
tailcall_poll:
	DECQ	CTX_POLL(R15)
	JLE	exit_poll
	DISPATCH_NEXT

op_CALL:
	MOVL	R11, CX
	SHRL	$16, CX
	MOVBLZX	CL, CX                      // B
	TESTL	CX, CX
	JE	exit_cold
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX                      // A
	MOVQ	AX, DX
	SHLQ	$4, DX
	LEAQ	(R12)(DX*1), SI             // SI = &regs[A]
	CMPQ	VAL_TAG(SI), $TAG_CLOSURE
	JNE	call_check_static_nat
	MOVL	VAL_BITS(SI), BX            // callee.self
	MOVQ	BX, X0                      // save callee.self
	RESOLVE_SMALL(BX, BX, DX)           // BX = *Closure
	MOVL	CL_PROTOIDX(BX), DX
	MOVQ	CTX_PROTOS(R15), DI
	MOVQ	(DI)(DX*8), DI              // DI = *boundProto
	CMPB	BP_ISVARARG(DI), $0
	JNE	exit_cold
	MOVQ	CTX_TH(R15), SI             // SI = *Thread
	MOVQ	TH_NFRAME(SI), DX           // newCI = th.nframe
	CMPQ	DX, TH_FRAMESCAP(SI)
	JGE	exit_cold
	ADDQ	CTX_BASE(R15), AX           // funcIdx = curBase + A
	MOVQ	AX, X1                      // save funcIdx
	INCQ	AX                          // newBase = funcIdx + 1
	MOVBLZX	BP_MAXSTACK(DI), DX
	ADDQ	AX, DX                      // frameEnd = newBase + maxStack
	LEAQ	STACK_SLACK(DX), SI
	MOVQ	CTX_TH(R15), DX             // reload *Thread
	CMPQ	SI, TH_STACKLEN(DX)
	JG	exit_cold
	// All exit_cold guards passed! Commit call frame.
	MOVQ	TH_NFRAME(DX), SI           // SI = newCI
	IMUL3Q	$CF_SIZE, SI, DX
	ADDQ	CTX_FRAMES(R15), DX         // DX = nf = &frames[newCI]
	CMPB	CF_DIRTY(DX), $0
	JE	call_nf_clean
	XORPS	X2, X2
	MOVUPS	X2, 0(DX)
	MOVUPS	X2, 16(DX)
	MOVUPS	X2, 32(DX)
	MOVUPS	X2, 48(DX)
	MOVUPS	X2, 64(DX)
	MOVUPS	X2, 80(DX)
	MOVUPS	X2, 96(DX)
	MOVUPS	X2, 112(DX)
	MOVUPS	X2, 128(DX)
	MOVUPS	X2, 144(DX)
	MOVUPS	X2, 160(DX)
	MOVUPS	X2, 176(DX)
	MOVQ	$0, 192(DX)
call_nf_clean:
	// Fill missing params with nil: nargs = B - 1 < nparams
	DECL	CX                          // nargs = B - 1
	MOVBLZX	BP_NUMPARAMS(DI), R12       // R12 temporarily = nparams (will be set to &stack[newBase] below)
	CMPQ	CX, R12
	JGE	call_params_done
	SUBQ	CX, R12                     // count = nparams - nargs (> 0)
	ADDQ	AX, CX                      // newBase + nargs
	SHLQ	$4, CX
	ADDQ	CTX_STACK(R15), CX          // &stack[newBase + nargs]
call_nil_loop:
	MOVQ	$0, VAL_BITS(CX)
	MOVQ	$TAG_NIL, VAL_TAG(CX)
	ADDQ	$16, CX
	DECQ	R12
	JNE	call_nil_loop
call_params_done:
	// Save caller PC into frames[curCI].pc
	MOVQ	R13, CX
	SUBQ	CTX_INSNS(R15), CX
	SHRQ	$2, CX
	MOVQ	CTX_CI(R15), R12
	IMULQ	$CF_SIZE, R12
	ADDQ	CTX_FRAMES(R15), R12
	MOVQ	CX, CF_PC(R12)
	// Populate callee callFrame nf (DX)
	MOVQ	X0, CX                      // callee.self
	MOVL	CX, CF_CLREF(DX)
	MOVQ	AX, CF_BASE(DX)             // newBase
	MOVQ	X1, CX                      // funcIdx
	MOVQ	CX, CF_FUNCIDX(DX)
	MOVQ	$0, CF_PC(DX)
	MOVL	R11, CX
	SHRL	$24, CX
	SUBQ	$1, CX                      // c - 1
	MOVQ	CX, CF_NRESULTS(DX)
	// Update Thread watermarks
	MOVQ	CTX_TH(R15), DX
	LEAQ	1(SI), CX                   // newCI + 1
	MOVQ	CX, TH_NFRAME(DX)
	CMPQ	CX, TH_HIGHFRAMES(DX)
	JLE	call_hf_ok
	MOVQ	CX, TH_HIGHFRAMES(DX)
call_hf_ok:
	MOVBLZX	BP_MAXSTACK(DI), CX
	ADDQ	AX, CX                      // frameEnd = newBase + maxStack
	CMPQ	CX, TH_HIGHSTACK(DX)
	JLE	call_hs_ok
	MOVQ	CX, TH_HIGHSTACK(DX)
call_hs_ok:
	// Update ctx & pinned registers for callee
	MOVQ	SI, CTX_CI(R15)
	MOVQ	AX, CTX_BASE(R15)
	SHLQ	$4, AX
	MOVQ	CTX_STACK(R15), R12
	ADDQ	AX, R12                     // R12 = &stack[newBase]
	// Resolve callee.upvalsRef -> ctx.clUpvals
	MOVL	CL_UPVALSREF(BX), AX
	TESTL	AX, AX
	JE	call_no_upvals
	RESOLVE_SMALL(AX, AX, CX)
	MOVQ	AX, CTX_CLUPVALS(R15)
	JMP	call_check_bp
call_no_upvals:
	MOVQ	$0, CTX_CLUPVALS(R15)
call_check_bp:
	CMPQ	DI, CTX_CURBP(R15)
	JE	call_same_bp
	MOVQ	DI, CTX_CURBP(R15)
	MOVQ	BP_INSNS(DI), R13
	MOVQ	BP_CONSTS(DI), R10
	MOVQ	BP_ICSLOTS(DI), AX
	MOVQ	BP_IC(DI), CX
	MOVQ	R13, CTX_INSNS(R15)
	MOVQ	R10, CTX_CONSTS(R15)
	MOVQ	AX, CTX_ICSLOTS(R15)
	MOVQ	CX, CTX_IC(R15)
	JMP	call_poll
call_same_bp:
	MOVQ	CTX_INSNS(R15), R13
call_poll:
	DECQ	CTX_POLL(R15)
	JLE	exit_poll
	DISPATCH_NEXT

// Fast hardware intrinsics for math.sqrt (SQRTSD) and math.abs (BTRQ $63 / NEG) when C==2, B>=2.
call_check_static_nat:
	CMPQ	VAL_TAG(SI), $TAG_STATICNAT
	JNE	exit_cold
	CMPL	CX, $2                      // B >= 2 (nargs >= 1)
	JB	exit_cold
	MOVL	R11, DX
	SHRL	$24, DX
	CMPL	DX, $2                      // C == 2 (single fixed result)
	JNE	exit_cold
	MOVL	VAL_BITS(SI), AX            // staticNative idx
	MOVL	CTX_SQRTIDX(R15), DX
	TESTL	DX, DX
	JE	call_check_abs
	CMPL	AX, DX
	JNE	call_check_abs
	MOVQ	(VAL_SIZE+VAL_TAG)(SI), CX
	CMPQ	CX, $TAG_FLOAT
	JE	call_sqrt_float
	TESTQ	CX, CX
	JNE	exit_cold
	CVTSQ2SD (VAL_SIZE+VAL_BITS)(SI), X0
	SQRTSD	X0, X0
	MOVSD	X0, VAL_BITS(SI)
	MOVQ	$TAG_FLOAT, VAL_TAG(SI)
	DISPATCH_NEXT
call_sqrt_float:
	SQRTSD	(VAL_SIZE+VAL_BITS)(SI), X0
	MOVSD	X0, VAL_BITS(SI)
	MOVQ	$TAG_FLOAT, VAL_TAG(SI)
	DISPATCH_NEXT
call_check_abs:
	MOVL	CTX_ABSIDX(R15), DX
	TESTL	DX, DX
	JE	exit_cold
	CMPL	AX, DX
	JNE	exit_cold
	MOVQ	(VAL_SIZE+VAL_BITS)(SI), BX
	MOVQ	(VAL_SIZE+VAL_TAG)(SI), CX
	CMPQ	CX, $TAG_FLOAT
	JE	call_abs_float
	TESTQ	CX, CX
	JNE	exit_cold
	TESTQ	BX, BX
	JNS	call_abs_int_store
	NEGQ	BX
call_abs_int_store:
	MOVQ	BX, VAL_BITS(SI)
	MOVQ	$TAG_INT, VAL_TAG(SI)
	DISPATCH_NEXT
call_abs_float:
	BTRQ	$63, BX                     // clear IEEE 754 sign bit (matches math.Abs)
	MOVQ	BX, VAL_BITS(SI)
	MOVQ	$TAG_FLOAT, VAL_TAG(SI)
	DISPATCH_NEXT

op_RETURN0:
	MOVQ	CTX_CI(R15), AX
	IMULQ	$CF_SIZE, AX
	ADDQ	CTX_FRAMES(R15), AX         // cf
	MOVQ	CF_NRESULTS(AX), CX         // nres
	MOVQ	CF_FUNCIDX(AX), DX          // funcIdx
	MOVQ	CTX_TH(R15), SI
	MOVQ	TH_NFRAME(SI), BX
	DECQ	BX                          // newNFrame
	MOVQ	BX, TH_NFRAME(SI)
	CMPQ	CX, $0
	JLE	ret0_no_fill
	MOVQ	DX, DI
	SHLQ	$4, DI
	ADDQ	CTX_STACK(R15), DI
	ADDQ	CX, DX                      // top = funcIdx + nres
ret0_nil_loop:
	MOVQ	$0, VAL_BITS(DI)
	MOVQ	$TAG_NIL, VAL_TAG(DI)
	ADDQ	$16, DI
	DECQ	CX
	JNE	ret0_nil_loop
ret0_no_fill:
	MOVQ	DX, TH_TOP(SI)
	CMPQ	BX, CTX_ENTRYDEPTH(R15)
	JLE	exit_done
	JMP	return_restore_caller

op_RETURN1:
	MOVL	R11, AX
	SHRL	$7, AX
	MOVBLZX	AL, AX
	SHLQ	$4, AX
	MOVOU	(R12)(AX*1), X0             // retVal
	MOVQ	CTX_CI(R15), AX
	IMULQ	$CF_SIZE, AX
	ADDQ	CTX_FRAMES(R15), AX         // cf
	MOVQ	CF_NRESULTS(AX), CX         // nres
	MOVQ	CF_FUNCIDX(AX), DX          // funcIdx
	MOVQ	CTX_TH(R15), SI
	MOVQ	TH_NFRAME(SI), BX
	DECQ	BX                          // newNFrame
	MOVQ	BX, TH_NFRAME(SI)
	TESTQ	CX, CX
	JE	ret1_set_top
	MOVQ	DX, DI
	SHLQ	$4, DI
	ADDQ	CTX_STACK(R15), DI
	MOVOU	X0, (DI)                    // stack[funcIdx] = retVal
	CMPQ	CX, $1
	JLE	ret1_one_res
	ADDQ	$16, DI
	ADDQ	CX, DX                      // top = funcIdx + nres
	DECQ	CX                          // nres - 1 (> 0)
ret1_nil_loop:
	MOVQ	$0, VAL_BITS(DI)
	MOVQ	$TAG_NIL, VAL_TAG(DI)
	ADDQ	$16, DI
	DECQ	CX
	JNE	ret1_nil_loop
	JMP	ret1_set_top
ret1_one_res:
	INCQ	DX                          // top = funcIdx + 1
ret1_set_top:
	MOVQ	DX, TH_TOP(SI)
	CMPQ	BX, CTX_ENTRYDEPTH(R15)
	JLE	exit_done

// Shared fast caller restore for OP_RETURN0 and OP_RETURN1.
// Input: BX = newNFrame (> entryDepth).
return_restore_caller:
	DECQ	BX                          // callerCI = newNFrame - 1
	IMUL3Q	$CF_SIZE, BX, AX
	ADDQ	CTX_FRAMES(R15), AX         // callerCF
	CMPB	CF_KIND(AX), $FRAME_LUA
	JNE	exit_reload
	CMPB	CF_FINISH(AX), $FINISH_NONE
	JNE	exit_reload
	MOVQ	BX, CTX_CI(R15)
	MOVQ	CF_BASE(AX), DX
	MOVQ	DX, CTX_BASE(R15)
	SHLQ	$4, DX
	MOVQ	CTX_STACK(R15), R12
	ADDQ	DX, R12                     // R12 = &stack[caller.base]
	MOVL	CF_CLREF(AX), SI
	RESOLVE_SMALL(SI, SI, CX)           // SI = caller *Closure
	MOVL	CL_UPVALSREF(SI), CX
	TESTL	CX, CX
	JE	ret_no_upvals
	RESOLVE_SMALL(CX, CX, DX)
	MOVQ	CX, CTX_CLUPVALS(R15)
	JMP	ret_check_bp
ret_no_upvals:
	MOVQ	$0, CTX_CLUPVALS(R15)
ret_check_bp:
	MOVL	CL_PROTOIDX(SI), CX
	MOVQ	CTX_PROTOS(R15), DX
	MOVQ	(DX)(CX*8), DI              // DI = caller *boundProto
	CMPQ	DI, CTX_CURBP(R15)
	JE	ret_same_bp
	MOVQ	DI, CTX_CURBP(R15)
	MOVQ	BP_INSNS(DI), R13
	MOVQ	BP_CONSTS(DI), R10
	MOVQ	BP_ICSLOTS(DI), CX
	MOVQ	BP_IC(DI), DX
	MOVQ	R13, CTX_INSNS(R15)
	MOVQ	R10, CTX_CONSTS(R15)
	MOVQ	CX, CTX_ICSLOTS(R15)
	MOVQ	DX, CTX_IC(R15)
	JMP	ret_set_pc
ret_same_bp:
	MOVQ	CTX_INSNS(R15), R13
ret_set_pc:
	MOVQ	CF_PC(AX), CX
	LEAQ	(R13)(CX*4), R13
	DISPATCH_NEXT

// ============================================================================
// Exit Paths
// ============================================================================

exit_cold:
	MOVQ	R13, AX
	SUBQ	CTX_INSNS(R15), AX
	SHRQ	$2, AX
	DECQ	AX                          // faulting instruction PC
	MOVQ	AX, CTX_PC(R15)
	MOVQ	R12, CTX_REGS(R15)
	MOVQ	R10, CTX_CONSTS(R15)
	MOVQ	$ASM_EXIT_COLD, ret+8(FP)
	RET

exit_poll:
	MOVQ	R13, AX
	SUBQ	CTX_INSNS(R15), AX
	SHRQ	$2, AX                      // next instruction PC
	MOVQ	AX, CTX_PC(R15)
	MOVQ	R12, CTX_REGS(R15)
	MOVQ	R10, CTX_CONSTS(R15)
	MOVQ	$ASM_EXIT_POLL, ret+8(FP)
	RET

exit_reload:
	MOVQ	$ASM_EXIT_RELOAD, ret+8(FP)
	RET

exit_done:
	MOVQ	$ASM_EXIT_DONE, ret+8(FP)
	RET
