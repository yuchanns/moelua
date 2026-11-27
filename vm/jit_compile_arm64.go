//go:build arm64 && !moeluanojit && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"fmt"
	"math"
	"sync"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/internal/jit/arm64"
	"github.com/yuchanns/moelua/internal/jit/execmem"
	"github.com/yuchanns/moelua/internal/jit/ir"
)

// Pinned ARM64 registers across the baseline JIT:
//
//	X19: *jitExecCtx
//	X20: regs (&stack[base], frame R[0])
//	X21: consts (&bp.consts[0])
//	X22: pagePtrs (&s.arena.pagePtrs[0])
//	X23: stack (&th.stack[0])
//	X24: poll countdown
//	X25: shared trampoline epilogue address
//	X26: ic (&bp.ic[0])
//	X27: clUpvals (&cl.upvals[0], 0 if none)
//	X0..X17, D0..D31: free scratch registers
//	X18 (platform), X28 (Go g), X29 (FP), X30 (LR), SP: untouched
const (
	a64Ctx      = arm64.X19
	a64Regs     = arm64.X20
	a64Consts   = arm64.X21
	a64PagePtrs = arm64.X22
	a64Stack    = arm64.X23
	a64Poll     = arm64.X24
	a64Epilogue = arm64.X25
	a64IC       = arm64.X26
	a64ClUpvals = arm64.X27
)

var (
	a64TrampOnce     sync.Once
	a64TrampRegion   *execmem.Region
	a64TrampEntry    uintptr
	a64TrampEpilogue uintptr
	a64TrampOK       bool
)

func getJITTrampoline() (entry, epilogue uintptr, ok bool) {
	a64TrampOnce.Do(func() {
		a := arm64.New(64)
		fl := arm64.ComputeFrameLayout(0, []arm64.Reg{
			a64Ctx, a64Regs, a64Consts, a64PagePtrs, a64Stack,
			a64Poll, a64Epilogue, a64IC, a64ClUpvals,
		})
		a.EmitPrologue(fl)
		a.MOV(a64Ctx, arm64.X0)
		a.LDR(a64Regs, a64Ctx, jitCtxRegs)
		a.LDR(a64Consts, a64Ctx, jitCtxConsts)
		a.LDR(a64PagePtrs, a64Ctx, jitCtxPagePtrs)
		a.LDR(a64Stack, a64Ctx, jitCtxStack)
		a.LDR(a64Poll, a64Ctx, jitCtxPoll)
		a.LDR(a64Epilogue, a64Ctx, jitCtxEpilogue)
		a.LDR(a64IC, a64Ctx, jitCtxIC)
		a.LDR(a64ClUpvals, a64Ctx, jitCtxClUpvals)
		a.LDR(arm64.X16, a64Ctx, jitCtxEntryPC)
		a.BR(arm64.X16)

		epOff := a.Len()
		// Sync pinned registers back into ctx before returning X0 (exitCode) to Go.
		a.STR(a64Regs, a64Ctx, jitCtxRegs)
		a.STR(a64Consts, a64Ctx, jitCtxConsts)
		a.STR(a64IC, a64Ctx, jitCtxIC)
		a.STR(a64ClUpvals, a64Ctx, jitCtxClUpvals)
		a.STR(a64Poll, a64Ctx, jitCtxPoll)
		a.EmitEpilogue(fl)

		code, err := a.Finish()
		if err != nil {
			return
		}
		reg, err := execmem.Publish(code)
		if err != nil {
			return
		}
		a64TrampRegion = reg
		a64TrampEntry = reg.Addr()
		a64TrampEpilogue = reg.Addr() + uintptr(epOff)
		a64TrampOK = true
	})
	return a64TrampEntry, a64TrampEpilogue, a64TrampOK
}

type a64Stub struct {
	label arm64.Label
	pc    int
	code  uint64
}

type a64Baseline struct {
	a           *arm64.Assembler
	bp          *boundProto
	code        []bytecode.Instruction
	pcLabels    []arm64.Label
	pcOffsets   []int
	coldMap     map[int]arm64.Label
	pollMap     map[int]arm64.Label
	stubs       []a64Stub
	retRestore  arm64.Label
	needRetRest bool
	exitDone    arm64.Label
	needDone    bool
	exitReload  arm64.Label
	needReload  bool
	optLoops    []*a64OptLoopRef
	optByEntry  map[int]*a64OptLoopRef
	optByEnd    map[int]*a64OptLoopRef
}

func compileBaselineNative(bp *boundProto, fb *ir.Feedback) (*jitFunc, error) {
	code := bp.proto.Code
	n := len(code)
	if n == 0 {
		return nil, fmt.Errorf("jit/arm64: empty proto")
	}
	c := &a64Baseline{
		a:         arm64.New(max(64, n*14)),
		bp:        bp,
		code:      code,
		pcLabels:  make([]arm64.Label, n+1),
		pcOffsets: make([]int, n),
		coldMap:   make(map[int]arm64.Label, 16),
		pollMap:   make(map[int]arm64.Label, 8),
	}
	for i := range n + 1 {
		c.pcLabels[i] = c.a.NewLabel()
	}

	if optRegs := ir.OptimizeProto(bp.proto, fb, a64OptNumGPR, a64OptNumFP, a64OptMaxSpills); len(optRegs) > 0 {
		c.optByEntry = make(map[int]*a64OptLoopRef, len(optRegs))
		c.optByEnd = make(map[int]*a64OptLoopRef, len(optRegs))
		for ipc := 0; ipc < n; ipc++ {
			if r, ok := optRegs[ipc]; ok {
				ref := &a64OptLoopRef{
					reg:       r,
					entryLbl:  c.a.NewLabel(),
					resumeLbl: c.a.NewLabel(),
					baseLbl:   c.a.NewLabel(),
				}
				c.optLoops = append(c.optLoops, ref)
				c.optByEntry[r.EntryPC] = ref
				if r.Kind == ir.LoopForInt {
					c.optByEnd[r.EndPC] = ref
				}
			}
		}
	}

	for ipc := 0; ipc < n; ipc++ {
		c.pcOffsets[ipc] = c.a.Len()
		c.a.Bind(c.pcLabels[ipc])
		if ref, ok := c.optByEntry[ipc]; ok {
			c.a.B(ref.entryLbl)
			c.a.Bind(ref.baseLbl)
		}
		c.emitOp(ipc, code[ipc])
	}
	c.a.Bind(c.pcLabels[n])

	c.emitAllOptLoops()

	if c.needRetRest {
		c.emitReturnRestoreCaller()
	}
	if c.needDone {
		c.a.Bind(c.exitDone)
		c.a.MOVImm64(arm64.X0, jitExitDone)
		c.a.BR(a64Epilogue)
	}
	if c.needReload {
		c.a.Bind(c.exitReload)
		c.a.MOVImm64(arm64.X0, jitExitReload)
		c.a.BR(a64Epilogue)
	}
	for _, st := range c.stubs {
		c.a.Bind(st.label)
		c.a.MOVImm64(arm64.X1, uint64(st.pc))
		c.a.STR(arm64.X1, a64Ctx, jitCtxPC)
		c.a.MOVImm64(arm64.X0, st.code)
		c.a.BR(a64Epilogue)
	}

	bytes, err := c.a.Finish()
	if err != nil {
		return nil, err
	}
	reg, err := execmem.Publish(bytes)
	if err != nil {
		return nil, err
	}
	base := reg.Addr()
	entries := make([]uintptr, n)
	for i, off := range c.pcOffsets {
		entries[i] = base + uintptr(off)
	}
	return &jitFunc{
		proto:     bp.proto,
		region:    reg,
		pcEntries: entries,
	}, nil
}

func (c *a64Baseline) cold(ipc int) arm64.Label {
	if l, ok := c.coldMap[ipc]; ok {
		return l
	}
	l := c.a.NewLabel()
	c.coldMap[ipc] = l
	c.stubs = append(c.stubs, a64Stub{label: l, pc: ipc, code: jitExitCold})
	return l
}

func (c *a64Baseline) poll(targetPC int) arm64.Label {
	if l, ok := c.pollMap[targetPC]; ok {
		return l
	}
	l := c.a.NewLabel()
	c.pollMap[targetPC] = l
	c.stubs = append(c.stubs, a64Stub{label: l, pc: targetPC, code: jitExitPoll})
	return l
}

func (c *a64Baseline) doneLabel() arm64.Label {
	if !c.needDone {
		c.needDone = true
		c.exitDone = c.a.NewLabel()
	}
	return c.exitDone
}

func (c *a64Baseline) reloadLabel() arm64.Label {
	if !c.needReload {
		c.needReload = true
		c.exitReload = c.a.NewLabel()
	}
	return c.exitReload
}

func (c *a64Baseline) retRestoreLabel() arm64.Label {
	if !c.needRetRest {
		c.needRetRest = true
		c.retRestore = c.a.NewLabel()
	}
	return c.retRestore
}

// --- Register & Arena Helpers ---

func (c *a64Baseline) loadReg(slot int, bitsReg, tagReg arm64.Reg) {
	off := int32(slot * 16)
	if off <= 504 {
		c.a.LDP(bitsReg, tagReg, a64Regs, off)
		return
	}
	c.a.LDR(bitsReg, a64Regs, off)
	c.a.LDR(tagReg, a64Regs, off+8)
}

func (c *a64Baseline) storeReg(slot int, bitsReg, tagReg arm64.Reg) {
	off := int32(slot * 16)
	if off <= 504 {
		c.a.STP(bitsReg, tagReg, a64Regs, off)
		return
	}
	c.a.STR(bitsReg, a64Regs, off)
	c.a.STR(tagReg, a64Regs, off+8)
}

func (c *a64Baseline) loadConst(idx int, bitsReg, tagReg arm64.Reg) {
	off := int32(idx * 16)
	if off <= 504 {
		c.a.LDP(bitsReg, tagReg, a64Consts, off)
		return
	}
	if off+8 < (1<<12)*8 {
		c.a.LDR(bitsReg, a64Consts, off)
		c.a.LDR(tagReg, a64Consts, off+8)
		return
	}
	c.a.MOVImm32(bitsReg, uint32(off))
	c.a.ADD(bitsReg, a64Consts, bitsReg)
	c.a.LDP(bitsReg, tagReg, bitsReg, 0)
}

func (c *a64Baseline) loadRegOrConst(isK bool, idx int, bitsReg, tagReg arm64.Reg) {
	if isK {
		c.loadConst(idx, bitsReg, tagReg)
	} else {
		c.loadReg(idx, bitsReg, tagReg)
	}
}

// emitResolveSmall resolves a non-null, non-large Arena ObjRef in ref into dst using tmp.
// ref and dst may alias; tmp must be distinct from ref and dst.
func (c *a64Baseline) emitResolveSmall(ref, dst, tmp arm64.Reg) {
	c.a.LSRImm(tmp, ref, jitArenaWordBits)
	c.a.LDRReg(tmp, a64PagePtrs, tmp, true) // page base
	c.a.ANDImm(dst, ref, jitArenaWordMask)
	c.a.ADDShift(dst, tmp, dst, arm64.ShiftLSL, jitArenaAlignShift)
}

// emitResolveFast resolves either a small-slab or large-span Arena ObjRef in ref into dst.
func (c *a64Baseline) emitResolveFast(ref, dst, tmp arm64.Reg) {
	lLarge := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.TBNZ(ref, jitArenaLargeBit, lLarge)
	c.emitResolveSmall(ref, dst, tmp)
	c.a.B(lDone)
	c.a.Bind(lLarge)
	c.a.ANDImm(tmp, ref, 0x7FFFFFFF)
	c.a.ADDShift(tmp, tmp, tmp, arm64.ShiftLSL, 1) // idx * 3 (24B slice header = 3*8B)
	c.a.LDR(dst, a64Ctx, jitCtxLargeSpans)
	c.a.LDRReg(dst, dst, tmp, true)
	c.a.Bind(lDone)
}

// branchBitSet branches to target if bit is set in reg, using TSTImm + B.NE (±1 MiB range)
// rather than TBNZ (±32 KiB) so large functions can reach out-of-line cold stubs.
func (c *a64Baseline) branchBitSet(reg arm64.Reg, bit uint8, target arm64.Label) {
	c.a.TSTImm(reg, uint64(1)<<bit)
	c.a.BCond(arm64.NE, target)
}

func (c *a64Baseline) emitJumpToPC(target int, isBack bool) {
	if isBack {
		c.a.SUBSImm(a64Poll, a64Poll, 1)
		c.a.BCond(arm64.LE, c.poll(target))
	}
	c.a.B(c.pcLabels[target])
}

// emitBranchIfCondTrue emits a fused comparison jump using the OP_JMP at ipc+1.
// If cond is true, branches to jumpTarget (when w.K()==true) or skipTarget (when w.K()==false);
// if cond is false, branches to the opposite target.
func (c *a64Baseline) emitFusedCond(ipc int, w bytecode.Instruction, trueCond arm64.Cond) {
	if ipc+1 >= len(c.code) || c.code[ipc+1].Op() != bytecode.OP_JMP {
		c.a.B(c.cold(ipc))
		return
	}
	sj := c.code[ipc+1].SJ()
	jumpTarget := ipc + 2 + sj
	skipTarget := ipc + 2
	if jumpTarget < 0 || jumpTarget > len(c.code) {
		c.a.B(c.cold(ipc))
		return
	}
	takeCond := trueCond
	if !w.K() {
		takeCond = trueCond.Invert()
	}
	if sj < 0 {
		lSkip := c.pcLabels[skipTarget]
		c.a.BCond(takeCond.Invert(), lSkip)
		c.a.SUBSImm(a64Poll, a64Poll, 1)
		c.a.BCond(arm64.LE, c.poll(jumpTarget))
		c.a.B(c.pcLabels[jumpTarget])
		return
	}
	c.a.BCond(takeCond, c.pcLabels[jumpTarget])
	c.a.B(c.pcLabels[skipTarget])
}

