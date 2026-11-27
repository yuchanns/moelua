package vm

import (
	"unsafe"

	"github.com/yuchanns/moelua/internal/arena"
)

const (
	maxShapeProps = 64

	shapeFlagTransMask         uint8 = 0x07 // bits 0..2: nTrans (0..4)
	shapeFlagTracked           uint8 = 1 << 3
	shapeFlagMetaProxy         uint8 = 1 << 4 // metaRef is the __index target proto table, not the wrapper metatable
	shapeFlagMetaProxyPoisoned uint8 = 1 << 5 // proxy shape invalidated by in-place mutation of a proxy metatable
)

type shapeTrans struct {
	keyID    uint64       // 8B: LuaString.ID
	childRef arena.ObjRef // 4B: Shape ObjRef in State.arena (weak edge pruned in PreSweep)
	_        uint32       // 4B
}

type shapeIndexEntry struct {
	keyRef arena.ObjRef // 4B: LuaString ObjRef
	keyTag uint8        // 1B: tagFrozenString or tagString
	slot   int8         // 1B: slot index (0..63), -1 = empty
	_      uint16       // 2B pad -> 8B exact
}

const (
	_ = uint(8 - unsafe.Sizeof(shapeIndexEntry{}))
	_ = uint(unsafe.Sizeof(shapeIndexEntry{}) - 8)
)

// Shape is a 128-byte noscan hidden class stored in State.arena (ptrdata == 0).
// Metatable identity (metaRef) is encoded directly inside the Shape so a single
// shapeID comparison covers receiver layout, __index/__newindex, and metatable identity.
type Shape struct {
	hdr        arena.GCHeader // 8B: Kind=gcKindShape, Flags=nTrans|shapeFlagTracked
	id         uint64         // 8B: monotonic shape ID
	keyVal     Value          // 16B: short LuaString Value added by this transition (Nil on root)
	self       arena.ObjRef   // 4B: own ObjRef in State.arena
	parentRef  arena.ObjRef   // 4B: parent Shape ObjRef (NullRef on root)
	metaRef    arena.ObjRef   // 4B: metatable Table ObjRef (NullRef if none)
	transRef   arena.ObjRef   // 4B: optional raw Arena []shapeTrans overflow array
	indexRef   arena.ObjRef   // 4B: lazily-built compact Arena []shapeIndexEntry for >8 props
	slot       int16          // 2B: slot index for keyVal (-1 on root)
	length     int16          // 2B: total number of properties in this shape
	transLen   uint16         // 2B: live entries in transRef
	transCap   uint16         // 2B: capacity of transRef
	indexMask  uint8          // 1B: 0 if no index, else len(entries)-1 (31/63/127)
	lookupHits uint8          // 1B: saturated lookup count; builds index lazily on 2nd lookup
	_          uint16         // 2B pad
	transSmall [4]shapeTrans  // 64B: inline small transition cache (weak edges)
}

const (
	_ = uint(128 - unsafe.Sizeof(Shape{}))
	_ = uint(unsafe.Sizeof(Shape{}) - 128)
)

func (sh *Shape) nTrans() int {
	return int(sh.hdr.Flags & shapeFlagTransMask)
}

func (sh *Shape) setNTrans(n int) {
	sh.hdr.Flags = (sh.hdr.Flags &^ shapeFlagTransMask) | (uint8(n) & shapeFlagTransMask)
}

func (sh *Shape) isTracked() bool {
	return sh.hdr.Flags&shapeFlagTracked != 0
}

func (sh *Shape) markTracked() {
	sh.hdr.Flags |= shapeFlagTracked
}

func (sh *Shape) isMetaProxy() bool {
	return sh.hdr.Flags&shapeFlagMetaProxy != 0
}

func (sh *Shape) isMetaProxyPoisoned() bool {
	return sh.hdr.Flags&shapeFlagMetaProxyPoisoned != 0
}

func shapeIndexHash(v Value, mask uint32) uint32 {
	x := v.bits ^ (v.tag * 0x9e3779b97f4a7c15)
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	return uint32(x^(x>>33)) & mask
}

