package vm

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/yuchanns/moelua/internal/arena"
)

// Per-State PUC-Rio Lua 5.5.1 Incremental & Generational GC Bridge:
//
// Wires State's noscan Arena objects (LuaString, Table, Shape, Closure, Upvalue,
// ArenaNative, Userdata, Thread, ArenaProto) directly into internal/arena.Collector.
// Go's GC never scans Lua heaps (O(pages) mark per VM), while Lua's Collector runs
// exact PUC-Rio lgc.c incremental tri-color + generational minor/major cycles,
// weak/ephemeron clearing, LIFO __gc finalizers, and automatic GoRefs reclamation.

func (s *State) initCollector() {
	s.collector = arena.NewCollector(s.arena, arena.Callbacks{
		MarkRoots:         s.gcMarkRoots,
		MarkAtomicRoots:   s.gcMarkRoots,
		AfterGrayAgain:    s.gcRemarkUpvals,
		PreSweep:          s.gcPreSweep,
		AuxBytes:          s.gcAuxBytes,
		Traverse:          s.gcTraverse,
		TraverseEphemeron: s.gcTraverseEphemeron,
		ClearByKeys:       s.gcClearByKeys,
		ClearByValues:     s.gcClearByValues,
		IsThread:          s.gcIsThread,
		IsOpenUpval:       s.gcIsOpenUpval,
		MarkLeaf:          s.gcMarkLeaf,
		FreeObject:        s.gcFreeObject,
		RunFinalizer:      s.gcRunFinalizer,
		CheckSizes:        s.gcCheckSizes,
		CancelCheck:       s.gcCancelCheck,
	})
	s.intern.arena = s.arena
	s.intern.collector = s.collector
}

func (s *State) markValue(c *arena.Collector, v Value) {
	if v.IsGCObject() {
		c.MarkObject(v.Ref())
	}
}

func (s *State) gcCancelCheck() error {
	if s.abortErr != nil {
		return s.abortErr
	}
	if s.ctx != nil || s.insnLimit > 0 {
		return s.checkNativeCancelStep()
	}
	return nil
}

func (s *State) gcIsThread(ref arena.ObjRef) bool {
	return s.arena.Header(ref).Kind == gcKindThread
}

func (s *State) gcIsOpenUpval(ref arena.ObjRef) bool {
	if s.arena.Header(ref).Kind != gcKindUpvalue {
		return false
	}
	return arena.AsSmall[Upvalue](s.arena, ref).ownerRef != arena.NullRef
}

func (s *State) gcMarkLeaf(c *arena.Collector, ref arena.ObjRef) bool {
	kind := s.arena.Header(ref).Kind
	return kind == gcKindShortStr || kind == gcKindLongStr
}

func (s *State) gcAuxBytes(a *arena.Arena, ref arena.ObjRef) int64 {
	switch a.Header(ref).Kind {
	case gcKindTable:
		t := arena.AsSmall[Table](a, ref)
		var total int64
		if t.arrRef != arena.NullRef {
			total += int64(a.SlotSize(t.arrRef))
		} else if t.arrHint > 0 {
			total += int64(t.arrHint) * 8
		}
		if t.slotsRef != arena.NullRef {
			total += int64(a.SlotSize(t.slotsRef))
		}
		if t.hashRef != arena.NullRef {
			total += int64(a.SlotSize(t.hashRef))
			h := arena.AsSmall[tableHash](a, t.hashRef)
			if h.nodesRef != arena.NullRef {
				total += int64(a.SlotSize(h.nodesRef))
			}
		}
		return total
	case gcKindShape:
		sh := arena.AsSmall[Shape](a, ref)
		var total int64
		if sh.transRef != arena.NullRef {
			total += int64(a.SlotSize(sh.transRef))
		}
		if sh.indexRef != arena.NullRef {
			total += int64(a.SlotSize(sh.indexRef))
		}
		return total
	case gcKindClosure:
		cl := arena.AsSmall[Closure](a, ref)
		if cl.upvalsRef != arena.NullRef {
			return int64(a.SlotSize(cl.upvalsRef))
		}
		return 0
	case gcKindNative:
		an := arena.AsSmall[ArenaNative](a, ref)
		var total int64
		if an.upvalsRef != arena.NullRef {
			total += int64(a.SlotSize(an.upvalsRef))
		}
		if an.upvalIDsRef != arena.NullRef {
			total += int64(a.SlotSize(an.upvalIDsRef))
		}
		return total
	case gcKindUserdata:
		u := arena.AsSmall[Userdata](a, ref)
		var total int64
		if u.uvalsRef != arena.NullRef {
			total += int64(a.SlotSize(u.uvalsRef))
		}
		if u.gcRefsRef != arena.NullRef {
			total += int64(a.SlotSize(u.gcRefsRef))
		}
		return total
	case gcKindThread:
		th := arena.AsSmall[Thread](a, ref)
		var total int64
		if th.stackRef != arena.NullRef {
			total += int64(a.SlotSize(th.stackRef))
		}
		if th.framesRef != arena.NullRef {
			total += int64(a.SlotSize(th.framesRef))
		}
		if th.openUpvalsRef != arena.NullRef {
			total += int64(a.SlotSize(th.openUpvalsRef))
		}
		if th.tbcRef != arena.NullRef {
			total += int64(a.SlotSize(th.tbcRef))
		}
		if th.yieldValsRef != arena.NullRef {
			total += int64(a.SlotSize(th.yieldValsRef))
		}
		if th.syncCloseRef != arena.NullRef {
			total += int64(a.SlotSize(th.syncCloseRef))
		}
		return total
	case gcKindProto:
		ap := arena.AsSmall[ArenaProto](a, ref)
		var total int64
		if ap.constsRef != arena.NullRef {
			total += int64(a.SlotSize(ap.constsRef))
		}
		if ap.subRef != arena.NullRef {
			total += int64(a.SlotSize(ap.subRef))
		}
		if ap.icRef != arena.NullRef {
			total += int64(a.SlotSize(ap.icRef))
			ics := arena.SliceFast[ICEntry](a, ap.icRef, 0, int(ap.nIC))
			for i := range ics {
				if ics[i].polyRef != arena.NullRef {
					total += int64(a.SlotSize(ics[i].polyRef))
				}
			}
		}
		if int(ap.protoIdx) > 0 && int(ap.protoIdx) < len(s.protos) {
			if bp := s.protos[ap.protoIdx]; bp != nil {
				total += bp.goBytes
			}
		}
		return total
	default:
		return 0
	}
}

