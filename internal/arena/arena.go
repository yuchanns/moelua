package arena

import (
	"encoding/binary"
	"errors"
	"math"
	"unsafe"
)

// ErrOutOfMemory is returned when an allocation would exceed the Arena's configured
// memory limit, hard single-allocation cap, or addressable page/span capacity.
var ErrOutOfMemory = errors.New("not enough memory")

// MaxSmallSize is the largest allocation served from segregated 64 KiB slab pages.
// Allocations strictly larger than MaxSmallSize are backed by dedicated large []byte spans.
const MaxSmallSize = 32768

// hardMaxAllocSize guards against integer overflow and pathological single allocations (1 GiB).
const hardMaxAllocSize = 1 << 30

// classSizes defines 26 segregated 8-byte-aligned size classes spanning 16 B .. 32 KiB.
// Every class packs >= 93.75% of each 64 KiB slab page with zero external free-list overhead.
var classSizes = [...]uint32{
	16, 24, 32, 48, 64, 80, 96, 112,
	128, 160, 192, 256, 384, 512, 768, 1024,
	1536, 2048, 3072, 4096, 6144, 8192, 10920, 16384,
	21840, 32768,
}

// NumClasses is the number of small/medium segregated size classes.
const NumClasses = len(classSizes)

var (
	classWords       [NumClasses]uint16
	classSlotsPerPg  [NumClasses]uint16
	smallSizeToClass [129]uint8 // O(1) lookup for (size+7)>>3 when size <= 1024
)

func init() {
	for i, sz := range classSizes {
		if sz < 16 || sz%Align != 0 {
			panic("arena: invalid size class alignment")
		}
		classWords[i] = uint16(sz >> alignShift)
		classSlotsPerPg[i] = uint16(PageSize / sz)
	}
	cls := 0
	for w := 0; w < len(smallSizeToClass); w++ {
		req := uint32(w << alignShift)
		if req == 0 {
			req = 16
		}
		for uint32(classSizes[cls]) < req {
			cls++
		}
		smallSizeToClass[w] = uint8(cls)
	}
}

// SizeClassFor returns the segregated size class index and exact allocated byte size for size.
// Returns (LargeClass, alignedSize) when size > MaxSmallSize.
func SizeClassFor(size int) (uint8, int) {
	if size <= 1024 {
		if size <= 16 {
			return 0, int(classSizes[0])
		}
		cls := smallSizeToClass[(size+Align-1)>>alignShift]
		return cls, int(classSizes[cls])
	}
	if size > MaxSmallSize {
		if size > math.MaxInt-(Align-1) {
			return LargeClass, math.MaxInt &^ (Align - 1)
		}
		aligned := (size + Align - 1) &^ (Align - 1)
		return LargeClass, aligned
	}
	// Linear scan over only 10 upper classes (1536..32768).
	u := uint32(size)
	for cls := 16; cls < NumClasses; cls++ {
		if classSizes[cls] >= u {
			return uint8(cls), int(classSizes[cls])
		}
	}
	return uint8(NumClasses - 1), MaxSmallSize
}

// pageMeta tracks per-page allocation state.
type pageMeta struct {
	liveBits   []uint64 // 1 bit per slot (noscan []uint64); prevents double-free & free-list cycles
	freeHead   ObjRef   // head of per-page intrusive free list (NullRef if empty)
	prevAvail  uint32   // doubly-linked list of pages in classAvail[class] with free space
	nextAvail  uint32
	bumpOff    uint16 // next unallocated 8-byte word offset in [0..capWords]
	capWords   uint16 // backed page capacity in 8-byte words (<= pageWords)
	liveCount  uint16 // number of live objects in this page
	class      uint8  // size class index (0..NumClasses-1)
	inAvail    bool   // true while linked into classAvail[class]
	active     bool   // true while pages[pid] != nil
	sharedSlab bool   // true while pages[pid] is a sub-slice of a compacted/cloned flat slab
}

// nextPageBytes computes the progressive non-moving slab page size for size class cls.
// Small VMs start with compact pages (2 KiB..32 KiB) and ramp smoothly up to PageSize (64 KiB)
// by the 4th page in that class so allocation-heavy workloads still run on full 64 KiB slabs.
func nextPageBytes(cls uint8, opened uint8) int {
	sz := int(classSizes[cls])
	var target int
	switch opened {
	case 0:
		target = max(4*sz, 2048)
	case 1:
		target = max(8*sz, 8192)
	case 2:
		target = max(16*sz, 32768)
	default:
		return PageSize
	}
	if target >= PageSize {
		return PageSize
	}
	slots := target / sz
	if slots < 1 {
		slots = 1
	}
	if n := slots * sz; n <= PageSize {
		return n
	}
	return PageSize
}

