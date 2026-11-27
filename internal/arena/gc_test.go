package arena_test

import (
	"math/rand/v2"
	"runtime"
	"slices"
	"testing"
	"time"
	"unsafe"
	"weak"

	"github.com/yuchanns/moelua/internal/arena"
)

const (
	toyKindTable uint8 = 1
	toyKindStr   uint8 = 2
	toyKindUD    uint8 = 3
	toyKindTh    uint8 = 4
)

type toyEntry struct {
	key arena.ObjRef
	val arena.ObjRef
}

type toyTable struct {
	hdr     arena.GCHeader
	id      uint32
	weak    arena.WeakKind
	len     uint32
	entries [8]toyEntry
}

type toyString struct {
	hdr arena.GCHeader
	id  uint32
}

type toyUserdata struct {
	hdr    arena.GCHeader
	id     uint32
	goRef  arena.GoHandle
	meta   arena.ObjRef
	resRef arena.ObjRef // optional object kept alive via finalizer resurrection
}

type toyThread struct {
	hdr  arena.GCHeader
	id   uint32
	slot arena.ObjRef
}

type toyVM struct {
	a       *arena.Arena
	goRefs  *arena.GoRefs
	gc      *arena.Collector
	roots   []arena.ObjRef
	nextID  uint32
	finLog  []uint32
	onFin   func(id uint32, ref arena.ObjRef)
	freed   map[uint32]bool
}

func newToyVM() *toyVM {
	vm := &toyVM{
		a:      arena.New(),
		goRefs: arena.NewGoRefs(),
		freed:  make(map[uint32]bool),
	}
	traverseEph := func(c *arena.Collector, ref arena.ObjRef, dir int) bool {
		tbl := arena.As[toyTable](vm.a, ref)
		marked := false
		hasWW := false
		hasClears := false
		for step := range int(tbl.len) {
			i := step
			if dir != 0 {
				i = int(tbl.len) - 1 - step
			}
			k, v := tbl.entries[i].key, tbl.entries[i].val
			if k == arena.NullRef || v == arena.NullRef {
				continue
			}
			if vm.isCleared(k) {
				hasClears = true
				if vm.a.Header(v).IsWhite() {
					hasWW = true
				}
			} else if vm.a.Header(v).IsWhite() {
				c.MarkObject(v)
				marked = true
			}
		}
		c.MarkEphemeron(ref, hasWW, hasClears)
		return marked
	}

	vm.gc = arena.NewCollector(vm.a, arena.Callbacks{
		MarkRoots: func(c *arena.Collector) {
			for _, r := range vm.roots {
				c.MarkObject(r)
			}
		},
		MarkAtomicRoots: func(c *arena.Collector) {
			for _, r := range vm.roots {
				c.MarkObject(r)
			}
		},
		IsThread: func(ref arena.ObjRef) bool {
			return vm.a.Header(ref).Kind == toyKindTh
		},
		MarkLeaf: func(c *arena.Collector, ref arena.ObjRef) bool {
			return vm.a.Header(ref).Kind == toyKindStr
		},
		Traverse: func(c *arena.Collector, ref arena.ObjRef) int64 {
			switch vm.a.Header(ref).Kind {
			case toyKindTable:
				tbl := arena.As[toyTable](vm.a, ref)
				switch tbl.weak {
				case arena.WeakNone:
					for i := range tbl.len {
						c.MarkObject(tbl.entries[i].key)
						c.MarkObject(tbl.entries[i].val)
					}
				case arena.WeakKeys:
					traverseEph(c, ref, 0)
				case arena.WeakValues:
					hasClears := false
					for i := range tbl.len {
						k, v := tbl.entries[i].key, tbl.entries[i].val
						if k == arena.NullRef || v == arena.NullRef {
							continue
						}
						c.MarkObject(k)
						if !hasClears && vm.isCleared(v) {
							hasClears = true
						}
					}
					c.MarkWeakValues(ref, hasClears)
				case arena.WeakAll:
					c.MarkAllWeak(ref)
				}
				return int64(tbl.len) + 1
			case toyKindUD:
				u := arena.As[toyUserdata](vm.a, ref)
				c.MarkObject(u.meta)
				c.MarkObject(u.resRef)
				return 2
			case toyKindTh:
				th := arena.As[toyThread](vm.a, ref)
				if vm.a.Header(ref).IsOld() || c.State() == arena.GCSPropagate {
					c.MarkGrayAgain(ref)
				}
				c.MarkObject(th.slot)
				return 2
			default:
				return 1
			}
		},
		TraverseEphemeron: traverseEph,
		ClearByKeys: func(c *arena.Collector, ref arena.ObjRef) {
			tbl := arena.As[toyTable](vm.a, ref)
			for i := range tbl.len {
				if vm.isCleared(tbl.entries[i].key) {
					tbl.entries[i] = toyEntry{}
				}
			}
		},
		ClearByValues: func(c *arena.Collector, ref arena.ObjRef) {
			tbl := arena.As[toyTable](vm.a, ref)
			for i := range tbl.len {
				if vm.isCleared(tbl.entries[i].val) {
					tbl.entries[i] = toyEntry{}
				}
			}
		},
		FreeObject: func(a *arena.Arena, ref arena.ObjRef) {
			hdr := a.Header(ref)
			switch hdr.Kind {
			case toyKindTable:
				vm.freed[arena.As[toyTable](a, ref).id] = true
			case toyKindStr:
				vm.freed[arena.As[toyString](a, ref).id] = true
			case toyKindUD:
				u := arena.As[toyUserdata](a, ref)
				vm.freed[u.id] = true
				if !u.goRef.IsNull() {
					vm.goRefs.Free(u.goRef)
				}
			case toyKindTh:
				vm.freed[arena.As[toyThread](a, ref).id] = true
			}
			a.Free(ref)
		},
		RunFinalizer: func(c *arena.Collector, ref arena.ObjRef) {
			var id uint32
			switch vm.a.Header(ref).Kind {
			case toyKindTable:
				id = arena.As[toyTable](vm.a, ref).id
			case toyKindUD:
				id = arena.As[toyUserdata](vm.a, ref).id
			}
			vm.finLog = append(vm.finLog, id)
			if vm.onFin != nil {
				vm.onFin(id, ref)
			}
		},
	})
	return vm
}

