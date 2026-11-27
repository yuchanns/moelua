package vm

import (
	"testing"

	"github.com/yuchanns/moelua/internal/arena"
)

func TestProxyIndexMetatableShapeSharingAndPoisoning(t *testing.T) {
	t.Run("per_instance_metatable_shares_proxy_shape_and_preserves_protoEpoch", func(t *testing.T) {
		if !OptimizeEnabled {
			t.Skip("shape optimization disabled in faithful mode")
		}
		s := NewState()
		defer s.Close()

		cl, err := s.LoadChunk([]byte(`
			local Ball = {}
			function Ball:step()
				self.x = self.x + self.vx
				return self.x
			end
			local balls = {}
			for i = 1, 64 do
				balls[i] = setmetatable({ x = i, vx = 2 }, { __index = Ball })
			end
			local sum = 0
			for i = 1, 64 do
				sum = sum + balls[i]:step()
			end
			assert(not rawequal(getmetatable(balls[1]), getmetatable(balls[2])))
			assert(rawequal(getmetatable(balls[1]).__index, Ball))
			return balls, sum
		`), "=proxy_share")
		if err != nil {
			t.Fatal(err)
		}
		epochBefore := s.protoEpoch
		res, err := s.Call(cl)
		if err != nil {
			t.Fatal(err)
		}
		if res[1].Int() != 64*(65+4)/2 {
			t.Fatalf("unexpected sum: got %v", res[1])
		}
		if s.protoEpoch != epochBefore {
			t.Fatalf("per-instance {__index = Ball} should not bump protoEpoch: before=%d after=%d", epochBefore, s.protoEpoch)
		}
		balls := s.asTable(res[0])
		b1 := s.asTable(balls.RawGetInt(s, 1))
		b2 := s.asTable(balls.RawGetInt(s, 2))
		b64 := s.asTable(balls.RawGetInt(s, 64))
		if b1.shapeRef == arena.NullRef || b1.shapeRef != b2.shapeRef || b1.shapeRef != b64.shapeRef {
			t.Fatalf("expected all instances to share one proxy shape, got b1=%d b2=%d b64=%d", b1.shapeRef, b2.shapeRef, b64.shapeRef)
		}
		sh := b1.shapePtr(s.arena)
		if !sh.isMetaProxy() {
			t.Fatalf("expected shapeFlagMetaProxy on shared proxy shape")
		}
		if b1.metaRef == b2.metaRef {
			t.Fatalf("expected distinct underlying metatable tables per instance")
		}
	})

	t.Run("in_place_proxy_mt_index_mutation_invalidates_and_isolates_instances", func(t *testing.T) {
		s := NewState()
		defer s.Close()

		// Critical soundness test: mutate mt1.__index = Other, then read t2.m BEFORE t1.m.
		// If proxy shape were re-cached on t2 without poisoning, t1.m would wrongly read Ball.m!
		cl, err := s.LoadChunk([]byte(`
			local Ball = { kind = "ball" }
			local Other = { kind = "other" }
			function Ball:tag() return "B:" .. self.id end
			function Other:tag() return "O:" .. self.id end

			local function readKind(o) return o.kind end
			local function callTag(o) return o:tag() end
			local function readId(o) return o.id end

			local mt1 = { __index = Ball }
			local mt2 = { __index = Ball }
			local t1 = setmetatable({ id = 1 }, mt1)
			local t2 = setmetatable({ id = 2 }, mt2)

			-- Warm monomorphic ICs on the shared proxy shape.
			assert(readKind(t1) == "ball" and readKind(t2) == "ball")
			assert(callTag(t1) == "B:1" and callTag(t2) == "B:2")
			assert(readId(t1) == 1 and readId(t2) == 2)

			-- Mutate mt1 in place!
			mt1.__index = Other

			-- Exercise t2 FIRST (still points to Ball), THEN t1 (now points to Other).
			assert(readKind(t2) == "ball")
			assert(callTag(t2) == "B:2")
			assert(readKind(t1) == "other")
			assert(callTag(t1) == "O:1")
			assert(readId(t1) == 1 and readId(t2) == 2)

			-- Future instances using {__index = Ball} must still behave identically.
			local t3 = setmetatable({ id = 3 }, { __index = Ball })
			assert(readKind(t3) == "ball" and callTag(t3) == "B:3")
			return "ok"
		`), "=proxy_poison_index")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(cl)
		if err != nil || len(res) != 1 || s.FormatValue(res[0]) != "ok" {
			t.Fatalf("unexpected result: res=%v err=%v", res, err)
		}
	})

	t.Run("in_place_proxy_mt_newindex_and_setmetatable_and_rawset", func(t *testing.T) {
		s := NewState()
		defer s.Close()

		cl, err := s.LoadChunk([]byte(`
			local C1 = { v = 10 }
			local mt1 = { __index = C1 }
			local mt2 = { __index = C1 }
			local a = setmetatable({ x = 1 }, mt1)
			local b = setmetatable({ x = 2 }, mt2)

			local function setY(o, v) o.y = v end
			local function setZ(o, v) o.z = v end
			-- Warm icAddSlot on shared proxy shape (transitioning proxy shape must preserve proxy flag).
			setY(a, 100)
			setY(b, 200)
			assert(a.y == 100 and b.y == 200)

			-- Add __newindex to mt1 in place, then write to b BEFORE a.
			local logged = {}
			mt1.__newindex = function(self, k, v) logged[k] = v end
			setZ(b, 20)
			setZ(a, 10)
			assert(b.z == 20 and rawget(a, "z") == nil and logged.z == 10)

			-- Also test setting a metatable onto a proxy metatable + rawset integer/table keys.
			local C2 = { k = "c2" }
			local pm = { __index = C2 }
			local p = setmetatable({}, pm)
			assert(p.k == "c2")
			setmetatable(pm, { __index = { extra = 1 } })
			pm[1] = 42
			pm[{}] = 99
			assert(p.k == "c2")

			-- Self-referential metatable must not trigger proxy canonicalization bug.
			local selfMT = { __index = C2 }
			setmetatable(selfMT, selfMT)
			assert(selfMT.k == "c2")
			return "ok"
		`), "=proxy_poison_newindex")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(cl)
		if err != nil || len(res) != 1 || s.FormatValue(res[0]) != "ok" {
			t.Fatalf("unexpected result: res=%v err=%v", res, err)
		}
	})

	t.Run("proxy_shape_gc_reclaims_wrapper_mt_and_poison_oom_safe", func(t *testing.T) {
		s := NewState()
		defer s.Close()

		proto := s.NewTable(0, 1)
		proto.RawSetStr(s, s.intern.Intern("v"), IntValue(7))
		s.GlobalEnv().RawSetStr(s, s.intern.Intern("PROTO"), TableValue(proto))

		mt := s.NewTable(0, 1)
		mt.RawSetStr(s, s.intern.Intern("__index"), TableValue(proto))
		mtRef := mt.self
		obj := s.NewTable(0, 1)
		obj.RawSetStr(s, s.intern.Intern("x"), IntValue(1))
		s.SetMetatable(obj, mt)
		mt = nil
		obj = nil

		s.CollectFull()
		if s.arena.IsLive(mtRef) {
			t.Fatalf("dropped proxy wrapper metatable was retained by shape caches")
		}

		// Verify poisoned proxy migration under tight memory limit falls back safely without stale memExceeded.
		cl, err := s.LoadChunk([]byte(`
			local P = { val = "P" }
			local Q = { val = "Q" }
			local m1 = { __index = P }
			local o1 = setmetatable({ a = 1 }, m1)
			local function getV(o) return o.val end
			assert(getV(o1) == "P")
			m1.__index = Q
			return getV, o1
		`), "=proxy_oom")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(cl)
		if err != nil {
			t.Fatal(err)
		}
		// Tight cap equal to current usage so optional non-proxy shape migration cannot allocate.
		s.SetMemoryLimit(s.MemoryUsage())
		v, err := s.Call1(res[0], res[1])
		s.SetMemoryLimit(0)
		if err != nil {
			t.Fatalf("poisoned proxy read failed under tight memLimit: %v", err)
		}
		if s.FormatValue(v) != "Q" {
			t.Fatalf("expected Q after in-place __index update under tight memLimit, got %q", s.FormatValue(v))
		}
		if s.memExceeded {
			t.Fatalf("expected memExceeded to stay cleared after optional proxy migration skip")
		}
	})

	t.Run("clearing_proxy_index_releases_prototype_for_weak_tables_without_reaccessing_instances", func(t *testing.T) {
		s := NewState()
		defer s.Close()

		cl, err := s.LoadChunk([]byte(`
			local weak = setmetatable({}, { __mode = "v" })
			local function getX(o) return o.x end
			local mt, instIdle, instOwn
			do
				local proto = { method = function() return 42 end }
				weak[1] = proto
				mt = { __index = proto }
				instIdle = setmetatable({ x = 1 }, mt)
				instOwn = setmetatable({ x = 2 }, { __index = proto })
				-- Warm own-slot IC on instOwn; leave instIdle untouched after construction.
				assert(getX(instOwn) == 2)
				-- Replace/clear __index on both metatables without touching instIdle or slow-accessing instOwn.
				mt.__index = nil
				getmetatable(instOwn).__index = nil
			end

			-- Even with instIdle idle and instOwn only hitting own-slot IC, proto must be collected!
			assert(getX(instOwn) == 2)
			collectgarbage("collect")
			collectgarbage("collect")
			assert(weak[1] == nil, "stale proxy Shape retained old prototype across GC")

			-- Both instances remain completely valid afterward (and no longer see old proto method).
			assert(instIdle.x == 1 and instIdle.method == nil)
			assert(instOwn.x == 2 and instOwn.method == nil)
			return "ok"
		`), "=proxy_weak_release")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Call(cl)
		if err != nil || len(res) != 1 || s.FormatValue(res[0]) != "ok" {
			t.Fatalf("unexpected result: res=%v err=%v", res, err)
		}
	})
}

