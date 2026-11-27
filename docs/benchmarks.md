# MoeLua 5.5 — Verification & Benchmark Report

- **Target Language**: Official PUC-Rio Lua **5.5.1** only
- **Implementation**: Pure Go (`CGO_ENABLED=0`), zero assembly, zero JIT, per-`State` noscan `[]byte` Arena (`ptrdata == 0`) + true PUC-Rio Lua 5.5.1 incremental/generational GC + generational `GoRefs` host bridge
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
  - Pure-Go stdlib kernels (`internal/luastdlib`): patterns, `string.format`, `string.pack`/`unpack`, `utf8`, `math` RNG, number parsing.
  - **15 / 15** yield-across-native/metamethod matrix (`TestPhase2P6YieldAcrossNativeMatrix`): `table.sort`, `string.gsub`, `load` reader, `tostring`/`print`, `require`, `table.insert`/`concat`, `coroutine.close`, `pcall`/`xpcall`, `pairs`, iterators, and arithmetic/index/`<close>` metamethods.
  - Float array `table.sort` with `NaN` (`TestPhase2P5FloatSortNaNParity`).

### 1C. Phase 4 Noscan Arena, Per-State GC & Multi-Configuration Matrix
- **Zero-Pointer Arena Guarantee (`TestPhase4NoscanArenaStructZeroPointers`)**: verified by reflection across all 17 Arena VM types (`Value`, `LuaString`, `Table`, `tableHash`, `hashNode`, `Shape`, `shapeTrans`, `shapeIndexEntry`, `ICEntry`, `polyEntry`, `Thread`, `callFrame`, `Closure`, `Upvalue`, `ArenaNative`, `Userdata`, `ArenaProto`) — zero Go pointers inside any Lua VM heap slab.
- **True Per-State Lua 5.5.1 Collector (`internal/arena/gc.go`, `vm/gc.go`)**: unified PUC-Rio `lgc.c` incremental tri-color + generational minor/major GC with exact 6-step `atomic()`, ephemeron convergence, LIFO `__gc` finalizers, resurrection, weak-table pruning, and automatic `GoRefs` release on sweep — always active with no Tier 0 / Tier 1 split.
- **All packages pass under**:
  - `default` (`opt + intnil`)
  - `-tags moeluaregcheck` (per-instruction `stackGen`, register-window, PC, and `callFrame.dirty` clean-frame verification)
  - `-tags moeluafloatnil`
  - `-tags moeluafaithful`
  - `-race` (including concurrent independent `State`s sharing immutable `*bytecode.Proto`, atomic copy-on-write static native registry, and frozen stdlib string arena)
  - `GOGC=1 GODEBUG=gccheckmark=1,clobberfree=1` and `-gcflags=all=-d=checkptr=2`
  - Native `CGO_ENABLED=0` builds and test matrices on Linux, macOS, and Windows (automated via `.github/workflows/ci.yml`, plus Linux `GOGC=1` + `checkptr=2` stress)
  - `TestRunIsNotBig` (`-gcflags=-m=2` verifies zero `'big'` warnings and confirms hot helpers inline cleanly in both `interp_run.go` and `interp_run_hooked.go`) + `TestGeneratedInterpFresh`
- **Fuzzing (zero Go panics; zero `recover()` inside `vm/`)**:
  - `FuzzBytecode`, `FuzzRun`, `FuzzStructuredProto` (under `-tags moeluaregcheck`), and `FuzzCompileIdentity` totaling **10.5M+** executions.

---

## 2. Benchmark Methodology

- **Primary metric**: strict single-core **`GOMAXPROCS=1` CPU time (`user + sys`)**, reported as `C_cpu / Moe_cpu` (`> 1.00×` means MoeLua is faster). Wall ratios are shown alongside CPU ratios.
- **Interleaving & spread**: **10 interleaved ABAB rounds** per benchmark (alternating `C-O2 / Moe / C-O3` and `C-O3 / Moe / C-O2`) after warmup, reporting **median ± IQR (`p75 - p25`)**. Exact stdout equivalence against C Lua `5.5.1` is asserted on every benchmark.
- **Disjoint PGO**: the PGO profile (`build/disjoint.pgo`) is trained solely on the official Lua 5.5.1 `testes/` suite — **100% disjoint** from all 13 benchmark scripts.
- **Peak RSS**: measured per benchmark from child-process `rusage.Maxrss` (`MiB`) for both C Lua and MoeLua (via `-worker` isolated subprocess).
- **`sort_int` separation**: `sort_int` exercises a stdlib typed-array sort fast path and is excluded from the 12-benchmark Core VM geometric mean.
- **Multi-VM Go GC & footprint**: measured on live `vm.State` pools (`256` and `1,000` simultaneously live VMs) via `bench/multivm_bench_test.go` and `internal/arena/arena_test.go`.

