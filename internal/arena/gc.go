package arena

import (
	"math"
	"math/bits"
	"slices"
)

// GCState identifies the current phase of the incremental Lua 5.5.1 GC state machine
// (matching third_party/lua/lgc.h).
type GCState uint8

const (
	GCSPropagate GCState = iota
	GCSEnterAtomic
	GCSAtomic
	GCSSwpAllGC
	GCSSwpFinObj
	GCSSwpToBeFnz
	GCSSwpEnd
	GCSCallFin
	GCSPause
)

// GCMode identifies the active collection mode (matching KGC_INC / KGC_GENMINOR / KGC_GENMAJOR).
type GCMode uint8

const (
	ModeIncremental GCMode = iota
	ModeGenMinor
	ModeGenMajor
)

// WeakKind classifies a table's weak-reference mode during GC traversal.
type WeakKind uint8

const (
	WeakNone WeakKind = iota
	WeakKeys          // ephemeron ('k')
	WeakValues        // weak values ('v')
	WeakAll           // weak keys and values ('kv')
)

// Lua 5.5.1 GC parameter indices (matching LUA_GCP*).
const (
	ParamMinorMul = iota
	ParamMajorMinor
	ParamMinorMajor
	ParamPause
	ParamStepMul
	ParamStepSize
	numGCParams
)

const (
	gcSweepMax = 20 // max objects swept per incremental singlestep (GCSWEEPMAX)
	cwuFin     = 10 // work units charged per finalizer invocation (CWUFIN)

	sweepListNone uint8 = iota
	sweepListAllGC
	sweepListFinObj
	sweepListToBeFnz
)

const (
	stepRes2Pause int64 = -3
	stepResAtomic int64 = -2
	stepRes2Minor int64 = -1
)

// CodeGCParam encodes a GC parameter matching Lua 5.5.1 lobject.c:luaO_codeparam
// (4-bit exponent + 4-bit mantissa).
func CodeGCParam(p uint64) byte {
	const maxParam = (uint64(0x1F) << (0xF - 7 - 1)) * 100
	if p >= maxParam {
		return 0xFF
	}
	q := (p*128 + 99) / 100
	if q < 0x10 {
		return byte(q)
	}
	log := uint(bits.Len64(q) - 5)
	return byte(((q >> log) - 0x10) | (uint64(log+1) << 4))
}

// ApplyGCParam applies an encoded GC parameter to x matching Lua 5.5.1 lobject.c:luaO_applyparam.
func ApplyGCParam(p byte, x int64) int64 {
	if x <= 0 {
		return 0
	}
	m := int64(p & 0xF)
	e := int(p >> 4)
	if e > 0 {
		e--
		m += 0x10
	}
	e -= 7
	if e >= 0 {
		if x < (math.MaxInt64/0x1F)>>uint(e) {
			return (x * m) << uint(e)
		}
		return math.MaxInt64
	}
	sh := uint(-e)
	if x < math.MaxInt64/0x1F {
		return (x * m) >> sh
	}
	if (x >> sh) < math.MaxInt64/0x1F {
		return (x >> sh) * m
	}
	return math.MaxInt64
}

// Callbacks bridges object-layout-specific traversal, weak clearing, finalization,
// and cleanup into the generic Arena Lua 5.5.1 GC engine.
type Callbacks struct {
	// MarkRoots marks all root objects at the start of an incremental cycle.
	MarkRoots func(c *Collector)

	// MarkAtomicRoots re-marks roots that may have changed incrementally before atomic
	// (e.g. active thread, registry, global type metatables).
	MarkAtomicRoots func(c *Collector)

	// AfterGrayAgain optionally runs during atomic() right after grayAgain propagation
	// and before ephemeron convergence / weak clearing (e.g. lgc.c:remarkupvals).
	AfterGrayAgain func(c *Collector)

	// PreSweep optionally runs at the end of atomic() right after resurrection & weak-table
	// clearing and right before flipping currentWhite and sweeping. At this point every
	// surviving object (including resurrected finalizables and old-gen objects) is non-White
	// and every soon-to-be-freed object is White (ideal for pruning inline caches, shape
	// transition caches, and dead hash keys).
	PreSweep func(c *Collector)

	// AuxBytes optionally returns the number of auxiliary raw Arena bytes owned exclusively
	// by ref (e.g. a table's backing array/hash/slots or a thread's stack/frames) so
	// gcMarked and generational Old1 promotion accounting match Arena.AllocBytes().
	AuxBytes func(a *Arena, ref ObjRef) int64

	// Traverse marks outgoing ObjRefs from ref (which has already been popped from gray
	// and turned Black unless it is a thread or weak table). Returns work units (>= 1).
	Traverse func(c *Collector, ref ObjRef) int64

	// TraverseEphemeron propagates marks from non-white keys to values in an ephemeron table
	// and must report the table's remaining weak state via Collector.MarkEphemeron(ref, hasWW, hasClears).
	// dir alternates 0/1 across convergence passes. Returns true iff any value was marked.
	TraverseEphemeron func(c *Collector, ref ObjRef, dir int) bool

	// ClearByKeys clears entries with unmarked (non-string) keys from weak-k / all-weak table ref.
	ClearByKeys func(c *Collector, ref ObjRef)

	// ClearByValues clears entries with unmarked (non-string) values from weak-v / all-weak table ref.
	ClearByValues func(c *Collector, ref ObjRef)

	// IsThread reports whether ref is a coroutine/thread (threads remain gray on grayAgain
	// across generational cycles and are always re-traversed in atomic).
	IsThread func(ref ObjRef) bool

	// IsOpenUpval reports whether ref is an open upvalue (kept gray rather than black).
	IsOpenUpval func(ref ObjRef) bool

	// MarkLeaf optionally handles leaf objects (e.g. strings, 0-uvalue userdata, closed upvalues)
	// directly inside MarkObject without pushing them onto the gray stack. Return true if handled.
	MarkLeaf func(c *Collector, ref ObjRef) bool

	// FreeObject releases any auxiliary arena buffers, intern table entries, or GoRefs handles
	// owned by ref, and frees ref from the Arena. If nil, Collector calls a.Free(ref) directly.
	FreeObject func(a *Arena, ref ObjRef)

	// RunFinalizer invokes the __gc finalizer for ref (already moved back onto allgc with
	// MarkFinalized cleared).
	RunFinalizer func(c *Collector, ref ObjRef)

	// CheckSizes optionally shrinks internal tables (e.g. string intern table) at GCSswpend
	// when not in emergency mode.
	CheckSizes func(c *Collector)

	// CancelCheck optionally polls cooperative execution controls during long GC work.
	CancelCheck func() error
}

