package vm

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/yuchanns/moelua/internal/arena"
)

// ThreadStatus represents the lifecycle state of a Lua 5.5 coroutine / Thread.
type ThreadStatus uint8

const (
	ThreadOK        ThreadStatus = iota // running or ready to start (suspended before first resume)
	ThreadSuspended                     // yielded via coroutine.yield
	ThreadNormal                        // resumed another coroutine
	ThreadDead                          // finished normally, errored, or explicitly closed
)

const (
	thFlagInTwups uint8 = 1 << 0 // true while Thread is registered in State.twups (has open upvalues)
)

// errYieldSentinel is returned internally by run() when coroutine.yield suspends a Thread.
// No Go panic/recover is involved; all frames remain intact on Thread's Arena frame stack.
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

// callFrame is a zero-pointer (noscan, ptrdata == 0) entry on a Thread's flat Arena call stack.
type callFrame struct {
	kind         frameKind
	clRef        arena.ObjRef // Closure ObjRef in State.arena (NullRef for non-Lua frames)
	varargTabRef arena.ObjRef // Table ObjRef for vararg table (NullRef if none)
	base         int          // register R[0] index in Thread stack
	funcIdx      int          // callee slot index in Thread stack
	pc           int          // saved PC
	nresults     int          // expected results (-1 = MULTRET)
	nextraargs   int          // hidden vararg count (Lua 5.5 PF_VAHID)

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
	closeLevel  int
	closeErr    Value
	closeHasErr bool
	closeResume uint8 // 0=continue pc, 1=resume OP_RETURN, 2=resume error unwind, 3=coroutine.close
	closeRetSrc int
	closeRetN   int
	closeEntry  int

	// Debug metadata
	dirty        bool // true if any non-fast-path field was written (cleared lazily on next fast OP_CALL)
	isTailCall   bool
	isHook       bool
	isFinalizer  bool // true when frame was invoked as a __gc finalizer (CIST_FIN -> db.lua:920)
	atCurrentPC  bool // true during line/count hook before instruction executes (db.lua:696)
	topProtected bool // true for host CallProtected frames (rendered as "[C]: in ?" in tracebacks)
	errHandling  bool // true while this frameXPCall's message handler is actively running
	extraArgs    uint8
	oldPC        int // per-frame last traced PC (-1 = fresh frame)
	ftransfer    uint16
	ntransfer    uint16
}

// Thread is a zero-pointer (noscan, ptrdata == 0) Lua 5.5 coroutine / execution thread
// stored in State.arena (ZERO Go goroutines, ZERO Go GC pointer scanning!).
type Thread struct {
	hdr            arena.GCHeader // 8B: Kind=gcKindThread, Flags=thFlag*
	self           arena.ObjRef   // 4B: own ObjRef inside State.arena
	callerRef      arena.ObjRef   // 4B: resuming parent Thread ObjRef (NullRef if none)
	stackRef       arena.ObjRef   // 4B: raw Arena []Value
	framesRef      arena.ObjRef   // 4B: raw Arena []callFrame
	openUpvalsRef  arena.ObjRef   // 4B: raw Arena []arena.ObjRef (ascending stackIdx)
	tbcRef         arena.ObjRef   // 4B: raw Arena []int32 (ascending stackIdx)
	yieldValsRef   arena.ObjRef   // 4B: raw Arena []Value
	syncCloseRef   arena.ObjRef   // 4B: raw Arena []Value
	uncaughtErrRef arena.GoHandle // 8B: GoRefs handle for replacement error escaping protected call

	stackLen      int
	top           int
	highStack     int // high-water mark of written stack slots since last top-level clear
	framesCap     int
	nframe        int
	highFrames    int // high-water mark of written frame slots since last top-level clear
	openUpvalsLen int32
	openUpvalsCap int32
	tbcLen        int32
	tbcCap        int32
	yieldValsLen  int32
	yieldValsCap  int32
	syncCloseLen  int32
	syncCloseCap  int32

	yieldDst      int
	yieldNRes     int
	baseHookCount int
	hookCount     int
	oldPC         int
	stackGen      uint32 // window generation counter for -tags moeluaregcheck verification

	errObj        Value // last error object when dead due to error
	lastErrNative Value // native function that raised an uncaught error on this thread
	yieldNativeFn Value // yielded native callee (for HookMaskRet frameNative on resume)
	hook          Value // active debug hook function

	// Number of non-yieldable C/Go calls active on this thread (matching Lua 5.5 L->nny).
	// mainThread starts with nny=1; new coroutines start with nny=0.
	nny                uint16
	nativeArgsDepth    uint16 // >0 while Go native args slice borrows th.stackRef
	status             ThreadStatus
	hookMask           uint8 // bit 0=call, 1=ret, 2=line, 3=count
	yieldedNative      bool  // true if suspended inside a native call (for HookMaskRet on resume)
	inHook             bool
	inCCallback        bool // true while a native callee is running directly inside callOnThread
	suppressHook       bool // true inside __gc finalizers (matching L->allowhook = 0 without clobbering hookMask)
	inErrHandler       bool // true while running xpcall message handler (allows extra frame headroom)
	closing            bool // true while coroutine.close(co) is actively closing TBC vars on co
	nextFrameFinalizer bool // consumed by next pushLuaFrame to tag __gc finalizer frame (CIST_FIN)
}

