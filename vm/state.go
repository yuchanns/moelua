package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
	"github.com/yuchanns/moelua/internal/arena"
)

// ErrInstructionLimit is returned when a State exceeds its configured VM instruction budget.
// It cannot be caught or suppressed by Lua pcall/xpcall or coroutine.resume.
var ErrInstructionLimit = errors.New("moelua: instruction limit exceeded")

func isUncatchableErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrInstructionLimit) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

const (
	initialStackSize    = 512
	stackSlack          = 264 // >= 256 slots above frame base so *[256]Value window is always in-bounds
	maxFrames           = 1000000
	maxCCalls           = 200
	maxCCallsErrHandler = maxCCalls * 11 / 10 // extra headroom for xpcall error handlers (ldo.c)
)

// Intrinsic IDs for direct VM call-site inlining (zero slice/call overhead on hot stdlib).
const (
	intrNone uint8 = iota
	intrMathFloor
	intrMathCeil
	intrMathAbs
	intrMathSqrt
	intrMathSin
	intrMathCos
	intrMathMin
	intrMathMax
	intrType
	intrRawGet
	intrRawSet
	intrRawLen
	intrTableInsert
	intrTableRemove
	intrSelect
	intrNext
	intrIPairsIter
	intrStringByte
	intrStringLen
	intrPCall
	intrXPCall
	intrDoFile
	intrPairs
)

// NativeFunc represents a direct-Value Go builtin or host function descriptor.
// Static stdlib builtins live in the process-wide immutable staticNatives table;
// dynamic host/C closures are bridged per-State via ArenaNative + GoRefs.
type NativeFunc struct {
	Name        string
	Fn          func(s *State, args []Value) ([]Value, error)
	FastFn      func(s *State, args []Value) (Value, error) // optional zero-alloc single-return fast path
	Intrinsic   uint8
	NumUpvals   uint8   // logical C closure upvalue count (e.g. string.gmatch = 3)
	Collectable bool    // true for dynamic C closures (coroutine.wrap, gmatch, io.lines, utf8.codes)
	upvals      []Value // initial captured C closure upvalues copied into ArenaNative on allocation
}

func (nf *NativeFunc) callMulti(s *State, args []Value) ([]Value, error) {
	if nf.FastFn != nil {
		v, err := nf.FastFn(s, args)
		if err != nil {
			return nil, err
		}
		return []Value{v}, nil
	}
	return nf.Fn(s, args)
}

// Upvalue is a 48-byte zero-pointer (noscan, ptrdata == 0) Lua 5.5 open or closed upvalue
// stored inside State.arena. Open iff ownerRef != arena.NullRef.
type Upvalue struct {
	hdr      arena.GCHeader // 8B: Kind=gcKindUpvalue
	self     arena.ObjRef   // 4B: own ObjRef in State.arena
	ownerRef arena.ObjRef   // 4B: owning Thread ObjRef (NullRef when closed)
	closed   Value          // 16B: value when closed
	id       uint64         // 8B: lazily assigned non-owning debug.upvalueid token
	stackIdx int32          // 4B: stack index on owner thread when open
	_        uint32         // 4B pad -> 48B exact
}

const (
	_ = uint(48 - unsafe.Sizeof(Upvalue{}))
	_ = uint(unsafe.Sizeof(Upvalue{}) - 48)
)

var globalUpvalTokenID atomic.Uint64

func (u *Upvalue) tokenID() uint64 {
	if u.id == 0 {
		u.id = globalUpvalTokenID.Add(1)
	}
	return u.id
}

func (u *Upvalue) Get(s *State) Value {
	if u.ownerRef != arena.NullRef {
		return s.threadByRef(u.ownerRef).stack[u.stackIdx]
	}
	return u.closed
}

func (u *Upvalue) Set(s *State, v Value) {
	if u.ownerRef != arena.NullRef {
		s.threadByRef(u.ownerRef).stack[u.stackIdx] = v
		return
	}
	u.closed = v
	if v.IsGCObject() && u.hdr.IsBlack() {
		s.collector.Barrier(u.self, v.Ref())
	}
}

// ArenaProto is a 48-byte zero-pointer (noscan, ptrdata == 0) GC-managed prototype header
// stored inside State.arena. Its constants, inline caches, and child proto refs live in
// State.arena; the Go *boundProto descriptor lives in State.protos[protoIdx] and is cleared
// automatically when Collector sweeps this ArenaProto.
type ArenaProto struct {
	hdr       arena.GCHeader // 8B: Kind=gcKindProto
	self      arena.ObjRef   // 4B: own ObjRef in State.arena
	constsRef arena.ObjRef   // 4B: raw Arena []Value constants
	icRef     arena.ObjRef   // 4B: raw Arena []ICEntry inline caches
	subRef    arena.ObjRef   // 4B: raw Arena []arena.ObjRef child ArenaProtos
	protoIdx  uint32         // 4B: 1-based index into State.protos
	nConsts   uint32         // 4B
	nIC       uint32         // 4B
	nSub      uint32         // 4B
	_         uint64         // 8B pad -> 48B exact
}

const (
	_ = uint(48 - unsafe.Sizeof(ArenaProto{}))
	_ = uint(unsafe.Sizeof(ArenaProto{}) - 48)
)

// boundProto binds an immutable *bytecode.Proto to one State's Arena constants and IC slice.
// Owned strictly by its ArenaProto in State.arena so GC reclaims unused load() chunks cleanly.
type boundProto struct {
	proto     *bytecode.Proto
	insns     codeWords
	consts    []Value        // slice view directly into ArenaProto.constsRef (noscan!)
	icSlots   []int32
	ic        []ICEntry      // slice view directly into ArenaProto.icRef (noscan!)
	sub       []arena.ObjRef // slice view directly into ArenaProto.subRef (noscan!)
	arenaRef  arena.ObjRef   // owning ArenaProto ObjRef
	protoIdx  uint32
	goBytes   int64 // Go-side retained bytecode.Proto bytes accounted in State.extraMemBytes
	numParams uint8
	maxStack  uint8
	isVararg  bool
}

// Closure is a 32-byte zero-pointer (noscan, ptrdata == 0) Lua function closure stored in State.arena.
type Closure struct {
	hdr       arena.GCHeader // 8B: Kind=gcKindClosure
	self      arena.ObjRef   // 4B: own ObjRef in State.arena
	protoRef  arena.ObjRef   // 4B: ArenaProto ObjRef in State.arena
	upvalsRef arena.ObjRef   // 4B: raw Arena []arena.ObjRef (length == nUpvals)
	protoIdx  uint32         // 4B: cached ArenaProto.protoIdx for 1-load OP_CALL dispatch
	nUpvals   uint16         // 2B
	_         [6]byte        // 6B pad -> 32B exact
}

const (
	_ = uint(32 - unsafe.Sizeof(Closure{}))
	_ = uint(unsafe.Sizeof(Closure{}) - 32)
)

func (cl *Closure) upvalsSlice(a *arena.Arena) []arena.ObjRef {
	if cl.upvalsRef == arena.NullRef || cl.nUpvals == 0 {
		return nil
	}
	return arena.SliceFast[arena.ObjRef](a, cl.upvalsRef, 0, int(cl.nUpvals))
}

func (an *ArenaNative) upvalsSlice(a *arena.Arena) []Value {
	if an.upvalsRef == arena.NullRef || an.nUpvals == 0 {
		return nil
	}
	return arena.SliceFast[Value](a, an.upvalsRef, 0, int(an.nUpvals))
}

func (an *ArenaNative) upvalID(s *State, idx int) uint64 {
	total := max(int(an.nUpvals), int(an.numUpvals))
	if idx < 0 || idx >= total {
		return 0
	}
	if an.upvalIDsRef == arena.NullRef {
		ref, err := s.arena.AllocZeroed(total * 8)
		if err != nil {
			return 0
		}
		s.collector.AddDebt(int64(s.arena.SlotSize(ref)))
		an.upvalIDsRef = ref
	}
	ids := arena.SliceFast[uint64](s.arena, an.upvalIDsRef, 0, total)
	if ids[idx] == 0 {
		ids[idx] = globalUpvalTokenID.Add(1)
	}
	return ids[idx]
}

func (u *Userdata) uvalsSlice(a *arena.Arena) []Value {
	if u.uvalsRef == arena.NullRef || u.nUValues == 0 {
		return nil
	}
	return arena.SliceFast[Value](a, u.uvalsRef, 0, int(u.nUValues))
}

func (u *Userdata) gcRefsSlice(a *arena.Arena) []Value {
	if u.gcRefsRef == arena.NullRef || u.nGCRefs == 0 {
		return nil
	}
	return arena.SliceFast[Value](a, u.gcRefsRef, 0, int(u.nGCRefs))
}

// CompileFunc compiles Lua 5.5 source text into a verified *bytecode.Proto.
type CompileFunc func(name, src string) (*bytecode.Proto, error)

// Predefined Lua 5.5.1 registry indices (lua.h: LUA_RIDX_*).
const (
	ridxRef        = 1 // reserved for the reference mechanism
	ridxGlobals    = 2 // LUA_RIDX_GLOBALS
	ridxMainThread = 3 // LUA_RIDX_MAINTHREAD
)

// tableStub and threadStub are 16-byte noscan Arena GC headers bridging Go-heap
// Table and Thread structs into State.collector until Step 5d migrates them into Arena.
type tableStub struct {
	hdr  arena.GCHeader // 8B: Kind=gcKindTable
	self arena.ObjRef   // 4B
	idx  uint32         // 4B: 1-based index into State.tables
}

type threadStub struct {
	hdr  arena.GCHeader // 8B: Kind=gcKindThread
	self arena.ObjRef   // 4B
	idx  uint32         // 4B: 1-based index into State.threads
}

