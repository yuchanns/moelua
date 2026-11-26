package vm

import (
	"errors"
	"fmt"
	"unsafe"
)

// ThreadStatus represents the lifecycle state of a Lua 5.5 coroutine / Thread.
type ThreadStatus uint8

const (
	ThreadOK        ThreadStatus = iota // running or ready to start (suspended before first resume)
	ThreadSuspended                     // yielded via coroutine.yield
	ThreadNormal                        // resumed another coroutine
	ThreadDead                          // finished normally, errored, or explicitly closed
)

// errYieldSentinel is returned internally by run() when coroutine.yield suspends a Thread.
// No Go panic/recover is involved; all frames remain intact on Thread.frames!
var errYieldSentinel = errors.New("moelua: yield")

// errCoroCloseSelf is returned internally when a running coroutine calls coroutine.close() on itself.
var errCoroCloseSelf = errors.New("moelua: close self")

// frameKind identifies the kind of entry on Thread.frames.
type frameKind uint8

const (
	frameLua frameKind = iota
	framePCall
	frameXPCall
	frameNative
)

// finishKind records how an interrupted opcode in a caller Lua frame should
// complete once a pushed metamethod/continuation frame returns (enabling
// stackful coroutine.yield across ALL metamethods, __close, and pcall!).
type finishKind uint8

const (
	finishNone     finishKind = iota
	finishSetReg              // store 1st return value into caller R[finishReg]
	finishCondJump            // evaluate truthiness of 1st return value vs finishFlag (for __eq/__lt/__le)
	finishCondNot             // negate truthiness of 1st return value vs finishFlag (for __le via not __lt)
	finishSetDrop             // ignore return value (for __newindex)
	finishConcat              // resume multi-operand OP_CONCAT loop
	finishTForCall            // resume OP_TFORCALL -> OP_TFORLOOP
	finishClose               // resume closing remaining TBC vars after __close returns
	finishPairs               // normalize __pairs return arity to exactly 4 values
	finishTailReturn          // finish OP_TAILCALL native yield when next insn is not matching OP_RETURN
)

// callFrame is one frame on a Thread's flat stackful call stack.
type callFrame struct {
	kind       frameKind
	cl         *Closure
	base       int // register R[0] index in Thread.stack
	funcIdx    int // callee slot index in Thread.stack
	pc         int // saved PC
	nresults   int // expected results (-1 = MULTRET)
	nextraargs int // hidden vararg count (Lua 5.5 PF_VAHID)
	varargTab  *Table

	// Protected call / native hook metadata (framePCall / frameXPCall / frameNative).
	errFunc  Value // message handler for xpcall
	nativeFn Value // native func value for frameNative debug hooks
	oldTop   int

	// Continuation metadata for yieldable metamethods & TBC closing.
	finish     finishKind
	finishReg  uint8
	finishFlag bool
	// Concat continuation state
	concatStart int
	concatIdx   int
	// Close continuation state
	closeLevel   int
	closeErr     Value
	closeHasErr  bool
	closeResume  uint8 // 0=continue pc, 1=resume OP_RETURN, 2=resume error unwind, 3=coroutine.close
	closeRetSrc  int
	closeRetN    int
	closeEntry   int

	// Debug metadata
	dirty        bool // true if any non-fast-path field was written (cleared lazily on next fast OP_CALL)
	isTailCall   bool
	isHook       bool
	isFinalizer  bool // true when frame was invoked as a __gc finalizer (CIST_FIN -> db.lua:920)
	atCurrentPC  bool // true during line/count hook before instruction executes (db.lua:696)
	topProtected bool // true for host CallProtected frames (rendered as "[C]: in ?" in tracebacks)
	errHandling  bool // true while this frameXPCall's message handler is actively running
	extraArgs    uint8
	oldPC       int // per-frame last traced PC (-1 = fresh frame)
	ftransfer   uint16
	ntransfer   uint16
}

