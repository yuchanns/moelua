package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
	_ "time/tzdata"
	"weak"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
)

func TestArrBoolLifecycleAndPromotion(t *testing.T) {
	s := NewState()
	defer s.Close()

	cl, err := s.LoadChunk([]byte(`
		-- Dense boolean array via table.create
		local b = table.create(100, 0)
		for i = 1, 100 do b[i] = (i % 2 == 0) end
		assert(#b == 100)
		for i = 1, 100 do assert(b[i] == (i % 2 == 0)) end

		-- Flip elements via OP_SETTABLE and OP_SETI
		b[1] = true
		b[2] = false
		assert(b[1] == true and b[2] == false and #b == 100)

		-- Iterate with ipairs and pairs (next)
		local cnt = 0
		for i, v in ipairs(b) do
			assert(type(v) == "boolean")
			cnt = cnt + 1
		end
		assert(cnt == 100)

		local pcnt = 0
		for k, v in pairs(b) do
			assert(type(k) == "number" and type(v) == "boolean")
			pcnt = pcnt + 1
		end
		assert(pcnt == 100)

		-- Trailing nil shrinks dense bool array cleanly without corrupting #b
		b[100] = nil
		assert(#b == 99 and b[100] == nil and b[99] == false)

		-- Grow beyond initial hint triggers bool slice growth
		b[100] = true
		for i = 101, 260 do b[i] = (i % 3 == 0) end
		assert(#b == 260 and b[260] == false and b[258] == true)

		-- Promotion to generic array when storing non-bool or interior nil
		b[50] = "promoted"
		assert(b[50] == "promoted" and b[49] == false and b[51] == false and #b == 260)

		-- Another bool array promoted via interior nil
		local b2 = { true, false, true, true }
		b2[2] = nil
		assert(b2[1] == true and b2[2] == nil and b2[3] == true and b2[4] == true)

		-- Tier 1 GC safety with bool arrays + weak tables
		local w = setmetatable({}, { __mode = "v" })
		local b3 = { true, false, true }
		w[1] = b3
		collectgarbage()
		assert(w[1] == b3 and b3[1] == true and b3[2] == false)
		return "ok"
	`), "=arrbool")
	if err != nil {
		t.Fatalf("LoadChunk: %v", err)
	}
	res, err := s.Call(cl)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(res) != 1 || res[0].AsString().S != "ok" {
		t.Fatalf("unexpected res: %v", res)
	}
}

func TestFastNativeFuncSemantics(t *testing.T) {
	s := NewState()
	defer s.Close()

	s.SetGlobal("fast_add", NativeValue(&NativeFunc{
		Name: "fast_add",
		FastFn: func(_ *State, args []Value) (Value, error) {
			if len(args) < 2 {
				return NilValue(), errors.New("need 2 args")
			}
			return IntValue(args[0].Int() + args[1].Int()), nil
		},
	}))

	cl, err := s.LoadChunk([]byte(`
		-- Fixed 1 result (c == 2)
		assert(fast_add(10, 32) == 42)
		-- Discarded result (c == 1)
		fast_add(1, 2)
		-- Extra results nil-padded (c == 4)
		local a, b, c = fast_add(3, 4)
		assert(a == 7 and b == nil and c == nil)
		-- MULTRET tail argument (c == 0) + vararg caller (b == 0)
		local function pair(x, y) return x, y end
		local x, y = pair(100, fast_add(5, 6))
		assert(x == 100 and y == 11)
		local s2 = fast_add(pair(20, 22))
		assert(s2 == 42)
		-- Tailcall to FastFn
		local function tail(u, v) return fast_add(u, v) end
		assert(tail(19, 23) == 42)
		-- Protected call (pcall) normal + error
		local ok1, r1 = pcall(fast_add, 8, 9)
		assert(ok1 and r1 == 17)
		local ok2, e2 = pcall(fast_add, 1)
		assert(not ok2 and tostring(e2):find("need 2 args"))
		-- Debug call/return hooks observe FastFn
		local events = {}
		debug.sethook(function(ev) events[#events+1] = ev end, "cr")
		local r3 = fast_add(2, 3)
		debug.sethook()
		assert(r3 == 5 and #events >= 2)
		-- Coroutine entry with FastFn
		local co = coroutine.create(fast_add)
		local cok, cres = coroutine.resume(co, 11, 22)
		assert(cok and cres == 33)
		return "ok"
	`), "=fastnative")
	if err != nil {
		t.Fatalf("LoadChunk: %v", err)
	}
	res, err := s.Call(cl)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(res) != 1 || res[0].AsString().S != "ok" {
		t.Fatalf("unexpected res: %v", res)
	}
}

func TestIntrinsicMultretAndMixedArith(t *testing.T) {
	s := NewState()
	defer s.Close()

	cl, err := s.LoadChunk([]byte(`
		-- Nested tail-argument intrinsics (OP_CALL with c == 0 feeding b == 0)
		assert(math.floor(math.abs(-3.7)) == 3)
		assert(math.ceil(math.abs(-3.2)) == 4)
		local function id(...) return ... end
		local a, b = id(math.abs(-9))
		assert(a == 9 and b == nil)
		assert(math.abs(math.mininteger) == math.mininteger)

		-- Mixed int/float OP_MULK and OP_ADD fast paths + metamethod fallbacks
		local i = 7
		assert(i * 0.5 == 3.5 and math.type(i * 0.5) == "float")
		assert(3.5 + i == 10.5 and math.type(3.5 + i) == "float")
		assert(i + 3.5 == 10.5 and math.type(i + 3.5) == "float")
		local mt = {
			__mul = function(x, y) return x.v * y end,
			__add = function(x, y) return x.v + y end,
		}
		local obj = setmetatable({ v = 10 }, mt)
		assert(obj * 0.5 == 5.0)
		assert(obj + 2 == 12)
		return "ok"
	`), "=intrmixed")
	if err != nil {
		t.Fatalf("LoadChunk: %v", err)
	}
	res, err := s.Call(cl)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(res) != 1 || res[0].AsString().S != "ok" {
		t.Fatalf("unexpected res: %v", res)
	}
}

func TestCoroutineStackShrinkWithOpenUpvalues(t *testing.T) {
	s := NewState()
	defer s.Close()

	cl, err := s.LoadChunk([]byte(`
		-- Activate Tier 1 GC via a dummy weak table so CollectFull traces and shrinks threads.
		local _ = setmetatable({}, { __mode = "v" })

		local getter, setter
		local function deep(n)
			local a, b, c, d, e, f, g, h = n, n+1, n+2, n+3, n+4, n+5, n+6, n+7
			if n > 0 then
				return deep(n - 1) + a - a + b - b + c - c + d - d + e - e + f - f + g - g + h - h
			end
			return 0
		end

		local co = coroutine.create(function()
			local captured = 123
			getter = function() return captured end
			setter = function(v) captured = v end
			-- Grow coroutine stack well beyond 2048 slots, then return to shallow depth and yield
			-- while keeping 'captured' as an OPEN upvalue on co's stack!
			deep(400)
			coroutine.yield("ready")
			return captured
		end)

		local ok, msg = coroutine.resume(co)
		assert(ok and msg == "ready")
		return co, getter, setter
	`), "=shrink_openuv")
	if err != nil {
		t.Fatalf("LoadChunk: %v", err)
	}
	res, err := s.Call(cl)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	co := res[0].AsThread()
	getter := res[1]
	setter := res[2]

	if co.openUpvals == nil {
		t.Fatalf("expected coroutine to have open upvalues before GC shrink")
	}
	beforeLen := len(co.stack)
	if beforeLen <= 2048 {
		t.Fatalf("expected grown stack > 2048 before GC, got %d", beforeLen)
	}

	// Anchor co/getter/setter as globals so Tier 1 GC keeps them alive while shrinking co.stack!
	s.SetGlobal("_co", res[0])
	s.SetGlobal("_getter", getter)
	s.SetGlobal("_setter", setter)
	s.CollectFull()

	afterLen := len(co.stack)
	if afterLen >= beforeLen/2 {
		t.Fatalf("expected coroutine stack with open upvalues to shrink significantly: before=%d after=%d", beforeLen, afterLen)
	}

	// Verify open upvalue reads & mutations still target co's live shrunk stack!
	check, err := s.LoadChunk([]byte(`
		assert(_getter() == 123)
		_setter(456)
		assert(_getter() == 456)
		local ok, finalVal = coroutine.resume(_co)
		assert(ok and finalVal == 456, tostring(finalVal))
		assert(_getter() == 456)
		return "shrunk-ok"
	`), "=shrink_verify")
	if err != nil {
		t.Fatalf("LoadChunk verify: %v", err)
	}
	out, err := s.Call(check)
	if err != nil {
		t.Fatalf("Call verify: %v", err)
	}
	if len(out) != 1 || out[0].AsString().S != "shrunk-ok" {
		t.Fatalf("unexpected verify res: %v", out)
	}
}

func TestTablePairsClearLastAndHashArrayMigration(t *testing.T) {
	s := NewState()
	defer s.Close()

	cl, err := s.LoadChunk([]byte(`
		-- Regression 1: clearing the last element of int, float, and bool arrays during pairs()
		for _, arr in ipairs({ {1, 2, 3}, {1.5, 2.5, 3.5}, {true, false, true} }) do
			local seen = 0
			for k, v in pairs(arr) do
				seen = seen + 1
				if k == 3 then arr[k] = nil end
			end
			assert(seen == 3 and #arr == 2 and arr[3] == nil)
		end

		-- Regression 2: stale hash entry must never resurrect when array grows to absorb key
		local t = {1}
		t[3] = 3
		t[2] = 2
		t[3] = 4
		t[3] = nil
		assert(t[3] == nil, "stale hash value resurrected: " .. tostring(t[3]))
		local keys = {}
		local guard = 0
		for k, v in pairs(t) do
			guard = guard + 1
			assert(guard < 20, "pairs() infinite loop on migrated hash/array key")
			keys[k] = v
		end
		assert(guard == 2 and keys[1] == 1 and keys[2] == 2 and keys[3] == nil)

		-- Regression 2b: gap growth migrates existing hash value into array rather than clobbering it
		local t2 = {}
		t2[20] = "twenty"
		for i = 1, 19 do t2[i] = i end
		assert(t2[20] == "twenty")
		t2[20] = nil
		assert(t2[20] == nil and #t2 == 19)
		local g2 = 0
		for _ in pairs(t2) do
			g2 = g2 + 1
			assert(g2 <= 25)
		end
		assert(g2 == 19)
		return "ok"
	`), "=tbl_audit")
	if err != nil {
		t.Fatalf("LoadChunk: %v", err)
	}
	res, err := s.Call(cl)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(res) != 1 || res[0].AsString().S != "ok" {
		t.Fatalf("unexpected res: %v", res)
	}
}

func TestCodexReviewVMRegressions(t *testing.T) {
	// 1. Reverse finalizer compaction: older object dead while newer stays live.
	t.Run("finalizer_reverse_compaction", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local fired = {}
			local mt = { __gc = function(o) fired[#fired+1] = o.id end }
			local a = setmetatable({ id = "a" }, mt)
			local b = setmetatable({ id = "b" }, mt)
			local c = setmetatable({ id = "c" }, mt)
			a = nil
			collectgarbage("collect")
			assert(#fired == 1 and fired[1] == "a", "expected only a to finalize, got " .. table.concat(fired, ","))
			b = nil
			c = nil
			collectgarbage("collect")
			assert(#fired == 3 and fired[2] == "c" and fired[3] == "b", "expected LIFO c then b, got " .. table.concat(fired, ","))
			return "ok"
		`), "=gc_reverse")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 2. Math builtins must return catchable Lua errors (never Go panics) on missing args.
	t.Run("math_missing_args_no_panic", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local cases = {
				function() return math.ult() end,
				function() return math.ult(1) end,
				function() return math.fmod() end,
				function() return math.fmod(1) end,
				function() return math.fmod("bad", 2) end,
				function() return math.frexp() end,
				function() return math.ldexp() end,
				function() return math.ldexp(1) end,
			}
			for i, fn in ipairs(cases) do
				local ok = pcall(fn)
				assert(not ok, "case " .. i .. " should have errored")
			end
			return "ok"
		`), "=math_args")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 3. Unbiased wide-range math.random(math.mininteger, 1 << 62).
	t.Run("math_random_unbiased_wide_range", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			math.randomseed(1, 2)
			local threshold = -(1 << 62)
			local low = 0
			local N = 20000
			for i = 1, N do
				local v = math.random(math.mininteger, 1 << 62)
				if v < threshold then low = low + 1 end
			end
			-- Exact probability is 1/3 (~6667/20000); biased % would give ~10000/20000.
			assert(low > 5500 and low < 7800, "unexpected low count: " .. low)
			return low
		`), "=rand_bias")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(cl); err != nil {
			t.Fatalf("Call failed: %v", err)
		}
	})

	// 4. Return hook error propagation across OP_RETURN0, OP_RETURN1, and native returns.
	t.Run("return_hook_error_propagation", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function f1() return 42 end
			local function f0() end
			debug.sethook(function()
				local info = debug.getinfo(2, "f")
				if info and (info.func == f1 or info.func == f0) then
					error("ret-hook-boom")
				end
			end, "r")
			local ok1, err1 = pcall(f1)
			local ok0, err0 = pcall(f0)
			debug.sethook()
			assert(not ok1 and tostring(err1):find("ret-hook-boom", 1, true), "OP_RETURN1 hook err lost: " .. tostring(err1))
			assert(not ok0 and tostring(err0):find("ret-hook-boom", 1, true), "OP_RETURN0 hook err lost: " .. tostring(err0))
			return "ok"
		`), "=rethook_err")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 5. coroutine.create(coroutine.yield) preserves resume args as return values.
	t.Run("coroutine_create_yield_entry", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local co = coroutine.create(coroutine.yield)
			local ok1, a1 = coroutine.resume(co, "first")
			assert(ok1 and a1 == "first")
			local ok2, b1, b2 = coroutine.resume(co, "second", 3)
			assert(ok2 and b1 == "second" and b2 == 3)
			assert(coroutine.status(co) == "dead")
			return "ok"
		`), "=coro_yield_entry")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 6. State.SetMemoryLimit enforced on io file reads (numeric, line, and read-all).
	t.Run("io_read_memory_limit", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "big.txt")
		if err := os.WriteFile(tmp, []byte(strings.Repeat("x", 512*1024)), 0o644); err != nil {
			t.Fatal(err)
		}
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(256 * 1024)
		s.SetGlobal("BIG_PATH", StringValue(s.Intern(tmp)))
		cl, err := s.LoadChunk([]byte(`
			local f = assert(io.open(BIG_PATH, "rb"))
			local ok1, e1 = pcall(function() return f:read(400000) end)
			assert(not ok1 and tostring(e1):find("not enough memory", 1, true), "numeric read bypassed memLimit: " .. tostring(e1))
			f:seek("set", 0)
			local ok2, e2 = pcall(function() return f:read("a") end)
			assert(not ok2 and tostring(e2):find("not enough memory", 1, true), "read('a') bypassed memLimit: " .. tostring(e2))
			f:seek("set", 0)
			local ok3, e3 = pcall(function() return f:read("l") end)
			assert(not ok3 and tostring(e3):find("not enough memory", 1, true), "read('l') bypassed memLimit: " .. tostring(e3))
			f:close()
			return "ok"
		`), "=io_memlimit")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 7. Short string interner reclaims unreachable short strings cleanly.
	t.Run("short_string_reclamation", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			for i = 1, 5000 do
				local tmp = "ephemeral_" .. tostring(i)
			end
		`), "=short_str_gc")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(cl); err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		for range 3 {
			runtime.GC()
		}
		live := s.intern.Prune()
		if live > 1000 {
			t.Fatalf("expected unreachable ephemeral short strings to be reclaimed, still have %d live", live)
		}
	})
}

func TestCodexReviewRound2VMRegressions(t *testing.T) {
	// 1. Thread stack growth & table.unpack honor State memory limit.
	t.Run("stack_growth_memlimit", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(65536)
		cl, err := s.LoadChunk([]byte(`
			local ok, err = pcall(function() return table.unpack({}, 1, 999999) end)
			assert(not ok and tostring(err):find("not enough memory", 1, true), "unpack bypassed memLimit: " .. tostring(err))
			-- Also test vararg stack growth directly.
			local function rec(n, ...)
				if n == 0 then return ... end
				return rec(n - 1, 1, 2, 3, 4, 5, 6, 7, 8, ...)
			end
			local ok2, err2 = pcall(rec, 2000)
			assert(not ok2 and (tostring(err2):find("not enough memory", 1, true) or tostring(err2):find("stack overflow", 1, true)), "rec bypassed memLimit: " .. tostring(err2))
			return "ok"
		`), "=stack_memlimit")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
		if used := s.MemoryUsage(); used > 131072 {
			t.Fatalf("MemoryUsage blew past limit: %d", used)
		}
	})

	// 2. string.gsub bounds output growth and accounts completed strings under memory limit.
	t.Run("string_gsub_memlimit", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(65536)
		cl, err := s.LoadChunk([]byte(`
			local x = string.rep("x", 1000)
			local ok1, e1 = pcall(string.gsub, x, ".", x)
			assert(not ok1 and tostring(e1):find("not enough memory", 1, true), "gsub string bypassed memLimit: " .. tostring(e1))
			local tbl = {x = x}
			local ok2, e2 = pcall(string.gsub, x, ".", tbl)
			assert(not ok2 and tostring(e2):find("not enough memory", 1, true), "gsub table bypassed memLimit: " .. tostring(e2))
			local ok3, e3 = pcall(string.gsub, x, ".", function() return x end)
			assert(not ok3 and tostring(e3):find("not enough memory", 1, true), "gsub func bypassed memLimit: " .. tostring(e3))
			return "ok"
		`), "=gsub_memlimit")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 3. string.byte validates arg count and types without panicking.
	t.Run("string_byte_arg_validation", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local ok1, e1 = pcall(string.byte)
			assert(not ok1 and tostring(e1):find("string expected", 1, true), tostring(e1))
			local ok2, e2 = pcall(string.byte, {})
			assert(not ok2 and tostring(e2):find("string expected", 1, true), tostring(e2))
			local ok3, e3 = pcall(string.byte, "abc", "bad")
			assert(not ok3 and tostring(e3):find("number expected", 1, true), tostring(e3))
			local ok4, e4 = pcall(string.byte, "abc", 1, 1.5)
			assert(not ok4 and tostring(e4):find("no integer representation", 1, true), tostring(e4))
			local a, b, c = string.byte(123, 1, 3)
			assert(a == 49 and b == 50 and c == 51)
			return "ok"
		`), "=string_byte_args")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 4. pcall and xpcall fire call hooks for protected Lua callees and catch call-hook errors.
	t.Run("pcall_xpcall_call_hook", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function f() return 42 end
			local calls, rets = 0, 0
			debug.sethook(function(ev)
				local info = debug.getinfo(2, "f")
				if info and info.func == f then
					if ev == "call" then calls = calls + 1
					elseif ev == "return" then rets = rets + 1 end
				end
			end, "cr")
			f()
			assert(pcall(f))
			assert(xpcall(f, function(e) return e end))
			debug.sethook()
			assert(calls == 3 and rets == 3, "expected 3 calls / 3 rets, got " .. calls .. "/" .. rets)

			-- Error thrown from call hook inside pcall must be caught by pcall.
			debug.sethook(function(ev)
				local info = debug.getinfo(2, "f")
				if ev == "call" and info and info.func == f then
					error("call_hook_boom")
				end
			end, "c")
			local ok, err = pcall(f)
			debug.sethook()
			assert(not ok and tostring(err):find("call_hook_boom", 1, true), tostring(err))
			return "ok"
		`), "=pcall_call_hook")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 5. ipairs reads raw table entries before consulting __index metatable.
	t.Run("ipairs_metatable_raw_first", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local t1 = setmetatable({10, 20}, {})
			local out1 = {}
			for i, v in ipairs(t1) do out1[#out1+1] = i .. "=" .. v end
			assert(table.concat(out1, ",") == "1=10,2=20", table.concat(out1, ","))

			local t2 = setmetatable({10, 20}, {__index = function(_, k)
				if k == 1 then return 999 end
				if k == 3 then return 30 end
			end})
			local out2 = {}
			for i, v in ipairs(t2) do out2[#out2+1] = i .. "=" .. v end
			assert(table.concat(out2, ",") == "1=10,2=20,3=30", table.concat(out2, ","))
			return "ok"
		`), "=ipairs_mt")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 6. GC clearing weak-value prototype entries invalidates icProtoVal inline caches.
	t.Run("weak_value_proto_ic_invalidation", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local p = setmetatable({}, {__mode = "v"})
			p.foo = {marker = 123}
			local x = setmetatable({}, {__index = p})
			local function get(o) return o.foo end
			for i = 1, 10 do
				assert(get(x).marker == 123)
			end
			collectgarbage("collect")
			assert(p.foo == nil, "p.foo should be collected")
			assert(get(x) == nil, "cached icProtoVal returned stale collected table")
			return "ok"
		`), "=weak_proto_ic")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound3VMRegressions(t *testing.T) {
	// 1. File methods, os.difftime, and package.searchers validate missing args without panicking.
	t.Run("file_os_searcher_arg_validation", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			for _, fn in ipairs({
				io.stdin.read, io.stdin.write, io.stdin.seek, io.stdin.lines,
				os.difftime,
				package.searchers[1], package.searchers[2], package.searchers[3],
			}) do
				local ok, e = pcall(fn)
				assert(not ok and type(e) == "string", "expected Lua error, got " .. tostring(ok) .. " " .. tostring(e))
			end
			local ok, e = pcall(os.difftime, 10)
			assert(not ok and tostring(e):find("number expected", 1, true), tostring(e))
			return "ok"
		`), "=file_os_searcher_args")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 2. table.move polls context cancellation periodically in huge loops.
	t.Run("table_move_cancellation", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		s.SetContext(ctx)
		cl, err := s.LoadChunk([]byte(`
			table.move({}, 1, 100000000, 1)
		`), "=table_move_cancel")
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err = s.Call(cl)
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded from table.move, got %v (elapsed=%v)", err, elapsed)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("table.move took too long to cancel: %v", elapsed)
		}
	})

	// 3. Cached metatable shapes (metaRoots & metaTrans) do not retain unreachable metatables.
	t.Run("weak_metatable_shapes_reclaimed", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		var w1, w2 weak.Pointer[Table]
		func() {
			mt1 := s.NewTable(0, 1)
			mt1.RawSetStr(s, s.intern.Intern("payload"), StringValue(s.intern.Intern("empty_tbl_mt")))
			w1 = weak.Make(mt1)
			t1 := s.NewTable(0, 0)
			s.SetMetatable(t1, mt1)

			mt2 := s.NewTable(0, 1)
			mt2.RawSetStr(s, s.intern.Intern("payload"), StringValue(s.intern.Intern("shaped_tbl_mt")))
			w2 = weak.Make(mt2)
			t2 := s.NewTable(0, 2)
			t2.RawSetStr(s, s.intern.Intern("x"), IntValue(1))
			s.SetMetatable(t2, mt2)
			_ = t1
			_ = t2
		}()
		for range 5 {
			runtime.GC()
		}
		s.shapes.Prune()
		if w1.Value() != nil {
			t.Fatalf("metatable retained by metaRoots after dropping table")
		}
		if w2.Value() != nil {
			t.Fatalf("metatable retained by metaTrans after dropping shaped table")
		}
	})

	// 4. Appending to specialized arrays (int/float/bool) via OP_SETTABLE honors __newindex.
	t.Run("array_append_honors_newindex", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			for _, tc in ipairs({
				{ name = "int", init = {1}, val = 9 },
				{ name = "float", init = {1.5}, val = 2.5 },
				{ name = "bool", init = {true}, val = false },
			}) do
				local called = 0
				local t = setmetatable(tc.init, {
					__newindex = function(self, k, v)
						called = called + 1
						error("blocked_" .. tc.name)
					end,
				})
				local i = 2
				local ok, err = pcall(function() t[i] = tc.val end)
				assert(not ok and called == 1 and tostring(err):find("blocked_" .. tc.name, 1, true),
					tc.name .. ": ok=" .. tostring(ok) .. " called=" .. called .. " err=" .. tostring(err))
				assert(rawget(t, 2) == nil, tc.name .. ": rawget(t,2) should stay nil")
			end
			return "ok"
		`), "=array_append_newindex")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 5. Re-assigning __gc metatable on userdata does not duplicate finalizer execution.
	t.Run("userdata_finalizer_dedup", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local count = 0
			local mt = { __gc = function() count = count + 1 end }
			do
				local f = assert(io.tmpfile())
				debug.setmetatable(f, mt)
				debug.setmetatable(f, mt)
			end
			collectgarbage("collect")
			assert(count == 1, "expected userdata __gc to run once, got " .. count)
			return "ok"
		`), "=ud_gc_dedup")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 6. string.format defers __tostring until %s actually consumes the argument.
	t.Run("string_format_lazy_tostring", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local calls = 0
			local t = setmetatable({}, {
				__tostring = function()
					calls = calls + 1
					error("tostring_boom")
				end,
			})
			assert(string.format("plain", t) == "plain")
			assert(calls == 0)
			local p = string.format("%p", t)
			assert(p:sub(1, 2) == "0x", p)
			assert(calls == 0)
			local ok, err = pcall(string.format, "%s", t)
			assert(not ok and tostring(err):find("tostring_boom", 1, true) and calls == 1)
			return "ok"
		`), "=format_lazy_tostring")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 7. Failed seek on a non-seekable buffered pipe preserves unread buffered bytes.
	if runtime.GOOS != "windows" {
		t.Run("file_seek_failure_preserves_buffer", func(t *testing.T) {
			s := NewState()
			defer s.Close()
			cl, err := s.LoadChunk([]byte(`
				local f = assert(io.popen("printf abcdef", "r"))
				assert(f:read(1) == "a")
				local pos, msg = f:seek()
				assert(pos == nil and type(msg) == "string")
				local rest = f:read("a")
				f:close()
				assert(rest == "bcdef", "expected 'bcdef', got " .. string.format("%q", tostring(rest)))
				return "ok"
			`), "=seek_preserve_buf")
			if err != nil {
				t.Fatal(err)
			}
			if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
				t.Fatalf("Call failed: res=%v err=%v", res, err)
			}
		})
	}
}

