package arena_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"runtime"
	"testing"
	"time"
	"unsafe"
	"weak"

	"github.com/yuchanns/moelua/internal/arena"
)

type testNode struct {
	hdr   arena.GCHeader
	left  arena.ObjRef
	right arena.ObjRef
	value int64
}

func TestSizeClassesAndAlignment(t *testing.T) {
	a := arena.New()
	defer a.Close()

	testSizes := []int{
		1, 8, 15, 16, 17, 24, 25, 32, 47, 48, 64, 80, 96, 112, 128,
		129, 160, 192, 256, 384, 512, 768, 1024, 1025, 1536, 2048,
		3072, 4096, 6144, 8192, 10920, 16384, 21840, 32768,
		32769, 40000, 65536, 131072,
	}
	refs := make([]arena.ObjRef, 0, len(testSizes))
	for _, sz := range testSizes {
		ref, err := a.AllocZeroed(sz)
		if err != nil {
			t.Fatalf("AllocZeroed(%d) failed: %v", sz, err)
		}
		if ref.IsNull() {
			t.Fatalf("AllocZeroed(%d) returned NullRef", sz)
		}
		if uintptr(a.Ptr(ref))%arena.Align != 0 {
			t.Fatalf("AllocZeroed(%d) ptr %p not %d-byte aligned", sz, a.Ptr(ref), arena.Align)
		}
		gotSlot := a.SlotSize(ref)
		if gotSlot < sz {
			t.Fatalf("AllocZeroed(%d) SlotSize=%d < requested", sz, gotSlot)
		}
		if sz > arena.MaxSmallSize && !ref.IsLarge() {
			t.Fatalf("AllocZeroed(%d) expected large ref", sz)
		}
		if sz <= arena.MaxSmallSize && ref.IsLarge() {
			t.Fatalf("AllocZeroed(%d) expected slab ref", sz)
		}
		b := a.Bytes(ref, sz)
		for i, c := range b {
			if c != 0 {
				t.Fatalf("AllocZeroed(%d) non-zero byte at %d: %d", sz, i, c)
			}
			b[i] = byte((sz + i) & 0xFF)
		}
		refs = append(refs, ref)
	}
	if a.LiveObjects() != int64(len(testSizes)) {
		t.Fatalf("LiveObjects=%d, want %d", a.LiveObjects(), len(testSizes))
	}

	// Verify zero overlap across all simultaneously live allocations.
	for idx, sz := range testSizes {
		b := a.Bytes(refs[idx], sz)
		for i, c := range b {
			want := byte((sz + i) & 0xFF)
			if c != want {
				t.Fatalf("slot %d (sz=%d) byte %d corrupted: got %d want %d", idx, sz, i, c, want)
			}
		}
	}

	// Free everything (including duplicate Free calls to verify double-free safety) and verify complete reclamation.
	for _, ref := range refs {
		if !a.IsLive(ref) {
			t.Fatalf("expected ref %v to be live before Free", ref)
		}
		a.Free(ref)
		if a.IsLive(ref) {
			t.Fatalf("expected ref %v to be dead after Free", ref)
		}
		a.Free(ref) // idempotent double-free must not corrupt accounting or free list
	}
	a.Free(arena.NullRef)
	if a.LiveObjects() != 0 || a.AllocBytes() != 0 {
		t.Fatalf("after freeing all: live=%d allocBytes=%d, want 0/0", a.LiveObjects(), a.AllocBytes())
	}
	if a.ActivePages() != 0 || a.ActiveLargeSpans() != 0 {
		t.Fatalf("after freeing all: pages=%d large=%d, want 0/0", a.ActivePages(), a.ActiveLargeSpans())
	}
	a.ReleaseSpare()
	if a.BackingBytes() != 0 {
		t.Fatalf("after ReleaseSpare: BackingBytes=%d, want 0", a.BackingBytes())
	}
}

