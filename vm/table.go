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
	arrInt             // homogeneous 1..N dense []int64 (noscan!)
	arrFloat           // homogeneous 1..N dense []float64 (noscan!)
	arrBool            // homogeneous 1..N dense []bool (noscan, 1 byte/elem!)
	arrGeneric         // general 1..N []Value (may contain nil holes)
)

const (
	tblFlagIsProto     uint8 = 1 << 0
	tblFlagGCWeakCand  uint8 = 1 << 1
	tblFlagTrackedHash uint8 = 1 << 2
	tblFlagHasDeadKeys uint8 = 1 << 3
)

// hashNode is a slot in the open-addressing hash table.
type hashNode struct {
	key  Value
	val  Value
	used bool
	dead bool // key deleted while table existed (preserves probe chains)
}

// tableHash holds the lazily-allocated hash/dict part of a Lua Table.
// Pure record tables (Shape/slots) and pure dense arrays never allocate a tableHash.
type tableHash struct {
	nodes   []hashNode
	dictGen uint64 // bumped on rehash / structure change for _ENV IC (uint64 prevents 32-bit wraparound)
	mask    uint32
	count   int32 // active + dead slots (for load factor)
	live    int32 // active non-nil entries
}

func (h *tableHash) markDead(slot int) {
	nd := &h.nodes[slot]
	nd.val = NilValue()
	nd.dead = true
	h.live--
	h.dictGen++
}

// Table is a Lua 5.5 table (bridged into State.arena's Collector via tableStub until Step 5d).
type Table struct {
	shape       *Shape
	meta        *Table
	hash        *tableHash
	arrPtr      unsafe.Pointer // backing array for arrInt/arrFloat/arrBool/arrGeneric (GC-traced)
	slots       []Value
	inlineSlots [2]Value // co-allocates up to 2 record fields inline (1 alloc instead of 2)

	memBytes int64
	self     arena.ObjRef // Arena tableStub ObjRef in State.arena
	arrLen   uint32       // active length of backing array pointed to by arrPtr
	arrCap   uint32       // capacity of backing array pointed to by arrPtr
	arrHint  int32        // preallocated capacity hint from table.create / NEWTABLE
	arrKind  arrKind      // discriminator for arrPtr backing slice type
	tmFlags  uint8        // negative metamethod cache (bits 0..5 for TMIndex..TMEq)
	flags    uint8        // tblFlag* bitfield
}

func (t *Table) isProto() bool { return t.flags&tblFlagIsProto != 0 }

func (t *Table) arrIntSlice(_ *arena.Arena) []int64     { return unsafe.Slice((*int64)(t.arrPtr), t.arrLen) }
func (t *Table) arrFloatSlice(_ *arena.Arena) []float64 { return unsafe.Slice((*float64)(t.arrPtr), t.arrLen) }
func (t *Table) arrBoolSlice(_ *arena.Arena) []bool     { return unsafe.Slice((*bool)(t.arrPtr), t.arrLen) }
func (t *Table) arrValSlice(_ *arena.Arena) []Value     { return unsafe.Slice((*Value)(t.arrPtr), t.arrLen) }

func (t *Table) setArrInt(vs []int64) {
	t.arrPtr = unsafe.Pointer(unsafe.SliceData(vs))
	t.arrLen = uint32(len(vs))
	t.arrCap = uint32(cap(vs))
	t.arrKind = arrInt
}

func (t *Table) setArrFloat(vs []float64) {
	t.arrPtr = unsafe.Pointer(unsafe.SliceData(vs))
	t.arrLen = uint32(len(vs))
	t.arrCap = uint32(cap(vs))
	t.arrKind = arrFloat
}

func (t *Table) setArrBool(vs []bool) {
	t.arrPtr = unsafe.Pointer(unsafe.SliceData(vs))
	t.arrLen = uint32(len(vs))
	t.arrCap = uint32(cap(vs))
	t.arrKind = arrBool
}