func TestCodexReviewRound4VMRegressions(t *testing.T) {
	// 1. file:setvbuf bounds buffer allocations against State memory limits.
	t.Run("setvbuf_memory_limit", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(256 * 1024)
		cl, err := s.LoadChunk([]byte(`
			local f = assert(io.tmpfile())
			local ok1, e1 = pcall(f.setvbuf, f, "full", 1073741824)
			assert(not ok1 and tostring(e1):find("not enough memory", 1, true), tostring(e1))
			local ok2, e2 = pcall(f.setvbuf, f, "line", 512 * 1024)
			assert(not ok2 and tostring(e2):find("not enough memory", 1, true), tostring(e2))
			assert(f:setvbuf("full", 1024))
			f:close()
			return "ok"
		`), "=setvbuf_mem")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 2. string.pack bounds output allocations against State memory limits.
	t.Run("string_pack_memory_limit", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(256 * 1024)
		cl, err := s.LoadChunk([]byte(`
			local ok1, e1 = pcall(string.pack, "c1073741824", "")
			assert(not ok1 and tostring(e1):find("not enough memory", 1, true), tostring(e1))
			local ok2, e2 = pcall(string.pack, "c524288", "")
			assert(not ok2 and tostring(e2):find("not enough memory", 1, true), tostring(e2))
			assert(string.pack("c4", "ab") == "ab\0\0")
			return "ok"
		`), "=pack_mem")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 3. Weak-key table tombstones clear nd.key so Go GC immediately reclaims collected keys.
	t.Run("weak_key_tombstone_reclaimed", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		wt := s.NewTable(0, 4)
		mt := s.NewTable(0, 1)
		mt.RawSetStr(s, s.intern.Intern("__mode"), StringValue(s.intern.Intern("k")))
		s.SetMetatable(wt, mt)
		s.SetGlobal("wt", TableValue(wt))

		var wk weak.Pointer[Table]
		func() {
			k := s.NewTable(0, 1)
			k.RawSetStr(s, s.intern.Intern("tag"), StringValue(s.intern.Intern("ephemeral")))
			wk = weak.Make(k)
			_ = wt.RawSet(s, TableValue(k), IntValue(42))
		}()

		s.CollectFull()
		for range 5 {
			runtime.GC()
		}
		if wk.Value() != nil {
			t.Fatalf("weak-key table tombstone retained collected key after CollectGarbage + runtime.GC")
		}
	})

	// 4. Ordinary shape transitions use weak pointers so transient shapes are Go-GC reclaimable.
	t.Run("weak_shape_transitions_reclaimed", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		var wRootChild, wNestedChild weak.Pointer[Shape]
		parent := s.NewTable(0, 2)
		parent.RawSetStr(s, s.intern.Intern("base"), IntValue(1))
		func() {
			for i := range 20 {
				tmp := s.NewTable(0, 2)
				k := s.intern.Intern(fmt.Sprintf("transient_root_%d", i))
				tmp.RawSetStr(s, k, IntValue(int64(i)))
				if i == 0 {
					wRootChild = weak.Make(tmp.shape)
				}
				tmp2 := s.NewTable(0, 2)
				tmp2.RawSetStr(s, s.intern.Intern("base"), IntValue(1))
				k2 := s.intern.Intern(fmt.Sprintf("transient_nested_%d", i))
				tmp2.RawSetStr(s, k2, IntValue(int64(i)))
				if i == 0 {
					wNestedChild = weak.Make(tmp2.shape)
				}
			}
		}()
		for range 5 {
			runtime.GC()
		}
		s.shapes.Prune()
		if wRootChild.Value() != nil {
			t.Fatalf("root shape retained transient child shape")
		}
		if wNestedChild.Value() != nil {
			t.Fatalf("non-root shape retained transient child shape")
		}
		_ = parent
	})

	// 5. next(t, k) matches PUC-Rio ltable.c:findindex/equalkey raw tag semantics:
	// integral float keys (normalized to integer on insert) raise "invalid key to 'next'",
	// while genuine integer keys and non-integral/out-of-int64 floats succeed.
	t.Run("next_integral_float_hash_key", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local t = { [-1] = "a", [-2] = "b", [1.5] = "half", [1e20] = "big" }
			assert(t[-1.0] == "a" and t[-2.0] == "b")
			local ok1, err1 = pcall(next, t, -1.0)
			assert(not ok1 and tostring(err1):find("invalid key to 'next'", 1, true), tostring(err1))
			local k1, v1 = next(t, -1)
			assert(k1 ~= nil or v1 == nil)
			local okf1 = pcall(next, t, 1.5)
			local okf2 = pcall(next, t, 1e20)
			assert(okf1 and okf2)
			local t2 = { [0] = "z", [1] = "one" }
			assert(t2[0.0] == "z" and t2[1.0] == "one")
			local ok2, err2 = pcall(next, t2, 0.0)
			local ok3, err3 = pcall(next, t2, 1.0)
			assert(not ok2 and tostring(err2):find("invalid key to 'next'", 1, true), tostring(err2))
			assert(not ok3 and tostring(err3):find("invalid key to 'next'", 1, true), tostring(err3))
			local nk2, nv2 = next(t2, 1)
			assert(nk2 == 0 and nv2 == "z", tostring(nk2) .. "," .. tostring(nv2))
			return "ok"
		`), "=next_float_key")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 6. debug.getlocal/setlocal guard MinInt64 overflow and restrict negative indices to PF_VAHID.
	t.Run("debug_local_mininteger_and_vatab", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function testMinInt(...)
				local n1, v1 = debug.getlocal(1, math.mininteger)
				assert(n1 == nil and v1 == nil)
				local n2 = debug.setlocal(1, math.mininteger, 99)
				assert(n2 == nil)
				local n3, v3 = debug.getlocal(1, -1)
				assert(n3 == "(vararg)" and v3 == 10)
				assert(debug.setlocal(1, -1, 77) == "(vararg)")
				local n4, v4 = debug.getlocal(1, -1)
				assert(n4 == "(vararg)" and v4 == 77)
			end
			testMinInt(10, 20)

			local function testVatabCaller()
				local sentinel = "untouched"
				-- Escaping vararg table compiles to PF_VATAB (matching PUC-Rio ldebug.c:findvararg):
				-- both getlocal(1,-1) and setlocal(1,-1,...) return nil and never touch caller locals.
				local function innerEscaped(...v)
					local t = v
					local gn, gv = debug.getlocal(1, -1)
					assert(gn == nil and gv == nil, "PF_VATAB getlocal(-1) must return nil")
					local sn = debug.setlocal(1, -1, "corrupted")
					assert(sn == nil, "PF_VATAB setlocal(-1) must return nil")
					assert(t[1] == "arg1" and t.n == 2)
				end
				innerEscaped("arg1", "arg2")
				assert(sentinel == "untouched", "caller local corrupted: " .. tostring(sentinel))

				-- Non-escaping named vararg compiles to PF_VAHID: negative indices inspect/mutate hidden varargs.
				local function innerHidden(...v)
					local gn, gv = debug.getlocal(1, -1)
					assert(gn == "(vararg)" and gv == "arg1")
					assert(debug.setlocal(1, -1, "updated") == "(vararg)")
					assert(v[1] == "updated" and v.n == 2)
				end
				innerHidden("arg1", "arg2")
			end
			testVatabCaller()
			return "ok"
		`), "=debug_vararg_guard")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 7. Yielding through native metamethods (coroutine.yield) and callable-table metamethods.
	t.Run("yieldable_native_and_callable_table_metamethods", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			-- Native coroutine.yield as __add, __lt, __concat, and __close
			local a = setmetatable({ v = 10 }, {
				__add = coroutine.yield,
				__lt = coroutine.yield,
				__concat = coroutine.yield,
				__close = coroutine.yield,
			})
			local co1 = coroutine.create(function()
				local x = a + 5
				local cmp = (a < a) and "lt" or "ge"
				local cat = a .. "tail"
				do
					local c <close> = a
				end
				return x, cmp, cat
			end)
			local ok, y1, y2 = coroutine.resume(co1)
			assert(ok and y1 == a and y2 == 5)
			local ok2, y3, y4 = coroutine.resume(co1, 15)
			assert(ok2 and y3 == a and y4 == a)
			local ok3, y5, y6 = coroutine.resume(co1, true)
			assert(ok3 and y5 == a and y6 == "tail")
			local ok4, y7 = coroutine.resume(co1, "joined")
			assert(ok4 and y7 == a)
			local ok5, r1, r2, r3 = coroutine.resume(co1)
			assert(ok5 and r1 == 15 and r2 == "lt" and r3 == "joined",
				string.format("%s %s %s %s", tostring(ok5), tostring(r1), tostring(r2), tostring(r3)))

			-- Callable-table metamethods yielding across __add, __concat, and __close
			local addCallable = setmetatable({}, {
				__call = function(self, lhs, rhs)
					return coroutine.yield("add", lhs.v + rhs)
				end,
			})
			local catCallable = setmetatable({}, {
				__call = function(self, lhs, rhs)
					return coroutine.yield("cat", tostring(lhs.v) .. rhs)
				end,
			})
			local closeCallable = setmetatable({}, {
				__call = function(self, obj)
					return coroutine.yield("close", obj.v)
				end,
			})
			local b = setmetatable({ v = 20 }, {
				__add = addCallable,
				__concat = catCallable,
				__close = closeCallable,
			})
			local co2 = coroutine.create(function()
				local s1 = b + 2
				local s2 = b .. "!"
				do
					local c <close> = b
				end
				return s1, s2
			end)
			local c1, k1, v1 = coroutine.resume(co2)
			assert(c1 and k1 == "add" and v1 == 22)
			local c2, k2, v2 = coroutine.resume(co2, 220)
			assert(c2 and k2 == "cat" and v2 == "20!")
			local c3, k3, v3 = coroutine.resume(co2, "20!!")
			assert(c3 and k3 == "close" and v3 == 20)
			local c4, out1, out2 = coroutine.resume(co2)
			assert(c4 and out1 == 220 and out2 == "20!!")
			return "ok"
		`), "=yieldable_mm")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 8. GC marks cf.closeErr on suspended coroutines while unwinding TBC __close.
	t.Run("gc_marks_closeErr_while_suspended", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local wv = setmetatable({}, { __mode = "v" })
			local co = coroutine.create(function()
				return pcall(function()
					local x <close> = setmetatable({}, {
						__close = function()
							coroutine.yield("closing")
						end,
					})
					local errObj = { msg = "alive" }
					wv.e = errObj
					error(errObj)
				end)
			end)
			local ok1, tag = coroutine.resume(co)
			assert(ok1 and tag == "closing")
			collectgarbage("collect")
			assert(wv.e ~= nil and wv.e.msg == "alive", "closeErr was prematurely collected")
			local ok2, pok, perr = coroutine.resume(co)
			assert(ok2 and not pok and type(perr) == "table" and perr.msg == "alive")
			return "ok"
		`), "=gc_closeerr")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 9. os.execute/io.popen inherit State stdio streams and pipe:close() returns exit status tuple.
	if runtime.GOOS != "windows" {
		t.Run("os_execute_and_io_popen_stdio_and_close_status", func(t *testing.T) {
			s := NewState()
			defer s.Close()
			var outBuf bytes.Buffer
			s.SetOutput(&outBuf)
			cl, err := s.LoadChunk([]byte(`
				local ok, why, code = os.execute("printf exec_ok")
				assert(ok == true and why == "exit" and code == 0,
					string.format("%s %s %s", tostring(ok), tostring(why), tostring(code)))

				local p1 = assert(io.popen("exit 7", "r"))
				local pok, pwhy, pcode = p1:close()
				assert(pok == nil and pwhy == "exit" and pcode == 7,
					string.format("%s %s %s", tostring(pok), tostring(pwhy), tostring(pcode)))

				local p2 = assert(io.popen("printf pipe_read", "r"))
				local data = p2:read("a")
				local pok2, pwhy2, pcode2 = p2:close()
				assert(data == "pipe_read" and pok2 == true and pwhy2 == "exit" and pcode2 == 0)

				local p3 = assert(io.popen("cat", "w"))
				assert(p3:write("|pipe_write"))
				local pok3, pwhy3, pcode3 = p3:close()
				assert(pok3 == true and pwhy3 == "exit" and pcode3 == 0)
				return "ok"
			`), "=exec_popen_status")
			if err != nil {
				t.Fatal(err)
			}
			if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
				t.Fatalf("Call failed: res=%v err=%v", res, err)
			}
			if got := outBuf.String(); got != "exec_ok|pipe_write" {
				t.Fatalf("expected captured stdout 'exec_ok|pipe_write', got %q", got)
			}
		})
	}
}

func TestCodexReviewRound5VMRegressions(t *testing.T) {
	// 1. load(reader) bounds cumulative input buffers against State memory limits.
	t.Run("load_reader_memory_limit", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(65536)
		var peakMem int64
		s.SetGlobal("recordMem", NativeValue(&NativeFunc{
			Name: "recordMem",
			Fn: func(s *State, _ []Value) ([]Value, error) {
				if u := s.MemoryUsage(); u > peakMem {
					peakMem = u
				}
				return nil, nil
			},
		}))
		cl, err := s.LoadChunk([]byte(`
			local payload = string.rep("a", 1024)
			local n = 0
			local fn, err = load(function()
				recordMem()
				n = n + 1
				if n == 1 then return "local s = \"" end
				if n <= 1024 then return payload end
				if n == 1025 then return "\"\nreturn s" end
				return nil
			end)
			assert(fn == nil and tostring(err):find("not enough memory", 1, true),
				"expected memory error, got fn=" .. tostring(fn) .. " err=" .. tostring(err))
			assert(n < 100, "reader ran too many times before hitting 64KiB limit: " .. n)
			return "ok"
		`), "=load_reader_mem")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
		if peakMem < 16384 {
			t.Fatalf("expected peak MemoryUsage during load(reader) to include compiler memory, got %d", peakMem)
		}
		if s.gc.tempBytes != 0 {
			t.Fatalf("expected tempBytes == 0 after load(reader) returns, got %d", s.gc.tempBytes)
		}
	})

	// 2. loadfile()/dofile() without filename share stdin's buffered reader with earlier io.read().
	t.Run("loadfile_stdin_preserves_buffered_bytes", func(t *testing.T) {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer pr.Close()
		if _, err := pw.WriteString("Xreturn 42\n"); err != nil {
			pw.Close()
			t.Fatal(err)
		}
		pw.Close()

		origStdin := os.Stdin
		os.Stdin = pr
		s := NewState()
		os.Stdin = origStdin
		defer s.Close()

		cl, err := s.LoadChunk([]byte(`
			local c = io.read(1)
			local f = assert(loadfile())
			return c, f()
		`), "=loadfile_stdin")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(cl)
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if len(res) != 2 || res[0].AsString().S != "X" || res[1].Int() != 42 {
			t.Fatalf("expected ('X', 42), got %v", res)
		}
	})

	// 3. table.unpack roots accumulated results on the thread stack across __index GC cycles.
	t.Run("table_unpack_roots_results_across_index_gc", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function runCase()
				local fin = 0
				local p = setmetatable({}, {
					__index = function(_, i)
						if i == 1 then
							return setmetatable({ v = 123 }, { __gc = function() fin = fin + 1 end })
						end
						collectgarbage("collect")
						return "second"
					end,
				})
				local a, b = table.unpack(p, 1, 2)
				assert(fin == 0, "first unpacked result was finalized prematurely: fin=" .. fin)
				assert(a.v == 123 and b == "second")
			end
			runCase()
			-- Also verify under active call hook (frameNative on stack)
			debug.sethook(function() end, "c")
			runCase()
			debug.sethook()
			return "ok"
		`), "=unpack_gc_root")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 4. os.time honors isdst (disambiguating autumn folds, normalizing mismatches, and round-tripping os.date("*t")).
	t.Run("os_time_isdst_disambiguation_and_roundtrip", func(t *testing.T) {
		ny, err := time.LoadLocation("America/New_York")
		if err != nil {
			t.Skipf("America/New_York tzdata unavailable: %v", err)
		}
		origLocal := time.Local
		time.Local = ny
		defer func() { time.Local = origLocal }()

		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local cases = {
				{ "fold_true",    {year=2024, month=11, day=3,  hour=1,  min=30, sec=0, isdst=true},  1730611800, true  },
				{ "fold_false",   {year=2024, month=11, day=3,  hour=1,  min=30, sec=0, isdst=false}, 1730615400, false },
				{ "fold_nil",     {year=2024, month=11, day=3,  hour=1,  min=30, sec=0},              1730611800, true  },
				{ "winter_true",  {year=2024, month=1,  day=15, hour=12, min=0,  sec=0, isdst=true},  1705334400, false },
				{ "winter_false", {year=2024, month=1,  day=15, hour=12, min=0,  sec=0, isdst=false}, 1705338000, false },
				{ "summer_true",  {year=2024, month=7,  day=15, hour=12, min=0,  sec=0, isdst=true},  1721059200, true  },
				{ "summer_false", {year=2024, month=7,  day=15, hour=12, min=0,  sec=0, isdst=false}, 1721062800, true  },
				{ "gap_true",     {year=2024, month=3,  day=10, hour=2,  min=30, sec=0, isdst=true},  1710052200, false },
				{ "gap_false",    {year=2024, month=3,  day=10, hour=2,  min=30, sec=0, isdst=false}, 1710055800, true  },
			}
			for _, tc in ipairs(cases) do
				local name, tbl, wantTS, wantDST = tc[1], tc[2], tc[3], tc[4]
				local gotTS = os.time(tbl)
				assert(gotTS == wantTS and tbl.isdst == wantDST,
					string.format("%s: got ts=%s isdst=%s, want ts=%s isdst=%s",
						name, tostring(gotTS), tostring(tbl.isdst), tostring(wantTS), tostring(wantDST)))
			end
			-- Round-trip both occurrences of the autumn DST fold via os.date("*t", ts)
			for _, ts in ipairs({1730611800, 1730615400}) do
				local d = os.date("*t", ts)
				assert(os.time(d) == ts, string.format("roundtrip failed for %d: got %s", ts, tostring(os.time(d))))
			end
			return "ok"
		`), "=os_time_isdst")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound6VMRegressions(t *testing.T) {
	// 1. Calling string arithmetic metamethods (including __unm) with no arguments raises a Lua error, not a Go panic.
	t.Run("string_unm_missing_arg_no_panic", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local mt = getmetatable("")
			for _, mm in ipairs({"__unm", "__add", "__sub", "__mul", "__div", "__idiv", "__mod", "__pow"}) do
				local ok, e = pcall(mt[mm])
				assert(not ok and type(e) == "string", mm .. " expected Lua error, got " .. tostring(ok) .. " " .. tostring(e))
			end
			assert(mt.__unm("42") == -42)
			return "ok"
		`), "=str_unm_guard")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 2. table.sort roots buffered elements across __index, comparator, and __newindex GC cycles.
	t.Run("table_sort_roots_elements_across_callbacks", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local fin = 0
			local written = {}
			local proxy = setmetatable({}, {
				__len = function() return 3 end,
				__index = function(_, i)
					if i == 1 then
						return setmetatable({ v = 30 }, { __gc = function() fin = fin + 1 end })
					elseif i == 2 then
						collectgarbage("collect")
						return setmetatable({ v = 10 }, { __gc = function() fin = fin + 1 end })
					else
						collectgarbage("collect")
						return setmetatable({ v = 20 }, { __gc = function() fin = fin + 1 end })
					end
				end,
				__newindex = function(_, i, v)
					collectgarbage("collect")
					written[i] = v
				end,
			})
			table.sort(proxy, function(a, b)
				collectgarbage("collect")
				return a.v < b.v
			end)
			assert(fin == 0, "buffered sort elements were finalized prematurely: fin=" .. fin)
			assert(#written == 3 and written[1].v == 10 and written[2].v == 20 and written[3].v == 30)
			return "ok"
		`), "=sort_gc_root")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 3. Optimized ipairs fast path nil-fills extra loop variables beyond (i, v) on every iteration.
	t.Run("ipairs_fast_path_nil_fills_extra_vars", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local seen = {}
			for i, v, w, x in ipairs({10, 20, 30}) do
				assert(w == nil, "iter " .. i .. ": expected w == nil, got " .. tostring(w))
				assert(x == nil, "iter " .. i .. ": expected x == nil, got " .. tostring(x))
				seen[i] = v
				w = 99
				x = 88
			end
			assert(#seen == 3 and seen[1] == 10 and seen[2] == 20 and seen[3] == 30)
			return "ok"
		`), "=ipairs_extra_nil")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 4. Discarded file handles are reclaimed automatically by Go GC + runtime.AddCleanup in Tier 0.
	t.Run("discarded_file_handles_reclaimed_in_tier0", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			for i = 1, 200 do
				assert(io.tmpfile())
			end
			return "ok"
		`), "=tier0_file_gc")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
		if s.gc.tier1 {
			t.Fatalf("opening standard files must not activate Tier 1 GC")
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			runtime.GC()
			s.compactOpenFiles()
			if len(s.openFiles) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("expected discarded tmpfiles to be closed automatically by Go GC, still open: %d", len(s.openFiles))
			}
			time.Sleep(10 * time.Millisecond)
		}
		s.compactWeakFiles()
		if len(s.gc.weakFiles) != 0 {
			t.Fatalf("expected weakFiles compacted to 0, got %d", len(s.gc.weakFiles))
		}
	})
}

