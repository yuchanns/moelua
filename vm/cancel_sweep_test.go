package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestSandboxCancelAndBudgetSweep systematically verifies that cooperative context
// cancellation and instruction budgets stop long-running pure-Lua, stdlib, GC, and
// OS subprocess operations promptly without hanging or blowing past deadlines.
func TestSandboxCancelAndBudgetSweep(t *testing.T) {
	// Pure VM + stdlib operations tested under both a tight context timeout and an instruction budget.
	pureCases := []struct {
		name      string
		src       string
		insnLimit int64
	}{
		{
			name: "plain_string_find_loop",
			src: `
				local s = string.rep("a", 4 * 1024 * 1024)
				for i = 1, 10000 do
					s:find("ab", 1, true)
				end
			`,
			insnLimit: 10000,
		},
		{
			name: "nospecial_string_find_loop",
			src: `
				local s = string.rep("a", 4 * 1024 * 1024)
				for i = 1, 10000 do
					s:find("ab")
				end
			`,
			insnLimit: 10000,
		},
		{
			name: "string_format_q_large",
			src: `
				local s = string.rep("a\nb\"c", 1024 * 1024)
				for i = 1, 1000 do
					string.format("%q", s)
				end
			`,
			insnLimit: 10000,
		},
		{
			name: "vararg_table_copy_loop",
			src: `
				local args = {}
				for i = 1, 20000 do args[i] = i end
				local function sink(...)
					for i = 1, 10000 do
						local t = {...}
						if #t == 0 then break end
					end
				end
				sink(table.unpack(args))
			`,
			insnLimit: 100000,
		},
		{
			name: "named_vararg_prep_loop",
			src: `
				local args = {}
				for i = 1, 10000 do args[i] = i end
				local function mk(...t) return #t end
				for i = 1, 10000 do
					mk(table.unpack(args))
				end
			`,
			insnLimit: 100000,
		},
		{
			name: "table_move_huge",
			src: `
				local t = {1, 2, 3}
				table.move(t, 1, 1000000000, 2)
			`,
			insnLimit: 20000,
		},
		{
			name: "gc_finalizer_sweep_loop",
			src: `
				local mt = {__gc = function() end}
				while true do
					for i = 1, 50000 do
						setmetatable({}, mt)
					end
					collectgarbage()
				end
			`,
			insnLimit: 50000,
		},
		{
			name: "pcall_cannot_swallow_cancel",
			src: `
				while true do
					pcall(function()
						for i = 1, 1000000 do end
					end)
				end
			`,
			insnLimit: 10000,
		},
		{
			name: "xpcall_handler_loop",
			src: `
				xpcall(function() error("x") end, function()
					while true do end
				end)
			`,
			insnLimit: 10000,
		},
	}

	for _, tc := range pureCases {
		t.Run("ctx_"+tc.name, func(t *testing.T) {
			s := NewState()
			defer s.Close()
			cl, err := s.LoadChunk([]byte(tc.src), "="+tc.name)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
			defer cancel()
			s.SetContext(ctx)

			start := time.Now()
			_, err = s.Call(cl)
			elapsed := time.Since(start)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected context.DeadlineExceeded, got %v (elapsed %v)", err, elapsed)
			}
			if elapsed > 2*time.Second {
				t.Fatalf("cancellation overshot deadline badly: elapsed %v", elapsed)
			}
			// Stale abortErr must not poison subsequent host writes after Call returns.
			if leaked := s.ConsumeMemErr(); leaked != nil {
				t.Fatalf("stale abortErr leaked after ctx cancel: %v", leaked)
			}
		})

		t.Run("insn_"+tc.name, func(t *testing.T) {
			s := NewState()
			defer s.Close()
			cl, err := s.LoadChunk([]byte(tc.src), "="+tc.name)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			s.SetInstructionLimit(tc.insnLimit)
			_, err = s.Call(cl)
			if !errors.Is(err, ErrInstructionLimit) {
				t.Fatalf("expected ErrInstructionLimit, got %v", err)
			}
			if leaked := s.ConsumeMemErr(); leaked != nil {
				t.Fatalf("stale abortErr leaked after instruction limit: %v", leaked)
			}
		})
	}

	// Shell subprocess cancellation: os.execute and io.popen must respect State context.
	sleepCmd := "sleep 5"
	timeout := 150 * time.Millisecond
	maxElapsed := 2 * time.Second
	if runtime.GOOS == "windows" {
		sleepCmd = "ping -n 6 127.0.0.1 > NUL"
		timeout = 250 * time.Millisecond
		maxElapsed = 3 * time.Second
	}

	t.Run("ctx_os_execute", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(fmt.Sprintf(`return os.execute(%q)`, sleepCmd)), "=ctx_osexec")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		s.SetContext(ctx)

		start := time.Now()
		_, err = s.Call(cl)
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("os.execute expected context.DeadlineExceeded, got %v (elapsed %v)", err, elapsed)
		}
		if elapsed > maxElapsed {
			t.Fatalf("os.execute cancellation took too long: %v", elapsed)
		}
	})

	t.Run("ctx_io_popen_read", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(fmt.Sprintf(`
			local f = assert(io.popen(%q, "r"))
			return f:read("a")
		`, sleepCmd)), "=ctx_popen")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		s.SetContext(ctx)

		start := time.Now()
		_, err = s.Call(cl)
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("io.popen:read expected context.DeadlineExceeded, got %v (elapsed %v)", err, elapsed)
		}
		if elapsed > maxElapsed {
			t.Fatalf("io.popen:read cancellation took too long: %v", elapsed)
		}
	})

	t.Run("ctx_precanceled_shell", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(`return os.execute("echo should_not_run")`), "=ctx_pre")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.SetContext(ctx)
		if _, err := s.Call(cl); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})
}

