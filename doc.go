// Package moelua provides a pure-Go (CGO_ENABLED=0, zero assembly, zero JIT)
// Lua 5.5.1 compiler, bytecode loader/verifier, and virtual machine.
//
// Each State is strictly single-goroutine (matching PUC-Rio Lua's lua_State)
// and lock-free. Compiled *Chunk values and the process-wide frozen standard
// library template are immutable and safe to reuse across independent States
// running on different goroutines.
package moelua