// Collector implements PUC-Rio Lua 5.5.1's per-State incremental tri-color mark-sweep
// and generational minor/major garbage collector directly over a noscan Arena.
type Collector struct {
	arena *Arena
	cb    Callbacks

	state        GCState
	mode         GCMode
	currentWhite uint8
	running      bool
	inGC         bool
	inFinalizer  bool
	emergency    bool
	closing      bool
	freedAll     bool

	// Intrusive GC object lists linked via GCHeader.Next (zero Go pointers).
	allgc   ObjRef
	finobj  ObjRef
	tobefnz ObjRef
	fixedgc ObjRef

	// Generational boundary cursors inside allgc and finobj (exact lgc.c layout).
	survival   ObjRef
	old1       ObjRef
	reallyold  ObjRef
	firstold1  ObjRef
	finobjsur  ObjRef
	finobjold1 ObjRef
	finobjrold ObjRef

	// Incremental sweep cursor (sweepPrev == NullRef points to list head).
	sweepList uint8
	sweepPrev ObjRef

	// Noscan []ObjRef gray and weak worklists (ptrdata == 0 -> invisible to Go GC).
	gray           []ObjRef
	grayAgain      []ObjRef
	weak           []ObjRef
	allWeak        []ObjRef
	ephemeron      []ObjRef
	ephemeronClear []ObjRef

	// Pacing and generational promotion accounting (exact lgc.c semantics).
	gcMarked     int64
	gcMajorMinor int64
	debt         int64
	debtThresh   int64
	params       [numGCParams]byte

	// Diagnostic counters for tests and observability.
	minorCycles uint64
	majorCycles uint64
	cancelErr   error
}

// NewCollector creates a Lua 5.5.1 Collector managing GC objects inside a.
func NewCollector(a *Arena, cb Callbacks) *Collector {
	return &Collector{
		arena:        a,
		cb:           cb,
		state:        GCSPause,
		mode:         ModeIncremental,
		currentWhite: MarkWhite0,
		running:      true,
		debtThresh:   9600,
		params: [numGCParams]byte{
			CodeGCParam(20),   // minormul
			CodeGCParam(50),   // majorminor
			CodeGCParam(70),   // minormajor
			CodeGCParam(250),  // pause
			CodeGCParam(200),  // stepmul
			CodeGCParam(9600), // stepsize
		},
	}
}

// Arena returns the underlying noscan Arena managed by this Collector.
func (c *Collector) Arena() *Arena { return c.arena }

// State returns the current incremental GC state.
func (c *Collector) State() GCState { return c.state }

// Mode returns the current GC mode (ModeIncremental, ModeGenMinor, or ModeGenMajor).
func (c *Collector) Mode() GCMode { return c.mode }

// CurrentWhite returns the active white bit (MarkWhite0 or MarkWhite1).
func (c *Collector) CurrentWhite() uint8 { return c.currentWhite }

// OtherWhite returns the non-current (dead during sweep) white bit.
func (c *Collector) OtherWhite() uint8 { return c.currentWhite ^ MarkWhiteBits }

// IsRunning reports whether automatic GC steps are enabled.
func (c *Collector) IsRunning() bool { return c.running }

// Stop pauses automatic GC steps.
func (c *Collector) Stop() { c.running = false }

// Restart resumes automatic GC steps.
func (c *Collector) Restart() { c.running = true }

// IsEmergency reports whether the collector is currently inside an emergency GC cycle.
func (c *Collector) IsEmergency() bool { return c.emergency }

// InFinalizer reports whether a __gc finalizer callback is currently executing.
func (c *Collector) InFinalizer() bool { return c.inFinalizer }

// IsClosing reports whether FreeAll has begun shutting down this Collector.
func (c *Collector) IsClosing() bool { return c.closing }

// CollectorSnapshot captures the intrusive GC list heads of a quiescent Collector
// (in GCSPause, ModeIncremental) so a cloned Arena snapshot can initialize its Collector
// with zero pointer relocation or list reconstruction.
type CollectorSnapshot struct {
	AllGC        ObjRef
	FinObj       ObjRef
	FixedGC      ObjRef
	CurrentWhite uint8
}

// Snapshot returns the current intrusive list heads for template cloning.
func (c *Collector) Snapshot() CollectorSnapshot {
	return CollectorSnapshot{
		AllGC:        c.allgc,
		FinObj:       c.finobj,
		FixedGC:      c.fixedgc,
		CurrentWhite: c.currentWhite,
	}
}

// RestoreSnapshot initializes a fresh Collector from a cloned Arena's template snapshot.
func (c *Collector) RestoreSnapshot(snap CollectorSnapshot) {
	c.allgc = snap.AllGC
	c.finobj = snap.FinObj
	c.fixedgc = snap.FixedGC
	c.tobefnz = NullRef
	c.currentWhite = snap.CurrentWhite
	if c.currentWhite != MarkWhite0 && c.currentWhite != MarkWhite1 {
		c.currentWhite = MarkWhite0
	}
	c.state = GCSPause
	c.mode = ModeIncremental
	c.inFinalizer = false
	c.closing = false
	c.freedAll = false
	c.cancelErr = nil
	c.survival, c.old1, c.reallyold, c.firstold1 = NullRef, NullRef, NullRef, NullRef
	c.finobjsur, c.finobjold1, c.finobjrold = NullRef, NullRef, NullRef
	c.sweepList = sweepListNone
	c.sweepPrev = NullRef
	c.clearGrayLists()
	c.gcMarked = c.arena.AllocBytes()
	c.gcMajorMinor = c.gcMarked
	c.setPause()
}

// IsWhite reports whether ref is currently white (unmarked / dead during atomic weak clearing & PreSweep).
func (c *Collector) IsWhite(ref ObjRef) bool {
	return ref != NullRef && c.arena.Header(ref).IsWhite()
}

// MinorCycles returns the total number of generational minor collections completed.
func (c *Collector) MinorCycles() uint64 { return c.minorCycles }

// MajorCycles returns the total number of full/incremental atomic cycles completed.
func (c *Collector) MajorCycles() uint64 { return c.majorCycles }

// MarkedBytes returns bytes marked in the last/current cycle (or added old bytes in minor mode).
func (c *Collector) MarkedBytes() int64 { return c.gcMarked }

// CancelErr returns and clears any cooperative cancellation error observed during GC.
func (c *Collector) CancelErr() error {
	err := c.cancelErr
	c.cancelErr = nil
	return err
}

// SetCancelErr records a cooperative cancellation error observed inside a VM GC callback.
func (c *Collector) SetCancelErr(err error) {
	if err != nil && c.cancelErr == nil {
		c.cancelErr = err
	}
}

// HasCancelErr reports whether a cooperative cancellation error has been recorded in this cycle.
func (c *Collector) HasCancelErr() bool {
	return c.cancelErr != nil
}