// State is a single-goroutine Lua 5.5.1 execution state (multiple States share immutable Protos
// and frozen stdlib strings, while each State owns an independent noscan Arena & Lua 5.5 Collector).
type State struct {
	// Per-State noscan Arena, true PUC-Rio Lua 5.5.1 Collector, and generational GoRefs bridge.
	arena     *arena.Arena
	collector *arena.Collector
	goRefs    *arena.GoRefs

	mainThreadRef arena.ObjRef
	curRef        arena.ObjRef // currently executing Thread (mainThread or resumed coroutine)
	registryRef   arena.ObjRef

	intern     stringInterner
	shapes     shapeRegistry
	protoEpoch uint64

	// Live boundProto table indexed by 1-based ArenaProto.protoIdx (cleared on ArenaProto sweep).
	protos       []*boundProto
	freeProtoIDs []uint32

	// Live Go-heap Table & Thread tables indexed by tableStub.idx / threadStub.idx (cleared on sweep).
	tables        []*Table
	freeTableIDs  []uint32
	threads       []*Thread
	freeThreadIDs []uint32

	// Weak/auxiliary GC tracking lists (noscan []ObjRef; maintained in PreSweep).
	hashedTables       []arena.ObjRef
	twups              []arena.ObjRef
	hasDeadHashKeys    bool
	preserveFramesInGC bool

	// Host-pinned references (analog of luaL_ref) & transient GC roots.
	pinnedRefs  map[int]Value
	nextRefID   int
	pinnedShape *Shape

	out       io.Writer
	errOut    io.Writer
	warnOn    bool
	startTime time.Time
	nCcalls   int

	compilerFn       CompileFunc
	compilerReaderFn func(name string, r io.Reader, opts ...compiler.Option) (*bytecode.Proto, error)

	// Cached immutable stdlib builtin identities for TFORCALL / pcall / debug fast paths.
	builtinNext           *NativeFunc
	builtinIPairsIter     *NativeFunc
	builtinDebugGetInfo   *NativeFunc
	builtinDebugTraceback *NativeFunc
	builtinDebugGetLocal  *NativeFunc
	builtinDebugSetLocal  *NativeFunc
	builtinDebugSetHook   *NativeFunc
	builtinCoroYield      *NativeFunc

	stringMetaRef  arena.ObjRef
	fileMetaRef    arena.ObjRef
	pkgRef         arena.ObjRef
	typeMetaRefs   [9]arena.ObjRef // per-type metatables (debug.setmetatable on primitives!)
	stdinInput     Value
	defaultInput   Value
	defaultOutput  Value
	openFiles      []*fileInner      // open non-std file handles flushed on os.exit / closed LIFO on State.Close
	immUpvalTokens map[uint32][]uint64 // lazy debug.upvalueid tokens for shared static builtins

	// Non-Arena Go-side memory accounting & per-State soft/hard memory limit (0 = unlimited).
	extraMemBytes int64
	shapeMemBytes int64
	tempBytes     int64
	memLimit      int64
	memExceeded   bool

	// Optional async interrupt flag, context cancellation, and VM instruction step budget;
	// zero overhead in fast (*State).run when all are unset!
	intrFlag       *atomic.Bool
	intrTicks      uint32
	ctx            context.Context
	insnLimit      int64
	insnCount      int64
	abortErr       error // pending uncatchable abort error (ctx / instruction limit)
	fsSearchers    int
	childExecDepth atomic.Int32 // >0 while os.execute child process is running (ignores CLI SIGINT)

	// Lua 5.5 xoshiro256** PRNG state.
	rng [4]uint64
}

func defaultCompileFunc(name, src string) (*bytecode.Proto, error) {
	return compiler.Compile(name, src)
}

func defaultCompileReaderFunc(name string, r io.Reader, opts ...compiler.Option) (*bytecode.Proto, error) {
	return compiler.CompileReader(name, r, opts...)
}

// Fast per-State Arena dereference helpers.
func (s *State) asString(v Value) *LuaString {
	if v.tag == tagFrozenString {
		return arena.AsFast[LuaString](s.intern.frozenArena, v.Ref())
	}
	return arena.AsFast[LuaString](s.arena, v.Ref())
}

func (s *State) tableByRef(ref arena.ObjRef) *Table {
	if ref == arena.NullRef {
		return nil
	}
	idx := arena.AsSmall[tableStub](s.arena, ref).idx
	if int(idx) < len(s.tables) {
		return s.tables[idx]
	}
	return nil
}

func (s *State) threadByRef(ref arena.ObjRef) *Thread {
	if ref == arena.NullRef {
		return nil
	}
	idx := arena.AsSmall[threadStub](s.arena, ref).idx
	if int(idx) < len(s.threads) {
		return s.threads[idx]
	}
	return nil
}

func (s *State) allocTableStub(t *Table) {
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(tableStub{})), gcKindTable)
	if err != nil {
		panic(err)
	}
	var idx uint32
	if n := len(s.freeTableIDs); n > 0 {
		idx = s.freeTableIDs[n-1]
		s.freeTableIDs = s.freeTableIDs[:n-1]
		s.tables[idx] = t
	} else {
		if len(s.tables) == 0 {
			s.tables = []*Table{nil}
		}
		idx = uint32(len(s.tables))
		s.tables = append(s.tables, t)
	}
	stub := arena.AsSmall[tableStub](s.arena, ref)
	stub.self = ref
	stub.idx = idx
	t.self = ref
}

func (s *State) allocThreadStub(th *Thread) {
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(threadStub{})), gcKindThread)
	if err != nil {
		panic(err)
	}
	var idx uint32
	if n := len(s.freeThreadIDs); n > 0 {
		idx = s.freeThreadIDs[n-1]
		s.freeThreadIDs = s.freeThreadIDs[:n-1]
		s.threads[idx] = th
	} else {
		if len(s.threads) == 0 {
			s.threads = []*Thread{nil}
		}
		idx = uint32(len(s.threads))
		s.threads = append(s.threads, th)
	}
	stub := arena.AsSmall[threadStub](s.arena, ref)
	stub.self = ref
	stub.idx = idx
	th.self = ref
}

func (s *State) addTableMem(t *Table, delta int64) {
	if delta <= 0 {
		return
	}
	t.memBytes += delta
	s.extraMemBytes += delta
	if s.collector != nil {
		s.collector.AddDebt(delta)
	}
}

func (s *State) subTableMem(t *Table, delta int64) {
	if delta <= 0 {
		return
	}
	if t.memBytes > delta {
		t.memBytes -= delta
	} else {
		delta = t.memBytes
		t.memBytes = 0
	}
	s.subExtraMem(delta)
}

func (s *State) addThreadMem(th *Thread, delta int64) {
	if delta <= 0 {
		return
	}
	th.memBytes += delta
	s.extraMemBytes += delta
	if s.collector != nil {
		s.collector.AddDebt(delta)
	}
}

func (s *State) subThreadMem(th *Thread, delta int64) {
	if delta <= 0 {
		return
	}
	if th.memBytes > delta {
		th.memBytes -= delta
	} else {
		delta = th.memBytes
		th.memBytes = 0
	}
	s.subExtraMem(delta)
}

func (s *State) newThread(stackCap, _ int) (*Thread, error) {
	return newThread(s, stackCap), nil
}

func (s *State) allocOpenUpval(th *Thread, stackIdx int) (*Upvalue, error) {
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(Upvalue{})), gcKindUpvalue)
	if err != nil {
		return nil, err
	}
	uv := arena.AsSmall[Upvalue](s.arena, ref)
	uv.self = ref
	uv.ownerRef = th.self
	uv.stackIdx = int32(stackIdx)
	return uv, nil
}

func (s *State) asTable(v Value) *Table {
	return s.tableByRef(v.Ref())
}

func (s *State) asClosure(v Value) *Closure {
	return arena.AsSmall[Closure](s.arena, v.Ref())
}

func (s *State) asThread(v Value) *Thread {
	return s.threadByRef(v.Ref())
}

func (s *State) asUserdata(v Value) *Userdata {
	return arena.AsSmall[Userdata](s.arena, v.Ref())
}

func (s *State) asArenaNative(v Value) *ArenaNative {
	return arena.AsSmall[ArenaNative](s.arena, v.Ref())
}

func (s *State) asNative(v Value) *NativeFunc {
	if v.tag == tagStaticNative {
		return lookupStaticNative(v.staticNativeIdx())
	}
	an := arena.AsSmall[ArenaNative](s.arena, v.Ref())
	raw, _ := s.goRefs.Get(an.goRef)
	nf, _ := raw.(*NativeFunc)
	return nf
}

func (s *State) asUpvalue(ref arena.ObjRef) *Upvalue {
	if ref == arena.NullRef {
		return nil
	}
	return arena.AsSmall[Upvalue](s.arena, ref)
}

func (s *State) asProto(ref arena.ObjRef) *ArenaProto {
	if ref == arena.NullRef {
		return nil
	}
	return arena.AsSmall[ArenaProto](s.arena, ref)
}

func (s *State) boundProtoOf(cl *Closure) *boundProto {
	return s.protos[cl.protoIdx]
}

func (s *State) mainThread() *Thread {
	return s.threadByRef(s.mainThreadRef)
}

func (s *State) cur() *Thread {
	return s.threadByRef(s.curRef)
}

func (s *State) registry() *Table {
	return s.tableByRef(s.registryRef)
}

func (s *State) fileMeta() *Table {
	return s.tableByRef(s.fileMetaRef)
}

func (s *State) pkgTbl() *Table {
	return s.tableByRef(s.pkgRef)
}

func (s *State) trackHashedTable(ref arena.ObjRef) {
	t := s.tableByRef(ref)
	if t != nil && t.flags&tblFlagTrackedHash == 0 {
		t.flags |= tblFlagTrackedHash
		s.hashedTables = append(s.hashedTables, ref)
	}
}

// Exported State-scoped value dereference helpers for embedding callers.
func (s *State) AsString(v Value) *LuaString {
	if !v.IsString() {
		return nil
	}
	return s.asString(v)
}

func (s *State) AsTable(v Value) *Table {
	if !v.IsTable() {
		return nil
	}
	return s.asTable(v)
}

