// Copyright 2026 The lua-go Authors. All rights reserved.
// Use of the source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package compare compares MoeLua 5.5 against native C Lua 5.5.1 (-O2 / -O3),
// LuaJIT 2.1, and other pure-Go Lua implementations:
//   - github.com/matjam/apogee (Lua 5.5, JIT and interpreter)
//   - modernc.org/lua (CCGO-translated Lua 5.5)
//   - github.com/arnodel/golua (Lua 5.5)
//   - github.com/yuin/gopher-lua (Lua 5.1)
//   - github.com/Shopify/go-lua (Lua 5.2)
//   - github.com/milochristiansen/lua (DCLua, Lua 5.3)
//
// Harness adapted from modernc.org/lua/compare (BSD-3-Clause).
package compare

import (
	"bytes"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	golua "github.com/Shopify/go-lua"
	arnodel "github.com/arnodel/golua/lib"
	arnodelrt "github.com/arnodel/golua/runtime"
	apogeelua "github.com/matjam/apogee/lua"
	apogeestdlib "github.com/matjam/apogee/stdlib"
	dclua "github.com/milochristiansen/lua"
	"github.com/milochristiansen/lua/lmodbase"
	"github.com/milochristiansen/lua/lmodmath"
	"github.com/milochristiansen/lua/lmodpackage"
	"github.com/milochristiansen/lua/lmodstring"
	"github.com/milochristiansen/lua/lmodtable"
	"github.com/milochristiansen/lua/lmodutf8"
	"github.com/yuchanns/moelua"
	gopher "github.com/yuin/gopher-lua"
	"modernc.org/lua"
)

var (
	oEngines  = flag.String("engines", "", "regexp selecting the engines")
	oLua      = flag.String("lua", filepath.Join("..", "..", "build", "lua55_o2"), "native C Lua 5.5.1 -O2")
	oLuaO3    = flag.String("lua-o3", filepath.Join("..", "..", "build", "lua55_o3"), "native C Lua 5.5.1 -O3")
	oLuaJIT   = flag.String("luajit", "luajit", "native LuaJIT 2.1")
	oPrograms = flag.String("programs", "", "regexp selecting the programs")
	oSlow     = flag.Bool("slow", false, "also run the programs skip says are too slow")

	reEngines, rePrograms *regexp.Regexp
)

func TestMain(m *testing.M) {
	flag.Parse()
	reEngines = regexp.MustCompile(*oEngines)
	rePrograms = regexp.MustCompile(*oPrograms)
	os.Exit(m.Run())
}

// program is a benchmark program bundled into one chunk, see bundle.
type program struct {
	name string
	src  string // the bundle
}

func read(name string) string {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		panic(err)
	}

	return string(b)
}

// prelude is the start of every bundle. It adds what Lua 5.1 lacks and 5.2+
// has, or the reverse.
const prelude = `table.unpack = table.unpack or unpack
unpack = unpack or table.unpack
if loadstring then -- Lua 5.1: load takes no string
  local load0 = load
  load = function(s, ...)
    if type(s) == "string" then return loadstring(s, ...) end
    return load0(s, ...)
  end
end
`

// preload returns Lua code making module name, the source src, available to
// require. A module for Lua 5.3+ only, which an older parser rejects, is
// compiled when required.
func preload(name, src string) string {
	if strings.HasSuffix(name, "-53") {
		return fmt.Sprintf("package.preload[%q] = function(...) return assert(load(%s, %[1]q))(...) end\n", name, longString(src))
	}

	return fmt.Sprintf("package.preload[%q] = function(...)\n%s\nend\n", name, src)
}

// longString returns s as a Lua long string, [==[...]==], of a level s does
// not close.
func longString(s string) string {
	eq := ""
	for strings.Contains(s, "]"+eq+"]") {
		eq += "="
	}
	// A newline right after the opening bracket is skipped.
	return "[" + eq + "[\n" + s + "]" + eq + "]"
}

