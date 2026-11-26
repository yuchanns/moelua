package vm

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/yuchanns/moelua/bytecode"
)

type coldAction uint8

const (
	coldContinue coldAction = iota
	coldReload
	coldReturn
)

func (s *State) catchSlow(th *Thread, ev *frameEnv, pushed bool, err error) (bool, error) {
	if err != nil {
		if s.handleError(th, ev.entryDepth, err) {
			return true, nil
		}
		if uerr := th.uncaughtErr(s); uerr != nil {
			err = uerr
			th.setUncaughtErr(s, nil)
		}
		if s.abortErr != nil {
			return false, s.abortErr
		}
		return false, err
	}
	return pushed, nil
}

// prepareLuaFrame completes any pending non-Lua boundary frame (framePCall/frameXPCall)
// or yieldable continuation (finish != finishNone) at the top of th.frames.
// Outlined out of (*State).run so reloadFrame stays a single fast branch.
//
//go:noinline
func (s *State) prepareLuaFrame(th *Thread, entryDepth int) (bool, error) {
	for th.nframe > entryDepth {
		cf := &th.framesSlice(s.arena)[th.nframe-1]
		if cf.finish != finishNone {
			cont, err := s.completeFinishOp(th, cf, entryDepth)
			if err != nil {
				if err == errYieldSentinel {
					return false, errYieldSentinel
				}
				if s.handleError(th, entryDepth, err) {
					continue
				}
				if uerr := th.uncaughtErr(s); uerr != nil {
					err = uerr
					th.setUncaughtErr(s, nil)
				}
				if s.abortErr != nil {
					return false, s.abortErr
				}
				return false, err
			}
			if !cont {
				continue
			}
			cf = &th.framesSlice(s.arena)[th.nframe-1]
		}
		if cf.kind != frameLua {
			done, err := s.finishPCallReturn(th, entryDepth)
			if err != nil {
				if s.handleError(th, entryDepth, err) {
					continue
				}
				if uerr := th.uncaughtErr(s); uerr != nil {
					err = uerr
					th.setUncaughtErr(s, nil)
				}
				if s.abortErr != nil {
					return false, s.abortErr
				}
				return false, err
			}
			if done {
				return true, nil
			}
			continue
		}
		return false, nil
	}
	return true, nil
}

// finishPCallReturn completes a protected call frame (framePCall / frameXPCall)
// whose callee returned normally, prepending `true` to the callee's results.
//
//go:noinline
func (s *State) finishPCallReturn(th *Thread, entryDepth int) (bool, error) {
	cf := &th.framesSlice(s.arena)[th.nframe-1]
	funcIdx := cf.funcIdx
	nres := cf.nresults
	calleeStart := cf.base
	got := th.top - calleeStart
	if got < 0 {
		got = 0
	}
	// Results are at stack[calleeStart : calleeStart+got].
	// We need [true, res0, res1, ...] at stack[funcIdx..].
	total := 1 + got
	if th.hookMask&HookMaskRet != 0 && !th.inHook && !th.suppressHook {
		if err := th.ensureStack(s, calleeStart+got+4); err != nil {
			return false, err
		}
		cf = &th.framesSlice(s.arena)[th.nframe-1]
		cf.ftransfer = 1
		cf.ntransfer = uint16(got)
		if herr := s.fireHook(th, "return", NilValue()); herr != nil {
			th.nframe--
			return false, herr
		}
	}
	if nres < 0 { // MULTRET
		if err := th.ensureStack(s, funcIdx+total+4); err != nil {
			return false, err
		}
		th.nframe--
		st := th.stackSlice(s.arena)
		copy(st[funcIdx+1:funcIdx+1+got], st[calleeStart:calleeStart+got])
		st[funcIdx] = TrueValue()
		th.top = funcIdx + total
	} else {
		if err := th.ensureStack(s, funcIdx+nres+4); err != nil {
			return false, err
		}
		th.nframe--
		if nres > 0 {
			st := th.stackSlice(s.arena)
			cp := min(got, nres-1)
			if cp > 0 {
				copy(st[funcIdx+1:funcIdx+1+cp], st[calleeStart:calleeStart+cp])
			}
			st[funcIdx] = TrueValue()
			if 1+cp < nres {
				fillNil(st[funcIdx+1+cp : funcIdx+nres])
			}
		}
		th.top = funcIdx + nres
	}
	return th.nframe <= entryDepth, nil
}

// handleError unwinds th.frames inside the flat loop (ZERO Go panic/recover!),
// closing upvalues & <close> TBC vars, catching errors at framePCall/frameXPCall.
// Returns true if caught by pcall/xpcall (caller should goto reloadFrame).
//
//go:noinline
func (s *State) callXPCallHandler(th *Thread, xpFrameIdx int, msgh Value, errVal Value) (Value, error) {
	if s.nCcalls >= maxCCallsErrHandler {
		return StringValue(s.intern.Intern("error in error handling")), nil
	}
	if xpFrameIdx >= 0 && xpFrameIdx < th.framesLen() {
		th.framesSlice(s.arena)[xpFrameIdx].errHandling = true
		defer func() {
			if xpFrameIdx < th.framesLen() {
				th.framesSlice(s.arena)[xpFrameIdx].errHandling = false
			}
		}()
	}
	prevErrH := th.inErrHandler
	th.inErrHandler = true
	defer func() { th.inErrHandler = prevErrH }()
	invokeMsgh := func(arg Value) ([]Value, error) {
		if msgh.IsNative() && (th.hookMask == 0 || th.inHook || th.suppressHook) {
			if ferr := s.ensureFrameCapWithRoots(th, th.nframe+1, maxFrames+128, msgh, []Value{arg}); ferr == nil {
				savedN := th.nframe
				th.framesSlice(s.arena)[savedN] = callFrame{
					kind:     frameNative,
					base:     th.top,
					funcIdx:  th.top,
					nativeFn: msgh,
					dirty:    true,
				}
				th.nframe = savedN + 1
				defer func() {
					th.nframe = savedN
				}()
			}
		}
		return s.callOnThread(th, msgh, []Value{arg})
	}
	for depth := 0; depth <= maxCCalls; depth++ {
		if depth == maxCCalls || s.nCcalls >= maxCCalls {
			// Give msgh one last chance with "C stack overflow" (errors.lua:639) using the
			// reserved headroom up to maxCCallsErrHandler; never lower s.nCcalls.
			overflowMsg := StringValue(s.intern.Intern("C stack overflow"))
			hres, herr := invokeMsgh(overflowMsg)
			if herr != nil {
				if errors.Is(herr, errCoroCloseSelf) || isUncatchableErr(herr) {
					return NilValue(), herr
				}
				return StringValue(s.intern.Intern("error in error handling")), nil
			}
			if len(hres) == 0 || hres[0].IsNil() {
				return StringValue(s.intern.Intern("<no error object>")), nil
			}
			return hres[0], nil
		}
		hres, herr := invokeMsgh(errVal)
		if herr != nil {
			if errors.Is(herr, errCoroCloseSelf) || isUncatchableErr(herr) {
				return NilValue(), herr
			}
			// Per Lua 5.5 ldo.c:luaG_errormsg, an error inside msgh re-invokes msgh with the new error (errors.lua:635)!
			errVal = s.rawRuntimeErrorToValue(th, herr)
			continue
		}
		if len(hres) == 0 || hres[0].IsNil() {
			return StringValue(s.intern.Intern("<no error object>")), nil
		}
		return hres[0], nil
	}
	return StringValue(s.intern.Intern("error in error handling")), nil
}

func (s *State) normalizeErrVal(v Value) Value {
	if v.IsNil() {
		return StringValue(s.intern.Intern("<no error object>"))
	}
	return v
}

func (s *State) runtimeErrorToValue(th *Thread, err error) Value {
	if err == nil {
		return NilValue()
	}
	return s.normalizeErrVal(s.rawRuntimeErrorToValue(th, err))
}

func (s *State) rawRuntimeErrorToValue(th *Thread, err error) Value {
	if err == nil {
		return NilValue()
	}
	var le *LuaError
	if errors.As(err, &le) {
		return le.Val
	}
	msg := err.Error()
	// Prepend Lua file:line: prefix for runtime VM errors (matching ldebug.c:luaG_addinfo / lauxlib.c:luaL_where(1)).
	// Do NOT walk past framePCall/frameXPCall (C boundary), so pcall(require, 'XXX') has no caller prefix (attrib.lua:67).
	if th != nil && th.nframe > 0 {
		frames := th.framesSlice(s.arena)
		topIdx := th.nframe - 1
		if frames[topIdx].kind == frameNative {
			topIdx--
		}
		if topIdx >= 0 {
			cf := &frames[topIdx]
			if cf.kind == frameLua && s.frameClosure(cf) != nil {
				p := s.boundProtoOf(s.frameClosure(cf)).proto
				pc := cf.pc
				if !cf.atCurrentPC && pc > 0 {
					pc--
				}
				line := getFuncLine(p, pc)
				var prefix string
				if line > 0 {
					prefix = fmt.Sprintf("%s:%d: ", shortSrc(p.Source), line)
				} else {
					// Stripped function with no debug line info (errors.lua:309: "?:?: ...")
					src := "?"
					if !p.NoSource && p.Source != "" {
						src = shortSrc(p.Source)
					}
					prefix = fmt.Sprintf("%s:?: ", src)
				}
				joined, jerr := s.joinStringsWithCancel(prefix, msg)
				if jerr != nil {
					if isUncatchableErr(jerr) || s.abortErr != nil {
						return NilValue()
					}
					return StringValue(s.intern.Intern("not enough memory"))
				}
				msg = joined
			}
		}
	}
	if s.memLimit > 0 {
		if ls, ierr := s.TryIntern(msg); ierr == nil {
			return StringValue(ls)
		}
		return StringValue(s.intern.Intern("not enough memory"))
	}
	return StringValue(s.intern.Intern(msg))
}

func (s *State) handleError(th *Thread, entryDepth int, err error) bool {
	if err == errYieldSentinel || err == errCoroCloseSelf || errors.Is(err, errYieldSentinel) || errors.Is(err, errCoroCloseSelf) || isUncatchableErr(err) {
		return false
	}
	var alreadyXPHandled bool
	isMem := isMemError(err)
	if le, ok := err.(*LuaError); ok && le.xpHandled {
		alreadyXPHandled = true
	} else {
		var le2 *LuaError
		if errors.As(err, &le2) && le2.xpHandled {
			alreadyXPHandled = true
		}
	}
	rawErrVal := s.rawRuntimeErrorToValue(th, err)
	if s.abortErr != nil {
		return false
	}
	// Locate nearest enclosing pcall/xpcall frame before unwinding anything.
	frames := th.framesSlice(s.arena)
	catchIdx := -1
	for j := th.nframe - 1; j >= entryDepth; j-- {
		if frames[j].kind == framePCall || frames[j].kind == frameXPCall {
			catchIdx = j
			break
		}
	}
	if catchIdx < 0 {
		// If this error is escaping a nested Go->Lua callback (e.g. table.sort/string.gsub)
		// beneath an enclosing xpcall frame, invoke that xpcall's message handler NOW while
		// the full nested call stack (including native frames) is still intact (ldebug.c:luaG_errormsg).
		// Per Lua 5.5 §2.3, memory-allocation errors (LUA_ERRMEM) never invoke message handlers.
		if !alreadyXPHandled && !isMem && entryDepth > 0 && th.uncaughtErr(s) == nil {
			outerCatch := -1
			for j := entryDepth - 1; j >= 0; j-- {
				if frames[j].kind == framePCall || frames[j].kind == frameXPCall {
					outerCatch = j
					break
				}
			}
			if outerCatch >= 0 && frames[outerCatch].kind == frameXPCall && !frames[outerCatch].errHandling {
				errVal, uerr := s.callXPCallHandler(th, outerCatch, frames[outerCatch].errFunc, rawErrVal)
				if uerr != nil {
					s.abortErr = uerr
					return false
				}
				th.setUncaughtErr(s, &LuaError{Val: errVal, xpHandled: true})
			}
		}
		// Uncaught error: leave TBC open on dead coroutine (for coroutine.close) or let top Call clean up.
		return false
	}
	isXP := frames[catchIdx].kind == frameXPCall
	var msgh Value
	var errVal Value
	if isXP && !alreadyXPHandled && !isMem {
		msgh = frames[catchIdx].errFunc
		// Call xpcall message handler BEFORE unwinding so debug.traceback/getinfo see full stack,
		// passing raw error value (including nil) per ldebug.c:luaG_errormsg.
		var uerr error
		errVal, uerr = s.callXPCallHandler(th, catchIdx, msgh, rawErrVal)
		if uerr != nil {
			s.abortErr = uerr
			return false
		}
	} else {
		errVal = s.normalizeErrVal(rawErrVal)
	}
	// Unwind frames down to catchIdx BEFORE running TBC __close so debug.getinfo(2) sees pcall/xpcall (coroutine.lua:249)!
	cf := &th.framesSlice(s.arena)[catchIdx]
	th.nframe = catchIdx + 1
	th.closeUpvals(s, cf.base)
	// If inside a yieldable coroutine and TBC vars remain, close them yieldably via finishClose (locals.lua:1015)!
	if th.nny == 0 && th.hasTBCAtOrAbove(s.arena, cf.base) {
		cf.closeLevel = cf.base
		cf.closeErr = errVal
		cf.closeHasErr = true
		cf.closeResume = 2 // finish protected-call error return after all TBC close
		cont, cerr := s.resumeCloseTBC(th, cf, entryDepth)
		if cerr != nil {
			if errors.Is(cerr, errCoroCloseSelf) || isUncatchableErr(cerr) {
				s.abortErr = cerr
				return false
			}
			// Nested sync error during yieldable TBC setup: recurse to handle!
			if s.handleError(th, entryDepth, cerr) {
				return true
			}
			if th.uncaughtErr(s) == nil {
				th.setUncaughtErr(s, cerr)
			}
			return false
		}
		if !cont {
			return true
		}
		// All closed synchronously (e.g. only falsy/native TBC) — fall through to place [false, errVal]!
		cf = &th.framesSlice(s.arena)[catchIdx]
		errVal = cf.closeErr
	} else {
		newErr, _, raised := th.closeAllSync(s, cf.base, errVal, true)
		if s.abortErr != nil {
			return false
		}
		if raised && isXP {
			var uerr error
			errVal, uerr = s.callXPCallHandler(th, catchIdx, th.framesSlice(s.arena)[catchIdx].errFunc, newErr)
			if uerr != nil {
				s.abortErr = uerr
				return false
			}
		} else {
			errVal = newErr
		}
	}
	if th.hookMask&HookMaskRet != 0 && !th.inHook && !th.suppressHook {
		if herr := s.fireHook(th, "return", NilValue()); herr != nil {
			th.nframe--
			if errors.Is(herr, errCoroCloseSelf) || isUncatchableErr(herr) {
				s.abortErr = herr
				return false
			}
			if s.handleError(th, entryDepth, herr) {
				return true
			}
			if th.uncaughtErr(s) == nil {
				th.setUncaughtErr(s, herr)
			}
			return false
		}
	}
	cf = &th.framesSlice(s.arena)[catchIdx]
	funcIdx := cf.funcIdx
	nres := cf.nresults
	th.nframe--
	if perr := th.placeResults(s, funcIdx, []Value{FalseValue(), errVal}, nres); perr != nil {
		if s.handleError(th, entryDepth, perr) {
			return true
		}
		if th.uncaughtErr(s) == nil {
			th.setUncaughtErr(s, perr)
		}
		return false
	}
	return true
}

// completeFinishOp finishes an interrupted Lua opcode after its yieldable metamethod frame returned.
// Returns (true, nil) to continue executing cf at cf.pc, or (false, nil) if another frame was pushed.
//
//go:noinline
func (s *State) completeFinishOp(th *Thread, cf *callFrame, entryDepth int) (bool, error) {
	fk := cf.finish
	cf.finish = finishNone
	retSlot := cf.oldTop
	st := th.stackSlice(s.arena)
	var res Value
	if th.top > retSlot {
		res = st[retSlot]
	} else {
		res = NilValue()
	}
	switch fk {
	case finishSetReg:
		st[cf.base+int(cf.finishReg)] = res
		return true, nil
	case finishSetDrop:
		return true, nil
	case finishCondJump:
		cond := !res.IsFalsy()
		if cond != cf.finishFlag {
			cf.pc++
		} else {
			cf.pc += 1 + s.boundProtoOf(s.frameClosure(cf)).proto.Code[cf.pc].SJ()
		}
		return true, nil
	case finishCondNot:
		cond := res.IsFalsy()
		if cond != cf.finishFlag {
			cf.pc++
		} else {
			cf.pc += 1 + s.boundProtoOf(s.frameClosure(cf)).proto.Code[cf.pc].SJ()
		}
		return true, nil
	case finishConcat:
		// Store metamethod result back at concatIdx and resume remaining right-to-left steps.
		st[cf.concatIdx] = res
		if cf.concatIdx > cf.concatStart {
			pushed, err := s.concatStep(th, cf, cf.concatStart, cf.concatIdx)
			if err != nil {
				return false, err
			}
			if pushed {
				return false, nil
			}
		}
		return true, nil
	case finishClose:
		return s.resumeCloseTBC(th, cf, entryDepth)
	case finishPairs:
		if cf.closeRetN < 0 {
			th.top = retSlot + 4
		} else if cf.closeRetN > 4 {
			if err := th.ensureStack(s, retSlot+cf.closeRetN+4); err != nil {
				return false, err
			}
			fillNil(th.stackSlice(s.arena)[retSlot+4 : retSlot+cf.closeRetN])
			end := retSlot + cf.closeRetN
			if s.frameClosure(cf) != nil {
				if fe := cf.base + int(s.boundProtoOf(s.frameClosure(cf)).proto.MaxStackSize); fe > end {
					end = fe
				}
			}
			th.top = end
		}
		return true, nil
	case finishTailReturn:
		nres := max(0, th.top-retSlot)
		if th.hookMask&HookMaskRet != 0 && !th.inHook && !th.suppressHook {
			cf.dirty = true
			cf.ftransfer = uint16(retSlot - cf.base + 1)
			cf.ntransfer = uint16(nres)
			if herr := s.fireHook(th, "return", NilValue()); herr != nil {
				return false, herr
			}
			cf = &th.framesSlice(s.arena)[th.nframe-1]
		}
		var out []Value
		if nres > 0 {
			out = make([]Value, nres)
			copy(out, th.stackSlice(s.arena)[retSlot:retSlot+nres])
		}
		funcIdx := cf.funcIdx
		want := cf.nresults
		if err := th.placeResults(s, funcIdx, out, want); err != nil {
			return false, err
		}
		th.nframe--
		if s.gcNeedsCheck() {
			s.gcCheckStep(th.top)
			if s.abortErr != nil {
				return false, s.abortErr
			}
		}
		return false, nil
	}
	return true, nil
}

