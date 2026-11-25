package moelua

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
	"github.com/yuchanns/moelua/vm"
)

// ErrInstructionLimit is returned when Lua execution exceeds the configured
// instruction step budget (WithInstructionLimit / SetInstructionLimit).
// It cannot be caught by Lua pcall/xpcall or coroutine.resume.
var ErrInstructionLimit = vm.ErrInstructionLimit

// ErrStateClosed is returned when operations are attempted on a nil or closed State/Table.
var ErrStateClosed = errors.New("moelua: state is closed")

// State is a single-goroutine Lua 5.5.1 execution environment.
// Call Close when finished to flush open files and run remaining __gc finalizers in LIFO order.
type State struct {
	vm     *vm.State
	closed bool
}

func (s *State) ready() bool {
	return s != nil && !s.closed && s.vm != nil
}

// Option configures a newly created State.
type Option func(*stateConfig)

type stateConfig struct {
	out       io.Writer
	errOut    io.Writer
	memLimit  int64
	ctx       context.Context
	insnLimit int64
	fsList    []fs.FS
	noEnv     bool
}

// WithOutput redirects Lua print() output to w.
func WithOutput(w io.Writer) Option {
	return func(c *stateConfig) { c.out = w }
}

// WithErrorOutput redirects Lua warn() output to w.
func WithErrorOutput(w io.Writer) Option {
	return func(c *stateConfig) { c.errOut = w }
}

// WithMemoryLimit sets a per-State VM-accounted memory cap in bytes (<= 0 disables the cap).
// It tracks Lua tables, strings, closures, threads, and stack growth, triggering an emergency
// GC recount (without running __gc finalizers mid-allocation) before raising a recoverable
// Lua "not enough memory" error. It is a soft VM-level resource guard rather than an OS RSS cgroup.
func WithMemoryLimit(bytes int64) Option {
	return func(c *stateConfig) { c.memLimit = bytes }
}

// WithContext attaches a context.Context for cooperative cancellation and deadlines.
// Cancellation propagates out of Lua pcall/xpcall and coroutine.resume with the context's error
// (context.Canceled or context.DeadlineExceeded). Leaving ctx nil imposes zero overhead on the
// hot VM dispatch loop.
func WithContext(ctx context.Context) Option {
	return func(c *stateConfig) { c.ctx = ctx }
}

// WithInstructionLimit sets a deterministic VM instruction step budget (<= 0 disables the cap).
// Exceeding the budget aborts execution with ErrInstructionLimit, which cannot be caught by Lua
// pcall/xpcall or coroutine.resume. Leaving the limit unset imposes zero overhead on the hot VM loop.
func WithInstructionLimit(limit int64) Option {
	return func(c *stateConfig) { c.insnLimit = limit }
}

// WithFS registers an io/fs.FS (such as embed.FS or os.DirFS) as a Lua require() module searcher
// right after package.preload and before OS filesystem searchers. Multiple WithFS options are
// searched in registration order.
func WithFS(fsys fs.FS) Option {
	return func(c *stateConfig) {
		if fsys != nil {
			c.fsList = append(c.fsList, fsys)
		}
	}
}

// WithNoEnv ignores LUA_PATH/LUA_CPATH environment variables (equivalent to lua -E).
func WithNoEnv() Option {
	return func(c *stateConfig) { c.noEnv = true }
}

// New creates a new Lua 5.5.1 State cloned rapidly from the frozen stdlib template.
func New(opts ...Option) *State {
	var cfg stateConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	var raw *vm.State
	if cfg.noEnv {
		raw = vm.NewStateNoEnv()
	} else {
		raw = vm.NewState()
	}
	if cfg.out != nil {
		raw.SetOutput(cfg.out)
	}
	if cfg.errOut != nil {
		raw.SetErrOutput(cfg.errOut)
	}
	if cfg.memLimit > 0 {
		raw.SetMemoryLimit(cfg.memLimit)
	}
	if cfg.ctx != nil {
		raw.SetContext(cfg.ctx)
	}
	if cfg.insnLimit > 0 {
		raw.SetInstructionLimit(cfg.insnLimit)
	}
	for _, fsys := range cfg.fsList {
		raw.AddFS(fsys)
	}
	return &State{vm: raw}
}

// Raw returns the underlying *vm.State for advanced low-level integration (nil if closed).
func (s *State) Raw() *vm.State {
	if !s.ready() {
		return nil
	}
	return s.vm
}

// Close runs all remaining __gc finalizers in strict LIFO order and flushes/closes
// all open non-standard Lua file handles. Calling Close multiple times or on a nil
// State is safe and idempotent.
func (s *State) Close() {
	if !s.ready() {
		return
	}
	s.vm.Close()
	s.closed = true
}

