package vm

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"unsafe"
	"weak"
)

// Two-Tier GC Semantics Engine ("Go frees, VM decides reachability"):
//
//   - Tier 0 (default): State has never seen "__gc" or "__mode".
//     100% pure Go GC, ZERO extra GC overhead.
//   - Tier 1 (lazily activated on first rawset of "__gc" or "__mode" into any table):
//     Deterministic VM-side mark-only tracer (per-object gcMark uint64 + explicit gray stack,
//     NO second write barrier!) running at collectgarbage() / safepoints in exact PUC-Rio lgc.c
//     atomic() 6-step order:
//       1. convergeEphemerons
//       2. clearByValues (weak-v / all-weak) -> BEFORE __gc!
//       3. separateToBeFnz (strict LIFO order)
//       4. markBeingFnz + propagate (resurrection -> keeps weak keys alive DURING __gc!)
//       5. convergeEphemerons + clearByKeys (ephemeron / all-weak)
//       6. clearByValues again (from orig weak/all-weak)
//     Then synchronously drains __gc finalizers on the VM thread.

type gcFinalEntry struct {
	obj Value // *Table or *Userdata
	seq uint64
}

type gcWeakFileEntry struct {
	u      weak.Pointer[Userdata]
	strong *Userdata // non-nil when fileMeta.__gc was customized so Go GC cannot drop the userdata before VM __gc runs
	fi     *fileInner
	seq    uint64
}

func (wf gcWeakFileEntry) userdata() *Userdata {
	if wf.strong != nil {
		return wf.strong
	}
	return wf.u.Value()
}

type gcMode uint8

const (
	gcModeIncremental gcMode = iota
	gcModeGenerational
)

type gcState struct {
	tier1           bool // true once __gc or __mode has been observed in this State
	hasDeadHashKeys bool // true once a string or collectable hash key has been deleted since last full trace
	epoch           uint64
	running       bool
	inGCTM        bool // true while draining __gc finalizers (prevents re-entrant collectgarbage)
	closing       bool // true once State.Close() has begun (new __gc objects registered during close are ignored)
	mode          gcMode
	allocBytes    int64 // logical VM-accounted bytes for collectgarbage("count")
	tempBytes     int64 // transient Go-side buffer bytes (e.g. load(reader)) held across calls
	lastLiveBytes int64 // live bytes measured at end of last full trace (proportional Tier 1 pacing)
	debt          int64
	params        [6]byte // encoded via luaGCParamCode: minormul, majorminor, minormajor, pause, stepmul, stepsize

	// Shared monotonic sequence for strict LIFO finalizer order across tables, userdata, and files.
	finalSeq uint64
	// True once Lua has written fileMeta.__gc; retains open file userdata strongly until VM __gc runs.
	customFileGC bool
	// Objects marked for finalization at setmetatable time (ascending seq; merged in reverse LIFO).
	finalizable []gcFinalEntry
	// Weakly-tracked standard file userdata so Go GC + runtime.AddCleanup reclaims discarded
	// handles automatically in Tier 0, while explicit collectgarbage()/Close() closes live ones.
	weakFiles []gcWeakFileEntry
	// Queue of unreachable objects pending __gc invocation in LIFO order.
	toBeFnz []Value
	// Host-pinned references (analog of luaL_ref) so Tier 1 marker keeps host-held Lua values alive.
	pinnedRefs map[int]Value
	nextRefID  int
	// Transient shape pinned while table slot growth checks memory limits (may trigger emergency GC).
	pinnedShape *Shape
}

const (
	gcParamMinorMul = iota
	gcParamMajorMinor
	gcParamMinorMajor
	gcParamPause
	gcParamStepMul
	gcParamStepSize
)

// luaGCParamCode implements Lua 5.5.1 lobject.c:luaO_codeparam (4-bit exponent + 4-bit mantissa).
func luaGCParamCode(p uint64) byte {
	const maxParam = (uint64(0x1F) << (0xF - 7 - 1)) * 100
	if p >= maxParam {
		return 0xFF
	}
	q := (p*128 + 99) / 100
	if q < 0x10 {
		return byte(q)
	}
	log := uint(bits.Len64(q) - 5)
	return byte(((q >> log) - 0x10) | (uint64(log+1) << 4))
}

// luaGCParamApply implements Lua 5.5.1 lobject.c:luaO_applyparam.
func luaGCParamApply(p byte, x int64) int64 {
	m := int64(p & 0xF)
	e := int(p >> 4)
	if e > 0 {
		e--
		m += 0x10
	}
	e -= 7
	if e >= 0 {
		if x < (math.MaxInt64/0x1F)>>uint(e) {
			return (x * m) << uint(e)
		}
		return math.MaxInt64
	}
	sh := uint(-e)
	if x < math.MaxInt64/0x1F {
		return (x * m) >> sh
	}
	if (x >> sh) < math.MaxInt64/0x1F {
		return (x >> sh) * m
	}
	return math.MaxInt64
}

func newGCState() gcState {
	return gcState{
		running:       true,
		mode:          gcModeIncremental,
		allocBytes:    4096,
		lastLiveBytes: 4096,
		params: [6]byte{
			luaGCParamCode(20),   // minormul
			luaGCParamCode(50),   // majorminor
			luaGCParamCode(70),   // minormajor
			luaGCParamCode(250),  // pause
			luaGCParamCode(200),  // stepmul
			luaGCParamCode(9600), // stepsize (200 * sizeof(Table) on 64-bit)
		},
		nextRefID: 1,
	}
}

// gcAddDebt adds bytes to the Tier 1 automatic GC pacing debt counter without
// triggering a collection mid-operation (call gcCheckDebt at a safe point once
// newly allocated objects are rooted on the thread stack).
func (s *State) gcAddDebt(bytes int64) {
	if !s.gc.tier1 || bytes <= 0 || !s.gc.running || s.gc.inGCTM {
		return
	}
	s.gc.debt += bytes
}

func (s *State) gcDebtThreshold() int64 {
	pause := luaGCParamApply(s.gc.params[gcParamPause], 100)
	threshold := int64(65536)
	if pause > 100 {
		if prop := s.gc.lastLiveBytes * (pause - 100) / 100; prop > threshold {
			threshold = prop
		}
	}
	return threshold
}

// gcNeedsCheck reports whether gcCheckDebt would trigger a Tier-1 collection or drain finalizers.
func (s *State) gcNeedsCheck() bool {
	if !s.gc.tier1 || !s.gc.running || s.gc.inGCTM {
		return false
	}
	return s.gc.debt >= s.gcDebtThreshold() || len(s.gc.toBeFnz) > 0
}

// gcCheckDebt triggers an automatic Tier 1 GC cycle (or drains pending finalizers)
// when accumulated allocation debt reaches the pause threshold.
// liveTop matches PUC-Rio lvm.c's checkGC(L, ra+1): only stack[0..liveTop) is live
// so dead temporaries above R[a] are collected.
func (s *State) gcCheckDebt(liveTop int) {
	if !s.gc.tier1 || !s.gc.running || s.gc.inGCTM {
		return
	}
	if s.gc.debt >= s.gcDebtThreshold() {
		s.gc.debt = 0
		if s.cur != nil && liveTop > 0 {
			savedTop := s.cur.top
			s.cur.top = liveTop
			s.CollectFull()
			if savedTop > s.cur.top {
				s.cur.top = savedTop
			}
			return
		}
		s.CollectFull()
		return
	}
	if len(s.gc.toBeFnz) > 0 {
		s.drainFinalizers()
	}
}

// gcRecordAlloc accounts allocations in Tier 1 mode and triggers automatic GC cycles
// so loops allocating tables/strings/closures fire weak clearing and __gc finalizers
// (required by closure.lua:39, gc.lua:88-100, db.lua:926). Zero cost in Tier 0!
func (s *State) gcRecordAlloc(bytes int64, liveTop int) {
	if !s.gc.tier1 {
		return
	}
	s.gc.allocBytes += bytes
	s.gcAddDebt(bytes)
	s.gcCheckDebt(liveTop)
}

// activateTier1 flips this State into deterministic Tier 1 GC semantics.
func (s *State) activateTier1() {
	s.gc.tier1 = true
}

