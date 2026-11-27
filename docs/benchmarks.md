# MoeLua 5.5 — Verification & Benchmark Report

- **Target Language**: Official PUC-Rio Lua **5.5.1** only
- **Implementation**: Zero-CGo (`CGO_ENABLED=0`), zero third-party dependencies, per-`State` noscan `[]byte` Arena (`ptrdata == 0`) + true PUC-Rio Lua 5.5.1 incremental/generational GC + generational `GoRefs` host bridge
  - **`MoeLua 5.5 (noJIT)`** *(default)*: Plan 9 direct-threaded bytecode interpreter on ARM64/AMD64 (pure-Go fallback via `-tags purego` and on all other architectures)
  - **`MoeLua 5.5 (JIT)`** *(opt-in via `moelua.WithJIT()` or `moelua -jit`)*: Native ARM64/AMD64 baseline compiler + typed-SSA loop optimizer (BCE, Tier-2 Shape/inlining feedback, ARM64 NEON / AMD64 SSE2 2-wide `f64` auto-vectorization) + shared reference-counted Tier-1 code cache across independent `State`s (excluded via `-tags moeluanojit`; automatic interpreter fallback on unsupported platforms or `PROT_EXEC` denial)
- **Test Environment**: Apple M1 Max (`darwin/arm64`, 10 cores), Go `1.27.4`, C baseline `onelua.c` (Lua `5.5.1`, Apple `clang 16.0.0`, `-O2` and `-O3 -mcpu=apple-m1`)

---

## 1. Conformance & Verification Summary

### 1A. Official PUC-Rio Lua 5.5.1 Compiler Identity
- Verified byte-for-byte identical binary dumps (`compiler.Compile` + `bytecode.Dump` vs official `luac 5.5.1`) across:
  - **34 / 34** official `testes/*.lua` files (normal and stripped dumps)
  - **13 / 13** benchmark scripts (`bench/scripts/*.lua`)
  - **70 / 70** constant-folding and codegen edge cases (`-0.0`, `NaN`, `mininteger // -1`, division/modulo by zero, bitwise shifts, `global <const> *`, `<close>`, named/hidden varargs, and 60 synthesized operator combinations)

### 1B. Official PUC-Rio Lua 5.5.1 Runtime `testes/` (`_U = true` + `big.lua` + `main.lua`)
- **Portable `_U=true` suite (`TestOfficialLua55Testes`)**: **26 / 26** unmodified files pass end-to-end on Linux (including full `files.lua` with `/dev/full`); on macOS, **25 / 26** pass unmodified (`bitwise`, `literals`, `tpack`, `utf8`, `pm`, `vararg`, `math`, `sort`, `constructs`, `goto`, `closure`, `strings`, `attrib`, `events`, `nextvar`, `calls`, `locals`, `coroutine`, `errors`, `db`, `gc`, `gengc`, `cstack`, `verybig`, and `big.lua`), plus `files.lua` matches official C `lua55_o2` identically (unmodified `files.lua` passes lines 1–473 and hits macOS `/dev/full` permission failure at line 474 identically to C; omitting only the 6-line `/dev/full` block passes lines 483–1004).
- **Standalone CLI (`cmd/moelua`, `TestOfficialMainLuaCLI`)**: passes official PUC-Rio `testes/main.lua` end-to-end (omitting only the 4-line C `.so` `dlopen` test, out of scope for `CGO_ENABLED=0`), including `-jit`, `-e`, `-l`, `-i`, `-v`, `-W`, `-E`, `--`, `-`, BOM/shebang handling, `LUA_INIT_5_5`/`LUA_PATH_5_5`, `;;` path expansion, `arg` table, `debug.debug()`, interactive REPL with `_PROMPT` metamethods, `os.exit` `<close>`/`__gc` ordering, and `SIGINT` handling.
- **Hooked dispatch parity (`TestOfficialLua55TestesHooked`)**: 23 / 23 portable files pass inside `(*State).runHooked` with an active count hook.
- **Differential parity against live C `lua55_o2`**:
  - Pure-Go stdlib kernels (`internal/luastdlib`): patterns, `string.format`, `string.pack`/`unpack`, `utf8`, `math` RNG, number parsing.
  - **15 / 15** yield-across-native/metamethod matrix (`TestPhase2P6YieldAcrossNativeMatrix`): `table.sort`, `string.gsub`, `load` reader, `tostring`/`print`, `require`, `table.insert`/`concat`, `coroutine.close`, `pcall`/`xpcall`, `pairs`, iterators, and arithmetic/index/`<close>` metamethods.
  - Float array `table.sort` with `NaN` (`TestPhase2P5FloatSortNaNParity`).