func (c *a64Baseline) emitOp(ipc int, w bytecode.Instruction) {
	a := w.A()
	switch w.Op() {
	case bytecode.OP_MOVE:
		c.loadReg(w.B(), arm64.X0, arm64.X1)
		c.storeReg(a, arm64.X0, arm64.X1)

	case bytecode.OP_LOADI:
		c.a.MOVImm64(arm64.X0, uint64(int64(w.SBx())))
		c.storeReg(a, arm64.X0, arm64.XZR) // tagInt == 0

	case bytecode.OP_LOADF:
		c.a.MOVImm64(arm64.X0, math.Float64bits(float64(w.SBx())))
		c.a.MOVZ(arm64.X1, uint16(tagFloat), 0)
		c.storeReg(a, arm64.X0, arm64.X1)

	case bytecode.OP_LOADK:
		bx := w.Bx()
		kc := c.bp.proto.Consts[bx]
		switch kc.Kind {
		case bytecode.ConstInt:
			c.a.MOVImm64(arm64.X0, uint64(kc.Int))
			c.storeReg(a, arm64.X0, arm64.XZR)
		case bytecode.ConstFloat:
			c.a.MOVImm64(arm64.X0, math.Float64bits(kc.Float))
			c.a.MOVZ(arm64.X1, uint16(tagFloat), 0)
			c.storeReg(a, arm64.X0, arm64.X1)
		case bytecode.ConstNil:
			c.a.MOVZ(arm64.X1, uint16(tagNil), 0)
			c.storeReg(a, arm64.XZR, arm64.X1)
		case bytecode.ConstFalse:
			c.a.MOVZ(arm64.X1, uint16(tagFalse), 0)
			c.storeReg(a, arm64.XZR, arm64.X1)
		case bytecode.ConstTrue:
			c.a.MOVZ(arm64.X1, uint16(tagTrue), 0)
			c.storeReg(a, arm64.XZR, arm64.X1)
		default:
			c.loadConst(bx, arm64.X0, arm64.X1)
			c.storeReg(a, arm64.X0, arm64.X1)
		}

	case bytecode.OP_LOADFALSE:
		c.a.MOVZ(arm64.X1, uint16(tagFalse), 0)
		c.storeReg(a, arm64.XZR, arm64.X1)

	case bytecode.OP_LFALSESKIP:
		c.a.MOVZ(arm64.X1, uint16(tagFalse), 0)
		c.storeReg(a, arm64.XZR, arm64.X1)
		c.a.B(c.pcLabels[ipc+2])

	case bytecode.OP_LOADTRUE:
		c.a.MOVZ(arm64.X1, uint16(tagTrue), 0)
		c.storeReg(a, arm64.XZR, arm64.X1)

	case bytecode.OP_LOADNIL:
		b := w.B()
		c.a.MOVZ(arm64.X1, uint16(tagNil), 0)
		for i := 0; i <= b; i++ {
			c.storeReg(a+i, arm64.XZR, arm64.X1)
		}

	case bytecode.OP_GETUPVAL:
		c.emitGetUpval(ipc, a, w.B())

	case bytecode.OP_SETUPVAL:
		c.emitSetUpval(ipc, a, w.B())

	case bytecode.OP_GETTABUP:
		c.emitGetTabUp(ipc, w)

	case bytecode.OP_SETTABUP:
		c.emitSetTabUp(ipc, w)

	case bytecode.OP_GETFIELD:
		c.emitGetField(ipc, w, false)

	case bytecode.OP_SELF:
		c.emitGetField(ipc, w, true)

	case bytecode.OP_SETFIELD:
		c.emitSetField(ipc, w)

	case bytecode.OP_GETI:
		c.emitGetI(ipc, w)

	case bytecode.OP_GETTABLE:
		c.emitGetTable(ipc, w)

	case bytecode.OP_SETI:
		c.emitSetI(ipc, w)

	case bytecode.OP_SETTABLE:
		c.emitSetTable(ipc, w)

	case bytecode.OP_ADDI:
		c.emitAddI(ipc, w)

	case bytecode.OP_ADDK, bytecode.OP_SUBK, bytecode.OP_MULK, bytecode.OP_DIVK, bytecode.OP_IDIVK, bytecode.OP_MODK:
		c.emitArithK(ipc, w)

	case bytecode.OP_ADD, bytecode.OP_SUB, bytecode.OP_MUL, bytecode.OP_DIV, bytecode.OP_IDIV, bytecode.OP_MOD:
		c.emitArith(ipc, w)

	case bytecode.OP_BANDK, bytecode.OP_BORK, bytecode.OP_BXORK:
		c.emitBitwiseK(ipc, w)

	case bytecode.OP_BAND, bytecode.OP_BOR, bytecode.OP_BXOR:
		c.emitBitwise(ipc, w)

	case bytecode.OP_SHLI, bytecode.OP_SHRI:
		c.emitShiftImm(ipc, w)

	case bytecode.OP_SHL, bytecode.OP_SHR:
		c.emitShiftReg(ipc, w)

	case bytecode.OP_UNM:
		c.emitUnm(ipc, w)

	case bytecode.OP_BNOT:
		c.loadReg(w.B(), arm64.X0, arm64.X1)
		c.a.CBNZ(arm64.X1, c.cold(ipc))
		c.a.MVN(arm64.X0, arm64.X0)
		c.storeReg(a, arm64.X0, arm64.XZR)

	case bytecode.OP_NOT:
		c.loadReg(w.B(), arm64.X0, arm64.X1)
		c.a.SUBImm(arm64.X1, arm64.X1, uint32(tagNil))
		c.a.CMPImm(arm64.X1, 1) // falsy iff (tag - tagNil) <= 1 unsigned
		c.a.MOVZ(arm64.X2, uint16(tagTrue), 0)
		c.a.MOVZ(arm64.X3, uint16(tagFalse), 0)
		c.a.CSEL(arm64.X1, arm64.X2, arm64.X3, arm64.LS)
		c.storeReg(a, arm64.XZR, arm64.X1)

	case bytecode.OP_LEN:
		c.emitLen(ipc, w)

	case bytecode.OP_JMP:
		sj := w.SJ()
		target := ipc + 1 + sj
		if target < 0 || target > len(c.code) {
			c.a.B(c.cold(ipc))
			return
		}
		c.emitJumpToPC(target, sj < 0)

	case bytecode.OP_EQ:
		c.emitEQ(ipc, w)
	case bytecode.OP_LT:
		c.emitOrd(ipc, w, arm64.LT, arm64.MI)
	case bytecode.OP_LE:
		c.emitOrd(ipc, w, arm64.LE, arm64.LS)
	case bytecode.OP_EQK:
		c.emitEQK(ipc, w)
	case bytecode.OP_EQI:
		c.emitEQI(ipc, w)
	case bytecode.OP_LTI:
		c.emitOrdI(ipc, w, arm64.LT, arm64.MI)
	case bytecode.OP_LEI:
		c.emitOrdI(ipc, w, arm64.LE, arm64.LS)
	case bytecode.OP_GTI:
		c.emitOrdI(ipc, w, arm64.GT, arm64.GT)
	case bytecode.OP_GEI:
		c.emitOrdI(ipc, w, arm64.GE, arm64.GE)
	case bytecode.OP_TEST:
		c.emitTest(ipc, w)
	case bytecode.OP_TESTSET:
		c.emitTestSet(ipc, w)

	case bytecode.OP_FORPREP:
		c.emitForPrep(ipc, w)
	case bytecode.OP_FORLOOP:
		c.emitForLoop(ipc, w)
	case bytecode.OP_TFORLOOP:
		target := ipc + 1 - w.Bx()
		if target < 0 || target >= len(c.code) {
			c.a.B(c.cold(ipc))
			return
		}
		c.loadReg(a+3, arm64.X0, arm64.X1)
		c.a.CMPImm(arm64.X1, uint32(tagNil))
		c.a.BCond(arm64.EQ, c.pcLabels[ipc+1])
		c.emitJumpToPC(target, true)

	case bytecode.OP_CALL:
		c.emitCall(ipc, w)
	case bytecode.OP_TAILCALL:
		c.emitTailCall(ipc, w)
	case bytecode.OP_RETURN0:
		c.emitReturn0()
	case bytecode.OP_RETURN1:
		c.emitReturn1(a)
	case bytecode.OP_RETURN:
		c.emitReturn(ipc, w)

	default:
		c.a.B(c.cold(ipc))
	}
}

// --- Upvalues ---

// emitLoadUpval resolves closure upvalue b into (bitsReg, tagReg); exits cold on cross-thread open upval.
func (c *a64Baseline) emitLoadUpval(ipc, b int, bitsReg, tagReg, tmp1, tmp2 arm64.Reg) {
	c.a.CBZ(a64ClUpvals, c.cold(ipc))
	c.a.LDRW(tmp1, a64ClUpvals, int32(b*4))
	c.emitResolveSmall(tmp1, tmp1, tmp2) // tmp1 = *Upvalue
	c.a.LDRW(tmp2, tmp1, jitUvOwnerRef)
	lOpen := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.CBNZ(tmp2, lOpen)
	c.a.LDP(bitsReg, tagReg, tmp1, jitUvClosed)
	c.a.B(lDone)
	c.a.Bind(lOpen)
	c.a.LDRW(bitsReg, a64Ctx, jitCtxThSelf)
	c.a.CMPW(tmp2, bitsReg)
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.LDRSW(tmp2, tmp1, jitUvStackIdx)
	c.a.ADDShift(tmp2, a64Stack, tmp2, arm64.ShiftLSL, 4)
	c.a.LDP(bitsReg, tagReg, tmp2, 0)
	c.a.Bind(lDone)
}

func (c *a64Baseline) emitGetUpval(ipc, a, b int) {
	c.emitLoadUpval(ipc, b, arm64.X0, arm64.X1, arm64.X2, arm64.X3)
	c.storeReg(a, arm64.X0, arm64.X1)
}

func (c *a64Baseline) emitSetUpval(ipc, a, b int) {
	c.a.CBZ(a64ClUpvals, c.cold(ipc))
	c.a.LDRW(arm64.X2, a64ClUpvals, int32(b*4))
	c.emitResolveSmall(arm64.X2, arm64.X2, arm64.X3) // X2 = *Upvalue
	c.loadReg(a, arm64.X0, arm64.X1)
	c.a.LDRW(arm64.X3, arm64.X2, jitUvOwnerRef)
	lOpen := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.CBNZ(arm64.X3, lOpen)
	// Closed upvalue: check GC barrier if val is GC object and upval is black.
	lStoreClosed := c.a.NewLabel()
	c.a.SUBImm(arm64.X4, arm64.X1, uint32(tagFirstGC))
	c.a.CMPImm(arm64.X4, uint32(tagLastGC-tagFirstGC))
	c.a.BCond(arm64.HI, lStoreClosed)
	c.a.LDRB(arm64.X4, arm64.X2, jitUvMark)
	c.branchBitSet(arm64.X4, jitMarkBlackBit, c.cold(ipc))
	c.a.Bind(lStoreClosed)
	c.a.STP(arm64.X0, arm64.X1, arm64.X2, jitUvClosed)
	c.a.B(lDone)
	c.a.Bind(lOpen)
	c.a.LDRW(arm64.X4, a64Ctx, jitCtxThSelf)
	c.a.CMPW(arm64.X3, arm64.X4)
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.LDRSW(arm64.X3, arm64.X2, jitUvStackIdx)
	c.a.ADDShift(arm64.X3, a64Stack, arm64.X3, arm64.ShiftLSL, 4)
	c.a.STP(arm64.X0, arm64.X1, arm64.X3, 0)
	c.a.Bind(lDone)
}

// --- Arithmetic & Bitwise ---

func (c *a64Baseline) emitAddI(ipc int, w bytecode.Instruction) {
	a := w.A()
	sc := int64(w.SC())
	c.loadReg(w.B(), arm64.X0, arm64.X1)
	lFloat := c.a.NewLabel()
	c.a.CBNZ(arm64.X1, lFloat)
	if sc >= 0 {
		c.a.ADDImm(arm64.X0, arm64.X0, uint32(sc))
	} else {
		c.a.SUBImm(arm64.X0, arm64.X0, uint32(-sc))
	}
	c.storeReg(a, arm64.X0, arm64.XZR)
	c.a.B(c.pcLabels[ipc+2])

	c.a.Bind(lFloat)
	c.a.CMPImm(arm64.X1, uint32(tagFloat))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.FMOVToF(arm64.D0, arm64.X0)
	c.a.MOVImm64(arm64.X2, math.Float64bits(float64(sc)))
	c.a.FMOVToF(arm64.D1, arm64.X2)
	c.a.FADD(arm64.D0, arm64.D0, arm64.D1)
	c.a.FMOVToG(arm64.X0, arm64.D0)
	c.storeReg(a, arm64.X0, arm64.X1)
	c.a.B(c.pcLabels[ipc+2])
}

// emitIntFloorDivTo writes Lua 5.5 floor(lhs / rhs) into dst (distinct from lhs, rhs, tmp).
func (c *a64Baseline) emitIntFloorDivTo(ipc int, dst, lhs, rhs, tmp arm64.Reg) {
	c.a.CBZ(rhs, c.cold(ipc))
	lNotNeg1 := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.CMNImm(rhs, 1)
	c.a.BCond(arm64.NE, lNotNeg1)
	c.a.NEG(dst, lhs)
	c.a.B(lDone)
	c.a.Bind(lNotNeg1)
	c.a.SDIV(dst, lhs, rhs)      // dst = q
	c.a.MSUB(tmp, dst, rhs, lhs) // tmp = r
	c.a.CBZ(tmp, lDone)
	c.a.EOR(tmp, lhs, rhs)
	c.a.TBZ(tmp, 63, lDone) // same sign -> done
	c.a.SUBImm(dst, dst, 1)
	c.a.Bind(lDone)
}

// emitIntFloorModTo writes Lua 5.5 floor(lhs % rhs) into dst (distinct from lhs, rhs, tmp).
func (c *a64Baseline) emitIntFloorModTo(ipc int, dst, lhs, rhs, tmp arm64.Reg) {
	c.a.CBZ(rhs, c.cold(ipc))
	lNotNeg1 := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.CMNImm(rhs, 1)
	c.a.BCond(arm64.NE, lNotNeg1)
	c.a.MOVZ(dst, 0, 0)
	c.a.B(lDone)
	c.a.Bind(lNotNeg1)
	c.a.SDIV(tmp, lhs, rhs)
	c.a.MSUB(dst, tmp, rhs, lhs) // dst = r
	c.a.CBZ(dst, lDone)
	c.a.EOR(tmp, dst, rhs)
	c.a.TBZ(tmp, 63, lDone)
	c.a.ADD(dst, dst, rhs)
	c.a.Bind(lDone)
}

func (c *a64Baseline) emitArithK(ipc int, w bytecode.Instruction) {
	op := w.Op()
	a := w.A()
	kc := c.bp.proto.Consts[w.C()]
	if kc.Kind != bytecode.ConstInt && kc.Kind != bytecode.ConstFloat {
		c.a.B(c.cold(ipc))
		return
	}
	c.loadReg(w.B(), arm64.X0, arm64.X1)
	if kc.Kind == bytecode.ConstInt && op != bytecode.OP_DIVK {
		lFloat := c.a.NewLabel()
		c.a.CBNZ(arm64.X1, lFloat)
		c.a.MOVImm64(arm64.X2, uint64(kc.Int))
		switch op {
		case bytecode.OP_ADDK:
			c.a.ADD(arm64.X3, arm64.X0, arm64.X2)
		case bytecode.OP_SUBK:
			c.a.SUB(arm64.X3, arm64.X0, arm64.X2)
		case bytecode.OP_MULK:
			c.a.MUL(arm64.X3, arm64.X0, arm64.X2)
		case bytecode.OP_IDIVK:
			c.emitIntFloorDivTo(ipc, arm64.X3, arm64.X0, arm64.X2, arm64.X4)
		case bytecode.OP_MODK:
			c.emitIntFloorModTo(ipc, arm64.X3, arm64.X0, arm64.X2, arm64.X4)
		}
		c.storeReg(a, arm64.X3, arm64.XZR)
		c.a.B(c.pcLabels[ipc+2])

		c.a.Bind(lFloat)
		if op == bytecode.OP_MODK {
			c.a.B(c.cold(ipc))
			return
		}
		c.a.CMPImm(arm64.X1, uint32(tagFloat))
		c.a.BCond(arm64.NE, c.cold(ipc))
		c.a.FMOVToF(arm64.D0, arm64.X0)
		c.a.MOVImm64(arm64.X2, math.Float64bits(float64(kc.Int)))
		c.a.FMOVToF(arm64.D1, arm64.X2)
		c.emitFPBin(op, arm64.D0, arm64.D0, arm64.D1)
		c.a.FMOVToG(arm64.X0, arm64.D0)
		c.storeReg(a, arm64.X0, arm64.X1)
		c.a.B(c.pcLabels[ipc+2])
		return
	}

	// Float constant or OP_DIVK: result is always float.
	if op == bytecode.OP_MODK {
		c.a.B(c.cold(ipc))
		return
	}
	var kf float64
	if kc.Kind == bytecode.ConstFloat {
		kf = kc.Float
	} else {
		kf = float64(kc.Int)
	}
	lInt := c.a.NewLabel()
	lReady := c.a.NewLabel()
	c.a.CBZ(arm64.X1, lInt)
	c.a.CMPImm(arm64.X1, uint32(tagFloat))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.FMOVToF(arm64.D0, arm64.X0)
	c.a.B(lReady)
	c.a.Bind(lInt)
	c.a.SCVTF(arm64.D0, arm64.X0)
	c.a.Bind(lReady)
	c.a.MOVImm64(arm64.X2, math.Float64bits(kf))
	c.a.FMOVToF(arm64.D1, arm64.X2)
	c.emitFPBin(op, arm64.D0, arm64.D0, arm64.D1)
	c.a.FMOVToG(arm64.X0, arm64.D0)
	c.a.MOVZ(arm64.X1, uint16(tagFloat), 0)
	c.storeReg(a, arm64.X0, arm64.X1)
	c.a.B(c.pcLabels[ipc+2])
}