// promoteFileFinalizers switches open file userdata tracking from weak-only to strong retention
// once Lua customizes fileMeta.__gc, ensuring Go GC cannot discard userdata before VM __gc runs.
func (s *State) promoteFileFinalizers() {
	s.activateTier1()
	if s.gc.customFileGC {
		return
	}
	s.gc.customFileGC = true
	for i := range s.gc.weakFiles {
		wf := &s.gc.weakFiles[i]
		if wf.strong == nil {
			if u := wf.u.Value(); u != nil && !u.gcInFnz && !u.gcFnzed {
				wf.strong = u
			}
		}
	}
}

// sweepCoallocTables clears outgoing Go references from unreachable stdlib tables/hashes
// embedded in stateAlloc so Go GC can reclaim values stored in removed stdlib tables
// (including values written in-place into shared template slabs).
func (s *State) sweepCoallocTables(epoch uint64) {
	for i := range s.coallocTables {
		t := &s.coallocTables[i]
		if t.gcMark == epoch {
			continue
		}
		if len(t.slots) > 0 {
			clear(t.slots)
		}
		if t.arrPtr != nil && t.arrKind == arrGeneric && t.arrCap > 0 {
			clear(unsafe.Slice((*Value)(t.arrPtr), t.arrCap))
		}
		if t.hash != nil {
			if len(t.hash.nodes) > 0 {
				clear(t.hash.nodes)
			}
			*t.hash = tableHash{}
		}
		*t = Table{}
		if i < len(s.coallocHashes) {
			h := &s.coallocHashes[i]
			if len(h.nodes) > 0 {
				clear(h.nodes)
			}
			*h = tableHash{}
		}
	}
}

// registerFinalizable records obj (Table or Userdata) for __gc finalization in LIFO order
// when setmetatable attaches a metatable containing "__gc" (matching lgc.c:luaC_checkfinalizer).
func (s *State) registerFinalizable(obj Value, mt *Table) {
	if mt == nil || s.gc.closing {
		return
	}
	if mt.RawGetStr(s.intern.tms[TMGC]).IsNil() {
		return
	}
	s.activateTier1()
	switch {
	case obj.IsTable():
		t := obj.AsTable()
		if t.flags&tblFlagGCPending != 0 {
			return // already separated into toBeFnz; matches lgc.c:luaC_checkfinalizer FINALIZEDBIT
		}
		if t.flags&tblFlagGCFnzed != 0 {
			// Re-arming inside __gc (tracegc.lua / gc.lua)!
			t.flags &^= tblFlagGCFnzed
		}
		if t.flags&tblFlagGCInFnz == 0 {
			t.flags |= tblFlagGCInFnz
			s.gc.finalSeq++
			s.gc.finalizable = append(s.gc.finalizable, gcFinalEntry{obj: obj, seq: s.gc.finalSeq})
		}
	case obj.IsUserdata():
		u := obj.AsUserdata()
		if u.gcPending {
			return
		}
		if u.gcFnzed {
			u.gcFnzed = false
		}
		if !u.gcInFnz {
			u.gcInFnz = true
			s.gc.finalSeq++
			s.gc.finalizable = append(s.gc.finalizable, gcFinalEntry{obj: obj, seq: s.gc.finalSeq})
		}
	}
}

// registerWeakCandidate activates Tier 1 GC if mt has a __mode field.
func (s *State) registerWeakCandidate(t, mt *Table) {
	if mt == nil {
		return
	}
	if !mt.RawGetStr(s.intern.tms[TMMode]).IsNil() {
		s.activateTier1()
		t.flags |= tblFlagGCWeakCand
	}
}

// Ref pins a Lua Value so Tier 1 GC keeps it reachable from the Go host (like luaL_ref).
func (s *State) Ref(v Value) int {
	if s.gc.pinnedRefs == nil {
		s.gc.pinnedRefs = make(map[int]Value)
	}
	id := s.gc.nextRefID
	s.gc.nextRefID++
	s.gc.pinnedRefs[id] = v
	return id
}

// Unref releases a host-pinned reference.
func (s *State) Unref(id int) {
	delete(s.gc.pinnedRefs, id)
}

func (s *State) weakModeOf(t *Table) (weakK, weakV bool) {
	if t.meta == nil {
		return false, false
	}
	mv := t.meta.RawGetStr(s.intern.tms[TMMode])
	if !mv.IsString() {
		return false, false
	}
	ms := mv.AsString().S
	cancelFn := s.nativeCancelFunc()
	if nul, err := indexByteWithCancel(ms, 0, cancelFn); err != nil {
		return false, false
	} else if nul >= 0 {
		ms = ms[:nul]
	}
	wk, err := containsByteWithCancel(ms, 'k', cancelFn)
	if err != nil {
		return false, false
	}
	wv, err := containsByteWithCancel(ms, 'v', cancelFn)
	if err != nil {
		return false, false
	}
	return wk, wv
}

// isCollectableWeak reports whether v can be cleared from a weak table.
// Per Lua 5.5 spec (lgc.c:223-230, lobject.h, gc.lua:246): strings, numbers, bools, nil,
// lightud, and light C functions (immutable stdlib builtins) are NEVER weak!
// Lua closures and dynamic C closures (coroutine.wrap, gmatch, io.lines, utf8.codes) ARE collectable.
func isCollectableWeak(v Value) bool {
	if v.IsNative() {
		return v.AsNative().Collectable
	}
	return v.IsTable() || v.IsClosure() || v.IsUserdata() || v.IsThread()
}

func gcObjPtr(v Value) (unsafe.Pointer, bool) {
	switch {
	case v.IsTable():
		return unsafe.Pointer(v.AsTable()), true
	case v.IsClosure():
		return unsafe.Pointer(v.AsClosure()), true
	case v.IsNative():
		nf := v.AsNative()
		if nf.Collectable {
			return unsafe.Pointer(nf), true
		}
	case v.IsUserdata():
		return unsafe.Pointer(v.AsUserdata()), true
	case v.IsThread():
		return unsafe.Pointer(v.AsThread()), true
	}
	return nil, false
}

type gcTracer struct {
	s                *State
	epoch            uint64
	preserveFrames   bool // true when triggered mid-opcode (e.g. checkMem) where s.cur.top may be stale
	countStrings     bool // true when s.memLimit > 0 so live strings are counted accurately in liveBytes
	trackDeadStrings bool // true when deleted hash keys exist so unreachable dead string keys can be released
	cancelFn         func() error
	cancelSteps      int64
	aborted          bool
	seenStrings      map[*LuaString]struct{}
	seenProtos       map[*boundProto]struct{}
	seenShapes       map[*Shape]struct{}
	seenShapeIDs     map[uint64]struct{}
	gray             []Value
	hashedTables     []*Table // all marked tables with t.hash != nil (for dead hash-key clearing)
	weakVals         []*Table // __mode="v"
	ephemerons       []*Table // __mode="k"
	allWeaks         []*Table // __mode="kv"
	ephemDeps        map[unsafe.Pointer][]Value
	ephemReady       []Value
	ephemScanned     int
	liveOpenUpvals   []*Upvalue
	liveBytes        int64
}

func (tr *gcTracer) tickCancel() bool {
	if tr.aborted {
		return true
	}
	if tr.cancelFn == nil {
		return false
	}
	tr.cancelSteps++
	if (tr.cancelSteps & 4095) == 0 {
		if err := tr.cancelFn(); err != nil {
			tr.aborted = true
			return true
		}
	}
	return false
}

func (tr *gcTracer) unlockEphemDeps(ptr unsafe.Pointer) {
	if len(tr.ephemDeps) == 0 {
		return
	}
	if deps, ok := tr.ephemDeps[ptr]; ok {
		tr.ephemReady = append(tr.ephemReady, deps...)
		delete(tr.ephemDeps, ptr)
	}
}