// pushMetamethodFrame pushes a yieldable metamethod call onto th.frames (ZERO Go recursion!).
// Supports Lua closures, native functions (including coroutine.yield), and callable tables (__call chains).
// Returns (true, nil) if a Lua closure frame was pushed (caller must goto reloadFrame),
// or (false, nil) if completed inline via native function.
func (s *State) pushMetamethodFrame(th *Thread, ev *frameEnv, tm Value, args []Value, fk finishKind, reg uint8, flag bool) (bool, error) {
	var extraDepth uint8
	if !tm.IsClosure() && !tm.IsNative() {
		cur := tm
		curArgs := args
		for depth := 1; depth <= 15; depth++ {
			callTM := s.getTMByObj(cur, TMCall)
			if callTM.IsNil() {
				if depth == 1 {
					// Decorate non-callable metamethod error with "(metamethod '<name>')" per Lua 5.5 (errors.lua:175-182).
					cf := &th.framesSlice(s.arena)[ev.ci]
					p := s.boundProtoOf(s.frameClosure(cf)).proto
					ipc := cf.pc - 1
					if ipc >= 0 && ipc < len(p.Code) {
						w := p.Code[ipc]
						mm := ""
						switch w.Op() {
						case bytecode.OP_MMBIN, bytecode.OP_MMBINI, bytecode.OP_MMBINK:
							t := TM(w.C())
							if t < tmCount {
								mm = strings.TrimPrefix(tmNames[t], "__")
							}
						case bytecode.OP_LT, bytecode.OP_LTI, bytecode.OP_GTI:
							mm = "lt"
						case bytecode.OP_LE, bytecode.OP_LEI, bytecode.OP_GEI:
							if fk == finishCondNot {
								mm = "lt"
							} else {
								mm = "le"
							}
						case bytecode.OP_EQ:
							mm = "eq"
						case bytecode.OP_LEN:
							mm = "len"
						case bytecode.OP_UNM:
							mm = "unm"
						case bytecode.OP_BNOT:
							mm = "bnot"
						case bytecode.OP_CONCAT:
							mm = "concat"
						}
						if mm != "" {
							return false, fmt.Errorf("attempt to call a %s value (metamethod '%s')", cur.TypeName(), mm)
						}
					}
				}
				return false, fmt.Errorf("attempt to call a %s value", cur.TypeName())
			}
			nextArgs := make([]Value, 1+len(curArgs))
			nextArgs[0] = cur
			copy(nextArgs[1:], curArgs)
			curArgs = nextArgs
			cur = callTM
			extraDepth = uint8(depth)
			if cur.IsClosure() || cur.IsNative() {
				break
			}
		}
		if !cur.IsClosure() && !cur.IsNative() {
			return false, fmt.Errorf("'__call' chain too long")
		}
		tm = cur
		args = curArgs
	}
	cf := &th.framesSlice(s.arena)[ev.ci]
	slot := cf.base + int(s.boundProtoOf(s.frameClosure(cf)).proto.MaxStackSize)
	if th.top > slot {
		slot = th.top
	}
	if tm.IsClosure() {
		if err := s.ensureStackWithRoots(th, slot+1+len(args)+8, tm, args); err != nil {
			return false, err
		}
		st := th.stackSlice(s.arena)
		st[slot] = tm
		for i, a := range args {
			st[slot+1+i] = a
		}
		th.top = slot + 1 + len(args)
		cf = &th.framesSlice(s.arena)[ev.ci]
		cf.dirty = true
		cf.oldTop = slot
		cf.finish = fk
		cf.finishReg = reg
		cf.finishFlag = flag
		cl := s.asClosure(tm)
		if err := th.pushLuaFrame(s, cl, slot, len(args), 1); err != nil {
			fillNil(th.stackSlice(s.arena)[slot : slot+1+len(args)])
			th.top = slot
			th.framesSlice(s.arena)[ev.ci].finish = finishNone
			return false, err
		}
		th.framesSlice(s.arena)[th.nframe-1].extraArgs = extraDepth
		if herr := s.fireLuaCallHook(th, cl, len(args)); herr != nil {
			return false, herr
		}
		return true, nil
	}
	// Yieldable native metamethod invocation (supports coroutine.yield as a metamethod and debug hooks!).
	if err := s.ensureStackWithRoots(th, slot+1+len(args)+8, tm, args); err != nil {
		return false, err
	}
	st := th.stackSlice(s.arena)
	st[slot] = tm
	copy(st[slot+1:slot+1+len(args)], args)
	res, _, err := s.callNativeHooked(th, tm, slot, len(args), extraDepth, maxFrames+128)
	cf = &th.framesSlice(s.arena)[ev.ci]
	if errors.Is(err, errYieldSentinel) {
		cf.dirty = true
		cf.oldTop = slot
		cf.finish = fk
		cf.finishReg = reg
		cf.finishFlag = flag
		th.yieldDst = slot
		th.yieldNRes = 1
		th.yieldedNative = true
		th.yieldNativeFn = tm
		return false, errYieldSentinel
	}
	th.top = slot
	if err != nil {
		if !errors.Is(err, errCoroCloseSelf) {
			th.lastErrNative = tm
		}
		return false, err
	}
	v := NilValue()
	if len(res) > 0 {
		v = res[0]
	}
	switch fk {
	case finishSetReg:
		th.stackSlice(s.arena)[ev.base+int(reg)] = v
	case finishCondJump:
		if (!v.IsFalsy()) != flag {
			cf.pc++
		} else {
			cf.pc += 1 + s.boundProtoOf(s.frameClosure(cf)).proto.Code[cf.pc].SJ()
		}
	case finishCondNot:
		if v.IsFalsy() != flag {
			cf.pc++
		} else {
			cf.pc += 1 + s.boundProtoOf(s.frameClosure(cf)).proto.Code[cf.pc].SJ()
		}
	}
	if s.gcNeedsCheck() {
		s.gcCheckStep(th.top)
		if s.abortErr != nil {
			return false, s.abortErr
		}
	}
	return false, nil
}

//go:noinline
func (s *State) getTabUpSlow(th *Thread, ev *frameEnv, ipc int, w bytecode.Instruction, regs *[256]Value) (bool, error) {
	a := uint8(w >> bytecode.PosA)
	b := uint8(w >> bytecode.PosB)
	c := uint8(w >> bytecode.PosC)
	uv := s.closureUpval(ev.cl, int(b))
	tv := uv.Get(s)
	key := s.asString(ev.consts[c])
	if tv.IsTable() {
		t := s.asTable(tv)
		if err := s.ensureLookupStrKeyHash(t, key); err != nil {
			return s.catchSlow(th, ev, false, err)
		}
		if OptimizeEnabled {
			ic := icAt(ev.bp.ic, ev.bp.icSlots[ipc])
			if sh := t.shapePtr(s.arena); sh != nil && key.IsShort() {
				if slot := sh.lookupKey(s, key); slot >= 0 {
					v := t.slotAt(s.arena, int(slot))
					if !v.IsNil() || !t.hasMeta() {
						ic.setOwnSlotFor(sh, slot, !t.hasMeta(), false, 0)
						regs[a] = v
						return false, nil
					}
				}
			} else if t.shapePtr(s.arena) == nil {
				if slot, ok := t.hashFindStrSlot(s, key); ok {
					v := s.tableHashValAt(t, slot)
					if !v.IsNil() || !t.hasMeta() {
						ic.setEnvSlotFor(s, t, int32(slot), !t.hasMeta())
						regs[a] = v
						return false, nil
					}
				}
			}
		}
		v := t.RawGetStr(s, key)
		if !v.IsNil() || !t.hasMeta() {
			regs[a] = v
			return false, nil
		}
	}
	pushed, err := s.finishGetYieldable(th, ev, tv, ev.consts[c], a)
	return s.catchSlow(th, ev, pushed, err)
}

//go:noinline
func (s *State) getFieldSlow(th *Thread, ev *frameEnv, ipc int, w bytecode.Instruction, regs *[256]Value) (bool, error) {
	a := uint8(w >> bytecode.PosA)
	b := uint8(w >> bytecode.PosB)
	c := uint8(w >> bytecode.PosC)
	rb := regs[b]
	key := s.asString(ev.consts[c])
	if rb.IsTable() {
		t := s.asTable(rb)
		if sh := t.shapePtr(s.arena); OptimizeEnabled && sh != nil && key.IsShort() {
			ic := icAt(ev.bp.ic, ev.bp.icSlots[ipc])
			if ic.polyLookupFor(s, sh) {
				if v, ok := ic.tryPolyGetField(s, t); ok {
					regs[a] = v
					return false, nil
				}
			}
			if ic.differsFromShape(sh) {
				ic.polySavePrimaryFor(s)
			}
			if slot := sh.lookupKey(s, key); slot >= 0 {
				v := t.slotAt(s.arena, int(slot))
				if !v.IsNil() || !t.hasMeta() {
					ic.setOwnSlotFor(sh, slot, !t.hasMeta(), false, 0)
					regs[a] = v
					return false, nil
				}
				// Slot is nil and t.meta != nil: try table-only __index chain; if function, use yieldable call!
				if pv, fnTM, fnObj, cacheable, err := s.walkIndexChainOrFn(t, key); err != nil {
					return s.catchSlow(th, ev, false, err)
				} else if !fnTM.IsNil() {
					p, e := s.pushMetamethodFrame(th, ev, fnTM, []Value{fnObj, StringValue(key)}, finishSetReg, a, false)
					return s.catchSlow(th, ev, p, e)
				} else {
					if cacheable && pv.IsNil() {
						ic.setOwnSlotFor(sh, slot, false, true, s.protoEpoch)
					}
					regs[a] = pv
					return false, nil
				}
			} else if !t.hasMeta() {
				ic.setProtoValFor(sh, NilValue(), 0, true)
				regs[a] = NilValue()
				return false, nil
			} else {
				if v, fnTM, fnObj, cacheable, err := s.walkIndexChainOrFn(t, key); err != nil {
					return s.catchSlow(th, ev, false, err)
				} else if !fnTM.IsNil() {
					p, e := s.pushMetamethodFrame(th, ev, fnTM, []Value{fnObj, StringValue(key)}, finishSetReg, a, false)
					return s.catchSlow(th, ev, p, e)
				} else {
					if cacheable {
						ic.setProtoValFor(sh, v, s.protoEpoch, false)
					}
					regs[a] = v
					return false, nil
				}
			}
		}
		if err := s.ensureLookupStrKeyHash(t, key); err != nil {
			return s.catchSlow(th, ev, false, err)
		}
		v := t.RawGetStr(s, key)
		if !v.IsNil() || !t.hasMeta() {
			regs[a] = v
			return false, nil
		}
	}
	pushed, err := s.finishGetYieldable(th, ev, rb, ev.consts[c], a)
	return s.catchSlow(th, ev, pushed, err)
}

//go:noinline
func (s *State) selfSlow(th *Thread, ev *frameEnv, ipc int, w bytecode.Instruction, regs *[256]Value) (bool, error) {
	return s.getFieldSlow(th, ev, ipc, w, regs)
}

func (s *State) walkIndexChainOrFn(orig *Table, key *LuaString) (val Value, fnTM Value, fnObj Value, cacheable bool, err error) {
	cur := TableValue(orig)
	for loop := 0; loop < 2000; loop++ {
		var tm Value
		if cur.IsTable() {
			t := s.asTable(cur)
			if loop > 0 {
				t.markProto(s)
				if herr := s.ensureLookupStrKeyHash(t, key); herr != nil {
					return NilValue(), NilValue(), NilValue(), false, herr
				}
				v := t.RawGetStr(s, key)
				if !v.IsNil() {
					return v, NilValue(), NilValue(), true, nil
				}
			}
			if !t.hasMeta() {
				return NilValue(), NilValue(), NilValue(), true, nil
			}
			mt := t.Metatable(s)
			mt.markProto(s)
			tm = s.getTM(mt, TMIndex)
			if tm.IsNil() {
				return NilValue(), NilValue(), NilValue(), true, nil
			}
		} else {
			if mt := s.getMetatableOf(cur); mt != nil {
				mt.markProto(s)
			}
			tm = s.getTMByObj(cur, TMIndex)
			if tm.IsNil() {
				return NilValue(), NilValue(), NilValue(), false, fmt.Errorf("attempt to index a %s value", cur.TypeName())
			}
		}
		if tm.IsFunction() {
			return NilValue(), tm, cur, false, nil
		}
		cur = tm
	}
	return NilValue(), NilValue(), NilValue(), false, fmt.Errorf("'__index' chain too long; possible loop")
}

//go:noinline
func (s *State) setFieldSlow(th *Thread, ev *frameEnv, ipc int, w bytecode.Instruction, regs *[256]Value) (bool, error) {
	a := uint8(w >> bytecode.PosA)
	b := uint8(w >> bytecode.PosB)
	c := uint8(w >> bytecode.PosC)
	ra := regs[a]
	var val Value
	if w.K() {
		val = ev.consts[c]
	} else {
		val = regs[c]
	}
	key := s.asString(ev.consts[b])
	if ra.IsTable() {
		t := s.asTable(ra)
		if sh := t.shapePtr(s.arena); OptimizeEnabled && sh != nil && key.IsShort() {
			ic := icAt(ev.bp.ic, ev.bp.icSlots[ipc])
			if !t.isProto() && ic.polyLookupFor(s, sh) {
				if ic.tryPolySetField(s, t, val) {
					return false, nil
				}
			}
			if !t.isProto() && ic.differsFromShape(sh) {
				ic.polySavePrimaryFor(s)
			}
			if slot := sh.lookupKey(s, key); slot >= 0 {
				noNewIdx := !t.hasMeta() || s.getTM(t.Metatable(s), TMNewIndex).IsNil()
				sp := t.slotPtr(s.arena, int(slot))
				if !sp.IsNil() || noNewIdx {
					s.noteShapeMetaKeyWrite(t, key)
					*sp = val
					s.barrierTableVal(t, val)
					if t.tmFlags != 0 {
						t.tmFlags = 0
					}
					if t.isProto() {
						s.protoEpoch++
						if key == s.intern.tms[TMIndex] || key == s.intern.tms[TMNewIndex] {
							if val.IsTable() {
								s.asTable(val).markProto(s)
							}
						}
					} else {
						ic.setOwnSlotFor(sh, slot, !t.hasMeta(), noNewIdx && t.hasMeta(), s.protoEpoch)
					}
					return false, nil
				}
			} else if !t.hasMeta() || s.getTM(t.Metatable(s), TMNewIndex).IsNil() {
				if val.IsNil() {
					return false, nil
				}
				s.noteShapeMetaKeyWrite(t, key)
				oldSh := sh
				next, serr := s.transitionShapeFor(oldSh, key, TableValue(t), val)
				if serr != nil {
					return s.catchSlow(th, ev, false, serr)
				}
				if next != nil {
					if err := s.checkShapeSlotMem(t, next, key, val); err != nil {
						return s.catchSlow(th, ev, false, err)
					}
					if !t.appendSlotFor(s, val) {
						return s.catchSlow(th, ev, false, s.ConsumeMemErr())
					}
					t.setShape(s, next)
					s.barrierBackTable(t)
					if t.tmFlags != 0 {
						t.tmFlags = 0
					}
					if t.isProto() {
						s.protoEpoch++
						if key == s.intern.tms[TMIndex] || key == s.intern.tms[TMNewIndex] {
							if val.IsTable() {
								s.asTable(val).markProto(s)
							}
						}
					} else {
						ic.setAddSlotFor(oldSh, next, int32(next.slot), s.protoEpoch, !oldSh.hasMeta())
					}
					return false, nil
				}
				if err := s.demoteTableToDict(t, StringValue(key), val); err != nil {
					return s.catchSlow(th, ev, false, err)
				}
				t.RawSetStr(s, key, val)
				if err := s.ConsumeMemErr(); err != nil {
					return s.catchSlow(th, ev, false, err)
				}
				return false, nil
			}
		} else {
			if err := s.ensureLookupStrKeyHash(t, key); err != nil {
				return s.catchSlow(th, ev, false, err)
			}
			if !t.RawGetStr(s, key).IsNil() || !t.hasMeta() || s.getTM(t.Metatable(s), TMNewIndex).IsNil() {
				t.RawSetStr(s, key, val)
				if err := s.ConsumeMemErr(); err != nil {
					return s.catchSlow(th, ev, false, err)
				}
				return false, nil
			}
		}
	}
	pushed, err := s.finishSetYieldable(th, ev, ra, ev.consts[b], val)
	return s.catchSlow(th, ev, pushed, err)
}

//go:noinline
func (s *State) getISlow(th *Thread, ev *frameEnv, w bytecode.Instruction, regs *[256]Value) (bool, error) {
	a := uint8(w >> bytecode.PosA)
	rb := regs[uint8(w>>bytecode.PosB)]
	c := int64(uint8(w >> bytecode.PosC))
	if rb.IsTable() {
		t := s.asTable(rb)
		v := t.RawGetInt(s, c)
		if !v.IsNil() || !t.hasMeta() {
			regs[a] = v
			return false, nil
		}
	}
	pushed, err := s.finishGetYieldable(th, ev, rb, IntValue(c), a)
	return s.catchSlow(th, ev, pushed, err)
}

//go:noinline
func (s *State) getTableSlow(th *Thread, ev *frameEnv, w bytecode.Instruction, regs *[256]Value) (bool, error) {
	a := uint8(w >> bytecode.PosA)
	rb := regs[uint8(w>>bytecode.PosB)]
	rc := regs[uint8(w>>bytecode.PosC)]
	if rb.IsTable() {
		t := s.asTable(rb)
		if err := s.ensureLookupKeyHash(t, rc); err != nil {
			return s.catchSlow(th, ev, false, err)
		}
		v := t.RawGet(s, rc)
		if !v.IsNil() || !t.hasMeta() {
			regs[a] = v
			return false, nil
		}
	}
	pushed, err := s.finishGetYieldable(th, ev, rb, rc, a)
	return s.catchSlow(th, ev, pushed, err)
}

//go:noinline
func (s *State) setISlow(th *Thread, ev *frameEnv, w bytecode.Instruction, regs *[256]Value) (bool, error) {
	a := uint8(w >> bytecode.PosA)
	b := int64(uint8(w >> bytecode.PosB))
	c := uint8(w >> bytecode.PosC)
	var val Value
	if w.K() {
		val = ev.consts[c]
	} else {
		val = regs[c]
	}
	ra := regs[a]
	if ra.IsTable() {
		t := s.asTable(ra)
		if !t.RawGetInt(s, b).IsNil() || !t.hasMeta() || s.getTM(t.Metatable(s), TMNewIndex).IsNil() {
			t.RawSetInt(s, b, val)
			if err := s.ConsumeMemErr(); err != nil {
				return s.catchSlow(th, ev, false, err)
			}
			return false, nil
		}
	}
	pushed, err := s.finishSetYieldable(th, ev, ra, IntValue(b), val)
	return s.catchSlow(th, ev, pushed, err)
}

//go:noinline
func (s *State) setTableSlow(th *Thread, ev *frameEnv, w bytecode.Instruction, regs *[256]Value) (bool, error) {
	a := uint8(w >> bytecode.PosA)
	rb := regs[uint8(w>>bytecode.PosB)]
	c := uint8(w >> bytecode.PosC)
	var val Value
	if w.K() {
		val = ev.consts[c]
	} else {
		val = regs[c]
	}
	ra := regs[a]
	if ra.IsTable() {
		t := s.asTable(ra)
		if err := s.ensureLookupKeyHash(t, rb); err != nil {
			return s.catchSlow(th, ev, false, err)
		}
		if !t.RawGet(s, rb).IsNil() || !t.hasMeta() || s.getTM(t.Metatable(s), TMNewIndex).IsNil() {
			return s.catchSlow(th, ev, false, t.RawSet(s, rb, val))
		}
	}
	pushed, err := s.finishSetYieldable(th, ev, ra, rb, val)
	return s.catchSlow(th, ev, pushed, err)
}