func (sh *Shape) buildIndexLazy(s *State) {
	if sh.indexRef != arena.NullRef || sh.length <= 8 || s == nil || s.arena == nil {
		return
	}
	sz := 32
	for sz < int(sh.length)*2 {
		sz <<= 1
	}
	byteLen := sz * int(unsafe.Sizeof(shapeIndexEntry{}))
	if s.memLimit > 0 {
		_, slotBytes := arena.SizeClassFor(byteLen)
		if s.MemoryUsage()+int64(slotBytes) > s.memLimit {
			return
		}
	}
	idxRef, err := s.arena.Alloc(byteLen)
	if err != nil {
		return
	}
	if s.collector != nil {
		s.collector.AddDebt(int64(s.arena.SlotSize(idxRef)))
	}
	mask := uint32(sz - 1)
	entries := arena.SliceFast[shapeIndexEntry](s.arena, idxRef, 0, sz)
	for i := range entries {
		entries[i].slot = -1
	}
	a := s.arena
	for cur := sh; cur != nil && !cur.keyVal.IsNil(); {
		pos := shapeIndexHash(cur.keyVal, mask)
		for entries[pos].slot >= 0 {
			pos = (pos + 1) & mask
		}
		entries[pos] = shapeIndexEntry{
			keyRef: arena.ObjRef(cur.keyVal.bits),
			keyTag: uint8(cur.keyVal.tag),
			slot:   int8(cur.slot),
		}
		if cur.parentRef == arena.NullRef {
			break
		}
		cur = arena.AsSmall[Shape](a, cur.parentRef)
	}
	sh.indexRef = idxRef
	sh.indexMask = uint8(mask)
}

// Lookup returns the slot index for short-string key k, or -1 if absent.
func (sh *Shape) Lookup(s *State, k *LuaString) int32 {
	if sh == nil || sh.length == 0 || k == nil {
		return -1
	}
	kv := StringValue(k)
	if sh.indexRef != arena.NullRef {
		mask := uint32(sh.indexMask)
		entries := arena.SliceFast[shapeIndexEntry](s.arena, sh.indexRef, 0, int(mask)+1)
		idx := shapeIndexHash(kv, mask)
		kRef := arena.ObjRef(kv.bits)
		kTag := uint8(kv.tag)
		for {
			e := &entries[idx]
			if e.slot < 0 {
				return -1
			}
			if e.keyRef == kRef && e.keyTag == kTag {
				return int32(e.slot)
			}
			idx = (idx + 1) & mask
		}
	}
	if sh.length > 8 {
		if sh.lookupHits < 255 {
			sh.lookupHits++
		}
		if sh.lookupHits == 2 {
			sh.buildIndexLazy(s)
			if sh.indexRef != arena.NullRef {
				return sh.Lookup(s, k)
			}
		}
	}
	a := s.arena
	for cur := sh; cur != nil && !cur.keyVal.IsNil(); {
		if cur.keyVal == kv {
			return int32(cur.slot)
		}
		if cur.parentRef == arena.NullRef {
			break
		}
		cur = arena.AsSmall[Shape](a, cur.parentRef)
	}
	return -1
}

// Keys returns the ordered short-string Values of this shape (slot 0..length-1).
func (sh *Shape) Keys(a *arena.Arena) []Value {
	if sh == nil || sh.length == 0 {
		return nil
	}
	out := make([]Value, sh.length)
	for cur := sh; cur != nil && !cur.keyVal.IsNil(); {
		out[cur.slot] = cur.keyVal
		if cur.parentRef == arena.NullRef {
			break
		}
		cur = arena.AsSmall[Shape](a, cur.parentRef)
	}
	return out
}

type metaRootKey struct {
	metaRef   arena.ObjRef
	metaProxy bool
}

type metaTransKey struct {
	shapeRef  arena.ObjRef
	metaRef   arena.ObjRef
	metaProxy bool
}

type shapeRegistry struct {
	nextID      uint64
	rootRef     arena.ObjRef
	metaRoots   map[metaRootKey]arena.ObjRef
	metaTrans   map[metaTransKey]arena.ObjRef
	transShapes []arena.ObjRef // shapes with >= 1 transition; pruned in GC PreSweep
	proxyShapes []arena.ObjRef // canonical __index-proxy shapes; cleared on poison or GC PreSweep
}

func (sr *shapeRegistry) init(s *State) {
	sr.nextID = 1
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(Shape{})), gcKindShape)
	if err != nil {
		panic(err)
	}
	sh := arena.AsSmall[Shape](s.arena, ref)
	sh.id = sr.nextID
	sh.self = ref
	sh.keyVal = NilValue()
	sh.slot = -1
	sr.rootRef = ref
}

