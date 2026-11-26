package vm

import "weak"

const maxShapeProps = 64

// Shape represents a hidden class for record-like Lua tables (per State).
// The metatable pointer is encoded directly inside the Shape so a single pointer
// comparison covers receiver layout, __index/__newindex, and metatable identity.
type Shape struct {
	id     uint64
	parent *Shape
	key    *LuaString // key added by this transition (nil on root shapes)
	slot   int32      // slot index for key (-1 on root shapes)
	length int32      // total number of slots in this shape
	meta   *Table     // metatable encoded in this shape (may be nil)

	// Fast lookup for >8 properties (lazily populated).
	index map[*LuaString]int32

	// Transitions: inline small list for <= 4 children, weak map beyond that so unused shapes/keys can be reclaimed.
	transSmall   [4]shapeTrans
	nTrans       uint8
	frozen       bool // true for read-only shapes from frozen stdlib template (transitions go to shapeRegistry.frozenTrans)
	transMap     map[weak.Pointer[LuaString]]weak.Pointer[Shape]
	transInserts uint32
}

type shapeTrans struct {
	key   weak.Pointer[LuaString]
	child weak.Pointer[Shape]
}

// Lookup returns the slot index for short-string key k, or -1 if absent.
func (sh *Shape) Lookup(k *LuaString) int32 {
	if sh == nil || sh.length == 0 {
		return -1
	}
	if sh.index != nil {
		if idx, ok := sh.index[k]; ok {
			return idx
		}
		return -1
	}
	for cur := sh; cur != nil && cur.key != nil; cur = cur.parent {
		if cur.key == k {
			return cur.slot
		}
	}
	return -1
}

// Keys returns the ordered keys of this shape (slot 0..length-1).
func (sh *Shape) Keys() []*LuaString {
	if sh == nil || sh.length == 0 {
		return nil
	}
	out := make([]*LuaString, sh.length)
	for cur := sh; cur != nil && cur.key != nil; cur = cur.parent {
		out[cur.slot] = cur.key
	}
	return out
}

type weakFrozenTransKey struct {
	shape weak.Pointer[Shape]
	key   weak.Pointer[LuaString]
}

type shapeRegistry struct {
	nextID      uint64
	frozenMaxID uint64
	rootShape   Shape // embedded per-State root shape (meta == nil) -> zero extra alloc
	root        *Shape
	// metaRoots weakly caches root shapes per metatable so unreachable metatables are collected by Go's GC.
	metaRoots map[weak.Pointer[Table]]weak.Pointer[Shape]
	// metaTrans weakly caches (shape, newMeta) -> shape with same keys and newMeta.
	metaTrans map[weakMetaTransKey]weak.Pointer[Shape]
	// frozenTrans weakly caches transitions out of read-only frozen stdlib shapes without mutating them.
	frozenTrans map[weakFrozenTransKey]weak.Pointer[Shape]
	metaInserts uint32
}

type weakMetaTransKey struct {
	shape   weak.Pointer[Shape]
	meta    weak.Pointer[Table]
	hasMeta bool
}

func (sr *shapeRegistry) init(nextID uint64) {
	sr.frozenMaxID = nextID
	sr.nextID = nextID + 1
	sr.rootShape = Shape{id: sr.nextID, slot: -1}
	sr.root = &sr.rootShape
}

func newShapeRegistry() shapeRegistry {
	var sr shapeRegistry
	sr.nextID = 1
	sr.rootShape = Shape{id: 1, slot: -1}
	sr.root = &sr.rootShape
	return sr
}

func pruneShapeTransMap(sh *Shape) {
	for k, v := range sh.transMap {
		if k.Value() == nil || v.Value() == nil {
			delete(sh.transMap, k)
		}
	}
}

