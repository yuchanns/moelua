package vm_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/yuchanns/moelua/vm"
)

func TestArenaStringsInternSweepAutoPacingAndClose(t *testing.T) {
	s := vm.NewState()

	// Freshly cloned State should lazily defer Arena allocation until first dynamic string.
	st0 := s.ArenaStats()
	if st0.AllocBytes != 0 || st0.LiveObjects != 0 || st0.BackingBytes != 0 {
		t.Fatalf("expected fresh State to have lazy empty Arena, got %+v", st0)
	}

	// Interning a frozen stdlib short string must NOT allocate in the per-State Arena.
	frozen := s.Intern("tostring")
	if !frozen.IsShort() || !frozen.IsFixed() || frozen.Str() != "tostring" {
		t.Fatalf("expected frozen stdlib string, got short=%v fixed=%v str=%q",
			frozen.IsShort(), frozen.IsFixed(), frozen.Str())
	}
	if st := s.ArenaStats(); st.LiveObjects != 0 {
		t.Fatalf("frozen stdlib lookup unexpectedly allocated in per-State Arena: %+v", st)
	}

	// Dynamic short strings deduplicate by pointer equality and live in the per-State Arena.
	a := s.Intern("arena_short_key_1")
	b := s.Intern("arena_short_key_1")
	if a != b || !a.IsShort() || a.IsFixed() || a.Str() != "arena_short_key_1" {
		t.Fatalf("short string interning mismatch: a=%p b=%p short=%v fixed=%v", a, b, a.IsShort(), a.IsFixed())
	}

	// Dynamic long string (> 32 KiB) lives in a dedicated large span released back to Go on sweep.
	big := s.Intern(strings.Repeat("M", 40000))
	if big.IsShort() || big.Len != 40000 {
		t.Fatalf("long string metadata mismatch: short=%v len=%d", big.IsShort(), big.Len)
	}
	wpBig := weak.Make(big)
	big = nil

	stMid := s.ArenaStats()
	if stMid.ActiveLargeSpans != 1 || stMid.DynamicShortStrs < 1 || stMid.AllocBytes < 40000 {
		t.Fatalf("unexpected mid ArenaStats: %+v", stMid)
	}

	// CollectFull sweeps unreachable short and large strings and frees the large span back to Go.
	s.CollectFull()
	stAfter := s.ArenaStats()
	if stAfter.ActiveLargeSpans != 0 || stAfter.DynamicShortStrs != 0 || stAfter.LiveObjects != 0 {
		t.Fatalf("expected all unrooted Arena strings swept, got %+v", stAfter)
	}
	reclaimed := false
	for range 20 {
		runtime.GC()
		if wpBig.Value() == nil {
			reclaimed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !reclaimed {
		t.Fatalf("large Arena string span not reclaimed by Go GC after sweep")
	}

	// Automatic Tier-0 pacing: a loop creating 40,000 ephemeral strings without collectgarbage()
	// must stay bounded in Arena memory rather than growing unboundedly.
	cl, err := s.LoadChunk([]byte(`
		local last
		for i = 1, 40000 do
			last = "ephem_key_" .. tostring(i)
		end
		return last
	`), "=arena_autopace")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Call(cl)
	if err != nil || len(res) != 1 || s.AsString(res[0]).Str() != "ephem_key_40000" {
		t.Fatalf("auto-pace loop failed: res=%v err=%v", res, err)
	}
	if st := s.ArenaStats(); st.AllocBytes > 1024*1024 {
		t.Fatalf("expected Tier-0 automatic Arena string sweep to bound AllocBytes <= 1 MiB, got %d", st.AllocBytes)
	}

	// State.Close releases all Arena backing memory immediately.
	s.Close()
	if st := s.ArenaStats(); st.BackingBytes != 0 || st.LiveObjects != 0 || st.DynamicShortStrs != 0 {
		t.Fatalf("expected zero Arena backing bytes after Close, got %+v", st)
	}
}

func TestArenaStringsConcurrentStatesRaceFree(t *testing.T) {
	const workers = 8
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s := vm.NewState()
			defer s.Close()
			cl, err := s.LoadChunk([]byte(`
				local t = {}
				for i = 1, 500 do
					local k = "worker_" .. tostring(i)
					t[k] = string.rep("a", 64) .. tostring(i)
				end
				local sum = 0
				for k, v in pairs(t) do
					sum = sum + #k + #v
				end
				t = nil
				collectgarbage("collect")
				return sum
			`), fmt.Sprintf("=worker_%d", id))
			if err != nil {
				errCh <- err
				return
			}
			res, err := s.Call(cl)
			if err != nil || len(res) != 1 || res[0].Int() <= 0 {
				errCh <- fmt.Errorf("worker %d failed: res=%v err=%v", id, res, err)
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestStateCall1FastPathAndHighWaterCleanup(t *testing.T) {
	s := vm.NewState()
	defer s.Close()

	cl, err := s.LoadChunk([]byte(`
		return function(a, b) return a * 3 + b end,
		       function() end,
		       function(x) return x, x + 1 end,
		       function(...) return select("#", ...) end,
		       function() error("boom", 0) end
	`), "=call1")
	if err != nil {
		t.Fatal(err)
	}
	fns, err := s.Call(cl)
	if err != nil || len(fns) != 5 {
		t.Fatalf("setup failed: %v %v", fns, err)
	}

	// Fast single-return Lua closure.
	for i := range int64(100) {
		v, err := s.Call1(fns[0], vm.IntValue(i), vm.IntValue(7))
		if err != nil || v.Int() != i*3+7 {
			t.Fatalf("Call1(%d) = (%v, %v)", i, v, err)
		}
	}
	// Zero-return closure returns NilValue.
	if v, err := s.Call1(fns[1]); err != nil || !v.IsNil() {
		t.Fatalf("Call1 zero-return = (%v, %v), want nil", v, err)
	}
	// Multi-return closure returns first result.
	if v, err := s.Call1(fns[2], vm.IntValue(41)); err != nil || v.Int() != 41 {
		t.Fatalf("Call1 multi-return = (%v, %v), want 41", v, err)
	}
	// Vararg closure falls back cleanly.
	if v, err := s.Call1(fns[3], vm.IntValue(1), vm.IntValue(2), vm.IntValue(3)); err != nil || v.Int() != 3 {
		t.Fatalf("Call1 vararg = (%v, %v), want 3", v, err)
	}
	// Error propagation preserves LuaError cleanly.
	if _, err := s.Call1(fns[4]); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Call1 error = %v, want boom", err)
	}

	// High-water cleanup must NOT retain a temporary table argument across Call1/Call!
	var wp weak.Pointer[vm.Table]
	func() {
		idCl, err := s.LoadChunk([]byte(`return function(x) local y = x; return 1 end`), "=hwm")
		if err != nil {
			t.Fatal(err)
		}
		idFn, err := s.Call1(idCl)
		if err != nil {
			t.Fatal(err)
		}
		tbl := s.NewTable(0, 2)
		wp = weak.Make(tbl)
		if v, err := s.Call1(idFn, vm.TableValue(tbl)); err != nil || v.Int() != 1 {
			t.Fatalf("Call1(idFn) failed: %v %v", v, err)
		}
		tbl = nil
	}()
	reclaimed := false
	for range 12 {
		runtime.GC()
		if wp.Value() == nil {
			reclaimed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !reclaimed {
		t.Fatalf("Call1 high-water cleanup retained temporary table argument on Thread stack")
	}
}

func TestFormatErrorClonesArenaStrings(t *testing.T) {
	s := vm.NewState()
	defer s.Close()

	// 1. Direct string error object (20-byte dynamic Arena short string).
	errMsg := "alpha_err_msg_123456"
	var savedDirect string
	func() {
		cl, err := s.LoadChunk([]byte(`error(string.sub("ZZalpha_err_msg_123456", 3), 0)`), "=fmt_err_direct")
		if err != nil {
			t.Fatal(err)
		}
		_, cerr := s.Call(cl)
		if cerr == nil {
			t.Fatal("expected Lua error")
		}
		savedDirect = s.FormatError(cerr)
	}()
	if savedDirect != errMsg {
		t.Fatalf("FormatError direct = %q, want %q", savedDirect, errMsg)
	}

	// 2. Non-string error object formatted via __tostring returning a dynamic Arena string.
	var savedMeta string
	func() {
		cl, err := s.LoadChunk([]byte(`
			local mt = { __tostring = function() return string.sub("YYmeta_err_msg_9876543", 3) end }
			error(setmetatable({}, mt))
		`), "=fmt_err_meta")
		if err != nil {
			t.Fatal(err)
		}
		_, cerr := s.Call(cl)
		if cerr == nil {
			t.Fatal("expected Lua error")
		}
		savedMeta = s.FormatError(cerr)
	}()
	if savedMeta != "meta_err_msg_9876543" {
		t.Fatalf("FormatError __tostring = %q, want %q", savedMeta, "meta_err_msg_9876543")
	}

	// Sweep the unreachable error strings and reuse their 64-byte Arena slots with fresh 20-byte strings.
	s.CollectFull()
	for i := range 64 {
		_ = s.Intern(fmt.Sprintf("overwrite_slot_%05d", i))
	}
	if savedDirect != errMsg {
		t.Fatalf("FormatError direct string was mutated after Arena slot reuse: got %q, want %q", savedDirect, errMsg)
	}
	if savedMeta != "meta_err_msg_9876543" {
		t.Fatalf("FormatError __tostring string was mutated after Arena slot reuse: got %q, want %q", savedMeta, "meta_err_msg_9876543")
	}
}

func TestArenaStringAutoPaceInNativeCallLoops(t *testing.T) {
	s := vm.NewState()
	defer s.Close()

	var peakDirect, peakPCall, peakGMatch int64
	s.SetGlobal("sample_arena", s.NativeValue(&vm.NativeFunc{
		Name: "sample_arena",
		FastFn: func(st *vm.State, args []vm.Value) (vm.Value, error) {
			cur := st.ArenaStats().AllocBytes
			switch args[0].Int() {
			case 1:
				if cur > peakDirect {
					peakDirect = cur
				}
			case 2:
				if cur > peakPCall {
					peakPCall = cur
				}
			case 3:
				if cur > peakGMatch {
					peakGMatch = cur
				}
			}
			return vm.IntValue(cur), nil
		},
	}))

	// 1. Tight Tier-0 loop allocating strings via string.rep (no OP_CONCAT).
	cl, err := s.LoadChunk([]byte(`
		local last
		for i = 1, 100000 do
			last = string.rep("x", 100)
			if i % 1000 == 0 then
				sample_arena(1)
			end
		end
		-- 2. Same loop inside pcall (pcallSlow native return path).
		for i = 1, 20000 do
			local ok, v = pcall(string.rep, "y", 100)
			assert(ok)
			last = v
			if i % 1000 == 0 then
				sample_arena(2)
			end
		end
		-- 3. Native iterator loop via string.gmatch (tforCallSlow native return path).
		local big = string.rep(string.rep("z", 80) .. " ", 10000)
		local n = 0
		for w in string.gmatch(big, "%S+") do
			last = w
			n = n + 1
			if n % 1000 == 0 then
				sample_arena(3)
			end
		end
		return #last, n
	`), "=native_arena_pace")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Call(cl)
	if err != nil || len(res) != 2 || res[0].Int() != 80 || res[1].Int() != 10000 {
		t.Fatalf("native loop failed: res=%v err=%v", res, err)
	}
	const maxAllowed = 4 * 1024 * 1024
	if peakDirect > maxAllowed {
		t.Fatalf("direct string.rep loop exceeded bounded Arena memory: peak=%d bytes", peakDirect)
	}
	if peakPCall > maxAllowed {
		t.Fatalf("pcall(string.rep) loop exceeded bounded Arena memory: peak=%d bytes", peakPCall)
	}
	if peakGMatch > maxAllowed {
		t.Fatalf("string.gmatch iterator loop exceeded bounded Arena memory: peak=%d bytes", peakGMatch)
	}
}

func TestIOOpenClonesArenaFilename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "arena_open_test.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := vm.NewState()
	defer s.Close()
	s.SetGlobal("test_dir", vm.StringValue(s.Intern(dir)))
	s.SetGlobal("test_sep", vm.StringValue(s.Intern(string(filepath.Separator))))

	cl, err := s.LoadChunk([]byte(`
		local p = test_dir .. test_sep .. "arena_open_test.txt"
		local plen = #p
		local f, oerr = io.open(p, "r")
		assert(f, oerr)
		p = nil
		collectgarbage("collect")
		-- Allocate equal-length dynamic strings to overwrite freed Arena slots.
		for i = 1, 128 do
			local _ = string.rep("X", plen - 3) .. string.format("%03d", i)
		end
		local ok, werr = f:write("fail")
		f:close()
		assert(not ok, "expected write to read-only file to fail")
		return werr
	`), "=io_open_clone")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Call(cl)
	if err != nil || len(res) != 1 || !res[0].IsString() {
		t.Fatalf("io.open test failed: res=%v err=%v", res, err)
	}
	got := vm.FormatValue(res[0])
	if !strings.Contains(got, path) {
		t.Fatalf("expected file write error to retain real path %q, got %q", path, got)
	}
	if strings.Contains(got, "XXXXX") {
		t.Fatalf("file handle path was overwritten by reused Arena slot: %q", got)
	}
}
