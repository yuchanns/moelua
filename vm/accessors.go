package vm

import (
	"unsafe"

	"github.com/yuchanns/moelua/internal/arena"
)

// Accessor layer decoupling interpreter, debug, and stdlib call sites from
// Go-heap vs noscan Arena storage layouts. Every helper is kept small so the
// Go compiler inlines it cleanly on hot execution paths.

// --- State root & GC pacing accessors ---

func (s *State) cur() *Thread                 { return s.curThread }
func (s *State) setCur(th *Thread)            { s.curThread = th }
func (s *State) mainThread() *Thread          { return s.mainThreadPtr }
func (s *State) isMainThread(th *Thread) bool { return th != nil && th == s.mainThreadPtr }
func (s *State) ownsThread(th *Thread) bool   { return th != nil && th.state == s }
func (s *State) registry() *Table             { return s.registryTbl }
func (s *State) stringMeta() *Table           { return s.stringMetaTbl }
func (s *State) fileMeta() *Table             { return s.fileMetaTbl }
func (s *State) setFileMeta(mt *Table)        { s.fileMetaTbl = mt }
func (s *State) setStringMeta(mt *Table) {
	s.stringMetaTbl = mt
	s.typeMetas[4] = mt
}
func (s *State) addTempBytes(delta int64) { s.gc.tempBytes += delta }
func (s *State) valueAddr(v Value) uintptr {
	if v.ptr == tagTokenLightUD {
		return uintptr(v.bits)
	}
	if v.ptr == tagNullLightUD {
		return 0
	}
	return uintptr(v.ptr)
}
func (s *State) gcNeedsStep() bool       { return s.gc.tier1 }
func (s *State) gcCheckStep(liveTop int) { s.gcCheckDebt(liveTop) }
func (s *State) newSmallRecordTable() *Table {
	t := &Table{shape: s.shapes.root}
	t.slots = t.inlineSlots[:0]
	return t
}
func (s *State) recordNewTableAlloc(estBytes int64, liveTop int) {
	if s.gc.tier1 {
		s.gcRecordAlloc(estBytes, liveTop)
	} else if s.memLimit > 0 {
		s.gc.allocBytes += estBytes
	}
}
func (s *State) noteShapeMetaKeyWrite(t *Table, key *LuaString) {
	if key == s.intern.tms[TMMode] || key == s.intern.tms[TMGC] {
		s.activateTier1()
		if key == s.intern.tms[TMGC] && t == s.fileMetaTbl {
			s.promoteFileFinalizers()
		}
	}
}
func (s *State) allocStaticNative(nf *NativeFunc) Value { return s.NativeValue(nf) }
func registerStaticNative(_ *NativeFunc) uint32         { return 0 }

func (s *State) checkPreallocMem(delta int64) error {
	return s.checkMemAndAccount(delta)
}

func (s *State) checkPreallocMemWithRoots(delta int64, r1, r2, r3 Value) error {
	if s.memLimit > 0 || s.gc.tier1 {
		return s.checkMemAndAccountWithRoots(delta, r1, r2, r3)
	}
	return nil
}

func (s *State) tableCreateCheckBytes(nseq, nrec int64) int64 {
	checkBytes := tablePreallocBytes(nseq, nrec)
	if OptimizeEnabled && nseq > 0 {
		// Enforce requested sequence capacity up front (matching lua_createtable) even when arrHint is lazy.
		checkBytes += nseq * 16
	}
	return checkBytes
}

func (s *State) accountTableCreateDebt(checkBytes int64) {
	s.gc.allocBytes += checkBytes
	s.gcAddDebt(checkBytes)
}

func (s *State) gmatchPreallocBytes() int64 {
	return 48 + 3*16 + 64 + 2*16
}

func (s *State) coroWrapPreallocBytes() int64 {
	return threadInitMemBytes(128, defaultFrameCap) + 48 + 16
}

// --- Write barriers (no-ops under Go GC; active Arena barriers once heap moves into Arena) ---

func (s *State) barrierTableVal(_ *Table, _ Value) {}
func (s *State) barrierBackTable(_ *Table)         {}

// --- Thread slice, frame, & error accessors ---

func (th *Thread) framesSlice(_ *arena.Arena) []callFrame { return th.frames }
func (th *Thread) stackSlice(_ *arena.Arena) []Value      { return th.stack }
func (th *Thread) framesLen() int                         { return len(th.frames) }
func (th *Thread) stackSize() int                         { return len(th.stack) }
func (th *Thread) popTBCAtOrAbove(_ *arena.Arena, level int) (int, bool) {
	if len(th.tbc) == 0 {
		return 0, false
	}
	idx := th.tbc[len(th.tbc)-1]
	if idx < level {
		return 0, false
	}
	th.tbc = th.tbc[:len(th.tbc)-1]
	return idx, true
}
func (th *Thread) topTBCIndex(_ *arena.Arena) (int, bool) {
	if len(th.tbc) == 0 {
		return 0, false
	}
	return th.tbc[len(th.tbc)-1], true
}