func (s *State) AsClosure(v Value) *Closure {
	if !v.IsClosure() {
		return nil
	}
	return s.asClosure(v)
}

func (s *State) AsNative(v Value) *NativeFunc {
	if !v.IsNative() {
		return nil
	}
	return s.asNative(v)
}

func (s *State) AsThread(v Value) *Thread {
	if !v.IsThread() {
		return nil
	}
	return s.asThread(v)
}

func (s *State) AsUserdata(v Value) *Userdata {
	if !v.IsUserdata() {
		return nil
	}
	return s.asUserdata(v)
}

// NewState creates a new Lua 5.5.1 State with the complete standard library installed,
// cloned rapidly via Arena.CloneSnapshot() from the process-wide frozen stdlib template.
func NewState() *State {
	return cloneFromStdTemplate(false)
}

// NewStateNoEnv creates a new Lua 5.5.1 State ignoring LUA_PATH/LUA_CPATH environment variables (CLI -E).
func NewStateNoEnv() *State {
	return cloneFromStdTemplate(true)
}

// ArenaStats reports live memory and handle-table diagnostics for a State's noscan Arena.
type ArenaStats struct {
	AllocBytes       int64
	LiveObjects      int64
	ActivePages      int
	ActiveLargeSpans int
	BackingBytes     int64
	DynamicShortStrs int
	GoRefsLive       int
}

// ArenaStats returns a snapshot of this State's per-State noscan Arena and GoRefs usage.
func (s *State) ArenaStats() ArenaStats {
	var st ArenaStats
	if s.arena != nil {
		st.AllocBytes = s.arena.AllocBytes()
		st.LiveObjects = s.arena.LiveObjects()
		st.ActivePages = s.arena.ActivePages()
		st.ActiveLargeSpans = s.arena.ActiveLargeSpans()
		st.BackingBytes = s.arena.BackingBytes()
	}
	st.DynamicShortStrs = s.intern.LiveDynamicShort()
	if s.goRefs != nil {
		st.GoRefsLive = s.goRefs.LiveCount()
	}
	return st
}

// IsLive reports whether v refers to a currently live Arena-managed GC object in this State.
func (s *State) IsLive(v Value) bool {
	if s.arena == nil || !v.IsGCObject() {
		return false
	}
	return s.arena.IsLive(v.Ref())
}

// newBootstrapState builds a fresh State from scratch via initStdlib(); used once to construct stdTemplate.
func newBootstrapState() *State {
	a := arena.New()
	s := &State{
		arena:            a,
		goRefs:           arena.NewGoRefs(),
		intern:           newBootstrapStringInterner(),
		shapes:           newShapeRegistry(),
		protos:           []*boundProto{nil},
		tables:           []*Table{nil},
		threads:          []*Thread{nil},
		nextRefID:        1,
		out:              os.Stdout,
		errOut:           os.Stderr,
		startTime:        time.Now(),
		compilerFn:       defaultCompileFunc,
		compilerReaderFn: defaultCompileReaderFunc,
	}
	s.initCollector()
	s.seedRNG(makeRandomSeed(s), 0)

	th, err := s.newThread(initFastStackCap, initFastFrameCap)
	if err != nil {
		panic(err)
	}
	th.nny = 1 // main thread is never yieldable (L->nny = 1)
	s.mainThreadRef = th.self
	s.curRef = th.self

	env := s.NewDictTable(64)
	reg := s.NewDictTable(16)
	s.registryRef = reg.self
	reg.RawSetInt(s, ridxRef, FalseValue())
	reg.RawSetInt(s, ridxGlobals, TableValue(env))
	reg.RawSetInt(s, ridxMainThread, ThreadValue(th))
	s.initStdlib()
	s.shapes.frozenMaxID = s.shapes.nextID
	s.shapeMemBytes = 0
	return s
}

// SetMemoryLimit sets a per-State VM memory limit in bytes (<= 0 disables the limit).
// Allocations exceeding the limit trigger one emergency GC cycle before raising a recoverable
// Lua "not enough memory" error.
func (s *State) SetMemoryLimit(limit int64) {
	if limit < 0 {
		limit = 0
	}
	s.memLimit = limit
	s.memExceeded = false
}

// MemoryUsage returns the current VM-accounted memory usage in bytes.
func (s *State) MemoryUsage() int64 {
	var arenaBytes int64
	if s.arena != nil {
		arenaBytes = s.arena.AllocBytes()
	}
	n := arenaBytes + s.extraMemBytes + s.shapeMemBytes + s.tempBytes
	if n < 0 {
		return 0
	}
	return n
}

func (s *State) memError() *LuaError {
	return &LuaError{Val: StringValue(s.intern.Intern("not enough memory")), isMemErr: true}
}

func isMemError(err error) bool {
	if le, ok := err.(*LuaError); ok && le.isMemErr {
		return true
	}
	var le2 *LuaError
	return errors.As(err, &le2) && le2.isMemErr
}

// ConsumeMemErr returns and clears a pending deferred memory-limit error (or uncatchable abort error)
// from table mutation.
func (s *State) ConsumeMemErr() error {
	if s.abortErr != nil {
		err := s.abortErr
		if s.nCcalls == 0 {
			s.abortErr = nil
		}
		s.memExceeded = false
		return err
	}
	if s.memExceeded {
		s.memExceeded = false
		return s.memError()
	}
	return nil
}

// checkMem verifies that allocating delta bytes stays within hard safety bounds and s.memLimit.
// On limit pressure it runs an emergency GC cycle (without invoking __gc finalizers,
// matching PUC-Rio Lua's emergency collector).
func (s *State) checkMem(delta int64) error {
	const hardSingleAllocLimit = 1 << 30 // 1 GiB single allocation guard against Go OOM fatal
	if delta < 0 || delta > hardSingleAllocLimit {
		return s.memError()
	}
	if s.memLimit > 0 && s.MemoryUsage()+delta > s.memLimit {
		if delta > s.memLimit {
			return s.memError()
		}
		s.collectEmergency()
		if s.abortErr != nil {
			return s.abortErr
		}
		if s.MemoryUsage()+delta > s.memLimit {
			return s.memError()
		}
	}
	return nil
}

// checkMemWithRoots behaves like checkMem, temporarily rooting up to three live Values
// across emergency GC when allocation pressure triggers a collection.
func (s *State) checkMemWithRoots(delta int64, r1, r2, r3 Value) error {
	if s.memLimit > 0 && s.MemoryUsage()+delta > s.memLimit {
		if delta > s.memLimit {
			return s.memError()
		}
		var id1, id2, id3 int
		if isGCRootable(r1) {
			id1 = s.Ref(r1)
		}
		if isGCRootable(r2) {
			id2 = s.Ref(r2)
		}
		if isGCRootable(r3) {
			id3 = s.Ref(r3)
		}
		err := s.checkMem(delta)
		if id1 != 0 {
			s.Unref(id1)
		}
		if id2 != 0 {
			s.Unref(id2)
		}
		if id3 != 0 {
			s.Unref(id3)
		}
		return err
	}
	return s.checkMem(delta)
}

func isGCRootable(v Value) bool {
	return v.IsGCObject()
}

// ensureStackWithRoots grows th.stack to hold need slots while rooting extra and args
// across any emergency GC triggered when s.memLimit > 0. Zero overhead when stack fits.
func (s *State) ensureStackWithRoots(th *Thread, need int, extra Value, args []Value) error {
	if need+stackSlack <= len(th.stack) {
		return nil
	}
	if s.memLimit > 0 {
		var ids []int
		if isGCRootable(extra) {
			ids = append(ids, s.Ref(extra))
		}
		for _, a := range args {
			if isGCRootable(a) {
				ids = append(ids, s.Ref(a))
			}
		}
		err := th.ensureStack(s, need)
		for _, id := range ids {
			s.Unref(id)
		}
		return err
	}
	return th.ensureStack(s, need)
}

// ensureFrameCapWithRoots grows th.frames to hold need entries while rooting extra and args
// across any emergency GC triggered when s.memLimit > 0. Zero overhead when frames fit.
func (s *State) ensureFrameCapWithRoots(th *Thread, need, limit int, extra Value, args []Value) error {
	if need <= len(th.frames) {
		return nil
	}
	if s.memLimit > 0 {
		var ids []int
		if isGCRootable(extra) {
			ids = append(ids, s.Ref(extra))
		}
		for _, a := range args {
			if isGCRootable(a) {
				ids = append(ids, s.Ref(a))
			}
		}
		err := th.ensureFrameCap(s, need, limit)
		for _, id := range ids {
			s.Unref(id)
		}
		return err
	}
	return th.ensureFrameCap(s, need, limit)
}

// SetCompiler registers a source-to-bytecode compiler for load()/loadfile()/dofile()/require().
func (s *State) SetCompiler(fn CompileFunc) {
	s.compilerFn = fn
	s.compilerReaderFn = nil
}

// SetOutput redirects print() output (useful for differential testing and benchmarks).
func (s *State) SetOutput(w io.Writer) { s.out = w }

// SetErrOutput redirects warn() output.
func (s *State) SetErrOutput(w io.Writer) { s.errOut = w }

// SetWarnings enables or disables Lua warning emission (equivalent to warn("@on") / lua -W).
func (s *State) SetWarnings(on bool) { s.warnOn = on }

// EmitWarning emits a Lua warning if warnings are currently enabled (matching lua_warning).
func (s *State) EmitWarning(msg string) { s.emitWarning(msg) }

// SetGCGenerational switches this State's GC mode to generational (matching lua.c startup).
func (s *State) SetGCGenerational() {
	if s.collector != nil {
		s.collector.SetMode(arena.ModeGenMinor)
	}
}

// ToStringValue converts v to a Go string honoring __tostring / __name metamethods
// (matching PUC-Rio luaL_tolstring without consulting global _G.tostring).
func (s *State) ToStringValue(v Value) (string, error) { return s.toStringValue(v) }