// awfy are the are-we-fast-yet benchmarks, run by their inner_benchmark_loop,
// which runs the benchmark and verifies its result. inner is the inner
// iterations of one op. For CD, Havlak, Mandelbrot and NBody it is a problem
// size, of which only some can be verified; the others run the benchmark
// inner times.
var awfy = []struct {
	name  string
	inner int
}{
	{"Bounce", 1},
	{"CD", 100},
	{"DeltaBlue", 1000},
	{"Havlak", 1},
	{"Json", 1},
	{"List", 1},
	{"Mandelbrot", 500},
	{"NBody", 250000},
	{"Permute", 1},
	{"Queens", 1},
	{"Richards", 1},
	{"Sieve", 1},
	{"Storage", 1},
	{"Towers", 1},
}

func awfyProgram(name string, inner int) program {
	var b strings.Builder
	b.WriteString(prelude)
	for _, v := range []string{"som", "benchmark", "mandelbrot-fn", "mandelbrot-fn-53", "hashindextable", "hashindextable-53", strings.ToLower(name)} {
		b.WriteString(preload(v, read(filepath.Join("awfy", v+".lua"))))
	}
	fmt.Fprintf(&b, `
local bench = require(%q)
local function run(n)
  for _ = 1, n do
    if not bench:inner_benchmark_loop(%d) then error(%[1]q .. ": incorrect result") end
  end
  return "ok"
end
`, strings.ToLower(name), inner)
	b.WriteString(driver)
	return program{name: "awfy/" + name, src: b.String()}
}

// driver ends every bundle. run(n) does n ops and returns the output of the
// last one.
const driver = `
local r = run(N)
if PRINT then io.write(r) end
return r
`

// game are programs of the Benchmarks Game, with the size of one op. They
// get the size as arg[1] and write their output with io.write, which the
// bundle collects.
var game = []struct {
	name string
	size int
}{
	{"binarytrees", 12},
	{"fannkuchredux", 8},
	{"fasta", 25000},
	{"mandelbrot", 200},
	{"nbody", 50000},
	{"spectralnorm", 100},
}

func gameProgram(name string, size int) program {
	var b strings.Builder
	b.WriteString(prelude)
	fmt.Fprintf(&b, `
local function program(arg)
%s
end
local function run(n)
  local out
  local write, tostring, select = io.write, tostring, select
  -- Also the code the program compiles with load writes with io.write.
  io.write = function(...)
    for i = 1, select("#", ...) do out[#out+1] = tostring((select(i, ...))) end
  end
  for _ = 1, n do
    out = {}
    program({%q})
  end
  io.write = write
  return table.concat(out)
end
`, read(filepath.Join("game", name+".lua")), fmt.Sprint(size))
	b.WriteString(driver)
	return program{name: "game/" + name, src: b.String()}
}

// scimark are the kernels of Lua SciMark, with their small sizes, and the
// cycles of one op.
var scimark = []struct {
	name   string
	cycles int
}{
	{"FFT", 64},
	{"SOR", 8},
	{"MC", 100000},
	{"SPARSE", 64},
	{"LU", 4},
}

func scimarkProgram(name string, cycles int) program {
	src := read(filepath.Join("scimark", "scimark.lua"))
	i := strings.Index(src, "-- Main program.")
	if i < 0 {
		panic("scimark.lua: no main program")
	}

	var b strings.Builder
	b.WriteString(prelude)
	// Plain Lua tables everywhere, also for LuaJIT, as scimark's -noffi.
	b.WriteString("package.preload.ffi = nil\n")
	b.WriteString(src[:i])
	fmt.Fprintf(&b, `
local function run(n)
  array_init()
  rand_init(RANDOM_SEED)
  local p = benchmarks.small[%q]
  local kernel = benchmarks[%[1]q](unpack(p))
  local flops = 0
  -- Every op starts from the seed, as every measure of scimark does. MC
  -- checks its result statistically, which one long stream of the
  -- generator scimark uses under LuaJIT's JIT fails.
  for _ = 1, n do
    rand_init(RANDOM_SEED)
    flops = flops + kernel(%d, unpack(p))
  end
  return string.format("%%s %%.0f\n", %[1]q, flops / n)
end
`, name, cycles)
	b.WriteString(driver)
	return program{name: "scimark/" + name, src: b.String()}
}

