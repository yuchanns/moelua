//go:build amd64 && !moeluanojit && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"github.com/yuchanns/moelua/internal/jit/amd64"
	"github.com/yuchanns/moelua/internal/jit/ir"
)

// Allocatable register banks for AMD64 Tier-2 optimizing loop IR.
// RBX=ctx, R12=regs, R13=pagePtrs, R14=g, R15=poll, RBP/RSP stay pinned to baseline.
// RAX, RCX, RDX are reserved emitter scratch GPRs (free for IDIV/CQO, shifts via CL, spills, resolveFast).
// XMM0..XMM11 are allocatable FP regs; XMM12..XMM14 are scratch; XMM15 stays zero (Go ABIInternal).
var (
	x64OptGPRs = [6]amd64.Reg{
		amd64.RSI, amd64.RDI, amd64.R8, amd64.R9, amd64.R10, amd64.R11,
	}
	x64OptFPs = [12]amd64.XReg{
		amd64.XMM0, amd64.XMM1, amd64.XMM2, amd64.XMM3,
		amd64.XMM4, amd64.XMM5, amd64.XMM6, amd64.XMM7,
		amd64.XMM8, amd64.XMM9, amd64.XMM10, amd64.XMM11,
	}
	x64AllScratchFPs = [14]amd64.XReg{
		amd64.XMM0, amd64.XMM1, amd64.XMM2, amd64.XMM3,
		amd64.XMM4, amd64.XMM5, amd64.XMM6, amd64.XMM7,
		amd64.XMM8, amd64.XMM9, amd64.XMM10, amd64.XMM11,
		amd64.XMM12, amd64.XMM13,
	}
)

const (
	x64OptNumGPR    = len(x64OptGPRs)
	x64OptNumFP     = len(x64OptFPs)
	x64OptMaxSpills = 16
)

type x64OptLoopRef struct {
	reg       *ir.LoopRegion
	entryLbl  amd64.Label
	resumeLbl amd64.Label
	baseLbl   amd64.Label
}

type x64OptEmit struct {
	c       *x64Baseline
	a       *amd64.Assembler
	reg     *ir.LoopRegion
	alloc   *ir.AllocResult
	ref     *x64OptLoopRef
	preFail amd64.Label
	deopt   amd64.Label
	pollLbl amd64.Label

	cntVal  ir.ValID
	stepVal ir.ValID
	idxVar  int
}

func (c *x64Baseline) emitAllOptLoops() {
	for _, ref := range c.optLoops {
		e := &x64OptEmit{
			c:       c,
			a:       c.a,
			reg:     ref.reg,
			alloc:   ref.reg.Alloc,
			ref:     ref,
			cntVal:  ir.NoVal,
			stepVal: ir.NoVal,
			idxVar:  -1,
		}
		e.emit()
	}
}

func (e *x64OptEmit) spillMem(slot int8) amd64.Mem {
	if slot < 0 || slot >= x64OptMaxSpills {
		panic("jit/amd64: invalid optSpill slot")
	}
	return amd64.BaseDisp(x64Ctx, jitCtxOptSpill+int32(slot)*8)
}

func (e *x64OptEmit) loadG(loc ir.RegLoc, scratch amd64.Reg) amd64.Reg {
	if loc.Phys >= 0 {
		return x64OptGPRs[loc.Phys]
	}
	e.a.MOVQLoad(scratch, e.spillMem(loc.Spill))
	return scratch
}

func (e *x64OptEmit) storeG(loc ir.RegLoc, src amd64.Reg) {
	if loc.Phys >= 0 {
		dst := x64OptGPRs[loc.Phys]
		if dst != src {
			e.a.MOVQ(dst, src)
		}
		return
	}
	e.a.MOVQStore(e.spillMem(loc.Spill), src)
}

func (e *x64OptEmit) loadF(loc ir.RegLoc, scratch amd64.XReg) amd64.XReg {
	if loc.Phys >= 0 {
		return x64OptFPs[loc.Phys]
	}
	e.a.MOVSDLoad(scratch, e.spillMem(loc.Spill))
	return scratch
}

func (e *x64OptEmit) storeF(loc ir.RegLoc, src amd64.XReg) {
	if loc.Phys >= 0 {
		dst := x64OptFPs[loc.Phys]
		if dst != src {
			e.a.MOVSD(dst, src)
		}
		return
	}
	e.a.MOVSDStore(e.spillMem(loc.Spill), src)
}

func (e *x64OptEmit) emit() {
	for i := range e.reg.Preheader {
		switch e.reg.Preheader[i].Op {
		case ir.OpLoopCnt:
			e.cntVal = e.reg.Preheader[i].ID
		case ir.OpLoopStep:
			e.stepVal = e.reg.Preheader[i].ID
		}
	}
	for i, lv := range e.reg.Vars {
		if lv.IsForIdx {
			e.idxVar = i
			break
		}
	}
	if e.reg.Kind == ir.LoopForInt {
		e.emitForIntLoop()
	} else {
		e.emitWhileLoop()
	}
}

