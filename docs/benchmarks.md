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