// Hook mask bits matching Lua 5.5 lua.h.
const (
	HookMaskCall  uint8 = 1 << 0
	HookMaskRet   uint8 = 1 << 1
	HookMaskLine  uint8 = 1 << 2
	HookMaskCount uint8 = 1 << 3
)

const (
	defaultFrameCap = 32
	maxStackSlots   = 2_000_000
)

func arenaRoundedBytes(n int) int64 {
	if n <= 0 {
		return 0
	}
	_, sz := arena.SizeClassFor(n)
	return int64(sz)
}

func threadInitMemBytes(stackCap, frameCap int) int64 {
	if stackCap < 128 {
		stackCap = 128
	}
	if frameCap < 16 {
		frameCap = defaultFrameCap
	}
	return arenaRoundedBytes(int(unsafe.Sizeof(Thread{}))) +
		arenaRoundedBytes(stackCap*int(unsafe.Sizeof(Value{}))) +
		arenaRoundedBytes(frameCap*int(unsafe.Sizeof(callFrame{})))
}

func (s *State) newThread(stackCap, frameCap int) (*Thread, error) {
	if stackCap < 128 {
		stackCap = 128
	}
	if frameCap < 16 {
		frameCap = defaultFrameCap
	}
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(Thread{})), gcKindThread)
	if err != nil {
		return nil, err
	}
	th := arena.AsSmall[Thread](s.arena, ref)
	th.self = ref
	th.status = ThreadOK
	th.errObj = NilValue()
	th.lastErrNative = NilValue()
	th.yieldNativeFn = NilValue()
	th.hook = NilValue()

	sref, err := s.arena.Alloc(stackCap * int(unsafe.Sizeof(Value{})))
	if err != nil {
		return nil, err
	}
	s.collector.AddDebt(int64(s.arena.SlotSize(sref)))
	fillNil(arena.SliceFast[Value](s.arena, sref, 0, stackCap))
	th.stackRef = sref
	th.stackLen = stackCap

	fref, err := s.arena.AllocZeroed(frameCap * int(unsafe.Sizeof(callFrame{})))
	if err != nil {
		return nil, err
	}
	s.collector.AddDebt(int64(s.arena.SlotSize(fref)))
	th.framesRef = fref
	th.framesCap = frameCap
	return th, nil
}

func (th *Thread) stackSlice(a *arena.Arena) []Value {
	if th.stackRef == arena.NullRef || th.stackLen == 0 {
		return nil
	}
	return arena.SliceFast[Value](a, th.stackRef, 0, th.stackLen)
}

func (th *Thread) framesSlice(a *arena.Arena) []callFrame {
	if th.framesRef == arena.NullRef || th.framesCap == 0 {
		return nil
	}
	return arena.SliceFast[callFrame](a, th.framesRef, 0, th.framesCap)
}

func (th *Thread) openUpvalsSlice(a *arena.Arena) []arena.ObjRef {
	if th.openUpvalsRef == arena.NullRef || th.openUpvalsLen == 0 {
		return nil
	}
	return arena.SliceFast[arena.ObjRef](a, th.openUpvalsRef, 0, int(th.openUpvalsLen))
}

func (th *Thread) tbcSlice(a *arena.Arena) []int32 {
	if th.tbcRef == arena.NullRef || th.tbcLen == 0 {
		return nil
	}
	return arena.SliceFast[int32](a, th.tbcRef, 0, int(th.tbcLen))
}