// micro are loops of a single operation, 100000 of them an op. pcall and
// error measure what ccgo's lowering of setjmp and longjmp, a Go panic and
// recover, costs in luaD_rawrunprotected and luaD_throw.
var micro = []struct{ name, body string }{
	{"call", `local function f() end
for _ = 1, 100000 do f() end`},
	{"pcall", `local pcall = pcall
local function f() end
for _ = 1, 100000 do pcall(f) end`},
	{"error", `local pcall, error = pcall, error
for _ = 1, 100000 do pcall(error, "x") end`},
}

func microProgram(name, body string) program {
	var b strings.Builder
	b.WriteString(prelude)
	fmt.Fprintf(&b, `
local function op()
%s
end
local function run(n)
  for _ = 1, n do op() end
  return "ok"
end
`, body)
	b.WriteString(driver)
	return program{name: "micro/" + name, src: b.String()}
}

// own are the benchmarks the Go engines publish themselves, one op of each:
//
//   - fib35, the recursive fib(35) of GopherLua's wiki page Benchmarks.
//   - fibonacci, BenchmarkFibonnaci of go-lua's vm_test.go, also the fib of
//     DCLua's fib_test.go: an iterative loop, here of 1e6 steps. The sum
//     overflows, as a float before Lua 5.3, so it is not compared.
//   - sort and sort2, BenchmarkSort and BenchmarkSort2 of go-lua's
//     vm_test.go: table.sort of 100000 numbers in [0, 1), the second with a
//     comparison function that counts its calls. The numbers come from the
//     Park-Miller generator, exact in every engine, not math.random.
var own = []struct{ name, body string }{
	{"fib35", `local function fib(n)
  if n < 2 then return n end
  return fib(n - 2) + fib(n - 1)
end
return tostring(fib(35))`},
	{"fibonacci", `local function fib(n)
  if n == 0 then
    return 0
  elseif n == 1 then
    return 1
  end
  local n0, n1 = 0, 1
  for i = n, 2, -1 do
    local tmp = n0 + n1
    n0 = n1
    n1 = tmp
  end
  return n1
end
fib(1000000)
return "ok"`},
	{"sort", sortBody("table.sort(a)")},
	{"sort2", sortBody("i = 0; table.sort(a, function(x,y) i=i+1; return y<x end)")},
}

func sortBody(sort string) string {
	return fmt.Sprintf(`local a, x = {}, 42
for i = 1, 100000 do
  x = x * 16807 %% 2147483647
  a[i] = x / 2147483647
end
%s
local up = a[1] <= a[#a]
for i = 2, #a do
  if (a[i-1] <= a[i]) ~= up and a[i-1] ~= a[i] then error("not sorted") end
end
return "ok"`, sort)
}

func ownProgram(name, body string) program {
	var b strings.Builder
	b.WriteString(prelude)
	fmt.Fprintf(&b, `
local function op()
%s
end
local function run(n)
  local r
  for _ = 1, n do r = op() end
  return r
end
`, body)
	b.WriteString(driver)
	return program{name: "own/" + name, src: b.String()}
}

func programs() (r []program) {
	for _, v := range awfy {
		r = append(r, awfyProgram(v.name, v.inner))
	}
	for _, v := range game {
		r = append(r, gameProgram(v.name, v.size))
	}
	for _, v := range scimark {
		r = append(r, scimarkProgram(v.name, v.cycles))
	}
	for _, v := range own {
		r = append(r, ownProgram(v.name, v.body))
	}
	for _, v := range micro {
		r = append(r, microProgram(v.name, v.body))
	}
	return slices.DeleteFunc(r, func(p program) bool { return !rePrograms.MatchString(p.name) })
}