// SetOutput redirects print() output.
func (s *State) SetOutput(w io.Writer) {
	if s.ready() {
		s.vm.SetOutput(w)
	}
}

// SetErrorOutput redirects warn() output.
func (s *State) SetErrorOutput(w io.Writer) {
	if s.ready() {
		s.vm.SetErrOutput(w)
	}
}

// SetMemoryLimit sets or updates the per-State memory cap in bytes (<= 0 disables).
func (s *State) SetMemoryLimit(bytes int64) {
	if s.ready() {
		s.vm.SetMemoryLimit(bytes)
	}
}

// MemoryUsage returns the current VM-accounted memory usage in bytes.
func (s *State) MemoryUsage() int64 {
	if !s.ready() {
		return 0
	}
	return s.vm.MemoryUsage()
}

// SetContext attaches or clears the cancellation context for subsequent execution.
func (s *State) SetContext(ctx context.Context) {
	if s.ready() {
		s.vm.SetContext(ctx)
	}
}

// SetInstructionLimit sets or clears the VM instruction budget (<= 0 disables) and resets
// the executed instruction counter to 0.
func (s *State) SetInstructionLimit(limit int64) {
	if s.ready() {
		s.vm.SetInstructionLimit(limit)
	}
}

// ResetInstructionCount resets the executed instruction counter to 0.
func (s *State) ResetInstructionCount() {
	if s.ready() {
		s.vm.ResetInstructionCount()
	}
}

// InstructionsExecuted returns the number of VM instructions executed while an instruction limit is active.
func (s *State) InstructionsExecuted() int64 {
	if !s.ready() {
		return 0
	}
	return s.vm.InstructionsExecuted()
}

// AddFS registers an io/fs.FS module searcher into package.searchers.
func (s *State) AddFS(fsys fs.FS) {
	if s.ready() {
		s.vm.AddFS(fsys)
	}
}

// AddSearcher appends a custom Go searcher function to package.searchers.
func (s *State) AddSearcher(name string, fn GoFunc) {
	if !s.ready() || fn == nil {
		return
	}
	v, err := s.TryNewFunc(name, fn)
	if err != nil {
		return
	}
	s.vm.AddSearcher(v.AsNative())
	_ = s.vm.ConsumeMemErr()
}

// Preload registers a Go module loader into package.preload[name] so Lua require(name)
// invokes loader lazily and caches its result in package.loaded[name].
// Passing a nil loader removes any existing preload entry for name.
func (s *State) Preload(name string, loader GoFunc) error {
	if !s.ready() {
		return ErrStateClosed
	}
	v := vm.NilValue()
	var ls *vm.LuaString
	if loader != nil {
		var err error
		ls, err = s.vm.TryIntern(name)
		if err != nil {
			return err
		}
		// Root the interned name across the loader allocation (emergency GC recount).
		defer s.pinTemps(vm.StringValue(ls))()
		v, err = s.TryNewFunc(name, loader)
		if err != nil {
			return err
		}
	} else {
		var ok bool
		ls, ok = s.vm.LookupString(name)
		if !ok {
			return nil
		}
	}
	s.vm.PreloadTable().RawSetStr(s.vm, ls, v)
	return s.vm.ConsumeMemErr()
}

// CollectGarbage executes a full deterministic Lua 5.5 GC cycle (including weak-table
// clearing and synchronous __gc finalizers in Tier 1 mode).
func (s *State) CollectGarbage() {
	if s.ready() {
		s.vm.CollectFull()
	}
}

// Chunk is an immutable, verified Lua 5.5.1 compiled bytecode prototype.
// A single *Chunk may be loaded into any number of independent States concurrently.
type Chunk struct {
	proto *bytecode.Proto
}

// Compile compiles Lua 5.5.1 source text into a verified immutable *Chunk
// using the zero-copy string fast path.
func Compile(name, src string) (*Chunk, error) {
	p, err := compiler.Compile(name, src)
	if err != nil {
		return nil, err
	}
	return &Chunk{proto: p}, nil
}

// CompileReader streams Lua 5.5.1 source text from r into a verified immutable *Chunk
// without reading the entire input into memory first.
func CompileReader(name string, r io.Reader) (*Chunk, error) {
	p, err := compiler.CompileReader(name, r)
	if err != nil {
		return nil, err
	}
	return &Chunk{proto: p}, nil
}

// LoadChunk decodes and verifies a PUC-Rio Lua 5.5.1 binary chunk using the slice fast path.
// Binary chunks are trusted input; verification provides panic-freedom defense in depth.
func LoadChunk(binary []byte) (*Chunk, error) {
	p, err := bytecode.Undump(binary)
	if err != nil {
		return nil, err
	}
	return &Chunk{proto: p}, nil
}