func (sr *shapeRegistry) trackTrans(sh *Shape) {
	if !sh.isTracked() {
		sh.markTracked()
		sr.transShapes = append(sr.transShapes, sh.self)
	}
}

// pruneDead removes all weak shape cache entries referencing White (soon-to-be-freed)
// shapes or metatables during GC PreSweep (zero weak.Pointer overhead!).
func (sr *shapeRegistry) pruneDead(a *arena.Arena, c *arena.Collector) {
	for k, shRef := range sr.metaRoots {
		if c.IsWhite(k.metaRef) || c.IsWhite(shRef) {
			delete(sr.metaRoots, k)
		}
	}
	for k, shRef := range sr.metaTrans {
		if c.IsWhite(k.shapeRef) || (k.metaRef != arena.NullRef && c.IsWhite(k.metaRef)) || c.IsWhite(shRef) {
			delete(sr.metaTrans, k)
		}
	}
	kept := 0
	for _, shRef := range sr.transShapes {
		if c.IsWhite(shRef) {
			continue
		}
		sh := arena.AsSmall[Shape](a, shRef)
		n := sh.nTrans()
		kn := 0
		for i := range n {
			st := sh.transSmall[i]
			if st.childRef != arena.NullRef && !c.IsWhite(st.childRef) {
				sh.transSmall[kn] = st
				kn++
			}
		}
		for i := kn; i < n; i++ {
			sh.transSmall[i] = shapeTrans{}
		}
		sh.setNTrans(kn)
		if sh.transRef != arena.NullRef && sh.transLen > 0 {
			arr := arena.SliceFast[shapeTrans](a, sh.transRef, 0, int(sh.transLen))
			kl := 0
			for i := range int(sh.transLen) {
				st := arr[i]
				if st.childRef != arena.NullRef && !c.IsWhite(st.childRef) {
					arr[kl] = st
					kl++
				}
			}
			for i := kl; i < int(sh.transLen); i++ {
				arr[i] = shapeTrans{}
			}
			sh.transLen = uint16(kl)
			if kl == 0 {
				a.Free(sh.transRef)
				sh.transRef = arena.NullRef
				sh.transCap = 0
			}
		}
		if kn == 0 && sh.transLen == 0 {
			sh.hdr.Flags &^= shapeFlagTracked
			continue
		}
		sr.transShapes[kept] = shRef
		kept++
	}
	sr.transShapes = sr.transShapes[:kept]
	keptProxy := 0
	for _, shRef := range sr.proxyShapes {
		if c.IsWhite(shRef) {
			continue
		}
		sh := arena.AsSmall[Shape](a, shRef)
		if sh.metaRef == arena.NullRef || c.IsWhite(sh.metaRef) {
			sh.hdr.Flags |= shapeFlagMetaProxyPoisoned
			sh.metaRef = arena.NullRef
			continue
		}
		sr.proxyShapes[keptProxy] = shRef
		keptProxy++
	}
	sr.proxyShapes = sr.proxyShapes[:keptProxy]
}

// poisonProxyProto releases strong/weak proxy-shape references to protoRef immediately
// when any wrapper metatable proxying protoRef is mutated in place.
func (sr *shapeRegistry) poisonProxyProto(a *arena.Arena, protoRef arena.ObjRef) {
	delete(sr.metaRoots, metaRootKey{metaRef: protoRef, metaProxy: true})
	for k := range sr.metaTrans {
		if k.metaProxy && k.metaRef == protoRef {
			delete(sr.metaTrans, k)
		}
	}
	kept := 0
	for _, shRef := range sr.proxyShapes {
		sh := arena.AsSmall[Shape](a, shRef)
		if sh.metaRef == protoRef {
			sh.hdr.Flags |= shapeFlagMetaProxyPoisoned
			sh.metaRef = arena.NullRef
			continue
		}
		sr.proxyShapes[kept] = shRef
		kept++
	}
	sr.proxyShapes = sr.proxyShapes[:kept]
}