// Arena is a per-State noscan []byte memory allocator designed to keep Lua VM heaps
// invisible to Go's GC pointer scanner while returning empty pages and large spans to Go.
//
// An Arena is owned by a single Lua State (single-goroutine execution). Different States
// own completely independent Arenas and may execute concurrently across goroutines.
type Arena struct {
	// 1-based slab page table (pages[0] == nil sentinel so page ObjRefs are always != NullRef).
	// Every element is a noscan []byte of length <= PageSize (64 KiB), aligned to 8 bytes.
	// pagePtrs mirrors pages with cached base pointers for hot-path slab dereferencing.
	pages       [][]byte
	pagePtrs    []unsafe.Pointer
	pageMeta    []pageMeta
	freePageIDs []uint32
	classAvail  [NumClasses]uint32 // head pid of pages with >= 1 free slot per class
	classOpened [NumClasses]uint8  // saturating page-open count per class for progressive sizing
	sparePage   []byte             // 1-page hysteresis cache (always PageSize) to avoid thrashing

	// 1-based dedicated large-object spans (> MaxSmallSize); every element is a noscan []byte.
	largeSpans   [][]byte
	freeLargeIDs []uint32

	allocBytes       int64 // exact live bytes currently allocated across slabs and large spans
	backingSlabBytes int64 // exact bytes currently backed by standalone active slab pages
	sharedSlabBytes  int64 // exact bytes of the shared compacted/cloned flat slab while >= 1 sub-page lives
	sharedSlabPages  int   // number of active pages currently referencing the shared flat slab
	limit            int64 // optional hard byte limit (<= 0 disables limit)
	liveObjects      int64 // exact count of live allocations
	activePages      int   // number of non-nil slab pages
	activeLarge      int   // number of non-nil large spans
}

// New creates an empty noscan Arena with no memory limit (limit <= 0).
func New() *Arena {
	return &Arena{
		pages:      make([][]byte, 1, 8),
		pagePtrs:   make([]unsafe.Pointer, 1, 8),
		pageMeta:   make([]pageMeta, 1, 8),
		largeSpans: make([][]byte, 1, 4),
	}
}

// SetLimit configures a hard byte cap for subsequent allocations (<= 0 disables the cap).
func (a *Arena) SetLimit(limit int64) {
	if limit < 0 {
		limit = 0
	}
	a.limit = limit
}

// Limit returns the configured hard byte cap (0 if unlimited).
func (a *Arena) Limit() int64 { return a.limit }

// AllocBytes returns the exact number of live bytes currently allocated in the Arena.
func (a *Arena) AllocBytes() int64 { return a.allocBytes }

// LiveObjects returns the exact number of live allocations currently held in the Arena.
func (a *Arena) LiveObjects() int64 { return a.liveObjects }

// ActivePages returns the number of currently backed 64 KiB slab pages.
func (a *Arena) ActivePages() int { return a.activePages }

// ActiveLargeSpans returns the number of currently backed large-object spans.
func (a *Arena) ActiveLargeSpans() int { return a.activeLarge }

// BackingBytes returns the total []byte memory currently backed by Go for this Arena
// (standalone active slab pages + retained shared flat slab + cached spare page + active large spans).
func (a *Arena) BackingBytes() int64 {
	total := a.backingSlabBytes + a.sharedSlabBytes
	if a.sparePage != nil {
		total += int64(len(a.sparePage))
	}
	for i := 1; i < len(a.largeSpans); i++ {
		total += int64(len(a.largeSpans[i]))
	}
	return total
}

// CanAlloc reports whether allocating size bytes would stay within the configured limit
// and single-allocation bounds without mutating Arena state.
func (a *Arena) CanAlloc(size int) bool {
	if size <= 0 || size > hardMaxAllocSize {
		return false
	}
	_, need := SizeClassFor(size)
	if a.limit > 0 && (int64(need) > a.limit || a.allocBytes > a.limit-int64(need)) {
		return false
	}
	return true
}