func TestTableSortCustomComparatorAndSetTabUpIC(t *testing.T) {
	s := NewState()
	defer s.Close()

	cl, err := s.LoadChunk([]byte(`
		-- 1. Global counter via OP_SETTABUP IC inside custom 2-arg comparator.
		cmpCount = 0
		local a, x = {}, 42
		for idx = 1, 2000 do
			x = (x * 16807) % 2147483647
			a[idx] = x / 2147483647
		end
		table.sort(a, function(u, v)
			cmpCount = cmpCount + 1
			return v < u
		end)
		assert(cmpCount > 0)
		for idx = 2, #a do
			assert(a[idx - 1] >= a[idx])
		end

		-- 2. Deleting global key invalidates OP_SETTABUP IC (respects __newindex afterward).
		local function bump() cmpCount = (cmpCount or 0) + 1 end
		bump()
		local prev = cmpCount
		cmpCount = nil
		local intercepted = 0
		setmetatable(_ENV, {
			__newindex = function(t, k, v)
				intercepted = intercepted + 1
				rawset(t, k, v)
			end
		})
		bump()
		assert(intercepted == 1 and cmpCount == 1 and prev > 1)
		setmetatable(_ENV, nil)

		-- 3. 0-param, 1-param, vararg, and __call-table comparators.
		local t0 = {3, 1, 2}
		table.sort(t0, function() return false end)
		assert(#t0 == 3)

		local seen = 0
		local t1 = {3, 1, 2}
		table.sort(t1, function(a) seen = seen + 1; return false end)
		assert(seen > 0)

		local tv = {5, 1, 4, 2, 3}
		table.sort(tv, function(...)
			local x, y = ...
			return x < y
		end)
		for i = 1, 5 do assert(tv[i] == i) end

		local tc = {5, 1, 4, 2, 3}
		local okTbl, errTbl = pcall(table.sort, tc, setmetatable({}, { __call = function(_, x, y) return x > y end }))
		assert(not okTbl and tostring(errTbl):find("function expected"))

		local tn = {5, 1, 4, 2, 3}
		table.sort(tn, math.ult)
		for i = 1, 5 do assert(tn[i] == i) end

		-- 4. Invalid order function error propagation still matches Lua 5.5.
		local ok, err = pcall(table.sort, {1, 2, 3, 4}, function() return true end)
		assert(not ok and tostring(err):find("invalid order function"))
		return "ok"
	`), "=sort_settabup")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Call(cl)
	if err != nil || len(res) != 1 || s.FormatValue(res[0]) != "ok" {
		t.Fatalf("unexpected result: res=%v err=%v", res, err)
	}
}