---

## 3. Core VM Benchmarks (`GOMAXPROCS=1`, 10-Round ABAB, Median ± IQR, Peak RSS)

### 3A. No-PGO vs Disjoint-PGO (`opt+intnil`, unified Arena GC always active, in-process pure VM time; RSS via subprocess)

| Benchmark | Class | C `-O2` CPU (±IQR) | C `-O3 -mcpu=apple-m1` CPU (±IQR) | Moe No-PGO CPU (±IQR) | Moe Disjoint-PGO CPU (±IQR) | No-PGO vs `-O2` (C / W) | No-PGO vs `-O3` (C) | Peak RSS C / Moe (MiB) |
|---|---|--:|--:|--:|--:|--:|--:|--:|
| `fib(34)` | (a) | 335.5 (±5.5) ms | 299.2 (±4.9) ms | 406.8 (±2.9) ms | 406.1 (±3.5) ms | 0.82× / 0.83× | 0.74× | 1.5 / 6.3 |
| `forloop_int` (50M) | (a) | 493.9 (±2.9) ms | 389.0 (±0.4) ms | 508.5 (±1.8) ms | 509.0 (±3.3) ms | **0.97× / 0.98×** | **0.76×** | 1.6 / 6.0 |
| `sieve` (500×5k) | (a) | 85.6 (±1.7) ms | 72.6 (±2.9) ms | 106.9 (±1.6) ms | 106.7 (±1.9) ms | 0.80× / 0.82× | 0.68× | 2.1 / 6.3 |
| `mandelbrot` (500) | (a) | 197.1 (±2.2) ms | 149.4 (±2.3) ms | 222.1 (±1.8) ms | 221.8 (±4.3) ms | **0.89× / 0.90×** | 0.67× | 1.6 / 6.1 |
| `fannkuch` (9) | (a) | 126.5 (±2.5) ms | 104.4 (±4.0) ms | 150.1 (±2.1) ms | 150.5 (±5.5) ms | 0.84× / 0.86× | 0.70× | 1.5 / 5.9 |
| `richards` (20×) | (b) | 541.1 (±35.8) ms | 503.7 (±16.6) ms | 652.9 (±3.2) ms | 655.3 (±4.1) ms | 0.83× / 0.84× | **0.77×** | 2.0 / 6.5 |
| `deltablue` (25×) | (b) | 68.6 (±4.0) ms | 65.3 (±1.1) ms | 69.8 (±0.8) ms | 70.2 (±1.3) ms | **0.98× / 1.01×** | **0.94×** | 2.4 / 10.5 |
| `nbody` (200k) | (b) | 283.4 (±6.5) ms | 236.6 (±19.6) ms | 357.7 (±2.7) ms | 355.3 (±3.8) ms | 0.79× / 0.80× | 0.66× | 1.6 / 6.2 |
| `binarytrees` (13) | (b) | 174.8 (±6.0) ms | 152.6 (±3.7) ms | 200.5 (±3.5) ms | 202.5 (±1.8) ms | **0.87× / 0.88×** | **0.76×** | 7.5 / **18.1** |
| `ecs_stdlib` | (b) | 77.4 (±7.5) ms | 74.1 (±2.7) ms | 79.4 (±0.6) ms | 79.9 (±0.9) ms | **0.97× / 0.99×** | **0.93×** | 2.1 / 6.8 |
| `spectralnorm` (400) | (c) | 260.7 (±1.7) ms | 209.0 (±5.0) ms | 335.1 (±5.0) ms | 335.5 (±3.2) ms | 0.78× / 0.79× | 0.62× | 1.7 / 6.3 |
| `matmul` (160²) | (c) | 96.9 (±1.3) ms | 70.7 (±1.5) ms | 93.9 (±1.7) ms | 94.0 (±1.2) ms | **1.03× / 1.05×** | **0.75×** | 2.4 / 6.8 |
| **Core VM Geomean (`n=12`)** | | | | *(Disjoint-PGO paired run: **0.87× / 0.89×** vs O2; **0.74×** vs O3; `binarytrees` RSS 16.3 MiB)* | | **0.88× / 0.89×** | **0.74×** | |