func (t *Table) setArrVal(vs []Value) {
	t.arrPtr = unsafe.Pointer(unsafe.SliceData(vs))
	t.arrLen = uint32(len(vs))
	t.arrCap = uint32(cap(vs))
	t.arrKind = arrGeneric
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

// tablePreallocBytes returns the exact backing-store bytes allocated by NewTable(nseq, nrec).
func tablePreallocBytes(nseq, nrec int64) int64 {
	const structBytes = int64(96)
	var arrBytes, recBytes int64
	if OptimizeEnabled {
		if nrec > int64(len(Table{}.inlineSlots)) && nrec <= maxShapeProps {
			recBytes = nrec * 16
		} else if nrec > maxShapeProps {
			recBytes = hashCapFor(nrec) * 40
		}
	} else {
		if nseq > 0 {
			arrBytes = nseq * 16
		}
		if nrec > 0 {
			recBytes = hashCapFor(nrec) * 40
		}
	}
	return structBytes + arrBytes + recBytes
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
		checkBytes += int64(nseq) * 16
	}
	if err := s.checkMem(checkBytes); err != nil {
		return nil, err
	}
	t := s.NewTable(nseq, nrec)
	return t, nil
}

// NewTable allocates a Lua table with sequence/record capacity hints.
func (s *State) NewTable(nseq, nrec int) *Table {
	t := &Table{inlineSlots: [2]Value{NilValue(), NilValue()}}
	s.allocTableStub(t)
	// Subtract 16B already accounted in State.arena by tableStub so arena+extra == tablePreallocBytes.
	s.addTableMem(t, tablePreallocBytes(int64(nseq), int64(nrec))-16)
	if OptimizeEnabled {
		t.shape = s.shapes.root
		if nseq > 0 {
			t.arrHint = int32(nseq)
			s.addTableMem(t, int64(nseq)*8)
		}
		if nrec <= len(t.inlineSlots) {
			t.slots = t.inlineSlots[:0]
		} else if nrec <= maxShapeProps {
			t.slots = make([]Value, 0, nrec)
		} else {
			// Large record table (e.g. _ENV or big dict) starts directly in dict mode.
			t.shape = nil
			t.initHash(s, nrec)
		}
		return t
	}
	// Faithful mode (Gate 1): direct []Value + hash table.
	if nseq > 0 {
		arr := make([]Value, nseq)
		fillNil(arr)
		t.setArrVal(arr)
	}
	if nrec > 0 {
		t.initHash(s, nrec)
	}
	return t
}

func (t *Table) consumeArrHint(s *State) {
	if t.arrHint > 0 {
		if s != nil {
			s.subTableMem(t, int64(t.arrHint)*8)
		}
		t.arrHint = 0
	}
}

// NewDictTable allocates a table directly in dict mode (ideal for _ENV / globals).
func (s *State) NewDictTable(nrec int) *Table {
	t := &Table{}
	s.allocTableStub(t)
	if nrec < 16 {
		nrec = 16
	}
	t.initHash(s, nrec)
	s.addTableMem(t, 80+int64(len(t.hash.nodes))*40)
	return t
}

func fillNil(vs []Value) {
	n := NilValue()
	for i := range vs {
		vs[i] = n
	}
}

func (t *Table) Metatable(_ *State) *Table { return t.meta }

// SetMetatable sets t's metatable, updating Shape (in opt mode), tmFlags, proto flags, and Tier 1 GC.
// When shape allocation fails under a memory limit, t's metatable and shape remain unchanged so
// existing inline caches stay consistent with t.meta.
func (s *State) SetMetatable(t, mt *Table) {
	var newShape *Shape
	if OptimizeEnabled && t.shape != nil {
		var ok bool
		newShape, ok = s.shapes.WithMeta(s, t.shape, mt, TableValue(t))
		if !ok {
			s.memExceeded = true
			return
		}
	}
	if mt != nil {
		mt.markProto(s)
		// If mt.__index is a table, mark it as proto too so writes to it bump protoEpoch.
		if idx := mt.RawGetStr(s, s.intern.tms[TMIndex]); idx.IsTable() {
			s.asTable(idx).markProto(s)
		}
		if nidx := mt.RawGetStr(s, s.intern.tms[TMNewIndex]); nidx.IsTable() {
			s.asTable(nidx).markProto(s)
		}
		s.registerFinalizable(t.self, mt)
		s.registerWeakCandidate(t, mt)
	}
	t.meta = mt
	t.tmFlags = 0
	if mt != nil {
		s.barrierBackTable(t)
	}
	if OptimizeEnabled {
		if newShape != nil {
			t.shape = newShape
		}
		if t.isProto() {
			s.protoEpoch++
		}
	}
}