func (sr *shapeRegistry) allocShape(s *State, parentRef, metaRef arena.ObjRef, proxyFlags uint8, keyVal Value, slot, length int16) (*Shape, error) {
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(Shape{})), gcKindShape)
	if err != nil {
		return nil, err
	}
	sr.nextID++
	sh := arena.AsSmall[Shape](s.arena, ref)
	sh.id = sr.nextID
	sh.self = ref
	sh.parentRef = parentRef
	sh.metaRef = metaRef
	sh.hdr.Flags |= proxyFlags & (shapeFlagMetaProxy | shapeFlagMetaProxyPoisoned)
	sh.keyVal = keyVal
	sh.slot = slot
	sh.length = length
	if proxyFlags&shapeFlagMetaProxy != 0 && proxyFlags&shapeFlagMetaProxyPoisoned == 0 && metaRef != arena.NullRef {
		sr.proxyShapes = append(sr.proxyShapes, ref)
	}
	return sh, nil
}

func (sr *shapeRegistry) Root(s *State, metaRef arena.ObjRef, metaProxy bool, rootObj, metaRoot Value) *Shape {
	if metaRef == arena.NullRef {
		return arena.AsSmall[Shape](s.arena, sr.rootRef)
	}
	rk := metaRootKey{metaRef: metaRef, metaProxy: metaProxy}
	if sr.metaRoots == nil {
		sr.metaRoots = make(map[metaRootKey]arena.ObjRef, 16)
	} else if shRef, ok := sr.metaRoots[rk]; ok {
		return arena.AsSmall[Shape](s.arena, shRef)
	}
	if s.memLimit > 0 {
		if err := s.checkMemWithRoots(128, rootObj, metaRoot, NilValue()); err != nil {
			s.memExceeded = true
			return nil
		}
	}
	var proxyFlags uint8
	if metaProxy {
		proxyFlags = shapeFlagMetaProxy
	}
	sh, err := sr.allocShape(s, arena.NullRef, metaRef, proxyFlags, NilValue(), -1, 0)
	if err != nil {
		s.memExceeded = true
		return nil
	}
	sr.metaRoots[rk] = sh.self
	return sh
}

const (
	// maxShapeTransCap caps per-shape overflow transition cache entries so uint16
	// transLen/transCap never overflow and linear transition scans stay bounded.
	maxShapeTransCap = 1024
)

// Transition returns the child shape adding key k (assumed absent from sh), or (nil, nil) if maxShapeProps exceeded.
func (sr *shapeRegistry) Transition(s *State, shRef arena.ObjRef, k *LuaString, r1, r2 Value) (*Shape, error) {
	sh := arena.AsSmall[Shape](s.arena, shRef)
	if sh.length >= maxShapeProps {
		return nil, nil
	}
	kid := k.ID
	n := sh.nTrans()
	for i := range n {
		if sh.transSmall[i].keyID == kid && sh.transSmall[i].childRef != arena.NullRef {
			return arena.AsSmall[Shape](s.arena, sh.transSmall[i].childRef), nil
		}
	}
	if sh.transRef != arena.NullRef && sh.transLen > 0 {
		arr := arena.SliceFast[shapeTrans](s.arena, sh.transRef, 0, int(sh.transLen))
		for i := range arr {
			if arr[i].keyID == kid && arr[i].childRef != arena.NullRef {
				return arena.AsSmall[Shape](s.arena, arr[i].childRef), nil
			}
		}
	}
	if s.memLimit > 0 {
		est := arenaAllocSize(int64(unsafe.Sizeof(Shape{})))
		if n >= 4 && int(sh.transLen) >= int(sh.transCap) && int(sh.transCap) < maxShapeTransCap {
			newCap := min(max(int(sh.transCap)*2, 8), maxShapeTransCap)
			_, newSlot := arena.SizeClassFor(newCap * int(unsafe.Sizeof(shapeTrans{})))
			var oldSlot int
			if sh.transRef != arena.NullRef {
				oldSlot = s.arena.SlotSize(sh.transRef)
			}
			if newSlot > oldSlot {
				est += int64(newSlot - oldSlot)
			}
		}
		if err := s.checkMemWithRoots(est, r1, StringValue(k), r2); err != nil {
			return nil, err
		}
	}
	// Reload sh pointer after potential emergency GC (Arena is non-moving, sh stays valid).
	sh = arena.AsSmall[Shape](s.arena, shRef)
	n = sh.nTrans()
	child, err := sr.allocShape(s, sh.self, sh.metaRef, sh.hdr.Flags&(shapeFlagMetaProxy|shapeFlagMetaProxyPoisoned), StringValue(k), sh.length, sh.length+1)
	if err != nil {
		return nil, err
	}
	if n < 4 {
		sh.transSmall[n] = shapeTrans{keyID: kid, childRef: child.self}
		sh.setNTrans(n + 1)
	} else {
		if int(sh.transLen) >= int(sh.transCap) {
			if int(sh.transCap) >= maxShapeTransCap {
				return child, nil // transition cache full; child shape remains valid without caching
			}
			newCap := min(max(int(sh.transCap)*2, 8), maxShapeTransCap)
			byteLen := newCap * int(unsafe.Sizeof(shapeTrans{}))
			if s.memLimit > 0 {
				_, newSlot := arena.SizeClassFor(byteLen)
				var oldSlot int
				if sh.transRef != arena.NullRef {
					oldSlot = s.arena.SlotSize(sh.transRef)
				}
				if newSlot > oldSlot && s.MemoryUsage()+int64(newSlot-oldSlot) > s.memLimit {
					return child, nil // child shape is still valid even without caching transition
				}
			}
			newRef, aerr := s.arena.Alloc(byteLen)
			if aerr != nil {
				return child, nil // child shape is still valid even without caching transition
			}
			s.collector.AddDebt(int64(s.arena.SlotSize(newRef)))
			dst := arena.SliceFast[shapeTrans](s.arena, newRef, 0, newCap)
			if sh.transRef != arena.NullRef {
				src := arena.SliceFast[shapeTrans](s.arena, sh.transRef, 0, int(sh.transLen))
				copy(dst, src)
				s.arena.Free(sh.transRef)
			}
			sh.transRef = newRef
			sh.transCap = uint16(newCap)
		}
		arr := arena.SliceFast[shapeTrans](s.arena, sh.transRef, 0, int(sh.transCap))
		arr[sh.transLen] = shapeTrans{keyID: kid, childRef: child.self}
		sh.transLen++
	}
	sr.trackTrans(sh)
	return child, nil
}

