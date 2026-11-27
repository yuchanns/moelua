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

func TestNEONVectorAXPYAndIntSum(t *testing.T) {
	if !execmem.Supported() {
		t.Skip("execmem unsupported on this platform")
	}

	// Test 1: float64 DAXPY: dst[i] = dst[i] + alpha * src[i] using 2-wide NEON (.2D)
	// plus dot-product exact lane-ordered accumulation: sum = sum + dst[i] * src[i].
	type daxpyCtx struct {
		dst   uintptr
		src   uintptr
		n     int64 // even count
		alpha float64
		dot   float64
	}

	a := arm64.New(128)
	// X0 = *daxpyCtx
	a.LDR(arm64.X1, arm64.X0, 0)     // X1 = dst
	a.LDR(arm64.X2, arm64.X0, 8)     // X2 = src
	a.LDR(arm64.X3, arm64.X0, 16)    // X3 = n
	a.LDRD(arm64.D0, arm64.X0, 24)   // D0 = alpha
	a.DUP2DElem0(arm64.D1, arm64.D0) // V1.2D = {alpha, alpha}
	a.MOVI2DZero(arm64.D2)           // D2 = dot = 0.0

	loop := a.NewLabel()
	done := a.NewLabel()
	a.Bind(loop)
	a.CMPImm(arm64.X3, 2)
	a.BCond(arm64.LT, done)
	a.LDRQ(arm64.D3, arm64.X1, 0)          // V3 = dst[i..i+1]
	a.LDRQPost(arm64.D4, arm64.X2, 16)     // V4 = src[i..i+1], X2 += 16
	a.FMUL2D(arm64.D5, arm64.D1, arm64.D4) // V5 = alpha * src
	a.FADD2D(arm64.D3, arm64.D3, arm64.D5) // V3 = dst + alpha * src
	a.STRQPost(arm64.D3, arm64.X1, 16)     // dst[i..i+1] = V3, X1 += 16
	a.FMUL2D(arm64.D6, arm64.D3, arm64.D4) // V6 = newDst * src
	a.FADD(arm64.D2, arm64.D2, arm64.D6)   // dot += lane 0
	a.DUPD1(arm64.D7, arm64.D6)            // D7 = lane 1
	a.FADD(arm64.D2, arm64.D2, arm64.D7)   // dot += lane 1
	a.SUBImm(arm64.X3, arm64.X3, 2)
	a.B(loop)

	a.Bind(done)
	a.STRD(arm64.D2, arm64.X0, 32)
	a.MOVZ(arm64.X0, 42, 0)
	a.RET()

	code, err := a.Finish()
	if err != nil {
		t.Fatalf("arm64 NEON Finish failed: %v", err)
	}
	reg, err := execmem.Publish(code)
	if err != nil {
		t.Fatalf("Publish failed: %v", err)
	}
	defer reg.Free()

	const n = 16
	dst := make([]float64, n)
	src := make([]float64, n)
	wantDst := make([]float64, n)
	alpha := -1.375
	var wantDot float64
	for i := range n {
		dst[i] = float64(i)*0.5 + 1.25
		src[i] = float64(n-i)*0.25 - 0.75
		wantDst[i] = dst[i] + alpha*src[i]
		wantDot += wantDst[i] * src[i]
	}
	ctx := daxpyCtx{
		dst:   uintptr(unsafe.Pointer(&dst[0])),
		src:   uintptr(unsafe.Pointer(&src[0])),
		n:     n,
		alpha: alpha,
	}
	ret := call.Call(reg.Addr(), unsafe.Pointer(&ctx))
	runtime.KeepAlive(dst)
	runtime.KeepAlive(src)
	if ret != 42 {
		t.Fatalf("ret=%d, want 42", ret)
	}
	for i := range n {
		if math.Float64bits(dst[i]) != math.Float64bits(wantDst[i]) {
			t.Fatalf("dst[%d]=%v (%x), want %v (%x)", i, dst[i], math.Float64bits(dst[i]), wantDst[i], math.Float64bits(wantDst[i]))
		}
	}
	if math.Float64bits(ctx.dot) != math.Float64bits(wantDot) {
		t.Fatalf("dot=%v (%x), want %v (%x)", ctx.dot, math.Float64bits(ctx.dot), wantDot, math.Float64bits(wantDot))
	}

	// Test 2: int64 2-wide ADD2D + ADDPD horizontal sum.
	type intSumCtx struct {
		arr uintptr
		n   int64
	}
	a2 := arm64.New(64)
	a2.LDR(arm64.X1, arm64.X0, 0)
	a2.LDR(arm64.X2, arm64.X0, 8)
	a2.MOVI2DZero(arm64.D0)
	l2 := a2.NewLabel()
	d2 := a2.NewLabel()
	a2.Bind(l2)
	a2.CMPImm(arm64.X2, 2)
	a2.BCond(arm64.LT, d2)
	a2.LDRQPost(arm64.D1, arm64.X1, 16)
	a2.ADD2D(arm64.D0, arm64.D0, arm64.D1)
	a2.SUBImm(arm64.X2, arm64.X2, 2)
	a2.B(l2)
	a2.Bind(d2)
	a2.ADDPD(arm64.D0, arm64.D0)
	a2.FMOVToG(arm64.X0, arm64.D0)
	a2.RET()

	code2, err := a2.Finish()
	if err != nil {
		t.Fatalf("arm64 NEON intSum Finish failed: %v", err)
	}
	reg2, err := execmem.Publish(code2)
	if err != nil {
		t.Fatalf("Publish failed: %v", err)
	}
	defer reg2.Free()

	nums := make([]int64, 20)
	var wantSum int64
	for i := range nums {
		nums[i] = int64(i*7 - 33)
		wantSum += nums[i]
	}
	ic := intSumCtx{arr: uintptr(unsafe.Pointer(&nums[0])), n: int64(len(nums))}
	gotSum := int64(call.Call(reg2.Addr(), unsafe.Pointer(&ic)))
	runtime.KeepAlive(nums)
	if gotSum != wantSum {
		t.Fatalf("gotSum=%d, want %d", gotSum, wantSum)
	}
}
