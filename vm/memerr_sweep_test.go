package vm

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// TestMemErrSweepVM implements a memerr.lua-style critical memory-limit sweep across
// allocating VM operations. For each operation, it sweeps memLimit across tight deltas
// above the post-GC baseline and verifies:
//   - zero panics
//   - zero leaked memExceeded / abortErr flags after the call
//   - subsequent no-alloc operations do not fail spuriously
//   - if the operation succeeds, post-GC MemoryUsage() stays within the configured limit
//   - operation-specific invariants (e.g. os.date("*t") never returns a truncated table)
func TestMemErrSweepVM(t *testing.T) {
	cases := []struct {
		name  string
		setup string
		op    string
		check string // optional assertion run when ok == true (with result in _res)
	}{
		{
			name: "table_create",
			op:   `return table.create(200, 32)`,
		},
		{
			name: "table_pack",
			op:   `return table.pack(1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20)`,
			check: `assert(type(_res) == "table" and _res.n == 20 and _res[20] == 20)`,
		},
		{
			name: "os_date_table",
			op:   `return os.date("*t", 1700000000)`,
			check: `
				assert(type(_res) == "table")
				for _, k in ipairs({"year","month","day","hour","min","sec","wday","yday","isdst"}) do
					assert(_res[k] ~= nil, "truncated os.date table missing field: " .. k)
				end
			`,
		},
		{
			name: "shape_transitions_and_dict_demote",
			op: `
				local t = {}
				for i = 1, 80 do
					t["k" .. i] = i
				end
				return t
			`,
			check: `assert(_res.k1 == 1 and _res.k80 == 80)`,
		},
		{
			name: "typed_array_grow_and_promote",
			op: `
				local t = {}
				for i = 1, 200 do t[i] = i end
				t[50] = "promote"
				return t
			`,
			check: `assert(_res[1] == 1 and _res[50] == "promote" and _res[200] == 200)`,
		},
		{
			name: "hash_growth",
			op: `
				local t = {}
				for i = 1, 80 do t[i * 1000] = i end
				return t
			`,
			check: `assert(_res[1000] == 1 and _res[80000] == 80)`,
		},
		{
			name:  "concat_fast_and_slow",
			setup: `local a = string.rep("a", 64); local mt = setmetatable({}, {__concat = function(x, y) return tostring(y) end})`,
			op:    `local s = a .. a .. a .. a; return mt .. s .. a`,
			check: `assert(type(_res) == "string")`,
		},
		{
			name:  "string_gsub_and_format",
			setup: `local base = string.rep("ab", 120)`,
			op:    `local s = string.gsub(base, "a", "XYZ"); return string.format("%q", s)`,
			check: `assert(type(_res) == "string" and #_res > 200)`,
		},
		{
			name:  "string_gmatch",
			setup: `local src = "one two three four five"`,
			op: `
				local out = {}
				for w in string.gmatch(src, "%w+") do out[#out+1] = w end
				return out
			`,
			check: `assert(#_res == 5 and _res[5] == "five")`,
		},
		{
			name:  "string_pack_unpack",
			setup: `local packed = string.pack("s2s2s2", string.rep("x", 100), string.rep("y", 100), string.rep("z", 100))`,
			op:    `local a, b, c = string.unpack("s2s2s2", packed); return a .. b .. c`,
			check: `assert(#_res == 300)`,
		},
		{
			name: "load_chunk",
			op:   `local f = assert(load("local x = {1,2,3,4,5}; return x[5]")); return f()`,
			check: `assert(_res == 5)`,
		},
		{
			name: "coroutine_create_resume_stack_grow",
			op: `
				local function rec(n) if n <= 0 then return 1 end local a,b,c,d = n,n,n,n return rec(n-1)+a-a+b-b+c-c+d-d end
				local co = coroutine.create(function() return rec(60) end)
				local ok, v = coroutine.resume(co)
				if not ok then error(v) end
				return v
			`,
			check: `assert(_res == 1)`,
		},
		{
			name: "setmetatable_with_meta",
			op: `
				local t = {a = 1, b = 2}
				local mt = {__index = {c = 3}}
				setmetatable(t, mt)
				return t.c
			`,
			check: `assert(_res == 3)`,
		},
		{
			name:  "vararg_table",
			setup: `local function make(...t) return t end`,
			op:    `return make(1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20)`,
			check: `assert(_res.n == 20 and _res[20] == 20)`,
		},
	}

	deltas := []int64{0, 64, 128, 256, 384, 512, 768, 1024, 1536, 2048, 3072, 4096, 6144, 8192, 16384, 32768, 65536, 131072, 262144}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sawOK := false
			sawOOM := false
			for _, delta := range deltas {
				s := NewState()
				src := fmt.Sprintf(`
					%s
					_sentinel = 1
					function _run()
						%s
					end
					function _verify(_res)
						%s
					end
				`, tc.setup, tc.op, tc.check)
				cl, err := s.LoadChunk([]byte(src), "=memerr_setup")
				if err != nil {
					s.Close()
					t.Fatalf("load setup: %v", err)
				}
				if _, err := s.Call(cl); err != nil {
					s.Close()
					t.Fatalf("run setup: %v", err)
				}
				// Enable a generous limit before measuring baseline so CollectFull counts
				// reachable strings consistently with active memLimit accounting.
				s.SetMemoryLimit(1 << 30)
				s.CollectFull()
				base := s.MemoryUsage()
				limit := base + delta
				s.SetMemoryLimit(limit)

				runFn := s.GetGlobal("_run")
				res, callErr := s.Call(runFn)

				// 1. Never leak memExceeded / abortErr after a call.
				if leaked := s.ConsumeMemErr(); leaked != nil {
					s.Close()
					t.Fatalf("delta=%d: leaked VM mem/abort error: %v", delta, leaked)
				}

				// 2. Subsequent unrelated no-alloc global update must not fail spuriously.
				s.GlobalEnv().RawSetStr(s, s.Intern("_sentinel"), IntValue(2))
				if err := s.ConsumeMemErr(); err != nil {
					s.Close()
					t.Fatalf("delta=%d: spurious error on no-alloc write after callErr=%v: %v", delta, callErr, err)
				}

				if callErr != nil {
					if !strings.Contains(callErr.Error(), "not enough memory") {
						s.Close()
						t.Fatalf("delta=%d: unexpected non-OOM error: %v", delta, callErr)
					}
					sawOOM = true
				} else {
					sawOK = true
					val := NilValue()
					if len(res) > 0 {
						val = res[0]
					}
					// Root result via Ref before verifying post-GC live bytes stay <= limit.
					refID := s.Ref(val)
					s.CollectFull()
					after := s.MemoryUsage()
					s.Unref(refID)
					if after > limit {
						s.Close()
						t.Fatalf("delta=%d: succeeded with post-GC MemoryUsage=%d > limit=%d", delta, after, limit)
					}
					if tc.check != "" {
						s.SetMemoryLimit(0)
						if _, verr := s.Call(s.GetGlobal("_verify"), val); verr != nil {
							s.Close()
							t.Fatalf("delta=%d: invariant check failed on success: %v", delta, verr)
						}
					}
				}
				s.Close()
			}
			if !sawOOM {
				t.Errorf("%s: sweep never hit OOM (deltas may be too large)", tc.name)
			}
			if !sawOK {
				t.Errorf("%s: sweep never succeeded (deltas may be too small)", tc.name)
			}
		})
	}
}