// LoadChunkReader streams and verifies a PUC-Rio Lua 5.5.1 binary chunk from r without
// materializing the full input buffer first.
func LoadChunkReader(r io.Reader) (*Chunk, error) {
	p, err := bytecode.UndumpReader(r)
	if err != nil {
		return nil, err
	}
	return &Chunk{proto: p}, nil
}

// Dump serializes the chunk into PUC-Rio Lua 5.5.1 binary bytecode format.
// If strip is true, debug info (source name, line numbers, locals, upvalue names) is omitted.
func (c *Chunk) Dump(strip bool) ([]byte, error) {
	if c == nil || c.proto == nil {
		return nil, fmt.Errorf("moelua: nil chunk")
	}
	return bytecode.Dump(c.proto, strip)
}

// DumpTo streams the chunk in PUC-Rio Lua 5.5.1 binary bytecode format directly to w
// without allocating a full-size intermediate buffer.
func (c *Chunk) DumpTo(w io.Writer, strip bool) error {
	if c == nil || c.proto == nil {
		return fmt.Errorf("moelua: nil chunk")
	}
	return bytecode.DumpTo(w, c.proto, strip)
}

// Proto returns the underlying *bytecode.Proto for read-only inspection.
// Callers must not mutate the returned prototype while any State may load or run it.
func (c *Chunk) Proto() *bytecode.Proto {
	if c == nil {
		return nil
	}
	return c.proto
}

// Value is a tagged Lua 5.5 value.
//
// GC Anchoring Rule: Go variables holding a Value pointing to a Lua table, closure,
// userdata, or coroutine are invisible to MoeLua's deterministic Tier 1 tracer.
// If a Go value must stay alive across Lua execution or CollectGarbage() without
// being stored in Lua globals/tables, anchor it with (*State).Pin(v) and call
// (*Handle).Release() when done.
type Value = vm.Value

// Nil returns the Lua nil value.
func Nil() Value { return vm.NilValue() }

// Bool returns a Lua boolean value.
func Bool(b bool) Value { return vm.BoolValue(b) }

// Int returns a Lua 64-bit integer value.
func Int(i int64) Value { return vm.IntValue(i) }

// Float returns a Lua 64-bit floating-point value.
func Float(f float64) Value { return vm.FloatValue(f) }

// TryString interns or wraps a Go string as a Lua string Value owned by s after checking
// and accounting for newly allocated string storage against the State's memory limit.
func (s *State) TryString(str string) (Value, error) {
	if !s.ready() {
		return vm.NilValue(), ErrStateClosed
	}
	ls, err := s.vm.TryIntern(str)
	if err != nil {
		return vm.NilValue(), err
	}
	return vm.StringValue(ls), nil
}

// String interns or wraps a Go string as a Lua string Value owned by s.
// Returns Nil() if the State is closed or if allocating the string would exceed the
// configured memory limit (use TryString to inspect allocation errors).
func (s *State) String(str string) Value {
	v, err := s.TryString(str)
	if err != nil {
		return vm.NilValue()
	}
	return v
}

// TryNewUserdata creates a full Lua userdata carrying arbitrary Go data after checking and
// accounting for its wrapper storage against the State's memory limit.
func (s *State) TryNewUserdata(data any) (Value, error) {
	if !s.ready() {
		return vm.NilValue(), ErrStateClosed
	}
	return s.vm.TryNewUserdata(data, 0)
}

// NewUserdata creates a full Lua userdata carrying arbitrary Go data.
// Returns Nil if the State is closed or allocation exceeds the configured memory limit
// (use TryNewUserdata to inspect allocation errors).
func (s *State) NewUserdata(data any) Value {
	u, err := s.TryNewUserdata(data)
	if err != nil {
		return vm.NilValue()
	}
	return u
}

// NewUserdataWithMeta creates a full Lua userdata carrying arbitrary Go data and
// attaches mt as its metatable (registering __gc finalization if present on mt).
func (s *State) NewUserdataWithMeta(data any, mt *Table) (Value, error) {
	if !s.ready() {
		return vm.NilValue(), ErrStateClosed
	}
	if mt != nil {
		if !mt.ready() {
			return vm.NilValue(), ErrStateClosed
		}
		if mt.state != s {
			return vm.NilValue(), fmt.Errorf("moelua: metatable belongs to another State")
		}
	}
	u, err := s.TryNewUserdata(data)
	if err != nil {
		return vm.NilValue(), err
	}
	if mt != nil {
		s.vm.SetMetatableOf(u, mt.raw)
		if err := s.vm.ConsumeMemErr(); err != nil {
			return vm.NilValue(), err
		}
	}
	return u, nil
}