// Prune removes dead weak entries from metaRoots, metaTrans, frozenTrans, and root transitions.
func (sr *shapeRegistry) Prune() {
	for k, v := range sr.metaRoots {
		if k.Value() == nil || v.Value() == nil {
			delete(sr.metaRoots, k)
		}
	}
	for k, v := range sr.metaTrans {
		if k.shape.Value() == nil || (k.hasMeta && k.meta.Value() == nil) || v.Value() == nil {
			delete(sr.metaTrans, k)
		}
	}
	for k, v := range sr.frozenTrans {
		if k.shape.Value() == nil || k.key.Value() == nil || v.Value() == nil {
			delete(sr.frozenTrans, k)
		}
	}
	if sr.root != nil && sr.root.transMap != nil {
		pruneShapeTransMap(sr.root)
	}
}

func (sr *shapeRegistry) maybePrune() {
	sr.metaInserts++
	if sr.metaInserts&255 == 0 {
		sr.Prune()
	}
}

// shapeNodeBytes estimates the retained Go heap storage of a single non-frozen Shape node
// (excluding parent chain and interned key strings, which are counted separately).
func shapeNodeBytes(sh *Shape) int64 {
	if sh == nil || sh.frozen {
		return 0
	}
	n := int64(192)
	if sh.index != nil {
		n += 48 + int64(len(sh.index))*32
	}
	if sh.transMap != nil {
		n += 48 + int64(len(sh.transMap))*32
	}
	return n
}

func (sr *shapeRegistry) Root(s *State, meta *Table, rootObj Value) *Shape {
	if meta == nil {
		return sr.root
	}
	wk := weak.Make(meta)
	if sr.metaRoots == nil {
		sr.metaRoots = make(map[weak.Pointer[Table]]weak.Pointer[Shape], 16)
	} else if wp, ok := sr.metaRoots[wk]; ok {
		if sh := wp.Value(); sh != nil {
			return sh
		}
	}
	if s != nil && (s.memLimit > 0 || s.gc.tier1) {
		if err := s.checkMemAndAccountWithRoots(192, rootObj, TableValue(meta), NilValue()); err != nil {
			s.memExceeded = true
			return nil
		}
	}
	sr.nextID++
	sh := &Shape{id: sr.nextID, slot: -1, meta: meta}
	sr.metaRoots[wk] = weak.Make(sh)
	sr.maybePrune()
	return sh
}

// Transition returns the child shape adding key k (assumed absent from sh), or (nil, nil) if maxShapeProps exceeded.
func (sr *shapeRegistry) Transition(s *State, sh *Shape, k *LuaString, r1, r2 Value) (*Shape, error) {
	if sh.length >= maxShapeProps {
		return nil, nil
	}
	wk := weak.Make(k)
	if sh.frozen {
		fk := weakFrozenTransKey{shape: weak.Make(sh), key: wk}
		if sr.frozenTrans != nil {
			if wp, ok := sr.frozenTrans[fk]; ok {
				if child := wp.Value(); child != nil {
					return child, nil
				}
			}
		} else {
			sr.frozenTrans = make(map[weakFrozenTransKey]weak.Pointer[Shape], 8)
		}
		if s != nil && (s.memLimit > 0 || s.gc.tier1) {
			est := int64(192)
			if sh.length+1 > 8 {
				est += 48 + int64(sh.length+1)*32
			}
			if err := s.checkMemAndAccountWithRoots(est, r1, StringValue(k), r2); err != nil {
				return nil, err
			}
		}
		sr.nextID++
		child := &Shape{
			id:     sr.nextID,
			parent: sh,
			key:    k,
			slot:   sh.length,
			length: sh.length + 1,
			meta:   sh.meta,
		}
		if child.length > 8 {
			idx := make(map[*LuaString]int32, child.length)
			for cur := child; cur != nil && cur.key != nil; cur = cur.parent {
				idx[cur.key] = cur.slot
			}
			child.index = idx
		}
		sr.frozenTrans[fk] = weak.Make(child)
		sr.maybePrune()
		return child, nil
	}
	freeSmall := -1
	for i := range int(sh.nTrans) {
		st := sh.transSmall[i]
		if st.key == wk {
			if child := st.child.Value(); child != nil {
				return child, nil
			}
			freeSmall = i
		} else if freeSmall < 0 && (st.key.Value() == nil || st.child.Value() == nil) {
			freeSmall = i
		}
	}
	if sh.transMap != nil {
		if wp, ok := sh.transMap[wk]; ok {
			if child := wp.Value(); child != nil {
				return child, nil
			}
		}
	}
	if s != nil && (s.memLimit > 0 || s.gc.tier1) {
		est := int64(192)
		if sh.length+1 > 8 {
			est += 48 + int64(sh.length+1)*32
		}
		if freeSmall < 0 && sh.nTrans >= 4 {
			if sh.transMap == nil {
				est += 48 + 8*32
			} else {
				est += 32
			}
		}
		if err := s.checkMemAndAccountWithRoots(est, r1, StringValue(k), r2); err != nil {
			return nil, err
		}
	}
	sr.nextID++
	child := &Shape{
		id:     sr.nextID,
		parent: sh,
		key:    k,
		slot:   sh.length,
		length: sh.length + 1,
		meta:   sh.meta,
	}
	if child.length > 8 {
		idx := make(map[*LuaString]int32, child.length)
		for cur := child; cur != nil && cur.key != nil; cur = cur.parent {
			idx[cur.key] = cur.slot
		}
		child.index = idx
	}
	if freeSmall >= 0 {
		sh.transSmall[freeSmall] = shapeTrans{key: wk, child: weak.Make(child)}
	} else if sh.nTrans < 4 {
		sh.transSmall[sh.nTrans] = shapeTrans{key: wk, child: weak.Make(child)}
		sh.nTrans++
	} else {
		if sh.transMap == nil {
			sh.transMap = make(map[weak.Pointer[LuaString]]weak.Pointer[Shape], 8)
		}
		sh.transMap[wk] = weak.Make(child)
		sh.transInserts++
		if sh.transInserts&255 == 0 {
			pruneShapeTransMap(sh)
		}
	}
	return child, nil
}

