package vm

import (
	"fmt"
	"math"
	"unsafe"

	"github.com/yuchanns/moelua/internal/arena"
)

type arrKind uint8

const (
	arrEmpty   arrKind = iota
	arrInt             // homogeneous 1..N dense []int64 in Arena
	arrFloat           // homogeneous 1..N dense []float64 in Arena
	arrBool            // homogeneous 1..N dense []bool in Arena
	arrGeneric         // general 1..N []Value in Arena (may contain nil holes)
)

const (
	tblFlagIsProto     uint8 = 1 << 0
	tblFlagGCWeakCand  uint8 = 1 << 1
	tblFlagHasDeadKeys uint8 = 1 << 2
	tblFlagTrackedHash uint8 = 1 << 3
)

const (
	nodeEmpty uint8 = 0
	nodeLive  uint8 = 1
	nodeDead  uint8 = 2 // deleted tombstone (preserves probe chains & live next() keys; dead collectable keys swept in PreSweep)
)

// hashNode is a 40-byte noscan open-addressing hash slot stored inside State.arena.
type hashNode struct {
	key   Value // 16B
	val   Value // 16B
	state uint8 // 1B: nodeEmpty / nodeLive / nodeDead
	_     [7]byte
}

const (
	_ = uint(40 - unsafe.Sizeof(hashNode{}))
	_ = uint(unsafe.Sizeof(hashNode{}) - 40)
)

// tableHash is a 32-byte noscan hash/dict header stored inside State.arena.
// Pure record tables (Shape/slots) and pure dense arrays never allocate a tableHash.
type tableHash struct {
	nodesRef arena.ObjRef // 4B: raw Arena []hashNode (length == mask + 1)
	mask     uint32       // 4B
	dictGen  uint64       // 8B: bumped on rehash / structural change for _ENV IC
	count    int32        // 4B: live + dead slots (for load factor)
	live     int32        // 4B: active non-nil entries
	_        uint64       // 8B pad -> 32B exact
}

const (
	_ = uint(32 - unsafe.Sizeof(tableHash{}))
	_ = uint(unsafe.Sizeof(tableHash{}) - 32)
)

func (h *tableHash) nodesSlice(a *arena.Arena) []hashNode {
	if h.nodesRef == arena.NullRef {
		return nil
	}
	return arena.SliceFast[hashNode](a, h.nodesRef, 0, int(h.mask)+1)
}

func (h *tableHash) markDead(a *arena.Arena, slot int) {
	nd := &h.nodesSlice(a)[slot]
	nd.val = NilValue()
	nd.state = nodeDead
	h.live--
	h.dictGen++
}

// Table is an 80-byte zero-pointer (noscan, ptrdata == 0) Lua 5.5 table stored in State.arena.
// Supports both:
//   - Gate 1 (-tags moeluafaithful): hybrid Arena []Value array + open-addressing hash map
//   - Gate 2 (default): homogeneous Arena []int64 / []float64 / []bool arrays + Shape/slots + lazy dict fallback
type Table struct {
	hdr         arena.GCHeader // 8B: Next, Kind=gcKindTable, Mark, Class, Flags(tblFlag*)
	self        arena.ObjRef   // 4B: own ObjRef inside State.arena
	shapeRef    arena.ObjRef   // 4B: Shape ObjRef (NullRef in dict/faithful mode)
	metaRef     arena.ObjRef   // 4B: metatable Table ObjRef (NullRef if none)
	hashRef     arena.ObjRef   // 4B: raw Arena tableHash ObjRef (NullRef if none)
	arrRef      arena.ObjRef   // 4B: raw Arena backing array ObjRef (NullRef if arrEmpty)
	slotsRef    arena.ObjRef   // 4B: raw Arena []Value for record slots (NullRef => uses inlineSlots)
	arrLen      uint32         // 4B: active length of array part
	arrCap      uint32         // 4B: element capacity of arrRef
	arrHint     int32          // 4B: preallocated capacity hint from table.create / NEWTABLE
	slotsLen    uint8          // 1B: active record slots (0..64)
	slotsCap    uint8          // 1B: record slots capacity (2 when inline, <=64 when external)
	arrKind     arrKind        // 1B: discriminator for arrRef
	tmFlags     uint8          // 1B: negative metamethod cache (bits 0..5 for TMIndex..TMEq)
	inlineSlots [2]Value       // 32B: co-allocates up to 2 record fields inline inside the 80B Table!
}

// Compile-time assertion: sizeof(Table) == 80 bytes (exact Arena 80B slab class, ptrdata == 0).
const (
	_ = uint(80 - unsafe.Sizeof(Table{}))
	_ = uint(unsafe.Sizeof(Table{}) - 80)
)

func (t *Table) isProto() bool { return t.hdr.Flags&tblFlagIsProto != 0 }

func (t *Table) arrIntSlice(a *arena.Arena) []int64 {
	return arena.SliceFast[int64](a, t.arrRef, 0, int(t.arrLen))
}

func (t *Table) arrFloatSlice(a *arena.Arena) []float64 {
	return arena.SliceFast[float64](a, t.arrRef, 0, int(t.arrLen))
}

func (t *Table) arrBoolSlice(a *arena.Arena) []bool {
	return arena.SliceFast[bool](a, t.arrRef, 0, int(t.arrLen))
}

func (t *Table) arrValSlice(a *arena.Arena) []Value {
	return arena.SliceFast[Value](a, t.arrRef, 0, int(t.arrLen))
}

func (t *Table) slotsSlice(a *arena.Arena) []Value {
	if t.slotsLen == 0 {
		return nil
	}
	if t.slotsRef == arena.NullRef {
		return t.inlineSlots[:t.slotsLen]
	}
	return arena.SliceFast[Value](a, t.slotsRef, 0, int(t.slotsLen))
}

func (t *Table) slotAt(a *arena.Arena, idx int) Value {
	if t.slotsRef == arena.NullRef {
		return t.inlineSlots[idx]
	}
	return arena.SliceFast[Value](a, t.slotsRef, 0, int(t.slotsCap))[idx]
}

func (t *Table) slotPtr(a *arena.Arena, idx int) *Value {
	if t.slotsRef == arena.NullRef {
		return &t.inlineSlots[idx]
	}
	return &arena.SliceFast[Value](a, t.slotsRef, 0, int(t.slotsCap))[idx]
}

func (s *State) allocArrBuf(elemSize, capElems int) (arena.ObjRef, error) {
	ref, err := s.arena.Alloc(elemSize * capElems)
	if err != nil {
		return arena.NullRef, err
	}
	s.collector.AddDebt(int64(s.arena.SlotSize(ref)))
	return ref, nil
}

func hashCapFor(nrec int64) int64 {
	if nrec <= 0 {
		return 0
	}
	sz := int64(4)
	need := nrec * 2
	for sz < need {
		if sz > (1 << 30) {
			return sz
		}
		sz <<= 1
	}
	return sz
}