### 1C. Phase 4 Noscan Arena, Per-State GC & Multi-Configuration Matrix
- **Zero-Pointer Arena Guarantee (`TestPhase4NoscanArenaStructZeroPointers`)**: verified by reflection across all 17 Arena VM types (`Value`, `LuaString`, `Table`, `tableHash`, `hashNode`, `Shape`, `shapeTrans`, `shapeIndexEntry`, `ICEntry`, `polyEntry`, `Thread`, `callFrame`, `Closure`, `Upvalue`, `ArenaNative`, `Userdata`, `ArenaProto`) — zero Go pointers inside any Lua VM heap slab.
- **True Per-State Lua 5.5.1 Collector (`internal/arena/gc.go`, `vm/gc.go`)**: unified PUC-Rio `lgc.c` incremental tri-color + generational minor/major GC with exact 6-step `atomic()`, ephemeron convergence, LIFO `__gc` finalizers, resurrection, weak-table pruning, and automatic `GoRefs` release on sweep — always active with no Tier 0 / Tier 1 split.
- **All packages pass under**:
  - `default` (`opt + intnil`, Plan 9 threaded interpreter + optional native JIT)
  - `-tags moeluanojit` (Plan 9 threaded interpreter, native JIT excluded at compile time)
  - `-tags purego` and `-tags "purego moeluanojit"` (100% pure-Go fallback, zero assembly)
  - `-tags moeluaregcheck` (per-instruction `stackGen`, register-window, PC, and `callFrame.dirty` clean-frame verification)
  - `-tags moeluafloatnil`
  - `-tags moeluafaithful`
  - `-race` (including concurrent independent `State`s sharing immutable `*bytecode.Proto`, shared Tier-1 JIT code regions, atomic copy-on-write static native registry, and frozen stdlib string arena)
  - `GOGC=1 GODEBUG=gccheckmark=1,clobberfree=1` and `-gcflags=all=-d=checkptr=2`
  - Native `CGO_ENABLED=0` builds and test matrices on Linux, macOS, and Windows (automated via `.github/workflows/ci.yml`, plus Linux `GOGC=1` + `checkptr=2` stress)
  - `TestRunIsNotBig` (`-gcflags=-m=2` verifies zero `'big'` warnings and confirms hot helpers inline cleanly in both `interp_run.go` and `interp_run_hooked.go`) + `TestGeneratedInterpFresh`
- **Fuzzing (zero Go panics; zero `recover()` inside `vm/`)**:
  - `FuzzBytecode`, `FuzzRun`, `FuzzStructuredProto` (under `-tags moeluaregcheck`), and `FuzzCompileIdentity` totaling **10.5M+** executions.

### 1D. Phase 5A Threaded Interpreter & Phase 5B Native JIT Verification
- **3-Way Execution Parity (`Pure-Go` vs `Plan 9 Threaded (noJIT)` vs `Native JIT`)**:
  - Official `testes/` suite passes with `SetJITEnabled(true)` on both native ARM64 (`darwin/arm64`) and AMD64 (`linux/amd64`, `windows/amd64`, Rosetta 2 `darwin/amd64`).
  - `bench/compare` `TestOutputs` verifies **32 / 32** byte-exact stdout parity against C Lua 5.5.1 `-O2` for both `MoeLua` (`noJIT`) and `MoeLua-JIT` on ARM64 and AMD64.
  - Dedicated typed-SSA / BCE / Tier-2 feedback / NEON & SSE2 SIMD deopt-safety tests (`vm/jit_opt_test.go`, `internal/jit/ir/ir_test.go`) verify cross-object field aliasing, int64/float64 comparison precision, negative-zero / `NaN` semantics, non-unit stride, dynamic array growth, metamethod installation, `WithContext` / `WithInstructionLimit` / `SIGINT` poll safepoints, and `WithMemoryLimit`.
  - Shared Tier-1 executable code cache (`vm/jit_cache.go`, `vm/jit_cache_test.go`) verified under `-race` across 16 concurrent goroutines × 4 States, structural-fingerprint LRU deduplication across repeated `DoString` parses, SIMD-toggle key separation, and reference-counted RX region lifetime after cache eviction and partial `State.Close()`.