func (s *State) indexVarInfo(th *Thread, ev *frameEnv) string {
	cf := &th.framesSlice(s.arena)[ev.ci]
	p := ev.bp.proto
	ipc := cf.pc - 1
	if p == nil || ipc < 0 || ipc >= len(p.Code) {
		return ""
	}
	w := p.Code[ipc]
	switch w.Op() {
	case bytecode.OP_GETFIELD, bytecode.OP_GETTABLE, bytecode.OP_GETI, bytecode.OP_SELF:
		return varInfo(p, ipc, int(w.B()))
	case bytecode.OP_SETFIELD, bytecode.OP_SETTABLE, bytecode.OP_SETI:
		return varInfo(p, ipc, int(w.A()))
	case bytecode.OP_GETTABUP:
		uv := int(w.B())
		if uv < len(p.Upvalues) && p.Upvalues[uv].Name != "" {
			return fmt.Sprintf(" (upvalue '%s')", p.Upvalues[uv].Name)
		}
	case bytecode.OP_SETTABUP:
		uv := int(w.A())
		if uv < len(p.Upvalues) && p.Upvalues[uv].Name != "" {
			return fmt.Sprintf(" (upvalue '%s')", p.Upvalues[uv].Name)
		}
	}
	return ""
}

func (s *State) finishGetYieldable(th *Thread, ev *frameEnv, obj, key Value, dstReg uint8) (bool, error) {
	cur := obj
	for loop := 0; loop < 2000; loop++ {
		var tm Value
		if cur.IsTable() {
			t := s.asTable(cur)
			if loop > 0 {
				if err := s.ensureLookupKeyHash(t, key); err != nil {
					return false, err
				}
				v := t.RawGet(s, key)
				if !v.IsNil() {
					th.stackSlice(s.arena)[ev.base+int(dstReg)] = v
					return false, nil
				}
			}
			tm = s.getTM(t.Metatable(s), TMIndex)
			if tm.IsNil() {
				th.stackSlice(s.arena)[ev.base+int(dstReg)] = NilValue()
				return false, nil
			}
		} else {
			tm = s.getTMByObj(cur, TMIndex)
			if tm.IsNil() {
				info := ""
				if loop == 0 {
					info = s.indexVarInfo(th, ev)
				}
				return false, fmt.Errorf("attempt to index a %s value%s", cur.TypeName(), info)
			}
		}
		if tm.IsFunction() {
			return s.pushMetamethodFrame(th, ev, tm, []Value{cur, key}, finishSetReg, dstReg, false)
		}
		cur = tm
	}
	return false, fmt.Errorf("'__index' chain too long; possible loop")
}

func (s *State) finishSetYieldable(th *Thread, ev *frameEnv, obj, key, val Value) (bool, error) {
	cur := obj
	for loop := 0; loop < 2000; loop++ {
		var tm Value
		if cur.IsTable() {
			t := s.asTable(cur)
			if loop > 0 {
				if err := s.ensureLookupKeyHash(t, key); err != nil {
					return false, err
				}
				if !t.RawGet(s, key).IsNil() {
					return false, t.RawSet(s, key, val)
				}
			}
			tm = s.getTM(t.Metatable(s), TMNewIndex)
			if tm.IsNil() {
				return false, t.RawSet(s, key, val)
			}
		} else {
			tm = s.getTMByObj(cur, TMNewIndex)
			if tm.IsNil() {
				info := ""
				if loop == 0 {
					info = s.indexVarInfo(th, ev)
				}
				return false, fmt.Errorf("attempt to index a %s value%s", s.objTypeName(cur), info)
			}
		}
		if tm.IsFunction() {
			return s.pushMetamethodFrame(th, ev, tm, []Value{cur, key, val}, finishSetDrop, 0, false)
		}
		cur = tm
	}
	return false, fmt.Errorf("'__newindex' chain too long; possible loop")
}

func (s *State) finishGet(obj, key Value) (Value, error) {
	cur := obj
	for loop := 0; loop < 2000; loop++ {
		var tm Value
		if cur.IsTable() {
			t := s.asTable(cur)
			if loop > 0 {
				if err := s.ensureLookupKeyHash(t, key); err != nil {
					return NilValue(), err
				}
				v := t.RawGet(s, key)
				if !v.IsNil() {
					return v, nil
				}
			}
			tm = s.getTM(t.Metatable(s), TMIndex)
			if tm.IsNil() {
				return NilValue(), nil
			}
		} else {
			tm = s.getTMByObj(cur, TMIndex)
			if tm.IsNil() {
				return NilValue(), fmt.Errorf("attempt to index a %s value", s.objTypeName(cur))
			}
		}
		if !tm.IsFunction() {
			cur = tm
			continue
		}
		res, err := s.Call(tm, cur, key)
		if err != nil {
			return NilValue(), err
		}
		if len(res) == 0 {
			return NilValue(), nil
		}
		return res[0], nil
	}
	return NilValue(), fmt.Errorf("'__index' chain too long; possible loop")
}

//go:noinline
func (s *State) callSlow(th *Thread, ev *frameEnv, w bytecode.Instruction, a, nargs, nresults int) (bool, error) {
	th.highStack = th.stackSize()
	th.highFrames = th.framesLen()
	funcIdx := ev.base + a
	fn := th.stackSlice(s.arena)[funcIdx]

	if fn.IsClosure() {
		cl := s.asClosure(fn)
		if err := th.pushLuaFrame(s, cl, funcIdx, nargs, nresults); err != nil {
			return s.catchSlow(th, ev, false, err)
		}
		if herr := s.fireLuaCallHook(th, cl, nargs); herr != nil {
			return s.catchSlow(th, ev, false, herr)
		}
		return true, nil
	}
	if fn.IsNative() {
		nf := s.asNative(fn)
		if nf.Intrinsic != intrNone {
			st := th.stackSlice(s.arena)
			// Yieldable flat-stack pcall / xpcall (including nested pcall(pcall, ...) / xpcall(pcall, ...))!
			if nf.Intrinsic == intrPCall && nargs >= 1 && isFlatProtectedTarget(st[funcIdx+1]) {
				p, e := s.pushProtectedCall(th, funcIdx, nargs, nresults, false, ev.entryDepth)
				return s.catchSlow(th, ev, p, e)
			}
			if nf.Intrinsic == intrXPCall && nargs >= 2 && isFlatProtectedTarget(st[funcIdx+1]) && st[funcIdx+2].IsFunction() {
				p, e := s.pushProtectedCall(th, funcIdx, nargs, nresults, true, ev.entryDepth)
				return s.catchSlow(th, ev, p, e)
			}
			if nf.Intrinsic == intrDoFile {
				th.top = funcIdx + 1 + nargs
				loaded, lerr := nativeLoadFile(s, dofileArgs(st[funcIdx+1:funcIdx+1+nargs]))
				if lerr != nil {
					return s.catchSlow(th, ev, false, lerr)
				}
				if len(loaded) == 0 || loaded[0].IsNil() {
					if len(loaded) > 1 {
						return s.catchSlow(th, ev, false, &LuaError{Val: loaded[1]})
					}
					return s.catchSlow(th, ev, false, fmt.Errorf("dofile failed"))
				}
				th.stackSlice(s.arena)[funcIdx] = loaded[0]
				cl := s.asClosure(loaded[0])
				if err := th.pushLuaFrame(s, cl, funcIdx, 0, nresults); err != nil {
					return s.catchSlow(th, ev, false, err)
				}
				if herr := s.fireLuaCallHook(th, cl, 0); herr != nil {
					return s.catchSlow(th, ev, false, herr)
				}
				return true, nil
			}
			if nf.Intrinsic == intrPairs && nargs >= 1 {
				tm, cargs, extraDepth, hasTM, terr := s.resolvePairsTM(st[funcIdx+1])
				if terr != nil {
					return s.catchSlow(th, ev, false, terr)
				}
				if hasTM {
					if err := s.ensureStackWithRoots(th, funcIdx+1+len(cargs)+8, tm, cargs); err != nil {
						return s.catchSlow(th, ev, false, err)
					}
					st = th.stackSlice(s.arena)
					st[funcIdx] = tm
					copy(st[funcIdx+1:funcIdx+1+len(cargs)], cargs)
					th.top = funcIdx + 1 + len(cargs)
					callNRes := nresults
					needFinish := nresults < 0 || nresults > 4
					if needFinish {
						callNRes = 4
					}
					if tm.IsClosure() {
						cl := s.asClosure(tm)
						if needFinish {
							caller := &th.framesSlice(s.arena)[ev.ci]
							caller.dirty = true
							caller.finish = finishPairs
							caller.oldTop = funcIdx
							caller.closeRetN = nresults
						}
						if err := th.pushLuaFrame(s, cl, funcIdx, len(cargs), callNRes); err != nil {
							if needFinish {
								th.framesSlice(s.arena)[ev.ci].finish = finishNone
							}
							return s.catchSlow(th, ev, false, err)
						}
						th.framesSlice(s.arena)[th.nframe-1].extraArgs = extraDepth
						if herr := s.fireLuaCallHook(th, cl, len(cargs)); herr != nil {
							return s.catchSlow(th, ev, false, herr)
						}
						return true, nil
					}
					res, _, err := s.callNativeHooked(th, tm, funcIdx, len(cargs), extraDepth, maxFrames+128)
					if errors.Is(err, errYieldSentinel) {
						if needFinish {
							caller := &th.framesSlice(s.arena)[ev.ci]
							caller.dirty = true
							caller.finish = finishPairs
							caller.oldTop = funcIdx
							caller.closeRetN = nresults
						}
						th.yieldDst = funcIdx
						th.yieldNRes = callNRes
						th.yieldedNative = true
						th.yieldNativeFn = tm
						return false, errYieldSentinel
					}
					if err != nil {
						if !errors.Is(err, errCoroCloseSelf) {
							th.lastErrNative = tm
						}
						return s.catchSlow(th, ev, false, err)
					}
					out := []Value{NilValue(), NilValue(), NilValue(), NilValue()}
					copy(out, res)
					if err := th.placeResults(s, funcIdx, out, nresults); err != nil {
						return s.catchSlow(th, ev, false, err)
					}
					liveTop := th.top
					if nresults >= 0 {
						liveTop = funcIdx + nresults
						if end := ev.base + int(ev.bp.proto.MaxStackSize); th.top < end {
							th.top = end
						}
					}
					if s.gcNeedsCheck() {
						s.gcCheckStep(liveTop)
						if s.abortErr != nil {
							return false, s.abortErr
						}
					}
					return false, nil
				}
			}
		}
		th.top = funcIdx + 1 + nargs
		hookedNative := th.hookMask != 0 && th.hookMask&(HookMaskCall|HookMaskRet) != 0 && !th.inHook && !th.suppressHook
		pushedFrame := hookedNative || (nf.FastFn == nil && nf.Intrinsic == intrNone)
		if pushedFrame {
			if err := th.ensureFrameCap(s, th.nframe+1, maxFrames+128); err != nil {
				return s.catchSlow(th, ev, false, err)
			}
			th.framesSlice(s.arena)[th.nframe] = callFrame{
				kind:      frameNative,
				base:      funcIdx + 1,
				funcIdx:   funcIdx,
				nativeFn:  fn,
				ftransfer: 1,
				ntransfer: uint16(nargs),
				dirty:     true,
			}
			th.nframe++
			if hookedNative && th.hookMask&HookMaskCall != 0 {
				if herr := s.fireHook(th, "call", NilValue()); herr != nil {
					th.nframe--
					return s.catchSlow(th, ev, false, herr)
				}
			}
		}
		// Slice arguments after any call hook in case the hook grew stackRef or modified locals via debug.setlocal.
		args := th.stackSlice(s.arena)[funcIdx+1 : funcIdx+1+nargs]
		if nf.FastFn != nil && !pushedFrame {
			th.nativeArgsDepth++
			rv, err := nf.FastFn(s, args)
			th.finishNativeArgs(s, nil)
			if err == nil && (th.hookMask == 0 || th.hookMask&HookMaskRet == 0 || th.inHook || th.suppressHook) {
				liveTop := funcIdx + 1
				switch nresults {
				case 1:
					th.stackSlice(s.arena)[funcIdx] = rv
					if end := ev.base + int(ev.bp.maxStack); th.top < end {
						th.top = end
					}
				case 0:
					liveTop = funcIdx
					if end := ev.base + int(ev.bp.maxStack); th.top < end {
						th.top = end
					}
				case -1:
					th.stackSlice(s.arena)[funcIdx] = rv
					th.top = funcIdx + 1
				default:
					if err := s.ensureStackWithRoots(th, funcIdx+nresults+4, rv, nil); err != nil {
						return s.catchSlow(th, ev, false, err)
					}
					st := th.stackSlice(s.arena)
					st[funcIdx] = rv
					fillNil(st[funcIdx+1 : funcIdx+nresults])
					th.top = funcIdx + nresults
					liveTop = funcIdx + nresults
					if end := ev.base + int(ev.bp.maxStack); th.top < end {
						th.top = end
					}
				}
				if s.gcNeedsCheck() {
					s.gcCheckStep(liveTop)
					if s.abortErr != nil {
						return false, s.abortErr
					}
				}
				return false, nil
			}
			if err != nil {
				if err == errYieldSentinel || errors.Is(err, errYieldSentinel) {
					th.yieldDst = funcIdx
					th.yieldNRes = nresults
					th.yieldedNative = true
					th.yieldNativeFn = fn
					return false, errYieldSentinel
				}
				if !errors.Is(err, errCoroCloseSelf) {
					th.lastErrNative = fn
				}
				// Temporarily push frameNative while handling error so xpcall tracebacks
				// include the failing FastFn C frame (e.g. [C]: in field 'floor').
				var pushedTemp bool
				var errRoot Value
				if le, ok := err.(*LuaError); ok {
					errRoot = le.Val
				}
				if ferr := s.ensureFrameCapWithRoots(th, th.nframe+1, maxFrames+128, errRoot, nil); ferr == nil {
					th.framesSlice(s.arena)[th.nframe] = callFrame{
						kind:      frameNative,
						base:      funcIdx + 1,
						funcIdx:   funcIdx,
						nativeFn:  fn,
						ftransfer: 1,
						ntransfer: uint16(nargs),
						dirty:     true,
					}
					th.nframe++
					pushedTemp = true
				}
				handled, cerr := s.catchSlow(th, ev, false, err)
				if !handled && pushedTemp {
					th.nframe--
				}
				return handled, cerr
			}
			// Hook was enabled during FastFn call: fire return hook and place result.
			if err := s.ensureFrameCapWithRoots(th, th.nframe+1, maxFrames+128, rv, nil); err != nil {
				return s.catchSlow(th, ev, false, err)
			}
			resSlot := funcIdx + 1 + nargs
			if err := s.ensureStackWithRoots(th, resSlot+1+4, rv, nil); err != nil {
				return s.catchSlow(th, ev, false, err)
			}
			th.stackSlice(s.arena)[resSlot] = rv
			if end := resSlot + 1; th.top < end {
				th.top = end
			}
			th.framesSlice(s.arena)[th.nframe] = callFrame{
				kind:      frameNative,
				base:      funcIdx + 1,
				funcIdx:   funcIdx,
				nativeFn:  fn,
				ftransfer: uint16(nargs + 1),
				ntransfer: 1,
				dirty:     true,
			}
			th.nframe++
			herr := s.fireHook(th, "return", NilValue())
			th.nframe--
			if herr != nil {
				return s.catchSlow(th, ev, false, herr)
			}
			rv = th.stackSlice(s.arena)[resSlot]
			if err := th.placeResults(s, funcIdx, []Value{rv}, nresults); err != nil {
				return s.catchSlow(th, ev, false, err)
			}
			liveTop := th.top
			if nresults >= 0 {
				liveTop = funcIdx + nresults
				if end := ev.base + int(ev.bp.maxStack); th.top < end {
					th.top = end
				}
			}
			if s.gcNeedsCheck() {
				s.gcCheckStep(liveTop)
				if s.abortErr != nil {
					return false, s.abortErr
				}
			}
			return false, nil
		}
		th.nativeArgsDepth++
		res, err := nf.callMulti(s, args)
		res = th.finishNativeArgs(s, res)
		if err != nil {
			if err == errYieldSentinel || errors.Is(err, errYieldSentinel) {
				if pushedFrame {
					th.nframe--
				}
				th.yieldDst = funcIdx
				th.yieldNRes = nresults
				th.yieldedNative = true
				th.yieldNativeFn = fn
				return false, errYieldSentinel
			}
			if !errors.Is(err, errCoroCloseSelf) {
				th.lastErrNative = fn
			}
			// Keep pushedFrame (frameNative) on th.frames during catchSlow so xpcall
			// message handlers (debug.traceback / debug.getinfo) see the C frame!
			handled, cerr := s.catchSlow(th, ev, false, err)
			if !handled && pushedFrame {
				th.nframe--
			}
			return handled, cerr
		}
		if pushedFrame || (th.hookMask != 0 && th.hookMask&HookMaskRet != 0 && !th.inHook && !th.suppressHook) {
			var hookErr error
			if pushedFrame {
				if th.hookMask&HookMaskRet != 0 && !th.inHook && !th.suppressHook {
					// Place returned results above args (ftransfer = nargs+1) matching ldo.c:rethook!
					resStart := funcIdx + 1 + nargs
					if serr := s.ensureStackWithRoots(th, resStart+len(res)+4, NilValue(), res); serr != nil {
						hookErr = serr
					} else {
						copy(th.stackSlice(s.arena)[resStart:resStart+len(res)], res)
						if end := resStart + len(res); th.top < end {
							th.top = end
						}
						frames := th.framesSlice(s.arena)
						frames[th.nframe-1].ftransfer = uint16(nargs + 1)
						frames[th.nframe-1].ntransfer = uint16(len(res))
						hookErr = s.fireHook(th, "return", NilValue())
						if hookErr == nil && len(res) > 0 {
							out := make([]Value, len(res))
							copy(out, th.stackSlice(s.arena)[resStart:resStart+len(res)])
							res = out
						}
					}
				}
				th.nframe--
			} else {
				// Hook was enabled during this native call (e.g. debug.sethook(hook, "r") in locals.lua:842)!
				resStart := funcIdx + 1 + nargs
				if ferr := s.ensureFrameCapWithRoots(th, th.nframe+1, maxFrames+128, NilValue(), res); ferr != nil {
					hookErr = ferr
				} else if serr := s.ensureStackWithRoots(th, resStart+len(res)+4, NilValue(), res); serr != nil {
					hookErr = serr
				} else {
					copy(th.stackSlice(s.arena)[resStart:resStart+len(res)], res)
					if end := resStart + len(res); th.top < end {
						th.top = end
					}
					th.framesSlice(s.arena)[th.nframe] = callFrame{
						kind:      frameNative,
						base:      funcIdx + 1,
						funcIdx:   funcIdx,
						nativeFn:  fn,
						ftransfer: uint16(nargs + 1),
						ntransfer: uint16(len(res)),
						dirty:     true,
					}
					th.nframe++
					hookErr = s.fireHook(th, "return", NilValue())
					if hookErr == nil && len(res) > 0 {
						out := make([]Value, len(res))
						copy(out, th.stackSlice(s.arena)[resStart:resStart+len(res)])
						res = out
					}
					th.nframe--
				}
			}
			if hookErr != nil {
				return s.catchSlow(th, ev, false, hookErr)
			}
		}
		if nresults == 1 && len(res) >= 1 {
			th.stackSlice(s.arena)[funcIdx] = res[0]
			if end := ev.base + int(ev.bp.proto.MaxStackSize); th.top < end {
				th.top = end
			}
			if s.gcNeedsCheck() {
				s.gcCheckStep(funcIdx + 1)
				if s.abortErr != nil {
					return false, s.abortErr
				}
			}
			return false, nil
		}
		if err := th.placeResults(s, funcIdx, res, nresults); err != nil {
			return s.catchSlow(th, ev, false, err)
		}
		liveTop := th.top
		if nresults >= 0 {
			liveTop = funcIdx + nresults
			if end := ev.base + int(ev.bp.proto.MaxStackSize); th.top < end {
				th.top = end
			}
		}
		if s.gcNeedsCheck() {
			s.gcCheckStep(liveTop)
			if s.abortErr != nil {
				return false, s.abortErr
			}
		}
		return false, nil
	}
	// Try __call chain (max 15 per Lua 5.5).
	for depth := 1; depth <= 15; depth++ {
		tm := s.getTMByObj(fn, TMCall)
		if tm.IsNil() {
			return s.catchSlow(th, ev, false, fmt.Errorf("attempt to call a %s value%s", fn.TypeName(), varInfo(ev.bp.proto, th.framesSlice(s.arena)[ev.ci].pc-1, a)))
		}
		if err := s.ensureStackWithRoots(th, funcIdx+nargs+4, tm, nil); err != nil {
			return s.catchSlow(th, ev, false, err)
		}
		st := th.stackSlice(s.arena)
		copy(st[funcIdx+2:funcIdx+2+nargs], st[funcIdx+1:funcIdx+1+nargs])
		st[funcIdx+1] = fn
		st[funcIdx] = tm
		nargs++
		if end := funcIdx + 1 + nargs; th.top < end {
			th.top = end
		}
		fn = tm
		if fn.IsClosure() {
			cl := s.asClosure(fn)
			if err := th.pushLuaFrame(s, cl, funcIdx, nargs, nresults); err != nil {
				return s.catchSlow(th, ev, false, err)
			}
			th.framesSlice(s.arena)[th.nframe-1].extraArgs = uint8(depth)
			if herr := s.fireLuaCallHook(th, cl, nargs); herr != nil {
				return s.catchSlow(th, ev, false, herr)
			}
			return true, nil
		}
		if fn.IsNative() {
			res, _, err := s.callNativeHooked(th, fn, funcIdx, nargs, uint8(depth), maxFrames+128)
			if err != nil {
				if errors.Is(err, errYieldSentinel) {
					th.yieldDst = funcIdx
					th.yieldNRes = nresults
					th.yieldedNative = true
					th.yieldNativeFn = fn
					return false, errYieldSentinel
				}
				if !errors.Is(err, errCoroCloseSelf) {
					th.lastErrNative = fn
				}
				return s.catchSlow(th, ev, false, err)
			}
			if err := th.placeResults(s, funcIdx, res, nresults); err != nil {
				return s.catchSlow(th, ev, false, err)
			}
			liveTop := th.top
			if nresults >= 0 {
				liveTop = funcIdx + nresults
				if end := ev.base + int(ev.bp.proto.MaxStackSize); th.top < end {
					th.top = end
				}
			}
			if s.gcNeedsCheck() {
				s.gcCheckStep(liveTop)
				if s.abortErr != nil {
					return false, s.abortErr
				}
			}
			return false, nil
		}
	}
	return s.catchSlow(th, ev, false, fmt.Errorf("'__call' chain too long"))
}

