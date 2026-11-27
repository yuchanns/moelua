// Package amd64 implements a self-contained, zero-dependency x86-64 (AMD64) machine-code
// assembler for MoeLua's native JIT compiler.
//
// All methods record encoding or label-resolution errors internally and surface them
// cleanly via Finish so callers never panic on invalid operands or unbound labels.
package amd64

import (
	"encoding/binary"
	"fmt"
)

// Reg identifies a 64-bit x86-64 general-purpose register (RAX..R15).
type Reg uint8

const (
	RAX Reg = 0
	RCX Reg = 1
	RDX Reg = 2
	RBX Reg = 3
	RSP Reg = 4
	RBP Reg = 5
	RSI Reg = 6
	RDI Reg = 7
	R8  Reg = 8
	R9  Reg = 9
	R10 Reg = 10
	R11 Reg = 11
	R12 Reg = 12
	R13 Reg = 13
	R14 Reg = 14 // Go runtime g register (never clobber)
	R15 Reg = 15
)

// XReg identifies an x86-64 SSE2/SSE4.1 floating-point register (XMM0..XMM15).
// XMM15 is Go's ABIInternal zero register and is reserved by the JIT convention.
type XReg uint8

const (
	XMM0  XReg = 0
	XMM1  XReg = 1
	XMM2  XReg = 2
	XMM3  XReg = 3
	XMM4  XReg = 4
	XMM5  XReg = 5
	XMM6  XReg = 6
	XMM7  XReg = 7
	XMM8  XReg = 8
	XMM9  XReg = 9
	XMM10 XReg = 10
	XMM11 XReg = 11
	XMM12 XReg = 12
	XMM13 XReg = 13
	XMM14 XReg = 14
	XMM15 XReg = 15 // Go zero register (do not clobber)
)

// Cond identifies an x86-64 condition code (low 4 bits of Jcc / SETcc / CMOVcc).
type Cond uint8

const (
	O  Cond = 0x0 // Overflow
	NO Cond = 0x1 // Not overflow
	B  Cond = 0x2 // Below / unsigned < (CF == 1)
	C  Cond = 0x2 // Carry
	AE Cond = 0x3 // Above or equal / unsigned >= (CF == 0)
	NC Cond = 0x3 // Not carry
	E  Cond = 0x4 // Equal (ZF == 1)
	Z  Cond = 0x4 // Zero
	NE Cond = 0x5 // Not equal (ZF == 0)
	NZ Cond = 0x5 // Not zero
	BE Cond = 0x6 // Below or equal / unsigned <= (CF == 1 || ZF == 1)
	A  Cond = 0x7 // Above / unsigned > (CF == 0 && ZF == 0)
	S  Cond = 0x8 // Sign / negative (SF == 1)
	NS Cond = 0x9 // Not sign / non-negative (SF == 0)
	P  Cond = 0xA // Parity / unordered FP (PF == 1)
	NP Cond = 0xB // Not parity / ordered FP (PF == 0)
	L  Cond = 0xC // Signed < (SF != OF)
	GE Cond = 0xD // Signed >= (SF == OF)
	LE Cond = 0xE // Signed <= (ZF == 1 || SF != OF)
	G  Cond = 0xF // Signed > (ZF == 0 && SF == OF)
)

// Invert returns the logical negation of condition c.
func (c Cond) Invert() Cond {
	return c ^ 1
}

// Mem describes an x86-64 base + index*scale + disp32 memory addressing operand.
type Mem struct {
	Base     Reg
	Index    Reg
	Scale    uint8 // 0 (no index) or 1, 2, 4, 8
	Disp     int32
	HasIndex bool
}

// BaseDisp returns [base + disp].
func BaseDisp(base Reg, disp int32) Mem {
	return Mem{Base: base, Disp: disp}
}

// Indexed returns [base + index*scale + disp], with scale in {1, 2, 4, 8}.
func Indexed(base, index Reg, scale uint8, disp int32) Mem {
	return Mem{Base: base, Index: index, Scale: scale, Disp: disp, HasIndex: true}
}

// Label represents a bindable byte offset within an Assembler.
type Label int

type patch struct {
	dispOff int   // byte offset of the rel32 field in code
	nextPC  int   // byte offset immediately following the instruction
	label   Label
}

// Assembler encodes x86-64 instructions into a byte slice with two-pass rel32 label resolution.
type Assembler struct {
	code   []byte
	labels []int // byte offset per label (-1 if unbound)
	patch  []patch
	err    error
}

