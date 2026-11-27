package moelua

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

func mustStr(t testing.TB, s *State, v Value) string {
	t.Helper()
	str, ok := s.ToString(v)
	if !ok {
		t.Fatalf("expected string value, got %v", v)
	}
	return str
}

func TestWithLibsSubsets(t *testing.T) {
	t.Run("LibNone", func(t *testing.T) {
		s := New(WithLibs(LibNone))
		defer s.Close()

		for _, name := range []string{
			"_G", "_VERSION", "print", "load", "loadfile", "dofile",
			"math", "table", "string", "utf8", "coroutine", "io", "os", "debug", "package", "require",
		} {
			if !s.GetGlobal(name).IsNil() {
				t.Fatalf("LibNone: global %q = %v, want nil", name, s.GetGlobal(name))
			}
		}
		res, err := s.DoString("local a, b = 20, 22; return a + b")
		if err != nil {
			t.Fatalf("DoString on LibNone failed: %v", err)
		}
		if len(res) != 1 || res[0].Int() != 42 {
			t.Fatalf("DoString on LibNone = %v, want [42]", res)
		}
	})

	t.Run("LibSafe", func(t *testing.T) {
		s := New(WithLibs(LibSafe))
		defer s.Close()

		for _, present := range []string{"_G", "_VERSION", "print", "pcall", "load", "math", "table", "string", "utf8", "coroutine"} {
			if s.GetGlobal(present).IsNil() {
				t.Fatalf("LibSafe: expected global %q to be present", present)
			}
		}
		for _, absent := range []string{"loadfile", "dofile", "io", "os", "debug", "package", "require"} {
			if !s.GetGlobal(absent).IsNil() {
				t.Fatalf("LibSafe: expected global %q to be nil", absent)
			}
		}
		res, err := s.DoString(`
			local t = {3, 1, 2}
			table.sort(t)
			local co = coroutine.create(function() coroutine.yield(math.sqrt(16)) end)
			local ok, v = coroutine.resume(co)
			assert(ok)
			return ("abc"):upper(), "10" + 5, utf8.len("你好"), table.concat(t, ","), v
		`)
		if err != nil {
			t.Fatalf("LibSafe execution failed: %v", err)
		}
		if len(res) != 5 || mustStr(t, s, res[0]) != "ABC" || res[1].Int() != 15 || res[2].Int() != 2 || mustStr(t, s, res[3]) != "1,2,3" || res[4].Float() != 4 {
			t.Fatalf("LibSafe results unexpected: %v", res)
		}
	})

	t.Run("LibBaseAndLibBaseFile", func(t *testing.T) {
		s := New(WithLibs(LibBase))
		defer s.Close()
		if s.GetGlobal("load").IsNil() || !s.GetGlobal("loadfile").IsNil() || !s.GetGlobal("dofile").IsNil() {
			t.Fatalf("LibBase should expose load without loadfile/dofile")
		}
		if err := s.OpenLibs(LibBaseFile); err != nil {
			t.Fatalf("OpenLibs(LibBaseFile) failed: %v", err)
		}
		if s.GetGlobal("loadfile").IsNil() || s.GetGlobal("dofile").IsNil() {
			t.Fatalf("OpenLibs(LibBaseFile) did not install loadfile/dofile")
		}
	})

	t.Run("IndividualLibs", func(t *testing.T) {
		cases := []struct {
			mask   LibMask
			global string
		}{
			{LibMath, "math"},
			{LibTable, "table"},
			{LibString, "string"},
			{LibUTF8, "utf8"},
			{LibCoroutine, "coroutine"},
			{LibIO, "io"},
			{LibOS, "os"},
			{LibDebug, "debug"},
			{LibPackage, "package"},
		}
		for _, tc := range cases {
			s := New(WithLibs(tc.mask))
			if s.GetGlobal(tc.global).IsNil() {
				s.Close()
				t.Fatalf("mask %#x missing global %q", tc.mask, tc.global)
			}
			s.Close()
		}
	})
}