// GetMetatable returns the metatable of v (table, userdata, string, or type metatable), or nil.
func (s *State) GetMetatable(v Value) *Table {
	if !s.ready() {
		return nil
	}
	mt := s.vm.GetMetatableOf(v)
	if mt == nil {
		return nil
	}
	return &Table{state: s, raw: mt}
}

// SetMetatable sets the metatable of v (table, userdata, string, or type metatable).
// Passing a nil mt clears the metatable.
func (s *State) SetMetatable(v Value, mt *Table) error {
	if !s.ready() {
		return ErrStateClosed
	}
	var rawMT *vm.Table
	if mt != nil {
		if !mt.ready() {
			return ErrStateClosed
		}
		if mt.state != s {
			return fmt.Errorf("moelua: metatable belongs to another State")
		}
		rawMT = mt.raw
	}
	s.vm.SetMetatableOf(v, rawMT)
	return s.vm.ConsumeMemErr()
}

// TryNewMetatable creates and registers a named metatable in the Lua registry (analogous to
// luaL_newmetatable). If a table is already registered under name, it returns (existing, false, nil);
// otherwise it allocates a new table with __name = name, stores it in the registry, and returns
// (mt, true, nil). Returns an error if the State is closed or the memory limit prevents allocation
// or registration, consuming any deferred VM memory error.
func (s *State) TryNewMetatable(name string) (*Table, bool, error) {
	if !s.ready() {
		return nil, false, ErrStateClosed
	}
	key, err := s.vm.TryIntern(name)
	if err != nil {
		return nil, false, err
	}
	reg := s.vm.Registry()
	if existing := reg.RawGetStr(key); existing.IsTable() {
		return &Table{state: s, raw: existing.AsTable()}, false, nil
	}
	// Root the interned name across the metatable allocation (emergency GC recount).
	defer s.pinTemps(vm.StringValue(key))()
	mt, err := s.vm.TryNewTable(0, 4)
	if err != nil {
		return nil, false, err
	}
	mt.RawSetStr(s.vm, s.vm.Intern("__name"), vm.StringValue(key))
	if err := s.vm.ConsumeMemErr(); err != nil {
		return nil, false, err
	}
	reg.RawSetStr(s.vm, key, vm.TableValue(mt))
	if err := s.vm.ConsumeMemErr(); err != nil {
		return nil, false, err
	}
	return &Table{state: s, raw: mt}, true, nil
}

// NewMetatable creates and registers a named metatable in the Lua registry (analogous to
// luaL_newmetatable). If a table is already registered under name, it returns (existing, false);
// otherwise it creates a new table with __name = name, stores it in the registry, and returns (mt, true).
// Returns (nil, false) if the State is closed or the memory limit prevents allocation or registration
// (use TryNewMetatable to inspect the error).
func (s *State) NewMetatable(name string) (*Table, bool) {
	mt, created, err := s.TryNewMetatable(name)
	if err != nil {
		return nil, false
	}
	return mt, created
}

// GetMetatableByName retrieves a named metatable previously created with NewMetatable
// (analogous to luaL_getmetatable), or nil if none exists.
func (s *State) GetMetatableByName(name string) *Table {
	if !s.ready() {
		return nil
	}
	ls, ok := s.vm.LookupString(name)
	if !ok {
		return nil
	}
	if existing := s.vm.Registry().RawGetStr(ls); existing.IsTable() {
		return &Table{state: s, raw: existing.AsTable()}
	}
	return nil
}

// CheckUserdata verifies that v is a userdata whose metatable matches the registry
// metatable named name (analogous to luaL_testudata) and returns its Go payload.
func (s *State) CheckUserdata(v Value, name string) (any, bool) {
	if !s.ready() || !v.IsUserdata() {
		return nil, false
	}
	mt := s.GetMetatableByName(name)
	if mt == nil || s.vm.GetMetatableOf(v) != mt.raw {
		return nil, false
	}
	return v.AsUserdata().Data, true
}

// Handle is a GC-anchored host reference to a Lua Value (analogous to luaL_ref).
// While pinned, MoeLua's Tier 1 GC tracer treats the referenced value as a live root.
type Handle struct {
	state *State
	id    int
	val   Value
}

// Pin anchors v as a live GC root in s until Release is called.
func (s *State) Pin(v Value) *Handle {
	if !s.ready() {
		return nil
	}
	return &Handle{
		state: s,
		id:    s.vm.Ref(v),
		val:   v,
	}
}

// Value returns the anchored Lua Value (or Nil if already released or state closed).
func (h *Handle) Value() Value {
	if h == nil || h.id == 0 || !h.state.ready() {
		return vm.NilValue()
	}
	return h.val
}