// New creates an Assembler with an optional initial byte capacity.
func New(capBytes int) *Assembler {
	if capBytes <= 0 {
		capBytes = 256
	}
	return &Assembler{
		code: make([]byte, 0, capBytes),
	}
}

// Reset clears the Assembler for reuse while keeping backing storage.
func (a *Assembler) Reset() {
	a.code = a.code[:0]
	a.labels = a.labels[:0]
	a.patch = a.patch[:0]
	a.err = nil
}

// Len returns the current emitted byte length.
func (a *Assembler) Len() int { return len(a.code) }

// Err returns the first encoding error encountered, if any.
func (a *Assembler) Err() error { return a.err }

func (a *Assembler) setErr(err error) {
	if a.err == nil {
		a.err = err
	}
}

func (a *Assembler) emit1(b byte)          { a.code = append(a.code, b) }
func (a *Assembler) emit2(b0, b1 byte)     { a.code = append(a.code, b0, b1) }
func (a *Assembler) emit3(b0, b1, b2 byte) { a.code = append(a.code, b0, b1, b2) }
func (a *Assembler) emitU32(v uint32)      { a.code = binary.LittleEndian.AppendUint32(a.code, v) }
func (a *Assembler) emitU64(v uint64)      { a.code = binary.LittleEndian.AppendUint64(a.code, v) }

// NewLabel allocates a fresh unbound Label.
func (a *Assembler) NewLabel() Label {
	id := Label(len(a.labels))
	a.labels = append(a.labels, -1)
	return id
}

// Bind binds l to the current byte offset.
func (a *Assembler) Bind(l Label) {
	if int(l) < 0 || int(l) >= len(a.labels) {
		a.setErr(fmt.Errorf("amd64: invalid label %d", l))
		return
	}
	if a.labels[l] != -1 {
		a.setErr(fmt.Errorf("amd64: label %d bound more than once", l))
		return
	}
	a.labels[l] = len(a.code)
}

// Finish resolves all rel32 branch fixups and returns a copy of the encoded machine code.
func (a *Assembler) Finish() ([]byte, error) {
	if a.err != nil {
		return nil, a.err
	}
	for _, p := range a.patch {
		if int(p.label) < 0 || int(p.label) >= len(a.labels) {
			return nil, fmt.Errorf("amd64: invalid label %d in branch", p.label)
		}
		target := a.labels[p.label]
		if target < 0 {
			return nil, fmt.Errorf("amd64: unbound label %d", p.label)
		}
		rel := int64(target) - int64(p.nextPC)
		if rel < -1<<31 || rel > (1<<31)-1 {
			return nil, fmt.Errorf("amd64: branch displacement %d out of int32 range", rel)
		}
		binary.LittleEndian.PutUint32(a.code[p.dispOff:], uint32(int32(rel)))
	}
	out := make([]byte, len(a.code))
	copy(out, a.code)
	return out, nil
}

// --- REX, ModRM & SIB Helpers ---

func (a *Assembler) emitREX(w, r, x, b, force bool) {
	var rex byte = 0x40
	if w {
		rex |= 0x08
	}
	if r {
		rex |= 0x04
	}
	if x {
		rex |= 0x02
	}
	if b {
		rex |= 0x01
	}
	if force || rex != 0x40 {
		a.emit1(rex)
	}
}

func (a *Assembler) emitREXRegReg(w bool, reg, rm uint8, force bool) {
	a.emitREX(w, reg >= 8, false, rm >= 8, force)
}

func (a *Assembler) emitREXMem(w bool, reg uint8, m Mem, force bool) {
	a.emitREX(w, reg >= 8, m.HasIndex && m.Index >= 8, m.Base >= 8, force)
}

func (a *Assembler) emitModRMReg(reg, rm uint8) {
	a.emit1(0xC0 | ((reg & 7) << 3) | (rm & 7))
}

func scaleBits(scale uint8) (byte, bool) {
	switch scale {
	case 1:
		return 0, true
	case 2:
		return 1, true
	case 4:
		return 2, true
	case 8:
		return 3, true
	default:
		return 0, false
	}
}