func (a *Arena) ensureInit() {
	if len(a.pages) == 0 {
		a.pages = make([][]byte, 1, 8)
		a.pagePtrs = make([]unsafe.Pointer, 1, 8)
		a.pageMeta = make([]pageMeta, 1, 8)
	}
	if len(a.largeSpans) == 0 {
		a.largeSpans = make([][]byte, 1, 4)
	}
}

// Alloc allocates at least size bytes (8-byte aligned) in noscan []byte storage.
// Returned memory may contain stale bytes from prior freed allocations; use AllocZeroed
// or AllocGC when zero-initialized memory is required.
func (a *Arena) Alloc(size int) (ObjRef, error) {
	if size <= 0 || size > hardMaxAllocSize {
		return NullRef, ErrOutOfMemory
	}
	a.ensureInit()
	cls, need := SizeClassFor(size)
	if a.limit > 0 && (int64(need) > a.limit || a.allocBytes > a.limit-int64(need)) {
		return NullRef, ErrOutOfMemory
	}
	if cls == LargeClass {
		return a.allocLarge(need)
	}
	return a.allocSmall(cls, need)
}

// AllocZeroed allocates at least size bytes (8-byte aligned) and zeroes all allocated class bytes.
func (a *Arena) AllocZeroed(size int) (ObjRef, error) {
	ref, err := a.Alloc(size)
	if err != nil {
		return NullRef, err
	}
	clear(a.slotBytes(ref))
	return ref, nil
}

// AllocGC allocates a zeroed GC-managed arena object of at least size bytes (>= 8),
// initializes its GCHeader (Kind, Class, Mark=white|AgeNew), and returns its ObjRef.
func (a *Arena) AllocGC(size int, kind uint8, currentWhite uint8) (ObjRef, error) {
	if size < int(unsafe.Sizeof(GCHeader{})) {
		return NullRef, ErrOutOfMemory
	}
	ref, err := a.AllocZeroed(size)
	if err != nil {
		return NullRef, err
	}
	cls, _ := SizeClassFor(size)
	white := currentWhite & MarkWhiteBits
	if white == 0 {
		white = MarkWhite0
	}
	hdr := a.Header(ref)
	hdr.Kind = kind
	hdr.Class = cls
	hdr.Mark = white | (AgeNew << AgeShift)
	return ref, nil
}

func (a *Arena) allocLarge(alignedSize int) (ObjRef, error) {
	var idx uint32
	if n := len(a.freeLargeIDs); n > 0 {
		idx = a.freeLargeIDs[n-1]
		a.freeLargeIDs = a.freeLargeIDs[:n-1]
	} else {
		if len(a.largeSpans) > math.MaxInt32 {
			return NullRef, ErrOutOfMemory
		}
		idx = uint32(len(a.largeSpans))
		a.largeSpans = append(a.largeSpans, nil)
	}
	buf := make([]byte, alignedSize)
	if uintptr(unsafe.Pointer(unsafe.SliceData(buf)))%Align != 0 {
		panic("arena: large span not 8-byte aligned")
	}
	a.largeSpans[idx] = buf
	a.allocBytes += int64(alignedSize)
	a.liveObjects++
	a.activeLarge++
	return makeLargeRef(idx), nil
}

func (a *Arena) allocSmall(cls uint8, need int) (ObjRef, error) {
	pid := a.classAvail[cls]
	if pid == 0 {
		var err error
		pid, err = a.allocPage(cls)
		if err != nil {
			return NullRef, err
		}
	}
	meta := &a.pageMeta[pid]
	page := a.pages[pid]
	var ref ObjRef
	var wordOff uint16
	if meta.freeHead != NullRef {
		ref = meta.freeHead
		_, wordOff = splitPageRef(ref)
		byteOff := int(wordOff) << alignShift
		meta.freeHead = ObjRef(binary.LittleEndian.Uint32(page[byteOff : byteOff+4]))
	} else {
		wordOff = meta.bumpOff
		meta.bumpOff += classWords[cls]
		ref = makePageRef(pid, wordOff)
	}
	slotIdx := uint(wordOff / classWords[cls])
	meta.liveBits[slotIdx>>6] |= 1 << (slotIdx & 63)
	meta.liveCount++
	a.allocBytes += int64(need)
	a.liveObjects++

	// Unlink from classAvail once the page has neither free slots nor bump room left.
	if meta.freeHead == NullRef && uint32(meta.bumpOff)+uint32(classWords[cls]) > uint32(meta.capWords) {
		a.unlinkAvail(pid)
	}
	return ref, nil
}