// TestLua55SemanticParityRegressions locks down all Lua 5.5.1 semantic parity fixes
// verified against PUC-Rio Lua 5.5.1 in Track 3.
func TestLua55SemanticParityRegressions(t *testing.T) {
	nonExist := filepath.Join(t.TempDir(), "does_not_exist.lua")

	s := NewState()
	defer s.Close()
	s.SetGlobal("NONEXIST_PATH", StringValue(s.Intern(nonExist)))
	cl, err := s.LoadChunk([]byte(`
		-- 1. Coroutine uncaught runtime errors retain chunk:line: prefix (resume + wrap + post-yield).
		do
			local co = coroutine.create(function()
				local a = 1
				return a + nil
			end)
			local ok, msg = coroutine.resume(co)
			assert(not ok and type(msg) == "string" and msg:find("^parity:%d+: attempt to perform arithmetic"), "got: " .. tostring(msg))

			local co2 = coroutine.create(function()
				coroutine.yield(1)
				local x = nil
				return x + 1
			end)
			assert(coroutine.resume(co2))
			local ok2, msg2 = coroutine.resume(co2)
			assert(not ok2 and msg2:find("^parity:%d+: attempt to perform arithmetic"), "got: " .. tostring(msg2))

			local w = coroutine.wrap(function()
				local y = nil
				return y + 1
			end)
			local ok3, msg3 = pcall(w)
			assert(not ok3 and msg3:find("^parity:%d+: attempt to perform arithmetic"), "got: " .. tostring(msg3))
		end

		-- 2. xpcall message handler runs BEFORE native unwind (table.sort comparator + direct debug.traceback).
		do
			local ok, tb = xpcall(function()
				table.sort({1, 2, 3}, function()
					error("stop")
				end)
			end, debug.traceback)
			assert(not ok, "expected xpcall failure")
			assert(tb:find("[C]: in global 'error'", 1, true), "missing error frame:\n" .. tb)
			assert(tb:find("[C]: in field 'sort'", 1, true), "missing sort frame:\n" .. tb)
			assert(tb:find("[C]: in global 'xpcall'", 1, true), "missing xpcall frame:\n" .. tb)

			-- Inside Lua message handler, level 2 is C error().
			local lvl2Name
			xpcall(function() error("boom") end, function(e)
				lvl2Name = debug.getinfo(2, "n").name
				return e
			end)
			assert(lvl2Name == "error", "expected getinfo(2).name == 'error', got: " .. tostring(lvl2Name))

			-- Normal __close error inside xpcall runs msgh with __close on stack.
			local okc, tbc = xpcall(function()
				local _ <close> = setmetatable({}, {__close = function() error("from_close") end})
			end, debug.traceback)
			assert(not okc and tbc:find("from_close", 1, true) and tbc:find("[C]: in global 'error'", 1, true), "got:\n" .. tostring(tbc))
		end

		-- 3. OS/IO error messages strip Go verb prefix (no doubled "cannot open open ...").
		do
			local path = NONEXIST_PATH
			local f, err1 = io.open(path, "r")
			assert(f == nil and not err1:find("^open "), "Go 'open ' prefix leaked: " .. tostring(err1))
			local fn, err2 = loadfile(path)
			assert(fn == nil and not err2:find("cannot open open ", 1, true), "doubled open prefix: " .. tostring(err2))
			local okr, err3 = os.remove(path)
			assert(okr == nil and not err3:find("^remove "), "Go 'remove ' prefix leaked: " .. tostring(err3))
		end

		-- 4. Argument & comparison error messages match PUC-Rio Lua 5.5.1.
		do
			local function assertErr(fn, pat)
				local ok, msg = pcall(fn)
				assert(not ok and type(msg) == "string" and msg:find(pat), "expected " .. pat .. ", got: " .. tostring(msg))
			end
			assertErr(function() string.rep() end, "bad argument #1 to 'rep' %(string expected, got no value%)")
			assertErr(function() math.floor("a") end, "bad argument #1 to 'floor' %(number expected, got string%)")
			assertErr(function() warn() end, "bad argument #1 to 'warn' %(string expected, got no value%)")
			assertErr(function() io.stdout:write({}) end, "bad argument #1 to 'write' %(string expected, got table%)")
			assertErr(function() io.stdout:seek("bad") end, "bad argument #1 to 'seek' %(invalid option 'bad'%)")
			assertErr(function() io.stdout.seek(io.stdout, "bad") end, "bad argument #2 to 'seek' %(invalid option 'bad'%)")
			assertErr(function() table.move({}, "a", 1, 1) end, "bad argument #2 to 'move'")
			assertErr(function() string.packsize("Xz") end, "bad argument #1 to 'packsize'")
			assertErr(function() os.date("%Q") end, "bad argument #1 to 'date'")
			assertErr(function() debug.upvaluejoin(1, 1, 1, 1) end, "bad argument #1 to 'upvaluejoin' %(function expected, got number%)")
			assertErr(function() table.sort({true, false}) end, "attempt to compare two boolean values")
			assertErr(function() debug.sethook(1) end, "bad argument #2 to 'sethook' %(string expected, got no value%)")
			assertErr(function() debug.getinfo(1, "x") end, "bad argument #2 to 'getinfo' %(invalid option%)$")
			assertErr(function() debug.getinfo(1, ">") end, "bad argument #2 to 'getinfo' %(invalid option '>'%)$")
		end

		-- 5. debug.getupvalue / setupvalue bad index returns 0 values; getuservalue returns 1 nil.
		do
			local function fn() end
			assert(select("#", debug.getupvalue(fn, 99)) == 0)
			assert(select("#", debug.setupvalue(fn, 99, 1)) == 0)
			assert(select("#", debug.getuservalue(123)) == 1 and debug.getuservalue(123) == nil)
			assert(select("#", debug.getuservalue(io.stdout, 99)) == 1 and debug.getuservalue(io.stdout, 99) == nil)
		end

		-- 6. C return hook ftransfer == nargs + 1 and getlocal reads transferred result at ftransfer.
		do
			local gotF, gotN, gotVal
			local function hook(ev)
				if ev == "return" then
					local info = debug.getinfo(2, "fnr")
					if info.func == math.max then
						gotF, gotN = info.ftransfer, info.ntransfer
						_, gotVal = debug.getlocal(2, info.ftransfer)
					end
				end
			end
			debug.sethook(hook, "r")
			local m = math.max(1, 2, 3)
			debug.sethook()
			assert(m == 3 and gotF == 4 and gotN == 1 and gotVal == 3,
				string.format("got ftransfer=%s ntransfer=%s val=%s", tostring(gotF), tostring(gotN), tostring(gotVal)))
		end

		-- 7. Call hook naming: C function called from C (pcall(error, "x")) has name == nil.
		do
			local seenName = "unset"
			debug.sethook(function(ev)
				if ev == "call" then
					local info = debug.getinfo(2, "fn")
					if info.func == error then
						seenName = info.name
					end
				end
			end, "c")
			pcall(error, "x")
			debug.sethook()
			assert(seenName == nil, "expected nil name for pcall(error), got: " .. tostring(seenName))
		end

		-- 8. Yield across C-call boundary has no chunk:line: prefix.
		do
			local ok, msg = pcall(function() coroutine.yield() end)
			assert(not ok and msg == "attempt to yield from outside a coroutine", "got: " .. tostring(msg))
			local co = coroutine.create(function()
				table.sort({1, 2}, function() coroutine.yield() end)
			end)
			local ok2, msg2 = coroutine.resume(co)
			assert(not ok2 and msg2 == "attempt to yield across a C-call boundary", "got: " .. tostring(msg2))
		end

		-- 9. package.loadlib exists and returns nil, msg, "absent".
		do
			assert(type(package.loadlib) == "function")
			local fn, msg, where = package.loadlib("nonexistent.so", "luaopen_x")
			assert(fn == nil and type(msg) == "string" and where == "absent",
				string.format("got %s, %s, %s", tostring(fn), tostring(msg), tostring(where)))
		end

		return "ok"
	`), "=parity")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Call(cl)
	if err != nil {
		t.Fatalf("parity regressions failed: %v", err)
	}
	if len(res) != 1 || s.AsString(res[0]).Str() != "ok" {
		t.Fatalf("unexpected res: %v", res)
	}

	// Ensure temp dir check doesn't leave stray files.
	_ = os.Remove(nonExist)
}