// SetParam updates GC parameter idx to val (or returns its current decoded value if val == math.MaxUint64).
// Returns the previous decoded value (matching Lua 5.5 collectgarbage("param", name, val)).
func (c *Collector) SetParam(idx int, val uint64, update bool) uint64 {
	if idx < 0 || idx >= numGCParams {
		return 0
	}
	prev := uint64(ApplyGCParam(c.params[idx], 100))
	if update {
		c.params[idx] = CodeGCParam(val)
	}
	return prev
}

// AddDebt records bytes of allocation/growth debt toward the next automatic GC step.
func (c *Collector) AddDebt(bytes int64) {
	if bytes > 0 {
		c.debt += bytes
	}
}

// NeedsStep reports whether automatic GC pacing is due at the next safepoint.
func (c *Collector) NeedsStep() bool {
	return c.running && !c.inGC && !c.inFinalizer && !c.closing && c.debt >= c.debtThresh
}

func (c *Collector) setDebtTarget(target int64) {
	if target < 0 {
		target = 0
	}
	c.debt = 0
	c.debtThresh = target
}

func (c *Collector) keepInvariant() bool {
	return c.state <= GCSAtomic
}

func (c *Collector) isSweepPhase() bool {
	return c.state >= GCSSwpAllGC && c.state <= GCSSwpEnd
}

// Alloc allocates a zeroed GC-managed object of at least size bytes in the Arena,
// initializes its GCHeader with currentWhite and AgeNew, links it onto allgc, and records debt.
func (c *Collector) Alloc(size int, kind uint8) (ObjRef, error) {
	ref, err := c.arena.AllocGC(size, kind, c.currentWhite)
	if err != nil {
		return NullRef, err
	}
	hdr := c.arena.Header(ref)
	hdr.Next = c.allgc
	c.allgc = ref
	c.debt += int64(c.arena.SlotSize(ref))
	return ref, nil
}

// Fix moves the most recently allocated object (must be at the head of allgc) onto fixedgc
// so it is never swept until FreeAll() (matching luaC_fix).
func (c *Collector) Fix(ref ObjRef) {
	if ref == NullRef || c.allgc != ref {
		return
	}
	hdr := c.arena.Header(ref)
	c.allgc = hdr.Next
	hdr.SetGray()
	hdr.SetAge(AgeOld)
	hdr.Next = c.fixedgc
	c.fixedgc = ref
}

// MarkObject marks ref if it is currently white, either via MarkLeaf or by painting it gray
// and pushing it onto the gray stack (matching markobject / reallymarkobject).
func (c *Collector) MarkObject(ref ObjRef) {
	if ref == NullRef {
		return
	}
	if !c.arena.Header(ref).IsWhite() {
		return
	}
	c.reallyMarkObject(ref)
}

func (c *Collector) reallyMarkObject(ref ObjRef) {
	hdr := c.arena.Header(ref)
	c.gcMarked += int64(c.arena.SlotSize(ref))
	if c.cb.AuxBytes != nil {
		if aux := c.cb.AuxBytes(c.arena, ref); aux > 0 {
			c.gcMarked += aux
		}
	}
	if c.cb.MarkLeaf != nil && c.cb.MarkLeaf(c, ref) {
		if c.cb.IsOpenUpval != nil && c.cb.IsOpenUpval(ref) {
			hdr.SetGray()
		} else {
			hdr.SetBlack()
		}
		return
	}
	if c.cb.IsOpenUpval != nil && c.cb.IsOpenUpval(ref) {
		hdr.SetGray()
		return
	}
	hdr.SetGray()
	c.gray = append(c.gray, ref)
}

// GenLink maintains generational grayAgain linkage for touched old objects
// after traversal (matching lgc.c:genlink).
func (c *Collector) GenLink(ref ObjRef) {
	if ref == NullRef {
		return
	}
	hdr := c.arena.Header(ref)
	if !hdr.IsBlack() {
		return
	}
	switch hdr.Age() {
	case AgeTouched1:
		hdr.SetGray()
		c.grayAgain = append(c.grayAgain, ref)
	case AgeTouched2:
		hdr.SetAge(AgeOld)
	}
}

// MarkGrayAgain transitions a black object back to gray and places it on grayAgain
// (used by Traverse for threads or tables requiring re-traversal in atomic).
func (c *Collector) MarkGrayAgain(ref ObjRef) {
	if ref == NullRef {
		return
	}
	hdr := c.arena.Header(ref)
	if hdr.IsGray() {
		return
	}
	hdr.SetGray()
	c.grayAgain = append(c.grayAgain, ref)
}

// MarkWeakValues links a weak-value table to the appropriate list after traversing its keys
// (matching lgc.c:traverseweakvalue).
func (c *Collector) MarkWeakValues(ref ObjRef, hasClears bool) {
	if ref == NullRef {
		return
	}
	if c.state == GCSPropagate {
		c.MarkGrayAgain(ref)
	} else if hasClears {
		c.arena.Header(ref).SetGray()
		c.weak = append(c.weak, ref)
	} else {
		c.GenLink(ref)
	}
}

// MarkEphemeron links an ephemeron (weak-key) table to the appropriate list after traversal
// (matching lgc.c:traverseephemeron).
func (c *Collector) MarkEphemeron(ref ObjRef, hasWW, hasClears bool) {
	if ref == NullRef {
		return
	}
	if c.state == GCSPropagate {
		c.MarkGrayAgain(ref)
	} else if hasWW {
		c.arena.Header(ref).SetGray()
		c.ephemeron = append(c.ephemeron, ref)
	} else if hasClears {
		c.arena.Header(ref).SetGray()
		c.ephemeronClear = append(c.ephemeronClear, ref)
	} else {
		c.GenLink(ref)
	}
}

// MarkAllWeak links a weak-key+weak-value table to the appropriate list (matching lgc.c:traversetable case 3).
func (c *Collector) MarkAllWeak(ref ObjRef) {
	if ref == NullRef {
		return
	}
	if c.state == GCSPropagate {
		c.MarkGrayAgain(ref)
	} else {
		c.arena.Header(ref).SetGray()
		c.allWeak = append(c.allWeak, ref)
	}
}

// Barrier implements the forward write barrier (matching luaC_barrier_):
// call whenever parent (black) is updated to reference child (white).
func (c *Collector) Barrier(parent, child ObjRef) {
	if parent == NullRef || child == NullRef {
		return
	}
	ph := c.arena.Header(parent)
	ch := c.arena.Header(child)
	if !ph.IsBlack() || !ch.IsWhite() {
		return
	}
	if c.keepInvariant() {
		c.MarkObject(child)
		if ph.IsOld() {
			ch.SetAge(AgeOld0)
		}
	} else if c.mode != ModeGenMinor {
		ph.SetWhite(c.currentWhite)
	}
}

