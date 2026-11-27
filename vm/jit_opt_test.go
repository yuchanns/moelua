//go:build (arm64 || amd64) && !moeluanojit && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yuchanns/moelua/compiler"
	"github.com/yuchanns/moelua/vm"
)

func runLua3Way(t *testing.T, name, src string) {
	t.Helper()
	p, err := compiler.Compile("="+name, src)
	if err != nil {
		t.Fatalf("%s compile: %v", name, err)
	}
	runMode := func(jit, simd bool) (string, error) {
		s := vm.NewState()
		defer s.Close()
		if jit {
			s.SetJITEnabled(true)
			s.SetJITSIMDEnabled(simd)
		}
		var out bytes.Buffer
		s.SetOutput(&out)
		cl, err := s.Load(p)
		if err != nil {
			return "", err
		}
		_, err = s.Call(vm.ClosureValue(cl))
		return out.String(), err
	}

	outNo, errNo := runMode(false, false)
	outSc, errSc := runMode(true, false)
	outSi, errSi := runMode(true, true)

	if (errNo != nil) != (errSc != nil) || (errNo != nil) != (errSi != nil) {
		t.Fatalf("%s error mismatch:\n  noJIT: %v\n  scal:  %v\n  simd:  %v", name, errNo, errSc, errSi)
	}
	if outNo != outSc {
		t.Fatalf("%s JIT-Scalar mismatch:\n--- noJIT ---\n%s\n--- scalar ---\n%s", name, outNo, outSc)
	}
	if outNo != outSi {
		t.Fatalf("%s JIT+SIMD mismatch:\n--- noJIT ---\n%s\n--- simd ---\n%s", name, outNo, outSi)
	}
}