// TestBulkFileWriteCancelAndBudget verifies PR #4 Round 37 P1:
// both buffered and unbuffered file:write poll context deadlines and instruction budgets
// in bounded chunks without blocking or bypassing execution limits.
func TestBulkFileWriteCancelAndBudget(t *testing.T) {
	for _, bufMode := range []string{"full", "no"} {
		t.Run("expired_ctx_"+bufMode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.bin")
			s := NewState()
			defer s.Close()
			s.SetGlobal("OUT_PATH", StringValue(s.Intern(path)))
			s.SetGlobal("BUF_MODE", StringValue(s.Intern(bufMode)))
			setup, err := s.LoadChunk([]byte(`
				F = assert(io.open(OUT_PATH, "wb"))
				F:setvbuf(BUF_MODE)
				BIG = string.rep("x", 2 * 1024 * 1024)
			`), "=write_setup")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Call(setup); err != nil {
				t.Fatal(err)
			}
			writeFn, err := s.LoadChunk([]byte(`return F:write(BIG)`), "=write_run")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			defer cancel()
			<-ctx.Done()
			s.SetContext(ctx)
			if _, err := s.Call(writeFn); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("bufMode=%s: expected context.DeadlineExceeded, got %v", bufMode, err)
			}
		})

		t.Run("insn_limit_"+bufMode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.bin")
			s := NewState()
			defer s.Close()
			s.SetGlobal("OUT_PATH", StringValue(s.Intern(path)))
			s.SetGlobal("BUF_MODE", StringValue(s.Intern(bufMode)))
			setup, err := s.LoadChunk([]byte(`
				F = assert(io.open(OUT_PATH, "wb"))
				F:setvbuf(BUF_MODE)
				BIG = string.rep("x", 2 * 1024 * 1024)
			`), "=write_setup")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Call(setup); err != nil {
				t.Fatal(err)
			}
			smallFn, err := s.LoadChunk([]byte(`return F:write("hello")`), "=write_small")
			if err != nil {
				t.Fatal(err)
			}
			s.SetInstructionLimit(100)
			if _, err := s.Call(smallFn); err != nil {
				t.Fatalf("bufMode=%s: small write failed under 100-insn budget: %v", bufMode, err)
			}

			bigFn, err := s.LoadChunk([]byte(`return F:write(BIG)`), "=write_big")
			if err != nil {
				t.Fatal(err)
			}
			s.SetInstructionLimit(100)
			if _, err := s.Call(bigFn); !errors.Is(err, ErrInstructionLimit) {
				t.Fatalf("bufMode=%s: expected ErrInstructionLimit on 2 MiB write, got %v", bufMode, err)
			}
		})
	}
}