func TestCodexReviewRound7VMRegressions(t *testing.T) {
	// 1. Promoting specialized arrays (arrBool/arrInt/arrFloat -> arrGeneric) enforces State memory limits.
	t.Run("specialized_array_promotion_memlimit", func(t *testing.T) {
		if !OptimizeEnabled {
			t.Skip("specialized arrays are only active when OptimizeEnabled")
		}
		for _, tc := range []struct {
			name  string
			count int
			val   string
		}{
			{"bool", 10000, "true"},
			{"int", 3500, "1"},
			{"float", 3500, "1.5"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := NewState()
				defer s.Close()
				s.SetMemoryLimit(65536)
				cl, err := s.LoadChunk([]byte(fmt.Sprintf(`
					local t = {}
					for i = 1, %d do t[i] = %s end
					local ok, err = pcall(function() t[1] = nil end)
					assert(not ok and tostring(err):find("not enough memory", 1, true),
						"expected memory error promoting %s array, got ok=" .. tostring(ok) .. " err=" .. tostring(err))
					return "ok"
				`, tc.count, tc.val, tc.name)), "=promo_mem_"+tc.name)
				if err != nil {
					t.Fatal(err)
				}
				if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
					t.Fatalf("Call failed: res=%v err=%v", res, err)
				}
			})
		}
	})

	// 2. table.remove roots the removed value across subsequent __index/__newindex GC cycles.
	t.Run("table_remove_roots_removed_value", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local fin = 0
			local store = {}
			local proxy = setmetatable({}, {
				__len = function() return 2 end,
				__index = function(_, i)
					if i == 1 then
						return setmetatable({ tag = "kept" }, { __gc = function() fin = fin + 1 end })
					end
					collectgarbage("collect")
					return "second"
				end,
				__newindex = function(_, i, v)
					collectgarbage("collect")
					store[i] = v
				end,
			})
			local removed = table.remove(proxy, 1)
			assert(fin == 0, "removed value was finalized prematurely: fin=" .. fin)
			assert(removed.tag == "kept" and store[1] == "second" and store[2] == nil)
			return "ok"
		`), "=remove_gc_root")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 3. Numeric file:read(n) bounds huge counts unconditionally without panicking Go's makeslice.
	t.Run("file_read_maxinteger_no_panic", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local f = assert(io.tmpfile())
			local ok, e = pcall(function() return f:read(math.maxinteger) end)
			assert(not ok and tostring(e):find("too large", 1, true), tostring(e))
			assert(f:write("hello"))
			assert(f:seek("set", 0))
			assert(f:read(1 << 28) == "hello")
			assert(f:read(1 << 28) == nil)
			f:close()
			return "ok"
		`), "=read_maxint_default")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}

		s2 := NewState()
		defer s2.Close()
		s2.SetMemoryLimit(65536)
		cl2, err := s2.LoadChunk([]byte(`
			local f = assert(io.tmpfile())
			local ok, e = pcall(function() return f:read(math.maxinteger) end)
			assert(not ok and tostring(e):find("not enough memory", 1, true), tostring(e))
			f:close()
			return "ok"
		`), "=read_maxint_memlimit")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s2.Call(cl2); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 4. Native call hooks that grow th.stack and mutate arguments via debug.setlocal take effect.
	t.Run("native_call_hook_refreshes_args_after_stack_growth", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function grow(n)
				if n <= 0 then return 0 end
				local a, b, c, d = n, n+1, n+2, n+3
				return grow(n - 1) + a - a + b - b + c - c + d - d
			end
			debug.sethook(function()
				if debug.getinfo(2, "f").func == math.abs then
					grow(120)
					debug.setlocal(2, 1, -99)
					local _, v = debug.getlocal(2, 1)
					assert(v == -99)
				end
			end, "c")
			local r = math.abs(-1)
			debug.sethook()
			assert(r == 99, "expected math.abs to observe modified arg -99 -> 99, got " .. tostring(r))
			return "ok"
		`), "=hook_native_args")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 5. xpcall on callable-table targets reuses recursive message-handler dispatch.
	t.Run("xpcall_callable_table_reuses_recursive_msgh", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local callable = setmetatable({}, {
				__call = function() error("boom") end,
			})
			local calls = 0
			local ok, msg = xpcall(callable, function(err)
				calls = calls + 1
				if calls == 1 then
					error("retry")
				end
				return "handled:" .. tostring(err):match("retry$")
			end)
			assert(not ok and calls == 2 and msg == "handled:retry",
				string.format("ok=%s calls=%d msg=%s", tostring(ok), calls, tostring(msg)))
			return "ok"
		`), "=xpcall_callable_msgh")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound8VMRegressions(t *testing.T) {
	// 1. table.create accounts for actual power-of-two hash backing capacity before allocating.
	t.Run("table_create_actual_hash_capacity_memlimit", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(65536)
		cl, err := s.LoadChunk([]byte(`
			local ok, err = pcall(table.create, 0, 1000)
			assert(not ok and tostring(err):find("not enough memory", 1, true),
				"expected memory error for table.create(0, 1000), got ok=" .. tostring(ok) .. " err=" .. tostring(err))
			return "ok"
		`), "=table_create_hash_mem")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}

		// Also verify huge nrec hits hardSingleAllocLimit even without memLimit.
		s2 := NewState()
		defer s2.Close()
		cl2, err := s2.LoadChunk([]byte(`
			local ok, err = pcall(table.create, 0, 50000000)
			assert(not ok and tostring(err):find("not enough memory", 1, true), tostring(err))
			return "ok"
		`), "=table_create_hard_cap")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s2.Call(cl2); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 2. Native return hooks advance th.top past all return values so hook frames do not clobber results.
	t.Run("native_return_hook_preserves_multi_returns", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local seen = 0
			debug.sethook(function() seen = seen + 1 end, "r")
			local a, b, c, d, e = table.unpack({10, 20, 30, 40, 50})
			debug.sethook()
			assert(seen >= 1)
			assert(a == 10 and b == 20 and c == 30 and d == 40 and e == 50,
				string.format("got %s %s %s %s %s", tostring(a), tostring(b), tostring(c), tostring(d), tostring(e)))
			return "ok"
		`), "=ret_hook_multi")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 3. pcall and xpcall on callable tables (including chained __call) remain yieldable.
	t.Run("yieldable_pcall_and_xpcall_callable_table", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local inner = setmetatable({ tag = "in" }, {
				__call = function(self, outer, x)
					local v = coroutine.yield("pause", self.tag, outer.tag, x)
					return v * 2
				end,
			})
			local outer = setmetatable({ tag = "out" }, { __call = inner })

			local co1 = coroutine.create(function()
				return pcall(outer, 7)
			end)
			local ok1, y1, y2, y3, y4 = coroutine.resume(co1)
			assert(ok1 and y1 == "pause" and y2 == "in" and y3 == "out" and y4 == 7,
				string.format("%s %s %s %s %s", tostring(ok1), tostring(y1), tostring(y2), tostring(y3), tostring(y4)))
			local ok2, pok, res = coroutine.resume(co1, 21)
			assert(ok2 and pok and res == 42)

			local co2 = coroutine.create(function()
				return xpcall(outer, function(e) return "handled:" .. tostring(e) end, 9)
			end)
			local x1, xy1, xy2, xy3, xy4 = coroutine.resume(co2)
			assert(x1 and xy1 == "pause" and xy2 == "in" and xy3 == "out" and xy4 == 9)
			local x2, xok, xres = coroutine.resume(co2, 11)
			assert(x2 and xok and xres == 22)
			return "ok"
		`), "=yield_callable_pcall")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 4. require roots searcher loader data (extra) across loader execution so GC does not finalize it early.
	t.Run("require_roots_searcher_loader_data", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local fin = 0
			table.insert(package.searchers, 1, function(name)
				if name == "rooted_extra_mod" then
					local extra = setmetatable({ id = 77 }, { __gc = function() fin = fin + 1 end })
					return function(modname)
						collectgarbage("collect")
						return { loaded = modname }
					end, extra
				end
			end)
			local mod, extra = require("rooted_extra_mod")
			assert(fin == 0, "loader data was finalized prematurely: fin=" .. fin)
			assert(mod.loaded == "rooted_extra_mod" and extra.id == 77)
			return "ok"
		`), "=require_root_extra")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 5. string.pack rejects unsupported argument types when consumed without rejecting unused trailing args.
	t.Run("string_pack_rejects_invalid_types_only_when_consumed", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			for _, tc in ipairs({
				{"z", false},
				{"s", {}},
				{"c4", nil},
				{"i4", true},
				{"d", {}},
			}) do
				local ok, e = pcall(string.pack, tc[1], tc[2])
				assert(not ok and type(e) == "string",
					"expected pack error for fmt=" .. tc[1] .. " val=" .. tostring(tc[2]))
			end
			-- Unused trailing invalid arguments must be ignored
			assert(string.pack("b", 42, false, {}, nil) == string.char(42))
			return "ok"
		`), "=pack_invalid_types")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 6. table.move uses metamethod-aware equality (__eq) when deciding copy direction and propagates errors.
	t.Run("table_move_honors_eq_metamethod", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local backing = {1, 2, 3}
			local mt = {
				__index = backing,
				__newindex = backing,
				__eq = function() return true end,
			}
			local src = setmetatable({}, mt)
			local dst = setmetatable({}, mt)
			table.move(src, 1, 3, 2, dst)
			assert(backing[1] == 1 and backing[2] == 1 and backing[3] == 2 and backing[4] == 3,
				string.format("got {%s,%s,%s,%s}", tostring(backing[1]), tostring(backing[2]), tostring(backing[3]), tostring(backing[4])))

			-- Error raised by __eq propagates cleanly
			local errMT = {
				__index = backing,
				__newindex = backing,
				__eq = function() error("eq_boom") end,
			}
			local ok, e = pcall(table.move, setmetatable({}, errMT), 1, 3, 2, setmetatable({}, errMT))
			assert(not ok and tostring(e):find("eq_boom", 1, true), tostring(e))
			return "ok"
		`), "=move_eq_mm")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound9VMRegressions(t *testing.T) {
	// 1. Demoting shaped tables to dict mode enforces State memory limits before initHash.
	t.Run("shape_demotion_memlimit", func(t *testing.T) {
		if !OptimizeEnabled {
			t.Skip("shape demotion applies only when OptimizeEnabled")
		}
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(196608)
		cl, err := s.LoadChunk([]byte(`
			local keys = {}
			for i = 1, 64 do keys[i] = "k" .. i end
			local kept = {}
			for i = 1, 32 do
				local t = {}
				for j = 1, 64 do t[keys[j]] = j end
				kept[i] = t
			end
			local ok, err = pcall(function()
				for i = 1, #kept do
					kept[i][false] = 1
				end
			end)
			kept = nil
			collectgarbage("collect")
			assert(not ok and tostring(err):find("not enough memory", 1, true),
				"expected memory error on shape demotion, got ok=" .. tostring(ok) .. " err=" .. tostring(err))
			return "ok"
		`), "=shape_demote_mem")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 2. GC recounts retained array backing capacity (arrCap) rather than logical length.
	t.Run("gc_recounts_array_backing_capacity", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(65536)
		cl, err := s.LoadChunk([]byte(`
			local kept = {}
			local ok, err = pcall(function()
				for i = 1, 100 do
					local t = table.create(2000)
					t[1] = true
					kept[i] = t
					collectgarbage("collect")
				end
			end)
			kept = nil
			collectgarbage("collect")
			assert(not ok and tostring(err):find("not enough memory", 1, true),
				"expected memory error retaining preallocated arrays, got ok=" .. tostring(ok) .. " err=" .. tostring(err))
			return "ok"
		`), "=gc_recount_cap")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 3. Synchronous closeAllSync roots replacement errors across subsequent __close callbacks.
	t.Run("closeAllSync_roots_replacement_error", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local fin = 0
			local co = coroutine.create(function()
				local outer <close> = setmetatable({}, {
					__close = function()
						collectgarbage("collect")
					end,
				})
				local inner <close> = setmetatable({}, {
					__close = function()
						error(setmetatable({ tag = "from_inner" }, { __gc = function() fin = fin + 1 end }))
					end,
				})
				coroutine.yield("ready")
			end)
			local ok1, tag = coroutine.resume(co)
			assert(ok1 and tag == "ready")
			local cok, cerr = coroutine.close(co)
			assert(not cok and type(cerr) == "table" and cerr.tag == "from_inner")
			assert(fin == 0, "replacement close error was finalized prematurely: fin=" .. fin)
			return "ok"
		`), "=sync_close_root")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 4. Standard-library numeric arguments coerce numeric strings while keeping strict contexts distinct.
	t.Run("stdlib_coerces_numeric_strings", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			assert(math.sqrt("9") == 3.0)
			assert(math.floor("3.5") == 3 and math.type(math.floor("3.5")) == "integer")
			assert(math.abs("-5") == 5.0 and math.type(math.abs("-5")) == "float")
			assert(math.tointeger("3") == 3 and math.tointeger("3.0") == 3 and math.tointeger("3.5") == nil)
			assert(math.type("3") == nil)
			assert(string.rep("x", "3") == "xxx")
			assert(string.sub("abcd", "2", "3") == "bc")
			assert(string.byte("abc", "2") == 98)
			assert(string.char("65") == "A")
			assert(table.concat({"a", "b", "c"}, "-", "2", "3") == "b-c")
			assert(os.difftime("10", "3") == 7.0)
			assert(utf8.char("65") == "A")
			local f = assert(io.tmpfile())
			f:write("abcdef"); f:seek("set", 0)
			local ok = pcall(f.read, f, "4")
			assert(not ok, "f:read('4') must stay a string format error")
			f:close()
			return "ok"
		`), "=stdlib_num_coerce")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 5. os.clock returns process CPU time rather than elapsed wall-clock sleep time.
	t.Run("os_clock_measures_cpu_not_wall_sleep", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetGlobal("hostSleep", NativeValue(&NativeFunc{
			Name: "hostSleep",
			Fn: func(_ *State, _ []Value) ([]Value, error) {
				time.Sleep(150 * time.Millisecond)
				return nil, nil
			},
		}))
		cl, err := s.LoadChunk([]byte(`
			local t0 = os.clock()
			hostSleep()
			local dt = os.clock() - t0
			assert(dt >= 0 and dt < 0.08, "os.clock counted wall-clock sleep time: dt=" .. tostring(dt))
			return "ok"
		`), "=os_clock_cpu")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 6. I/O failure helpers preserve the underlying OS errno (e.g. ENOENT).
	t.Run("io_fail_preserves_os_errno", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		missing := filepath.Join(t.TempDir(), "does-not-exist", "file.txt")
		cl, err := s.LoadChunk([]byte(fmt.Sprintf(`
			local f, msg, code = io.open(%q, "r")
			assert(f == nil and type(msg) == "string" and type(code) == "number")
			return code
		`, missing)), "=io_errno")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(cl)
		if err != nil || len(res) != 1 {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
		got := res[0].Int()
		if runtime.GOOS != "windows" {
			if got != int64(syscall.ENOENT) {
				t.Fatalf("expected ENOENT (%d), got %d", int64(syscall.ENOENT), got)
			}
		} else if got <= 1 {
			t.Fatalf("expected Windows OS error code > 1 for missing path, got %d", got)
		}
	})

	// 7. Native tail calls dispatch call/return debug hooks and support debug.setlocal on arguments.
	t.Run("native_tailcall_dispatches_debug_hooks", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local events = {}
			debug.sethook(function(ev)
				local info = debug.getinfo(2, "f")
				if info and info.func == math.abs then
					events[#events + 1] = ev
					if ev == "call" then
						debug.setlocal(2, 1, -99)
					end
				end
			end, "cr")
			local function g() return math.abs(-42) end
			local r = g()
			debug.sethook()
			assert(r == 99, "expected 99 after tailcall hook setlocal, got " .. tostring(r))
			assert(table.concat(events, ",") == "call,return", "expected 'call,return', got " .. table.concat(events, ","))
			return "ok"
		`), "=tailcall_native_hook")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound10VMRegressions(t *testing.T) {
	// 1. Compiled chunk/Proto storage is checked on Load and retained in GC recounting.
	t.Run("compiled_chunk_storage_memlimit", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local src = string.rep("x=x+1\n", 1000)
			local kept = {}
			local failed = false
			local errMsg = ""
			for i = 1, 100 do
				local fn, err = load(src)
				if not fn then
					failed = true
					errMsg = tostring(err)
					break
				end
				kept[i] = fn
				collectgarbage("collect")
			end
			local count = #kept
			kept = nil
			src = nil
			collectgarbage("collect")
			assert(failed and string.find(errMsg, "not enough memory", 1, true),
				"expected OOM loading repeated compiled chunks, got failed=" .. tostring(failed) .. " count=" .. count .. " err=" .. errMsg)
			local okFn = assert(load("return 42"))
			assert(okFn() == 42)
			return "ok"
		`), "=chunk_memlimit")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(65536)
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 2. Live file setvbuf buffers are included in GC memory recounting.
	t.Run("file_setvbuf_recounted_by_gc", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local kept = {}
			local failed = false
			local errMsg = ""
			for i = 1, 100 do
				local okOpen, f, ferr = pcall(io.tmpfile)
				if not okOpen or not f then
					failed = true
					errMsg = tostring(not okOpen and f or ferr)
					break
				end
				local ok, err = pcall(f.setvbuf, f, "full", 32768)
				if not ok then
					f:close()
					failed = true
					errMsg = tostring(err)
					break
				end
				kept[i] = f
				collectgarbage("collect")
			end
			local count = #kept
			for i = 1, count do
				kept[i]:close()
			end
			kept = nil
			collectgarbage("collect")
			assert(failed and count >= 1 and count < 4 and string.find(errMsg, "not enough memory", 1, true),
				"expected OOM retaining 32KiB file buffers, got failed=" .. tostring(failed) .. " count=" .. count .. " err=" .. errMsg)
			return "ok"
		`), "=file_buf_memlimit")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(98304)
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 3. Immediate comparisons resolve metamethods from both operands in logical order (luaT_callorderiTM).
	t.Run("immediate_comparisons_both_operands_and_precedence", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local calls = {}
			local plain = {}
			debug.setmetatable(0, {
				__lt = function(a, b)
					calls[#calls + 1] = "num_lt(" .. type(a) .. "," .. type(b) .. ")"
					return true
				end,
				__le = function(a, b)
					calls[#calls + 1] = "num_le(" .. type(a) .. "," .. type(b) .. ")"
					return true
				end,
			})
			assert((plain < 1) == true)
			assert((plain <= 1) == true)
			assert((plain > 1) == true)
			assert((plain >= 1) == true)
			assert((plain < 1.0) == true)
			assert((plain >= 1.0) == true)
			local expected1 = "num_lt(table,number);num_le(table,number);num_lt(number,table);num_le(number,table);num_lt(table,number);num_le(number,table)"
			assert(table.concat(calls, ";") == expected1, "unexpected plain calls: " .. table.concat(calls, ";"))

			-- Operand-order precedence when both operands have metamethods:
			calls = {}
			local both = setmetatable({}, {
				__lt = function(a, b)
					calls[#calls + 1] = "tbl_lt(" .. type(a) .. "," .. type(b) .. ")"
					return false
				end,
				__le = function(a, b)
					calls[#calls + 1] = "tbl_le(" .. type(a) .. "," .. type(b) .. ")"
					return false
				end,
			})
			assert((both < 1) == false)
			assert((both <= 1) == false)
			assert((both > 1) == true)
			assert((both >= 1) == true)
			local n = 1
			assert((both < n) == false)
			assert((both <= n) == false)
			assert((both > n) == true)
			assert((both >= n) == true)
			local expected2 = "tbl_lt(table,number);tbl_le(table,number);num_lt(number,table);num_le(number,table);tbl_lt(table,number);tbl_le(table,number);num_lt(number,table);num_le(number,table)"
			assert(table.concat(calls, ";") == expected2, "unexpected precedence calls: " .. table.concat(calls, ";"))

			-- Lua 5.5 does not fall back from __le to __lt.
			debug.setmetatable(0, nil)
			local onlyLt = setmetatable({}, { __lt = function() return false end })
			assert(not pcall(function() return onlyLt <= 1 end))
			assert(not pcall(function() return onlyLt <= n end))
			return "ok"
		`), "=imm_cmp_mm")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 4. os.tmpname keeps the reserved file on POSIX systems until removed by caller.
	t.Run("os_tmpname_reserves_posix_file", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local name = os.tmpname()
			assert(type(name) == "string" and #name > 0)
			return name
		`), "=tmpname_reserve")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(cl)
		if err != nil || len(res) != 1 {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
		name := res[0].AsString().S
		defer os.Remove(name)
		if runtime.GOOS != "windows" {
			if _, err := os.Stat(name); err != nil {
				t.Fatalf("expected os.tmpname path %q to remain reserved on POSIX: %v", name, err)
			}
		}
	})

	// 5. file:write failures return the 4th result containing total written bytes.
	t.Run("file_write_failure_returns_byte_count", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		roPath := filepath.Join(t.TempDir(), "readonly.txt")
		if err := os.WriteFile(roPath, []byte("hello"), 0o600); err != nil {
			t.Fatal(err)
		}
		cl, err := s.LoadChunk([]byte(fmt.Sprintf(`
			local function checkFail4(r1, r2, r3, r4, n, expectBytes, label)
				assert(n == 4, label .. ": expected 4 return values, got " .. tostring(n))
				assert(r1 == nil and type(r2) == "string" and type(r3) == "number", label .. ": bad ioFail prefix")
				assert(r4 == expectBytes, label .. ": expected written=" .. expectBytes .. ", got " .. tostring(r4))
			end
			-- Unbuffered read-only file:
			local ro = assert(io.open(%q, "r"))
			local a, b, c, d = ro:write("abc", "def")
			checkFail4(a, b, c, d, select("#", ro:write("abc", "def")), 0, "ro_unbuf")
			-- Line-buffered flush error:
			assert(ro:setvbuf("line", 16))
			a, b, c, d = ro:write("abc\n")
			checkFail4(a, b, c, d, select("#", ro:write("abc\n")), 0, "ro_line")
			-- Full-buffered overflow error:
			assert(ro:setvbuf("full", 8))
			a, b, c, d = ro:write("1234", "567890123456")
			checkFail4(a, b, c, d, select("#", ro:write("1234", "567890123456")), 0, "ro_full")
			ro:close()

			-- Also verify /dev/full when available (e.g. Linux):
			local full = io.open("/dev/full", "w")
			if full then
				assert(full:setvbuf("no"))
				a, b, c, d = full:write("abc", "def")
				checkFail4(a, b, c, d, select("#", full:write("abc", "def")), 0, "devfull_no")
				assert(full:setvbuf("line", 16))
				a, b, c, d = full:write("abc\n")
				checkFail4(a, b, c, d, select("#", full:write("abc\n")), 0, "devfull_line")
				assert(full:setvbuf("full", 8))
				a, b, c, d = full:write("1234", "567890123456")
				checkFail4(a, b, c, d, select("#", full:write("1234", "567890123456")), 0, "devfull_full")
				full:close()
			end
			return "ok"
		`, roPath)), "=file_write_err_count")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound11VMRegressions(t *testing.T) {
	// 1. Thread call-frame backing arrays are accounted on creation/growth and recounted on GC.
	t.Run("thread_call_frames_memlimit_recount", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function dive(n)
				if n == 0 then
					coroutine.yield("ready")
					return
				end
				return (dive(n - 1))
			end
			local kept = {}
			local failed = false
			local errMsg = ""
			for i = 1, 10 do
				local okCreate, co = pcall(coroutine.create, function() dive(100) end)
				if not okCreate then
					failed = true
					errMsg = tostring(co)
					break
				end
				local okResume, res = coroutine.resume(co)
				if not okResume then
					failed = true
					errMsg = tostring(res)
					break
				end
				kept[i] = co
				collectgarbage("collect")
			end
			local count = #kept
			kept = nil
			collectgarbage("collect")
			assert(failed and count < 10 and string.find(errMsg, "not enough memory", 1, true),
				"expected OOM retaining depth-100 coroutines, got failed=" .. tostring(failed) .. " count=" .. count .. " err=" .. errMsg)
			return "ok"
		`), "=thread_frames_memlimit")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(262144)
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 2. require roots the original package.searchers table across searcher callbacks.
	t.Run("require_roots_searchers_table_across_callbacks", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local finalized = false
			local orig = setmetatable({}, {
				__gc = function(self)
					finalized = true
					self[2] = nil
				end,
			})
			orig[1] = function()
				package.searchers = {}
				collectgarbage("collect")
				return "miss from 1"
			end
			orig[2] = function()
				return function() return "from_second" end
			end
			package.searchers = orig
			orig = nil
			local mod = require("rooted_searchers_mod")
			assert(mod == "from_second", "expected 'from_second', got " .. tostring(mod))
			assert(not finalized, "original searchers table was finalized prematurely during require")
			collectgarbage("collect")
			assert(finalized, "original searchers table should finalize after require returns")
			return "ok"
		`), "=require_root_searchers")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 3. pcall and xpcall dispatch call/return debug hooks for native targets.
	t.Run("protected_call_dispatches_native_debug_hooks", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local events = {}
			local function hook(ev)
				local info = debug.getinfo(2, "f")
				if info and info.func == math.abs then
					events[#events + 1] = ev
					if ev == "call" then
						debug.setlocal(2, 1, -77)
					end
				end
			end
			debug.sethook(hook, "cr")
			local ok1, r1 = pcall(math.abs, -42)
			debug.sethook()
			assert(ok1 and r1 == 77, "pcall(math.abs) unexpected result: " .. tostring(ok1) .. "," .. tostring(r1))
			assert(table.concat(events, ",") == "call,return", "pcall events: " .. table.concat(events, ","))

			events = {}
			debug.sethook(hook, "cr")
			local ok2, r2 = xpcall(math.abs, function(e) return e end, -42)
			debug.sethook()
			assert(ok2 and r2 == 77, "xpcall(math.abs) unexpected result: " .. tostring(ok2) .. "," .. tostring(r2))
			assert(table.concat(events, ",") == "call,return", "xpcall events: " .. table.concat(events, ","))
			return "ok"
		`), "=pcall_native_hooks")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 4. math.randomseed() with no args generates fresh seeds and new States start with varying RNG seeds.
	t.Run("randomseed_no_args_and_fresh_states_vary", func(t *testing.T) {
		s1 := NewState()
		defer s1.Close()
		s2 := NewState()
		defer s2.Close()

		cl1, err := s1.LoadChunk([]byte(`
			local init1 = math.random(0)
			local a1, b1 = math.randomseed()
			local v1 = math.random(0)
			local a2, b2 = math.randomseed()
			local v2 = math.random(0)
			local a3, b3 = math.randomseed()
			local v3 = math.random(0)
			assert(not (a1 == 0 and b1 == 0 and a2 == 0 and b2 == 0), "no-arg randomseed stayed at 0,0")
			assert(a1 ~= a2 or b1 ~= b2, "successive randomseed() returned identical seeds")
			assert(v1 ~= v2 or v2 ~= v3, "successive randomseed() produced identical sequences")
			assert(not pcall(math.randomseed, nil), "math.randomseed(nil) must reject nil")

			-- Explicit seeds remain strictly reproducible.
			local x1, y1 = math.randomseed(12345, 67890)
			assert(x1 == 12345 and y1 == 67890)
			local r1, r2 = math.random(0), math.random(0)
			math.randomseed(12345, 67890)
			assert(math.random(0) == r1 and math.random(0) == r2)
			return init1
		`), "=randomseed_vary1")
		if err != nil {
			t.Fatal(err)
		}
		res1, err := s1.Call(cl1)
		if err != nil || len(res1) != 1 {
			t.Fatalf("s1 Call failed: res=%v err=%v", res1, err)
		}

		cl2, err := s2.LoadChunk([]byte(`return math.random(0)`), "=randomseed_vary2")
		if err != nil {
			t.Fatal(err)
		}
		res2, err := s2.Call(cl2)
		if err != nil || len(res2) != 1 {
			t.Fatalf("s2 Call failed: res=%v err=%v", res2, err)
		}
		if res1[0].Int() == res2[0].Int() {
			t.Fatalf("expected separate fresh States to initialize distinct RNG seeds, both produced %d", res1[0].Int())
		}
	})

	// 5. LightUDValue(nil) preserves LUA_TLIGHTUSERDATA type, equality, formatting, and nil round-trip.
	t.Run("null_light_userdata_preserves_type", func(t *testing.T) {
		p := LightUDValue(nil)
		if !p.IsLightUD() || p.IsNumber() || p.IsNil() || p.AsLightUD() != nil {
			t.Fatalf("LightUDValue(nil) broken: IsLightUD=%v IsNumber=%v IsNil=%v AsLightUD=%v",
				p.IsLightUD(), p.IsNumber(), p.IsNil(), p.AsLightUD())
		}
		s := NewState()
		defer s.Close()
		s.SetGlobal("null_p1", p)
		s.SetGlobal("null_p2", LightUDValue(nil))
		cl, err := s.LoadChunk([]byte(`
			local upv = 1
			local fn = function() return upv end
			local nonNil = debug.upvalueid(fn, 1)

			assert(type(null_p1) == "userdata", "expected userdata, got " .. type(null_p1))
			assert(tonumber(null_p1) == nil, "null light userdata must not convert as number")
			assert(rawequal(null_p1, null_p2) and null_p1 == null_p2)
			assert(not rawequal(null_p1, 0) and null_p1 ~= 0)
			assert(not rawequal(null_p1, nil) and null_p1 ~= nil)
			assert(not rawequal(null_p1, nonNil) and null_p1 ~= nonNil)
			assert(not pcall(function() return null_p1 + 1 end), "arithmetic on null light userdata must fail")
			assert(tostring(null_p1) == "userdata: 0x0", "unexpected tostring: " .. tostring(null_p1))
			local tbl = { [null_p1] = "null", [0] = "zero", [nonNil] = "ptr" }
			assert(tbl[null_p2] == "null" and tbl[0] == "zero" and tbl[nonNil] == "ptr")
			return "ok"
		`), "=null_lightud")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound12VMRegressions(t *testing.T) {
	// 1. Long substrings from string.sub and string.match clone their backing storage instead of pinning huge sources.
	t.Run("substring_clones_backing_storage", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local kept = {}
			for i = 1, 300 do
				local big = string.rep(string.char(65 + (i % 26)), 32768)
				kept[i] = string.sub(big, 1, 41)
			end
			for i = 1, 100 do
				local big = string.rep("z", 32768)
				kept[300 + i] = string.match(big, "^(zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz)")
			end
			_G._kept_subs = kept
			return #kept
		`), "=sub_clone")
		if err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		res, err := s.Call(cl)
		if err != nil || len(res) != 1 || res[0].Int() != 400 {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
		runtime.GC()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		// Without cloning, 400 * 32 KiB = 13.1 MB would stay pinned on the Go heap.
		if after.HeapAlloc > before.HeapAlloc+4*1024*1024 {
			t.Fatalf("substrings pinned source allocations: before=%d after=%d delta=%d",
				before.HeapAlloc, after.HeapAlloc, after.HeapAlloc-before.HeapAlloc)
		}
	})

	// 2. Shape transitions and lookup maps are checked against memLimit and recounted on GC.
	t.Run("retained_shapes_accounted_in_memlimit", func(t *testing.T) {
		if !OptimizeEnabled {
			t.Skip("shape trees are disabled in faithful mode")
		}
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local kept = {}
			local failed = false
			local errMsg = ""
			for i = 1, 30 do
				local ok, res = pcall(function()
					local tbl = {}
					for j = 1, 64 do
						tbl["p_" .. i .. "_" .. j] = j
					end
					return tbl
				end)
				if not ok then
					failed = true
					errMsg = tostring(res)
					break
				end
				kept[i] = res
				collectgarbage("collect")
			end
			local count = #kept
			kept = nil
			collectgarbage("collect")
			assert(failed and count < 10 and string.find(errMsg, "not enough memory", 1, true),
				"expected OOM creating distinct 64-prop shapes, got failed=" .. tostring(failed) .. " count=" .. count .. " err=" .. errMsg)
			return "ok"
		`), "=shape_memlimit")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(131072)
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 3. os.time honors __index when reading date fields and __newindex when writing normalized fields back.
	t.Run("os_time_honors_index_and_newindex", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local writes = {}
			local backing = { year = 2024, month = 1, day = 1, hour = 0, min = 0, sec = 0 }
			local dt = setmetatable({}, {
				__index = backing,
				__newindex = function(_, k, v) writes[k] = v end,
			})
			local ts = os.time(dt)
			assert(type(ts) == "number")
			assert(writes.year == 2024 and writes.month == 1 and writes.day == 1 and writes.wday == 2 and writes.yday == 1)

			local bad = setmetatable({}, {
				__index = function() error("boom_from_index") end,
			})
			local ok, err = pcall(os.time, bad)
			assert(not ok and string.find(tostring(err), "boom_from_index", 1, true))
			return "ok"
		`), "=os_time_mm")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 4. pairs() normalizes __pairs results to exactly four values on all dispatch paths (including yieldable).
	t.Run("pairs_normalizes_metamethod_arity_to_four", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local t1 = setmetatable({}, { __pairs = function() return next end })
			local t5 = setmetatable({}, { __pairs = function() return 1, 2, 3, 4, 5 end })
			assert(select("#", pairs({})) == 4)
			assert(select("#", pairs(t1)) == 4)
			assert(select("#", pairs(t5)) == 4)
			local a, b, c, d, e = pairs(t5)
			assert(a == 1 and b == 2 and c == 3 and d == 4 and e == nil)
			local function tailPairs(x) return pairs(x) end
			assert(select("#", tailPairs(t1)) == 4)
			assert(select("#", tailPairs(t5)) == 4)
			assert(select("#", pcall(pairs, t1)) == 5)
			assert(select("#", pcall(pairs, t5)) == 5)

			-- Yieldable __pairs preserving exact 4-value normalization:
			local ty = setmetatable({}, {
				__pairs = function()
					coroutine.yield("in_pairs")
					return 10, 20, 30, 40, 50
				end,
			})
			local co = coroutine.create(function()
				local n = select("#", pairs(ty))
				return n
			end)
			local ok1, y1 = coroutine.resume(co)
			assert(ok1 and y1 == "in_pairs")
			local ok2, n2 = coroutine.resume(co)
			assert(ok2 and n2 == 4, "expected yieldable __pairs arity 4, got " .. tostring(n2))
			return "ok"
		`), "=pairs_arity")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 5. os.date supports standard C89/C99 conversion specifiers including %I, %p, %U, %W.
	t.Run("os_date_supports_c89_and_c99_specifiers", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local fmt = "!%Y-%m-%d %I:%M:%S %p %U %W %C %D %e %F %g %G %V %r %R %T %u"
			assert(os.date(fmt, 0) == "1970-01-01 12:00:00 AM 00 00 19 01/01/70  1 1970-01-01 70 1970 01 12:00:00 AM 00:00 00:00:00 4")
			assert(os.date(fmt, 12*3600) == "1970-01-01 12:00:00 PM 00 00 19 01/01/70  1 1970-01-01 70 1970 01 12:00:00 PM 12:00 12:00:00 4")
			assert(os.date(fmt, 13*3600) == "1970-01-01 01:00:00 PM 00 00 19 01/01/70  1 1970-01-01 70 1970 01 01:00:00 PM 13:00 13:00:00 4")
			assert(os.date(fmt, 1704067200) == "2024-01-01 12:00:00 AM 00 01 20 01/01/24  1 2024-01-01 24 2024 01 12:00:00 AM 00:00 00:00:00 1")
			assert(os.date(fmt, 1735689599) == "2024-12-31 11:59:59 PM 52 53 20 12/31/24 31 2024-12-31 25 2025 01 11:59:59 PM 23:59 23:59:59 2")
			assert(not pcall(os.date, "!%Q", 0))
			return "ok"
		`), "=os_date_c99")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 6. string.gmatch iterators observe instruction limits attached after iterator creation.
	t.Run("gmatch_refreshes_cancellation_controls", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		mk, err := s.LoadChunk([]byte(`
			_G.iter = string.gmatch(string.rep("a", 300), "a*a*a*b")
		`), "=gmatch_mk")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(mk); err != nil {
			t.Fatalf("setup Call failed: %v", err)
		}
		s.SetInstructionLimit(100)
		runIt, err := s.LoadChunk([]byte(`return _G.iter()`), "=gmatch_run")
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err = s.Call(runIt)
		elapsed := time.Since(start)
		if !errors.Is(err, ErrInstructionLimit) {
			t.Fatalf("expected ErrInstructionLimit advancing pre-created gmatch iterator, got %v (elapsed=%v)", err, elapsed)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("gmatch abort took too long: %v", elapsed)
		}
	})

	// 7. Primitive tostring formatting ignores __name when __tostring is absent.
	t.Run("primitive_tostring_ignores_metatable_name", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetGlobal("null_ptr", LightUDValue(nil))
		cl, err := s.LoadChunk([]byte(`
			debug.setmetatable(0, { __name = "MyNum" })
			debug.setmetatable(true, { __name = "MyBool" })
			debug.setmetatable(nil, { __name = "MyNil" })
			local origStrMT = debug.getmetatable("")
			origStrMT.__name = "MyStr"
			assert(tostring(42) == "42", "got " .. tostring(42))
			assert(tostring(3.5) == "3.5", "got " .. tostring(3.5))
			assert(tostring(true) == "true", "got " .. tostring(true))
			assert(tostring(false) == "false", "got " .. tostring(false))
			assert(tostring(nil) == "nil", "got " .. tostring(nil))
			assert(tostring("hi") == "hi", "got " .. tostring("hi"))

			-- Explicit __tostring on primitives still takes precedence.
			debug.setmetatable(0, { __name = "MyNum", __tostring = function(x) return "num:" .. x end })
			assert(tostring(42) == "num:42")

			-- Non-primitives (table, lightuserdata) still honor __name.
			local t = setmetatable({}, { __name = "MyTbl" })
			assert(string.sub(tostring(t), 1, 7) == "MyTbl: ", "got " .. tostring(t))
			debug.setmetatable(null_ptr, { __name = "MyPtr" })
			assert(tostring(null_ptr) == "MyPtr: 0x0", "got " .. tostring(null_ptr))

			origStrMT.__name = nil
			debug.setmetatable(0, nil)
			debug.setmetatable(true, nil)
			debug.setmetatable(nil, nil)
			debug.setmetatable(null_ptr, nil)
			return "ok"
		`), "=primitive_tostring_name")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound13VMRegressions(t *testing.T) {
	// 1. string.gmatch iterators expose captured subject/pattern strings to GC recounting.
	t.Run("gmatch_retains_subject_pattern_in_gc_memlimit", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local kept = {}
			local failed = false
			local errMsg = ""
			for i = 1, 100 do
				local ok, res = pcall(function()
					return string.gmatch(string.rep("x", 32768), ".")
				end)
				if not ok then
					failed = true
					errMsg = tostring(res)
					break
				end
				kept[i] = res
				collectgarbage("collect")
			end
			local count = #kept
			kept = nil
			collectgarbage("collect")
			assert(failed and count >= 1 and count < 6 and string.find(errMsg, "not enough memory", 1, true),
				"expected OOM retaining 32KiB gmatch iterators, got failed=" .. tostring(failed) .. " count=" .. count .. " err=" .. errMsg)
			return "ok"
		`), "=gmatch_gc_memlimit")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(131072)
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 2. Deleted hash keys release unreachable Go references on GC while preserving live pairs() traversal.
	t.Run("deleted_hash_keys_released_on_gc", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local kept = {}
			for i = 1, 200 do
				local t = {}
				local k = string.rep(string.char(65 + (i % 26)), 32768)
				t[k] = i
				t[k] = nil
				kept[i] = t
				k = nil
				collectgarbage("collect")
			end
			_G._kept_tombstone_tables = kept

			-- Live deleted keys remain valid for active pairs() traversal even across GC:
			local pt = {}
			for i = 1, 8 do
				pt[string.rep(string.char(97 + i), 64)] = i
			end
			local seen = 0
			for k, v in pairs(pt) do
				seen = seen + v
				pt[k] = nil
				collectgarbage("collect")
			end
			assert(seen == 36, "expected pairs sum 36, got " .. tostring(seen))
			return #kept
		`), "=deleted_hash_keys")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(262144)
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		res, err := s.Call(cl)
		if err != nil || len(res) != 1 || res[0].Int() != 200 {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
		runtime.GC()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		// Without dead-key release, 200 * 32 KiB = 6.55 MB would stay pinned on the Go heap.
		if after.HeapAlloc > before.HeapAlloc+2*1024*1024 {
			t.Fatalf("deleted hash keys pinned heap allocations: before=%d after=%d delta=%d",
				before.HeapAlloc, after.HeapAlloc, after.HeapAlloc-before.HeapAlloc)
		}
	})

	// 3. string.dump enforces the state's memory limit before and after serialization.
	t.Run("string_dump_enforces_memlimit", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function sample(x) return x + 1 end
			local kept = {}
			local failed = false
			local errMsg = ""
			for i = 1, 2000 do
				local ok, res = pcall(string.dump, sample)
				if not ok then
					failed = true
					errMsg = tostring(res)
					break
				end
				kept[i] = res
			end
			local count = #kept
			kept = nil
			collectgarbage("collect")
			assert(failed and count < 1500 and string.find(errMsg, "not enough memory", 1, true),
				"expected OOM retaining repeated string.dump outputs, got failed=" .. tostring(failed) .. " count=" .. count .. " err=" .. errMsg)
			return "ok"
		`), "=dump_memlimit")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(131072)
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 4. Native metamethods (__len, __call, __concat, __close) dispatch call/return debug hooks.
	t.Run("native_metamethods_dispatch_debug_hooks", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local events = {}
			local target = nil
			local function hook(ev)
				local info = debug.getinfo(2, "f")
				if info and info.func == target then
					events[#events + 1] = ev
				end
			end

			-- __len native metamethod:
			local tlen = setmetatable({}, { __len = tostring })
			target = tostring
			events = {}
			debug.sethook(hook, "cr")
			local rlen = #tlen
			debug.sethook()
			assert(type(rlen) == "string")
			assert(table.concat(events, ",") == "call,return", "__len events: " .. table.concat(events, ","))

			-- __call native metamethod:
			local tcall = setmetatable({}, { __call = tostring })
			target = tostring
			events = {}
			debug.sethook(hook, "cr")
			local rcall = tcall()
			debug.sethook()
			assert(type(rcall) == "string")
			assert(table.concat(events, ",") == "call,return", "__call events: " .. table.concat(events, ","))

			-- __concat native metamethod:
			local tcat = setmetatable({}, { __concat = tostring })
			target = tostring
			events = {}
			debug.sethook(hook, "cr")
			local rcat = tcat .. "x"
			debug.sethook()
			assert(type(rcat) == "string")
			assert(table.concat(events, ",") == "call,return", "__concat events: " .. table.concat(events, ","))

			-- __close native metamethod:
			target = tostring
			events = {}
			debug.sethook(hook, "cr")
			do
				local x <close> = setmetatable({}, { __close = tostring })
			end
			debug.sethook()
			assert(table.concat(events, ",") == "call,return", "__close events: " .. table.concat(events, ","))
			return "ok"
		`), "=native_mm_hooks")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 5. Numeric file reads propagate non-EOF OS read errors (e.g. EISDIR on a directory) instead of returning bare nil.
	t.Run("numeric_file_read_propagates_non_eof_error", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("opening a directory for reading is POSIX-specific")
		}
		s := NewState()
		defer s.Close()
		dir := t.TempDir()
		cl, err := s.LoadChunk([]byte(fmt.Sprintf(`
			local f = assert(io.open(%q, "r"))
			local v, msg, code = f:read("n")
			assert(v == nil and type(msg) == "string" and code == %d,
				"expected nil, errmsg, EISDIR; got " .. tostring(v) .. ", " .. tostring(msg) .. ", " .. tostring(code))
			local it = f:lines("n")
			local ok, lerr = pcall(it)
			assert(not ok and type(lerr) == "string", "expected lines('n') to error on directory read, got " .. tostring(ok) .. ", " .. tostring(lerr))
			f:close()
			return "ok"
		`, dir, int(syscall.EISDIR))), "=read_num_eisdir")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 6. table.remove clears pos even when pos == #t + 1 (or pos == #t == 0) with custom __len / proxy tables.
	t.Run("table_remove_clears_boundary_position", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local t = setmetatable({ 42 }, { __len = function() return 0 end })
			local v = table.remove(t, 1)
			assert(v == 42, "expected removed value 42, got " .. tostring(v))
			assert(t[1] == nil, "expected t[1] to be cleared, got " .. tostring(t[1]))

			local t0 = setmetatable({ [0] = 99 }, { __len = function() return 0 end })
			local v0 = table.remove(t0)
			assert(v0 == 99 and t0[0] == nil, "expected t0[0] to be removed and cleared")

			local writes = {}
			local proxy = setmetatable({}, {
				__len = function() return 0 end,
				__index = function(_, k) if k == 1 then return "proxy_val" end end,
				__newindex = function(_, k, val) writes[#writes + 1] = tostring(k) .. "=" .. tostring(val) end,
			})
			assert(table.remove(proxy, 1) == "proxy_val")
			assert(table.concat(writes, ",") == "1=nil", "expected __newindex(1, nil), got " .. table.concat(writes, ","))
			return "ok"
		`), "=table_remove_boundary")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 7. string.len coerces numeric arguments while keeping invalid types rejected.
	t.Run("string_len_coerces_numbers", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			assert(string.len(12345) == 5)
			assert(string.len(1.5) == 3)
			assert(string.len(-0) == 1)
			assert(string.len("abc") == 3)
			local ok1, r1 = pcall(string.len, 12345)
			assert(ok1 and r1 == 5)
			assert(not pcall(string.len))
			assert(not pcall(string.len, nil))
			assert(not pcall(string.len, true))
			assert(not pcall(string.len, {}))
			return "ok"
		`), "=string_len_num")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 8. io.popen validates mode before starting the subprocess.
	t.Run("io_popen_validates_mode_before_starting", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		marker := filepath.Join(t.TempDir(), "should_not_exist.txt")
		var cmdStr string
		if runtime.GOOS == "windows" {
			cmdStr = fmt.Sprintf(`echo boom > "%s"`, marker)
		} else {
			cmdStr = fmt.Sprintf(`touch %q`, marker)
		}
		cl, err := s.LoadChunk([]byte(fmt.Sprintf(`
			local cmd = %q
			local ok1, e1 = pcall(io.popen, cmd, "invalid")
			assert(not ok1 and string.find(tostring(e1), "invalid mode", 1, true), "unexpected invalid mode result: " .. tostring(ok1) .. "," .. tostring(e1))
			local ok2, e2 = pcall(io.popen, cmd, {})
			assert(not ok2 and string.find(tostring(e2), "string expected", 1, true), "unexpected non-string mode result: " .. tostring(ok2) .. "," .. tostring(e2))
			local ok3, e3 = pcall(io.popen, cmd, 123)
			assert(not ok3 and string.find(tostring(e3), "invalid mode", 1, true), "unexpected numeric mode result: " .. tostring(ok3) .. "," .. tostring(e3))
			if %v then
				local ok4, e4 = pcall(io.popen, cmd, "rb")
				assert(not ok4 and string.find(tostring(e4), "invalid mode", 1, true), "expected POSIX popen to reject 'rb', got " .. tostring(ok4) .. "," .. tostring(e4))
			end
			return "ok"
		`, cmdStr, runtime.GOOS != "windows")), "=popen_mode")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Fatalf("io.popen started subprocess despite invalid mode (marker %q exists or stat err=%v)", marker, statErr)
		}
	})
}

