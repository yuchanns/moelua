//go:build (arm64 || amd64) && !moeluanojit && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"runtime"
	"unsafe"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/internal/arena"
	"github.com/yuchanns/moelua/internal/jit/call"
)

// protoHasLoop reports whether p contains any numeric/generic loop or backward branch.
// Protos with loops compile on first entry so top-level compute scripts immediately
// execute natively; loop-free functions wait until jitCompileThreshold calls.
func protoHasLoop(p *bytecode.Proto) bool {
	for _, w := range p.Code {
		switch w.Op() {
		case bytecode.OP_FORLOOP, bytecode.OP_TFORLOOP:
			return true
		case bytecode.OP_JMP:
			if w.SJ() < 0 {
				return true
			}
		}
	}
	return false
}

// getOrCompileJIT returns the compiled jitFunc for bp if already compiled or once
// its hotness threshold is reached. Returns nil while the function remains cold or
// if compilation fails/is skipped.
func (j *jitState) getOrCompileJIT(bp *boundProto) *jitFunc {
	idx := bp.protoIdx
	j.ensureIdx(idx)
	if f := j.funcs[idx]; f != nil {
		if f.proto == bp.proto {
			return f
		}
		j.invalidateProto(idx)
	}
	if j.entries[idx] == jitEntrySkip {
		return nil
	}
	h := j.hotness[idx] + 1
	j.hotness[idx] = h
	if h < jitCompileThreshold && !protoHasLoop(bp.proto) {
		return nil
	}
	f, err := compileBaselineNative(bp)
	if err != nil || f == nil || len(f.pcEntries) == 0 {
		j.entries[idx] = jitEntrySkip
		return nil
	}
	j.funcs[idx] = f
	j.entries[idx] = f.pcEntries[0]
	j.pcTables[idx] = uintptr(unsafe.Pointer(&f.pcEntries[0]))
	return f
}

