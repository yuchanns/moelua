//go:build arm64 && !purego && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

#include "textflag.h"
#include "asm_offsets.h"

// Pinned registers across the direct-threaded ARM64 interpreter loop:
//   R16: bp.insns base pointer
//   R17: th.stack base pointer
//   R18: UNTOUCHED (platform-reserved on Darwin/Windows ARM64)
//   R19: *asmExecCtx
//   R20: current instruction pointer (points to NEXT instruction after fetch)
//   R21: current frame R[0] (&stack[base])
//   R22: bp.consts base pointer
//   R23: s.arena.pagePtrs base pointer
//   R24: inline branch jump table base (&jtable[0])
//   R25: current 32-bit bytecode instruction word
//   R26: countdown poll counter (yields to Go at <= 0)
//   R27: UNTOUCHED (Plan 9 assembler temp)
//   R28: UNTOUCHED (Go g)
//   R29: UNTOUCHED (FP)
//   R30: UNTOUCHED (LR)

// DISPATCH_NEXT fetches the next 32-bit instruction, advances R20, and jumps via a
// per-opcode indirect branch site so the CPU BTB predicts each opcode transition
// independently without switch-loop aliasing.
#define DISPATCH_NEXT \
	MOVWU.P	4(R20), R25; \
	AND	$0x7f, R25, R0; \
	ADD	R0<<2, R24, R1; \
	JMP	(R1)

// RESOLVE_SMALL resolves a non-null, non-large Arena ObjRef in REF into DST.
#define RESOLVE_SMALL(REF, DST, TMP) \
	LSR	$ARENA_WORDBITS, REF, TMP; \
	MOVD	(R23)(TMP<<3), DST; \
	AND	$ARENA_WORDMASK, REF, TMP; \
	ADD	TMP<<ARENA_ALIGNSHIFT, DST, DST

// RESOLVE_FAST resolves any live small-slab or large-span Arena ObjRef in REF into DST.
#define RESOLVE_FAST(REF, DST, TMP, L_LARGE, L_DONE) \
	TBNZ	$ARENA_LARGEBIT, REF, L_LARGE; \
	LSR	$ARENA_WORDBITS, REF, TMP; \
	MOVD	(R23)(TMP<<3), DST; \
	AND	$ARENA_WORDMASK, REF, TMP; \
	ADD	TMP<<ARENA_ALIGNSHIFT, DST, DST; \
	B	L_DONE; \
L_LARGE: \
	AND	$0x7fffffff, REF, TMP; \
	ADD	TMP<<1, TMP, TMP; \
	MOVD	CTX_LARGESPANS(R19), DST; \
	MOVD	(DST)(TMP<<3), DST; \
L_DONE:

// COND_DISPATCH finishes a comparison opcode whose boolean result (0 or 1) is in R0.
// If cond != w.K(), skips the following OP_JMP; otherwise executes its signed jump.
#define COND_DISPATCH(L_SKIP, L_BACK) \
	UBFX	$15, R25, $1, R1; \
	CMP	R1, R0; \
	BNE	L_SKIP; \
	MOVWU	(R20), R0; \
	LSR	$7, R0, R0; \
	MOVD	$OFFSET_SJ, R2; \
	SUBS	R2, R0, R0; \
	ADD	$1, R0, R1; \
	ADD	R1<<2, R20, R20; \
	BLT	L_BACK; \
	DISPATCH_NEXT; \
L_BACK: \
	SUBS	$1, R26, R26; \
	BLE	exit_poll; \
	DISPATCH_NEXT; \
L_SKIP: \
	ADD	$4, R20, R20; \
	DISPATCH_NEXT

// func runAsmExec(ctx *asmExecCtx) uint64
TEXT ·runAsmExec(SB), NOSPLIT, $0-16
	MOVD	ctx+0(FP), R19
	MOVD	CTX_REGS(R19), R21
	MOVD	CTX_INSNS(R19), R16
	MOVD	CTX_CONSTS(R19), R22
	MOVD	CTX_PAGEPTRS(R19), R23
	MOVD	CTX_STACK(R19), R17
	MOVD	CTX_PC(R19), R0
	ADD	R0<<2, R16, R20
	MOVD	CTX_POLL(R19), R26
	ADR	jtable, R24
	DISPATCH_NEXT

// 128-entry fixed-stride (4B) inline jump table covering every 7-bit opcode (0..127).
// Entries 85..127 pad to 128 so AND $0x7f guarantees bounds safety without a branch.
jtable:
	B	op_MOVE        // 0
	B	op_LOADI       // 1
	B	op_LOADF       // 2
	B	op_LOADK       // 3
	B	exit_cold      // 4  OP_LOADKX
	B	op_LOADFALSE   // 5
	B	op_LFALSESKIP  // 6
	B	op_LOADTRUE    // 7
	B	op_LOADNIL     // 8
	B	op_GETUPVAL    // 9
	B	op_SETUPVAL    // 10
	B	op_GETTABUP    // 11
	B	op_GETTABLE    // 12
	B	op_GETI        // 13
	B	op_GETFIELD    // 14
	B	op_SETTABUP    // 15
	B	op_SETTABLE    // 16
	B	op_SETI        // 17
	B	op_SETFIELD    // 18
	B	exit_cold      // 19 OP_NEWTABLE
	B	op_SELF        // 20
	B	op_ADDI        // 21
	B	op_ADDK        // 22
	B	op_SUBK        // 23
	B	op_MULK        // 24
	B	op_MODK        // 25
	B	exit_cold      // 26 OP_POWK
	B	op_DIVK        // 27
	B	op_IDIVK       // 28
	B	op_BANDK       // 29
	B	op_BORK        // 30
	B	op_BXORK       // 31
	B	op_SHLI        // 32
	B	op_SHRI        // 33
	B	op_ADD         // 34
	B	op_SUB         // 35
	B	op_MUL         // 36
	B	op_MOD         // 37
	B	exit_cold      // 38 OP_POW
	B	op_DIV         // 39
	B	op_IDIV        // 40
	B	op_BAND        // 41
	B	op_BOR         // 42
	B	op_BXOR        // 43
	B	op_SHL         // 44
	B	op_SHR         // 45
	B	exit_cold      // 46 OP_MMBIN
	B	exit_cold      // 47 OP_MMBINI
	B	exit_cold      // 48 OP_MMBINK
	B	op_UNM         // 49
	B	op_BNOT        // 50
	B	op_NOT         // 51
	B	op_LEN         // 52
	B	exit_cold      // 53 OP_CONCAT
	B	exit_cold      // 54 OP_CLOSE
	B	exit_cold      // 55 OP_TBC
	B	op_JMP         // 56
	B	op_EQ          // 57
	B	op_LT          // 58
	B	op_LE          // 59
	B	op_EQK         // 60
	B	op_EQI         // 61
	B	op_LTI         // 62
	B	op_LEI         // 63
	B	op_GTI         // 64
	B	op_GEI         // 65
	B	op_TEST        // 66
	B	op_TESTSET     // 67
	B	op_CALL        // 68
	B	op_TAILCALL    // 69
	B	exit_cold      // 70 OP_RETURN
	B	op_RETURN0     // 71
	B	op_RETURN1     // 72
	B	op_FORLOOP     // 73
	B	op_FORPREP     // 74
	B	exit_cold      // 75 OP_TFORPREP
	B	exit_cold      // 76 OP_TFORCALL
	B	op_TFORLOOP    // 77
	B	exit_cold      // 78 OP_SETLIST
	B	exit_cold      // 79 OP_CLOSURE
	B	exit_cold      // 80 OP_VARARG
	B	exit_cold      // 81 OP_GETVARG
	B	exit_cold      // 82 OP_ERRNNIL
	B	exit_cold      // 83 OP_VARARGPREP
	B	exit_cold      // 84 OP_EXTRAARG
	// Pad 85..127 (43 entries) to 128
	B	exit_cold      // 85
	B	exit_cold      // 86
	B	exit_cold      // 87
	B	exit_cold      // 88
	B	exit_cold      // 89
	B	exit_cold      // 90
	B	exit_cold      // 91
	B	exit_cold      // 92
	B	exit_cold      // 93
	B	exit_cold      // 94
	B	exit_cold      // 95
	B	exit_cold      // 96
	B	exit_cold      // 97
	B	exit_cold      // 98
	B	exit_cold      // 99
	B	exit_cold      // 100
	B	exit_cold      // 101
	B	exit_cold      // 102
	B	exit_cold      // 103
	B	exit_cold      // 104
	B	exit_cold      // 105
	B	exit_cold      // 106
	B	exit_cold      // 107
	B	exit_cold      // 108
	B	exit_cold      // 109
	B	exit_cold      // 110
	B	exit_cold      // 111
	B	exit_cold      // 112
	B	exit_cold      // 113
	B	exit_cold      // 114
	B	exit_cold      // 115
	B	exit_cold      // 116
	B	exit_cold      // 117
	B	exit_cold      // 118
	B	exit_cold      // 119
	B	exit_cold      // 120
	B	exit_cold      // 121
	B	exit_cold      // 122
	B	exit_cold      // 123
	B	exit_cold      // 124
	B	exit_cold      // 125
	B	exit_cold      // 126
	B	exit_cold      // 127

// ============================================================================
// Loads & Moves
// ============================================================================

op_MOVE:
	UBFX	$7, R25, $8, R2
	UBFX	$16, R25, $8, R3
	ADD	R3<<4, R21, R4
	LDP	(R4), (R5, R6)
	ADD	R2<<4, R21, R7
	STP	(R5, R6), (R7)
	DISPATCH_NEXT

op_LOADI:
	UBFX	$7, R25, $8, R2
	LSR	$15, R25, R3
	MOVD	$OFFSET_SBX, R4
	SUB	R4, R3, R3
	ADD	R2<<4, R21, R5
	STP	(R3, ZR), (R5)
	DISPATCH_NEXT

op_LOADF:
	UBFX	$7, R25, $8, R2
	LSR	$15, R25, R3
	MOVD	$OFFSET_SBX, R4
	SUB	R4, R3, R3
	SCVTFD	R3, F0
	ADD	R2<<4, R21, R5
	FMOVD	F0, (R5)
	MOVD	$TAG_FLOAT, R6
	MOVD	R6, VAL_TAG(R5)
	DISPATCH_NEXT

op_LOADK:
	UBFX	$7, R25, $8, R2
	LSR	$15, R25, R3
	ADD	R3<<4, R22, R4
	LDP	(R4), (R5, R6)
	ADD	R2<<4, R21, R7
	STP	(R5, R6), (R7)
	DISPATCH_NEXT

op_LOADFALSE:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R3
	MOVD	$TAG_FALSE, R4
	STP	(ZR, R4), (R3)
	DISPATCH_NEXT

op_LFALSESKIP:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R3
	MOVD	$TAG_FALSE, R4
	STP	(ZR, R4), (R3)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_LOADTRUE:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R3
	MOVD	$TAG_TRUE, R4
	STP	(ZR, R4), (R3)
	DISPATCH_NEXT

op_LOADNIL:
	UBFX	$7, R25, $8, R2
	UBFX	$16, R25, $8, R3
	ADD	R2<<4, R21, R4
	MOVD	$TAG_NIL, R5
loadnil_loop:
	STP	(ZR, R5), (R4)
	ADD	$16, R4, R4
	SUBS	$1, R3, R3
	BGE	loadnil_loop
	DISPATCH_NEXT

// ============================================================================
// Upvalues
// ============================================================================

op_GETUPVAL:
	UBFX	$16, R25, $8, R3
	MOVD	CTX_CLUPVALS(R19), R4
	MOVWU	(R4)(R3<<2), R5             // uvRef
	RESOLVE_SMALL(R5, R6, R7)           // R6 = *Upvalue
	MOVWU	UV_OWNERREF(R6), R8
	CBZ	R8, getupval_closed
	MOVWU	CTX_THSELF(R19), R9
	CMP	R9, R8
	BNE	exit_cold
	MOVW	UV_STACKIDX(R6), R10
	ADD	R10<<4, R17, R11
	LDP	(R11), (R2, R3)
	UBFX	$7, R25, $8, R4
	ADD	R4<<4, R21, R5
	STP	(R2, R3), (R5)
	DISPATCH_NEXT
getupval_closed:
	LDP	UV_CLOSED(R6), (R2, R3)
	UBFX	$7, R25, $8, R4
	ADD	R4<<4, R21, R5
	STP	(R2, R3), (R5)
	DISPATCH_NEXT

