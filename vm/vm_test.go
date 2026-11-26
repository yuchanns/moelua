package vm_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"weak"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
	"github.com/yuchanns/moelua/vm"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}

func hasCLua55(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(repoRoot(t), "build", "lua55_o2"))
	return err == nil
}

func compileLua55(t *testing.T, src string) []byte {
	t.Helper()
	root := repoRoot(t)
	luac := filepath.Join(root, "build", "luac55")
	if _, err := os.Stat(luac); err != nil {
		p, cerr := compiler.Compile("=test", src)
		if cerr != nil {
			t.Fatalf("pure-Go compiler failed: %v", cerr)
		}
		b, derr := bytecode.Dump(p, false)
		if derr != nil {
			t.Fatalf("bytecode.Dump failed: %v", derr)
		}
		return b
	}
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.lua")
	outPath := filepath.Join(dir, "out.luac")
	if err := os.WriteFile(inPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(luac, "-o", outPath, inPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("luac55 failed: %v\n%s", err, out)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func runCLua55(t *testing.T, src string) string {
	t.Helper()
	root := repoRoot(t)
	luaBin := filepath.Join(root, "build", "lua55_o2")
	if _, err := os.Stat(luaBin); err != nil {
		t.Skipf("lua55_o2 missing at %s (run `make tools`): %v", luaBin, err)
	}
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.lua")
	if err := os.WriteFile(inPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(luaBin, inPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lua55_o2 failed: %v\n%s", err, out)
	}
	return string(out)
}

func runMoeLua(t *testing.T, src string) string {
	t.Helper()
	chunk := compileLua55(t, src)
	s := vm.NewState()
	var buf bytes.Buffer
	s.SetOutput(&buf)
	cl, err := s.UndumpAndLoad(chunk)
	if err != nil {
		t.Fatalf("UndumpAndLoad failed: %v", err)
	}
	if _, err := s.Call(vm.ClosureValue(cl)); err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	return buf.String()
}

func TestDifferentialLua55(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "arithmetic_bitwise_loops",
			src: `
local sum = 0
for i = 1, 100 do
  sum = sum + (i * 3 - i // 2 + i % 7)
end
local fsum = 0.0
for x = 0.5, 5.0, 0.5 do
  fsum = fsum + x * 1.5
end
local bits = (0xdeadbeef ~ 0x12345678) & 0xffffffff
local big = math.maxinteger - 5
local wrap = math.maxinteger + 1
print(sum, fsum, bits, (1 << 62), (big >> 3), wrap == math.mininteger)
`,
		},
		{
			name: "oop_metatable_ic_invalidation",
			src: `
local Vec = {}
Vec.__index = Vec
function Vec.new(x, y)
  return setmetatable({x = x, y = y}, Vec)
end
function Vec:norm2()
  return self.x * self.x + self.y * self.y
end
function Vec:add(other)
  return Vec.new(self.x + other.x, self.y + other.y)
end
local a = Vec.new(3, 4)
local b = Vec.new(1, 2)
local c = a:add(b)
print(a:norm2(), c:norm2())
-- Dynamically mutate method on prototype: IC must invalidate via protoEpoch!
function Vec:norm2()
  return self.x + self.y
end
print(a:norm2(), c:norm2())
-- Shadow method on instance: Shape transition must invalidate IC!
a.norm2 = function(self) return 999 end
print(a:norm2(), c:norm2())
`,
		},
		{
			name: "lua55_features_global_vararg_tablecreate",
			src: `
global *
global G_VAL = 42
global function addG(x)
  return x + G_VAL
end
local function sumv(...t)
  local s = 0
  for i = 1, t.n do
    s = s + t[i]
  end
  return s, t.n
end
local function sumv_escape(...t)
  local function get(i) return t[i] end
  return get(1) + get(2) + t.n
end
local arr = table.create(10, 0)
for i = 1, 10 do arr[i] = i * i end
local total = 0
for i, v in ipairs(arr) do
  total = total + i + v
end
print(addG(8), sumv(10, 20, 30), sumv_escape(5, 7), #arr, total)
print(tostring(1.1), tostring(1.0), tostring(1/3))
`,
		},
		{
			name: "closures_tailcall_pcall_sort",
			src: `
local function makeCounter(start)
  local n = start
  return function(step)
    n = n + (step or 1)
    return n
  end
end
local c1 = makeCounter(10)
local c2 = makeCounter(100)
print(c1(), c1(5), c2(2), c1())

local function sumTail(n, acc)
  if n == 0 then return acc end
  return sumTail(n - 1, acc + n)
end
print(sumTail(50000, 0))

local ok1, v1 = pcall(function() return 7, 8 end)
local ok2, e2 = pcall(function() error("boom") end)
print(ok1, v1, ok2, e2 ~= nil)

local t = {9, 3, 7, 1, 8, 2, 6, 4, 5}
table.sort(t)
print(table.concat(t, ","))
`,
		},
		{
			name: "typed_array_transitions_and_holes",
			src: `
local a = {}
for i = 1, 5 do a[i] = i * 10 end
a[3] = 3.5 -- int -> generic promotion
a[6] = "hi"
print(a[1], a[3], a[5], a[6], #a)

local f = table.create(4)
for i = 1, 4 do f[i] = i * 0.5 end
f[4] = nil -- pop tail stays float
print(#f, f[1], f[3], f[4])
`,
		},
	}

	t.Logf("Running differential tests in mode=%s polarity=%s", vm.ModeName, vm.PolarityName)
	haveC := hasCLua55(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runMoeLua(t, tc.src)
			if got == "" {
				t.Fatalf("expected non-empty output from MoeLua")
			}
			if !haveC {
				return
			}
			want := runCLua55(t, tc.src)
			if got != want {
				t.Fatalf("stdout mismatch vs C Lua 5.5.1:\n--- want (C) ---\n%s--- got (MoeLua) ---\n%s", want, got)
			}
		})
	}
}

// TestDeletedCollectableHashKeyReclaimedInTier0 verifies that deleting a collectable hash key
// drops the strong Go pointer immediately via a weak tombstone so Go GC reclaims the deleted
// key graph in Tier 0 without waiting for rehash or State.CollectFull, while preserving
// active pairs()/next() traversal while the key remains live.
func TestDeletedCollectableHashKeyReclaimedInTier0(t *testing.T) {
	s := vm.NewState()
	defer s.Close()

	var canary vm.Value
	func() {
		cl, err := s.LoadChunk([]byte(`
			T = {}
			local k1 = { payload = string.rep("x", 65536) }
			local k2 = { name = "keep" }
			T[k1] = 1
			T[k2] = 2
			-- Deleting during pairs() traversal must keep next(T, k1) working while k1 is live.
			local seen = 0
			for k, _ in pairs(T) do
				seen = seen + 1
				if k == k1 then
					T[k] = nil
				end
			end
			assert(seen == 2, "expected to visit both entries while deleting k1, got " .. seen)
			assert(T[k1] == nil and T[k2] == 2)
			return k1
		`), "=weak_tombstone")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(cl)
		if err != nil {
			t.Fatal(err)
		}
		canary = res[0]
		res[0] = vm.NilValue()
	}()

	s.CollectFull()
	if s.IsLive(canary) {
		t.Fatalf("deleted collectable hash key remained strongly retained by tombstone after CollectFull")
	}
}

// TestNativeResultRootedAcrossStackGrowthEmergencyGC verifies that values returned by a native
// callback remain rooted if growing the result stack triggers emergency GC under a memory limit,
// covering both direct State.Call and Lua-side placeResults (MULTRET).
func TestNativeResultRootedAcrossStackGrowthEmergencyGC(t *testing.T) {
	runCase := func(t *testing.T, viaLua bool) {
		t.Helper()
		s := vm.NewState()
		defer s.Close()

		// Enable Tier 1 via a weak-value table W and hold 512 KiB of droppable garbage G
		// plus a canary table C so we can prove emergency GC ran during stack growth.
		setup, err := s.LoadChunk([]byte(`
			collectgarbage("param", "pause", 100)
			W = setmetatable({}, { __mode = "v" })
			G = string.rep("g", 512 * 1024)
			C = {}
			W.c = C
		`), "=setup")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(setup); err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(64 << 20)
		s.CollectFull()
		baseMem := s.MemoryUsage()

		kStr := s.Intern("k")
		cStr := s.Intern("c")
		wTbl := s.AsTable(s.GetGlobal("W"))
		cb := s.NativeValue(&vm.NativeFunc{
			Name: "make_many",
			Fn: func(st *vm.State, _ []vm.Value) ([]vm.Value, error) {
				// Release 512 KiB garbage + canary so emergency GC during stack growth succeeds
				// and clears W.c.
				st.SetGlobal("G", vm.NilValue())
				st.SetGlobal("C", vm.NilValue())
				// Allocate a fresh table reachable only via the returned slice and weak W.k.
				tbl, err := st.TryNewTable(8, 0)
				if err != nil {
					return nil, err
				}
				tbl.RawSetInt(st, 1, vm.IntValue(42))
				if err := st.ConsumeMemErr(); err != nil {
					return nil, err
				}
				wTbl.RawSetStr(st, kStr, vm.TableValue(tbl))
				if err := st.ConsumeMemErr(); err != nil {
					return nil, err
				}
				// Push Tier-1 debt past 64 KiB threshold without tripping baseMem+96 KiB yet,
				// so State.Call enters its post-native GC check and grows th.stack for 5000 values.
				if _, err := st.TryIntern(string(bytes.Repeat([]byte{'d'}, 70*1024))); err != nil {
					return nil, err
				}
				res := make([]vm.Value, 5000)
				for i := range res {
					res[i] = vm.TableValue(tbl)
				}
				return res, nil
			},
		})
		s.SetGlobal("make_many", cb)

		// Tight memory limit: 70 KiB debt allocation fits, but growing th.stack for 5000
		// results (>= 64 KiB) exceeds baseMem+96 KiB and forces emergency GC while res is
		// still a Go-local slice.
		s.SetMemoryLimit(baseMem + 96*1024)

		if !viaLua {
			res, err := s.Call(cb)
			if err != nil {
				t.Fatalf("State.Call(cb) failed: %v", err)
			}
			if len(res) != 5000 || !res[0].IsTable() {
				t.Fatalf("unexpected result slice len=%d type=%s", len(res), res[0].TypeName())
			}
			if !wTbl.RawGetStr(s, cStr).IsNil() {
				t.Fatalf("expected stack growth to trigger emergency GC and clear canary W.c")
			}
			gotWeak := wTbl.RawGetStr(s, kStr)
			if !gotWeak.IsTable() || s.AsTable(gotWeak) != s.AsTable(res[0]) {
				t.Fatalf("weak-value entry cleared during stack-growth emergency GC in State.Call: got %s", gotWeak.TypeName())
			}
		} else {
			// Stop automatic GC so ONLY emergency GC during th.placeResults stack growth can clear W.c.
			// select(1, make_many()) compiles make_many() as MULTRET (nresults = -1), forcing
			// th.placeResults to grow th.stack for all 5000 return values.
			caller, err := s.LoadChunk([]byte(`
				collectgarbage("stop")
				local t = select(1, make_many())
				assert(type(t) == "table" and t[1] == 42)
				assert(W.c == nil, "expected stack growth to trigger emergency GC and clear canary W.c")
				assert(W.k == t, "weak-value entry cleared during stack-growth emergency GC in Lua call")
				return t
			`), "=lua_call")
			if err != nil {
				t.Fatal(err)
			}
			callerRef := s.Ref(caller)
			s.SetMemoryLimit(64 << 20)
			s.CollectFull()
			baseMem = s.MemoryUsage()
			s.SetMemoryLimit(baseMem + 96*1024)
			_, callErr := s.Call(caller)
			s.Unref(callerRef)
			if callErr != nil {
				t.Fatalf("Lua call failed: %v", callErr)
			}
		}
	}

	t.Run("state_call_direct", func(t *testing.T) { runCase(t, false) })
	t.Run("lua_call_place_results", func(t *testing.T) { runCase(t, true) })
}

// TestStringFormatResolvesToStringMetamethodSequentially verifies that string.format("%s", ...)
// resolves primitive __tostring metamethods lazily when each %s specifier is consumed, matching
// Lua 5.5.1 luaL_tolstring sequential evaluation.
func TestStringFormatResolvesToStringMetamethodSequentially(t *testing.T) {
	src := `
local savedStrMT = getmetatable("")
local savedStrToStr = savedStrMT.__tostring

local installer = setmetatable({}, {
  __tostring = function()
    debug.setmetatable(0, { __tostring = function(x) return "NUM:" .. (x + 1) end })
    debug.setmetatable(true, { __tostring = function(x) return x and "YES" or "NO" end })
    debug.setmetatable(nil, { __tostring = function() return "EMPTY" end })
    savedStrMT.__tostring = function(x) return "S(" .. x .. ")" end
    return "first"
  end
})
local a = string.format("%s|%s|%s|%s|%s|%s", installer, 7, 1.5, true, nil, "abc")

local remover = setmetatable({}, {
  __tostring = function()
    debug.setmetatable(0, nil)
    debug.setmetatable(true, nil)
    debug.setmetatable(nil, nil)
    savedStrMT.__tostring = savedStrToStr
    return "clear"
  end
})
local b = string.format("%s|%s|%s|%s|%s", remover, 7, true, nil, "abc")
print(a)
print(b)
`
	got := runMoeLua(t, src)
	want := "first|NUM:8|NUM:2.5|YES|EMPTY|S(abc)\nclear|7|true|nil|abc\n"
	if got != want {
		t.Fatalf("string.format sequential __tostring mismatch:\nwant:\n%s\ngot:\n%s", want, got)
	}
	if hasCLua55(t) {
		cWant := runCLua55(t, src)
		if got != cWant {
			t.Fatalf("mismatch vs C Lua 5.5.1:\nwant:\n%s\ngot:\n%s", cWant, got)
		}
	}
}

