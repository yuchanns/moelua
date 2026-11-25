package vm_test

import (
	"bytes"
	"testing"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
	"github.com/yuchanns/moelua/vm"
)

func newBoundedFuzzState(stepLimit int) (*vm.State, error) {
	s := vm.NewStateNoEnv()
	var sink bytes.Buffer
	s.SetOutput(&sink)
	s.SetErrOutput(&sink)
	// Restrict OS/IO/filesystem side effects across globals, package.loaded, and package.searchers.
	s.SetGlobal("io", vm.NilValue())
	s.SetGlobal("os", vm.NilValue())
	s.SetGlobal("dofile", vm.NilValue())
	s.SetGlobal("loadfile", vm.NilValue())
	if pkgVal := s.GetGlobal("package"); pkgVal.IsTable() {
		pkg := pkgVal.AsTable()
		if loadedVal := pkg.RawGetStr(s.Intern("loaded")); loadedVal.IsTable() {
			loaded := loadedVal.AsTable()
			loaded.RawSetStr(s, s.Intern("io"), vm.NilValue())
			loaded.RawSetStr(s, s.Intern("os"), vm.NilValue())
		}
		if searchersVal := pkg.RawGetStr(s.Intern("searchers")); searchersVal.IsTable() {
			// Keep only searcher_preload (slot 1); drop filesystem/C searchers.
			searchers := searchersVal.AsTable()
			searchers.RawSetInt(s, 2, vm.NilValue())
			searchers.RawSetInt(s, 3, vm.NilValue())
		}
	}
	// Enforce uncatchable instruction and memory limits so fuzz inputs cannot hang or OOM.
	s.SetMemoryLimit(2 * 1024 * 1024)
	s.SetInstructionLimit(int64(stepLimit))
	return s, nil
}

var fuzzSeedScripts = []string{
	`local s = 0; for i = 1, 20 do s = s + i end; return s`,
	`local t = {a = 1, b = 2, [3] = 4}; for k, v in pairs(t) do t[k] = v end`,
	`local function f(x, ...) return x, select("#", ...), ... end; return f(1, 2, 3)`,
	`local co = coroutine.create(function(x) local y = coroutine.yield(x+1); return y*2 end); coroutine.resume(co, 5); coroutine.resume(co, 7)`,
	`local x <close> = setmetatable({}, {__close = function(self, err) return err end}); return x`,
	`local t = setmetatable({}, {__gc = function() end}); t = nil; collectgarbage()`,
	`local a = setmetatable({}, {__index = function(_, k) return k end}); return a.foo`,
	`return string.format("%d %.2f %q %x", 42, 3.14, "hi\n", 255)`,
	`local p = string.pack("<i4s2", -123, "ok"); return string.unpack("<i4s2", p)`,
	`return string.gsub("hello world 123", "(%a+)", string.upper)`,
	`return utf8.len("hello 世界 🌍"), utf8.codepoint("世界", 1, -1)`,
	`local t = {5, 2, 8, 1, 9}; table.sort(t); return table.concat(t, ",")`,
	`local ok, err = xpcall(function() error("boom") end, debug.traceback); return ok, err`,
	`local function f(n) if n <= 0 then return 0 end return f(n-1) end; return f(50)`,
	`local function g(...) local t = {...}; return table.unpack(t) end; return g(1, nil, 3)`,
}

// FuzzBytecode mutates raw Lua 5.5.1 binary chunks and asserts that Undump + Verify + bounded VM execution
// NEVER trigger a Go runtime panic (bounds check, nil pointer, slice OOB, etc.).
func FuzzBytecode(f *testing.F) {
	for _, src := range fuzzSeedScripts {
		p, err := compiler.Compile("=seed", src)
		if err != nil {
			continue
		}
		if dump, err := bytecode.Dump(p, false); err == nil {
			f.Add(dump)
			if len(dump) > 10 {
				f.Add(dump[:len(dump)/2])
			}
		}
		if dump, err := bytecode.Dump(p, true); err == nil {
			f.Add(dump)
		}
	}
	f.Add([]byte("\x1bLua\x55\x00"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := bytecode.Undump(data)
		if err != nil {
			return
		}
		s, err := newBoundedFuzzState(2000)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		cl, err := s.Load(p)
		if err != nil {
			return
		}
		_, _ = s.Call(vm.ClosureValue(cl))
	})
}

// FuzzRun feeds arbitrary UTF-8/byte source strings into compiler.Compile + bounded VM execution
// and asserts zero Go panics across lexer, parser, codegen, verifier, and interpreter.
func FuzzRun(f *testing.F) {
	for _, src := range fuzzSeedScripts {
		f.Add(src)
	}
	f.Add(`local a = 1 + "2" * 3.5 // 2 % 4 ^ 2`)
	f.Add(`global x = 1; local <const> y = 2; return x + y`)
	f.Add(`goto done; ::done:: return true`)

	f.Fuzz(func(t *testing.T, src string) {
		p, err := compiler.Compile("=fuzz", src)
		if err != nil {
			return
		}
		if err := bytecode.Verify(p); err != nil {
			t.Fatalf("compiler emitted invalid bytecode for %q: %v", src, err)
		}
		s, err := newBoundedFuzzState(2000)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		cl, err := s.Load(p)
		if err != nil {
			return
		}
		_, _ = s.Call(vm.ClosureValue(cl))
	})
}