func (e *x64OptEmit) emitForIntLoop() {
	a := int(e.reg.ForBase)
	e.preFail = e.c.pcLabels[e.reg.BodyPC]
	e.deopt = e.a.NewLabel()
	e.pollLbl = e.a.NewLabel()

	// --- Entry from OP_FORPREP ---
	e.a.Bind(e.ref.entryLbl)
	e.c.loadReg(a, amd64.RSI, amd64.RAX)
	e.c.loadReg(a+1, amd64.RDI, amd64.RCX)
	e.c.loadReg(a+2, amd64.R8, amd64.RDX)
	e.a.ORQ(amd64.RAX, amd64.RCX)
	e.a.ORQ(amd64.RAX, amd64.RDX)
	e.a.TESTQ(amd64.RAX, amd64.RAX)
	e.a.JCC(amd64.NE, e.ref.baseLbl)
	e.a.TESTQ(amd64.R8, amd64.R8)
	e.a.JCC(amd64.E, e.ref.baseLbl)

	lNeg := e.a.NewLabel()
	lStepNot1 := e.a.NewLabel()
	lStorePrep := e.a.NewLabel()
	e.a.CMPQImm(amd64.R8, 0)
	e.a.JCC(amd64.L, lNeg)
	e.a.CMPQ(amd64.RSI, amd64.RDI)
	e.a.JCC(amd64.G, e.c.pcLabels[e.reg.ExitPC])
	e.a.MOVQ(amd64.RAX, amd64.RDI)
	e.a.SUBQ(amd64.RAX, amd64.RSI)
	e.a.CMPQImm(amd64.R8, 1)
	e.a.JCC(amd64.NE, lStepNot1)
	e.a.JMP(lStorePrep)
	e.a.Bind(lStepNot1)
	e.a.XORL(amd64.RDX, amd64.RDX)
	e.a.DIVQ(amd64.R8)
	e.a.JMP(lStorePrep)

	e.a.Bind(lNeg)
	e.a.CMPQ(amd64.RSI, amd64.RDI)
	e.a.JCC(amd64.L, e.c.pcLabels[e.reg.ExitPC])
	e.a.MOVQ(amd64.RAX, amd64.RSI)
	e.a.SUBQ(amd64.RAX, amd64.RDI)
	e.a.LEAQ(amd64.RCX, amd64.BaseDisp(amd64.R8, 1))
	e.a.NEGQ(amd64.RCX)
	e.a.ADDQImm(amd64.RCX, 1)
	e.a.XORL(amd64.RDX, amd64.RDX)
	e.a.DIVQ(amd64.RCX)

	e.a.Bind(lStorePrep)
	e.c.storeRegTagImm(a, amd64.RAX, int32(tagInt))
	e.c.storeRegTagImm(a+1, amd64.R8, int32(tagInt))
	e.c.storeRegTagImm(a+2, amd64.RSI, int32(tagInt))
	lCommon := e.a.NewLabel()
	e.a.JMP(lCommon)

	// --- OSR Re-entry from baseline OP_FORLOOP ---
	e.a.Bind(e.ref.resumeLbl)
	e.c.loadReg(a, amd64.RAX, amd64.RCX)
	e.c.loadReg(a+1, amd64.R8, amd64.RDX)
	e.c.loadReg(a+2, amd64.RSI, amd64.RDI)
	e.a.ORQ(amd64.RCX, amd64.RDX)
	e.a.ORQ(amd64.RCX, amd64.RDI)
	e.a.TESTQ(amd64.RCX, amd64.RCX)
	e.a.JCC(amd64.NE, e.preFail)

	// Common preheader: RAX=cnt, R8=step, RSI=idx
	e.a.Bind(lCommon)
	// Ensure at least 1 poll unit before entering iteration 1 so pollLbl can never
	// materialize ZeroInit LoopVars before the first iteration executes.
	e.a.CMPQImm(x64Poll, 0)
	e.a.JCC(amd64.LE, e.preFail)
	if e.reg.HasConstStep {
		e.a.MOVImm64(amd64.RCX, uint64(e.reg.ConstStep))
		e.a.CMPQ(amd64.R8, amd64.RCX)
		e.a.JCC(amd64.NE, e.preFail)
	}
	e.a.MOVImm64(amd64.RCX, uint64(1)<<60)
	e.a.CMPQ(amd64.RAX, amd64.RCX)
	e.a.JCC(amd64.AE, e.preFail)

	// Move idx->RDX and step->RCX so storing into allocated x64OptGPRs never clobbers RSI/R8.
	e.a.MOVQ(amd64.RDX, amd64.RSI)
	e.a.MOVQ(amd64.RCX, amd64.R8)
	e.storeG(e.alloc.ValLoc[e.cntVal], amd64.RAX)
	if !e.reg.HasConstStep && e.stepVal != ir.NoVal {
		e.storeG(e.alloc.ValLoc[e.stepVal], amd64.RCX)
	}
	e.storeG(e.alloc.VarLoc[e.idxVar], amd64.RDX)

	e.emitPreheader(e.preFail)

	for i, lv := range e.reg.Vars {
		if lv.IsForIdx {
			continue
		}
		e.copyValToLoc(lv.InitVal, e.alloc.VarLoc[i], lv.Type.IsFPReg())
	}

	// rem = cnt + 1
	remR := e.loadG(e.alloc.ValLoc[e.cntVal], amd64.RAX)
	e.a.ADDQImm(remR, 1)
	e.storeG(e.alloc.ValLoc[e.cntVal], remR)

	// --- Strip-mined Outer Chunk Loop ---
	lChunkTop := e.a.NewLabel()
	lScalarLoop := e.a.NewLabel()
	lChunkDone := e.a.NewLabel()

	e.a.Bind(lChunkTop)
	e.a.CMPQImm(x64Poll, 0)
	e.a.JCC(amd64.LE, e.pollLbl)

	remCur := e.loadG(e.alloc.ValLoc[e.cntVal], amd64.RDX)
	e.a.MOVQ(amd64.RAX, remCur)
	e.a.CMPQ(amd64.RAX, x64Poll)
	e.a.CMOVQ(amd64.G, amd64.RAX, x64Poll)
	e.a.MOVQ(amd64.RCX, remCur)
	e.a.SUBQ(amd64.RCX, amd64.RAX)
	e.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxOptOuterRem), amd64.RCX)
	e.a.SUBQ(x64Poll, amd64.RAX)
	e.storeG(e.alloc.ValLoc[e.cntVal], amd64.RAX)

	if e.reg.Vec != nil {
		e.emitSSE2VecLoop(lScalarLoop, lChunkDone)
	}

	e.a.Bind(lScalarLoop)
	blockLabels := make([]amd64.Label, len(e.reg.Blocks))
	for i := range e.reg.Blocks {
		blockLabels[i] = e.a.NewLabel()
	}
	for i, blk := range e.reg.Blocks {
		e.a.Bind(blockLabels[i])
		e.emitBlockInsts(blk)
		e.emitBlockTerm(blk, blockLabels, lScalarLoop, lChunkDone, true)
	}

	e.a.Bind(lChunkDone)
	e.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(x64Ctx, jitCtxOptOuterRem))
	lFinish := e.a.NewLabel()
	e.a.TESTQ(amd64.RAX, amd64.RAX)
	e.a.JCC(amd64.E, lFinish)
	e.storeG(e.alloc.ValLoc[e.cntVal], amd64.RAX)
	e.a.JMP(lChunkTop)

	e.a.Bind(lFinish)
	e.emitMaterializeVars(true)
	e.a.JMP(e.c.pcLabels[e.reg.ExitPC])

	e.a.Bind(e.pollLbl)
	remP := e.loadG(e.alloc.ValLoc[e.cntVal], amd64.RAX)
	e.a.LEAQ(amd64.RAX, amd64.BaseDisp(remP, -1))
	e.c.storeRegTagImm(a, amd64.RAX, int32(tagInt))
	e.emitStoreForStepAndIdx(a)
	e.emitMaterializeVars(false)
	e.a.JMP(e.c.poll(e.reg.BodyPC))

	e.a.Bind(e.deopt)
	remD := e.loadG(e.alloc.ValLoc[e.cntVal], amd64.RAX)
	if remD != amd64.RAX {
		e.a.MOVQ(amd64.RAX, remD)
	}
	e.a.ADDQMem(amd64.RAX, amd64.BaseDisp(x64Ctx, jitCtxOptOuterRem))
	e.a.SUBQImm(amd64.RAX, 1)
	e.c.storeRegTagImm(a, amd64.RAX, int32(tagInt))
	e.emitStoreForStepAndIdx(a)
	e.emitMaterializeVars(false)
	e.a.JMP(e.c.pcLabels[e.reg.BodyPC])
}

func (e *x64OptEmit) emitStoreForStepAndIdx(forBase int) {
	if e.reg.HasConstStep {
		e.a.MOVImm64(amd64.RAX, uint64(e.reg.ConstStep))
		e.c.storeRegTagImm(forBase+1, amd64.RAX, int32(tagInt))
	} else {
		stepR := e.loadG(e.alloc.ValLoc[e.stepVal], amd64.RAX)
		e.c.storeRegTagImm(forBase+1, stepR, int32(tagInt))
	}
	idxR := e.loadG(e.alloc.VarLoc[e.idxVar], amd64.RAX)
	e.c.storeRegTagImm(forBase+2, idxR, int32(tagInt))
}

func (e *x64OptEmit) emitWhileLoop() {
	e.preFail = e.ref.baseLbl
	e.deopt = e.a.NewLabel()
	e.pollLbl = e.a.NewLabel()

	e.a.Bind(e.ref.entryLbl)
	e.emitPreheader(e.preFail)
	for i, lv := range e.reg.Vars {
		e.copyValToLoc(lv.InitVal, e.alloc.VarLoc[i], lv.Type.IsFPReg())
	}

	blockLabels := make([]amd64.Label, len(e.reg.Blocks))
	for i := range e.reg.Blocks {
		blockLabels[i] = e.a.NewLabel()
	}
	lBack := e.a.NewLabel()

	for i, blk := range e.reg.Blocks {
		e.a.Bind(blockLabels[i])
		e.emitBlockInsts(blk)
		e.emitBlockTerm(blk, blockLabels, lBack, lBack, false)
	}

	e.a.Bind(lBack)
	e.a.SUBQImm(x64Poll, 1)
	e.a.JCC(amd64.LE, e.pollLbl)
	e.a.JMP(blockLabels[0])

	e.a.Bind(e.pollLbl)
	e.emitMaterializeVars(false)
	e.a.JMP(e.c.poll(e.reg.EntryPC))

	e.a.Bind(e.deopt)
	e.emitMaterializeVars(false)
	e.a.JMP(e.ref.baseLbl)
}

func (e *x64OptEmit) emitMaterializeVars(onlyLiveOut bool) {
	for i, lv := range e.reg.Vars {
		if lv.IsForIdx || !lv.Written {
			continue
		}
		if onlyLiveOut && !lv.LiveOut {
			continue
		}
		loc := e.alloc.VarLoc[i]
		switch lv.Type {
		case ir.TInt:
			r := e.loadG(loc, amd64.RAX)
			e.c.storeRegTagImm(int(lv.Slot), r, int32(tagInt))
		case ir.TFloat:
			f := e.loadF(loc, amd64.XMM12)
			e.a.MOVQFromX(amd64.RAX, f)
			e.c.storeRegTagImm(int(lv.Slot), amd64.RAX, int32(tagFloat))
		case ir.TBool:
			r := e.loadG(loc, amd64.RAX)
			e.a.LEAQ(amd64.RCX, amd64.BaseDisp(r, int32(tagFalse)))
			e.a.XORL(amd64.RAX, amd64.RAX)
			e.c.storeReg(int(lv.Slot), amd64.RAX, amd64.RCX)
		}
	}
}

func (e *x64OptEmit) copyValToLoc(srcID ir.ValID, dst ir.RegLoc, isFP bool) {
	src := e.alloc.ValLoc[srcID]
	if isFP {
		f := e.loadF(src, amd64.XMM12)
		e.storeF(dst, f)
	} else {
		r := e.loadG(src, amd64.RAX)
		e.storeG(dst, r)
	}
}