// WithMeta returns a shape having the exact same ordered keys as shRef, but with metatable
// (or canonical __index proxy prototype when metaProxy is true) metaRef.
// metaRoot must be the live metatable Value so emergency GC keeps both the wrapper and its
// target prototype rooted across shape allocation.
func (sr *shapeRegistry) WithMeta(s *State, shRef, metaRef arena.ObjRef, metaProxy bool, rootObj, metaRoot Value) (arena.ObjRef, bool) {
	if shRef == arena.NullRef {
		r := sr.Root(s, metaRef, metaProxy, rootObj, metaRoot)
		if r == nil {
			return arena.NullRef, false
		}
		return r.self, true
	}
	sh := arena.AsSmall[Shape](s.arena, shRef)
	if sh.metaRef == metaRef && sh.isMetaProxy() == metaProxy {
		return shRef, true
	}
	if sh.length == 0 {
		if r := sr.Root(s, metaRef, metaProxy, rootObj, metaRoot); r != nil {
			return r.self, true
		}
		s.memExceeded = true
		return shRef, false
	}
	key := metaTransKey{shapeRef: shRef, metaRef: metaRef, metaProxy: metaProxy}
	if sr.metaTrans != nil {
		if cached, ok := sr.metaTrans[key]; ok {
			return cached, true
		}
	} else {
		sr.metaTrans = make(map[metaTransKey]arena.ObjRef, 16)
	}
	cur := sr.Root(s, metaRef, metaProxy, rootObj, metaRoot)
	if cur == nil {
		s.memExceeded = true
		return shRef, false
	}
	curRef := cur.self
	keys := sh.Keys(s.arena)
	for _, kv := range keys {
		prevPinned := s.pinnedShapeRef
		s.pinnedShapeRef = curRef
		next, err := sr.Transition(s, curRef, s.asString(kv), rootObj, metaRoot)
		s.pinnedShapeRef = prevPinned
		if err != nil || next == nil {
			s.memExceeded = true
			return shRef, false
		}
		curRef = next.self
	}
	sr.metaTrans[key] = curRef
	return curRef, true
}

// icKind identifies what a per-PC inline cache entry caches.
type icKind uint8

