package vm

import (
	"errors"
	"fmt"
)

func (s *State) initCoroLib() *Table {
	coro := s.newLibTable(0, 10)
	if coro == nil {
		return nil
	}
	pin := s.pinRuntime(TableValue(coro))
	defer s.unpinRuntime(pin)
	s.registerFunc(coro, "create", intrNone, nativeCoroCreate)
	s.registerFunc(coro, "resume", intrNone, nativeCoroResume)
	yieldFn := s.registerFunc(coro, "yield", intrNone, nativeCoroYield)
	s.registerFunc(coro, "status", intrNone, nativeCoroStatus)
	s.registerFunc(coro, "running", intrNone, nativeCoroRunning)
	s.registerFunc(coro, "isyieldable", intrNone, nativeCoroIsYieldable)
	s.registerFunc(coro, "wrap", intrNone, nativeCoroWrap)
	s.registerFunc(coro, "close", intrNone, nativeCoroClose)
	if s.memExceeded {
		return nil
	}
	s.builtinCoroYield = yieldFn
	return coro
}

// NewThread creates a new suspended coroutine ready to run fn (matching coroutine.create).
func (s *State) NewThread(fn Value) (*Thread, error) {
	if !fn.IsFunction() {
		return nil, fmt.Errorf("bad argument #1 to 'NewThread' (function expected)")
	}
	if err := s.checkPreallocMemWithRoots(threadInitMemBytes(128, defaultFrameCap), fn, NilValue(), NilValue()); err != nil {
		return nil, err
	}
	co, err := s.newThread(128, defaultFrameCap)
	if err != nil {
		return nil, s.memError()
	}
	co.stackSlice(s.arena)[0] = fn
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
	if !s.ownsThread(co) {
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
			return nil, false, s.materializeLuaError(&LuaError{Val: res[0]})
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
	if !s.ownsThread(co) {
		return fmt.Errorf("cannot close thread belonging to another State")
	}
	switch co.StatusString(s) {
	case "normal":
		return fmt.Errorf("cannot close a normal coroutine")
	case "running":
		if s.isMainThread(co) {
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
		errVal, hasErr, _ := co.closeAllSync(s, 0, errVal, hasInitErr)
		co.nny--
		co.closing = false
		co.status = ThreadDead
		co.clearDeadRefs(s)
		co.errObj = NilValue()
		if uerr := s.abortErr; uerr != nil {
			if s.nCcalls == 0 {
				s.abortErr = nil
			}
			return uerr
		}
		if hasErr {
			return s.materializeLuaError(&LuaError{Val: errVal})
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
	if err := s.checkPreallocMemWithRoots(threadInitMemBytes(128, defaultFrameCap), args[0], NilValue(), NilValue()); err != nil {
		return nil, err
	}
	co, err := s.newThread(128, defaultFrameCap)
	if err != nil {
		return nil, s.memError()
	}
	co.stackSlice(s.arena)[0] = args[0]
	co.top = 1
	return []Value{ThreadValue(co)}, nil
}

func (s *State) takeYieldVals(co *Thread) []Value {
	co.onSuspend()
	yv := co.yieldValsSlice(s.arena)
	if len(yv) == 0 {
		return nil
	}
	out := make([]Value, len(yv))
	copy(out, yv)
	co.clearYieldVals()
	return out
}

func (s *State) resumeThread(co *Thread, args []Value) (bool, []Value, error) {
	if co == s.cur() || co.closing {
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

	parent := s.cur()
	parentStatus := parent.status
	parent.status = ThreadNormal
	co.setCallerThread(s, parent)
	co.lastErrNative = NilValue()
	s.setCur(co)
	s.nCcalls++
	defer func() {
		s.nCcalls--
		co.setCallerThread(s, nil)
		s.setCur(parent)
		parent.status = parentStatus
	}()

	if co.status == ThreadOK && co.nframe == 0 {
		stack := co.stackSlice(s.arena)
		fn := NilValue()
		if len(stack) > 0 {
			fn = stack[0]
		}
		if fn.IsNil() {
			co.status = ThreadDead
			co.clearDeadRefs(s)
			return false, []Value{StringValue(s.intern.Intern("cannot resume dead coroutine"))}, nil
		}
		if fn.IsNative() {
			nf := s.asNative(fn)
			if nf.Intrinsic == intrPCall && len(args) >= 1 && isFlatProtectedTarget(args[0]) {
				if err := s.ensureStackWithRoots(co, len(args)+8, NilValue(), args); err != nil {
					co.status = ThreadDead
					ev := s.runtimeErrorToValue(co, err)
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				st := co.stackSlice(s.arena)
				st[0] = fn
				copy(st[1:1+len(args)], args)
				co.top = 1 + len(args)
				if _, err := s.pushProtectedCall(co, 0, len(args), -1, false, 0); err != nil {
					if errors.Is(err, errYieldSentinel) {
						co.status = ThreadSuspended
						return true, s.takeYieldVals(co), nil
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
				st := co.stackSlice(s.arena)
				st[0] = fn
				copy(st[1:1+len(args)], args)
				co.top = 1 + len(args)
				if _, err := s.pushProtectedCall(co, 0, len(args), -1, true, 0); err != nil {
					if errors.Is(err, errYieldSentinel) {
						co.status = ThreadSuspended
						return true, s.takeYieldVals(co), nil
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
					co.stackSlice(s.arena)[0] = NilValue()
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
				cl := s.asClosure(loaded[0])
				co.stackSlice(s.arena)[0] = loaded[0]
				co.top = 1
				if err := co.pushLuaFrame(s, cl, 0, 0, -1); err != nil {
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
					co.stackSlice(s.arena)[0] = NilValue()
					ev := s.runtimeErrorToValue(co, terr)
					co.clearUnusedRefs(s)
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				if hasTM {
					if err := s.ensureStackWithRoots(co, 1+len(cargs)+8, tm, cargs); err != nil {
						co.status = ThreadDead
						ev := s.runtimeErrorToValue(co, err)
						co.clearUnusedRefs(s)
						co.errObj = ev
						return false, []Value{ev}, nil
					}
					st := co.stackSlice(s.arena)
					st[0] = tm
					copy(st[1:1+len(cargs)], cargs)
					co.top = 1 + len(cargs)
					if tm.IsClosure() {
						cl := s.asClosure(tm)
						if err := co.pushLuaFrame(s, cl, 0, len(cargs), 4); err != nil {
							co.status = ThreadDead
							ev := s.runtimeErrorToValue(co, err)
							co.clearUnusedRefs(s)
							co.errObj = ev
							return false, []Value{ev}, nil
						}
						co.framesSlice(s.arena)[co.nframe-1].extraArgs = tmExtra
						if herr := s.fireLuaCallHook(co, cl, len(cargs)); herr != nil {
							if errors.Is(herr, errCoroCloseSelf) {
								return s.finishCoroCloseSelf(co)
							}
							if isUncatchableErr(herr) {
								co.status = ThreadDead
								co.clearDeadRefs(s)
								return false, nil, herr
							}
							co.status = ThreadDead
							ev := s.runtimeErrorToValue(co, herr)
							co.clearUnusedRefs(s)
							co.errObj = ev
							return false, []Value{ev}, nil
						}
						goto runCoro
					}
					res, _, err := s.callNativeHooked(co, tm, 0, len(cargs), tmExtra, maxFrames+128)
					if errors.Is(err, errYieldSentinel) {
						co.status = ThreadSuspended
						co.yieldDst = 0
						co.yieldNRes = 4
						co.yieldedNative = true
						co.yieldNativeFn = tm
						return true, s.takeYieldVals(co), nil
					}
					co.status = ThreadDead
					if err != nil {
						if errors.Is(err, errCoroCloseSelf) || errors.Is(s.abortErr, errCoroCloseSelf) {
							return s.finishCoroCloseSelf(co)
						}
						if isUncatchableErr(err) {
							co.discardTBCAtOrAbove(s, 0)
							co.closeUpvals(s, 0)
							co.clearDeadRefs(s)
							return false, nil, err
						}
						co.stackSlice(s.arena)[0] = NilValue()
						ev := s.runtimeErrorToValue(co, err)
						co.clearUnusedRefs(s)
						co.errObj = ev
						return false, []Value{ev}, nil
					}
					out := []Value{NilValue(), NilValue(), NilValue(), NilValue()}
					copy(out, res)
					co.clearDeadRefs(s)
					return true, out, nil
				}
			}
			if err := s.ensureStackWithRoots(co, 1+len(args)+8, NilValue(), args); err != nil {
				co.status = ThreadDead
				ev := s.runtimeErrorToValue(co, err)
				co.errObj = ev
				return false, []Value{ev}, nil
			}
			st := co.stackSlice(s.arena)
			st[0] = fn
			copy(st[1:1+len(args)], args)
			res, _, err := s.callNativeHooked(co, fn, 0, len(args), 0, maxFrames+128)
			if errors.Is(err, errYieldSentinel) {
				co.status = ThreadSuspended
				co.yieldDst = 0
				co.yieldNRes = -1
				co.yieldedNative = true
				co.yieldNativeFn = fn
				return true, s.takeYieldVals(co), nil
			}
			co.status = ThreadDead
			if err != nil {
				if errors.Is(err, errCoroCloseSelf) || errors.Is(s.abortErr, errCoroCloseSelf) {
					return s.finishCoroCloseSelf(co)
				}
				if isUncatchableErr(err) {
					co.discardTBCAtOrAbove(s, 0)
					co.closeUpvals(s, 0)
					co.clearDeadRefs(s)
					return false, nil, err
				}
				co.stackSlice(s.arena)[0] = NilValue()
				ev := s.runtimeErrorToValue(co, err)
				co.clearUnusedRefs(s)
				co.errObj = ev
				return false, []Value{ev}, nil
			}
			if len(res) > 0 && slicesOverlapValues(res, co.stackSlice(s.arena)) {
				cp := make([]Value, len(res))
				copy(cp, res)
				res = cp
			}
			co.clearDeadRefs(s)
			return true, res, nil
		}
		if !fn.IsClosure() {
			co.status = ThreadDead
			co.clearDeadRefs(s)
			return false, []Value{StringValue(s.intern.Intern("cannot resume non-function coroutine"))}, nil
		}
		cl := s.asClosure(fn)
		bp := s.boundProtoOf(cl)
		if err := s.ensureStackWithRoots(co, 1+len(args)+int(bp.maxStack)+16, NilValue(), args); err != nil {
			co.status = ThreadDead
			ev := s.runtimeErrorToValue(co, err)
			co.errObj = ev
			return false, []Value{ev}, nil
		}
		st := co.stackSlice(s.arena)
		copy(st[1:1+len(args)], args)
		co.top = 1 + len(args)
		if err := co.pushLuaFrame(s, cl, 0, len(args), -1); err != nil {
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
					co.discardTBCAtOrAbove(s, 0)
					co.closeUpvals(s, 0)
					co.clearDeadRefs(s)
					return false, nil, herr
				}
				if !s.handleError(co, 0, herr) {
					if errors.Is(s.abortErr, errCoroCloseSelf) {
						return s.finishCoroCloseSelf(co)
					}
					if isUncatchableErr(s.abortErr) {
						uerr := s.abortErr
						co.status = ThreadDead
						co.discardTBCAtOrAbove(s, 0)
						co.closeUpvals(s, 0)
						co.clearDeadRefs(s)
						return false, nil, uerr
					}
					co.status = ThreadDead
					ev := s.runtimeErrorToValue(co, herr)
					co.clearUnusedRefs(s)
					co.errObj = ev
					return false, []Value{ev}, nil
				}
			}
		}
	} else if co.status == ThreadSuspended {
		co.status = ThreadOK
		co.onResume()
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
				st := co.stackSlice(s.arena)
				if !yieldNativeFn.IsNil() {
					st[dst] = yieldNativeFn
				}
				if len(args) > 0 {
					copy(st[dst+1:dst+1+len(args)], args)
				}
				if end := dst + 1 + len(args); co.top < end {
					co.top = end
				}
				co.framesSlice(s.arena)[co.nframe] = callFrame{
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
				st = co.stackSlice(s.arena)
				if herr == nil && len(args) > 0 {
					updated := make([]Value, len(args))
					copy(updated, st[dst+1:dst+1+len(args)])
					args = updated
				}
				co.nframe--
				if len(args) > 0 {
					fillNil(st[dst+1 : dst+1+len(args)])
				}
				if co.yieldNRes == 0 {
					st[dst] = NilValue()
				}
			}
			if herr != nil {
				if errors.Is(herr, errCoroCloseSelf) {
					return s.finishCoroCloseSelf(co)
				}
				if isUncatchableErr(herr) {
					co.status = ThreadDead
					co.discardTBCAtOrAbove(s, 0)
					co.closeUpvals(s, 0)
					co.clearDeadRefs(s)
					return false, nil, herr
				}
				if !s.handleError(co, 0, herr) {
					if errors.Is(s.abortErr, errCoroCloseSelf) {
						return s.finishCoroCloseSelf(co)
					}
					if isUncatchableErr(s.abortErr) {
						uerr := s.abortErr
						co.status = ThreadDead
						co.discardTBCAtOrAbove(s, 0)
						co.closeUpvals(s, 0)
						co.clearDeadRefs(s)
						return false, nil, uerr
					}
					co.status = ThreadDead
					ev := s.runtimeErrorToValue(co, herr)
					co.clearUnusedRefs(s)
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				goto runCoro
			}
		}
		needSlots := max(len(args), co.yieldNRes)
		if err := s.ensureStackWithRoots(co, co.yieldDst+needSlots+4, NilValue(), args); err != nil {
			if !s.handleError(co, 0, err) {
				if errors.Is(s.abortErr, errCoroCloseSelf) {
					return s.finishCoroCloseSelf(co)
				}
				if isUncatchableErr(s.abortErr) {
					uerr := s.abortErr
					co.status = ThreadDead
					co.discardTBCAtOrAbove(s, 0)
					co.closeUpvals(s, 0)
					co.clearDeadRefs(s)
					return false, nil, uerr
				}
				co.status = ThreadDead
				ev := s.runtimeErrorToValue(co, err)
				co.clearUnusedRefs(s)
				co.errObj = ev
				return false, []Value{ev}, nil
			}
			goto runCoro
		}
		if err := co.placeResults(s, co.yieldDst, args, co.yieldNRes); err != nil {
			if !s.handleError(co, 0, err) {
				if errors.Is(s.abortErr, errCoroCloseSelf) {
					return s.finishCoroCloseSelf(co)
				}
				if isUncatchableErr(s.abortErr) {
					uerr := s.abortErr
					co.status = ThreadDead
					co.discardTBCAtOrAbove(s, 0)
					co.closeUpvals(s, 0)
					co.clearDeadRefs(s)
					return false, nil, uerr
				}
				co.status = ThreadDead
				ev := s.runtimeErrorToValue(co, err)
				co.clearUnusedRefs(s)
				co.errObj = ev
				return false, []Value{ev}, nil
			}
			goto runCoro
		}
		for co.nframe > 0 && co.framesSlice(s.arena)[co.nframe-1].finish != finishNone {
			cf := &co.framesSlice(s.arena)[co.nframe-1]
			cont, cerr := s.completeFinishOp(co, cf, 0)
			if errors.Is(cerr, errYieldSentinel) {
				co.status = ThreadSuspended
				return true, s.takeYieldVals(co), nil
			}
			if cerr != nil {
				if errors.Is(cerr, errCoroCloseSelf) {
					return s.finishCoroCloseSelf(co)
				}
				if isUncatchableErr(cerr) {
					co.status = ThreadDead
					co.discardTBCAtOrAbove(s, 0)
					co.closeUpvals(s, 0)
					co.clearDeadRefs(s)
					return false, nil, cerr
				}
				if !s.handleError(co, 0, cerr) {
					if errors.Is(s.abortErr, errCoroCloseSelf) {
						return s.finishCoroCloseSelf(co)
					}
					if isUncatchableErr(s.abortErr) {
						uerr := s.abortErr
						co.status = ThreadDead
						co.discardTBCAtOrAbove(s, 0)
						co.closeUpvals(s, 0)
						co.clearDeadRefs(s)
						return false, nil, uerr
					}
					co.status = ThreadDead
					ev := s.runtimeErrorToValue(co, cerr)
					co.clearUnusedRefs(s)
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
			co.closeAllSync(s, 0, NilValue(), false)
			co.status = ThreadDead
			nres := co.top
			if nres <= 0 {
				co.clearDeadRefs(s)
				return true, nil, nil
			}
			res := make([]Value, nres)
			copy(res, co.stackSlice(s.arena)[:nres])
			co.clearDeadRefs(s)
			return true, res, nil
		}
	}

runCoro:
	err := s.runThread(co, 0)
	if uerr := co.uncaughtErr(s); uerr != nil {
		err = uerr
		co.setUncaughtErr(s, nil)
	}
	if errors.Is(err, errYieldSentinel) {
		co.status = ThreadSuspended
		return true, s.takeYieldVals(co), nil
	}
	if errors.Is(err, errCoroCloseSelf) || errors.Is(s.abortErr, errCoroCloseSelf) {
		return s.finishCoroCloseSelf(co)
	}
	if err != nil {
		if isUncatchableErr(err) {
			co.status = ThreadDead
			co.discardTBCAtOrAbove(s, 0)
			co.closeUpvals(s, 0)
			co.clearDeadRefs(s)
			return false, nil, err
		}
		ev := s.runtimeErrorToValue(co, err)
		co.status = ThreadDead
		co.clearUnusedRefs(s)
		co.errObj = ev
		return false, []Value{ev}, nil
	}
	co.closeAllSync(s, 0, NilValue(), false)
	co.status = ThreadDead
	nres := co.top
	if nres <= 0 {
		co.clearDeadRefs(s)
		return true, nil, nil
	}
	res := make([]Value, nres)
	copy(res, co.stackSlice(s.arena)[:nres])
	co.clearDeadRefs(s)
	return true, res, nil
}

func (s *State) finishCoroCloseSelf(co *Thread) (bool, []Value, error) {
	if errors.Is(s.abortErr, errCoroCloseSelf) {
		s.abortErr = nil
	}
	co.closing = true
	co.nframe = 0
	co.nny++
	errVal, hasErr, _ := co.closeAllSync(s, 0, NilValue(), false)
	co.nny--
	co.closing = false
	co.status = ThreadDead
	co.clearDeadRefs(s)
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
	co := s.asThread(args[0])
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
	th := s.cur()
	if !th.IsYieldable() {
		if s.isMainThread(th) {
			return nil, &LuaError{Val: StringValue(s.intern.Intern("attempt to yield from outside a coroutine"))}
		}
		return nil, &LuaError{Val: StringValue(s.intern.Intern("attempt to yield across a C-call boundary"))}
	}
	if err := th.setYieldVals(s, args); err != nil {
		return nil, s.memError()
	}
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
	return []Value{StringValue(s.intern.Intern(s.asThread(args[0]).StatusString(s)))}, nil
}

func nativeCoroRunning(s *State, _ []Value) ([]Value, error) {
	cur := s.cur()
	isMain := s.isMainThread(cur)
	return []Value{ThreadValue(cur), BoolValue(isMain)}, nil
}

func nativeCoroIsYieldable(s *State, args []Value) ([]Value, error) {
	th := s.cur()
	if len(args) >= 1 && !args[0].IsNil() {
		if !args[0].IsThread() {
			return nil, fmt.Errorf("bad argument #1 to 'isyieldable' (thread expected, got %s)", s.objTypeName(args[0]))
		}
		th = s.asThread(args[0])
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
	if err := s.checkPreallocMemWithRoots(s.coroWrapPreallocBytes(), args[0], NilValue(), NilValue()); err != nil {
		return nil, err
	}
	co, err := s.newThread(128, defaultFrameCap)
	if err != nil {
		return nil, s.memError()
	}
	co.stackSlice(s.arena)[0] = args[0]
	co.top = 1
	var selfVal Value
	wrapper := &NativeFunc{
		Name:        "coroutine.wrap",
		NumUpvals:   1,
		Collectable: true,
		upvals:      []Value{ThreadValue(co)},
		Fn: func(s *State, callArgs []Value) ([]Value, error) {
			uv, okUV := s.nativeUpvalGet(selfVal, 0)
			if !okUV || !uv.IsThread() {
				got := "no value"
				if okUV {
					got = s.objTypeName(uv)
				}
				return nil, fmt.Errorf("thread expected, got %s", got)
			}
			co := s.asThread(uv)
			wasActive := co == s.cur() || co.status == ThreadNormal
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
					co.closing = true
					co.nframe = 0
					errVal, _, _ = co.closeAllSync(s, 0, errVal, true)
					co.closing = false
					co.status = ThreadDead
					co.clearDeadRefs(s)
					co.errObj = NilValue()
				}
				if errVal.IsString() {
					if where := s.errorWhere(1); where != "" {
						if ls, ierr := s.TryIntern(where + s.asString(errVal).Str()); ierr == nil {
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
	val, err := s.allocDynNative(wrapper)
	if err != nil {
		return nil, s.memError()
	}
	selfVal = val
	return []Value{val}, nil
}

func nativeCoroClose(s *State, args []Value) ([]Value, error) {
	co := s.cur()
	if len(args) >= 1 && !args[0].IsNil() {
		if !args[0].IsThread() {
			return nil, fmt.Errorf("bad argument #1 to 'close' (thread expected, got %s)", s.objTypeName(args[0]))
		}
		co = s.asThread(args[0])
	}
	status := co.StatusString(s)
	switch status {
	case "normal":
		return nil, fmt.Errorf("cannot close a normal coroutine")
	case "running":
		if s.isMainThread(co) {
			return nil, fmt.Errorf("cannot close main thread")
		}
		return nil, errCoroCloseSelf
	case "dead", "suspended":
		co.closing = true
		co.nframe = 0
		co.nny++
		hasInitErr := co.status == ThreadDead && !co.errObj.IsNil()
		errVal := co.errObj
		errVal, hasErr, _ := co.closeAllSync(s, 0, errVal, hasInitErr)
		co.nny--
		co.closing = false
		co.status = ThreadDead
		co.clearDeadRefs(s)
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
