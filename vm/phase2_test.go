package vm_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/yuchanns/moelua/compiler"
	"github.com/yuchanns/moelua/vm"
)

// TestPhase2P1MemoryLimits verifies per-State memory caps catch
// huge string.rep, table.create, NEWTABLE loops, single-table array/hash growth,
// and OP_CONCAT bombs as recoverable Lua "not enough memory" errors.
func TestPhase2P1MemoryLimits(t *testing.T) {
	s := vm.NewState()
	defer s.Close()
	s.SetMemoryLimit(512 * 1024)

	run := func(name, src string) {
		t.Helper()
		p, err := compiler.Compile("="+name, "local ok, err = xpcall(function()\n"+src+"\nend, debug.traceback); if not ok then error(err, 0) end")
		if err != nil {
			t.Fatalf("%s compile: %v", name, err)
		}
		cl, err := s.Load(p)
		if err != nil {
			t.Fatalf("%s load: %v", name, err)
		}
		if _, err := s.Call(vm.ClosureValue(cl)); err != nil {
			t.Fatalf("%s failed: %v", name, err)
		}
	}

	run("mem_caps", `
		local function assertOOM(label, f)
			local ok, err = pcall(f)
			assert(not ok, label .. ": expected error")
			assert(tostring(err):find("not enough memory"), label .. ": unexpected error: " .. tostring(err))
		end

		assertOOM("string.rep", function() return string.rep("a", 2 * 1024 * 1024) end)
		assertOOM("table.create", function() return table.create(100000, 0) end)
		assertOOM("newtable_loop", function()
			local keep = {}
			for i = 1, 50000 do keep[i] = {i, i+1} end
		end)
		assertOOM("array_growth", function()
			local t = {}
			for i = 1, 200000 do t[i] = i end
		end)
		assertOOM("hash_growth", function()
			local t = {}
			for i = 1, 50000 do t["k_" .. i] = i end
		end)
		assertOOM("concat_bomb", function()
			local s = "0123456789abcdef"
			for i = 1, 30 do s = s .. s end
		end)
		-- Live strings retained in a table must remain accounted across emergency GC!
		assertOOM("string_retention", function()
			local keep = {}
			for i = 1, 50 do keep[i] = string.rep("x", 64 * 1024) end
		end)
		-- table.insert hitting the memory limit must raise an error instead of dropping values!
		assertOOM("table_insert", function()
			local t = {}
			for i = 1, 200000 do table.insert(t, i) end
		end)
		collectgarbage()
		assert(1 + 1 == 2)
	`)

	// Emergency GC triggered during table growth must NOT run __gc finalizers mid-allocation!
	run("emergency_gc_no_finalizer", `
		local ranDuringGrow = false
		local growing = {}
		do
			local _ = setmetatable({}, {__gc = function()
				ranDuringGrow = true
				growing[1] = "mutated"
			end})
		end
		local ok, err = pcall(function()
			for i = 1, 200000 do growing[i] = i end
		end)
		assert(not ok and tostring(err):find("not enough memory"), tostring(err))
		assert(not ranDuringGrow, "emergency GC must NOT invoke __gc finalizers mid-allocation")
		-- Explicit collectgarbage() at safe point drains finalizer cleanly.
		growing = nil
		collectgarbage()
		assert(ranDuringGrow == true)
	`)
}

// TestPhase2LoadLoopNoProtoCacheLeak verifies repeated load() calls do not leak
// boundProto or IC slices across a long-lived State.
func TestPhase2LoadLoopNoProtoCacheLeak(t *testing.T) {
	s := vm.NewState()
	defer s.Close()
	s.SetMemoryLimit(4 * 1024 * 1024)

	p, err := compiler.Compile("=loadloop", `
		local sum = 0
		for i = 1, 3000 do
			local f = assert(load("local t = {a = " .. i .. "}; return t.a"))
			sum = sum + f()
		end
		return sum
	`)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := s.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	res, err := s.Call(vm.ClosureValue(cl))
	if err != nil {
		t.Fatalf("load loop failed: %v", err)
	}
	if len(res) != 1 || res[0].Int() != 3000*3001/2 {
		t.Fatalf("unexpected sum: %v", res)
	}
	s.CollectFull()
	runtime.GC()
	runtime.ReadMemStats(&m1)
	if s.MemoryUsage() > 512*1024 {
		t.Fatalf("VM MemoryUsage leaked after load loop: %d bytes", s.MemoryUsage())
	}
	if m1.HeapAlloc > m0.HeapAlloc+8*1024*1024 {
		t.Fatalf("Go heap leaked after load loop: before=%d after=%d", m0.HeapAlloc, m1.HeapAlloc)
	}
}