func (tr *gcTracer) markShape(sh *Shape) {
	root := tr.s.shapes.root
	for cur := sh; cur != nil && !cur.frozen; cur = cur.parent {
		if _, ok := tr.seenShapes[cur]; ok {
			return
		}
		if tr.seenShapes == nil {
			tr.seenShapes = make(map[*Shape]struct{}, 32)
			tr.seenShapeIDs = make(map[uint64]struct{}, 32)
		}
		tr.seenShapes[cur] = struct{}{}
		tr.seenShapeIDs[cur.id] = struct{}{}
		if cur == root {
			if cur.transMap != nil {
				tr.liveBytes += 48 + int64(len(cur.transMap))*32
			}
			continue
		}
		tr.liveBytes += shapeNodeBytes(cur)
		if (tr.countStrings || tr.trackDeadStrings) && cur.key != nil {
			tr.mark(StringValue(cur.key))
		}
	}
}

func (tr *gcTracer) markBoundProto(bp *boundProto) {
	if bp == nil {
		return
	}
	if _, ok := tr.seenProtos[bp]; ok {
		return
	}
	if tr.seenProtos == nil {
		tr.seenProtos = make(map[*boundProto]struct{}, 32)
	}
	tr.seenProtos[bp] = struct{}{}
	tr.liveBytes += protoOwnBytes(bp.proto, len(bp.ic) > 0)
	if tr.countStrings || tr.trackDeadStrings {
		for _, ls := range bp.strConsts {
			if ls != nil {
				tr.mark(StringValue(ls))
			}
		}
	}
	for _, sub := range bp.sub {
		tr.markBoundProto(sub)
	}
}

func (tr *gcTracer) markRoots() {
	s := tr.s
	tr.mark(TableValue(s.env))
	if s.registry != nil {
		tr.mark(TableValue(s.registry))
	}
	if s.pkgTbl != nil {
		tr.mark(TableValue(s.pkgTbl))
	}
	for _, mt := range s.typeMetas {
		if mt != nil {
			tr.mark(TableValue(mt))
		}
	}
	if s.fileMeta != nil {
		tr.mark(TableValue(s.fileMeta))
	}
	if !s.stdinInput.IsNil() {
		tr.mark(s.stdinInput)
	}
	if !s.defaultInput.IsNil() {
		tr.mark(s.defaultInput)
	}
	if !s.defaultOutput.IsNil() {
		tr.mark(s.defaultOutput)
	}
	tr.mark(ThreadValue(s.mainThread))
	if s.cur != nil && s.cur != s.mainThread {
		tr.mark(ThreadValue(s.cur))
	}
	for _, v := range s.gc.pinnedRefs {
		if tr.cancelFn != nil && tr.tickCancel() {
			return
		}
		tr.mark(v)
	}
	for _, v := range s.gc.toBeFnz {
		if tr.cancelFn != nil && tr.tickCancel() {
			return
		}
		tr.mark(v)
	}
	if s.gc.pinnedShape != nil {
		tr.markShape(s.gc.pinnedShape)
	}
}

func (tr *gcTracer) isMarked(v Value) bool {
	if !isCollectableWeak(v) {
		return true // primitives & strings are always "marked" (never cleared from weak tables)
	}
	switch {
	case v.IsTable():
		return v.AsTable().gcMark == tr.epoch
	case v.IsClosure():
		return v.AsClosure().gcMark == tr.epoch
	case v.IsNative():
		return v.AsNative().gcMark == tr.epoch
	case v.IsUserdata():
		return v.AsUserdata().gcMark == tr.epoch
	case v.IsThread():
		return v.AsThread().gcMark == tr.epoch
	default:
		return true
	}
}

func (tr *gcTracer) mark(v Value) {
	if v.IsString() {
		if tr.countStrings || tr.trackDeadStrings {
			ls := v.AsString()
			if _, ok := tr.seenStrings[ls]; !ok {
				if tr.seenStrings == nil {
					tr.seenStrings = make(map[*LuaString]struct{}, 64)
				}
				tr.seenStrings[ls] = struct{}{}
				if tr.countStrings {
					tr.liveBytes += 48 + int64(len(ls.S))
				}
			}
		}
		return
	}
	if !isCollectableWeak(v) {
		return
	}
	switch {
	case v.IsTable():
		t := v.AsTable()
		if t.gcMark == tr.epoch {
			return
		}
		t.gcMark = tr.epoch
		tr.unlockEphemDeps(unsafe.Pointer(t))
		var arrBytes, nodeBytes, slotBytes int64
		switch t.arrKind {
		case arrInt, arrFloat:
			arrBytes = int64(t.arrCap) * 8
		case arrBool:
			arrBytes = int64(t.arrCap)
		case arrGeneric:
			arrBytes = int64(t.arrCap) * 16
		}
		if t.hash != nil {
			nodeBytes = int64(len(t.hash.nodes)) * 40
		}
		if c := cap(t.slots); c > len(t.inlineSlots) {
			slotBytes = int64(c) * 16
		}
		if t.shape != nil {
			tr.markShape(t.shape)
		}
		tr.liveBytes += 96 + slotBytes + nodeBytes + arrBytes
		tr.gray = append(tr.gray, v)
	case v.IsClosure():
		c := v.AsClosure()
		if c.gcMark == tr.epoch {
			return
		}
		c.gcMark = tr.epoch
		tr.unlockEphemDeps(unsafe.Pointer(c))
		tr.liveBytes += 64 + int64(len(c.upvals)*24)
		tr.markBoundProto(c.bp)
		tr.gray = append(tr.gray, v)
	case v.IsNative():
		nf := v.AsNative()
		if nf.gcMark == tr.epoch {
			return
		}
		nf.gcMark = tr.epoch
		tr.unlockEphemDeps(unsafe.Pointer(nf))
		tr.liveBytes += 48 + int64(len(nf.upvals))*16
		if len(nf.upvals) > 0 {
			tr.gray = append(tr.gray, v)
		}
	case v.IsUserdata():
		u := v.AsUserdata()
		if u.gcMark == tr.epoch {
			return
		}
		u.gcMark = tr.epoch
		tr.unlockEphemDeps(unsafe.Pointer(u))
		tr.liveBytes += 64 + int64((len(u.UValues)+len(u.gcRefs))*16)
		if lf, ok := u.Data.(*luaFile); ok {
			tr.liveBytes += lf.memBytes()
		}
		tr.gray = append(tr.gray, v)
	case v.IsThread():
		th := v.AsThread()
		if th.gcMark == tr.epoch {
			return
		}
		th.gcMark = tr.epoch
		tr.unlockEphemDeps(unsafe.Pointer(th))
		tr.liveBytes += 512 + int64(cap(th.stack))*16 + int64(cap(th.frames))*int64(unsafe.Sizeof(callFrame{}))
		tr.gray = append(tr.gray, v)
	}
}

func (tr *gcTracer) propagateAll() {
	for len(tr.gray) > 0 {
		if tr.tickCancel() {
			return
		}
		v := tr.gray[len(tr.gray)-1]
		tr.gray = tr.gray[:len(tr.gray)-1]
		switch {
		case v.IsTable():
			tr.traverseTable(v.AsTable())
			if tr.aborted {
				return
			}
		case v.IsClosure():
			c := v.AsClosure()
			for _, uv := range c.upvals {
				if uv == nil {
					continue
				}
				if uv.owner == nil {
					tr.mark(uv.closed)
				} else {
					// Per lgc.c:remarkupvals, mark open upvalue's slot value WITHOUT marking
					// uv.owner so an unreachable coroutine can still be collected while live closures
					// keep their captured values intact (closed after Step 4 if owner remains unmarked).
					tr.mark(uv.Get())
					tr.liveOpenUpvals = append(tr.liveOpenUpvals, uv)
				}
			}
		case v.IsNative():
			nf := v.AsNative()
			for _, uv := range nf.upvals {
				tr.mark(uv)
			}
		case v.IsUserdata():
			u := v.AsUserdata()
			if u.Meta != nil {
				tr.mark(TableValue(u.Meta))
			}
			for _, uv := range u.UValues {
				if tr.cancelFn != nil && tr.tickCancel() {
					return
				}
				tr.mark(uv)
			}
			for _, rv := range u.gcRefs {
				tr.mark(rv)
			}
		case v.IsThread():
			tr.traverseThread(v.AsThread())
			if tr.aborted {
				return
			}
		}
	}
}

