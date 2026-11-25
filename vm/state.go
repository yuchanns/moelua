package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
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
	initialStackSize = 512
	stackSlack       = 264 // >= 256 slots above frame base so *[256]Value window is always in-bounds
	maxFrames        = 10000
	maxCCalls        = 200
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
	Collectable bool    // true for dynamic C closures (coroutine.wrap, gmatch, io.lines, utf8.codes); false for immutable light builtins
	gcMark      uint64  // Tier 1 GC mark epoch (only mutated when Collectable == true)
	upvals      []Value // captured C closure upvalues (traced by Tier 1 GC and exposed to debug.getupvalue/setupvalue)
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
	next     *Upvalue // linked list of open upvalues sorted descending by stackIdx
	closed   Value
	stackIdx int
}

func upvaluePtr(u *Upvalue) unsafe.Pointer { return unsafe.Pointer(u) }

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

// State is a single-goroutine Lua 5.5.1 execution state (multiple States share immutable Protos).
type State struct {
	mainThread *Thread
	cur        *Thread // currently executing Thread (mainThread or resumed coroutine)

	env        *Table
	registry   *Table
	intern     stringInterner
	shapes     shapeRegistry
	protoEpoch uint32

	out       io.Writer
	errOut    io.Writer
	warnOn    bool
	startTime time.Time
	nCcalls   int

	compilerFn       CompileFunc
	compilerReaderFn func(name string, r io.Reader, opts ...compiler.Option) (*bytecode.Proto, error)

	// Cached builtin identities for TFORCALL / pcall / debug.getinfo fast paths & standard libs.
	builtinNext         *NativeFunc
	builtinIPairsIter   *NativeFunc
	builtinDebugGetInfo *NativeFunc
	builtinCoroYield    *NativeFunc
	stringMeta        *Table
	fileMeta          *Table
	pkgTbl            *Table
	loadedTbl         *Table
	preloadTbl        *Table
	typeMetas         [9]*Table // per-type metatables (debug.setmetatable on primitives!)
	stdinInput        Value
	defaultInput      Value
	defaultOutput     Value
	openFiles         []*fileInner // open non-std file handles flushed on os.exit / closed LIFO on State.Close (does NOT pin *luaFile)

	// Two-Tier GC controller (Tier 0 = 0 overhead, Tier 1 = deterministic Lua 5.5 semantics).
	gc gcState

	// Optional per-State soft/hard memory limit in bytes (0 = unlimited).
	memLimit    int64
	memExceeded bool

	// Optional async interrupt flag, context cancellation, and VM instruction step budget;
	// zero overhead in fast (*State).run when all are unset!
	intrFlag    *atomic.Bool
	intrTicks   uint32
	ctx         context.Context
	insnLimit   int64
	insnCount   int64
	abortErr    error // pending uncatchable abort error (ctx / instruction limit)
	fsSearchers int

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

// newBootstrapState builds a fresh State from scratch via initStdlib(); used once to construct stdTemplate.
func newBootstrapState() *State {
	s := &State{
		intern:     newStringInterner(0x9e3779b9),
		shapes:     newShapeRegistry(),
		out:        os.Stdout,
		errOut:     os.Stderr,
		startTime:  time.Now(),
		gc:               newGCState(),
		compilerFn:       defaultCompileFunc,
		compilerReaderFn: defaultCompileReaderFunc,
	}
	s.seedRNG(makeRandomSeed(s), 0)
	s.shapes.root = &s.shapes.rootShape
	s.mainThread = newThread(s, initialStackSize)
	s.mainThread.nny = 1 // main thread is never yieldable (L->nny = 1)
	s.cur = s.mainThread
	s.env = s.NewDictTable(64)
	s.registry = s.NewDictTable(16)
	s.registry.RawSetInt(s, 1, ThreadValue(s.mainThread))
	s.registry.RawSetInt(s, 2, TableValue(s.env))
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
	s.memLimit = limit
	s.memExceeded = false
}

// MemoryUsage returns the current VM-accounted memory usage in bytes.
func (s *State) MemoryUsage() int64 {
	return s.gc.allocBytes + s.gc.tempBytes
}

// ConsumeMemErr returns and clears a pending deferred memory-limit error from table mutation.
func (s *State) ConsumeMemErr() error {
	if s.memExceeded {
		s.memExceeded = false
		return &LuaError{Val: StringValue(s.intern.Intern("not enough memory"))}
	}
	return nil
}

// checkMem verifies that allocating delta bytes stays within hard safety bounds and s.memLimit.
// On limit pressure it runs an emergency GC trace (without invoking __gc finalizers or
// mutating weak tables mid-allocation, matching PUC-Rio Lua's emergency collector).
func (s *State) checkMem(delta int64) error {
	const hardSingleAllocLimit = 1 << 30 // 1 GiB single allocation guard against Go OOM fatal
	if delta < 0 || delta > hardSingleAllocLimit {
		return &LuaError{Val: StringValue(s.intern.Intern("not enough memory"))}
	}
	if s.memLimit > 0 && s.gc.allocBytes+s.gc.tempBytes+delta > s.memLimit {
		if !s.gc.inGCTM {
			s.collectEmergency()
		}
		if s.gc.allocBytes+s.gc.tempBytes+delta > s.memLimit {
			return &LuaError{Val: StringValue(s.intern.Intern("not enough memory"))}
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
	}
	return nil
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

// SetInterruptFlag attaches an atomic interrupt flag polled periodically in runHooked
// (used by CLI SIGINT handling; leaves fast run() completely untouched when nil).
func (s *State) SetInterruptFlag(flag *atomic.Bool) { s.intrFlag = flag }

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
			return v.AsString().S
		}
		if v.IsNumber() {
			return FormatValue(v)
		}
		if tm := s.getTMByObj(v, TMToString); !tm.IsNil() {
			res, terr := s.Call(tm, v)
			if terr == nil && len(res) > 0 && res[0].IsString() {
				return res[0].AsString().S
			}
		}
		return fmt.Sprintf("error object is a %s value", v.TypeName())
	}
	return err.Error()
}