func (a *Arena) allocPage(cls uint8) (uint32, error) {
	var pid uint32
	reused := false
	if n := len(a.freePageIDs); n > 0 {
		pid = a.freePageIDs[n-1]
		a.freePageIDs = a.freePageIDs[:n-1]
		reused = true
	} else {
		if len(a.pages) > maxPageID {
			return 0, ErrOutOfMemory
		}
		pid = uint32(len(a.pages))
	}
	pageBytes := nextPageBytes(cls, a.classOpened[cls])
	var buf []byte
	if pageBytes == PageSize && len(a.sparePage) == PageSize {
		buf = a.sparePage
		a.sparePage = nil
	} else {
		buf = make([]byte, pageBytes)
		if uintptr(unsafe.Pointer(unsafe.SliceData(buf)))%Align != 0 {
			panic("arena: slab page not 8-byte aligned")
		}
	}
	if a.classOpened[cls] < 3 {
		a.classOpened[cls]++
	}
	basePtr := unsafe.Pointer(unsafe.SliceData(buf))
	slots := len(buf) / int(classSizes[cls])
	bitWords := (slots + 63) >> 6
	pm := pageMeta{
		liveBits: make([]uint64, bitWords),
		capWords: uint16(len(buf) >> alignShift),
		class:    cls,
		active:   true,
	}
	if reused {
		a.pages[pid] = buf
		a.pagePtrs[pid] = basePtr
		a.pageMeta[pid] = pm
	} else {
		a.pages = append(a.pages, buf)
		a.pagePtrs = append(a.pagePtrs, basePtr)
		a.pageMeta = append(a.pageMeta, pm)
	}
	a.activePages++
	a.backingSlabBytes += int64(len(buf))
	a.linkAvail(pid)
	return pid, nil
}

func (a *Arena) linkAvail(pid uint32) {
	meta := &a.pageMeta[pid]
	if meta.inAvail {
		return
	}
	cls := meta.class
	head := a.classAvail[cls]
	meta.prevAvail = 0
	meta.nextAvail = head
	if head != 0 {
		a.pageMeta[head].prevAvail = pid
	}
	a.classAvail[cls] = pid
	meta.inAvail = true
}

func (a *Arena) unlinkAvail(pid uint32) {
	meta := &a.pageMeta[pid]
	if !meta.inAvail {
		return
	}
	cls := meta.class
	if meta.prevAvail != 0 {
		a.pageMeta[meta.prevAvail].nextAvail = meta.nextAvail
	} else {
		a.classAvail[cls] = meta.nextAvail
	}
	if meta.nextAvail != 0 {
		a.pageMeta[meta.nextAvail].prevAvail = meta.prevAvail
	}
	meta.prevAvail = 0
	meta.nextAvail = 0
	meta.inAvail = false
}

// Free returns ref to the Arena. Freeing NullRef is a no-op.
// When the last live slot in a 64 KiB page is freed, the entire page is reclaimed
// (keeping at most one spare page to avoid boundary thrashing) and its []byte span
// is returned to Go. Freeing a large span immediately releases its []byte back to Go.
func (a *Arena) Free(ref ObjRef) {
	if ref == NullRef {
		return
	}
	if ref.IsLarge() {
		idx := largeRefIndex(ref)
		if idx == 0 || int(idx) >= len(a.largeSpans) {
			return
		}
		buf := a.largeSpans[idx]
		if buf == nil {
			return
		}
		a.largeSpans[idx] = nil
		a.freeLargeIDs = append(a.freeLargeIDs, idx)
		a.allocBytes -= int64(len(buf))
		a.liveObjects--
		a.activeLarge--
		return
	}
	pid, wordOff := splitPageRef(ref)
	if pid == 0 || int(pid) >= len(a.pages) {
		return
	}
	meta := &a.pageMeta[pid]
	if !meta.active || meta.liveCount == 0 {
		return
	}
	cls := meta.class
	if wordOff >= meta.bumpOff || wordOff%classWords[cls] != 0 {
		return
	}
	slotIdx := uint(wordOff / classWords[cls])
	bitMask := uint64(1) << (slotIdx & 63)
	if meta.liveBits[slotIdx>>6]&bitMask == 0 {
		return // already freed (idempotent against double-free)
	}
	meta.liveBits[slotIdx>>6] &^= bitMask
	meta.liveCount--
	a.allocBytes -= int64(classSizes[cls])
	a.liveObjects--

	if meta.liveCount == 0 {
		a.unlinkAvail(pid)
		buf := a.pages[pid]
		shared := meta.sharedSlab
		a.pages[pid] = nil
		a.pagePtrs[pid] = nil
		*meta = pageMeta{}
		a.freePageIDs = append(a.freePageIDs, pid)
		a.activePages--
		if shared {
			a.sharedSlabPages--
			if a.sharedSlabPages == 0 {
				a.sharedSlabBytes = 0
			}
		} else {
			a.backingSlabBytes -= int64(len(buf))
			if a.sparePage == nil && len(buf) == PageSize {
				a.sparePage = buf
			}
		}
		return
	}

	page := a.pages[pid]
	byteOff := int(wordOff) << alignShift
	binary.LittleEndian.PutUint32(page[byteOff:byteOff+4], uint32(meta.freeHead))
	meta.freeHead = ref
	if !meta.inAvail {
		a.linkAvail(pid)
	}
}

