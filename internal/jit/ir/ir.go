// Package ir implements a lightweight typed SSA loop optimizer, loop-invariant
// code motion (LICM), bounds-check elimination (BCE), 2-wide SIMD auto-vectorizer,
// and linear-scan register allocator for MoeLua's native JIT backend.
package ir

import (
	"fmt"

	"github.com/yuchanns/moelua/bytecode"
)

// Type identifies the unboxed / guarded machine type of an SSA value.
type Type uint8

const (
	TUnknown    Type = iota
	TInt             // unboxed 64-bit signed int64 in GPR
	TFloat           // unboxed IEEE-754 float64 in FP/XMM register
	TBool            // unboxed 0/1 boolean in GPR
	TTable           // resolved non-null *Table pointer in GPR
	TArrInt          // hoisted (*Table -> arrBase, arrLen) for arrInt
	TArrFloat        // hoisted (*Table -> arrBase, arrLen) for arrFloat
	TArrBool         // hoisted (*Table -> arrBase, arrLen) for arrBool
	TArrGeneric      // hoisted (*Table -> arrBase, arrLen) for arrGeneric
	TShapeObj        // guarded (*Table, slotsBase) matching a known ShapeRef
)

func (t Type) IsNumeric() bool { return t == TInt || t == TFloat }
func (t Type) IsArray() bool {
	return t == TArrInt || t == TArrFloat || t == TArrBool || t == TArrGeneric
}
func (t Type) IsFPReg() bool { return t == TFloat }

// ArrKind constants matching vm.arrKind (kept decoupled from vm to avoid import cycles).
const (
	ArrKindEmpty   uint8 = 0
	ArrKindInt     uint8 = 1
	ArrKindFloat   uint8 = 2
	ArrKindBool    uint8 = 3
	ArrKindGeneric uint8 = 4
)

func (t Type) ExpectedArrKind() uint8 {
	switch t {
	case TArrInt:
		return ArrKindInt
	case TArrFloat:
		return ArrKindFloat
	case TArrBool:
		return ArrKindBool
	case TArrGeneric:
		return ArrKindGeneric
	default:
		return ArrKindEmpty
	}
}

func (t Type) ElemType() Type {
	switch t {
	case TArrInt:
		return TInt
	case TArrFloat:
		return TFloat
	case TArrBool:
		return TBool
	default:
		return TUnknown
	}
}

// ValID uniquely identifies an SSA instruction result within a LoopRegion.
type ValID int32

const NoVal ValID = -1

// Op identifies a typed SSA IR operation.
type Op uint8

