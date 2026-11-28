package moelua_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
	"weak"

	"github.com/yuchanns/moelua"
)

// TestHostHookYieldCountInterleavedCoroutines mirrors PUC-Rio third_party/lua/testes/coroutine.lua:597-627.
func TestHostHookYieldCountInterleavedCoroutines(t *testing.T) {
	for _, useJIT := range []bool{false, true} {
		name := "noJIT"
		if useJIT {
			name = "JIT"
		}
		t.Run(name, func(t *testing.T) {
			var opts []moelua.Option
			if useJIT {
				opts = append(opts, moelua.WithJIT())
			}
			s := moelua.New(opts...)
			defer s.Close()

			s.RegisterFunc("set_yield_hook", func(st *moelua.State, args []moelua.Value) ([]moelua.Value, error) {
				count := int(args[0].Int())
				st.SetHostHook(func(_ *moelua.State, _ moelua.HookEvent, _ int) error {
					return moelua.ErrHookYield
				}, moelua.HookMaskCount, count)
				return nil, nil
			})

			res, err := s.DoString(`
				local turn
				local function fact(t, x)
					assert(turn == t)
					if x == 0 then return 1
					else return x * fact(t, x - 1)
					end
				end

				local A, B = 0, 0
				local x = coroutine.create(function()
					set_yield_hook(2)
					A = fact("A", 6)
				end)
				local y = coroutine.create(function()
					set_yield_hook(3)
					B = fact("B", 7)
				end)

				local steps = 0
				while A == 0 or B == 0 do
					steps = steps + 1
					if A == 0 then
						turn = "A"
						local ok, err = coroutine.resume(x)
						assert(ok, err)
					end
					if B == 0 then
						turn = "B"
						local ok, err = coroutine.resume(y)
						assert(ok, err)
					end
					local tbx = debug.traceback(x)
					local tby = debug.traceback(y)
					assert(not tbx:find("in field 'yield'", 1, true), tbx)
					assert(not tby:find("in field 'yield'", 1, true), tby)
				end
				assert(B // A == 7)
				return A, B, steps
			`)
			if err != nil {
				t.Fatalf("interleaved count hook yield failed: %v", err)
			}
			if len(res) != 3 || res[0].Int() != 720 || res[1].Int() != 5040 || res[2].Int() < 10 {
				t.Fatalf("unexpected results: %v", res)
			}
		})
	}
}

// TestHostHookYieldMultretAndPerturbedStack mirrors PUC-Rio third_party/lua/testes/coroutine.lua:629-649.
func TestHostHookYieldMultretAndPerturbedStack(t *testing.T) {
	s := moelua.New()
	defer s.Close()

	perturbFn, err := s.DoString(`return function(x) local y = x + 1; return y, y + 1, y + 2 end`)
	if err != nil {
		t.Fatal(err)
	}
	perturbPin := s.Pin(perturbFn[0])
	defer perturbPin.Release()

	s.RegisterFunc("set_step1_hook", func(st *moelua.State, _ []moelua.Value) ([]moelua.Value, error) {
		st.SetHostHook(func(hs *moelua.State, _ moelua.HookEvent, _ int) error {
			// Call a Lua function from inside the host hook to actively perturb the stack
			// and verify th.top is preserved across MULTRET instructions like {table.unpack(a)}.
			if _, cerr := hs.Call(perturbPin.Value(), moelua.Int(10)); cerr != nil {
				return cerr
			}
			return moelua.ErrHookYield
		}, moelua.HookMaskCount, 1)
		return nil, nil
	})

	_, err = s.DoString(`
		local done
		local function test(n)
			done = false
			return coroutine.wrap(function()
				local a = {}
				for i = 1, n do a[i] = i end
				set_step1_hook()
				local a1 = {table.unpack(a)}
				assert(#a1 == n, "expected " .. n .. " got " .. #a1)
				for i = 1, n do assert(a1[i] == i) end
				done = true
			end)
		end
		local co = test(0); while not done do co(30) end
		co = test(1); while not done do co(20, 10) end
		co = test(3); while not done do co() end
		co = test(100); while not done do co("ignored", 1, 2, 3) end
	`)
	if err != nil {
		t.Fatalf("multret stack preservation under count=1 hook yield failed: %v", err)
	}
}

