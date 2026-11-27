//go:build arm64 && !moeluanojit && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"github.com/yuchanns/moelua/internal/jit/arm64"
	"github.com/yuchanns/moelua/internal/jit/ir"
)

// Allocatable register banks for ARM64 Tier-2 optimizing loop IR.
// X19..X27 stay pinned to baseline jitExecCtx; X12..X17 are reserved emitter scratch GPRs.
// D8..D15 are AAPCS64 callee-saved and never touched; D24..D31 are reserved scratch/SIMD regs.
var (
	a64OptGPRs = [12]arm64.Reg{
		arm64.X0, arm64.X1, arm64.X2, arm64.X3,
		arm64.X4, arm64.X5, arm64.X6, arm64.X7,
		arm64.X8, arm64.X9, arm64.X10, arm64.X11,
	}
	a64OptFPs = [16]arm64.FReg{
		arm64.D0, arm64.D1, arm64.D2, arm64.D3,
		arm64.D4, arm64.D5, arm64.D6, arm64.D7,
		arm64.D16, arm64.D17, arm64.D18, arm64.D19,
		arm64.D20, arm64.D21, arm64.D22, arm64.D23,
	}
	a64AllScratchFPs = [24]arm64.FReg{
		arm64.D0, arm64.D1, arm64.D2, arm64.D3,
		arm64.D4, arm64.D5, arm64.D6, arm64.D7,
		arm64.D16, arm64.D17, arm64.D18, arm64.D19,
		arm64.D20, arm64.D21, arm64.D22, arm64.D23,
		arm64.D24, arm64.D25, arm64.D26, arm64.D27,
		arm64.D28, arm64.D29, arm64.D30, arm64.D31,
	}
)

const (
	a64OptNumGPR    = len(a64OptGPRs)
	a64OptNumFP     = len(a64OptFPs)
	a64OptMaxSpills = 16
)

type a64OptLoopRef struct {
	reg       *ir.LoopRegion
	entryLbl  arm64.Label // jumped to from OP_FORPREP / while header
	resumeLbl arm64.Label // jumped to from OP_FORLOOP backedge (OSR re-entry after poll/deopt)
	baseLbl   arm64.Label // baseline instruction at EntryPC (fallback when entry check fails)
}

type a64OptEmit struct {
	c       *a64Baseline
	a       *arm64.Assembler
	reg     *ir.LoopRegion
	alloc   *ir.AllocResult
	ref     *a64OptLoopRef
	preFail arm64.Label
	deopt   arm64.Label
	pollLbl arm64.Label

	cntVal  ir.ValID
	stepVal ir.ValID
	idxVar  int // index into reg.Vars of IsForIdx var (-1 for LoopWhile)
}