// --- Preheader Guards & Hoisting ---

func (e *x64OptEmit) emitPreheader(fail amd64.Label) {
	for i := range e.reg.Preheader {
		in := &e.reg.Preheader[i]
		if in.Dead {
			continue
		}
		loc := e.alloc.ValLoc[in.ID]
		hasLoc := loc.Phys >= 0 || loc.Spill >= 0
		switch in.Op {
		case ir.OpLoopCnt, ir.OpLoopStep, ir.OpLoopIdx, ir.OpLoadArrLen:
		case ir.OpConstInt:
			if hasLoc {
				e.a.MOVImm64(amd64.RAX, uint64(in.ImmI))
				e.storeG(loc, amd64.RAX)
			}
		case ir.OpConstFloat:
			if hasLoc {
				e.a.MOVImm64(amd64.RAX, in.ImmU)
				e.a.MOVQToX(amd64.XMM12, amd64.RAX)
				e.storeF(loc, amd64.XMM12)
			}
		case ir.OpConstBool:
			if hasLoc {
				e.a.MOVImm64(amd64.RAX, uint64(in.ImmI))
				e.storeG(loc, amd64.RAX)
			}
		case ir.OpUnboxInt:
			e.c.loadReg(int(in.Slot), amd64.RAX, amd64.RCX)
			e.a.TESTQ(amd64.RCX, amd64.RCX)
			e.a.JCC(amd64.NE, fail)
			if hasLoc {
				e.storeG(loc, amd64.RAX)
			}
		case ir.OpUnboxFloat:
			e.c.loadReg(int(in.Slot), amd64.RAX, amd64.RCX)
			e.a.CMPQImm(amd64.RCX, int32(tagFloat))
			e.a.JCC(amd64.NE, fail)
			if hasLoc {
				e.a.MOVQToX(amd64.XMM12, amd64.RAX)
				e.storeF(loc, amd64.XMM12)
			}
		case ir.OpUnboxNumFloat:
			e.c.loadReg(int(in.Slot), amd64.RAX, amd64.RCX)
			lFloat := e.a.NewLabel()
			lDone := e.a.NewLabel()
			e.a.CMPQImm(amd64.RCX, int32(tagFloat))
			e.a.JCC(amd64.E, lFloat)
			e.a.TESTQ(amd64.RCX, amd64.RCX)
			e.a.JCC(amd64.NE, fail)
			if hasLoc {
				e.a.CVTSI2SDQ(amd64.XMM12, amd64.RAX)
				e.storeF(loc, amd64.XMM12)
			}
			e.a.JMP(lDone)
			e.a.Bind(lFloat)
			if hasLoc {
				e.a.MOVQToX(amd64.XMM12, amd64.RAX)
				e.storeF(loc, amd64.XMM12)
			}
			e.a.Bind(lDone)
		case ir.OpUnboxBool:
			e.c.loadReg(int(in.Slot), amd64.RAX, amd64.RCX)
			e.a.SUBQImm(amd64.RCX, int32(tagFalse))
			e.a.CMPQImm(amd64.RCX, 1)
			e.a.JCC(amd64.A, fail)
			if hasLoc {
				e.storeG(loc, amd64.RCX)
			}
		case ir.OpGuardUpvalInt:
			e.emitLoadUpvalPre(int(in.AuxIdx), amd64.RAX, amd64.RCX, fail)
			e.a.TESTQ(amd64.RCX, amd64.RCX)
			e.a.JCC(amd64.NE, fail)
			if hasLoc {
				e.storeG(loc, amd64.RAX)
			}
		case ir.OpGuardUpvalFlt:
			e.emitLoadUpvalPre(int(in.AuxIdx), amd64.RAX, amd64.RCX, fail)
			e.a.CMPQImm(amd64.RCX, int32(tagFloat))
			e.a.JCC(amd64.NE, fail)
			if hasLoc {
				e.a.MOVQToX(amd64.XMM12, amd64.RAX)
				e.storeF(loc, amd64.XMM12)
			}
		case ir.OpGuardUpvalSqrt:
			e.emitLoadUpvalPre(int(in.AuxIdx), amd64.RAX, amd64.RCX, fail)
			e.a.CMPQImm(amd64.RCX, int32(tagStaticNative))
			e.a.JCC(amd64.NE, fail)
			e.a.MOVLLoad(amd64.RDX, amd64.BaseDisp(x64Ctx, jitCtxSqrtIdx))
			e.a.TESTL(amd64.RDX, amd64.RDX)
			e.a.JCC(amd64.E, fail)
			e.a.CMPQ(amd64.RAX, amd64.RDX)
			e.a.JCC(amd64.NE, fail)
		case ir.OpGuardUpvalAbs:
			e.emitLoadUpvalPre(int(in.AuxIdx), amd64.RAX, amd64.RCX, fail)
			e.a.CMPQImm(amd64.RCX, int32(tagStaticNative))
			e.a.JCC(amd64.NE, fail)
			e.a.MOVLLoad(amd64.RDX, amd64.BaseDisp(x64Ctx, jitCtxAbsIdx))
			e.a.TESTL(amd64.RDX, amd64.RDX)
			e.a.JCC(amd64.E, fail)
			e.a.CMPQ(amd64.RAX, amd64.RDX)
			e.a.JCC(amd64.NE, fail)
		case ir.OpGuardRegSqrt:
			e.c.loadReg(int(in.Slot), amd64.RAX, amd64.RCX)
			e.a.CMPQImm(amd64.RCX, int32(tagStaticNative))
			e.a.JCC(amd64.NE, fail)
			e.a.MOVLLoad(amd64.RDX, amd64.BaseDisp(x64Ctx, jitCtxSqrtIdx))
			e.a.TESTL(amd64.RDX, amd64.RDX)
			e.a.JCC(amd64.E, fail)
			e.a.CMPQ(amd64.RAX, amd64.RDX)
			e.a.JCC(amd64.NE, fail)
		case ir.OpGuardRegAbs:
			e.c.loadReg(int(in.Slot), amd64.RAX, amd64.RCX)
			e.a.CMPQImm(amd64.RCX, int32(tagStaticNative))
			e.a.JCC(amd64.NE, fail)
			e.a.MOVLLoad(amd64.RDX, amd64.BaseDisp(x64Ctx, jitCtxAbsIdx))
			e.a.TESTL(amd64.RDX, amd64.RDX)
			e.a.JCC(amd64.E, fail)
			e.a.CMPQ(amd64.RAX, amd64.RDX)
			e.a.JCC(amd64.NE, fail)
		case ir.OpGuardCalleePro:
			if in.UpvalIdx >= 0 {
				e.emitLoadUpvalPre(int(in.UpvalIdx), amd64.RAX, amd64.RCX, fail)
			} else {
				e.c.loadReg(int(in.Slot), amd64.RAX, amd64.RCX)
			}
			e.a.CMPQImm(amd64.RCX, int32(tagClosure))
			e.a.JCC(amd64.NE, fail)
			e.c.emitResolveSmall(amd64.RAX, amd64.RAX, amd64.RDX)
			e.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(amd64.RAX, jitClProtoIdx))
			e.a.MOVImm64(amd64.RDX, uint64(in.AuxU32))
			e.a.CMPQ(amd64.RCX, amd64.RDX)
			e.a.JCC(amd64.NE, fail)
		case ir.OpGuardTableArr:
			if in.UpvalIdx >= 0 {
				e.emitLoadUpvalPre(int(in.UpvalIdx), amd64.RAX, amd64.RCX, fail)
			} else {
				e.c.loadReg(int(in.Slot), amd64.RAX, amd64.RCX)
			}
			e.a.CMPQImm(amd64.RCX, int32(tagTable))
			e.a.JCC(amd64.NE, fail)
			e.c.emitResolveSmall(amd64.RAX, amd64.RCX, amd64.RDX)
			e.a.CMPBMemImm(amd64.BaseDisp(amd64.RCX, jitTblArrKind), in.Type.ExpectedArrKind())
			e.a.JCC(amd64.NE, fail)
			e.a.MOVLLoad(amd64.RDX, amd64.BaseDisp(amd64.RCX, jitTblArrLen))
			e.a.TESTL(amd64.RDX, amd64.RDX)
			e.a.JCC(amd64.E, fail)
			lenLoc := e.alloc.LenLoc[in.ID]
			if lenLoc.Phys >= 0 || lenLoc.Spill >= 0 {
				e.storeG(lenLoc, amd64.RDX)
			}
			e.a.MOVLLoad(amd64.RAX, amd64.BaseDisp(amd64.RCX, jitTblArrRef))
			e.a.TESTL(amd64.RAX, amd64.RAX)
			e.a.JCC(amd64.E, fail)
			e.c.emitResolveFast(amd64.RAX, amd64.RAX, amd64.RDX)
			if hasLoc {
				e.storeG(loc, amd64.RAX)
			}
		case ir.OpGuardRange:
			e.emitGuardRange(in, fail)
		}
	}
}