// Release unpins the reference so the Lua value may be collected when otherwise unreachable.
// Calling Release multiple times is safe and idempotent.
func (h *Handle) Release() {
	if h == nil || h.id == 0 {
		return
	}
	if h.state.ready() {
		h.state.vm.Unref(h.id)
	}
	h.id = 0
	h.val = vm.NilValue()
}

// GoFunc is a Go function callable from Lua.
type GoFunc func(s *State, args []Value) ([]Value, error)

// GoFunc1 is a zero-allocation single-return Go function callable from Lua.
type GoFunc1 func(s *State, args []Value) (Value, error)

// TryNewFunc wraps a GoFunc as a Lua callable Value after checking and accounting for
// the dynamically allocated native closure against the State's memory limit.
func (s *State) TryNewFunc(name string, fn GoFunc) (Value, error) {
	if !s.ready() {
		return vm.NilValue(), ErrStateClosed
	}
	if fn == nil {
		return vm.NilValue(), nil
	}
	return s.vm.TryAllocNative(&vm.NativeFunc{
		Name:        name,
		Collectable: true,
		Fn: func(_ *vm.State, args []vm.Value) ([]vm.Value, error) {
			return fn(s, args)
		},
	})
}

// NewFunc wraps a GoFunc as a Lua callable Value.
// Returns Nil if the State is closed, fn is nil, or allocation exceeds the configured
// memory limit (use TryNewFunc to inspect allocation errors).
func (s *State) NewFunc(name string, fn GoFunc) Value {
	v, err := s.TryNewFunc(name, fn)
	if err != nil {
		return vm.NilValue()
	}
	return v
}

// TryNewFunc1 wraps a single-return GoFunc1 as a Lua callable Value with zero per-call
// return-slice allocation after checking and accounting against the State's memory limit.
func (s *State) TryNewFunc1(name string, fn GoFunc1) (Value, error) {
	if !s.ready() {
		return vm.NilValue(), ErrStateClosed
	}
	if fn == nil {
		return vm.NilValue(), nil
	}
	return s.vm.TryAllocNative(&vm.NativeFunc{
		Name:        name,
		Collectable: true,
		FastFn: func(_ *vm.State, args []vm.Value) (vm.Value, error) {
			return fn(s, args)
		},
	})
}

// NewFunc1 wraps a single-return GoFunc1 as a Lua callable Value with zero per-call return-slice allocation.
// Returns Nil if the State is closed, fn is nil, or allocation exceeds the configured
// memory limit (use TryNewFunc1 to inspect allocation errors).
func (s *State) NewFunc1(name string, fn GoFunc1) Value {
	v, err := s.TryNewFunc1(name, fn)
	if err != nil {
		return vm.NilValue()
	}
	return v
}

// RegisterFunc installs a GoFunc as a global variable in _G.
func (s *State) RegisterFunc(name string, fn GoFunc) {
	if !s.ready() || fn == nil {
		return
	}
	v, err := s.TryNewFunc(name, fn)
	if err != nil {
		return
	}
	s.SetGlobal(name, v)
}

// RegisterFunc1 installs a zero-allocation single-return GoFunc1 as a global variable in _G.
func (s *State) RegisterFunc1(name string, fn GoFunc1) {
	if !s.ready() || fn == nil {
		return
	}
	v, err := s.TryNewFunc1(name, fn)
	if err != nil {
		return
	}
	s.SetGlobal(name, v)
}

// pinTemps temporarily roots Go-held values so an emergency GC triggered by a following
// allocation still traces and recounts them against the memory limit. Call the returned
// function to release the roots once the values are reachable from Lua.
func (s *State) pinTemps(vals ...Value) func() {
	ids := make([]int, 0, len(vals))
	for _, v := range vals {
		if v.IsNil() {
			continue
		}
		ids = append(ids, s.vm.Ref(v))
	}
	return func() {
		for _, id := range ids {
			s.vm.Unref(id)
		}
	}
}

// TrySetGlobal sets a global variable in _G after checking and accounting for the key string
// and any table growth against the State's memory limit.
func (s *State) TrySetGlobal(name string, v Value) error {
	if !s.ready() {
		return ErrStateClosed
	}
	var ls *vm.LuaString
	if v.IsNil() {
		var ok bool
		ls, ok = s.vm.LookupString(name)
		if !ok {
			return nil
		}
	} else {
		// Root v (held only by Go) across key allocation so an emergency GC triggered by
		// TryIntern recounts it against the memory limit.
		defer s.pinTemps(v)()
		var err error
		ls, err = s.vm.TryIntern(name)
		if err != nil {
			return err
		}
	}
	s.vm.GlobalEnv().RawSetStr(s.vm, ls, v)
	return s.vm.ConsumeMemErr()
}