// TestNativeNumericCoercionCancelAndBudget verifies that native numeric argument coercion
// (math.*, string.sub/format/pack, table.create, utf8.offset, debug.*, os.*, collectgarbage)
// polls both instruction-budget and context-cancellation controls on huge numeral strings.
func TestNativeNumericCoercionCancelAndBudget(t *testing.T) {
	bigZeros := strings.Repeat("0", 4*1024*1024)
	cases := []struct {
		name string
		src  string
	}{
		{"math_sqrt", `return math.sqrt(BIG)`},
		{"math_floor", `return math.floor(BIG)`},
		{"math_ldexp", `return math.ldexp(1, BIG)`},
		{"string_sub", `return string.sub("abcdef", BIG)`},
		{"string_format_d", `return string.format("%d", BIG)`},
		{"string_format_f", `return string.format("%f", BIG)`},
		{"string_pack_i", `return string.pack("i", BIG)`},
		{"string_pack_d", `return string.pack("d", BIG)`},
		{"utf8_offset", `return utf8.offset("abcdef", BIG)`},
		{"table_create", `return table.create(BIG)`},
		{"debug_getuservalue", `return debug.getuservalue(nil, BIG)`},
		{"os_date", `return os.date("%Y", BIG)`},
		{"collectgarbage_step", `return collectgarbage("step", BIG)`},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/insn_budget", func(t *testing.T) {
			s := vm.NewState()
			defer s.Close()
			s.SetGlobal("BIG", vm.StringValue(s.Intern(bigZeros)))
			cl, err := s.LoadChunk([]byte(tc.src), "="+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			s.SetInstructionLimit(100)
			if _, err := s.Call(cl); err == nil {
				t.Fatalf("%s unexpectedly succeeded past instruction budget 100 (executed=%d)", tc.name, s.InstructionsExecuted())
			}
			if s.InstructionsExecuted() == 0 {
				t.Fatalf("%s charged 0 instructions during huge numeral coercion", tc.name)
			}
		})
		t.Run(tc.name+"/ctx_canceled", func(t *testing.T) {
			s := vm.NewState()
			defer s.Close()
			s.SetGlobal("BIG", vm.StringValue(s.Intern(bigZeros)))
			cl, err := s.LoadChunk([]byte(tc.src), "="+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			s.SetContext(ctx)
			start := time.Now()
			if _, err := s.Call(cl); err == nil {
				t.Fatalf("%s unexpectedly succeeded with expired context deadline", tc.name)
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Fatalf("%s took %v despite already-expired context", tc.name, elapsed)
			}
		})
	}

	// Direct State.Call into math.sqrt (zero Lua bytecode instructions executed) matching exact
	// host-to-native invocation path under both instruction limit and expired context deadline.
	t.Run("direct_state_call_math_sqrt", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		mathSqrt := s.AsTable(s.GetGlobal("math")).RawGetStr(s, s.Intern("sqrt"))
		bigVal := vm.StringValue(s.Intern(bigZeros))

		s.SetInstructionLimit(100)
		if _, err := s.Call(mathSqrt, bigVal); err == nil {
			t.Fatalf("direct State.Call(math.sqrt, bigZeros) bypassed instruction limit 100")
		}
		if s.InstructionsExecuted() == 0 {
			t.Fatalf("direct State.Call(math.sqrt, bigZeros) charged 0 instructions")
		}

		s2 := vm.NewState()
		defer s2.Close()
		mathSqrt2 := s2.AsTable(s2.GetGlobal("math")).RawGetStr(s2, s2.Intern("sqrt"))
		bigVal2 := vm.StringValue(s2.Intern(bigZeros))
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		s2.SetContext(ctx)
		if _, err := s2.Call(mathSqrt2, bigVal2); err == nil {
			t.Fatalf("direct State.Call(math.sqrt, bigZeros) bypassed expired context deadline")
		}
	})
}

// TestLongStringComparisonAndEqualityCancelAndBudget verifies that comparing or equality-checking
// long strings sharing a large prefix polls both instruction-budget and context-cancellation
// controls across math.min/max, table.sort, <, <=, ==, rawequal, and table lookups.
func TestLongStringComparisonAndEqualityCancelAndBudget(t *testing.T) {
	prefix := strings.Repeat("a", 8*1024*1024)
	sA := prefix + "1"
	sB := prefix + "2"
	sA2 := strings.Clone(sA)

	cases := []struct {
		name string
		src  string
	}{
		{"math_min", `return math.min(A, B)`},
		{"math_max", `return math.max(A, B)`},
		{"table_sort", `local t = {B, A}; table.sort(t); return t[1]`},
		{"op_lt", `return A < B`},
		{"op_le", `return A <= B`},
		{"op_eq", `return A == A2`},
		{"rawequal", `return rawequal(A, A2)`},
		{"table_lookup_prehashed", `return T[A2]`},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/insn_budget", func(t *testing.T) {
			s := vm.NewState()
			defer s.Close()
			vA := vm.StringValue(s.Intern(sA))
			vB := vm.StringValue(s.Intern(sB))
			vA2 := vm.StringValue(s.Intern(sA2))
			s.SetGlobal("A", vA)
			s.SetGlobal("B", vB)
			s.SetGlobal("A2", vA2)
			// Pre-populate and pre-hash T[A] and T2[A2] outside the budget so table_lookup_prehashed
			// specifically exercises long-string key equality comparison on hash collision/match.
			pre, err := s.LoadChunk([]byte(`T = { [A] = 1 }; local _ = ({ [A2] = 1 })[A2]`), "=pre")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Call(pre); err != nil {
				t.Fatal(err)
			}
			cl, err := s.LoadChunk([]byte(tc.src), "="+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			s.SetInstructionLimit(100)
			if _, err := s.Call(cl); err == nil {
				t.Fatalf("%s unexpectedly succeeded past instruction limit 100 (executed=%d)", tc.name, s.InstructionsExecuted())
			}
			if s.InstructionsExecuted() == 0 {
				t.Fatalf("%s charged 0 instructions during huge string comparison", tc.name)
			}
		})
		t.Run(tc.name+"/ctx_canceled", func(t *testing.T) {
			s := vm.NewState()
			defer s.Close()
			vA := vm.StringValue(s.Intern(sA))
			vB := vm.StringValue(s.Intern(sB))
			vA2 := vm.StringValue(s.Intern(sA2))
			s.SetGlobal("A", vA)
			s.SetGlobal("B", vB)
			s.SetGlobal("A2", vA2)
			pre, err := s.LoadChunk([]byte(`T = { [A] = 1 }; local _ = ({ [A2] = 1 })[A2]`), "=pre")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Call(pre); err != nil {
				t.Fatal(err)
			}
			cl, err := s.LoadChunk([]byte(tc.src), "="+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			s.SetContext(ctx)
			start := time.Now()
			if _, err := s.Call(cl); err == nil {
				t.Fatalf("%s unexpectedly succeeded with expired context deadline", tc.name)
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Fatalf("%s took %v despite already-expired context", tc.name, elapsed)
			}
		})
	}

	// Direct State.Call(math.min, A, B) matching exact host-to-native path (0 Lua bytecode ops).
	t.Run("direct_state_call_math_min", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		mathMin := s.AsTable(s.GetGlobal("math")).RawGetStr(s, s.Intern("min"))
		vA := vm.StringValue(s.Intern(sA))
		vB := vm.StringValue(s.Intern(sB))
		s.SetInstructionLimit(100)
		if _, err := s.Call(mathMin, vA, vB); err == nil {
			t.Fatalf("direct State.Call(math.min, A, B) bypassed instruction limit 100")
		}
		if s.InstructionsExecuted() == 0 {
			t.Fatalf("direct State.Call(math.min, A, B) charged 0 instructions")
		}
	})
}

// TestDeadCoroutineClearsStackAndFramesInTier0 verifies that completed and closed coroutines
// immediately drop stack slot and frame references so Go's GC reclaims discarded arguments and
// locals in Tier 0 without waiting for State.CollectFull, while errored coroutines preserve
// debug.traceback until coroutine.close.
func TestDeadCoroutineClearsStackAndFramesInTier0(t *testing.T) {
	runReclaimCheck := func(t *testing.T, src string) {
		t.Helper()
		s := vm.NewState()
		defer s.Close()

		var canary vm.Value
		func() {
			cl, err := s.LoadChunk([]byte(src), "=dead_coro")
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Call(cl)
			if err != nil {
				t.Fatal(err)
			}
			canary = res[0]
			res[0] = vm.NilValue()
		}()

		s.CollectFull()
		if s.IsLive(canary) {
			t.Fatalf("discarded coroutine argument/local remained retained by dead coroutine")
		}
	}

	t.Run("normal_lua_completion", func(t *testing.T) {
		runReclaimCheck(t, `
			CO = coroutine.create(function(x)
				local y = { x }
				return 1
			end)
			local arg = { payload = string.rep("z", 65536) }
			local ok, r = coroutine.resume(CO, arg)
			assert(ok and r == 1 and coroutine.status(CO) == "dead")
			return arg
		`)
	})

	t.Run("normal_native_completion", func(t *testing.T) {
		runReclaimCheck(t, `
			CO = coroutine.create(function(x) return x end)
			-- Test native-backed coroutine directly as well.
			CO = coroutine.create(type)
			local arg = { payload = string.rep("z", 65536) }
			local ok, r = coroutine.resume(CO, arg)
			assert(ok and r == "table" and coroutine.status(CO) == "dead")
			return arg
		`)
	})

	t.Run("yield_then_close", func(t *testing.T) {
		runReclaimCheck(t, `
			CO = coroutine.create(function(x)
				local y = x
				coroutine.yield(1)
				return y
			end)
			local arg = { payload = string.rep("z", 65536) }
			local ok, r = coroutine.resume(CO, arg)
			assert(ok and r == 1 and coroutine.status(CO) == "suspended")
			local cok = coroutine.close(CO)
			assert(cok and coroutine.status(CO) == "dead")
			return arg
		`)
	})

	t.Run("error_preserves_traceback_then_close_releases", func(t *testing.T) {
		runReclaimCheck(t, `
			CO = coroutine.create(function(x)
				local y = x
				error("boom")
				return y
			end)
			local arg = { payload = string.rep("z", 65536) }
			local ok = coroutine.resume(CO, arg)
			assert(not ok and coroutine.status(CO) == "dead")
			local tb = debug.traceback(CO)
			assert(string.find(tb, "dead_coro", 1, true), "expected error traceback before close, got: " .. tb)
			local cok = coroutine.close(CO)
			assert(not cok)
			return arg
		`)
	})

	t.Run("self_close", func(t *testing.T) {
		runReclaimCheck(t, `
			CO = coroutine.create(function(x)
				local y = x
				coroutine.close()
				return y
			end)
			local arg = { payload = string.rep("z", 65536) }
			local ok = coroutine.resume(CO, arg)
			assert(ok and coroutine.status(CO) == "dead")
			return arg
		`)
	})

	t.Run("wrap_error_auto_close", func(t *testing.T) {
		runReclaimCheck(t, `
			W = coroutine.wrap(function(x)
				local y = x
				error("wrap_boom")
				return y
			end)
			local arg = { payload = string.rep("z", 65536) }
			local ok = pcall(W, arg)
			assert(not ok)
			return arg
		`)
	})
}

// TestLargeSubstringCopyCancelAndBudget verifies that copying large substrings in string.sub,
// string.match captures, and string.unpack polls both instruction-budget and context-cancellation
// controls (including direct State.Call into string.sub with zero Lua opcodes).
func TestLargeSubstringCopyCancelAndBudget(t *testing.T) {
	bigStr := "a" + strings.Repeat("b", 8*1024*1024)

	t.Run("direct_state_call_string_sub", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		strSub := s.AsTable(s.GetGlobal("string")).RawGetStr(s, s.Intern("sub"))
		bigVal := vm.StringValue(s.Intern(bigStr))

		s.SetInstructionLimit(100)
		if _, err := s.Call(strSub, bigVal, vm.IntValue(2)); err == nil {
			t.Fatalf("direct State.Call(string.sub, bigStr, 2) bypassed instruction limit 100")
		}
		if s.InstructionsExecuted() == 0 {
			t.Fatalf("direct State.Call(string.sub, bigStr, 2) charged 0 instructions")
		}

		s2 := vm.NewState()
		defer s2.Close()
		strSub2 := s2.AsTable(s2.GetGlobal("string")).RawGetStr(s2, s2.Intern("sub"))
		bigVal2 := vm.StringValue(s2.Intern(bigStr))
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		s2.SetContext(ctx)
		start := time.Now()
		if _, err := s2.Call(strSub2, bigVal2, vm.IntValue(2)); err == nil {
			t.Fatalf("direct State.Call(string.sub, bigStr, 2) bypassed expired context deadline")
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("direct State.Call(string.sub) took %v despite expired context", elapsed)
		}
	})

	t.Run("lua_string_sub_and_unpack", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			src  string
		}{
			{"string_sub", `return string.sub(BIG, 2)`},
			{"string_unpack", `return string.unpack("s4", PACKED)`},
		} {
			s := vm.NewState()
			defer s.Close()
			s.SetGlobal("BIG", vm.StringValue(s.Intern(bigStr)))
			packCl, err := s.LoadChunk([]byte(`PACKED = string.pack("s4", BIG)`), "=pack")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Call(packCl); err != nil {
				t.Fatal(err)
			}
			cl, err := s.LoadChunk([]byte(tc.src), "="+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			s.SetInstructionLimit(100)
			if _, err := s.Call(cl); err == nil {
				t.Fatalf("%s bypassed instruction limit 100", tc.name)
			}
			if s.InstructionsExecuted() == 0 {
				t.Fatalf("%s charged 0 instructions", tc.name)
			}
		}
	})
}