func (s *State) gcMarkRoots(c *arena.Collector) {
	if s.registryRef != arena.NullRef {
		c.MarkObject(s.registryRef)
	}
	if s.pkgRef != arena.NullRef {
		c.MarkObject(s.pkgRef)
	}
	if s.fileMetaRef != arena.NullRef {
		c.MarkObject(s.fileMetaRef)
	}
	if s.stringMetaRef != arena.NullRef {
		c.MarkObject(s.stringMetaRef)
	}
	for _, ref := range s.typeMetaRefs {
		if ref != arena.NullRef {
			c.MarkObject(ref)
		}
	}
	if s.shapes.rootRef != arena.NullRef {
		c.MarkObject(s.shapes.rootRef)
	}
	if s.pinnedShapeRef != arena.NullRef {
		c.MarkObject(s.pinnedShapeRef)
	}
	s.markValue(c, s.stdinInput)
	s.markValue(c, s.defaultInput)
	s.markValue(c, s.defaultOutput)
	if s.mainThreadRef != arena.NullRef {
		c.MarkObject(s.mainThreadRef)
	}
	if s.curRef != arena.NullRef && s.curRef != s.mainThreadRef {
		c.MarkObject(s.curRef)
	}
	for _, v := range s.pinnedRefs {
		s.markValue(c, v)
	}
}

// isWeakCollectable reports whether v can be cleared from a weak table.
// Per Lua 5.5 spec (lgc.c:iscleared): strings, numbers, bools, nil, lightud, and
// non-collectable C functions are NEVER weak!
func (s *State) isWeakCollectable(v Value) bool {
	switch v.tag {
	case tagTable, tagClosure, tagUserdata, tagThread:
		return true
	case tagNative:
		return arena.AsSmall[ArenaNative](s.arena, v.Ref()).isCollectable()
	default:
		return false
	}
}

func (s *State) isClearedWeak(c *arena.Collector, v Value) bool {
	if !s.isWeakCollectable(v) {
		// Strings and non-collectable natives survive weak tables; ensure they are marked!
		if v.IsGCObject() {
			c.MarkObject(v.Ref())
		}
		return false
	}
	return c.IsWhite(v.Ref())
}

