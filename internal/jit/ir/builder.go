package ir

import (
	"math"

	"github.com/yuchanns/moelua/bytecode"
)

// BuildLoopRegion constructs a typed SSA LoopRegion from a discovered LoopCandidate.
// Returns nil if the loop contains unsupported operations or cannot be cleanly typed.
func BuildLoopRegion(p *bytecode.Proto, cand LoopCandidate, fb *Feedback) *LoopRegion {
	b := &loopBuilder{
		p:          p,
		code:       p.Code,
		cand:       cand,
		fb:         fb,
		slotType:   make(map[uint8]Type, 16),
		upvalType:  make(map[int]Type, 8),
		arrKindMap: make(map[uint8]Type, 8),
		upvArrKind: make(map[int]Type, 4),
		written:    make(map[uint8]bool, 16),
		readInLoop: make(map[uint8]bool, 16),
		liveOut:    make(map[uint8]bool, 16),
		invarVal:   make(map[uint8]ValID, 16),
		upvalVal:   make(map[int]ValID, 8),
		arrHoist:   make(map[arrKey]ValID, 8),
		arrLenVal:  make(map[ValID]ValID, 8),
		varIdx:     make(map[uint8]int, 16),
	}
	return b.build()
}

type arrKey struct {
	isUpval bool
	idx     int
	typ     Type
}

type loopBuilder struct {
	p    *bytecode.Proto
	code []bytecode.Instruction
	cand LoopCandidate
	fb   *Feedback

	reg *LoopRegion

	// Static + feedback type inference maps
	slotType   map[uint8]Type
	upvalType  map[int]Type
	arrKindMap map[uint8]Type // Lua reg holding table -> TArrInt/Float/Bool/Generic
	upvArrKind map[int]Type   // Upval holding table -> TArrInt/Float/Bool/Generic
	protoFloat bool           // Heuristic hint: proto uses floating-point arithmetic

	written    map[uint8]bool
	readInLoop map[uint8]bool
	liveOut    map[uint8]bool

	// Preheader hoisted values & LoopVar index
	invarVal  map[uint8]ValID
	upvalVal  map[int]ValID
	arrHoist  map[arrKey]ValID
	arrLenVal map[ValID]ValID
	varIdx    map[uint8]int

	forIdxVal  ValID
	forStepVal ValID
	forCntVal  ValID

	// Per-block current SSA map & shape CSE cache
	curSlot   map[uint8]ValID
	curShapes map[ValID]shapeCacheEntry
	failed    bool
}

type shapeCacheEntry struct {
	shapeRef uint32
	objVal   ValID
}

func (b *loopBuilder) build() *LoopRegion {
	// 1. Validate all opcodes in [BodyPC, EndPC-1] and gather read/written slots + proto-level float hints.
	if !b.scanAndInferTypes() {
		return nil
	}

	// 2. Partition [BodyPC, EndPC-1] into basic blocks.
	blockStarts := b.findBlockStarts()
	if len(blockStarts) == 0 || len(blockStarts) > 16 {
		return nil
	}
	pcToBlock := make(map[int]int, len(blockStarts))
	for i, pc := range blockStarts {
		pcToBlock[pc] = i
	}

	// 3. Allocate LoopRegion and preheader loop-control / mutable LoopVars.
	b.reg = &LoopRegion{
		Kind:         b.cand.Kind,
		EntryPC:      b.cand.EntryPC,
		BodyPC:       b.cand.BodyPC,
		EndPC:        b.cand.EndPC,
		ExitPC:       b.cand.ExitPC,
		ForBase:      b.cand.ForBase,
		HasConstStep: b.cand.HasConstStep,
		ConstStep:    b.cand.ConstStep,
		Affine:       make(map[ValID]AffineIdx, 16),
	}

	if b.cand.Kind == LoopForInt {
		b.forCntVal = b.emitPre(Inst{Op: OpLoopCnt, Type: TInt})
		b.forStepVal = b.emitPre(Inst{Op: OpLoopStep, Type: TInt})
		b.forIdxVal = b.emitPre(Inst{Op: OpLoopIdx, Type: TInt})
		idxSlot := b.cand.ForBase + 2
		b.varIdx[idxSlot] = len(b.reg.Vars)
		b.reg.Vars = append(b.reg.Vars, LoopVar{
			Slot:     idxSlot,
			Type:     TInt,
			InitVal:  b.forIdxVal,
			LiveOut:  false,
			Written:  true,
			IsForIdx: true,
		})
	}

	// Create LoopVars for all slots written inside the loop that are either read before write
	// (loop-carried) or live across blocks or live out after the loop.
	singleBlock := len(blockStarts) == 1
	for slot := uint8(0); slot < uint8(b.p.MaxStackSize); slot++ {
		if !b.written[slot] {
			continue
		}
		if b.cand.Kind == LoopForInt && slot >= b.cand.ForBase && slot <= b.cand.ForBase+2 {
			// Loop control registers must not be overwritten inside body (except internal FORLOOP).
			return nil
		}
		// In a single-block loop, a slot that is written before any read and not live-out is
		// purely a block-local SSA temporary (not a loop-carried mutable LoopVar).
		needsInit := b.needsPreheaderInit(slot)
		carried := needsInit || b.liveOut[slot] || !singleBlock
		if !carried {
			continue
		}
		t := b.slotType[slot]
		if t != TInt && t != TFloat && t != TBool {
			if b.liveOut[slot] || !singleBlock {
				return nil
			}
			continue
		}
		var initVal ValID
		acceptInt := false
		if needsInit {
			switch t {
			case TInt:
				initVal = b.emitPre(Inst{Op: OpUnboxInt, Type: TInt, Slot: slot})
			case TFloat:
				// Only allow int->float preheader widening when the single loop body is
				// guaranteed to execute at least once and unconditionally perform the write.
				// Conditional or while-loop variables require strict tagFloat on entry so an
				// untouched Lua integer never loses precision or changes math.type().
				if b.cand.Kind == LoopForInt && singleBlock {
					acceptInt = true
					initVal = b.emitPre(Inst{Op: OpUnboxNumFloat, Type: TFloat, Slot: slot})
				} else {
					initVal = b.emitPre(Inst{Op: OpUnboxFloat, Type: TFloat, Slot: slot})
				}
			case TBool:
				initVal = b.emitPre(Inst{Op: OpUnboxBool, Type: TBool, Slot: slot})
			}
		} else {
			// Pure temp written before read on every iteration; zero-init so register is defined.
			switch t {
			case TInt:
				initVal = b.emitPre(Inst{Op: OpConstInt, Type: TInt, ImmI: 0})
			case TFloat:
				initVal = b.emitPre(Inst{Op: OpConstFloat, Type: TFloat, ImmU: 0})
			case TBool:
				initVal = b.emitPre(Inst{Op: OpConstBool, Type: TBool, ImmI: 0})
			}
		}
		b.varIdx[slot] = len(b.reg.Vars)
		b.reg.Vars = append(b.reg.Vars, LoopVar{
			Slot:          slot,
			Type:          t,
			InitVal:       initVal,
			LiveOut:       b.liveOut[slot],
			Written:       true,
			AcceptIntInit: acceptInt,
			ZeroInit:      !needsInit,
		})
	}

	// 4. Lower each basic block into SSA instructions.
	b.reg.Blocks = make([]*Block, len(blockStarts))
	for bi, startPC := range blockStarts {
		endPC := b.cand.EndPC
		if bi+1 < len(blockStarts) {
			endPC = blockStarts[bi+1]
		}
		blk := &Block{ID: bi, StartPC: startPC}
		b.reg.Blocks[bi] = blk
		if !b.lowerBlock(blk, startPC, endPC, pcToBlock) || b.failed {
			return nil
		}
	}

	return b.reg
}

func (b *loopBuilder) emitPre(in Inst) ValID {
	in.ID = b.reg.newVal()
	in.UpvalIdx = -1
	b.reg.Preheader = append(b.reg.Preheader, in)
	return in.ID
}

func (b *loopBuilder) emitPreRaw(in Inst) ValID {
	in.ID = b.reg.newVal()
	b.reg.Preheader = append(b.reg.Preheader, in)
	return in.ID
}

func (b *loopBuilder) emitBody(blk *Block, in Inst) ValID {
	in.ID = b.reg.newVal()
	in.UpvalIdx = -1
	blk.Insts = append(blk.Insts, in)
	return in.ID
}

