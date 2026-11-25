package vm

import (
	"fmt"
	"math"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/internal/luastdlib"
)

func (s *State) initStringExtraAndUTF8(strTbl *Table) {
	// Pattern matching, binary pack/unpack, and Lua 5.5 string.dump on string library
	s.registerFunc(strTbl, "find", intrNone, nativeStringFind)
	s.registerFunc(strTbl, "match", intrNone, nativeStringMatch)
	s.registerFunc(strTbl, "gmatch", intrNone, nativeStringGMatch)
	s.registerFunc(strTbl, "gsub", intrNone, nativeStringGSub)
	s.registerFunc(strTbl, "pack", intrNone, nativeStringPack)
	s.registerFunc(strTbl, "unpack", intrNone, nativeStringUnpack)
	s.registerFunc(strTbl, "packsize", intrNone, nativeStringPackSize)
	s.registerFunc(strTbl, "dump", intrNone, nativeStringDump)

	// utf8 library (Lua 5.5.1 including two-return utf8.offset!)
	utf8Tbl := s.NewTable(0, 8)
	utf8Tbl.RawSetStr(s, s.intern.Intern("charpattern"), StringValue(s.intern.Intern(luastdlib.CharPattern)))
	s.registerFunc(utf8Tbl, "char", intrNone, nativeUTF8Char)
	s.registerFunc(utf8Tbl, "codepoint", intrNone, nativeUTF8Codepoint)
	s.registerFunc(utf8Tbl, "len", intrNone, nativeUTF8Len)
	s.registerFunc(utf8Tbl, "codes", intrNone, nativeUTF8Codes)
	s.registerFunc(utf8Tbl, "offset", intrNone, nativeUTF8Offset)
	s.SetGlobal("utf8", TableValue(utf8Tbl))
}

func checkStringArg(args []Value, idx int, fnName string) (string, error) {
	if idx >= len(args) {
		return "", fmt.Errorf("bad argument #%d to '%s' (string expected, got no value)", idx+1, fnName)
	}
	v := args[idx]
	if v.IsString() {
		return v.AsString().S, nil
	}
	if v.IsNumber() {
		return FormatValue(v), nil
	}
	return "", fmt.Errorf("bad argument #%d to '%s' (string expected, got %s)", idx+1, fnName, v.TypeName())
}

func captureToValue(s *State, c luastdlib.CaptureVal) Value {
	if c.IsPos {
		return IntValue(c.Pos)
	}
	return StringValue(s.intern.Intern(c.Str))
}

func nativeStringFind(s *State, args []Value) ([]Value, error) {
	str, err := checkStringArg(args, 0, "find")
	if err != nil {
		return nil, err
	}
	pat, err := checkStringArg(args, 1, "find")
	if err != nil {
		return nil, err
	}
	initPos := 1
	if len(args) >= 3 && !args[2].IsNil() {
		iv, ok := args[2].ToInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'find' (number expected)")
		}
		initPos = int(iv)
	}
	plain := len(args) >= 4 && !args[3].IsFalsy()
	start, end, caps, found, err := luastdlib.FindWithCancel(str, pat, initPos, plain, s.nativeCancelFunc())
	if err != nil {
		return nil, err
	}
	if !found {
		return []Value{NilValue()}, nil
	}
	out := make([]Value, 2+len(caps))
	out[0] = IntValue(int64(start))
	out[1] = IntValue(int64(end))
	for i, c := range caps {
		out[2+i] = captureToValue(s, c)
	}
	return out, nil
}

func nativeStringMatch(s *State, args []Value) ([]Value, error) {
	str, err := checkStringArg(args, 0, "match")
	if err != nil {
		return nil, err
	}
	pat, err := checkStringArg(args, 1, "match")
	if err != nil {
		return nil, err
	}
	initPos := 1
	if len(args) >= 3 && !args[2].IsNil() {
		iv, ok := args[2].ToInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'match' (number expected)")
		}
		initPos = int(iv)
	}
	caps, found, err := luastdlib.MatchWithCancel(str, pat, initPos, s.nativeCancelFunc())
	if err != nil {
		return nil, err
	}
	if !found {
		return []Value{NilValue()}, nil
	}
	out := make([]Value, len(caps))
	for i, c := range caps {
		out[i] = captureToValue(s, c)
	}
	return out, nil
}

