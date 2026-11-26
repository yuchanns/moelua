package vm

import (
	"cmp"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/yuchanns/moelua/bytecode"
)

func (s *State) registerFunc(tbl *Table, name string, intr uint8, fn func(*State, []Value) ([]Value, error)) *NativeFunc {
	nf := &NativeFunc{Name: name, Fn: fn, Intrinsic: intr}
	tbl.RawSetStr(s, s.intern.Intern(name), NativeValue(nf))
	return nf
}

func (s *State) initStdlib() {
	s.SetGlobal("_G", TableValue(s.env))
	s.SetGlobal("_VERSION", StringValue(s.intern.Intern("Lua 5.5")))

	// Core globals
	s.registerFunc(s.env, "print", intrNone, nativePrint)
	s.registerFunc(s.env, "assert", intrNone, nativeAssert)
	s.registerFunc(s.env, "error", intrNone, nativeError)
	s.registerFunc(s.env, "pcall", intrPCall, nativePCall)
	s.registerFunc(s.env, "xpcall", intrXPCall, nativeXPCall)
	s.registerFunc(s.env, "type", intrType, nativeType)
	s.registerFunc(s.env, "tostring", intrNone, nativeToString)
	s.registerFunc(s.env, "tonumber", intrNone, nativeToNumber)
	s.registerFunc(s.env, "select", intrSelect, nativeSelect)
	s.registerFunc(s.env, "rawget", intrRawGet, nativeRawGet)
	s.registerFunc(s.env, "rawset", intrRawSet, nativeRawSet)
	s.registerFunc(s.env, "rawequal", intrNone, nativeRawEqual)
	s.registerFunc(s.env, "rawlen", intrRawLen, nativeRawLen)
	s.registerFunc(s.env, "setmetatable", intrNone, nativeSetMetatable)
	s.registerFunc(s.env, "getmetatable", intrNone, nativeGetMetatable)
	s.registerFunc(s.env, "collectgarbage", intrNone, nativeCollectGarbage)

	s.builtinNext = s.registerFunc(s.env, "next", intrNext, nativeNext)
	s.builtinIPairsIter = &NativeFunc{Name: "ipairs_iter", Fn: nativeIPairsIter, Intrinsic: intrIPairsIter}
	s.registerFunc(s.env, "pairs", intrPairs, nativePairs)
	s.registerFunc(s.env, "ipairs", intrNone, nativeIPairs)

	// math library
	mathTbl := s.NewTable(0, 32)
	mathTbl.RawSetStr(s, s.intern.Intern("pi"), FloatValue(math.Pi))
	mathTbl.RawSetStr(s, s.intern.Intern("huge"), FloatValue(math.Inf(1)))
	mathTbl.RawSetStr(s, s.intern.Intern("maxinteger"), IntValue(math.MaxInt64))
	mathTbl.RawSetStr(s, s.intern.Intern("mininteger"), IntValue(math.MinInt64))
	s.registerFunc(mathTbl, "floor", intrMathFloor, nativeMathFloor)
	s.registerFunc(mathTbl, "ceil", intrMathCeil, nativeMathCeil)
	s.registerFunc(mathTbl, "abs", intrMathAbs, nativeMathAbs)
	s.registerFunc(mathTbl, "sqrt", intrMathSqrt, nativeMathSqrt)
	s.registerFunc(mathTbl, "sin", intrMathSin, nativeMathSin)
	s.registerFunc(mathTbl, "cos", intrMathCos, nativeMathCos)
	s.registerFunc(mathTbl, "tan", intrNone, nativeMathUnaryFloat(math.Tan))
	s.registerFunc(mathTbl, "asin", intrNone, nativeMathUnaryFloat(math.Asin))
	s.registerFunc(mathTbl, "acos", intrNone, nativeMathUnaryFloat(math.Acos))
	s.registerFunc(mathTbl, "atan", intrNone, nativeMathAtan)
	s.registerFunc(mathTbl, "deg", intrNone, nativeMathUnaryFloat(func(x float64) float64 { return x * (180.0 / math.Pi) }))
	s.registerFunc(mathTbl, "rad", intrNone, nativeMathUnaryFloat(func(x float64) float64 { return x * (math.Pi / 180.0) }))
	s.registerFunc(mathTbl, "exp", intrNone, nativeMathUnaryFloat(math.Exp))
	s.registerFunc(mathTbl, "log", intrNone, nativeMathLog)
	s.registerFunc(mathTbl, "fmod", intrNone, nativeMathFmod)
	s.registerFunc(mathTbl, "modf", intrNone, nativeMathModf)
	s.registerFunc(mathTbl, "frexp", intrNone, nativeMathFrexp)
	s.registerFunc(mathTbl, "ldexp", intrNone, nativeMathLdexp)
	s.registerFunc(mathTbl, "min", intrMathMin, nativeMathMin)
	s.registerFunc(mathTbl, "max", intrMathMax, nativeMathMax)
	s.registerFunc(mathTbl, "tointeger", intrNone, nativeMathToInteger)
	s.registerFunc(mathTbl, "type", intrNone, nativeMathType)
	s.registerFunc(mathTbl, "ult", intrNone, nativeMathUlt)
	s.registerFunc(mathTbl, "random", intrNone, nativeMathRandom)
	s.registerFunc(mathTbl, "randomseed", intrNone, nativeMathRandomSeed)
	s.SetGlobal("math", TableValue(mathTbl))

	// table library
	tableTbl := s.NewTable(0, 12)
	s.registerFunc(tableTbl, "create", intrNone, nativeTableCreate)
	s.registerFunc(tableTbl, "insert", intrTableInsert, nativeTableInsert)
	s.registerFunc(tableTbl, "remove", intrTableRemove, nativeTableRemove)
	s.registerFunc(tableTbl, "concat", intrNone, nativeTableConcat)
	s.registerFunc(tableTbl, "move", intrNone, nativeTableMove)
	s.registerFunc(tableTbl, "pack", intrNone, nativeTablePack)
	s.registerFunc(tableTbl, "unpack", intrNone, nativeTableUnpack)
	s.registerFunc(tableTbl, "sort", intrNone, nativeTableSort)
	s.SetGlobal("table", TableValue(tableTbl))

	// string library + string metatable + utf8 library
	strTbl := s.NewTable(0, 20)
	s.registerFunc(strTbl, "len", intrStringLen, nativeStringLen)
	s.registerFunc(strTbl, "byte", intrStringByte, nativeStringByte)
	s.registerFunc(strTbl, "char", intrNone, nativeStringChar)
	s.registerFunc(strTbl, "sub", intrNone, nativeStringSub)
	s.registerFunc(strTbl, "rep", intrNone, nativeStringRep)
	s.registerFunc(strTbl, "reverse", intrNone, nativeStringReverse)
	s.registerFunc(strTbl, "lower", intrNone, nativeStringLower)
	s.registerFunc(strTbl, "upper", intrNone, nativeStringUpper)
	s.registerFunc(strTbl, "format", intrNone, nativeStringFormatStdlib)
	s.initStringExtraAndUTF8(strTbl)
	s.SetGlobal("string", TableValue(strTbl))

	s.stringMeta = s.NewTable(0, 12)
	s.stringMeta.RawSetStr(s, s.intern.tms[TMIndex], TableValue(strTbl))
	s.initStringArithMetamethods(s.stringMeta)
	s.stringMeta.markProto(s)
	s.typeMetas[4] = s.stringMeta

	// coroutine, debug, io, os, package, load/loadfile/dofile/warn
	s.initCoroLib()
	s.initDebugLib()
	s.initIOOSPackageLibs()
}

// initStringArithMetamethods installs Lua 5.4/5.5 string-to-number arithmetic metamethods
// on the string metatable (stateless so they are safe in the frozen stdlib template).
func (s *State) initStringArithMetamethods(mt *Table) {
	strToNum := func(st *State, v Value) (Value, bool, error) {
		if v.IsNumber() {
			return v, true, nil
		}
		if !v.IsString() {
			return NilValue(), false, nil
		}
		res, err := nativeToNumber(st, []Value{v})
		if err != nil {
			return NilValue(), false, err
		}
		if len(res) == 0 || res[0].IsNil() {
			return NilValue(), false, nil
		}
		return res[0], true, nil
	}
	binArith := func(tm TM, op string, iop func(int64, int64) (int64, error), fop func(float64, float64) float64, floatOnly bool) func(*State, []Value) ([]Value, error) {
		return func(st *State, args []Value) ([]Value, error) {
			if len(args) < 2 {
				return nil, fmt.Errorf("attempt to %s", op)
			}
			n1, ok1, err1 := strToNum(st, args[0])
			if err1 != nil {
				return nil, err1
			}
			n2, ok2, err2 := strToNum(st, args[1])
			if err2 != nil {
				return nil, err2
			}
			if !ok1 || !ok2 {
				// Per Lua 5.5 lstrlib.c:trymt — if the 2nd operand is NOT a string and has the metamethod, call it!
				if !args[1].IsString() {
					if other := st.getTMByObj(args[1], tm); !other.IsNil() {
						return st.Call(other, args[0], args[1])
					}
				}
				return nil, fmt.Errorf("attempt to %s a '%s' with a '%s'", op, args[0].TypeName(), args[1].TypeName())
			}
			if !floatOnly && BothInt(n1, n2) {
				r, err := iop(n1.Int(), n2.Int())
				if err != nil {
					return nil, err
				}
				return []Value{IntValue(r)}, nil
			}
			f1, _ := n1.ToFloat()
			f2, _ := n2.ToFloat()
			return []Value{FloatValue(fop(f1, f2))}, nil
		}
	}
	s.registerFunc(mt, "__add", intrNone, binArith(TMAdd, "add", func(a, b int64) (int64, error) { return a + b, nil }, func(a, b float64) float64 { return a + b }, false))
	s.registerFunc(mt, "__sub", intrNone, binArith(TMSub, "sub", func(a, b int64) (int64, error) { return a - b, nil }, func(a, b float64) float64 { return a - b }, false))
	s.registerFunc(mt, "__mul", intrNone, binArith(TMMul, "mul", func(a, b int64) (int64, error) { return a * b, nil }, func(a, b float64) float64 { return a * b }, false))
	s.registerFunc(mt, "__mod", intrNone, binArith(TMMod, "mod", IntMod, FloatMod, false))
	s.registerFunc(mt, "__pow", intrNone, binArith(TMPow, "pow", nil, math.Pow, true))
	s.registerFunc(mt, "__div", intrNone, binArith(TMDiv, "div", nil, func(a, b float64) float64 { return a / b }, true))
	s.registerFunc(mt, "__idiv", intrNone, binArith(TMIDiv, "idiv", IntDiv, func(a, b float64) float64 { return math.Floor(a / b) }, false))
	s.registerFunc(mt, "__unm", intrNone, func(st *State, args []Value) ([]Value, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("attempt to negate a 'nil' value")
		}
		n, ok, err := strToNum(st, args[0])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("attempt to negate a '%s' value", args[0].TypeName())
		}
		if n.IsInt() {
			return []Value{IntValue(-n.Int())}, nil
		}
		return []Value{FloatValue(-n.Float())}, nil
	})
}

