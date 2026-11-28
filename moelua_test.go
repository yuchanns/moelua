package moelua_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yuchanns/moelua"
)

func TestPublicAPIQuickstart(t *testing.T) {
	var out bytes.Buffer
	s := moelua.New(moelua.WithOutput(&out))
	defer s.Close()

	s.RegisterFunc("add", func(s *moelua.State, args []moelua.Value) ([]moelua.Value, error) {
		a, _ := args[0].ToInt()
		b, _ := args[1].ToInt()
		return []moelua.Value{moelua.Int(a + b)}, nil
	})
	s.RegisterFunc1("fast_mul", func(s *moelua.State, args []moelua.Value) (moelua.Value, error) {
		a, _ := args[0].ToInt()
		b, _ := args[1].ToInt()
		return moelua.Int(a * b), nil
	})

	tbl := s.NewTable(0, 2)
	if err := tbl.SetString("greeting", s.String("hello")); err != nil {
		t.Fatal(err)
	}
	if err := tbl.SetInt(1, moelua.Int(42)); err != nil {
		t.Fatal(err)
	}
	s.SetGlobal("cfg", tbl.Value())
	if _, err := s.Load(nil); err == nil {
		t.Fatalf("expected error on Load(nil)")
	}

	res, err := s.DoString(`
		print(cfg.greeting, add(cfg[1], 8))
		assert(fast_mul(6, 7) == 42)
		return add(100, 23), "done"
	`)
	if err != nil {
		t.Fatalf("DoString failed: %v", err)
	}
	if out.String() != "hello\t50\n" {
		t.Fatalf("unexpected output: %q", out.String())
	}
	if len(res) != 2 || res[0].Int() != 123 || s.FormatValue(res[1]) != "done" {
		t.Fatalf("unexpected results: %v", res)
	}
}

func TestPublicAPIPinHandleGCAnchoring(t *testing.T) {
	// Go-held Lua values anchored via State.Pin survive Tier 1 GC & weak-value tables,
	// and are collected promptly once Handle.Release is called.
	s := moelua.New()
	defer s.Close()

	res, err := s.DoString(`
		weak = setmetatable({}, {__mode = "v"})
		finalized = false
		local obj = setmetatable({ tag = "anchored" }, {
			__gc = function() finalized = true end,
		})
		weak[1] = obj
		return obj
	`)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	h := s.Pin(res[0])

	// Object is unreachable from Lua globals, ONLY anchored by Go Handle h!
	s.CollectGarbage()
	if _, err := s.DoString(`assert(weak[1] ~= nil and weak[1].tag == "anchored" and not finalized)`); err != nil {
		t.Fatalf("pinned value should survive GC: %v", err)
	}
	if s.FormatValue(s.TableOf(h.Value()).GetString("tag")) != "anchored" {
		t.Fatalf("handle value mismatch")
	}

	// Releasing handle allows GC to clear weak table and fire __gc finalizer!
	h.Release()
	h.Release() // idempotent
	if !h.Value().IsNil() {
		t.Fatalf("released handle must return nil")
	}
	s.CollectGarbage()
	if _, err := s.DoString(`assert(weak[1] == nil and finalized == true)`); err != nil {
		t.Fatalf("released value should be collected: %v", err)
	}
}

func TestPublicAPISharedChunkParallelStates(t *testing.T) {
	// Immutable *Chunk compiled once and executed concurrently across 8 independent States
	// on 8 goroutines (each State strictly single-goroutine).
	chunk, err := moelua.Compile("=shared", `
		local longkey = string.rep("k", 60)
		local t = { [longkey] = ... }
		local sum = 0
		for i = 1, 200 do
			sum = sum + i + t[longkey]
		end
		-- Mutate stdlib on this State to prove complete State isolation!
		math.sin = function() return -999 end
		math.custom_field = t[longkey]
		return sum, math.custom_field
	`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	dumped, err := chunk.Dump(false)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	reloaded, err := moelua.LoadChunk(dumped)
	if err != nil {
		t.Fatalf("LoadChunk: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for iter := range 20 {
				s := moelua.New()
				ch := chunk
				if iter%2 == 1 {
					ch = reloaded
				}
				fn, err := s.Load(ch)
				if err != nil {
					errs <- err
					s.Close()
					return
				}
				arg := int64(id*10 + iter)
				res, err := s.Call(fn, moelua.Int(arg))
				if err != nil {
					errs <- err
					s.Close()
					return
				}
				want := int64(200*201/2) + 200*arg
				if len(res) != 2 || res[0].Int() != want || res[1].Int() != arg {
					errs <- fmt.Errorf("goroutine %d iter %d: got %v want %d/%d", id, iter, res, want, arg)
					s.Close()
					return
				}
				s.Close()
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// Verify fresh State still has unmodified math.sin and no math.custom_field!
	s := moelua.New()
	defer s.Close()
	if _, err := s.DoString(`assert(math.sin(0) == 0.0 and math.custom_field == nil)`); err != nil {
		t.Fatalf("template isolation broken: %v", err)
	}
}

func TestPublicAPIMemoryLimit(t *testing.T) {
	s := moelua.New(moelua.WithMemoryLimit(512 * 1024))
	defer s.Close()

	res, err := s.DoString(`
		local ok1, err1 = pcall(function() return string.rep("x", 2 * 1024 * 1024) end)
		assert(not ok1 and tostring(err1):find("not enough memory"), tostring(err1))

		local ok2, err2 = pcall(function() return table.create(100000, 0) end)
		assert(not ok2 and tostring(err2):find("not enough memory"), tostring(err2))

		local ok3, err3 = pcall(function()
			local keep = {}
			for i = 1, 100000 do keep[i] = {i, i+1, i+2} end
		end)
		assert(not ok3 and tostring(err3):find("not enough memory"), tostring(err3))

		-- After failed allocations, garbage is reclaimable and State remains usable!
		collectgarbage()
		return "still-alive"
	`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) != 1 || s.FormatValue(res[0]) != "still-alive" {
		t.Fatalf("unexpected res: %v", res)
	}
}

func TestPublicAPIDoFileBOMAndShebang(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "script.lua")
	content := "\xEF\xBB\xBF#!/usr/bin/env lua\nreturn 7 * 6\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s := moelua.New()
	defer s.Close()
	res, err := s.DoFile(path)
	if err != nil {
		t.Fatalf("DoFile failed: %v", err)
	}
	if len(res) != 1 || res[0].Int() != 42 {
		t.Fatalf("got %v, want 42", res)
	}

	// Verify Close flushes open files.
	outFile := filepath.Join(tmp, "out.txt")
	s2 := moelua.New()
	_, err = s2.DoString(fmt.Sprintf(`F = assert(io.open(%q, "w")); F:setvbuf("full", 4096); F:write("public-api-close")`, outFile))
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(string(got), "public-api-close") {
		t.Fatalf("expected flushed file, got %q", string(got))
	}
}

func TestPublicAPIContextCancellationAndDeadline(t *testing.T) {
	// 1. Pre-canceled context aborts before executing any instruction.
	cctx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	s0 := moelua.New(moelua.WithContext(cctx))
	defer s0.Close()
	if _, err := s0.DoString(`return 1 + 1`); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// 2. Deadline aborts infinite loops and CANNOT be trapped by pcall, xpcall, or coroutine.resume.
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"plain_loop", `while true do end`},
		{"pcall_escape", `while true do pcall(function() while true do end end) end`},
		{"xpcall_escape", `while true do xpcall(function() while true do end end, function(e) return e end) end`},
		{"coro_resume_escape", `while true do local co = coroutine.create(function() while true do end end); coroutine.resume(co) end`},
		{"coro_wrap_escape", `while true do local f = coroutine.wrap(function() while true do end end); pcall(f) end`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			s := moelua.New(moelua.WithContext(ctx))
			defer s.Close()
			_, err := s.DoString(tc.src)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s: expected context.DeadlineExceeded, got %v", tc.name, err)
			}
			// Clearing context restores usability of the State.
			s.SetContext(nil)
			res, err := s.DoString(`return 21 * 2`)
			if err != nil || len(res) != 1 || res[0].Int() != 42 {
				t.Fatalf("%s: state unusable after SetContext(nil): res=%v err=%v", tc.name, res, err)
			}
		})
	}
}

