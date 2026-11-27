package vm

import (
	"unsafe"

	"github.com/yuchanns/moelua/internal/arena"
)

// Accessor layer backing the interpreter, debug, and stdlib call sites over
// per-State noscan Arena storage (all methods are small and inlineable).

// --- State root & GC pacing accessors ---

func (s *State) setCur(th *Thread) {
	if th == nil {
		s.curRef = arena.NullRef
		return
	}
	s.curRef = th.self
}

func (s *State) isMainThread(th *Thread) bool {
	return th != nil && th.self == s.mainThreadRef
}

func (s *State) ownsThread(th *Thread) bool {
	return th != nil && th.self != arena.NullRef && s.arena.IsLive(th.self) && arena.AsSmall[Thread](s.arena, th.self) == th
}

func (s *State) setStringMeta(mt *Table) {
	if mt == nil {
		s.stringMetaRef = arena.NullRef
		s.typeMetaRefs[4] = arena.NullRef
		return
	}
	s.stringMetaRef = mt.self
	s.typeMetaRefs[4] = mt.self
}

func (s *State) gcNeedsStep() bool { return s.collector.NeedsStep() }

func (s *State) recordNewTableAlloc(_ int64, _ int)      {}
func (s *State) noteShapeMetaKeyWrite(_ *Table, _ *LuaString) {}

func (s *State) checkPreallocMem(delta int64) error {
	if s.memLimit > 0 {
		return s.checkMem(delta)
	}
	return nil
}

func (s *State) checkPreallocMemWithRoots(delta int64, r1, r2, r3 Value) error {
	if s.memLimit > 0 {
		return s.checkMemWithRoots(delta, r1, r2, r3)
	}
	return nil
}

func (s *State) tableCreateCheckBytes(nseq, nrec int64) int64 {
	checkBytes := tablePreallocBytes(nseq, nrec)
	if OptimizeEnabled && nseq > 0 {
		// Enforce requested sequence capacity (16B/elem worst case) up front (matching lua_createtable).
		checkBytes += nseq * 8
	}
	return checkBytes
}

func (s *State) accountTableCreateDebt(_ int64) {}

func (s *State) gmatchPreallocBytes() int64 {
	return int64(unsafe.Sizeof(ArenaNative{})) + 3*16 + int64(unsafe.Sizeof(Userdata{})) + 2*16
}

func (s *State) coroWrapPreallocBytes() int64 {
	return threadInitMemBytes(128, defaultFrameCap) +
		arenaRoundedBytes(int(unsafe.Sizeof(ArenaNative{}))) +
		arenaRoundedBytes(int(unsafe.Sizeof(Value{})))
}

// --- Arena GC write barriers ---

func (s *State) barrierTableVal(t *Table, val Value) {
	if val.IsGCObject() && t.hdr.IsBlack() {
		s.barrierBackRef(t.self)
	}
}

func (s *State) barrierBackTable(t *Table) {
	if t.hdr.IsBlack() {
		s.barrierBackRef(t.self)
	}
}

// --- Thread storage & lifecycle accessors ---

func (th *Thread) framesLen() int { return th.framesCap }
func (th *Thread) stackSize() int { return th.stackLen }

func (th *Thread) popTBCAtOrAbove(a *arena.Arena, level int) (int, bool) {
	if th.tbcLen <= 0 {
		return 0, false
	}
	idx := int(th.tbcSlice(a)[th.tbcLen-1])
	if idx < level {
		return 0, false
	}
	th.tbcLen--
	return idx, true
}

func (th *Thread) topTBCIndex(a *arena.Arena) (int, bool) {
	if th.tbcLen <= 0 {
		return 0, false
	}
	return int(th.tbcSlice(a)[th.tbcLen-1]), true
}

func (th *Thread) callerThread(s *State) *Thread {
	if th.callerRef == arena.NullRef {
		return nil
	}
	return arena.AsSmall[Thread](s.arena, th.callerRef)
}

func (th *Thread) setCallerThread(_ *State, c *Thread) {
	if c == nil {
		th.callerRef = arena.NullRef
		return
	}
	th.callerRef = c.self
}