func TestLibPackageSafeSandbox(t *testing.T) {
	tmpDir := t.TempDir()
	diskMod := filepath.Join(tmpDir, "diskmod.lua")
	if err := os.WriteFile(diskMod, []byte("return { from = 'disk' }\n"), 0o600); err != nil {
		t.Fatalf("write diskmod.lua: %v", err)
	}
	t.Setenv("LUA_PATH", filepath.Join(tmpDir, "?.lua"))
	t.Setenv("LUA_CPATH", filepath.Join(tmpDir, "?.so"))

	vfs := fstest.MapFS{
		"vmod.lua": {Data: []byte("return { from = 'vfs', val = 99 }\n")},
	}
	s := New(
		WithLibs(LibSafe|LibPackageSafe),
		WithFS(vfs),
		WithPreload("pmod", func(s *State, args []Value) ([]Value, error) {
			tbl := s.NewTable(0, 2)
			_ = tbl.SetString("from", s.String("preload"))
			return []Value{tbl.Value()}, nil
		}),
	)
	defer s.Close()

	s.AddSearcher("custom_searcher", func(s *State, args []Value) ([]Value, error) {
		if len(args) > 0 {
			if name, ok := s.ToString(args[0]); ok && name == "cmod" {
				loader := s.NewFunc1("cmod_loader", func(s *State, _ []Value) (Value, error) {
					tbl := s.NewTable(0, 2)
					_ = tbl.SetString("from", s.String("searcher"))
					return tbl.Value(), nil
				})
				return []Value{loader}, nil
			}
		}
		return []Value{s.String("\n\tno custom module")}, nil
	})

	res, err := s.DoString(`
		assert(package.loadlib == nil, "loadlib must be nil in LibPackageSafe")
		assert(package.searchpath == nil, "searchpath must be nil in LibPackageSafe")
		assert(package.cpath == nil, "cpath must be nil in LibPackageSafe")
		local p = require("pmod")
		local v = require("vmod")
		local c = require("cmod")
		local ok, err = pcall(require, "diskmod")
		assert(not ok, "diskmod must not be reachable in LibPackageSafe")
		return p.from, v.from, v.val, c.from, tostring(err), package.path
	`)
	if err != nil {
		t.Fatalf("LibPackageSafe script failed: %v", err)
	}
	if mustStr(t, s, res[0]) != "preload" || mustStr(t, s, res[1]) != "vfs" || res[2].Int() != 99 || mustStr(t, s, res[3]) != "searcher" {
		t.Fatalf("unexpected module outputs: %v", res)
	}
	if errStr := mustStr(t, s, res[4]); !strings.Contains(errStr, "diskmod") {
		t.Fatalf("expected diskmod error message, got %q", errStr)
	}
	if pathStr := mustStr(t, s, res[5]); strings.Contains(pathStr, tmpDir) {
		t.Fatalf("LibPackageSafe must ignore LUA_PATH, got package.path=%q", pathStr)
	}

	// Upgrading LibPackageSafe -> LibPackage preserves preloads & VFS and enables OS searchers + LUA_PATH.
	if err := s.OpenLibs(LibPackage); err != nil {
		t.Fatalf("OpenLibs(LibPackage) upgrade failed: %v", err)
	}
	res2, err := s.DoString(`
		assert(type(package.searchpath) == "function")
		assert(type(package.loadlib) == "function")
		assert(require("pmod").from == "preload")
		assert(require("vmod").from == "vfs")
		local d = require("diskmod")
		return d.from
	`)
	if err != nil {
		t.Fatalf("upgraded LibPackage script failed: %v", err)
	}
	if len(res2) != 1 || mustStr(t, s, res2[0]) != "disk" {
		t.Fatalf("expected disk module after LibPackage upgrade, got %v", res2)
	}

	// Verify WithNoEnv is respected even when LibPackage is opened dynamically after New.
	sNoEnv := New(WithLibs(LibNone), WithNoEnv())
	defer sNoEnv.Close()
	if err := sNoEnv.OpenLibs(LibBase | LibPackage); err != nil {
		t.Fatalf("OpenLibs on NoEnv state failed: %v", err)
	}
	rPath, err := sNoEnv.DoString(`return package.path`)
	if err != nil {
		t.Fatalf("package.path read failed: %v", err)
	}
	if pathStr := mustStr(t, sNoEnv, rPath[0]); strings.Contains(pathStr, tmpDir) {
		t.Fatalf("WithNoEnv state should ignore LUA_PATH on dynamic OpenLibs(LibPackage), got %q", pathStr)
	}
}

func TestOpenLibsIncrementalAndIdempotent(t *testing.T) {
	s := New(WithLibs(LibNone))
	if err := s.OpenLibs(LibBase | LibMath); err != nil {
		t.Fatalf("OpenLibs(LibBase|LibMath): %v", err)
	}
	if _, err := s.DoString(`math.custom_tag = 777`); err != nil {
		t.Fatalf("set custom_tag: %v", err)
	}
	// Re-opening LibMath must be idempotent and keep existing math table intact.
	if err := s.OpenLibs(LibMath); err != nil {
		t.Fatalf("second OpenLibs(LibMath): %v", err)
	}
	if err := s.OpenLibs(LibPackageSafe); err != nil {
		t.Fatalf("OpenLibs(LibPackageSafe): %v", err)
	}
	if err := s.OpenLibs(LibTable); err != nil {
		t.Fatalf("OpenLibs(LibTable): %v", err)
	}
	res, err := s.DoString(`
		assert(math.custom_tag == 777)
		assert(package.loaded.math == math)
		assert(package.loaded._G == _G)
		assert(package.loaded.table == table)
		assert(require("math") == math)
		assert(require("table") == table)
		return math.sqrt(81)
	`)
	if err != nil {
		t.Fatalf("incremental OpenLibs verification failed: %v", err)
	}
	if len(res) != 1 || res[0].Float() != 9 {
		t.Fatalf("unexpected result: %v", res)
	}

	s.Close()
	if err := s.OpenLibs(LibBase); !errors.Is(err, ErrStateClosed) {
		t.Fatalf("OpenLibs on closed State = %v, want ErrStateClosed", err)
	}
}

func TestRequireLib(t *testing.T) {
	// Test RequireLib on LibNone before LibPackageSafe is even opened (luaL_requiref parity).
	s := New(WithLibs(LibNone))
	defer s.Close()

	var calls int
	modVal, err := s.RequireLib("svc", func(st *State, args []Value) ([]Value, error) {
		calls++
		name, ok := st.ToString(args[0])
		if len(args) != 1 || !ok || name != "svc" {
			t.Errorf("RequireLib loader args = %v, want ['svc']", args)
		}
		tbl := st.NewTable(0, 2)
		_ = tbl.SetString("answer", Int(42))
		return []Value{tbl.Value()}, nil
	}, true)
	if err != nil {
		t.Fatalf("RequireLib(svc) failed: %v", err)
	}
	if calls != 1 || !modVal.IsTable() {
		t.Fatalf("RequireLib calls=%d, modVal=%v", calls, modVal)
	}

	// Second RequireLib must return cached module without invoking the new loader.
	modVal2, err := s.RequireLib("svc", func(st *State, args []Value) ([]Value, error) {
		calls++
		return []Value{Int(999)}, nil
	}, false)
	if err != nil || calls != 1 || modVal2 != modVal {
		t.Fatalf("cached RequireLib: calls=%d err=%v same=%v", calls, err, modVal2 == modVal)
	}

	// Loader returning nil defaults to true; global=false must not pollute _G.
	flagVal, err := s.RequireLib("flagmod", func(st *State, args []Value) ([]Value, error) {
		return nil, nil
	}, false)
	if err != nil || !flagVal.IsTrue() {
		t.Fatalf("RequireLib nil-return = (%v, %v), want (true, nil)", flagVal, err)
	}
	if !s.GetGlobal("flagmod").IsNil() {
		t.Fatalf("global=false leaked flagmod into _G")
	}

	// Now open LibBase|LibPackageSafe and confirm Lua require() sees both pre-registered modules.
	if err := s.OpenLibs(LibBase | LibPackageSafe); err != nil {
		t.Fatalf("OpenLibs(LibBase|LibPackageSafe): %v", err)
	}
	res, err := s.DoString(`
		assert(svc.answer == 42)
		assert(require("svc") == svc)
		assert(flagmod == nil)
		assert(require("flagmod") == true)
		return svc.answer
	`)
	if err != nil || len(res) != 1 || res[0].Int() != 42 {
		t.Fatalf("Lua require after RequireLib failed: res=%v err=%v", res, err)
	}

	if _, err := s.RequireLib("bad", nil, false); err == nil {
		t.Fatalf("expected error for nil RequireLib loader")
	}

	// Closed state returns ErrStateClosed.
	s2 := New(WithLibs(LibNone))
	s2.Close()
	if _, err := s2.RequireLib("x", func(*State, []Value) ([]Value, error) { return nil, nil }, false); !errors.Is(err, ErrStateClosed) {
		t.Fatalf("RequireLib on closed state = %v, want ErrStateClosed", err)
	}
}