// GlobalEnv returns _G / _ENV.
func (s *State) GlobalEnv() *Table { return s.env }

// Registry returns the State's Lua registry table (LUA_REGISTRYINDEX).
func (s *State) Registry() *Table { return s.registry }

// PreloadTable returns the State's package.preload table.
func (s *State) PreloadTable() *Table { return s.preloadTbl }

// Intern interns a Go string into this State's string table.
func (s *State) Intern(str string) *LuaString { return s.intern.Intern(str) }

// SetGlobal installs a global variable in _ENV.
func (s *State) SetGlobal(name string, v Value) {
	s.env.RawSetStr(s, s.intern.Intern(name), v)
}

// GetGlobal retrieves a global variable from _ENV.
func (s *State) GetGlobal(name string) Value {
	return s.env.RawGetStr(s.intern.Intern(name))
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
		cl.upvals[0] = &Upvalue{closed: TableValue(s.env)}
		for i := 1; i < len(cl.upvals); i++ {
			cl.upvals[i] = &Upvalue{closed: NilValue()}
		}
	}
	if chunkBytes > 0 {
		s.gc.allocBytes += chunkBytes
	}
	return cl, nil
}

// UndumpAndLoad decodes, verifies, and loads a Lua 5.5.1 binary chunk.
func (s *State) UndumpAndLoad(chunk []byte) (*Closure, error) {
	p, err := bytecode.Undump(chunk)
	if err != nil {
		return nil, err
	}
	return s.Load(p)
}

