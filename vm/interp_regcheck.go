//go:build moeluaregcheck

package vm

import (
	"fmt"

	"github.com/yuchanns/moelua/bytecode"
)

// Checked codeWords verifies every instruction fetch against slice bounds.
type codeWords struct {
	code []bytecode.Instruction
}

func codeOf(code []bytecode.Instruction) codeWords {
	return codeWords{code: code}
}

func (c codeWords) at(pc int) bytecode.Instruction {
	if pc < 0 || pc >= len(c.code) {
		panic(fmt.Sprintf("moelua: regcheck: pc %d out of range [0, %d)", pc, len(c.code)))
	}
	return c.code[pc]
}

// nonNilWindow asserts w != nil in checked builds.
func nonNilWindow(w *[256]Value) *[256]Value {
	if w == nil {
		panic("moelua: regcheck: nil register window")
	}
	return w
}

// icAt bounds-checks every IC slot access in checked builds.
func icAt(ic []ICEntry, slot int32) *ICEntry {
	idx := int(slot)
	if idx < 0 || idx >= len(ic) {
		panic(fmt.Sprintf("moelua: regcheck: IC index %d out of range [0, %d)", idx, len(ic)))
	}
	return &ic[idx]
}

// regcheckVerifyWindow verifies that th.stack did not reallocate since regs was taken,
// and that base + maxStack fits safely in th.stack.
func regcheckVerifyWindow(th *Thread, base int, winGen uint32, maxStack uint8, pc int, codeLen int) {
	if th.stackGen != winGen {
		panic(fmt.Sprintf("moelua: regcheck: stale register window (have gen %d, stack gen %d) at pc %d", winGen, th.stackGen, pc))
	}
	if base < 0 || base+int(maxStack) > len(th.stack) || base+256 > len(th.stack) {
		panic(fmt.Sprintf("moelua: regcheck: frame window base=%d maxStack=%d exceeds stack len=%d", base, maxStack, len(th.stack)))
	}
	if pc < 0 || pc >= codeLen {
		panic(fmt.Sprintf("moelua: regcheck: pc %d out of range [0, %d)", pc, codeLen))
	}
}

// regcheckVerifyCleanFrame asserts the callFrame.dirty invariant: whenever fast OP_CALL reuses
// a callFrame slot with !nf.dirty, every cold field MUST already be zero.
func regcheckVerifyCleanFrame(nf *callFrame) {
	if nf.kind != frameLua || nf.finish != finishNone || nf.varargTab != nil || nf.nextraargs != 0 ||
		nf.isTailCall || nf.isHook || nf.isFinalizer || nf.atCurrentPC || nf.extraArgs != 0 || nf.ntransfer != 0 {
		panic(fmt.Sprintf("moelua: regcheck: fast OP_CALL reused dirty callFrame without dirty flag set (kind=%d finish=%d tail=%v hook=%v fin=%v ntransfer=%d)",
			nf.kind, nf.finish, nf.isTailCall, nf.isHook, nf.isFinalizer, nf.ntransfer))
	}
}