// WithMeta returns a shape having the exact same ordered keys as sh, but with metatable meta.
// Returns (newShape, true) on success, or (sh, false) when memory allocation fails.
func (sr *shapeRegistry) WithMeta(s *State, sh *Shape, meta *Table, rootObj Value) (*Shape, bool) {
	if sh == nil {
		r := sr.Root(s, meta, rootObj)
		return r, r != nil
	}
	if sh.meta == meta {
		return sh, true
	}
	if sh.length == 0 {
		if r := sr.Root(s, meta, rootObj); r != nil {
			return r, true
		}
		if s != nil {
			s.memExceeded = true
		}
		return sh, false
	}
	var wm weak.Pointer[Table]
	if meta != nil {
		wm = weak.Make(meta)
	}
	key := weakMetaTransKey{shape: weak.Make(sh), meta: wm, hasMeta: meta != nil}
	if sr.metaTrans != nil {
		if wp, ok := sr.metaTrans[key]; ok {
			if cached := wp.Value(); cached != nil {
				return cached, true
			}
		}
	} else {
		sr.metaTrans = make(map[weakMetaTransKey]weak.Pointer[Shape], 16)
	}
	cur := sr.Root(s, meta, rootObj)
	if cur == nil {
		if s != nil {
			s.memExceeded = true
		}
		return sh, false
	}
	metaVal := NilValue()
	if meta != nil {
		metaVal = TableValue(meta)
	}
	keys := sh.Keys()
	for _, k := range keys {
		next, err := sr.Transition(s, cur, k, rootObj, metaVal)
		if err != nil {
			if s != nil {
				s.memExceeded = true
			}
			return sh, false
		}
		if next == nil {
			if s != nil {
				s.memExceeded = true
			}
			return sh, false
		}
		cur = next
	}
	sr.metaTrans[key] = weak.Make(cur)
	sr.maybePrune()
	return cur, true
}

// icKind identifies what a per-PC inline cache entry caches.
type icKind uint8

