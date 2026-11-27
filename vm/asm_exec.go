//go:build (arm64 || amd64) && !purego && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"runtime"
	"unsafe"

	"github.com/yuchanns/moelua/internal/arena"
)

// runFast executes Lua frames using the optional native JIT when enabled, otherwise the
// Plan 9 direct-threaded interpreter loop, bouncing to Go only for cold opcodes, IC misses,
// allocations, GC barriers, hooks, and periodic Go preemption safepoints.
func (s *State) runFast(th *Thread, entryDepth int) error {
	if s.jit != nil && s.jit.enabled {
		return s.runJIT(th, entryDepth)
	}
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
			if s.abortErr != nil || s.intrFlag != nil || s.ctx != nil {
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
				reload, err := s.checkAsyncInterrupt(th, &ev, int(ctx.pc), true, entryDepth)
				if err != nil {
					runtime.KeepAlive(s)
					return err
				}
				if reload {
					goto reloadFrame
				}
			}
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
	}
}