func TestPublicAPIInstructionLimit(t *testing.T) {
	s := moelua.New(moelua.WithInstructionLimit(5000))
	defer s.Close()

	// Within budget succeeds and counts instructions.
	res, err := s.DoString(`local sum = 0; for i = 1, 100 do sum = sum + i end; return sum`)
	if err != nil {
		t.Fatalf("unexpected error within budget: %v", err)
	}
	if len(res) != 1 || res[0].Int() != 5050 {
		t.Fatalf("got %v want 5050", res)
	}
	if n := s.InstructionsExecuted(); n <= 0 || n > 5000 {
		t.Fatalf("unexpected InstructionsExecuted: %d", n)
	}

	// Exceeding budget aborts with ErrInstructionLimit and cannot be trapped by pcall or coroutine.resume.
	s.SetInstructionLimit(2000)
	_, err = s.DoString(`while true do pcall(function() while true do end end) end`)
	if !errors.Is(err, moelua.ErrInstructionLimit) {
		t.Fatalf("expected ErrInstructionLimit, got %v", err)
	}

	s.SetInstructionLimit(2000)
	_, err = s.DoString(`while true do local co = coroutine.create(function() while true do end end); coroutine.resume(co) end`)
	if !errors.Is(err, moelua.ErrInstructionLimit) {
		t.Fatalf("expected ErrInstructionLimit from coroutine, got %v", err)
	}

	// Resetting limit restores normal execution; setting limit from inside a host callback takes effect immediately.
	s.SetInstructionLimit(0)
	s.RegisterFunc("arm_limit", func(st *moelua.State, _ []moelua.Value) ([]moelua.Value, error) {
		st.SetInstructionLimit(1000)
		return nil, nil
	})
	_, err = s.DoString(`arm_limit(); while true do end`)
	if !errors.Is(err, moelua.ErrInstructionLimit) {
		t.Fatalf("expected ErrInstructionLimit after dynamic arm_limit, got %v", err)
	}

	// Regression: uncatchable instruction limit across all indirect native/metamethod/GC/TBC paths.
	uncatchableCases := []struct {
		name string
		src  string
	}{
		{"index_pcall", `local function spin() while true do end end; local t = setmetatable({}, {__index = function() return pcall(spin) end}); return t.x`},
		{"native_pcall_metatable", `local t = setmetatable({}, {__index = pcall}); local _ = t[1]; local a = {function() while true do end end, function() while true do end end}; table.sort(a, pcall)`},
		{"xpcall_msgh_spin", `xpcall(function() error("boom") end, function() while true do end end)`},
		{"load_reader_spin", `load(function() while true do end end)`},
		{"tbc_close_spin_in_pcall", `pcall(function() local _ <close> = setmetatable({}, {__close = function() while true do end end}); error("boom") end)`},
		{"gc_finalizer_spin", `do local _ = setmetatable({}, {__gc = function() while true do end end}) end; collectgarbage()`},
		{"pattern_backtracking", `string.find(string.rep("a", 30), "a*a*a*a*a*a*a*a*a*b")`},
	}
	for _, tc := range uncatchableCases {
		t.Run(tc.name, func(t *testing.T) {
			st := moelua.New(moelua.WithInstructionLimit(5000))
			defer st.Close()
			_, err := st.DoString(tc.src)
			if !errors.Is(err, moelua.ErrInstructionLimit) {
				t.Fatalf("%s: expected ErrInstructionLimit, got %v", tc.name, err)
			}
			// After hard abort (including TBC discard), clearing limit keeps State cleanly usable.
			st.SetInstructionLimit(0)
			out, err := st.DoString(`local _ <close> = setmetatable({}, {__close = function() end}); return 42`)
			if err != nil || len(out) != 1 || out[0].Int() != 42 {
				t.Fatalf("%s: state broken after abort: out=%v err=%v", tc.name, out, err)
			}
		})
	}

	// Regression: catastrophic string.find backtracking respects context deadline promptly.
	t.Run("pattern_ctx_deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		st := moelua.New(moelua.WithContext(ctx))
		defer st.Close()
		start := time.Now()
		_, err := st.DoString(`return string.find(string.rep("a", 30), "a*a*a*a*a*a*a*a*a*b")`)
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded, got %v", err)
		}
		if elapsed > time.Second {
			t.Fatalf("pattern deadline took too long: %v", elapsed)
		}
	})

	// Non-cancelable context.Background() must not error or leak abort state.
	t.Run("background_ctx_noop", func(t *testing.T) {
		st := moelua.New(moelua.WithContext(context.Background()))
		defer st.Close()
		out, err := st.DoString(`return 6 * 7`)
		if err != nil || len(out) != 1 || out[0].Int() != 42 {
			t.Fatalf("unexpected background ctx result: %v %v", out, err)
		}
	})
}

func TestPublicAPIFSModuleLoaderAndCustomSearcher(t *testing.T) {
	binChunk, err := moelua.Compile("=binmod", `return { kind = "bytecode", val = 99 }`)
	if err != nil {
		t.Fatal(err)
	}
	binBytes, err := binChunk.Dump(true)
	if err != nil {
		t.Fatal(err)
	}

	fs1 := fstest.MapFS{
		"utils/math.lua":  {Data: []byte("\xEF\xBB\xBF#!/usr/bin/env lua\nreturn { add = function(a, b) return a + b end, src = 'fs1' }\n")},
		"pkg/init.lua":    {Data: []byte(`return { name = "pkg-init" }`)},
		"binmod.lua":      {Data: binBytes},
		"scripts/app.lua": {Data: []byte(`return "from-custom-path"`)},
	}
	fs2 := fstest.MapFS{
		"utils/math.lua": {Data: []byte(`return { src = 'fs2' }`)},
		"only_fs2.lua":   {Data: []byte(`return "layer2"`)},
	}

	s := moelua.New(moelua.WithNoEnv(), moelua.WithFS(fs1), moelua.WithFS(fs2))
	defer s.Close()

	s.AddSearcher("virtual", func(st *moelua.State, args []moelua.Value) ([]moelua.Value, error) {
		name := st.FormatValue(args[0])
		if name == "virtual.cfg" {
			loader := st.NewFunc("virtual_loader", func(st *moelua.State, _ []moelua.Value) ([]moelua.Value, error) {
				tbl := st.NewTable(0, 1)
				_ = tbl.SetString("mode", st.String("embedded"))
				return []moelua.Value{tbl.Value()}, nil
			})
			return []moelua.Value{loader, st.String(":virtual:")}, nil
		}
		return []moelua.Value{st.String("no virtual module '" + name + "'")}, nil
	})

	res, err := s.DoString(`
		-- First WithFS layer wins over second layer; BOM + shebang supported.
		local m, p1 = require("utils.math")
		assert(m.add(20, 22) == 42 and m.src == "fs1" and p1 == "utils/math.lua")

		-- Package init.lua resolution.
		local p, p2 = require("pkg")
		assert(p.name == "pkg-init" and p2 == "pkg/init.lua")

		-- Second WithFS layer fallback.
		local l2 = require("only_fs2")
		assert(l2 == "layer2")

		-- Binary chunk (.luac) loaded from fs.FS.
		local bm = require("binmod")
		assert(bm.kind == "bytecode" and bm.val == 99)

		-- package.preload takes precedence over fs.FS.
		package.preload["only_fs2_pre"] = function() return "preloaded" end
		assert(require("only_fs2_pre") == "preloaded")

		-- Custom package.path prefix works with fs.FS.
		package.path = "scripts/?.lua;" .. package.path
		assert(require("app") == "from-custom-path")

		-- Path traversal rejected cleanly.
		local ok, err = pcall(require, "../escape")
		assert(not ok and tostring(err):find("not found"))

		-- Custom Go searcher via AddSearcher.
		local vc, extra = require("virtual.cfg")
		assert(vc.mode == "embedded" and extra == ":virtual:")
		return "ok"
	`)
	if err != nil {
		t.Fatalf("DoString failed: %v", err)
	}
	if len(res) != 1 || s.FormatValue(res[0]) != "ok" {
		t.Fatalf("unexpected res: %v", res)
	}
}

func TestPublicAPINilAndPostCloseSafety(t *testing.T) {
	// Nil receivers must never panic.
	var nilState *moelua.State
	nilState.Close()
	nilState.SetOutput(nil)
	nilState.SetErrorOutput(nil)
	nilState.SetMemoryLimit(100)
	nilState.SetContext(context.Background())
	nilState.SetInstructionLimit(100)
	nilState.ResetInstructionCount()
	nilState.CollectGarbage()
	nilState.AddFS(nil)
	nilState.AddSearcher("x", nil)
	nilState.RegisterFunc("x", nil)
	nilState.RegisterFunc1("x", nil)
	nilState.SetGlobal("x", moelua.Int(1))
	if nilState.Raw() != nil || nilState.MemoryUsage() != 0 || nilState.InstructionsExecuted() != 0 {
		t.Fatalf("unexpected non-zero nil State getters")
	}
	if !nilState.GetGlobal("x").IsNil() || !nilState.String("x").IsNil() || !nilState.NewUserdata(1).IsNil() {
		t.Fatalf("expected Nil from nil State value constructors")
	}
	if nilState.NewTable(0, 0) != nil || nilState.TableOf(moelua.Nil()) != nil || nilState.Pin(moelua.Nil()) != nil {
		t.Fatalf("expected nil handle/table from nil State")
	}
	if _, err := nilState.DoString("return 1"); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on nil State.DoString, got %v", err)
	}
	if _, err := nilState.Call(moelua.Nil()); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on nil State.Call, got %v", err)
	}

	var nilTbl *moelua.Table
	if !nilTbl.Value().IsNil() || nilTbl.Raw() != nil || nilTbl.Len() != 0 || !nilTbl.GetInt(1).IsNil() || !nilTbl.GetString("k").IsNil() {
		t.Fatalf("unexpected non-zero nil Table getters")
	}
	if err := nilTbl.SetInt(1, moelua.Int(1)); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on nil Table.SetInt, got %v", err)
	}

	var nilChunk *moelua.Chunk
	if nilChunk.Proto() != nil {
		t.Fatalf("expected nil Proto")
	}
	if _, err := nilChunk.Dump(false); err == nil {
		t.Fatalf("expected error on nil Chunk.Dump")
	}

	var nilHandle *moelua.Handle
	nilHandle.Release()
	if !nilHandle.Value().IsNil() {
		t.Fatalf("expected Nil on nil Handle")
	}

	// Post-Close guards on real State, Table, and Handle.
	s := moelua.New()
	tbl := s.NewTable(0, 1)
	_ = tbl.SetString("k", moelua.Int(7))
	h := s.Pin(tbl.Value())
	s.Close()
	s.Close() // idempotent

	if s.Raw() != nil {
		t.Fatalf("expected nil Raw() after Close")
	}
	if _, err := s.DoString("return 1"); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed after Close, got %v", err)
	}
	if _, err := s.Load(nil); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on Load after Close, got %v", err)
	}
	if err := tbl.SetString("k", moelua.Int(8)); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on Table.SetString after Close, got %v", err)
	}
	if !tbl.GetString("k").IsNil() || !h.Value().IsNil() {
		t.Fatalf("expected Nil from Table/Handle after State.Close")
	}
	h.Release() // safe after Close

	// Nil/closed guards on Thread and new metatable/preload/iteration methods.
	var nilTh *moelua.Thread
	if !nilTh.Value().IsNil() || nilTh.Raw() != nil || nilTh.Status() != moelua.ThreadDead || nilTh.StatusString() != "dead" {
		t.Fatalf("unexpected nil Thread getters")
	}
	if _, _, err := nilTh.Resume(); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on nil Thread.Resume, got %v", err)
	}
	if err := nilTh.Close(); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on nil Thread.Close, got %v", err)
	}
	if err := s.Preload("m", nil); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on closed State.Preload, got %v", err)
	}
	if _, err := s.NewThread(moelua.Nil()); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on closed State.NewThread, got %v", err)
	}
	if err := tbl.ForEach(func(_, _ moelua.Value) error { return nil }); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on closed Table.ForEach, got %v", err)
	}
}

