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
	s.registerFunc(coro, "status", intrNone, nativeCoroStatus)
	s.registerFunc(coro, "running", intrNone, nativeCoroRunning)
	s.registerFunc(coro, "isyieldable", intrNone, nativeCoroIsYieldable)
	s.registerFunc(coro, "wrap", intrNone, nativeCoroWrap)
	s.registerFunc(coro, "close", intrNone, nativeCoroClose)
	s.SetGlobal("coroutine", TableValue(coro))
}

func nativeCoroCreate(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsFunction() {
		return nil, fmt.Errorf("bad argument #1 to 'create' (function expected)")
	}
	if s.memLimit > 0 {
		if err := s.checkMemAndAccount(512 + 128*16); err != nil {
			return nil, err
		}
	}
	co := newThread(s, 128)
	co.stack[0] = args[0]
	co.top = 1
	return []Value{ThreadValue(co)}, nil
}

func (s *State) resumeThread(co *Thread, args []Value) (bool, []Value, error) {
	if co == s.cur {
		return false, []Value{StringValue(s.intern.Intern("cannot resume non-suspended coroutine"))}, nil
	}
	if co.status == ThreadDead {
		return false, []Value{StringValue(s.intern.Intern("cannot resume dead coroutine"))}, nil
	}
	if co.status == ThreadNormal {
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
			return false, []Value{StringValue(s.intern.Intern("cannot resume dead coroutine"))}, nil
		}
		if fn.IsNative() {
			nf := fn.AsNative()
			if nf.Intrinsic == intrPCall && len(args) >= 1 && args[0].IsClosure() {
				co.ensureStack(len(args) + 8)
				for i, a := range args {
					co.stack[1+i] = a
				}
				co.top = 1 + len(args)
				if _, err := s.pushProtectedCall(co, 0, len(args), -1, false); err != nil {
					co.status = ThreadDead
					if isUncatchableErr(err) {
						return false, nil, err
					}
					ev := errorToLuaValue(s, err)
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				goto runCoro
			}
			if nf.Intrinsic == intrXPCall && len(args) >= 2 && args[0].IsClosure() && args[1].IsFunction() {
				co.ensureStack(len(args) + 8)
				for i, a := range args {
					co.stack[1+i] = a
				}
				co.top = 1 + len(args)
				if _, err := s.pushProtectedCall(co, 0, len(args), -1, true); err != nil {
					co.status = ThreadDead
					if isUncatchableErr(err) {
						return false, nil, err
					}
					ev := errorToLuaValue(s, err)
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				goto runCoro
			}
			if nf.Intrinsic == intrDoFile {
				loaded, lerr := nativeLoadFile(s, args)
				if lerr != nil || len(loaded) == 0 || loaded[0].IsNil() {
					co.status = ThreadDead
					co.stack[0] = NilValue()
					var ev Value
					if lerr != nil {
						ev = errorToLuaValue(s, lerr)
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
					ev := errorToLuaValue(s, err)
					co.errObj = ev
					return false, []Value{ev}, nil
				}
				goto runCoro
			}
			res, err := nf.callMulti(s, args)
			if errors.Is(err, errYieldSentinel) {
				co.status = ThreadSuspended
				out := co.yieldVals
				co.yieldVals = nil
				return true, out, nil
			}
			co.status = ThreadDead
			co.stack[0] = NilValue()
			if err != nil {
				if isUncatchableErr(err) {
					return false, nil, err
				}
				ev := errorToLuaValue(s, err)
				co.errObj = ev
				return false, []Value{ev}, nil
			}
			return true, res, nil
		}
		if !fn.IsClosure() {
			co.status = ThreadDead
			return false, []Value{StringValue(s.intern.Intern("cannot resume non-function coroutine"))}, nil
		}
		cl := fn.AsClosure()
		co.ensureStack(1 + len(args) + int(cl.bp.proto.MaxStackSize) + 16)
		for i, a := range args {
			co.stack[1+i] = a
		}
		co.top = 1 + len(args)
		if err := co.pushLuaFrame(cl, 0, len(args), -1); err != nil {
			co.status = ThreadDead
			ev := errorToLuaValue(s, err)
			co.errObj = ev
			return false, []Value{ev}, nil
		}
		if co.hookMask&HookMaskCall != 0 && !co.inHook {
			if herr := s.fireHook(co, "call", NilValue()); herr != nil {
				co.status = ThreadDead
				if isUncatchableErr(herr) {
					return false, nil, herr
				}
				ev := errorToLuaValue(s, herr)
				co.errObj = ev
				return false, []Value{ev}, nil
			}
		}
	} else if co.status == ThreadSuspended {
		// Resuming a yielded coroutine: deliver resume args as the results of coroutine.yield(...)!
		co.status = ThreadOK
		co.placeResults(co.yieldDst, args, co.yieldNRes)
		if co.yieldedNative {
			co.yieldedNative = false
			if co.hookMask&HookMaskRet != 0 && !co.inHook {
				if herr := s.fireHook(co, "return", NilValue()); herr != nil {
					co.status = ThreadDead
					if isUncatchableErr(herr) {
						return false, nil, herr
					}
					ev := errorToLuaValue(s, herr)
					co.errObj = ev
					return false, []Value{ev}, nil
				}
			}
		}
		// If yielded from a native metamethod/TBC __close (e.g. func2close(coroutine.yield)), complete continuation!
		for co.nframe > 0 && co.frames[co.nframe-1].finish != finishNone {
			cf := &co.frames[co.nframe-1]
			cont, cerr := s.completeFinishOp(co, cf, 0)
			if errors.Is(cerr, errYieldSentinel) {
				co.status = ThreadSuspended
				out := co.yieldVals
				co.yieldVals = nil
				return true, out, nil
			}
			if cerr != nil {
				if isUncatchableErr(cerr) {
					co.status = ThreadDead
					return false, nil, cerr
				}
				if !s.handleError(co, 0, cerr) {
					co.status = ThreadDead
					ev := errorToLuaValue(s, cerr)
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
				co.stack[0] = NilValue()
				return true, nil, nil
			}
			res := make([]Value, nres)
			copy(res, co.stack[:nres])
			co.stack[0] = NilValue()
			return true, res, nil
		}
	}

runCoro:
	err := s.runThread(co, 0)
	if errors.Is(err, errYieldSentinel) {
		co.status = ThreadSuspended
		out := co.yieldVals
		co.yieldVals = nil
		return true, out, nil
	}
	if errors.Is(err, errCoroCloseSelf) {
		// Coroutine closed itself via coroutine.close(): run TBC finalizers (non-yieldable) and terminate!
		co.nny++
		errVal, hasErr, _ := co.closeAllSync(0, NilValue(), false)
		co.nny--
		co.status = ThreadDead
		co.nframe = 0
		co.top = 0
		co.stack[0] = NilValue()
		co.errObj = NilValue()
		if hasErr {
			return false, []Value{errVal}, nil
		}
		return true, nil, nil
	}
	if err != nil {
		if isUncatchableErr(err) {
			co.status = ThreadDead
			return false, nil, err
		}
		// Per Lua 5.5 spec, error in coroutine does NOT automatically close TBC vars until coroutine.close().
		co.status = ThreadDead
		ev := errorToLuaValue(s, err)
		co.errObj = ev
		return false, []Value{ev}, nil
	}
	// Normal completion: close any remaining TBC vars and mark dead.
	co.closeAllSync(0, NilValue(), false)
	co.status = ThreadDead
	nres := co.top
	if nres <= 0 {
		co.stack[0] = NilValue()
		return true, nil, nil
	}
	res := make([]Value, nres)
	copy(res, co.stack[:nres])
	co.stack[0] = NilValue()
	co.top = 0
	return true, res, nil
}

func nativeCoroResume(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsThread() {
		return nil, fmt.Errorf("bad argument #1 to 'resume' (thread expected)")
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
			return nil, fmt.Errorf("attempt to yield from outside a coroutine")
		}
		return nil, fmt.Errorf("attempt to yield across a C-call boundary")
	}
	yv := make([]Value, len(args))
	copy(yv, args)
	th.yieldVals = yv
	return nil, errYieldSentinel
}

func nativeCoroStatus(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsThread() {
		return nil, fmt.Errorf("bad argument #1 to 'status' (thread expected)")
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
			return nil, fmt.Errorf("bad argument #1 to 'isyieldable' (thread expected)")
		}
		th = args[0].AsThread()
	}
	return []Value{BoolValue(th.IsYieldable())}, nil
}

func nativeCoroWrap(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsFunction() {
		return nil, fmt.Errorf("bad argument #1 to 'wrap' (function expected)")
	}
	if s.memLimit > 0 {
		if err := s.checkMemAndAccount(512 + 128*16); err != nil {
			return nil, err
		}
	}
	co := newThread(s, 128)
	co.stack[0] = args[0]
	co.top = 1
	wrapper := &NativeFunc{
		Name:        "coroutine.wrap",
		Collectable: true,
		gcChild:     ThreadValue(co),
		Fn: func(s *State, callArgs []Value) ([]Value, error) {
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
					errVal, _, _ = co.closeAllSync(0, errVal, true)
					co.closing = false
					co.status = ThreadDead
					co.nframe = 0
					co.top = 0
					co.stack[0] = NilValue()
					co.errObj = NilValue()
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
			return nil, fmt.Errorf("bad argument #1 to 'close' (thread expected)")
		}
		co = args[0].AsThread()
	}
	if co.closing {
		return []Value{TrueValue()}, nil
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
		hasInitErr := co.status == ThreadDead && !co.errObj.IsNil()
		errVal := co.errObj
		errVal, hasErr, _ := co.closeAllSync(0, errVal, hasInitErr)
		co.closing = false
		co.status = ThreadDead
		co.nframe = 0
		co.top = 0
		co.stack[0] = NilValue()
		co.errObj = NilValue()
		if hasErr {
			return []Value{FalseValue(), errVal}, nil
		}
		return []Value{TrueValue()}, nil
	default:
		return []Value{TrueValue()}, nil
	}
}
