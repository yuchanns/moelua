package vm_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/yuchanns/moelua/vm"
)

func TestPhase1CoroutinesTBCDebugGC(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "coroutines_yield_across_pcall_metamethods_for",
			src: `
-- 1. Nested calls + yield + resume args
local function inner(x)
  local a, b = coroutine.yield(x + 1, x + 2)
  return a * b
end
local function outer(x)
  return inner(x * 10)
end
local co = coroutine.create(outer)
print(coroutine.status(co))
local ok1, v1, v2 = coroutine.resume(co, 5)
print(ok1, v1, v2, coroutine.status(co))
local ok2, v3 = coroutine.resume(co, 6, 7)
print(ok2, v3, coroutine.status(co))

-- 2. Yield across pcall & xpcall!
local co2 = coroutine.create(function()
  local ok, a, b = pcall(function()
    local r = coroutine.yield("inside_pcall")
    return r .. "_ok", 99
  end)
  return ok, a, b
end)
print(coroutine.resume(co2))
print(coroutine.resume(co2, "resumed"))

-- 3. Yield across __add and __index metamethods!
local mt = {
  __add = function(a, b)
    local delta = coroutine.yield("add_yield")
    return a.v + b.v + delta
  end,
  __index = function(t, k)
    local v = coroutine.yield("idx_" .. k)
    return v
  end,
}
local o1 = setmetatable({v = 10}, mt)
local o2 = setmetatable({v = 20}, mt)
local co3 = coroutine.create(function()
  local s = o1 + o2
  local f = o1.missing
  return s, f
end)
print(coroutine.resume(co3))
print(coroutine.resume(co3, 5))
print(coroutine.resume(co3, 77))

-- 4. coroutine.wrap
local gen = coroutine.wrap(function(n)
  for i = 1, n do coroutine.yield(i * i) end
  return "done"
end)
print(gen(3), gen(), gen(), gen())
`,
		},
		{
			name: "tbc_close_order_errors_and_coroutine_close",
			src: `
local log = {}
local function closer(name)
  return setmetatable({}, {
    __close = function(self, err)
      log[#log + 1] = name .. ":" .. tostring(err)
    end
  })
end

do
  local a <close> = closer("a")
  local b <close> = closer("b")
end
print(table.concat(log, ","))

log = {}
local ok, err = pcall(function()
  local x <close> = closer("x")
  local y <close> = closer("y")
  error("boom", 0)
end)
print(ok, err, table.concat(log, ","))

-- coroutine.close closes suspended coroutine's <close> vars!
log = {}
local co = coroutine.create(function()
  local z <close> = closer("z")
  coroutine.yield("paused")
end)
coroutine.resume(co)
print("before_close", #log, coroutine.status(co))
local cok = coroutine.close(co)
print("after_close", cok, coroutine.status(co), table.concat(log, ","))
`,
		},
		{
			name: "string_arithmetic_and_lua55_errors",
			src: `
print("10" + 5, "3.5" * "2", -"42", "17" // "5", "17" % "5")
local ok1, e1 = pcall(function() error() end)
print(ok1, e1)
local errTbl = { code = 404 }
local ok2, e2 = pcall(function() error(errTbl) end)
print(ok2, e2 == errTbl, e2.code)
local ok3, e3 = xpcall(function() error("fail") end, function(e) return nil end)
print(ok3, e3)
`,
		},
		{
			name: "debug_introspection_and_hooks",
			src: `
local function foo(a, b)
  local c = a + b
  local n1, v1 = debug.getlocal(1, 1)
  local n3, v3 = debug.getlocal(1, 3)
  debug.setlocal(1, 3, 100)
  return n1, v1, n3, v3, c
end
print(foo(3, 4))

-- upvalueid / upvaluejoin
local function makePair()
  local x = 10
  local y = 20
  return function() return x end, function() return y end
end
local f1, f2 = makePair()
print(f1(), f2(), debug.upvalueid(f1, 1) == debug.upvalueid(f2, 1))
debug.upvaluejoin(f1, 1, f2, 1)
print(f1(), f2(), debug.upvalueid(f1, 1) == debug.upvalueid(f2, 1))

-- debug.sethook count hook (exercises runHooked!)
local ticks = 0
debug.sethook(function(ev) ticks = ticks + 1 end, "", 5)
local s = 0
for i = 1, 20 do s = s + i end
debug.sethook()
print(s, ticks > 0)
`,
		},
		{
			name: "tier1_gc_ephemerons_lifo_weak_order_resurrection",
			src: `
-- 1. LIFO __gc finalizer ordering + weak-val cleared BEFORE __gc + weak-key ALIVE DURING __gc!
local order = {}
local weakV = setmetatable({}, { __mode = "v" })
local weakK = setmetatable({}, { __mode = "k" })
local seenDuringGC = {}

do
  local mt1 = {
    __gc = function(self)
      order[#order + 1] = self.name
      seenDuringGC[self.name .. "_v"] = (weakV[self.name] == nil)
      seenDuringGC[self.name .. "_k"] = (weakK[self] ~= nil)
    end
  }
  local a = setmetatable({ name = "A" }, mt1)
  local b = setmetatable({ name = "B" }, mt1)
  local c = setmetatable({ name = "C" }, mt1)
  weakV["A"], weakV["B"], weakV["C"] = a, b, c
  weakK[a], weakK[b], weakK[c] = 1, 2, 3
end
collectgarbage("collect")
print("lifo", table.concat(order, ","))
print("weakV_cleared_before_gc", seenDuringGC.A_v, seenDuringGC.B_v, seenDuringGC.C_v)
print("weakK_alive_during_gc", seenDuringGC.A_k, seenDuringGC.B_k, seenDuringGC.C_k)
-- Second cycle clears weak keys of non-resurrected finalized objects:
collectgarbage("collect")
print("weakK_cleared_after_second_gc", next(weakK) == nil)

-- 2. Ephemeron cycle: k1 -> {k2}, k2 -> {k3}, ..., k20 -> {k1} (must ALL collect!)
local eph = setmetatable({}, { __mode = "k" })
do
  local first = {}
  local prev = first
  for i = 1, 20 do
    local cur = {}
    eph[prev] = { ref = cur }
    prev = cur
  end
  eph[prev] = { ref = first }
end
collectgarbage("collect")
print("ephemeron_cycle_cleared", next(eph) == nil)

-- 3. Strings are NEVER weak!
local ws = setmetatable({}, { __mode = "kv" })
ws["keep_" .. tostring(123)] = "val_" .. tostring(456)
collectgarbage("collect")
print("strings_not_weak", ws["keep_123"])

-- 4. Resurrection & re-arming inside __gc (tracegc.lua pattern!)
local rearmCount = 0
local saved = nil
do
  local mt
  mt = {
    __gc = function(o)
      rearmCount = rearmCount + 1
      if rearmCount == 1 then
        saved = o
        setmetatable(o, mt) -- re-arm!
      end
    end
  }
  local x = setmetatable({}, mt)
end
collectgarbage("collect")
assert(rearmCount == 1 and saved ~= nil)
saved = nil
collectgarbage("collect")
print("rearm_count", rearmCount)
`,
		},
		{
			name: "io_tmpfile_and_os_time_date",
			src: `
local f = assert(io.tmpfile())
f:write("hello\nworld\n42\n")
f:seek("set", 0)
print(f:read("l"), f:read("l"), f:read("n"))
f:close()
print(io.type(f))

local t0 = os.time({ year = 2025, month = 6, day = 15, hour = 12, min = 30, sec = 0 })
local d = os.date("*t", t0)
print(d.year, d.month, d.day, d.hour, d.min, os.difftime(t0 + 60, t0))
`,
		},
	}

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

func TestStateCloseRunsRemainingFinalizersLIFO(t *testing.T) {
	chunk := compileLua55(t, `
local mt = { __gc = function(o) print("close_" .. o.id) end }
A = setmetatable({ id = 1 }, mt)
B = setmetatable({ id = 2 }, mt)
C = setmetatable({ id = 3 }, mt)
`)
	s := vm.NewState()
	var buf bytes.Buffer
	s.SetOutput(&buf)
	cl, err := s.UndumpAndLoad(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(vm.ClosureValue(cl)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	want := "close_3\nclose_2\nclose_1\n"
	if buf.String() != want {
		t.Fatalf("State.Close LIFO mismatch: got %q, want %q", buf.String(), want)
	}
}

func TestGeneratedInterpFilesExist(t *testing.T) {
	for _, f := range []string{"interp_run_regcheck.go", "interp_run_hooked.go"} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("generated file %s missing (run go generate ./vm/...): %v", f, err)
		}
	}
}