const (
	icEmpty icKind = iota
	icOwnSlot      // receiver.shape.id == shapeID => receiver.slots[slot]
	icProtoVal     // receiver.shape.id == shapeID && (noMeta || s.protoEpoch == epoch) => ic.val
	icAddSlot      // SETFIELD transition: receiver.shape.id == shapeID && (noMeta || s.protoEpoch == epoch) => targetShapeRef
	icEnvSlot      // GETTABUP on dict table: holderRef == t.self && holder.hash.dictGen == epoch => holder.nodes[slot].val
)

// polyEntry is a 40-byte noscan slot in a lazily-allocated 4-way polymorphic IC in State.arena.
// All cached references are weak Arena ObjRefs/Values swept in Collector PreSweep (zero weak.Pointer!).
type polyEntry struct {
	val            Value
	epoch          uint64
	shapeRef       arena.ObjRef
	targetShapeRef arena.ObjRef
	slot           int32
	kind           icKind
	noMeta         bool
	protoAbsent    bool
	_              byte
}

const (
	_ = uint(40 - unsafe.Sizeof(polyEntry{}))
	_ = uint(unsafe.Sizeof(polyEntry{}) - 40)
)

// ICEntry is a 48-byte noscan per-State, per-PC inline cache entry stored in State.arena.
// Monomorphic sites hit directly on t.shapeRef == ic.shapeRef with ZERO Shape deref and ZERO
// weak.Pointer barriers; polymorphic sites (2..4 shapes) lazily allocate a 160B Arena [4]polyEntry array.
type ICEntry struct {
	val            Value        // for icProtoVal (stored directly — noscan!)
	epoch          uint64       // protoEpoch or dictGen
	shapeRef       arena.ObjRef // receiver Shape ObjRef (swept in PreSweep before Shape reuse)
	targetShapeRef arena.ObjRef // for icAddSlot
	holderRef      arena.ObjRef // for icEnvSlot
	polyRef        arena.ObjRef // optional Arena [4]polyEntry
	slot           int32
	kind           icKind
	noMeta         bool
	protoAbsent    bool
	polyCount      uint8
}

const (
	_ = uint(48 - unsafe.Sizeof(ICEntry{}))
	_ = uint(unsafe.Sizeof(ICEntry{}) - 48)
)

func (ic *ICEntry) getTabUpFast(a *arena.Arena, t *Table) (Value, bool) {
	if ic.kind == icEnvSlot && ic.holderRef == t.self && t.hashRef != arena.NullRef {
		h := arena.AsSmall[tableHash](a, t.hashRef)
		if ic.epoch == h.dictGen && h.nodesRef != arena.NullRef {
			v := (*hashNode)(unsafe.Add(a.PtrFast(h.nodesRef), uintptr(ic.slot)*unsafe.Sizeof(hashNode{}))).val
			if !v.IsNil() || ic.noMeta {
				return v, true
			}
		}
		return NilValue(), false
	}
	if ic.kind == icOwnSlot && t.shapeRef == ic.shapeRef {
		v := t.slotAt(a, int(ic.slot))
		if !v.IsNil() || ic.noMeta {
			return v, true
		}
	}
	return NilValue(), false
}

func (ic *ICEntry) setTabUpFast(s *State, t *Table, val Value) bool {
	if ic.kind == icEnvSlot && ic.holderRef == t.self && !t.isProto() && !val.IsNil() && t.hashRef != arena.NullRef {
		h := arena.AsSmall[tableHash](s.arena, t.hashRef)
		if ic.epoch == h.dictGen && h.nodesRef != arena.NullRef {
			nd := (*hashNode)(unsafe.Add(s.arena.PtrFast(h.nodesRef), uintptr(ic.slot)*unsafe.Sizeof(hashNode{})))
			if !nd.val.IsNil() {
				nd.val = val
				if t.tmFlags != 0 {
					t.tmFlags = 0
				}
				if val.IsGCObject() && t.hdr.IsBlack() {
					s.barrierBackRef(t.self)
				}
				return true
			}
		}
	}
	return false
}