// callNativeHooked invokes native function fn located at stack[funcIdx] with arguments
// at stack[funcIdx+1 : funcIdx+1+nargs], firing Lua call/return debug hooks around the
// call and returning (results, hookedNative, err).
func (s *State) callNativeHooked(th *Thread, fn Value, funcIdx, nargs int, extraDepth uint8, frameLimit int) ([]Value, bool, error) {
	th.top = funcIdx + 1 + nargs
	nf := s.asNative(fn)
	hookedNative := th.hookMask != 0 && th.hookMask&(HookMaskCall|HookMaskRet) != 0 && !th.inHook && !th.suppressHook
	pushedFrame := hookedNative || (nf.FastFn == nil && nf.Intrinsic == intrNone)
	if pushedFrame {
		if err := th.ensureFrameCap(s, th.nframe+1, frameLimit); err != nil {
			return nil, false, err
		}
		th.framesSlice(s.arena)[th.nframe] = callFrame{
			kind:      frameNative,
			base:      funcIdx + 1,
			funcIdx:   funcIdx,
			nativeFn:  fn,
			ftransfer: 1,
			ntransfer: uint16(nargs),
			extraArgs: extraDepth,
			dirty:     true,
		}
		th.nframe++
		if hookedNative && th.hookMask&HookMaskCall != 0 {
			if herr := s.fireHook(th, "call", NilValue()); herr != nil {
				th.nframe--
				return nil, false, herr
			}
		}
	}
	// Re-slice arguments after any call hook in case the hook grew stackRef or modified locals via debug.setlocal.
	args := th.stackSlice(s.arena)[funcIdx+1 : funcIdx+1+nargs]
	th.nativeArgsDepth++
	res, err := nf.callMulti(s, args)
	res = th.finishNativeArgs(s, res)
	if pushedFrame || (th.hookMask != 0 && err == nil && th.hookMask&HookMaskRet != 0 && !th.inHook && !th.suppressHook) {
		var hookErr error
		if pushedFrame {
			if err == nil && th.hookMask&HookMaskRet != 0 && !th.inHook && !th.suppressHook {
				resStart := funcIdx + 1 + nargs
				if serr := s.ensureStackWithRoots(th, resStart+len(res)+4, NilValue(), res); serr != nil {
					hookErr = serr
				} else {
					copy(th.stackSlice(s.arena)[resStart:resStart+len(res)], res)
					if end := resStart + len(res); th.top < end {
						th.top = end
					}
					frames := th.framesSlice(s.arena)
					frames[th.nframe-1].ftransfer = uint16(nargs + 1)
					frames[th.nframe-1].ntransfer = uint16(len(res))
					hookErr = s.fireHook(th, "return", NilValue())
					if hookErr == nil && len(res) > 0 {
						out := make([]Value, len(res))
						copy(out, th.stackSlice(s.arena)[resStart:resStart+len(res)])
						res = out
					}
				}
			}
			th.nframe--
		} else {
			resStart := funcIdx + 1 + nargs
			if ferr := s.ensureFrameCapWithRoots(th, th.nframe+1, frameLimit, NilValue(), res); ferr != nil {
				hookErr = ferr
			} else if serr := s.ensureStackWithRoots(th, resStart+len(res)+4, NilValue(), res); serr != nil {
				hookErr = serr
			} else {
				copy(th.stackSlice(s.arena)[resStart:resStart+len(res)], res)
				if end := resStart + len(res); th.top < end {
					th.top = end
				}
				th.framesSlice(s.arena)[th.nframe] = callFrame{
					kind:      frameNative,
					base:      funcIdx + 1,
					funcIdx:   funcIdx,
					nativeFn:  fn,
					ftransfer: uint16(nargs + 1),
					ntransfer: uint16(len(res)),
					extraArgs: extraDepth,
					dirty:     true,
				}
				th.nframe++
				hookErr = s.fireHook(th, "return", NilValue())
				if hookErr == nil && len(res) > 0 {
					out := make([]Value, len(res))
					copy(out, th.stackSlice(s.arena)[resStart:resStart+len(res)])
					res = out
				}
				th.nframe--
			}
		}
		if hookErr != nil && err == nil {
			err = hookErr
		}
	}
	return res, hookedNative, err
}

// resolvePairsTM resolves obj's __pairs metamethod (including any __call chain up to 15 hops).
// Returns hasTM == false when obj has no __pairs metamethod.
func (s *State) resolvePairsTM(obj Value) (fn Value, args []Value, extraDepth uint8, hasTM bool, err error) {
	tm := s.getTMByObj(obj, TMPairs)
	if tm.IsNil() {
		return NilValue(), nil, 0, false, nil
	}
	cargs := []Value{obj}
	cur := tm
	if !cur.IsClosure() && !cur.IsNative() {
		for depth := 1; depth <= 15; depth++ {
			callTM := s.getTMByObj(cur, TMCall)
			if callTM.IsNil() {
				return NilValue(), nil, 0, true, fmt.Errorf("attempt to call a %s value (metamethod 'pairs')", s.objTypeName(cur))
			}
			next := make([]Value, 1+len(cargs))
			next[0] = cur
			copy(next[1:], cargs)
			cargs = next
			cur = callTM
			extraDepth = uint8(depth)
			if cur.IsClosure() || cur.IsNative() {
				break
			}
		}
		if !cur.IsClosure() && !cur.IsNative() {
			return NilValue(), nil, 0, true, fmt.Errorf("'__call' chain too long")
		}
	}
	return cur, cargs, extraDepth, true, nil
}

func isFlatProtectedTarget(_ Value) bool {
	return true
}

// pushProtectedCall pushes a framePCall/frameXPCall boundary frame + target Lua closure,
// callable table (__call chain), or native (including nested pcall/xpcall and coroutine.yield!)
// directly onto th.frames so protected calls remain yieldable without a Go C-call boundary.
func (s *State) pushProtectedCall(th *Thread, funcIdx, nargs, nresults int, isXP bool, entryDepth int) (bool, error) {
	return s.pushProtectedCallMode(th, funcIdx, nargs, nresults, isXP, entryDepth, false)
}

func (s *State) pushProtectedCallMode(th *Thread, funcIdx, nargs, nresults int, isXP bool, entryDepth int, topProtected bool) (bool, error) {
	// Enforce Lua 5.5 LUAI_MAXCCALLS (200) across nested pcall/xpcall frames (calls.lua:162).
	pcalls := 0
	frames := th.framesSlice(s.arena)
	for i := 0; i < th.nframe; i++ {
		if frames[i].kind == framePCall || frames[i].kind == frameXPCall {
			pcalls++
		}
	}
	cLimit := maxCCalls
	if th.inErrHandler {
		cLimit = maxCCallsErrHandler
	}
	if s.nCcalls+pcalls >= cLimit {
		return false, fmt.Errorf("C stack overflow")
	}
	frameLimit := maxFrames
	if th.inErrHandler {
		frameLimit = maxFrames + 128
	}
	if err := th.ensureFrameCap(s, th.nframe+2, frameLimit); err != nil {
		return false, err
	}
	st := th.stackSlice(s.arena)
	targetVal := st[funcIdx+1]
	var errFunc Value
	var calleeIdx, calleeArgs int
	kind := framePCall
	if isXP {
		kind = frameXPCall
		errFunc = st[funcIdx+2]
		// Shift target to funcIdx+2 (overwriting msgh which is saved in frame.errFunc)
		// so args start at funcIdx+3!
		st[funcIdx+2] = st[funcIdx+1]
		calleeIdx = funcIdx + 2
		calleeArgs = nargs - 2
	} else {
		calleeIdx = funcIdx + 1
		calleeArgs = nargs - 1
	}
	th.framesSlice(s.arena)[th.nframe] = callFrame{
		kind:         kind,
		base:         calleeIdx,
		funcIdx:      funcIdx,
		nresults:     nresults,
		errFunc:      errFunc,
		nativeFn:     st[funcIdx],
		ftransfer:    1,
		ntransfer:    uint16(nargs),
		topProtected: topProtected,
		dirty:        true,
	}
	th.nframe++
	if th.hookMask&HookMaskCall != 0 && !th.inHook && !th.suppressHook {
		if herr := s.fireHook(th, "call", NilValue()); herr != nil {
			th.nframe--
			return false, herr
		}
	}
	outerDepth := th.nframe
	uncaught := func(err error) error {
		if uerr := th.uncaughtErr(s); uerr != nil {
			err = uerr
			th.setUncaughtErr(s, nil)
		}
		if s.abortErr != nil {
			return s.abortErr
		}
		return err
	}
	var extraDepth uint8
	if !targetVal.IsClosure() && !targetVal.IsNative() {
		cur := targetVal
		var prepended []Value
		for depth := 1; depth <= 15; depth++ {
			tm := s.getTMByObj(cur, TMCall)
			if tm.IsNil() {
				err := fmt.Errorf("attempt to call a %s value", cur.TypeName())
				if s.handleError(th, entryDepth, err) {
					return true, nil
				}
				return false, uncaught(err)
			}
			prepended = append(prepended, cur)
			cur = tm
			extraDepth = uint8(depth)
			if cur.IsClosure() || cur.IsNative() {
				break
			}
		}
		if !cur.IsClosure() && !cur.IsNative() {
			err := fmt.Errorf("'__call' chain too long")
			if s.handleError(th, entryDepth, err) {
				return true, nil
			}
			return false, uncaught(err)
		}
		k := len(prepended)
		if err := s.ensureStackWithRoots(th, calleeIdx+1+k+calleeArgs+8, cur, prepended); err != nil {
			if s.handleError(th, entryDepth, err) {
				return true, nil
			}
			return false, uncaught(err)
		}
		st = th.stackSlice(s.arena)
		copy(st[calleeIdx+1+k:calleeIdx+1+k+calleeArgs], st[calleeIdx+1:calleeIdx+1+calleeArgs])
		for i := range k {
			st[calleeIdx+1+i] = prepended[k-1-i]
		}
		st[calleeIdx] = cur
		targetVal = cur
		calleeArgs += k
		if end := calleeIdx + 1 + calleeArgs; th.top < end {
			th.top = end
		}
	}
	if targetVal.IsNative() {
		nf := s.asNative(targetVal)
		intr := nf.Intrinsic
		st = th.stackSlice(s.arena)
		if (intr == intrPCall && calleeArgs >= 1 && isFlatProtectedTarget(st[calleeIdx+1])) ||
			(intr == intrXPCall && calleeArgs >= 2 && isFlatProtectedTarget(st[calleeIdx+1]) && st[calleeIdx+2].IsFunction()) {
			ok, err := s.pushProtectedCall(th, calleeIdx, calleeArgs, -1, intr == intrXPCall, entryDepth)
			if err != nil {
				if errors.Is(err, errYieldSentinel) {
					return false, errYieldSentinel
				}
				if th.nframe == outerDepth {
					if s.handleError(th, entryDepth, err) {
						return th.nframe != outerDepth-1, nil
					}
				}
				return false, uncaught(err)
			}
			// If the nested protected call completed synchronously (either via a normal native
			// return or by catching an error at its own inner frame), our outer framePCall/frameXPCall
			// is back on top of th.frames with the inner results at calleeIdx..th.top. Complete it now!
			if th.nframe == outerDepth && th.framesSlice(s.arena)[outerDepth-1].kind != frameLua {
				if _, ferr := s.finishPCallReturn(th, entryDepth); ferr != nil {
					if s.handleError(th, entryDepth, ferr) {
						return true, nil
					}
					return false, uncaught(ferr)
				}
				return false, nil
			}
			return ok, nil
		}
		if intr == intrDoFile {
			th.top = calleeIdx + 1 + calleeArgs
			loaded, lerr := nativeLoadFile(s, dofileArgs(st[calleeIdx+1:calleeIdx+1+calleeArgs]))
			if lerr != nil {
				if s.handleError(th, entryDepth, lerr) {
					return true, nil
				}
				return false, uncaught(lerr)
			}
			if len(loaded) == 0 || loaded[0].IsNil() {
				var derr error
				if len(loaded) > 1 {
					derr = &LuaError{Val: loaded[1]}
				} else {
					derr = fmt.Errorf("dofile failed")
				}
				if s.handleError(th, entryDepth, derr) {
					return true, nil
				}
				return false, uncaught(derr)
			}
			th.stackSlice(s.arena)[calleeIdx] = loaded[0]
			cl := s.asClosure(loaded[0])
			if err := th.pushLuaFrame(s, cl, calleeIdx, 0, -1); err != nil {
				th.nframe--
				return false, err
			}
			if herr := s.fireLuaCallHook(th, cl, 0); herr != nil {
				if s.handleError(th, entryDepth, herr) {
					return true, nil
				}
				return false, uncaught(herr)
			}
			return true, nil
		}
		if intr == intrPairs && calleeArgs >= 1 {
			tm, cargs, tmExtra, hasTM, terr := s.resolvePairsTM(st[calleeIdx+1])
			if terr != nil {
				if s.handleError(th, entryDepth, terr) {
					return true, nil
				}
				return false, uncaught(terr)
			}
			if hasTM {
				if err := s.ensureStackWithRoots(th, calleeIdx+1+len(cargs)+8, tm, cargs); err != nil {
					if s.handleError(th, entryDepth, err) {
						return true, nil
					}
					return false, uncaught(err)
				}
				st = th.stackSlice(s.arena)
				st[calleeIdx] = tm
				copy(st[calleeIdx+1:calleeIdx+1+len(cargs)], cargs)
				th.top = calleeIdx + 1 + len(cargs)
				if tm.IsClosure() {
					cl := s.asClosure(tm)
					if err := th.pushLuaFrame(s, cl, calleeIdx, len(cargs), 4); err != nil {
						th.nframe--
						return false, err
					}
					th.framesSlice(s.arena)[th.nframe-1].extraArgs = tmExtra
					if herr := s.fireLuaCallHook(th, cl, len(cargs)); herr != nil {
						if s.handleError(th, entryDepth, herr) {
							return true, nil
						}
						return false, uncaught(herr)
					}
					return true, nil
				}
				res, _, err := s.callNativeHooked(th, tm, calleeIdx, len(cargs), tmExtra, frameLimit)
				if errors.Is(err, errYieldSentinel) {
					th.yieldDst = calleeIdx
					th.yieldNRes = 4
					th.yieldedNative = true
					th.yieldNativeFn = tm
					return false, errYieldSentinel
				}
				if err != nil {
					if !errors.Is(err, errCoroCloseSelf) {
						th.lastErrNative = tm
					}
					if s.handleError(th, entryDepth, err) {
						return true, nil
					}
					return false, uncaught(err)
				}
				out := []Value{NilValue(), NilValue(), NilValue(), NilValue()}
				copy(out, res)
				if perr := th.placeResults(s, calleeIdx, out, 4); perr != nil {
					if s.handleError(th, entryDepth, perr) {
						return true, nil
					}
					return false, uncaught(perr)
				}
				if _, ferr := s.finishPCallReturn(th, entryDepth); ferr != nil {
					if s.handleError(th, entryDepth, ferr) {
						return true, nil
					}
					return false, uncaught(ferr)
				}
				if s.gcNeedsCheck() {
					s.gcCheckStep(th.top)
					if s.abortErr != nil {
						return false, s.abortErr
					}
				}
				return false, nil
			}
		}
		// General native inside pcall/xpcall (e.g. xpcall(coroutine.yield, msgh) in coroutine.lua:568!):
		res, _, err := s.callNativeHooked(th, targetVal, calleeIdx, calleeArgs, extraDepth, frameLimit)
		if errors.Is(err, errYieldSentinel) {
			th.yieldDst = calleeIdx
			th.yieldNRes = -1
			th.yieldedNative = true
			th.yieldNativeFn = targetVal
			return false, errYieldSentinel
		}
		if err != nil {
			if !errors.Is(err, errCoroCloseSelf) {
				th.lastErrNative = targetVal
			}
			// Keep targetVal's C frame visible to xpcall message handlers (e.g. xpcall(error, debug.traceback, "x")).
			var pushedTemp bool
			var errRoot Value
			if le, ok := err.(*LuaError); ok {
				errRoot = le.Val
			}
			if ferr := s.ensureFrameCapWithRoots(th, th.nframe+1, frameLimit, errRoot, nil); ferr == nil {
				th.framesSlice(s.arena)[th.nframe] = callFrame{
					kind:      frameNative,
					base:      calleeIdx + 1,
					funcIdx:   calleeIdx,
					nativeFn:  targetVal,
					ftransfer: 1,
					ntransfer: uint16(calleeArgs),
					extraArgs: extraDepth,
					dirty:     true,
				}
				th.nframe++
				pushedTemp = true
			}
			// Let handleError catch it at the framePCall/frameXPCall we just pushed!
			if s.handleError(th, entryDepth, err) {
				return true, nil
			}
			if pushedTemp {
				th.nframe--
			}
			return false, uncaught(err)
		}
		// Normal return from native inside pcall/xpcall: place results at calleeIdx and finishPCallReturn!
		if perr := th.placeResults(s, calleeIdx, res, -1); perr != nil {
			if s.handleError(th, entryDepth, perr) {
				return true, nil
			}
			return false, uncaught(perr)
		}
		if _, ferr := s.finishPCallReturn(th, entryDepth); ferr != nil {
			if s.handleError(th, entryDepth, ferr) {
				return true, nil
			}
			return false, uncaught(ferr)
		}
		if s.gcNeedsCheck() {
			s.gcCheckStep(th.top)
			if s.abortErr != nil {
				return false, s.abortErr
			}
		}
		return false, nil
	}
	cl := s.asClosure(targetVal)
	if err := th.pushLuaFrame(s, cl, calleeIdx, calleeArgs, -1); err != nil {
		th.nframe--
		return false, err
	}
	th.framesSlice(s.arena)[th.nframe-1].extraArgs = extraDepth
	if herr := s.fireLuaCallHook(th, cl, calleeArgs); herr != nil {
		if s.handleError(th, entryDepth, herr) {
			return true, nil
		}
		return false, uncaught(herr)
	}
	return true, nil
}

