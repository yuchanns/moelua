package ir_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
	"github.com/yuchanns/moelua/internal/jit/ir"
)

func TestOptimizeProtoMatMulVectorizes(t *testing.T) {
	src := `
local N = 160
local A, B, C = {}, {}, {}
for i = 1, N*N do A[i] = 1.0; B[i] = 2.0; C[i] = 0.0 end
for i = 0, N - 1 do
  local rowA = i * N
  for k = 0, N - 1 do
    local aik = A[rowA + k + 1]
    local rowB = k * N
    for j = 1, N do
      C[rowA + j] = C[rowA + j] + aik * B[rowB + j]
    end
  end
end
local sum = 0.0
for i = 1, N*N do sum = sum + C[i] end
return sum
`
	p, err := compiler.Compile("=matmul", src)
	if err != nil {
		t.Fatal(err)
	}
	regs := ir.OptimizeProto(p, &ir.Feedback{EnableSIMD: true}, 12, 16, 16)
	if len(regs) < 3 {
		t.Fatalf("expected at least 3 optimized inner loops in matmul (init, DAXPY, sum), got %d", len(regs))
	}
	vecCount := 0
	for _, r := range regs {
		if r.Vec != nil {
			vecCount++
		}
	}
	if vecCount < 2 {
		t.Fatalf("expected both DAXPY inner loop and reduction loop to vectorize, got %d vectorized", vecCount)
	}
}

func TestOptimizeProtoSORRejectsDependentSIMD(t *testing.T) {
	src := `
local function sor(v, vp, vn, n, om4, om1)
  for x = 2, n do
    v[x] = om4 * ((vp[x] + vn[x]) + (v[x - 1] + v[x + 1])) + om1 * v[x]
  end
end
return sor
`
	p, err := compiler.Compile("=sor", src)
	if err != nil {
		t.Fatal(err)
	}
	regs := ir.OptimizeProto(p.Protos[0], &ir.Feedback{EnableSIMD: true}, 12, 16, 16)
	if len(regs) != 1 {
		t.Fatalf("expected 1 optimized scalar loop for SOR, got %d", len(regs))
	}
	for _, r := range regs {
		if r.Vec != nil {
			t.Fatalf("SOR has loop-carried v[x-1] dependence and must NOT vectorize")
		}
	}
}

func TestOptimizeProtoMandelbrotMultiBlockWhile(t *testing.T) {
	src := `
local function mandel(cr, ci)
  local zr, zrzr, zi, zizi = 0.0, 0.0, 0.0, 0.0
  local z = 0
  local not_done = true
  local escape = 0
  while not_done and z < 50 do
    local nzr = zrzr - zizi + cr
    zi = 2.0 * zr * zi + ci
    zr = nzr
    zrzr = zr * zr
    zizi = zi * zi
    if zrzr + zizi > 4.0 then
      not_done = false
      escape = 1
    end
    z = z + 1
  end
  return escape
end
return mandel
`
	p, err := compiler.Compile("=mandel", src)
	if err != nil {
		t.Fatal(err)
	}
	regs := ir.OptimizeProto(p.Protos[0], &ir.Feedback{EnableSIMD: true}, 12, 16, 16)
	if len(regs) != 1 {
		t.Fatalf("expected 1 optimized multi-block while loop for mandelbrot, got %d", len(regs))
	}
	for _, r := range regs {
		if r.Kind != ir.LoopWhile || len(r.Blocks) < 2 {
			t.Fatalf("expected multi-block LoopWhile, got %s", r)
		}
	}
}

func TestOptimizeProtoSpectralNormLeafInline(t *testing.T) {
	src := `
local function A(i, j)
  local ij = i + j - 1
  return 1.0 / (ij * (ij - 1) * 0.5 + i)
end
local function Av(x, y, N)
  for i = 1, N do
    local a = 0.0
    for j = 1, N do a = a + x[j] * A(i, j) end
    y[i] = a
  end
end
return Av
`
	p, err := compiler.Compile("=spec", src)
	if err != nil {
		t.Fatal(err)
	}
	ap := p.Protos[0]
	avp := p.Protos[1]
	callPC := -1
	for pc, w := range avp.Code {
		if w.Op() == bytecode.OP_CALL {
			callPC = pc
			break
		}
	}
	if callPC < 0 {
		t.Fatal("could not find CALL in Av")
	}
	fb := &ir.Feedback{
		EnableSIMD: true,
		InlineCalls: map[int]*ir.InlineTarget{
			callPC: {ProtoIdx: 7, Proto: ap, NumParams: 2},
		},
	}
	regs := ir.OptimizeProto(avp, fb, 12, 16, 16)
	if len(regs) != 1 {
		t.Fatalf("expected 1 optimized loop in Av with inlined A(i,j), got %d", len(regs))
	}
}

func TestOptimizeBenchAndSciMarkCoverage(t *testing.T) {
	countOpt := func(name, src string) (optCount, vecCount int) {
		root, err := compiler.Compile("="+name, src)
		if err != nil {
			t.Fatalf("compile %s: %v", name, err)
		}
		var walk func(p *bytecode.Proto)
		walk = func(p *bytecode.Proto) {
			regs := ir.OptimizeProto(p, &ir.Feedback{EnableSIMD: true}, 12, 16, 16)
			for _, r := range regs {
				optCount++
				if r.Vec != nil {
					vecCount++
				}
			}
			for _, sub := range p.Protos {
				walk(sub)
			}
		}
		walk(root)
		return optCount, vecCount
	}

	expectations := []struct {
		file   string
		minOpt int
		minVec int
	}{
		{"matmul.lua", 2, 2},
		{"spectralnorm.lua", 2, 2},
		{"sieve.lua", 2, 0},
		{"fannkuch.lua", 3, 1},
		{"forloop_int.lua", 1, 0},
		{"mandelbrot.lua", 1, 0},
	}
	for _, tc := range expectations {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "bench", "scripts", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		o, v := countOpt(tc.file, string(b))
		if o < tc.minOpt || v < tc.minVec {
			t.Errorf("%s: got opt=%d vec=%d, want >= opt=%d vec=%d", tc.file, o, v, tc.minOpt, tc.minVec)
		}
	}

	sciRaw, err := os.ReadFile(filepath.Join("..", "..", "..", "bench", "compare", "testdata", "scimark", "scimark.lua"))
	if err != nil {
		t.Fatal(err)
	}
	sciBody := string(sciRaw)
	cut := strings.Index(sciBody, "-- Main program.")
	o, v := countOpt("scimark.lua", sciBody[:cut])
	if o < 10 || v < 4 {
		t.Errorf("scimark.lua: got opt=%d vec=%d, want >= opt=10 vec=4", o, v)
	}
}
