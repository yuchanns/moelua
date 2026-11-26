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

// NativeFunc represents a direct-Value Go builtin or host function.
type NativeFunc struct {
	Name        string
	Fn          func(s *State, args []Value) ([]Value, error)
	FastFn      func(s *State, args []Value) (Value, error) // optional zero-alloc single-return fast path
	Intrinsic   uint8
	NumUpvals   uint8   // logical C closure upvalue count (e.g. string.gmatch = 3)
	Collectable bool     // true for dynamic C closures (coroutine.wrap, gmatch, io.lines, utf8.codes); false for immutable light builtins
	gcMark      uint64   // Tier 1 GC mark epoch (only mutated when Collectable == true)
	upvals      []Value  // captured C closure upvalues (traced by Tier 1 GC and exposed to debug.getupvalue/setupvalue)
	upvalIDs    []uint64 // lazily assigned non-owning debug.upvalueid tokens
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

// Upvalue is a Lua 5.5 open or closed upvalue.
// An upvalue is open iff owner != nil; closed upvalues store their value in closed with owner == nil.
type Upvalue struct {
	owner    *Thread
	closed   Value
	stackIdx int
	id       uint64 // lazily assigned non-owning debug.upvalueid token
}

var globalUpvalTokenID atomic.Uint64

func (u *Upvalue) tokenID() uint64 {
	if u.id == 0 {
		u.id = globalUpvalTokenID.Add(1)
	}
	return u.id
}

func (u *Upvalue) Get() Value {
	if ow := u.owner; ow != nil {
		return ow.stack[u.stackIdx]
	}
	return u.closed
}

func (u *Upvalue) Set(v Value) {
	if ow := u.owner; ow != nil {
		ow.stack[u.stackIdx] = v
	} else {
		u.closed = v
	}
}

// boundProto binds an immutable *bytecode.Proto to one State's interned constants and IC slice.
// Ownership flows strictly from Closure -> boundProto so GC reclaims unused load() chunks cleanly.
// Hot proto metadata (insns, icSlots, numParams, maxStack, isVararg) is copied directly onto
// boundProto so OP_CALL avoids a secondary pointer chase through proto.
type boundProto struct {
	proto     *bytecode.Proto
	insns     codeWords
	consts    []Value
	strConsts []*LuaString // parallel to consts; non-nil when ConstString
	icSlots   []int32
	ic        []ICEntry
	sub       []*boundProto // parallel to proto.Protos (O(1) OP_CLOSURE lookup, zero protoCache leak)
	numParams uint8
	maxStack  uint8
	isVararg  bool
}

// Closure is a Lua function closure.
type Closure struct {
	bp     *boundProto
	upvals []*Upvalue
	gcMark uint64
}

// CompileFunc compiles Lua 5.5 source text into a verified *bytecode.Proto.
type CompileFunc func(name, src string) (*bytecode.Proto, error)

// Predefined Lua 5.5.1 registry indices (lua.h: LUA_RIDX_*).
const (
	ridxRef        = 1 // reserved for the reference mechanism
	ridxGlobals    = 2 // LUA_RIDX_GLOBALS
	ridxMainThread = 3 // LUA_RIDX_MAINTHREAD
)

// State is a single-goroutine Lua 5.5.1 execution state (multiple States share immutable Protos).
type State struct {
	mainThread *Thread
	cur        *Thread // currently executing Thread (mainThread or resumed coroutine)

	registry   *Table
	intern     stringInterner
	shapes     shapeRegistry
	protoEpoch uint64

	out       io.Writer
	errOut    io.Writer
	warnOn    bool
	startTime time.Time
	nCcalls   int

	compilerFn       CompileFunc
	compilerReaderFn func(name string, r io.Reader, opts ...compiler.Option) (*bytecode.Proto, error)

	// Cached builtin identities for TFORCALL / pcall / debug fast paths & standard libs.
	builtinNext           *NativeFunc
	builtinIPairsIter     *NativeFunc
	builtinDebugGetInfo   *NativeFunc
	builtinDebugTraceback *NativeFunc
	builtinDebugGetLocal  *NativeFunc
	builtinDebugSetLocal  *NativeFunc
	builtinDebugSetHook   *NativeFunc
	builtinCoroYield      *NativeFunc
	stringMeta        *Table
	fileMeta          *Table
	pkgTbl            *Table
	typeMetas         [9]*Table // per-type metatables (debug.setmetatable on primitives!)
	stdinInput        Value
	defaultInput      Value
	defaultOutput     Value
	openFiles         []*fileInner // open non-std file handles flushed on os.exit / closed LIFO on State.Close (does NOT pin *luaFile)
	coallocTables     []Table      // views into stateAlloc.tables swept on GC when unreachable
	coallocHashes     []tableHash  // views into stateAlloc.hashes swept on GC when unreachable
	immUpvalTokens    map[*NativeFunc][]uint64 // lazy debug.upvalueid tokens for shared non-collectable builtins

	// Two-Tier GC controller + per-State noscan Arena & generational GoRefs bridge.
	gc               gcState
	goRefs           *arena.GoRefs
	arenaSweepThresh int64

	// Optional per-State soft/hard memory limit in bytes (0 = unlimited).
	memLimit    int64
	memExceeded bool

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

// NewState creates a new Lua 5.5.1 State with the complete standard library installed,
// cloned rapidly from the process-wide frozen stdlib template.
func NewState() *State {
	return cloneFromStdTemplate(false)
}

// NewStateNoEnv creates a new Lua 5.5.1 State ignoring LUA_PATH/LUA_CPATH environment variables (CLI -E).
func NewStateNoEnv() *State {
	return cloneFromStdTemplate(true)
}

const defaultArenaSweepThresh = 256 * 1024

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
	if a := s.intern.arena; a != nil {
		st.AllocBytes = a.AllocBytes()
		st.LiveObjects = a.LiveObjects()
		st.ActivePages = a.ActivePages()
		st.ActiveLargeSpans = a.ActiveLargeSpans()
		st.BackingBytes = a.BackingBytes()
	}
	st.DynamicShortStrs = s.intern.LiveDynamicShort()
	if s.goRefs != nil {
		st.GoRefsLive = s.goRefs.LiveCount()
	}
	return st
}

// newBootstrapState builds a fresh State from scratch via initStdlib(); used once to construct stdTemplate.
func newBootstrapState() *State {
	s := &State{
		intern:           newStringInterner(arena.New()),
		shapes:           newShapeRegistry(),
		out:              os.Stdout,
		errOut:           os.Stderr,
		startTime:        time.Now(),
		gc:               newGCState(),
		arenaSweepThresh: defaultArenaSweepThresh,
		compilerFn:       defaultCompileFunc,
		compilerReaderFn: defaultCompileReaderFunc,
	}
	s.seedRNG(makeRandomSeed(s), 0)
	s.shapes.root = &s.shapes.rootShape
	s.mainThread = newThread(s, initialStackSize)
	s.mainThread.nny = 1 // main thread is never yieldable (L->nny = 1)
	s.cur = s.mainThread
	env := s.NewDictTable(64)
	s.registry = s.NewDictTable(16)
	s.registry.RawSetInt(s, ridxRef, FalseValue())
	s.registry.RawSetInt(s, ridxGlobals, TableValue(env))
	s.registry.RawSetInt(s, ridxMainThread, ThreadValue(s.mainThread))
	s.initStdlib()
	s.gc.tier1 = false // Stdlib internal setup (e.g. fileMeta.__gc) must not activate Tier 1!
	return s
}

// SetMemoryLimit sets a per-State VM memory limit in bytes (<= 0 disables the limit).
// Allocations exceeding the limit trigger one full GC cycle before raising a recoverable
// Lua "not enough memory" error.
func (s *State) SetMemoryLimit(limit int64) {
	if limit < 0 {
		limit = 0
	}
	wasUnlimited := s.memLimit == 0
	s.memLimit = limit
	s.memExceeded = false
	if limit > 0 && wasUnlimited {
		s.recountLiveBytes()
	}
}

// MemoryUsage returns the current VM-accounted memory usage in bytes.
func (s *State) MemoryUsage() int64 {
	return s.gc.allocBytes + s.gc.tempBytes
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
// On limit pressure it runs an emergency GC trace (without invoking __gc finalizers,
// matching PUC-Rio Lua's emergency collector).
func (s *State) checkMem(delta int64) error {
	const hardSingleAllocLimit = 1 << 30 // 1 GiB single allocation guard against Go OOM fatal
	if delta < 0 || delta > hardSingleAllocLimit {
		return s.memError()
	}
	if s.memLimit > 0 && s.gc.allocBytes+s.gc.tempBytes+delta > s.memLimit {
		s.collectEmergency()
		if s.abortErr != nil {
			return s.abortErr
		}
		if s.gc.allocBytes+s.gc.tempBytes+delta > s.memLimit {
			return s.memError()
		}
	}
	return nil
}

// checkMemAndAccount checks and accounts delta bytes when s.memLimit > 0 (or single-alloc guard).
func (s *State) checkMemAndAccount(delta int64) error {
	if err := s.checkMem(delta); err != nil {
		return err
	}
	if s.memLimit > 0 || s.gc.tier1 {
		s.gc.allocBytes += delta
		s.gcAddDebt(delta)
	}
	return nil
}

// checkMemAndAccountWithRoots behaves like checkMemAndAccount, temporarily rooting up to three
// live Values across emergency GC when allocation pressure triggers a recount (so host-held
// keys/values/tables not yet linked into _G are never under-counted or collected).
func (s *State) checkMemAndAccountWithRoots(delta int64, r1, r2, r3 Value) error {
	if s.memLimit > 0 && s.gc.allocBytes+s.gc.tempBytes+delta > s.memLimit {
		var id1, id2, id3 int
		if !r1.IsNil() {
			id1 = s.Ref(r1)
		}
		if !r2.IsNil() {
			id2 = s.Ref(r2)
		}
		if !r3.IsNil() {
			id3 = s.Ref(r3)
		}
		err := s.checkMemAndAccount(delta)
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
	return s.checkMemAndAccount(delta)
}

func isGCRootable(v Value) bool {
	return !v.IsNil() && !v.IsBool() && !v.IsInt() && !v.IsFloat() && !v.IsLightUD()
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
		err := th.ensureStack(need)
		for _, id := range ids {
			s.Unref(id)
		}
		return err
	}
	return th.ensureStack(need)
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
		err := th.ensureFrameCap(need, limit)
		for _, id := range ids {
			s.Unref(id)
		}
		return err
	}
	return th.ensureFrameCap(need, limit)
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
func (s *State) SetGCGenerational() { s.gc.mode = gcModeGenerational }

// ToStringValue converts v to a Go string honoring __tostring / __name metamethods
// (matching PUC-Rio luaL_tolstring without consulting global _G.tostring).
func (s *State) ToStringValue(v Value) (string, error) { return s.toStringValue(v) }

// TracebackFromNative formats a Lua 5.5 stack traceback starting just below the given
// native function (skipping self if a frameNative was pushed by debug hooks).
func (s *State) TracebackFromNative(msg string, self *NativeFunc) string {
	level := 0
	if topFrameIsNative(s.cur, self) {
		level = 1
	}
	return s.formatTraceback(s.cur, msg, true, level)
}

// SetInterruptFlag attaches an atomic interrupt flag polled periodically in runHooked
// (used by CLI SIGINT handling; leaves fast run() completely untouched when nil).
func (s *State) SetInterruptFlag(flag *atomic.Bool) { s.intrFlag = flag }

// InChildExec reports whether this State is currently waiting on an os.execute child process.
func (s *State) InChildExec() bool { return s.childExecDepth.Load() > 0 }

// SetContext attaches a context for cooperative VM cancellation / timeout.
// Passing nil or a non-cancelable context (ctx.Done() == nil, e.g. context.Background())
// clears the context and keeps the zero-overhead fast dispatch loop active.
func (s *State) SetContext(ctx context.Context) {
	if ctx != nil && ctx.Done() == nil {
		ctx = nil
	}
	s.ctx = ctx
	s.abortErr = nil
}

// SetInstructionLimit sets the maximum number of Lua VM instructions this State may execute
// before aborting with ErrInstructionLimit, and resets the instruction counter to 0.
// Passing limit <= 0 disables the limit and restores the zero-overhead fast dispatch loop.
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

// nativeCancelFunc returns nil when no Go context or instruction budget is active (zero allocation),
// or a cooperative callback polled during potentially long-running native operations
// (such as string pattern backtracking). CLI SIGINT (intrFlag) intentionally follows PUC-Rio
// lua.c's two-SIGINT convention for unbreakable native searches (testes/main.lua:580-596).
func (s *State) nativeCancelFunc() func() error {
	if s.ctx == nil && s.insnLimit <= 0 {
		return nil
	}
	return s.checkNativeCancelStep
}

// dynamicNativeCancelFunc returns a cooperative cancellation callback that consults the State's
// current context and instruction limit on every poll (used by long-lived iterators such as
// string.gmatch so limits attached after iterator creation are still enforced).
func (s *State) dynamicNativeCancelFunc() func() error {
	return func() error {
		if s.ctx == nil && s.insnLimit <= 0 && s.abortErr == nil {
			return nil
		}
		return s.checkNativeCancelStep()
	}
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
			return strings.Clone(v.AsString().Str())
		}
		if v.IsNumber() {
			return FormatValue(v)
		}
		if tm := s.getTMByObj(v, TMToString); !tm.IsNil() {
			res, terr := s.Call(tm, v)
			if terr == nil && len(res) > 0 && res[0].IsString() {
				return strings.Clone(res[0].AsString().Str())
			}
		}
		return fmt.Sprintf("error object is a %s value", v.TypeName())
	}
	return err.Error()
}

