package vm

import (
	"errors"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/internal/arena"
)

// checkInstructionHook runs count & line debug hooks in runHooked() (never called from fast run()).
// Returns (reload, err) where reload==true means an error was caught by pcall/xpcall.
//
//go:noinline
func (s *State) checkInstructionHook(th *Thread, ev *frameEnv, pc int, entryDepth int) (bool, error) {
	err := s.checkInstructionHookImpl(th, ev, pc)
	if err != nil {
		if errors.Is(err, errYieldSentinel) {
			return false, errYieldSentinel
		}
		if s.handleError(th, entryDepth, err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func (s *State) checkInstructionHookImpl(th *Thread, ev *frameEnv, pc int) error {
	if s.abortErr != nil {
		return s.abortErr
	}
	if s.intrFlag != nil || s.ctx != nil {
		s.intrTicks++
		if s.intrTicks&0x3ff == 0 || s.intrTicks == 1 {
			if s.intrFlag != nil && s.intrFlag.Load() {
				s.intrFlag.Store(false)
				return &LuaError{Val: StringValue(s.intern.Intern("interrupted!"))}
			}
			if s.ctx != nil {
				if err := s.ctx.Err(); err != nil {
					s.abortErr = err
					return err
				}
			}
		}
	}
	// Per PUC-Rio ldebug.c:952-955 (CIST_HOOKYIELD): when resuming an instruction
	// previously suspended by a line/count hook yield, clear the flag and execute
	// the instruction without re-firing hooks or double-counting instruction limits.
	if th.hookYielded {
		th.hookYielded = false
		return nil
	}
	if s.insnLimit > 0 {
		s.insnCount++
		if s.insnCount > s.insnLimit {
			s.abortErr = ErrInstructionLimit
			return ErrInstructionLimit
		}
	}
	if th.hookMask == 0 || th.inHook || th.suppressHook {
		return nil
	}
	p := ev.bp.proto
	// Per Lua 5.5 lvm.c/ldebug.c: OP_VARARGPREP at pc==0 is prologue, not a traced user instruction.
	if pc == 0 && len(p.Code) > 0 && p.Code[0].Op() == bytecode.OP_VARARGPREP {
		return nil
	}
	frames := th.framesSlice(s.arena)
	frames[ev.ci].dirty = true
	frames[ev.ci].atCurrentPC = true
	defer func() {
		// Keep atCurrentPC set while suspended via hook yield so debug.getinfo/getlocal/traceback
		// inspect the exact suspended instruction; resumeThread clears it upon resume.
		if th.hookYielded {
			return
		}
		fs := th.framesSlice(s.arena)
		if ev.ci < len(fs) {
			fs[ev.ci].atCurrentPC = false
		}
	}()
	if th.hookMask&HookMaskCount != 0 {
		th.hookCount--
		if th.hookCount <= 0 {
			th.hookCount = th.baseHookCount
			if err := s.fireHook(th, HookCount, -1); err != nil {
				return err
			}
		}
	}
	if th.hookMask&HookMaskLine != 0 && ev.bp != nil {
		cf := &th.framesSlice(s.arena)[ev.ci]
		line := getFuncLine(p, pc)
		oldPC := cf.oldPC
		fire := false
		if oldPC < 0 {
			fire = true
		} else if pc <= oldPC {
			fire = true
		} else {
			oldLine := getFuncLine(p, oldPC)
			if line != oldLine {
				fire = true
			}
		}
		cf.oldPC = pc
		if fire {
			if err := s.fireHook(th, HookLine, line); err != nil {
				return err
			}
		}
	}
	return nil
}

//go:noinline
func (s *State) fireCallHook(th *Thread, ci, nargs, nparams, entryDepth int) (bool, error) {
	if th.hookMask&HookMaskCall == 0 || th.inHook || th.suppressHook {
		return false, nil
	}
	nt := min(nargs, nparams)
	cf := &th.framesSlice(s.arena)[ci]
	cf.dirty = true
	cf.ftransfer = 1
	cf.ntransfer = uint16(nt)
	if err := s.fireHook(th, HookCall, -1); err != nil {
		if s.handleError(th, entryDepth, err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// fireLuaCallHook triggers a Lua "call" hook for the newly pushed Lua frame at th.frames[th.nframe-1].
func (s *State) fireLuaCallHook(th *Thread, cl *Closure, nargs int) error {
	if th.hookMask&HookMaskCall == 0 || th.inHook || th.suppressHook {
		return nil
	}
	nt := min(nargs, int(s.boundProtoOf(cl).numParams))
	cf := &th.framesSlice(s.arena)[th.nframe-1]
	cf.ftransfer = 1
	cf.ntransfer = uint16(nt)
	return s.fireHook(th, HookCall, -1)
}

//go:noinline
func (s *State) fireHook(th *Thread, event HookEvent, line int) error {
	if !th.hasHook() || th.inHook || th.suppressHook {
		return nil
	}
	if th.hostHookRef != arena.NullGoHandle {
		hfn, ok := s.goRefs.GetAs[HostHookFunc](th.hostHookRef)
		if !ok || hfn == nil {
			return nil
		}
		if s.nCcalls >= maxCCalls {
			return &LuaError{Val: StringValue(s.intern.Intern("C stack overflow"))}
		}
		savedTop := th.top
		savedNFrame := th.nframe
		s.nCcalls++
		th.inHook = true
		defer func() {
			th.top = savedTop
			th.inHook = false
			s.nCcalls--
		}()
		err := hfn(s, th, event, line)
		if th.nframe > savedNFrame {
			return errCoroCloseSelf
		}
		if err == nil && s.abortErr != nil {
			err = s.abortErr
		}
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrHookYield) {
			if event != HookLine && event != HookCount {
				return &LuaError{Val: StringValue(s.intern.Intern("attempt to yield across a C-call boundary"))}
			}
			if !th.IsYieldable() {
				if s.isMainThread(th) {
					return &LuaError{Val: StringValue(s.intern.Intern("attempt to yield from outside a coroutine"))}
				}
				return &LuaError{Val: StringValue(s.intern.Intern("attempt to yield across a C-call boundary"))}
			}
			th.hookYielded = true
			th.clearYieldVals()
			return errYieldSentinel
		}
		return err
	}
	if th.hook.IsNil() {
		return nil
	}
	eventName := event.String()
	lineVal := NilValue()
	if event == HookLine && line >= 0 {
		lineVal = IntValue(int64(line))
	}
	th.inHook = true
	th.nny++
	defer func() {
		th.nny--
		th.inHook = false
	}()
	if th.hook.IsClosure() {
		cl := s.asClosure(th.hook)
		bp := s.boundProtoOf(cl)
		savedTop := th.top
		base := th.top
		if th.nframe > 0 {
			prev := &th.framesSlice(s.arena)[th.nframe-1]
			if prev.kind == frameLua {
				if prevCL := s.frameClosure(prev); prevCL != nil {
					if prevEnd := prev.base + int(s.boundProtoOf(prevCL).maxStack); prevEnd > base {
						base = prevEnd
					}
				}
			}
		}
		funcIdx := base
		if err := s.ensureStackWithRoots(th, funcIdx+3+int(bp.maxStack)+8, lineVal, nil); err != nil {
			th.top = savedTop
			return err
		}
		stack := th.stackSlice(s.arena)
		stack[funcIdx] = th.hook
		stack[funcIdx+1] = StringValue(s.intern.Intern(eventName))
		stack[funcIdx+2] = lineVal
		th.top = funcIdx + 3
		entryDepth := th.nframe
		if err := th.pushLuaFrame(s, cl, funcIdx, 2, 0); err != nil {
			th.top = savedTop
			return err
		}
		th.framesSlice(s.arena)[th.nframe-1].isHook = true
		var err error
		if s.needsHookedRun(th) {
			err = s.runHooked(th, entryDepth)
		} else {
			err = s.runFast(th, entryDepth)
		}
		if err != nil {
			if errors.Is(err, errCoroCloseSelf) {
				if errors.Is(s.abortErr, errCoroCloseSelf) {
					s.abortErr = nil
				}
				_, _, _ = th.closeAllSync(s, funcIdx, NilValue(), false)
				th.nframe = entryDepth
				th.top = savedTop
				return errCoroCloseSelf
			}
			if isUncatchableErr(err) {
				s.abortErr = err
				th.discardTBCAtOrAbove(s, funcIdx)
				th.closeUpvals(s, funcIdx)
				th.nframe = entryDepth
				th.top = savedTop
				return err
			}
			ev := s.runtimeErrorToValue(th, err)
			ev, _, _ = th.closeAllSync(s, funcIdx, ev, true)
			th.nframe = entryDepth
			th.top = savedTop
			if errors.Is(s.abortErr, errCoroCloseSelf) {
				return errCoroCloseSelf
			}
			if isUncatchableErr(s.abortErr) {
				return s.abortErr
			}
			return &LuaError{Val: ev}
		}
		th.top = savedTop
		return nil
	}
	_, err := s.callOnThread(th, th.hook, []Value{StringValue(s.intern.Intern(eventName)), lineVal})
	return err
}