// TestPhase2P4StaleFramesAndCoroutineStackShrink verifies that
// CollectFull clears stale frames/stack slots so dead objects are not pinned,
// and shrinks oversized stacks of non-running / dead coroutines without breaking
// suspended resumes or dead-coro tracebacks.
func TestPhase2P4StaleFramesAndCoroutineStackShrink(t *testing.T) {
	runLuaSnippet(t, `
		local weak = setmetatable({}, {__mode = "v"})
		local function deep(n)
			local obj = { val = n }
			if n == 1 then weak[1] = obj end
			if n <= 0 then return 0 end
			return deep(n - 1) + 1
		end
		assert(deep(100) == 100)
		-- Stale frames/stack slots above current nframe/top must be cleared by GC!
		collectgarbage()
		assert(weak[1] == nil, "stale frame/stack slot pinned dead object")

		-- Coroutine grows a large stack, returns to shallow frame, yields, GC shrinks it, resume still works!
		local co = coroutine.create(function()
			local function grow(n)
				local a,b,c,d,e,f,g,h = n,n,n,n,n,n,n,n
				if n <= 0 then return a end
				return grow(n - 1) + (b - c)
			end
			assert(grow(400) == 0)
			local v = coroutine.yield("grown")
			return v .. "-resumed"
		end)
		local ok, r1 = coroutine.resume(co)
		assert(ok and r1 == "grown")
		collectgarbage()
		local ok2, r2 = coroutine.resume(co, "ok")
		assert(ok2 and r2 == "ok-resumed")
		collectgarbage()
		assert(coroutine.status(co) == "dead")

		-- Dead errored coroutine still keeps debug.traceback working after GC!
		local bad = coroutine.create(function()
			local function boom() error("kaboom") end
			boom()
		end)
		local bok, berr = coroutine.resume(bad)
		assert(not bok and tostring(berr):find("kaboom"))
		collectgarbage()
		local tb = debug.traceback(bad)
		assert(tb:find("boom"), "traceback lost after GC: " .. tb)
	`)
}

// TestPhase2P5FloatSortNaNParity verifies float typed-array table.sort
// with NaN falls back to auxsort and matches official C Lua 5.5.1 behavior.
func TestPhase2P5FloatSortNaNParity(t *testing.T) {
	cBin := findCLua55(t)
	scripts := []struct {
		name string
		src  string
	}{
		{
			name: "single_nan",
			src: `
				local t = {3.0, 1.0, 0/0, 2.0, 4.0}
				local ok, err = pcall(table.sort, t)
				print(ok, not ok and tostring(err):find("invalid order") ~= nil)
				if ok then
					local out = {}
					for i = 1, #t do out[i] = tostring(t[i]) end
					print(table.concat(out, ","))
				end
			`,
		},
		{
			name: "many_nans",
			src: `
				local t = {0/0, 5.5, 0/0, 1.5, 0/0, 3.5, 2.5, 0/0}
				local ok, err = pcall(table.sort, t)
				print(ok, not ok and tostring(err):find("invalid order") ~= nil)
				if ok then
					local out = {}
					for i = 1, #t do out[i] = tostring(t[i]) end
					print(table.concat(out, ","))
				end
			`,
		},
		{
			name: "normal_floats",
			src: `
				local t = {5.5, -2.25, 0.0, 100.5, 1.25}
				table.sort(t)
				print(table.concat(t, ","))
			`,
		},
	}

	for _, tc := range scripts {
		t.Run(tc.name, func(t *testing.T) {
			cOut := strings.ReplaceAll(runCLuaSnippet(t, cBin, tc.src), "-nan", "nan")
			moeOut := strings.ReplaceAll(runMoeLuaCapture(t, tc.src), "-nan", "nan")
			if cOut != moeOut {
				t.Fatalf("mismatch vs C Lua 5.5.1:\n  C:      %q\n  MoeLua: %q", cOut, moeOut)
			}
		})
	}
}