func (ic *ICEntry) getFieldFast(a *arena.Arena, t *Table, protoEpoch uint64) (Value, bool) {
	if t.shapeRef == ic.shapeRef {
		if ic.kind == icOwnSlot {
			v := t.slotAt(a, int(ic.slot))
			if !v.IsNil() || ic.noMeta || (ic.protoAbsent && protoEpoch == ic.epoch) {
				return v, true
			}
		} else if ic.kind == icProtoVal && (ic.noMeta || protoEpoch == ic.epoch) {
			return ic.val, true
		}
	}
	if ic.polyRef != arena.NullRef {
		return ic.getFieldPoly(a, t, protoEpoch)
	}
	return NilValue(), false
}

//go:noinline
func (ic *ICEntry) getFieldPoly(a *arena.Arena, t *Table, protoEpoch uint64) (Value, bool) {
	poly := (*[4]polyEntry)(a.PtrSmall(ic.polyRef))
	shRef := t.shapeRef
	for i := range int(ic.polyCount) {
		pe := &poly[i]
		if pe.shapeRef == shRef {
			if pe.kind == icOwnSlot {
				v := t.slotAt(a, int(pe.slot))
				if !v.IsNil() || pe.noMeta || (pe.protoAbsent && protoEpoch == pe.epoch) {
					return v, true
				}
			} else if pe.kind == icProtoVal && (pe.noMeta || protoEpoch == pe.epoch) {
				return pe.val, true
			}
			return NilValue(), false
		}
	}
	return NilValue(), false
}

func (ic *ICEntry) setFieldFast(s *State, t *Table, val Value, protoEpoch uint64) bool {
	if t.hdr.Flags&tblFlagIsProto == 0 {
		if t.shapeRef == ic.shapeRef {
			if ic.kind == icOwnSlot {
				sp := t.slotPtr(s.arena, int(ic.slot))
				if !sp.IsNil() || ic.noMeta || (ic.protoAbsent && protoEpoch == ic.epoch) {
					*sp = val
					if val.IsGCObject() && t.hdr.IsBlack() {
						s.barrierBackRef(t.self)
					}
					return true
				}
				return false
			}
			if ic.kind == icAddSlot {
				return ic.setAddSlotFast(s, t, val, protoEpoch)
			}
		} else if ic.polyRef != arena.NullRef {
			return ic.setFieldPoly(s, t, val, protoEpoch)
		}
	}
	return false
}

//go:noinline
func (ic *ICEntry) setAddSlotFast(s *State, t *Table, val Value, protoEpoch uint64) bool {
	if !val.IsNil() && (ic.noMeta || protoEpoch == ic.epoch) {
		slot := int(ic.slot)
		if ic.targetShapeRef != arena.NullRef && slot < int(t.slotsCap) {
			t.slotsLen = uint8(slot + 1)
			*t.slotPtr(s.arena, slot) = val
			t.shapeRef = ic.targetShapeRef
			if t.hdr.IsBlack() {
				s.barrierBackRef(t.self)
			}
			return true
		}
	}
	return false
}

//go:noinline
func (ic *ICEntry) setFieldPoly(s *State, t *Table, val Value, protoEpoch uint64) bool {
	a := s.arena
	poly := (*[4]polyEntry)(a.PtrSmall(ic.polyRef))
	shRef := t.shapeRef
	for i := range int(ic.polyCount) {
		pe := &poly[i]
		if pe.shapeRef == shRef {
			if pe.kind == icOwnSlot {
				sp := t.slotPtr(a, int(pe.slot))
				if !sp.IsNil() || pe.noMeta || (pe.protoAbsent && protoEpoch == pe.epoch) {
					*sp = val
					if val.IsGCObject() && t.hdr.IsBlack() {
						s.barrierBackRef(t.self)
					}
					return true
				}
			} else if pe.kind == icAddSlot && !val.IsNil() && (pe.noMeta || protoEpoch == pe.epoch) {
				slot := int(pe.slot)
				if pe.targetShapeRef != arena.NullRef && slot < int(t.slotsCap) {
					t.slotsLen = uint8(slot + 1)
					*t.slotPtr(a, slot) = val
					t.shapeRef = pe.targetShapeRef
					if t.hdr.IsBlack() {
						s.barrierBackRef(t.self)
					}
					return true
				}
			}
			return false
		}
	}
	return false
}

func (ic *ICEntry) setOwnSlot(shapeRef arena.ObjRef, slot int32, noMeta, protoAbsent bool, epoch uint64) {
	ic.kind = icOwnSlot
	ic.shapeRef = shapeRef
	ic.targetShapeRef = arena.NullRef
	ic.holderRef = arena.NullRef
	ic.val = NilValue()
	ic.slot = slot
	ic.noMeta = noMeta
	ic.protoAbsent = protoAbsent
	ic.epoch = epoch
}