func (e *x64OptEmit) emitLoadUpvalPre(uvIdx int, bitsReg, tagReg amd64.Reg, fail amd64.Label) {
	e.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(x64Ctx, jitCtxClUpvals))
	e.a.TESTQ(amd64.RDX, amd64.RDX)
	e.a.JCC(amd64.E, fail)
	e.a.MOVLLoad(amd64.RDX, amd64.BaseDisp(amd64.RDX, int32(uvIdx*4)))
	e.c.emitResolveSmall(amd64.RDX, amd64.RDX, bitsReg)
	e.a.MOVLLoad(tagReg, amd64.BaseDisp(amd64.RDX, jitUvOwnerRef))
	lOpen := e.a.NewLabel()
	lDone := e.a.NewLabel()
	e.a.TESTL(tagReg, tagReg)
	e.a.JCC(amd64.NE, lOpen)
	e.a.MOVQLoad(bitsReg, amd64.BaseDisp(amd64.RDX, jitUvClosed))
	e.a.MOVQLoad(tagReg, amd64.BaseDisp(amd64.RDX, jitUvClosed+8))
	e.a.JMP(lDone)
	e.a.Bind(lOpen)
	e.a.MOVLLoad(bitsReg, amd64.BaseDisp(x64Ctx, jitCtxThSelf))
	e.a.CMPL(tagReg, bitsReg)
	e.a.JCC(amd64.NE, fail)
	e.a.MOVQLoad(bitsReg, amd64.BaseDisp(amd64.RDX, jitUvStackIdx))
	e.a.SHLQImm(bitsReg, 4)
	e.a.MOVQLoad(amd64.RDX, amd64.BaseDisp(x64Ctx, jitCtxStack))
	e.a.ADDQ(amd64.RDX, bitsReg)
	e.a.MOVQLoad(bitsReg, amd64.BaseDisp(amd64.RDX, 0))
	e.a.MOVQLoad(tagReg, amd64.BaseDisp(amd64.RDX, 8))
	e.a.Bind(lDone)
}

func (e *x64OptEmit) emitGuardRange(in *ir.Inst, fail amd64.Label) {
	arrLen := e.loadG(e.alloc.LenLoc[in.Args[0]], amd64.RCX)
	if arrLen != amd64.RCX {
		e.a.MOVQ(amd64.RCX, arrLen)
	}
	initR := e.loadG(e.alloc.VarLoc[e.idxVar], amd64.RAX)
	if initR != amd64.RAX {
		e.a.MOVQ(amd64.RAX, initR)
	}
	if in.NumArgs >= 2 && in.Args[1] != ir.NoVal {
		offR := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RDX)
		e.a.ADDQ(amd64.RAX, offR)
	}
	e.a.MOVQ(amd64.RDX, amd64.RAX)
	e.a.ADDQImm(amd64.RAX, int32(in.ImmI-1))
	e.a.CMPQ(amd64.RAX, amd64.RCX)
	e.a.JCC(amd64.AE, fail)

	e.a.ADDQImm(amd64.RDX, int32(in.ImmI2-1))
	if in.ImmI2 != in.ImmI {
		e.a.CMPQ(amd64.RDX, amd64.RCX)
		e.a.JCC(amd64.AE, fail)
	}

	if e.reg.HasConstStep && e.reg.ConstStep == 1 {
		e.a.SUBQ(amd64.RCX, amd64.RDX)
		cntR := e.loadG(e.alloc.ValLoc[e.cntVal], amd64.RAX)
		e.a.CMPQ(cntR, amd64.RCX)
		e.a.JCC(amd64.AE, fail)
		return
	}
	e.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxOptOuterRem), amd64.RAX)
	cntR := e.loadG(e.alloc.ValLoc[e.cntVal], amd64.RAX)
	e.a.CMPQImm(cntR, 0x7FFFFFFF)
	e.a.JCC(amd64.A, fail)
	if e.reg.HasConstStep {
		e.a.MOVImm64(amd64.RAX, uint64(e.reg.ConstStep))
	} else {
		stR := e.loadG(e.alloc.ValLoc[e.stepVal], amd64.RAX)
		if stR != amd64.RAX {
			e.a.MOVQ(amd64.RAX, stR)
		}
	}
	lNeg := e.a.NewLabel()
	lDone := e.a.NewLabel()
	e.a.CMPQImm(amd64.RAX, 0)
	e.a.JCC(amd64.L, lNeg)
	e.a.SUBQ(amd64.RCX, amd64.RDX)
	e.a.CMPQImm(amd64.RAX, 0x7FFFFFFF)
	e.a.JCC(amd64.A, fail)
	cntR2 := e.loadG(e.alloc.ValLoc[e.cntVal], amd64.RDX)
	e.a.IMULQ(amd64.RAX, cntR2)
	e.a.CMPQ(amd64.RAX, amd64.RCX)
	e.a.JCC(amd64.AE, fail)
	e.a.JMP(lDone)

	e.a.Bind(lNeg)
	e.a.NEGQ(amd64.RAX)
	e.a.CMPQImm(amd64.RAX, 0x7FFFFFFF)
	e.a.JCC(amd64.A, fail)
	cntR3 := e.loadG(e.alloc.ValLoc[e.cntVal], amd64.RDX)
	e.a.IMULQ(amd64.RAX, cntR3)
	e.a.CMPQMem(amd64.RAX, amd64.BaseDisp(x64Ctx, jitCtxOptOuterRem))
	e.a.JCC(amd64.A, fail)
	e.a.Bind(lDone)
}

// --- 2-Wide AMD64 SSE2 SIMD Main Loop ---