// Thread is a heap-allocated Lua 5.5 coroutine / execution thread (ZERO Go goroutines!).
// An unreachable suspended Thread is naturally collected by Go's GC (and Tier 1 marker).
type Thread struct {
	state *State

	stack  []Value
	top    int
	frames []callFrame
	nframe int

	// Optional views of stateAlloc's embedded initStack/initFrames for mainThread.
	// Cleared only once abandoned AND no active native args / frames can still borrow them.
	initStack           []Value
	initFrames          []callFrame
	nativeArgsDepth     int
	initStackAbandoned  bool
	initFramesAbandoned bool

	openUpvals []*Upvalue // ascending stack indices of open upvalues owned by this thread
	tbc        []int      // ascending stack indices of active <close> variables

	status        ThreadStatus
	errObj        Value   // last error object when dead due to error
	lastErrNative Value   // native function that raised an uncaught error on this thread (for dead-coro traceback)
	uncaughtErr   error   // replacement error raised while unwinding a protected call that escaped entryDepth
	syncCloseErrs []Value // active error objects held across synchronous closeAllSync callbacks (GC-traced)
	caller        *Thread // resuming parent thread (non-nil while running)

	// Yielded results & resume target slot (transferred directly between threads on yield/resume).
	yieldVals     []Value
	yieldDst      int
	yieldNRes     int
	yieldedNative bool  // true if suspended inside a native call (for HookMaskRet on resume)
	yieldNativeFn Value // yielded native callee (for HookMaskRet frameNative on resume)

	// Debug hooks (zero overhead in fast run() when hookMask == 0!).
	hook          Value
	hookMask      uint8 // bit 0=call, 1=ret, 2=line, 3=count
	baseHookCount int
	hookCount     int
	oldPC         int
	inHook        bool
	suppressHook  bool // true inside __gc finalizers (matching L->allowhook = 0 without clobbering hookMask)

	// Window generation counter for -tags moeluaregcheck verification.
	stackGen uint32

	// Number of non-yieldable C/Go calls active on this thread (matching Lua 5.5 L->nny).
	// mainThread starts with nny=1; new coroutines start with nny=0.
	nny                uint16
	inErrHandler       bool // true while running xpcall message handler (allows extra frame headroom on stack overflow)
	closing            bool // true while coroutine.close(co) is actively closing TBC vars on co
	nextFrameFinalizer bool // consumed by next pushLuaFrame to tag __gc finalizer frame (CIST_FIN)

	// Tier 1 GC mark field.
	gcMark uint64
}

// Hook mask bits matching Lua 5.5 lua.h.
const (
	HookMaskCall  uint8 = 1 << 0
	HookMaskRet   uint8 = 1 << 1
	HookMaskLine  uint8 = 1 << 2
	HookMaskCount uint8 = 1 << 3
)

const defaultFrameCap = 32

func threadInitMemBytes(stackCap, frameCap int) int64 {
	return 512 + int64(stackCap)*16 + int64(frameCap)*int64(unsafe.Sizeof(callFrame{}))
}

func newThread(s *State, stackCap int) *Thread {
	if stackCap < 128 {
		stackCap = 128
	}
	th := &Thread{
		state:  s,
		stack:  make([]Value, stackCap),
		frames: make([]callFrame, defaultFrameCap),
		status: ThreadOK,
		errObj: NilValue(),
		hook:   NilValue(),
	}
	fillNil(th.stack)
	return th
}

func (th *Thread) ensureFrameCap(need, limit int) error {
	if need <= len(th.frames) {
		return nil
	}
	if need > limit {
		return fmt.Errorf("stack overflow")
	}
	newCap := len(th.frames) * 2
	if newCap < defaultFrameCap {
		newCap = defaultFrameCap
	}
	for newCap < need {
		newCap *= 2
	}
	if newCap > limit {
		newCap = limit
	}
	if th.state != nil && (th.state.memLimit > 0 || th.state.gc.tier1) {
		delta := int64(newCap-len(th.frames)) * int64(unsafe.Sizeof(callFrame{}))
		if err := th.state.checkMemAndAccountWithRoots(delta, ThreadValue(th), NilValue(), NilValue()); err != nil {
			return err
		}
	}
	old := th.frames
	nf := make([]callFrame, newCap)
	copy(nf, old)
	th.frames = nf
	if th.initFrames != nil && len(old) > 0 && unsafe.SliceData(old) == unsafe.SliceData(th.initFrames) {
		if th.nframe == 0 {
			clear(th.initFrames)
			th.initFrames = nil
		} else {
			th.initFramesAbandoned = true
		}
	}
	return nil
}

const maxStackSlots = 2_000_000