// IsLive reports whether ref currently refers to a live (allocated, not freed) arena slot.
func (a *Arena) IsLive(ref ObjRef) bool {
	if ref == NullRef {
		return false
	}
	if ref.IsLarge() {
		idx := largeRefIndex(ref)
		return idx != 0 && int(idx) < len(a.largeSpans) && a.largeSpans[idx] != nil
	}
	pid, wordOff := splitPageRef(ref)
	if pid == 0 || int(pid) >= len(a.pages) {
		return false
	}
	meta := &a.pageMeta[pid]
	if !meta.active || wordOff >= meta.bumpOff || wordOff%classWords[meta.class] != 0 {
		return false
	}
	slotIdx := uint(wordOff / classWords[meta.class])
	return meta.liveBits[slotIdx>>6]&(uint64(1)<<(slotIdx&63)) != 0
}

// SlotSize returns the exact allocated byte capacity of ref (0 if ref is NullRef or inactive).
func (a *Arena) SlotSize(ref ObjRef) int {
	if ref == NullRef {
		return 0
	}
	if ref.IsLarge() {
		idx := largeRefIndex(ref)
		if idx == 0 || int(idx) >= len(a.largeSpans) || a.largeSpans[idx] == nil {
			return 0
		}
		return len(a.largeSpans[idx])
	}
	pid, _ := splitPageRef(ref)
	if pid == 0 || int(pid) >= len(a.pages) || !a.pageMeta[pid].active {
		return 0
	}
	return int(classSizes[a.pageMeta[pid].class])
}

func (a *Arena) slotBytes(ref ObjRef) []byte {
	if ref.IsLarge() {
		return a.largeSpans[largeRefIndex(ref)]
	}
	pid, wordOff := splitPageRef(ref)
	byteOff := int(wordOff) << alignShift
	sz := int(classSizes[a.pageMeta[pid].class])
	return a.pages[pid][byteOff : byteOff+sz]
}

// Bytes returns a slice of length n backed directly by ref's noscan []byte storage.
// Panics if ref is invalid or n exceeds the slot size.
func (a *Arena) Bytes(ref ObjRef, n int) []byte {
	if ref == NullRef || n < 0 {
		panic("arena: invalid Bytes access")
	}
	if ref.IsLarge() {
		idx := largeRefIndex(ref)
		if idx == 0 || int(idx) >= len(a.largeSpans) || a.largeSpans[idx] == nil {
			panic("arena: invalid large Bytes ref")
		}
		buf := a.largeSpans[idx]
		if n > len(buf) {
			panic("arena: Bytes length exceeds large span size")
		}
		return buf[:n:n]
	}
	pid, wordOff := splitPageRef(ref)
	if pid == 0 || int(pid) >= len(a.pages) || !a.pageMeta[pid].active {
		panic("arena: invalid slab Bytes ref")
	}
	byteOff := int(wordOff) << alignShift
	if n > int(classSizes[a.pageMeta[pid].class]) || byteOff+n > len(a.pages[pid]) {
		panic("arena: Bytes length exceeds slot size")
	}
	return a.pages[pid][byteOff : byteOff+n : byteOff+n]
}