func TestGCHeaderAndTypedViews(t *testing.T) {
	a := arena.New()
	defer a.Close()

	ref, err := a.AllocGC(int(unsafe.Sizeof(testNode{})), 7, arena.MarkWhite0)
	if err != nil {
		t.Fatalf("AllocGC failed: %v", err)
	}
	node := arena.As[testNode](a, ref)
	if node.hdr.Kind != 7 || !node.hdr.IsWhite() || node.hdr.IsBlack() || node.hdr.IsGray() {
		t.Fatalf("unexpected initial header: %+v", node.hdr)
	}
	if node.hdr.Age() != arena.AgeNew || node.hdr.IsOld() {
		t.Fatalf("unexpected initial age: %d", node.hdr.Age())
	}

	// Verify mark/age bit independence.
	node.hdr.SetFinalized()
	node.hdr.SetAge(arena.AgeTouched2)
	node.hdr.SetGray()
	if !node.hdr.IsGray() || !node.hdr.IsFinalized() || node.hdr.Age() != arena.AgeTouched2 || !node.hdr.IsOld() {
		t.Fatalf("header gray/age/finalized mismatch: %+v", node.hdr)
	}
	node.hdr.SetBlack()
	if !node.hdr.IsBlack() || !node.hdr.IsFinalized() || node.hdr.Age() != arena.AgeTouched2 {
		t.Fatalf("header black mismatch: %+v", node.hdr)
	}
	node.hdr.SetWhite(arena.MarkWhite1)
	if !node.hdr.IsWhite() || !node.hdr.IsDead(arena.MarkWhite1) || node.hdr.IsDead(arena.MarkWhite0) {
		t.Fatalf("header white1 mismatch: %+v", node.hdr)
	}
	node.hdr.ClearFinalized()
	if node.hdr.IsFinalized() || node.hdr.Age() != arena.AgeTouched2 {
		t.Fatalf("ClearFinalized clobbered age: %+v", node.hdr)
	}

	// Verify arena.Slice view on an array allocation.
	arrRef, err := a.AllocZeroed(8 + 16*8)
	if err != nil {
		t.Fatalf("AllocZeroed array failed: %v", err)
	}
	nums := arena.Slice[int64](a, arrRef, 8, 16)
	for i := range nums {
		nums[i] = int64(i*i + 3)
	}
	nums2 := arena.Slice[int64](a, arrRef, 8, 16)
	for i, v := range nums2 {
		if v != int64(i*i+3) {
			t.Fatalf("nums[%d]=%d, want %d", i, v, i*i+3)
		}
	}
}

func TestMultiPageGrowthFullUnlinkAndWholePageReclamation(t *testing.T) {
	a := arena.New()
	defer a.Close()

	// Allocate 3 full pages of 64-byte slots (3 * 1024 = 3072 objects).
	const total = 3072
	refs := make([]arena.ObjRef, total)
	for i := range total {
		ref, err := a.Alloc(64)
		if err != nil {
			t.Fatalf("Alloc #%d failed: %v", i, err)
		}
		binary.LittleEndian.PutUint64(a.Bytes(ref, 8), uint64(i+1))
		refs[i] = ref
	}
	if a.ActivePages() != 3 {
		t.Fatalf("ActivePages=%d, want 3", a.ActivePages())
	}
	if a.AllocBytes() != total*64 {
		t.Fatalf("AllocBytes=%d, want %d", a.AllocBytes(), total*64)
	}

	// Free middle page entirely (slots 1024..2047) -> ActivePages must drop to 2.
	for i := 1024; i < 2048; i++ {
		a.Free(refs[i])
	}
	if a.ActivePages() != 2 {
		t.Fatalf("after freeing middle page: ActivePages=%d, want 2", a.ActivePages())
	}

	// Free one slot in page 1 (which was previously full & unlinked from classAvail),
	// then re-allocate and ensure the freed slot is reused without adding a new page.
	a.Free(refs[10])
	reused, err := a.AllocZeroed(64)
	if err != nil {
		t.Fatalf("re-alloc failed: %v", err)
	}
	if reused != refs[10] {
		t.Fatalf("expected reused slot %v, got %v", refs[10], reused)
	}
	if a.ActivePages() != 2 {
		t.Fatalf("reusing freed slot should not allocate new page; ActivePages=%d", a.ActivePages())
	}
	// Verify survivors on pages 1 & 3 stayed intact.
	for i := range 1024 {
		if i == 10 {
			continue
		}
		if got := binary.LittleEndian.Uint64(a.Bytes(refs[i], 8)); got != uint64(i+1) {
			t.Fatalf("page 1 slot %d corrupted: got %d", i, got)
		}
	}
	for i := 2048; i < total; i++ {
		if got := binary.LittleEndian.Uint64(a.Bytes(refs[i], 8)); got != uint64(i+1) {
			t.Fatalf("page 3 slot %d corrupted: got %d", i, got)
		}
	}
}