func nativePrint(s *State, args []Value) ([]Value, error) {
	cancelFn := s.nativeCancelFunc()
	work := 0
	for i, a := range args {
		if i > 0 {
			_, _ = s.out.Write([]byte{'\t'})
		}
		str, err := s.toStringValue(a)
		if err != nil {
			return nil, err
		}
		if err := writeIOStringWithCancel(s.out, str, cancelFn, &work); err != nil {
			return nil, err
		}
	}
	_, _ = s.out.Write([]byte{'\n'})
	return nil, nil
}

func (s *State) toStringValue(v Value) (string, error) {
	if tm := s.getTMByObj(v, TMToString); !tm.IsNil() {
		res, err := s.Call(tm, v)
		if err != nil {
			return "", err
		}
		if len(res) > 0 && (res[0].IsString() || res[0].IsNumber()) {
			return FormatValue(res[0]), nil
		}
		return "", fmt.Errorf("'__tostring' must return a string")
	}
	// Per Lua 5.5 lauxlib.c:luaL_tolstring, numbers, strings, booleans, and nil always
	// format as their primitive representation even if their metatable defines __name.
	if v.IsNumber() || v.IsString() || v.IsBool() || v.IsNil() {
		return FormatValue(v), nil
	}
	if mt := s.getMetatableOf(v); mt != nil {
		if nm := mt.RawGetStr(s.intern.Intern("__name")); nm.IsString() {
			if v.IsLightUD() {
				return s.joinStringsWithCancel(nm.AsString().S, ": ", v.formatLightUDAddr())
			}
			return s.joinStringsWithCancel(nm.AsString().S, ": ", fmt.Sprintf("%p", v.ptr))
		}
	}
	return FormatValue(v), nil
}

func nativeAssert(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'assert' (value expected)")
	}
	if args[0].IsFalsy() {
		if len(args) == 1 {
			return nativeError(s, []Value{StringValue(s.intern.Intern("assertion failed!"))})
		}
		return nativeError(s, []Value{args[1]})
	}
	return args, nil
}

func (s *State) errorWhere(level int) string {
	if level <= 0 || s.cur == nil {
		return ""
	}
	// Walk ALL frames (including framePCall/frameXPCall C frames!): if target level is a C frame (e.g. pcall(assert, false, "X")),
	// luaL_where returns "" (no file:line prefix, matching errors.lua:659)!
	target := level - 1
	if s.cur.nframe > 0 && s.cur.frames[s.cur.nframe-1].kind == frameNative {
		target = level
	}
	idx := s.cur.nframe - 1 - target
	if idx < 0 || idx >= s.cur.nframe {
		return ""
	}
	cf := &s.cur.frames[idx]
	if cf.kind != frameLua || cf.cl == nil {
		return ""
	}
	p := cf.cl.bp.proto
	pc := cf.pc
	if !cf.atCurrentPC && pc > 0 {
		pc--
	}
	line := getFuncLine(p, pc)
	if line > 0 {
		return fmt.Sprintf("%s:%d: ", shortSrc(p.Source), line)
	}
	return ""
}

func nativeError(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, &LuaError{Val: NilValue()}
	}
	level := 1
	if len(args) >= 2 && !args[1].IsNil() {
		lv, ok, nonInt, err := s.coerceIntKind(args[1])
		if err != nil {
			return nil, err
		}
		if !ok {
			if nonInt {
				return nil, fmt.Errorf("bad argument #2 to 'error' (number has no integer representation)")
			}
			return nil, fmt.Errorf("bad argument #2 to 'error' (number expected, got %s)", s.objTypeName(args[1]))
		}
		level = int(lv)
	}
	if level > 0 && args[0].IsString() {
		if where := s.errorWhere(level); where != "" {
			msg := args[0].AsString().S
			joined, err := s.joinStringsWithCancel(where, msg)
			if err != nil {
				return nil, err
			}
			ls, err := s.TryIntern(joined)
			if err != nil {
				return nil, err
			}
			return nil, &LuaError{Val: StringValue(ls)}
		}
	}
	return nil, &LuaError{Val: args[0]}
}

func nativePCall(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'pcall' (value expected)")
	}
	res, errVal, ok, abortErr := s.callProtectedMode(args[0], NilValue(), false, args[1:], false, NilValue())
	if abortErr != nil {
		return nil, abortErr
	}
	if !ok {
		return []Value{FalseValue(), errVal}, nil
	}
	out := make([]Value, 1+len(res))
	out[0] = TrueValue()
	copy(out[1:], res)
	return out, nil
}

func nativeXPCall(s *State, args []Value) ([]Value, error) {
	if len(args) < 2 || !args[1].IsFunction() {
		return nil, fmt.Errorf("bad argument #2 to 'xpcall' (function expected)")
	}
	res, errVal, ok, abortErr := s.callProtectedMode(args[0], args[1], true, args[2:], false, NilValue())
	if abortErr != nil {
		return nil, abortErr
	}
	if !ok {
		return []Value{FalseValue(), errVal}, nil
	}
	out := make([]Value, 1+len(res))
	out[0] = TrueValue()
	copy(out[1:], res)
	return out, nil
}

func nativeType(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'type' (value expected)")
	}
	return []Value{StringValue(s.intern.Intern(args[0].TypeName()))}, nil
}

func nativeToString(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'tostring' (value expected)")
	}
	if args[0].IsString() && s.getTMByObj(args[0], TMToString).IsNil() {
		return []Value{args[0]}, nil
	}
	str, err := s.toStringValue(args[0])
	if err != nil {
		return nil, err
	}
	ls, err := s.TryIntern(str)
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(ls)}, nil
}

func nativeToNumber(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'tonumber' (value expected)")
	}
	v := args[0]
	if len(args) == 1 || args[1].IsNil() {
		if v.IsNumber() {
			return []Value{v}, nil
		}
		if !v.IsString() {
			return []Value{NilValue()}, nil
		}
		iv, fv, isInt, ok, err := s.strToNum(v.AsString().S)
		if err != nil {
			return nil, err
		}
		if !ok {
			return []Value{NilValue()}, nil
		}
		if isInt {
			return []Value{IntValue(iv)}, nil
		}
		return []Value{FloatValue(fv)}, nil
	}
	// Per Lua 5.5 lbaselib.c:luaB_tonumber: validate argument #1 is a string BEFORE checking base (#2).
	if !v.IsString() {
		return nil, fmt.Errorf("bad argument #1 to 'tonumber' (string expected, got %s)", s.objTypeName(v))
	}
	base, ok, nonInt, err := s.coerceIntKind(args[1])
	if err != nil {
		return nil, err
	}
	if !ok {
		if nonInt {
			return nil, fmt.Errorf("bad argument #2 to 'tonumber' (number has no integer representation)")
		}
		return nil, fmt.Errorf("bad argument #2 to 'tonumber' (number expected, got %s)", s.objTypeName(args[1]))
	}
	if base < 2 || base > 36 {
		return nil, fmt.Errorf("bad argument #2 to 'tonumber' (base out of range)")
	}
	iv, ok, err := s.strToIntBase(v.AsString().S, int(base))
	if err != nil {
		return nil, err
	}
	if !ok {
		return []Value{NilValue()}, nil
	}
	return []Value{IntValue(iv)}, nil
}

func nativeSelect(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'select'")
	}
	if args[0].IsString() && strings.HasPrefix(args[0].AsString().S, "#") {
		return []Value{IntValue(int64(len(args) - 1))}, nil
	}
	idx, ok, err := s.coerceInt(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'select' (number expected)")
	}
	n := int64(len(args) - 1)
	if idx < 0 {
		idx = n + idx + 1
	}
	if idx < 1 {
		return nil, fmt.Errorf("bad argument #1 to 'select' (index out of range)")
	}
	if idx > n {
		return nil, nil
	}
	return args[idx:], nil
}

func nativeRawGet(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsTable() {
		got := "no value"
		if len(args) >= 1 {
			got = s.objTypeName(args[0])
		}
		return nil, fmt.Errorf("bad argument #1 to 'rawget' (table expected, got %s)", got)
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("bad argument #2 to 'rawget' (value expected)")
	}
	t := args[0].AsTable()
	if err := s.ensureLookupKeyHash(t, args[1]); err != nil {
		return nil, err
	}
	return []Value{t.RawGet(args[1])}, nil
}

func nativeRawSet(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsTable() {
		got := "no value"
		if len(args) >= 1 {
			got = s.objTypeName(args[0])
		}
		return nil, fmt.Errorf("bad argument #1 to 'rawset' (table expected, got %s)", got)
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("bad argument #2 to 'rawset' (value expected)")
	}
	if len(args) < 3 {
		return nil, fmt.Errorf("bad argument #3 to 'rawset' (value expected)")
	}
	if err := args[0].AsTable().RawSet(s, args[1], args[2]); err != nil {
		return nil, err
	}
	return []Value{args[0]}, nil
}

func nativeRawEqual(s *State, args []Value) ([]Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("bad argument to 'rawequal'")
	}
	eq, err := s.rawEqual(args[0], args[1])
	if err != nil {
		return nil, err
	}
	return []Value{BoolValue(eq)}, nil
}

func nativeRawLen(_ *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'rawlen'")
	}
	if args[0].IsTable() {
		return []Value{IntValue(args[0].AsTable().Len())}, nil
	}
	if args[0].IsString() {
		return []Value{IntValue(int64(len(args[0].AsString().S)))}, nil
	}
	return nil, fmt.Errorf("bad argument #1 to 'rawlen' (table or string expected)")
}