// engine runs the bundle src with the global N set to n and returns its
// result.
type engine struct {
	name string
	run  func(b *testing.B, src string, n int) (string, error)
}

func ccgo(b *testing.B, src string, n int) (string, error) {
	m, err := lua.NewVM()
	if err != nil {
		return "", err
	}

	defer m.Close()

	if err := m.SetGlobal("N", n); err != nil {
		return "", err
	}

	if b != nil {
		b.ResetTimer()
	}
	r, err := m.Eval(src)
	if err != nil {
		return "", err
	}

	return fmt.Sprint(r[0]), nil
}

// gopherBit returns the functions of a 'bit' module for GopherLua, which has
// neither 'bit' nor 'bit32', and Lua 5.1 no bitwise operators. som.lua and
// hashindextable.lua of are-we-fast-yet require it then. The functions are Go,
// as LuaBitOp's and LuaJIT's are C, with LuaBitOp's semantics: arguments are
// taken modulo 2^32, results are signed 32-bit numbers.
func gopherBit() map[string]gopher.LGFunction {
	arg := func(L *gopher.LState, i int) uint32 {
		return uint32(int64(math.Floor(float64(L.CheckNumber(i)))))
	}
	ret := func(L *gopher.LState, x uint32) int {
		L.Push(gopher.LNumber(int32(x)))
		return 1
	}
	fold := func(f func(a, b uint32) uint32) gopher.LGFunction {
		return func(L *gopher.LState) int {
			r := arg(L, 1)
			for i := 2; i <= L.GetTop(); i++ {
				r = f(r, arg(L, i))
			}
			return ret(L, r)
		}
	}
	return map[string]gopher.LGFunction{
		"tobit": func(L *gopher.LState) int { return ret(L, arg(L, 1)) },
		"bnot":  func(L *gopher.LState) int { return ret(L, ^arg(L, 1)) },
		"band":  fold(func(a, b uint32) uint32 { return a & b }),
		"bor":   fold(func(a, b uint32) uint32 { return a | b }),
		"bxor":  fold(func(a, b uint32) uint32 { return a ^ b }),
		"lshift": func(L *gopher.LState) int {
			return ret(L, arg(L, 1)<<(arg(L, 2)&31))
		},
		"rshift": func(L *gopher.LState) int {
			return ret(L, arg(L, 1)>>(arg(L, 2)&31))
		},
		"arshift": func(L *gopher.LState) int {
			return ret(L, uint32(int32(arg(L, 1))>>(arg(L, 2)&31)))
		},
	}
}

func gopherLua(b *testing.B, src string, n int) (string, error) {
	// The default call stack, 256 frames, is too small for DeltaBlue and
	// Havlak.
	L := gopher.NewState(gopher.Options{CallStackSize: 1 << 16, RegistrySize: 1 << 16, RegistryMaxSize: 1 << 24})
	defer L.Close()

	// require'bit' returns package.loaded.bit.
	L.SetField(L.GetField(L.GetGlobal("package"), "loaded"), "bit", L.SetFuncs(L.NewTable(), gopherBit()))
	L.SetGlobal("N", gopher.LNumber(n))
	if b != nil {
		b.ResetTimer()
	}
	if err := L.DoString(src); err != nil {
		return "", err
	}

	return L.ToString(-1), nil
}

func goLua(b *testing.B, src string, n int) (r string, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("go-lua: %v", e)
		}
	}()

	l := golua.NewState()
	golua.OpenLibraries(l)
	l.PushInteger(n)
	l.SetGlobal("N")
	if b != nil {
		b.ResetTimer()
	}
	if err := golua.DoString(l, src); err != nil {
		return "", err
	}

	r, _ = l.ToString(-1)
	return r, nil
}