func TestMemoryLimitEnforcement(t *testing.T) {
	a := arena.New()
	defer a.Close()

	a.SetLimit(256)
	var refs []arena.ObjRef
	for range 4 {
		ref, err := a.Alloc(64)
		if err != nil {
			t.Fatalf("expected allocation within 256B limit, got %v", err)
		}
		refs = append(refs, ref)
	}
	if _, err := a.Alloc(16); !errors.Is(err, arena.ErrOutOfMemory) {
		t.Fatalf("expected ErrOutOfMemory above limit, got %v", err)
	}
	if _, err := a.Alloc(40000); !errors.Is(err, arena.ErrOutOfMemory) {
		t.Fatalf("expected ErrOutOfMemory for oversized large span, got %v", err)
	}
	if _, err := a.Alloc(0); !errors.Is(err, arena.ErrOutOfMemory) {
		t.Fatalf("expected ErrOutOfMemory for zero size, got %v", err)
	}
	if _, err := a.Alloc(-8); !errors.Is(err, arena.ErrOutOfMemory) {
		t.Fatalf("expected ErrOutOfMemory for negative size, got %v", err)
	}

	// Free one 64B slot -> allocating a 64B slot must succeed again.
	a.Free(refs[0])
	if _, err := a.Alloc(64); err != nil {
		t.Fatalf("expected allocation after Free to succeed, got %v", err)
	}
}

func TestCloneSnapshotIndependence(t *testing.T) {
	src := arena.New()
	defer src.Close()

	r1, err := src.AllocZeroed(64)
	if err != nil {
		t.Fatal(err)
	}
	copy(src.Bytes(r1, 12), "hello moelua")

	rLarge, err := src.AllocZeroed(40000)
	if err != nil {
		t.Fatal(err)
	}
	copy(src.Bytes(rLarge, 10), "large-span")

	// Also exercise cloning a non-empty free list inside a slab page.
	rTemp, err := src.AllocZeroed(64)
	if err != nil {
		t.Fatal(err)
	}
	src.Free(rTemp)

	clone := src.CloneSnapshot()
	defer clone.Close()

	if clone.AllocBytes() != src.AllocBytes() || clone.LiveObjects() != src.LiveObjects() {
		t.Fatalf("clone accounting mismatch: (%d,%d) vs (%d,%d)",
			clone.AllocBytes(), clone.LiveObjects(), src.AllocBytes(), src.LiveObjects())
	}
	if !bytes.Equal(clone.Bytes(r1, 12), []byte("hello moelua")) {
		t.Fatalf("clone small bytes mismatch: %q", clone.Bytes(r1, 12))
	}
	if !bytes.Equal(clone.Bytes(rLarge, 10), []byte("large-span")) {
		t.Fatalf("clone large bytes mismatch: %q", clone.Bytes(rLarge, 10))
	}

	// Mutating clone must not affect src (and vice versa).
	copy(clone.Bytes(r1, 12), "MUTATED_CLON")
	copy(clone.Bytes(rLarge, 10), "MUTATED_LG")
	if !bytes.Equal(src.Bytes(r1, 12), []byte("hello moelua")) {
		t.Fatalf("mutating clone small slot affected src: %q", src.Bytes(r1, 12))
	}
	if !bytes.Equal(src.Bytes(rLarge, 10), []byte("large-span")) {
		t.Fatalf("mutating clone large span affected src: %q", src.Bytes(rLarge, 10))
	}

	// Reusing freed slot in clone must work independently.
	rReused, err := clone.AllocZeroed(64)
	if err != nil {
		t.Fatal(err)
	}
	if rReused != rTemp {
		t.Fatalf("expected clone free-list reuse of %v, got %v", rTemp, rReused)
	}
}