func (th *Thread) clearYieldVals() { th.yieldValsLen = 0 }
func (th *Thread) onSuspend()      {}
func (th *Thread) onResume()       {}

// --- callFrame accessors ---

func (s *State) frameClosure(cf *callFrame) *Closure {
	if cf.clRef == arena.NullRef {
		return nil
	}
	return arena.AsSmall[Closure](s.arena, cf.clRef)
}

func (s *State) setFrameClosure(cf *callFrame, cl *Closure) {
	if cl == nil {
		cf.clRef = arena.NullRef
		return
	}
	cf.clRef = cl.self
}

func (s *State) frameVarargTab(cf *callFrame) *Table {
	if cf.varargTabRef == arena.NullRef {
		return nil
	}
	return arena.AsSmall[Table](s.arena, cf.varargTabRef)
}

func (s *State) setFrameVarargTab(cf *callFrame, t *Table) {
	if t == nil {
		cf.varargTabRef = arena.NullRef
		return
	}
	cf.varargTabRef = t.self
}

func (cf *callFrame) hasVarargTab() bool { return cf.varargTabRef != arena.NullRef }

// --- Closure & Upvalue accessors ---

func (s *State) closureUpvalCount(cl *Closure) int { return int(cl.nUpvals) }

func (s *State) closureUpval(cl *Closure, idx int) *Upvalue {
	a := s.arena
	uvRef := *(*arena.ObjRef)(unsafe.Add(a.PtrSmall(cl.upvalsRef), uintptr(idx)*unsafe.Sizeof(arena.ObjRef(0))))
	return arena.AsSmall[Upvalue](a, uvRef)
}

func (s *State) setClosureUpval(cl *Closure, idx int, uv *Upvalue) {
	uvRef := arena.NullRef
	if uv != nil {
		uvRef = uv.self
	}
	cl.upvalsSlice(s.arena)[idx] = uvRef
	if cl.hdr.IsBlack() && uvRef != arena.NullRef {
		s.collector.Barrier(cl.self, uvRef)
	}
}

func (uv *Upvalue) isClosed() bool            { return uv.ownerRef == arena.NullRef }
func (uv *Upvalue) isOwnedBy(th *Thread) bool { return uv.ownerRef == th.self }

//go:noinline
func (s *State) getCrossThreadUpval(uv *Upvalue) Value {
	return arena.AsSmall[Thread](s.arena, uv.ownerRef).stackSlice(s.arena)[uv.stackIdx]
}

//go:noinline
func (s *State) setCrossThreadUpval(uv *Upvalue, val Value) {
	arena.AsSmall[Thread](s.arena, uv.ownerRef).stackSlice(s.arena)[uv.stackIdx] = val
}

func (s *State) upvalOwner(uv *Upvalue) *Thread {
	if uv.ownerRef == arena.NullRef {
		return nil
	}
	return arena.AsSmall[Thread](s.arena, uv.ownerRef)
}

//go:noinline
func (s *State) barrierObjRef(parent, child arena.ObjRef) {
	s.collector.Barrier(parent, child)
}

func (s *State) setClosedUpvalVal(uv *Upvalue, val Value) {
	uv.closed = val
	if val.tag >= tagFirstGC && uv.hdr.Mark&arena.MarkBlack != 0 {
		s.barrierObjRef(uv.self, val.Ref())
	}
}

func (s *State) newSubClosure(th *Thread, bp *boundProto, curCl *Closure, base, bx int) (*Closure, int64, error) {
	subRef := bp.sub[bx]
	subAP := s.asProto(subRef)
	sbp := s.protos[subAP.protoIdx]
	sub := sbp.proto
	if s.memLimit > 0 {
		estBytes := int64(32 + len(sub.Upvalues)*56)
		if err := s.checkMem(estBytes); err != nil {
			return nil, 0, err
		}
	}
	cl, err := s.allocClosure(subAP, sbp, len(sub.Upvalues))
	if err != nil {
		return nil, 0, s.memError()
	}
	upvals := cl.upvalsSlice(s.arena)
	curUpvals := curCl.upvalsSlice(s.arena)
	for i, ud := range sub.Upvalues {
		if ud.InStack != 0 {
			uv := th.findOrCreateUpval(s, base+int(ud.Idx))
			if uv == nil {
				return nil, 0, s.memError()
			}
			upvals[i] = uv.self
		} else {
			upvals[i] = curUpvals[ud.Idx]
		}
	}
	return cl, 0, nil
}