func (e *x64OptEmit) emitSSE2VecLoop(lScalar, lChunkDone amd64.Label) {
	plan := e.reg.Vec
	usedFP := make(map[amd64.XReg]bool)
	for i, lv := range e.reg.Vars {
		if lv.Type.IsFPReg() && e.alloc.VarLoc[i].Phys >= 0 {
			usedFP[x64OptFPs[e.alloc.VarLoc[i].Phys]] = true
		}
	}
	for i := range e.reg.Preheader {
		loc := e.alloc.ValLoc[e.reg.Preheader[i].ID]
		if loc.IsFP && loc.Phys >= 0 {
			usedFP[x64OptFPs[loc.Phys]] = true
		}
	}
	var freeV []amd64.XReg
	for _, r := range x64AllScratchFPs {
		if !usedFP[r] {
			freeV = append(freeV, r)
		}
	}
	// Reserve 1 extra XMM scratch (XMM14 is always reserved outside x64AllScratchFPs!).
	if len(freeV) < plan.NumVRegs {
		return
	}
	usedGPR := make(map[amd64.Reg]bool)
	for i, lv := range e.reg.Vars {
		if !lv.Type.IsFPReg() && e.alloc.VarLoc[i].Phys >= 0 {
			usedGPR[x64OptGPRs[e.alloc.VarLoc[i].Phys]] = true
		}
	}
	for i := range e.reg.Preheader {
		loc := e.alloc.ValLoc[e.reg.Preheader[i].ID]
		if !loc.IsFP && loc.Phys >= 0 {
			usedGPR[x64OptGPRs[loc.Phys]] = true
		}
		lloc := e.alloc.LenLoc[e.reg.Preheader[i].ID]
		if lloc.Phys >= 0 {
			usedGPR[x64OptGPRs[lloc.Phys]] = true
		}
	}
	freeG := []amd64.Reg{amd64.RAX, amd64.RDX}
	for _, r := range x64OptGPRs {
		if !usedGPR[r] {
			freeG = append(freeG, r)
		}
	}
	if len(freeG) < len(plan.Ptrs) {
		return
	}
	vreg := func(vr int) amd64.XReg { return freeV[vr] }
	ptrRegs := freeG[:len(plan.Ptrs)]

	remR := e.loadG(e.alloc.ValLoc[e.cntVal], amd64.RAX)
	e.a.CMPQImm(remR, 2)
	e.a.JCC(amd64.L, lScalar)

	// Runtime alias checks before initializing ptrRegs.
	for _, ac := range plan.AliasChecks {
		idxR := e.loadG(e.alloc.VarLoc[e.idxVar], amd64.RCX)
		e.emitVecElemPtr(amd64.RAX, ac.DstArr, ac.DstOff, ac.DstDel, idxR, amd64.RCX)
		e.a.MOVQToX(amd64.XMM14, amd64.RAX)
		idxR2 := e.loadG(e.alloc.VarLoc[e.idxVar], amd64.RCX)
		e.emitVecElemPtr(amd64.RDX, ac.SrcArr, ac.SrcOff, ac.SrcDel, idxR2, amd64.RCX)
		e.a.MOVQFromX(amd64.RAX, amd64.XMM14)
		lOK := e.a.NewLabel()
		e.a.CMPQ(amd64.RAX, amd64.RDX)
		e.a.JCC(amd64.E, lOK)
		remCur := e.loadG(e.alloc.ValLoc[e.cntVal], amd64.RCX)
		if remCur != amd64.RCX {
			e.a.MOVQ(amd64.RCX, remCur)
		}
		e.a.SHLQImm(amd64.RCX, 3)
		e.a.ADDQ(amd64.RAX, amd64.RCX)
		e.a.CMPQ(amd64.RAX, amd64.RDX)
		e.a.JCC(amd64.BE, lOK)
		e.a.SUBQ(amd64.RAX, amd64.RCX)
		e.a.ADDQ(amd64.RDX, amd64.RCX)
		e.a.CMPQ(amd64.RDX, amd64.RAX)
		e.a.JCC(amd64.BE, lOK)
		e.a.JMP(lScalar)
		e.a.Bind(lOK)
	}

	// Broadcast splats.
	for _, sp := range plan.Splats {
		vd := vreg(sp.VReg)
		if sp.IsConst {
			e.a.MOVImm64(amd64.RAX, sp.ConstBits)
			e.a.MOVQToX(vd, amd64.RAX)
			if sp.IsFloat {
				e.a.UNPCKLPD(vd, vd)
			} else {
				e.a.PUNPCKLQDQ(vd, vd)
			}
			continue
		}
		sloc := e.alloc.ValLoc[sp.ScalarVal]
		if sp.IsFloat {
			sf := e.loadF(sloc, amd64.XMM14)
			if vd != sf {
				e.a.MOVSD(vd, sf)
			}
			e.a.UNPCKLPD(vd, vd)
		} else {
			sr := e.loadG(sloc, amd64.RAX)
			e.a.MOVQToX(vd, sr)
			e.a.PUNPCKLQDQ(vd, vd)
		}
	}

	for _, acc := range plan.IntAccums {
		e.a.PXOR(vreg(acc.VReg), vreg(acc.VReg))
	}

	// Initialize VecPtrs in reverse so RAX/RDX (indices 0,1) are written last after scratch use!
	for i := len(plan.Ptrs) - 1; i >= 0; i-- {
		p := plan.Ptrs[i]
		idxR := e.loadG(e.alloc.VarLoc[e.idxVar], amd64.RCX)
		e.emitVecElemPtr(ptrRegs[i], p.ArrVal, p.BaseOff, p.Delta, idxR, amd64.RCX)
	}

	// Load pairs = rem >> 1 into RCX and save into XMM14 (Wait: VAccumF64/VNegF64/VAbsF64 may need XMM14!
	// Even better: compute doneElems = (rem &^ 1) and advance rem & idx BEFORE initializing ptrRegs!)
	// Actually we already initialized ptrRegs using original idx, so let's keep pairs in RCX and update rem/idx
	// AFTER the loop by saving original pairs in RAX? No, RAX may be ptrRegs[0].
	// Solution: recompute pairsAfter = rem >> 1 AFTER lVecLoop (rem wasn't mutated during lVecLoop!!).
	remCur := e.loadG(e.alloc.ValLoc[e.cntVal], amd64.RCX)
	if remCur != amd64.RCX {
		e.a.MOVQ(amd64.RCX, remCur)
	}
	e.a.SHRQImm(amd64.RCX, 1)

	lVecLoop := e.a.NewLabel()
	e.a.Bind(lVecLoop)
	emitBinF := func(vi ir.VecInst, fn func(d, s amd64.XReg)) {
		d, s1, s2 := vreg(vi.Dst), vreg(vi.Src1), vreg(vi.Src2)
		if d != s1 {
			e.a.MOVAPD(d, s1)
		}
		fn(d, s2)
	}
	emitBinI := func(vi ir.VecInst, fn func(d, s amd64.XReg)) {
		d, s1, s2 := vreg(vi.Dst), vreg(vi.Src1), vreg(vi.Src2)
		if d != s1 {
			e.a.MOVDQA(d, s1)
		}
		fn(d, s2)
	}
	for _, vi := range plan.Body {
		switch vi.Op {
		case ir.VLoadF64:
			e.a.MOVUPDLoad(vreg(vi.Dst), amd64.BaseDisp(ptrRegs[vi.Src1], 0))
		case ir.VLoadI64:
			e.a.MOVDQULoad(vreg(vi.Dst), amd64.BaseDisp(ptrRegs[vi.Src1], 0))
		case ir.VStoreF64:
			e.a.MOVUPDStore(amd64.BaseDisp(ptrRegs[vi.Src1], 0), vreg(vi.Src2))
		case ir.VStoreI64:
			e.a.MOVDQUStore(amd64.BaseDisp(ptrRegs[vi.Src1], 0), vreg(vi.Src2))
		case ir.VAddF64:
			emitBinF(vi, func(d, s amd64.XReg) { e.a.ADDPD(d, s) })
		case ir.VSubF64:
			emitBinF(vi, func(d, s amd64.XReg) { e.a.SUBPD(d, s) })
		case ir.VMulF64:
			emitBinF(vi, func(d, s amd64.XReg) { e.a.MULPD(d, s) })
		case ir.VDivF64:
			emitBinF(vi, func(d, s amd64.XReg) { e.a.DIVPD(d, s) })
		case ir.VNegF64:
			e.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxOptVecTmp), amd64.RCX)
			e.a.MOVImm64(amd64.RCX, 0x8000000000000000)
			e.a.MOVQToX(amd64.XMM14, amd64.RCX)
			e.a.MOVQLoad(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxOptVecTmp))
			e.a.UNPCKLPD(amd64.XMM14, amd64.XMM14)
			if vreg(vi.Dst) != vreg(vi.Src1) {
				e.a.MOVAPD(vreg(vi.Dst), vreg(vi.Src1))
			}
			e.a.XORPD(vreg(vi.Dst), amd64.XMM14)
		case ir.VSqrtF64:
			e.a.SQRTPD(vreg(vi.Dst), vreg(vi.Src1))
		case ir.VAbsF64:
			e.a.MOVQStore(amd64.BaseDisp(x64Ctx, jitCtxOptVecTmp), amd64.RCX)
			e.a.MOVImm64(amd64.RCX, 0x7FFFFFFFFFFFFFFF)
			e.a.MOVQToX(amd64.XMM14, amd64.RCX)
			e.a.MOVQLoad(amd64.RCX, amd64.BaseDisp(x64Ctx, jitCtxOptVecTmp))
			e.a.UNPCKLPD(amd64.XMM14, amd64.XMM14)
			if vreg(vi.Dst) != vreg(vi.Src1) {
				e.a.MOVAPD(vreg(vi.Dst), vreg(vi.Src1))
			}
			e.a.ANDPD(vreg(vi.Dst), amd64.XMM14)
		case ir.VAddI64:
			emitBinI(vi, func(d, s amd64.XReg) { e.a.PADDQ(d, s) })
		case ir.VSubI64:
			emitBinI(vi, func(d, s amd64.XReg) { e.a.PSUBQ(d, s) })
		case ir.VAndI64:
			emitBinI(vi, func(d, s amd64.XReg) { e.a.PAND(d, s) })
		case ir.VOrI64:
			emitBinI(vi, func(d, s amd64.XReg) { e.a.POR(d, s) })
		case ir.VXorI64:
			emitBinI(vi, func(d, s amd64.XReg) { e.a.PXOR(d, s) })
		case ir.VAccumF64:
			// Exact left-to-right IEEE-754 order: acc = (acc + lane0) + lane1!
			vloc := e.alloc.VarLoc[vi.VarIdx]
			srcV := vreg(vi.Src1)
			if vloc.Phys >= 0 {
				accF := x64OptFPs[vloc.Phys]
				e.a.ADDSD(accF, srcV)
				e.a.MOVAPD(amd64.XMM14, srcV)
				e.a.UNPCKHPD(amd64.XMM14, amd64.XMM14)
				e.a.ADDSD(accF, amd64.XMM14)
			} else {
				e.a.MOVSDLoad(amd64.XMM14, e.spillMem(vloc.Spill))
				e.a.ADDSD(amd64.XMM14, srcV)
				e.a.MOVSDStore(e.spillMem(vloc.Spill), amd64.XMM14)
				e.a.MOVAPD(amd64.XMM14, srcV)
				e.a.UNPCKHPD(amd64.XMM14, amd64.XMM14)
				e.a.ADDSDMem(amd64.XMM14, e.spillMem(vloc.Spill))
				e.a.MOVSDStore(e.spillMem(vloc.Spill), amd64.XMM14)
			}
		case ir.VAccumI64:
			e.a.PADDQ(vreg(vi.Dst), vreg(vi.Src1))
		}
	}
	for _, pr := range ptrRegs {
		e.a.ADDQImm(pr, 16)
	}
	e.a.SUBQImm(amd64.RCX, 1)
	e.a.JCC(amd64.NE, lVecLoop)

	// Now all ptrRegs are dead; RAX, RCX, RDX are 100% free again!
	// Fold 2-wide int64 accumulators horizontally into their scalar LoopVars.
	for _, acc := range plan.IntAccums {
		av := vreg(acc.VReg)
		e.a.MOVDQA(amd64.XMM14, av)
		e.a.PUNPCKHQDQ(amd64.XMM14, amd64.XMM14)
		e.a.PADDQ(amd64.XMM14, av)
		e.a.MOVQFromX(amd64.RAX, amd64.XMM14)
		vloc := e.alloc.VarLoc[acc.VarIdx]
		cur := e.loadG(vloc, amd64.RDX)
		e.a.ADDQ(cur, amd64.RAX)
		e.storeG(vloc, cur)
	}

	// Advance idx += (rem &^ 1) and update rem = rem & 1!
	remEnd := e.loadG(e.alloc.ValLoc[e.cntVal], amd64.RAX)
	e.a.MOVQ(amd64.RCX, remEnd)
	e.a.ANDQImm(amd64.RCX, -2) // doneElems = rem &^ 1
	idxDst := e.loadG(e.alloc.VarLoc[e.idxVar], amd64.RDX)
	e.a.ADDQ(idxDst, amd64.RCX)
	e.storeG(e.alloc.VarLoc[e.idxVar], idxDst)

	e.a.MOVQ(amd64.RAX, remEnd)
	e.a.ANDQImm(amd64.RAX, 1) // tail rem (0 or 1)
	e.storeG(e.alloc.ValLoc[e.cntVal], amd64.RAX)
	e.a.TESTQ(amd64.RAX, amd64.RAX)
	e.a.JCC(amd64.E, lChunkDone)
}