func (a *Assembler) emitModRMMem(reg uint8, m Mem) {
	if m.HasIndex && m.Index == RSP {
		a.setErr(fmt.Errorf("amd64: RSP cannot be used as SIB index register"))
		return
	}
	baseLow := byte(m.Base & 7)
	regLow := (reg & 7) << 3

	var mod byte
	switch {
	case m.Disp == 0 && baseLow != 5: // RBP (5) and R13 (13) require disp8=0
		mod = 0x00
	case m.Disp >= -128 && m.Disp <= 127:
		mod = 0x40
	default:
		mod = 0x80
	}

	if m.HasIndex {
		sb, ok := scaleBits(m.Scale)
		if !ok {
			a.setErr(fmt.Errorf("amd64: invalid SIB scale %d", m.Scale))
			return
		}
		a.emit1(mod | regLow | 4) // rm=4 signals SIB byte
		a.emit1((sb << 6) | (byte(m.Index&7) << 3) | baseLow)
	} else if baseLow == 4 { // RSP (4) and R12 (12) require SIB byte
		a.emit1(mod | regLow | 4)
		a.emit1((4 << 3) | baseLow) // index=4 (none), base=4
	} else {
		a.emit1(mod | regLow | baseLow)
	}

	switch mod {
	case 0x40:
		a.emit1(byte(int8(m.Disp)))
	case 0x80:
		a.emitU32(uint32(m.Disp))
	}
}

// --- Moves, LEA & Immediate Constants ---

func (a *Assembler) MOVQ(dst, src Reg) {
	a.emitREXRegReg(true, uint8(src), uint8(dst), false)
	a.emit1(0x89)
	a.emitModRMReg(uint8(src), uint8(dst))
}

func (a *Assembler) MOVL(dst, src Reg) {
	a.emitREXRegReg(false, uint8(src), uint8(dst), false)
	a.emit1(0x89)
	a.emitModRMReg(uint8(src), uint8(dst))
}

// MOVSXD sign-extends the low 32 bits of src into 64-bit dst.
func (a *Assembler) MOVSXD(dst, src Reg) {
	a.emitREXRegReg(true, uint8(dst), uint8(src), false)
	a.emit1(0x63)
	a.emitModRMReg(uint8(dst), uint8(src))
}

// MOVImm64 loads an arbitrary 64-bit constant into dst using the shortest encoding.
func (a *Assembler) MOVImm64(dst Reg, val uint64) {
	if val == 0 {
		a.XORL(dst, dst)
		return
	}
	if val <= 0xFFFFFFFF {
		a.MOVImm32(dst, uint32(val))
		return
	}
	s := int64(val)
	if s >= -1<<31 && s <= (1<<31)-1 {
		// MOV r/m64, sign-extended imm32 (C7 /0)
		a.emitREXRegReg(true, 0, uint8(dst), false)
		a.emit1(0xC7)
		a.emitModRMReg(0, uint8(dst))
		a.emitU32(uint32(int32(s)))
		return
	}
	// MOVABS r64, imm64 (REX.W + B8+rd)
	a.emitREX(true, false, false, dst >= 8, false)
	a.emit1(0xB8 | byte(dst&7))
	a.emitU64(val)
}

// MOVImm32 loads a zero-extended 32-bit constant into dst.
func (a *Assembler) MOVImm32(dst Reg, val uint32) {
	if val == 0 {
		a.XORL(dst, dst)
		return
	}
	a.emitREX(false, false, false, dst >= 8, false)
	a.emit1(0xB8 | byte(dst&7))
	a.emitU32(val)
}

func (a *Assembler) LEAQ(dst Reg, m Mem) {
	a.emitREXMem(true, uint8(dst), m, false)
	a.emit1(0x8D)
	a.emitModRMMem(uint8(dst), m)
}

// --- Memory Loads & Stores ---

func (a *Assembler) MOVQLoad(dst Reg, m Mem) {
	a.emitREXMem(true, uint8(dst), m, false)
	a.emit1(0x8B)
	a.emitModRMMem(uint8(dst), m)
}

func (a *Assembler) MOVQStore(m Mem, src Reg) {
	a.emitREXMem(true, uint8(src), m, false)
	a.emit1(0x89)
	a.emitModRMMem(uint8(src), m)
}

// MOVLLoad loads a zero-extended 32-bit value from memory into 64-bit dst.
func (a *Assembler) MOVLLoad(dst Reg, m Mem) {
	a.emitREXMem(false, uint8(dst), m, false)
	a.emit1(0x8B)
	a.emitModRMMem(uint8(dst), m)
}

func (a *Assembler) MOVLStore(m Mem, src Reg) {
	a.emitREXMem(false, uint8(src), m, false)
	a.emit1(0x89)
	a.emitModRMMem(uint8(src), m)
}

// MOVSLQ loads a sign-extended 32-bit value from memory into 64-bit dst.
func (a *Assembler) MOVSLQ(dst Reg, m Mem) {
	a.emitREXMem(true, uint8(dst), m, false)
	a.emit1(0x63)
	a.emitModRMMem(uint8(dst), m)
}