op_SETUPVAL:
	UBFX	$7, R25, $8, R2
	UBFX	$16, R25, $8, R3
	MOVD	CTX_CLUPVALS(R19), R4
	MOVWU	(R4)(R3<<2), R5             // uvRef
	RESOLVE_SMALL(R5, R6, R7)           // R6 = *Upvalue
	ADD	R2<<4, R21, R8
	LDP	(R8), (R9, R10)             // val = (R9=bits, R10=tag)
	MOVWU	UV_OWNERREF(R6), R11
	CBZ	R11, setupval_closed
	MOVWU	CTX_THSELF(R19), R12
	CMP	R12, R11
	BNE	exit_cold
	MOVW	UV_STACKIDX(R6), R13
	ADD	R13<<4, R17, R14
	STP	(R9, R10), (R14)
	DISPATCH_NEXT
setupval_closed:
	SUB	$TAG_FIRST_GC, R10, R11
	CMP	$(TAG_LAST_GC-TAG_FIRST_GC), R11
	BHI	setupval_closed_store
	MOVBU	UV_MARK(R6), R12
	TBNZ	$MARK_BLACK_BIT, R12, exit_cold
setupval_closed_store:
	STP	(R9, R10), UV_CLOSED(R6)
	DISPATCH_NEXT

// ============================================================================
// Table Upvalue / Field / Self / Array Ops
// ============================================================================

op_GETTABUP:
	UBFX	$16, R25, $8, R3
	MOVD	CTX_CLUPVALS(R19), R4
	MOVWU	(R4)(R3<<2), R5
	RESOLVE_SMALL(R5, R6, R7)           // R6 = *Upvalue
	MOVWU	UV_OWNERREF(R6), R8
	CBZ	R8, gettabup_uv_closed
	MOVWU	CTX_THSELF(R19), R9
	CMP	R9, R8
	BNE	exit_cold
	MOVW	UV_STACKIDX(R6), R10
	ADD	R10<<4, R17, R11
	LDP	(R11), (R2, R3)
	B	gettabup_have_tv
gettabup_uv_closed:
	LDP	UV_CLOSED(R6), (R2, R3)
gettabup_have_tv:
	CMP	$TAG_TABLE, R3
	BNE	exit_cold
	RESOLVE_SMALL(R2, R4, R7)           // R4 = *Table
	// Compute ic = &bp.ic[bp.icSlots[ipc]] using byte offset (R20 - 4 - R16) directly
	SUB	$4, R20, R5
	SUB	R16, R5, R5
	MOVD	CTX_ICSLOTS(R19), R6
	MOVW	(R6)(R5), R7                // icSlot (int32)
	ADD	R7<<1, R7, R7               // icSlot * 3
	MOVD	CTX_IC(R19), R8
	ADD	R7<<4, R8, R8               // R8 = *ICEntry (icSlot * 48)
	MOVBU	IC_KIND(R8), R9
	CMP	$IC_ENVSLOT, R9
	BNE	gettabup_check_own
	MOVWU	IC_HOLDERREF(R8), R10
	MOVWU	TBL_SELF(R4), R11
	CMP	R11, R10
	BNE	exit_cold
	MOVWU	TBL_HASHREF(R4), R12
	CBZ	R12, exit_cold
	RESOLVE_SMALL(R12, R13, R7)         // R13 = *tableHash
	MOVD	IC_EPOCH(R8), R14
	MOVD	THASH_DICTGEN(R13), R15
	CMP	R15, R14
	BNE	exit_cold
	MOVWU	THASH_NODESREF(R13), R10
	CBZ	R10, exit_cold
	RESOLVE_FAST(R10, R11, R7, gettabup_nodes_large, gettabup_nodes_ready)
	MOVW	IC_SLOT(R8), R12
	ADD	R12<<2, R12, R12            // slot * 5
	ADD	R12<<3, R11, R11            // &nodes[slot] (slot * 40)
	LDP	HNODE_VAL(R11), (R2, R3)
	CMP	$TAG_NIL, R3
	BNE	gettabup_store
	MOVBU	IC_NOMETA(R8), R13
	CBZ	R13, exit_cold
gettabup_store:
	UBFX	$7, R25, $8, R5
	ADD	R5<<4, R21, R6
	STP	(R2, R3), (R6)
	DISPATCH_NEXT
gettabup_check_own:
	CMP	$IC_OWNSLOT, R9
	BNE	exit_cold
	MOVWU	IC_SHAPEREF(R8), R10
	MOVWU	TBL_SHAPEREF(R4), R11
	CMP	R11, R10
	BNE	exit_cold
	MOVW	IC_SLOT(R8), R12
	MOVWU	TBL_SLOTSREF(R4), R13
	CBNZ	R13, gettabup_own_ext
	ADD	$TBL_INLINESLOTS, R4, R14
	B	gettabup_own_load
gettabup_own_ext:
	RESOLVE_SMALL(R13, R14, R7)
gettabup_own_load:
	ADD	R12<<4, R14, R14
	LDP	(R14), (R2, R3)
	CMP	$TAG_NIL, R3
	BNE	gettabup_store
	MOVBU	IC_NOMETA(R8), R13
	CBZ	R13, exit_cold
	B	gettabup_store

op_SETTABUP:
	UBFX	$7, R25, $8, R2             // upval idx A
	MOVD	CTX_CLUPVALS(R19), R4
	MOVWU	(R4)(R2<<2), R5
	RESOLVE_SMALL(R5, R6, R7)           // R6 = *Upvalue
	MOVWU	UV_OWNERREF(R6), R8
	CBZ	R8, settabup_uv_closed
	MOVWU	CTX_THSELF(R19), R9
	CMP	R9, R8
	BNE	exit_cold
	MOVW	UV_STACKIDX(R6), R10
	ADD	R10<<4, R17, R11
	LDP	(R11), (R2, R3)
	B	settabup_have_tv
settabup_uv_closed:
	LDP	UV_CLOSED(R6), (R2, R3)
settabup_have_tv:
	CMP	$TAG_TABLE, R3
	BNE	exit_cold
	RESOLVE_SMALL(R2, R4, R7)           // R4 = *Table
	MOVBU	TBL_FLAGS(R4), R5
	TBNZ	$TBL_FLAG_ISPROTO_BIT, R5, exit_cold
	// Load val = RK(C)
	UBFX	$24, R25, $8, R6
	TBNZ	$15, R25, settabup_val_k
	ADD	R6<<4, R21, R7
	LDP	(R7), (R9, R10)
	B	settabup_have_val
settabup_val_k:
	ADD	R6<<4, R22, R7
	LDP	(R7), (R9, R10)
settabup_have_val:
	CMP	$TAG_NIL, R10
	BEQ	exit_cold
	// Load ICEntry
	SUB	$4, R20, R5
	SUB	R16, R5, R5
	MOVD	CTX_ICSLOTS(R19), R6
	MOVW	(R6)(R5), R7
	ADD	R7<<1, R7, R7
	MOVD	CTX_IC(R19), R8
	ADD	R7<<4, R8, R8               // R8 = *ICEntry
	MOVBU	IC_KIND(R8), R5
	CMP	$IC_ENVSLOT, R5
	BNE	exit_cold
	MOVWU	IC_HOLDERREF(R8), R5
	MOVWU	TBL_SELF(R4), R6
	CMP	R6, R5
	BNE	exit_cold
	MOVWU	TBL_HASHREF(R4), R12
	CBZ	R12, exit_cold
	RESOLVE_SMALL(R12, R13, R7)         // R13 = *tableHash
	MOVD	IC_EPOCH(R8), R14
	MOVD	THASH_DICTGEN(R13), R15
	CMP	R15, R14
	BNE	exit_cold
	MOVWU	THASH_NODESREF(R13), R11
	CBZ	R11, exit_cold
	RESOLVE_FAST(R11, R12, R7, settabup_nodes_large, settabup_nodes_ready)
	MOVW	IC_SLOT(R8), R13
	ADD	R13<<2, R13, R13
	ADD	R13<<3, R12, R12            // &nodes[slot]
	MOVD	(HNODE_VAL+VAL_TAG)(R12), R14
	CMP	$TAG_NIL, R14
	BEQ	exit_cold
	// GC barrier check before mutation
	SUB	$TAG_FIRST_GC, R10, R14
	CMP	$(TAG_LAST_GC-TAG_FIRST_GC), R14
	BHI	settabup_do_store
	MOVBU	TBL_MARK(R4), R15
	TBNZ	$MARK_BLACK_BIT, R15, exit_cold
settabup_do_store:
	STP	(R9, R10), HNODE_VAL(R12)
	MOVB	ZR, TBL_TMFLAGS(R4)
	DISPATCH_NEXT

op_GETFIELD:
	UBFX	$16, R25, $8, R3
	ADD	R3<<4, R21, R4
	LDP	(R4), (R2, R3)              // rb = (R2, R3)
	CMP	$TAG_TABLE, R3
	BNE	exit_cold
	RESOLVE_SMALL(R2, R4, R7)           // R4 = *Table
	// Load ICEntry
	SUB	$4, R20, R5
	SUB	R16, R5, R5
	MOVD	CTX_ICSLOTS(R19), R6
	MOVW	(R6)(R5), R7
	ADD	R7<<1, R7, R7
	MOVD	CTX_IC(R19), R8
	ADD	R7<<4, R8, R8               // R8 = *ICEntry
	MOVWU	TBL_SHAPEREF(R4), R9        // t.shapeRef
	MOVWU	IC_SHAPEREF(R8), R10
	CMP	R10, R9
	BNE	getfield_poly
	MOVBU	IC_KIND(R8), R11
	CMP	$IC_OWNSLOT, R11
	BNE	getfield_proto
	MOVW	IC_SLOT(R8), R12
	MOVWU	TBL_SLOTSREF(R4), R13
	CBNZ	R13, getfield_own_ext
	ADD	$TBL_INLINESLOTS, R4, R14
	B	getfield_own_load
getfield_own_ext:
	RESOLVE_SMALL(R13, R14, R7)
getfield_own_load:
	ADD	R12<<4, R14, R14
	LDP	(R14), (R2, R3)
	CMP	$TAG_NIL, R3
	BNE	getfield_store
	MOVBU	IC_NOMETA(R8), R15
	CBNZ	R15, getfield_store
	MOVBU	IC_PROTOABSENT(R8), R15
	CBZ	R15, exit_cold
	MOVD	IC_EPOCH(R8), R12
	MOVD	CTX_PROTOEPOCH(R19), R13
	CMP	R13, R12
	BNE	exit_cold
getfield_store:
	UBFX	$7, R25, $8, R5
	ADD	R5<<4, R21, R6
	STP	(R2, R3), (R6)
	DISPATCH_NEXT
getfield_proto:
	CMP	$IC_PROTOVAL, R11
	BNE	getfield_poly
	MOVBU	IC_NOMETA(R8), R15
	CBNZ	R15, getfield_proto_hit
	MOVD	IC_EPOCH(R8), R12
	MOVD	CTX_PROTOEPOCH(R19), R13
	CMP	R13, R12
	BNE	getfield_poly
getfield_proto_hit:
	LDP	IC_VAL(R8), (R2, R3)
	UBFX	$7, R25, $8, R5
	ADD	R5<<4, R21, R6
	STP	(R2, R3), (R6)
	DISPATCH_NEXT
getfield_poly:
	MOVWU	IC_POLYREF(R8), R10
	CBZ	R10, exit_cold
	MOVBU	IC_POLYCOUNT(R8), R11
	CBZ	R11, exit_cold
	RESOLVE_SMALL(R10, R12, R7)         // R12 = *polyEntry
getfield_poly_loop:
	MOVWU	POLY_SHAPEREF(R12), R13
	CMP	R13, R9
	BEQ	getfield_poly_match
	ADD	$POLY_SIZE, R12, R12
	SUBS	$1, R11, R11
	BNE	getfield_poly_loop
	B	exit_cold
getfield_poly_match:
	MOVBU	POLY_KIND(R12), R14
	CMP	$IC_OWNSLOT, R14
	BNE	getfield_poly_proto
	MOVW	POLY_SLOT(R12), R13
	MOVWU	TBL_SLOTSREF(R4), R15
	CBNZ	R15, getfield_poly_ext
	ADD	$TBL_INLINESLOTS, R4, R14
	B	getfield_poly_load
getfield_poly_ext:
	RESOLVE_SMALL(R15, R14, R7)
getfield_poly_load:
	ADD	R13<<4, R14, R14
	LDP	(R14), (R2, R3)
	CMP	$TAG_NIL, R3
	BNE	getfield_store
	MOVBU	POLY_NOMETA(R12), R15
	CBNZ	R15, getfield_store
	MOVBU	POLY_PROTOABSENT(R12), R15
	CBZ	R15, exit_cold
	MOVD	POLY_EPOCH(R12), R13
	MOVD	CTX_PROTOEPOCH(R19), R14
	CMP	R14, R13
	BNE	exit_cold
	B	getfield_store