// SetGlobal sets a global variable in _G.
// If the State is closed or allocating the key/storage exceeds the configured memory limit,
// the assignment is skipped without leaving an unconsumed VM memory error (use TrySetGlobal to inspect errors).
func (s *State) SetGlobal(name string, v Value) {
	_ = s.TrySetGlobal(name, v)
}

// GetGlobal retrieves a global variable from _G.
func (s *State) GetGlobal(name string) Value {
	if !s.ready() {
		return vm.NilValue()
	}
	return s.vm.GetGlobal(name)
}

// Compile compiles Lua 5.5.1 source text into a *Chunk while honoring this State's
// memory limit, cancellation context, and instruction budget.
func (s *State) Compile(name, src string) (*Chunk, error) {
	if !s.ready() {
		return nil, ErrStateClosed
	}
	p, err := s.vm.CompileString(name, src)
	if err != nil {
		return nil, err
	}
	return &Chunk{proto: p}, nil
}

// CompileReader streams Lua 5.5.1 source text from r into a *Chunk while honoring this State's
// memory limit, cancellation context, and instruction budget.
func (s *State) CompileReader(name string, r io.Reader) (*Chunk, error) {
	if !s.ready() {
		return nil, ErrStateClosed
	}
	p, err := s.vm.CompileReader(name, r)
	if err != nil {
		return nil, err
	}
	return &Chunk{proto: p}, nil
}

// Load instantiates an immutable *Chunk as a Lua closure bound to this State's _ENV.
func (s *State) Load(chunk *Chunk) (Value, error) {
	if !s.ready() {
		return vm.NilValue(), ErrStateClosed
	}
	if chunk == nil || chunk.proto == nil {
		return vm.NilValue(), fmt.Errorf("moelua: nil chunk")
	}
	cl, err := s.vm.Load(chunk.proto)
	if err != nil {
		return vm.NilValue(), err
	}
	return vm.ClosureValue(cl), nil
}

// LoadReader streams a Lua 5.5.1 text or binary chunk from r (stripping UTF-8 BOM and
// leading '#' line) and binds it as a closure against this State's _ENV.
func (s *State) LoadReader(chunkName string, r io.Reader) (Value, error) {
	if !s.ready() {
		return vm.NilValue(), ErrStateClosed
	}
	return s.vm.LoadChunkReader(r, chunkName)
}

// Call invokes a Lua function, GoFunc, or callable table/userdata with args.
func (s *State) Call(fn Value, args ...Value) ([]Value, error) {
	if !s.ready() {
		return nil, ErrStateClosed
	}
	return s.vm.Call(fn, args...)
}

// DoString compiles and executes a Lua 5.5.1 source snippet.
func (s *State) DoString(src string) ([]Value, error) {
	return s.DoStringNamed("=(dostring)", src)
}

// DoStringNamed compiles and executes a Lua 5.5.1 source snippet with a custom chunk name.
func (s *State) DoStringNamed(name, src string) ([]Value, error) {
	if !s.ready() {
		return nil, ErrStateClosed
	}
	ch, err := s.Compile(name, src)
	if err != nil {
		return nil, err
	}
	fn, err := s.Load(ch)
	if err != nil {
		return nil, err
	}
	return s.Call(fn)
}

// DoReader streams and executes a Lua 5.5.1 text or binary chunk from r (handling UTF-8 BOM
// and initial '#' comment line) without buffering the entire input in memory first.
func (s *State) DoReader(chunkName string, r io.Reader) ([]Value, error) {
	if !s.ready() {
		return nil, ErrStateClosed
	}
	fn, err := s.vm.LoadChunkReader(r, chunkName)
	if err != nil {
		return nil, err
	}
	return s.Call(fn)
}