func (tr *gcTracer) traverseThread(th *Thread) {
	// Mirror lgc.c:traversethread literally: mark only stack[0..th.top) and nil-fill stack[th.top:)!
	for i := range th.nframe {
		if tr.cancelFn != nil && tr.tickCancel() {
			return
		}
		cf := &th.frames[i]
		if cf.cl != nil {
			tr.mark(ClosureValue(cf.cl))
		}
		tr.mark(cf.errFunc)
		tr.mark(cf.closeErr)
		tr.mark(cf.nativeFn)
		if cf.varargTab != nil {
			tr.mark(TableValue(cf.varargTab))
		}
	}
	tr.mark(th.errObj)
	tr.mark(th.lastErrNative)
	tr.mark(th.yieldNativeFn)
	tr.mark(th.hook)
	if th.caller != nil {
		tr.mark(ThreadValue(th.caller))
	}
	for _, yv := range th.yieldVals {
		tr.mark(yv)
	}
	for _, ce := range th.syncCloseErrs {
		tr.mark(ce)
	}
	// Clear stale frame slots above nframe so dead closures/errFuncs/tables are never pinned.
	if th.nframe < len(th.frames) {
		clear(th.frames[th.nframe:])
	}
	lim := th.top
	maxFrameEnd := lim
	for i := range th.nframe {
		cf := &th.frames[i]
		if cf.cl != nil {
			if end := cf.base + int(cf.cl.bp.proto.MaxStackSize); end > maxFrameEnd {
				maxFrameEnd = end
			}
		}
	}
	if tr.preserveFrames && maxFrameEnd > lim {
		lim = maxFrameEnd
	}
	if lim > len(th.stack) {
		lim = len(th.stack)
	}
	for i := 0; i < lim; i++ {
		if tr.cancelFn != nil && tr.tickCancel() {
			return
		}
		tr.mark(th.stack[i])
	}
	if lim < len(th.stack) {
		fillNil(th.stack[lim:])
	}
	maxOpenUpvalIdx := -1
	for _, uv := range th.openUpvals {
		if tr.cancelFn != nil && tr.tickCancel() {
			return
		}
		if uv.owner == nil {
			tr.mark(uv.closed)
		} else if uv.stackIdx < len(th.stack) {
			tr.mark(th.stack[uv.stackIdx])
		}
		if uv.stackIdx > maxOpenUpvalIdx {
			maxOpenUpvalIdx = uv.stackIdx
		}
	}
	// Shrink oversized stacks on non-running coroutines (including coroutines with open upvalues,
	// since Upvalue tracks stackIdx rather than raw slice pointers) & release finished dead stacks.
	if th != tr.s.cur && th != tr.s.mainThread {
		if th.status == ThreadDead && th.nframe == 0 && th.top == 0 && len(th.openUpvals) == 0 && len(th.tbc) == 0 {
			if len(th.stack) > 8 {
				th.stack = []Value{NilValue()}
				th.stackGen++
			}
			if len(th.frames) > 0 {
				th.frames = nil
			}
		} else if th.status == ThreadSuspended {
			keepEnd := max(lim, maxFrameEnd)
			if maxOpenUpvalIdx+1 > keepEnd {
				keepEnd = maxOpenUpvalIdx + 1
			}
			if len(th.tbc) > 0 && th.tbc[len(th.tbc)-1]+1 > keepEnd {
				keepEnd = th.tbc[len(th.tbc)-1] + 1
			}
			needed := keepEnd + stackSlack
			if needed < 128 {
				needed = 128
			}
			if len(th.stack) > needed*4 && len(th.stack) > 2048 {
				shrunk := make([]Value, needed)
				copyLen := min(keepEnd, len(th.stack))
				copy(shrunk, th.stack[:copyLen])
				fillNil(shrunk[copyLen:])
				th.stack = shrunk
				th.stackGen++
			}
		}
	}
}

func (tr *gcTracer) traverseTable(t *Table) {
	if t.meta != nil {
		tr.mark(TableValue(t.meta))
	}
	if t.hash != nil {
		tr.hashedTables = append(tr.hashedTables, t)
	}
	weakK, weakV := tr.s.weakModeOf(t)
	if !weakK && !weakV {
		// Strong table: mark all keys and values.
		if t.arrKind == arrGeneric && t.arrLen > 0 {
			for _, v := range t.arrValSlice() {
				if tr.cancelFn != nil && tr.tickCancel() {
					return
				}
				tr.mark(v)
			}
		}
		for _, v := range t.slots {
			if tr.cancelFn != nil && tr.tickCancel() {
				return
			}
			tr.mark(v)
		}
		if t.hash != nil {
			nodes := t.hash.nodes
			for i := range nodes {
				if tr.cancelFn != nil && tr.tickCancel() {
					return
				}
				nd := &nodes[i]
				if nd.used && !nd.dead && !nd.val.IsNil() {
					tr.mark(nd.key)
					tr.mark(nd.val)
				}
			}
		}
		return
	}
	if weakK && weakV {
		tr.allWeaks = append(tr.allWeaks, t)
		return
	}
	if weakV {
		// Weak values, strong keys: mark all keys now; defer value clearing.
		tr.weakVals = append(tr.weakVals, t)
		if t.hash != nil {
			nodes := t.hash.nodes
			for i := range nodes {
				if tr.cancelFn != nil && tr.tickCancel() {
					return
				}
				nd := &nodes[i]
				if nd.used && !nd.dead && !nd.val.IsNil() {
					tr.mark(nd.key)
				}
			}
		}
		return
	}
	// Ephemeron table (__mode = "k"): mark values whose keys are non-collectable or already marked!
	tr.ephemerons = append(tr.ephemerons, t)
	// Dense array & shape slots have non-collectable keys (int / string) -> always strong!
	if t.arrKind == arrGeneric && t.arrLen > 0 {
		for _, v := range t.arrValSlice() {
			if tr.cancelFn != nil && tr.tickCancel() {
				return
			}
			tr.mark(v)
		}
	}
	for _, v := range t.slots {
		if tr.cancelFn != nil && tr.tickCancel() {
			return
		}
		tr.mark(v)
	}
	if t.hash != nil {
		nodes := t.hash.nodes
		for i := range nodes {
			if tr.cancelFn != nil && tr.tickCancel() {
				return
			}
			nd := &nodes[i]
			if nd.used && !nd.dead && !nd.val.IsNil() {
				if tr.isMarked(nd.key) {
					tr.mark(nd.val)
				}
			}
		}
	}
}

func (tr *gcTracer) convergeEphemerons() {
	for !tr.aborted {
		// Scan any newly discovered ephemeron tables once into the key -> dependent-values worklist.
		for tr.ephemScanned < len(tr.ephemerons) {
			t := tr.ephemerons[tr.ephemScanned]
			tr.ephemScanned++
			if t.hash == nil {
				continue
			}
			nodes := t.hash.nodes
			for i := range nodes {
				if tr.tickCancel() {
					return
				}
				nd := &nodes[i]
				if !nd.used || nd.dead || nd.val.IsNil() {
					continue
				}
				if tr.isMarked(nd.key) {
					if !tr.isMarked(nd.val) {
						tr.mark(nd.val)
					}
				} else if isCollectableWeak(nd.val) && !tr.isMarked(nd.val) {
					if kptr, ok := gcObjPtr(nd.key); ok {
						if tr.ephemDeps == nil {
							tr.ephemDeps = make(map[unsafe.Pointer][]Value, 16)
						}
						tr.ephemDeps[kptr] = append(tr.ephemDeps[kptr], nd.val)
					}
				}
			}
		}
		for len(tr.ephemReady) > 0 && !tr.aborted {
			if tr.tickCancel() {
				return
			}
			v := tr.ephemReady[len(tr.ephemReady)-1]
			tr.ephemReady = tr.ephemReady[:len(tr.ephemReady)-1]
			if !tr.isMarked(v) {
				tr.mark(v)
			}
		}
		if len(tr.gray) == 0 {
			break
		}
		tr.propagateAll()
	}
}

