package moelua_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yuchanns/moelua"
)

// TestEmbeddingAPIMemErrUnpinnedSweep implements a memerr.lua-style critical memory-limit
// sweep across all allocating Go embedding API methods while holding unpinned Go-side Lua
// values, verifying:
//   - emergency GC recounts never lose Go-held arguments/receivers (memory limit is never bypassed)
//   - post-GC MemoryUsage() stays <= memLimit whenever an operation succeeds
//   - stale memExceeded / abortErr flags never poison subsequent host writes
func TestEmbeddingAPIMemErrUnpinnedSweep(t *testing.T) {
	const limit = int64(2 * 1024 * 1024)

	buildHeavyTable := func(t *testing.T, s *moelua.State) *moelua.Table {
		t.Helper()
		tb, err := s.TryNewTable(20000, 0)
		if err != nil {
			t.Fatalf("TryNewTable: %v", err)
		}
		for i := int64(1); i <= 20000; i++ {
			if err := tb.SetInt(i, moelua.Float(float64(i)+0.5)); err != nil {
				t.Fatalf("SetInt: %v", err)
			}
		}
		return tb
	}

	fillToMargin := func(t *testing.T, s *moelua.State, margin int64) {
		t.Helper()
		s.CollectGarbage()
		avail := limit - s.MemoryUsage() - margin
		if avail > 64 {
			filler, err := s.TryString(strings.Repeat("x", int(avail-48)))
			if err != nil {
				t.Fatalf("filler TryString: %v", err)
			}
			if err := s.TrySetGlobal("_filler", filler); err != nil {
				t.Fatalf("TrySetGlobal _filler: %v", err)
			}
		}
	}

	t.Run("SetMetatable_roots_value_and_mt", func(t *testing.T) {
		s := moelua.New(moelua.WithMemoryLimit(limit))
		defer s.Close()
		// Create heavy table and fresh metatable held ONLY in Go variables (unpinned).
		tb := buildHeavyTable(t, s)
		mt, err := s.TryNewTable(0, 2)
		if err != nil {
			t.Fatal(err)
		}
		// Keep tb and mt temporarily rooted while creating filler, then unpin them to simulate
		// a caller holding Go-only Value handles right before SetMetatable.
		hTB := s.Pin(tb.Value())
		hMT := s.Pin(mt.Value())
		fillToMargin(t, s, 64)
		hTB.Release()
		hMT.Release()

		err = s.SetMetatable(tb.Value(), mt)
		if err == nil {
			// If it succeeded, live bytes including tb and mt must still fit within limit!
			h1 := s.Pin(tb.Value())
			h2 := s.Pin(mt.Value())
			s.CollectGarbage()
			usage := s.MemoryUsage()
			h1.Release()
			h2.Release()
			if usage > limit {
				t.Fatalf("SetMetatable bypassed memory limit: usage=%d > limit=%d", usage, limit)
			}
		} else if !strings.Contains(err.Error(), "not enough memory") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("Call_and_Resume_root_fn_and_args_across_stack_growth", func(t *testing.T) {
		s := moelua.New(moelua.WithMemoryLimit(limit))
		defer s.Close()
		res, err := s.DoString(`return function(...) return select("#", ...) end`)
		if err != nil {
			t.Fatal(err)
		}
		callee := res[0]
		tb := buildHeavyTable(t, s)

		hFn := s.Pin(callee)
		hTB := s.Pin(tb.Value())
		fillToMargin(t, s, 256)
		hFn.Release()
		hTB.Release()

		args := make([]moelua.Value, 2000)
		args[0] = tb.Value()
		for i := 1; i < len(args); i++ {
			args[i] = moelua.Nil()
		}
		if _, err := s.Call(callee, args...); err == nil {
			h1 := s.Pin(callee)
			h2 := s.Pin(tb.Value())
			s.CollectGarbage()
			usage := s.MemoryUsage()
			h1.Release()
			h2.Release()
			if usage > limit {
				t.Fatalf("Call bypassed memory limit across stack growth: usage=%d > limit=%d", usage, limit)
			}
		} else if !strings.Contains(err.Error(), "not enough memory") {
			t.Fatalf("unexpected Call error: %v", err)
		}

		// Also test Thread.Resume with unpinned heavy arg across stack growth.
		co, err := s.NewThread(callee)
		if err != nil {
			if !strings.Contains(err.Error(), "not enough memory") {
				t.Fatalf("unexpected NewThread error: %v", err)
			}
			return
		}
		if _, _, err := co.Resume(args...); err == nil {
			h1 := s.Pin(tb.Value())
			s.CollectGarbage()
			usage := s.MemoryUsage()
			h1.Release()
			if usage > limit {
				t.Fatalf("Thread.Resume bypassed memory limit across stack growth: usage=%d > limit=%d", usage, limit)
			}
		} else if !strings.Contains(err.Error(), "not enough memory") {
			t.Fatalf("unexpected Resume error: %v", err)
		}
	})

	t.Run("Preload_TryNewMetatable_NewUserdataWithMeta_AddSearcher_sweep", func(t *testing.T) {
		for _, delta := range []int64{0, 64, 128, 256, 512, 1024, 2048, 8192} {
			s := moelua.New(moelua.WithMemoryLimit(1 << 30))
			if err := s.TrySetGlobal("_sentinel", moelua.Int(1)); err != nil {
				s.Close()
				t.Fatal(err)
			}
			s.CollectGarbage()
			lim := s.MemoryUsage() + delta
			s.SetMemoryLimit(lim)

			_ = s.Preload("mod_sweep", func(_ *moelua.State, _ []moelua.Value) ([]moelua.Value, error) {
				return []moelua.Value{moelua.Int(1)}, nil
			})
			mt, _, _ := s.TryNewMetatable("mt_sweep")
			if mt != nil {
				_, _ = s.NewUserdataWithMeta("payload", mt)
			}
			s.AddSearcher("srch_sweep", func(_ *moelua.State, _ []moelua.Value) ([]moelua.Value, error) {
				return nil, nil
			})
			s.AddFS(fstest.MapFS{"x.lua": {Data: []byte("return 1")}})

			// No-alloc write to existing global must NEVER fail with a leaked mem error.
			if err := s.TrySetGlobal("_sentinel", moelua.Int(2)); err != nil {
				s.Close()
				t.Fatalf("delta=%d: leaked mem error poisoned TrySetGlobal: %v", delta, err)
			}
			s.Close()
		}
	})
}

// TestStaleAbortErrDoesNotPoisonHostWrites verifies Track 1 A1 P1:
// after an instruction-limit or context abort, host writes (Table.Set, TrySetGlobal,
// Preload, TryNewMetatable, NewUserdataWithMeta) must NOT spuriously report the stale abort error.
func TestStaleAbortErrDoesNotPoisonHostWrites(t *testing.T) {
	t.Run("after_instruction_limit", func(t *testing.T) {
		s := moelua.New(moelua.WithInstructionLimit(10000))
		defer s.Close()
		tbl := s.NewTable(4, 4)
		s.SetGlobal("tbl", tbl.Value())

		_, err := s.DoString(`while true do end`)
		if !errors.Is(err, moelua.ErrInstructionLimit) {
			t.Fatalf("expected ErrInstructionLimit, got %v", err)
		}

		if err := tbl.Set(moelua.Int(1), moelua.Int(42)); err != nil {
			t.Fatalf("Table.Set failed with stale abortErr: %v", err)
		}
		if got := tbl.GetInt(1).Int(); got != 42 {
			t.Fatalf("expected tbl[1]==42, got %d", got)
		}
		if err := tbl.SetString("k", moelua.Int(7)); err != nil {
			t.Fatalf("Table.SetString failed with stale abortErr: %v", err)
		}
		if err := s.TrySetGlobal("g", moelua.Int(99)); err != nil {
			t.Fatalf("TrySetGlobal failed with stale abortErr: %v", err)
		}
		if err := s.Preload("m", func(*moelua.State, []moelua.Value) ([]moelua.Value, error) { return nil, nil }); err != nil {
			t.Fatalf("Preload failed with stale abortErr: %v", err)
		}
		mt, created, err := s.TryNewMetatable("after_insn_mt")
		if err != nil || !created || mt == nil {
			t.Fatalf("TryNewMetatable failed with stale abortErr: mt=%v created=%v err=%v", mt, created, err)
		}
		if _, err := s.NewUserdataWithMeta("u", mt); err != nil {
			t.Fatalf("NewUserdataWithMeta failed with stale abortErr: %v", err)
		}
	})

	t.Run("after_context_cancel", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		s := moelua.New(moelua.WithContext(ctx))
		defer s.Close()
		tbl := s.NewTable(0, 4)
		s.SetGlobal("tbl", tbl.Value())

		_, err := s.DoString(`while true do end`)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded, got %v", err)
		}
		if err := tbl.SetString("after_cancel", moelua.Int(1)); err != nil {
			t.Fatalf("Table.SetString failed with stale ctx abortErr: %v", err)
		}
	})
}