// BarrierBack implements the backward write barrier for tables (matching luaC_barrierback_):
// call whenever a black table is mutated to store a white key or value.
func (c *Collector) BarrierBack(tbl ObjRef) {
	if tbl == NullRef {
		return
	}
	hdr := c.arena.Header(tbl)
	if !hdr.IsBlack() {
		return
	}
	if hdr.Age() == AgeTouched2 {
		// Already present in grayAgain; paint gray to mark touched in this cycle.
		hdr.SetGray()
	} else {
		hdr.SetGray()
		c.grayAgain = append(c.grayAgain, tbl)
	}
	if hdr.IsOld() {
		hdr.SetAge(AgeTouched1)
	}
}

// RegisterFinalizer marks ref for __gc finalization and moves it from allgc to finobj
// (matching luaC_checkfinalizer). Preserves strict LIFO order and generational cursors.
func (c *Collector) RegisterFinalizer(ref ObjRef) {
	if ref == NullRef || c.closing {
		return
	}
	hdr := c.arena.Header(ref)
	if hdr.IsFinalized() {
		return
	}
	if c.isSweepPhase() {
		hdr.SetWhite(c.currentWhite)
		hdr.SetAge(AgeNew)
	} else {
		c.correctAllGCPointers(ref, hdr.Next)
	}

	// Unlink ref from allgc.
	var prev ObjRef
	found := false
	for cur := c.allgc; cur != NullRef; {
		if cur == ref {
			found = true
			break
		}
		prev = cur
		cur = c.arena.Header(cur).Next
	}
	if !found {
		return
	}
	if c.sweepList == sweepListAllGC && c.sweepPrev == ref {
		// Advance sweep past ref before unlinking it so incremental sweep stays anchored.
		c.sweepPrev = prev
	}
	if prev == NullRef {
		c.allgc = hdr.Next
	} else {
		c.arena.Header(prev).Next = hdr.Next
	}
	hdr.Next = c.finobj
	c.finobj = ref
	hdr.SetFinalized()
}

func (c *Collector) correctAllGCPointers(removed, next ObjRef) {
	if c.survival == removed {
		c.survival = next
	}
	if c.old1 == removed {
		c.old1 = next
	}
	if c.reallyold == removed {
		c.reallyold = next
	}
	if c.firstold1 == removed {
		c.firstold1 = next
	}
}

func (c *Collector) freeObj(ref ObjRef) {
	if c.cb.FreeObject != nil {
		c.cb.FreeObject(c.arena, ref)
		return
	}
	c.arena.Free(ref)
}

func (c *Collector) clearGrayLists() {
	c.gray = c.gray[:0]
	c.grayAgain = c.grayAgain[:0]
	c.weak = c.weak[:0]
	c.allWeak = c.allWeak[:0]
	c.ephemeron = c.ephemeron[:0]
	c.ephemeronClear = c.ephemeronClear[:0]
}

func (c *Collector) restartCollection() {
	c.clearGrayLists()
	c.gcMarked = 0
	if c.cb.MarkRoots != nil {
		c.cb.MarkRoots(c)
	}
	c.markBeingFnz()
}

func (c *Collector) propagateOne() int64 {
	n := len(c.gray)
	if n == 0 {
		return 0
	}
	ref := c.gray[n-1]
	c.gray = c.gray[:n-1]
	hdr := c.arena.Header(ref)
	hdr.SetBlack()
	var work int64 = 1
	if c.cb.Traverse != nil {
		if w := c.cb.Traverse(c, ref); w > 0 {
			work = w
		}
	}
	if c.cancelErr != nil {
		// Traverse was interrupted before visiting all outgoing references;
		// requeue ref as gray so resuming GC completes its traversal.
		hdr.SetGray()
		c.gray = append(c.gray, ref)
		return work
	}
	if hdr.IsBlack() {
		c.GenLink(ref)
	}
	return work
}

func (c *Collector) propagateAll() {
	for len(c.gray) > 0 && c.cancelErr == nil {
		c.propagateOne()
	}
}

func (c *Collector) convergeEphemerons() {
	dir := 0
	for {
		if len(c.ephemeron) == 0 || c.cancelErr != nil {
			return
		}
		list := make([]ObjRef, len(c.ephemeron))
		copy(list, c.ephemeron)
		c.ephemeron = c.ephemeron[:0]
		changed := false
		for i, ref := range list {
			if c.cancelErr != nil {
				c.ephemeron = append(c.ephemeron, list[i:]...)
				return
			}
			hdr := c.arena.Header(ref)
			hdr.SetBlack()
			if c.cb.TraverseEphemeron != nil {
				if c.cb.TraverseEphemeron(c, ref, dir) {
					c.propagateAll()
					changed = true
				}
			}
			if c.cancelErr != nil {
				if !slices.Contains(c.ephemeron, ref) {
					c.ephemeron = append(c.ephemeron, ref)
				}
				c.ephemeron = append(c.ephemeron, list[i+1:]...)
				return
			}
		}
		if !changed {
			return
		}
		dir ^= 1
	}
}

func (c *Collector) separateToBeFnz(all bool) {
	// Find tail of tobefnz to append newly separated objects while preserving LIFO order.
	var tail ObjRef
	for cur := c.tobefnz; cur != NullRef; cur = c.arena.Header(cur).Next {
		tail = cur
	}
	limit := c.finobjold1
	if all {
		limit = NullRef
	}
	var prev ObjRef
	cur := c.finobj
	steps := 0
	for cur != limit && cur != NullRef {
		if !all && c.cb.CancelCheck != nil && (steps&2047) == 2047 {
			if err := c.cb.CancelCheck(); err != nil {
				c.cancelErr = err
				break
			}
		}
		steps++
		hdr := c.arena.Header(cur)
		next := hdr.Next
		if !all && !hdr.IsWhite() {
			prev = cur
			cur = next
			continue
		}
		if cur == c.finobjsur {
			c.finobjsur = next
		}
		if cur == c.finobjold1 {
			c.finobjold1 = next
		}
		if cur == c.finobjrold {
			c.finobjrold = next
		}
		if prev == NullRef {
			c.finobj = next
		} else {
			c.arena.Header(prev).Next = next
		}
		hdr.Next = NullRef
		if tail == NullRef {
			c.tobefnz = cur
		} else {
			c.arena.Header(tail).Next = cur
		}
		tail = cur
		cur = next
	}
}

func (c *Collector) markBeingFnz() {
	for cur := c.tobefnz; cur != NullRef; cur = c.arena.Header(cur).Next {
		c.MarkObject(cur)
	}
}

