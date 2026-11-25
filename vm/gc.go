package vm

import (
	"errors"
	"fmt"
	"strings"
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
}

type gcWeakFileEntry struct {
	u  weak.Pointer[Userdata]
	fi *fileInner
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
	params      [6]int // minormul, majorminor, minormajor, pause, stepmul, stepsize

	// Objects marked for finalization at setmetatable time (registration order; scanned in reverse LIFO).
	finalizable []gcFinalEntry
	// Weakly-tracked standard file userdata so Go GC + runtime.AddCleanup reclaims discarded
	// handles automatically in Tier 0, while explicit collectgarbage()/Close() closes live ones.
	weakFiles []gcWeakFileEntry
	// Queue of unreachable objects pending __gc invocation in LIFO order.
	toBeFnz []Value
	// Host-pinned references (analog of luaL_ref) so Tier 1 marker keeps host-held Lua values alive.
	pinnedRefs map[int]Value
	nextRefID  int
}

const (
	gcParamMinorMul = iota
	gcParamMajorMinor
	gcParamMinorMajor
	gcParamPause
	gcParamStepMul
	gcParamStepSize
)

func newGCState() gcState {
	return gcState{
		running:       true,
		mode:          gcModeIncremental,
		allocBytes:    4096,
		lastLiveBytes: 4096,
		params:        [6]int{20, 50, 70, 250, 200, 13}, // Lua 5.5.1 defaults
		nextRefID:     1,
	}
}

