package vm_test

import (
	"bytes"
	"context"
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

	var wp weak.Pointer[vm.Table]
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
		wp = weak.Make(res[0].AsTable())
		res[0] = vm.NilValue()
	}()

	// Do NOT call s.CollectFull(); Go's GC alone must reclaim the deleted table key in Tier 0.
	reclaimed := false
	for range 10 {
		runtime.GC()
		if wp.Value() == nil {
			reclaimed = true
			break
		}
	}
	if !reclaimed {
		t.Fatalf("deleted collectable hash key remained strongly retained by tombstone in Tier 0")
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
		wTbl := s.GetGlobal("W").AsTable()
		cb := vm.NativeValue(&vm.NativeFunc{
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
			if !wTbl.RawGetStr(cStr).IsNil() {
				t.Fatalf("expected stack growth to trigger emergency GC and clear canary W.c")
			}
			gotWeak := wTbl.RawGetStr(kStr)
			if !gotWeak.IsTable() || gotWeak.AsTable() != res[0].AsTable() {
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
			s.SetMemoryLimit(64 << 20)
			s.CollectFull()
			baseMem = s.MemoryUsage()
			s.SetMemoryLimit(baseMem + 96*1024)
			if _, err := s.Call(caller); err != nil {
				t.Fatalf("Lua call failed: %v", err)
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
		mathSqrt := s.GetGlobal("math").AsTable().RawGetStr(s.Intern("sqrt"))
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
		mathSqrt2 := s2.GetGlobal("math").AsTable().RawGetStr(s2.Intern("sqrt"))
		bigVal2 := vm.StringValue(s2.Intern(bigZeros))
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		s2.SetContext(ctx)
		if _, err := s2.Call(mathSqrt2, bigVal2); err == nil {
			t.Fatalf("direct State.Call(math.sqrt, bigZeros) bypassed expired context deadline")
		}
	})
}