func (th *Thread) yieldValsSlice(a *arena.Arena) []Value {
	if th.yieldValsRef == arena.NullRef || th.yieldValsLen == 0 {
		return nil
	}
	return arena.SliceFast[Value](a, th.yieldValsRef, 0, int(th.yieldValsLen))
}

func (th *Thread) syncCloseSlice(a *arena.Arena) []Value {
	if th.syncCloseRef == arena.NullRef || th.syncCloseLen == 0 {
		return nil
	}
	return arena.SliceFast[Value](a, th.syncCloseRef, 0, int(th.syncCloseLen))
}

func (th *Thread) setYieldVals(s *State, vals []Value) error {
	if len(vals) == 0 {
		th.yieldValsLen = 0
		return nil
	}
	if len(vals) > int(th.yieldValsCap) {
		newCap := max(int(th.yieldValsCap)*2, len(vals), 4)
		if s.memLimit > 0 {
			var oldBytes int64
			if th.yieldValsRef != arena.NullRef {
				oldBytes = int64(s.arena.SlotSize(th.yieldValsRef))
			}
			delta := arenaRoundedBytes(newCap*int(unsafe.Sizeof(Value{}))) - oldBytes
			if delta < 0 {
				delta = 0
			}
			if err := s.checkMemWithRoots(delta, ThreadValue(th), NilValue(), NilValue()); err != nil {
				return err
			}
		}
		ref, err := s.arena.Alloc(newCap * int(unsafe.Sizeof(Value{})))
		if err != nil {
			return err
		}
		s.collector.AddDebt(int64(s.arena.SlotSize(ref)))
		if th.yieldValsRef != arena.NullRef {
			s.arena.Free(th.yieldValsRef)
		}
		th.yieldValsRef = ref
		th.yieldValsCap = int32(newCap)
	}
	dst := arena.SliceFast[Value](s.arena, th.yieldValsRef, 0, int(th.yieldValsCap))
	copy(dst[:len(vals)], vals)
	th.yieldValsLen = int32(len(vals))
	return nil
}

func (th *Thread) pushSyncCloseErr(s *State, v Value) int {
	idx := int(th.syncCloseLen)
	if idx >= int(th.syncCloseCap) {
		newCap := max(int(th.syncCloseCap)*2, 4)
		ref, err := s.arena.Alloc(newCap * int(unsafe.Sizeof(Value{})))
		if err != nil {
			return -1
		}
		s.collector.AddDebt(int64(s.arena.SlotSize(ref)))
		dst := arena.SliceFast[Value](s.arena, ref, 0, newCap)
		if idx > 0 {
			copy(dst, th.syncCloseSlice(s.arena))
		}
		if th.syncCloseRef != arena.NullRef {
			s.arena.Free(th.syncCloseRef)
		}
		th.syncCloseRef = ref
		th.syncCloseCap = int32(newCap)
	}
	buf := arena.SliceFast[Value](s.arena, th.syncCloseRef, 0, int(th.syncCloseCap))
	buf[idx] = v
	th.syncCloseLen = int32(idx + 1)
	return idx
}

func (th *Thread) setSyncCloseErr(s *State, idx int, v Value) {
	if idx < 0 || th.syncCloseRef == arena.NullRef || idx >= int(th.syncCloseLen) {
		return
	}
	arena.SliceFast[Value](s.arena, th.syncCloseRef, 0, int(th.syncCloseCap))[idx] = v
}

func (th *Thread) popSyncCloseErr(s *State, idx int) {
	if idx < 0 || th.syncCloseRef == arena.NullRef || idx >= int(th.syncCloseLen) {
		return
	}
	arena.SliceFast[Value](s.arena, th.syncCloseRef, 0, int(th.syncCloseCap))[idx] = NilValue()
	th.syncCloseLen = int32(idx)
}

func (th *Thread) uncaughtErr(s *State) error {
	if th.uncaughtErrRef == arena.NullGoHandle {
		return nil
	}
	raw, _ := s.goRefs.Get(th.uncaughtErrRef)
	err, _ := raw.(error)
	return err
}