const (
	OpNop Op = iota

	// Preheader constants, unboxing, guards, and hoisting (failure branches cleanly to baseline entry).
	OpConstInt       // ImmI = int64
	OpConstFloat     // ImmF = float64 (bits in ImmU)
	OpConstBool      // ImmI = 0 or 1
	OpUnboxInt       // Slot = Lua reg; guard tag == tagInt
	OpUnboxFloat     // Slot = Lua reg; guard tag == tagFloat
	OpUnboxNumFloat  // Slot = Lua reg; guard tagFloat OR tagInt->float64 (exact for widening accumulators)
	OpUnboxBool      // Slot = Lua reg; guard tagFalse/tagTrue -> 0/1
	OpGuardTableArr  // Slot = Lua reg (or UpvalIdx >= 0 if from upval); Type = TArrInt/Float/Bool/Generic; yields arrBase (AuxLen holds arrLen in separate Val)
	OpLoadArrLen     // Args[0] = OpGuardTableArr ValID; yields arrLen (TInt)
	OpGuardRange     // Args[0] = arrVal, Args[1] = baseOff (NoVal if 0); ImmI = minDelta, ImmI2 = maxDelta (proven safe for entire counted loop!)
	OpGuardUpvalInt  // AuxIdx = upval idx; guard tagInt
	OpGuardUpvalFlt  // AuxIdx = upval idx; guard tagFloat or int->float
	OpGuardUpvalSqrt // AuxIdx = upval idx; guard staticNative == sqrtIdx
	OpGuardUpvalAbs  // AuxIdx = upval idx; guard staticNative == absIdx
	OpGuardRegSqrt   // Slot = Lua reg; guard staticNative == sqrtIdx
	OpGuardRegAbs    // Slot = Lua reg; guard staticNative == absIdx
	OpGuardCalleePro // Slot or UpvalIdx; guard tagClosure && cl.protoIdx == AuxU32 (for leaf inlining)
	OpLoopIdx        // Initial value of loop induction variable (R[forBase+2])
	OpLoopStep       // Loop step (R[forBase+1])
	OpLoopCnt        // Initial remaining count (R[forBase])

	// Loop-body virtual register access (for mutable loop-carried slots).
	OpGetVar // AuxIdx = index into LoopRegion.Vars
	OpSetVar // AuxIdx = index into LoopRegion.Vars, Args[0] = new value

	// Integer arithmetic & bitwise (unboxed 64-bit, exact Lua 5.5 semantics).
	OpAddI
	OpSubI
	OpMulI
	OpIDivI // Lua floor division (deopts on divisor == 0)
	OpModI  // Lua floor modulo (deopts on divisor == 0)
	OpNegI
	OpBNotI
	OpAndI
	OpOrI
	OpXorI
	OpShlI // Lua shift semantics (negative shifts reverse, |s|>=64 -> 0)
	OpShrI
	OpAddIImm // Args[0] + ImmI
	OpIntToFloat

	// Float64 arithmetic & intrinsics (strict IEEE-754, non-FMA for bit-exact parity).
	OpAddF
	OpSubF
	OpMulF
	OpDivF
	OpNegF
	OpSqrtF
	OpAbsF
	OpAbsI

	// Comparisons (produce TBool 0/1 or drive TermCondBr).
	OpCmpI // Cond in AuxCond; Args[0], Args[1]
	OpCmpF // Cond in AuxCond; Args[0], Args[1] (NaN-safe!)
	OpNotB // !Args[0]

	// Unboxed array loads & stores.
	// When BCE == true, preheader OpGuardRange already proved 1 <= idx <= arrLen -> ZERO bounds check!
	// When BCE == false, emitter checks uint64(idx-1) < arrLen and deopts on OOB.
	OpLoadArrInt   // Args[0] = arrVal, Args[1] = 1-based idx (TInt)
	OpLoadArrFloat // Args[0] = arrVal, Args[1] = 1-based idx (TInt)
	OpLoadArrBool  // Args[0] = arrVal, Args[1] = 1-based idx (TInt)
	OpLoadArrTable // Args[0] = arrGenericVal, Args[1] = 1-based idx (TInt); guard elem tag == tagTable -> *Table
	OpStoreArrInt  // Args[0] = arrVal, Args[1] = 1-based idx, Args[2] = val (TInt)
	OpStoreArrFlt  // Args[0] = arrVal, Args[1] = 1-based idx, Args[2] = val (TFloat)
	OpStoreArrBool // Args[0] = arrVal, Args[1] = 1-based idx, Args[2] = val (TBool)

	// Shape-guarded object field access (e.g. NBody bj.x / bj.vx with CSE across all 7 fields!).
	OpGuardShape     // Args[0] = TTable (*Table); AuxU32 = expected shapeRef; yields slotsBase pointer (TShapeObj)
	OpLoadSlotFloat  // Args[0] = TShapeObj (slotsBase); AuxIdx = slotIdx; guard tagFloat (or int->float)
	OpLoadSlotInt    // Args[0] = TShapeObj (slotsBase); AuxIdx = slotIdx; guard tagInt
	OpStoreSlotFloat // Args[0] = TShapeObj (slotsBase); AuxIdx = slotIdx; Args[1] = TFloat (0 GC barrier!)
	OpStoreSlotInt   // Args[0] = TShapeObj (slotsBase); AuxIdx = slotIdx; Args[1] = TInt (0 GC barrier!)
)

// CmpCond identifies an integer or floating-point comparison predicate.
type CmpCond uint8

const (
	CmpEQ CmpCond = iota
	CmpNE
	CmpLT
	CmpLE
	CmpGT
	CmpGE
)

func (c CmpCond) Invert() CmpCond {
	switch c {
	case CmpEQ:
		return CmpNE
	case CmpNE:
		return CmpEQ
	case CmpLT:
		return CmpGE
	case CmpLE:
		return CmpGT
	case CmpGT:
		return CmpLE
	case CmpGE:
		return CmpLT
	default:
		return c
	}
}