func (s *State) globalEnvValue() Value {
	if s.registry == nil {
		return NilValue()
	}
	return s.registry.RawGetInt(ridxGlobals)
}

// GlobalEnv returns the current global environment table at registry[LUA_RIDX_GLOBALS],
// or nil if Lua replaced registry[2] with a non-table value.
func (s *State) GlobalEnv() *Table {
	if v := s.globalEnvValue(); v.IsTable() {
		return v.AsTable()
	}
	return nil
}

// Registry returns the State's Lua registry table (LUA_REGISTRYINDEX).
func (s *State) Registry() *Table { return s.registry }

// PreloadTable returns the State's active registry _PRELOAD table, recreating it
// on the registry if Lua replaced or cleared debug.getregistry()._PRELOAD.
func (s *State) PreloadTable() *Table {
	if s.registry == nil {
		return nil
	}
	key := s.intern.Intern("_PRELOAD")
	if v := s.registry.RawGetStr(key); v.IsTable() {
		return v.AsTable()
	}
	t := s.NewTable(0, 4)
	s.registry.RawSetStr(s, key, TableValue(t))
	return t
}

// Intern interns a Go string into this State's string table.
func (s *State) Intern(str string) *LuaString { return s.intern.Intern(str) }

// LookupString looks up str without populating the short-string intern cache.
// For short strings (<= 40 bytes) not yet interned in this State, returns (nil, false).
// For long strings (> 40 bytes), returns a transient *LuaString wrapper suitable for table lookup.
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
	return env.RawGetStr(ls)
}

