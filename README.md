# MoeLua 5.5

A pure-Go (`CGO_ENABLED=0`, zero assembly, zero JIT) **Lua 5.5.1** compiler, bytecode dumper/verifier, virtual machine, standard library, and standalone CLI — backed by per-`State` **noscan `[]byte` Arenas (`ptrdata == 0`)** and a true **PUC-Rio Lua 5.5.1 incremental + generational GC** so high-density multi-VM Go applications avoid Go GC pointer-scanning overhead.

## Highlights

- **Exact Lua 5.5.1 Compiler & Bytecode Dumper (`compiler/`, `bytecode/`)**:
  - Full Lua 5.5.1 grammar and codegen (`global`, `global <const> *`, `local <const>`, `local <close>`, named/hidden varargs, `goto`/labels, constant folding, line/local/upvalue debug info).
  - Compiled + dumped chunks match PUC-Rio `luac 5.5.1` byte-for-byte across all 34 official Lua 5.5.1 `testes/*.lua` files (normal and stripped), all 13 benchmark scripts, and 70 edge-case constant-folding/codegen cases.
- **Noscan Arena VM & Per-State Lua 5.5.1 GC (`internal/arena/`, `vm/`)**:
  - **Pointer-free 16-byte tagged `Value` (`ptrdata == 0`)** and **100% noscan Arena heap**: `Table` (`80B`), `Shape` (`128B`), `ICEntry` (`48B`), `polyEntry` (`40B`), `LuaString`, `Thread`, `callFrame`, `Closure`, `Upvalue`, `ArenaNative`, `Userdata`, and `ArenaProto` all live in per-`State` noscan `[]byte` slabs linked by 32-bit `ObjRef` handles. Go's GC performs zero pointer scanning inside Lua VM heaps.
  - **True PUC-Rio Lua 5.5.1 Incremental & Generational GC (`internal/arena/gc.go`, `vm/gc.go`)**: tri-color incremental mark-sweep and generational minor/major modes matching `lgc.c` (exact 6-step `atomic()`, ephemeron convergence, LIFO `__gc` finalizers, resurrection, weak-table pruning, and whole-page slab reclamation).
  - **Generational `GoRefs` Host Bridge**: userdata payloads, dynamic Go callbacks, open files, and escaping Go errors are held in per-`State` generational handle tables (`GoHandle`) and released automatically when Lua's Arena GC sweeps them.
  - **Heap-allocated stackful coroutines** (zero Go goroutines): yieldable across metamethods (`__index`, `__newindex`, `__call`, `__concat`, comparisons, arithmetic), `pcall`/`xpcall`, `dofile`, `pairs`, and `<close>` finalizers, with automatic stack and auxiliary-buffer shrinking.
  - **Compacted Zero-Relocation Stdlib Snapshot & Progressive Slab Sizing**: process-wide read-only frozen stdlib string Arena plus a compacted `Arena.CompactSnapshot()` template (`42.94 KiB` Arena backing / `52.84 KiB` Go `HeapAlloc` per idle VM). `NewState()` clones a complete, isolated Lua 5.5.1 state in **4.81 µs** with zero pointer relocation; runtime pages grow progressively (`2 KiB → 8 KiB → 32 KiB → 64 KiB`).
  - **Production Embedding Guards**:
    - `WithContext(ctx)` / `SetContext(ctx)` cooperative cancellation & deadlines (cannot be swallowed by `pcall`/`xpcall`/`coroutine.resume`, zero cost on the fast loop when unset).
    - `WithInstructionLimit(n)` / `SetInstructionLimit(n)` deterministic VM step budget (cannot be swallowed by `pcall`/`xpcall`/`coroutine.resume`, zero cost when unset).
    - `WithMemoryLimit(bytes)` per-State VM memory cap with Arena size-class accounting and emergency GC recount raising recoverable Lua `"not enough memory"` errors.
    - `WithFS(fsys)` / `AddFS(fsys)` / `AddSearcher` for loading Lua source and `.luac` modules directly from Go `embed.FS` / `io/fs.FS` with path-traversal protection.
    - `Call1` zero-allocation Go→Lua calls (**39.2 ns / 0 B / 0 allocs**) and `RegisterFunc1` / `NewFunc1` (`NativeFunc.FastFn`) zero-allocation single-return Go callbacks (**29.53 ms / 0 per-call allocs** for 1M calls).
    - `NewMetatable` / `GetMetatable` / `CheckUserdata`, `Table.Next` / `ForEach`, `Preload`, and `NewThread` / `Resume` / `Thread.Status` for full Go host bindings and coroutine orchestration.
    - `State.Pin(v)` / `Handle.Release()` GC-anchored host references and nil-receiver / post-`Close()` safety guards across `State`, `Table`, `Chunk`, `Thread`, and `Handle`.
  - **Full `debug` Library & Zero-Cost Hooks**: single-source `go generate` (`internal/gen/interpgen`) produces `fast`, `-tags moeluaregcheck`, and `runHooked` dispatch variants from `vm/interp_run.go`.
