package vm

import (
	"fmt"
	"math"
	"strings"

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

func captureToValue(s *State, c luastdlib.CaptureVal, srcLen int) (Value, error) {
	if c.IsPos {
		return IntValue(c.Pos), nil
	}
	str := c.Str
	if len(str) > maxShortStringLen && len(str) < srcLen {
		str = strings.Clone(str)
	}
	ls, err := s.TryIntern(str)
	if err != nil {
		return NilValue(), err
	}
	return StringValue(ls), nil
}

// capturesToValuesRooted converts pattern captures into Lua Values, rooting accumulated
// values on the active thread stack whenever a memory limit is active and multiple captures
// are converted so emergency GC during a later capture still accounts for earlier strings.
func (s *State) capturesToValuesRooted(caps []luastdlib.CaptureVal, srcLen int, prefix ...Value) ([]Value, error) {
	total := len(prefix) + len(caps)
	if total == 0 {
		return nil, nil
	}
	if s.memLimit > 0 {
		if err := s.checkMem(int64(total) * 16); err != nil {
			return nil, err
		}
	}
	if s.memLimit > 0 && len(caps) > 1 {
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
		if err := th.ensureStack(baseTop + total + 2); err != nil {
			return nil, err
		}
		written := 0
		defer func() {
			if written > 0 {
				fillNil(th.stack[baseTop : baseTop+written])
			}
			th.top = savedTop
		}()
		for _, pv := range prefix {
			th.stack[baseTop+written] = pv
			written++
		}
		th.top = baseTop + written
		for _, c := range caps {
			cv, cerr := captureToValue(s, c, srcLen)
			if cerr != nil {
				return nil, cerr
			}
			th.stack[baseTop+written] = cv
			written++
			th.top = baseTop + written
		}
		out := make([]Value, written)
		copy(out, th.stack[baseTop:baseTop+written])
		return out, nil
	}
	out := make([]Value, total)
	copy(out, prefix)
	for i, c := range caps {
		cv, cerr := captureToValue(s, c, srcLen)
		if cerr != nil {
			return nil, cerr
		}
		out[len(prefix)+i] = cv
	}
	return out, nil
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
		iv, ok := args[2].CoerceInt()
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
	return s.capturesToValuesRooted(caps, len(str), IntValue(int64(start)), IntValue(int64(end)))
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
		iv, ok := args[2].CoerceInt()
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
	return s.capturesToValuesRooted(caps, len(str))
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
		iv, ok := args[2].CoerceInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'gmatch' (number expected)")
		}
		initPos = int(iv)
	}
	var strVal Value
	if args[0].IsString() {
		strVal = args[0]
	} else {
		ls, ierr := s.TryIntern(str)
		if ierr != nil {
			return nil, ierr
		}
		strVal = StringValue(ls)
		str = ls.S
	}
	var patVal Value
	if args[1].IsString() {
		patVal = args[1]
	} else {
		ls, ierr := s.TryIntern(pat)
		if ierr != nil {
			return nil, ierr
		}
		patVal = StringValue(ls)
		pat = ls.S
	}
	if s.memLimit > 0 || s.gc.tier1 {
		if err := s.checkMemAndAccount(48 + 3*16 + 64); err != nil {
			return nil, err
		}
	}
	it := luastdlib.GMatchWithCancel(str, pat, initPos, s.dynamicNativeCancelFunc())
	stateUD := UserdataValue(&Userdata{Data: it})
	fn := &NativeFunc{
		Name:        "gmatch_iter",
		NumUpvals:   3,
		Collectable: true,
		upvals:      []Value{strVal, patVal, stateUD},
		Fn: func(s *State, _ []Value) ([]Value, error) {
			caps, ok, err := it.Next()
			if err != nil {
				return nil, err
			}
			if !ok {
				return []Value{NilValue()}, nil
			}
			return s.capturesToValuesRooted(caps, len(str))
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
		iv, ok := args[3].CoerceInt()
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
	var checkSize func(int64) error
	if s.memLimit > 0 {
		checkSize = func(n int64) error {
			return s.checkMem(n + 48)
		}
	}
	switch {
	case repl.IsString() || repl.IsNumber():
		outStr, count, err = luastdlib.GSubStringWithLimits(str, pat, FormatValue(repl), maxRepl, cancelFn, checkSize)
		replaced = count > 0
	case repl.IsTable():
		outStr, count, err = luastdlib.GSubTableWithLimits(str, pat, func(key luastdlib.CaptureVal) (string, bool, error) {
			kv, kerr := captureToValue(s, key, len(str))
			if kerr != nil {
				return "", false, kerr
			}
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
		}, maxRepl, cancelFn, checkSize)
	case repl.IsFunction():
		outStr, count, err = luastdlib.GSubWithLimits(str, pat, func(caps []luastdlib.CaptureVal) (string, bool, error) {
			cargs, cerr := s.capturesToValuesRooted(caps, len(str))
			if cerr != nil {
				return "", false, cerr
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
		}, maxRepl, cancelFn, checkSize)
	default:
		return nil, fmt.Errorf("bad argument #3 to 'gsub' (string/function/table expected, got %s)", repl.TypeName())
	}
	if err != nil {
		return nil, err
	}
	if !replaced && args[0].IsString() {
		return []Value{args[0], IntValue(int64(count))}, nil
	}
	ls, err := s.TryIntern(outStr)
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(ls), IntValue(int64(count))}, nil
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
			// Preserve an invalid PackKind so PackWithLimits rejects unsupported values iff consumed,
			// without rejecting unused trailing arguments.
			pargs[i] = luastdlib.PackValue{Kind: ^luastdlib.PackKind(0)}
		}
	}
	var checkSize func(int64) error
	if s.memLimit > 0 {
		checkSize = func(n int64) error {
			return s.checkMem(n + 48)
		}
	}
	res, err := luastdlib.PackWithControls(fmtStr, checkSize, s.nativeCancelFunc(), pargs...)
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
		initPos, ok = args[2].CoerceInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'unpack' (number expected)")
		}
	}
	// Pass 1: validate format/data and count decoded outputs with zero slice allocations,
	// enforcing memLimit and cancellation controls before allocating any result storage.
	cancelFn := s.nativeCancelFunc()
	var count int64
	var strBytes int64
	nextPos, err := luastdlib.UnpackEachWithCancel(fmtStr, data, initPos, cancelFn, func(pv luastdlib.PackValue) error {
		count++
		if s.memLimit > 0 {
			checkNow := (count & 63) == 0
			if pv.Kind == luastdlib.PackString {
				if _, exists := s.intern.lookupExistingShort(pv.Str); !exists {
					strBytes += int64(len(pv.Str)) + 48
					checkNow = true
				}
			}
			if checkNow {
				if err := s.checkMem((count+1)*16 + strBytes); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.memLimit > 0 {
		if err := s.checkMem((count+1)*16 + strBytes); err != nil {
			return nil, err
		}
	}
	// Pass 2: fill a single exact-sized []Value slice (rooting on the active thread stack
	// when strings are interned under a memory limit so emergency GC recounts earlier strings).
	nOut := int(count) + 1
	if s.memLimit > 0 && strBytes > 0 {
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
		if err := th.ensureStack(baseTop + nOut + 2); err != nil {
			return nil, err
		}
		written := 0
		defer func() {
			if written > 0 {
				fillNil(th.stack[baseTop : baseTop+written])
			}
			th.top = savedTop
		}()
		_, err = luastdlib.UnpackEachWithCancel(fmtStr, data, initPos, cancelFn, func(pv luastdlib.PackValue) error {
			var v Value
			switch pv.Kind {
			case luastdlib.PackInt:
				v = IntValue(pv.Int)
			case luastdlib.PackFloat:
				v = FloatValue(pv.Flt)
			case luastdlib.PackString:
				str := pv.Str
				if len(str) > maxShortStringLen && len(str) < len(data) {
					str = strings.Clone(str)
				}
				ls, ierr := s.TryIntern(str)
				if ierr != nil {
					return ierr
				}
				v = StringValue(ls)
			}
			th.stack[baseTop+written] = v
			written++
			th.top = baseTop + written
			return nil
		})
		if err != nil {
			return nil, err
		}
		th.stack[baseTop+written] = IntValue(nextPos)
		written++
		th.top = baseTop + written
		out := make([]Value, written)
		copy(out, th.stack[baseTop:baseTop+written])
		return out, nil
	}
	out := make([]Value, nOut)
	idx := 0
	_, err = luastdlib.UnpackEachWithCancel(fmtStr, data, initPos, cancelFn, func(pv luastdlib.PackValue) error {
		switch pv.Kind {
		case luastdlib.PackInt:
			out[idx] = IntValue(pv.Int)
		case luastdlib.PackFloat:
			out[idx] = FloatValue(pv.Flt)
		case luastdlib.PackString:
			str := pv.Str
			if len(str) > maxShortStringLen && len(str) < len(data) {
				str = strings.Clone(str)
			}
			ls, ierr := s.TryIntern(str)
			if ierr != nil {
				return ierr
			}
			out[idx] = StringValue(ls)
		}
		idx++
		return nil
	})
	if err != nil {
		return nil, err
	}
	out[idx] = IntValue(nextPos)
	return out, nil
}

func nativeStringPackSize(s *State, args []Value) ([]Value, error) {
	fmtStr, err := checkStringArg(args, 0, "packsize")
	if err != nil {
		return nil, err
	}
	sz, err := luastdlib.PackSizeWithCancel(fmtStr, s.nativeCancelFunc())
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
		val := a
		lazyToStr := func() (string, error) {
			return s.toStringValue(val)
		}
		hasToStrTM := !s.getTMByObj(a, TMToString).IsNil()
		switch {
		case a.IsNil():
			fa := luastdlib.NilArg()
			if hasToStrTM {
				fa.ToString = lazyToStr
			}
			fargs[i] = fa
		case a.IsBool():
			fa := luastdlib.BoolArg(a.IsTrue())
			if hasToStrTM {
				fa.ToString = lazyToStr
			}
			fargs[i] = fa
		case a.IsInt():
			fa := luastdlib.IntArg(a.Int())
			if hasToStrTM {
				fa.ToString = lazyToStr
			}
			fargs[i] = fa
		case a.IsFloat():
			fa := luastdlib.FloatArg(a.Float())
			if hasToStrTM {
				fa.ToString = lazyToStr
			}
			fargs[i] = fa
		case a.IsString():
			fa := luastdlib.FormatArg{Kind: luastdlib.ArgString, Str: a.AsString().S, Ptr: uintptr(a.ptr)}
			if hasToStrTM {
				fa.ToString = lazyToStr
			}
			fargs[i] = fa
		default:
			// Defer __tostring / __name evaluation until a %s conversion actually consumes this argument.
			fargs[i] = luastdlib.FormatArg{Kind: luastdlib.ArgPointer, Ptr: uintptr(a.ptr), ToString: lazyToStr}
		}
	}
	var checkSize func(int64) error
	if s.memLimit > 0 {
		checkSize = func(n int64) error {
			return s.checkMem(n + 48)
		}
	}
	res, err := luastdlib.FormatWithControls(fmtStr, fargs, checkSize, s.nativeCancelFunc())
	if err != nil {
		return nil, err
	}
	ls, err := s.TryIntern(res)
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(ls)}, nil
}

// --- utf8 library ---

func nativeUTF8Char(s *State, args []Value) ([]Value, error) {
	if s.memLimit > 0 && len(args) > 0 {
		// Check budget before allocating the transient []int64 and encoded UTF-8 buffer.
		if err := s.checkMem(int64(len(args))*9 + 48); err != nil {
			return nil, err
		}
	}
	cps := make([]int64, len(args))
	for i, a := range args {
		cp, ok := a.CoerceInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #%d to 'utf8.char' (number expected)", i+1)
		}
		cps[i] = cp
	}
	res, err := luastdlib.Char(cps, true)
	if err != nil {
		return nil, err
	}
	ls, err := s.TryIntern(res)
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(ls)}, nil
}

