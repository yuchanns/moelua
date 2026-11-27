//go:build amd64 && !moeluanojit && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"fmt"
	"math"
	"sync"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/internal/jit/amd64"
	"github.com/yuchanns/moelua/internal/jit/execmem"
	"github.com/yuchanns/moelua/internal/jit/ir"
)

// Pinned AMD64 registers across the baseline JIT:
//
//	RBX: *jitExecCtx
//	R12: regs (&stack[base], frame R[0])
//	R13: pagePtrs (&s.arena.pagePtrs[0])
//	R15: poll countdown
//	R14: Go runtime g (untouched)
//	RBP, RSP: stack frame (untouched by compiled functions)
//	XMM15: Go ABIInternal zero register (untouched)
//	RAX, RCX, RDX, RSI, RDI, R8, R9, R10, R11, XMM0..XMM14: free scratch registers
const (
	x64Ctx      = amd64.RBX
	x64Regs     = amd64.R12
	x64PagePtrs = amd64.R13
	x64Poll     = amd64.R15
)

var (
	x64TrampOnce     sync.Once
	x64TrampRegion   *execmem.Region
	x64TrampEntry    uintptr
	x64TrampEpilogue uintptr
	x64TrampOK       bool
)

func getJITTrampoline() (entry, epilogue uintptr, ok bool) {
	x64TrampOnce.Do(func() {
		a := amd64.New(128)
		fl := amd64.ComputeFrameLayout(0, []amd64.Reg{x64Ctx, x64Regs, x64PagePtrs, x64Poll})
		a.EmitPrologue(fl)
		a.MOVQ(x64Ctx, amd64.RDI)
		a.MOVQLoad(x64Regs, amd64.BaseDisp(x64Ctx, jitCtxRegs))
		a.MOVQLoad(x64PagePtrs, amd64.BaseDisp(x64Ctx, jitCtxPagePtrs))
		a.MOVQLoad(x64Poll, amd64.BaseDisp(x64Ctx, jitCtxPoll))
		a.MOVQLoad(amd64.RAX, amd64.BaseDisp(x64Ctx, jitCtxEntryPC))
		a.JMPReg(amd64.RAX)

		epOff := a.Len()
		// Sync pinned registers back into ctx before returning RAX (exitCode) to Go.
		a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxRegs), x64Regs)
		a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxPoll), x64Poll)
		a.EmitEpilogue(fl)

		code, err := a.Finish()
		if err != nil {
			return
		}
		reg, err := execmem.Publish(code)
		if err != nil {
			return
		}
		x64TrampRegion = reg
		x64TrampEntry = reg.Addr()
		x64TrampEpilogue = reg.Addr() + uintptr(epOff)
		x64TrampOK = true
	})
	return x64TrampEntry, x64TrampEpilogue, x64TrampOK
}

type x64Stub struct {
	label amd64.Label
	pc    int
	code  uint64
}

type x64Baseline struct {
	a           *amd64.Assembler
	bp          *boundProto
	code        []bytecode.Instruction
	pcLabels    []amd64.Label
	pcOffsets   []int
	coldMap     map[int]amd64.Label
	pollMap     map[int]amd64.Label
	stubs       []x64Stub
	retRestore  amd64.Label
	needRetRest bool
	exitDone    amd64.Label
	needDone    bool
	exitReload  amd64.Label
	needReload  bool
	optLoops    []*x64OptLoopRef
	optByEntry  map[int]*x64OptLoopRef
	optByEnd    map[int]*x64OptLoopRef
}

func compileBaselineNative(bp *boundProto, fb *ir.Feedback) (*jitFunc, error) {
	code := bp.proto.Code
	n := len(code)
	if n == 0 {
		return nil, fmt.Errorf("jit/amd64: empty proto")
	}
	c := &x64Baseline{
		a:         amd64.New(max(256, n*56)),
		bp:        bp,
		code:      code,
		pcLabels:  make([]amd64.Label, n+1),
		pcOffsets: make([]int, n),
		coldMap:   make(map[int]amd64.Label, 16),
		pollMap:   make(map[int]amd64.Label, 8),
	}
	for i := range n + 1 {
		c.pcLabels[i] = c.a.NewLabel()
	}

	if optRegs := ir.OptimizeProto(bp.proto, fb, x64OptNumGPR, x64OptNumFP, x64OptMaxSpills); len(optRegs) > 0 {
		c.optByEntry = make(map[int]*x64OptLoopRef, len(optRegs))
		c.optByEnd = make(map[int]*x64OptLoopRef, len(optRegs))
		for ipc := 0; ipc < n; ipc++ {
			if r, ok := optRegs[ipc]; ok {
				ref := &x64OptLoopRef{
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
			c.a.JMP(ref.entryLbl)
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
		c.a.MOVImm64(amd64.RAX, jitExitDone)
		c.a.MOVQLoad(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxEpilogue))
		c.a.JMPReg(amd64.RCX)
	}
	if c.needReload {
		c.a.Bind(c.exitReload)
		c.a.MOVImm64(amd64.RAX, jitExitReload)
		c.a.MOVQLoad(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxEpilogue))
		c.a.JMPReg(amd64.RCX)
	}
	for _, st := range c.stubs {
		c.a.Bind(st.label)
		c.a.MOVQStoreImm32(amd64.BaseDisp(x64Ctx, jitCtxPC), int32(st.pc))
		c.a.MOVImm64(amd64.RAX, st.code)
		c.a.MOVQLoad(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxEpilogue))
		c.a.JMPReg(amd64.RCX)
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

func (c *x64Baseline) cold(ipc int) amd64.Label {
	if l, ok := c.coldMap[ipc]; ok {
		return l
	}
	l := c.a.NewLabel()
	c.coldMap[ipc] = l
	c.stubs = append(c.stubs, x64Stub{label: l, pc: ipc, code: jitExitCold})
	return l
}

func (c *x64Baseline) poll(targetPC int) amd64.Label {
	if l, ok := c.pollMap[targetPC]; ok {
		return l
	}
	l := c.a.NewLabel()
	c.pollMap[targetPC] = l
	c.stubs = append(c.stubs, x64Stub{label: l, pc: targetPC, code: jitExitPoll})
	return l
}

func (c *x64Baseline) doneLabel() amd64.Label {
	if !c.needDone {
		c.needDone = true
		c.exitDone = c.a.NewLabel()
	}
	return c.exitDone
}

func (c *x64Baseline) reloadLabel() amd64.Label {
	if !c.needReload {
		c.needReload = true
		c.exitReload = c.a.NewLabel()
	}
	return c.exitReload
}

func (c *x64Baseline) retRestoreLabel() amd64.Label {
	if !c.needRetRest {
		c.needRetRest = true
		c.retRestore = c.a.NewLabel()
	}
	return c.retRestore
}

// --- Register & Arena Helpers ---

func (c *x64Baseline) regBits(slot int) amd64.Mem { return amd64.BaseDisp(x64Regs, int32(slot*16)) }
func (c *x64Baseline) regTag(slot int) amd64.Mem  { return amd64.BaseDisp(x64Regs, int32(slot*16+8)) }

func (c *x64Baseline) loadReg(slot int, bitsReg, tagReg amd64.Reg) {
	c.a.MOVQLoad(bitsReg, c.regBits(slot))
	c.a.MOVQLoad(tagReg, c.regTag(slot))
}

func (c *x64Baseline) storeReg(slot int, bitsReg, tagReg amd64.Reg) {
	c.a.MOVQStore(c.regBits(slot), bitsReg)
	c.a.MOVQStore(c.regTag(slot), tagReg)
}

func (c *x64Baseline) storeRegTagImm(slot int, bitsReg amd64.Reg, tag int32) {
	c.a.MOVQStore(c.regBits(slot), bitsReg)
	c.a.MOVQStoreImm32(c.regTag(slot), tag)
}

func (c *x64Baseline) loadConst(idx int, bitsReg, tagReg, tmpBase amd64.Reg) {
	c.a.MOVQLoad(tmpBase, amd64.BaseDisp(x64Ctx, jitCtxConsts))
	c.a.MOVQLoad(bitsReg, amd64.BaseDisp(tmpBase, int32(idx*16)))
	c.a.MOVQLoad(tagReg, amd64.BaseDisp(tmpBase, int32(idx*16+8)))
}

func (c *x64Baseline) loadRegOrConst(isK bool, idx int, bitsReg, tagReg, tmpBase amd64.Reg) {
	if isK {
		c.loadConst(idx, bitsReg, tagReg, tmpBase)
	} else {
		c.loadReg(idx, bitsReg, tagReg)
	}
}

// emitResolveSmall resolves a non-null, non-large Arena ObjRef in ref into dst using tmp.
// ref and dst may alias; tmp must be distinct from ref and dst.
func (c *x64Baseline) emitResolveSmall(ref, dst, tmp amd64.Reg) {
	c.a.MOVL(tmp, ref)
	c.a.SHRLImm(tmp, jitArenaWordBits)
	c.a.MOVQLoad(tmp, amd64.Indexed(x64PagePtrs, tmp, 8, 0))
	if dst != ref {
		c.a.MOVL(dst, ref)
	}
	c.a.ANDLImm(dst, jitArenaWordMask)
	c.a.LEAQ(dst, amd64.Indexed(tmp, dst, 8, 0))
}

// emitResolveFast resolves either a small-slab or large-span Arena ObjRef in ref into dst.
func (c *x64Baseline) emitResolveFast(ref, dst, tmp amd64.Reg) {
	lLarge := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.TESTQImm(ref, -1<<31) // test bit 31
	c.a.JCC(amd64.NE, lLarge)
	c.emitResolveSmall(ref, dst, tmp)
	c.a.JMP(lDone)
	c.a.Bind(lLarge)
	c.a.MOVL(tmp, ref)
	c.a.ANDLImm(tmp, 0x7FFFFFFF)
	c.a.LEAQ(tmp, amd64.Indexed(tmp, tmp, 2, 0)) // idx * 3 (24B slice header = 3*8B)
	c.a.MOVQLoad(dst, amd64.BaseDisp(x64Ctx, jitCtxLargeSpans))
	c.a.MOVQLoad(dst, amd64.Indexed(dst, tmp, 8, 0))
	c.a.Bind(lDone)
}

func (c *x64Baseline) emitJumpToPC(target int, isBack bool) {
	if isBack {
		c.a.SUBQImm(x64Poll, 1)
		c.a.JCC(amd64.LE, c.poll(target))
	}
	c.a.JMP(c.pcLabels[target])
}

func (c *x64Baseline) emitFusedCond(ipc int, w bytecode.Instruction, trueCond amd64.Cond) {
	if ipc+1 >= len(c.code) || c.code[ipc+1].Op() != bytecode.OP_JMP {
		c.a.JMP(c.cold(ipc))
		return
	}
	sj := c.code[ipc+1].SJ()
	jumpTarget := ipc + 2 + sj
	skipTarget := ipc + 2
	if jumpTarget < 0 || jumpTarget > len(c.code) {
		c.a.JMP(c.cold(ipc))
		return
	}
	takeCond := trueCond
	if !w.K() {
		takeCond = trueCond.Invert()
	}
	if sj < 0 {
		c.a.JCC(takeCond.Invert(), c.pcLabels[skipTarget])
		c.a.SUBQImm(x64Poll, 1)
		c.a.JCC(amd64.LE, c.poll(jumpTarget))
		c.a.JMP(c.pcLabels[jumpTarget])
		return
	}
	c.a.JCC(takeCond, c.pcLabels[jumpTarget])
	c.a.JMP(c.pcLabels[skipTarget])
}

// emitFusedBool branches based on a compile-time-known boolean result (res == w.K() jumps).
func (c *x64Baseline) emitFusedBool(ipc int, w bytecode.Instruction, res bool) {
	if ipc+1 >= len(c.code) || c.code[ipc+1].Op() != bytecode.OP_JMP {
		c.a.JMP(c.cold(ipc))
		return
	}
	sj := c.code[ipc+1].SJ()
	jumpTarget := ipc + 2 + sj
	skipTarget := ipc + 2
	if jumpTarget < 0 || jumpTarget > len(c.code) {
		c.a.JMP(c.cold(ipc))
		return
	}
	if res == w.K() {
		c.emitJumpToPC(jumpTarget, sj < 0)
	} else {
		c.a.JMP(c.pcLabels[skipTarget])
	}
}

func (c *x64Baseline) emitOp(ipc int, w bytecode.Instruction) {
	a := w.A()
	switch w.Op() {
	case bytecode.OP_MOVE:
		c.loadReg(w.B(), amd64.RAX, amd64.RDX)
		c.storeReg(a, amd64.RAX, amd64.RDX)

	case bytecode.OP_LOADI:
		c.a.MOVQStoreImm32(c.regBits(a), int32(w.SBx()))
		c.a.MOVQStoreImm32(c.regTag(a), int32(tagInt))

	case bytecode.OP_LOADF:
		c.a.MOVImm64(amd64.RAX, math.Float64bits(float64(w.SBx())))
		c.storeRegTagImm(a, amd64.RAX, int32(tagFloat))

	case bytecode.OP_LOADK:
		bx := w.Bx()
		kc := c.bp.proto.Consts[bx]
		switch kc.Kind {
		case bytecode.ConstInt:
			c.a.MOVImm64(amd64.RAX, uint64(kc.Int))
			c.storeRegTagImm(a, amd64.RAX, int32(tagInt))
		case bytecode.ConstFloat:
			c.a.MOVImm64(amd64.RAX, math.Float64bits(kc.Float))
			c.storeRegTagImm(a, amd64.RAX, int32(tagFloat))
		case bytecode.ConstNil:
			c.a.MOVQStoreImm32(c.regBits(a), 0)
			c.a.MOVQStoreImm32(c.regTag(a), int32(tagNil))
		case bytecode.ConstFalse:
			c.a.MOVQStoreImm32(c.regBits(a), 0)
			c.a.MOVQStoreImm32(c.regTag(a), int32(tagFalse))
		case bytecode.ConstTrue:
			c.a.MOVQStoreImm32(c.regBits(a), 0)
			c.a.MOVQStoreImm32(c.regTag(a), int32(tagTrue))
		default:
			c.loadConst(bx, amd64.RAX, amd64.RDX, amd64.RCX)
			c.storeReg(a, amd64.RAX, amd64.RDX)
		}

	case bytecode.OP_LOADFALSE:
		c.a.MOVQStoreImm32(c.regBits(a), 0)
		c.a.MOVQStoreImm32(c.regTag(a), int32(tagFalse))

	case bytecode.OP_LFALSESKIP:
		c.a.MOVQStoreImm32(c.regBits(a), 0)
		c.a.MOVQStoreImm32(c.regTag(a), int32(tagFalse))
		c.a.JMP(c.pcLabels[ipc+2])

	case bytecode.OP_LOADTRUE:
		c.a.MOVQStoreImm32(c.regBits(a), 0)
		c.a.MOVQStoreImm32(c.regTag(a), int32(tagTrue))

	case bytecode.OP_LOADNIL:
		b := w.B()
		for i := 0; i <= b; i++ {
			c.a.MOVQStoreImm32(c.regBits(a+i), 0)
			c.a.MOVQStoreImm32(c.regTag(a+i), int32(tagNil))
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
		c.loadReg(w.B(), amd64.RAX, amd64.RDX)
		c.a.TESTQ(amd64.RDX, amd64.RDX)
		c.a.JCC(amd64.NE, c.cold(ipc))
		c.a.NOTQ(amd64.RAX)
		c.storeRegTagImm(a, amd64.RAX, int32(tagInt))

	case bytecode.OP_NOT:
		c.a.MOVQLoad(amd64.RAX, c.regTag(w.B()))
		c.a.SUBQImm(amd64.RAX, int32(tagNil))
		c.a.CMPQImm(amd64.RAX, 1) // BE => falsy => result true
		c.a.MOVImm32(amd64.RCX, uint32(tagFalse))
		c.a.MOVImm32(amd64.RDX, uint32(tagTrue))
		c.a.CMOVQ(amd64.BE, amd64.RCX, amd64.RDX)
		c.a.MOVQStoreImm32(c.regBits(a), 0)
		c.a.MOVQStore(c.regTag(a), amd64.RCX)

	case bytecode.OP_LEN:
		c.emitLen(ipc, w)

	case bytecode.OP_JMP:
		sj := w.SJ()
		target := ipc + 1 + sj
		if target < 0 || target > len(c.code) {
			c.a.JMP(c.cold(ipc))
			return
		}
		c.emitJumpToPC(target, sj < 0)

	case bytecode.OP_EQ:
		c.emitEQ(ipc, w)
	case bytecode.OP_LT:
		c.emitOrd(ipc, w, amd64.L, true)
	case bytecode.OP_LE:
		c.emitOrd(ipc, w, amd64.LE, false)
	case bytecode.OP_EQK:
		c.emitEQK(ipc, w)
	case bytecode.OP_EQI:
		c.emitEQI(ipc, w)
	case bytecode.OP_LTI:
		c.emitOrdI(ipc, w, amd64.L, true, false)
	case bytecode.OP_LEI:
		c.emitOrdI(ipc, w, amd64.LE, false, false)
	case bytecode.OP_GTI:
		c.emitOrdI(ipc, w, amd64.G, true, true)
	case bytecode.OP_GEI:
		c.emitOrdI(ipc, w, amd64.GE, false, true)
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
			c.a.JMP(c.cold(ipc))
			return
		}
		c.a.CMPQMemImm(c.regTag(a+3), int32(tagNil))
		c.a.JCC(amd64.E, c.pcLabels[ipc+1])
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
		c.a.JMP(c.cold(ipc))
	}
}

// --- Upvalues ---

func (c *x64Baseline) emitLoadUpval(ipc, b int, bitsReg, tagReg, tmp1, tmp2 amd64.Reg) {
	c.a.MOVQLoad(tmp1, amd64.BaseDisp(x64Ctx, jitCtxClUpvals))
	c.a.TESTQ(tmp1, tmp1)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.a.MOVLLoad(tmp1, amd64.BaseDisp(tmp1, int32(b*4)))
	c.emitResolveSmall(tmp1, tmp1, tmp2) // tmp1 = *Upvalue
	c.a.MOVLLoad(tmp2, amd64.BaseDisp(tmp1, jitUvOwnerRef))
	lOpen := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.TESTL(tmp2, tmp2)
	c.a.JCC(amd64.NE, lOpen)
	c.a.MOVQLoad(bitsReg, amd64.BaseDisp(tmp1, jitUvClosed))
	c.a.MOVQLoad(tagReg, amd64.BaseDisp(tmp1, jitUvClosed+8))
	c.a.JMP(lDone)
	c.a.Bind(lOpen)
	c.a.MOVLLoad(bitsReg, amd64.BaseDisp(x64Ctx, jitCtxThSelf))
	c.a.CMPL(tmp2, bitsReg)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVSLQ(tmp2, amd64.BaseDisp(tmp1, jitUvStackIdx))
	c.a.SHLQImm(tmp2, 4)
	c.a.ADDQMem(tmp2, amd64.BaseDisp(x64Ctx, jitCtxStack))
	c.a.MOVQLoad(bitsReg, amd64.BaseDisp(tmp2, 0))
	c.a.MOVQLoad(tagReg, amd64.BaseDisp(tmp2, 8))
	c.a.Bind(lDone)
}

func (c *x64Baseline) emitGetUpval(ipc, a, b int) {
	c.emitLoadUpval(ipc, b, amd64.RAX, amd64.RDX, amd64.RCX, amd64.RSI)
	c.storeReg(a, amd64.RAX, amd64.RDX)
}

func (c *x64Baseline) emitSetUpval(ipc, a, b int) {
	c.a.MOVQLoad(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxClUpvals))
	c.a.TESTQ(amd64.RCX, amd64.RCX)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(amd64.RCX, int32(b*4)))
	c.emitResolveSmall(amd64.RCX, amd64.RCX, amd64.RSI) // RCX = *Upvalue
	c.loadReg(a, amd64.RAX, amd64.RDX)
	c.a.MOVLLoad(amd64.RSI, amd64.BaseDisp(amd64.RCX, jitUvOwnerRef))
	lOpen := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.TESTL(amd64.RSI, amd64.RSI)
	c.a.JCC(amd64.NE, lOpen)
	// Closed upvalue GC barrier check
	lStoreClosed := c.a.NewLabel()
	c.a.LEAQ(amd64.RDI, amd64.BaseDisp(amd64.RDX, -int32(tagFirstGC)))
	c.a.CMPQImm(amd64.RDI, int32(tagLastGC-tagFirstGC))
	c.a.JCC(amd64.A, lStoreClosed)
	c.a.MOVZXBL(amd64.RDI, amd64.BaseDisp(amd64.RCX, jitUvMark))
	c.a.TESTQImm(amd64.RDI, 1<<jitMarkBlackBit)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.Bind(lStoreClosed)
	c.a.MOVQStore(amd64.BaseDisp(amd64.RCX, jitUvClosed), amd64.RAX)
	c.a.MOVQStore(amd64.BaseDisp(amd64.RCX, jitUvClosed+8), amd64.RDX)
	c.a.JMP(lDone)
	c.a.Bind(lOpen)
	c.a.MOVLLoad(amd64.RDI, amd64.BaseDisp(x64Ctx, jitCtxThSelf))
	c.a.CMPL(amd64.RSI, amd64.RDI)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVSLQ(amd64.RSI, amd64.BaseDisp(amd64.RCX, jitUvStackIdx))
	c.a.SHLQImm(amd64.RSI, 4)
	c.a.ADDQMem(amd64.RSI, amd64.BaseDisp(x64Ctx, jitCtxStack))
	c.a.MOVQStore(amd64.BaseDisp(amd64.RSI, 0), amd64.RAX)
	c.a.MOVQStore(amd64.BaseDisp(amd64.RSI, 8), amd64.RDX)
	c.a.Bind(lDone)
}