const (
	icEmpty icKind = iota
	icOwnSlot      // receiver.shape.id == shapeID => receiver.slots[slot]
	icProtoVal     // receiver.shape.id == shapeID && (noMeta || s.protoEpoch == epoch) => protoValue()
	icAddSlot      // SETFIELD transition: receiver.shape.id == shapeID && (noMeta || s.protoEpoch == epoch) => targetShape
	icEnvSlot      // GETTABUP on dict table: holder.Value() == table && holder.dictGen == epoch => holder.nodes[slot].val
)

// icWeakKind discriminates how an icProtoVal entry stores its cached value without
// holding any strong Go reference to collectable Lua heap objects in Tier 0.
type icWeakKind uint8

const (
	icWeakNone    icWeakKind = iota // immediate value stored directly in val (nil/bool/num/lightUD)
	icWeakClosure                   // fast weak.Pointer[Closure] stored in weakCl (OOP method calls)
	icWeakAny                       // weak.Pointer[Table|LuaString|Userdata|Thread|NativeFunc] stored in weakVal
)

func encodeICProtoVal(v Value) (val Value, weakCl weak.Pointer[Closure], weakVal any, wk icWeakKind) {
	switch {
	case v.IsClosure():
		return NilValue(), weak.Make(v.AsClosure()), nil, icWeakClosure
	case v.IsTable():
		return NilValue(), weak.Pointer[Closure]{}, weak.Make(v.AsTable()), icWeakAny
	case v.IsString():
		return NilValue(), weak.Pointer[Closure]{}, weak.Make(v.AsString()), icWeakAny
	case v.IsUserdata():
		return NilValue(), weak.Pointer[Closure]{}, weak.Make(v.AsUserdata()), icWeakAny
	case v.IsThread():
		return NilValue(), weak.Pointer[Closure]{}, weak.Make(v.AsThread()), icWeakAny
	case v.IsNative():
		return NilValue(), weak.Pointer[Closure]{}, weak.Make(v.AsNative()), icWeakAny
	default:
		return v, weak.Pointer[Closure]{}, nil, icWeakNone
	}
}

//go:noinline
func loadWeakICVal(w any) (Value, bool) {
	switch wp := w.(type) {
	case weak.Pointer[Table]:
		if t := wp.Value(); t != nil {
			return TableValue(t), true
		}
	case weak.Pointer[LuaString]:
		if ls := wp.Value(); ls != nil {
			return StringValue(ls), true
		}
	case weak.Pointer[Userdata]:
		if u := wp.Value(); u != nil {
			return UserdataValue(u), true
		}
	case weak.Pointer[Thread]:
		if th := wp.Value(); th != nil {
			return ThreadValue(th), true
		}
	case weak.Pointer[NativeFunc]:
		if nf := wp.Value(); nf != nil {
			return NativeValue(nf), true
		}
	}
	return NilValue(), false
}

// polyEntry is one slot in a lazily-allocated 4-way polymorphic IC (only used on polymorphic sites).
// All cached references to Lua objects and shapes are weak or numeric IDs so Tier 0 Go GC can
// reclaim discarded prototypes, metatables, shapes, and values without State.CollectFull.
type polyEntry struct {
	shapeID     uint64
	targetShape weak.Pointer[Shape]
	val         Value
	weakCl      weak.Pointer[Closure]
	weakVal     any
	epoch       uint64
	slot        int32
	kind        icKind
	weakKind    icWeakKind
	noMeta      bool
	protoAbsent bool
}

func (pe *polyEntry) protoValue() (Value, bool) {
	if pe.weakKind == icWeakNone {
		return pe.val, true
	}
	if pe.weakKind == icWeakClosure {
		if cl := pe.weakCl.Value(); cl != nil {
			return ClosureValue(cl), true
		}
		return NilValue(), false
	}
	return loadWeakICVal(pe.weakVal)
}