- **Complete Pure-Go Standard Library**: `base`, `math`, `table`, `string` (patterns, `format`, `pack`/`unpack`), `utf8` (including Lua 5.5 two-return `utf8.offset`), `coroutine`, `debug`, `io` (buffered `setvbuf`, `runtime.AddCleanup` + `State.Close()`/`os.exit()` flush), `os`, and `package`/`require`.
- **Standalone CLI (`cmd/moelua`)**: supports `-e`, `-l`, `-i`, `-v`, `-W`, `-E`, `--`, `-`, `LUA_INIT_5_5`/`LUA_INIT`, `LUA_PATH_5_5`/`LUA_PATH`, interactive REPL, and `SIGINT` handling (passes official PUC-Rio `testes/main.lua` excluding C `.so` dynamic loading).

## Quick Start (Go Embedding)

```go
package main

import (
	"context"
	"embed"
	"fmt"
	"time"

	"github.com/yuchanns/moelua"
)

//go:embed scripts/*.lua
var scriptsFS embed.FS

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	s := moelua.New(
		moelua.WithContext(ctx),
		moelua.WithMemoryLimit(32<<20),
		moelua.WithInstructionLimit(1_000_000),
		moelua.WithFS(scriptsFS),
	)
	defer s.Close()

	// Zero-allocation single-return Go host callback:
	s.RegisterFunc1("add", func(s *moelua.State, args []moelua.Value) (moelua.Value, error) {
		a, _ := args[0].ToInt()
		b, _ := args[1].ToInt()
		return moelua.Int(a + b), nil
	})

	res, err := s.DoString(`return add(20, 22)`)
	if err != nil {
		panic(err)
	}
	fmt.Println(res[0].Int()) // 42
}
```

### Sharing Compiled Chunks & High-Density Multi-VM Concurrency

Each `*moelua.State` is strictly **single-goroutine** and lock-free (matching PUC-Rio `lua_State`), owning an independent noscan Arena and Lua 5.5.1 Collector. Compiled `*moelua.Chunk` values and the frozen stdlib string arena are immutable and shared concurrently across independent `*moelua.State` instances running on separate goroutines.

```go
chunk, err := moelua.Compile("=worker", `return ... * 2`)
// Share `chunk` across independent States on separate goroutines:
fn, _ := state.Load(chunk)
out, _ := state.Call1(fn, moelua.Int(21))
```

## Standalone CLI

```bash
go build -o build/moelua ./cmd/moelua
./build/moelua -e 'print(_VERSION, 6 * 7)'
```

## Testing & Verification

Run the self-contained pure-Go test suite across all build configurations:

```bash
make test
```

The official PUC-Rio Lua 5.5.1 source and `testes/` suite are pinned as a git submodule at `third_party/lua` (`v5.5.1`). Initialize the submodule to run the full official `testes/` conformance suite, compiler byte-identity suite, CLI `main.lua` test, and C Lua 5.5.1 differential/benchmark suites automatically (or override with `LUA55_SRC` / `LUA55_TESTES`):

```bash
git submodule update --init
make tools
make test
```

## Benchmarks

On Apple M1 Max (`darwin/arm64`, Go 1.27.4, 10-round interleaved ABAB, strict single-core `GOMAXPROCS=1` CPU time vs C Lua 5.5.1 `clang -O2` and `-O3 -mcpu=apple-m1`, with full per-`State` Lua 5.5.1 Arena GC always enabled). Reproduce after `git submodule update --init` with `make bench` (No-PGO) or `make bench-pgo` (disjoint PGO trained solely on official `testes/`):