func TestMetatablesAndTypedUserdata(t *testing.T) {
	type counter struct{ n int64 }

	s := moelua.New()
	defer s.Close()

	mt, created := s.NewMetatable("moelua.Counter")
	if !created || mt == nil {
		t.Fatalf("expected new metatable created=true")
	}
	mt2, created2 := s.NewMetatable("moelua.Counter")
	if created2 || mt2.Raw() != mt.Raw() {
		t.Fatalf("expected second NewMetatable to return existing table")
	}
	if got := s.GetMetatableByName("moelua.Counter"); got == nil || got.Raw() != mt.Raw() {
		t.Fatalf("GetMetatableByName mismatch")
	}
	if name := mt.GetString("__name"); !name.IsString() || s.FormatValue(name) != "moelua.Counter" {
		t.Fatalf("expected __name='moelua.Counter', got %v", name)
	}

	methods := s.NewTable(0, 2)
	_ = methods.SetString("inc", s.NewFunc1("inc", func(st *moelua.State, args []moelua.Value) (moelua.Value, error) {
		raw, ok := st.CheckUserdata(args[0], "moelua.Counter")
		if !ok {
			return moelua.Nil(), errors.New("expected Counter")
		}
		c := raw.(*counter)
		delta := int64(1)
		if len(args) >= 2 {
			if d, ok := args[1].ToInt(); ok {
				delta = d
			}
		}
		c.n += delta
		return moelua.Int(c.n), nil
	}))
	_ = mt.SetString("__index", methods.Value())

	finalized := int64(0)
	_ = mt.SetString("__gc", s.NewFunc1("gc", func(st *moelua.State, args []moelua.Value) (moelua.Value, error) {
		if raw, ok := st.CheckUserdata(args[0], "moelua.Counter"); ok {
			finalized = raw.(*counter).n
		}
		return moelua.Nil(), nil
	}))

	ud, err := s.NewUserdataWithMeta(&counter{n: 10}, mt)
	if err != nil {
		t.Fatalf("NewUserdataWithMeta: %v", err)
	}
	if gotMT := s.GetMetatable(ud); gotMT == nil || gotMT.Raw() != mt.Raw() {
		t.Fatalf("GetMetatable(ud) mismatch")
	}
	if _, ok := s.CheckUserdata(ud, "other.Type"); ok {
		t.Fatalf("CheckUserdata should reject mismatched metatable name")
	}
	s.SetGlobal("c", ud)

	res, err := s.DoString(`return c:inc(), c:inc(5), tostring(c):find("moelua.Counter", 1, true) ~= nil`)
	if err != nil {
		t.Fatalf("DoString: %v", err)
	}
	if res[0].Int() != 11 || res[1].Int() != 16 || !res[2].IsTrue() {
		t.Fatalf("unexpected userdata method results: %v", res)
	}

	// Verify __gc triggers on CollectGarbage once unreferenced.
	s.SetGlobal("c", moelua.Nil())
	s.CollectGarbage()
	if finalized != 16 {
		t.Fatalf("expected __gc finalizer to observe counter=16, got %d", finalized)
	}

	// Verify Table.SetMetatable / Table.Metatable and cross-State rejection.
	tbl := s.NewTable(0, 1)
	if err := tbl.SetMetatable(mt); err != nil {
		t.Fatalf("Table.SetMetatable: %v", err)
	}
	if tbl.Metatable() == nil || tbl.Metatable().Raw() != mt.Raw() {
		t.Fatalf("Table.Metatable mismatch")
	}
	if err := tbl.SetMetatable(nil); err != nil || tbl.Metatable() != nil {
		t.Fatalf("clearing Table metatable failed: err=%v mt=%v", err, tbl.Metatable())
	}

	other := moelua.New()
	defer other.Close()
	otherMT := other.NewTable(0, 1)
	if err := s.SetMetatable(tbl.Value(), otherMT); err == nil {
		t.Fatalf("expected cross-State SetMetatable error")
	}
}