// Inst is one SSA instruction in the preheader or a loop body block.
type Inst struct {
	ID       ValID
	Op       Op
	Type     Type
	Args     [3]ValID
	NumArgs  uint8
	Slot     uint8   // Lua register slot (for Unbox/Guard ops)
	UpvalIdx int16   // >= 0 when sourced from closure upvalue instead of Lua reg
	AuxIdx   int32   // Var index, slot index, or upval index
	AuxU32   uint32  // Expected shapeRef or protoIdx
	AuxCond  CmpCond // Comparison condition
	ImmI     int64   // Integer immediate or minDelta
	ImmI2    int64   // Secondary immediate (maxDelta)
	ImmU     uint64  // Float64 bit pattern for OpConstFloat
	BCE      bool    // Bounds-check eliminated by preheader OpGuardRange
	Dead     bool    // Eliminated by DCE
	DeoptPC  int     // Bytecode PC to resume at in baseline on mid-loop guard failure
}

// LoopVar describes a mutable Lua register carried across loop iterations or blocks.
type LoopVar struct {
	Slot          uint8 // Lua register slot
	Type          Type  // TInt, TFloat, or TBool
	InitVal       ValID // Preheader SSA value initializing this var
	LiveOut       bool  // Must be written back to regs[Slot] on loop exit
	Written       bool  // Written inside the loop body
	AcceptIntInit bool  // For TFloat accumulators initialized to int 0 (e.g. local sum = 0)
	IsForIdx      bool  // True for R[forBase+2] loop induction variable
}

// TermKind describes how a loop body basic block terminates.
type TermKind uint8

const (
	TermJump     TermKind = iota // Unconditional jump to Succ[0]
	TermCondBr                   // Branch on CondVal (TBool) or CmpOp: true -> Succ[0], false -> Succ[1]
	TermBackedge                 // End of loop iteration -> advance counter & branch to header or exit
	TermExit                     // Early exit (e.g. break) -> materialize LiveOut & jump to ExitTargetPC
)

// Term is a basic block terminator.
type Term struct {
	Kind         TermKind
	CondVal      ValID  // TBool value for TermCondBr (or NoVal if fused Cmp)
	Succ         [2]int // Target block indices inside LoopRegion.Blocks
	ExitTargetPC int    // Baseline bytecode PC for TermExit
	DeoptPC      int    // Associated bytecode PC
}

// Block is a basic block inside a LoopRegion body.
type Block struct {
	ID      int
	StartPC int
	Insts   []Inst
	Term    Term
}

// LoopKind distinguishes counted numeric for loops from general scalar while/repeat loops.
type LoopKind uint8

const (
	LoopForInt LoopKind = iota
	LoopWhile
)

// AffineIdx represents an index expression `BaseOff + LoopIdx + Delta` (stride +1).
type AffineIdx struct {
	Valid   bool
	BaseOff ValID // NoVal if 0, else loop-invariant TInt ValID
	Delta   int64 // Constant integer delta
}

// AliasCheck requires runtime verification in the preheader that two hoisted arrays do not
// overlap in a loop-carried direction before entering the SIMD vector loop.
type AliasCheck struct {
	DstArr ValID
	DstOff ValID // NoVal if 0
	DstDel int64
	SrcArr ValID
	SrcOff ValID // NoVal if 0
	SrcDel int64
}

// VecSplat broadcasts a loop-invariant scalar ValID into a 128-bit vector register.
type VecSplat struct {
	ScalarVal ValID
	IsFloat   bool
	VReg      int // Virtual vector register index
}

// VecPtr tracks a contiguous stride-1 array pointer advanced by 16 bytes per SIMD iteration.
type VecPtr struct {
	ArrVal  ValID
	BaseOff ValID // NoVal if 0
	Delta   int64
	IsFloat bool
	IsWrite bool
	PtrIdx  int // Index into VecPlan.Ptrs
}

// VecOp identifies a 2-wide (128-bit) SIMD instruction in VecPlan.Body.
type VecOp uint8

const (
	VLoadF64 VecOp = iota
	VLoadI64
	VStoreF64
	VStoreI64
	VAddF64
	VSubF64
	VMulF64
	VDivF64
	VNegF64
	VSqrtF64
	VAbsF64
	VAddI64
	VSubI64
	VAndI64
	VOrI64
	VXorI64
	VAccumF64 // Strict left-to-right ordered scalar accumulation: LoopVar += lane0; LoopVar += lane1
	VAccumI64 // Associative 2-wide int64 vector accumulation: VecAcc += vecVal
)

// VecInst is one 2-wide vector instruction operating on virtual vector registers.
type VecInst struct {
	Op     VecOp
	Dst    int // Destination virtual vector reg (or -1 for stores / VAccumF64)
	Src1   int // Source virtual vector reg (or PtrIdx for VLoad/VStore)
	Src2   int // Second source virtual vector reg (or data vreg for VStore)
	VarIdx int // LoopRegion.Vars index for VAccumF64 / VAccumI64
	AccIdx int // Index into VecPlan.IntAccums for VAccumI64
}

