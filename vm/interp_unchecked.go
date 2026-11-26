//go:build !moeluaregcheck

package vm

import (
	"unsafe"

	"github.com/yuchanns/moelua/bytecode"
)

// codeWords allows bounds-check-free instruction fetch inside run(),
// proven safe by bytecode.Verify before execution.
type codeWords struct {
	p unsafe.Pointer
}

func codeOf(code []bytecode.Instruction) codeWords {
	return codeWords{p: unsafe.Pointer(unsafe.SliceData(code))}
}

func (c codeWords) at(pc int) bytecode.Instruction {
	return *(*bytecode.Instruction)(unsafe.Add(c.p, pc*4))
}

// nonNilWindow proves to Go's SSA compiler that w != nil at zero instruction cost
// (eliminating repeated nil-checks on regs[...] across every switch case).
//
//go:nocheckptr
func nonNilWindow(w *[256]Value) *[256]Value {
	return (*[256]Value)(unsafe.Add(unsafe.Pointer(w), 0))
}

// icAt fetches the per-boundProto inline cache entry at slot without bounds checks.
func icAt(ic []ICEntry, slot int32) *ICEntry {
	return (*ICEntry)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(ic)), int(slot)*int(unsafe.Sizeof(ICEntry{}))))
}