func TestJITOptSIMDVsScalarAndInterpreterParity(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "daxpy_trip_counts_and_poll_boundaries",
			src: `
local function daxpy(c, b, s, n)
  for i = 1, n do
    c[i] = c[i] + s * b[i]
  end
end
for _, n in ipairs({0, 1, 2, 3, 4, 7, 15, 16, 17, 100, 8191, 8192, 8193, 16390}) do
  local c, b = {}, {}
  for i = 1, n do
    c[i] = i * 0.25 - 1.5
    b[i] = (i % 7) * 0.125 + 0.5
  end
  daxpy(c, b, 2.375, n)
  local h = 0.0
  for i = 1, n do h = h + c[i] end
  print(n, string.format("%.17g", h))
end
`,
		},
		{
			name: "fp64_reduction_strict_ieee754_order",
			src: `
-- Non-associative values that differ bit-for-bit if lanes are reassociated!
local a = {}
for i = 1, 513 do
  local m = i % 4
  if m == 1 then a[i] = 1e16
  elseif m == 2 then a[i] = 1.0
  elseif m == 3 then a[i] = -1e16
  else a[i] = 0.125 * (i % 13) + 1e-15
  end
end
local sum = 0.0
for i = 1, #a do
  sum = sum + a[i]
end
print(string.format("%.17g", sum))
`,
		},
		{
			name: "int64_simd_arith_bitwise_and_reduction",
			src: `
local n = 1027
local a, b, c = {}, {}, {}
for i = 1, n do
  a[i] = (i * 1103515245 + 12345) ~ (i << 33)
  b[i] = (i * 2654435761) - (1 << 60)
  c[i] = 0
end
for i = 1, n do
  c[i] = ((a[i] + b[i]) - (a[i] & b[i])) ~ (a[i] | b[i])
end
local sum = 0
for i = 1, n do
  sum = sum + c[i]
end
print(sum, c[1], c[512], c[n])
`,
		},
		{
			name: "unary_fp64_simd_neg_sqrt_abs_signed_zero",
			src: `
local a, neg, rt, ab = {}, {}, {}, {}
for i = 1, 17 do
  a[i] = (i - 9) * 0.5
  neg[i], rt[i], ab[i] = 0.0, 0.0, 0.0
end
a[9] = 0.0
a[10] = -0.0
for i = 1, 17 do neg[i] = -a[i] end
for i = 1, 17 do ab[i] = math.abs(a[i]) end
for i = 1, 17 do rt[i] = math.sqrt(ab[i]) end
for i = 1, 17 do
  print(i, string.format("%.17g %.17g %.17g", 1.0 / neg[i], ab[i], rt[i]))
end
`,
		},
		{
			name: "runtime_alias_overlap_checks",
			src: `
local function axpy_off(dst, src, offD, offS, s, n)
  for i = 1, n do
    dst[offD + i] = dst[offD + i] + s * src[offS + i]
  end
end
-- Case 1: exact same slice (dst == src && offD == offS): elementwise safe
local a = {}
for i = 1, 40 do a[i] = i * 1.0 end
axpy_off(a, a, 0, 0, 2.5, 40)
-- Case 2: overlapping forward dependence (offD = 1, offS = 0): reads a[i], writes a[i+1]
axpy_off(a, a, 1, 0, 0.5, 30)
-- Case 3: overlapping backward dependence (offD = 0, offS = 1): reads a[i+1], writes a[i]
axpy_off(a, a, 0, 1, 0.25, 30)
-- Case 4: non-overlapping slices of same array (offD = 0, offS = 20, n = 20)
axpy_off(a, a, 0, 20, 1.5, 20)
local out = {}
for i = 1, 40 do out[i] = string.format("%.17g", a[i]) end
print(table.concat(out, ","))
`,
		},
		{
			name: "sor_dependent_stencil_stays_scalar",
			src: `
local v, vp, vn = {}, {}, {}
local n = 64
for i = 1, n do
  v[i] = i * 0.1
  vp[i] = i * 0.2
  vn[i] = i * 0.3
end
for iter = 1, 5 do
  for x = 2, n - 1 do
    v[x] = 0.3125 * ((vp[x] + vn[x]) + (v[x - 1] + v[x + 1])) + (-0.25) * v[x]
  end
end
local s = 0.0
for i = 1, n do s = s + v[i] end
print(string.format("%.17g", s))
`,
		},
		{
			name: "five_array_stream_exceeds_simd_ptr_budget",
			src: `
local n = 33
local a, b, c, d, e = {}, {}, {}, {}, {}
for i = 1, n do
  a[i], b[i], c[i], d[i], e[i] = 0.0, i*1.0, i*2.0, i*3.0, i*4.0
end
for i = 1, n do
  a[i] = b[i] + c[i] + d[i] + e[i]
end
print(a[1], a[17], a[n])
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runLua3Way(t, tc.name, tc.src)
		})
	}
}

func TestJITOptEdgeCasesAndDeoptSafety(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "zero_trip_int_vs_widening_float_accumulator",
			src: `
local function run(n)
  local a = {1.5, 2.5, 3.5}
  local sum = 0
  for i = 1, n do
    sum = sum + a[i]
  end
  return sum, math.type(sum)
end
print(run(0))
print(run(3))
`,
		},
		{
			name: "negative_and_dynamic_steps_with_bce",
			src: `
local a = {}
for i = 1, 50 do a[i] = i * 3 end
local s1 = 0
for i = 50, 1, -1 do s1 = s1 + a[i] end
local s2 = 0
for i = 50, 2, -3 do s2 = s2 + a[i] end
local function dyn(step)
  local s = 0
  local lo, hi = 1, 50
  if step < 0 then lo, hi = 50, 1 end
  for i = lo, hi, step do s = s + a[i] end
  return s
end
print(s1, s2, dyn(1), dyn(2), dyn(-1), dyn(-4))
`,
		},
		{
			name: "idiv_mod_minint_and_divzero_deopt",
			src: `
local function test_div(divs)
  local acc = 0
  for i = 1, #divs do
    acc = acc + (math.mininteger // divs[i]) + (math.mininteger % divs[i])
  end
  return acc
end
print(test_div({-1, 3, -7, 1, -1}))
local ok, err = pcall(test_div, {-1, 2, 0, 4})
print(ok, type(err))
`,
		},
		{
			name: "nan_comparisons_in_opt_loops",
			src: `
local function cmp_nan(vals)
  local lt, le, eq, ne = 0, 0, 0, 0
  local n = #vals
  local i = 1
  while i <= n do
    local x = vals[i]
    if x < 1.0 then lt = lt + 1 end
    if x <= 1.0 then le = le + 1 end
    if x == x then eq = eq + 1 end
    if x ~= x then ne = ne + 1 end
    i = i + 1
  end
  return lt, le, eq, ne
end
local nan = 0.0 / 0.0
print(cmp_nan({0.5, 1.0, 2.0, nan, -0.0, nan}))
`,
		},
		{
			name: "tier2_wide_shape_over_32_fields_and_newindex_deopt",
			src: `
local function make_wide(v)
  return {
    f01=v, f02=v, f03=v, f04=v, f05=v, f06=v, f07=v, f08=v,
    f09=v, f10=v, f11=v, f12=v, f13=v, f14=v, f15=v, f16=v,
    f17=v, f18=v, f19=v, f20=v, f21=v, f22=v, f23=v, f24=v,
    f25=v, f26=v, f27=v, f28=v, f29=v, f30=v, f31=v, f32=v,
    f33=v, f34=v, f35=v, f36=v,
  }
end
local objs = {}
for i = 1, 5 do objs[i] = make_wide(i * 1.0) end
local function bump(list)
  local s = 0.0
  for i = 1, #list do
    local o = list[i]
    local v = o.f01 + o.f36
    o.f36 = v
    s = s + v
  end
  return s
end
-- Warm past jitPollInterval (8192) to force Tier-2 compilation of bump() with >32-slot offsets!
local total = 0.0
for k = 1, 2000 do
  total = total + bump(objs)
end
-- Now set f36 = nil on an object of the exact same shape WITH a __newindex metatable:
-- OpGuardShape MUST deopt and invoke __newindex instead of silently overwriting the nil slot!
local intercepted = 0
local special = make_wide(1.0)
special.f36 = nil
setmetatable(special, {
  __index = function() return 10.0 end,
  __newindex = function(t, k, v)
    intercepted = intercepted + 1
    rawset(t, k, v + 100.0)
  end,
})
local s2 = bump({special})
print(string.format("%.17g", total), intercepted, s2, rawget(special, "f36"))
`,
		},
		{
			name: "arrgeneric_len_fast_path_and_holes",
			src: `
local dense = {{1}, {2}, {3}, {4}, {5}}
local empty_gen = {{1}}
empty_gen[1] = nil
local hole = {{1}, {2}, nil, {4}}
local trailing_nil = {{1}, {2}, {3}}
trailing_nil[3] = nil
local mt = setmetatable({{1}, {2}}, { __len = function() return 99 end })
local sum = 0
for i = 1, 100 do
  sum = sum + #dense + #empty_gen + #trailing_nil + #mt
  local h = #hole
  assert(h == 2 or h == 4)
end
print(sum, #dense, #empty_gen, #trailing_nil, #mt)
`,
		},
		{
			name: "codex_aliased_field_load_hoist_safety",
			src: `
local t = { x = 1.0, y = 1.0 }
local objs = { t, t }
local function step(o)
  for i = 1, 20000 do
    local a = o[1]
    local b = o[2]
    local av = a.x
    local bv = b.y
    a.x = av + 1.0
    b.y = bv + b.x
  end
end
step(objs)
print(string.format("%.1f %.1f", t.x, t.y))
`,
		},
		{
			name: "codex_conditional_float_preserves_int64_and_mathtype",
			src: `
local x = 9007199254740993
for i = 1, 10 do
  if i < 0 then
    x = x + 0.5
  end
end
local w = 9007199254740993
local k = 0
while k < 0 do
  w = w + 0.5
  k = k + 1
end
print(x, math.type(x), w, math.type(w))
`,
		},
		{
			name: "codex_multichunk_simd_neg_and_abs_over_20000",
			src: `
local n = 20000
local a, b, c = {}, {}, {}
for i = 1, n do
  a[i] = i * 1.0
  b[i] = 0.0
  c[i] = 0.0
end
for i = 1, n do b[i] = -a[i] end
for i = 1, n do c[i] = math.abs(b[i]) end
print(b[1], b[8191], b[8192], b[8193], b[20000], c[1], c[8192], c[20000])
`,
		},
		{
			name: "codex_unused_non_bce_array_load_invokes_index_metamethod",
			src: `
local calls = 0
local a = setmetatable({1, 2}, {
  __index = function(t, k)
    calls = calls + 1
    return 10
  end,
})
for i = 1, 10 do
  local unused = a[i * i]
end
print(calls)
`,
		},
		{
			name: "codex_body_local_constants_in_simd_splats",
			src: `
local a, b = {}, {}
for i = 1, 20 do a[i] = i * 1.0; b[i] = 0.0 end
for i = 1, 20 do
  local c = 2.0
  b[i] = a[i] + c
end
local ia, ib = {}, {}
for i = 1, 20 do ia[i] = i; ib[i] = 0 end
for i = 1, 20 do
  local k = 7
  ib[i] = ia[i] + k
end
print(b[1], b[2], b[20], ib[1], ib[2], ib[20])
`,
		},
		{
			name: "codex_type_changing_loop_var_write_rejected",
			src: `
local x = true
for i = 1, 10 do x = 1 end
local y = 5
for i = 1, 10 do y = false end
print(x, type(x), y, type(y))
`,
		},
		{
			name: "codex_shli_operand_order",
			src: `
local x, y = 0, 0
for i = 1, 10 do
  x = x + (1 << i)
  y = y + (1024 >> (i - 1))
end
print(x, y)
`,
		},
		{
			name: "codex_int_store_into_float_array_preserves_integer",
			src: `
local a, b = {}, {}
for i = 1, 100 do a[i] = 1.0; b[i] = 1.0 end
for i = 1, 100 do a[i] = 9007199254740993 end
local v = 9007199254740993
for i = 1, 100 do b[i] = v end
print(a[100], math.type(a[100]), b[100], math.type(b[100]))
`,
		},
		{
			name: "codex_mixed_int64_float_const_equality_no_rounding",
			src: `
local x = 9007199254740993
local f = 9007199254740992.0
local s1, s2, s3 = 0, 0, 0
for i = 1, 10 do
  if x == 9007199254740992.0 then s1 = s1 + 1 end
  if f == 9007199254740993 then s2 = s2 + 1 end
  local dummy = x + 0.5
  if x == 9007199254740992.0 then s3 = s3 + 1 end
end
print(s1, s2, s3)
`,
		},
		{
			name: "codex_leaf_inline_rejects_non_numeric_bool_args",
			src: `
local function f(x, y)
  return x + y
end
local function neg(x)
  return -x
end
local function run_add(n)
  local s = 0
  for i = 1, n do s = s + f(true, 1) end
  return s
end
local function run_neg(n)
  local s = 0
  for i = 1, n do s = s + neg(true) end
  return s
end
for _ = 1, 6 do run_add(0); run_neg(0) end
local ok1 = pcall(run_add, 10)
local ok2 = pcall(run_neg, 10)
print(ok1, ok2)
`,
		},
		{
			name: "codex_zero_init_var_preserved_on_deopt_before_first_write",
			src: `
local x = 42
local seen = {}
local a = setmetatable({1}, {
  __index = function(t, k)
    seen[#seen + 1] = x
    return 1
  end,
})
for i = 1, 10 do
  x = a[i * i + 1]
end
print(table.concat(seen, ","), x)
`,
		},
		{
			name: "codex_r3_shape_field_copy_preserves_int64",
			src: `
local t = { x = 9007199254740993, y = 0.0 }
local objs = { t }
for i = 1, 20000 do
  local a = objs[1]
  a.y = a.x
end
print(t.y, math.type(t.y))
`,
		},
		{
			name: "codex_r3_loopvar_int_rhs_assignment_preserves_int64",
			src: `
local x = 0.0
for i = 1, 10 do
  x = 9007199254740993
end
local u = 9007199254740993
local function copy_uv()
  local y = 0.0
  for i = 1, 10 do y = u end
  return y, math.type(y)
end
print(x, math.type(x), copy_uv())
`,
		},
		{
			name: "codex_r3_minint64_const_for_step",
			src: `
local s = 0
for i = 0, 0x8000000000000000, 0x8000000000000000 do
  s = s ~ i
end
print(s)
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runLua3Way(t, tc.name, tc.src)
		})
	}
}

func TestJITOptStripMinedSIMDCancelContext(t *testing.T) {
	src := `
local n = 100000
local a, b = {}, {}
for i = 1, n do a[i] = 1.0; b[i] = 2.0 end
for outer = 1, 1000000 do
  for i = 1, n do
    a[i] = a[i] + 1.5 * b[i]
  end
end
return a[1]
`
	p, err := compiler.Compile("=cancel_simd", src)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	s := vm.NewState()
	defer s.Close()
	s.SetJITEnabled(true)
	s.SetContext(ctx)
	cl, err := s.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	_, err = s.Call(vm.ClosureValue(cl))
	elapsed := time.Since(t0)
	if err == nil {
		t.Fatalf("expected context cancellation error inside SIMD loop, got nil")
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "cancel") && !strings.Contains(err.Error(), "interrupt") {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("SIMD loop took too long to cancel: %v", elapsed)
	}
}