func nativeStringGMatch(s *State, args []Value) ([]Value, error) {
	str, err := checkStringArg(args, 0, "gmatch")
	if err != nil {
		return nil, err
	}
	pat, err := checkStringArg(args, 1, "gmatch")
	if err != nil {
		return nil, err
	}
	initPos := 1
	if len(args) >= 3 && !args[2].IsNil() {
		iv, ok := args[2].ToInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'gmatch' (number expected)")
		}
		initPos = int(iv)
	}
	it := luastdlib.GMatchWithCancel(str, pat, initPos, s.nativeCancelFunc())
	fn := &NativeFunc{
		Name:        "gmatch_iter",
		NumUpvals:   3,
		Collectable: true,
		Fn: func(s *State, _ []Value) ([]Value, error) {
			caps, ok, err := it.Next()
			if err != nil {
				return nil, err
			}
			if !ok {
				return []Value{NilValue()}, nil
			}
			out := make([]Value, len(caps))
			for i, c := range caps {
				out[i] = captureToValue(s, c)
			}
			return out, nil
		},
	}
	return []Value{NativeValue(fn)}, nil
}

func nativeStringGSub(s *State, args []Value) ([]Value, error) {
	str, err := checkStringArg(args, 0, "gsub")
	if err != nil {
		return nil, err
	}
	pat, err := checkStringArg(args, 1, "gsub")
	if err != nil {
		return nil, err
	}
	if len(args) < 3 {
		return nil, fmt.Errorf("bad argument #3 to 'gsub' (string/function/table expected)")
	}
	repl := args[2]
	maxRepl := -1
	if len(args) >= 4 && !args[3].IsNil() {
		iv, ok := args[3].ToInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #4 to 'gsub' (number expected)")
		}
		if iv < 0 {
			maxRepl = 0
		} else {
			maxRepl = int(iv)
		}
	}

	var outStr string
	var count int
	replaced := false
	cancelFn := s.nativeCancelFunc()
	switch {
	case repl.IsString() || repl.IsNumber():
		outStr, count, err = luastdlib.GSubStringWithCancel(str, pat, FormatValue(repl), maxRepl, cancelFn)
		replaced = count > 0
	case repl.IsTable():
		outStr, count, err = luastdlib.GSubTableWithCancel(str, pat, func(key luastdlib.CaptureVal) (string, bool, error) {
			kv := captureToValue(s, key)
			var val Value
			t := repl.AsTable()
			val = t.RawGet(kv)
			if val.IsNil() && t.meta != nil {
				var gerr error
				val, gerr = s.finishGet(repl, kv)
				if gerr != nil {
					return "", false, gerr
				}
			}
			if val.IsFalsy() {
				return "", false, nil
			}
			if val.IsString() || val.IsNumber() {
				replaced = true
				return FormatValue(val), true, nil
			}
			return "", false, fmt.Errorf("invalid replacement value (a %s)", val.TypeName())
		}, maxRepl, cancelFn)
	case repl.IsFunction():
		outStr, count, err = luastdlib.GSubWithCancel(str, pat, func(caps []luastdlib.CaptureVal) (string, bool, error) {
			cargs := make([]Value, len(caps))
			for i, c := range caps {
				cargs[i] = captureToValue(s, c)
			}
			res, cerr := s.Call(repl, cargs...)
			if cerr != nil {
				return "", false, cerr
			}
			if len(res) == 0 || res[0].IsFalsy() {
				return "", false, nil
			}
			if res[0].IsString() || res[0].IsNumber() {
				replaced = true
				return FormatValue(res[0]), true, nil
			}
			return "", false, fmt.Errorf("invalid replacement value (a %s)", res[0].TypeName())
		}, maxRepl, cancelFn)
	default:
		return nil, fmt.Errorf("bad argument #3 to 'gsub' (string/function/table expected, got %s)", repl.TypeName())
	}
	if err != nil {
		return nil, err
	}
	if !replaced && args[0].IsString() {
		return []Value{args[0], IntValue(int64(count))}, nil
	}
	return []Value{StringValue(s.intern.Intern(outStr)), IntValue(int64(count))}, nil
}