func TestTableNextAndForEach(t *testing.T) {
	s := moelua.New()
	defer s.Close()

	res, err := s.DoString(`return { 10, 20, 30, a = "alpha", b = "beta" }`)
	if err != nil {
		t.Fatalf("DoString: %v", err)
	}
	tbl := s.TableOf(res[0])
	if tbl == nil {
		t.Fatalf("expected table")
	}

	seen := map[string]string{}
	if err := tbl.ForEach(func(k, v moelua.Value) error {
		seen[s.FormatValue(k)] = s.FormatValue(v)
		return nil
	}); err != nil {
		t.Fatalf("ForEach: %v", err)
	}
	want := map[string]string{"1": "10", "2": "20", "3": "30", "a": "alpha", "b": "beta"}
	if len(seen) != len(want) {
		t.Fatalf("ForEach count=%d want=%d: %v", len(seen), len(want), seen)
	}
	for k, w := range want {
		if seen[k] != w {
			t.Fatalf("ForEach[%q]=%q want %q", k, seen[k], w)
		}
	}

	// Early stop propagates caller error.
	sentinel := errors.New("stop-early")
	visits := 0
	err = tbl.ForEach(func(_, _ moelua.Value) error {
		visits++
		if visits == 2 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) || visits != 2 {
		t.Fatalf("expected early stop sentinel at 2 visits, got err=%v visits=%d", err, visits)
	}

	// Invalid key to Next returns an error.
	if _, _, _, err := tbl.Next(s.String("nonexistent-key")); err == nil {
		t.Fatalf("expected invalid key error from Next")
	}
}

func TestPreloadModule(t *testing.T) {
	s := moelua.New()
	defer s.Close()

	loads := 0
	if err := s.Preload("native.mathx", func(st *moelua.State, args []moelua.Value) ([]moelua.Value, error) {
		loads++
		mod := st.NewTable(0, 1)
		_ = mod.SetString("triple", st.NewFunc1("triple", func(_ *moelua.State, a []moelua.Value) (moelua.Value, error) {
			return moelua.Int(a[0].Int() * 3), nil
		}))
		return []moelua.Value{mod.Value()}, nil
	}); err != nil {
		t.Fatalf("Preload: %v", err)
	}

	res, err := s.DoString(`
		local m1 = require("native.mathx")
		local m2 = require("native.mathx")
		return m1 == m2, m1.triple(14)
	`)
	if err != nil {
		t.Fatalf("DoString: %v", err)
	}
	if !res[0].IsTrue() || res[1].Int() != 42 || loads != 1 {
		t.Fatalf("unexpected preload behavior: res=%v loads=%d", res, loads)
	}
}

func TestGoDrivenCoroutines(t *testing.T) {
	s := moelua.New()
	defer s.Close()

	res, err := s.DoString(`
		closed = false
		return function(init)
			local guard <close> = setmetatable({}, {
				__close = function() closed = true end
			})
			local a = coroutine.yield("step1", init + 1)
			local b = coroutine.yield("step2", a * 10)
			return "done", b + 5
		end
	`)
	if err != nil {
		t.Fatalf("DoString: %v", err)
	}
	th, err := s.NewThread(res[0])
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	if th.StatusString() != "suspended" || th.Status() != moelua.ThreadOK {
		t.Fatalf("fresh thread status=%v (%q)", th.Status(), th.StatusString())
	}
	if wrapped := s.ThreadOf(th.Value()); wrapped == nil || wrapped.Raw() != th.Raw() {
		t.Fatalf("ThreadOf roundtrip failed")
	}

	r1, yielded, err := th.Resume(moelua.Int(4))
	if err != nil || !yielded || len(r1) != 2 || s.FormatValue(r1[0]) != "step1" || r1[1].Int() != 5 {
		t.Fatalf("Resume 1: res=%v yielded=%v err=%v", r1, yielded, err)
	}
	if th.Status() != moelua.ThreadSuspended || th.StatusString() != "suspended" {
		t.Fatalf("after yield status=%v (%q)", th.Status(), th.StatusString())
	}

	r2, yielded, err := th.Resume(moelua.Int(7))
	if err != nil || !yielded || len(r2) != 2 || s.FormatValue(r2[0]) != "step2" || r2[1].Int() != 70 {
		t.Fatalf("Resume 2: res=%v yielded=%v err=%v", r2, yielded, err)
	}

	r3, yielded, err := th.Resume(moelua.Int(100))
	if err != nil || yielded || len(r3) != 2 || s.FormatValue(r3[0]) != "done" || r3[1].Int() != 105 {
		t.Fatalf("Resume 3: res=%v yielded=%v err=%v", r3, yielded, err)
	}
	if th.Status() != moelua.ThreadDead || th.StatusString() != "dead" {
		t.Fatalf("after return status=%v (%q)", th.Status(), th.StatusString())
	}
	if !s.GetGlobal("closed").IsTrue() {
		t.Fatalf("expected <close> variable to run on coroutine completion")
	}

	// Test Close() running <close> metamethod on a suspended coroutine.
	res2, err := s.DoString(`
		aborted_closed = false
		return function()
			local guard <close> = setmetatable({}, {
				__close = function() aborted_closed = true end
			})
			coroutine.yield("waiting")
		end
	`)
	if err != nil {
		t.Fatalf("DoString 2: %v", err)
	}
	th2, err := s.NewThread(res2[0])
	if err != nil {
		t.Fatalf("NewThread 2: %v", err)
	}
	if _, y, err := th2.Resume(); err != nil || !y {
		t.Fatalf("Resume th2: yielded=%v err=%v", y, err)
	}
	if err := th2.Close(); err != nil {
		t.Fatalf("th2.Close: %v", err)
	}
	if th2.Status() != moelua.ThreadDead || !s.GetGlobal("aborted_closed").IsTrue() {
		t.Fatalf("expected th2 dead and <close> executed; status=%v closed=%v", th2.Status(), s.GetGlobal("aborted_closed"))
	}

	// Test instruction limit propagation out of Thread.Resume.
	limState := moelua.New(moelua.WithInstructionLimit(5000))
	defer limState.Close()
	fnRes, err := limState.DoString(`return function() while true do end end`)
	if err != nil {
		t.Fatalf("DoString lim: %v", err)
	}
	limTh, err := limState.NewThread(fnRes[0])
	if err != nil {
		t.Fatalf("NewThread lim: %v", err)
	}
	_, yielded, err = limTh.Resume()
	if yielded || !errors.Is(err, moelua.ErrInstructionLimit) {
		t.Fatalf("expected ErrInstructionLimit from Thread.Resume, got yielded=%v err=%v", yielded, err)
	}
}

func TestDynamicGoFuncsCollectableInWeakTables(t *testing.T) {
	s := moelua.New()
	defer s.Close()

	s.RegisterFunc("make_fn", func(s *moelua.State, _ []moelua.Value) ([]moelua.Value, error) {
		return []moelua.Value{
			s.NewFunc("dyn", func(_ *moelua.State, _ []moelua.Value) ([]moelua.Value, error) {
				return []moelua.Value{moelua.Int(1)}, nil
			}),
		}, nil
	})
	s.RegisterFunc("make_fn1", func(s *moelua.State, _ []moelua.Value) ([]moelua.Value, error) {
		return []moelua.Value{
			s.NewFunc1("dyn1", func(_ *moelua.State, _ []moelua.Value) (moelua.Value, error) {
				return moelua.Int(2), nil
			}),
		}, nil
	})

	if _, err := s.DoString(`
		local w = setmetatable({}, {__mode = "v"})
		w.f = make_fn()
		w.f1 = make_fn1()
		assert(w.f() == 1 and w.f1() == 2)
		collectgarbage("collect")
		assert(w.f == nil, "NewFunc callback should be collected from weak-value table")
		assert(w.f1 == nil, "NewFunc1 callback should be collected from weak-value table")
	`); err != nil {
		t.Fatalf("weak table collection test failed: %v", err)
	}
}

func TestNewTableMemoryLimitEnforcement(t *testing.T) {
	s := moelua.New(moelua.WithMemoryLimit(131072))
	defer s.Close()

	if tbl := s.NewTable(0, 100000); tbl != nil {
		t.Fatalf("expected nil from NewTable(0, 100000) under 128KiB memory limit")
	}
	if tbl, err := s.TryNewTable(0, 100000); tbl != nil || err == nil || !strings.Contains(err.Error(), "not enough memory") {
		t.Fatalf("expected memory error from TryNewTable(0, 100000), got tbl=%v err=%v", tbl, err)
	}
	before := s.MemoryUsage()
	tbl, err := s.TryNewTable(0, 200)
	if err != nil || tbl == nil {
		t.Fatalf("expected TryNewTable(0, 200) to succeed, got tbl=%v err=%v", tbl, err)
	}
	if after := s.MemoryUsage(); after <= before {
		t.Fatalf("expected MemoryUsage to increase after TryNewTable(0, 200): before=%d after=%d", before, after)
	}
}

func TestStringMemoryLimitEnforcement(t *testing.T) {
	s := moelua.New(moelua.WithMemoryLimit(131072))
	defer s.Close()

	huge := strings.Repeat("x", 1_000_000)
	if v := s.String(huge); !v.IsNil() {
		t.Fatalf("expected Nil from String(1MB) under 128KiB memory limit, got %v", v)
	}
	if v, err := s.TryString(huge); !v.IsNil() || err == nil || !strings.Contains(err.Error(), "not enough memory") {
		t.Fatalf("expected memory error from TryString(1MB), got v=%v err=%v", v, err)
	}
	before := s.MemoryUsage()
	med, err := s.TryString(strings.Repeat("a", 8192))
	if err != nil || !med.IsString() {
		t.Fatalf("expected TryString(8KiB) to succeed, got v=%v err=%v", med, err)
	}
	if after := s.MemoryUsage(); after < before+8192 {
		t.Fatalf("expected MemoryUsage to include 8KiB string: before=%d after=%d", before, after)
	}
}

func TestNewMetatableMemoryLimitEnforcement(t *testing.T) {
	s := moelua.New(moelua.WithMemoryLimit(65536))
	defer s.Close()

	// Repeatedly register distinct metatable names until the 64 KiB limit is hit.
	failedNew := false
	for i := range 500 {
		name := fmt.Sprintf("type_mt_%d", i)
		mt, created := s.NewMetatable(name)
		if mt == nil {
			if created {
				t.Fatalf("NewMetatable(%q) returned nil with created=true", name)
			}
			if got := s.GetMetatableByName(name); got != nil {
				t.Fatalf("failed NewMetatable(%q) unexpectedly registered in registry", name)
			}
			failedNew = true
			break
		}
		if !created {
			t.Fatalf("expected first NewMetatable(%q) to report created=true", name)
		}
		if got := s.GetMetatableByName(name); got == nil || got.Raw() != mt.Raw() {
			t.Fatalf("NewMetatable(%q) reported success but GetMetatableByName returned %v", name, got)
		}
	}
	if !failedNew {
		t.Fatalf("expected NewMetatable loop to eventually hit 64KiB memory limit")
	}

	// TryNewMetatable must expose the memory-limit error explicitly.
	if mt, created, err := s.TryNewMetatable("type_mt_overflow"); mt != nil || created || err == nil || !strings.Contains(err.Error(), "not enough memory") {
		t.Fatalf("expected memory error from TryNewMetatable, got mt=%v created=%v err=%v", mt, created, err)
	}

	// No unconsumed memory error should poison subsequent Lua execution that fits in budget.
	s.SetMemoryLimit(0)
	res, err := s.DoString(`return 1 + 2`)
	if err != nil || len(res) != 1 || res[0].Int() != 3 {
		t.Fatalf("subsequent Lua execution failed after NewMetatable OOM: res=%v err=%v", res, err)
	}

	// Regression: distinct <=40-byte metatable names must be accounted BEFORE interning so
	// recounted live storage never silently exceeds WithMemoryLimit.
	s2 := moelua.New(moelua.WithMemoryLimit(65536))
	defer s2.Close()
	s2.CollectGarbage()
	hitLimit := false
	for i := range 200 {
		name := fmt.Sprintf("%040d", i) // exact 40-byte short string
		mt, created, err := s2.TryNewMetatable(name)
		if err != nil {
			if mt != nil || created || !strings.Contains(err.Error(), "not enough memory") {
				t.Fatalf("unexpected TryNewMetatable error at %d: mt=%v created=%v err=%v", i, mt, created, err)
			}
			hitLimit = true
			break
		}
		if !created || mt == nil {
			t.Fatalf("expected created=true for %q", name)
		}
	}
	if !hitLimit {
		t.Fatalf("expected 40-byte TryNewMetatable registrations to hit 64KiB memory limit")
	}
	s2.CollectGarbage()
	if usage := s2.MemoryUsage(); usage > 65536 {
		t.Fatalf("recounted MemoryUsage()=%d exceeded 65536 limit after short-name TryNewMetatable registrations", usage)
	}
}

func TestLookupBeforeInsertPreservesMemoryLimit(t *testing.T) {
	const limit = int64(65536)

	t.Run("GetGlobal_before_TrySetGlobal", func(t *testing.T) {
		s := moelua.New(moelua.WithMemoryLimit(limit))
		defer s.Close()
		s.CollectGarbage()
		hit := false
		for i := range 500 {
			name := fmt.Sprintf("g%039d", i)
			if !s.GetGlobal(name).IsNil() {
				t.Fatalf("unexpected existing global %q", name)
			}
			// Deleting unseen key must also avoid interning it.
			if err := s.TrySetGlobal(name, moelua.Nil()); err != nil {
				t.Fatalf("unexpected error deleting unseen global: %v", err)
			}
			if err := s.TrySetGlobal(name, moelua.Int(int64(i))); err != nil {
				if !strings.Contains(err.Error(), "not enough memory") {
					t.Fatalf("unexpected TrySetGlobal error: %v", err)
				}
				hit = true
				break
			}
		}
		if !hit {
			t.Fatalf("expected GetGlobal+TrySetGlobal loop to hit 64KiB memory limit")
		}
		s.CollectGarbage()
		if usage := s.MemoryUsage(); usage > limit {
			t.Fatalf("recounted MemoryUsage()=%d exceeded %d after GetGlobal+TrySetGlobal", usage, limit)
		}
	})

	t.Run("TableGetString_before_SetString", func(t *testing.T) {
		s := moelua.New(moelua.WithMemoryLimit(limit))
		defer s.Close()
		tbl, err := s.TryNewTable(0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.TrySetGlobal("t", tbl.Value()); err != nil {
			t.Fatal(err)
		}
		s.CollectGarbage()
		hit := false
		for i := range 500 {
			key := fmt.Sprintf("k%039d", i)
			if !tbl.GetString(key).IsNil() {
				t.Fatalf("unexpected existing key %q", key)
			}
			if err := tbl.SetString(key, moelua.Nil()); err != nil {
				t.Fatalf("unexpected error deleting unseen key: %v", err)
			}
			if err := tbl.SetString(key, moelua.Int(int64(i))); err != nil {
				if !strings.Contains(err.Error(), "not enough memory") {
					t.Fatalf("unexpected Table.SetString error: %v", err)
				}
				hit = true
				break
			}
		}
		if !hit {
			t.Fatalf("expected Table.GetString+SetString loop to hit 64KiB memory limit")
		}
		s.CollectGarbage()
		if usage := s.MemoryUsage(); usage > limit {
			t.Fatalf("recounted MemoryUsage()=%d exceeded %d after Table.GetString+SetString", usage, limit)
		}
	})

	t.Run("GetMetatableByName_before_TryNewMetatable", func(t *testing.T) {
		s := moelua.New(moelua.WithMemoryLimit(limit))
		defer s.Close()
		s.CollectGarbage()
		hit := false
		for i := range 300 {
			name := fmt.Sprintf("m%039d", i)
			if s.GetMetatableByName(name) != nil {
				t.Fatalf("unexpected existing metatable %q", name)
			}
			if _, _, err := s.TryNewMetatable(name); err != nil {
				if !strings.Contains(err.Error(), "not enough memory") {
					t.Fatalf("unexpected TryNewMetatable error: %v", err)
				}
				hit = true
				break
			}
		}
		if !hit {
			t.Fatalf("expected GetMetatableByName+TryNewMetatable loop to hit 64KiB memory limit")
		}
		s.CollectGarbage()
		if usage := s.MemoryUsage(); usage > limit {
			t.Fatalf("recounted MemoryUsage()=%d exceeded %d after GetMetatableByName+TryNewMetatable", usage, limit)
		}
	})
}

func TestTableSetStringMemoryLimitEnforcement(t *testing.T) {
	s := moelua.New(moelua.WithMemoryLimit(131072))
	defer s.Close()

	tbl, err := s.TryNewTable(0, 0)
	if err != nil {
		t.Fatalf("TryNewTable failed: %v", err)
	}
	if err := s.TrySetGlobal("host_tbl", tbl.Value()); err != nil {
		t.Fatalf("TrySetGlobal failed: %v", err)
	}

	hugeKey := strings.Repeat("k", 1_000_000)

	// Table.SetString must reject oversized string keys before mutating the table.
	if err := tbl.SetString(hugeKey, moelua.Int(1)); err == nil || !strings.Contains(err.Error(), "not enough memory") {
		t.Fatalf("expected memory error from Table.SetString(1MB), got %v", err)
	}
	if !tbl.GetString(hugeKey).IsNil() {
		t.Fatalf("Table.SetString mutated table despite OOM")
	}

	// State.TrySetGlobal / Preload / TryNewMetatable must also enforce key string budgets.
	if err := s.TrySetGlobal(hugeKey, moelua.Int(1)); err == nil || !strings.Contains(err.Error(), "not enough memory") {
		t.Fatalf("expected memory error from TrySetGlobal(1MB), got %v", err)
	}
	if err := s.Preload(hugeKey, func(*moelua.State, []moelua.Value) ([]moelua.Value, error) { return nil, nil }); err == nil || !strings.Contains(err.Error(), "not enough memory") {
		t.Fatalf("expected memory error from Preload(1MB), got %v", err)
	}
	if mt, created, err := s.TryNewMetatable(hugeKey); mt != nil || created || err == nil || !strings.Contains(err.Error(), "not enough memory") {
		t.Fatalf("expected memory error from TryNewMetatable(1MB), got mt=%v created=%v err=%v", mt, created, err)
	}

	// Keys within budget must be accounted in MemoryUsage() and remain readable from Lua.
	before := s.MemoryUsage()
	mediumKey := strings.Repeat("m", 8192)
	if err := tbl.SetString(mediumKey, moelua.Int(42)); err != nil {
		t.Fatalf("expected 8KiB key within budget to succeed, got %v", err)
	}
	if after := s.MemoryUsage(); after < before+8192 {
		t.Fatalf("expected MemoryUsage to account for 8KiB key: before=%d after=%d", before, after)
	}
	res, err := s.DoString(`local n = 0; for k, v in pairs(host_tbl) do n = n + #k + v end; return n`)
	if err != nil || len(res) != 1 || res[0].Int() != 8192+42 {
		t.Fatalf("unexpected Lua readback: res=%v err=%v", res, err)
	}
}

func TestStreamingPublicAPI(t *testing.T) {
	// 1. CompileReader + Chunk.DumpTo + LoadChunkReader round-trip.
	src := "\xef\xbb\xbf#!/usr/bin/env lua\nreturn 6 * 7\n"
	ch, err := moelua.CompileReader("@stream_api.lua", strings.NewReader(src))
	if err != nil {
		t.Fatalf("CompileReader failed: %v", err)
	}
	var binBuf bytes.Buffer
	if err := ch.DumpTo(&binBuf, false); err != nil {
		t.Fatalf("Chunk.DumpTo failed: %v", err)
	}
	ch2, err := moelua.LoadChunkReader(bytes.NewReader(binBuf.Bytes()))
	if err != nil {
		t.Fatalf("LoadChunkReader failed: %v", err)
	}

	s := moelua.New()
	defer s.Close()
	fn, err := s.Load(ch2)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	res, err := s.Call(fn)
	if err != nil || len(res) != 1 || res[0].Int() != 42 {
		t.Fatalf("unexpected Call result: res=%v err=%v", res, err)
	}

	// 2. State.DoReader streams both text and binary chunks (including large comments under a tight memLimit).
	sMem := moelua.New(moelua.WithMemoryLimit(131072))
	defer sMem.Close()
	bigSrc := strings.Repeat("-- "+strings.Repeat("x", 1020)+"\n", 512) + "return 9876\n"
	resMem, err := sMem.DoReader("=big_stream_api", strings.NewReader(bigSrc))
	if err != nil || len(resMem) != 1 || resMem[0].Int() != 9876 {
		t.Fatalf("DoReader large source under 128KiB memLimit failed: res=%v err=%v", resMem, err)
	}

	// 3. State.DoFile streams text (with BOM/shebang) and binary files.
	dir := t.TempDir()
	textPath := filepath.Join(dir, "script.lua")
	if err := os.WriteFile(textPath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	resText, err := s.DoFile(textPath)
	if err != nil || len(resText) != 1 || resText[0].Int() != 42 {
		t.Fatalf("DoFile(text) failed: res=%v err=%v", resText, err)
	}
	binPath := filepath.Join(dir, "script.luac")
	if err := os.WriteFile(binPath, binBuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	resBin, err := s.DoFile(binPath)
	if err != nil || len(resBin) != 1 || resBin[0].Int() != 42 {
		t.Fatalf("DoFile(binary) failed: res=%v err=%v", resBin, err)
	}
}

func TestCallbackFactoryMemoryLimitAccounting(t *testing.T) {
	s := moelua.New(moelua.WithMemoryLimit(65536))
	defer s.Close()

	var pinned []*moelua.Handle
	defer func() {
		for _, h := range pinned {
			h.Release()
		}
	}()

	var hitErr error
	for i := range 3000 {
		var (
			fn  moelua.Value
			err error
		)
		if i%2 == 0 {
			fn, err = s.TryNewFunc("cb", func(_ *moelua.State, args []moelua.Value) ([]moelua.Value, error) {
				return args, nil
			})
		} else {
			fn, err = s.TryNewFunc1("cb1", func(_ *moelua.State, _ []moelua.Value) (moelua.Value, error) {
				return moelua.Int(1), nil
			})
		}
		if err != nil {
			hitErr = err
			break
		}
		pinned = append(pinned, s.Pin(fn))
	}
	if hitErr == nil || !strings.Contains(hitErr.Error(), "not enough memory") {
		t.Fatalf("expected TryNewFunc/TryNewFunc1 to enforce 64KiB memory limit, pinned=%d err=%v", len(pinned), hitErr)
	}
	if usage := s.MemoryUsage(); usage < int64(len(pinned))*48 {
		t.Fatalf("expected MemoryUsage (%d) to account for %d pinned callbacks (>= %d)", usage, len(pinned), int64(len(pinned))*48)
	}
	// Non-erroring NewFunc / NewFunc1 must return Nil rather than bypassing the limit.
	if v := s.NewFunc("over", func(_ *moelua.State, _ []moelua.Value) ([]moelua.Value, error) { return nil, nil }); !v.IsNil() {
		t.Fatalf("expected NewFunc to return Nil when over memory limit, got %v", v)
	}
	if v := s.NewFunc1("over1", func(_ *moelua.State, _ []moelua.Value) (moelua.Value, error) { return moelua.Nil(), nil }); !v.IsNil() {
		t.Fatalf("expected NewFunc1 to return Nil when over memory limit, got %v", v)
	}
}

func TestUserdataMemoryLimitAccounting(t *testing.T) {
	s := moelua.New(moelua.WithMemoryLimit(65536))
	defer s.Close()

	var pinned []*moelua.Handle
	defer func() {
		for _, h := range pinned {
			h.Release()
		}
	}()

	var hitErr error
	for i := range 3000 {
		u, err := s.TryNewUserdata(i)
		if err != nil {
			hitErr = err
			break
		}
		pinned = append(pinned, s.Pin(u))
	}
	if hitErr == nil || !strings.Contains(hitErr.Error(), "not enough memory") {
		t.Fatalf("expected TryNewUserdata to enforce 64KiB memory limit, pinned=%d err=%v", len(pinned), hitErr)
	}
	if usage := s.MemoryUsage(); usage < int64(len(pinned))*64 {
		t.Fatalf("expected MemoryUsage (%d) to account for %d pinned userdata (>= %d)", usage, len(pinned), int64(len(pinned))*64)
	}
	if u := s.NewUserdata("over"); !u.IsNil() {
		t.Fatalf("expected NewUserdata to return Nil when over memory limit, got %v", u)
	}
	if u, err := s.NewUserdataWithMeta("over", nil); !u.IsNil() || err == nil || !strings.Contains(err.Error(), "not enough memory") {
		t.Fatalf("expected NewUserdataWithMeta to fail with memory error, got u=%v err=%v", u, err)
	}
}

func TestStateCompileEnforcesSandboxControls(t *testing.T) {
	// 1. Closed / nil State returns ErrStateClosed from Compile and CompileReader.
	var nilState *moelua.State
	if _, err := nilState.Compile("=nil", "return 1"); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on nil State.Compile, got %v", err)
	}
	if _, err := nilState.CompileReader("=nil", strings.NewReader("return 1")); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on nil State.CompileReader, got %v", err)
	}
	closed := moelua.New()
	closed.Close()
	if _, err := closed.Compile("=closed", "return 1"); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on closed State.Compile, got %v", err)
	}
	if _, err := closed.CompileReader("=closed", strings.NewReader("return 1")); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("expected ErrStateClosed on closed State.CompileReader, got %v", err)
	}

	// 2. State.Compile, State.CompileReader, and State.DoString enforce WithMemoryLimit.
	hugeLit := `return "` + strings.Repeat("a", 512*1024) + `"`
	sMem := moelua.New(moelua.WithMemoryLimit(128 * 1024))
	defer sMem.Close()
	if _, err := sMem.Compile("=big", hugeLit); err == nil || !strings.Contains(err.Error(), "not enough memory") {
		t.Fatalf("expected memory error from State.Compile, got %v", err)
	}
	if _, err := sMem.CompileReader("=big", strings.NewReader(hugeLit)); err == nil || !strings.Contains(err.Error(), "not enough memory") {
		t.Fatalf("expected memory error from State.CompileReader, got %v", err)
	}
	if _, err := sMem.DoString(hugeLit); err == nil || !strings.Contains(err.Error(), "not enough memory") {
		t.Fatalf("expected memory error from State.DoString, got %v", err)
	}

	// 3. State.Compile and State.CompileReader honor pre-canceled context.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	sCtx := moelua.New(moelua.WithContext(cctx))
	defer sCtx.Close()
	if _, err := sCtx.Compile("=cancel", "return 1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from State.Compile, got %v", err)
	}
	if _, err := sCtx.CompileReader("=cancel", strings.NewReader("return 1")); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from State.CompileReader, got %v", err)
	}

	// 4. State.Compile, State.CompileReader, and State.DoString honor WithInstructionLimit on huge source.
	hugeComments := strings.Repeat("-- "+strings.Repeat("x", 1020)+"\n", 64) + "return 1\n"
	sInsn := moelua.New(moelua.WithInstructionLimit(500))
	defer sInsn.Close()
	if _, err := sInsn.Compile("=huge", hugeComments); !errors.Is(err, moelua.ErrInstructionLimit) {
		t.Fatalf("expected ErrInstructionLimit from State.Compile, got %v", err)
	}
	sInsn.SetInstructionLimit(500)
	if _, err := sInsn.CompileReader("=huge", strings.NewReader(hugeComments)); !errors.Is(err, moelua.ErrInstructionLimit) {
		t.Fatalf("expected ErrInstructionLimit from State.CompileReader, got %v", err)
	}
	sInsn.SetInstructionLimit(500)
	if _, err := sInsn.DoString(hugeComments); !errors.Is(err, moelua.ErrInstructionLimit) {
		t.Fatalf("expected ErrInstructionLimit from State.DoString, got %v", err)
	}
	sInsn.SetInstructionLimit(0)
	res, err := sInsn.DoString("return 21 + 21")
	if err != nil || len(res) != 1 || res[0].Int() != 42 {
		t.Fatalf("State unusable after compile abort: res=%v err=%v", res, err)
	}
}