// --- Arithmetic & Bitwise ---

func (c *x64Baseline) emitAddI(ipc int, w bytecode.Instruction) {
	a := w.A()
	sc := int32(w.SC())
	c.loadReg(w.B(), amd64.RAX, amd64.RDX)
	lFloat := c.a.NewLabel()
	c.a.TESTQ(amd64.RDX, amd64.RDX)
	c.a.JCC(amd64.NE, lFloat)
	c.a.ADDQImm(amd64.RAX, sc)
	c.storeRegTagImm(a, amd64.RAX, int32(tagInt))
	c.a.JMP(c.pcLabels[ipc+2])

	c.a.Bind(lFloat)
	c.a.CMPQImm(amd64.RDX, int32(tagFloat))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVQToX(amd64.XMM0, amd64.RAX)
	c.a.MOVImm64(amd64.RCX, math.Float64bits(float64(sc)))
	c.a.MOVQToX(amd64.XMM1, amd64.RCX)
	c.a.ADDSD(amd64.XMM0, amd64.XMM1)
	c.a.MOVSDStore(c.regBits(a), amd64.XMM0)
	c.a.MOVQStoreImm32(c.regTag(a), int32(tagFloat))
	c.a.JMP(c.pcLabels[ipc+2])
}

// emitIntFloorDivRAX computes Lua 5.5 floor(RAX / rhs) into RAX. rhs must not be RAX/RDX/RSI.
func (c *x64Baseline) emitIntFloorDivRAX(ipc int, rhs amd64.Reg) {
	c.a.TESTQ(rhs, rhs)
	c.a.JCC(amd64.E, c.cold(ipc))
	lNotNeg1 := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.CMPQImm(rhs, -1)
	c.a.JCC(amd64.NE, lNotNeg1)
	c.a.NEGQ(amd64.RAX)
	c.a.JMP(lDone)
	c.a.Bind(lNotNeg1)
	c.a.MOVQ(amd64.RSI, amd64.RAX) // save lhs for sign check
	c.a.CQO()
	c.a.IDIVQ(rhs) // RAX = q, RDX = r
	c.a.TESTQ(amd64.RDX, amd64.RDX)
	c.a.JCC(amd64.E, lDone)
	c.a.XORQ(amd64.RSI, rhs)
	c.a.JCC(amd64.NS, lDone) // same sign -> done
	c.a.SUBQImm(amd64.RAX, 1)
	c.a.Bind(lDone)
}

// emitIntFloorModRAX computes Lua 5.5 floor(RAX % rhs) into RAX. rhs must not be RAX/RDX/RSI.
func (c *x64Baseline) emitIntFloorModRAX(ipc int, rhs amd64.Reg) {
	c.a.TESTQ(rhs, rhs)
	c.a.JCC(amd64.E, c.cold(ipc))
	lNotNeg1 := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.CMPQImm(rhs, -1)
	c.a.JCC(amd64.NE, lNotNeg1)
	c.a.XORL(amd64.RAX, amd64.RAX)
	c.a.JMP(lDone)
	c.a.Bind(lNotNeg1)
	c.a.CQO()
	c.a.IDIVQ(rhs) // RDX = r
	c.a.MOVQ(amd64.RAX, amd64.RDX)
	c.a.TESTQ(amd64.RAX, amd64.RAX)
	c.a.JCC(amd64.E, lDone)
	c.a.MOVQ(amd64.RSI, amd64.RAX)
	c.a.XORQ(amd64.RSI, rhs)
	c.a.JCC(amd64.NS, lDone)
	c.a.ADDQ(amd64.RAX, rhs)
	c.a.Bind(lDone)
}

func (c *x64Baseline) emitFPBin(op bytecode.OpCode, dst, src amd64.XReg) {
	switch op {
	case bytecode.OP_ADD, bytecode.OP_ADDK:
		c.a.ADDSD(dst, src)
	case bytecode.OP_SUB, bytecode.OP_SUBK:
		c.a.SUBSD(dst, src)
	case bytecode.OP_MUL, bytecode.OP_MULK:
		c.a.MULSD(dst, src)
	case bytecode.OP_DIV, bytecode.OP_DIVK:
		c.a.DIVSD(dst, src)
	}
}