func nativeStringPack(s *State, args []Value) ([]Value, error) {
	fmtStr, err := checkStringArg(args, 0, "pack")
	if err != nil {
		return nil, err
	}
	pargs := make([]luastdlib.PackValue, len(args)-1)
	for i, a := range args[1:] {
		switch {
		case a.IsInt():
			pargs[i] = luastdlib.PackIntVal(a.Int())
		case a.IsFloat():
			pargs[i] = luastdlib.PackFloatVal(a.Float())
		case a.IsString():
			pargs[i] = luastdlib.PackStringVal(a.AsString().S)
		default:
			pargs[i] = luastdlib.PackStringVal(FormatValue(a))
		}
	}
	res, err := luastdlib.Pack(fmtStr, pargs...)
	if err != nil {
		return nil, err
	}
	if err := s.checkMemAndAccount(int64(len(res)) + 48); err != nil {
		return nil, err
	}
	return []Value{StringValue(s.intern.Intern(res))}, nil
}

func nativeStringUnpack(s *State, args []Value) ([]Value, error) {
	fmtStr, err := checkStringArg(args, 0, "unpack")
	if err != nil {
		return nil, err
	}
	data, err := checkStringArg(args, 1, "unpack")
	if err != nil {
		return nil, err
	}
	initPos := int64(1)
	if len(args) >= 3 && !args[2].IsNil() {
		var ok bool
		initPos, ok = args[2].ToInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'unpack' (number expected)")
		}
	}
	vals, nextPos, err := luastdlib.Unpack(fmtStr, data, initPos)
	if err != nil {
		return nil, err
	}
	out := make([]Value, len(vals)+1)
	for i, pv := range vals {
		switch pv.Kind {
		case luastdlib.PackInt:
			out[i] = IntValue(pv.Int)
		case luastdlib.PackFloat:
			out[i] = FloatValue(pv.Flt)
		case luastdlib.PackString:
			out[i] = StringValue(s.intern.Intern(pv.Str))
		}
	}
	out[len(vals)] = IntValue(nextPos)
	return out, nil
}

func nativeStringPackSize(_ *State, args []Value) ([]Value, error) {
	fmtStr, err := checkStringArg(args, 0, "packsize")
	if err != nil {
		return nil, err
	}
	sz, err := luastdlib.PackSize(fmtStr)
	if err != nil {
		return nil, err
	}
	return []Value{IntValue(sz)}, nil
}

func nativeStringFormatStdlib(s *State, args []Value) ([]Value, error) {
	fmtStr, err := checkStringArg(args, 0, "format")
	if err != nil {
		return nil, err
	}
	fargs := make([]luastdlib.FormatArg, len(args)-1)
	for i, a := range args[1:] {
		switch {
		case a.IsNil():
			fargs[i] = luastdlib.NilArg()
		case a.IsBool():
			fargs[i] = luastdlib.BoolArg(a.IsTrue())
		case a.IsInt():
			fargs[i] = luastdlib.IntArg(a.Int())
		case a.IsFloat():
			fargs[i] = luastdlib.FloatArg(a.Float())
		case a.IsString():
			fargs[i] = luastdlib.FormatArg{Kind: luastdlib.ArgString, Str: a.AsString().S, Ptr: uintptr(a.ptr)}
		default:
			// Support __tostring and __name for %s, and pointer address for %p.
			str, terr := s.toStringValue(a)
			if terr != nil {
				return nil, terr
			}
			fargs[i] = luastdlib.FormatArg{Kind: luastdlib.ArgPointer, Str: str, Ptr: uintptr(a.ptr)}
		}
	}
	res, err := luastdlib.Format(fmtStr, fargs)
	if err != nil {
		return nil, err
	}
	if err := s.checkMemAndAccount(int64(len(res)) + 48); err != nil {
		return nil, err
	}
	return []Value{StringValue(s.intern.Intern(res))}, nil
}

// --- utf8 library ---

func nativeUTF8Char(s *State, args []Value) ([]Value, error) {
	cps := make([]int64, len(args))
	for i, a := range args {
		cp, ok := a.ToInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #%d to 'utf8.char' (number expected)", i+1)
		}
		cps[i] = cp
	}
	res, err := luastdlib.Char(cps, true)
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(s.intern.Intern(res))}, nil
}