// TestMemErrAddFSAndAddSearcherPreservesSearchers verifies that AddFS and AddSearcher
// under tight memory limits never corrupt package.searchers or leak memExceeded.
func TestMemErrAddFSAndAddSearcherPreservesSearchers(t *testing.T) {
	for _, delta := range []int64{0, 16, 48, 96, 160, 256, 512, 2048} {
		s := NewState()
		s.CollectFull()
		origSearchers := s.AsTable(s.AsTable(s.GetGlobal("package")).RawGetStr(s, s.Intern("searchers")))
		origLen := origSearchers.Len(s)
		origVals := make([]Value, origLen)
		for i := int64(1); i <= origLen; i++ {
			origVals[i-1] = origSearchers.RawGetInt(s, i)
		}

		s.SetMemoryLimit(s.MemoryUsage() + delta)
		s.AddFS(fstest.MapFS{"m.lua": {Data: []byte("return 1")}})
		if leaked := s.ConsumeMemErr(); leaked != nil {
			s.Close()
			t.Fatalf("AddFS delta=%d leaked mem error: %v", delta, leaked)
		}

		// Every original searcher must still be present (either in-place on OOM, or shifted +1 on success).
		curLen := origSearchers.Len(s)
		if curLen != origLen && curLen != origLen+1 {
			s.Close()
			t.Fatalf("AddFS delta=%d corrupted searchers length: orig=%d cur=%d", delta, origLen, curLen)
		}
		if curLen == origLen {
			for i := int64(1); i <= origLen; i++ {
				if origSearchers.RawGetInt(s, i) != origVals[i-1] {
					s.Close()
					t.Fatalf("AddFS delta=%d OOM overwrote searcher slot %d", delta, i)
				}
			}
		} else {
			if origSearchers.RawGetInt(s, 1) != origVals[0] {
				s.Close()
				t.Fatalf("AddFS delta=%d moved preload searcher", delta)
			}
			for i := int64(2); i <= origLen; i++ {
				if origSearchers.RawGetInt(s, i+1) != origVals[i-1] {
					s.Close()
					t.Fatalf("AddFS delta=%d lost searcher %d", delta, i)
				}
			}
		}

		// Also exercise AddSearcher under tight memory.
		beforeLen := origSearchers.Len(s)
		nf := &NativeFunc{Name: "custom_searcher", Fn: func(*State, []Value) ([]Value, error) { return nil, nil }}
		s.AddSearcher(nf)
		if leaked := s.ConsumeMemErr(); leaked != nil {
			s.Close()
			t.Fatalf("AddSearcher delta=%d leaked mem error: %v", delta, leaked)
		}
		if cur := origSearchers.Len(s); cur != beforeLen && cur != beforeLen+1 {
			s.Close()
			t.Fatalf("AddSearcher delta=%d corrupted searchers length: before=%d cur=%d", delta, beforeLen, cur)
		}
		s.Close()
	}
}

