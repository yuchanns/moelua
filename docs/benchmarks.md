# MoeLua 5.5 — Verification & Benchmark Report

- **Target Language**: Official PUC-Rio Lua **5.5.1** only
- **Implementation**: Pure Go (`CGO_ENABLED=0`), zero assembly, zero JIT, Go-GC-backed
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
- **Standalone CLI (`cmd/moelua`, `TestOfficialMainLuaCLI`)**: passes official PUC-Rio `testes/main.lua` end-to-end (omitting only the 4-line C `.so` `dlopen` test, out of scope for `CGO_ENABLED=0`), including `-e`, `-l`, `-i`, `-v`, `-W`, `-E`, `--`, `-`, BOM/shebang handling, `LUA_INIT_5_5`/`LUA_PATH_5_5`, `;;` path expansion, `arg` table, `debug.debug()`, interactive REPL with `_PROMPT` metamethods, `os.exit` `<close>`/`__gc` ordering, and `SIGINT` handling.
- **Hooked dispatch parity (`TestOfficialLua55TestesHooked`)**: 23 / 23 portable files pass inside `(*State).runHooked` with an active count hook.
- **Differential parity against live C `lua55_o2`**:
  - Pure-Go stdlib kernels (`internal/luastdlib`, **96.9% test coverage**): patterns, `string.format`, `string.pack`/`unpack`, `utf8`, `math` RNG, number parsing.
  - **15 / 15** yield-across-native/metamethod matrix (`TestPhase2P6YieldAcrossNativeMatrix`): `table.sort`, `string.gsub`, `load` reader, `tostring`/`print`, `require`, `table.insert`/`concat`, `coroutine.close`, `pcall`/`xpcall`, `pairs`, iterators, and arithmetic/index/`<close>` metamethods.
  - Float array `table.sort` with `NaN` (`TestPhase2P5FloatSortNaNParity`).

### 1C. Multi-Configuration Matrix, Cross-OS CI & Fuzzing
- All packages pass under:
  - `default` (`opt + intnil`)
  - `-tags moeluaregcheck` (per-instruction `stackGen`, register-window, PC, and `callFrame.dirty` clean-frame verification)
  - `-tags moeluafloatnil`
  - `-tags moeluafaithful`
  - `-race` (including 8-goroutine independent `State`s sharing immutable `*bytecode.Proto` and frozen stdlib template)
  - `GOGC=1 GODEBUG=gccheckmark=1,clobberfree=1` and `-gcflags=all=-d=checkptr=2`
  - Native `CGO_ENABLED=0` builds and test matrices on Linux, macOS, and Windows (automated via `.github/workflows/ci.yml`, plus Linux `GOGC=1` + `checkptr=2` stress)
  - `TestRunIsNotBig` (`-gcflags=-m=2` verifies zero `'big'` warnings and confirms hot `Value` helpers inline in both `interp_run.go` and `interp_run_hooked.go`) + `TestGeneratedInterpFresh`
- **Fuzzing (zero Go panics; zero `recover()` inside `vm/`)**:
  - `FuzzBytecode`, `FuzzRun`, `FuzzStructuredProto` (under `-tags moeluaregcheck` verifying `Verify => zero regcheck violations/panics`), and `FuzzCompileIdentity` totaling **10.5M+** executions.

---

## 2. Benchmark Methodology