func (th *Thread) ensureStack(need int) error {
	if need+stackSlack <= len(th.stack) {
		return nil
	}
	limit := maxStackSlots
	if th.inErrHandler {
		limit = maxStackSlots + 8192
	}
	if need < 0 || need+stackSlack > limit {
		return fmt.Errorf("stack overflow")
	}
	newSize := len(th.stack) * 2
	for newSize < need+stackSlack {
		newSize *= 2
	}
	if newSize > limit {
		newSize = limit
	}
	delta := int64(newSize-len(th.stack)) * 16
	if th.state != nil && (th.state.memLimit > 0 || th.state.gc.tier1) {
		if err := th.state.checkMemAndAccountWithRoots(delta, ThreadValue(th), NilValue(), NilValue()); err != nil {
			return err
		}
	}
	old := th.stack
	ns := make([]Value, newSize)
	copy(ns, old)
	fillNil(ns[len(old):])
	th.stack = ns
	th.stackGen++
	if th.initStack != nil && len(old) > 0 && unsafe.SliceData(old) == unsafe.SliceData(th.initStack) {
		if th.nativeArgsDepth == 0 {
			clear(th.initStack)
			th.initStack = nil
		} else {
			th.initStackAbandoned = true
		}
	}
	return nil
}

func (th *Thread) clearAbandonedInitStack() {
	if th.initStackAbandoned && th.initStack != nil && th.nativeArgsDepth == 0 {
		clear(th.initStack)
		th.initStack = nil
		th.initStackAbandoned = false
	}
}

// finishNativeArgs decrements nativeArgsDepth after a borrowed-stack native call.
// If the call grew th.stack off initStack and the native returned a slice aliasing
// initStack (e.g. returning its args slice directly), res is copied before clearing
// the abandoned initStack storage.
func (th *Thread) finishNativeArgs(res []Value) []Value {
	th.nativeArgsDepth--
	if th.nativeArgsDepth == 0 && th.initStackAbandoned && th.initStack != nil {
		if len(res) > 0 && slicesOverlapValues(res, th.initStack) {
			cp := make([]Value, len(res))
			copy(cp, res)
			res = cp
		}
		clear(th.initStack)
		th.initStack = nil
		th.initStackAbandoned = false
	}
	return res
}

func (th *Thread) clearAbandonedInitStorage() {
	th.clearAbandonedInitStack()
	if th.initFramesAbandoned && th.initFrames != nil && th.nframe == 0 {
		clear(th.initFrames)
		th.initFrames = nil
		th.initFramesAbandoned = false
	}
}

// window returns a *[256]Value view rooted at th.stack[base].
//
//go:nocheckptr
func (th *Thread) window(base int) *[256]Value {
	return (*[256]Value)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(th.stack)), base*int(unsafe.Sizeof(Value{}))))
}

func (th *Thread) findOrCreateUpval(stackIdx int) *Upvalue {
	n := len(th.openUpvals)
	i := n
	for i > 0 && th.openUpvals[i-1].stackIdx > stackIdx {
		i--
	}
	if i > 0 && th.openUpvals[i-1].stackIdx == stackIdx {
		return th.openUpvals[i-1]
	}
	uv := &Upvalue{owner: th, stackIdx: stackIdx}
	th.openUpvals = append(th.openUpvals, nil)
	copy(th.openUpvals[i+1:], th.openUpvals[i:n])
	th.openUpvals[i] = uv
	return uv
}

func (th *Thread) closeUpvals(level int) {
	n := len(th.openUpvals)
	for n > 0 && th.openUpvals[n-1].stackIdx >= level {
		n--
		uv := th.openUpvals[n]
		th.openUpvals[n] = nil
		if uv.owner == th {
			uv.closed = th.stack[uv.stackIdx]
			uv.owner = nil
		}
	}
	th.openUpvals = th.openUpvals[:n]
}

// detachOpenUpvals snapshots open upvalues into uv.closed, clears uv.owner = nil, and
// nils the shadowed stack slot while keeping th.openUpvals intact. Breaking strong
// uv -> Thread edges (with no sibling uv.next pointers) while a coroutine is suspended
// or errored lets Go's Tier 0 GC reclaim unreachable coroutines and unrelated locals,
// arguments, and sibling upvalues even if escaped closures survive.
func (th *Thread) detachOpenUpvals() {
	for _, uv := range th.openUpvals {
		if uv.owner == th {
			uv.closed = th.stack[uv.stackIdx]
			th.stack[uv.stackIdx] = NilValue()
			uv.owner = nil
		}
	}
}