// scanAndInferTypes validates every opcode in the loop body, records written/read/live-out
// slots, and runs fixed-point type inference over slots and array kinds.
func (b *loopBuilder) scanAndInferTypes() bool {
	// Quick proto-wide float hint (checks if proto uses any float constants or OP_DIV).
	for _, k := range b.p.Consts {
		if k.Kind == bytecode.ConstFloat {
			b.protoFloat = true
			break
		}
	}
	if !b.protoFloat {
		for _, w := range b.code {
			op := w.Op()
			if op == bytecode.OP_LOADF || op == bytecode.OP_DIV || op == bytecode.OP_DIVK {
				b.protoFloat = true
				break
			}
		}
	}

	// Record initial pre-loop constant/load types from straight-line code immediately preceding EntryPC.
	b.seedPreLoopTypes()

	// Seed for-loop control variable type.
	if b.cand.Kind == LoopForInt {
		b.slotType[b.cand.ForBase+2] = TInt
	}

	// Seed feedback array kinds if supplied.
	if b.fb != nil {
		for slot, ak := range b.fb.ArrKinds {
			if t := arrKindToType(ak); t != TUnknown {
				b.arrKindMap[slot] = t
				b.slotType[slot] = t
			}
		}
		for uv, ak := range b.fb.UpvalArrs {
			if t := arrKindToType(ak); t != TUnknown {
				b.upvArrKind[uv] = t
				b.upvalType[uv] = t
			}
		}
	}

	// Validate opcodes in [BodyPC, EndPC-1] and gather written/read sets.
	for pc := b.cand.BodyPC; pc < b.cand.EndPC; pc++ {
		w := b.code[pc]
		op := w.Op()
		if isMMBinOp(op) {
			continue
		}
		if pc > b.cand.BodyPC && isCompareOrTestOp(b.code[pc-1].Op()) && op == bytecode.OP_JMP {
			continue
		}
		a := uint8(w.A())
		switch op {
		case bytecode.OP_MOVE:
			b.readInLoop[uint8(w.B())] = true
			b.written[a] = true
		case bytecode.OP_LOADI, bytecode.OP_LOADF, bytecode.OP_LOADK,
			bytecode.OP_LOADFALSE, bytecode.OP_LOADTRUE:
			b.written[a] = true
		case bytecode.OP_GETUPVAL:
			b.written[a] = true
		case bytecode.OP_ADDI, bytecode.OP_ADDK, bytecode.OP_SUBK, bytecode.OP_MULK,
			bytecode.OP_DIVK, bytecode.OP_IDIVK, bytecode.OP_MODK,
			bytecode.OP_BANDK, bytecode.OP_BORK, bytecode.OP_BXORK,
			bytecode.OP_SHLI, bytecode.OP_SHRI, bytecode.OP_UNM, bytecode.OP_BNOT, bytecode.OP_NOT:
			b.readInLoop[uint8(w.B())] = true
			b.written[a] = true
		case bytecode.OP_ADD, bytecode.OP_SUB, bytecode.OP_MUL, bytecode.OP_DIV,
			bytecode.OP_IDIV, bytecode.OP_MOD, bytecode.OP_BAND, bytecode.OP_BOR,
			bytecode.OP_BXOR, bytecode.OP_SHL, bytecode.OP_SHR:
			b.readInLoop[uint8(w.B())] = true
			b.readInLoop[uint8(w.C())] = true
			b.written[a] = true
		case bytecode.OP_GETI:
			if w.C() == 0 {
				return false
			}
			b.readInLoop[uint8(w.B())] = true
			b.written[a] = true
		case bytecode.OP_GETTABLE:
			b.readInLoop[uint8(w.B())] = true
			b.readInLoop[uint8(w.C())] = true
			b.written[a] = true
		case bytecode.OP_SETI:
			if w.B() == 0 {
				return false
			}
			b.readInLoop[a] = true
			if !w.K() {
				b.readInLoop[uint8(w.C())] = true
			}
		case bytecode.OP_SETTABLE:
			b.readInLoop[a] = true
			b.readInLoop[uint8(w.B())] = true
			if !w.K() {
				b.readInLoop[uint8(w.C())] = true
			}
		case bytecode.OP_GETFIELD:
			if b.fb == nil {
				return false
			}
			if _, ok := b.fb.FieldICs[pc]; !ok {
				return false
			}
			b.readInLoop[uint8(w.B())] = true
			b.written[a] = true
		case bytecode.OP_SETFIELD:
			if b.fb == nil {
				return false
			}
			if _, ok := b.fb.FieldICs[pc]; !ok {
				return false
			}
			b.readInLoop[a] = true
			if !w.K() {
				b.readInLoop[uint8(w.C())] = true
			}
		case bytecode.OP_CALL:
			// Allow either recognized math.sqrt/math.abs intrinsic (B==2, C==2) OR
			// an explicit small pure leaf Lua inline target from feedback.
			bArg, cArg := w.B(), w.C()
			if cArg != 2 || bArg < 1 {
				return false
			}
			if !b.isSupportedCall(pc, a, bArg) {
				return false
			}
			for k := 0; k < bArg; k++ {
				b.readInLoop[a+uint8(k)] = true
			}
			b.written[a] = true
		case bytecode.OP_EQ, bytecode.OP_LT, bytecode.OP_LE:
			b.readInLoop[a] = true
			b.readInLoop[uint8(w.B())] = true
			if pc+1 >= b.cand.EndPC || b.code[pc+1].Op() != bytecode.OP_JMP {
				return false
			}
		case bytecode.OP_EQK, bytecode.OP_EQI, bytecode.OP_LTI, bytecode.OP_LEI,
			bytecode.OP_GTI, bytecode.OP_GEI, bytecode.OP_TEST:
			b.readInLoop[a] = true
			if pc+1 >= b.cand.EndPC || b.code[pc+1].Op() != bytecode.OP_JMP {
				return false
			}
		case bytecode.OP_JMP:
			// Forward unconditional jump (e.g. else skip or break).
			target := pc + 1 + w.SJ()
			if target <= pc || target > len(b.code) {
				return false
			}
		default:
			return false
		}
	}

	// Reject loops that write to any table slot they also use as a hoisted array (can't hoist mutated table variable).
	// Compute live-out set conservatively: any written slot < ForBase (for LoopForInt) or read after ExitPC.
	b.computeLiveOut()

	// Run fixed-point type inference over the loop body (max 6 passes).
	for pass := 0; pass < 6; pass++ {
		changed := false
		for pc := b.cand.BodyPC; pc < b.cand.EndPC; pc++ {
			w := b.code[pc]
			op := w.Op()
			if isMMBinOp(op) || (pc > b.cand.BodyPC && isCompareOrTestOp(b.code[pc-1].Op()) && op == bytecode.OP_JMP) {
				continue
			}
			a := uint8(w.A())
			switch op {
			case bytecode.OP_GETUPVAL:
				if ak := b.upvArrKind[w.B()]; ak != TUnknown {
					if b.setArrKind(a, ak) {
						changed = true
					}
				}
			case bytecode.OP_MOVE:
				if t := b.slotType[uint8(w.B())]; t != TUnknown {
					if b.mergeSlotType(a, t) {
						changed = true
					}
				}
			case bytecode.OP_LOADI:
				if b.mergeSlotType(a, TInt) {
					changed = true
				}
			case bytecode.OP_LOADF:
				if b.mergeSlotType(a, TFloat) {
					changed = true
				}
			case bytecode.OP_LOADTRUE, bytecode.OP_LOADFALSE:
				if b.mergeSlotType(a, TBool) {
					changed = true
				}
			case bytecode.OP_LOADK:
				k := b.p.Consts[w.Bx()]
				if k.Kind == bytecode.ConstInt && b.mergeSlotType(a, TInt) {
					changed = true
				} else if k.Kind == bytecode.ConstFloat && b.mergeSlotType(a, TFloat) {
					changed = true
				}
			case bytecode.OP_DIV, bytecode.OP_DIVK:
				if b.mergeSlotType(a, TFloat) {
					changed = true
				}
			case bytecode.OP_BAND, bytecode.OP_BOR, bytecode.OP_BXOR, bytecode.OP_SHL, bytecode.OP_SHR,
				bytecode.OP_BANDK, bytecode.OP_BORK, bytecode.OP_BXORK, bytecode.OP_SHLI, bytecode.OP_SHRI,
				bytecode.OP_BNOT:
				if b.mergeSlotType(a, TInt) {
					changed = true
				}
				if b.mergeSlotType(uint8(w.B()), TInt) {
					changed = true
				}
				if op == bytecode.OP_BAND || op == bytecode.OP_BOR || op == bytecode.OP_BXOR || op == bytecode.OP_SHL || op == bytecode.OP_SHR {
					if b.mergeSlotType(uint8(w.C()), TInt) {
						changed = true
					}
				}
			case bytecode.OP_NOT:
				if b.mergeSlotType(a, TBool) {
					changed = true
				}
			case bytecode.OP_ADDI, bytecode.OP_UNM:
				bt := b.slotType[uint8(w.B())]
				if bt == TFloat || bt == TInt {
					if b.mergeSlotType(a, bt) {
						changed = true
					}
				}
			case bytecode.OP_ADDK, bytecode.OP_SUBK, bytecode.OP_MULK, bytecode.OP_IDIVK, bytecode.OP_MODK:
				bt := b.slotType[uint8(w.B())]
				k := b.p.Consts[w.C()]
				kt := TUnknown
				if k.Kind == bytecode.ConstInt {
					kt = TInt
				} else if k.Kind == bytecode.ConstFloat {
					kt = TFloat
				}
				if rt := combineNumType(bt, kt); rt != TUnknown {
					if b.mergeSlotType(a, rt) {
						changed = true
					}
				}
			case bytecode.OP_ADD, bytecode.OP_SUB, bytecode.OP_MUL, bytecode.OP_IDIV, bytecode.OP_MOD:
				bSlot, cSlot := uint8(w.B()), uint8(w.C())
				bt := b.slotType[bSlot]
				ct := b.slotType[cSlot]
				if rt := combineNumType(bt, ct); rt != TUnknown {
					if b.mergeSlotType(a, rt) {
						changed = true
					}
				}
				// Backward propagation: if result is known TInt (e.g. array index `rowA + j`), both operands must be TInt!
				if b.slotType[a] == TInt {
					if b.mergeSlotType(bSlot, TInt) {
						changed = true
					}
					if b.mergeSlotType(cSlot, TInt) {
						changed = true
					}
				} else if b.slotType[a] == TFloat {
					if bt == TUnknown && ct == TFloat && b.mergeSlotType(bSlot, TFloat) {
						changed = true
					}
					if ct == TUnknown && bt == TFloat && b.mergeSlotType(cSlot, TFloat) {
						changed = true
					}
				}
			case bytecode.OP_GETI, bytecode.OP_GETTABLE:
				tblSlot := uint8(w.B())
				if op == bytecode.OP_GETTABLE {
					if b.mergeSlotType(uint8(w.C()), TInt) {
						changed = true
					}
				}
				if ak := b.arrKindMap[tblSlot]; ak != TUnknown {
					et := ak.ElemType()
					if ak == TArrGeneric {
						et = TTable
					}
					if et != TUnknown && b.mergeSlotType(a, et) {
						changed = true
					}
				} else if !(op == bytecode.OP_GETTABLE && a == uint8(w.C())) {
					if at := b.slotType[a]; at == TInt || at == TFloat || at == TBool || at == TTable {
						if b.setArrKind(tblSlot, elemToArrType(at)) {
							changed = true
						}
					}
				}
			case bytecode.OP_SETI, bytecode.OP_SETTABLE:
				tblSlot := a
				if op == bytecode.OP_SETTABLE {
					if b.mergeSlotType(uint8(w.B()), TInt) {
						changed = true
					}
				}
				vt := b.rhsType(w)
				if vt == TInt || vt == TFloat || vt == TBool {
					if b.setArrKind(tblSlot, elemToArrType(vt)) {
						changed = true
					}
				}
			case bytecode.OP_GETFIELD:
				if b.mergeSlotType(uint8(w.B()), TTable) {
					changed = true
				}
				if b.protoFloat {
					if b.mergeSlotType(a, TFloat) {
						changed = true
					}
				}
			case bytecode.OP_SETFIELD:
				if b.mergeSlotType(a, TTable) {
					changed = true
				}
			case bytecode.OP_TEST:
				if b.slotType[a] == TUnknown {
					if b.mergeSlotType(a, TBool) {
						changed = true
					}
				}
			case bytecode.OP_CALL:
				if b.fb != nil {
					if tgt, ok := b.fb.InlineCalls[pc]; ok && tgt != nil {
						if rt := inferLeafReturnType(tgt.Proto); rt != TUnknown && b.mergeSlotType(a, rt) {
							changed = true
						}
						continue
					}
				}
				// math.sqrt always returns float64; math.abs returns float64 on float or int on int.
				if b.mergeSlotType(a, TFloat) {
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}

	// Default any still-unknown array kinds using proto-wide float hint (arrFloat if proto uses floats, else arrInt).
	for pc := b.cand.BodyPC; pc < b.cand.EndPC; pc++ {
		w := b.code[pc]
		switch w.Op() {
		case bytecode.OP_GETI, bytecode.OP_GETTABLE:
			tbl := uint8(w.B())
			if b.arrKindMap[tbl] == TUnknown {
				def := TArrInt
				if b.protoFloat {
					def = TArrFloat
				}
				b.arrKindMap[tbl] = def
				if b.slotType[uint8(w.A())] == TUnknown {
					b.slotType[uint8(w.A())] = def.ElemType()
				}
			}
		case bytecode.OP_SETI, bytecode.OP_SETTABLE:
			tbl := uint8(w.A())
			if b.arrKindMap[tbl] == TUnknown {
				def := TArrInt
				if b.protoFloat {
					def = TArrFloat
				}
				b.arrKindMap[tbl] = def
			}
		}
	}

	// One final propagation pass after defaulting unknown arrays.
	for pass := 0; pass < 3; pass++ {
		for pc := b.cand.BodyPC; pc < b.cand.EndPC; pc++ {
			w := b.code[pc]
			a := uint8(w.A())
			switch w.Op() {
			case bytecode.OP_GETUPVAL:
				if ak := b.upvArrKind[w.B()]; ak != TUnknown {
					b.setArrKind(a, ak)
				}
			case bytecode.OP_MOVE:
				if t := b.slotType[uint8(w.B())]; t != TUnknown {
					b.mergeSlotType(a, t)
				}
			case bytecode.OP_GETI, bytecode.OP_GETTABLE:
				if ak := b.arrKindMap[uint8(w.B())]; ak != TUnknown {
					et := ak.ElemType()
					if ak == TArrGeneric {
						et = TTable
					}
					if et != TUnknown {
						b.mergeSlotType(a, et)
					}
				}
			case bytecode.OP_ADD, bytecode.OP_SUB, bytecode.OP_MUL, bytecode.OP_IDIV, bytecode.OP_MOD:
				if rt := combineNumType(b.slotType[uint8(w.B())], b.slotType[uint8(w.C())]); rt != TUnknown {
					b.mergeSlotType(a, rt)
				}
			case bytecode.OP_ADDI, bytecode.OP_UNM:
				if bt := b.slotType[uint8(w.B())]; bt == TInt || bt == TFloat {
					b.mergeSlotType(a, bt)
				}
			case bytecode.OP_ADDK, bytecode.OP_SUBK, bytecode.OP_MULK:
				kt := TUnknown
				if k := b.p.Consts[w.C()]; k.Kind == bytecode.ConstInt {
					kt = TInt
				} else if k.Kind == bytecode.ConstFloat {
					kt = TFloat
				}
				if rt := combineNumType(b.slotType[uint8(w.B())], kt); rt != TUnknown {
					b.mergeSlotType(a, rt)
				}
			}
		}
	}
	return true
}

func (b *loopBuilder) seedPreLoopTypes() {
	// Scan instructions before EntryPC (linear seed for local initializers, outer for-loops & array stores).
	for pc := 0; pc < b.cand.EntryPC; pc++ {
		w := b.code[pc]
		a := uint8(w.A())
		switch w.Op() {
		case bytecode.OP_LOADI:
			b.slotType[a] = TInt
		case bytecode.OP_LOADF:
			b.slotType[a] = TFloat
		case bytecode.OP_LOADTRUE, bytecode.OP_LOADFALSE:
			b.slotType[a] = TBool
		case bytecode.OP_LOADK:
			k := b.p.Consts[w.Bx()]
			if k.Kind == bytecode.ConstInt {
				b.slotType[a] = TInt
			} else if k.Kind == bytecode.ConstFloat {
				b.slotType[a] = TFloat
			}
		case bytecode.OP_FORPREP:
			// Outer integer `for` loop index variable R[A+2] is TInt unless initialized from float.
			if b.slotType[a] != TFloat && b.slotType[a+1] != TFloat && b.slotType[a+2] != TFloat {
				b.slotType[a+2] = TInt
			}
		case bytecode.OP_DIV, bytecode.OP_DIVK:
			b.slotType[a] = TFloat
		case bytecode.OP_ADD, bytecode.OP_SUB, bytecode.OP_MUL, bytecode.OP_IDIV, bytecode.OP_MOD:
			if rt := combineNumType(b.slotType[uint8(w.B())], b.slotType[uint8(w.C())]); rt != TUnknown {
				b.slotType[a] = rt
			}
		case bytecode.OP_ADDI, bytecode.OP_UNM:
			if bt := b.slotType[uint8(w.B())]; bt == TInt || bt == TFloat {
				b.slotType[a] = bt
			}
		case bytecode.OP_ADDK, bytecode.OP_SUBK, bytecode.OP_MULK:
			kt := TUnknown
			if k := b.p.Consts[w.C()]; k.Kind == bytecode.ConstInt {
				kt = TInt
			} else if k.Kind == bytecode.ConstFloat {
				kt = TFloat
			}
			if rt := combineNumType(b.slotType[uint8(w.B())], kt); rt != TUnknown {
				b.slotType[a] = rt
			}
		case bytecode.OP_MOVE:
			if t := b.slotType[uint8(w.B())]; t != TUnknown {
				b.slotType[a] = t
			}
		case bytecode.OP_GETI, bytecode.OP_GETTABLE:
			if ak := b.arrKindMap[uint8(w.B())]; ak != TUnknown {
				if et := ak.ElemType(); et != TUnknown {
					b.slotType[a] = et
				}
			}
		case bytecode.OP_SETI, bytecode.OP_SETTABLE:
			vt := b.rhsType(w)
			if vt == TInt || vt == TFloat || vt == TBool {
				b.arrKindMap[a] = elemToArrType(vt)
			}
		}
	}
}

func (b *loopBuilder) isSupportedCall(pc int, a uint8, bArg int) bool {
	if b.fb != nil {
		if tgt, ok := b.fb.InlineCalls[pc]; ok && tgt != nil && canInlineLeafProto(tgt.Proto) {
			return bArg-1 == tgt.NumParams
		}
	}
	// Check if callee slot is loaded by a preceding GETUPVAL (e.g. `local sqrt = math.sqrt` or `abs`)
	// or is a loop-invariant register (only for 1-arg math.sqrt / math.abs: bArg == 2).
	if bArg != 2 {
		return false
	}
	// Accept if callee register is not written inside loop (invariant local) OR written ONLY by GETUPVAL.
	if !b.written[a] {
		return true
	}
	// Check if `a` is written inside loop ONLY by OP_GETUPVAL right before this CALL (e.g. `abs(x)`).
	if pc > b.cand.BodyPC && b.code[pc-1].Op() == bytecode.OP_GETUPVAL && uint8(b.code[pc-1].A()) == a {
		return true
	}
	if pc > b.cand.BodyPC+1 && b.code[pc-2].Op() == bytecode.OP_GETUPVAL && uint8(b.code[pc-2].A()) == a {
		return true
	}
	return false
}

func (b *loopBuilder) computeLiveOut() {
	// Mark any slot written inside the loop that is read anywhere after EntryPC (conservatively
	// scan [ExitPC, len(code)) plus outer loop ranges before EntryPC).
	// For LoopForInt, any slot < ForBase that is written is considered live-out; temps >= ForBase+3
	// are live-out only if read after ExitPC.
	readAfter := make(map[uint8]bool, 16)
	for pc := b.cand.ExitPC; pc < len(b.code); pc++ {
		w := b.code[pc]
		op := w.Op()
		if isMMBinOp(op) {
			continue
		}
		// Conservatively mark all operand registers as potentially read.
		switch op {
		case bytecode.OP_RETURN1:
			readAfter[uint8(w.A())] = true
		case bytecode.OP_RETURN:
			a := uint8(w.A())
			bArg := w.B()
			if bArg == 0 {
				for s := a; s < uint8(b.p.MaxStackSize); s++ {
					readAfter[s] = true
				}
			} else if bArg > 1 {
				for s := a; s < a+uint8(bArg-1); s++ {
					readAfter[s] = true
				}
			}
		case bytecode.OP_CALL, bytecode.OP_TAILCALL:
			a := uint8(w.A())
			bArg := w.B()
			if bArg == 0 {
				for s := a; s < uint8(b.p.MaxStackSize); s++ {
					readAfter[s] = true
				}
			} else {
				for s := a; s < a+uint8(bArg); s++ {
					readAfter[s] = true
				}
			}
		case bytecode.OP_LOADI, bytecode.OP_LOADF, bytecode.OP_LOADK, bytecode.OP_LOADKX,
			bytecode.OP_LOADTRUE, bytecode.OP_LOADFALSE, bytecode.OP_LFALSESKIP,
			bytecode.OP_LOADNIL, bytecode.OP_JMP, bytecode.OP_RETURN0,
			bytecode.OP_NEWTABLE, bytecode.OP_CLOSURE, bytecode.OP_EXTRAARG,
			bytecode.OP_VARARGPREP, bytecode.OP_GETUPVAL:
			// Writes A or no register reads (iABx/iAsBx/iAx fields are not register indices).
		case bytecode.OP_FORPREP, bytecode.OP_FORLOOP:
			a := uint8(w.A())
			readAfter[a] = true
			readAfter[a+1] = true
			readAfter[a+2] = true
		case bytecode.OP_TFORPREP, bytecode.OP_TFORLOOP, bytecode.OP_TFORCALL:
			a := uint8(w.A())
			for s := a; s <= a+3; s++ {
				readAfter[s] = true
			}
		case bytecode.OP_SETUPVAL, bytecode.OP_TEST, bytecode.OP_EQK, bytecode.OP_EQI,
			bytecode.OP_LTI, bytecode.OP_LEI, bytecode.OP_GTI, bytecode.OP_GEI:
			readAfter[uint8(w.A())] = true
		case bytecode.OP_EQ, bytecode.OP_LT, bytecode.OP_LE:
			readAfter[uint8(w.A())] = true
			readAfter[uint8(w.B())] = true
		case bytecode.OP_GETTABUP:
			// Reads upval B, const C; writes A.
		case bytecode.OP_SETTABUP:
			if !w.K() {
				readAfter[uint8(w.C())] = true
			}
		case bytecode.OP_SETI, bytecode.OP_SETFIELD:
			readAfter[uint8(w.A())] = true
			if !w.K() {
				readAfter[uint8(w.C())] = true
			}
		case bytecode.OP_SETTABLE:
			readAfter[uint8(w.A())] = true
			readAfter[uint8(w.B())] = true
			if !w.K() {
				readAfter[uint8(w.C())] = true
			}
		case bytecode.OP_MOVE, bytecode.OP_ADDI, bytecode.OP_ADDK, bytecode.OP_SUBK, bytecode.OP_MULK,
			bytecode.OP_DIVK, bytecode.OP_IDIVK, bytecode.OP_MODK, bytecode.OP_BANDK, bytecode.OP_BORK,
			bytecode.OP_BXORK, bytecode.OP_SHLI, bytecode.OP_SHRI, bytecode.OP_UNM, bytecode.OP_BNOT,
			bytecode.OP_NOT, bytecode.OP_GETI, bytecode.OP_GETFIELD:
			readAfter[uint8(w.B())] = true
		case bytecode.OP_ADD, bytecode.OP_SUB, bytecode.OP_MUL, bytecode.OP_DIV, bytecode.OP_IDIV,
			bytecode.OP_MOD, bytecode.OP_BAND, bytecode.OP_BOR, bytecode.OP_BXOR, bytecode.OP_SHL,
			bytecode.OP_SHR, bytecode.OP_GETTABLE:
			readAfter[uint8(w.B())] = true
			readAfter[uint8(w.C())] = true
		default:
			readAfter[uint8(w.A())] = true
			readAfter[uint8(w.B())] = true
			readAfter[uint8(w.C())] = true
		}
	}
	for slot := range b.written {
		if b.cand.Kind == LoopForInt {
			// In Lua 5.5, registers >= ForBase are scoped strictly inside the `for` loop body
			// and are never live after OP_FORLOOP exits.
			if slot < b.cand.ForBase {
				b.liveOut[slot] = true
			}
		} else if readAfter[slot] || b.cand.Kind == LoopWhile {
			b.liveOut[slot] = true
		}
	}
}

func (b *loopBuilder) needsPreheaderInit(slot uint8) bool {
	// If the first reference to slot in block 0 is a write (without reading slot on RHS),
	// and it's a single-block loop, it doesn't need preheader unboxing; otherwise unbox in preheader.
	for pc := b.cand.BodyPC; pc < b.cand.EndPC; pc++ {
		w := b.code[pc]
		op := w.Op()
		if isMMBinOp(op) {
			continue
		}
		if b.instReadsSlot(w, slot) {
			return true
		}
		if b.instWritesSlot(w, slot) {
			return false
		}
		if isCompareOrTestOp(op) || op == bytecode.OP_JMP {
			return true
		}
	}
	return true
}

func (b *loopBuilder) instReadsSlot(w bytecode.Instruction, slot uint8) bool {
	op := w.Op()
	switch op {
	case bytecode.OP_MOVE, bytecode.OP_ADDI, bytecode.OP_ADDK, bytecode.OP_SUBK, bytecode.OP_MULK,
		bytecode.OP_DIVK, bytecode.OP_IDIVK, bytecode.OP_MODK, bytecode.OP_BANDK, bytecode.OP_BORK,
		bytecode.OP_BXORK, bytecode.OP_SHLI, bytecode.OP_SHRI, bytecode.OP_UNM, bytecode.OP_BNOT,
		bytecode.OP_NOT, bytecode.OP_GETI, bytecode.OP_GETFIELD:
		return uint8(w.B()) == slot
	case bytecode.OP_ADD, bytecode.OP_SUB, bytecode.OP_MUL, bytecode.OP_DIV, bytecode.OP_IDIV,
		bytecode.OP_MOD, bytecode.OP_BAND, bytecode.OP_BOR, bytecode.OP_BXOR, bytecode.OP_SHL,
		bytecode.OP_SHR, bytecode.OP_GETTABLE:
		return uint8(w.B()) == slot || uint8(w.C()) == slot
	case bytecode.OP_SETI, bytecode.OP_SETFIELD:
		return uint8(w.A()) == slot || (!w.K() && uint8(w.C()) == slot)
	case bytecode.OP_SETTABLE:
		return uint8(w.A()) == slot || uint8(w.B()) == slot || (!w.K() && uint8(w.C()) == slot)
	case bytecode.OP_EQ, bytecode.OP_LT, bytecode.OP_LE:
		return uint8(w.A()) == slot || uint8(w.B()) == slot
	case bytecode.OP_EQK, bytecode.OP_EQI, bytecode.OP_LTI, bytecode.OP_LEI, bytecode.OP_GTI,
		bytecode.OP_GEI, bytecode.OP_TEST:
		return uint8(w.A()) == slot
	case bytecode.OP_CALL:
		a := uint8(w.A())
		return slot >= a && slot < a+uint8(w.B())
	}
	return false
}

func (b *loopBuilder) instWritesSlot(w bytecode.Instruction, slot uint8) bool {
	switch w.Op() {
	case bytecode.OP_SETI, bytecode.OP_SETTABLE, bytecode.OP_SETFIELD,
		bytecode.OP_EQ, bytecode.OP_LT, bytecode.OP_LE, bytecode.OP_EQK, bytecode.OP_EQI,
		bytecode.OP_LTI, bytecode.OP_LEI, bytecode.OP_GTI, bytecode.OP_GEI,
		bytecode.OP_TEST, bytecode.OP_JMP:
		return false
	default:
		return uint8(w.A()) == slot
	}
}

func (b *loopBuilder) mergeSlotType(slot uint8, t Type) bool {
	if t == TUnknown {
		return false
	}
	cur := b.slotType[slot]
	if cur == t {
		return false
	}
	if cur == TUnknown {
		b.slotType[slot] = t
		return true
	}
	// Float widens Int (e.g., `local sum = 0; sum = sum + float`).
	if (cur == TInt && t == TFloat) || (cur == TFloat && t == TInt) {
		if cur != TFloat {
			b.slotType[slot] = TFloat
			return true
		}
		return false
	}
	return false
}

func (b *loopBuilder) setArrKind(slot uint8, ak Type) bool {
	if ak == TUnknown {
		return false
	}
	cur := b.arrKindMap[slot]
	if cur == ak {
		return false
	}
	if cur == TUnknown {
		b.arrKindMap[slot] = ak
		return true
	}
	// Float widens Int during static inference unless feedback explicitly observed ArrKindInt.
	if cur == TArrInt && ak == TArrFloat && (b.fb == nil || b.fb.ArrKinds[slot] == 0) {
		b.arrKindMap[slot] = TArrFloat
		return true
	}
	return false
}

func (b *loopBuilder) rhsType(w bytecode.Instruction) Type {
	if w.K() {
		k := b.p.Consts[w.C()]
		switch k.Kind {
		case bytecode.ConstInt:
			return TInt
		case bytecode.ConstFloat:
			return TFloat
		case bytecode.ConstTrue, bytecode.ConstFalse:
			return TBool
		}
		return TUnknown
	}
	return b.slotType[uint8(w.C())]
}

func combineNumType(a, c Type) Type {
	if a == TFloat || c == TFloat {
		return TFloat
	}
	if a == TInt && c == TInt {
		return TInt
	}
	return TUnknown
}

func arrKindToType(ak uint8) Type {
	switch ak {
	case ArrKindInt:
		return TArrInt
	case ArrKindFloat:
		return TArrFloat
	case ArrKindBool:
		return TArrBool
	case ArrKindGeneric:
		return TArrGeneric
	default:
		return TUnknown
	}
}

func elemToArrType(et Type) Type {
	switch et {
	case TInt:
		return TArrInt
	case TFloat:
		return TArrFloat
	case TBool:
		return TArrBool
	case TTable:
		return TArrGeneric
	default:
		return TUnknown
	}
}

func (b *loopBuilder) findBlockStarts() []int {
	isStart := make(map[int]bool, 8)
	isStart[b.cand.BodyPC] = true
	for pc := b.cand.BodyPC; pc < b.cand.EndPC; pc++ {
		w := b.code[pc]
		op := w.Op()
		if isMMBinOp(op) {
			continue
		}
		if isCompareOrTestOp(op) {
			if pc+1 >= b.cand.EndPC || b.code[pc+1].Op() != bytecode.OP_JMP {
				return nil
			}
			target := pc + 2 + b.code[pc+1].SJ()
			fall := pc + 2
			if target >= b.cand.BodyPC && target < b.cand.EndPC {
				isStart[target] = true
			}
			if fall >= b.cand.BodyPC && fall < b.cand.EndPC {
				isStart[fall] = true
			}
			pc++ // skip fused JMP
		} else if op == bytecode.OP_JMP {
			target := pc + 1 + w.SJ()
			if target >= b.cand.BodyPC && target < b.cand.EndPC {
				isStart[target] = true
			}
			if pc+1 < b.cand.EndPC {
				isStart[pc+1] = true
			}
		}
	}
	var starts []int
	for pc := b.cand.BodyPC; pc < b.cand.EndPC; pc++ {
		if isStart[pc] {
			starts = append(starts, pc)
		}
	}
	return starts
}

// lowerBlock lowers bytecode instructions in [startPC, endPC) into typed SSA instructions on blk.
func (b *loopBuilder) lowerBlock(blk *Block, startPC, endPC int, pcToBlock map[int]int) bool {
	b.curSlot = make(map[uint8]ValID, 16)
	b.curShapes = make(map[ValID]shapeCacheEntry, 4)

	// Materialize OpGetVar for LoopVars at block entry only lazily on read so unused vars aren't loaded.
	for pc := startPC; pc < endPC; pc++ {
		w := b.code[pc]
		op := w.Op()
		if isMMBinOp(op) {
			continue
		}
		a := uint8(w.A())
		switch op {
		case bytecode.OP_MOVE:
			v, ok := b.readSlot(blk, uint8(w.B()), TUnknown, pc)
			if !ok {
				return false
			}
			b.writeSlot(blk, a, v, pc)

		case bytecode.OP_LOADI:
			v := b.emitBody(blk, Inst{Op: OpConstInt, Type: TInt, ImmI: int64(w.SBx()), DeoptPC: pc})
			b.writeSlot(blk, a, v, pc)

		case bytecode.OP_LOADF:
			v := b.emitBody(blk, Inst{Op: OpConstFloat, Type: TFloat, ImmU: math.Float64bits(float64(w.SBx())), DeoptPC: pc})
			b.writeSlot(blk, a, v, pc)

		case bytecode.OP_LOADTRUE:
			v := b.emitBody(blk, Inst{Op: OpConstBool, Type: TBool, ImmI: 1, DeoptPC: pc})
			b.writeSlot(blk, a, v, pc)

		case bytecode.OP_LOADFALSE:
			v := b.emitBody(blk, Inst{Op: OpConstBool, Type: TBool, ImmI: 0, DeoptPC: pc})
			b.writeSlot(blk, a, v, pc)

		case bytecode.OP_LOADK:
			k := b.p.Consts[w.Bx()]
			switch k.Kind {
			case bytecode.ConstInt:
				v := b.emitBody(blk, Inst{Op: OpConstInt, Type: TInt, ImmI: k.Int, DeoptPC: pc})
				b.writeSlot(blk, a, v, pc)
			case bytecode.ConstFloat:
				v := b.emitBody(blk, Inst{Op: OpConstFloat, Type: TFloat, ImmU: math.Float64bits(k.Float), DeoptPC: pc})
				b.writeSlot(blk, a, v, pc)
			default:
				return false
			}

		case bytecode.OP_GETUPVAL:
			// Hoist read-only upvalue into preheader (only for typed primitives, tables, or known callees).
			uv := w.B()
			if !b.lowerGetUpval(blk, a, uv, pc) {
				return false
			}

		case bytecode.OP_ADDI:
			sc := int64(w.SC())
			v, ok := b.readSlot(blk, uint8(w.B()), TUnknown, pc)
			if !ok {
				return false
			}
			vt := b.valType(v)
			if vt == TInt {
				res := b.emitBody(blk, Inst{Op: OpAddIImm, Type: TInt, Args: [3]ValID{v, NoVal, NoVal}, NumArgs: 1, ImmI: sc, DeoptPC: pc})
				b.trackAffineAddImm(res, v, sc)
				b.writeSlot(blk, a, res, pc)
			} else if vt == TFloat {
				kc := b.emitPre(Inst{Op: OpConstFloat, Type: TFloat, ImmU: math.Float64bits(float64(sc))})
				res := b.emitBody(blk, Inst{Op: OpAddF, Type: TFloat, Args: [3]ValID{v, kc, NoVal}, NumArgs: 2, DeoptPC: pc})
				b.writeSlot(blk, a, res, pc)
			} else {
				return false
			}

		case bytecode.OP_ADDK, bytecode.OP_SUBK, bytecode.OP_MULK, bytecode.OP_DIVK, bytecode.OP_IDIVK, bytecode.OP_MODK:
			if !b.lowerArithK(blk, op, a, uint8(w.B()), w.C(), pc) {
				return false
			}

		case bytecode.OP_ADD, bytecode.OP_SUB, bytecode.OP_MUL, bytecode.OP_DIV, bytecode.OP_IDIV, bytecode.OP_MOD:
			if !b.lowerArithRR(blk, op, a, uint8(w.B()), uint8(w.C()), pc) {
				return false
			}

		case bytecode.OP_BAND, bytecode.OP_BOR, bytecode.OP_BXOR, bytecode.OP_SHL, bytecode.OP_SHR:
			if !b.lowerBitRR(blk, op, a, uint8(w.B()), uint8(w.C()), pc) {
				return false
			}

		case bytecode.OP_BANDK, bytecode.OP_BORK, bytecode.OP_BXORK, bytecode.OP_SHLI, bytecode.OP_SHRI:
			if !b.lowerBitK(blk, op, a, uint8(w.B()), w, pc) {
				return false
			}

		case bytecode.OP_UNM:
			v, ok := b.readSlot(blk, uint8(w.B()), TUnknown, pc)
			if !ok {
				return false
			}
			vt := b.valType(v)
			if vt == TInt {
				res := b.emitBody(blk, Inst{Op: OpNegI, Type: TInt, Args: [3]ValID{v, NoVal, NoVal}, NumArgs: 1, DeoptPC: pc})
				b.writeSlot(blk, a, res, pc)
			} else if vt == TFloat {
				res := b.emitBody(blk, Inst{Op: OpNegF, Type: TFloat, Args: [3]ValID{v, NoVal, NoVal}, NumArgs: 1, DeoptPC: pc})
				b.writeSlot(blk, a, res, pc)
			} else {
				return false
			}

		case bytecode.OP_BNOT:
			v, ok := b.readSlot(blk, uint8(w.B()), TInt, pc)
			if !ok {
				return false
			}
			res := b.emitBody(blk, Inst{Op: OpBNotI, Type: TInt, Args: [3]ValID{v, NoVal, NoVal}, NumArgs: 1, DeoptPC: pc})
			b.writeSlot(blk, a, res, pc)

		case bytecode.OP_NOT:
			v, ok := b.readSlot(blk, uint8(w.B()), TBool, pc)
			if !ok {
				return false
			}
			res := b.emitBody(blk, Inst{Op: OpNotB, Type: TBool, Args: [3]ValID{v, NoVal, NoVal}, NumArgs: 1, DeoptPC: pc})
			b.writeSlot(blk, a, res, pc)

		case bytecode.OP_GETI:
			idxVal := b.emitBody(blk, Inst{Op: OpConstInt, Type: TInt, ImmI: int64(w.C()), DeoptPC: pc})
			if !b.lowerArrGet(blk, a, uint8(w.B()), idxVal, pc) {
				return false
			}

		case bytecode.OP_GETTABLE:
			idxVal, ok := b.readSlot(blk, uint8(w.C()), TInt, pc)
			if !ok {
				return false
			}
			if !b.lowerArrGet(blk, a, uint8(w.B()), idxVal, pc) {
				return false
			}

		case bytecode.OP_SETI:
			idxVal := b.emitBody(blk, Inst{Op: OpConstInt, Type: TInt, ImmI: int64(w.B()), DeoptPC: pc})
			if !b.lowerArrSet(blk, a, idxVal, w, pc) {
				return false
			}

		case bytecode.OP_SETTABLE:
			idxVal, ok := b.readSlot(blk, uint8(w.B()), TInt, pc)
			if !ok {
				return false
			}
			if !b.lowerArrSet(blk, a, idxVal, w, pc) {
				return false
			}

		case bytecode.OP_GETFIELD:
			if !b.lowerGetField(blk, a, uint8(w.B()), pc) {
				return false
			}

		case bytecode.OP_SETFIELD:
			if !b.lowerSetField(blk, a, w, pc) {
				return false
			}

		case bytecode.OP_CALL:
			if !b.lowerCall(blk, a, w.B(), pc) {
				return false
			}

		case bytecode.OP_EQ, bytecode.OP_LT, bytecode.OP_LE,
			bytecode.OP_EQK, bytecode.OP_EQI, bytecode.OP_LTI, bytecode.OP_LEI,
			bytecode.OP_GTI, bytecode.OP_GEI, bytecode.OP_TEST:
			if pc+1 != endPC-1 || b.code[pc+1].Op() != bytecode.OP_JMP {
				// Comparison must terminate its basic block (with fused OP_JMP at endPC-1).
				return false
			}
			return b.lowerCondBranch(blk, w, b.code[pc+1], pc, pcToBlock)

		case bytecode.OP_JMP:
			if pc != endPC-1 {
				return false
			}
			target := pc + 1 + w.SJ()
			if target >= b.cand.ExitPC {
				blk.Term = Term{Kind: TermExit, ExitTargetPC: target, DeoptPC: pc}
				return true
			}
			if target == b.cand.EndPC {
				blk.Term = Term{Kind: TermBackedge, DeoptPC: pc}
				return true
			}
			tb, ok := pcToBlock[target]
			if !ok {
				return false
			}
			blk.Term = Term{Kind: TermJump, Succ: [2]int{tb, -1}, DeoptPC: pc}
			return true

		default:
			return false
		}
	}

	// Block ended without explicit jump: fall through to next block or backedge.
	if endPC == b.cand.EndPC {
		blk.Term = Term{Kind: TermBackedge, DeoptPC: b.cand.EndPC}
		return true
	}
	nb, ok := pcToBlock[endPC]
	if !ok {
		return false
	}
	blk.Term = Term{Kind: TermJump, Succ: [2]int{nb, -1}, DeoptPC: endPC}
	return true
}

func (b *loopBuilder) lowerGetUpval(blk *Block, a uint8, uv int, pc int) bool {
	// Check if this slot is used as a table array, math intrinsic, inline callee, or numeric scalar.
	if b.fb != nil {
		// Check if immediately consumed by an inline call.
		for cpc := pc + 1; cpc < b.cand.EndPC && cpc <= pc+4; cpc++ {
			cw := b.code[cpc]
			if cw.Op() == bytecode.OP_CALL && uint8(cw.A()) == a {
				if tgt, ok := b.fb.InlineCalls[cpc]; ok && tgt != nil {
					v := b.emitPreRaw(Inst{
						Op:       OpGuardCalleePro,
						Type:     TInt,
						Slot:     a,
						UpvalIdx: int16(uv),
						AuxU32:   tgt.ProtoIdx,
						DeoptPC:  pc,
					})
					b.curSlot[a] = v
					return true
				}
			}
		}
	}
	// If consumed by GETI/GETTABLE/SETI/SETTABLE, hoist as OpGuardTableArr.
	if ak := b.upvArrKind[uv]; ak != TUnknown {
		v := b.hoistTableArr(true, uv, ak)
		b.curSlot[a] = v
		return true
	}
	if ak := b.arrKindMap[a]; ak != TUnknown {
		v := b.hoistTableArr(true, uv, ak)
		b.curSlot[a] = v
		return true
	}
	// If consumed as math.sqrt / math.abs callee in a subsequent OP_CALL (checked by name in Upvalues).
	if uv < len(b.p.Upvalues) {
		name := b.p.Upvalues[uv].Name
		if name == "sqrt" {
			v := b.emitPreRaw(Inst{Op: OpGuardUpvalSqrt, Type: TInt, UpvalIdx: int16(uv), AuxIdx: int32(uv)})
			b.curSlot[a] = v
			return true
		}
		if name == "abs" {
			v := b.emitPreRaw(Inst{Op: OpGuardUpvalAbs, Type: TInt, UpvalIdx: int16(uv), AuxIdx: int32(uv)})
			b.curSlot[a] = v
			return true
		}
	}
	// Numeric scalar upvalue (e.g. SOLAR_MASS, pi, etc.)
	t := b.slotType[a]
	if t == TInt {
		v, ok := b.upvalVal[uv]
		if !ok {
			v = b.emitPreRaw(Inst{Op: OpGuardUpvalInt, Type: TInt, UpvalIdx: int16(uv), AuxIdx: int32(uv)})
			b.upvalVal[uv] = v
		}
		b.writeSlot(blk, a, v, pc)
		return true
	}
	if t == TFloat || b.protoFloat {
		v, ok := b.upvalVal[uv]
		if !ok {
			v = b.emitPreRaw(Inst{Op: OpGuardUpvalFlt, Type: TFloat, UpvalIdx: int16(uv), AuxIdx: int32(uv)})
			b.upvalVal[uv] = v
		}
		b.writeSlot(blk, a, v, pc)
		return true
	}
	return false
}

func (b *loopBuilder) hoistTableArr(isUpval bool, idx int, ak Type) ValID {
	key := arrKey{isUpval: isUpval, idx: idx, typ: ak}
	if v, ok := b.arrHoist[key]; ok {
		return v
	}
	in := Inst{Op: OpGuardTableArr, Type: ak, UpvalIdx: -1}
	if isUpval {
		in.UpvalIdx = int16(idx)
		in.AuxIdx = int32(idx)
	} else {
		in.Slot = uint8(idx)
	}
	arrV := b.emitPreRaw(in)
	lenV := b.emitPre(Inst{Op: OpLoadArrLen, Type: TInt, Args: [3]ValID{arrV, NoVal, NoVal}, NumArgs: 1})
	b.arrHoist[key] = arrV
	b.arrLenVal[arrV] = lenV
	return arrV
}

func (b *loopBuilder) readSlot(blk *Block, slot uint8, want Type, pc int) (ValID, bool) {
	if v, ok := b.curSlot[slot]; ok {
		return b.coerceVal(blk, v, want, pc)
	}
	if vi, ok := b.varIdx[slot]; ok {
		lv := b.reg.Vars[vi]
		v := b.emitBody(blk, Inst{Op: OpGetVar, Type: lv.Type, AuxIdx: int32(vi), DeoptPC: pc})
		if lv.IsForIdx {
			b.reg.Affine[v] = AffineIdx{Valid: true, BaseOff: NoVal, Delta: 0}
		}
		b.curSlot[slot] = v
		return b.coerceVal(blk, v, want, pc)
	}
	// Loop-invariant Lua register! Hoist unbox to preheader.
	if b.written[slot] {
		return NoVal, false
	}
	if v, ok := b.invarVal[slot]; ok {
		return b.coerceVal(blk, v, want, pc)
	}
	t := b.slotType[slot]
	if t == TUnknown {
		t = want
	}
	var v ValID
	switch t {
	case TInt:
		v = b.emitPre(Inst{Op: OpUnboxInt, Type: TInt, Slot: slot})
	case TFloat:
		v = b.emitPre(Inst{Op: OpUnboxFloat, Type: TFloat, Slot: slot})
	case TBool:
		v = b.emitPre(Inst{Op: OpUnboxBool, Type: TBool, Slot: slot})
	default:
		return NoVal, false
	}
	b.invarVal[slot] = v
	return b.coerceVal(blk, v, want, pc)
}

func (b *loopBuilder) coerceVal(blk *Block, v ValID, want Type, pc int) (ValID, bool) {
	vt := b.valType(v)
	if want == TUnknown || vt == want {
		return v, true
	}
	if vt == TInt && want == TFloat {
		fv := b.emitBody(blk, Inst{Op: OpIntToFloat, Type: TFloat, Args: [3]ValID{v, NoVal, NoVal}, NumArgs: 1, DeoptPC: pc})
		return fv, true
	}
	return NoVal, false
}

func (b *loopBuilder) writeSlot(blk *Block, slot uint8, v ValID, pc int) {
	if vi, ok := b.varIdx[slot]; ok {
		lv := b.reg.Vars[vi]
		if b.valType(v) != lv.Type {
			b.failed = true
			return
		}
		b.emitBody(blk, Inst{Op: OpSetVar, Type: lv.Type, AuxIdx: int32(vi), Args: [3]ValID{v, NoVal, NoVal}, NumArgs: 1, DeoptPC: pc})
	}
	b.curSlot[slot] = v
}

func (b *loopBuilder) valType(id ValID) Type {
	if id == NoVal {
		return TUnknown
	}
	for i := range b.reg.Preheader {
		if b.reg.Preheader[i].ID == id {
			return b.reg.Preheader[i].Type
		}
	}
	for _, blk := range b.reg.Blocks {
		if blk == nil {
			continue
		}
		for i := range blk.Insts {
			if blk.Insts[i].ID == id {
				return blk.Insts[i].Type
			}
		}
	}
	return TUnknown
}

func (b *loopBuilder) isInvariant(id ValID) bool {
	if id == NoVal {
		return false
	}
	for i := range b.reg.Preheader {
		if b.reg.Preheader[i].ID == id {
			return b.reg.Preheader[i].Op != OpLoopIdx && b.reg.Preheader[i].Op != OpLoopCnt
		}
	}
	return false
}

func (b *loopBuilder) trackAffineAddImm(dst, src ValID, imm int64) {
	if af, ok := b.reg.Affine[src]; ok && af.Valid {
		b.reg.Affine[dst] = AffineIdx{Valid: true, BaseOff: af.BaseOff, Delta: af.Delta + imm}
	}
}

func (b *loopBuilder) trackAffineAdd(dst, lhs, rhs ValID) {
	la, lok := b.reg.Affine[lhs]
	ra, rok := b.reg.Affine[rhs]
	if lok && la.Valid && la.BaseOff == NoVal && b.isInvariant(rhs) {
		b.reg.Affine[dst] = AffineIdx{Valid: true, BaseOff: rhs, Delta: la.Delta}
	} else if rok && ra.Valid && ra.BaseOff == NoVal && b.isInvariant(lhs) {
		b.reg.Affine[dst] = AffineIdx{Valid: true, BaseOff: lhs, Delta: ra.Delta}
	}
}

func (b *loopBuilder) lowerArithK(blk *Block, op bytecode.OpCode, a, bSlot uint8, cIdx, pc int) bool {
	k := b.p.Consts[cIdx]
	if k.Kind != bytecode.ConstInt && k.Kind != bytecode.ConstFloat {
		return false
	}
	wantL := TUnknown
	if op == bytecode.OP_DIVK || k.Kind == bytecode.ConstFloat {
		wantL = TFloat
	}
	lhs, ok := b.readSlot(blk, bSlot, wantL, pc)
	if !ok {
		return false
	}
	forceFloat := op == bytecode.OP_DIVK || k.Kind == bytecode.ConstFloat || b.valType(lhs) == TFloat
	if forceFloat {
		lhs, ok = b.coerceVal(blk, lhs, TFloat, pc)
		if !ok {
			return false
		}
		kf := k.Float
		if k.Kind == bytecode.ConstInt {
			kf = float64(k.Int)
		}
		rhs := b.emitPre(Inst{Op: OpConstFloat, Type: TFloat, ImmU: math.Float64bits(kf)})
		var irOp Op
		switch op {
		case bytecode.OP_ADDK:
			irOp = OpAddF
		case bytecode.OP_SUBK:
			irOp = OpSubF
		case bytecode.OP_MULK:
			irOp = OpMulF
		case bytecode.OP_DIVK:
			irOp = OpDivF
		default:
			return false
		}
		res := b.emitBody(blk, Inst{Op: irOp, Type: TFloat, Args: [3]ValID{lhs, rhs, NoVal}, NumArgs: 2, DeoptPC: pc})
		b.writeSlot(blk, a, res, pc)
		return true
	}
	if b.valType(lhs) != TInt {
		return false
	}
	rhs := b.emitPre(Inst{Op: OpConstInt, Type: TInt, ImmI: k.Int})
	var irOp Op
	switch op {
	case bytecode.OP_ADDK:
		irOp = OpAddI
	case bytecode.OP_SUBK:
		irOp = OpSubI
	case bytecode.OP_MULK:
		irOp = OpMulI
	case bytecode.OP_IDIVK:
		irOp = OpIDivI
	case bytecode.OP_MODK:
		irOp = OpModI
	default:
		return false
	}
	res := b.emitBody(blk, Inst{Op: irOp, Type: TInt, Args: [3]ValID{lhs, rhs, NoVal}, NumArgs: 2, DeoptPC: pc})
	if op == bytecode.OP_ADDK {
		b.trackAffineAddImm(res, lhs, k.Int)
	} else if op == bytecode.OP_SUBK {
		b.trackAffineAddImm(res, lhs, -k.Int)
	}
	b.writeSlot(blk, a, res, pc)
	return true
}

func (b *loopBuilder) peekSlotType(slot uint8) Type {
	if v, ok := b.curSlot[slot]; ok {
		return b.valType(v)
	}
	if vi, ok := b.varIdx[slot]; ok {
		return b.reg.Vars[vi].Type
	}
	if v, ok := b.invarVal[slot]; ok {
		return b.valType(v)
	}
	return b.slotType[slot]
}

func (b *loopBuilder) lowerArithRR(blk *Block, op bytecode.OpCode, a, bSlot, cSlot uint8, pc int) bool {
	ltHint := b.peekSlotType(bSlot)
	rtHint := b.peekSlotType(cSlot)
	want := TUnknown
	if op == bytecode.OP_DIV || ltHint == TFloat || rtHint == TFloat {
		want = TFloat
	} else if ltHint == TInt || rtHint == TInt {
		want = TInt
	}
	lhs, ok1 := b.readSlot(blk, bSlot, want, pc)
	rhs, ok2 := b.readSlot(blk, cSlot, want, pc)
	if !ok1 || !ok2 {
		return false
	}
	lt, rt := b.valType(lhs), b.valType(rhs)
	wantFloat := op == bytecode.OP_DIV || lt == TFloat || rt == TFloat
	if wantFloat {
		if op == bytecode.OP_IDIV || op == bytecode.OP_MOD {
			return false
		}
		var ok bool
		lhs, ok = b.coerceVal(blk, lhs, TFloat, pc)
		if !ok {
			return false
		}
		rhs, ok = b.coerceVal(blk, rhs, TFloat, pc)
		if !ok {
			return false
		}
		var irOp Op
		switch op {
		case bytecode.OP_ADD:
			irOp = OpAddF
		case bytecode.OP_SUB:
			irOp = OpSubF
		case bytecode.OP_MUL:
			irOp = OpMulF
		case bytecode.OP_DIV:
			irOp = OpDivF
		}
		res := b.emitBody(blk, Inst{Op: irOp, Type: TFloat, Args: [3]ValID{lhs, rhs, NoVal}, NumArgs: 2, DeoptPC: pc})
		b.writeSlot(blk, a, res, pc)
		return true
	}
	if lt != TInt || rt != TInt {
		return false
	}
	var irOp Op
	switch op {
	case bytecode.OP_ADD:
		irOp = OpAddI
	case bytecode.OP_SUB:
		irOp = OpSubI
	case bytecode.OP_MUL:
		irOp = OpMulI
	case bytecode.OP_IDIV:
		irOp = OpIDivI
	case bytecode.OP_MOD:
		irOp = OpModI
	}
	res := b.emitBody(blk, Inst{Op: irOp, Type: TInt, Args: [3]ValID{lhs, rhs, NoVal}, NumArgs: 2, DeoptPC: pc})
	if op == bytecode.OP_ADD {
		b.trackAffineAdd(res, lhs, rhs)
	}
	b.writeSlot(blk, a, res, pc)
	return true
}

func (b *loopBuilder) lowerBitRR(blk *Block, op bytecode.OpCode, a, bSlot, cSlot uint8, pc int) bool {
	lhs, ok := b.readSlot(blk, bSlot, TInt, pc)
	if !ok {
		return false
	}
	rhs, ok := b.readSlot(blk, cSlot, TInt, pc)
	if !ok {
		return false
	}
	var irOp Op
	switch op {
	case bytecode.OP_BAND:
		irOp = OpAndI
	case bytecode.OP_BOR:
		irOp = OpOrI
	case bytecode.OP_BXOR:
		irOp = OpXorI
	case bytecode.OP_SHL:
		irOp = OpShlI
	case bytecode.OP_SHR:
		irOp = OpShrI
	}
	res := b.emitBody(blk, Inst{Op: irOp, Type: TInt, Args: [3]ValID{lhs, rhs, NoVal}, NumArgs: 2, DeoptPC: pc})
	b.writeSlot(blk, a, res, pc)
	return true
}

func (b *loopBuilder) lowerBitK(blk *Block, op bytecode.OpCode, a, bSlot uint8, w bytecode.Instruction, pc int) bool {
	lhs, ok := b.readSlot(blk, bSlot, TInt, pc)
	if !ok {
		return false
	}
	var imm int64
	var irOp Op
	switch op {
	case bytecode.OP_BANDK:
		k := b.p.Consts[w.C()]
		if k.Kind != bytecode.ConstInt {
			return false
		}
		imm = k.Int
		irOp = OpAndI
	case bytecode.OP_BORK:
		k := b.p.Consts[w.C()]
		if k.Kind != bytecode.ConstInt {
			return false
		}
		imm = k.Int
		irOp = OpOrI
	case bytecode.OP_BXORK:
		k := b.p.Consts[w.C()]
		if k.Kind != bytecode.ConstInt {
			return false
		}
		imm = k.Int
		irOp = OpXorI
	case bytecode.OP_SHLI:
		imm = int64(w.SC())
		irOp = OpShlI
	case bytecode.OP_SHRI:
		imm = int64(w.SC())
		irOp = OpShrI
	}
	rhs := b.emitPre(Inst{Op: OpConstInt, Type: TInt, ImmI: imm})
	args := [3]ValID{lhs, rhs, NoVal}
	if op == bytecode.OP_SHLI {
		// Lua 5.5 OP_SHLI computes SC << R[B] (constant on the left, register on the right).
		args = [3]ValID{rhs, lhs, NoVal}
	}
	res := b.emitBody(blk, Inst{Op: irOp, Type: TInt, Args: args, NumArgs: 2, DeoptPC: pc})
	b.writeSlot(blk, a, res, pc)
	return true
}

func (b *loopBuilder) resolveArrVal(tblSlot uint8) (ValID, Type, bool) {
	if v, ok := b.curSlot[tblSlot]; ok {
		vt := b.valType(v)
		if vt.IsArray() {
			return v, vt, true
		}
	}
	if b.written[tblSlot] {
		return NoVal, TUnknown, false
	}
	ak := b.arrKindMap[tblSlot]
	if !ak.IsArray() {
		return NoVal, TUnknown, false
	}
	v := b.hoistTableArr(false, int(tblSlot), ak)
	return v, ak, true
}

func (b *loopBuilder) lowerArrGet(blk *Block, a, tblSlot uint8, idxVal ValID, pc int) bool {
	arrV, ak, ok := b.resolveArrVal(tblSlot)
	if !ok {
		return false
	}
	var res ValID
	switch ak {
	case TArrInt:
		res = b.emitBody(blk, Inst{Op: OpLoadArrInt, Type: TInt, Args: [3]ValID{arrV, idxVal, NoVal}, NumArgs: 2, DeoptPC: pc})
	case TArrFloat:
		res = b.emitBody(blk, Inst{Op: OpLoadArrFloat, Type: TFloat, Args: [3]ValID{arrV, idxVal, NoVal}, NumArgs: 2, DeoptPC: pc})
	case TArrBool:
		res = b.emitBody(blk, Inst{Op: OpLoadArrBool, Type: TBool, Args: [3]ValID{arrV, idxVal, NoVal}, NumArgs: 2, DeoptPC: pc})
	case TArrGeneric:
		res = b.emitBody(blk, Inst{Op: OpLoadArrTable, Type: TTable, Args: [3]ValID{arrV, idxVal, NoVal}, NumArgs: 2, DeoptPC: pc})
	default:
		return false
	}
	b.writeSlot(blk, a, res, pc)
	return true
}

func (b *loopBuilder) lowerArrSet(blk *Block, tblSlot uint8, idxVal ValID, w bytecode.Instruction, pc int) bool {
	arrV, ak, ok := b.resolveArrVal(tblSlot)
	if !ok {
		return false
	}
	elemT := ak.ElemType()
	if elemT == TUnknown {
		return false
	}
	var valV ValID
	if w.K() {
		k := b.p.Consts[w.C()]
		switch elemT {
		case TInt:
			if k.Kind != bytecode.ConstInt {
				return false
			}
			valV = b.emitPre(Inst{Op: OpConstInt, Type: TInt, ImmI: k.Int})
		case TFloat:
			// Never coerce integer constants into a float array: Lua preserves integer
			// type by promoting the backing array to generic storage.
			if k.Kind != bytecode.ConstFloat {
				return false
			}
			valV = b.emitPre(Inst{Op: OpConstFloat, Type: TFloat, ImmU: math.Float64bits(k.Float)})
		case TBool:
			if k.Kind == bytecode.ConstTrue {
				valV = b.emitPre(Inst{Op: OpConstBool, Type: TBool, ImmI: 1})
			} else if k.Kind == bytecode.ConstFalse {
				valV = b.emitPre(Inst{Op: OpConstBool, Type: TBool, ImmI: 0})
			} else {
				return false
			}
		}
	} else {
		var rok bool
		valV, rok = b.readSlot(blk, uint8(w.C()), TUnknown, pc)
		if !rok || b.valType(valV) != elemT {
			return false
		}
	}
	var op Op
	switch ak {
	case TArrInt:
		op = OpStoreArrInt
	case TArrFloat:
		op = OpStoreArrFlt
	case TArrBool:
		op = OpStoreArrBool
	default:
		return false
	}
	b.emitBody(blk, Inst{Op: op, Type: TUnknown, Args: [3]ValID{arrV, idxVal, valV}, NumArgs: 3, DeoptPC: pc})
	return true
}

func (b *loopBuilder) guardShapeObj(blk *Block, tblVal ValID, shapeRef uint32, pc int) ValID {
	if ce, ok := b.curShapes[tblVal]; ok && ce.shapeRef == shapeRef {
		return ce.objVal
	}
	objV := b.emitBody(blk, Inst{
		Op:      OpGuardShape,
		Type:    TShapeObj,
		Args:    [3]ValID{tblVal, NoVal, NoVal},
		NumArgs: 1,
		AuxU32:  shapeRef,
		DeoptPC: pc,
	})
	b.curShapes[tblVal] = shapeCacheEntry{shapeRef: shapeRef, objVal: objV}
	return objV
}

func (b *loopBuilder) lowerGetField(blk *Block, a, tblSlot uint8, pc int) bool {
	if b.fb == nil {
		return false
	}
	ic, ok := b.fb.FieldICs[pc]
	if !ok {
		return false
	}
	tblV, ok := b.curSlot[tblSlot]
	if !ok || b.valType(tblV) != TTable {
		return false
	}
	objV := b.guardShapeObj(blk, tblV, ic.ShapeRef, pc)
	t := b.slotType[a]
	if t == TInt {
		res := b.emitBody(blk, Inst{Op: OpLoadSlotInt, Type: TInt, Args: [3]ValID{objV, NoVal, NoVal}, NumArgs: 1, AuxIdx: int32(ic.Slot), DeoptPC: pc})
		b.writeSlot(blk, a, res, pc)
		return true
	}
	res := b.emitBody(blk, Inst{Op: OpLoadSlotFloat, Type: TFloat, Args: [3]ValID{objV, NoVal, NoVal}, NumArgs: 1, AuxIdx: int32(ic.Slot), DeoptPC: pc})
	b.writeSlot(blk, a, res, pc)
	return true
}

func (b *loopBuilder) lowerSetField(blk *Block, tblSlot uint8, w bytecode.Instruction, pc int) bool {
	if b.fb == nil {
		return false
	}
	ic, ok := b.fb.FieldICs[pc]
	if !ok {
		return false
	}
	tblV, ok := b.curSlot[tblSlot]
	if !ok || b.valType(tblV) != TTable {
		return false
	}
	objV := b.guardShapeObj(blk, tblV, ic.ShapeRef, pc)
	var valV ValID
	if w.K() {
		k := b.p.Consts[w.C()]
		if k.Kind == bytecode.ConstFloat {
			valV = b.emitPre(Inst{Op: OpConstFloat, Type: TFloat, ImmU: math.Float64bits(k.Float)})
		} else if k.Kind == bytecode.ConstInt {
			valV = b.emitPre(Inst{Op: OpConstInt, Type: TInt, ImmI: k.Int})
		} else {
			return false
		}
	} else {
		var rok bool
		valV, rok = b.readSlot(blk, uint8(w.C()), TUnknown, pc)
		if !rok {
			return false
		}
	}
	vt := b.valType(valV)
	if vt == TFloat {
		b.emitBody(blk, Inst{Op: OpStoreSlotFloat, Type: TUnknown, Args: [3]ValID{objV, valV, NoVal}, NumArgs: 2, AuxIdx: int32(ic.Slot), DeoptPC: pc})
		return true
	}
	if vt == TInt {
		b.emitBody(blk, Inst{Op: OpStoreSlotInt, Type: TUnknown, Args: [3]ValID{objV, valV, NoVal}, NumArgs: 2, AuxIdx: int32(ic.Slot), DeoptPC: pc})
		return true
	}
	return false
}

func (b *loopBuilder) lowerCall(blk *Block, a uint8, bArg, pc int) bool {
	// Case 1: Inline tiny pure leaf Lua function from feedback (e.g. SpectralNorm A(i, j)).
	if b.fb != nil {
		if tgt, ok := b.fb.InlineCalls[pc]; ok && tgt != nil && canInlineLeafProto(tgt.Proto) {
			// Guard callee protoIdx in preheader if not already guarded by lowerGetUpval.
			if v, ok := b.curSlot[a]; !ok || b.instOp(v) != OpGuardCalleePro {
				if b.written[a] {
					return false
				}
				b.emitPreRaw(Inst{
					Op:       OpGuardCalleePro,
					Type:     TInt,
					Slot:     a,
					UpvalIdx: -1,
					AuxU32:   tgt.ProtoIdx,
					DeoptPC:  pc,
				})
			}
			args := make([]ValID, tgt.NumParams)
			for i := 0; i < tgt.NumParams; i++ {
				av, ok := b.readSlot(blk, a+1+uint8(i), TUnknown, pc)
				if !ok {
					return false
				}
				args[i] = av
			}
			res, ok := b.inlineLeafProto(blk, tgt.Proto, args, pc)
			if !ok {
				return false
			}
			b.writeSlot(blk, a, res, pc)
			return true
		}
	}

	// Case 2: Hardware math.sqrt / math.abs intrinsic (1 arg, 1 return).
	if bArg != 2 {
		return false
	}
	var isSqrt, isAbs bool
	if v, ok := b.curSlot[a]; ok {
		op := b.instOp(v)
		isSqrt = op == OpGuardUpvalSqrt || op == OpGuardRegSqrt
		isAbs = op == OpGuardUpvalAbs || op == OpGuardRegAbs
	}
	if !isSqrt && !isAbs {
		// Callee is a loop-invariant local register (check local name if available or emit guard).
		if b.written[a] {
			return false
		}
		name := b.localNameForSlot(a, pc)
		switch name {
		case "sqrt":
			b.emitPre(Inst{Op: OpGuardRegSqrt, Type: TInt, Slot: a})
			isSqrt = true
		case "abs":
			b.emitPre(Inst{Op: OpGuardRegAbs, Type: TInt, Slot: a})
			isAbs = true
		default:
			return false
		}
	}
	if isSqrt {
		argV, ok := b.readSlot(blk, a+1, TFloat, pc)
		if !ok {
			return false
		}
		res := b.emitBody(blk, Inst{Op: OpSqrtF, Type: TFloat, Args: [3]ValID{argV, NoVal, NoVal}, NumArgs: 1, DeoptPC: pc})
		b.writeSlot(blk, a, res, pc)
		return true
	}
	argV, ok := b.readSlot(blk, a+1, TUnknown, pc)
	if !ok {
		return false
	}
	if b.valType(argV) == TInt {
		res := b.emitBody(blk, Inst{Op: OpAbsI, Type: TInt, Args: [3]ValID{argV, NoVal, NoVal}, NumArgs: 1, DeoptPC: pc})
		b.writeSlot(blk, a, res, pc)
		return true
	}
	argF, ok := b.coerceVal(blk, argV, TFloat, pc)
	if !ok {
		return false
	}
	res := b.emitBody(blk, Inst{Op: OpAbsF, Type: TFloat, Args: [3]ValID{argF, NoVal, NoVal}, NumArgs: 1, DeoptPC: pc})
	b.writeSlot(blk, a, res, pc)
	return true
}

func (b *loopBuilder) localNameForSlot(slot uint8, pc int) string {
	// Scan pre-loop bytecode for `local sqrt = math.sqrt` / `local abs = math.abs`
	// (GETFIELD dst, tbl, K["sqrt"|"abs"] or MOVE dst, src). Preheader OpGuardRegSqrt/Abs
	// still verifies the exact runtime staticNative index on entry.
	cur := slot
	for p := pc - 1; p >= 0; p-- {
		w := b.code[p]
		switch w.Op() {
		case bytecode.OP_MOVE:
			if uint8(w.A()) == cur {
				cur = uint8(w.B())
			}
		case bytecode.OP_GETFIELD:
			if uint8(w.A()) == cur {
				c := w.C()
				if c < len(b.p.Consts) && b.p.Consts[c].Kind == bytecode.ConstString {
					name := b.p.Consts[c].Str
					if name == "sqrt" || name == "abs" {
						return name
					}
				}
				return ""
			}
		case bytecode.OP_GETUPVAL:
			if uint8(w.A()) == cur {
				uv := w.B()
				if uv < len(b.p.Upvalues) {
					name := b.p.Upvalues[uv].Name
					if name == "sqrt" || name == "abs" {
						return name
					}
				}
				return ""
			}
		}
	}
	return ""
}

func (b *loopBuilder) instOp(id ValID) Op {
	for i := range b.reg.Preheader {
		if b.reg.Preheader[i].ID == id {
			return b.reg.Preheader[i].Op
		}
	}
	return OpNop
}

// leafReturnIndex finds the single OP_RETURN1 (or OP_RETURN with B==2) terminating a tiny leaf proto,
// allowing the standard trailing unreachable OP_RETURN0 emitted by the Lua compiler.
func leafReturnIndex(p *bytecode.Proto) int {
	n := len(p.Code)
	if n < 2 {
		return -1
	}
	end := n - 1
	if end >= 1 && p.Code[end].Op() == bytecode.OP_RETURN0 {
		end--
	}
	w := p.Code[end]
	if w.Op() == bytecode.OP_RETURN1 || (w.Op() == bytecode.OP_RETURN && w.B() == 2) {
		return end
	}
	return -1
}

// canInlineLeafProto reports whether p is a tiny straight-line pure numeric leaf function
// (no loops, no branches, no upvals, no varargs, single return value).
func canInlineLeafProto(p *bytecode.Proto) bool {
	if p == nil || p.IsVararg() || len(p.Upvalues) > 0 || len(p.Code) > 28 {
		return false
	}
	retIdx := leafReturnIndex(p)
	if retIdx < 1 {
		return false
	}
	for i := 0; i < retIdx; i++ {
		op := p.Code[i].Op()
		if isMMBinOp(op) {
			continue
		}
		switch op {
		case bytecode.OP_MOVE, bytecode.OP_LOADI, bytecode.OP_LOADF, bytecode.OP_LOADK,
			bytecode.OP_ADDI, bytecode.OP_ADDK, bytecode.OP_SUBK, bytecode.OP_MULK, bytecode.OP_DIVK,
			bytecode.OP_ADD, bytecode.OP_SUB, bytecode.OP_MUL, bytecode.OP_DIV, bytecode.OP_UNM:
		default:
			return false
		}
	}
	return true
}

func inferLeafReturnType(p *bytecode.Proto) Type {
	for _, w := range p.Code {
		if w.Op() == bytecode.OP_DIV || w.Op() == bytecode.OP_DIVK || w.Op() == bytecode.OP_LOADF {
			return TFloat
		}
	}
	for _, k := range p.Consts {
		if k.Kind == bytecode.ConstFloat {
			return TFloat
		}
	}
	return TInt
}

func (b *loopBuilder) inlineLeafProto(blk *Block, lp *bytecode.Proto, args []ValID, callPC int) (ValID, bool) {
	retIdx := leafReturnIndex(lp)
	if retIdx < 1 {
		return NoVal, false
	}
	slots := make(map[uint8]ValID, lp.MaxStackSize)
	for i, v := range args {
		slots[uint8(i)] = v
	}
	for i := 0; i < retIdx; i++ {
		w := lp.Code[i]
		op := w.Op()
		if isMMBinOp(op) {
			continue
		}
		a := uint8(w.A())
		switch op {
		case bytecode.OP_MOVE:
			v, ok := slots[uint8(w.B())]
			if !ok {
				return NoVal, false
			}
			slots[a] = v
		case bytecode.OP_LOADI:
			slots[a] = b.emitBody(blk, Inst{Op: OpConstInt, Type: TInt, ImmI: int64(w.SBx()), DeoptPC: callPC})
		case bytecode.OP_LOADF:
			slots[a] = b.emitBody(blk, Inst{Op: OpConstFloat, Type: TFloat, ImmU: math.Float64bits(float64(w.SBx())), DeoptPC: callPC})
		case bytecode.OP_LOADK:
			k := lp.Consts[w.Bx()]
			if k.Kind == bytecode.ConstInt {
				slots[a] = b.emitBody(blk, Inst{Op: OpConstInt, Type: TInt, ImmI: k.Int, DeoptPC: callPC})
			} else if k.Kind == bytecode.ConstFloat {
				slots[a] = b.emitBody(blk, Inst{Op: OpConstFloat, Type: TFloat, ImmU: math.Float64bits(k.Float), DeoptPC: callPC})
			} else {
				return NoVal, false
			}
		case bytecode.OP_ADDI:
			v, ok := slots[uint8(w.B())]
			if !ok {
				return NoVal, false
			}
			sc := int64(w.SC())
			vt := b.valType(v)
			if vt == TInt {
				slots[a] = b.emitBody(blk, Inst{Op: OpAddIImm, Type: TInt, Args: [3]ValID{v, NoVal, NoVal}, NumArgs: 1, ImmI: sc, DeoptPC: callPC})
			} else if vt == TFloat {
				kc := b.emitPre(Inst{Op: OpConstFloat, Type: TFloat, ImmU: math.Float64bits(float64(sc))})
				slots[a] = b.emitBody(blk, Inst{Op: OpAddF, Type: TFloat, Args: [3]ValID{v, kc, NoVal}, NumArgs: 2, DeoptPC: callPC})
			} else {
				return NoVal, false
			}
		case bytecode.OP_UNM:
			v, ok := slots[uint8(w.B())]
			if !ok {
				return NoVal, false
			}
			vt := b.valType(v)
			if vt == TInt {
				slots[a] = b.emitBody(blk, Inst{Op: OpNegI, Type: TInt, Args: [3]ValID{v, NoVal, NoVal}, NumArgs: 1, DeoptPC: callPC})
			} else if vt == TFloat {
				slots[a] = b.emitBody(blk, Inst{Op: OpNegF, Type: TFloat, Args: [3]ValID{v, NoVal, NoVal}, NumArgs: 1, DeoptPC: callPC})
			} else {
				return NoVal, false
			}
		case bytecode.OP_ADDK, bytecode.OP_SUBK, bytecode.OP_MULK, bytecode.OP_DIVK:
			lhs, ok := slots[uint8(w.B())]
			if !ok {
				return NoVal, false
			}
			k := lp.Consts[w.C()]
			if k.Kind != bytecode.ConstInt && k.Kind != bytecode.ConstFloat {
				return NoVal, false
			}
			lt := b.valType(lhs)
			if lt != TInt && lt != TFloat {
				return NoVal, false
			}
			isF := op == bytecode.OP_DIVK || k.Kind == bytecode.ConstFloat || lt == TFloat
			if isF {
				lf, ok := b.coerceVal(blk, lhs, TFloat, callPC)
				if !ok {
					return NoVal, false
				}
				kf := k.Float
				if k.Kind == bytecode.ConstInt {
					kf = float64(k.Int)
				}
				rf := b.emitPre(Inst{Op: OpConstFloat, Type: TFloat, ImmU: math.Float64bits(kf)})
				slots[a] = b.emitBody(blk, Inst{Op: mapArithFOp(op), Type: TFloat, Args: [3]ValID{lf, rf, NoVal}, NumArgs: 2, DeoptPC: callPC})
			} else {
				ri := b.emitPre(Inst{Op: OpConstInt, Type: TInt, ImmI: k.Int})
				slots[a] = b.emitBody(blk, Inst{Op: mapArithIOp(op), Type: TInt, Args: [3]ValID{lhs, ri, NoVal}, NumArgs: 2, DeoptPC: callPC})
			}
		case bytecode.OP_ADD, bytecode.OP_SUB, bytecode.OP_MUL, bytecode.OP_DIV:
			lhs, ok1 := slots[uint8(w.B())]
			rhs, ok2 := slots[uint8(w.C())]
			if !ok1 || !ok2 {
				return NoVal, false
			}
			lt, rt := b.valType(lhs), b.valType(rhs)
			if (lt != TInt && lt != TFloat) || (rt != TInt && rt != TFloat) {
				return NoVal, false
			}
			isF := op == bytecode.OP_DIV || lt == TFloat || rt == TFloat
			if isF {
				lf, ok1 := b.coerceVal(blk, lhs, TFloat, callPC)
				rf, ok2 := b.coerceVal(blk, rhs, TFloat, callPC)
				if !ok1 || !ok2 {
					return NoVal, false
				}
				slots[a] = b.emitBody(blk, Inst{Op: mapArithFOp(op), Type: TFloat, Args: [3]ValID{lf, rf, NoVal}, NumArgs: 2, DeoptPC: callPC})
			} else {
				slots[a] = b.emitBody(blk, Inst{Op: mapArithIOp(op), Type: TInt, Args: [3]ValID{lhs, rhs, NoVal}, NumArgs: 2, DeoptPC: callPC})
			}
		default:
			return NoVal, false
		}
	}
	retW := lp.Code[retIdx]
	res, ok := slots[uint8(retW.A())]
	return res, ok
}

func mapArithFOp(op bytecode.OpCode) Op {
	switch op {
	case bytecode.OP_ADD, bytecode.OP_ADDK:
		return OpAddF
	case bytecode.OP_SUB, bytecode.OP_SUBK:
		return OpSubF
	case bytecode.OP_MUL, bytecode.OP_MULK:
		return OpMulF
	default:
		return OpDivF
	}
}

func mapArithIOp(op bytecode.OpCode) Op {
	switch op {
	case bytecode.OP_ADD, bytecode.OP_ADDK:
		return OpAddI
	case bytecode.OP_SUB, bytecode.OP_SUBK:
		return OpSubI
	default:
		return OpMulI
	}
}

func (b *loopBuilder) lowerCondBranch(blk *Block, w, jmp bytecode.Instruction, pc int, pcToBlock map[int]int) bool {
	jumpPC := pc + 2 + jmp.SJ()
	fallPC := pc + 2

	resolveTarget := func(tpc int) (int, int, bool) {
		if tpc >= b.cand.ExitPC {
			return -2, tpc, true // early exit
		}
		if tpc == b.cand.EndPC {
			return -3, tpc, true // backedge
		}
		if bi, ok := pcToBlock[tpc]; ok {
			return bi, 0, true
		}
		return -1, 0, false
	}

	jIdx, jExit, ok1 := resolveTarget(jumpPC)
	fIdx, fExit, ok2 := resolveTarget(fallPC)
	if !ok1 || !ok2 {
		return false
	}
	// If either branch exits or backedges directly, create a tiny trampoline block if needed so
	// TermCondBr always targets valid block indices inside LoopRegion.Blocks.
	ensureBlock := func(idx, exitPC int, isBack bool) int {
		if idx >= 0 {
			return idx
		}
		nb := &Block{ID: len(b.reg.Blocks), StartPC: exitPC}
		if isBack {
			nb.Term = Term{Kind: TermBackedge, DeoptPC: b.cand.EndPC}
		} else {
			nb.Term = Term{Kind: TermExit, ExitTargetPC: exitPC, DeoptPC: pc}
		}
		b.reg.Blocks = append(b.reg.Blocks, nb)
		return nb.ID
	}

	jumpBlk := ensureBlock(jIdx, jExit, jIdx == -3)
	fallBlk := ensureBlock(fIdx, fExit, fIdx == -3)

	// Build condition value (true => take jumpBlk if w.K()==true, or fallBlk if w.K()==false).
	a := uint8(w.A())
	var condVal ValID
	switch w.Op() {
	case bytecode.OP_TEST:
		v, ok := b.readSlot(blk, a, TBool, pc)
		if !ok {
			return false
		}
		// Lua OP_TEST: if (isTruthy(R[A]) != k) then skip next JMP; i.e. jump when isTruthy == k.
		if w.K() {
			condVal = v
		} else {
			condVal = b.emitBody(blk, Inst{Op: OpNotB, Type: TBool, Args: [3]ValID{v, NoVal, NoVal}, NumArgs: 1, DeoptPC: pc})
		}
	case bytecode.OP_EQ, bytecode.OP_LT, bytecode.OP_LE:
		condVal = b.emitCmpRR(blk, w.Op(), a, uint8(w.B()), w.K(), pc)
		if condVal == NoVal {
			return false
		}
	case bytecode.OP_EQI, bytecode.OP_LTI, bytecode.OP_LEI, bytecode.OP_GTI, bytecode.OP_GEI:
		condVal = b.emitCmpI(blk, w.Op(), a, int64(w.SB()), w.K(), pc)
		if condVal == NoVal {
			return false
		}
	case bytecode.OP_EQK:
		k := b.p.Consts[w.B()]
		if k.Kind == bytecode.ConstInt {
			condVal = b.emitCmpI(blk, bytecode.OP_EQI, a, k.Int, w.K(), pc)
		} else if k.Kind == bytecode.ConstFloat {
			// Only compare against a float constant when lhs is already float64
			// (avoid rounding int64 values > 2^53 into equal float64 values).
			lhs, ok := b.readSlot(blk, a, TUnknown, pc)
			if !ok || b.valType(lhs) != TFloat {
				return false
			}
			rhs := b.emitPre(Inst{Op: OpConstFloat, Type: TFloat, ImmU: math.Float64bits(k.Float)})
			cc := CmpEQ
			if !w.K() {
				cc = CmpNE
			}
			condVal = b.emitBody(blk, Inst{Op: OpCmpF, Type: TBool, Args: [3]ValID{lhs, rhs, NoVal}, NumArgs: 2, AuxCond: cc, DeoptPC: pc})
		} else {
			return false
		}
		if condVal == NoVal {
			return false
		}
	default:
		return false
	}

	blk.Term = Term{
		Kind:    TermCondBr,
		CondVal: condVal,
		Succ:    [2]int{jumpBlk, fallBlk},
		DeoptPC: pc,
	}
	return true
}

func (b *loopBuilder) emitCmpRR(blk *Block, op bytecode.OpCode, a, bSlot uint8, k bool, pc int) ValID {
	var cc CmpCond
	switch op {
	case bytecode.OP_EQ:
		cc = CmpEQ
	case bytecode.OP_LT:
		cc = CmpLT
	case bytecode.OP_LE:
		cc = CmpLE
	}
	// Note: for floats, !(a < b) is NOT (a >= b) on NaN! Instead we keep the true condition and swap succs via OpNotB if needed.
	// Wait: OpCmpF directly evaluates the IEEE-754 ordered relation for EQ/LT/LE/GT/GE, and for NE evaluates unordered-or-not-equal!
	// For !k with LT/LE on floats, emitting OpCmpF(LT) + OpNotB preserves exact IEEE-754 NaN semantics!
	isF := b.slotType[a] == TFloat || b.slotType[bSlot] == TFloat
	if isF {
		// Only compare as float64 when both operands are already float64 (avoid int64 > 2^53 precision loss).
		lhs, ok1 := b.readSlot(blk, a, TUnknown, pc)
		rhs, ok2 := b.readSlot(blk, bSlot, TUnknown, pc)
		if !ok1 || !ok2 || b.valType(lhs) != TFloat || b.valType(rhs) != TFloat {
			return NoVal
		}
		v := b.emitBody(blk, Inst{Op: OpCmpF, Type: TBool, Args: [3]ValID{lhs, rhs, NoVal}, NumArgs: 2, AuxCond: cc, DeoptPC: pc})
		if !k {
			v = b.emitBody(blk, Inst{Op: OpNotB, Type: TBool, Args: [3]ValID{v, NoVal, NoVal}, NumArgs: 1, DeoptPC: pc})
		}
		return v
	}
	lhs, ok1 := b.readSlot(blk, a, TInt, pc)
	rhs, ok2 := b.readSlot(blk, bSlot, TInt, pc)
	if !ok1 || !ok2 {
		return NoVal
	}
	if !k {
		cc = cc.Invert()
	}
	return b.emitBody(blk, Inst{Op: OpCmpI, Type: TBool, Args: [3]ValID{lhs, rhs, NoVal}, NumArgs: 2, AuxCond: cc, DeoptPC: pc})
}

func (b *loopBuilder) emitCmpI(blk *Block, op bytecode.OpCode, a uint8, imm int64, k bool, pc int) ValID {
	var cc CmpCond
	switch op {
	case bytecode.OP_EQI:
		cc = CmpEQ
	case bytecode.OP_LTI:
		cc = CmpLT
	case bytecode.OP_LEI:
		cc = CmpLE
	case bytecode.OP_GTI:
		cc = CmpGT
	case bytecode.OP_GEI:
		cc = CmpGE
	}
	lhs, ok := b.readSlot(blk, a, TUnknown, pc)
	if !ok {
		return NoVal
	}
	lt := b.valType(lhs)
	if lt == TFloat {
		// Only compare float64 against an integer immediate when the integer is
		// exactly representable in IEEE-754 float64 (|imm| <= 2^53).
		if imm < -(1<<53) || imm > (1<<53) {
			return NoVal
		}
		rhs := b.emitPre(Inst{Op: OpConstFloat, Type: TFloat, ImmU: math.Float64bits(float64(imm))})
		v := b.emitBody(blk, Inst{Op: OpCmpF, Type: TBool, Args: [3]ValID{lhs, rhs, NoVal}, NumArgs: 2, AuxCond: cc, DeoptPC: pc})
		if !k {
			v = b.emitBody(blk, Inst{Op: OpNotB, Type: TBool, Args: [3]ValID{v, NoVal, NoVal}, NumArgs: 1, DeoptPC: pc})
		}
		return v
	}
	if lt != TInt {
		return NoVal
	}
	rhs := b.emitPre(Inst{Op: OpConstInt, Type: TInt, ImmI: imm})
	if !k {
		cc = cc.Invert()
	}
	return b.emitBody(blk, Inst{Op: OpCmpI, Type: TBool, Args: [3]ValID{lhs, rhs, NoVal}, NumArgs: 2, AuxCond: cc, DeoptPC: pc})
}