// Ptr returns an 8-byte-aligned unsafe.Pointer to the start of ref's noscan storage.
// Safe under -d=checkptr=2 when cast to any pointer-free struct T with alignof(T) <= 8
// and sizeof(T) <= SlotSize(ref).
func (a *Arena) Ptr(ref ObjRef) unsafe.Pointer {
	if ref == NullRef {
		return nil
	}
	if ref.IsLarge() {
		idx := largeRefIndex(ref)
		if idx == 0 || int(idx) >= len(a.largeSpans) || a.largeSpans[idx] == nil {
			panic("arena: invalid large Ptr ref")
		}
		return unsafe.Pointer(unsafe.SliceData(a.largeSpans[idx]))
	}
	pid, wordOff := splitPageRef(ref)
	if pid == 0 || int(pid) >= len(a.pages) || !a.pageMeta[pid].active {
		panic("arena: invalid slab Ptr ref")
	}
	byteOff := uintptr(wordOff) << alignShift
	return unsafe.Add(a.pagePtrs[pid], byteOff)
}

// PagePtrs returns the backing slab page base pointer table for hot-loop caching.
// Callers caching the returned slice header must reload it after any allocation that may
// append a new slab page or after Close.
func (a *Arena) PagePtrs() []unsafe.Pointer {
	return a.pagePtrs
}

// LargeSpans returns the backing large-span table for hot-loop dereferencing.
// Callers caching the returned slice header must reload it after any allocation that may
// append a new large span or after Close.
func (a *Arena) LargeSpans() [][]byte {
	return a.largeSpans
}

// PtrInPages resolves a live non-large slab ObjRef directly against a cached PagePtrs slice.
// Caller must guarantee ref != NullRef, !ref.IsLarge(), and that ref addresses a live slab slot.
func PtrInPages(pagePtrs []unsafe.Pointer, ref ObjRef) unsafe.Pointer {
	return unsafe.Add(pagePtrs[ref>>wordBits], uintptr(ref&wordMask)<<alignShift)
}

// PtrSmall resolves a live non-large slab ObjRef without large-span branching or pageMeta
// validation. Returns nil for NullRef. Caller must guarantee !ref.IsLarge() and that ref
// addresses a live slab slot; passing a large ref panics on the pagePtrs bounds check.
func (a *Arena) PtrSmall(ref ObjRef) unsafe.Pointer {
	if ref == NullRef {
		return nil
	}
	return unsafe.Add(a.pagePtrs[ref>>wordBits], uintptr(ref&wordMask)<<alignShift)
}

// PtrFast resolves any live slab or large ObjRef with minimal branching.
// Returns nil for NullRef. Caller must guarantee ref addresses a live allocation.
func (a *Arena) PtrFast(ref ObjRef) unsafe.Pointer {
	if ref == NullRef {
		return nil
	}
	if !ref.IsLarge() {
		return unsafe.Add(a.pagePtrs[ref>>wordBits], uintptr(ref&wordMask)<<alignShift)
	}
	return unsafe.Pointer(unsafe.SliceData(a.largeSpans[largeRefIndex(ref)]))
}

// Header returns the 8-byte GCHeader located at offset 0 of ref.
func (a *Arena) Header(ref ObjRef) *GCHeader {
	return (*GCHeader)(a.Ptr(ref))
}

// As casts ref's 8-byte-aligned arena slot to *T.
// T must be a pointer-free (noscan) struct with alignof(T) <= 8 and sizeof(T) <= SlotSize(ref).
func As[T any](a *Arena, ref ObjRef) *T {
	return (*T)(a.Ptr(ref))
}

// AsSmall casts a live non-large slab ref to *T using the unchecked PtrSmall fast path.
// T must be a pointer-free (noscan) struct with alignof(T) <= 8 and sizeof(T) <= SlotSize(ref).
func AsSmall[T any](a *Arena, ref ObjRef) *T {
	return (*T)(a.PtrSmall(ref))
}

// AsFast casts a live slab or large ref to *T using the PtrFast fast path.
// T must be a pointer-free (noscan) struct with alignof(T) <= 8 and sizeof(T) <= SlotSize(ref).
func AsFast[T any](a *Arena, ref ObjRef) *T {
	return (*T)(a.PtrFast(ref))
}

// SliceFast returns a Go slice view of length n of pointer-free element type T starting at
// byteOff bytes inside a live slab or large allocation without SlotSize validation.
// Caller must guarantee ref is live, byteOff is aligned to alignof(T), and byteOff + n*sizeof(T) <= SlotSize(ref).
func SliceFast[T any](a *Arena, ref ObjRef, byteOff, n int) []T {
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*T)(unsafe.Add(a.PtrFast(ref), byteOff)), n)
}