// attachOpenUpvals re-attaches detached open upvalues back to th.stack when a suspended
// coroutine resumes (or before closing TBC vars), syncing any mutations written to
// uv.closed while suspended.
func (th *Thread) attachOpenUpvals() {
	for _, uv := range th.openUpvals {
		if uv.owner == nil {
			th.stack[uv.stackIdx] = uv.closed
			uv.closed = NilValue()
			uv.owner = th
		}
	}
}

// localSlotValue reads th.stack[slot], transparently consulting detached open upvalues
// on suspended or errored coroutines.
func (th *Thread) localSlotValue(slot int) Value {
	for i := len(th.openUpvals) - 1; i >= 0; i-- {
		uv := th.openUpvals[i]
		if uv.stackIdx < slot {
			break
		}
		if uv.stackIdx == slot {
			if uv.owner == nil {
				return uv.closed
			}
			break
		}
	}
	return th.stack[slot]
}

// setLocalSlotValue writes val to th.stack[slot], updating detached open upvalues on
// suspended or errored coroutines so live escaped closures observe debug.setlocal writes.
func (th *Thread) setLocalSlotValue(slot int, val Value) {
	for i := len(th.openUpvals) - 1; i >= 0; i-- {
		uv := th.openUpvals[i]
		if uv.stackIdx < slot {
			break
		}
		if uv.stackIdx == slot {
			if uv.owner == nil {
				uv.closed = val
				return
			}
			break
		}
	}
	th.stack[slot] = val
}

// markTBC marks th.stack[idx] as a <close> to-be-closed variable (OP_TBC / OP_TFORPREP).
func (th *Thread) markTBC(idx int) error {
	v := th.stack[idx]
	if v.IsFalsy() {
		return nil // nil and false are ignored for <close>
	}
	tm := th.state.getTMByObj(v, TMClose)
	if tm.IsNil() {
		return &LuaError{Val: StringValue(th.state.intern.Intern("variable got a non-closable value"))}
	}
	th.tbc = append(th.tbc, idx)
	return nil
}

// hasTBCAtOrAbove reports whether any <close> variable sits at stack index >= level.
func (th *Thread) hasTBCAtOrAbove(level int) bool {
	return len(th.tbc) > 0 && th.tbc[len(th.tbc)-1] >= level
}

// discardTBCAtOrAbove removes pending <close> entries >= level without executing user Lua
// __close callbacks (used when unwinding after an uncatchable context/instruction-limit abort).
// Open upvalues >= level are closed first so escaped closures capture the live values
// before the TBC stack slots are cleared.
func (th *Thread) discardTBCAtOrAbove(level int) {
	th.closeUpvals(level)
	for len(th.tbc) > 0 && th.tbc[len(th.tbc)-1] >= level {
		idx := th.tbc[len(th.tbc)-1]
		th.tbc = th.tbc[:len(th.tbc)-1]
		if idx < len(th.stack) {
			th.stack[idx] = NilValue()
		}
	}
}

