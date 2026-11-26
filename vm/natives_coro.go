package vm

import (
	"errors"
	"fmt"
)

func (s *State) initCoroLib() {
	coro := s.NewTable(0, 10)
	s.registerFunc(coro, "create", intrNone, nativeCoroCreate)
	s.registerFunc(coro, "resume", intrNone, nativeCoroResume)
	s.registerFunc(coro, "yield", intrNone, nativeCoroYield)
	if v := coro.RawGetStr(s.intern.Intern("yield")); v.IsNative() {
		s.builtinCoroYield = v.AsNative()
	}
	s.registerFunc(coro, "status", intrNone, nativeCoroStatus)
	s.registerFunc(coro, "running", intrNone, nativeCoroRunning)
	s.registerFunc(coro, "isyieldable", intrNone, nativeCoroIsYieldable)
	s.registerFunc(coro, "wrap", intrNone, nativeCoroWrap)
	s.registerFunc(coro, "close", intrNone, nativeCoroClose)
	s.SetGlobal("coroutine", TableValue(coro))
}

// NewThread creates a new suspended coroutine ready to run fn (matching coroutine.create).
func (s *State) NewThread(fn Value) (*Thread, error) {
	if !fn.IsFunction() {
		return nil, fmt.Errorf("bad argument #1 to 'NewThread' (function expected)")
	}
	if s.memLimit > 0 || s.gc.tier1 {
		if err := s.checkMemAndAccountWithRoots(threadInitMemBytes(128, defaultFrameCap), fn, NilValue(), NilValue()); err != nil {
			return nil, err
		}
	}
	co := newThread(s, 128)
	co.stack[0] = fn
	co.top = 1
	return co, nil
}

// Resume resumes co with args. It returns (results, yielded, err):
//   - yielded == true, err == nil: co suspended via coroutine.yield(results...)
//   - yielded == false, err == nil: co returned normally with results
//   - err != nil: co errored (LuaError) or aborted via uncatchable cancellation/limit.
func (s *State) Resume(co *Thread, args ...Value) ([]Value, bool, error) {
	if co == nil {
		return nil, false, fmt.Errorf("cannot resume nil thread")
	}
	if co.state != s {
		return nil, false, fmt.Errorf("cannot resume thread belonging to another State")
	}
	if s.nCcalls == 0 {
		s.abortErr = nil
	}
	ok, res, uerr := s.resumeThread(co, args)
	if uerr == nil && s.abortErr != nil {
		uerr = s.abortErr
	}
	if s.nCcalls == 0 {
		s.abortErr = nil
	}
	if uerr != nil {
		return nil, false, uerr
	}
	if !ok {
		if len(res) > 0 {
			return nil, false, &LuaError{Val: res[0]}
		}
		return nil, false, fmt.Errorf("coroutine error")
	}
	return res, co.status == ThreadSuspended, nil
}

// CloseThread closes a suspended or dead coroutine, running all pending <close>
// metamethods (matching coroutine.close).
func (s *State) CloseThread(co *Thread) error {
	if co == nil {
		return fmt.Errorf("cannot close nil thread")
	}
	if co.state != s {
		return fmt.Errorf("cannot close thread belonging to another State")
	}
	switch co.StatusString() {
	case "normal":
		return fmt.Errorf("cannot close a normal coroutine")
	case "running":
		if co == s.mainThread {
			return fmt.Errorf("cannot close main thread")
		}
		return fmt.Errorf("cannot close a running coroutine from Go")
	case "dead", "suspended":
		if s.nCcalls == 0 {
			s.abortErr = nil
		}
		co.closing = true
		co.nframe = 0
		co.nny++
		hasInitErr := co.status == ThreadDead && !co.errObj.IsNil()
		errVal := co.errObj
		errVal, hasErr, _ := co.closeAllSync(0, errVal, hasInitErr)
		co.nny--
		co.closing = false
		co.status = ThreadDead
		co.clearDeadRefs()
		co.errObj = NilValue()
		if uerr := s.abortErr; uerr != nil {
			if s.nCcalls == 0 {
				s.abortErr = nil
			}
			return uerr
		}
		if hasErr {
			return &LuaError{Val: errVal}
		}
		return nil
	default:
		return nil
	}
}

