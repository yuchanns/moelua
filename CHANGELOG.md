# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## 0.1.1 - 2026-11-27

## What's Changed
* chore(bench): add native LuaJIT 2.1 multi-VM RSS & cold-start reference by @yuchanns in https://github.com/yuchanns/moelua/pull/37
* chore: update CLI version banner and copyright headers to MoeLua Contributors by @yuchanns in https://github.com/yuchanns/moelua/pull/38


**Full Changelog**: https://github.com/yuchanns/moelua/compare/v0.1.0...v0.1.1

## 0.1.0 - 2026-11-27

## What's Changed
* feat(bytecode): Lua 5.5.1 bytecode format, disassembler, verifier, and dump/undump by @yuchanns in https://github.com/yuchanns/moelua/pull/1
* feat(compiler): pure-Go Lua 5.5.1 lexer, parser, and bytecode generator by @yuchanns in https://github.com/yuchanns/moelua/pull/2
* feat(stdlib): pure-Go Lua 5.5.1 pattern matching, format, pack, utf8, and strnum kernels by @yuchanns in https://github.com/yuchanns/moelua/pull/3
* feat(vm): pure-Go Lua 5.5.1 VM runtime, GC, interpreter, stdlib natives, and debug library by @yuchanns in https://github.com/yuchanns/moelua/pull/4
* test(vm): official Lua 5.5.1 testes harness, conformance suites, audit regressions, and fuzzers by @yuchanns in https://github.com/yuchanns/moelua/pull/5
* feat(api,cli): Go embedding API and standalone Lua 5.5.1 CLI by @yuchanns in https://github.com/yuchanns/moelua/pull/6
* bench,docs: benchmark suite, benchcmp runner, README, and performance report by @yuchanns in https://github.com/yuchanns/moelua/pull/7
* feat(arena): noscan []byte slab arena allocator, GCHeader, and generational GoRefs table by @yuchanns in https://github.com/yuchanns/moelua/pull/9
* feat(arena): per-State Lua 5.5 incremental tri-color and generational GC engine by @yuchanns in https://github.com/yuchanns/moelua/pull/10
* feat(vm): noscan Arena LuaString/interner and high-water State.Call / zero-alloc Call1 by @yuchanns in https://github.com/yuchanns/moelua/pull/12
* feat(arena,vm): add fast slab pointer accessors, Collector hooks, and State-scoped Value/error API by @yuchanns in https://github.com/yuchanns/moelua/pull/13
* feat(arena,vm): Phase 4 Step 5a — Collector snapshot/cancel hardening and State-scoped Value API by @yuchanns in https://github.com/yuchanns/moelua/pull/14
* refactor(vm): Phase 4 Step 5b — inlineable Arena-ready accessor layer across interpreter, debug, and stdlib by @yuchanns in https://github.com/yuchanns/moelua/pull/15
* feat(vm): switch Value, GC collector, and core objects to noscan Arena (Phase 4 Step 5c) by @yuchanns in https://github.com/yuchanns/moelua/pull/16
* feat(vm): migrate Thread, callFrame, and coroutine stacks into noscan Arena (Phase 4 Step 5d) by @yuchanns in https://github.com/yuchanns/moelua/pull/17
* feat(vm): migrate Table, Shape, and inline caches to noscan Arena with zero-relocation snapshot cloning by @yuchanns in https://github.com/yuchanns/moelua/pull/18
* feat(arena,vm): add CompactSnapshot, progressive slab sizing, hot-path inlining, and Phase 4 benchmarks by @yuchanns in https://github.com/yuchanns/moelua/pull/19
* perf(vm): proxy-metatable shape sharing, fast sort/SETTABUP, bench/compare module & README by @yuchanns in https://github.com/yuchanns/moelua/pull/20
* ci: reusable composite actions, plan-based PR benchmarks, and tag-only release workflow by @yuchanns in https://github.com/yuchanns/moelua/pull/21
* perf(vm): add Plan 9 ARM64 direct-threaded bytecode interpreter by @yuchanns in https://github.com/yuchanns/moelua/pull/22
* perf(vm): add Plan 9 AMD64 direct-threaded bytecode interpreter by @yuchanns in https://github.com/yuchanns/moelua/pull/23
* feat(jit): add W^X execmem allocator, Go-to-JIT trampolines, and WithJIT API by @yuchanns in https://github.com/yuchanns/moelua/pull/25
* feat(jit): add ARM64 and AMD64 machine-code assemblers and JIT layout invariants by @yuchanns in https://github.com/yuchanns/moelua/pull/27
* feat(jit): ARM64 & AMD64 baseline JIT compilers and runtime execution (Phase 5B PR 3/5) by @yuchanns in https://github.com/yuchanns/moelua/pull/28
* jit(opt): typed SSA loop optimizer, Tier-2 feedback, and NEON/SSE2 SIMD (4/5) by @yuchanns in https://github.com/yuchanns/moelua/pull/30
* feat(jit): shared Tier-1 code cache, cross-engine JIT benchmarks, and docs by @yuchanns in https://github.com/yuchanns/moelua/pull/31
* feat(bench): include LuaJIT 2.1 reference bar in comparison chart and workflow by @yuchanns in https://github.com/yuchanns/moelua/pull/32
* chore: add Apache-2.0 LICENSE by @yuchanns in https://github.com/yuchanns/moelua/pull/33
* ci(release): fix embedded NUL byte in release.yml git log format by @yuchanns in https://github.com/yuchanns/moelua/pull/34
* chore(bench): align bench-compare GC panel with highest-scale 1,000-VM count by @yuchanns in https://github.com/yuchanns/moelua/pull/36
* feat(api): add selective stdlib loading, lazy preloading, and custom Template snapshots by @yuchanns in https://github.com/yuchanns/moelua/pull/35

## New Contributors
* @yuchanns made their first contribution in https://github.com/yuchanns/moelua/pull/1

**Full Changelog**: https://github.com/yuchanns/moelua/commits/v0.1.0