// TestTryNewUserdataInitializesUValuesToNil verifies PR #4 Round 37 P2:
// TryNewUserdata(data, nUValues) initializes every user value slot to Lua nil
// across both default (intnil) and moeluafloatnil build modes.
func TestTryNewUserdataInitializesUValuesToNil(t *testing.T) {
	s := NewState()
	defer s.Close()
	u, err := s.TryNewUserdata(nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	s.SetGlobal("UD", u)
	cl, err := s.LoadChunk([]byte(`
		local v1, ok1 = debug.getuservalue(UD, 1)
		local v2, ok2 = debug.getuservalue(UD, 2)
		assert(ok1 == true and v1 == nil and type(v1) == "nil", "slot 1: " .. tostring(v1) .. " " .. type(v1))
		assert(ok2 == true and v2 == nil and type(v2) == "nil", "slot 2: " .. tostring(v2) .. " " .. type(v2))
		local v3, ok3 = debug.getuservalue(UD, 3)
		assert(v3 == nil and ok3 == nil)
		return "ok"
	`), "=ud_uvalues")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(cl); err != nil {
		t.Fatal(err)
	}
}

// TestCanceledGCSeparationPreservesAllFinalizers verifies PR #4 Round 38 P2:
// if a full GC is cancelled mid-Step-3 (finalizer/file separation), no gcFnzed flags
// are left mutated, so a subsequent uncancelled collection runs 100% of finalizers.
func TestCanceledGCSeparationPreservesAllFinalizers(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "probe.txt")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewState()
	defer s.Close()
	s.SetGlobal("PROBE_PATH", StringValue(s.Intern(tmpFile)))
	cl, err := s.LoadChunk([]byte(`
		collectgarbage("stop")
		TBL_FNZ = 0
		FILE_FNZ = 0
		local tmt = { __gc = function() TBL_FNZ = TBL_FNZ + 1 end }
		local sample = assert(io.open(PROBE_PATH, "r"))
		local fmt = debug.getmetatable(sample)
		local origGC = fmt.__gc
		fmt.__gc = function(f)
			FILE_FNZ = FILE_FNZ + 1
			return origGC(f)
		end
		sample:close()
		-- Interleave discarded files and finalizable tables (> 4096 total entries so
		-- Step 3 hits tickCancel mid-loop under SetInstructionLimit(1)).
		for i = 1, 50 do
			assert(io.open(PROBE_PATH, "r"))
		end
		for i = 1, 5000 do
			setmetatable({}, tmt)
		end
		for i = 1, 50 do
			assert(io.open(PROBE_PATH, "r"))
		end
	`), "=cancel_gc_sep")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(cl); err != nil {
		t.Fatal(err)
	}

	// Abort CollectFull mid-way with a 1-instruction budget.
	s.SetInstructionLimit(1)
	s.CollectFull()

	// Disable budget and collect cleanly: every single table & file finalizer must run.
	s.SetInstructionLimit(0)
	s.CollectFull()

	tblCount := s.GetGlobal("TBL_FNZ").Int()
	fileCount := s.GetGlobal("FILE_FNZ").Int()
	if tblCount != 5000 {
		t.Fatalf("lost table finalizers after cancelled GC separation: got %d, want 5000", tblCount)
	}
	// sample + 50 + 50 = 101 files total
	if fileCount < 100 {
		t.Fatalf("lost file finalizers after cancelled GC separation: got %d, want >= 100", fileCount)
	}
}

