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
	if len(res) != 2 || res[0].Int() != 123 || res[1].AsString().S != "done" {
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
	if h.Value().AsTable().RawGetStr(s.Raw().Intern("tag")).AsString().S != "anchored" {
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
	if len(res) != 1 || res[0].AsString().S != "still-alive" {
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
		name := moelua.FormatValue(args[0])
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
	if len(res) != 1 || res[0].AsString().S != "ok" {
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
	if name := mt.GetString("__name"); !name.IsString() || name.AsString().S != "moelua.Counter" {
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
		seen[moelua.FormatValue(k)] = moelua.FormatValue(v)
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
	if err != nil || !yielded || len(r1) != 2 || r1[0].AsString().S != "step1" || r1[1].Int() != 5 {
		t.Fatalf("Resume 1: res=%v yielded=%v err=%v", r1, yielded, err)
	}
	if th.Status() != moelua.ThreadSuspended || th.StatusString() != "suspended" {
		t.Fatalf("after yield status=%v (%q)", th.Status(), th.StatusString())
	}

	r2, yielded, err := th.Resume(moelua.Int(7))
	if err != nil || !yielded || len(r2) != 2 || r2[0].AsString().S != "step2" || r2[1].Int() != 70 {
		t.Fatalf("Resume 2: res=%v yielded=%v err=%v", r2, yielded, err)
	}

	r3, yielded, err := th.Resume(moelua.Int(100))
	if err != nil || yielded || len(r3) != 2 || r3[0].AsString().S != "done" || r3[1].Int() != 105 {
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
	s := moelua.New(moelua.WithMemoryLimit(65536))
	defer s.Close()

	if tbl := s.NewTable(0, 100000); tbl != nil {
		t.Fatalf("expected nil from NewTable(0, 100000) under 64KiB memory limit")
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
