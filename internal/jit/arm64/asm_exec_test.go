//go:build arm64 && !moeluanojit

package arm64_test

import (
	"math"
	"runtime"
	"testing"
	"unsafe"

	"github.com/yuchanns/moelua/internal/jit/arm64"
	"github.com/yuchanns/moelua/internal/jit/call"
	"github.com/yuchanns/moelua/internal/jit/execmem"
)

func publishARM64(t *testing.T, a *arm64.Assembler) *execmem.Region {
	t.Helper()
	if !execmem.Supported() {
		t.Skip("execmem unsupported on this platform")
	}
	code, err := a.Finish()
	if err != nil {
		t.Fatalf("Assembler.Finish: %v", err)
	}
	reg, err := execmem.Publish(code)
	if err != nil {
		t.Skipf("execmem.Publish unavailable: %v", err)
	}
	return reg
}

func TestExecIntegerArithmeticAndLoop(t *testing.T) {
	// Sum 1..N + test MUL, SDIV, MSUB, shifts, logical immediates, and frame save/restore.
	type ctxStruct struct {
		n      int64
		sum    int64
		prod   int64
		quot   int64
		rem    int64
		masked uint64
		bigImm uint64
	}
	ctx := ctxStruct{n: 100}
	wantBig := uint64(0xCAFEBABE12345678)

	a := arm64.New(64)
	fl := arm64.ComputeFrameLayout(32, []arm64.Reg{arm64.X19, arm64.X20, arm64.X21})
	a.EmitPrologue(fl)

	a.MOV(arm64.X19, arm64.X0)       // X19 = &ctx
	a.LDR(arm64.X20, arm64.X19, 0)   // X20 = n
	a.MOVImm64(arm64.X21, 0)         // X21 = sum

	loop := a.NewLabel()
	done := a.NewLabel()
	a.CBZ(arm64.X20, done)
	a.Bind(loop)
	a.ADD(arm64.X21, arm64.X21, arm64.X20)
	a.SUBImm(arm64.X20, arm64.X20, 1)
	a.CBNZ(arm64.X20, loop)
	a.Bind(done)

	a.STR(arm64.X21, arm64.X19, 8) // ctx.sum = 5050

	// Spill & reload test on local stack slot [SP+0]
	a.STR(arm64.X21, arm64.SP, 0)
	a.LDR(arm64.X1, arm64.SP, 0)
	a.MOVImm64(arm64.X2, 7)
	a.MUL(arm64.X3, arm64.X1, arm64.X2) // 5050 * 7 = 35350
	a.STR(arm64.X3, arm64.X19, 16)

	// Signed division & remainder via SDIV + MSUB: -5050 / 13 = -388, rem = -6
	a.NEG(arm64.X4, arm64.X1)
	a.MOVImm64(arm64.X5, 13)
	a.SDIV(arm64.X6, arm64.X4, arm64.X5)
	a.MSUB(arm64.X7, arm64.X6, arm64.X5, arm64.X4)
	a.STR(arm64.X6, arm64.X19, 24)
	a.STR(arm64.X7, arm64.X19, 32)

	// Logical immediate ANDImm + MOVImm64
	a.MOVImm64(arm64.X8, wantBig)
	a.ANDImm(arm64.X9, arm64.X8, 0x1FFF)
	a.STR(arm64.X9, arm64.X19, 40)
	a.STR(arm64.X8, arm64.X19, 48)

	a.MOV(arm64.X0, arm64.X21) // return sum
	a.EmitEpilogue(fl)

	reg := publishARM64(t, a)
	defer reg.Free()
	ret := call.Call(reg.Addr(), unsafe.Pointer(&ctx))
	if ret != 5050 || ctx.sum != 5050 {
		t.Fatalf("sum: ret=%d ctx.sum=%d, want 5050", ret, ctx.sum)
	}
	if ctx.prod != 35350 {
		t.Fatalf("prod=%d, want 35350", ctx.prod)
	}
	if ctx.quot != -388 || ctx.rem != -6 {
		t.Fatalf("quot=%d rem=%d, want -388, -6", ctx.quot, ctx.rem)
	}
	if ctx.masked != (wantBig&0x1FFF) || ctx.bigImm != wantBig {
		t.Fatalf("masked=0x%x bigImm=0x%x", ctx.masked, ctx.bigImm)
	}
}