func (c *a64Baseline) emitFPBin(op bytecode.OpCode, fd, fn, fm arm64.FReg) {
	switch op {
	case bytecode.OP_ADD, bytecode.OP_ADDK:
		c.a.FADD(fd, fn, fm)
	case bytecode.OP_SUB, bytecode.OP_SUBK:
		c.a.FSUB(fd, fn, fm)
	case bytecode.OP_MUL, bytecode.OP_MULK:
		c.a.FMUL(fd, fn, fm)
	case bytecode.OP_DIV, bytecode.OP_DIVK:
		c.a.FDIV(fd, fn, fm)
	case bytecode.OP_IDIV, bytecode.OP_IDIVK:
		c.a.FDIV(fd, fn, fm)
		c.a.FRINTM(fd, fd)
	}
}

func (c *a64Baseline) emitArith(ipc int, w bytecode.Instruction) {
	op := w.Op()
	a := w.A()
	c.loadReg(w.B(), arm64.X0, arm64.X1) // lhs = (X0, X1)
	c.loadReg(w.C(), arm64.X2, arm64.X3) // rhs = (X2, X3)

	if op != bytecode.OP_DIV {
		// Fast both-int path: (X1 | X3) == 0
		lNotBothInt := c.a.NewLabel()
		c.a.ORR(arm64.X4, arm64.X1, arm64.X3)
		c.a.CBNZ(arm64.X4, lNotBothInt)
		switch op {
		case bytecode.OP_ADD:
			c.a.ADD(arm64.X5, arm64.X0, arm64.X2)
		case bytecode.OP_SUB:
			c.a.SUB(arm64.X5, arm64.X0, arm64.X2)
		case bytecode.OP_MUL:
			c.a.MUL(arm64.X5, arm64.X0, arm64.X2)
		case bytecode.OP_IDIV:
			c.emitIntFloorDivTo(ipc, arm64.X5, arm64.X0, arm64.X2, arm64.X4)
		case bytecode.OP_MOD:
			c.emitIntFloorModTo(ipc, arm64.X5, arm64.X0, arm64.X2, arm64.X4)
		}
		c.storeReg(a, arm64.X5, arm64.XZR)
		c.a.B(c.pcLabels[ipc+2])
		c.a.Bind(lNotBothInt)
		if op == bytecode.OP_MOD {
			c.a.B(c.cold(ipc))
			return
		}
	}

	// Both operands must be numeric (tag <= tagFloat == 1).
	c.a.ORR(arm64.X4, arm64.X1, arm64.X3)
	c.a.CMPImm(arm64.X4, uint32(tagFloat))
	c.a.BCond(arm64.HI, c.cold(ipc))

	// Convert lhs -> D0
	lLhsFloat := c.a.NewLabel()
	lLhsDone := c.a.NewLabel()
	c.a.CBNZ(arm64.X1, lLhsFloat)
	c.a.SCVTF(arm64.D0, arm64.X0)
	c.a.B(lLhsDone)
	c.a.Bind(lLhsFloat)
	c.a.FMOVToF(arm64.D0, arm64.X0)
	c.a.Bind(lLhsDone)

	// Convert rhs -> D1
	lRhsFloat := c.a.NewLabel()
	lRhsDone := c.a.NewLabel()
	c.a.CBNZ(arm64.X3, lRhsFloat)
	c.a.SCVTF(arm64.D1, arm64.X2)
	c.a.B(lRhsDone)
	c.a.Bind(lRhsFloat)
	c.a.FMOVToF(arm64.D1, arm64.X2)
	c.a.Bind(lRhsDone)

	c.emitFPBin(op, arm64.D0, arm64.D0, arm64.D1)
	c.a.FMOVToG(arm64.X0, arm64.D0)
	c.a.MOVZ(arm64.X1, uint16(tagFloat), 0)
	c.storeReg(a, arm64.X0, arm64.X1)
	c.a.B(c.pcLabels[ipc+2])
}

func (c *a64Baseline) emitBitwiseK(ipc int, w bytecode.Instruction) {
	kc := c.bp.proto.Consts[w.C()]
	if kc.Kind != bytecode.ConstInt {
		c.a.B(c.cold(ipc))
		return
	}
	c.loadReg(w.B(), arm64.X0, arm64.X1)
	c.a.CBNZ(arm64.X1, c.cold(ipc))
	c.a.MOVImm64(arm64.X2, uint64(kc.Int))
	switch w.Op() {
	case bytecode.OP_BANDK:
		c.a.AND(arm64.X0, arm64.X0, arm64.X2)
	case bytecode.OP_BORK:
		c.a.ORR(arm64.X0, arm64.X0, arm64.X2)
	case bytecode.OP_BXORK:
		c.a.EOR(arm64.X0, arm64.X0, arm64.X2)
	}
	c.storeReg(w.A(), arm64.X0, arm64.XZR)
	c.a.B(c.pcLabels[ipc+2])
}

func (c *a64Baseline) emitBitwise(ipc int, w bytecode.Instruction) {
	c.loadReg(w.B(), arm64.X0, arm64.X1)
	c.loadReg(w.C(), arm64.X2, arm64.X3)
	c.a.ORR(arm64.X4, arm64.X1, arm64.X3)
	c.a.CBNZ(arm64.X4, c.cold(ipc))
	switch w.Op() {
	case bytecode.OP_BAND:
		c.a.AND(arm64.X0, arm64.X0, arm64.X2)
	case bytecode.OP_BOR:
		c.a.ORR(arm64.X0, arm64.X0, arm64.X2)
	case bytecode.OP_BXOR:
		c.a.EOR(arm64.X0, arm64.X0, arm64.X2)
	}
	c.storeReg(w.A(), arm64.X0, arm64.XZR)
	c.a.B(c.pcLabels[ipc+2])
}

func (c *a64Baseline) emitShiftImm(ipc int, w bytecode.Instruction) {
	// Lua 5.5: OP_SHRI: R[A] = R[B] >> sC; OP_SHLI: R[A] = sC << R[B] (signed shift!).
	c.loadReg(w.B(), arm64.X0, arm64.X1)
	c.a.CBNZ(arm64.X1, c.cold(ipc))
	sc := w.SC()
	if w.Op() == bytecode.OP_SHRI {
		if sc >= 64 || sc <= -64 {
			c.a.MOVZ(arm64.X0, 0, 0)
		} else if sc > 0 {
			c.a.LSRImm(arm64.X0, arm64.X0, uint8(sc))
		} else if sc < 0 {
			c.a.LSLImm(arm64.X0, arm64.X0, uint8(-sc))
		}
		c.storeReg(w.A(), arm64.X0, arm64.XZR)
		c.a.B(c.pcLabels[ipc+2])
		return
	}
	// OP_SHLI: R[A] = int64(sC) << R[B] (Lua shifts handle negative amounts and |y|>=64 -> 0)
	c.a.MOVImm64(arm64.X2, uint64(int64(sc)))
	c.emitLuaShift(arm64.X3, arm64.X2, arm64.X0, true)
	c.storeReg(w.A(), arm64.X3, arm64.XZR)
	c.a.B(c.pcLabels[ipc+2])
}

func (c *a64Baseline) emitShiftReg(ipc int, w bytecode.Instruction) {
	c.loadReg(w.B(), arm64.X0, arm64.X1)
	c.loadReg(w.C(), arm64.X2, arm64.X3)
	c.a.ORR(arm64.X4, arm64.X1, arm64.X3)
	c.a.CBNZ(arm64.X4, c.cold(ipc))
	c.emitLuaShift(arm64.X5, arm64.X0, arm64.X2, w.Op() == bytecode.OP_SHL)
	c.storeReg(w.A(), arm64.X5, arm64.XZR)
	c.a.B(c.pcLabels[ipc+2])
}

// emitLuaShift computes Lua 5.5 shift (val << amt if left else val >> amt) into dst.
// dst must be distinct from val and amt. Clobbers X4.
func (c *a64Baseline) emitLuaShift(dst, val, amt arm64.Reg, left bool) {
	lNeg := c.a.NewLabel()
	lZero := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.CMPImm(amt, 0)
	c.a.BCond(arm64.LT, lNeg)
	c.a.CMPImm(amt, 64)
	c.a.BCond(arm64.HS, lZero)
	if left {
		c.a.LSL(dst, val, amt)
	} else {
		c.a.LSR(dst, val, amt)
	}
	c.a.B(lDone)
	c.a.Bind(lNeg)
	c.a.NEG(arm64.X4, amt)
	c.a.CMPImm(arm64.X4, 64)
	c.a.BCond(arm64.HS, lZero)
	if left {
		c.a.LSR(dst, val, arm64.X4)
	} else {
		c.a.LSL(dst, val, arm64.X4)
	}
	c.a.B(lDone)
	c.a.Bind(lZero)
	c.a.MOVZ(dst, 0, 0)
	c.a.Bind(lDone)
}

func (c *a64Baseline) emitUnm(ipc int, w bytecode.Instruction) {
	c.loadReg(w.B(), arm64.X0, arm64.X1)
	lFloat := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.CBNZ(arm64.X1, lFloat)
	c.a.NEG(arm64.X0, arm64.X0)
	c.storeReg(w.A(), arm64.X0, arm64.XZR)
	c.a.B(lDone)
	c.a.Bind(lFloat)
	c.a.CMPImm(arm64.X1, uint32(tagFloat))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.FMOVToF(arm64.D0, arm64.X0)
	c.a.FNEG(arm64.D0, arm64.D0)
	c.a.FMOVToG(arm64.X0, arm64.D0)
	c.storeReg(w.A(), arm64.X0, arm64.X1)
	c.a.Bind(lDone)
}

func (c *a64Baseline) emitLen(ipc int, w bytecode.Instruction) {
	c.loadReg(w.B(), arm64.X0, arm64.X1)
	lStr := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.CMPImm(arm64.X1, uint32(tagTable))
	c.a.BCond(arm64.NE, lStr)
	// Fast path for non-metatable table without hash part.
	c.emitResolveSmall(arm64.X0, arm64.X2, arm64.X3)
	c.a.LDRW(arm64.X3, arm64.X2, jitTblMetaRef)
	c.a.CBNZ(arm64.X3, c.cold(ipc))
	c.a.LDRW(arm64.X3, arm64.X2, jitTblHashRef)
	c.a.CBNZ(arm64.X3, c.cold(ipc))
	c.a.LDRW(arm64.X0, arm64.X2, jitTblArrLen)
	c.a.LDRB(arm64.X3, arm64.X2, jitTblArrKind)
	lStoreLen := c.a.NewLabel()
	c.a.CMPImm(arm64.X3, uint32(arrGeneric))
	c.a.BCond(arm64.NE, lStoreLen)
	// For arrGeneric with hashRef==0: #t == arrLen iff arrLen == 0 || !arr[arrLen-1].IsNil().
	c.a.CBZ(arm64.X0, lStoreLen)
	c.a.LDRW(arm64.X3, arm64.X2, jitTblArrRef)
	c.a.CBZ(arm64.X3, c.cold(ipc))
	c.emitResolveFast(arm64.X3, arm64.X4, arm64.X5)
	c.a.SUBImm(arm64.X5, arm64.X0, 1)
	c.a.ADDShift(arm64.X4, arm64.X4, arm64.X5, arm64.ShiftLSL, 4)
	c.a.LDR(arm64.X5, arm64.X4, jitValTag)
	c.a.CMPImm(arm64.X5, uint32(tagNil))
	c.a.BCond(arm64.EQ, c.cold(ipc))
	c.a.Bind(lStoreLen)
	c.storeReg(w.A(), arm64.X0, arm64.XZR)
	c.a.B(lDone)

	c.a.Bind(lStr)
	c.a.CMPImm(arm64.X1, uint32(tagString))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.emitResolveFast(arm64.X0, arm64.X2, arm64.X3)
	c.a.LDRW(arm64.X0, arm64.X2, jitStrLen)
	c.storeReg(w.A(), arm64.X0, arm64.XZR)
	c.a.Bind(lDone)
}

// --- Comparisons & Tests ---

func (c *a64Baseline) emitEQ(ipc int, w bytecode.Instruction) {
	c.loadReg(w.A(), arm64.X0, arm64.X1)
	c.loadReg(w.B(), arm64.X2, arm64.X3)
	lFloat := c.a.NewLabel()
	lNotBothInt := c.a.NewLabel()
	c.a.ORR(arm64.X4, arm64.X1, arm64.X3)
	c.a.CBNZ(arm64.X4, lNotBothInt)
	c.a.CMP(arm64.X0, arm64.X2)
	c.emitFusedCond(ipc, w, arm64.EQ)

	c.a.Bind(lNotBothInt)
	c.a.CMPImm(arm64.X1, uint32(tagFloat))
	c.a.BCond(arm64.EQ, lFloat)
	c.a.CMPImm(arm64.X3, uint32(tagFloat))
	c.a.BCond(arm64.EQ, c.cold(ipc))
	// Non-float: if tags differ -> definitely not equal (except string/frozenStr which both have tag 8/9 -> cold).
	lSameTag := c.a.NewLabel()
	c.a.CMP(arm64.X1, arm64.X3)
	c.a.BCond(arm64.EQ, lSameTag)
	// Different tags: if either is string/frozenStr/table/udata, defer to cold? Only frozenStr(8) vs string(9) can equal!
	c.a.SUBImm(arm64.X4, arm64.X1, uint32(tagFrozenString))
	c.a.CMPImm(arm64.X4, 1)
	c.a.BCond(arm64.LS, c.cold(ipc))
	// Definitely false:
	c.a.CMPImm(arm64.XZR, 1) // EQ is false
	c.emitFusedCond(ipc, w, arm64.EQ)

	c.a.Bind(lSameTag)
	// Same non-float tag: strings/tables/userdata with unequal bits may have __eq or long-string equality.
	lNotEqBits := c.a.NewLabel()
	c.a.CMP(arm64.X0, arm64.X2)
	c.a.BCond(arm64.NE, lNotEqBits)
	// Same tag & same bits & non-float => true!
	c.a.CMP(arm64.XZR, arm64.XZR) // EQ is true
	c.emitFusedCond(ipc, w, arm64.EQ)

	c.a.Bind(lNotEqBits)
	c.a.CMPImm(arm64.X1, uint32(tagString))
	c.a.BCond(arm64.EQ, c.cold(ipc))
	c.a.CMPImm(arm64.X1, uint32(tagTable))
	c.a.BCond(arm64.EQ, c.cold(ipc))
	c.a.CMPImm(arm64.X1, uint32(tagUserdata))
	c.a.BCond(arm64.EQ, c.cold(ipc))
	c.a.CMPImm(arm64.XZR, 1) // EQ is false
	c.emitFusedCond(ipc, w, arm64.EQ)

	c.a.Bind(lFloat)
	c.a.CMPImm(arm64.X3, uint32(tagFloat))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.FMOVToF(arm64.D0, arm64.X0)
	c.a.FMOVToF(arm64.D1, arm64.X2)
	c.a.FCMP(arm64.D0, arm64.D1)
	c.emitFusedCond(ipc, w, arm64.EQ)
}