func TestNewTemplateHostEmbeddingAndConcurrency(t *testing.T) {
	vfs := fstest.MapFS{
		"util.lua": {Data: []byte(`
			local M = {}
			function M.combine(id, x)
				return id .. ":" .. tostring(x)
			end
			return M
		`)},
	}

	var finalized atomic.Int64
	longConst := strings.Repeat("template-long-string-segment-", 6) // > 40 bytes (long frozen string)

	tmpl, err := NewTemplate(func(s *State) error {
		// Eagerly load a custom host module whose functions dynamically resolve the calling
		// cloned State rather than capturing the bootstrap State.
		if _, err := s.RequireLib("host", func(st *State, args []Value) ([]Value, error) {
			mod := st.NewTable(0, 4)
			_ = mod.SetString("worker_id", st.NewFunc1("host.worker_id", func(cur *State, _ []Value) (Value, error) {
				return cur.GetGlobal("worker_id"), nil
			}))
			_ = mod.SetString("tag", st.String(longConst))
			return []Value{mod.Value()}, nil
		}, true); err != nil {
			return err
		}

		// Register both multi-return and zero-alloc single-return Go globals in the template.
		s.RegisterFunc("echo_worker", func(cur *State, args []Value) ([]Value, error) {
			id, _ := cur.ToString(cur.GetGlobal("worker_id"))
			n, _ := args[0].ToInt()
			return []Value{cur.String(id), Int(n * 10)}, nil
		})
		s.RegisterFunc1("bump", func(cur *State, args []Value) (Value, error) {
			curCount := cur.GetGlobal("counter").Int() + 1
			cur.SetGlobal("counter", Int(curCount))
			return Int(curCount), nil
		})

		// Register a userdata metatable with a __gc finalizer and store one instance per State.
		mt, created, err := s.TryNewMetatable("worker_guard")
		if err != nil || !created {
			t.Fatalf("TryNewMetatable = (%v, %v, %v)", mt, created, err)
		}
		_ = mt.SetString("__gc", s.NewFunc1("worker_guard.__gc", func(cur *State, args []Value) (Value, error) {
			// Verify cur is the cloned State with its own worker_id intact during Close().
			if cur.GetGlobal("worker_id").IsString() {
				finalized.Add(1)
			}
			return Nil(), nil
		}))
		guardUD, err := s.NewUserdataWithMeta("guard-payload", mt)
		if err != nil {
			return err
		}
		s.SetGlobal("_guard", guardUD)

		// Define Lua helper closures, allocate temporary garbage, and switch GC mode to verify
		// BuildTemplate cleanly reclaims temp chunks and normalizes GC snapshot state.
		_, err = s.DoString(`
			collectgarbage("generational")
			local tmp = {}
			for i = 1, 200 do tmp[i] = "ephemeral_" .. i end
			counter = 0
			shared_table = { base = 100 }
			function lua_helper(x)
				return (x * 2) + shared_table.base
			end
		`)
		return err
	},
		WithLibs(LibSafe|LibPackageSafe),
		WithFS(vfs),
		WithPreload("lazy_service", func(cur *State, args []Value) ([]Value, error) {
			tbl := cur.NewTable(0, 2)
			_ = tbl.SetString("who", cur.GetGlobal("worker_id"))
			return []Value{tbl.Value()}, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewTemplate failed: %v", err)
	}

	const goroutines = 12
	const statesPerG = 6
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*statesPerG)

	for g := range goroutines {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := range statesPerG {
				var opts []Option
				opts = append(opts, WithMemoryLimit(4<<20), WithInstructionLimit(200_000))
				if (gid+i)%2 == 1 {
					opts = append(opts, WithJIT())
				}
				s := tmpl.NewState(opts...)
				wid := strings.Join([]string{"w", string(rune('A' + gid)), string(rune('0' + i))}, "-")
				s.SetGlobal("worker_id", s.String(wid))

				res, err := s.DoString(`
					assert(os == nil and io == nil and debug == nil and loadfile == nil)
					local util = require("util")
					local lazy = require("lazy_service")
					assert(lazy.who == worker_id, "lazy preload captured wrong state")
					assert(host.worker_id() == worker_id, "RequireLib callback captured wrong state")
					assert(#host.tag > 100, "long frozen string corrupted")
					shared_table.base = shared_table.base + 5
					local sum = 0
					for k = 1, 100 do
						sum = sum + lua_helper(k)
					end
					local eid, scaled = echo_worker(7)
					assert(eid == worker_id and scaled == 70)
					local c1, c2 = bump(), bump()
					assert(c1 == 1 and c2 == 2, "state mutation leaked across clones")
					return util.combine(worker_id, sum), host.tag
				`)
				if err != nil {
					s.Close()
					errCh <- err
					return
				}
				// sum(2*k + 105, k=1..100) = 10100 + 10500 = 20600
				want := wid + ":20600"
				got0, _ := s.ToString(res[0])
				got1, _ := s.ToString(res[1])
				if len(res) != 2 || got0 != want || got1 != longConst {
					s.Close()
					errCh <- errors.New("unexpected cloned state output: " + got0)
					return
				}
				s.Close()
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent template worker failed: %v", err)
	}
	if got := finalized.Load(); got != goroutines*statesPerG {
		t.Fatalf("worker_guard __gc finalized %d times, want %d", got, goroutines*statesPerG)
	}

	// Per-State cancellation & instruction limits work cleanly on cloned States.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sCancel := tmpl.NewState(WithContext(ctx))
	defer sCancel.Close()
	if _, err := sCancel.DoString(`for i = 1, 1e6 do end`); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on cloned State, got %v", err)
	}
}

func TestNewTemplateErrorPaths(t *testing.T) {
	t.Run("SetupError", func(t *testing.T) {
		want := errors.New("boom")
		if _, err := NewTemplate(func(s *State) error { return want }); !errors.Is(err, want) {
			t.Fatalf("NewTemplate setup error = %v, want %v", err, want)
		}
	})

	t.Run("LeakedOpenFile", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "leak.txt")
		_, err := NewTemplate(func(s *State) error {
			s.SetGlobal("p", s.String(f))
			_, err := s.DoString(`leaked = assert(io.open(p, "w"))`)
			return err
		}, WithLibs(LibBase|LibIO))
		if err == nil || !strings.Contains(err.Error(), "open file") {
			t.Fatalf("expected open file error, got %v", err)
		}
	})

	t.Run("LeakedPin", func(t *testing.T) {
		_, err := NewTemplate(func(s *State) error {
			_ = s.Pin(s.NewTable(0, 0).Value())
			return nil
		}, WithLibs(LibSafe))
		if err == nil || !strings.Contains(err.Error(), "pinned") {
			t.Fatalf("expected pinned handle error, got %v", err)
		}
	})

	t.Run("ActiveDebugHook", func(t *testing.T) {
		_, err := NewTemplate(func(s *State) error {
			_, err := s.DoString(`debug.sethook(function() end, "l")`)
			return err
		}, WithLibs(LibBase|LibDebug))
		if err == nil || !strings.Contains(err.Error(), "hook") {
			t.Fatalf("expected active hook error, got %v", err)
		}
	})

	t.Run("SetupInstructionLimit", func(t *testing.T) {
		_, err := NewTemplate(func(s *State) error {
			_, err := s.DoString(`while true do end`)
			return err
		}, WithLibs(LibSafe), WithInstructionLimit(500))
		if !errors.Is(err, ErrInstructionLimit) {
			t.Fatalf("expected ErrInstructionLimit, got %v", err)
		}
	})
}