func (c *a64Baseline) emitAllOptLoops() {
	for _, ref := range c.optLoops {
		e := &a64OptEmit{
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

func (e *a64OptEmit) spillOff(slot int8) int32 {
	if slot < 0 || slot >= a64OptMaxSpills {
		panic("jit/arm64: invalid optSpill slot")
	}
	return jitCtxOptSpill + int32(slot)*8
}

func (e *a64OptEmit) loadG(loc ir.RegLoc, scratch arm64.Reg) arm64.Reg {
	if loc.Phys >= 0 {
		return a64OptGPRs[loc.Phys]
	}
	e.a.LDR(scratch, a64Ctx, e.spillOff(loc.Spill))
	return scratch
}

func (e *a64OptEmit) dstG(loc ir.RegLoc, scratch arm64.Reg) arm64.Reg {
	if loc.Phys >= 0 {
		return a64OptGPRs[loc.Phys]
	}
	return scratch
}

func (e *a64OptEmit) storeG(loc ir.RegLoc, src arm64.Reg) {
	if loc.Phys >= 0 {
		dst := a64OptGPRs[loc.Phys]
		if dst != src {
			e.a.MOV(dst, src)
		}
		return
	}
	e.a.STR(src, a64Ctx, e.spillOff(loc.Spill))
}

func (e *a64OptEmit) loadF(loc ir.RegLoc, scratch arm64.FReg) arm64.FReg {
	if loc.Phys >= 0 {
		return a64OptFPs[loc.Phys]
	}
	e.a.LDRD(scratch, a64Ctx, e.spillOff(loc.Spill))
	return scratch
}

func (e *a64OptEmit) dstF(loc ir.RegLoc, scratch arm64.FReg) arm64.FReg {
	if loc.Phys >= 0 {
		return a64OptFPs[loc.Phys]
	}
	return scratch
}

func (e *a64OptEmit) storeF(loc ir.RegLoc, src arm64.FReg) {
	if loc.Phys >= 0 {
		dst := a64OptFPs[loc.Phys]
		if dst != src {
			e.a.FMOV(dst, src)
		}
		return
	}
	e.a.STRD(src, a64Ctx, e.spillOff(loc.Spill))
}

func (e *a64OptEmit) emit() {
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

func (e *a64OptEmit) emitForIntLoop() {
	a := int(e.reg.ForBase)
	e.preFail = e.c.pcLabels[e.reg.BodyPC]
	e.deopt = e.a.NewLabel()
	e.pollLbl = e.a.NewLabel()

	// --- Entry from OP_FORPREP ---
	// Perform exact Lua 5.5 integer FORPREP setup, storing R[A..A+2] so any subsequent preheader
	// guard failure can jump directly to baseline BodyPC with zero cleanup.
	e.a.Bind(e.ref.entryLbl)
	e.c.loadReg(a, arm64.X12, arm64.X13)   // init
	e.c.loadReg(a+1, arm64.X14, arm64.X15) // lim
	e.c.loadReg(a+2, arm64.X16, arm64.X17) // step
	e.a.ORR(arm64.X13, arm64.X13, arm64.X15)
	e.a.ORR(arm64.X13, arm64.X13, arm64.X17)
	e.a.CBNZ(arm64.X13, e.ref.baseLbl) // non-int -> baseline OP_FORPREP
	e.a.CBZ(arm64.X16, e.ref.baseLbl)  // step == 0 -> baseline raises error

	lNeg := e.a.NewLabel()
	lStepNot1 := e.a.NewLabel()
	lStorePrep := e.a.NewLabel()
	e.a.CMPImm(arm64.X16, 0)
	e.a.BCond(arm64.LT, lNeg)
	// step > 0: 0 trips if init > lim
	e.a.CMP(arm64.X12, arm64.X14)
	e.a.BCond(arm64.GT, e.c.pcLabels[e.reg.ExitPC])
	e.a.SUB(arm64.X13, arm64.X14, arm64.X12) // cnt = uint64(lim - init)
	e.a.CMPImm(arm64.X16, 1)
	e.a.BCond(arm64.NE, lStepNot1)
	e.a.B(lStorePrep)
	e.a.Bind(lStepNot1)
	e.a.UDIV(arm64.X13, arm64.X13, arm64.X16)
	e.a.B(lStorePrep)

	e.a.Bind(lNeg)
	e.a.CMP(arm64.X12, arm64.X14)
	e.a.BCond(arm64.LT, e.c.pcLabels[e.reg.ExitPC])
	e.a.SUB(arm64.X13, arm64.X12, arm64.X14) // cnt = uint64(init - lim)
	e.a.ADDImm(arm64.X15, arm64.X16, 1)
	e.a.NEG(arm64.X15, arm64.X15)
	e.a.ADDImm(arm64.X15, arm64.X15, 1)
	e.a.UDIV(arm64.X13, arm64.X13, arm64.X15)

	e.a.Bind(lStorePrep)
	// Store R[A]=cnt, R[A+1]=step, R[A+2]=init (now identical to post-FORPREP / post-FORLOOP state!)
	e.c.storeReg(a, arm64.X13, arm64.XZR)
	e.c.storeReg(a+1, arm64.X16, arm64.XZR)
	e.c.storeReg(a+2, arm64.X12, arm64.XZR)
	lCommon := e.a.NewLabel()
	e.a.B(lCommon)

	// --- OSR Re-entry from baseline OP_FORLOOP backedge (after poll/deopt) ---
	e.a.Bind(e.ref.resumeLbl)
	e.c.loadReg(a, arm64.X13, arm64.X15)   // cnt
	e.c.loadReg(a+1, arm64.X16, arm64.X17) // step
	e.c.loadReg(a+2, arm64.X12, arm64.X14) // idx
	e.a.ORR(arm64.X15, arm64.X15, arm64.X17)
	e.a.ORR(arm64.X15, arm64.X15, arm64.X14)
	e.a.CBNZ(arm64.X15, e.preFail)

	// Common preheader: X13=cnt, X16=step, X12=idx
	e.a.Bind(lCommon)
	if e.reg.HasConstStep {
		e.a.MOVImm64(arm64.X14, uint64(e.reg.ConstStep))
		e.a.CMP(arm64.X16, arm64.X14)
		e.a.BCond(arm64.NE, e.preFail)
	}
	// Guard cnt < (1<<60) so rem = cnt + 1 never overflows int64.
	e.a.MOVImm64(arm64.X14, uint64(1)<<60)
	e.a.CMP(arm64.X13, arm64.X14)
	e.a.BCond(arm64.HS, e.preFail)

	// Store cnt (temporarily in LoopCnt loc), step (in LoopStep loc if dynamic), idx (in idxVar loc).
	e.storeG(e.alloc.ValLoc[e.cntVal], arm64.X13)
	if !e.reg.HasConstStep && e.stepVal != ir.NoVal {
		e.storeG(e.alloc.ValLoc[e.stepVal], arm64.X16)
	}
	e.storeG(e.alloc.VarLoc[e.idxVar], arm64.X12)

	// Run all preheader guards, unboxing, array hoisting, and BCE range checks!
	// Note: OpGuardRange sees cnt in LoopCnt loc and init in idxVar loc.
	e.emitPreheader(e.preFail)

	// Initialize non-index LoopVars from their preheader InitVal.
	for i, lv := range e.reg.Vars {
		if lv.IsForIdx {
			continue
		}
		e.copyValToLoc(lv.InitVal, e.alloc.VarLoc[i], lv.Type.IsFPReg())
	}

	// Convert cnt -> rem = cnt + 1 (total remaining iterations >= 1) in LoopCnt loc.
	remReg := e.loadG(e.alloc.ValLoc[e.cntVal], arm64.X12)
	e.a.ADDImm(remReg, remReg, 1)
	e.storeG(e.alloc.ValLoc[e.cntVal], remReg)

	// --- Strip-mined Outer Chunk Loop ---
	lChunkTop := e.a.NewLabel()
	lScalarLoop := e.a.NewLabel()
	lChunkDone := e.a.NewLabel()

	e.a.Bind(lChunkTop)
	e.a.CMPImm(a64Poll, 0)
	e.a.BCond(arm64.LE, e.pollLbl)

	// chunk (X12) = min(rem, a64Poll); outerRem = rem - chunk; a64Poll -= chunk; rem = chunk
	remR := e.loadG(e.alloc.ValLoc[e.cntVal], arm64.X13)
	e.a.CMP(remR, a64Poll)
	e.a.CSEL(arm64.X12, remR, a64Poll, arm64.LE) // X12 = chunk
	e.a.SUB(arm64.X14, remR, arm64.X12)          // X14 = outerRem
	e.a.STR(arm64.X14, a64Ctx, jitCtxOptOuterRem)
	e.a.SUB(a64Poll, a64Poll, arm64.X12)
	e.storeG(e.alloc.ValLoc[e.cntVal], arm64.X12)

	// --- Optional 2-wide NEON SIMD Vector Loop ---
	if e.reg.Vec != nil {
		e.emitNEONVecLoop(lScalarLoop, lChunkDone)
	}

	// --- Scalar Optimized Loop Body ---
	e.a.Bind(lScalarLoop)
	blockLabels := make([]arm64.Label, len(e.reg.Blocks))
	for i := range e.reg.Blocks {
		blockLabels[i] = e.a.NewLabel()
	}
	for i, blk := range e.reg.Blocks {
		e.a.Bind(blockLabels[i])
		e.emitBlockInsts(blk)
		e.emitBlockTerm(blk, blockLabels, lScalarLoop, lChunkDone, true)
	}

	// --- Chunk Done ---
	e.a.Bind(lChunkDone)
	e.a.LDR(arm64.X12, a64Ctx, jitCtxOptOuterRem)
	lFinish := e.a.NewLabel()
	e.a.CBZ(arm64.X12, lFinish)
	e.storeG(e.alloc.ValLoc[e.cntVal], arm64.X12)
	e.a.B(lChunkTop)

	// Normal loop completion: materialize LiveOut LoopVars -> ExitPC.
	e.a.Bind(lFinish)
	e.emitMaterializeVars(true)
	e.a.B(e.c.pcLabels[e.reg.ExitPC])

	// Poll safepoint: rem is total remaining (>=1), idx is next iteration's idx.
	e.a.Bind(e.pollLbl)
	remP := e.loadG(e.alloc.ValLoc[e.cntVal], arm64.X12)
	e.a.SUBImm(arm64.X12, remP, 1) // R[A] = rem - 1
	e.c.storeReg(a, arm64.X12, arm64.XZR)
	e.emitStoreForStepAndIdx(a)
	e.emitMaterializeVars(false)
	e.a.B(e.c.poll(e.reg.BodyPC))

	// Mid-loop deopt (guaranteed before any mutation in current iteration!):
	// totalRem = rem + outerRem; replay iteration cleanly in baseline at BodyPC.
	e.a.Bind(e.deopt)
	remD := e.loadG(e.alloc.ValLoc[e.cntVal], arm64.X12)
	e.a.LDR(arm64.X13, a64Ctx, jitCtxOptOuterRem)
	e.a.ADD(arm64.X12, remD, arm64.X13)
	e.a.SUBImm(arm64.X12, arm64.X12, 1)
	e.c.storeReg(a, arm64.X12, arm64.XZR)
	e.emitStoreForStepAndIdx(a)
	e.emitMaterializeVars(false)
	e.a.B(e.c.pcLabels[e.reg.BodyPC])
}

func (e *a64OptEmit) emitStoreForStepAndIdx(forBase int) {
	if e.reg.HasConstStep {
		e.a.MOVImm64(arm64.X13, uint64(e.reg.ConstStep))
	} else {
		e.a.MOV(arm64.X13, e.loadG(e.alloc.ValLoc[e.stepVal], arm64.X13))
	}
	e.c.storeReg(forBase+1, arm64.X13, arm64.XZR)
	idxR := e.loadG(e.alloc.VarLoc[e.idxVar], arm64.X14)
	e.c.storeReg(forBase+2, idxR, arm64.XZR)
}

func (e *a64OptEmit) emitWhileLoop() {
	e.preFail = e.ref.baseLbl
	e.deopt = e.a.NewLabel()
	e.pollLbl = e.a.NewLabel()

	e.a.Bind(e.ref.entryLbl)
	e.emitPreheader(e.preFail)
	for i, lv := range e.reg.Vars {
		e.copyValToLoc(lv.InitVal, e.alloc.VarLoc[i], lv.Type.IsFPReg())
	}

	blockLabels := make([]arm64.Label, len(e.reg.Blocks))
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
	e.a.SUBSImm(a64Poll, a64Poll, 1)
	e.a.BCond(arm64.LE, e.pollLbl)
	e.a.B(blockLabels[0])

	e.a.Bind(e.pollLbl)
	e.emitMaterializeVars(false)
	e.a.B(e.c.poll(e.reg.EntryPC))

	e.a.Bind(e.deopt)
	e.emitMaterializeVars(false)
	e.a.B(e.ref.baseLbl)
}

func (e *a64OptEmit) emitMaterializeVars(onlyLiveOut bool) {
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
			r := e.loadG(loc, arm64.X12)
			e.c.storeReg(int(lv.Slot), r, arm64.XZR)
		case ir.TFloat:
			f := e.loadF(loc, arm64.D24)
			e.a.FMOVToG(arm64.X12, f)
			e.a.MOVZ(arm64.X13, uint16(tagFloat), 0)
			e.c.storeReg(int(lv.Slot), arm64.X12, arm64.X13)
		case ir.TBool:
			r := e.loadG(loc, arm64.X12)
			e.a.ADDImm(arm64.X13, r, uint32(tagFalse))
			e.c.storeReg(int(lv.Slot), arm64.XZR, arm64.X13)
		}
	}
}

func (e *a64OptEmit) copyValToLoc(srcID ir.ValID, dst ir.RegLoc, isFP bool) {
	src := e.alloc.ValLoc[srcID]
	if isFP {
		f := e.loadF(src, arm64.D24)
		e.storeF(dst, f)
	} else {
		r := e.loadG(src, arm64.X12)
		e.storeG(dst, r)
	}
}

// --- Preheader Guards & Hoisting ---

func (e *a64OptEmit) emitPreheader(fail arm64.Label) {
	for i := range e.reg.Preheader {
		in := &e.reg.Preheader[i]
		if in.Dead {
			continue
		}
		loc := e.alloc.ValLoc[in.ID]
		hasLoc := loc.Phys >= 0 || loc.Spill >= 0
		switch in.Op {
		case ir.OpLoopCnt, ir.OpLoopStep, ir.OpLoopIdx:
			// Already loaded before emitPreheader.
		case ir.OpConstInt:
			if hasLoc {
				d := e.dstG(loc, arm64.X12)
				e.a.MOVImm64(d, uint64(in.ImmI))
				e.storeG(loc, d)
			}
		case ir.OpConstFloat:
			if hasLoc {
				d := e.dstF(loc, arm64.D24)
				e.a.MOVImm64(arm64.X12, in.ImmU)
				e.a.FMOVToF(d, arm64.X12)
				e.storeF(loc, d)
			}
		case ir.OpConstBool:
			if hasLoc {
				d := e.dstG(loc, arm64.X12)
				e.a.MOVZ(d, uint16(in.ImmI), 0)
				e.storeG(loc, d)
			}
		case ir.OpUnboxInt:
			e.c.loadReg(int(in.Slot), arm64.X12, arm64.X13)
			e.a.CBNZ(arm64.X13, fail)
			if hasLoc {
				e.storeG(loc, arm64.X12)
			}
		case ir.OpUnboxFloat:
			e.c.loadReg(int(in.Slot), arm64.X12, arm64.X13)
			e.a.CMPImm(arm64.X13, uint32(tagFloat))
			e.a.BCond(arm64.NE, fail)
			if hasLoc {
				d := e.dstF(loc, arm64.D24)
				e.a.FMOVToF(d, arm64.X12)
				e.storeF(loc, d)
			}
		case ir.OpUnboxNumFloat:
			e.c.loadReg(int(in.Slot), arm64.X12, arm64.X13)
			lFloat := e.a.NewLabel()
			lDone := e.a.NewLabel()
			e.a.CMPImm(arm64.X13, uint32(tagFloat))
			e.a.BCond(arm64.EQ, lFloat)
			e.a.CBNZ(arm64.X13, fail) // not tagInt either
			if hasLoc {
				d := e.dstF(loc, arm64.D24)
				e.a.SCVTF(d, arm64.X12)
				e.storeF(loc, d)
			}
			e.a.B(lDone)
			e.a.Bind(lFloat)
			if hasLoc {
				d := e.dstF(loc, arm64.D24)
				e.a.FMOVToF(d, arm64.X12)
				e.storeF(loc, d)
			}
			e.a.Bind(lDone)
		case ir.OpUnboxBool:
			e.c.loadReg(int(in.Slot), arm64.X12, arm64.X13)
			e.a.SUBImm(arm64.X13, arm64.X13, uint32(tagFalse))
			e.a.CMPImm(arm64.X13, 1)
			e.a.BCond(arm64.HI, fail)
			if hasLoc {
				e.storeG(loc, arm64.X13)
			}
		case ir.OpGuardUpvalInt:
			e.emitLoadUpvalPre(int(in.AuxIdx), arm64.X12, arm64.X13, fail)
			e.a.CBNZ(arm64.X13, fail)
			if hasLoc {
				e.storeG(loc, arm64.X12)
			}
		case ir.OpGuardUpvalFlt:
			e.emitLoadUpvalPre(int(in.AuxIdx), arm64.X12, arm64.X13, fail)
			lFloat := e.a.NewLabel()
			lDone := e.a.NewLabel()
			e.a.CMPImm(arm64.X13, uint32(tagFloat))
			e.a.BCond(arm64.EQ, lFloat)
			e.a.CBNZ(arm64.X13, fail)
			if hasLoc {
				d := e.dstF(loc, arm64.D24)
				e.a.SCVTF(d, arm64.X12)
				e.storeF(loc, d)
			}
			e.a.B(lDone)
			e.a.Bind(lFloat)
			if hasLoc {
				d := e.dstF(loc, arm64.D24)
				e.a.FMOVToF(d, arm64.X12)
				e.storeF(loc, d)
			}
			e.a.Bind(lDone)
		case ir.OpGuardUpvalSqrt:
			e.emitLoadUpvalPre(int(in.AuxIdx), arm64.X12, arm64.X13, fail)
			e.a.CMPImm(arm64.X13, uint32(tagStaticNative))
			e.a.BCond(arm64.NE, fail)
			e.a.LDRW(arm64.X14, a64Ctx, jitCtxSqrtIdx)
			e.a.CBZ(arm64.X14, fail)
			e.a.CMP(arm64.X12, arm64.X14)
			e.a.BCond(arm64.NE, fail)
		case ir.OpGuardUpvalAbs:
			e.emitLoadUpvalPre(int(in.AuxIdx), arm64.X12, arm64.X13, fail)
			e.a.CMPImm(arm64.X13, uint32(tagStaticNative))
			e.a.BCond(arm64.NE, fail)
			e.a.LDRW(arm64.X14, a64Ctx, jitCtxAbsIdx)
			e.a.CBZ(arm64.X14, fail)
			e.a.CMP(arm64.X12, arm64.X14)
			e.a.BCond(arm64.NE, fail)
		case ir.OpGuardRegSqrt:
			e.c.loadReg(int(in.Slot), arm64.X12, arm64.X13)
			e.a.CMPImm(arm64.X13, uint32(tagStaticNative))
			e.a.BCond(arm64.NE, fail)
			e.a.LDRW(arm64.X14, a64Ctx, jitCtxSqrtIdx)
			e.a.CBZ(arm64.X14, fail)
			e.a.CMP(arm64.X12, arm64.X14)
			e.a.BCond(arm64.NE, fail)
		case ir.OpGuardRegAbs:
			e.c.loadReg(int(in.Slot), arm64.X12, arm64.X13)
			e.a.CMPImm(arm64.X13, uint32(tagStaticNative))
			e.a.BCond(arm64.NE, fail)
			e.a.LDRW(arm64.X14, a64Ctx, jitCtxAbsIdx)
			e.a.CBZ(arm64.X14, fail)
			e.a.CMP(arm64.X12, arm64.X14)
			e.a.BCond(arm64.NE, fail)
		case ir.OpGuardCalleePro:
			if in.UpvalIdx >= 0 {
				e.emitLoadUpvalPre(int(in.UpvalIdx), arm64.X12, arm64.X13, fail)
			} else {
				e.c.loadReg(int(in.Slot), arm64.X12, arm64.X13)
			}
			e.a.CMPImm(arm64.X13, uint32(tagClosure))
			e.a.BCond(arm64.NE, fail)
			e.c.emitResolveSmall(arm64.X12, arm64.X12, arm64.X14)
			e.a.LDRW(arm64.X13, arm64.X12, jitClProtoIdx)
			e.a.MOVImm32(arm64.X14, in.AuxU32)
			e.a.CMP(arm64.X13, arm64.X14)
			e.a.BCond(arm64.NE, fail)
		case ir.OpGuardTableArr:
			if in.UpvalIdx >= 0 {
				e.emitLoadUpvalPre(int(in.UpvalIdx), arm64.X12, arm64.X13, fail)
			} else {
				e.c.loadReg(int(in.Slot), arm64.X12, arm64.X13)
			}
			e.a.CMPImm(arm64.X13, uint32(tagTable))
			e.a.BCond(arm64.NE, fail)
			e.c.emitResolveSmall(arm64.X12, arm64.X14, arm64.X15) // X14 = *Table
			e.a.LDRB(arm64.X13, arm64.X14, jitTblArrKind)
			e.a.CMPImm(arm64.X13, uint32(in.Type.ExpectedArrKind()))
			e.a.BCond(arm64.NE, fail)
			e.a.LDRW(arm64.X15, arm64.X14, jitTblArrLen) // X15 = arrLen
			e.a.CBZ(arm64.X15, fail)
			e.a.LDRW(arm64.X12, arm64.X14, jitTblArrRef)
			e.a.CBZ(arm64.X12, fail)
			e.c.emitResolveFast(arm64.X12, arm64.X12, arm64.X13) // X12 = arrBase
			if hasLoc {
				e.storeG(loc, arm64.X12)
			}
			lenLoc := e.alloc.LenLoc[in.ID]
			if lenLoc.Phys >= 0 || lenLoc.Spill >= 0 {
				e.storeG(lenLoc, arm64.X15)
			}
		case ir.OpLoadArrLen:
			// Already stored into LenLoc[in.Args[0]] (which aliases ValLoc[in.ID]) by OpGuardTableArr.
		case ir.OpGuardRange:
			e.emitGuardRange(in, fail)
		}
	}
}

func (e *a64OptEmit) emitLoadUpvalPre(uvIdx int, bitsReg, tagReg arm64.Reg, fail arm64.Label) {
	e.a.CBZ(a64ClUpvals, fail)
	e.a.LDRW(arm64.X14, a64ClUpvals, int32(uvIdx*4))
	e.c.emitResolveSmall(arm64.X14, arm64.X14, arm64.X15)
	e.a.LDRW(arm64.X15, arm64.X14, jitUvOwnerRef)
	lOpen := e.a.NewLabel()
	lDone := e.a.NewLabel()
	e.a.CBNZ(arm64.X15, lOpen)
	e.a.LDP(bitsReg, tagReg, arm64.X14, jitUvClosed)
	e.a.B(lDone)
	e.a.Bind(lOpen)
	e.a.LDRW(arm64.X16, a64Ctx, jitCtxThSelf)
	e.a.CMP(arm64.X15, arm64.X16)
	e.a.BCond(arm64.NE, fail)
	e.a.LDR(arm64.X15, arm64.X14, jitUvStackIdx)
	e.a.ADDShift(arm64.X15, a64Stack, arm64.X15, arm64.ShiftLSL, 4)
	e.a.LDP(bitsReg, tagReg, arm64.X15, 0)
	e.a.Bind(lDone)
}

func (e *a64OptEmit) emitGuardRange(in *ir.Inst, fail arm64.Label) {
	// Verify every access `baseOff + (init + k*step) + delta` for 0 <= k <= cnt and
	// minDelta <= delta <= maxDelta lies in [1, arrLen] with zero 64-bit overflow!
	arrLen := e.loadG(e.alloc.LenLoc[in.Args[0]], arm64.X12)
	if arrLen != arm64.X12 {
		e.a.MOV(arm64.X12, arrLen)
	}
	initR := e.loadG(e.alloc.VarLoc[e.idxVar], arm64.X13)
	e.a.MOV(arm64.X13, initR)
	if in.NumArgs >= 2 && in.Args[1] != ir.NoVal {
		offR := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X14)
		e.a.ADD(arm64.X13, arm64.X13, offR)
	}
	// X14 = firstMin - 1 = (baseOff + init + minDelta - 1)
	// X15 = firstMax - 1 = (baseOff + init + maxDelta - 1)
	e.a.MOVImm64(arm64.X14, uint64(in.ImmI-1))
	e.a.ADD(arm64.X14, arm64.X13, arm64.X14)
	e.a.CMP(arm64.X14, arm64.X12)
	e.a.BCond(arm64.HS, fail) // firstMin < 1 || firstMin > arrLen

	if in.ImmI2 != in.ImmI {
		e.a.MOVImm64(arm64.X15, uint64(in.ImmI2-1))
		e.a.ADD(arm64.X15, arm64.X13, arm64.X15)
		e.a.CMP(arm64.X15, arm64.X12)
		e.a.BCond(arm64.HS, fail)
	} else {
		e.a.MOV(arm64.X15, arm64.X14)
	}

	cntR := e.loadG(e.alloc.ValLoc[e.cntVal], arm64.X16)
	if e.reg.HasConstStep && e.reg.ConstStep == 1 {
		// Need cnt < arrLen - (firstMax - 1)  <==>  (firstMax - 1) + cnt < arrLen (overflow-free!)
		e.a.SUB(arm64.X17, arm64.X12, arm64.X15) // arrLen - (firstMax-1) >= 1
		e.a.CMP(cntR, arm64.X17)
		e.a.BCond(arm64.HS, fail)
		return
	}
	// General positive/negative step check: require |step| <= 0x7FFFFFFF && cnt <= 0x7FFFFFFF.
	var stepR arm64.Reg
	if e.reg.HasConstStep {
		e.a.MOVImm64(arm64.X17, uint64(e.reg.ConstStep))
		stepR = arm64.X17
	} else {
		stepR = e.loadG(e.alloc.ValLoc[e.stepVal], arm64.X17)
	}
	e.a.MOVImm32(arm64.X13, 0x7FFFFFFF)
	e.a.CMP(cntR, arm64.X13)
	e.a.BCond(arm64.HI, fail)

	lNeg := e.a.NewLabel()
	lDone := e.a.NewLabel()
	e.a.CMPImm(stepR, 0)
	e.a.BCond(arm64.LT, lNeg)
	// step > 0
	e.a.CMP(stepR, arm64.X13)
	e.a.BCond(arm64.HI, fail)
	e.a.MUL(arm64.X13, cntR, stepR)          // span <= 2^62
	e.a.SUB(arm64.X17, arm64.X12, arm64.X15) // arrLen - (firstMax - 1)
	e.a.CMP(arm64.X13, arm64.X17)
	e.a.BCond(arm64.HS, fail)
	e.a.B(lDone)

	e.a.Bind(lNeg)
	e.a.NEG(arm64.X17, stepR)
	e.a.CMP(arm64.X17, arm64.X13)
	e.a.BCond(arm64.HI, fail)
	e.a.MUL(arm64.X13, cntR, arm64.X17) // span <= 2^62
	e.a.CMP(arm64.X13, arm64.X14)       // need span <= firstMin - 1
	e.a.BCond(arm64.HI, fail)
	e.a.Bind(lDone)
}

// --- 2-Wide ARM64 NEON SIMD Main Loop ---

func (e *a64OptEmit) emitNEONVecLoop(lScalar, lChunkDone arm64.Label) {
	plan := e.reg.Vec
	// Need at least 2 iterations in current chunk.
	remR := e.loadG(e.alloc.ValLoc[e.cntVal], arm64.X12)
	e.a.CMPImm(remR, 2)
	e.a.BCond(arm64.LT, lScalar)

	// Pick free 128-bit NEON registers not used by any pinned FP LoopVar or Preheader value
	// that the scalar tail epilogue still needs!
	usedFP := make(map[arm64.FReg]bool)
	for i, lv := range e.reg.Vars {
		if lv.Type.IsFPReg() && e.alloc.VarLoc[i].Phys >= 0 {
			usedFP[a64OptFPs[e.alloc.VarLoc[i].Phys]] = true
		}
	}
	for i := range e.reg.Preheader {
		loc := e.alloc.ValLoc[e.reg.Preheader[i].ID]
		if loc.IsFP && loc.Phys >= 0 {
			usedFP[a64OptFPs[loc.Phys]] = true
		}
	}
	var freeV []arm64.FReg
	for _, r := range a64AllScratchFPs {
		if !usedFP[r] && r != arm64.D30 && r != arm64.D31 { // reserve D30/D31 as lane-extract/spill scratch
			freeV = append(freeV, r)
		}
	}
	if len(freeV) < plan.NumVRegs || len(plan.Ptrs) > 4 {
		// Not enough free NEON regs/ptr regs without clobbering scalar tail -> fall straight to scalar!
		return
	}
	vreg := func(vr int) arm64.FReg { return freeV[vr] }

	// Runtime preheader alias checks: if ANY pair overlaps dangerously, jump to lScalar!
	idxR := e.loadG(e.alloc.VarLoc[e.idxVar], arm64.X17)
	for _, ac := range plan.AliasChecks {
		// p1 (X12) = dstBase + (dstOff + idx + dstDel - 1) * 8
		e.emitVecElemPtr(arm64.X12, ac.DstArr, ac.DstOff, ac.DstDel, idxR, arm64.X15)
		// p2 (X13) = srcBase + (srcOff + idx + srcDel - 1) * 8
		e.emitVecElemPtr(arm64.X13, ac.SrcArr, ac.SrcOff, ac.SrcDel, idxR, arm64.X15)
		lOK := e.a.NewLabel()
		e.a.CMP(arm64.X12, arm64.X13)
		e.a.BCond(arm64.EQ, lOK) // exact same address: elementwise safe!
		remCur := e.loadG(e.alloc.ValLoc[e.cntVal], arm64.X14)
		e.a.LSLImm(arm64.X14, remCur, 3) // bytes = rem * 8
		e.a.ADD(arm64.X15, arm64.X12, arm64.X14)
		e.a.CMP(arm64.X15, arm64.X13)
		e.a.BCond(arm64.LS, lOK) // p1 + bytes <= p2
		e.a.ADD(arm64.X15, arm64.X13, arm64.X14)
		e.a.CMP(arm64.X15, arm64.X12)
		e.a.BCond(arm64.LS, lOK) // p2 + bytes <= p1
		e.a.B(lScalar)           // overlap -> run scalar loop!
		e.a.Bind(lOK)
	}

	// Broadcast loop-invariant splats into NEON vector registers.
	for _, sp := range plan.Splats {
		vd := vreg(sp.VReg)
		sloc := e.alloc.ValLoc[sp.ScalarVal]
		if sp.IsFloat {
			if sloc.Phys >= 0 || sloc.Spill >= 0 {
				sf := e.loadF(sloc, arm64.D31)
				e.a.DUP2DElem0(vd, sf)
			} else {
				// Body-local constant (e.g. OpConstFloat not allocated a preheader loc)
				bits := e.findConstBits(sp.ScalarVal)
				e.a.MOVImm64(arm64.X12, bits)
				e.a.DUP2DGPR(vd, arm64.X12)
			}
		} else {
			if sloc.Phys >= 0 || sloc.Spill >= 0 {
				sr := e.loadG(sloc, arm64.X12)
				e.a.DUP2DGPR(vd, sr)
			} else {
				bits := e.findConstBits(sp.ScalarVal)
				e.a.MOVImm64(arm64.X12, bits)
				e.a.DUP2DGPR(vd, arm64.X12)
			}
		}
	}

	// Zero-init 2-wide int64 vector accumulators.
	for _, acc := range plan.IntAccums {
		e.a.MOVI2DZero(vreg(acc.VReg))
	}

	// Initialize up to 4 advancing array pointers in reserved scratch GPRs X12..X15!
	ptrRegs := [4]arm64.Reg{arm64.X12, arm64.X13, arm64.X14, arm64.X15}
	for i, p := range plan.Ptrs {
		e.emitVecElemPtr(ptrRegs[i], p.ArrVal, p.BaseOff, p.Delta, idxR, arm64.X16)
	}

	// pairs (X16) = rem >> 1; doneElems = pairs << 1; rem -= doneElems; idx += doneElems
	remCur := e.loadG(e.alloc.ValLoc[e.cntVal], arm64.X17)
	e.a.LSRImm(arm64.X16, remCur, 1) // pairs >= 1
	e.a.LSLImm(arm64.X17, arm64.X16, 1)
	e.a.SUB(arm64.X17, remCur, arm64.X17) // tail rem (0 or 1)
	e.storeG(e.alloc.ValLoc[e.cntVal], arm64.X17)
	// Advance idx by pairs*2 right now so idx is exact after the vector loop!
	idxDst := e.loadG(e.alloc.VarLoc[e.idxVar], arm64.X17)
	e.a.ADDShift(idxDst, idxDst, arm64.X16, arm64.ShiftLSL, 1)
	e.storeG(e.alloc.VarLoc[e.idxVar], idxDst)

	// Tight 2-wide NEON loop!
	lVecLoop := e.a.NewLabel()
	e.a.Bind(lVecLoop)
	for _, vi := range plan.Body {
		switch vi.Op {
		case ir.VLoadF64, ir.VLoadI64:
			e.a.LDRQ(vreg(vi.Dst), ptrRegs[vi.Src1], 0)
		case ir.VStoreF64, ir.VStoreI64:
			e.a.STRQ(vreg(vi.Src2), ptrRegs[vi.Src1], 0)
		case ir.VAddF64:
			e.a.FADD2D(vreg(vi.Dst), vreg(vi.Src1), vreg(vi.Src2))
		case ir.VSubF64:
			e.a.FSUB2D(vreg(vi.Dst), vreg(vi.Src1), vreg(vi.Src2))
		case ir.VMulF64:
			e.a.FMUL2D(vreg(vi.Dst), vreg(vi.Src1), vreg(vi.Src2))
		case ir.VDivF64:
			e.a.FDIV2D(vreg(vi.Dst), vreg(vi.Src1), vreg(vi.Src2))
		case ir.VNegF64:
			e.a.FNEG2D(vreg(vi.Dst), vreg(vi.Src1))
		case ir.VSqrtF64:
			e.a.FSQRT2D(vreg(vi.Dst), vreg(vi.Src1))
		case ir.VAbsF64:
			e.a.FABS2D(vreg(vi.Dst), vreg(vi.Src1))
		case ir.VAddI64:
			e.a.ADD2D(vreg(vi.Dst), vreg(vi.Src1), vreg(vi.Src2))
		case ir.VSubI64:
			e.a.SUB2D(vreg(vi.Dst), vreg(vi.Src1), vreg(vi.Src2))
		case ir.VAndI64:
			e.a.AND16B(vreg(vi.Dst), vreg(vi.Src1), vreg(vi.Src2))
		case ir.VOrI64:
			e.a.ORR16B(vreg(vi.Dst), vreg(vi.Src1), vreg(vi.Src2))
		case ir.VXorI64:
			e.a.EOR16B(vreg(vi.Dst), vreg(vi.Src1), vreg(vi.Src2))
		case ir.VAccumF64:
			// Exact left-to-right IEEE-754 order: acc = (acc + lane0) + lane1!
			vloc := e.alloc.VarLoc[vi.VarIdx]
			accF := e.loadF(vloc, arm64.D30)
			srcV := vreg(vi.Src1)
			e.a.FADD(accF, accF, srcV) // lane 0 (D alias of V)
			e.a.DUPD1(arm64.D31, srcV) // lane 1 -> D31
			e.a.FADD(accF, accF, arm64.D31)
			if vloc.Phys < 0 {
				e.storeF(vloc, accF)
			}
		case ir.VAccumI64:
			e.a.ADD2D(vreg(vi.Dst), vreg(vi.Dst), vreg(vi.Src1))
		}
	}
	for i := range plan.Ptrs {
		e.a.ADDImm(ptrRegs[i], ptrRegs[i], 16)
	}
	e.a.SUBSImm(arm64.X16, arm64.X16, 1)
	e.a.BCond(arm64.NE, lVecLoop)

	// Fold 2-wide int64 vector accumulators horizontally into their scalar LoopVars.
	for _, acc := range plan.IntAccums {
		e.a.ADDPD(arm64.D31, vreg(acc.VReg))
		e.a.FMOVToG(arm64.X12, arm64.D31)
		vloc := e.alloc.VarLoc[acc.VarIdx]
		cur := e.loadG(vloc, arm64.X13)
		e.a.ADD(cur, cur, arm64.X12)
		e.storeG(vloc, cur)
	}

	// If tail rem == 0, skip scalar epilogue directly to lChunkDone!
	tailR := e.loadG(e.alloc.ValLoc[e.cntVal], arm64.X12)
	e.a.CBZ(tailR, lChunkDone)
}

func (e *a64OptEmit) emitVecElemPtr(dst arm64.Reg, arrVal, baseOff ir.ValID, delta int64, idxReg, tmp arm64.Reg) {
	// dst = arrBase + (baseOff + idx + delta - 1) * 8
	e.a.MOV(tmp, idxReg)
	if baseOff != ir.NoVal {
		offR := e.loadG(e.alloc.ValLoc[baseOff], dst)
		e.a.ADD(tmp, tmp, offR)
	}
	adj := delta - 1
	if adj > 0 && adj < 4096 {
		e.a.ADDImm(tmp, tmp, uint32(adj))
	} else if adj < 0 && -adj < 4096 {
		e.a.SUBImm(tmp, tmp, uint32(-adj))
	} else if adj != 0 {
		e.a.MOVImm64(dst, uint64(adj))
		e.a.ADD(tmp, tmp, dst)
	}
	baseR := e.loadG(e.alloc.ValLoc[arrVal], dst)
	e.a.ADDShift(dst, baseR, tmp, arm64.ShiftLSL, 3)
}

func (e *a64OptEmit) findConstBits(id ir.ValID) uint64 {
	for i := range e.reg.Preheader {
		if e.reg.Preheader[i].ID == id {
			if e.reg.Preheader[i].Op == ir.OpConstFloat {
				return e.reg.Preheader[i].ImmU
			}
			return uint64(e.reg.Preheader[i].ImmI)
		}
	}
	for _, blk := range e.reg.Blocks {
		for i := range blk.Insts {
			if blk.Insts[i].ID == id {
				if blk.Insts[i].Op == ir.OpConstFloat {
					return blk.Insts[i].ImmU
				}
				return uint64(blk.Insts[i].ImmI)
			}
		}
	}
	return 0
}

// --- Scalar Body Lowering ---

func (e *a64OptEmit) emitBlockInsts(blk *ir.Block) {
	for i := range blk.Insts {
		in := &blk.Insts[i]
		if in.Dead {
			continue
		}
		loc := e.alloc.ValLoc[in.ID]
		switch in.Op {
		case ir.OpConstInt:
			d := e.dstG(loc, arm64.X12)
			e.a.MOVImm64(d, uint64(in.ImmI))
			e.storeG(loc, d)
		case ir.OpConstFloat:
			d := e.dstF(loc, arm64.D24)
			e.a.MOVImm64(arm64.X12, in.ImmU)
			e.a.FMOVToF(d, arm64.X12)
			e.storeF(loc, d)
		case ir.OpConstBool:
			d := e.dstG(loc, arm64.X12)
			e.a.MOVZ(d, uint16(in.ImmI), 0)
			e.storeG(loc, d)

		case ir.OpGetVar:
			vloc := e.alloc.VarLoc[in.AuxIdx]
			if vloc == loc {
				continue // zero-cost aliased register!
			}
			if in.Type.IsFPReg() {
				e.storeF(loc, e.loadF(vloc, arm64.D24))
			} else {
				e.storeG(loc, e.loadG(vloc, arm64.X12))
			}

		case ir.OpSetVar:
			vloc := e.alloc.VarLoc[in.AuxIdx]
			if in.Type.IsFPReg() {
				e.storeF(vloc, e.loadF(e.alloc.ValLoc[in.Args[0]], arm64.D24))
			} else {
				e.storeG(vloc, e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12))
			}

		case ir.OpAddI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			d := e.dstG(loc, arm64.X14)
			e.a.ADD(d, l, r)
			e.storeG(loc, d)
		case ir.OpSubI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			d := e.dstG(loc, arm64.X14)
			e.a.SUB(d, l, r)
			e.storeG(loc, d)
		case ir.OpMulI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			d := e.dstG(loc, arm64.X14)
			e.a.MUL(d, l, r)
			e.storeG(loc, d)
		case ir.OpAddIImm:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			d := e.dstG(loc, arm64.X14)
			if in.ImmI >= 0 && in.ImmI < 4096 {
				e.a.ADDImm(d, l, uint32(in.ImmI))
			} else if in.ImmI < 0 && -in.ImmI < 4096 {
				e.a.SUBImm(d, l, uint32(-in.ImmI))
			} else {
				e.a.MOVImm64(arm64.X13, uint64(in.ImmI))
				e.a.ADD(d, l, arm64.X13)
			}
			e.storeG(loc, d)
		case ir.OpIDivI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			e.a.CBZ(r, e.deopt)
			// Guard MinInt64 // -1 -> deopt or handle cleanly
			e.a.MOV(arm64.X14, l)
			e.a.MOV(arm64.X15, r)
			e.a.SDIV(arm64.X16, arm64.X14, arm64.X15)
			e.a.MSUB(arm64.X17, arm64.X16, arm64.X15, arm64.X14) // rem = l - q*r
			lDone := e.a.NewLabel()
			e.a.CBZ(arm64.X17, lDone)
			e.a.EOR(arm64.X17, arm64.X17, arm64.X15)
			e.a.TBZ(arm64.X17, 63, lDone)
			e.a.SUBImm(arm64.X16, arm64.X16, 1)
			e.a.Bind(lDone)
			e.storeG(loc, arm64.X16)
		case ir.OpModI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			e.a.CBZ(r, e.deopt)
			e.a.MOV(arm64.X14, l)
			e.a.MOV(arm64.X15, r)
			e.a.SDIV(arm64.X16, arm64.X14, arm64.X15)
			e.a.MSUB(arm64.X16, arm64.X16, arm64.X15, arm64.X14) // rem
			lDone := e.a.NewLabel()
			e.a.CBZ(arm64.X16, lDone)
			e.a.EOR(arm64.X17, arm64.X16, arm64.X15)
			e.a.TBZ(arm64.X17, 63, lDone)
			e.a.ADD(arm64.X16, arm64.X16, arm64.X15)
			e.a.Bind(lDone)
			e.storeG(loc, arm64.X16)
		case ir.OpNegI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			d := e.dstG(loc, arm64.X14)
			e.a.NEG(d, l)
			e.storeG(loc, d)
		case ir.OpBNotI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			d := e.dstG(loc, arm64.X14)
			e.a.MVN(d, l)
			e.storeG(loc, d)
		case ir.OpAndI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			d := e.dstG(loc, arm64.X14)
			e.a.AND(d, l, r)
			e.storeG(loc, d)
		case ir.OpOrI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			d := e.dstG(loc, arm64.X14)
			e.a.ORR(d, l, r)
			e.storeG(loc, d)
		case ir.OpXorI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			d := e.dstG(loc, arm64.X14)
			e.a.EOR(d, l, r)
			e.storeG(loc, d)
		case ir.OpShlI, ir.OpShrI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			e.emitLuaShift(in.Op == ir.OpShlI, l, r, loc)
		case ir.OpIntToFloat:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			d := e.dstF(loc, arm64.D24)
			e.a.SCVTF(d, l)
			e.storeF(loc, d)
		case ir.OpAbsI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			d := e.dstG(loc, arm64.X14)
			e.a.CMPImm(l, 0)
			e.a.CNEG(d, l, arm64.LT)
			e.storeG(loc, d)

		case ir.OpAddF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], arm64.D24)
			r := e.loadF(e.alloc.ValLoc[in.Args[1]], arm64.D25)
			d := e.dstF(loc, arm64.D26)
			e.a.FADD(d, l, r)
			e.storeF(loc, d)
		case ir.OpSubF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], arm64.D24)
			r := e.loadF(e.alloc.ValLoc[in.Args[1]], arm64.D25)
			d := e.dstF(loc, arm64.D26)
			e.a.FSUB(d, l, r)
			e.storeF(loc, d)
		case ir.OpMulF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], arm64.D24)
			r := e.loadF(e.alloc.ValLoc[in.Args[1]], arm64.D25)
			d := e.dstF(loc, arm64.D26)
			e.a.FMUL(d, l, r)
			e.storeF(loc, d)
		case ir.OpDivF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], arm64.D24)
			r := e.loadF(e.alloc.ValLoc[in.Args[1]], arm64.D25)
			d := e.dstF(loc, arm64.D26)
			e.a.FDIV(d, l, r)
			e.storeF(loc, d)
		case ir.OpNegF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], arm64.D24)
			d := e.dstF(loc, arm64.D26)
			e.a.FNEG(d, l)
			e.storeF(loc, d)
		case ir.OpSqrtF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], arm64.D24)
			d := e.dstF(loc, arm64.D26)
			e.a.FSQRT(d, l)
			e.storeF(loc, d)
		case ir.OpAbsF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], arm64.D24)
			d := e.dstF(loc, arm64.D26)
			e.a.FABS(d, l)
			e.storeF(loc, d)

		case ir.OpNotB:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			d := e.dstG(loc, arm64.X14)
			e.a.EORImm(d, l, 1)
			e.storeG(loc, d)
		case ir.OpCmpI:
			l := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			r := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			d := e.dstG(loc, arm64.X14)
			e.a.CMP(l, r)
			e.a.CSET(d, mapA64IntCond(in.AuxCond))
			e.storeG(loc, d)
		case ir.OpCmpF:
			l := e.loadF(e.alloc.ValLoc[in.Args[0]], arm64.D24)
			r := e.loadF(e.alloc.ValLoc[in.Args[1]], arm64.D25)
			d := e.dstG(loc, arm64.X14)
			e.a.FCMP(l, r)
			e.a.CSET(d, mapA64FPCond(in.AuxCond))
			e.storeG(loc, d)

		case ir.OpLoadArrInt:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			e.a.SUBImm(arm64.X14, idxR, 1)
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], arm64.X15)
				e.a.CMP(arm64.X14, lenR)
				e.a.BCond(arm64.HS, e.deopt)
			}
			d := e.dstG(loc, arm64.X15)
			e.a.LDRReg(d, baseR, arm64.X14, true)
			e.storeG(loc, d)

		case ir.OpLoadArrFloat:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			e.a.SUBImm(arm64.X14, idxR, 1)
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], arm64.X15)
				e.a.CMP(arm64.X14, lenR)
				e.a.BCond(arm64.HS, e.deopt)
			}
			d := e.dstF(loc, arm64.D24)
			e.a.LDRDReg(d, baseR, arm64.X14, true)
			e.storeF(loc, d)

		case ir.OpLoadArrBool:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			e.a.SUBImm(arm64.X14, idxR, 1)
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], arm64.X15)
				e.a.CMP(arm64.X14, lenR)
				e.a.BCond(arm64.HS, e.deopt)
			}
			d := e.dstG(loc, arm64.X15)
			e.a.LDRBReg(d, baseR, arm64.X14)
			e.storeG(loc, d)

		case ir.OpLoadArrTable:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			e.a.SUBImm(arm64.X14, idxR, 1)
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], arm64.X15)
				e.a.CMP(arm64.X14, lenR)
				e.a.BCond(arm64.HS, e.deopt)
			}
			e.a.ADDShift(arm64.X14, baseR, arm64.X14, arm64.ShiftLSL, 4)
			e.a.LDP(arm64.X15, arm64.X16, arm64.X14, 0)
			e.a.CMPImm(arm64.X16, uint32(tagTable))
			e.a.BCond(arm64.NE, e.deopt)
			d := e.dstG(loc, arm64.X15)
			e.c.emitResolveSmall(arm64.X15, d, arm64.X16)
			e.storeG(loc, d)

		case ir.OpStoreArrInt:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			e.a.SUBImm(arm64.X14, idxR, 1)
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], arm64.X15)
				e.a.CMP(arm64.X14, lenR)
				e.a.BCond(arm64.HS, e.deopt)
			}
			valR := e.loadG(e.alloc.ValLoc[in.Args[2]], arm64.X15)
			e.a.STRReg(valR, baseR, arm64.X14, true)

		case ir.OpStoreArrFlt:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			e.a.SUBImm(arm64.X14, idxR, 1)
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], arm64.X15)
				e.a.CMP(arm64.X14, lenR)
				e.a.BCond(arm64.HS, e.deopt)
			}
			valF := e.loadF(e.alloc.ValLoc[in.Args[2]], arm64.D24)
			e.a.STRDReg(valF, baseR, arm64.X14, true)

		case ir.OpStoreArrBool:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			idxR := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			e.a.SUBImm(arm64.X14, idxR, 1)
			if !in.BCE {
				lenR := e.loadG(e.alloc.LenLoc[in.Args[0]], arm64.X15)
				e.a.CMP(arm64.X14, lenR)
				e.a.BCond(arm64.HS, e.deopt)
			}
			valR := e.loadG(e.alloc.ValLoc[in.Args[2]], arm64.X15)
			e.a.STRBReg(valR, baseR, arm64.X14)

		case ir.OpGuardShape:
			tblR := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			e.a.LDRW(arm64.X13, tblR, jitTblShapeRef)
			e.a.MOVImm32(arm64.X14, in.AuxU32)
			e.a.CMP(arm64.X13, arm64.X14)
			e.a.BCond(arm64.NE, e.deopt)
			// Also ensure non-prototype table (hdr.Flags & 1 == 0) so field writes never need protoEpoch bump.
			e.a.LDRB(arm64.X13, tblR, jitTblFlags)
			e.c.branchBitSet(arm64.X13, 0, e.deopt)
			if in.ImmI != 0 {
				// Reject tables with metatables before any OpStoreSlot* (preserves __newindex on nil own slots).
				e.a.LDRW(arm64.X13, tblR, jitTblMetaRef)
				e.a.CBNZ(arm64.X13, e.deopt)
			}
			// Resolve slotsBase pointer once (inlineSlots or PtrSmall(slotsRef))!
			d := e.dstG(loc, arm64.X15)
			e.a.LDRW(arm64.X13, tblR, jitTblSlotsRef)
			lExt := e.a.NewLabel()
			lDone := e.a.NewLabel()
			e.a.CBNZ(arm64.X13, lExt)
			e.a.ADDImm(d, tblR, uint32(jitTblInlineSlots))
			e.a.B(lDone)
			e.a.Bind(lExt)
			e.c.emitResolveSmall(arm64.X13, d, arm64.X14)
			e.a.Bind(lDone)
			e.storeG(loc, d)

		case ir.OpLoadSlotFloat:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			off := in.AuxIdx * 16
			if off <= 504 {
				e.a.LDP(arm64.X13, arm64.X14, baseR, off)
			} else {
				e.a.ADDImm(arm64.X15, baseR, uint32(off))
				e.a.LDP(arm64.X13, arm64.X14, arm64.X15, 0)
			}
			lFloat := e.a.NewLabel()
			lDone := e.a.NewLabel()
			d := e.dstF(loc, arm64.D24)
			e.a.CMPImm(arm64.X14, uint32(tagFloat))
			e.a.BCond(arm64.EQ, lFloat)
			e.a.CBNZ(arm64.X14, e.deopt)
			e.a.SCVTF(d, arm64.X13)
			e.a.B(lDone)
			e.a.Bind(lFloat)
			e.a.FMOVToF(d, arm64.X13)
			e.a.Bind(lDone)
			e.storeF(loc, d)

		case ir.OpLoadSlotInt:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			off := in.AuxIdx * 16
			if off <= 504 {
				e.a.LDP(arm64.X13, arm64.X14, baseR, off)
			} else {
				e.a.ADDImm(arm64.X15, baseR, uint32(off))
				e.a.LDP(arm64.X13, arm64.X14, arm64.X15, 0)
			}
			e.a.CBNZ(arm64.X14, e.deopt)
			e.storeG(loc, arm64.X13)

		case ir.OpStoreSlotFloat:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			valF := e.loadF(e.alloc.ValLoc[in.Args[1]], arm64.D24)
			e.a.FMOVToG(arm64.X13, valF)
			e.a.MOVZ(arm64.X14, uint16(tagFloat), 0)
			off := in.AuxIdx * 16
			if off <= 504 {
				e.a.STP(arm64.X13, arm64.X14, baseR, off)
			} else {
				e.a.ADDImm(arm64.X15, baseR, uint32(off))
				e.a.STP(arm64.X13, arm64.X14, arm64.X15, 0)
			}

		case ir.OpStoreSlotInt:
			baseR := e.loadG(e.alloc.ValLoc[in.Args[0]], arm64.X12)
			valR := e.loadG(e.alloc.ValLoc[in.Args[1]], arm64.X13)
			off := in.AuxIdx * 16
			if off <= 504 {
				e.a.STP(valR, arm64.XZR, baseR, off)
			} else {
				e.a.ADDImm(arm64.X15, baseR, uint32(off))
				e.a.STP(valR, arm64.XZR, arm64.X15, 0)
			}
		}
	}
}