func nativeUTF8Codepoint(s *State, args []Value) ([]Value, error) {
	str, err := checkStringArg(args, 0, "codepoint")
	if err != nil {
		return nil, err
	}
	i := int64(1)
	if len(args) >= 2 && !args[1].IsNil() {
		var ok bool
		i, ok = args[1].CoerceInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'codepoint'")
		}
	}
	j := i
	if len(args) >= 3 && !args[2].IsNil() {
		var ok bool
		j, ok = args[2].CoerceInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'codepoint'")
		}
	}
	lax := len(args) >= 4 && !args[3].IsFalsy()
	// Pass 1: validate UTF-8 and count codepoints with zero allocations, stopping early
	// before any slice allocation if the final []Value result would exceed memLimit.
	cancelFn := s.nativeCancelFunc()
	var count int64
	if err := luastdlib.CodepointEach(str, i, j, lax, func(_ int64) error {
		count++
		if cancelFn != nil && (count&1023) == 0 {
			if cerr := cancelFn(); cerr != nil {
				return cerr
			}
		}
		if s.memLimit > 0 && count&63 == 0 {
			if err := s.checkMem(count * 16); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	if s.memLimit > 0 {
		if err := s.checkMem(count * 16); err != nil {
			return nil, err
		}
	}
	// Pass 2: fill a single exact-sized []Value slice with no transient []int64 allocation.
	out := make([]Value, 0, count)
	if err := luastdlib.CodepointEach(str, i, j, lax, func(cp int64) error {
		out = append(out, IntValue(cp))
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func nativeUTF8Len(s *State, args []Value) ([]Value, error) {
	str, err := checkStringArg(args, 0, "len")
	if err != nil {
		return nil, err
	}
	i := int64(1)
	if len(args) >= 2 && !args[1].IsNil() {
		var ok bool
		i, ok = args[1].CoerceInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'len'")
		}
	}
	j := int64(-1)
	if len(args) >= 3 && !args[2].IsNil() {
		var ok bool
		j, ok = args[2].CoerceInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'len'")
		}
	}
	lax := len(args) >= 4 && !args[3].IsFalsy()
	cnt, errPos, err := luastdlib.LenWithCancel(str, i, j, lax, s.nativeCancelFunc())
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
	var strVal Value
	if args[0].IsString() {
		strVal = args[0]
	} else {
		ls, ierr := s.TryIntern(str)
		if ierr != nil {
			return nil, ierr
		}
		strVal = StringValue(ls)
	}
	if s.memLimit > 0 {
		if err := s.checkMemAndAccount(48); err != nil {
			return nil, err
		}
	}
	iter := &NativeFunc{
		Name:        "utf8_codes_iter",
		Collectable: true,
		Fn: func(_ *State, iargs []Value) ([]Value, error) {
			sv, _ := checkStringArg(iargs, 0, "codes")
			pos := int64(0)
			if len(iargs) >= 2 && !iargs[1].IsNil() {
				pos, _ = iargs[1].CoerceInt()
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
	return []Value{NativeValue(iter), strVal, IntValue(0)}, nil
}

// nativeUTF8Offset implements Lua 5.5.1 two-return utf8.offset(s, n [, i]) -> start, end!
func nativeUTF8Offset(s *State, args []Value) ([]Value, error) {
	str, err := checkStringArg(args, 0, "offset")
	if err != nil {
		return nil, err
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("bad argument #2 to 'offset' (number expected)")
	}
	n, ok := args[1].CoerceInt()
	if !ok {
		return nil, fmt.Errorf("bad argument #2 to 'offset' (number expected)")
	}
	i := int64(1)
	if n < 0 {
		i = int64(len(str)) + 1
	}
	if len(args) >= 3 && !args[2].IsNil() {
		i, ok = args[2].CoerceInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'offset' (number expected)")
		}
	}
	start, end, found, err := luastdlib.OffsetWithCancel(str, n, i, s.nativeCancelFunc())
	if err != nil {
		return nil, err
	}
	if !found {
		return []Value{NilValue()}, nil
	}
	return []Value{IntValue(start), IntValue(end)}, nil
}

func estimateDumpBytes(p *bytecode.Proto, strip bool) int64 {
	seen := make(map[string]struct{})
	total := int64(41) // Lua 5.5 header + root upvalue count
	addStr := func(str string) {
		if _, ok := seen[str]; ok {
			total += 3
			return
		}
		seen[str] = struct{}{}
		total += int64(len(str)) + 3
	}
	var walk func(fn *bytecode.Proto)
	walk = func(fn *bytecode.Proto) {
		if fn == nil {
			return
		}
		total += 16 + int64(len(fn.Code))*4 + int64(len(fn.Upvalues))*3
		for _, c := range fn.Consts {
			switch c.Kind {
			case bytecode.ConstInt, bytecode.ConstFloat:
				total += 9
			case bytecode.ConstString:
				addStr(c.Str)
			default:
				total++
			}
		}
		if !strip && !fn.NoSource {
			addStr(fn.Source)
		} else {
			total += 2
		}
		if !strip {
			total += int64(len(fn.LineInfo)) + int64(len(fn.AbsLineInfo))*8
			for _, lv := range fn.LocVars {
				if lv.Name != "" {
					addStr(lv.Name)
				} else {
					total += 2
				}
				total += 4
			}
			for _, uv := range fn.Upvalues {
				if uv.Name != "" {
					addStr(uv.Name)
				} else {
					total += 2
				}
			}
		}
		for _, sub := range fn.Protos {
			walk(sub)
		}
	}
	walk(p)
	return total
}

type limitedStringWriter struct {
	sb        strings.Builder
	checkSize func(int64) error
}

func (w *limitedStringWriter) Write(p []byte) (int, error) {
	if w.checkSize != nil {
		if err := w.checkSize(int64(w.sb.Len() + len(p))); err != nil {
			return 0, err
		}
	}
	return w.sb.Write(p)
}

func (w *limitedStringWriter) WriteString(s string) (int, error) {
	if w.checkSize != nil {
		if err := w.checkSize(int64(w.sb.Len() + len(s))); err != nil {
			return 0, err
		}
	}
	return w.sb.WriteString(s)
}

func nativeStringDump(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsClosure() {
		return nil, fmt.Errorf("bad argument #1 to 'dump' (Lua function expected)")
	}
	strip := len(args) >= 2 && !args[1].IsFalsy()
	p := args[0].AsClosure().bp.proto
	var checkSize func(int64) error
	if s.memLimit > 0 {
		est := estimateDumpBytes(p, strip)
		if err := s.checkMem(est + 48); err != nil {
			return nil, err
		}
		checkSize = func(n int64) error {
			return s.checkMem(n + 48)
		}
	}
	w := limitedStringWriter{checkSize: checkSize}
	if err := bytecode.DumpTo(&w, p, strip); err != nil {
		return nil, err
	}
	ls, err := s.TryIntern(w.sb.String())
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(ls)}, nil
}

// Unused math import guard if needed
var _ = math.IsNaN
