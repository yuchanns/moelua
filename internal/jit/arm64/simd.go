package arm64

import "fmt"

// --- 128-bit Advanced SIMD (NEON) Loads, Stores, and 2x64-bit Vector Operations ---
//
// All methods operate on the 128-bit V0..V31 / Q0..Q31 vector register bank (aliased with
// D0..D31 via FReg). Callers must follow AAPCS64 callee-saved rules (D8..D15 low 64 bits
// are callee-saved; V0..V7 and V16..V31 are caller-saved scratch registers).

// LDRQ loads 128 bits from [rn + off] into Q(vt). off must be a multiple of 16 in [0, 65520].
func (a *Assembler) LDRQ(vt FReg, rn Reg, off int32) {
	a.emitLdStUImm(0x3DC00000, Reg(vt), rn, off, 16)
}

// STRQ stores 128 bits from Q(vt) to [rn + off]. off must be a multiple of 16 in [0, 65520].
func (a *Assembler) STRQ(vt FReg, rn Reg, off int32) {
	a.emitLdStUImm(0x3D800000, Reg(vt), rn, off, 16)
}

// LDRQReg loads 128 bits from [rn + rm] (unscaled byte offset rm) into Q(vt).
func (a *Assembler) LDRQReg(vt FReg, rn, rm Reg) {
	a.emitLdStReg(0x3CE06800, Reg(vt), rn, rm, false)
}

// STRQReg stores 128 bits from Q(vt) to [rn + rm] (unscaled byte offset rm).
func (a *Assembler) STRQReg(vt FReg, rn, rm Reg) {
	a.emitLdStReg(0x3CA06800, Reg(vt), rn, rm, false)
}

// LDRQPost loads 128 bits from [rn] into Q(vt) and post-increments rn by imm9 (in [-256, 255]).
func (a *Assembler) LDRQPost(vt FReg, rn Reg, imm9 int32) {
	if imm9 < -256 || imm9 > 255 {
		a.setErr(fmt.Errorf("arm64: LDRQPost imm9=%d out of [-256, 255]", imm9))
		return
	}
	a.emit(0x3CC00400 | (uint32(imm9)&0x1FF)<<12 | uint32(rn&31)<<5 | uint32(vt&31))
}

// STRQPost stores 128 bits from Q(vt) to [rn] and post-increments rn by imm9 (in [-256, 255]).
func (a *Assembler) STRQPost(vt FReg, rn Reg, imm9 int32) {
	if imm9 < -256 || imm9 > 255 {
		a.setErr(fmt.Errorf("arm64: STRQPost imm9=%d out of [-256, 255]", imm9))
		return
	}
	a.emit(0x3C800400 | (uint32(imm9)&0x1FF)<<12 | uint32(rn&31)<<5 | uint32(vt&31))
}

// 2x float64 (.2D) vector arithmetic (strict IEEE-754 per lane, no fused rounding changes).
func (a *Assembler) FADD2D(vd, vn, vm FReg) {
	a.emit(0x4E60D400 | uint32(vm&31)<<16 | uint32(vn&31)<<5 | uint32(vd&31))
}
func (a *Assembler) FSUB2D(vd, vn, vm FReg) {
	a.emit(0x4EE0D400 | uint32(vm&31)<<16 | uint32(vn&31)<<5 | uint32(vd&31))
}
func (a *Assembler) FMUL2D(vd, vn, vm FReg) {
	a.emit(0x6E60DC00 | uint32(vm&31)<<16 | uint32(vn&31)<<5 | uint32(vd&31))
}
func (a *Assembler) FDIV2D(vd, vn, vm FReg) {
	a.emit(0x6E60FC00 | uint32(vm&31)<<16 | uint32(vn&31)<<5 | uint32(vd&31))
}
func (a *Assembler) FSQRT2D(vd, vn FReg) {
	a.emit(0x6EE1F800 | uint32(vn&31)<<5 | uint32(vd&31))
}
func (a *Assembler) FABS2D(vd, vn FReg) {
	a.emit(0x4EE0F800 | uint32(vn&31)<<5 | uint32(vd&31))
}
func (a *Assembler) FNEG2D(vd, vn FReg) {
	a.emit(0x6EE0F800 | uint32(vn&31)<<5 | uint32(vd&31))
}

// 2x int64 (.2D) and 128-bit bitwise (.16B) vector arithmetic.
func (a *Assembler) ADD2D(vd, vn, vm FReg) {
	a.emit(0x4EE08400 | uint32(vm&31)<<16 | uint32(vn&31)<<5 | uint32(vd&31))
}
func (a *Assembler) SUB2D(vd, vn, vm FReg) {
	a.emit(0x6EE08400 | uint32(vm&31)<<16 | uint32(vn&31)<<5 | uint32(vd&31))
}
func (a *Assembler) AND16B(vd, vn, vm FReg) {
	a.emit(0x4E201C00 | uint32(vm&31)<<16 | uint32(vn&31)<<5 | uint32(vd&31))
}
func (a *Assembler) ORR16B(vd, vn, vm FReg) {
	a.emit(0x4EA01C00 | uint32(vm&31)<<16 | uint32(vn&31)<<5 | uint32(vd&31))
}
func (a *Assembler) EOR16B(vd, vn, vm FReg) {
	a.emit(0x6E201C00 | uint32(vm&31)<<16 | uint32(vn&31)<<5 | uint32(vd&31))
}

// MOV16B copies all 128 bits of vn into vd (ORR Vd.16B, Vn.16B, Vn.16B).
func (a *Assembler) MOV16B(vd, vn FReg) {
	a.ORR16B(vd, vn, vn)
}

// DUP2DElem0 broadcasts lane 0 (scalar D) of vn into both 64-bit lanes of vd (DUP Vd.2D, Vn.D[0]).
func (a *Assembler) DUP2DElem0(vd, vn FReg) {
	a.emit(0x4E080400 | uint32(vn&31)<<5 | uint32(vd&31))
}

// DUP2DGPR broadcasts 64-bit general-purpose register rn into both 64-bit lanes of vd (DUP Vd.2D, Rn).
func (a *Assembler) DUP2DGPR(vd FReg, rn Reg) {
	a.emit(0x4E080C00 | uint32(rn&31)<<5 | uint32(vd&31))
}

// DUPD1 extracts lane 1 (upper 64 bits) of vn into scalar register dd (DUP Dd, Vn.D[1]).
func (a *Assembler) DUPD1(dd, vn FReg) {
	a.emit(0x5E180400 | uint32(vn&31)<<5 | uint32(dd&31))
}

// ADDPD computes the horizontal sum of both int64 lanes of vn into scalar dd (ADDP Dd, Vn.2D).
func (a *Assembler) ADDPD(dd, vn FReg) {
	a.emit(0x5EF1B800 | uint32(vn&31)<<5 | uint32(dd&31))
}

// FADDPD computes the horizontal IEEE-754 sum of both float64 lanes of vn into scalar dd (FADDP Dd, Vn.2D).
func (a *Assembler) FADDPD(dd, vn FReg) {
	a.emit(0x7E70D800 | uint32(vn&31)<<5 | uint32(dd&31))
}

// MOVI2DZero zeroes all 128 bits of vd (MOVI Vd.2D, #0).
func (a *Assembler) MOVI2DZero(vd FReg) {
	a.emit(0x6F00E400 | uint32(vd&31))
}