// --- Dynamic NativeFunc & Userdata helpers ---

func (s *State) allocDynNative(nf *NativeFunc) (Value, error) {
	return s.allocArenaNative(nf)
}

func (s *State) nativeUpvalCount(fn Value) int {
	if fn.IsStaticNative() {
		nf := lookupStaticNative(fn.staticNativeIdx())
		if nf == nil {
			return 0
		}
		return max(len(nf.upvals), int(nf.NumUpvals))
	}
	if fn.IsDynNative() {
		an := s.asArenaNative(fn)
		return max(int(an.nUpvals), int(an.numUpvals))
	}
	return 0
}

func (s *State) nativeUpvalGet(fn Value, idx int) (Value, bool) {
	if !fn.IsDynNative() {
		return NilValue(), false
	}
	ups := s.asArenaNative(fn).upvalsSlice(s.arena)
	if idx < 0 || idx >= len(ups) {
		return NilValue(), false
	}
	return ups[idx], true
}

func (s *State) nativeUpvalSet(fn Value, idx int, val Value) bool {
	if !fn.IsDynNative() {
		return false
	}
	an := s.asArenaNative(fn)
	ups := an.upvalsSlice(s.arena)
	if idx < 0 || idx >= len(ups) {
		return false
	}
	ups[idx] = val
	if val.IsGCObject() && an.hdr.IsBlack() {
		s.collector.Barrier(an.self, val.Ref())
	}
	return true
}

func (s *State) nativeUpvalID(fn Value, idx int) uint64 {
	if fn.IsDynNative() {
		return s.asArenaNative(fn).upvalID(s, idx)
	}
	n := s.nativeUpvalCount(fn)
	sidx := fn.staticNativeIdx()
	if s.immUpvalTokens == nil {
		s.immUpvalTokens = make(map[uint32][]uint64)
	}
	ids := s.immUpvalTokens[sidx]
	if len(ids) < n {
		grown := make([]uint64, n)
		copy(grown, ids)
		ids = grown
		s.immUpvalTokens[sidx] = ids
	}
	if ids[idx] == 0 {
		ids[idx] = globalUpvalTokenID.Add(1)
	}
	return ids[idx]
}

func (s *State) userdataUValCount(u *Userdata) int { return int(u.nUValues) }

func (s *State) userdataUValGet(u *Userdata, idx int) Value {
	return u.uvalsSlice(s.arena)[idx]
}

// --- Table, Shape & IC accessors ---

func (t *Table) hasMeta() bool { return t.metaRef != arena.NullRef }
func (t *Table) hasHash() bool { return t.hashRef != arena.NullRef }

func (ic *ICEntry) hitOwnSlot(t *Table) bool {
	return ic.kind == icOwnSlot && t.shapeRef == ic.shapeRef
}

func (ic *ICEntry) hitSetOwnSlot(t *Table) bool {
	return ic.kind == icOwnSlot && t.shapeRef == ic.shapeRef && t.hdr.Flags&tblFlagIsProto == 0
}

func (t *Table) shapePtr(a *arena.Arena) *Shape {
	if t.shapeRef == arena.NullRef {
		return nil
	}
	return arena.AsSmall[Shape](a, t.shapeRef)
}

func (t *Table) setShape(_ *State, sh *Shape) {
	if sh == nil {
		t.shapeRef = arena.NullRef
		return
	}
	t.shapeRef = sh.self
}

func (t *Table) appendSlotFor(s *State, val Value) bool { return t.appendSlot(s, val) }

func (sh *Shape) lookupKey(s *State, k *LuaString) int32 { return sh.Lookup(s, k) }
func (sh *Shape) hasMeta() bool                          { return sh.metaRef != arena.NullRef }