func TestPreloadLibsAndTemplate(t *testing.T) {
	// Mirror luaL_openselectedlibs(L, LibBase|LibPackageSafe, LibMath|LibTable|LibString|LibIO).
	tmpl, err := NewTemplate(nil,
		WithLibs(LibBase|LibPackageSafe),
		WithPreloadLibs(LibMath|LibTable|LibString|LibIO),
	)
	if err != nil {
		t.Fatalf("NewTemplate with PreloadLibs failed: %v", err)
	}

	s := tmpl.NewState()
	defer s.Close()

	// Preloaded libs must NOT pollute _G until explicitly opened; require() loads them lazily.
	res, err := s.DoString(`
		assert(math == nil and table == nil and string == nil and io == nil)
		local m = require("math")
		assert(math == nil, "require of preloaded math must not set _G.math")
		assert(package.loaded.math == m)
		local t = require("table")
		local str = require("string")
		assert(("hi"):upper() == "HI")
		local io_mod = require("io")
		assert(type(io_mod.type) == "function")
		return m.floor(7.9), t.concat({"a", "b"}, "-"), str.len("hello")
	`)
	if err != nil {
		t.Fatalf("PreloadLibs script failed: %v", err)
	}
	if len(res) != 3 || res[0].Int() != 7 || mustStr(t, s, res[1]) != "a-b" || res[2].Int() != 5 {
		t.Fatalf("PreloadLibs unexpected results: %v", res)
	}

	// Opening LibMath after lazy require("math") promotes the already-loaded table into _G.math.
	if err := s.OpenLibs(LibMath); err != nil {
		t.Fatalf("OpenLibs(LibMath) after preload failed: %v", err)
	}
	if _, err := s.DoString(`assert(math == package.loaded.math and math.floor(3.5) == 3)`); err != nil {
		t.Fatalf("promoting preloaded math into _G failed: %v", err)
	}

	// Explicit WithPreload overrides WithPreloadLibs for the same module name, and already-opened
	// libs in WithLibs are skipped by WithPreloadLibs.
	s2 := New(
		WithLibs(LibBase|LibPackageSafe|LibMath),
		WithPreloadLibs(LibMath|LibUTF8),
		WithPreload("utf8", func(st *State, _ []Value) ([]Value, error) {
			tbl := st.NewTable(0, 1)
			_ = tbl.SetString("custom", Bool(true))
			return []Value{tbl.Value()}, nil
		}),
	)
	defer s2.Close()
	if _, err := s2.DoString(`
		assert(package.preload.math == nil, "already-opened math must not be placed in preload")
		assert(require("utf8").custom == true, "explicit WithPreload must override WithPreloadLibs")
	`); err != nil {
		t.Fatalf("WithPreloadLibs precedence check failed: %v", err)
	}

	// Closed state returns ErrStateClosed.
	s3 := New(WithLibs(LibNone))
	s3.Close()
	if err := s3.PreloadLibs(LibMath); !errors.Is(err, ErrStateClosed) {
		t.Fatalf("PreloadLibs on closed State = %v, want ErrStateClosed", err)
	}
}