// FuzzStructuredProto synthesizes structure-aware *bytecode.Proto values
// from raw fuzzer bytes and enforces the oracle:
//   "Any Proto that passes bytecode.Verify executes under regcheck within an instruction
//    and memory budget with ZERO regcheck violations and ZERO Go panics."
func FuzzStructuredProto(f *testing.F) {
	f.Add([]byte{4, 1, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15})
	f.Add([]byte{8, 2, 0, 0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80, 0x90})
	f.Add([]byte{6, 3, 1, 30, 1, 2, 3, 35, 0, 1, 2, 46, 0, 2, 1, 70, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 6 {
			return
		}
		maxStack := uint8(2 + data[0]%30)
		numParams := data[1] % maxStack
		flag := uint8(0)
		if data[2]&1 != 0 {
			flag = bytecode.PFVarargHidden
		} else if data[2]&2 != 0 {
			flag = bytecode.PFVarargTable
		}
		consts := []bytecode.Constant{
			{Kind: bytecode.ConstNil},
			{Kind: bytecode.ConstTrue},
			{Kind: bytecode.ConstFalse},
			{Kind: bytecode.ConstInt, Int: 0},
			{Kind: bytecode.ConstInt, Int: 1},
			{Kind: bytecode.ConstInt, Int: -1},
			{Kind: bytecode.ConstFloat, Float: 0.0},
			{Kind: bytecode.ConstFloat, Float: 1.5},
			{Kind: bytecode.ConstString, Str: "a"},
			{Kind: bytecode.ConstString, Str: "b"},
			{Kind: bytecode.ConstString, Str: "print"},
		}
		ninsns := min((len(data)-3)/4, 64)
		if ninsns == 0 {
			return
		}
		code := make([]bytecode.Instruction, 0, ninsns+2)
		if flag != 0 {
			code = append(code, bytecode.Instruction(bytecode.OP_VARARGPREP)|(bytecode.Instruction(numParams)<<bytecode.PosA))
		}
		for i := range ninsns {
			off := 3 + i*4
			op := bytecode.OpCode(data[off] % byte(bytecode.NumOpcodes))
			a := uint32(data[off+1]) % uint32(maxStack)
			b := uint32(data[off+2])
			c := uint32(data[off+3])
			k := uint32((data[off] >> 7) & 1)
			w := bytecode.Instruction(op) |
				(bytecode.Instruction(a) << bytecode.PosA) |
				(bytecode.Instruction(k) << bytecode.PosK) |
				(bytecode.Instruction(b&0xff) << bytecode.PosB) |
				(bytecode.Instruction(c&0xff) << bytecode.PosC)
			code = append(code, w)
		}
		code = append(code, bytecode.Instruction(bytecode.OP_RETURN0))
		p := &bytecode.Proto{
			Source:       "=structfuzz",
			NumParams:    numParams,
			Flag:         flag,
			MaxStackSize: maxStack,
			Code:         code,
			Consts:       consts,
			Upvalues:     []bytecode.UpvalDesc{{Name: "_ENV", InStack: 1, Idx: 0}},
		}
		if err := bytecode.Verify(p); err != nil {
			return
		}
		s, err := newBoundedFuzzState(1500)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		cl, err := s.Load(p)
		if err != nil {
			return
		}
		_, _ = s.Call(vm.ClosureValue(cl))
	})
}

func TestFuzzStateSandboxGuards(t *testing.T) {
	// 1. debug.sethook() cannot disable the fuzz instruction budget.
	s, err := newBoundedFuzzState(2000)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cl, err := s.LoadChunk([]byte(`debug.sethook(); while true do end`), "=fuzz_loop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(cl); err == nil {
		t.Fatalf("expected instruction limit error")
	}

	// 2. require('os') and package.loaded.io are removed from fuzz state.
	s2, err := newBoundedFuzzState(2000)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	cl2, err := s2.LoadChunk([]byte(`
		assert(package.loaded.os == nil and package.loaded.io == nil)
		local ok = pcall(require, "os")
		assert(not ok, "require('os') should fail in fuzz state")
	`), "=fuzz_pkg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Call(cl2); err != nil {
		t.Fatalf("fuzz package isolation check failed: %v", err)
	}

	// 3. Memory limit bounds string.rep allocations in fuzz state.
	s3, err := newBoundedFuzzState(2000)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	cl3, err := s3.LoadChunk([]byte(`
		local ok, err = pcall(function() return string.rep("x", 10 * 1024 * 1024) end)
		assert(not ok and tostring(err):find("not enough memory", 1, true), tostring(err))
	`), "=fuzz_mem")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s3.Call(cl3); err != nil {
		t.Fatalf("fuzz memory limit check failed: %v", err)
	}
}