func nativeCoroCreate(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsFunction() {
		got := "no value"
		if len(args) >= 1 {
			got = s.objTypeName(args[0])
		}
		return nil, fmt.Errorf("bad argument #1 to 'create' (function expected, got %s)", got)
	}
	if s.memLimit > 0 || s.gc.tier1 {
		if err := s.checkMemAndAccountWithRoots(threadInitMemBytes(128, defaultFrameCap), args[0], NilValue(), NilValue()); err != nil {
			return nil, err
		}
	}
	co := newThread(s, 128)
	co.stack[0] = args[0]
	co.top = 1
	return []Value{ThreadValue(co)}, nil
}

func (s *State) resumeThread(co *Thread, args []Value) (bool, []Value, error) {
	if co == s.cur || co.closing {
		return false, []Value{StringValue(s.intern.Intern("cannot resume non-suspended coroutine"))}, nil
	}
	if co.status == ThreadDead {
		return false, []Value{StringValue(s.intern.Intern("cannot resume dead coroutine"))}, nil
	}
	if co.status == ThreadNormal || (co.status == ThreadOK && co.nframe > 0) {
		return false, []Value{StringValue(s.intern.Intern("cannot resume non-suspended coroutine"))}, nil
	}
	if s.nCcalls >= maxCCalls {
		return false, []Value{StringValue(s.intern.Intern("C stack overflow"))}, nil
	}
	if s.ctx != nil {
		if err := s.ctx.Err(); err != nil {
			return false, nil, err
		}
	}

	parent := s.cur
	parentStatus := parent.status
	parent.status = ThreadNormal
	co.caller = parent
	co.lastErrNative = NilValue()
	s.cur = co
	s.nCcalls++
	defer func() {
		s.nCcalls--
		co.caller = nil
		s.cur = parent
		parent.status = parentStatus
	}()

	if co.status == ThreadOK && co.nframe == 0 {
		// First resume of a freshly created coroutine!
		fn := co.stack[0]
		if fn.IsNil() {
			co.status = ThreadDead
			co.clearDeadRefs()
			return false, []Value{StringValue(s.intern.Intern("cannot resume dead coroutine"))}, nil
		}
		if fn.IsNative() {
			nf := fn.AsNative()
			if nf.Intrinsic == intrPCall && len(args) >= 1 && isFlatProtectedTarget(args[0]) {
				if err := s.ensureStackWithRoots(co, len(args)+8, NilValue(), args); err != nil {
					co.status = ThreadDead
					ev := s.runtimeErrorToValue(co, err)
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				co.stack[0] = fn
				for i, a := range args {
					co.stack[1+i] = a
				}
				co.top = 1 + len(args)
				if _, err := s.pushProtectedCall(co, 0, len(args), -1, false, 0); err != nil {
					if errors.Is(err, errYieldSentinel) {
						co.status = ThreadSuspended
						co.detachOpenUpvals()
						out := co.yieldVals
						co.yieldVals = nil
						return true, out, nil
					}
					if errors.Is(err, errCoroCloseSelf) || errors.Is(s.abortErr, errCoroCloseSelf) {
						return s.finishCoroCloseSelf(co)
					}
					co.status = ThreadDead
					if isUncatchableErr(err) {
						return false, nil, err
					}
					ev := s.runtimeErrorToValue(co, err)
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				goto runCoro
			}
			if nf.Intrinsic == intrXPCall && len(args) >= 2 && isFlatProtectedTarget(args[0]) && args[1].IsFunction() {
				if err := s.ensureStackWithRoots(co, len(args)+8, NilValue(), args); err != nil {
					co.status = ThreadDead
					ev := s.runtimeErrorToValue(co, err)
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				co.stack[0] = fn
				for i, a := range args {
					co.stack[1+i] = a
				}
				co.top = 1 + len(args)
				if _, err := s.pushProtectedCall(co, 0, len(args), -1, true, 0); err != nil {
					if errors.Is(err, errYieldSentinel) {
						co.status = ThreadSuspended
						co.detachOpenUpvals()
						out := co.yieldVals
						co.yieldVals = nil
						return true, out, nil
					}
					if errors.Is(err, errCoroCloseSelf) || errors.Is(s.abortErr, errCoroCloseSelf) {
						return s.finishCoroCloseSelf(co)
					}
					co.status = ThreadDead
					if isUncatchableErr(err) {
						return false, nil, err
					}
					ev := s.runtimeErrorToValue(co, err)
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				goto runCoro
			}
			if nf.Intrinsic == intrDoFile {
				loaded, lerr := nativeLoadFile(s, dofileArgs(args))
				if lerr != nil || len(loaded) == 0 || loaded[0].IsNil() {
					co.status = ThreadDead
					co.stack[0] = NilValue()
					var ev Value
					if lerr != nil {
						ev = s.runtimeErrorToValue(co, lerr)
					} else if len(loaded) > 1 {
						ev = loaded[1]
					} else {
						ev = StringValue(s.intern.Intern("dofile failed"))
					}
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				cl := loaded[0].AsClosure()
				co.stack[0] = loaded[0]
				co.top = 1
				if err := co.pushLuaFrame(cl, 0, 0, -1); err != nil {
					co.status = ThreadDead
					ev := s.runtimeErrorToValue(co, err)
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				goto runCoro
			}
			if nf.Intrinsic == intrPairs && len(args) >= 1 {
				tm, cargs, tmExtra, hasTM, terr := s.resolvePairsTM(args[0])
				if terr != nil {
					co.status = ThreadDead
					co.stack[0] = NilValue()
					ev := s.runtimeErrorToValue(co, terr)
					co.clearUnusedRefs()
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				if hasTM {
					if err := s.ensureStackWithRoots(co, 1+len(cargs)+8, tm, cargs); err != nil {
						co.status = ThreadDead
						ev := s.runtimeErrorToValue(co, err)
						co.clearUnusedRefs()
						co.errObj = ev
						return false, []Value{ev}, nil
					}
					co.stack[0] = tm
					copy(co.stack[1:1+len(cargs)], cargs)
					co.top = 1 + len(cargs)
					if tm.IsClosure() {
						cl := tm.AsClosure()
						if err := co.pushLuaFrame(cl, 0, len(cargs), 4); err != nil {
							co.status = ThreadDead
							ev := s.runtimeErrorToValue(co, err)
							co.clearUnusedRefs()
							co.errObj = ev
							return false, []Value{ev}, nil
						}
						co.frames[co.nframe-1].extraArgs = tmExtra
						if herr := s.fireLuaCallHook(co, cl, len(cargs)); herr != nil {
							if errors.Is(herr, errCoroCloseSelf) {
								return s.finishCoroCloseSelf(co)
							}
							if isUncatchableErr(herr) {
								co.status = ThreadDead
								co.clearDeadRefs()
								return false, nil, herr
							}
							co.status = ThreadDead
							ev := s.runtimeErrorToValue(co, herr)
							co.clearUnusedRefs()
							co.errObj = ev
							return false, []Value{ev}, nil
						}
						goto runCoro
					}
					res, _, err := s.callNativeHooked(co, tm, 0, len(cargs), tmExtra, maxFrames+128)
					if errors.Is(err, errYieldSentinel) {
						co.status = ThreadSuspended
						co.detachOpenUpvals()
						co.yieldDst = 0
						co.yieldNRes = 4
						co.yieldedNative = true
						co.yieldNativeFn = tm
						out := co.yieldVals
						co.yieldVals = nil
						return true, out, nil
					}
					co.status = ThreadDead
					if err != nil {
						if errors.Is(err, errCoroCloseSelf) || errors.Is(s.abortErr, errCoroCloseSelf) {
							return s.finishCoroCloseSelf(co)
						}
						if isUncatchableErr(err) {
							co.discardTBCAtOrAbove(0)
							co.closeUpvals(0)
							co.clearDeadRefs()
							return false, nil, err
						}
						co.stack[0] = NilValue()
						ev := s.runtimeErrorToValue(co, err)
						co.clearUnusedRefs()
						co.errObj = ev
						return false, []Value{ev}, nil
					}
					out := []Value{NilValue(), NilValue(), NilValue(), NilValue()}
					copy(out, res)
					co.clearDeadRefs()
					return true, out, nil
				}
			}
			if err := s.ensureStackWithRoots(co, 1+len(args)+8, NilValue(), args); err != nil {
				co.status = ThreadDead
				ev := s.runtimeErrorToValue(co, err)
				co.errObj = ev
				return false, []Value{ev}, nil
			}
			co.stack[0] = fn
			copy(co.stack[1:1+len(args)], args)
			res, _, err := s.callNativeHooked(co, fn, 0, len(args), 0, maxFrames+128)
			if errors.Is(err, errYieldSentinel) {
				co.status = ThreadSuspended
				co.detachOpenUpvals()
				co.yieldDst = 0
				co.yieldNRes = -1 // MULTRET: resume args become coroutine return values
				co.yieldedNative = true
				co.yieldNativeFn = fn
				out := co.yieldVals
				co.yieldVals = nil
				return true, out, nil
			}
			co.status = ThreadDead
			if err != nil {
				if errors.Is(err, errCoroCloseSelf) || errors.Is(s.abortErr, errCoroCloseSelf) {
					return s.finishCoroCloseSelf(co)
				}
				if isUncatchableErr(err) {
					co.discardTBCAtOrAbove(0)
					co.closeUpvals(0)
					co.clearDeadRefs()
					return false, nil, err
				}
				co.stack[0] = NilValue()
				ev := s.runtimeErrorToValue(co, err)
				co.clearUnusedRefs()
				co.errObj = ev
				return false, []Value{ev}, nil
			}
			if len(res) > 0 && slicesOverlapValues(res, co.stack) {
				cp := make([]Value, len(res))
				copy(cp, res)
				res = cp
			}
			co.clearDeadRefs()
			return true, res, nil
		}
		if !fn.IsClosure() {
			co.status = ThreadDead
			co.clearDeadRefs()
			return false, []Value{StringValue(s.intern.Intern("cannot resume non-function coroutine"))}, nil
		}
		cl := fn.AsClosure()
		if err := s.ensureStackWithRoots(co, 1+len(args)+int(cl.bp.proto.MaxStackSize)+16, NilValue(), args); err != nil {
			co.status = ThreadDead
			ev := s.runtimeErrorToValue(co, err)
			co.errObj = ev
			return false, []Value{ev}, nil
		}
		for i, a := range args {
			co.stack[1+i] = a
		}
		co.top = 1 + len(args)
		if err := co.pushLuaFrame(cl, 0, len(args), -1); err != nil {
			co.status = ThreadDead
			ev := s.runtimeErrorToValue(co, err)
			co.errObj = ev
			return false, []Value{ev}, nil
		}
		if co.hookMask&HookMaskCall != 0 && !co.inHook && !co.suppressHook {
			if herr := s.fireHook(co, "call", NilValue()); herr != nil {
				if errors.Is(herr, errCoroCloseSelf) {
					return s.finishCoroCloseSelf(co)
				}
				if isUncatchableErr(herr) {
					co.status = ThreadDead
					co.discardTBCAtOrAbove(0)
					co.closeUpvals(0)
					co.clearDeadRefs()
					return false, nil, herr
				}
				if !s.handleError(co, 0, herr) {
					if errors.Is(s.abortErr, errCoroCloseSelf) {
						return s.finishCoroCloseSelf(co)
					}
					if isUncatchableErr(s.abortErr) {
						uerr := s.abortErr
						co.status = ThreadDead
						co.discardTBCAtOrAbove(0)
						co.closeUpvals(0)
						co.clearDeadRefs()
						return false, nil, uerr
					}
					co.status = ThreadDead
					ev := s.runtimeErrorToValue(co, herr)
					co.clearUnusedRefs()
					co.errObj = ev
					return false, []Value{ev}, nil
				}
			}
		}
	} else if co.status == ThreadSuspended {
		// Resuming a yielded coroutine: re-attach detached open upvalues and deliver resume args!
		co.status = ThreadOK
		co.attachOpenUpvals()
		yieldedNative := co.yieldedNative
		yieldNativeFn := co.yieldNativeFn
		co.yieldedNative = false
		co.yieldNativeFn = NilValue()
		if yieldedNative && co.hookMask&HookMaskRet != 0 && !co.inHook && !co.suppressHook {
			dst := co.yieldDst
			var herr error
			if ferr := s.ensureFrameCapWithRoots(co, co.nframe+1, maxFrames+128, yieldNativeFn, args); ferr != nil {
				herr = ferr
			} else if serr := s.ensureStackWithRoots(co, dst+1+len(args)+4, yieldNativeFn, args); serr != nil {
				herr = serr
			} else {
				if !yieldNativeFn.IsNil() {
					co.stack[dst] = yieldNativeFn
				}
				if len(args) > 0 {
					copy(co.stack[dst+1:dst+1+len(args)], args)
				}
				if end := dst + 1 + len(args); co.top < end {
					co.top = end
				}
				co.frames[co.nframe] = callFrame{
					kind:      frameNative,
					base:      dst + 1,
					funcIdx:   dst,
					nativeFn:  yieldNativeFn,
					ftransfer: 1,
					ntransfer: uint16(len(args)),
					dirty:     true,
				}
				co.nframe++
				herr = s.fireHook(co, "return", NilValue())
				if herr == nil && len(args) > 0 {
					updated := make([]Value, len(args))
					copy(updated, co.stack[dst+1:dst+1+len(args)])
					args = updated
				}
				co.nframe--
				if len(args) > 0 {
					fillNil(co.stack[dst+1 : dst+1+len(args)])
				}
				if co.yieldNRes == 0 {
					co.stack[dst] = NilValue()
				}
			}
			if herr != nil {
				if errors.Is(herr, errCoroCloseSelf) {
					return s.finishCoroCloseSelf(co)
				}
				if isUncatchableErr(herr) {
					co.status = ThreadDead
					co.discardTBCAtOrAbove(0)
					co.closeUpvals(0)
					co.clearDeadRefs()
					return false, nil, herr
				}
				if !s.handleError(co, 0, herr) {
					if errors.Is(s.abortErr, errCoroCloseSelf) {
						return s.finishCoroCloseSelf(co)
					}
					if isUncatchableErr(s.abortErr) {
						uerr := s.abortErr
						co.status = ThreadDead
						co.discardTBCAtOrAbove(0)
						co.closeUpvals(0)
						co.clearDeadRefs()
						return false, nil, uerr
					}
					co.status = ThreadDead
					ev := s.runtimeErrorToValue(co, herr)
					co.clearUnusedRefs()
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				goto runCoro
			}
		}
		needSlots := len(args)
		if co.yieldNRes > needSlots {
			needSlots = co.yieldNRes
		}
		if err := s.ensureStackWithRoots(co, co.yieldDst+needSlots+4, NilValue(), args); err != nil {
			if !s.handleError(co, 0, err) {
				if errors.Is(s.abortErr, errCoroCloseSelf) {
					return s.finishCoroCloseSelf(co)
				}
				if isUncatchableErr(s.abortErr) {
					uerr := s.abortErr
					co.status = ThreadDead
					co.discardTBCAtOrAbove(0)
					co.closeUpvals(0)
					co.clearDeadRefs()
					return false, nil, uerr
				}
				co.status = ThreadDead
				ev := s.runtimeErrorToValue(co, err)
				co.clearUnusedRefs()
				co.errObj = ev
				return false, []Value{ev}, nil
			}
			goto runCoro
		}
		if err := co.placeResults(co.yieldDst, args, co.yieldNRes); err != nil {
			if !s.handleError(co, 0, err) {
				if errors.Is(s.abortErr, errCoroCloseSelf) {
					return s.finishCoroCloseSelf(co)
				}
				if isUncatchableErr(s.abortErr) {
					uerr := s.abortErr
					co.status = ThreadDead
					co.discardTBCAtOrAbove(0)
					co.closeUpvals(0)
					co.clearDeadRefs()
					return false, nil, uerr
				}
				co.status = ThreadDead
				ev := s.runtimeErrorToValue(co, err)
				co.clearUnusedRefs()
				co.errObj = ev
				return false, []Value{ev}, nil
			}
			goto runCoro
		}
		// If yielded from a native metamethod/TBC __close (e.g. func2close(coroutine.yield)), complete continuation!
		for co.nframe > 0 && co.frames[co.nframe-1].finish != finishNone {
			cf := &co.frames[co.nframe-1]
			cont, cerr := s.completeFinishOp(co, cf, 0)
			if errors.Is(cerr, errYieldSentinel) {
				co.status = ThreadSuspended
				co.detachOpenUpvals()
				out := co.yieldVals
				co.yieldVals = nil
				return true, out, nil
			}
			if cerr != nil {
				if errors.Is(cerr, errCoroCloseSelf) {
					return s.finishCoroCloseSelf(co)
				}
				if isUncatchableErr(cerr) {
					co.status = ThreadDead
					co.discardTBCAtOrAbove(0)
					co.closeUpvals(0)
					co.clearDeadRefs()
					return false, nil, cerr
				}
				if !s.handleError(co, 0, cerr) {
					if errors.Is(s.abortErr, errCoroCloseSelf) {
						return s.finishCoroCloseSelf(co)
					}
					if isUncatchableErr(s.abortErr) {
						uerr := s.abortErr
						co.status = ThreadDead
						co.discardTBCAtOrAbove(0)
						co.closeUpvals(0)
						co.clearDeadRefs()
						return false, nil, uerr
					}
					co.status = ThreadDead
					ev := s.runtimeErrorToValue(co, cerr)
					co.clearUnusedRefs()
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				break
			}
			if cont || co.nframe == 0 {
				break
			}
		}
		if co.nframe == 0 {
			// Completed final OP_RETURN after yielding in __close!
			co.closeAllSync(0, NilValue(), false)
			co.status = ThreadDead
			nres := co.top
			if nres <= 0 {
				co.clearDeadRefs()
				return true, nil, nil
			}
			res := make([]Value, nres)
			copy(res, co.stack[:nres])
			co.clearDeadRefs()
			return true, res, nil
		}
	}

runCoro:
	err := s.runThread(co, 0)
	if co.uncaughtErr != nil {
		err = co.uncaughtErr
		co.uncaughtErr = nil
	}
	if errors.Is(err, errYieldSentinel) {
		co.status = ThreadSuspended
		co.detachOpenUpvals()
		out := co.yieldVals
		co.yieldVals = nil
		return true, out, nil
	}
	if errors.Is(err, errCoroCloseSelf) || errors.Is(s.abortErr, errCoroCloseSelf) {
		return s.finishCoroCloseSelf(co)
	}
	if err != nil {
		if isUncatchableErr(err) {
			co.status = ThreadDead
			co.discardTBCAtOrAbove(0)
			co.closeUpvals(0)
			co.clearDeadRefs()
			return false, nil, err
		}
		// Per Lua 5.5 spec, error in coroutine does NOT automatically close TBC vars until coroutine.close().
		ev := s.runtimeErrorToValue(co, err)
		co.status = ThreadDead
		co.clearUnusedRefs()
		co.errObj = ev
		return false, []Value{ev}, nil
	}
	// Normal completion: close any remaining TBC vars and mark dead.
	co.closeAllSync(0, NilValue(), false)
	co.status = ThreadDead
	nres := co.top
	if nres <= 0 {
		co.clearDeadRefs()
		return true, nil, nil
	}
	res := make([]Value, nres)
	copy(res, co.stack[:nres])
	co.clearDeadRefs()
	return true, res, nil
}

func (s *State) finishCoroCloseSelf(co *Thread) (bool, []Value, error) {
	if errors.Is(s.abortErr, errCoroCloseSelf) {
		s.abortErr = nil
	}
	// Coroutine closed itself via coroutine.close(): run TBC finalizers (non-yieldable) and terminate!
	co.closing = true
	co.nframe = 0
	co.nny++
	errVal, hasErr, _ := co.closeAllSync(0, NilValue(), false)
	co.nny--
	co.closing = false
	co.status = ThreadDead
	co.clearDeadRefs()
	if hasErr {
		co.errObj = errVal
		return false, []Value{errVal}, nil
	}
	co.errObj = NilValue()
	return true, nil, nil
}

func nativeCoroResume(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsThread() {
		got := "no value"
		if len(args) >= 1 {
			got = s.objTypeName(args[0])
		}
		return nil, fmt.Errorf("bad argument #1 to 'resume' (thread expected, got %s)", got)
	}
	co := args[0].AsThread()
	ok, res, uerr := s.resumeThread(co, args[1:])
	if uerr != nil {
		return nil, uerr
	}
	out := make([]Value, 1+len(res))
	out[0] = BoolValue(ok)
	copy(out[1:], res)
	return out, nil
}

func nativeCoroYield(s *State, args []Value) ([]Value, error) {
	th := s.cur
	if !th.IsYieldable() {
		if th == s.mainThread {
			return nil, &LuaError{Val: StringValue(s.intern.Intern("attempt to yield from outside a coroutine"))}
		}
		return nil, &LuaError{Val: StringValue(s.intern.Intern("attempt to yield across a C-call boundary"))}
	}
	yv := make([]Value, len(args))
	copy(yv, args)
	th.yieldVals = yv
	return nil, errYieldSentinel
}

func nativeCoroStatus(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsThread() {
		got := "no value"
		if len(args) >= 1 {
			got = s.objTypeName(args[0])
		}
		return nil, fmt.Errorf("bad argument #1 to 'status' (thread expected, got %s)", got)
	}
	return []Value{StringValue(s.intern.Intern(args[0].AsThread().StatusString()))}, nil
}

func nativeCoroRunning(s *State, _ []Value) ([]Value, error) {
	isMain := s.cur == s.mainThread
	return []Value{ThreadValue(s.cur), BoolValue(isMain)}, nil
}

func nativeCoroIsYieldable(s *State, args []Value) ([]Value, error) {
	th := s.cur
	if len(args) >= 1 && !args[0].IsNil() {
		if !args[0].IsThread() {
			return nil, fmt.Errorf("bad argument #1 to 'isyieldable' (thread expected, got %s)", s.objTypeName(args[0]))
		}
		th = args[0].AsThread()
	}
	return []Value{BoolValue(th.IsYieldable())}, nil
}

func nativeCoroWrap(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsFunction() {
		got := "no value"
		if len(args) >= 1 {
			got = s.objTypeName(args[0])
		}
		return nil, fmt.Errorf("bad argument #1 to 'wrap' (function expected, got %s)", got)
	}
	if s.memLimit > 0 || s.gc.tier1 {
		if err := s.checkMemAndAccount(threadInitMemBytes(128, defaultFrameCap) + 48 + 16); err != nil {
			return nil, err
		}
	}
	co := newThread(s, 128)
	co.stack[0] = args[0]
	co.top = 1
	var wrapper *NativeFunc
	wrapper = &NativeFunc{
		Name:        "coroutine.wrap",
		NumUpvals:   1,
		Collectable: true,
		upvals:      []Value{ThreadValue(co)},
		Fn: func(s *State, callArgs []Value) ([]Value, error) {
			if !wrapper.upvals[0].IsThread() {
				return nil, fmt.Errorf("thread expected, got %s", s.objTypeName(wrapper.upvals[0]))
			}
			co := wrapper.upvals[0].AsThread()
			wasActive := co == s.cur || co.status == ThreadNormal
			ok, res, uerr := s.resumeThread(co, callArgs)
			if uerr != nil {
				return nil, uerr
			}
			if !ok {
				var errVal Value
				if len(res) > 0 {
					errVal = res[0]
				} else {
					errVal = StringValue(s.intern.Intern("error in wrapped coroutine"))
				}
				if !wasActive {
					// Per Lua 5.5 spec, wrap closes the coroutine on error (passing original error to __close)
					// and propagates the final error (including any new error raised by __close, locals.lua:1070)!
					co.closing = true
					co.nframe = 0
					errVal, _, _ = co.closeAllSync(0, errVal, true)
					co.closing = false
					co.status = ThreadDead
					co.clearDeadRefs()
					co.errObj = NilValue()
				}
				// Match Lua 5.5 lcorolib.c:luaB_auxwrap: prepend caller location to string error objects.
				if errVal.IsString() {
					if where := s.errorWhere(1); where != "" {
						if ls, ierr := s.TryIntern(where + errVal.AsString().S); ierr == nil {
							errVal = StringValue(ls)
						} else {
							return nil, ierr
						}
					}
				}
				return nil, &LuaError{Val: errVal}
			}
			return res, nil
		},
	}
	return []Value{NativeValue(wrapper)}, nil
}

func nativeCoroClose(s *State, args []Value) ([]Value, error) {
	co := s.cur
	if len(args) >= 1 && !args[0].IsNil() {
		if !args[0].IsThread() {
			return nil, fmt.Errorf("bad argument #1 to 'close' (thread expected, got %s)", s.objTypeName(args[0]))
		}
		co = args[0].AsThread()
	}
	status := co.StatusString()
	switch status {
	case "normal":
		return nil, fmt.Errorf("cannot close a normal coroutine")
	case "running":
		if co == s.mainThread {
			return nil, fmt.Errorf("cannot close main thread")
		}
		// Closing self: abort coroutine execution immediately via uncatchable sentinel!
		return nil, errCoroCloseSelf
	case "dead", "suspended":
		co.closing = true
		co.nframe = 0
		co.nny++
		hasInitErr := co.status == ThreadDead && !co.errObj.IsNil()
		errVal := co.errObj
		errVal, hasErr, _ := co.closeAllSync(0, errVal, hasInitErr)
		co.nny--
		co.closing = false
		co.status = ThreadDead
		co.clearDeadRefs()
		co.errObj = NilValue()
		if s.abortErr != nil {
			return nil, s.abortErr
		}
		if hasErr {
			return []Value{FalseValue(), errVal}, nil
		}
		return []Value{TrueValue()}, nil
	default:
		return []Value{TrueValue()}, nil
	}
}