func (e *a64OptEmit) emitLuaShift(isLeft bool, valR, amtR arm64.Reg, loc ir.RegLoc) {
	e.a.MOV(arm64.X14, valR)
	e.a.MOV(arm64.X15, amtR)
	lNeg := e.a.NewLabel()
	lZero := e.a.NewLabel()
	lDone := e.a.NewLabel()
	e.a.CMPImm(arm64.X15, 0)
	e.a.BCond(arm64.LT, lNeg)
	e.a.CMPImm(arm64.X15, 64)
	e.a.BCond(arm64.GE, lZero)
	if isLeft {
		e.a.LSL(arm64.X16, arm64.X14, arm64.X15)
	} else {
		e.a.LSR(arm64.X16, arm64.X14, arm64.X15)
	}
	e.a.B(lDone)
	e.a.Bind(lNeg)
	e.a.NEG(arm64.X15, arm64.X15)
	e.a.CMPImm(arm64.X15, 64)
	e.a.BCond(arm64.HS, lZero) // HS handles MinInt64 (-MinInt64 == MinInt64 >= 64 unsigned!)
	if isLeft {
		e.a.LSR(arm64.X16, arm64.X14, arm64.X15)
	} else {
		e.a.LSL(arm64.X16, arm64.X14, arm64.X15)
	}
	e.a.B(lDone)
	e.a.Bind(lZero)
	e.a.MOVZ(arm64.X16, 0, 0)
	e.a.Bind(lDone)
	e.storeG(loc, arm64.X16)
}