func (e *x64OptEmit) emitVecElemPtr(dst amd64.Reg, arrVal, baseOff ir.ValID, delta int64, idxReg, tmp amd64.Reg) {
	// dst = arrBase + (baseOff + idx + delta - 1) * 8
	if tmp != idxReg {
		e.a.MOVQ(tmp, idxReg)
	}
	if baseOff != ir.NoVal {
		offR := e.loadG(e.alloc.ValLoc[baseOff], dst)
		e.a.ADDQ(tmp, offR)
	}
	if adj := delta - 1; adj != 0 {
		e.a.ADDQImm(tmp, int32(adj))
	}
	baseR := e.loadG(e.alloc.ValLoc[arrVal], dst)
	e.a.LEAQ(dst, amd64.Indexed(baseR, tmp, 8, 0))
}

// --- Scalar Body Lowering ---

func (e *x64OptEmit) emitBlockInsts(blk *ir.Block) {
	for i := range blk.Insts {
		in := &blk.Insts[i]
		if in.Dead {
			continue
		}
		loc := e.alloc.ValLoc[in.ID]
		switch in.Op {
		case ir.OpConstInt:
			e.a.MOVImm64(amd64.RAX, uint64(in.ImmI))
			e.storeG(loc, amd64.RAX)
		case ir.OpConstFloat:
			e.a.MOVImm64(amd64.RAX, in.ImmU)
			e.a.MOVQToX(amd64.XMM12, amd64.RAX)
			e.storeF(loc, amd64.XMM12)
		case ir.OpConstBool:
			e.a.MOVImm64(amd64.RAX, uint64(in.ImmI))
			e.storeG(loc, amd64.RAX)

		case ir.OpGetVar:
			vloc := e.alloc.VarLoc[in.AuxIdx]
			if vloc == loc {
				continue
			}
			if in.Type.IsFPReg() {
				e.storeF(loc, e.loadF(vloc, amd64.XMM12))
			} else {
				e.storeG(loc, e.loadG(vloc, amd64.RAX))
			}

		case ir.OpSetVar:
			vloc := e.alloc.VarLoc[in.AuxIdx]
			if in.Type.IsFPReg() {
				e.storeF(vloc, e.loadF(e.alloc.ValLoc[in.Args[0]], amd64.XMM12))
			} else {
				e.storeG(vloc, e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX))
			}

		case ir.OpAddI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			e.a.LEAQ(amd64.RAX, amd64.Indexed(l, r, 1, 0))
			e.storeG(loc, amd64.RAX)
		case ir.OpSubI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			if l != amd64.RAX {
				e.a.MOVQ(amd64.RAX, l)
			}
			e.a.SUBQ(amd64.RAX, r)
			e.storeG(loc, amd64.RAX)
		case ir.OpMulI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			if l != amd64.RAX {
				e.a.MOVQ(amd64.RAX, l)
			}
			e.a.IMULQ(amd64.RAX, r)
			e.storeG(loc, amd64.RAX)
		case ir.OpAddIImm:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			if in.ImmI >= -0x80000000 && in.ImmI <= 0x7fffffff {
				e.a.LEAQ(amd64.RAX, amd64.BaseDisp(l, int32(in.ImmI)))
			} else {
				e.a.MOVImm64(amd64.RCX, uint64(in.ImmI))
				e.a.LEAQ(amd64.RAX, amd64.Indexed(l, amd64.RCX, 1, 0))
			}
			e.storeG(loc, amd64.RAX)
		case ir.OpIDivI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			if r != amd64.RCX {
				e.a.MOVQ(amd64.RCX, r)
			}
			if l != amd64.RAX {
				e.a.MOVQ(amd64.RAX, l)
			}
			e.a.TESTQ(amd64.RCX, amd64.RCX)
			e.a.JCC(amd64.E, e.deopt)
			// Guard MinInt64 // -1 (x86-64 IDIV traps with #DE!)
			lNotNeg1 := e.a.NewLabel()
			lDone := e.a.NewLabel()
			e.a.CMPQImm(amd64.RCX, -1)
			e.a.JCC(amd64.NE, lNotNeg1)
			e.a.NEGQ(amd64.RAX)
			e.a.JMP(lDone)
			e.a.Bind(lNotNeg1)
			e.a.CQO()
			e.a.IDIVQ(amd64.RCX) // RAX = q, RDX = rem
			e.a.TESTQ(amd64.RDX, amd64.RDX)
			e.a.JCC(amd64.E, lDone)
			e.a.XORQ(amd64.RDX, amd64.RCX)
			e.a.JCC(amd64.NS, lDone)
			e.a.SUBQImm(amd64.RAX, 1)
			e.a.Bind(lDone)
			e.storeG(loc, amd64.RAX)
		case ir.OpModI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			if r != amd64.RCX {
				e.a.MOVQ(amd64.RCX, r)
			}
			if l != amd64.RAX {
				e.a.MOVQ(amd64.RAX, l)
			}
			e.a.TESTQ(amd64.RCX, amd64.RCX)
			e.a.JCC(amd64.E, e.deopt)
			lNotNeg1 := e.a.NewLabel()
			lDone := e.a.NewLabel()
			e.a.CMPQImm(amd64.RCX, -1)
			e.a.JCC(amd64.NE, lNotNeg1)
			e.a.XORL(amd64.RDX, amd64.RDX)
			e.a.JMP(lDone)
			e.a.Bind(lNotNeg1)
			e.a.CQO()
			e.a.IDIVQ(amd64.RCX) // RDX = rem
			e.a.TESTQ(amd64.RDX, amd64.RDX)
			e.a.JCC(amd64.E, lDone)
			e.a.MOVQ(amd64.RAX, amd64.RDX)
			e.a.XORQ(amd64.RAX, amd64.RCX)
			e.a.JCC(amd64.NS, lDone)
			e.a.ADDQ(amd64.RDX, amd64.RCX)
			e.a.Bind(lDone)
			e.storeG(loc, amd64.RDX)
		case ir.OpNegI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			if l != amd64.RAX {
				e.a.MOVQ(amd64.RAX, l)
			}
			e.a.NEGQ(amd64.RAX)
			e.storeG(loc, amd64.RAX)
		case ir.OpBNotI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			if l != amd64.RAX {
				e.a.MOVQ(amd64.RAX, l)
			}
			e.a.NOTQ(amd64.RAX)
			e.storeG(loc, amd64.RAX)
		case ir.OpAndI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			if l != amd64.RAX {
				e.a.MOVQ(amd64.RAX, l)
			}
			e.a.ANDQ(amd64.RAX, r)
			e.storeG(loc, amd64.RAX)
		case ir.OpOrI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			if l != amd64.RAX {
				e.a.MOVQ(amd64.RAX, l)
			}
			e.a.ORQ(amd64.RAX, r)
			e.storeG(loc, amd64.RAX)
		case ir.OpXorI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			if l != amd64.RAX {
				e.a.MOVQ(amd64.RAX, l)
			}
			e.a.XORQ(amd64.RAX, r)
			e.storeG(loc, amd64.RAX)
		case ir.OpShlI, ir.OpShrI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			e.emitLuaShift(in.Op == ir.OpShlI, l, r, loc)
		case ir.OpIntToFloat:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			e.a.CVTSI2SDQ(amd64.XMM12, l)
			e.storeF(loc, amd64.XMM12)
		case ir.OpAbsI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			if l != amd64.RAX {
				e.a.MOVQ(amd64.RAX, l)
			}
			e.a.MOVQ(amd64.RCX, amd64.RAX)
			e.a.NEGQ(amd64.RCX)
			e.a.TESTQ(amd64.RAX, amd64.RAX)
			e.a.CMOVQ(amd64.L, amd64.RAX, amd64.RCX)
			e.storeG(loc, amd64.RAX)

		case ir.OpAddF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], amd64.XMM12)
			r := e.loadF(e.alloc.ValLoc[in.Args[1]], amd64.XMM13)
			if l != amd64.XMM12 {
				e.a.MOVSD(amd64.XMM12, l)
			}
			e.a.ADDSD(amd64.XMM12, r)
			e.storeF(loc, amd64.XMM12)
		case ir.OpSubF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], amd64.XMM12)
			r := e.loadF(e.alloc.ValLoc[in.Args[1]], amd64.XMM13)
			if l != amd64.XMM12 {
				e.a.MOVSD(amd64.XMM12, l)
			}
			e.a.SUBSD(amd64.XMM12, r)
			e.storeF(loc, amd64.XMM12)
		case ir.OpMulF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], amd64.XMM12)
			r := e.loadF(e.alloc.ValLoc[in.Args[1]], amd64.XMM13)
			if l != amd64.XMM12 {
				e.a.MOVSD(amd64.XMM12, l)
			}
			e.a.MULSD(amd64.XMM12, r)
			e.storeF(loc, amd64.XMM12)
		case ir.OpDivF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], amd64.XMM12)
			r := e.loadF(e.alloc.ValLoc[in.Args[1]], amd64.XMM13)
			if l != amd64.XMM12 {
				e.a.MOVSD(amd64.XMM12, l)
			}
			e.a.DIVSD(amd64.XMM12, r)
			e.storeF(loc, amd64.XMM12)
		case ir.OpNegF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], amd64.XMM12)
			if l != amd64.XMM12 {
				e.a.MOVSD(amd64.XMM12, l)
			}
			e.a.MOVImm64(amd64.RAX, 0x8000000000000000)
			e.a.MOVQToX(amd64.XMM13, amd64.RAX)
			e.a.XORPD(amd64.XMM12, amd64.XMM13)
			e.storeF(loc, amd64.XMM12)
		case ir.OpSqrtF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], amd64.XMM12)
			e.a.SQRTSD(amd64.XMM12, l)
			e.storeF(loc, amd64.XMM12)
		case ir.OpAbsF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], amd64.XMM12)
			if l != amd64.XMM12 {
				e.a.MOVSD(amd64.XMM12, l)
			}
			e.a.MOVImm64(amd64.RAX, 0x7FFFFFFFFFFFFFFF)
			e.a.MOVQToX(amd64.XMM13, amd64.RAX)
			e.a.ANDPD(amd64.XMM12, amd64.XMM13)
			e.storeF(loc, amd64.XMM12)

		case ir.OpNotB:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			if l != amd64.RAX {
				e.a.MOVQ(amd64.RAX, l)
			}
			e.a.XORQImm(amd64.RAX, 1)
			e.storeG(loc, amd64.RAX)
		case ir.OpCmpI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			e.a.CMPQ(l, r)
			e.a.SETCC(mapX64IntCond(in.AuxCond), amd64.RAX)
			e.a.MOVZXBQReg(amd64.RAX, amd64.RAX)
			e.storeG(loc, amd64.RAX)
		case ir.OpCmpF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], amd64.XMM12)
			r := e.loadF(e.alloc.ValLoc[in.Args[1]], amd64.XMM13)
			e.emitCmpF(in.AuxCond, l, r, loc)

		case ir.OpLoadArrInt:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			e.a.LEAQ(amd64.RCX, amd64.BaseDisp(idxR, -1))
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], amd64.RDX)
				e.a.CMPQ(amd64.RCX, lenR)
				e.a.JCC(amd64.AE, e.deopt)
			}
			e.a.MOVQLoad(amd64.RAX, amd64.Indexed(baseR, amd64.RCX, 8, 0))
			e.storeG(loc, amd64.RAX)

		case ir.OpLoadArrFloat:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			e.a.LEAQ(amd64.RCX, amd64.BaseDisp(idxR, -1))
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], amd64.RDX)
				e.a.CMPQ(amd64.RCX, lenR)
				e.a.JCC(amd64.AE, e.deopt)
			}
			e.a.MOVSDLoad(amd64.XMM12, amd64.Indexed(baseR, amd64.RCX, 8, 0))
			e.storeF(loc, amd64.XMM12)

		case ir.OpLoadArrBool:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			e.a.LEAQ(amd64.RCX, amd64.BaseDisp(idxR, -1))
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], amd64.RDX)
				e.a.CMPQ(amd64.RCX, lenR)
				e.a.JCC(amd64.AE, e.deopt)
			}
			e.a.MOVZXBL(amd64.RAX, amd64.Indexed(baseR, amd64.RCX, 1, 0))
			e.storeG(loc, amd64.RAX)

		case ir.OpLoadArrTable:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			e.a.LEAQ(amd64.RCX, amd64.BaseDisp(idxR, -1))
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], amd64.RDX)
				e.a.CMPQ(amd64.RCX, lenR)
				e.a.JCC(amd64.AE, e.deopt)
			}
			e.a.SHLQImm(amd64.RCX, 4)
			e.a.ADDQ(amd64.RCX, baseR)
			e.a.CMPQMemImm(amd64.BaseDisp(amd64.RCX, jitValTag), int32(tagTable))
			e.a.JCC(amd64.NE, e.deopt)
			e.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.RCX, jitValBits))
			e.c.emitResolveSmall(amd64.RAX, amd64.RAX, amd64.RDX)
			e.storeG(loc, amd64.RAX)

		case ir.OpStoreArrInt:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			e.a.LEAQ(amd64.RCX, amd64.BaseDisp(idxR, -1))
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], amd64.RDX)
				e.a.CMPQ(amd64.RCX, lenR)
				e.a.JCC(amd64.AE, e.deopt)
			}
			valR := e.loadG(e.alloc.ValLoc[in.Args[2]], amd64.RDX)
			e.a.MOVQStore(amd64.Indexed(baseR, amd64.RCX, 8, 0), valR)

		case ir.OpStoreArrFlt:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			e.a.LEAQ(amd64.RCX, amd64.BaseDisp(idxR, -1))
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], amd64.RDX)
				e.a.CMPQ(amd64.RCX, lenR)
				e.a.JCC(amd64.AE, e.deopt)
			}
			valF := e.loadF(e.alloc.ValLoc[in.Args[2]], amd64.XMM12)
			e.a.MOVSDStore(amd64.Indexed(baseR, amd64.RCX, 8, 0), valF)

		case ir.OpStoreArrBool:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			e.a.LEAQ(amd64.RCX, amd64.BaseDisp(idxR, -1))
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], amd64.RDX)
				e.a.CMPQ(amd64.RCX, lenR)
				e.a.JCC(amd64.AE, e.deopt)
			}
			valR := e.loadG(e.alloc.ValLoc[in.Args[2]], amd64.RDX)
			e.a.MOVBStore(amd64.Indexed(baseR, amd64.RCX, 1, 0), valR)

		case ir.OpGuardShape:
			tblR := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			e.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(tblR, jitTblShapeRef))
			e.a.MOVImm64(amd64.RDX, uint64(in.AuxU32))
			e.a.CMPQ(amd64.RCX, amd64.RDX)
			e.a.JCC(amd64.NE, e.deopt)
			e.a.MOVZXBL(amd64.RCX, amd64.BaseDisp(tblR, jitTblFlags))
			e.a.TESTQImm(amd64.RCX, 1)
			e.a.JCC(amd64.NE, e.deopt)
			if in.ImmI != 0 {
				// Reject tables with metatables before any OpStoreSlot* (preserves __newindex on nil own slots).
				e.a.CMPLMemImm(amd64.BaseDisp(tblR, jitTblMetaRef), 0)
				e.a.JCC(amd64.NE, e.deopt)
			}
			e.a.MOVLLoad(amd64.RCX, amd64.BaseDisp(tblR, jitTblSlotsRef))
			lExt := e.a.NewLabel()
			lDone := e.a.NewLabel()
			e.a.TESTL(amd64.RCX, amd64.RCX)
			e.a.JCC(amd64.NE, lExt)
			e.a.LEAQ(amd64.RAX, amd64.BaseDisp(tblR, jitTblInlineSlots))
			e.a.JMP(lDone)
			e.a.Bind(lExt)
			e.c.emitResolveSmall(amd64.RCX, amd64.RAX, amd64.RDX)
			e.a.Bind(lDone)
			e.storeG(loc, amd64.RAX)

		case ir.OpLoadSlotFloat:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			off := in.AuxIdx * 16
			// Require strict tagFloat (deopt on tagInt so plain field copies preserve integer values).
			e.a.CMPQMemImm(amd64.BaseDisp(baseR, off+jitValTag), int32(tagFloat))
			e.a.JCC(amd64.NE, e.deopt)
			e.a.MOVSDLoad(amd64.XMM12, amd64.BaseDisp(baseR, off+jitValBits))
			e.storeF(loc, amd64.XMM12)

		case ir.OpLoadSlotInt:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			off := in.AuxIdx * 16
			e.a.CMPQMemImm(amd64.BaseDisp(baseR, off+jitValTag), int32(tagInt))
			e.a.JCC(amd64.NE, e.deopt)
			e.a.MOVQLoad(amd64.RAX, amd64.BaseDisp(baseR, off+jitValBits))
			e.storeG(loc, amd64.RAX)

		case ir.OpStoreSlotFloat:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			valF := e.loadF(e.alloc.ValLoc[in.Args[1]], amd64.XMM12)
			off := in.AuxIdx * 16
			e.a.MOVSDStore(amd64.BaseDisp(baseR, off+jitValBits), valF)
			e.a.MOVQStoreImm32(amd64.BaseDisp(baseR, off+jitValTag), int32(tagFloat))

		case ir.OpStoreSlotInt:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], amd64.RAX)
			valR := e.loadG(e.alloc.ValLoc[in.Args[1]], amd64.RCX)
			off := in.AuxIdx * 16
			e.a.MOVQStore(amd64.BaseDisp(baseR, off+jitValBits), valR)
			e.a.MOVQStoreImm32(amd64.BaseDisp(baseR, off+jitValTag), int32(tagInt))
		}
	}
}