func (c *a64Baseline) emitOrd(ipc int, w bytecode.Instruction, intCond, fpCond arm64.Cond) {
	c.loadReg(w.A(), arm64.X0, arm64.X1)
	c.loadReg(w.B(), arm64.X2, arm64.X3)
	lNotBothInt := c.a.NewLabel()
	c.a.ORR(arm64.X4, arm64.X1, arm64.X3)
	c.a.CBNZ(arm64.X4, lNotBothInt)
	c.a.CMP(arm64.X0, arm64.X2)
	c.emitFusedCond(ipc, w, intCond)

	c.a.Bind(lNotBothInt)
	c.a.CMPImm(arm64.X1, uint32(tagFloat))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.CMPImm(arm64.X3, uint32(tagFloat))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.FMOVToF(arm64.D0, arm64.X0)
	c.a.FMOVToF(arm64.D1, arm64.X2)
	c.a.FCMP(arm64.D0, arm64.D1)
	c.emitFusedCond(ipc, w, fpCond)
}

func (c *a64Baseline) emitEQK(ipc int, w bytecode.Instruction) {
	kc := c.bp.proto.Consts[w.B()]
	c.loadReg(w.A(), arm64.X0, arm64.X1)
	switch kc.Kind {
	case bytecode.ConstNil:
		c.a.CMPImm(arm64.X1, uint32(tagNil))
		c.emitFusedCond(ipc, w, arm64.EQ)
	case bytecode.ConstFalse:
		c.a.CMPImm(arm64.X1, uint32(tagFalse))
		c.emitFusedCond(ipc, w, arm64.EQ)
	case bytecode.ConstTrue:
		c.a.CMPImm(arm64.X1, uint32(tagTrue))
		c.emitFusedCond(ipc, w, arm64.EQ)
	case bytecode.ConstInt:
		lFloat := c.a.NewLabel()
		c.a.CBNZ(arm64.X1, lFloat)
		c.a.MOVImm64(arm64.X2, uint64(kc.Int))
		c.a.CMP(arm64.X0, arm64.X2)
		c.emitFusedCond(ipc, w, arm64.EQ)
		c.a.Bind(lFloat)
		c.a.CMPImm(arm64.X1, uint32(tagFloat))
		c.a.BCond(arm64.EQ, c.cold(ipc)) // exact int-vs-float equality via Go
		c.a.CMPImm(arm64.XZR, 1)         // false
		c.emitFusedCond(ipc, w, arm64.EQ)
	case bytecode.ConstFloat:
		lNotFloat := c.a.NewLabel()
		c.a.CMPImm(arm64.X1, uint32(tagFloat))
		c.a.BCond(arm64.NE, lNotFloat)
		c.a.FMOVToF(arm64.D0, arm64.X0)
		c.a.MOVImm64(arm64.X2, math.Float64bits(kc.Float))
		c.a.FMOVToF(arm64.D1, arm64.X2)
		c.a.FCMP(arm64.D0, arm64.D1)
		c.emitFusedCond(ipc, w, arm64.EQ)
		c.a.Bind(lNotFloat)
		c.a.CBZ(arm64.X1, c.cold(ipc)) // int-vs-float via Go
		c.a.CMPImm(arm64.XZR, 1)       // false
		c.emitFusedCond(ipc, w, arm64.EQ)
	default:
		// String constant: fast pointer-identity hit for interned short strings.
		c.loadConst(w.B(), arm64.X2, arm64.X3)
		lSameTag := c.a.NewLabel()
		c.a.CMP(arm64.X1, arm64.X3)
		c.a.BCond(arm64.EQ, lSameTag)
		c.a.SUBImm(arm64.X4, arm64.X1, uint32(tagFrozenString))
		c.a.CMPImm(arm64.X4, 1)
		c.a.BCond(arm64.LS, c.cold(ipc))
		c.a.CMPImm(arm64.XZR, 1)
		c.emitFusedCond(ipc, w, arm64.EQ)
		c.a.Bind(lSameTag)
		lDiffBits := c.a.NewLabel()
		c.a.CMP(arm64.X0, arm64.X2)
		c.a.BCond(arm64.NE, lDiffBits)
		c.a.CMP(arm64.XZR, arm64.XZR)
		c.emitFusedCond(ipc, w, arm64.EQ)
		c.a.Bind(lDiffBits)
		// If both are interned short strings (kind == gcKindShortStr), different ObjRef means unequal!
		// Large-span strings (>32 KiB) are never short strings and must exit before small-slab deref.
		c.a.CMPImm(arm64.X1, uint32(tagString))
		c.a.BCond(arm64.NE, c.cold(ipc))
		c.branchBitSet(arm64.X0, jitArenaLargeBit, c.cold(ipc))
		c.branchBitSet(arm64.X2, jitArenaLargeBit, c.cold(ipc))
		c.emitResolveSmall(arm64.X0, arm64.X4, arm64.X5)
		c.a.LDRB(arm64.X4, arm64.X4, jitStrKind)
		c.a.CMPImm(arm64.X4, uint32(gcKindShortStr))
		c.a.BCond(arm64.NE, c.cold(ipc))
		c.emitResolveSmall(arm64.X2, arm64.X4, arm64.X5)
		c.a.LDRB(arm64.X4, arm64.X4, jitStrKind)
		c.a.CMPImm(arm64.X4, uint32(gcKindShortStr))
		c.a.BCond(arm64.NE, c.cold(ipc))
		c.a.CMPImm(arm64.XZR, 1)
		c.emitFusedCond(ipc, w, arm64.EQ)
	}
}

func (c *a64Baseline) emitEQI(ipc int, w bytecode.Instruction) {
	im := int64(w.SB())
	c.loadReg(w.A(), arm64.X0, arm64.X1)
	lFloat := c.a.NewLabel()
	c.a.CBNZ(arm64.X1, lFloat)
	if im >= 0 {
		c.a.CMPImm(arm64.X0, uint32(im))
	} else {
		c.a.CMNImm(arm64.X0, uint32(-im))
	}
	c.emitFusedCond(ipc, w, arm64.EQ)

	c.a.Bind(lFloat)
	lNonNum := c.a.NewLabel()
	c.a.CMPImm(arm64.X1, uint32(tagFloat))
	c.a.BCond(arm64.NE, lNonNum)
	c.a.FMOVToF(arm64.D0, arm64.X0)
	c.a.MOVImm64(arm64.X2, math.Float64bits(float64(im)))
	c.a.FMOVToF(arm64.D1, arm64.X2)
	c.a.FCMP(arm64.D0, arm64.D1)
	c.emitFusedCond(ipc, w, arm64.EQ)

	c.a.Bind(lNonNum)
	c.a.CMPImm(arm64.XZR, 1) // false
	c.emitFusedCond(ipc, w, arm64.EQ)
}

func (c *a64Baseline) emitOrdI(ipc int, w bytecode.Instruction, intCond, fpCond arm64.Cond) {
	im := int64(w.SB())
	c.loadReg(w.A(), arm64.X0, arm64.X1)
	lFloat := c.a.NewLabel()
	c.a.CBNZ(arm64.X1, lFloat)
	if im >= 0 {
		c.a.CMPImm(arm64.X0, uint32(im))
	} else {
		c.a.CMNImm(arm64.X0, uint32(-im))
	}
	c.emitFusedCond(ipc, w, intCond)

	c.a.Bind(lFloat)
	c.a.CMPImm(arm64.X1, uint32(tagFloat))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.FMOVToF(arm64.D0, arm64.X0)
	c.a.MOVImm64(arm64.X2, math.Float64bits(float64(im)))
	c.a.FMOVToF(arm64.D1, arm64.X2)
	c.a.FCMP(arm64.D0, arm64.D1)
	c.emitFusedCond(ipc, w, fpCond)
}

func (c *a64Baseline) emitTest(ipc int, w bytecode.Instruction) {
	// OP_TEST: truthy = !IsFalsy(R[A]); if truthy == w.K(), jump (i.e. if IsFalsy == w.K(), skip!).
	c.loadReg(w.A(), arm64.X0, arm64.X1)
	c.a.SUBImm(arm64.X1, arm64.X1, uint32(tagNil))
	c.a.CMPImm(arm64.X1, 1) // HI means truthy!
	c.emitFusedCond(ipc, w, arm64.HI)
}

func (c *a64Baseline) emitTestSet(ipc int, w bytecode.Instruction) {
	if ipc+1 >= len(c.code) || c.code[ipc+1].Op() != bytecode.OP_JMP {
		c.a.B(c.cold(ipc))
		return
	}
	sj := c.code[ipc+1].SJ()
	jumpTarget := ipc + 2 + sj
	skipTarget := ipc + 2
	if jumpTarget < 0 || jumpTarget > len(c.code) {
		c.a.B(c.cold(ipc))
		return
	}
	c.loadReg(w.B(), arm64.X0, arm64.X1)
	c.a.SUBImm(arm64.X2, arm64.X1, uint32(tagNil))
	c.a.CMPImm(arm64.X2, 1) // LS => falsy, HI => truthy
	// Skip when IsFalsy == w.K() (i.e., if K==true skip on LS; if K==false skip on HI).
	if w.K() {
		c.a.BCond(arm64.LS, c.pcLabels[skipTarget])
	} else {
		c.a.BCond(arm64.HI, c.pcLabels[skipTarget])
	}
	c.storeReg(w.A(), arm64.X0, arm64.X1)
	c.emitJumpToPC(jumpTarget, sj < 0)
}

// --- Numeric FOR Loops (Lua 5.5: R[A]=count/lim, R[A+1]=step, R[A+2]=ctrl) ---

func (c *a64Baseline) emitForPrep(ipc int, w bytecode.Instruction) {
	a := w.A()
	skipTarget := ipc + 1 + w.Bx() + 1
	if skipTarget < 0 || skipTarget > len(c.code) {
		c.a.B(c.cold(ipc))
		return
	}
	c.loadReg(a, arm64.X0, arm64.X1)   // init
	c.loadReg(a+1, arm64.X2, arm64.X3) // lim
	c.loadReg(a+2, arm64.X4, arm64.X5) // step
	// Fast path: all three are tagInt (0).
	c.a.ORR(arm64.X6, arm64.X1, arm64.X3)
	c.a.ORR(arm64.X6, arm64.X6, arm64.X5)
	c.a.CBNZ(arm64.X6, c.cold(ipc))
	c.a.CBZ(arm64.X4, c.cold(ipc)) // step == 0 raises error

	lNegStep := c.a.NewLabel()
	lStepNot1 := c.a.NewLabel()
	lStore := c.a.NewLabel()
	c.a.CMPImm(arm64.X4, 0)
	c.a.BCond(arm64.LT, lNegStep)
	// step > 0
	c.a.CMP(arm64.X0, arm64.X2)
	c.a.BCond(arm64.GT, c.pcLabels[skipTarget])
	c.a.SUB(arm64.X6, arm64.X2, arm64.X0) // cnt = uint64(lim) - uint64(init)
	c.a.CMPImm(arm64.X4, 1)
	c.a.BCond(arm64.NE, lStepNot1)
	c.a.B(lStore)
	c.a.Bind(lStepNot1)
	c.a.UDIV(arm64.X6, arm64.X6, arm64.X4)
	c.a.B(lStore)

	c.a.Bind(lNegStep)
	// step < 0
	c.a.CMP(arm64.X0, arm64.X2)
	c.a.BCond(arm64.LT, c.pcLabels[skipTarget])
	c.a.SUB(arm64.X6, arm64.X0, arm64.X2) // cnt = uint64(init) - uint64(lim)
	c.a.ADDImm(arm64.X7, arm64.X4, 1)     // step + 1
	c.a.NEG(arm64.X7, arm64.X7)           // -(step + 1)
	c.a.ADDImm(arm64.X7, arm64.X7, 1)     // uint64(-(step+1)) + 1 (MinInt64-safe!)
	c.a.UDIV(arm64.X6, arm64.X6, arm64.X7)

	c.a.Bind(lStore)
	c.storeReg(a, arm64.X6, arm64.XZR)   // R[A] = cnt
	c.storeReg(a+1, arm64.X4, arm64.XZR) // R[A+1] = step
	c.storeReg(a+2, arm64.X0, arm64.XZR) // R[A+2] = init
}

func (c *a64Baseline) emitForLoop(ipc int, w bytecode.Instruction) {
	a := w.A()
	bodyTarget := ipc + 1 - w.Bx()
	if bodyTarget < 0 || bodyTarget >= len(c.code) {
		c.a.B(c.cold(ipc))
		return
	}
	c.loadReg(a+1, arm64.X2, arm64.X3) // step
	lFloat := c.a.NewLabel()
	lDone := c.pcLabels[ipc+1]
	c.a.CBNZ(arm64.X3, lFloat)

	// Integer FORLOOP
	c.loadReg(a, arm64.X0, arm64.X1)   // cnt
	c.loadReg(a+2, arm64.X4, arm64.X5) // idx
	c.a.ORR(arm64.X6, arm64.X1, arm64.X5)
	c.a.CBNZ(arm64.X6, lDone)
	c.a.CBZ(arm64.X0, lDone)
	c.a.SUBImm(arm64.X0, arm64.X0, 1)
	c.a.ADD(arm64.X4, arm64.X4, arm64.X2)
	c.storeReg(a, arm64.X0, arm64.XZR)
	c.storeReg(a+2, arm64.X4, arm64.XZR)
	c.a.SUBSImm(a64Poll, a64Poll, 1)
	c.a.BCond(arm64.LE, c.poll(bodyTarget))
	if ref, ok := c.optByEnd[ipc]; ok {
		c.a.B(ref.resumeLbl)
	} else {
		c.a.B(c.pcLabels[bodyTarget])
	}

	// Float FORLOOP
	c.a.Bind(lFloat)
	c.a.CMPImm(arm64.X3, uint32(tagFloat))
	c.a.BCond(arm64.NE, lDone)
	c.loadReg(a, arm64.X0, arm64.X1)
	c.loadReg(a+2, arm64.X4, arm64.X5)
	c.a.CMPImm(arm64.X1, uint32(tagFloat))
	c.a.BCond(arm64.NE, lDone)
	c.a.CMPImm(arm64.X5, uint32(tagFloat))
	c.a.BCond(arm64.NE, lDone)
	c.a.FMOVToF(arm64.D0, arm64.X0) // lim
	c.a.FMOVToF(arm64.D1, arm64.X2) // step
	c.a.FMOVToF(arm64.D2, arm64.X4) // idx
	c.a.FADD(arm64.D2, arm64.D2, arm64.D1)
	lNeg := c.a.NewLabel()
	lCont := c.a.NewLabel()
	c.a.FCMPZero(arm64.D1)
	c.a.BCond(arm64.LE, lNeg)
	// step > 0: continue if idx <= lim
	c.a.FCMP(arm64.D2, arm64.D0)
	c.a.BCond(arm64.LS, lCont)
	c.a.B(lDone)
	c.a.Bind(lNeg)
	// step <= 0: continue if idx >= lim
	c.a.FCMP(arm64.D2, arm64.D0)
	c.a.BCond(arm64.GE, lCont)
	c.a.B(lDone)
	c.a.Bind(lCont)
	c.a.FMOVToG(arm64.X4, arm64.D2)
	c.storeReg(a+2, arm64.X4, arm64.X5)
	c.a.SUBSImm(a64Poll, a64Poll, 1)
	c.a.BCond(arm64.LE, c.poll(bodyTarget))
	c.a.B(c.pcLabels[bodyTarget])
}