getfield_poly_proto:
	CMP	$IC_PROTOVAL, R14
	BNE	exit_cold
	MOVBU	POLY_NOMETA(R12), R15
	CBNZ	R15, getfield_poly_proto_hit
	MOVD	POLY_EPOCH(R12), R13
	MOVD	CTX_PROTOEPOCH(R19), R14
	CMP	R14, R13
	BNE	exit_cold
getfield_poly_proto_hit:
	LDP	POLY_VAL(R12), (R2, R3)
	B	getfield_store

op_SELF:
	UBFX	$7, R25, $8, R5             // A
	UBFX	$16, R25, $8, R3            // B
	ADD	R3<<4, R21, R4
	LDP	(R4), (R2, R3)              // rb = (R2, R3)
	ADD	R5<<4, R21, R6              // &regs[A]
	STP	(R2, R3), VAL_SIZE(R6)      // regs[A+1] = rb
	CMP	$TAG_TABLE, R3
	BNE	exit_cold
	RESOLVE_SMALL(R2, R4, R7)           // R4 = *Table
	// Load ICEntry
	SUB	$4, R20, R1
	SUB	R16, R1, R1
	MOVD	CTX_ICSLOTS(R19), R7
	MOVW	(R7)(R1), R7
	ADD	R7<<1, R7, R7
	MOVD	CTX_IC(R19), R8
	ADD	R7<<4, R8, R8               // R8 = *ICEntry
	MOVWU	TBL_SHAPEREF(R4), R9
	MOVWU	IC_SHAPEREF(R8), R10
	CMP	R10, R9
	BNE	self_poly
	MOVBU	IC_KIND(R8), R11
	CMP	$IC_PROTOVAL, R11           // method calls hit icProtoVal most often!
	BNE	self_own
	MOVBU	IC_NOMETA(R8), R15
	CBNZ	R15, self_proto_hit
	MOVD	IC_EPOCH(R8), R12
	MOVD	CTX_PROTOEPOCH(R19), R13
	CMP	R13, R12
	BNE	self_poly
self_proto_hit:
	LDP	IC_VAL(R8), (R2, R3)
	STP	(R2, R3), (R6)
	DISPATCH_NEXT
self_own:
	CMP	$IC_OWNSLOT, R11
	BNE	self_poly
	MOVW	IC_SLOT(R8), R12
	MOVWU	TBL_SLOTSREF(R4), R13
	CBNZ	R13, self_own_ext
	ADD	$TBL_INLINESLOTS, R4, R14
	B	self_own_load
self_own_ext:
	RESOLVE_SMALL(R13, R14, R7)
self_own_load:
	ADD	R12<<4, R14, R14
	LDP	(R14), (R2, R3)
	CMP	$TAG_NIL, R3
	BNE	self_store
	MOVBU	IC_NOMETA(R8), R15
	CBNZ	R15, self_store
	MOVBU	IC_PROTOABSENT(R8), R15
	CBZ	R15, exit_cold
	MOVD	IC_EPOCH(R8), R12
	MOVD	CTX_PROTOEPOCH(R19), R13
	CMP	R13, R12
	BNE	exit_cold
self_store:
	STP	(R2, R3), (R6)
	DISPATCH_NEXT
self_poly:
	MOVWU	IC_POLYREF(R8), R10
	CBZ	R10, exit_cold
	MOVBU	IC_POLYCOUNT(R8), R11
	CBZ	R11, exit_cold
	RESOLVE_SMALL(R10, R12, R7)
self_poly_loop:
	MOVWU	POLY_SHAPEREF(R12), R13
	CMP	R13, R9
	BEQ	self_poly_match
	ADD	$POLY_SIZE, R12, R12
	SUBS	$1, R11, R11
	BNE	self_poly_loop
	B	exit_cold
self_poly_match:
	MOVBU	POLY_KIND(R12), R14
	CMP	$IC_PROTOVAL, R14
	BNE	self_poly_own
	MOVBU	POLY_NOMETA(R12), R15
	CBNZ	R15, self_poly_proto_hit
	MOVD	POLY_EPOCH(R12), R13
	MOVD	CTX_PROTOEPOCH(R19), R14
	CMP	R14, R13
	BNE	exit_cold
self_poly_proto_hit:
	LDP	POLY_VAL(R12), (R2, R3)
	STP	(R2, R3), (R6)
	DISPATCH_NEXT
self_poly_own:
	CMP	$IC_OWNSLOT, R14
	BNE	exit_cold
	MOVW	POLY_SLOT(R12), R13
	MOVWU	TBL_SLOTSREF(R4), R15
	CBNZ	R15, self_poly_ext
	ADD	$TBL_INLINESLOTS, R4, R14
	B	self_poly_load
self_poly_ext:
	RESOLVE_SMALL(R15, R14, R7)
self_poly_load:
	ADD	R13<<4, R14, R14
	LDP	(R14), (R2, R3)
	CMP	$TAG_NIL, R3
	BNE	self_store
	MOVBU	POLY_NOMETA(R12), R15
	CBNZ	R15, self_store
	MOVBU	POLY_PROTOABSENT(R12), R15
	CBZ	R15, exit_cold
	MOVD	POLY_EPOCH(R12), R13
	MOVD	CTX_PROTOEPOCH(R19), R14
	CMP	R14, R13
	BNE	exit_cold
	B	self_store

op_SETFIELD:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R3
	LDP	(R3), (R2, R3)              // ra = (R2, R3)
	CMP	$TAG_TABLE, R3
	BNE	exit_cold
	RESOLVE_SMALL(R2, R4, R7)           // R4 = *Table
	MOVBU	TBL_FLAGS(R4), R5
	TBNZ	$TBL_FLAG_ISPROTO_BIT, R5, exit_cold
	// Load val = RK(C) into (R2, R3)
	UBFX	$24, R25, $8, R6
	TBNZ	$15, R25, setfield_val_k
	ADD	R6<<4, R21, R7
	LDP	(R7), (R2, R3)
	B	setfield_have_val
setfield_val_k:
	ADD	R6<<4, R22, R7
	LDP	(R7), (R2, R3)
setfield_have_val:
	// Load ICEntry
	SUB	$4, R20, R5
	SUB	R16, R5, R5
	MOVD	CTX_ICSLOTS(R19), R6
	MOVW	(R6)(R5), R7
	ADD	R7<<1, R7, R7
	MOVD	CTX_IC(R19), R8
	ADD	R7<<4, R8, R8               // R8 = *ICEntry
	MOVWU	TBL_SHAPEREF(R4), R9
	MOVWU	IC_SHAPEREF(R8), R10
	CMP	R10, R9
	BNE	setfield_poly
	MOVBU	IC_KIND(R8), R11
	CMP	$IC_OWNSLOT, R11
	BNE	setfield_addslot
	MOVW	IC_SLOT(R8), R12
	MOVWU	TBL_SLOTSREF(R4), R13
	CBNZ	R13, setfield_own_ext
	ADD	$TBL_INLINESLOTS, R4, R14
	B	setfield_own_ptr
setfield_own_ext:
	RESOLVE_SMALL(R13, R14, R7)
setfield_own_ptr:
	ADD	R12<<4, R14, R14            // R14 = sp
	MOVD	VAL_TAG(R14), R15
	CMP	$TAG_NIL, R15
	BNE	setfield_own_barrier
	MOVBU	IC_NOMETA(R8), R15
	CBNZ	R15, setfield_own_barrier
	MOVBU	IC_PROTOABSENT(R8), R15
	CBZ	R15, exit_cold
	MOVD	IC_EPOCH(R8), R12
	MOVD	CTX_PROTOEPOCH(R19), R13
	CMP	R13, R12
	BNE	exit_cold
setfield_own_barrier:
	SUB	$TAG_FIRST_GC, R3, R15
	CMP	$(TAG_LAST_GC-TAG_FIRST_GC), R15
	BHI	setfield_own_store
	MOVBU	TBL_MARK(R4), R15
	TBNZ	$MARK_BLACK_BIT, R15, exit_cold
setfield_own_store:
	STP	(R2, R3), (R14)
	DISPATCH_NEXT
setfield_addslot:
	CMP	$IC_ADDSLOT, R11
	BNE	exit_cold
	CMP	$TAG_NIL, R3
	BEQ	exit_cold
	MOVBU	IC_NOMETA(R8), R15
	CBNZ	R15, setfield_add_ok_epoch
	MOVD	IC_EPOCH(R8), R12
	MOVD	CTX_PROTOEPOCH(R19), R13
	CMP	R13, R12
	BNE	exit_cold
setfield_add_ok_epoch:
	MOVWU	IC_TARGETSHAPE(R8), R10
	CBZ	R10, exit_cold
	MOVW	IC_SLOT(R8), R12
	MOVBU	TBL_SLOTSCAP(R4), R13
	CMP	R13, R12
	BGE	exit_cold
	MOVBU	TBL_MARK(R4), R15
	TBNZ	$MARK_BLACK_BIT, R15, exit_cold
	MOVWU	TBL_SLOTSREF(R4), R13
	CBNZ	R13, setfield_add_ext
	ADD	$TBL_INLINESLOTS, R4, R14
	B	setfield_add_store
setfield_add_ext:
	RESOLVE_SMALL(R13, R14, R7)
setfield_add_store:
	ADD	R12<<4, R14, R14
	STP	(R2, R3), (R14)
	ADD	$1, R12, R12
	MOVB	R12, TBL_SLOTSLEN(R4)
	MOVW	R10, TBL_SHAPEREF(R4)
	DISPATCH_NEXT
setfield_poly:
	MOVWU	IC_POLYREF(R8), R10
	CBZ	R10, exit_cold
	MOVBU	IC_POLYCOUNT(R8), R11
	CBZ	R11, exit_cold
	RESOLVE_SMALL(R10, R12, R7)         // R12 = *polyEntry
setfield_poly_loop:
	MOVWU	POLY_SHAPEREF(R12), R13
	CMP	R13, R9
	BEQ	setfield_poly_match
	ADD	$POLY_SIZE, R12, R12
	SUBS	$1, R11, R11
	BNE	setfield_poly_loop
	B	exit_cold
setfield_poly_match:
	MOVBU	POLY_KIND(R12), R14
	CMP	$IC_OWNSLOT, R14
	BNE	setfield_poly_add
	MOVW	POLY_SLOT(R12), R13
	MOVWU	TBL_SLOTSREF(R4), R15
	CBNZ	R15, setfield_poly_own_ext
	ADD	$TBL_INLINESLOTS, R4, R14
	B	setfield_poly_own_ptr
setfield_poly_own_ext:
	RESOLVE_SMALL(R15, R14, R7)
setfield_poly_own_ptr:
	ADD	R13<<4, R14, R14
	MOVD	VAL_TAG(R14), R15
	CMP	$TAG_NIL, R15
	BNE	setfield_own_barrier
	MOVBU	POLY_NOMETA(R12), R15
	CBNZ	R15, setfield_own_barrier
	MOVBU	POLY_PROTOABSENT(R12), R15
	CBZ	R15, exit_cold
	MOVD	POLY_EPOCH(R12), R13
	MOVD	CTX_PROTOEPOCH(R19), R15
	CMP	R15, R13
	BNE	exit_cold
	B	setfield_own_barrier
setfield_poly_add:
	CMP	$IC_ADDSLOT, R14
	BNE	exit_cold
	CMP	$TAG_NIL, R3
	BEQ	exit_cold
	MOVBU	POLY_NOMETA(R12), R15
	CBNZ	R15, setfield_poly_add_epoch
	MOVD	POLY_EPOCH(R12), R13
	MOVD	CTX_PROTOEPOCH(R19), R15
	CMP	R15, R13
	BNE	exit_cold
setfield_poly_add_epoch:
	MOVWU	POLY_TARGETSHAPE(R12), R10
	CBZ	R10, exit_cold
	MOVW	POLY_SLOT(R12), R12
	MOVBU	TBL_SLOTSCAP(R4), R13
	CMP	R13, R12
	BGE	exit_cold
	MOVBU	TBL_MARK(R4), R15
	TBNZ	$MARK_BLACK_BIT, R15, exit_cold
	MOVWU	TBL_SLOTSREF(R4), R13
	CBNZ	R13, setfield_poly_add_ext
	ADD	$TBL_INLINESLOTS, R4, R14
	B	setfield_add_store
setfield_poly_add_ext:
	RESOLVE_SMALL(R13, R14, R7)
	B	setfield_add_store

