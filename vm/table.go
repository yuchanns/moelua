package vm

import (
	"fmt"
	"math"
	"unsafe"
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
	tblFlagIsProto    uint8 = 1 << 0
	tblFlagGCFnzed    uint8 = 1 << 1
	tblFlagGCInFnz    uint8 = 1 << 2
	tblFlagGCWeakCand uint8 = 1 << 3
	tblFlagGCPending  uint8 = 1 << 4
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

// Table is a Lua 5.5 table supporting both:
//   - Gate 1 (-tags moeluafaithful): hybrid []Value array + open-addressing hash map
//   - Gate 2 (default): homogeneous []int64 / []float64 / []bool arrays + Shape/slots for record tables + lazy dict fallback
//
// Pointer-bearing fields are packed first (ptrdata = 80B) and scalar metadata sits at the tail
// so Table fits an exact 112-byte Go size class with minimal GC scan overhead.
type Table struct {
	// Pointer-bearing fields (ptrdata = 80 bytes: last pointer word is inlineSlots[1].ptr at offset 72..79).
	shape       *Shape
	meta        *Table
	hash        *tableHash
	arrPtr      unsafe.Pointer // backing array for arrInt/arrFloat/arrBool/arrGeneric (GC-traced)
	slots       []Value
	inlineSlots [2]Value // co-allocates up to 2 record fields inline (1 alloc instead of 2)

	// Non-pointer scalar tail (ZERO Go GC scan).
	gcMark  uint64  // Tier 1 GC mark epoch (uint64 prevents 32-bit wraparound)
	arrLen  uint32  // active length of backing array pointed to by arrPtr
	arrCap  uint32  // capacity of backing array pointed to by arrPtr
	arrHint int32   // preallocated capacity hint from table.create / NEWTABLE
	arrKind arrKind // discriminator for arrPtr backing slice type
	tmFlags uint8   // negative metamethod cache (bits 0..5 for TMIndex..TMEq)
	flags   uint8   // tblFlag* bitfield
}

// Compile-time assertion: sizeof(Table) == 112 bytes (exact Go size class).
const (
	_ = uint(112 - unsafe.Sizeof(Table{}))
	_ = uint(unsafe.Sizeof(Table{}) - 112)
)

func (t *Table) isProto() bool { return t.flags&tblFlagIsProto != 0 }

func (t *Table) arrIntSlice() []int64     { return unsafe.Slice((*int64)(t.arrPtr), t.arrLen) }
func (t *Table) arrFloatSlice() []float64 { return unsafe.Slice((*float64)(t.arrPtr), t.arrLen) }
func (t *Table) arrBoolSlice() []bool     { return unsafe.Slice((*bool)(t.arrPtr), t.arrLen) }
func (t *Table) arrValSlice() []Value     { return unsafe.Slice((*Value)(t.arrPtr), t.arrLen) }

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
	if s.memLimit > 0 || s.gc.tier1 {
		s.gc.allocBytes += checkBytes
		s.gcAddDebt(checkBytes)
	}
	return t, nil
}

// NewTable allocates a Lua table with sequence/record capacity hints.
func (s *State) NewTable(nseq, nrec int) *Table {
	t := &Table{}
	if OptimizeEnabled {
		t.shape = s.shapes.root
		if nseq > 0 {
			t.arrHint = int32(nseq)
		}
		if nrec <= len(t.inlineSlots) {
			t.slots = t.inlineSlots[:0]
		} else if nrec <= maxShapeProps {
			t.slots = make([]Value, 0, nrec)
		} else {
			// Large record table (e.g. _ENV or big dict) starts directly in dict mode.
			t.shape = nil
			t.initHash(nrec)
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
		t.initHash(nrec)
	}
	return t
}

// NewDictTable allocates a table directly in dict mode (ideal for _ENV / globals).
func (s *State) NewDictTable(nrec int) *Table {
	t := &Table{}
	if nrec < 16 {
		nrec = 16
	}
	t.initHash(nrec)
	return t
}

func fillNil(vs []Value) {
	n := NilValue()
	for i := range vs {
		vs[i] = n
	}
}

func (t *Table) Metatable() *Table { return t.meta }

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
		if idx := mt.RawGetStr(s.intern.tms[TMIndex]); idx.IsTable() {
			idx.AsTable().markProto(s)
		}
		if nidx := mt.RawGetStr(s.intern.tms[TMNewIndex]); nidx.IsTable() {
			nidx.AsTable().markProto(s)
		}
		s.registerFinalizable(TableValue(t), mt)
		s.registerWeakCandidate(t, mt)
	}
	t.meta = mt
	t.tmFlags = 0
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