func windowIn(stack []Value, base int) *[256]Value {
	return (*[256]Value)(unsafe.Pointer(&stack[base]))
}

func (th *Thread) uncaughtErr(_ *State) error            { return th.uncaughtErrVal }
func (th *Thread) setUncaughtErr(_ *State, err error)    { th.uncaughtErrVal = err }
func (th *Thread) callerThread(_ *State) *Thread       { return th.caller }
func (th *Thread) setCallerThread(_ *State, c *Thread) { th.caller = c }
func (th *Thread) yieldValsSlice(_ *arena.Arena) []Value {
	return th.yieldVals
}
func (th *Thread) clearYieldVals() { th.yieldVals = nil }
func (th *Thread) onSuspend()      { th.detachOpenUpvals() }
func (th *Thread) onResume()       { th.attachOpenUpvals() }
func (th *Thread) setYieldVals(_ *State, vals []Value) error {
	if len(vals) == 0 {
		th.yieldVals = nil
		return nil
	}
	cp := make([]Value, len(vals))
	copy(cp, vals)
	th.yieldVals = cp
	return nil
}

func (s *State) newThread(stackCap, _ int) (*Thread, error) {
	return newThread(s, stackCap), nil
}

// --- callFrame accessors ---

func (s *State) frameClosure(cf *callFrame) *Closure         { return cf.cl }
func (s *State) setFrameClosure(cf *callFrame, cl *Closure)  { cf.cl = cl }
func (s *State) frameVarargTab(cf *callFrame) *Table         { return cf.varargTab }
func (s *State) setFrameVarargTab(cf *callFrame, t *Table)   { cf.varargTab = t }
func (cf *callFrame) hasVarargTab() bool                     { return cf.varargTab != nil }

// --- Closure & Upvalue accessors ---

func (s *State) boundProtoOf(cl *Closure) *boundProto              { return cl.bp }
func (s *State) closureUpvalCount(cl *Closure) int                 { return len(cl.upvals) }
func (s *State) closureUpval(cl *Closure, idx int) *Upvalue        { return cl.upvals[idx] }
func (s *State) setClosureUpval(cl *Closure, idx int, uv *Upvalue) { cl.upvals[idx] = uv }
func (s *State) upvalOwner(uv *Upvalue) *Thread                    { return uv.owner }
func (s *State) setClosedUpvalVal(uv *Upvalue, val Value)          { uv.closed = val }

func (s *State) setChunkEnv(cl *Closure, env Value) error {
	if len(cl.upvals) == 0 {
		return nil
	}
	cl.upvals[0] = &Upvalue{closed: env}
	return nil
}

func (s *State) newSubClosure(th *Thread, bp *boundProto, curCl *Closure, base, bx, liveTop int) (*Closure, error) {
	sub := bp.proto.Protos[bx]
	sbp := bp.sub[bx]
	estBytes := int64(64 + len(sub.Upvalues)*24)
	if sbp == nil && (s.memLimit > 0 || s.gc.tier1) {
		estBytes += protoTreeBytes(sub, OptimizeEnabled)
	}
	if s.memLimit > 0 {
		if err := s.checkMem(estBytes); err != nil {
			return nil, err
		}
	}
	if sbp == nil {
		sbp = s.bindProto(sub)
		bp.sub[bx] = sbp
	}
	cl := &Closure{bp: sbp, upvals: make([]*Upvalue, len(sub.Upvalues))}
	for i, ud := range sub.Upvalues {
		if ud.InStack != 0 {
			cl.upvals[i] = th.findOrCreateUpval(base + int(ud.Idx))
		} else {
			cl.upvals[i] = curCl.upvals[ud.Idx]
		}
	}
	if s.gc.tier1 {
		s.gcRecordAlloc(estBytes, liveTop)
	} else if s.memLimit > 0 {
		s.gc.allocBytes += estBytes
	}
	return cl, nil
}

// --- Dynamic NativeFunc & Userdata helpers ---

func (s *State) allocDynNative(nf *NativeFunc) (Value, error) {
	return s.NativeValue(nf), nil
}

func (s *State) nativeUpvalCount(fn Value) int {
	if !fn.IsNative() {
		return 0
	}
	nf := s.asNative(fn)
	return max(len(nf.upvals), int(nf.NumUpvals))
}

