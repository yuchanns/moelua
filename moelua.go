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
	s.vm.AddSearcher(&vm.NativeFunc{
		Name: name,
		Fn: func(_ *vm.State, args []vm.Value) ([]vm.Value, error) {
			return fn(s, args)
		},
	})
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

// Compile compiles Lua 5.5.1 source text into a verified immutable *Chunk.
func Compile(name, src string) (*Chunk, error) {
	p, err := compiler.Compile(name, src)
	if err != nil {
		return nil, err
	}
	return &Chunk{proto: p}, nil
}

// LoadChunk decodes and verifies a PUC-Rio Lua 5.5.1 binary chunk.
// Binary chunks are trusted input; verification provides panic-freedom defense in depth.
func LoadChunk(binary []byte) (*Chunk, error) {
	p, err := bytecode.Undump(binary)
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

// String interns or wraps a Go string as a Lua string Value owned by s.
func (s *State) String(str string) Value {
	if !s.ready() {
		return vm.NilValue()
	}
	return vm.StringValue(s.vm.Intern(str))
}

// NewUserdata creates a full Lua userdata carrying arbitrary Go data.
func (s *State) NewUserdata(data any) Value {
	if !s.ready() {
		return vm.NilValue()
	}
	return vm.UserdataValue(&vm.Userdata{Data: data})
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

// NewFunc wraps a GoFunc as a Lua callable Value.
func (s *State) NewFunc(name string, fn GoFunc) Value {
	if !s.ready() || fn == nil {
		return vm.NilValue()
	}
	return vm.NativeValue(&vm.NativeFunc{
		Name: name,
		Fn: func(_ *vm.State, args []vm.Value) ([]vm.Value, error) {
			return fn(s, args)
		},
	})
}

// NewFunc1 wraps a single-return GoFunc1 as a Lua callable Value with zero per-call return-slice allocation.
func (s *State) NewFunc1(name string, fn GoFunc1) Value {
	if !s.ready() || fn == nil {
		return vm.NilValue()
	}
	return vm.NativeValue(&vm.NativeFunc{
		Name: name,
		FastFn: func(_ *vm.State, args []vm.Value) (vm.Value, error) {
			return fn(s, args)
		},
	})
}

// RegisterFunc installs a GoFunc as a global variable in _G.
func (s *State) RegisterFunc(name string, fn GoFunc) {
	if !s.ready() || fn == nil {
		return
	}
	s.vm.SetGlobal(name, s.NewFunc(name, fn))
}

// RegisterFunc1 installs a zero-allocation single-return GoFunc1 as a global variable in _G.
func (s *State) RegisterFunc1(name string, fn GoFunc1) {
	if !s.ready() || fn == nil {
		return
	}
	s.vm.SetGlobal(name, s.NewFunc1(name, fn))
}

// SetGlobal sets a global variable in _G.
func (s *State) SetGlobal(name string, v Value) {
	if s.ready() {
		s.vm.SetGlobal(name, v)
	}
}

// GetGlobal retrieves a global variable from _G.
func (s *State) GetGlobal(name string) Value {
	if !s.ready() {
		return vm.NilValue()
	}
	return s.vm.GetGlobal(name)
}

// Compile compiles Lua 5.5.1 source text into a *Chunk.
func (s *State) Compile(name, src string) (*Chunk, error) {
	return Compile(name, src)
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
	ch, err := Compile(name, src)
	if err != nil {
		return nil, err
	}
	fn, err := s.Load(ch)
	if err != nil {
		return nil, err
	}
	return s.Call(fn)
}

// DoFile loads and executes a Lua 5.5.1 source or binary file (handling UTF-8 BOM and initial '#' comment).
func (s *State) DoFile(path string) ([]Value, error) {
	if !s.ready() {
		return nil, ErrStateClosed
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s: %w", path, err)
	}
	return s.DoBytes("@"+path, data)
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
func (s *State) NewTable(nseq, nrec int) *Table {
	if !s.ready() {
		return nil
	}
	return &Table{state: s, raw: s.vm.NewTable(nseq, nrec)}
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

// Set performs rawset(t, key, val).
func (t *Table) Set(key, val Value) error {
	if !t.ready() {
		return ErrStateClosed
	}
	return t.raw.RawSet(t.state.vm, key, val)
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
	t.raw.RawSetInt(t.state.vm, i, val)
	return t.state.vm.ConsumeMemErr()
}

// GetString returns rawget(t, key).
func (t *Table) GetString(key string) Value {
	if !t.ready() {
		return vm.NilValue()
	}
	return t.raw.RawGetStr(t.state.vm.Intern(key))
}

// SetString performs rawset(t, key, val), returning an error if the State is closed or memory limit is exceeded.
func (t *Table) SetString(key string, val Value) error {
	if !t.ready() {
		return ErrStateClosed
	}
	t.raw.RawSetStr(t.state.vm, t.state.vm.Intern(key), val)
	return t.state.vm.ConsumeMemErr()
}

// FormatValue formats any Lua Value as a Go string (matching tostring without metamethods).
func FormatValue(v Value) string { return vm.FormatValue(v) }
