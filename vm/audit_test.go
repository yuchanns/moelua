package vm_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
	"github.com/yuchanns/moelua/vm"
)

func runLuaSnippet(t *testing.T, src string) {
	t.Helper()
	p, err := compiler.Compile("=audit", src)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	s := vm.NewState()
	defer s.Close()
	var out bytes.Buffer
	s.SetOutput(&out)
	s.SetErrOutput(&out)
	cl, err := s.Load(p)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if _, err := s.Call(vm.ClosureValue(cl)); err != nil {
		t.Fatalf("exec failed: %v\noutput:\n%s", err, out.String())
	}
}

// TestAuditVerifierBoundsAndCFG verifies bytecode verifier & undumper safety checks:
// CFG successor bounds, register range bounds, MMBIN tag range, VARARGPREP flag consistency,
// and proto nesting depth limits.
func TestAuditVerifierBoundsAndCFG(t *testing.T) {
	mkABC := func(op bytecode.OpCode, a, b, c int, k bool) bytecode.Instruction {
		w := uint32(op) | uint32(a)<<bytecode.PosA | uint32(b)<<bytecode.PosB | uint32(c)<<bytecode.PosC
		if k {
			w |= 1 << bytecode.PosK
		}
		return bytecode.Instruction(w)
	}
	mkAx := func(op bytecode.OpCode, ax int) bytecode.Instruction {
		return bytecode.Instruction(uint32(op) | uint32(ax)<<bytecode.PosA)
	}
	mkSJ := func(op bytecode.OpCode, sj int) bytecode.Instruction {
		return bytecode.Instruction(uint32(op) | uint32(sj+bytecode.OffsetSJ)<<bytecode.PosA)
	}

	// OP_LFALSESKIP at len-2 skips RETURN0 and runs off end of Code.
	p1 := &bytecode.Proto{
		MaxStackSize: 2,
		Code: []bytecode.Instruction{
			mkABC(bytecode.OP_LFALSESKIP, 0, 0, 0, false),
			mkABC(bytecode.OP_RETURN0, 0, 0, 0, false),
		},
	}
	if err := bytecode.Verify(p1); err == nil {
		t.Fatalf("expected Verify to reject OP_LFALSESKIP at len-2")
	}

	// OP_EQ at len-2 followed by OP_JMP at len-1 falls through past end of Code.
	p2 := &bytecode.Proto{
		MaxStackSize: 2,
		Code: []bytecode.Instruction{
			mkABC(bytecode.OP_EQ, 0, 1, 0, false),
			mkSJ(bytecode.OP_JMP, -1),
		},
	}
	if err := bytecode.Verify(p2); err == nil {
		t.Fatalf("expected Verify to reject OP_EQ+OP_JMP at len-2..len-1")
	}

	// OP_LFALSESKIP before OP_LOADKX + OP_EXTRAARG lands on OP_EXTRAARG.
	p3 := &bytecode.Proto{
		MaxStackSize: 2,
		Consts:       []bytecode.Constant{{Kind: bytecode.ConstInt, Int: 1}},
		Code: []bytecode.Instruction{
			mkABC(bytecode.OP_LFALSESKIP, 0, 0, 0, false),
			mkABC(bytecode.OP_LOADKX, 0, 0, 0, false),
			mkAx(bytecode.OP_EXTRAARG, 0),
			mkABC(bytecode.OP_RETURN0, 0, 0, 0, false),
		},
	}
	if err := bytecode.Verify(p3); err == nil {
		t.Fatalf("expected Verify to reject OP_LFALSESKIP landing on OP_EXTRAARG")
	}

	// OP_TFORCALL A=251, C=2 exceeds maxStack=255 (A+2+C = 255 >= maxStack).
	p4 := &bytecode.Proto{
		MaxStackSize: 255,
		Code: []bytecode.Instruction{
			mkABC(bytecode.OP_TFORCALL, 251, 0, 2, false),
			mkABC(bytecode.OP_TFORLOOP, 251, 0, 0, false),
			mkABC(bytecode.OP_RETURN0, 0, 0, 0, false),
		},
	}
	if err := bytecode.Verify(p4); err == nil {
		t.Fatalf("expected Verify to reject OP_TFORCALL register overflow")
	}

	// OP_MMBIN with C=0 (TM_INDEX) must be rejected (only TM_ADD..TM_SHR 6..17 allowed).
	p5 := &bytecode.Proto{
		MaxStackSize: 2,
		Code: []bytecode.Instruction{
			mkABC(bytecode.OP_ADD, 0, 0, 1, false),
			mkABC(bytecode.OP_MMBIN, 0, 1, 0, false),
			mkABC(bytecode.OP_RETURN0, 0, 0, 0, false),
		},
	}
	if err := bytecode.Verify(p5); err == nil {
		t.Fatalf("expected Verify to reject OP_MMBIN with C=TM_INDEX")
	}

	// Non-vararg proto with OP_VARARGPREP at pc 0 must be rejected.
	p6 := &bytecode.Proto{
		MaxStackSize: 2,
		Code: []bytecode.Instruction{
			mkABC(bytecode.OP_VARARGPREP, 0, 0, 0, false),
			mkABC(bytecode.OP_RETURN0, 0, 0, 0, false),
		},
	}
	if err := bytecode.Verify(p6); err == nil {
		t.Fatalf("expected Verify to reject OP_VARARGPREP on non-vararg proto")
	}

	// Deeply nested proto tree (>200 levels) must fail cleanly without Go stack overflow.
	root := &bytecode.Proto{
		MaxStackSize: 2,
		Code:         []bytecode.Instruction{mkABC(bytecode.OP_RETURN0, 0, 0, 0, false)},
	}
	cur := root
	for range 210 {
		child := &bytecode.Proto{
			MaxStackSize: 2,
			Code:         []bytecode.Instruction{mkABC(bytecode.OP_RETURN0, 0, 0, 0, false)},
		}
		cur.Protos = []*bytecode.Proto{child}
		cur = child
	}
	if err := bytecode.Verify(root); err == nil {
		t.Fatalf("expected Verify to reject 210-deep nested proto")
	}
	dumped, err := bytecode.Dump(root, false)
	if err != nil {
		t.Fatalf("dump deep proto: %v", err)
	}
	if _, err := bytecode.Undump(dumped); err == nil {
		t.Fatalf("expected Undump to reject 210-deep nested proto")
	}
}