func (s *State) weakModeOf(t *Table) (weakK, weakV bool) {
	if t.metaRef == arena.NullRef {
		return false, false
	}
	mt := arena.AsSmall[Table](s.arena, t.metaRef)
	mv := mt.RawGetStr(s, s.intern.tms[TMMode])
	if !mv.IsString() {
		return false, false
	}
	ms := s.asString(mv).Str()
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

func (s *State) gcTraverse(c *arena.Collector, ref arena.ObjRef) int64 {
	a := s.arena
	switch a.Header(ref).Kind {
	case gcKindTable:
		return s.gcTraverseTable(c, arena.AsSmall[Table](a, ref))
	case gcKindShape:
		sh := arena.AsSmall[Shape](a, ref)
		if sh.parentRef != arena.NullRef {
			c.MarkObject(sh.parentRef)
		}
		if sh.metaRef != arena.NullRef && !sh.isMetaProxy() {
			c.MarkObject(sh.metaRef)
		}
		s.markValue(c, sh.keyVal)
		return 1
	case gcKindClosure:
		cl := arena.AsSmall[Closure](a, ref)
		if cl.protoRef != arena.NullRef {
			c.MarkObject(cl.protoRef)
		}
		ups := cl.upvalsSlice(a)
		for _, uvRef := range ups {
			if uvRef != arena.NullRef {
				c.MarkObject(uvRef)
			}
		}
		return int64(1 + len(ups))
	case gcKindUpvalue:
		uv := arena.AsSmall[Upvalue](a, ref)
		if uv.ownerRef == arena.NullRef {
			s.markValue(c, uv.closed)
		}
		return 1
	case gcKindNative:
		an := arena.AsSmall[ArenaNative](a, ref)
		ups := an.upvalsSlice(a)
		for _, v := range ups {
			s.markValue(c, v)
		}
		return int64(1 + len(ups))
	case gcKindUserdata:
		u := arena.AsSmall[Userdata](a, ref)
		if u.metaRef != arena.NullRef {
			c.MarkObject(u.metaRef)
		}
		uvals := u.uvalsSlice(a)
		for _, v := range uvals {
			s.markValue(c, v)
		}
		grefs := u.gcRefsSlice(a)
		for _, v := range grefs {
			s.markValue(c, v)
		}
		return int64(1 + len(uvals) + len(grefs))
	case gcKindThread:
		return s.gcTraverseThread(c, arena.AsSmall[Thread](a, ref))
	case gcKindProto:
		ap := arena.AsSmall[ArenaProto](a, ref)
		if ap.constsRef != arena.NullRef {
			consts := arena.SliceFast[Value](a, ap.constsRef, 0, int(ap.nConsts))
			for _, v := range consts {
				s.markValue(c, v)
			}
		}
		if ap.subRef != arena.NullRef {
			subs := arena.SliceFast[arena.ObjRef](a, ap.subRef, 0, int(ap.nSub))
			for _, subRef := range subs {
				if subRef != arena.NullRef {
					c.MarkObject(subRef)
				}
			}
		}
		return int64(1 + ap.nConsts + ap.nSub)
	default:
		return 1
	}
}

func (s *State) gcTraverseTable(c *arena.Collector, t *Table) int64 {
	a := s.arena
	if t.shapeRef != arena.NullRef {
		c.MarkObject(t.shapeRef)
	}
	if t.metaRef != arena.NullRef {
		c.MarkObject(t.metaRef)
	}
	var work int64 = 1
	weakK, weakV := s.weakModeOf(t)
	checkCancel := s.hasInterruptControl()
	if !weakK && !weakV {
		if t.arrKind == arrGeneric && t.arrLen > 0 {
			arr := t.arrValSlice(a)
			for i, v := range arr {
				if checkCancel && (i&4095) == 4095 {
					if err := s.gcCancelCheck(); err != nil {
						c.SetCancelErr(err)
						return work + int64(i)
					}
				}
				s.markValue(c, v)
			}
			work += int64(len(arr))
		}
		if slots := t.slotsSlice(a); len(slots) > 0 {
			for _, v := range slots {
				s.markValue(c, v)
			}
			work += int64(len(slots))
		}
		if t.hashRef != arena.NullRef {
			nodes := arena.AsSmall[tableHash](a, t.hashRef).nodesSlice(a)
			for i := range nodes {
				if checkCancel && (i&4095) == 4095 {
					if err := s.gcCancelCheck(); err != nil {
						c.SetCancelErr(err)
						return work + int64(i)
					}
				}
				nd := &nodes[i]
				if nd.state == nodeLive && !nd.val.IsNil() {
					s.markValue(c, nd.key)
					s.markValue(c, nd.val)
				}
			}
			work += int64(len(nodes))
		}
		return work
	}
	if weakK && weakV {
		// All-weak table: mark non-weak values/keys (e.g. strings, non-collectable natives).
		if t.arrKind == arrGeneric && t.arrLen > 0 {
			for i, v := range t.arrValSlice(a) {
				if checkCancel && (i&4095) == 4095 {
					if err := s.gcCancelCheck(); err != nil {
						c.SetCancelErr(err)
						return work + int64(i)
					}
				}
				if !v.IsNil() && !s.isWeakCollectable(v) {
					s.markValue(c, v)
				}
			}
		}
		for _, v := range t.slotsSlice(a) {
			if !v.IsNil() && !s.isWeakCollectable(v) {
				s.markValue(c, v)
			}
		}
		if t.hashRef != arena.NullRef {
			nodes := arena.AsSmall[tableHash](a, t.hashRef).nodesSlice(a)
			for i := range nodes {
				if checkCancel && (i&4095) == 4095 {
					if err := s.gcCancelCheck(); err != nil {
						c.SetCancelErr(err)
						return work + int64(i)
					}
				}
				nd := &nodes[i]
				if nd.state == nodeLive && !nd.val.IsNil() {
					if !s.isWeakCollectable(nd.key) {
						s.markValue(c, nd.key)
					}
					if !s.isWeakCollectable(nd.val) {
						s.markValue(c, nd.val)
					}
				}
			}
			work += int64(len(nodes))
		}
		c.MarkAllWeak(t.self)
		return work
	}
	if weakV {
		hasClears := false
		if t.arrKind == arrGeneric && t.arrLen > 0 {
			arr := t.arrValSlice(a)
			for i, v := range arr {
				if checkCancel && (i&4095) == 4095 {
					if err := s.gcCancelCheck(); err != nil {
						c.SetCancelErr(err)
						return work + int64(i)
					}
				}
				if !v.IsNil() {
					if !s.isWeakCollectable(v) {
						s.markValue(c, v)
					} else if c.IsWhite(v.Ref()) {
						hasClears = true
					}
				}
			}
			work += int64(len(arr))
		}
		if slots := t.slotsSlice(a); len(slots) > 0 {
			for _, v := range slots {
				if !v.IsNil() {
					if !s.isWeakCollectable(v) {
						s.markValue(c, v)
					} else if c.IsWhite(v.Ref()) {
						hasClears = true
					}
				}
			}
			work += int64(len(slots))
		}
		if t.hashRef != arena.NullRef {
			nodes := arena.AsSmall[tableHash](a, t.hashRef).nodesSlice(a)
			for i := range nodes {
				if checkCancel && (i&4095) == 4095 {
					if err := s.gcCancelCheck(); err != nil {
						c.SetCancelErr(err)
						return work + int64(i)
					}
				}
				nd := &nodes[i]
				if nd.state == nodeLive && !nd.val.IsNil() {
					s.markValue(c, nd.key)
					if !s.isWeakCollectable(nd.val) {
						s.markValue(c, nd.val)
					} else if c.IsWhite(nd.val.Ref()) {
						hasClears = true
					}
				}
			}
			work += int64(len(nodes))
		}
		c.MarkWeakValues(t.self, hasClears)
		return work
	}
	// Ephemeron table (__mode = "k"): array & shape slots have non-weak int/string keys -> strong values!
	if t.arrKind == arrGeneric && t.arrLen > 0 {
		arr := t.arrValSlice(a)
		for i, v := range arr {
			if checkCancel && (i&4095) == 4095 {
				if err := s.gcCancelCheck(); err != nil {
					c.SetCancelErr(err)
					return work + int64(i)
				}
			}
			s.markValue(c, v)
		}
		work += int64(len(arr))
	}
	if slots := t.slotsSlice(a); len(slots) > 0 {
		for _, v := range slots {
			s.markValue(c, v)
		}
		work += int64(len(slots))
	}
	s.gcTraverseEphemeron(c, t.self, 0)
	if t.hashRef != arena.NullRef {
		work += int64(arena.AsSmall[tableHash](a, t.hashRef).mask) + 1
	}
	return work
}

func (s *State) gcTraverseEphemeron(c *arena.Collector, ref arena.ObjRef, dir int) bool {
	a := s.arena
	t := arena.AsSmall[Table](a, ref)
	hasWW := false
	hasClears := false
	marked := false
	if t.hashRef != arena.NullRef {
		nodes := arena.AsSmall[tableHash](a, t.hashRef).nodesSlice(a)
		n := len(nodes)
		for step := range n {
			i := step
			if dir&1 != 0 {
				i = n - 1 - step
			}
			nd := &nodes[i]
			if nd.state != nodeLive || nd.val.IsNil() {
				continue
			}
			if s.isClearedWeak(c, nd.key) {
				hasClears = true
				if !s.isWeakCollectable(nd.val) {
					s.markValue(c, nd.val)
				} else if c.IsWhite(nd.val.Ref()) {
					hasWW = true
				}
			} else {
				if nd.val.IsGCObject() && c.IsWhite(nd.val.Ref()) {
					c.MarkObject(nd.val.Ref())
					marked = true
				}
			}
		}
	}
	c.MarkEphemeron(ref, hasWW, hasClears)
	return marked
}

func (s *State) gcClearByValues(c *arena.Collector, ref arena.ObjRef) {
	a := s.arena
	t := arena.AsSmall[Table](a, ref)
	cleared := false
	if t.arrKind == arrGeneric && t.arrLen > 0 {
		arr := t.arrValSlice(a)
		for i := range arr {
			if !arr[i].IsNil() && s.isClearedWeak(c, arr[i]) {
				arr[i] = NilValue()
				cleared = true
			}
		}
	}
	if slots := t.slotsSlice(a); len(slots) > 0 {
		for i := range slots {
			if !slots[i].IsNil() && s.isClearedWeak(c, slots[i]) {
				slots[i] = NilValue()
				cleared = true
			}
		}
	}
	if t.hashRef != arena.NullRef {
		h := arena.AsSmall[tableHash](a, t.hashRef)
		nodes := h.nodesSlice(a)
		for i := range nodes {
			nd := &nodes[i]
			if nd.state == nodeLive && !nd.val.IsNil() && s.isClearedWeak(c, nd.val) {
				h.markDead(a, i)
				if nd.key.IsGCObject() {
					t.hdr.Flags |= tblFlagHasDeadKeys
					s.hasDeadHashKeys = true
				}
				cleared = true
			}
		}
	}
	if cleared {
		s.protoEpoch++
	}
}

func (s *State) gcClearByKeys(c *arena.Collector, ref arena.ObjRef) {
	a := s.arena
	t := arena.AsSmall[Table](a, ref)
	if t.hashRef == arena.NullRef {
		return
	}
	h := arena.AsSmall[tableHash](a, t.hashRef)
	nodes := h.nodesSlice(a)
	cleared := false
	for i := range nodes {
		nd := &nodes[i]
		if nd.state == nodeLive && !nd.val.IsNil() && s.isClearedWeak(c, nd.key) {
			nd.key = deadKeyValue()
			nd.val = NilValue()
			nd.state = nodeDead
			h.live--
			h.dictGen++
			cleared = true
		}
	}
	if cleared {
		s.protoEpoch++
	}
}

func (s *State) gcTraverseThread(c *arena.Collector, th *Thread) int64 {
	a := s.arena
	// Threads always stay on grayAgain so stack writes need zero write barriers (lgc.c:traversethread).
	c.MarkGrayAgain(th.self)

	if th.callerRef != arena.NullRef {
		c.MarkObject(th.callerRef)
	}
	frames := th.framesSlice(a)
	maxFrameEnd := th.top
	for i := range th.nframe {
		cf := &frames[i]
		if cf.clRef != arena.NullRef {
			c.MarkObject(cf.clRef)
			cl := arena.AsSmall[Closure](a, cf.clRef)
			if end := cf.base + int(s.boundProtoOf(cl).maxStack); end > maxFrameEnd {
				maxFrameEnd = end
			}
		}
		s.markValue(c, cf.errFunc)
		s.markValue(c, cf.closeErr)
		s.markValue(c, cf.nativeFn)
		if cf.varargTabRef != arena.NullRef {
			c.MarkObject(cf.varargTabRef)
		}
	}
	s.markValue(c, th.errObj)
	s.markValue(c, th.lastErrNative)
	s.markValue(c, th.yieldNativeFn)
	s.markValue(c, th.hook)
	for _, yv := range th.yieldValsSlice(a) {
		s.markValue(c, yv)
	}
	for _, ce := range th.syncCloseSlice(a) {
		s.markValue(c, ce)
	}

	stack := th.stackSlice(a)
	lim := th.top
	inAtomic := c.State() == arena.GCSAtomic
	if !inAtomic || s.preserveFramesInGC {
		if maxFrameEnd > lim {
			lim = maxFrameEnd
		}
	}
	if lim > len(stack) {
		lim = len(stack)
	}
	for i := range lim {
		s.markValue(c, stack[i])
	}
	maxOpenUpvalIdx := -1
	for _, uvRef := range th.openUpvalsSlice(a) {
		uv := arena.AsSmall[Upvalue](a, uvRef)
		idx := int(uv.stackIdx)
		if idx < len(stack) {
			s.markValue(c, stack[idx])
		}
		if idx > maxOpenUpvalIdx {
			maxOpenUpvalIdx = idx
		}
	}

	if inAtomic {
		if th.nframe < len(frames) {
			clear(frames[th.nframe:])
		}
		if lim < len(stack) {
			fillNil(stack[lim:])
		}
		// Shrink oversized stacks on non-running coroutines (matching lgc.c:traversethread).
		if th.self != s.curRef && th.self != s.mainThreadRef && !c.IsEmergency() {
			if th.status == ThreadDead && th.nframe == 0 && th.top == 0 && th.openUpvalsLen == 0 && th.tbcLen == 0 {
				if th.stackLen > 8 && th.stackRef != arena.NullRef {
					if sref, err := a.Alloc(16); err == nil {
						arena.SliceFast[Value](a, sref, 0, 1)[0] = NilValue()
						a.Free(th.stackRef)
						th.stackRef = sref
						th.stackLen = 1
						th.stackGen++
					}
				}
				if th.framesRef != arena.NullRef {
					a.Free(th.framesRef)
					th.framesRef = arena.NullRef
					th.framesCap = 0
				}
				if th.openUpvalsRef != arena.NullRef {
					a.Free(th.openUpvalsRef)
					th.openUpvalsRef = arena.NullRef
					th.openUpvalsCap = 0
				}
				if th.tbcRef != arena.NullRef {
					a.Free(th.tbcRef)
					th.tbcRef = arena.NullRef
					th.tbcCap = 0
				}
				if th.yieldValsRef != arena.NullRef && th.yieldValsLen == 0 {
					a.Free(th.yieldValsRef)
					th.yieldValsRef = arena.NullRef
					th.yieldValsCap = 0
				}
				if th.syncCloseRef != arena.NullRef && th.syncCloseLen == 0 {
					a.Free(th.syncCloseRef)
					th.syncCloseRef = arena.NullRef
					th.syncCloseCap = 0
				}
			} else if th.status == ThreadSuspended {
				if th.yieldValsRef != arena.NullRef && th.yieldValsLen == 0 && th.yieldValsCap > 16 {
					a.Free(th.yieldValsRef)
					th.yieldValsRef = arena.NullRef
					th.yieldValsCap = 0
				}
				keepEnd := max(lim, maxFrameEnd)
				if maxOpenUpvalIdx+1 > keepEnd {
					keepEnd = maxOpenUpvalIdx + 1
				}
				if tbc := th.tbcSlice(a); len(tbc) > 0 && int(tbc[len(tbc)-1])+1 > keepEnd {
					keepEnd = int(tbc[len(tbc)-1]) + 1
				}
				needed := max(keepEnd+stackSlack, 128)
				if th.stackLen > needed*4 && th.stackLen > 2048 {
					if sref, err := a.Alloc(needed * int(unsafe.Sizeof(Value{}))); err == nil {
						shrunk := arena.SliceFast[Value](a, sref, 0, needed)
						copyLen := min(keepEnd, len(stack))
						copy(shrunk, stack[:copyLen])
						fillNil(shrunk[copyLen:])
						a.Free(th.stackRef)
						th.stackRef = sref
						th.stackLen = needed
						th.stackGen++
					}
				}
			}
		}
	}
	return int64(1 + th.nframe + lim)
}

// gcRemarkUpvals runs during atomic() right after grayAgain propagation (lgc.c:remarkupvals):
// marks the captured stack values of any open upvalue reachable from a live closure,
// even if the upvalue's owning coroutine thread itself is unreachable.
func (s *State) gcRemarkUpvals(c *arena.Collector) {
	a := s.arena
	for _, thRef := range s.twups {
		th := arena.AsSmall[Thread](a, thRef)
		stack := th.stackSlice(a)
		for _, uvRef := range th.openUpvalsSlice(a) {
			if !c.IsWhite(uvRef) {
				uv := arena.AsSmall[Upvalue](a, uvRef)
				if uv.ownerRef == thRef && int(uv.stackIdx) < len(stack) {
					s.markValue(c, stack[uv.stackIdx])
				}
			}
		}
	}
}

// gcPreSweep runs at the end of atomic() after resurrection and weak clearing and right
// before flipping currentWhite and sweeping. Every surviving object is non-White and every
// soon-to-be-freed object is White.
func (s *State) gcPreSweep(c *arena.Collector) {
	a := s.arena

	// 1. Close surviving open upvalues on dead threads; prune dead open upvalues on live threads.
	keptTwups := 0
	for _, thRef := range s.twups {
		th := arena.AsSmall[Thread](a, thRef)
		if c.IsWhite(thRef) {
			stack := th.stackSlice(a)
			for _, uvRef := range th.openUpvalsSlice(a) {
				if !c.IsWhite(uvRef) {
					uv := arena.AsSmall[Upvalue](a, uvRef)
					if uv.ownerRef == thRef {
						val := NilValue()
						if int(uv.stackIdx) < len(stack) {
							val = stack[uv.stackIdx]
						}
						closeUpvalObj(s, uv, val)
					}
				}
			}
			th.openUpvalsLen = 0
			th.hdr.Flags &^= thFlagInTwups
			continue
		}
		// Live thread: unlink dead open upvalues before sweep frees them.
		ups := th.openUpvalsSlice(a)
		k := 0
		for _, uvRef := range ups {
			if !c.IsWhite(uvRef) {
				ups[k] = uvRef
				k++
			}
		}
		th.openUpvalsLen = int32(k)
		if k == 0 {
			th.hdr.Flags &^= thFlagInTwups
			continue
		}
		s.twups[keptTwups] = thRef
		keptTwups++
	}
	s.twups = s.twups[:keptTwups]

	// 2. Prune dead weak shape transitions and metatable shape caches.
	s.shapes.pruneDead(a, c)

	// 3. Sweep inline caches on all surviving ArenaProtos so no IC entry outlives a White shape/table/val.
	s.gcSweepInlineCaches(c)

	// 4. Sweep dead collectable tombstoned hash keys (replacing dead keys with deadKeyValue)
	// and compact dead tables out of s.hashedTables.
	anyDead := false
	keptHashed := 0
	for _, tRef := range s.hashedTables {
		if c.IsWhite(tRef) {
			continue
		}
		t := arena.AsSmall[Table](a, tRef)
		if t.hashRef == arena.NullRef {
			t.hdr.Flags &^= tblFlagTrackedHash | tblFlagHasDeadKeys
			continue
		}
		if s.hasDeadHashKeys && t.hdr.Flags&tblFlagHasDeadKeys != 0 {
			tableHasDead := false
			nodes := arena.AsSmall[tableHash](a, t.hashRef).nodesSlice(a)
			for i := range nodes {
				nd := &nodes[i]
				if nd.state == nodeDead && nd.key.IsGCObject() {
					if c.IsWhite(nd.key.Ref()) {
						nd.key = deadKeyValue()
					} else {
						tableHasDead = true
					}
				}
			}
			if !tableHasDead {
				t.hdr.Flags &^= tblFlagHasDeadKeys
			} else {
				anyDead = true
			}
		}
		s.hashedTables[keptHashed] = tRef
		keptHashed++
	}
	s.hashedTables = s.hashedTables[:keptHashed]
	s.hasDeadHashKeys = anyDead
}

func (s *State) icValAlive(c *arena.Collector, v Value) bool {
	if v.IsGCObject() {
		return !c.IsWhite(v.Ref())
	}
	return true
}

func (s *State) icPrimaryAlive(c *arena.Collector, ic *ICEntry) bool {
	switch ic.kind {
	case icEmpty:
		return false
	case icEnvSlot:
		if ic.holderRef == arena.NullRef || c.IsWhite(ic.holderRef) {
			return false
		}
		h := arena.AsSmall[Table](s.arena, ic.holderRef)
		if h.hashRef == arena.NullRef || arena.AsSmall[tableHash](s.arena, h.hashRef).dictGen != ic.epoch {
			return false
		}
		return true
	case icProtoVal:
		if !ic.noMeta && ic.epoch != s.protoEpoch {
			return false
		}
		return ic.shapeRef != arena.NullRef && !c.IsWhite(ic.shapeRef) && s.icValAlive(c, ic.val)
	case icAddSlot:
		if !ic.noMeta && ic.epoch != s.protoEpoch {
			return false
		}
		return ic.shapeRef != arena.NullRef && !c.IsWhite(ic.shapeRef) &&
			ic.targetShapeRef != arena.NullRef && !c.IsWhite(ic.targetShapeRef)
	case icOwnSlot:
		if !ic.noMeta && ic.epoch != s.protoEpoch {
			return false
		}
		return ic.shapeRef != arena.NullRef && !c.IsWhite(ic.shapeRef)
	default:
		return false
	}
}

func (s *State) icPolyAlive(c *arena.Collector, pe *polyEntry) bool {
	switch pe.kind {
	case icEmpty:
		return false
	case icProtoVal:
		if !pe.noMeta && pe.epoch != s.protoEpoch {
			return false
		}
		return pe.shapeRef != arena.NullRef && !c.IsWhite(pe.shapeRef) && s.icValAlive(c, pe.val)
	case icAddSlot:
		if !pe.noMeta && pe.epoch != s.protoEpoch {
			return false
		}
		return pe.shapeRef != arena.NullRef && !c.IsWhite(pe.shapeRef) &&
			pe.targetShapeRef != arena.NullRef && !c.IsWhite(pe.targetShapeRef)
	case icOwnSlot:
		if !pe.noMeta && pe.epoch != s.protoEpoch {
			return false
		}
		return pe.shapeRef != arena.NullRef && !c.IsWhite(pe.shapeRef)
	default:
		return false
	}
}

func (s *State) gcSweepInlineCaches(c *arena.Collector) {
	a := s.arena
	for idx := 1; idx < len(s.protos); idx++ {
		bp := s.protos[idx]
		if bp == nil || c.IsWhite(bp.arenaRef) {
			continue
		}
		ics := bp.ic
		for i := range ics {
			ic := &ics[i]
			if ic.kind == icEmpty && ic.polyRef == arena.NullRef {
				continue
			}
			if ic.polyRef != arena.NullRef {
				poly := arena.SliceFast[polyEntry](a, ic.polyRef, 0, 4)
				kept := 0
				for j := range int(ic.polyCount) {
					pe := &poly[j]
					if s.icPolyAlive(c, pe) {
						if kept != j {
							poly[kept] = *pe
						}
						kept++
					}
				}
				for j := kept; j < int(ic.polyCount); j++ {
					poly[j] = polyEntry{}
				}
				ic.polyCount = uint8(kept)
				if kept == 0 {
					a.Free(ic.polyRef)
					ic.polyRef = arena.NullRef
				}
			}
			if !s.icPrimaryAlive(c, ic) {
				if ic.polyRef != arena.NullRef && ic.polyCount > 0 {
					poly := arena.SliceFast[polyEntry](a, ic.polyRef, 0, 4)
					pe := poly[0]
					for j := 1; j < int(ic.polyCount); j++ {
						poly[j-1] = poly[j]
					}
					poly[ic.polyCount-1] = polyEntry{}
					ic.polyCount--
					if ic.polyCount == 0 {
						a.Free(ic.polyRef)
						ic.polyRef = arena.NullRef
					}
					ic.shapeRef = pe.shapeRef
					ic.targetShapeRef = pe.targetShapeRef
					ic.holderRef = arena.NullRef
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

func (s *State) gcFreeObject(a *arena.Arena, ref arena.ObjRef) {
	switch a.Header(ref).Kind {
	case gcKindShortStr:
		ls := arena.AsSmall[LuaString](a, ref)
		s.intern.unlinkBucket(ref, ls.Hash)
		a.Free(ref)
	case gcKindLongStr:
		a.Free(ref)
	case gcKindTable:
		t := arena.AsSmall[Table](a, ref)
		t.consumeArrHint(s)
		if t.arrRef != arena.NullRef {
			a.Free(t.arrRef)
		}
		if t.slotsRef != arena.NullRef {
			a.Free(t.slotsRef)
		}
		if t.hashRef != arena.NullRef {
			h := arena.AsSmall[tableHash](a, t.hashRef)
			if h.nodesRef != arena.NullRef {
				a.Free(h.nodesRef)
			}
			a.Free(t.hashRef)
		}
		a.Free(ref)
	case gcKindShape:
		sh := arena.AsSmall[Shape](a, ref)
		if sh.transRef != arena.NullRef {
			a.Free(sh.transRef)
		}
		if sh.indexRef != arena.NullRef {
			a.Free(sh.indexRef)
		}
		a.Free(ref)
	case gcKindClosure:
		cl := arena.AsSmall[Closure](a, ref)
		if cl.upvalsRef != arena.NullRef {
			a.Free(cl.upvalsRef)
		}
		a.Free(ref)
	case gcKindUpvalue:
		a.Free(ref)
	case gcKindNative:
		an := arena.AsSmall[ArenaNative](a, ref)
		if an.upvalsRef != arena.NullRef {
			a.Free(an.upvalsRef)
		}
		if an.upvalIDsRef != arena.NullRef {
			a.Free(an.upvalIDsRef)
		}
		if an.goRef != arena.NullGoHandle && s.goRefs != nil {
			s.goRefs.Free(an.goRef)
		}
		a.Free(ref)
	case gcKindUserdata:
		u := arena.AsSmall[Userdata](a, ref)
		if u.uvalsRef != arena.NullRef {
			a.Free(u.uvalsRef)
		}
		if u.gcRefsRef != arena.NullRef {
			a.Free(u.gcRefsRef)
		}
		if u.dataRef != arena.NullGoHandle && s.goRefs != nil {
			if raw, ok := s.goRefs.Get(u.dataRef); ok {
				if lf, ok := raw.(*luaFile); ok && !lf.closed && !lf.isStd {
					if freed := lf.memBytes(); freed > 0 {
						s.subExtraMem(freed)
					}
				}
			}
			s.goRefs.Free(u.dataRef)
		}
		a.Free(ref)
	case gcKindThread:
		th := arena.AsSmall[Thread](a, ref)
		if th.stackRef != arena.NullRef {
			a.Free(th.stackRef)
		}
		if th.framesRef != arena.NullRef {
			a.Free(th.framesRef)
		}
		if th.openUpvalsRef != arena.NullRef {
			a.Free(th.openUpvalsRef)
		}
		if th.tbcRef != arena.NullRef {
			a.Free(th.tbcRef)
		}
		if th.yieldValsRef != arena.NullRef {
			a.Free(th.yieldValsRef)
		}
		if th.syncCloseRef != arena.NullRef {
			a.Free(th.syncCloseRef)
		}
		if th.uncaughtErrRef != arena.NullGoHandle && s.goRefs != nil {
			s.goRefs.Free(th.uncaughtErrRef)
		}
		if len(s.deferredStackFrees) > 0 {
			keep := s.deferredStackFrees[:0]
			for _, df := range s.deferredStackFrees {
				if df.owner == ref {
					a.Free(df.ref)
				} else {
					keep = append(keep, df)
				}
			}
			s.deferredStackFrees = keep
		}
		a.Free(ref)
	case gcKindProto:
		ap := arena.AsSmall[ArenaProto](a, ref)
		if ap.icRef != arena.NullRef {
			ics := arena.SliceFast[ICEntry](a, ap.icRef, 0, int(ap.nIC))
			for i := range ics {
				if ics[i].polyRef != arena.NullRef {
					a.Free(ics[i].polyRef)
				}
			}
			a.Free(ap.icRef)
		}
		if ap.constsRef != arena.NullRef {
			a.Free(ap.constsRef)
		}
		if ap.subRef != arena.NullRef {
			a.Free(ap.subRef)
		}
		if int(ap.protoIdx) > 0 && int(ap.protoIdx) < len(s.protos) {
			if bp := s.protos[ap.protoIdx]; bp != nil {
				s.subExtraMem(bp.goBytes)
				s.protos[ap.protoIdx] = nil
				if s.jit != nil {
					s.jit.invalidateProto(ap.protoIdx)
				}
				s.freeProtoIDs = append(s.freeProtoIDs, ap.protoIdx)
			}
		}
		a.Free(ref)
	default:
		a.Free(ref)
	}
}

func (s *State) gcCheckSizes(_ *arena.Collector) {
	s.intern.checkSizes()
	s.compactOpenFiles()
}

func (s *State) gcRunFinalizer(_ *arena.Collector, ref arena.ObjRef) {
	var (
		obj Value
		mt  *Table
	)
	switch s.arena.Header(ref).Kind {
	case gcKindTable:
		t := arena.AsSmall[Table](s.arena, ref)
		obj = TableValue(t)
		mt = t.Metatable(s)
	case gcKindUserdata:
		u := arena.AsSmall[Userdata](s.arena, ref)
		obj = UserdataValue(u)
		mt = s.UserdataMeta(u)
	default:
		return
	}
	if mt == nil {
		return
	}
	tm := mt.RawGetStr(s, s.intern.tms[TMGC])
	if tm.IsNil() {
		return
	}
	if !tm.IsClosure() && !tm.IsNative() && s.getTMByObj(tm, TMCall).IsNil() {
		s.emitWarning(fmt.Sprintf("error in __gc (attempt to call a %s value (metamethod '__gc'))", s.objTypeName(tm)))
		return
	}
	th := s.cur()
	if th == nil {
		return
	}
	savedSuppress := th.suppressHook
	th.suppressHook = true
	th.nny++
	th.nextFrameFinalizer = true
	_, err := s.callOnThread(th, tm, []Value{obj})
	th.nextFrameFinalizer = false
	th.nny--
	th.suppressHook = savedSuppress
	if err != nil {
		if errors.Is(err, errCoroCloseSelf) || isUncatchableErr(err) {
			s.abortErr = err
			return
		}
		var le *LuaError
		if errors.As(err, &le) {
			if le.Val.IsString() {
				if werr := s.emitWarningParts("error in __gc (", s.asString(le.Val).Str(), ")"); werr != nil {
					s.abortErr = werr
					return
				}
			} else {
				s.emitWarning("error in __gc (error object is not a string)")
			}
		} else {
			if werr := s.emitWarningParts("error in __gc (", err.Error(), ")"); werr != nil {
				s.abortErr = werr
				return
			}
		}
	}
}

func (s *State) subExtraMem(delta int64) {
	if delta <= 0 {
		return
	}
	if s.extraMemBytes > delta {
		s.extraMemBytes -= delta
	} else {
		s.extraMemBytes = 0
	}
}

// gcNeedsCheck reports whether an automatic GC step is due at the next safepoint.
func (s *State) gcNeedsCheck() bool {
	return s.collector != nil && s.collector.NeedsStep()
}

// gcCheckStep runs one automatic incremental or generational GC step if pacing debt is due.
// liveTop matches PUC-Rio lvm.c checkGC(L, c): only stack[0..liveTop) is live in s.cur.
func (s *State) gcCheckStep(liveTop int) {
	if s.collector == nil || !s.collector.NeedsStep() {
		return
	}
	th := s.cur()
	if th != nil && liveTop > 0 {
		savedTop := th.top
		th.top = liveTop
		s.collector.Step()
		if savedTop > th.top {
			th.top = savedTop
		}
	} else {
		s.collector.Step()
	}
	if err := s.collector.CancelErr(); err != nil && s.abortErr == nil {
		s.abortErr = err
	}
}

// registerFinalizable marks ref (Table or Userdata) for __gc finalization in strict LIFO order
// when setmetatable attaches a metatable containing "__gc" (matching lgc.c:luaC_checkfinalizer).
func (s *State) registerFinalizable(ref arena.ObjRef, mt *Table) {
	if ref == arena.NullRef || mt == nil || s.collector == nil || s.collector.IsClosing() {
		return
	}
	if mt.RawGetStr(s, s.intern.tms[TMGC]).IsNil() {
		return
	}
	s.collector.RegisterFinalizer(ref)
}

func (s *State) registerWeakCandidate(t, mt *Table) {
	if mt == nil {
		return
	}
	if !mt.RawGetStr(s, s.intern.tms[TMMode]).IsNil() {
		t.hdr.Flags |= tblFlagGCWeakCand
	}
}

// Ref pins a Lua Value so Lua's Arena GC keeps it reachable from the Go host (like luaL_ref).
func (s *State) Ref(v Value) int {
	if s.pinnedRefs == nil {
		s.pinnedRefs = make(map[int]Value)
	}
	if s.nextRefID <= 0 {
		s.nextRefID = 1
	}
	id := s.nextRefID
	s.nextRefID++
	s.pinnedRefs[id] = v
	return id
}

// Unref releases a host-pinned reference.
func (s *State) Unref(id int) {
	delete(s.pinnedRefs, id)
}

// CollectFull executes a complete PUC-Rio Lua 5.5.1 GC cycle (exact lgc.c atomic() 6-step order)
// and drains all newly separated __gc finalizers synchronously on the VM thread.
func (s *State) CollectFull() {
	if s.collector == nil {
		return
	}
	s.collector.Full(false)
	if err := s.collector.CancelErr(); err != nil && s.abortErr == nil {
		s.abortErr = err
	}
	s.compactOpenFiles()
}

// collectEmergency runs an emergency GC pass under memory-limit pressure without invoking __gc finalizers.
func (s *State) collectEmergency() {
	if s.collector == nil {
		return
	}
	prev := s.preserveFramesInGC
	s.preserveFramesInGC = true
	s.collector.Full(true)
	s.preserveFramesInGC = prev
	if err := s.collector.CancelErr(); err != nil && s.abortErr == nil {
		s.abortErr = err
	}
}

// Close closes the State: closes remaining mainThread <close> variables, runs ALL remaining
// __gc finalizers in strict LIFO order regardless of reachability (matching lua_close),
// flushes/closes all open files, clears GoRefs, and releases all Arena pages back to Go.
func (s *State) Close() {
	if s.collector == nil {
		s.closeOpenFiles()
		return
	}
	wasInFin := s.collector.InFinalizer()
	if !s.collector.IsClosing() {
		if mt := s.mainThread(); mt != nil {
			mt.nframe = 0
			mt.nny++
			_, _, _ = mt.closeAllSync(s, 0, NilValue(), false)
			mt.nny--
		}
	}
	s.collector.FreeAll()
	s.closeOpenFiles()
	if !wasInFin {
		s.closeJIT()
		s.pinnedRefs = nil
		s.hashedTables = nil
		s.twups = nil
		s.deferredStackFrees = nil
		s.protos = nil
		s.freeProtoIDs = nil
		if s.goRefs != nil {
			s.goRefs.ClearAll()
		}
		s.intern.Close()
		if s.arena != nil {
			s.arena.Close()
		}
		s.collector = nil
	}
}

var gcParamNames = map[string]int{
	"minormul":   arena.ParamMinorMul,
	"majorminor": arena.ParamMajorMinor,
	"minormajor": arena.ParamMinorMajor,
	"pause":      arena.ParamPause,
	"stepmul":    arena.ParamStepMul,
	"stepsize":   arena.ParamStepSize,
}

func nativeCollectGarbage(s *State, args []Value) ([]Value, error) {
	opt := "collect"
	if len(args) >= 1 && !args[0].IsNil() {
		if !args[0].IsString() {
			return nil, fmt.Errorf("bad argument #1 to 'collectgarbage' (string expected, got %s)", s.objTypeName(args[0]))
		}
		opt = s.asString(args[0]).Str()
	}
	inFin := s.collector.InFinalizer()
	switch opt {
	case "collect":
		if inFin {
			return []Value{NilValue()}, nil
		}
		s.CollectFull()
		if s.abortErr != nil {
			return nil, s.abortErr
		}
		return []Value{IntValue(0)}, nil
	case "stop":
		if inFin {
			return []Value{NilValue()}, nil
		}
		s.collector.Stop()
		return []Value{IntValue(0)}, nil
	case "restart":
		if inFin {
			return []Value{NilValue()}, nil
		}
		s.collector.Restart()
		return []Value{IntValue(0)}, nil
	case "count":
		if inFin {
			return []Value{NilValue()}, nil
		}
		kb := float64(s.MemoryUsage()) / 1024.0
		return []Value{FloatValue(kb)}, nil
	case "step":
		var stepBytes int64
		if len(args) >= 2 && !args[1].IsNil() {
			n, ok, nonInt, err := s.coerceIntKind(args[1])
			if err != nil {
				return nil, err
			}
			if !ok {
				if nonInt {
					return nil, fmt.Errorf("bad argument #2 to 'collectgarbage' (number has no integer representation)")
				}
				return nil, fmt.Errorf("bad argument #2 to 'collectgarbage' (number expected, got %s)", s.objTypeName(args[1]))
			}
			if n > 0 {
				stepBytes = n
			}
		}
		if inFin {
			return []Value{NilValue()}, nil
		}
		finished := s.collector.StepBytes(stepBytes)
		if err := s.collector.CancelErr(); err != nil {
			s.abortErr = err
			return nil, err
		}
		if s.abortErr != nil {
			return nil, s.abortErr
		}
		return []Value{BoolValue(finished)}, nil
	case "isrunning":
		if inFin {
			return []Value{NilValue()}, nil
		}
		return []Value{BoolValue(s.collector.IsRunning())}, nil
	case "incremental":
		if inFin {
			return []Value{NilValue()}, nil
		}
		prev := "incremental"
		if s.collector.Mode() != arena.ModeIncremental {
			prev = "generational"
		}
		s.collector.SetMode(arena.ModeIncremental)
		return []Value{StringValue(s.intern.Intern(prev))}, nil
	case "generational":
		if inFin {
			return []Value{NilValue()}, nil
		}
		prev := "incremental"
		if s.collector.Mode() != arena.ModeIncremental {
			prev = "generational"
		}
		s.collector.SetMode(arena.ModeGenMinor)
		return []Value{StringValue(s.intern.Intern(prev))}, nil
	case "param":
		if len(args) < 2 || !args[1].IsString() {
			got := "no value"
			if len(args) >= 2 {
				got = s.objTypeName(args[1])
			}
			return nil, fmt.Errorf("bad argument #2 to 'collectgarbage' (string expected, got %s)", got)
		}
		name := s.asString(args[1]).Str()
		idx, ok := gcParamNames[name]
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'collectgarbage' (invalid option '%s')", name)
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
		if inFin {
			return []Value{NilValue()}, nil
		}
		var prev uint64
		if nv >= 0 {
			prev = s.collector.SetParam(idx, uint64(nv), true)
		} else {
			prev = s.collector.SetParam(idx, 0, false)
		}
		return []Value{IntValue(int64(prev))}, nil
	default:
		return nil, fmt.Errorf("bad argument #1 to 'collectgarbage' (invalid option '%s')", opt)
	}
}