op_GETI:
	UBFX	$16, R25, $8, R3
	ADD	R3<<4, R21, R4
	LDP	(R4), (R2, R3)              // rb
	CMP	$TAG_TABLE, R3
	BNE	exit_cold
	RESOLVE_SMALL(R2, R4, R7)           // R4 = *Table
	UBFX	$24, R25, $8, R5            // C (0..255)
	SUB	$1, R5, R5                  // idx = uint64(C - 1)
	MOVWU	TBL_ARRLEN(R4), R6
	CMP	R6, R5
	BHS	exit_cold
	MOVWU	TBL_ARRREF(R4), R8
	RESOLVE_FAST(R8, R9, R7, geti_large, geti_arr_ready)
	MOVBU	TBL_ARRKIND(R4), R10
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R11             // &regs[A]
	CMP	$ARR_INT, R10
	BNE	geti_not_int
	MOVD	(R9)(R5<<3), R12
	STP	(R12, ZR), (R11)
	DISPATCH_NEXT
geti_not_int:
	CMP	$ARR_FLOAT, R10
	BNE	geti_not_float
	MOVD	(R9)(R5<<3), R12
	MOVD	$TAG_FLOAT, R13
	STP	(R12, R13), (R11)
	DISPATCH_NEXT
geti_not_float:
	CMP	$ARR_GENERIC, R10
	BNE	geti_bool
	ADD	R5<<4, R9, R9
	LDP	(R9), (R12, R13)
	CMP	$TAG_NIL, R13
	BNE	geti_gen_store
	MOVWU	TBL_METAREF(R4), R14
	CBNZ	R14, exit_cold
geti_gen_store:
	STP	(R12, R13), (R11)
	DISPATCH_NEXT
geti_bool:
	CMP	$ARR_BOOL, R10
	BNE	exit_cold
	MOVBU	(R9)(R5), R12
	ADD	$TAG_FALSE, R12, R12
	STP	(ZR, R12), (R11)
	DISPATCH_NEXT

op_GETTABLE:
	UBFX	$16, R25, $8, R3            // B
	UBFX	$24, R25, $8, R4            // C
	ADD	R3<<4, R21, R5
	LDP	(R5), (R2, R3)              // rb
	ADD	R4<<4, R21, R6
	LDP	(R6), (R5, R6)              // rc
	CMP	$TAG_TABLE, R3
	BNE	exit_cold
	CBNZ	R6, exit_cold               // rc must be TAG_INT (0)
	RESOLVE_SMALL(R2, R4, R7)           // R4 = *Table
	SUB	$1, R5, R5                  // idx = uint64(rc.Int() - 1)
	MOVWU	TBL_ARRLEN(R4), R6
	CMP	R6, R5
	BHS	exit_cold
	MOVWU	TBL_ARRREF(R4), R8
	RESOLVE_FAST(R8, R9, R7, gettab_large, gettab_arr_ready)
	MOVBU	TBL_ARRKIND(R4), R10
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R11             // &regs[A]
	CMP	$ARR_INT, R10
	BNE	gettab_not_int
	MOVD	(R9)(R5<<3), R12
	STP	(R12, ZR), (R11)
	DISPATCH_NEXT
gettab_not_int:
	CMP	$ARR_FLOAT, R10
	BNE	gettab_not_float
	MOVD	(R9)(R5<<3), R12
	MOVD	$TAG_FLOAT, R13
	STP	(R12, R13), (R11)
	DISPATCH_NEXT
gettab_not_float:
	CMP	$ARR_BOOL, R10
	BNE	gettab_generic
	MOVBU	(R9)(R5), R12
	ADD	$TAG_FALSE, R12, R12
	STP	(ZR, R12), (R11)
	DISPATCH_NEXT
gettab_generic:
	CMP	$ARR_GENERIC, R10
	BNE	exit_cold
	ADD	R5<<4, R9, R9
	LDP	(R9), (R12, R13)
	CMP	$TAG_NIL, R13
	BNE	gettab_gen_store
	MOVWU	TBL_METAREF(R4), R14
	CBNZ	R14, exit_cold
gettab_gen_store:
	STP	(R12, R13), (R11)
	DISPATCH_NEXT

op_SETI:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R3
	LDP	(R3), (R2, R3)              // ra
	CMP	$TAG_TABLE, R3
	BNE	exit_cold
	RESOLVE_SMALL(R2, R4, R7)           // R4 = *Table
	MOVBU	TBL_FLAGS(R4), R5
	TBNZ	$TBL_FLAG_ISPROTO_BIT, R5, exit_cold
	UBFX	$16, R25, $8, R5            // B
	SUB	$1, R5, R5                  // idx = uint64(B - 1)
	MOVWU	TBL_ARRLEN(R4), R6
	CMP	R6, R5
	BHS	exit_cold
	// val = RK(C) -> (R2, R3)
	UBFX	$24, R25, $8, R6
	TBNZ	$15, R25, seti_val_k
	ADD	R6<<4, R21, R7
	LDP	(R7), (R2, R3)
	B	seti_have_val
seti_val_k:
	ADD	R6<<4, R22, R7
	LDP	(R7), (R2, R3)
seti_have_val:
	MOVWU	TBL_ARRREF(R4), R8
	RESOLVE_FAST(R8, R9, R7, seti_large, seti_arr_ready)
	MOVBU	TBL_ARRKIND(R4), R10
	CMP	$ARR_INT, R10
	BNE	seti_not_int
	CBNZ	R3, exit_cold
	MOVD	R2, (R9)(R5<<3)
	DISPATCH_NEXT
seti_not_int:
	CMP	$ARR_FLOAT, R10
	BNE	seti_not_float
	CMP	$TAG_FLOAT, R3
	BNE	exit_cold
	MOVD	R2, (R9)(R5<<3)
	DISPATCH_NEXT
seti_not_float:
	CMP	$ARR_GENERIC, R10
	BNE	seti_bool
	ADD	R5<<4, R9, R9
	MOVD	VAL_TAG(R9), R11
	CMP	$TAG_NIL, R11
	BNE	seti_gen_barrier
	MOVWU	TBL_METAREF(R4), R12
	CBNZ	R12, exit_cold
seti_gen_barrier:
	SUB	$TAG_FIRST_GC, R3, R13
	CMP	$(TAG_LAST_GC-TAG_FIRST_GC), R13
	BHI	seti_gen_store
	MOVBU	TBL_MARK(R4), R14
	TBNZ	$MARK_BLACK_BIT, R14, exit_cold
seti_gen_store:
	STP	(R2, R3), (R9)
	DISPATCH_NEXT
seti_bool:
	CMP	$ARR_BOOL, R10
	BNE	exit_cold
	SUB	$TAG_FALSE, R3, R11
	CMP	$1, R11
	BHI	exit_cold
	MOVB	R11, (R9)(R5)
	DISPATCH_NEXT

op_SETTABLE:
	UBFX	$7, R25, $8, R2             // A
	UBFX	$16, R25, $8, R3            // B
	ADD	R2<<4, R21, R4
	LDP	(R4), (R2, R4)              // ra = (R2, R4)
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)              // rb = (R5, R6)
	CMP	$TAG_TABLE, R4
	BNE	exit_cold
	CBNZ	R6, exit_cold               // rb must be TAG_INT (0)
	RESOLVE_SMALL(R2, R4, R7)           // R4 = *Table
	MOVBU	TBL_FLAGS(R4), R7
	TBNZ	$TBL_FLAG_ISPROTO_BIT, R7, exit_cold
	// val = RK(C) -> (R2, R3)
	UBFX	$24, R25, $8, R6
	TBNZ	$15, R25, settab_val_k
	ADD	R6<<4, R21, R7
	LDP	(R7), (R2, R3)
	B	settab_have_val
settab_val_k:
	ADD	R6<<4, R22, R7
	LDP	(R7), (R2, R3)
settab_have_val:
	SUB	$1, R5, R5                  // idx = uint64(rb.Int() - 1)
	MOVWU	TBL_ARRLEN(R4), R6          // n = t.arrLen
	CMP	R6, R5
	BHS	settab_maybe_append         // idx >= n
	// In-bounds write (idx < n):
	MOVWU	TBL_ARRREF(R4), R8
	RESOLVE_FAST(R8, R9, R7, settab_large, settab_arr_ready)
	MOVBU	TBL_ARRKIND(R4), R10
	CMP	$ARR_INT, R10
	BNE	settab_not_int
	CBNZ	R3, exit_cold
	MOVD	R2, (R9)(R5<<3)
	DISPATCH_NEXT
settab_not_int:
	CMP	$ARR_FLOAT, R10
	BNE	settab_not_float
	CMP	$TAG_FLOAT, R3
	BNE	exit_cold
	MOVD	R2, (R9)(R5<<3)
	DISPATCH_NEXT
settab_not_float:
	CMP	$ARR_BOOL, R10
	BNE	settab_generic
	SUB	$TAG_FALSE, R3, R11
	CMP	$1, R11
	BHI	exit_cold
	MOVB	R11, (R9)(R5)
	DISPATCH_NEXT
settab_generic:
	CMP	$ARR_GENERIC, R10
	BNE	exit_cold
	ADD	R5<<4, R9, R9
	MOVD	VAL_TAG(R9), R11
	CMP	$TAG_NIL, R11
	BNE	settab_gen_barrier
	MOVWU	TBL_METAREF(R4), R12
	CBNZ	R12, exit_cold
settab_gen_barrier:
	SUB	$TAG_FIRST_GC, R3, R13
	CMP	$(TAG_LAST_GC-TAG_FIRST_GC), R13
	BHI	settab_gen_store
	MOVBU	TBL_MARK(R4), R14
	TBNZ	$MARK_BLACK_BIT, R14, exit_cold
settab_gen_store:
	STP	(R2, R3), (R9)
	DISPATCH_NEXT
settab_maybe_append:
	// canAppend: idx == n && n < t.arrCap && !t.hasHash() && !t.hasMeta()
	BNE	exit_cold                   // idx != n
	MOVWU	TBL_ARRCAP(R4), R11
	CMP	R11, R6
	BHS	exit_cold                   // n >= arrCap
	MOVWU	TBL_HASHREF(R4), R11
	CBNZ	R11, exit_cold
	MOVWU	TBL_METAREF(R4), R11
	CBNZ	R11, exit_cold
	MOVWU	TBL_ARRREF(R4), R8
	RESOLVE_FAST(R8, R9, R7, settab_app_large, settab_app_ready)
	MOVBU	TBL_ARRKIND(R4), R10
	ADD	$1, R6, R11                 // n + 1
	CMP	$ARR_INT, R10
	BNE	settab_app_not_int
	CBNZ	R3, exit_cold
	MOVW	R11, TBL_ARRLEN(R4)
	MOVD	R2, (R9)(R5<<3)
	DISPATCH_NEXT
settab_app_not_int:
	CMP	$ARR_FLOAT, R10
	BNE	settab_app_not_float
	CMP	$TAG_FLOAT, R3
	BNE	exit_cold
	MOVW	R11, TBL_ARRLEN(R4)
	MOVD	R2, (R9)(R5<<3)
	DISPATCH_NEXT
settab_app_not_float:
	CMP	$ARR_BOOL, R10
	BNE	settab_app_generic
	SUB	$TAG_FALSE, R3, R12
	CMP	$1, R12
	BHI	exit_cold
	MOVW	R11, TBL_ARRLEN(R4)
	MOVB	R12, (R9)(R5)
	DISPATCH_NEXT
settab_app_generic:
	CMP	$ARR_GENERIC, R10
	BNE	exit_cold
	CMP	$TAG_NIL, R3
	BEQ	exit_cold
	SUB	$TAG_FIRST_GC, R3, R13
	CMP	$(TAG_LAST_GC-TAG_FIRST_GC), R13
	BHI	settab_app_gen_store
	MOVBU	TBL_MARK(R4), R14
	TBNZ	$MARK_BLACK_BIT, R14, exit_cold
settab_app_gen_store:
	MOVW	R11, TBL_ARRLEN(R4)
	ADD	R5<<4, R9, R9
	STP	(R2, R3), (R9)
	DISPATCH_NEXT

// ============================================================================
// Arithmetic & Bitwise Ops (skip following OP_MMBIN* on success via ADD $4, R20)
// ============================================================================

op_ADDI:
	UBFX	$16, R25, $8, R3            // B
	ADD	R3<<4, R21, R4
	LDP	(R4), (R5, R6)              // rb = (R5, R6)
	UBFX	$24, R25, $8, R7            // C (0..255)
	SUB	$OFFSET_SC, R7, R7          // sC
	CBNZ	R6, addi_not_int
	ADD	R7, R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R8
	STP	(R5, ZR), (R8)
	ADD	$4, R20, R20                // skip OP_MMBINI
	DISPATCH_NEXT