func (th *Thread) setUncaughtErr(s *State, err error) {
	if th.uncaughtErrRef != arena.NullGoHandle {
		s.goRefs.Free(th.uncaughtErrRef)
		th.uncaughtErrRef = arena.NullGoHandle
	}
	if err != nil {
		th.uncaughtErrRef = s.goRefs.Alloc(err)
	}
}

func (th *Thread) caller(s *State) *Thread {
	if th.callerRef == arena.NullRef {
		return nil
	}
	return arena.AsSmall[Thread](s.arena, th.callerRef)
}

func (th *Thread) noteStack(end int) {
	if end > th.highStack {
		th.highStack = end
	}
}

func (th *Thread) noteFrame(nf int) {
	if nf > th.highFrames {
		th.highFrames = nf
	}
}

func (th *Thread) ensureFrameCap(s *State, need, limit int) error {
	th.noteFrame(need)
	if need <= th.framesCap {
		return nil
	}
	if need > limit {
		return fmt.Errorf("stack overflow")
	}
	newCap := th.framesCap * 2
	if newCap < defaultFrameCap {
		newCap = defaultFrameCap
	}
	for newCap < need {
		newCap *= 2
	}
	if newCap > limit {
		newCap = limit
	}
	if s.memLimit > 0 {
		var oldBytes int64
		if th.framesRef != arena.NullRef {
			oldBytes = int64(s.arena.SlotSize(th.framesRef))
		}
		delta := arenaRoundedBytes(newCap*int(unsafe.Sizeof(callFrame{}))) - oldBytes
		if delta < 0 {
			delta = 0
		}
		if err := s.checkMemWithRoots(delta, ThreadValue(th), NilValue(), NilValue()); err != nil {
			return err
		}
		if need <= th.framesCap {
			return nil
		}
	}
	oldRef := th.framesRef
	oldCap := th.framesCap
	newRef, err := s.arena.AllocZeroed(newCap * int(unsafe.Sizeof(callFrame{})))
	if err != nil {
		return s.memError()
	}
	s.collector.AddDebt(int64(s.arena.SlotSize(newRef)))
	nf := arena.SliceFast[callFrame](s.arena, newRef, 0, newCap)
	if oldRef != arena.NullRef && oldCap > 0 {
		copy(nf, arena.SliceFast[callFrame](s.arena, oldRef, 0, oldCap))
		s.arena.Free(oldRef)
	}
	th.framesRef = newRef
	th.framesCap = newCap
	return nil
}

func (th *Thread) ensureStack(s *State, need int) error {
	if need+stackSlack <= th.stackLen {
		return nil
	}
	limit := maxStackSlots
	if th.inErrHandler {
		limit = maxStackSlots + 8192
	}
	if need < 0 || need+stackSlack > limit {
		return fmt.Errorf("stack overflow")
	}
	newSize := th.stackLen * 2
	if newSize < 128 {
		newSize = 128
	}
	for newSize < need+stackSlack {
		newSize *= 2
	}
	if newSize > limit {
		newSize = limit
	}
	if s.memLimit > 0 {
		delta := arenaRoundedBytes(newSize * int(unsafe.Sizeof(Value{})))
		if th.nativeArgsDepth == 0 && th.stackRef != arena.NullRef {
			delta -= int64(s.arena.SlotSize(th.stackRef))
			if delta < 0 {
				delta = 0
			}
		}
		if err := s.checkMemWithRoots(delta, ThreadValue(th), NilValue(), NilValue()); err != nil {
			return err
		}
		if need+stackSlack <= th.stackLen {
			return nil
		}
	}
	oldRef := th.stackRef
	oldLen := th.stackLen
	newRef, err := s.arena.Alloc(newSize * int(unsafe.Sizeof(Value{})))
	if err != nil {
		return s.memError()
	}
	s.collector.AddDebt(int64(s.arena.SlotSize(newRef)))
	ns := arena.SliceFast[Value](s.arena, newRef, 0, newSize)
	if oldRef != arena.NullRef && oldLen > 0 {
		copy(ns, arena.SliceFast[Value](s.arena, oldRef, 0, oldLen))
	}
	fillNil(ns[oldLen:])
	th.stackRef = newRef
	th.stackLen = newSize
	th.highStack = newSize
	th.stackGen++
	if oldRef != arena.NullRef {
		if th.nativeArgsDepth == 0 {
			s.arena.Free(oldRef)
		} else {
			s.deferredStackFrees = append(s.deferredStackFrees, deferredStackFree{owner: th.self, ref: oldRef, len: oldLen})
		}
	}
	return nil
}

