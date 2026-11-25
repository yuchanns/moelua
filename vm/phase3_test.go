package vm

import (
	"errors"
	"testing"
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