// TestUnreachableSuspendedCoroutineReclaimedInTier0 verifies that detaching open upvalues on
// coroutine suspend/error breaks strong upvalue->Thread edges in Tier 0 so unreachable
// coroutines and unrelated locals/arguments are reclaimed by Go's GC without State.CollectFull,
// while live closures, debug.getlocal/setlocal, resume mutations, and <close> vars remain exact.
func TestUnreachableSuspendedCoroutineReclaimedInTier0(t *testing.T) {
	t.Run("reclaim_suspended_coro_keep_closure_live", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()

		var canary vm.Value
		func() {
			cl, err := s.LoadChunk([]byte(`
				local big = { payload = string.rep("x", 65536) }
				local co = coroutine.create(function(b)
					local small = 41
					coroutine.yield(function()
						small = small + 1
						return small
					end)
					return b
				end)
				local ok, fn = coroutine.resume(co, big)
				assert(ok and type(fn) == "function")
				FN = fn
				return big
			`), "=unreach_coro")
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Call(cl)
			if err != nil {
				t.Fatal(err)
			}
			canary = res[0]
			res[0] = vm.NilValue()
		}()

		s.CollectFull()
		if s.IsLive(canary) {
			t.Fatalf("unreachable suspended coroutine retained unrelated argument via escaped closure upvalue")
		}

		// The escaped closure must remain fully functional after its owning coroutine was collected.
		check, err := s.LoadChunk([]byte(`
			assert(FN() == 42)
			assert(FN() == 43)
		`), "=check_fn")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(check); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("escaped_higher_upval_does_not_retain_sibling_upval", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()

		var canary vm.Value
		func() {
			cl, err := s.LoadChunk([]byte(`
				local big = { payload = string.rep("x", 1024 * 1024) }
				local co = coroutine.create(function(b)
					local holdBig = function() return b end
					local small = 7
					coroutine.yield(function()
						small = small + 1
						return small
					end)
					return holdBig()
				end)
				local ok, fn = coroutine.resume(co, big)
				assert(ok and type(fn) == "function")
				FN = fn
				return big
			`), "=sibling_upval")
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Call(cl)
			if err != nil {
				t.Fatal(err)
			}
			canary = res[0]
			res[0] = vm.NilValue()
		}()

		s.CollectFull()
		if s.IsLive(canary) {
			t.Fatalf("escaped higher-index upvalue retained lower-index sibling upvalue payload")
		}

		check, err := s.LoadChunk([]byte(`assert(FN() == 8 and FN() == 9)`), "=check_sibling")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(check); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("suspended_upval_mutations_debug_and_tbc_close", func(t *testing.T) {
		src := `
local co = coroutine.create(function()
	local x = 10
	coroutine.yield(function(v)
		if v ~= nil then x = v end
		return x
	end)
	return x
end)
local ok, fn = coroutine.resume(co)
assert(ok and fn() == 10)
assert(fn(25) == 25)
local name, val = debug.getlocal(co, 1, 1)
assert(name == "x" and val == 25, "getlocal expected 25, got " .. tostring(val))
assert(debug.setlocal(co, 1, 1, 99) == "x")
assert(fn() == 99, "closure should observe debug.setlocal")
local ok2, finalX = coroutine.resume(co)
assert(ok2 and finalX == 99 and fn() == 99)

-- TBC <close> variable also captured as an upvalue must still run __close on coroutine.close(co).
local closed = 0
local co2 = coroutine.create(function()
	local res <close> = setmetatable({ tag = "tbc" }, {
		__close = function(self) closed = closed + 1; assert(self.tag == "tbc") end
	})
	coroutine.yield(function() return res.tag end)
end)
local ok3, fn2 = coroutine.resume(co2)
assert(ok3 and fn2() == "tbc")
assert(coroutine.close(co2) == true)
assert(closed == 1 and fn2() == "tbc")
print("ok")
`
		got := runMoeLua(t, src)
		if got != "ok\n" {
			t.Fatalf("unexpected output: %q", got)
		}
		if hasCLua55(t) {
			if want := runCLua55(t, src); got != want {
				t.Fatalf("mismatch vs C Lua 5.5.1: want %q got %q", want, got)
			}
		}
	})
}

// TestDebugGetInfoRootsInfoTableAcrossActivelinesMemLimit verifies that debug.getinfo(f, "SLufn")
// keeps the partially constructed info table rooted across emergency GC while building activelines,
// so succeeding calls never exceed the configured memory limit after rooting the returned table.
func TestDebugGetInfoRootsInfoTableAcrossActivelinesMemLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("local function big(x)\n")
	for range 1000 {
		b.WriteString("  x = x + 1\n")
	}
	b.WriteString("  return x\nend\nBIGFN = big\n")

	s := vm.NewState()
	defer s.Close()
	setup, err := s.LoadChunk([]byte(b.String()), "=bigfn")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(setup); err != nil {
		t.Fatal(err)
	}
	caller, err := s.LoadChunk([]byte(`RES = debug.getinfo(BIGFN, "SLufn")`), "=getinfo")
	if err != nil {
		t.Fatal(err)
	}
	callerRef := s.Ref(caller)
	defer s.Unref(callerRef)

	s.SetMemoryLimit(64 << 20)
	s.CollectFull()
	baseMem := s.MemoryUsage()

	// Sweep tight headrooms around the info+activelines allocation size: if debug.getinfo succeeds,
	// its retained RES table MUST fit within limit after a full collection.
	for _, headroom := range []int64{1024, 2048, 3000, 4096, 6144, 8192} {
		s.SetGlobal("RES", vm.NilValue())
		s.SetMemoryLimit(64 << 20)
		s.CollectFull()
		baseMem = s.MemoryUsage()

		limit := baseMem + headroom
		s.SetMemoryLimit(limit)
		_, callErr := s.Call(caller)
		if callErr == nil {
			s.CollectFull()
			after := s.MemoryUsage()
			if after > limit {
				t.Fatalf("headroom=%d: debug.getinfo succeeded but rooted result uses %d bytes > limit %d (delta=%d)",
					headroom, after, limit, after-baseMem)
			}
		}
	}
}

// TestYieldablePairsInTailProtectedAndCoroCalls verifies that yielding __pairs metamethods
// (Lua closures, callable tables, and native coroutine.yield) suspend cleanly across direct calls,
// tail calls, pcall/xpcall, and coroutine.create(pairs), normalizing results to exactly 4 values.
func TestYieldablePairsInTailProtectedAndCoroCalls(t *testing.T) {
	src := `
local t = setmetatable({}, {
	__pairs = function(self)
		local x = coroutine.yield("from_pairs")
		return x, self, 1, 2, 3, 4
	end
})
local ct = setmetatable({}, {
	__pairs = setmetatable({}, {
		__call = function(_, self)
			local x = coroutine.yield("from_callable_pairs")
			return x, self
		end
	})
})
local nt = setmetatable({}, { __pairs = coroutine.yield })

local function run2(co, a, b)
	local r1 = table.pack(coroutine.resume(co, a))
	local r2 = table.pack(coroutine.resume(co, b))
	return r1, r2
end

local r1, r2 = run2(coroutine.create(function() return pairs(t) end), nil, "iter_tail")
assert(r1[1] == true and r1[2] == "from_pairs")
assert(r2.n == 5 and r2[1] == true and r2[2] == "iter_tail" and r2[3] == t and r2[4] == 1 and r2[5] == 2)

local p1, p2 = run2(coroutine.create(function() return pcall(pairs, t) end), nil, "iter_pcall")
assert(p1[1] == true and p1[2] == "from_pairs")
assert(p2.n == 6 and p2[1] == true and p2[2] == true and p2[3] == "iter_pcall" and p2[4] == t and p2[5] == 1 and p2[6] == 2)

local x1, x2 = run2(coroutine.create(function() return xpcall(pairs, tostring, t) end), nil, "iter_xpcall")
assert(x1[1] == true and x1[2] == "from_pairs")
assert(x2.n == 6 and x2[1] == true and x2[2] == true and x2[3] == "iter_xpcall" and x2[4] == t and x2[5] == 1 and x2[6] == 2)

local c1, c2 = run2(coroutine.create(pairs), t, "iter_coro")
assert(c1[1] == true and c1[2] == "from_pairs")
assert(c2.n == 5 and c2[1] == true and c2[2] == "iter_coro" and c2[3] == t and c2[4] == 1 and c2[5] == 2)

local k1, k2 = run2(coroutine.create(function() return pairs(ct) end), nil, "iter_call")
assert(k1[1] == true and k1[2] == "from_callable_pairs")
assert(k2.n == 5 and k2[1] == true and k2[2] == "iter_call" and k2[3] == ct and k2[4] == nil and k2[5] == nil)

local n1, n2 = run2(coroutine.create(function() return pcall(pairs, nt) end), nil, "iter_native")
assert(n1[1] == true and n1[2] == nt)
assert(n2.n == 6 and n2[1] == true and n2[2] == true and n2[3] == "iter_native" and n2[4] == nil and n2[5] == nil and n2[6] == nil)

print("pairs_ok")
`
	got := runMoeLua(t, src)
	if got != "pairs_ok\n" {
		t.Fatalf("unexpected output: %q", got)
	}
	if hasCLua55(t) {
		if want := runCLua55(t, src); got != want {
			t.Fatalf("mismatch vs C Lua 5.5.1:\nwant:\n%s\ngot:\n%s", want, got)
		}
	}
}

// TestPackagePreloadAndFieldsHonorIndexMetamethod verifies that package.preload,
// package.path, package.cpath, and package.searchers honor __index metamethods
// (matching PUC-Rio Lua 5.5.1 loadlib.c lua_getfield).
func TestPackagePreloadAndFieldsHonorIndexMetamethod(t *testing.T) {
	src := `
setmetatable(package.preload, {
	__index = function(_, name)
		if name == "virt_mod" then
			return function(modname, extra)
				return "loaded:" .. modname .. ":" .. tostring(extra)
			end
		end
	end
})
local v, src = require("virt_mod")
assert(v == "loaded:virt_mod::preload:" and src == ":preload:", tostring(v) .. "|" .. tostring(src))

-- package.searchers resolved through __index on package
local origSearchers = package.searchers
package.searchers = nil
setmetatable(package, {
	__index = function(_, k)
		if k == "searchers" then
			return {
				function(modname)
					if modname == "via_meta_searchers" then
						return function(n) return "ms:" .. n end, ":meta:"
					end
				end,
				origSearchers[1],
				origSearchers[2],
				origSearchers[3],
			}
		elseif k == "path" then
			return "./nonexistent_meta_dir/?.lua"
		elseif k == "cpath" then
			return "./nonexistent_meta_dir/?.so"
		end
	end
})
local mv, msrc = require("via_meta_searchers")
assert(mv == "ms:via_meta_searchers" and msrc == ":meta:")

-- package.path & package.cpath resolved through __index on package
local origPath, origCPath = package.path, package.cpath
package.path = nil
package.cpath = nil
local ok, err = pcall(require, "missing_meta_mod_xyz")
assert(not ok and string.find(err, "nonexistent_meta_dir", 1, true), tostring(err))
package.path = origPath
package.cpath = origCPath
package.searchers = origSearchers
setmetatable(package, nil)
print("pkg_meta_ok")
`
	got := runMoeLua(t, src)
	if got != "pkg_meta_ok\n" {
		t.Fatalf("unexpected output: %q", got)
	}
	if hasCLua55(t) {
		if want := runCLua55(t, src); got != want {
			t.Fatalf("mismatch vs C Lua 5.5.1:\nwant:\n%s\ngot:\n%s", want, got)
		}
	}
}

// TestTableLibStringBuiltInLengthAndCheckTab verifies Lua 5.5.1 ltablib.c:checktab
// and built-in string length handling across table.concat/unpack/move/insert/remove/sort.
func TestTableLibStringBuiltInLengthAndCheckTab(t *testing.T) {
	src := `
local oldmt = debug.getmetatable("")
debug.setmetatable("", {
	__index = function(str, i) return string.sub(str, i, i) end,
	__len = function() return 999 end,
})
assert(table.concat("abc", "-") == "a-b-c")
local a, b, c, d = table.unpack("abc")
assert(a == "a" and b == "b" and c == "c" and d == nil)
local dst = table.move("abc", 1, 3, 1, {})
assert(table.concat(dst, ",") == "a,b,c")

local ok1, e1 = pcall(table.insert, "abc", "d")
assert(not ok1 and string.find(e1, "table expected, got string", 1, true), tostring(e1))
local ok2, e2 = pcall(table.remove, "abc")
assert(not ok2 and string.find(e2, "table expected, got string", 1, true), tostring(e2))
local ok3, e3 = pcall(table.sort, "abc")
assert(not ok3 and string.find(e3, "table expected, got string", 1, true), tostring(e3))
local ok4, e4 = pcall(table.move, "abc", 1, 2, 1)
assert(not ok4 and string.find(e4, "table expected, got string", 1, true), tostring(e4))

-- String without __index must be rejected even with explicit range.
debug.setmetatable("", {})
local ok5, e5 = pcall(table.concat, "abc", "", 1, 0)
assert(not ok5 and string.find(e5, "table expected, got string", 1, true), tostring(e5))
local ok6, e6 = pcall(table.unpack, "abc", 1, 0)
assert(not ok6 and string.find(e6, "table expected, got string", 1, true), tostring(e6))

-- Non-string with __index only (no __len) is accepted as table.move src, rejected by concat/unpack.
debug.setmetatable(1, { __index = function(_, i) return "n" .. i end })
local m = table.move(1, 1, 2, 1, {})
assert(m[1] == "n1" and m[2] == "n2")
local ok7, e7 = pcall(table.concat, 1, ",", 1, 2)
assert(not ok7 and string.find(e7, "table expected, got number", 1, true), tostring(e7))
local ok8, e8 = pcall(table.unpack, 1, 1, 2)
assert(not ok8 and string.find(e8, "table expected, got number", 1, true), tostring(e8))
debug.setmetatable(1, nil)
debug.setmetatable("", oldmt)
print("table_checktab_ok")
`
	got := runMoeLua(t, src)
	if got != "table_checktab_ok\n" {
		t.Fatalf("unexpected output: %q", got)
	}
	if hasCLua55(t) {
		if want := runCLua55(t, src); got != want {
			t.Fatalf("mismatch vs C Lua 5.5.1:\nwant:\n%s\ngot:\n%s", want, got)
		}
	}
}

// TestGoCallXPCallRunsHandlerBeforeUnwindAndClose verifies that invoking Lua's xpcall
// (and pcall) via Go State.Call uses a real protected-call boundary so the message handler
// runs before frame unwinding and <close> finalizers, sees the live failing traceback frames,
// and an inner Go State.Call(pcall, ...) does not leak to an enclosing xpcall handler.
func TestGoCallXPCallRunsHandlerBeforeUnwindAndClose(t *testing.T) {
	s := vm.NewState()
	defer s.Close()

	setup, err := s.LoadChunk([]byte(`
CLOSED = false
HANDLER_SAW_CLOSED = nil
HANDLER_TRACE = nil

function INNER_HELPER()
	error("boom_go_xpcall")
end

function FAILING()
	local guard <close> = setmetatable({}, {
		__close = function() CLOSED = true end
	})
	INNER_HELPER()
end

function MSGH(err)
	HANDLER_SAW_CLOSED = CLOSED
	HANDLER_TRACE = debug.traceback(tostring(err), 1)
	return HANDLER_SAW_CLOSED
end
`), "=go_xpcall_test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(setup); err != nil {
		t.Fatal(err)
	}

	xp := s.GetGlobal("xpcall")
	fn := s.GetGlobal("FAILING")
	msgh := s.GetGlobal("MSGH")

	res, err := s.Call(xp, fn, msgh)
	if err != nil {
		t.Fatalf("State.Call(xpcall) returned unexpected error: %v", err)
	}
	if len(res) != 2 || !res[0].IsFalse() {
		t.Fatalf("expected [false, false], got %v", res)
	}
	if !res[1].IsFalse() {
		t.Fatalf("message handler ran after <close>: got HANDLER_SAW_CLOSED=%v, want false", res[1])
	}
	if !s.GetGlobal("CLOSED").IsTrue() {
		t.Fatalf("<close> handler did not run after message handler")
	}
	trace := s.FormatValue(s.GetGlobal("HANDLER_TRACE"))
	if !strings.Contains(trace, "INNER_HELPER") || !strings.Contains(trace, "go_xpcall_test:") || !strings.Contains(trace, "in function 'xpcall'") {
		t.Fatalf("traceback from State.Call(xpcall) lost live failing frames: %s", trace)
	}

	// Verify hook-enabled State.Call(xpcall) does not duplicate call/return hooks or break ordering.
	s.SetGlobal("CLOSED", vm.FalseValue())
	s.SetGlobal("HANDLER_SAW_CLOSED", vm.NilValue())
	hookSetup, err := s.LoadChunk([]byte(`
HOOK_EVENTS = {}
debug.sethook(function(ev)
	local info = debug.getinfo(2, "n")
	HOOK_EVENTS[#HOOK_EVENTS + 1] = ev .. ":" .. tostring(info and info.name)
end, "cr")
`), "=hook_setup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(hookSetup); err != nil {
		t.Fatal(err)
	}
	res2, err := s.Call(xp, fn, msgh)
	if err != nil {
		t.Fatalf("hooked State.Call(xpcall) error: %v", err)
	}
	if len(res2) != 2 || !res2[0].IsFalse() || !res2[1].IsFalse() {
		t.Fatalf("hooked State.Call(xpcall) expected [false, false], got %v", res2)
	}
	clearHook, err := s.LoadChunk([]byte(`
debug.sethook()
local callXP, retXP = 0, 0
for _, ev in ipairs(HOOK_EVENTS) do
	if ev == "call:xpcall" then callXP = callXP + 1 end
	if ev == "return:xpcall" then retXP = retXP + 1 end
end
assert(callXP == 1 and retXP == 1, "expected 1 call/return hook for xpcall, got " .. callXP .. "/" .. retXP)
`), "=clear_hook")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(clearHook); err != nil {
		t.Fatal(err)
	}
	// Verify nativePCall inside an outer xpcall does NOT trigger the outer xpcall's handler.
	outerCalled := false
	s.SetGlobal("GO_CALL_PCALL", s.NativeValue(&vm.NativeFunc{
		Name: "go_call_pcall",
		Fn: func(st *vm.State, _ []vm.Value) ([]vm.Value, error) {
			return st.Call(st.GetGlobal("pcall"), st.GetGlobal("FAILING"))
		},
	}))
	s.SetGlobal("OUTER_MSGH", s.NativeValue(&vm.NativeFunc{
		Name: "outer_msgh",
		Fn: func(_ *vm.State, args []vm.Value) ([]vm.Value, error) {
			outerCalled = true
			return args, nil
		},
	}))
	checkInner, err := s.LoadChunk([]byte(`
local ok, innerOk, innerErr = xpcall(GO_CALL_PCALL, OUTER_MSGH)
assert(ok == true and innerOk == false and string.find(innerErr, "boom_go_xpcall", 1, true))
`), "=check_inner_pcall")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(checkInner); err != nil {
		t.Fatal(err)
	}
	if outerCalled {
		t.Fatalf("outer xpcall handler was erroneously invoked for error caught by Go State.Call(pcall)")
	}
}

// TestRegistryLoadedAndRawSearchersParity verifies that require resolves _LOADED
// dynamically from debug.getregistry()._LOADED (honoring __index/__newindex) while
// package.searchers iteration uses raw integer indexing (lua_rawgeti, per loadlib.c:629).
func TestRegistryLoadedAndRawSearchersParity(t *testing.T) {
	src := `
local origLoaded = debug.getregistry()._LOADED
debug.getregistry()._LOADED = { virtual = 42 }
assert(require("virtual") == 42)

local stored = {}
debug.getregistry()._LOADED = setmetatable({}, {
	__index = function(_, k)
		if k == "lazy_loaded" then return 99 end
		return stored[k]
	end,
	__newindex = function(_, k, v)
		stored[k] = v
	end,
})
assert(require("lazy_loaded") == 99)

local origPreload = package.preload.custom_reg_mod
package.preload.custom_reg_mod = function(n) return "val:" .. n end
local r, extra = require("custom_reg_mod")
assert(r == "val:custom_reg_mod" and extra == ":preload:" and stored.custom_reg_mod == "val:custom_reg_mod")
package.preload.custom_reg_mod = origPreload
debug.getregistry()._LOADED = origLoaded

-- Per Lua 5.5.1 loadlib.c:629 (lua_rawgeti), package.searchers ignores __index.
local origSearchers = package.searchers
package.searchers = setmetatable({}, {
	__index = function(_, i)
		if i == 1 then
			return function() return function() return "should_not_run" end end
		end
	end
})
local ok, err = pcall(require, "not_found_raw_searchers")
package.searchers = origSearchers
assert(not ok and string.find(err, "module 'not_found_raw_searchers' not found:", 1, true), tostring(err))
print("reg_loaded_ok")
`
	got := runMoeLua(t, src)
	if got != "reg_loaded_ok\n" {
		t.Fatalf("unexpected output: %q", got)
	}
	if hasCLua55(t) {
		if want := runCLua55(t, src); got != want {
			t.Fatalf("mismatch vs C Lua 5.5.1:\nwant:\n%s\ngot:\n%s", want, got)
		}
	}
}

// TestInlineCacheDoesNotPinObjectsInTier0 verifies that stale or orphaned inline-cache
// entries (icProtoVal, polymorphic ICs, icAddSlot, icEnvSlot) hold no strong Go pointers
// to collectable Lua objects or Shapes, allowing Go's GC to reclaim them in Tier 0
// without calling State.CollectFull().
func TestInlineCacheDoesNotPinObjectsInTier0(t *testing.T) {
	runReclaim := func(t *testing.T, src string) {
		t.Helper()
		s := vm.NewState()
		defer s.Close()
		var canary vm.Value
		func() {
			cl, err := s.LoadChunk([]byte(src), "=ic_reclaim")
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Call(cl)
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 1 || !res[0].IsTable() {
				t.Fatalf("expected table canary, got %v", res)
			}
			canary = res[0]
			res[0] = vm.NilValue()
		}()
		s.CollectFull()
		if s.IsLive(canary) {
			t.Fatalf("inline cache retained unreachable table across State.CollectFull")
		}
	}

	t.Run("deleted_proto_field_icProtoVal", func(t *testing.T) {
		runReclaim(t, `
			GETTER = function(t) return t.x end
			local payload = { data = string.rep("a", 1024 * 1024) }
			local proto = { x = payload }
			local mt = { __index = proto }
			local obj = setmetatable({}, mt)
			assert(GETTER(obj) == payload)
			assert(GETTER(obj) == payload)
			proto.x = nil
			return payload
		`)
	})

	t.Run("dropped_unmutated_proto_and_shape", func(t *testing.T) {
		runReclaim(t, `
			GETTER = function(t) return t.x end
			local payload = { data = string.rep("b", 1024 * 1024) }
			local proto = { x = payload }
			local obj = setmetatable({}, { __index = proto })
			assert(GETTER(obj) == payload)
			assert(GETTER(obj) == payload)
			return payload
		`)
	})

	t.Run("polymorphic_proto_and_env_slot", func(t *testing.T) {
		runReclaim(t, `
			POLY = function(t) return t.x end
			local p1 = { data = string.rep("c", 65536) }
			local p2 = { data = string.rep("d", 65536) }
			local o1 = setmetatable({ a = 1 }, { __index = { x = p1 } })
			local o2 = setmetatable({ b = 2 }, { __index = { x = p2 } })
			assert(POLY(o1) == p1 and POLY(o2) == p2 and POLY(o1) == p1)
			return p2
		`)
	})

	t.Run("swapped_env_dict_icEnvSlot", func(t *testing.T) {
		runReclaim(t, `
			local env1 = { GVAL = 123, BIG = { data = string.rep("e", 65536) } }
			for i = 1, 80 do env1["k" .. i] = i end -- force dict mode (shape == nil)
			FN = load("return GVAL", "=env_ic", "t", env1)
			assert(FN() == 123 and FN() == 123)
			debug.setupvalue(FN, 1, { GVAL = 456 })
			assert(FN() == 456)
			return env1.BIG
		`)
	})
}

// TestMathRandomFullRangeAppliesLowerBound verifies that math.random(math.mininteger, math.maxinteger)
// applies the +low wraparound offset matching Lua 5.5.1 lmathlib.c:math_random.
func TestMathRandomFullRangeAppliesLowerBound(t *testing.T) {
	src := `
math.randomseed(123, 456)
local a1, a2, a3 = math.random(0), math.random(0), math.random(0)
math.randomseed(123, 456)
local b1 = math.random(math.mininteger, math.maxinteger)
local b2 = math.random(math.mininteger, math.maxinteger)
local b3 = math.random(math.mininteger, math.maxinteger)
assert(b1 == a1 + math.mininteger, string.format("%d ~= %d", b1, a1 + math.mininteger))
assert(b2 == a2 + math.mininteger, string.format("%d ~= %d", b2, a2 + math.mininteger))
assert(b3 == a3 + math.mininteger, string.format("%d ~= %d", b3, a3 + math.mininteger))
print(b1, b2, b3)
`
	got := runMoeLua(t, src)
	if hasCLua55(t) {
		if want := runCLua55(t, src); got != want {
			t.Fatalf("mismatch vs C Lua 5.5.1:\nwant: %q\ngot:  %q", want, got)
		}
	}
}

// TestLargeStringCopyCancelAndBudget verifies that copying large strings in table.concat,
// OP_CONCAT, print, warn, and package.searchpath cooperatively polls execution controls
// (both instruction limits and expired contexts) even on single-element/few-element inputs.
func TestLargeStringCopyCancelAndBudget(t *testing.T) {
	bigStr := strings.Repeat("z", 8*1024*1024)

	t.Run("direct_state_call_table_concat", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		tblConcat := s.AsTable(s.GetGlobal("table")).RawGetStr(s, s.Intern("concat"))
		arr := s.NewTable(1, 0)
		arr.RawSetInt(s, 1, vm.StringValue(s.Intern(bigStr)))

		s.SetInstructionLimit(100)
		if _, err := s.Call(tblConcat, vm.TableValue(arr)); err == nil {
			t.Fatalf("direct State.Call(table.concat, {bigStr}) bypassed instruction limit 100")
		}
		if s.InstructionsExecuted() == 0 {
			t.Fatalf("direct State.Call(table.concat) charged 0 instructions")
		}

		s2 := vm.NewState()
		defer s2.Close()
		tblConcat2 := s2.AsTable(s2.GetGlobal("table")).RawGetStr(s2, s2.Intern("concat"))
		arr2 := s2.NewTable(1, 0)
		arr2.RawSetInt(s2, 1, vm.StringValue(s2.Intern(bigStr)))
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		s2.SetContext(ctx)
		start := time.Now()
		if _, err := s2.Call(tblConcat2, vm.TableValue(arr2)); err == nil {
			t.Fatalf("direct State.Call(table.concat) bypassed expired context deadline")
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("direct State.Call(table.concat) took %v despite expired context", elapsed)
		}
	})

	t.Run("lua_op_concat_print_warn_searchpath", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			src  string
		}{
			{"table_concat_sep", `return table.concat({"a", "b"}, BIG)`},
			{"op_concat", `return BIG .. "x"`},
			{"print", `print(BIG)`},
			{"warn", `warn("@on"); warn(BIG)`},
			{"searchpath", `return package.searchpath(BIG, "?.lua")`},
		} {
			s := vm.NewState()
			defer s.Close()
			var discard bytes.Buffer
			s.SetOutput(&discard)
			s.SetErrOutput(&discard)
			s.SetGlobal("BIG", vm.StringValue(s.Intern(bigStr)))
			cl, err := s.LoadChunk([]byte(tc.src), "="+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			s.SetInstructionLimit(100)
			if _, err := s.Call(cl); err == nil {
				t.Fatalf("%s bypassed instruction limit 100", tc.name)
			}
			if s.InstructionsExecuted() == 0 {
				t.Fatalf("%s charged 0 instructions", tc.name)
			}
		}
	})
}

