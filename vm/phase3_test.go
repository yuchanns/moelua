package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