func (s *State) bindProto(p *bytecode.Proto) *boundProto {
	return s.bindProtoWithCache(p, nil)
}

func (s *State) bindProtoWithCache(p *bytecode.Proto, longStrs map[string]*LuaString) *boundProto {
	bp := &boundProto{
		proto:     p,
		insns:     codeOf(p.Code),
		consts:    make([]Value, len(p.Consts)),
		strConsts: make([]*LuaString, len(p.Consts)),
		icSlots:   p.ICSlot,
		numParams: p.NumParams,
		maxStack:  p.MaxStackSize,
		isVararg:  p.IsVararg(),
	}
	for i, c := range p.Consts {
		switch c.Kind {
		case bytecode.ConstNil:
			bp.consts[i] = NilValue()
		case bytecode.ConstFalse:
			bp.consts[i] = FalseValue()
		case bytecode.ConstTrue:
			bp.consts[i] = TrueValue()
		case bytecode.ConstInt:
			bp.consts[i] = IntValue(c.Int)
		case bytecode.ConstFloat:
			bp.consts[i] = FloatValue(c.Float)
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
			bp.consts[i] = StringValue(ls)
			bp.strConsts[i] = ls
		}
	}
	if OptimizeEnabled && p.ICCount > 0 {
		bp.ic = make([]ICEntry, p.ICCount)
	}
	if len(p.Protos) > 0 {
		bp.sub = make([]*boundProto, len(p.Protos))
		for i, sub := range p.Protos {
			bp.sub[i] = s.bindProtoWithCache(sub, longStrs)
		}
	}
	return bp
}