- **Primary metric**: strict single-core **`GOMAXPROCS=1` CPU time (`user + sys`)**, reported as `C_cpu / Moe_cpu` (`> 1.00×` means MoeLua is faster). Wall ratios are shown alongside CPU ratios.
- **Interleaving & spread**: **10 interleaved ABAB rounds** per benchmark (alternating `C-O2 / Moe / C-O3` and `C-O3 / Moe / C-O2`) after warmup, reporting **median ± IQR (`p75 - p25`)**. Exact stdout equivalence against C Lua `5.5.1` is asserted on every benchmark.
- **Disjoint PGO**: the PGO profile (`build/disjoint.pgo`) is trained solely on the official Lua 5.5.1 `testes/` suite — **100% disjoint** from all 13 benchmark scripts.
- **Peak RSS**: measured per benchmark from child-process `rusage.Maxrss` (`MiB`) for both C Lua and MoeLua (via `-worker` isolated subprocess).
- **C compiler flags**: verified Apple `clang 16.0.0` `-O3 -mcpu=apple-m1` against `-O3 -march=native` (`forloop_int` 386.6 vs 385.7 ms, `richards` 499.2 vs 504.3 ms, `deltablue` 65.5 vs 65.7 ms, `matmul` 70.8 vs 69.1 ms — equivalent within noise).
- **`sort_int` separation**: `sort_int` exercises a stdlib typed-array sort fast path and is excluded from the 12-benchmark Core VM geometric mean.

---

## 3. Core VM Benchmarks (`GOMAXPROCS=1`, 10-Round ABAB, Median ± IQR, Peak RSS)

### 3A. Disjoint-PGO vs No-PGO (`opt+intnil`, in-process pure VM time; RSS via subprocess)

| Benchmark | Class | C `-O2` CPU (±IQR) | C `-O3 -mcpu=apple-m1` CPU (±IQR) | Moe No-PGO CPU (±IQR) | Moe Disjoint-PGO CPU (±IQR) | Disjoint-PGO vs `-O2` (C / W) | Disjoint-PGO vs `-O3` (C) | Peak RSS C / Moe (MiB) |
|---|---|--:|--:|--:|--:|--:|--:|--:|
| `fib(34)` | (a) | 330.5 (±3.1) ms | 295.0 (±2.6) ms | 362.6 (±2.3) ms | 361.6 (±2.8) ms | 0.91× / 0.92× | 0.82× | 1.5 / 5.4 |
| `forloop_int` (50M) | (a) | 490.5 (±2.7) ms | 385.5 (±2.8) ms | 583.5 (±3.5) ms | 582.4 (±1.8) ms | 0.84× / 0.85× | 0.66× | 1.6 / 5.3 |
| `sieve` (500×5k) | (a) | 84.3 (±1.9) ms | 71.3 (±1.8) ms | 101.7 (±1.1) ms | 102.0 (±1.4) ms | 0.83× / 0.85× | 0.70× | 2.5 / 8.2 |
| `mandelbrot` (500) | (a) | 189.2 (±0.6) ms | 144.4 (±0.7) ms | 240.9 (±0.2) ms | 242.1 (±0.8) ms | 0.78× / 0.79× | 0.60× | 1.6 / 5.5 |
| `fannkuch` (9) | (a) | 124.6 (±0.7) ms | 101.7 (±2.4) ms | 151.6 (±2.0) ms | 150.2 (±2.0) ms | 0.83× / 0.84× | 0.68× | 1.6 / 5.1 |
| `richards` (20×) | (b) | 537.9 (±22.5) ms | 476.1 (±17.3) ms | 526.6 (±4.5) ms | 527.6 (±2.0) ms | **1.02× / 1.02×** | **0.90×** | 2.1 / 5.7 |
| `deltablue` (25×) | (b) | 70.0 (±5.5) ms | 66.8 (±4.0) ms | 71.1 (±1.5) ms | 71.3 (±1.2) ms | **0.98× / 1.00×** | **0.94×** | 3.1 / 10.8 |
| `nbody` (200k) | (b) | 279.4 (±6.4) ms | 231.4 (±9.7) ms | 330.4 (±4.6) ms | 328.9 (±2.5) ms | 0.85× / 0.85× | 0.70× | 1.6 / 5.3 |
| `binarytrees` (13) | (b) | 169.6 (±7.3) ms | 148.8 (±2.6) ms | 213.6 (±5.2) ms | 212.1 (±4.7) ms | 0.80× / 0.80× | 0.70× | 7.4 / 24.7 |
| `ecs_stdlib` | (b) | 71.0 (±1.8) ms | 75.0 (±4.9) ms | 63.8 (±0.9) ms | 63.5 (±0.9) ms | **1.12× / 1.13×** | **1.18×** | 2.1 / 6.0 |
| `spectralnorm` (400) | (c) | 256.1 (±2.2) ms | 202.8 (±1.5) ms | 309.1 (±3.2) ms | 306.2 (±0.7) ms | 0.84× / 0.84× | 0.66× | 1.6 / 5.4 |
| `matmul` (160²) | (c) | 96.2 (±0.8) ms | 70.0 (±1.2) ms | 106.8 (±1.1) ms | 105.3 (±0.8) ms | 0.91× / 0.92× | 0.66× | 2.3 / 6.0 |
| **Core VM Geomean (`n=12`)** | | | | *(Paired No-PGO run: 0.89× / 0.90× vs O2; 0.75× vs O3)* | | **0.89× / 0.90×** | **0.75×** | |