// closeAllSync closes upvalues and all <close> variables >= level synchronously (used on unwind/close).
// If errVal is non-nil (hasErr == true), passes it as the 2nd argument to __close;
// if __close itself raises an error, the new error replaces errVal while continuing to close remaining TBC vars!
// Returns (finalErrVal, hasErr, newErrRaisedByClose).
func (th *Thread) closeAllSync(level int, errVal Value, hasErr bool) (Value, bool, bool) {
	// Restore any detached open upvalues back onto th.stack before closing so TBC <close>
	// variables that were also captured by escaped closures remain present in th.stack[idx].
	th.attachOpenUpvals()
	th.closeUpvals(level)
	raised := false
	prevCur := th.state.cur
	var prevStatus ThreadStatus
	var prevCurRef int
	if prevCur != nil && prevCur != th {
		prevStatus = prevCur.status
		prevCur.status = ThreadNormal
		// Pin the calling coroutine across synchronous closing on another thread so
		// GC triggered inside __close cannot collect live values on prevCur's stack.
		prevCurRef = th.state.Ref(ThreadValue(prevCur))
	}
	th.state.cur = th
	errSlot := len(th.syncCloseErrs)
	th.syncCloseErrs = append(th.syncCloseErrs, errVal)
	defer func() {
		th.syncCloseErrs[errSlot] = NilValue()
		th.syncCloseErrs = th.syncCloseErrs[:errSlot]
		th.state.cur = prevCur
		if prevCur != nil && prevCur != th {
			prevCur.status = prevStatus
			th.state.Unref(prevCurRef)
		}
	}()
	for len(th.tbc) > 0 && th.tbc[len(th.tbc)-1] >= level {
		idx := th.tbc[len(th.tbc)-1]
		th.tbc = th.tbc[:len(th.tbc)-1]
		obj := th.stack[idx]
		th.stack[idx] = NilValue()
		if obj.IsFalsy() {
			continue
		}
		tm := th.state.getTMByObj(obj, TMClose)
		if tm.IsNil() {
			errVal = StringValue(th.state.intern.Intern("attempt to call a nil value (metamethod 'close')"))
			th.syncCloseErrs[errSlot] = errVal
			hasErr = true
			raised = true
			continue
		}
		var args []Value
		if hasErr {
			args = []Value{obj, errVal}
		} else {
			args = []Value{obj}
		}
		if _, err := th.state.callOnThread(th, tm, args); err != nil {
			if errors.Is(err, errYieldSentinel) {
				// Yielding inside sync close path: propagate cleanly via errVal marker handled by caller.
				return errVal, hasErr, raised
			}
			if errors.Is(err, errCoroCloseSelf) {
				if th.closing {
					// Coroutine called coroutine.close(self) while already closing:
					// unwind this __close call and continue closing any remaining TBC vars.
					if errors.Is(th.state.abortErr, errCoroCloseSelf) {
						th.state.abortErr = nil
					}
					continue
				}
				th.state.abortErr = err
				return errVal, hasErr, raised
			}
			if isUncatchableErr(err) {
				th.state.abortErr = err
				th.discardTBCAtOrAbove(level)
				return errVal, hasErr, raised
			}
			errVal = errorToLuaValue(th.state, err)
			th.syncCloseErrs[errSlot] = errVal
			hasErr = true
			if le, ok := err.(*LuaError); ok && le.xpHandled {
				raised = false
			} else {
				raised = true
			}
		}
	}
	return errVal, hasErr, raised
}

// pushLuaFrame pushes a Lua closure frame onto th's flat call stack (zero Go recursion!).
func (th *Thread) pushLuaFrame(cl *Closure, funcIdx, nargs, nresults int) error {
	limit := maxFrames
	if th.inErrHandler {
		limit = maxFrames + 128
	}
	if err := th.ensureFrameCap(th.nframe+1, limit); err != nil {
		return err
	}
	p := cl.bp.proto
	nparams := int(p.NumParams)
	base := funcIdx + 1

	if p.IsVararg() {
		if err := th.ensureStack(base + max(nargs, int(p.MaxStackSize)) + nparams + 8); err != nil {
			return err
		}
		th.top = base + nargs
	} else {
		if err := th.ensureStack(base + int(p.MaxStackSize) + 8); err != nil {
			return err
		}
		if nargs < nparams {
			fillNil(th.stack[base+nargs : base+nparams])
		}
		if int(p.MaxStackSize) > nparams {
			fillNil(th.stack[base+nparams : base+int(p.MaxStackSize)])
		}
		th.top = base + int(p.MaxStackSize)
	}

	th.frames[th.nframe] = callFrame{
		kind:        frameLua,
		cl:          cl,
		base:        base,
		funcIdx:     funcIdx,
		pc:          0,
		nresults:    nresults,
		oldPC:       -1,
		isFinalizer: th.nextFrameFinalizer,
		dirty:       true,
	}
	th.nextFrameFinalizer = false
	th.nframe++
	return nil
}