// TestMathModfNormalizesIntegralZero verifies that math.modf returns +0.0 as the
// fractional part for all integral inputs (including -1.0, -0.0, and large negative floats),
// matching Lua 5.5.1 lmathlib.c:math_modf.
func TestMathModfNormalizesIntegralZero(t *testing.T) {
	src := `
for _, x in ipairs({-1.0, -0.0, 0.0, 1.0, -1.5, 1.5, -1e20, -math.huge, math.huge}) do
	local i, f = math.modf(x)
	print(tostring(i), tostring(f), tostring(1 / f))
end
`
	got := runMoeLua(t, src)
	if hasCLua55(t) {
		if want := runCLua55(t, src); got != want {
			t.Fatalf("mismatch vs C Lua 5.5.1:\nwant:\n%s\ngot:\n%s", want, got)
		}
	}
}

// TestCallProtectedAbortClosesCapturedUpvalues verifies that aborting State.CallProtected
// (or Go State.Call(xpcall/pcall, ...)) via instruction limit or context cancellation closes
// open upvalues and discards pending TBC entries before clearing the stack, so escaped
// closures retain their captured values when the State is reused.
func TestCallProtectedAbortClosesCapturedUpvalues(t *testing.T) {
	for _, mode := range []string{"CallProtected", "StateCallXPCall"} {
		t.Run(mode, func(t *testing.T) {
			s := vm.NewState()
			defer s.Close()

			cl, err := s.LoadChunk([]byte(`
				local x = 42
				local guard <close> = setmetatable({}, { __close = function() CLOSED_ON_ABORT = true end })
				SAVED = function() return x end
				while true do end
			`), "=abort_upval")
			if err != nil {
				t.Fatal(err)
			}
			msgh := s.GetGlobal("tostring")
			s.SetInstructionLimit(100)
			if mode == "CallProtected" {
				_, _, ok, abortErr := s.CallProtected(cl, msgh)
				if ok || abortErr == nil {
					t.Fatalf("expected instruction-limit abort, got ok=%v abortErr=%v", ok, abortErr)
				}
			} else {
				_, callErr := s.Call(s.GetGlobal("xpcall"), cl, msgh)
				if callErr == nil {
					t.Fatalf("expected instruction-limit abort from State.Call(xpcall)")
				}
			}

			s.SetInstructionLimit(0)
			res, err := s.Call(s.GetGlobal("SAVED"))
			if err != nil {
				t.Fatalf("calling escaped closure after abort failed: %v", err)
			}
			if len(res) != 1 || !res[0].IsInt() || res[0].Int() != 42 {
				t.Fatalf("escaped closure lost captured upvalue after %s abort: got %v, want 42", mode, res)
			}
			if !s.GetGlobal("CLOSED_ON_ABORT").IsNil() {
				t.Fatalf("pending <close> handler should be discarded on uncatchable abort")
			}
		})
	}
}