func (vm *toyVM) isCleared(ref arena.ObjRef) bool {
	if ref == arena.NullRef {
		return false
	}
	hdr := vm.a.Header(ref)
	if hdr.Kind == toyKindStr {
		vm.gc.MarkObject(ref) // Lua 5.5: strings are values, never weak!
		return false
	}
	return hdr.IsWhite()
}

func (vm *toyVM) newTable(wk arena.WeakKind) (arena.ObjRef, uint32) {
	ref, err := vm.gc.Alloc(int(unsafe.Sizeof(toyTable{})), toyKindTable)
	if err != nil {
		panic(err)
	}
	vm.nextID++
	t := arena.As[toyTable](vm.a, ref)
	t.id = vm.nextID
	t.weak = wk
	return ref, vm.nextID
}

func (vm *toyVM) newStr() (arena.ObjRef, uint32) {
	ref, err := vm.gc.Alloc(int(unsafe.Sizeof(toyString{})), toyKindStr)
	if err != nil {
		panic(err)
	}
	vm.nextID++
	arena.As[toyString](vm.a, ref).id = vm.nextID
	return ref, vm.nextID
}

func (vm *toyVM) newUD(goVal any) (arena.ObjRef, uint32) {
	ref, err := vm.gc.Alloc(int(unsafe.Sizeof(toyUserdata{})), toyKindUD)
	if err != nil {
		panic(err)
	}
	vm.nextID++
	u := arena.As[toyUserdata](vm.a, ref)
	u.id = vm.nextID
	if goVal != nil {
		u.goRef = vm.goRefs.Alloc(goVal)
	}
	return ref, vm.nextID
}

func (vm *toyVM) newThread() (arena.ObjRef, uint32) {
	ref, err := vm.gc.Alloc(int(unsafe.Sizeof(toyThread{})), toyKindTh)
	if err != nil {
		panic(err)
	}
	vm.nextID++
	arena.As[toyThread](vm.a, ref).id = vm.nextID
	return ref, vm.nextID
}

