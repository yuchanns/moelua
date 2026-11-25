package vm

import "weak"

const maxShapeProps = 64

// Shape represents a hidden class for record-like Lua tables (per State).
// The metatable pointer is encoded directly inside the Shape so a single pointer
// comparison covers receiver layout, __index/__newindex, and metatable identity.
type Shape struct {
	id     uint32
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
	nextID    uint32
	rootShape Shape // embedded per-State root shape (meta == nil) -> zero extra alloc
	root      *Shape
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

func (sr *shapeRegistry) init(nextID uint32) {
	sr.nextID = nextID
	sr.rootShape = Shape{id: 1, slot: -1}
	sr.root = &sr.rootShape
}

func newShapeRegistry() shapeRegistry {
	var sr shapeRegistry
	sr.init(1)
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

func (sr *shapeRegistry) Root(meta *Table) *Shape {
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
	sr.nextID++
	sh := &Shape{id: sr.nextID, slot: -1, meta: meta}
	sr.metaRoots[wk] = weak.Make(sh)
	sr.maybePrune()
	return sh
}

// Transition returns the child shape adding key k (assumed absent from sh), or nil if maxShapeProps exceeded.
func (sr *shapeRegistry) Transition(sh *Shape, k *LuaString) *Shape {
	if sh.length >= maxShapeProps {
		return nil
	}
	wk := weak.Make(k)
	if sh.frozen {
		fk := weakFrozenTransKey{shape: weak.Make(sh), key: wk}
		if sr.frozenTrans != nil {
			if wp, ok := sr.frozenTrans[fk]; ok {
				if child := wp.Value(); child != nil {
					return child
				}
			}
		} else {
			sr.frozenTrans = make(map[weakFrozenTransKey]weak.Pointer[Shape], 8)
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
		return child
	}
	freeSmall := -1
	for i := range int(sh.nTrans) {
		st := sh.transSmall[i]
		if st.key == wk {
			if child := st.child.Value(); child != nil {
				return child
			}
			freeSmall = i
		} else if freeSmall < 0 && (st.key.Value() == nil || st.child.Value() == nil) {
			freeSmall = i
		}
	}
	if sh.transMap != nil {
		if wp, ok := sh.transMap[wk]; ok {
			if child := wp.Value(); child != nil {
				return child
			}
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
	return child
}

// WithMeta returns a shape having the exact same ordered keys as sh, but with metatable meta.
func (sr *shapeRegistry) WithMeta(sh *Shape, meta *Table) *Shape {
	if sh == nil {
		return sr.Root(meta)
	}
	if sh.meta == meta {
		return sh
	}
	if sh.length == 0 {
		return sr.Root(meta)
	}
	var wm weak.Pointer[Table]
	if meta != nil {
		wm = weak.Make(meta)
	}
	key := weakMetaTransKey{shape: weak.Make(sh), meta: wm, hasMeta: meta != nil}
	if sr.metaTrans != nil {
		if wp, ok := sr.metaTrans[key]; ok {
			if cached := wp.Value(); cached != nil {
				return cached
			}
		}
	} else {
		sr.metaTrans = make(map[weakMetaTransKey]weak.Pointer[Shape], 16)
	}
	cur := sr.Root(meta)
	keys := sh.Keys()
	for _, k := range keys {
		cur = sr.Transition(cur, k)
	}
	sr.metaTrans[key] = weak.Make(cur)
	sr.maybePrune()
	return cur
}

// icKind identifies what a per-PC inline cache entry caches.
type icKind uint8

const (
	icEmpty icKind = iota
	icOwnSlot      // receiver.shape == shape => receiver.slots[slot]
	icProtoVal     // receiver.shape == shape && (noMeta || s.protoEpoch == epoch) => val
	icAddSlot      // SETFIELD transition: receiver.shape == shape && (noMeta || s.protoEpoch == epoch) => targetShape
	icEnvSlot      // GETTABUP on dict table: holder == table && holder.dictGen == epoch => holder.nodes[slot].val
)

// polyEntry is one slot in a lazily-allocated 4-way polymorphic IC (only used on polymorphic sites).
type polyEntry struct {
	shape       *Shape
	targetShape *Shape
	val         Value
	epoch       uint32
	slot        int32
	kind        icKind
	noMeta      bool
	protoAbsent bool
}

// ICEntry is a per-State, per-PC inline cache entry (never shared across goroutines).
// Monomorphic sites hit directly on the primary fields with zero extra indirection;
// polymorphic sites (2..4 shapes, e.g. OOP method dispatch across subclasses) lazily
// attach a 4-way poly array and promote hits to primary (MRU).
type ICEntry struct {
	shape       *Shape
	targetShape *Shape // for icAddSlot
	holder      *Table // for icEnvSlot
	poly        *[4]polyEntry
	val         Value  // for icProtoVal (cached method/field from __index chain or absent nil)
	epoch       uint32 // protoEpoch or dictGen
	slot        int32
	kind        icKind
	noMeta      bool // true when shape.meta == nil
	protoAbsent bool // true when nil slot / missing key verified absent in __index / __newindex at epoch
	polyCount   uint8
}

// polyAdd inserts the displaced primary entry into the 4-way polymorphic cache.
func (ic *ICEntry) polySavePrimary() {
	if ic.kind != icOwnSlot && ic.kind != icProtoVal && ic.kind != icAddSlot {
		return
	}
	if ic.poly == nil {
		ic.poly = new([4]polyEntry)
	}
	// Update existing shape entry if already present.
	for i := range int(ic.polyCount) {
		if ic.poly[i].shape == ic.shape {
			ic.poly[i] = polyEntry{
				shape: ic.shape, targetShape: ic.targetShape, val: ic.val,
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
		shape: ic.shape, targetShape: ic.targetShape, val: ic.val,
		epoch: ic.epoch, slot: ic.slot, kind: ic.kind,
		noMeta: ic.noMeta, protoAbsent: ic.protoAbsent,
	}
}

// polyLookup searches the 4-way poly array for shape sh; on hit, swaps it into the primary MRU slot.
func (ic *ICEntry) polyLookup(sh *Shape) bool {
	if ic.poly == nil {
		return false
	}
	for i := range int(ic.polyCount) {
		pe := &ic.poly[i]
		if pe.shape == sh {
			// Swap primary <-> pe (MRU promotion).
			old := polyEntry{
				shape: ic.shape, targetShape: ic.targetShape, val: ic.val,
				epoch: ic.epoch, slot: ic.slot, kind: ic.kind,
				noMeta: ic.noMeta, protoAbsent: ic.protoAbsent,
			}
			ic.shape = pe.shape
			ic.targetShape = pe.targetShape
			ic.val = pe.val
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