func (th *Thread) placeResults(dst int, res []Value, nresults int) error {
	if nresults < 0 {
		if th.state != nil {
			if err := th.state.ensureStackWithRoots(th, dst+len(res)+4, NilValue(), res); err != nil {
				return err
			}
		} else if err := th.ensureStack(dst + len(res) + 4); err != nil {
			return err
		}
		copy(th.stack[dst:dst+len(res)], res)
		th.top = dst + len(res)
		return nil
	}
	n := min(len(res), nresults)
	if th.state != nil {
		if err := th.state.ensureStackWithRoots(th, dst+nresults+4, NilValue(), res[:n]); err != nil {
			return err
		}
	} else if err := th.ensureStack(dst + nresults + 4); err != nil {
		return err
	}
	copy(th.stack[dst:dst+n], res[:n])
	if n < nresults {
		fillNil(th.stack[dst+n : dst+nresults])
	}
	th.top = dst + nresults
	return nil
}

// IsYieldable reports whether th can currently yield (matching PUC-Rio lua_isyieldable: th.nny == 0).
func (th *Thread) IsYieldable() bool {
	return th.nny == 0
}

// clearDeadRefs clears all stack slots, frame entries, and transient references on a
// completed or closed coroutine so Go's GC can reclaim discarded arguments, locals,
// and closures immediately in Tier 0 without waiting for State.CollectFull.
// Preserves th.hook so debug.gethook(deadCo) remains faithful to Lua 5.5.1.
func (th *Thread) clearDeadRefs() {
	fillNil(th.stack)
	clear(th.frames)
	th.nframe = 0
	th.top = 0
	clear(th.openUpvals)
	th.openUpvals = nil
	th.tbc = nil
	th.yieldVals = nil
	th.yieldedNative = false
	th.yieldNativeFn = NilValue()
	th.lastErrNative = NilValue()
	th.uncaughtErr = nil
	th.syncCloseErrs = nil
}

// clearUnusedRefs clears stack slots above the live frame/TBC/upvalue watermark and
// unused frame entries on an errored dead coroutine while keeping live frames, locals,
// and TBC variables intact for debug.traceback(co), debug.getlocal(co, ...), and
// deferred coroutine.close(co).
func (th *Thread) clearUnusedRefs() {
	if th.nframe < len(th.frames) {
		clear(th.frames[th.nframe:])
	}
	liveTop := th.top
	for i := range th.nframe {
		cf := &th.frames[i]
		if cf.kind == frameLua && cf.cl != nil {
			if end := cf.base + int(cf.cl.bp.proto.MaxStackSize); end > liveTop {
				liveTop = end
			}
		}
	}
	for _, idx := range th.tbc {
		if idx+1 > liveTop {
			liveTop = idx + 1
		}
	}
	if n := len(th.openUpvals); n > 0 && th.openUpvals[n-1].stackIdx+1 > liveTop {
		liveTop = th.openUpvals[n-1].stackIdx + 1
	}
	if liveTop < len(th.stack) {
		fillNil(th.stack[liveTop:])
	}
	th.detachOpenUpvals()
	th.yieldVals = nil
	th.yieldedNative = false
	th.yieldNativeFn = NilValue()
	th.uncaughtErr = nil
}

// Status returns the current ThreadStatus of th.
func (th *Thread) Status() ThreadStatus {
	if th == nil {
		return ThreadDead
	}
	return th.status
}

// StatusString returns the Lua 5.5 coroutine.status(co) string.
func (th *Thread) StatusString() string {
	if th == th.state.cur {
		return "running"
	}
	switch th.status {
	case ThreadNormal:
		return "normal"
	case ThreadDead:
		return "dead"
	default:
		if th.nframe == 0 && th.status == ThreadOK {
			// Freshly created coroutine (has initial function on stack[0]) or finished main thread.
			if th == th.state.mainThread {
				return "dead"
			}
			if !th.stack[0].IsNil() {
				return "suspended"
			}
			return "dead"
		}
		return "suspended"
	}
}

// errorToLuaValue converts a Go error into a Lua Value, applying Lua 5.5's nil -> "<no error object>".
func errorToLuaValue(s *State, err error) Value {
	if err == nil {
		return NilValue()
	}
	var le *LuaError
	if errors.As(err, &le) {
		if le.Val.IsNil() {
			return StringValue(s.intern.Intern("<no error object>"))
		}
		return le.Val
	}
	return StringValue(s.intern.Intern(err.Error()))
}

func (th *Thread) FormatStatusError() error {
	return fmt.Errorf("%s", FormatValue(th.errObj))
}