func TestCodexReviewRound14VMRegressions(t *testing.T) {
	// 1. string.unpack clones long extracted substrings instead of pinning large input buffers.
	t.Run("string_unpack_clones_backing_storage", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local kept = {}
			for i = 1, 300 do
				local big = string.rep(string.char(65 + (i % 26)), 32768)
				local v = string.unpack("c41", big)
				kept[i] = v
				big = nil
				collectgarbage("collect")
			end
			_G._kept_unpacked = kept
			return #kept
		`), "=unpack_clone")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(131072)
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		res, err := s.Call(cl)
		if err != nil || len(res) != 1 || res[0].Int() != 300 {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
		runtime.GC()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		// Without cloning, 300 * 32 KiB = 9.8 MB would remain pinned on the Go heap.
		if after.HeapAlloc > before.HeapAlloc+2*1024*1024 {
			t.Fatalf("string.unpack pinned source allocations: before=%d after=%d delta=%d",
				before.HeapAlloc, after.HeapAlloc, after.HeapAlloc-before.HeapAlloc)
		}
	})

	// 2. string.format enforces memory limits incrementally before growing large output buffers.
	t.Run("string_format_enforces_memlimit_during_formatting", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local chunk = string.rep("a", 16384)
			local args = {}
			for i = 1, 200 do args[i] = chunk end

			local fmtS = string.rep("%s", 200)
			local ok1, err1 = pcall(string.format, fmtS, table.unpack(args))
			assert(not ok1 and string.find(tostring(err1), "not enough memory", 1, true),
				"expected OOM on %s format, got ok=" .. tostring(ok1) .. " err=" .. tostring(err1))

			local fmtQ = string.rep("%q", 200)
			local ok2, err2 = pcall(string.format, fmtQ, table.unpack(args))
			assert(not ok2 and string.find(tostring(err2), "not enough memory", 1, true),
				"expected OOM on %q format, got ok=" .. tostring(ok2) .. " err=" .. tostring(err2))
			return "ok"
		`), "=format_memlimit")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(131072)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
		runtime.ReadMemStats(&after)
		// Without incremental checks, formatting 200 * 16 KiB allocated ~17 MB+ before returning OOM.
		if delta := after.TotalAlloc - before.TotalAlloc; delta > 4*1024*1024 {
			t.Fatalf("string.format allocated too much before enforcing memLimit: delta=%d", delta)
		}
	})

	// 3. Numeric for loops decide integer vs float mode before coercing string init/step operands.
	t.Run("numeric_for_selects_integer_loop_before_string_coercion", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local got = {}
			for i = "1", "1" do got[#got + 1] = math.type(i) .. ":" .. tostring(i) end
			for i = 1, 1, "1" do got[#got + 1] = math.type(i) .. ":" .. tostring(i) end
			for i = "1", 2, 1 do got[#got + 1] = math.type(i) .. ":" .. tostring(i) end
			for i = 1, "2", 1 do got[#got + 1] = math.type(i) .. ":" .. tostring(i) end
			for i = 1, "2.5", 1 do got[#got + 1] = math.type(i) .. ":" .. tostring(i) end
			for i = 1, 2.0, 1 do got[#got + 1] = math.type(i) .. ":" .. tostring(i) end
			local expected = "float:1.0,float:1.0,float:1.0,float:2.0,integer:1,integer:2,integer:1,integer:2,integer:1,integer:2"
			assert(table.concat(got, ",") == expected, "unexpected for loop subtypes: " .. table.concat(got, ","))
			return "ok"
		`), "=forprep_str_coercion")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 4. os.exit and os.execute reject invalid arguments with catchable Lua errors before exiting/executing.
	t.Run("os_exit_and_execute_reject_invalid_arguments", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local ok1, e1 = pcall(os.exit, "invalid")
			assert(not ok1 and string.find(tostring(e1), "number expected", 1, true), "got " .. tostring(ok1) .. "," .. tostring(e1))
			local ok2, e2 = pcall(os.exit, 1.5)
			assert(not ok2 and string.find(tostring(e2), "no integer representation", 1, true), "got " .. tostring(ok2) .. "," .. tostring(e2))
			local ok3, e3 = pcall(os.exit, "1.5")
			assert(not ok3 and string.find(tostring(e3), "no integer representation", 1, true), "got " .. tostring(ok3) .. "," .. tostring(e3))
			local ok4, e4 = pcall(os.exit, {})
			assert(not ok4 and string.find(tostring(e4), "number expected", 1, true), "got " .. tostring(ok4) .. "," .. tostring(e4))
			local ok5, e5 = pcall(os.execute, {})
			assert(not ok5 and string.find(tostring(e5), "string expected", 1, true), "got " .. tostring(ok5) .. "," .. tostring(e5))
			return "ok"
		`), "=os_exit_invalid")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound15VMRegressions(t *testing.T) {
	// 1. Inline caches (monomorphic and polymorphic) release invalidated/unreachable references on GC.
	t.Run("inline_cache_releases_unreachable_entries_on_gc", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local getters = {}
			for i = 1, 80 do
				local fn = assert(load("return function(o) return o.v end"))()
				local proto = { v = string.rep(string.char(65 + (i % 26)), 32768) }
				local mt = { __index = proto }
				local o1 = setmetatable({ a = 1 }, mt)
				local o2 = setmetatable({ b = 2 }, mt)
				assert(#fn(o1) == 32768)
				assert(#fn(o2) == 32768) -- populate polymorphic IC slot too
				mt.__index = nil
				proto = nil
				o1, o2, mt = nil, nil, nil
				getters[i] = fn
				collectgarbage("collect")
			end
			_G._kept_ic_getters = getters
			return #getters
		`), "=ic_gc_sweep")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(524288)
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		res, err := s.Call(cl)
		if err != nil || len(res) != 1 || res[0].Int() != 80 {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
		runtime.GC()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		// Without IC sweeping, 80 * 32 KiB = 2.62 MB would stay pinned on the Go heap.
		if after.HeapAlloc > before.HeapAlloc+1536*1024 {
			t.Fatalf("inline caches pinned unreachable strings: before=%d after=%d delta=%d",
				before.HeapAlloc, after.HeapAlloc, after.HeapAlloc-before.HeapAlloc)
		}
	})

	// 2. State.Call roots native callee and arguments even when debug hooks are disabled.
	t.Run("state_call_roots_native_args_without_hooks", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local t = setmetatable({ live = true }, {
				__gc = function(self) self.live = false end,
			})
			return t
		`), "=mk_gc_tbl")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(cl)
		if err != nil || len(res) != 1 {
			t.Fatalf("setup failed: res=%v err=%v", res, err)
		}
		tblVal := res[0]
		liveDuringCall := false
		consumer := NativeValue(&NativeFunc{
			Name: "gc_consumer",
			Fn: func(st *State, args []Value) ([]Value, error) {
				st.CollectFull()
				v := args[0].AsTable().RawGetStr(st.Intern("live"))
				liveDuringCall = v.IsTrue()
				return []Value{BoolValue(liveDuringCall)}, nil
			},
		})
		out, err := s.Call(consumer, tblVal)
		if err != nil || len(out) != 1 || !out[0].IsTrue() || !liveDuringCall {
			t.Fatalf("native argument finalized prematurely during State.Call: out=%v live=%v err=%v", out, liveDuringCall, err)
		}
		// Once the call returns and the table is no longer referenced in the VM, GC finalizes it.
		s.CollectFull()
		if tblVal.AsTable().RawGetStr(s.Intern("live")).IsTrue() {
			t.Fatalf("expected table to finalize after State.Call returned")
		}
	})

	// 3. __close metamethod receives 1 argument on normal close (matching PUC-Rio lfunc.c:callclosemethod)
	// and 2 arguments (obj, err) when unwinding an active error (including error(nil)).
	t.Run("close_metamethod_arity_matches_lua55", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local n1, a1, b1
			do
				local x <close> = setmetatable({}, {
					__close = function(...) n1 = select("#", ...); a1, b1 = ... end,
				})
			end
			assert(n1 == 1 and type(a1) == "table" and b1 == nil, "normal close arity: " .. tostring(n1))

			local n2
			local co = coroutine.create(function()
				local x <close> = setmetatable({}, {
					__close = function(...) n2 = select("#", ...) end,
				})
				coroutine.yield()
			end)
			assert(coroutine.resume(co))
			assert(coroutine.close(co))
			assert(n2 == 1, "coroutine.close arity: " .. tostring(n2))

			local n3, e3
			local ok = pcall(function()
				local x <close> = setmetatable({}, {
					__close = function(...) n3 = select("#", ...); e3 = select(2, ...) end,
				})
				error("boom")
			end)
			assert(not ok and n3 == 2 and string.find(tostring(e3), "boom", 1, true), "error close arity: " .. tostring(n3))
			return "ok"
		`), "=close_arity")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 4. dofile preserves yieldability across normal call, tail call, pcall, and xpcall.
	t.Run("dofile_yieldable_across_tailcall_and_protected_calls", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		p := filepath.Join(t.TempDir(), "yield_dofile.lua")
		if err := os.WriteFile(p, []byte(`local v = coroutine.yield("paused"); return "done:" .. tostring(v)`), 0o600); err != nil {
			t.Fatal(err)
		}
		cl, err := s.LoadChunk([]byte(fmt.Sprintf(`
			local path = %q
			local function runCase(fn)
				local co = coroutine.create(fn)
				local ok1, y1 = coroutine.resume(co)
				assert(ok1 and y1 == "paused", "first resume failed: " .. tostring(ok1) .. "," .. tostring(y1))
				local ok2, r2, r3 = coroutine.resume(co, 7)
				return ok2, r2, r3
			end

			-- Direct call:
			local ok, r = runCase(function() local x = dofile(path); return x end)
			assert(ok and r == "done:7", "direct: " .. tostring(ok) .. "," .. tostring(r))

			-- Tail call:
			ok, r = runCase(function() return dofile(path) end)
			assert(ok and r == "done:7", "tailcall: " .. tostring(ok) .. "," .. tostring(r))

			-- pcall(dofile, path):
			local okP, rP
			ok, okP, rP = runCase(function() return pcall(dofile, path) end)
			assert(ok and okP and rP == "done:7", "pcall: " .. tostring(ok) .. "," .. tostring(okP) .. "," .. tostring(rP))

			-- xpcall(dofile, msgh, path):
			ok, okP, rP = runCase(function() return xpcall(dofile, function(e) return "msgh:" .. tostring(e) end, path) end)
			assert(ok and okP and rP == "done:7", "xpcall: " .. tostring(ok) .. "," .. tostring(okP) .. "," .. tostring(rP))
			return "ok"
		`, p)), "=dofile_yield")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 5. require honors __index and __newindex metamethods on package.loaded.
	t.Run("require_honors_package_loaded_metamethods", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local writes = {}
			setmetatable(package.loaded, {
				__index = function(_, k)
					if k == "virt.sentinel" then return "SENTINEL" end
					if k == "virt.err" then error("index_boom") end
				end,
				__newindex = function(t, k, v)
					writes[k] = v
					rawset(t, k, v)
				end,
			})
			assert(require("virt.sentinel") == "SENTINEL")
			local okErr, errMsg = pcall(require, "virt.err")
			assert(not okErr and string.find(tostring(errMsg), "index_boom", 1, true))

			package.preload["virt.preload_mod"] = function() return "PRELOAD_VAL" end
			local v, extra = require("virt.preload_mod")
			assert(v == "PRELOAD_VAL" and extra == ":preload:")
			assert(writes["virt.preload_mod"] == "PRELOAD_VAL")
			setmetatable(package.loaded, nil)
			return "ok"
		`), "=require_loaded_mm")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 6. debug.getinfo(0, ...) populates func, nups, nparams, isvararg, istailcall (active and suspended).
	t.Run("debug_getinfo_level0_populates_all_fields", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local info = debug.getinfo(0, "futSlnr")
			assert(info.func == debug.getinfo, "expected info.func == debug.getinfo")
			assert(info.nups == 0 and info.nparams == 0 and info.isvararg == true)
			assert(info.istailcall == false and info.extraargs == 0)
			assert(info.what == "C" and info.name == "getinfo")

			local co = coroutine.create(function() coroutine.yield("here") end)
			assert(coroutine.resume(co))
			local cinfo = debug.getinfo(co, 0, "futSlnr")
			assert(cinfo.func == coroutine.yield, "expected suspended level 0 func == coroutine.yield")
			assert(cinfo.nups == 0 and cinfo.nparams == 0 and cinfo.isvararg == true)
			assert(cinfo.istailcall == false and cinfo.what == "C" and cinfo.name == "yield")
			return "ok"
		`), "=getinfo_lvl0")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})

	// 7. tableSetInt and tableSetStr follow non-function __newindex values as indexing targets.
	t.Run("table_set_follows_non_function_newindex_targets", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local backing = {}
			debug.setmetatable(0, {
				__newindex = function(_, k, v) backing[k] = v end,
			})
			local t = setmetatable({}, { __newindex = 0 })
			table.insert(t, 42)
			assert(backing[1] == 42, "expected table.insert to follow non-function __newindex target, got " .. tostring(backing[1]))

			local dt = setmetatable({}, {
				__index = { year = 2024, month = 1, day = 1, hour = 0, min = 0, sec = 0 },
				__newindex = 0,
			})
			assert(type(os.time(dt)) == "number")
			assert(backing.year == 2024 and backing.month == 1 and backing.day == 1)
			debug.setmetatable(0, nil)
			return "ok"
		`), "=newindex_nonfunc")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
	})
}

type tinyChunkReader struct {
	data []byte
	step int
	off  int
}

func (r *tinyChunkReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := min(len(p), r.step)
	n = min(n, len(r.data)-r.off)
	copy(p[:n], r.data[r.off:r.off+n])
	r.off += n
	return n, nil
}