// VecIntAccum binds a 2-wide int64 vector accumulator register to its scalar LoopVar.
type VecIntAccum struct {
	VarIdx int
	VReg   int
}

// VecPlan describes the 2-wide SIMD vector main loop (with scalar optimized tail epilogue)
// generated by the auto-vectorizer for a single-block counted stride-1 loop.
type VecPlan struct {
	Width       int // 2 (128-bit: 2x float64 or 2x int64)
	NumVRegs    int
	AliasChecks []AliasCheck
	Splats      []VecSplat
	Ptrs        []VecPtr
	IntAccums   []VecIntAccum
	Body        []VecInst
}

// RegAlloc holds linear-scan physical register assignments for a LoopRegion.
// Phys >= 0 is a target physical register index (GPR or FP/SIMD depending on Type);
// Spill >= 0 is a slot index into jitExecCtx.optSpill (0..15).
type RegLoc struct {
	IsFP  bool
	Phys  int8 // physical reg index, or -1 if spilled
	Spill int8 // spill slot 0..15, or -1 if in register
}

// AllocResult stores physical register assignments for every ValID and LoopVar.
type AllocResult struct {
	ValLoc     []RegLoc // indexed by ValID
	VarLoc     []RegLoc // indexed by LoopVar index
	LenLoc     []RegLoc // optional arrLen GPR for OpGuardTableArr values that need dynamic bounds
	UsedSpills int
}

// LoopRegion is the complete optimized IR for one natural loop inside a *bytecode.Proto.
type LoopRegion struct {
	Kind         LoopKind
	EntryPC      int
	BodyPC       int
	EndPC        int
	ExitPC       int
	ForBase      uint8
	HasConstStep bool
	ConstStep    int64
	Preheader    []Inst
	Vars         []LoopVar
	Blocks       []*Block
	NextVal      ValID
	Vec          *VecPlan
	Alloc        *AllocResult

	// Internal lookup maps used during building/optimization.
	Affine map[ValID]AffineIdx
}

func (r *LoopRegion) newVal() ValID {
	id := r.NextVal
	r.NextVal++
	return id
}

// FieldFeedback captures a warmed monomorphic Shape IC for one GETFIELD/SETFIELD PC.
type FieldFeedback struct {
	ShapeRef uint32
	Slot     uint16
}

// InlineTarget captures a small pure leaf Lua callee observed at an OP_CALL PC.
type InlineTarget struct {
	ProtoIdx  uint32
	Proto     *bytecode.Proto
	NumParams int
}

// Feedback provides optional runtime type, shape IC, and callee feedback for Tier-2 recompilation.
type Feedback struct {
	EnableSIMD  bool
	ArrKinds    map[uint8]uint8       // Lua reg slot -> observed arrKind (optional)
	UpvalArrs   map[int]uint8         // upval index -> observed arrKind (optional)
	FieldICs    map[int]FieldFeedback // bytecode PC -> monomorphic shape/slot IC
	InlineCalls map[int]*InlineTarget // bytecode PC -> small pure leaf callee
}

// OptimizeProto discovers, builds, optimizes, optionally vectorizes, and register-allocates
// all eligible inner loops in p. Returns a map keyed by LoopRegion.EntryPC.
func OptimizeProto(p *bytecode.Proto, fb *Feedback, numGPR, numFP, maxSpills int) map[int]*LoopRegion {
	cands := DiscoverLoops(p)
	if len(cands) == 0 {
		return nil
	}
	enableSIMD := true
	if fb != nil {
		enableSIMD = fb.EnableSIMD
	}
	out := make(map[int]*LoopRegion, len(cands))
	for _, cand := range cands {
		reg := BuildLoopRegion(p, cand, fb)
		if reg == nil {
			continue
		}
		if !OptimizeRegion(reg) {
			continue
		}
		if enableSIMD {
			VectorizeRegion(reg)
		}
		alloc := AllocateRegisters(reg, numGPR, numFP, maxSpills)
		if alloc == nil {
			continue
		}
		reg.Alloc = alloc
		out[reg.EntryPC] = reg
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (r *LoopRegion) String() string {
	return fmt.Sprintf("LoopRegion{kind=%d entry=%d body=%d end=%d vars=%d pre=%d blocks=%d vec=%v}",
		r.Kind, r.EntryPC, r.BodyPC, r.EndPC, len(r.Vars), len(r.Preheader), len(r.Blocks), r.Vec != nil)
}