// TracebackFromNative formats a Lua 5.5 stack traceback starting just below the given
// native function (skipping self if a frameNative was pushed by debug hooks).
func (s *State) TracebackFromNative(msg string, self *NativeFunc) string {
	th := s.cur()
	level := 0
	if s.topFrameIsNative(th, self) {
		level = 1
	}
	return s.formatTraceback(th, msg, true, level)
}

// SetInterruptFlag attaches an atomic interrupt flag polled periodically in runHooked
// (used by CLI SIGINT handling; leaves fast run() completely untouched when nil).
func (s *State) SetInterruptFlag(flag *atomic.Bool) { s.intrFlag = flag }

// InChildExec reports whether this State is currently waiting on an os.execute child process.
func (s *State) InChildExec() bool { return s.childExecDepth.Load() > 0 }

// SetContext attaches a context for cooperative VM cancellation / timeout.
func (s *State) SetContext(ctx context.Context) {
	if ctx != nil && ctx.Done() == nil {
		ctx = nil
	}
	s.ctx = ctx
	s.abortErr = nil
}

// SetInstructionLimit sets the maximum number of Lua VM instructions this State may execute
// before aborting with ErrInstructionLimit, and resets the instruction counter to 0.
func (s *State) SetInstructionLimit(limit int64) {
	if limit < 0 {
		limit = 0
	}
	s.insnLimit = limit
	s.insnCount = 0
	s.abortErr = nil
}

// ResetInstructionCount resets the executed instruction counter to 0.
func (s *State) ResetInstructionCount() {
	s.insnCount = 0
	s.abortErr = nil
}

// InstructionsExecuted returns the number of VM instructions counted while an instruction limit is active.
func (s *State) InstructionsExecuted() int64 { return s.insnCount }

func (s *State) hasInterruptControl() bool {
	return s.intrFlag != nil || s.ctx != nil || s.insnLimit > 0
}

func (s *State) checkNativeCancelStep() error {
	if s.abortErr != nil {
		return s.abortErr
	}
	if s.insnLimit > 0 {
		s.insnCount += 1024
		if s.insnCount > s.insnLimit {
			s.abortErr = ErrInstructionLimit
			return ErrInstructionLimit
		}
	}
	if s.ctx != nil {
		if err := s.ctx.Err(); err != nil {
			s.abortErr = err
			return err
		}
	}
	return nil
}

func (s *State) nativeCancelFunc() func() error {
	if s.ctx == nil && s.insnLimit <= 0 {
		return nil
	}
	return s.checkNativeCancelStep
}

func (s *State) dynamicNativeCancelFunc() func() error {
	return func() error {
		if s.ctx == nil && s.insnLimit <= 0 && s.abortErr == nil {
			return nil
		}
		return s.checkNativeCancelStep()
	}
}

// UserdataData returns the Go host payload stored on userdata u via State.goRefs.
func (s *State) UserdataData(u *Userdata) any {
	if u == nil || u.dataRef == arena.NullGoHandle || s.goRefs == nil {
		return nil
	}
	v, _ := s.goRefs.Get(u.dataRef)
	return v
}

// SetUserdataData updates the Go host payload stored on userdata u via State.goRefs.
func (s *State) SetUserdataData(u *Userdata, data any) {
	if u == nil || s.goRefs == nil {
		return
	}
	if u.dataRef != arena.NullGoHandle {
		s.goRefs.Free(u.dataRef)
		u.dataRef = arena.NullGoHandle
	}
	if data != nil {
		u.dataRef = s.goRefs.Alloc(data)
	}
}

// UserdataMeta returns the metatable Table of userdata u, or nil.
func (s *State) UserdataMeta(u *Userdata) *Table {
	if u == nil || u.metaRef == arena.NullRef {
		return nil
	}
	return s.tableByRef(u.metaRef)
}

// UserdataUValues returns the Lua user-value slice of userdata u.
func (s *State) UserdataUValues(u *Userdata) []Value {
	if u == nil {
		return nil
	}
	return u.uvalsSlice(s.arena)
}

// SetUserdataUValue sets user value idx on userdata u with a GC write barrier.
func (s *State) SetUserdataUValue(u *Userdata, idx int, v Value) {
	if u == nil || idx < 0 || idx >= int(u.nUValues) {
		return
	}
	u.uvalsSlice(s.arena)[idx] = v
	if v.IsGCObject() && u.hdr.IsBlack() {
		s.collector.Barrier(u.self, v.Ref())
	}
}

// SetUserdataGCRefs stores internal GC-traced Lua values on userdata u (e.g. io.lines file ref).
func (s *State) SetUserdataGCRefs(u *Userdata, refs []Value) error {
	if u == nil {
		return nil
	}
	if u.gcRefsRef != arena.NullRef {
		s.arena.Free(u.gcRefsRef)
		u.gcRefsRef = arena.NullRef
		u.nGCRefs = 0
	}
	if len(refs) == 0 {
		return nil
	}
	ref, err := s.arena.Alloc(len(refs) * int(unsafe.Sizeof(Value{})))
	if err != nil {
		return err
	}
	s.collector.AddDebt(int64(s.arena.SlotSize(ref)))
	copy(arena.SliceFast[Value](s.arena, ref, 0, len(refs)), refs)
	u.gcRefsRef = ref
	u.nGCRefs = uint32(len(refs))
	if u.hdr.IsBlack() {
		for _, v := range refs {
			if v.IsGCObject() {
				s.collector.Barrier(u.self, v.Ref())
			}
		}
	}
	return nil
}

// NativeValue wraps a Go *NativeFunc as a Lua Value owned by this State.
// Stdlib static builtins use the immutable process-wide static table; all other
// native callbacks allocate a per-State ArenaNative bridged via GoRefs.
func (s *State) NativeValue(nf *NativeFunc) Value {
	if nf == nil {
		return NilValue()
	}
	staticNativeMu.Lock()
	idx, ok := staticNativeBy[nf]
	staticNativeMu.Unlock()
	if ok {
		return staticNativeValue(idx)
	}
	v, err := s.allocArenaNative(nf)
	if err != nil {
		s.memExceeded = true
		return NilValue()
	}
	return v
}

// allocStaticNative registers a process-wide immutable stdlib builtin and returns its Value.
func (s *State) allocStaticNative(nf *NativeFunc) Value {
	return staticNativeValue(registerStaticNative(nf))
}

func (s *State) allocArenaNative(nf *NativeFunc) (Value, error) {
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(ArenaNative{})), gcKindNative)
	if err != nil {
		return NilValue(), err
	}
	an := arena.AsSmall[ArenaNative](s.arena, ref)
	an.self = ref
	an.goRef = s.goRefs.Alloc(nf)
	an.numUpvals = nf.NumUpvals
	an.intrinsic = nf.Intrinsic
	if nf.Collectable {
		an.hdr.Flags |= natFlagCollectable
	}
	if len(nf.upvals) > 0 {
		uref, uerr := s.arena.Alloc(len(nf.upvals) * int(unsafe.Sizeof(Value{})))
		if uerr != nil {
			return NilValue(), uerr
		}
		s.collector.AddDebt(int64(s.arena.SlotSize(uref)))
		copy(arena.SliceFast[Value](s.arena, uref, 0, len(nf.upvals)), nf.upvals)
		an.upvalsRef = uref
		an.nUpvals = uint16(len(nf.upvals))
	}
	return ArenaNativeValue(an), nil
}

// materializeLuaError caches a cloned Go string message on an uncaught *LuaError when it
// escapes to the Go host so err.Error() and FormatError(err) remain valid across subsequent
// Lua GC cycles or State.Close().
func (s *State) materializeLuaError(err error) error {
	if err == nil {
		return nil
	}
	if le, ok := err.(*LuaError); ok && !le.hasMsg {
		le.msg = s.FormatValue(le.Val)
		le.hasMsg = true
	}
	return err
}

// FormatError formats an error (calling __tostring on non-string error objects per lua.c:msghandler).
func (s *State) FormatError(err error) string {
	if err == nil {
		return ""
	}
	var le *LuaError
	if errors.As(err, &le) {
		v := le.Val
		if v.IsString() {
			if le.hasMsg {
				return le.msg
			}
			return strings.Clone(s.asString(v).Str())
		}
		if v.IsNumber() {
			return s.FormatValue(v)
		}
		if tm := s.getTMByObj(v, TMToString); !tm.IsNil() {
			res, terr := s.Call(tm, v)
			if terr == nil && len(res) > 0 && res[0].IsString() {
				return strings.Clone(s.asString(res[0]).Str())
			}
		}
		return fmt.Sprintf("error object is a %s value", v.TypeName())
	}
	return err.Error()
}

func (s *State) globalEnvValue() Value {
	reg := s.registry()
	if reg == nil {
		return NilValue()
	}
	return reg.RawGetInt(s, ridxGlobals)
}

// GlobalEnv returns the current global environment table at registry[LUA_RIDX_GLOBALS],
// or nil if Lua replaced registry[2] with a non-table value.
func (s *State) GlobalEnv() *Table {
	if v := s.globalEnvValue(); v.IsTable() {
		return s.asTable(v)
	}
	return nil
}

// Registry returns the State's Lua registry table (LUA_REGISTRYINDEX).
func (s *State) Registry() *Table { return s.registry() }

// PreloadTable returns the State's active registry _PRELOAD table, recreating it
// on the registry if Lua replaced or cleared debug.getregistry()._PRELOAD.
func (s *State) PreloadTable() *Table {
	reg := s.registry()
	if reg == nil {
		return nil
	}
	key := s.intern.Intern("_PRELOAD")
	if v := reg.RawGetStr(s, key); v.IsTable() {
		return s.asTable(v)
	}
	t := s.NewTable(0, 4)
	reg.RawSetStr(s, key, TableValue(t))
	return t
}

// Intern interns a Go string into this State's string table.
func (s *State) Intern(str string) *LuaString { return s.intern.Intern(str) }