func TestArenaCloseReleasesBackingSlicesWhileReachable(t *testing.T) {
	a := arena.New()

	var wpSlab, wpLarge weak.Pointer[byte]
	func() {
		rSmall, err := a.AllocZeroed(64)
		if err != nil {
			t.Fatal(err)
		}
		rLarge, err := a.AllocZeroed(arena.MaxSmallSize + 4096)
		if err != nil {
			t.Fatal(err)
		}
		wpSlab = weak.Make(&a.Bytes(rSmall, 64)[0])
		wpLarge = weak.Make(&a.Bytes(rLarge, 64)[0])
	}()

	// Close while keeping a reachable; Go GC must reclaim prior slab and large span buffers.
	a.Close()
	if a.BackingBytes() != 0 || a.LiveObjects() != 0 || a.ActivePages() != 0 || a.ActiveLargeSpans() != 0 {
		t.Fatalf("post-Close accounting non-zero: backing=%d live=%d pages=%d large=%d",
			a.BackingBytes(), a.LiveObjects(), a.ActivePages(), a.ActiveLargeSpans())
	}

	reclaimed := false
	for range 20 {
		runtime.GC()
		if wpSlab.Value() == nil && wpLarge.Value() == nil {
			reclaimed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !reclaimed {
		t.Fatalf("Close retained backing buffers while Arena stayed reachable: slabAlive=%v largeAlive=%v",
			wpSlab.Value() != nil, wpLarge.Value() != nil)
	}

	// Reusing the closed Arena must lazily re-initialize cleanly.
	rReuse, err := a.AllocZeroed(32)
	if err != nil {
		t.Fatalf("Alloc after Close failed: %v", err)
	}
	if !a.IsLive(rReuse) || a.LiveObjects() != 1 {
		t.Fatalf("post-Close reuse invalid: live=%v count=%d", a.IsLive(rReuse), a.LiveObjects())
	}
	a.Close()
	runtime.KeepAlive(a)
}

func TestRandomizedAllocFreeFuzzProperty(t *testing.T) {
	a := arena.New()
	defer a.Close()

	rng := rand.New(rand.NewPCG(42, 99))
	type entry struct {
		ref  arena.ObjRef
		size int
		tag  uint64
	}
	live := make([]entry, 0, 512)
	var expectedBytes int64

	for step := range 20000 {
		if len(live) == 0 || (len(live) < 400 && rng.IntN(100) < 58) {
			var sz int
			switch rng.IntN(10) {
			case 0:
				sz = arena.MaxSmallSize + 1 + rng.IntN(40000)
			case 1, 2:
				sz = 1025 + rng.IntN(arena.MaxSmallSize-1025)
			default:
				sz = 16 + rng.IntN(1024)
			}
			ref, err := a.Alloc(sz)
			if err != nil {
				t.Fatalf("step %d Alloc(%d) failed: %v", step, sz, err)
			}
			tag := uint64(step) + 1
			buf := a.Bytes(ref, sz)
			binary.LittleEndian.PutUint64(buf[0:8], tag)
			binary.LittleEndian.PutUint64(buf[sz-8:sz], ^tag)
			expectedBytes += int64(a.SlotSize(ref))
			live = append(live, entry{ref: ref, size: sz, tag: tag})
		} else {
			idx := rng.IntN(len(live))
			e := live[idx]
			buf := a.Bytes(e.ref, e.size)
			if binary.LittleEndian.Uint64(buf[0:8]) != e.tag || binary.LittleEndian.Uint64(buf[e.size-8:e.size]) != ^e.tag {
				t.Fatalf("step %d memory corruption in ref=%v size=%d", step, e.ref, e.size)
			}
			expectedBytes -= int64(a.SlotSize(e.ref))
			a.Free(e.ref)
			live[idx] = live[len(live)-1]
			live = live[:len(live)-1]
		}
		if a.AllocBytes() != expectedBytes || a.LiveObjects() != int64(len(live)) {
			t.Fatalf("step %d accounting drift: got (%d,%d), want (%d,%d)",
				step, a.AllocBytes(), a.LiveObjects(), expectedBytes, len(live))
		}
	}

	for _, e := range live {
		buf := a.Bytes(e.ref, e.size)
		if binary.LittleEndian.Uint64(buf[0:8]) != e.tag || binary.LittleEndian.Uint64(buf[e.size-8:e.size]) != ^e.tag {
			t.Fatalf("final check memory corruption in ref=%v size=%d", e.ref, e.size)
		}
		a.Free(e.ref)
	}
	if a.AllocBytes() != 0 || a.LiveObjects() != 0 || a.ActivePages() != 0 || a.ActiveLargeSpans() != 0 {
		t.Fatalf("post-drain leak: alloc=%d live=%d pages=%d large=%d",
			a.AllocBytes(), a.LiveObjects(), a.ActivePages(), a.ActiveLargeSpans())
	}
}

func TestGoRefsGenerationalSafetyAndGCReclamation(t *testing.T) {
	g := arena.NewGoRefs()

	type payload struct {
		id   int
		data [64]byte
	}
	p1 := &payload{id: 101}
	wp1 := weak.Make(p1)

	h1 := g.Alloc(p1)
	p1 = nil // only GoRefs keeps payload alive now
	if h1.IsNull() || g.LiveCount() != 1 {
		t.Fatalf("unexpected handle %v live=%d", h1, g.LiveCount())
	}

	// Run Go GC: GoRefs MUST keep p1 alive (no dangling pointer!).
	runtime.GC()
	runtime.GC()
	if wp1.Value() == nil {
		t.Fatalf("GoRefs failed to keep payload alive across Go GC")
	}
	got, ok := g.Get(h1)
	if !ok || got.(*payload).id != 101 {
		t.Fatalf("Get(h1) = (%v, %v), want id=101", got, ok)
	}
	got = nil

	// Free h1: double-free must return false, stale handle must fail Get/Set.
	if !g.Free(h1) {
		t.Fatalf("Free(h1) returned false")
	}
	if g.Free(h1) {
		t.Fatalf("double Free(h1) must return false")
	}
	if _, ok := g.Get(h1); ok {
		t.Fatalf("stale Get(h1) succeeded after Free")
	}
	if g.Set(h1, "x") {
		t.Fatalf("stale Set(h1) succeeded after Free")
	}

	// Allocate into the recycled slot: new handle must have a different generation so h1 stays invalid!
	h2 := g.Alloc("reused")
	if h2 == h1 {
		t.Fatalf("ABA violation: reused handle %v equals freed handle %v", h2, h1)
	}
	if _, ok := g.Get(h1); ok {
		t.Fatalf("stale handle h1 resolved after slot reuse by h2")
	}
	if v, ok := g.Get(h2); !ok || v != "reused" {
		t.Fatalf("Get(h2) = (%v, %v), want reused", v, ok)
	}

	// Verify Go GC reclaims p1 now that h1 is freed.
	reclaimed := false
	for range 20 {
		runtime.GC()
		if wp1.Value() == nil {
			reclaimed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !reclaimed {
		t.Fatalf("payload was not reclaimed by Go GC after GoRefs.Free")
	}

	// Nil interface payload is valid and distinguishable from freed handle.
	hNil := g.Alloc(nil)
	if v, ok := g.Get(hNil); !ok || v != nil {
		t.Fatalf("Get(hNil) = (%v, %v), want (nil, true)", v, ok)
	}
	if !g.Set(hNil, 42) {
		t.Fatalf("Set(hNil, 42) returned false")
	}
	if v, ok := g.Get(hNil); !ok || v != 42 {
		t.Fatalf("Get(hNil) after Set = (%v, %v), want (42, true)", v, ok)
	}

	// ClearAll releases all handles and invalidates them.
	p2 := &payload{id: 202}
	wp2 := weak.Make(p2)
	h3 := g.Alloc(p2)
	p2 = nil
	g.ClearAll()
	if g.LiveCount() != 0 {
		t.Fatalf("LiveCount after ClearAll = %d, want 0", g.LiveCount())
	}
	if _, ok := g.Get(h2); ok {
		t.Fatalf("h2 still valid after ClearAll")
	}
	if _, ok := g.Get(h3); ok {
		t.Fatalf("h3 still valid after ClearAll")
	}
	reclaimed = false
	for range 20 {
		runtime.GC()
		if wp2.Value() == nil {
			reclaimed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !reclaimed {
		t.Fatalf("p2 was not reclaimed by Go GC after ClearAll")
	}
}

// BenchmarkMultiVMGoGCScan compares Go runtime.GC() latency across 256 independent VMs
// holding 2,000 linked nodes each (512,000 live objects total):
//   - NoscanArena: 256 independent []byte Arenas (ptrdata == 0 -> Go GC scans O(1) per VM)
//   - GoHeapGraph: 256 Go-pointer-linked graphs (Go GC scans 512,000 pointer-bearing structs)
func BenchmarkMultiVMGoGCScan(b *testing.B) {
	const (
		numVMs        = 256
		nodesPerVM    = 2000
		nodeSizeBytes = int(unsafe.Sizeof(testNode{}))
	)

	b.Run("NoscanArena", func(b *testing.B) {
		arenas := make([]*arena.Arena, numVMs)
		for i := range numVMs {
			a := arena.New()
			var prev arena.ObjRef
			for j := range nodesPerVM {
				ref, err := a.AllocGC(nodeSizeBytes, 1, arena.MarkWhite0)
				if err != nil {
					b.Fatal(err)
				}
				n := arena.As[testNode](a, ref)
				n.left = prev
				n.value = int64(j)
				prev = ref
			}
			arenas[i] = a
		}
		defer func() {
			for _, a := range arenas {
				a.Close()
			}
		}()
		runtime.GC()
		b.ResetTimer()
		for b.Loop() {
			runtime.GC()
		}
		runtime.KeepAlive(arenas)
	})

	type goNode struct {
		left  *goNode
		right *goNode
		value int64
	}
	b.Run("GoHeapGraph", func(b *testing.B) {
		roots := make([]*goNode, numVMs)
		for i := range numVMs {
			var prev *goNode
			for j := range nodesPerVM {
				n := &goNode{left: prev, value: int64(j)}
				prev = n
			}
			roots[i] = prev
		}
		runtime.GC()
		b.ResetTimer()
		for b.Loop() {
			runtime.GC()
		}
		runtime.KeepAlive(roots)
	})
}