func (t *Table) markProto(s *State) {
	if t.flags&tblFlagIsProto == 0 {
		t.flags |= tblFlagIsProto
		s.protoEpoch++
	}
}

func (t *Table) initHash(s *State, n int) {
	sz := 4
	for sz < n*2 {
		sz <<= 1
	}
	h := t.hash
	if h == nil {
		h = &tableHash{}
		t.hash = h
	}
	h.nodes = make([]hashNode, sz)
	h.mask = uint32(sz - 1)
	h.dictGen++
	if s != nil && t.self != arena.NullRef {
		s.trackHashedTable(t.self)
	}
}

func (t *Table) checkHashMem(s *State, key, val Value) error {
	if s == nil || s.memLimit <= 0 {
		return nil
	}
	if t.hash == nil || len(t.hash.nodes) == 0 {
		return s.checkMemWithRoots(hashCapFor(4)*40, TableValue(t), key, val)
	}
	curLen := len(t.hash.nodes)
	curCnt := int(t.hash.count)
	if (curCnt+1)*10 >= curLen*7 {
		growSz := max(curLen*2, 4)
		if int(t.hash.live)*4 < curLen && curLen >= 8 {
			growSz = curLen
		}
		if growSz > curLen {
			return s.checkMemWithRoots(int64(growSz-curLen)*40, TableValue(t), key, val)
		}
	}
	return nil
}

func (t *Table) growHash(s *State) {
	h := t.hash
	old := h.nodes
	newSz := len(old) * 2
	if newSz < 4 {
		newSz = 4
	}
	// If mostly dead entries, keep same size to reclaim tombstones.
	if int(h.live)*4 < len(old) && len(old) >= 8 {
		newSz = len(old)
	}
	h.nodes = make([]hashNode, newSz)
	h.mask = uint32(newSz - 1)
	h.count = 0
	h.live = 0
	h.dictGen++
	if newSz > len(old) && s != nil {
		s.addTableMem(t, int64(newSz-len(old))*40)
	}
	for i := range old {
		nd := &old[i]
		if nd.used && !nd.dead && !nd.val.IsNil() {
			t.hashPutNoGrow(s, nd.key, nd.val)
		}
	}
	clear(old)
}