### 3B. Stdlib Algorithmic Fast Path (excluded from VM Geomean)

| Benchmark | C `-O2` CPU (±IQR) | C `-O3` CPU (±IQR) | Moe Disjoint-PGO CPU (±IQR) | vs `-O2` (C / W) | vs `-O3` (C) | Peak RSS C / Moe (MiB) |
|---|--:|--:|--:|--:|--:|--:|
| `sort_int` (5×50k) | 59.3 (±0.9) ms | 55.0 (±1.0) ms | 16.4 (±0.4) ms | 3.62× / 3.70× | 3.35× | 3.8 / 7.0 |

### 3C. Equal-Footing Subprocess Mode (`-subproc`, fresh OS process per round, `GOMAXPROCS=1`, 5-round ABAB)

Includes Go runtime startup and chunk load on every round, matching C's per-round OS process lifecycle:

| Benchmark | C `-O2` CPU (±IQR) | C `-O3` CPU (±IQR) | Moe Subproc CPU (±IQR) | vs `-O2` (C / W) | vs `-O3` (C) | Peak RSS C / Moe (MiB) |
|---|--:|--:|--:|--:|--:|--:|
| `fib` | 327.6 (±1.9) ms | 291.9 (±0.8) ms | 364.4 (±1.5) ms | 0.90× / 0.90× | 0.80× | 1.5 / 5.4 |
| `forloop_int` | 483.8 (±4.1) ms | 379.1 (±2.8) ms | 580.0 (±1.9) ms | 0.83× / 0.84× | 0.65× | 1.6 / 5.5 |
| `sieve` | 83.7 (±1.6) ms | 69.2 (±0.3) ms | 103.6 (±0.3) ms | 0.81× / 0.82× | 0.67× | 2.4 / 8.2 |
| `mandelbrot` | 188.0 (±0.2) ms | 143.4 (±0.2) ms | 244.3 (±0.2) ms | 0.77× / 0.77× | 0.59× | 1.5 / 5.5 |
| `fannkuch` | 122.6 (±0.4) ms | 98.4 (±1.4) ms | 151.3 (±0.5) ms | 0.81× / 0.81× | 0.65× | 1.6 / 5.5 |
| `richards` | 533.0 (±4.8) ms | 493.9 (±12.3) ms | 526.7 (±1.3) ms | **1.01× / 1.01×** | **0.94×** | 2.1 / 5.8 |
| `deltablue` | 66.0 (±1.5) ms | 65.9 (±0.3) ms | 74.0 (±1.5) ms | **0.89× / 0.89×** | **0.89×** | 2.2 / 11.1 |
| `nbody` | 271.8 (±14.5) ms | 236.0 (±15.6) ms | 329.0 (±2.0) ms | 0.83× / 0.83× | 0.72× | 1.8 / 5.5 |
| `binarytrees` | 170.5 (±3.8) ms | 148.0 (±4.1) ms | 212.1 (±3.4) ms | 0.80× / 0.80× | 0.70× | 7.5 / 24.4 |
| `ecs_stdlib` | 70.8 (±2.0) ms | 72.9 (±1.0) ms | 65.2 (±1.2) ms | **1.09× / 1.09×** | **1.12×** | 2.0 / 6.1 |
| `spectralnorm` | 256.7 (±0.7) ms | 203.1 (±1.5) ms | 312.6 (±3.3) ms | 0.82× / 0.82× | 0.65× | 1.6 / 5.6 |
| `matmul` | 96.5 (±0.4) ms | 70.2 (±0.3) ms | 108.5 (±0.7) ms | 0.89× / 0.89× | 0.65× | 2.3 / 6.1 |
| **Core VM Geomean (`n=12`)** | | | | **0.87× / 0.87×** | **0.74×** | |
| `sort_int` *(separate)* | 59.3 (±1.1) ms | 55.6 (±0.6) ms | 19.6 (±0.5) ms | 3.03× / 2.93× | 2.84× | 3.4 / 7.3 |