// MOVZXBL loads a zero-extended byte from memory into 64-bit dst.
func (a *Assembler) MOVZXBL(dst Reg, m Mem) {
	a.emitREXMem(false, uint8(dst), m, false)
	a.emit2(0x0F, 0xB6)
	a.emitModRMMem(uint8(dst), m)
}

// MOVSXBQ loads a sign-extended byte from memory into 64-bit dst.
func (a *Assembler) MOVSXBQ(dst Reg, m Mem) {
	a.emitREXMem(true, uint8(dst), m, false)
	a.emit2(0x0F, 0xBE)
	a.emitModRMMem(uint8(dst), m)
}

// MOVZXWL loads a zero-extended uint16 from memory into 64-bit dst.
func (a *Assembler) MOVZXWL(dst Reg, m Mem) {
	a.emitREXMem(false, uint8(dst), m, false)
	a.emit2(0x0F, 0xB7)
	a.emitModRMMem(uint8(dst), m)
}

func (a *Assembler) MOVWStore(m Mem, src Reg) {
	a.emit1(0x66)
	a.emitREXMem(false, uint8(src), m, false)
	a.emit1(0x89)
	a.emitModRMMem(uint8(src), m)
}

func (a *Assembler) MOVBStore(m Mem, src Reg) {
	// Force REX when src is SPL/BPL/SIL/DIL (4..7) so low byte is encoded instead of AH..BH.
	a.emitREXMem(false, uint8(src), m, src >= 4)
	a.emit1(0x88)
	a.emitModRMMem(uint8(src), m)
}

func (a *Assembler) MOVQStoreImm32(m Mem, imm int32) {
	a.emitREXMem(true, 0, m, false)
	a.emit1(0xC7)
	a.emitModRMMem(0, m)
	a.emitU32(uint32(imm))
}

func (a *Assembler) MOVLStoreImm32(m Mem, imm uint32) {
	a.emitREXMem(false, 0, m, false)
	a.emit1(0xC7)
	a.emitModRMMem(0, m)
	a.emitU32(imm)
}

func (a *Assembler) MOVBStoreImm8(m Mem, imm uint8) {
	a.emitREXMem(false, 0, m, false)
	a.emit1(0xC6)
	a.emitModRMMem(0, m)
	a.emit1(imm)
}

// --- Integer ALU (64-bit and 32-bit) ---

func (a *Assembler) emitALURegReg(w bool, op byte, dst, src Reg) {
	a.emitREXRegReg(w, uint8(src), uint8(dst), false)
	a.emit1(op)
	a.emitModRMReg(uint8(src), uint8(dst))
}

func (a *Assembler) emitALUImm(w bool, ext uint8, dst Reg, imm int32) {
	a.emitREXRegReg(w, 0, uint8(dst), false)
	if imm >= -128 && imm <= 127 {
		a.emit1(0x83)
		a.emitModRMReg(ext, uint8(dst))
		a.emit1(byte(int8(imm)))
		return
	}
	a.emit1(0x81)
	a.emitModRMReg(ext, uint8(dst))
	a.emitU32(uint32(imm))
}

func (a *Assembler) ADDQ(dst, src Reg) { a.emitALURegReg(true, 0x01, dst, src) }
func (a *Assembler) ORQ(dst, src Reg)  { a.emitALURegReg(true, 0x09, dst, src) }
func (a *Assembler) ANDQ(dst, src Reg) { a.emitALURegReg(true, 0x21, dst, src) }
func (a *Assembler) SUBQ(dst, src Reg) { a.emitALURegReg(true, 0x29, dst, src) }
func (a *Assembler) XORQ(dst, src Reg) { a.emitALURegReg(true, 0x31, dst, src) }
func (a *Assembler) CMPQ(lhs, rhs Reg) { a.emitALURegReg(true, 0x39, lhs, rhs) }
func (a *Assembler) TESTQ(aReg, bReg Reg) {
	a.emitALURegReg(true, 0x85, aReg, bReg)
}

