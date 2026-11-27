//go:build (arm64 || amd64) && !purego && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"runtime"
	"unsafe"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/internal/arena"
)

// runFast executes Lua frames using the Plan 9 direct-threaded interpreter loop,
// bouncing to Go only for cold opcodes, IC misses, allocations, GC barriers, hooks,
// and periodic Go preemption safepoints.
func (s *State) runFast(th *Thread, entryDepth int) error {
	return s.runAsm(th, entryDepth)
}

func (s *State) runAsm(th *Thread, entryDepth int) error {
	var ev frameEnv
	ev.entryDepth = entryDepth
	var ctx asmExecCtx
	ctx.entryDepth = int64(entryDepth)
	ctx.th = uintptr(unsafe.Pointer(th))
	ctx.thSelf = uint32(th.self)
	ctx.sqrtIdx = staticMathSqrtIdx.Load()
	ctx.absIdx = staticMathAbsIdx.Load()

	var regs *[256]Value
	var pc int

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
		if (th.hookMask != 0 && !th.inHook) || s.hasInterruptControl() {
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

	var cachedUpvalCl *Closure
	var cachedUpvalPages uintptr
	for {
		// Refresh pointer-free asm execution context from live rooted slices.
		pagesPtr := uintptr(unsafe.Pointer(unsafe.SliceData(s.arena.PagePtrs())))
		ctx.regs = uintptr(unsafe.Pointer(regs))
		ctx.insns = uintptr(ev.insns.p)
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
		ctx.protoEpoch = s.protoEpoch
		ctx.pc = int64(pc)
		ctx.poll = asmPollInterval
		ctx.ci = int64(ev.ci)
		ctx.base = int64(ev.base)

	reenter:
		exitCode := runAsmExec(&ctx)
		if exitCode == asmExitDone {
			runtime.KeepAlive(s)
			return nil
		}
		if exitCode == asmExitPoll {
			ctx.poll = asmPollInterval
			goto reenter
		}
		if exitCode == asmExitReload {
			goto reloadFrame
		}

		// asmExitCold: sync frameEnv only when fast OP_CALL/OP_TAILCALL/OP_RETURN inside asm
		// changed the active frame or closure.
		if ci := int(ctx.ci); ci != ev.ci || int(ctx.base) != ev.base || ctx.curBP != uintptr(unsafe.Pointer(ev.bp)) {
			ev.ci = ci
			ev.base = int(ctx.base)
			cf = &ev.frames[ev.ci]
			ev.cl = s.frameClosure(cf)
			if bp := s.boundProtoOf(ev.cl); bp != ev.bp {
				ev.bp = bp
				ev.consts = bp.consts
				ev.insns = bp.insns
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
		if (th.hookMask != 0 && !th.inHook) || s.hasInterruptControl() {
			if !reload {
				th.framesSlice(s.arena)[ev.ci].pc = nextPC
			}
			runtime.KeepAlive(s)
			return s.runHooked(th, entryDepth)
		}
		if reload {
			goto reloadFrame
		}
		pc = nextPC
		ev.frames = th.framesSlice(s.arena)
		ev.stack = th.stackSlice(s.arena)
		regs = windowIn(ev.stack, ev.base)
	}
}

// stepAsmCold executes one instruction that exited the Plan 9 fast loop (cold opcode,
// IC miss, allocation, GC barrier, metamethod, vararg call, or native call).
//
//go:noinline
func (s *State) stepAsmCold(th *Thread, ev *frameEnv, ipc int, w bytecode.Instruction, regs *[256]Value, entryDepth int) (nextPC int, reload bool, done bool, err error) {
	pc := ipc + 1
	a := uint8(w >> bytecode.PosA)

	switch bytecode.OpCode(w & 0x7f) {
	case bytecode.OP_GETUPVAL:
		uv := s.closureUpval(ev.cl, int(uint8(w>>bytecode.PosB)))
		if uv.isClosed() {
			regs[a] = uv.closed
		} else if uv.isOwnedBy(th) {
			regs[a] = ev.stack[uv.stackIdx]
		} else {
			regs[a] = s.getCrossThreadUpval(uv)
		}
		return pc, false, false, nil

	case bytecode.OP_SETUPVAL:
		uv := s.closureUpval(ev.cl, int(uint8(w>>bytecode.PosB)))
		val := regs[a]
		if uv.isClosed() {
			s.setClosedUpvalVal(uv, val)
		} else if uv.isOwnedBy(th) {
			ev.stack[uv.stackIdx] = val
		} else {
			s.setCrossThreadUpval(uv, val)
		}
		return pc, false, false, nil

	case bytecode.OP_GETTABUP:
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.getTabUpSlow(th, ev, ipc, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_GETFIELD:
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.getFieldSlow(th, ev, ipc, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_SETFIELD:
		ra := regs[a]
		if ra.IsTable() {
			t := s.asTable(ra)
			var val Value
			if w.K() {
				val = ev.consts[uint8(w>>bytecode.PosC)]
			} else {
				val = regs[uint8(w>>bytecode.PosC)]
			}
			ic := icAt(ev.bp.ic, ev.bp.icSlots[ipc])
			if ic.setFieldFast(s, t, val, s.protoEpoch) {
				return pc, false, false, nil
			}
		}
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.setFieldSlow(th, ev, ipc, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_NEWTABLE:
		if !s.gcNeedsStep() && s.memLimit == 0 && !w.K() && w.VC() == 0 && w.VB() <= 2 {
			if t := s.newSmallRecordTable(); t != nil {
				regs[a] = TableValue(t)
				return pc + 1, false, false, nil
			}
		}
		goto cold

	case bytecode.OP_SELF:
		regs[a+1] = regs[uint8(w>>bytecode.PosB)]
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.selfSlow(th, ev, ipc, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_GETI:
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.getISlow(th, ev, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_GETTABLE:
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.getTableSlow(th, ev, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_SETI:
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.setISlow(th, ev, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_SETTABLE:
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.setTableSlow(th, ev, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_ADDI:
		// Reached only when rb is neither int nor float: fall through to OP_MMBINI at pc.
		return pc, false, false, nil

	case bytecode.OP_LEN:
		rb := regs[uint8(w>>bytecode.PosB)]
		if rb.IsTable() {
			t := s.asTable(rb)
			if !t.hasMeta() {
				regs[a].SetInt(t.Len(s))
				return pc, false, false, nil
			}
		} else if rb.IsString() {
			regs[a].SetInt(int64(s.asString(rb).Len))
			return pc, false, false, nil
		}
		goto cold

	case bytecode.OP_CALL:
		fn := regs[a]
		b := int(uint8(w >> bytecode.PosB))
		c := int(uint8(w >> bytecode.PosC))
		nargs := b - 1
		if b == 0 {
			nargs = max(0, th.top-(ev.base+int(a)+1))
		}
		if fn.IsNative() {
			nf := s.asNative(fn)
			if nf.Intrinsic != intrNone && (c == 2 || c == 0) && nargs >= 1 {
				if execIntrinsic1(s, nf.Intrinsic, regs, a, nargs) {
					if c == 0 {
						th.top = ev.base + int(a) + 1
					} else if b == 0 {
						if end := ev.base + int(ev.bp.maxStack); th.top < end {
							th.top = end
						}
					}
					return pc, false, false, nil
				}
			}
		}
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.callSlow(th, ev, w, int(a), nargs, c-1)
		return pc, pushed, false, serr

	case bytecode.OP_FORPREP:
		ev.frames[ev.ci].pc = pc
		skip, serr := s.forPrepSlow(regs, a)
		if serr != nil {
			if s.handleError(th, entryDepth, serr) {
				return 0, true, false, nil
			}
			return 0, false, false, serr
		}
		if skip {
			pc += w.Bx() + 1
		}
		return pc, false, false, nil

	case bytecode.OP_TFORCALL:
		fn := regs[a]
		c := int(uint8(w >> bytecode.PosC))
		if fn.IsNative() && s.asNative(fn).Intrinsic == intrIPairsIter && regs[a+1].IsTable() {
			t := s.asTable(regs[a+1])
			if !t.hasMeta() && regs[a+3].IsInt() {
				ni := regs[a+3].Int() + 1
				v := t.RawGetInt(s, ni)
				if v.IsNil() {
					regs[a+3].SetNil()
				} else {
					regs[a+3].SetInt(ni)
					if c >= 2 {
						regs[a+4] = v
						for k := 2; k < c; k++ {
							regs[int(a)+3+k].SetNil()
						}
					}
				}
				w2 := ev.insns.at(pc)
				pc++
				if !regs[a+3].IsNil() {
					pc -= w2.Bx()
				}
				return pc, false, false, nil
			}
		}
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.tforCallSlow(th, ev, a, c)
		return pc, pushed, false, serr
	}

cold:
	ev.frames[ev.ci].pc = pc
	action, newPC, cerr := s.coldOp(th, ev, ipc, w, regs, entryDepth)
	if cerr != nil {
		return 0, false, false, cerr
	}
	switch action {
	case coldContinue:
		return newPC, false, false, nil
	case coldReload:
		return 0, true, false, nil
	case coldReturn:
		return 0, false, true, nil
	}
	return pc, false, false, nil
}