func (tr *gcTracer) clearByValues(tables []*Table) {
	cleared := false
	defer func() {
		if cleared {
			tr.s.protoEpoch++
		}
	}()
	for _, t := range tables {
		if t.arrKind == arrGeneric && t.arrLen > 0 {
			arr := t.arrValSlice()
			for i := range arr {
				if tr.cancelFn != nil && tr.tickCancel() {
					return
				}
				if !arr[i].IsNil() && !tr.isMarked(arr[i]) {
					arr[i] = NilValue()
					cleared = true
				}
			}
		}
		for i := range t.slots {
			if tr.cancelFn != nil && tr.tickCancel() {
				return
			}
			if !t.slots[i].IsNil() && !tr.isMarked(t.slots[i]) {
				t.slots[i] = NilValue()
				cleared = true
			}
		}
		if h := t.hash; h != nil {
			nodes := h.nodes
			for i := range nodes {
				if tr.cancelFn != nil && tr.tickCancel() {
					return
				}
				nd := &nodes[i]
				if nd.used && !nd.dead && !nd.val.IsNil() && !tr.isMarked(nd.val) {
					h.markDead(i)
					cleared = true
				}
			}
		}
	}
}

func (tr *gcTracer) clearByKeys(tables []*Table) {
	cleared := false
	defer func() {
		if cleared {
			tr.s.protoEpoch++
		}
	}()
	for _, t := range tables {
		if h := t.hash; h != nil {
			nodes := h.nodes
			for i := range nodes {
				if tr.cancelFn != nil && tr.tickCancel() {
					return
				}
				nd := &nodes[i]
				if nd.used && !nd.dead && !nd.val.IsNil() && !tr.isMarked(nd.key) {
					// Key is provably unreachable; clear both key and val so Go's GC reclaims the key immediately.
					nd.key = NilValue()
					nd.val = NilValue()
					nd.dead = true
					h.live--
					h.dictGen++
					cleared = true
				}
			}
		}
	}
}

func (tr *gcTracer) accountWeakStrings() {
	if !tr.countStrings && !tr.trackDeadStrings {
		return
	}
	markTableStrings := func(tables []*Table) {
		for _, t := range tables {
			if t.arrKind == arrGeneric && t.arrLen > 0 {
				for _, v := range t.arrValSlice() {
					if tr.cancelFn != nil && tr.tickCancel() {
						return
					}
					if v.IsString() {
						tr.mark(v)
					}
				}
			}
			for _, v := range t.slots {
				if tr.cancelFn != nil && tr.tickCancel() {
					return
				}
				if v.IsString() {
					tr.mark(v)
				}
			}
			if t.hash != nil {
				nodes := t.hash.nodes
				for i := range nodes {
					if tr.cancelFn != nil && tr.tickCancel() {
						return
					}
					nd := &nodes[i]
					if nd.used && !nd.dead && !nd.val.IsNil() {
						if nd.key.IsString() {
							tr.mark(nd.key)
						}
						if nd.val.IsString() {
							tr.mark(nd.val)
						}
					}
				}
			}
		}
	}
	markTableStrings(tr.weakVals)
	if tr.aborted {
		return
	}
	markTableStrings(tr.ephemerons)
	if tr.aborted {
		return
	}
	markTableStrings(tr.allWeaks)
}

// clearDeadHashKeys releases strong Go references held by dead/tombstoned hash keys
// that are no longer reachable from anywhere in the VM (matching lgc.c:clearkey),
// while preserving live tombstoned keys (e.g. active pairs() traversal) and hash probe chains.
func (tr *gcTracer) clearDeadHashKeys() bool {
	anyDead := false
	for _, t := range tr.hashedTables {
		nodes := t.hash.nodes
		for i := range nodes {
			if tr.cancelFn != nil && tr.tickCancel() {
				return true
			}
			nd := &nodes[i]
			if !nd.used || !nd.dead || nd.key.IsNil() {
				continue
			}
			if nd.key.IsString() {
				if tr.countStrings || tr.trackDeadStrings {
					if _, live := tr.seenStrings[nd.key.AsString()]; !live {
						nd.key = NilValue()
						continue
					}
				}
				anyDead = true
			} else if isCollectableWeak(nd.key) {
				if !tr.isMarked(nd.key) {
					nd.key = NilValue()
					continue
				}
				anyDead = true
			}
		}
		if len(t.hash.deadKeys) > 0 {
			for slot, ref := range t.hash.deadKeys {
				if tr.cancelFn != nil && tr.tickCancel() {
					return true
				}
				v, alive := deadKeyValue(ref)
				if !alive {
					delete(t.hash.deadKeys, slot)
					continue
				}
				if v.IsString() {
					if tr.countStrings || tr.trackDeadStrings {
						if _, live := tr.seenStrings[v.AsString()]; !live {
							delete(t.hash.deadKeys, slot)
							continue
						}
					}
					anyDead = true
				} else if isCollectableWeak(v) {
					if !tr.isMarked(v) {
						delete(t.hash.deadKeys, slot)
						continue
					}
					anyDead = true
				}
			}
			if len(t.hash.deadKeys) == 0 {
				t.hash.deadKeys = nil
			}
		}
	}
	return anyDead
}

func (tr *gcTracer) icValAlive(v Value) bool {
	if v.IsString() {
		if tr.countStrings || tr.trackDeadStrings {
			_, live := tr.seenStrings[v.AsString()]
			return live
		}
		return true
	}
	if isCollectableWeak(v) {
		return tr.isMarked(v)
	}
	return true
}

func (tr *gcTracer) icShapeAlive(sh *Shape) bool {
	if sh == nil {
		return true
	}
	if sh.meta != nil && sh.meta.gcMark != tr.epoch {
		return false
	}
	if !sh.frozen {
		if _, live := tr.seenShapes[sh]; !live {
			return false
		}
	}
	return true
}

func (tr *gcTracer) icShapeIDAlive(id uint64) bool {
	if id == 0 {
		return false
	}
	if id <= tr.s.shapes.frozenMaxID {
		return true
	}
	_, live := tr.seenShapeIDs[id]
	return live
}

func (tr *gcTracer) icPrimaryAlive(ic *ICEntry) bool {
	switch ic.kind {
	case icEmpty:
		return false
	case icEnvSlot:
		h := ic.holder.Value()
		if h == nil || h.gcMark != tr.epoch || h.hash == nil || h.hash.dictGen != ic.epoch {
			return false
		}
		return true
	case icProtoVal:
		if !ic.noMeta && ic.epoch != tr.s.protoEpoch {
			return false
		}
		if !tr.icShapeIDAlive(ic.shapeID) {
			return false
		}
		v, ok := ic.protoValue()
		return ok && tr.icValAlive(v)
	case icAddSlot:
		if !ic.noMeta && ic.epoch != tr.s.protoEpoch {
			return false
		}
		ts := ic.targetShape.Value()
		return tr.icShapeIDAlive(ic.shapeID) && ts != nil && tr.icShapeAlive(ts)
	case icOwnSlot:
		if !ic.noMeta && ic.epoch != tr.s.protoEpoch {
			return false
		}
		return tr.icShapeIDAlive(ic.shapeID)
	default:
		return false
	}
}

func (tr *gcTracer) icPolyAlive(pe *polyEntry) bool {
	switch pe.kind {
	case icEmpty:
		return false
	case icProtoVal:
		if !pe.noMeta && pe.epoch != tr.s.protoEpoch {
			return false
		}
		if !tr.icShapeIDAlive(pe.shapeID) {
			return false
		}
		v, ok := pe.protoValue()
		return ok && tr.icValAlive(v)
	case icAddSlot:
		if !pe.noMeta && pe.epoch != tr.s.protoEpoch {
			return false
		}
		ts := pe.targetShape.Value()
		return tr.icShapeIDAlive(pe.shapeID) && ts != nil && tr.icShapeAlive(ts)
	case icOwnSlot:
		if !pe.noMeta && pe.epoch != tr.s.protoEpoch {
			return false
		}
		return tr.icShapeIDAlive(pe.shapeID)
	default:
		return false
	}
}