func (c *x64Baseline) emitArithK(ipc int, w bytecode.Instruction) {
	op := w.Op()
	a := w.A()
	kc := c.bp.proto.Consts[w.C()]
	if kc.Kind != bytecode.ConstInt && kc.Kind != bytecode.ConstFloat {
		c.a.JMP(c.cold(ipc))
		return
	}
	c.loadReg(w.B(), amd64.RAX, amd64.RDX)
	if kc.Kind == bytecode.ConstInt && op != bytecode.OP_DIVK {
		lFloat := c.a.NewLabel()
		c.a.TESTQ(amd64.RDX, amd64.RDX)
		c.a.JCC(amd64.NE, lFloat)
		c.a.MOVImm64(amd64.RCX, uint64(kc.Int))
		switch op {
		case bytecode.OP_ADDK:
			c.a.ADDQ(amd64.RAX, amd64.RCX)
		case bytecode.OP_SUBK:
			c.a.SUBQ(amd64.RAX, amd64.RCX)
		case bytecode.OP_MULK:
			c.a.IMULQ(amd64.RAX, amd64.RCX)
		case bytecode.OP_IDIVK:
			c.emitIntFloorDivRAX(ipc, amd64.RCX)
		case bytecode.OP_MODK:
			c.emitIntFloorModRAX(ipc, amd64.RCX)
		}
		c.storeRegTagImm(a, amd64.RAX, int32(tagInt))
		c.a.JMP(c.pcLabels[ipc+2])

		c.a.Bind(lFloat)
		// Defer float IDIVK/MODK to cold so we never require SSE4.1 ROUNDSD on baseline x86-64.
		if op == bytecode.OP_IDIVK || op == bytecode.OP_MODK {
			c.a.JMP(c.cold(ipc))
			return
		}
		c.a.CMPQImm(amd64.RDX, int32(tagFloat))
		c.a.JCC(amd64.NE, c.cold(ipc))
		c.a.MOVQToX(amd64.XMM0, amd64.RAX)
		c.a.MOVImm64(amd64.RCX, math.Float64bits(float64(kc.Int)))
		c.a.MOVQToX(amd64.XMM1, amd64.RCX)
		c.emitFPBin(op, amd64.XMM0, amd64.XMM1)
		c.a.MOVSDStore(c.regBits(a), amd64.XMM0)
		c.a.MOVQStoreImm32(c.regTag(a), int32(tagFloat))
		c.a.JMP(c.pcLabels[ipc+2])
		return
	}

	if op == bytecode.OP_IDIVK || op == bytecode.OP_MODK {
		c.a.JMP(c.cold(ipc))
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
	c.a.TESTQ(amd64.RDX, amd64.RDX)
	c.a.JCC(amd64.E, lInt)
	c.a.CMPQImm(amd64.RDX, int32(tagFloat))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVQToX(amd64.XMM0, amd64.RAX)
	c.a.JMP(lReady)
	c.a.Bind(lInt)
	c.a.CVTSI2SDQ(amd64.XMM0, amd64.RAX)
	c.a.Bind(lReady)
	c.a.MOVImm64(amd64.RCX, math.Float64bits(kf))
	c.a.MOVQToX(amd64.XMM1, amd64.RCX)
	c.emitFPBin(op, amd64.XMM0, amd64.XMM1)
	c.a.MOVSDStore(c.regBits(a), amd64.XMM0)
	c.a.MOVQStoreImm32(c.regTag(a), int32(tagFloat))
	c.a.JMP(c.pcLabels[ipc+2])
}

func (c *x64Baseline) emitArith(ipc int, w bytecode.Instruction) {
	op := w.Op()
	a := w.A()
	c.loadReg(w.B(), amd64.RAX, amd64.R8) // lhs
	c.loadReg(w.C(), amd64.RCX, amd64.R9) // rhs

	if op != bytecode.OP_DIV {
		lNotBothInt := c.a.NewLabel()
		c.a.MOVQ(amd64.R10, amd64.R8)
		c.a.ORQ(amd64.R10, amd64.R9)
		c.a.JCC(amd64.NE, lNotBothInt)
		switch op {
		case bytecode.OP_ADD:
			c.a.ADDQ(amd64.RAX, amd64.RCX)
		case bytecode.OP_SUB:
			c.a.SUBQ(amd64.RAX, amd64.RCX)
		case bytecode.OP_MUL:
			c.a.IMULQ(amd64.RAX, amd64.RCX)
		case bytecode.OP_IDIV:
			c.emitIntFloorDivRAX(ipc, amd64.RCX)
		case bytecode.OP_MOD:
			c.emitIntFloorModRAX(ipc, amd64.RCX)
		}
		c.storeRegTagImm(a, amd64.RAX, int32(tagInt))
		c.a.JMP(c.pcLabels[ipc+2])
		c.a.Bind(lNotBothInt)
		if op == bytecode.OP_IDIV || op == bytecode.OP_MOD {
			c.a.JMP(c.cold(ipc))
			return
		}
	}

	c.a.MOVQ(amd64.R10, amd64.R8)
	c.a.ORQ(amd64.R10, amd64.R9)
	c.a.CMPQImm(amd64.R10, int32(tagFloat))
	c.a.JCC(amd64.A, c.cold(ipc))

	lLhsF := c.a.NewLabel()
	lLhsDone := c.a.NewLabel()
	c.a.TESTQ(amd64.R8, amd64.R8)
	c.a.JCC(amd64.NE, lLhsF)
	c.a.CVTSI2SDQ(amd64.XMM0, amd64.RAX)
	c.a.JMP(lLhsDone)
	c.a.Bind(lLhsF)
	c.a.MOVQToX(amd64.XMM0, amd64.RAX)
	c.a.Bind(lLhsDone)

	lRhsF := c.a.NewLabel()
	lRhsDone := c.a.NewLabel()
	c.a.TESTQ(amd64.R9, amd64.R9)
	c.a.JCC(amd64.NE, lRhsF)
	c.a.CVTSI2SDQ(amd64.XMM1, amd64.RCX)
	c.a.JMP(lRhsDone)
	c.a.Bind(lRhsF)
	c.a.MOVQToX(amd64.XMM1, amd64.RCX)
	c.a.Bind(lRhsDone)

	c.emitFPBin(op, amd64.XMM0, amd64.XMM1)
	c.a.MOVSDStore(c.regBits(a), amd64.XMM0)
	c.a.MOVQStoreImm32(c.regTag(a), int32(tagFloat))
	c.a.JMP(c.pcLabels[ipc+2])
}

func (c *x64Baseline) emitBitwiseK(ipc int, w bytecode.Instruction) {
	kc := c.bp.proto.Consts[w.C()]
	if kc.Kind != bytecode.ConstInt {
		c.a.JMP(c.cold(ipc))
		return
	}
	c.loadReg(w.B(), amd64.RAX, amd64.RDX)
	c.a.TESTQ(amd64.RDX, amd64.RDX)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVImm64(amd64.RCX, uint64(kc.Int))
	switch w.Op() {
	case bytecode.OP_BANDK:
		c.a.ANDQ(amd64.RAX, amd64.RCX)
	case bytecode.OP_BORK:
		c.a.ORQ(amd64.RAX, amd64.RCX)
	case bytecode.OP_BXORK:
		c.a.XORQ(amd64.RAX, amd64.RCX)
	}
	c.storeRegTagImm(w.A(), amd64.RAX, int32(tagInt))
	c.a.JMP(c.pcLabels[ipc+2])
}

func (c *x64Baseline) emitBitwise(ipc int, w bytecode.Instruction) {
	c.loadReg(w.B(), amd64.RAX, amd64.R8)
	c.loadReg(w.C(), amd64.RCX, amd64.R9)
	c.a.ORQ(amd64.R8, amd64.R9)
	c.a.JCC(amd64.NE, c.cold(ipc))
	switch w.Op() {
	case bytecode.OP_BAND:
		c.a.ANDQ(amd64.RAX, amd64.RCX)
	case bytecode.OP_BOR:
		c.a.ORQ(amd64.RAX, amd64.RCX)
	case bytecode.OP_BXOR:
		c.a.XORQ(amd64.RAX, amd64.RCX)
	}
	c.storeRegTagImm(w.A(), amd64.RAX, int32(tagInt))
	c.a.JMP(c.pcLabels[ipc+2])
}

func (c *x64Baseline) emitLuaShiftRAXByRCX(left bool) {
	lNeg := c.a.NewLabel()
	lZero := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.TESTQ(amd64.RCX, amd64.RCX)
	c.a.JCC(amd64.L, lNeg)
	c.a.CMPQImm(amd64.RCX, 64)
	c.a.JCC(amd64.AE, lZero)
	if left {
		c.a.SHLQCL(amd64.RAX)
	} else {
		c.a.SHRQCL(amd64.RAX)
	}
	c.a.JMP(lDone)
	c.a.Bind(lNeg)
	c.a.NEGQ(amd64.RCX)
	c.a.CMPQImm(amd64.RCX, 64)
	c.a.JCC(amd64.AE, lZero)
	if left {
		c.a.SHRQCL(amd64.RAX)
	} else {
		c.a.SHLQCL(amd64.RAX)
	}
	c.a.JMP(lDone)
	c.a.Bind(lZero)
	c.a.XORL(amd64.RAX, amd64.RAX)
	c.a.Bind(lDone)
}

func (c *x64Baseline) emitShiftImm(ipc int, w bytecode.Instruction) {
	c.loadReg(w.B(), amd64.RAX, amd64.RDX)
	c.a.TESTQ(amd64.RDX, amd64.RDX)
	c.a.JCC(amd64.NE, c.cold(ipc))
	sc := w.SC()
	if w.Op() == bytecode.OP_SHRI {
		if sc >= 64 || sc <= -64 {
			c.a.XORL(amd64.RAX, amd64.RAX)
		} else if sc > 0 {
			c.a.SHRQImm(amd64.RAX, uint8(sc))
		} else if sc < 0 {
			c.a.SHLQImm(amd64.RAX, uint8(-sc))
		}
		c.storeRegTagImm(w.A(), amd64.RAX, int32(tagInt))
		c.a.JMP(c.pcLabels[ipc+2])
		return
	}
	// OP_SHLI: R[A] = int64(sc) << R[B]
	c.a.MOVQ(amd64.RCX, amd64.RAX)
	c.a.MOVImm64(amd64.RAX, uint64(int64(sc)))
	c.emitLuaShiftRAXByRCX(true)
	c.storeRegTagImm(w.A(), amd64.RAX, int32(tagInt))
	c.a.JMP(c.pcLabels[ipc+2])
}

func (c *x64Baseline) emitShiftReg(ipc int, w bytecode.Instruction) {
	c.loadReg(w.B(), amd64.RAX, amd64.R8)
	c.loadReg(w.C(), amd64.RCX, amd64.R9)
	c.a.ORQ(amd64.R8, amd64.R9)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.emitLuaShiftRAXByRCX(w.Op() == bytecode.OP_SHL)
	c.storeRegTagImm(w.A(), amd64.RAX, int32(tagInt))
	c.a.JMP(c.pcLabels[ipc+2])
}

func (c *x64Baseline) emitUnm(ipc int, w bytecode.Instruction) {
	c.loadReg(w.B(), amd64.RAX, amd64.RDX)
	lFloat := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.TESTQ(amd64.RDX, amd64.RDX)
	c.a.JCC(amd64.NE, lFloat)
	c.a.NEGQ(amd64.RAX)
	c.storeRegTagImm(w.A(), amd64.RAX, int32(tagInt))
	c.a.JMP(lDone)
	c.a.Bind(lFloat)
	c.a.CMPQImm(amd64.RDX, int32(tagFloat))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVImm64(amd64.RCX, 1<<63)
	c.a.XORQ(amd64.RAX, amd64.RCX)
	c.storeRegTagImm(w.A(), amd64.RAX, int32(tagFloat))
	c.a.Bind(lDone)
}

func (c *x64Baseline) emitLen(ipc int, w bytecode.Instruction) {
	c.loadReg(w.B(), amd64.RAX, amd64.RDX)
	lStr := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.CMPQImm(amd64.RDX, int32(tagTable))
	c.a.JCC(amd64.NE, lStr)
	c.emitResolveSmall(amd64.RAX, amd64.RCX, amd64.RSI)
	c.a.CMPLMemImm(amd64.BaseDisp(amd64.RCX, jitTblMetaRef), 0)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.CMPLMemImm(amd64.BaseDisp(amd64.RCX, jitTblHashRef), 0)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVLLoad(amd64.RAX, amd64.BaseDisp(amd64.RCX, jitTblArrLen))
	lStoreLen := c.a.NewLabel()
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.RCX, jitTblArrKind), uint8(arrGeneric))
	c.a.JCC(amd64.NE, lStoreLen)
	// For arrGeneric with hashRef==0: #t == arrLen iff arrLen == 0 || !arr[arrLen-1].IsNil().
	c.a.TESTQ(amd64.RAX, amd64.RAX)
	c.a.JCC(amd64.E, lStoreLen)
	c.a.MOVLLoad(amd64.RDX, amd64.BaseDisp(amd64.RCX, jitTblArrRef))
	c.a.TESTL(amd64.RDX, amd64.RDX)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.emitResolveFast(amd64.RDX, amd64.RCX, amd64.RSI)
	c.a.LEAQ(amd64.RSI, amd64.BaseDisp(amd64.RAX, -1))
	c.a.SHLQImm(amd64.RSI, 4)
	c.a.CMPQMemImm(amd64.Indexed(amd64.RCX, amd64.RSI, 1, jitValTag), int32(tagNil))
	c.a.JCC(amd64.E, c.cold(ipc))
	c.a.Bind(lStoreLen)
	c.storeRegTagImm(w.A(), amd64.RAX, int32(tagInt))
	c.a.JMP(lDone)

	c.a.Bind(lStr)
	c.a.CMPQImm(amd64.RDX, int32(tagString))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.emitResolveFast(amd64.RAX, amd64.RCX, amd64.RSI)
	c.a.MOVLLoad(amd64.RAX, amd64.BaseDisp(amd64.RCX, jitStrLen))
	c.storeRegTagImm(w.A(), amd64.RAX, int32(tagInt))
	c.a.Bind(lDone)
}

// --- Comparisons & Tests ---

func (c *x64Baseline) emitFusedFPEq(ipc int, w bytecode.Instruction) {
	// XMM0 == XMM1 (NaN-safe: UCOMISD sets PF=1 on NaN; ordered equal iff PF==0 && ZF==1).
	if ipc+1 >= len(c.code) || c.code[ipc+1].Op() != bytecode.OP_JMP {
		c.a.JMP(c.cold(ipc))
		return
	}
	sj := c.code[ipc+1].SJ()
	jumpTarget := ipc + 2 + sj
	skipTarget := ipc + 2
	if jumpTarget < 0 || jumpTarget > len(c.code) {
		c.a.JMP(c.cold(ipc))
		return
	}
	c.a.UCOMISD(amd64.XMM0, amd64.XMM1)
	if w.K() {
		// Jump when equal (PF==0 && ZF==1); skip on PF==1 or ZF==0.
		c.a.JCC(amd64.P, c.pcLabels[skipTarget])
		c.a.JCC(amd64.NE, c.pcLabels[skipTarget])
		c.emitJumpToPC(jumpTarget, sj < 0)
	} else {
		// Jump when NOT equal (PF==1 || ZF==0); skip on PF==0 && ZF==1.
		lJump := c.a.NewLabel()
		c.a.JCC(amd64.P, lJump)
		c.a.JCC(amd64.NE, lJump)
		c.a.JMP(c.pcLabels[skipTarget])
		c.a.Bind(lJump)
		c.emitJumpToPC(jumpTarget, sj < 0)
	}
}

func (c *x64Baseline) emitEQ(ipc int, w bytecode.Instruction) {
	c.loadReg(w.A(), amd64.RAX, amd64.R8)
	c.loadReg(w.B(), amd64.RCX, amd64.R9)
	lNotBothInt := c.a.NewLabel()
	lFloat := c.a.NewLabel()
	c.a.MOVQ(amd64.R10, amd64.R8)
	c.a.ORQ(amd64.R10, amd64.R9)
	c.a.JCC(amd64.NE, lNotBothInt)
	c.a.CMPQ(amd64.RAX, amd64.RCX)
	c.emitFusedCond(ipc, w, amd64.E)

	c.a.Bind(lNotBothInt)
	c.a.CMPQImm(amd64.R8, int32(tagFloat))
	c.a.JCC(amd64.E, lFloat)
	c.a.CMPQImm(amd64.R9, int32(tagFloat))
	c.a.JCC(amd64.E, c.cold(ipc))
	lSameTag := c.a.NewLabel()
	c.a.CMPQ(amd64.R8, amd64.R9)
	c.a.JCC(amd64.E, lSameTag)
	c.a.LEAQ(amd64.R10, amd64.BaseDisp(amd64.R8, -int32(tagFrozenString)))
	c.a.CMPQImm(amd64.R10, 1)
	c.a.JCC(amd64.BE, c.cold(ipc))
	c.emitFusedBool(ipc, w, false)

	c.a.Bind(lSameTag)
	lNotEqBits := c.a.NewLabel()
	c.a.CMPQ(amd64.RAX, amd64.RCX)
	c.a.JCC(amd64.NE, lNotEqBits)
	c.emitFusedBool(ipc, w, true)

	c.a.Bind(lNotEqBits)
	c.a.CMPQImm(amd64.R8, int32(tagString))
	c.a.JCC(amd64.E, c.cold(ipc))
	c.a.CMPQImm(amd64.R8, int32(tagTable))
	c.a.JCC(amd64.E, c.cold(ipc))
	c.a.CMPQImm(amd64.R8, int32(tagUserdata))
	c.a.JCC(amd64.E, c.cold(ipc))
	c.emitFusedBool(ipc, w, false)

	c.a.Bind(lFloat)
	c.a.CMPQImm(amd64.R9, int32(tagFloat))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVQToX(amd64.XMM0, amd64.RAX)
	c.a.MOVQToX(amd64.XMM1, amd64.RCX)
	c.emitFusedFPEq(ipc, w)
}

func (c *x64Baseline) emitOrd(ipc int, w bytecode.Instruction, intCond amd64.Cond, strict bool) {
	c.loadReg(w.A(), amd64.RAX, amd64.R8)
	c.loadReg(w.B(), amd64.RCX, amd64.R9)
	lNotBothInt := c.a.NewLabel()
	c.a.MOVQ(amd64.R10, amd64.R8)
	c.a.ORQ(amd64.R10, amd64.R9)
	c.a.JCC(amd64.NE, lNotBothInt)
	c.a.CMPQ(amd64.RAX, amd64.RCX)
	c.emitFusedCond(ipc, w, intCond)

	c.a.Bind(lNotBothInt)
	c.a.CMPQImm(amd64.R8, int32(tagFloat))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.CMPQImm(amd64.R9, int32(tagFloat))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVQToX(amd64.XMM0, amd64.RAX)
	c.a.MOVQToX(amd64.XMM1, amd64.RCX)
	// NaN-safe lhs < rhs <=> rhs > lhs (UCOMISD rhs, lhs -> A); lhs <= rhs <=> rhs >= lhs (AE)!
	c.a.UCOMISD(amd64.XMM1, amd64.XMM0)
	if strict {
		c.emitFusedCond(ipc, w, amd64.A)
	} else {
		c.emitFusedCond(ipc, w, amd64.AE)
	}
}