// TestAuditStackReallocInGC tests a __gc finalizer recursing deeply (forcing th.stack
// reallocation) triggered mid-allocation loop while keeping live registers intact,
// and ensures illegal yield inside __gc does not corrupt a running coroutine.
func TestAuditStackReallocInGC(t *testing.T) {
	runLuaSnippet(t, `
		local ran = false
		local function recurse(n)
			if n <= 0 then return 1 end
			local a, b, c, d = n, n+1, n+2, n+3
			return recurse(n - 1) + (a - a) + (b - b) + (c - c) + (d - d)
		end
		do
			local sentinel = setmetatable({}, {__gc = function()
				assert(recurse(500) == 1)
				ran = true
			end})
		end
		local keep1, keep2, keep3 = "alpha", 12345, {x = 99}
		for i = 1, 5000 do
			local tmp = {i, i+1}
			if ran then break end
		end
		assert(ran, "finalizer must run via proportional Tier 1 debt trigger")
		assert(keep1 == "alpha" and keep2 == 12345 and keep3.x == 99)

		-- Yield inside __gc inside a running coroutine must fail cleanly as non-yieldable
		-- without corrupting the coroutine state.
		local co = coroutine.create(function()
			do
				local _ = setmetatable({}, {__gc = function()
					coroutine.yield("illegal")
				end})
			end
			collectgarbage()
			return "survived"
		end)
		local ok, res = coroutine.resume(co)
		assert(ok and res == "survived", "coroutine must survive illegal yield inside __gc")
	`)
}