// TestStringPackAndUnpackCancelAndBudget verifies that string.pack (s/c/z fields and padding)
// and string.unpack ('z' NUL scanning) cooperatively poll execution controls (both instruction
// budgets and expired contexts), including direct State.Call with zero Lua opcodes.
func TestStringPackAndUnpackCancelAndBudget(t *testing.T) {
	bigStr := strings.Repeat("p", 8*1024*1024)

	t.Run("direct_state_call_pack_s8", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		strPack := s.AsTable(s.GetGlobal("string")).RawGetStr(s, s.Intern("pack"))
		fmtVal := vm.StringValue(s.Intern("s8"))
		bigVal := vm.StringValue(s.Intern(bigStr))

		s.SetInstructionLimit(100)
		if _, err := s.Call(strPack, fmtVal, bigVal); err == nil {
			t.Fatalf("direct State.Call(string.pack, 's8', bigStr) bypassed instruction limit 100")
		}
		if s.InstructionsExecuted() == 0 {
			t.Fatalf("direct State.Call(string.pack) charged 0 instructions")
		}

		s2 := vm.NewState()
		defer s2.Close()
		strPack2 := s2.AsTable(s2.GetGlobal("string")).RawGetStr(s2, s2.Intern("pack"))
		fmtVal2 := vm.StringValue(s2.Intern("s8"))
		bigVal2 := vm.StringValue(s2.Intern(bigStr))
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		s2.SetContext(ctx)
		start := time.Now()
		if _, err := s2.Call(strPack2, fmtVal2, bigVal2); err == nil {
			t.Fatalf("direct State.Call(string.pack) bypassed expired context deadline")
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("direct State.Call(string.pack) took %v despite expired context", elapsed)
		}
	})

	t.Run("pack_c_padding_z_and_unpack_z", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			src  string
		}{
			{"pack_s8", `return string.pack("s8", BIG)`},
			{"pack_c_pad", `return string.pack("c8388608", "a")`},
			{"pack_z", `return string.pack("z", BIG)`},
			{"unpack_z", `return string.unpack("z", BIG)`},
		} {
			s := vm.NewState()
			defer s.Close()
			s.SetGlobal("BIG", vm.StringValue(s.Intern(bigStr)))
			cl, err := s.LoadChunk([]byte(tc.src), "="+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			s.SetInstructionLimit(100)
			if _, err := s.Call(cl); err == nil {
				t.Fatalf("%s bypassed instruction limit 100", tc.name)
			}
			if s.InstructionsExecuted() == 0 {
				t.Fatalf("%s charged 0 instructions", tc.name)
			}
		}
	})
}

// TestStdlibSlabAndInitStackGrowthDoNotPinObjects verifies that reallocating a stdlib
// table's slots/hash/array off the shared stdlib slab (or growing mainThread's stack off
// stateAlloc.initStack) clears abandoned entries so discarded objects are reclaimed by Go GC.
func TestStdlibSlabAndInitStackGrowthDoNotPinObjects(t *testing.T) {
	runReclaim := func(t *testing.T, src string) {
		t.Helper()
		s := vm.NewState()
		defer s.Close()
		var canary vm.Value
		func() {
			cl, err := s.LoadChunk([]byte(src), "=slab_reclaim")
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Call(cl)
			if err != nil {
				t.Fatal(err)
			}
			canary = res[0]
			res[0] = vm.NilValue()
		}()
		s.CollectFull()
		if s.IsLive(canary) {
			t.Fatalf("abandoned shared slab or initStack retained unreachable table")
		}
	}

	t.Run("stdlib_math_slots_and_hash_growth", func(t *testing.T) {
		runReclaim(t, `
			local payload = { data = string.rep("m", 1024 * 1024) }
			math.pi = payload
			for i = 1, 80 do
				math["extra_" .. i] = i
			end
			math.pi = nil
			return payload
		`)
	})

	t.Run("stdlib_searchers_array_growth", func(t *testing.T) {
		runReclaim(t, `
			local payload = { data = string.rep("s", 1024 * 1024) }
			package.searchers[1] = payload
			for i = 4, 20 do
				package.searchers[i] = i
			end
			package.searchers[1] = nil
			return payload
		`)
	})

	t.Run("main_thread_init_stack_growth", func(t *testing.T) {
		runReclaim(t, `
			local payload = { data = string.rep("k", 1024 * 1024) }
			local function grow(n, x)
				if n == 0 then return x end
				return grow(n - 1, x)
			end
			local keep = grow(120, payload)
			return keep
		`)
	})
}

// TestIOInputOutputRejectClosedHandles verifies that io.input(closedFile) and
// io.output(closedFile) raise "attempt to use a closed file" and preserve the
// previously installed default streams (matching Lua 5.5.1 liolib.c:g_iofile).
func TestIOInputOutputRejectClosedHandles(t *testing.T) {
	src := `
local prevIn = io.input()
local prevOut = io.output()
local f = assert(io.tmpfile())
assert(f:close())

local okIn, errIn = pcall(io.input, f)
assert(not okIn and string.find(errIn, "closed file", 1, true), tostring(errIn))
assert(io.input() == prevIn, "io.input replaced default stream on closed handle")

local okOut, errOut = pcall(io.output, f)
assert(not okOut and string.find(errOut, "closed file", 1, true), tostring(errOut))
assert(io.output() == prevOut, "io.output replaced default stream on closed handle")
print("io_closed_ok")
`
	got := runMoeLua(t, src)
	if got != "io_closed_ok\n" {
		t.Fatalf("unexpected output: %q", got)
	}
	if hasCLua55(t) {
		if want := runCLua55(t, src); got != want {
			t.Fatalf("mismatch vs C Lua 5.5.1:\nwant:\n%s\ngot:\n%s", want, got)
		}
	}
}

// TestNativeArgsSurviveStackGrowth verifies that growing th.stack off initStack during a
// native call (such as 350-element table.sort or a stack-growing comparator) never clobbers
// active native argument slices, while abandoned initStack storage is still cleared afterwards.
func TestNativeArgsSurviveStackGrowth(t *testing.T) {
	src := `
local t = {}
for i = 1, 350 do
	t[i] = string.format("%04d", 351 - i)
end
table.sort(t)
assert(#t == 350 and t[1] == "0001" and t[350] == "0350")

local function grow(n)
	if n == 0 then return 0 end
	return grow(n - 1) + 1
end
local t2 = {}
for i = 1, 40 do t2[i] = 41 - i end
local grew = false
table.sort(t2, function(a, b)
	if not grew then
		grew = true
		grow(150)
	end
	return a < b
end)
assert(t2[1] == 1 and t2[40] == 40)
print("sort_stack_growth_ok")
`
	got := runMoeLua(t, src)
	if got != "sort_stack_growth_ok\n" {
		t.Fatalf("unexpected output: %q", got)
	}
	if hasCLua55(t) {
		if want := runCLua55(t, src); got != want {
			t.Fatalf("mismatch vs C Lua 5.5.1:\nwant:\n%s\ngot:\n%s", want, got)
		}
	}
}

// TestDebugAndOptionStringScansCancelAndBudget verifies that unbounded string scans in
// debug.sethook, debug.getinfo, debug.traceback, load, string.packsize, and string.format
// cooperatively poll execution controls (instruction budget and context deadline).
func TestDebugAndOptionStringScansCancelAndBudget(t *testing.T) {
	bigMask := strings.Repeat("x", 8*1024*1024)
	bigWhat := strings.Repeat("S", 8*1024*1024)

	t.Run("direct_state_call_debug_sethook", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		setHook := s.AsTable(s.GetGlobal("debug")).RawGetStr(s, s.Intern("sethook"))
		noop := s.GetGlobal("tostring")
		maskVal := vm.StringValue(s.Intern(bigMask))

		s.SetInstructionLimit(100)
		if _, err := s.Call(setHook, noop, maskVal); err == nil {
			t.Fatalf("direct State.Call(debug.sethook, fn, bigMask) bypassed instruction limit 100")
		}
		if s.InstructionsExecuted() == 0 {
			t.Fatalf("direct State.Call(debug.sethook) charged 0 instructions")
		}

		s2 := vm.NewState()
		defer s2.Close()
		setHook2 := s2.AsTable(s2.GetGlobal("debug")).RawGetStr(s2, s2.Intern("sethook"))
		noop2 := s2.GetGlobal("tostring")
		maskVal2 := vm.StringValue(s2.Intern(bigMask))
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		s2.SetContext(ctx)
		start := time.Now()
		if _, err := s2.Call(setHook2, noop2, maskVal2); err == nil {
			t.Fatalf("direct State.Call(debug.sethook) bypassed expired context deadline")
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("direct State.Call(debug.sethook) took %v despite expired context", elapsed)
		}
	})

	t.Run("debug_getinfo_traceback_load_packsize_format", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			src  string
		}{
			{"debug_sethook", `debug.sethook(tostring, BIG_MASK)`},
			{"debug_getinfo", `return debug.getinfo(tostring, BIG_WHAT)`},
			{"debug_traceback", `return debug.traceback(BIG_MASK)`},
			{"load_mode", `return load("return 1", "=x", BIG_MASK)`},
			{"string_packsize", `return string.packsize(BIG_SPACES)`},
			{"string_format", `return string.format(BIG_MASK)`},
		} {
			s := vm.NewState()
			defer s.Close()
			s.SetGlobal("BIG_MASK", vm.StringValue(s.Intern(bigMask)))
			s.SetGlobal("BIG_WHAT", vm.StringValue(s.Intern(bigWhat)))
			s.SetGlobal("BIG_SPACES", vm.StringValue(s.Intern(strings.Repeat(" ", 8*1024*1024))))
			cl, err := s.LoadChunk([]byte(tc.src), "="+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			s.SetInstructionLimit(100)
			if _, err := s.Call(cl); err == nil {
				t.Fatalf("%s bypassed instruction limit 100", tc.name)
			}
			if s.InstructionsExecuted() == 0 {
				t.Fatalf("%s charged 0 instructions", tc.name)
			}
		}
	})
}