func TestStreamingLoadAndDump(t *testing.T) {
	// 1. bytecode.DumpTo / DumpString / UndumpReader match fast Dump / Undump byte-for-byte,
	// and malformed huge length headers fail cleanly at EOF without pre-allocating gigabytes.
	t.Run("bytecode_stream_dump_and_undump_identity", func(t *testing.T) {
		src := `
			local function fib(n)
				if n < 2 then return n end
				return fib(n - 1) + fib(n - 2)
			end
			local s = "hello " .. string.rep("z", 64)
			return fib(10), s
		`
		p, err := compiler.Compile("=stream_bc", src)
		if err != nil {
			t.Fatal(err)
		}
		wantBytes, err := bytecode.Dump(p, false)
		if err != nil {
			t.Fatal(err)
		}
		var streamed bytes.Buffer
		if err := bytecode.DumpTo(&streamed, p, false); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(wantBytes, streamed.Bytes()) {
			t.Fatalf("DumpTo mismatch: got %d bytes, want %d bytes", streamed.Len(), len(wantBytes))
		}
		strDump, err := bytecode.DumpString(p, true)
		if err != nil {
			t.Fatal(err)
		}
		wantStrip, err := bytecode.Dump(p, true)
		if err != nil {
			t.Fatal(err)
		}
		if strDump != string(wantStrip) {
			t.Fatalf("DumpString(strip=true) mismatch")
		}

		// Stream back through a 7-byte reader and re-dump to verify exact Proto fidelity.
		p2, err := bytecode.UndumpReader(&tinyChunkReader{data: wantBytes, step: 7})
		if err != nil {
			t.Fatalf("UndumpReader failed: %v", err)
		}
		reDumped, err := bytecode.Dump(p2, false)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(wantBytes, reDumped) {
			t.Fatalf("UndumpReader round-trip mismatch")
		}

		// Truncated stream declaring an enormous instruction count must pass header verification,
		// decode ncode = 1<<28, and fail cleanly on the truncated instruction stream without OOM.
		// Header is 40 bytes + 1 byte root nup + 5 bytes root proto metadata (lineDefined=0,
		// lastLineDefined=0, numParams, flag, maxStackSize) = 46 bytes up to ncode.
		corrupt := append([]byte(nil), wantBytes[:46]...)
		// Append varint encoding of 1<<28 (268,435,456 instructions = 1 GiB) + 1 byte align-to-4 pad.
		corrupt = append(corrupt, 0x81, 0x80, 0x80, 0x80, 0x00, 0x00)
		if _, err := bytecode.UndumpReader(bytes.NewReader(corrupt)); err == nil || !strings.Contains(err.Error(), "truncated chunk reading 268435456 instructions") {
			t.Fatalf("expected truncated instruction stream error on huge ncode, got %v", err)
		}
	})

	// 2. compiler.CompileReader streams 3 bytes at a time and produces byte-identical bytecode to Compile.
	t.Run("compiler_stream_matches_string_fast_path", func(t *testing.T) {
		src := "\xef\xbb\xbf#!/usr/bin/env lua\n" + `
			-- line 2 comment
			local long = [==[
				hello \n world
				multiline text
			]==]
			local escaped = "a\tb\x41\u{1F600}\z
			   continued"
			local sum = 0
			for i = 1, 10 do
				sum = sum + i
			end
			return sum, #long, escaped
		`
		pFast, err := compiler.Compile("@stream.lua", src)
		if err != nil {
			t.Fatal(err)
		}
		pStream, err := compiler.CompileReader("@stream.lua", &tinyChunkReader{data: []byte(src), step: 3})
		if err != nil {
			t.Fatalf("CompileReader failed: %v", err)
		}
		b1, err := bytecode.Dump(pFast, false)
		if err != nil {
			t.Fatal(err)
		}
		b2, err := bytecode.Dump(pStream, false)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("CompileReader bytecode diverged from Compile (%d vs %d bytes)", len(b2), len(b1))
		}
	})

	// 3. load(reader) streams lazily (early stop on mode mismatch / syntax error, low memory on large comment stream)
	// and loadfile streams text and binary files with BOM/shebang headers.
	t.Run("vm_streaming_load_and_loadfile", func(t *testing.T) {
		s := NewState()
		defer s.Close()

		// Early stop on mode mismatch & syntax error:
		cl, err := s.LoadChunk([]byte(`
			local calls1 = 0
			local f1, e1 = load(function()
				calls1 = calls1 + 1
				if calls1 == 1 then return "return 1" end
				return " + 2"
			end, "=m", "b")
			assert(f1 == nil and calls1 == 1 and string.find(e1, "text chunk", 1, true),
				"expected 1 reader call on mode mismatch, got calls=" .. calls1 .. " err=" .. tostring(e1))

			local calls2 = 0
			local f2, e2 = load(function()
				calls2 = calls2 + 1
				if calls2 == 1 then return "local = ???\n" end
				return "return 1\n"
			end, "=syn")
			assert(f2 == nil and calls2 == 1, "expected 1 reader call on early syntax error, got calls=" .. calls2)

			-- Streaming binary chunk through load(reader) in 5-byte slices:
			local dumped = string.dump(function(x) return x * 6 end)
			local pos = 1
			local bf = assert(load(function()
				if pos > #dumped then return nil end
				local piece = string.sub(dumped, pos, pos + 4)
				pos = pos + 5
				return piece
			end, "=bin_stream", "b"))
			assert(bf(7) == 42)
			return "ok"
		`), "=stream_early_stop")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}

		// Streaming 1.5 MiB of Lua comments + code via load(reader) under a 131 KiB memory limit succeeds
		// (would immediately OOM if load(reader) buffered the entire source before compiling!).
		sMem := NewState()
		defer sMem.Close()
		clMem, err := sMem.LoadChunk([]byte(`
			local comment = "-- " .. string.rep("x", 3000) .. "\n"
			local i = 0
			local fn = assert(load(function()
				i = i + 1
				if i <= 500 then return comment end
				if i == 501 then return "return 12345" end
				return nil
			end, "=big_stream", "t"))
			return fn()
		`), "=stream_low_mem")
		if err != nil {
			t.Fatal(err)
		}
		sMem.SetMemoryLimit(131072)
		if res, err := sMem.Call(clMem); err != nil || len(res) != 1 || res[0].Int() != 12345 {
			t.Fatalf("streaming large source under 128KiB memLimit failed: res=%v err=%v", res, err)
		}

		// loadfile streaming with BOM + shebang + binary payload & directory error:
		dir := t.TempDir()
		p, err := compiler.Compile("@shebang_bin.lua", "return 99")
		if err != nil {
			t.Fatal(err)
		}
		binChunk, err := bytecode.Dump(p, false)
		if err != nil {
			t.Fatal(err)
		}
		binFile := filepath.Join(dir, "shebang_bin.luac")
		header := append([]byte("\xef\xbb\xbf#!/usr/bin/env lua\n"), binChunk...)
		if err := os.WriteFile(binFile, header, 0o600); err != nil {
			t.Fatal(err)
		}
		clFile, err := s.LoadChunk([]byte(fmt.Sprintf(`
			local fn = assert(loadfile(%q, "b"))
			assert(fn() == 99)
			return "ok"
		`, binFile)), "=loadfile_stream_bin")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(clFile); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("loadfile binary shebang failed: res=%v err=%v", res, err)
		}
		if runtime.GOOS != "windows" {
			clDir, err := s.LoadChunk([]byte(fmt.Sprintf(`
				local fn, msg = loadfile(%q)
				assert(fn == nil and string.find(tostring(msg), "cannot read", 1, true), "got " .. tostring(fn) .. "," .. tostring(msg))
				return "ok"
			`, dir)), "=loadfile_dir")
			if err != nil {
				t.Fatal(err)
			}
			if res, err := s.Call(clDir); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
				t.Fatalf("loadfile dir check failed: res=%v err=%v", res, err)
			}
		}
	})
}

func TestCodexReviewRound16VMRegressions(t *testing.T) {
	// 1. os.date enforces memLimit while building output and accounts for the interned result.
	t.Run("os_date_memory_budget_and_accounting", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(131072)

		cl1, err := s.LoadChunk([]byte(`
			assert(os.date("!%Y-%m-%d", 0) == "1970-01-01")
			local dt = os.date("!*t", 0)
			assert(dt.year == 1970 and dt.month == 1 and dt.day == 1)
			local kept = {}
			local fmt = string.rep("%c", 1000)
			local ok, err = pcall(function()
				for i = 1, 100 do
					kept[i] = os.date(fmt, 0)
				end
			end)
			local nkept = #kept
			local memPeak = collectgarbage("count") * 1024
			kept = nil
			collectgarbage("collect")
			assert(not ok and string.find(tostring(err), "not enough memory", 1, true),
				"expected OOM retaining os.date results, got " .. tostring(ok) .. "," .. tostring(err))
			assert(nkept > 0 and nkept < 10, "expected 1..9 retained 24KB results under 128KiB limit, got " .. tostring(nkept))
			assert(memPeak >= nkept * 20000, "expected accounted memory >= retained os.date bytes, got " .. tostring(memPeak))
			return "ok"
		`), "=os_date_retain_oom")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl1); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("os.date retain OOM failed: res=%v err=%v", res, err)
		}

		// Also verify a single oversized format string fails before materializing the full output.
		s2 := NewState()
		defer s2.Close()
		cl2, err := s2.LoadChunk([]byte(`
			local fmt = string.rep("%c", 50000)
			local ok, err = pcall(os.date, fmt, 0)
			assert(not ok and string.find(tostring(err), "not enough memory", 1, true),
				"expected OOM on single huge os.date, got " .. tostring(ok) .. "," .. tostring(err))
			return "ok"
		`), "=os_date_single_oom")
		if err != nil {
			t.Fatal(err)
		}
		s2.SetMemoryLimit(262144)
		if res, err := s2.Call(cl2); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("os.date single OOM failed: res=%v err=%v", res, err)
		}
	})

	// 2. utf8.codepoint enforces memLimit before allocating codepoint slices.
	t.Run("utf8_codepoint_prealloc_budget", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local a, b, c = utf8.codepoint("A\xc3\xa9!", 1, 4)
			assert(a == 65 and b == 0xE9 and c == 33)
			local big = string.rep("a", 32768)
			local ok, err = pcall(utf8.codepoint, big, 1, #big)
			assert(not ok and string.find(tostring(err), "not enough memory", 1, true),
				"expected OOM from utf8.codepoint, got " .. tostring(ok) .. "," .. tostring(err))
			local okBad, errBad = pcall(utf8.codepoint, "\xff", 1, 1)
			assert(not okBad and string.find(tostring(errBad), "invalid UTF-8", 1, true),
				"expected invalid UTF-8 error, got " .. tostring(okBad) .. "," .. tostring(errBad))
			return "ok"
		`), "=utf8_codepoint_oom")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(131072)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		res, err := s.Call(cl)
		runtime.ReadMemStats(&after)
		if err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("utf8.codepoint OOM failed: res=%v err=%v", res, err)
		}
		// Previously allocated ~1.16 MB before failing; zero-alloc pass 1 must stay far below 256 KiB.
		if delta := after.TotalAlloc - before.TotalAlloc; delta > 262144 {
			t.Fatalf("utf8.codepoint allocated %d bytes before OOM (expected <= 262144)", delta)
		}
	})

	// 3. package.searchers uses raw integer indexing per PUC-Rio Lua 5.5.1 loadlib.c:629 (lua_rawgeti).
	t.Run("package_searchers_raw_lookup_parity", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local orig = package.searchers
			package.searchers = setmetatable({}, {
				__index = function(_, k)
					if k == 1 then
						return function(m) return function() return "FROM_INDEX:" .. m end end
					end
				end,
			})
			local ok, msg = pcall(require, "test_searchers_raw_parity")
			package.searchers = orig
			assert(not ok and string.find(tostring(msg), "module 'test_searchers_raw_parity' not found:", 1, true),
				"expected raw package.searchers lookup to ignore __index, got " .. tostring(ok) .. "," .. tostring(msg))
			return "ok"
		`), "=searchers_raw_parity")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("package.searchers parity failed: res=%v err=%v", res, err)
		}
	})

	// 4. Native closures expose their actual captured upvalues through debug.getupvalue / setupvalue / upvalueid.
	t.Run("native_closure_upvalues_exposed", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local it = string.gmatch("abc", ".")
			assert(debug.getinfo(it, "u").nups == 3)
			local n1, v1 = debug.getupvalue(it, 1)
			local n2, v2 = debug.getupvalue(it, 2)
			local n3, v3 = debug.getupvalue(it, 3)
			local n4, v4 = debug.getupvalue(it, 4)
			assert(n1 == "" and v1 == "abc", "uv1: " .. tostring(n1) .. "," .. tostring(v1))
			assert(n2 == "" and v2 == ".", "uv2: " .. tostring(n2) .. "," .. tostring(v2))
			assert(n3 == "" and type(v3) == "userdata", "uv3: " .. tostring(n3) .. "," .. type(v3))
			assert(n4 == nil and v4 == nil)
			assert(debug.upvalueid(it, 1) ~= nil and debug.upvalueid(it, 1) ~= debug.upvalueid(it, 2))
			local sn = table.pack(debug.setupvalue(it, 1, "xyz"))
			assert(sn.n == 1 and sn[1] == "", "setupvalue must return single empty name")
			local _, v1new = debug.getupvalue(it, 1)
			assert(v1new == "xyz", "expected updated upvalue 'xyz', got " .. tostring(v1new))

			local wr = coroutine.wrap(function() return 7 end)
			assert(debug.getinfo(wr, "u").nups == 1)
			local wn, wv = debug.getupvalue(wr, 1)
			assert(wn == "" and type(wv) == "thread")
			assert(wr() == 7)

			local f = assert(io.tmpfile())
			local ln = f:lines("l", 5)
			assert(debug.getinfo(ln, "u").nups == 5)
			local _, l1 = debug.getupvalue(ln, 1)
			local _, l2 = debug.getupvalue(ln, 2)
			local _, l3 = debug.getupvalue(ln, 3)
			local _, l4 = debug.getupvalue(ln, 4)
			local _, l5 = debug.getupvalue(ln, 5)
			assert(l1 == f and l2 == 2 and l3 == false and l4 == "l" and l5 == 5)
			f:close()

			local uc = utf8.codes("abc")
			assert(debug.getinfo(uc, "u").nups == 0)
			assert(debug.getupvalue(uc, 1) == nil)
			return "ok"
		`), "=native_upvals")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("native closure upvalues failed: res=%v err=%v", res, err)
		}
	})

	// 5. io.popen flushes buffered open output streams before spawning the child process.
	t.Run("io_popen_flushes_buffered_output", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		fname := filepath.Join(t.TempDir(), "buffered.txt")
		cl, err := s.LoadChunk([]byte(fmt.Sprintf(`
			local fname = %q
			local fh = assert(io.open(fname, "w"))
			assert(fh:setvbuf("full", 4096))
			assert(fh:write("payload"))
			local p = assert(io.popen("exit 0", "r"))
			p:close()
			local r = assert(io.open(fname, "r"))
			local got = r:read("a")
			r:close()
			fh:close()
			assert(got == "payload", "expected 'payload' flushed before io.popen, got " .. string.format("%%q", tostring(got)))
			return "ok"
		`, fname)), "=popen_flush")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("io.popen flush failed: res=%v err=%v", res, err)
		}
	})

	// 6. os.difftime validates integer timestamps and preserves precision above 2^53 without overflow.
	t.Run("os_difftime_integer_precision_and_validation", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			assert(os.difftime(9007199254740993, 9007199254740992) == 1.0)
			assert(math.type(os.difftime(9007199254740993, 9007199254740992)) == "float")
			assert(os.difftime(math.maxinteger, math.maxinteger - 1) == 1.0)
			assert(os.difftime(math.maxinteger - 1, math.maxinteger) == -1.0)
			local dmin = os.difftime(math.mininteger, math.maxinteger)
			local dmax = os.difftime(math.maxinteger, math.mininteger)
			assert(dmin < -1e19 and dmin > -2e19 and dmax == -dmin)
			assert(os.difftime("10", "3") == 7.0)
			assert(os.difftime(10.0, 3.0) == 7.0)
			assert(os.difftime(5, 5) == 0.0)

			local ok1, e1 = pcall(os.difftime, 1.5, 1)
			assert(not ok1 and string.find(tostring(e1), "no integer representation", 1, true), "got " .. tostring(e1))
			local ok2, e2 = pcall(os.difftime, 1, 1.5)
			assert(not ok2 and string.find(tostring(e2), "no integer representation", 1, true), "got " .. tostring(e2))
			local ok3, e3 = pcall(os.difftime, math.huge, 1)
			assert(not ok3 and string.find(tostring(e3), "no integer representation", 1, true), "got " .. tostring(e3))
			local ok4, e4 = pcall(os.difftime, 10)
			assert(not ok4 and string.find(tostring(e4), "number expected", 1, true), "got " .. tostring(e4))
			return "ok"
		`), "=os_difftime_precision")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("os.difftime precision failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound17VMRegressions(t *testing.T) {
	// 1. Multi-format file:read roots accumulated strings on stack across subsequent reads.
	t.Run("file_read_roots_accumulated_results_under_memlimit", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "big.bin")
		// 512 KiB of data so reading 16 x 32 KiB exceeds the 128 KiB memory limit.
		if err := os.WriteFile(path, bytes.Repeat([]byte("A"), 512*1024), 0o600); err != nil {
			t.Fatal(err)
		}
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(fmt.Sprintf(`
			local f = assert(io.open(%q, "rb"))
			local a, b = f:read(4, 4)
			assert(a == "AAAA" and b == "AAAA")
			local fmts = {}
			for i = 1, 16 do fmts[i] = 32768 end
			local ok, err = pcall(function()
				return f:read(table.unpack(fmts))
			end)
			f:close()
			assert(not ok and string.find(tostring(err), "not enough memory", 1, true),
				"expected OOM on multi-format file:read, got " .. tostring(ok) .. "," .. tostring(err))
			return "ok"
		`, path)), "=read_root_oom")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(131072)
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("multi-format file:read rooting failed: res=%v err=%v", res, err)
		}
	})

	// 2. string.unpack checks memLimit before allocating unpacked result slices.
	t.Run("string_unpack_bounds_alloc_before_decoding", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local x, y, nxt = string.unpack("<I2c3", string.pack("<I2c3", 42, "xyz"))
			assert(x == 42 and y == "xyz" and nxt == 6)

			local big = string.rep("A", 32768)
			local fmt = string.rep("B", 32768)
			local ok, err = pcall(string.unpack, fmt, big)
			assert(not ok and string.find(tostring(err), "not enough memory", 1, true),
				"expected OOM from string.unpack, got " .. tostring(ok) .. "," .. tostring(err))
			return "ok"
		`), "=unpack_prealloc_oom")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(131072)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		res, err := s.Call(cl)
		runtime.ReadMemStats(&after)
		if err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("string.unpack OOM failed: res=%v err=%v", res, err)
		}
		// Previously allocated ~6.7 MB before failing; zero-alloc pass 1 must stay well under 512 KiB.
		if delta := after.TotalAlloc - before.TotalAlloc; delta > 524288 {
			t.Fatalf("string.unpack allocated %d bytes before OOM (expected <= 524288)", delta)
		}
	})

	// 3. math.modf returns integer subtype when the integral part fits in lua_Integer (lmathlib.c:155 pushnumint).
	t.Run("math_modf_lua55_integer_part_parity", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local i1, f1 = math.modf(-0.5)
			assert(i1 == 0 and math.type(i1) == "integer" and 1/i1 == math.huge and f1 == -0.5 and math.type(f1) == "float")
			local i2, f2 = math.modf(3.5)
			assert(i2 == 3 and math.type(i2) == "integer" and f2 == 0.5 and math.type(f2) == "float")
			local i3, f3 = math.modf(-3.5)
			assert(i3 == -3 and math.type(i3) == "integer" and f3 == -0.5 and math.type(f3) == "float")
			local i4, f4 = math.modf(1e20)
			assert(i4 == 1e20 and math.type(i4) == "float" and f4 == 0.0)
			local i5, f5 = math.modf(math.huge)
			assert(i5 == math.huge and math.type(i5) == "float" and f5 == 0.0)
			return "ok"
		`), "=modf_parity")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("math.modf parity failed: res=%v err=%v", res, err)
		}
	})

	// 4. table.sort numeric fast paths honor instruction limits and context cancellation.
	t.Run("table_sort_numeric_fastpath_honors_execution_controls", func(t *testing.T) {
		// Verify correctness of cancelable introsort on both integer and float arrays.
		sOK := NewState()
		defer sOK.Close()
		sOK.SetInstructionLimit(50_000_000)
		clOK, err := sOK.LoadChunk([]byte(`
			local ti, tf = {}, {}
			for i = 1, 2000 do
				ti[i] = (i * 7919) % 2000
				tf[i] = ((i * 7919) % 2000) + 0.5
			end
			table.sort(ti)
			table.sort(tf)
			for i = 2, 2000 do
				assert(ti[i-1] <= ti[i], "int array unsorted at " .. i)
				assert(tf[i-1] <= tf[i], "float array unsorted at " .. i)
			end
			return "ok"
		`), "=sort_cancelable_ok")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := sOK.Call(clOK); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("cancelable sort correctness failed: res=%v err=%v", res, err)
		}
		if sOK.InstructionsExecuted() <= 0 {
			t.Fatalf("expected non-zero instructions counted during bounded sort")
		}

		// Verify instruction limit aborts sorting a large integer array quickly.
		for _, kind := range []string{"int", "float"} {
			s := NewState()
			defer s.Close()
			tTbl := s.NewTable(100000, 0)
			for i := int64(1); i <= 100000; i++ {
				if kind == "int" {
					tTbl.RawSetInt(s, i, IntValue(100001-i))
				} else {
					tTbl.RawSetInt(s, i, FloatValue(float64(100001-i)+0.25))
				}
			}
			s.SetGlobal("arr", TableValue(tTbl))
			clSort, err := s.LoadChunk([]byte(`table.sort(arr)`), "=sort_abort")
			if err != nil {
				t.Fatal(err)
			}
			s.SetInstructionLimit(100)
			_, err = s.Call(clSort)
			if !errors.Is(err, ErrInstructionLimit) {
				t.Fatalf("%s sort expected ErrInstructionLimit, got %v", kind, err)
			}
			if s.InstructionsExecuted() <= 100 {
				t.Fatalf("%s sort expected InstructionsExecuted > 100, got %d", kind, s.InstructionsExecuted())
			}

			// Verify expired context cancels sort promptly.
			s.SetInstructionLimit(0)
			ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			defer cancel()
			time.Sleep(time.Millisecond)
			s.SetContext(ctx)
			_, err = s.Call(clSort)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s sort expected context.DeadlineExceeded, got %v", kind, err)
			}
		}
	})

	// 5. warn streams directly without building unbounded intermediate strings.
	t.Run("warn_streams_without_intermediate_allocation", func(t *testing.T) {
		var buf bytes.Buffer
		s := NewState()
		defer s.Close()
		s.SetErrOutput(&buf)
		clFmt, err := s.LoadChunk([]byte(`
			warn("ignored")
			warn("@on")
			warn("hello ", 123, "!")
			warn("@off")
			warn("ignored2")
			return "ok"
		`), "=warn_fmt")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(clFmt); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("warn formatting failed: res=%v err=%v", res, err)
		}
		if got := buf.String(); got != "Lua warning: hello 123!\n" {
			t.Fatalf("unexpected warn output: %q", got)
		}

		// Calling warn with 200 copies of a 256 KiB string (~50 MiB total) must not allocate a 50 MiB buffer.
		s2 := NewState()
		defer s2.Close()
		s2.SetErrOutput(io.Discard)
		s2.SetGlobal("big", StringValue(s2.Intern(strings.Repeat("x", 256*1024))))
		clBig, err := s2.LoadChunk([]byte(`
			local args = {}
			for i = 1, 200 do args[i] = big end
			warn(table.unpack(args))
			warn("@on")
			warn(table.unpack(args))
			return "ok"
		`), "=warn_big")
		if err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		res, err := s2.Call(clBig)
		runtime.ReadMemStats(&after)
		if err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("warn big failed: res=%v err=%v", res, err)
		}
		if delta := after.TotalAlloc - before.TotalAlloc; delta > 2*1024*1024 {
			t.Fatalf("warn allocated %d bytes for 50 MiB streamed args (expected <= 2 MiB)", delta)
		}
	})
}