- **12-benchmark Core VM Geomean**: **0.88× CPU / 0.89× Wall** vs C `-O2` (**0.74×** vs C `-O3 -mcpu=apple-m1`; Disjoint-PGO **0.87× / 0.89×** vs `-O2`, **0.74×** vs `-O3`; equal-footing subprocess **0.85× / 0.72×**)
- **Core & OOP highlights (No-PGO vs C `-O2`)**: `matmul` **1.03×**, `deltablue` **0.98×** (**0.94×** vs `-O3`), `forloop_int` **0.97×**, `ecs_stdlib` **0.97×** (**0.93×** vs `-O3`), `mandelbrot` **0.89×**, `binarytrees` **0.87×** (**0.76×** vs `-O3`, Peak RSS **16.3–18.1 MiB**), `fannkuch` **0.84×**, `richards` **0.83×**, `fib(34)` **0.82×**, `sieve` **0.80×** (Peak RSS **6.3 MiB**), `nbody` **0.79×**, `spectralnorm` **0.78×**
- **Stdlib typed-array sort (`sort_int`, reported separately)**: **3.61×** vs C `-O2` (**3.37×** vs `-O3`)
- **High-density multi-VM Go GC scalability (`./bench`, `./internal/arena`)**:
  - **1,000 idle States**: **0.75 ms** `runtime.GC()`, **42.94 KiB** Arena backing / **52.84 KiB** Go `HeapAlloc` per VM (51.6 MiB total)
  - **256 active States (418,816 live Lua objects)**: **1.07 ms** `runtime.GC()`
  - **1,000 active States (1,636,000 live Lua objects)**: **3.36 ms** `runtime.GC()`
  - **256 Arenas × 2,000 linked nodes (512,000 objects)**: **0.289 ms** vs **2.273 ms** Go-heap pointer graph (**7.9× faster Go GC**)
- **Embedding (`GOMAXPROCS=1`)**: `NewState` **4.81 µs** (`54,104 B`), Go→Lua `Call1` **39.2 ns (0 B / 0 allocs)** (`Call` **92.1 ns / 1 alloc**), Lua→Go 1M calls **29.53 ms (~29.5 ns/call, 0 allocs/call)** via `RegisterFunc1`/`FastFn` (**48.01 ms / 1 alloc/call** via multi-return `Fn`)

See [`docs/benchmarks.md`](docs/benchmarks.md) for full tables, IQR spreads, Peak RSS, subprocess mode, multi-VM scalability numbers, and conformance details.

## Trust Model & Resource Limits

- **Binary chunks are trusted input**: `bytecode.Undump` and `bytecode.Verify` provide panic-freedom defense in depth (nesting depth caps, CFG successor validation, register/constant/upvalue bounds), matching PUC-Rio Lua's trust model rather than acting as an untrusted-bytecode sandbox.
- **Per-State memory limit (`WithMemoryLimit`)**: accounts Lua tables, shapes, inline caches, strings, closures, threads, and stack/frame growth (including Arena size-class rounding) with emergency GC recount (skipping `__gc` finalizers mid-allocation) and raises recoverable `"not enough memory"` errors; it is a VM-level resource guard rather than an OS RSS cgroup.
- **Cooperative cancellation & step budgets (`WithContext`, `WithInstructionLimit`)**: checked inside `runHooked` (zero overhead in the default fast loop when unset, including non-cancelable `context.Background()`), at call boundaries, and periodically inside pattern matching (`string.find`/`match`/`gmatch`/`gsub`). Treated as uncatchable by Lua `pcall`/`xpcall`, `coroutine.resume`/`wrap`, `load`, `<close>`, and `__gc`; hard cancellation unwinds frames and discards pending Lua `<close>` metamethods above the aborted boundary without running arbitrary Lua code. Blocking Go host callbacks or OS I/O remain the caller's responsibility.
- **Zero `recover()` in `vm/`**: Lua errors propagate strictly via Go error return values; runtime panics are never masked as Lua errors.