// ICEntry is a per-State, per-PC inline cache entry (never shared across goroutines).
// Monomorphic sites hit directly on the primary fields with zero extra indirection;
// polymorphic sites (2..4 shapes, e.g. OOP method dispatch across subclasses) lazily
// attach a 4-way poly array and promote hits to primary (MRU).
// No field holds a strong Go pointer to any collectable Lua object or Shape.
type ICEntry struct {
	shapeID     uint64
	targetShape weak.Pointer[Shape] // for icAddSlot
	holder      weak.Pointer[Table] // for icEnvSlot
	poly        *[4]polyEntry
	val         Value                 // for icProtoVal when weakKind == icWeakNone
	weakCl      weak.Pointer[Closure] // for icProtoVal when weakKind == icWeakClosure
	weakVal     any                   // for icProtoVal when weakKind == icWeakAny
	epoch       uint64                // protoEpoch or dictGen (uint64 prevents 32-bit wraparound)
	slot        int32
	kind        icKind
	weakKind    icWeakKind
	noMeta      bool // true when shape.meta == nil
	protoAbsent bool // true when nil slot / missing key verified absent in __index / __newindex at epoch
	polyCount   uint8
}

//go:noinline
func (ic *ICEntry) protoValue() (Value, bool) {
	if ic.weakKind == icWeakNone {
		return ic.val, true
	}
	if ic.weakKind == icWeakClosure {
		if cl := ic.weakCl.Value(); cl != nil {
			return ClosureValue(cl), true
		}
		return NilValue(), false
	}
	return loadWeakICVal(ic.weakVal)
}

func (ic *ICEntry) getTabUpFast(t *Table) (Value, bool) {
	if ic.kind == icEnvSlot && ic.holder.Value() == t && ic.epoch == t.hash.dictGen {
		v := t.hash.nodes[ic.slot].val
		if !v.IsNil() || ic.noMeta {
			return v, true
		}
	}
	if ic.kind == icOwnSlot && t.shape != nil && t.shape.id == ic.shapeID {
		v := t.slots[ic.slot]
		if !v.IsNil() || ic.noMeta {
			return v, true
		}
	}
	return NilValue(), false
}

func (ic *ICEntry) getFieldFast(t *Table, protoEpoch uint64) (Value, bool) {
	if t.shape != nil && t.shape.id == ic.shapeID {
		if ic.kind == icOwnSlot {
			v := t.slots[ic.slot]
			if !v.IsNil() || ic.noMeta || (ic.protoAbsent && protoEpoch == ic.epoch) {
				return v, true
			}
		} else if ic.kind == icProtoVal && (ic.noMeta || protoEpoch == ic.epoch) {
			return ic.protoValue()
		}
	}
	return NilValue(), false
}

func (ic *ICEntry) setFieldFast(t *Table, val Value, protoEpoch uint64) bool {
	if t.shape != nil && t.shape.id == ic.shapeID && t.flags&tblFlagIsProto == 0 {
		if ic.kind == icOwnSlot {
			if !t.slots[ic.slot].IsNil() || ic.noMeta || (ic.protoAbsent && protoEpoch == ic.epoch) {
				t.slots[ic.slot] = val
				return true
			}
		} else if ic.kind == icAddSlot && !val.IsNil() && (ic.noMeta || protoEpoch == ic.epoch) {
			slot := int(ic.slot)
			if ts := ic.targetShape.Value(); ts != nil && slot < cap(t.slots) {
				t.slots = t.slots[:slot+1]
				t.slots[slot] = val
				t.shape = ts
				return true
			}
		}
	}
	return false
}

func (ic *ICEntry) setOwnSlot(shapeID uint64, slot int32, noMeta, protoAbsent bool, epoch uint64) {
	ic.kind = icOwnSlot
	ic.shapeID = shapeID
	ic.targetShape = weak.Pointer[Shape]{}
	ic.holder = weak.Pointer[Table]{}
	ic.val = NilValue()
	ic.weakCl = weak.Pointer[Closure]{}
	ic.weakVal = nil
	ic.weakKind = icWeakNone
	ic.slot = slot
	ic.noMeta = noMeta
	ic.protoAbsent = protoAbsent
	ic.epoch = epoch
}