func (c *x64Baseline) emitEQK(ipc int, w bytecode.Instruction) {
	kc := c.bp.proto.Consts[w.B()]
	c.loadReg(w.A(), amd64.RAX, amd64.R8)
	switch kc.Kind {
	case bytecode.ConstNil:
		c.a.CMPQImm(amd64.R8, int32(tagNil))
		c.emitFusedCond(ipc, w, amd64.E)
	case bytecode.ConstFalse:
		c.a.CMPQImm(amd64.R8, int32(tagFalse))
		c.emitFusedCond(ipc, w, amd64.E)
	case bytecode.ConstTrue:
		c.a.CMPQImm(amd64.R8, int32(tagTrue))
		c.emitFusedCond(ipc, w, amd64.E)
	case bytecode.ConstInt:
		lFloat := c.a.NewLabel()
		c.a.TESTQ(amd64.R8, amd64.R8)
		c.a.JCC(amd64.NE, lFloat)
		c.a.MOVImm64(amd64.RCX, uint64(kc.Int))
		c.a.CMPQ(amd64.RAX, amd64.RCX)
		c.emitFusedCond(ipc, w, amd64.E)
		c.a.Bind(lFloat)
		c.a.CMPQImm(amd64.R8, int32(tagFloat))
		c.a.JCC(amd64.E, c.cold(ipc))
		c.emitFusedBool(ipc, w, false)
	case bytecode.ConstFloat:
		lNotFloat := c.a.NewLabel()
		c.a.CMPQImm(amd64.R8, int32(tagFloat))
		c.a.JCC(amd64.NE, lNotFloat)
		c.a.MOVQToX(amd64.XMM0, amd64.RAX)
		c.a.MOVImm64(amd64.RCX, math.Float64bits(kc.Float))
		c.a.MOVQToX(amd64.XMM1, amd64.RCX)
		c.emitFusedFPEq(ipc, w)
		c.a.Bind(lNotFloat)
		c.a.TESTQ(amd64.R8, amd64.R8)
		c.a.JCC(amd64.E, c.cold(ipc))
		c.emitFusedBool(ipc, w, false)
	default:
		c.loadConst(w.B(), amd64.RCX, amd64.R9, amd64.RSI)
		lSameTag := c.a.NewLabel()
		c.a.CMPQ(amd64.R8, amd64.R9)
		c.a.JCC(amd64.E, lSameTag)
		c.a.LEAQ(amd64.R10, amd64.BaseDisp(amd64.R8, -int32(tagFrozenString)))
		c.a.CMPQImm(amd64.R10, 1)
		c.a.JCC(amd64.BE, c.cold(ipc))
		c.emitFusedBool(ipc, w, false)
		c.a.Bind(lSameTag)
		lDiffBits := c.a.NewLabel()
		c.a.CMPQ(amd64.RAX, amd64.RCX)
		c.a.JCC(amd64.NE, lDiffBits)
		c.emitFusedBool(ipc, w, true)
		c.a.Bind(lDiffBits)
		c.a.CMPQImm(amd64.R8, int32(tagString))
		c.a.JCC(amd64.NE, c.cold(ipc))
		// Large-span strings (>32 KiB) are never short strings and must exit before small-slab deref.
		c.a.TESTQImm(amd64.RAX, -1<<31)
		c.a.JCC(amd64.NE, c.cold(ipc))
		c.a.TESTQImm(amd64.RCX, -1<<31)
		c.a.JCC(amd64.NE, c.cold(ipc))
		c.emitResolveSmall(amd64.RAX, amd64.RSI, amd64.RDI)
		c.a.CMPBMemImm(amd64.BaseDisp(amd64.RSI, jitStrKind), uint8(gcKindShortStr))
		c.a.JCC(amd64.NE, c.cold(ipc))
		c.emitResolveSmall(amd64.RCX, amd64.RSI, amd64.RDI)
		c.a.CMPBMemImm(amd64.BaseDisp(amd64.RSI, jitStrKind), uint8(gcKindShortStr))
		c.a.JCC(amd64.NE, c.cold(ipc))
		c.emitFusedBool(ipc, w, false)
	}
}

func (c *x64Baseline) emitEQI(ipc int, w bytecode.Instruction) {
	im := int32(w.SB())
	c.loadReg(w.A(), amd64.RAX, amd64.R8)
	lFloat := c.a.NewLabel()
	c.a.TESTQ(amd64.R8, amd64.R8)
	c.a.JCC(amd64.NE, lFloat)
	c.a.CMPQImm(amd64.RAX, im)
	c.emitFusedCond(ipc, w, amd64.E)

	c.a.Bind(lFloat)
	lNonNum := c.a.NewLabel()
	c.a.CMPQImm(amd64.R8, int32(tagFloat))
	c.a.JCC(amd64.NE, lNonNum)
	c.a.MOVQToX(amd64.XMM0, amd64.RAX)
	c.a.MOVImm64(amd64.RCX, math.Float64bits(float64(im)))
	c.a.MOVQToX(amd64.XMM1, amd64.RCX)
	c.emitFusedFPEq(ipc, w)

	c.a.Bind(lNonNum)
	c.emitFusedBool(ipc, w, false)
}

func (c *x64Baseline) emitOrdI(ipc int, w bytecode.Instruction, intCond amd64.Cond, strict, greater bool) {
	im := int32(w.SB())
	c.loadReg(w.A(), amd64.RAX, amd64.R8)
	lFloat := c.a.NewLabel()
	c.a.TESTQ(amd64.R8, amd64.R8)
	c.a.JCC(amd64.NE, lFloat)
	c.a.CMPQImm(amd64.RAX, im)
	c.emitFusedCond(ipc, w, intCond)

	c.a.Bind(lFloat)
	c.a.CMPQImm(amd64.R8, int32(tagFloat))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVQToX(amd64.XMM0, amd64.RAX)
	c.a.MOVImm64(amd64.RCX, math.Float64bits(float64(im)))
	c.a.MOVQToX(amd64.XMM1, amd64.RCX)
	if greater {
		c.a.UCOMISD(amd64.XMM0, amd64.XMM1)
	} else {
		c.a.UCOMISD(amd64.XMM1, amd64.XMM0)
	}
	if strict {
		c.emitFusedCond(ipc, w, amd64.A)
	} else {
		c.emitFusedCond(ipc, w, amd64.AE)
	}
}

func (c *x64Baseline) emitTest(ipc int, w bytecode.Instruction) {
	c.a.MOVQLoad(amd64.RAX, c.regTag(w.A()))
	c.a.SUBQImm(amd64.RAX, int32(tagNil))
	c.a.CMPQImm(amd64.RAX, 1) // A => truthy
	c.emitFusedCond(ipc, w, amd64.A)
}

func (c *x64Baseline) emitTestSet(ipc int, w bytecode.Instruction) {
	if ipc+1 >= len(c.code) || c.code[ipc+1].Op() != bytecode.OP_JMP {
		c.a.JMP(c.cold(ipc))
		return
	}
	sj := c.code[ipc+1].SJ()
	jumpTarget := ipc + 2 + sj
	skipTarget := ipc + 2
	if jumpTarget < 0 || jumpTarget > len(c.code) {
		c.a.JMP(c.cold(ipc))
		return
	}
	c.loadReg(w.B(), amd64.RAX, amd64.RDX)
	c.a.LEAQ(amd64.RCX, amd64.BaseDisp(amd64.RDX, -int32(tagNil)))
	c.a.CMPQImm(amd64.RCX, 1) // BE => falsy, A => truthy
	if w.K() {
		c.a.JCC(amd64.BE, c.pcLabels[skipTarget])
	} else {
		c.a.JCC(amd64.A, c.pcLabels[skipTarget])
	}
	c.storeReg(w.A(), amd64.RAX, amd64.RDX)
	c.emitJumpToPC(jumpTarget, sj < 0)
}

// --- Numeric FOR Loops ---

func (c *x64Baseline) emitForPrep(ipc int, w bytecode.Instruction) {
	a := w.A()
	skipTarget := ipc + 1 + w.Bx() + 1
	if skipTarget < 0 || skipTarget > len(c.code) {
		c.a.JMP(c.cold(ipc))
		return
	}
	c.loadReg(a, amd64.R8, amd64.RAX)    // init
	c.loadReg(a+1, amd64.R9, amd64.RCX)  // lim
	c.loadReg(a+2, amd64.R10, amd64.RDX) // step
	c.a.ORQ(amd64.RAX, amd64.RCX)
	c.a.ORQ(amd64.RAX, amd64.RDX)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.TESTQ(amd64.R10, amd64.R10)
	c.a.JCC(amd64.E, c.cold(ipc))

	lNegStep := c.a.NewLabel()
	lStepNot1 := c.a.NewLabel()
	lStore := c.a.NewLabel()
	c.a.JCC(amd64.L, lNegStep)
	// step > 0
	c.a.CMPQ(amd64.R8, amd64.R9)
	c.a.JCC(amd64.G, c.pcLabels[skipTarget])
	c.a.MOVQ(amd64.RAX, amd64.R9)
	c.a.SUBQ(amd64.RAX, amd64.R8)
	c.a.CMPQImm(amd64.R10, 1)
	c.a.JCC(amd64.NE, lStepNot1)
	c.a.JMP(lStore)
	c.a.Bind(lStepNot1)
	c.a.XORL(amd64.RDX, amd64.RDX)
	c.a.DIVQ(amd64.R10)
	c.a.JMP(lStore)

	c.a.Bind(lNegStep)
	// step < 0
	c.a.CMPQ(amd64.R8, amd64.R9)
	c.a.JCC(amd64.L, c.pcLabels[skipTarget])
	c.a.MOVQ(amd64.RAX, amd64.R8)
	c.a.SUBQ(amd64.RAX, amd64.R9)
	c.a.LEAQ(amd64.RCX, amd64.BaseDisp(amd64.R10, 1))
	c.a.NEGQ(amd64.RCX)
	c.a.ADDQImm(amd64.RCX, 1) // uint64(-(step+1)) + 1 (MinInt64-safe)
	c.a.XORL(amd64.RDX, amd64.RDX)
	c.a.DIVQ(amd64.RCX)

	c.a.Bind(lStore)
	c.storeRegTagImm(a, amd64.RAX, int32(tagInt))
	c.storeRegTagImm(a+1, amd64.R10, int32(tagInt))
	c.storeRegTagImm(a+2, amd64.R8, int32(tagInt))
}

func (c *x64Baseline) emitForLoop(ipc int, w bytecode.Instruction) {
	a := w.A()
	bodyTarget := ipc + 1 - w.Bx()
	if bodyTarget < 0 || bodyTarget >= len(c.code) {
		c.a.JMP(c.cold(ipc))
		return
	}
	c.loadReg(a+1, amd64.RCX, amd64.R9) // step
	lFloat := c.a.NewLabel()
	lDone := c.pcLabels[ipc+1]
	c.a.TESTQ(amd64.R9, amd64.R9)
	c.a.JCC(amd64.NE, lFloat)

	// Integer FORLOOP
	c.loadReg(a, amd64.RAX, amd64.R8)    // cnt
	c.loadReg(a+2, amd64.RDX, amd64.R10) // idx
	c.a.ORQ(amd64.R8, amd64.R10)
	c.a.JCC(amd64.NE, lDone)
	c.a.TESTQ(amd64.RAX, amd64.RAX)
	c.a.JCC(amd64.E, lDone)
	c.a.SUBQImm(amd64.RAX, 1)
	c.a.ADDQ(amd64.RDX, amd64.RCX)
	c.a.MOVQStore(c.regBits(a), amd64.RAX)
	c.a.MOVQStore(c.regBits(a+2), amd64.RDX)
	c.a.SUBQImm(x64Poll, 1)
	c.a.JCC(amd64.LE, c.poll(bodyTarget))
	if ref, ok := c.optByEnd[ipc]; ok {
		c.a.JMP(ref.resumeLbl)
	} else {
		c.a.JMP(c.pcLabels[bodyTarget])
	}

	// Float FORLOOP
	c.a.Bind(lFloat)
	c.a.CMPQImm(amd64.R9, int32(tagFloat))
	c.a.JCC(amd64.NE, lDone)
	c.a.CMPQMemImm(c.regTag(a), int32(tagFloat))
	c.a.JCC(amd64.NE, lDone)
	c.a.CMPQMemImm(c.regTag(a+2), int32(tagFloat))
	c.a.JCC(amd64.NE, lDone)
	c.a.MOVSDLoad(amd64.XMM0, c.regBits(a))   // lim
	c.a.MOVQToX(amd64.XMM1, amd64.RCX)        // step
	c.a.MOVSDLoad(amd64.XMM2, c.regBits(a+2)) // idx
	c.a.ADDSD(amd64.XMM2, amd64.XMM1)
	c.a.XORPD(amd64.XMM3, amd64.XMM3) // 0.0
	lNeg := c.a.NewLabel()
	lCont := c.a.NewLabel()
	c.a.UCOMISD(amd64.XMM1, amd64.XMM3) // step > 0 => A
	c.a.JCC(amd64.BE, lNeg)
	// step > 0: continue if idx <= lim <=> lim >= idx (AE)
	c.a.UCOMISD(amd64.XMM0, amd64.XMM2)
	c.a.JCC(amd64.AE, lCont)
	c.a.JMP(lDone)
	c.a.Bind(lNeg)
	// step <= 0: continue if idx >= lim (AE)
	c.a.UCOMISD(amd64.XMM2, amd64.XMM0)
	c.a.JCC(amd64.AE, lCont)
	c.a.JMP(lDone)
	c.a.Bind(lCont)
	c.a.MOVSDStore(c.regBits(a+2), amd64.XMM2)
	c.a.SUBQImm(x64Poll, 1)
	c.a.JCC(amd64.LE, c.poll(bodyTarget))
	c.a.JMP(c.pcLabels[bodyTarget])
}

// --- Table Array Indexing ---

