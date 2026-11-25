package vm_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
	"github.com/yuchanns/moelua/vm"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}

func hasCLua55(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(repoRoot(t), "build", "lua55_o2"))
	return err == nil
}

func compileLua55(t *testing.T, src string) []byte {
	t.Helper()
	root := repoRoot(t)
	luac := filepath.Join(root, "build", "luac55")
	if _, err := os.Stat(luac); err != nil {
		p, cerr := compiler.Compile("=test", src)
		if cerr != nil {
			t.Fatalf("pure-Go compiler failed: %v", cerr)
		}
		b, derr := bytecode.Dump(p, false)
		if derr != nil {
			t.Fatalf("bytecode.Dump failed: %v", derr)
		}
		return b
	}
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.lua")
	outPath := filepath.Join(dir, "out.luac")
	if err := os.WriteFile(inPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(luac, "-o", outPath, inPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("luac55 failed: %v\n%s", err, out)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func runCLua55(t *testing.T, src string) string {
	t.Helper()
	root := repoRoot(t)
	luaBin := filepath.Join(root, "build", "lua55_o2")
	if _, err := os.Stat(luaBin); err != nil {
		t.Skipf("lua55_o2 missing at %s (run `make tools`): %v", luaBin, err)
	}
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.lua")
	if err := os.WriteFile(inPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(luaBin, inPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lua55_o2 failed: %v\n%s", err, out)
	}
	return string(out)
}

func runMoeLua(t *testing.T, src string) string {
	t.Helper()
	chunk := compileLua55(t, src)
	s := vm.NewState()
	var buf bytes.Buffer
	s.SetOutput(&buf)
	cl, err := s.UndumpAndLoad(chunk)
	if err != nil {
		t.Fatalf("UndumpAndLoad failed: %v", err)
	}
	if _, err := s.Call(vm.ClosureValue(cl)); err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	return buf.String()
}

func TestDifferentialLua55(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "arithmetic_bitwise_loops",
			src: `
local sum = 0
for i = 1, 100 do
  sum = sum + (i * 3 - i // 2 + i % 7)
end
local fsum = 0.0
for x = 0.5, 5.0, 0.5 do
  fsum = fsum + x * 1.5
end
local bits = (0xdeadbeef ~ 0x12345678) & 0xffffffff
local big = math.maxinteger - 5
local wrap = math.maxinteger + 1
print(sum, fsum, bits, (1 << 62), (big >> 3), wrap == math.mininteger)
`,
		},
		{
			name: "oop_metatable_ic_invalidation",
			src: `
local Vec = {}
Vec.__index = Vec
function Vec.new(x, y)
  return setmetatable({x = x, y = y}, Vec)
end
function Vec:norm2()
  return self.x * self.x + self.y * self.y
end
function Vec:add(other)
  return Vec.new(self.x + other.x, self.y + other.y)
end
local a = Vec.new(3, 4)
local b = Vec.new(1, 2)
local c = a:add(b)
print(a:norm2(), c:norm2())
-- Dynamically mutate method on prototype: IC must invalidate via protoEpoch!
function Vec:norm2()
  return self.x + self.y
end
print(a:norm2(), c:norm2())
-- Shadow method on instance: Shape transition must invalidate IC!
a.norm2 = function(self) return 999 end
print(a:norm2(), c:norm2())
`,
		},
		{
			name: "lua55_features_global_vararg_tablecreate",
			src: `
global *
global G_VAL = 42
global function addG(x)
  return x + G_VAL
end
local function sumv(...t)
  local s = 0
  for i = 1, t.n do
    s = s + t[i]
  end
  return s, t.n
end
local function sumv_escape(...t)
  local function get(i) return t[i] end
  return get(1) + get(2) + t.n
end
local arr = table.create(10, 0)
for i = 1, 10 do arr[i] = i * i end
local total = 0
for i, v in ipairs(arr) do
  total = total + i + v
end
print(addG(8), sumv(10, 20, 30), sumv_escape(5, 7), #arr, total)
print(tostring(1.1), tostring(1.0), tostring(1/3))
`,
		},
		{
			name: "closures_tailcall_pcall_sort",
			src: `
local function makeCounter(start)
  local n = start
  return function(step)
    n = n + (step or 1)
    return n
  end
end
local c1 = makeCounter(10)
local c2 = makeCounter(100)
print(c1(), c1(5), c2(2), c1())

local function sumTail(n, acc)
  if n == 0 then return acc end
  return sumTail(n - 1, acc + n)
end
print(sumTail(50000, 0))

local ok1, v1 = pcall(function() return 7, 8 end)
local ok2, e2 = pcall(function() error("boom") end)
print(ok1, v1, ok2, e2 ~= nil)

local t = {9, 3, 7, 1, 8, 2, 6, 4, 5}
table.sort(t)
print(table.concat(t, ","))
`,
		},
		{
			name: "typed_array_transitions_and_holes",
			src: `
local a = {}
for i = 1, 5 do a[i] = i * 10 end
a[3] = 3.5 -- int -> generic promotion
a[6] = "hi"
print(a[1], a[3], a[5], a[6], #a)

local f = table.create(4)
for i = 1, 4 do f[i] = i * 0.5 end
f[4] = nil -- pop tail stays float
print(#f, f[1], f[3], f[4])
`,
		},
	}

	t.Logf("Running differential tests in mode=%s polarity=%s", vm.ModeName, vm.PolarityName)
	haveC := hasCLua55(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runMoeLua(t, tc.src)
			if got == "" {
				t.Fatalf("expected non-empty output from MoeLua")
			}
			if !haveC {
				return
			}
			want := runCLua55(t, tc.src)
			if got != want {
				t.Fatalf("stdout mismatch vs C Lua 5.5.1:\n--- want (C) ---\n%s--- got (MoeLua) ---\n%s", want, got)
			}
		})
	}
}