// Slice returns a Go slice view of length n of pointer-free element type T starting at byteOff
// bytes inside ref's slot. byteOff must be aligned to alignof(T) and fit within SlotSize(ref).
func Slice[T any](a *Arena, ref ObjRef, byteOff, n int) []T {
	if n == 0 {
		return nil
	}
	slotSz := a.SlotSize(ref)
	if slotSz == 0 {
		panic("arena: Slice on invalid ref")
	}
	var zero T
	elemSize := int(unsafe.Sizeof(zero))
	if align := int(unsafe.Alignof(zero)); align > 1 && byteOff%align != 0 {
		panic("arena: Slice byteOff misaligned")
	}
	if byteOff < 0 || n < 0 || byteOff > slotSz || (elemSize > 0 && n > (slotSz-byteOff)/elemSize) {
		panic("arena: Slice out of slot bounds")
	}
	base := unsafe.Add(a.Ptr(ref), byteOff)
	return unsafe.Slice((*T)(base), n)
}

// ReleaseSpare drops the cached empty spare page back to Go.
func (a *Arena) ReleaseSpare() {
	a.sparePage = nil
}

// Close releases all backing slab pages, large spans, and metadata immediately back to Go.
// After Close, the Arena is empty and ready for reuse or garbage collection.
func (a *Arena) Close() {
	clear(a.pages)
	clear(a.pagePtrs)
	clear(a.pageMeta)
	clear(a.largeSpans)
	a.pages = nil
	a.pagePtrs = nil
	a.pageMeta = nil
	a.freePageIDs = nil
	a.classAvail = [NumClasses]uint32{}
	a.classOpened = [NumClasses]uint8{}
	a.sparePage = nil
	a.largeSpans = nil
	a.freeLargeIDs = nil
	a.allocBytes = 0
	a.backingSlabBytes = 0
	a.sharedSlabBytes = 0
	a.sharedSlabPages = 0
	a.liveObjects = 0
	a.activePages = 0
	a.activeLarge = 0
}

// CompactSnapshot shrinks every active slab page to its exact bumped prefix (meta.bumpOff
// words), packs all compacted pages into one contiguous 8-byte-aligned noscan []byte slab
// and all live bitmaps into one contiguous []uint64 slice, releases any spare page, resets
// progressive page-size ramps, and unlinks pages with no remaining free slots from classAvail.
// All live ObjRefs remain bitwise identical (zero pointer relocation).
//
// Calling CompactSnapshot once on a frozen read-only template Arena minimizes both idle
// per-State memory footprint and CloneSnapshot allocation count/latency.
func (a *Arena) CompactSnapshot() {
	a.ReleaseSpare()
	a.classOpened = [NumClasses]uint8{}
	if len(a.pages) <= 1 {
		return
	}
	var totalBytes int
	var totalBitWords int
	var packedPages int
	for pid := 1; pid < len(a.pages); pid++ {
		meta := &a.pageMeta[pid]
		if !meta.active || a.pages[pid] == nil {
			continue
		}
		usedBytes := int(meta.bumpOff) << alignShift
		totalBytes += usedBytes
		slots := int(meta.bumpOff / classWords[meta.class])
		totalBitWords += (slots + 63) >> 6
		packedPages++
	}
	if totalBytes == 0 {
		a.backingSlabBytes = 0
		a.sharedSlabBytes = 0
		a.sharedSlabPages = 0
		return
	}
	flatSlab := make([]byte, totalBytes)
	if uintptr(unsafe.Pointer(unsafe.SliceData(flatSlab)))%Align != 0 {
		panic("arena: compact slab not 8-byte aligned")
	}
	var flatBits []uint64
	if totalBitWords > 0 {
		flatBits = make([]uint64, totalBitWords)
	}
	shared := packedPages > 1
	byteOff := 0
	bitOff := 0
	for pid := 1; pid < len(a.pages); pid++ {
		meta := &a.pageMeta[pid]
		if !meta.active || a.pages[pid] == nil {
			continue
		}
		usedBytes := int(meta.bumpOff) << alignShift
		sub := flatSlab[byteOff : byteOff+usedBytes : byteOff+usedBytes]
		copy(sub, a.pages[pid][:usedBytes])
		byteOff += usedBytes

		slots := int(meta.bumpOff / classWords[meta.class])
		bitWords := (slots + 63) >> 6
		var subBits []uint64
		if bitWords > 0 {
			subBits = flatBits[bitOff : bitOff+bitWords : bitOff+bitWords]
			copy(subBits, meta.liveBits[:bitWords])
			bitOff += bitWords
		}

		a.pages[pid] = sub
		a.pagePtrs[pid] = unsafe.Pointer(unsafe.SliceData(sub))
		meta.liveBits = subBits
		meta.capWords = meta.bumpOff
		meta.sharedSlab = shared
		if meta.freeHead == NullRef {
			a.unlinkAvail(uint32(pid))
		}
	}
	if shared {
		a.backingSlabBytes = 0
		a.sharedSlabBytes = int64(totalBytes)
		a.sharedSlabPages = packedPages
	} else {
		a.backingSlabBytes = int64(totalBytes)
		a.sharedSlabBytes = 0
		a.sharedSlabPages = 0
	}
}