### 3B. Stdlib Algorithmic Fast Path (excluded from VM Geomean)

| Benchmark | C `-O2` CPU (±IQR) | C `-O3` CPU (±IQR) | Moe No-PGO / Disjoint-PGO CPU (±IQR) | vs `-O2` (C / W) | vs `-O3` (C) | Peak RSS C / Moe (MiB) |
|---|--:|--:|--:|--:|--:|--:|
| `sort_int` (5×50k) | 60.5 (±2.0) ms | 56.5 (±1.5) ms | 16.8 (±0.8) ms / 16.8 (±0.6) ms | 3.61× / 3.70× | 3.37× | 3.9 / 8.0 |

### 3C. Equal-Footing Subprocess Mode (`-subproc`, fresh OS process per round, `GOMAXPROCS=1`, 5-round ABAB, Disjoint-PGO)

Includes Go runtime startup and chunk load on every round, matching C's per-round OS process lifecycle:

| Benchmark | C `-O2` CPU (±IQR) | C `-O3` CPU (±IQR) | Moe Subproc CPU (±IQR) | vs `-O2` (C / W) | vs `-O3` (C) | Peak RSS C / Moe (MiB) |
|---|--:|--:|--:|--:|--:|--:|
| `fib` | 336.9 (±7.2) ms | 298.0 (±3.2) ms | 410.1 (±2.1) ms | 0.82× / 0.82× | 0.73× | 1.6 / 6.3 |
| `forloop_int` | 491.9 (±3.3) ms | 387.8 (±1.5) ms | 514.3 (±2.7) ms | **0.96× / 0.96×** | 0.75× | 1.6 / 6.1 |
| `sieve` | 84.1 (±2.2) ms | 72.3 (±1.0) ms | 111.7 (±3.0) ms | 0.75× / 0.75× | 0.65× | 1.6 / 6.3 |
| `mandelbrot` | 197.5 (±0.7) ms | 150.5 (±3.6) ms | 227.1 (±2.2) ms | 0.87× / 0.87× | 0.66× | 1.6 / 6.1 |
| `fannkuch` | 127.0 (±2.5) ms | 100.8 (±2.1) ms | 155.6 (±4.1) ms | 0.82× / 0.82× | 0.65× | 1.6 / 6.1 |
| `richards` | 529.0 (±6.4) ms | 523.8 (±23.9) ms | 663.6 (±1.6) ms | 0.80× / 0.80× | 0.79× | 2.1 / 6.6 |
| `deltablue` | 67.1 (±5.4) ms | 66.7 (±5.3) ms | 73.5 (±0.9) ms | **0.91× / 0.91×** | **0.91×** | 2.0 / 10.5 |
| `nbody` | 285.2 (±3.8) ms | 230.8 (±6.0) ms | 361.6 (±5.3) ms | 0.79× / 0.79× | 0.64× | 1.6 / 6.2 |
| `binarytrees` | 176.9 (±8.3) ms | 153.3 (±5.8) ms | 204.0 (±2.2) ms | **0.87× / 0.87×** | **0.75×** | 7.5 / **18.8** |
| `ecs_stdlib` | 73.0 (±7.0) ms | 72.9 (±2.3) ms | 81.7 (±0.5) ms | **0.89× / 0.90×** | **0.89×** | 2.0 / 6.8 |
| `spectralnorm` | 263.0 (±1.5) ms | 207.1 (±1.2) ms | 342.1 (±3.0) ms | 0.77× / 0.78× | 0.61× | 1.7 / 6.3 |
| `matmul` | 96.7 (±0.5) ms | 70.7 (±1.6) ms | 97.4 (±0.9) ms | **0.99× / 1.00×** | 0.73× | 2.4 / 6.8 |
| **Core VM Geomean (`n=12`)** | | | | **0.85× / 0.85×** | **0.72×** | |
| `sort_int` *(separate)* | 60.7 (±1.7) ms | 55.7 (±0.5) ms | 19.3 (±0.3) ms | 3.14× / 3.02× | 2.88× | 3.0 / 8.0 |

