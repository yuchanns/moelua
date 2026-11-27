// Package arm64 implements a self-contained, zero-dependency AArch64 (ARM64) machine-code
// assembler for MoeLua's native JIT compiler.
//
// All methods record encoding or label-resolution errors internally and surface them
// cleanly via Finish so callers never panic on out-of-range branches or unbound labels.
package arm64

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
)

// Reg identifies a 64-bit ARM64 general-purpose register (X0..X30, plus XZR/SP = 31).
type Reg uint8

const (
	X0  Reg = 0
	X1  Reg = 1
	X2  Reg = 2
	X3  Reg = 3
	X4  Reg = 4
	X5  Reg = 5
	X6  Reg = 6
	X7  Reg = 7
	X8  Reg = 8
	X9  Reg = 9
	X10 Reg = 10
	X11 Reg = 11
	X12 Reg = 12
	X13 Reg = 13
	X14 Reg = 14
	X15 Reg = 15
	X16 Reg = 16
	X17 Reg = 17
	X18 Reg = 18 // Platform register (reserved on Darwin/Windows; never clobber)
	X19 Reg = 19
	X20 Reg = 20
	X21 Reg = 21
	X22 Reg = 22
	X23 Reg = 23
	X24 Reg = 24
	X25 Reg = 25
	X26 Reg = 26
	X27 Reg = 27
	X28 Reg = 28 // Go runtime g register (never clobber)
	X29 Reg = 29 // Frame pointer (FP)
	X30 Reg = 30 // Link register (LR)
	XZR Reg = 31 // Zero register in data-processing instructions
	SP  Reg = 31 // Stack pointer in base-address / ADD(S)/SUB(S) immediate instructions
)

// FReg identifies an ARM64 64-bit scalar floating-point register (D0..D31).
type FReg uint8

const (
	D0  FReg = 0
	D1  FReg = 1
	D2  FReg = 2
	D3  FReg = 3
	D4  FReg = 4
	D5  FReg = 5
	D6  FReg = 6
	D7  FReg = 7
	D8  FReg = 8
	D9  FReg = 9
	D10 FReg = 10
	D11 FReg = 11
	D12 FReg = 12
	D13 FReg = 13
	D14 FReg = 14
	D15 FReg = 15
	D16 FReg = 16
	D17 FReg = 17
	D18 FReg = 18
	D19 FReg = 19
	D20 FReg = 20
	D21 FReg = 21
	D22 FReg = 22
	D23 FReg = 23
	D24 FReg = 24
	D25 FReg = 25
	D26 FReg = 26
	D27 FReg = 27
	D28 FReg = 28
	D29 FReg = 29
	D30 FReg = 30
	D31 FReg = 31
)

// Cond identifies an ARM64 condition code.
type Cond uint8

const (
	EQ Cond = 0x0 // Equal (Z == 1)
	NE Cond = 0x1 // Not equal (Z == 0)
	CS Cond = 0x2 // Carry set / unsigned >= (C == 1)
	HS Cond = 0x2 // Unsigned >=
	CC Cond = 0x3 // Carry clear / unsigned < (C == 0)
	LO Cond = 0x3 // Unsigned <
	MI Cond = 0x4 // Minus / negative (N == 1)
	PL Cond = 0x5 // Plus / non-negative (N == 0)
	VS Cond = 0x6 // Overflow (V == 1)
	VC Cond = 0x7 // No overflow (V == 0)
	HI Cond = 0x8 // Unsigned > (C == 1 && Z == 0)
	LS Cond = 0x9 // Unsigned <= (C == 0 || Z == 1)
	GE Cond = 0xA // Signed >= (N == V)
	LT Cond = 0xB // Signed < (N != V)
	GT Cond = 0xC // Signed > (Z == 0 && N == V)
	LE Cond = 0xD // Signed <= (Z == 1 || N != V)
	AL Cond = 0xE // Always
)

// Invert returns the logical negation of condition c.
func (c Cond) Invert() Cond {
	return c ^ 1
}

// Shift specifies a register shift kind in shifted-register ALU instructions.
type Shift uint8

const (
	ShiftLSL Shift = 0
	ShiftLSR Shift = 1
	ShiftASR Shift = 2
)

// Label represents a bindable code position within an Assembler.
type Label int

type patchKind uint8

const (
	patchB26 patchKind = iota // B / BL (signed 26-bit instruction offset)
	patchB19                  // B.cond / CBZ / CBNZ (signed 19-bit instruction offset, bits 23:5)
	patchB14                  // TBZ / TBNZ (signed 14-bit instruction offset, bits 18:5)
)

type patch struct {
	insnIdx int
	label   Label
	kind    patchKind
}

// Assembler encodes ARM64 instructions into a little-endian byte buffer with two-pass
// PC-relative label resolution.
type Assembler struct {
	insns  []uint32
	labels []int // instruction index per label (-1 if unbound)
	patch  []patch
	err    error
}

// New creates an Assembler with an optional initial instruction capacity.
func New(capInsns int) *Assembler {
	if capInsns <= 0 {
		capInsns = 64
	}
	return &Assembler{
		insns: make([]uint32, 0, capInsns),
	}
}

// Reset clears the Assembler for reuse while keeping backing storage.
func (a *Assembler) Reset() {
	a.insns = a.insns[:0]
	a.labels = a.labels[:0]
	a.patch = a.patch[:0]
	a.err = nil
}