// TestPhase2P6YieldAcrossNativeMatrix runs a comprehensive differential test matrix
// against live C Lua 5.5.1 verifying which native/metamethod callbacks are yieldable
// vs raise "attempt to yield across a C-call boundary".
func TestPhase2P6YieldAcrossNativeMatrix(t *testing.T) {
	cBin := findCLua55(t)

	cases := []struct {
		name string
		src  string
	}{
		{
			name: "table_sort_comparator",
			src: `
				local co = coroutine.create(function()
					table.sort({2, 1}, function(a, b) coroutine.yield("y"); return a < b end)
				end)
				local ok, err = coroutine.resume(co)
				print(ok, tostring(err):find("yield across") ~= nil)
			`,
		},
		{
			name: "string_gsub_func",
			src: `
				local co = coroutine.create(function()
					return string.gsub("abc", "a", function() coroutine.yield("y"); return "x" end)
				end)
				local ok, err = coroutine.resume(co)
				print(ok, tostring(err):find("yield across") ~= nil)
			`,
		},
		{
			name: "string_gsub_table_index",
			src: `
				local mt = { __index = function() coroutine.yield("y"); return "x" end }
				local co = coroutine.create(function()
					return string.gsub("a", "a", setmetatable({}, mt))
				end)
				local ok, err = coroutine.resume(co)
				print(ok, tostring(err):find("yield across") ~= nil)
			`,
		},
		{
			name: "load_reader",
			src: `
				local co = coroutine.create(function()
					return load(function() coroutine.yield("y"); return nil end)
				end)
				local ok, err = coroutine.resume(co)
				print(ok, tostring(err):find("yield across") ~= nil)
			`,
		},
		{
			name: "tostring_metamethod",
			src: `
				local x = setmetatable({}, { __tostring = function() coroutine.yield("y"); return "hi" end })
				local co = coroutine.create(function() return tostring(x) end)
				local ok, err = coroutine.resume(co)
				print(ok, tostring(err):find("yield across") ~= nil)
			`,
		},
		{
			name: "print_tostring_metamethod",
			src: `
				local x = setmetatable({}, { __tostring = function() coroutine.yield("y"); return "hi" end })
				local co = coroutine.create(function() print(x) end)
				local ok, err = coroutine.resume(co)
				print(ok, tostring(err):find("yield across") ~= nil)
			`,
		},
		{
			name: "require_loader",
			src: `
				package.preload["__yield_mod__"] = function() coroutine.yield("y"); return 123 end
				local co = coroutine.create(function() return require("__yield_mod__") end)
				local ok, err = coroutine.resume(co)
				print(ok, tostring(err):find("yield across") ~= nil)
			`,
		},
		{
			name: "table_insert_metamethod",
			src: `
				local t = setmetatable({}, {
					__len = function() return 1 end,
					__newindex = function() coroutine.yield("y") end,
				})
				local co = coroutine.create(function() table.insert(t, "v") end)
				local ok, err = coroutine.resume(co)
				print(ok, tostring(err):find("yield across") ~= nil)
			`,
		},
		{
			name: "table_concat_metamethod",
			src: `
				local t = setmetatable({}, {
					__len = function() return 2 end,
					__index = function() coroutine.yield("y"); return "a" end,
				})
				local co = coroutine.create(function() return table.concat(t) end)
				local ok, err = coroutine.resume(co)
				print(ok, tostring(err):find("yield across") ~= nil)
			`,
		},
		{
			name: "coroutine_close_tbc",
			src: `
				local inner = coroutine.create(function()
					local x <close> = setmetatable({}, { __close = function() coroutine.yield("y") end })
					coroutine.yield("ready")
				end)
				assert(coroutine.resume(inner))
				local outer = coroutine.create(function()
					return coroutine.close(inner)
				end)
				local ok, c1, c2 = coroutine.resume(outer)
				print(ok, c1, tostring(c2):find("yield across") ~= nil)
			`,
		},
		{
			name: "pcall_yieldable",
			src: `
				local co = coroutine.create(function()
					return pcall(function() return coroutine.yield("in-pcall") + 1 end)
				end)
				local ok1, v1 = coroutine.resume(co)
				local ok2, p, v2 = coroutine.resume(co, 41)
				print(ok1, v1, ok2, p, v2)
			`,
		},
		{
			name: "xpcall_yieldable",
			src: `
				local co = coroutine.create(function()
					return xpcall(function() return coroutine.yield("in-xpcall") * 2 end, function(e) return e end)
				end)
				local ok1, v1 = coroutine.resume(co)
				local ok2, p, v2 = coroutine.resume(co, 21)
				print(ok1, v1, ok2, p, v2)
			`,
		},
		{
			name: "pairs_metamethod_yieldable",
			src: `
				local t = setmetatable({}, {
					__pairs = function()
						local k = coroutine.yield("in-pairs")
						return next, { [k] = 99 }, nil
					end,
				})
				local co = coroutine.create(function()
					for k, v in pairs(t) do return k, v end
				end)
				local ok1, v1 = coroutine.resume(co)
				local ok2, rk, rv = coroutine.resume(co, "key")
				print(ok1, v1, ok2, rk, rv)
			`,
		},
		{
			name: "for_iterator_yieldable",
			src: `
				local co = coroutine.create(function()
					local function iter(s, i)
						i = i + 1
						if i > s then return nil end
						return i, coroutine.yield("step-" .. i)
					end
					local acc = {}
					for i, v in iter, 2, 0 do acc[i] = v end
					return table.concat(acc, ":")
				end)
				local ok1, v1 = coroutine.resume(co)
				local ok2, v2 = coroutine.resume(co, "A")
				local ok3, v3 = coroutine.resume(co, "B")
				print(ok1, v1, ok2, v2, ok3, v3)
			`,
		},
		{
			name: "metamethod_add_index_close_yieldable",
			src: `
				local obj = setmetatable({}, {
					__add = function() return coroutine.yield("add") end,
					__index = function() return coroutine.yield("idx") end,
					__close = function() coroutine.yield("cls") end,
				})
				local co = coroutine.create(function()
					local c <close> = obj
					local a = obj + 1
					local b = obj.foo
					return a .. b
				end)
				local ok1, v1 = coroutine.resume(co)
				local ok2, v2 = coroutine.resume(co, "X")
				local ok3, v3 = coroutine.resume(co, "Y")
				local ok4, v4 = coroutine.resume(co)
				print(ok1, v1, ok2, v2, ok3, v3, ok4, v4)
			`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cOut := runCLuaSnippet(t, cBin, tc.src)
			moeOut := runMoeLuaCapture(t, tc.src)
			if cOut != moeOut {
				t.Fatalf("yield differential mismatch vs C Lua 5.5.1:\n  C:      %q\n  MoeLua: %q", cOut, moeOut)
			}
		})
	}
}