// --- Table Array Indexing (GETI, GETTABLE, SETI, SETTABLE) ---

// emitTableGetIdx fetches t[idx] (0-based uint64 in idxReg) from Table* in tblReg into R[a].
func (c *a64Baseline) emitTableGetIdx(ipc, a int, tblReg, idxReg arm64.Reg) {
	c.a.LDRW(arm64.X4, tblReg, jitTblArrLen)
	c.a.CMP(idxReg, arm64.X4)
	c.a.BCond(arm64.HS, c.cold(ipc))
	c.a.LDRW(arm64.X4, tblReg, jitTblArrRef)
	c.emitResolveFast(arm64.X4, arm64.X5, arm64.X6) // X5 = arr base ptr
	c.a.LDRB(arm64.X6, tblReg, jitTblArrKind)

	lFloat := c.a.NewLabel()
	lBool := c.a.NewLabel()
	lGen := c.a.NewLabel()
	lDone := c.a.NewLabel()

	c.a.CMPImm(arm64.X6, uint32(arrInt))
	c.a.BCond(arm64.NE, lFloat)
	c.a.LDRReg(arm64.X0, arm64.X5, idxReg, true)
	c.storeReg(a, arm64.X0, arm64.XZR)
	c.a.B(lDone)

	c.a.Bind(lFloat)
	c.a.CMPImm(arm64.X6, uint32(arrFloat))
	c.a.BCond(arm64.NE, lBool)
	c.a.LDRReg(arm64.X0, arm64.X5, idxReg, true)
	c.a.MOVZ(arm64.X1, uint16(tagFloat), 0)
	c.storeReg(a, arm64.X0, arm64.X1)
	c.a.B(lDone)

	c.a.Bind(lBool)
	c.a.CMPImm(arm64.X6, uint32(arrBool))
	c.a.BCond(arm64.NE, lGen)
	c.a.LDRBReg(arm64.X1, arm64.X5, idxReg)
	c.a.ADDImm(arm64.X1, arm64.X1, uint32(tagFalse)) // 0->tagFalse(3), 1->tagTrue(4)
	c.storeReg(a, arm64.XZR, arm64.X1)
	c.a.B(lDone)

	c.a.Bind(lGen)
	c.a.CMPImm(arm64.X6, uint32(arrGeneric))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.ADDShift(arm64.X5, arm64.X5, idxReg, arm64.ShiftLSL, 4)
	c.a.LDP(arm64.X0, arm64.X1, arm64.X5, 0)
	lStoreGen := c.a.NewLabel()
	c.a.CMPImm(arm64.X1, uint32(tagNil))
	c.a.BCond(arm64.NE, lStoreGen)
	c.a.LDRW(arm64.X7, tblReg, jitTblMetaRef)
	c.a.CBNZ(arm64.X7, c.cold(ipc))
	c.a.Bind(lStoreGen)
	c.storeReg(a, arm64.X0, arm64.X1)
	c.a.Bind(lDone)
}

func (c *a64Baseline) emitGetI(ipc int, w bytecode.Instruction) {
	key := w.C()
	if key == 0 {
		c.a.B(c.cold(ipc))
		return
	}
	c.loadReg(w.B(), arm64.X0, arm64.X1)
	c.a.CMPImm(arm64.X1, uint32(tagTable))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.emitResolveSmall(arm64.X0, arm64.X2, arm64.X3)
	c.a.MOVZ(arm64.X3, uint16(key-1), 0)
	c.emitTableGetIdx(ipc, w.A(), arm64.X2, arm64.X3)
}

func (c *a64Baseline) emitGetTable(ipc int, w bytecode.Instruction) {
	c.loadReg(w.B(), arm64.X0, arm64.X1)
	c.a.CMPImm(arm64.X1, uint32(tagTable))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.loadReg(w.C(), arm64.X3, arm64.X4)
	c.a.CBNZ(arm64.X4, c.cold(ipc)) // key must be tagInt
	c.emitResolveSmall(arm64.X0, arm64.X2, arm64.X4)
	c.a.SUBImm(arm64.X3, arm64.X3, 1) // idx = uint64(key - 1)
	c.emitTableGetIdx(ipc, w.A(), arm64.X2, arm64.X3)
}

// emitTableSetIdx stores (valBits=X0, valTag=X1) into t[idx] (tblReg=X2, idxReg=X3).
func (c *a64Baseline) emitTableSetIdx(ipc int, allowAppend bool) {
	const (
		valBits = arm64.X0
		valTag  = arm64.X1
		tblReg  = arm64.X2
		idxReg  = arm64.X3
	)
	// Reject prototype tables (isProto bit 0).
	c.a.LDRB(arm64.X4, tblReg, jitTblFlags)
	c.branchBitSet(arm64.X4, 0, c.cold(ipc))

	c.a.LDRW(arm64.X4, tblReg, jitTblArrLen) // n = arrLen
	lInBounds := c.a.NewLabel()
	c.a.CMP(idxReg, arm64.X4)
	c.a.BCond(arm64.LO, lInBounds)
	if !allowAppend {
		c.a.B(c.cold(ipc))
	} else {
		// Append check: idx == n && n < arrCap && hashRef == 0 && metaRef == 0
		c.a.BCond(arm64.NE, c.cold(ipc))
		c.a.LDRW(arm64.X5, tblReg, jitTblArrCap)
		c.a.CMP(arm64.X4, arm64.X5)
		c.a.BCond(arm64.HS, c.cold(ipc))
		c.a.LDRW(arm64.X5, tblReg, jitTblHashRef)
		c.a.CBNZ(arm64.X5, c.cold(ipc))
		c.a.LDRW(arm64.X5, tblReg, jitTblMetaRef)
		c.a.CBNZ(arm64.X5, c.cold(ipc))
		// Validate val type matches arrKind BEFORE bumping arrLen!
		c.a.LDRB(arm64.X6, tblReg, jitTblArrKind)
		lAppFloat := c.a.NewLabel()
		lAppBool := c.a.NewLabel()
		lAppGen := c.a.NewLabel()
		lAppOK := c.a.NewLabel()

		c.a.CMPImm(arm64.X6, uint32(arrInt))
		c.a.BCond(arm64.NE, lAppFloat)
		c.a.CBNZ(valTag, c.cold(ipc))
		c.a.B(lAppOK)

		c.a.Bind(lAppFloat)
		c.a.CMPImm(arm64.X6, uint32(arrFloat))
		c.a.BCond(arm64.NE, lAppBool)
		c.a.CMPImm(valTag, uint32(tagFloat))
		c.a.BCond(arm64.NE, c.cold(ipc))
		c.a.B(lAppOK)

		c.a.Bind(lAppBool)
		c.a.CMPImm(arm64.X6, uint32(arrBool))
		c.a.BCond(arm64.NE, lAppGen)
		c.a.SUBImm(arm64.X7, valTag, uint32(tagFalse))
		c.a.CMPImm(arm64.X7, 1)
		c.a.BCond(arm64.HI, c.cold(ipc))
		c.a.B(lAppOK)

		c.a.Bind(lAppGen)
		c.a.CMPImm(arm64.X6, uint32(arrGeneric))
		c.a.BCond(arm64.NE, c.cold(ipc))
		c.a.CMPImm(valTag, uint32(tagNil))
		c.a.BCond(arm64.EQ, c.cold(ipc))
		// Check GC barrier before mutating arrLen on generic append!
		lAppGenBarrierOK := c.a.NewLabel()
		c.a.SUBImm(arm64.X7, valTag, uint32(tagFirstGC))
		c.a.CMPImm(arm64.X7, uint32(tagLastGC-tagFirstGC))
		c.a.BCond(arm64.HI, lAppGenBarrierOK)
		c.a.LDRB(arm64.X7, tblReg, jitTblMark)
		c.branchBitSet(arm64.X7, jitMarkBlackBit, c.cold(ipc))
		c.a.Bind(lAppGenBarrierOK)

		c.a.Bind(lAppOK)
		c.a.ADDWImm(arm64.X4, arm64.X4, 1)
		c.a.STRW(arm64.X4, tblReg, jitTblArrLen)
	}

	c.a.Bind(lInBounds)
	c.a.LDRW(arm64.X4, tblReg, jitTblArrRef)
	c.emitResolveFast(arm64.X4, arm64.X5, arm64.X6) // X5 = arr base ptr
	c.a.LDRB(arm64.X6, tblReg, jitTblArrKind)

	lFloat := c.a.NewLabel()
	lBool := c.a.NewLabel()
	lGen := c.a.NewLabel()
	lDone := c.a.NewLabel()

	c.a.CMPImm(arm64.X6, uint32(arrInt))
	c.a.BCond(arm64.NE, lFloat)
	c.a.CBNZ(valTag, c.cold(ipc))
	c.a.STRReg(valBits, arm64.X5, idxReg, true)
	c.a.B(lDone)

	c.a.Bind(lFloat)
	c.a.CMPImm(arm64.X6, uint32(arrFloat))
	c.a.BCond(arm64.NE, lBool)
	c.a.CMPImm(valTag, uint32(tagFloat))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.STRReg(valBits, arm64.X5, idxReg, true)
	c.a.B(lDone)

	c.a.Bind(lBool)
	c.a.CMPImm(arm64.X6, uint32(arrBool))
	c.a.BCond(arm64.NE, lGen)
	c.a.SUBImm(arm64.X7, valTag, uint32(tagFalse))
	c.a.CMPImm(arm64.X7, 1)
	c.a.BCond(arm64.HI, c.cold(ipc))
	c.a.STRBReg(arm64.X7, arm64.X5, idxReg)
	c.a.B(lDone)

	c.a.Bind(lGen)
	c.a.CMPImm(arm64.X6, uint32(arrGeneric))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.ADDShift(arm64.X5, arm64.X5, idxReg, arm64.ShiftLSL, 4)
	c.a.LDR(arm64.X7, arm64.X5, jitValTag) // existing tag
	lMetaOK := c.a.NewLabel()
	c.a.CMPImm(arm64.X7, uint32(tagNil))
	c.a.BCond(arm64.NE, lMetaOK)
	c.a.LDRW(arm64.X7, tblReg, jitTblMetaRef)
	c.a.CBNZ(arm64.X7, c.cold(ipc))
	c.a.Bind(lMetaOK)
	// GC write barrier check
	lBarrierOK := c.a.NewLabel()
	c.a.SUBImm(arm64.X7, valTag, uint32(tagFirstGC))
	c.a.CMPImm(arm64.X7, uint32(tagLastGC-tagFirstGC))
	c.a.BCond(arm64.HI, lBarrierOK)
	c.a.LDRB(arm64.X7, tblReg, jitTblMark)
	c.branchBitSet(arm64.X7, jitMarkBlackBit, c.cold(ipc))
	c.a.Bind(lBarrierOK)
	c.a.STP(valBits, valTag, arm64.X5, 0)
	c.a.Bind(lDone)
}

func (c *a64Baseline) emitSetI(ipc int, w bytecode.Instruction) {
	key := w.B()
	if key == 0 {
		c.a.B(c.cold(ipc))
		return
	}
	c.loadReg(w.A(), arm64.X8, arm64.X9)
	c.a.CMPImm(arm64.X9, uint32(tagTable))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.emitResolveSmall(arm64.X8, arm64.X2, arm64.X4) // X2 = *Table
	c.loadRegOrConst(w.K(), w.C(), arm64.X0, arm64.X1)
	c.a.MOVZ(arm64.X3, uint16(key-1), 0)
	c.emitTableSetIdx(ipc, false)
}

func (c *a64Baseline) emitSetTable(ipc int, w bytecode.Instruction) {
	c.loadReg(w.A(), arm64.X8, arm64.X9)
	c.a.CMPImm(arm64.X9, uint32(tagTable))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.loadReg(w.B(), arm64.X3, arm64.X4)
	c.a.CBNZ(arm64.X4, c.cold(ipc)) // key must be int
	c.emitResolveSmall(arm64.X8, arm64.X2, arm64.X4)
	c.loadRegOrConst(w.K(), w.C(), arm64.X0, arm64.X1)
	c.a.SUBImm(arm64.X3, arm64.X3, 1)
	c.emitTableSetIdx(ipc, true)
}

// --- Inline Cache Field & Global Access ---

// emitTableSlotAddr computes &t.slot(slotReg) into dstReg (clobbering tmpReg).
// Matches Table.slotPtr: inlineSlots when slotsRef == 0, else PtrSmall(slotsRef) + slot*16.
func (c *a64Baseline) emitTableSlotAddr(tblReg, slotReg, dstReg, tmpReg arm64.Reg) {
	lExt := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.LDRW(dstReg, tblReg, jitTblSlotsRef)
	c.a.CBNZ(dstReg, lExt)
	c.a.ADDImm(dstReg, tblReg, uint32(jitTblInlineSlots))
	c.a.ADDShift(dstReg, dstReg, slotReg, arm64.ShiftLSL, 4)
	c.a.B(lDone)
	c.a.Bind(lExt)
	c.emitResolveSmall(dstReg, dstReg, tmpReg)
	c.a.ADDShift(dstReg, dstReg, slotReg, arm64.ShiftLSL, 4)
	c.a.Bind(lDone)
}

func (c *a64Baseline) loadICPtr(ipc int, dst arm64.Reg) bool {
	if ipc >= len(c.bp.icSlots) || c.bp.icSlots[ipc] < 0 {
		return false
	}
	off := uint32(c.bp.icSlots[ipc]) * uint32(jitICSize)
	if off < (1 << 12) {
		c.a.ADDImm(dst, a64IC, off)
	} else {
		c.a.MOVImm32(dst, off)
		c.a.ADD(dst, a64IC, dst)
	}
	return true
}