// TestHostHookYieldLineByLineAndCount mirrors PUC-Rio third_party/lua/testes/coroutine.lua:651-686.
func TestHostHookYieldLineByLineAndCount(t *testing.T) {
	s := moelua.New()
	defer s.Close()

	s.RegisterFunc("set_line_hook", func(st *moelua.State, _ []moelua.Value) ([]moelua.Value, error) {
		st.SetHostHook(func(hs *moelua.State, ev moelua.HookEvent, line int) error {
			if ev == moelua.HookLine {
				hs.SetGlobal("X", moelua.Int(int64(line)))
				return moelua.ErrHookYield
			}
			return nil
		}, moelua.HookMaskLine, 0)
		return nil, nil
	})
	s.RegisterFunc("set_count_hook", func(st *moelua.State, args []moelua.Value) ([]moelua.Value, error) {
		n := int(args[0].Int())
		st.SetHostHook(func(_ *moelua.State, ev moelua.HookEvent, _ int) error {
			if ev == moelua.HookCount {
				return moelua.ErrHookYield
			}
			return nil
		}, moelua.HookMaskCount, n)
		return nil, nil
	})

	_, err := s.DoString(`
		local line = debug.getinfo(1, "l").currentline + 2
		local function foo()
			local x = 10
			x = x + 10
			_G.XX = x
		end

		-- Line hook yielding before each line executes.
		local co = coroutine.wrap(function()
			set_line_hook(); foo(); return 10
		end)
		_G.XX = nil
		_G.X = nil; assert(co() == nil); assert(_G.X == line, "got " .. tostring(_G.X) .. " want " .. line)
		_G.X = nil; assert(co() == nil); assert(_G.X == line + 1)
		_G.X = nil; assert(co() == nil); assert(_G.X == line + 2 and _G.XX == nil)
		_G.X = nil; assert(co() == nil); assert(_G.X == line + 3 and _G.XX == 20)
		assert(co() == 10)

		-- Count=1 hook.
		co = coroutine.wrap(function()
			set_count_hook(1); foo(); return 10
		end)
		_G.XX = nil
		local c = 0
		repeat c = c + 1; local a = co() until a == 10
		assert(_G.XX == 20 and c >= 5)

		-- Count=2 hook.
		co = coroutine.wrap(function()
			set_count_hook(2); foo(); return 10
		end)
		_G.XX = nil
		c = 0
		repeat c = c + 1; local a = co() until a == 10
		assert(_G.XX == 20 and c >= 5)
	`)
	if err != nil {
		t.Fatalf("line/count hook stepping failed: %v", err)
	}
}

// TestHostHookYieldDebugInspectSuspendedCoroutine mirrors PUC-Rio third_party/lua/testes/coroutine.lua:688-710.
func TestHostHookYieldDebugInspectSuspendedCoroutine(t *testing.T) {
	s := moelua.New()
	defer s.Close()

	s.RegisterFunc("set_line_yield", func(st *moelua.State, _ []moelua.Value) ([]moelua.Value, error) {
		st.SetHostHook(func(_ *moelua.State, ev moelua.HookEvent, _ int) error {
			if ev == moelua.HookLine {
				return moelua.ErrHookYield
			}
			return nil
		}, moelua.HookMaskLine, 0)
		return nil, nil
	})

	_, err := s.DoString(`
		local c = coroutine.create(function(a, ...)
			set_line_yield()
			local b = a
			return ...
		end)

		local ok = coroutine.resume(c, 1, 2, 3)
		assert(ok)
		local n, v = debug.getlocal(c, 0, 1)
		assert(n == "a" and v == 1, "local 1: " .. tostring(n) .. "=" .. tostring(v))
		assert(debug.getlocal(c, 0, 2) ~= "b")
		assert(debug.setlocal(c, 0, 1, 10))
		local info = debug.getinfo(c, 0)
		assert(info.currentline == info.linedefined + 2, "line=" .. tostring(info.currentline) .. " def=" .. tostring(info.linedefined))
		assert(not debug.getinfo(c, 1), "expected no synthetic yield frame above level 0")

		assert(coroutine.resume(c))
		local n2, v2 = debug.getlocal(c, 0, 2)
		assert(n2 == "(vararg table)" and v2 == nil, "local 2: " .. tostring(n2) .. "=" .. tostring(v2))
		local n3, v3 = debug.getlocal(c, 0, 3)
		assert(n3 == "b" and v3 == 10, "local 3: " .. tostring(n3) .. "=" .. tostring(v3))

		local r = {coroutine.resume(c)}
		assert(r[1] == true and r[2] == 2 and r[3] == 3 and r[4] == nil)
	`)
	if err != nil {
		t.Fatalf("debug inspection on hook-suspended coroutine failed: %v", err)
	}
}