// arnodelGolua runs the bundle in github.com/arnodel/golua as its own
// benchmarks do, see its benchmarks/bench_test.go.
func arnodelGolua(b *testing.B, src string, n int) (string, error) {
	r := arnodelrt.New(os.Stdout)
	defer arnodel.LoadAll(r)()

	// golua's _VERSION is "Golua 5.5", which sorts before "Lua 5.3", the
	// version are-we-fast-yet and the prelude compare it with to choose
	// between the bitwise operators and a bit library. golua's own
	// benchmarks set it the same way.
	r.SetEnv(r.GlobalEnv(), "_VERSION", arnodelrt.StringValue("Lua 5.5"))
	r.SetEnv(r.GlobalEnv(), "N", arnodelrt.IntValue(int64(n)))
	if b != nil {
		b.ResetTimer()
	}
	chunk, err := r.CompileAndLoadLuaChunk("bundle", []byte(src), arnodelrt.TableValue(r.GlobalEnv()))
	if err != nil {
		return "", err
	}

	v, err := arnodelrt.Call1(r.MainThread(), arnodelrt.FunctionValue(chunk))
	if err != nil {
		return "", err
	}

	s, _ := v.ToString()
	return s, nil
}

// dcLua runs the bundle in github.com/milochristiansen/lua, DCLua, with all
// of its standard modules.
func dcLua(b *testing.B, src string, n int) (r string, err error) {
	l := dclua.NewState()
	if err := l.Protect(func() {
		for _, f := range []dclua.NativeFunction{lmodbase.Open, lmodpackage.Open, lmodstring.Open, lmodtable.Open, lmodmath.Open, lmodutf8.Open} {
			l.Push(f)
			l.Call(0, 0)
		}
		// DCLua has no io and os modules, by design. The programs need only
		// io.write, which the game bundles replace while they run, and
		// os.clock, which scimark reads when it loads, so stubs do.
		for _, v := range []struct {
			module, name string
			f            dclua.NativeFunction
		}{
			{"io", "write", func(l *dclua.State) int { return 0 }},
			{"os", "clock", func(l *dclua.State) int { l.Push(0.0); return 1 }},
		} {
			l.NewTable(0, 1)
			l.Push(v.name)
			l.Push(v.f)
			l.SetTableRaw(-3)
			l.SetGlobal(v.module)
		}
		l.Push(int64(n))
		l.SetGlobal("N")
	}); err != nil {
		return "", err
	}

	if b != nil {
		b.ResetTimer()
	}
	if err := l.LoadText(strings.NewReader(src), "bundle", 0); err != nil {
		return "", err
	}

	if err := l.PCall(0, 1); err != nil {
		return "", err
	}

	return l.ToString(-1), nil
}

// process runs the bundle in a native engine.
func process(bin string, args ...string) func(b *testing.B, src string, n int) (string, error) {
	return func(b *testing.B, src string, n int) (string, error) {
		fn := filepath.Join(os.TempDir(), fmt.Sprintf("lua-compare-%d.lua", os.Getpid()))
		if err := os.WriteFile(fn, []byte(src), 0o600); err != nil {
			return "", err
		}

		defer os.Remove(fn)

		cmd := exec.Command(bin, append(args, "-e", fmt.Sprintf("N=%d PRINT=true", n), fn)...)
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if b != nil {
			b.ResetTimer()
		}
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("%v: %s", err, stderr.Bytes())
		}

		return out.String(), nil
	}
}

// skip are the programs an engine is not run on, and why.
var skip = map[string]string{
	// C Lua takes about 5.6 s an op, go-lua more than 89 min.
	"GoLua awfy/Havlak": "too slow",
	// DCLua's string.format %d rejects a float with an integer value, which
	// Lua 5.3 accepts: binarytrees prints 2^k so.
	"DCLua game/binarytrees": "wrong output: string.format %d of a float",
}