//go:noinline
func (s *State) tforCallSlow(th *Thread, ev *frameEnv, a uint8, c int) (bool, error) {
	base := ev.base
	fnIdx := base + int(a) + 3
	st := th.stackSlice(s.arena)
	fn := st[base+int(a)]
	if OptimizeEnabled && (th.hookMask&(HookMaskCall|HookMaskRet) == 0 || th.inHook || th.suppressHook) && fn.IsNative() && s.asNative(fn).Intrinsic == intrNext && st[base+int(a)+1].IsTable() {
		t := s.asTable(st[base+int(a)+1])
		ctrl := st[base+int(a)+3]
		if herr := s.ensureLookupKeyHash(t, ctrl); herr != nil {
			return s.catchSlow(th, ev, false, herr)
		}
		k, v, err := t.Next(s, ctrl)
		if err == nil {
			st[fnIdx] = k
			if c >= 2 {
				st[fnIdx+1] = v
				if c > 2 {
					fillNil(st[fnIdx+2 : fnIdx+c])
				}
			}
			return false, nil
		}
	}
	if err := th.ensureStack(s, fnIdx+max(c, 3)+4); err != nil {
		return s.catchSlow(th, ev, false, err)
	}
	st = th.stackSlice(s.arena)
	st[fnIdx+2] = st[base+int(a)+3] // control var
	st[fnIdx+1] = st[base+int(a)+1] // state
	st[fnIdx] = fn                  // iterator fn
	if end := fnIdx + 3; th.top < end {
		th.top = end
	}
	if fn.IsNative() {
		res, _, err := s.callNativeHooked(th, fn, fnIdx, 2, 0, maxFrames+128)
		if err != nil {
			if errors.Is(err, errYieldSentinel) {
				th.yieldDst = fnIdx
				th.yieldNRes = c
				th.yieldedNative = true
				th.yieldNativeFn = fn
				return false, errYieldSentinel
			}
			if !errors.Is(err, errCoroCloseSelf) {
				th.lastErrNative = fn
			}
			return s.catchSlow(th, ev, false, err)
		}
		if err := th.placeResults(s, fnIdx, res, c); err != nil {
			return s.catchSlow(th, ev, false, err)
		}
		liveTop := max(th.top, fnIdx+c)
		if end := ev.base + int(ev.bp.maxStack); th.top < end {
			th.top = end
		}
		if s.gcNeedsCheck() {
			s.gcCheckStep(liveTop)
			if s.abortErr != nil {
				return false, s.abortErr
			}
		}
		return false, nil
	}
	if fn.IsClosure() {
		cl := s.asClosure(fn)
		if err := th.pushLuaFrame(s, cl, fnIdx, 2, c); err != nil {
			return s.catchSlow(th, ev, false, err)
		}
		if herr := s.fireLuaCallHook(th, cl, 2); herr != nil {
			return s.catchSlow(th, ev, false, herr)
		}
		return true, nil
	}
	return s.callSlow(th, ev, 0, int(a)+3, 2, c)
}

// resumeCloseTBC continues closing remaining <close> TBC vars in a yieldable loop.
func (s *State) resumeCloseTBC(th *Thread, cf *callFrame, entryDepth int) (bool, error) {
	cfIdx := th.nframe - 1
	level := cf.closeLevel
	for {
		idx, ok := th.popTBCAtOrAbove(s.arena, level)
		if !ok {
			break
		}
		obj := th.stackSlice(s.arena)[idx]
		if obj.IsFalsy() {
			continue
		}
		tm := s.getTMByObj(obj, TMClose)
		if tm.IsNil() {
			return false, fmt.Errorf("attempt to call a nil value (metamethod 'close')")
		}
		// Match PUC-Rio Lua 5.5.1 lfunc.c:callclosemethod (and testes/locals.lua:314,325-330):
		// normal scope exit passes 1 argument (obj) so select('#', ...) == 1;
		// error unwinding passes 2 arguments (obj, err) so select('#', ...) == 2.
		var cargs []Value
		if cf.closeHasErr {
			cargs = []Value{obj, cf.closeErr}
		} else {
			cargs = []Value{obj}
		}
		var extraDepth uint8
		if !tm.IsClosure() && !tm.IsNative() {
			curTM := tm
			for depth := 1; depth <= 15; depth++ {
				callTM := s.getTMByObj(curTM, TMCall)
				if callTM.IsNil() {
					if depth == 1 {
						return false, fmt.Errorf("attempt to call a %s value (metamethod 'close')", curTM.TypeName())
					}
					return false, fmt.Errorf("attempt to call a %s value", curTM.TypeName())
				}
				nextArgs := make([]Value, 1+len(cargs))
				nextArgs[0] = curTM
				copy(nextArgs[1:], cargs)
				cargs = nextArgs
				curTM = callTM
				extraDepth = uint8(depth)
				if curTM.IsClosure() || curTM.IsNative() {
					break
				}
			}
			if !curTM.IsClosure() && !curTM.IsNative() {
				return false, fmt.Errorf("'__call' chain too long")
			}
			tm = curTM
		}
		slotBase := cf.base
		if s.frameClosure(cf) != nil {
			slotBase = cf.base + int(s.boundProtoOf(s.frameClosure(cf)).proto.MaxStackSize)
		} else if topIdx, ok := th.topTBCIndex(s.arena); ok {
			// Keep stack slot above any remaining TBC indices in th.tbc!
			if topTBC := topIdx + 2; topTBC > slotBase {
				slotBase = topTBC
			}
		}
		if idx+2 > slotBase {
			slotBase = idx + 2
		}
		if tm.IsClosure() {
			slot := slotBase
			if th.top > slot {
				slot = th.top
			}
			if err := s.ensureStackWithRoots(th, slot+1+len(cargs)+4, tm, cargs); err != nil {
				return false, err
			}
			st := th.stackSlice(s.arena)
			st[slot] = tm
			for i, ca := range cargs {
				st[slot+1+i] = ca
			}
			th.top = slot + 1 + len(cargs)
			cf = &th.framesSlice(s.arena)[cfIdx]
			cf.dirty = true
			cf.oldTop = slot
			cf.finish = finishClose
			cl := s.asClosure(tm)
			if err := th.pushLuaFrame(s, cl, slot, len(cargs), 0); err != nil {
				fillNil(th.stackSlice(s.arena)[slot : slot+1+len(cargs)])
				th.top = slot
				th.framesSlice(s.arena)[cfIdx].finish = finishNone
				return false, err
			}
			th.framesSlice(s.arena)[th.nframe-1].extraArgs = extraDepth
			if herr := s.fireLuaCallHook(th, cl, len(cargs)); herr != nil {
				return false, herr
			}
			return false, nil
		}
		slot := slotBase
		if th.top > slot {
			slot = th.top
		}
		if err := s.ensureStackWithRoots(th, slot+1+len(cargs)+8, tm, cargs); err != nil {
			return false, err
		}
		st := th.stackSlice(s.arena)
		st[slot] = tm
		copy(st[slot+1:slot+1+len(cargs)], cargs)
		_, _, err := s.callNativeHooked(th, tm, slot, len(cargs), extraDepth, maxFrames+128)
		cf = &th.framesSlice(s.arena)[cfIdx]
		if errors.Is(err, errYieldSentinel) {
			cf.dirty = true
			cf.oldTop = slot
			cf.finish = finishClose
			th.yieldDst = slot
			th.yieldNRes = 0
			th.yieldedNative = true
			th.yieldNativeFn = tm
			return false, errYieldSentinel
		}
		th.top = slot
		if err != nil {
			if !errors.Is(err, errCoroCloseSelf) {
				th.lastErrNative = tm
			}
			return false, err
		}
	}
	// All TBC vars >= level are closed!
	cf = &th.framesSlice(s.arena)[cfIdx]
	if cf.closeResume == 2 {
		// Finish protected-call error unwind after yieldable TBC close (locals.lua:1015)!
		if th.hookMask&HookMaskRet != 0 && !th.inHook && !th.suppressHook {
			if herr := s.fireHook(th, "return", NilValue()); herr != nil {
				th.framesSlice(s.arena)[cfIdx].closeResume = 0
				th.nframe--
				return false, herr
			}
			cf = &th.framesSlice(s.arena)[cfIdx]
		}
		funcIdx := cf.funcIdx
		nres := cf.nresults
		errVal := cf.closeErr
		cf.closeResume = 0
		th.nframe--
		if err := th.placeResults(s, funcIdx, []Value{FalseValue(), errVal}, nres); err != nil {
			return false, err
		}
		return false, nil
	}
	if cf.closeResume == 1 {
		// Finish pending OP_RETURN (fire return hook while cf is still on top of stack, locals.lua:852)!
		if th.hookMask&HookMaskRet != 0 && !th.inHook && !th.suppressHook {
			cf.ftransfer = uint16(cf.closeRetSrc - cf.base + 1)
			cf.ntransfer = uint16(cf.closeRetN)
			if herr := s.fireHook(th, "return", NilValue()); herr != nil {
				return false, herr
			}
			cf = &th.framesSlice(s.arena)[cfIdx]
		}
		funcIdx := cf.funcIdx
		nres := cf.nresults
		srcStart := cf.closeRetSrc
		n := cf.closeRetN
		if nres < 0 {
			if err := th.ensureStack(s, funcIdx+n+4); err != nil {
				return false, err
			}
			th.nframe--
			st := th.stackSlice(s.arena)
			copy(st[funcIdx:funcIdx+n], st[srcStart:srcStart+n])
			th.top = funcIdx + n
		} else {
			if err := th.ensureStack(s, funcIdx+nres+4); err != nil {
				return false, err
			}
			th.nframe--
			st := th.stackSlice(s.arena)
			cp := min(n, nres)
			copy(st[funcIdx:funcIdx+cp], st[srcStart:srcStart+cp])
			if cp < nres {
				fillNil(st[funcIdx+cp : funcIdx+nres])
			}
			th.top = funcIdx + nres
		}
		return false, nil
	}
	return true, nil
}

func (s *State) strToNumForLoop(v Value) (Value, error) {
	if !v.IsString() {
		return v, nil
	}
	iv, fv, isInt, ok, err := s.strToNum(s.asString(v).Str())
	if err != nil {
		return NilValue(), err
	}
	if !ok {
		return v, nil
	}
	if isInt {
		return IntValue(iv), nil
	}
	return FloatValue(fv), nil
}

//go:noinline
func (s *State) forPrepSlow(regs *[256]Value, a uint8) (bool, error) {
	origInit, origLim, origStep := regs[a], regs[a+1], regs[a+2]
	if BothInt(origInit, origStep) {
		initV := origInit.Int()
		step := origStep.Int()
		if step == 0 {
			return false, fmt.Errorf("'for' step is zero")
		}
		plim, err := s.strToNumForLoop(origLim)
		if err != nil {
			return false, err
		}
		var lim int64
		var ok bool
		if plim.IsInt() {
			lim, ok = plim.Int(), true
		} else if plim.IsFloat() {
			f := plim.Float()
			if step < 0 {
				lim, ok = FloatToIntCeil(f)
			} else {
				lim, ok = FloatToIntFloor(f)
			}
			if !ok {
				if f > 0 {
					if step < 0 {
						return true, nil
					}
					lim = math.MaxInt64
					ok = true
				} else {
					if step > 0 {
						return true, nil
					}
					lim = math.MinInt64
					ok = true
				}
			}
			if !ok {
				return false, fmt.Errorf("bad 'for' limit (number expected, got %s)", s.objTypeName(plim))
			}
		} else {
			return false, fmt.Errorf("bad 'for' limit (number expected, got %s)", s.objTypeName(plim))
		}
		if (step > 0 && initV > lim) || (step < 0 && initV < lim) {
			return true, nil
		}
		var cnt uint64
		if step > 0 {
			cnt = uint64(lim) - uint64(initV)
			if step != 1 {
				cnt /= uint64(step)
			}
		} else {
			cnt = uint64(initV) - uint64(lim)
			cnt /= uint64(-(step+1)) + 1
		}
		regs[a].SetInt(int64(cnt))
		regs[a+1].SetInt(step)
		regs[a+2].SetInt(initV)
		return false, nil
	}
	plim, err := s.strToNumForLoop(origLim)
	if err != nil {
		return false, err
	}
	pstep, err := s.strToNumForLoop(origStep)
	if err != nil {
		return false, err
	}
	pinit, err := s.strToNumForLoop(origInit)
	if err != nil {
		return false, err
	}
	lim, ok := plim.ToFloat()
	if !ok {
		return false, fmt.Errorf("bad 'for' limit (number expected, got %s)", s.objTypeName(plim))
	}
	step, ok := pstep.ToFloat()
	if !ok {
		return false, fmt.Errorf("bad 'for' step (number expected, got %s)", s.objTypeName(pstep))
	}
	initV, ok := pinit.ToFloat()
	if !ok {
		return false, fmt.Errorf("bad 'for' initial value (number expected, got %s)", s.objTypeName(pinit))
	}
	if step == 0 {
		return false, fmt.Errorf("'for' step is zero")
	}
	// Match Lua 5.5.1 lvm.c:forprep: luai_numlt(0, step) ? luai_numlt(limit, init) : luai_numlt(init, limit)
	// When step is NaN, step > 0 is false so the descending check (initV < lim) applies.
	var skip bool
	if step > 0 {
		skip = lim < initV
	} else {
		skip = initV < lim
	}
	if skip {
		return true, nil
	}
	regs[a].SetFloat(lim)
	regs[a+1].SetFloat(step)
	regs[a+2].SetFloat(initV)
	return false, nil
}

//go:noinline
func (s *State) coldOp(th *Thread, ev *frameEnv, ipc int, w bytecode.Instruction, regs *[256]Value, entryDepth int) (coldAction, int, error) {
	th.highStack = th.stackSize()
	th.highFrames = th.framesLen()
	act, newPC, err := s.coldOpImpl(th, ev, ipc, w, regs, entryDepth)
	if err == nil && s.abortErr != nil {
		err = s.abortErr
	}
	if err != nil {
		if s.handleError(th, entryDepth, err) {
			return coldReload, 0, nil
		}
		if uerr := th.uncaughtErr(s); uerr != nil {
			err = uerr
			th.setUncaughtErr(s, nil)
		}
		if s.abortErr != nil {
			return coldContinue, 0, s.abortErr
		}
		return coldContinue, 0, err
	}
	return act, newPC, nil
}