// Len returns the current emitted byte length.
func (a *Assembler) Len() int { return len(a.insns) * 4 }

// Pos returns the current instruction index (Len()/4).
func (a *Assembler) Pos() int { return len(a.insns) }

// Err returns the first encoding error encountered, if any.
func (a *Assembler) Err() error { return a.err }

func (a *Assembler) setErr(err error) {
	if a.err == nil {
		a.err = err
	}
}

func (a *Assembler) emit(w uint32) {
	a.insns = append(a.insns, w)
}

// NewLabel allocates a fresh unbound Label.
func (a *Assembler) NewLabel() Label {
	id := Label(len(a.labels))
	a.labels = append(a.labels, -1)
	return id
}

// Bind binds l to the current instruction position.
func (a *Assembler) Bind(l Label) {
	if int(l) < 0 || int(l) >= len(a.labels) {
		a.setErr(fmt.Errorf("arm64: invalid label %d", l))
		return
	}
	if a.labels[l] != -1 {
		a.setErr(fmt.Errorf("arm64: label %d bound more than once", l))
		return
	}
	a.labels[l] = len(a.insns)
}

// Finish resolves all label fixups and returns the encoded little-endian machine code.
func (a *Assembler) Finish() ([]byte, error) {
	if a.err != nil {
		return nil, a.err
	}
	for _, p := range a.patch {
		if int(p.label) < 0 || int(p.label) >= len(a.labels) {
			return nil, fmt.Errorf("arm64: invalid label %d in branch", p.label)
		}
		target := a.labels[p.label]
		if target < 0 {
			return nil, fmt.Errorf("arm64: unbound label %d", p.label)
		}
		delta := target - p.insnIdx
		switch p.kind {
		case patchB26:
			if delta < -(1<<25) || delta >= (1<<25) {
				return nil, fmt.Errorf("arm64: B/BL offset %d out of range", delta)
			}
			a.insns[p.insnIdx] |= uint32(delta) & 0x03FFFFFF
		case patchB19:
			if delta < -(1<<18) || delta >= (1<<18) {
				return nil, fmt.Errorf("arm64: B.cond/CBZ offset %d out of range", delta)
			}
			a.insns[p.insnIdx] |= (uint32(delta) & 0x7FFFF) << 5
		case patchB14:
			if delta < -(1<<13) || delta >= (1<<13) {
				return nil, fmt.Errorf("arm64: TBZ/TBNZ offset %d out of range", delta)
			}
			a.insns[p.insnIdx] |= (uint32(delta) & 0x3FFF) << 5
		default:
			return nil, errors.New("arm64: unknown patch kind")
		}
	}
	out := make([]byte, len(a.insns)*4)
	for i, w := range a.insns {
		binary.LittleEndian.PutUint32(out[i*4:], w)
	}
	return out, nil
}

// --- Moves & Immediate Constants ---

// MOV emits a 64-bit GPR-to-GPR move (ORR Xd, XZR, Xm). Do not pass SP; use MOVSP for SP.
func (a *Assembler) MOV(rd, rm Reg) {
	a.emit(0xAA0003E0 | uint32(rm&31)<<16 | uint32(rd&31))
}

// MOVSP emits ADD Xd, Xn, #0, valid when either operand is SP.
func (a *Assembler) MOVSP(rd, rn Reg) {
	a.emit(0x91000000 | uint32(rn&31)<<5 | uint32(rd&31))
}

// MOVZ emits MOVZ Xd, #imm16, LSL #(hw*16) with hw in 0..3.
func (a *Assembler) MOVZ(rd Reg, imm16 uint16, hw uint8) {
	if hw > 3 {
		a.setErr(fmt.Errorf("arm64: MOVZ shift hw=%d out of range", hw))
		return
	}
	a.emit(0xD2800000 | uint32(hw)<<21 | uint32(imm16)<<5 | uint32(rd&31))
}

// MOVN emits MOVN Xd, #imm16, LSL #(hw*16) with hw in 0..3.
func (a *Assembler) MOVN(rd Reg, imm16 uint16, hw uint8) {
	if hw > 3 {
		a.setErr(fmt.Errorf("arm64: MOVN shift hw=%d out of range", hw))
		return
	}
	a.emit(0x92800000 | uint32(hw)<<21 | uint32(imm16)<<5 | uint32(rd&31))
}

// MOVK emits MOVK Xd, #imm16, LSL #(hw*16) with hw in 0..3.
func (a *Assembler) MOVK(rd Reg, imm16 uint16, hw uint8) {
	if hw > 3 {
		a.setErr(fmt.Errorf("arm64: MOVK shift hw=%d out of range", hw))
		return
	}
	a.emit(0xF2800000 | uint32(hw)<<21 | uint32(imm16)<<5 | uint32(rd&31))
}

