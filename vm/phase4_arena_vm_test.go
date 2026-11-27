package vm

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
	"weak"

	"github.com/yuchanns/moelua/internal/arena"
)

func assertTypeContainsNoGoPointers(t *testing.T, typ reflect.Type, path string) {
	t.Helper()
	switch typ.Kind() {
	case reflect.Ptr, reflect.UnsafePointer, reflect.String, reflect.Slice,
		reflect.Map, reflect.Chan, reflect.Func, reflect.Interface:
		t.Fatalf("Arena noscan type %s contains Go pointer-bearing kind %s (%s)", path, typ.Kind(), typ)
	case reflect.Array:
		assertTypeContainsNoGoPointers(t, typ.Elem(), path+"[]")
	case reflect.Struct:
		for i := range typ.NumField() {
			f := typ.Field(i)
			assertTypeContainsNoGoPointers(t, f.Type, path+"."+f.Name)
		}
	}
}

// TestPhase4NoscanArenaStructZeroPointers enforces at test time that every VM object stored
// inside per-State noscan Arena slabs contains zero Go pointers (ptrdata == 0), guaranteeing
// Go's GC never scans Lua VM heaps and writes emit zero Go write barriers.
func TestPhase4NoscanArenaStructZeroPointers(t *testing.T) {
	types := []any{
		Value{},
		LuaString{},
		Table{},
		tableHash{},
		hashNode{},
		Shape{},
		shapeTrans{},
		shapeIndexEntry{},
		ICEntry{},
		polyEntry{},
		Closure{},
		Upvalue{},
		Userdata{},
		ArenaNative{},
		ArenaProto{},
		callFrame{},
		Thread{},
	}
	for _, sample := range types {
		rt := reflect.TypeOf(sample)
		assertTypeContainsNoGoPointers(t, rt, rt.Name())
	}
}

// TestPhase4GenerationalAndIncrementalGCStress exercises both incremental tri-color and
// generational minor/major modes across old->young barriers, weak/ephemeron tables,
// open/closed upvalues, coroutines, and LIFO __gc finalizers with resurrection.
func TestPhase4GenerationalAndIncrementalGCStress(t *testing.T) {
	for _, mode := range []string{"incremental", "generational"} {
		t.Run(mode, func(t *testing.T) {
			s := NewState()
			defer s.Close()

			src := fmt.Sprintf(`
				collectgarbage(%q)
				local oldRoot = {}
				-- Promote oldRoot to old generation via multiple collections.
				collectgarbage("collect")
				collectgarbage("collect")

				local finOrder = {}
				local resHolder = nil
				local resMT = {
					__gc = function(self)
						finOrder[#finOrder + 1] = self.tag
						if self.resurrect then
							self.resurrect = false
							resHolder = self
						end
					end,
				}

				local wk = setmetatable({}, { __mode = "k" })
				local wv = setmetatable({}, { __mode = "v" })
				local we = setmetatable({}, { __mode = "kv" })

				-- Mutate old table to reference young objects (exercises generational BarrierBack).
				for i = 1, 200 do
					local young = { idx = i, s = "young_" .. tostring(i) }
					if i <= 10 then
						oldRoot[i] = young
						wk[young] = { payload = i }
						wv[i] = young
					else
						local ephemKey = { k = i }
						wk[ephemKey] = { cyclic = ephemKey, val = i }
						wv[i] = { drop = i }
						we[ephemKey] = { drop2 = i }
					end
				end

				do
					local a = setmetatable({ tag = "a" }, resMT)
					local b = setmetatable({ tag = "b", resurrect = true }, resMT)
					local c = setmetatable({ tag = "c" }, resMT)
					local _ = a, b, c
				end

				-- Allocate burst of young garbage to trigger automatic minor/incremental steps.
				for i = 1, 4000 do
					local tmp = { i, i + 1, "burst_" .. tostring(i) }
					local _ = tmp[1]
				end

				collectgarbage("collect")
				assert(table.concat(finOrder, ",") == "c,b,a", "expected LIFO c,b,a got " .. table.concat(finOrder, ","))
				assert(resHolder ~= nil and resHolder.tag == "b", "resurrected object lost")
				-- Release resurrected object; finalizer must NOT run a second time (Lua 5.5 marked-finalized).
				resHolder = nil
				collectgarbage("collect")
				assert(#finOrder == 3, "resurrected finalizer ran twice: " .. table.concat(finOrder, ","))

				-- Verify surviving old->young entries intact and dead weak entries cleared.
				for i = 1, 10 do
					assert(oldRoot[i].idx == i and oldRoot[i].s == "young_" .. tostring(i))
					assert(wk[oldRoot[i]] ~= nil and wk[oldRoot[i]].payload == i)
					assert(wv[i] == oldRoot[i])
				end
				for i = 11, 200 do
					assert(wv[i] == nil, "weak value not cleared at " .. i)
				end
				local wkCount = 0
				for _ in pairs(wk) do wkCount = wkCount + 1 end
				assert(wkCount == 10, "ephemeron cyclic garbage not cleared: count=" .. wkCount)
				local weCount = 0
				for _ in pairs(we) do weCount = weCount + 1 end
				assert(weCount == 0, "all-weak table not cleared: count=" .. weCount)

				-- Coroutine + open/closed upvalues stress under GC.
				local closures = {}
				for i = 1, 50 do
					local co = coroutine.create(function(v)
						local x = v
						coroutine.yield(function() x = x + 1; return x end)
						return x
					end)
					local ok, fn = coroutine.resume(co, i * 10)
					assert(ok and type(fn) == "function")
					closures[i] = fn
				end
				collectgarbage("collect")
				for i = 1, 50 do
					assert(closures[i]() == i * 10 + 1)
				end
				return "ok"
			`, mode)

			cl, err := s.LoadChunk([]byte(src), "=gc_stress_"+mode)
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Call(cl)
			if err != nil || len(res) != 1 || s.AsString(res[0]).Str() != "ok" {
				t.Fatalf("mode=%s failed: res=%v err=%v", mode, res, err)
			}
		})
	}
}

