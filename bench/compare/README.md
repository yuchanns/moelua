# Cross-Engine Comparison Suite (`bench/compare`)

This directory is a standalone nested Go module (`github.com/yuchanns/moelua/bench/compare`) so that third-party Lua engine dependencies stay completely isolated from MoeLua's zero-dependency root `go.mod`.

The harness and standardized 32-program suite are adapted from [`modernc.org/lua/compare`](https://pkg.go.dev/modernc.org/lua) (BSD-3-Clause) and bundle:
- **Are We Fast Yet** (14 Lua programs, MIT — `testdata/awfy/LICENSE.md`)
- **The Computer Language Benchmarks Game** (6 Lua programs, BSD-3-Clause — `testdata/game/LICENSE.md`)
- **Lua SciMark 2.0a** (5 kernels — `testdata/scimark/scimark.lua`)
- **Go engines' own benchmarks** (4 programs: `fib35`, `fibonacci`, `sort`, `sort2`)
- **Microbenchmarks** (3 programs: `call`, `pcall`, `error`)

All 32 programs run identical Lua 5.1-compatible source across every engine and are verified byte-for-byte against C Lua 5.5.1 via `TestOutputs`.

## Running Locally

First build the reference C Lua 5.5.1 binaries at the repo root (`git submodule update --init && make tools`):

```bash
cd bench/compare

# Verify 32/32 byte-exact output equivalence against C Lua 5.5.1:
go test -vet=off -timeout 10m -run TestOutputs -engines '^(C|MoeLua)$' .

# Run single-core cross-engine CPU benchmarks (all pure-Go engines + C -O2/-O3):
GOLUA_PGO="$(go list -m -f '{{.Dir}}' github.com/arnodel/golua)/default.pgo"
(
  GOMAXPROCS=1 go test -vet=off -timeout 60m -run @ -bench . -benchtime 300ms -count 3 \
    -engines '^(C|C-O3|MoeLua|Apogee-JIT|Apogee-Interp|CCGO|GopherLua|GoLua|DCLua)$' . &&
  GOMAXPROCS=1 go test -vet=off -timeout 30m -pgo="$GOLUA_PGO" -run @ -bench . -benchtime 300ms -count 3 \
    -engines '^ArnodelGolua$' .
) | go run benchreport.go -json bench_summary.json

# Run isolated-process high-density multi-VM memory & Go GC comparison (64/256/512/1000 VMs) across all Go engines:
go run memcmp.go -json memcmp.json

# Regenerate both README SVG charts from benchreport.go -json and memcmp.go -json outputs:
go run genchart.go -bench bench_summary.json -mem memcmp.json \
  -out ../../docs/assets/bench-compare.svg \
  -out-multivm ../../docs/assets/bench-multivm.svg
```
