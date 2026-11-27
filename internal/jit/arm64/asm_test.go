package arm64

import (
	"encoding/binary"
	"testing"
)

func mustWord(t *testing.T, code []byte, idx int) uint32 {
	t.Helper()
	if (idx+1)*4 > len(code) {
		t.Fatalf("instruction index %d out of range (len=%d)", idx, len(code))
	}
	return binary.LittleEndian.Uint32(code[idx*4:])
}

func TestEncodeBasicInstructions(t *testing.T) {
	a := New(32)
	a.MOV(X0, X1)             // 0xAA0103E0
	a.MOVSP(X29, SP)          // 0x910003FD
	a.MOVZ(X0, 42, 0)         // 0xD2800540
	a.ADD(X0, X1, X2)         // 0x8B020020
	a.SUBImm(SP, SP, 32)      // 0xD10083FF
	a.MUL(X3, X4, X5)         // 0x9B057C83
	a.SDIV(X6, X7, X8)        // 0x9AC80CE6
	a.LDR(X9, X0, 16)         // 0xF9400809
	a.STR(X10, X0, 24)        // 0xF9000C0A
	a.LDRDReg(D0, X1, X2, true) // 0xFC627820
	a.FADD(D0, D1, D2)        // 0x1E622820
	a.FSQRT(D3, D4)           // 0x1E61C083
	a.RET()                   // 0xD65F03C0

	code, err := a.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	want := []uint32{
		0xAA0103E0,
		0x910003FD,
		0xD2800540,
		0x8B020020,
		0xD10083FF,
		0x9B057C83,
		0x9AC80CE6,
		0xF9400809,
		0xF9000C0A,
		0xFC627820,
		0x1E622820,
		0x1E61C083,
		0xD65F03C0,
	}
	if len(code) != len(want)*4 {
		t.Fatalf("len=%d, want %d", len(code), len(want)*4)
	}
	for i, w := range want {
		if got := mustWord(t, code, i); got != w {
			t.Errorf("insn[%d] = 0x%08X, want 0x%08X", i, got, w)
		}
	}
}

func TestEncodeLogicalImm64(t *testing.T) {
	valid := []uint64{
		1,
		0xFF,
		0x1FFF,
		0xFFFF,
		1 << 63,
		^(uint64(1) << 63),
		0x00FF00FF00FF00FF,
		0x5555555555555555,
		0xAAAAAAAAAAAAAAAA,
		0x0000FFFF0000FFFF,
		0xFFFFFFFF00000000,
		0x8000000000000001,
	}
	for _, v := range valid {
		n, immr, imms, ok := EncodeLogicalImm64(v)
		if !ok {
			t.Fatalf("EncodeLogicalImm64(0x%016x) failed, expected ok", v)
		}
		got := decodeLogicalImm64(n, immr, imms)
		if got != v {
			t.Fatalf("roundtrip(0x%016x) = 0x%016x (n=%d immr=%d imms=%d)", v, got, n, immr, imms)
		}
	}
	invalid := []uint64{0, ^uint64(0), 0x1234, 0x101, 0x5}
	for _, v := range invalid {
		if _, _, _, ok := EncodeLogicalImm64(v); ok {
			t.Fatalf("EncodeLogicalImm64(0x%016x) unexpectedly succeeded", v)
		}
	}
}

// decodeLogicalImm64 mirrors ARMv8 DecodeBitMasks for verification in tests.
func decodeLogicalImm64(n, immr, imms uint32) uint64 {
	var esize uint
	switch {
	case n == 1:
		esize = 64
	case (imms & 0x20) == 0:
		esize = 32
	case (imms & 0x10) == 0:
		esize = 16
	case (imms & 0x08) == 0:
		esize = 8
	case (imms & 0x04) == 0:
		esize = 4
	default:
		esize = 2
	}
	levels := esize - 1
	s := uint(imms) & levels
	r := uint(immr) & levels
	welem := (uint64(1) << (s + 1)) - 1
	mask := ^uint64(0)
	if esize < 64 {
		mask = (uint64(1) << esize) - 1
	}
	if r != 0 {
		welem = ((welem >> r) | (welem << (esize - r))) & mask
	}
	var out uint64
	for shift := uint(0); shift < 64; shift += esize {
		out |= welem << shift
	}
	return out
}

func TestLabelErrorsAndForwardBackwardBranches(t *testing.T) {
	a := New(16)
	loop := a.NewLabel()
	done := a.NewLabel()
	a.CBZ(X0, done) // insn 0 -> target 3 (+3)
	a.Bind(loop)    // insn 1
	a.SUBImm(X0, X0, 1)
	a.CBNZ(X0, loop) // insn 2 -> target 1 (-1)
	a.Bind(done)     // insn 3
	a.RET()

	code, err := a.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if got := mustWord(t, code, 0); got != uint32(0xB4000000|(3<<5)) {
		t.Fatalf("CBZ = 0x%08X, want 0x%08X", got, uint32(0xB4000000|(3<<5)))
	}
	neg1 := int32(-1)
	wantBack := uint32(0xB5000000) | ((uint32(neg1) & 0x7FFFF) << 5)
	if got := mustWord(t, code, 2); got != wantBack {
		t.Fatalf("CBNZ = 0x%08X, want 0x%08X", got, wantBack)
	}

	// Unbound label must return error, not panic.
	b := New(8)
	unbound := b.NewLabel()
	b.B(unbound)
	if _, err := b.Finish(); err == nil {
		t.Fatal("expected error for unbound label")
	}

	// Duplicate bind must return error.
	c := New(8)
	dup := c.NewLabel()
	c.Bind(dup)
	c.Bind(dup)
	if _, err := c.Finish(); err == nil {
		t.Fatal("expected error for duplicate Bind")
	}
}

func TestLargeSpillFramePrologueEpilogue(t *testing.T) {
	for _, localBytes := range []int32{512, 1024, 4096, 4112} {
		a := New(32)
		fl := ComputeFrameLayout(localBytes, []Reg{X19, X20, X21})
		a.EmitPrologue(fl)
		a.STR(X0, SP, localBytes-8)
		a.LDR(X0, SP, localBytes-8)
		a.EmitEpilogue(fl)
		if _, err := a.Finish(); err != nil {
			t.Fatalf("large frame localBytes=%d failed: %v", localBytes, err)
		}
	}
}