// emitTableGetIdx fetches t[idx] (tblReg=RSI, idxReg=RDI) into R[a].
func (c *x64Baseline) emitTableGetIdx(ipc, a int) {
	const (
		tblReg = amd64.RSI
		idxReg = amd64.RDI
	)
	c.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(tblReg, jitTblArrLen))
	c.a.CMPQ(idxReg, amd64.RCX)
	c.a.JCC(amd64.AE, c.cold(ipc))
	c.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(tblReg, jitTblArrRef))
	c.emitResolveFast(amd64.RCX, amd64.R8, amd64.R9) // R8 = arr base
	c.a.MOVZXBL(amd64.R9, amd64.BaseDisp(tblReg, jitTblArrKind))

	lFloat := c.a.NewLabel()
	lBool := c.a.NewLabel()
	lGen := c.a.NewLabel()
	lDone := c.a.NewLabel()

	c.a.CMPLImm(amd64.R9, int32(arrInt))
	c.a.JCC(amd64.NE, lFloat)
	c.a.MOVQLoad(amd64.RAX, amd64.Indexed(amd64.R8, idxReg, 8, 0))
	c.storeRegTagImm(a, amd64.RAX, int32(tagInt))
	c.a.JMP(lDone)

	c.a.Bind(lFloat)
	c.a.CMPLImm(amd64.R9, int32(arrFloat))
	c.a.JCC(amd64.NE, lBool)
	c.a.MOVQLoad(amd64.RAX, amd64.Indexed(amd64.R8, idxReg, 8, 0))
	c.storeRegTagImm(a, amd64.RAX, int32(tagFloat))
	c.a.JMP(lDone)

	c.a.Bind(lBool)
	c.a.CMPLImm(amd64.R9, int32(arrBool))
	c.a.JCC(amd64.NE, lGen)
	c.a.MOVZXBL(amd64.RDX, amd64.Indexed(amd64.R8, idxReg, 1, 0))
	c.a.ADDQImm(amd64.RDX, int32(tagFalse))
	c.a.MOVQStoreImm32(c.regBits(a), 0)
	c.a.MOVQStore(c.regTag(a), amd64.RDX)
	c.a.JMP(lDone)

	c.a.Bind(lGen)
	c.a.CMPLImm(amd64.R9, int32(arrGeneric))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.SHLQImm(idxReg, 4)
	c.a.ADDQ(amd64.R8, idxReg)
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.R8, 0))
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.R8, 8))
	lStoreGen := c.a.NewLabel()
	c.a.CMPQImm(amd64.RDX, int32(tagNil))
	c.a.JCC(amd64.NE, lStoreGen)
	c.a.CMPLMemImm(amd64.BaseDisp(tblReg, jitTblMetaRef), 0)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.Bind(lStoreGen)
	c.storeReg(a, amd64.RAX, amd64.RDX)
	c.a.Bind(lDone)
}

func (c *x64Baseline) emitGetI(ipc int, w bytecode.Instruction) {
	key := w.C()
	if key == 0 {
		c.a.JMP(c.cold(ipc))
		return
	}
	c.loadReg(w.B(), amd64.RAX, amd64.RDX)
	c.a.CMPQImm(amd64.RDX, int32(tagTable))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.emitResolveSmall(amd64.RAX, amd64.RSI, amd64.RCX)
	c.a.MOVImm32(amd64.RDI, uint32(key-1))
	c.emitTableGetIdx(ipc, w.A())
}

func (c *x64Baseline) emitGetTable(ipc int, w bytecode.Instruction) {
	c.loadReg(w.B(), amd64.RAX, amd64.RDX)
	c.a.CMPQImm(amd64.RDX, int32(tagTable))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.loadReg(w.C(), amd64.RDI, amd64.RCX)
	c.a.TESTQ(amd64.RCX, amd64.RCX)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.emitResolveSmall(amd64.RAX, amd64.RSI, amd64.RCX)
	c.a.SUBQImm(amd64.RDI, 1)
	c.emitTableGetIdx(ipc, w.A())
}

// emitTableSetIdx stores (valBits=RAX, valTag=RDX) into t[idx] (tblReg=RSI, idxReg=RDI).
func (c *x64Baseline) emitTableSetIdx(ipc int, allowAppend bool) {
	const (
		valBits = amd64.RAX
		valTag  = amd64.RDX
		tblReg  = amd64.RSI
		idxReg  = amd64.RDI
	)
	c.a.MOVZXBL(amd64.RCX, amd64.BaseDisp(tblReg, jitTblFlags))
	c.a.TESTQImm(amd64.RCX, 1)
	c.a.JCC(amd64.NE, c.cold(ipc))

	c.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(tblReg, jitTblArrLen))
	lInBounds := c.a.NewLabel()
	c.a.CMPQ(idxReg, amd64.RCX)
	c.a.JCC(amd64.B, lInBounds)
	if !allowAppend {
		c.a.JMP(c.cold(ipc))
	} else {
		c.a.JCC(amd64.NE, c.cold(ipc))
		c.a.MOVLLoad(amd64.R8, amd64.BaseDisp(tblReg, jitTblArrCap))
		c.a.CMPQ(amd64.RCX, amd64.R8)
		c.a.JCC(amd64.AE, c.cold(ipc))
		c.a.CMPLMemImm(amd64.BaseDisp(tblReg, jitTblHashRef), 0)
		c.a.JCC(amd64.NE, c.cold(ipc))
		c.a.CMPLMemImm(amd64.BaseDisp(tblReg, jitTblMetaRef), 0)
		c.a.JCC(amd64.NE, c.cold(ipc))

		c.a.MOVZXBL(amd64.R9, amd64.BaseDisp(tblReg, jitTblArrKind))
		lAppFloat := c.a.NewLabel()
		lAppBool := c.a.NewLabel()
		lAppGen := c.a.NewLabel()
		lAppOK := c.a.NewLabel()

		c.a.CMPLImm(amd64.R9, int32(arrInt))
		c.a.JCC(amd64.NE, lAppFloat)
		c.a.TESTQ(valTag, valTag)
		c.a.JCC(amd64.NE, c.cold(ipc))
		c.a.JMP(lAppOK)

		c.a.Bind(lAppFloat)
		c.a.CMPLImm(amd64.R9, int32(arrFloat))
		c.a.JCC(amd64.NE, lAppBool)
		c.a.CMPQImm(valTag, int32(tagFloat))
		c.a.JCC(amd64.NE, c.cold(ipc))
		c.a.JMP(lAppOK)

		c.a.Bind(lAppBool)
		c.a.CMPLImm(amd64.R9, int32(arrBool))
		c.a.JCC(amd64.NE, lAppGen)
		c.a.LEAQ(amd64.R10, amd64.BaseDisp(valTag, -int32(tagFalse)))
		c.a.CMPQImm(amd64.R10, 1)
		c.a.JCC(amd64.A, c.cold(ipc))
		c.a.JMP(lAppOK)

		c.a.Bind(lAppGen)
		c.a.CMPLImm(amd64.R9, int32(arrGeneric))
		c.a.JCC(amd64.NE, c.cold(ipc))
		c.a.CMPQImm(valTag, int32(tagNil))
		c.a.JCC(amd64.E, c.cold(ipc))
		lAppGenBarrierOK := c.a.NewLabel()
		c.a.LEAQ(amd64.R10, amd64.BaseDisp(valTag, -int32(tagFirstGC)))
		c.a.CMPQImm(amd64.R10, int32(tagLastGC-tagFirstGC))
		c.a.JCC(amd64.A, lAppGenBarrierOK)
		c.a.MOVZXBL(amd64.R10, amd64.BaseDisp(tblReg, jitTblMark))
		c.a.TESTQImm(amd64.R10, 1<<jitMarkBlackBit)
		c.a.JCC(amd64.NE, c.cold(ipc))
		c.a.Bind(lAppGenBarrierOK)

		c.a.Bind(lAppOK)
		c.a.ADDLImm(amd64.RCX, 1)
		c.a.MOVLStore(amd64.BaseDisp(tblReg, jitTblArrLen), amd64.RCX)
	}

	c.a.Bind(lInBounds)
	c.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(tblReg, jitTblArrRef))
	c.emitResolveFast(amd64.RCX, amd64.R8, amd64.R9) // R8 = arr base
	c.a.MOVZXBL(amd64.R9, amd64.BaseDisp(tblReg, jitTblArrKind))

	lFloat := c.a.NewLabel()
	lBool := c.a.NewLabel()
	lGen := c.a.NewLabel()
	lDone := c.a.NewLabel()

	c.a.CMPLImm(amd64.R9, int32(arrInt))
	c.a.JCC(amd64.NE, lFloat)
	c.a.TESTQ(valTag, valTag)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVQStore(amd64.Indexed(amd64.R8, idxReg, 8, 0), valBits)
	c.a.JMP(lDone)

	c.a.Bind(lFloat)
	c.a.CMPLImm(amd64.R9, int32(arrFloat))
	c.a.JCC(amd64.NE, lBool)
	c.a.CMPQImm(valTag, int32(tagFloat))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVQStore(amd64.Indexed(amd64.R8, idxReg, 8, 0), valBits)
	c.a.JMP(lDone)

	c.a.Bind(lBool)
	c.a.CMPLImm(amd64.R9, int32(arrBool))
	c.a.JCC(amd64.NE, lGen)
	c.a.LEAQ(amd64.R10, amd64.BaseDisp(valTag, -int32(tagFalse)))
	c.a.CMPQImm(amd64.R10, 1)
	c.a.JCC(amd64.A, c.cold(ipc))
	c.a.MOVBStore(amd64.Indexed(amd64.R8, idxReg, 1, 0), amd64.R10)
	c.a.JMP(lDone)

	c.a.Bind(lGen)
	c.a.CMPLImm(amd64.R9, int32(arrGeneric))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.SHLQImm(idxReg, 4)
	c.a.ADDQ(amd64.R8, idxReg)
	lMetaOK := c.a.NewLabel()
	c.a.CMPQMemImm(amd64.BaseDisp(amd64.R8, jitValTag), int32(tagNil))
	c.a.JCC(amd64.NE, lMetaOK)
	c.a.CMPLMemImm(amd64.BaseDisp(tblReg, jitTblMetaRef), 0)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.Bind(lMetaOK)
	lBarrierOK := c.a.NewLabel()
	c.a.LEAQ(amd64.R10, amd64.BaseDisp(valTag, -int32(tagFirstGC)))
	c.a.CMPQImm(amd64.R10, int32(tagLastGC-tagFirstGC))
	c.a.JCC(amd64.A, lBarrierOK)
	c.a.MOVZXBL(amd64.R10, amd64.BaseDisp(tblReg, jitTblMark))
	c.a.TESTQImm(amd64.R10, 1<<jitMarkBlackBit)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.Bind(lBarrierOK)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R8, 0), valBits)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R8, 8), valTag)
	c.a.Bind(lDone)
}

func (c *x64Baseline) emitSetI(ipc int, w bytecode.Instruction) {
	key := w.B()
	if key == 0 {
		c.a.JMP(c.cold(ipc))
		return
	}
	c.loadReg(w.A(), amd64.R8, amd64.R9)
	c.a.CMPQImm(amd64.R9, int32(tagTable))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.emitResolveSmall(amd64.R8, amd64.RSI, amd64.RCX)
	c.loadRegOrConst(w.K(), w.C(), amd64.RAX, amd64.RDX, amd64.RCX)
	c.a.MOVImm32(amd64.RDI, uint32(key-1))
	c.emitTableSetIdx(ipc, false)
}

func (c *x64Baseline) emitSetTable(ipc int, w bytecode.Instruction) {
	c.loadReg(w.A(), amd64.R8, amd64.R9)
	c.a.CMPQImm(amd64.R9, int32(tagTable))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.loadReg(w.B(), amd64.RDI, amd64.RCX)
	c.a.TESTQ(amd64.RCX, amd64.RCX)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.emitResolveSmall(amd64.R8, amd64.RSI, amd64.RCX)
	c.loadRegOrConst(w.K(), w.C(), amd64.RAX, amd64.RDX, amd64.RCX)
	c.a.SUBQImm(amd64.RDI, 1)
	c.emitTableSetIdx(ipc, true)
}

// --- Inline Cache Field & Global Access ---

// emitTableSlotAddr computes &t.slot(slotReg) into dstReg (clobbering tmpReg).
// Matches Table.slotPtr: inlineSlots when slotsRef == 0, else PtrSmall(slotsRef) + slot*16.
func (c *x64Baseline) emitTableSlotAddr(tblReg, slotReg, dstReg, tmpReg amd64.Reg) {
	lExt := c.a.NewLabel()
	lDone := c.a.NewLabel()
	c.a.MOVLLoad(dstReg, amd64.BaseDisp(tblReg, jitTblSlotsRef))
	c.a.TESTL(dstReg, dstReg)
	c.a.JCC(amd64.NE, lExt)
	c.a.MOVQ(tmpReg, slotReg)
	c.a.SHLQImm(tmpReg, 4)
	c.a.LEAQ(dstReg, amd64.Indexed(tblReg, tmpReg, 1, jitTblInlineSlots))
	c.a.JMP(lDone)
	c.a.Bind(lExt)
	c.emitResolveSmall(dstReg, dstReg, tmpReg)
	c.a.MOVQ(tmpReg, slotReg)
	c.a.SHLQImm(tmpReg, 4)
	c.a.ADDQ(dstReg, tmpReg)
	c.a.Bind(lDone)
}

func (c *x64Baseline) loadICPtr(ipc int, dst amd64.Reg) bool {
	if ipc >= len(c.bp.icSlots) || c.bp.icSlots[ipc] < 0 {
		return false
	}
	c.a.MOVQLoad(dst, amd64.BaseDisp(x64Ctx, jitCtxIC))
	if off := c.bp.icSlots[ipc] * jitICSize; off != 0 {
		c.a.ADDQImm(dst, off)
	}
	return true
}