---

## 2. Benchmark Methodology

- **Cross-engine suite (`bench/compare`)**: isolated nested Go module comparing 13 engine configurations across 32 programs (`14 AWFY + 6 Benchmarks Game + 5 SciMark 2.0a + 4 Go engines' own + 3 micro`), `-benchtime 300ms -count 3`, `GOMAXPROCS=1`, median of 3 rounds, reported as `engine_ns / C_O2_ns` (`< 1.000` means faster than C Lua 5.5.1 `-O2`).
- **Core VM suite (`cmd/benchcmp`)**: strict single-core **`GOMAXPROCS=1` CPU time (`user + sys`)**, **10 interleaved ABAB rounds** after warmup, reporting **median ± IQR (`p75 - p25`)** as `C_cpu / Moe_cpu` (`> 1.00×` means MoeLua is faster), plus child-process `rusage.Maxrss` Peak RSS.
- **High-density multi-VM (`bench/compare/memcmp.go` & `bench/multivm_bench_test.go`)**: isolated OS subprocess per engine (`GOMAXPROCS=1`), measuring 256 idle full-stdlib VMs (`NewState` cold creation, Go heap, OS RSS, `runtime.GC()`) and scaling active VMs across `64 → 256 → 512 → 1,000` with a 2,047-table binary tree per VM (up to **2.05M live Lua tables** at 1,000 VMs).

---

## 3. Core VM Reference Benchmarks (`GOMAXPROCS=1`, Pure-Go Phase 4 Baseline, Multi-VM & Embedding)

### 3A. Pure-Go Interpreter Reference (`-tags "purego moeluanojit"` / Phase 4, 10-Round ABAB, Peak RSS)

*(See Section 4 below for current Phase 5A Plan 9 threaded `MoeLua 5.5 (noJIT)` and Phase 5B `MoeLua 5.5 (JIT)` cross-engine numbers across all 32 programs.)*

| Benchmark | Class | C `-O2` CPU (±IQR) | C `-O3 -mcpu=apple-m1` CPU (±IQR) | Pure-Go No-PGO CPU (±IQR) | Pure-Go vs `-O2` (C / W) | Pure-Go vs `-O3` (C) | Peak RSS C / Moe (MiB) |
|---|---|--:|--:|--:|--:|--:|--:|
| `fib(34)` | (a) | 335.5 (±5.5) ms | 299.2 (±4.9) ms | 406.8 (±2.9) ms | 0.82× / 0.83× | 0.74× | 1.5 / 6.3 |
| `forloop_int` (50M) | (a) | 493.9 (±2.9) ms | 389.0 (±0.4) ms | 508.5 (±1.8) ms | **0.97× / 0.98×** | **0.76×** | 1.6 / 6.0 |
| `sieve` (500×5k) | (a) | 85.6 (±1.7) ms | 72.6 (±2.9) ms | 106.9 (±1.6) ms | 0.80× / 0.82× | 0.68× | 2.1 / 6.3 |
| `mandelbrot` (500) | (a) | 197.1 (±2.2) ms | 149.4 (±2.3) ms | 222.1 (±1.8) ms | **0.89× / 0.90×** | 0.67× | 1.6 / 6.1 |
| `fannkuch` (9) | (a) | 126.5 (±2.5) ms | 104.4 (±4.0) ms | 150.1 (±2.1) ms | 0.84× / 0.86× | 0.70× | 1.5 / 5.9 |
| `richards` (20×) | (b) | 541.1 (±35.8) ms | 503.7 (±16.6) ms | 652.9 (±3.2) ms | 0.83× / 0.84× | **0.77×** | 2.0 / 6.5 |
| `deltablue` (25×) | (b) | 68.6 (±4.0) ms | 65.3 (±1.1) ms | 69.8 (±0.8) ms | **0.98× / 1.01×** | **0.94×** | 2.4 / 10.5 |
| `nbody` (200k) | (b) | 283.4 (±6.5) ms | 236.6 (±19.6) ms | 357.7 (±2.7) ms | 0.79× / 0.80× | 0.66× | 1.6 / 6.2 |
| `binarytrees` (13) | (b) | 174.8 (±6.0) ms | 152.6 (±3.7) ms | 200.5 (±3.5) ms | **0.87× / 0.88×** | **0.76×** | 7.5 / **18.1** |
| `ecs_stdlib` | (b) | 77.4 (±7.5) ms | 74.1 (±2.7) ms | 79.4 (±0.6) ms | **0.97× / 0.99×** | **0.93×** | 2.1 / 6.8 |
| `spectralnorm` (400) | (c) | 260.7 (±1.7) ms | 209.0 (±5.0) ms | 335.1 (±5.0) ms | 0.78× / 0.79× | 0.62× | 1.7 / 6.3 |
| `matmul` (160²) | (c) | 96.9 (±1.3) ms | 70.7 (±1.5) ms | 93.9 (±1.7) ms | **1.03× / 1.05×** | **0.75×** | 2.4 / 6.8 |
| **Core VM Geomean (`n=12`)** | | | | *(Phase 5A Plan 9 threaded noJIT: **1.08×** vs `-O2`; Phase 5B JIT: **3.77×** vs `-O2`, `matmul` **63.98×** via NEON SIMD)* | **0.88× / 0.89×** | **0.74×** | |