// TestPhase2P7DeadThreadOpenUpvalue verifies PUC-Rio lgc.c:remarkupvals semantics:
// an unreachable suspended coroutine whose open upvalue is captured by a live
// closure is collected from weak tables, while the captured value remains alive
// and accessible through the closure (and is not prematurely finalized/weak-cleared).
func TestPhase2P7DeadThreadOpenUpvalue(t *testing.T) {
	runLuaSnippet(t, `
		local f
		local x_finalized = false
		local wx = setmetatable({}, {__mode = "v"})
		local co = coroutine.create(function()
			local x = setmetatable({ msg = "captured" }, {
				__gc = function() x_finalized = true end,
			})
			wx[1] = x
			f = function() return x end
			coroutine.yield()
		end)
		assert(coroutine.resume(co))
		local wco = setmetatable({ co }, { __mode = "v" })
		co = nil

		collectgarbage()
		assert(wco[1] == nil, "dead suspended coroutine with open upvalue must be cleared from weak table")
		assert(not x_finalized, "captured upvalue target must NOT be finalized while closure f is live")
		assert(wx[1] ~= nil and wx[1].msg == "captured", "captured upvalue target must stay live in weak table")
		assert(f().msg == "captured", "live closure must still read closed upvalue after coroutine collected")

		-- Now drop closure f: x must finally be collected and finalized!
		f = nil
		collectgarbage()
		assert(wx[1] == nil and x_finalized == true, "captured target must be collected once closure f dies")
	`)
}