// DoFile streams and executes a Lua 5.5.1 source or binary file (handling UTF-8 BOM and initial '#' comment).
func (s *State) DoFile(path string) ([]Value, error) {
	if !s.ready() {
		return nil, ErrStateClosed
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer f.Close()
	return s.DoReader("@"+path, f)
}

// DoBytes loads and executes raw Lua text or binary bytes (stripping UTF-8 BOM and leading '#' line).
func (s *State) DoBytes(chunkName string, data []byte) ([]Value, error) {
	if !s.ready() {
		return nil, ErrStateClosed
	}
	fn, err := s.vm.LoadChunk(data, chunkName)
	if err != nil {
		return nil, err
	}
	return s.Call(fn)
}

// Table is a typed handle to a Lua table bound to a State.
type Table struct {
	state *State
	raw   *vm.Table
}

func (t *Table) ready() bool {
	return t != nil && t.raw != nil && t.state.ready()
}

// NewTable allocates a new Lua table with optional sequence/record capacity hints.
// Returns nil if the State is closed or if allocating the requested capacity would
// exceed the configured memory limit (use TryNewTable to inspect allocation errors).
func (s *State) NewTable(nseq, nrec int) *Table {
	t, err := s.TryNewTable(nseq, nrec)
	if err != nil {
		return nil
	}
	return t
}

// TryNewTable allocates a new Lua table after checking and accounting for its requested
// storage against the State's memory limit, returning an error if the State is closed or
// allocation exceeds the limit.
func (s *State) TryNewTable(nseq, nrec int) (*Table, error) {
	if !s.ready() {
		return nil, ErrStateClosed
	}
	raw, err := s.vm.TryNewTable(nseq, nrec)
	if err != nil {
		return nil, err
	}
	return &Table{state: s, raw: raw}, nil
}

// TableOf wraps a Lua table Value into a *Table helper (returns nil if v is not a table or s is closed).
func (s *State) TableOf(v Value) *Table {
	if !s.ready() || !v.IsTable() {
		return nil
	}
	return &Table{state: s, raw: v.AsTable()}
}

// Value returns the underlying Lua Value (Nil if t is nil or its State is closed).
func (t *Table) Value() Value {
	if !t.ready() {
		return vm.NilValue()
	}
	return vm.TableValue(t.raw)
}

// Raw returns the underlying *vm.Table (nil if t is nil or its State is closed).
func (t *Table) Raw() *vm.Table {
	if !t.ready() {
		return nil
	}
	return t.raw
}

// Len returns the raw sequence length (#t).
func (t *Table) Len() int64 {
	if !t.ready() {
		return 0
	}
	return t.raw.Len()
}

// Get returns rawget(t, key).
func (t *Table) Get(key Value) Value {
	if !t.ready() {
		return vm.NilValue()
	}
	return t.raw.RawGet(key)
}

// Set performs rawset(t, key, val), returning an error if the State is closed, key is invalid,
// or memory limit is exceeded.
func (t *Table) Set(key, val Value) error {
	if !t.ready() {
		return ErrStateClosed
	}
	// Root the table and operands across any array/hash growth (emergency GC recount).
	defer t.state.pinTemps(vm.TableValue(t.raw), key, val)()
	if err := t.raw.RawSet(t.state.vm, key, val); err != nil {
		_ = t.state.vm.ConsumeMemErr()
		return err
	}
	return t.state.vm.ConsumeMemErr()
}

// GetInt returns rawget(t, i).
func (t *Table) GetInt(i int64) Value {
	if !t.ready() {
		return vm.NilValue()
	}
	return t.raw.RawGetInt(i)
}

// SetInt performs rawset(t, i, val), returning an error if the State is closed or memory limit is exceeded.
func (t *Table) SetInt(i int64, val Value) error {
	if !t.ready() {
		return ErrStateClosed
	}
	// Root the table and value across any array/hash growth (emergency GC recount).
	defer t.state.pinTemps(vm.TableValue(t.raw), val)()
	t.raw.RawSetInt(t.state.vm, i, val)
	return t.state.vm.ConsumeMemErr()
}

// GetString returns rawget(t, key).
func (t *Table) GetString(key string) Value {
	if !t.ready() {
		return vm.NilValue()
	}
	ls, ok := t.state.vm.LookupString(key)
	if !ok {
		return vm.NilValue()
	}
	return t.raw.RawGetStr(ls)
}

// SetString performs rawset(t, key, val), returning an error if the State is closed or memory limit is exceeded.
func (t *Table) SetString(key string, val Value) error {
	if !t.ready() {
		return ErrStateClosed
	}
	var ls *vm.LuaString
	if val.IsNil() {
		var ok bool
		ls, ok = t.state.vm.LookupString(key)
		if !ok {
			return nil
		}
	} else {
		// Root val and t (possibly held only by Go) across key allocation so an emergency GC
		// triggered by TryIntern recounts them against the memory limit.
		defer t.state.pinTemps(val, vm.TableValue(t.raw))()
		var err error
		ls, err = t.state.vm.TryIntern(key)
		if err != nil {
			return err
		}
	}
	t.raw.RawSetStr(t.state.vm, ls, val)
	return t.state.vm.ConsumeMemErr()
}

// Metatable returns t's metatable, or nil if none is set (or t/State is closed).
func (t *Table) Metatable() *Table {
	if !t.ready() {
		return nil
	}
	mt := t.raw.Metatable()
	if mt == nil {
		return nil
	}
	return &Table{state: t.state, raw: mt}
}

// SetMetatable sets or clears (when mt == nil) t's metatable.
func (t *Table) SetMetatable(mt *Table) error {
	if !t.ready() {
		return ErrStateClosed
	}
	return t.state.SetMetatable(t.Value(), mt)
}

// Next returns the next (key, value) pair after key in t, matching Lua next(t, key).
// Pass Nil() for key to start iteration. When no more entries exist, ok is false and err is nil.
func (t *Table) Next(key Value) (nextKey, nextVal Value, ok bool, err error) {
	if !t.ready() {
		return vm.NilValue(), vm.NilValue(), false, ErrStateClosed
	}
	nk, nv, err := t.raw.Next(key)
	if err != nil {
		return vm.NilValue(), vm.NilValue(), false, err
	}
	if nk.IsNil() {
		return vm.NilValue(), vm.NilValue(), false, nil
	}
	return nk, nv, true, nil
}

// ForEach iterates over all raw entries in t using Lua next() traversal order,
// stopping early and returning the error if fn returns a non-nil error.
func (t *Table) ForEach(fn func(key, val Value) error) error {
	if !t.ready() {
		return ErrStateClosed
	}
	if fn == nil {
		return nil
	}
	k := vm.NilValue()
	for {
		nk, nv, ok, err := t.Next(k)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := fn(nk, nv); err != nil {
			return err
		}
		k = nk
	}
}

// ThreadStatus identifies the lifecycle state of a Lua coroutine Thread.
type ThreadStatus = vm.ThreadStatus

const (
	ThreadOK        ThreadStatus = vm.ThreadOK        // fresh coroutine (ready to start) or running
	ThreadSuspended ThreadStatus = vm.ThreadSuspended // suspended via coroutine.yield
	ThreadNormal    ThreadStatus = vm.ThreadNormal    // active coroutine resuming another coroutine
	ThreadDead      ThreadStatus = vm.ThreadDead      // finished, errored, or closed
)

// Thread is a handle to a Lua 5.5 coroutine bound to a State.
// Hold a Pin(th.Value()) handle if a Go-created Thread must stay alive across
// Tier 1 CollectGarbage() without being referenced from Lua globals/tables.
type Thread struct {
	state *State
	raw   *vm.Thread
}

func (th *Thread) ready() bool {
	return th != nil && th.raw != nil && th.state.ready()
}

// NewThread creates a new suspended coroutine ready to execute fn (analogous to coroutine.create).
func (s *State) NewThread(fn Value) (*Thread, error) {
	if !s.ready() {
		return nil, ErrStateClosed
	}
	raw, err := s.vm.NewThread(fn)
	if err != nil {
		return nil, err
	}
	return &Thread{state: s, raw: raw}, nil
}

// ThreadOf wraps a Lua thread Value into a *Thread handle (returns nil if v is not a thread or s is closed).
func (s *State) ThreadOf(v Value) *Thread {
	if !s.ready() || !v.IsThread() {
		return nil
	}
	return &Thread{state: s, raw: v.AsThread()}
}

// Value returns the underlying Lua thread Value (Nil if th is nil or its State is closed).
func (th *Thread) Value() Value {
	if !th.ready() {
		return vm.NilValue()
	}
	return vm.ThreadValue(th.raw)
}

// Raw returns the underlying *vm.Thread (nil if th is nil or its State is closed).
func (th *Thread) Raw() *vm.Thread {
	if !th.ready() {
		return nil
	}
	return th.raw
}

// Status returns the internal ThreadStatus of th (ThreadDead if nil or closed).
// Note that a freshly created Thread has internal status ThreadOK before its first
// Resume, while StatusString reports "suspended" per Lua's coroutine.status().
func (th *Thread) Status() ThreadStatus {
	if !th.ready() {
		return ThreadDead
	}
	return th.raw.Status()
}

// StatusString returns Lua's coroutine.status(co) string ("suspended", "running", "normal", or "dead").
func (th *Thread) StatusString() string {
	if !th.ready() {
		return "dead"
	}
	return th.raw.StatusString()
}

// Resume starts or resumes th with args. When err is nil, yielded is true if th suspended
// via coroutine.yield(results...) and false if th returned normally with results.
// Uncatchable cancellation (WithContext / WithInstructionLimit) propagates directly as err.
func (th *Thread) Resume(args ...Value) (results []Value, yielded bool, err error) {
	if !th.ready() {
		return nil, false, ErrStateClosed
	}
	return th.state.vm.Resume(th.raw, args...)
}

// Close closes a suspended or dead coroutine and executes any pending <close>
// to-be-closed variable metamethods (analogous to coroutine.close).
func (th *Thread) Close() error {
	if !th.ready() {
		return ErrStateClosed
	}
	return th.state.vm.CloseThread(th.raw)
}

// FormatValue formats any Lua Value as a Go string (matching tostring without metamethods).
func FormatValue(v Value) string { return vm.FormatValue(v) }