func (ic *ICEntry) setProtoVal(shapeID uint64, v Value, epoch uint64, noMeta bool) {
	val, weakCl, weakVal, wk := encodeICProtoVal(v)
	ic.kind = icProtoVal
	ic.shapeID = shapeID
	ic.targetShape = weak.Pointer[Shape]{}
	ic.holder = weak.Pointer[Table]{}
	ic.val = val
	ic.weakCl = weakCl
	ic.weakVal = weakVal
	ic.weakKind = wk
	ic.slot = 0
	ic.noMeta = noMeta
	ic.protoAbsent = false
	ic.epoch = epoch
}

func (ic *ICEntry) setAddSlot(shapeID uint64, target *Shape, slot int32, epoch uint64, noMeta bool) {
	ic.kind = icAddSlot
	ic.shapeID = shapeID
	ic.targetShape = weak.Make(target)
	ic.holder = weak.Pointer[Table]{}
	ic.val = NilValue()
	ic.weakCl = weak.Pointer[Closure]{}
	ic.weakVal = nil
	ic.weakKind = icWeakNone
	ic.slot = slot
	ic.epoch = epoch
	ic.noMeta = noMeta
	ic.protoAbsent = false
}

func (ic *ICEntry) setEnvSlot(holder *Table, dictGen uint64, slot int32, noMeta bool) {
	*ic = ICEntry{
		kind:   icEnvSlot,
		holder: weak.Make(holder),
		epoch:  dictGen,
		slot:   slot,
		noMeta: noMeta,
	}
}

// polySavePrimary inserts the displaced primary entry into the 4-way polymorphic cache.
func (ic *ICEntry) polySavePrimary() {
	if ic.kind != icOwnSlot && ic.kind != icProtoVal && ic.kind != icAddSlot {
		return
	}
	if ic.poly == nil {
		ic.poly = new([4]polyEntry)
	}
	// Update existing shape entry if already present.
	for i := range int(ic.polyCount) {
		if ic.poly[i].shapeID == ic.shapeID {
			ic.poly[i] = polyEntry{
				shapeID: ic.shapeID, targetShape: ic.targetShape,
				val: ic.val, weakCl: ic.weakCl, weakVal: ic.weakVal, weakKind: ic.weakKind,
				epoch: ic.epoch, slot: ic.slot, kind: ic.kind,
				noMeta: ic.noMeta, protoAbsent: ic.protoAbsent,
			}
			return
		}
	}
	idx := int(ic.polyCount)
	if idx >= 4 {
		idx = 3 // evict oldest slot
	} else {
		ic.polyCount++
	}
	ic.poly[idx] = polyEntry{
		shapeID: ic.shapeID, targetShape: ic.targetShape,
		val: ic.val, weakCl: ic.weakCl, weakVal: ic.weakVal, weakKind: ic.weakKind,
		epoch: ic.epoch, slot: ic.slot, kind: ic.kind,
		noMeta: ic.noMeta, protoAbsent: ic.protoAbsent,
	}
}

// polyLookup searches the 4-way poly array for shapeID; on hit, swaps it into the primary MRU slot.
func (ic *ICEntry) polyLookup(shapeID uint64) bool {
	if ic.poly == nil {
		return false
	}
	for i := range int(ic.polyCount) {
		pe := &ic.poly[i]
		if pe.shapeID == shapeID {
			// Swap primary <-> pe (MRU promotion).
			old := polyEntry{
				shapeID: ic.shapeID, targetShape: ic.targetShape,
				val: ic.val, weakCl: ic.weakCl, weakVal: ic.weakVal, weakKind: ic.weakKind,
				epoch: ic.epoch, slot: ic.slot, kind: ic.kind,
				noMeta: ic.noMeta, protoAbsent: ic.protoAbsent,
			}
			ic.shapeID = pe.shapeID
			ic.targetShape = pe.targetShape
			ic.val = pe.val
			ic.weakCl = pe.weakCl
			ic.weakVal = pe.weakVal
			ic.weakKind = pe.weakKind
			ic.epoch = pe.epoch
			ic.slot = pe.slot
			ic.kind = pe.kind
			ic.noMeta = pe.noMeta
			ic.protoAbsent = pe.protoAbsent
			*pe = old
			return true
		}
	}
	return false
}