func (t *Table) initHash(n int) {
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
}

func (t *Table) checkHashMem(s *State, key, val Value) error {
	if s == nil || (s.memLimit <= 0 && !s.gc.tier1) {
		return nil
	}
	if t.hash == nil || len(t.hash.nodes) == 0 {
		// Match initHash(4) which allocates hashCapFor(4) == 8 nodes.
		return s.checkMemAndAccountWithRoots(hashCapFor(4)*40, TableValue(t), key, val)
	}
	curLen := len(t.hash.nodes)
	curCnt := int(t.hash.count)
	if (curCnt+1)*10 >= curLen*7 {
		growSz := max(curLen*2, 4)
		if int(t.hash.live)*4 < curLen && curLen >= 8 {
			growSz = curLen
		}
		if growSz > curLen {
			return s.checkMemAndAccountWithRoots(int64(growSz-curLen)*40, TableValue(t), key, val)
		}
	}
	return nil
}

func (t *Table) growHash() {
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
	for i := range old {
		nd := &old[i]
		if nd.used && !nd.dead && !nd.val.IsNil() {
			t.hashPutNoGrow(nd.key, nd.val)
		}
	}
}

func hashValue(v Value) uint32 {
	switch {
	case v.IsInt():
		x := uint64(v.Int())
		x ^= x >> 33
		x *= 0xff51afd7ed558ccd
		x ^= x >> 33
		return uint32(x)
	case v.IsString():
		return v.AsString().hash()
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
	case v.ptr == tagTokenLightUD:
		x := v.bits
		x ^= x >> 33
		x *= 0xff51afd7ed558ccd
		return uint32(x ^ (x >> 33))
	default:
		p := uint64(uintptr(v.ptr))
		p ^= p >> 33
		p *= 0xff51afd7ed558ccd
		return uint32(p ^ (p >> 33))
	}
}

func (t *Table) hashFindSlot(key Value) (int, bool) {
	h := t.hash
	if h == nil || len(h.nodes) == 0 {
		return -1, false
	}
	mask := h.mask
	idx := hashValue(key) & mask
	for {
		nd := &h.nodes[idx]
		if !nd.used {
			return -1, false
		}
		if !nd.dead && RawEqual(nd.key, key) {
			return int(idx), true
		}
		idx = (idx + 1) & mask
	}
}

func (t *Table) hashFindStrSlot(k *LuaString) (int, bool) {
	h := t.hash
	if h == nil || len(h.nodes) == 0 {
		return -1, false
	}
	mask := h.mask
	idx := k.hash() & mask
	kv := StringValue(k)
	for {
		nd := &h.nodes[idx]
		if !nd.used {
			return -1, false
		}
		if !nd.dead {
			if nd.key.ptr == kv.ptr {
				return int(idx), true
			}
			if !k.Short && nd.key.IsString() && nd.key.AsString().S == k.S {
				return int(idx), true
			}
		}
		idx = (idx + 1) & mask
	}
}

func (t *Table) hashPutNoGrow(key, val Value) int {
	h := t.hash
	mask := h.mask
	idx := hashValue(key) & mask
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
		} else if RawEqual(nd.key, key) {
			nd.val = val
			return int(idx)
		}
		idx = (idx + 1) & mask
	}
}