func arenaAllocSize(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if n > (1 << 30) {
		return n
	}
	_, sz := arena.SizeClassFor(int(n))
	return int64(sz)
}

func (t *Table) consumeArrHint(s *State) {
	if t.arrHint > 0 {
		s.subExtraMem(int64(t.arrHint) * 8)
		t.arrHint = 0
	}
}

// tablePreallocBytes returns the exact backing-store bytes allocated by NewTable(nseq, nrec).
func tablePreallocBytes(nseq, nrec int64) int64 {
	structBytes := arenaAllocSize(int64(unsafe.Sizeof(Table{})))
	hashHdrBytes := arenaAllocSize(int64(unsafe.Sizeof(tableHash{})))
	var arrBytes, recBytes int64
	if OptimizeEnabled {
		if nseq > 0 {
			arrBytes = nseq * 8
		}
		if nrec > int64(len(Table{}.inlineSlots)) && nrec <= maxShapeProps {
			recBytes = arenaAllocSize(nrec * 16)
		} else if nrec > maxShapeProps {
			recBytes = hashHdrBytes + arenaAllocSize(hashCapFor(nrec)*int64(unsafe.Sizeof(hashNode{})))
		}
	} else {
		if nseq > 0 {
			arrBytes = arenaAllocSize(nseq * 16)
		}
		if nrec > 0 {
			recBytes = hashHdrBytes + arenaAllocSize(hashCapFor(nrec)*int64(unsafe.Sizeof(hashNode{})))
		}
	}
	return structBytes + arrBytes + recBytes
}

func (t *Table) arrGrowthDelta(s *State, nc int, elemSize int64) int64 {
	newBytes := arenaAllocSize(int64(nc) * elemSize)
	var oldBytes int64
	if t.arrRef != arena.NullRef {
		oldBytes = int64(s.arena.SlotSize(t.arrRef))
	}
	if newBytes <= oldBytes {
		return 0
	}
	return newBytes - oldBytes
}

func (t *Table) slotsGrowthDelta(s *State) int64 {
	newCap := min(max(int(t.slotsCap)*2, 4), maxShapeProps)
	newBytes := arenaAllocSize(int64(newCap) * 16)
	var oldBytes int64
	if t.slotsRef != arena.NullRef {
		oldBytes = int64(s.arena.SlotSize(t.slotsRef))
	}
	if newBytes <= oldBytes {
		return 0
	}
	return newBytes - oldBytes
}

// TryNewTable allocates a Lua table after enforcing State memory limits and accounting
// the requested backing capacity. Suitable for host embedding APIs and table.create.
func (s *State) TryNewTable(nseq, nrec int) (*Table, error) {
	if nseq < 0 {
		nseq = 0
	}
	if nrec < 0 {
		nrec = 0
	}
	if int64(nrec) > (1<<26) || int64(nseq) > (1<<28) {
		return nil, fmt.Errorf("table overflow")
	}
	actualBytes := tablePreallocBytes(int64(nseq), int64(nrec))
	checkBytes := actualBytes
	if OptimizeEnabled && nseq > 0 {
		checkBytes += int64(nseq) * 8
	}
	if err := s.checkMem(checkBytes); err != nil {
		return nil, err
	}
	t := s.NewTable(nseq, nrec)
	if t == nil {
		return nil, s.memError()
	}
	return t, nil
}

// NewTable allocates a noscan Lua table inside State.arena with sequence/record capacity hints.
func (s *State) NewTable(nseq, nrec int) *Table {
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(Table{})), gcKindTable)
	if err != nil {
		s.memExceeded = true
		return nil
	}
	t := arena.AsSmall[Table](s.arena, ref)
	t.self = ref
	t.inlineSlots[0] = NilValue()
	t.inlineSlots[1] = NilValue()
	if OptimizeEnabled {
		t.shapeRef = s.shapes.rootRef
		if nseq > 0 {
			t.arrHint = int32(nseq)
			hintBytes := int64(nseq) * 8
			s.extraMemBytes += hintBytes
			s.collector.AddDebt(hintBytes)
		}
		if nrec <= len(t.inlineSlots) {
			t.slotsCap = uint8(len(t.inlineSlots))
		} else if nrec <= maxShapeProps {
			sref, serr := s.allocArrBuf(16, nrec)
			if serr != nil {
				s.memExceeded = true
				return t
			}
			fillNil(arena.SliceFast[Value](s.arena, sref, 0, nrec))
			t.slotsRef = sref
			t.slotsCap = uint8(nrec)
		} else {
			t.shapeRef = arena.NullRef
			t.initHash(s, nrec)
		}
		return t
	}
	// Faithful mode (Gate 1): direct Arena []Value + hash table.
	if nseq > 0 {
		aref, aerr := s.allocArrBuf(16, nseq)
		if aerr != nil {
			s.memExceeded = true
			return t
		}
		fillNil(arena.SliceFast[Value](s.arena, aref, 0, nseq))
		t.arrRef = aref
		t.arrLen = uint32(nseq)
		t.arrCap = uint32(nseq)
		t.arrKind = arrGeneric
	}
	if nrec > 0 {
		t.initHash(s, nrec)
	}
	return t
}

// newSmallRecordTable is the fast inlineable OP_NEWTABLE helper for empty/small record tables (<= 2 fields).
func (s *State) newSmallRecordTable() *Table {
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(Table{})), gcKindTable)
	if err != nil {
		return nil
	}
	t := arena.AsSmall[Table](s.arena, ref)
	t.self = ref
	t.shapeRef = s.shapes.rootRef
	t.slotsCap = 2
	t.inlineSlots[0] = NilValue()
	t.inlineSlots[1] = NilValue()
	return t
}

// NewDictTable allocates a table directly in dict mode (ideal for _ENV / globals).
func (s *State) NewDictTable(nrec int) *Table {
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(Table{})), gcKindTable)
	if err != nil {
		s.memExceeded = true
		return nil
	}
	t := arena.AsSmall[Table](s.arena, ref)
	t.self = ref
	t.inlineSlots[0] = NilValue()
	t.inlineSlots[1] = NilValue()
	if nrec < 16 {
		nrec = 16
	}
	t.initHash(s, nrec)
	return t
}

func fillNil(vs []Value) {
	n := NilValue()
	for i := range vs {
		vs[i] = n
	}
}

func (t *Table) Metatable(s *State) *Table {
	if t == nil || t.metaRef == arena.NullRef {
		return nil
	}
	return arena.AsSmall[Table](s.arena, t.metaRef)
}

func (s *State) tableBarrier(t *Table, v Value) {
	if v.IsGCObject() && t.hdr.IsBlack() {
		s.collector.BarrierBack(t.self)
	}
}

func (s *State) tableBarrierRef(t *Table, ref arena.ObjRef) {
	if ref != arena.NullRef && t.hdr.IsBlack() {
		s.collector.BarrierBack(t.self)
	}
}