### 3D. Tier 0 vs Tier 1 Proportional GC Overhead (`GOMAXPROCS=1`, No-PGO, 5-round ABAB)

Tier 1 pre-activated via `setmetatable({}, {__mode="k"})` before each run (`-tier1`):

| Benchmark | Tier 0 CPU (ms) | Tier 1 CPU (ms) | Tier 1 Overhead | Tier 1 vs C `-O2` (C / W) |
|---|--:|--:|--:|--:|
| `forloop_int` | 583.5 | 574.5 | **~0.0%** | 0.84× / 0.84× |
| `richards` | 526.6 | 523.5 | **~0.0%** | 1.03× / 1.04× |
| `ecs_stdlib` | 63.8 | 63.6 | **~0.0%** | 1.18× / 1.19× |
| `spectralnorm` | 309.1 | 311.9 | **+0.9%** | 0.83× / 0.84× |
| `deltablue` | 71.1 | 72.3 | **+1.7%** | 0.93× / 0.94× |
| `binarytrees` | 213.6 | 238.7 | **+11.8%** | 0.69× / 0.70× |
| **Core VM Geomean (`n=12`)** | **0.89×** | **0.88×** | **~1% geomean** | **0.88× / 0.89×** |

### 3E. Memory & `GODEBUG=gctrace=1` Notes

- Packing pointer-bearing fields first and moving hash storage into a lazy `*tableHash` shrank `vm.Table` from **240 bytes (`ptrdata = 224B`) to 112 bytes (`ptrdata = 80B`)**, and adding dense `[]bool` (`arrBool`) eliminated 16-byte pointer-bearing `[]Value` scans on boolean arrays:
  - `binarytrees` Peak RSS dropped from **44.3 MiB → 24.7 MiB** (-44%), with runtime improving **0.62× → 0.80×** vs C `-O2`.
  - `sieve` Peak RSS dropped from **10.1 MiB → 8.2 MiB**, with runtime improving **0.75× → 0.83×** vs C `-O2`.
  - `ecs_stdlib` Peak RSS dropped from **10.3 MiB → 6.0 MiB** (-42%), with runtime improving **0.81× → 1.12×** vs C `-O2` (**1.18×** vs C `-O3 -mcpu=apple-m1`).

### 3F. Go-Host Embedding Microbenchmarks (`GOMAXPROCS=1`, `go test -bench=. -benchmem -count=5 ./bench`)

| Benchmark | Median Time | Bytes / op | Allocs / op | Notes |
|---|--:|--:|--:|---|
| `BenchmarkNewState` | **4.46 µs** | 25,296 B | **4** | Full Lua 5.5.1 stdlib cloned from frozen template (down from 34.5 µs / 752 allocs uncached) |
| `BenchmarkGoCallLua` | **76.3 ns** | 48 B | **2** | Full `Thread` / `nCcalls` / `nny` boundary bookkeeping |
| `BenchmarkLuaCallGoHost1M` | **34.02 ms** (~34.0 ns/call) | 16 B/call | 1/call | Standard multi-return `NativeFunc.Fn` / `RegisterFunc` |
| `BenchmarkLuaCallGoHostFast1M` | **17.91 ms** (~17.9 ns/call) | **32 B total** | **2 total (0/call)** | Zero-allocation single-return `NativeFunc.FastFn` / `RegisterFunc1` |