// MOVImm64 loads an arbitrary 64-bit constant into rd using the fewest MOVZ/MOVN/MOVK
// instructions (1 to 4 instructions).
func (a *Assembler) MOVImm64(rd Reg, val uint64) {
	if val == 0 {
		a.MOVZ(rd, 0, 0)
		return
	}
	if val == ^uint64(0) {
		a.MOVN(rd, 0, 0)
		return
	}
	var chunks [4]uint16
	zeroChunks := 0
	onesChunks := 0
	for i := range 4 {
		c := uint16(val >> (uint(i) * 16))
		chunks[i] = c
		if c == 0 {
			zeroChunks++
		} else if c == 0xFFFF {
			onesChunks++
		}
	}
	if onesChunks > zeroChunks {
		// Start with MOVN on the first non-0xFFFF chunk, then MOVK the rest.
		first := true
		for i := range 4 {
			if chunks[i] == 0xFFFF {
				continue
			}
			if first {
				a.MOVN(rd, ^chunks[i], uint8(i))
				first = false
			} else {
				a.MOVK(rd, chunks[i], uint8(i))
			}
		}
		return
	}
	first := true
	for i := range 4 {
		if chunks[i] == 0 {
			continue
		}
		if first {
			a.MOVZ(rd, chunks[i], uint8(i))
			first = false
		} else {
			a.MOVK(rd, chunks[i], uint8(i))
		}
	}
}

// MOVImm32 loads a zero-extended 32-bit constant into rd.
func (a *Assembler) MOVImm32(rd Reg, val uint32) {
	a.MOVImm64(rd, uint64(val))
}

// --- Integer ALU (Immediate & Shifted Register) ---

func encodeAddSubImm(imm uint32) (imm12 uint32, shift12 uint32, ok bool) {
	if imm < 1<<12 {
		return imm, 0, true
	}
	if imm&(0xFFF) == 0 && (imm>>12) < 1<<12 {
		return imm >> 12, 1, true
	}
	return 0, 0, false
}

func (a *Assembler) emitAddSubImm(base uint32, rd, rn Reg, imm uint32) {
	imm12, sh, ok := encodeAddSubImm(imm)
	if !ok {
		a.setErr(fmt.Errorf("arm64: add/sub immediate 0x%x not encodable in 12 bits", imm))
		return
	}
	a.emit(base | sh<<22 | imm12<<10 | uint32(rn&31)<<5 | uint32(rd&31))
}

func (a *Assembler) ADDImm(rd, rn Reg, imm uint32)  { a.emitAddSubImm(0x91000000, rd, rn, imm) }
func (a *Assembler) ADDSImm(rd, rn Reg, imm uint32) { a.emitAddSubImm(0xB1000000, rd, rn, imm) }
func (a *Assembler) SUBImm(rd, rn Reg, imm uint32)  { a.emitAddSubImm(0xD1000000, rd, rn, imm) }
func (a *Assembler) SUBSImm(rd, rn Reg, imm uint32) { a.emitAddSubImm(0xF1000000, rd, rn, imm) }
func (a *Assembler) ADDWImm(rd, rn Reg, imm uint32) { a.emitAddSubImm(0x11000000, rd, rn, imm) }
func (a *Assembler) SUBWImm(rd, rn Reg, imm uint32) { a.emitAddSubImm(0x51000000, rd, rn, imm) }

func (a *Assembler) CMPImm(rn Reg, imm uint32)  { a.emitAddSubImm(0xF1000000, XZR, rn, imm) }
func (a *Assembler) CMNImm(rn Reg, imm uint32)  { a.emitAddSubImm(0xB1000000, XZR, rn, imm) }
func (a *Assembler) CMPWImm(rn Reg, imm uint32) { a.emitAddSubImm(0x71000000, XZR, rn, imm) }

func (a *Assembler) emitAddSubShift(base uint32, rd, rn, rm Reg, sh Shift, amt uint8) {
	if sh > ShiftASR || amt > 63 {
		a.setErr(fmt.Errorf("arm64: invalid shift kind=%d amt=%d", sh, amt))
		return
	}
	a.emit(base | uint32(sh)<<22 | uint32(rm&31)<<16 | uint32(amt)<<10 | uint32(rn&31)<<5 | uint32(rd&31))
}

func (a *Assembler) ADD(rd, rn, rm Reg)  { a.emitAddSubShift(0x8B000000, rd, rn, rm, ShiftLSL, 0) }
func (a *Assembler) ADDS(rd, rn, rm Reg) { a.emitAddSubShift(0xAB000000, rd, rn, rm, ShiftLSL, 0) }
func (a *Assembler) SUB(rd, rn, rm Reg)  { a.emitAddSubShift(0xCB000000, rd, rn, rm, ShiftLSL, 0) }
func (a *Assembler) SUBS(rd, rn, rm Reg) { a.emitAddSubShift(0xEB000000, rd, rn, rm, ShiftLSL, 0) }

func (a *Assembler) ADDShift(rd, rn, rm Reg, sh Shift, amt uint8) {
	a.emitAddSubShift(0x8B000000, rd, rn, rm, sh, amt)
}
func (a *Assembler) SUBShift(rd, rn, rm Reg, sh Shift, amt uint8) {
	a.emitAddSubShift(0xCB000000, rd, rn, rm, sh, amt)
}

func (a *Assembler) CMP(rn, rm Reg)  { a.emitAddSubShift(0xEB000000, XZR, rn, rm, ShiftLSL, 0) }
func (a *Assembler) CMN(rn, rm Reg)  { a.emitAddSubShift(0xAB000000, XZR, rn, rm, ShiftLSL, 0) }
func (a *Assembler) CMPW(rn, rm Reg) { a.emitAddSubShift(0x6B000000, XZR, rn, rm, ShiftLSL, 0) }
func (a *Assembler) NEG(rd, rm Reg)  { a.SUB(rd, XZR, rm) }
func (a *Assembler) NEGS(rd, rm Reg) { a.SUBS(rd, XZR, rm) }

