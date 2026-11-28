package arena

import "slices"

// GoHandle is a generation-tagged 64-bit handle stored inside noscan Arena objects
// to reference Go heap values (userdata payloads, Go native callbacks, open file handles)
// without placing any Go pointers inside the noscan []byte Arena.
//
// Encoding:
//   - NullGoHandle (0): invalid/empty handle.
//   - Low 32 bits:  1-based slot index inside GoRefs.slots.
//   - High 32 bits: non-zero generation counter preventing ABA / stale-handle access after Free.
type GoHandle uint64

// NullGoHandle represents the absence of a Go host reference.
const NullGoHandle GoHandle = 0

// IsNull reports whether h is NullGoHandle.
func (h GoHandle) IsNull() bool { return h == NullGoHandle }

func makeGoHandle(idx, gen uint32) GoHandle {
	return GoHandle((uint64(gen) << 32) | uint64(idx))
}

func splitGoHandle(h GoHandle) (idx, gen uint32) {
	return uint32(h), uint32(h >> 32)
}

type goRefSlot struct {
	val      any
	gen      uint32
	nextFree uint32
	live     bool
}

// GoRefs is the per-State boundary table keeping Go values strongly reachable from Go's GC
// for exactly as long as the Lua State's Arena GC considers their owning Lua objects live.
//
// When Lua's Arena GC sweeps a userdata or native closure (or when State.Close() calls
// ClearAll), Free clears slot.val = nil so Go's GC can reclaim the Go value immediately,
// and advances slot.gen so any stale GoHandle fails lookup deterministically.
type GoRefs struct {
	slots     []goRefSlot // 1-based (slots[0] unused sentinel)
	freeHead  uint32
	liveCount int
}

// NewGoRefs creates an empty generational Go handle table.
func NewGoRefs() *GoRefs {
	return &GoRefs{
		slots: make([]goRefSlot, 1, 16),
	}
}

// Alloc pins v in the handle table and returns a unique generation-tagged GoHandle.
// Even a nil interface value is permitted (for userdata with nil Go payload) and remains
// distinguishable from a freed/invalid handle until Free is called.
func (g *GoRefs) Alloc(v any) GoHandle {
	if len(g.slots) == 0 {
		g.slots = make([]goRefSlot, 1, 16)
	}
	var idx uint32
	if g.freeHead != 0 {
		idx = g.freeHead
		slot := &g.slots[idx]
		g.freeHead = slot.nextFree
		slot.nextFree = 0
		slot.val = v
		slot.live = true
		g.liveCount++
		return makeGoHandle(idx, slot.gen)
	}
	idx = uint32(len(g.slots))
	g.slots = append(g.slots, goRefSlot{
		val:  v,
		gen:  1,
		live: true,
	})
	g.liveCount++
	return makeGoHandle(idx, 1)
}

// Get retrieves the Go value pinned under h, or (nil, false) if h is null, stale, or freed.
func (g *GoRefs) Get(h GoHandle) (any, bool) {
	if g == nil || h == NullGoHandle {
		return nil, false
	}
	idx, gen := splitGoHandle(h)
	if idx == 0 || int(idx) >= len(g.slots) {
		return nil, false
	}
	slot := &g.slots[idx]
	if !slot.live || slot.gen != gen {
		return nil, false
	}
	return slot.val, true
}

// GetAs retrieves the Go value pinned under h if h is live and its stored value
// is non-nil and assignable to T.
func (g *GoRefs) GetAs[T any](h GoHandle) (T, bool) {
	var zero T
	raw, ok := g.Get(h)
	if !ok {
		return zero, false
	}
	v, ok := raw.(T)
	return v, ok
}

// Set replaces the Go value pinned under an active handle h.
// Returns false if h is null, stale, or freed.
func (g *GoRefs) Set(h GoHandle, v any) bool {
	if g == nil || h == NullGoHandle {
		return false
	}
	idx, gen := splitGoHandle(h)
	if idx == 0 || int(idx) >= len(g.slots) {
		return false
	}
	slot := &g.slots[idx]
	if !slot.live || slot.gen != gen {
		return false
	}
	slot.val = v
	return true
}

// Free releases h, clearing its Go reference immediately so Go's GC can reclaim the value,
// and increments the slot generation to invalidate any stale copy of h.
// Returns false if h is null, stale, or already freed (safe against double-free).
func (g *GoRefs) Free(h GoHandle) bool {
	if g == nil || h == NullGoHandle {
		return false
	}
	idx, gen := splitGoHandle(h)
	if idx == 0 || int(idx) >= len(g.slots) {
		return false
	}
	slot := &g.slots[idx]
	if !slot.live || slot.gen != gen {
		return false
	}
	slot.val = nil
	slot.live = false
	slot.gen++
	if slot.gen == 0 {
		slot.gen = 1 // never allow generation 0
	}
	slot.nextFree = g.freeHead
	g.freeHead = idx
	g.liveCount--
	return true
}

// LiveCount returns the number of currently pinned Go handles.
func (g *GoRefs) LiveCount() int {
	if g == nil {
		return 0
	}
	return g.liveCount
}

// Clone returns an independent copy of g preserving all active GoHandle indices and
// generations (used by Template.Clone so cloned States share immutable Go callbacks
// without sharing mutable handle-table state).
func (g *GoRefs) Clone() *GoRefs {
	return g.CloneWith(nil)
}

// CloneWith returns an independent copy of g preserving all active GoHandle indices and
// generations. When cloneVal is non-nil, it is invoked on each live slot's Go value to
// independently clone mutable built-in payloads.
func (g *GoRefs) CloneWith(cloneVal func(any) any) *GoRefs {
	if g == nil || len(g.slots) <= 1 {
		return NewGoRefs()
	}
	slots := slices.Clone(g.slots)
	if cloneVal != nil {
		for i := 1; i < len(slots); i++ {
			if slots[i].live {
				slots[i].val = cloneVal(slots[i].val)
			}
		}
	}
	return &GoRefs{
		slots:     slots,
		freeHead:  g.freeHead,
		liveCount: g.liveCount,
	}
}

// ClearAll releases every pinned Go value immediately (used on State.Close),
// advancing generations on all previously allocated slots so old handles remain invalid
// if the GoRefs instance is ever queried again.
func (g *GoRefs) ClearAll() {
	if g == nil || len(g.slots) <= 1 {
		return
	}
	g.freeHead = 0
	for i := len(g.slots) - 1; i >= 1; i-- {
		slot := &g.slots[i]
		slot.val = nil
		slot.live = false
		slot.gen++
		if slot.gen == 0 {
			slot.gen = 1
		}
		slot.nextFree = g.freeHead
		g.freeHead = uint32(i)
	}
	g.liveCount = 0
}