// runJIT executes Lua frames using the native baseline JIT for hot/looping functions,
// falling back to single-instruction stepAsmCold for cold opcodes, IC misses, metamethods,
// allocations, GC barriers, hooks, and cold functions.
func (s *State) runJIT(th *Thread, entryDepth int) error {
	trampEntry, trampEpilogue, ok := getJITTrampoline()
	if !ok {
		if s.hasInterruptControl() {
			return s.runHooked(th, entryDepth)
		}
		return s.runFallbackNoJIT(th, entryDepth)
	}

	var ev frameEnv
	ev.entryDepth = entryDepth
	var ctx jitExecCtx
	ctx.entryDepth = int64(entryDepth)
	ctx.th = uintptr(unsafe.Pointer(th))
	ctx.thSelf = uint32(th.self)
	ctx.epilogue = trampEpilogue
	ctx.sqrtIdx = staticMathSqrtIdx.Load()
	ctx.absIdx = staticMathAbsIdx.Load()

	var regs *[256]Value
	var pc int
	var jf *jitFunc

reloadFrame:
	if th.nframe <= entryDepth {
		return nil
	}
	ev.frames = th.framesSlice(s.arena)
	cf := &ev.frames[th.nframe-1]
	if cf.kind != frameLua || cf.finish != finishNone {
		done, err := s.prepareLuaFrame(th, entryDepth)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if s.needsHookedRun(th) {
			return s.runHooked(th, entryDepth)
		}
		ev.frames = th.framesSlice(s.arena)
		cf = &ev.frames[th.nframe-1]
	}
	ev.ci = th.nframe - 1
	ev.cl = s.frameClosure(cf)
	if bp := s.boundProtoOf(ev.cl); bp != ev.bp {
		ev.bp = bp
		ev.consts = bp.consts
		ev.insns = bp.insns
	}
	ev.base = cf.base
	pc = cf.pc
	ev.stack = th.stackSlice(s.arena)
	regs = windowIn(ev.stack, ev.base)
	if s.abortErr != nil || s.intrFlag != nil || s.ctx != nil {
		reload, err := s.checkAsyncInterrupt(th, &ev, pc, true, entryDepth)
		if err != nil {
			return err
		}
		if reload {
			goto reloadFrame
		}
	}
	jf = s.jit.getOrCompileJIT(ev.bp)

	var cachedUpvalCl *Closure
	var cachedUpvalPages uintptr
	for {
		if jf == nil {
			// Cold loop-free function (or compilation skipped): step until return or hot callee.
			ipc := pc
			w := ev.insns.at(ipc)
			nextPC, reload, done, err := s.stepJITUncompiled(th, &ev, ipc, w, regs, entryDepth)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			if s.needsHookedRun(th) {
				if !reload {
					th.framesSlice(s.arena)[ev.ci].pc = nextPC
				}
				return s.runHooked(th, entryDepth)
			}
			if reload {
				goto reloadFrame
			}
			if s.abortErr != nil || s.intrFlag != nil || s.ctx != nil {
				ireload, ierr := s.checkAsyncInterrupt(th, &ev, nextPC, true, entryDepth)
				if ierr != nil {
					return ierr
				}
				if ireload {
					goto reloadFrame
				}
			}
			pc = nextPC
			ev.frames = th.framesSlice(s.arena)
			ev.stack = th.stackSlice(s.arena)
			regs = windowIn(ev.stack, ev.base)
			continue
		}

		// Refresh pointer-free JIT execution context from live rooted slices.
		pagesPtr := uintptr(unsafe.Pointer(unsafe.SliceData(s.arena.PagePtrs())))
		ctx.regs = uintptr(unsafe.Pointer(regs))
		ctx.consts = uintptr(unsafe.Pointer(unsafe.SliceData(ev.consts)))
		ctx.pagePtrs = pagesPtr
		ctx.largeSpans = uintptr(unsafe.Pointer(unsafe.SliceData(s.arena.LargeSpans())))
		ctx.stack = uintptr(unsafe.Pointer(unsafe.SliceData(ev.stack)))
		ctx.frames = uintptr(unsafe.Pointer(unsafe.SliceData(ev.frames)))
		ctx.protos = uintptr(unsafe.Pointer(unsafe.SliceData(s.protos)))
		ctx.ic = uintptr(unsafe.Pointer(unsafe.SliceData(ev.bp.ic)))
		ctx.icSlots = uintptr(unsafe.Pointer(unsafe.SliceData(ev.bp.icSlots)))
		if ev.cl != cachedUpvalCl || pagesPtr != cachedUpvalPages {
			cachedUpvalCl = ev.cl
			cachedUpvalPages = pagesPtr
			if ev.cl.upvalsRef != arena.NullRef {
				ctx.clUpvals = uintptr(s.arena.PtrSmall(ev.cl.upvalsRef))
			} else {
				ctx.clUpvals = 0
			}
		}
		ctx.curBP = uintptr(unsafe.Pointer(ev.bp))
		ctx.jitEntries = uintptr(unsafe.Pointer(unsafe.SliceData(s.jit.entries)))
		ctx.jitPCTables = uintptr(unsafe.Pointer(unsafe.SliceData(s.jit.pcTables)))
		ctx.jitProtoCap = int64(len(s.jit.entries))
		ctx.protoEpoch = s.protoEpoch
		ctx.pc = int64(pc)
		ctx.poll = jitPollInterval
		ctx.ci = int64(ev.ci)
		ctx.base = int64(ev.base)
		ctx.deoptID = 0
		ctx.entryPC = jf.pcEntries[pc]

	reenter:
		exitCode := call.Call(trampEntry, unsafe.Pointer(&ctx))
		if exitCode == jitExitDone {
			runtime.KeepAlive(s)
			return nil
		}
		if exitCode == jitExitPoll {
			// Sync active frame if native Lua-to-Lua call/return changed it before polling.
			if ci := int(ctx.ci); ci != ev.ci || int(ctx.base) != ev.base || ctx.curBP != uintptr(unsafe.Pointer(ev.bp)) {
				ev.ci = ci
				ev.base = int(ctx.base)
				cf = &ev.frames[ev.ci]
				ev.cl = s.frameClosure(cf)
				if bp := s.boundProtoOf(ev.cl); bp != ev.bp {
					ev.bp = bp
					ev.consts = bp.consts
					ev.insns = bp.insns
					jf = s.jit.funcs[bp.protoIdx]
				}
				regs = windowIn(ev.stack, ev.base)
			} else if cf := &ev.frames[ev.ci]; cf.clRef != ev.cl.self {
				ev.cl = s.frameClosure(cf)
			}
			pc = int(ctx.pc)
			if s.abortErr != nil || s.intrFlag != nil || s.ctx != nil {
				reload, err := s.checkAsyncInterrupt(th, &ev, pc, true, entryDepth)
				if err != nil {
					runtime.KeepAlive(s)
					return err
				}
				if reload {
					goto reloadFrame
				}
			}
			if jf == nil || pc < 0 || pc >= len(jf.pcEntries) {
				goto reloadFrame
			}
			ctx.poll = jitPollInterval
			ctx.entryPC = jf.pcEntries[pc]
			goto reenter
		}
		if exitCode == jitExitReload {
			goto reloadFrame
		}

		// jitExitCold: sync frameEnv if native OP_CALL/OP_TAILCALL/OP_RETURN changed active frame.
		if ci := int(ctx.ci); ci != ev.ci || int(ctx.base) != ev.base || ctx.curBP != uintptr(unsafe.Pointer(ev.bp)) {
			ev.ci = ci
			ev.base = int(ctx.base)
			cf = &ev.frames[ev.ci]
			ev.cl = s.frameClosure(cf)
			if bp := s.boundProtoOf(ev.cl); bp != ev.bp {
				ev.bp = bp
				ev.consts = bp.consts
				ev.insns = bp.insns
				jf = s.jit.funcs[bp.protoIdx]
			}
			regs = windowIn(ev.stack, ev.base)
		} else if cf := &ev.frames[ev.ci]; cf.clRef != ev.cl.self {
			ev.cl = s.frameClosure(cf)
		}
		ipc := int(ctx.pc)
		w := ev.insns.at(ipc)
		nextPC, reload, done, err := s.stepAsmCold(th, &ev, ipc, w, regs, entryDepth)
		if err != nil {
			runtime.KeepAlive(s)
			return err
		}
		if done {
			runtime.KeepAlive(s)
			return nil
		}
		if s.needsHookedRun(th) {
			if !reload {
				th.framesSlice(s.arena)[ev.ci].pc = nextPC
			}
			runtime.KeepAlive(s)
			return s.runHooked(th, entryDepth)
		}
		if reload {
			goto reloadFrame
		}
		if s.abortErr != nil || s.intrFlag != nil || s.ctx != nil {
			ireload, ierr := s.checkAsyncInterrupt(th, &ev, nextPC, true, entryDepth)
			if ierr != nil {
				runtime.KeepAlive(s)
				return ierr
			}
			if ireload {
				goto reloadFrame
			}
		}
		pc = nextPC
		ev.frames = th.framesSlice(s.arena)
		ev.stack = th.stackSlice(s.arena)
		regs = windowIn(ev.stack, ev.base)
		if jf == nil || jf.proto != ev.bp.proto {
			jf = s.jit.getOrCompileJIT(ev.bp)
		}
	}
}