// CloneSnapshot produces a completely independent deep copy of this Arena by copying
// active noscan []byte pages into a single contiguous flat slab and copying active large
// spans directly. Because all internal links use ObjRef handles rather than Go pointers,
// every ObjRef remains identical with zero pointer relocation.
func (a *Arena) CloneSnapshot() *Arena {
	if len(a.pages) == 0 && len(a.largeSpans) == 0 {
		cp := New()
		cp.limit = a.limit
		return cp
	}
	pageLen := max(len(a.pages), 1)
	cp := &Arena{
		pages:       make([][]byte, pageLen),
		pagePtrs:    make([]unsafe.Pointer, pageLen),
		pageMeta:    make([]pageMeta, max(len(a.pageMeta), 1)),
		classAvail:  a.classAvail,
		classOpened: a.classOpened,
		largeSpans:  make([][]byte, max(len(a.largeSpans), 1)),
		allocBytes:  a.allocBytes,
		limit:       a.limit,
		liveObjects: a.liveObjects,
		activePages: a.activePages,
		activeLarge: a.activeLarge,
	}
	copy(cp.pageMeta, a.pageMeta)
	if len(a.freePageIDs) > 0 {
		cp.freePageIDs = make([]uint32, len(a.freePageIDs))
		copy(cp.freePageIDs, a.freePageIDs)
	}
	if len(a.freeLargeIDs) > 0 {
		cp.freeLargeIDs = make([]uint32, len(a.freeLargeIDs))
		copy(cp.freeLargeIDs, a.freeLargeIDs)
	}

	var totalSlabBytes int
	var totalBitWords int
	var packedPages int
	for pid := 1; pid < len(a.pages); pid++ {
		if src := a.pages[pid]; src != nil {
			totalSlabBytes += len(src)
			packedPages++
		}
		if srcBits := a.pageMeta[pid].liveBits; len(srcBits) > 0 {
			totalBitWords += len(srcBits)
		}
	}
	var flatSlab []byte
	if totalSlabBytes > 0 {
		flatSlab = make([]byte, totalSlabBytes)
		if uintptr(unsafe.Pointer(unsafe.SliceData(flatSlab)))%Align != 0 {
			panic("arena: cloned slab not 8-byte aligned")
		}
	}
	var flatBits []uint64
	if totalBitWords > 0 {
		flatBits = make([]uint64, totalBitWords)
	}
	shared := packedPages > 1
	byteOff := 0
	bitOff := 0
	for pid := 1; pid < len(a.pages); pid++ {
		if src := a.pages[pid]; src != nil {
			n := len(src)
			dst := flatSlab[byteOff : byteOff+n : byteOff+n]
			copy(dst, src)
			byteOff += n
			cp.pages[pid] = dst
			cp.pagePtrs[pid] = unsafe.Pointer(unsafe.SliceData(dst))
			cp.pageMeta[pid].sharedSlab = shared
		}
		if srcBits := a.pageMeta[pid].liveBits; len(srcBits) > 0 {
			nb := len(srcBits)
			dstBits := flatBits[bitOff : bitOff+nb : bitOff+nb]
			copy(dstBits, srcBits)
			bitOff += nb
			cp.pageMeta[pid].liveBits = dstBits
		}
	}
	if shared {
		cp.sharedSlabBytes = int64(totalSlabBytes)
		cp.sharedSlabPages = packedPages
	} else {
		cp.backingSlabBytes = int64(totalSlabBytes)
	}

	for idx := 1; idx < len(a.largeSpans); idx++ {
		if src := a.largeSpans[idx]; src != nil {
			dst := make([]byte, len(src))
			copy(dst, src)
			cp.largeSpans[idx] = dst
		}
	}
	return cp
}