// --- Logical Operations ---

func (a *Assembler) emitLogicalReg(base uint32, rd, rn, rm Reg, sh Shift, amt uint8) {
	if amt > 63 {
		a.setErr(fmt.Errorf("arm64: logical shift amt=%d out of range", amt))
		return
	}
	a.emit(base | uint32(sh)<<22 | uint32(rm&31)<<16 | uint32(amt)<<10 | uint32(rn&31)<<5 | uint32(rd&31))
}

func (a *Assembler) AND(rd, rn, rm Reg)  { a.emitLogicalReg(0x8A000000, rd, rn, rm, ShiftLSL, 0) }
func (a *Assembler) ANDS(rd, rn, rm Reg) { a.emitLogicalReg(0xEA000000, rd, rn, rm, ShiftLSL, 0) }
func (a *Assembler) BIC(rd, rn, rm Reg)  { a.emitLogicalReg(0x8A200000, rd, rn, rm, ShiftLSL, 0) }
func (a *Assembler) ORR(rd, rn, rm Reg)  { a.emitLogicalReg(0xAA000000, rd, rn, rm, ShiftLSL, 0) }
func (a *Assembler) ORN(rd, rn, rm Reg)  { a.emitLogicalReg(0xAA200000, rd, rn, rm, ShiftLSL, 0) }
func (a *Assembler) EOR(rd, rn, rm Reg)  { a.emitLogicalReg(0xCA000000, rd, rn, rm, ShiftLSL, 0) }
func (a *Assembler) MVN(rd, rm Reg)      { a.ORN(rd, XZR, rm) }
func (a *Assembler) TST(rn, rm Reg)      { a.ANDS(XZR, rn, rm) }
func (a *Assembler) TSTW(rn, rm Reg)     { a.emitLogicalReg(0x6A000000, XZR, rn, rm, ShiftLSL, 0) }

// EncodeLogicalImm64 encodes a 64-bit ARM64 logical bitmask immediate into (N, immr, imms).
// Returns ok=false for 0, ^uint64(0), or non-repeating/non-contiguous bit patterns.
func EncodeLogicalImm64(v uint64) (n, immr, imms uint32, ok bool) {
	if v == 0 || v == ^uint64(0) {
		return 0, 0, 0, false
	}
	// Find smallest repeating element size: 2, 4, 8, 16, 32, or 64.
	size := uint(64)
	for size > 2 {
		half := size / 2
		mask := (uint64(1) << half) - 1
		if (v & mask) != ((v >> half) & mask) {
			break
		}
		v &= mask
		size = half
	}
	elemMask := ^uint64(0)
	if size < 64 {
		elemMask = (uint64(1) << size) - 1
	}
	elem := v & elemMask
	// Verify elem is a single rotated contiguous run of ones within 'size' bits.
	// Find a rotation where bit 0 is 1 and bit size-1 is 0.
	var rot uint
	for rot = 0; rot < size; rot++ {
		if (elem&1) == 1 && ((elem>>(size-1))&1) == 0 {
			break
		}
		elem = ((elem >> 1) | ((elem & 1) << (size - 1))) & elemMask
	}
	if rot == size {
		return 0, 0, 0, false
	}
	ones := uint(bits.OnesCount64(elem))
	if ones == 0 || ones == size || elem != (uint64(1)<<ones)-1 {
		return 0, 0, 0, false
	}
	// Right-rotation from canonical pattern to original elem is (size - rot) % size.
	immr = uint32((size - rot) % size)
	if size == 64 {
		n = 1
		imms = uint32(ones - 1)
	} else {
		n = 0
		imms = uint32((^(size*2 - 1) & 0x3F) | (ones - 1))
	}
	return n, immr, imms, true
}

func (a *Assembler) emitLogicalImm(base uint32, rd, rn Reg, imm uint64) {
	n, immr, imms, ok := EncodeLogicalImm64(imm)
	if !ok {
		a.setErr(fmt.Errorf("arm64: logical immediate 0x%x not encodable", imm))
		return
	}
	a.emit(base | n<<22 | immr<<16 | imms<<10 | uint32(rn&31)<<5 | uint32(rd&31))
}

func (a *Assembler) ANDImm(rd, rn Reg, imm uint64)  { a.emitLogicalImm(0x92000000, rd, rn, imm) }
func (a *Assembler) ORRImm(rd, rn Reg, imm uint64)  { a.emitLogicalImm(0xB2000000, rd, rn, imm) }
func (a *Assembler) EORImm(rd, rn Reg, imm uint64)  { a.emitLogicalImm(0xD2000000, rd, rn, imm) }
func (a *Assembler) ANDSImm(rd, rn Reg, imm uint64) { a.emitLogicalImm(0xF2000000, rd, rn, imm) }
func (a *Assembler) TSTImm(rn Reg, imm uint64)      { a.ANDSImm(XZR, rn, imm) }

// --- Shifts & Bitfields ---