// finishNativeArgs decrements nativeArgsDepth after a borrowed-stack native call.
// If the call grew th.stackRef while args were borrowed and the native returned a slice
// aliasing a deferred old stack buffer owned by th, res is copied before freeing th's
// deferred buffers. Deferred buffers belonging to other threads are preserved until
// those threads finish their own borrowed native calls.
func (th *Thread) finishNativeArgs(s *State, res []Value) []Value {
	th.nativeArgsDepth--
	if th.nativeArgsDepth == 0 && len(s.deferredStackFrees) > 0 {
		return th.finishNativeArgsSlow(s, res)
	}
	return res
}

//go:noinline
func (th *Thread) finishNativeArgsSlow(s *State, res []Value) []Value {
	if len(res) > 0 {
		for _, df := range s.deferredStackFrees {
			if df.owner == th.self && slicesOverlapValues(res, arena.SliceFast[Value](s.arena, df.ref, 0, df.len)) {
				cp := make([]Value, len(res))
				copy(cp, res)
				res = cp
				break
			}
		}
	}
	keep := s.deferredStackFrees[:0]
	for _, df := range s.deferredStackFrees {
		if df.owner == th.self {
			s.arena.Free(df.ref)
		} else {
			keep = append(keep, df)
		}
	}
	s.deferredStackFrees = keep
	return res
}

// window returns a *[256]Value view rooted at th.stack[base].
//
//go:nocheckptr
func (th *Thread) window(a *arena.Arena, base int) *[256]Value {
	return (*[256]Value)(unsafe.Add(a.PtrFast(th.stackRef), base*int(unsafe.Sizeof(Value{}))))
}

// windowIn returns a *[256]Value view rooted at stack[base] without dereferencing Arena.
//
//go:nocheckptr
func windowIn(stack []Value, base int) *[256]Value {
	return (*[256]Value)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(stack)), base*int(unsafe.Sizeof(Value{}))))
}

func (th *Thread) findOrCreateUpval(s *State, stackIdx int) *Upvalue {
	a := s.arena
	n := int(th.openUpvalsLen)
	ups := th.openUpvalsSlice(a)
	i := n
	for i > 0 && int(arena.AsSmall[Upvalue](a, ups[i-1]).stackIdx) > stackIdx {
		i--
	}
	if i > 0 && int(arena.AsSmall[Upvalue](a, ups[i-1]).stackIdx) == stackIdx {
		return arena.AsSmall[Upvalue](a, ups[i-1])
	}
	uvRef, err := s.collector.Alloc(int(unsafe.Sizeof(Upvalue{})), gcKindUpvalue)
	if err != nil {
		s.memExceeded = true
		return nil
	}
	uv := arena.AsSmall[Upvalue](a, uvRef)
	uv.self = uvRef
	uv.ownerRef = th.self
	uv.stackIdx = int32(stackIdx)
	uv.closed = NilValue()

	if n >= int(th.openUpvalsCap) {
		newCap := max(int(th.openUpvalsCap)*2, 4)
		newRef, aerr := a.Alloc(newCap * int(unsafe.Sizeof(arena.ObjRef(0))))
		if aerr != nil {
			uv.ownerRef = arena.NullRef
			s.memExceeded = true
			return nil
		}
		s.collector.AddDebt(int64(a.SlotSize(newRef)))
		dst := arena.SliceFast[arena.ObjRef](a, newRef, 0, newCap)
		if n > 0 {
			copy(dst, ups)
		}
		if th.openUpvalsRef != arena.NullRef {
			a.Free(th.openUpvalsRef)
		}
		th.openUpvalsRef = newRef
		th.openUpvalsCap = int32(newCap)
	}
	buf := arena.SliceFast[arena.ObjRef](a, th.openUpvalsRef, 0, int(th.openUpvalsCap))
	copy(buf[i+1:n+1], buf[i:n])
	buf[i] = uvRef
	th.openUpvalsLen = int32(n + 1)
	if th.hdr.Flags&thFlagInTwups == 0 {
		th.hdr.Flags |= thFlagInTwups
		s.twups = append(s.twups, th.self)
	}
	return uv
}

