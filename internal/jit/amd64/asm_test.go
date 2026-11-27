package amd64

import (
	"bytes"
	"testing"
)

func TestEncodeEdgeCasesAndModRMSIB(t *testing.T) {
	tests := []struct {
		name string
		emit func(*Assembler)
		want []byte
	}{
		{
			name: "MOVQ RAX, RDI",
			emit: func(a *Assembler) { a.MOVQ(RAX, RDI) },
			want: []byte{0x48, 0x89, 0xF8},
		},
		{
			name: "MOVQLoad RAX, [RSP]",
			emit: func(a *Assembler) { a.MOVQLoad(RAX, BaseDisp(RSP, 0)) },
			want: []byte{0x48, 0x8B, 0x04, 0x24},
		},
		{
			name: "MOVQLoad RAX, [R12+16]",
			emit: func(a *Assembler) { a.MOVQLoad(RAX, BaseDisp(R12, 16)) },
			want: []byte{0x49, 0x8B, 0x44, 0x24, 0x10},
		},
		{
			name: "MOVQLoad RAX, [RBP+0] forces disp8",
			emit: func(a *Assembler) { a.MOVQLoad(RAX, BaseDisp(RBP, 0)) },
			want: []byte{0x48, 0x8B, 0x45, 0x00},
		},
		{
			name: "MOVQLoad RAX, [R13+0] forces disp8",
			emit: func(a *Assembler) { a.MOVQLoad(RAX, BaseDisp(R13, 0)) },
			want: []byte{0x49, 0x8B, 0x45, 0x00},
		},
		{
			name: "MOVSDLoad XMM0, [RDI+RSI*8]",
			emit: func(a *Assembler) { a.MOVSDLoad(XMM0, Indexed(RDI, RSI, 8, 0)) },
			want: []byte{0xF2, 0x0F, 0x10, 0x04, 0xF7},
		},
		{
			name: "SETCC E, SIL forces REX 0x40",
			emit: func(a *Assembler) { a.SETCC(E, RSI) },
			want: []byte{0x40, 0x0F, 0x94, 0xC6},
		},
		{
			name: "MOVBStore [RDI], DIL forces REX 0x40",
			emit: func(a *Assembler) { a.MOVBStore(BaseDisp(RDI, 0), RDI) },
			want: []byte{0x40, 0x88, 0x3F},
		},
		{
			name: "MOVImm64 0 uses XORL",
			emit: func(a *Assembler) { a.MOVImm64(RAX, 0) },
			want: []byte{0x31, 0xC0},
		},
		{
			name: "MOVImm64 full 64-bit uses MOVABS",
			emit: func(a *Assembler) { a.MOVImm64(R8, 0xCAFEBABE12345678) },
			want: []byte{0x49, 0xB8, 0x78, 0x56, 0x34, 0x12, 0xBE, 0xBA, 0xFE, 0xCA},
		},
		{
			name: "ADDSD XMM8, XMM9",
			emit: func(a *Assembler) { a.ADDSD(XMM8, XMM9) },
			want: []byte{0xF2, 0x45, 0x0F, 0x58, 0xC1},
		},
		{
			name: "SHLLImm RAX, 0 zero-extends via MOVL",
			emit: func(a *Assembler) { a.SHLLImm(RAX, 0) },
			want: []byte{0x89, 0xC0},
		},
		{
			name: "SHRLImm R8, 0 zero-extends via MOVL",
			emit: func(a *Assembler) { a.SHRLImm(R8, 0) },
			want: []byte{0x45, 0x89, 0xC0},
		},
		{
			name: "SHLQImm RAX, 0 is a no-op",
			emit: func(a *Assembler) { a.SHLQImm(RAX, 0) },
			want: []byte{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := New(32)
			tc.emit(a)
			got, err := a.Finish()
			if err != nil {
				t.Fatalf("Finish failed: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("got % X, want % X", got, tc.want)
			}
		})
	}
}

func TestInvalidOperandsAndLabels(t *testing.T) {
	// RSP as SIB index must produce an error.
	a := New(16)
	a.MOVQLoad(RAX, Indexed(RDI, RSP, 8, 0))
	if _, err := a.Finish(); err == nil {
		t.Fatal("expected error for RSP SIB index")
	}

	// Unbound label must produce an error.
	b := New(16)
	l := b.NewLabel()
	b.JMP(l)
	if _, err := b.Finish(); err == nil {
		t.Fatal("expected error for unbound label")
	}

	// Duplicate bind must produce an error.
	c := New(16)
	dup := c.NewLabel()
	c.Bind(dup)
	c.Bind(dup)
	if _, err := c.Finish(); err == nil {
		t.Fatal("expected error for duplicate bind")
	}
}