func nativeSetMetatable(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsTable() {
		got := "no value"
		if len(args) >= 1 {
			got = s.objTypeName(args[0])
		}
		return nil, fmt.Errorf("bad argument #1 to 'setmetatable' (table expected, got %s)", got)
	}
	// Per Lua 5.5 lbaselib.c:luaB_setmetatable: check argument #2 type BEFORE checking __metatable protection.
	if len(args) < 2 || (!args[1].IsNil() && !args[1].IsTable()) {
		got := "no value"
		if len(args) >= 2 {
			got = s.objTypeName(args[1])
		}
		return nil, fmt.Errorf("bad argument #2 to 'setmetatable' (nil or table expected, got %s)", got)
	}
	t := args[0].AsTable()
	if t.meta != nil && !t.meta.RawGetStr(s.intern.Intern("__metatable")).IsNil() {
		return nil, fmt.Errorf("cannot change a protected metatable")
	}
	if args[1].IsNil() {
		s.SetMetatable(t, nil)
		if err := s.ConsumeMemErr(); err != nil {
			return nil, err
		}
		return []Value{args[0]}, nil
	}
	s.SetMetatable(t, args[1].AsTable())
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return []Value{args[0]}, nil
}

func nativeGetMetatable(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'getmetatable'")
	}
	mt := s.getMetatableOf(args[0])
	if mt == nil {
		return []Value{NilValue()}, nil
	}
	if protected := mt.RawGetStr(s.intern.Intern("__metatable")); !protected.IsNil() {
		return []Value{protected}, nil
	}
	return []Value{TableValue(mt)}, nil
}

func nativeNext(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsTable() {
		return nil, fmt.Errorf("bad argument #1 to 'next' (table expected)")
	}
	key := NilValue()
	if len(args) >= 2 {
		key = args[1]
	}
	t := args[0].AsTable()
	if err := s.ensureLookupKeyHash(t, key); err != nil {
		return nil, err
	}
	k, v, err := t.Next(key)
	if err != nil {
		return nil, err
	}
	if k.IsNil() {
		return []Value{NilValue()}, nil
	}
	return []Value{k, v}, nil
}

func nativePairs(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'pairs'")
	}
	if tm := s.getTMByObj(args[0], TMPairs); !tm.IsNil() {
		res, err := s.Call(tm, args[0])
		if err != nil {
			return nil, err
		}
		out := []Value{NilValue(), NilValue(), NilValue(), NilValue()}
		copy(out, res)
		return out, nil
	}
	return []Value{NativeValue(s.builtinNext), args[0], NilValue(), NilValue()}, nil
}

func nativeIPairsIter(s *State, args []Value) ([]Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("bad arguments to ipairs iterator")
	}
	i, ok, err := s.coerceInt(args[1])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #2 to ipairs iterator (integer expected)")
	}
	ni := i + 1
	var v Value
	if args[0].IsTable() {
		t := args[0].AsTable()
		v = t.RawGetInt(ni)
		if v.IsNil() && t.meta != nil {
			var err error
			v, err = s.finishGet(args[0], IntValue(ni))
			if err != nil {
				return nil, err
			}
		}
	} else {
		var err error
		v, err = s.finishGet(args[0], IntValue(ni))
		if err != nil {
			return nil, err
		}
	}
	if v.IsNil() {
		return []Value{NilValue()}, nil
	}
	return []Value{IntValue(ni), v}, nil
}

func nativeIPairs(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'ipairs'")
	}
	return []Value{NativeValue(s.builtinIPairsIter), args[0], IntValue(0)}, nil
}

// --- math ---

func nativeMathFloor(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'floor' (number expected, got no value)")
	}
	if args[0].IsInt() {
		return []Value{args[0]}, nil
	}
	f, ok, err := s.coerceFloat(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'floor' (number expected, got %s)", s.objTypeName(args[0]))
	}
	fl := math.Floor(f)
	if i, ok := FloatToIntExact(fl); ok {
		return []Value{IntValue(i)}, nil
	}
	return []Value{FloatValue(fl)}, nil
}

func nativeMathCeil(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'ceil' (number expected, got no value)")
	}
	if args[0].IsInt() {
		return []Value{args[0]}, nil
	}
	f, ok, err := s.coerceFloat(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'ceil' (number expected, got %s)", s.objTypeName(args[0]))
	}
	cl := math.Ceil(f)
	if i, ok := FloatToIntExact(cl); ok {
		return []Value{IntValue(i)}, nil
	}
	return []Value{FloatValue(cl)}, nil
}

func nativeMathAbs(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'abs'")
	}
	if args[0].IsInt() {
		x := args[0].Int()
		if x < 0 {
			x = -x
		}
		return []Value{IntValue(x)}, nil
	}
	f, ok, err := s.coerceFloat(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'abs' (number expected)")
	}
	return []Value{FloatValue(math.Abs(f))}, nil
}

func nativeMathSqrt(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'sqrt'")
	}
	f, ok, err := s.coerceFloat(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'sqrt' (number expected)")
	}
	return []Value{FloatValue(math.Sqrt(f))}, nil
}

func nativeMathSin(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'sin' (number expected, got no value)")
	}
	f, ok, err := s.coerceFloat(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'sin' (number expected, got %s)", s.objTypeName(args[0]))
	}
	return []Value{FloatValue(math.Sin(f))}, nil
}

func nativeMathCos(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'cos' (number expected, got no value)")
	}
	f, ok, err := s.coerceFloat(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'cos' (number expected, got %s)", args[0].TypeName())
	}
	return []Value{FloatValue(math.Cos(f))}, nil
}

func nativeMathUnaryFloat(fn func(float64) float64) func(*State, []Value) ([]Value, error) {
	return func(s *State, args []Value) ([]Value, error) {
		if len(args) == 0 {
			return nil, fmt.Errorf("bad argument #1 (number expected, got no value)")
		}
		f, ok, err := s.coerceFloat(args[0])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #1 (number expected, got %s)", args[0].TypeName())
		}
		return []Value{FloatValue(fn(f))}, nil
	}
}

func nativeMathAtan(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'atan' (number expected, got no value)")
	}
	y, ok, err := s.coerceFloat(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'atan' (number expected, got %s)", args[0].TypeName())
	}
	if len(args) >= 2 && !args[1].IsNil() {
		x, ok, err := s.coerceFloat(args[1])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'atan' (number expected, got %s)", args[1].TypeName())
		}
		return []Value{FloatValue(math.Atan2(y, x))}, nil
	}
	return []Value{FloatValue(math.Atan(y))}, nil
}

func nativeMathLog(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'log' (number expected, got no value)")
	}
	x, ok, err := s.coerceFloat(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'log' (number expected, got %s)", s.objTypeName(args[0]))
	}
	if len(args) >= 2 && !args[1].IsNil() {
		b, ok, err := s.coerceFloat(args[1])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'log' (number expected, got %s)", s.objTypeName(args[1]))
		}
		if b == 2.0 {
			return []Value{FloatValue(math.Log2(x))}, nil
		}
		if b == 10.0 {
			r := math.Log10(x)
			if !math.IsNaN(r) && !math.IsInf(r, 0) {
				if ri := math.Round(r); ri >= -308 && ri <= 308 && math.Pow10(int(ri)) == x {
					r = ri
				}
			}
			return []Value{FloatValue(r)}, nil
		}
		return []Value{FloatValue(math.Log(x) / math.Log(b))}, nil
	}
	return []Value{FloatValue(math.Log(x))}, nil
}

func nativeMathFmod(s *State, args []Value) ([]Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("bad argument #1 to 'fmod' (number expected, got no value)")
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("bad argument #2 to 'fmod' (number expected, got no value)")
	}
	if BothInt(args[0], args[1]) {
		b := args[1].Int()
		if b == 0 {
			return nil, fmt.Errorf("bad argument #2 to 'fmod' (zero)")
		}
		if b == -1 {
			return []Value{IntValue(0)}, nil
		}
		return []Value{IntValue(args[0].Int() % b)}, nil
	}
	x, ok1, err := s.coerceFloat(args[0])
	if err != nil {
		return nil, err
	}
	if !ok1 {
		return nil, fmt.Errorf("bad argument #1 to 'fmod' (number expected)")
	}
	y, ok2, err := s.coerceFloat(args[1])
	if err != nil {
		return nil, err
	}
	if !ok2 {
		return nil, fmt.Errorf("bad argument #2 to 'fmod' (number expected)")
	}
	return []Value{FloatValue(math.Mod(x, y))}, nil
}

func nativeMathModf(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'modf' (number expected, got no value)")
	}
	if args[0].IsInt() {
		return []Value{args[0], FloatValue(0)}, nil
	}
	f, ok, err := s.coerceFloat(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'modf' (number expected)")
	}
	if math.IsInf(f, 0) {
		return []Value{FloatValue(f), FloatValue(0)}, nil
	}
	ip, fp := math.Modf(f)
	if f == ip {
		fp = 0
	}
	if i, ok := FloatToIntExact(ip); ok {
		return []Value{IntValue(i), FloatValue(fp)}, nil
	}
	return []Value{FloatValue(ip), FloatValue(fp)}, nil
}

func nativeMathFrexp(s *State, args []Value) ([]Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("bad argument #1 to 'frexp' (number expected, got no value)")
	}
	f, ok, err := s.coerceFloat(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'frexp' (number expected)")
	}
	m, e := math.Frexp(f)
	return []Value{FloatValue(m), IntValue(int64(e))}, nil
}

func nativeMathLdexp(s *State, args []Value) ([]Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("bad argument #1 to 'ldexp' (number expected, got no value)")
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("bad argument #2 to 'ldexp' (number expected, got no value)")
	}
	m, ok, err := s.coerceFloat(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'ldexp' (number expected)")
	}
	e, ok, err := s.coerceInt(args[1])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #2 to 'ldexp' (number expected)")
	}
	return []Value{FloatValue(math.Ldexp(m, int(e)))}, nil
}

func nativeMathMin(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'min' (value expected)")
	}
	cancelFn := s.nativeCancelFunc()
	best := args[0]
	for i, a := range args[1:] {
		if cancelFn != nil && ((i+1)&1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		lt, err := s.lessThan(a, best)
		if err != nil {
			return nil, err
		}
		if lt {
			best = a
		}
	}
	return []Value{best}, nil
}

func nativeMathMax(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'max' (value expected)")
	}
	cancelFn := s.nativeCancelFunc()
	best := args[0]
	for i, a := range args[1:] {
		if cancelFn != nil && ((i+1)&1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		lt, err := s.lessThan(best, a)
		if err != nil {
			return nil, err
		}
		if lt {
			best = a
		}
	}
	return []Value{best}, nil
}