// protoOwnBytes estimates the VM-owned retained storage for one Proto and its
// boundProto wrapper (excluding child protos and interned string payloads).
func protoOwnBytes(p *bytecode.Proto, withIC bool) int64 {
	if p == nil {
		return 0
	}
	n := int64(256) // Proto + boundProto headers
	n += int64(len(p.Source))
	n += int64(len(p.Code)) * 4        // []Instruction (uint32)
	n += int64(len(p.Consts)) * 48     // []Constant + boundProto.consts + strConsts
	n += int64(len(p.Upvalues)) * 24   // []UpvalDesc
	for _, uv := range p.Upvalues {
		n += int64(len(uv.Name))
	}
	n += int64(len(p.Protos)) * 16     // []*Proto + []*boundProto
	n += int64(len(p.LineInfo))        // []int8
	n += int64(len(p.AbsLineInfo)) * 8 // []AbsLineInfo
	n += int64(len(p.LocVars)) * 24    // []LocVar
	for _, lv := range p.LocVars {
		n += int64(len(lv.Name))
	}
	n += int64(len(p.ICSlot)) * 4 // []int32
	if withIC && p.ICCount > 0 {
		n += int64(p.ICCount) * 64 // []ICEntry
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
					total += 48 + int64(len(c.Str))
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

// Load instantiates an immutable verified *bytecode.Proto as a top-level Closure with _ENV as upvalue 0.
func (s *State) Load(p *bytecode.Proto) (*Closure, error) {
	var chunkBytes int64
	if s.memLimit > 0 || s.gc.tier1 {
		chunkBytes = int64(64+len(p.Upvalues)*24) + protoTreeBytes(p, OptimizeEnabled)
		if s.memLimit > 0 {
			if err := s.checkMem(chunkBytes); err != nil {
				return nil, err
			}
		}
	}
	bp := s.bindProtoWithCache(p, make(map[string]*LuaString))
	cl := &Closure{
		bp:     bp,
		upvals: make([]*Upvalue, len(p.Upvalues)),
	}
	if len(cl.upvals) > 0 {
		cl.upvals[0] = &Upvalue{closed: s.globalEnvValue()}
		for i := 1; i < len(cl.upvals); i++ {
			cl.upvals[i] = &Upvalue{closed: NilValue()}
		}
	}
	if chunkBytes > 0 {
		s.gc.allocBytes += chunkBytes
		s.gcAddDebt(chunkBytes)
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
	return s.callOnThread(s.cur, fn, args)
}

// Call1 invokes fn(args...) and returns its first result (or NilValue if none) with zero
// return-slice allocation on the hot Lua/FastFn paths.
func (s *State) Call1(fn Value, args ...Value) (Value, error) {
	return s.call1OnThread(s.cur, fn, args)
}

// CallProtected invokes fn(args...) in protected mode with message handler msgh
// (analogous to lua_pcall with a C message handler, without pushing a Lua xpcall wrapper frame).
// Returns (results, errVal, ok, abortErr).
func (s *State) CallProtected(fn Value, msgh Value, args ...Value) ([]Value, Value, bool, error) {
	return s.callProtectedMode(fn, msgh, !msgh.IsNil(), args, true, NilValue())
}

func (s *State) callProtectedMode(fn Value, msgh Value, isXP bool, args []Value, topProtected bool, nativeFn Value) ([]Value, Value, bool, error) {
	th := s.cur
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
	for fi := range th.nframe {
		cf := &th.frames[fi]
		if cf.kind == frameLua && cf.cl != nil {
			if end := cf.base + int(cf.cl.bp.proto.MaxStackSize); end > base {
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
		th.stack[base] = nativeFn
		th.stack[base+1] = fn
		th.stack[base+2] = msgh
		copy(th.stack[base+3:base+3+len(args)], args)
		th.top = base + 3 + len(args)
		nargs = 2 + len(args)
	} else {
		rootArgs := make([]Value, 1+len(args))
		rootArgs[0] = nativeFn
		copy(rootArgs[1:], args)
		if err := s.ensureStackWithRoots(th, base+2+len(args)+8, fn, rootArgs); err != nil {
			return nil, errorToLuaValue(s, err), false, nil
		}
		th.stack[base] = nativeFn
		th.stack[base+1] = fn
		copy(th.stack[base+2:base+2+len(args)], args)
		th.top = base + 2 + len(args)
		nargs = 1 + len(args)
	}
	setupEnd := base + 1 + nargs
	th.noteStack(setupEnd)
	entryDepth := th.nframe
	clearTop := func() {
		th.nframe = entryDepth
		if entryDepth == 0 && len(th.openUpvals) == 0 {
			if end := min(max(th.highStack, th.top, setupEnd), len(th.stack)); base < end {
				fillNil(th.stack[base:end])
			}
			if hf := min(th.highFrames, len(th.frames)); hf > 0 {
				clear(th.frames[:hf])
			}
			th.highStack = base
			th.highFrames = 0
			th.clearAbandonedInitStorage()
		} else {
			if base < len(th.stack) {
				end := min(max(th.highStack, th.top, setupEnd), len(th.stack))
				if base < end {
					fillNil(th.stack[base:end])
				}
			}
			if hf := min(th.highFrames, len(th.frames)); entryDepth < hf {
				clear(th.frames[entryDepth:hf])
				th.highFrames = entryDepth
			}
		}
		th.top = savedTop
	}
	abortCleanup := func(uerr error) {
		s.abortErr = uerr
		th.discardTBCAtOrAbove(base)
		th.closeUpvals(base)
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
		th.discardTBCAtOrAbove(base)
		th.closeUpvals(base)
		clearTop()
		return nil, errorToLuaValue(s, err), false, nil
	}
	if pushed {
		runErr := s.runThread(th, entryDepth)
		if th.uncaughtErr != nil {
			runErr = th.uncaughtErr
			th.uncaughtErr = nil
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
			th.discardTBCAtOrAbove(base)
			th.closeUpvals(base)
			clearTop()
			return nil, errorToLuaValue(s, runErr), false, nil
		}
	}
	if s.gcNeedsCheck() {
		s.gcCheckDebt(th.top)
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
	if th.stack[base].IsFalsy() {
		ev := NilValue()
		if nres > 1 {
			ev = th.stack[base+1]
		}
		clearTop()
		return nil, ev, false, nil
	}
	var res []Value
	if nres > 1 {
		res = make([]Value, nres-1)
		copy(res, th.stack[base+1:th.top])
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
		return nil, &LuaError{Val: StringValue(s.intern.Intern("C stack overflow"))}
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
				return nil, &LuaError{Val: StringValue(s.intern.Intern(fmt.Sprintf("attempt to call a %s value", fn.TypeName())))}
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
			return nil, &LuaError{Val: StringValue(s.intern.Intern("'__call' chain too long"))}
		}
	}
	if fn.IsNative() {
		// Always root native callee and arguments on the active thread stack across the call
		// so GC / finalizers cannot prematurely collect live arguments even when debug hooks are off.
		savedTop := th.top
		base := savedTop
		for fi := range th.nframe {
			cf := &th.frames[fi]
			if cf.kind == frameLua && cf.cl != nil {
				if end := cf.base + int(cf.cl.bp.proto.MaxStackSize); end > base {
					base = end
				}
			}
		}
		if err := s.ensureStackWithRoots(th, base+1+len(args)+8, fn, args); err != nil {
			return nil, err
		}
		th.stack[base] = fn
		copy(th.stack[base+1:base+1+len(args)], args)
		th.top = base + 1 + len(args)
		cleanupEnd := base + 1 + len(args)
		defer func() {
			if end := min(cleanupEnd, len(th.stack)); base < end {
				fillNil(th.stack[base:end])
			}
			th.top = savedTop
		}()
		var (
			res []Value
			err error
		)
		nf := fn.AsNative()
		isDirectProtected := (nf.Intrinsic == intrPCall && len(args) >= 1) ||
			(nf.Intrinsic == intrXPCall && len(args) >= 2 && args[1].IsFunction())
		if isDirectProtected {
			if nf.Intrinsic == intrXPCall {
				pres, errVal, ok, abortErr := s.callProtectedMode(th.stack[base+1], th.stack[base+2], true, th.stack[base+3:base+1+len(args)], false, fn)
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
				pres, errVal, ok, abortErr := s.callProtectedMode(th.stack[base+1], NilValue(), false, th.stack[base+2:base+1+len(args)], false, fn)
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
			res, err = nf.callMulti(s, th.stack[base+1:base+1+len(args)])
			res = th.finishNativeArgs(res)
			th.inCCallback = prevCB
		} else {
			prevCB := th.inCCallback
			th.inCCallback = true
			res, _, err = s.callNativeHooked(th, fn, base, len(args), extraDepth, maxFrames+128)
			th.inCCallback = prevCB
		}
		if err == nil && len(res) > 0 && (slicesOverlapValues(res, th.stack) || slicesOverlapValues(res, th.initStack)) {
			cp := make([]Value, len(res))
			copy(cp, res)
			res = cp
		}
		if err == nil && s.gcNeedsCheck() {
			// Clear finished callee/args before growing the stack and root all native return
			// values across both stack growth (emergency GC) and Tier-1 debt collection.
			fillNil(th.stack[base:cleanupEnd])
			th.top = base
			if len(res) > 0 {
				if serr := s.ensureStackWithRoots(th, base+len(res)+4, NilValue(), res); serr == nil {
					copy(th.stack[base:base+len(res)], res)
					if end := base + len(res); end > cleanupEnd {
						cleanupEnd = end
					}
					th.top = base + len(res)
					s.gcCheckDebt(th.top)
				} else if s.abortErr == nil {
					// Stack growth hit memLimit: pin res transiently via Ref handles.
					var refs []int
					for _, v := range res {
						if isGCRootable(v) {
							refs = append(refs, s.Ref(v))
						}
					}
					s.gcCheckDebt(th.top)
					for _, id := range refs {
						s.Unref(id)
					}
				}
			} else {
				s.gcCheckDebt(th.top)
			}
			if s.abortErr != nil {
				return nil, s.abortErr
			}
		}
		return res, err
	}

	prevCB := th.inCCallback
	th.inCCallback = false
	defer func() {
		th.inCCallback = prevCB
	}()

	cl := fn.AsClosure()
	p := cl.bp.proto
	base := th.top
	if th.nframe > 0 {
		prev := &th.frames[th.nframe-1]
		if prev.kind == frameLua && prev.cl != nil {
			if prevEnd := prev.base + int(prev.cl.bp.proto.MaxStackSize); prevEnd > base {
				base = prevEnd
			}
		}
	}
	funcIdx := base
	argBase := funcIdx + 1
	if err := s.ensureStackWithRoots(th, argBase+max(len(args), int(p.MaxStackSize))+8, fn, args); err != nil {
		return nil, err
	}
	th.stack[funcIdx] = fn
	for i, a := range args {
		th.stack[argBase+i] = a
	}
	th.top = argBase + len(args)
	th.noteStack(argBase + max(len(args), int(p.MaxStackSize)))

	entryDepth := th.nframe
	if err := th.pushLuaFrame(cl, funcIdx, len(args), -1); err != nil {
		return nil, err
	}
	if herr := s.fireLuaCallHook(th, cl, len(args)); herr != nil {
		th.nframe = entryDepth
		th.top = funcIdx
		return nil, herr
	}
	clearTopStack := func() {
		th.top = funcIdx
		if entryDepth == 0 && len(th.openUpvals) == 0 {
			if end := min(max(th.highStack, th.top), len(th.stack)); funcIdx < end {
				fillNil(th.stack[funcIdx:end])
			}
			if hf := min(th.highFrames, len(th.frames)); hf > 0 {
				clear(th.frames[:hf])
			}
			th.highStack = funcIdx
			th.highFrames = 0
			th.clearAbandonedInitStorage()
		}
	}
	err := s.runThread(th, entryDepth)
	if th.uncaughtErr != nil {
		err = th.uncaughtErr
		th.uncaughtErr = nil
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
			th.discardTBCAtOrAbove(funcIdx)
			th.closeUpvals(funcIdx)
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
		ev, _, raised := th.closeAllSync(funcIdx, ev, true)
		if raised {
			xpHandled = false
			isMem = false
		}
		if s.abortErr != nil {
			uerr := s.abortErr
			if !errors.Is(uerr, errCoroCloseSelf) {
				th.discardTBCAtOrAbove(funcIdx)
			}
			th.closeUpvals(funcIdx)
			th.nframe = entryDepth
			clearTopStack()
			return nil, uerr
		}
		th.nframe = entryDepth
		clearTopStack()
		return nil, &LuaError{Val: ev, xpHandled: xpHandled, isMemErr: isMem}
	}
	if s.gcNeedsCheck() {
		s.gcCheckDebt(th.top)
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
	copy(res, th.stack[funcIdx:th.top])
	clearTopStack()
	return res, nil
}

func (s *State) call1OnThread(th *Thread, fn Value, args []Value) (Value, error) {
	// Zero-allocation single-return fast path for direct Lua closure calls with no hooks/interrupts.
	if fn.IsClosure() && s.abortErr == nil && !s.hasInterruptControl() &&
		s.nCcalls < maxCCalls && th.hookMask == 0 && !th.inHook && !th.suppressHook {
		cl := fn.AsClosure()
		p := cl.bp.proto
		if !p.IsVararg() {
			s.nCcalls++
			th.nny++
			prevCB := th.inCCallback
			th.inCCallback = false

			base := th.top
			if th.nframe > 0 {
				prev := &th.frames[th.nframe-1]
				if prev.kind == frameLua && prev.cl != nil {
					if prevEnd := prev.base + int(prev.cl.bp.proto.MaxStackSize); prevEnd > base {
						base = prevEnd
					}
				}
			}
			funcIdx := base
			argBase := funcIdx + 1
			need := argBase + max(len(args), int(p.MaxStackSize)) + 8
			if need+stackSlack <= len(th.stack) && th.nframe < len(th.frames) {
				th.stack[funcIdx] = fn
				copy(th.stack[argBase:argBase+len(args)], args)
				nparams := int(p.NumParams)
				if len(args) < nparams {
					fillNil(th.stack[argBase+len(args) : argBase+nparams])
				}
				if int(p.MaxStackSize) > nparams {
					fillNil(th.stack[argBase+nparams : argBase+int(p.MaxStackSize)])
				}
				frameEnd := argBase + int(p.MaxStackSize)
				th.top = frameEnd
				th.noteStack(max(frameEnd, argBase+len(args)))

				entryDepth := th.nframe
				nf := &th.frames[entryDepth]
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
				if th.uncaughtErr != nil {
					err = th.uncaughtErr
					th.uncaughtErr = nil
				}
				if err == nil && s.abortErr != nil {
					err = s.abortErr
				}

				clearTop := func() {
					th.top = funcIdx
					if entryDepth == 0 && len(th.openUpvals) == 0 {
						if end := min(max(th.highStack, th.top), len(th.stack)); funcIdx < end {
							fillNil(th.stack[funcIdx:end])
						}
						if hf := min(th.highFrames, len(th.frames)); hf > 0 {
							clear(th.frames[:hf])
						}
						th.highStack = funcIdx
						th.highFrames = 0
						th.clearAbandonedInitStorage()
					}
				}

				var ret Value
				var retErr error
				if err != nil {
					if errors.Is(err, errYieldSentinel) || errors.Is(err, errCoroCloseSelf) {
						retErr = err
					} else if isUncatchableErr(err) {
						s.abortErr = err
						th.discardTBCAtOrAbove(funcIdx)
						th.closeUpvals(funcIdx)
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
						ev, _, raised := th.closeAllSync(funcIdx, ev, true)
						if raised {
							xpHandled = false
							isMem = false
						}
						if s.abortErr != nil {
							uerr := s.abortErr
							if !errors.Is(uerr, errCoroCloseSelf) {
								th.discardTBCAtOrAbove(funcIdx)
							}
							th.closeUpvals(funcIdx)
							th.nframe = entryDepth
							clearTop()
							retErr = uerr
						} else {
							th.nframe = entryDepth
							clearTop()
							retErr = &LuaError{Val: ev, xpHandled: xpHandled, isMemErr: isMem}
						}
					}
				} else {
					if s.gcNeedsCheck() {
						s.gcCheckDebt(th.top)
						if s.abortErr != nil {
							clearTop()
							retErr = s.abortErr
						}
					}
					if retErr == nil {
						if th.top > funcIdx {
							ret = th.stack[funcIdx]
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
	v := mt.RawGetStr(s.intern.tms[tm])
	if v.IsNil() && tm < 6 {
		mt.tmFlags |= 1 << tm
	}
	return v
}

func (s *State) getMetatableOf(v Value) *Table {
	switch {
	case v.IsTable():
		return v.AsTable().meta
	case v.IsUserdata():
		return v.AsUserdata().Meta
	case v.IsString():
		return s.stringMeta
	default:
		return s.typeMetas[v.TypeTag()]
	}
}

func (s *State) setMetatableOf(v Value, mt *Table) {
	switch {
	case v.IsTable():
		s.SetMetatable(v.AsTable(), mt)
	case v.IsUserdata():
		u := v.AsUserdata()
		u.Meta = mt
		if mt != nil {
			mt.markProto(s)
			s.registerFinalizable(v, mt)
		}
		s.protoEpoch++
	case v.IsString():
		s.stringMeta = mt
		s.typeMetas[4] = mt
		if mt != nil {
			mt.markProto(s)
		}
		s.protoEpoch++
	default:
		s.typeMetas[v.TypeTag()] = mt
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

// TryIntern interns or wraps str as a *LuaString after checking and accounting for newly
// allocated string storage against the State's memory limit.
func (s *State) TryIntern(str string) (*LuaString, error) {
	if ls, ok := s.intern.lookupExistingShort(str); ok {
		return ls, nil
	}
	if s.memLimit > 0 || s.gc.tier1 {
		need := int64(len(str)) + 48
		if s.memLimit > 0 && s.intern.arena != nil && len(str) > 0 && s.gc.allocBytes+s.gc.tempBytes+need > s.memLimit {
			// Guard against str aliasing an unrooted Arena slot that emergency GC might sweep.
			str = strings.Clone(str)
		}
		if err := s.checkMemAndAccount(need); err != nil {
			return nil, err
		}
	}
	return s.intern.Intern(str), nil
}

// TryAllocNative checks and accounts for a dynamically allocated native closure against
// the State's memory limit and returns it as a Value.
func (s *State) TryAllocNative(nf *NativeFunc) (Value, error) {
	if nf == nil {
		return NilValue(), nil
	}
	if s.memLimit > 0 || s.gc.tier1 {
		if err := s.checkMemAndAccount(48 + int64(len(nf.upvals))*16); err != nil {
			return NilValue(), err
		}
	}
	return NativeValue(nf), nil
}

// TryNewUserdata allocates a full Lua userdata carrying data with nUValues user values
// after checking and accounting for its storage against the State's memory limit.
func (s *State) TryNewUserdata(data any, nUValues int) (Value, error) {
	if nUValues < 0 {
		nUValues = 0
	}
	if s.memLimit > 0 || s.gc.tier1 {
		if err := s.checkMemAndAccount(64 + int64(nUValues)*16); err != nil {
			return NilValue(), err
		}
	}
	u := &Userdata{Data: data}
	if nUValues > 0 {
		u.UValues = make([]Value, nUValues)
		fillNil(u.UValues)
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
		t := tblVal.AsTable()
		if err := s.ensureLookupStrKeyHash(t, keyStr); err != nil {
			return err
		}
		if !t.RawGetStr(keyStr).IsNil() || t.meta == nil || s.getTM(t.meta, TMNewIndex).IsNil() {
			t.RawSetStr(s, keyStr, val)
			return s.ConsumeMemErr()
		}
	}
	cur := tblVal
	key := StringValue(keyStr)
	for loop := range 2000 {
		var tm Value
		if cur.IsTable() {
			t := cur.AsTable()
			if loop > 0 {
				if err := s.ensureLookupKeyHash(t, key); err != nil {
					return err
				}
				if !t.RawGet(key).IsNil() {
					if err := t.RawSet(s, key, val); err != nil {
						return err
					}
					return s.ConsumeMemErr()
				}
			}
			tm = s.getTM(t.meta, TMNewIndex)
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