func (c *x64Baseline) emitGetField(ipc int, w bytecode.Instruction, isSelf bool) {
	a := w.A()
	c.loadReg(w.B(), amd64.RAX, amd64.RDX)
	if isSelf {
		c.storeReg(a+1, amd64.RAX, amd64.RDX)
	}
	if !c.loadICPtr(ipc, amd64.R8) {
		c.a.JMP(c.cold(ipc))
		return
	}
	c.a.CMPQImm(amd64.RDX, int32(tagTable))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.emitResolveSmall(amd64.RAX, amd64.RSI, amd64.RCX) // RSI = *Table
	c.a.MOVLLoad(amd64.R9, amd64.BaseDisp(amd64.RSI, jitTblShapeRef))
	c.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(amd64.R8, jitICShapeRef))

	lPoly := c.a.NewLabel()
	lProto := c.a.NewLabel()
	lStore := c.a.NewLabel()

	c.a.CMPL(amd64.R9, amd64.RCX)
	c.a.JCC(amd64.NE, lPoly)
	c.a.MOVZXBL(amd64.RCX, amd64.BaseDisp(amd64.R8, jitICKind))
	c.a.CMPLImm(amd64.RCX, int32(icOwnSlot))
	c.a.JCC(amd64.NE, lProto)

	c.a.MOVLLoad(amd64.RDI, amd64.BaseDisp(amd64.R8, jitICSlot))
	c.emitTableSlotAddr(amd64.RSI, amd64.RDI, amd64.R10, amd64.R11)
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.R10, 0))
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.R10, 8))
	c.a.CMPQImm(amd64.RDX, int32(tagNil))
	c.a.JCC(amd64.NE, lStore)
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitICNoMeta), 0)
	c.a.JCC(amd64.NE, lStore)
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitICProtoAbsent), 0)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.a.MOVQLoad(amd64.RCX, amd64.BaseDisp(amd64.R8, jitICEpoch))
	c.a.CMPQMem(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxProtoEpoch))
	c.a.JCC(amd64.E, lStore)
	c.a.JMP(c.cold(ipc))

	c.a.Bind(lProto)
	c.a.CMPLImm(amd64.RCX, int32(icProtoVal))
	c.a.JCC(amd64.NE, lPoly)
	lProtoHit := c.a.NewLabel()
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitICNoMeta), 0)
	c.a.JCC(amd64.NE, lProtoHit)
	c.a.MOVQLoad(amd64.RCX, amd64.BaseDisp(amd64.R8, jitICEpoch))
	c.a.CMPQMem(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxProtoEpoch))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.Bind(lProtoHit)
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.R8, jitICVal))
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.R8, jitICVal+8))
	c.a.JMP(lStore)

	// Polymorphic IC loop
	c.a.Bind(lPoly)
	c.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(amd64.R8, jitICPolyRef))
	c.a.TESTL(amd64.RCX, amd64.RCX)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.a.MOVZXBL(amd64.R11, amd64.BaseDisp(amd64.R8, jitICPolyCount))
	c.a.TESTL(amd64.R11, amd64.R11)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.emitResolveSmall(amd64.RCX, amd64.R8, amd64.RDI) // R8 = &poly[0]
	lPolyLoop := c.a.NewLabel()
	lPolyNext := c.a.NewLabel()
	lPolyProto := c.a.NewLabel()
	c.a.Bind(lPolyLoop)
	c.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(amd64.R8, jitPolyShapeRef))
	c.a.CMPL(amd64.R9, amd64.RCX)
	c.a.JCC(amd64.NE, lPolyNext)
	c.a.MOVZXBL(amd64.RCX, amd64.BaseDisp(amd64.R8, jitPolyKind))
	c.a.CMPLImm(amd64.RCX, int32(icOwnSlot))
	c.a.JCC(amd64.NE, lPolyProto)
	c.a.MOVLLoad(amd64.RDI, amd64.BaseDisp(amd64.R8, jitPolySlot))
	c.emitTableSlotAddr(amd64.RSI, amd64.RDI, amd64.R10, amd64.RCX)
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.R10, 0))
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.R10, 8))
	c.a.CMPQImm(amd64.RDX, int32(tagNil))
	c.a.JCC(amd64.NE, lStore)
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitPolyNoMeta), 0)
	c.a.JCC(amd64.NE, lStore)
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitPolyProtoAbsent), 0)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.a.MOVQLoad(amd64.RCX, amd64.BaseDisp(amd64.R8, jitPolyEpoch))
	c.a.CMPQMem(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxProtoEpoch))
	c.a.JCC(amd64.E, lStore)
	c.a.JMP(c.cold(ipc))

	c.a.Bind(lPolyProto)
	c.a.CMPLImm(amd64.RCX, int32(icProtoVal))
	c.a.JCC(amd64.NE, c.cold(ipc))
	lPolyProtoHit := c.a.NewLabel()
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitPolyNoMeta), 0)
	c.a.JCC(amd64.NE, lPolyProtoHit)
	c.a.MOVQLoad(amd64.RCX, amd64.BaseDisp(amd64.R8, jitPolyEpoch))
	c.a.CMPQMem(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxProtoEpoch))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.Bind(lPolyProtoHit)
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.R8, jitPolyVal))
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.R8, jitPolyVal+8))
	c.a.JMP(lStore)

	c.a.Bind(lPolyNext)
	c.a.ADDQImm(amd64.R8, jitPolySize)
	c.a.SUBLImm(amd64.R11, 1)
	c.a.JCC(amd64.NE, lPolyLoop)
	c.a.JMP(c.cold(ipc))

	c.a.Bind(lStore)
	c.storeReg(a, amd64.RAX, amd64.RDX)
}

func (c *x64Baseline) emitSetField(ipc int, w bytecode.Instruction) {
	if !c.loadICPtr(ipc, amd64.R8) {
		c.a.JMP(c.cold(ipc))
		return
	}
	c.loadReg(w.A(), amd64.RAX, amd64.RDX)
	c.a.CMPQImm(amd64.RDX, int32(tagTable))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.emitResolveSmall(amd64.RAX, amd64.RSI, amd64.RCX) // RSI = *Table
	c.a.MOVZXBL(amd64.RCX, amd64.BaseDisp(amd64.RSI, jitTblFlags))
	c.a.TESTQImm(amd64.RCX, 1)
	c.a.JCC(amd64.NE, c.cold(ipc))

	c.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(amd64.RSI, jitTblShapeRef))
	c.a.MOVLLoad(amd64.R9, amd64.BaseDisp(amd64.R8, jitICShapeRef))
	c.a.CMPL(amd64.RCX, amd64.R9)
	c.a.JCC(amd64.NE, c.cold(ipc))

	c.loadRegOrConst(w.K(), w.C(), amd64.RAX, amd64.RDX, amd64.RCX)
	c.a.MOVZXBL(amd64.RCX, amd64.BaseDisp(amd64.R8, jitICKind))
	lAddSlot := c.a.NewLabel()
	lDone := c.a.NewLabel()

	c.a.CMPLImm(amd64.RCX, int32(icOwnSlot))
	c.a.JCC(amd64.NE, lAddSlot)
	c.a.MOVLLoad(amd64.RDI, amd64.BaseDisp(amd64.R8, jitICSlot))
	c.emitTableSlotAddr(amd64.RSI, amd64.RDI, amd64.R10, amd64.R11)
	lWriteOwn := c.a.NewLabel()
	c.a.CMPQMemImm(amd64.BaseDisp(amd64.R10, jitValTag), int32(tagNil))
	c.a.JCC(amd64.NE, lWriteOwn)
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitICNoMeta), 0)
	c.a.JCC(amd64.NE, lWriteOwn)
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitICProtoAbsent), 0)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.a.MOVQLoad(amd64.R11, amd64.BaseDisp(amd64.R8, jitICEpoch))
	c.a.CMPQMem(amd64.R11, amd64.BaseDisp(x64Ctx, jitCtxProtoEpoch))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.Bind(lWriteOwn)
	lBarrierOK := c.a.NewLabel()
	c.a.LEAQ(amd64.R11, amd64.BaseDisp(amd64.RDX, -int32(tagFirstGC)))
	c.a.CMPQImm(amd64.R11, int32(tagLastGC-tagFirstGC))
	c.a.JCC(amd64.A, lBarrierOK)
	c.a.MOVZXBL(amd64.R11, amd64.BaseDisp(amd64.RSI, jitTblMark))
	c.a.TESTQImm(amd64.R11, 1<<jitMarkBlackBit)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.Bind(lBarrierOK)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R10, 0), amd64.RAX)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R10, 8), amd64.RDX)
	c.a.JMP(lDone)

	// icAddSlot
	c.a.Bind(lAddSlot)
	c.a.CMPLImm(amd64.RCX, int32(icAddSlot))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.CMPQImm(amd64.RDX, int32(tagNil))
	c.a.JCC(amd64.E, c.cold(ipc))
	lAddMetaOK := c.a.NewLabel()
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitICNoMeta), 0)
	c.a.JCC(amd64.NE, lAddMetaOK)
	c.a.MOVQLoad(amd64.R11, amd64.BaseDisp(amd64.R8, jitICEpoch))
	c.a.CMPQMem(amd64.R11, amd64.BaseDisp(x64Ctx, jitCtxProtoEpoch))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.Bind(lAddMetaOK)
	c.a.MOVLLoad(amd64.R9, amd64.BaseDisp(amd64.R8, jitICTargetShape))
	c.a.TESTL(amd64.R9, amd64.R9)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.a.MOVLLoad(amd64.RDI, amd64.BaseDisp(amd64.R8, jitICSlot))
	c.a.MOVZXBL(amd64.R11, amd64.BaseDisp(amd64.RSI, jitTblSlotsCap))
	c.a.CMPL(amd64.RDI, amd64.R11)
	c.a.JCC(amd64.AE, c.cold(ipc))
	c.a.MOVZXBL(amd64.R11, amd64.BaseDisp(amd64.RSI, jitTblMark))
	c.a.TESTQImm(amd64.R11, 1<<jitMarkBlackBit)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.emitTableSlotAddr(amd64.RSI, amd64.RDI, amd64.R10, amd64.R11)
	c.a.LEAQ(amd64.R11, amd64.BaseDisp(amd64.RDI, 1))
	c.a.MOVBStore(amd64.BaseDisp(amd64.RSI, jitTblSlotsLen), amd64.R11)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R10, 0), amd64.RAX)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R10, 8), amd64.RDX)
	c.a.MOVLStore(amd64.BaseDisp(amd64.RSI, jitTblShapeRef), amd64.R9)
	c.a.Bind(lDone)
}

func (c *x64Baseline) emitGetTabUp(ipc int, w bytecode.Instruction) {
	if !c.loadICPtr(ipc, amd64.R8) {
		c.a.JMP(c.cold(ipc))
		return
	}
	c.emitLoadUpval(ipc, w.B(), amd64.RAX, amd64.RDX, amd64.RCX, amd64.RSI)
	c.a.CMPQImm(amd64.RDX, int32(tagTable))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.emitResolveSmall(amd64.RAX, amd64.RSI, amd64.RCX) // RSI = *Table

	lOwn := c.a.NewLabel()
	lStore := c.a.NewLabel()
	c.a.MOVZXBL(amd64.RCX, amd64.BaseDisp(amd64.R8, jitICKind))
	c.a.CMPLImm(amd64.RCX, int32(icEnvSlot))
	c.a.JCC(amd64.NE, lOwn)

	c.a.MOVLLoad(amd64.R9, amd64.BaseDisp(amd64.R8, jitICHolderRef))
	c.a.MOVLLoad(amd64.R10, amd64.BaseDisp(amd64.RSI, jitTblSelf))
	c.a.CMPL(amd64.R9, amd64.R10)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVLLoad(amd64.R9, amd64.BaseDisp(amd64.RSI, jitTblHashRef))
	c.a.TESTL(amd64.R9, amd64.R9)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.emitResolveSmall(amd64.R9, amd64.R9, amd64.R10) // *tableHash
	c.a.MOVQLoad(amd64.R10, amd64.BaseDisp(amd64.R9, jitThashDictGen))
	c.a.CMPQMem(amd64.R10, amd64.BaseDisp(amd64.R8, jitICEpoch))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVLLoad(amd64.R9, amd64.BaseDisp(amd64.R9, jitThashNodesRef))
	c.a.TESTL(amd64.R9, amd64.R9)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.emitResolveFast(amd64.R9, amd64.R9, amd64.R10) // *hashNode[0]
	c.a.MOVLLoad(amd64.R10, amd64.BaseDisp(amd64.R8, jitICSlot))
	c.a.LEAQ(amd64.R10, amd64.Indexed(amd64.R10, amd64.R10, 4, 0)) // slot * 5
	c.a.LEAQ(amd64.R9, amd64.Indexed(amd64.R9, amd64.R10, 8, 0))   // + slot * 40
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.R9, jitHNodeVal))
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.R9, jitHNodeVal+8))
	c.a.CMPQImm(amd64.RDX, int32(tagNil))
	c.a.JCC(amd64.NE, lStore)
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitICNoMeta), 0)
	c.a.JCC(amd64.NE, lStore)
	c.a.JMP(c.cold(ipc))

	c.a.Bind(lOwn)
	c.a.CMPLImm(amd64.RCX, int32(icOwnSlot))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVLLoad(amd64.R9, amd64.BaseDisp(amd64.RSI, jitTblShapeRef))
	c.a.MOVLLoad(amd64.R10, amd64.BaseDisp(amd64.R8, jitICShapeRef))
	c.a.CMPL(amd64.R9, amd64.R10)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVLLoad(amd64.RDI, amd64.BaseDisp(amd64.R8, jitICSlot))
	c.emitTableSlotAddr(amd64.RSI, amd64.RDI, amd64.R10, amd64.R11)
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.R10, 0))
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.R10, 8))
	c.a.CMPQImm(amd64.RDX, int32(tagNil))
	c.a.JCC(amd64.NE, lStore)
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitICNoMeta), 0)
	c.a.JCC(amd64.E, c.cold(ipc))

	c.a.Bind(lStore)
	c.storeReg(w.A(), amd64.RAX, amd64.RDX)
}

func (c *x64Baseline) emitSetTabUp(ipc int, w bytecode.Instruction) {
	if !c.loadICPtr(ipc, amd64.R8) {
		c.a.JMP(c.cold(ipc))
		return
	}
	c.emitLoadUpval(ipc, w.A(), amd64.R9, amd64.R10, amd64.RCX, amd64.RSI)
	c.a.CMPQImm(amd64.R10, int32(tagTable))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.emitResolveSmall(amd64.R9, amd64.RSI, amd64.RCX)
	c.a.MOVZXBL(amd64.RCX, amd64.BaseDisp(amd64.RSI, jitTblFlags))
	c.a.TESTQImm(amd64.RCX, 1)
	c.a.JCC(amd64.NE, c.cold(ipc))

	c.loadRegOrConst(w.K(), w.C(), amd64.RAX, amd64.RDX, amd64.RCX)
	c.a.CMPQImm(amd64.RDX, int32(tagNil))
	c.a.JCC(amd64.E, c.cold(ipc))

	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitICKind), uint8(icEnvSlot))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVLLoad(amd64.R9, amd64.BaseDisp(amd64.R8, jitICHolderRef))
	c.a.MOVLLoad(amd64.R10, amd64.BaseDisp(amd64.RSI, jitTblSelf))
	c.a.CMPL(amd64.R9, amd64.R10)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVLLoad(amd64.R9, amd64.BaseDisp(amd64.RSI, jitTblHashRef))
	c.a.TESTL(amd64.R9, amd64.R9)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.emitResolveSmall(amd64.R9, amd64.R9, amd64.R10)
	c.a.MOVQLoad(amd64.R10, amd64.BaseDisp(amd64.R9, jitThashDictGen))
	c.a.CMPQMem(amd64.R10, amd64.BaseDisp(amd64.R8, jitICEpoch))
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVLLoad(amd64.R9, amd64.BaseDisp(amd64.R9, jitThashNodesRef))
	c.a.TESTL(amd64.R9, amd64.R9)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.emitResolveFast(amd64.R9, amd64.R9, amd64.R10)
	c.a.MOVLLoad(amd64.R10, amd64.BaseDisp(amd64.R8, jitICSlot))
	c.a.LEAQ(amd64.R10, amd64.Indexed(amd64.R10, amd64.R10, 4, 0))
	c.a.LEAQ(amd64.R9, amd64.Indexed(amd64.R9, amd64.R10, 8, 0))
	c.a.CMPQMemImm(amd64.BaseDisp(amd64.R9, jitHNodeVal+jitValTag), int32(tagNil))
	c.a.JCC(amd64.E, c.cold(ipc))
	lBarrierOK := c.a.NewLabel()
	c.a.LEAQ(amd64.R11, amd64.BaseDisp(amd64.RDX, -int32(tagFirstGC)))
	c.a.CMPQImm(amd64.R11, int32(tagLastGC-tagFirstGC))
	c.a.JCC(amd64.A, lBarrierOK)
	c.a.MOVZXBL(amd64.R11, amd64.BaseDisp(amd64.RSI, jitTblMark))
	c.a.TESTQImm(amd64.R11, 1<<jitMarkBlackBit)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.Bind(lBarrierOK)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R9, jitHNodeVal), amd64.RAX)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R9, jitHNodeVal+8), amd64.RDX)
	c.a.MOVBStoreImm8(amd64.BaseDisp(amd64.RSI, jitTblTMFlags), 0)
}