// TestPhase4GoRefsUserdataAndDynamicNativeReclamation verifies that Go host objects bridged
// via State.goRefs (Userdata payloads and dynamic NativeFunc closures) are released cleanly
// when Lua's Arena GC sweeps them and reclaimed by Go's GC.
func TestPhase4GoRefsUserdataAndDynamicNativeReclamation(t *testing.T) {
	s := NewState()
	defer s.Close()

	baseRefs := s.ArenaStats().GoRefsLive
	var wpUD, wpFn weak.Pointer[byte]
	var udCanary, fnCanary Value

	func() {
		udBuf := make([]byte, 32*1024)
		wpUD = weak.Make(&udBuf[0])
		udVal, err := s.TryNewUserdata(udBuf, 0)
		if err != nil {
			t.Fatal(err)
		}
		udCanary = udVal
		s.SetGlobal("HOST_UD", udVal)

		fnBuf := make([]byte, 32*1024)
		wpFn = weak.Make(&fnBuf[0])
		captured := fnBuf
		fnVal := s.NativeValue(&NativeFunc{
			Name: "ephemeral_cb",
			Fn: func(_ *State, _ []Value) ([]Value, error) {
				return []Value{IntValue(int64(len(captured)))}, nil
			},
		})
		fnCanary = fnVal
		s.SetGlobal("HOST_FN", fnVal)

		res, err := s.Call(fnVal)
		if err != nil || len(res) != 1 || res[0].Int() != 32*1024 {
			t.Fatalf("dynamic native call failed: res=%v err=%v", res, err)
		}
		if live := s.ArenaStats().GoRefsLive; live != baseRefs+2 {
			t.Fatalf("expected GoRefsLive=%d while userdata+native live, got %d", baseRefs+2, live)
		}
	}()

	s.SetGlobal("HOST_UD", NilValue())
	s.SetGlobal("HOST_FN", NilValue())
	s.CollectFull()

	if s.IsLive(udCanary) {
		t.Fatalf("unreachable userdata remained live in Arena after CollectFull")
	}
	if s.IsLive(fnCanary) {
		t.Fatalf("unreachable dynamic native remained live in Arena after CollectFull")
	}
	if live := s.ArenaStats().GoRefsLive; live != baseRefs {
		t.Fatalf("expected GoRefsLive to return to %d after sweep, got %d", baseRefs, live)
	}

	reclaimed := false
	for range 20 {
		runtime.GC()
		if wpUD.Value() == nil && wpFn.Value() == nil {
			reclaimed = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !reclaimed {
		t.Fatalf("Go host payloads bridged via GoRefs were not reclaimed by Go GC (ud=%v fn=%v)",
			wpUD.Value() != nil, wpFn.Value() != nil)
	}
}

// TestPhase4InlineCacheSweepAndShapeReuse verifies that Arena GC PreSweep invalidates
// stale monomorphic and polymorphic inline-cache entries before dead Shapes are swept and
// their Arena slots reused for unrelated new Shapes.
func TestPhase4InlineCacheSweepAndShapeReuse(t *testing.T) {
	s := NewState()
	defer s.Close()

	cl, err := s.LoadChunk([]byte(`
		local function getX(t) return t.x end
		local function setZ(t, v) t.z = v end

		-- Phase 1: warm monomorphic & polymorphic ICs with ephemeral shapes and prototypes.
		for round = 1, 20 do
			local proto = { x = "round_" .. round }
			local mt = { __index = proto }
			local o1 = setmetatable({ ["a" .. round] = 1 }, mt)
			local o2 = setmetatable({ ["b" .. round] = 2 }, mt)
			assert(getX(o1) == "round_" .. round)
			assert(getX(o2) == "round_" .. round)
			setZ(o1, round * 10)
			assert(o1.z == round * 10)
		end

		-- Sweep all dead shapes and proto values; Arena will reuse freed Shape slots.
		collectgarbage("collect")

		-- Phase 2: allocate completely different shapes reusing freed Arena slots.
		for round = 1, 20 do
			local fresh = { ["q" .. round] = 100, x = 900 + round }
			assert(getX(fresh) == 900 + round, "stale IC hit after Shape slot reuse at round " .. round)
			setZ(fresh, "ok_" .. round)
			assert(fresh.z == "ok_" .. round)
		end
		return "ok"
	`), "=ic_shape_reuse")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Call(cl)
	if err != nil || len(res) != 1 || s.AsString(res[0]).Str() != "ok" {
		t.Fatalf("IC sweep & shape reuse failed: res=%v err=%v", res, err)
	}
}

// TestPhase4CloneSnapshotStateIsolation verifies that concurrent States cloned via
// Arena.CloneSnapshot() share frozen stdlib strings safely while keeping all mutable
// stdlib tables, metatables, shapes, and GC state strictly isolated per VM.
func TestPhase4CloneSnapshotStateIsolation(t *testing.T) {
	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s := NewState()
			defer s.Close()

			script := fmt.Sprintf(`
				-- Mutate stdlib tables, metatables, and package state uniquely per VM.
				math.worker_id = %d
				string.worker_tag = "vm_%d"
				package.loaded["mod_%d"] = { id = %d }
				local smt = getmetatable("")
				smt.__index.custom_%d = function(self) return self .. ":%d" end

				if (%d %% 2) == 0 then
					collectgarbage("generational")
				else
					collectgarbage("incremental")
				end
				local t = {}
				for i = 1, 200 do
					t["k" .. i] = ("hi"):custom_%d()
				end
				collectgarbage("collect")

				assert(math.worker_id == %d)
				assert(string.worker_tag == "vm_%d")
				assert(package.loaded["mod_%d"].id == %d)
				assert(t.k200 == "hi:%d")
				-- Ensure no other worker's mutations leaked into this VM.
				local other = (%d + 1) %% %d
				assert(package.loaded["mod_" .. other] == nil)
				assert(smt.__index["custom_" .. other] == nil)
				return "ok"
			`, id, id, id, id, id, id, id, id, id, id, id, id, id, id, workers)

			cl, err := s.LoadChunk([]byte(script), fmt.Sprintf("=iso_%d", id))
			if err != nil {
				errs <- fmt.Errorf("worker %d load: %w", id, err)
				return
			}
			res, err := s.Call(cl)
			if err != nil || len(res) != 1 || s.AsString(res[0]).Str() != "ok" {
				errs <- fmt.Errorf("worker %d call: res=%v err=%v", id, res, err)
				return
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestPhase4HighDensityMultiVMGoGCIsolation exercises many live concurrent States holding
// rich Arena-backed object graphs across Go GC cycles and verifies clean teardown.
func TestPhase4HighDensityMultiVMGoGCIsolation(t *testing.T) {
	const numVMs = 128
	states := make([]*State, numVMs)
	for i := range numVMs {
		s := NewState()
		cl, err := s.LoadChunk([]byte(fmt.Sprintf(`
			STATE = {}
			for j = 1, 50 do
				STATE[j] = { id = %d, j = j, text = "item_" .. %d .. "_" .. j }
			end
			function SUM()
				local acc = 0
				for j = 1, #STATE do
					acc = acc + STATE[j].id + STATE[j].j
				end
				return acc
			end
		`, i, i)), fmt.Sprintf("=vm_%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(cl); err != nil {
			t.Fatal(err)
		}
		states[i] = s
	}

	runtime.GC()

	var wg sync.WaitGroup
	errs := make(chan error, numVMs)
	for i, s := range states {
		wg.Add(1)
		go func(id int, st *State) {
			defer wg.Done()
			v, err := st.Call1(st.GetGlobal("SUM"))
			want := int64(50*id + 50*51/2)
			if err != nil || !v.IsInt() || v.Int() != want {
				errs <- fmt.Errorf("vm %d SUM = (%v, %v), want %d", id, v, err, want)
				return
			}
			st.CollectFull()
		}(i, s)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	for i, s := range states {
		s.Close()
		st := s.ArenaStats()
		if st.BackingBytes != 0 || st.LiveObjects != 0 || st.GoRefsLive != 0 {
			t.Fatalf("vm %d did not release Arena/GoRefs on Close: %+v", i, st)
		}
	}
	// Ensure strings import is used in a sanity assertion on frozen stdlib strings.
	sCheck := NewState()
	defer sCheck.Close()
	if !strings.HasPrefix(sCheck.FormatValue(sCheck.GetGlobal("_VERSION")), "Lua 5.5") {
		t.Fatalf("unexpected _VERSION: %s", sCheck.FormatValue(sCheck.GetGlobal("_VERSION")))
	}
}

func TestPhase4ShapeTransitionCacheCapsWithoutOverflow(t *testing.T) {
	s := NewState()
	defer s.Close()

	// Disable automatic GC while creating 33,000 tables with distinct first short-string keys
	// sharing the root shape; transition cache must cap cleanly instead of overflowing uint16.
	s.collector.Stop()
	const n = 33000
	holder := s.NewTable(n, 0)
	s.SetGlobal("HOLDER", TableValue(holder))
	for i := range n {
		tbl := s.NewTable(0, 0)
		k := s.Intern(fmt.Sprintf("k%d", i))
		tbl.RawSetStr(s, k, IntValue(int64(i+1)))
		holder.RawSetInt(s, int64(i+1), TableValue(tbl))
	}

	rootSh := arena.AsSmall[Shape](s.arena, s.shapes.rootRef)
	if int(rootSh.transCap) > maxShapeTransCap || int(rootSh.transLen) > maxShapeTransCap {
		t.Fatalf("root shape transition cache exceeded cap: len=%d cap=%d max=%d",
			rootSh.transLen, rootSh.transCap, maxShapeTransCap)
	}
	// Spot-check early, boundary, and late tables to ensure every uncached child shape works properly.
	for _, idx := range []int{0, 3, 4, maxShapeTransCap - 1, maxShapeTransCap, maxShapeTransCap + 1, n - 1} {
		tv := holder.RawGetInt(s, int64(idx+1))
		if !tv.IsTable() {
			t.Fatalf("holder[%d] is not a table: %v", idx+1, tv)
		}
		got := s.asTable(tv).RawGetStr(s, s.Intern(fmt.Sprintf("k%d", idx)))
		if !got.IsInt() || got.Int() != int64(idx+1) {
			t.Fatalf("table %d key k%d = %v, want %d", idx, idx, got, idx+1)
		}
	}
}

func TestPhase4LoadChunkICMemoryPreflightAccountsArenaRounding(t *testing.T) {
	if !OptimizeEnabled {
		t.Skip("IC tables are only allocated when OptimizeEnabled is true")
	}
	s := NewState()
	defer s.Close()

	var sb strings.Builder
	sb.WriteString("local n = 0\n")
	for range 47 {
		sb.WriteString("n = n + math.pi\n")
	}
	sb.WriteString("return n\n")

	p, err := s.compilerFn("=ic_preflight", sb.String())
	if err != nil {
		t.Fatal(err)
	}
	if p.ICCount < 94 {
		t.Fatalf("expected >= 94 IC entries for 47 math.pi ops, got %d", p.ICCount)
	}

	s.CollectFull()
	base := s.MemoryUsage()

	// Unrounded logical IC size (4,512B for 94 entries) is strictly smaller than its 6,144B Arena slot;
	// loading under a limit equal to the unrounded estimate must fail rather than overshooting the limit.
	unroundedICBytes := int64(p.ICCount) * int64(unsafe.Sizeof(ICEntry{}))
	roundedICBytes := arenaAllocSize(unroundedICBytes)
	if roundedICBytes <= unroundedICBytes {
		t.Fatalf("expected Arena rounding gap for ICCount=%d: unrounded=%d rounded=%d",
			p.ICCount, unroundedICBytes, roundedICBytes)
	}
	preflight := loadPreflightBytes(p, true)
	unroundedLimit := base + preflight - (roundedICBytes - unroundedICBytes)
	s.SetMemoryLimit(unroundedLimit)
	if _, err := s.Load(p); err == nil {
		t.Fatalf("Load unexpectedly succeeded under unrounded limit=%d (usage=%d, preflight=%d)",
			unroundedLimit, s.MemoryUsage(), preflight)
	}
	if usage := s.MemoryUsage(); usage > unroundedLimit {
		t.Fatalf("MemoryUsage=%d exceeded limit=%d after rejected Load", usage, unroundedLimit)
	}

	// Setting the limit to base + preflight must succeed and stay within budget.
	s.SetMemoryLimit(base + preflight)
	cl, err := s.Load(p)
	if err != nil {
		t.Fatalf("Load failed at exact preflight budget=%d: %v", base+preflight, err)
	}
	if cl == nil {
		t.Fatal("Load returned nil closure")
	}
	if usage := s.MemoryUsage(); usage > base+preflight {
		t.Fatalf("Load succeeded while exceeding limit: usage=%d > limit=%d", usage, base+preflight)
	}
}