// sweepInlineCaches clears invalidated or unreachable references from monomorphic and
// polymorphic inline caches on all reachable boundProtos so IC entries act as weak caches
// and never pin dead strings, tables, shapes, or closures across GC cycles.
func (tr *gcTracer) sweepInlineCaches() {
	for bp := range tr.seenProtos {
		ics := bp.ic
		for i := range ics {
			if tr.cancelFn != nil && tr.tickCancel() {
				return
			}
			ic := &ics[i]
			if ic.kind == icEmpty && ic.poly == nil {
				continue
			}
			// Filter polymorphic entries first.
			if ic.poly != nil {
				kept := 0
				for j := range int(ic.polyCount) {
					pe := &ic.poly[j]
					if tr.icPolyAlive(pe) {
						if kept != j {
							ic.poly[kept] = *pe
						}
						kept++
					}
				}
				for j := kept; j < int(ic.polyCount); j++ {
					ic.poly[j] = polyEntry{}
				}
				ic.polyCount = uint8(kept)
				if kept == 0 {
					ic.poly = nil
				}
			}
			if !tr.icPrimaryAlive(ic) {
				if ic.poly != nil && ic.polyCount > 0 {
					// Promote first surviving poly entry to primary slot.
					pe := ic.poly[0]
					for j := 1; j < int(ic.polyCount); j++ {
						ic.poly[j-1] = ic.poly[j]
					}
					ic.poly[ic.polyCount-1] = polyEntry{}
					ic.polyCount--
					if ic.polyCount == 0 {
						ic.poly = nil
					}
					ic.shapeID = pe.shapeID
					ic.targetShape = pe.targetShape
					ic.holder = ICEntry{}.holder
					ic.val = pe.val
					ic.weakCl = pe.weakCl
					ic.weakVal = pe.weakVal
					ic.weakKind = pe.weakKind
					ic.epoch = pe.epoch
					ic.slot = pe.slot
					ic.kind = pe.kind
					ic.noMeta = pe.noMeta
					ic.protoAbsent = pe.protoAbsent
				} else {
					*ic = ICEntry{}
				}
			}
		}
	}
}

// CollectFull executes a full deterministic Tier 1 GC cycle (exact lgc.c atomic() 6-step order)
// and drains all newly separated __gc finalizers synchronously on the VM thread.
func (s *State) CollectFull() {
	s.collectFullImpl(false, false)
}

// collectEmergency runs a mark-and-recount emergency GC pass during memory-limit pressure:
// conservatively preserves all active frame register windows and NEVER invokes __gc finalizers
// or mutates weak tables mid-allocation (matching PUC-Rio lgc.c emergency collector semantics).
func (s *State) collectEmergency() {
	s.collectFullImpl(true, true)
}

// recountLiveBytes recomputes s.gc.allocBytes from the State's existing live object graph
// without running __gc finalizers, clearing weak entries, or mutating caches. Used when
// SetMemoryLimit transitions from unlimited (0) to limited (> 0) so pre-existing stdlib
// and Tier-0 allocations are accounted immediately before the next allocation check.
func (s *State) recountLiveBytes() {
	s.gc.epoch++
	if s.gc.epoch == 0 {
		s.gc.epoch = 1
	}
	tr := &gcTracer{
		s:              s,
		epoch:          s.gc.epoch,
		preserveFrames: true,
		countStrings:   true,
	}
	tr.markRoots()
	tr.propagateAll()
	tr.convergeEphemerons()
	for _, fe := range s.gc.finalizable {
		if fe.obj.IsTable() && fe.obj.AsTable().flags&tblFlagGCFnzed != 0 {
			continue
		}
		if fe.obj.IsUserdata() && fe.obj.AsUserdata().gcFnzed {
			continue
		}
		if !tr.isMarked(fe.obj) {
			tr.mark(fe.obj)
		}
	}
	for _, wf := range s.gc.weakFiles {
		if u := wf.userdata(); u != nil && !u.gcFnzed {
			obj := UserdataValue(u)
			if !tr.isMarked(obj) {
				tr.mark(obj)
			}
		}
	}
	tr.propagateAll()
	tr.convergeEphemerons()
	tr.accountWeakStrings()
	s.gc.allocBytes = tr.liveBytes
	s.gc.lastLiveBytes = tr.liveBytes
}