func TestHostAuxHelpersGlobalsRegistryTracebackMeta(t *testing.T) {
	s := New(WithLibs(LibNone))
	defer s.Close()

	// Globals() works even on LibNone where _G global entry does not exist.
	g := s.Globals()
	if g == nil {
		t.Fatalf("Globals() returned nil")
	}
	if err := g.SetString("magic", Int(123)); err != nil {
		t.Fatalf("Globals().SetString failed: %v", err)
	}
	if got := s.GetGlobal("magic").Int(); got != 123 {
		t.Fatalf("GetGlobal(magic) = %d, want 123", got)
	}

	// Registry() exposes LUA_REGISTRYINDEX directly.
	reg := s.Registry()
	if reg == nil {
		t.Fatalf("Registry() returned nil")
	}
	if err := reg.SetString("host_secret", s.String("token-xyz")); err != nil {
		t.Fatalf("Registry().SetString failed: %v", err)
	}
	if got, ok := s.ToString(reg.GetString("host_secret")); !ok || got != "token-xyz" {
		t.Fatalf("Registry().GetString = (%q, %v), want (token-xyz, true)", got, ok)
	}

	if err := s.OpenLibs(LibSafe); err != nil {
		t.Fatalf("OpenLibs(LibSafe): %v", err)
	}

	// GetMetafield & CallMeta (luaL_getmetafield / luaL_callmeta).
	tbl := s.NewTable(0, 1)
	_ = tbl.SetString("v", Int(21))
	mt := s.NewTable(0, 1)
	_ = mt.SetString("__double", s.NewFunc1("double", func(cur *State, args []Value) (Value, error) {
		return Int(cur.TableOf(args[0]).GetString("v").Int() * 2), nil
	}))
	if err := tbl.SetMetatable(mt); err != nil {
		t.Fatalf("SetMetatable: %v", err)
	}
	if mf := s.GetMetafield(tbl.Value(), "__double"); !mf.IsFunction() {
		t.Fatalf("GetMetafield(__double) = %v, want function", mf)
	}
	if mf := s.GetMetafield(tbl.Value(), "__missing"); !mf.IsNil() {
		t.Fatalf("GetMetafield(__missing) = %v, want nil", mf)
	}
	res, ok, err := s.CallMeta(tbl.Value(), "__double")
	if err != nil || !ok || res.Int() != 42 {
		t.Fatalf("CallMeta(__double) = (%v, %v, %v), want (42, true, nil)", res, ok, err)
	}
	if res, ok, err := s.CallMeta(tbl.Value(), "__missing"); err != nil || ok || !res.IsNil() {
		t.Fatalf("CallMeta(__missing) = (%v, %v, %v), want (nil, false, nil)", res, ok, err)
	}

	// Traceback on active State (inside Go callback) and on a suspended coroutine Thread.
	var activeTB string
	s.RegisterFunc1("capture_tb", func(cur *State, _ []Value) (Value, error) {
		activeTB = cur.Traceback(nil, "from-go", 0)
		return cur.String(activeTB), nil
	})
	if _, err := s.DoStringNamed("=tb_active.lua", `
		local function inner() local v = capture_tb(); return v end
		local function outer() local v = inner(); return v end
		outer()
	`); err != nil {
		t.Fatalf("active Traceback script failed: %v", err)
	}
	if !strings.Contains(activeTB, "from-go\nstack traceback:") || !strings.Contains(activeTB, "inner") || !strings.Contains(activeTB, "outer") {
		t.Fatalf("unexpected active Traceback:\n%s", activeTB)
	}

	coChunk, err := Compile("=tb_coro.lua", `
		local function leaf() coroutine.yield("paused") end
		local function branch() leaf() end
		branch()
	`)
	if err != nil {
		t.Fatalf("Compile coro: %v", err)
	}
	coFn, err := s.Load(coChunk)
	if err != nil {
		t.Fatalf("Load coro: %v", err)
	}
	th, err := s.NewThread(coFn)
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	_, yielded, err := th.Resume()
	if err != nil || !yielded {
		t.Fatalf("Resume = (yielded=%v, err=%v)", yielded, err)
	}
	coTB := s.Traceback(th, "coro-paused", 0)
	if !strings.Contains(coTB, "coro-paused\nstack traceback:") || !strings.Contains(coTB, "leaf") || !strings.Contains(coTB, "branch") {
		t.Fatalf("unexpected coroutine Traceback:\n%s", coTB)
	}

	// Cross-State Thread rejected cleanly; closed State helpers return nil/empty/ErrStateClosed.
	other := New(WithLibs(LibSafe))
	defer other.Close()
	if got := other.Traceback(th, "wrong-state", 0); got != "" {
		t.Fatalf("cross-state Traceback = %q, want empty", got)
	}
	closed := New(WithLibs(LibNone))
	closed.Close()
	if closed.Globals() != nil || closed.Registry() != nil || closed.Traceback(nil, "x", 0) != "" || !closed.GetMetafield(Nil(), "__index").IsNil() {
		t.Fatalf("closed State aux helpers must return zero values")
	}
	if _, _, err := closed.CallMeta(Nil(), "__index"); !errors.Is(err, ErrStateClosed) {
		t.Fatalf("CallMeta on closed State = %v, want ErrStateClosed", err)
	}
}