// LookupString looks up str without populating the short-string intern cache.
func (s *State) LookupString(str string) (*LuaString, bool) {
	if len(str) <= maxShortStringLen {
		return s.intern.lookupExistingShort(str)
	}
	return newTransientLongString(str), true
}

// SetGlobal installs a global variable in the current global environment table.
func (s *State) SetGlobal(name string, v Value) {
	if env := s.GlobalEnv(); env != nil {
		env.RawSetStr(s, s.intern.Intern(name), v)
	}
}

// GetGlobal retrieves a global variable from the current global environment table
// without interning unseen short names.
func (s *State) GetGlobal(name string) Value {
	env := s.GlobalEnv()
	if env == nil {
		return NilValue()
	}
	ls, ok := s.LookupString(name)
	if !ok {
		return NilValue()
	}
	return env.RawGetStr(s, ls)
}

func (s *State) allocProtoSlot(bp *boundProto) uint32 {
	if n := len(s.freeProtoIDs); n > 0 {
		idx := s.freeProtoIDs[n-1]
		s.freeProtoIDs = s.freeProtoIDs[:n-1]
		s.protos[idx] = bp
		return idx
	}
	if len(s.protos) == 0 {
		s.protos = []*boundProto{nil}
	}
	idx := uint32(len(s.protos))
	s.protos = append(s.protos, bp)
	return idx
}

func (s *State) bindProto(p *bytecode.Proto) (*ArenaProto, *boundProto, error) {
	return s.bindProtoWithCache(p, nil)
}

func (s *State) bindProtoWithCache(p *bytecode.Proto, longStrs map[string]*LuaString) (*ArenaProto, *boundProto, error) {
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(ArenaProto{})), gcKindProto)
	if err != nil {
		return nil, nil, err
	}
	ap := arena.AsSmall[ArenaProto](s.arena, ref)
	ap.self = ref

	var consts []Value
	if len(p.Consts) > 0 {
		cref, cerr := s.arena.Alloc(len(p.Consts) * int(unsafe.Sizeof(Value{})))
		if cerr != nil {
			return nil, nil, cerr
		}
		s.collector.AddDebt(int64(s.arena.SlotSize(cref)))
		ap.constsRef = cref
		ap.nConsts = uint32(len(p.Consts))
		consts = arena.SliceFast[Value](s.arena, cref, 0, len(p.Consts))
		for i, c := range p.Consts {
			switch c.Kind {
			case bytecode.ConstNil:
				consts[i] = NilValue()
			case bytecode.ConstFalse:
				consts[i] = FalseValue()
			case bytecode.ConstTrue:
				consts[i] = TrueValue()
			case bytecode.ConstInt:
				consts[i] = IntValue(c.Int)
			case bytecode.ConstFloat:
				consts[i] = FloatValue(c.Float)
			case bytecode.ConstString:
				var ls *LuaString
				if len(c.Str) > maxShortStringLen && longStrs != nil {
					if cached, ok := longStrs[c.Str]; ok {
						ls = cached
					} else {
						ls = s.intern.Intern(c.Str)
						longStrs[c.Str] = ls
					}
				} else {
					ls = s.intern.Intern(c.Str)
				}
				consts[i] = StringValue(ls)
			}
		}
	}

	var ics []ICEntry
	if OptimizeEnabled && p.ICCount > 0 {
		ics = make([]ICEntry, p.ICCount)
		ap.nIC = uint32(p.ICCount)
	}

	var subs []arena.ObjRef
	if len(p.Protos) > 0 {
		sref, serr := s.arena.AllocZeroed(len(p.Protos) * int(unsafe.Sizeof(arena.ObjRef(0))))
		if serr != nil {
			return nil, nil, serr
		}
		s.collector.AddDebt(int64(s.arena.SlotSize(sref)))
		ap.subRef = sref
		ap.nSub = uint32(len(p.Protos))
		subs = arena.SliceFast[arena.ObjRef](s.arena, sref, 0, len(p.Protos))
		for i, sub := range p.Protos {
			subAP, _, berr := s.bindProtoWithCache(sub, longStrs)
			if berr != nil {
				return nil, nil, berr
			}
			subs[i] = subAP.self
		}
	}

	goBytes := protoGoBytes(p)
	bp := &boundProto{
		proto:     p,
		insns:     codeOf(p.Code),
		consts:    consts,
		icSlots:   p.ICSlot,
		ic:        ics,
		sub:       subs,
		arenaRef:  ref,
		goBytes:   goBytes,
		numParams: p.NumParams,
		maxStack:  p.MaxStackSize,
		isVararg:  p.IsVararg(),
	}
	idx := s.allocProtoSlot(bp)
	bp.protoIdx = idx
	ap.protoIdx = idx
	s.extraMemBytes += goBytes
	s.collector.AddDebt(goBytes)
	return ap, bp, nil
}

// protoGoBytes returns the Go-side retained storage for one immutable *bytecode.Proto
// (excluding Arena-allocated consts/IC/sub buffers already counted in Arena.AllocBytes).
func protoGoBytes(p *bytecode.Proto) int64 {
	if p == nil {
		return 0
	}
	n := int64(192) // Proto + boundProto headers
	n += int64(len(p.Source))
	n += int64(len(p.Code)) * 4
	n += int64(len(p.Consts)) * 24
	n += int64(len(p.Upvalues)) * 24
	for _, uv := range p.Upvalues {
		n += int64(len(uv.Name))
	}
	n += int64(len(p.Protos)) * 8
	n += int64(len(p.LineInfo))
	n += int64(len(p.AbsLineInfo)) * 8
	n += int64(len(p.LocVars)) * 24
	for _, lv := range p.LocVars {
		n += int64(len(lv.Name))
	}
	n += int64(len(p.ICSlot)) * 4
	return n
}

func protoOwnBytes(p *bytecode.Proto, withIC bool) int64 {
	if p == nil {
		return 0
	}
	n := protoGoBytes(p) + int64(unsafe.Sizeof(ArenaProto{})) + int64(len(p.Consts))*16 + int64(len(p.Protos))*4
	if withIC && p.ICCount > 0 {
		n += int64(p.ICCount) * int64(unsafe.Sizeof(ICEntry{}))
	}
	return n
}

// protoTreeBytes estimates the total retained storage for a Proto tree,
// deduplicating string constant payloads within the tree.
func protoTreeBytes(p *bytecode.Proto, withIC bool) int64 {
	if p == nil {
		return 0
	}
	var seen map[string]struct{}
	var walk func(cur *bytecode.Proto) int64
	walk = func(cur *bytecode.Proto) int64 {
		total := protoOwnBytes(cur, withIC)
		for _, c := range cur.Consts {
			if c.Kind == bytecode.ConstString {
				if seen == nil {
					seen = make(map[string]struct{}, 16)
				}
				if _, ok := seen[c.Str]; !ok {
					seen[c.Str] = struct{}{}
					total += int64(luaStringHeaderSize) + int64(len(c.Str))
				}
			}
		}
		for _, sub := range cur.Protos {
			total += walk(sub)
		}
		return total
	}
	return walk(p)
}

func (s *State) allocClosedUpval(val Value) (*Upvalue, error) {
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(Upvalue{})), gcKindUpvalue)
	if err != nil {
		return nil, err
	}
	uv := arena.AsSmall[Upvalue](s.arena, ref)
	uv.self = ref
	uv.closed = val
	return uv, nil
}

func (s *State) allocClosure(ap *ArenaProto, bp *boundProto, nUpvals int) (*Closure, error) {
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(Closure{})), gcKindClosure)
	if err != nil {
		return nil, err
	}
	cl := arena.AsSmall[Closure](s.arena, ref)
	cl.self = ref
	cl.protoRef = ap.self
	cl.protoIdx = bp.protoIdx
	if nUpvals > 0 {
		uref, uerr := s.arena.AllocZeroed(nUpvals * int(unsafe.Sizeof(arena.ObjRef(0))))
		if uerr != nil {
			return nil, uerr
		}
		s.collector.AddDebt(int64(s.arena.SlotSize(uref)))
		cl.upvalsRef = uref
		cl.nUpvals = uint16(nUpvals)
	}
	return cl, nil
}

// Load instantiates an immutable verified *bytecode.Proto as a top-level Closure with _ENV as upvalue 0.
func (s *State) Load(p *bytecode.Proto) (*Closure, error) {
	if s.memLimit > 0 {
		chunkBytes := int64(unsafe.Sizeof(Closure{})) + int64(len(p.Upvalues))*(int64(unsafe.Sizeof(Upvalue{}))+4) + protoTreeBytes(p, OptimizeEnabled)
		if err := s.checkMem(chunkBytes); err != nil {
			return nil, err
		}
	}
	ap, bp, err := s.bindProtoWithCache(p, make(map[string]*LuaString))
	if err != nil {
		return nil, err
	}
	cl, err := s.allocClosure(ap, bp, len(p.Upvalues))
	if err != nil {
		return nil, err
	}
	if cl.nUpvals > 0 {
		ups := cl.upvalsSlice(s.arena)
		u0, uerr := s.allocClosedUpval(s.globalEnvValue())
		if uerr != nil {
			return nil, uerr
		}
		ups[0] = u0.self
		for i := 1; i < len(ups); i++ {
			ui, uerr := s.allocClosedUpval(NilValue())
			if uerr != nil {
				return nil, uerr
			}
			ups[i] = ui.self
		}
	}
	return cl, nil
}

// UndumpAndLoad decodes, verifies, and loads a Lua 5.5.1 binary chunk.
func (s *State) UndumpAndLoad(chunk []byte) (*Closure, error) {
	p, err := s.undumpBytes(chunk)
	if err != nil {
		return nil, err
	}
	return s.Load(p)
}

// Call invokes a Lua closure or native function on the current Thread and returns its results.
func (s *State) Call(fn Value, args ...Value) ([]Value, error) {
	return s.callOnThread(s.cur(), fn, args)
}

// Call1 invokes fn(args...) and returns its first result (or NilValue if none) with zero
// return-slice allocation on the hot Lua/FastFn paths.
func (s *State) Call1(fn Value, args ...Value) (Value, error) {
	return s.call1OnThread(s.cur(), fn, args)
}