func (s *State) collectFullImpl(preserveFrames bool, emergency bool) {
	if s.gc.inGCTM && !emergency {
		return // non-reentrant inside __gc (gc.lua:698-705), except emergency GC under memLimit
	}
	countStr := s.memLimit > 0
	trackDeadStr := s.gc.hasDeadHashKeys
	cancelFn := s.nativeCancelFunc()
	if emergency || (!s.gc.tier1 && len(s.gc.finalizable) == 0 && len(s.gc.weakFiles) == 0) {
		// Tier 0 (with no open finalizable handles) or emergency allocation-time recount:
		// nil-fill dead stack/frame slack and recompute exact live VM bytes without running
		// __gc finalizers mid-operation!
		if s.cur != nil {
			if s.cur.nframe < len(s.cur.frames) {
				clear(s.cur.frames[s.cur.nframe:])
			}
			if s.cur.top < len(s.cur.stack) {
				lim := s.cur.top
				for i := range s.cur.nframe {
					cf := &s.cur.frames[i]
					if cf.cl != nil {
						if end := cf.base + int(cf.cl.bp.proto.MaxStackSize); end > lim {
							lim = end
						}
					}
				}
				if lim < len(s.cur.stack) {
					fillNil(s.cur.stack[lim:])
				}
			}
		}
		s.gc.epoch++
		if s.gc.epoch == 0 {
			s.gc.epoch = 1
		}
		tr := &gcTracer{
			s:                s,
			epoch:            s.gc.epoch,
			preserveFrames:   preserveFrames,
			countStrings:     countStr,
			trackDeadStrings: trackDeadStr,
			cancelFn:         cancelFn,
		}
		tr.markRoots()
		if tr.aborted {
			return
		}
		tr.propagateAll()
		if tr.aborted {
			return
		}
		if emergency && (len(tr.ephemerons) > 0 || len(tr.weakVals) > 0 || len(tr.allWeaks) > 0 || len(s.gc.finalizable) > 0 || len(s.gc.weakFiles) > 0) {
			// Match PUC-Rio lgc.c:luaC_fullgc(L, isemergency=1): clear unreachable weak entries
			// so weak caches cannot pin gigabytes when collectgarbage("stop") is active, while
			// deferring __gc execution and keeping pending-finalizer objects accounted in liveBytes.
			tr.convergeEphemerons()
			if tr.aborted {
				return
			}
			tr.clearByValues(tr.weakVals)
			if tr.aborted {
				return
			}
			tr.clearByValues(tr.allWeaks)
			if tr.aborted {
				return
			}
			for _, fe := range s.gc.finalizable {
				if tr.cancelFn != nil && tr.tickCancel() {
					return
				}
				if fe.obj.IsTable() && fe.obj.AsTable().flags&tblFlagGCFnzed != 0 {
					continue
				}
				if fe.obj.IsUserdata() && fe.obj.AsUserdata().gcFnzed {
					continue
				}
				if !tr.isMarked(fe.obj) {
					tr.mark(fe.obj)
				}
			}
			for _, wf := range s.gc.weakFiles {
				if tr.cancelFn != nil && tr.tickCancel() {
					return
				}
				if u := wf.userdata(); u != nil && !u.gcFnzed {
					obj := UserdataValue(u)
					if !tr.isMarked(obj) {
						tr.mark(obj)
					}
				}
			}
			tr.propagateAll()
			if tr.aborted {
				return
			}
			tr.convergeEphemerons()
			if tr.aborted {
				return
			}
			tr.clearByKeys(tr.ephemerons)
			if tr.aborted {
				return
			}
			tr.clearByKeys(tr.allWeaks)
			if tr.aborted {
				return
			}
			tr.clearByValues(tr.weakVals)
			if tr.aborted {
				return
			}
			tr.clearByValues(tr.allWeaks)
			if tr.aborted {
				return
			}
		}
		tr.accountWeakStrings()
		if tr.aborted {
			return
		}
		s.gc.hasDeadHashKeys = tr.clearDeadHashKeys()
		if tr.aborted {
			return
		}
		tr.sweepInlineCaches()
		if tr.aborted {
			return
		}
		// Close open upvalues belonging to threads that remain unreachable, so dead
		// coroutines are unpinned for Go's GC even in Tier 0 / emergency GC paths.
		for _, uv := range tr.liveOpenUpvals {
			if uv.owner != nil && uv.owner.gcMark != tr.epoch {
				uv.closed = uv.owner.stack[uv.stackIdx]
				uv.owner = nil
			}
		}
		s.sweepCoallocTables(tr.epoch)
		s.intern.Prune()
		s.shapes.Prune()
		s.gc.allocBytes = tr.liveBytes
		s.gc.lastLiveBytes = tr.liveBytes
		return
	}

	s.gc.epoch++
	if s.gc.epoch == 0 {
		s.gc.epoch = 1
	}
	tr := &gcTracer{
		s:                s,
		epoch:            s.gc.epoch,
		preserveFrames:   preserveFrames,
		countStrings:     countStr,
		trackDeadStrings: trackDeadStr,
		cancelFn:         cancelFn,
	}

	// Mark all VM roots.
	tr.markRoots()
	if tr.aborted {
		return
	}
	tr.propagateAll()
	if tr.aborted {
		return
	}

	// Step 1: Converge ephemerons before clearing weak values.
	tr.convergeEphemerons()
	if tr.aborted {
		return
	}

	// Step 2: clearByValues BEFORE __gc separation (weak values are nil inside __gc!).
	tr.clearByValues(tr.weakVals)
	if tr.aborted {
		return
	}
	tr.clearByValues(tr.allWeaks)
	if tr.aborted {
		return
	}

	// Step 3: separateToBeFnz in STRICT REVERSE (LIFO) registration order across tables,
	// userdata, and file handles using the shared monotonic registration sequence!
	var kept []gcFinalEntry
	var keptFiles []gcWeakFileEntry
	var newlySep []Value
	fi := len(s.gc.finalizable) - 1
	wi := len(s.gc.weakFiles) - 1
	for fi >= 0 || wi >= 0 {
		if tr.cancelFn != nil && tr.tickCancel() {
			return
		}
		if wi >= 0 && (fi < 0 || s.gc.weakFiles[wi].seq > s.gc.finalizable[fi].seq) {
			wf := s.gc.weakFiles[wi]
			wi--
			u := wf.userdata()
			if u == nil {
				// Go GC already collected the userdata; ensure flush+close completes synchronously now.
				if wf.fi != nil {
					_ = wf.fi.flushAndClose()
				}
				continue
			}
			if u.gcInFnz || u.gcFnzed {
				continue
			}
			obj := UserdataValue(u)
			if !tr.isMarked(obj) {
				newlySep = append(newlySep, obj)
			} else {
				keptFiles = append(keptFiles, wf)
			}
			continue
		}
		entry := s.gc.finalizable[fi]
		fi--
		obj := entry.obj
		if !tr.isMarked(obj) {
			newlySep = append(newlySep, obj)
		} else {
			kept = append(kept, entry)
		}
	}
	// Commit finalization flags only after separation completes without cancellation
	// so an aborted collection never drops pending table/userdata/file finalizers.
	for _, obj := range newlySep {
		switch {
		case obj.IsTable():
			t := obj.AsTable()
			t.flags = (t.flags | tblFlagGCFnzed | tblFlagGCPending) &^ tblFlagGCInFnz
		case obj.IsUserdata():
			u := obj.AsUserdata()
			u.gcFnzed = true
			u.gcPending = true
			u.gcInFnz = false
		}
	}
	// Restore ascending registration order in kept and keptFiles (since we merged backwards).
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	for i, j := 0, len(keptFiles)-1; i < j; i, j = i+1, j-1 {
		keptFiles[i], keptFiles[j] = keptFiles[j], keptFiles[i]
	}
	s.gc.finalizable = kept
	s.gc.weakFiles = keptFiles
	s.gc.toBeFnz = append(s.gc.toBeFnz, newlySep...)

	// Step 4: markBeingFnz + propagate (resurrection: keeps dying object & its refs alive during __gc!).
	for _, obj := range s.gc.toBeFnz {
		if tr.cancelFn != nil && tr.tickCancel() {
			return
		}
		tr.mark(obj)
	}
	tr.propagateAll()
	if tr.aborted {
		return
	}

	// Step 5: convergeEphemerons + clearByKeys (weak keys of finalizing objects stay alive during __gc!).
	tr.convergeEphemerons()
	if tr.aborted {
		return
	}
	tr.clearByKeys(tr.ephemerons)
	if tr.aborted {
		return
	}
	tr.clearByKeys(tr.allWeaks)
	if tr.aborted {
		return
	}

	// Step 6: clearByValues again on ALL weak-value/all-weak tables (including any resurrected in Step 4, gc.lua:444!).
	tr.clearByValues(tr.weakVals)
	if tr.aborted {
		return
	}
	tr.clearByValues(tr.allWeaks)
	if tr.aborted {
		return
	}

	// Strings are value-like in Lua 5.5 and survive weak clearing; account for surviving strings
	// in weak table positions before sweeping dead hash keys and updating allocBytes.
	tr.accountWeakStrings()
	if tr.aborted {
		return
	}

	// Release strong Go pointers from unreachable dead/tombstoned hash keys (matching lgc.c:clearkey)
	// and sweep invalidated/unreachable inline cache entries.
	s.gc.hasDeadHashKeys = tr.clearDeadHashKeys()
	if tr.aborted {
		return
	}
	tr.sweepInlineCaches()
	if tr.aborted {
		return
	}

	// Close open upvalues belonging to threads that remain unreachable after Step 4 resurrection,
	// so live closures keep their captured values and dead threads are unpinned for Go's GC.
	for _, uv := range tr.liveOpenUpvals {
		if uv.owner != nil && uv.owner.gcMark != tr.epoch {
			uv.closed = uv.owner.stack[uv.stackIdx]
			uv.owner = nil
		}
	}

	s.sweepCoallocTables(tr.epoch)
	s.intern.Prune()
	s.shapes.Prune()
	s.gc.allocBytes = tr.liveBytes
	s.gc.lastLiveBytes = tr.liveBytes
	s.gc.debt = 0

	// Drain all queued __gc finalizers synchronously on the VM thread.
	s.drainFinalizers()
	s.compactOpenFiles()
}

func (s *State) runFinalizerObj(th *Thread, obj Value) bool {
	var mt *Table
	switch {
	case obj.IsTable():
		t := obj.AsTable()
		t.flags &^= tblFlagGCPending
		mt = t.meta
	case obj.IsUserdata():
		u := obj.AsUserdata()
		u.gcPending = false
		mt = u.Meta
	}
	if mt == nil {
		return true
	}
	tm := mt.RawGetStr(s.intern.tms[TMGC])
	if tm.IsNil() {
		return true
	}
	if !tm.IsClosure() && !tm.IsNative() && s.getTMByObj(tm, TMCall).IsNil() {
		s.emitWarning(fmt.Sprintf("error in __gc (attempt to call a %s value (metamethod '__gc'))", s.objTypeName(tm)))
		return true
	}
	th.nextFrameFinalizer = true
	_, err := s.callOnThread(th, tm, []Value{obj})
	th.nextFrameFinalizer = false
	if err != nil {
		if errors.Is(err, errCoroCloseSelf) || isUncatchableErr(err) {
			s.abortErr = err
			return false
		}
		var le *LuaError
		if errors.As(err, &le) {
			if le.Val.IsString() {
				s.emitWarning(fmt.Sprintf("error in __gc (%s)", le.Val.AsString().S))
			} else {
				s.emitWarning("error in __gc (error object is not a string)")
			}
		} else {
			s.emitWarning(fmt.Sprintf("error in __gc (%s)", err.Error()))
		}
	}
	return true
}