func (e *x64OptEmit) emitCmpF(cc ir.CmpCond, l, r amd64.XReg, loc ir.RegLoc) {
	// NaN-safe IEEE-754 float64 comparisons via UCOMISD.
	switch cc {
	case ir.CmpEQ:
		e.a.UCOMISD(l, r)
		e.a.SETCC(amd64.E, amd64.RAX)
		e.a.SETCC(amd64.NP, amd64.RCX)
		e.a.ANDQ(amd64.RAX, amd64.RCX)
		e.a.MOVZXBQReg(amd64.RAX, amd64.RAX)
	case ir.CmpNE:
		e.a.UCOMISD(l, r)
		e.a.SETCC(amd64.NE, amd64.RAX)
		e.a.SETCC(amd64.P, amd64.RCX)
		e.a.ORQ(amd64.RAX, amd64.RCX)
		e.a.MOVZXBQReg(amd64.RAX, amd64.RAX)
	case ir.CmpGT:
		e.a.UCOMISD(l, r)
		e.a.SETCC(amd64.A, amd64.RAX)
		e.a.MOVZXBQReg(amd64.RAX, amd64.RAX)
	case ir.CmpGE:
		e.a.UCOMISD(l, r)
		e.a.SETCC(amd64.AE, amd64.RAX)
		e.a.MOVZXBQReg(amd64.RAX, amd64.RAX)
	case ir.CmpLT:
		e.a.UCOMISD(r, l)
		e.a.SETCC(amd64.A, amd64.RAX)
		e.a.MOVZXBQReg(amd64.RAX, amd64.RAX)
	case ir.CmpLE:
		e.a.UCOMISD(r, l)
		e.a.SETCC(amd64.AE, amd64.RAX)
		e.a.MOVZXBQReg(amd64.RAX, amd64.RAX)
	}
	e.storeG(loc, amd64.RAX)
}