func TestReplacedLoadedAndPreloadCachesAreCollected(t *testing.T) {
	script := `
local finalized = {}
local w = setmetatable({}, { __mode = "v" })

local mod = setmetatable({ payload = string.rep("x", 1024 * 1024) }, {
	__gc = function() finalized[#finalized + 1] = "mod" end,
})
local pre = setmetatable({ payload = string.rep("y", 1024 * 1024) }, {
	__gc = function() finalized[#finalized + 1] = "pre" end,
})

package.loaded.big_mod = mod
package.preload.big_pre = pre
w.oldLoaded = package.loaded
w.oldPreload = package.preload
w.mod = mod
w.pre = pre
mod = nil
pre = nil

local reg = debug.getregistry()
local newLoaded = {}
local newPreload = {}
package.loaded = newLoaded
reg._LOADED = newLoaded
package.preload = newPreload
reg._PRELOAD = newPreload

collectgarbage("collect")
collectgarbage("collect")

return w.oldLoaded == nil, w.oldPreload == nil, w.mod == nil, w.pre == nil, #finalized
`
	s := vm.NewState()
	defer s.Close()

	// Also retain a Go weak reference to a host payload stored in original package.loaded
	// via userdata (GoRefs) plus an Arena liveness canary to confirm both Arena GC and Go GC reclaim it.
	goMod := s.NewTable(0, 2)
	goMod.RawSetStr(s, s.Intern("buf"), vm.StringValue(s.Intern(strings.Repeat("z", 1024*1024))))
	hostBuf := make([]byte, 64*1024)
	wpMod := weak.Make(&hostBuf[0])
	udVal, err := s.TryNewUserdata(hostBuf, 0)
	if err != nil {
		t.Fatal(err)
	}
	goMod.RawSetStr(s, s.Intern("ud"), udVal)
	hostBuf = nil
	goModCanary := vm.TableValue(goMod)
	s.AsTable(s.Registry().RawGetStr(s, s.Intern("_LOADED"))).RawSetStr(s, s.Intern("go_mod"), goModCanary)
	goMod = nil

	cl, err := s.LoadChunk([]byte(script), "=replaced_pkg_caches")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Call(cl)
	if err != nil {
		t.Fatal(err)
	}
	nFin, okFin := res[4].ToInt()
	if len(res) != 5 || !res[0].IsTrue() || !res[1].IsTrue() || !res[2].IsTrue() || !res[3].IsTrue() || !okFin || nFin != 2 {
		t.Fatalf("expected replaced _LOADED/_PRELOAD caches and payloads to be collected & finalized, got %v", res)
	}

	if s.IsLive(goModCanary) {
		t.Fatalf("replaced original package.loaded still pinned Arena module table after collectgarbage")
	}
	reclaimed := false
	for range 20 {
		runtime.GC()
		if wpMod.Value() == nil {
			reclaimed = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !reclaimed {
		t.Fatalf("replaced original package.loaded still pinned Go-heap userdata payload after collectgarbage")
	}

	// State.PreloadTable must follow the live registry._PRELOAD replacement and recreate if cleared.
	reg := s.Registry()
	livePreload := s.AsTable(reg.RawGetStr(s, s.Intern("_PRELOAD")))
	if s.PreloadTable() != livePreload {
		t.Fatalf("PreloadTable() did not return live registry._PRELOAD replacement")
	}
	reg.RawSetStr(s, s.Intern("_PRELOAD"), vm.NilValue())
	recreated := s.PreloadTable()
	if recreated == nil || s.AsTable(reg.RawGetStr(s, s.Intern("_PRELOAD"))) != recreated {
		t.Fatalf("PreloadTable() did not recreate cleared registry._PRELOAD")
	}
}

func TestNativeReturningArgsAfterStackGrowthPreservesResults(t *testing.T) {
	makeState := func(t *testing.T) (*vm.State, vm.Value) {
		t.Helper()
		s := vm.NewState()
		cl, err := s.LoadChunk([]byte(`
			local function grow(n)
				if n == 0 then return 0 end
				return grow(n - 1) + 1
			end
			return grow
		`), "=grow")
		if err != nil {
			t.Fatal(err)
		}
		growRes, err := s.Call(cl)
		if err != nil {
			t.Fatal(err)
		}
		growFn := growRes[0]
		echoGrow := s.NativeValue(&vm.NativeFunc{
			Name: "echo_grow",
			Fn: func(st *vm.State, args []vm.Value) ([]vm.Value, error) {
				if _, err := st.Call(growFn, vm.IntValue(400)); err != nil {
					return nil, err
				}
				return args, nil
			},
		})
		s.SetGlobal("echo_grow", echoGrow)
		return s, echoGrow
	}

	t.Run("direct_state_call", func(t *testing.T) {
		s, echoGrow := makeState(t)
		defer s.Close()
		res, err := s.Call(echoGrow, vm.IntValue(42), vm.StringValue(s.Intern("ok")))
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 {
			t.Fatalf("expected 2 results, got %d", len(res))
		}
		got, ok := res[0].ToInt()
		if !ok || got != 42 || !res[1].IsString() || s.AsString(res[1]).Str() != "ok" {
			t.Fatalf("aliased native results corrupted: got (%v, %v)", res[0], res[1])
		}
	})

	t.Run("direct_state_call_with_hooks", func(t *testing.T) {
		s, echoGrow := makeState(t)
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`debug.sethook(function() end, "cr")`), "=hook")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(cl); err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(echoGrow, vm.IntValue(42), vm.StringValue(s.Intern("ok")))
		if err != nil {
			t.Fatal(err)
		}
		got, ok := res[0].ToInt()
		if len(res) != 2 || !ok || got != 42 || !res[1].IsString() || s.AsString(res[1]).Str() != "ok" {
			t.Fatalf("hooked aliased native results corrupted: got %v", res)
		}
	})

	t.Run("lua_call_and_nested_native", func(t *testing.T) {
		s, echoGrow := makeState(t)
		defer s.Close()
		outer := s.NativeValue(&vm.NativeFunc{
			Name: "outer_echo",
			Fn: func(st *vm.State, args []vm.Value) ([]vm.Value, error) {
				innerRes, err := st.Call(echoGrow, args...)
				if err != nil {
					return nil, err
				}
				if got, ok := innerRes[0].ToInt(); !ok || got != 42 {
					t.Fatalf("inner result corrupted: %v", innerRes[0])
				}
				return args, nil
			},
		})
		s.SetGlobal("outer_echo", outer)
		cl, err := s.LoadChunk([]byte(`
			local a, b = echo_grow(42, "lua")
			assert(a == 42 and b == "lua", "callSlow corrupted: " .. tostring(a) .. "," .. tostring(b))
			local c, d = outer_echo(42, "nested")
			assert(c == 42 and d == "nested", "nested native corrupted: " .. tostring(c) .. "," .. tostring(d))
			return a, b, c, d
		`), "=lua_call_echo")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(cl)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 4 {
			t.Fatalf("expected 4 results, got %v", res)
		}
	})
}

func TestAbortPreservesCapturedToBeClosedUpvalues(t *testing.T) {
	src := `
		local x <close> = setmetatable({ value = 42 }, { __close = function() end })
		saved = function() return x.value end
		while true do end
	`
	t.Run("State.Call", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(src), "=tbc_abort_call")
		if err != nil {
			t.Fatal(err)
		}
		s.SetInstructionLimit(200)
		if _, err := s.Call(cl); err == nil {
			t.Fatalf("expected instruction limit abort")
		}
		s.SetInstructionLimit(0)
		saved := s.GetGlobal("saved")
		res, err := s.Call(saved)
		if err != nil {
			t.Fatalf("calling saved closure after abort failed: %v", err)
		}
		got, ok := res[0].ToInt()
		if !ok || got != 42 {
			t.Fatalf("expected captured <close> upvalue 42, got %v", res[0])
		}
	})

	t.Run("State.CallProtected", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(src), "=tbc_abort_protected")
		if err != nil {
			t.Fatal(err)
		}
		s.SetInstructionLimit(200)
		if _, _, _, abortErr := s.CallProtected(cl, vm.NilValue()); abortErr == nil {
			t.Fatalf("expected uncatchable abort from CallProtected")
		}
		s.SetInstructionLimit(0)
		saved := s.GetGlobal("saved")
		res, err := s.Call(saved)
		if err != nil {
			t.Fatalf("calling saved closure after CallProtected abort failed: %v", err)
		}
		got, ok := res[0].ToInt()
		if !ok || got != 42 {
			t.Fatalf("expected captured <close> upvalue 42, got %v", res[0])
		}
	})

	t.Run("coroutine_abort", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local co = coroutine.create(function()
				local x <close> = setmetatable({ value = 99 }, { __close = function() end })
				saved = function() return x.value end
				while true do end
			end)
			return coroutine.resume(co)
		`), "=tbc_abort_coro")
		if err != nil {
			t.Fatal(err)
		}
		s.SetInstructionLimit(300)
		if _, err := s.Call(cl); err == nil {
			t.Fatalf("expected instruction limit abort in coroutine")
		}
		s.SetInstructionLimit(0)
		saved := s.GetGlobal("saved")
		res, err := s.Call(saved)
		if err != nil {
			t.Fatalf("calling saved closure after coroutine abort failed: %v", err)
		}
		got, ok := res[0].ToInt()
		if !ok || got != 99 {
			t.Fatalf("expected captured <close> upvalue 99, got %v", res[0])
		}
	})
}

func TestDoFileDiscardsExtraArguments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.lua")
	if err := os.WriteFile(path, []byte("return marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
marker = 7
local p = ...
assert(dofile(p, "b") == 7, "direct dofile failed on extra mode arg")
assert(dofile(p, nil, { marker = 99 }) == 7, "direct dofile used extra env arg")
local function tail() return dofile(p, "b", { marker = 99 }) end
assert(tail() == 7, "tail-call dofile failed on extra args")
local ok, v = pcall(dofile, p, "b", { marker = 99 })
assert(ok and v == 7, "pcall(dofile) failed on extra args")
local co = coroutine.create(dofile)
local ok2, v2 = coroutine.resume(co, p, "b", { marker = 99 })
assert(ok2 and v2 == 7, "coroutine(dofile) failed on extra args")
print("dofile_ok")
`
	s := vm.NewState()
	defer s.Close()
	var buf bytes.Buffer
	s.SetOutput(&buf)
	cl, err := s.LoadChunk([]byte(script), "=dofile_extra_args")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(cl, vm.StringValue(s.Intern(path))); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "dofile_ok\n" {
		t.Fatalf("unexpected output: %q", buf.String())
	}

	// Direct State.Call(dofile, path, "b", {marker=99})
	s2 := vm.NewState()
	defer s2.Close()
	s2.SetGlobal("marker", vm.IntValue(7))
	envTbl := s2.NewTable(0, 1)
	envTbl.RawSetStr(s2, s2.Intern("marker"), vm.IntValue(99))
	dofileFn := s2.GetGlobal("dofile")
	res, err := s2.Call(dofileFn, vm.StringValue(s2.Intern(path)), vm.StringValue(s2.Intern("b")), vm.TableValue(envTbl))
	if err != nil {
		t.Fatalf("direct State.Call(dofile) failed: %v", err)
	}
	got, ok := res[0].ToInt()
	if !ok || got != 7 {
		t.Fatalf("direct State.Call(dofile) expected 7, got %v", res[0])
	}
}

func TestFileFlushSynchronizesBufferedInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flush_sync.txt")
	if err := os.WriteFile(path, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}

	script := `
local p = ...
local r = assert(io.open(p, "rb"))
assert(r:read(1) == "a")
assert(r:flush())
local w = assert(io.open(p, "r+b"))
assert(w:seek("set", 1))
assert(w:write("X"))
assert(w:close())
assert(r:read(1) == "X", "flush must discard stale read-ahead bytes")
assert(r:close())

-- Verify read -> flush -> write on r+b writes at logical position
local rw = assert(io.open(p, "r+b"))
assert(rw:read(1) == "a")
assert(rw:flush())
assert(rw:write("Z"))
assert(rw:flush())
assert(rw:seek("set", 0))
assert(rw:read("a") == "aZcdef")
assert(rw:close())
`
	s := vm.NewState()
	defer s.Close()
	cl, err := s.LoadChunk([]byte(script), "=flush_sync")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(cl, vm.StringValue(s.Intern(path))); err != nil {
		t.Fatal(err)
	}

	// Non-seekable pipe flush must succeed and preserve unread buffered input.
	if runtime.GOOS != "windows" {
		pipeScript := `
local p = assert(io.popen("printf hello", "r"))
assert(p:read(2) == "he")
assert(p:flush())
assert(p:read("a") == "llo")
assert(p:close())
`
		cl2, err := s.LoadChunk([]byte(pipeScript), "=flush_pipe")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(cl2); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUTF8ContinuationScanCancelAndBudget(t *testing.T) {
	contRun := strings.Repeat("\x80", 4*1024*1024)
	cases := []struct {
		name string
		str  string
		n    int64
	}{
		{name: "forward", str: "a" + contRun, n: 2},
		{name: "backward", str: "a" + contRun, n: -1},
		{name: "endpos", str: "\xC2" + contRun, n: 1},
	}

	for _, tc := range cases {
		t.Run("limit_"+tc.name, func(t *testing.T) {
			s := vm.NewState()
			defer s.Close()
			s.SetInstructionLimit(100)
			utf8Tbl := s.AsTable(s.GetGlobal("utf8"))
			offsetFn := utf8Tbl.RawGetStr(s, s.Intern("offset"))
			_, err := s.Call(offsetFn, vm.StringValue(s.Intern(tc.str)), vm.IntValue(tc.n))
			if !errors.Is(err, vm.ErrInstructionLimit) {
				t.Fatalf("utf8.offset(%s) expected ErrInstructionLimit, got %v", tc.name, err)
			}
		})
		t.Run("cancel_"+tc.name, func(t *testing.T) {
			s := vm.NewState()
			defer s.Close()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.SetContext(ctx)
			utf8Tbl := s.AsTable(s.GetGlobal("utf8"))
			offsetFn := utf8Tbl.RawGetStr(s, s.Intern("offset"))
			_, err := s.Call(offsetFn, vm.StringValue(s.Intern(tc.str)), vm.IntValue(tc.n))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("utf8.offset(%s) expected context.Canceled, got %v", tc.name, err)
			}
		})
	}

	t.Run("codes_iter_pos0_limit", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		utf8Tbl := s.AsTable(s.GetGlobal("utf8"))
		codesFn := utf8Tbl.RawGetStr(s, s.Intern("codes"))
		res, err := s.Call(codesFn, vm.StringValue(s.Intern("a")))
		if err != nil {
			t.Fatal(err)
		}
		iterFn := res[0]
		s.SetInstructionLimit(100)
		_, err = s.Call(iterFn, vm.StringValue(s.Intern(contRun)), vm.IntValue(0))
		if !errors.Is(err, vm.ErrInstructionLimit) {
			t.Fatalf("utf8.codes iter(pos=0) expected ErrInstructionLimit, got %v", err)
		}
	})
}