func (c *Collector) abortAtomic(savedGrayAgain []ObjRef) {
	requeue := func(list []ObjRef) {
		for _, ref := range list {
			c.arena.Header(ref).SetGray()
			if !slices.Contains(c.grayAgain, ref) {
				c.grayAgain = append(c.grayAgain, ref)
			}
		}
	}
	requeue(savedGrayAgain)
	requeue(c.weak)
	requeue(c.ephemeron)
	requeue(c.ephemeronClear)
	requeue(c.allWeak)
	c.weak = c.weak[:0]
	c.ephemeron = c.ephemeron[:0]
	c.ephemeronClear = c.ephemeronClear[:0]
	c.allWeak = c.allWeak[:0]
	c.state = GCSPropagate
}

func (c *Collector) atomic() {
	// Save grayAgain before re-marking roots.
	savedGrayAgain := make([]ObjRef, len(c.grayAgain))
	copy(savedGrayAgain, c.grayAgain)
	c.grayAgain = c.grayAgain[:0]

	c.state = GCSAtomic
	if c.cb.MarkAtomicRoots != nil {
		c.cb.MarkAtomicRoots(c)
	} else if c.cb.MarkRoots != nil {
		c.cb.MarkRoots(c)
	}
	c.propagateAll()
	if c.cancelErr != nil {
		c.abortAtomic(savedGrayAgain)
		return
	}

	// Re-traverse grayAgain objects (tables hit by BarrierBack, active threads, etc.).
	c.gray = append(c.gray, savedGrayAgain...)
	c.propagateAll()

	if c.cancelErr != nil {
		c.abortAtomic(savedGrayAgain)
		return
	}

	if c.cb.AfterGrayAgain != nil {
		c.cb.AfterGrayAgain(c)
		c.propagateAll()
	}
	if c.cancelErr != nil {
		c.abortAtomic(savedGrayAgain)
		return
	}

	// 1. Converge ephemeron tables before clearing weak values.
	c.convergeEphemerons()
	if c.cancelErr != nil {
		c.abortAtomic(savedGrayAgain)
		return
	}

	// 2. Clear dead values from weak-v and all-weak tables BEFORE checking finalizers.
	if c.cb.ClearByValues != nil {
		for _, ref := range c.weak {
			c.cb.ClearByValues(c, ref)
			if c.cancelErr != nil {
				c.abortAtomic(savedGrayAgain)
				return
			}
		}
		for _, ref := range c.allWeak {
			c.cb.ClearByValues(c, ref)
			if c.cancelErr != nil {
				c.abortAtomic(savedGrayAgain)
				return
			}
		}
	}
	origWeakLen := len(c.weak)
	origAllWeakLen := len(c.allWeak)

	// 3. Separate unreachable objects with __gc into tobefnz (strict LIFO).
	c.separateToBeFnz(false)
	if c.cancelErr != nil {
		c.abortAtomic(savedGrayAgain)
		return
	}

	// 4. Mark objects pending finalization and propagate resurrection (keeps weak keys alive during __gc!).
	c.markBeingFnz()
	c.propagateAll()
	if c.cancelErr != nil {
		c.abortAtomic(savedGrayAgain)
		return
	}

	// 5. Re-converge ephemerons and clear dead keys from ephemeron & all-weak tables.
	c.convergeEphemerons()
	if c.cancelErr != nil {
		c.abortAtomic(savedGrayAgain)
		return
	}
	if c.cb.ClearByKeys != nil {
		for _, ref := range c.ephemeron {
			c.cb.ClearByKeys(c, ref)
			if c.cancelErr != nil {
				c.abortAtomic(savedGrayAgain)
				return
			}
		}
		for _, ref := range c.ephemeronClear {
			c.cb.ClearByKeys(c, ref)
			if c.cancelErr != nil {
				c.abortAtomic(savedGrayAgain)
				return
			}
		}
		for _, ref := range c.allWeak {
			c.cb.ClearByKeys(c, ref)
			if c.cancelErr != nil {
				c.abortAtomic(savedGrayAgain)
				return
			}
		}
	}

	// 6. Clear dead values from weak tables newly discovered during resurrection.
	if c.cb.ClearByValues != nil {
		for _, ref := range c.weak[origWeakLen:] {
			c.cb.ClearByValues(c, ref)
			if c.cancelErr != nil {
				c.abortAtomic(savedGrayAgain)
				return
			}
		}
		for _, ref := range c.allWeak[origAllWeakLen:] {
			c.cb.ClearByValues(c, ref)
			if c.cancelErr != nil {
				c.abortAtomic(savedGrayAgain)
				return
			}
		}
	}

	if c.cb.PreSweep != nil {
		c.cb.PreSweep(c)
		if c.cancelErr != nil {
			c.abortAtomic(savedGrayAgain)
			return
		}
	}

	// Flip current white for the upcoming sweep phase.
	c.currentWhite = c.OtherWhite()
	c.majorCycles++
}

func (c *Collector) listHeadPtr(listID uint8) *ObjRef {
	switch listID {
	case sweepListAllGC:
		return &c.allgc
	case sweepListFinObj:
		return &c.finobj
	case sweepListToBeFnz:
		return &c.tobefnz
	default:
		return nil
	}
}

func (c *Collector) sweepNextPtr(listID uint8, prev ObjRef) *ObjRef {
	if prev == NullRef {
		return c.listHeadPtr(listID)
	}
	return &c.arena.Header(prev).Next
}

// sweepListStep sweeps at most count objects starting at (listID, prev).
// Returns (nextPrev, done). When done is true, the entire list has been swept.
func (c *Collector) sweepListStep(listID uint8, prev ObjRef, count int) (ObjRef, bool) {
	ow := c.OtherWhite()
	cw := c.currentWhite
	for count > 0 {
		ptr := c.sweepNextPtr(listID, prev)
		if ptr == nil || *ptr == NullRef {
			return NullRef, true
		}
		cur := *ptr
		hdr := c.arena.Header(cur)
		if hdr.IsDead(ow) {
			*ptr = hdr.Next
			c.freeObj(cur)
		} else {
			hdr.SetWhite(cw)
			hdr.SetAge(AgeNew)
			prev = cur
		}
		count--
	}
	ptr := c.sweepNextPtr(listID, prev)
	if ptr == nil || *ptr == NullRef {
		return NullRef, true
	}
	return prev, false
}

// sweepToLive sweeps elements from the head of listID until a live element becomes sweepPrev
// (or the list empties), matching lgc.c:sweeptolive so objects allocated during sweep are not revisited.
func (c *Collector) sweepToLive(listID uint8) {
	c.sweepList = listID
	c.sweepPrev = NullRef
	for {
		prev, done := c.sweepListStep(listID, c.sweepPrev, 1)
		if done {
			c.sweepList = sweepListNone
			c.sweepPrev = NullRef
			return
		}
		if prev != NullRef {
			c.sweepPrev = prev
			return
		}
	}
}

