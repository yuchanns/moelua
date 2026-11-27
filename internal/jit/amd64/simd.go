package amd64

// --- 128-bit SSE2 Packed 2x64-bit Vector Instructions ---
//
// SSE2 is architecturally mandatory on all x86-64 CPUs (Linux, macOS, Windows).
// XMM0..XMM14 are caller-saved under Go ABIInternal; XMM15 must remain zero.

// 128-bit unaligned vector loads and stores.
func (a *Assembler) MOVUPDLoad(dst XReg, m Mem)  { a.emitSSE2RegMem(0x66, 0x10, dst, m) }
func (a *Assembler) MOVUPDStore(m Mem, src XReg) { a.emitSSE2RegMem(0x66, 0x11, src, m) }
func (a *Assembler) MOVDQULoad(dst XReg, m Mem)  { a.emitSSE2RegMem(0xF3, 0x6F, dst, m) }
func (a *Assembler) MOVDQUStore(m Mem, src XReg) { a.emitSSE2RegMem(0xF3, 0x7F, src, m) }

// 128-bit register-to-register moves.
func (a *Assembler) MOVAPD(dst, src XReg) { a.emitSSE2RegReg(0x66, 0x28, dst, src) }
func (a *Assembler) MOVDQA(dst, src XReg) { a.emitSSE2RegReg(0x66, 0x6F, dst, src) }

// 2x float64 (PD) packed arithmetic (strict IEEE-754 per lane).
func (a *Assembler) ADDPD(dst, src XReg)      { a.emitSSE2RegReg(0x66, 0x58, dst, src) }
func (a *Assembler) SUBPD(dst, src XReg)      { a.emitSSE2RegReg(0x66, 0x5C, dst, src) }
func (a *Assembler) MULPD(dst, src XReg)      { a.emitSSE2RegReg(0x66, 0x59, dst, src) }
func (a *Assembler) DIVPD(dst, src XReg)      { a.emitSSE2RegReg(0x66, 0x5E, dst, src) }
func (a *Assembler) SQRTPD(dst, src XReg)     { a.emitSSE2RegReg(0x66, 0x51, dst, src) }
func (a *Assembler) ADDPDMem(dst XReg, m Mem) { a.emitSSE2RegMem(0x66, 0x58, dst, m) }
func (a *Assembler) SUBPDMem(dst XReg, m Mem) { a.emitSSE2RegMem(0x66, 0x5C, dst, m) }
func (a *Assembler) MULPDMem(dst XReg, m Mem) { a.emitSSE2RegMem(0x66, 0x59, dst, m) }
func (a *Assembler) DIVPDMem(dst XReg, m Mem) { a.emitSSE2RegMem(0x66, 0x5E, dst, m) }

// 2x int64 (PQ) and 128-bit bitwise packed integer arithmetic.
func (a *Assembler) PADDQ(dst, src XReg)      { a.emitSSE2RegReg(0x66, 0xD4, dst, src) }
func (a *Assembler) PSUBQ(dst, src XReg)      { a.emitSSE2RegReg(0x66, 0xFB, dst, src) }
func (a *Assembler) PAND(dst, src XReg)       { a.emitSSE2RegReg(0x66, 0xDB, dst, src) }
func (a *Assembler) POR(dst, src XReg)        { a.emitSSE2RegReg(0x66, 0xEB, dst, src) }
func (a *Assembler) PXOR(dst, src XReg)       { a.emitSSE2RegReg(0x66, 0xEF, dst, src) }
func (a *Assembler) PADDQMem(dst XReg, m Mem) { a.emitSSE2RegMem(0x66, 0xD4, dst, m) }

// Lane broadcast and extraction (64-bit lanes).
// UNPCKLPD(x, x) duplicates float64 lane 0 into both lanes; UNPCKHPD(dst, src) moves lane 1 of src into lane 0 of dst.
func (a *Assembler) UNPCKLPD(dst, src XReg)   { a.emitSSE2RegReg(0x66, 0x14, dst, src) }
func (a *Assembler) UNPCKHPD(dst, src XReg)   { a.emitSSE2RegReg(0x66, 0x15, dst, src) }
func (a *Assembler) PUNPCKLQDQ(dst, src XReg) { a.emitSSE2RegReg(0x66, 0x6C, dst, src) }
func (a *Assembler) PUNPCKHQDQ(dst, src XReg) { a.emitSSE2RegReg(0x66, 0x6D, dst, src) }