// CallProtected invokes fn(args...) in protected mode with message handler msgh
// (analogous to lua_pcall with a C message handler, without pushing a Lua xpcall wrapper frame).
func (s *State) CallProtected(fn Value, msgh Value, args ...Value) ([]Value, Value, bool, error) {
	return s.callProtectedMode(fn, msgh, !msgh.IsNil(), args, true, NilValue())
}

func (s *State) callProtectedMode(fn Value, msgh Value, isXP bool, args []Value, topProtected bool, nativeFn Value) ([]Value, Value, bool, error) {
	th := s.cur()
	if s.nCcalls == 0 {
		s.abortErr = nil
	}
	if s.abortErr != nil {
		err := s.abortErr
		if s.nCcalls == 0 {
			s.abortErr = nil
		}
		return nil, NilValue(), false, err
	}
	if s.ctx != nil {
		if err := s.ctx.Err(); err != nil {
			s.abortErr = err
			return nil, NilValue(), false, err
		}
	}
	if s.insnLimit > 0 && s.insnCount > s.insnLimit {
		s.abortErr = ErrInstructionLimit
		return nil, NilValue(), false, ErrInstructionLimit
	}
	cLimit := maxCCalls
	if th.inErrHandler {
		cLimit = maxCCallsErrHandler
	}
	if s.nCcalls >= cLimit {
		return nil, StringValue(s.intern.Intern("C stack overflow")), false, nil
	}
	s.nCcalls++
	th.nny++
	defer func() {
		th.nny--
		s.nCcalls--
		if s.nCcalls == 0 {
			s.abortErr = nil
		}
	}()
	savedTop := th.top
	base := savedTop
	frames := th.framesSlice(s.arena)
	for fi := range th.nframe {
		cf := &frames[fi]
		if cf.kind == frameLua && cf.cl != nil {
			if end := cf.base + int(s.boundProtoOf(cf.cl).maxStack); end > base {
				base = end
			}
		}
	}
	var nargs int
	if isXP {
		rootArgs := make([]Value, 2+len(args))
		rootArgs[0] = nativeFn
		rootArgs[1] = msgh
		copy(rootArgs[2:], args)
		if err := s.ensureStackWithRoots(th, base+3+len(args)+8, fn, rootArgs); err != nil {
			return nil, errorToLuaValue(s, err), false, nil
		}
		stack := th.stackSlice(s.arena)
		stack[base] = nativeFn
		stack[base+1] = fn
		stack[base+2] = msgh
		copy(stack[base+3:base+3+len(args)], args)
		th.top = base + 3 + len(args)
		nargs = 2 + len(args)
	} else {
		rootArgs := make([]Value, 1+len(args))
		rootArgs[0] = nativeFn
		copy(rootArgs[1:], args)
		if err := s.ensureStackWithRoots(th, base+2+len(args)+8, fn, rootArgs); err != nil {
			return nil, errorToLuaValue(s, err), false, nil
		}
		stack := th.stackSlice(s.arena)
		stack[base] = nativeFn
		stack[base+1] = fn
		copy(stack[base+2:base+2+len(args)], args)
		th.top = base + 2 + len(args)
		nargs = 1 + len(args)
	}
	setupEnd := base + 1 + nargs
	th.noteStack(setupEnd)
	entryDepth := th.nframe
	clearTop := func() {
		th.nframe = entryDepth
		stack := th.stackSlice(s.arena)
		fslice := th.framesSlice(s.arena)
		if entryDepth == 0 && th.openUpvalsCount() == 0 {
			if end := min(max(th.highStack, th.top, setupEnd), len(stack)); base < end {
				fillNil(stack[base:end])
			}
			if hf := min(th.highFrames, len(fslice)); hf > 0 {
				clear(fslice[:hf])
			}
			th.highStack = base
			th.highFrames = 0
		} else {
			if base < len(stack) {
				end := min(max(th.highStack, th.top, setupEnd), len(stack))
				if base < end {
					fillNil(stack[base:end])
				}
			}
			if hf := min(th.highFrames, len(fslice)); entryDepth < hf {
				clear(fslice[entryDepth:hf])
				th.highFrames = entryDepth
			}
		}
		th.top = savedTop
	}
	abortCleanup := func(uerr error) {
		s.abortErr = uerr
		th.discardTBCAtOrAbove(s, base)
		th.closeUpvals(s, base)
		clearTop()
	}
	pushed, err := s.pushProtectedCallMode(th, base, nargs, -1, isXP, entryDepth, topProtected)
	if err != nil {
		if errors.Is(err, errCoroCloseSelf) {
			return nil, NilValue(), false, err
		}
		if isUncatchableErr(err) {
			abortCleanup(err)
			return nil, NilValue(), false, err
		}
		th.discardTBCAtOrAbove(s, base)
		th.closeUpvals(s, base)
		clearTop()
		return nil, errorToLuaValue(s, err), false, nil
	}
	if pushed {
		runErr := s.runThread(th, entryDepth)
		if uerr := th.uncaughtErr(s); uerr != nil {
			runErr = uerr
			th.setUncaughtErr(s, nil)
		}
		if runErr == nil && s.abortErr != nil {
			runErr = s.abortErr
		}
		if runErr != nil {
			if errors.Is(runErr, errCoroCloseSelf) {
				return nil, NilValue(), false, runErr
			}
			if isUncatchableErr(runErr) {
				abortCleanup(runErr)
				return nil, NilValue(), false, runErr
			}
			th.discardTBCAtOrAbove(s, base)
			th.closeUpvals(s, base)
			clearTop()
			return nil, errorToLuaValue(s, runErr), false, nil
		}
	}
	if s.gcNeedsCheck() {
		s.gcCheckStep(th.top)
		if s.abortErr != nil {
			uerr := s.abortErr
			abortCleanup(uerr)
			return nil, NilValue(), false, uerr
		}
	}
	nres := th.top - base
	if nres <= 0 {
		clearTop()
		return nil, NilValue(), true, nil
	}
	stack := th.stackSlice(s.arena)
	if stack[base].IsFalsy() {
		ev := NilValue()
		if nres > 1 {
			ev = stack[base+1]
		}
		clearTop()
		return nil, ev, false, nil
	}
	var res []Value
	if nres > 1 {
		res = make([]Value, nres-1)
		copy(res, stack[base+1:th.top])
	}
	clearTop()
	return res, NilValue(), true, nil
}

func slicesOverlapValues(a, b []Value) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	aStart := uintptr(unsafe.Pointer(unsafe.SliceData(a)))
	aEnd := aStart + uintptr(len(a))*unsafe.Sizeof(Value{})
	bStart := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	bEnd := bStart + uintptr(len(b))*unsafe.Sizeof(Value{})
	return aStart < bEnd && bStart < aEnd
}