func (a *Assembler) ADDL(dst, src Reg) { a.emitALURegReg(false, 0x01, dst, src) }
func (a *Assembler) ORL(dst, src Reg)  { a.emitALURegReg(false, 0x09, dst, src) }
func (a *Assembler) ANDL(dst, src Reg) { a.emitALURegReg(false, 0x21, dst, src) }
func (a *Assembler) SUBL(dst, src Reg) { a.emitALURegReg(false, 0x29, dst, src) }
func (a *Assembler) XORL(dst, src Reg) { a.emitALURegReg(false, 0x31, dst, src) }
func (a *Assembler) CMPL(lhs, rhs Reg) { a.emitALURegReg(false, 0x39, lhs, rhs) }
func (a *Assembler) TESTL(aReg, bReg Reg) {
	a.emitALURegReg(false, 0x85, aReg, bReg)
}

func (a *Assembler) ADDQImm(dst Reg, imm int32) { a.emitALUImm(true, 0, dst, imm) }
func (a *Assembler) ORQImm(dst Reg, imm int32)  { a.emitALUImm(true, 1, dst, imm) }
func (a *Assembler) ANDQImm(dst Reg, imm int32) { a.emitALUImm(true, 4, dst, imm) }
func (a *Assembler) SUBQImm(dst Reg, imm int32) { a.emitALUImm(true, 5, dst, imm) }
func (a *Assembler) XORQImm(dst Reg, imm int32) { a.emitALUImm(true, 6, dst, imm) }
func (a *Assembler) CMPQImm(lhs Reg, imm int32) { a.emitALUImm(true, 7, lhs, imm) }

func (a *Assembler) ADDLImm(dst Reg, imm int32) { a.emitALUImm(false, 0, dst, imm) }
func (a *Assembler) ANDLImm(dst Reg, imm int32) { a.emitALUImm(false, 4, dst, imm) }
func (a *Assembler) SUBLImm(dst Reg, imm int32) { a.emitALUImm(false, 5, dst, imm) }
func (a *Assembler) CMPLImm(lhs Reg, imm int32) { a.emitALUImm(false, 7, lhs, imm) }

func (a *Assembler) TESTQImm(dst Reg, imm int32) {
	a.emitREXRegReg(true, 0, uint8(dst), false)
	a.emit1(0xF7)
	a.emitModRMReg(0, uint8(dst))
	a.emitU32(uint32(imm))
}

func (a *Assembler) CMPBMemImm(m Mem, imm uint8) {
	a.emitREXMem(false, 0, m, false)
	a.emit1(0x80)
	a.emitModRMMem(7, m)
	a.emit1(imm)
}

func (a *Assembler) CMPLMemImm(m Mem, imm int32) {
	a.emitREXMem(false, 0, m, false)
	if imm >= -128 && imm <= 127 {
		a.emit1(0x83)
		a.emitModRMMem(7, m)
		a.emit1(byte(int8(imm)))
		return
	}
	a.emit1(0x81)
	a.emitModRMMem(7, m)
	a.emitU32(uint32(imm))
}

func (a *Assembler) CMPQMemImm(m Mem, imm int32) {
	a.emitREXMem(true, 0, m, false)
	if imm >= -128 && imm <= 127 {
		a.emit1(0x83)
		a.emitModRMMem(7, m)
		a.emit1(byte(int8(imm)))
		return
	}
	a.emit1(0x81)
	a.emitModRMMem(7, m)
	a.emitU32(uint32(imm))
}

func (a *Assembler) ADDQMem(dst Reg, m Mem) {
	a.emitREXMem(true, uint8(dst), m, false)
	a.emit1(0x03)
	a.emitModRMMem(uint8(dst), m)
}

func (a *Assembler) SUBQMem(dst Reg, m Mem) {
	a.emitREXMem(true, uint8(dst), m, false)
	a.emit1(0x2B)
	a.emitModRMMem(uint8(dst), m)
}

func (a *Assembler) CMPQMem(lhs Reg, m Mem) {
	a.emitREXMem(true, uint8(lhs), m, false)
	a.emit1(0x3B)
	a.emitModRMMem(uint8(lhs), m)
}

func (a *Assembler) NEGQ(dst Reg) {
	a.emitREXRegReg(true, 0, uint8(dst), false)
	a.emit1(0xF7)
	a.emitModRMReg(3, uint8(dst))
}

func (a *Assembler) NOTQ(dst Reg) {
	a.emitREXRegReg(true, 0, uint8(dst), false)
	a.emit1(0xF7)
	a.emitModRMReg(2, uint8(dst))
}

// --- Shifts ---