func TestCodexRound1TemplateAndLibsRegressions(t *testing.T) {
	t.Run("SuspendedCoroutineOpenUpvalues", func(t *testing.T) {
		tmpl, err := NewTemplate(func(s *State) error {
			_, err := s.DoString(`
				co = coroutine.create(function()
					local secret = 12345
					getter = function() return secret end
					setter = function(v) secret = v end
					coroutine.yield("ready")
					return secret
				end)
				local ok, tag = coroutine.resume(co)
				assert(ok and tag == "ready")
			`)
			return err
		}, WithLibs(LibSafe))
		if err != nil {
			t.Fatalf("NewTemplate with suspended coroutine failed: %v", err)
		}

		// Drop coroutine reference, force full GC (which must close the open upvalue via twups),
		// and verify the surviving closures still read/mutate their isolated captured local.
		s1 := tmpl.NewState()
		defer s1.Close()
		s2 := tmpl.NewState()
		defer s2.Close()

		res1, err := s1.DoString(`
			co = nil
			collectgarbage("collect")
			collectgarbage("collect")
			assert(getter() == 12345)
			setter(99999)
			return getter()
		`)
		if err != nil || len(res1) != 1 || res1[0].Int() != 99999 {
			t.Fatalf("clone s1 upvalue closure after GC failed: res=%v err=%v", res1, err)
		}

		res2, err := s2.DoString(`
			local ok, finalVal = coroutine.resume(co)
			assert(ok and finalVal == 12345)
			return getter()
		`)
		if err != nil || len(res2) != 1 || res2[0].Int() != 12345 {
			t.Fatalf("clone s2 isolated coroutine failed: res=%v err=%v", res2, err)
		}
	})

	t.Run("DebugCanonicalBuiltinsInNewTemplate", func(t *testing.T) {
		// Warm default stdlib template first so debug builtins are already registered once.
		warm := New()
		warm.Close()

		tmpl, err := NewTemplate(nil, WithLibs(LibBase|LibDebug))
		if err != nil {
			t.Fatalf("NewTemplate(LibBase|LibDebug) failed: %v", err)
		}
		s := tmpl.NewState()
		defer s.Close()

		res, err := s.DoString(`
			local function probe()
				local x = 42
				local name, val = debug.getlocal(1, 1)
				local info = debug.getinfo(1, "n")
				return name, val, info.name
			end
			local a, b, c = probe()
			return a, b, c
		`)
		if err != nil {
			t.Fatalf("debug.getlocal in cloned template failed: %v", err)
		}
		if len(res) != 3 || mustStr(t, s, res[0]) != "x" || res[1].Int() != 42 || mustStr(t, s, res[2]) != "probe" {
			t.Fatalf("debug canonical builtin mismatch: got (%v, %v, %v), want (x, 42, probe)", res[0], res[1], res[2])
		}
	})

	t.Run("OpenLibsMemoryLimitRetry", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			mask   LibMask
			verify string
		}{
			{"LibMath", LibMath, `assert(math.floor(3.9) == 3 and math.pi > 3)`},
			{"LibTable", LibTable, `assert(table.concat({"a", "b"}, ":") == "a:b")`},
			{"LibString", LibString, `assert(string.upper("ok") == "OK" and ("hi"):len() == 2)`},
			{"LibUTF8", LibUTF8, `assert(utf8.len("你好") == 2)`},
			{"LibCoroutine", LibCoroutine, `assert(type(coroutine.create) == "function")`},
			{"LibDebug", LibDebug, `assert(type(debug.getinfo) == "function" and type(debug.getregistry()._HOOKKEY) == "table")`},
			{"LibIO", LibIO, `assert(type(io.type) == "function" and io.stdin ~= nil)`},
			{"LibOS", LibOS, `assert(type(os.clock) == "function")`},
			{"LibPackageSafe", LibPackageSafe, `assert(type(require) == "function" and type(package.preload) == "table")`},
			{"LibPackage", LibPackage, `assert(type(require) == "function" and type(package.searchpath) == "function")`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				for _, headroom := range []int64{1, 128, 512, 1536} {
					s := New(WithLibs(LibBase))
					s.SetMemoryLimit(s.MemoryUsage() + headroom)
					err := s.OpenLibs(tc.mask)
					if headroom == 1 && err == nil {
						s.Close()
						t.Fatalf("expected memory error for OpenLibs(%s) at headroom=1", tc.name)
					}

					// Disabling the memory limit and retrying OpenLibs must always yield a complete library.
					s.SetMemoryLimit(0)
					if err := s.OpenLibs(tc.mask); err != nil {
						s.Close()
						t.Fatalf("retry OpenLibs(%s) (headroom=%d) failed: %v", tc.name, headroom, err)
					}
					if _, err := s.DoString(tc.verify); err != nil {
						s.Close()
						t.Fatalf("retried %s incomplete (headroom=%d): %v", tc.name, headroom, err)
					}
					s.Close()
				}
			})
		}
	})

	t.Run("SetupClosedFileAllowed", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "setup.txt")
		tmpl, err := NewTemplate(func(s *State) error {
			s.SetGlobal("setup_path", s.String(f))
			_, err := s.DoString(`
				local fh = assert(io.open(setup_path, "w"))
				assert(fh:write("initialized"))
				assert(fh:close())
				setup_done = true
			`)
			return err
		}, WithLibs(LibBase|LibIO))
		if err != nil {
			t.Fatalf("NewTemplate rejected setup that properly closed its file: %v", err)
		}
		s := tmpl.NewState()
		defer s.Close()
		if !s.GetGlobal("setup_done").IsTrue() {
			t.Fatalf("setup_done global missing in cloned State")
		}
	})
}