func (a *Assembler) LSL(rd, rn, rm Reg) {
	a.emit(0x9AC02000 | uint32(rm&31)<<16 | uint32(rn&31)<<5 | uint32(rd&31))
}
func (a *Assembler) LSR(rd, rn, rm Reg) {
	a.emit(0x9AC02400 | uint32(rm&31)<<16 | uint32(rn&31)<<5 | uint32(rd&31))
}
func (a *Assembler) ASR(rd, rn, rm Reg) {
	a.emit(0x9AC02800 | uint32(rm&31)<<16 | uint32(rn&31)<<5 | uint32(rd&31))
}

func (a *Assembler) UBFM(rd, rn Reg, immr, imms uint8) {
	if immr > 63 || imms > 63 {
		a.setErr(fmt.Errorf("arm64: UBFM immr=%d imms=%d out of range", immr, imms))
		return
	}
	a.emit(0xD3400000 | uint32(immr)<<16 | uint32(imms)<<10 | uint32(rn&31)<<5 | uint32(rd&31))
}

func (a *Assembler) SBFM(rd, rn Reg, immr, imms uint8) {
	if immr > 63 || imms > 63 {
		a.setErr(fmt.Errorf("arm64: SBFM immr=%d imms=%d out of range", immr, imms))
		return
	}
	a.emit(0x93400000 | uint32(immr)<<16 | uint32(imms)<<10 | uint32(rn&31)<<5 | uint32(rd&31))
}

func (a *Assembler) LSLImm(rd, rn Reg, shift uint8) {
	if shift == 0 {
		a.MOV(rd, rn)
		return
	}
	if shift > 63 {
		a.setErr(fmt.Errorf("arm64: LSLImm shift=%d out of range", shift))
		return
	}
	a.UBFM(rd, rn, 64-shift, 63-shift)
}

func (a *Assembler) LSRImm(rd, rn Reg, shift uint8) {
	if shift > 63 {
		a.setErr(fmt.Errorf("arm64: LSRImm shift=%d out of range", shift))
		return
	}
	a.UBFM(rd, rn, shift, 63)
}

func (a *Assembler) ASRImm(rd, rn Reg, shift uint8) {
	if shift > 63 {
		a.setErr(fmt.Errorf("arm64: ASRImm shift=%d out of range", shift))
		return
	}
	a.SBFM(rd, rn, shift, 63)
}

func (a *Assembler) UBFX(rd, rn Reg, lsb, width uint8) {
	if width == 0 || uint16(lsb)+uint16(width) > 64 {
		a.setErr(fmt.Errorf("arm64: UBFX lsb=%d width=%d out of range", lsb, width))
		return
	}
	a.UBFM(rd, rn, lsb, lsb+width-1)
}

func (a *Assembler) SBFX(rd, rn Reg, lsb, width uint8) {
	if width == 0 || uint16(lsb)+uint16(width) > 64 {
		a.setErr(fmt.Errorf("arm64: SBFX lsb=%d width=%d out of range", lsb, width))
		return
	}
	a.SBFM(rd, rn, lsb, lsb+width-1)
}

// SXTW sign-extends the low 32 bits of rn into 64-bit rd.
func (a *Assembler) SXTW(rd, rn Reg) { a.SBFM(rd, rn, 0, 31) }

// UXTW zero-extends the low 32 bits of rn into 64-bit rd (MOV Wd, Wn).
func (a *Assembler) UXTW(rd, rn Reg) {
	a.emit(0x2A0003E0 | uint32(rn&31)<<16 | uint32(rd&31))
}

// --- Multiply & Divide ---

func (a *Assembler) MADD(rd, rn, rm, ra Reg) {
	a.emit(0x9B000000 | uint32(rm&31)<<16 | uint32(ra&31)<<10 | uint32(rn&31)<<5 | uint32(rd&31))
}
func (a *Assembler) MSUB(rd, rn, rm, ra Reg) {
	a.emit(0x9B008000 | uint32(rm&31)<<16 | uint32(ra&31)<<10 | uint32(rn&31)<<5 | uint32(rd&31))
}
func (a *Assembler) MUL(rd, rn, rm Reg) { a.MADD(rd, rn, rm, XZR) }
func (a *Assembler) SMULH(rd, rn, rm Reg) {
	a.emit(0x9B407C00 | uint32(rm&31)<<16 | uint32(rn&31)<<5 | uint32(rd&31))
}
func (a *Assembler) UDIV(rd, rn, rm Reg) {
	a.emit(0x9AC00800 | uint32(rm&31)<<16 | uint32(rn&31)<<5 | uint32(rd&31))
}
func (a *Assembler) SDIV(rd, rn, rm Reg) {
	a.emit(0x9AC00C00 | uint32(rm&31)<<16 | uint32(rn&31)<<5 | uint32(rd&31))
}

// --- Conditional Select ---

func (a *Assembler) CSEL(rd, rn, rm Reg, cond Cond) {
	a.emit(0x9A800000 | uint32(rm&31)<<16 | uint32(cond&0xF)<<12 | uint32(rn&31)<<5 | uint32(rd&31))
}
func (a *Assembler) CSINC(rd, rn, rm Reg, cond Cond) {
	a.emit(0x9A800400 | uint32(rm&31)<<16 | uint32(cond&0xF)<<12 | uint32(rn&31)<<5 | uint32(rd&31))
}
func (a *Assembler) CSNEG(rd, rn, rm Reg, cond Cond) {
	a.emit(0xDA800400 | uint32(rm&31)<<16 | uint32(cond&0xF)<<12 | uint32(rn&31)<<5 | uint32(rd&31))
}
func (a *Assembler) CSET(rd Reg, cond Cond)      { a.CSINC(rd, XZR, XZR, cond.Invert()) }
func (a *Assembler) CNEG(rd, rn Reg, cond Cond)  { a.CSNEG(rd, rn, rn, cond.Invert()) }