func TestCodexReviewRound18VMRegressions(t *testing.T) {
	// 1. Obsolete Table.inlineSlots are cleared when growing record slots or demoting to dict.
	t.Run("table_inline_slots_cleared_on_growth_and_demotion", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local kept = {}
			for i = 1, 60 do
				local t = {}
				t.a = string.rep(string.char(65 + (i % 26)), 32768)
				t.b = 1
				t.c = 2
				t.a = nil
				kept[i] = t
			end
			-- Also exercise demotion from inline record slots to dictionary via long string key.
			local d = {}
			d.a = string.rep("Z", 32768)
			d[string.rep("k", 64)] = 9
			d.a = nil
			kept[#kept + 1] = d
			_G._kept_inline_tables = kept
			return #kept
		`), "=inline_slots_clear")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].Int() != 61 {
			t.Fatalf("Call failed: res=%v err=%v", res, err)
		}
		keptVal := s.GetGlobal("_kept_inline_tables")
		if !keptVal.IsTable() {
			t.Fatalf("expected kept table")
		}
		keptTbl := keptVal.AsTable()
		for i := int64(1); i <= 61; i++ {
			tv := keptTbl.RawGetInt(i)
			if !tv.IsTable() {
				t.Fatalf("entry %d not a table", i)
			}
			tbl := tv.AsTable()
			if (!tbl.inlineSlots[0].IsNil() && tbl.inlineSlots[0].ptr != nil) || (!tbl.inlineSlots[1].IsNil() && tbl.inlineSlots[1].ptr != nil) {
				t.Fatalf("table %d retained stale inlineSlots pointer after growth/demotion: %v", i, tbl.inlineSlots)
			}
		}
	})

	// 2. Converted captures are rooted across subsequent captureToValue allocations in match/find/gmatch/gsub.
	t.Run("string_captures_rooted_across_allocation_checks", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local subj = string.rep("x", 32768) .. ";"
			local pat = string.rep("(", 16) .. "x+" .. string.rep(")", 16)

			local ok1, e1 = pcall(string.match, subj, pat)
			assert(not ok1 and string.find(tostring(e1), "not enough memory", 1, true),
				"expected match OOM, got " .. tostring(ok1) .. "," .. tostring(e1))

			local ok2, e2 = pcall(string.find, subj, pat)
			assert(not ok2 and string.find(tostring(e2), "not enough memory", 1, true),
				"expected find OOM, got " .. tostring(ok2) .. "," .. tostring(e2))

			local ok3, e3 = pcall(function()
				local it = string.gmatch(subj, pat)
				return it()
			end)
			assert(not ok3 and string.find(tostring(e3), "not enough memory", 1, true),
				"expected gmatch OOM, got " .. tostring(ok3) .. "," .. tostring(e3))

			local ok4, e4 = pcall(string.gsub, subj, pat, function(...) return "ok" end)
			assert(not ok4 and string.find(tostring(e4), "not enough memory", 1, true),
				"expected gsub OOM, got " .. tostring(ok4) .. "," .. tostring(e4))

			-- Small multi-capture patterns within budget still work normally.
			local a, b = string.match("hello world", "(%w+) (%w+)")
			assert(a == "hello" and b == "world")
			return "ok"
		`), "=capture_root_oom")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(131072)
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("capture rooting OOM failed: res=%v err=%v", res, err)
		}
	})

	// 3. coroutine.close() inside an xpcall message handler propagates immediately and runs once.
	t.Run("xpcall_handler_coro_close_self_terminates_cleanly", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local calls = 0
			local closed = false
			local co = coroutine.create(function()
				local guard <close> = setmetatable({}, {
					__close = function() closed = true end,
				})
				return pcall(function()
					return xpcall(function()
						error("boom")
					end, function(err)
						calls = calls + 1
						assert(calls == 1, "handler must not re-enter on self-close")
						coroutine.close(coroutine.running())
						return "unreachable"
					end)
				end)
			end)
			local ok, r = coroutine.resume(co)
			assert(ok == true and r == nil, "expected resume true, nil; got " .. tostring(ok) .. "," .. tostring(r))
			assert(coroutine.status(co) == "dead")
			assert(calls == 1, "expected handler called once, got " .. tostring(calls))
			assert(closed == true, "expected <close> var finalized on self-close")
			-- Main thread must remain completely unaffected afterwards.
			local sum = 0
			for i = 1, 10 do sum = sum + i end
			assert(sum == 55)
			return "ok"
		`), "=xpcall_self_close")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("xpcall self-close failed: res=%v err=%v", res, err)
		}
	})

	// 4. Surviving strings in weak-value, weak-key, and all-weak tables are counted in liveBytes.
	t.Run("weak_tables_account_for_surviving_strings", func(t *testing.T) {
		for _, mode := range []string{"v", "k", "kv"} {
			s := NewState()
			defer s.Close()
			s.SetMemoryLimit(131072)
			cl, err := s.LoadChunk([]byte(fmt.Sprintf(`
				local w = setmetatable({}, { __mode = %q })
				local ok, err = pcall(function()
					for i = 1, 40 do
						local str = string.rep(string.char(65 + (i %% 26)), 32768) .. tostring(i)
						if %q == "k" then
							w[str] = i
						else
							w[i] = str
						end
						str = nil
						collectgarbage("collect")
					end
				end)
				w = nil
				collectgarbage("collect")
				assert(not ok and string.find(tostring(err), "not enough memory", 1, true),
					"expected OOM for __mode=%s weak table strings, got " .. tostring(ok) .. "," .. tostring(err))
				return "ok"
			`, mode, mode, mode)), "=weak_strings_"+mode)
			if err != nil {
				t.Fatal(err)
			}
			if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
				t.Fatalf("weak table __mode=%s string accounting failed: res=%v err=%v", mode, res, err)
			}
		}

		// Verify dead collectable keys/values in all-weak tables are still cleared and do NOT pin strings.
		s2 := NewState()
		defer s2.Close()
		s2.SetMemoryLimit(131072)
		cl2, err := s2.LoadChunk([]byte(`
			local w = setmetatable({}, { __mode = "kv" })
			for i = 1, 20 do
				w[{}] = string.rep(string.char(65 + (i % 26)), 32768) .. tostring(i)
				collectgarbage("collect")
			end
			assert(next(w) == nil, "dead table keys in __mode='kv' must still be cleared")
			return "ok"
		`), "=weak_kv_dead_keys_cleared")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s2.Call(cl2); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("weak kv dead keys check failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound19VMRegressions(t *testing.T) {
	// 1. AddFS streams modules through LoadChunkReader so oversized modules fail before full allocation.
	t.Run("addfs_streams_module_under_memlimit", func(t *testing.T) {
		fsys := fstest.MapFS{
			"small.lua": {Data: []byte("return 42\n")},
			"comments.lua": {Data: []byte(
				strings.Repeat("-- "+strings.Repeat("c", 1020)+"\n", 512) + "return 77\n",
			)},
			"huge.lua": {Data: []byte(`return "` + strings.Repeat("a", 2*1024*1024) + `"`)},
		}
		s := NewState()
		defer s.Close()
		s.AddFS(fsys)
		s.SetMemoryLimit(131072)

		clOK, err := s.LoadChunk([]byte(`
			assert(require("small") == 42)
			assert(require("comments") == 77)
			return "ok"
		`), "=addfs_ok")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(clOK); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("AddFS small/streaming comment modules failed: res=%v err=%v", res, err)
		}

		clHuge, err := s.LoadChunk([]byte(`
			local ok, err = pcall(require, "huge")
			assert(not ok and string.find(tostring(err), "not enough memory", 1, true),
				"expected OOM requiring huge FS module, got " .. tostring(ok) .. "," .. tostring(err))
			return "ok"
		`), "=addfs_huge")
		if err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		res, err := s.Call(clHuge)
		runtime.ReadMemStats(&after)
		if err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("AddFS huge module OOM check failed: res=%v err=%v", res, err)
		}
		// Previously allocated ~2.1 MB via fs.ReadFile; streaming must stay far below 512 KiB.
		if delta := after.TotalAlloc - before.TotalAlloc; delta > 524288 {
			t.Fatalf("AddFS huge module allocated %d bytes before OOM (expected <= 524288)", delta)
		}
	})

	// 2. Internally retained package table stays rooted when global package and package.loaded.package are cleared.
	t.Run("package_table_traced_when_globals_cleared", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local finalized = false
			do
				local sentinel = setmetatable({ tag = "alive" }, {
					__gc = function() finalized = true end,
				})
				table.insert(package.searchers, 1, function(name)
					if name == "gc_pkg_root_test" then
						return function() return sentinel end
					end
				end)
			end
			package.loaded.package = nil
			package = nil
			collectgarbage("collect")
			collectgarbage("collect")
			assert(finalized == false, "captured sentinel in package.searchers was prematurely finalized")
			local mod = require("gc_pkg_root_test")
			assert(mod.tag == "alive" and finalized == false)
			return "ok"
		`), "=pkg_gc_root")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("package GC root check failed: res=%v err=%v", res, err)
		}
	})

	// 3. Emergency GC clears unreachable weak-table objects even when collectgarbage("stop") is active,
	//    and keeps pending-finalizer objects accounted so weak tables cannot bypass memLimit.
	t.Run("emergency_gc_handles_weak_tables_when_stopped", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(131072)
		cl, err := s.LoadChunk([]byte(`
			collectgarbage("stop")
			local w = setmetatable({}, { __mode = "v" })
			for i = 1, 100 do
				w[i] = { string.rep(string.char(65 + (i % 26)), 32768) .. tostring(i) }
			end
			local alive = 0
			for _ in pairs(w) do alive = alive + 1 end
			assert(alive <= 5, "expected emergency GC to clear dead weak entries, found " .. tostring(alive))
			return "ok"
		`), "=emerg_weak_clear")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("emergency weak clear failed: res=%v err=%v", res, err)
		}

		// With pending __gc finalizers (which cannot run during emergency GC), liveBytes must still count them.
		s2 := NewState()
		defer s2.Close()
		s2.SetMemoryLimit(131072)
		cl2, err := s2.LoadChunk([]byte(`
			collectgarbage("stop")
			local w = setmetatable({}, { __mode = "v" })
			local mt = { __gc = function() end }
			local ok, err = pcall(function()
				for i = 1, 40 do
					w[i] = setmetatable({ string.rep(string.char(65 + (i % 26)), 32768) .. tostring(i) }, mt)
				end
			end)
			assert(not ok and string.find(tostring(err), "not enough memory", 1, true),
				"expected OOM when pending-__gc weak values exceed memLimit, got " .. tostring(ok) .. "," .. tostring(err))
			return "ok"
		`), "=emerg_weak_fnz_oom")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s2.Call(cl2); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("emergency weak pending-__gc accounting failed: res=%v err=%v", res, err)
		}
	})

	// 4. Catchable errors in resumed coroutine return hooks unwind through enclosing pcall/xpcall.
	t.Run("coroutine_resumed_return_hook_unwinds_through_pcall_and_xpcall", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local co1 = coroutine.create(function()
				return pcall(function()
					coroutine.yield("mid")
					return "after"
				end)
			end)
			local ok1a, v1a = coroutine.resume(co1)
			assert(ok1a == true and v1a == "mid")
			debug.sethook(co1, function()
				debug.sethook(co1)
				error("hookfail")
			end, "r")
			local ok1b, pok1, perr1 = coroutine.resume(co1)
			assert(ok1b == true and pok1 == false and string.find(tostring(perr1), "hookfail", 1, true),
				"pcall resume got: " .. tostring(ok1b) .. "," .. tostring(pok1) .. "," .. tostring(perr1))

			local co2 = coroutine.create(function()
				return xpcall(function()
					coroutine.yield("mid2")
					return "after2"
				end, function(e) return "MSGH:" .. tostring(e) end)
			end)
			local ok2a, v2a = coroutine.resume(co2)
			assert(ok2a == true and v2a == "mid2")
			debug.sethook(co2, function()
				debug.sethook(co2)
				error("hookfail2")
			end, "r")
			local ok2b, pok2, perr2 = coroutine.resume(co2)
			assert(ok2b == true and pok2 == false and string.find(tostring(perr2), "MSGH:", 1, true) and string.find(tostring(perr2), "hookfail2", 1, true),
				"xpcall resume got: " .. tostring(ok2b) .. "," .. tostring(pok2) .. "," .. tostring(perr2))

			-- Unprotected coroutine still fails resume and becomes dead.
			local co3 = coroutine.create(function()
				coroutine.yield("mid3")
			end)
			assert(coroutine.resume(co3))
			debug.sethook(co3, function()
				debug.sethook(co3)
				error("unprotected_hook")
			end, "r")
			local ok3, err3 = coroutine.resume(co3)
			assert(ok3 == false and string.find(tostring(err3), "unprotected_hook", 1, true) and coroutine.status(co3) == "dead")
			return "ok"
		`), "=coro_rethook_pcall")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("coroutine resumed return-hook unwind failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound20VMRegressions(t *testing.T) {
	// 1. Binary string and byte-slice chunk loading enforces memLimit during undump before allocating huge slices.
	t.Run("binary_string_and_bytes_undump_enforces_memlimit", func(t *testing.T) {
		bigProto, err := compiler.Compile("=bigconst", "return")
		if err != nil {
			t.Fatal(err)
		}
		bigProto.Consts = make([]bytecode.Constant, 32768)
		binBytes, err := bytecode.Dump(bigProto, true)
		if err != nil {
			t.Fatal(err)
		}

		// Via State.LoadChunk([]byte)
		s1 := NewState()
		defer s1.Close()
		s1.SetMemoryLimit(131072)
		runtime.GC()
		var b1, a1 runtime.MemStats
		runtime.ReadMemStats(&b1)
		_, lerr := s1.LoadChunk(binBytes, "=bigconst_bytes")
		runtime.ReadMemStats(&a1)
		if lerr == nil || !strings.Contains(lerr.Error(), "not enough memory") {
			t.Fatalf("expected OOM from LoadChunk(32768 consts), got %v", lerr)
		}
		if delta := a1.TotalAlloc - b1.TotalAlloc; delta > 524288 {
			t.Fatalf("LoadChunk allocated %d bytes before OOM (expected <= 524288)", delta)
		}

		// Via Lua load(binaryString, ..., "b")
		s2 := NewState()
		defer s2.Close()
		s2.SetGlobal("bin", StringValue(s2.Intern(string(binBytes))))
		cl, err := s2.LoadChunk([]byte(`
			local fn, err = load(bin, "=bigconst_str", "b")
			assert(fn == nil and string.find(tostring(err), "not enough memory", 1, true),
				"expected OOM from load(binStr), got " .. tostring(fn) .. "," .. tostring(err))
			return "ok"
		`), "=load_bin_oom")
		if err != nil {
			t.Fatal(err)
		}
		s2.SetMemoryLimit(131072)
		runtime.GC()
		var b2, a2 runtime.MemStats
		runtime.ReadMemStats(&b2)
		res, err := s2.Call(cl)
		runtime.ReadMemStats(&a2)
		if err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("load(binStr) OOM check failed: res=%v err=%v", res, err)
		}
		if delta := a2.TotalAlloc - b2.TotalAlloc; delta > 524288 {
			t.Fatalf("load(binStr) allocated %d bytes before OOM (expected <= 524288)", delta)
		}
	})

	// 2. utf8.len and utf8.offset poll instruction limits and context cancellation during scans.
	t.Run("utf8_len_and_offset_honor_execution_controls", func(t *testing.T) {
		big := strings.Repeat("a", 2*1024*1024)
		for _, tc := range []struct {
			name string
			src  string
		}{
			{"utf8_len", `return utf8.len(big)`},
			{"utf8_offset", `return utf8.offset(big, 500000, 1)`},
		} {
			s := NewState()
			defer s.Close()
			s.SetGlobal("big", StringValue(s.Intern(big)))
			cl, err := s.LoadChunk([]byte(tc.src), "="+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			s.SetInstructionLimit(100)
			_, err = s.Call(cl)
			if !errors.Is(err, ErrInstructionLimit) {
				t.Fatalf("%s expected ErrInstructionLimit, got %v", tc.name, err)
			}
			if s.InstructionsExecuted() <= 100 {
				t.Fatalf("%s expected InstructionsExecuted > 100, got %d", tc.name, s.InstructionsExecuted())
			}

			s.SetInstructionLimit(0)
			ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			defer cancel()
			time.Sleep(time.Millisecond)
			s.SetContext(ctx)
			_, err = s.Call(cl)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s expected context.DeadlineExceeded, got %v", tc.name, err)
			}
		}
	})

	// 3. coroutine.close() inside synchronous __close during native-callback unwind terminates the coroutine cleanly.
	t.Run("sync_close_coro_close_self_terminates_cleanly", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local log = {}
			local co = coroutine.create(function()
				local outer <close> = setmetatable({}, {
					__close = function() log[#log + 1] = "outer" end,
				})
				local ok, err = pcall(string.gsub, "x", ".", function()
					local g1 <close> = setmetatable({}, {
						__close = function() log[#log + 1] = "g1" end,
					})
					local g2 <close> = setmetatable({}, {
						__close = function()
							log[#log + 1] = "g2"
							coroutine.close(coroutine.running())
						end,
					})
					error("inner")
				end)
				log[#log + 1] = "after:" .. tostring(ok) .. ":" .. tostring(err)
			end)
			local rok, rval = coroutine.resume(co)
			assert(rok == true and rval == nil, "expected resume true, nil; got " .. tostring(rok) .. "," .. tostring(rval))
			assert(coroutine.status(co) == "dead")
			assert(table.concat(log, ",") == "g2,g1,outer", "unexpected close log: " .. table.concat(log, ","))
			return "ok"
		`), "=sync_close_self")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("sync close self-close failed: res=%v err=%v", res, err)
		}
	})

	// 4. Nested synchronous pcall/xpcall (including tailcalls and yields) complete outer protected frames properly.
	t.Run("nested_pcall_and_xpcall_complete_outer_frames", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local a, b, c = pcall(pcall, type, 1)
			assert(a == true and b == true and c == "number", "pcall(pcall, type, 1) got " .. tostring(a) .. "," .. tostring(b) .. "," .. tostring(c))

			local x1, x2, x3 = xpcall(pcall, function(e) return e end, type, 1)
			assert(x1 == true and x2 == true and x3 == "number", "xpcall(pcall, msgh, type, 1) got " .. tostring(x1) .. "," .. tostring(x2) .. "," .. tostring(x3))

			local p1, p2, p3, p4 = pcall(pcall, pcall, type, 1)
			assert(p1 == true and p2 == true and p3 == true and p4 == "number")

			local e1, e2, e3 = pcall(xpcall, error, function(e) return "H:" .. tostring(e) end, "boom")
			assert(e1 == true and e2 == false and e3 == "H:boom", "pcall(xpcall, error) got " .. tostring(e1) .. "," .. tostring(e2) .. "," .. tostring(e3))

			local t1, t2, t3 = pcall(function() return pcall(type, 1) end)
			assert(t1 == true and t2 == true and t3 == "number", "tailcall nested pcall got " .. tostring(t1) .. "," .. tostring(t2) .. "," .. tostring(t3))

			local co = coroutine.create(function()
				return pcall(pcall, coroutine.yield, "hi")
			end)
			local ok1, yv = coroutine.resume(co)
			assert(ok1 == true and yv == "hi")
			local ok2, r1, r2, r3 = coroutine.resume(co, "res")
			assert(ok2 == true and r1 == true and r2 == true and r3 == "res",
				"yield nested pcall got " .. tostring(ok2) .. "," .. tostring(r1) .. "," .. tostring(r2) .. "," .. tostring(r3))
			return "ok"
		`), "=nested_pcall")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("nested pcall/xpcall failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound21VMRegressions(t *testing.T) {
	// 1. table.insert / table.remove fast paths honor instruction limits and context timeouts
	// even when invoked directly through State.Call without executing Lua bytecode.
	t.Run("table_insert_and_remove_fast_path_honor_execution_controls", func(t *testing.T) {
		const n = 500_000
		for _, op := range []string{"insert", "remove"} {
			s := NewState()
			defer s.Close()
			tbl := s.NewTable(n, 0)
			for i := int64(1); i <= n; i++ {
				tbl.RawSetInt(s, i, IntValue(i))
			}
			tableLib := s.GetGlobal("table").AsTable()
			fn := tableLib.RawGetStr(s.Intern(op))
			callArgs := []Value{TableValue(tbl), IntValue(1)}
			if op == "insert" {
				callArgs = append(callArgs, IntValue(0))
			}

			s.SetInstructionLimit(2000)
			_, err := s.Call(fn, callArgs...)
			if !errors.Is(err, ErrInstructionLimit) {
				t.Fatalf("table.%s expected ErrInstructionLimit, got %v", op, err)
			}
			if s.InstructionsExecuted() <= 2000 {
				t.Fatalf("table.%s expected InstructionsExecuted > 2000, got %d", op, s.InstructionsExecuted())
			}

			// Context timeout without instruction limit aborts promptly rather than shifting the full array.
			s.SetInstructionLimit(0)
			s.ResetInstructionCount()
			bigTbl := s.NewTable(2_000_000, 0)
			for i := int64(1); i <= 2_000_000; i++ {
				bigTbl.RawSetInt(s, i, IntValue(i))
			}
			bigArgs := []Value{TableValue(bigTbl), IntValue(1)}
			if op == "insert" {
				bigArgs = append(bigArgs, IntValue(0))
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			defer cancel()
			s.SetContext(ctx)
			_, err = s.Call(fn, bigArgs...)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("table.%s expected context.DeadlineExceeded, got %v", op, err)
			}
		}

		// Small table.insert/remove must NOT falsely fail a modest instruction limit.
		s := NewState()
		defer s.Close()
		s.SetInstructionLimit(500)
		cl, err := s.LoadChunk([]byte(`
			local t = {10, 20, 30}
			table.insert(t, 1, 5)
			assert(#t == 4 and t[1] == 5 and t[2] == 10 and t[4] == 30)
			local r = table.remove(t, 1)
			assert(r == 5 and #t == 3 and t[1] == 10 and t[3] == 30)
			return "ok"
		`), "=small_table_ops")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("small table.insert/remove under insnLimit=500 failed: res=%v err=%v", res, err)
		}
	})

	// 2. Bulk string and os.date native loops also honor instruction limits and context cancellation.
	t.Run("bulk_string_and_date_loops_honor_execution_controls", func(t *testing.T) {
		bigStr := strings.Repeat("AbCdEfGh", 128*1024) // 1 MiB
		bigFmt := strings.Repeat("%Y", 200_000)
		cases := []struct {
			name string
			fn   func(s *State) (Value, []Value)
		}{
			{
				name: "string.rep_plain",
				fn: func(s *State) (Value, []Value) {
					return s.GetGlobal("string").AsTable().RawGetStr(s.Intern("rep")),
						[]Value{StringValue(s.Intern("a")), IntValue(2_000_000)}
				},
			},
			{
				name: "string.rep_sep",
				fn: func(s *State) (Value, []Value) {
					return s.GetGlobal("string").AsTable().RawGetStr(s.Intern("rep")),
						[]Value{StringValue(s.Intern("a")), IntValue(2_000_000), StringValue(s.Intern(","))}
				},
			},
			{
				name: "string.byte",
				fn: func(s *State) (Value, []Value) {
					return s.GetGlobal("string").AsTable().RawGetStr(s.Intern("byte")),
						[]Value{StringValue(s.Intern(bigStr)), IntValue(1), IntValue(500_000)}
				},
			},
			{
				name: "string.reverse",
				fn: func(s *State) (Value, []Value) {
					return s.GetGlobal("string").AsTable().RawGetStr(s.Intern("reverse")),
						[]Value{StringValue(s.Intern(bigStr))}
				},
			},
			{
				name: "string.lower",
				fn: func(s *State) (Value, []Value) {
					return s.GetGlobal("string").AsTable().RawGetStr(s.Intern("lower")),
						[]Value{StringValue(s.Intern(bigStr))}
				},
			},
			{
				name: "string.upper",
				fn: func(s *State) (Value, []Value) {
					return s.GetGlobal("string").AsTable().RawGetStr(s.Intern("upper")),
						[]Value{StringValue(s.Intern(bigStr))}
				},
			},
			{
				name: "os.date",
				fn: func(s *State) (Value, []Value) {
					return s.GetGlobal("os").AsTable().RawGetStr(s.Intern("date")),
						[]Value{StringValue(s.Intern(bigFmt)), IntValue(1_700_000_000)}
				},
			},
		}
		for _, tc := range cases {
			s := NewState()
			defer s.Close()
			fn, args := tc.fn(s)
			s.SetInstructionLimit(100)
			_, err := s.Call(fn, args...)
			if !errors.Is(err, ErrInstructionLimit) {
				t.Fatalf("%s expected ErrInstructionLimit, got %v", tc.name, err)
			}
			if s.InstructionsExecuted() <= 100 {
				t.Fatalf("%s expected InstructionsExecuted > 100, got %d", tc.name, s.InstructionsExecuted())
			}

			s.SetInstructionLimit(0)
			s.ResetInstructionCount()
			ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			defer cancel()
			time.Sleep(time.Millisecond)
			s.SetContext(ctx)
			_, err = s.Call(fn, args...)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s expected context.DeadlineExceeded, got %v", tc.name, err)
			}
		}

		// Verify normal outputs under active instruction limit (correctness of cancelable paths).
		s := NewState()
		defer s.Close()
		s.SetInstructionLimit(500_000)
		cl, err := s.LoadChunk([]byte(`
			assert(string.rep("ab", 3) == "ababab")
			assert(string.rep("ab", 3, "-") == "ab-ab-ab")
			assert(string.rep("", 1000000) == "")
			assert(string.reverse("hello") == "olleh")
			assert(string.lower("AbC123") == "abc123")
			assert(string.upper("AbC123") == "ABC123")
			local b1, b2, b3 = string.byte("XYZ", 1, 3)
			assert(b1 == 88 and b2 == 89 and b3 == 90)
			assert(string.char(88, 89, 90) == "XYZ")
			return "ok"
		`), "=small_string_ops")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("small string ops under insnLimit failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound22VMRegressions(t *testing.T) {
	// 1. string.packsize / pack / unpack / format honor instruction limits and context timeouts
	// even on formats with no-output options (spaces, endianness, %%, literals).
	t.Run("packsize_pack_unpack_format_honor_execution_controls", func(t *testing.T) {
		bigSpaces := strings.Repeat(" ", 1024*1024)
		bigPct := strings.Repeat("%%", 512*1024)
		bigLit := strings.Repeat("x", 1024*1024)
		cases := []struct {
			name string
			fn   func(s *State) (Value, []Value)
		}{
			{
				name: "string.packsize_spaces",
				fn: func(s *State) (Value, []Value) {
					return s.GetGlobal("string").AsTable().RawGetStr(s.Intern("packsize")),
						[]Value{StringValue(s.Intern(bigSpaces))}
				},
			},
			{
				name: "string.pack_spaces",
				fn: func(s *State) (Value, []Value) {
					return s.GetGlobal("string").AsTable().RawGetStr(s.Intern("pack")),
						[]Value{StringValue(s.Intern(bigSpaces))}
				},
			},
			{
				name: "string.unpack_spaces",
				fn: func(s *State) (Value, []Value) {
					return s.GetGlobal("string").AsTable().RawGetStr(s.Intern("unpack")),
						[]Value{StringValue(s.Intern(bigSpaces)), StringValue(s.Intern(""))}
				},
			},
			{
				name: "string.format_percent",
				fn: func(s *State) (Value, []Value) {
					return s.GetGlobal("string").AsTable().RawGetStr(s.Intern("format")),
						[]Value{StringValue(s.Intern(bigPct))}
				},
			},
			{
				name: "string.format_literal",
				fn: func(s *State) (Value, []Value) {
					return s.GetGlobal("string").AsTable().RawGetStr(s.Intern("format")),
						[]Value{StringValue(s.Intern(bigLit))}
				},
			},
		}
		for _, tc := range cases {
			s := NewState()
			defer s.Close()
			fn, args := tc.fn(s)
			s.SetInstructionLimit(100)
			_, err := s.Call(fn, args...)
			if !errors.Is(err, ErrInstructionLimit) {
				t.Fatalf("%s expected ErrInstructionLimit, got %v", tc.name, err)
			}
			if s.InstructionsExecuted() <= 100 {
				t.Fatalf("%s expected InstructionsExecuted > 100, got %d", tc.name, s.InstructionsExecuted())
			}

			s.SetInstructionLimit(0)
			s.ResetInstructionCount()
			ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			defer cancel()
			time.Sleep(time.Millisecond)
			s.SetContext(ctx)
			_, err = s.Call(fn, args...)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s expected context.DeadlineExceeded, got %v", tc.name, err)
			}
		}
	})

	// 2 & 3. coroutine.close() inside load readers, Lua debug hooks (including inside pcall),
	// __gc finalizers, and coroutine.create(coroutine.close) terminates the coroutine cleanly.
	t.Run("self_close_through_load_reader_debug_hook_gc_and_native_coro", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			-- (a) load() reader calling coroutine.close() on first chunk
			local log1 = {}
			local co1 = coroutine.create(function()
				local g <close> = setmetatable({}, { __close = function() log1[#log1 + 1] = "closed" end })
				local f, err = load(function() coroutine.close() end)
				log1[#log1 + 1] = "continued:" .. tostring(f) .. ":" .. tostring(err)
				return "bad"
			end)
			local ok1, r1 = coroutine.resume(co1)
			assert(ok1 == true and r1 == nil, "co1 resume got " .. tostring(ok1) .. "," .. tostring(r1))
			assert(coroutine.status(co1) == "dead")
			assert(table.concat(log1, ",") == "closed", "co1 log=" .. table.concat(log1, ","))

			-- (b) load() reader calling coroutine.close() on second chunk
			local co1b = coroutine.create(function()
				local n = 0
				local f, err = load(function()
					n = n + 1
					if n == 1 then return "return " end
					coroutine.close()
				end)
				return "bad", f, err
			end)
			local ok1b, r1b = coroutine.resume(co1b)
			assert(ok1b == true and r1b == nil, "co1b resume got " .. tostring(ok1b) .. "," .. tostring(r1b))
			assert(coroutine.status(co1b) == "dead")

			-- (c) Lua debug line hook inside pcall calling coroutine.close()
			local log2 = {}
			local co2 = coroutine.create(function()
				local outer <close> = setmetatable({}, { __close = function() log2[#log2 + 1] = "outer" end })
				local ok, err = pcall(function()
					local inner <close> = setmetatable({}, { __close = function() log2[#log2 + 1] = "inner" end })
					debug.sethook(function()
						debug.sethook()
						coroutine.close()
					end, "l")
					local x = 1
					return x
				end)
				log2[#log2 + 1] = "after:" .. tostring(ok) .. ":" .. tostring(err)
				return "bad"
			end)
			local ok2, r2 = coroutine.resume(co2)
			assert(ok2 == true and r2 == nil, "co2 resume got " .. tostring(ok2) .. "," .. tostring(r2))
			assert(coroutine.status(co2) == "dead")
			assert(table.concat(log2, ",") == "inner,outer", "co2 log=" .. table.concat(log2, ","))

			-- (d) coroutine.create(coroutine.close)
			local co3 = coroutine.create(coroutine.close)
			local ok3, r3 = coroutine.resume(co3)
			assert(ok3 == true and r3 == nil, "co3 resume got " .. tostring(ok3) .. "," .. tostring(r3))
			assert(coroutine.status(co3) == "dead")

			-- (e) __gc finalizer calling coroutine.close()
			local co4 = coroutine.create(function()
				local x = setmetatable({}, { __gc = function() coroutine.close() end })
				x = nil
				collectgarbage("collect")
				return "bad"
			end)
			local ok4, r4 = coroutine.resume(co4)
			assert(ok4 == true and r4 == nil, "co4 resume got " .. tostring(ok4) .. "," .. tostring(r4))
			assert(coroutine.status(co4) == "dead")

			return "ok"
		`), "=self_close_paths")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("self-close paths failed: res=%v err=%v", res, err)
		}
	})

	// 4. Failed SetMetatable shape allocation leaves t.meta and t.shape unchanged and caches consistent.
	t.Run("setmetatable_shape_allocation_failure_keeps_cache_consistent", func(t *testing.T) {
		s := NewState()
		defer s.Close()

		clSetup, err := s.LoadChunk([]byte(`
			t = { a = 1 }
			mt = { __index = { x = 42 } }
			function getWarmed(tbl)
				return tbl.x
			end
			-- Warm inline cache for absent field t.x on plain table shape.
			assert(getWarmed(t) == nil)
			assert(getWarmed(t) == nil)
		`), "=setup_mt_oom")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(clSetup); err != nil {
			t.Fatal(err)
		}

		clFail, err := s.LoadChunk([]byte(`return pcall(setmetatable, t, mt)`), "=fail_mt")
		if err != nil {
			t.Fatal(err)
		}
		// Pin memory limit tightly below current usage + shape allocation cost.
		s.CollectFull()
		s.SetMemoryLimit(s.MemoryUsage() + 1)

		res, err := s.Call(clFail)
		if err != nil {
			t.Fatal(err)
		}
		// Restore ample budget before assertions and fresh chunk compilation.
		s.SetMemoryLimit(0)

		if OptimizeEnabled {
			if len(res) < 2 || res[0].IsTrue() || !strings.Contains(FormatValue(res[1]), "not enough memory") {
				t.Fatalf("expected setmetatable OOM in optimized mode, got %v", res)
			}
			clCheckConsistent, err := s.LoadChunk([]byte(`
				assert(getmetatable(t) == nil, "failed setmetatable must not mutate t.meta")
				local freshGetter = load("return function(tbl) return tbl.x end")()
				assert(getWarmed(t) == nil, "warmed getter expected nil after failed setmetatable")
				assert(freshGetter(t) == nil, "fresh getter expected nil after failed setmetatable")
				-- Now apply metatable with ample budget and verify both caches agree.
				assert(setmetatable(t, mt) == t)
				assert(getWarmed(t) == 42, "warmed getter expected 42 after succeeding setmetatable")
				assert(freshGetter(t) == 42, "fresh getter expected 42 after succeeding setmetatable")
				return "ok"
			`), "=check_mt_consistent")
			if err != nil {
				t.Fatal(err)
			}
			if r, err := s.Call(clCheckConsistent); err != nil || len(r) != 1 || r[0].AsString().S != "ok" {
				t.Fatalf("cache consistency check failed: res=%v err=%v", r, err)
			}
		}

		// Also verify debug.setmetatable checks ConsumeMemErr on shape OOM.
		clSetup2, err := s.LoadChunk([]byte(`t2 = { b = 2 }; mt2 = { __index = { y = 99 } }`), "=setup_dbg_mt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(clSetup2); err != nil {
			t.Fatal(err)
		}
		clDbgFail, err := s.LoadChunk([]byte(`return pcall(debug.setmetatable, t2, mt2)`), "=fail_dbg_mt")
		if err != nil {
			t.Fatal(err)
		}
		s.CollectFull()
		s.SetMemoryLimit(s.MemoryUsage() + 1)
		res2, err := s.Call(clDbgFail)
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(0)
		if OptimizeEnabled {
			if len(res2) < 2 || res2[0].IsTrue() || !strings.Contains(FormatValue(res2[1]), "not enough memory") {
				t.Fatalf("expected debug.setmetatable OOM in optimized mode, got %v", res2)
			}
		}
	})

	// 5. Draining finalizers clears dequeued entries from toBeFnz backing storage so Go GC
	// can reclaim finalized object graphs promptly.
	t.Run("drain_finalizers_clears_backing_array_for_reclamation", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(131072)

		mt := s.NewTable(0, 1)
		mt.RawSetStr(s, s.Intern("__gc"), NativeValue(&NativeFunc{
			Name: "noop_gc",
			Fn:   func(_ *State, _ []Value) ([]Value, error) { return nil, nil },
		}))
		s.SetGlobal("mt", TableValue(mt))

		// First finalize 100 empty tables to establish a large toBeFnz backing array.
		for range 100 {
			tbl := s.NewTable(0, 0)
			s.SetMetatable(tbl, mt)
			_ = s.ConsumeMemErr()
		}
		s.CollectFull()

		// Separately finalize 8 tables containing fresh 32 KiB strings and keep only weak pointers.
		var weakTables []weak.Pointer[Table]
		for i := range 8 {
			tbl := s.NewTable(0, 1)
			payload := strings.Repeat(string(byte('A'+i)), 32768)
			ls, err := s.TryIntern(payload)
			if err != nil {
				t.Fatalf("TryIntern #%d failed: %v", i, err)
			}
			tbl.RawSetStr(s, s.Intern("data"), StringValue(ls))
			if err := s.ConsumeMemErr(); err != nil {
				t.Fatalf("RawSetStr #%d failed: %v", i, err)
			}
			s.SetMetatable(tbl, mt)
			if err := s.ConsumeMemErr(); err != nil {
				t.Fatalf("SetMetatable #%d failed: %v", i, err)
			}
			weakTables = append(weakTables, weak.Make(tbl))
			tbl = nil
			s.CollectFull()
			s.CollectFull()
		}

		if len(s.gc.toBeFnz) != 0 || cap(s.gc.toBeFnz) != 0 {
			t.Fatalf("expected toBeFnz to be cleared after drain, got len=%d cap=%d", len(s.gc.toBeFnz), cap(s.gc.toBeFnz))
		}
		// Trigger Go GC so cleared finalized tables are reclaimed.
		for range 3 {
			runtime.GC()
		}
		alive := 0
		for _, wp := range weakTables {
			if wp.Value() != nil {
				alive++
			}
		}
		if alive != 0 {
			t.Fatalf("expected all 8 finalized tables to be reclaimed by Go GC, %d still pinned", alive)
		}
	})
}

