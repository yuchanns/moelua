# MoeLua 5.5

A pure-Go (`CGO_ENABLED=0`, zero assembly, zero JIT, Go-GC-backed) **Lua 5.5.1** compiler, bytecode dumper/verifier, virtual machine, standard library, and standalone CLI.

## Highlights

- **Exact Lua 5.5.1 Compiler & Bytecode Dumper (`compiler/`, `bytecode/`)**:
  - Full Lua 5.5.1 grammar and codegen (`global`, `global <const> *`, `local <const>`, `local <close>`, named/hidden varargs, `goto`/labels, constant folding, line/local/upvalue debug info).
  - Compiled + dumped chunks match PUC-Rio `luac 5.5.1` byte-for-byte across all 34 official Lua 5.5.1 `testes/*.lua` files (normal and stripped), all 13 benchmark scripts, and 70 edge-case constant-folding/codegen cases.
- **Full Lua 5.5.1 VM Semantics (`vm/`)**:
  - **16-byte Go-GC-safe tagged `Value`** with zero heap boxing for integers, floats, booleans, and nil.
  - **Heap-allocated stackful coroutines** (zero Go goroutines): yieldable across metamethods (`__index`, `__newindex`, `__call`, `__concat`, comparisons, arithmetic), `pcall`/`xpcall`, `dofile`, `pairs`, and `<close>` finalizers, with automatic stack shrinking (including coroutines holding open upvalues).
  - **Two-Tier GC Semantics Engine**: Tier 0 runs purely on Go's GC with zero extra VM GC overhead; Tier 1 activates lazily on `__gc`/`__mode` and executes PUC-Rio `lgc.c:atomic()`'s exact 6-step order (`convergeEphemerons`, pre-`__gc` `clearByValues`, LIFO `separateToBeFnz`, resurrection, `clearByKeys`, post-resurrection `clearByValues`, and `remarkupvals`) with proportional live-heap pacing.
  - **Compact 112-Byte Tables, Shapes, Inline Caches, and Homogeneous Arrays**: hidden classes (`Shape`) with metatable encoding, 2 inline record slots, lazy hash parts, mono/4-way polymorphic ICs, and `noscan` `[]int64`/`[]float64`/`[]bool` dense arrays.
  - **Frozen Stdlib Template**: `NewState()` clones a fully isolated Lua 5.5.1 state with the complete standard library in **4.46 µs / 4 allocations** (`GOMAXPROCS=1`).
  - **Production Embedding Guards**:
    - `WithContext(ctx)` / `SetContext(ctx)` cooperative cancellation & deadlines (cannot be swallowed by `pcall`/`xpcall`/`coroutine.resume`, zero cost on the fast loop when unset).
    - `WithInstructionLimit(n)` / `SetInstructionLimit(n)` deterministic VM step budget (cannot be swallowed by `pcall`/`xpcall`/`coroutine.resume`, zero cost when unset).
    - `WithMemoryLimit(bytes)` per-State VM memory cap with emergency GC recount raising recoverable Lua `"not enough memory"` errors.
    - `WithFS(fsys)` / `AddFS(fsys)` / `AddSearcher` for loading Lua source and `.luac` modules directly from Go `embed.FS` / `io/fs.FS` with path-traversal protection.
    - `RegisterFunc1` / `NewFunc1` (`NativeFunc.FastFn`) zero-allocation single-return Go callbacks (**17.91 ms / 0 per-call allocs** for 1M calls).
    - `State.Pin(v)` / `Handle.Release()` GC-anchored host references and nil-receiver / post-`Close()` safety guards across `State`, `Table`, `Chunk`, and `Handle`.
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

### Sharing Compiled Chunks & Concurrency Model

Each `*moelua.State` is strictly **single-goroutine** and lock-free (matching PUC-Rio `lua_State`). Compiled `*moelua.Chunk` values are immutable and may be loaded concurrently into independent `*moelua.State` instances running on separate goroutines.