// closeUpvalObj transitions an open upvalue uv into its closed state holding val,
// preserving both incremental and generational GC write-barrier invariants.
func closeUpvalObj(s *State, uv *Upvalue, val Value) {
	uv.closed = val
	uv.ownerRef = arena.NullRef
	// Open upvalues are kept Gray (never Black) while open. When closing into a
	// closed upvalue, ensure GC traverses uv.closed if uv is already Gray or Old!
	if uv.hdr.IsGray() {
		uv.hdr.SetBlack()
		if val.IsGCObject() {
			s.collector.BarrierBack(uv.self)
		}
	} else if val.IsGCObject() && uv.hdr.IsBlack() {
		s.collector.Barrier(uv.self, val.Ref())
	}
}

func (th *Thread) closeUpvals(s *State, level int) {
	a := s.arena
	n := int(th.openUpvalsLen)
	if n == 0 {
		return
	}
	ups := arena.SliceFast[arena.ObjRef](a, th.openUpvalsRef, 0, int(th.openUpvalsCap))
	stack := th.stackSlice(a)
	for n > 0 {
		uv := arena.AsSmall[Upvalue](a, ups[n-1])
		if int(uv.stackIdx) < level {
			break
		}
		n--
		ups[n] = arena.NullRef
		if uv.ownerRef == th.self {
			val := NilValue()
			if int(uv.stackIdx) < len(stack) {
				val = stack[uv.stackIdx]
			}
			closeUpvalObj(s, uv, val)
		}
	}
	th.openUpvalsLen = int32(n)
}

func (th *Thread) localSlotValue(s *State, slot int) Value {
	return th.stackSlice(s.arena)[slot]
}

func (th *Thread) setLocalSlotValue(s *State, slot int, val Value) {
	th.stackSlice(s.arena)[slot] = val
}

// markTBC marks th.stack[idx] as a <close> to-be-closed variable (OP_TBC / OP_TFORPREP).
func (th *Thread) markTBC(s *State, idx int) error {
	v := th.stackSlice(s.arena)[idx]
	if v.IsFalsy() {
		return nil // nil and false are ignored for <close>
	}
	tm := s.getTMByObj(v, TMClose)
	if tm.IsNil() {
		return &LuaError{Val: StringValue(s.intern.Intern("variable got a non-closable value"))}
	}
	n := int(th.tbcLen)
	if n >= int(th.tbcCap) {
		newCap := max(int(th.tbcCap)*2, 4)
		ref, err := s.arena.Alloc(newCap * 4)
		if err != nil {
			return s.memError()
		}
		s.collector.AddDebt(int64(s.arena.SlotSize(ref)))
		dst := arena.SliceFast[int32](s.arena, ref, 0, newCap)
		if n > 0 {
			copy(dst, th.tbcSlice(s.arena))
		}
		if th.tbcRef != arena.NullRef {
			s.arena.Free(th.tbcRef)
		}
		th.tbcRef = ref
		th.tbcCap = int32(newCap)
	}
	arena.SliceFast[int32](s.arena, th.tbcRef, 0, int(th.tbcCap))[n] = int32(idx)
	th.tbcLen = int32(n + 1)
	return nil
}

// hasTBCAtOrAbove reports whether any <close> variable sits at stack index >= level.
func (th *Thread) hasTBCAtOrAbove(a *arena.Arena, level int) bool {
	if th.tbcLen == 0 {
		return false
	}
	return int(th.tbcSlice(a)[th.tbcLen-1]) >= level
}

// discardTBCAtOrAbove removes pending <close> entries >= level without executing user Lua
// __close callbacks (used when unwinding after an uncatchable context/instruction-limit abort).
func (th *Thread) discardTBCAtOrAbove(s *State, level int) {
	th.closeUpvals(s, level)
	if th.tbcLen == 0 {
		return
	}
	tbc := th.tbcSlice(s.arena)
	stack := th.stackSlice(s.arena)
	for th.tbcLen > 0 && int(tbc[th.tbcLen-1]) >= level {
		idx := int(tbc[th.tbcLen-1])
		th.tbcLen--
		if idx < len(stack) {
			stack[idx] = NilValue()
		}
	}
}