func nativeUTF8Codepoint(_ *State, args []Value) ([]Value, error) {
	str, err := checkStringArg(args, 0, "codepoint")
	if err != nil {
		return nil, err
	}
	i := int64(1)
	if len(args) >= 2 && !args[1].IsNil() {
		var ok bool
		i, ok = args[1].ToInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'codepoint'")
		}
	}
	j := i
	if len(args) >= 3 && !args[2].IsNil() {
		var ok bool
		j, ok = args[2].ToInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'codepoint'")
		}
	}
	lax := len(args) >= 4 && !args[3].IsFalsy()
	cps, err := luastdlib.Codepoint(str, i, j, lax)
	if err != nil {
		return nil, err
	}
	out := make([]Value, len(cps))
	for k, cp := range cps {
		out[k] = IntValue(cp)
	}
	return out, nil
}

func nativeUTF8Len(_ *State, args []Value) ([]Value, error) {
	str, err := checkStringArg(args, 0, "len")
	if err != nil {
		return nil, err
	}
	i := int64(1)
	if len(args) >= 2 && !args[1].IsNil() {
		var ok bool
		i, ok = args[1].ToInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'len'")
		}
	}
	j := int64(-1)
	if len(args) >= 3 && !args[2].IsNil() {
		var ok bool
		j, ok = args[2].ToInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'len'")
		}
	}
	lax := len(args) >= 4 && !args[3].IsFalsy()
	cnt, errPos, err := luastdlib.Len(str, i, j, lax)
	if err != nil {
		return nil, err
	}
	if errPos > 0 {
		return []Value{NilValue(), IntValue(errPos)}, nil
	}
	return []Value{IntValue(cnt)}, nil
}

func nativeUTF8Codes(s *State, args []Value) ([]Value, error) {
	str, err := checkStringArg(args, 0, "codes")
	if err != nil {
		return nil, err
	}
	if err := luastdlib.ValidCodesInit(str); err != nil {
		return nil, err
	}
	lax := len(args) >= 2 && !args[1].IsFalsy()
	iter := &NativeFunc{
		Name:        "utf8_codes_iter",
		Collectable: true,
		Fn: func(_ *State, iargs []Value) ([]Value, error) {
			sv, _ := checkStringArg(iargs, 0, "codes")
			pos := int64(0)
			if len(iargs) >= 2 && !iargs[1].IsNil() {
				pos, _ = iargs[1].ToInt()
			}
			nextPos, cp, ok, err := luastdlib.Next(sv, pos, lax)
			if err != nil {
				return nil, err
			}
			if !ok {
				return []Value{NilValue()}, nil
			}
			return []Value{IntValue(nextPos), IntValue(cp)}, nil
		},
	}
	return []Value{NativeValue(iter), StringValue(s.intern.Intern(str)), IntValue(0)}, nil
}

// nativeUTF8Offset implements Lua 5.5.1 two-return utf8.offset(s, n [, i]) -> start, end!
func nativeUTF8Offset(_ *State, args []Value) ([]Value, error) {
	str, err := checkStringArg(args, 0, "offset")
	if err != nil {
		return nil, err
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("bad argument #2 to 'offset' (number expected)")
	}
	n, ok := args[1].ToInt()
	if !ok {
		return nil, fmt.Errorf("bad argument #2 to 'offset' (number expected)")
	}
	var start, end int64
	var found bool
	if len(args) >= 3 && !args[2].IsNil() {
		i, ok := args[2].ToInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'offset' (number expected)")
		}
		start, end, found, err = luastdlib.Offset(str, n, i)
	} else {
		start, end, found, err = luastdlib.OffsetDefault(str, n)
	}
	if err != nil {
		return nil, err
	}
	if !found {
		return []Value{NilValue()}, nil
	}
	return []Value{IntValue(start), IntValue(end)}, nil
}

func nativeStringDump(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsClosure() {
		return nil, fmt.Errorf("bad argument #1 to 'dump' (Lua function expected)")
	}
	strip := len(args) >= 2 && !args[1].IsFalsy()
	data, err := bytecode.Dump(args[0].AsClosure().bp.proto, strip)
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(s.intern.Intern(string(data)))}, nil
}

// Unused math import guard if needed
var _ = math.IsNaN