func TestExecFP64AndIndexedArray(t *testing.T) {
	// Dot product + sqrt + floor + float-to-int over dense float64 array using LDRDReg scaled!
	arr := []float64{1.5, 2.5, 3.5, 4.5}
	type fpCtx struct {
		base    uintptr
		n       int64
		sumSq   float64
		root    float64
		floored float64
		asInt   int64
	}
	ctx := fpCtx{
		base: uintptr(unsafe.Pointer(&arr[0])),
		n:    int64(len(arr)),
	}

	a := arm64.New(64)
	fl := arm64.ComputeFrameLayout(0, []arm64.Reg{arm64.X19})
	a.EmitPrologue(fl)

	a.MOV(arm64.X19, arm64.X0)
	a.LDR(arm64.X1, arm64.X19, 0) // base
	a.LDR(arm64.X2, arm64.X19, 8) // n
	a.MOVImm64(arm64.X3, 0)       // i = 0
	a.FMOVToF(arm64.D0, arm64.X3) // acc = 0.0

	loop := a.NewLabel()
	done := a.NewLabel()
	a.Bind(loop)
	a.CMP(arm64.X3, arm64.X2)
	a.BCond(arm64.GE, done)
	a.LDRDReg(arm64.D1, arm64.X1, arm64.X3, true) // D1 = arr[i]
	a.FMADD(arm64.D0, arm64.D1, arm64.D1, arm64.D0)
	a.ADDImm(arm64.X3, arm64.X3, 1)
	a.B(loop)
	a.Bind(done)

	a.STRD(arm64.D0, arm64.X19, 16) // sumSq = 39.0
	a.FSQRT(arm64.D2, arm64.D0)
	a.STRD(arm64.D2, arm64.X19, 24)
	a.FRINTM(arm64.D3, arm64.D2)
	a.STRD(arm64.D3, arm64.X19, 32)
	a.FCVTZS(arm64.X4, arm64.D3)
	a.STR(arm64.X4, arm64.X19, 40)

	a.MOVImm64(arm64.X0, 0)
	a.EmitEpilogue(fl)

	reg := publishARM64(t, a)
	defer reg.Free()
	ctx.base = uintptr(unsafe.Pointer(&arr[0]))
	ret := call.Call(reg.Addr(), unsafe.Pointer(&ctx))
	runtime.KeepAlive(arr)
	if ret != 0 {
		t.Fatalf("unexpected ret=%d", ret)
	}
	wantSumSq := 1.5*1.5 + 2.5*2.5 + 3.5*3.5 + 4.5*4.5 // 41.0
	if ctx.sumSq != wantSumSq {
		t.Fatalf("sumSq=%v, want %v", ctx.sumSq, wantSumSq)
	}
	if math.Abs(ctx.root-math.Sqrt(wantSumSq)) > 1e-12 {
		t.Fatalf("root=%v, want %v", ctx.root, math.Sqrt(wantSumSq))
	}
	if ctx.floored != 6.0 || ctx.asInt != 6 {
		t.Fatalf("floored=%v asInt=%d, want 6.0 / 6", ctx.floored, ctx.asInt)
	}
}

func TestExecLargeSpillFrame(t *testing.T) {
	a := arm64.New(32)
	fl := arm64.ComputeFrameLayout(640, []arm64.Reg{arm64.X19, arm64.X20, arm64.X21})
	a.EmitPrologue(fl)
	a.MOV(arm64.X19, arm64.X0)
	a.LDR(arm64.X20, arm64.X19, 0)
	a.STR(arm64.X20, arm64.SP, 632)
	a.LDR(arm64.X0, arm64.SP, 632)
	a.ADDImm(arm64.X0, arm64.X0, 7)
	a.EmitEpilogue(fl)

	reg := publishARM64(t, a)
	defer reg.Free()
	var in uint64 = 35
	if got := call.Call(reg.Addr(), unsafe.Pointer(&in)); got != 42 {
		t.Fatalf("large spill frame returned %d, want 42", got)
	}
}