func (c *a64Baseline) emitGetField(ipc int, w bytecode.Instruction, isSelf bool) {
	a := w.A()
	c.loadReg(w.B(), arm64.X0, arm64.X1)
	if isSelf {
		c.storeReg(a+1, arm64.X0, arm64.X1)
	}
	if !c.loadICPtr(ipc, arm64.X4) {
		c.a.B(c.cold(ipc))
		return
	}
	c.a.CMPImm(arm64.X1, uint32(tagTable))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.emitResolveSmall(arm64.X0, arm64.X2, arm64.X3) // X2 = *Table
	c.a.LDRW(arm64.X3, arm64.X2, jitTblShapeRef)     // X3 = t.shapeRef
	c.a.LDRW(arm64.X5, arm64.X4, jitICShapeRef)      // X5 = ic.shapeRef

	lPoly := c.a.NewLabel()
	lProto := c.a.NewLabel()
	lStore := c.a.NewLabel()

	c.a.CMPW(arm64.X3, arm64.X5)
	c.a.BCond(arm64.NE, lPoly)
	c.a.LDRB(arm64.X5, arm64.X4, jitICKind)
	c.a.CMPImm(arm64.X5, uint32(icOwnSlot))
	c.a.BCond(arm64.NE, lProto)

	// Monomorphic icOwnSlot
	c.a.LDRW(arm64.X6, arm64.X4, jitICSlot)
	c.emitTableSlotAddr(arm64.X2, arm64.X6, arm64.X7, arm64.X8)
	c.a.LDP(arm64.X0, arm64.X1, arm64.X7, 0)
	c.a.CMPImm(arm64.X1, uint32(tagNil))
	c.a.BCond(arm64.NE, lStore)
	c.a.LDRB(arm64.X6, arm64.X4, jitICNoMeta)
	c.a.CBNZ(arm64.X6, lStore)
	c.a.LDRB(arm64.X6, arm64.X4, jitICProtoAbsent)
	c.a.CBZ(arm64.X6, c.cold(ipc))
	c.a.LDR(arm64.X6, arm64.X4, jitICEpoch)
	c.a.LDR(arm64.X7, a64Ctx, jitCtxProtoEpoch)
	c.a.CMP(arm64.X6, arm64.X7)
	c.a.BCond(arm64.EQ, lStore)
	c.a.B(c.cold(ipc))

	// Monomorphic icProtoVal
	c.a.Bind(lProto)
	c.a.CMPImm(arm64.X5, uint32(icProtoVal))
	c.a.BCond(arm64.NE, lPoly)
	lProtoHit := c.a.NewLabel()
	c.a.LDRB(arm64.X6, arm64.X4, jitICNoMeta)
	c.a.CBNZ(arm64.X6, lProtoHit)
	c.a.LDR(arm64.X6, arm64.X4, jitICEpoch)
	c.a.LDR(arm64.X7, a64Ctx, jitCtxProtoEpoch)
	c.a.CMP(arm64.X6, arm64.X7)
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.Bind(lProtoHit)
	c.a.LDP(arm64.X0, arm64.X1, arm64.X4, jitICVal)
	c.a.B(lStore)

	// Polymorphic IC loop (up to 4 entries)
	c.a.Bind(lPoly)
	c.a.LDRW(arm64.X5, arm64.X4, jitICPolyRef)
	c.a.CBZ(arm64.X5, c.cold(ipc))
	c.a.LDRB(arm64.X9, arm64.X4, jitICPolyCount)
	c.a.CBZ(arm64.X9, c.cold(ipc))
	c.emitResolveSmall(arm64.X5, arm64.X5, arm64.X6) // X5 = &poly[0]
	lPolyLoop := c.a.NewLabel()
	lPolyNext := c.a.NewLabel()
	lPolyProto := c.a.NewLabel()
	c.a.Bind(lPolyLoop)
	c.a.LDRW(arm64.X6, arm64.X5, jitPolyShapeRef)
	c.a.CMPW(arm64.X3, arm64.X6)
	c.a.BCond(arm64.NE, lPolyNext)
	c.a.LDRB(arm64.X6, arm64.X5, jitPolyKind)
	c.a.CMPImm(arm64.X6, uint32(icOwnSlot))
	c.a.BCond(arm64.NE, lPolyProto)
	c.a.LDRW(arm64.X6, arm64.X5, jitPolySlot)
	c.emitTableSlotAddr(arm64.X2, arm64.X6, arm64.X7, arm64.X8)
	c.a.LDP(arm64.X0, arm64.X1, arm64.X7, 0)
	c.a.CMPImm(arm64.X1, uint32(tagNil))
	c.a.BCond(arm64.NE, lStore)
	c.a.LDRB(arm64.X6, arm64.X5, jitPolyNoMeta)
	c.a.CBNZ(arm64.X6, lStore)
	c.a.LDRB(arm64.X6, arm64.X5, jitPolyProtoAbsent)
	c.a.CBZ(arm64.X6, c.cold(ipc))
	c.a.LDR(arm64.X6, arm64.X5, jitPolyEpoch)
	c.a.LDR(arm64.X7, a64Ctx, jitCtxProtoEpoch)
	c.a.CMP(arm64.X6, arm64.X7)
	c.a.BCond(arm64.EQ, lStore)
	c.a.B(c.cold(ipc))

	c.a.Bind(lPolyProto)
	c.a.CMPImm(arm64.X6, uint32(icProtoVal))
	c.a.BCond(arm64.NE, c.cold(ipc))
	lPolyProtoHit := c.a.NewLabel()
	c.a.LDRB(arm64.X6, arm64.X5, jitPolyNoMeta)
	c.a.CBNZ(arm64.X6, lPolyProtoHit)
	c.a.LDR(arm64.X6, arm64.X5, jitPolyEpoch)
	c.a.LDR(arm64.X7, a64Ctx, jitCtxProtoEpoch)
	c.a.CMP(arm64.X6, arm64.X7)
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.Bind(lPolyProtoHit)
	c.a.LDP(arm64.X0, arm64.X1, arm64.X5, jitPolyVal)
	c.a.B(lStore)

	c.a.Bind(lPolyNext)
	c.a.ADDImm(arm64.X5, arm64.X5, uint32(jitPolySize))
	c.a.SUBSImm(arm64.X9, arm64.X9, 1)
	c.a.BCond(arm64.NE, lPolyLoop)
	c.a.B(c.cold(ipc))

	c.a.Bind(lStore)
	c.storeReg(a, arm64.X0, arm64.X1)
}

func (c *a64Baseline) emitSetField(ipc int, w bytecode.Instruction) {
	if !c.loadICPtr(ipc, arm64.X4) {
		c.a.B(c.cold(ipc))
		return
	}
	c.loadReg(w.A(), arm64.X8, arm64.X9)
	c.a.CMPImm(arm64.X9, uint32(tagTable))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.emitResolveSmall(arm64.X8, arm64.X2, arm64.X3) // X2 = *Table
	c.a.LDRB(arm64.X3, arm64.X2, jitTblFlags)
	c.branchBitSet(arm64.X3, 0, c.cold(ipc)) // reject isProto

	c.a.LDRW(arm64.X3, arm64.X2, jitTblShapeRef)
	c.a.LDRW(arm64.X5, arm64.X4, jitICShapeRef)
	c.a.CMPW(arm64.X3, arm64.X5)
	c.a.BCond(arm64.NE, c.cold(ipc))

	c.loadRegOrConst(w.K(), w.C(), arm64.X0, arm64.X1)
	c.a.LDRB(arm64.X5, arm64.X4, jitICKind)
	lAddSlot := c.a.NewLabel()
	lDone := c.a.NewLabel()

	c.a.CMPImm(arm64.X5, uint32(icOwnSlot))
	c.a.BCond(arm64.NE, lAddSlot)
	c.a.LDRW(arm64.X6, arm64.X4, jitICSlot)
	c.emitTableSlotAddr(arm64.X2, arm64.X6, arm64.X7, arm64.X8)
	c.a.LDR(arm64.X8, arm64.X7, jitValTag) // existing tag
	lWriteOwn := c.a.NewLabel()
	c.a.CMPImm(arm64.X8, uint32(tagNil))
	c.a.BCond(arm64.NE, lWriteOwn)
	c.a.LDRB(arm64.X8, arm64.X4, jitICNoMeta)
	c.a.CBNZ(arm64.X8, lWriteOwn)
	c.a.LDRB(arm64.X8, arm64.X4, jitICProtoAbsent)
	c.a.CBZ(arm64.X8, c.cold(ipc))
	c.a.LDR(arm64.X8, arm64.X4, jitICEpoch)
	c.a.LDR(arm64.X9, a64Ctx, jitCtxProtoEpoch)
	c.a.CMP(arm64.X8, arm64.X9)
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.Bind(lWriteOwn)
	// GC write barrier check
	lOwnBarrierOK := c.a.NewLabel()
	c.a.SUBImm(arm64.X8, arm64.X1, uint32(tagFirstGC))
	c.a.CMPImm(arm64.X8, uint32(tagLastGC-tagFirstGC))
	c.a.BCond(arm64.HI, lOwnBarrierOK)
	c.a.LDRB(arm64.X8, arm64.X2, jitTblMark)
	c.branchBitSet(arm64.X8, jitMarkBlackBit, c.cold(ipc))
	c.a.Bind(lOwnBarrierOK)
	c.a.STP(arm64.X0, arm64.X1, arm64.X7, 0)
	c.a.B(lDone)

	// icAddSlot fast transition
	c.a.Bind(lAddSlot)
	c.a.CMPImm(arm64.X5, uint32(icAddSlot))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.CMPImm(arm64.X1, uint32(tagNil))
	c.a.BCond(arm64.EQ, c.cold(ipc))
	lAddMetaOK := c.a.NewLabel()
	c.a.LDRB(arm64.X8, arm64.X4, jitICNoMeta)
	c.a.CBNZ(arm64.X8, lAddMetaOK)
	c.a.LDR(arm64.X8, arm64.X4, jitICEpoch)
	c.a.LDR(arm64.X9, a64Ctx, jitCtxProtoEpoch)
	c.a.CMP(arm64.X8, arm64.X9)
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.Bind(lAddMetaOK)
	c.a.LDRW(arm64.X9, arm64.X4, jitICTargetShape)
	c.a.CBZ(arm64.X9, c.cold(ipc))
	c.a.LDRW(arm64.X6, arm64.X4, jitICSlot)
	c.a.LDRB(arm64.X8, arm64.X2, jitTblSlotsCap)
	c.a.CMP(arm64.X6, arm64.X8)
	c.a.BCond(arm64.HS, c.cold(ipc))
	// Reject black table on shape transition (needs barrierBackRef for new targetShapeRef + val)
	c.a.LDRB(arm64.X8, arm64.X2, jitTblMark)
	c.branchBitSet(arm64.X8, jitMarkBlackBit, c.cold(ipc))
	c.emitTableSlotAddr(arm64.X2, arm64.X6, arm64.X7, arm64.X8)
	c.a.ADDWImm(arm64.X8, arm64.X6, 1)
	c.a.STRB(arm64.X8, arm64.X2, jitTblSlotsLen)
	c.a.STP(arm64.X0, arm64.X1, arm64.X7, 0)
	c.a.STRW(arm64.X9, arm64.X2, jitTblShapeRef)
	c.a.Bind(lDone)
}

func (c *a64Baseline) emitGetTabUp(ipc int, w bytecode.Instruction) {
	if !c.loadICPtr(ipc, arm64.X4) {
		c.a.B(c.cold(ipc))
		return
	}
	c.emitLoadUpval(ipc, w.B(), arm64.X0, arm64.X1, arm64.X2, arm64.X3)
	c.a.CMPImm(arm64.X1, uint32(tagTable))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.emitResolveSmall(arm64.X0, arm64.X2, arm64.X3) // X2 = *Table

	lOwn := c.a.NewLabel()
	lStore := c.a.NewLabel()
	c.a.LDRB(arm64.X5, arm64.X4, jitICKind)
	c.a.CMPImm(arm64.X5, uint32(icEnvSlot))
	c.a.BCond(arm64.NE, lOwn)

	// icEnvSlot: check holderRef == t.self, hashRef != 0, dictGen == ic.epoch
	c.a.LDRW(arm64.X6, arm64.X4, jitICHolderRef)
	c.a.LDRW(arm64.X7, arm64.X2, jitTblSelf)
	c.a.CMPW(arm64.X6, arm64.X7)
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.LDRW(arm64.X6, arm64.X2, jitTblHashRef)
	c.a.CBZ(arm64.X6, c.cold(ipc))
	c.emitResolveSmall(arm64.X6, arm64.X6, arm64.X7) // *tableHash
	c.a.LDR(arm64.X7, arm64.X6, jitThashDictGen)
	c.a.LDR(arm64.X8, arm64.X4, jitICEpoch)
	c.a.CMP(arm64.X7, arm64.X8)
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.LDRW(arm64.X6, arm64.X6, jitThashNodesRef)
	c.a.CBZ(arm64.X6, c.cold(ipc))
	c.emitResolveFast(arm64.X6, arm64.X6, arm64.X7) // *hashNode[0]
	c.a.LDRW(arm64.X7, arm64.X4, jitICSlot)
	c.a.ADDShift(arm64.X8, arm64.X7, arm64.X7, arm64.ShiftLSL, 2) // slot * 5
	c.a.ADDShift(arm64.X6, arm64.X6, arm64.X8, arm64.ShiftLSL, 3) // + slot * 40
	c.a.LDP(arm64.X0, arm64.X1, arm64.X6, jitHNodeVal)
	c.a.CMPImm(arm64.X1, uint32(tagNil))
	c.a.BCond(arm64.NE, lStore)
	c.a.LDRB(arm64.X7, arm64.X4, jitICNoMeta)
	c.a.CBNZ(arm64.X7, lStore)
	c.a.B(c.cold(ipc))

	// icOwnSlot fallback
	c.a.Bind(lOwn)
	c.a.CMPImm(arm64.X5, uint32(icOwnSlot))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.LDRW(arm64.X6, arm64.X2, jitTblShapeRef)
	c.a.LDRW(arm64.X7, arm64.X4, jitICShapeRef)
	c.a.CMPW(arm64.X6, arm64.X7)
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.LDRW(arm64.X6, arm64.X4, jitICSlot)
	c.emitTableSlotAddr(arm64.X2, arm64.X6, arm64.X7, arm64.X8)
	c.a.LDP(arm64.X0, arm64.X1, arm64.X7, 0)
	c.a.CMPImm(arm64.X1, uint32(tagNil))
	c.a.BCond(arm64.NE, lStore)
	c.a.LDRB(arm64.X7, arm64.X4, jitICNoMeta)
	c.a.CBZ(arm64.X7, c.cold(ipc))

	c.a.Bind(lStore)
	c.storeReg(w.A(), arm64.X0, arm64.X1)
}