// SetMetatable sets t's metatable, updating Shape (in opt mode), tmFlags, proto flags,
// write barriers, weak-candidate flag, and Collector finalizer registration.
func (s *State) SetMetatable(t, mt *Table) {
	var mtRef arena.ObjRef
	if mt != nil {
		mtRef = mt.self
	}
	var newShapeRef arena.ObjRef
	if OptimizeEnabled && t.shapeRef != arena.NullRef {
		var ok bool
		newShapeRef, ok = s.shapes.WithMeta(s, t.shapeRef, mtRef, TableValue(t))
		if !ok {
			s.memExceeded = true
			return
		}
	}
	if mt != nil {
		mt.markProto(s)
		if idx := mt.RawGetStr(s, s.intern.tms[TMIndex]); idx.IsTable() {
			s.asTable(idx).markProto(s)
		}
		if nidx := mt.RawGetStr(s, s.intern.tms[TMNewIndex]); nidx.IsTable() {
			s.asTable(nidx).markProto(s)
		}
		s.registerFinalizable(t.self, mt)
		s.registerWeakCandidate(t, mt)
	}
	t.metaRef = mtRef
	t.tmFlags = 0
	if OptimizeEnabled {
		if newShapeRef != arena.NullRef {
			t.shapeRef = newShapeRef
		}
		if t.isProto() {
			s.protoEpoch++
		}
	}
	if t.hdr.IsBlack() && (mtRef != arena.NullRef || newShapeRef != arena.NullRef) {
		s.collector.BarrierBack(t.self)
	}
}

func (t *Table) markProto(s *State) {
	if t.hdr.Flags&tblFlagIsProto == 0 {
		t.hdr.Flags |= tblFlagIsProto
		s.protoEpoch++
	}
}

func (t *Table) initHash(s *State, n int) bool {
	sz := 4
	for sz < n*2 {
		sz <<= 1
	}
	var h *tableHash
	if t.hashRef == arena.NullRef {
		href, err := s.arena.AllocZeroed(int(unsafe.Sizeof(tableHash{})))
		if err != nil {
			s.memExceeded = true
			return false
		}
		s.collector.AddDebt(int64(s.arena.SlotSize(href)))
		t.hashRef = href
		h = arena.AsSmall[tableHash](s.arena, href)
	} else {
		h = arena.AsSmall[tableHash](s.arena, t.hashRef)
		if h.nodesRef != arena.NullRef {
			s.arena.Free(h.nodesRef)
			h.nodesRef = arena.NullRef
		}
	}
	nref, err := s.arena.AllocZeroed(sz * int(unsafe.Sizeof(hashNode{})))
	if err != nil {
		s.memExceeded = true
		return false
	}
	s.collector.AddDebt(int64(s.arena.SlotSize(nref)))
	h.nodesRef = nref
	h.mask = uint32(sz - 1)
	h.count = 0
	h.live = 0
	h.dictGen++
	s.trackHashedTable(t.self)
	return true
}

func (t *Table) checkHashMem(s *State, key, val Value) error {
	if s == nil || s.memLimit <= 0 {
		return nil
	}
	if t.hashRef == arena.NullRef {
		return s.checkMemWithRoots(32+arenaAllocSize(hashCapFor(4)*40), TableValue(t), key, val)
	}
	h := arena.AsSmall[tableHash](s.arena, t.hashRef)
	if h.nodesRef == arena.NullRef {
		return s.checkMemWithRoots(arenaAllocSize(hashCapFor(4)*40), TableValue(t), key, val)
	}
	curLen := int(h.mask) + 1
	curCnt := int(h.count)
	if (curCnt+1)*10 >= curLen*7 {
		growSz := max(curLen*2, 4)
		if int(h.live)*4 < curLen && curLen >= 8 {
			growSz = curLen
		}
		if growSz > curLen {
			delta := arenaAllocSize(int64(growSz)*40) - int64(s.arena.SlotSize(h.nodesRef))
			if delta > 0 {
				return s.checkMemWithRoots(delta, TableValue(t), key, val)
			}
		}
	}
	return nil
}

func (t *Table) growHash(s *State) bool {
	h := arena.AsSmall[tableHash](s.arena, t.hashRef)
	oldRef := h.nodesRef
	oldLen := int(h.mask) + 1
	if oldRef == arena.NullRef {
		oldLen = 0
	}
	newSz := oldLen * 2
	if newSz < 4 {
		newSz = 4
	}
	if int(h.live)*4 < oldLen && oldLen >= 8 {
		newSz = oldLen
	}
	newRef, err := s.arena.AllocZeroed(newSz * int(unsafe.Sizeof(hashNode{})))
	if err != nil {
		s.memExceeded = true
		return false
	}
	s.collector.AddDebt(int64(s.arena.SlotSize(newRef)))
	h.nodesRef = newRef
	h.mask = uint32(newSz - 1)
	h.count = 0
	h.live = 0
	h.dictGen++
	if oldRef != arena.NullRef {
		old := arena.SliceFast[hashNode](s.arena, oldRef, 0, oldLen)
		for i := range old {
			nd := &old[i]
			if nd.state == nodeLive && !nd.val.IsNil() {
				t.hashPutNoGrow(s, nd.key, nd.val)
			}
		}
		s.arena.Free(oldRef)
	}
	return true
}

func hashValue(s *State, v Value) uint32 {
	switch {
	case v.IsInt():
		x := v.bits
		x ^= x >> 33
		x *= 0xff51afd7ed558ccd
		x ^= x >> 33
		return uint32(x)
	case v.IsString():
		return s.asString(v).hash()
	case v.IsFloat():
		f := v.Float()
		if f == 0 {
			return 0
		}
		x := math.Float64bits(f)
		x ^= x >> 33
		x *= 0xff51afd7ed558ccd
		return uint32(x ^ (x >> 33))
	case v.IsBool():
		if v.IsTrue() {
			return 1
		}
		return 2
	default:
		p := v.bits ^ (v.tag * 0x9e3779b97f4a7c15)
		p ^= p >> 33
		p *= 0xff51afd7ed558ccd
		return uint32(p ^ (p >> 33))
	}
}

func (t *Table) hashFindSlot(s *State, key Value) (int, bool) {
	if t.hashRef == arena.NullRef {
		return -1, false
	}
	h := arena.AsSmall[tableHash](s.arena, t.hashRef)
	if h.nodesRef == arena.NullRef {
		return -1, false
	}
	nodes := h.nodesSlice(s.arena)
	mask := h.mask
	idx := hashValue(s, key) & mask
	for {
		nd := &nodes[idx]
		if nd.state == nodeEmpty {
			return -1, false
		}
		if nd.state == nodeLive && s.RawEqual(nd.key, key) {
			return int(idx), true
		}
		idx = (idx + 1) & mask
	}
}