func (e *a64OptEmit) emitBlockTerm(blk *ir.Block, blockLabels []arm64.Label, lLoopTop, lChunkDone arm64.Label, isFor bool) {
	switch blk.Term.Kind {
	case ir.TermJump:
		e.a.B(blockLabels[blk.Term.Succ[0]])
	case ir.TermCondBr:
		cr := e.loadG(e.alloc.ValLoc[blk.Term.CondVal], arm64.X12)
		e.a.CBNZ(cr, blockLabels[blk.Term.Succ[0]])
		e.a.B(blockLabels[blk.Term.Succ[1]])
	case ir.TermExit:
		e.emitMaterializeVars(true)
		e.a.B(e.c.pcLabels[blk.Term.ExitTargetPC])
	case ir.TermBackedge:
		if !isFor {
			e.a.B(lLoopTop)
			return
		}
		// Advance idx += step, decrement rem -= 1; loop while rem > 0.
		idxR := e.loadG(e.alloc.VarLoc[e.idxVar], arm64.X12)
		if e.reg.HasConstStep {
			st := e.reg.ConstStep
			if st >= 0 && st < 4096 {
				e.a.ADDImm(idxR, idxR, uint32(st))
			} else if st < 0 && -st < 4096 {
				e.a.SUBImm(idxR, idxR, uint32(-st))
			} else {
				e.a.MOVImm64(arm64.X13, uint64(st))
				e.a.ADD(idxR, idxR, arm64.X13)
			}
		} else {
			stepR := e.loadG(e.alloc.ValLoc[e.stepVal], arm64.X13)
			e.a.ADD(idxR, idxR, stepR)
		}
		e.storeG(e.alloc.VarLoc[e.idxVar], idxR)

		remR := e.loadG(e.alloc.ValLoc[e.cntVal], arm64.X14)
		e.a.SUBSImm(remR, remR, 1)
		e.storeG(e.alloc.ValLoc[e.cntVal], remR)
		e.a.BCond(arm64.NE, lLoopTop)
		e.a.B(lChunkDone)
	}
}

func mapA64IntCond(c ir.CmpCond) arm64.Cond {
	switch c {
	case ir.CmpEQ:
		return arm64.EQ
	case ir.CmpNE:
		return arm64.NE
	case ir.CmpLT:
		return arm64.LT
	case ir.CmpLE:
		return arm64.LE
	case ir.CmpGT:
		return arm64.GT
	default:
		return arm64.GE
	}
}

func mapA64FPCond(c ir.CmpCond) arm64.Cond {
	// All six ARM64 FCMP conditions below are exact IEEE-754 NaN-safe!
	switch c {
	case ir.CmpEQ:
		return arm64.EQ
	case ir.CmpNE:
		return arm64.NE
	case ir.CmpLT:
		return arm64.MI
	case ir.CmpLE:
		return arm64.LS
	case ir.CmpGT:
		return arm64.GT
	default:
		return arm64.GE
	}
}