func (s *State) runFallbackNoJIT(th *Thread, entryDepth int) error {
	return s.run(th, entryDepth)
}

// stepJITUncompiled executes a single bytecode instruction for a cold loop-free function
// (before it reaches jitCompileThreshold) or an uncompilable function, delegating all
// complex opcodes to stepAsmCold/coldOp.
//
//go:noinline
func (s *State) stepJITUncompiled(th *Thread, ev *frameEnv, ipc int, w bytecode.Instruction, regs *[256]Value, entryDepth int) (nextPC int, reload bool, done bool, err error) {
	pc := ipc + 1
	a := uint8(w >> bytecode.PosA)

	switch bytecode.OpCode(w & 0x7f) {
	case bytecode.OP_MOVE:
		regs[a] = regs[uint8(w>>bytecode.PosB)]
		return pc, false, false, nil
	case bytecode.OP_LOADI:
		regs[a].SetInt(int64(w.SBx()))
		return pc, false, false, nil
	case bytecode.OP_LOADF:
		regs[a].SetFloat(float64(w.SBx()))
		return pc, false, false, nil
	case bytecode.OP_LOADK:
		regs[a] = ev.consts[w.Bx()]
		return pc, false, false, nil
	case bytecode.OP_LOADFALSE:
		regs[a].SetBool(false)
		return pc, false, false, nil
	case bytecode.OP_LFALSESKIP:
		regs[a].SetBool(false)
		return pc + 1, false, false, nil
	case bytecode.OP_LOADTRUE:
		regs[a].SetBool(true)
		return pc, false, false, nil
	case bytecode.OP_LOADNIL:
		b := int(uint8(w >> bytecode.PosB))
		for i := 0; i <= b; i++ {
			regs[uint8(int(a)+i)].SetNil()
		}
		return pc, false, false, nil
	case bytecode.OP_ADDI:
		rb := regs[uint8(w>>bytecode.PosB)]
		sc := int64(w.SC())
		if rb.IsInt() {
			regs[a].SetInt(rb.Int() + sc)
			return pc + 1, false, false, nil
		}
		if rb.IsFloat() {
			regs[a].SetFloat(rb.Float() + float64(sc))
			return pc + 1, false, false, nil
		}
		return pc, false, false, nil
	case bytecode.OP_ADDK:
		rb, kc := regs[uint8(w>>bytecode.PosB)], ev.consts[uint8(w>>bytecode.PosC)]
		if BothInt(rb, kc) {
			regs[a].SetInt(rb.Int() + kc.Int())
			return pc + 1, false, false, nil
		}
		if BothFloat(rb, kc) {
			regs[a].SetFloat(rb.Float() + kc.Float())
			return pc + 1, false, false, nil
		}
	case bytecode.OP_SUBK:
		rb, kc := regs[uint8(w>>bytecode.PosB)], ev.consts[uint8(w>>bytecode.PosC)]
		if BothInt(rb, kc) {
			regs[a].SetInt(rb.Int() - kc.Int())
			return pc + 1, false, false, nil
		}
		if BothFloat(rb, kc) {
			regs[a].SetFloat(rb.Float() - kc.Float())
			return pc + 1, false, false, nil
		}
	case bytecode.OP_MULK:
		rb, kc := regs[uint8(w>>bytecode.PosB)], ev.consts[uint8(w>>bytecode.PosC)]
		if BothInt(rb, kc) {
			regs[a].SetInt(rb.Int() * kc.Int())
			return pc + 1, false, false, nil
		}
		if BothFloat(rb, kc) {
			regs[a].SetFloat(rb.Float() * kc.Float())
			return pc + 1, false, false, nil
		}
	case bytecode.OP_ADD:
		rb, rc := regs[uint8(w>>bytecode.PosB)], regs[uint8(w>>bytecode.PosC)]
		if BothInt(rb, rc) {
			regs[a].SetInt(rb.Int() + rc.Int())
			return pc + 1, false, false, nil
		}
		if BothFloat(rb, rc) {
			regs[a].SetFloat(rb.Float() + rc.Float())
			return pc + 1, false, false, nil
		}
	case bytecode.OP_SUB:
		rb, rc := regs[uint8(w>>bytecode.PosB)], regs[uint8(w>>bytecode.PosC)]
		if BothInt(rb, rc) {
			regs[a].SetInt(rb.Int() - rc.Int())
			return pc + 1, false, false, nil
		}
		if BothFloat(rb, rc) {
			regs[a].SetFloat(rb.Float() - rc.Float())
			return pc + 1, false, false, nil
		}
	case bytecode.OP_MUL:
		rb, rc := regs[uint8(w>>bytecode.PosB)], regs[uint8(w>>bytecode.PosC)]
		if BothInt(rb, rc) {
			regs[a].SetInt(rb.Int() * rc.Int())
			return pc + 1, false, false, nil
		}
		if BothFloat(rb, rc) {
			regs[a].SetFloat(rb.Float() * rc.Float())
			return pc + 1, false, false, nil
		}
	case bytecode.OP_UNM:
		rb := regs[uint8(w>>bytecode.PosB)]
		if rb.IsInt() {
			regs[a].SetInt(-rb.Int())
			return pc, false, false, nil
		}
		if rb.IsFloat() {
			regs[a].SetFloat(-rb.Float())
			return pc, false, false, nil
		}
	case bytecode.OP_NOT:
		regs[a].SetBool(regs[uint8(w>>bytecode.PosB)].IsFalsy())
		return pc, false, false, nil
	case bytecode.OP_JMP:
		return pc + w.SJ(), false, false, nil
	case bytecode.OP_LT:
		ra, rb := regs[a], regs[uint8(w>>bytecode.PosB)]
		if BothInt(ra, rb) {
			if (ra.Int() < rb.Int()) != w.K() {
				return pc + 1, false, false, nil
			}
			return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
		}
		if BothFloat(ra, rb) {
			if (ra.Float() < rb.Float()) != w.K() {
				return pc + 1, false, false, nil
			}
			return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
		}
	case bytecode.OP_LE:
		ra, rb := regs[a], regs[uint8(w>>bytecode.PosB)]
		if BothInt(ra, rb) {
			if (ra.Int() <= rb.Int()) != w.K() {
				return pc + 1, false, false, nil
			}
			return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
		}
		if BothFloat(ra, rb) {
			if (ra.Float() <= rb.Float()) != w.K() {
				return pc + 1, false, false, nil
			}
			return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
		}
	case bytecode.OP_EQI:
		ra := regs[a]
		im := int64(w.SB())
		var cond bool
		if ra.IsInt() {
			cond = ra.Int() == im
		} else if ra.IsFloat() {
			cond = ra.Float() == float64(im)
		}
		if cond != w.K() {
			return pc + 1, false, false, nil
		}
		return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
	case bytecode.OP_LTI:
		ra, im := regs[a], int64(w.SB())
		if ra.IsInt() {
			if (ra.Int() < im) != w.K() {
				return pc + 1, false, false, nil
			}
			return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
		}
		if ra.IsFloat() {
			if (ra.Float() < float64(im)) != w.K() {
				return pc + 1, false, false, nil
			}
			return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
		}
	case bytecode.OP_LEI:
		ra, im := regs[a], int64(w.SB())
		if ra.IsInt() {
			if (ra.Int() <= im) != w.K() {
				return pc + 1, false, false, nil
			}
			return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
		}
		if ra.IsFloat() {
			if (ra.Float() <= float64(im)) != w.K() {
				return pc + 1, false, false, nil
			}
			return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
		}
	case bytecode.OP_GTI:
		ra, im := regs[a], int64(w.SB())
		if ra.IsInt() {
			if (ra.Int() > im) != w.K() {
				return pc + 1, false, false, nil
			}
			return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
		}
		if ra.IsFloat() {
			if (ra.Float() > float64(im)) != w.K() {
				return pc + 1, false, false, nil
			}
			return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
		}
	case bytecode.OP_GEI:
		ra, im := regs[a], int64(w.SB())
		if ra.IsInt() {
			if (ra.Int() >= im) != w.K() {
				return pc + 1, false, false, nil
			}
			return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
		}
		if ra.IsFloat() {
			if (ra.Float() >= float64(im)) != w.K() {
				return pc + 1, false, false, nil
			}
			return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
		}
	case bytecode.OP_TEST:
		if regs[a].IsFalsy() == w.K() {
			return pc + 1, false, false, nil
		}
		return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
	case bytecode.OP_TESTSET:
		rb := regs[uint8(w>>bytecode.PosB)]
		if rb.IsFalsy() == w.K() {
			return pc + 1, false, false, nil
		}
		regs[a] = rb
		return pc + 1 + ev.insns.at(pc).SJ(), false, false, nil
	case bytecode.OP_FORLOOP:
		if regs[a+1].IsInt() {
			if regs[a].IsInt() && regs[a+2].IsInt() {
				cnt := uint64(regs[a].Int())
				if cnt > 0 {
					regs[a].SetInt(int64(cnt - 1))
					regs[a+2].SetInt(regs[a+2].Int() + regs[a+1].Int())
					pc -= w.Bx()
				}
			}
			return pc, false, false, nil
		}
		if regs[a+1].IsFloat() && regs[a].IsFloat() && regs[a+2].IsFloat() {
			step := regs[a+1].Float()
			idx := regs[a+2].Float() + step
			lim := regs[a].Float()
			if (step > 0 && idx <= lim) || (step <= 0 && idx >= lim) {
				regs[a+2].SetFloat(idx)
				pc -= w.Bx()
			}
		}
		return pc, false, false, nil
	case bytecode.OP_TFORLOOP:
		if !regs[a+3].IsNil() {
			pc -= w.Bx()
		}
		return pc, false, false, nil
	}
	return s.stepAsmCold(th, ev, ipc, w, regs, entryDepth)
}