func (t *Table) hashFindStrSlot(s *State, k *LuaString) (int, bool) {
	if t.hashRef == arena.NullRef {
		return -1, false
	}
	h := arena.AsSmall[tableHash](s.arena, t.hashRef)
	if h.nodesRef == arena.NullRef {
		return -1, false
	}
	nodes := h.nodesSlice(s.arena)
	mask := h.mask
	idx := k.hash() & mask
	isShort := k.IsShort()
	hasRef := k.self != arena.NullRef
	var kv Value
	if hasRef {
		kv = StringValue(k)
	}
	for {
		nd := &nodes[idx]
		if nd.state == nodeEmpty {
			return -1, false
		}
		if nd.state == nodeLive {
			if isShort {
				if hasRef && nd.key == kv {
					return int(idx), true
				}
			} else if nd.key.IsString() {
				if hasRef && nd.key == kv {
					return int(idx), true
				}
				other := s.asString(nd.key)
				if !other.IsShort() && other.Len == k.Len && other.Str() == k.Str() {
					return int(idx), true
				}
			}
		}
		idx = (idx + 1) & mask
	}
}

func (t *Table) hashPutNoGrow(s *State, key, val Value) int {
	h := arena.AsSmall[tableHash](s.arena, t.hashRef)
	nodes := h.nodesSlice(s.arena)
	mask := h.mask
	idx := hashValue(s, key) & mask
	firstDead := -1
	for {
		nd := &nodes[idx]
		if nd.state == nodeEmpty {
			target := int(idx)
			if firstDead >= 0 {
				target = firstDead
				h.dictGen++
			} else {
				h.count++
			}
			nodes[target] = hashNode{key: key, val: val, state: nodeLive}
			h.live++
			if t.hdr.IsBlack() && (key.IsGCObject() || val.IsGCObject()) {
				s.collector.BarrierBack(t.self)
			}
			return target
		}
		if nd.state == nodeDead {
			if firstDead < 0 {
				firstDead = int(idx)
			}
		} else if s.RawEqual(nd.key, key) {
			nd.val = val
			if val.IsGCObject() && t.hdr.IsBlack() {
				s.collector.BarrierBack(t.self)
			}
			return int(idx)
		}
		idx = (idx + 1) & mask
	}
}

func (t *Table) hashSetChecked(s *State, key, val Value) (int, error) {
	if val.IsNil() {
		if slot, ok := t.hashFindSlot(s, key); ok {
			arena.AsSmall[tableHash](s.arena, t.hashRef).markDead(s.arena, slot)
			return slot, nil
		}
		return -1, nil
	}
	if slot, ok := t.hashFindSlot(s, key); ok {
		h := arena.AsSmall[tableHash](s.arena, t.hashRef)
		h.nodesSlice(s.arena)[slot].val = val
		if val.IsGCObject() && t.hdr.IsBlack() {
			s.collector.BarrierBack(t.self)
		}
		return slot, nil
	}
	if err := t.checkHashMem(s, key, val); err != nil {
		return -1, err
	}
	if t.hashRef == arena.NullRef {
		if !t.initHash(s, 4) {
			return -1, s.memError()
		}
	} else {
		h := arena.AsSmall[tableHash](s.arena, t.hashRef)
		if h.nodesRef == arena.NullRef {
			if !t.initHash(s, 4) {
				return -1, s.memError()
			}
		} else if int(h.count+1)*10 >= (int(h.mask)+1)*7 {
			if !t.growHash(s) {
				return -1, s.memError()
			}
		}
	}
	return t.hashPutNoGrow(s, key, val), nil
}

// demoteToDict converts a Shape-mode table into a dict-mode hash table.
func (t *Table) demoteToDict(s *State, key, val Value) bool {
	if t.shapeRef == arena.NullRef {
		return true
	}
	sh := arena.AsSmall[Shape](s.arena, t.shapeRef)
	keys := sh.Keys(s.arena)
	n := len(keys)
	if n < 4 {
		n = 4
	}
	if s.memLimit > 0 {
		hashBytes := arenaAllocSize(int64(unsafe.Sizeof(tableHash{}))) +
			arenaAllocSize(hashCapFor(int64(n))*int64(unsafe.Sizeof(hashNode{})))
		var oldSlotBytes int64
		if t.slotsRef != arena.NullRef {
			oldSlotBytes = int64(s.arena.SlotSize(t.slotsRef))
		}
		delta := hashBytes - oldSlotBytes
		if delta > 0 {
			if err := s.checkMemWithRoots(delta, TableValue(t), key, val); err != nil {
				s.memExceeded = true
				return false
			}
		}
	}
	if !t.initHash(s, n) {
		return false
	}
	slots := t.slotsSlice(s.arena)
	for i, kv := range keys {
		if i < len(slots) && !slots[i].IsNil() {
			t.hashPutNoGrow(s, kv, slots[i])
		}
	}
	t.shapeRef = arena.NullRef
	if t.slotsRef != arena.NullRef {
		s.arena.Free(t.slotsRef)
		t.slotsRef = arena.NullRef
	}
	t.slotsLen = 0
	t.slotsCap = 0
	t.inlineSlots[0] = NilValue()
	t.inlineSlots[1] = NilValue()
	return true
}

func (t *Table) appendSlot(s *State, val Value) bool {
	idx := int(t.slotsLen)
	if idx < int(t.slotsCap) {
		t.slotsLen = uint8(idx + 1)
		*t.slotPtr(s.arena, idx) = val
		return true
	}
	newCap := int(t.slotsCap) * 2
	if newCap < 4 {
		newCap = 4
	}
	if newCap > maxShapeProps {
		newCap = maxShapeProps
	}
	newRef, err := s.allocArrBuf(16, newCap)
	if err != nil {
		s.memExceeded = true
		return false
	}
	dst := arena.SliceFast[Value](s.arena, newRef, 0, newCap)
	if t.slotsLen > 0 {
		copy(dst, t.slotsSlice(s.arena))
	}
	fillNil(dst[idx+1:])
	dst[idx] = val
	if t.slotsRef != arena.NullRef {
		s.arena.Free(t.slotsRef)
	} else {
		t.inlineSlots[0] = NilValue()
		t.inlineSlots[1] = NilValue()
	}
	t.slotsRef = newRef
	t.slotsLen = uint8(idx + 1)
	t.slotsCap = uint8(newCap)
	return true
}

// RawGetStr looks up a LuaString key (fast path for GETFIELD/SELF/GETTABUP).
func (t *Table) RawGetStr(s *State, k *LuaString) Value {
	if OptimizeEnabled && t.shapeRef != arena.NullRef {
		if k.IsShort() {
			if idx := arena.AsSmall[Shape](s.arena, t.shapeRef).Lookup(s, k); idx >= 0 {
				return t.slotAt(s.arena, int(idx))
			}
		}
		return NilValue()
	}
	if slot, ok := t.hashFindStrSlot(s, k); ok {
		return arena.AsSmall[tableHash](s.arena, t.hashRef).nodesSlice(s.arena)[slot].val
	}
	return NilValue()
}