// Call invokes a Lua closure or native function on the current Thread and returns its results.
func (s *State) Call(fn Value, args ...Value) ([]Value, error) {
	return s.callOnThread(s.cur, fn, args)
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
	if s.nCcalls >= maxCCalls {
		return nil, &LuaError{Val: StringValue(s.intern.Intern("C stack overflow"))}
	}
	s.nCcalls++
	th.nny++
	defer func() {
		th.nny--
		s.nCcalls--
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
		if err := th.ensureStack(base + 1 + len(args) + 8); err != nil {
			return nil, err
		}
		th.stack[base] = fn
		copy(th.stack[base+1:base+1+len(args)], args)
		th.top = base + 1 + len(args)
		defer func() {
			if end := min(base+1+len(args), len(th.stack)); base < end {
				fillNil(th.stack[base:end])
			}
			th.top = savedTop
		}()
		if th.hookMask == 0 || th.inHook {
			return fn.AsNative().callMulti(s, th.stack[base+1:base+1+len(args)])
		}
		res, _, err := s.callNativeHooked(th, fn, base, len(args), extraDepth, maxFrames+128)
		return res, err
	}

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
	if err := th.ensureStack(argBase + max(len(args), int(p.MaxStackSize)) + 8); err != nil {
		return nil, err
	}
	th.stack[funcIdx] = fn
	for i, a := range args {
		th.stack[argBase+i] = a
	}
	th.top = argBase + len(args)

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
		if entryDepth == 0 && th.openUpvals == nil && funcIdx < len(th.stack) {
			fillNil(th.stack[funcIdx:])
		}
	}
	err := s.runThread(th, entryDepth)
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
		ev := errorToLuaValue(s, err)
		ev, _, _ = th.closeAllSync(funcIdx, ev, true)
		if s.abortErr != nil {
			uerr := s.abortErr
			th.discardTBCAtOrAbove(funcIdx)
			th.closeUpvals(funcIdx)
			th.nframe = entryDepth
			clearTopStack()
			return nil, uerr
		}
		th.nframe = entryDepth
		clearTopStack()
		return nil, &LuaError{Val: ev}
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

// runThread dispatches to the fast loop when no debug hooks or interrupt controls are active,
// or to the hooked variant when debug.sethook / ctx / instruction limits are active (zero cost in fast path!).
func (s *State) runThread(th *Thread, entryDepth int) error {
	if (th.hookMask != 0 && !th.inHook) || s.hasInterruptControl() {
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

// GetGlobalMM retrieves a global from `_G`, honoring `_G`'s `__index` metamethod
// (matching PUC-Rio `lua_getglobal`).
func (s *State) GetGlobalMM(name string) (Value, error) {
	key := s.intern.Intern(name)
	v := s.env.RawGetStr(key)
	if !v.IsNil() || s.env.meta == nil {
		return v, nil
	}
	return s.finishGet(TableValue(s.env), StringValue(key))
}

// TryIntern interns or wraps str as a *LuaString after checking and accounting for newly
// allocated string storage against the State's memory limit.
func (s *State) TryIntern(str string) (*LuaString, error) {
	if ls, ok := s.intern.lookupExistingShort(str); ok {
		return ls, nil
	}
	if s.memLimit > 0 || s.gc.tier1 {
		if err := s.checkMemAndAccount(int64(len(str)) + 48); err != nil {
			return nil, err
		}
	}
	return s.intern.Intern(str), nil
}

// SetGlobalMM assigns a global in `_G`, honoring `_G`'s `__newindex` metamethod
// (matching PUC-Rio `lua_setglobal`).
func (s *State) SetGlobalMM(name string, val Value) error {
	return s.tableSetStr(TableValue(s.env), s.intern.Intern(name), val)
}

// tableSetStr assigns keyStr = val on a Lua table honoring `__newindex` metamethods
// (matching PUC-Rio `lua_setfield`).
func (s *State) tableSetStr(tblVal Value, keyStr *LuaString, val Value) error {
	if tblVal.IsTable() {
		t := tblVal.AsTable()
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
			if loop > 0 && !t.RawGet(key).IsNil() {
				if err := t.RawSet(s, key, val); err != nil {
					return err
				}
				return s.ConsumeMemErr()
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