// TestPhase2FrozenTemplateIsolationAndParallelRace verifies that mutating stdlib
// tables/shapes/metatables on one State never leaks to another State, and spinning
// up many States across parallel goroutines is 100% race-free under -race.
func TestPhase2FrozenTemplateIsolationAndParallelRace(t *testing.T) {
	// Mutate EVERY stdlib table in State 1 (including adding new keys to trigger frozen Shape transitions!).
	runLuaSnippet(t, `
		math.sin = function() return 999 end
		math.extra_key = "polluted"
		setmetatable(math, { __index = function() return "meta" end })
		table.insert = nil
		string.custom = 123
		getmetatable("").__index = {}
		package.loaded.math = nil
		package.path = "corrupted"
		_G.print = nil
	`)

	// Verify fresh State 2 is 100% pristine!
	runLuaSnippet(t, `
		assert(math.sin(0) == 0.0)
		assert(math.extra_key == nil)
		assert(getmetatable(math) == nil)
		assert(type(table.insert) == "function")
		assert(string.custom == nil)
		assert(("abc"):upper() == "ABC")
		assert(package.loaded.math == math)
		assert(package.path ~= "corrupted")
		assert(type(print) == "function")
	`)

	// Concurrent independent States sharing frozen template & compiled Proto under -race!
	p, err := compiler.Compile("=parallel_tmpl", `
		local longA = string.rep("A", 55)
		local longB = string.rep("B", 55)
		local t = { [longA] = 10, [longB] = 20 }
		math.extra = t[longA] + t[longB]
		local s = ("hello"):upper()
		return math.extra, s
	`)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for range 50 {
				s := vm.NewState()
				cl, err := s.Load(p)
				if err != nil {
					errs <- err
					s.Close()
					return
				}
				res, err := s.Call(vm.ClosureValue(cl))
				if err != nil {
					errs <- err
					s.Close()
					return
				}
				if len(res) != 2 || res[0].Int() != 30 || res[1].AsString().Str() != "HELLO" {
					errs <- fmt.Errorf("g=%d unexpected res: %v", id, res)
					s.Close()
					return
				}
				s.Close()
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func findCLua55(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "build", "lua55_o2")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("C Lua 5.5.1 binary %s not built (run `make tools`)", p)
	}
	return p
}

func runCLuaSnippet(t *testing.T, cBin, src string) string {
	t.Helper()
	cmd := exec.Command(cBin, "-e", src)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("C lua55_o2 failed: %v\nstderr: %s", err, errBuf.String())
	}
	return strings.TrimSpace(out.String())
}

func runMoeLuaCapture(t *testing.T, src string) string {
	t.Helper()
	s := vm.NewState()
	defer s.Close()
	var out bytes.Buffer
	s.SetOutput(&out)
	p, err := compiler.Compile("=diff", src)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	cl, err := s.Load(p)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if _, err := s.Call(vm.ClosureValue(cl)); err != nil {
		t.Fatalf("exec failed: %v", err)
	}
	return strings.TrimSpace(out.String())
}