func (a *Assembler) emitShiftImm(w bool, ext uint8, dst Reg, count uint8) {
	if count == 0 {
		if w {
			return
		}
		// 32-bit shifts with count 0 preserve flags while zero-extending the low 32 bits.
		a.MOVL(dst, dst)
		return
	}
	a.emitREXRegReg(w, 0, uint8(dst), false)
	if count == 1 {
		a.emit1(0xD1)
		a.emitModRMReg(ext, uint8(dst))
		return
	}
	a.emit1(0xC1)
	a.emitModRMReg(ext, uint8(dst))
	a.emit1(count)
}

func (a *Assembler) emitShiftCL(w bool, ext uint8, dst Reg) {
	a.emitREXRegReg(w, 0, uint8(dst), false)
	a.emit1(0xD3)
	a.emitModRMReg(ext, uint8(dst))
}

func (a *Assembler) SHLQImm(dst Reg, count uint8) { a.emitShiftImm(true, 4, dst, count) }
func (a *Assembler) SHRQImm(dst Reg, count uint8) { a.emitShiftImm(true, 5, dst, count) }
func (a *Assembler) SARQImm(dst Reg, count uint8) { a.emitShiftImm(true, 7, dst, count) }
func (a *Assembler) SHLLImm(dst Reg, count uint8) { a.emitShiftImm(false, 4, dst, count) }
func (a *Assembler) SHRLImm(dst Reg, count uint8) { a.emitShiftImm(false, 5, dst, count) }
func (a *Assembler) SHLQCL(dst Reg)               { a.emitShiftCL(true, 4, dst) }
func (a *Assembler) SHRQCL(dst Reg)               { a.emitShiftCL(true, 5, dst) }
func (a *Assembler) SARQCL(dst Reg)               { a.emitShiftCL(true, 7, dst) }

// --- Multiply & Divide ---

func (a *Assembler) IMULQ(dst, src Reg) {
	a.emitREXRegReg(true, uint8(dst), uint8(src), false)
	a.emit2(0x0F, 0xAF)
	a.emitModRMReg(uint8(dst), uint8(src))
}

func (a *Assembler) IMULQImm(dst, src Reg, imm int32) {
	a.emitREXRegReg(true, uint8(dst), uint8(src), false)
	if imm >= -128 && imm <= 127 {
		a.emit1(0x6B)
		a.emitModRMReg(uint8(dst), uint8(src))
		a.emit1(byte(int8(imm)))
		return
	}
	a.emit1(0x69)
	a.emitModRMReg(uint8(dst), uint8(src))
	a.emitU32(uint32(imm))
}

// CQO sign-extends RAX into RDX:RAX (48 99).
func (a *Assembler) CQO() { a.emit2(0x48, 0x99) }

// IDIVQ divides signed RDX:RAX by src, leaving quotient in RAX and remainder in RDX.
func (a *Assembler) IDIVQ(src Reg) {
	a.emitREXRegReg(true, 0, uint8(src), false)
	a.emit1(0xF7)
	a.emitModRMReg(7, uint8(src))
}

// DIVQ divides unsigned RDX:RAX by src, leaving quotient in RAX and remainder in RDX.
func (a *Assembler) DIVQ(src Reg) {
	a.emitREXRegReg(true, 0, uint8(src), false)
	a.emit1(0xF7)
	a.emitModRMReg(6, uint8(src))
}

// --- Conditional Set & Move ---

// SETCC writes 1 or 0 to the low byte of dst according to cond (upper 56 bits untouched).
func (a *Assembler) SETCC(cond Cond, dst Reg) {
	a.emitREXRegReg(false, 0, uint8(dst), dst >= 4)
	a.emit2(0x0F, 0x90|byte(cond&0xF))
	a.emitModRMReg(0, uint8(dst))
}

// MOVZXBQReg zero-extends the low byte of src into 64-bit dst.
func (a *Assembler) MOVZXBQReg(dst, src Reg) {
	a.emitREXRegReg(false, uint8(dst), uint8(src), src >= 4)
	a.emit2(0x0F, 0xB6)
	a.emitModRMReg(uint8(dst), uint8(src))
}

func (a *Assembler) CMOVQ(cond Cond, dst, src Reg) {
	a.emitREXRegReg(true, uint8(dst), uint8(src), false)
	a.emit2(0x0F, 0x40|byte(cond&0xF))
	a.emitModRMReg(uint8(dst), uint8(src))
}

// --- SSE2 / SSE4.1 Scalar Double-Precision Floating Point ---

func (a *Assembler) emitSSE2RegReg(prefix byte, op byte, dst, src XReg) {
	if prefix != 0 {
		a.emit1(prefix)
	}
	a.emitREXRegReg(false, uint8(dst), uint8(src), false)
	a.emit2(0x0F, op)
	a.emitModRMReg(uint8(dst), uint8(src))
}