func nativeMathToInteger(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'tointeger' (value expected)")
	}
	if i, ok := args[0].ToInt(); ok {
		return []Value{IntValue(i)}, nil
	}
	if args[0].IsString() {
		iv, fv, isInt, ok, err := s.strToNum(args[0].AsString().S)
		if err != nil {
			return nil, err
		}
		if ok {
			if isInt {
				return []Value{IntValue(iv)}, nil
			}
			if i, ok := FloatToIntExact(fv); ok {
				return []Value{IntValue(i)}, nil
			}
		}
	}
	return []Value{NilValue()}, nil
}

func nativeMathType(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'math.type'")
	}
	if args[0].IsInt() {
		return []Value{StringValue(s.intern.Intern("integer"))}, nil
	}
	if args[0].IsFloat() {
		return []Value{StringValue(s.intern.Intern("float"))}, nil
	}
	return []Value{NilValue()}, nil
}

func nativeMathUlt(s *State, args []Value) ([]Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("bad argument #1 to 'ult' (number expected, got no value)")
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("bad argument #2 to 'ult' (number expected, got no value)")
	}
	a, ok1, err := s.coerceInt(args[0])
	if err != nil {
		return nil, err
	}
	if !ok1 {
		return nil, fmt.Errorf("bad argument #1 to 'ult' (number expected)")
	}
	b, ok2, err := s.coerceInt(args[1])
	if err != nil {
		return nil, err
	}
	if !ok2 {
		return nil, fmt.Errorf("bad argument #2 to 'ult' (number expected)")
	}
	return []Value{BoolValue(uint64(a) < uint64(b))}, nil
}

func (s *State) nextRand() uint64 {
	res := bits.RotateLeft64(s.rng[1]*5, 7) * 9
	t := s.rng[1] << 17
	s.rng[2] ^= s.rng[0]
	s.rng[3] ^= s.rng[1]
	s.rng[1] ^= s.rng[2]
	s.rng[0] ^= s.rng[3]
	s.rng[2] ^= t
	s.rng[3] = bits.RotateLeft64(s.rng[3], 45)
	return res
}

func nativeMathRandom(s *State, args []Value) ([]Value, error) {
	r := s.nextRand()
	if len(args) == 0 {
		// 53-bit double in [0, 1)
		return []Value{FloatValue(float64(r>>11) * (1.0 / (1 << 53)))}, nil
	}
	if len(args) > 2 {
		return nil, fmt.Errorf("wrong number of arguments to 'random'")
	}
	var lo, hi int64
	if len(args) == 1 {
		u, ok, err := s.coerceInt(args[0])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #1 to 'random'")
		}
		if u == 0 {
			return []Value{IntValue(int64(r))}, nil
		}
		lo, hi = 1, u
	} else {
		var ok1, ok2 bool
		var err error
		lo, ok1, err = s.coerceInt(args[0])
		if err != nil {
			return nil, err
		}
		hi, ok2, err = s.coerceInt(args[1])
		if err != nil {
			return nil, err
		}
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("bad arguments to 'random'")
		}
	}
	if lo > hi {
		return nil, fmt.Errorf("bad argument to 'random' (interval is empty)")
	}
	// Exact PUC-Rio Lua 5.5 lmathlib.c:project unbiased rejection sampling over [0..n].
	n := uint64(hi) - uint64(lo)
	if n == ^uint64(0) {
		return []Value{IntValue(int64(r + uint64(lo)))}, nil
	}
	lim := n
	for sh := uint(1); (lim & (lim + 1)) != 0; sh *= 2 {
		lim |= lim >> sh
	}
	for {
		p := r & lim
		if p <= n {
			return []Value{IntValue(lo + int64(p))}, nil
		}
		r = s.nextRand()
	}
}

var (
	globalRandSalt    uint64
	globalRandCounter atomic.Uint64
)

func init() {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		globalRandSalt = binary.LittleEndian.Uint64(b[:])
	} else {
		globalRandSalt = uint64(time.Now().UnixNano()) ^ 0x9e3779b97f4a7c15
	}
}

func makeRandomSeed(s *State) uint64 {
	seq := globalRandCounter.Add(1)
	x := globalRandSalt ^ uint64(time.Now().UnixNano()) ^ (seq * 0x9e3779b97f4a7c15) ^ uint64(uintptr(unsafe.Pointer(s)))
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

func (s *State) seedRNG(x, y uint64) {
	s.rng = [4]uint64{x, 0xff, y, 0}
	for range 16 {
		s.nextRand()
	}
}

func nativeMathRandomSeed(s *State, args []Value) ([]Value, error) {
	var x, y uint64
	if len(args) == 0 {
		x = makeRandomSeed(s)
		y = s.nextRand()
	} else {
		v, ok, err := s.coerceInt(args[0])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #1 to 'randomseed' (number expected, got %s)", args[0].TypeName())
		}
		x = uint64(v)
		if len(args) >= 2 && !args[1].IsNil() {
			v2, ok, err := s.coerceInt(args[1])
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("bad argument #2 to 'randomseed' (number expected, got %s)", args[1].TypeName())
			}
			y = uint64(v2)
		}
	}
	s.seedRNG(x, y)
	return []Value{IntValue(int64(x)), IntValue(int64(y))}, nil
}

// --- table ---

func nativeTableCreate(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'create' (number expected, got no value)")
	}
	const maxCInt = (1 << 31) - 1
	nseq, ok, err := s.coerceInt(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'create' (number expected)")
	}
	if nseq < 0 || nseq > maxCInt {
		return nil, fmt.Errorf("bad argument #1 to 'create' (out of range)")
	}
	nrec := int64(0)
	if len(args) >= 2 && !args[1].IsNil() {
		nrec, ok, err = s.coerceInt(args[1])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'create' (number expected)")
		}
		if nrec < 0 || nrec > maxCInt {
			return nil, fmt.Errorf("bad argument #2 to 'create' (out of range)")
		}
	}
	if nrec > (1<<26) || nseq > (1<<28) {
		return nil, fmt.Errorf("table overflow")
	}
	actualBytes := tablePreallocBytes(nseq, nrec)
	checkBytes := actualBytes
	if OptimizeEnabled && nseq > 0 {
		// Enforce requested sequence capacity up front (matching lua_createtable) even when arrHint is lazy.
		checkBytes += nseq * 16
	}
	if err := s.checkMem(checkBytes); err != nil {
		return nil, err
	}
	t := s.NewTable(int(nseq), int(nrec))
	s.gc.allocBytes += checkBytes
	s.gcAddDebt(checkBytes)
	return []Value{TableValue(t)}, nil
}

const (
	tabR  = 1
	tabW  = 2
	tabL  = 4
	tabRW = tabR | tabW
)

// checkTab mirrors Lua 5.5 ltablib.c:checktab, verifying that v is a table or
// has a metatable defining the required raw metamethods (strings satisfy tabL natively).
func (s *State) checkTab(v Value, what int, argNum int, fnName string) error {
	if v.IsTable() {
		return nil
	}
	mt := s.getMetatableOf(v)
	if mt != nil &&
		(what&tabR == 0 || !s.getTM(mt, TMIndex).IsNil()) &&
		(what&tabW == 0 || !s.getTM(mt, TMNewIndex).IsNil()) &&
		(what&tabL == 0 || v.IsString() || !s.getTM(mt, TMLen).IsNil()) {
		return nil
	}
	return fmt.Errorf("bad argument #%d to '%s' (table expected, got %s)", argNum, fnName, s.objTypeName(v))
}

func (s *State) tableLen(v Value) (int64, error) {
	if v.IsString() {
		return int64(len(v.AsString().S)), nil
	}
	if v.IsTable() {
		t := v.AsTable()
		if t.meta == nil {
			return t.Len(), nil
		}
		if tm := s.getTM(t.meta, TMLen); !tm.IsNil() {
			res, err := s.Call(tm, v, v)
			if err != nil {
				return 0, err
			}
			if len(res) > 0 {
				n, ok, err := s.coerceInt(res[0])
				if err != nil {
					return 0, err
				}
				if ok {
					return n, nil
				}
			}
			return 0, fmt.Errorf("object length is not an integer")
		}
		return t.Len(), nil
	}
	if tm := s.getTMByObj(v, TMLen); !tm.IsNil() {
		res, err := s.Call(tm, v, v)
		if err != nil {
			return 0, err
		}
		if len(res) > 0 {
			n, ok, err := s.coerceInt(res[0])
			if err != nil {
				return 0, err
			}
			if ok {
				return n, nil
			}
		}
		return 0, fmt.Errorf("object length is not an integer")
	}
	return 0, fmt.Errorf("attempt to get length of a %s value", v.TypeName())
}

func (s *State) tableGetInt(v Value, idx int64) (Value, error) {
	if v.IsTable() {
		t := v.AsTable()
		r := t.RawGetInt(idx)
		if !r.IsNil() || t.meta == nil {
			return r, nil
		}
	}
	return s.finishGet(v, IntValue(idx))
}

func (s *State) tableSetInt(v Value, idx int64, val Value) error {
	if v.IsTable() {
		t := v.AsTable()
		if !t.RawGetInt(idx).IsNil() || t.meta == nil || s.getTM(t.meta, TMNewIndex).IsNil() {
			t.RawSetInt(s, idx, val)
			return s.ConsumeMemErr()
		}
	}
	cur := v
	key := IntValue(idx)
	for loop := 0; loop < 2000; loop++ {
		var tm Value
		if cur.IsTable() {
			t := cur.AsTable()
			if loop > 0 && !t.RawGet(key).IsNil() {
				return t.RawSet(s, key, val)
			}
			tm = s.getTM(t.meta, TMNewIndex)
			if tm.IsNil() {
				return t.RawSet(s, key, val)
			}
		} else {
			tm = s.getTMByObj(cur, TMNewIndex)
			if tm.IsNil() {
				return fmt.Errorf("attempt to index a %s value", cur.TypeName())
			}
		}
		if tm.IsFunction() {
			_, err := s.Call(tm, cur, key, val)
			return err
		}
		cur = tm
	}
	return fmt.Errorf("'__newindex' chain too long; possible loop")
}