// gcRecordAlloc accounts allocations in Tier 1 mode and triggers automatic GC cycles
// so loops allocating tables/strings/closures fire weak clearing and __gc finalizers
// (required by closure.lua:39, gc.lua:88-100, db.lua:926). Zero cost in Tier 0!
// liveTop matches PUC-Rio lvm.c's checkGC(L, ra+1): only stack[0..liveTop) is live
// so dead temporaries above R[a] (e.g. from discarded setmetatable({}) calls) are collected!
func (s *State) gcRecordAlloc(bytes int64, liveTop int) {
	if !s.gc.tier1 {
		return
	}
	s.gc.allocBytes += bytes
	if !s.gc.running || s.gc.inGCTM {
		return
	}
	s.gc.debt += bytes
	pause := s.gc.params[gcParamPause]
	threshold := int64(65536)
	if pause > 100 {
		if prop := s.gc.lastLiveBytes * int64(pause-100) / 100; prop > threshold {
			threshold = prop
		}
	}
	if s.gc.debt >= threshold {
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

// activateTier1 flips this State into deterministic Tier 1 GC semantics.
func (s *State) activateTier1() {
	s.gc.tier1 = true
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
		if t.flags&tblFlagGCFnzed != 0 {
			// Re-arming inside __gc (tracegc.lua / gc.lua)!
			t.flags &^= tblFlagGCFnzed
		}
		if t.flags&tblFlagGCInFnz == 0 {
			t.flags |= tblFlagGCInFnz
			s.gc.finalizable = append(s.gc.finalizable, gcFinalEntry{obj: obj})
		}
	case obj.IsUserdata():
		u := obj.AsUserdata()
		if u.gcFnzed {
			u.gcFnzed = false
		}
		if !u.gcInFnz {
			u.gcInFnz = true
			s.gc.finalizable = append(s.gc.finalizable, gcFinalEntry{obj: obj})
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
	return strings.ContainsRune(ms, 'k'), strings.ContainsRune(ms, 'v')
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

type gcTracer struct {
	s                *State
	epoch            uint64
	preserveFrames   bool // true when triggered mid-opcode (e.g. checkMem) where s.cur.top may be stale
	countStrings     bool // true when s.memLimit > 0 so live strings are counted accurately in liveBytes
	trackDeadStrings bool // true when deleted hash keys exist so unreachable dead string keys can be released
	seenStrings      map[*LuaString]struct{}
	seenProtos       map[*boundProto]struct{}
	seenShapes       map[*Shape]struct{}
	gray             []Value
	hashedTables     []*Table // all marked tables with t.hash != nil (for dead hash-key clearing)
	weakVals         []*Table // __mode="v"
	ephemerons       []*Table // __mode="k"
	allWeaks         []*Table // __mode="kv"
	liveOpenUpvals   []*Upvalue
	liveBytes        int64
}

func (tr *gcTracer) markShape(sh *Shape) {
	root := tr.s.shapes.root
	for cur := sh; cur != nil && !cur.frozen; cur = cur.parent {
		if _, ok := tr.seenShapes[cur]; ok {
			return
		}
		if tr.seenShapes == nil {
			tr.seenShapes = make(map[*Shape]struct{}, 32)
		}
		tr.seenShapes[cur] = struct{}{}
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
	if s.loadedTbl != nil {
		tr.mark(TableValue(s.loadedTbl))
	}
	if s.preloadTbl != nil {
		tr.mark(TableValue(s.preloadTbl))
	}
	for _, mt := range s.typeMetas {
		if mt != nil {
			tr.mark(TableValue(mt))
		}
	}
	if s.stringMeta != nil {
		tr.mark(TableValue(s.stringMeta))
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
		tr.mark(v)
	}
	for _, v := range s.gc.toBeFnz {
		tr.mark(v)
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
		tr.liveBytes += 64 + int64(len(c.upvals)*24)
		tr.markBoundProto(c.bp)
		tr.gray = append(tr.gray, v)
	case v.IsNative():
		nf := v.AsNative()
		if nf.gcMark == tr.epoch {
			return
		}
		nf.gcMark = tr.epoch
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
		tr.liveBytes += 64 + int64(len(u.UValues)*16)
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
		tr.liveBytes += 512 + int64(cap(th.stack))*16 + int64(cap(th.frames))*int64(unsafe.Sizeof(callFrame{}))
		tr.gray = append(tr.gray, v)
	}
}

func (tr *gcTracer) propagateAll() {
	for len(tr.gray) > 0 {
		v := tr.gray[len(tr.gray)-1]
		tr.gray = tr.gray[:len(tr.gray)-1]
		switch {
		case v.IsTable():
			tr.traverseTable(v.AsTable())
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
				tr.mark(uv)
			}
		case v.IsThread():
			tr.traverseThread(v.AsThread())
		}
	}
}

func (tr *gcTracer) traverseThread(th *Thread) {
	// Mirror lgc.c:traversethread literally: mark only stack[0..th.top) and nil-fill stack[th.top:)!
	for i := range th.nframe {
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
		tr.mark(th.stack[i])
	}
	if lim < len(th.stack) {
		fillNil(th.stack[lim:])
	}
	maxOpenUpvalIdx := -1
	for uv := th.openUpvals; uv != nil; uv = uv.next {
		if uv.stackIdx < len(th.stack) {
			tr.mark(th.stack[uv.stackIdx])
		}
		if uv.stackIdx > maxOpenUpvalIdx {
			maxOpenUpvalIdx = uv.stackIdx
		}
	}
	for _, yv := range th.yieldVals {
		tr.mark(yv)
	}
	for _, ce := range th.syncCloseErrs {
		tr.mark(ce)
	}
	tr.mark(th.errObj)
	tr.mark(th.hook)
	if th.caller != nil {
		tr.mark(ThreadValue(th.caller))
	}
	// Shrink oversized stacks on non-running coroutines (including coroutines with open upvalues,
	// since Upvalue tracks stackIdx rather than raw slice pointers) & release finished dead stacks.
	if th != tr.s.cur && th != tr.s.mainThread {
		if th.status == ThreadDead && th.nframe == 0 && th.top == 0 && th.openUpvals == nil && len(th.tbc) == 0 {
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
				tr.mark(v)
			}
		}
		for _, v := range t.slots {
			tr.mark(v)
		}
		if t.hash != nil {
			nodes := t.hash.nodes
			for i := range nodes {
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
			tr.mark(v)
		}
	}
	for _, v := range t.slots {
		tr.mark(v)
	}
	if t.hash != nil {
		nodes := t.hash.nodes
		for i := range nodes {
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
	changed := true
	for changed {
		changed = false
		for _, t := range tr.ephemerons {
			if t.hash == nil {
				continue
			}
			nodes := t.hash.nodes
			for i := range nodes {
				nd := &nodes[i]
				if nd.used && !nd.dead && !nd.val.IsNil() {
					if tr.isMarked(nd.key) && !tr.isMarked(nd.val) {
						tr.mark(nd.val)
						changed = true
					}
				}
			}
		}
		if changed {
			tr.propagateAll()
		}
	}
}

func (tr *gcTracer) clearByValues(tables []*Table) {
	cleared := false
	for _, t := range tables {
		if t.arrKind == arrGeneric && t.arrLen > 0 {
			arr := t.arrValSlice()
			for i := range arr {
				if !arr[i].IsNil() && !tr.isMarked(arr[i]) {
					arr[i] = NilValue()
					cleared = true
				}
			}
		}
		for i := range t.slots {
			if !t.slots[i].IsNil() && !tr.isMarked(t.slots[i]) {
				t.slots[i] = NilValue()
				cleared = true
			}
		}
		if h := t.hash; h != nil {
			nodes := h.nodes
			for i := range nodes {
				nd := &nodes[i]
				if nd.used && !nd.dead && !nd.val.IsNil() && !tr.isMarked(nd.val) {
					nd.val = NilValue()
					nd.dead = true
					h.live--
					h.dictGen++
					cleared = true
				}
			}
		}
	}
	if cleared {
		tr.s.protoEpoch++
	}
}

func (tr *gcTracer) clearByKeys(tables []*Table) {
	cleared := false
	for _, t := range tables {
		if h := t.hash; h != nil {
			nodes := h.nodes
			for i := range nodes {
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
	if cleared {
		tr.s.protoEpoch++
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
					if v.IsString() {
						tr.mark(v)
					}
				}
			}
			for _, v := range t.slots {
				if v.IsString() {
					tr.mark(v)
				}
			}
			if t.hash != nil {
				nodes := t.hash.nodes
				for i := range nodes {
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
	markTableStrings(tr.ephemerons)
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

func (tr *gcTracer) icEntryAlive(kind icKind, shape, targetShape *Shape, holder *Table, val Value, epoch uint32, noMeta bool) bool {
	switch kind {
	case icEmpty:
		return false
	case icEnvSlot:
		if holder == nil || holder.gcMark != tr.epoch || holder.hash == nil || holder.hash.dictGen != epoch {
			return false
		}
		return true
	case icProtoVal:
		if !noMeta && epoch != tr.s.protoEpoch {
			return false
		}
		if !tr.icShapeAlive(shape) || !tr.icValAlive(val) {
			return false
		}
		return true
	case icAddSlot:
		if !noMeta && epoch != tr.s.protoEpoch {
			return false
		}
		return tr.icShapeAlive(shape) && tr.icShapeAlive(targetShape)
	case icOwnSlot:
		if !noMeta && epoch != tr.s.protoEpoch {
			return false
		}
		return tr.icShapeAlive(shape)
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
			ic := &ics[i]
			if ic.kind == icEmpty && ic.poly == nil {
				continue
			}
			// Filter polymorphic entries first.
			if ic.poly != nil {
				kept := 0
				for j := range int(ic.polyCount) {
					pe := ic.poly[j]
					if tr.icEntryAlive(pe.kind, pe.shape, pe.targetShape, nil, pe.val, pe.epoch, pe.noMeta) {
						if kept != j {
							ic.poly[kept] = pe
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
			if !tr.icEntryAlive(ic.kind, ic.shape, ic.targetShape, ic.holder, ic.val, ic.epoch, ic.noMeta) {
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
					ic.shape = pe.shape
					ic.targetShape = pe.targetShape
					ic.holder = nil
					ic.val = pe.val
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

func (s *State) collectFullImpl(preserveFrames bool, emergency bool) {
	if s.gc.inGCTM {
		return // non-reentrant inside __gc (gc.lua:698-705)
	}
	countStr := s.memLimit > 0
	trackDeadStr := s.gc.hasDeadHashKeys
	if emergency || (!s.gc.tier1 && len(s.gc.finalizable) == 0 && len(s.gc.weakFiles) == 0) {
		// Tier 0 (with no open finalizable handles) or emergency allocation-time recount:
		// nil-fill dead stack/frame slack and recompute exact live VM bytes without running
		// __gc finalizers or mutating weak-table slots mid-operation!
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
		}
		tr.markRoots()
		tr.propagateAll()
		if emergency && (len(tr.ephemerons) > 0 || len(tr.weakVals) > 0 || len(tr.allWeaks) > 0 || len(s.gc.finalizable) > 0 || len(s.gc.weakFiles) > 0) {
			// Match PUC-Rio lgc.c:luaC_fullgc(L, isemergency=1): clear unreachable weak entries
			// so weak caches cannot pin gigabytes when collectgarbage("stop") is active, while
			// deferring __gc execution and keeping pending-finalizer objects accounted in liveBytes.
			tr.convergeEphemerons()
			tr.clearByValues(tr.weakVals)
			tr.clearByValues(tr.allWeaks)
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
				if u := wf.u.Value(); u != nil && !u.gcFnzed {
					obj := UserdataValue(u)
					if !tr.isMarked(obj) {
						tr.mark(obj)
					}
				}
			}
			tr.propagateAll()
			tr.convergeEphemerons()
			tr.clearByKeys(tr.ephemerons)
			tr.clearByKeys(tr.allWeaks)
			tr.clearByValues(tr.weakVals)
			tr.clearByValues(tr.allWeaks)
		}
		tr.accountWeakStrings()
		s.gc.hasDeadHashKeys = tr.clearDeadHashKeys()
		tr.sweepInlineCaches()
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
	}

	// Mark all VM roots.
	tr.markRoots()
	tr.propagateAll()

	// Step 1: Converge ephemerons before clearing weak values.
	tr.convergeEphemerons()

	// Step 2: clearByValues BEFORE __gc separation (weak values are nil inside __gc!).
	tr.clearByValues(tr.weakVals)
	tr.clearByValues(tr.allWeaks)

	// Step 3: separateToBeFnz in STRICT REVERSE (LIFO) registration order!
	// Use a separate slice for kept entries so reverse traversal never overwrites
	// earlier unvisited entries in s.gc.finalizable.
	var kept []gcFinalEntry
	var newlySep []Value
	for i := len(s.gc.finalizable) - 1; i >= 0; i-- {
		obj := s.gc.finalizable[i].obj
		if !tr.isMarked(obj) {
			// Mark as finalized so it won't fire twice unless re-armed by setmetatable in __gc.
			switch {
			case obj.IsTable():
				t := obj.AsTable()
				t.flags = (t.flags | tblFlagGCFnzed) &^ tblFlagGCInFnz
			case obj.IsUserdata():
				u := obj.AsUserdata()
				u.gcFnzed = true
				u.gcInFnz = false
			}
			newlySep = append(newlySep, obj)
		} else {
			kept = append(kept, s.gc.finalizable[i])
		}
	}
	// Restore original registration order in kept (since we iterated backwards).
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	s.gc.finalizable = kept
	var keptFiles []gcWeakFileEntry
	for i := len(s.gc.weakFiles) - 1; i >= 0; i-- {
		wf := s.gc.weakFiles[i]
		u := wf.u.Value()
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
			u.gcFnzed = true
			newlySep = append(newlySep, obj)
		} else {
			keptFiles = append(keptFiles, wf)
		}
	}
	for i, j := 0, len(keptFiles)-1; i < j; i, j = i+1, j-1 {
		keptFiles[i], keptFiles[j] = keptFiles[j], keptFiles[i]
	}
	s.gc.weakFiles = keptFiles
	s.gc.toBeFnz = append(s.gc.toBeFnz, newlySep...)

	// Step 4: markBeingFnz + propagate (resurrection: keeps dying object & its refs alive during __gc!).
	for _, obj := range s.gc.toBeFnz {
		tr.mark(obj)
	}
	tr.propagateAll()

	// Step 5: convergeEphemerons + clearByKeys (weak keys of finalizing objects stay alive during __gc!).
	tr.convergeEphemerons()
	tr.clearByKeys(tr.ephemerons)
	tr.clearByKeys(tr.allWeaks)

	// Step 6: clearByValues again on ALL weak-value/all-weak tables (including any resurrected in Step 4, gc.lua:444!).
	tr.clearByValues(tr.weakVals)
	tr.clearByValues(tr.allWeaks)

	// Strings are value-like in Lua 5.5 and survive weak clearing; account for surviving strings
	// in weak table positions before sweeping dead hash keys and updating allocBytes.
	tr.accountWeakStrings()

	// Release strong Go pointers from unreachable dead/tombstoned hash keys (matching lgc.c:clearkey)
	// and sweep invalidated/unreachable inline cache entries.
	s.gc.hasDeadHashKeys = tr.clearDeadHashKeys()
	tr.sweepInlineCaches()

	// Close open upvalues belonging to threads that remain unreachable after Step 4 resurrection,
	// so live closures keep their captured values and dead threads are unpinned for Go's GC.
	for _, uv := range tr.liveOpenUpvals {
		if uv.owner != nil && uv.owner.gcMark != tr.epoch {
			uv.closed = uv.owner.stack[uv.stackIdx]
			uv.owner = nil
			uv.next = nil
		}
	}

	s.intern.Prune()
	s.shapes.Prune()
	s.gc.allocBytes = tr.liveBytes
	s.gc.lastLiveBytes = tr.liveBytes
	s.gc.debt = 0

	// Drain all queued __gc finalizers synchronously on the VM thread.
	s.drainFinalizers()
	s.compactOpenFiles()
}

func (s *State) drainFinalizers() {
	if s.gc.inGCTM || len(s.gc.toBeFnz) == 0 {
		return
	}
	s.gc.inGCTM = true
	// Disable debug hooks and forbid coroutine.yield while running __gc (lgc.c:GCTM).
	th := s.cur
	savedMask := uint8(0)
	if th != nil {
		savedMask = th.hookMask
		th.hookMask = 0
		th.nny++
	}
	defer func() {
		if th != nil {
			th.nny--
			th.hookMask = savedMask
		}
		s.gc.inGCTM = false
	}()

	for len(s.gc.toBeFnz) > 0 {
		obj := s.gc.toBeFnz[0]
		s.gc.toBeFnz[0] = Value{}
		s.gc.toBeFnz = s.gc.toBeFnz[1:]
		var mt *Table
		switch {
		case obj.IsTable():
			mt = obj.AsTable().meta
		case obj.IsUserdata():
			mt = obj.AsUserdata().Meta
		}
		if mt == nil {
			continue
		}
		tm := mt.RawGetStr(s.intern.tms[TMGC])
		if tm.IsNil() || (!tm.IsClosure() && !tm.IsNative() && s.getTMByObj(tm, TMCall).IsNil()) {
			continue
		}
		th.nextFrameFinalizer = true
		_, err := s.callOnThread(th, tm, []Value{obj})
		th.nextFrameFinalizer = false
		if err != nil {
			if errors.Is(err, errCoroCloseSelf) || isUncatchableErr(err) {
				s.abortErr = err
				return
			}
			s.emitWarning(fmt.Sprintf("error in __gc metamethod (%s)", err.Error()))
		}
	}
	s.gc.toBeFnz = nil
}

// Close closes the State: closes remaining mainThread <close> variables, runs ALL remaining
// __gc finalizers in strict LIFO order regardless of reachability (matching lua_close /
// gc.lua:653-668 / main.lua:333-360,491-501), and flushes/closes all open files.
func (s *State) Close() {
	if !s.gc.closing {
		// Close any active <close> TBC variables on mainThread first (e.g. os.exit(true, true)).
		if s.mainThread != nil {
			s.mainThread.nny++
			_, _, _ = s.mainThread.closeAllSync(0, NilValue(), false)
			s.mainThread.nny--
		}
		s.gc.closing = true
		// Reverse all remaining finalizable objects into toBeFnz (LIFO).
		for i := len(s.gc.finalizable) - 1; i >= 0; i-- {
			obj := s.gc.finalizable[i].obj
			switch {
			case obj.IsTable():
				t := obj.AsTable()
				t.flags = (t.flags | tblFlagGCFnzed) &^ tblFlagGCInFnz
			case obj.IsUserdata():
				u := obj.AsUserdata()
				u.gcFnzed = true
				u.gcInFnz = false
			}
			s.gc.toBeFnz = append(s.gc.toBeFnz, obj)
		}
		s.gc.finalizable = nil
		for i := len(s.gc.weakFiles) - 1; i >= 0; i-- {
			wf := s.gc.weakFiles[i]
			u := wf.u.Value()
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
			s.gc.toBeFnz = append(s.gc.toBeFnz, UserdataValue(u))
		}
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
}

func (s *State) drainFinalizersQueued() {
	th := s.cur
	for len(s.gc.toBeFnz) > 0 {
		obj := s.gc.toBeFnz[0]
		s.gc.toBeFnz[0] = Value{}
		s.gc.toBeFnz = s.gc.toBeFnz[1:]
		var mt *Table
		switch {
		case obj.IsTable():
			mt = obj.AsTable().meta
		case obj.IsUserdata():
			mt = obj.AsUserdata().Meta
		}
		if mt == nil {
			continue
		}
		tm := mt.RawGetStr(s.intern.tms[TMGC])
		if tm.IsNil() || (!tm.IsClosure() && !tm.IsNative() && s.getTMByObj(tm, TMCall).IsNil()) {
			continue
		}
		th.nextFrameFinalizer = true
		_, err := s.callOnThread(th, tm, []Value{obj})
		th.nextFrameFinalizer = false
		if err != nil {
			if errors.Is(err, errCoroCloseSelf) || isUncatchableErr(err) {
				s.abortErr = err
				return
			}
			s.emitWarning(fmt.Sprintf("error in __gc metamethod (%s)", err.Error()))
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
			return nil, fmt.Errorf("bad argument #1 to 'collectgarbage' (string expected)")
		}
		opt = args[0].AsString().S
	}
	// Non-reentrant inside __gc finalizer (gc.lua:698-705).
	if s.gc.inGCTM {
		return []Value{NilValue()}, nil
	}
	switch opt {
	case "collect":
		s.CollectFull()
		if s.abortErr != nil {
			return nil, s.abortErr
		}
		return []Value{IntValue(0)}, nil
	case "stop":
		s.gc.running = false
		return []Value{IntValue(0)}, nil
	case "restart":
		s.gc.running = true
		return []Value{IntValue(0)}, nil
	case "count":
		kb := float64(s.gc.allocBytes) / 1024.0
		return []Value{FloatValue(kb)}, nil
	case "step":
		if s.gc.tier1 {
			s.CollectFull()
			if s.abortErr != nil {
				return nil, s.abortErr
			}
		}
		return []Value{TrueValue()}, nil
	case "isrunning":
		return []Value{BoolValue(s.gc.running)}, nil
	case "incremental":
		prev := "incremental"
		if s.gc.mode == gcModeGenerational {
			prev = "generational"
		}
		s.gc.mode = gcModeIncremental
		return []Value{StringValue(s.intern.Intern(prev))}, nil
	case "generational":
		prev := "incremental"
		if s.gc.mode == gcModeGenerational {
			prev = "generational"
		}
		s.gc.mode = gcModeGenerational
		return []Value{StringValue(s.intern.Intern(prev))}, nil
	case "param":
		if len(args) < 2 || !args[1].IsString() {
			return nil, fmt.Errorf("bad argument #2 to 'collectgarbage' (string expected)")
		}
		idx, ok := gcParamNames[args[1].AsString().S]
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'collectgarbage' (invalid option '%s')", args[1].AsString().S)
		}
		old := s.gc.params[idx]
		if len(args) >= 3 && !args[2].IsNil() {
			nv, ok := args[2].ToInt()
			if !ok {
				return nil, fmt.Errorf("bad argument #3 to 'collectgarbage' (number expected)")
			}
			s.gc.params[idx] = int(nv)
		}
		return []Value{IntValue(int64(old))}, nil
	default:
		return nil, fmt.Errorf("bad argument #1 to 'collectgarbage' (invalid option '%s')", opt)
	}
}
