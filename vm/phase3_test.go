package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
	"weak"
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

	// 5. next(t, k) normalizes integral float keys in hash tables (e.g. -1.0 -> -1).
	t.Run("next_integral_float_hash_key", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local t = { [-1] = "a", [-2] = "b" }
			local k1, v1 = next(t, -1.0)
			local k2, v2 = next(t, -2.0)
			assert((k1 == -2 and v1 == "b" and k2 == nil) or (k2 == -1 and v2 == "a" and k1 == nil),
				string.format("unexpected next results: (%s,%s) (%s,%s)", tostring(k1), tostring(v1), tostring(k2), tostring(v2)))
			local t2 = { [0] = "z", [1] = "one" }
			local nk, nv = next(t2, 0.0)
			assert(nk == nil or nk == 1)
			local nk2, nv2 = next(t2, 1.0)
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
			local chunk = string.rep(" ", 1024)
			local n = 0
			local fn, err = load(function()
				recordMem()
				n = n + 1
				if n <= 1024 then return chunk end
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
			t.Fatalf("expected peak MemoryUsage during load(reader) to include buffered bytes, got %d", peakMem)
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
