// Package moelua provides a zero-CGo, zero-dependency Lua 5.5.1 compiler,
// bytecode loader/verifier, Plan 9 direct-threaded virtual machine, optional
// opt-in ARM64/AMD64 native JIT (WithJIT), and per-State noscan Arena with a
// PUC-Rio Lua 5.5.1 incremental and generational garbage collector.
//
// Build with -tags moeluanojit to omit the native JIT backend, or
// -tags "purego moeluanojit" for a 100% pure-Go build with zero assembly.
//
// # Concurrency and Templates
//
// Each *State is strictly single-goroutine (matching PUC-Rio Lua's lua_State)
// and lock-free. Compiled *Chunk values and preconfigured *Template snapshots
// (created via NewTemplate) are immutable and safe to share concurrently across
// goroutines to spawn isolated States in microseconds.
//
// # Standard Library Control and Sandboxing
//
// By default New opens the complete Lua 5.5.1 standard library (LibAll).
// Embedders can open a restricted subset via WithLibs (for example
// WithLibs(LibSafe | LibPackageSafe) combined with WithFS and WithPreload for
// an in-memory sandboxed require), register lazy standard libraries via
// WithPreloadLibs / PreloadLibs, or eagerly register custom host modules via
// RequireLib.
package moelua