// TestHostHookRemoteDebuggerPatternAndPCallYield verifies calling a Lua callback from
// inside a Go host hook without adding an extra frame (so debug.getinfo(2)/getlocal(2)
// inspect the interrupted frame directly), yielding through pcall/xpcall/metamethods,
// disabling hooks while suspended, and closing a hook-suspended coroutine with <close>.
func TestHostHookRemoteDebuggerPatternAndPCallYield(t *testing.T) {
	for _, useJIT := range []bool{false, true} {
		name := "noJIT"
		if useJIT {
			name = "JIT"
		}
		t.Run(name, func(t *testing.T) {
			var opts []moelua.Option
			if useJIT {
				opts = append(opts, moelua.WithJIT())
			}
			s := moelua.New(opts...)
			defer s.Close()

			var cbHandle *moelua.Handle
			defer func() {
				if cbHandle != nil {
					cbHandle.Release()
				}
			}()

			s.RegisterFunc("install_debugger_hook", func(st *moelua.State, args []moelua.Value) ([]moelua.Value, error) {
				if cbHandle != nil {
					cbHandle.Release()
					cbHandle = nil
				}
				if len(args) == 0 || args[0].IsNil() {
					st.SetHostHook(nil, 0, 0)
					return nil, nil
				}
				cbHandle = st.Pin(args[0])
				st.SetHostHook(func(hs *moelua.State, ev moelua.HookEvent, line int) error {
					res, cerr := hs.Call1(cbHandle.Value(), hs.String(ev.String()), moelua.Int(int64(line)))
					if cerr != nil {
						return cerr
					}
					if res.IsTrue() {
						return moelua.ErrHookYield
					}
					return nil
				}, moelua.HookMaskLine, 0)
				return nil, nil
			})

			// 1. Direct frame inspection (level 2 == interrupted Lua frame) + yield inside pcall & __add.
			fn, err := s.DoString(`
				local mt = {
					__add = function(a, b)
						local sum = a.v + b.v
						return sum * 2
					end
				}
				return function()
					local target = 21
					local ok, ans = pcall(function()
						local a = setmetatable({v = target}, mt)
						local b = setmetatable({v = 9}, mt)
						return a + b
					end)
					return ok, ans, target
				end
			`)
			if err != nil {
				t.Fatal(err)
			}
			co, err := s.NewThread(fn[0])
			if err != nil {
				t.Fatal(err)
			}
			coPin := s.Pin(co.Value())
			defer coPin.Release()

			var seenLines []int
			pausedAtAdd := false
			cb, err := s.DoString(`
				return function(ev, line)
					local info = debug.getinfo(2, "nlS")
					-- Look for local 'sum' inside __add and mutate it from 30 -> 50, then pause.
					for i = 1, 8 do
						local k, v = debug.getlocal(2, i)
						if not k then break end
						if k == "sum" and v == 30 then
							debug.setlocal(2, i, 50)
							_G.CAPTURED_LINE = info.currentline
							return true
						end
					end
					return false
				end
			`)
			if err != nil {
				t.Fatal(err)
			}
			cbPin := s.Pin(cb[0])
			defer cbPin.Release()

			co.SetHostHook(func(hs *moelua.State, ev moelua.HookEvent, line int) error {
				seenLines = append(seenLines, line)
				res, cerr := hs.Call1(cbPin.Value(), hs.String(ev.String()), moelua.Int(int64(line)))
				if cerr != nil {
					return cerr
				}
				if res.IsTrue() {
					pausedAtAdd = true
					return moelua.ErrHookYield
				}
				return nil
			}, moelua.HookMaskLine, 0)

			// First resume must pause inside __add (inside pcall) with 0 yielded values.
			res, yielded, err := co.Resume()
			if err != nil {
				t.Fatalf("first resume error: %v", err)
			}
			if !yielded || len(res) != 0 || !pausedAtAdd {
				t.Fatalf("expected hook yield inside __add: yielded=%v res=%v paused=%v lines=%v", yielded, res, pausedAtAdd, seenLines)
			}
			if co.StatusString() != "suspended" {
				t.Fatalf("expected suspended status, got %q", co.StatusString())
			}
			if s.GetGlobal("CAPTURED_LINE").Int() <= 0 {
				t.Fatalf("expected debug.getinfo(2) currentline > 0, got %v", s.GetGlobal("CAPTURED_LINE"))
			}

			// Disable hook while suspended, then resume to completion (must NOT stall or re-fire).
			co.SetHostHook(nil, 0, 0)
			res, yielded, err = co.Resume(moelua.Int(999)) // extra arg must be discarded
			if err != nil {
				t.Fatalf("second resume error: %v", err)
			}
			if yielded {
				t.Fatalf("expected coroutine to finish after clearing hook")
			}
			// Mutated sum = 50 -> sum * 2 = 100.
			if len(res) != 3 || !res[0].IsTrue() || res[1].Int() != 100 || res[2].Int() != 21 {
				t.Fatalf("unexpected completion values: %v", res)
			}

			// 2. Closing a hook-suspended coroutine executes <close> variables cleanly.
			closeFn, err := s.DoString(`
				CLOSED = false
				INITED = false
				return function()
					local _ <close> = setmetatable({}, {
						__close = function() CLOSED = true end
					})
					INITED = true
					local x = 1
					local y = x + 1
					return y
				end
			`)
			if err != nil {
				t.Fatal(err)
			}
			co2, err := s.NewThread(closeFn[0])
			if err != nil {
				t.Fatal(err)
			}
			co2.SetHostHook(func(_ *moelua.State, ev moelua.HookEvent, _ int) error {
				if ev == moelua.HookLine {
					return moelua.ErrHookYield
				}
				return nil
			}, moelua.HookMaskLine, 0)

			// Step line-by-line until `<close>` has been initialized (INITED == true), then close while hook-suspended.
			for range 10 {
				_, y, rerr := co2.Resume()
				if rerr != nil || !y {
					t.Fatalf("expected suspended step before close: y=%v err=%v", y, rerr)
				}
				if s.GetGlobal("INITED").IsTrue() {
					break
				}
			}
			if !s.GetGlobal("INITED").IsTrue() {
				t.Fatalf("coroutine did not reach INITED=true before closing")
			}
			// Clear the debugger hook before closing so __close runs cleanly without yielding.
			co2.SetHostHook(nil, 0, 0)
			if err := co2.Close(); err != nil {
				t.Fatalf("co2.Close failed: %v", err)
			}
			if co2.StatusString() != "dead" || !s.GetGlobal("CLOSED").IsTrue() {
				t.Fatalf("expected dead coroutine with CLOSED=true, got status=%q CLOSED=%v",
					co2.StatusString(), s.GetGlobal("CLOSED"))
			}

			// 3. Closing a hook-suspended coroutine while a yielding hook remains active rejects
			// yielding inside __close (matching PUC-Rio lua_closethread -> luaD_callnoyield).
			co3, err := s.NewThread(closeFn[0])
			if err != nil {
				t.Fatal(err)
			}
			s.SetGlobal("INITED", moelua.Bool(false))
			co3.SetHostHook(func(_ *moelua.State, ev moelua.HookEvent, _ int) error {
				if ev == moelua.HookLine {
					return moelua.ErrHookYield
				}
				return nil
			}, moelua.HookMaskLine, 0)
			for range 10 {
				_, y, rerr := co3.Resume()
				if rerr != nil || !y {
					t.Fatalf("expected suspended step before close: y=%v err=%v", y, rerr)
				}
				if s.GetGlobal("INITED").IsTrue() {
					break
				}
			}
			if cerr := co3.Close(); cerr == nil || !strings.Contains(cerr.Error(), "C-call boundary") {
				t.Fatalf("expected C-call boundary error when hook yields inside __close, got %v", cerr)
			}
		})
	}
}