func (s *State) callOnThread(th *Thread, fn Value, args []Value) ([]Value, error) {
	if s.nCcalls == 0 {
		s.abortErr = nil
	}
	if s.abortErr != nil {
		return nil, s.abortErr
	}
	if s.ctx != nil {
		if err := s.ctx.Err(); err != nil {
			s.abortErr = err
			return nil, err
		}
	}
	if s.insnLimit > 0 && s.insnCount > s.insnLimit {
		s.abortErr = ErrInstructionLimit
		return nil, ErrInstructionLimit
	}
	cLimit := maxCCalls
	if th.inErrHandler {
		cLimit = maxCCallsErrHandler
	}
	if s.nCcalls >= cLimit {
		return nil, s.materializeLuaError(&LuaError{Val: StringValue(s.intern.Intern("C stack overflow"))})
	}
	s.nCcalls++
	th.nny++
	defer func() {
		th.nny--
		s.nCcalls--
		if s.nCcalls == 0 {
			s.abortErr = nil
		}
	}()

	var extraDepth uint8
	if !fn.IsClosure() && !fn.IsNative() {
		for depth := 1; depth <= 15; depth++ {
			tm := s.getTMByObj(fn, TMCall)
			if tm.IsNil() {
				le := &LuaError{Val: StringValue(s.intern.Intern(fmt.Sprintf("attempt to call a %s value", fn.TypeName())))}
				if s.nCcalls == 1 {
					_ = s.materializeLuaError(le)
				}
				return nil, le
			}
			all := make([]Value, len(args)+1)
			all[0] = fn
			copy(all[1:], args)
			fn = tm
			args = all
			extraDepth = uint8(depth)
			if fn.IsClosure() || fn.IsNative() {
				break
			}
		}
		if !fn.IsClosure() && !fn.IsNative() {
			le := &LuaError{Val: StringValue(s.intern.Intern("'__call' chain too long"))}
			if s.nCcalls == 1 {
				_ = s.materializeLuaError(le)
			}
			return nil, le
		}
	}
	if fn.IsNative() {
		savedTop := th.top
		base := savedTop
		frames := th.framesSlice(s.arena)
		for fi := range th.nframe {
			cf := &frames[fi]
			if cf.kind == frameLua && cf.cl != nil {
				if end := cf.base + int(s.boundProtoOf(cf.cl).maxStack); end > base {
					base = end
				}
			}
		}
		if err := s.ensureStackWithRoots(th, base+1+len(args)+8, fn, args); err != nil {
			return nil, err
		}
		stack := th.stackSlice(s.arena)
		stack[base] = fn
		copy(stack[base+1:base+1+len(args)], args)
		th.top = base + 1 + len(args)
		cleanupEnd := base + 1 + len(args)
		defer func() {
			st := th.stackSlice(s.arena)
			if end := min(cleanupEnd, len(st)); base < end {
				fillNil(st[base:end])
			}
			th.top = savedTop
		}()
		var (
			res []Value
			err error
		)
		nf := s.asNative(fn)
		isDirectProtected := (nf.Intrinsic == intrPCall && len(args) >= 1) ||
			(nf.Intrinsic == intrXPCall && len(args) >= 2 && args[1].IsFunction())
		if isDirectProtected {
			if nf.Intrinsic == intrXPCall {
				pres, errVal, ok, abortErr := s.callProtectedMode(stack[base+1], stack[base+2], true, stack[base+3:base+1+len(args)], false, fn)
				if abortErr != nil {
					err = abortErr
				} else if !ok {
					res = []Value{FalseValue(), errVal}
				} else {
					res = make([]Value, 1+len(pres))
					res[0] = TrueValue()
					copy(res[1:], pres)
				}
			} else {
				pres, errVal, ok, abortErr := s.callProtectedMode(stack[base+1], NilValue(), false, stack[base+2:base+1+len(args)], false, fn)
				if abortErr != nil {
					err = abortErr
				} else if !ok {
					res = []Value{FalseValue(), errVal}
				} else {
					res = make([]Value, 1+len(pres))
					res[0] = TrueValue()
					copy(res[1:], pres)
				}
			}
		} else if th.hookMask == 0 || th.inHook || th.suppressHook {
			prevCB := th.inCCallback
			th.inCCallback = true
			th.nativeArgsDepth++
			res, err = nf.callMulti(s, stack[base+1:base+1+len(args)])
			res = th.finishNativeArgs(s, res)
			th.inCCallback = prevCB
		} else {
			prevCB := th.inCCallback
			th.inCCallback = true
			res, _, err = s.callNativeHooked(th, fn, base, len(args), extraDepth, maxFrames+128)
			th.inCCallback = prevCB
		}
		if err == nil && len(res) > 0 && slicesOverlapValues(res, th.stackSlice(s.arena)) {
			cp := make([]Value, len(res))
			copy(cp, res)
			res = cp
		}
		if err == nil && s.gcNeedsCheck() {
			st := th.stackSlice(s.arena)
			fillNil(st[base:min(cleanupEnd, len(st))])
			th.top = base
			if len(res) > 0 {
				if serr := s.ensureStackWithRoots(th, base+len(res)+4, NilValue(), res); serr == nil {
					copy(th.stackSlice(s.arena)[base:base+len(res)], res)
					if end := base + len(res); end > cleanupEnd {
						cleanupEnd = end
					}
					th.top = base + len(res)
					s.gcCheckStep(th.top)
				} else if s.abortErr == nil {
					var refs []int
					for _, v := range res {
						if isGCRootable(v) {
							refs = append(refs, s.Ref(v))
						}
					}
					s.gcCheckStep(th.top)
					for _, id := range refs {
						s.Unref(id)
					}
				}
			} else {
				s.gcCheckStep(th.top)
			}
			if s.abortErr != nil {
				return nil, s.abortErr
			}
		}
		if s.nCcalls == 1 {
			err = s.materializeLuaError(err)
		}
		return res, err
	}

	prevCB := th.inCCallback
	th.inCCallback = false
	defer func() {
		th.inCCallback = prevCB
	}()

	cl := s.asClosure(fn)
	bp := s.boundProtoOf(cl)
	base := th.top
	if th.nframe > 0 {
		prev := &th.framesSlice(s.arena)[th.nframe-1]
		if prev.kind == frameLua && prev.cl != nil {
			if prevEnd := prev.base + int(s.boundProtoOf(prev.cl).maxStack); prevEnd > base {
				base = prevEnd
			}
		}
	}
	funcIdx := base
	argBase := funcIdx + 1
	if err := s.ensureStackWithRoots(th, argBase+max(len(args), int(bp.maxStack))+8, fn, args); err != nil {
		return nil, err
	}
	stack := th.stackSlice(s.arena)
	stack[funcIdx] = fn
	copy(stack[argBase:argBase+len(args)], args)
	th.top = argBase + len(args)
	th.noteStack(argBase + max(len(args), int(bp.maxStack)))

	entryDepth := th.nframe
	if err := th.pushLuaFrame(s, cl, funcIdx, len(args), -1); err != nil {
		return nil, err
	}
	if herr := s.fireLuaCallHook(th, cl, len(args)); herr != nil {
		th.nframe = entryDepth
		th.top = funcIdx
		if s.nCcalls == 1 {
			herr = s.materializeLuaError(herr)
		}
		return nil, herr
	}
	clearTopStack := func() {
		th.top = funcIdx
		if entryDepth == 0 && th.openUpvalsCount() == 0 {
			st := th.stackSlice(s.arena)
			if end := min(max(th.highStack, th.top), len(st)); funcIdx < end {
				fillNil(st[funcIdx:end])
			}
			fs := th.framesSlice(s.arena)
			if hf := min(th.highFrames, len(fs)); hf > 0 {
				clear(fs[:hf])
			}
			th.highStack = funcIdx
			th.highFrames = 0
		}
	}
	err := s.runThread(th, entryDepth)
	if uerr := th.uncaughtErr(s); uerr != nil {
		err = uerr
		th.setUncaughtErr(s, nil)
	}
	if err == nil && s.abortErr != nil {
		err = s.abortErr
	}
	if err != nil {
		if errors.Is(err, errYieldSentinel) || errors.Is(err, errCoroCloseSelf) {
			return nil, err
		}
		if isUncatchableErr(err) {
			s.abortErr = err
			th.discardTBCAtOrAbove(s, funcIdx)
			th.closeUpvals(s, funcIdx)
			th.nframe = entryDepth
			clearTopStack()
			return nil, err
		}
		var xpHandled bool
		isMem := isMemError(err)
		if le, ok := err.(*LuaError); ok && le.xpHandled {
			xpHandled = true
		}
		ev := s.runtimeErrorToValue(th, err)
		ev, _, raised := th.closeAllSync(s, funcIdx, ev, true)
		if raised {
			xpHandled = false
			isMem = false
		}
		if s.abortErr != nil {
			uerr := s.abortErr
			if !errors.Is(uerr, errCoroCloseSelf) {
				th.discardTBCAtOrAbove(s, funcIdx)
			}
			th.closeUpvals(s, funcIdx)
			th.nframe = entryDepth
			clearTopStack()
			return nil, uerr
		}
		th.nframe = entryDepth
		clearTopStack()
		le := &LuaError{Val: ev, xpHandled: xpHandled, isMemErr: isMem}
		if s.nCcalls == 1 {
			_ = s.materializeLuaError(le)
		}
		return nil, le
	}
	if s.gcNeedsCheck() {
		s.gcCheckStep(th.top)
		if s.abortErr != nil {
			clearTopStack()
			return nil, s.abortErr
		}
	}
	nres := th.top - funcIdx
	if nres <= 0 {
		clearTopStack()
		return nil, nil
	}
	res := make([]Value, nres)
	copy(res, th.stackSlice(s.arena)[funcIdx:th.top])
	clearTopStack()
	return res, nil
}

func (s *State) call1OnThread(th *Thread, fn Value, args []Value) (Value, error) {
	// Zero-allocation single-return fast path for direct Lua closure calls with no hooks/interrupts.
	if fn.IsClosure() && s.abortErr == nil && !s.hasInterruptControl() &&
		s.nCcalls < maxCCalls && th.hookMask == 0 && !th.inHook && !th.suppressHook {
		cl := s.asClosure(fn)
		bp := s.boundProtoOf(cl)
		if !bp.isVararg {
			s.nCcalls++
			th.nny++
			prevCB := th.inCCallback
			th.inCCallback = false

			base := th.top
			if th.nframe > 0 {
				prev := &th.framesSlice(s.arena)[th.nframe-1]
				if prev.kind == frameLua && prev.cl != nil {
					if prevEnd := prev.base + int(s.boundProtoOf(prev.cl).maxStack); prevEnd > base {
						base = prevEnd
					}
				}
			}
			funcIdx := base
			argBase := funcIdx + 1
			maxStack := int(bp.maxStack)
			need := argBase + max(len(args), maxStack) + 8
			if need+stackSlack <= len(th.stack) && th.nframe < len(th.frames) {
				stack := th.stackSlice(s.arena)
				stack[funcIdx] = fn
				copy(stack[argBase:argBase+len(args)], args)
				nparams := int(bp.numParams)
				if len(args) < nparams {
					fillNil(stack[argBase+len(args) : argBase+nparams])
				}
				if maxStack > nparams {
					fillNil(stack[argBase+nparams : argBase+maxStack])
				}
				frameEnd := argBase + maxStack
				th.top = frameEnd
				th.noteStack(max(frameEnd, argBase+len(args)))

				entryDepth := th.nframe
				frames := th.framesSlice(s.arena)
				nf := &frames[entryDepth]
				if nf.dirty {
					*nf = callFrame{}
				}
				nf.cl = cl
				nf.base = argBase
				nf.funcIdx = funcIdx
				nf.pc = 0
				nf.nresults = 1
				nf.oldPC = -1
				nf.isFinalizer = th.nextFrameFinalizer
				th.nextFrameFinalizer = false
				th.nframe = entryDepth + 1
				th.noteFrame(th.nframe)

				err := s.run(th, entryDepth)
				if uerr := th.uncaughtErr(s); uerr != nil {
					err = uerr
					th.setUncaughtErr(s, nil)
				}
				if err == nil && s.abortErr != nil {
					err = s.abortErr
				}

				clearTop := func() {
					th.top = funcIdx
					if entryDepth == 0 && th.openUpvalsCount() == 0 {
						st := th.stackSlice(s.arena)
						if end := min(max(th.highStack, th.top), len(st)); funcIdx < end {
							fillNil(st[funcIdx:end])
						}
						fs := th.framesSlice(s.arena)
						if hf := min(th.highFrames, len(fs)); hf > 0 {
							clear(fs[:hf])
						}
						th.highStack = funcIdx
						th.highFrames = 0
					}
				}

				var ret Value
				var retErr error
				if err != nil {
					if errors.Is(err, errYieldSentinel) || errors.Is(err, errCoroCloseSelf) {
						retErr = err
					} else if isUncatchableErr(err) {
						s.abortErr = err
						th.discardTBCAtOrAbove(s, funcIdx)
						th.closeUpvals(s, funcIdx)
						th.nframe = entryDepth
						clearTop()
						retErr = err
					} else {
						var xpHandled bool
						isMem := isMemError(err)
						if le, ok := err.(*LuaError); ok && le.xpHandled {
							xpHandled = true
						}
						ev := s.runtimeErrorToValue(th, err)
						ev, _, raised := th.closeAllSync(s, funcIdx, ev, true)
						if raised {
							xpHandled = false
							isMem = false
						}
						if s.abortErr != nil {
							uerr := s.abortErr
							if !errors.Is(uerr, errCoroCloseSelf) {
								th.discardTBCAtOrAbove(s, funcIdx)
							}
							th.closeUpvals(s, funcIdx)
							th.nframe = entryDepth
							clearTop()
							retErr = uerr
						} else {
							th.nframe = entryDepth
							clearTop()
							le := &LuaError{Val: ev, xpHandled: xpHandled, isMemErr: isMem}
							if s.nCcalls == 1 {
								_ = s.materializeLuaError(le)
							}
							retErr = le
						}
					}
				} else {
					if s.gcNeedsCheck() {
						s.gcCheckStep(th.top)
						if s.abortErr != nil {
							clearTop()
							retErr = s.abortErr
						}
					}
					if retErr == nil {
						if th.top > funcIdx {
							ret = th.stackSlice(s.arena)[funcIdx]
						} else {
							ret = NilValue()
						}
						clearTop()
					}
				}

				th.inCCallback = prevCB
				th.nny--
				s.nCcalls--
				if s.nCcalls == 0 {
					s.abortErr = nil
				}
				return ret, retErr
			}
			th.inCCallback = prevCB
			th.nny--
			s.nCcalls--
		}
	}
	res, err := s.callOnThread(th, fn, args)
	if err != nil || len(res) == 0 {
		return NilValue(), err
	}
	return res[0], nil
}