func (c *a64Baseline) emitSetTabUp(ipc int, w bytecode.Instruction) {
	if !c.loadICPtr(ipc, arm64.X4) {
		c.a.B(c.cold(ipc))
		return
	}
	c.emitLoadUpval(ipc, w.A(), arm64.X8, arm64.X9, arm64.X2, arm64.X3)
	c.a.CMPImm(arm64.X9, uint32(tagTable))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.emitResolveSmall(arm64.X8, arm64.X2, arm64.X3) // X2 = *Table
	c.a.LDRB(arm64.X3, arm64.X2, jitTblFlags)
	c.branchBitSet(arm64.X3, 0, c.cold(ipc))

	c.loadRegOrConst(w.K(), w.C(), arm64.X0, arm64.X1)
	c.a.CMPImm(arm64.X1, uint32(tagNil))
	c.a.BCond(arm64.EQ, c.cold(ipc))

	c.a.LDRB(arm64.X5, arm64.X4, jitICKind)
	c.a.CMPImm(arm64.X5, uint32(icEnvSlot))
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.LDRW(arm64.X6, arm64.X4, jitICHolderRef)
	c.a.LDRW(arm64.X7, arm64.X2, jitTblSelf)
	c.a.CMPW(arm64.X6, arm64.X7)
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.LDRW(arm64.X6, arm64.X2, jitTblHashRef)
	c.a.CBZ(arm64.X6, c.cold(ipc))
	c.emitResolveSmall(arm64.X6, arm64.X6, arm64.X7)
	c.a.LDR(arm64.X7, arm64.X6, jitThashDictGen)
	c.a.LDR(arm64.X8, arm64.X4, jitICEpoch)
	c.a.CMP(arm64.X7, arm64.X8)
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.a.LDRW(arm64.X6, arm64.X6, jitThashNodesRef)
	c.a.CBZ(arm64.X6, c.cold(ipc))
	c.emitResolveFast(arm64.X6, arm64.X6, arm64.X7)
	c.a.LDRW(arm64.X7, arm64.X4, jitICSlot)
	c.a.ADDShift(arm64.X8, arm64.X7, arm64.X7, arm64.ShiftLSL, 2)
	c.a.ADDShift(arm64.X6, arm64.X6, arm64.X8, arm64.ShiftLSL, 3)
	c.a.LDR(arm64.X8, arm64.X6, jitHNodeVal+jitValTag)
	c.a.CMPImm(arm64.X8, uint32(tagNil))
	c.a.BCond(arm64.EQ, c.cold(ipc))
	// GC write barrier check
	lBarrierOK := c.a.NewLabel()
	c.a.SUBImm(arm64.X8, arm64.X1, uint32(tagFirstGC))
	c.a.CMPImm(arm64.X8, uint32(tagLastGC-tagFirstGC))
	c.a.BCond(arm64.HI, lBarrierOK)
	c.a.LDRB(arm64.X8, arm64.X2, jitTblMark)
	c.branchBitSet(arm64.X8, jitMarkBlackBit, c.cold(ipc))
	c.a.Bind(lBarrierOK)
	c.a.STP(arm64.X0, arm64.X1, arm64.X6, jitHNodeVal)
	c.a.STRB(arm64.XZR, arm64.X2, jitTblTMFlags)
}

// --- Calls, TailCalls & Returns (Flat Tail-Jump Native-to-Native Dispatch!) ---

func (c *a64Baseline) emitCall(ipc int, w bytecode.Instruction) {
	a := w.A()
	b := w.B()
	cArg := w.C()
	if b == 0 {
		c.a.B(c.cold(ipc))
		return
	}
	nargs := b - 1
	c.loadReg(a, arm64.X5, arm64.X6) // fn = (X5, X6)

	lCheckStatic := c.a.NewLabel()
	c.a.CMPImm(arm64.X6, uint32(tagClosure))
	c.a.BCond(arm64.NE, lCheckStatic)

	// Fast native-to-native Lua call
	c.emitResolveSmall(arm64.X5, arm64.X7, arm64.X8) // X7 = *Closure
	c.a.LDRW(arm64.X8, arm64.X7, jitClProtoIdx)      // X8 = calleeProtoIdx
	c.a.LDR(arm64.X9, a64Ctx, jitCtxJITProtoCap)
	c.a.CMP(arm64.X8, arm64.X9)
	c.a.BCond(arm64.HS, c.cold(ipc))
	c.a.LDR(arm64.X9, a64Ctx, jitCtxJITEntries)
	c.a.LDRReg(arm64.X16, arm64.X9, arm64.X8, true) // X16 = callee native entryPC
	c.a.CMPImm(arm64.X16, 4096)
	c.a.BCond(arm64.LO, c.cold(ipc))

	c.a.LDR(arm64.X9, a64Ctx, jitCtxProtos)
	c.a.LDRReg(arm64.X10, arm64.X9, arm64.X8, true) // X10 = callee *boundProto
	c.a.LDRB(arm64.X11, arm64.X10, jitBPIsVararg)
	c.a.CBNZ(arm64.X11, c.cold(ipc))

	c.a.LDR(arm64.X12, a64Ctx, jitCtxTh)
	c.a.LDR(arm64.X13, arm64.X12, jitThNFrame)
	c.a.LDR(arm64.X14, arm64.X12, jitThFramesCap)
	c.a.CMP(arm64.X13, arm64.X14)
	c.a.BCond(arm64.GE, c.cold(ipc))

	c.a.LDR(arm64.X14, a64Ctx, jitCtxBase)
	if a > 0 {
		c.a.ADDImm(arm64.X14, arm64.X14, uint32(a)) // funcIdx = base + a
	}
	c.a.ADDImm(arm64.X15, arm64.X14, 1) // newBase = funcIdx + 1
	c.a.LDRB(arm64.X8, arm64.X10, jitBPMaxStack)
	c.a.ADD(arm64.X9, arm64.X15, arm64.X8)      // frameEnd = newBase + maxStack
	c.a.ADDImm(arm64.X11, arm64.X9, stackSlack) // need = frameEnd + stackSlack
	c.a.LDR(arm64.X2, arm64.X12, jitThStackLen)
	c.a.CMP(arm64.X11, arm64.X2)
	c.a.BCond(arm64.GT, c.cold(ipc))

	// Poll check before mutating frame state so re-entry cleanly retries OP_CALL.
	c.a.SUBSImm(a64Poll, a64Poll, 1)
	c.a.BCond(arm64.LE, c.poll(ipc))

	// Compute nf = &frames[newCI]
	c.a.MOVZ(arm64.X2, uint16(jitCFSize), 0)
	c.a.MUL(arm64.X4, arm64.X13, arm64.X2)
	c.a.LDR(arm64.X6, a64Ctx, jitCtxFrames)
	c.a.ADD(arm64.X4, arm64.X6, arm64.X4) // X4 = nf
	c.a.LDRB(arm64.X11, arm64.X4, jitCFDirty)
	lClean := c.a.NewLabel()
	c.a.CBZ(arm64.X11, lClean)
	for off := int32(0); off < 192; off += 16 {
		c.a.STP(arm64.XZR, arm64.XZR, arm64.X4, off)
	}
	c.a.STR(arm64.XZR, arm64.X4, 192)
	c.a.Bind(lClean)

	// Fill missing params with nil if nargs < numParams
	c.a.LDRB(arm64.X11, arm64.X10, jitBPNumParams)
	c.a.MOVZ(arm64.X3, uint16(nargs), 0)
	lParamsDone := c.a.NewLabel()
	c.a.CMP(arm64.X3, arm64.X11)
	c.a.BCond(arm64.GE, lParamsDone)
	c.a.ADD(arm64.X2, arm64.X15, arm64.X3)
	c.a.ADDShift(arm64.X2, a64Stack, arm64.X2, arm64.ShiftLSL, 4)
	c.a.SUB(arm64.X3, arm64.X11, arm64.X3)
	c.a.MOVZ(arm64.X11, uint16(tagNil), 0)
	lNilLoop := c.a.NewLabel()
	c.a.Bind(lNilLoop)
	c.a.STP(arm64.XZR, arm64.X11, arm64.X2, 0)
	c.a.ADDImm(arm64.X2, arm64.X2, 16)
	c.a.SUBSImm(arm64.X3, arm64.X3, 1)
	c.a.BCond(arm64.NE, lNilLoop)
	c.a.Bind(lParamsDone)

	// Save caller PC (ipc+1) into frames[curCI].pc
	c.a.LDR(arm64.X3, a64Ctx, jitCtxCI)
	c.a.MOVZ(arm64.X11, uint16(jitCFSize), 0)
	c.a.MUL(arm64.X3, arm64.X3, arm64.X11)
	c.a.ADD(arm64.X3, arm64.X6, arm64.X3)
	c.a.MOVImm64(arm64.X2, uint64(ipc+1))
	c.a.STR(arm64.X2, arm64.X3, jitCFPC)

	// Populate nf
	c.a.STRW(arm64.X5, arm64.X4, jitCFClRef)
	c.a.STR(arm64.X15, arm64.X4, jitCFBase)
	c.a.STR(arm64.X14, arm64.X4, jitCFFuncIdx)
	c.a.STR(arm64.XZR, arm64.X4, jitCFPC)
	c.a.MOVImm64(arm64.X2, uint64(int64(cArg-1)))
	c.a.STR(arm64.X2, arm64.X4, jitCFNResults)
	c.a.MOVImm64(arm64.X2, ^uint64(0))
	c.a.STR(arm64.X2, arm64.X4, jitCFOldPC)

	// Update Thread nframe & high watermarks
	c.a.ADDImm(arm64.X2, arm64.X13, 1)
	c.a.STR(arm64.X2, arm64.X12, jitThNFrame)
	c.a.LDR(arm64.X3, arm64.X12, jitThHighFrames)
	lHFOK := c.a.NewLabel()
	c.a.CMP(arm64.X2, arm64.X3)
	c.a.BCond(arm64.LE, lHFOK)
	c.a.STR(arm64.X2, arm64.X12, jitThHighFrames)
	c.a.Bind(lHFOK)
	c.a.LDR(arm64.X3, arm64.X12, jitThHighStack)
	lHSOK := c.a.NewLabel()
	c.a.CMP(arm64.X9, arm64.X3)
	c.a.BCond(arm64.LE, lHSOK)
	c.a.STR(arm64.X9, arm64.X12, jitThHighStack)
	c.a.Bind(lHSOK)

	// Switch ctx & pinned regs to callee and tail-jump to X16!
	c.a.STR(arm64.X13, a64Ctx, jitCtxCI)
	c.a.STR(arm64.X15, a64Ctx, jitCtxBase)
	c.a.ADDShift(a64Regs, a64Stack, arm64.X15, arm64.ShiftLSL, 4)

	c.a.LDRW(arm64.X2, arm64.X7, jitClUpvalsRef)
	lNoUv := c.a.NewLabel()
	lCheckBP := c.a.NewLabel()
	c.a.CBZ(arm64.X2, lNoUv)
	c.emitResolveSmall(arm64.X2, a64ClUpvals, arm64.X8)
	c.a.B(lCheckBP)
	c.a.Bind(lNoUv)
	c.a.MOVZ(a64ClUpvals, 0, 0)

	c.a.Bind(lCheckBP)
	c.a.LDR(arm64.X2, a64Ctx, jitCtxCurBP)
	lSameBP := c.a.NewLabel()
	c.a.CMP(arm64.X2, arm64.X10)
	c.a.BCond(arm64.EQ, lSameBP)
	c.a.STR(arm64.X10, a64Ctx, jitCtxCurBP)
	c.a.LDR(a64Consts, arm64.X10, jitBPConsts)
	c.a.LDR(arm64.X3, arm64.X10, jitBPICSlots)
	c.a.LDR(a64IC, arm64.X10, jitBPIC)
	c.a.STR(arm64.X3, a64Ctx, jitCtxICSlots)
	c.a.Bind(lSameBP)
	c.a.BR(arm64.X16)

	// Inline hardware intrinsics for math.sqrt / math.abs when C == 2 && B >= 2
	c.a.Bind(lCheckStatic)
	if cArg != 2 || b < 2 {
		c.a.B(c.cold(ipc))
		return
	}
	c.a.CMPImm(arm64.X6, uint32(tagStaticNative))
	c.a.BCond(arm64.NE, c.cold(ipc))
	lCheckAbs := c.a.NewLabel()
	lDone := c.pcLabels[ipc+1]
	c.a.LDRW(arm64.X8, a64Ctx, jitCtxSqrtIdx)
	c.a.CBZ(arm64.X8, lCheckAbs)
	c.a.CMP(arm64.X5, arm64.X8)
	c.a.BCond(arm64.NE, lCheckAbs)
	c.loadReg(a+1, arm64.X9, arm64.X10)
	lSqrtF := c.a.NewLabel()
	c.a.CMPImm(arm64.X10, uint32(tagFloat))
	c.a.BCond(arm64.EQ, lSqrtF)
	c.a.CBNZ(arm64.X10, c.cold(ipc))
	c.a.SCVTF(arm64.D0, arm64.X9)
	c.a.FSQRT(arm64.D0, arm64.D0)
	c.a.FMOVToG(arm64.X9, arm64.D0)
	c.a.MOVZ(arm64.X10, uint16(tagFloat), 0)
	c.storeReg(a, arm64.X9, arm64.X10)
	c.a.B(lDone)
	c.a.Bind(lSqrtF)
	c.a.FMOVToF(arm64.D0, arm64.X9)
	c.a.FSQRT(arm64.D0, arm64.D0)
	c.a.FMOVToG(arm64.X9, arm64.D0)
	c.storeReg(a, arm64.X9, arm64.X10)
	c.a.B(lDone)

	c.a.Bind(lCheckAbs)
	c.a.LDRW(arm64.X8, a64Ctx, jitCtxAbsIdx)
	c.a.CBZ(arm64.X8, c.cold(ipc))
	c.a.CMP(arm64.X5, arm64.X8)
	c.a.BCond(arm64.NE, c.cold(ipc))
	c.loadReg(a+1, arm64.X9, arm64.X10)
	lAbsF := c.a.NewLabel()
	c.a.CMPImm(arm64.X10, uint32(tagFloat))
	c.a.BCond(arm64.EQ, lAbsF)
	c.a.CBNZ(arm64.X10, c.cold(ipc))
	c.a.CMPImm(arm64.X9, 0)
	c.a.CNEG(arm64.X9, arm64.X9, arm64.LT)
	c.storeReg(a, arm64.X9, arm64.XZR)
	c.a.B(lDone)
	c.a.Bind(lAbsF)
	c.a.FMOVToF(arm64.D0, arm64.X9)
	c.a.FABS(arm64.D0, arm64.D0)
	c.a.FMOVToG(arm64.X9, arm64.D0)
	c.storeReg(a, arm64.X9, arm64.X10)
}