func (vm *toyVM) setEntry(tblRef arena.ObjRef, k, v arena.ObjRef) {
	tbl := arena.As[toyTable](vm.a, tblRef)
	for i := range tbl.len {
		if tbl.entries[i].key == k {
			tbl.entries[i].val = v
			if v != arena.NullRef {
				vm.gc.BarrierBack(tblRef)
			}
			return
		}
	}
	idx := tbl.len
	tbl.entries[idx] = toyEntry{key: k, val: v}
	tbl.len++
	if k != arena.NullRef || v != arena.NullRef {
		vm.gc.BarrierBack(tblRef)
	}
}

func (vm *toyVM) lookup(tblRef, k arena.ObjRef) arena.ObjRef {
	tbl := arena.As[toyTable](vm.a, tblRef)
	for i := range tbl.len {
		if tbl.entries[i].key == k {
			return tbl.entries[i].val
		}
	}
	return arena.NullRef
}

func TestGCWeakTablesEphemeronsLIFOFinalizersAndResurrection(t *testing.T) {
	for _, mode := range []arena.GCMode{arena.ModeIncremental, arena.ModeGenMinor} {
		name := "incremental"
		if mode == arena.ModeGenMinor {
			name = "generational"
		}
		t.Run(name, func(t *testing.T) {
			vm := newToyVM()
			defer vm.a.Close()
			if mode == arena.ModeGenMinor {
				vm.gc.SetMode(arena.ModeGenMinor)
			}

			// Root tables: ephemeron (weak-k), weak-v, all-weak.
			eph, _ := vm.newTable(arena.WeakKeys)
			wv, _ := vm.newTable(arena.WeakValues)
			wkv, _ := vm.newTable(arena.WeakAll)
			vm.roots = append(vm.roots, eph, wv, wkv)

			// 1. Ephemeron chain: rootKey -> mid -> leaf (must ALL survive),
			//    deadKey -> deadVal referencing deadKey (ephemeron cycle -> must be collected!).
			rootKey, rootKeyID := vm.newTable(arena.WeakNone)
			vm.roots = append(vm.roots, rootKey)
			mid, midID := vm.newTable(arena.WeakNone)
			leaf, leafID := vm.newTable(arena.WeakNone)
			vm.setEntry(eph, rootKey, mid)
			vm.setEntry(eph, mid, leaf)

			deadKey, deadKeyID := vm.newTable(arena.WeakNone)
			deadVal, deadValID := vm.newTable(arena.WeakNone)
			vm.setEntry(deadVal, deadKey, deadKey)
			vm.setEntry(eph, deadKey, deadVal)

			// 2. Weak-v entry vs string value (strings are never cleared!).
			strK, _ := vm.newStr()
			strV, strVID := vm.newStr()
			vm.setEntry(wv, strK, strV)

			// 3. Object with __gc in BOTH weak-v (value) and ephemeron (key):
			//    Lua 5.5 rule: weak-v MUST be cleared BEFORE __gc runs,
			//    while weak-k MUST still contain the object DURING __gc!
			type goRes struct {
				tag int
				pad [64]byte // >16B so Go's tiny allocator never batches it with unrelated live objects
			}
			gObj := &goRes{tag: 77}
			wpGo := weak.Make(gObj)
			ud1, id1 := vm.newUD(gObj)
			gObj = nil
			ud2, id2 := vm.newUD(nil)
			vm.gc.RegisterFinalizer(ud1)
			vm.gc.RegisterFinalizer(ud2)

			markerK, _ := vm.newStr()
			vm.roots = append(vm.roots, markerK)
			vm.setEntry(wv, markerK, ud1)
			vm.setEntry(eph, ud1, markerK)

			var sawWVDuringFin, sawEphDuringFin arena.ObjRef
			vm.onFin = func(id uint32, ref arena.ObjRef) {
				if id == id1 {
					sawWVDuringFin = vm.lookup(wv, markerK)
					sawEphDuringFin = vm.lookup(eph, ref)
				}
			}

			// First full cycle: runs __gc in strict LIFO order [id2, id1], clears wv BEFORE __gc,
			// keeps eph key alive DURING __gc!
			vm.gc.Full(false)

			if !slices.Equal(vm.finLog, []uint32{id2, id1}) {
				t.Fatalf("finalizer LIFO order = %v, want [%d %d]", vm.finLog, id2, id1)
			}
			if sawWVDuringFin != arena.NullRef {
				t.Fatalf("weak-v entry must be cleared BEFORE __gc, got %v", sawWVDuringFin)
			}
			if sawEphDuringFin != markerK {
				t.Fatalf("weak-k entry must remain visible DURING __gc, got %v", sawEphDuringFin)
			}
			if vm.freed[rootKeyID] || vm.freed[midID] || vm.freed[leafID] || vm.freed[strVID] {
				t.Fatalf("live ephemeron chain or string was prematurely freed")
			}
			if !vm.freed[deadKeyID] || !vm.freed[deadValID] {
				t.Fatalf("unreachable ephemeron cycle was not collected: deadKey=%v deadVal=%v",
					vm.freed[deadKeyID], vm.freed[deadValID])
			}
			// ud1 was resurrected for __gc in cycle 1, so it is freed on cycle 2 (along with GoRef!).
			if vm.freed[id1] {
				t.Fatalf("ud1 freed in same cycle it was finalized")
			}

			// Second full cycle: ud1 & ud2 now have FINALIZEDBIT cleared and are unreachable -> freed + GoRef released!
			vm.gc.Full(false)
			if !vm.freed[id1] || !vm.freed[id2] {
				t.Fatalf("finalized userdata not collected on second cycle: id1=%v id2=%v", vm.freed[id1], vm.freed[id2])
			}
			if vm.lookup(eph, ud1) != arena.NullRef {
				t.Fatalf("weak-k entry for collected ud1 was not cleared on cycle 2")
			}
			if vm.goRefs.LiveCount() != 0 {
				t.Fatalf("GoRefs.LiveCount = %d, want 0 after ud1 swept", vm.goRefs.LiveCount())
			}
			reclaimed := false
			for range 20 {
				runtime.GC()
				if wpGo.Value() == nil {
					reclaimed = true
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !reclaimed {
				t.Fatalf("Go payload not reclaimed by Go GC after Arena GC swept owning userdata")
			}
		})
	}
}

func TestGCGenerationalMinorVsMajorAndOldToYoungBarrier(t *testing.T) {
	vm := newToyVM()
	defer vm.a.Close()

	// Create a long-lived old root table + thread and enter generational mode.
	oldTbl, oldID := vm.newTable(arena.WeakNone)
	th, thID := vm.newThread()
	vm.roots = append(vm.roots, oldTbl, th)
	vm.gc.SetParam(arena.ParamMinorMajor, 0, true) // keep in pure minor mode while verifying age transitions
	vm.gc.SetMode(arena.ModeGenMinor)

	if vm.gc.Mode() != arena.ModeGenMinor {
		t.Fatalf("expected ModeGenMinor, got %v", vm.gc.Mode())
	}
	if vm.a.Header(oldTbl).Age() != arena.AgeOld {
		t.Fatalf("expected oldTbl to be AgeOld after entering gen mode, got %d", vm.a.Header(oldTbl).Age())
	}

	// Mutate oldTbl to point to a brand-new young table (triggers BarrierBack -> AgeTouched1).
	youngKeep, youngKeepID := vm.newTable(arena.WeakNone)
	keyStr, _ := vm.newStr()
	vm.setEntry(oldTbl, keyStr, youngKeep)
	if vm.a.Header(oldTbl).Age() != arena.AgeTouched1 {
		t.Fatalf("expected oldTbl AgeTouched1 after BarrierBack, got %d", vm.a.Header(oldTbl).Age())
	}

	// Also point thread slot to another young object (threads stay gray on grayAgain).
	thYoung, thYoungID := vm.newTable(arena.WeakNone)
	arena.As[toyThread](vm.a, th).slot = thYoung

	// Allocate 100 short-lived garbage tables in the nursery.
	deadIDs := make([]uint32, 100)
	for i := range deadIDs {
		_, deadIDs[i] = vm.newTable(arena.WeakNone)
	}

	// Run one minor collection via StepBytes.
	vm.gc.StepBytes(0)
	if vm.gc.MinorCycles() < 1 {
		t.Fatalf("expected minor cycle to run")
	}

	// All 100 nursery garbage objects must be swept; oldTbl, th, youngKeep, thYoung MUST survive!
	for _, id := range deadIDs {
		if !vm.freed[id] {
			t.Fatalf("nursery garbage id=%d survived minor collection", id)
		}
	}
	if vm.freed[oldID] || vm.freed[thID] || vm.freed[youngKeepID] || vm.freed[thYoungID] {
		t.Fatalf("live old/young objects prematurely freed during minor GC")
	}
	if vm.a.Header(oldTbl).Age() != arena.AgeTouched2 {
		t.Fatalf("expected oldTbl to advance AgeTouched1 -> AgeTouched2, got %d", vm.a.Header(oldTbl).Age())
	}
	if vm.a.Header(youngKeep).Age() != arena.AgeSurvival {
		t.Fatalf("expected youngKeep to advance AgeNew -> AgeSurvival, got %d", vm.a.Header(youngKeep).Age())
	}

	// Second minor collection: youngKeep promotes Survival -> Old1, oldTbl advances Touched2 -> Old.
	vm.gc.StepBytes(0)
	if vm.a.Header(youngKeep).Age() != arena.AgeOld1 {
		t.Fatalf("expected youngKeep AgeOld1 after 2nd minor GC, got %d", vm.a.Header(youngKeep).Age())
	}
	if vm.a.Header(oldTbl).Age() != arena.AgeOld {
		t.Fatalf("expected oldTbl AgeOld after 2nd minor GC, got %d", vm.a.Header(oldTbl).Age())
	}

	// Third minor collection: markOld promotes youngKeep Old1 -> Old!
	vm.gc.StepBytes(0)
	if vm.a.Header(youngKeep).Age() != arena.AgeOld {
		t.Fatalf("expected youngKeep AgeOld after 3rd minor GC, got %d", vm.a.Header(youngKeep).Age())
	}
}

func TestGCGenerationalMinorToMajorAndBackToMinor(t *testing.T) {
	vm := newToyVM()
	defer vm.a.Close()

	// Seed a baseline old root and enter generational minor mode with default parameters.
	root, _ := vm.newTable(arena.WeakNone)
	vm.roots = append(vm.roots, root)
	vm.gc.SetMode(arena.ModeGenMinor)
	if vm.gc.Mode() != arena.ModeGenMinor {
		t.Fatalf("expected ModeGenMinor, got %v", vm.gc.Mode())
	}

	// Promote enough surviving objects to exceed minormajor (70%) of baseline old bytes.
	promoted := make([]arena.ObjRef, 0, 16)
	for range 16 {
		obj, _ := vm.newTable(arena.WeakNone)
		promoted = append(promoted, obj)
		vm.roots = append(vm.roots, obj)
	}
	vm.gc.StepBytes(0) // AgeNew -> AgeSurvival
	vm.gc.StepBytes(0) // AgeSurvival -> AgeOld1 -> exceeds minormajor -> shifts to ModeGenMajor!
	if vm.gc.Mode() != arena.ModeGenMajor {
		t.Fatalf("expected automatic shift ModeGenMinor -> ModeGenMajor, got %v", vm.gc.Mode())
	}

	// Drop the promoted objects so major collection reclaims > majorminor% of added bytes
	// and automatically shifts back to ModeGenMinor!
	vm.roots = vm.roots[:1]
	_ = promoted
	returnedToMinor := false
	for range 200 {
		vm.gc.StepBytes(512)
		if vm.gc.Mode() == arena.ModeGenMinor {
			returnedToMinor = true
			break
		}
	}
	if !returnedToMinor {
		t.Fatalf("expected automatic return ModeGenMajor -> ModeGenMinor after reclaiming old garbage, stuck in %v (state=%v)",
			vm.gc.Mode(), vm.gc.State())
	}
	if vm.a.LiveObjects() != 1 {
		t.Fatalf("expected only root live after major->minor return, got %d", vm.a.LiveObjects())
	}
}

func TestGCGenerationalEmptyBaselineTriggersMajorAndReclaimsOld(t *testing.T) {
	vm := newToyVM()
	defer vm.a.Close()

	// Enter generational mode while the arena is completely empty (gcMajorMinor == 0).
	vm.gc.SetMode(arena.ModeGenMinor)
	if vm.gc.Mode() != arena.ModeGenMinor {
		t.Fatalf("expected ModeGenMinor, got %v", vm.gc.Mode())
	}
	if vm.gc.NeedsStep() {
		t.Fatalf("expected non-zero minor debt threshold on empty arena")
	}

	// Allocate and promote a rooted table through minor collections; with default
	// non-zero ParamMinorMajor, promotion must trigger ModeGenMajor rather than getting stuck.
	tbl, tblID := vm.newTable(arena.WeakNone)
	vm.roots = append(vm.roots, tbl)
	vm.gc.StepBytes(0) // AgeNew -> AgeSurvival
	vm.gc.StepBytes(0) // AgeSurvival -> AgeOld1 -> shifts to ModeGenMajor
	if vm.gc.Mode() != arena.ModeGenMajor {
		t.Fatalf("expected ModeGenMajor after promotion from empty baseline, got %v", vm.gc.Mode())
	}

	// Drop the root and step until major collection reclaims the old table.
	vm.roots = nil
	for range 200 {
		vm.gc.StepBytes(512)
		if vm.freed[tblID] {
			break
		}
	}
	if !vm.freed[tblID] || vm.a.LiveObjects() != 0 {
		t.Fatalf("old object allocated after empty gen baseline was not reclaimed: freed=%v live=%d",
			vm.freed[tblID], vm.a.LiveObjects())
	}
}

func TestGCFinalizerReentryGuardsNormalFullAndSetMode(t *testing.T) {
	vm := newToyVM()
	defer vm.a.Close()

	u1, id1 := vm.newUD(nil)
	u2, id2 := vm.newUD(nil)
	vm.gc.RegisterFinalizer(u1)
	vm.gc.RegisterFinalizer(u2)

	reentered := false
	vm.onFin = func(id uint32, ref arena.ObjRef) {
		if id == id2 {
			// Attempting Full(false) or SetMode inside __gc must be ignored (Lua 5.5 GCSTPGC).
			vm.gc.Full(false)
			vm.gc.SetMode(arena.ModeGenMinor)
			if vm.freed[id] {
				reentered = true
			}
		}
	}

	vm.gc.Full(false)
	if reentered {
		t.Fatalf("Full(false) inside RunFinalizer prematurely swept the executing object")
	}
	if vm.gc.Mode() != arena.ModeIncremental {
		t.Fatalf("SetMode inside RunFinalizer unexpectedly changed mode to %v", vm.gc.Mode())
	}
	if !slices.Equal(vm.finLog, []uint32{id2, id1}) {
		t.Fatalf("finalizer order corrupted by reentrant Full(false): got %v, want [%d %d]", vm.finLog, id2, id1)
	}
}

func TestGCEmergencySuppressesFinalizersAndFreeAllRunsLIFO(t *testing.T) {
	vm := newToyVM()
	defer vm.a.Close()

	u1, id1 := vm.newUD(nil)
	u2, id2 := vm.newUD(nil)
	vm.gc.RegisterFinalizer(u1)
	vm.gc.RegisterFinalizer(u2)

	// Emergency GC must NOT invoke __gc finalizers.
	vm.gc.Full(true)
	if len(vm.finLog) != 0 {
		t.Fatalf("emergency GC ran finalizers: %v", vm.finLog)
	}

	// FreeAll (State.Close) must run remaining finalizers in strict LIFO order and release all arena memory.
	vm.gc.FreeAll()
	if !slices.Equal(vm.finLog, []uint32{id2, id1}) {
		t.Fatalf("FreeAll finalizers = %v, want [%d %d]", vm.finLog, id2, id1)
	}
	if vm.a.LiveObjects() != 0 || vm.a.AllocBytes() != 0 {
		t.Fatalf("FreeAll leaked arena objects: live=%d bytes=%d", vm.a.LiveObjects(), vm.a.AllocBytes())
	}
}

// TestGCRandomizedIncrementalAndGenerationalSoundness mutates a random object graph while
// interleaving incremental steps, mode switches, forward/backward write barriers, and full
// collections, verifying ZERO premature frees (dangling refs) and ZERO unreachable leaks.
func TestGCRandomizedIncrementalAndGenerationalSoundness(t *testing.T) {
	vm := newToyVM()
	defer vm.a.Close()

	rng := rand.New(rand.NewPCG(2026, 551))
	const rootSlots = 16
	vm.roots = make([]arena.ObjRef, rootSlots)

	for step := range 5000 {
		op := rng.IntN(100)
		switch {
		case op < 35:
			// Allocate a table and link it into a random root slot or existing root table.
			ref, _ := vm.newTable(arena.WeakNone)
			slot := rng.IntN(rootSlots)
			if vm.roots[slot] == arena.NullRef || rng.IntN(3) == 0 {
				vm.roots[slot] = ref
			} else {
				parent := vm.roots[slot]
				if vm.a.Header(parent).Kind == toyKindTable {
					pt := arena.As[toyTable](vm.a, parent)
					if pt.len < 8 {
						k, _ := vm.newStr()
						vm.setEntry(parent, k, ref)
					} else {
						idx := rng.IntN(int(pt.len))
						pt.entries[idx].val = ref
						vm.gc.BarrierBack(parent)
					}
				}
			}
		case op < 55:
			// Allocate a userdata pointing to a random existing root via forward Barrier.
			ref, _ := vm.newUD(step)
			if target := vm.roots[rng.IntN(rootSlots)]; target != arena.NullRef {
				u := arena.As[toyUserdata](vm.a, ref)
				u.meta = target
				vm.gc.Barrier(ref, target)
			}
			vm.roots[rng.IntN(rootSlots)] = ref
		case op < 70:
			// Allocate unreachable garbage.
			_, _ = vm.newTable(arena.WeakNone)
			_, _ = vm.newStr()
		case op < 80:
			// Drop a root slot.
			vm.roots[rng.IntN(rootSlots)] = arena.NullRef
		case op < 95:
			// Advance incremental or minor GC step.
			vm.gc.StepBytes(128)
		case op < 98:
			// Switch GC mode mid-flight.
			if vm.gc.Mode() == arena.ModeIncremental {
				vm.gc.SetMode(arena.ModeGenMinor)
			} else {
				vm.gc.SetMode(arena.ModeIncremental)
			}
		default:
			vm.gc.Full(false)
		}

		// Verify every reachable object from roots is still live in Arena (ZERO premature collection!).
		for _, r := range vm.roots {
			if r == arena.NullRef {
				continue
			}
			if !vm.a.IsLive(r) {
				t.Fatalf("step %d: root ref %v was prematurely freed!", step, r)
			}
			if vm.a.Header(r).Kind == toyKindTable {
				tbl := arena.As[toyTable](vm.a, r)
				for i := range tbl.len {
					if tbl.entries[i].key != arena.NullRef && !vm.a.IsLive(tbl.entries[i].key) {
						t.Fatalf("step %d: table key %v was prematurely freed!", step, tbl.entries[i].key)
					}
					if tbl.entries[i].val != arena.NullRef && !vm.a.IsLive(tbl.entries[i].val) {
						t.Fatalf("step %d: table val %v was prematurely freed!", step, tbl.entries[i].val)
					}
				}
			} else if vm.a.Header(r).Kind == toyKindUD {
				u := arena.As[toyUserdata](vm.a, r)
				if u.meta != arena.NullRef && !vm.a.IsLive(u.meta) {
					t.Fatalf("step %d: userdata meta %v was prematurely freed!", step, u.meta)
				}
			}
		}
	}

	// Clear all roots and run full GC -> ZERO live objects or GoRefs must remain!
	clear(vm.roots)
	vm.gc.Full(false)
	vm.gc.Full(false)
	if vm.a.LiveObjects() != 0 || vm.a.AllocBytes() != 0 || vm.goRefs.LiveCount() != 0 {
		t.Fatalf("post-clear leak: live=%d bytes=%d goRefs=%d",
			vm.a.LiveObjects(), vm.a.AllocBytes(), vm.goRefs.LiveCount())
	}
}

func TestGCAtomicHooksAndAuxBytesAccounting(t *testing.T) {
	a := arena.New()
	defer a.Close()

	type auxObj struct {
		hdr    arena.GCHeader
		bufRef arena.ObjRef
		auxSz  int64
	}

	var (
		rootObj     arena.ObjRef
		remarkedObj arena.ObjRef
		deadObj     arena.ObjRef
		finObj      arena.ObjRef
		order       []string
		preSweepSaw struct {
			rootWhite     bool
			remarkedWhite bool
			finWhite      bool
			deadWhite     bool
			deadStillLive bool
		}
	)

	var c *arena.Collector
	c = arena.NewCollector(a, arena.Callbacks{
		MarkRoots: func(c *arena.Collector) {
			c.MarkObject(rootObj)
		},
		AfterGrayAgain: func(c *arena.Collector) {
			order = append(order, "afterGrayAgain")
			c.MarkObject(remarkedObj)
		},
		PreSweep: func(c *arena.Collector) {
			order = append(order, "preSweep")
			preSweepSaw.rootWhite = c.IsWhite(rootObj)
			preSweepSaw.remarkedWhite = c.IsWhite(remarkedObj)
			preSweepSaw.finWhite = c.IsWhite(finObj)
			preSweepSaw.deadWhite = c.IsWhite(deadObj)
			preSweepSaw.deadStillLive = a.IsLive(deadObj)
		},
		AuxBytes: func(a *arena.Arena, ref arena.ObjRef) int64 {
			return arena.As[auxObj](a, ref).auxSz
		},
		FreeObject: func(a *arena.Arena, ref arena.ObjRef) {
			obj := arena.As[auxObj](a, ref)
			if obj.bufRef != arena.NullRef {
				a.Free(obj.bufRef)
			}
			a.Free(ref)
			order = append(order, "free")
		},
		RunFinalizer: func(c *arena.Collector, ref arena.ObjRef) {
			order = append(order, "finalizer")
		},
	})

	allocAux := func(bufBytes int) arena.ObjRef {
		ref, err := c.Alloc(int(unsafe.Sizeof(auxObj{})), 1)
		if err != nil {
			t.Fatal(err)
		}
		if bufBytes > 0 {
			bufRef, err := a.AllocZeroed(bufBytes)
			if err != nil {
				t.Fatal(err)
			}
			obj := arena.As[auxObj](a, ref)
			obj.bufRef = bufRef
			obj.auxSz = int64(a.SlotSize(bufRef))
		}
		return ref
	}

	rootObj = allocAux(4096)
	remarkedObj = allocAux(1024)
	finObj = allocAux(0)
	c.RegisterFinalizer(finObj)
	deadObj = allocAux(2048)

	c.Full(false)

	if preSweepSaw.rootWhite || preSweepSaw.remarkedWhite || preSweepSaw.finWhite {
		t.Fatalf("PreSweep saw surviving/resurrected object as white: %+v", preSweepSaw)
	}
	if !preSweepSaw.deadWhite || !preSweepSaw.deadStillLive {
		t.Fatalf("PreSweep expected dead object white & still live before sweep: %+v", preSweepSaw)
	}
	// Verify order: afterGrayAgain -> preSweep -> free -> finalizer.
	wantOrder := []string{"afterGrayAgain", "preSweep", "free", "finalizer"}
	if !slices.Equal(order, wantOrder) {
		t.Fatalf("hook order = %v, want %v", order, wantOrder)
	}
	// Verify MarkedBytes included both GC headers and auxiliary buffer sizes (4096 + 1024).
	wantMinMarked := int64(a.SlotSize(rootObj)+a.SlotSize(remarkedObj)) + 4096 + 1024
	if c.MarkedBytes() < wantMinMarked {
		t.Fatalf("MarkedBytes = %d, want >= %d (including AuxBytes)", c.MarkedBytes(), wantMinMarked)
	}
	if a.IsLive(deadObj) {
		t.Fatalf("deadObj was not freed after PreSweep")
	}

	// Verify generational minor cycle also invokes PreSweep with dead young objects white and old objects non-white.
	c.SetMode(arena.ModeGenMinor)
	order = order[:0]
	deadObj = allocAux(512)
	finObj = arena.NullRef
	c.StepBytes(1)
	if !preSweepSaw.deadWhite || !preSweepSaw.deadStillLive || preSweepSaw.rootWhite {
		t.Fatalf("generational PreSweep invariant violated: %+v", preSweepSaw)
	}
	if a.IsLive(deadObj) {
		t.Fatalf("generational deadObj was not freed")
	}
	c.FreeAll()
}
