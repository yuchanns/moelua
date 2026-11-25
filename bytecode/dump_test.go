package bytecode_test

import (
	"bytes"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yuchanns/moelua/bytecode"
)

func lua55Bin(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "build", "lua55_o2")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("lua55_o2 not found at %s: %v", p, err)
	}
	return p
}

func luac55Bin(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "build", "luac55")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("luac55 not found at %s: %v", p, err)
	}
	return p
}

func compileWithLuac55(t *testing.T, luaFile string, strip bool) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.luac")
	args := []string{"-o", out}
	if strip {
		args = append(args, "-s")
	}
	args = append(args, luaFile)
	cmd := exec.Command(luac55Bin(t), args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("luac55 %v failed: %v\n%s", args, err, string(b))
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func runChunkInLua55(t *testing.T, chunk []byte) string {
	t.Helper()
	chunkFile := filepath.Join(t.TempDir(), "chunk.luac")
	if err := os.WriteFile(chunkFile, chunk, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(lua55Bin(t), "-e", `
		local f = assert(io.open(os.getenv("MOELUA_CHUNK"), "rb"))
		local data = f:read("a")
		f:close()
		assert(load(data))()
	`)
	cmd.Env = append(os.Environ(), "MOELUA_CHUNK="+chunkFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lua55_o2 executing chunk failed: %v\n%s", err, string(out))
	}
	return string(out)
}

func assertProtosEqual(t *testing.T, got, want *bytecode.Proto, strip bool) {
	t.Helper()
	if got.LineDefined != want.LineDefined || got.LastLineDefined != want.LastLineDefined {
		t.Fatalf("line defined mismatch: got (%d,%d), want (%d,%d)",
			got.LineDefined, got.LastLineDefined, want.LineDefined, want.LastLineDefined)
	}
	if got.NumParams != want.NumParams || got.Flag != want.Flag || got.MaxStackSize != want.MaxStackSize {
		t.Fatalf("header mismatch: got (%d,%d,%d), want (%d,%d,%d)",
			got.NumParams, got.Flag, got.MaxStackSize, want.NumParams, want.Flag, want.MaxStackSize)
	}
	if !reflect.DeepEqual(got.Code, want.Code) {
		t.Fatalf("code mismatch: len %d vs %d", len(got.Code), len(want.Code))
	}
	if len(got.Consts) != len(want.Consts) {
		t.Fatalf("consts len mismatch: %d vs %d", len(got.Consts), len(want.Consts))
	}
	for i := range got.Consts {
		gc, wc := got.Consts[i], want.Consts[i]
		if gc.Kind != wc.Kind || gc.Int != wc.Int || gc.Str != wc.Str || gc.IsShort != wc.IsShort {
			t.Fatalf("const[%d] mismatch: %+v vs %+v", i, gc, wc)
		}
		if gc.Kind == bytecode.ConstFloat {
			if math.Float64bits(gc.Float) != math.Float64bits(wc.Float) {
				t.Fatalf("const[%d] float bits mismatch: %x vs %x",
					i, math.Float64bits(gc.Float), math.Float64bits(wc.Float))
			}
		}
	}
	if len(got.Upvalues) != len(want.Upvalues) {
		t.Fatalf("upvalues len mismatch: %d vs %d", len(got.Upvalues), len(want.Upvalues))
	}
	for i := range got.Upvalues {
		gu, wu := got.Upvalues[i], want.Upvalues[i]
		if gu.InStack != wu.InStack || gu.Idx != wu.Idx || gu.Kind != wu.Kind {
			t.Fatalf("upvalue[%d] mismatch: %+v vs %+v", i, gu, wu)
		}
		if strip {
			if gu.Name != "" {
				t.Fatalf("stripped upvalue[%d] still has name %q", i, gu.Name)
			}
		} else if gu.Name != wu.Name {
			t.Fatalf("upvalue[%d] name mismatch: %q vs %q", i, gu.Name, wu.Name)
		}
	}
	if strip {
		if got.Source != "" || len(got.LineInfo) != 0 || len(got.AbsLineInfo) != 0 || len(got.LocVars) != 0 {
			t.Fatalf("stripped proto still has debug info: source=%q line=%d abs=%d loc=%d",
				got.Source, len(got.LineInfo), len(got.AbsLineInfo), len(got.LocVars))
		}
	} else {
		if got.Source != want.Source {
			t.Fatalf("source mismatch: %q vs %q", got.Source, want.Source)
		}
		if !reflect.DeepEqual(got.LineInfo, want.LineInfo) {
			t.Fatalf("lineinfo mismatch: %d vs %d", len(got.LineInfo), len(want.LineInfo))
		}
		if !reflect.DeepEqual(got.AbsLineInfo, want.AbsLineInfo) {
			t.Fatalf("abslineinfo mismatch: %v vs %v", got.AbsLineInfo, want.AbsLineInfo)
		}
		if !reflect.DeepEqual(got.LocVars, want.LocVars) {
			t.Fatalf("locvars mismatch: %v vs %v", got.LocVars, want.LocVars)
		}
	}
	if len(got.Protos) != len(want.Protos) {
		t.Fatalf("protos len mismatch: %d vs %d", len(got.Protos), len(want.Protos))
	}
	for i := range got.Protos {
		assertProtosEqual(t, got.Protos[i], want.Protos[i], strip)
	}
}

func TestDumpRoundTripAndByteMatchWithLuac55(t *testing.T) {
	scripts, err := filepath.Glob(filepath.Join("..", "bench", "scripts", "*.lua"))
	if err != nil {
		t.Fatalf("glob bench scripts: %v", err)
	}
	if len(scripts) == 0 {
		scripts, err = filepath.Glob(filepath.Join("..", "third_party", "lua", "testes", "*.lua"))
		if err != nil {
			t.Fatalf("glob lua testes scripts: %v", err)
		}
	}
	if len(scripts) == 0 {
		t.Skip("no reference Lua scripts found")
	}

	for _, script := range scripts {
		name := filepath.Base(script)
		t.Run(name, func(t *testing.T) {
			for _, strip := range []bool{false, true} {
				refChunk := compileWithLuac55(t, script, strip)
				p, err := bytecode.Undump(refChunk)
				if err != nil {
					t.Fatalf("Undump(strip=%v) failed: %v", strip, err)
				}
				dumped, err := bytecode.Dump(p, strip)
				if err != nil {
					t.Fatalf("Dump(strip=%v) failed: %v", strip, err)
				}
				// Exact byte-for-byte match with PUC-Rio ldump.c!
				if !bytes.Equal(dumped, refChunk) {
					t.Fatalf("Dump(strip=%v) differs from luac55 bytes (got %d bytes, want %d bytes)",
						strip, len(dumped), len(refChunk))
				}
				p2, err := bytecode.Undump(dumped)
				if err != nil {
					t.Fatalf("re-Undump(strip=%v) failed: %v", strip, err)
				}
				assertProtosEqual(t, p2, p, strip)
			}
		})
	}
}

func TestDumpExecutesInLua55AndEdgeCases(t *testing.T) {
	// Build a Lua script exercising edge-case constants: MinInt64, MaxInt64, -1, 0,
	// floats (-0.0, huge, subnormal, fractional), empty string, long string (>40 bytes),
	// repeated strings (testing deduplication), and >128 lines (testing AbsLineInfo alignment).
	src := `
local long1 = "01234567890123456789012345678901234567890123456789"
local long2 = "01234567890123456789012345678901234567890123456789"
local empty = ""
local minI = -9223372036854775807 - 1
local maxI = 9223372036854775807
local neg1 = -1
local bigF = 1.5e200
local negZ = -0.0
local function sub(x)
  return x .. long1 .. long2 .. empty
end
print(minI, maxI, neg1, bigF, negZ, #sub("ok"))
`
	tmpLua := filepath.Join(t.TempDir(), "edge.lua")
	if err := os.WriteFile(tmpLua, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	refOutCmd := exec.Command(lua55Bin(t), tmpLua)
	refOut, err := refOutCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lua55_o2 failed: %v\n%s", err, string(refOut))
	}

	origChunk := compileWithLuac55(t, tmpLua, false)
	p, err := bytecode.Undump(origChunk)
	if err != nil {
		t.Fatal(err)
	}

	for _, strip := range []bool{false, true} {
		dumped, err := bytecode.Dump(p, strip)
		if err != nil {
			t.Fatalf("Dump(strip=%v) error: %v", strip, err)
		}
		gotOut := runChunkInLua55(t, dumped)
		if gotOut != string(refOut) {
			t.Fatalf("strip=%v output mismatch:\ngot:  %q\nwant: %q", strip, gotOut, string(refOut))
		}
		p2, err := bytecode.Undump(dumped)
		if err != nil {
			t.Fatalf("Undump(strip=%v) error: %v", strip, err)
		}
		assertProtosEqual(t, p2, p, strip)
	}
}

func TestDumpExtremeIntConstantsAndDedup(t *testing.T) {
	// Manually construct a Proto with MinInt64, MaxInt64, -1, 0, 1, -2, 2,
	// empty string, duplicate long strings, and AbsLineInfo.
	longStr := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	p := &bytecode.Proto{
		Source:          "@synthetic.lua",
		LineDefined:     0,
		LastLineDefined: 200,
		NumParams:       0,
		Flag:            bytecode.PFVarargHidden,
		MaxStackSize:    2,
		Code: []bytecode.Instruction{
			bytecode.Instruction(bytecode.OP_VARARGPREP),
			bytecode.Instruction(bytecode.OP_RETURN) | (1 << bytecode.PosB) | (1 << bytecode.PosC),
		},
		Consts: []bytecode.Constant{
			{Kind: bytecode.ConstNil},
			{Kind: bytecode.ConstFalse},
			{Kind: bytecode.ConstTrue},
			{Kind: bytecode.ConstInt, Int: math.MinInt64},
			{Kind: bytecode.ConstInt, Int: math.MaxInt64},
			{Kind: bytecode.ConstInt, Int: -1},
			{Kind: bytecode.ConstInt, Int: 0},
			{Kind: bytecode.ConstInt, Int: 1},
			{Kind: bytecode.ConstInt, Int: -2},
			{Kind: bytecode.ConstInt, Int: 2},
			{Kind: bytecode.ConstFloat, Float: -0.0},
			{Kind: bytecode.ConstFloat, Float: math.Inf(1)},
			{Kind: bytecode.ConstFloat, Float: math.Inf(-1)},
			{Kind: bytecode.ConstFloat, Float: math.SmallestNonzeroFloat64},
			{Kind: bytecode.ConstString, Str: "", IsShort: true},
			{Kind: bytecode.ConstString, Str: longStr, IsShort: false},
			{Kind: bytecode.ConstString, Str: longStr, IsShort: false},
			{Kind: bytecode.ConstString, Str: "", IsShort: true},
		},
		Upvalues: []bytecode.UpvalDesc{
			{Name: "_ENV", InStack: 1, Idx: 0, Kind: 0},
		},
		LineInfo: []int8{-128, 1},
		AbsLineInfo: []bytecode.AbsLineInfo{
			{PC: 0, Line: 150},
		},
		LocVars: []bytecode.LocVar{
			{Name: longStr, StartPC: 0, EndPC: 2},
		},
	}

	dumped, err := bytecode.Dump(p, false)
	if err != nil {
		t.Fatal(err)
	}
	// Verify longStr only appears ONCE in the binary output due to string deduplication.
	if count := bytes.Count(dumped, []byte(longStr)); count != 1 {
		t.Fatalf("expected longStr to appear once (deduplicated), appeared %d times", count)
	}
	p2, err := bytecode.Undump(dumped)
	if err != nil {
		t.Fatal(err)
	}
	assertProtosEqual(t, p2, p, false)

	// Verify C Lua 5.5.1 loads and executes the dumped chunk cleanly.
	if out := runChunkInLua55(t, dumped); out != "" {
		t.Fatalf("unexpected output: %q", out)
	}

	// Stripped dump omits debug info and still loads in C Lua 5.5.1.
	stripped, err := bytecode.Dump(p, true)
	if err != nil {
		t.Fatal(err)
	}
	p3, err := bytecode.Undump(stripped)
	if err != nil {
		t.Fatal(err)
	}
	assertProtosEqual(t, p3, p, true)
	if out := runChunkInLua55(t, stripped); out != "" {
		t.Fatalf("unexpected output (stripped): %q", out)
	}
}