// TestHostHookPUCComplianceAndLifecycle verifies PUC-Rio Lua 5.5.1 rules:
//   - Pure Lua debug.sethook still forbids coroutine.yield.
//   - ErrHookYield on call/tailcall/return hooks, main thread, or non-yieldable boundary raises Lua error.
//   - debug.gethook() returns "external hook" for host hooks; debug.sethook and SetHostHook replace each other.
//   - Context cancellation is not swallowed across hook yields.
//   - HostHookFunc closures are reclaimed by Go GC when cleared or when the coroutine is collected.
//   - NewTemplate rejects leftover active host hooks.
func TestHostHookPUCComplianceAndLifecycle(t *testing.T) {
	s := moelua.New()
	defer s.Close()

	// 1. Pure Lua debug.sethook MUST still forbid coroutine.yield (PUC-Rio spec).
	_, err := s.DoString(`
		local co = coroutine.create(function()
			debug.sethook(function() coroutine.yield() end, "l")
			local x = 1
			return x
		end)
		local ok, msg = coroutine.resume(co)
		assert(not ok, "pure Lua debug.sethook must not yield")
		assert(tostring(msg):find("attempt to yield", 1, true), tostring(msg))
	`)
	if err != nil {
		t.Fatalf("pure Lua debug.sethook yield check failed: %v", err)
	}

	// 2. debug.gethook() returns "external hook", mask, count; mutual replacement with debug.sethook.
	s.SetHostHook(func(_ *moelua.State, _ moelua.HookEvent, _ int) error { return nil },
		moelua.HookMaskCall|moelua.HookMaskLine, 5)
	res, err := s.DoString(`
		local f, m, c = debug.gethook()
		assert(f == "external hook", "got " .. tostring(f))
		assert(m == "cl", "got " .. tostring(m))
		assert(c == 5, "got " .. tostring(c))
		debug.sethook()
		local f2 = debug.gethook()
		assert(f2 == nil, "expected nil after debug.sethook()")
		return true
	`)
	if err != nil || len(res) != 1 || !res[0].IsTrue() {
		t.Fatalf("debug.gethook external hook check failed: res=%v err=%v", res, err)
	}

	// 3. ErrHookYield on main thread raises "attempt to yield from outside a coroutine".
	s.SetHostHook(func(_ *moelua.State, _ moelua.HookEvent, _ int) error {
		return moelua.ErrHookYield
	}, moelua.HookMaskLine, 0)
	_, err = s.DoString(`local x = 1; return x`)
	s.SetHostHook(nil, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "outside a coroutine") {
		t.Fatalf("expected outside-coroutine error on main thread, got: %v", err)
	}

	// 4. ErrHookYield on call/return/tailcall hooks raises C-call boundary error (catchable by pcall).
	for _, tc := range []struct {
		name string
		mask moelua.HookMask
		src  string
	}{
		{"call", moelua.HookMaskCall, `local function helper(v) return v + 1 end; return function() return pcall(helper, 41) end`},
		{"return", moelua.HookMaskRet, `local function helper(v) return v + 1 end; return function() return pcall(helper, 41) end`},
		{"tailcall", moelua.HookMaskCall, `local function leaf(v) return v + 1 end; local function mid(v) return leaf(v) end; return function() return pcall(mid, 41) end`},
	} {
		fn, err := s.DoString(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		co, err := s.NewThread(fn[0])
		if err != nil {
			t.Fatal(err)
		}
		co.SetHostHook(func(_ *moelua.State, ev moelua.HookEvent, _ int) error {
			if tc.name == "tailcall" {
				if ev == moelua.HookTailCall {
					return moelua.ErrHookYield
				}
				return nil
			}
			if ev == moelua.HookCall || ev == moelua.HookReturn || ev == moelua.HookTailCall {
				return moelua.ErrHookYield
			}
			return nil
		}, tc.mask, 0)
		r, yielded, err := co.Resume()
		if yielded {
			t.Fatalf("%s must not yield from call/return/tailcall hook", tc.name)
		}
		// Either outer coroutine errored on initial call hook, or inner pcall caught it.
		if err != nil {
			if !strings.Contains(err.Error(), "C-call boundary") {
				t.Fatalf("%s unexpected error: %v", tc.name, err)
			}
		} else if len(r) < 2 || r[0].IsTrue() || !strings.Contains(s.FormatValue(r[1]), "C-call boundary") {
			t.Fatalf("%s expected pcall to catch C-call boundary error, got %v", tc.name, r)
		}
	}

	// 5. ErrHookYield across non-yieldable Go callback boundary raises C-call boundary error.
	inInner := false
	s.RegisterFunc("call_from_go", func(st *moelua.State, args []moelua.Value) ([]moelua.Value, error) {
		inInner = true
		defer func() { inInner = false }()
		return st.Call(args[0])
	})
	fnNonYield, err := s.DoString(`
		return function()
			return call_from_go(function()
				local a = 1
				return a + 1
			end)
		end
	`)
	if err != nil {
		t.Fatal(err)
	}
	coNY, err := s.NewThread(fnNonYield[0])
	if err != nil {
		t.Fatal(err)
	}
	coNY.SetHostHook(func(_ *moelua.State, ev moelua.HookEvent, _ int) error {
		if ev == moelua.HookLine && inInner {
			return moelua.ErrHookYield
		}
		return nil
	}, moelua.HookMaskLine, 0)
	_, yielded, err := coNY.Resume()
	if yielded || err == nil || !strings.Contains(err.Error(), "C-call boundary") {
		t.Fatalf("expected C-call boundary error across Go call, got yielded=%v err=%v", yielded, err)
	}

	// 6. Context cancellation while suspended aborts on resume cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	sc := moelua.New(moelua.WithContext(ctx))
	defer sc.Close()
	fnC, err := sc.DoString(`return function() local x = 1; local y = x + 1; return y end`)
	if err != nil {
		t.Fatal(err)
	}
	coC, err := sc.NewThread(fnC[0])
	if err != nil {
		t.Fatal(err)
	}
	coC.SetHostHook(func(_ *moelua.State, ev moelua.HookEvent, _ int) error {
		if ev == moelua.HookLine {
			return moelua.ErrHookYield
		}
		return nil
	}, moelua.HookMaskLine, 0)
	if _, y, rerr := coC.Resume(); rerr != nil || !y {
		t.Fatalf("expected initial hook yield: y=%v err=%v", y, rerr)
	}
	cancel()
	if _, _, rerr := coC.Resume(); !errors.Is(rerr, context.Canceled) {
		t.Fatalf("expected context.Canceled on resume after cancel, got %v", rerr)
	}

	// 7. Go GC reclaims HostHookFunc closure payload after coroutine is Lua-GC-collected.
	type hookPayload struct{ data [64]byte }
	var wp weak.Pointer[hookPayload]
	func() {
		fnGC, err := s.DoString(`return function() return 1 end`)
		if err != nil {
			t.Fatal(err)
		}
		coGC, err := s.NewThread(fnGC[0])
		if err != nil {
			t.Fatal(err)
		}
		p := &hookPayload{}
		wp = weak.Make(p)
		coGC.SetHostHook(func(_ *moelua.State, _ moelua.HookEvent, _ int) error {
			_ = p.data[0]
			return nil
		}, moelua.HookMaskLine, 0)
	}()
	s.CollectGarbage()
	reclaimed := false
	for range 20 {
		runtime.GC()
		if wp.Value() == nil {
			reclaimed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !reclaimed {
		t.Fatalf("HostHookFunc payload was not reclaimed after coroutine GC")
	}

	// 8. Replacing a host hook from inside a line hook does not double-fire the current line.
	var rehookLines []int
	var hookB moelua.HostHookFunc
	hookB = func(_ *moelua.State, ev moelua.HookEvent, line int) error {
		if ev == moelua.HookLine {
			rehookLines = append(rehookLines, line)
		}
		return nil
	}
	s.SetHostHook(func(hs *moelua.State, ev moelua.HookEvent, line int) error {
		if ev == moelua.HookLine {
			rehookLines = append(rehookLines, line)
			hs.SetHostHook(hookB, moelua.HookMaskLine, 0)
		}
		return nil
	}, moelua.HookMaskLine, 0)
	_, err = s.DoString("local a = 1 + 2 + 3\nlocal b = a * 2\nreturn b")
	s.SetHostHook(nil, 0, 0)
	if err != nil {
		t.Fatalf("rehook inside line hook failed: %v", err)
	}
	if len(rehookLines) != 3 || rehookLines[0] != 1 || rehookLines[1] != 2 || rehookLines[2] != 3 {
		t.Fatalf("expected lines [1 2 3] after rehook, got %v", rehookLines)
	}

	// 9. NewTemplate must reject setup that leaves an active host hook (main thread or coroutine).
	if _, err := moelua.NewTemplate(func(ts *moelua.State) error {
		ts.SetHostHook(func(_ *moelua.State, _ moelua.HookEvent, _ int) error { return nil }, moelua.HookMaskLine, 0)
		return nil
	}); err == nil {
		t.Fatalf("expected NewTemplate to reject active host hook on main thread")
	}
	if _, err := moelua.NewTemplate(func(ts *moelua.State) error {
		f, derr := ts.DoString(`return function() return 1 end`)
		if derr != nil {
			return derr
		}
		c, cerr := ts.NewThread(f[0])
		if cerr != nil {
			return cerr
		}
		c.SetHostHook(func(_ *moelua.State, _ moelua.HookEvent, _ int) error { return nil }, moelua.HookMaskLine, 0)
		ts.SetGlobal("CO", c.Value())
		return nil
	}); err == nil {
		t.Fatalf("expected NewTemplate to reject active host hook on coroutine")
	}
}

// TestHostHookZeroAllocPerInvocation confirms firing a Go host hook on line/count events
// allocates zero Go heap objects per invocation.
func TestHostHookZeroAllocPerInvocation(t *testing.T) {
	s := moelua.New()
	defer s.Close()

	fn, err := s.DoString(`
		SUM = 0
		return function()
			local sum = 0
			for i = 1, 200 do
				sum = sum + i
			end
			SUM = sum
		end
	`)
	if err != nil {
		t.Fatal(err)
	}
	fnPin := s.Pin(fn[0])
	defer fnPin.Release()

	var hits int
	s.SetHostHook(func(_ *moelua.State, _ moelua.HookEvent, _ int) error {
		hits++
		return nil
	}, moelua.HookMaskLine|moelua.HookMaskCount, 10)
	defer s.SetHostHook(nil, 0, 0)

	// Warm up once.
	if _, err := s.Call1(fnPin.Value()); err != nil {
		t.Fatal(err)
	}
	if hits < 100 || s.GetGlobal("SUM").Int() != 20100 {
		t.Fatalf("expected host hook hits >= 100 and SUM=20100, got hits=%d SUM=%v", hits, s.GetGlobal("SUM"))
	}

	allocs := testing.AllocsPerRun(50, func() {
		if _, err := s.Call1(fnPin.Value()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	if allocs != 0 {
		t.Fatalf("expected 0 Go allocs/run across %d host hook invocations, got %.2f", hits, allocs)
	}
}