### 3B. Stdlib Algorithmic Fast Path (excluded from VM Geomean)

| Benchmark | C `-O2` CPU (±IQR) | C `-O3` CPU (±IQR) | Moe Pure-Go CPU (±IQR) | vs `-O2` (C / W) | vs `-O3` (C) | Peak RSS C / Moe (MiB) |
|---|--:|--:|--:|--:|--:|--:|
| `sort_int` (5×50k) | 60.5 (±2.0) ms | 56.5 (±1.5) ms | 16.8 (±0.8) ms | 3.61× / 3.70× | 3.37× | 3.9 / 8.0 |

### 3C. High-Density Multi-VM Go GC Scalability & Per-VM Memory Footprint (`./bench`, `count=5` medians)

Because all Lua VM objects live inside per-`State` noscan `[]byte` Arenas (`ptrdata == 0`) and stdlib snapshots are compacted via `Arena.CompactSnapshot()` with progressive non-moving slab page sizing (`2 KiB → 8 KiB → 32 KiB → 64 KiB`), Go's garbage collector performs zero pointer scanning inside Lua heaps even when hundreds or thousands of independent VMs are live in one process:

| Benchmark | Live VMs | Live Lua Objects | Median `runtime.GC()` | Arena Backing / VM | Go `HeapAlloc` / VM | Notes |
|---|--:|--:|--:|--:|--:|---|
| `BenchmarkIdleVMFootprint1000` (`./bench`) | 1,000 | Full stdlib | **0.75 ms** | **42.94 KiB** | **52.84 KiB** | 51.6 MiB total Go heap for 1,000 idle States (`collectgarbage("count")` = 45.39 KiB vs C Lua 5.5.1's 19.32 KiB) |
| `BenchmarkMultiVMGoGCScan/256VMs` (`./bench`) | 256 | 418,816 (1,636/VM) | **1.07 ms** | 245.6 KiB | 262.5 KiB | Tables, nested records, dynamic strings, closures, and suspended coroutine per VM |
| `BenchmarkMultiVMGoGCScan/1000VMs` (`./bench`) | 1,000 | 1,636,000 (1,636/VM) | **3.36 ms** | 245.6 KiB | 262.5 KiB | 1.64M live Lua objects across 1,000 independent States |
| `BenchmarkMultiVMGoGCScan/NoscanArena` (`./internal/arena`) | 256 | 512,000 (2,000/VM) | **0.289 ms** | — | — | **7.9× faster Go GC** vs identical Go-heap pointer graph (`2.273 ms`) |

- **Arena pointer dereference microbenchmark (`./internal/arena`, `BenchmarkArenaPtrDeref`)**: `Ptr` **2.52 ns**, `PtrFast` **2.20 ns**, `PtrSmall` **2.21 ns**, `PtrInPages` **2.21 ns** (0 allocs).

### 3D. Go-Host Embedding Microbenchmarks (`GOMAXPROCS=1`, `go test -bench=. -benchmem -count=5 ./bench`)

| Benchmark | Median Time | Bytes / op | Allocs / op | Notes |
|---|--:|--:|--:|---|
| `BenchmarkNewState` | **4.81 µs** | 54,104 B | 32 | Full Lua 5.5.1 stdlib cloned verbatim via compacted `Arena.CloneSnapshot()` (zero pointer relocation) |
| `BenchmarkGoCallLua` | **92.1 ns** | 16 B | **1** | Multi-return `State.Call` with `Thread`/`nCcalls`/`nny` boundary bookkeeping |
| `BenchmarkGoCallLua1` | **39.2 ns** | **0 B** | **0** | Zero-allocation single-return `State.Call1` |
| `BenchmarkLuaCallGoHost1M` | **48.01 ms** (~48.0 ns/call) | 16 B/call | 1/call | Standard multi-return `NativeFunc.Fn` / `RegisterFunc` (via `GoRefs` handle bridge) |
| `BenchmarkLuaCallGoHostFast1M` | **29.53 ms** (~29.5 ns/call) | **16 B total** | **1 total (0/call)** | Zero-allocation single-return `NativeFunc.FastFn` / `RegisterFunc1` |

---

## 4. Standardized Cross-Engine Comparison (`bench/compare`)

Measured via the isolated nested Go module [`bench/compare`](../bench/compare) (adapted from `modernc.org/lua/compare`) on Apple M1 Max (`darwin/arm64`, Go `1.27.4`, `GOMAXPROCS=1`, `-benchtime 300ms -count 3`, median of 3 rounds). Every engine runs identical Lua 5.1-compatible source across 32 programs and is verified byte-for-byte against C Lua 5.5.1 via `TestOutputs`. Reproduce locally with `make bench-compare` or on GitHub Actions via the `Cross-Engine Benchmarks` (`workflow_dispatch`) workflow.

### 4A. Suite Geomeans (Time/op Relative to C Lua 5.5.1 `-O2`, Lower Is Faster)

| Category | Engine | Lua | AWFY (14) | Game (6) | SciMark (5) | Own (4) | Micro (3) | **Common 27** | **All 32** |
|---|---|---|---:|---:|---:|---:|---:|---:|---:|
| Native Trace JIT | LuaJIT 2.1 (JIT) | 5.1 | 0.180 | 0.159 | 0.099 | 0.462 | 0.415 | **0.167** | **0.194** |
| **Go + Native JIT #1** | **MoeLua 5.5 (JIT)** | **5.5** | **0.775** | **0.695** | **0.260** | **0.391** | **2.099** | **0.527** | **0.645** |
| Go + ARM64 JIT #2 | Apogee v1.2.1 (JIT) | 5.5 | 0.916 | 0.637 | 0.254 | 0.556 | 3.412 | **0.573** | **0.744** |
| Reference C | C Lua 5.5.1 `-O3` | 5.5 | 0.924 | 0.861 | 0.798 | 0.869 | 1.012 | **0.875** | **0.892** |
| Native Asm Interp | LuaJIT 2.1 (`-joff`) | 5.1 | 0.954 | 0.990 | 0.890 | 1.006 | 4.465 | **0.972** | **1.106** |
| **Go Interp #1 (noJIT)** | **MoeLua 5.5 (noJIT)** | **5.5** | **1.086** | **1.113** | **0.950** | **0.809** | **2.325** | **0.990** | **1.106** |
| Reference C | C Lua 5.5.1 `-O2` | 5.5 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | **1.000** | **1.000** |
| Go Interp #2 | Apogee v1.2.1 (`WithoutJIT`) | 5.5 | 1.936 | 1.928 | 2.053 | 1.669 | 4.091 | **1.898** | **2.056** |
| Go Interp #3 | `arnodel/golua` (`default.pgo`) | 5.5 | 2.955 | 2.682 | 2.380 | 2.641 | 4.869 | **2.734** | **2.899** |
| Go (ccgo) #4 | `modernc.org/lua` v0.2.0 (`CCGO`) | 5.5 | 2.632 | 2.736 | 2.865 | 2.658 | 3.558 | **2.750** | **2.767** |
| Go Interp #5 | `yuin/gopher-lua` | 5.1 | 5.252 | 5.028 | 5.760 | 3.714 | 8.607 | **5.075** | **5.301** |
| Go Interp #6 | `Shopify/go-lua` | 5.2 | 7.700* | 6.054 | 6.315 | 5.110 | 6.465 | **6.792** | **6.638** |
| Go Interp #7 | `milochristiansen/lua` (`DCLua`) | 5.3 | 9.341 | 9.384* | 9.795 | 11.170 | 8.045 | **9.829** | **9.502** |

*\*Skipped per `bench/compare` defaults: `Shopify/go-lua` on `awfy/Havlak` (too slow); `DCLua` on `game/binarytrees` (wrong output).*

### 4B. Per-Program Ratios (Relative to C Lua 5.5.1 `-O2`, Median of 3 Rounds)

| Suite / Program | C `-O2` | C `-O3` | **MoeLua 5.5 (JIT)** | **MoeLua 5.5 (noJIT)** | Apogee-JIT | Apogee-noJIT | CCGO | golua | GopherLua | go-lua | DCLua |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `awfy/Bounce` | 1.000 | 0.971 | **0.545** | **0.926** | 0.900 | 1.953 | 2.864 | 3.066 | 5.536 | 8.072 | 11.200 |
| `awfy/CD` | 1.000 | 0.930 | **1.253** | **1.289** | 3.010 | 2.169 | 2.372 | 4.140 | 6.150 | 7.216 | 7.730 |
| `awfy/DeltaBlue` | 1.000 | 1.000 | **1.135** | **1.126** | 2.111 | 2.023 | 2.329 | 3.490 | 5.307 | 78.480 | 8.385 |
| `awfy/Havlak` | 1.000 | 0.956 | **1.404** | **1.341** | 1.655 | 1.787 | 2.059 | 3.095 | 6.190 | n/a | 6.278 |
| `awfy/Json` | 1.000 | 0.973 | **1.381** | **1.413** | 2.258 | 2.245 | 2.550 | 3.889 | 5.201 | 6.106 | 8.245 |
| `awfy/List` | 1.000 | 0.939 | **0.668** | **1.116** | 0.789 | 1.902 | 3.050 | 2.835 | 4.281 | 4.727 | 11.201 |
| `awfy/Mandelbrot` | 1.000 | 0.800 | **0.261** | **1.072** | 0.169 | 1.564 | 2.957 | 2.296 | 5.334 | 6.167 | 10.981 |
| `awfy/NBody` | 1.000 | 0.884 | **0.694** | **1.004** | 0.420 | 1.799 | 3.519 | 2.589 | 7.913 | 10.302 | 14.992 |
| `awfy/Permute` | 1.000 | 0.925 | **0.707** | **1.030** | 0.559 | 2.127 | 2.971 | 2.743 | 5.037 | 6.911 | 11.884 |
| `awfy/Queens` | 1.000 | 0.886 | **0.724** | **1.239** | 0.680 | 1.791 | 2.295 | 2.428 | 3.555 | 4.588 | 8.839 |
| `awfy/Richards` | 1.000 | 0.894 | **0.783** | **0.842** | 0.652 | 1.424 | 2.396 | 3.134 | 4.250 | 5.225 | 8.497 |
| `awfy/Sieve` | 1.000 | 0.856 | **0.463** | **0.895** | 0.669 | 3.427 | 2.987 | 3.229 | 6.614 | 6.470 | 10.278 |
| `awfy/Storage` | 1.000 | 0.958 | **0.662** | **0.747** | 2.012 | 1.653 | 2.060 | 2.139 | 4.865 | 4.950 | 5.339 |
| `awfy/Towers` | 1.000 | 0.992 | **1.271** | **1.437** | 0.728 | 1.843 | 2.885 | 2.992 | 4.721 | 7.527 | 11.352 |
| `game/binarytrees` | 1.000 | 0.952 | **2.007** | **1.942** | 3.989 | 2.590 | 2.059 | 2.795 | 3.431 | 3.875 | n/a |
| `game/fannkuchredux` | 1.000 | 0.800 | **0.311** | **0.820** | 0.387 | 2.281 | 2.697 | 2.048 | 3.812 | 5.268 | 6.471 |
| `game/fasta` | 1.000 | 0.952 | **0.991** | **1.229** | 2.206 | 1.937 | 2.568 | 3.879 | 5.595 | 5.764 | 6.922 |
| `game/mandelbrot` | 1.000 | 0.883 | **0.812** | **0.977** | 0.138 | 1.527 | 2.963 | 2.473 | 6.143 | 6.877 | 12.914 |
| `game/nbody` | 1.000 | 0.834 | **0.699** | **0.951** | 0.499 | 1.718 | 3.551 | 2.512 | 7.902 | 9.950 | 15.104 |
| `game/spectralnorm` | 1.000 | 0.763 | **0.322** | **1.045** | 0.284 | 1.710 | 2.797 | 2.699 | 4.545 | 6.114 | 8.331 |
| `scimark/FFT` | 1.000 | 0.830 | **0.398** | **1.063** | 0.200 | 1.935 | 2.915 | 2.282 | 6.790 | 6.742 | 10.994 |
| `scimark/LU` | 1.000 | 0.693 | **0.050** | **0.738** | 0.125 | 2.071 | 2.722 | 2.072 | 6.161 | 6.325 | 9.000 |
| `scimark/MC` | 1.000 | 0.829 | **0.778** | **1.219** | 0.662 | 1.859 | 2.390 | 2.906 | 4.687 | 4.622 | 8.161 |
| `scimark/SOR` | 1.000 | 0.848 | **0.204** | **0.941** | 0.215 | 2.045 | 3.029 | 2.246 | 4.807 | 6.732 | 9.364 |
| `scimark/SPARSE` | 1.000 | 0.802 | **0.374** | **0.861** | 0.296 | 2.392 | 3.361 | 2.476 | 6.729 | 7.569 | 11.923 |
| `own/fib35` | 1.000 | 0.922 | **0.848** | **0.899** | 0.695 | 2.016 | 2.444 | 3.554 | 3.868 | 4.446 | 7.888 |
| `own/fibonacci` | 1.000 | 0.766 | **0.053** | **0.896** | 0.104 | 2.182 | 4.176 | 2.712 | 7.244 | 8.390 | 14.406 |
| `own/sort` | 1.000 | 0.930 | **0.324** | **0.355** | 1.058 | 1.136 | 1.942 | 2.055 | 1.682 | 2.695 | 15.013 |
| `own/sort2` | 1.000 | 0.871 | **1.605** | **1.497** | 1.248 | 1.555 | 2.518 | 2.456 | 4.039 | 6.779 | 9.126 |
| `micro/call` | 1.000 | 1.008 | **0.869** | **1.332** | 0.807 | 1.839 | 2.605 | 2.595 | 4.723 | 5.433 | 8.860 |
| `micro/error` | 1.000 | 1.009 | **2.832** | **2.784** | 6.235 | 5.723 | 5.301 | 7.636 | 30.292 | 5.369 | 7.504 |
| `micro/pcall` | 1.000 | 1.019 | **3.761** | **3.386** | 7.900 | 6.507 | 3.261 | 5.825 | 4.456 | 9.263 | 7.831 |

### 4C. Isolated-Process High-Density Multi-VM Memory & Go GC Comparison (`cd bench/compare && go run memcmp.go`)

Each engine runs in its own isolated OS subprocess (`GOMAXPROCS=1`): 256 idle full-stdlib VMs (measuring average `NewState` cold creation, heap, OS RSS, and Go GC), followed by scaling active VMs from `64 → 256 → 512 → 1,000` with a 2,047-table binary tree inside every VM (up to **2,047,000 live Lua tables** at 1,000 VMs).

| Engine | `NewState` (256 live) | Idle Heap / VM | Idle RSS / VM | 256-Idle GC | 256-Act RSS / VM | 1,000-Act RSS / VM | 64-Act GC | 256-Act GC | 512-Act GC | **1,000-Act GC (2.05M tables)** |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| C Lua 5.5.1 *(ref)* | 38.7 µs | n/a | 21.2 KiB | n/a | n/a | 264.4 KiB | n/a | n/a | n/a | n/a |
| **MoeLua 5.5 (noJIT)** | **26.8 µs** | **52.9 KiB** | **53.6 KiB** | **0.77 ms** | **332.0 KiB** | **325.7 KiB** | **0.40 ms** | **1.05 ms** | **1.85 ms** | **3.57 ms** |
| **MoeLua 5.5 (JIT)** | **11.5 µs** | **53.0 KiB** | **53.7 KiB** | **0.53 ms** | **333.9 KiB** | **326.8 KiB** | **0.41 ms** | **0.99 ms** | **1.95 ms** | **3.75 ms** |
| `modernc.org/lua` (`CCGO`) | 33.5 µs | 1.6 KiB *(off-heap mmap)* | 32.2 KiB | 0.20 ms | 275.8 KiB | 274.4 KiB | 0.17 ms | 0.25 ms | 0.31 ms | 0.46 ms |
| Apogee v1.2.1 (`WithoutJIT`) | 215.7 µs | 174.6 KiB | 192.3 KiB | 20.34 ms | 514.8 KiB | 510.0 KiB | 12.62 ms | 55.20 ms | 111.29 ms | 218.27 ms |
| Apogee v1.2.1 (JIT) | 217.8 µs | 174.6 KiB | 191.2 KiB | 20.01 ms | 548.9 KiB | 557.8 KiB | 12.35 ms | 55.15 ms | 111.87 ms | 220.21 ms |
| `arnodel/golua` | 63.4 µs | 113.7 KiB | 61.8 KiB | 3.21 ms | 636.7 KiB | 623.4 KiB | 12.31 ms | 54.70 ms | 111.72 ms | 217.81 ms |
| `milochristiansen/lua` (`DCLua`) | 42.6 µs | 66.5 KiB | 78.8 KiB | 4.01 ms | 1,121.6 KiB | 1,090.4 KiB | 19.73 ms | 75.54 ms | 154.37 ms | 301.20 ms |
| `Shopify/go-lua` | 31.8 µs | 17.2 KiB | 24.2 KiB | 2.40 ms | 1,024.8 KiB | 1,021.1 KiB | 21.71 ms | 88.90 ms | 175.47 ms | 345.18 ms |
| `yuin/gopher-lua` | 67.7 µs | 162.4 KiB | 176.6 KiB | 9.67 ms | 2,024.6 KiB | 2,008.6 KiB | 42.68 ms | 180.85 ms | 365.22 ms | 719.80 ms |

---

## 5. Trust Model & Resource Limits

- **Binary chunks are trusted input**: `bytecode.Undump` and `bytecode.Verify` provide panic-freedom defense in depth (nesting depth caps, CFG successor validation, register/constant/upvalue bounds), matching PUC-Rio Lua's trust model rather than acting as an untrusted-bytecode sandbox.
- **Per-State memory limit (`WithMemoryLimit`)**: accounts Lua tables, shapes, inline caches, strings, closures, threads, and stack/frame growth (including Arena size-class rounding) with emergency GC recount (skipping `__gc` finalizers mid-allocation) and raises recoverable `"not enough memory"` errors; it is a VM-level resource guard rather than an OS RSS cgroup.
- **Cooperative cancellation & step budgets (`WithContext`, `WithInstructionLimit`)**: checked inside `runHooked` (zero overhead in the default fast loop when unset, including non-cancelable `context.Background()`), at native JIT poll safepoints, at call boundaries, and periodically inside pattern matching (`string.find`/`match`/`gmatch`/`gsub`). Treated as uncatchable by Lua `pcall`/`xpcall`, `coroutine.resume`/`wrap`, `load`, `<close>`, and `__gc`; hard cancellation unwinds frames and discards pending Lua `<close>` metamethods above the aborted boundary without running arbitrary Lua code. Blocking Go host callbacks or OS I/O remain the caller's responsibility.
- **Zero `recover()` in `vm/`**: Lua errors propagate strictly via Go error return values; runtime panics are never masked as Lua errors.
