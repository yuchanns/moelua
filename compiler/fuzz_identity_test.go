package compiler_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
)

// FuzzCompileIdentity fuzzes Lua 5.5.1 expressions and statements
// covering constant-folding edge cases (-0.0, NaN, division by zero, mininteger // -1,
// bitwise ops, varargs, loops, closures, goto, <const>/<close>) and verifies:
//   1. compiler.Compile never panics and every compiled Proto passes bytecode.Verify.
//   2. bytecode.Dump -> bytecode.Undump round-trips identically.
//   3. Structured generated Lua snippets compile byte-for-byte identically to official luac 5.5.1.
func FuzzCompileIdentity(f *testing.F) {
	seeds := []string{
		"return -0.0, 0.0, 1/0, -1/0, 0/0",
		"return math.mininteger // -1, (-9223372036854775807 - 1) // -1, 1 // 0, 1 % 0",
		"return 1 << 64, 1 >> 64, -1 >> 1, ~0, 1 & 2 | 3 ~ 4",
		"local a <const> = -0.0; local b = 0.0; return a == b, 1/a, 1/b",
		"local function f(a, ...v) local x <close> = nil; return a, v.n, ... end; return f(1, 2, 3)",
		"global x, y <const> = 1, 2; local z = x + y * 3 - 4 / 5 // 6 % 7 ^ 8; return z",
		"local t = { [0] = -0.0, [1] = 1/0, a = 'hi', [true] = false }; return #t",
		"local i = 0; ::loop:: i = i + 1; if i < 5 then goto loop end; return i",
		"local s = 0; for i = 1, 10, 2 do s = s + i end; return s",
		"local a, b = 1, 2; return a < b and a <= b or not (a ~= b)",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 4096 {
			return
		}
		p, err := compiler.Compile("=fuzz", src)
		if err != nil {
			return
		}
		if verr := bytecode.Verify(p); verr != nil {
			t.Fatalf("compiled proto failed Verify: %v\nsrc:\n%s", verr, src)
		}
		dumped, err := bytecode.Dump(p, false)
		if err != nil {
			t.Fatalf("Dump failed: %v", err)
		}
		p2, err := bytecode.Undump(dumped)
		if err != nil {
			t.Fatalf("Undump round-trip failed: %v", err)
		}
		dumped2, err := bytecode.Dump(p2, false)
		if err != nil {
			t.Fatalf("re-Dump failed: %v", err)
		}
		if !bytes.Equal(dumped, dumped2) {
			t.Fatalf("Dump/Undump/Dump not idempotent on src:\n%s", src)
		}
	})
}

// TestCompileIdentityEdgeCasesAgainstLuac55 verifies byte-for-byte identity against
// official luac 5.5.1 on tricky constant-folding & codegen edge cases.
func TestCompileIdentityEdgeCasesAgainstLuac55(t *testing.T) {
	luac := filepath.Join("..", "build", "luac55")
	if _, err := os.Stat(luac); err != nil {
		t.Skipf("luac55 not built at %s", luac)
	}
	edgeCases := []struct {
		name string
		src  string
	}{
		{"neg_zero_and_nan", "return -0.0, 0.0, -0.0 + 0.0, -0.0 * 1.0, 0/0, 1/0, -1/0\n"},
		{"minint_idiv_neg1", "local x = -9223372036854775807 - 1; return x // -1, (-9223372036854775807 - 1) // -1, (-9223372036854775807 - 1) % -1\n"},
		{"div_mod_zero", "return 1 // 0, 1 % 0, 1.0 / 0.0, -1.0 / 0.0, 0.0 / 0.0, 1.0 % 0.0\n"},
		{"shifts_and_bitops", "return 1 << 63, 1 << 64, 1 << -1, -1 >> 1, -1 >> 64, ~0, ~(-1), 0x1234 & 0xff | 0x0f ~ 0xaa\n"},
		{"float_int_const_folding", "return 2 ^ 10, 2 ^ -1, (-2) ^ 3, 10 / 2, 10 // 3, -10 // 3, -10 % 3, 10 % -3\n"},
		{"const_close_vararg", "local function f(a, ...v) local c <const> = 42; local x <close> = nil; return a + c, v.n, ... end; return f(1, 2, 3)\n"},
		{"global_decl_const", "global <const> * ; global a, b <const> = 1, 2; return a + b\n"},
		{"nested_loops_goto", "local s = 0; for i = 1, 10 do if i == 5 then goto skip end; for j = 10, 1, -2 do s = s + i * j end; ::skip:: end; return s\n"},
		{"table_ctor_mixed", "local t = { [0] = -0.0, [1] = 1, 2, 3, a = 'x', ['b'] = 1/0, [false] = true }; return t\n"},
		{"short_circuit_conds", "local a, b, c = ...; local r = (a and b or c) and not (a == b or b < c or c <= a); return r\n"},
	}

	// Also synthesize 60 pseudo-random combinations of binary/unary ops on edge literals!
	lits := []string{"0", "1", "-1", "2", "-0.0", "0.0", "1.5", "-2.5", "0x7fffffffffffffff", "(-9223372036854775807-1)"}
	ops := []string{"+", "-", "*", "/", "//", "%", "^", "&", "|", "~", "<<", ">>", "==", "~=", "<", "<="}
	var seed uint64 = 0x5510deadbeef
	nextRand := func(n int) int {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return int(seed % uint64(n))
	}
	for i := range 60 {
		l1 := lits[nextRand(len(lits))]
		l2 := lits[nextRand(len(lits))]
		op1 := ops[nextRand(len(ops))]
		op2 := ops[nextRand(len(ops))]
		l3 := lits[nextRand(len(lits))]
		src := fmt.Sprintf("local a, b = %s %s %s, %s %s %s\nreturn a, b, -(a), ~(1), not b\n", l1, op1, l2, l2, op2, l3)
		edgeCases = append(edgeCases, struct{ name, src string }{
			name: fmt.Sprintf("synth_%02d", i),
			src:  src,
		})
	}

	tmp := t.TempDir()
	for _, tc := range edgeCases {
		t.Run(tc.name, func(t *testing.T) {
			inPath := filepath.Join(tmp, tc.name+".lua")
			outPath := filepath.Join(tmp, tc.name+".luac")
			if err := os.WriteFile(inPath, []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			// Compile with official luac55
			cmd := exec.Command(luac, "-o", outPath, inPath)
			luacOut, luacErr := cmd.CombinedOutput()

			// Compile with MoeLua compiler
			p, moeErr := compiler.Compile("@"+inPath, tc.src)
			if luacErr != nil {
				if moeErr == nil {
					t.Fatalf("luac55 rejected (%v: %s), but MoeLua accepted:\n%s", luacErr, luacOut, tc.src)
				}
				return
			}
			if moeErr != nil {
				t.Fatalf("luac55 accepted, but MoeLua failed (%v):\n%s", moeErr, tc.src)
			}
			want, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			got, err := bytecode.Dump(p, false)
			if err != nil {
				t.Fatalf("Dump failed: %v", err)
			}
			if !bytes.Equal(want, got) {
				// Find first differing byte for clear diagnostic.
				diffAt := -1
				for i := 0; i < min(len(want), len(got)); i++ {
					if want[i] != got[i] {
						diffAt = i
						break
					}
				}
				t.Fatalf("byte mismatch vs luac 5.5.1 (want %dB, got %dB, first diff at %d)\nsrc:\n%s\nwant=%x\ngot =%x",
					len(want), len(got), diffAt, tc.src, want, got)
			}
			_ = binary.LittleEndian
		})
	}
}