// --- Memory Loads & Stores ---

func (a *Assembler) emitLdStUImm(base uint32, rt, rn Reg, off int32, scale int32) {
	if off < 0 || off%scale != 0 || off/scale >= (1<<12) {
		a.setErr(fmt.Errorf("arm64: unsigned offset %d invalid for scale %d", off, scale))
		return
	}
	a.emit(base | uint32(off/scale)<<10 | uint32(rn&31)<<5 | uint32(rt&31))
}

func (a *Assembler) LDR(rt, rn Reg, off int32)   { a.emitLdStUImm(0xF9400000, rt, rn, off, 8) }
func (a *Assembler) STR(rt, rn Reg, off int32)   { a.emitLdStUImm(0xF9000000, rt, rn, off, 8) }
func (a *Assembler) LDRW(rt, rn Reg, off int32)  { a.emitLdStUImm(0xB9400000, rt, rn, off, 4) }
func (a *Assembler) STRW(rt, rn Reg, off int32)  { a.emitLdStUImm(0xB9000000, rt, rn, off, 4) }
func (a *Assembler) LDRSW(rt, rn Reg, off int32) { a.emitLdStUImm(0xB9800000, rt, rn, off, 4) }
func (a *Assembler) LDRH(rt, rn Reg, off int32)  { a.emitLdStUImm(0x79400000, rt, rn, off, 2) }
func (a *Assembler) STRH(rt, rn Reg, off int32)  { a.emitLdStUImm(0x79000000, rt, rn, off, 2) }
func (a *Assembler) LDRB(rt, rn Reg, off int32)  { a.emitLdStUImm(0x39400000, rt, rn, off, 1) }
func (a *Assembler) STRB(rt, rn Reg, off int32)  { a.emitLdStUImm(0x39000000, rt, rn, off, 1) }
func (a *Assembler) LDRSB(rt, rn Reg, off int32) { a.emitLdStUImm(0x39800000, rt, rn, off, 1) }

func (a *Assembler) emitLdStUnscaled(base uint32, rt, rn Reg, off int32) {
	if off < -256 || off > 255 {
		a.setErr(fmt.Errorf("arm64: unscaled offset %d out of [-256, 255]", off))
		return
	}
	a.emit(base | (uint32(off)&0x1FF)<<12 | uint32(rn&31)<<5 | uint32(rt&31))
}

func (a *Assembler) LDUR(rt, rn Reg, off int32)  { a.emitLdStUnscaled(0xF8400000, rt, rn, off) }
func (a *Assembler) STUR(rt, rn Reg, off int32)  { a.emitLdStUnscaled(0xF8000000, rt, rn, off) }
func (a *Assembler) LDURW(rt, rn Reg, off int32) { a.emitLdStUnscaled(0xB8400000, rt, rn, off) }
func (a *Assembler) STURW(rt, rn Reg, off int32) { a.emitLdStUnscaled(0xB8000000, rt, rn, off) }
func (a *Assembler) LDURB(rt, rn Reg, off int32) { a.emitLdStUnscaled(0x38400000, rt, rn, off) }
func (a *Assembler) STURB(rt, rn Reg, off int32) { a.emitLdStUnscaled(0x38000000, rt, rn, off) }

// Indexed register-offset loads/stores: [rn, rm{, LSL #scale}].
// When scaled is true, rm is shifted left by log2(elementSize).
func (a *Assembler) emitLdStReg(base uint32, rt, rn, rm Reg, scaled bool) {
	var s uint32
	if scaled {
		s = 1 << 12
	}
	a.emit(base | uint32(rm&31)<<16 | s | uint32(rn&31)<<5 | uint32(rt&31))
}

func (a *Assembler) LDRReg(rt, rn, rm Reg, scaled bool)  { a.emitLdStReg(0xF8606800, rt, rn, rm, scaled) }
func (a *Assembler) STRReg(rt, rn, rm Reg, scaled bool)  { a.emitLdStReg(0xF8206800, rt, rn, rm, scaled) }
func (a *Assembler) LDRWReg(rt, rn, rm Reg, scaled bool) { a.emitLdStReg(0xB8606800, rt, rn, rm, scaled) }
func (a *Assembler) STRWReg(rt, rn, rm Reg, scaled bool) { a.emitLdStReg(0xB8206800, rt, rn, rm, scaled) }
func (a *Assembler) LDRBReg(rt, rn, rm Reg)              { a.emitLdStReg(0x38606800, rt, rn, rm, false) }
func (a *Assembler) STRBReg(rt, rn, rm Reg)              { a.emitLdStReg(0x38206800, rt, rn, rm, false) }