func hashValue(s *State, v Value) uint32 {
	switch {
	case v.IsInt():
		x := uint64(v.Int())
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
	h := t.hash
	if h == nil || len(h.nodes) == 0 {
		return -1, false
	}
	mask := h.mask
	idx := hashValue(s, key) & mask
	for {
		nd := &h.nodes[idx]
		if !nd.used {
			return -1, false
		}
		if !nd.dead && s.RawEqual(nd.key, key) {
			return int(idx), true
		}
		idx = (idx + 1) & mask
	}
}

func (t *Table) hashFindStrSlot(s *State, k *LuaString) (int, bool) {
	h := t.hash
	if h == nil || len(h.nodes) == 0 {
		return -1, false
	}
	mask := h.mask
	idx := k.hash() & mask
	hasRef := k.self != arena.NullRef
	var kv Value
	if hasRef {
		kv = StringValue(k)
	}
	for {
		nd := &h.nodes[idx]
		if !nd.used {
			return -1, false
		}
		if !nd.dead {
			if hasRef && nd.key == kv {
				return int(idx), true
			}
			if !k.IsShort() && nd.key.IsString() {
				ok := s.asString(nd.key)
				if ok.Len == k.Len && ok.Str() == k.Str() {
					return int(idx), true
				}
			}
		}
		idx = (idx + 1) & mask
	}
}

func (t *Table) hashPutNoGrow(s *State, key, val Value) int {
	h := t.hash
	mask := h.mask
	idx := hashValue(s, key) & mask
	firstDead := -1
	for {
		nd := &h.nodes[idx]
		if !nd.used {
			target := int(idx)
			if firstDead >= 0 {
				target = firstDead
				h.dictGen++
			} else {
				h.count++
			}
			h.nodes[target] = hashNode{key: key, val: val, used: true}
			h.live++
			return target
		}
		if nd.dead {
			if firstDead < 0 {
				firstDead = int(idx)
			}
		} else if s.RawEqual(nd.key, key) {
			nd.val = val
			return int(idx)
		}
		idx = (idx + 1) & mask
	}
}

// hashSetChecked updates an existing hash entry in place (or deletes on nil) without
// triggering growth or memory checks, and only checks memory before inserting a new key.
func (t *Table) hashSetChecked(s *State, key, val Value) (int, error) {
	if val.IsNil() {
		if slot, ok := t.hashFindSlot(s, key); ok {
			t.hash.markDead(slot)
			return slot, nil
		}
		return -1, nil
	}
	if slot, ok := t.hashFindSlot(s, key); ok {
		t.hash.nodes[slot].val = val
		if s != nil {
			s.barrierTableVal(t, val)
		}
		return slot, nil
	}
	if err := t.checkHashMem(s, key, val); err != nil {
		return -1, err
	}
	if t.hash == nil || len(t.hash.nodes) == 0 {
		t.initHash(s, 4)
		if s != nil {
			s.addTableMem(t, int64(len(t.hash.nodes))*40)
		}
	} else if int(t.hash.count+1)*10 >= len(t.hash.nodes)*7 {
		t.growHash(s)
	}
	slot := t.hashPutNoGrow(s, key, val)
	if s != nil && (key.IsGCObject() || val.IsGCObject()) {
		s.barrierBackTable(t)
	}
	return slot, nil
}

// demoteToDict converts a Shape-mode table into a dict-mode hash table.
// Returns false (setting s.memExceeded) if allocating the hash backing store exceeds s.memLimit.
func (t *Table) demoteToDict(s *State, key, val Value) bool {
	if t.shape == nil {
		return true
	}
	keys := t.shape.Keys()
	n := len(keys)
	if n < 4 {
		n = 4
	}
	hashBytes := hashCapFor(int64(n)) * 40
	var oldSlotBytes int64
	if c := cap(t.slots); c > len(t.inlineSlots) {
		oldSlotBytes = int64(c) * 16
	}
	delta := hashBytes - oldSlotBytes
	if s != nil && s.memLimit > 0 && delta > 0 {
		if err := s.checkMemWithRoots(delta, TableValue(t), key, val); err != nil {
			s.memExceeded = true
			return false
		}
	}
	t.initHash(s, n)
	if s != nil && delta > 0 {
		s.addTableMem(t, delta)
	}
	for i, k := range keys {
		if i < len(t.slots) && !t.slots[i].IsNil() {
			t.hashPutNoGrow(s, StringValue(k), t.slots[i])
		}
	}
	t.shape = nil
	if len(t.slots) > 0 {
		clear(t.slots)
	}
	t.slots = nil
	t.inlineSlots = [2]Value{NilValue(), NilValue()}
	return true
}

// appendSlot appends val to t.slots and clears the abandoned backing slice whenever
// t.slots relocates (including moves off t.inlineSlots or shared stdlib slabs) so
// obsolete entries are never retained on the Go heap.
func (t *Table) appendSlot(val Value) {
	oldSlots := t.slots
	oldCap := cap(oldSlots)
	t.slots = append(t.slots, val)
	if cap(t.slots) > oldCap {
		if len(oldSlots) > 0 {
			clear(oldSlots)
		}
		if oldCap <= len(t.inlineSlots) {
			t.inlineSlots = [2]Value{NilValue(), NilValue()}
		}
	}
}

// RawGetStr looks up a LuaString key (fast path for GETFIELD/SELF/GETTABUP).
func (t *Table) RawGetStr(s *State, k *LuaString) Value {
	if OptimizeEnabled && t.shape != nil {
		if k.IsShort() {
			if idx := t.shape.Lookup(k); idx >= 0 {
				return t.slots[idx]
			}
			return NilValue()
		}
		return NilValue()
	}
	if slot, ok := t.hashFindStrSlot(s, k); ok {
		return t.hash.nodes[slot].val
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
	if OptimizeEnabled && t.shape != nil {
		if k.IsShort() {
			if idx := t.shape.Lookup(k); idx >= 0 {
				// Assigning nil keeps slot & shape intact (next/pairs stability!).
				t.slots[idx] = val
				s.barrierTableVal(t, val)
				return
			}
			if val.IsNil() {
				return
			}
			next, serr := s.shapes.Transition(s, t.shape, k, TableValue(t), val)
			if serr != nil {
				s.memExceeded = true
				return
			}
			if next != nil {
				if s.memLimit > 0 && len(t.slots) == cap(t.slots) {
					prevPinned := s.pinnedShape
					s.pinnedShape = next
					err := s.checkMemWithRoots(int64(max(cap(t.slots), 4))*16, TableValue(t), StringValue(k), val)
					s.pinnedShape = prevPinned
					if err != nil {
						s.memExceeded = true
						return
					}
				}
				t.shape = next
				oldCap := cap(t.slots)
				t.appendSlot(val)
				if cap(t.slots) > oldCap {
					s.addTableMem(t, int64(cap(t.slots)-oldCap)*16)
				}
				s.barrierBackTable(t)
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
			t.hash.markDead(slot)
			if s != nil {
				t.flags |= tblFlagHasDeadKeys
				s.hasDeadHashKeys = true
			}
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
					return IntValue(t.arrIntSlice(nil)[idx])
				case arrFloat:
					return FloatValue(t.arrFloatSlice(nil)[idx])
				case arrBool:
					return BoolValue(t.arrBoolSlice(nil)[idx])
				case arrGeneric:
					return t.arrValSlice(nil)[idx]
				}
			} else {
				return t.arrValSlice(nil)[idx]
			}
		}
	}
	if t.hash == nil {
		return NilValue()
	}
	if slot, ok := t.hashFindSlot(s, IntValue(i)); ok {
		return t.hash.nodes[slot].val
	}
	return NilValue()
}

func (t *Table) promoteArrToGeneric(s *State, val Value) bool {
	var oldElem int64
	switch t.arrKind {
	case arrInt, arrFloat:
		oldElem = 8
	case arrBool:
		oldElem = 1
	}
	var delta int64
	if oldElem > 0 && t.arrCap > 0 {
		delta = int64(t.arrCap) * (16 - oldElem)
	}
	if s != nil && s.memLimit > 0 && delta > 0 {
		if err := s.checkMemWithRoots(delta, TableValue(t), val, NilValue()); err != nil {
			s.memExceeded = true
			return false
		}
	}
	switch t.arrKind {
	case arrEmpty:
		t.arrKind = arrGeneric
	case arrInt:
		src := t.arrIntSlice(nil)
		g := make([]Value, len(src), t.arrCap)
		for i, v := range src {
			g[i] = IntValue(v)
		}
		t.setArrVal(g)
	case arrFloat:
		src := t.arrFloatSlice(nil)
		g := make([]Value, len(src), t.arrCap)
		for i, v := range src {
			g[i] = FloatValue(v)
		}
		t.setArrVal(g)
	case arrBool:
		src := t.arrBoolSlice(nil)
		g := make([]Value, len(src), t.arrCap)
		for i, v := range src {
			g[i] = BoolValue(v)
		}
		t.setArrVal(g)
	}
	if s != nil && delta > 0 {
		s.addTableMem(t, delta)
	}
	return true
}

func (t *Table) checkGenericArrGrowth(s *State, idx int64, val Value) bool {
	if s != nil && s.memLimit > 0 && int(idx)+1 > int(t.arrCap) {
		nc := nextCap(int(t.arrCap), int(idx)+1)
		if err := s.checkMemWithRoots(int64(nc-int(t.arrCap))*16, TableValue(t), val, NilValue()); err != nil {
			s.memExceeded = true
			return false
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
	if t.hash == nil || t.hash.live == 0 {
		return
	}
	if slot, ok := t.hashFindSlot(s, IntValue(k)); ok {
		h := t.hash
		h.nodes[slot].val = NilValue()
		h.nodes[slot].dead = true
		h.live--
		h.dictGen++
	}
}

// migrateHashIntRange moves any active integer keys in (oldLen, newLen] from t.hash into
// the generic array (deleting them from t.hash so stale hash entries can never resurrect).
// Key skipIdx+1 is deleted without writing into arr because the caller is overwriting it.
func (t *Table) migrateHashIntRange(s *State, oldLen, newLen int, skipIdx int64) {
	if t.hash == nil || t.hash.live == 0 {
		return
	}
	arr := t.arrValSlice(nil)
	for k := oldLen + 1; k <= newLen; k++ {
		if slot, ok := t.hashFindSlot(s, IntValue(int64(k))); ok {
			h := t.hash
			oldVal := h.nodes[slot].val
			h.nodes[slot].val = NilValue()
			h.nodes[slot].dead = true
			h.live--
			h.dictGen++
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
	// If key already exists in the hash part (outside [1..arrLen]), update it in place
	// rather than migrating it into the array mid-traversal (matching ltable.c).
	if t.hash != nil && t.hash.live > 0 && (i < 1 || uint64(i-1) >= uint64(t.arrLen)) {
		if slot, ok := t.hashFindSlot(s, IntValue(i)); ok {
			h := t.hash
			if val.IsNil() {
				h.nodes[slot].val = NilValue()
				h.nodes[slot].dead = true
				h.live--
				h.dictGen++
			} else {
				h.nodes[slot].val = val
				s.barrierTableVal(t, val)
			}
			return
		}
	}
	if OptimizeEnabled {
		if i >= 1 {
			idx := i - 1
			switch t.arrKind {
			case arrEmpty:
				if !val.IsNil() && idx == 0 {
					c := max(int(t.arrHint), 8)
					if val.IsInt() {
						if s.memLimit > 0 {
							if err := s.checkMemWithRoots(int64(c)*8-int64(t.arrHint)*8, TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
						t.consumeArrHint(s)
						arr := make([]int64, 1, c)
						arr[0] = val.Int()
						t.setArrInt(arr)
						s.addTableMem(t, int64(c)*8)
						t.hashDeleteInt(s, 1)
						return
					}
					if val.IsFloat() {
						if s.memLimit > 0 {
							if err := s.checkMemWithRoots(int64(c)*8-int64(t.arrHint)*8, TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
						t.consumeArrHint(s)
						arr := make([]float64, 1, c)
						arr[0] = val.Float()
						t.setArrFloat(arr)
						s.addTableMem(t, int64(c)*8)
						t.hashDeleteInt(s, 1)
						return
					}
					if val.IsBool() {
						if s.memLimit > 0 {
							if err := s.checkMemWithRoots(max(int64(c)-int64(t.arrHint)*8, 0), TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
						t.consumeArrHint(s)
						arr := make([]bool, 1, c)
						arr[0] = val.IsTrue()
						t.setArrBool(arr)
						s.addTableMem(t, int64(c))
						t.hashDeleteInt(s, 1)
						return
					}
					if s.memLimit > 0 {
						if err := s.checkMemWithRoots(int64(c)*16-int64(t.arrHint)*8, TableValue(t), val, NilValue()); err != nil {
							s.memExceeded = true
							return
						}
					}
					t.consumeArrHint(s)
					arr := make([]Value, 1, c)
					arr[0] = val
					t.setArrVal(arr)
					s.addTableMem(t, int64(c)*16)
					s.barrierTableVal(t, val)
					t.hashDeleteInt(s, 1)
					return
				}
				// Allow filling within preallocated hint or small sequential index.
				if !val.IsNil() && idx < max(int64(t.arrHint), 16) {
					c := max(int(t.arrHint), int(idx)+1)
					nc := nextCap(c/2, c)
					if s.memLimit > 0 {
						if err := s.checkMemWithRoots(int64(nc)*16-int64(t.arrHint)*8, TableValue(t), val, NilValue()); err != nil {
							s.memExceeded = true
							return
						}
					}
					t.consumeArrHint(s)
					arr := make([]Value, int(idx)+1, nc)
					fillNil(arr)
					t.setArrVal(arr)
					s.addTableMem(t, int64(nc)*16)
					t.migrateHashIntRange(s, 0, int(idx)+1, idx)
					arr[idx] = val
					s.barrierTableVal(t, val)
					return
				}
			case arrInt:
				n := int64(t.arrLen)
				if idx < n {
					if val.IsInt() {
						t.arrIntSlice(nil)[idx] = val.Int()
						return
					}
					if !t.promoteArrToGeneric(s, val) {
						return
					}
					t.arrValSlice(nil)[idx] = val
					s.barrierTableVal(t, val)
					return
				}
				if idx == n && val.IsInt() {
					if uint32(n) == t.arrCap {
						nc := nextCap(int(t.arrCap), int(n)+1)
						delta := int64(nc-int(t.arrCap)) * 8
						if s.memLimit > 0 {
							if err := s.checkMemWithRoots(delta, TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
						grown := make([]int64, n+1, nc)
						copy(grown, t.arrIntSlice(nil))
						grown[idx] = val.Int()
						t.setArrInt(grown)
						s.addTableMem(t, delta)
					} else {
						t.arrLen = uint32(n + 1)
						t.arrIntSlice(nil)[idx] = val.Int()
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
						t.arrFloatSlice(nil)[idx] = val.Float()
						return
					}
					if !t.promoteArrToGeneric(s, val) {
						return
					}
					t.arrValSlice(nil)[idx] = val
					s.barrierTableVal(t, val)
					return
				}
				if idx == n && val.IsFloat() {
					if uint32(n) == t.arrCap {
						nc := nextCap(int(t.arrCap), int(n)+1)
						delta := int64(nc-int(t.arrCap)) * 8
						if s.memLimit > 0 {
							if err := s.checkMemWithRoots(delta, TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
						grown := make([]float64, n+1, nc)
						copy(grown, t.arrFloatSlice(nil))
						grown[idx] = val.Float()
						t.setArrFloat(grown)
						s.addTableMem(t, delta)
					} else {
						t.arrLen = uint32(n + 1)
						t.arrFloatSlice(nil)[idx] = val.Float()
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
						t.arrBoolSlice(nil)[idx] = val.IsTrue()
						return
					}
					if !t.promoteArrToGeneric(s, val) {
						return
					}
					t.arrValSlice(nil)[idx] = val
					s.barrierTableVal(t, val)
					return
				}
				if idx == n && val.IsBool() {
					if uint32(n) == t.arrCap {
						nc := nextCap(int(t.arrCap), int(n)+1)
						delta := int64(nc - int(t.arrCap))
						if s.memLimit > 0 {
							if err := s.checkMemWithRoots(delta, TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
						grown := make([]bool, n+1, nc)
						copy(grown, t.arrBoolSlice(nil))
						grown[idx] = val.IsTrue()
						t.setArrBool(grown)
						s.addTableMem(t, delta)
					} else {
						t.arrLen = uint32(n + 1)
						t.arrBoolSlice(nil)[idx] = val.IsTrue()
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
					t.arrValSlice(nil)[idx] = val
					s.barrierTableVal(t, val)
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
		// Sparse integer or <= 0 goes into hash; demote shape first if needed.
		if t.shape != nil {
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
			t.arrValSlice(nil)[idx] = val
			s.barrierTableVal(t, val)
			return
		}
		if !val.IsNil() && (idx == n || (idx < max(n*2, 16) && idx < 1<<24)) {
			if int(idx)+1 > int(t.arrCap) && s.memLimit > 0 {
				nc := nextCap(int(t.arrCap), int(idx)+1)
				if err := s.checkMemWithRoots(int64(nc-int(t.arrCap))*16, TableValue(t), val, NilValue()); err != nil {
					s.memExceeded = true
					return
				}
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
	oldCap := int(t.arrCap)
	newLen := int(idx) + 1
	if newLen > oldCap {
		nc := nextCap(oldCap, newLen)
		grown := make([]Value, newLen, nc)
		if oldLen > 0 {
			old := t.arrValSlice(nil)
			copy(grown, old)
			clear(old)
		}
		t.setArrVal(grown)
		if s != nil {
			s.addTableMem(t, int64(nc-oldCap)*16)
		}
	} else {
		t.arrLen = uint32(newLen)
	}
	arr := t.arrValSlice(nil)
	if newLen > oldLen+1 {
		fillNil(arr[oldLen : newLen-1])
	}
	if t.hash != nil {
		t.migrateHashIntRange(s, oldLen, newLen, idx)
	}
	arr[idx] = val
	if s != nil {
		s.barrierTableVal(t, val)
	}
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
			return t.hash.nodes[slot].val
		}
		return NilValue()
	case key.IsNil():
		return NilValue()
	default:
		if slot, ok := t.hashFindSlot(s, key); ok {
			return t.hash.nodes[slot].val
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
		if err := s.ensureWriteStrKeyHash(t, s.asString(key), val.IsNil()); err != nil {
			return err
		}
		t.RawSetStr(s, s.asString(key), val)
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
		if OptimizeEnabled && t.shape != nil {
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
		if OptimizeEnabled && t.shape != nil {
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
		if val.IsNil() && slot >= 0 && s != nil && key.IsGCObject() {
			t.flags |= tblFlagHasDeadKeys
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
			if t.hash == nil || t.RawGetInt(s, n+1).IsNil() {
				return n
			}
			return t.hashBorder(s, n)
		}
	}
	n := int(t.arrLen)
	if n > 0 {
		arr := t.arrValSlice(nil)
		if arr[n-1].IsNil() {
			// Binary search for a border inside generic array.
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
	if t.hash == nil || t.RawGetInt(s, int64(n)+1).IsNil() {
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
			// Linear fallback near MaxInt64.
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
	h := t.hash
	if h == nil || len(h.nodes) == 0 {
		return -1, false
	}
	if key.IsFloat() {
		// Match PUC-Rio ltable.c:findindex/equalkey: integral float keys are normalized to
		// integer on insert, so a float key with an exact int64 representation never matches
		// any stored node tag in next().
		if _, ok := FloatToIntExact(key.Float()); ok {
			return -1, false
		}
	}
	mask := h.mask
	idx := hashValue(s, key) & mask
	for {
		nd := &h.nodes[idx]
		if !nd.used {
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
		// Check array first ONLY for genuine integer keys (matching ltable.c:keyinarray).
		if key.IsInt() {
			i := key.Int()
			if i >= 1 && i <= int64(arrLen) {
				startPhase = 0
				startIdx = int(i) // next 0-based index after i-1
				found = true
			}
		}
		if !found && OptimizeEnabled && t.shape != nil && key.IsString() && s.asString(key).IsShort() {
			if slot := t.shape.Lookup(s.asString(key)); slot >= 0 {
				startPhase = 1
				startIdx = int(slot) + 1
				found = true
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
			v := t.arrAt(idx)
			if !v.IsNil() {
				return IntValue(int64(idx + 1)), v, nil
			}
		}
		startPhase = 1
		startIdx = 0
	}
	if startPhase == 1 {
		if OptimizeEnabled && t.shape != nil && t.shape.length > 0 {
			keys := t.shape.Keys()
			for idx := startIdx; idx < len(keys); idx++ {
				if idx < len(t.slots) && !t.slots[idx].IsNil() {
					return StringValue(keys[idx]), t.slots[idx], nil
				}
			}
		}
		startPhase = 2
		startIdx = 0
	}
	if t.hash != nil {
		nodes := t.hash.nodes
		for idx := startIdx; idx < len(nodes); idx++ {
			nd := &nodes[idx]
			if nd.used && !nd.dead && !nd.val.IsNil() {
				return nd.key, nd.val, nil
			}
		}
	}
	return NilValue(), NilValue(), nil
}

func (t *Table) arrAt(idx int) Value {
	if OptimizeEnabled {
		switch t.arrKind {
		case arrInt:
			return IntValue(t.arrIntSlice(nil)[idx])
		case arrFloat:
			return FloatValue(t.arrFloatSlice(nil)[idx])
		case arrBool:
			return BoolValue(t.arrBoolSlice(nil)[idx])
		case arrGeneric:
			return t.arrValSlice(nil)[idx]
		default:
			return NilValue()
		}
	}
	return t.arrValSlice(nil)[idx]
}