addi_not_int:
	CMP	$TAG_FLOAT, R6
	BNE	exit_cold
	FMOVD	R5, F0
	SCVTFD	R7, F1
	FADDD	F1, F0, F0
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R8
	FMOVD	F0, (R8)
	MOVD	$TAG_FLOAT, R9
	MOVD	R9, VAL_TAG(R8)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_ADDK:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)              // rb
	ADD	R4<<4, R22, R7
	LDP	(R7), (R7, R8)              // kc
	ORR	R6, R8, R9
	CBNZ	R9, addk_not_both_int
	ADD	R7, R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	STP	(R5, ZR), (R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT
addk_not_both_int:
	CMP	$1, R9
	BHI	exit_cold
	CBZ	R6, addk_rb_int
	FMOVD	R5, F0
	B	addk_kc_conv
addk_rb_int:
	SCVTFD	R5, F0
addk_kc_conv:
	CBZ	R8, addk_kc_int
	FMOVD	R7, F1
	B	addk_fdo
addk_kc_int:
	SCVTFD	R7, F1
addk_fdo:
	FADDD	F1, F0, F0
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	FMOVD	F0, (R10)
	MOVD	$TAG_FLOAT, R11
	MOVD	R11, VAL_TAG(R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_SUBK:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R22, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, subk_not_both_int
	SUB	R7, R5, R5                  // rb - kc
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	STP	(R5, ZR), (R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT
subk_not_both_int:
	CMP	$1, R9
	BHI	exit_cold
	CBZ	R6, subk_rb_int
	FMOVD	R5, F0
	B	subk_kc_conv
subk_rb_int:
	SCVTFD	R5, F0
subk_kc_conv:
	CBZ	R8, subk_kc_int
	FMOVD	R7, F1
	B	subk_fdo
subk_kc_int:
	SCVTFD	R7, F1
subk_fdo:
	FSUBD	F1, F0, F0                  // F0 - F1
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	FMOVD	F0, (R10)
	MOVD	$TAG_FLOAT, R11
	MOVD	R11, VAL_TAG(R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_MULK:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R22, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, mulk_not_both_int
	MUL	R7, R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	STP	(R5, ZR), (R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT
mulk_not_both_int:
	CMP	$1, R9
	BHI	exit_cold
	CBZ	R6, mulk_rb_int
	FMOVD	R5, F0
	B	mulk_kc_conv
mulk_rb_int:
	SCVTFD	R5, F0
mulk_kc_conv:
	CBZ	R8, mulk_kc_int
	FMOVD	R7, F1
	B	mulk_fdo
mulk_kc_int:
	SCVTFD	R7, F1
mulk_fdo:
	FMULD	F1, F0, F0
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	FMOVD	F0, (R10)
	MOVD	$TAG_FLOAT, R11
	MOVD	R11, VAL_TAG(R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_DIVK:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R22, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CMP	$1, R9
	BHI	exit_cold
	CBZ	R6, divk_rb_int
	FMOVD	R5, F0
	B	divk_kc_conv
divk_rb_int:
	SCVTFD	R5, F0
divk_kc_conv:
	CBZ	R8, divk_kc_int
	FMOVD	R7, F1
	B	divk_fdo
divk_kc_int:
	SCVTFD	R7, F1
divk_fdo:
	FDIVD	F1, F0, F0                  // F0 / F1
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	FMOVD	F0, (R10)
	MOVD	$TAG_FLOAT, R11
	MOVD	R11, VAL_TAG(R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_MODK:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R22, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, exit_cold
	TBNZ	$63, R5, exit_cold          // require a >= 0
	CMP	$0, R7
	BLE	exit_cold                   // require b > 0
	UDIV	R7, R5, R10                 // q = a / b
	MUL	R7, R10, R10
	SUB	R10, R5, R5                 // r = a - q*b
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R11
	STP	(R5, ZR), (R11)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_IDIVK:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R22, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, exit_cold
	TBNZ	$63, R5, exit_cold
	CMP	$0, R7
	BLE	exit_cold
	UDIV	R7, R5, R10
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R11
	STP	(R10, ZR), (R11)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_BANDK:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	CBNZ	R6, exit_cold
	ADD	R4<<4, R22, R7
	MOVD	VAL_BITS(R7), R7
	AND	R7, R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R8
	STP	(R5, ZR), (R8)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_BORK:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	CBNZ	R6, exit_cold
	ADD	R4<<4, R22, R7
	MOVD	VAL_BITS(R7), R7
	ORR	R7, R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R8
	STP	(R5, ZR), (R8)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_BXORK:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	CBNZ	R6, exit_cold
	ADD	R4<<4, R22, R7
	MOVD	VAL_BITS(R7), R7
	EOR	R7, R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R8
	STP	(R5, ZR), (R8)
	ADD	$4, R20, R20
	DISPATCH_NEXT

// OP_SHLI: R[A] = sC << R[B] (note Lua 5.5 operand order!)
op_SHLI:
	UBFX	$16, R25, $8, R3
	ADD	R3<<4, R21, R4
	LDP	(R4), (R5, R6)              // ib = R5
	CBNZ	R6, exit_cold
	CMP	$64, R5
	BHS	exit_cold                   // require 0 <= ib < 64
	UBFX	$24, R25, $8, R7
	SUB	$OFFSET_SC, R7, R7          // sC
	LSL	R5, R7, R7                  // sC << ib
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R8
	STP	(R7, ZR), (R8)
	ADD	$4, R20, R20
	DISPATCH_NEXT

// OP_SHRI: R[A] = uint64(R[B]) >> sC
op_SHRI:
	UBFX	$16, R25, $8, R3
	ADD	R3<<4, R21, R4
	LDP	(R4), (R5, R6)
	CBNZ	R6, exit_cold
	UBFX	$24, R25, $8, R7
	SUB	$OFFSET_SC, R7, R7          // sC
	CMP	$64, R7
	BHS	exit_cold                   // require 0 <= sC < 64
	LSR	R7, R5, R5                  // uint64(rb) >> sC
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R8
	STP	(R5, ZR), (R8)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_ADD:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R21, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, add_not_both_int
	ADD	R7, R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	STP	(R5, ZR), (R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT
add_not_both_int:
	CMP	$1, R9
	BHI	exit_cold
	CBZ	R6, add_rb_int
	FMOVD	R5, F0
	B	add_rc_conv
add_rb_int:
	SCVTFD	R5, F0
add_rc_conv:
	CBZ	R8, add_rc_int
	FMOVD	R7, F1
	B	add_fdo
add_rc_int:
	SCVTFD	R7, F1
add_fdo:
	FADDD	F1, F0, F0
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	FMOVD	F0, (R10)
	MOVD	$TAG_FLOAT, R11
	MOVD	R11, VAL_TAG(R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_SUB:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R21, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, sub_not_both_int
	SUB	R7, R5, R5                  // rb - rc
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	STP	(R5, ZR), (R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT
sub_not_both_int:
	CMP	$1, R9
	BHI	exit_cold
	CBZ	R6, sub_rb_int
	FMOVD	R5, F0
	B	sub_rc_conv
sub_rb_int:
	SCVTFD	R5, F0
sub_rc_conv:
	CBZ	R8, sub_rc_int
	FMOVD	R7, F1
	B	sub_fdo
sub_rc_int:
	SCVTFD	R7, F1
sub_fdo:
	FSUBD	F1, F0, F0                  // F0 - F1
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	FMOVD	F0, (R10)
	MOVD	$TAG_FLOAT, R11
	MOVD	R11, VAL_TAG(R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_MUL:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R21, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, mul_not_both_int
	MUL	R7, R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	STP	(R5, ZR), (R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT
mul_not_both_int:
	CMP	$1, R9
	BHI	exit_cold
	CBZ	R6, mul_rb_int
	FMOVD	R5, F0
	B	mul_rc_conv
mul_rb_int:
	SCVTFD	R5, F0
mul_rc_conv:
	CBZ	R8, mul_rc_int
	FMOVD	R7, F1
	B	mul_fdo
mul_rc_int:
	SCVTFD	R7, F1
mul_fdo:
	FMULD	F1, F0, F0
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	FMOVD	F0, (R10)
	MOVD	$TAG_FLOAT, R11
	MOVD	R11, VAL_TAG(R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_DIV:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R21, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CMP	$1, R9
	BHI	exit_cold
	CBZ	R6, div_rb_int
	FMOVD	R5, F0
	B	div_rc_conv
div_rb_int:
	SCVTFD	R5, F0
div_rc_conv:
	CBZ	R8, div_rc_int
	FMOVD	R7, F1
	B	div_fdo
div_rc_int:
	SCVTFD	R7, F1
div_fdo:
	FDIVD	F1, F0, F0
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	FMOVD	F0, (R10)
	MOVD	$TAG_FLOAT, R11
	MOVD	R11, VAL_TAG(R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_MOD:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R21, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, exit_cold
	TBNZ	$63, R5, exit_cold
	CMP	$0, R7
	BLE	exit_cold
	UDIV	R7, R5, R10
	MUL	R7, R10, R10
	SUB	R10, R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R11
	STP	(R5, ZR), (R11)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_IDIV:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R21, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, exit_cold
	TBNZ	$63, R5, exit_cold
	CMP	$0, R7
	BLE	exit_cold
	UDIV	R7, R5, R10
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R11
	STP	(R10, ZR), (R11)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_BAND:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R21, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, exit_cold
	AND	R7, R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	STP	(R5, ZR), (R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_BOR:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R21, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, exit_cold
	ORR	R7, R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	STP	(R5, ZR), (R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_BXOR:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R21, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, exit_cold
	EOR	R7, R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	STP	(R5, ZR), (R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_SHL:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R21, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, exit_cold
	CMP	$64, R7
	BHS	exit_cold
	LSL	R7, R5, R5                  // rb << rc
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	STP	(R5, ZR), (R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_SHR:
	UBFX	$16, R25, $8, R3
	UBFX	$24, R25, $8, R4
	ADD	R3<<4, R21, R5
	LDP	(R5), (R5, R6)
	ADD	R4<<4, R21, R7
	LDP	(R7), (R7, R8)
	ORR	R6, R8, R9
	CBNZ	R9, exit_cold
	CMP	$64, R7
	BHS	exit_cold
	LSR	R7, R5, R5                  // uint64(rb) >> rc
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R10
	STP	(R5, ZR), (R10)
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_UNM:
	UBFX	$16, R25, $8, R3
	ADD	R3<<4, R21, R4
	LDP	(R4), (R5, R6)
	CBNZ	R6, unm_not_int
	NEG	R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R7
	STP	(R5, ZR), (R7)
	DISPATCH_NEXT
unm_not_int:
	CMP	$TAG_FLOAT, R6
	BNE	exit_cold
	FMOVD	R5, F0
	FNEGD	F0, F0
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R7
	FMOVD	F0, (R7)
	MOVD	$TAG_FLOAT, R8
	MOVD	R8, VAL_TAG(R7)
	DISPATCH_NEXT

op_BNOT:
	UBFX	$16, R25, $8, R3
	ADD	R3<<4, R21, R4
	LDP	(R4), (R5, R6)
	CBNZ	R6, exit_cold
	MVN	R5, R5
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R7
	STP	(R5, ZR), (R7)
	DISPATCH_NEXT

op_NOT:
	UBFX	$16, R25, $8, R3
	ADD	R3<<4, R21, R4
	MOVD	VAL_TAG(R4), R5
	SUB	$TAG_NIL, R5, R5
	CMP	$1, R5                      // IsFalsy: tag - 2 <= 1
	CSET	LS, R6                      // 1 if falsy (-> true), 0 if truthy (-> false)
	ADD	$TAG_FALSE, R6, R6
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R7
	STP	(ZR, R6), (R7)
	DISPATCH_NEXT

op_LEN:
	UBFX	$16, R25, $8, R3
	ADD	R3<<4, R21, R4
	LDP	(R4), (R5, R6)
	CMP	$TAG_TABLE, R6
	BNE	len_check_str
	RESOLVE_SMALL(R5, R7, R8)
	MOVWU	TBL_METAREF(R7), R9
	CBNZ	R9, exit_cold
	MOVWU	TBL_HASHREF(R7), R9
	CBNZ	R9, exit_cold
	MOVBU	TBL_ARRKIND(R7), R9
	SUB	$ARR_INT, R9, R10
	CMP	$(ARR_BOOL-ARR_INT), R10    // dense arrInt/arrFloat/arrBool
	BHI	exit_cold
	MOVWU	TBL_ARRLEN(R7), R11
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R12
	STP	(R11, ZR), (R12)
	DISPATCH_NEXT
len_check_str:
	CMP	$TAG_STRING, R6
	BNE	exit_cold
	RESOLVE_FAST(R5, R7, R8, len_str_large, len_str_ready)
	MOVWU	STR_LEN(R7), R11
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R12
	STP	(R11, ZR), (R12)
	DISPATCH_NEXT

// ============================================================================
// Branches, Comparisons & Loops
// ============================================================================

op_JMP:
	LSR	$7, R25, R0
	MOVD	$OFFSET_SJ, R2
	SUBS	R2, R0, R0                  // sJ = (w >> 7) - 16777215
	ADD	R0<<2, R20, R20
	BLT	jmp_back
	DISPATCH_NEXT
jmp_back:
	SUBS	$1, R26, R26
	BLE	exit_poll
	DISPATCH_NEXT

op_EQ:
	UBFX	$7, R25, $8, R2
	UBFX	$16, R25, $8, R3
	ADD	R2<<4, R21, R4
	LDP	(R4), (R4, R5)              // ra = (R4, R5)
	ADD	R3<<4, R21, R6
	LDP	(R6), (R6, R7)              // rb = (R6, R7)
	ORR	R5, R7, R8
	CBNZ	R8, eq_not_both_int
	CMP	R6, R4
	CSET	EQ, R0
	COND_DISPATCH(eq_skip, eq_back)
eq_not_both_int:
	CMP	R7, R5
	BNE	eq_diff_tag
	CMP	$TAG_FLOAT, R5
	BEQ	eq_both_float
	CMP	R6, R4
	BEQ	eq_true
	CMP	$TAG_TABLE, R5
	BEQ	eq_diff_tables
	CMP	$TAG_USERDATA, R5
	BEQ	exit_cold
	CMP	$TAG_STRING, R5
	BEQ	eq_diff_str
	CMP	$TAG_FROZENSTR, R5
	BEQ	exit_cold
eq_false:
	MOVD	$0, R0
	COND_DISPATCH(eq_skip2, eq_back2)
eq_diff_str:
	// Short strings (gcKindShortStr) are uniquely interned per State: distinct ObjRef => false!
	TBNZ	$ARENA_LARGEBIT, R4, exit_cold
	TBNZ	$ARENA_LARGEBIT, R6, exit_cold
	RESOLVE_SMALL(R4, R9, R10)
	MOVBU	STR_KIND(R9), R11
	CMP	$GCKIND_SHORTSTR, R11
	BNE	exit_cold
	RESOLVE_SMALL(R6, R9, R10)
	MOVBU	STR_KIND(R9), R11
	CMP	$GCKIND_SHORTSTR, R11
	BNE	exit_cold
	B	eq_false
eq_true:
	MOVD	$1, R0
	COND_DISPATCH(eq_skip3, eq_back3)
eq_both_float:
	FMOVD	R4, F0
	FMOVD	R6, F1
	FCMPD	F1, F0
	CSET	EQ, R0
	COND_DISPATCH(eq_skip4, eq_back4)
eq_diff_tables:
	RESOLVE_SMALL(R4, R9, R10)
	MOVWU	TBL_METAREF(R9), R11
	CBNZ	R11, exit_cold
	RESOLVE_SMALL(R6, R9, R10)
	MOVWU	TBL_METAREF(R9), R11
	CBNZ	R11, exit_cold
	B	eq_false
eq_diff_tag:
	CMP	$1, R8
	BLS	exit_cold                   // int vs float -> exact boundary check in Go
	SUB	$TAG_FROZENSTR, R5, R9
	CMP	$1, R9
	BHI	eq_false
	SUB	$TAG_FROZENSTR, R7, R10
	CMP	$1, R10
	BLS	exit_cold                   // frozen str vs dynamic str -> Go
	B	eq_false

op_LT:
	UBFX	$7, R25, $8, R2
	UBFX	$16, R25, $8, R3
	ADD	R2<<4, R21, R4
	LDP	(R4), (R4, R5)
	ADD	R3<<4, R21, R6
	LDP	(R6), (R6, R7)
	ORR	R5, R7, R8
	CBNZ	R8, lt_not_both_int
	CMP	R6, R4
	CSET	LT, R0
	COND_DISPATCH(lt_skip, lt_back)
lt_not_both_int:
	CMP	$TAG_FLOAT, R5
	BNE	exit_cold
	CMP	$TAG_FLOAT, R7
	BNE	exit_cold
	FMOVD	R4, F0
	FMOVD	R6, F1
	FCMPD	F1, F0                      // F0 - F1; MI (N==1) is NaN-safe <
	CSET	MI, R0
	COND_DISPATCH(lt_skip2, lt_back2)

op_LE:
	UBFX	$7, R25, $8, R2
	UBFX	$16, R25, $8, R3
	ADD	R2<<4, R21, R4
	LDP	(R4), (R4, R5)
	ADD	R3<<4, R21, R6
	LDP	(R6), (R6, R7)
	ORR	R5, R7, R8
	CBNZ	R8, le_not_both_int
	CMP	R6, R4
	CSET	LE, R0
	COND_DISPATCH(le_skip, le_back)
le_not_both_int:
	CMP	$TAG_FLOAT, R5
	BNE	exit_cold
	CMP	$TAG_FLOAT, R7
	BNE	exit_cold
	FMOVD	R4, F0
	FMOVD	R6, F1
	FCMPD	F1, F0                      // LS (C==0||Z==1) is NaN-safe <=
	CSET	LS, R0
	COND_DISPATCH(le_skip2, le_back2)

op_EQK:
	UBFX	$7, R25, $8, R2
	UBFX	$16, R25, $8, R3
	ADD	R2<<4, R21, R4
	LDP	(R4), (R4, R5)              // ra
	ADD	R3<<4, R22, R6
	LDP	(R6), (R6, R7)              // kb
	ORR	R5, R7, R8
	CBNZ	R8, eqk_not_both_int
	CMP	R6, R4
	CSET	EQ, R0
	COND_DISPATCH(eqk_skip, eqk_back)
eqk_not_both_int:
	CMP	R7, R5
	BNE	eqk_diff_tag
	CMP	$TAG_FLOAT, R5
	BEQ	eqk_both_float
	CMP	R6, R4
	BEQ	eqk_true
	CMP	$TAG_STRING, R5
	BEQ	eqk_diff_str
	CMP	$TAG_FROZENSTR, R5
	BEQ	exit_cold
eqk_false:
	MOVD	$0, R0
	COND_DISPATCH(eqk_skip2, eqk_back2)
eqk_diff_str:
	TBNZ	$ARENA_LARGEBIT, R4, exit_cold
	TBNZ	$ARENA_LARGEBIT, R6, exit_cold
	RESOLVE_SMALL(R4, R9, R10)
	MOVBU	STR_KIND(R9), R11
	CMP	$GCKIND_SHORTSTR, R11
	BNE	exit_cold
	RESOLVE_SMALL(R6, R9, R10)
	MOVBU	STR_KIND(R9), R11
	CMP	$GCKIND_SHORTSTR, R11
	BNE	exit_cold
	B	eqk_false
eqk_true:
	MOVD	$1, R0
	COND_DISPATCH(eqk_skip3, eqk_back3)
eqk_both_float:
	FMOVD	R4, F0
	FMOVD	R6, F1
	FCMPD	F1, F0
	CSET	EQ, R0
	COND_DISPATCH(eqk_skip4, eqk_back4)
eqk_diff_tag:
	CMP	$1, R8
	BLS	exit_cold
	SUB	$TAG_FROZENSTR, R5, R9
	CMP	$1, R9
	BHI	eqk_false
	SUB	$TAG_FROZENSTR, R7, R10
	CMP	$1, R10
	BLS	exit_cold
	B	eqk_false

op_EQI:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R4
	LDP	(R4), (R4, R5)
	UBFX	$16, R25, $8, R6
	SUB	$OFFSET_SC, R6, R6          // sB
	CBNZ	R5, eqi_not_int
	CMP	R6, R4
	CSET	EQ, R0
	COND_DISPATCH(eqi_skip, eqi_back)
eqi_not_int:
	CMP	$TAG_FLOAT, R5
	BNE	eqi_false
	FMOVD	R4, F0
	SCVTFD	R6, F1
	FCMPD	F1, F0
	CSET	EQ, R0
	COND_DISPATCH(eqi_skip2, eqi_back2)
eqi_false:
	MOVD	$0, R0
	COND_DISPATCH(eqi_skip3, eqi_back3)

op_LTI:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R4
	LDP	(R4), (R4, R5)
	UBFX	$16, R25, $8, R6
	SUB	$OFFSET_SC, R6, R6
	CBNZ	R5, lti_not_int
	CMP	R6, R4
	CSET	LT, R0
	COND_DISPATCH(lti_skip, lti_back)
lti_not_int:
	CMP	$TAG_FLOAT, R5
	BNE	exit_cold
	FMOVD	R4, F0
	SCVTFD	R6, F1
	FCMPD	F1, F0
	CSET	MI, R0
	COND_DISPATCH(lti_skip2, lti_back2)

op_LEI:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R4
	LDP	(R4), (R4, R5)
	UBFX	$16, R25, $8, R6
	SUB	$OFFSET_SC, R6, R6
	CBNZ	R5, lei_not_int
	CMP	R6, R4
	CSET	LE, R0
	COND_DISPATCH(lei_skip, lei_back)
lei_not_int:
	CMP	$TAG_FLOAT, R5
	BNE	exit_cold
	FMOVD	R4, F0
	SCVTFD	R6, F1
	FCMPD	F1, F0
	CSET	LS, R0
	COND_DISPATCH(lei_skip2, lei_back2)

op_GTI:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R4
	LDP	(R4), (R4, R5)
	UBFX	$16, R25, $8, R6
	SUB	$OFFSET_SC, R6, R6
	CBNZ	R5, gti_not_int
	CMP	R6, R4
	CSET	GT, R0
	COND_DISPATCH(gti_skip, gti_back)
gti_not_int:
	CMP	$TAG_FLOAT, R5
	BNE	exit_cold
	FMOVD	R4, F0
	SCVTFD	R6, F1
	FCMPD	F1, F0                      // GT is NaN-safe >
	CSET	GT, R0
	COND_DISPATCH(gti_skip2, gti_back2)

op_GEI:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R4
	LDP	(R4), (R4, R5)
	UBFX	$16, R25, $8, R6
	SUB	$OFFSET_SC, R6, R6
	CBNZ	R5, gei_not_int
	CMP	R6, R4
	CSET	GE, R0
	COND_DISPATCH(gei_skip, gei_back)
gei_not_int:
	CMP	$TAG_FLOAT, R5
	BNE	exit_cold
	FMOVD	R4, F0
	SCVTFD	R6, F1
	FCMPD	F1, F0                      // GE is NaN-safe >=
	CSET	GE, R0
	COND_DISPATCH(gei_skip2, gei_back2)

op_TEST:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R4
	MOVD	VAL_TAG(R4), R5
	SUB	$TAG_NIL, R5, R5
	CMP	$1, R5
	CSET	HI, R0                      // 1 if truthy (!IsFalsy), 0 if falsy
	// interp_run.go: if IsFalsy() == K { skip } else { jump } <=> if truthy != K { skip } else { jump }
	// Check: K=1, falsy (truthy=0): IsFalsy()==K -> skip; truthy!=K -> skip!
	//        K=0, falsy (truthy=0): IsFalsy()!=K -> jump; truthy==K -> jump!
	//        K=1, truthy(truthy=1): IsFalsy()!=K -> jump; truthy==K -> jump!
	//        K=0, truthy(truthy=1): IsFalsy()==K -> skip; truthy!=K -> skip!
	COND_DISPATCH(test_skip, test_back)

op_TESTSET:
	UBFX	$16, R25, $8, R3
	ADD	R3<<4, R21, R4
	LDP	(R4), (R5, R6)
	SUB	$TAG_NIL, R6, R7
	CMP	$1, R7
	CSET	HI, R0                      // 1 if truthy, 0 if falsy
	UBFX	$15, R25, $1, R1            // K
	CMP	R1, R0
	BNE	testset_skip
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R8
	STP	(R5, R6), (R8)
	MOVWU	(R20), R0
	LSR	$7, R0, R0
	MOVD	$OFFSET_SJ, R2
	SUBS	R2, R0, R0
	ADD	$1, R0, R1
	ADD	R1<<2, R20, R20
	BLT	testset_back
	DISPATCH_NEXT
testset_back:
	SUBS	$1, R26, R26
	BLE	exit_poll
	DISPATCH_NEXT
testset_skip:
	ADD	$4, R20, R20
	DISPATCH_NEXT

op_FORLOOP:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R3              // &regs[A]
	LDP	VAL_SIZE(R3), (R4, R5)      // regs[A+1] = step
	CBNZ	R5, forloop_float
	LDP	(R3), (R6, R7)              // regs[A] = cnt
	LDP	(2*VAL_SIZE)(R3), (R8, R9)  // regs[A+2] = idx
	ORR	R7, R9, R10
	CBNZ	R10, forloop_done
	CBZ	R6, forloop_done            // uint64(cnt) == 0 -> done
	SUB	$1, R6, R6
	ADD	R4, R8, R8
	MOVD	R6, (R3)
	MOVD	R8, (2*VAL_SIZE)(R3)
	LSR	$15, R25, R11               // Bx
	SUB	R11<<2, R20, R20
	SUBS	$1, R26, R26
	BLE	exit_poll
	DISPATCH_NEXT
forloop_float:
	CMP	$TAG_FLOAT, R5
	BNE	forloop_done
	LDP	(R3), (R6, R7)              // regs[A] = lim
	LDP	(2*VAL_SIZE)(R3), (R8, R9)  // regs[A+2] = idx
	CMP	$TAG_FLOAT, R7
	BNE	forloop_done
	CMP	$TAG_FLOAT, R9
	BNE	forloop_done
	FMOVD	R4, F0                      // step
	FMOVD	R6, F1                      // lim
	FMOVD	R8, F2                      // idx
	FADDD	F0, F2, F2                  // idx += step
	FCMPD	$(0.0), F0
	BGT	forloop_fpos
	BLS	forloop_fneg                // step <= 0 (rejects NaN!)
	DISPATCH_NEXT
forloop_fpos:
	FCMPD	F1, F2                      // idx <= lim (LS is NaN-safe)
	BLS	forloop_ftake
	DISPATCH_NEXT
forloop_fneg:
	FCMPD	F1, F2                      // idx >= lim (GE is NaN-safe)
	BGE	forloop_ftake
	DISPATCH_NEXT
forloop_ftake:
	FMOVD	F2, (2*VAL_SIZE)(R3)
	LSR	$15, R25, R11
	SUB	R11<<2, R20, R20
	SUBS	$1, R26, R26
	BLE	exit_poll
	DISPATCH_NEXT
forloop_done:
	DISPATCH_NEXT

op_TFORLOOP:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R3
	MOVD	(3*VAL_SIZE+VAL_TAG)(R3), R4
	CMP	$TAG_NIL, R4
	BEQ	tforloop_done
	LSR	$15, R25, R5
	SUB	R5<<2, R20, R20
	SUBS	$1, R26, R26
	BLE	exit_poll
tforloop_done:
	DISPATCH_NEXT

// OP_FORPREP: integer fast path matching (*State).forPrepSlow byte-for-byte.
op_FORPREP:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R3              // &regs[A]
	LDP	(R3), (R4, R5)              // origInit = (R4, R5)
	LDP	VAL_SIZE(R3), (R6, R7)      // origLim  = (R6, R7)
	LDP	(2*VAL_SIZE)(R3), (R8, R9)  // origStep = (R8, R9)
	ORR	R5, R7, R10
	ORR	R9, R10, R10
	CBNZ	R10, exit_cold              // require all three TAG_INT (0)
	CMP	$0, R8
	BLE	forprep_nonpos
	// step > 0
	CMP	R6, R4                      // init > lim?
	BGT	forprep_skip
	SUB	R4, R6, R11                 // uint64(lim) - uint64(init)
	CMP	$1, R8
	BEQ	forprep_store
	UDIV	R8, R11, R11
forprep_store:
	MOVD	R11, (R3)                   // regs[A] = cnt (tag already TAG_INT)
	MOVD	R8, VAL_SIZE(R3)            // regs[A+1] = step
	MOVD	R4, (2*VAL_SIZE)(R3)        // regs[A+2] = init
	DISPATCH_NEXT
forprep_nonpos:
	CBZ	R8, exit_cold               // step == 0 -> error in Go
	// step < 0
	CMP	R6, R4                      // init < lim?
	BLT	forprep_skip
	SUB	R6, R4, R11                 // uint64(init) - uint64(lim)
	ADD	$1, R8, R12                 // step + 1 (safe even for MinInt64)
	NEG	R12, R12                    // -(step + 1)
	ADD	$1, R12, R12                // uint64(-(step+1)) + 1
	UDIV	R12, R11, R11
	B	forprep_store
forprep_skip:
	LSR	$15, R25, R0                // Bx
	ADD	$1, R0, R0                  // Bx + 1
	ADD	R0<<2, R20, R20
	DISPATCH_NEXT

// OP_TAILCALL: fixed-arity Lua-to-Lua tail call (B != 0, K == 0, non-vararg closure).
op_TAILCALL:
	TBNZ	$15, R25, exit_cold         // K == 1 -> closeUpvals in Go
	UBFX	$16, R25, $8, R3            // B
	CBZ	R3, exit_cold               // B == 0 (MULTARG) -> Go
	UBFX	$7, R25, $8, R2             // A
	ADD	R2<<4, R21, R4              // &stack[curBase + A] = &srcStart
	LDP	(R4), (R5, R6)              // fn = regs[A]
	CMP	$TAG_CLOSURE, R6
	BNE	exit_cold
	RESOLVE_SMALL(R5, R7, R8)           // R7 = *Closure (callee)
	MOVWU	CL_PROTOIDX(R7), R8
	MOVD	CTX_PROTOS(R19), R9
	MOVD	(R9)(R8<<3), R10            // R10 = *boundProto
	MOVBU	BP_ISVARARG(R10), R11
	CBNZ	R11, exit_cold              // vararg -> Go
	// Load current callFrame cf = &frames[curCI]
	MOVD	CTX_CI(R19), R12
	MOVD	$CF_SIZE, R13
	MUL	R12, R13, R13
	MOVD	CTX_FRAMES(R19), R14
	ADD	R13, R14, R13               // R13 = cf
	MOVD	CF_FUNCIDX(R13), R14        // funcIdx
	ADD	$1, R14, R15                // newBase = funcIdx + 1
	SUB	$1, R3, R3                  // nargs = B - 1
	MOVBU	BP_MAXSTACK(R10), R8        // maxStack
	// Compute need = newBase + max(nargs, maxStack) + STACK_SLACK
	CMP	R8, R3
	CSEL	GT, R3, R8, R9              // max(nargs, maxStack)
	ADD	R15, R9, R9                 // newBase + max(nargs, maxStack)
	ADD	$STACK_SLACK, R9, R11
	MOVD	CTX_TH(R19), R12
	MOVD	TH_STACKLEN(R12), R2
	CMP	R2, R11
	BGT	exit_cold                   // stack growth -> Go
	// Copy [srcStart .. srcStart+nargs] -> [funcIdx .. funcIdx+nargs] (ascending, overlap-safe since funcIdx < curBase <= srcStart)
	ADD	R14<<4, R17, R2             // dst = &stack[funcIdx]
	ADD	$1, R3, R11                 // count = 1 + nargs (>= 1)
tailcall_copy_loop:
	LDP.P	16(R4), (R6, R9)
	STP.P	(R6, R9), 16(R2)
	SUBS	$1, R11, R11
	BNE	tailcall_copy_loop
	// Fill [newBase+nargs .. newBase+maxStack) with NilValue if nargs < maxStack
	CMP	R8, R3
	BGE	tailcall_nil_done
	SUB	R3, R8, R11                 // maxStack - nargs (> 0)
	MOVD	$TAG_NIL, R6
tailcall_nil_loop:
	STP.P	(ZR, R6), 16(R2)
	SUBS	$1, R11, R11
	BNE	tailcall_nil_loop
tailcall_nil_done:
	// Update Thread top & highStack
	ADD	R8, R15, R9                 // frameEnd = newBase + maxStack
	MOVD	R9, TH_TOP(R12)
	MOVD	TH_HIGHSTACK(R12), R2
	CMP	R2, R9
	BLE	tailcall_hs_ok
	MOVD	R9, TH_HIGHSTACK(R12)
tailcall_hs_ok:
	// Update callFrame cf (R13) in place matching interp_cold.go:3336-3344
	MOVW	R5, CF_CLREF(R13)
	MOVW	ZR, CF_VARARGTABREF(R13)
	MOVD	R15, CF_BASE(R13)
	MOVD	ZR, CF_PC(R13)
	MOVD	ZR, CF_NEXTRAARGS(R13)
	MOVD	$1, R2
	MOVB	R2, CF_DIRTY(R13)
	MOVB	R2, CF_ISTAILCALL(R13)
	MOVB	ZR, CF_EXTRAARGS(R13)
	MOVD	$-1, R2
	MOVD	R2, CF_OLDPC(R13)
	// Update ctx & pinned registers for callee
	MOVD	R15, CTX_BASE(R19)
	ADD	R15<<4, R17, R21            // R_REGS = &stack[newBase]
	MOVWU	CL_UPVALSREF(R7), R2
	CBZ	R2, tailcall_no_upvals
	RESOLVE_SMALL(R2, R3, R8)
	MOVD	R3, CTX_CLUPVALS(R19)
	B	tailcall_check_bp
tailcall_no_upvals:
	MOVD	ZR, CTX_CLUPVALS(R19)
tailcall_check_bp:
	MOVD	CTX_CURBP(R19), R2
	CMP	R2, R10
	BEQ	tailcall_same_bp
	MOVD	R10, CTX_CURBP(R19)
	MOVD	BP_INSNS(R10), R16
	MOVD	BP_CONSTS(R10), R22
	MOVD	BP_ICSLOTS(R10), R3
	MOVD	BP_IC(R10), R4
	MOVD	R16, CTX_INSNS(R19)
	MOVD	R22, CTX_CONSTS(R19)
	MOVD	R3, CTX_ICSLOTS(R19)
	MOVD	R4, CTX_IC(R19)
tailcall_same_bp:
	MOVD	R16, R20                    // pc = 0
	SUBS	$1, R26, R26
	BLE	exit_poll
	DISPATCH_NEXT

// ============================================================================
// Fast Lua-to-Lua Call & Return (stays 100% inside Plan 9 ARM64 loop!)
// ============================================================================

op_CALL:
	UBFX	$16, R25, $8, R3            // B
	CBZ	R3, exit_cold               // B == 0 (MULTARG) -> Go
	UBFX	$7, R25, $8, R2             // A
	ADD	R2<<4, R21, R4
	LDP	(R4), (R5, R6)              // fn = regs[A]
	CMP	$TAG_CLOSURE, R6
	BNE	call_check_static_nat       // check inline math.sqrt / math.abs before exit_cold
	RESOLVE_SMALL(R5, R7, R8)           // R7 = *Closure (callee)
	MOVWU	CL_PROTOIDX(R7), R8
	MOVD	CTX_PROTOS(R19), R9
	MOVD	(R9)(R8<<3), R10            // R10 = *boundProto
	MOVBU	BP_ISVARARG(R10), R11
	CBNZ	R11, exit_cold              // vararg -> Go
	MOVD	CTX_TH(R19), R12            // R12 = *Thread
	MOVD	TH_NFRAME(R12), R13         // newCI = th.nframe
	MOVD	TH_FRAMESCAP(R12), R14
	CMP	R14, R13
	BGE	exit_cold                   // frames full -> Go
	MOVD	CTX_BASE(R19), R14
	ADD	R2, R14, R14                // funcIdx = curBase + A
	ADD	$1, R14, R15                // newBase = funcIdx + 1
	MOVBU	BP_MAXSTACK(R10), R8
	ADD	R8, R15, R9                 // frameEnd = newBase + maxStack
	ADD	$STACK_SLACK, R9, R11       // need = frameEnd + stackSlack
	MOVD	TH_STACKLEN(R12), R2
	CMP	R2, R11
	BGT	exit_cold                   // need > stackLen -> Go
	// Compute nf = &frames[newCI] (sizeof(callFrame) == 200)
	MOVD	$CF_SIZE, R2
	MUL	R13, R2, R4
	MOVD	CTX_FRAMES(R19), R6
	ADD	R4, R6, R4                  // R4 = nf
	MOVBU	CF_DIRTY(R4), R11
	CBZ	R11, call_nf_clean
	// Lazily clear dirty callFrame in place (200B = 12*16B + 8B) so future calls at this
	// depth stay inside Plan 9 asm instead of bouncing to callSlow/pushLuaFrame!
	STP	(ZR, ZR), 0(R4)
	STP	(ZR, ZR), 16(R4)
	STP	(ZR, ZR), 32(R4)
	STP	(ZR, ZR), 48(R4)
	STP	(ZR, ZR), 64(R4)
	STP	(ZR, ZR), 80(R4)
	STP	(ZR, ZR), 96(R4)
	STP	(ZR, ZR), 112(R4)
	STP	(ZR, ZR), 128(R4)
	STP	(ZR, ZR), 144(R4)
	STP	(ZR, ZR), 160(R4)
	STP	(ZR, ZR), 176(R4)
	MOVD	ZR, 192(R4)
call_nf_clean:
	// Fill missing params with nil: nargs = B - 1 < nparams
	SUB	$1, R3, R3                  // nargs = B - 1
	MOVBU	BP_NUMPARAMS(R10), R11
	CMP	R11, R3
	BGE	call_params_done
	ADD	R15, R3, R2                 // newBase + nargs
	ADD	R2<<4, R17, R2              // &stack[newBase + nargs]
	SUB	R3, R11, R3                 // count = nparams - nargs (> 0)
	MOVD	$TAG_NIL, R11
call_nil_loop:
	STP	(ZR, R11), (R2)
	ADD	$16, R2, R2
	SUBS	$1, R3, R3
	BNE	call_nil_loop
call_params_done:
	// Save caller PC into frames[curCI].pc
	SUB	R16, R20, R2
	LSR	$2, R2, R2                  // caller next pc
	MOVD	CTX_CI(R19), R3
	MOVD	$CF_SIZE, R11
	MUL	R3, R11, R3
	ADD	R3, R6, R3                  // &frames[curCI]
	MOVD	R2, CF_PC(R3)
	// Populate callee callFrame nf (R4)
	MOVW	R5, CF_CLREF(R4)            // nf.clRef = callee.self (R5)
	MOVD	R15, CF_BASE(R4)            // nf.base = newBase
	MOVD	R14, CF_FUNCIDX(R4)         // nf.funcIdx = funcIdx
	MOVD	ZR, CF_PC(R4)               // nf.pc = 0
	UBFX	$24, R25, $8, R2            // C
	SUB	$1, R2, R2                  // c - 1
	MOVD	R2, CF_NRESULTS(R4)
	// Update Thread watermarks
	ADD	$1, R13, R2                 // newCI + 1
	MOVD	R2, TH_NFRAME(R12)
	MOVD	TH_HIGHFRAMES(R12), R3
	CMP	R3, R2
	BLE	call_hf_ok
	MOVD	R2, TH_HIGHFRAMES(R12)
call_hf_ok:
	// R9 still holds frameEnd = newBase + maxStack
	MOVD	TH_HIGHSTACK(R12), R3
	CMP	R3, R9
	BLE	call_hs_ok
	MOVD	R9, TH_HIGHSTACK(R12)
call_hs_ok:
	// Update ctx & pinned registers for callee
	MOVD	R13, CTX_CI(R19)
	MOVD	R15, CTX_BASE(R19)
	ADD	R15<<4, R17, R21            // R_REGS = &stack[newBase]
	// Resolve callee.upvalsRef -> ctx.clUpvals
	MOVWU	CL_UPVALSREF(R7), R2
	CBZ	R2, call_no_upvals
	RESOLVE_SMALL(R2, R3, R8)
	MOVD	R3, CTX_CLUPVALS(R19)
	B	call_check_bp
call_no_upvals:
	MOVD	ZR, CTX_CLUPVALS(R19)
call_check_bp:
	MOVD	CTX_CURBP(R19), R2
	CMP	R2, R10
	BEQ	call_same_bp
	MOVD	R10, CTX_CURBP(R19)
	MOVD	BP_INSNS(R10), R16
	MOVD	BP_CONSTS(R10), R22
	MOVD	BP_ICSLOTS(R10), R3
	MOVD	BP_IC(R10), R4
	MOVD	R16, CTX_INSNS(R19)
	MOVD	R22, CTX_CONSTS(R19)
	MOVD	R3, CTX_ICSLOTS(R19)
	MOVD	R4, CTX_IC(R19)
call_same_bp:
	MOVD	R16, R20                    // pc = 0
	SUBS	$1, R26, R26
	BLE	exit_poll
	DISPATCH_NEXT

// Fast hardware intrinsics for math.sqrt (FSQRTD) and math.abs (FABSD / CNEG) when C==2, B>=2.
call_check_static_nat:
	CMP	$TAG_STATICNAT, R6
	BNE	exit_cold
	CMP	$2, R3                      // require nargs >= 1 (B >= 2)
	BLO	exit_cold
	UBFX	$24, R25, $8, R7            // C
	CMP	$2, R7                      // single fixed result (C == 2)
	BNE	exit_cold
	MOVWU	CTX_SQRTIDX(R19), R8
	CBZ	R8, call_check_abs
	CMP	R8, R5
	BNE	call_check_abs
	LDP	VAL_SIZE(R4), (R9, R10)     // arg0 = regs[A+1]
	CMP	$TAG_FLOAT, R10
	BEQ	call_sqrt_float
	CBNZ	R10, exit_cold
	SCVTFD	R9, F0
	FSQRTD	F0, F0
	FMOVD	F0, (R4)
	MOVD	$TAG_FLOAT, R11
	MOVD	R11, VAL_TAG(R4)
	DISPATCH_NEXT
call_sqrt_float:
	FMOVD	R9, F0
	FSQRTD	F0, F0
	FMOVD	F0, (R4)
	MOVD	$TAG_FLOAT, R11
	MOVD	R11, VAL_TAG(R4)
	DISPATCH_NEXT
call_check_abs:
	MOVWU	CTX_ABSIDX(R19), R8
	CBZ	R8, exit_cold
	CMP	R8, R5
	BNE	exit_cold
	LDP	VAL_SIZE(R4), (R9, R10)     // arg0 = regs[A+1]
	CMP	$TAG_FLOAT, R10
	BEQ	call_abs_float
	CBNZ	R10, exit_cold
	CMP	$0, R9
	CNEG	LT, R9, R9
	STP	(R9, ZR), (R4)
	DISPATCH_NEXT
call_abs_float:
	FMOVD	R9, F0
	FABSD	F0, F0
	FMOVD	F0, (R4)
	MOVD	$TAG_FLOAT, R11
	MOVD	R11, VAL_TAG(R4)
	DISPATCH_NEXT

op_RETURN0:
	MOVD	CTX_CI(R19), R2
	MOVD	$CF_SIZE, R3
	MUL	R2, R3, R4
	MOVD	CTX_FRAMES(R19), R5
	ADD	R4, R5, R4                  // cf = &frames[curCI]
	MOVD	CF_NRESULTS(R4), R6         // nres
	MOVD	CF_FUNCIDX(R4), R7          // funcIdx
	MOVD	CTX_TH(R19), R8
	MOVD	TH_NFRAME(R8), R9
	SUB	$1, R9, R9                  // newNFrame
	MOVD	R9, TH_NFRAME(R8)
	CMP	$0, R6
	BLE	ret0_no_fill
	ADD	R7<<4, R17, R10             // &stack[funcIdx]
	MOVD	R6, R11
	MOVD	$TAG_NIL, R12
ret0_nil_loop:
	STP	(ZR, R12), (R10)
	ADD	$16, R10, R10
	SUBS	$1, R11, R11
	BNE	ret0_nil_loop
	ADD	R6, R7, R7                  // top = funcIdx + nres
ret0_no_fill:
	MOVD	R7, TH_TOP(R8)
	MOVD	CTX_ENTRYDEPTH(R19), R10
	CMP	R10, R9
	BLE	exit_done
	B	return_restore_caller

op_RETURN1:
	UBFX	$7, R25, $8, R2
	ADD	R2<<4, R21, R3
	LDP	(R3), (R14, R15)            // retVal = (R14, R15)
	MOVD	CTX_CI(R19), R2
	MOVD	$CF_SIZE, R3
	MUL	R2, R3, R4
	MOVD	CTX_FRAMES(R19), R5
	ADD	R4, R5, R4                  // cf = &frames[curCI]
	MOVD	CF_NRESULTS(R4), R6         // nres
	MOVD	CF_FUNCIDX(R4), R7          // funcIdx
	MOVD	CTX_TH(R19), R8
	MOVD	TH_NFRAME(R8), R9
	SUB	$1, R9, R9                  // newNFrame
	MOVD	R9, TH_NFRAME(R8)
	CBZ	R6, ret1_zero_res
	ADD	R7<<4, R17, R10             // &stack[funcIdx]
	STP	(R14, R15), (R10)           // stack[funcIdx] = retVal
	CMP	$1, R6
	BLE	ret1_one_res
	ADD	$16, R10, R10
	SUB	$1, R6, R11                 // nres - 1 (> 0)
	MOVD	$TAG_NIL, R12
ret1_nil_loop:
	STP	(ZR, R12), (R10)
	ADD	$16, R10, R10
	SUBS	$1, R11, R11
	BNE	ret1_nil_loop
	ADD	R6, R7, R7                  // top = funcIdx + nres
	B	ret1_set_top
ret1_one_res:
	ADD	$1, R7, R7                  // top = funcIdx + 1
ret1_zero_res:
ret1_set_top:
	MOVD	R7, TH_TOP(R8)
	MOVD	CTX_ENTRYDEPTH(R19), R10
	CMP	R10, R9
	BLE	exit_done

// Shared fast caller restore for OP_RETURN0 and OP_RETURN1.
// Inputs: R5 = frames base, R9 = newNFrame (> entryDepth).
return_restore_caller:
	SUB	$1, R9, R2                  // callerCI = newNFrame - 1
	MOVD	$CF_SIZE, R3
	MUL	R2, R3, R4
	ADD	R4, R5, R4                  // callerCF = &frames[callerCI]
	MOVBU	CF_KIND(R4), R6
	CBNZ	R6, exit_reload             // non-Lua frame -> Go prepareLuaFrame
	MOVBU	CF_FINISH(R4), R6
	CBNZ	R6, exit_reload             // pending continuation -> Go prepareLuaFrame
	MOVD	R2, CTX_CI(R19)
	MOVD	CF_BASE(R4), R7
	MOVD	R7, CTX_BASE(R19)
	ADD	R7<<4, R17, R21             // R_REGS = &stack[caller.base]
	MOVWU	CF_CLREF(R4), R8
	RESOLVE_SMALL(R8, R9, R10)          // R9 = caller *Closure
	MOVWU	CL_UPVALSREF(R9), R11
	CBZ	R11, ret_no_upvals
	RESOLVE_SMALL(R11, R12, R10)
	MOVD	R12, CTX_CLUPVALS(R19)
	B	ret_check_bp
ret_no_upvals:
	MOVD	ZR, CTX_CLUPVALS(R19)
ret_check_bp:
	MOVWU	CL_PROTOIDX(R9), R11
	MOVD	CTX_PROTOS(R19), R12
	MOVD	(R12)(R11<<3), R13          // R13 = caller *boundProto
	MOVD	CTX_CURBP(R19), R14
	CMP	R14, R13
	BEQ	ret_same_bp
	MOVD	R13, CTX_CURBP(R19)
	MOVD	BP_INSNS(R13), R16
	MOVD	BP_CONSTS(R13), R22
	MOVD	BP_ICSLOTS(R13), R14
	MOVD	BP_IC(R13), R15
	MOVD	R16, CTX_INSNS(R19)
	MOVD	R22, CTX_CONSTS(R19)
	MOVD	R14, CTX_ICSLOTS(R19)
	MOVD	R15, CTX_IC(R19)
ret_same_bp:
	MOVD	CF_PC(R4), R14
	ADD	R14<<2, R16, R20            // R_PC = insns + caller.pc*4
	DISPATCH_NEXT

// ============================================================================
// Exit Paths
// ============================================================================

exit_cold:
	SUB	R16, R20, R0
	LSR	$2, R0, R0
	SUB	$1, R0, R0                  // faulting instruction PC
	MOVD	R0, CTX_PC(R19)
	MOVD	R21, CTX_REGS(R19)
	MOVD	R16, CTX_INSNS(R19)
	MOVD	R22, CTX_CONSTS(R19)
	MOVD	$ASM_EXIT_COLD, R0
	MOVD	R0, ret+8(FP)
	RET

exit_poll:
	SUB	R16, R20, R0
	LSR	$2, R0, R0                  // next instruction PC
	MOVD	R0, CTX_PC(R19)
	MOVD	R21, CTX_REGS(R19)
	MOVD	R16, CTX_INSNS(R19)
	MOVD	R22, CTX_CONSTS(R19)
	MOVD	R26, CTX_POLL(R19)
	MOVD	$ASM_EXIT_POLL, R0
	MOVD	R0, ret+8(FP)
	RET

exit_reload:
	MOVD	$ASM_EXIT_RELOAD, R0
	MOVD	R0, ret+8(FP)
	RET

exit_done:
	MOVD	$ASM_EXIT_DONE, R0
	MOVD	R0, ret+8(FP)
	RET