func (s *State) drainFinalizers() {
	if s.gc.inGCTM || len(s.gc.toBeFnz) == 0 {
		return
	}
	s.gc.inGCTM = true
	// Suppress debug hooks (without clearing hookMask so debug.gethook/sethook still work)
	// and forbid coroutine.yield while running __gc (lgc.c:GCTM: L->allowhook = 0).
	th := s.cur
	savedSuppress := false
	if th != nil {
		savedSuppress = th.suppressHook
		th.suppressHook = true
		th.nny++
	}
	defer func() {
		if th != nil {
			th.nny--
			th.suppressHook = savedSuppress
		}
		s.gc.inGCTM = false
	}()

	for len(s.gc.toBeFnz) > 0 {
		obj := s.gc.toBeFnz[0]
		s.gc.toBeFnz[0] = Value{}
		s.gc.toBeFnz = s.gc.toBeFnz[1:]
		if !s.runFinalizerObj(th, obj) {
			return
		}
	}
	s.gc.toBeFnz = nil
}

// Close closes the State: closes remaining mainThread <close> variables, runs ALL remaining
// __gc finalizers in strict LIFO order regardless of reachability (matching lua_close /
// gc.lua:653-668 / main.lua:333-360,491-501), flushes/closes all open files, and clears
// host-pinned references once top-level finalization finishes.
func (s *State) Close() {
	if !s.gc.closing {
		// Close any active <close> TBC variables on mainThread first (e.g. os.exit(true, true)).
		if s.mainThread != nil {
			s.mainThread.nframe = 0
			s.mainThread.nny++
			_, _, _ = s.mainThread.closeAllSync(0, NilValue(), false)
			s.mainThread.nny--
		}
		s.gc.closing = true
		// Merge all remaining finalizable objects and file handles into toBeFnz in strict LIFO order.
		fi := len(s.gc.finalizable) - 1
		wi := len(s.gc.weakFiles) - 1
		for fi >= 0 || wi >= 0 {
			if wi >= 0 && (fi < 0 || s.gc.weakFiles[wi].seq > s.gc.finalizable[fi].seq) {
				wf := s.gc.weakFiles[wi]
				wi--
				u := wf.userdata()
				if u == nil {
					if wf.fi != nil {
						_ = wf.fi.flushAndClose()
					}
					continue
				}
				if u.gcInFnz || u.gcFnzed {
					continue
				}
				u.gcFnzed = true
				u.gcPending = true
				s.gc.toBeFnz = append(s.gc.toBeFnz, UserdataValue(u))
				continue
			}
			obj := s.gc.finalizable[fi].obj
			fi--
			switch {
			case obj.IsTable():
				t := obj.AsTable()
				t.flags = (t.flags | tblFlagGCFnzed | tblFlagGCPending) &^ tblFlagGCInFnz
			case obj.IsUserdata():
				u := obj.AsUserdata()
				u.gcFnzed = true
				u.gcPending = true
				u.gcInFnz = false
			}
			s.gc.toBeFnz = append(s.gc.toBeFnz, obj)
		}
		s.gc.finalizable = nil
		s.gc.weakFiles = nil
	}
	if s.gc.inGCTM {
		// Re-entrant Close (e.g. os.exit(0, true) inside a __gc finalizer, main.lua:349):
		// drain any remaining queued finalizers in LIFO order while keeping inGCTM active!
		s.drainFinalizersQueued()
	} else {
		s.drainFinalizers()
	}
	s.closeOpenFiles()
	if !s.gc.inGCTM {
		s.gc.pinnedRefs = nil
	}
}

func (s *State) drainFinalizersQueued() {
	th := s.cur
	for len(s.gc.toBeFnz) > 0 {
		obj := s.gc.toBeFnz[0]
		s.gc.toBeFnz[0] = Value{}
		s.gc.toBeFnz = s.gc.toBeFnz[1:]
		if !s.runFinalizerObj(th, obj) {
			return
		}
	}
	s.gc.toBeFnz = nil
}

var gcParamNames = map[string]int{
	"minormul":   gcParamMinorMul,
	"majorminor": gcParamMajorMinor,
	"minormajor": gcParamMinorMajor,
	"pause":      gcParamPause,
	"stepmul":    gcParamStepMul,
	"stepsize":   gcParamStepSize,
}

func nativeCollectGarbage(s *State, args []Value) ([]Value, error) {
	opt := "collect"
	if len(args) >= 1 && !args[0].IsNil() {
		if !args[0].IsString() {
			return nil, fmt.Errorf("bad argument #1 to 'collectgarbage' (string expected, got %s)", s.objTypeName(args[0]))
		}
		opt = args[0].AsString().S
	}
	switch opt {
	case "collect":
		if s.gc.inGCTM {
			return []Value{NilValue()}, nil
		}
		s.CollectFull()
		if s.abortErr != nil {
			return nil, s.abortErr
		}
		return []Value{IntValue(0)}, nil
	case "stop":
		if s.gc.inGCTM {
			return []Value{NilValue()}, nil
		}
		s.gc.running = false
		return []Value{IntValue(0)}, nil
	case "restart":
		if s.gc.inGCTM {
			return []Value{NilValue()}, nil
		}
		s.gc.running = true
		return []Value{IntValue(0)}, nil
	case "count":
		if s.gc.inGCTM {
			return []Value{NilValue()}, nil
		}
		kb := float64(s.gc.allocBytes) / 1024.0
		return []Value{FloatValue(kb)}, nil
	case "step":
		if len(args) >= 2 && !args[1].IsNil() {
			_, ok, nonInt, err := s.coerceIntKind(args[1])
			if err != nil {
				return nil, err
			}
			if !ok {
				if nonInt {
					return nil, fmt.Errorf("bad argument #2 to 'collectgarbage' (number has no integer representation)")
				}
				return nil, fmt.Errorf("bad argument #2 to 'collectgarbage' (number expected, got %s)", s.objTypeName(args[1]))
			}
		}
		if s.gc.inGCTM {
			return []Value{NilValue()}, nil
		}
		if s.gc.tier1 {
			s.CollectFull()
			if s.abortErr != nil {
				return nil, s.abortErr
			}
		}
		return []Value{TrueValue()}, nil
	case "isrunning":
		if s.gc.inGCTM {
			return []Value{NilValue()}, nil
		}
		return []Value{BoolValue(s.gc.running)}, nil
	case "incremental":
		if s.gc.inGCTM {
			return []Value{NilValue()}, nil
		}
		prev := "incremental"
		if s.gc.mode == gcModeGenerational {
			prev = "generational"
		}
		s.gc.mode = gcModeIncremental
		return []Value{StringValue(s.intern.Intern(prev))}, nil
	case "generational":
		if s.gc.inGCTM {
			return []Value{NilValue()}, nil
		}
		prev := "incremental"
		if s.gc.mode == gcModeGenerational {
			prev = "generational"
		}
		s.gc.mode = gcModeGenerational
		return []Value{StringValue(s.intern.Intern(prev))}, nil
	case "param":
		if len(args) < 2 || !args[1].IsString() {
			got := "no value"
			if len(args) >= 2 {
				got = s.objTypeName(args[1])
			}
			return nil, fmt.Errorf("bad argument #2 to 'collectgarbage' (string expected, got %s)", got)
		}
		idx, ok := gcParamNames[args[1].AsString().S]
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'collectgarbage' (invalid option '%s')", args[1].AsString().S)
		}
		nv := int64(-1)
		if len(args) >= 3 && !args[2].IsNil() {
			var ok, nonInt bool
			var err error
			nv, ok, nonInt, err = s.coerceIntKind(args[2])
			if err != nil {
				return nil, err
			}
			if !ok {
				if nonInt {
					return nil, fmt.Errorf("bad argument #3 to 'collectgarbage' (number has no integer representation)")
				}
				return nil, fmt.Errorf("bad argument #3 to 'collectgarbage' (number expected, got %s)", s.objTypeName(args[2]))
			}
		}
		if s.gc.inGCTM {
			return []Value{NilValue()}, nil
		}
		old := luaGCParamApply(s.gc.params[idx], 100)
		if nv >= 0 {
			s.gc.params[idx] = luaGCParamCode(uint64(nv))
		}
		return []Value{IntValue(old)}, nil
	default:
		return nil, fmt.Errorf("bad argument #1 to 'collectgarbage' (invalid option '%s')", opt)
	}
}