// Load/Store Pair (64-bit, signed imm7 scaled by 8: [-512, 504]).
func (a *Assembler) emitPair(base uint32, rt, rt2, rn Reg, off int32) {
	if off < -512 || off > 504 || off%8 != 0 {
		a.setErr(fmt.Errorf("arm64: pair offset %d out of [-512, 504] or unaligned", off))
		return
	}
	imm7 := uint32(off/8) & 0x7F
	a.emit(base | imm7<<15 | uint32(rt2&31)<<10 | uint32(rn&31)<<5 | uint32(rt&31))
}

func (a *Assembler) STP(rt, rt2, rn Reg, off int32)     { a.emitPair(0xA9000000, rt, rt2, rn, off) }
func (a *Assembler) LDP(rt, rt2, rn Reg, off int32)     { a.emitPair(0xA9400000, rt, rt2, rn, off) }
func (a *Assembler) STPPre(rt, rt2, rn Reg, off int32)  { a.emitPair(0xA9800000, rt, rt2, rn, off) }
func (a *Assembler) LDPPost(rt, rt2, rn Reg, off int32) { a.emitPair(0xA8C00000, rt, rt2, rn, off) }

// --- Scalar Double-Precision Floating Point (IEEE-754 float64) ---

func (a *Assembler) FMOVToF(fd FReg, rn Reg) {
	a.emit(0x9E670000 | uint32(rn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FMOVToG(rd Reg, fn FReg) {
	a.emit(0x9E660000 | uint32(fn&31)<<5 | uint32(rd&31))
}
func (a *Assembler) FMOV(fd, fn FReg) {
	a.emit(0x1E604000 | uint32(fn&31)<<5 | uint32(fd&31))
}

func (a *Assembler) LDRD(ft FReg, rn Reg, off int32) {
	a.emitLdStUImm(0xFD400000, Reg(ft), rn, off, 8)
}
func (a *Assembler) STRD(ft FReg, rn Reg, off int32) {
	a.emitLdStUImm(0xFD000000, Reg(ft), rn, off, 8)
}
func (a *Assembler) LDURD(ft FReg, rn Reg, off int32) {
	a.emitLdStUnscaled(0xFC400000, Reg(ft), rn, off)
}
func (a *Assembler) STURD(ft FReg, rn Reg, off int32) {
	a.emitLdStUnscaled(0xFC000000, Reg(ft), rn, off)
}
func (a *Assembler) LDRDReg(ft FReg, rn, rm Reg, scaled bool) {
	a.emitLdStReg(0xFC606800, Reg(ft), rn, rm, scaled)
}
func (a *Assembler) STRDReg(ft FReg, rn, rm Reg, scaled bool) {
	a.emitLdStReg(0xFC206800, Reg(ft), rn, rm, scaled)
}

func (a *Assembler) FADD(fd, fn, fm FReg) {
	a.emit(0x1E602800 | uint32(fm&31)<<16 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FSUB(fd, fn, fm FReg) {
	a.emit(0x1E603800 | uint32(fm&31)<<16 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FMUL(fd, fn, fm FReg) {
	a.emit(0x1E600800 | uint32(fm&31)<<16 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FDIV(fd, fn, fm FReg) {
	a.emit(0x1E601800 | uint32(fm&31)<<16 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FMADD(fd, fn, fm, fa FReg) {
	a.emit(0x1F400000 | uint32(fm&31)<<16 | uint32(fa&31)<<10 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FMSUB(fd, fn, fm, fa FReg) {
	a.emit(0x1F408000 | uint32(fm&31)<<16 | uint32(fa&31)<<10 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FABS(fd, fn FReg) {
	a.emit(0x1E60C000 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FNEG(fd, fn FReg) {
	a.emit(0x1E614000 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FSQRT(fd, fn FReg) {
	a.emit(0x1E61C000 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FRINTN(fd, fn FReg) {
	a.emit(0x1E644000 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FRINTP(fd, fn FReg) {
	a.emit(0x1E64C000 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FRINTM(fd, fn FReg) {
	a.emit(0x1E654000 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FRINTZ(fd, fn FReg) {
	a.emit(0x1E65C000 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FCMP(fn, fm FReg) {
	a.emit(0x1E602000 | uint32(fm&31)<<16 | uint32(fn&31)<<5)
}
func (a *Assembler) FCMPZero(fn FReg) {
	a.emit(0x1E602008 | uint32(fn&31)<<5)
}
func (a *Assembler) FCSEL(fd, fn, fm FReg, cond Cond) {
	a.emit(0x1E600C00 | uint32(fm&31)<<16 | uint32(cond&0xF)<<12 | uint32(fn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) SCVTF(fd FReg, rn Reg) {
	a.emit(0x9E620000 | uint32(rn&31)<<5 | uint32(fd&31))
}
func (a *Assembler) FCVTZS(rd Reg, fn FReg) {
	a.emit(0x9E780000 | uint32(fn&31)<<5 | uint32(rd&31))
}

// --- Branches & Control Flow ---

func (a *Assembler) B(l Label) {
	a.patch = append(a.patch, patch{insnIdx: len(a.insns), label: l, kind: patchB26})
	a.emit(0x14000000)
}
func (a *Assembler) BL(l Label) {
	a.patch = append(a.patch, patch{insnIdx: len(a.insns), label: l, kind: patchB26})
	a.emit(0x94000000)
}
func (a *Assembler) BCond(cond Cond, l Label) {
	a.patch = append(a.patch, patch{insnIdx: len(a.insns), label: l, kind: patchB19})
	a.emit(0x54000000 | uint32(cond&0xF))
}
func (a *Assembler) CBZ(rn Reg, l Label) {
	a.patch = append(a.patch, patch{insnIdx: len(a.insns), label: l, kind: patchB19})
	a.emit(0xB4000000 | uint32(rn&31))
}
func (a *Assembler) CBNZ(rn Reg, l Label) {
	a.patch = append(a.patch, patch{insnIdx: len(a.insns), label: l, kind: patchB19})
	a.emit(0xB5000000 | uint32(rn&31))
}
func (a *Assembler) CBZW(rn Reg, l Label) {
	a.patch = append(a.patch, patch{insnIdx: len(a.insns), label: l, kind: patchB19})
	a.emit(0x34000000 | uint32(rn&31))
}
func (a *Assembler) CBNZW(rn Reg, l Label) {
	a.patch = append(a.patch, patch{insnIdx: len(a.insns), label: l, kind: patchB19})
	a.emit(0x35000000 | uint32(rn&31))
}

func (a *Assembler) emitTestBitBranch(base uint32, rn Reg, bit uint8, l Label) {
	if bit > 63 {
		a.setErr(fmt.Errorf("arm64: TBZ/TBNZ bit=%d out of range", bit))
		return
	}
	a.patch = append(a.patch, patch{insnIdx: len(a.insns), label: l, kind: patchB14})
	b5 := uint32(bit>>5) & 1
	b40 := uint32(bit) & 0x1F
	a.emit(base | b5<<31 | b40<<19 | uint32(rn&31))
}

func (a *Assembler) TBZ(rn Reg, bit uint8, l Label)  { a.emitTestBitBranch(0x36000000, rn, bit, l) }
func (a *Assembler) TBNZ(rn Reg, bit uint8, l Label) { a.emitTestBitBranch(0x37000000, rn, bit, l) }

func (a *Assembler) BR(rn Reg)  { a.emit(0xD61F0000 | uint32(rn&31)<<5) }
func (a *Assembler) BLR(rn Reg) { a.emit(0xD63F0000 | uint32(rn&31)<<5) }
func (a *Assembler) RET()       { a.emit(0xD65F03C0) }
func (a *Assembler) NOP()       { a.emit(0xD503201F) }

// --- Prologue & Epilogue Helpers ---

// FrameLayout describes a 16-byte aligned stack frame emitted by EmitPrologue/EmitEpilogue.
// Local spill slots occupy [SP, SP+LocalBytes); saved registers sit above them, topped by
// (X29, X30) at the highest 16-byte pair.
type FrameLayout struct {
	LocalBytes int32
	TotalBytes int32
	SavedRegs  []Reg
}

// ComputeFrameLayout rounds localBytes and savedRegs into a 16-byte aligned frame layout.
func ComputeFrameLayout(localBytes int32, savedRegs []Reg) FrameLayout {
	if localBytes < 0 {
		localBytes = 0
	}
	alignedLocal := (localBytes + 15) &^ 15
	savedPairs := int32((len(savedRegs) + 1) / 2)
	// Always reserve 16 bytes for (X29, X30) + 16 bytes per pair of saved registers.
	total := alignedLocal + 16 + savedPairs*16
	cp := make([]Reg, len(savedRegs))
	copy(cp, savedRegs)
	return FrameLayout{
		LocalBytes: alignedLocal,
		TotalBytes: total,
		SavedRegs:  cp,
	}
}

// EmitPrologue allocates the frame, saves (X29, X30) and savedRegs, and sets FP=SP+fpOff.
func (a *Assembler) EmitPrologue(fl FrameLayout) {
	if fl.TotalBytes == 0 {
		return
	}
	a.SUBImm(SP, SP, uint32(fl.TotalBytes))
	fpOff := fl.TotalBytes - 16
	a.STP(X29, X30, SP, fpOff)
	a.ADDImm(X29, SP, uint32(fpOff))
	off := fl.LocalBytes
	for i := 0; i < len(fl.SavedRegs); i += 2 {
		r1 := fl.SavedRegs[i]
		r2 := XZR
		if i+1 < len(fl.SavedRegs) {
			r2 = fl.SavedRegs[i+1]
		}
		if r2 == XZR {
			a.STR(r1, SP, off)
		} else {
			a.STP(r1, r2, SP, off)
		}
		off += 16
	}
}

// EmitEpilogue restores savedRegs and (X29, X30), deallocates the frame, and emits RET.
func (a *Assembler) EmitEpilogue(fl FrameLayout) {
	if fl.TotalBytes > 0 {
		off := fl.LocalBytes
		for i := 0; i < len(fl.SavedRegs); i += 2 {
			r1 := fl.SavedRegs[i]
			r2 := XZR
			if i+1 < len(fl.SavedRegs) {
				r2 = fl.SavedRegs[i+1]
			}
			if r2 == XZR {
				a.LDR(r1, SP, off)
			} else {
				a.LDP(r1, r2, SP, off)
			}
			off += 16
		}
		fpOff := fl.TotalBytes - 16
		a.LDP(X29, X30, SP, fpOff)
		a.ADDImm(SP, SP, uint32(fl.TotalBytes))
	}
	a.RET()
}