func nativeTableInsert(s *State, args []Value) ([]Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("bad argument #1 to 'insert' (table expected, got no value)")
	}
	if args[0].IsTable() && args[0].AsTable().meta == nil {
		if len(args) < 2 || len(args) > 3 {
			return nil, fmt.Errorf("wrong number of arguments to 'insert'")
		}
		t := args[0].AsTable()
		e := t.Len() + 1
		if len(args) == 2 {
			t.RawSetInt(s, e, args[1])
			return nil, s.ConsumeMemErr()
		}
		pos, ok, err := s.coerceInt(args[1])
		if err != nil {
			return nil, err
		}
		if !ok || pos < 1 || pos > e {
			return nil, fmt.Errorf("bad argument #2 to 'insert' (position out of bounds)")
		}
		cancelFn := s.nativeCancelFunc()
		var steps int64
		for i := e; i > pos; i-- {
			steps++
			if cancelFn != nil && (steps&1023) == 0 {
				if err := cancelFn(); err != nil {
					return nil, err
				}
			}
			t.RawSetInt(s, i, t.RawGetInt(i-1))
			if err := s.ConsumeMemErr(); err != nil {
				return nil, err
			}
		}
		t.RawSetInt(s, pos, args[2])
		return nil, s.ConsumeMemErr()
	}
	if err := s.checkTab(args[0], tabRW|tabL, 1, "insert"); err != nil {
		return nil, err
	}
	n, err := s.tableLen(args[0])
	if err != nil {
		return nil, err
	}
	if len(args) < 2 || len(args) > 3 {
		return nil, fmt.Errorf("wrong number of arguments to 'insert'")
	}
	e := n + 1
	if len(args) == 2 {
		return nil, s.tableSetInt(args[0], e, args[1])
	}
	pos, ok, err := s.coerceInt(args[1])
	if err != nil {
		return nil, err
	}
	if !ok || pos < 1 || pos > e {
		return nil, fmt.Errorf("bad argument #2 to 'insert' (position out of bounds)")
	}
	cancelFn := s.nativeCancelFunc()
	var steps int64
	for i := e; i > pos; i-- {
		steps++
		if cancelFn != nil && (steps&1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		v, err := s.tableGetInt(args[0], i-1)
		if err != nil {
			return nil, err
		}
		if err := s.tableSetInt(args[0], i, v); err != nil {
			return nil, err
		}
	}
	return nil, s.tableSetInt(args[0], pos, args[2])
}

func nativeTableRemove(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'remove' (table expected)")
	}
	cancelFn := s.nativeCancelFunc()
	if args[0].IsTable() && args[0].AsTable().meta == nil {
		t := args[0].AsTable()
		size := t.Len()
		pos := size
		if len(args) >= 2 && !args[1].IsNil() {
			var ok bool
			var err error
			pos, ok, err = s.coerceInt(args[1])
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("bad argument #2 to 'remove' (number expected)")
			}
		}
		if pos != size && (pos < 1 || pos > size+1) {
			return nil, fmt.Errorf("bad argument #2 to 'remove' (position out of bounds)")
		}
		res := t.RawGetInt(pos)
		var steps int64
		for ; pos < size; pos++ {
			steps++
			if cancelFn != nil && (steps&1023) == 0 {
				if err := cancelFn(); err != nil {
					return nil, err
				}
			}
			t.RawSetInt(s, pos, t.RawGetInt(pos+1))
			if err := s.ConsumeMemErr(); err != nil {
				return nil, err
			}
		}
		t.RawSetInt(s, pos, NilValue())
		if err := s.ConsumeMemErr(); err != nil {
			return nil, err
		}
		return []Value{res}, nil
	}
	if err := s.checkTab(args[0], tabRW|tabL, 1, "remove"); err != nil {
		return nil, err
	}
	size, err := s.tableLen(args[0])
	if err != nil {
		return nil, err
	}
	pos := size
	if len(args) >= 2 && !args[1].IsNil() {
		var ok bool
		pos, ok, err = s.coerceInt(args[1])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'remove' (number expected)")
		}
	}
	if pos != size && (pos < 1 || pos > size+1) {
		return nil, fmt.Errorf("bad argument #2 to 'remove' (position out of bounds)")
	}
	res, err := s.tableGetInt(args[0], pos)
	if err != nil {
		return nil, err
	}
	// Root res on the active thread stack across subsequent __index/__newindex callbacks
	// so GC cannot prematurely finalize or clear the removed value before return.
	th := s.cur
	savedTop := th.top
	baseTop := savedTop
	for fi := range th.nframe {
		cf := &th.frames[fi]
		if cf.cl != nil {
			if end := cf.base + int(cf.cl.bp.proto.MaxStackSize); end > baseTop {
				baseTop = end
			}
		}
	}
	if err := s.ensureStackWithRoots(th, baseTop+4, res, nil); err != nil {
		return nil, err
	}
	th.stack[baseTop] = res
	th.top = baseTop + 1
	defer func() {
		th.stack[baseTop] = NilValue()
		th.top = savedTop
	}()
	var steps int64
	for ; pos < size; pos++ {
		steps++
		if cancelFn != nil && (steps&1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		th.top = baseTop + 1
		v, err := s.tableGetInt(args[0], pos+1)
		if err != nil {
			return nil, err
		}
		th.top = baseTop + 1
		if err := s.tableSetInt(args[0], pos, v); err != nil {
			return nil, err
		}
	}
	th.top = baseTop + 1
	if err := s.tableSetInt(args[0], pos, NilValue()); err != nil {
		return nil, err
	}
	return []Value{res}, nil
}

func nativeTableConcat(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'concat' (table expected, got no value)")
	}
	rawFast := args[0].IsTable() && args[0].AsTable().meta == nil
	var defaultLast int64
	if !rawFast {
		if err := s.checkTab(args[0], tabR|tabL, 1, "concat"); err != nil {
			return nil, err
		}
		var err error
		defaultLast, err = s.tableLen(args[0])
		if err != nil {
			return nil, err
		}
	}
	sep := ""
	if len(args) >= 2 && !args[1].IsNil() {
		if !args[1].IsString() && !args[1].IsNumber() {
			return nil, fmt.Errorf("bad argument #2 to 'concat' (string expected)")
		}
		sep = FormatValue(args[1])
	}
	i := int64(1)
	if len(args) >= 3 && !args[2].IsNil() {
		var ok bool
		var err error
		i, ok, err = s.coerceInt(args[2])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'concat' (number expected)")
		}
	}
	var j int64
	if len(args) >= 4 && !args[3].IsNil() {
		var ok bool
		var err error
		j, ok, err = s.coerceInt(args[3])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #4 to 'concat' (number expected)")
		}
	} else if rawFast {
		j = args[0].AsTable().Len()
	} else {
		j = defaultLast
	}
	var sb strings.Builder
	var t *Table
	if rawFast {
		t = args[0].AsTable()
	}
	cancelFn := s.nativeCancelFunc()
	var iterCount int64
	copyWork := 0
	for k := i; k <= j; k++ {
		if cancelFn != nil && (iterCount&1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		iterCount++
		if k > i && sep != "" {
			if s.memLimit > 0 {
				if err := s.checkMem(int64(sb.Len()) + int64(len(sep)) + 48); err != nil {
					return nil, err
				}
			}
			if err := writeStringWithCancel(&sb, sep, cancelFn, &copyWork); err != nil {
				return nil, err
			}
		}
		var v Value
		if rawFast {
			v = t.RawGetInt(k)
		} else {
			var err error
			v, err = s.tableGetInt(args[0], k)
			if err != nil {
				return nil, err
			}
		}
		if !v.IsString() && !v.IsNumber() {
			return nil, fmt.Errorf("invalid value (%s) at index %d in table for 'concat'", v.TypeName(), k)
		}
		piece := FormatValue(v)
		if s.memLimit > 0 {
			if err := s.checkMem(int64(sb.Len()) + int64(len(piece)) + 48); err != nil {
				return nil, err
			}
		}
		if err := writeStringWithCancel(&sb, piece, cancelFn, &copyWork); err != nil {
			return nil, err
		}
		if k == math.MaxInt64 {
			break
		}
	}
	outStr := sb.String()
	if err := s.checkMemAndAccount(int64(len(outStr)) + 48); err != nil {
		return nil, err
	}
	return []Value{StringValue(s.intern.Intern(outStr))}, nil
}