func (c *Collector) enterSweep() {
	c.state = GCSSwpAllGC
	c.sweepToLive(sweepListAllGC)
}

func (c *Collector) sweepStep(nextState GCState, nextList uint8, fast bool) {
	if c.sweepList != sweepListNone {
		limit := gcSweepMax
		if fast {
			limit = math.MaxInt
		}
		prev, done := c.sweepListStep(c.sweepList, c.sweepPrev, limit)
		if done {
			c.sweepList = sweepListNone
			c.sweepPrev = NullRef
		} else {
			c.sweepPrev = prev
		}
		return
	}
	c.state = nextState
	if nextList != sweepListNone {
		c.sweepList = nextList
		c.sweepPrev = NullRef
	}
}

func (c *Collector) popFinalizer() ObjRef {
	ref := c.tobefnz
	if ref == NullRef {
		return NullRef
	}
	hdr := c.arena.Header(ref)
	c.tobefnz = hdr.Next
	hdr.Next = c.allgc
	c.allgc = ref
	hdr.ClearFinalized()
	if c.isSweepPhase() {
		hdr.SetWhite(c.currentWhite)
		hdr.SetAge(AgeNew)
	} else if hdr.Age() == AgeOld1 {
		c.firstold1 = ref
	}
	return ref
}

func (c *Collector) runOneFinalizer() bool {
	ref := c.popFinalizer()
	if ref == NullRef {
		return false
	}
	if c.cb.RunFinalizer != nil {
		savedInGC := c.inGC
		savedInFin := c.inFinalizer
		c.inGC = false
		c.inFinalizer = true
		defer func() {
			c.inFinalizer = savedInFin
			c.inGC = savedInGC
		}()
		c.cb.RunFinalizer(c, ref)
	}
	return true
}

func (c *Collector) callAllPendingFinalizers() {
	for c.tobefnz != NullRef && !c.emergency {
		if c.cancelErr != nil {
			break
		}
		if c.cb.CancelCheck != nil {
			if err := c.cb.CancelCheck(); err != nil {
				c.cancelErr = err
				break
			}
		}
		if !c.runOneFinalizer() {
			break
		}
	}
}

// --- Generational collector (exact Lua 5.5.1 lgc.c) ---

var genNextAge = [...]uint8{
	AgeNew:      AgeSurvival,
	AgeSurvival: AgeOld1,
	AgeOld0:     AgeOld1,
	AgeOld1:     AgeOld,
	AgeOld:      AgeOld,
	AgeTouched1: AgeTouched1,
	AgeTouched2: AgeTouched2,
}

func (c *Collector) sweep2Old(head *ObjRef) {
	var prev ObjRef
	for {
		var cur ObjRef
		if prev == NullRef {
			cur = *head
		} else {
			cur = c.arena.Header(prev).Next
		}
		if cur == NullRef {
			return
		}
		hdr := c.arena.Header(cur)
		if hdr.IsWhite() {
			if prev == NullRef {
				*head = hdr.Next
			} else {
				c.arena.Header(prev).Next = hdr.Next
			}
			c.freeObj(cur)
			continue
		}
		hdr.SetAge(AgeOld)
		if c.cb.IsThread != nil && c.cb.IsThread(cur) {
			hdr.SetGray()
			c.grayAgain = append(c.grayAgain, cur)
		} else if c.cb.IsOpenUpval != nil && c.cb.IsOpenUpval(cur) {
			hdr.SetGray()
		} else {
			hdr.SetBlack()
		}
		prev = cur
	}
}

// sweepGen sweeps objects in [start..limit) for generational minor mode.
// Returns the prev ObjRef pointing to the slot holding limit (NullRef means *head).
func (c *Collector) sweepGen(head *ObjRef, startPrev ObjRef, limit ObjRef, firstOld1 *ObjRef, addedOld *int64) ObjRef {
	prev := startPrev
	cw := c.currentWhite
	for {
		var cur ObjRef
		if prev == NullRef {
			cur = *head
		} else {
			cur = c.arena.Header(prev).Next
		}
		if cur == limit || cur == NullRef {
			return prev
		}
		hdr := c.arena.Header(cur)
		if hdr.IsWhite() {
			if prev == NullRef {
				*head = hdr.Next
			} else {
				c.arena.Header(prev).Next = hdr.Next
			}
			c.freeObj(cur)
			continue
		}
		age := hdr.Age()
		if age == AgeNew {
			hdr.SetWhite(cw)
			hdr.SetAge(AgeSurvival)
		} else {
			nextAge := genNextAge[age]
			hdr.SetAge(nextAge)
			if nextAge == AgeOld1 {
				*addedOld += int64(c.arena.SlotSize(cur))
				if c.cb.AuxBytes != nil {
					if aux := c.cb.AuxBytes(c.arena, cur); aux > 0 {
						*addedOld += aux
					}
				}
				if firstOld1 != nil && *firstOld1 == NullRef {
					*firstOld1 = cur
				}
			}
		}
		prev = cur
	}
}

func (c *Collector) nextOf(head ObjRef, prev ObjRef) ObjRef {
	if prev == NullRef {
		return head
	}
	return c.arena.Header(prev).Next
}

func (c *Collector) correctGrayList(src []ObjRef, dst []ObjRef) []ObjRef {
	for _, ref := range src {
		hdr := c.arena.Header(ref)
		if hdr.IsWhite() {
			continue
		}
		switch {
		case hdr.Age() == AgeTouched1:
			hdr.SetBlack()
			hdr.SetAge(AgeTouched2)
			dst = append(dst, ref)
		case c.cb.IsThread != nil && c.cb.IsThread(ref):
			hdr.SetGray()
			dst = append(dst, ref)
		default:
			if hdr.Age() == AgeTouched2 {
				hdr.SetAge(AgeOld)
			}
			hdr.SetBlack()
		}
	}
	return dst
}

func (c *Collector) correctGrayLists() {
	// Compact grayAgain, weak, allWeak, ephemeron, and ephemeronClear into grayAgain.
	dst := make([]ObjRef, 0, len(c.grayAgain))
	dst = c.correctGrayList(c.grayAgain, dst)
	dst = c.correctGrayList(c.weak, dst)
	dst = c.correctGrayList(c.allWeak, dst)
	dst = c.correctGrayList(c.ephemeron, dst)
	dst = c.correctGrayList(c.ephemeronClear, dst)
	c.grayAgain = dst
	c.weak = c.weak[:0]
	c.allWeak = c.allWeak[:0]
	c.ephemeron = c.ephemeron[:0]
	c.ephemeronClear = c.ephemeronClear[:0]
}