func (e *x64OptEmit) emitLuaShift(isLeft bool, valR, amtR amd64.Reg, loc ir.RegLoc) {
	if valR != amd64.RAX {
		e.a.MOVQ(amd64.RAX, valR)
	}
	if amtR != amd64.RCX {
		e.a.MOVQ(amd64.RCX, amtR)
	}
	lNeg := e.a.NewLabel()
	lZero := e.a.NewLabel()
	lDone := e.a.NewLabel()
	e.a.CMPQImm(amd64.RCX, 0)
	e.a.JCC(amd64.L, lNeg)
	e.a.CMPQImm(amd64.RCX, 64)
	e.a.JCC(amd64.GE, lZero)
	if isLeft {
		e.a.SHLQCL(amd64.RAX)
	} else {
		e.a.SHRQCL(amd64.RAX)
	}
	e.a.JMP(lDone)
	e.a.Bind(lNeg)
	e.a.NEGQ(amd64.RCX)
	e.a.CMPQImm(amd64.RCX, 64)
	e.a.JCC(amd64.AE, lZero)
	if isLeft {
		e.a.SHRQCL(amd64.RAX)
	} else {
		e.a.SHLQCL(amd64.RAX)
	}
	e.a.JMP(lDone)
	e.a.Bind(lZero)
	e.a.XORL(amd64.RAX, amd64.RAX)
	e.a.Bind(lDone)
	e.storeG(loc, amd64.RAX)
}

func (e *x64OptEmit) emitBlockTerm(blk *ir.Block, blockLabels []amd64.Label, lLoopTop, lChunkDone amd64.Label, isFor bool) {
	switch blk.Term.Kind {
	case ir.TermJump:
		e.a.JMP(blockLabels[blk.Term.Succ[0]])
	case ir.TermCondBr:
		cr := e.loadG(e.alloc.ValLoc[blk.Term.CondVal], amd64.RAX)
		e.a.TESTQ(cr, cr)
		e.a.JCC(amd64.NE, blockLabels[blk.Term.Succ[0]])
		e.a.JMP(blockLabels[blk.Term.Succ[1]])
	case ir.TermExit:
		e.emitMaterializeVars(true)
		e.a.JMP(e.c.pcLabels[blk.Term.ExitTargetPC])
	case ir.TermBackedge:
		if !isFor {
			e.a.JMP(lLoopTop)
			return
		}
		idxR := e.loadG(e.alloc.VarLoc[e.idxVar], amd64.RAX)
		if e.reg.HasConstStep {
			st := e.reg.ConstStep
			if st >= -0x80000000 && st <= 0x7fffffff {
				e.a.ADDQImm(idxR, int32(st))
			} else {
				e.a.MOVImm64(amd64.RCX, uint64(st))
				e.a.ADDQ(idxR, amd64.RCX)
			}
		} else {
			stepR := e.loadG(e.alloc.ValLoc[e.stepVal], amd64.RCX)
			e.a.ADDQ(idxR, stepR)
		}
		e.storeG(e.alloc.VarLoc[e.idxVar], idxR)

		remR := e.loadG(e.alloc.ValLoc[e.cntVal], amd64.RDX)
		e.a.SUBQImm(remR, 1)
		e.storeG(e.alloc.ValLoc[e.cntVal], remR)
		e.a.JCC(amd64.NE, lLoopTop)
		e.a.JMP(lChunkDone)
	}
}

func mapX64IntCond(c ir.CmpCond) amd64.Cond {
	switch c {
	case ir.CmpEQ:
		return amd64.E
	case ir.CmpNE:
		return amd64.NE
	case ir.CmpLT:
		return amd64.L
	case ir.CmpLE:
		return amd64.LE
	case ir.CmpGT:
		return amd64.G
	default:
		return amd64.GE
	}
}