func TestCodexReviewRound23VMRegressions(t *testing.T) {
	// 1. package.searchpath and AddFS searcher enforce memLimit before expanding '?' templates
	// and honor execution controls on large template lists.
	t.Run("package_searchpath_and_searchfs_enforce_memory_budget", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.AddFS(fstest.MapFS{"other.lua": &fstest.MapFile{Data: []byte("return 1")}})
		cl, err := s.LoadChunk([]byte(`
			local bigName = string.rep("x", 32768)
			local bigPath = string.rep("?", 1000)
			local ok1, err1 = pcall(package.searchpath, bigName, bigPath)
			assert(not ok1 and tostring(err1):find("not enough memory", 1, true),
				"searchpath should fail OOM, got ok=" .. tostring(ok1) .. " err=" .. tostring(err1))

			local savedPath = package.path
			package.path = bigPath
			local ok2, err2 = pcall(require, bigName)
			package.path = savedPath
			assert(not ok2 and tostring(err2):find("not enough memory", 1, true),
				"require with huge path should fail OOM, got ok=" .. tostring(ok2) .. " err=" .. tostring(err2))
			return "ok"
		`), "=searchpath_mem")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(131072)
		res, err := s.Call(cl)
		if err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("searchpath memory budget check failed: res=%v err=%v", res, err)
		}

		// Also verify execution controls on many ';' templates.
		s2 := NewState()
		defer s2.Close()
		searchPathFn := s2.GetGlobal("package").AsTable().RawGetStr(s2.Intern("searchpath"))
		manySep := StringValue(s2.Intern(strings.Repeat(";", 200_000)))
		s2.SetInstructionLimit(100)
		_, err = s2.Call(searchPathFn, StringValue(s2.Intern("m")), manySep)
		if !errors.Is(err, ErrInstructionLimit) {
			t.Fatalf("package.searchpath expected ErrInstructionLimit, got %v", err)
		}
	})

	// 2. Caught error() strings with prepended source locations and debug.traceback() respect memLimit.
	t.Run("error_location_and_traceback_account_for_memory_budget", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local big = string.rep("E", 32768)
			local function fail()
				error(big)
			end
			local kept = table.create(100, 0)
			local sawOOM = false
			for i = 1, 100 do
				local ok, e = pcall(fail)
				assert(not ok)
				if e == "not enough memory" then
					sawOOM = true
					break
				end
				kept[i] = e
			end
			kept = nil
			collectgarbage("collect")
			assert(sawOOM, "expected repeated caught error() strings to hit memLimit")

			local keptTB = table.create(100, 0)
			local sawTBOOM = false
			for i = 1, 100 do
				local ok, tb = pcall(debug.traceback, big)
				if not ok then
					sawTBOOM = (tb == "not enough memory")
					break
				end
				keptTB[i] = tb
			end
			keptTB = nil
			collectgarbage("collect")
			assert(sawTBOOM, "expected repeated debug.traceback() strings to hit memLimit")
			return "ok"
		`), "=error_mem")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(163840)
		res, err := s.Call(cl)
		if err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("error/traceback memory budget check failed: res=%v err=%v", res, err)
		}
	})

	// 3. debug.getinfo(2, 'l').currentline inside line hooks honors cf.atCurrentPC.
	t.Run("hook_currentline_honors_at_current_pc", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local mismatches = {}
			local count = 0
			local function target()
				local a = 1
				local b = 2
				local c = a + b
				return c
			end
			debug.sethook(function(ev, line)
				count = count + 1
				local info = debug.getinfo(2, "l")
				if info.currentline ~= line then
					mismatches[#mismatches + 1] = tostring(line) .. "!=" .. tostring(info.currentline)
				end
			end, "l")
			local out = target()
			debug.sethook()
			assert(out == 3 and count >= 4, "expected hook invocations, got count=" .. tostring(count))
			assert(#mismatches == 0, "hook currentline mismatches: " .. table.concat(mismatches, ", "))
			return "ok"
		`), "=hook_currentline")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("hook currentline check failed: res=%v err=%v", res, err)
		}
	})

	// 4. debug.getinfo(2, 'f').func returns the active pcall/xpcall function from the frame
	// even if global pcall/xpcall is removed or overwritten.
	t.Run("protected_call_frame_retains_callee_identity", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local savedP, savedXP = pcall, xpcall
			local p, xp = pcall, xpcall

			-- Direct call after clearing globals
			local ok1, r1 = p(function()
				pcall = nil
				local info = debug.getinfo(2, "nf")
				assert(info.func == p, "pcall frame func mismatch: got " .. tostring(info.func))
				assert(info.name == "p" and info.namewhat == "local",
					"pcall frame name mismatch: " .. tostring(info.name) .. "/" .. tostring(info.namewhat))
				return "p_ok"
			end)
			pcall = savedP
			assert(ok1 and r1 == "p_ok", "pcall check failed: " .. tostring(ok1) .. "," .. tostring(r1))

			local ok2, r2 = xp(function()
				xpcall = nil
				local info = debug.getinfo(2, "nf")
				assert(info.func == xp, "xpcall frame func mismatch: got " .. tostring(info.func))
				assert(info.name == "xp" and info.namewhat == "local",
					"xpcall frame name mismatch: " .. tostring(info.name) .. "/" .. tostring(info.namewhat))
				return "xp_ok"
			end, function(e) return e end)
			xpcall = savedXP
			assert(ok2 and r2 == "xp_ok", "xpcall check failed: " .. tostring(ok2) .. "," .. tostring(r2))

			-- Coroutine created directly with pcall / xpcall
			local co1 = coroutine.create(p)
			pcall = nil
			local cok1, pok1, cres1 = coroutine.resume(co1, function()
				return debug.getinfo(2, "f").func == p
			end)
			pcall = savedP
			assert(cok1 and pok1 and cres1 == true, "coro pcall func identity failed")

			local co2 = coroutine.create(xp)
			xpcall = nil
			local cok2, pok2, cres2 = coroutine.resume(co2, function()
				return debug.getinfo(2, "f").func == xp
			end, function(e) return e end)
			xpcall = savedXP
			assert(cok2 and pok2 and cres2 == true, "coro xpcall func identity failed")
			return "ok"
		`), "=pcall_frame_identity")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("protected call frame identity check failed: res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound24VMRegressions(t *testing.T) {
	t.Run("streamed_local_names_charge_compile_budget", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local piece = string.rep("n", 4096)
			local parts = {}
			for b = 1, 100 do
				parts[#parts + 1] = "do local "
				for k = 1, 8 do parts[#parts + 1] = piece end
				parts[#parts + 1] = " = 1 end "
			end
			local i = 0
			local function reader()
				i = i + 1
				return parts[i]
			end
			local f, e = load(reader)
			assert(f == nil and tostring(e):find("not enough memory", 1, true), "got " .. tostring(e))
			return "ok"
		`), "=locvar_mem")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(131072)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		res, err := s.Call(cl)
		runtime.ReadMemStats(&after)
		if err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
		if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
			t.Fatalf("load allocated %d bytes under a 128 KiB limit", got)
		}
	})

	t.Run("load_polls_execution_controls_during_scan", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		loadFn := s.GetGlobal("load")
		src := StringValue(s.Intern(strings.Repeat(" ", 32<<20)))
		s.SetInstructionLimit(100)
		_, err := s.Call(loadFn, src)
		if !errors.Is(err, ErrInstructionLimit) {
			t.Fatalf("expected ErrInstructionLimit, got %v", err)
		}

		s2 := NewState()
		defer s2.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		time.Sleep(2 * time.Millisecond)
		s2.SetContext(ctx)
		_, err = s2.Call(s2.GetGlobal("load"), StringValue(s2.Intern(strings.Repeat(" ", 32<<20))))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded, got %v", err)
		}
	})
}

func TestAuditAndRound25VMRegressions(t *testing.T) {
	t.Run("p0_forloop_setlocal_type_safety", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function corrupt(bad)
				local n = 0
				for i = 1, 5 do
					n = n + 1
					debug.setlocal(1, 2, bad)
					debug.setlocal(1, 3, bad)
					if n > 2 then break end
				end
				return n
			end
			local function corruptFloat(bad)
				local n = 0
				for i = 1.0, 5.0, 1.0 do
					n = n + 1
					debug.setlocal(1, 2, bad)
					debug.setlocal(1, 3, bad)
					if n > 2 then break end
				end
				return n
			end
			for _, bad in ipairs({"x", {}, true}) do
				pcall(corrupt, bad)
				pcall(corruptFloat, bad)
			end
			return "ok"
		`), "=forloop_safety")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("p0_xpcall_wrap_cstack_overflow", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function f() return coroutine.wrap(f)() end
			local ok, msg = xpcall(f, function() return coroutine.wrap(f)() end)
			assert(ok == false and msg == "error in error handling", "got: " .. tostring(msg))
			return "ok"
		`), "=xpcall_wrap_overflow")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("p0_coro_close_normal_and_no_reentry", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local co
			local sawNormal, closeErr, resumeOk, resumeErr
			co = coroutine.create(function()
				local x <close> = setmetatable({}, {__close = function()
					assert(coroutine.status(co) == "running")
					local c2 = coroutine.create(function()
						sawNormal = coroutine.status(co)
						local ok1, e1 = pcall(coroutine.close, co)
						assert(not ok1)
						closeErr = e1
						resumeOk, resumeErr = coroutine.resume(co)
					end)
					local ok2, e2 = coroutine.resume(c2)
					assert(ok2, tostring(e2))
				end})
				coroutine.yield(1)
			end)
			assert(coroutine.resume(co))
			local ok, msg = coroutine.close(co)
			assert(ok == true and msg == nil, "got " .. tostring(ok) .. " " .. tostring(msg))
			assert(sawNormal == "normal", "sawNormal=" .. tostring(sawNormal))
			assert(tostring(closeErr):find("normal", 1, true), "closeErr=" .. tostring(closeErr))
			assert(resumeOk == false and tostring(resumeErr):find("non-suspended", 1, true), "resume=" .. tostring(resumeErr))
			return "ok"
		`), "=coro_close_normal")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("p0_bytecode_vararg_flags_and_abslineinfo", func(t *testing.T) {
		badFlags := &bytecode.Proto{
			Flag:         bytecode.PFVarargHidden | bytecode.PFVarargTable,
			MaxStackSize: 2,
			Code:         []bytecode.Instruction{bytecode.Instruction(bytecode.OP_VARARGPREP), bytecode.Instruction(bytecode.OP_RETURN0)},
		}
		if err := bytecode.Verify(badFlags); err == nil {
			t.Fatal("expected conflicting vararg flags to be rejected")
		}
		badUnknownFlag := &bytecode.Proto{
			Flag:         0x80,
			MaxStackSize: 2,
			Code:         []bytecode.Instruction{bytecode.Instruction(bytecode.OP_RETURN0)},
		}
		if err := bytecode.Verify(badUnknownFlag); err == nil {
			t.Fatal("expected unknown proto flag bits to be rejected")
		}
		badNegAbs := &bytecode.Proto{
			MaxStackSize: 2,
			Code:         []bytecode.Instruction{bytecode.Instruction(bytecode.OP_RETURN0)},
			LineInfo:     []int8{0},
			AbsLineInfo:  []bytecode.AbsLineInfo{{PC: -5, Line: 1}},
		}
		if err := bytecode.Verify(badNegAbs); err == nil {
			t.Fatal("expected negative AbsLineInfo.PC to be rejected")
		}
		badNonMonoAbs := &bytecode.Proto{
			MaxStackSize: 2,
			Code:         []bytecode.Instruction{bytecode.Instruction(bytecode.OP_RETURN0), bytecode.Instruction(bytecode.OP_RETURN0)},
			LineInfo:     []int8{0, 0},
			AbsLineInfo:  []bytecode.AbsLineInfo{{PC: 1, Line: 1}, {PC: 1, Line: 2}},
		}
		if err := bytecode.Verify(badNonMonoAbs); err == nil {
			t.Fatal("expected non-monotonic AbsLineInfo.PC to be rejected")
		}
		badLineLen := &bytecode.Proto{
			MaxStackSize: 2,
			Code:         []bytecode.Instruction{bytecode.Instruction(bytecode.OP_RETURN0)},
			LineInfo:     []int8{0, 0},
		}
		if err := bytecode.Verify(badLineLen); err == nil {
			t.Fatal("expected mismatched LineInfo length to be rejected")
		}
	})

	t.Run("p0_string_pack_huge_c_catchable_without_memlimit", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local ok, msg = pcall(string.pack, "c9223372036854775807", "")
			assert(ok == false and tostring(msg):find("not enough memory", 1, true), "got: " .. tostring(msg))
			return "ok"
		`), "=pack_huge_c")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("codex25_emergency_gc_closes_orphaned_upvals", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function make()
				local co = coroutine.create(function()
					local secret = "captured-value"
					coroutine.yield(function() return secret end)
				end)
				local ok, fn = coroutine.resume(co)
				assert(ok)
				return co, fn
			end
			local co, fn = make()
			return co, fn
		`), "=emerg_upval")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(cl)
		if err != nil || len(res) != 2 {
			t.Fatalf("res=%v err=%v", res, err)
		}
		wco := weak.Make(res[0].AsThread())
		fn := res[1]
		res = nil
		s.SetGlobal("kept_fn", fn)
		// Stay in Tier 0 and trigger emergency GC via memory pressure.
		if s.gc.tier1 {
			t.Fatal("expected Tier 0 before emergency GC")
		}
		s.collectEmergency()
		// Verify open upvalue was closed and captured value survived.
		got, err := s.Call(fn)
		if err != nil || len(got) != 1 || got[0].AsString().S != "captured-value" {
			t.Fatalf("captured value corrupted: got=%v err=%v", got, err)
		}
		for range 10 {
			runtime.GC()
			if wco.Value() == nil {
				break
			}
		}
		if wco.Value() != nil {
			t.Fatal("dead coroutine was still pinned by open upvalue after emergency GC")
		}
	})

	t.Run("codex25_string_dump_cancellation", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		bigSrc := "return '" + strings.Repeat("a", 512*1024) + "'"
		fn, err := s.LoadChunk([]byte(bigSrc), "=big_dump")
		if err != nil {
			t.Fatal(err)
		}
		dumpFn := s.GetGlobal("string").AsTable().RawGetStr(s.Intern("dump"))
		s.SetInstructionLimit(100)
		_, err = s.Call(dumpFn, fn)
		if !errors.Is(err, ErrInstructionLimit) {
			t.Fatalf("expected ErrInstructionLimit from string.dump, got %v", err)
		}

		s2 := NewState()
		defer s2.Close()
		fn2, err := s2.LoadChunk([]byte(bigSrc), "=big_dump_ctx")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s2.SetContext(ctx)
		_, err = s2.Call(s2.GetGlobal("string").AsTable().RawGetStr(s2.Intern("dump")), fn2)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled from string.dump, got %v", err)
		}
	})

	t.Run("codex25_table_create_gc_pacing", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local finalized = 0
			setmetatable({}, {__gc = function() finalized = finalized + 1 end})
			for i = 1, 200 do
				local _ = table.create(10000, 0)
			end
			assert(finalized == 1, "expected automatic GC pacing on table.create, finalized=" .. tostring(finalized))
			return "ok"
		`), "=table_create_gc")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("hash_update_and_delete_mid_pairs_stability", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local t = {}
			for i = 1, 200 do t["k" .. i] = i end
			for i = 1, 150 do t["k" .. i] = nil end
			-- Updating remaining existing keys during pairs must not rehash or lose iteration order.
			local seen = 0
			for k, v in pairs(t) do
				seen = seen + 1
				t[k] = v + 1
			end
			assert(seen == 50, "expected 50, got " .. seen)
			-- Existing hash integer key update must stay in place without rehash.
			local h = {}
			h[100000] = 1
			for i = 1, 50 do h["s" .. i] = i end
			for i = 1, 40 do h["s" .. i] = nil end
			local sawInt = false
			for k, v in pairs(h) do
				if k == 100000 then
					sawInt = true
					h[100000] = 2
				end
			end
			assert(sawInt and h[100000] == 2)
			return "ok"
		`), "=hash_inplace")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("ephemeron_chain_linear_convergence", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local e = setmetatable({}, {__mode = "k"})
			local first = {}
			local prev = first
			for i = 1, 2000 do
				local nxt = {i}
				e[prev] = nxt
				prev = nxt
			end
			local lastWeak = setmetatable({v = prev}, {__mode = "v"})
			prev = nil
			collectgarbage("collect")
			assert(lastWeak.v ~= nil and lastWeak.v[1] == 2000)
			first = nil
			collectgarbage("collect")
			assert(lastWeak.v == nil)
			return "ok"
		`), "=ephem_chain")
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("ephemeron chain took too long (%v); expected linear convergence", elapsed)
		}
	})

	t.Run("math_log_tostring_native_frames_and_tailcall_hooks", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			assert(math.log(1000, 10) == 3.0)
			assert(math.log(1e15, 10) == 15.0)
			assert(math.log(8, 2) == 3.0)
			local t = setmetatable({}, {__tostring = function() return 42 end})
			assert(tostring(t) == "42")

			-- Native frame visible inside table.sort comparator
			local info2, info3
			local function doSort()
				table.sort({2, 1}, function(a, b)
					info2 = debug.getinfo(2, "nS")
					info3 = debug.getinfo(3, "nS")
					return a < b
				end)
			end
			doSort()
			assert(info2 and info2.what == "C" and info2.name == "sort", "info2=" .. tostring(info2 and info2.what) .. "/" .. tostring(info2 and info2.name))
			assert(info3 and info3.what == "Lua" and info3.name == "doSort", "info3=" .. tostring(info3 and info3.name))

			-- Tailcall to native and pcall fires caller return hook
			local ev = {}
			debug.sethook(function(e)
				local info = debug.getinfo(2, "nS")
				ev[#ev + 1] = e .. ":" .. (info.name or info.what)
			end, "cr")
			local function f() return tonumber("1") end
			f()
			local function g() return pcall(function() return 1 end) end
			g()
			debug.sethook()
			local joined = table.concat(ev, ",")
			assert(joined:find("return:f", 1, true), "missing return:f in " .. joined)
			assert(joined:find("return:g", 1, true), "missing return:g in " .. joined)
			assert(joined:find("return:pcall", 1, true), "missing return:pcall in " .. joined)
			return "ok"
		`), "=parity_core")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("debug_hooks_iterators_and_unpack_caps", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			-- debug.gethook returns 1 value when no hook is set
			assert(select("#", debug.gethook()) == 1 and debug.gethook() == nil)
			-- debug.sethook with empty mask clears hook; invalid mask type errors
			debug.sethook(function() end, "")
			assert(select("#", debug.gethook()) == 1)
			local okBadMask = pcall(debug.sethook, function() end, {})
			assert(not okBadMask)
			-- Hook cannot yield
			local co = coroutine.create(function() local x = 1; x = x + 1; return x end)
			debug.sethook(co, function() coroutine.yield() end, "l")
			local rok, rmsg = coroutine.resume(co)
			assert(not rok and tostring(rmsg):find("C-call boundary", 1, true), "got " .. tostring(rmsg))

			-- utf8.codes singleton iterator + 0 return values when exhausted
			local f1 = utf8.codes("a")
			local f2 = utf8.codes("b")
			assert(f1 == f2)
			local g = string.gmatch("a", "a")
			assert(g() == "a")
			assert(select("#", g()) == 0)
			assert(select("#", f1("a", 1)) == 0)

			-- string.unpack & utf8.codepoint 1M result cap
			local big = string.rep("a", 1000001)
			local uok = pcall(string.unpack, string.rep("b", 1000001), big)
			assert(not uok)
			local cok = pcall(utf8.codepoint, big, 1, -1)
			assert(not cok)
			return "ok"
		`), "=hooks_iters_caps")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("load_searchpath_gc_params_and_stdlib_edge_parity", func(t *testing.T) {
		var warnBuf bytes.Buffer
		s := NewState()
		defer s.Close()
		s.SetErrOutput(&warnBuf)
		s.SetWarnings(true)

		// expandDefaultPath replaces only first ;; and preserves leading/trailing ;
		if got := expandDefaultPath(";a;;b;;c;", "DEF"); got != ";a;DEF;b;;c;" {
			t.Fatalf("expandDefaultPath got %q", got)
		}

		cl, err := s.LoadChunk([]byte(`
			-- load(string) does NOT strip BOM or shebang
			assert(load("\xef\xbb\xbfreturn 1") == nil)
			assert(load("#!/usr/bin/env lua\nreturn 1") == nil)
			assert(not pcall(loadfile, "nonexistent.lua", "B"))

			-- package.searchpath expands ? BEFORE splitting ;
			local p, em = package.searchpath("q;r", "?.lua")
			assert(p == nil and em == "no file 'q'\n\tno file 'r.lua'", "em=" .. tostring(em))

			-- Deep Lua recursion > 10000 frames succeeds
			local function rec(n)
				if n == 0 then return 1 end
				return rec(n - 1) + 0
			end
			assert(rec(12000) == 1)

			-- next({10,20,30}, 1.0) raises invalid key to 'next'
			local nok, nerr = pcall(next, {10, 20, 30}, 1.0)
			assert(not nok and tostring(nerr):find("invalid key to 'next'", 1, true), "nerr=" .. tostring(nerr))

			-- Integer for-loop with NaN limit and negative step executes
			local cnt = 0
			for i = 1, 0/0, -1 do
				cnt = cnt + 1
				if cnt == 3 then break end
			end
			assert(cnt == 3)

			-- ipairs fires call/return hooks when active
			local hits = 0
			debug.sethook(function(e) if e == "call" then hits = hits + 1 end end, "c")
			for i, v in ipairs({10, 20}) do end
			debug.sethook()
			assert(hits >= 3, "expected ipairs hooks, got " .. hits)

			-- String metatable IC invalidation
			local function getFoo(str) return str.foo end
			assert(getFoo("a") == nil and getFoo("b") == nil)
			string.foo = 99
			assert(getFoo("a") == 99)
			string.foo = nil

			-- collectgarbage param defaults & luaO_codeparam encoding
			assert(collectgarbage("param", "stepsize") == 9600)
			local oldPause = collectgarbage("param", "pause", 12345)
			assert(collectgarbage("param", "pause") == 12000)
			collectgarbage("param", "pause", oldPause)

			-- __gc preserves hookMask (allowHook) and warns on non-callable __gc
			local hookSaved = function() end
			debug.sethook(hookSaved, "c")
			local hookSeenInGC
			setmetatable({}, {__gc = function() hookSeenInGC = debug.gethook() end})
			setmetatable({}, {__gc = 42})
			collectgarbage("collect")
			debug.sethook()
			assert(hookSeenInGC == hookSaved)

			-- io.close() returns 2 values on standard file
			assert(select("#", io.close(io.stdout)) == 2)

			-- os.date rejects out-of-range timestamps; os.setlocale rejects invalid category
			assert(not pcall(os.date, "*t", 1 << 60))
			assert(not pcall(os.setlocale, "C", "bogus"))
			assert(os.date("!%EY", 0) == "1970")

			-- Arg validation parity
			local okE, errE = pcall(error, "x", 2.5)
			assert(not okE and tostring(errE):find("integer", 1, true))
			local okTN, errTN = pcall(tonumber, 10, 99)
			assert(not okTN and tostring(errTN):find("#1", 1, true))
			local okRG, errRG = pcall(rawget, {})
			assert(not okRG and tostring(errRG):find("#2", 1, true))
			local prot = setmetatable({}, {__metatable = "locked"})
			local okSM, errSM = pcall(setmetatable, prot, 5)
			assert(not okSM and tostring(errSM):find("#2", 1, true))
			table.sort({}, 5) -- n <= 1 succeeds
			assert(not pcall(table.sort, {2, 1}, 5))

			-- coroutine.wrap prepends caller location
			local w = coroutine.wrap(function() error("wrapped-boom") end)
			local wok, werr = pcall(w)
			assert(not wok and tostring(werr):find("edge_parity", 1, true), "werr=" .. tostring(werr))
			return "ok"
		`), "=edge_parity")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
		if !strings.Contains(warnBuf.String(), "error in __gc (attempt to call a number value") {
			t.Fatalf("expected non-callable __gc warning, got %q", warnBuf.String())
		}
	})
}