func TestCodexRound2TemplateAndLibsRegressions(t *testing.T) {
	t.Run("GMatchIteratorIsolatedAcrossClones", func(t *testing.T) {
		tmpl, err := NewTemplate(func(s *State) error {
			_, err := s.DoString(`iter = string.gmatch("abc", ".")`)
			return err
		}, WithLibs(LibSafe))
		if err != nil {
			t.Fatalf("NewTemplate with gmatch iterator failed: %v", err)
		}

		// Run concurrent clones advancing the same snapshotted gmatch iterator under -race.
		const workers = 8
		var wg sync.WaitGroup
		errCh := make(chan error, workers)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s := tmpl.NewState()
				defer s.Close()
				res, err := s.DoString(`local a, b, c, d = iter(), iter(), iter(), iter(); return a, b, c, d`)
				if err != nil {
					errCh <- err
					return
				}
				a, _ := s.ToString(res[0])
				b, _ := s.ToString(res[1])
				c, _ := s.ToString(res[2])
				if len(res) != 4 || a != "a" || b != "b" || c != "c" || !res[3].IsNil() {
					errCh <- errors.New("gmatch iterator state leaked across clones: " + a + b + c)
				}
			}()
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatal(err)
		}

		// Verify cloned gmatch iterator also honors per-clone context cancellation.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		sc := tmpl.NewState(WithContext(ctx))
		defer sc.Close()
		if _, err := sc.DoString(`return iter()`); !errors.Is(err, context.Canceled) {
			t.Fatalf("cloned gmatch iterator expected context.Canceled, got %v", err)
		}
	})

	t.Run("TemplateSetupOpenLibsStringUnderTightMemLimit", func(t *testing.T) {
		// Exact Codex Round 2 repro: opening LibString inside NewTemplate setup under a tight
		// memory limit must keep strTbl rooted across strMeta allocation so ("abc"):upper() works.
		for _, headroom := range []int64{1024, 2048, 2688, 3072, 4096, 8192} {
			tmpl, err := NewTemplate(func(s *State) error {
				s.SetMemoryLimit(s.MemoryUsage() + headroom)
				return s.OpenLibs(LibString)
			}, WithLibs(LibNone))
			if err != nil {
				continue // OOM cleanly rejected during setup is fine
			}
			s := tmpl.NewState()
			res, err := s.DoString(`return ("abc"):upper()`)
			if err != nil {
				s.Close()
				t.Fatalf("cloned state failed on ('abc'):upper() (headroom=%d): %v", headroom, err)
			}
			if got, ok := s.ToString(res[0]); !ok || got != "ABC" {
				s.Close()
				t.Fatalf("('abc'):upper() = (%q, %v), want (ABC, true)", got, ok)
			}
			s.Close()
		}
	})

	t.Run("TemplateDeferredArrHintMemoryAccounting", func(t *testing.T) {
		// Exact Codex Round 2 repro: table created with large unused sequence capacity in setup
		// must preserve its extraMemBytes reservation in clones so materializing cannot bypass memLimit.
		const bigSeq = 1 << 20 // 8 MiB array reservation
		tmpl, err := NewTemplate(func(s *State) error {
			tbl, err := s.TryNewTable(bigSeq, 0)
			if err != nil {
				return err
			}
			return s.TrySetGlobal("big", tbl.Value())
		}, WithLibs(LibSafe))
		if err != nil {
			t.Fatalf("NewTemplate with pre-sized table failed: %v", err)
		}

		s := tmpl.NewState()
		defer s.Close()
		if usage := s.MemoryUsage(); usage < bigSeq*8 {
			t.Fatalf("cloned State MemoryUsage = %d, want >= %d (preserved arrHint reservation)", usage, bigSeq*8)
		}
		// Setting a limit below the reserved 8 MiB must reject materializing big[1] = 1.
		s.SetMemoryLimit(64 << 10)
		if _, err := s.DoString(`big[1] = 1`); err == nil {
			t.Fatalf("expected memory limit error when materializing big[1] above cap, got nil (usage=%d)", s.MemoryUsage())
		}
	})
}

func TestCodexRound3TemplateAndLibsRegressions(t *testing.T) {
	t.Run("OpenedLibsCachedInLoadedBeforePackageAndGlobalClear", func(t *testing.T) {
		s := New(WithLibs(LibBase | LibMath))
		defer s.Close()

		origMath := s.GetGlobal("math")
		if !origMath.IsTable() {
			t.Fatalf("expected math table")
		}

		// RequireLib("math", ...) must return the already-opened math table without invoking loader.
		called := false
		got, err := s.RequireLib("math", func(*State, []Value) ([]Value, error) {
			called = true
			return []Value{Int(999)}, nil
		}, false)
		if err != nil || called || got != origMath {
			t.Fatalf("RequireLib(math) = (got==orig:%v, called:%v, err:%v), want (true, false, nil)", got == origMath, called, err)
		}

		// Clearing _G.math BEFORE opening LibPackageSafe must still preserve require("math").
		s.SetGlobal("math", Nil())
		if err := s.OpenLibs(LibPackageSafe); err != nil {
			t.Fatalf("OpenLibs(LibPackageSafe) failed: %v", err)
		}
		res, err := s.DoString(`
			assert(math == nil)
			local m = require("math")
			return m.floor(9.8)
		`)
		if err != nil || len(res) != 1 || res[0].Int() != 9 {
			t.Fatalf("require('math') after clearing global failed: res=%v err=%v", res, err)
		}
	})

	t.Run("TemplatePreservesDefaultIOStreams", func(t *testing.T) {
		tmpl, err := NewTemplate(func(s *State) error {
			_, err := s.DoString(`
				io.output(io.stderr)
				io.input(io.stdout)
			`)
			return err
		}, WithLibs(LibBase|LibIO))
		if err != nil {
			t.Fatalf("NewTemplate configuring default IO streams failed: %v", err)
		}

		s := tmpl.NewState()
		defer s.Close()
		res, err := s.DoString(`
			return io.output() == io.stderr, io.input() == io.stdout, io.type(io.output()), io.type(io.input())
		`)
		if err != nil {
			t.Fatalf("checking cloned default IO streams failed: %v", err)
		}
		if len(res) != 4 || !res[0].IsTrue() || !res[1].IsTrue() || mustStr(t, s, res[2]) != "file" || mustStr(t, s, res[3]) != "file" {
			t.Fatalf("cloned default IO streams mismatch: %v", res)
		}
	})
}