func (a *Assembler) emitSSE2RegMem(prefix byte, op byte, reg XReg, m Mem) {
	if prefix != 0 {
		a.emit1(prefix)
	}
	a.emitREXMem(false, uint8(reg), m, false)
	a.emit2(0x0F, op)
	a.emitModRMMem(uint8(reg), m)
}

func (a *Assembler) MOVQToX(dst XReg, src Reg) {
	a.emit1(0x66)
	a.emitREXRegReg(true, uint8(dst), uint8(src), false)
	a.emit2(0x0F, 0x6E)
	a.emitModRMReg(uint8(dst), uint8(src))
}

func (a *Assembler) MOVQFromX(dst Reg, src XReg) {
	a.emit1(0x66)
	a.emitREXRegReg(true, uint8(src), uint8(dst), false)
	a.emit2(0x0F, 0x7E)
	a.emitModRMReg(uint8(src), uint8(dst))
}

func (a *Assembler) MOVSD(dst, src XReg)       { a.emitSSE2RegReg(0xF2, 0x10, dst, src) }
func (a *Assembler) MOVSDLoad(dst XReg, m Mem) { a.emitSSE2RegMem(0xF2, 0x10, dst, m) }
func (a *Assembler) MOVSDStore(m Mem, src XReg) {
	a.emitSSE2RegMem(0xF2, 0x11, src, m)
}

func (a *Assembler) ADDSD(dst, src XReg)       { a.emitSSE2RegReg(0xF2, 0x58, dst, src) }
func (a *Assembler) SUBSD(dst, src XReg)       { a.emitSSE2RegReg(0xF2, 0x5C, dst, src) }
func (a *Assembler) MULSD(dst, src XReg)       { a.emitSSE2RegReg(0xF2, 0x59, dst, src) }
func (a *Assembler) DIVSD(dst, src XReg)       { a.emitSSE2RegReg(0xF2, 0x5E, dst, src) }
func (a *Assembler) SQRTSD(dst, src XReg)      { a.emitSSE2RegReg(0xF2, 0x51, dst, src) }
func (a *Assembler) MINSD(dst, src XReg)       { a.emitSSE2RegReg(0xF2, 0x5D, dst, src) }
func (a *Assembler) MAXSD(dst, src XReg)       { a.emitSSE2RegReg(0xF2, 0x5F, dst, src) }
func (a *Assembler) ADDSDMem(dst XReg, m Mem)  { a.emitSSE2RegMem(0xF2, 0x58, dst, m) }
func (a *Assembler) SUBSDMem(dst XReg, m Mem)  { a.emitSSE2RegMem(0xF2, 0x5C, dst, m) }
func (a *Assembler) MULSDMem(dst XReg, m Mem)  { a.emitSSE2RegMem(0xF2, 0x59, dst, m) }
func (a *Assembler) DIVSDMem(dst XReg, m Mem)  { a.emitSSE2RegMem(0xF2, 0x5E, dst, m) }
func (a *Assembler) UCOMISD(lhs, rhs XReg)     { a.emitSSE2RegReg(0x66, 0x2E, lhs, rhs) }
func (a *Assembler) XORPD(dst, src XReg)       { a.emitSSE2RegReg(0x66, 0x57, dst, src) }
func (a *Assembler) ANDPD(dst, src XReg)       { a.emitSSE2RegReg(0x66, 0x54, dst, src) }
func (a *Assembler) XORPS(dst, src XReg)       { a.emitSSE2RegReg(0x00, 0x57, dst, src) }

// CVTSI2SDQ converts signed int64 in src to float64 in dst.
func (a *Assembler) CVTSI2SDQ(dst XReg, src Reg) {
	a.emit1(0xF2)
	a.emitREXRegReg(true, uint8(dst), uint8(src), false)
	a.emit2(0x0F, 0x2A)
	a.emitModRMReg(uint8(dst), uint8(src))
}

// CVTTSD2SIQ truncates float64 in src toward zero into signed int64 in dst.
func (a *Assembler) CVTTSD2SIQ(dst Reg, src XReg) {
	a.emit1(0xF2)
	a.emitREXRegReg(true, uint8(dst), uint8(src), false)
	a.emit2(0x0F, 0x2C)
	a.emitModRMReg(uint8(dst), uint8(src))
}

// ROUNDSD rounding modes (SSE4.1, with precision-exception suppression bit 0x08 set).
const (
	RoundNearest byte = 0x08
	RoundFloor   byte = 0x09
	RoundCeil    byte = 0x0A
	RoundTrunc   byte = 0x0B
)

