# MoeLua

A very fast pure-Go Lua 5.5 compiler, bytecode dumper/verifier, virtual machine, standard library, and standalone CLI.

MoeLua compiles with `CGO_ENABLED=0`, has **zero third-party dependencies**, uses **zero assembly and zero JIT**, and passes the official PUC-Rio Lua 5.5.1 conformance suite (`testes/`, `main.lua`, and byte-for-byte `luac` binary chunk identity). Across the 27-program standardized cross-engine suite (`Are We Fast Yet`, `Benchmarks Game`, `SciMark 2.0a`, and Go engines' own benchmarks), MoeLua is the fastest pure-Go Lua interpreter (**1.30×** C Lua 5.5.1 `-O2` on Apple M1 Max, 1.45×–7.3× faster than other pure-Go interpreters), while keeping Go GC pauses around **1 ms** across 256 active VMs (**62×–210×** faster Go GC than Go-heap Lua VMs; see [`docs/benchmarks.md`](docs/benchmarks.md)).

## Performance

![MoeLua 5.5 vs Go Lua Engines and C Lua 5.5.1](docs/assets/bench-compare.svg)

This chart is automatically regenerated on every merge to `main` by GitHub Actions ([`bench-compare.yml`](.github/workflows/bench-compare.yml)) using the standalone [`bench/compare`](bench/compare) nested module, where every engine (including `Apogee JIT`) runs identical Lua 5.1-compatible source and is verified byte-for-byte against C Lua 5.5.1. Full per-program tables, `LuaJIT` baselines, Peak RSS, embedding microbenchmarks (`NewState` 4.8 µs, `Call1` 39 ns / 0 allocs, `RegisterFunc1` ~30 ns / 0 allocs), and Apple M1 Max reference numbers are in [`docs/benchmarks.md`](docs/benchmarks.md).

## Why MoeLua

- **Pure Go, zero dependencies**: builds with `CGO_ENABLED=0`, cross-compiles cleanly to Linux, macOS, and Windows, works with `pprof` and `-race`, and avoids CGo thread pinning.
- **Noscan Arena & true Lua 5.5.1 GC**: all Lua values (16-byte tagged `Value`, `ptrdata == 0`), tables, shapes, inline caches, strings, closures, upvalues, threads, and frames live in per-`State` noscan `[]byte` slabs managed by a PUC-Rio Lua 5.5.1 incremental + generational GC (`__gc`, weak tables, ephemerons, resurrection). Go's GC performs zero pointer scanning inside Lua heaps — on Apple M1 Max, **1,000 idle VMs take 51.6 MiB total and 0.75 ms Go GC**; **256 active VMs (524K tables) take 0.84 ms Go GC**.
- **Exact Lua 5.5.1 compatibility**: `global`, `<const>`, `<close>`, named/hidden varargs, stackful yieldable coroutines (across metamethods, `pcall`/`xpcall`, iterators, and `<close>`), full standard library (`base`, `math`, `table`, `string`, `utf8`, `coroutine`, `debug`, `io`, `os`, `package`), and `luac 5.5.1` binary chunk format parity.
- **Built for safe host embedding**: cooperative `context.Context` cancellation and deterministic instruction budgets (uncatchable by Lua `pcall`/`xpcall`, zero overhead on the hot loop when unset), per-`State` memory limits, `io/fs.FS` / `embed.FS` module loading, zero-allocation `Call1` / `RegisterFunc1`, and immutable `*moelua.Chunk` sharing across independent goroutines.

## Installation

### Standalone CLI

```sh
go install github.com/yuchanns/moelua/cmd/moelua@latest
moelua -e 'print(_VERSION, 6 * 7)'
```

Supports `-e`, `-l`, `-i`, `-v`, `-W`, `-E`, `--`, `-`, `LUA_INIT_5_5`, `LUA_PATH_5_5`, interactive REPL, and `SIGINT` handling.

### Go Library

```sh
go get github.com/yuchanns/moelua
```

Requires Go 1.27 or later. Each `*moelua.State` is single-goroutine and lock-free; compile once with `moelua.Compile` and load the immutable `*moelua.Chunk` into independent states across goroutines.

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

	// Zero-allocation single-return Go callback:
	s.RegisterFunc1("add", func(s *moelua.State, args []moelua.Value) (moelua.Value, error) {
		a, _ := args[0].ToInt()
		b, _ := args[1].ToInt()
		return moelua.Int(a + b), nil
	})

	chunk, err := moelua.Compile("=example", `return add(20, 22)`)
	if err != nil {
		panic(err)
	}
	fn, err := s.Load(chunk)
	if err != nil {
		panic(err)
	}
	out, err := s.Call1(fn)
	if err != nil {
		panic(err)
	}
	fmt.Println(out.Int()) // 42
}
```

## Testing & Benchmarks

```sh
# Pure-Go test suite across all build configurations:
go test ./...

# Full official Lua 5.5.1 conformance & differential suites (uses pinned third_party/lua submodule):
git submodule update --init
make tools
make test

# Cross-engine benchmark comparison (isolated nested module in bench/compare/):
make test-compare
make bench-compare
```

## Acknowledgements

MoeLua is written independently in pure Go and draws design inspiration, specifications, or benchmark baselines from the following projects:

- [moejs](https://github.com/MoeForge/moejs): inspiration for the project name, pure-Go no-assembly VM engineering direction, frozen shared stdlib snapshots, and Go embedding design.
- [PUC-Rio Lua 5.5.1](https://www.lua.org/): the Lua 5.5.1 language specification, bytecode format, incremental/generational collector (`lgc.c`), reference C implementation (`third_party/lua`), and official conformance test suite (`testes/`).
- [V8](https://v8.dev/) and [QuickJS](https://bellard.org/quickjs/): hidden classes (Shapes), inline caches, prototype-epoch invalidation, and compact 16-byte tagged values.
- [modernc.org/lua](https://pkg.go.dev/modernc.org/lua): the cross-engine comparison harness adapted in [`bench/compare`](bench/compare) (BSD-3-Clause), along with the standardized suites it bundles — [Are We Fast Yet](https://github.com/smarr/are-we-fast-yet), [The Computer Language Benchmarks Game](https://benchmarksgame-team.pages.debian.net/benchmarksgame/), and Mike Pall's Lua SciMark 2.0a.
- Pure-Go Lua ecosystem projects included as baselines in `bench/compare`: [apogee](https://github.com/matjam/apogee), [modernc.org/lua](https://pkg.go.dev/modernc.org/lua), [arnodel/golua](https://github.com/arnodel/golua), [yuin/gopher-lua](https://github.com/yuin/gopher-lua), [Shopify/go-lua](https://github.com/Shopify/go-lua), and [milochristiansen/lua](https://github.com/milochristiansen/lua).