// TestTier1GCDebtWithoutMemLimit verifies that table growth, slow concat, io.read,
// and vararg/thread allocations all accumulate Tier-1 GC debt and trigger __gc/weak
// collection even when memLimit == 0 (matching PUC-Rio Lua 5.5.1).
func TestTier1GCDebtWithoutMemLimit(t *testing.T) {
	t.Run("table_growth", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local fired = false
			setmetatable({}, {__gc = function() fired = true end})
			local wk = setmetatable({}, {__mode = "v"}); wk[1] = {}
			for i = 1, 80 do
				local t = {}
				for j = 1, 2000 do t[j] = j end
				if fired and wk[1] == nil then break end
			end
			assert(fired, "__gc must fire from table array growth debt without memLimit")
			assert(wk[1] == nil, "weak value must clear from table growth debt")
			return "ok"
		`), "=debt_table")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(cl); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("slow_concat", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local obj = setmetatable({}, {__concat = function() return 1 end})
			local big = string.rep("x", 8192)
			collectgarbage()
			local fired = false
			setmetatable({}, {__gc = function() fired = true end})
			for i = 1, 500 do
				local _ = obj .. big .. big
				if fired then break end
			end
			assert(fired, "__gc must fire from slow concat string debt without memLimit")
			return "ok"
		`), "=debt_concat")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(cl); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("io_read", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "lines.txt")
		line := strings.Repeat("a", 512) + "\n"
		var b strings.Builder
		b.Grow(len(line) * 1500)
		for range 1500 {
			b.WriteString(line)
		}
		if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}

		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(fmt.Sprintf(`
			local f = assert(io.open(%q, "r"))
			local fired = false
			setmetatable({}, {__gc = function() fired = true end})
			for i = 1, 1500 do
				local l = f:read("l")
				if not l or fired then break end
			end
			f:close()
			assert(fired, "__gc must fire from file:read('l') debt without memLimit")
			return "ok"
		`, tmp)), "=debt_ioread")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(cl); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("vararg_and_thread", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`
			local function mk(...t) return t end
			local fired = false
			setmetatable({}, {__gc = function() fired = true end})
			for i = 1, 1000 do
				local _ = mk(1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16)
				if fired then break end
			end
			assert(fired, "__gc must fire from OP_VARARGPREP debt without memLimit")
			return "ok"
		`), "=debt_vararg")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Call(cl); err != nil {
			t.Fatal(err)
		}
	})
}

