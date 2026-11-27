package vm

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/internal/arena"
)

// TestAsmOffsetsHeaderSync parses vm/asm_offsets.h and checks every #define constant
// against Go's actual unsafe.Sizeof, unsafe.Offsetof, and VM constants so arm64/amd64
// assembly can never silently drift out of sync with Go struct layouts.
func TestAsmOffsetsHeaderSync(t *testing.T) {
	if PolarityName != "intnil" {
		t.Skip("asm_offsets.h targets default intnil polarity")
	}
	f, err := os.Open("asm_offsets.h")
	if err != nil {
		t.Fatalf("open asm_offsets.h: %v", err)
	}
	defer f.Close()

	defines := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "#define ") {
			continue
		}
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		v, err := strconv.ParseInt(fields[2], 0, 64)
		if err != nil {
			t.Fatalf("parse #define %s %s: %v", fields[1], fields[2], err)
		}
		defines[fields[1]] = v
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	want := map[string]int64{
		"VAL_SIZE":             int64(unsafe.Sizeof(Value{})),
		"VAL_BITS":             int64(unsafe.Offsetof(Value{}.bits)),
		"VAL_TAG":              int64(unsafe.Offsetof(Value{}.tag)),
		"TAG_INT":              int64(tagInt),
		"TAG_FLOAT":            int64(tagFloat),
		"TAG_NIL":              int64(tagNil),
		"TAG_FALSE":            int64(tagFalse),
		"TAG_TRUE":             int64(tagTrue),
		"TAG_LIGHTUD":          int64(tagLightUD),
		"TAG_TOKENLUD":         int64(tagTokenLightUD),
		"TAG_STATICNAT":        int64(tagStaticNative),
		"TAG_FROZENSTR":        int64(tagFrozenString),
		"TAG_STRING":           int64(tagString),
		"TAG_CLOSURE":          int64(tagClosure),
		"TAG_NATIVE":           int64(tagNative),
		"TAG_TABLE":            int64(tagTable),
		"TAG_USERDATA":         int64(tagUserdata),
		"TAG_THREAD":           int64(tagThread),
		"TAG_FIRST_GC":         int64(tagFirstGC),
		"TAG_LAST_GC":          int64(tagLastGC),
		"ARENA_WORDBITS":       13,
		"ARENA_WORDMASK":       0x1fff,
		"ARENA_ALIGNSHIFT":     3,
		"ARENA_LARGEBIT":       31,
		"MARK_BLACK_BIT":       2,
		"TBL_SIZE":             int64(unsafe.Sizeof(Table{})),
		"TBL_MARK":             int64(unsafe.Offsetof(Table{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Mark)),
		"TBL_FLAGS":            int64(unsafe.Offsetof(Table{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Flags)),
		"TBL_SELF":             int64(unsafe.Offsetof(Table{}.self)),
		"TBL_SHAPEREF":         int64(unsafe.Offsetof(Table{}.shapeRef)),
		"TBL_METAREF":          int64(unsafe.Offsetof(Table{}.metaRef)),
		"TBL_HASHREF":          int64(unsafe.Offsetof(Table{}.hashRef)),
		"TBL_ARRREF":           int64(unsafe.Offsetof(Table{}.arrRef)),
		"TBL_SLOTSREF":         int64(unsafe.Offsetof(Table{}.slotsRef)),
		"TBL_ARRLEN":           int64(unsafe.Offsetof(Table{}.arrLen)),
		"TBL_ARRCAP":           int64(unsafe.Offsetof(Table{}.arrCap)),
		"TBL_SLOTSLEN":         int64(unsafe.Offsetof(Table{}.slotsLen)),
		"TBL_SLOTSCAP":         int64(unsafe.Offsetof(Table{}.slotsCap)),
		"TBL_ARRKIND":          int64(unsafe.Offsetof(Table{}.arrKind)),
		"TBL_TMFLAGS":          int64(unsafe.Offsetof(Table{}.tmFlags)),
		"TBL_INLINESLOTS":      int64(unsafe.Offsetof(Table{}.inlineSlots)),
		"TBL_FLAG_ISPROTO_BIT": 0,
		"ARR_EMPTY":            int64(arrEmpty),
		"ARR_INT":              int64(arrInt),
		"ARR_FLOAT":            int64(arrFloat),
		"ARR_BOOL":             int64(arrBool),
		"ARR_GENERIC":          int64(arrGeneric),
		"CL_SIZE":              int64(unsafe.Sizeof(Closure{})),
		"CL_SELF":              int64(unsafe.Offsetof(Closure{}.self)),
		"CL_PROTOREF":          int64(unsafe.Offsetof(Closure{}.protoRef)),
		"CL_UPVALSREF":         int64(unsafe.Offsetof(Closure{}.upvalsRef)),
		"CL_PROTOIDX":          int64(unsafe.Offsetof(Closure{}.protoIdx)),
		"CL_NUPVALS":           int64(unsafe.Offsetof(Closure{}.nUpvals)),
		"UV_SIZE":              int64(unsafe.Sizeof(Upvalue{})),
		"UV_MARK":              int64(unsafe.Offsetof(Upvalue{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Mark)),
		"UV_SELF":              int64(unsafe.Offsetof(Upvalue{}.self)),
		"UV_OWNERREF":          int64(unsafe.Offsetof(Upvalue{}.ownerRef)),
		"UV_CLOSED":            int64(unsafe.Offsetof(Upvalue{}.closed)),
		"UV_STACKIDX":          int64(unsafe.Offsetof(Upvalue{}.stackIdx)),
		"IC_SIZE":              int64(unsafe.Sizeof(ICEntry{})),
		"IC_VAL":               int64(unsafe.Offsetof(ICEntry{}.val)),
		"IC_EPOCH":             int64(unsafe.Offsetof(ICEntry{}.epoch)),
		"IC_SHAPEREF":          int64(unsafe.Offsetof(ICEntry{}.shapeRef)),
		"IC_TARGETSHAPE":       int64(unsafe.Offsetof(ICEntry{}.targetShapeRef)),
		"IC_HOLDERREF":         int64(unsafe.Offsetof(ICEntry{}.holderRef)),
		"IC_POLYREF":           int64(unsafe.Offsetof(ICEntry{}.polyRef)),
		"IC_SLOT":              int64(unsafe.Offsetof(ICEntry{}.slot)),
		"IC_KIND":              int64(unsafe.Offsetof(ICEntry{}.kind)),
		"IC_NOMETA":            int64(unsafe.Offsetof(ICEntry{}.noMeta)),
		"IC_PROTOABSENT":       int64(unsafe.Offsetof(ICEntry{}.protoAbsent)),
		"IC_POLYCOUNT":         int64(unsafe.Offsetof(ICEntry{}.polyCount)),
		"IC_EMPTY":             int64(icEmpty),
		"IC_OWNSLOT":           int64(icOwnSlot),
		"IC_PROTOVAL":          int64(icProtoVal),
		"IC_ADDSLOT":           int64(icAddSlot),
		"IC_ENVSLOT":           int64(icEnvSlot),
		"POLY_SIZE":            int64(unsafe.Sizeof(polyEntry{})),
		"POLY_VAL":             int64(unsafe.Offsetof(polyEntry{}.val)),
		"POLY_EPOCH":           int64(unsafe.Offsetof(polyEntry{}.epoch)),
		"POLY_SHAPEREF":        int64(unsafe.Offsetof(polyEntry{}.shapeRef)),
		"POLY_TARGETSHAPE":     int64(unsafe.Offsetof(polyEntry{}.targetShapeRef)),
		"POLY_SLOT":            int64(unsafe.Offsetof(polyEntry{}.slot)),
		"POLY_KIND":            int64(unsafe.Offsetof(polyEntry{}.kind)),
		"POLY_NOMETA":          int64(unsafe.Offsetof(polyEntry{}.noMeta)),
		"POLY_PROTOABSENT":     int64(unsafe.Offsetof(polyEntry{}.protoAbsent)),
		"THASH_NODESREF":       int64(unsafe.Offsetof(tableHash{}.nodesRef)),
		"THASH_MASK":           int64(unsafe.Offsetof(tableHash{}.mask)),
		"THASH_DICTGEN":        int64(unsafe.Offsetof(tableHash{}.dictGen)),
		"HNODE_SIZE":           int64(unsafe.Sizeof(hashNode{})),
		"HNODE_KEY":            int64(unsafe.Offsetof(hashNode{}.key)),
		"HNODE_VAL":            int64(unsafe.Offsetof(hashNode{}.val)),
		"HNODE_STATE":          int64(unsafe.Offsetof(hashNode{}.state)),
		"CF_SIZE":              int64(unsafe.Sizeof(callFrame{})),
		"CF_KIND":              int64(unsafe.Offsetof(callFrame{}.kind)),
		"CF_CLREF":             int64(unsafe.Offsetof(callFrame{}.clRef)),
		"CF_VARARGTABREF":      int64(unsafe.Offsetof(callFrame{}.varargTabRef)),
		"CF_BASE":              int64(unsafe.Offsetof(callFrame{}.base)),
		"CF_FUNCIDX":           int64(unsafe.Offsetof(callFrame{}.funcIdx)),
		"CF_PC":                int64(unsafe.Offsetof(callFrame{}.pc)),
		"CF_NRESULTS":          int64(unsafe.Offsetof(callFrame{}.nresults)),
		"CF_NEXTRAARGS":        int64(unsafe.Offsetof(callFrame{}.nextraargs)),
		"CF_FINISH":            int64(unsafe.Offsetof(callFrame{}.finish)),
		"CF_DIRTY":             int64(unsafe.Offsetof(callFrame{}.dirty)),
		"CF_ISTAILCALL":        int64(unsafe.Offsetof(callFrame{}.isTailCall)),
		"CF_EXTRAARGS":         int64(unsafe.Offsetof(callFrame{}.extraArgs)),
		"CF_OLDPC":             int64(unsafe.Offsetof(callFrame{}.oldPC)),
		"FRAME_LUA":            int64(frameLua),
		"FINISH_NONE":          int64(finishNone),
		"TH_SELF":              int64(unsafe.Offsetof(Thread{}.self)),
		"TH_STACKLEN":          int64(unsafe.Offsetof(Thread{}.stackLen)),
		"TH_TOP":               int64(unsafe.Offsetof(Thread{}.top)),
		"TH_HIGHSTACK":         int64(unsafe.Offsetof(Thread{}.highStack)),
		"TH_FRAMESCAP":         int64(unsafe.Offsetof(Thread{}.framesCap)),
		"TH_NFRAME":            int64(unsafe.Offsetof(Thread{}.nframe)),
		"TH_HIGHFRAMES":        int64(unsafe.Offsetof(Thread{}.highFrames)),
		"STR_KIND":             int64(unsafe.Offsetof(LuaString{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Kind)),
		"STR_LEN":              int64(unsafe.Offsetof(LuaString{}.Len)),
		"GCKIND_SHORTSTR":      int64(gcKindShortStr),
		"STACK_SLACK":          int64(stackSlack),
		"OFFSET_SC":            int64(bytecode.OffsetSC),
		"OFFSET_SBX":           int64(bytecode.OffsetSBx),
		"OFFSET_SJ":            int64(bytecode.OffsetSJ),
	}

	if asmDispatchEnabled {
		want["BP_INSNS"] = int64(unsafe.Offsetof(boundProto{}.insns))
		want["BP_CONSTS"] = int64(unsafe.Offsetof(boundProto{}.consts))
		want["BP_ICSLOTS"] = int64(unsafe.Offsetof(boundProto{}.icSlots))
		want["BP_IC"] = int64(unsafe.Offsetof(boundProto{}.ic))
		want["BP_NUMPARAMS"] = int64(unsafe.Offsetof(boundProto{}.numParams))
		want["BP_MAXSTACK"] = int64(unsafe.Offsetof(boundProto{}.maxStack))
		want["BP_ISVARARG"] = int64(unsafe.Offsetof(boundProto{}.isVararg))
	}

	for name, exp := range want {
		got, ok := defines[name]
		if !ok {
			t.Errorf("asm_offsets.h missing #define %s (want %d)", name, exp)
			continue
		}
		if got != exp {
			t.Errorf("asm_offsets.h %s = %d, want %d", name, got, exp)
		}
	}
}

// runViaGoSwitch executes src strictly through the Go switch (*State).run loop
// by bypassing runFast so differential tests can compare Plan 9 asm vs Go switch directly.
func runScriptOutput(t *testing.T, src string, useGoSwitch bool) string {
	t.Helper()
	var buf bytes.Buffer
	s := NewState()
	defer s.Close()
	s.SetOutput(&buf)

	p, err := defaultCompileFunc("=diff", src)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	cl, err := s.Load(p)
	if err != nil {
		t.Fatalf("bind failed: %v", err)
	}
	th := s.mainThread()
	if !useGoSwitch {
		if _, err := s.Call(ClosureValue(cl)); err != nil {
			t.Fatalf("asm execution failed: %v", err)
		}
		return buf.String()
	}
	// Run directly through (*State).run (Go switch loop) for exact differential parity.
	funcIdx := th.top
	if err := th.pushLuaFrame(s, cl, funcIdx, 0, 0); err != nil {
		t.Fatalf("pushLuaFrame failed: %v", err)
	}
	if err := s.run(th, 0); err != nil {
		t.Fatalf("go switch execution failed: %v", err)
	}
	return buf.String()
}

// TestAsmDifferentialParity verifies bit-for-bit identical output between the Plan 9
// direct-threaded interpreter and the portable Go switch interpreter across tricky edge
// cases: NaN/signed-zero comparisons, int64 boundaries, mixed int/float arithmetic,
// bitwise/shift operand order, negative mod/idiv, large (>32 KiB) dense arrays,
// polymorphic inline caches, black-table/closed-upval GC barriers, deep recursion,
// poll-interval yields, coroutines, and metamethods.
func TestAsmDifferentialParity(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "ArithmeticAndBitwise",
			src: `
				local function check(a, b)
					print(a + b, a - b, a * b, a / b, a // b, a % b)
				end
				check(17, 5)
				check(-17, 5)
				check(17, -5)
				check(-17, -5)
				check(17.5, 5)
				check(17, 2.5)
				check(10.0, 3.0)
				local x, y = 0x123456789abcdef, 0xf0f0f0f0
				print(x & y, x | y, x ~ y, ~x, -x, -(3.25))
				print(x & 0xff, x | 0x100, x ~ 0x55)
				print(1 << 3, 1 << 63, 1 << 64, 1 << -1, 16 >> 2, 16 >> 64, 16 >> -2)
				local s = 4
				print(3 << s, s >> 1, 7 << s, (1 << 5) >> 2)
				print(10 + 20, 10 - 3, 10 * 4, 10 / 4, 10 // 3, 10 % 3)
				print(math.mininteger, -math.mininteger, math.mininteger // -1)
				print(math.sqrt(9), math.sqrt(2.25), math.sqrt(0), math.abs(-42), math.abs(42), math.abs(-3.5), math.abs(math.mininteger))
				-- Short vs long string equality & inequality
				local s1, s2 = "abc", "def"
				local l1 = string.rep("x", 60)
				local l2 = string.rep("x", 60)
				local l3 = string.rep("y", 60)
				print(s1 == "abc", s1 == s2, s1 == "def", l1 == l2, l1 == l3)
				-- Tail calls & dirty frame reuse + negative/stepped/empty for loops
				local function tsum(n, acc)
					if n == 0 then return acc end
					return tsum(n - 1, acc + n)
				end
				local function wrap(n) return tsum(n, 0) end
				local f1, f2, f3 = 0, 0, 0
				for i = 10, 1, -3 do f1 = f1 + i end
				for i = 10, 1, 1 do f2 = f2 + 1 end
				for i = 1, 10, -1 do f3 = f3 + 1 end
				print(wrap(500), f1, f2, f3)
			`,
		},
		{
			name: "NaNAndCompareEdgeCases",
			src: `
				local nan = 0.0 / 0.0
				local z, nz = 0.0, -0.0
				local big1 = (1 << 53)
				local big2 = big1 + 1
				local bigf = big1 + 0.0
				print(nan == nan, nan ~= nan, nan < 0, nan <= 0, nan > 0, nan >= 0)
				print(0 < nan, 0 <= nan, 0 > nan, 0 >= nan, nan < nan, nan <= nan)
				print(z == nz, z == 0, nz == 0, 1 == 1.0, 1.0 == 1)
				print(big1 == bigf, big2 == bigf, big1 < big2, big2 < bigf, bigf < big2)
				local function cmpi(v)
					local ok1, r1 = pcall(function() return v < 5 end)
					local ok2, r2 = pcall(function() return v <= 5 end)
					local ok3, r3 = pcall(function() return v > 5 end)
					local ok4, r4 = pcall(function() return v >= 5 end)
					print(v == 5, ok1, r1, ok2, r2, ok3, r3, ok4, r4)
				end
				cmpi(5); cmpi(5.0); cmpi(4); cmpi(6); cmpi(nan); cmpi("5"); cmpi(nil)
				-- Float for-loop with NaN and negative step
				local s1 = 0
				for i = 1.0, 5.0, 0.5 do s1 = s1 + i end
				local s2 = 0
				for i = 5.0, 1.0, -1.0 do s2 = s2 + i end
				local s3 = 0
				for i = 1.0, nan, 1.0 do s3 = s3 + 1 end
				local s4 = 0
				for i = 1.0, 5.0, nan do s4 = s4 + 1 end
				print(s1, s2, s3, s4)
				-- Int for-loop near maxinteger
				local cnt = 0
				for i = math.maxinteger - 3, math.maxinteger do cnt = cnt + 1 end
				print(cnt)
			`,
		},
		{
			name: "LargeArraysAndTables",
			src: `
				-- > 32 KiB backing buffers trigger Arena largeSpans (>4096 int64/float64, >32768 bool, >2048 Value)
				local ai, af, ab, ag = {}, {}, {}, {}
				for i = 1, 5000 do
					ai[i] = i * 3 - 1
					af[i] = i * 0.25
					ab[i] = (i % 3 == 0)
				end
				for i = 1, 2500 do
					ag[i] = (i % 2 == 0) and ("s" .. i) or i
				end
				-- Force >32 KiB bool array as well
				for i = 5001, 35000 do ab[i] = (i % 2 == 1) end
				local si, sf, sb, sg = 0, 0.0, 0, 0
				for i = 1, 5000 do
					si = si + ai[i]
					sf = sf + af[i]
				end
				for i = 1, 35000 do
					if ab[i] then sb = sb + 1 end
				end
				for i = 1, 2500 do
					if type(ag[i]) == "string" then sg = sg + #ag[i] else sg = sg + ag[i] end
				end
				print(#ai, #af, #ab, #ag, si, sf, sb, sg, ai[1], ai[5000], af[1], af[5000], ab[1], ab[35000])
			`,
		},
		{
			name: "PolymorphicICAndMetatables",
			src: `
				local Base = {}
				Base.__index = Base
				function Base:score() return self.x + self.y end
				local Sub = setmetatable({}, Base)
				Sub.__index = Sub
				function Sub:score() return self.x * 2 + self.y end

				local objs = {
					setmetatable({x = 1, y = 2}, Base),
					setmetatable({y = 3, x = 4}, Base),
					setmetatable({x = 5, y = 6, z = 7}, Sub),
					setmetatable({a = 0, x = 8, y = 9}, Sub),
				}
				local total = 0
				for r = 1, 50 do
					for i = 1, #objs do
						local o = objs[i]
						o.x = o.x + 1
						total = total + o:score()
					end
				end
				-- Mutate prototype mid-run to bump protoEpoch and verify IC invalidation
				function Base:score() return self.x - self.y end
				for i = 1, #objs do
					total = total + objs[i]:score()
				end
				-- Closed & open upvalues + globals
				G_SUM = 0
				local function makeAcc(init)
					local acc = init
					return function(d)
						acc = acc + d
						G_SUM = G_SUM + acc
						return acc
					end
				end
				local f = makeAcc(10)
				for i = 1, 20 do f(i) end
				print(total, f(0), G_SUM)
			`,
		},
		{
			name: "DeepRecursionAndPollYield",
			src: `
				local function fib(n)
					if n < 2 then return n end
					return fib(n - 1) + fib(n - 2)
				end
				local function multi(a, b)
					if a == 0 then return end
					return a + b
				end
				-- Loop well past asmPollInterval (8192) to exercise asmExitPoll reentry
				local acc = 0
				local i = 0
				while i < 25000 do
					acc = acc + (i & 7)
					i = i + 1
				end
				local fsum = 0
				for j = 1, 25000 do fsum = fsum + (j % 11) end
				local r0, r1 = multi(0, 5)
				local r2, r3 = multi(7, 8)
				print(fib(18), acc, fsum, r0, r1, r2, r3)
			`,
		},
		{
			name: "GCBarriersAndCoroutines",
			src: `
				collectgarbage("incremental", 100, 100)
				local root = {}
				local closed_up
				do
					local holder = {}
					closed_up = function(v)
						holder = v
						return holder
					end
				end
				-- Force incremental mark steps while mutating fields, array slots, globals, and closed upvalues
				for i = 1, 400 do
					local node = {v = i, s = "val_" .. i}
					root[i] = node
					root.last = node
					G_LAST = node
					closed_up(node)
					if i % 10 == 0 then collectgarbage("step", 1) end
				end
				collectgarbage("collect")
				-- Coroutines & cross-thread upvalues
				local shared = 100
				local co = coroutine.create(function(k)
					for i = 1, k do
						shared = shared + i
						coroutine.yield(shared)
					end
					return shared * 2
				end)
				local res = {}
				while true do
					local ok, v = coroutine.resume(co, 5)
					if not ok or coroutine.status(co) == "dead" then
						res[#res + 1] = v
						break
					end
					res[#res + 1] = v
				end
				print(#root, root.last.v, G_LAST.s, closed_up(root[400]).v, table.concat(res, ","))
			`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotAsm := runScriptOutput(t, tc.src, false)
			gotGo := runScriptOutput(t, tc.src, true)
			if gotAsm != gotGo {
				t.Fatalf("mismatch between runFast (asm=%v) and Go switch:\nasm:\n%s\ngo:\n%s", asmDispatchEnabled, gotAsm, gotGo)
			}
		})
	}
}