func TestColdOpcodeCallbackSwitchesToHookedDispatch(t *testing.T) {
	t.Run("native_index_enables_instruction_limit", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		s.SetGlobal("arm_limit", s.NativeValue(&vm.NativeFunc{
			Name: "arm_limit",
			Fn: func(st *vm.State, _ []vm.Value) ([]vm.Value, error) {
				st.SetInstructionLimit(10)
				return []vm.Value{vm.IntValue(1)}, nil
			},
		}))
		cl, err := s.LoadChunk([]byte(`
			local t = setmetatable({}, { __index = arm_limit })
			local v = t.trigger
			local sum = v
			for i = 1, 10000 do
				sum = sum + i
			end
			return sum
		`), "=cold_limit")
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Call(cl)
		if !errors.Is(err, vm.ErrInstructionLimit) {
			t.Fatalf("expected ErrInstructionLimit after __index callback armed limit, got %v", err)
		}
	})

	t.Run("native_add_cold_opcode_enables_instruction_limit", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		s.SetGlobal("arm_limit", s.NativeValue(&vm.NativeFunc{
			Name: "arm_limit",
			Fn: func(st *vm.State, _ []vm.Value) ([]vm.Value, error) {
				st.SetInstructionLimit(10)
				return []vm.Value{vm.IntValue(1)}, nil
			},
		}))
		cl, err := s.LoadChunk([]byte(`
			local t = setmetatable({}, { __add = arm_limit })
			local v = t + 1
			local sum = v
			for i = 1, 10000 do
				sum = sum + i
			end
			return sum
		`), "=cold_add_limit")
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Call(cl)
		if !errors.Is(err, vm.ErrInstructionLimit) {
			t.Fatalf("expected ErrInstructionLimit after __add cold callback armed limit, got %v", err)
		}
	})

	t.Run("pure_lua_cold_metamethod_installs_count_hook", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local hits = 0
			local arr = setmetatable({3, 1, 2}, {
				__index = function(self, k)
					if k == "sort_and_hook" then
						table.sort(self, function(a, b)
							debug.sethook(function() hits = hits + 1 end, "", 5)
							return a < b
						end)
						return true
					end
				end
			})
			assert(arr.sort_and_hook)
			local sum = 0
			for i = 1, 200 do
				sum = sum + i
			end
			debug.sethook()
			return hits, sum
		`), "=cold_hook")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(cl)
		if err != nil {
			t.Fatal(err)
		}
		hits, ok := res[0].ToInt()
		if !ok || hits < 50 {
			t.Fatalf("expected caller loop to be traced by hook installed in __index callback, got hits=%v", res[0])
		}
	})
}

func TestSetMemoryLimitRecountsExistingAllocations(t *testing.T) {
	t.Run("fresh_state_baseline_enforced_immediately", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		const limit = int64(65536)
		s.SetMemoryLimit(limit)
		initial := s.MemoryUsage()
		if initial <= 4096 {
			t.Fatalf("expected SetMemoryLimit to recount stdlib baseline (>4096), got %d", initial)
		}
		strTbl := s.AsTable(s.GetGlobal("string"))
		repFn := strTbl.RawGetStr(s, s.Intern("rep"))
		res, err := s.Call(repFn, vm.StringValue(s.Intern("x")), vm.IntValue(49152))
		if err == nil {
			s.SetGlobal("PIN", res[0])
			s.CollectFull()
			if used := s.MemoryUsage(); used > limit {
				t.Fatalf("string.rep('x', 49152) succeeded under limit=%d but pinned live usage is %d", limit, used)
			}
		} else if !strings.Contains(err.Error(), "not enough memory") {
			t.Fatalf("expected 'not enough memory', got %v", err)
		}
	})

	t.Run("tier0_preloaded_data_accounted_on_enable", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		setup, err := s.LoadChunk([]byte(`
			PRELOAD = {}
			local p = string.rep("p", 4096)
			for i = 1, 64 do
				PRELOAD[i] = p .. tostring(i)
			end
		`), "=tier0_preload")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(setup); err != nil {
			t.Fatal(err)
		}
		// Transition from unlimited (0) to limited (> 0): must recount PRELOAD immediately.
		s.SetMemoryLimit(64 << 20)
		baseline := s.MemoryUsage()
		if baseline < 256*1024 {
			t.Fatalf("expected recounted baseline >= 256 KiB for preloaded Tier-0 data, got %d", baseline)
		}
		s.SetMemoryLimit(0)
		// Enable a tight limit directly from unlimited state without calling CollectFull first.
		tight := baseline + 16*1024
		s.SetMemoryLimit(tight)
		strTbl := s.AsTable(s.GetGlobal("string"))
		repFn := strTbl.RawGetStr(s, s.Intern("rep"))
		_, err = s.Call(repFn, vm.StringValue(s.Intern("z")), vm.IntValue(64*1024))
		if err == nil || !strings.Contains(err.Error(), "not enough memory") {
			t.Fatalf("expected 'not enough memory' for 64 KiB alloc with 16 KiB headroom above Tier-0 preload, got %v", err)
		}
	})
}

func TestStringFormatLargeStringCancelAndBudget(t *testing.T) {
	big := strings.Repeat("a", 4*1024*1024)
	for _, spec := range []string{"%s", "%-10s", "%10s"} {
		t.Run("limit_"+spec, func(t *testing.T) {
			s := vm.NewState()
			defer s.Close()
			s.SetInstructionLimit(100)
			strTbl := s.AsTable(s.GetGlobal("string"))
			fmtFn := strTbl.RawGetStr(s, s.Intern("format"))
			_, err := s.Call(fmtFn, vm.StringValue(s.Intern(spec)), vm.StringValue(s.Intern(big)))
			if !errors.Is(err, vm.ErrInstructionLimit) {
				t.Fatalf("string.format(%q, 4MiB) expected ErrInstructionLimit, got %v", spec, err)
			}
		})
		t.Run("cancel_"+spec, func(t *testing.T) {
			s := vm.NewState()
			defer s.Close()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.SetContext(ctx)
			strTbl := s.AsTable(s.GetGlobal("string"))
			fmtFn := strTbl.RawGetStr(s, s.Intern("format"))
			_, err := s.Call(fmtFn, vm.StringValue(s.Intern(spec)), vm.StringValue(s.Intern(big)))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("string.format(%q, 4MiB) expected context.Canceled, got %v", spec, err)
			}
		})
	}
}

func TestShapeTransitionRootsNewShapeAcrossSlotGrowthMemLimit(t *testing.T) {
	runCase := func(t *testing.T, viaRawSet bool) {
		for _, headroom := range []int64{64, 128, 160, 200, 256, 320, 400, 512} {
			s := vm.NewState()
			defer s.Close()
			setup, err := s.LoadChunk([]byte(`
				t = {a = 1, b = 2}
				function add() t.c = 42 end
			`), "=shape_root")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Call(setup); err != nil {
				t.Fatal(err)
			}
			s.SetMemoryLimit(64 << 20)
			s.CollectFull()
			base := s.MemoryUsage()
			limit := base + headroom
			s.SetMemoryLimit(limit)

			var opErr error
			if viaRawSet {
				tbl := s.AsTable(s.GetGlobal("t"))
				k := s.Intern("c")
				tbl.RawSetStr(s, k, vm.IntValue(42))
				opErr = s.ConsumeMemErr()
			} else {
				addFn := s.GetGlobal("add")
				_, opErr = s.Call(addFn)
			}
			if opErr == nil {
				s.CollectFull()
				if used := s.MemoryUsage(); used > limit {
					t.Fatalf("headroom=%d viaRawSet=%v: assignment succeeded but post-GC live memory %d > limit %d (delta=%d)",
						headroom, viaRawSet, used, limit, used-base)
				}
			}
		}
	}
	t.Run("setfield", func(t *testing.T) { runCase(t, false) })
	t.Run("rawsetstr", func(t *testing.T) { runCase(t, true) })
}

func TestXPCallBypassesMessageHandlerOnAllocationError(t *testing.T) {
	s := vm.NewState()
	defer s.Close()
	cl, err := s.LoadChunk([]byte(`
		local direct_called = false
		local ok1, err1 = xpcall(function()
			return string.rep("x", 65536)
		end, function(e)
			direct_called = true
			return 42
		end)
		assert(not ok1, "expected allocation error")
		assert(err1 == "not enough memory", "expected raw 'not enough memory', got: " .. tostring(err1))
		assert(not direct_called, "xpcall msgh must NOT be called for LUA_ERRMEM")

		local nested_called = false
		local ok2, err2 = xpcall(function()
			table.sort({2, 1}, function()
				return string.rep("x", 65536)
			end)
		end, function(e)
			nested_called = true
			return 99
		end)
		assert(not ok2, "expected nested allocation error")
		assert(err2 == "not enough memory", "expected raw 'not enough memory' from nested callback, got: " .. tostring(err2))
		assert(not nested_called, "outer xpcall msgh must NOT be called for nested LUA_ERRMEM")

		-- Contrast: explicit user error("not enough memory") IS LUA_ERRRUN and MUST invoke msgh!
		local user_called = false
		local ok3, err3 = xpcall(function()
			error("not enough memory", 0)
		end, function(e)
			user_called = true
			return "handled:" .. tostring(e)
		end)
		assert(not ok3 and user_called and err3 == "handled:not enough memory")
	`), "=xpcall_errmem")
	if err != nil {
		t.Fatal(err)
	}
	clRef := s.Ref(cl)
	defer s.Unref(clRef)
	s.SetMemoryLimit(64 << 20)
	s.CollectFull()
	base := s.MemoryUsage()
	s.SetMemoryLimit(base + 16*1024)
	if _, err := s.Call(cl); err != nil {
		t.Fatalf("xpcall LUA_ERRMEM contract test failed: %v", err)
	}
}

func TestRequireDiagnosticsAndStringFormatErrorsCancelAndBudget(t *testing.T) {
	bigDiag := strings.Repeat("d", 8*1024*1024)
	t.Run("require_searcher_diag_instruction_limit", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		bigVal := vm.StringValue(s.Intern(bigDiag))
		pkgTbl := s.AsTable(s.GetGlobal("package"))
		searchers := s.NewTable(1, 0)
		searchers.RawSetInt(s, 1, s.NativeValue(&vm.NativeFunc{
			Name: "big_diag_searcher",
			Fn: func(*vm.State, []vm.Value) ([]vm.Value, error) {
				return []vm.Value{bigVal}, nil
			},
		}))
		pkgTbl.RawSetStr(s, s.Intern("searchers"), vm.TableValue(searchers))

		reqFn := s.GetGlobal("require")
		s.SetInstructionLimit(100)
		_, err := s.Call(reqFn, vm.StringValue(s.Intern("missing_mod")))
		if !errors.Is(err, vm.ErrInstructionLimit) {
			t.Fatalf("require(64-chunk diag) expected ErrInstructionLimit, got %v", err)
		}
		if s.InstructionsExecuted() <= 0 {
			t.Fatalf("expected non-zero instructions charged during require diagnostics copy")
		}
	})

	t.Run("require_searcher_diag_context_cancel", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		bigVal := vm.StringValue(s.Intern(bigDiag))
		pkgTbl := s.AsTable(s.GetGlobal("package"))
		searchers := s.NewTable(1, 0)
		searchers.RawSetInt(s, 1, s.NativeValue(&vm.NativeFunc{
			Name: "big_diag_searcher",
			Fn: func(*vm.State, []vm.Value) ([]vm.Value, error) {
				return []vm.Value{bigVal}, nil
			},
		}))
		pkgTbl.RawSetStr(s, s.Intern("searchers"), vm.TableValue(searchers))

		reqFn := s.GetGlobal("require")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.SetContext(ctx)
		_, err := s.Call(reqFn, vm.StringValue(s.Intern("missing_mod")))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("require(64-chunk diag) expected context.Canceled, got %v", err)
		}
	})

	t.Run("require_normal_not_found_format", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		pkgTbl := s.AsTable(s.GetGlobal("package"))
		searchers := s.NewTable(2, 0)
		searchers.RawSetInt(s, 1, s.NativeValue(&vm.NativeFunc{
			Name: "s1",
			Fn: func(st *vm.State, _ []vm.Value) ([]vm.Value, error) {
				return []vm.Value{vm.StringValue(st.Intern("no foo"))}, nil
			},
		}))
		searchers.RawSetInt(s, 2, s.NativeValue(&vm.NativeFunc{
			Name: "s2",
			Fn: func(st *vm.State, _ []vm.Value) ([]vm.Value, error) {
				return []vm.Value{vm.StringValue(st.Intern("no bar"))}, nil
			},
		}))
		pkgTbl.RawSetStr(s, s.Intern("searchers"), vm.TableValue(searchers))

		reqFn := s.GetGlobal("require")
		_, err := s.Call(reqFn, vm.StringValue(s.Intern("demo")))
		want := "module 'demo' not found:\n\tno foo\n\tno bar"
		if err == nil || err.Error() != want {
			t.Fatalf("require not-found message = %v, want %q", err, want)
		}
	})

	t.Run("searcher_preload_big_name_limit", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		bigVal := vm.StringValue(s.Intern(bigDiag))
		pkgTbl := s.AsTable(s.GetGlobal("package"))
		searchers := s.AsTable(pkgTbl.RawGetStr(s, s.Intern("searchers")))
		preloadFn := searchers.RawGetInt(s, 1)
		s.SetInstructionLimit(100)
		_, err := s.Call(preloadFn, bigVal)
		if !errors.Is(err, vm.ErrInstructionLimit) {
			t.Fatalf("searcher_preload(big name) expected ErrInstructionLimit, got %v", err)
		}
	})

	t.Run("tostring_big_name_limit", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		tbl := s.NewTable(0, 0)
		mt := s.NewTable(0, 1)
		mt.RawSetStr(s, s.Intern("__name"), vm.StringValue(s.Intern(bigDiag)))
		s.SetMetatable(tbl, mt)
		s.SetInstructionLimit(100)
		_, err := s.Call(s.GetGlobal("tostring"), vm.TableValue(tbl))
		if !errors.Is(err, vm.ErrInstructionLimit) {
			t.Fatalf("tostring(big __name) expected ErrInstructionLimit, got %v", err)
		}
	})

	t.Run("error_big_msg_limit", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		s.SetGlobal("BIG_ERR", vm.StringValue(s.Intern(bigDiag)))
		cl, err := s.LoadChunk([]byte(`error(BIG_ERR)`), "=big_err")
		if err != nil {
			t.Fatal(err)
		}
		s.SetInstructionLimit(100)
		_, err = s.Call(cl)
		if !errors.Is(err, vm.ErrInstructionLimit) {
			t.Fatalf("error(big msg) expected ErrInstructionLimit, got %v", err)
		}
	})
}

func TestNumericForNaNStepAndBoundsParity(t *testing.T) {
	s := vm.NewState()
	defer s.Close()
	cl, err := s.LoadChunk([]byte(`
		local function run(code)
			return assert(load(code))()
		end
		-- Float loops with NaN step (Lua 5.5.1 lvm.c:forprep takes descending branch when !(0 < step))
		assert(run("local c=0; for i=1.0,10.0,0/0 do c=c+1 end; return c") == 0)
		assert(run("local c=0; for i=1,10,0/0 do c=c+1 end; return c") == 0)
		assert(run("local c=0; for i=10.0,1.0,0/0 do c=c+1 end; return c") == 1)
		assert(run("local c=0; for i=1.0,1.0,0/0 do c=c+1 end; return c") == 1)
		-- Float loops with NaN init or NaN limit
		assert(run("local c=0; for i=0/0,10.0,1.0 do c=c+1 end; return c") == 1)
		assert(run("local c=0; for i=0/0,10.0,-1.0 do c=c+1 end; return c") == 1)
		assert(run("local c=0; for i=1.0,0/0,1.0 do c=c+1 end; return c") == 1)
		assert(run("local c=0; for i=1.0,0/0,-1.0 do c=c+1 end; return c") == 1)
		-- Integer loops with NaN limit (Lua 5.5.1 lvm.c:forlimit)
		assert(run("local c=0; for i=1,0/0,1 do c=c+1 end; return c") == 0)
		assert(run("local c=0; for i=1,0/0,-1 do c=c+1; if c>5 then break end end; return c") == 6)
		assert(run("local c=0; for i=math.mininteger,0/0,-1 do c=c+1; if c>5 then break end end; return c") == 1)
		-- Infinity edge cases
		assert(run("local c=0; for i=math.huge,-math.huge,-math.huge do c=c+1; if c>5 then break end end; return c") == 1)
		assert(run("local c=0; for i=math.huge,math.huge,math.huge do c=c+1; if c>5 then break end end; return c") == 6)
		assert(run("local c=0; for i=-math.huge,math.huge,math.huge do c=c+1; if c>5 then break end end; return c") == 1)
	`), "=for_nan")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(cl); err != nil {
		t.Fatalf("numeric for NaN/Inf parity failed: %v", err)
	}
}

func TestRegistryLayoutAndDynamicGlobalsParity(t *testing.T) {
	tmpFile := t.TempDir() + "/dyn_env_chunk.lua"
	if err := os.WriteFile(tmpFile, []byte("return marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, noEnv := range []bool{false, true} {
		name := "default"
		if noEnv {
			name = "noenv"
		}
		t.Run(name, func(t *testing.T) {
			var s *vm.State
			if noEnv {
				s = vm.NewStateNoEnv()
			} else {
				s = vm.NewState()
			}
			defer s.Close()
			s.SetGlobal("TMP_CHUNK_FILE", vm.StringValue(s.Intern(tmpFile)))

			cl, err := s.LoadChunk([]byte(`
				local reg = debug.getregistry()
				-- Lua 5.5.1 lua.h / lstate.c: [1]=false (ref), [2]=_G (LUA_RIDX_GLOBALS), [3]=mainthread (LUA_RIDX_MAINTHREAD)
				assert(reg[1] == false, "expected reg[1] == false, got " .. tostring(reg[1]))
				assert(reg[2] == _G, "expected reg[2] == _G")
				local main, ismain = coroutine.running()
				assert(ismain and reg[3] == main, "expected reg[3] == main thread")

				local co = coroutine.create(function()
					local cur, isM = coroutine.running()
					assert(not isM and reg[3] == main and reg[3] ~= cur, "reg[3] must stay main thread inside coroutine")
				end)
				local ok, cerr = coroutine.resume(co)
				assert(ok, cerr)

				-- Replacing reg[2] dynamically redirects load / loadfile / dofile default _ENV
				local oldG = reg[2]
				local collected = false
				local replacedEnv = setmetatable({marker = 99}, {
					__gc = function() collected = true end,
				})
				reg[2] = replacedEnv
				assert(assert(load("return marker"))() == 99)
				assert(assert(loadfile(TMP_CHUNK_FILE))() == 99)
				assert(dofile(TMP_CHUNK_FILE) == 99)
				-- Explicit env overrides registry[2]
				assert(assert(load("return marker", "=exp", "t", {marker = 7}))() == 7)

				-- Non-table and nil values in reg[2] are also passed verbatim as _ENV
				reg[2] = "not_a_table"
				assert(assert(load("return _ENV"))() == "not_a_table")
				reg[2] = nil
				assert(assert(load("return _ENV"))() == nil)

				-- Restore and ensure replacedEnv is collectable (not pinned by any obsolete root)
				reg[2] = oldG
				replacedEnv = nil
				collectgarbage("collect")
				assert(collected, "replaced global environment table must be collected")
				assert(assert(load("return _G == _ENV"))() == true)
			`), "=reg_layout")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Call(cl); err != nil {
				t.Fatalf("registry layout & dynamic globals test failed: %v", err)
			}

			// Also verify Go State.LoadChunk / GlobalEnv / GetGlobal / GetGlobalMM observe registry[2] replacement.
			alt := s.NewTable(0, 1)
			alt.RawSetStr(s, s.Intern("marker"), vm.IntValue(123))
			s.Registry().RawSetInt(s, 2, vm.TableValue(alt))
			if s.GlobalEnv() != alt {
				t.Fatalf("GlobalEnv() did not reflect updated registry[2]")
			}
			if got := s.GetGlobal("marker"); !got.IsInt() || got.Int() != 123 {
				t.Fatalf("GetGlobal(marker) = %v, want 123", got)
			}
			if got, err := s.GetGlobalMM("marker"); err != nil || !got.IsInt() || got.Int() != 123 {
				t.Fatalf("GetGlobalMM(marker) = (%v, %v), want (123, nil)", got, err)
			}
			c2, err := s.LoadChunk([]byte(`return marker`), "=go_load")
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Call(c2)
			if err != nil || len(res) != 1 || !res[0].IsInt() || res[0].Int() != 123 {
				t.Fatalf("LoadChunk after registry[2] swap returned (%v, %v), want (123, nil)", res, err)
			}
		})
	}
}

func TestTableArgErrorGlobalAndCallSiteNames(t *testing.T) {
	run := func(t *testing.T, hooked bool) {
		s := vm.NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function check(code, want)
				local ok, err = pcall(assert(load(code)))
				assert(not ok, "expected error for " .. code)
				assert(string.find(tostring(err), want, 1, true),
					"code=" .. code .. " got=" .. tostring(err) .. " want substring=" .. want)
			end
			check("table.sort(1)", "to 'sort'")
			check("local f = table.sort; f(1)", "to 'f'")
			check("local function g() return table.sort(1) end; g()", "to 'sort'")
			check("table.sort({1,2,3}, table.sort)", "to 'table.sort'")
			check("table.sort({1,2,3}, table.insert)", "to 'table.insert'")
			check("local ok, e = pcall(table.sort, 1); assert(not ok); error(e, 0)", "to 'table.sort'")
			local aaa = setmetatable({}, {__index = string})
			_G._aaa = aaa
			check("_aaa:sub()", "calling 'sub' on bad self")
			check("('a'):sub{}", "bad argument #1 to 'sub'")
			_G._aaa = nil
		`), "=argerr_names")
		if err != nil {
			t.Fatal(err)
		}
		clRef := s.Ref(cl)
		defer s.Unref(clRef)
		if hooked {
			setup, err := s.LoadChunk([]byte(`debug.sethook(function() end, "cr")`), "=hook")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Call(setup); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Call(cl); err != nil {
			t.Fatalf("arg error name test failed (hooked=%v): %v", hooked, err)
		}
	}
	t.Run("unhooked", func(t *testing.T) { run(t, false) })
	t.Run("hooked", func(t *testing.T) { run(t, true) })
}