func TestCodexReviewRound26VMRegressions(t *testing.T) {
	t.Run("native_call_roots_return_values_across_gc_debt", func(t *testing.T) {
		for _, withHook := range []bool{false, true} {
			name := "unhooked"
			if withHook {
				name = "hooked"
			}
			t.Run(name, func(t *testing.T) {
				s := NewState()
				defer s.Close()
				var finalized bool
				gcFn := NativeValue(&NativeFunc{
					Name: "mark_gc",
					Fn: func(_ *State, _ []Value) ([]Value, error) {
						finalized = true
						return nil, nil
					},
				})
				mt := s.NewTable(0, 1)
				mt.RawSetStr(s, s.Intern("__gc"), gcFn)
				s.SetGlobal("fin_mt", TableValue(mt))

				if withHook {
					cl, err := s.LoadChunk([]byte(`debug.sethook(function() end, "cr")`), "=hook_setup")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := s.Call(cl); err != nil {
						t.Fatal(err)
					}
				}

				maker := NativeValue(&NativeFunc{
					Name: "make_pair",
					Fn: func(st *State, _ []Value) ([]Value, error) {
						tbl := st.NewTable(0, 1)
						tbl.RawSetStr(st, st.Intern("tag"), StringValue(st.Intern("alive")))
						st.SetMetatableOf(TableValue(tbl), mt)
						big, err := st.TryIntern(strings.Repeat("x", 128*1024))
						if err != nil {
							return nil, err
						}
						return []Value{TableValue(tbl), StringValue(big)}, nil
					},
				})
				res, err := s.Call(maker)
				if err != nil {
					t.Fatalf("State.Call(maker) failed: %v", err)
				}
				if finalized {
					t.Fatalf("returned table's __gc ran prematurely before State.Call returned")
				}
				if len(res) != 2 || !res[0].IsTable() || !res[1].IsString() || len(res[1].AsString().S) != 128*1024 {
					t.Fatalf("unexpected native return values: %v", res)
				}
				// Now drop references and verify __gc runs on explicit GC.
				res = nil
				s.CollectFull()
				if !finalized {
					t.Fatalf("expected __gc to run after returned table became unreachable")
				}
			})
		}
	})

	t.Run("count_hook_frame_growth_clears_at_current_pc", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
local function recurse(n)
	if n > 0 then
		local r = recurse(n - 1)
		return r + 1
	end
	return 0
end

local function probe(depth)
	debug.sethook(function()
		debug.sethook()
		recurse(depth)
	end, "", 1)
	local a = 1
	local b = 2
	local line = debug.getinfo(1, "l").currentline
	return line
end

local l1 = probe(1)
local l40 = probe(40)
assert(l1 == l40, "expected identical currentline across frame growth: l1=" .. tostring(l1) .. " l40=" .. tostring(l40))

-- Also verify combined count+line hook keeps accurate oldPC/lines after frame growth.
local lines = {}
local firedCount = false
local function probeLine()
	debug.sethook(function(ev, l)
		if ev == "count" and not firedCount then
			firedCount = true
			recurse(40)
		elseif ev == "line" then
			lines[#lines + 1] = l
		end
	end, "l", 1)
	local x = 10
	local y = x + 20
	debug.sethook()
	return y
end
assert(probeLine() == 30 and #lines >= 2)
return "ok"
`), "=hook_frame_growth")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("file_and_table_finalizer_lifo_order", func(t *testing.T) {
		s := NewState()
		var closeOrder []string
		s.SetGlobal("record_close", NativeValue(&NativeFunc{
			Name: "record_close",
			Fn: func(_ *State, args []Value) ([]Value, error) {
				closeOrder = append(closeOrder, FormatValue(args[0]))
				return nil, nil
			},
		}))
		cl, err := s.LoadChunk([]byte(`
			local log = {}
			local mt = debug.getmetatable(io.stdout)
			local orig_gc = mt.__gc
			local fileTag = "file1"
			mt.__gc = function(f)
				log[#log + 1] = fileTag
				record_close(fileTag)
				return orig_gc(f)
			end
			do
				local t1 = setmetatable({}, {__gc = function() log[#log + 1] = "t1" end})
				local f1 = assert(io.tmpfile())
				local t2 = setmetatable({}, {__gc = function() log[#log + 1] = "t2" end})
				local _ = t1, f1, t2
			end
			collectgarbage("collect")
			assert(table.concat(log, ",") == "t2,file1,t1", "expected t2,file1,t1, got " .. table.concat(log, ","))

			-- Leave live table + tmpfile + table for State.Close() LIFO verification.
			fileTag = "file2"
			_G.keep1 = setmetatable({}, {__gc = function() record_close("t3") end})
			_G.keepF = assert(io.tmpfile())
			_G.keep2 = setmetatable({}, {__gc = function() record_close("t4") end})
			return "ok"
		`), "=file_table_gc_order")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
		closeOrder = nil
		s.Close()
		got := strings.Join(closeOrder, ",")
		if got != "t4,file2,t3" {
			t.Fatalf("State.Close finalizer order = %q, want %q", got, "t4,file2,t3")
		}
	})

	t.Run("tailcall_yielding_native_preserves_frame_and_return_hooks", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function f()
				return coroutine.yield(42)
			end
			local co = coroutine.create(function()
				local x = f()
				return x + 1
			end)
			local events = {}
			debug.sethook(co, function(ev)
				local info = debug.getinfo(co, 2, "nSf")
				local tag = info and (info.func == f and "F" or info.func == coroutine.yield and "Y" or "O") or "?"
				events[#events + 1] = ev .. ":" .. tag
			end, "cr")

			local ok1, v1 = coroutine.resume(co)
			assert(ok1 and v1 == 42)
			local i0 = debug.getinfo(co, 0, "nfS")
			local i1 = debug.getinfo(co, 1, "nfS")
			local i2 = debug.getinfo(co, 2, "nfS")
			assert(i0 and i0.func == coroutine.yield, "level 0 must be coroutine.yield")
			assert(i1 and i1.func == f, "level 1 must preserve tail-calling Lua frame f")
			assert(i2 and i2.what == "Lua", "level 2 must be outer coroutine body")
			assert(table.concat(events, ",") == "call:O,call:F,call:Y", "before=" .. table.concat(events, ","))

			local ok2, v2 = coroutine.resume(co, 99)
			assert(ok2 and v2 == 100)
			assert(table.concat(events, ",") == "call:O,call:F,call:Y,return:Y,return:F,return:O", "after=" .. table.concat(events, ","))

			-- Installing a return hook AFTER suspension must still fire return:Y, return:F, return:O on resume.
			local co2 = coroutine.create(function()
				local x = f()
				return x + 2
			end)
			assert(coroutine.resume(co2))
			local late = {}
			debug.sethook(co2, function(ev)
				local info = debug.getinfo(co2, 2, "f")
				local tag = info and (info.func == f and "F" or info.func == coroutine.yield and "Y" or "O") or "?"
				late[#late + 1] = ev .. ":" .. tag
			end, "r")
			local ok3, v3 = coroutine.resume(co2, 10)
			assert(ok3 and v3 == 12)
			assert(table.concat(late, ",") == "return:Y,return:F,return:O", "late=" .. table.concat(late, ","))

			-- Error raised inside yield's return hook on resume must propagate cleanly.
			local co3 = coroutine.create(function()
				return f()
			end)
			assert(coroutine.resume(co3))
			debug.sethook(co3, function(ev)
				if ev == "return" then
					error("hook-boom", 0)
				end
			end, "r")
			local ok4, err4 = coroutine.resume(co3, 7)
			assert(not ok4 and err4 == "hook-boom", "ok4=" .. tostring(ok4) .. " err4=" .. tostring(err4))
			return "ok"
		`), "=tail_yield_hooks")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound27VMRegressions(t *testing.T) {
	t.Run("coallocated_stdlib_tables_release_payloads_on_gc", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(131072)

		var watched []weak.Pointer[LuaString]
		s.SetGlobal("watch_str", NativeValue(&NativeFunc{
			Name: "watch_str",
			Fn: func(_ *State, args []Value) ([]Value, error) {
				if len(args) > 0 && args[0].IsString() {
					watched = append(watched, weak.Make(args[0].AsString()))
				}
				return args, nil
			},
		}))

		cl, err := s.LoadChunk([]byte(`
			local rep, char = string.rep, string.char
			local libs = {"math", "table", "utf8", "coroutine", "os", "io", "debug"}
			for i = 1, #libs do
				local name = libs[i]
				local lib = _G[name]
				-- Store both a new key and overwrite an existing slot with a large payload.
				local payload = watch_str(rep(char(64 + i), 50000))
				lib._big_payload = payload
				for k in pairs(lib) do
					if k ~= "_big_payload" then
						lib[k] = payload
						break
					end
				end
				payload = nil
				lib = nil
				_G[name] = nil
				package.loaded[name] = nil
				collectgarbage("collect")
			end
			return "ok"
		`), "=coalloc_sweep")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
		if len(watched) != 7 {
			t.Fatalf("expected 7 watched payloads, got %d", len(watched))
		}
		for range 5 {
			runtime.GC()
		}
		alive := 0
		for _, wp := range watched {
			if wp.Value() != nil {
				alive++
			}
		}
		if alive != 0 {
			t.Fatalf("expected Go GC to reclaim all 7 removed stdlib payloads, %d still alive", alive)
		}
	})

	t.Run("tonumber_polls_ctx_and_instruction_limit", func(t *testing.T) {
		s := NewState()
		defer s.Close()

		// 1. Huge base-2 numeral aborts promptly under instruction limit and deadline.
		bigBin := StringValue(s.Intern(strings.Repeat("10", 16*1024*1024)))
		toNumFn := s.GetGlobal("tonumber")
		s.SetInstructionLimit(100)
		start := time.Now()
		_, err := s.Call(toNumFn, bigBin, IntValue(2))
		if !errors.Is(err, ErrInstructionLimit) {
			t.Fatalf("expected ErrInstructionLimit from tonumber(32MiB, 2), got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("tonumber(32MiB, 2) took too long to cancel: %v", elapsed)
		}
		if s.InstructionsExecuted() <= 0 {
			t.Fatalf("expected InstructionsExecuted > 0 after canceled tonumber")
		}

		s.SetInstructionLimit(0)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.SetContext(ctx)
		_, err = s.Call(toNumFn, bigBin, IntValue(2))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled from tonumber(32MiB, 2), got %v", err)
		}

		// 2. No-base huge decimal and hex numerals also poll cancellation controls.
		bigDec := StringValue(s.Intern(strings.Repeat("0", 16*1024*1024) + "42"))
		bigHex := StringValue(s.Intern("0x" + strings.Repeat("0", 16*1024*1024) + "ff"))
		s.SetContext(nil)
		s.SetInstructionLimit(100)
		if _, err := s.Call(toNumFn, bigDec); !errors.Is(err, ErrInstructionLimit) {
			t.Fatalf("expected ErrInstructionLimit from no-base decimal tonumber, got %v", err)
		}
		s.SetInstructionLimit(100)
		if _, err := s.Call(toNumFn, bigHex); !errors.Is(err, ErrInstructionLimit) {
			t.Fatalf("expected ErrInstructionLimit from no-base hex tonumber, got %v", err)
		}
		s.SetInstructionLimit(100)
		mathToInt := s.GetGlobal("math").AsTable().RawGetStr(s.Intern("tointeger"))
		if _, err := s.Call(mathToInt, bigDec); !errors.Is(err, ErrInstructionLimit) {
			t.Fatalf("expected ErrInstructionLimit from math.tointeger, got %v", err)
		}

		// 3. Valid long numerals (> 4096 bytes) under a generous budget match exact Lua 5.5.1 values.
		s.SetInstructionLimit(1_000_000)
		cl, err := s.LoadChunk([]byte(`
			local z = string.rep("0", 5000)
			local sp = string.rep(" ", 5000)
			assert(tonumber(z .. "42") == 42 and math.type(tonumber(z .. "42")) == "integer")
			assert(tonumber("-" .. z .. "1.5" .. z) == -1.5)
			assert(tonumber("0x" .. z .. "ff") == 255 and math.type(tonumber("0x" .. z .. "ff")) == "integer")
			assert(tonumber("0x" .. z .. "1.8p+1") == 3.0)
			assert(tonumber(z .. "101010", 2) == 42)
			local nz = tonumber(sp .. "-" .. z .. ".0" .. sp)
			assert(nz == 0.0 and 1/nz == -math.huge, "expected -0.0")
			return "ok"
		`), "=long_tonumber_parity")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("custom_file_gc_survives_go_gc_before_vm_collect", func(t *testing.T) {
		s := NewState()
		defer s.Close()

		s.SetGlobal("force_go_gc", NativeValue(&NativeFunc{
			Name: "force_go_gc",
			Fn: func(_ *State, _ []Value) ([]Value, error) {
				for range 4 {
					runtime.GC()
				}
				return nil, nil
			},
		}))

		cl, err := s.LoadChunk([]byte(`
			local count = 0
			-- Create one tmpfile BEFORE customizing fileMeta.__gc to test promotion of existing handles.
			local pre = assert(io.tmpfile())
			local mt = debug.getmetatable(io.stdout)
			local orig_gc = mt.__gc
			mt.__gc = function(f)
				count = count + 1
				return orig_gc(f)
			end
			do
				local post = assert(io.tmpfile())
				local _ = post
			end
			pre = nil
			force_go_gc()
			collectgarbage("collect")
			assert(count == 2, "expected both pre- and post-customization file __gc to run, got " .. tostring(count))
			return "ok"
		`), "=custom_file_gc_go_gc")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("upvalueid_does_not_root_captured_objects", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(131072)

		var watched []weak.Pointer[LuaString]
		s.SetGlobal("watch_str", NativeValue(&NativeFunc{
			Name: "watch_str",
			Fn: func(_ *State, args []Value) ([]Value, error) {
				if len(args) > 0 && args[0].IsString() {
					watched = append(watched, weak.Make(args[0].AsString()))
				}
				return args, nil
			},
		}))
		s.SetGlobal("null_lud", LightUDValue(nil))

		cl, err := s.LoadChunk([]byte(`
			local ids = {}
			local seen = {}
			for i = 1, 50 do
				local big = watch_str(string.rep("z", 32768) .. tostring(i))
				local fn = function() return big end
				local id = debug.upvalueid(fn, 1)
				assert(type(id) == "userdata")
				assert(id ~= null_lud)
				assert(tostring(id):find("^userdata: 0x"))
				assert(seen[id] == nil, "duplicate upvalueid")
				seen[id] = i
				ids[i] = id
				big = nil
				fn = nil
				collectgarbage("collect")
			end
			-- Verify identity preservation across shared upvalues and upvaluejoin.
			local x = 1
			local f1 = function() return x end
			local f2 = function() return x end
			local y = 2
			local f3 = function() return y end
			assert(debug.upvalueid(f1, 1) == debug.upvalueid(f2, 1))
			assert(debug.upvalueid(f1, 1) ~= debug.upvalueid(f3, 1))
			debug.upvaluejoin(f3, 1, f1, 1)
			assert(debug.upvalueid(f3, 1) == debug.upvalueid(f1, 1))
			-- Verify native closure upvalueid doesn't pin captured strings either.
			local g = string.gmatch(watch_str(string.rep("q", 32768)), ".")
			local gid = debug.upvalueid(g, 1)
			assert(type(gid) == "userdata" and gid ~= debug.upvalueid(g, 2))
			ids[#ids + 1] = gid
			g = nil
			collectgarbage("collect")
			return #ids
		`), "=upvalueid_non_owning")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].Int() != 51 {
			t.Fatalf("res=%v err=%v", res, err)
		}
		for range 5 {
			runtime.GC()
		}
		alive := 0
		for _, wp := range watched {
			if wp.Value() != nil {
				alive++
			}
		}
		if alive != 0 {
			t.Fatalf("expected Go GC to reclaim all %d strings captured by discarded closures despite retaining upvalueids, %d still alive", len(watched), alive)
		}
	})
}

func TestCodexReviewRound28VMRegressions(t *testing.T) {
	t.Run("gmatch_userdata_accounts_retained_strings_under_memlimit", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(131072)

		cl, err := s.LoadChunk([]byte(`
			local keep = {}
			local ok, err = pcall(function()
				for i = 1, 100 do
					local iter = string.gmatch(string.rep("a", 32768) .. tostring(i), ".")
					local _, ud = debug.getupvalue(iter, 3)
					assert(type(ud) == "userdata")
					assert(debug.getuservalue(ud, 1) == nil, "gmatch state userdata must have 0 Lua uservalues")
					keep[i] = ud
					iter = nil
					collectgarbage("collect")
				end
			end)
			assert(not ok and tostring(err):find("not enough memory", 1, true), "expected OOM retaining gmatch userdata, got ok=" .. tostring(ok) .. " err=" .. tostring(err))
			assert(#keep < 10, "expected OOM after <10 retained 32KiB gmatch userdata, kept=" .. tostring(#keep))
			return #keep
		`), "=gmatch_ud_memlimit")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].Int() >= 10 {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("tbc_close_arity_matches_lua55", func(t *testing.T) {
		s := NewState()
		defer s.Close()

		cl, err := s.LoadChunk([]byte(`
			local got = {}
			local function mk(tag)
				return setmetatable({}, {
					__close = function(...)
						got[tag] = select("#", ...)
					end
				})
			end
			do
				local _ <close> = mk("normal")
			end
			pcall(function()
				local _ <close> = mk("err")
				error("boom")
			end)
			local co = coroutine.create(function()
				local _ <close> = mk("coro_normal")
				coroutine.yield()
			end)
			assert(coroutine.resume(co))
			assert(coroutine.resume(co))
			local co2 = coroutine.create(function()
				local _ <close> = mk("coro_close")
				coroutine.yield()
			end)
			assert(coroutine.resume(co2))
			assert(coroutine.close(co2))
			assert(got.normal == 1 and got.coro_normal == 1 and got.coro_close == 1 and got.err == 2)
			return "ok"
		`), "=tbc_arity")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound29VMRegressions(t *testing.T) {
	t.Run("long_string_key_hash_polls_execution_controls", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		tbl := s.NewDictTable(4)
		for _, k := range []string{"a", "b", "c"} {
			tbl.RawSetStr(s, s.intern.Intern(k), TrueValue())
		}
		key := StringValue(s.intern.Intern(strings.Repeat("k", 32<<20)))
		s.SetInstructionLimit(100)
		_, err := s.Call(s.GetGlobal("rawget"), TableValue(tbl), key)
		if !errors.Is(err, ErrInstructionLimit) {
			t.Fatalf("rawget with fresh 32MiB key ignored instruction limit: err=%v", err)
		}
	})

	t.Run("gc_table_traversal_polls_execution_controls", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local shared = {}
			local big = table.create(2000000)
			for i = 1, 2000000 do big[i] = shared end
			KEEP = big
		`), "=gc_big")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(cl); err != nil {
			t.Fatal(err)
		}
		s.SetInstructionLimit(100)
		_, err = s.Call(s.GetGlobal("collectgarbage"), StringValue(s.intern.Intern("collect")))
		if !errors.Is(err, ErrInstructionLimit) {
			t.Fatalf("collectgarbage over 2M-ref table ignored instruction limit: err=%v", err)
		}
	})

	t.Run("hook_error_respects_native_call_boundary", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function cb()
				debug.sethook(function()
					if debug.getinfo(2, 'f').func == pcall then debug.sethook(); error("hookfail") end
				end, "r")
				return pcall(type, 1)
			end
			local ok, e = pcall(string.gsub, 'x', '.', cb)
			assert(ok == false and tostring(e):find("hookfail", 1, true), tostring(e))
			return "ok"
		`), "=hook_native_boundary")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("native_results_aliasing_args_survive_state_call", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		x := StringValue(s.intern.Intern("x"))
		res, err := s.Call(s.GetGlobal("assert"), TrueValue(), x)
		if err != nil || len(res) != 2 || !res[0].IsTrue() || res[1] != x {
			t.Fatalf("assert: res=%v err=%v", res, err)
		}
		b := StringValue(s.intern.Intern("b"))
		res, err = s.Call(s.GetGlobal("select"), IntValue(2), x, b)
		if err != nil || len(res) != 1 || res[0] != b {
			t.Fatalf("select: res=%v err=%v", res, err)
		}
		cl, err := s.LoadChunk([]byte(`
			local t = setmetatable({}, {__index = assert})
			assert(t[1] == t and table.unpack(t, 1, 1) == t)
			return "ok"
		`), "=alias_index")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].AsString().S != "ok" {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})
}

func TestCodexReviewRound30VMRegressions(t *testing.T) {
	t.Run("gmatch_state_comes_from_live_upvalue", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		s.SetMemoryLimit(131072)
		cl, err := s.LoadChunk([]byte(`
			local it = string.gmatch("ab", ".")
			assert(it() == "a")
			local keep = {}
			for i = 1, 100 do
				local iter = string.gmatch(string.rep("a", 32768) .. tostring(i), ".")
				debug.setupvalue(iter, 1, nil)
				debug.setupvalue(iter, 2, nil)
				debug.setupvalue(iter, 3, nil)
				keep[i] = iter
				collectgarbage("collect")
			end
			assert(it() == "b" and it() == nil)
			local ok, e = pcall(keep[1])
			assert(not ok and tostring(e):find("bad gmatch iterator state", 1, true), tostring(e))
			return #keep
		`), "=gmatch_live_upvalue")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl); err != nil || len(res) != 1 || res[0].Int() != 100 {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})

	t.Run("read_number_whitespace_polls_execution_controls", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "spaces.txt")
		if err := os.WriteFile(path, bytes.Repeat([]byte{' '}, 32<<20), 0o644); err != nil {
			t.Fatal(err)
		}
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`local f = assert(io.open(...)); return f, f.read`), "=read_n")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(cl, StringValue(s.intern.Intern(path)))
		if err != nil || len(res) != 2 {
			t.Fatalf("res=%v err=%v", res, err)
		}
		s.SetInstructionLimit(100)
		_, err = s.Call(res[1], res[0], StringValue(s.intern.Intern("n")))
		if !errors.Is(err, ErrInstructionLimit) {
			t.Fatalf("read('n') over 32MiB whitespace ignored instruction limit: err=%v", err)
		}
		s.SetInstructionLimit(0)
		if _, err := s.Call(s.GetGlobal("io").AsTable().RawGetStr(s.intern.Intern("close")), res[0]); err != nil {
			t.Fatalf("close: %v", err)
		}
	})
}

func TestCodexReviewRound31VMRegressions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, 32<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"a", "l", "L", "n33554432"} {
		t.Run("bulk_read_"+format+"_polls_execution_controls", func(t *testing.T) {
			s := NewState()
			defer s.Close()
			cl, err := s.LoadChunk([]byte(`local f = assert(io.open(...)); return f, f.read`), "=bulk_read")
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Call(cl, StringValue(s.intern.Intern(path)))
			if err != nil || len(res) != 2 {
				t.Fatalf("res=%v err=%v", res, err)
			}
			var arg Value
			if format[0] == 'n' {
				arg = IntValue(32 << 20)
			} else {
				arg = StringValue(s.intern.Intern(format))
			}
			s.SetInstructionLimit(100)
			_, err = s.Call(res[1], res[0], arg)
			if !errors.Is(err, ErrInstructionLimit) {
				t.Fatalf("file:read(%s) over 32MiB ignored instruction limit: err=%v", format, err)
			}
		})
	}
	t.Run("short_line_reads_not_charged", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "lines.txt")
		if err := os.WriteFile(p, bytes.Repeat([]byte("abc\n"), 2000), 0o644); err != nil {
			t.Fatal(err)
		}
		s := NewState()
		defer s.Close()
		s.SetInstructionLimit(1 << 20)
		cl, err := s.LoadChunk([]byte(`local n = 0; for l in io.lines(...) do n = n + 1 end; return n`), "=short_lines")
		if err != nil {
			t.Fatal(err)
		}
		if res, err := s.Call(cl, StringValue(s.intern.Intern(p))); err != nil || len(res) != 1 || res[0].Int() != 2000 {
			t.Fatalf("res=%v err=%v", res, err)
		}
	})
}