// TestCloseAllSyncRootsCallingCoroutine verifies PR #4 Round 38 P1:
// when a Go-resumed coroutine (reachable only via s.cur) closes another suspended
// coroutine whose __close runs collectgarbage(), live locals on the calling coroutine's
// stack are NOT prematurely finalized or collected.
func TestCloseAllSyncRootsCallingCoroutine(t *testing.T) {
	s := NewState()
	defer s.Close()
	cl, err := s.LoadChunk([]byte(`
		PREMATURE_GC = false
		return function()
			local alive = setmetatable({ tag = "still-alive" }, {
				__gc = function() PREMATURE_GC = true end,
			})
			local coB = coroutine.create(function()
				local _ <close> = setmetatable({}, {
					__close = function()
						collectgarbage("collect")
					end,
				})
				coroutine.yield("ready")
			end)
			assert(coroutine.resume(coB))
			local ok, err = coroutine.close(coB)
			assert(ok, tostring(err))
			assert(not PREMATURE_GC, "calling coroutine's live local was finalized during coroutine.close")
			return alive.tag
		end
	`), "=close_roots_caller")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Call(cl)
	if err != nil {
		t.Fatal(err)
	}
	coA, err := s.NewThread(res[0])
	if err != nil {
		t.Fatal(err)
	}
	// Do NOT store coA in globals; resume directly from Go so s.cur is its sole root!
	out, yielded, err := s.Resume(coA)
	if err != nil || yielded {
		t.Fatalf("Resume failed: yielded=%v out=%v err=%v", yielded, out, err)
	}
	if len(out) != 1 || !out[0].IsString() || s.AsString(out[0]).Str() != "still-alive" {
		t.Fatalf("unexpected coroutine result: %v", out)
	}
}