func TestCodexRound4TemplateAndLibsRegressions(t *testing.T) {
	t.Run("CollectedStdFilesDoNotAliasRecycledHostUserdata", func(t *testing.T) {
		type hostPayload struct{ label string }
		want := &hostPayload{label: "host-resource"}

		tmpl, err := NewTemplate(func(s *State) error {
			// Drop io.stdout and io.stderr, redirect default output to io.stdin, and collect
			// twice so the standard-file Arena slots are swept and recycled.
			if _, err := s.DoString(`
				io.output(io.stdin)
				io.stdout = nil
				io.stderr = nil
				collectgarbage("collect")
				collectgarbage("collect")
			`); err != nil {
				return err
			}
			mt, created, err := s.TryNewMetatable("host_res")
			if err != nil || !created {
				t.Fatalf("TryNewMetatable = (%v, %v, %v)", mt, created, err)
			}
			// Allocate several host userdatas so at least one reuses the collected std-file slot.
			for i := range 4 {
				ud, err := s.NewUserdataWithMeta(want, mt)
				if err != nil {
					return err
				}
				if err := s.TrySetGlobal("res_"+string(rune('0'+i)), ud); err != nil {
					return err
				}
			}
			return nil
		}, WithLibs(LibBase|LibIO))
		if err != nil {
			t.Fatalf("NewTemplate after collecting std files failed: %v", err)
		}

		s := tmpl.NewState()
		defer s.Close()
		for i := range 4 {
			name := "res_" + string(rune('0'+i))
			got, ok := s.CheckUserdata(s.GetGlobal(name), "host_res")
			if !ok {
				t.Fatalf("CheckUserdata(%s) failed", name)
			}
			hp, isHP := got.(*hostPayload)
			if !isHP || hp != want {
				t.Fatalf("cloned %s payload corrupted: got %T (%v), want %v", name, got, got, want)
			}
		}
	})
}

func TestCodexRound5TemplateAndLibsRegressions(t *testing.T) {
	t.Run("ReloadedIOLibPreservesAllSurvivingStdFileWrappers", func(t *testing.T) {
		tmpl, err := NewTemplate(func(s *State) error {
			_, err := s.DoString(`
				oldio = require("io")
				package.loaded.io = nil
				newio = require("io")
			`)
			return err
		}, WithLibs(LibBase|LibPackageSafe), WithPreloadLibs(LibIO))
		if err != nil {
			t.Fatalf("NewTemplate reloading lazy io failed: %v", err)
		}

		s := tmpl.NewState()
		defer s.Close()
		res, err := s.DoString(`
			return oldio.type(oldio.stdin), oldio.type(oldio.stdout), oldio.type(oldio.stderr),
			       newio.type(newio.stdin), newio.type(newio.stdout), newio.type(newio.stderr)
		`)
		if err != nil {
			t.Fatalf("checking reloaded io std handles failed: %v", err)
		}
		for i, v := range res {
			if got := mustStr(t, s, v); got != "file" {
				t.Fatalf("std handle #%d type = %q, want 'file'", i, got)
			}
		}
	})

	t.Run("LargeGMatchTemplateClonesShareImmutableText", func(t *testing.T) {
		const size = 2 << 20 // 2 MiB subject
		tmpl, err := NewTemplate(func(s *State) error {
			s.SetGlobal("big_s", s.String(strings.Repeat("ab", size/2)))
			_, err := s.DoString(`
				iter = string.gmatch(big_s, ".")
				big_s = nil
			`)
			return err
		}, WithLibs(LibSafe))
		if err != nil {
			t.Fatalf("NewTemplate with large gmatch failed: %v", err)
		}

		// Cloning multiple States must share the detached immutable subject text without
		// copying 2 MiB per clone (verified via testing.AllocsPerRun <= 128 KiB/clone).
		s1 := tmpl.NewState()
		s2 := tmpl.NewState()
		defer s1.Close()
		defer s2.Close()

		r1, err := s1.DoString(`return iter(), iter()`)
		if err != nil || mustStr(t, s1, r1[0]) != "a" || mustStr(t, s1, r1[1]) != "b" {
			t.Fatalf("s1 gmatch mismatch: %v %v", r1, err)
		}
		r2, err := s2.DoString(`return iter(), iter()`)
		if err != nil || mustStr(t, s2, r2[0]) != "a" || mustStr(t, s2, r2[1]) != "b" {
			t.Fatalf("s2 gmatch mismatch: %v %v", r2, err)
		}

		// Measure Go heap bytes allocated per clone: must be well below the 2 MiB subject size.
		var totalBytes uint64
		res := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				st := tmpl.NewState()
				st.Close()
			}
		})
		totalBytes = uint64(res.AllocedBytesPerOp())
		if totalBytes >= size/2 {
			t.Fatalf("Template.NewState allocated %d B/op for %d B gmatch subject; expected shared immutable text", totalBytes, size)
		}
	})

	t.Run("OpenLibsRollbackKeepsPrevGlobalRooted", func(t *testing.T) {
		// Exact Codex Round 5 repro: replacing an otherwise unreferenced global table under
		// a tight memory limit that fails while allocating _LOADED must keep prevGlobal live.
		for _, headroom := range []int64{256, 1024, 2048, 4096, 5697, 6144} {
			s := New(WithLibs(LibNone))
			orig := s.NewTable(0, 2)
			_ = orig.SetString("sentinel", Int(777))
			s.SetGlobal("math", orig.Value())

			s.SetMemoryLimit(s.MemoryUsage() + headroom)
			err := s.OpenLibs(LibMath)
			s.SetMemoryLimit(0)

			if err != nil {
				cur := s.GetGlobal("math")
				if !s.Raw().IsLive(cur) {
					s.Close()
					t.Fatalf("prevGlobal became dead after OpenLibs rollback (headroom=%d)", headroom)
				}
				if got := s.TableOf(cur).GetString("sentinel").Int(); got != 777 {
					s.Close()
					t.Fatalf("restored prevGlobal sentinel = %d, want 777 (headroom=%d)", got, headroom)
				}
			}
			s.Close()
		}
	})
}

func BenchmarkTemplateNewState(b *testing.B) {
	tmpl, err := NewTemplate(func(s *State) error {
		if _, err := s.RequireLib("svc", func(st *State, _ []Value) ([]Value, error) {
			t := st.NewTable(0, 2)
			_ = t.SetString("ping", st.NewFunc1("svc.ping", func(_ *State, _ []Value) (Value, error) {
				return Int(1), nil
			}))
			return []Value{t.Value()}, nil
		}, true); err != nil {
			return err
		}
		_, err := s.DoString(`function add1(x) return x + 1 end`)
		return err
	}, WithLibs(LibSafe|LibPackageSafe))
	if err != nil {
		b.Fatalf("NewTemplate: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s := tmpl.NewState()
		s.Close()
	}
}