// TestConcatFastPathRespectsTempBytes verifies that OP_CONCAT fast path accounts for
// active compiler tempBytes when checking memLimit (Track 2 P2-5 regression).
func TestConcatFastPathRespectsTempBytes(t *testing.T) {
	s := NewState()
	defer s.Close()
	const limit = int64(2_000_000)
	s.SetMemoryLimit(limit)
	cl, err := s.LoadChunk([]byte(`
		local x = string.rep("x", 16384)
		local keep = {}
		local n = 0
		local ok, err = pcall(function()
			local fn, lerr = load(function()
				n = n + 1
				if n == 1 then return "return '" end
				if n <= 40 then
					-- Build strings while compiler holds tempBytes for the unfinished literal.
					keep[#keep+1] = x .. x .. x .. x .. tostring(n)
					return string.rep("a", 8192)
				end
				if n == 41 then return "'" end
				return nil
			end)
			if not fn then error(lerr) end
		end)
		return ok, tostring(err)
	`), "=concat_tempbytes")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(cl); err != nil {
		t.Fatal(err)
	}
	if usage := s.MemoryUsage(); usage > limit {
		t.Fatalf("MemoryUsage=%d exceeded memLimit=%d after concat during load(reader)", usage, limit)
	}
}

// TestPackAndVarargHashAccounting verifies that table.pack and named vararg tables
// do not under-account their hash part in either optimization or faithful mode (Track 2 P2-7).
func TestPackAndVarargHashAccounting(t *testing.T) {
	s := NewState()
	defer s.Close()
	cl, err := s.LoadChunk([]byte(`
		local function mk(...t) return t end
		G_PACK = {}
		G_VAR = {}
		for i = 1, 200 do
			G_PACK[i] = table.pack(1,2,3,4,5,6,7,8,9,10)
			G_VAR[i] = mk(1,2,3,4,5,6,7,8,9,10)
		end
	`), "=pack_vararg_acct")
	if err != nil {
		t.Fatal(err)
	}
	clRef := s.Ref(cl)
	defer s.Unref(clRef)
	s.SetMemoryLimit(64 * 1024 * 1024)
	s.CollectFull()
	if _, err := s.Call(cl); err != nil {
		t.Fatal(err)
	}
	before := s.MemoryUsage()
	s.CollectFull()
	after := s.MemoryUsage()
	// Recount must not jump upward (no under-accounted hash nodes in faithful or opt mode).
	if after > before+1024 {
		t.Fatalf("under-accounted table.pack/vararg tables: beforeGC=%d afterGC=%d (+%d bytes)", before, after, after-before)
	}
}

// TestEpochCountersAre64Bit verifies that protoEpoch and dictGen are 64-bit and do not
// wrap at 2^32 to resurrect stale inline-cache entries (Track 2 P1-4 regression).
func TestEpochCountersAre64Bit(t *testing.T) {
	s := NewState()
	defer s.Close()
	// Start right below the 32-bit boundary and cross it while mutating a prototype table.
	s.protoEpoch = uint64(math.MaxUint32) - 1
	cl, err := s.LoadChunk([]byte(`
		local proto = { x = "old" }
		local o = setmetatable({}, { __index = proto })
		local function get(obj) return obj.x end
		assert(get(o) == "old") -- caches IC entry at epoch MaxUint32-1
		proto.x = "mid"         -- bumps to MaxUint32
		assert(get(o) == "mid")
		proto.x = "new"         -- bumps past 2^32 (would wrap to 0 on uint32)
		assert(get(o) == "new")
		proto.x = "final"
		assert(get(o) == "final")
		return "ok"
	`), "=epoch64")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(cl); err != nil {
		t.Fatal(err)
	}
	if s.protoEpoch <= uint64(math.MaxUint32) {
		t.Fatalf("expected 64-bit protoEpoch > MaxUint32, got %d", s.protoEpoch)
	}
}