// --- Calls, TailCalls & Returns ---

func (c *x64Baseline) emitCall(ipc int, w bytecode.Instruction) {
	a := w.A()
	b := w.B()
	cArg := w.C()
	if b == 0 {
		c.a.JMP(c.cold(ipc))
		return
	}
	nargs := b - 1
	c.loadReg(a, amd64.RAX, amd64.RDX) // fn = (RAX, RDX)

	lCheckStatic := c.a.NewLabel()
	c.a.CMPQImm(amd64.RDX, int32(tagClosure))
	c.a.JCC(amd64.NE, lCheckStatic)

	// RAX = cl.self; resolve *Closure -> RSI
	c.emitResolveSmall(amd64.RAX, amd64.RSI, amd64.RCX)
	c.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(amd64.RSI, jitClProtoIdx)) // RCX = calleeProtoIdx
	c.a.CMPQMem(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxJITProtoCap))
	c.a.JCC(amd64.AE, c.cold(ipc))
	c.a.MOVQLoad(amd64.R8, amd64.BaseDisp(x64Ctx, jitCtxJITEntries))
	c.a.MOVQLoad(amd64.R11, amd64.Indexed(amd64.R8, amd64.RCX, 8, 0)) // R11 = callee native entryPC
	c.a.CMPQImm(amd64.R11, 4096)
	c.a.JCC(amd64.B, c.cold(ipc))

	c.a.MOVQLoad(amd64.R8, amd64.BaseDisp(x64Ctx, jitCtxProtos))
	c.a.MOVQLoad(amd64.R8, amd64.Indexed(amd64.R8, amd64.RCX, 8, 0)) // R8 = callee *boundProto
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitBPIsVararg), 0)
	c.a.JCC(amd64.NE, c.cold(ipc))

	c.a.MOVQLoad(amd64.R9, amd64.BaseDisp(x64Ctx, jitCtxTh))       // R9 = *Thread
	c.a.MOVQLoad(amd64.R10, amd64.BaseDisp(amd64.R9, jitThNFrame)) // R10 = newCI
	c.a.CMPQMem(amd64.R10, amd64.BaseDisp(amd64.R9, jitThFramesCap))
	c.a.JCC(amd64.GE, c.cold(ipc))

	// funcIdx -> RDX, newBase -> RDI
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(x64Ctx, jitCtxBase))
	if a > 0 {
		c.a.ADDQImm(amd64.RDX, int32(a))
	}
	c.a.LEAQ(amd64.RDI, amd64.BaseDisp(amd64.RDX, 1))
	c.a.MOVZXBL(amd64.RCX, amd64.BaseDisp(amd64.R8, jitBPMaxStack))
	c.a.ADDQ(amd64.RCX, amd64.RDI) // frameEnd = newBase + maxStack
	c.a.LEAQ(amd64.RCX, amd64.BaseDisp(amd64.RCX, stackSlack))
	c.a.CMPQMem(amd64.RCX, amd64.BaseDisp(amd64.R9, jitThStackLen))
	c.a.JCC(amd64.G, c.cold(ipc))

	// Poll check before mutating frame state
	c.a.SUBQImm(x64Poll, 1)
	c.a.JCC(amd64.LE, c.poll(ipc))

	// Compute nf = &frames[newCI] -> RCX
	c.a.IMULQImm(amd64.RCX, amd64.R10, jitCFSize)
	c.a.ADDQMem(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxFrames))
	lClean := c.a.NewLabel()
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.RCX, jitCFDirty), 0)
	c.a.JCC(amd64.E, lClean)
	for off := int32(0); off < jitCFSize; off += 8 {
		c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.RCX, off), 0)
	}
	c.a.Bind(lClean)

	// Populate nf (save clRef=RAX, base=RDI, funcIdx=RDX, pc=0, nresults=cArg-1, oldPC=-1)
	c.a.MOVLStore(amd64.BaseDisp(amd64.RCX, jitCFClRef), amd64.RAX)
	c.a.MOVQStore(amd64.BaseDisp(amd64.RCX, jitCFBase), amd64.RDI)
	c.a.MOVQStore(amd64.BaseDisp(amd64.RCX, jitCFFuncIdx), amd64.RDX)
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.RCX, jitCFPC), 0)
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.RCX, jitCFNResults), int32(cArg-1))
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.RCX, jitCFOldPC), -1)

	// Fill missing params with nil if nargs < numParams (clobbers RAX, RDX, RCX)
	c.a.MOVZXBL(amd64.RAX, amd64.BaseDisp(amd64.R8, jitBPNumParams))
	c.a.MOVImm32(amd64.RDX, uint32(nargs))
	lParamsDone := c.a.NewLabel()
	c.a.CMPQ(amd64.RDX, amd64.RAX)
	c.a.JCC(amd64.GE, lParamsDone)
	c.a.SUBQ(amd64.RAX, amd64.RDX) // count = nparams - nargs (> 0)
	c.a.ADDQ(amd64.RDX, amd64.RDI) // newBase + nargs
	c.a.SHLQImm(amd64.RDX, 4)
	c.a.ADDQMem(amd64.RDX, amd64.BaseDisp(x64Ctx, jitCtxStack))
	lNilLoop := c.a.NewLabel()
	c.a.Bind(lNilLoop)
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.RDX, 0), 0)
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.RDX, 8), int32(tagNil))
	c.a.ADDQImm(amd64.RDX, 16)
	c.a.SUBQImm(amd64.RAX, 1)
	c.a.JCC(amd64.NE, lNilLoop)
	c.a.Bind(lParamsDone)

	// Save caller PC (ipc+1) into frames[curCI].pc
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(x64Ctx, jitCtxCI))
	c.a.IMULQImm(amd64.RAX, amd64.RAX, jitCFSize)
	c.a.ADDQMem(amd64.RAX, amd64.BaseDisp(x64Ctx, jitCtxFrames))
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.RAX, jitCFPC), int32(ipc+1))

	// Update Thread nframe & high watermarks (R9=*Thread, R10=newCI, RDI=newBase, R8=*boundProto)
	c.a.LEAQ(amd64.RAX, amd64.BaseDisp(amd64.R10, 1))
	c.a.MOVQStore(amd64.BaseDisp(amd64.R9, jitThNFrame), amd64.RAX)
	lHFOK := c.a.NewLabel()
	c.a.CMPQMem(amd64.RAX, amd64.BaseDisp(amd64.R9, jitThHighFrames))
	c.a.JCC(amd64.LE, lHFOK)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R9, jitThHighFrames), amd64.RAX)
	c.a.Bind(lHFOK)
	c.a.MOVZXBL(amd64.RAX, amd64.BaseDisp(amd64.R8, jitBPMaxStack))
	c.a.ADDQ(amd64.RAX, amd64.RDI) // frameEnd
	lHSOK := c.a.NewLabel()
	c.a.CMPQMem(amd64.RAX, amd64.BaseDisp(amd64.R9, jitThHighStack))
	c.a.JCC(amd64.LE, lHSOK)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R9, jitThHighStack), amd64.RAX)
	c.a.Bind(lHSOK)

	// Switch ctx & pinned regs to callee and tail-jump to R11!
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxCI), amd64.R10)
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxBase), amd64.RDI)
	c.a.MOVQ(x64Regs, amd64.RDI)
	c.a.SHLQImm(x64Regs, 4)
	c.a.ADDQMem(x64Regs, amd64.BaseDisp(x64Ctx, jitCtxStack))

	c.a.MOVLLoad(amd64.RAX, amd64.BaseDisp(amd64.RSI, jitClUpvalsRef))
	lNoUv := c.a.NewLabel()
	lCheckBP := c.a.NewLabel()
	c.a.TESTL(amd64.RAX, amd64.RAX)
	c.a.JCC(amd64.E, lNoUv)
	c.emitResolveSmall(amd64.RAX, amd64.RAX, amd64.RCX)
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxClUpvals), amd64.RAX)
	c.a.JMP(lCheckBP)
	c.a.Bind(lNoUv)
	c.a.MOVQStoreImm32(amd64.BaseDisp(x64Ctx, jitCtxClUpvals), 0)

	c.a.Bind(lCheckBP)
	lSameBP := c.a.NewLabel()
	c.a.CMPQMem(amd64.R8, amd64.BaseDisp(x64Ctx, jitCtxCurBP))
	c.a.JCC(amd64.E, lSameBP)
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxCurBP), amd64.R8)
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.R8, jitBPConsts))
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxConsts), amd64.RAX)
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.R8, jitBPICSlots))
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxICSlots), amd64.RAX)
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.R8, jitBPIC))
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxIC), amd64.RAX)
	c.a.Bind(lSameBP)
	c.a.JMPReg(amd64.R11)

	// Inline hardware intrinsics for math.sqrt / math.abs when C == 2 && B >= 2
	c.a.Bind(lCheckStatic)
	if cArg != 2 || b < 2 {
		c.a.JMP(c.cold(ipc))
		return
	}
	c.a.CMPQImm(amd64.RDX, int32(tagStaticNative))
	c.a.JCC(amd64.NE, c.cold(ipc))
	lCheckAbs := c.a.NewLabel()
	lDone := c.pcLabels[ipc+1]
	c.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxSqrtIdx))
	c.a.TESTL(amd64.RCX, amd64.RCX)
	c.a.JCC(amd64.E, lCheckAbs)
	c.a.CMPQ(amd64.RAX, amd64.RCX)
	c.a.JCC(amd64.NE, lCheckAbs)
	c.loadReg(a+1, amd64.RSI, amd64.RDI)
	lSqrtF := c.a.NewLabel()
	c.a.CMPQImm(amd64.RDI, int32(tagFloat))
	c.a.JCC(amd64.E, lSqrtF)
	c.a.TESTQ(amd64.RDI, amd64.RDI)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.CVTSI2SDQ(amd64.XMM0, amd64.RSI)
	c.a.SQRTSD(amd64.XMM0, amd64.XMM0)
	c.a.MOVSDStore(c.regBits(a), amd64.XMM0)
	c.a.MOVQStoreImm32(c.regTag(a), int32(tagFloat))
	c.a.JMP(lDone)
	c.a.Bind(lSqrtF)
	c.a.MOVQToX(amd64.XMM0, amd64.RSI)
	c.a.SQRTSD(amd64.XMM0, amd64.XMM0)
	c.a.MOVSDStore(c.regBits(a), amd64.XMM0)
	c.a.MOVQStoreImm32(c.regTag(a), int32(tagFloat))
	c.a.JMP(lDone)

	c.a.Bind(lCheckAbs)
	c.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxAbsIdx))
	c.a.TESTL(amd64.RCX, amd64.RCX)
	c.a.JCC(amd64.E, c.cold(ipc))
	c.a.CMPQ(amd64.RAX, amd64.RCX)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.loadReg(a+1, amd64.RSI, amd64.RDI)
	lAbsF := c.a.NewLabel()
	c.a.CMPQImm(amd64.RDI, int32(tagFloat))
	c.a.JCC(amd64.E, lAbsF)
	c.a.TESTQ(amd64.RDI, amd64.RDI)
	c.a.JCC(amd64.NE, c.cold(ipc))
	c.a.MOVQ(amd64.R8, amd64.RSI)
	c.a.NEGQ(amd64.R8)
	c.a.TESTQ(amd64.RSI, amd64.RSI)
	c.a.CMOVQ(amd64.L, amd64.RSI, amd64.R8)
	c.storeRegTagImm(a, amd64.RSI, int32(tagInt))
	c.a.JMP(lDone)
	c.a.Bind(lAbsF)
	c.a.MOVImm64(amd64.R8, ^(uint64(1) << 63))
	c.a.ANDQ(amd64.RSI, amd64.R8)
	c.storeRegTagImm(a, amd64.RSI, int32(tagFloat))
}