func TestRootValueBeforeKeyAllocationPreservesMemoryLimit(t *testing.T) {
	const limit = int64(65536)
	// Size value and key at 60% of the remaining headroom each: the value alone fits, but
	// value+key cannot, so an unrooted value would let the assignment exceed the limit.
	sizes := func(s *moelua.State) (string, string) {
		n := int((limit - s.MemoryUsage()) * 3 / 5)
		return strings.Repeat("v", n), strings.Repeat("k", n)
	}

	t.Run("TrySetGlobal", func(t *testing.T) {
		s := moelua.New(moelua.WithMemoryLimit(limit))
		defer s.Close()
		s.CollectGarbage()
		big, name := sizes(s)
		v, err := s.TryString(big)
		if err != nil {
			t.Fatal(err)
		}
		err = s.TrySetGlobal(name, v)
		s.CollectGarbage()
		if usage := s.MemoryUsage(); usage > limit {
			t.Fatalf("TrySetGlobal err=%v but recounted MemoryUsage()=%d exceeded %d", err, usage, limit)
		}
	})

	t.Run("Table.SetString", func(t *testing.T) {
		s := moelua.New(moelua.WithMemoryLimit(limit))
		defer s.Close()
		tbl, err := s.TryNewTable(0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.TrySetGlobal("t", tbl.Value()); err != nil {
			t.Fatal(err)
		}
		s.CollectGarbage()
		big, name := sizes(s)
		v, err := s.TryString(big)
		if err != nil {
			t.Fatal(err)
		}
		err = tbl.SetString(name, v)
		s.CollectGarbage()
		if usage := s.MemoryUsage(); usage > limit {
			t.Fatalf("SetString err=%v but recounted MemoryUsage()=%d exceeded %d", err, usage, limit)
		}
	})
}

func TestRootOperandsAcrossAllocationPreservesMemoryLimit(t *testing.T) {
	t.Run("Table.SetInt", func(t *testing.T) {
		s := moelua.New()
		defer s.Close()
		tbl := s.NewTable(2000, 0)
		if err := s.TrySetGlobal("t", tbl.Value()); err != nil {
			t.Fatal(err)
		}
		s.CollectGarbage()
		// 50,000 bytes of headroom: the 30,000-byte value fits, value+array growth does not.
		limit := s.MemoryUsage() + 50000
		s.SetMemoryLimit(limit)
		v, err := s.TryString(strings.Repeat("v", 30000))
		if err != nil {
			t.Fatal(err)
		}
		err = tbl.SetInt(1, v)
		s.CollectGarbage()
		if usage := s.MemoryUsage(); usage > limit {
			t.Fatalf("SetInt err=%v but recounted MemoryUsage()=%d exceeded %d", err, usage, limit)
		}
	})

	t.Run("Table.Set_int_key", func(t *testing.T) {
		s := moelua.New()
		defer s.Close()
		tbl := s.NewTable(2000, 0)
		if err := s.TrySetGlobal("t", tbl.Value()); err != nil {
			t.Fatal(err)
		}
		s.CollectGarbage()
		// 50,000 bytes of headroom: the 30,000-byte value fits, value+array growth does not.
		limit := s.MemoryUsage() + 50000
		s.SetMemoryLimit(limit)
		v, err := s.TryString(strings.Repeat("v", 30000))
		if err != nil {
			t.Fatal(err)
		}
		err = tbl.Set(moelua.Int(1), v)
		s.CollectGarbage()
		if usage := s.MemoryUsage(); usage > limit {
			t.Fatalf("Set err=%v but recounted MemoryUsage()=%d exceeded %d", err, usage, limit)
		}
	})

	t.Run("Table.SetString_key", func(t *testing.T) {
		s := moelua.New()
		defer s.Close()
		tbl := s.NewTable(0, 0)
		if err := s.TrySetGlobal("t", tbl.Value()); err != nil {
			t.Fatal(err)
		}
		s.CollectGarbage()
		// Room for the 30,000-byte key (len+48) but not for the table's storage growth, so the
		// insertion runs an emergency GC that must still count the new key.
		limit := s.MemoryUsage() + 30048
		s.SetMemoryLimit(limit)
		err := tbl.SetString(strings.Repeat("k", 30000), moelua.Int(1))
		s.CollectGarbage()
		if usage := s.MemoryUsage(); usage > limit {
			t.Fatalf("SetString err=%v but recounted MemoryUsage()=%d exceeded %d", err, usage, limit)
		}
	})

	nameLimit := func(s *moelua.State, name string) int64 {
		s.CollectGarbage()
		// Room for the interned name (len+48) plus a few bytes: the following allocation must not fit.
		l := s.MemoryUsage() + int64(len(name)) + 48 + 8
		s.SetMemoryLimit(l)
		return l
	}

	t.Run("Preload", func(t *testing.T) {
		s := moelua.New()
		defer s.Close()
		name := strings.Repeat("p", 30000)
		l := nameLimit(s, name)
		err := s.Preload(name, func(*moelua.State, []moelua.Value) ([]moelua.Value, error) { return nil, nil })
		s.CollectGarbage()
		if usage := s.MemoryUsage(); usage > l {
			t.Fatalf("Preload err=%v but recounted MemoryUsage()=%d exceeded %d", err, usage, l)
		}
	})

	t.Run("TryNewMetatable", func(t *testing.T) {
		s := moelua.New()
		defer s.Close()
		name := strings.Repeat("m", 30000)
		l := nameLimit(s, name)
		_, _, err := s.TryNewMetatable(name)
		s.CollectGarbage()
		if usage := s.MemoryUsage(); usage > l {
			t.Fatalf("TryNewMetatable err=%v but recounted MemoryUsage()=%d exceeded %d", err, usage, l)
		}
	})

	t.Run("NewThread", func(t *testing.T) {
		s := moelua.New()
		defer s.Close()
		s.SetMemoryLimit(1 << 30)
		rets, err := s.DoString(`local big = string.rep("c", 35000) return function() return #big end`)
		if err != nil {
			t.Fatal(err)
		}
		fn := rets[0]
		// Too little headroom for the thread, so its allocation runs an emergency GC; that GC
		// must still count fn's 35,000-byte upvalue.
		l := s.MemoryUsage() + 5000
		s.SetMemoryLimit(l)
		th, err := s.NewThread(fn)
		if err != nil {
			return
		}
		h := s.Pin(th.Value())
		defer h.Release()
		s.CollectGarbage()
		if usage := s.MemoryUsage(); usage > l {
			t.Fatalf("NewThread succeeded but recounted MemoryUsage()=%d exceeded %d", usage, l)
		}
	})

	t.Run("NewUserdataWithMeta", func(t *testing.T) {
		s := moelua.New()
		defer s.Close()
		s.SetMemoryLimit(1 << 30)
		rets, err := s.DoString(`return {payload = string.rep("m", 30000)}`)
		if err != nil {
			t.Fatal(err)
		}
		mt := s.TableOf(rets[0])
		pin := s.Pin(rets[0])
		s.CollectGarbage()
		pin.Release()
		// Less headroom than the 64-byte userdata, so its allocation runs an emergency GC; that
		// GC must still count the metatable's 30,000-byte payload.
		l := s.MemoryUsage() + 32
		s.SetMemoryLimit(l)
		u, err := s.NewUserdataWithMeta(struct{}{}, mt)
		if err != nil {
			return
		}
		h := s.Pin(u)
		defer h.Release()
		s.CollectGarbage()
		if usage := s.MemoryUsage(); usage > l {
			t.Fatalf("NewUserdataWithMeta succeeded but recounted MemoryUsage()=%d exceeded %d", usage, l)
		}
	})
}

func TestCall1AndToStringClonesAcrossClose(t *testing.T) {
	s := moelua.New()
	res, err := s.DoString(`return function(a, b) return "sum=" .. tostring(a + b) end`)
	if err != nil || len(res) != 1 {
		t.Fatalf("DoString failed: %v %v", res, err)
	}
	v, err := s.Call1(res[0], moelua.Int(19), moelua.Int(23))
	if err != nil {
		t.Fatalf("Call1 failed: %v", err)
	}
	got, ok := s.ToString(v)
	if !ok || got != "sum=42" {
		t.Fatalf("ToString = (%q, %v), want (\"sum=42\", true)", got, ok)
	}
	if _, ok := s.ToString(moelua.Int(7)); ok {
		t.Fatalf("ToString on non-string must return false")
	}
	// Close the State and overwrite Go heap; the cloned Go string must stay intact.
	s.Close()
	if got != "sum=42" {
		t.Fatalf("cloned string corrupted after Close: %q", got)
	}
	if _, err := s.Call1(res[0]); !errors.Is(err, moelua.ErrStateClosed) {
		t.Fatalf("Call1 on closed State = %v, want ErrStateClosed", err)
	}
}

func TestStateFormatValueRawEqualAndLuaErrorStability(t *testing.T) {
	s := moelua.New()
	if got := s.FormatValue(moelua.Int(123)); got != "123" {
		t.Fatalf("FormatValue(123) = %q", got)
	}
	strVal := s.String("arena_boundary_msg_1")
	gotStr := s.FormatValue(strVal)
	if gotStr != "arena_boundary_msg_1" {
		t.Fatalf("FormatValue(str) = %q", gotStr)
	}
	if !s.RawEqual(moelua.Int(7), moelua.Float(7.0)) || s.RawEqual(moelua.Int(7), moelua.Int(8)) {
		t.Fatalf("RawEqual numeric mismatch")
	}
	if !s.RawEqual(s.String("hello"), s.String("hello")) || s.RawEqual(s.String("hello"), s.String("world")) {
		t.Fatalf("RawEqual string mismatch")
	}

	// Uncaught LuaError returned from DoString/Call/Call1/Resume must retain a stable Error()
	// string even after GC sweeps and overwrites the original Arena slot AND after State.Close().
	_, callErr := s.DoString(`error(string.sub("ZZerr_payload_call_0001", 3), 0)`)
	if callErr == nil {
		t.Fatal("expected error from DoString")
	}
	fnRes, err := s.DoString(`return function() error(string.sub("YYerr_payload_call1_002", 3), 0) end`)
	if err != nil || len(fnRes) != 1 {
		t.Fatalf("setup Call1 error fn failed: %v %v", fnRes, err)
	}
	_, call1Err := s.Call1(fnRes[0])
	if call1Err == nil {
		t.Fatal("expected error from Call1")
	}
	coFnRes, err := s.DoString(`return function() error(string.sub("XXerr_payload_coro_0003", 3), 0) end`)
	if err != nil || len(coFnRes) != 1 {
		t.Fatalf("setup coro error fn failed: %v %v", coFnRes, err)
	}
	co, err := s.NewThread(coFnRes[0])
	if err != nil {
		t.Fatal(err)
	}
	_, _, coroErr := co.Resume()
	if coroErr == nil {
		t.Fatal("expected error from Resume")
	}

	// Sweep unreachable Arena error strings and overwrite slots with fresh 21-byte strings.
	s.CollectGarbage()
	for i := range 64 {
		_ = s.String(fmt.Sprintf("overwrite_slot_x_%04d", i))
	}
	if callErr.Error() != "err_payload_call_0001" {
		t.Fatalf("DoString LuaError corrupted after GC reuse: %q", callErr.Error())
	}
	if call1Err.Error() != "err_payload_call1_002" {
		t.Fatalf("Call1 LuaError corrupted after GC reuse: %q", call1Err.Error())
	}
	if coroErr.Error() != "err_payload_coro_0003" {
		t.Fatalf("Resume LuaError corrupted after GC reuse: %q", coroErr.Error())
	}

	s.Close()
	if gotStr != "arena_boundary_msg_1" {
		t.Fatalf("FormatValue string corrupted after Close: %q", gotStr)
	}
	if callErr.Error() != "err_payload_call_0001" || call1Err.Error() != "err_payload_call1_002" || coroErr.Error() != "err_payload_coro_0003" {
		t.Fatalf("LuaError messages corrupted after Close: %q / %q / %q", callErr.Error(), call1Err.Error(), coroErr.Error())
	}
}

func TestDeleteLongStringKeyViaLookupString(t *testing.T) {
	s := moelua.New()
	defer s.Close()

	longKey := strings.Repeat("k", 64)
	missingKey := strings.Repeat("m", 64)

	tbl := s.NewTable(0, 4)
	if err := tbl.SetString(longKey, moelua.Int(123)); err != nil {
		t.Fatalf("SetString insert: %v", err)
	}
	if got := tbl.GetString(longKey); !got.IsInt() || got.Int() != 123 {
		t.Fatalf("GetString before delete = %v, want 123", got)
	}
	// Delete existing >40-byte key through LookupString transient probe.
	if err := tbl.SetString(longKey, moelua.Nil()); err != nil {
		t.Fatalf("SetString delete: %v", err)
	}
	if got := tbl.GetString(longKey); !got.IsNil() {
		t.Fatalf("GetString after delete = %v, want nil", got)
	}
	// Deleting an absent >40-byte key must be a clean no-op.
	if err := tbl.SetString(missingKey, moelua.Nil()); err != nil {
		t.Fatalf("SetString delete missing: %v", err)
	}

	// Same path through TrySetGlobal and Preload.
	if err := s.TrySetGlobal(longKey, moelua.Int(456)); err != nil {
		t.Fatalf("TrySetGlobal insert: %v", err)
	}
	if got := s.GetGlobal(longKey); !got.IsInt() || got.Int() != 456 {
		t.Fatalf("GetGlobal before delete = %v, want 456", got)
	}
	if err := s.TrySetGlobal(longKey, moelua.Nil()); err != nil {
		t.Fatalf("TrySetGlobal delete: %v", err)
	}
	if got := s.GetGlobal(longKey); !got.IsNil() {
		t.Fatalf("GetGlobal after delete = %v, want nil", got)
	}
	if err := s.Preload(longKey, func(_ *moelua.State, _ []moelua.Value) ([]moelua.Value, error) {
		return []moelua.Value{moelua.Bool(true)}, nil
	}); err != nil {
		t.Fatalf("Preload insert: %v", err)
	}
	if err := s.Preload(longKey, nil); err != nil {
		t.Fatalf("Preload delete: %v", err)
	}
}

func TestRetainedInlineCachesAccountedInMemoryLimit(t *testing.T) {
	// Generate a chunk with many distinct GETFIELD/SETFIELD IC sites.
	var sb strings.Builder
	sb.WriteString("local t = {a=1}\n")
	for i := range 64 {
		fmt.Fprintf(&sb, "t.f%d = t.a\n", i)
	}
	sb.WriteString("return t.a\n")
	chunk, err := moelua.Compile("=ic_heavy", sb.String())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	// Verify MemoryUsage grows with retained IC slices and drops when unpinned & collected.
	s := moelua.New()
	defer s.Close()
	s.CollectGarbage()
	base := s.MemoryUsage()

	var pins []*moelua.Handle
	for range 20 {
		fn, err := s.Load(chunk)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		pins = append(pins, s.Pin(fn))
	}
	s.CollectGarbage()
	withPins := s.MemoryUsage()
	if withPins <= base {
		t.Fatalf("expected MemoryUsage to grow with retained IC chunks: base=%d withPins=%d", base, withPins)
	}
	// Under a tight memory limit just above base, pinning many copies must hit OOM
	// rather than letting uncounted IC slices exceed the limit.
	limit := base + (withPins-base)/2
	sLim := moelua.New(moelua.WithMemoryLimit(limit))
	defer sLim.Close()
	var limPins []*moelua.Handle
	oom := false
	for range 30 {
		fn, err := sLim.Load(chunk)
		if err != nil {
			if strings.Contains(err.Error(), "not enough memory") {
				oom = true
				break
			}
			t.Fatalf("unexpected Load error: %v", err)
		}
		limPins = append(limPins, sLim.Pin(fn))
	}
	if !oom {
		t.Fatalf("expected OOM loading IC-heavy chunks under limit=%d (usage=%d)", limit, sLim.MemoryUsage())
	}
	sLim.CollectGarbage()
	if usage := sLim.MemoryUsage(); usage > limit {
		t.Fatalf("retained IC chunks exceeded memory limit: usage=%d limit=%d", usage, limit)
	}
	for _, h := range limPins {
		h.Release()
	}
	for _, h := range pins {
		h.Release()
	}
	s.CollectGarbage()
	afterFree := s.MemoryUsage()
	if afterFree > base+256 {
		t.Fatalf("expected IC memory reclaimed after unpinning: base=%d afterFree=%d", base, afterFree)
	}
}

func TestTempRootsZeroAllocAndEmergencyGCSafety(t *testing.T) {
	s := moelua.New()
	defer s.Close()

	// 1. Immediate non-GC values must not push onto tempRoots or allocate.
	raw := s.Raw()
	if mark := raw.PinTempMark(); mark != 0 {
		t.Fatalf("expected initial PinTempMark=0, got %d", mark)
	}
	raw.PinTemp(moelua.Nil())
	raw.PinTemp(moelua.Bool(true))
	raw.PinTemp(moelua.Int(42))
	raw.PinTemp(moelua.Float(3.14))
	if got := raw.PinTempMark(); got != 0 {
		t.Fatalf("expected non-GC immediates to skip tempRoots, got depth=%d", got)
	}

	// 2. GC objects pinned via PinTemp must survive CollectFull and release cleanly on UnpinTemp.
	unrooted := s.NewTable(0, 0)
	mark := raw.PinTempMark()
	raw.PinTemp(unrooted.Value())
	if raw.PinTempMark() != mark+1 {
		t.Fatalf("expected GC table to push onto tempRoots")
	}
	raw.CollectFull()
	if err := unrooted.SetInt(1, moelua.Int(99)); err != nil {
		t.Fatalf("pinned unrooted table should remain live across CollectFull: %v", err)
	}
	if got := unrooted.GetInt(1); !got.IsInt() || got.Int() != 99 {
		t.Fatalf("unexpected value in surviving table: %v", got)
	}
	raw.UnpinTemp(mark)
	if raw.PinTempMark() != 0 {
		t.Fatalf("expected PinTempMark=0 after UnpinTemp, got %d", raw.PinTempMark())
	}

	// 3. Oversized tempRoots (>64) shrinks back to inline buffer on full unwind.
	deepMark := raw.PinTempMark()
	tbl := s.NewTable(0, 0)
	s.SetGlobal("keep_tbl", tbl.Value())
	for range 80 {
		raw.PinTemp(tbl.Value())
	}
	if raw.PinTempMark() != 80 {
		t.Fatalf("expected depth=80, got %d", raw.PinTempMark())
	}
	raw.UnpinTemp(deepMark)
	if raw.PinTempMark() != 0 {
		t.Fatalf("expected depth=0 after deep unwind, got %d", raw.PinTempMark())
	}

	// 4. High-level Table.Set, Table.SetInt, Table.SetString, and TrySetGlobal must be 0 allocs/op.
	if err := tbl.SetString("alpha", moelua.Int(1)); err != nil {
		t.Fatal(err)
	}
	if err := tbl.SetInt(1, moelua.Int(1)); err != nil {
		t.Fatal(err)
	}
	keyStr := s.String("alpha")
	valStr := s.String("payload")
	if err := s.TrySetGlobal("bench_g", valStr); err != nil {
		t.Fatal(err)
	}

	checkZeroAlloc := func(name string, fn func()) {
		t.Helper()
		allocs := testing.AllocsPerRun(200, fn)
		if allocs != 0 {
			t.Fatalf("%s allocated %.2f allocs/op, want 0", name, allocs)
		}
	}
	checkZeroAlloc("Table.Set(int, int)", func() {
		if err := tbl.Set(moelua.Int(1), moelua.Int(123)); err != nil {
			t.Fatal(err)
		}
	})
	checkZeroAlloc("Table.Set(str, str)", func() {
		if err := tbl.Set(keyStr, valStr); err != nil {
			t.Fatal(err)
		}
	})
	checkZeroAlloc("Table.SetInt(1, valStr)", func() {
		if err := tbl.SetInt(1, valStr); err != nil {
			t.Fatal(err)
		}
	})
	checkZeroAlloc("Table.SetString(alpha, int)", func() {
		if err := tbl.SetString("alpha", moelua.Int(456)); err != nil {
			t.Fatal(err)
		}
	})
	checkZeroAlloc("State.TrySetGlobal(bench_g, valStr)", func() {
		if err := s.TrySetGlobal("bench_g", valStr); err != nil {
			t.Fatal(err)
		}
	})
}

func BenchmarkTableSet(b *testing.B) {
	s := moelua.New()
	defer s.Close()
	tbl := s.NewTable(0, 0)
	s.SetGlobal("t", tbl.Value())
	key := s.String("field")
	val := s.String("value")
	_ = tbl.Set(key, val)
	_ = tbl.SetInt(1, moelua.Int(1))

	b.Run("Set_StrVal", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = tbl.Set(key, val)
		}
	})
	b.Run("SetInt_Imm", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = tbl.SetInt(1, moelua.Int(42))
		}
	})
	b.Run("SetString_Imm", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = tbl.SetString("field", moelua.Int(42))
		}
	})
}
