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

func TestSSE2VectorAXPYAndIntSum(t *testing.T) {
	if !execmem.Supported() {
		t.Skip("execmem unsupported on this platform")
	}

	type daxpyCtx struct {
		dst   uintptr
		src   uintptr
		n     int64
		alpha float64
		dot   float64
	}

	a := amd64.New(128)
	// RDI = *daxpyCtx
	a.MOVQLoad(amd64.RSI, amd64.BaseDisp(amd64.RDI, 0))  // RSI = dst
	a.MOVQLoad(amd64.RDX, amd64.BaseDisp(amd64.RDI, 8))  // RDX = src
	a.MOVQLoad(amd64.RCX, amd64.BaseDisp(amd64.RDI, 16)) // RCX = n
	a.MOVSDLoad(amd64.XMM1, amd64.BaseDisp(amd64.RDI, 24))
	a.UNPCKLPD(amd64.XMM1, amd64.XMM1) // XMM1 = {alpha, alpha}
	a.XORPD(amd64.XMM2, amd64.XMM2)    // XMM2 = dot = 0.0

	loop := a.NewLabel()
	done := a.NewLabel()
	a.Bind(loop)
	a.CMPQImm(amd64.RCX, 2)
	a.JCC(amd64.L, done)
	a.MOVUPDLoad(amd64.XMM3, amd64.BaseDisp(amd64.RSI, 0)) // XMM3 = dst[i..i+1]
	a.MOVUPDLoad(amd64.XMM4, amd64.BaseDisp(amd64.RDX, 0)) // XMM4 = src[i..i+1]
	a.MOVAPD(amd64.XMM5, amd64.XMM4)
	a.MULPD(amd64.XMM5, amd64.XMM1) // alpha * src
	a.ADDPD(amd64.XMM3, amd64.XMM5) // dst + alpha * src
	a.MOVUPDStore(amd64.BaseDisp(amd64.RSI, 0), amd64.XMM3)
	a.MULPD(amd64.XMM3, amd64.XMM4) // newDst * src
	a.ADDSD(amd64.XMM2, amd64.XMM3) // dot += lane 0
	a.MOVAPD(amd64.XMM6, amd64.XMM3)
	a.UNPCKHPD(amd64.XMM6, amd64.XMM6) // lane 1 -> lane 0
	a.ADDSD(amd64.XMM2, amd64.XMM6)    // dot += lane 1
	a.ADDQImm(amd64.RSI, 16)
	a.ADDQImm(amd64.RDX, 16)
	a.SUBQImm(amd64.RCX, 2)
	a.JMP(loop)

	a.Bind(done)
	a.MOVSDStore(amd64.BaseDisp(amd64.RDI, 32), amd64.XMM2)
	a.MOVImm64(amd64.RAX, 42)
	a.RET()

	code, err := a.Finish()
	if err != nil {
		t.Fatalf("amd64 SSE2 Finish failed: %v", err)
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

	// Test 2: int64 2-wide PADDQ + PUNPCKHQDQ horizontal sum.
	type intSumCtx struct {
		arr uintptr
		n   int64
	}
	a2 := amd64.New(64)
	a2.MOVQLoad(amd64.RSI, amd64.BaseDisp(amd64.RDI, 0))
	a2.MOVQLoad(amd64.RCX, amd64.BaseDisp(amd64.RDI, 8))
	a2.PXOR(amd64.XMM0, amd64.XMM0)
	l2 := a2.NewLabel()
	d2 := a2.NewLabel()
	a2.Bind(l2)
	a2.CMPQImm(amd64.RCX, 2)
	a2.JCC(amd64.L, d2)
	a2.MOVDQULoad(amd64.XMM1, amd64.BaseDisp(amd64.RSI, 0))
	a2.PADDQ(amd64.XMM0, amd64.XMM1)
	a2.ADDQImm(amd64.RSI, 16)
	a2.SUBQImm(amd64.RCX, 2)
	a2.JMP(l2)
	a2.Bind(d2)
	a2.MOVDQA(amd64.XMM1, amd64.XMM0)
	a2.PUNPCKHQDQ(amd64.XMM1, amd64.XMM1)
	a2.PADDQ(amd64.XMM0, amd64.XMM1)
	a2.MOVQFromX(amd64.RAX, amd64.XMM0)
	a2.RET()

	code2, err := a2.Finish()
	if err != nil {
		t.Fatalf("amd64 SSE2 intSum Finish failed: %v", err)
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