// TestAsmCancellationAndHooks verifies that instruction limits, context cancellation,
// and debug.sethook still work seamlessly when Plan 9 asm dispatch is active.
func TestAsmCancellationAndHooks(t *testing.T) {
	// 1. Context cancellation on tight loop (must route through runHooked cleanly).
	s := NewState()
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.SetContext(ctx)
	p, err := defaultCompileFunc("=cancel", `local x = 0; for i = 1, 1000000 do x = x + i end; return x`)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := s.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(ClosureValue(cl)); err == nil {
		t.Fatalf("expected context cancellation error, got nil")
	}

	// 2. Enabling debug.sethook dynamically from inside an asm-executed Lua function.
	s2 := NewState()
	defer s2.Close()
	var out bytes.Buffer
	s2.SetOutput(&out)
	p2, err := defaultCompileFunc("=hook", `
		local n = 0
		local function work()
			local sum = 0
			for i = 1, 50 do sum = sum + i end
			return sum
		end
		local a = work()
		debug.sethook(function() n = n + 1 end, "", 10)
		local b = work()
		debug.sethook()
		local c = work()
		print(a, b, c, n > 0)
	`)
	if err != nil {
		t.Fatal(err)
	}
	cl2, err := s2.Load(p2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Call(ClosureValue(cl2)); err != nil {
		t.Fatalf("dynamic sethook failed: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "1275\t1275\t1275\ttrue" {
		t.Fatalf("unexpected dynamic sethook output: %q", got)
	}
}