// runThread dispatches to the fast loop when no debug hooks or interrupt controls are active,
// or to the hooked variant when debug.sethook / ctx / instruction limits are active (zero cost in fast path!).
func (s *State) runThread(th *Thread, entryDepth int) error {
	if (th.hookMask != 0 && !th.inHook && !th.suppressHook) || s.hasInterruptControl() {
		return s.runHooked(th, entryDepth)
	}
	return s.run(th, entryDepth)
}

// getTM checks a table's metatable for metamethod tm using the 6-bit negative cache.
func (s *State) getTM(mt *Table, tm TM) Value {
	if mt == nil {
		return NilValue()
	}
	if tm < 6 && (mt.tmFlags&(1<<tm)) != 0 {
		return NilValue()
	}
	v := mt.RawGetStr(s, s.intern.tms[tm])
	if v.IsNil() && tm < 6 {
		mt.tmFlags |= 1 << tm
	}
	return v
}

func (s *State) getMetatableOf(v Value) *Table {
	switch {
	case v.IsTable():
		return s.asTable(v).Metatable(s)
	case v.IsUserdata():
		return s.UserdataMeta(s.asUserdata(v))
	case v.IsString():
		return s.tableByRef(s.stringMetaRef)
	default:
		return s.tableByRef(s.typeMetaRefs[v.TypeTag()])
	}
}

func (s *State) setMetatableOf(v Value, mt *Table) {
	var mtRef arena.ObjRef
	if mt != nil {
		mtRef = mt.self
	}
	switch {
	case v.IsTable():
		s.SetMetatable(s.asTable(v), mt)
	case v.IsUserdata():
		u := s.asUserdata(v)
		u.metaRef = mtRef
		if mt != nil {
			mt.markProto(s)
			s.registerFinalizable(u.self, mt)
			if u.hdr.IsBlack() {
				s.collector.Barrier(u.self, mtRef)
			}
		}
		s.protoEpoch++
	case v.IsString():
		s.stringMetaRef = mtRef
		s.typeMetaRefs[4] = mtRef
		if mt != nil {
			mt.markProto(s)
		}
		s.protoEpoch++
	default:
		s.typeMetaRefs[v.TypeTag()] = mtRef
		if mt != nil {
			mt.markProto(s)
		}
		s.protoEpoch++
	}
}

// GetMetatableOf returns the metatable of v (table, userdata, string, or type metatable), or nil.
func (s *State) GetMetatableOf(v Value) *Table { return s.getMetatableOf(v) }

// SetMetatableOf sets the metatable of v (table, userdata, string, or type metatable)
// and updates finalizer/prototype tracking. Passing nil clears the metatable.
func (s *State) SetMetatableOf(v Value, mt *Table) { s.setMetatableOf(v, mt) }

// TableLen evaluates the Lua length operation `#v` (honoring `__len` metamethods).
func (s *State) TableLen(v Value) (int64, error) { return s.tableLen(v) }

// TableGetInt evaluates `v[i]` (honoring `__index` metamethods).
func (s *State) TableGetInt(v Value, i int64) (Value, error) { return s.tableGetInt(v, i) }

// GetGlobalMM retrieves a global from registry[LUA_RIDX_GLOBALS], honoring `__index`
// metamethods (matching PUC-Rio `lua_getglobal`).
func (s *State) GetGlobalMM(name string) (Value, error) {
	return s.tableGetStrAny(s.globalEnvValue(), s.intern.Intern(name))
}

// TryIntern interns or wraps str as a *LuaString after checking newly allocated string
// storage against the State's memory limit.
func (s *State) TryIntern(str string) (*LuaString, error) {
	if ls, ok := s.intern.lookupExistingShort(str); ok {
		return ls, nil
	}
	if s.memLimit > 0 {
		need := int64(len(str)) + int64(luaStringHeaderSize)
		if len(str) > 0 && s.MemoryUsage()+need > s.memLimit {
			// Guard against str aliasing an unrooted Arena slot that emergency GC might sweep.
			str = strings.Clone(str)
		}
		if err := s.checkMem(need); err != nil {
			return nil, err
		}
	}
	return s.intern.Intern(str), nil
}

// TryAllocNative checks a dynamically allocated native closure against the State's memory
// limit, allocates it in State.arena, and returns it as a Value.
func (s *State) TryAllocNative(nf *NativeFunc) (Value, error) {
	if nf == nil {
		return NilValue(), nil
	}
	if s.memLimit > 0 {
		if err := s.checkMem(int64(unsafe.Sizeof(ArenaNative{})) + int64(len(nf.upvals))*16); err != nil {
			return NilValue(), err
		}
	}
	return s.allocArenaNative(nf)
}

// TryNewUserdata allocates a full noscan Lua userdata in State.arena carrying data via
// State.goRefs with nUValues Lua user values after checking State memory limits.
func (s *State) TryNewUserdata(data any, nUValues int) (Value, error) {
	if nUValues < 0 {
		nUValues = 0
	}
	if s.memLimit > 0 {
		if err := s.checkMem(int64(unsafe.Sizeof(Userdata{})) + int64(nUValues)*16); err != nil {
			return NilValue(), err
		}
	}
	ref, err := s.collector.Alloc(int(unsafe.Sizeof(Userdata{})), gcKindUserdata)
	if err != nil {
		return NilValue(), s.memError()
	}
	u := arena.AsSmall[Userdata](s.arena, ref)
	u.self = ref
	if data != nil {
		u.dataRef = s.goRefs.Alloc(data)
	}
	if nUValues > 0 {
		uref, uerr := s.arena.Alloc(nUValues * int(unsafe.Sizeof(Value{})))
		if uerr != nil {
			return NilValue(), s.memError()
		}
		s.collector.AddDebt(int64(s.arena.SlotSize(uref)))
		fillNil(arena.SliceFast[Value](s.arena, uref, 0, nUValues))
		u.uvalsRef = uref
		u.nUValues = uint32(nUValues)
	}
	return UserdataValue(u), nil
}

// SetGlobalMM assigns a global in registry[LUA_RIDX_GLOBALS], honoring `__newindex`
// metamethods (matching PUC-Rio `lua_setglobal`).
func (s *State) SetGlobalMM(name string, val Value) error {
	return s.tableSetStr(s.globalEnvValue(), s.intern.Intern(name), val)
}

// tableSetStr assigns keyStr = val on a Lua table honoring `__newindex` metamethods
// (matching PUC-Rio `lua_setfield`).
func (s *State) tableSetStr(tblVal Value, keyStr *LuaString, val Value) error {
	if tblVal.IsTable() {
		t := s.asTable(tblVal)
		if err := s.ensureLookupStrKeyHash(t, keyStr); err != nil {
			return err
		}
		if !t.RawGetStr(s, keyStr).IsNil() || !t.hasMeta() || s.getTM(t.Metatable(s), TMNewIndex).IsNil() {
			t.RawSetStr(s, keyStr, val)
			return s.ConsumeMemErr()
		}
	}
	cur := tblVal
	key := StringValue(keyStr)
	for loop := range 2000 {
		var tm Value
		if cur.IsTable() {
			t := s.asTable(cur)
			if loop > 0 {
				if err := s.ensureLookupKeyHash(t, key); err != nil {
					return err
				}
				if !t.RawGet(s, key).IsNil() {
					if err := t.RawSet(s, key, val); err != nil {
						return err
					}
					return s.ConsumeMemErr()
				}
			}
			tm = s.getTM(t.Metatable(s), TMNewIndex)
			if tm.IsNil() {
				if err := t.RawSet(s, key, val); err != nil {
					return err
				}
				return s.ConsumeMemErr()
			}
		} else {
			tm = s.getTMByObj(cur, TMNewIndex)
			if tm.IsNil() {
				return fmt.Errorf("attempt to index a %s value", cur.TypeName())
			}
		}
		if tm.IsFunction() {
			_, err := s.Call(tm, cur, key, val)
			return err
		}
		cur = tm
	}
	return fmt.Errorf("'__newindex' chain too long; possible loop")
}

func (s *State) getTMByObj(v Value, tm TM) Value {
	return s.getTM(s.getMetatableOf(v), tm)
}