func (s *State) nativeUpvalGet(fn Value, idx int) (Value, bool) {
	if !fn.IsNative() {
		return NilValue(), false
	}
	nf := s.asNative(fn)
	if idx < 0 || idx >= len(nf.upvals) {
		return NilValue(), false
	}
	return nf.upvals[idx], true
}

func (s *State) nativeUpvalSet(fn Value, idx int, val Value) bool {
	if !fn.IsNative() {
		return false
	}
	nf := s.asNative(fn)
	if idx < 0 || idx >= len(nf.upvals) {
		return false
	}
	nf.upvals[idx] = val
	return true
}

func (s *State) nativeUpvalID(fn Value, idx int) uint64 {
	nf := s.asNative(fn)
	n := max(len(nf.upvals), int(nf.NumUpvals))
	if !nf.Collectable {
		if s.immUpvalTokens == nil {
			s.immUpvalTokens = make(map[*NativeFunc][]uint64)
		}
		ids := s.immUpvalTokens[nf]
		if len(ids) < n {
			grown := make([]uint64, n)
			copy(grown, ids)
			ids = grown
			s.immUpvalTokens[nf] = ids
		}
		if ids[idx] == 0 {
			ids[idx] = globalUpvalTokenID.Add(1)
		}
		return ids[idx]
	}
	if len(nf.upvalIDs) < n {
		grown := make([]uint64, n)
		copy(grown, nf.upvalIDs)
		nf.upvalIDs = grown
	}
	if nf.upvalIDs[idx] == 0 {
		nf.upvalIDs[idx] = globalUpvalTokenID.Add(1)
	}
	return nf.upvalIDs[idx]
}

func (s *State) SetUserdataGCRefs(u *Userdata, refs []Value) error {
	u.gcRefs = refs
	return nil
}

func (s *State) userdataUValCount(u *Userdata) int {
	return len(u.UValues)
}

func (s *State) userdataUValGet(u *Userdata, idx int) Value {
	return u.UValues[idx]
}

func (s *State) SetUserdataUValue(u *Userdata, idx int, val Value) {
	u.UValues[idx] = val
}

// --- Table, Shape & IC accessors ---

func (t *Table) hasMeta() bool                       { return t.meta != nil }
func (t *Table) hasHash() bool                       { return t.hash != nil }
func (t *Table) shapePtr(_ *arena.Arena) *Shape      { return t.shape }
func (t *Table) slotAt(_ *arena.Arena, i int) Value  { return t.slots[i] }
func (t *Table) slotPtr(_ *arena.Arena, i int) *Value { return &t.slots[i] }
func (t *Table) slotsFull() bool                     { return len(t.slots) == cap(t.slots) }
func (t *Table) slotsCapacity() int                  { return cap(t.slots) }
func (t *Table) setShape(_ *State, sh *Shape)        { t.shape = sh }
func (t *Table) appendSlotFor(_ *State, val Value) bool {
	t.appendSlot(val)
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
		return ic.protoValue()
	}
	return NilValue(), false
}

func (ic *ICEntry) tryPolySetField(s *State, t *Table, val Value) bool {
	if ic.kind == icOwnSlot {
		if !t.slots[ic.slot].IsNil() || ic.noMeta || (ic.protoAbsent && s.protoEpoch == ic.epoch) {
			t.slots[ic.slot] = val
			return true
		}
	} else if ic.kind == icAddSlot && !val.IsNil() && (ic.noMeta || s.protoEpoch == ic.epoch) {
		slot := int(ic.slot)
		if ts := ic.targetShape.Value(); ts != nil && slot < cap(t.slots) {
			t.slots = t.slots[:slot+1]
			t.slots[slot] = val
			t.shape = ts
			return true
		}
	}
	return false
}

func (s *State) transitionShapeFor(oldSh *Shape, key *LuaString, tblVal, val Value) (*Shape, error) {
	return s.shapes.Transition(s, oldSh, key, tblVal, val)
}

func (s *State) checkShapeSlotMem(t *Table, next *Shape, key *LuaString, val Value) error {
	if (s.memLimit > 0 || s.gc.tier1) && t.slotsFull() {
		prevPinned := s.gc.pinnedShape
		s.gc.pinnedShape = next
		err := s.checkMemAndAccountWithRoots(int64(max(t.slotsCapacity(), 4))*16, TableValue(t), StringValue(key), val)
		s.gc.pinnedShape = prevPinned
		return err
	}
	return nil
}

func (s *State) demoteTableToDict(t *Table, key, val Value) error {
	t.demoteToDict(s, key, val)
	return s.ConsumeMemErr()
}