func (ic *ICEntry) setProtoVal(shapeRef arena.ObjRef, v Value, epoch uint64, noMeta bool) {
	ic.kind = icProtoVal
	ic.shapeRef = shapeRef
	ic.targetShapeRef = arena.NullRef
	ic.holderRef = arena.NullRef
	ic.val = v
	ic.slot = 0
	ic.noMeta = noMeta
	ic.protoAbsent = false
	ic.epoch = epoch
}

func (ic *ICEntry) setAddSlot(shapeRef, targetRef arena.ObjRef, slot int32, epoch uint64, noMeta bool) {
	ic.kind = icAddSlot
	ic.shapeRef = shapeRef
	ic.targetShapeRef = targetRef
	ic.holderRef = arena.NullRef
	ic.val = NilValue()
	ic.slot = slot
	ic.epoch = epoch
	ic.noMeta = noMeta
	ic.protoAbsent = false
}

func (ic *ICEntry) setEnvSlot(a *arena.Arena, holderRef arena.ObjRef, dictGen uint64, slot int32, noMeta bool) {
	if ic.polyRef != arena.NullRef {
		a.Free(ic.polyRef)
	}
	*ic = ICEntry{
		kind:      icEnvSlot,
		holderRef: holderRef,
		epoch:     dictGen,
		slot:      slot,
		noMeta:    noMeta,
	}
}

// polySavePrimary inserts the displaced primary entry into the 4-way polymorphic cache in Arena.
func (ic *ICEntry) polySavePrimary(s *State) {
	if ic.kind != icOwnSlot && ic.kind != icProtoVal && ic.kind != icAddSlot {
		return
	}
	a := s.arena
	if ic.polyRef == arena.NullRef {
		byteLen := 4 * int(unsafe.Sizeof(polyEntry{}))
		if s.memLimit > 0 {
			_, slotBytes := arena.SizeClassFor(byteLen)
			if s.MemoryUsage()+int64(slotBytes) > s.memLimit {
				return
			}
		}
		ref, err := a.AllocZeroed(byteLen)
		if err != nil {
			return
		}
		if s.collector != nil {
			s.collector.AddDebt(int64(a.SlotSize(ref)))
		}
		ic.polyRef = ref
	}
	poly := arena.SliceFast[polyEntry](a, ic.polyRef, 0, 4)
	for i := range int(ic.polyCount) {
		if poly[i].shapeRef == ic.shapeRef {
			poly[i] = polyEntry{
				shapeRef: ic.shapeRef, targetShapeRef: ic.targetShapeRef,
				val: ic.val, epoch: ic.epoch, slot: ic.slot, kind: ic.kind,
				noMeta: ic.noMeta, protoAbsent: ic.protoAbsent,
			}
			return
		}
	}
	idx := int(ic.polyCount)
	if idx >= 4 {
		idx = 3
	} else {
		ic.polyCount++
	}
	poly[idx] = polyEntry{
		shapeRef: ic.shapeRef, targetShapeRef: ic.targetShapeRef,
		val: ic.val, epoch: ic.epoch, slot: ic.slot, kind: ic.kind,
		noMeta: ic.noMeta, protoAbsent: ic.protoAbsent,
	}
}

// polyLookup searches the 4-way poly array for shapeRef; on hit, swaps it into the primary MRU slot.
func (ic *ICEntry) polyLookup(a *arena.Arena, shapeRef arena.ObjRef) bool {
	if ic.polyRef == arena.NullRef {
		return false
	}
	poly := (*[4]polyEntry)(a.PtrSmall(ic.polyRef))
	for i := range int(ic.polyCount) {
		pe := &poly[i]
		if pe.shapeRef == shapeRef {
			old := polyEntry{
				shapeRef: ic.shapeRef, targetShapeRef: ic.targetShapeRef,
				val: ic.val, epoch: ic.epoch, slot: ic.slot, kind: ic.kind,
				noMeta: ic.noMeta, protoAbsent: ic.protoAbsent,
			}
			ic.shapeRef = pe.shapeRef
			ic.targetShapeRef = pe.targetShapeRef
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
