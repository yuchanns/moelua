package compiler_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
)

func lua55TestesDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("LUA55_TESTES")
	if dir == "" {
		dir = filepath.Join("..", "third_party", "lua", "testes")
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if _, err := os.Stat(filepath.Join(dir, "all.lua")); err != nil {
		t.Skipf("Lua 5.5.1 testes not found at %s (run `git submodule update --init` or set LUA55_TESTES): %v", dir, err)
	}
	return dir
}

func lua55Bin(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "build", "lua55_o2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("lua55_o2 not found at %s: %v", p, err)
	}
	return p
}

func luac55Bin(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "build", "luac55"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("luac55 not found at %s: %v", p, err)
	}
	return p
}

func compileRefWithLuac55(t *testing.T, src string) *bytecode.Proto {
	t.Helper()
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.lua")
	outPath := filepath.Join(dir, "out.luac")
	if err := os.WriteFile(inPath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(luac55Bin(t), "-o", outPath, inPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("luac55 failed: %v\n%s", err, string(out))
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	p, err := bytecode.Undump(data)
	if err != nil {
		t.Fatalf("Undump luac55 output failed: %v", err)
	}
	return p
}

func runCompiledInLua55(t *testing.T, proto *bytecode.Proto, strip bool, workDir string) string {
	t.Helper()
	dumped, err := bytecode.Dump(proto, strip)
	if err != nil {
		t.Fatalf("Dump(strip=%v) failed: %v", strip, err)
	}
	chunkFile := filepath.Join(t.TempDir(), "chunk.luac")
	if err := os.WriteFile(chunkFile, dumped, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(lua55Bin(t), "-e", `
		local f = assert(io.open(os.getenv("MOELUA_CHUNK"), "rb"))
		local data = f:read("a")
		f:close()
		assert(load(data))()
	`)
	cmd.Env = append(os.Environ(), "MOELUA_CHUNK="+chunkFile)
	if workDir != "" {
		cmd.Dir = workDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lua55_o2 executing compiled chunk failed: %v\n%s", err, string(out))
	}
	return string(out)
}

func loadCheckInLua55(t *testing.T, proto *bytecode.Proto) {
	t.Helper()
	dumped, err := bytecode.Dump(proto, false)
	if err != nil {
		t.Fatalf("Dump failed: %v", err)
	}
	if _, err := bytecode.Undump(dumped); err != nil {
		t.Fatalf("Undump round-trip failed: %v", err)
	}
	chunkFile := filepath.Join(t.TempDir(), "chunk.luac")
	if err := os.WriteFile(chunkFile, dumped, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(lua55Bin(t), "-e", `
		local f = assert(io.open(os.getenv("MOELUA_CHUNK"), "rb"))
		local data = f:read("a")
		f:close()
		assert(type(assert(load(data))) == "function")
	`)
	cmd.Env = append(os.Environ(), "MOELUA_CHUNK="+chunkFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("lua55_o2 load() failed: %v\n%s", err, string(out))
	}
}

func compareProtos(t *testing.T, path string, got, want *bytecode.Proto) {
	t.Helper()
	if got.NumParams != want.NumParams || got.Flag != want.Flag || got.MaxStackSize != want.MaxStackSize {
		t.Errorf("%s: header mismatch: got (params=%d flag=%d stack=%d), want (params=%d flag=%d stack=%d)",
			path, got.NumParams, got.Flag, got.MaxStackSize, want.NumParams, want.Flag, want.MaxStackSize)
	}
	if got.LineDefined != want.LineDefined || got.LastLineDefined != want.LastLineDefined {
		t.Errorf("%s: lines mismatch: got (%d..%d), want (%d..%d)",
			path, got.LineDefined, got.LastLineDefined, want.LineDefined, want.LastLineDefined)
	}
	if len(got.Code) != len(want.Code) {
		t.Errorf("%s: code len mismatch: got %d, want %d\ngot:\n%s\nwant:\n%s",
			path, len(got.Code), len(want.Code), disasm(got), disasm(want))
	} else {
		for i := range got.Code {
			if got.Code[i] != want.Code[i] {
				t.Errorf("%s: pc=%d inst mismatch: got %s (0x%08x), want %s (0x%08x)\ngot:\n%s\nwant:\n%s",
					path, i, bytecode.OpNames[got.Code[i].Op()], uint32(got.Code[i]),
					bytecode.OpNames[want.Code[i].Op()], uint32(want.Code[i]),
					disasm(got), disasm(want))
				break
			}
		}
	}
	if len(got.Consts) != len(want.Consts) {
		t.Errorf("%s: consts len mismatch: got %d (%+v), want %d (%+v)",
			path, len(got.Consts), got.Consts, len(want.Consts), want.Consts)
	} else {
		for i := range got.Consts {
			gc, wc := got.Consts[i], want.Consts[i]
			if gc.Kind != wc.Kind || gc.Int != wc.Int || gc.Str != wc.Str || gc.IsShort != wc.IsShort ||
				(gc.Kind == bytecode.ConstFloat && math.Float64bits(gc.Float) != math.Float64bits(wc.Float)) {
				t.Errorf("%s: const[%d] mismatch: got %+v, want %+v", path, i, gc, wc)
			}
		}
	}
	if len(got.Upvalues) != len(want.Upvalues) {
		t.Errorf("%s: upvalues len mismatch: got %d, want %d", path, len(got.Upvalues), len(want.Upvalues))
	} else {
		for i := range got.Upvalues {
			if got.Upvalues[i] != want.Upvalues[i] {
				t.Errorf("%s: upvalue[%d] mismatch: got %+v, want %+v", path, i, got.Upvalues[i], want.Upvalues[i])
			}
		}
	}
	if len(got.LocVars) != len(want.LocVars) {
		t.Errorf("%s: locvars len mismatch: got %d, want %d", path, len(got.LocVars), len(want.LocVars))
	} else {
		for i := range got.LocVars {
			if got.LocVars[i] != want.LocVars[i] {
				t.Errorf("%s: locvar[%d] mismatch: got %+v, want %+v", path, i, got.LocVars[i], want.LocVars[i])
			}
		}
	}
	if len(got.LineInfo) != len(want.LineInfo) {
		t.Errorf("%s: lineinfo len mismatch: got %d, want %d", path, len(got.LineInfo), len(want.LineInfo))
	} else {
		for i := range got.LineInfo {
			if got.LineInfo[i] != want.LineInfo[i] {
				t.Errorf("%s: lineinfo[%d] mismatch: got %d, want %d", path, i, got.LineInfo[i], want.LineInfo[i])
				break
			}
		}
	}
	if len(got.AbsLineInfo) != len(want.AbsLineInfo) {
		t.Errorf("%s: abslineinfo len mismatch: got %v, want %v", path, got.AbsLineInfo, want.AbsLineInfo)
	} else {
		for i := range got.AbsLineInfo {
			if got.AbsLineInfo[i] != want.AbsLineInfo[i] {
				t.Errorf("%s: abslineinfo[%d] mismatch: got %+v, want %+v", path, i, got.AbsLineInfo[i], want.AbsLineInfo[i])
			}
		}
	}
	if len(got.Protos) != len(want.Protos) {
		t.Errorf("%s: protos len mismatch: got %d, want %d", path, len(got.Protos), len(want.Protos))
		return
	}
	for i := range got.Protos {
		compareProtos(t, fmt.Sprintf("%s.p[%d]", path, i), got.Protos[i], want.Protos[i])
	}
}

func disasm(p *bytecode.Proto) string {
	var b strings.Builder
	for i, inst := range p.Code {
		fmt.Fprintf(&b, "  %3d: %-12s A=%d B=%d C=%d k=%d Bx=%d sJ=%d\n",
			i, bytecode.OpNames[inst.Op()], inst.A(), inst.B(), inst.C(), inst.KBit(), inst.Bx(), inst.SJ())
	}
	return b.String()
}

func TestBenchScriptsCompileVerifyAndExecute(t *testing.T) {
	scripts, err := filepath.Glob(filepath.Join("..", "bench", "scripts", "*.lua"))
	if err != nil {
		t.Fatalf("glob bench scripts: %v", err)
	}
	if len(scripts) == 0 {
		t.Skip("bench/scripts not present")
	}
	if len(scripts) != 13 {
		t.Fatalf("expected 13 bench scripts, found %d", len(scripts))
	}

	for _, script := range scripts {
		name := filepath.Base(script)
		t.Run(name, func(t *testing.T) {
			srcBytes, err := os.ReadFile(script)
			if err != nil {
				t.Fatal(err)
			}
			proto, err := compiler.Compile("@"+script, string(srcBytes))
			if err != nil {
				t.Fatalf("Compile failed: %v", err)
			}
			if err := bytecode.Verify(proto); err != nil {
				t.Fatalf("Verify failed: %v", err)
			}

			// Compare bytecode bit-for-bit against luac55!
			refProto := compileRefWithLuac55(t, string(srcBytes))
			compareProtos(t, name, proto, refProto)

			// Execute our compiled & dumped bytecode in real C lua55_o2 and compare stdout.
			refCmd := exec.Command(lua55Bin(t), script)
			refOut, err := refCmd.CombinedOutput()
			if err != nil {
				t.Fatalf("C lua55_o2 running %s failed: %v\n%s", script, err, string(refOut))
			}

			gotOut := runCompiledInLua55(t, proto, false, "")
			if gotOut != string(refOut) {
				t.Fatalf("stdout mismatch for %s:\ngot:\n%s\nwant:\n%s", name, gotOut, string(refOut))
			}
			gotStrippedOut := runCompiledInLua55(t, proto, true, "")
			if gotStrippedOut != string(refOut) {
				t.Fatalf("stripped stdout mismatch for %s:\ngot:\n%s\nwant:\n%s", name, gotStrippedOut, string(refOut))
			}
		})
	}
}

func TestOfficialLua55TestesCompileAndVerify(t *testing.T) {
	dir := lua55TestesDir(t)
	files, err := filepath.Glob(filepath.Join(dir, "*.lua"))
	if err != nil || len(files) == 0 {
		t.Skipf("Lua 5.5.1 testes not found at %s: %v", dir, err)
	}
	if len(files) != 34 {
		t.Fatalf("expected 34 testes/*.lua files, got %d", len(files))
	}

	for _, file := range files {
		name := filepath.Base(file)
		t.Run(name, func(t *testing.T) {
			srcBytes, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			proto, err := compiler.Compile("@"+file, string(srcBytes))
			if err != nil {
				t.Fatalf("Compile(%s) failed: %v", name, err)
			}
			if err := bytecode.Verify(proto); err != nil {
				t.Fatalf("Verify(%s) failed: %v", name, err)
			}
			// Verify dumped chunk loads cleanly in both Go Undump and C lua55_o2!
			loadCheckInLua55(t, proto)

			// Compare compiled bytecode directly against luac55!
			refOut := filepath.Join(t.TempDir(), "ref.luac")
			cmd := exec.Command(luac55Bin(t), "-o", refOut, file)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("luac55 %s failed: %v\n%s", name, err, string(out))
			}
			refData, err := os.ReadFile(refOut)
			if err != nil {
				t.Fatal(err)
			}
			refProto, err := bytecode.Undump(refData)
			if err != nil {
				t.Fatalf("Undump ref %s failed: %v", name, err)
			}
			compareProtos(t, name, proto, refProto)

			// Verify full Compile + Dump binary output matches luac55 byte-for-byte!
			dumped, err := bytecode.Dump(proto, false)
			if err != nil {
				t.Fatalf("Dump %s failed: %v", name, err)
			}
			if !bytes.Equal(dumped, refData) {
				t.Fatalf("%s: Compile+Dump binary chunk differs from luac55 (%d vs %d bytes)",
					name, len(dumped), len(refData))
			}
		})
	}
}

func TestExecuteCompiledOfficialTestesInLua55(t *testing.T) {
	dir := lua55TestesDir(t)
	// Run standalone Lua 5.5.1 official test suite files compiled by our Go compiler inside C lua55_o2!
	runnable := []string{
		"bitwise.lua",
		"closure.lua",
		"constructs.lua",
		"goto.lua",
		"literals.lua",
		"math.lua",
		"sort.lua",
		"tpack.lua",
		"utf8.lua",
		"vararg.lua",
	}
	for _, name := range runnable {
		t.Run(name, func(t *testing.T) {
			full := filepath.Join(dir, name)
			srcBytes, err := os.ReadFile(full)
			if err != nil {
				t.Fatal(err)
			}
			proto, err := compiler.Compile("@"+full, string(srcBytes))
			if err != nil {
				t.Fatalf("Compile(%s) failed: %v", name, err)
			}
			out := runCompiledInLua55(t, proto, false, dir)
			if !strings.Contains(strings.ToUpper(out), "OK") {
				t.Fatalf("%s did not print OK:\n%s", name, out)
			}
		})
	}
}

func TestLua55FeaturesBytecodeExactMatch(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "expressions_and_folding",
			src: `
local k0 <const> = 0
local k3 <const> = 3
local k6 <const> = k3 + (k3 << k0)
local function f(x, y)
  local a = x + 1
  local b = 128 + x
  local c = x - 127
  local d = x * 20
  local e = x / 40
  local g = x // 3
  local h = x % 7
  local i = x ^ 2
  local j = x & 0xff
  local k = 10 | x
  local l = -10 ~ x
  local m = 1 << x
  local n = x << 5
  local o = x >> 3
  local p = -0.0
  local q = 0x1.8p+2 + 0xF0.0
  local r = (1 + 2) * 3 - 4 // 2 + (0xF0 | 0x0F)
  return a, b, c, d, e, g, h, i, j, k, l, m, n, o, p, q, r, k6
end
return f(10, 20)
`,
		},
		{
			name: "short_circuit_and_comparisons",
			src: `
local function f(a, b, c)
  local x = (a and b) or c
  local y = not (a == 1 or b ~= "hi" or a < 10 or 5 <= b or a > 20.0 or a >= -5)
  local z = not not (a == b and b < c and c <= a)
  return x, y, z
end
return f(1, 2, 3)
`,
		},
		{
			name: "loops_numeric_and_generic",
			src: `
local sum = 0
for i = 1, 10 do
  sum = sum + i
end
for i = 10, 1, -2 do
  sum = sum + i
end
local t = {10, 20, 30}
for k, v in ipairs(t) do
  sum = sum + k * v
end
local n = 0
while n < 5 do
  n = n + 1
  if n == 3 then break end
end
repeat
  local captured = n
  local fn = function() return captured end
  n = n + 1
until n >= 6 and fn() > 0
return sum, n
`,
		},
		{
			name: "globals_and_errnnil",
			src: `
global <const> print, assert
global g1, g2 = 10, 20
global <const> gc = 30
global function gfn(x) return x + g1 + g2 + gc end
do
  global *
  implicitVar = gfn(5)
end
return gfn(1)
`,
		},
		{
			name: "compat_global_as_identifier",
			src: `
local global = 10
local t = { global = 20 }
local function f(global)
  return global + t.global
end
global = global + f(5)
return global
`,
		},
		{
			name: "named_vararg_hidden_vs_table",
			src: `
-- Non-escaping named vararg: stays PF_VAHID with OP_GETVARG!
local function fast(a, ...v)
  return a + v[1] + v[2] + v.n
end
-- Escaping via return: PF_VATAB + OP_GETTABLE rewrite + VARARG k=1!
local function escReturn(a, ...v)
  local x = v[1]
  local y = select('#', ...)
  return v, x, y
end
-- Escaping via mutation: PF_VATAB!
local function escMutate(...v)
  local before = v[1]
  v[1] = 99
  return before, v[1], ...
end
-- Escaping via upvalue capture: PF_VATAB!
local function escUpval(...v)
  local x = v[1]
  return function() return v[1], x end
end
local rv, rx, ry = escReturn(1, 2, 3)
return fast(1, 2, 3), rv.n, rv[1], rv[2], rx, ry, escMutate(10, 20), escUpval(42)()
`,
		},
		{
			name: "closures_upvalues_tbc_and_tailcalls",
			src: `
local function makeCounter(start)
  local n = start
  return function(step)
    n = n + step
    return n
  end
end
local function tail(n, acc)
  if n == 0 then return acc end
  return tail(n - 1, acc + n)
end
local function noTailWithClose(f)
  local x <close> = setmetatable({}, {__close = function() end})
  return f()
end
local c = makeCounter(10)
return c(5), tail(10, 0), noTailWithClose(function() return 7 end)
`,
		},
		{
			name: "goto_and_labels_with_close",
			src: `
local function test(a)
  local log = {}
  do
    local x = 1
    local f = function() return x end
    if a == 1 then goto skip end
    local y <close> = setmetatable({}, {__close = function() log[#log+1] = "closed" end})
    log[#log+1] = f()
    if a == 2 then goto skip end
    log[#log+1] = "end"
  end
  ::skip::
  return #log
end
return test(1), test(2), test(3)
`,
		},
		{
			name: "table_constructors_and_concat",
			src: `
local function f() return 10, 20, 30 end
local t1 = {1, 2, 3, a = 4, ["b"] = 5, [100] = 6, f()}
local t2 = {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
local s = "a" .. "b" .. tostring(t1.a) .. tostring(t2[20])
return #t1, #t2, s
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proto, err := compiler.Compile("@"+tc.name+".lua", tc.src)
			if err != nil {
				t.Fatalf("Compile failed: %v", err)
			}
			refProto := compileRefWithLuac55(t, tc.src)
			compareProtos(t, tc.name, proto, refProto)

			// Also verify runtime execution matches!
			srcFile := filepath.Join(t.TempDir(), tc.name+".lua")
			runSrc := tc.src
			if err := os.WriteFile(srcFile, []byte("print((function()\n"+runSrc+"\nend)())"), 0o600); err != nil {
				t.Fatal(err)
			}
			refOut, err := exec.Command(lua55Bin(t), srcFile).CombinedOutput()
			if err != nil {
				t.Fatalf("lua55_o2 failed: %v\n%s", err, string(refOut))
			}
			wrapProto, err := compiler.Compile("@wrap.lua", "print((function()\n"+runSrc+"\nend)())")
			if err != nil {
				t.Fatalf("wrap Compile failed: %v", err)
			}
			gotOut := runCompiledInLua55(t, wrapProto, false, "")
			if gotOut != string(refOut) {
				t.Fatalf("runtime mismatch:\ngot:  %q\nwant: %q", gotOut, string(refOut))
			}
		})
	}
}

func TestNamedVarargFlagsAndRewrite(t *testing.T) {
	// Verify PF_VAHID + OP_GETVARG when vararg table does not escape.
	p1, err := compiler.Compile("vahid", `return function(a, ...t) return t[1], t.n, ... end`)
	if err != nil {
		t.Fatal(err)
	}
	sub1 := p1.Protos[0]
	if sub1.Flag != bytecode.PFVarargHidden {
		t.Fatalf("expected PFVarargHidden (%d), got %d", bytecode.PFVarargHidden, sub1.Flag)
	}
	hasGetVarg := false
	for _, inst := range sub1.Code {
		if inst.Op() == bytecode.OP_GETVARG {
			hasGetVarg = true
		}
		if inst.Op() == bytecode.OP_VARARG && inst.K() {
			t.Fatalf("expected OP_VARARG k=0 for PF_VAHID")
		}
	}
	if !hasGetVarg {
		t.Fatalf("expected OP_GETVARG in non-escaping vararg function")
	}

	// Verify PF_VATAB + OP_GETVARG -> OP_GETTABLE rewrite + OP_VARARG k=1 when vararg escapes.
	p2, err := compiler.Compile("vatab", `return function(a, ...t) local x = t[1]; t[2] = x; return t, ... end`)
	if err != nil {
		t.Fatal(err)
	}
	sub2 := p2.Protos[0]
	if sub2.Flag != bytecode.PFVarargTable {
		t.Fatalf("expected PFVarargTable (%d), got %d", bytecode.PFVarargTable, sub2.Flag)
	}
	hasGetTable := false
	hasVarargK := false
	for _, inst := range sub2.Code {
		if inst.Op() == bytecode.OP_GETVARG {
			t.Fatalf("OP_GETVARG was not rewritten to OP_GETTABLE when PF_VATAB is set")
		}
		if inst.Op() == bytecode.OP_GETTABLE {
			hasGetTable = true
		}
		if inst.Op() == bytecode.OP_VARARG && inst.K() {
			hasVarargK = true
		}
	}
	if !hasGetTable || !hasVarargK {
		t.Fatalf("expected rewritten OP_GETTABLE (%v) and OP_VARARG k=1 (%v)", hasGetTable, hasVarargK)
	}
}

func TestCompatGlobalOption(t *testing.T) {
	// With default CompatGlobal(true), 'global' can be used as a local/field/variable name.
	if _, err := compiler.Compile("compat", `local global = 1; global = global + 1; return global`); err != nil {
		t.Fatalf("expected CompatGlobal(true) to allow 'global' as identifier: %v", err)
	}
	// With CompatGlobal(false), 'global' is a strict reserved keyword.
	if _, err := compiler.Compile("strict", `local global = 1`, compiler.CompatGlobal(false)); err == nil {
		t.Fatalf("expected CompatGlobal(false) to reject 'local global = 1'")
	}
	// Strict mode still parses global declarations properly.
	if _, err := compiler.Compile("strict_ok", `global <const> x = 1; return x`, compiler.CompatGlobal(false)); err != nil {
		t.Fatalf("expected CompatGlobal(false) to accept 'global <const> x = 1': %v", err)
	}
}

func TestLargeTableConstructorExtraArg(t *testing.T) {
	// >1023 list elements forces OP_NEWTABLE k=1 + OP_EXTRAARG and OP_SETLIST k=1 + OP_EXTRAARG.
	var sb strings.Builder
	sb.WriteString("local t = {")
	for i := 1; i <= 1100; i++ {
		if i > 1 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%d", i)
	}
	sb.WriteString("}\nprint(#t, t[1], t[1024], t[1100])\n")
	src := sb.String()

	p, err := compiler.Compile("@large_table.lua", src)
	if err != nil {
		t.Fatalf("Compile large table failed: %v", err)
	}
	refProto := compileRefWithLuac55(t, src)
	compareProtos(t, "large_table", p, refProto)

	out := runCompiledInLua55(t, p, false, "")
	if out != "1100\t1\t1024\t1100\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestLoadKXExtraArg(t *testing.T) {
	// Exceed MaxArgBx (131071) constants so the compiler emits OP_LOADKX + OP_EXTRAARG.
	var sb strings.Builder
	sb.WriteString("local x\n")
	for i := 0; i < 131080; i++ {
		fmt.Fprintf(&sb, "x = %d\n", 100000+i)
	}
	sb.WriteString("print(x)\n")
	src := sb.String()

	p, err := compiler.Compile("@loadkx.lua", src)
	if err != nil {
		t.Fatalf("Compile loadkx failed: %v", err)
	}
	foundLoadKX := false
	for i, inst := range p.Code {
		if inst.Op() == bytecode.OP_LOADKX {
			foundLoadKX = true
			if i+1 >= len(p.Code) || p.Code[i+1].Op() != bytecode.OP_EXTRAARG {
				t.Fatalf("OP_LOADKX at pc=%d not followed by OP_EXTRAARG", i)
			}
			break
		}
	}
	if !foundLoadKX {
		t.Fatalf("expected OP_LOADKX to be emitted for %d constants", len(p.Consts))
	}
	if out := runCompiledInLua55(t, p, false, ""); out != "231079\n" {
		t.Fatalf("unexpected loadkx output: %q", out)
	}
}

func TestLexerBOMShebangAndSyntaxErrors(t *testing.T) {
	// BOM + shebang + exact line number.
	p, err := compiler.Compile("@script.lua", "\xef\xbb\xbf#!/usr/bin/env lua\nreturn require'debug'.getinfo(1).currentline")
	if err != nil {
		t.Fatalf("Compile with BOM+shebang failed: %v", err)
	}
	wrap := &bytes.Buffer{}
	wrap.WriteString("\xef\xbb\xbf#!/usr/bin/env lua\nprint(require'debug'.getinfo(1).currentline)\n")
	p2, err := compiler.Compile("@script.lua", wrap.String())
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	if out := runCompiledInLua55(t, p2, false, ""); out != "2\n" {
		t.Fatalf("expected line 2 after BOM+shebang, got %q", out)
	}
	_ = p

	// Bad BOMs must fail.
	for _, bad := range []string{"\xef", "\xef\xbb", "\xefprint(1)", "\xef\xbbprint(1)"} {
		if _, err := compiler.Compile("@badbom.lua", bad); err == nil {
			t.Fatalf("expected bad BOM %q to fail", bad)
		}
	}

	// Exact error messages & 'near' tokens.
	errCases := []struct {
		src  string
		want string
	}{
		{`return "abc\x"`, `near '"abc\x"'`},
		{`return "\999"`, `near '"\999"'`},
		{`return "\u{100000000}"`, `UTF-8 value too large`},
		{`return [=[unfinished`, `unfinished long string (starting at line 1) near <eof>`},
		{`return 1print()`, `malformed number near '1p'`},
		{`return 0xep-p`, `malformed number near '0xep-p'`},
		{`local x <const> = 1; x = 2`, `attempt to assign to const variable 'x'`},
		{`for i = 1, 10 do i = 2 end`, `attempt to assign to const variable 'i'`},
		{`return function(...t) t = 1 end`, `attempt to assign to const variable 't'`},
		{`global <const> a; a = 1`, `attempt to assign to const variable 'a'`},
		{`global x; return y`, `variable 'y' not declared`},
		{`global <close> x`, `global variables cannot be to-be-closed`},
		{`goto l1; local a; ::l1:: print(a)`, `<goto l1> at line 1 jumps into the scope of 'a'`},
		{`goto l1; global *; ::l1:: print(1)`, `<goto l1> at line 1 jumps into the scope of '*'`},
		{`goto missing`, `no visible label 'missing' for <goto> at line 1`},
		{`::dup:: ::dup::`, `label 'dup' already defined on line 1`},
		{`break`, `break outside loop`},
		{`return function() return ... end`, `cannot use '...' outside a vararg function`},
		{`local a <unknown> = 1`, `unknown attribute 'unknown'`},
		{`local a <close>, b <close> = 1, 2`, `multiple to-be-closed variables in local list`},
	}

	for _, tc := range errCases {
		_, err := compiler.Compile("test", tc.src)
		if err == nil {
			t.Errorf("expected error for %q, got nil", tc.src)
			continue
		}
		var se *compiler.SyntaxError
		if !errors.As(err, &se) {
			t.Errorf("expected *SyntaxError for %q, got %T: %v", tc.src, err, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("error mismatch for %q:\ngot:  %s\nwant substring: %s", tc.src, err.Error(), tc.want)
		}
	}
}

func TestAdversarialDeepNestingDoesNotOverflowStack(t *testing.T) {
	depth := 500
	cases := []struct {
		name string
		src  string
	}{
		{"parens", "return " + strings.Repeat("(", depth) + "1" + strings.Repeat(")", depth)},
		{"tables", "return " + strings.Repeat("{", depth) + strings.Repeat("}", depth)},
		{"do_blocks", strings.Repeat("do ", depth) + strings.Repeat("end ", depth)},
		{"nested_funcs", "return " + strings.Repeat("function() return ", depth) + "1" + strings.Repeat(" end", depth)},
		{"multi_assign", strings.Repeat("a,", depth) + "a = 1"},
		{"multi_global", "global " + strings.Repeat("a,", depth) + "a = 1"},
		{"unary_ops", "return " + strings.Repeat("not ", depth) + "true"},
		{"many_labels", strings.Repeat("::a:: ", depth)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compiler.Compile("adversarial", tc.src)
			if err == nil {
				t.Fatalf("expected recursion limit error for %s, got nil", tc.name)
			}
			var se *compiler.SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("expected *SyntaxError, got %T: %v", err, err)
			}
		})
	}
}