func (c *Collector) markOld(from, to ObjRef) {
	for cur := from; cur != to && cur != NullRef; cur = c.arena.Header(cur).Next {
		hdr := c.arena.Header(cur)
		if hdr.Age() == AgeOld1 {
			hdr.SetAge(AgeOld)
			if hdr.IsBlack() {
				c.reallyMarkObject(cur)
			}
		}
	}
}

func (c *Collector) finishGenCycle() {
	c.correctGrayLists()
	if !c.emergency && c.cb.CheckSizes != nil {
		c.cb.CheckSizes(c)
	}
	c.state = GCSPropagate
	if c.tobefnz != NullRef && !c.emergency {
		c.callAllPendingFinalizers()
	}
}

func (c *Collector) minor2Inc(kind GCMode) {
	c.gcMajorMinor = c.gcMarked
	c.mode = kind
	c.reallyold, c.old1, c.survival, c.firstold1 = NullRef, NullRef, NullRef, NullRef
	c.finobjrold, c.finobjold1, c.finobjsur = NullRef, NullRef, NullRef
	c.enterSweep()
	c.setDebtTarget(ApplyGCParam(c.params[ParamStepSize], 100))
}

func (c *Collector) checkMinorMajor() bool {
	if c.params[ParamMinorMajor] == 0 {
		return false // special case: minormajor == 0 disables automatic major collections
	}
	limit := ApplyGCParam(c.params[ParamMinorMajor], c.gcMajorMinor)
	if limit == 0 {
		return c.gcMarked > 0
	}
	return c.gcMarked >= limit
}

func (c *Collector) checkMajorMinor() bool {
	if c.mode == ModeGenMajor {
		numBytes := c.arena.AllocBytes()
		addedBytes := numBytes - c.gcMajorMinor
		limit := ApplyGCParam(c.params[ParamMajorMinor], addedBytes)
		toBeCollected := numBytes - c.gcMarked
		if toBeCollected > limit {
			c.atomic2Gen()
			c.setMinorDebt()
			return true
		}
	}
	c.gcMajorMinor = c.gcMarked
	return false
}

func (c *Collector) setMinorDebt() {
	target := ApplyGCParam(c.params[ParamMinorMul], c.gcMajorMinor)
	if target == 0 && c.params[ParamMinorMul] != 0 {
		target = ApplyGCParam(c.params[ParamStepSize], 100)
	}
	c.setDebtTarget(target)
}

func (c *Collector) setPause() {
	threshold := ApplyGCParam(c.params[ParamPause], c.gcMarked)
	wait := threshold - c.arena.AllocBytes()
	if wait < 0 {
		wait = 0
	}
	c.setDebtTarget(wait)
}

func (c *Collector) atomic2Gen() {
	c.clearGrayLists()
	c.state = GCSSwpAllGC
	c.sweep2Old(&c.allgc)
	c.reallyold = c.allgc
	c.old1 = c.allgc
	c.survival = c.allgc
	c.firstold1 = NullRef

	c.sweep2Old(&c.finobj)
	c.finobjrold = c.finobj
	c.finobjold1 = c.finobj
	c.finobjsur = c.finobj

	c.sweep2Old(&c.tobefnz)

	c.mode = ModeGenMinor
	c.gcMajorMinor = c.gcMarked
	c.gcMarked = 0
	c.finishGenCycle()
}

func (c *Collector) youngCollection() {
	var addedOld1 int64
	marked := c.gcMarked
	if c.firstold1 != NullRef {
		c.markOld(c.firstold1, c.reallyold)
		c.firstold1 = NullRef
	}
	c.markOld(c.finobj, c.finobjrold)
	c.markOld(c.tobefnz, NullRef)

	c.atomic()
	if c.cancelErr != nil {
		return
	}

	// Sweep nursery [allgc..survival) and survival [survival..old1).
	c.state = GCSSwpAllGC
	psurvival := c.sweepGen(&c.allgc, NullRef, c.survival, &c.firstold1, &addedOld1)
	c.sweepGen(&c.allgc, psurvival, c.old1, &c.firstold1, &addedOld1)
	c.reallyold = c.old1
	c.old1 = c.nextOf(c.allgc, psurvival)
	c.survival = c.allgc

	// Sweep finobj nursery & survival.
	pfinSur := c.sweepGen(&c.finobj, NullRef, c.finobjsur, nil, &addedOld1)
	c.sweepGen(&c.finobj, pfinSur, c.finobjold1, nil, &addedOld1)
	c.finobjrold = c.finobjold1
	c.finobjold1 = c.nextOf(c.finobj, pfinSur)
	c.finobjsur = c.finobj

	// Sweep tobefnz.
	c.sweepGen(&c.tobefnz, NullRef, NullRef, nil, &addedOld1)

	c.gcMarked = marked + addedOld1
	c.minorCycles++

	if c.checkMinorMajor() {
		c.minor2Inc(ModeGenMajor)
		c.gcMarked = 0
	} else {
		c.finishGenCycle()
	}
}

func (c *Collector) singleStep(fast bool) int64 {
	var res int64
	switch c.state {
	case GCSPause:
		c.restartCollection()
		c.state = GCSPropagate
		res = 1
	case GCSPropagate:
		if fast || len(c.gray) == 0 {
			c.state = GCSEnterAtomic
			res = 1
		} else {
			res = c.propagateOne()
		}
	case GCSEnterAtomic:
		c.atomic()
		if c.cancelErr != nil {
			return 0
		}
		if c.checkMajorMinor() {
			res = stepRes2Minor
		} else {
			c.enterSweep()
			res = stepResAtomic
		}
	case GCSSwpAllGC:
		c.sweepStep(GCSSwpFinObj, sweepListFinObj, fast)
		res = gcSweepMax
	case GCSSwpFinObj:
		c.sweepStep(GCSSwpToBeFnz, sweepListToBeFnz, fast)
		res = gcSweepMax
	case GCSSwpToBeFnz:
		c.sweepStep(GCSSwpEnd, sweepListNone, fast)
		res = gcSweepMax
	case GCSSwpEnd:
		if !c.emergency && c.cb.CheckSizes != nil {
			c.cb.CheckSizes(c)
		}
		c.state = GCSCallFin
		res = gcSweepMax
	case GCSCallFin:
		if c.tobefnz != NullRef && !c.emergency {
			if c.cancelErr != nil {
				return 0
			}
			if c.cb.CancelCheck != nil {
				if err := c.cb.CancelCheck(); err != nil {
					c.cancelErr = err
					return 0
				}
			}
			c.runOneFinalizer()
			res = cwuFin
		} else {
			c.state = GCSPause
			res = stepRes2Pause
		}
	}
	return res
}