func (t *Table) hashSet(key, val Value) int {
	if val.IsNil() {
		if slot, ok := t.hashFindSlot(key); ok {
			h := t.hash
			h.nodes[slot].val = NilValue()
			h.nodes[slot].dead = true
			h.live--
			h.dictGen++
			return slot
		}
		return -1
	}
	if slot, ok := t.hashFindSlot(key); ok {
		t.hash.nodes[slot].val = val
		return slot
	}
	if t.hash == nil || len(t.hash.nodes) == 0 {
		t.initHash(4)
	} else if int(t.hash.count+1)*10 >= len(t.hash.nodes)*7 {
		t.growHash()
	}
	return t.hashPutNoGrow(key, val)
}

// hashSetChecked updates an existing hash entry in place (or deletes on nil) without
// triggering growth or memory checks, and only checks memory before inserting a new key.
func (t *Table) hashSetChecked(s *State, key, val Value) (int, error) {
	if val.IsNil() {
		if slot, ok := t.hashFindSlot(key); ok {
			h := t.hash
			h.nodes[slot].val = NilValue()
			h.nodes[slot].dead = true
			h.live--
			h.dictGen++
			return slot, nil
		}
		return -1, nil
	}
	if slot, ok := t.hashFindSlot(key); ok {
		t.hash.nodes[slot].val = val
		return slot, nil
	}
	if err := t.checkHashMem(s, key, val); err != nil {
		return -1, err
	}
	if t.hash == nil || len(t.hash.nodes) == 0 {
		t.initHash(4)
	} else if int(t.hash.count+1)*10 >= len(t.hash.nodes)*7 {
		t.growHash()
	}
	return t.hashPutNoGrow(key, val), nil
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
	if s != nil && (s.memLimit > 0 || s.gc.tier1) {
		hashBytes := hashCapFor(int64(n)) * 40
		var oldSlotBytes int64
		if c := cap(t.slots); c > len(t.inlineSlots) {
			oldSlotBytes = int64(c) * 16
		}
		delta := hashBytes - oldSlotBytes
		if delta > 0 {
			if err := s.checkMemAndAccountWithRoots(delta, TableValue(t), key, val); err != nil {
				s.memExceeded = true
				return false
			}
		}
	}
	t.initHash(n)
	for i, k := range keys {
		if i < len(t.slots) && !t.slots[i].IsNil() {
			t.hashPutNoGrow(StringValue(k), t.slots[i])
		}
	}
	t.shape = nil
	t.slots = nil
	t.inlineSlots = [2]Value{NilValue(), NilValue()}
	return true
}

// appendSlot appends val to t.slots and clears t.inlineSlots whenever t.slots moves
// off the inline backing array so obsolete inline values are not retained on the Go heap.
func (t *Table) appendSlot(val Value) {
	wasInline := cap(t.slots) <= len(t.inlineSlots)
	t.slots = append(t.slots, val)
	if wasInline && cap(t.slots) > len(t.inlineSlots) {
		t.inlineSlots = [2]Value{NilValue(), NilValue()}
	}
}

// RawGetStr looks up a LuaString key (fast path for GETFIELD/SELF/GETTABUP).
func (t *Table) RawGetStr(k *LuaString) Value {
	if OptimizeEnabled && t.shape != nil {
		if k.Short {
			if idx := t.shape.Lookup(k); idx >= 0 {
				return t.slots[idx]
			}
			return NilValue()
		}
		return NilValue()
	}
	if slot, ok := t.hashFindStrSlot(k); ok {
		return t.hash.nodes[slot].val
	}
	return NilValue()
}