// TestNumericConcatRespectsMemLimit verifies PR #4 Round 39 P2:
// numeric operands in OP_CONCAT fast path are included in the memory-limit check.
func TestNumericConcatRespectsMemLimit(t *testing.T) {
	s := NewState()
	defer s.Close()
	cl, err := s.LoadChunk([]byte(`
		local n = 9223372036854775807
		return n..n..n..n..n..n..n..n..n..n..n..n..n..n..n..n..n..n..n..n
	`), "=num_concat_mem")
	if err != nil {
		t.Fatal(err)
	}
	clRef := s.Ref(cl)
	defer s.Unref(clRef)
	s.SetMemoryLimit(1 << 30)
	s.CollectFull()
	limit := s.MemoryUsage() + 100
	s.SetMemoryLimit(limit)

	_, err = s.Call(cl)
	if err == nil || !errors.Is(err, err) || err.Error() != "not enough memory" {
		t.Fatalf("expected 'not enough memory', got %v", err)
	}
	s.CollectFull()
	if usage := s.MemoryUsage(); usage > limit {
		t.Fatalf("MemoryUsage=%d exceeded limit=%d", usage, limit)
	}
}

// TestIOPopenOOMDoesNotWaitOnLongChild verifies PR #4 Round 39 P2:
// io.popen and tryNewFileUD OOM error paths terminate any started child before waiting,
// returning promptly with "not enough memory" instead of blocking until the child finishes.
func TestIOPopenOOMDoesNotWaitOnLongChild(t *testing.T) {
	sleepCmd := "sleep 3"
	if runtime.GOOS == "windows" {
		sleepCmd = "ping -n 4 127.0.0.1 > NUL"
	}

	t.Run("io_popen_oom", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cl, err := s.LoadChunk([]byte(fmt.Sprintf(`return io.popen(%q, "r")`, sleepCmd)), "=popen_oom")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMemoryLimit(1)
		start := time.Now()
		_, err = s.Call(cl)
		elapsed := time.Since(start)
		if err == nil || err.Error() != "not enough memory" {
			t.Fatalf("expected 'not enough memory', got %v", err)
		}
		if elapsed > time.Second {
			t.Fatalf("io.popen OOM blocked too long: %v", elapsed)
		}
	})

	t.Run("tryNewFileUD_kills_started_child_before_wait", func(t *testing.T) {
		s := NewState()
		defer s.Close()
		cmd := shellCommand(context.Background(), sleepCmd)
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout = pw
		if err := cmd.Start(); err != nil {
			_ = pr.Close()
			_ = pw.Close()
			t.Fatal(err)
		}
		_ = pw.Close()

		s.SetMemoryLimit(1)
		start := time.Now()
		_, err = s.tryNewFileUD(&luaFile{f: pr, cmd: cmd})
		elapsed := time.Since(start)
		if err == nil || err.Error() != "not enough memory" {
			t.Fatalf("expected 'not enough memory', got %v", err)
		}
		if elapsed > time.Second {
			t.Fatalf("tryNewFileUD OOM waited for child instead of killing it: %v", elapsed)
		}
	})
}

// TestFormatPLightUserdataLogicalIdentity verifies PR #4 Round 39 P2:
// string.format("%p", x) formats encoded light userdata (debug.upvalueid tokens and
// NULL light userdata) using their logical pointer identity rather than the tag sentinel.
func TestFormatPLightUserdataLogicalIdentity(t *testing.T) {
	s := NewState()
	defer s.Close()
	s.SetGlobal("NULL_LUD", LightUDValue(nil))
	cl, err := s.LoadChunk([]byte(`
		local g1 = string.gmatch("a", "a")
		local g2 = string.gmatch("b", "b")
		local id1 = debug.upvalueid(g1, 1)
		local id2 = debug.upvalueid(g2, 1)
		assert(id1 ~= id2, "distinct gmatch closures must have distinct upvalueid tokens")
		local p1 = string.format("%p", id1)
		local p2 = string.format("%p", id2)
		assert(p1 ~= p2, "distinct upvalueid tokens formatted identically: " .. p1 .. " == " .. p2)
		assert(tostring(id1) == "userdata: " .. p1, "tostring/format mismatch: " .. tostring(id1) .. " vs " .. p1)
		assert(tostring(id2) == "userdata: " .. p2, "tostring/format mismatch: " .. tostring(id2) .. " vs " .. p2)
		assert(string.format("%p", NULL_LUD) == "(null)", "NULL lightud formatted as: " .. string.format("%p", NULL_LUD))
		return "ok"
	`), "=fmt_p_lud")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(cl); err != nil {
		t.Fatal(err)
	}
}