func (c *a64Baseline) emitTailCall(ipc int, w bytecode.Instruction) {
	if w.K() || w.B() == 0 {
		c.a.B(c.cold(ipc))
		return
	}
	a := w.A()
	nargs := w.B() - 1
	c.loadReg(a, arm64.X5, arm64.X6)
	c.a.CMPImm(arm64.X6, uint32(tagClosure))
	c.a.BCond(arm64.NE, c.cold(ipc))

	c.emitResolveSmall(arm64.X5, arm64.X7, arm64.X8)
	c.a.LDRW(arm64.X8, arm64.X7, jitClProtoIdx)
	c.a.LDR(arm64.X9, a64Ctx, jitCtxJITProtoCap)
	c.a.CMP(arm64.X8, arm64.X9)
	c.a.BCond(arm64.HS, c.cold(ipc))
	c.a.LDR(arm64.X9, a64Ctx, jitCtxJITEntries)
	c.a.LDRReg(arm64.X16, arm64.X9, arm64.X8, true)
	c.a.CMPImm(arm64.X16, 4096)
	c.a.BCond(arm64.LO, c.cold(ipc))

	c.a.LDR(arm64.X9, a64Ctx, jitCtxProtos)
	c.a.LDRReg(arm64.X10, arm64.X9, arm64.X8, true)
	c.a.LDRB(arm64.X11, arm64.X10, jitBPIsVararg)
	c.a.CBNZ(arm64.X11, c.cold(ipc))

	c.a.LDR(arm64.X12, a64Ctx, jitCtxCI)
	c.a.MOVZ(arm64.X13, uint16(jitCFSize), 0)
	c.a.MUL(arm64.X13, arm64.X12, arm64.X13)
	c.a.LDR(arm64.X14, a64Ctx, jitCtxFrames)
	c.a.ADD(arm64.X13, arm64.X14, arm64.X13) // X13 = cf
	c.a.LDR(arm64.X14, arm64.X13, jitCFFuncIdx)
	c.a.ADDImm(arm64.X15, arm64.X14, 1) // newBase = funcIdx + 1
	c.a.LDRB(arm64.X8, arm64.X10, jitBPMaxStack)
	c.a.MOVZ(arm64.X3, uint16(nargs), 0)
	c.a.CMP(arm64.X3, arm64.X8)
	c.a.CSEL(arm64.X9, arm64.X3, arm64.X8, arm64.GT)
	c.a.ADD(arm64.X9, arm64.X15, arm64.X9)
	c.a.ADDImm(arm64.X11, arm64.X9, stackSlack)
	c.a.LDR(arm64.X12, a64Ctx, jitCtxTh)
	c.a.LDR(arm64.X2, arm64.X12, jitThStackLen)
	c.a.CMP(arm64.X11, arm64.X2)
	c.a.BCond(arm64.GT, c.cold(ipc))

	// Poll check before clobbering current frame args!
	c.a.SUBSImm(a64Poll, a64Poll, 1)
	c.a.BCond(arm64.LE, c.poll(ipc))

	// Copy [regs+a*16 .. +(1+nargs)*16) -> [stack+funcIdx*16 ..)
	c.a.ADDImm(arm64.X4, a64Regs, uint32(a*16))
	c.a.ADDShift(arm64.X2, a64Stack, arm64.X14, arm64.ShiftLSL, 4)
	c.a.MOVZ(arm64.X11, uint16(nargs+1), 0)
	lCopy := c.a.NewLabel()
	c.a.Bind(lCopy)
	c.a.LDP(arm64.X6, arm64.X9, arm64.X4, 0)
	c.a.STP(arm64.X6, arm64.X9, arm64.X2, 0)
	c.a.ADDImm(arm64.X4, arm64.X4, 16)
	c.a.ADDImm(arm64.X2, arm64.X2, 16)
	c.a.SUBSImm(arm64.X11, arm64.X11, 1)
	c.a.BCond(arm64.NE, lCopy)

	// Fill [newBase+nargs .. newBase+maxStack) with nil
	lNilDone := c.a.NewLabel()
	c.a.CMP(arm64.X3, arm64.X8)
	c.a.BCond(arm64.GE, lNilDone)
	c.a.SUB(arm64.X11, arm64.X8, arm64.X3)
	c.a.MOVZ(arm64.X6, uint16(tagNil), 0)
	lNil := c.a.NewLabel()
	c.a.Bind(lNil)
	c.a.STP(arm64.XZR, arm64.X6, arm64.X2, 0)
	c.a.ADDImm(arm64.X2, arm64.X2, 16)
	c.a.SUBSImm(arm64.X11, arm64.X11, 1)
	c.a.BCond(arm64.NE, lNil)
	c.a.Bind(lNilDone)

	// Update top & highStack
	c.a.ADD(arm64.X9, arm64.X15, arm64.X8)
	c.a.STR(arm64.X9, arm64.X12, jitThTop)
	c.a.LDR(arm64.X2, arm64.X12, jitThHighStack)
	lHSOK := c.a.NewLabel()
	c.a.CMP(arm64.X9, arm64.X2)
	c.a.BCond(arm64.LE, lHSOK)
	c.a.STR(arm64.X9, arm64.X12, jitThHighStack)
	c.a.Bind(lHSOK)

	// Update cf in place
	c.a.STRW(arm64.X5, arm64.X13, jitCFClRef)
	c.a.STRW(arm64.XZR, arm64.X13, jitCFVarargTabRef)
	c.a.STR(arm64.X15, arm64.X13, jitCFBase)
	c.a.STR(arm64.XZR, arm64.X13, jitCFPC)
	c.a.STR(arm64.XZR, arm64.X13, jitCFNExtraArgs)
	c.a.MOVZ(arm64.X2, 1, 0)
	c.a.STRB(arm64.X2, arm64.X13, jitCFDirty)
	c.a.STRB(arm64.X2, arm64.X13, jitCFIsTailCall)
	c.a.STRB(arm64.XZR, arm64.X13, jitCFExtraArgs)
	c.a.MOVImm64(arm64.X2, ^uint64(0))
	c.a.STR(arm64.X2, arm64.X13, jitCFOldPC)

	// Update ctx & pinned regs and tail-jump
	c.a.STR(arm64.X15, a64Ctx, jitCtxBase)
	c.a.ADDShift(a64Regs, a64Stack, arm64.X15, arm64.ShiftLSL, 4)
	c.a.LDRW(arm64.X2, arm64.X7, jitClUpvalsRef)
	lNoUv := c.a.NewLabel()
	lCheckBP := c.a.NewLabel()
	c.a.CBZ(arm64.X2, lNoUv)
	c.emitResolveSmall(arm64.X2, a64ClUpvals, arm64.X8)
	c.a.B(lCheckBP)
	c.a.Bind(lNoUv)
	c.a.MOVZ(a64ClUpvals, 0, 0)
	c.a.Bind(lCheckBP)
	c.a.LDR(arm64.X2, a64Ctx, jitCtxCurBP)
	lSameBP := c.a.NewLabel()
	c.a.CMP(arm64.X2, arm64.X10)
	c.a.BCond(arm64.EQ, lSameBP)
	c.a.STR(arm64.X10, a64Ctx, jitCtxCurBP)
	c.a.LDR(a64Consts, arm64.X10, jitBPConsts)
	c.a.LDR(arm64.X3, arm64.X10, jitBPICSlots)
	c.a.LDR(a64IC, arm64.X10, jitBPIC)
	c.a.STR(arm64.X3, a64Ctx, jitCtxICSlots)
	c.a.Bind(lSameBP)
	c.a.BR(arm64.X16)
}

func (c *a64Baseline) emitReturn0() {
	c.a.LDR(arm64.X2, a64Ctx, jitCtxCI)
	c.a.MOVZ(arm64.X3, uint16(jitCFSize), 0)
	c.a.MUL(arm64.X4, arm64.X2, arm64.X3)
	c.a.LDR(arm64.X5, a64Ctx, jitCtxFrames)
	c.a.ADD(arm64.X4, arm64.X5, arm64.X4)
	c.a.LDR(arm64.X6, arm64.X4, jitCFNResults)
	c.a.LDR(arm64.X7, arm64.X4, jitCFFuncIdx)
	c.a.LDR(arm64.X8, a64Ctx, jitCtxTh)
	c.a.LDR(arm64.X9, arm64.X8, jitThNFrame)
	c.a.SUBImm(arm64.X9, arm64.X9, 1)
	c.a.STR(arm64.X9, arm64.X8, jitThNFrame)
	lNoFill := c.a.NewLabel()
	c.a.CMPImm(arm64.X6, 0)
	c.a.BCond(arm64.LE, lNoFill)
	c.a.ADDShift(arm64.X10, a64Stack, arm64.X7, arm64.ShiftLSL, 4)
	c.a.MOV(arm64.X11, arm64.X6)
	c.a.MOVZ(arm64.X12, uint16(tagNil), 0)
	lLoop := c.a.NewLabel()
	c.a.Bind(lLoop)
	c.a.STP(arm64.XZR, arm64.X12, arm64.X10, 0)
	c.a.ADDImm(arm64.X10, arm64.X10, 16)
	c.a.SUBSImm(arm64.X11, arm64.X11, 1)
	c.a.BCond(arm64.NE, lLoop)
	c.a.ADD(arm64.X7, arm64.X7, arm64.X6)
	c.a.Bind(lNoFill)
	c.a.STR(arm64.X7, arm64.X8, jitThTop)
	c.a.LDR(arm64.X10, a64Ctx, jitCtxEntryDepth)
	c.a.CMP(arm64.X9, arm64.X10)
	c.a.BCond(arm64.LE, c.doneLabel())
	c.a.B(c.retRestoreLabel())
}

func (c *a64Baseline) emitReturn1(a int) {
	c.loadReg(a, arm64.X14, arm64.X15)
	c.a.LDR(arm64.X2, a64Ctx, jitCtxCI)
	c.a.MOVZ(arm64.X3, uint16(jitCFSize), 0)
	c.a.MUL(arm64.X4, arm64.X2, arm64.X3)
	c.a.LDR(arm64.X5, a64Ctx, jitCtxFrames)
	c.a.ADD(arm64.X4, arm64.X5, arm64.X4)
	c.a.LDR(arm64.X6, arm64.X4, jitCFNResults)
	c.a.LDR(arm64.X7, arm64.X4, jitCFFuncIdx)
	c.a.LDR(arm64.X8, a64Ctx, jitCtxTh)
	c.a.LDR(arm64.X9, arm64.X8, jitThNFrame)
	c.a.SUBImm(arm64.X9, arm64.X9, 1)
	c.a.STR(arm64.X9, arm64.X8, jitThNFrame)

	lSetTop := c.a.NewLabel()
	lOneRes := c.a.NewLabel()
	c.a.CBZ(arm64.X6, lSetTop) // nres == 0 -> top = funcIdx
	c.a.ADDShift(arm64.X10, a64Stack, arm64.X7, arm64.ShiftLSL, 4)
	c.a.STP(arm64.X14, arm64.X15, arm64.X10, 0)
	c.a.CMPImm(arm64.X6, 1)
	c.a.BCond(arm64.LE, lOneRes) // nres == 1 or nres == -1 (MULTRET)
	c.a.ADDImm(arm64.X10, arm64.X10, 16)
	c.a.SUBImm(arm64.X11, arm64.X6, 1)
	c.a.MOVZ(arm64.X12, uint16(tagNil), 0)
	lLoop := c.a.NewLabel()
	c.a.Bind(lLoop)
	c.a.STP(arm64.XZR, arm64.X12, arm64.X10, 0)
	c.a.ADDImm(arm64.X10, arm64.X10, 16)
	c.a.SUBSImm(arm64.X11, arm64.X11, 1)
	c.a.BCond(arm64.NE, lLoop)
	c.a.ADD(arm64.X7, arm64.X7, arm64.X6)
	c.a.B(lSetTop)
	c.a.Bind(lOneRes)
	c.a.ADDImm(arm64.X7, arm64.X7, 1)
	c.a.Bind(lSetTop)
	c.a.STR(arm64.X7, arm64.X8, jitThTop)
	c.a.LDR(arm64.X10, a64Ctx, jitCtxEntryDepth)
	c.a.CMP(arm64.X9, arm64.X10)
	c.a.BCond(arm64.LE, c.doneLabel())
	c.a.B(c.retRestoreLabel())
}

func (c *a64Baseline) emitReturn(ipc int, w bytecode.Instruction) {
	if w.K() {
		c.a.B(c.cold(ipc)) // needs closeUpvals
		return
	}
	b := w.B()
	if b == 1 {
		c.emitReturn0()
		return
	}
	if b == 2 {
		c.emitReturn1(w.A())
		return
	}
	c.a.B(c.cold(ipc))
}

// emitReturnRestoreCaller restores caller frame (inputs: X5=frames, X9=newNFrame > entryDepth)
// and tail-jumps directly to caller's compiled native pcEntries[callerPC], or exits jitExitReload.
func (c *a64Baseline) emitReturnRestoreCaller() {
	c.a.Bind(c.retRestore)
	c.a.SUBImm(arm64.X2, arm64.X9, 1) // callerCI
	c.a.MOVZ(arm64.X3, uint16(jitCFSize), 0)
	c.a.MUL(arm64.X4, arm64.X2, arm64.X3)
	c.a.ADD(arm64.X4, arm64.X5, arm64.X4) // X4 = callerCF
	c.a.LDRB(arm64.X6, arm64.X4, jitCFKind)
	c.a.CBNZ(arm64.X6, c.reloadLabel())
	c.a.LDRB(arm64.X6, arm64.X4, jitCFFinish)
	c.a.CBNZ(arm64.X6, c.reloadLabel())

	c.a.LDRW(arm64.X8, arm64.X4, jitCFClRef)
	c.emitResolveSmall(arm64.X8, arm64.X9, arm64.X10) // X9 = caller *Closure
	c.a.LDRW(arm64.X11, arm64.X9, jitClProtoIdx)      // X11 = callerProtoIdx
	c.a.LDR(arm64.X12, a64Ctx, jitCtxJITProtoCap)
	c.a.CMP(arm64.X11, arm64.X12)
	c.a.BCond(arm64.HS, c.reloadLabel())
	c.a.LDR(arm64.X12, a64Ctx, jitCtxJITPCTables)
	c.a.LDRReg(arm64.X16, arm64.X12, arm64.X11, true) // X16 = caller pcEntries slice base
	c.a.CBZ(arm64.X16, c.reloadLabel())

	// Caller is compiled: update ctx & pinned regs and jump directly to pcEntries[callerPC]!
	c.a.STR(arm64.X2, a64Ctx, jitCtxCI)
	c.a.LDR(arm64.X7, arm64.X4, jitCFBase)
	c.a.STR(arm64.X7, a64Ctx, jitCtxBase)
	c.a.ADDShift(a64Regs, a64Stack, arm64.X7, arm64.ShiftLSL, 4)

	c.a.LDRW(arm64.X12, arm64.X9, jitClUpvalsRef)
	lNoUv := c.a.NewLabel()
	lCheckBP := c.a.NewLabel()
	c.a.CBZ(arm64.X12, lNoUv)
	c.emitResolveSmall(arm64.X12, a64ClUpvals, arm64.X10)
	c.a.B(lCheckBP)
	c.a.Bind(lNoUv)
	c.a.MOVZ(a64ClUpvals, 0, 0)

	c.a.Bind(lCheckBP)
	c.a.LDR(arm64.X12, a64Ctx, jitCtxProtos)
	c.a.LDRReg(arm64.X13, arm64.X12, arm64.X11, true) // caller *boundProto
	c.a.LDR(arm64.X14, a64Ctx, jitCtxCurBP)
	lSameBP := c.a.NewLabel()
	c.a.CMP(arm64.X14, arm64.X13)
	c.a.BCond(arm64.EQ, lSameBP)
	c.a.STR(arm64.X13, a64Ctx, jitCtxCurBP)
	c.a.LDR(a64Consts, arm64.X13, jitBPConsts)
	c.a.LDR(arm64.X14, arm64.X13, jitBPICSlots)
	c.a.LDR(a64IC, arm64.X13, jitBPIC)
	c.a.STR(arm64.X14, a64Ctx, jitCtxICSlots)
	c.a.Bind(lSameBP)

	c.a.LDR(arm64.X14, arm64.X4, jitCFPC)
	c.a.LDRReg(arm64.X16, arm64.X16, arm64.X14, true)
	c.a.BR(arm64.X16)
}