func nativeTableMove(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'move' (table expected, got no value)")
	}
	src := args[0]
	checkMoveInt := func(argIdx int) (int64, error) {
		if argIdx >= len(args) {
			return 0, fmt.Errorf("bad argument #%d to 'move' (number expected, got no value)", argIdx+1)
		}
		v, ok, nonInt, err := s.coerceIntKind(args[argIdx])
		if err != nil {
			return 0, err
		}
		if !ok {
			if nonInt {
				return 0, fmt.Errorf("bad argument #%d to 'move' (number has no integer representation)", argIdx+1)
			}
			return 0, fmt.Errorf("bad argument #%d to 'move' (number expected, got %s)", argIdx+1, s.objTypeName(args[argIdx]))
		}
		return v, nil
	}
	f, err := checkMoveInt(1)
	if err != nil {
		return nil, err
	}
	e, err := checkMoveInt(2)
	if err != nil {
		return nil, err
	}
	tpos, err := checkMoveInt(3)
	if err != nil {
		return nil, err
	}
	dst := src
	dstArg := 1
	if len(args) >= 5 && !args[4].IsNil() {
		dst = args[4]
		dstArg = 5
	}
	if err := s.checkTab(src, tabR, 1, "move"); err != nil {
		return nil, err
	}
	if err := s.checkTab(dst, tabW, dstArg, "move"); err != nil {
		return nil, err
	}
	if e >= f {
		if f <= 0 && e >= math.MaxInt64+f {
			return nil, fmt.Errorf("bad argument #3 to 'move' (too many elements to move)")
		}
		n := e - f + 1
		if tpos > math.MaxInt64-n+1 {
			return nil, fmt.Errorf("bad argument #4 to 'move' (destination wrap around)")
		}
		rawFast := src.IsTable() && src.AsTable().meta == nil && dst.IsTable() && dst.AsTable().meta == nil
		cancelFn := s.nativeCancelFunc()
		reverse := false
		if tpos > f && tpos <= e {
			eq, err := s.equalValues(src, dst)
			if err != nil {
				return nil, err
			}
			reverse = eq
		}
		var rootSlot int
		var savedTop int
		var th *Thread
		if !rawFast {
			th = s.cur
			savedTop = th.top
			rootSlot = savedTop
			for fi := range th.nframe {
				cf := &th.frames[fi]
				if cf.cl != nil {
					if end := cf.base + int(cf.cl.bp.proto.MaxStackSize); end > rootSlot {
						rootSlot = end
					}
				}
			}
			if err := th.ensureStack(rootSlot + 2); err != nil {
				return nil, err
			}
			defer func() {
				th.stack[rootSlot] = NilValue()
				th.top = savedTop
			}()
		}
		if reverse {
			for i := n - 1; i >= 0; i-- {
				if cancelFn != nil && ((n-1-i)&1023) == 0 {
					if err := cancelFn(); err != nil {
						return nil, err
					}
				}
				if rawFast {
					dst.AsTable().RawSetInt(s, tpos+i, src.AsTable().RawGetInt(f+i))
					if err := s.ConsumeMemErr(); err != nil {
						return nil, err
					}
				} else {
					v, err := s.tableGetInt(src, f+i)
					if err != nil {
						return nil, err
					}
					th.stack[rootSlot] = v
					th.top = rootSlot + 1
					if err := s.tableSetInt(dst, tpos+i, v); err != nil {
						return nil, err
					}
				}
			}
		} else {
			for i := int64(0); i < n; i++ {
				if cancelFn != nil && (i&1023) == 0 {
					if err := cancelFn(); err != nil {
						return nil, err
					}
				}
				if rawFast {
					dst.AsTable().RawSetInt(s, tpos+i, src.AsTable().RawGetInt(f+i))
					if err := s.ConsumeMemErr(); err != nil {
						return nil, err
					}
				} else {
					v, err := s.tableGetInt(src, f+i)
					if err != nil {
						return nil, err
					}
					th.stack[rootSlot] = v
					th.top = rootSlot + 1
					if err := s.tableSetInt(dst, tpos+i, v); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return []Value{dst}, nil
}

func nativeTablePack(s *State, args []Value) ([]Value, error) {
	if err := s.checkMemAndAccount(tablePreallocBytes(int64(len(args)), 1)); err != nil {
		return nil, err
	}
	cancelFn := s.nativeCancelFunc()
	t := s.NewTable(len(args), 1)
	for i, a := range args {
		if cancelFn != nil && ((i+1)&1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		t.RawSetInt(s, int64(i+1), a)
		if err := s.ConsumeMemErr(); err != nil {
			return nil, err
		}
	}
	t.RawSetStr(s, s.intern.Intern("n"), IntValue(int64(len(args))))
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return []Value{TableValue(t)}, nil
}

func nativeTableUnpack(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'unpack' (table expected, got no value)")
	}
	rawFast := args[0].IsTable() && args[0].AsTable().meta == nil
	var defaultLen int64
	if !rawFast {
		if err := s.checkTab(args[0], tabR|tabL, 1, "unpack"); err != nil {
			return nil, err
		}
		var err error
		defaultLen, err = s.tableLen(args[0])
		if err != nil {
			return nil, err
		}
	}
	i := int64(1)
	if len(args) >= 2 && !args[1].IsNil() {
		var ok bool
		var err error
		i, ok, err = s.coerceInt(args[1])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'unpack' (number expected)")
		}
	}
	var j int64
	if len(args) >= 3 && !args[2].IsNil() {
		var ok bool
		var err error
		j, ok, err = s.coerceInt(args[2])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'unpack' (number expected)")
		}
	} else if rawFast {
		j = args[0].AsTable().Len()
	} else {
		j = defaultLen
	}
	if i > j {
		return nil, nil
	}
	// Check unsigned range to guard against MaxInt64 - MinInt64 overflow!
	un := uint64(j) - uint64(i)
	if un >= 1000000 {
		return nil, fmt.Errorf("too many results to unpack")
	}
	n := int(un) + 1
	if s.memLimit > 0 {
		if err := s.checkMem(int64(n) * 16); err != nil {
			return nil, err
		}
	}
	if rawFast {
		out := make([]Value, n)
		t := args[0].AsTable()
		cancelFn := s.nativeCancelFunc()
		for k := 0; k < n; k++ {
			if cancelFn != nil && (k&1023) == 0 {
				if err := cancelFn(); err != nil {
					return nil, err
				}
			}
			out[k] = t.RawGetInt(i + int64(k))
		}
		return out, nil
	}
	// Root accumulated results on the active thread stack across __index calls so GC
	// cannot prematurely finalize or clear values already unpacked in earlier iterations.
	th := s.cur
	savedTop := th.top
	baseTop := savedTop
	for fi := range th.nframe {
		cf := &th.frames[fi]
		if cf.cl != nil {
			if end := cf.base + int(cf.cl.bp.proto.MaxStackSize); end > baseTop {
				baseTop = end
			}
		}
	}
	if err := th.ensureStack(baseTop + n + 4); err != nil {
		return nil, err
	}
	written := 0
	defer func() {
		if written > 0 {
			fillNil(th.stack[baseTop : baseTop+written])
		}
		th.top = savedTop
	}()
	cancelFn := s.nativeCancelFunc()
	for k := 0; k < n; k++ {
		if cancelFn != nil && (k&1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		th.top = baseTop + k
		v, err := s.tableGetInt(args[0], i+int64(k))
		if err != nil {
			return nil, err
		}
		th.stack[baseTop+k] = v
		written = k + 1
		th.top = baseTop + written
	}
	out := make([]Value, n)
	copy(out, th.stack[baseTop:baseTop+n])
	return out, nil
}

// sortOrderedCancelable sorts a slice of ordered numeric values in place using introsort
// (median-of-three quicksort + insertion sort + heapsort fallback) while polling cancelFn
// every 1024 comparisons so context timeouts and instruction budgets are enforced promptly.
func sortOrderedCancelable[T cmp.Ordered](a []T, cancelFn func() error) error {
	if len(a) <= 1 {
		return nil
	}
	var steps uint64
	tick := func() error {
		steps++
		if (steps & 1023) == 0 {
			return cancelFn()
		}
		return nil
	}
	insertionSort := func(lo, hi int) error {
		for i := lo + 1; i <= hi; i++ {
			key := a[i]
			j := i - 1
			for j >= lo {
				if err := tick(); err != nil {
					return err
				}
				if !(key < a[j]) {
					break
				}
				a[j+1] = a[j]
				j--
			}
			a[j+1] = key
		}
		return nil
	}
	siftDown := func(lo, root, hi int) error {
		for {
			child := 2*root + 1
			if child > hi {
				return nil
			}
			if child+1 <= hi {
				if err := tick(); err != nil {
					return err
				}
				if a[lo+child] < a[lo+child+1] {
					child++
				}
			}
			if err := tick(); err != nil {
				return err
			}
			if !(a[lo+root] < a[lo+child]) {
				return nil
			}
			a[lo+root], a[lo+child] = a[lo+child], a[lo+root]
			root = child
		}
	}
	heapSort := func(lo, hi int) error {
		n := hi - lo + 1
		for i := n/2 - 1; i >= 0; i-- {
			if err := siftDown(lo, i, n-1); err != nil {
				return err
			}
		}
		for end := n - 1; end > 0; end-- {
			a[lo], a[lo+end] = a[lo+end], a[lo]
			if err := siftDown(lo, 0, end-1); err != nil {
				return err
			}
		}
		return nil
	}
	var introSort func(lo, hi, depth int) error
	introSort = func(lo, hi, depth int) error {
		for hi-lo >= 16 {
			if depth == 0 {
				return heapSort(lo, hi)
			}
			depth--
			mid := lo + (hi-lo)/2
			if a[mid] < a[lo] {
				a[lo], a[mid] = a[mid], a[lo]
			}
			if a[hi] < a[lo] {
				a[lo], a[hi] = a[hi], a[lo]
			}
			if a[hi] < a[mid] {
				a[mid], a[hi] = a[hi], a[mid]
			}
			pivot := a[mid]
			i, j := lo-1, hi+1
			for {
				for {
					i++
					if err := tick(); err != nil {
						return err
					}
					if !(a[i] < pivot) {
						break
					}
				}
				for {
					j--
					if err := tick(); err != nil {
						return err
					}
					if !(pivot < a[j]) {
						break
					}
				}
				if i >= j {
					break
				}
				a[i], a[j] = a[j], a[i]
			}
			if j-lo < hi-j {
				if err := introSort(lo, j, depth); err != nil {
					return err
				}
				lo = j + 1
			} else {
				if err := introSort(j+1, hi, depth); err != nil {
					return err
				}
				hi = j
			}
		}
		return insertionSort(lo, hi)
	}
	return introSort(0, len(a)-1, 2*bits.Len(uint(len(a))))
}

func nativeTableSort(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'sort' (table expected, got no value)")
	}
	if err := s.checkTab(args[0], tabRW|tabL, 1, "sort"); err != nil {
		return nil, err
	}
	hasComp := len(args) >= 2 && !args[1].IsNil()
	cancelFn := s.nativeCancelFunc()
	if args[0].IsTable() && args[0].AsTable().meta == nil {
		t := args[0].AsTable()
		n := int(t.Len())
		if n <= 1 {
			return nil, nil
		}
		// Fast path: homogeneous int64/float64 array without custom comparator!
		if OptimizeEnabled && !hasComp {
			if t.arrKind == arrInt && int(t.arrLen) == n {
				if cancelFn == nil {
					slices.Sort(t.arrIntSlice())
					return nil, nil
				}
				if s.ctx != nil {
					if err := s.ctx.Err(); err != nil {
						s.abortErr = err
						return nil, err
					}
				}
				return nil, sortOrderedCancelable(t.arrIntSlice(), cancelFn)
			}
			if t.arrKind == arrFloat && int(t.arrLen) == n {
				// Fall back to auxsort when NaN is present so Lua 5.5 comparison semantics match PUC-Rio C Lua.
				fs := t.arrFloatSlice()
				if cancelFn != nil && s.ctx != nil {
					if err := s.ctx.Err(); err != nil {
						s.abortErr = err
						return nil, err
					}
				}
				hasNaN := false
				for i, fv := range fs {
					if cancelFn != nil && i > 0 && (i&1023) == 0 {
						if err := cancelFn(); err != nil {
							return nil, err
						}
					}
					if math.IsNaN(fv) {
						hasNaN = true
						break
					}
				}
				if !hasNaN {
					if cancelFn == nil {
						slices.Sort(fs)
						return nil, nil
					}
					return nil, sortOrderedCancelable(fs, cancelFn)
				}
			}
		}
	}
	n64, err := s.tableLen(args[0])
	if err != nil {
		return nil, err
	}
	if n64 >= math.MaxInt32 {
		return nil, fmt.Errorf("array too big")
	}
	n := int(n64)
	if n <= 1 {
		return nil, nil
	}
	if hasComp && !args[1].IsFunction() {
		return nil, fmt.Errorf("bad argument #2 to 'sort' (function expected, got %s)", s.objTypeName(args[1]))
	}
	if s.memLimit > 0 {
		if err := s.checkMem(int64(n) * 16); err != nil {
			return nil, err
		}
	}
	// Root buffered sort elements on the active thread stack across __index, comparator,
	// and __newindex callbacks so GC cannot prematurely finalize or clear them.
	// Always index th.stack[baseTop+...] directly in case callbacks grow th.stack.
	th := s.cur
	savedTop := th.top
	baseTop := savedTop
	for fi := range th.nframe {
		cf := &th.frames[fi]
		if cf.cl != nil {
			if end := cf.base + int(cf.cl.bp.proto.MaxStackSize); end > baseTop {
				baseTop = end
			}
		}
	}
	if err := th.ensureStack(baseTop + n + 4); err != nil {
		return nil, err
	}
	written := 0
	defer func() {
		if written > 0 {
			fillNil(th.stack[baseTop : baseTop+written])
		}
		th.top = savedTop
	}()
	for i := range n {
		if cancelFn != nil && (i&1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		th.top = baseTop + i
		v, err := s.tableGetInt(args[0], int64(i+1))
		if err != nil {
			return nil, err
		}
		th.stack[baseTop+i] = v
		written = i + 1
		th.top = baseTop + written
	}
	var cmpSteps uint64
	comp := func(a, b Value) (bool, error) {
		if cancelFn != nil {
			cmpSteps++
			if (cmpSteps & 1023) == 0 {
				if err := cancelFn(); err != nil {
					return false, err
				}
			}
		}
		th.top = baseTop + n
		if hasComp {
			res, err := s.Call(args[1], a, b)
			if err != nil {
				return false, err
			}
			return len(res) > 0 && !res[0].IsFalsy(), nil
		}
		return s.lessThan(a, b)
	}
	swap := func(i, j int) {
		th.stack[baseTop+i], th.stack[baseTop+j] = th.stack[baseTop+j], th.stack[baseTop+i]
	}
	// Port of PUC-Rio Lua 5.5 ltablib.c:auxsort — detects invalid order functions!
	var auxSort func(lo, up int) error
	auxSort = func(lo, up int) error {
		for lo < up {
			// Sort a[lo], a[up]
			lt, err := comp(th.stack[baseTop+up], th.stack[baseTop+lo])
			if err != nil {
				return err
			}
			if lt {
				swap(lo, up)
			}
			if up-lo == 1 {
				return nil
			}
			// Choose pivot (middle element; randomized on large subarrays).
			p := lo + (up-lo)/2
			lt, err = comp(th.stack[baseTop+p], th.stack[baseTop+lo])
			if err != nil {
				return err
			}
			if lt {
				swap(p, lo)
			} else {
				lt, err = comp(th.stack[baseTop+up], th.stack[baseTop+p])
				if err != nil {
					return err
				}
				if lt {
					swap(p, up)
				}
			}
			if up-lo == 2 {
				return nil
			}
			swap(p, up-1)
			// Partition [lo+1 .. up-2] around pivot (rooted at th.stack[baseTop+up-1])
			i, j := lo, up-1
			for {
				for {
					i++
					lt, err = comp(th.stack[baseTop+i], th.stack[baseTop+up-1])
					if err != nil {
						return err
					}
					if !lt {
						break
					}
					if i == up-1 {
						return fmt.Errorf("invalid order function for sorting")
					}
				}
				for {
					j--
					lt, err = comp(th.stack[baseTop+up-1], th.stack[baseTop+j])
					if err != nil {
						return err
					}
					if !lt {
						break
					}
					if j < i {
						return fmt.Errorf("invalid order function for sorting")
					}
				}
				if j < i {
					break
				}
				swap(i, j)
			}
			swap(up-1, i)
			// Recurse on smaller half, tail-iterate on larger half (O(log n) stack).
			if i-lo < up-i {
				if err := auxSort(lo, i-1); err != nil {
					return err
				}
				lo = i + 1
			} else {
				if err := auxSort(i+1, up); err != nil {
					return err
				}
				up = i - 1
			}
		}
		return nil
	}
	if err := auxSort(0, n-1); err != nil {
		return nil, err
	}
	for i := range n {
		if cancelFn != nil && (i&1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		th.top = baseTop + n
		if err := s.tableSetInt(args[0], int64(i+1), th.stack[baseTop+i]); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// --- string ---

func nativeStringLen(_ *State, args []Value) ([]Value, error) {
	str, err := checkStringArg(args, 0, "len")
	if err != nil {
		return nil, err
	}
	return []Value{IntValue(int64(len(str)))}, nil
}

func nativeStringByte(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, s.badArgErr(1, "byte", "string expected, got no value")
	}
	var str string
	if args[0].IsString() {
		str = args[0].AsString().S
	} else if args[0].IsNumber() {
		str = FormatValue(args[0])
	} else {
		return nil, s.badArgErr(1, "byte", fmt.Sprintf("string expected, got %s", s.objTypeName(args[0])))
	}
	n := int64(len(str))
	i := int64(1)
	if len(args) >= 2 && !args[1].IsNil() {
		var ok, nonInt bool
		var err error
		i, ok, nonInt, err = s.coerceIntKind(args[1])
		if err != nil {
			return nil, err
		}
		if !ok {
			if nonInt {
				return nil, s.badArgErr(2, "byte", "number has no integer representation")
			}
			return nil, s.badArgErr(2, "byte", fmt.Sprintf("number expected, got %s", s.objTypeName(args[1])))
		}
	}
	if i < 0 {
		i = n + i + 1
	}
	j := i
	if len(args) >= 3 && !args[2].IsNil() {
		var ok, nonInt bool
		var err error
		j, ok, nonInt, err = s.coerceIntKind(args[2])
		if err != nil {
			return nil, err
		}
		if !ok {
			if nonInt {
				return nil, s.badArgErr(3, "byte", "number has no integer representation")
			}
			return nil, s.badArgErr(3, "byte", fmt.Sprintf("number expected, got %s", s.objTypeName(args[2])))
		}
		if j < 0 {
			j = n + j + 1
		}
	}
	if i < 1 {
		i = 1
	}
	if j > n {
		j = n
	}
	if i > j {
		return nil, nil
	}
	count := j - i + 1
	if count > 1_000_000 {
		return nil, fmt.Errorf("string slice too long")
	}
	if s.memLimit > 0 {
		if err := s.checkMem(count * 16); err != nil {
			return nil, err
		}
	}
	cancelFn := s.nativeCancelFunc()
	if cancelFn == nil {
		out := make([]Value, count)
		for k := i; k <= j; k++ {
			out[k-i] = IntValue(int64(str[k-1]))
		}
		return out, nil
	}
	out := make([]Value, 0, min(count, 1024))
	var steps int64
	for k := i; k <= j; k++ {
		steps++
		if (steps & 1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		out = append(out, IntValue(int64(str[k-1])))
	}
	return out, nil
}

func nativeStringChar(s *State, args []Value) ([]Value, error) {
	checkByte := func(i int, a Value) (byte, error) {
		v, ok, nonInt, err := s.coerceIntKind(a)
		if err != nil {
			return 0, err
		}
		if !ok {
			if nonInt {
				return 0, fmt.Errorf("bad argument #%d to 'char' (number has no integer representation)", i+1)
			}
			return 0, fmt.Errorf("bad argument #%d to 'char' (number expected, got %s)", i+1, s.objTypeName(a))
		}
		if v < 0 || v > 255 {
			return 0, fmt.Errorf("bad argument #%d to 'char' (value out of range)", i+1)
		}
		return byte(v), nil
	}
	cancelFn := s.nativeCancelFunc()
	if cancelFn != nil {
		if s.memLimit > 0 {
			if err := s.checkMem(int64(len(args)) + 48); err != nil {
				return nil, err
			}
		}
		buf := make([]byte, 0, min(len(args), 1024))
		for i, a := range args {
			if ((i + 1) & 1023) == 0 {
				if err := cancelFn(); err != nil {
					return nil, err
				}
			}
			b, err := checkByte(i, a)
			if err != nil {
				return nil, err
			}
			buf = append(buf, b)
		}
		ls, err := s.TryIntern(string(buf))
		if err != nil {
			return nil, err
		}
		return []Value{StringValue(ls)}, nil
	}
	if err := s.checkMemAndAccount(int64(len(args)) + 48); err != nil {
		return nil, err
	}
	buf := make([]byte, len(args))
	for i, a := range args {
		b, err := checkByte(i, a)
		if err != nil {
			return nil, err
		}
		buf[i] = b
	}
	return []Value{StringValue(s.intern.Intern(string(buf)))}, nil
}

// callerIsMethod reports whether the current native function was invoked via Lua method syntax (OP_SELF).
func (s *State) callerIsMethod() bool {
	if s.cur == nil || s.cur.nframe == 0 {
		return false
	}
	idx := s.cur.nframe - 1
	if s.cur.frames[idx].kind == frameNative {
		idx--
	}
	if idx < 0 {
		return false
	}
	cf := &s.cur.frames[idx]
	if cf.kind == frameLua && cf.cl != nil {
		p := cf.cl.bp.proto
		pc := cf.pc - 1
		if pc >= 0 && pc < len(p.Code) {
			w := p.Code[pc]
			if w.Op() == bytecode.OP_CALL || w.Op() == bytecode.OP_TAILCALL {
				_, kind := findObjectName(p, int(w.A()), pc)
				return kind == "method"
			}
		}
	}
	return false
}

func (s *State) badArgErr(argNum int, fname, detail string) error {
	if s.callerIsMethod() {
		if argNum == 1 {
			return fmt.Errorf("calling '%s' on bad self (%s)", fname, detail)
		}
		argNum--
	}
	return fmt.Errorf("bad argument #%d to '%s' (%s)", argNum, fname, detail)
}

func nativeStringSub(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, s.badArgErr(1, "sub", "string expected, got no value")
	}
	var str string
	if args[0].IsString() {
		str = args[0].AsString().S
	} else if args[0].IsNumber() {
		str = FormatValue(args[0])
	} else {
		return nil, s.badArgErr(1, "sub", fmt.Sprintf("string expected, got %s", s.objTypeName(args[0])))
	}
	n := int64(len(str))
	if len(args) < 2 {
		return nil, s.badArgErr(2, "sub", "number expected, got no value")
	}
	i, ok, nonInt, err := s.coerceIntKind(args[1])
	if err != nil {
		return nil, err
	}
	if !ok {
		if nonInt {
			return nil, s.badArgErr(2, "sub", "number has no integer representation")
		}
		return nil, s.badArgErr(2, "sub", fmt.Sprintf("number expected, got %s", s.objTypeName(args[1])))
	}
	if i < 0 {
		i = n + i + 1
	}
	j := n
	if len(args) >= 3 && !args[2].IsNil() {
		j, ok, nonInt, err = s.coerceIntKind(args[2])
		if err != nil {
			return nil, err
		}
		if !ok {
			if nonInt {
				return nil, s.badArgErr(3, "sub", "number has no integer representation")
			}
			return nil, s.badArgErr(3, "sub", fmt.Sprintf("number expected, got %s", s.objTypeName(args[2])))
		}
		if j < 0 {
			j = n + j + 1
		}
	}
	if i < 1 {
		i = 1
	}
	if j > n {
		j = n
	}
	if i > j {
		return []Value{StringValue(s.intern.Intern(""))}, nil
	}
	sub := str[i-1 : j]
	if len(sub) == len(str) && args[0].IsString() {
		return []Value{args[0]}, nil
	}
	if len(sub) > maxShortStringLen && len(sub) < len(str) {
		if s.memLimit > 0 {
			if err := s.checkMem(int64(len(sub)) + 48); err != nil {
				return nil, err
			}
		}
		cloned, err := s.cloneStringWithCancel(sub)
		if err != nil {
			return nil, err
		}
		ls, err := s.TryIntern(cloned)
		if err != nil {
			return nil, err
		}
		return []Value{StringValue(ls)}, nil
	}
	ls, err := s.TryIntern(sub)
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(ls)}, nil
}

func nativeStringRep(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'rep' (string expected, got no value)")
	}
	var str string
	if args[0].IsString() {
		str = args[0].AsString().S
	} else if args[0].IsNumber() {
		str = FormatValue(args[0])
	} else {
		return nil, fmt.Errorf("bad argument #1 to 'rep' (string expected, got %s)", s.objTypeName(args[0]))
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("bad argument #2 to 'rep' (number expected, got no value)")
	}
	n, ok, nonInt, err := s.coerceIntKind(args[1])
	if err != nil {
		return nil, err
	}
	if !ok {
		if nonInt {
			return nil, fmt.Errorf("bad argument #2 to 'rep' (number has no integer representation)")
		}
		return nil, fmt.Errorf("bad argument #2 to 'rep' (number expected, got %s)", s.objTypeName(args[1]))
	}
	if n <= 0 {
		return []Value{StringValue(s.intern.Intern(""))}, nil
	}
	sep := ""
	if len(args) >= 3 && !args[2].IsNil() {
		if args[2].IsString() {
			sep = args[2].AsString().S
		} else if args[2].IsNumber() {
			sep = FormatValue(args[2])
		} else {
			return nil, fmt.Errorf("bad argument #3 to 'rep' (string expected, got %s)", args[2].TypeName())
		}
	}
	const maxRepSize = 1 << 30 // 1 GiB guard matching checkMem hardSingleAllocLimit
	l := int64(len(str))
	ls := int64(len(sep))
	if l == 0 && ls == 0 {
		return []Value{StringValue(s.intern.Intern(""))}, nil
	}
	cancelFn := s.nativeCancelFunc()
	if l+ls > 0 {
		if n > maxRepSize || (l > 0 && n > maxRepSize/l) || (ls > 0 && n-1 > maxRepSize/ls) || n*l+(n-1)*ls > maxRepSize {
			return nil, fmt.Errorf("resulting string too large")
		}
		totalBytes := n*l + (n-1)*ls
		if cancelFn != nil {
			if s.memLimit > 0 {
				if err := s.checkMem(totalBytes + 48); err != nil {
					return nil, err
				}
			}
			const repPollBytes = 262144
			if totalBytes <= repPollBytes {
				if s.ctx != nil {
					if err := s.ctx.Err(); err != nil {
						s.abortErr = err
						return nil, err
					}
				}
				var out string
				if sep == "" {
					out = strings.Repeat(str, int(n))
				} else {
					var sb strings.Builder
					sb.Grow(int(totalBytes))
					for i := int64(0); i < n; i++ {
						if i > 0 {
							sb.WriteString(sep)
						}
						sb.WriteString(str)
					}
					out = sb.String()
				}
				interned, err := s.TryIntern(out)
				if err != nil {
					return nil, err
				}
				return []Value{StringValue(interned)}, nil
			}
			var sb strings.Builder
			var work int64
			writeChunk := func(chunk string) error {
				if len(chunk) == 0 {
					return nil
				}
				for off := 0; off < len(chunk); {
					rem := repPollBytes - int(work&(repPollBytes-1))
					end := min(off+rem, len(chunk))
					sb.WriteString(chunk[off:end])
					work += int64(end - off)
					off = end
					if (work & (repPollBytes - 1)) == 0 {
						if err := cancelFn(); err != nil {
							return err
						}
					}
				}
				return nil
			}
			if sep == "" {
				unit := str
				unitCount := int64(1)
				for int64(len(unit))*2 <= repPollBytes && unitCount*2 <= n {
					unit += unit
					unitCount *= 2
				}
				remCount := n
				for remCount >= unitCount {
					if err := writeChunk(unit); err != nil {
						return nil, err
					}
					remCount -= unitCount
				}
				for remCount > 0 {
					if err := writeChunk(str); err != nil {
						return nil, err
					}
					remCount--
				}
			} else {
				for i := int64(0); i < n; i++ {
					if i > 0 {
						if err := writeChunk(sep); err != nil {
							return nil, err
						}
					}
					if err := writeChunk(str); err != nil {
						return nil, err
					}
				}
			}
			interned, err := s.TryIntern(sb.String())
			if err != nil {
				return nil, err
			}
			return []Value{StringValue(interned)}, nil
		}
		if err := s.checkMemAndAccount(totalBytes + 48); err != nil {
			return nil, err
		}
	}
	if sep == "" {
		return []Value{StringValue(s.intern.Intern(strings.Repeat(str, int(n))))}, nil
	}
	var sb strings.Builder
	sb.Grow(int(n)*len(str) + int(n-1)*len(sep))
	for i := int64(0); i < n; i++ {
		if i > 0 {
			sb.WriteString(sep)
		}
		sb.WriteString(str)
	}
	return []Value{StringValue(s.intern.Intern(sb.String()))}, nil
}

func nativeStringReverse(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || (!args[0].IsString() && !args[0].IsNumber()) {
		return nil, fmt.Errorf("bad argument #1 to 'reverse' (string expected)")
	}
	str := FormatValue(args[0])
	if cancelFn := s.nativeCancelFunc(); cancelFn != nil {
		if s.memLimit > 0 {
			if err := s.checkMem(int64(len(str)) + 48); err != nil {
				return nil, err
			}
		}
		b := make([]byte, 0, min(len(str), 65536))
		for i := len(str) - 1; i >= 0; i-- {
			b = append(b, str[i])
			if (len(b) & 65535) == 0 {
				if err := cancelFn(); err != nil {
					return nil, err
				}
			}
		}
		ls, err := s.TryIntern(string(b))
		if err != nil {
			return nil, err
		}
		return []Value{StringValue(ls)}, nil
	}
	if err := s.checkMemAndAccount(int64(len(str)) + 48); err != nil {
		return nil, err
	}
	b := []byte(str)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return []Value{StringValue(s.intern.Intern(string(b)))}, nil
}

func nativeStringLower(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || (!args[0].IsString() && !args[0].IsNumber()) {
		return nil, fmt.Errorf("bad argument #1 to 'lower' (string expected)")
	}
	// ASCII-only tolower matching C locale (Lua 5.5 default locale)
	str := FormatValue(args[0])
	if cancelFn := s.nativeCancelFunc(); cancelFn != nil {
		if s.memLimit > 0 {
			if err := s.checkMem(int64(len(str)) + 48); err != nil {
				return nil, err
			}
		}
		b := make([]byte, 0, min(len(str), 65536))
		for i := 0; i < len(str); i++ {
			c := str[i]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			b = append(b, c)
			if ((i + 1) & 65535) == 0 {
				if err := cancelFn(); err != nil {
					return nil, err
				}
			}
		}
		ls, err := s.TryIntern(string(b))
		if err != nil {
			return nil, err
		}
		return []Value{StringValue(ls)}, nil
	}
	if err := s.checkMemAndAccount(int64(len(str)) + 48); err != nil {
		return nil, err
	}
	b := []byte(str)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return []Value{StringValue(s.intern.Intern(string(b)))}, nil
}

func nativeStringUpper(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || (!args[0].IsString() && !args[0].IsNumber()) {
		return nil, fmt.Errorf("bad argument #1 to 'upper' (string expected)")
	}
	str := FormatValue(args[0])
	if cancelFn := s.nativeCancelFunc(); cancelFn != nil {
		if s.memLimit > 0 {
			if err := s.checkMem(int64(len(str)) + 48); err != nil {
				return nil, err
			}
		}
		b := make([]byte, 0, min(len(str), 65536))
		for i := 0; i < len(str); i++ {
			c := str[i]
			if c >= 'a' && c <= 'z' {
				c -= 'a' - 'A'
			}
			b = append(b, c)
			if ((i + 1) & 65535) == 0 {
				if err := cancelFn(); err != nil {
					return nil, err
				}
			}
		}
		ls, err := s.TryIntern(string(b))
		if err != nil {
			return nil, err
		}
		return []Value{StringValue(ls)}, nil
	}
	if err := s.checkMemAndAccount(int64(len(str)) + 48); err != nil {
		return nil, err
	}
	b := []byte(str)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - ('a' - 'A')
		}
	}
	return []Value{StringValue(s.intern.Intern(string(b)))}, nil
}

func nativeOSClock(s *State, _ []Value) ([]Value, error) {
	if cpu, ok := processCPUTime(); ok {
		return []Value{FloatValue(cpu)}, nil
	}
	return []Value{FloatValue(time.Since(s.startTime).Seconds())}, nil
}