func (c *x64Baseline) emitTailCall(ipc int, w bytecode.Instruction) {
	if w.K() || w.B() == 0 {
		c.a.JMP(c.cold(ipc))
		return
	}
	a := w.A()
	nargs := w.B() - 1
	c.loadReg(a, amd64.RAX, amd64.RDX)
	c.a.CMPQImm(amd64.RDX, int32(tagClosure))
	c.a.JCC(amd64.NE, c.cold(ipc))

	c.emitResolveSmall(amd64.RAX, amd64.RSI, amd64.RCX) // RSI = *Closure
	c.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(amd64.RSI, jitClProtoIdx))
	c.a.CMPQMem(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxJITProtoCap))
	c.a.JCC(amd64.AE, c.cold(ipc))
	c.a.MOVQLoad(amd64.R8, amd64.BaseDisp(x64Ctx, jitCtxJITEntries))
	c.a.MOVQLoad(amd64.R11, amd64.Indexed(amd64.R8, amd64.RCX, 8, 0)) // R11 = callee entryPC
	c.a.CMPQImm(amd64.R11, 4096)
	c.a.JCC(amd64.B, c.cold(ipc))

	c.a.MOVQLoad(amd64.R8, amd64.BaseDisp(x64Ctx, jitCtxProtos))
	c.a.MOVQLoad(amd64.R8, amd64.Indexed(amd64.R8, amd64.RCX, 8, 0)) // R8 = *boundProto
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.R8, jitBPIsVararg), 0)
	c.a.JCC(amd64.NE, c.cold(ipc))

	// Load cf -> R9, funcIdx -> RDX, newBase -> RDI
	c.a.MOVQLoad(amd64.R9, amd64.BaseDisp(x64Ctx, jitCtxCI))
	c.a.IMULQImm(amd64.R9, amd64.R9, jitCFSize)
	c.a.ADDQMem(amd64.R9, amd64.BaseDisp(x64Ctx, jitCtxFrames))
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.R9, jitCFFuncIdx))
	c.a.LEAQ(amd64.RDI, amd64.BaseDisp(amd64.RDX, 1))
	c.a.MOVZXBL(amd64.RCX, amd64.BaseDisp(amd64.R8, jitBPMaxStack)) // maxStack
	c.a.MOVImm32(amd64.R10, uint32(nargs))
	c.a.CMPQ(amd64.R10, amd64.RCX)
	c.a.CMOVQ(amd64.G, amd64.RCX, amd64.R10) // max(nargs, maxStack)
	c.a.ADDQ(amd64.RCX, amd64.RDI)
	c.a.ADDQImm(amd64.RCX, stackSlack)
	c.a.MOVQLoad(amd64.R10, amd64.BaseDisp(x64Ctx, jitCtxTh))
	c.a.CMPQMem(amd64.RCX, amd64.BaseDisp(amd64.R10, jitThStackLen))
	c.a.JCC(amd64.G, c.cold(ipc))

	// Poll check before clobbering current frame args
	c.a.SUBQImm(x64Poll, 1)
	c.a.JCC(amd64.LE, c.poll(ipc))

	// Update cf in place first while RAX still holds cl.self
	c.a.MOVLStore(amd64.BaseDisp(amd64.R9, jitCFClRef), amd64.RAX)
	c.a.MOVLStoreImm32(amd64.BaseDisp(amd64.R9, jitCFVarargTabRef), 0)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R9, jitCFBase), amd64.RDI)
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.R9, jitCFPC), 0)
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.R9, jitCFNExtraArgs), 0)
	c.a.MOVBStoreImm8(amd64.BaseDisp(amd64.R9, jitCFDirty), 1)
	c.a.MOVBStoreImm8(amd64.BaseDisp(amd64.R9, jitCFIsTailCall), 1)
	c.a.MOVBStoreImm8(amd64.BaseDisp(amd64.R9, jitCFExtraArgs), 0)
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.R9, jitCFOldPC), -1)

	// Copy [regs+a*16 .. +(nargs+1)*16) -> [stack+funcIdx*16 ..)
	c.a.LEAQ(amd64.RAX, amd64.BaseDisp(x64Regs, int32(a*16))) // src
	c.a.SHLQImm(amd64.RDX, 4)
	c.a.ADDQMem(amd64.RDX, amd64.BaseDisp(x64Ctx, jitCtxStack)) // dst
	c.a.MOVImm32(amd64.RCX, uint32(nargs+1))
	lCopy := c.a.NewLabel()
	c.a.Bind(lCopy)
	c.a.MOVQLoad(amd64.R9, amd64.BaseDisp(amd64.RAX, 0))
	c.a.MOVQStore(amd64.BaseDisp(amd64.RDX, 0), amd64.R9)
	c.a.MOVQLoad(amd64.R9, amd64.BaseDisp(amd64.RAX, 8))
	c.a.MOVQStore(amd64.BaseDisp(amd64.RDX, 8), amd64.R9)
	c.a.ADDQImm(amd64.RAX, 16)
	c.a.ADDQImm(amd64.RDX, 16)
	c.a.SUBQImm(amd64.RCX, 1)
	c.a.JCC(amd64.NE, lCopy)

	// Fill [newBase+nargs .. newBase+maxStack) with nil
	c.a.MOVZXBL(amd64.RCX, amd64.BaseDisp(amd64.R8, jitBPMaxStack))
	c.a.MOVImm32(amd64.RAX, uint32(nargs))
	lNilDone := c.a.NewLabel()
	c.a.CMPQ(amd64.RAX, amd64.RCX)
	c.a.JCC(amd64.GE, lNilDone)
	c.a.SUBQ(amd64.RCX, amd64.RAX)
	lNil := c.a.NewLabel()
	c.a.Bind(lNil)
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.RDX, 0), 0)
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.RDX, 8), int32(tagNil))
	c.a.ADDQImm(amd64.RDX, 16)
	c.a.SUBQImm(amd64.RCX, 1)
	c.a.JCC(amd64.NE, lNil)
	c.a.Bind(lNilDone)

	// Update top & highStack (R10=*Thread, RDI=newBase, R8=*boundProto)
	c.a.MOVZXBL(amd64.RAX, amd64.BaseDisp(amd64.R8, jitBPMaxStack))
	c.a.ADDQ(amd64.RAX, amd64.RDI)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R10, jitThTop), amd64.RAX)
	lHSOK := c.a.NewLabel()
	c.a.CMPQMem(amd64.RAX, amd64.BaseDisp(amd64.R10, jitThHighStack))
	c.a.JCC(amd64.LE, lHSOK)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R10, jitThHighStack), amd64.RAX)
	c.a.Bind(lHSOK)

	// Update ctx & pinned regs and tail-jump to R11
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxBase), amd64.RDI)
	c.a.MOVQ(x64Regs, amd64.RDI)
	c.a.SHLQImm(x64Regs, 4)
	c.a.ADDQMem(x64Regs, amd64.BaseDisp(x64Ctx, jitCtxStack))

	c.a.MOVLLoad(amd64.RAX, amd64.BaseDisp(amd64.RSI, jitClUpvalsRef))
	lNoUv := c.a.NewLabel()
	lCheckBP := c.a.NewLabel()
	c.a.TESTL(amd64.RAX, amd64.RAX)
	c.a.JCC(amd64.E, lNoUv)
	c.emitResolveSmall(amd64.RAX, amd64.RAX, amd64.RCX)
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxClUpvals), amd64.RAX)
	c.a.JMP(lCheckBP)
	c.a.Bind(lNoUv)
	c.a.MOVQStoreImm32(amd64.BaseDisp(x64Ctx, jitCtxClUpvals), 0)

	c.a.Bind(lCheckBP)
	lSameBP := c.a.NewLabel()
	c.a.CMPQMem(amd64.R8, amd64.BaseDisp(x64Ctx, jitCtxCurBP))
	c.a.JCC(amd64.E, lSameBP)
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxCurBP), amd64.R8)
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.R8, jitBPConsts))
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxConsts), amd64.RAX)
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.R8, jitBPICSlots))
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxICSlots), amd64.RAX)
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.R8, jitBPIC))
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxIC), amd64.RAX)
	c.a.Bind(lSameBP)
	c.a.JMPReg(amd64.R11)
}

func (c *x64Baseline) emitReturn0() {
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(x64Ctx, jitCtxCI))
	c.a.IMULQImm(amd64.RAX, amd64.RAX, jitCFSize)
	c.a.MOVQLoad(amd64.RSI, amd64.BaseDisp(x64Ctx, jitCtxFrames))
	c.a.ADDQ(amd64.RAX, amd64.RSI) // RAX = cf, RSI = frames
	c.a.MOVQLoad(amd64.RCX, amd64.BaseDisp(amd64.RAX, jitCFNResults))
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.RAX, jitCFFuncIdx))
	c.a.MOVQLoad(amd64.R8, amd64.BaseDisp(x64Ctx, jitCtxTh))
	c.a.MOVQLoad(amd64.R9, amd64.BaseDisp(amd64.R8, jitThNFrame))
	c.a.SUBQImm(amd64.R9, 1)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R8, jitThNFrame), amd64.R9)

	lNoFill := c.a.NewLabel()
	c.a.TESTQ(amd64.RCX, amd64.RCX)
	c.a.JCC(amd64.LE, lNoFill)
	c.a.MOVQ(amd64.RDI, amd64.RDX)
	c.a.SHLQImm(amd64.RDI, 4)
	c.a.ADDQMem(amd64.RDI, amd64.BaseDisp(x64Ctx, jitCtxStack))
	c.a.MOVQ(amd64.R10, amd64.RCX)
	lLoop := c.a.NewLabel()
	c.a.Bind(lLoop)
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.RDI, 0), 0)
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.RDI, 8), int32(tagNil))
	c.a.ADDQImm(amd64.RDI, 16)
	c.a.SUBQImm(amd64.R10, 1)
	c.a.JCC(amd64.NE, lLoop)
	c.a.ADDQ(amd64.RDX, amd64.RCX)
	c.a.Bind(lNoFill)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R8, jitThTop), amd64.RDX)
	c.a.CMPQMem(amd64.R9, amd64.BaseDisp(x64Ctx, jitCtxEntryDepth))
	c.a.JCC(amd64.LE, c.doneLabel())
	c.a.JMP(c.retRestoreLabel())
}

func (c *x64Baseline) emitReturn1(a int) {
	c.loadReg(a, amd64.R10, amd64.R11) // retVal = (R10, R11)
	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(x64Ctx, jitCtxCI))
	c.a.IMULQImm(amd64.RAX, amd64.RAX, jitCFSize)
	c.a.MOVQLoad(amd64.RSI, amd64.BaseDisp(x64Ctx, jitCtxFrames))
	c.a.ADDQ(amd64.RAX, amd64.RSI) // RAX = cf, RSI = frames
	c.a.MOVQLoad(amd64.RCX, amd64.BaseDisp(amd64.RAX, jitCFNResults))
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.RAX, jitCFFuncIdx))
	c.a.MOVQLoad(amd64.R8, amd64.BaseDisp(x64Ctx, jitCtxTh))
	c.a.MOVQLoad(amd64.R9, amd64.BaseDisp(amd64.R8, jitThNFrame))
	c.a.SUBQImm(amd64.R9, 1)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R8, jitThNFrame), amd64.R9)

	lSetTop := c.a.NewLabel()
	lOneRes := c.a.NewLabel()
	c.a.TESTQ(amd64.RCX, amd64.RCX)
	c.a.JCC(amd64.E, lSetTop)
	c.a.MOVQ(amd64.RDI, amd64.RDX)
	c.a.SHLQImm(amd64.RDI, 4)
	c.a.ADDQMem(amd64.RDI, amd64.BaseDisp(x64Ctx, jitCtxStack))
	c.a.MOVQStore(amd64.BaseDisp(amd64.RDI, 0), amd64.R10)
	c.a.MOVQStore(amd64.BaseDisp(amd64.RDI, 8), amd64.R11)
	c.a.CMPQImm(amd64.RCX, 1)
	c.a.JCC(amd64.LE, lOneRes)
	c.a.ADDQImm(amd64.RDI, 16)
	c.a.LEAQ(amd64.R10, amd64.BaseDisp(amd64.RCX, -1))
	lLoop := c.a.NewLabel()
	c.a.Bind(lLoop)
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.RDI, 0), 0)
	c.a.MOVQStoreImm32(amd64.BaseDisp(amd64.RDI, 8), int32(tagNil))
	c.a.ADDQImm(amd64.RDI, 16)
	c.a.SUBQImm(amd64.R10, 1)
	c.a.JCC(amd64.NE, lLoop)
	c.a.ADDQ(amd64.RDX, amd64.RCX)
	c.a.JMP(lSetTop)
	c.a.Bind(lOneRes)
	c.a.ADDQImm(amd64.RDX, 1)
	c.a.Bind(lSetTop)
	c.a.MOVQStore(amd64.BaseDisp(amd64.R8, jitThTop), amd64.RDX)
	c.a.CMPQMem(amd64.R9, amd64.BaseDisp(x64Ctx, jitCtxEntryDepth))
	c.a.JCC(amd64.LE, c.doneLabel())
	c.a.JMP(c.retRestoreLabel())
}

func (c *x64Baseline) emitReturn(ipc int, w bytecode.Instruction) {
	if w.K() {
		c.a.JMP(c.cold(ipc))
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
	c.a.JMP(c.cold(ipc))
}

// emitReturnRestoreCaller restores caller frame (inputs: RSI=frames, R9=newNFrame > entryDepth)
// and tail-jumps directly to caller's native pcEntries[callerPC], or exits jitExitReload.
func (c *x64Baseline) emitReturnRestoreCaller() {
	c.a.Bind(c.retRestore)
	c.a.LEAQ(amd64.RAX, amd64.BaseDisp(amd64.R9, -1)) // callerCI
	c.a.IMULQImm(amd64.RCX, amd64.RAX, jitCFSize)
	c.a.ADDQ(amd64.RCX, amd64.RSI) // RCX = callerCF
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.RCX, jitCFKind), 0)
	c.a.JCC(amd64.NE, c.reloadLabel())
	c.a.CMPBMemImm(amd64.BaseDisp(amd64.RCX, jitCFFinish), 0)
	c.a.JCC(amd64.NE, c.reloadLabel())

	c.a.MOVLLoad(amd64.RDX, amd64.BaseDisp(amd64.RCX, jitCFClRef))
	c.emitResolveSmall(amd64.RDX, amd64.RSI, amd64.RDI) // RSI = caller *Closure
	c.a.MOVLLoad(amd64.R8, amd64.BaseDisp(amd64.RSI, jitClProtoIdx))
	c.a.CMPQMem(amd64.R8, amd64.BaseDisp(x64Ctx, jitCtxJITProtoCap))
	c.a.JCC(amd64.AE, c.reloadLabel())
	c.a.MOVQLoad(amd64.R9, amd64.BaseDisp(x64Ctx, jitCtxJITPCTables))
	c.a.MOVQLoad(amd64.R11, amd64.Indexed(amd64.R9, amd64.R8, 8, 0)) // R11 = caller pcEntries base
	c.a.TESTQ(amd64.R11, amd64.R11)
	c.a.JCC(amd64.E, c.reloadLabel())

	// Caller is compiled: update ctx & pinned regs and tail-jump!
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxCI), amd64.RAX)
	c.a.MOVQLoad(amd64.RDI, amd64.BaseDisp(amd64.RCX, jitCFBase))
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxBase), amd64.RDI)
	c.a.MOVQ(x64Regs, amd64.RDI)
	c.a.SHLQImm(x64Regs, 4)
	c.a.ADDQMem(x64Regs, amd64.BaseDisp(x64Ctx, jitCtxStack))

	c.a.MOVLLoad(amd64.RDX, amd64.BaseDisp(amd64.RSI, jitClUpvalsRef))
	lNoUv := c.a.NewLabel()
	lCheckBP := c.a.NewLabel()
	c.a.TESTL(amd64.RDX, amd64.RDX)
	c.a.JCC(amd64.E, lNoUv)
	c.emitResolveSmall(amd64.RDX, amd64.RDX, amd64.RDI)
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxClUpvals), amd64.RDX)
	c.a.JMP(lCheckBP)
	c.a.Bind(lNoUv)
	c.a.MOVQStoreImm32(amd64.BaseDisp(x64Ctx, jitCtxClUpvals), 0)

	c.a.Bind(lCheckBP)
	c.a.MOVQLoad(amd64.R9, amd64.BaseDisp(x64Ctx, jitCtxProtos))
	c.a.MOVQLoad(amd64.R9, amd64.Indexed(amd64.R9, amd64.R8, 8, 0)) // R9 = caller *boundProto
	lSameBP := c.a.NewLabel()
	c.a.CMPQMem(amd64.R9, amd64.BaseDisp(x64Ctx, jitCtxCurBP))
	c.a.JCC(amd64.E, lSameBP)
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxCurBP), amd64.R9)
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.R9, jitBPConsts))
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxConsts), amd64.RDX)
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.R9, jitBPICSlots))
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxICSlots), amd64.RDX)
	c.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.R9, jitBPIC))
	c.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxIC), amd64.RDX)
	c.a.Bind(lSameBP)

	c.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.RCX, jitCFPC))
	c.a.MOVQLoad(amd64.R11, amd64.Indexed(amd64.R11, amd64.RAX, 8, 0))
	c.a.JMPReg(amd64.R11)
}