// RawSetStr stores a LuaString key -> val.
func (t *Table) RawSetStr(s *State, k *LuaString, val Value) {
	if err := s.ensureWriteStrKeyHash(t, k, val.IsNil()); err != nil {
		return
	}
	if k == s.intern.tms[TMGC] || k == s.intern.tms[TMMode] {
		s.activateTier1()
		if k == s.intern.tms[TMGC] && t == s.fileMeta {
			s.promoteFileFinalizers()
		}
	}
	if t.isProto() {
		s.protoEpoch++
		if k == s.intern.tms[TMIndex] || k == s.intern.tms[TMNewIndex] {
			if val.IsTable() {
				val.AsTable().markProto(s)
			}
		}
	}
	if t.tmFlags != 0 && k.Short {
		t.tmFlags = 0
	}
	if OptimizeEnabled && t.shape != nil {
		if k.Short {
			if idx := t.shape.Lookup(k); idx >= 0 {
				// Assigning nil keeps slot & shape intact (next/pairs stability!).
				t.slots[idx] = val
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
				if (s.memLimit > 0 || s.gc.tier1) && len(t.slots) == cap(t.slots) {
					if err := s.checkMemAndAccountWithRoots(int64(max(cap(t.slots), 4))*16, TableValue(t), StringValue(k), val); err != nil {
						s.memExceeded = true
						return
					}
				}
				t.shape = next
				t.appendSlot(val)
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
	slot, err := t.hashSetChecked(s, StringValue(k), val)
	if err != nil {
		s.memExceeded = true
		return
	}
	if val.IsNil() && slot >= 0 && s != nil {
		s.gc.hasDeadHashKeys = true
	}
}

// RawGetInt looks up integer key i (1-based dense array fast path).
func (t *Table) RawGetInt(i int64) Value {
	if i >= 1 {
		idx := uint64(i - 1)
		if idx < uint64(t.arrLen) {
			if OptimizeEnabled {
				switch t.arrKind {
				case arrInt:
					return IntValue(t.arrIntSlice()[idx])
				case arrFloat:
					return FloatValue(t.arrFloatSlice()[idx])
				case arrBool:
					return BoolValue(t.arrBoolSlice()[idx])
				case arrGeneric:
					return t.arrValSlice()[idx]
				}
			} else {
				return t.arrValSlice()[idx]
			}
		}
	}
	if t.hash == nil {
		return NilValue()
	}
	if slot, ok := t.hashFindSlot(IntValue(i)); ok {
		return t.hash.nodes[slot].val
	}
	return NilValue()
}

func (t *Table) promoteArrToGeneric(s *State, val Value) bool {
	if s != nil && (s.memLimit > 0 || s.gc.tier1) {
		var oldElem int64
		switch t.arrKind {
		case arrInt, arrFloat:
			oldElem = 8
		case arrBool:
			oldElem = 1
		}
		if oldElem > 0 && t.arrCap > 0 {
			delta := int64(t.arrCap) * (16 - oldElem)
			if err := s.checkMemAndAccountWithRoots(delta, TableValue(t), val, NilValue()); err != nil {
				s.memExceeded = true
				return false
			}
		}
	}
	switch t.arrKind {
	case arrEmpty:
		t.arrKind = arrGeneric
	case arrInt:
		src := t.arrIntSlice()
		g := make([]Value, len(src), t.arrCap)
		for i, v := range src {
			g[i] = IntValue(v)
		}
		t.setArrVal(g)
	case arrFloat:
		src := t.arrFloatSlice()
		g := make([]Value, len(src), t.arrCap)
		for i, v := range src {
			g[i] = FloatValue(v)
		}
		t.setArrVal(g)
	case arrBool:
		src := t.arrBoolSlice()
		g := make([]Value, len(src), t.arrCap)
		for i, v := range src {
			g[i] = BoolValue(v)
		}
		t.setArrVal(g)
	}
	return true
}

func (t *Table) checkGenericArrGrowth(s *State, idx int64, val Value) bool {
	if s != nil && (s.memLimit > 0 || s.gc.tier1) && int(idx)+1 > int(t.arrCap) {
		nc := nextCap(int(t.arrCap), int(idx)+1)
		if err := s.checkMemAndAccountWithRoots(int64(nc-int(t.arrCap))*16, TableValue(t), val, NilValue()); err != nil {
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

func (t *Table) hashDeleteInt(k int64) {
	if t.hash == nil || t.hash.live == 0 {
		return
	}
	if slot, ok := t.hashFindSlot(IntValue(k)); ok {
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
func (t *Table) migrateHashIntRange(oldLen, newLen int, skipIdx int64) {
	if t.hash == nil || t.hash.live == 0 {
		return
	}
	arr := t.arrValSlice()
	for k := oldLen + 1; k <= newLen; k++ {
		if slot, ok := t.hashFindSlot(IntValue(int64(k))); ok {
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
		if slot, ok := t.hashFindSlot(IntValue(i)); ok {
			h := t.hash
			if val.IsNil() {
				h.nodes[slot].val = NilValue()
				h.nodes[slot].dead = true
				h.live--
				h.dictGen++
			} else {
				h.nodes[slot].val = val
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
						if s.memLimit > 0 || s.gc.tier1 {
							if err := s.checkMemAndAccountWithRoots(int64(c)*8, TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
						arr := make([]int64, 1, c)
						arr[0] = val.Int()
						t.setArrInt(arr)
						t.hashDeleteInt(1)
						return
					}
					if val.IsFloat() {
						if s.memLimit > 0 || s.gc.tier1 {
							if err := s.checkMemAndAccountWithRoots(int64(c)*8, TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
						arr := make([]float64, 1, c)
						arr[0] = val.Float()
						t.setArrFloat(arr)
						t.hashDeleteInt(1)
						return
					}
					if val.IsBool() {
						if s.memLimit > 0 || s.gc.tier1 {
							if err := s.checkMemAndAccountWithRoots(int64(c), TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
						arr := make([]bool, 1, c)
						arr[0] = val.IsTrue()
						t.setArrBool(arr)
						t.hashDeleteInt(1)
						return
					}
					if s.memLimit > 0 || s.gc.tier1 {
						if err := s.checkMemAndAccountWithRoots(int64(c)*16, TableValue(t), val, NilValue()); err != nil {
							s.memExceeded = true
							return
						}
					}
					arr := make([]Value, 1, c)
					arr[0] = val
					t.setArrVal(arr)
					t.hashDeleteInt(1)
					return
				}
				// Allow filling within preallocated hint or small sequential index.
				if !val.IsNil() && idx < max(int64(t.arrHint), 16) {
					c := max(int(t.arrHint), int(idx)+1)
					nc := nextCap(c/2, c)
					if s.memLimit > 0 || s.gc.tier1 {
						if err := s.checkMemAndAccountWithRoots(int64(nc)*16, TableValue(t), val, NilValue()); err != nil {
							s.memExceeded = true
							return
						}
					}
					arr := make([]Value, int(idx)+1, nc)
					fillNil(arr)
					t.setArrVal(arr)
					t.migrateHashIntRange(0, int(idx)+1, idx)
					arr[idx] = val
					return
				}
			case arrInt:
				n := int64(t.arrLen)
				if idx < n {
					if val.IsInt() {
						t.arrIntSlice()[idx] = val.Int()
						return
					}
					// Promote on nil (including last element) or type change so next(t, k) never invalidates keys.
					if !t.promoteArrToGeneric(s, val) {
						return
					}
					t.arrValSlice()[idx] = val
					return
				}
				if idx == n && val.IsInt() {
					if uint32(n) == t.arrCap {
						nc := nextCap(int(t.arrCap), int(n)+1)
						if s.memLimit > 0 || s.gc.tier1 {
							if err := s.checkMemAndAccountWithRoots(int64(nc-int(t.arrCap))*8, TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
						grown := make([]int64, n+1, nc)
						copy(grown, t.arrIntSlice())
						grown[idx] = val.Int()
						t.setArrInt(grown)
					} else {
						t.arrLen = uint32(n + 1)
						t.arrIntSlice()[idx] = val.Int()
					}
					t.hashDeleteInt(i)
					return
				}
				if !val.IsNil() && idx < n*2 && idx < 1<<24 {
					if !t.promoteArrToGeneric(s, val) || !t.checkGenericArrGrowth(s, idx, val) {
						return
					}
					t.setArrGeneric(idx, val)
					return
				}
			case arrFloat:
				n := int64(t.arrLen)
				if idx < n {
					if val.IsFloat() {
						t.arrFloatSlice()[idx] = val.Float()
						return
					}
					if !t.promoteArrToGeneric(s, val) {
						return
					}
					t.arrValSlice()[idx] = val
					return
				}
				if idx == n && val.IsFloat() {
					if uint32(n) == t.arrCap {
						nc := nextCap(int(t.arrCap), int(n)+1)
						if s.memLimit > 0 || s.gc.tier1 {
							if err := s.checkMemAndAccountWithRoots(int64(nc-int(t.arrCap))*8, TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
						grown := make([]float64, n+1, nc)
						copy(grown, t.arrFloatSlice())
						grown[idx] = val.Float()
						t.setArrFloat(grown)
					} else {
						t.arrLen = uint32(n + 1)
						t.arrFloatSlice()[idx] = val.Float()
					}
					t.hashDeleteInt(i)
					return
				}
				if !val.IsNil() && idx < n*2 && idx < 1<<24 {
					if !t.promoteArrToGeneric(s, val) || !t.checkGenericArrGrowth(s, idx, val) {
						return
					}
					t.setArrGeneric(idx, val)
					return
				}
			case arrBool:
				n := int64(t.arrLen)
				if idx < n {
					if val.IsBool() {
						t.arrBoolSlice()[idx] = val.IsTrue()
						return
					}
					if !t.promoteArrToGeneric(s, val) {
						return
					}
					t.arrValSlice()[idx] = val
					return
				}
				if idx == n && val.IsBool() {
					if uint32(n) == t.arrCap {
						nc := nextCap(int(t.arrCap), int(n)+1)
						if s.memLimit > 0 || s.gc.tier1 {
							if err := s.checkMemAndAccountWithRoots(int64(nc-int(t.arrCap)), TableValue(t), val, NilValue()); err != nil {
								s.memExceeded = true
								return
							}
						}
						grown := make([]bool, n+1, nc)
						copy(grown, t.arrBoolSlice())
						grown[idx] = val.IsTrue()
						t.setArrBool(grown)
					} else {
						t.arrLen = uint32(n + 1)
						t.arrBoolSlice()[idx] = val.IsTrue()
					}
					t.hashDeleteInt(i)
					return
				}
				if !val.IsNil() && idx < n*2 && idx < 1<<24 {
					if !t.promoteArrToGeneric(s, val) || !t.checkGenericArrGrowth(s, idx, val) {
						return
					}
					t.setArrGeneric(idx, val)
					return
				}
			case arrGeneric:
				n := int64(t.arrLen)
				if idx < n {
					t.arrValSlice()[idx] = val
					return
				}
				if !val.IsNil() && (idx == n || (idx < max(n*2, 16) && idx < 1<<24)) {
					if !t.checkGenericArrGrowth(s, idx, val) {
						return
					}
					t.setArrGeneric(idx, val)
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
			t.arrValSlice()[idx] = val
			return
		}
		if !val.IsNil() && (idx == n || (idx < max(n*2, 16) && idx < 1<<24)) {
			if int(idx)+1 > int(t.arrCap) && (s.memLimit > 0 || s.gc.tier1) {
				nc := nextCap(int(t.arrCap), int(idx)+1)
				if err := s.checkMemAndAccountWithRoots(int64(nc-int(t.arrCap))*16, TableValue(t), val, NilValue()); err != nil {
					s.memExceeded = true
					return
				}
			}
			t.setArrGeneric(idx, val)
			return
		}
	}
	if _, err := t.hashSetChecked(s, IntValue(i), val); err != nil {
		s.memExceeded = true
		return
	}
}

func (t *Table) setArrGeneric(idx int64, val Value) {
	oldLen := int(t.arrLen)
	newLen := int(idx) + 1
	if newLen > int(t.arrCap) {
		grown := make([]Value, newLen, nextCap(int(t.arrCap), newLen))
		if oldLen > 0 {
			copy(grown, t.arrValSlice())
		}
		t.setArrVal(grown)
	} else {
		t.arrLen = uint32(newLen)
	}
	arr := t.arrValSlice()
	if newLen > oldLen+1 {
		fillNil(arr[oldLen : newLen-1])
	}
	if t.hash != nil {
		t.migrateHashIntRange(oldLen, newLen, idx)
	}
	arr[idx] = val
}

// RawGet looks up any Lua Value key.
func (t *Table) RawGet(key Value) Value {
	switch {
	case key.IsInt():
		return t.RawGetInt(key.Int())
	case key.IsString():
		return t.RawGetStr(key.AsString())
	case key.IsFloat():
		f := key.Float()
		if math.IsNaN(f) {
			return NilValue()
		}
		if i, ok := FloatToIntExact(f); ok {
			return t.RawGetInt(i)
		}
		if slot, ok := t.hashFindSlot(key); ok {
			return t.hash.nodes[slot].val
		}
		return NilValue()
	case key.IsNil():
		return NilValue()
	default:
		if slot, ok := t.hashFindSlot(key); ok {
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
		if err := s.ensureWriteStrKeyHash(t, key.AsString(), val.IsNil()); err != nil {
			return err
		}
		t.RawSetStr(s, key.AsString(), val)
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
		if val.IsNil() && slot >= 0 && s != nil && (key.IsString() || isCollectableWeak(key)) {
			s.gc.hasDeadHashKeys = true
		}
		return nil
	}
}

// Len computes Lua's raw #t (border search across dense array + hash part).
func (t *Table) Len() int64 {
	if OptimizeEnabled {
		switch t.arrKind {
		case arrInt, arrFloat, arrBool:
			n := int64(t.arrLen)
			if t.hash == nil || t.RawGetInt(n+1).IsNil() {
				return n
			}
			return t.hashBorder(n)
		}
	}
	n := int(t.arrLen)
	if n > 0 {
		arr := t.arrValSlice()
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
	if t.hash == nil || t.RawGetInt(int64(n)+1).IsNil() {
		return int64(n)
	}
	return t.hashBorder(int64(n))
}

func (t *Table) hashBorder(j int64) int64 {
	i := j
	j++
	for !t.RawGetInt(j).IsNil() {
		i = j
		if j > math.MaxInt64/2 {
			// Linear fallback near MaxInt64.
			for j = 1; !t.RawGetInt(j).IsNil(); j++ {
			}
			return j - 1
		}
		j *= 2
	}
	for j-i > 1 {
		m := i + (j-i)/2
		if t.RawGetInt(m).IsNil() {
			j = m
		} else {
			i = m
		}
	}
	return i
}

// hashFindSlotForNext finds key in t.hash.nodes including dead/tombstoned keys (needed by Lua's next()).
func (t *Table) hashFindSlotForNext(key Value) (int, bool) {
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
	idx := hashValue(key) & mask
	for {
		nd := &h.nodes[idx]
		if !nd.used {
			return -1, false
		}
		if RawEqual(nd.key, key) {
			return int(idx), true
		}
		idx = (idx + 1) & mask
	}
}

// Next implements Lua's next(t, key) across array, shape slots, and hash nodes.
func (t *Table) Next(key Value) (Value, Value, error) {
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
		if !found && OptimizeEnabled && t.shape != nil && key.IsString() && key.AsString().Short {
			if slot := t.shape.Lookup(key.AsString()); slot >= 0 {
				startPhase = 1
				startIdx = int(slot) + 1
				found = true
			}
		}
		if !found {
			if slot, ok := t.hashFindSlotForNext(key); ok {
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
			return IntValue(t.arrIntSlice()[idx])
		case arrFloat:
			return FloatValue(t.arrFloatSlice()[idx])
		case arrBool:
			return BoolValue(t.arrBoolSlice()[idx])
		case arrGeneric:
			return t.arrValSlice()[idx]
		default:
			return NilValue()
		}
	}
	return t.arrValSlice()[idx]
}