func (c *Collector) runUntilState(target GCState, fast bool) {
	iters := 0
	for c.state != target {
		if c.cancelErr != nil {
			return
		}
		if c.cb.CancelCheck != nil && (iters&4095) == 4095 {
			if err := c.cb.CancelCheck(); err != nil {
				c.cancelErr = err
				return
			}
		}
		c.singleStep(fast)
		if c.cancelErr != nil {
			return
		}
		iters++
	}
}

func (c *Collector) incStep(stepSize int64) bool {
	work2Do := ApplyGCParam(c.params[ParamStepMul], stepSize/8)
	fast := work2Do == 0
	finishedCycle := false
	iters := 0
	for {
		stres := c.singleStep(fast)
		if c.cancelErr != nil {
			break
		}
		if stres == stepRes2Minor {
			return true
		}
		if stres == stepRes2Pause {
			finishedCycle = true
			break
		}
		if stres == stepResAtomic && !fast {
			break
		}
		if stres > 0 {
			work2Do -= stres
		}
		if !fast && work2Do <= 0 {
			break
		}
		iters++
		if c.cb.CancelCheck != nil && (iters&1023) == 1023 {
			if err := c.cb.CancelCheck(); err != nil {
				c.cancelErr = err
				break
			}
		}
	}
	if c.state == GCSPause {
		c.setPause()
	} else {
		c.setDebtTarget(ApplyGCParam(c.params[ParamStepSize], 100))
	}
	return finishedCycle
}

func (c *Collector) enterGen() {
	c.runUntilState(GCSPause, true)
	if c.cancelErr != nil {
		return
	}
	c.runUntilState(GCSPropagate, true)
	if c.cancelErr != nil {
		return
	}
	c.atomic()
	if c.cancelErr != nil {
		return
	}
	c.atomic2Gen()
	c.setMinorDebt()
}

// SetMode switches between ModeIncremental and ModeGenMinor (matching luaC_changemode).
func (c *Collector) SetMode(newMode GCMode) {
	if c.inGC || c.inFinalizer || c.closing {
		return
	}
	c.inGC = true
	c.cancelErr = nil
	defer func() { c.inGC = false }()
	c.setModeLocked(newMode)
}

func (c *Collector) setModeLocked(newMode GCMode) {
	if c.mode == ModeGenMajor {
		c.mode = ModeIncremental
	}
	if newMode == ModeGenMajor {
		newMode = ModeGenMinor
	}
	if newMode == c.mode {
		return
	}
	if newMode == ModeIncremental {
		c.minor2Inc(ModeIncremental)
	} else {
		c.enterGen()
	}
}

// Step performs one automatic GC step according to the active mode (matching luaC_step).
func (c *Collector) Step() {
	if c.inGC || c.inFinalizer || c.closing {
		return
	}
	if !c.running {
		c.setDebtTarget(20000)
		return
	}
	c.inGC = true
	c.cancelErr = nil
	defer func() { c.inGC = false }()
	switch c.mode {
	case ModeIncremental, ModeGenMajor:
		stepSize := ApplyGCParam(c.params[ParamStepSize], 100)
		c.incStep(stepSize)
	case ModeGenMinor:
		c.youngCollection()
		if c.mode == ModeGenMinor {
			c.setMinorDebt()
		}
	}
}

// StepBytes performs an explicit GC step sized by stepBytes (matching collectgarbage("step", n)).
// Returns true if the step finished a collection cycle.
func (c *Collector) StepBytes(stepBytes int64) bool {
	if c.inGC || c.inFinalizer || c.closing {
		return false
	}
	c.inGC = true
	c.cancelErr = nil
	defer func() { c.inGC = false }()
	if c.mode == ModeGenMinor {
		c.youngCollection()
		if c.cancelErr != nil {
			return false
		}
		if c.mode == ModeGenMinor {
			c.setMinorDebt()
		}
		return true
	}
	if stepBytes <= 0 {
		stepBytes = ApplyGCParam(c.params[ParamStepSize], 100)
	}
	return c.incStep(stepBytes)
}

// Full executes a complete garbage collection cycle (matching luaC_fullgc).
// Non-emergency collections are ignored while a __gc finalizer is executing
// (matching Lua 5.5 GCSTPGC). When emergency is true, finalizers and cache
// shrinking are suppressed so allocation recovery never re-enters Lua code.
func (c *Collector) Full(emergency bool) {
	if c.inGC || c.closing || (c.inFinalizer && !emergency) {
		return
	}
	c.inGC = true
	c.cancelErr = nil
	prevEmergency := c.emergency
	c.emergency = emergency
	defer func() {
		c.emergency = prevEmergency
		c.inGC = false
	}()

	switch c.mode {
	case ModeGenMinor:
		c.minor2Inc(ModeIncremental)
		c.enterGen()
	case ModeIncremental:
		c.fullInc()
	case ModeGenMajor:
		c.mode = ModeIncremental
		c.fullInc()
		c.mode = ModeGenMajor
	}
}

func (c *Collector) fullInc() {
	if c.keepInvariant() {
		// Turn existing black objects back to white before starting a fresh cycle.
		c.enterSweep()
	}
	c.runUntilState(GCSPause, true)
	if c.cancelErr != nil {
		return
	}
	c.runUntilState(GCSCallFin, true)
	if c.cancelErr != nil {
		return
	}
	c.runUntilState(GCSPause, true)
	if c.cancelErr != nil {
		return
	}
	c.setPause()
}

// FreeAll runs all remaining __gc finalizers in strict LIFO order and frees every
// arena object on allgc, tobefnz, and fixedgc (matching luaC_freeallobjects on State.Close).
func (c *Collector) FreeAll() {
	if c.freedAll {
		return
	}
	wasInFin := c.inFinalizer
	if c.closing {
		if wasInFin {
			c.callAllPendingFinalizers()
			return
		}
	} else if !wasInFin {
		c.cancelErr = nil
	}
	c.closing = true
	c.inGC = true
	defer func() { c.inGC = false }()

	c.setModeLocked(ModeIncremental)
	c.separateToBeFnz(true)
	c.inGC = false
	c.callAllPendingFinalizers()
	if wasInFin {
		return
	}
	c.inGC = true

	freeList := func(head *ObjRef) {
		for *head != NullRef {
			cur := *head
			*head = c.arena.Header(cur).Next
			c.freeObj(cur)
		}
	}
	freeList(&c.allgc)
	freeList(&c.finobj)
	freeList(&c.tobefnz)
	freeList(&c.fixedgc)
	c.clearGrayLists()
	c.sweepList = sweepListNone
	c.sweepPrev = NullRef
	c.freedAll = true
}