func (s *State) coldOpImpl(th *Thread, ev *frameEnv, ipc int, w bytecode.Instruction, regs *[256]Value, entryDepth int) (coldAction, int, error) {
	pc := ipc + 1
	a := uint8(w >> bytecode.PosA)
	op := bytecode.OpCode(w & 0x7f)

	switch op {
	case bytecode.OP_LOADKX:
		ax := ev.insns.at(pc).Ax()
		pc++
		regs[a] = ev.consts[ax]
		return coldContinue, pc, nil

	case bytecode.OP_SETTABUP:
		uv := s.closureUpval(ev.cl, int(a))
		tv := uv.Get(s)
		b := uint8(w >> bytecode.PosB)
		c := uint8(w >> bytecode.PosC)
		var val Value
		if w.K() {
			val = ev.consts[c]
		} else {
			val = regs[c]
		}
		key := s.asString(ev.consts[b])
		if tv.IsTable() {
			t := s.asTable(tv)
			if err := s.ensureLookupStrKeyHash(t, key); err != nil {
				return coldContinue, pc, err
			}
			if !t.RawGetStr(s, key).IsNil() || !t.hasMeta() || s.getTM(t.Metatable(s), TMNewIndex).IsNil() {
				t.RawSetStr(s, key, val)
				if err := s.ConsumeMemErr(); err != nil {
					return coldContinue, pc, err
				}
				return coldContinue, pc, nil
			}
		}
		pushed, err := s.finishSetYieldable(th, ev, tv, ev.consts[b], val)
		if err != nil {
			return coldContinue, pc, err
		}
		if pushed {
			return coldReload, 0, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_NEWTABLE:
		vb := w.VB()
		vc := w.VC()
		nrec := 0
		if vb > 0 {
			nrec = 1 << (vb - 1)
		}
		nseq := vc
		if w.K() {
			nseq += ev.insns.at(pc).Ax() * (bytecode.MaxArgvC + 1)
		}
		pc++
		estBytes := tablePreallocBytes(int64(nseq), int64(nrec))
		if err := s.checkMem(estBytes); err != nil {
			return coldContinue, pc, err
		}
		t := s.NewTable(nseq, nrec)
		if err := s.ConsumeMemErr(); err != nil {
			return coldContinue, pc, err
		}
		if t == nil {
			return coldContinue, pc, s.memError()
		}
		regs[a] = TableValue(t)
		s.recordNewTableAlloc(estBytes, ev.base+int(a)+1)
		if s.gcNeedsCheck() {
			s.gcCheckStep(ev.base + int(a) + 1)
			if s.abortErr != nil {
				return coldContinue, pc, s.abortErr
			}
		}
		return coldContinue, pc, nil

	case bytecode.OP_ADDK, bytecode.OP_SUBK, bytecode.OP_MULK, bytecode.OP_DIVK:
		rb, kc := regs[uint8(w>>bytecode.PosB)], ev.consts[uint8(w>>bytecode.PosC)]
		if rb.IsNumber() && kc.IsNumber() {
			fb, _ := rb.ToFloat()
			fc, _ := kc.ToFloat()
			switch op {
			case bytecode.OP_ADDK:
				regs[a].SetFloat(fb + fc)
			case bytecode.OP_SUBK:
				regs[a].SetFloat(fb - fc)
			case bytecode.OP_MULK:
				regs[a].SetFloat(fb * fc)
			case bytecode.OP_DIVK:
				regs[a].SetFloat(fb / fc)
			}
			return coldContinue, pc + 1, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_ADD, bytecode.OP_SUB, bytecode.OP_MUL, bytecode.OP_DIV:
		rb, rc := regs[uint8(w>>bytecode.PosB)], regs[uint8(w>>bytecode.PosC)]
		if rb.IsNumber() && rc.IsNumber() {
			fb, _ := rb.ToFloat()
			fc, _ := rc.ToFloat()
			switch op {
			case bytecode.OP_ADD:
				regs[a].SetFloat(fb + fc)
			case bytecode.OP_SUB:
				regs[a].SetFloat(fb - fc)
			case bytecode.OP_MUL:
				regs[a].SetFloat(fb * fc)
			case bytecode.OP_DIV:
				regs[a].SetFloat(fb / fc)
			}
			return coldContinue, pc + 1, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_MODK:
		rb, kc := regs[uint8(w>>bytecode.PosB)], ev.consts[uint8(w>>bytecode.PosC)]
		if BothInt(rb, kc) {
			r, err := IntMod(rb.Int(), kc.Int())
			if err != nil {
				return coldContinue, pc, err
			}
			regs[a].SetInt(r)
			return coldContinue, pc + 1, nil
		}
		if rb.IsNumber() && kc.IsNumber() {
			fb, _ := rb.ToFloat()
			fc, _ := kc.ToFloat()
			regs[a].SetFloat(FloatMod(fb, fc))
			return coldContinue, pc + 1, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_POWK:
		rb, kc := regs[uint8(w>>bytecode.PosB)], ev.consts[uint8(w>>bytecode.PosC)]
		if rb.IsNumber() && kc.IsNumber() {
			fb, _ := rb.ToFloat()
			fc, _ := kc.ToFloat()
			regs[a].SetFloat(math.Pow(fb, fc))
			return coldContinue, pc + 1, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_IDIVK:
		rb, kc := regs[uint8(w>>bytecode.PosB)], ev.consts[uint8(w>>bytecode.PosC)]
		if BothInt(rb, kc) {
			r, err := IntDiv(rb.Int(), kc.Int())
			if err != nil {
				return coldContinue, pc, err
			}
			regs[a].SetInt(r)
			return coldContinue, pc + 1, nil
		}
		if rb.IsNumber() && kc.IsNumber() {
			fb, _ := rb.ToFloat()
			fc, _ := kc.ToFloat()
			regs[a].SetFloat(math.Floor(fb / fc))
			return coldContinue, pc + 1, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_BANDK:
		rb, kc := regs[uint8(w>>bytecode.PosB)], ev.consts[uint8(w>>bytecode.PosC)]
		if i1, ok := rb.ToInt(); ok {
			regs[a].SetInt(i1 & kc.Int())
			return coldContinue, pc + 1, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_BORK:
		rb, kc := regs[uint8(w>>bytecode.PosB)], ev.consts[uint8(w>>bytecode.PosC)]
		if i1, ok := rb.ToInt(); ok {
			regs[a].SetInt(i1 | kc.Int())
			return coldContinue, pc + 1, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_BXORK:
		rb, kc := regs[uint8(w>>bytecode.PosB)], ev.consts[uint8(w>>bytecode.PosC)]
		if i1, ok := rb.ToInt(); ok {
			regs[a].SetInt(i1 ^ kc.Int())
			return coldContinue, pc + 1, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_SHLI:
		rb := regs[uint8(w>>bytecode.PosB)]
		if ib, ok := rb.ToInt(); ok {
			regs[a].SetInt(ShiftLeft(int64(w.SC()), ib))
			return coldContinue, pc + 1, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_SHRI:
		rb := regs[uint8(w>>bytecode.PosB)]
		if ib, ok := rb.ToInt(); ok {
			regs[a].SetInt(ShiftRight(ib, int64(w.SC())))
			return coldContinue, pc + 1, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_MOD:
		rb, rc := regs[uint8(w>>bytecode.PosB)], regs[uint8(w>>bytecode.PosC)]
		if BothInt(rb, rc) {
			r, err := IntMod(rb.Int(), rc.Int())
			if err != nil {
				return coldContinue, pc, err
			}
			regs[a].SetInt(r)
			return coldContinue, pc + 1, nil
		}
		if rb.IsNumber() && rc.IsNumber() {
			fb, _ := rb.ToFloat()
			fc, _ := rc.ToFloat()
			regs[a].SetFloat(FloatMod(fb, fc))
			return coldContinue, pc + 1, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_POW:
		rb, rc := regs[uint8(w>>bytecode.PosB)], regs[uint8(w>>bytecode.PosC)]
		if rb.IsNumber() && rc.IsNumber() {
			fb, _ := rb.ToFloat()
			fc, _ := rc.ToFloat()
			regs[a].SetFloat(math.Pow(fb, fc))
			return coldContinue, pc + 1, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_IDIV:
		rb, rc := regs[uint8(w>>bytecode.PosB)], regs[uint8(w>>bytecode.PosC)]
		if BothInt(rb, rc) {
			r, err := IntDiv(rb.Int(), rc.Int())
			if err != nil {
				return coldContinue, pc, err
			}
			regs[a].SetInt(r)
			return coldContinue, pc + 1, nil
		}
		if rb.IsNumber() && rc.IsNumber() {
			fb, _ := rb.ToFloat()
			fc, _ := rc.ToFloat()
			regs[a].SetFloat(math.Floor(fb / fc))
			return coldContinue, pc + 1, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_BAND:
		rb, rc := regs[uint8(w>>bytecode.PosB)], regs[uint8(w>>bytecode.PosC)]
		if i1, ok := rb.ToInt(); ok {
			if i2, ok := rc.ToInt(); ok {
				regs[a].SetInt(i1 & i2)
				return coldContinue, pc + 1, nil
			}
		}
		return coldContinue, pc, nil

	case bytecode.OP_BOR:
		rb, rc := regs[uint8(w>>bytecode.PosB)], regs[uint8(w>>bytecode.PosC)]
		if i1, ok := rb.ToInt(); ok {
			if i2, ok := rc.ToInt(); ok {
				regs[a].SetInt(i1 | i2)
				return coldContinue, pc + 1, nil
			}
		}
		return coldContinue, pc, nil

	case bytecode.OP_BXOR:
		rb, rc := regs[uint8(w>>bytecode.PosB)], regs[uint8(w>>bytecode.PosC)]
		if i1, ok := rb.ToInt(); ok {
			if i2, ok := rc.ToInt(); ok {
				regs[a].SetInt(i1 ^ i2)
				return coldContinue, pc + 1, nil
			}
		}
		return coldContinue, pc, nil

	case bytecode.OP_SHL:
		rb, rc := regs[uint8(w>>bytecode.PosB)], regs[uint8(w>>bytecode.PosC)]
		if i1, ok := rb.ToInt(); ok {
			if i2, ok := rc.ToInt(); ok {
				regs[a].SetInt(ShiftLeft(i1, i2))
				return coldContinue, pc + 1, nil
			}
		}
		return coldContinue, pc, nil

	case bytecode.OP_SHR:
		rb, rc := regs[uint8(w>>bytecode.PosB)], regs[uint8(w>>bytecode.PosC)]
		if i1, ok := rb.ToInt(); ok {
			if i2, ok := rc.ToInt(); ok {
				regs[a].SetInt(ShiftRight(i1, i2))
				return coldContinue, pc + 1, nil
			}
		}
		return coldContinue, pc, nil

	case bytecode.OP_BNOT:
		b := uint8(w >> bytecode.PosB)
		rb := regs[b]
		if ib, ok := rb.ToInt(); ok {
			regs[a].SetInt(^ib)
			return coldContinue, pc, nil
		}
		f := s.getTMByObj(rb, TMBnot)
		if f.IsNil() {
			if rb.IsFloat() {
				return coldContinue, pc, fmt.Errorf("number%s has no integer representation", varInfo(ev.bp.proto, ipc, int(b)))
			}
			return coldContinue, pc, fmt.Errorf("attempt to perform bitwise operation on a %s value%s", s.objTypeName(rb), varInfo(ev.bp.proto, ipc, int(b)))
		}
		pushed, err := s.pushMetamethodFrame(th, ev, f, []Value{rb, rb}, finishSetReg, a, false)
		if err != nil {
			return coldContinue, pc, err
		}
		if pushed {
			return coldReload, 0, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_UNM:
		b := uint8(w >> bytecode.PosB)
		rb := regs[b]
		f := s.getTMByObj(rb, TMUnm)
		if f.IsNil() {
			return coldContinue, pc, fmt.Errorf("attempt to perform arithmetic on a %s value%s", s.objTypeName(rb), varInfo(ev.bp.proto, ipc, int(b)))
		}
		pushed, err := s.pushMetamethodFrame(th, ev, f, []Value{rb, rb}, finishSetReg, a, false)
		if err != nil {
			return coldContinue, pc, err
		}
		if pushed {
			return coldReload, 0, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_LEN:
		rb := regs[uint8(w>>bytecode.PosB)]
		tm := s.getTMByObj(rb, TMLen)
		if tm.IsNil() {
			return coldContinue, pc, fmt.Errorf("attempt to get length of a %s value", rb.TypeName())
		}
		pushed, err := s.pushMetamethodFrame(th, ev, tm, []Value{rb, rb}, finishSetReg, a, false)
		if err != nil {
			return coldContinue, pc, err
		}
		if pushed {
			return coldReload, 0, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_MMBIN:
		prev := ev.insns.at(ipc - 1)
		dst := uint8(prev >> bytecode.PosA)
		tm := TM(uint8(w >> bytecode.PosC))
		b := uint8(w >> bytecode.PosB)
		p1, p2 := regs[a], regs[b]
		f := s.getTMByObj(p1, tm)
		if f.IsNil() {
			f = s.getTMByObj(p2, tm)
		}
		if f.IsNil() {
			return coldContinue, pc, s.mmBinError(ev.bp.proto, ipc-1, tm, p1, p2, int(a), int(b))
		}
		pushed, err := s.pushMetamethodFrame(th, ev, f, []Value{p1, p2}, finishSetReg, dst, false)
		if err != nil {
			return coldContinue, pc, err
		}
		if pushed {
			return coldReload, 0, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_MMBINI:
		prev := ev.insns.at(ipc - 1)
		dst := uint8(prev >> bytecode.PosA)
		tm := TM(uint8(w >> bytecode.PosC))
		p1, p2 := regs[a], IntValue(int64(w.SB()))
		r1, r2 := int(a), -1
		if w.K() {
			p1, p2 = p2, p1
			r1, r2 = -1, int(a)
		}
		f := s.getTMByObj(p1, tm)
		if f.IsNil() {
			f = s.getTMByObj(p2, tm)
		}
		if f.IsNil() {
			return coldContinue, pc, s.mmBinError(ev.bp.proto, ipc-1, tm, p1, p2, r1, r2)
		}
		pushed, err := s.pushMetamethodFrame(th, ev, f, []Value{p1, p2}, finishSetReg, dst, false)
		if err != nil {
			return coldContinue, pc, err
		}
		if pushed {
			return coldReload, 0, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_MMBINK:
		prev := ev.insns.at(ipc - 1)
		dst := uint8(prev >> bytecode.PosA)
		tm := TM(uint8(w >> bytecode.PosC))
		p1, p2 := regs[a], ev.consts[uint8(w>>bytecode.PosB)]
		r1, r2 := int(a), -1
		if w.K() {
			p1, p2 = p2, p1
			r1, r2 = -1, int(a)
		}
		f := s.getTMByObj(p1, tm)
		if f.IsNil() {
			f = s.getTMByObj(p2, tm)
		}
		if f.IsNil() {
			return coldContinue, pc, s.mmBinError(ev.bp.proto, ipc-1, tm, p1, p2, r1, r2)
		}
		pushed, err := s.pushMetamethodFrame(th, ev, f, []Value{p1, p2}, finishSetReg, dst, false)
		if err != nil {
			return coldContinue, pc, err
		}
		if pushed {
			return coldReload, 0, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_CONCAT:
		b := int(uint8(w >> bytecode.PosB))
		pushed, err := s.concatYieldable(th, &th.framesSlice(s.arena)[ev.ci], ev.base+int(a), b)
		if err != nil {
			return coldContinue, pc, err
		}
		if pushed {
			return coldReload, 0, nil
		}
		return coldContinue, pc, nil

	case bytecode.OP_CLOSE:
		level := ev.base + int(a)
		th.closeUpvals(s, level)
		if th.hasTBCAtOrAbove(s.arena, level) {
			cf := &th.framesSlice(s.arena)[ev.ci]
			cf.dirty = true
			cf.closeLevel = level
			cf.closeHasErr = false
			cf.closeResume = 0
			cont, err := s.resumeCloseTBC(th, cf, entryDepth)
			if err != nil {
				return coldContinue, pc, err
			}
			if !cont {
				return coldReload, 0, nil
			}
		}
		return coldContinue, pc, nil

	case bytecode.OP_TBC:
		if err := th.markTBC(s, ev.base+int(a)); err != nil {
			if name, ok := findLocalName(ev.bp.proto, int(a)+1, pc); ok && name != "" {
				return coldContinue, pc, fmt.Errorf("variable '%s' got a non-closable value", name)
			}
			return coldContinue, pc, err
		}
		return coldContinue, pc, nil

	case bytecode.OP_TFORPREP:
		tmp := regs[a+3]
		regs[a+3] = regs[a+2]
		regs[a+2] = tmp
		if !tmp.IsFalsy() {
			if err := th.markTBC(s, ev.base+int(a)+2); err != nil {
				return coldContinue, pc, err
			}
		}
		return coldContinue, pc + w.Bx(), nil

	case bytecode.OP_EQ:
		ra, rb := regs[a], regs[uint8(w>>bytecode.PosB)]
		eq, err := s.rawEqual(ra, rb)
		if err != nil {
			return coldContinue, pc, err
		}
		if eq {
			if true != w.K() {
				pc++
			} else {
				pc += 1 + ev.insns.at(pc).SJ()
			}
			return coldContinue, pc, nil
		}
		if (ra.IsTable() && rb.IsTable()) || (ra.IsUserdata() && rb.IsUserdata()) {
			tm := s.getTMByObj(ra, TMEq)
			if tm.IsNil() {
				tm = s.getTMByObj(rb, TMEq)
			}
			if !tm.IsNil() {
				pushed, err := s.pushMetamethodFrame(th, ev, tm, []Value{ra, rb}, finishCondJump, 0, w.K())
				if err != nil {
					return coldContinue, pc, err
				}
				if pushed {
					return coldReload, 0, nil
				}
				return coldContinue, th.framesSlice(s.arena)[ev.ci].pc, nil
			}
		}
		if false != w.K() {
			pc++
		} else {
			pc += 1 + ev.insns.at(pc).SJ()
		}
		return coldContinue, pc, nil

	case bytecode.OP_EQK:
		ra, rb := regs[a], ev.consts[uint8(w>>bytecode.PosB)]
		eq, err := s.rawEqual(ra, rb)
		if err != nil {
			return coldContinue, pc, err
		}
		if eq != w.K() {
			pc++
		} else {
			pc += 1 + ev.insns.at(pc).SJ()
		}
		return coldContinue, pc, nil

	case bytecode.OP_LT:
		ra, rb := regs[a], regs[uint8(w>>bytecode.PosB)]
		if ra.IsString() && rb.IsString() {
			cmp, err := s.compareStrings(s.asString(ra), s.asString(rb))
			if err != nil {
				return coldContinue, pc, err
			}
			cond := cmp < 0
			if cond != w.K() {
				pc++
			} else {
				pc += 1 + ev.insns.at(pc).SJ()
			}
			return coldContinue, pc, nil
		}
		if ra.IsInt() && rb.IsFloat() {
			cond := LTIntFloat(ra.Int(), rb.Float())
			if cond != w.K() {
				pc++
			} else {
				pc += 1 + ev.insns.at(pc).SJ()
			}
			return coldContinue, pc, nil
		}
		if ra.IsFloat() && rb.IsInt() {
			cond := LTFloatInt(ra.Float(), rb.Int())
			if cond != w.K() {
				pc++
			} else {
				pc += 1 + ev.insns.at(pc).SJ()
			}
			return coldContinue, pc, nil
		}
		tm := s.getTMByObj(ra, TMLt)
		if tm.IsNil() {
			tm = s.getTMByObj(rb, TMLt)
		}
		if tm.IsNil() {
			return coldContinue, pc, s.compareError(ra, rb)
		}
		pushed, err := s.pushMetamethodFrame(th, ev, tm, []Value{ra, rb}, finishCondJump, 0, w.K())
		if err != nil {
			return coldContinue, pc, err
		}
		if pushed {
			return coldReload, 0, nil
		}
		return coldContinue, th.framesSlice(s.arena)[ev.ci].pc, nil

	case bytecode.OP_LE:
		ra, rb := regs[a], regs[uint8(w>>bytecode.PosB)]
		if ra.IsString() && rb.IsString() {
			cmp, err := s.compareStrings(s.asString(ra), s.asString(rb))
			if err != nil {
				return coldContinue, pc, err
			}
			cond := cmp <= 0
			if cond != w.K() {
				pc++
			} else {
				pc += 1 + ev.insns.at(pc).SJ()
			}
			return coldContinue, pc, nil
		}
		if ra.IsInt() && rb.IsFloat() {
			cond := LEIntFloat(ra.Int(), rb.Float())
			if cond != w.K() {
				pc++
			} else {
				pc += 1 + ev.insns.at(pc).SJ()
			}
			return coldContinue, pc, nil
		}
		if ra.IsFloat() && rb.IsInt() {
			cond := LEFloatInt(ra.Float(), rb.Int())
			if cond != w.K() {
				pc++
			} else {
				pc += 1 + ev.insns.at(pc).SJ()
			}
			return coldContinue, pc, nil
		}
		tm := s.getTMByObj(ra, TMLe)
		if tm.IsNil() {
			tm = s.getTMByObj(rb, TMLe)
		}
		if tm.IsNil() {
			return coldContinue, pc, s.compareError(ra, rb)
		}
		pushed, err := s.pushMetamethodFrame(th, ev, tm, []Value{ra, rb}, finishCondJump, 0, w.K())
		if err != nil {
			return coldContinue, pc, err
		}
		if pushed {
			return coldReload, 0, nil
		}
		return coldContinue, th.framesSlice(s.arena)[ev.ci].pc, nil

	case bytecode.OP_LTI, bytecode.OP_LEI, bytecode.OP_GTI, bytecode.OP_GEI:
		ra := regs[a]
		im := IntValue(int64(w.SB()))
		if w.C() != 0 {
			im = FloatValue(float64(w.SB()))
		}
		var p1, p2 Value
		var event TM
		switch op {
		case bytecode.OP_LTI:
			p1, p2, event = ra, im, TMLt
		case bytecode.OP_LEI:
			p1, p2, event = ra, im, TMLe
		case bytecode.OP_GTI:
			p1, p2, event = im, ra, TMLt
		case bytecode.OP_GEI:
			p1, p2, event = im, ra, TMLe
		}
		tm := s.getTMByObj(p1, event)
		if tm.IsNil() {
			tm = s.getTMByObj(p2, event)
		}
		if tm.IsNil() {
			return coldContinue, pc, s.compareError(p1, p2)
		}
		pushed, err := s.pushMetamethodFrame(th, ev, tm, []Value{p1, p2}, finishCondJump, 0, w.K())
		if err != nil {
			return coldContinue, pc, err
		}
		if pushed {
			return coldReload, 0, nil
		}
		return coldContinue, th.framesSlice(s.arena)[ev.ci].pc, nil

	case bytecode.OP_TAILCALL:
		b := int(uint8(w >> bytecode.PosB))
		nargs := b - 1
		if b == 0 {
			nargs = max(0, th.top-(ev.base+int(a)+1))
		}
		if w.K() {
			th.closeUpvals(s, ev.base)
		}
		cf := &th.framesSlice(s.arena)[ev.ci]
		funcIdx := cf.funcIdx
		srcStart := ev.base + int(a)
		fn := th.stackSlice(s.arena)[srcStart]
		// Resolve __call metamethod chain IN-PLACE (max 15 hops per Lua 5.5) so tail calls stay O(1) stack!
		extraDepth := uint8(0)
		if !fn.IsClosure() && !fn.IsNative() {
			for depth := 1; depth <= 15; depth++ {
				tm := s.getTMByObj(fn, TMCall)
				if tm.IsNil() {
					return coldContinue, pc, fmt.Errorf("attempt to call a %s value%s", fn.TypeName(), varInfo(ev.bp.proto, ipc, int(a)))
				}
				if err := s.ensureStackWithRoots(th, srcStart+nargs+4, tm, nil); err != nil {
					return coldContinue, pc, err
				}
				st := th.stackSlice(s.arena)
				copy(st[srcStart+2:srcStart+2+nargs], st[srcStart+1:srcStart+1+nargs])
				st[srcStart+1] = fn
				st[srcStart] = tm
				nargs++
				if end := srcStart + 1 + nargs; th.top < end {
					th.top = end
				}
				fn = tm
				extraDepth = uint8(depth)
				if fn.IsClosure() || fn.IsNative() {
					break
				}
			}
			if !fn.IsClosure() && !fn.IsNative() {
				return coldContinue, pc, fmt.Errorf("'__call' chain too long")
			}
		}
		if fn.IsClosure() {
			callee := s.asClosure(fn)
			p := s.boundProtoOf(callee).proto
			if err := th.ensureStack(s, funcIdx+1+max(nargs, int(p.MaxStackSize))+int(p.NumParams)+8); err != nil {
				return coldContinue, pc, err
			}
			st := th.stackSlice(s.arena)
			copy(st[funcIdx:funcIdx+1+nargs], st[srcStart:srcStart+1+nargs])
			newBase := funcIdx + 1
			if p.IsVararg() {
				th.top = newBase + nargs
			} else {
				nparams := int(p.NumParams)
				if nargs < nparams {
					fillNil(st[newBase+nargs : newBase+nparams])
				}
				if int(p.MaxStackSize) > nparams {
					fillNil(st[newBase+nparams : newBase+int(p.MaxStackSize)])
				}
				th.top = newBase + int(p.MaxStackSize)
			}
			cf = &th.framesSlice(s.arena)[ev.ci]
			s.setFrameClosure(cf, callee)
			cf.base = newBase
			cf.pc = 0
			cf.dirty = true
			cf.nextraargs = 0
			s.setFrameVarargTab(cf, nil)
			cf.isTailCall = true
			cf.extraArgs = extraDepth
			cf.oldPC = -1
			if th.hookMask&HookMaskCall != 0 && !th.inHook && !th.suppressHook {
				nt := min(nargs, int(p.NumParams))
				cf.ftransfer = 1
				cf.ntransfer = uint16(nt)
				if herr := s.fireHook(th, "tail call", NilValue()); herr != nil {
					return coldContinue, pc, herr
				}
			}
			return coldReload, 0, nil
		}
		// Tailcall to native (check yieldable pcall/xpcall and coroutine.yield!)
		nf := s.asNative(fn)
		st := th.stackSlice(s.arena)
		if nf.Intrinsic == intrPCall && nargs >= 1 && isFlatProtectedTarget(st[srcStart+1]) {
			if pc < len(ev.bp.proto.Code) && ev.bp.proto.Code[pc].Op() == bytecode.OP_RETURN {
				// Keep caller Lua frame alive across C pcall so stack trace & return hooks match Lua 5.5!
				th.framesSlice(s.arena)[ev.ci].pc = pc
				pushed, err := s.pushProtectedCall(th, srcStart, nargs, -1, false, entryDepth)
				if err != nil {
					return coldContinue, pc, err
				}
				if pushed {
					return coldReload, 0, nil
				}
				return coldContinue, pc, nil
			}
			nres := th.framesSlice(s.arena)[ev.ci].nresults
			if err := th.ensureStack(s, funcIdx+1+nargs+4); err != nil {
				return coldContinue, pc, err
			}
			st = th.stackSlice(s.arena)
			copy(st[funcIdx:funcIdx+1+nargs], st[srcStart:srcStart+1+nargs])
			th.nframe--
			_, err := s.pushProtectedCall(th, funcIdx, nargs, nres, false, entryDepth)
			if err != nil {
				return coldContinue, pc, err
			}
			if done, ferr := s.prepareLuaFrame(th, entryDepth); ferr != nil {
				return coldContinue, pc, ferr
			} else if done {
				return coldReturn, 0, nil
			}
			return coldReload, 0, nil
		}
		if nf.Intrinsic == intrXPCall && nargs >= 2 && isFlatProtectedTarget(st[srcStart+1]) && st[srcStart+2].IsFunction() {
			if pc < len(ev.bp.proto.Code) && ev.bp.proto.Code[pc].Op() == bytecode.OP_RETURN {
				th.framesSlice(s.arena)[ev.ci].pc = pc
				pushed, err := s.pushProtectedCall(th, srcStart, nargs, -1, true, entryDepth)
				if err != nil {
					return coldContinue, pc, err
				}
				if pushed {
					return coldReload, 0, nil
				}
				return coldContinue, pc, nil
			}
			nres := th.framesSlice(s.arena)[ev.ci].nresults
			if err := th.ensureStack(s, funcIdx+1+nargs+4); err != nil {
				return coldContinue, pc, err
			}
			st = th.stackSlice(s.arena)
			copy(st[funcIdx:funcIdx+1+nargs], st[srcStart:srcStart+1+nargs])
			th.nframe--
			_, err := s.pushProtectedCall(th, funcIdx, nargs, nres, true, entryDepth)
			if err != nil {
				return coldContinue, pc, err
			}
			if done, ferr := s.prepareLuaFrame(th, entryDepth); ferr != nil {
				return coldContinue, pc, ferr
			} else if done {
				return coldReturn, 0, nil
			}
			return coldReload, 0, nil
		}
		if nf.Intrinsic == intrDoFile {
			nres := th.framesSlice(s.arena)[ev.ci].nresults
			th.top = srcStart + 1 + nargs
			loaded, lerr := nativeLoadFile(s, dofileArgs(st[srcStart+1:srcStart+1+nargs]))
			if lerr != nil {
				return coldContinue, pc, lerr
			}
			if len(loaded) == 0 || loaded[0].IsNil() {
				if len(loaded) > 1 {
					return coldContinue, pc, &LuaError{Val: loaded[1]}
				}
				return coldContinue, pc, fmt.Errorf("dofile failed")
			}
			th.stackSlice(s.arena)[funcIdx] = loaded[0]
			th.nframe--
			cl := s.asClosure(loaded[0])
			if err := th.pushLuaFrame(s, cl, funcIdx, 0, nres); err != nil {
				return coldContinue, pc, err
			}
			if herr := s.fireLuaCallHook(th, cl, 0); herr != nil {
				return coldContinue, pc, herr
			}
			return coldReload, 0, nil
		}
		if nf.Intrinsic == intrPairs && nargs >= 1 {
			tm, cargs, tmExtra, hasTM, terr := s.resolvePairsTM(st[srcStart+1])
			if terr != nil {
				return coldContinue, pc, terr
			}
			if hasTM {
				if err := s.ensureStackWithRoots(th, srcStart+1+len(cargs)+8, tm, cargs); err != nil {
					return coldContinue, pc, err
				}
				st = th.stackSlice(s.arena)
				st[srcStart] = tm
				copy(st[srcStart+1:srcStart+1+len(cargs)], cargs)
				th.top = srcStart + 1 + len(cargs)
				nextIsMatchingRet := pc < len(ev.bp.proto.Code) &&
					ev.bp.proto.Code[pc].Op() == bytecode.OP_RETURN &&
					ev.bp.proto.Code[pc].A() == int(a) &&
					ev.bp.proto.Code[pc].B() == 0
				callerCF := &th.framesSlice(s.arena)[ev.ci]
				callerCF.pc = pc
				callerCF.dirty = true
				callerCF.oldTop = srcStart
				if nextIsMatchingRet {
					callerCF.finish = finishPairs
					callerCF.closeRetN = -1
				} else {
					callerCF.finish = finishTailReturn
				}
				if tm.IsClosure() {
					cl := s.asClosure(tm)
					if err := th.pushLuaFrame(s, cl, srcStart, len(cargs), 4); err != nil {
						th.framesSlice(s.arena)[ev.ci].finish = finishNone
						return coldContinue, pc, err
					}
					th.framesSlice(s.arena)[th.nframe-1].extraArgs = tmExtra
					if herr := s.fireLuaCallHook(th, cl, len(cargs)); herr != nil {
						return coldContinue, pc, herr
					}
					return coldReload, 0, nil
				}
				res, _, err := s.callNativeHooked(th, tm, srcStart, len(cargs), tmExtra, maxFrames+128)
				if errors.Is(err, errYieldSentinel) {
					th.yieldDst = srcStart
					th.yieldNRes = 4
					th.yieldedNative = true
					th.yieldNativeFn = tm
					return coldContinue, pc, errYieldSentinel
				}
				th.framesSlice(s.arena)[ev.ci].finish = finishNone
				if err != nil {
					if !errors.Is(err, errCoroCloseSelf) {
						th.lastErrNative = tm
					}
					return coldContinue, pc, err
				}
				out := []Value{NilValue(), NilValue(), NilValue(), NilValue()}
				copy(out, res)
				if nextIsMatchingRet {
					if perr := th.placeResults(s, srcStart, out, 4); perr != nil {
						return coldContinue, pc, perr
					}
					if s.gcNeedsCheck() {
						s.gcCheckStep(th.top)
						if s.abortErr != nil {
							return coldContinue, pc, s.abortErr
						}
					}
					return coldContinue, pc, nil
				}
				nres := th.framesSlice(s.arena)[ev.ci].nresults
				if perr := th.placeResults(s, funcIdx, out, nres); perr != nil {
					return coldContinue, pc, perr
				}
				th.nframe--
				if s.gcNeedsCheck() {
					s.gcCheckStep(th.top)
					if s.abortErr != nil {
						return coldContinue, pc, s.abortErr
					}
				}
				if th.nframe <= entryDepth {
					return coldReturn, 0, nil
				}
				return coldReload, 0, nil
			}
		}
		nres := th.framesSlice(s.arena)[ev.ci].nresults
		th.top = srcStart + 1 + nargs
		hookedNative := th.hookMask != 0 && th.hookMask&(HookMaskCall|HookMaskRet) != 0 && !th.inHook && !th.suppressHook
		pushedFrame := hookedNative || (nf.FastFn == nil && nf.Intrinsic == intrNone)
		if pushedFrame {
			if err := th.ensureFrameCap(s, th.nframe+1, maxFrames+128); err != nil {
				return coldContinue, pc, err
			}
			th.framesSlice(s.arena)[th.nframe] = callFrame{
				kind:      frameNative,
				base:      srcStart + 1,
				funcIdx:   srcStart,
				nativeFn:  fn,
				ftransfer: 1,
				ntransfer: uint16(nargs),
				dirty:     true,
			}
			th.nframe++
			if hookedNative && th.hookMask&HookMaskCall != 0 {
				if herr := s.fireHook(th, "call", NilValue()); herr != nil {
					th.nframe--
					return coldContinue, pc, herr
				}
			}
		}
		args := make([]Value, nargs)
		copy(args, th.stackSlice(s.arena)[srcStart+1:srcStart+1+nargs])
		res, err := nf.callMulti(s, args)
		if pushedFrame || (th.hookMask != 0 && err == nil && th.hookMask&HookMaskRet != 0 && !th.inHook && !th.suppressHook) {
			var hookErr error
			if pushedFrame {
				if err == nil && th.hookMask&HookMaskRet != 0 && !th.inHook && !th.suppressHook {
					if serr := s.ensureStackWithRoots(th, srcStart+1+len(res)+4, NilValue(), res); serr != nil {
						hookErr = serr
					} else {
						copy(th.stackSlice(s.arena)[srcStart+1:srcStart+1+len(res)], res)
						if end := srcStart + 1 + len(res); th.top < end {
							th.top = end
						}
						frames := th.framesSlice(s.arena)
						frames[th.nframe-1].ftransfer = 1
						frames[th.nframe-1].ntransfer = uint16(len(res))
						hookErr = s.fireHook(th, "return", NilValue())
						if hookErr == nil && len(res) > 0 {
							copy(res, th.stackSlice(s.arena)[srcStart+1:srcStart+1+len(res)])
						}
					}
				}
				th.nframe--
			} else {
				if ferr := s.ensureFrameCapWithRoots(th, th.nframe+1, maxFrames+128, NilValue(), res); ferr != nil {
					hookErr = ferr
				} else if serr := s.ensureStackWithRoots(th, srcStart+1+len(res)+4, NilValue(), res); serr != nil {
					hookErr = serr
				} else {
					copy(th.stackSlice(s.arena)[srcStart+1:srcStart+1+len(res)], res)
					if end := srcStart + 1 + len(res); th.top < end {
						th.top = end
					}
					th.framesSlice(s.arena)[th.nframe] = callFrame{
						kind:      frameNative,
						base:      srcStart + 1,
						funcIdx:   srcStart,
						nativeFn:  fn,
						ftransfer: 1,
						ntransfer: uint16(len(res)),
						dirty:     true,
					}
					th.nframe++
					hookErr = s.fireHook(th, "return", NilValue())
					if hookErr == nil && len(res) > 0 {
						copy(res, th.stackSlice(s.arena)[srcStart+1:srcStart+1+len(res)])
					}
					th.nframe--
				}
			}
			if hookErr != nil && err == nil {
				return coldContinue, pc, hookErr
			}
		}
		if errors.Is(err, errYieldSentinel) {
			// Keep the caller Lua frame alive across the yielding C tail call (matching Lua 5.5
			// lvm.c OP_TAILCALL + luaD_pretailcall) so debug.getinfo sees the caller while
			// suspended and resume fires both the native return hook and the caller's return hook.
			callerCF := &th.framesSlice(s.arena)[ev.ci]
			callerCF.pc = pc
			callerCF.dirty = true
			th.yieldDst = srcStart
			th.yieldNRes = -1
			th.yieldedNative = true
			th.yieldNativeFn = fn
			nextIsMatchingRet := pc < len(ev.bp.proto.Code) &&
				ev.bp.proto.Code[pc].Op() == bytecode.OP_RETURN &&
				ev.bp.proto.Code[pc].A() == int(a) &&
				ev.bp.proto.Code[pc].B() == 0
			if !nextIsMatchingRet {
				callerCF.finish = finishTailReturn
				callerCF.oldTop = srcStart
			}
			return coldContinue, pc, errYieldSentinel
		}
		if err != nil {
			return coldContinue, pc, err
		}
		// Fire caller Lua frame's return hook before popping it (matching Lua 5.5 lvm.c OP_TAILCALL + OP_RETURN).
		if th.hookMask&HookMaskRet != 0 && !th.inHook && !th.suppressHook {
			if serr := s.ensureStackWithRoots(th, srcStart+len(res)+4, NilValue(), res); serr != nil {
				return coldContinue, pc, serr
			}
			copy(th.stackSlice(s.arena)[srcStart:srcStart+len(res)], res)
			if end := srcStart + len(res); th.top < end {
				th.top = end
			}
			callerCF := &th.framesSlice(s.arena)[ev.ci]
			callerCF.dirty = true
			callerCF.ftransfer = uint16(srcStart - callerCF.base + 1)
			callerCF.ntransfer = uint16(len(res))
			if herr := s.fireHook(th, "return", NilValue()); herr != nil {
				return coldContinue, pc, herr
			}
			if len(res) > 0 {
				copy(res, th.stackSlice(s.arena)[srcStart:srcStart+len(res)])
			}
		}
		if err := th.placeResults(s, funcIdx, res, nres); err != nil {
			return coldContinue, pc, err
		}
		th.nframe--
		if s.gcNeedsCheck() {
			s.gcCheckStep(th.top)
			if s.abortErr != nil {
				return coldContinue, pc, s.abortErr
			}
		}
		if th.nframe <= entryDepth {
			return coldReturn, 0, nil
		}
		return coldReload, 0, nil

	case bytecode.OP_RETURN, bytecode.OP_RETURN0, bytecode.OP_RETURN1:
		var n int
		switch op {
		case bytecode.OP_RETURN0:
			n = 0
		case bytecode.OP_RETURN1:
			n = 1
		default:
			b := int(uint8(w >> bytecode.PosB))
			n = b - 1
			if n < 0 {
				n = max(0, th.top-(ev.base+int(a)))
			}
		}
		cf := &th.framesSlice(s.arena)[ev.ci]
		srcStart := ev.base + int(a)
		if srcStart+n > th.stackSize() {
			n = max(0, th.stackSize()-srcStart)
		}
		if op == bytecode.OP_RETURN && w.K() {
			th.closeUpvals(s, ev.base)
			if th.hasTBCAtOrAbove(s.arena, ev.base) {
				// Yieldable TBC close before completing return!
				// Protect multret return values above MaxStackSize while closing TBC variables.
				if srcStart+n > th.top {
					th.top = srcStart + n
				}
				cf.dirty = true
				cf.closeLevel = ev.base
				cf.closeHasErr = false
				cf.closeResume = 1
				cf.closeRetSrc = srcStart
				cf.closeRetN = n
				cont, err := s.resumeCloseTBC(th, cf, entryDepth)
				if err != nil {
					return coldContinue, pc, err
				}
				if !cont {
					if th.nframe <= entryDepth {
						return coldReturn, 0, nil
					}
					return coldReload, 0, nil
				}
				cf = &th.framesSlice(s.arena)[ev.ci]
			}
		}
		if th.hookMask&HookMaskRet != 0 && !th.inHook && !th.suppressHook {
			cf.dirty = true
			if op == bytecode.OP_RETURN0 {
				cf.ftransfer = 0
			} else {
				cf.ftransfer = uint16(srcStart - cf.base + 1)
			}
			cf.ntransfer = uint16(n)
			if herr := s.fireHook(th, "return", NilValue()); herr != nil {
				return coldContinue, pc, herr
			}
			cf = &th.framesSlice(s.arena)[ev.ci]
		}
		funcIdx := cf.funcIdx
		nres := cf.nresults
		if nres < 0 {
			if err := th.ensureStack(s, funcIdx+n+4); err != nil {
				return coldContinue, pc, err
			}
			th.nframe--
			st := th.stackSlice(s.arena)
			copy(st[funcIdx:funcIdx+n], st[srcStart:srcStart+n])
			th.top = funcIdx + n
		} else {
			if err := th.ensureStack(s, funcIdx+nres+4); err != nil {
				return coldContinue, pc, err
			}
			th.nframe--
			st := th.stackSlice(s.arena)
			cp := min(n, nres)
			copy(st[funcIdx:funcIdx+cp], st[srcStart:srcStart+cp])
			if cp < nres {
				fillNil(st[funcIdx+cp : funcIdx+nres])
			}
			th.top = funcIdx + nres
		}
		if th.nframe <= entryDepth {
			return coldReturn, 0, nil
		}
		return coldReload, 0, nil

	case bytecode.OP_SETLIST:
		n := w.VB()
		last := w.VC()
		if n == 0 {
			n = max(0, th.top-(ev.base+int(a))-1)
		}
		if w.K() {
			last += ev.insns.at(pc).Ax() * (bytecode.MaxArgvC + 1)
			pc++
		}
		if !regs[a].IsTable() {
			return coldContinue, pc, fmt.Errorf("invalid table in SETLIST")
		}
		if ev.base+int(a)+n >= th.stackSize() {
			n = max(0, th.stackSize()-1-(ev.base+int(a)))
		}
		t := s.asTable(regs[a])
		cancelFn := s.nativeCancelFunc()
		for i := 1; i <= n; i++ {
			if cancelFn != nil && (i&1023) == 0 {
				if cerr := cancelFn(); cerr != nil {
					return coldContinue, pc, cerr
				}
			}
			t.RawSetInt(s, int64(last+i), th.stackSlice(s.arena)[ev.base+int(a)+i])
			if s.memExceeded {
				s.memExceeded = false
				return coldContinue, pc, s.memError()
			}
		}
		return coldContinue, pc, nil

	case bytecode.OP_CLOSURE:
		bx := w.Bx()
		cl, err := s.newSubClosure(th, ev.bp, ev.cl, ev.base, bx, ev.base+int(a)+1)
		if err != nil {
			return coldContinue, pc, err
		}
		regs[a] = ClosureValue(cl)
		if s.gcNeedsCheck() {
			s.gcCheckStep(ev.base + int(a) + 1)
			if s.abortErr != nil {
				return coldContinue, pc, s.abortErr
			}
		}
		return coldContinue, pc, nil

	case bytecode.OP_VARARGPREP:
		cf := &th.framesSlice(s.arena)[ev.ci]
		p := ev.bp.proto
		nparams := int(p.NumParams)
		nactual := max(0, th.top-cf.base)
		if cf.base+nactual > th.stackSize() {
			nactual = max(0, th.stackSize()-cf.base)
		}
		if nactual < nparams {
			fillNil(th.stackSlice(s.arena)[cf.base+nactual : cf.base+nparams])
			nactual = nparams
		}
		nextra := nactual - nparams
		if p.Flag&bytecode.PFVarargTable != 0 {
			if err := s.checkPreallocMem(tablePreallocBytes(int64(nextra), 1)); err != nil {
				return coldContinue, pc, err
			}
			vt := s.NewTable(nextra, 1)
			if err := s.ConsumeMemErr(); err != nil {
				return coldContinue, pc, err
			}
			if vt == nil {
				return coldContinue, pc, s.memError()
			}
			cancelFn := s.nativeCancelFunc()
			for i := 0; i < nextra; i++ {
				if cancelFn != nil && (i&1023) == 1023 {
					if cerr := cancelFn(); cerr != nil {
						return coldContinue, pc, cerr
					}
				}
				vt.RawSetInt(s, int64(i+1), th.stackSlice(s.arena)[cf.base+nparams+i])
				if err := s.ConsumeMemErr(); err != nil {
					return coldContinue, pc, err
				}
			}
			vt.RawSetStr(s, s.intern.Intern("n"), IntValue(int64(nextra)))
			if err := s.ConsumeMemErr(); err != nil {
				return coldContinue, pc, err
			}
			if err := s.ensureStackWithRoots(th, cf.base+int(p.MaxStackSize)+8, TableValue(vt), nil); err != nil {
				return coldContinue, pc, err
			}
			st := th.stackSlice(s.arena)
			st[cf.base+nparams] = TableValue(vt)
			if int(p.MaxStackSize) > nparams+1 {
				fillNil(st[cf.base+nparams+1 : cf.base+int(p.MaxStackSize)])
			}
			cf = &th.framesSlice(s.arena)[ev.ci]
			cf.dirty = true
			s.setFrameVarargTab(cf, vt)
			cf.nextraargs = nextra
			th.top = cf.base + int(p.MaxStackSize)
			if s.gcNeedsCheck() {
				s.gcCheckStep(th.top)
				if s.abortErr != nil {
					return coldContinue, pc, s.abortErr
				}
				return coldReload, 0, nil
			}
			return coldContinue, pc, nil
		}
		if nextra > 0 {
			newBase := cf.base + nactual
			if err := th.ensureStack(s, newBase+int(p.MaxStackSize)+8); err != nil {
				return coldContinue, pc, err
			}
			st := th.stackSlice(s.arena)
			copy(st[newBase:newBase+nparams], st[cf.base:cf.base+nparams])
			if int(p.MaxStackSize) > nparams {
				fillNil(st[newBase+nparams : newBase+int(p.MaxStackSize)])
			}
			cf = &th.framesSlice(s.arena)[ev.ci]
			cf.dirty = true
			cf.base = newBase
			cf.nextraargs = nextra
			ev.base = newBase
			th.top = newBase + int(p.MaxStackSize)
		} else {
			if err := th.ensureStack(s, cf.base+int(p.MaxStackSize)+8); err != nil {
				return coldContinue, pc, err
			}
			if int(p.MaxStackSize) > nparams {
				fillNil(th.stackSlice(s.arena)[cf.base+nparams : cf.base+int(p.MaxStackSize)])
			}
			cf = &th.framesSlice(s.arena)[ev.ci]
			cf.nextraargs = 0
			th.top = cf.base + int(p.MaxStackSize)
		}
		return coldContinue, pc, nil

	case bytecode.OP_VARARG:
		cf := &th.framesSlice(s.arena)[ev.ci]
		want := int(uint8(w>>bytecode.PosC)) - 1
		if w.K() {
			vtv := regs[uint8(w>>bytecode.PosB)]
			if !vtv.IsTable() {
				return coldContinue, pc, fmt.Errorf("vararg table expected")
			}
			vt := s.asTable(vtv)
			nv := vt.RawGetStr(s, s.intern.Intern("n"))
			if !nv.IsInt() || nv.Int() < 0 || nv.Int() > math.MaxInt32/2 {
				return coldContinue, pc, fmt.Errorf("vararg table has no proper 'n'")
			}
			n := int(nv.Int())
			open := want < 0
			if open {
				want = n
			}
			if ev.base+int(a)+want+4 > 1000000 {
				return coldContinue, pc, fmt.Errorf("stack overflow")
			}
			if err := th.ensureStack(s, ev.base+int(a)+want+4); err != nil {
				return coldContinue, pc, err
			}
			if open {
				th.top = ev.base + int(a) + want
			}
			cancelFn := s.nativeCancelFunc()
			st := th.stackSlice(s.arena)
			for i := 0; i < want; i++ {
				if cancelFn != nil && (i&1023) == 1023 {
					if cerr := cancelFn(); cerr != nil {
						return coldContinue, pc, cerr
					}
				}
				if i < n {
					st[ev.base+int(a)+i] = vt.RawGetInt(s, int64(i+1))
				} else {
					st[ev.base+int(a)+i] = NilValue()
				}
			}
			return coldContinue, pc, nil
		}
		nextra := cf.nextraargs
		if cf.hasVarargTab() || ev.base < nextra {
			return coldContinue, pc, fmt.Errorf("invalid vararg frame")
		}
		open := want < 0
		if open {
			want = nextra
		}
		if err := th.ensureStack(s, ev.base+int(a)+want+4); err != nil {
			return coldContinue, pc, err
		}
		if open {
			th.top = ev.base + int(a) + want
		}
		extraStart := ev.base - nextra
		cancelFn := s.nativeCancelFunc()
		st := th.stackSlice(s.arena)
		for i := 0; i < want; i++ {
			if cancelFn != nil && (i&1023) == 1023 {
				if cerr := cancelFn(); cerr != nil {
					return coldContinue, pc, cerr
				}
			}
			if i < nextra {
				st[ev.base+int(a)+i] = st[extraStart+i]
			} else {
				st[ev.base+int(a)+i] = NilValue()
			}
		}
		return coldContinue, pc, nil

	case bytecode.OP_GETVARG:
		cf := &th.framesSlice(s.arena)[ev.ci]
		if cf.hasVarargTab() || ev.base < cf.nextraargs {
			return coldContinue, pc, fmt.Errorf("invalid vararg frame")
		}
		rc := regs[uint8(w>>bytecode.PosC)]
		if idx, ok := rc.ToInt(); ok {
			if idx >= 1 && idx <= int64(cf.nextraargs) {
				regs[a] = th.stackSlice(s.arena)[ev.base-cf.nextraargs+int(idx)-1]
			} else {
				regs[a] = NilValue()
			}
		} else if rc.IsString() && s.asString(rc).Str() == "n" {
			regs[a] = IntValue(int64(cf.nextraargs))
		} else {
			regs[a] = NilValue()
		}
		return coldContinue, pc, nil

	case bytecode.OP_ERRNNIL:
		if !regs[a].IsNil() {
			bx := w.Bx()
			if bx > 0 && bx-1 < len(ev.consts) && ev.consts[bx-1].IsString() {
				return coldContinue, pc, fmt.Errorf("global '%s' already defined", s.asString(ev.consts[bx-1]).Str())
			}
			return coldContinue, pc, fmt.Errorf("global '?' already defined")
		}
		return coldContinue, pc, nil

	default:
		return coldContinue, pc, fmt.Errorf("unimplemented opcode %s (%d) at pc %d", bytecode.OpNames[op], op, ipc)
	}
}

func varInfo(p *bytecode.Proto, pc, reg int) string {
	if p == nil || reg < 0 || pc < 0 || pc >= len(p.Code) {
		return ""
	}
	name, kind := findObjectName(p, reg, pc)
	if kind == "" {
		return ""
	}
	return fmt.Sprintf(" (%s '%s')", kind, name)
}

// objTypeName returns Lua 5.5's human-readable type name, honoring mt.__name and "light userdata" (errors.lua:268-280).
func (s *State) objTypeName(v Value) string {
	if v.IsLightUD() {
		return "light userdata"
	}
	if mt := s.getMetatableOf(v); mt != nil {
		if nm := mt.RawGetStr(s, s.intern.Intern("__name")); nm.IsString() {
			return s.asString(nm).Str()
		}
	}
	return v.TypeName()
}

func (s *State) mmBinError(p *bytecode.Proto, pc int, tm TM, p1, p2 Value, r1, r2 int) error {
	bad := p1
	badReg := r1
	switch tm {
	case TMBand, TMBor, TMBxor, TMShl, TMShr, TMBnot:
		if p1.IsNumber() {
			if _, ok := p1.ToInt(); !ok {
				return fmt.Errorf("number%s has no integer representation", varInfo(p, pc, r1))
			}
			bad = p2
			badReg = r2
		}
		if bad.IsNumber() {
			if _, ok := bad.ToInt(); !ok {
				return fmt.Errorf("number%s has no integer representation", varInfo(p, pc, badReg))
			}
		}
		return fmt.Errorf("attempt to perform bitwise operation on a %s value%s", s.objTypeName(bad), varInfo(p, pc, badReg))
	default:
		if p1.IsNumber() {
			bad = p2
			badReg = r2
		}
		return fmt.Errorf("attempt to perform arithmetic on a %s value%s", s.objTypeName(bad), varInfo(p, pc, badReg))
	}
}

func (s *State) compareError(a, b Value) error {
	t1, t2 := s.objTypeName(a), s.objTypeName(b)
	if t1 == t2 {
		return fmt.Errorf("attempt to compare two %s values", t1)
	}
	return fmt.Errorf("attempt to compare %s with %s", t1, t2)
}

func (s *State) lessThan(a, b Value) (bool, error) {
	if a.IsString() && b.IsString() {
		cmp, err := s.compareStrings(s.asString(a), s.asString(b))
		if err != nil {
			return false, err
		}
		return cmp < 0, nil
	}
	if BothInt(a, b) {
		return a.Int() < b.Int(), nil
	}
	if BothFloat(a, b) {
		return a.Float() < b.Float(), nil
	}
	if a.IsInt() && b.IsFloat() {
		return LTIntFloat(a.Int(), b.Float()), nil
	}
	if a.IsFloat() && b.IsInt() {
		return LTFloatInt(a.Float(), b.Int()), nil
	}
	tm := s.getTMByObj(a, TMLt)
	if tm.IsNil() {
		tm = s.getTMByObj(b, TMLt)
	}
	if tm.IsNil() {
		if a.TypeName() == b.TypeName() {
			return false, fmt.Errorf("attempt to compare two %s values", a.TypeName())
		}
		return false, fmt.Errorf("attempt to compare %s with %s", a.TypeName(), b.TypeName())
	}
	res, err := s.Call(tm, a, b)
	if err != nil {
		return false, err
	}
	return len(res) > 0 && !res[0].IsFalsy(), nil
}

func (s *State) equalValues(a, b Value) (bool, error) {
	eq, err := s.rawEqual(a, b)
	if err != nil {
		return false, err
	}
	if eq {
		return true, nil
	}
	if (a.IsTable() && b.IsTable()) || (a.IsUserdata() && b.IsUserdata()) {
		tm := s.getTMByObj(a, TMEq)
		if tm.IsNil() {
			tm = s.getTMByObj(b, TMEq)
		}
		if !tm.IsNil() {
			res, err := s.Call(tm, a, b)
			if err != nil {
				return false, err
			}
			return len(res) > 0 && !res[0].IsFalsy(), nil
		}
	}
	return false, nil
}

func (s *State) concatYieldable(th *Thread, cf *callFrame, start, n int) (bool, error) {
	if n <= 1 {
		return false, nil
	}
	allSimple := true
	totalLen := 0
	var numStrs []string
	st := th.stackSlice(s.arena)
	for i := 0; i < n; i++ {
		v := st[start+i]
		if v.IsString() {
			totalLen += len(s.asString(v).Str())
		} else if v.IsNumber() {
			ns := s.FormatValue(v)
			totalLen += len(ns)
			numStrs = append(numStrs, ns)
		} else {
			allSimple = false
			break
		}
	}
	if allSimple {
		if totalLen > (1<<30) || (s.memLimit > 0 && int64(totalLen)+48 > s.memLimit-s.MemoryUsage()) {
			if err := s.checkMem(int64(totalLen) + 48); err != nil {
				return false, err
			}
			st = th.stackSlice(s.arena)
		}
		cancelFn := s.nativeCancelFunc()
		var sb strings.Builder
		if cancelFn == nil {
			sb.Grow(totalLen)
		} else {
			if totalLen > strCloneCancelChunk {
				if err := cancelFn(); err != nil {
					return false, err
				}
			}
			sb.Grow(min(totalLen, strCloneCancelChunk))
		}
		copyWork := 0
		ni := 0
		for i := 0; i < n; i++ {
			v := st[start+i]
			var part string
			if v.IsString() {
				part = s.asString(v).Str()
			} else {
				part = numStrs[ni]
				ni++
			}
			if err := writeStringWithCancel(&sb, part, cancelFn, &copyWork); err != nil {
				return false, err
			}
		}
		out := sb.String()
		ls, err := s.TryIntern(out)
		if err != nil {
			return false, err
		}
		th.stackSlice(s.arena)[start] = StringValue(ls)
		s.recordNewTableAlloc(int64(48+len(out)), start+1)
		if s.gcNeedsCheck() {
			s.gcCheckStep(start + 1)
			if s.abortErr != nil {
				return false, s.abortErr
			}
		}
		return false, nil
	}
	return s.concatStep(th, cf, start, start+n-1)
}

func (s *State) concatStep(th *Thread, cf *callFrame, start, cur int) (bool, error) {
	cfIdx := th.nframe - 1
	cancelFn := s.nativeCancelFunc()
	for cur > start {
		st := th.stackSlice(s.arena)
		lhs := st[cur-1]
		rhs := st[cur]
		if (lhs.IsString() || lhs.IsNumber()) && (rhs.IsString() || rhs.IsNumber()) {
			ls, rs := s.FormatValue(lhs), s.FormatValue(rhs)
			totalLen := len(ls) + len(rs)
			if err := s.checkPreallocMem(int64(totalLen) + 48); err != nil {
				return false, err
			}
			var joined string
			if cancelFn != nil && totalLen > strCloneCancelChunk {
				var sb strings.Builder
				work := 0
				if err := writeStringWithCancel(&sb, ls, cancelFn, &work); err != nil {
					return false, err
				}
				if err := writeStringWithCancel(&sb, rs, cancelFn, &work); err != nil {
					return false, err
				}
				joined = sb.String()
			} else {
				joined = ls + rs
			}
			interned, err := s.TryIntern(joined)
			if err != nil {
				return false, err
			}
			th.stackSlice(s.arena)[cur-1] = StringValue(interned)
			if s.gcNeedsCheck() {
				s.gcCheckStep(cur)
				if s.abortErr != nil {
					return false, s.abortErr
				}
			}
			cur--
			continue
		}
		tm := s.getTMByObj(lhs, TMConcat)
		if tm.IsNil() {
			tm = s.getTMByObj(rhs, TMConcat)
		}
		if tm.IsNil() {
			bad := lhs
			if lhs.IsString() || lhs.IsNumber() {
				bad = rhs
			}
			return false, fmt.Errorf("attempt to concatenate a %s value", bad.TypeName())
		}
		cargs := []Value{lhs, rhs}
		var extraDepth uint8
		if !tm.IsClosure() && !tm.IsNative() {
			curTM := tm
			for depth := 1; depth <= 15; depth++ {
				callTM := s.getTMByObj(curTM, TMCall)
				if callTM.IsNil() {
					if depth == 1 {
						return false, fmt.Errorf("attempt to call a %s value (metamethod 'concat')", curTM.TypeName())
					}
					return false, fmt.Errorf("attempt to call a %s value", curTM.TypeName())
				}
				nextArgs := make([]Value, 1+len(cargs))
				nextArgs[0] = curTM
				copy(nextArgs[1:], cargs)
				cargs = nextArgs
				curTM = callTM
				extraDepth = uint8(depth)
				if curTM.IsClosure() || curTM.IsNative() {
					break
				}
			}
			if !curTM.IsClosure() && !curTM.IsNative() {
				return false, fmt.Errorf("'__call' chain too long")
			}
			tm = curTM
		}
		cf = &th.framesSlice(s.arena)[cfIdx]
		slot := cf.base + int(s.boundProtoOf(s.frameClosure(cf)).proto.MaxStackSize)
		if th.top > slot {
			slot = th.top
		}
		if tm.IsClosure() {
			if err := s.ensureStackWithRoots(th, slot+1+len(cargs)+8, tm, cargs); err != nil {
				return false, err
			}
			st = th.stackSlice(s.arena)
			st[slot] = tm
			for i, ca := range cargs {
				st[slot+1+i] = ca
			}
			th.top = slot + 1 + len(cargs)
			cf = &th.framesSlice(s.arena)[cfIdx]
			cf.dirty = true
			cf.oldTop = slot
			cf.finish = finishConcat
			cf.concatStart = start
			cf.concatIdx = cur - 1
			cl := s.asClosure(tm)
			if err := th.pushLuaFrame(s, cl, slot, len(cargs), 1); err != nil {
				fillNil(th.stackSlice(s.arena)[slot : slot+1+len(cargs)])
				th.top = slot
				th.framesSlice(s.arena)[cfIdx].finish = finishNone
				return false, err
			}
			th.framesSlice(s.arena)[th.nframe-1].extraArgs = extraDepth
			if herr := s.fireLuaCallHook(th, cl, len(cargs)); herr != nil {
				return false, herr
			}
			return true, nil
		}
		if err := s.ensureStackWithRoots(th, slot+1+len(cargs)+8, tm, cargs); err != nil {
			return false, err
		}
		st = th.stackSlice(s.arena)
		st[slot] = tm
		copy(st[slot+1:slot+1+len(cargs)], cargs)
		res, _, err := s.callNativeHooked(th, tm, slot, len(cargs), extraDepth, maxFrames+128)
		cf = &th.framesSlice(s.arena)[cfIdx]
		if errors.Is(err, errYieldSentinel) {
			cf.dirty = true
			cf.oldTop = slot
			cf.finish = finishConcat
			cf.concatStart = start
			cf.concatIdx = cur - 1
			th.yieldDst = slot
			th.yieldNRes = 1
			th.yieldedNative = true
			th.yieldNativeFn = tm
			return false, errYieldSentinel
		}
		th.top = slot
		if err != nil {
			if !errors.Is(err, errCoroCloseSelf) {
				th.lastErrNative = tm
			}
			return false, err
		}
		if len(res) > 0 {
			th.stackSlice(s.arena)[cur-1] = res[0]
		} else {
			th.stackSlice(s.arena)[cur-1] = NilValue()
		}
		if s.gcNeedsCheck() {
			s.gcCheckStep(cur)
			if s.abortErr != nil {
				return false, s.abortErr
			}
		}
		cur--
	}
	return false, nil
}