// ROUNDSD emits SSE4.1 ROUNDSD dst, src, mode. Callers must gate on SSE4.1 support.
func (a *Assembler) ROUNDSD(dst, src XReg, mode byte) {
	a.emit1(0x66)
	a.emitREXRegReg(false, uint8(dst), uint8(src), false)
	a.emit3(0x0F, 0x3A, 0x0B)
	a.emitModRMReg(uint8(dst), uint8(src))
	a.emit1(mode)
}

// --- Control Flow & Stack ---

func (a *Assembler) JMP(l Label) {
	a.emit1(0xE9)
	off := len(a.code)
	a.emitU32(0)
	a.patch = append(a.patch, patch{dispOff: off, nextPC: len(a.code), label: l})
}

func (a *Assembler) JCC(cond Cond, l Label) {
	a.emit2(0x0F, 0x80|byte(cond&0xF))
	off := len(a.code)
	a.emitU32(0)
	a.patch = append(a.patch, patch{dispOff: off, nextPC: len(a.code), label: l})
}

func (a *Assembler) CALLRel(l Label) {
	a.emit1(0xE8)
	off := len(a.code)
	a.emitU32(0)
	a.patch = append(a.patch, patch{dispOff: off, nextPC: len(a.code), label: l})
}

func (a *Assembler) CALLReg(reg Reg) {
	a.emitREXRegReg(false, 0, uint8(reg), false)
	a.emit1(0xFF)
	a.emitModRMReg(2, uint8(reg))
}

func (a *Assembler) JMPReg(reg Reg) {
	a.emitREXRegReg(false, 0, uint8(reg), false)
	a.emit1(0xFF)
	a.emitModRMReg(4, uint8(reg))
}

func (a *Assembler) PUSHQ(reg Reg) {
	a.emitREX(false, false, false, reg >= 8, false)
	a.emit1(0x50 | byte(reg&7))
}

func (a *Assembler) POPQ(reg Reg) {
	a.emitREX(false, false, false, reg >= 8, false)
	a.emit1(0x58 | byte(reg&7))
}

func (a *Assembler) RET() { a.emit1(0xC3) }
func (a *Assembler) NOP() { a.emit1(0x90) }

// --- Prologue & Epilogue Helpers ---

// FrameLayout describes a 16-byte aligned x86-64 stack frame emitted by EmitPrologue/EmitEpilogue.
// On entry after CALL (RSP%16 == 8), EmitPrologue pushes RBP (RSP%16 == 0), pushes SavedRegs,
// and subtracts SubBytes so RSP remains 16-byte aligned inside the JIT body.
type FrameLayout struct {
	LocalBytes int32
	SubBytes   int32
	SavedRegs  []Reg
}

// ComputeFrameLayout computes SubBytes so RSP is 16-byte aligned after pushing RBP and savedRegs.
func ComputeFrameLayout(localBytes int32, savedRegs []Reg) FrameLayout {
	if localBytes < 0 {
		localBytes = 0
	}
	sub := (localBytes + 7) &^ 7
	// After PUSH RBP, RSP is 16-byte aligned. Each saved reg shifts RSP by 8 bytes.
	if (int32(len(savedRegs))*8+sub)%16 != 0 {
		sub += 8
	}
	cp := make([]Reg, len(savedRegs))
	copy(cp, savedRegs)
	return FrameLayout{
		LocalBytes: localBytes,
		SubBytes:   sub,
		SavedRegs:  cp,
	}
}

// EmitPrologue establishes the frame pointer, pushes SavedRegs, and allocates SubBytes on RSP.
func (a *Assembler) EmitPrologue(fl FrameLayout) {
	a.PUSHQ(RBP)
	a.MOVQ(RBP, RSP)
	for _, r := range fl.SavedRegs {
		a.PUSHQ(r)
	}
	if fl.SubBytes > 0 {
		a.SUBQImm(RSP, fl.SubBytes)
	}
}

// EmitEpilogue deallocates SubBytes, pops SavedRegs in reverse order, restores RBP, and emits RET.
func (a *Assembler) EmitEpilogue(fl FrameLayout) {
	if fl.SubBytes > 0 {
		a.ADDQImm(RSP, fl.SubBytes)
	}
	for i := len(fl.SavedRegs) - 1; i >= 0; i-- {
		a.POPQ(fl.SavedRegs[i])
	}
	a.POPQ(RBP)
	a.RET()
}
