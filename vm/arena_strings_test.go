package vm_test

import (
	"fmt"
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
	if err != nil || len(res) != 1 || res[0].AsString().Str() != "ephem_key_40000" {
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