// TestAuditFileHandleGCAndFlush verifies dropped file handles flush & close on collectgarbage()
// and on State.Close() even without user __gc/__mode tables.
func TestAuditFileHandleGCAndFlush(t *testing.T) {
	tmp := t.TempDir()
	f1 := filepath.Join(tmp, "gc_closed.txt")
	f2 := filepath.Join(tmp, "state_close.txt")

	// 1. Dropped buffered file handle closed & flushed by explicit collectgarbage() in Tier 0 state.
	runLuaSnippet(t, fmt.Sprintf(`
		do
			local f = assert(io.open(%q, "w"))
			f:setvbuf("full", 4096)
			f:write("flushed-by-gc")
		end
		collectgarbage()
		local r = assert(io.open(%q, "r"))
		assert(r:read("a") == "flushed-by-gc")
		r:close()
	`, f1, f1))

	// 2. Live buffered file handle flushed & closed on State.Close().
	p, err := compiler.Compile("=closeflush", fmt.Sprintf(`
		G_FILE = assert(io.open(%q, "w"))
		G_FILE:setvbuf("full", 4096)
		G_FILE:write("flushed-by-state-close")
	`, f2))
	if err != nil {
		t.Fatal(err)
	}
	s := vm.NewState()
	cl, err := s.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(vm.ClosureValue(cl)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	data, err := os.ReadFile(f2)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "flushed-by-state-close" {
		t.Fatalf("expected flushed-by-state-close, got %q", string(data))
	}
}

// TestAuditLateWeakModeActivation verifies setting mt.__mode AFTER setmetatable activates Tier 1.
func TestAuditLateWeakModeActivation(t *testing.T) {
	runLuaSnippet(t, `
		local mt = {}
		local t = setmetatable({}, mt)
		t[{}] = 123
		mt.__mode = "k"
		collectgarbage()
		assert(next(t) == nil, "late __mode='k' must clear dead weak key")
	`)
	// Also test existing nil slot updated to __mode = "v".
	runLuaSnippet(t, `
		local mt = { __mode = nil }
		local t = setmetatable({ a = {} }, mt)
		mt.__mode = "v"
		collectgarbage()
		assert(t.a == nil, "late __mode='v' on existing shape slot must clear dead weak value")
	`)
}

// TestAuditTopDisciplineAndMultret tests multret stack growth, <close> return protection,
// xpcall handler frame inspection, and count-hook interaction with open-top calls.
func TestAuditTopDisciplineAndMultret(t *testing.T) {
	runLuaSnippet(t, `
		-- 1. Large table.unpack into table constructor
		local big = {}
		for i = 1, 10000 do big[i] = i end
		local copy = {table.unpack(big, 1, 10000)}
		assert(#copy == 10000 and copy[1] == 1 and copy[10000] == 10000)

		-- 2. Many return values through <close> (sync + yielding)
		local closed = 0
		local function regHungry(x)
			local a,b,c,d,e,f,g,h,i,j = 1,2,3,4,5,6,7,8,9,10
			closed = closed + a + b - 3 + 1
		end
		local function retMany()
			local x <close> = setmetatable({}, {__close = regHungry})
			return table.unpack(big, 1, 400)
		end
		local r = {retMany()}
		assert(#r == 400 and r[1] == 1 and r[400] == 400 and closed == 1)

		local co = coroutine.create(function()
			local x <close> = setmetatable({}, {__close = function()
				coroutine.yield("in-close")
			end})
			return table.unpack(big, 1, 300)
		end)
		local ok1, yv = coroutine.resume(co)
		assert(ok1 and yv == "in-close")
		local r2 = {coroutine.resume(co)}
		assert(r2[1] == true and #r2 == 301 and r2[2] == 1 and r2[301] == 300)

		-- 3. xpcall error handler inspecting fast-called frame with <close>
		local gotLocal, gotCloseObj
		local sentinel = {tag = "tbc-val"}
		local function fastCallee(arg)
			local c <close> = setmetatable(sentinel, {__close = function(self, err)
				gotCloseObj = self
				assert(err == "handled:boom")
			end})
			error("boom")
		end
		local ok, msg = xpcall(function() return fastCallee(42) end, function(e)
			local name, val = debug.getlocal(2, 1)
			if name == "arg" then gotLocal = val end
			return "handled:" .. tostring(e):match("boom$")
		end)
		assert(not ok and msg == "handled:boom")
		assert(gotCloseObj == sentinel)

		-- 4. Count hook = 1 between multret producer and consumer
		local small = {}
		for i = 1, 300 do small[i] = i end
		local hooks = 0
		debug.sethook(function() hooks = hooks + 1 end, "", 1)
		local cnt = select("#", table.unpack(small, 1, 300))
		local tbl = {table.unpack(small, 1, 300)}
		debug.sethook()
		assert(cnt == 300 and #tbl == 300 and tbl[300] == 300 and hooks > 0)
	`)
}

// TestAuditParallelStatesRace executes 8 independent *vm.State instances concurrently
// on separate goroutines sharing one immutable compiled *bytecode.Proto under -race.
func TestAuditParallelStatesRace(t *testing.T) {
	const script = `
		local sum = 0
		local cache = setmetatable({}, {__mode = "v"})
		for i = 1, 200 do
			local longKey = string.rep("k", 50) .. tostring(i)
			cache[longKey] = {v = i}
			sum = sum + i
		end
		collectgarbage()
		local co = coroutine.create(function(x)
			local c <close> = setmetatable({}, {__close = function() sum = sum + 1 end})
			coroutine.yield(x * 2)
			return x * 3
		end)
		local _, a = coroutine.resume(co, 10)
		local _, b = coroutine.resume(co)
		local s = string.gsub(string.format("%d:%d:%d", sum, a, b), "(%d+)", "[%1]")
		assert(s == "[20101]:[20]:[30]")
		return s
	`
	sharedProto, err := compiler.Compile("=parallel", script)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s := vm.NewState()
			defer s.Close()
			var buf bytes.Buffer
			s.SetOutput(&buf)
			s.SetErrOutput(&buf)
			cl, err := s.Load(sharedProto)
			if err != nil {
				errs <- fmt.Errorf("g%d load: %w", id, err)
				return
			}
			res, err := s.Call(vm.ClosureValue(cl))
			if err != nil {
				errs <- fmt.Errorf("g%d call: %w", id, err)
				return
			}
			if len(res) != 1 || !strings.Contains(vm.FormatValue(res[0]), "[20101]:[20]:[30]") {
				errs <- fmt.Errorf("g%d unexpected result: %v", id, res)
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