func (s *State) tableHashValAt(t *Table, slot int) Value {
	h := arena.AsSmall[tableHash](s.arena, t.hashRef)
	return h.nodesSlice(s.arena)[slot].val
}

func (ic *ICEntry) differsFromShape(sh *Shape) bool {
	return ic.shapeRef != arena.NullRef && ic.shapeRef != sh.self
}

func (ic *ICEntry) polyLookupFor(s *State, sh *Shape) bool {
	return ic.polyLookup(s.arena, sh.self)
}

func (ic *ICEntry) polySavePrimaryFor(s *State) {
	ic.polySavePrimary(s)
}

func (ic *ICEntry) setOwnSlotFor(sh *Shape, slot int32, noMeta, protoAbsent bool, epoch uint64) {
	ic.setOwnSlot(sh.self, slot, noMeta, protoAbsent, epoch)
}

func (ic *ICEntry) setEnvSlotFor(s *State, t *Table, slot int32, noMeta bool) {
	h := arena.AsSmall[tableHash](s.arena, t.hashRef)
	ic.setEnvSlot(s.arena, t.self, h.dictGen, slot, noMeta)
}

func (ic *ICEntry) setProtoValFor(sh *Shape, v Value, epoch uint64, noMeta bool) {
	ic.setProtoVal(sh.self, v, epoch, noMeta)
}

func (ic *ICEntry) setAddSlotFor(oldSh, nextSh *Shape, slot int32, epoch uint64, noMeta bool) {
	ic.setAddSlot(oldSh.self, nextSh.self, slot, epoch, noMeta)
}

func (ic *ICEntry) tryPolyGetField(s *State, t *Table) (Value, bool) {
	if ic.kind == icOwnSlot {
		v := t.slotAt(s.arena, int(ic.slot))
		if !v.IsNil() || ic.noMeta || (ic.protoAbsent && s.protoEpoch == ic.epoch) {
			return v, true
		}
	} else if ic.kind == icProtoVal && (ic.noMeta || s.protoEpoch == ic.epoch) {
		return ic.val, true
	}
	return NilValue(), false
}

func (ic *ICEntry) tryPolySetField(s *State, t *Table, val Value) bool {
	if ic.kind == icOwnSlot {
		sp := t.slotPtr(s.arena, int(ic.slot))
		if !sp.IsNil() || ic.noMeta || (ic.protoAbsent && s.protoEpoch == ic.epoch) {
			*sp = val
			if val.IsGCObject() && t.hdr.IsBlack() {
				s.collector.BarrierBack(t.self)
			}
			return true
		}
	} else if ic.kind == icAddSlot && !val.IsNil() && (ic.noMeta || s.protoEpoch == ic.epoch) {
		slot := int(ic.slot)
		if ic.targetShapeRef != arena.NullRef && slot < int(t.slotsCap) {
			t.slotsLen = uint8(slot + 1)
			*t.slotPtr(s.arena, slot) = val
			t.shapeRef = ic.targetShapeRef
			if t.hdr.IsBlack() {
				s.collector.BarrierBack(t.self)
			}
			return true
		}
	}
	return false
}

func (s *State) transitionShapeFor(oldSh *Shape, key *LuaString, tblVal, val Value) (*Shape, error) {
	return s.shapes.Transition(s, oldSh.self, key, tblVal, val)
}

func (s *State) checkShapeSlotMem(t *Table, next *Shape, key *LuaString, val Value) error {
	if s.memLimit > 0 && t.slotsLen == t.slotsCap {
		if delta := t.slotsGrowthDelta(s); delta > 0 {
			prevPinned := s.pinnedShapeRef
			s.pinnedShapeRef = next.self
			err := s.checkMemWithRoots(delta, TableValue(t), StringValue(key), val)
			s.pinnedShapeRef = prevPinned
			return err
		}
	}
	return nil
}

func (s *State) demoteTableToDict(t *Table, key, val Value) error {
	if !t.demoteToDict(s, key, val) {
		return s.ConsumeMemErr()
	}
	return nil
}