### 3D. High-Density Multi-VM Go GC Scalability & Per-VM Memory Footprint (`count=5` medians)

Because all Lua VM objects live inside per-`State` noscan `[]byte` Arenas (`ptrdata == 0`) and stdlib snapshots are compacted via `Arena.CompactSnapshot()` with progressive non-moving slab page sizing (`2 KiB → 8 KiB → 32 KiB → 64 KiB`), Go's garbage collector performs zero pointer scanning inside Lua heaps even when hundreds or thousands of independent VMs are live in one process:

| Benchmark | Live VMs | Live Lua Objects | Median `runtime.GC()` | Arena Backing / VM | Go `HeapAlloc` / VM | Notes |
|---|--:|--:|--:|--:|--:|---|
| `BenchmarkIdleVMFootprint1000` (`./bench`) | 1,000 | Full stdlib | **0.75 ms** | **42.94 KiB** | **52.84 KiB** | 51.6 MiB total Go heap for 1,000 idle States (`collectgarbage("count")` = 45.39 KiB vs C Lua 5.5.1's 19.32 KiB) |
| `BenchmarkMultiVMGoGCScan/256VMs` (`./bench`) | 256 | 418,816 (1,636/VM) | **1.07 ms** | 245.6 KiB | 262.5 KiB | Tables, nested records, dynamic strings, closures, and suspended coroutine per VM |
| `BenchmarkMultiVMGoGCScan/1000VMs` (`./bench`) | 1,000 | 1,636,000 (1,636/VM) | **3.36 ms** | 245.6 KiB | 262.5 KiB | 1.64M live Lua objects across 1,000 independent States |
| `BenchmarkMultiVMGoGCScan/NoscanArena` (`./internal/arena`) | 256 | 512,000 (2,000/VM) | **0.289 ms** | — | — | **7.9× faster Go GC** vs identical Go-heap pointer graph (`2.273 ms`) |

- **Arena pointer dereference microbenchmark (`./internal/arena`, `BenchmarkArenaPtrDeref`)**: `Ptr` **2.52 ns**, `PtrFast` **2.20 ns**, `PtrSmall` **2.21 ns**, `PtrInPages` **2.21 ns** (0 allocs).

### 3E. Memory Footprint & Phase 4 Arena GC Notes

- Moving all VM heap objects into per-`State` noscan `[]byte` slabs and replacing Phase 3's Go-heap objects (`112B` `Table` with `ptrdata = 80B`, `weak.Pointer` IC/shape barriers, and Go-GC Tier 0/1 split) with `80B` noscan `Table`, `128B` noscan `Shape`, `48B` noscan `ICEntry`, and true per-`State` Lua 5.5.1 incremental + generational GC produced major memory and consistency wins:
  - **`binarytrees` Peak RSS** dropped from **24.7 MiB → 16.3–18.1 MiB** (-27% to -34%), while runtime improved from **0.80× (Tier 0) / 0.69× (Tier 1) → 0.87× vs C `-O2`** (**0.76×** vs `-O3`) with full Lua 5.5.1 GC always enabled.
  - **`sieve` Peak RSS** dropped from **8.2 MiB → 6.3 MiB** (-23%).
  - **Fresh idle VM footprint** dropped from 12 × 64 KiB (`768 KiB` uncompacted slab backing) to **42.94 KiB** compacted Arena backing (**52.84 KiB** Go `HeapAlloc` per VM).

### 3F. Go-Host Embedding Microbenchmarks (`GOMAXPROCS=1`, `go test -bench=. -benchmem -count=5 ./bench`)

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
| Reference C | C Lua 5.5.1 `-O2` | 5.5 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | **1.000** | **1.000** |
| Reference C | C Lua 5.5.1 `-O3` | 5.5 | 0.910 | 0.863 | 0.799 | 0.881 | 1.016 | **0.871** | **0.889** |
| Native JIT | LuaJIT 2.1 (JIT) | 5.1 | 0.173 | 0.151 | 0.099 | 0.468 | 0.420 | **0.162** | **0.190** |
| Native Asm Interp | LuaJIT 2.1 (`-joff`) | 5.1 | 0.945 | 0.983 | 0.884 | 1.018 | 4.470 | **0.966** | **1.100** |
| Go + ARM64 JIT | Apogee v1.2.1 (JIT) | 5.5 | 0.912 | 0.625 | 0.249 | 0.573 | 3.414 | **0.569** | **0.741** |
| **Pure Go Interp #1** | **MoeLua** | **5.5** | **1.333** | **1.443** | **1.387** | **1.069** | **2.567** | **1.302** | **1.408** |
| Pure Go Interp #2 | Apogee v1.2.1 (`WithoutJIT`) | 5.5 | 1.905 | 1.888 | 2.038 | 1.720 | 4.093 | **1.882** | **2.039** |
| Pure Go (ccgo) #3 | `modernc.org/lua` v0.2.0 (`CCGO`) | 5.5 | 2.596 | 2.717 | 2.858 | 2.687 | 3.560 | **2.731** | **2.750** |
| Pure Go Interp #4 | `arnodel/golua` (`default.pgo`) | 5.5 | 2.970 | 2.659 | 2.403 | 2.721 | 4.930 | **2.757** | **2.919** |
| Pure Go Interp #5 | `yuin/gopher-lua` | 5.1 | 5.174 | 4.905 | 5.862 | 3.711 | 8.965 | **5.023** | **5.276** |
| Pure Go Interp #6 | `Shopify/go-lua` | 5.2 | 7.508* | 5.654 | 6.049 | 5.020 | 6.036 | **6.546** | **6.380** |
| Pure Go Interp #7 | `milochristiansen/lua` (`DCLua`) | 5.3 | 9.061 | 9.096* | 9.716 | 10.856 | 7.807 | **9.558** | **9.251** |

*\*Skipped per `bench/compare` defaults: `Shopify/go-lua` on `awfy/Havlak` (too slow); `DCLua` on `game/binarytrees` (wrong output).*

### 4B. Per-Program Ratios (Relative to C Lua 5.5.1 `-O2`, Median of 3 Rounds)

| Suite / Program | C `-O2` | C `-O3` | **MoeLua** | Apogee-JIT | Apogee-noJIT | CCGO | golua | GopherLua | go-lua | DCLua |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `awfy/Bounce` | 1.000 | 0.971 | **1.189** | 0.892 | 1.982 | 2.954 | 3.303 | 5.857 | 8.279 | 11.442 |
| `awfy/CD` | 1.000 | 0.931 | **1.494** | 3.075 | 2.189 | 2.365 | 4.087 | 5.979 | 7.098 | 7.624 |
| `awfy/DeltaBlue` | 1.000 | 0.977 | **1.269** | 2.138 | 1.986 | 2.324 | 3.952 | 5.157 | 78.561 | 8.092 |
| `awfy/Havlak` | 1.000 | 0.965 | **1.473** | 1.719 | 1.783 | 2.073 | 3.109 | 6.378 | n/a | 6.375 |
| `awfy/Json` | 1.000 | 0.937 | **1.727** | 2.447 | 2.284 | 2.499 | 3.778 | 5.111 | 6.217 | 8.217 |
| `awfy/List` | 1.000 | 0.873 | **1.279** | 0.717 | 1.704 | 2.790 | 2.522 | 3.829 | 4.098 | 9.865 |
| `awfy/Mandelbrot` | 1.000 | 0.802 | **1.232** | 0.171 | 1.561 | 2.982 | 2.319 | 5.427 | 6.011 | 10.567 |
| `awfy/NBody` | 1.000 | 0.789 | **1.149** | 0.411 | 1.699 | 3.393 | 2.476 | 7.561 | 9.512 | 13.525 |
| `awfy/Permute` | 1.000 | 0.934 | **1.553** | 0.549 | 2.075 | 2.896 | 2.780 | 4.888 | 6.293 | 11.317 |
| `awfy/Queens` | 1.000 | 0.893 | **1.515** | 0.661 | 1.788 | 2.352 | 2.499 | 3.633 | 4.734 | 8.799 |
| `awfy/Richards` | 1.000 | 0.938 | **1.306** | 0.684 | 1.441 | 2.399 | 3.267 | 4.461 | 5.231 | 8.613 |
| `awfy/Sieve` | 1.000 | 0.857 | **1.286** | 0.639 | 3.362 | 2.944 | 3.184 | 6.503 | 6.481 | 10.042 |
| `awfy/Storage` | 1.000 | 0.963 | **0.845** | 1.991 | 1.643 | 2.041 | 2.183 | 4.700 | 5.042 | 5.211 |
| `awfy/Towers` | 1.000 | 0.942 | **1.613** | 0.704 | 1.772 | 2.714 | 2.905 | 4.393 | 7.035 | 10.740 |
| `game/binarytrees` | 1.000 | 0.930 | **1.844** | 3.974 | 2.496 | 2.014 | 2.654 | 3.334 | 3.762 | n/a |
| `game/fannkuchredux` | 1.000 | 0.807 | **1.251** | 0.377 | 2.249 | 2.687 | 2.062 | 3.892 | 4.426 | 6.390 |
| `game/fasta` | 1.000 | 0.958 | **1.556** | 2.165 | 1.840 | 2.573 | 3.760 | 4.903 | 5.555 | 6.822 |
| `game/mandelbrot` | 1.000 | 0.891 | **1.548** | 0.135 | 1.544 | 2.953 | 2.480 | 6.038 | 6.663 | 12.340 |
| `game/nbody` | 1.000 | 0.830 | **1.213** | 0.491 | 1.650 | 3.440 | 2.490 | 7.639 | 9.604 | 13.961 |
| `game/spectralnorm` | 1.000 | 0.779 | **1.341** | 0.277 | 1.723 | 2.841 | 2.781 | 4.747 | 5.521 | 8.291 |
| `scimark/FFT` | 1.000 | 0.824 | **1.352** | 0.197 | 1.912 | 2.923 | 2.341 | 6.851 | 6.547 | 11.081 |
| `scimark/LU` | 1.000 | 0.701 | **1.241** | 0.124 | 2.086 | 2.733 | 2.118 | 6.259 | 6.251 | 8.905 |
| `scimark/MC` | 1.000 | 0.831 | **1.578** | 0.655 | 1.818 | 2.386 | 2.872 | 4.849 | 4.018 | 7.957 |
| `scimark/SOR` | 1.000 | 0.850 | **1.216** | 0.202 | 2.021 | 2.975 | 2.227 | 4.829 | 6.607 | 9.228 |
| `scimark/SPARSE` | 1.000 | 0.799 | **1.597** | 0.297 | 2.401 | 3.364 | 2.528 | 6.892 | 7.457 | 11.948 |
| `own/fib35` | 1.000 | 0.928 | **1.268** | 0.709 | 2.025 | 2.462 | 3.592 | 3.844 | 4.090 | 7.688 |
| `own/fibonacci` | 1.000 | 0.762 | **1.157** | 0.104 | 2.161 | 4.162 | 2.750 | 7.186 | 7.909 | 13.323 |
| `own/sort` | 1.000 | 0.927 | **0.451** | 1.064 | 1.148 | 1.930 | 2.077 | 1.655 | 2.732 | 14.105 |
| `own/sort2` | 1.000 | 0.918 | **1.977** | 1.369 | 1.742 | 2.638 | 2.672 | 4.148 | 7.184 | 9.613 |
| `micro/call` | 1.000 | 1.003 | **1.885** | 0.822 | 1.814 | 2.608 | 2.626 | 5.077 | 4.610 | 8.435 |
| `micro/error` | 1.000 | 1.023 | **2.803** | 6.175 | 5.735 | 5.255 | 7.823 | 29.668 | 5.220 | 7.370 |
| `micro/pcall` | 1.000 | 1.022 | **3.199** | 7.842 | 6.593 | 3.291 | 5.832 | 4.784 | 9.137 | 7.655 |

### 4C. Isolated-Process High-Density Multi-VM Memory & Go GC Comparison (`cd bench/compare && go run memcmp.go`)

Each engine runs in its own isolated OS subprocess (`GOMAXPROCS=1`): 256 idle full-stdlib VMs (measuring average `NewState` cold creation, heap, OS RSS, and Go GC), followed by scaling active VMs from `32 → 128 → 256 → 512` with a 2,047-table binary tree inside every VM (up to **1,048,064 live Lua tables** at 512 VMs).

| Engine | `NewState` (256 live) | Idle Heap / VM | Idle RSS / VM | 256-Idle GC | Active RSS / VM (256) | 32-Act GC | 128-Act GC | 256-Act GC | **512-Act GC (1.05M tables)** |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| **MoeLua** | **31.2 µs** | **52.9 KiB** | **53.8 KiB** | **0.64 ms** | **328.8 KiB** | **0.27 ms** | **0.57 ms** | **0.98 ms** | **1.95 ms** |
| `modernc.org/lua` (`CCGO`) | 32.9 µs | 1.6 KiB *(off-heap mmap)* | 31.9 KiB | 0.23 ms | 275.5 KiB | 0.17 ms | 0.20 ms | 0.23 ms | 0.30 ms |
| Apogee v1.2.1 (`WithoutJIT`) | 182.7 µs | 174.6 KiB | 192.8 KiB | 20.34 ms | 513.9 KiB | 6.05 ms | 26.83 ms | 55.29 ms | 110.80 ms |
| `arnodel/golua` | 62.7 µs | 113.7 KiB | 62.1 KiB | 3.60 ms | 631.3 KiB | 5.87 ms | 26.01 ms | 54.46 ms | 110.99 ms |
| Apogee v1.2.1 (JIT) | 213.6 µs | 174.6 KiB | 191.8 KiB | 20.35 ms | 563.5 KiB | 6.10 ms | 27.47 ms | 56.21 ms | 112.44 ms |
| `milochristiansen/lua` (`DCLua`) | 41.6 µs | 66.5 KiB | 79.9 KiB | 4.03 ms | 1,066.9 KiB | 8.01 ms | 39.38 ms | 75.93 ms | 154.30 ms |
| `Shopify/go-lua` | 32.3 µs | 17.2 KiB | 24.3 KiB | 2.49 ms | 1,023.2 KiB | 9.43 ms | 42.50 ms | 87.01 ms | 175.59 ms |
| `yuin/gopher-lua` | 63.3 µs | 162.4 KiB | 178.4 KiB | 10.75 ms | 2,012.5 KiB | 20.60 ms | 89.46 ms | 181.30 ms | 364.92 ms |

---

## 5. Trust Model & Resource Limits

- **Binary chunks are trusted input**: `bytecode.Undump` and `bytecode.Verify` provide panic-freedom defense in depth (nesting depth caps, CFG successor validation, register/constant/upvalue bounds), matching PUC-Rio Lua's trust model rather than acting as an untrusted-bytecode sandbox.
- **Per-State memory limit (`WithMemoryLimit`)**: accounts Lua tables, shapes, inline caches, strings, closures, threads, and stack/frame growth (including Arena size-class rounding) with emergency GC recount (skipping `__gc` finalizers mid-allocation) and raises recoverable `"not enough memory"` errors; it is a VM-level resource guard rather than an OS RSS cgroup.
- **Cooperative cancellation & step budgets (`WithContext`, `WithInstructionLimit`)**: checked inside `runHooked` (zero overhead in the default fast loop when unset, including non-cancelable `context.Background()`), at call boundaries, and periodically inside pattern matching (`string.find`/`match`/`gmatch`/`gsub`). Treated as uncatchable by Lua `pcall`/`xpcall`, `coroutine.resume`/`wrap`, `load`, `<close>`, and `__gc`; hard cancellation unwinds frames and discards pending Lua `<close>` metamethods above the aborted boundary without running arbitrary Lua code. Blocking Go host callbacks or OS I/O remain the caller's responsibility.
- **Zero `recover()` in `vm/`**: Lua errors propagate strictly via Go error return values; runtime panics are never masked as Lua errors.