```go
chunk, err := moelua.Compile("=worker", `return ... * 2`)
// Share `chunk` across independent States on separate goroutines:
fn, _ := state.Load(chunk)
out, _ := state.Call(fn, moelua.Int(21))
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

On Apple M1 Max (`darwin/arm64`, Go 1.27.4, 10-round interleaved ABAB, strict single-core `GOMAXPROCS=1` CPU time vs C Lua 5.5.1 `clang -O2` and `-O3 -mcpu=apple-m1`). Reproduce after `git submodule update --init` with `make bench` (No-PGO) or `make bench-pgo` (disjoint PGO trained solely on official `testes/`):

- **12-benchmark Core VM Geomean**: **0.89×** single-core CPU vs C `-O2` (**0.76×** vs C `-O3 -mcpu=apple-m1` in each paired 10-round ABAB run for both No-PGO and Disjoint-PGO; **0.87×** / **0.74×** in equal-footing subprocess mode)
- **Realistic OOP / ECS workloads**: `ecs_stdlib` **1.12×** vs C `-O2` (**1.18×** vs `-O3`; subprocess **1.09×** / **1.12×**), `richards` **1.02×** vs C `-O2` (**0.90×** vs `-O3`; subprocess **1.01×**), `deltablue` **0.98×** vs C `-O2` (**0.94×** vs `-O3`; subprocess **0.89×**)
- **Core algorithms**: `fib(34)` **0.91×**, `matmul` **0.91×**, `nbody` **0.85×**, `forloop_int` **0.84×**, `spectralnorm` **0.84×**, `sieve` **0.83×**, `fannkuch` **0.83×**, `binarytrees` **0.80×** (Peak RSS **24.7 MiB**), `mandelbrot` **0.79×** vs C `-O2`
- **Stdlib typed-array sort (`sort_int`, reported separately)**: **3.62×** vs C `-O2` (**3.35×** vs `-O3`)
- **Embedding (`GOMAXPROCS=1`)**: `NewState` **4.46 µs / 4 allocs** (`25,296 B`), Go→Lua call **76.3 ns**, Lua→Go 1M calls **17.91 ms (~17.9 ns/call, 0 allocs/call)** via `RegisterFunc1`/`FastFn` (**34.02 ms / 1 alloc/call** via multi-return `Fn`)
- **Tier 1 GC proportional pacing overhead**: 12-benchmark geomean **0.88×** vs Tier 0 **0.89×** (`~0.0%–1.7%` across non-allocation-heavy workloads; `+11.8%` on allocation-intensive `binarytrees`)

See [`docs/benchmarks.md`](docs/benchmarks.md) for full tables, IQR spreads, Peak RSS, subprocess mode, Tier 1 overhead, and conformance details.

## Trust Model & Resource Limits

- **Binary chunks are trusted input**: `bytecode.Undump` and `bytecode.Verify` provide panic-freedom defense in depth (nesting depth caps, CFG successor validation, register/constant/upvalue bounds), matching PUC-Rio Lua's trust model rather than acting as an untrusted-bytecode sandbox.
- **Per-State memory limit (`WithMemoryLimit`)**: accounts Lua tables, live strings, closures, threads, and stack growth with emergency GC recount (skipping `__gc` finalizers mid-allocation) and raises recoverable `"not enough memory"` errors; it is a VM-level resource guard rather than an OS RSS cgroup.
- **Cooperative cancellation & step budgets (`WithContext`, `WithInstructionLimit`)**: checked inside `runHooked` (zero overhead in the default fast loop when unset, including non-cancelable `context.Background()`), at call boundaries, and periodically inside pattern matching (`string.find`/`match`/`gmatch`/`gsub`). Treated as uncatchable by Lua `pcall`/`xpcall`, `coroutine.resume`/`wrap`, `load`, `<close>`, and `__gc`; hard cancellation unwinds frames and discards pending Lua `<close>` metamethods above the aborted boundary without running arbitrary Lua code. Blocking Go host callbacks or OS I/O remain the caller's responsibility.
- **Zero `recover()` in `vm/`**: Lua errors propagate strictly via Go error return values; runtime panics are never masked as Lua errors.
