//go:build amd64 && !moeluanojit

package amd64_test

import (
	"math"
	"runtime"
	"testing"
	"unsafe"

	"github.com/yuchanns/moelua/internal/jit/amd64"
	"github.com/yuchanns/moelua/internal/jit/call"
	"github.com/yuchanns/moelua/internal/jit/execmem"
)

func publishAMD64(t *testing.T, a *amd64.Assembler) *execmem.Region {
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
	type ctxStruct struct {
		n      int64
		sum    int64
		prod   int64
		quot   int64
		rem    int64
		flag   uint64
		bigImm uint64
	}
	ctx := ctxStruct{n: 100}
	wantBig := uint64(0xCAFEBABE12345678)

	a := amd64.New(128)
	fl := amd64.ComputeFrameLayout(24, []amd64.Reg{amd64.RBX, amd64.R12, amd64.R13})
	a.EmitPrologue(fl)

	a.MOVQ(amd64.R12, amd64.RDI)                        // R12 = &ctx (tests SIB base=12!)
	a.MOVQLoad(amd64.R13, amd64.BaseDisp(amd64.R12, 0)) // R13 = n (tests R13 base!)
	a.MOVImm64(amd64.RBX, 0)                            // RBX = sum

	loop := a.NewLabel()
	done := a.NewLabel()
	a.TESTQ(amd64.R13, amd64.R13)
	a.JCC(amd64.E, done)
	a.Bind(loop)
	a.ADDQ(amd64.RBX, amd64.R13)
	a.SUBQImm(amd64.R13, 1)
	a.JCC(amd64.NE, loop)
	a.Bind(done)

	a.MOVQStore(amd64.BaseDisp(amd64.R12, 8), amd64.RBX) // ctx.sum = 5050

	// Spill & reload on [RSP+0] (tests SIB base=RSP)
	a.MOVQStore(amd64.BaseDisp(amd64.RSP, 0), amd64.RBX)
	a.MOVQLoad(amd64.RAX, amd64.BaseDisp(amd64.RSP, 0))
	a.IMULQImm(amd64.RCX, amd64.RAX, 7) // 5050 * 7 = 35350
	a.MOVQStore(amd64.BaseDisp(amd64.R12, 16), amd64.RCX)

	// Signed division: -5050 / 13 = -388, rem = -6
	a.NEGQ(amd64.RAX)
	a.CQO()
	a.MOVImm64(amd64.RSI, 13)
	a.IDIVQ(amd64.RSI)
	a.MOVQStore(amd64.BaseDisp(amd64.R12, 24), amd64.RAX)
	a.MOVQStore(amd64.BaseDisp(amd64.R12, 32), amd64.RDX)

	// SETCC on SIL (tests REX 0x40 on reg 6) + MOVZXBQReg
	a.CMPQImm(amd64.RDX, -6)
	a.SETCC(amd64.E, amd64.RSI)
	a.MOVZXBQReg(amd64.RSI, amd64.RSI)
	a.MOVQStore(amd64.BaseDisp(amd64.R12, 40), amd64.RSI)

	// Full 64-bit MOVABS
	a.MOVImm64(amd64.R8, wantBig)
	a.MOVQStore(amd64.BaseDisp(amd64.R12, 48), amd64.R8)

	a.MOVQ(amd64.RAX, amd64.RBX) // return sum
	a.EmitEpilogue(fl)

	reg := publishAMD64(t, a)
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
	if ctx.flag != 1 || ctx.bigImm != wantBig {
		t.Fatalf("flag=%d bigImm=0x%x", ctx.flag, ctx.bigImm)
	}
}

func TestExecFP64AndIndexedArray(t *testing.T) {
	arr := []float64{1.5, 2.5, 3.5, 4.5}
	type fpCtx struct {
		base   uintptr
		n      int64
		sumSq  float64
		root   float64
		asInt  int64
		cmpRes uint64
	}
	ctx := fpCtx{
		base: uintptr(unsafe.Pointer(&arr[0])),
		n:    int64(len(arr)),
	}

	a := amd64.New(128)
	fl := amd64.ComputeFrameLayout(0, []amd64.Reg{amd64.RBX})
	a.EmitPrologue(fl)

	a.MOVQ(amd64.RBX, amd64.RDI)
	a.MOVQLoad(amd64.RSI, amd64.BaseDisp(amd64.RBX, 0)) // base
	a.MOVQLoad(amd64.RCX, amd64.BaseDisp(amd64.RBX, 8)) // n
	a.MOVImm64(amd64.RDX, 0)                            // i = 0
	a.XORPD(amd64.XMM0, amd64.XMM0)                     // acc = 0.0

	loop := a.NewLabel()
	done := a.NewLabel()
	a.Bind(loop)
	a.CMPQ(amd64.RDX, amd64.RCX)
	a.JCC(amd64.GE, done)
	a.MOVSDLoad(amd64.XMM1, amd64.Indexed(amd64.RSI, amd64.RDX, 8, 0))
	a.MULSD(amd64.XMM1, amd64.XMM1)
	a.ADDSD(amd64.XMM0, amd64.XMM1)
	a.ADDQImm(amd64.RDX, 1)
	a.JMP(loop)
	a.Bind(done)

	a.MOVSDStore(amd64.BaseDisp(amd64.RBX, 16), amd64.XMM0) // sumSq = 39.0
	a.SQRTSD(amd64.XMM2, amd64.XMM0)
	a.MOVSDStore(amd64.BaseDisp(amd64.RBX, 24), amd64.XMM2)
	a.CVTTSD2SIQ(amd64.R8, amd64.XMM2)
	a.MOVQStore(amd64.BaseDisp(amd64.RBX, 32), amd64.R8)

	// UCOMISD test: sumSq (39.0) > root (~6.245) => A (above)
	a.UCOMISD(amd64.XMM0, amd64.XMM2)
	a.SETCC(amd64.A, amd64.R9)
	a.MOVZXBQReg(amd64.R9, amd64.R9)
	a.MOVQStore(amd64.BaseDisp(amd64.RBX, 40), amd64.R9)

	a.MOVImm64(amd64.RAX, 0)
	a.EmitEpilogue(fl)

	reg := publishAMD64(t, a)
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
	if ctx.asInt != 6 || ctx.cmpRes != 1 {
		t.Fatalf("asInt=%d cmpRes=%d, want 6 / 1", ctx.asInt, ctx.cmpRes)
	}
}

func TestExecZeroCount32BitShiftZeroExtends(t *testing.T) {
	a := amd64.New(32)
	a.MOVImm64(amd64.RAX, ^uint64(0))
	a.SHLLImm(amd64.RAX, 0)
	a.MOVImm64(amd64.RCX, ^uint64(0))
	a.SHRLImm(amd64.RCX, 0)
	a.XORQ(amd64.RAX, amd64.RCX) // 0 if both equal 0xFFFFFFFF
	a.ORQ(amd64.RAX, amd64.RCX)  // returns 0x00000000FFFFFFFF
	a.RET()

	reg := publishAMD64(t, a)
	defer reg.Free()
	var dummy uint64
	if got := call.Call(reg.Addr(), unsafe.Pointer(&dummy)); got != 0xFFFFFFFF {
		t.Fatalf("got 0x%x, want 0xffffffff", got)
	}
}
