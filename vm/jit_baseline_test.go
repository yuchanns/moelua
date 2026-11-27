package vm_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuchanns/moelua/compiler"
	"github.com/yuchanns/moelua/vm"
)

func runLuaProgram(t *testing.T, src string, useJIT bool) (string, error) {
	t.Helper()
	proto, err := compiler.Compile("=jit_test", src)
	if err != nil {
		t.Fatalf("compile failed: %v\nsource:\n%s", err, src)
	}
	s := vm.NewState()
	defer s.Close()
	if useJIT {
		s.SetJITEnabled(true)
	}
	var out bytes.Buffer
	s.SetOutput(&out)
	s.SetErrOutput(&out)
	cl, err := s.Load(proto)
	if err != nil {
		return out.String(), err
	}
	res, err := s.Call(vm.ClosureValue(cl))
	if err != nil {
		return out.String(), err
	}
	for i, v := range res {
		if i > 0 {
			out.WriteByte('\t')
		}
		out.WriteString(s.FormatValue(v))
	}
	return out.String(), nil
}

func TestJITMatchesInterpreter(t *testing.T) {
	if !vm.JITSupported() {
		t.Skip("JIT unsupported in this build configuration")
	}

	cases := []struct {
		name string
		src  string
	}{
		{
			name: "arithmetic_and_edge_cases",
			src: `
local out = {}
local function emit(v) out[#out+1] = tostring(v) end
local a, b = 17, -5
emit(a + b); emit(a - b); emit(a * b); emit(a / b); emit(a // b); emit(a % b)
emit((-a) // b); emit((-a) % b); emit(a // (-b)); emit(a % (-b))
emit(math.mininteger // -1); emit(math.mininteger % -1)
emit(1.5 + 2); emit(2 + 1.5); emit(7.5 // 2.0); emit((-7.5) // 2.0); emit(7.5 % 2.0)
emit(-0.0 == 0.0); emit(tostring(-0.0)); emit(1.0 / -0.0)
local nan = 0.0 / 0.0
emit(nan == nan); emit(nan < 1); emit(nan <= 1); emit(nan > 1); emit(nan >= 1); emit(nan ~= nan)
local sum = 0
for i = 1, 100 do
  sum = sum + (i + 3) * 2 - (i // 3) + (i % 7)
end
emit(sum)
local fsum = 0.0
for x = 0.5, 10.0, 0.5 do
  fsum = fsum + x * 1.5 - 0.25
end
emit(string.format("%.6f", fsum))
return table.concat(out, ",")
`,
		},
		{
			name: "bitwise_and_shifts",
			src: `
local out = {}
local function emit(v) out[#out+1] = tostring(v) end
for i = -70, 70, 7 do
  emit((0x123456789ABCDEF0 << i) ~ (0xFEDCBA9876543210 >> i))
end
local x = 0x55AA55AA55AA55AA
for i = 1, 20 do
  x = ((x & 0x0F0F0F0F0F0F0F0F) | (~x & 0xF0F0F0F0F0F0F0F0)) ~ (i << 3)
  x = (x >> 1) ~ (1 << (i % 63))
end
emit(x)
emit(1 << 63); emit(1 << 64); emit(1 << -1); emit(8 >> -2); emit(8 >> 64); emit(8 >> -64)
return table.concat(out, ",")
`,
		},
		{
			name: "comparisons_and_test_testset",
			src: `
local out = {}
local function emit(v) out[#out+1] = tostring(v) end
local vals = {nil, false, true, 0, -0.0, 1, 1.0, 2, 2.5, "a", "a", "b", {}}
for i = 1, #vals do
  for j = 1, #vals do
    local x, y = vals[i], vals[j]
    emit((x == y) and 1 or 0)
    emit((x and y) ~= nil and 1 or 0)
    emit((x or y) and 1 or 0)
  end
end
local t = {10, 20, 30}
for i = 1, 20 do
  if i < 5 then emit("lt")
  elseif i <= 10 then emit("le")
  elseif i > 15 then emit("gt")
  elseif i >= 12 then emit("ge")
  else emit("mid") end
end
return table.concat(out, ",")
`,
		},
		{
			name: "typed_arrays_and_append",
			src: `
local ai, af, ab, ag = {}, {}, {}, {}
for i = 1, 64 do
  ai[i] = i * 3 - 7
  af[i] = i * 0.5 + 0.125
  ab[i] = (i % 2) == 0
  ag[i] = (i % 3 == 0) and ("s" .. i) or i
end
local si, sf, sb, sg = 0, 0.0, 0, 0
for i = 1, 64 do
  si = si + ai[i]
  sf = sf + af[i]
  if ab[i] then sb = sb + 1 end
  if type(ag[i]) == "number" then sg = sg + ag[i] else sg = sg + #ag[i] end
end
-- Trigger GETI / SETI constant indices
ai[1] = 100; ai[2] = ai[1] + ai[3]
af[1] = 2.5; af[2] = af[1] + af[3]
return si, sf, sb, sg, ai[2], af[2], #ai, #af, #ab
`,
		},
		{
			name: "shapes_poly_ic_and_self",
			src: `
local ProtoA = {}
ProtoA.__index = ProtoA
function ProtoA:step(dx) self.x = self.x + dx; return self.x end

local ProtoB = {}
ProtoB.__index = ProtoB
function ProtoB:step(dx) self.x = self.x + dx * 2; return self.x end

local objs = {}
for i = 1, 40 do
  if i % 2 == 0 then
    objs[i] = setmetatable({x = i, y = i * 2}, ProtoA)
  else
    objs[i] = setmetatable({y = i * 3, x = i, z = 1}, ProtoB)
  end
end

local sum = 0
for r = 1, 20 do
  for i = 1, #objs do
    sum = sum + objs[i]:step(r)
  end
end
return sum
`,
		},
		{
			name: "recursion_tailcall_upvalues_and_math_intrinsics",
			src: `
local function fib(n)
  if n < 2 then return n end
  return fib(n - 1) + fib(n - 2)
end

local function tailSum(n, acc)
  if n == 0 then return acc end
  return tailSum(n - 1, acc + n)
end

local function makeCounter(start)
  local v = start
  return function(d)
    v = v + d
    return v
  end
end

local c1 = makeCounter(10)
local c2 = makeCounter(100)
local s = 0
for i = 1, 30 do
  s = s + c1(i) + c2(1)
end

local r = 0.0
for i = 1, 50 do
  r = r + math.sqrt(i * i) + math.abs(-i) + math.abs(-i * 0.5)
end
return fib(18), tailSum(5000, 0), s, string.format("%.4f", r)
`,
		},
		{
			name: "globals_settabup_gettabup_and_gc_barriers",
			src: `
G_COUNT = 0
G_NODE = {v = 1}
local function bump(n)
  for i = 1, n do
    G_COUNT = G_COUNT + i
    if i % 50 == 0 then
      collectgarbage("step", 1)
      G_NODE.next = {v = i}
      G_NODE = G_NODE.next
    end
  end
  return G_COUNT, G_NODE.v
end
return bump(500)
`,
		},
		{
			name: "large_span_strings_len_and_eqk",
			src: `
local big1 = string.rep("x", 40000)
local big2 = string.rep("x", 40000)
local big3 = string.rep("y", 40000)
local n, eqShort, eqSame, eqDiff = 0, 0, 0, 0
for i = 1, 10 do
  n = #big1
  if big1 == "short" then eqShort = eqShort + 1 end
  if big1 == big2 then eqSame = eqSame + 1 end
  if big1 == big3 then eqDiff = eqDiff + 1 end
end
return n, eqShort, eqSame, eqDiff
`,
		},
		{
			name: "coroutines_pcall_and_metamethods",
			src: `
local mt = {
  __add = function(a, b) return setmetatable({v = a.v + b.v}, getmetatable(a)) end,
  __lt = function(a, b) return a.v < b.v end,
  __eq = function(a, b) return a.v == b.v end,
}
local function box(v) return setmetatable({v = v}, mt) end

local co = coroutine.create(function(n)
  local acc = box(0)
  for i = 1, n do
    acc = acc + box(i)
    if i % 3 == 0 then coroutine.yield(acc.v) end
  end
  return acc.v
end)
local out = {}
while coroutine.status(co) ~= "dead" do
  local ok, v = coroutine.resume(co, 15)
  assert(ok)
  out[#out+1] = tostring(v)
end
local ok, err = pcall(function() local x = 1; return x // 0 end)
out[#out+1] = tostring(ok)
out[#out+1] = tostring(box(3) < box(5))
out[#out+1] = tostring(box(4) == box(4))
return table.concat(out, ",")
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, wantErr := runLuaProgram(t, tc.src, false)
			got, gotErr := runLuaProgram(t, tc.src, true)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("error mismatch:\ninterp err=%v\njit err=%v", wantErr, gotErr)
			}
			if want != got {
				t.Fatalf("output mismatch:\ninterp: %s\njit:    %s", want, got)
			}
		})
	}
}

func TestJITDifferentialFuzz(t *testing.T) {
	if !vm.JITSupported() {
		t.Skip("JIT unsupported in this build configuration")
	}
	rng := rand.New(rand.NewSource(20261127))
	ops := []string{"+", "-", "*", "//", "%", "&", "|", "~", "<<", ">>"}
	cmps := []string{"==", "~=", "<", "<=", ">", ">="}

	for trial := range 25 {
		var sb strings.Builder
		sb.WriteString("local function kernel(a, b, c)\n")
		sb.WriteString("  local x, y, z = a, b, c\n")
		sb.WriteString("  for i = 1, 15 do\n")
		for range 8 {
			op := ops[rng.Intn(len(ops))]
			dst := []string{"x", "y", "z"}[rng.Intn(3)]
			lhs := []string{"x", "y", "z", "i"}[rng.Intn(4)]
			rhs := []string{"x", "y", "z", "i", fmt.Sprintf("%d", rng.Intn(17)-8)}[rng.Intn(5)]
			if op == "//" || op == "%" {
				fmt.Fprintf(&sb, "    if %s ~= 0 then %s = (%s %s %s) end\n", rhs, dst, lhs, op, rhs)
			} else if op == "<<" || op == ">>" {
				fmt.Fprintf(&sb, "    %s = (%s %s (%s %% 67 - 33))\n", dst, lhs, op, rhs)
			} else {
				fmt.Fprintf(&sb, "    %s = (%s %s %s)\n", dst, lhs, op, rhs)
			}
		}
		cmp := cmps[rng.Intn(len(cmps))]
		fmt.Fprintf(&sb, "    if x %s y then z = z ~ x else z = z + y end\n", cmp)
		sb.WriteString("  end\n")
		sb.WriteString("  return x, y, z\n")
		sb.WriteString("end\n")
		sb.WriteString("local out = {}\n")
		sb.WriteString("for k = 1, 10 do\n")
		fmt.Fprintf(&sb, "  local a, b, c = kernel(k * %d, k - %d, %d)\n", rng.Intn(11)-5, rng.Intn(9)-4, rng.Intn(100)-50)
		sb.WriteString("  out[#out+1] = tostring(a) .. ':' .. tostring(b) .. ':' .. tostring(c)\n")
		sb.WriteString("end\nreturn table.concat(out, ';')\n")

		src := sb.String()
		want, wantErr := runLuaProgram(t, src, false)
		got, gotErr := runLuaProgram(t, src, true)
		if (wantErr == nil) != (gotErr == nil) || want != got {
			t.Fatalf("trial %d mismatch:\ninterp=(%q, %v)\njit=(%q, %v)\nsrc:\n%s", trial, want, wantErr, got, gotErr, src)
		}
	}
}

func TestJITLimitsHooksAndProtoGCChurn(t *testing.T) {
	if !vm.JITSupported() {
		t.Skip("JIT unsupported in this build configuration")
	}

	// 1. Instruction limit must remain uncatchable even when JIT is enabled.
	t.Run("instruction_limit", func(t *testing.T) {
		s := vm.NewState()
		defer s.Close()
		s.SetJITEnabled(true)
		s.SetInstructionLimit(500)
		proto, err := compiler.Compile("=limit", `local ok = pcall(function() local x = 0; for i = 1, 100000 do x = x + i end end); return ok`)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := s.Load(proto)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Call(vm.ClosureValue(cl))
		if !errors.Is(err, vm.ErrInstructionLimit) {
			t.Fatalf("expected ErrInstructionLimit, got %v", err)
		}
	})

	// 2. Context cancellation must remain uncatchable when JIT is enabled.
	t.Run("context_cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s := vm.NewState()
		defer s.Close()
		s.SetJITEnabled(true)
		s.SetContext(ctx)
		proto, err := compiler.Compile("=ctx", `local x = 0; for i = 1, 100000 do x = x + i end; return x`)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := s.Load(proto)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Call(vm.ClosureValue(cl))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})

	// 3. load() + collectgarbage() proto churn must invalidate and free old JIT regions cleanly.
	t.Run("proto_gc_churn", func(t *testing.T) {
		src := `
local total = 0
for i = 1, 80 do
  local fn = assert(load("local s = 0; for k = 1, 10 do s = s + k + " .. i .. " end; return s"))
  total = total + fn()
  if i % 10 == 0 then collectgarbage("collect") end
end
return total
`
		want, err := runLuaProgram(t, src, false)
		if err != nil {
			t.Fatal(err)
		}
		got, err := runLuaProgram(t, src, true)
		if err != nil {
			t.Fatal(err)
		}
		if want != got {
			t.Fatalf("proto GC churn mismatch: want %s, got %s", want, got)
		}
	})

	// 4. Live mid-loop context cancellation (not pre-cancelled) must interrupt native loops
	// promptly for both noJIT (Plan 9 asm) and JIT without falling back to runHooked.
	for _, useJIT := range []bool{false, true} {
		mode := "noJIT"
		if useJIT {
			mode = "JIT"
		}
		t.Run("live_ctx_cancel_"+mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			s := vm.NewState()
			defer s.Close()
			if useJIT {
				s.SetJITEnabled(true)
			}
			s.SetContext(ctx)
			proto, err := compiler.Compile("=live_ctx", `local x = 0; while true do x = x + 1 end; return x`)
			if err != nil {
				t.Fatal(err)
			}
			cl, err := s.Load(proto)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Call(vm.ClosureValue(cl))
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s: expected DeadlineExceeded, got %v", mode, err)
			}
		})

		t.Run("live_intr_flag_pcall_"+mode, func(t *testing.T) {
			var flag atomic.Bool
			s := vm.NewState()
			defer s.Close()
			if useJIT {
				s.SetJITEnabled(true)
			}
			s.SetInterruptFlag(&flag)
			go func() {
				time.Sleep(15 * time.Millisecond)
				flag.Store(true)
			}()
			proto, err := compiler.Compile("=live_intr", `
local ok, err = pcall(function()
  local x = 0
  while true do x = x + 1 end
end)
return ok, err
`)
			if err != nil {
				t.Fatal(err)
			}
			cl, err := s.Load(proto)
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Call(vm.ClosureValue(cl))
			if err != nil {
				t.Fatalf("%s: unexpected uncaught error: %v", mode, err)
			}
			if len(res) < 2 || !res[0].IsFalsy() || !strings.Contains(s.FormatValue(res[1]), "interrupted!") {
				t.Fatalf("%s: expected (false, 'interrupted!'), got %v", mode, res)
			}
		})
	}
}

func TestJITMatchesBenchScripts(t *testing.T) {
	if !vm.JITSupported() {
		t.Skip("JIT unsupported in this build configuration")
	}
	scripts := []string{
		"sieve.lua",
		"matmul.lua",
		"fannkuch.lua",
		"mandelbrot.lua",
		"deltablue.lua",
		"sort_int.lua",
		"ecs_stdlib.lua",
		"spectralnorm.lua",
	}
	if !testing.Short() {
		scripts = append(scripts, "binarytrees.lua", "fib.lua", "forloop_int.lua", "nbody.lua", "richards.lua")
	}
	for _, name := range scripts {
		t.Run(name, func(t *testing.T) {
			srcBytes, err := os.ReadFile(filepath.Join("..", "bench", "scripts", name))
			if err != nil {
				t.Fatal(err)
			}
			src := string(srcBytes)
			want, wantErr := runLuaProgram(t, src, false)
			got, gotErr := runLuaProgram(t, src, true)
			if wantErr != nil || gotErr != nil {
				t.Fatalf("%s errors: interp=%v jit=%v", name, wantErr, gotErr)
			}
			if want != got {
				t.Fatalf("%s output mismatch:\ninterp: %q\njit:    %q", name, want, got)
			}
		})
	}
}

func runOfficialTestSourceJIT(t *testing.T, dir, file, src string) (bool, string, string) {
	t.Helper()
	tmpDir := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	proto, err := compiler.Compile("@"+file, src, compiler.SkipFileHeader(true))
	if err != nil {
		t.Fatalf("compile %s: %v", file, err)
	}

	s := vm.NewState()
	defer s.Close()
	s.SetJITEnabled(true)
	if !s.JITEnabled() {
		t.Skip("JIT unavailable at runtime on this host")
	}
	var out bytes.Buffer
	s.SetOutput(&out)
	s.SetErrOutput(&out)

	s.SetGlobal("_U", vm.TrueValue())
	s.SetGlobal("_soft", vm.TrueValue())
	s.SetGlobal("_port", vm.TrueValue())
	s.SetGlobal("_nomsg", vm.TrueValue())
	pkg := s.AsTable(s.GetGlobal("package"))
	pkg.RawSetStr(s, s.Intern("path"), vm.StringValue(s.Intern(filepath.Join(dir, "?.lua")+";./?.lua")))

	cl, err := s.Load(proto)
	if err != nil {
		t.Fatalf("load %s: %v", file, err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s (JIT) PANICKED: %v\n%s\n--- output tail ---\n%s", file, r, string(debug.Stack()), tailString(out.String(), 800))
		}
	}()
	s.SetGlobal("__moelua_test_chunk", vm.ClosureValue(cl))
	wrapProto, err := compiler.Compile("=harness", `local ok, err = xpcall(__moelua_test_chunk, debug.traceback); return ok, err`)
	if err != nil {
		t.Fatalf("compile harness: %v", err)
	}
	wrapCl, err := s.Load(wrapProto)
	if err != nil {
		t.Fatalf("load harness: %v", err)
	}
	res, err := s.Call(vm.ClosureValue(wrapCl))
	if err != nil {
		return false, err.Error(), out.String()
	}
	if len(res) == 0 || res[0].IsFalsy() {
		var msg string
		if len(res) > 1 {
			msg = s.FormatValue(res[1])
		}
		return false, msg, out.String()
	}
	return true, "", out.String()
}

func TestOfficialLua55TestesJIT(t *testing.T) {
	if !vm.JITSupported() {
		t.Skip("JIT unsupported in this build configuration")
	}
	dir := testesDir(t)
	for _, f := range portableOfficialTestesFiles {
		t.Run(f, func(t *testing.T) {
			srcBytes, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			ok, msg, out := runOfficialTestSourceJIT(t, dir, f, string(srcBytes))
			if !ok {
				t.Fatalf("%s (JIT) failed: %s\n--- output tail ---\n%s", f, msg, tailString(out, 1500))
			}
		})
	}
	t.Run("files.lua", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("files.lua exercises POSIX devices and shell utilities")
		}
		srcBytes, err := os.ReadFile(filepath.Join(dir, "files.lua"))
		if err != nil {
			t.Fatal(err)
		}
		src := string(srcBytes)
		if hasWritableDevFull() {
			ok, msg, out := runOfficialTestSourceJIT(t, dir, "files.lua", src)
			if !ok {
				t.Fatalf("files.lua (JIT) failed: %s\n--- output tail ---\n%s", msg, tailString(out, 1500))
			}
			return
		}
		const devFullBlock = `  local f = io.output("/dev/full")
  assert(f:write("abcd"))   -- write to buffer
  assert(not f:flush())     -- cannot write to device
  assert(f:write("abcd"))   -- write to buffer
  assert(not io.flush())    -- cannot write to device
  assert(f:close())`
		patched := strings.Replace(src, devFullBlock, "  -- /dev/full unavailable on host OS", 1)
		ok, msg, out := runOfficialTestSourceJIT(t, dir, "files.lua", patched)
		if !ok {
			t.Fatalf("files.lua (JIT) failed: %s\n--- output tail ---\n%s", msg, tailString(out, 1500))
		}
	})
	t.Run("big.lua", func(t *testing.T) {
		bigPath := filepath.Join(dir, "big.lua")
		src := fmt.Sprintf(`
			_soft = nil
			local fn, err = loadfile(%q)
			assert(fn, err)
			local co = coroutine.create(fn)
			local ok1, v1 = coroutine.resume(co)
			assert(ok1, tostring(v1))
			assert(v1 == 'b', "expected yield 'b', got " .. tostring(v1))
			local ok2, v2 = coroutine.resume(co)
			assert(ok2, tostring(v2))
			assert(v2 == 'a', "expected return 'a', got " .. tostring(v2))
		`, bigPath)
		ok, msg, out := runOfficialTestSourceJIT(t, dir, "big_driver.lua", src)
		if !ok {
			t.Fatalf("big.lua (JIT) failed: %s\n--- output tail ---\n%s", msg, tailString(out, 1500))
		}
	})
}
