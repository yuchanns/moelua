package vm

import (
	"unsafe"

	"github.com/yuchanns/moelua/internal/arena"
)

// Accessor layer backing the interpreter, debug, and stdlib call sites (Step 5c:
// noscan Value, Arena String/Closure/Upvalue/Native/Userdata/Proto + GoRefs, and
// arena.Collector with thin Arena stubs bridging Table & Thread until Step 5d).

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

func (s *State) newSmallRecordTable() *Table {
	t := &Table{shape: s.shapes.root}
	t.slots = t.inlineSlots[:0]
	s.allocTableStub(t)
	s.addTableMem(t, 80)
	return t
}

func (s *State) gcNeedsStep() bool { return s.collector.NeedsStep() }

func (s *State) recordNewTableAlloc(_ int64, _ int)          {}
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
		checkBytes += nseq * 8
	}
	return checkBytes
}

func (s *State) accountTableCreateDebt(_ int64) {}

func (s *State) gmatchPreallocBytes() int64 {
	return int64(unsafe.Sizeof(ArenaNative{})) + 3*16 + int64(unsafe.Sizeof(Userdata{})) + 2*16
}

func (s *State) coroWrapPreallocBytes() int64 {
	return threadInitMemBytes(128, defaultFrameCap) + int64(unsafe.Sizeof(ArenaNative{})) + 16
}

// --- Arena GC write barriers ---

func (s *State) barrierTableVal(t *Table, val Value) {
	if val.IsGCObject() && t.self != arena.NullRef && s.arena.Header(t.self).IsBlack() {
		s.collector.BarrierBack(t.self)
	}
}

func (s *State) barrierBackTable(t *Table) {
	if t.self != arena.NullRef && s.arena.Header(t.self).IsBlack() {
		s.collector.BarrierBack(t.self)
	}
}

// --- Thread storage & lifecycle accessors ---

func (th *Thread) framesLen() int       { return th.framesCap }
func (th *Thread) stackSize() int       { return th.stackLen }
func (th *Thread) openUpvalsCount() int { return int(th.openUpvalsLen) }

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
	return s.tableByRef(cf.varargTabRef)
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
	return arena.AsSmall[Upvalue](s.arena, cl.upvalsSlice(s.arena)[idx])
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

func (s *State) upvalOwner(uv *Upvalue) *Thread {
	if uv.ownerRef == arena.NullRef {
		return nil
	}
	return arena.AsSmall[Thread](s.arena, uv.ownerRef)
}

func (s *State) setClosedUpvalVal(uv *Upvalue, val Value) {
	uv.closed = val
	if val.IsGCObject() && uv.hdr.IsBlack() {
		s.collector.Barrier(uv.self, val.Ref())
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

func (t *Table) hasMeta() bool                         { return t.meta != nil }
func (t *Table) hasHash() bool                         { return t.hash != nil }
func (t *Table) shapePtr(_ *arena.Arena) *Shape        { return t.shape }
func (t *Table) setShape(_ *State, sh *Shape)          { t.shape = sh }
func (t *Table) slotAt(_ *arena.Arena, slot int) Value { return t.slots[slot] }
func (t *Table) slotPtr(_ *arena.Arena, slot int) *Value {
	return &t.slots[slot]
}
func (t *Table) appendSlotFor(s *State, val Value) bool {
	oldCap := cap(t.slots)
	t.appendSlot(val)
	if cap(t.slots) > oldCap {
		s.addTableMem(t, int64(cap(t.slots)-oldCap)*16)
	}
	return true
}

func (sh *Shape) lookupKey(_ *State, k *LuaString) int32 { return sh.Lookup(k) }
func (sh *Shape) hasMeta() bool                          { return sh.meta != nil }

func (s *State) tableHashValAt(t *Table, slot int) Value {
	return t.hash.nodes[slot].val
}

func (ic *ICEntry) differsFromShape(sh *Shape) bool {
	return ic.shapeID != 0 && ic.shapeID != sh.id
}

func (ic *ICEntry) polyLookupFor(_ *State, sh *Shape) bool {
	return ic.polyLookup(sh.id)
}

func (ic *ICEntry) polySavePrimaryFor(_ *State) {
	ic.polySavePrimary()
}

func (ic *ICEntry) setOwnSlotFor(sh *Shape, slot int32, noMeta, protoAbsent bool, epoch uint64) {
	ic.setOwnSlot(sh.id, slot, noMeta, protoAbsent, epoch)
}

func (ic *ICEntry) setEnvSlotFor(_ *State, t *Table, slot int32, noMeta bool) {
	ic.setEnvSlot(t, t.hash.dictGen, slot, noMeta)
}

func (ic *ICEntry) setProtoValFor(sh *Shape, v Value, epoch uint64, noMeta bool) {
	ic.setProtoVal(sh.id, v, epoch, noMeta)
}

func (ic *ICEntry) setAddSlotFor(oldSh, nextSh *Shape, slot int32, epoch uint64, noMeta bool) {
	ic.setAddSlot(oldSh.id, nextSh, slot, epoch, noMeta)
}

func (ic *ICEntry) tryPolyGetField(s *State, t *Table) (Value, bool) {
	if ic.kind == icOwnSlot {
		v := t.slots[ic.slot]
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
		if !t.slots[ic.slot].IsNil() || ic.noMeta || (ic.protoAbsent && s.protoEpoch == ic.epoch) {
			t.slots[ic.slot] = val
			s.barrierTableVal(t, val)
			return true
		}
	} else if ic.kind == icAddSlot && !val.IsNil() && (ic.noMeta || s.protoEpoch == ic.epoch) {
		slot := int(ic.slot)
		if ts := ic.targetShape.Value(); ts != nil && slot < cap(t.slots) {
			t.slots = t.slots[:slot+1]
			t.slots[slot] = val
			t.shape = ts
			s.barrierBackTable(t)
			return true
		}
	}
	return false
}

func (s *State) transitionShapeFor(oldSh *Shape, key *LuaString, tblVal, val Value) (*Shape, error) {
	return s.shapes.Transition(s, oldSh, key, tblVal, val)
}

func (s *State) checkShapeSlotMem(t *Table, next *Shape, key *LuaString, val Value) error {
	if s.memLimit > 0 && len(t.slots) == cap(t.slots) {
		prevPinned := s.pinnedShape
		s.pinnedShape = next
		err := s.checkMemWithRoots(int64(max(cap(t.slots), 4))*16, TableValue(t), StringValue(key), val)
		s.pinnedShape = prevPinned
		return err
	}
	return nil
}

func (s *State) demoteTableToDict(t *Table, key, val Value) error {
	if !t.demoteToDict(s, key, val) {
		return s.ConsumeMemErr()
	}
	return nil
}