// RawSetStr stores a LuaString key -> val.
func (t *Table) RawSetStr(s *State, k *LuaString, val Value) {
	if err := s.ensureWriteStrKeyHash(t, k, val.IsNil()); err != nil {
		return
	}
	if t.isProto() {
		s.protoEpoch++
		if k == s.intern.tms[TMIndex] || k == s.intern.tms[TMNewIndex] {
			if val.IsTable() {
				s.asTable(val).markProto(s)
			}
		}
	}
	if t.tmFlags != 0 && k.IsShort() {
		t.tmFlags = 0
	}
	if OptimizeEnabled && t.shapeRef != arena.NullRef {
		if k.IsShort() {
			sh := arena.AsSmall[Shape](s.arena, t.shapeRef)
			if idx := sh.Lookup(s, k); idx >= 0 {
				*t.slotPtr(s.arena, int(idx)) = val
				s.tableBarrier(t, val)
				return
			}
			if val.IsNil() {
				return
			}
			next, serr := s.shapes.Transition(s, t.shapeRef, k, TableValue(t), val)
			if serr != nil {
				s.memExceeded = true
				return
			}
			if next != nil {
				if s.memLimit > 0 && t.slotsLen == t.slotsCap {
					if delta := t.slotsGrowthDelta(s); delta > 0 {
						prevPinned := s.pinnedShapeRef
						s.pinnedShapeRef = next.self
						err := s.checkMemWithRoots(delta, TableValue(t), StringValue(k), val)
						s.pinnedShapeRef = prevPinned
						if err != nil {
							s.memExceeded = true
							return
						}
					}
				}
				if !t.appendSlot(s, val) {
					return
				}
				t.shapeRef = next.self
				if t.hdr.IsBlack() {
					s.collector.BarrierBack(t.self)
				}
				return
			}
		}
		if val.IsNil() {
			return
		}
		if !t.demoteToDict(s, StringValue(k), val) {
			return
		}
	}
	if val.IsNil() {
		// Delete via hashFindStrSlot directly so transient non-Arena long strings
		// from State.LookupString never enter StringValue(k).
		if slot, ok := t.hashFindStrSlot(s, k); ok {
			h := arena.AsSmall[tableHash](s.arena, t.hashRef)
			h.markDead(s.arena, slot)
			t.hdr.Flags |= tblFlagHasDeadKeys
			s.hasDeadHashKeys = true
		}
		return
	}
	if _, err := t.hashSetChecked(s, StringValue(k), val); err != nil {
		s.memExceeded = true
		return
	}
}

// RawGetInt looks up integer key i (1-based dense array fast path).
func (t *Table) RawGetInt(s *State, i int64) Value {
	if i >= 1 {
		idx := uint64(i - 1)
		if idx < uint64(t.arrLen) {
			if OptimizeEnabled {
				switch t.arrKind {
				case arrInt:
					return IntValue(t.arrIntSlice(s.arena)[idx])
				case arrFloat:
					return FloatValue(t.arrFloatSlice(s.arena)[idx])
				case arrBool:
					return BoolValue(t.arrBoolSlice(s.arena)[idx])
				case arrGeneric:
					return t.arrValSlice(s.arena)[idx]
				}
			} else {
				return t.arrValSlice(s.arena)[idx]
			}
		}
	}
	if t.hashRef == arena.NullRef {
		return NilValue()
	}
	if slot, ok := t.hashFindSlot(s, IntValue(i)); ok {
		return arena.AsSmall[tableHash](s.arena, t.hashRef).nodesSlice(s.arena)[slot].val
	}
	return NilValue()
}

func (t *Table) promoteArrToGeneric(s *State, val Value) bool {
	if s.memLimit > 0 && t.arrKind != arrEmpty {
		capN := max(int(t.arrCap), int(t.arrLen))
		if delta := t.arrGrowthDelta(s, capN, 16); delta > 0 {
			if err := s.checkMemWithRoots(delta, TableValue(t), val, NilValue()); err != nil {
				s.memExceeded = true
				return false
			}
		}
	}
	if t.arrKind == arrEmpty {
		t.arrKind = arrGeneric
		return true
	}
	oldRef := t.arrRef
	oldKind := t.arrKind
	n := int(t.arrLen)
	capN := max(int(t.arrCap), n)
	newRef, err := s.allocArrBuf(16, capN)
	if err != nil {
		s.memExceeded = true
		return false
	}
	g := arena.SliceFast[Value](s.arena, newRef, 0, capN)
	switch oldKind {
	case arrInt:
		src := arena.SliceFast[int64](s.arena, oldRef, 0, n)
		for i, v := range src {
			g[i] = IntValue(v)
		}
	case arrFloat:
		src := arena.SliceFast[float64](s.arena, oldRef, 0, n)
		for i, v := range src {
			g[i] = FloatValue(v)
		}
	case arrBool:
		src := arena.SliceFast[bool](s.arena, oldRef, 0, n)
		for i, v := range src {
			g[i] = BoolValue(v)
		}
	}
	if capN > n {
		fillNil(g[n:])
	}
	if oldRef != arena.NullRef {
		s.arena.Free(oldRef)
	}
	t.arrRef = newRef
	t.arrCap = uint32(capN)
	t.arrKind = arrGeneric
	return true
}

func (t *Table) checkGenericArrGrowth(s *State, idx int64, val Value) bool {
	if s.memLimit > 0 && int(idx)+1 > int(t.arrCap) {
		nc := nextCap(int(t.arrCap), int(idx)+1)
		if delta := t.arrGrowthDelta(s, nc, 16); delta > 0 {
			if err := s.checkMemWithRoots(delta, TableValue(t), val, NilValue()); err != nil {
				s.memExceeded = true
				return false
			}
		}
	}
	return true
}

func nextCap(cur, need int) int {
	c := cur * 2
	if c < 8 {
		c = 8
	}
	if c < need {
		c = need
	}
	return c
}

func (t *Table) hashDeleteInt(s *State, k int64) {
	if t.hashRef == arena.NullRef {
		return
	}
	h := arena.AsSmall[tableHash](s.arena, t.hashRef)
	if h.live == 0 {
		return
	}
	if slot, ok := t.hashFindSlot(s, IntValue(k)); ok {
		h.markDead(s.arena, slot)
	}
}

func (t *Table) migrateHashIntRange(s *State, oldLen, newLen int, skipIdx int64) {
	if t.hashRef == arena.NullRef {
		return
	}
	h := arena.AsSmall[tableHash](s.arena, t.hashRef)
	if h.live == 0 {
		return
	}
	arr := t.arrValSlice(s.arena)
	for k := oldLen + 1; k <= newLen; k++ {
		if slot, ok := t.hashFindSlot(s, IntValue(int64(k))); ok {
			nd := &h.nodesSlice(s.arena)[slot]
			oldVal := nd.val
			h.markDead(s.arena, slot)
			if int64(k-1) != skipIdx {
				arr[k-1] = oldVal
			}
		}
	}
}