func TestOPClosureStoresBeforeFinalizerGrowsStack(t *testing.T) {
	s := vm.NewState()
	defer s.Close()

	cl, err := s.LoadChunk([]byte(`
		local function grow(n)
			if n <= 0 then return 0 end
			local a, b, c, d, e, f, g, h = 1, 2, 3, 4, 5, 6, 7, 8
			return grow(n - 1) + a - a + b - b + c - c + d - d + e - e + f - f + g - g + h - h
		end
		local finRan = 0
		for i = 1, 32 do
			setmetatable({}, {
				__gc = function()
					finRan = finRan + 1
					grow(180) -- forces active thread stack reallocation during GC
				end
			})
		end
		-- Arm GC threshold to fire frequently while allocating closures in OP_CLOSURE.
		collectgarbage("param", "pause", 100)
		collectgarbage("param", "stepsize", 1)
		local sum = 0
		for i = 1, 2000 do
			local x = i
			local f = function() return x end
			sum = sum + f()
		end
		return sum, finRan
	`), "=closure_gc_stack_grow")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Call(cl)
	if err != nil {
		t.Fatalf("OP_CLOSURE with stack-growing __gc failed: %v", err)
	}
	wantSum := int64(2000 * 2001 / 2)
	gotSum, ok1 := res[0].ToInt()
	gotFin, ok2 := res[1].ToInt()
	if len(res) != 2 || !ok1 || !ok2 || gotSum != wantSum || gotFin == 0 {
		t.Fatalf("got sum=%v finRan=%v, want sum=%d finRan>0", res[0], res[1], wantSum)
	}
}

func TestConcatAndStringOpsAccountMemoryOnce(t *testing.T) {
	s := vm.NewState()
	defer s.Close()
	s.SetMemoryLimit(64 * 1024 * 1024)

	cl, err := s.LoadChunk([]byte(`
		collectgarbage("stop")
		local a = string.rep("a", 50)
		local b = string.rep("b", 50)
		return function()
			return a .. b
		end, function()
			return string.reverse(a)
		end, function()
			return string.upper(a)
		end
	`), "=concat_single_account")
	if err != nil {
		t.Fatal(err)
	}
	fns, err := s.Call(cl)
	if err != nil {
		t.Fatal(err)
	}

	before := s.MemoryUsage()
	res, err := s.Call(fns[0])
	if err != nil {
		t.Fatal(err)
	}
	after := s.MemoryUsage()
	delta := after - before
	if len(res) != 1 || len(s.AsString(res[0]).Str()) != 100 {
		t.Fatalf("unexpected concat result: %v", res)
	}
	// 100-byte string header + payload should be accounted once (~148B-176B), never double-counted (~296B+).
	if delta > 220 {
		t.Fatalf("concat double-accounted string memory: before=%d after=%d delta=%d", before, after, delta)
	}

	beforeRev := s.MemoryUsage()
	if _, err := s.Call(fns[1]); err != nil {
		t.Fatal(err)
	}
	if deltaRev := s.MemoryUsage() - beforeRev; deltaRev > 160 {
		t.Fatalf("string.reverse double-accounted string memory: delta=%d", deltaRev)
	}

	beforeUp := s.MemoryUsage()
	if _, err := s.Call(fns[2]); err != nil {
		t.Fatal(err)
	}
	if deltaUp := s.MemoryUsage() - beforeUp; deltaUp > 160 {
		t.Fatalf("string.upper double-accounted string memory: delta=%d", deltaUp)
	}
}