// closeAllSync closes upvalues and all <close> variables >= level synchronously (used on unwind/close).
func (th *Thread) closeAllSync(s *State, level int, errVal Value, hasErr bool) (Value, bool, bool) {
	th.closeUpvals(s, level)
	raised := false
	prevCurRef := s.curRef
	var prevStatus ThreadStatus
	var prevCurPin int
	if prevCurRef != arena.NullRef && prevCurRef != th.self {
		prevCur := arena.AsSmall[Thread](s.arena, prevCurRef)
		prevStatus = prevCur.status
		prevCur.status = ThreadNormal
		prevCurPin = s.Ref(threadRefValue(prevCurRef))
	}
	s.curRef = th.self
	errSlot := th.pushSyncCloseErr(s, errVal)
	defer func() {
		th.popSyncCloseErr(s, errSlot)
		s.curRef = prevCurRef
		if prevCurRef != arena.NullRef && prevCurRef != th.self {
			arena.AsSmall[Thread](s.arena, prevCurRef).status = prevStatus
			s.Unref(prevCurPin)
		}
	}()
	for th.tbcLen > 0 {
		tbc := th.tbcSlice(s.arena)
		if int(tbc[th.tbcLen-1]) < level {
			break
		}
		idx := int(tbc[th.tbcLen-1])
		th.tbcLen--
		stack := th.stackSlice(s.arena)
		obj := stack[idx]
		stack[idx] = NilValue()
		if obj.IsFalsy() {
			continue
		}
		tm := s.getTMByObj(obj, TMClose)
		if tm.IsNil() {
			errVal = StringValue(s.intern.Intern("attempt to call a nil value (metamethod 'close')"))
			th.setSyncCloseErr(s, errSlot, errVal)
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
		if _, err := s.callOnThread(th, tm, args); err != nil {
			if errors.Is(err, errYieldSentinel) {
				return errVal, hasErr, raised
			}
			if errors.Is(err, errCoroCloseSelf) {
				if th.closing {
					if errors.Is(s.abortErr, errCoroCloseSelf) {
						s.abortErr = nil
					}
					continue
				}
				s.abortErr = err
				return errVal, hasErr, raised
			}
			if isUncatchableErr(err) {
				s.abortErr = err
				th.discardTBCAtOrAbove(s, level)
				return errVal, hasErr, raised
			}
			errVal = errorToLuaValue(s, err)
			th.setSyncCloseErr(s, errSlot, errVal)
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
func (th *Thread) pushLuaFrame(s *State, cl *Closure, funcIdx, nargs, nresults int) error {
	limit := maxFrames
	if th.inErrHandler {
		limit = maxFrames + 128
	}
	if err := th.ensureFrameCap(s, th.nframe+1, limit); err != nil {
		return err
	}
	bp := s.boundProtoOf(cl)
	nparams := int(bp.numParams)
	maxStack := int(bp.maxStack)
	base := funcIdx + 1

	if bp.isVararg {
		if err := th.ensureStack(s, base+max(nargs, maxStack)+nparams+8); err != nil {
			return err
		}
		th.top = base + nargs
	} else {
		if err := th.ensureStack(s, base+maxStack+8); err != nil {
			return err
		}
		stack := th.stackSlice(s.arena)
		if nargs < nparams {
			fillNil(stack[base+nargs : base+nparams])
		}
		if maxStack > nparams {
			fillNil(stack[base+nparams : base+maxStack])
		}
		th.top = base + maxStack
	}

	frames := th.framesSlice(s.arena)
	frames[th.nframe] = callFrame{
		kind:        frameLua,
		clRef:       cl.self,
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
	th.noteFrame(th.nframe)
	th.noteStack(max(th.top, base+maxStack))
	return nil
}

func (th *Thread) placeResults(s *State, dst int, res []Value, nresults int) error {
	if nresults < 0 {
		if err := s.ensureStackWithRoots(th, dst+len(res)+4, NilValue(), res); err != nil {
			return err
		}
		stack := th.stackSlice(s.arena)
		copy(stack[dst:dst+len(res)], res)
		th.top = dst + len(res)
		th.noteStack(th.top)
		return nil
	}
	n := min(len(res), nresults)
	if err := s.ensureStackWithRoots(th, dst+nresults+4, NilValue(), res[:n]); err != nil {
		return err
	}
	stack := th.stackSlice(s.arena)
	copy(stack[dst:dst+n], res[:n])
	if n < nresults {
		fillNil(stack[dst+n : dst+nresults])
	}
	th.top = dst + nresults
	th.noteStack(th.top)
	return nil
}

// IsYieldable reports whether th can currently yield (matching PUC-Rio lua_isyieldable: th.nny == 0).
func (th *Thread) IsYieldable() bool {
	return th.nny == 0
}

// clearDeadRefs clears all stack slots, frame entries, and transient references on a
// completed or closed coroutine while preserving th.hook (faithful to Lua 5.5.1).
func (th *Thread) clearDeadRefs(s *State) {
	a := s.arena
	if stack := th.stackSlice(a); len(stack) > 0 {
		fillNil(stack)
	}
	if frames := th.framesSlice(a); len(frames) > 0 {
		clear(frames)
	}
	th.nframe = 0
	th.top = 0
	th.highStack = 0
	th.highFrames = 0
	if th.openUpvalsRef != arena.NullRef {
		a.Free(th.openUpvalsRef)
		th.openUpvalsRef = arena.NullRef
	}
	th.openUpvalsLen = 0
	th.openUpvalsCap = 0
	if th.tbcRef != arena.NullRef {
		a.Free(th.tbcRef)
		th.tbcRef = arena.NullRef
	}
	th.tbcLen = 0
	th.tbcCap = 0
	if th.yieldValsRef != arena.NullRef {
		a.Free(th.yieldValsRef)
		th.yieldValsRef = arena.NullRef
	}
	th.yieldValsLen = 0
	th.yieldValsCap = 0
	if th.syncCloseRef != arena.NullRef {
		a.Free(th.syncCloseRef)
		th.syncCloseRef = arena.NullRef
	}
	th.syncCloseLen = 0
	th.syncCloseCap = 0
	th.yieldedNative = false
	th.yieldNativeFn = NilValue()
	th.lastErrNative = NilValue()
	th.setUncaughtErr(s, nil)
}

// clearUnusedRefs clears stack slots above the live frame/TBC/upvalue watermark and
// unused frame entries on an errored dead coroutine while keeping live frames, locals,
// and TBC variables intact for debug.traceback(co), debug.getlocal(co, ...), and
// deferred coroutine.close(co).
func (th *Thread) clearUnusedRefs(s *State) {
	a := s.arena
	frames := th.framesSlice(a)
	if th.nframe < len(frames) {
		clear(frames[th.nframe:])
	}
	liveTop := th.top
	for i := range th.nframe {
		cf := &frames[i]
		if cf.kind == frameLua && cf.clRef != arena.NullRef {
			cl := arena.AsSmall[Closure](a, cf.clRef)
			if end := cf.base + int(s.boundProtoOf(cl).maxStack); end > liveTop {
				liveTop = end
			}
		}
	}
	for _, idx := range th.tbcSlice(a) {
		if int(idx)+1 > liveTop {
			liveTop = int(idx) + 1
		}
	}
	if ups := th.openUpvalsSlice(a); len(ups) > 0 {
		lastIdx := int(arena.AsSmall[Upvalue](a, ups[len(ups)-1]).stackIdx)
		if lastIdx+1 > liveTop {
			liveTop = lastIdx + 1
		}
	}
	stack := th.stackSlice(a)
	if liveTop < len(stack) {
		fillNil(stack[liveTop:])
	}
	th.highStack = liveTop
	th.highFrames = th.nframe
	if th.yieldValsRef != arena.NullRef {
		a.Free(th.yieldValsRef)
		th.yieldValsRef = arena.NullRef
	}
	th.yieldValsLen = 0
	th.yieldValsCap = 0
	th.yieldedNative = false
	th.yieldNativeFn = NilValue()
	th.setUncaughtErr(s, nil)
}

// Status returns the current ThreadStatus of th.
func (th *Thread) Status() ThreadStatus {
	if th == nil {
		return ThreadDead
	}
	return th.status
}

// StatusString returns the Lua 5.5 coroutine.status(co) string.
func (th *Thread) StatusString(s *State) string {
	if th.self == s.curRef {
		return "running"
	}
	switch th.status {
	case ThreadNormal:
		return "normal"
	case ThreadDead:
		return "dead"
	default:
		if th.nframe == 0 && th.status == ThreadOK {
			if th.self == s.mainThreadRef {
				return "dead"
			}
			stack := th.stackSlice(s.arena)
			if len(stack) > 0 && !stack[0].IsNil() {
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

func (th *Thread) FormatStatusError(s *State) error {
	return fmt.Errorf("%s", s.FormatValue(th.errObj))
}