// RawSetInt sets integer key i -> val.
func (t *Table) RawSetInt(s *State, i int64, val Value) {
	if t.isProto() {
		s.protoEpoch++
	}
	if t.hashRef != arena.NullRef && (i < 1 || uint64(i-1) >= uint64(t.arrLen)) {
		h := arena.AsSmall[tableHash](s.arena, t.hashRef)
		if h.live > 0 {
			if slot, ok := t.hashFindSlot(s, IntValue(i)); ok {
				if val.IsNil() {
					h.markDead(s.arena, slot)
				} else {
					h.nodesSlice(s.arena)[slot].val = val
					s.tableBarrier(t, val)
				}
				return
			}
		}
	}
	if OptimizeEnabled {
		if i >= 1 {
			idx := i - 1
			switch t.arrKind {
			case arrEmpty:
				if !val.IsNil() && idx == 0 {
					c := max(int(t.arrHint), 8)
					hintCredit := int64(t.arrHint) * 8
					if val.IsInt() {
						if s.memLimit > 0 {
							if need := arenaAllocSize(int64(c)*8) - hintCredit; need > 0 {
								if err := s.checkMemWithRoots(need, TableValue(t), val, NilValue()); err != nil {
									s.memExceeded = true
									return
								}
							}
						}
						aref, err := s.allocArrBuf(8, c)
						if err != nil {
							s.memExceeded = true
							return
						}
						t.consumeArrHint(s)
						arena.SliceFast[int64](s.arena, aref, 0, c)[0] = val.Int()
						t.arrRef = aref
						t.arrLen = 1
						t.arrCap = uint32(c)
						t.arrKind = arrInt
						t.hashDeleteInt(s, 1)
						return
					}
					if val.IsFloat() {
						if s.memLimit > 0 {
							if need := arenaAllocSize(int64(c)*8) - hintCredit; need > 0 {
								if err := s.checkMemWithRoots(need, TableValue(t), val, NilValue()); err != nil {
									s.memExceeded = true
									return
								}
							}
						}
						aref, err := s.allocArrBuf(8, c)
						if err != nil {
							s.memExceeded = true
							return
						}
						t.consumeArrHint(s)
						arena.SliceFast[float64](s.arena, aref, 0, c)[0] = val.Float()
						t.arrRef = aref
						t.arrLen = 1
						t.arrCap = uint32(c)
						t.arrKind = arrFloat
						t.hashDeleteInt(s, 1)
						return
					}
					if val.IsBool() {
						if s.memLimit > 0 {
							if need := arenaAllocSize(int64(c)) - hintCredit; need > 0 {
								if err := s.checkMemWithRoots(need, TableValue(t), val, NilValue()); err != nil {
									s.memExceeded = true
									return
								}
							}
						}
						aref, err := s.allocArrBuf(1, c)
						if err != nil {
							s.memExceeded = true
							return
						}
						t.consumeArrHint(s)
						arena.SliceFast[bool](s.arena, aref, 0, c)[0] = val.IsTrue()
						t.arrRef = aref
						t.arrLen = 1
						t.arrCap = uint32(c)
						t.arrKind = arrBool
						t.hashDeleteInt(s, 1)
						return
					}
					if s.memLimit > 0 {
						if need := arenaAllocSize(int64(c)*16) - hintCredit; need > 0 {
							if err := s.checkMemWithRoots(need, TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
					}
					aref, err := s.allocArrBuf(16, c)
					if err != nil {
						s.memExceeded = true
						return
					}
					t.consumeArrHint(s)
					arr := arena.SliceFast[Value](s.arena, aref, 0, c)
					arr[0] = val
					t.arrRef = aref
					t.arrLen = 1
					t.arrCap = uint32(c)
					t.arrKind = arrGeneric
					s.tableBarrier(t, val)
					t.hashDeleteInt(s, 1)
					return
				}
				if !val.IsNil() && idx < max(int64(t.arrHint), 16) {
					c := max(int(t.arrHint), int(idx)+1)
					nc := nextCap(c/2, c)
					hintCredit := int64(t.arrHint) * 8
					if s.memLimit > 0 {
						if need := arenaAllocSize(int64(nc)*16) - hintCredit; need > 0 {
							if err := s.checkMemWithRoots(need, TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
					}
					aref, err := s.allocArrBuf(16, nc)
					if err != nil {
						s.memExceeded = true
						return
					}
					t.consumeArrHint(s)
					arr := arena.SliceFast[Value](s.arena, aref, 0, int(idx)+1)
					fillNil(arr)
					t.arrRef = aref
					t.arrLen = uint32(idx + 1)
					t.arrCap = uint32(nc)
					t.arrKind = arrGeneric
					t.migrateHashIntRange(s, 0, int(idx)+1, idx)
					arr[idx] = val
					s.tableBarrier(t, val)
					return
				}
			case arrInt:
				n := int64(t.arrLen)
				if idx < n {
					if val.IsInt() {
						t.arrIntSlice(s.arena)[idx] = val.Int()
						return
					}
					if !t.promoteArrToGeneric(s, val) {
						return
					}
					t.arrValSlice(s.arena)[idx] = val
					s.tableBarrier(t, val)
					return
				}
				if idx == n && val.IsInt() {
					if uint32(n) == t.arrCap {
						nc := nextCap(int(t.arrCap), int(n)+1)
						if s.memLimit > 0 {
							if delta := t.arrGrowthDelta(s, nc, 8); delta > 0 {
								if err := s.checkMemWithRoots(delta, TableValue(t), val, NilValue()); err != nil {
									s.memExceeded = true
									return
								}
							}
						}
						newRef, err := s.allocArrBuf(8, nc)
						if err != nil {
							s.memExceeded = true
							return
						}
						grown := arena.SliceFast[int64](s.arena, newRef, 0, nc)
						copy(grown, t.arrIntSlice(s.arena))
						grown[idx] = val.Int()
						if t.arrRef != arena.NullRef {
							s.arena.Free(t.arrRef)
						}
						t.arrRef = newRef
						t.arrLen = uint32(n + 1)
						t.arrCap = uint32(nc)
					} else {
						t.arrLen = uint32(n + 1)
						t.arrIntSlice(s.arena)[idx] = val.Int()
					}
					t.hashDeleteInt(s, i)
					return
				}
				if !val.IsNil() && idx < n*2 && idx < 1<<24 {
					if !t.promoteArrToGeneric(s, val) || !t.checkGenericArrGrowth(s, idx, val) {
						return
					}
					t.setArrGeneric(s, idx, val)
					return
				}
			case arrFloat:
				n := int64(t.arrLen)
				if idx < n {
					if val.IsFloat() {
						t.arrFloatSlice(s.arena)[idx] = val.Float()
						return
					}
					if !t.promoteArrToGeneric(s, val) {
						return
					}
					t.arrValSlice(s.arena)[idx] = val
					s.tableBarrier(t, val)
					return
				}
				if idx == n && val.IsFloat() {
					if uint32(n) == t.arrCap {
						nc := nextCap(int(t.arrCap), int(n)+1)
						if s.memLimit > 0 {
							if delta := t.arrGrowthDelta(s, nc, 8); delta > 0 {
								if err := s.checkMemWithRoots(delta, TableValue(t), val, NilValue()); err != nil {
									s.memExceeded = true
									return
								}
							}
						}
						newRef, err := s.allocArrBuf(8, nc)
						if err != nil {
							s.memExceeded = true
							return
						}
						grown := arena.SliceFast[float64](s.arena, newRef, 0, nc)
						copy(grown, t.arrFloatSlice(s.arena))
						grown[idx] = val.Float()
						if t.arrRef != arena.NullRef {
							s.arena.Free(t.arrRef)
						}
						t.arrRef = newRef
						t.arrLen = uint32(n + 1)
						t.arrCap = uint32(nc)
					} else {
						t.arrLen = uint32(n + 1)
						t.arrFloatSlice(s.arena)[idx] = val.Float()
					}
					t.hashDeleteInt(s, i)
					return
				}
				if !val.IsNil() && idx < n*2 && idx < 1<<24 {
					if !t.promoteArrToGeneric(s, val) || !t.checkGenericArrGrowth(s, idx, val) {
						return
					}
					t.setArrGeneric(s, idx, val)
					return
				}
			case arrBool:
				n := int64(t.arrLen)
				if idx < n {
					if val.IsBool() {
						t.arrBoolSlice(s.arena)[idx] = val.IsTrue()
						return
					}
					if !t.promoteArrToGeneric(s, val) {
						return
					}
					t.arrValSlice(s.arena)[idx] = val
					s.tableBarrier(t, val)
					return
				}
				if idx == n && val.IsBool() {
					if uint32(n) == t.arrCap {
						nc := nextCap(int(t.arrCap), int(n)+1)
						if s.memLimit > 0 {
							if delta := t.arrGrowthDelta(s, nc, 1); delta > 0 {
								if err := s.checkMemWithRoots(delta, TableValue(t), val, NilValue()); err != nil {
									s.memExceeded = true
									return
								}
							}
						}
						newRef, err := s.allocArrBuf(1, nc)
						if err != nil {
							s.memExceeded = true
							return
						}
						grown := arena.SliceFast[bool](s.arena, newRef, 0, nc)
						copy(grown, t.arrBoolSlice(s.arena))
						grown[idx] = val.IsTrue()
						if t.arrRef != arena.NullRef {
							s.arena.Free(t.arrRef)
						}
						t.arrRef = newRef
						t.arrLen = uint32(n + 1)
						t.arrCap = uint32(nc)
					} else {
						t.arrLen = uint32(n + 1)
						t.arrBoolSlice(s.arena)[idx] = val.IsTrue()
					}
					t.hashDeleteInt(s, i)
					return
				}
				if !val.IsNil() && idx < n*2 && idx < 1<<24 {
					if !t.promoteArrToGeneric(s, val) || !t.checkGenericArrGrowth(s, idx, val) {
						return
					}
					t.setArrGeneric(s, idx, val)
					return
				}
			case arrGeneric:
				n := int64(t.arrLen)
				if idx < n {
					t.arrValSlice(s.arena)[idx] = val
					s.tableBarrier(t, val)
					return
				}
				if !val.IsNil() && (idx == n || (idx < max(n*2, 16) && idx < 1<<24)) {
					if !t.checkGenericArrGrowth(s, idx, val) {
						return
					}
					t.setArrGeneric(s, idx, val)
					return
				}
			}
		}
		if t.shapeRef != arena.NullRef {
			if val.IsNil() {
				return
			}
			if !t.demoteToDict(s, IntValue(i), val) {
				return
			}
		}
		if _, err := t.hashSetChecked(s, IntValue(i), val); err != nil {
			s.memExceeded = true
			return
		}
		return
	}

	// Faithful mode (Gate 1).
	if i >= 1 {
		idx := i - 1
		n := int64(t.arrLen)
		if idx < n {
			t.arrValSlice(s.arena)[idx] = val
			s.tableBarrier(t, val)
			return
		}
		if !val.IsNil() && (idx == n || (idx < max(n*2, 16) && idx < 1<<24)) {
			if !t.checkGenericArrGrowth(s, idx, val) {
				return
			}
			t.setArrGeneric(s, idx, val)
			return
		}
	}
	if _, err := t.hashSetChecked(s, IntValue(i), val); err != nil {
		s.memExceeded = true
		return
	}
}

func (t *Table) setArrGeneric(s *State, idx int64, val Value) {
	oldLen := int(t.arrLen)
	newLen := int(idx) + 1
	if newLen > int(t.arrCap) {
		nc := nextCap(int(t.arrCap), newLen)
		newRef, err := s.allocArrBuf(16, nc)
		if err != nil {
			s.memExceeded = true
			return
		}
		grown := arena.SliceFast[Value](s.arena, newRef, 0, nc)
		if oldLen > 0 {
			copy(grown, t.arrValSlice(s.arena))
		}
		if t.arrRef != arena.NullRef {
			s.arena.Free(t.arrRef)
		}
		t.arrRef = newRef
		t.arrCap = uint32(nc)
		t.arrKind = arrGeneric
	}
	t.arrLen = uint32(newLen)
	arr := t.arrValSlice(s.arena)
	if newLen > oldLen+1 {
		fillNil(arr[oldLen : newLen-1])
	}
	if t.hashRef != arena.NullRef {
		t.migrateHashIntRange(s, oldLen, newLen, idx)
	}
	arr[idx] = val
	s.tableBarrier(t, val)
}

// RawGet looks up any Lua Value key.
func (t *Table) RawGet(s *State, key Value) Value {
	switch {
	case key.IsInt():
		return t.RawGetInt(s, key.Int())
	case key.IsString():
		return t.RawGetStr(s, s.asString(key))
	case key.IsFloat():
		f := key.Float()
		if math.IsNaN(f) {
			return NilValue()
		}
		if i, ok := FloatToIntExact(f); ok {
			return t.RawGetInt(s, i)
		}
		if slot, ok := t.hashFindSlot(s, key); ok {
			return arena.AsSmall[tableHash](s.arena, t.hashRef).nodesSlice(s.arena)[slot].val
		}
		return NilValue()
	case key.IsNil():
		return NilValue()
	default:
		if slot, ok := t.hashFindSlot(s, key); ok {
			return arena.AsSmall[tableHash](s.arena, t.hashRef).nodesSlice(s.arena)[slot].val
		}
		return NilValue()
	}
}

// RawSet sets any Lua Value key -> val (returns error on nil/NaN key or memory limit exceeded).
func (t *Table) RawSet(s *State, key, val Value) error {
	switch {
	case key.IsInt():
		t.RawSetInt(s, key.Int(), val)
		return s.ConsumeMemErr()
	case key.IsString():
		ls := s.asString(key)
		if err := s.ensureWriteStrKeyHash(t, ls, val.IsNil()); err != nil {
			return err
		}
		t.RawSetStr(s, ls, val)
		return s.ConsumeMemErr()
	case key.IsFloat():
		f := key.Float()
		if math.IsNaN(f) {
			return fmt.Errorf("table index is NaN")
		}
		if i, ok := FloatToIntExact(f); ok {
			t.RawSetInt(s, i, val)
			return s.ConsumeMemErr()
		}
		if f == 0 {
			t.RawSetInt(s, 0, val)
			return s.ConsumeMemErr()
		}
		if t.isProto() {
			s.protoEpoch++
		}
		if OptimizeEnabled && t.shapeRef != arena.NullRef {
			if val.IsNil() {
				return nil
			}
			if !t.demoteToDict(s, key, val) {
				return s.ConsumeMemErr()
			}
		}
		if _, err := t.hashSetChecked(s, key, val); err != nil {
			return err
		}
		return nil
	case key.IsNil():
		return fmt.Errorf("table index is nil")
	default:
		if t.isProto() {
			s.protoEpoch++
		}
		if OptimizeEnabled && t.shapeRef != arena.NullRef {
			if val.IsNil() {
				return nil
			}
			if !t.demoteToDict(s, key, val) {
				return s.ConsumeMemErr()
			}
		}
		slot, err := t.hashSetChecked(s, key, val)
		if err != nil {
			return err
		}
		if val.IsNil() && slot >= 0 && key.IsGCObject() {
			t.hdr.Flags |= tblFlagHasDeadKeys
			s.hasDeadHashKeys = true
		}
		return nil
	}
}

// Len computes Lua's raw #t (border search across dense array + hash part).
func (t *Table) Len(s *State) int64 {
	if OptimizeEnabled {
		switch t.arrKind {
		case arrInt, arrFloat, arrBool:
			n := int64(t.arrLen)
			if t.hashRef == arena.NullRef || t.RawGetInt(s, n+1).IsNil() {
				return n
			}
			return t.hashBorder(s, n)
		}
	}
	n := int(t.arrLen)
	if n > 0 {
		arr := t.arrValSlice(s.arena)
		if arr[n-1].IsNil() {
			lo, hi := 0, n
			for hi-lo > 1 {
				mid := (lo + hi) / 2
				if arr[mid-1].IsNil() {
					hi = mid
				} else {
					lo = mid
				}
			}
			return int64(lo)
		}
	}
	if t.hashRef == arena.NullRef || t.RawGetInt(s, int64(n)+1).IsNil() {
		return int64(n)
	}
	return t.hashBorder(s, int64(n))
}

func (t *Table) hashBorder(s *State, j int64) int64 {
	i := j
	j++
	for !t.RawGetInt(s, j).IsNil() {
		i = j
		if j > math.MaxInt64/2 {
			for j = 1; !t.RawGetInt(s, j).IsNil(); j++ {
			}
			return j - 1
		}
		j *= 2
	}
	for j-i > 1 {
		m := i + (j-i)/2
		if t.RawGetInt(s, m).IsNil() {
			j = m
		} else {
			i = m
		}
	}
	return i
}

// hashFindSlotForNext finds key in t.hash.nodes including dead/tombstoned keys (needed by Lua's next()).
func (t *Table) hashFindSlotForNext(s *State, key Value) (int, bool) {
	if t.hashRef == arena.NullRef {
		return -1, false
	}
	h := arena.AsSmall[tableHash](s.arena, t.hashRef)
	if h.nodesRef == arena.NullRef {
		return -1, false
	}
	if key.IsFloat() {
		if _, ok := FloatToIntExact(key.Float()); ok {
			return -1, false
		}
	}
	nodes := h.nodesSlice(s.arena)
	mask := h.mask
	idx := hashValue(s, key) & mask
	for {
		nd := &nodes[idx]
		if nd.state == nodeEmpty {
			return -1, false
		}
		if !nd.key.IsDeadKey() && s.RawEqual(nd.key, key) {
			return int(idx), true
		}
		idx = (idx + 1) & mask
	}
}

// Next implements Lua's next(t, key) across array, shape slots, and hash nodes.
func (t *Table) Next(s *State, key Value) (Value, Value, error) {
	arrLen := int(t.arrLen)
	startPhase := 0 // 0=array, 1=shape, 2=hash
	startIdx := 0

	if !key.IsNil() {
		found := false
		if key.IsInt() {
			i := key.Int()
			if i >= 1 && i <= int64(arrLen) {
				startPhase = 0
				startIdx = int(i)
				found = true
			}
		}
		if !found && OptimizeEnabled && t.shapeRef != arena.NullRef && key.IsString() {
			ls := s.asString(key)
			if ls.IsShort() {
				if slot := arena.AsSmall[Shape](s.arena, t.shapeRef).Lookup(s, ls); slot >= 0 {
					startPhase = 1
					startIdx = int(slot) + 1
					found = true
				}
			}
		}
		if !found {
			if slot, ok := t.hashFindSlotForNext(s, key); ok {
				startPhase = 2
				startIdx = slot + 1
				found = true
			}
		}
		if !found {
			return NilValue(), NilValue(), fmt.Errorf("invalid key to 'next'")
		}
	}

	if startPhase == 0 {
		for idx := startIdx; idx < arrLen; idx++ {
			v := t.arrAt(s.arena, idx)
			if !v.IsNil() {
				return IntValue(int64(idx + 1)), v, nil
			}
		}
		startPhase = 1
		startIdx = 0
	}
	if startPhase == 1 {
		if OptimizeEnabled && t.shapeRef != arena.NullRef {
			sh := arena.AsSmall[Shape](s.arena, t.shapeRef)
			if sh.length > 0 {
				keys := sh.Keys(s.arena)
				slots := t.slotsSlice(s.arena)
				for idx := startIdx; idx < len(keys); idx++ {
					if idx < len(slots) && !slots[idx].IsNil() {
						return keys[idx], slots[idx], nil
					}
				}
			}
		}
		startPhase = 2
		startIdx = 0
	}
	if t.hashRef != arena.NullRef {
		h := arena.AsSmall[tableHash](s.arena, t.hashRef)
		nodes := h.nodesSlice(s.arena)
		for idx := startIdx; idx < len(nodes); idx++ {
			nd := &nodes[idx]
			if nd.state == nodeLive && !nd.val.IsNil() {
				return nd.key, nd.val, nil
			}
		}
	}
	return NilValue(), NilValue(), nil
}

func (t *Table) arrAt(a *arena.Arena, idx int) Value {
	if OptimizeEnabled {
		switch t.arrKind {
		case arrInt:
			return IntValue(t.arrIntSlice(a)[idx])
		case arrFloat:
			return FloatValue(t.arrFloatSlice(a)[idx])
		case arrBool:
			return BoolValue(t.arrBoolSlice(a)[idx])
		case arrGeneric:
			return t.arrValSlice(a)[idx]
		default:
			return NilValue()
		}
	}
	return t.arrValSlice(a)[idx]
}