// skipped returns why engine is not run on program, or "".
func skipped(engine, program string) string {
	why := skip[engine+" "+program]
	if why == "too slow" && *oSlow {
		return ""
	}

	return why
}

func moeLuaRun(opts ...moelua.Option) func(b *testing.B, src string, n int) (string, error) {
	return func(b *testing.B, src string, n int) (string, error) {
		s := moelua.New(opts...)
		defer s.Close()
		s.SetGlobal("N", moelua.Int(int64(n)))
		if b != nil {
			b.ResetTimer()
		}
		rets, err := s.DoString(src)
		if err != nil {
			return "", err
		}
		if len(rets) == 0 {
			return "", nil
		}
		return s.FormatValue(rets[0]), nil
	}
}

func apogeeRun(opts ...apogeelua.Option) func(b *testing.B, src string, n int) (string, error) {
	return func(b *testing.B, src string, n int) (string, error) {
		l := apogeelua.NewState(opts...)
		defer l.Close()
		apogeestdlib.Open(l)
		l.PushInteger(n)
		l.SetGlobal("N")
		if b != nil {
			b.ResetTimer()
		}
		if err := l.LoadString(src); err != nil {
			return "", err
		}
		if err := l.ProtectedCall(0, 1, 0); err != nil {
			return "", err
		}
		r, _ := l.ToString(-1)
		return r, nil
	}
}

func engines() []engine {
	return slices.DeleteFunc([]engine{
		{"C", process(*oLua)},
		{"C-O3", process(*oLuaO3)},
		{"MoeLua-JIT", moeLuaRun(moelua.WithJIT())},
		{"MoeLua", moeLuaRun()},
		{"Apogee-JIT", apogeeRun()},
		{"Apogee-Interp", apogeeRun(apogeelua.WithoutJIT())},
		{"CCGO", ccgo},
		{"LuaJIT", process(*oLuaJIT)},
		{"LuaJIT-joff", process(*oLuaJIT, "-joff")},
		{"GopherLua", gopherLua},
		{"GoLua", goLua},
		{"ArnodelGolua", arnodelGolua},
		{"DCLua", dcLua},
	}, func(e engine) bool { return !reEngines.MatchString(e.name) })
}

// TestOutputs runs every program once on every engine and checks that the
// output equals that of C Lua.
func TestOutputs(t *testing.T) {
	if _, err := os.Stat(*oLua); err != nil {
		if _, lookErr := exec.LookPath(*oLua); lookErr != nil {
			t.Skipf("reference C Lua binary %q not found (run 'make build/lua55_o2' at repo root): %v", *oLua, err)
		}
	}
	for _, p := range programs() {
		t.Run(p.name, func(t *testing.T) {
			ref, err := process(*oLua)(nil, p.src, 1)
			if err != nil {
				t.Fatalf("C: %v", err)
			}

			for _, e := range engines() {
				if e.name == "C" {
					continue
				}

				t.Run(e.name, func(t *testing.T) {
					if why := skipped(e.name, p.name); why != "" {
						t.Skip(why)
					}

					got, err := e.run(nil, p.src, 1)
					if err != nil {
						t.Fatal(err)
					}

					if got != ref {
						t.Fatalf("output differs:\n got %.200q\nwant %.200q", got, ref)
					}
				})
			}
		})
	}
}

func Benchmark(b *testing.B) {
	for _, e := range engines() {
		b.Run(e.name, func(b *testing.B) {
			for _, p := range programs() {
				b.Run(p.name, func(b *testing.B) {
					if why := skipped(e.name, p.name); why != "" {
						// Printed for benchreport, as go test -bench does not
						// print why a benchmark is skipped without -v.
						fmt.Printf("--- SKIP: %s: %s\n", b.Name(), why)
						b.Skip(why)
					}

					if _, err := e.run(b, p.src, b.N); err != nil {
						b.Fatal(err)
					}
				})
			}
		})
	}
}
